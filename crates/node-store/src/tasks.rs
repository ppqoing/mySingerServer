//! 节点任务、任务项、崩溃恢复和持久化事件序号。

use dedup_core::{DisplayPath, LocationKey, MachineId, NormalizedPath, TaskId};
use rusqlite::{OptionalExtension, Transaction, params};
use uuid::Uuid;

use crate::{ContentId, NodeStore, StoreError, open::sqlite_integer};

/// 节点任务允许的五种持久化状态。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum TaskStatus {
    /// 尚有等待执行的任务项。
    Queued,
    /// 至少一个任务项正在 Worker 中执行。
    Running,
    /// 所有任务项都进入终态；允许其中包含单项失败。
    Completed,
    /// 任务级基础设施错误导致整个任务失败。
    Failed,
    /// 用户取消整个任务。
    Cancelled,
}

/// 单个任务项允许的五种持久化状态。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum TaskItemStatus {
    /// 等待领取。
    Queued,
    /// 已被一个 Worker 领取。
    Running,
    /// 计算成功。
    Succeeded,
    /// 当前文件计算失败，但不终止同任务其他项。
    Failed,
    /// 当前项被取消。
    Cancelled,
}

/// 创建任务时写入的一个工作项。
#[derive(Clone, Debug)]
pub struct NewTaskItem {
    /// 关联的本机位置；纯控制任务可为空。
    pub location: Option<LocationKey>,
    /// 文件系统访问使用的原始路径。
    pub display_path: Option<DisplayPath>,
    /// 扫描时确认的文件大小。
    pub file_size: Option<u64>,
    /// 已知的 SQLite 本地内容 ID。
    pub content_id: Option<ContentId>,
    /// Worker pipeline 阶段名称。
    pub stage: String,
}

impl NewTaskItem {
    /// 创建不绑定文件的测试或控制项。
    pub fn detached(stage: impl Into<String>) -> Self {
        Self {
            location: None,
            display_path: None,
            file_size: None,
            content_id: None,
            stage: stage.into(),
        }
    }

    /// 创建绑定当前文件和已知内容的计算项。
    pub fn for_content(
        location: LocationKey,
        display_path: DisplayPath,
        file_size: u64,
        content_id: ContentId,
        stage: impl Into<String>,
    ) -> Self {
        Self {
            location: Some(location),
            display_path: Some(display_path),
            file_size: Some(file_size),
            content_id: Some(content_id),
            stage: stage.into(),
        }
    }
}

/// Worker 从 SQLite 原子领取到的任务项。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ClaimedTaskItem {
    /// UUID v7 字符串形式的任务项 ID。
    pub item_id: String,
    /// 所属任务。
    pub task_id: TaskId,
    /// 领取后固定为 Running。
    pub status: TaskItemStatus,
    /// 可选文件位置。
    pub location: Option<LocationKey>,
    /// 可选显示路径。
    pub display_path: Option<DisplayPath>,
    /// 可选文件大小。
    pub file_size: Option<u64>,
    /// 可选本地内容 ID。
    pub content_id: Option<ContentId>,
    /// Worker pipeline 阶段。
    pub stage: String,
}

/// Worker 完成一个已领取项时提交的终态。
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum TaskItemCompletion {
    /// 成功，并可补上本次计算确定的内容 ID。
    Succeeded {
        /// 已知内容 ID；无内容的控制项保持为空。
        content_id: Option<ContentId>,
    },
    /// 单文件失败及简短诊断。
    Failed(String),
    /// 已领取项响应取消。
    Cancelled,
}

/// 一次完成事务提交后可发送给客户端的事件。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TaskEvent {
    /// 所属任务。
    pub task_id: TaskId,
    /// 完成的任务项。
    pub item_id: String,
    /// 持久化后的项状态。
    pub item_status: TaskItemStatus,
    /// 持久化后的任务状态。
    pub task_status: TaskStatus,
    /// 任务内严格递增的事件序号。
    pub event_seq: u64,
}

