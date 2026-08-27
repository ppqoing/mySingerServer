//! 跨节点任务终态与 PostgreSQL 同步高水位的纯门禁规则。

use dedup_core::{MachineId, TaskId};

/// 节点协议任务状态在跨机器分析中的稳定表示。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum CrossTaskState {
    /// 尚未开始。
    Queued,
    /// 正在计算。
    Running,
    /// 全部任务项已经进入终态；文件级失败仍由结果完整性处理。
    Completed,
    /// 任务级基础设施失败。
    Failed,
    /// 用户取消。
    Cancelled,
}

/// 一个节点任务及其中心同步游标的门禁快照。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct GateState {
    /// 任务所属物理机器。
    pub machine_id: MachineId,
    /// 节点持久任务 ID。
    pub task_id: TaskId,
    /// 节点当前任务状态。
    pub state: CrossTaskState,
    /// 任务完成事务保存的真实 outbox 高水位。
    pub task_highwater: u64,
    /// PostgreSQL 已提交的该机器同步游标。
    pub sync_highwater: u64,
}

/// 一筛或二筛能否进入下一阶段。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum GateDecision {
    /// 所有任务完成且中心游标均已追上各自高水位。
    Ready,
    /// 至少一个任务或同步游标尚未满足条件；协调器保持当前阶段。
    Waiting,
}

/// 同时检查任务终态和固定高水位，任何节点未满足都不允许部分筛选。
pub fn stage_gate(states: &[GateState]) -> GateDecision {
    if !states.is_empty()
        && states.iter().all(|state| {
            state.state == CrossTaskState::Completed && state.sync_highwater >= state.task_highwater
        })
    {
        GateDecision::Ready
    } else {
        GateDecision::Waiting
    }
}

/// 二筛只等待任务进入任一终态并同步到其高水位；失败结果随后由完整性判定转为 Partial。
pub fn phase2_gate(states: &[GateState]) -> GateDecision {
    if !states.is_empty()
        && states.iter().all(|state| {
            matches!(
                state.state,
                CrossTaskState::Completed | CrossTaskState::Failed | CrossTaskState::Cancelled
            ) && state.sync_highwater >= state.task_highwater
        })
    {
        GateDecision::Ready
    } else {
        GateDecision::Waiting
    }
}
