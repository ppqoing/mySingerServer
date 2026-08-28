//! 当前 Node 任务和本地分析运行的阶段进度持久化。

use dedup_core::{AnalysisRunId, TaskId};
use rusqlite::{Connection, params};

use crate::{NodeStore, StoreError, open::sqlite_integer};

/// 任务阶段允许持久化的五种状态。
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
    /// 返回 SQLite 与 PostgreSQL 共用的稳定状态名。
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Waiting => "waiting",
            Self::Running => "running",
            Self::Completed => "completed",
            Self::Failed => "failed",
            Self::Skipped => "skipped",
        }
    }

    fn parse(value: &str) -> Result<Self, StoreError> {
        match value {
            "waiting" => Ok(Self::Waiting),
            "running" => Ok(Self::Running),
            "completed" => Ok(Self::Completed),
            "failed" => Ok(Self::Failed),
            "skipped" => Ok(Self::Skipped),
            _ => Err(StoreError::InvalidState("任务阶段状态无效".into())),
        }
    }
}

/// 写入一个任务或分析阶段的完整持久字段。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TaskStageWrite {
    /// 同一任务内稳定的阶段 ID。
    pub stage_id: String,
    /// 当前持久状态。
    pub state: PersistentStageState,
    /// 已完成工作项数量。
    pub completed: u64,
    /// 总数；未完成枚举等未知场景保存为空。
    pub total: Option<u64>,
    /// 当前阶段失败项数量。
    pub failed: u64,
    /// 当前阶段跳过项数量。
    pub skipped: u64,
    /// 阶段真正开始执行的时间戳。
    pub started_at_ms: Option<u64>,
    /// 阶段进入终态的时间戳。
    pub finished_at_ms: Option<u64>,
    /// PostgreSQL 降级等不终止任务的警告。
    pub warning_text: Option<String>,
}

/// 从数据库恢复的一个任务或分析阶段快照。
pub type TaskStageSnapshot = TaskStageWrite;

impl NodeStore {
    /// 幂等保存 Node 任务阶段；首次非空开始时间写入后不再被覆盖。
    pub fn save_task_stage(
        &mut self,
        task_id: TaskId,
        stage: TaskStageWrite,
    ) -> Result<(), StoreError> {
        save_stage(
            &self.connection,
            "task_stages",
            "task_id",
            &task_id.as_uuid().to_string(),
            &stage,
        )
    }

    /// 按首次写入顺序恢复 Node 任务的全部阶段。
    pub fn task_stages(&self, task_id: TaskId) -> Result<Vec<TaskStageSnapshot>, StoreError> {
        read_stages(
            &self.connection,
            "task_stages",
            "task_id",
            &task_id.as_uuid().to_string(),
        )
    }

    /// 幂等保存 SQLite 单机分析阶段；首次非空开始时间写入后不再被覆盖。
    pub fn save_analysis_stage(
        &mut self,
        run_id: AnalysisRunId,
        stage: TaskStageWrite,
    ) -> Result<(), StoreError> {
        save_stage(
            &self.connection,
            "analysis_run_stages",
            "analysis_run_id",
            &run_id.as_uuid().to_string(),
            &stage,
        )
    }

    /// 按首次写入顺序恢复 SQLite 单机分析的全部阶段。
    pub fn analysis_stages(
        &self,
        run_id: AnalysisRunId,
    ) -> Result<Vec<TaskStageSnapshot>, StoreError> {
        read_stages(
            &self.connection,
            "analysis_run_stages",
            "analysis_run_id",
            &run_id.as_uuid().to_string(),
        )
    }
}

fn save_stage(
    connection: &Connection,
    table: &str,
    owner_column: &str,
    owner_id: &str,
    stage: &TaskStageWrite,
) -> Result<(), StoreError> {
    validate_stage(stage)?;
    let sql = format!(
        "INSERT INTO {table}(
            {owner_column},stage_id,state,completed,total,failed,skipped,
            started_at_ms,finished_at_ms,warning_text)
         VALUES(?1,?2,?3,?4,?5,?6,?7,?8,?9,?10)
         ON CONFLICT({owner_column},stage_id) DO UPDATE SET
            state=excluded.state,
            completed=excluded.completed,
            total=excluded.total,
            failed=excluded.failed,
            skipped=excluded.skipped,
            started_at_ms=COALESCE({table}.started_at_ms,excluded.started_at_ms),
            finished_at_ms=excluded.finished_at_ms,
            warning_text=excluded.warning_text"
    );
    connection.execute(
        &sql,
        params![
            owner_id,
            stage.stage_id,
            stage.state.as_str(),
            sqlite_integer(stage.completed)?,
            optional_sqlite_integer(stage.total)?,
            sqlite_integer(stage.failed)?,
            sqlite_integer(stage.skipped)?,
            optional_sqlite_integer(stage.started_at_ms)?,
            optional_sqlite_integer(stage.finished_at_ms)?,
            stage.warning_text,
        ],
    )?;
    Ok(())
}

fn read_stages(
    connection: &Connection,
    table: &str,
    owner_column: &str,
    owner_id: &str,
) -> Result<Vec<TaskStageSnapshot>, StoreError> {
    let sql = format!(
        "SELECT stage_id,state,completed,total,failed,skipped,
                started_at_ms,finished_at_ms,warning_text
         FROM {table} WHERE {owner_column}=?1 ORDER BY rowid"
    );
    let mut statement = connection.prepare(&sql)?;
    statement
        .query_map([owner_id], |row| {
            Ok((
                row.get::<_, String>(0)?,
                row.get::<_, String>(1)?,
                row.get::<_, i64>(2)?,
                row.get::<_, Option<i64>>(3)?,
                row.get::<_, i64>(4)?,
                row.get::<_, i64>(5)?,
                row.get::<_, Option<i64>>(6)?,
                row.get::<_, Option<i64>>(7)?,
                row.get::<_, Option<String>>(8)?,
            ))
        })?
        .map(|row| {
            let (stage_id, state, completed, total, failed, skipped, started, finished, warning) =
                row?;
            Ok(TaskStageSnapshot {
                stage_id,
                state: PersistentStageState::parse(&state)?,
                completed: non_negative(completed, "阶段完成数")?,
                total: optional_non_negative(total, "阶段总数")?,
                failed: non_negative(failed, "阶段失败数")?,
                skipped: non_negative(skipped, "阶段跳过数")?,
                started_at_ms: optional_non_negative(started, "阶段开始时间")?,
                finished_at_ms: optional_non_negative(finished, "阶段结束时间")?,
                warning_text: warning,
            })
        })
        .collect()
}

fn validate_stage(stage: &TaskStageWrite) -> Result<(), StoreError> {
    if stage.stage_id.trim().is_empty() {
        return Err(StoreError::InvalidState("阶段 ID 不能为空".into()));
    }
    if let (Some(started), Some(finished)) = (stage.started_at_ms, stage.finished_at_ms)
        && finished < started
    {
        return Err(StoreError::InvalidState("阶段结束时间早于开始时间".into()));
    }
    Ok(())
}

fn optional_sqlite_integer(value: Option<u64>) -> Result<Option<i64>, StoreError> {
    value.map(sqlite_integer).transpose()
}

fn non_negative(value: i64, field: &str) -> Result<u64, StoreError> {
    u64::try_from(value).map_err(|_| StoreError::InvalidState(format!("{field}不能为负数")))
}

fn optional_non_negative(value: Option<i64>, field: &str) -> Result<Option<u64>, StoreError> {
    value.map(|value| non_negative(value, field)).transpose()
}