/// UI 和恢复逻辑读取的任务统计快照。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TaskSnapshot {
    /// 任务 ID。
    pub task_id: TaskId,
    /// 调度器识别的任务种类。
    pub kind: String,
    /// 当前任务状态。
    pub status: TaskStatus,
    /// 最后持久化事件序号。
    pub event_seq: u64,
    /// 总任务项数。
    pub total_items: u64,
    /// 成功项数。
    pub succeeded: u64,
    /// 失败项数。
    pub failed: u64,
    /// 取消项数。
    pub cancelled: u64,
}

/// 用于诊断恢复结果的单项快照。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TaskItemSnapshot {
    /// 任务项 ID。
    pub item_id: String,
    /// 当前项状态。
    pub status: TaskItemStatus,
}

impl NodeStore {
    /// 在一个事务中创建任务及全部初始 queued 项。
    pub fn create_task(
        &mut self,
        kind: &str,
        items: &[NewTaskItem],
        now_ms: i64,
    ) -> Result<TaskId, StoreError> {
        let task_id = TaskId::new();
        let transaction = self.connection.transaction()?;
        transaction.execute(
            "INSERT INTO tasks(task_id,kind,status,total_items,created_at_ms,updated_at_ms)
             VALUES(?1,?2,'queued',?3,?4,?4)",
            params![
                task_id.as_uuid().to_string(),
                kind,
                i64::try_from(items.len())
                    .map_err(|_| StoreError::InvalidState("任务项过多".into()))?,
                now_ms
            ],
        )?;
        for item in items {
            let item_id = Uuid::now_v7().to_string();
            let (machine_id, normalized_path) = item
                .location
                .as_ref()
                .map(|location| {
                    (
                        Some(location.machine_id().as_str()),
                        Some(location.normalized_path().as_str()),
                    )
                })
                .unwrap_or((None, None));
            let display_path = item
                .display_path
                .as_ref()
                .map(|path| path.as_path().to_string_lossy());
            transaction.execute(
                "INSERT INTO task_items(
                   item_id,task_id,machine_id,normalized_path,display_path,file_size,
                   content_id,status,stage)
                 VALUES(?1,?2,?3,?4,?5,?6,?7,'queued',?8)",
                params![
                    item_id,
                    task_id.as_uuid().to_string(),
                    machine_id,
                    normalized_path,
                    display_path.as_deref(),
                    item.file_size.map(sqlite_integer).transpose()?,
                    item.content_id.map(ContentId::as_i64),
                    item.stage
                ],
            )?;
        }
        transaction.commit()?;
        Ok(task_id)
    }

    /// 原子领取任务内按 ID 排序的下一个 queued 项。
    pub fn claim_next_item(
        &mut self,
        task_id: TaskId,
        now_ms: i64,
    ) -> Result<Option<ClaimedTaskItem>, StoreError> {
        let transaction = self.connection.transaction()?;
        let raw = select_next_item(&transaction, task_id)?;
        let Some(raw) = raw else {
            transaction.commit()?;
            return Ok(None);
        };
        transaction.execute(
            "UPDATE task_items SET status='running' WHERE item_id=?1 AND status='queued'",
            [&raw.item_id],
        )?;
        transaction.execute(
            "UPDATE tasks SET status='running',updated_at_ms=?2
             WHERE task_id=?1 AND status='queued'",
            params![task_id.as_uuid().to_string(), now_ms],
        )?;
        transaction.commit()?;
        Ok(Some(raw.into_claimed(task_id)?))
    }

    /// 完成一个 running 项，并在同一事务刷新统计、任务终态和事件序号。
    pub fn complete_item(
        &mut self,
        item_id: &str,
        completion: TaskItemCompletion,
        now_ms: i64,
    ) -> Result<TaskEvent, StoreError> {
        let transaction = self.connection.transaction()?;
        let (task_id_text, current): (String, String) = transaction.query_row(
            "SELECT task_id,status FROM task_items WHERE item_id=?1",
            [item_id],
            |row| Ok((row.get(0)?, row.get(1)?)),
        )?;
        if current != "running" {
            return Err(StoreError::InvalidState(format!(
                "任务项 {item_id} 不是 running"
            )));
        }
        let (item_status, error, content_id) = match completion {
            TaskItemCompletion::Succeeded { content_id } => {
                (TaskItemStatus::Succeeded, None, content_id)
            }
            TaskItemCompletion::Failed(error) => (TaskItemStatus::Failed, Some(error), None),
            TaskItemCompletion::Cancelled => (TaskItemStatus::Cancelled, None, None),
        };
        transaction.execute(
            "UPDATE task_items SET status=?2,error=?3,
               content_id=COALESCE(?4,content_id) WHERE item_id=?1",
            params![
                item_id,
                item_status.as_str(),
                error,
                content_id.map(ContentId::as_i64)
            ],
        )?;
        let counts = item_counts(&transaction, &task_id_text)?;
        let task_status = if counts.3 == 0 {
            TaskStatus::Completed
        } else {
            TaskStatus::Running
        };
        transaction.execute(
            "UPDATE tasks SET status=?2,event_seq=event_seq+1,
               succeeded=?3,failed_items=?4,cancelled=?5,updated_at_ms=?6
             WHERE task_id=?1",
            params![
                task_id_text,
                task_status.as_str(),
                counts.0,
                counts.1,
                counts.2,
                now_ms
            ],
        )?;
        let event_seq: i64 = transaction.query_row(
            "SELECT event_seq FROM tasks WHERE task_id=?1",
            [&task_id_text],
            |row| row.get(0),
        )?;
        let task_id = parse_task_id(&task_id_text)?;
        transaction.commit()?;
        Ok(TaskEvent {
            task_id,
            item_id: item_id.to_owned(),
            item_status,
            task_status,
            event_seq: event_seq as u64,
        })
    }

    /// 把上次进程遗留的 running 项重新排队，其他四种项状态保持不变。
    pub fn recover_running_items(&mut self, now_ms: i64) -> Result<usize, StoreError> {
        let transaction = self.connection.transaction()?;
        let task_ids = {
            let mut statement = transaction
                .prepare("SELECT DISTINCT task_id FROM task_items WHERE status='running'")?;
            statement
                .query_map([], |row| row.get::<_, String>(0))?
                .collect::<Result<Vec<_>, _>>()?
        };
        let changed = transaction.execute(
            "UPDATE task_items SET status='queued' WHERE status='running'",
            [],
        )?;
        for task_id in task_ids {
            transaction.execute(
                "UPDATE tasks SET status='queued',updated_at_ms=?2
                 WHERE task_id=?1 AND status='running'",
                params![task_id, now_ms],
            )?;
        }
        transaction.commit()?;
        Ok(changed)
    }

    /// 读取一个任务的持久化统计。
    pub fn task_snapshot(&self, task_id: TaskId) -> Result<TaskSnapshot, StoreError> {
        let row = self.connection.query_row(
            "SELECT kind,status,event_seq,total_items,succeeded,failed_items,cancelled
             FROM tasks WHERE task_id=?1",
            [task_id.as_uuid().to_string()],
            |row| {
                Ok((
                    row.get::<_, String>(0)?,
                    row.get::<_, String>(1)?,
                    row.get::<_, i64>(2)?,
                    row.get::<_, i64>(3)?,
                    row.get::<_, i64>(4)?,
                    row.get::<_, i64>(5)?,
                    row.get::<_, i64>(6)?,
                ))
            },
        )?;
        Ok(TaskSnapshot {
            task_id,
            kind: row.0,
            status: TaskStatus::parse(&row.1)?,
            event_seq: row.2 as u64,
            total_items: row.3 as u64,
            succeeded: row.4 as u64,
            failed: row.5 as u64,
            cancelled: row.6 as u64,
        })
    }

    /// 按稳定 item ID 返回任务项状态，供恢复诊断与 UI 使用。
    pub fn task_items(&self, task_id: TaskId) -> Result<Vec<TaskItemSnapshot>, StoreError> {
        let mut statement = self.connection.prepare_cached(
            "SELECT item_id,status FROM task_items WHERE task_id=?1 ORDER BY item_id",
        )?;
        let rows = statement
            .query_map([task_id.as_uuid().to_string()], |row| {
                Ok((row.get::<_, String>(0)?, row.get::<_, String>(1)?))
            })?
            .collect::<Result<Vec<_>, _>>()?;
        rows.into_iter()
            .map(|(item_id, status)| {
                Ok(TaskItemSnapshot {
                    item_id,
                    status: TaskItemStatus::parse(&status)?,
                })
            })
            .collect()
    }
}

