//! 重复文件清单任务的固定三阶段与持久快照构造。

use crate::central::{PersistentStageState, TaskStageWrite};

/// Desktop 重复文件清单生成任务的固定阶段。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum DuplicateListStage {
    /// 从冻结的一筛特征生成候选对。
    BuildCandidates,
    /// 按节点派发并等待二次特征任务。
    DispatchStage2,
    /// 使用完整二次特征精准判重并保存分组。
    FinalCompare,
}

impl DuplicateListStage {
    /// 返回 PostgreSQL 和任务详情共同使用的稳定阶段 ID。
    pub const fn id(self) -> &'static str {
        match self {
            Self::BuildCandidates => "build_candidates",
            Self::DispatchStage2 => "dispatch_stage2",
            Self::FinalCompare => "final_compare",
        }
    }
}

/// 创建一个阶段的完整持久值，调用方提供实际开始与结束时刻。
pub fn stage_write(
    stage: DuplicateListStage,
    state: PersistentStageState,
    completed: u64,
    total: Option<u64>,
    failed: u64,
    started_at_ms: Option<u64>,
    finished_at_ms: Option<u64>,
) -> TaskStageWrite {
    TaskStageWrite {
        stage_id: stage.id().into(),
        state,
        completed,
        total,
        failed,
        skipped: 0,
        started_at_ms,
        finished_at_ms,
        warning_text: None,
    }
}

/// 返回新任务按产品顺序显示的三个等待阶段。
pub fn waiting_stages() -> [TaskStageWrite; 3] {
    [
        DuplicateListStage::BuildCandidates,
        DuplicateListStage::DispatchStage2,
        DuplicateListStage::FinalCompare,
    ]
    .map(|stage| stage_write(stage, PersistentStageState::Waiting, 0, None, 0, None, None))
}

/// 按二次特征内容项状态构造派发阶段进度；失败和取消均视为已结束，避免永久等待。
pub fn stage2_dispatch_stage(states: &[String], started_at_ms: u64, now_ms: u64) -> TaskStageWrite {
    let completed = states
        .iter()
        .filter(|state| state.as_str() == "completed")
        .count() as u64;
    let failed = states
        .iter()
        .filter(|state| state.as_str() == "failed")
        .count() as u64;
    let skipped = states
        .iter()
        .filter(|state| state.as_str() == "cancelled")
        .count() as u64;
    let terminal = states
        .iter()
        .all(|state| matches!(state.as_str(), "completed" | "failed" | "cancelled"));
    TaskStageWrite {
        stage_id: DuplicateListStage::DispatchStage2.id().into(),
        state: if terminal {
            PersistentStageState::Completed
        } else {
            PersistentStageState::Running
        },
        completed,
        total: Some(states.len() as u64),
        failed,
        skipped,
        started_at_ms: Some(started_at_ms),
        finished_at_ms: terminal.then_some(now_ms),
        warning_text: None,
    }
}
