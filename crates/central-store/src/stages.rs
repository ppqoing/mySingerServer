//! 中心清单任务阶段和二次特征派发记录。

use dedup_core::{AnalysisRunId, ContentKey, MachineId, TaskId};

use crate::{CentralError, CentralStore, pg_i64};

/// 中心清单阶段允许持久化的五种状态。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum PersistentStageState {
    /// 尚未开始。
    Waiting,
    /// 已实际开始执行。
    Running,
    /// 阶段正常结束。
    Completed,
    /// 阶段级错误终止。
    Failed,
    /// 按任务决策跳过。
    Skipped,
}

impl PersistentStageState {
    /// 返回 PostgreSQL 固定状态名。
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Waiting => "waiting",
            Self::Running => "running",
            Self::Completed => "completed",
            Self::Failed => "failed",
            Self::Skipped => "skipped",
        }
    }

    fn parse(value: &str) -> Result<Self, CentralError> {
        match value {
            "waiting" => Ok(Self::Waiting),
            "running" => Ok(Self::Running),
            "completed" => Ok(Self::Completed),
            "failed" => Ok(Self::Failed),
            "skipped" => Ok(Self::Skipped),
            _ => Err(CentralError::InvalidState("中心阶段状态无效".into())),
        }
    }
}

/// 写入中心分析阶段的完整持久字段。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TaskStageWrite {
    /// 同一分析运行内稳定的阶段 ID。
    pub stage_id: String,
    /// 当前持久状态。
    pub state: PersistentStageState,
    /// 已完成工作项数量。
    pub completed: u64,
    /// 总数；未知时为空。
    pub total: Option<u64>,
    /// 当前阶段失败项数量。
    pub failed: u64,
    /// 当前阶段跳过项数量。
    pub skipped: u64,
    /// 阶段真正开始执行的时间戳。
    pub started_at_ms: Option<u64>,
    /// 阶段进入终态的时间戳。
    pub finished_at_ms: Option<u64>,
    /// 降级等不终止分析的警告。
    pub warning_text: Option<String>,
}

/// 从中心数据库恢复的一个分析阶段快照。
pub type TaskStageSnapshot = TaskStageWrite;

/// 一项二次特征内容派发的幂等写入字段。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Stage2DispatchWrite {
    /// 负责读取该内容的物理机器。
    pub machine_id: MachineId,
    /// MD5 与文件大小组成的全局内容键。
    pub content: ContentKey,
    /// Node 接受派发后返回的持久任务 ID。
    pub node_task_id: Option<TaskId>,
    /// `queued/running/completed/failed/cancelled` 之一。
    pub state: String,
    /// 最近一次状态变化的毫秒时间戳。
    pub updated_at_ms: u64,
}

/// 从中心数据库恢复的一项二次特征派发快照。
pub type Stage2DispatchSnapshot = Stage2DispatchWrite;