struct RawTaskItem {
    item_id: String,
    machine_id: Option<String>,
    normalized_path: Option<String>,
    display_path: Option<String>,
    file_size: Option<i64>,
    content_id: Option<i64>,
    stage: String,
}

impl RawTaskItem {
    fn into_claimed(self, task_id: TaskId) -> Result<ClaimedTaskItem, StoreError> {
        let location = match (self.machine_id, self.normalized_path) {
            (Some(machine), Some(path)) => Some(LocationKey::new(
                MachineId::parse(&machine)?,
                NormalizedPath::new(path)?,
            )),
            (None, None) => None,
            _ => return Err(StoreError::InvalidState("任务项位置字段不完整".into())),
        };
        Ok(ClaimedTaskItem {
            item_id: self.item_id,
            task_id,
            status: TaskItemStatus::Running,
            location,
            display_path: self.display_path.map(DisplayPath::new).transpose()?,
            file_size: self.file_size.map(|value| value as u64),
            content_id: self.content_id.map(ContentId::from_i64),
            stage: self.stage,
        })
    }
}

fn select_next_item(
    transaction: &Transaction<'_>,
    task_id: TaskId,
) -> Result<Option<RawTaskItem>, StoreError> {
    Ok(transaction
        .query_row(
            "SELECT item_id,machine_id,normalized_path,display_path,file_size,content_id,stage
             FROM task_items WHERE task_id=?1 AND status='queued' ORDER BY item_id LIMIT 1",
            [task_id.as_uuid().to_string()],
            |row| {
                Ok(RawTaskItem {
                    item_id: row.get(0)?,
                    machine_id: row.get(1)?,
                    normalized_path: row.get(2)?,
                    display_path: row.get(3)?,
                    file_size: row.get(4)?,
                    content_id: row.get(5)?,
                    stage: row.get(6)?,
                })
            },
        )
        .optional()?)
}

