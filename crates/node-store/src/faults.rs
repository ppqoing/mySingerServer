//! 读取超时和 Worker 崩溃文件的持久诊断记录与稳定分页。

use dedup_core::{DisplayPath, MachineId, NormalizedPath};
use rusqlite::params;

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
        self.connection.execute(
            "INSERT INTO file_faults(
                machine_id,normalized_path,display_path,file_size,fault_kind,stage,
                windows_error_code,message
             ) VALUES(?1,?2,?3,?4,?5,?6,?7,?8)
             ON CONFLICT(machine_id,normalized_path,fault_kind) DO UPDATE SET
                file_size=excluded.file_size,
                stage=excluded.stage,
                windows_error_code=excluded.windows_error_code,
                message=excluded.message",
            params![
                fault.machine_id.as_str(),
                fault.normalized_path.as_str(),
                fault.display_path.as_path().to_string_lossy().as_ref(),
                sqlite_integer(fault.file_size)?,
                fault.kind.as_str(),
                fault.stage,
                fault.windows_error_code,
                fault.message,
            ],
        )?;
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
                    windows_error_code,message
             FROM file_faults
             WHERE machine_id>?1
                OR (machine_id=?1 AND normalized_path>?2)
                OR (machine_id=?1 AND normalized_path=?2 AND fault_kind>?3)
             ORDER BY machine_id,normalized_path,fault_kind
             LIMIT ?4",
        )?;
        let raw = statement
            .query_map(
                params![
                    cursor_machine,
                    cursor_path,
                    cursor_kind,
                    (limit + 1) as i64
                ],
                |row| {
                    Ok((
                        row.get::<_, String>(0)?,
                        row.get::<_, String>(1)?,
                        row.get::<_, String>(2)?,
                        row.get::<_, i64>(3)?,
                        row.get::<_, String>(4)?,
                        row.get::<_, String>(5)?,
                        row.get::<_, Option<i32>>(6)?,
                        row.get::<_, String>(7)?,
                    ))
                },
            )?
            .collect::<Result<Vec<_>, _>>()?;
        let has_more = raw.len() > limit;
        let items = raw
            .into_iter()
            .take(limit)
            .map(
                |(machine_id, normalized_path, display_path, file_size, kind, stage, code, message)| {
                    Ok(FileFaultRecord {
                        machine_id: MachineId::parse(&machine_id)?,
                        normalized_path: NormalizedPath::new(&normalized_path)?,
                        display_path: DisplayPath::new(&display_path)?,
                        file_size: u64::try_from(file_size).map_err(|_| {
                            StoreError::InvalidState("文件故障大小不能为负数".into())
                        })?,
                        kind: FileFaultKind::parse(&kind)?,
                        stage,
                        windows_error_code: code,
                        message,
                    })
                },
            )
            .collect::<Result<Vec<_>, StoreError>>()?;
        let next_cursor = has_more
            .then(|| items.last().map(encode_cursor))
            .flatten();
        Ok(FileFaultPage { items, next_cursor })
    }
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
    Ok((machine.to_owned(), path.to_owned(), kind.as_str().to_owned()))
}