impl CentralStore {
    /// 幂等保存清单任务阶段，已记录的首次开始时间不会被覆盖。
    pub async fn save_analysis_stage(
        &self,
        run_id: AnalysisRunId,
        stage: TaskStageWrite,
    ) -> Result<(), CentralError> {
        validate_stage(&stage)?;
        self.client
            .execute(
                "INSERT INTO analysis_run_stages(
                    analysis_run_id,stage_id,state,completed,total,failed,skipped,
                    started_at_ms,finished_at_ms,warning_text)
                 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
                 ON CONFLICT(analysis_run_id,stage_id) DO UPDATE SET
                    state=EXCLUDED.state,
                    completed=EXCLUDED.completed,
                    total=EXCLUDED.total,
                    failed=EXCLUDED.failed,
                    skipped=EXCLUDED.skipped,
                    started_at_ms=COALESCE(analysis_run_stages.started_at_ms,EXCLUDED.started_at_ms),
                    finished_at_ms=EXCLUDED.finished_at_ms,
                    warning_text=EXCLUDED.warning_text",
                &[
                    &run_id.as_uuid().to_string(),
                    &stage.stage_id,
                    &stage.state.as_str(),
                    &pg_i64(stage.completed, "阶段完成数")?,
                    &optional_pg_i64(stage.total, "阶段总数")?,
                    &pg_i64(stage.failed, "阶段失败数")?,
                    &pg_i64(stage.skipped, "阶段跳过数")?,
                    &optional_pg_i64(stage.started_at_ms, "阶段开始时间")?,
                    &optional_pg_i64(stage.finished_at_ms, "阶段结束时间")?,
                    &stage.warning_text,
                ],
            )
            .await?;
        Ok(())
    }

    /// 按首次插入顺序恢复中心清单任务的全部阶段。
    pub async fn analysis_stages(
        &self,
        run_id: AnalysisRunId,
    ) -> Result<Vec<TaskStageSnapshot>, CentralError> {
        self.client
            .query(
                "SELECT stage_id,state,completed,total,failed,skipped,
                        started_at_ms,finished_at_ms,warning_text
                 FROM analysis_run_stages WHERE analysis_run_id=$1
                 ORDER BY CASE stage_id
                    WHEN 'build_candidates' THEN 1
                    WHEN 'dispatch_stage2' THEN 2
                    WHEN 'final_compare' THEN 3
                    ELSE 100
                 END,stage_id",
                &[&run_id.as_uuid().to_string()],
            )
            .await?
            .into_iter()
            .map(|row| {
                Ok(TaskStageSnapshot {
                    stage_id: row.get(0),
                    state: PersistentStageState::parse(row.get(1))?,
                    completed: non_negative(row.get(2), "阶段完成数")?,
                    total: optional_non_negative(row.get(3), "阶段总数")?,
                    failed: non_negative(row.get(4), "阶段失败数")?,
                    skipped: non_negative(row.get(5), "阶段跳过数")?,
                    started_at_ms: optional_non_negative(row.get(6), "阶段开始时间")?,
                    finished_at_ms: optional_non_negative(row.get(7), "阶段结束时间")?,
                    warning_text: row.get(8),
                })
            })
            .collect()
    }

    /// 按分析运行、机器和内容键幂等保存二次特征派发状态。
    pub async fn upsert_stage2_dispatch(
        &self,
        run_id: AnalysisRunId,
        dispatch: Stage2DispatchWrite,
    ) -> Result<(), CentralError> {
        validate_dispatch_state(&dispatch.state)?;
        let node_task_id = dispatch
            .node_task_id
            .map(|task_id| task_id.as_uuid().to_string());
        self.client
            .execute(
                "INSERT INTO analysis_stage2_dispatches(
                    analysis_run_id,machine_id,md5,file_size,node_task_id,state,updated_at_ms)
                 VALUES($1,$2,$3,$4,$5,$6,$7)
                 ON CONFLICT(analysis_run_id,machine_id,md5,file_size) DO UPDATE SET
                    node_task_id=COALESCE(EXCLUDED.node_task_id,analysis_stage2_dispatches.node_task_id),
                    state=EXCLUDED.state,
                    updated_at_ms=EXCLUDED.updated_at_ms",
                &[
                    &run_id.as_uuid().to_string(),
                    &dispatch.machine_id.as_str(),
                    &dispatch.content.md5().as_slice(),
                    &pg_i64(dispatch.content.file_size(), "派发文件大小")?,
                    &node_task_id,
                    &dispatch.state,
                    &pg_i64(dispatch.updated_at_ms, "派发更新时间")?,
                ],
            )
            .await?;
        Ok(())
    }

    /// 按机器和内容键稳定恢复一个分析运行的全部二次特征派发。
    pub async fn stage2_dispatches(
        &self,
        run_id: AnalysisRunId,
    ) -> Result<Vec<Stage2DispatchSnapshot>, CentralError> {
        self.client
            .query(
                "SELECT machine_id,md5,file_size,node_task_id,state,updated_at_ms
                 FROM analysis_stage2_dispatches WHERE analysis_run_id=$1
                 ORDER BY machine_id,md5,file_size",
                &[&run_id.as_uuid().to_string()],
            )
            .await?
            .into_iter()
            .map(|row| {
                let machine_id: String = row.get(0);
                let md5: Vec<u8> = row.get(1);
                let node_task_id: Option<String> = row.get(3);
                Ok(Stage2DispatchSnapshot {
                    machine_id: MachineId::parse(machine_id.trim_end())?,
                    content: ContentKey::new(
                        fixed_md5(md5)?,
                        non_negative(row.get(2), "派发文件大小")?,
                    ),
                    node_task_id: node_task_id
                        .map(|value| {
                            uuid::Uuid::parse_str(&value)
                                .map(TaskId::from_uuid)
                                .map_err(|_| CentralError::InvalidState("Node 任务 ID 无效".into()))
                        })
                        .transpose()?,
                    state: row.get(4),
                    updated_at_ms: non_negative(row.get(5), "派发更新时间")?,
                })
            })
            .collect()
    }
}

fn validate_stage(stage: &TaskStageWrite) -> Result<(), CentralError> {
    if stage.stage_id.trim().is_empty() {
        return Err(CentralError::InvalidState("阶段 ID 不能为空".into()));
    }
    if let (Some(started), Some(finished)) = (stage.started_at_ms, stage.finished_at_ms)
        && finished < started
    {
        return Err(CentralError::InvalidState(
            "阶段结束时间早于开始时间".into(),
        ));
    }
    Ok(())
}

fn validate_dispatch_state(state: &str) -> Result<(), CentralError> {
    if matches!(
        state,
        "queued" | "running" | "completed" | "failed" | "cancelled"
    ) {
        Ok(())
    } else {
        Err(CentralError::InvalidState("二次派发状态无效".into()))
    }
}

fn optional_pg_i64(value: Option<u64>, field: &str) -> Result<Option<i64>, CentralError> {
    value.map(|value| pg_i64(value, field)).transpose()
}

fn non_negative(value: i64, field: &str) -> Result<u64, CentralError> {
    u64::try_from(value).map_err(|_| CentralError::InvalidState(format!("{field}不能为负数")))
}

fn optional_non_negative(value: Option<i64>, field: &str) -> Result<Option<u64>, CentralError> {
    value.map(|value| non_negative(value, field)).transpose()
}

fn fixed_md5(value: Vec<u8>) -> Result<[u8; 16], CentralError> {
    value
        .try_into()
        .map_err(|_| CentralError::InvalidState("派发 MD5 长度无效".into()))
}