fn item_counts(
    transaction: &Transaction<'_>,
    task_id: &str,
) -> Result<(i64, i64, i64, i64), StoreError> {
    Ok(transaction.query_row(
        "SELECT
           SUM(status='succeeded'),SUM(status='failed'),SUM(status='cancelled'),
           SUM(status IN ('queued','running'))
         FROM task_items WHERE task_id=?1",
        [task_id],
        |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?, row.get(3)?)),
    )?)
}

fn parse_task_id(value: &str) -> Result<TaskId, StoreError> {
    let uuid = Uuid::parse_str(value)
        .map_err(|_| StoreError::InvalidState(format!("任务 ID 无效: {value}")))?;
    Ok(TaskId::from_uuid(uuid))
}

impl TaskStatus {
    pub(crate) const fn as_str(self) -> &'static str {
        match self {
            Self::Queued => "queued",
            Self::Running => "running",
            Self::Completed => "completed",
            Self::Failed => "failed",
            Self::Cancelled => "cancelled",
        }
    }

    fn parse(value: &str) -> Result<Self, StoreError> {
        match value {
            "queued" => Ok(Self::Queued),
            "running" => Ok(Self::Running),
            "completed" => Ok(Self::Completed),
            "failed" => Ok(Self::Failed),
            "cancelled" => Ok(Self::Cancelled),
            _ => Err(StoreError::InvalidState(format!("未知任务状态: {value}"))),
        }
    }
}

impl TaskItemStatus {
    const fn as_str(self) -> &'static str {
        match self {
            Self::Queued => "queued",
            Self::Running => "running",
            Self::Succeeded => "succeeded",
            Self::Failed => "failed",
            Self::Cancelled => "cancelled",
        }
    }

    fn parse(value: &str) -> Result<Self, StoreError> {
        match value {
            "queued" => Ok(Self::Queued),
            "running" => Ok(Self::Running),
            "succeeded" => Ok(Self::Succeeded),
            "failed" => Ok(Self::Failed),
            "cancelled" => Ok(Self::Cancelled),
            _ => Err(StoreError::InvalidState(format!("未知任务项状态: {value}"))),
        }
    }
}
