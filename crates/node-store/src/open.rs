//! SQLite V2 的创建、兼容性拒绝和单连接所有权边界。

use std::{
    path::{Path, PathBuf},
    time::Duration,
};

use dedup_core::{CoreError, MachineId, product_id};
use rusqlite::{Connection, OptionalExtension};
use thiserror::Error;

const SCHEMA: &str = include_str!("schema.sql");

/// 节点持久化边界返回的错误。
#[derive(Debug, Error)]
pub enum StoreError {
    /// SQLite 打开、约束、查询或事务失败。
    #[error("SQLite 操作失败: {0}")]
    Sqlite(#[from] rusqlite::Error),
    /// 数据库已存在但不是当前 Rust V2 schema。
    #[error("数据库不是 mySingerServer Rust V2 schema，不执行旧库迁移")]
    IncompatibleSchema,
    /// 数据库属于另一台物理机器。
    #[error("数据库机器 ID 与当前物理机器不一致")]
    MachineMismatch,
    /// 中心游标早于节点已裁剪的 outbox 边界。
    #[error("同步游标 {requested_seq} 早于已裁剪边界 {pruned_through_seq}，需要全量快照")]
    SnapshotRequired {
        /// 中心请求的最后提交序号。
        requested_seq: u64,
        /// 节点已经清理的最高序号。
        pruned_through_seq: u64,
    },
    /// `u64` 文件大小或时间不能进入 SQLite 有符号 INTEGER。
    #[error("数值 {0} 超出 SQLite INTEGER 范围")]
    IntegerOutOfRange(u64),
    /// 数据库中的固定字节列长度不符合当前 V2 schema。
    #[error("数据库字段 {field} 长度无效: {actual}")]
    InvalidBlob {
        /// 字段名称。
        field: &'static str,
        /// 实际字节数。
        actual: usize,
    },
    /// Sobel 持久化边界拒绝非有限值。
    #[error("Sobel 向量包含非有限值")]
    NonFiniteSobel,
    /// 槽位或部分特征组合不符合当前固定定义。
    #[error("特征结果无效: {0}")]
    InvalidFeature(&'static str),
    /// 快照请求了不属于同步基础数据的表。
    #[error("不支持的快照表: {0}")]
    InvalidSnapshotTable(String),
    /// 任务、分析或删除操作不符合当前持久化状态。
    #[error("状态操作无效: {0}")]
    InvalidState(String),
    /// 分析输入已经冻结，当前运行不能继续追加。
    #[error("分析运行的输入已经冻结")]
    AnalysisInputsFrozen,
    /// 分页游标不是当前 V2 编码。
    #[error("分页游标无效")]
    InvalidCursor,
    /// 删除组没有至少一个当前活动且明确保留的成员。
    #[error("重复组 {0} 没有活动 Keep 成员")]
    MissingKeep(String),
    /// 分页大小必须大于零。
    #[error("分页大小必须大于零")]
    EmptyPageLimit,
    /// 分数不是当前 schema 可保存的有限值。
    #[error("分数字段必须是有限值")]
    NonFiniteScore,
    /// 从数据库恢复强类型路径或机器 ID 失败。
    #[error("数据库领域值无效: {0}")]
    Core(#[from] CoreError),
}

/// 节点 actor 独占的 SQLite 连接和物理机器身份。
///
/// 类型不实现 `Clone`；所有写事务由后续 `NodeEngine` actor 串行调用。
pub struct NodeStore {
    pub(crate) connection: Connection,
    machine_id: MachineId,
    pub(crate) database_path: Option<PathBuf>,
}

impl NodeStore {
    /// 打开应用目录下的数据库；空库创建当前全量 schema，非 V2 旧库直接拒绝。
    pub fn open(path: &Path, machine_id: MachineId) -> Result<Self, StoreError> {
        let connection = Connection::open(path)?;
        initialize_or_validate(connection, machine_id, Some(path.to_path_buf()), true)
    }

    /// 创建用于单元测试和纯本地计算的内存 V2 数据库。
    pub fn open_in_memory(machine_id: MachineId) -> Result<Self, StoreError> {
        let connection = Connection::open_in_memory()?;
        initialize_or_validate(connection, machine_id, None, false)
    }

    /// 为同一文件数据库打开独立 WAL 连接，供后台计算与控制 actor 分离所有权。
    pub fn reopen(&self) -> Result<Self, StoreError> {
        let path = self
            .database_path
            .as_ref()
            .ok_or_else(|| StoreError::InvalidState("内存数据库不能打开独立后台连接".into()))?;
        Self::open(path, self.machine_id.clone())
    }

    /// 返回数据库 schema 的稳定产品标记。
    pub fn schema_id(&self) -> Result<String, StoreError> {
        Ok(self.connection.query_row(
            "SELECT value FROM metadata WHERE key='schema_id'",
            [],
            |row| row.get(0),
        )?)
    }

    /// 返回数据库绑定的物理机器身份。
    pub const fn machine_id(&self) -> &MachineId {
        &self.machine_id
    }
}

fn configure(connection: &Connection, file_backed: bool) -> Result<(), StoreError> {
    connection.pragma_update(None, "foreign_keys", "ON")?;
    connection.busy_timeout(Duration::from_secs(5))?;
    if file_backed {
        connection.pragma_update(None, "journal_mode", "WAL")?;
    }
    Ok(())
}

fn initialize_or_validate(
    connection: Connection,
    machine_id: MachineId,
    database_path: Option<PathBuf>,
    file_backed: bool,
) -> Result<NodeStore, StoreError> {
    let table_count: i64 = connection.query_row(
        "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'",
        [],
        |row| row.get(0),
    )?;
    if table_count == 0 {
        configure(&connection, file_backed)?;
        connection.execute_batch(SCHEMA)?;
        connection.execute(
            "INSERT INTO metadata(key,value) VALUES('schema_id',?1),('machine_id',?2)",
            (product_id(), machine_id.as_str()),
        )?;
    } else {
        let schema: Option<String> = connection
            .query_row(
                "SELECT value FROM metadata WHERE key='schema_id'",
                [],
                |row| row.get(0),
            )
            .optional()
            .map_err(|_| StoreError::IncompatibleSchema)?;
        if schema.as_deref() != Some(product_id()) {
            return Err(StoreError::IncompatibleSchema);
        }
        let schema_version: i64 = connection
            .query_row("PRAGMA user_version", [], |row| row.get(0))
            .map_err(|_| StoreError::IncompatibleSchema)?;
        if schema_version != 2 {
            return Err(StoreError::IncompatibleSchema);
        }
        let stored_machine: String = connection
            .query_row(
                "SELECT value FROM metadata WHERE key='machine_id'",
                [],
                |row| row.get(0),
            )
            .map_err(|_| StoreError::IncompatibleSchema)?;
        if stored_machine != machine_id.as_str() {
            return Err(StoreError::MachineMismatch);
        }
        configure(&connection, file_backed)?;
    }
    Ok(NodeStore {
        connection,
        machine_id,
        database_path,
    })
}

pub(crate) fn sqlite_integer(value: u64) -> Result<i64, StoreError> {
    i64::try_from(value).map_err(|_| StoreError::IntegerOutOfRange(value))
}

pub(crate) fn fixed_bytes<const N: usize>(
    value: Vec<u8>,
    field: &'static str,
) -> Result<[u8; N], StoreError> {
    let actual = value.len();
    value
        .try_into()
        .map_err(|_| StoreError::InvalidBlob { field, actual })
}
