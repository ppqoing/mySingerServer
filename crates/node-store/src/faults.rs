//! 读取超时和 Worker 崩溃文件的持久诊断记录与稳定分页。

use dedup_core::{DisplayPath, MachineId, NormalizedPath};
use rusqlite::{Transaction, params};

use crate::{NodeStore, StoreError, open::sqlite_integer};

/// 节点对单个文件确认的故障类别。
#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub enum FileFaultKind {
    /// 分块读取超过重试上限，疑似物理读取故障。
    SuspectedPhysicalRead,
    /// Worker 在处理该文件时意外退出。
    WorkerCrash,
}

impl FileFaultKind {
    /// 返回 SQLite 和协议共用的稳定小写名称。
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::SuspectedPhysicalRead => "suspected_physical_read",
            Self::WorkerCrash => "worker_crash",
        }
    }

    fn parse(value: &str) -> Result<Self, StoreError> {
        match value {
            "suspected_physical_read" => Ok(Self::SuspectedPhysicalRead),
            "worker_crash" => Ok(Self::WorkerCrash),
            _ => Err(StoreError::InvalidState("文件故障类别无效".into())),
        }
    }
}

/// 文件故障表中一个 `(机器, 路径, 类别)` 的当前记录。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct FileFaultRecord {
    /// 发生故障的物理机器身份。
    pub machine_id: MachineId,
    /// 故障文件的稳定规范路径。
    pub normalized_path: NormalizedPath,
    /// 首次记录时保存的实际显示和访问路径。
    pub display_path: DisplayPath,
    /// 最近一次故障观察到的文件大小。
    pub file_size: u64,
    /// 故障类别。
    pub kind: FileFaultKind,
    /// 最近一次故障发生的处理阶段。
    pub stage: String,
    /// 读取故障的 Windows 错误码；Worker 崩溃通常为空。
    pub windows_error_code: Option<i32>,
    /// 发生读取超时或崩溃时所在文件块的起始偏移。
    pub read_offset: Option<u64>,
    /// 发生读取超时或崩溃时所在文件块的计划读取大小。
    pub read_size: Option<u64>,
    /// Worker 崩溃时的进程 ID。
    pub worker_pid: Option<u32>,
    /// Worker 崩溃时的进程退出码。
    pub worker_exit_code: Option<i32>,
    /// 相同故障身份首次出现的时间戳。
    pub first_seen_at_ms: u64,
    /// 相同故障身份最近出现的时间戳。
    pub last_seen_at_ms: u64,
    /// 相同故障身份累计出现次数。
    pub occurrence_count: u64,
    /// 最近一次故障的诊断文案。
    pub message: String,
}

/// 文件故障稳定分页结果。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct FileFaultPage {
    /// 当前页记录，按机器、规范路径和故障类别排序。
    pub items: Vec<FileFaultRecord>,
    /// 还有记录时返回最后一项对应的不透明游标。
    pub next_cursor: Option<String>,
}

impl NodeStore {
    /// 插入或更新一个文件故障；同一身份只更新最近的可变诊断字段。
    pub fn upsert_file_fault(&mut self, fault: &FileFaultRecord) -> Result<(), StoreError> {
        let transaction = self.connection.transaction()?;
        upsert_file_fault_in_transaction(&transaction, fault)?;
        transaction.commit()?;
        Ok(())
    }

    /// 清除一台机器上一个规范路径的全部故障类别。
    pub fn clear_file_fault(
        &mut self,
        machine_id: &MachineId,
        normalized_path: &NormalizedPath,
    ) -> Result<usize, StoreError> {
        Ok(self.connection.execute(
            "DELETE FROM file_faults WHERE machine_id=?1 AND normalized_path=?2",
            params![machine_id.as_str(), normalized_path.as_str()],
        )?)
    }

