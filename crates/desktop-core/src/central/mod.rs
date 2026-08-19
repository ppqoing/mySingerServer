//! PostgreSQL 中心库入口；只连接和校验管理员手动创建的 V2 schema。

mod analysis;
mod content;
mod cross_analysis;
mod delete;
mod schema;

use std::fmt;

use thiserror::Error;
use tokio::task::JoinHandle;

pub use analysis::{
    CentralAnalysisInput, CentralAnalysisNode, CentralAnalysisStatus, CentralCandidate,
    CentralCandidateStatus, CentralGroup, CentralGroupKind, CentralGroupMember,
    CentralGroupMemberPage, CentralGroupPage, CentralGroupWrite, CentralPairKind,
    CentralReviewDecision,
};
pub use content::CentralSnapshot;
pub use cross_analysis::CentralRunSnapshot;
pub use delete::{CentralDeleteItem, CentralDeleteOutcome, CentralDeletePlan, CentralDeleteResult};
pub use schema::validate_schema;

/// 发布包内供管理员手动执行的建库脚本相对路径。
pub const CENTRAL_SCHEMA_SCRIPT: &str = "schema/central-v2.sql";

/// 中心连接、schema、同步载荷或事务错误。
#[derive(Debug, Error)]
pub enum CentralError {
    /// 数据库没有管理员手动创建的 V2 schema。
    #[error("中心数据库 schema 缺失，请手动执行 {script}")]
    SchemaMissing {
        /// 发布包内固定脚本路径。
        script: &'static str,
    },
    /// schema 标记或固定表列与当前程序不一致。
    #[error("中心数据库 schema 不兼容: {0}")]
    SchemaMismatch(String),
    /// 节点版本化同步载荷无法解码。
    #[error("节点同步载荷无效: {0}")]
    InvalidPayload(String),
    /// 调用顺序或持久化状态不满足领域规则。
    #[error("中心状态无效: {0}")]
    InvalidState(String),
    /// 页面游标不是本模块生成的规范值。
    #[error("中心分页游标无效")]
    InvalidCursor,
    /// 阈值快照无法序列化为固定 TOML 文本。
    #[error("阈值快照序列化失败: {0}")]
    ThresholdSnapshot(#[from] toml::ser::Error),
    /// PostgreSQL 连接、查询或事务失败。
    #[error(transparent)]
    Postgres(#[from] tokio_postgres::Error),
    /// 配置或跨边界领域值无效。
    #[error(transparent)]
    Core(#[from] dedup_core::CoreError),
}

/// desktop.exe 独占的 PostgreSQL 客户端与连接驱动任务。
pub struct CentralStore {
    pub(crate) client: tokio_postgres::Client,
    connection: JoinHandle<()>,
}

impl fmt::Debug for CentralStore {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("CentralStore")
            .finish_non_exhaustive()
    }
}

impl CentralStore {
    /// 使用明文 `NoTls` 连接并只读校验 schema；缺表时不会执行任何 DDL。
    pub async fn connect(url: &str) -> Result<Self, CentralError> {
        let (client, connection) = tokio_postgres::connect(url, tokio_postgres::NoTls).await?;
        let connection = tokio::spawn(async move {
            let _ = connection.await;
        });
        if let Err(error) = validate_schema(&client).await {
            connection.abort();
            return Err(error);
        }
        Ok(Self { client, connection })
    }
}

impl Drop for CentralStore {
    fn drop(&mut self) {
        self.connection.abort();
    }
}

pub(crate) fn invalid_payload(message: impl Into<String>) -> CentralError {
    CentralError::InvalidPayload(message.into())
}

/// 把领域层无符号计数转换为 PostgreSQL `BIGINT`，并保留字段语义。
pub(crate) fn pg_i64(value: u64, field: &str) -> Result<i64, CentralError> {
    i64::try_from(value)
        .map_err(|_| CentralError::InvalidState(format!("{field} 超出 PostgreSQL BIGINT")))
}