    /// 在一个事务内按机器、规范路径和类别精确清除一条故障身份。
    pub fn clear_file_fault_kind(
        &mut self,
        machine_id: &MachineId,
        normalized_path: &NormalizedPath,
        kind: FileFaultKind,
    ) -> Result<usize, StoreError> {
        let transaction = self.connection.transaction()?;
        let cleared = transaction.execute(
            "DELETE FROM file_faults
             WHERE machine_id=?1 AND normalized_path=?2 AND fault_kind=?3",
            params![machine_id.as_str(), normalized_path.as_str(), kind.as_str()],
        )?;
        transaction.commit()?;
        Ok(cleared)
    }

    /// 按机器、规范路径和故障类别稳定分页。
    pub fn page_file_faults(
        &self,
        cursor: Option<&str>,
        limit: usize,
    ) -> Result<FileFaultPage, StoreError> {
        if limit == 0 {
            return Err(StoreError::EmptyPageLimit);
        }
        let (cursor_machine, cursor_path, cursor_kind) = match cursor {
            Some(cursor) => decode_cursor(cursor)?,
            None => (String::new(), String::new(), String::new()),
        };
        let mut statement = self.connection.prepare_cached(
            "SELECT machine_id,normalized_path,display_path,file_size,fault_kind,stage,
                    windows_error_code,read_offset,read_size,worker_pid,worker_exit_code,
                    first_seen_at_ms,last_seen_at_ms,occurrence_count,message
             FROM file_faults
             WHERE machine_id>?1
                OR (machine_id=?1 AND normalized_path>?2)
                OR (machine_id=?1 AND normalized_path=?2 AND fault_kind>?3)
             ORDER BY machine_id,normalized_path,fault_kind
             LIMIT ?4",
        )?;
        let raw = statement
            .query_map(
                params![cursor_machine, cursor_path, cursor_kind, (limit + 1) as i64],
                |row| {
                    Ok(RawFileFault {
                        machine_id: row.get(0)?,
                        normalized_path: row.get(1)?,
                        display_path: row.get(2)?,
                        file_size: row.get(3)?,
                        kind: row.get(4)?,
                        stage: row.get(5)?,
                        windows_error_code: row.get(6)?,
                        read_offset: row.get(7)?,
                        read_size: row.get(8)?,
                        worker_pid: row.get(9)?,
                        worker_exit_code: row.get(10)?,
                        first_seen_at_ms: row.get(11)?,
                        last_seen_at_ms: row.get(12)?,
                        occurrence_count: row.get(13)?,
                        message: row.get(14)?,
                    })
                },
            )?
            .collect::<Result<Vec<_>, _>>()?;
        let has_more = raw.len() > limit;
        let items = raw
            .into_iter()
            .take(limit)
            .map(|raw| {
                Ok(FileFaultRecord {
                    machine_id: MachineId::parse(&raw.machine_id)?,
                    normalized_path: NormalizedPath::new(&raw.normalized_path)?,
                    display_path: DisplayPath::new(&raw.display_path)?,
                    file_size: u64::try_from(raw.file_size)
                        .map_err(|_| StoreError::InvalidState("文件故障大小不能为负数".into()))?,
                    kind: FileFaultKind::parse(&raw.kind)?,
                    stage: raw.stage,
                    windows_error_code: raw.windows_error_code,
                    read_offset: optional_non_negative(raw.read_offset, "读取块偏移")?,
                    read_size: optional_non_negative(raw.read_size, "读取块大小")?,
                    worker_pid: raw
                        .worker_pid
                        .map(|value| {
                            u32::try_from(value)
                                .map_err(|_| StoreError::InvalidState("Worker PID 超出范围".into()))
                        })
                        .transpose()?,
                    worker_exit_code: raw.worker_exit_code,
                    first_seen_at_ms: non_negative(raw.first_seen_at_ms, "首次发生时间")?,
                    last_seen_at_ms: non_negative(raw.last_seen_at_ms, "最近发生时间")?,
                    occurrence_count: non_negative(raw.occurrence_count, "重复发生次数")?,
                    message: raw.message,
                })
            })
            .collect::<Result<Vec<_>, StoreError>>()?;
        let next_cursor = has_more.then(|| items.last().map(encode_cursor)).flatten();
        Ok(FileFaultPage { items, next_cursor })
    }
}

/// 在调用方事务中幂等写入故障，并保留首次时间、递增重复次数。
pub(crate) fn upsert_file_fault_in_transaction(
    transaction: &Transaction<'_>,
    fault: &FileFaultRecord,
) -> Result<(), StoreError> {
    if fault.last_seen_at_ms < fault.first_seen_at_ms {
        return Err(StoreError::InvalidState(
            "文件故障最近时间早于首次时间".into(),
        ));
    }
    transaction.execute(
        "INSERT INTO file_faults(
            machine_id,normalized_path,display_path,file_size,fault_kind,stage,
            windows_error_code,read_offset,read_size,worker_pid,worker_exit_code,
            first_seen_at_ms,last_seen_at_ms,occurrence_count,message
         ) VALUES(?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11,?12,?13,1,?14)
         ON CONFLICT(machine_id,normalized_path,fault_kind) DO UPDATE SET
            file_size=excluded.file_size,
            stage=excluded.stage,
            windows_error_code=excluded.windows_error_code,
            read_offset=excluded.read_offset,
            read_size=excluded.read_size,
            worker_pid=excluded.worker_pid,
            worker_exit_code=excluded.worker_exit_code,
            last_seen_at_ms=excluded.last_seen_at_ms,
            occurrence_count=file_faults.occurrence_count+1,
            message=excluded.message",
        params![
            fault.machine_id.as_str(),
            fault.normalized_path.as_str(),
            fault.display_path.as_path().to_string_lossy().as_ref(),
            sqlite_integer(fault.file_size)?,
            fault.kind.as_str(),
            fault.stage,
            fault.windows_error_code,
            optional_sqlite_integer(fault.read_offset)?,
            optional_sqlite_integer(fault.read_size)?,
            fault.worker_pid.map(i64::from),
            fault.worker_exit_code,
            sqlite_integer(fault.first_seen_at_ms)?,
            sqlite_integer(fault.last_seen_at_ms)?,
            fault.message,
        ],
    )?;
    Ok(())
}

/// SQLite 查询阶段暂存的文件故障原始字段。
struct RawFileFault {
    machine_id: String,
    normalized_path: String,
    display_path: String,
    file_size: i64,
    kind: String,
    stage: String,
    windows_error_code: Option<i32>,
    read_offset: Option<i64>,
    read_size: Option<i64>,
    worker_pid: Option<i64>,
    worker_exit_code: Option<i32>,
    first_seen_at_ms: i64,
    last_seen_at_ms: i64,
    occurrence_count: i64,
    message: String,
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

fn encode_cursor(record: &FileFaultRecord) -> String {
    format!(
        "{}|{}|{}",
        record.machine_id.as_str(),
        record.kind.as_str(),
        record.normalized_path.as_str()
    )
}

fn decode_cursor(cursor: &str) -> Result<(String, String, String), StoreError> {
    let mut parts = cursor.splitn(3, '|');
    let machine = parts.next().ok_or(StoreError::InvalidCursor)?;
    let kind = parts.next().ok_or(StoreError::InvalidCursor)?;
    let path = parts.next().ok_or(StoreError::InvalidCursor)?;
    MachineId::parse(machine).map_err(|_| StoreError::InvalidCursor)?;
    NormalizedPath::new(path).map_err(|_| StoreError::InvalidCursor)?;
    let kind = FileFaultKind::parse(kind).map_err(|_| StoreError::InvalidCursor)?;
    Ok((
        machine.to_owned(),
        path.to_owned(),
        kind.as_str().to_owned(),
    ))
}
