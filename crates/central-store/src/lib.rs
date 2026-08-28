//! PostgreSQL 中心库入口；只连接和校验管理员手动创建的 V2 schema。
#![warn(missing_docs)]

mod analysis;
mod base_cache;
mod content;
mod cross_analysis;
mod delete;
mod schema;
mod stage2_cache;
mod stages;

use std::{collections::BTreeMap, fmt};

use dedup_core::{ContentKey, MediaKind};
use dedup_media::{ImageStage1, ImageStage2};
use thiserror::Error;
use tokio::task::JoinHandle;

pub use analysis::{
    CentralAnalysisInput, CentralAnalysisNode, CentralAnalysisStatus, CentralCandidate,
    CentralCandidateStatus, CentralGroup, CentralGroupKind, CentralGroupMember,
    CentralGroupMemberPage, CentralGroupPage, CentralGroupWrite, CentralPairKind,
    CentralReviewDecision,
};
pub use base_cache::{CentralBaseCacheEntry, CentralBaseStage1};
pub use content::CentralSnapshot;
pub use cross_analysis::CentralRunSnapshot;
pub use delete::{
    CentralDeleteItem, CentralDeleteOutcome, CentralDeletePlan, CentralDeleteResult,
    CentralDeleteSelection,
};
pub use schema::{inspect_database, validate_schema};
pub use stage2_cache::CentralStage2CacheEntry;
pub use stages::{
    PersistentStageState, Stage2DispatchSnapshot, Stage2DispatchWrite, TaskStageSnapshot,
    TaskStageWrite,
};

/// 一次中心运行从 PostgreSQL 读取的完整特征快照。
///
/// 缺字段的行不会进入对应 Map；`media_kinds` 仍保留内容，因此筛选可以准确统计跳过项。
#[derive(Clone, Debug, Default)]
pub struct CrossFeatureSet {
    /// 冻结输入中每个唯一内容的实际媒体类型。
    pub media_kinds: BTreeMap<ContentKey, MediaKind>,
    /// 字段完整的图片一筛。
    pub image_stage1: BTreeMap<ContentKey, ImageStage1>,
    /// 图片联合二筛。
    pub image_stage2: BTreeMap<ContentKey, ImageStage2>,
    /// 六槽记录完整且成功帧达到固定完整性要求的视频一筛。
    pub video_stage1: BTreeMap<ContentKey, Box<[Option<ImageStage1>; 6]>>,
    /// 覆盖全部成功一筛槽位的视频联合二筛。
    pub video_stage2: BTreeMap<ContentKey, Box<[Option<ImageStage2>; 6]>>,
}

impl CrossFeatureSet {
    /// 判断指定媒体内容的 PostgreSQL 联合二筛是否完整。
    pub fn stage2_complete(&self, content: ContentKey, kind: CentralPairKind) -> bool {
        match kind {
            CentralPairKind::Image => self.image_stage2.contains_key(&content),
            CentralPairKind::Video => self.video_stage2.contains_key(&content),
        }
    }

    /// 返回视频一筛成功槽位；图片或不完整视频返回空列表。
    pub fn video_frame_slots(&self, content: ContentKey) -> Vec<u32> {
        self.video_stage1
            .get(&content)
            .map(|frames| {
                frames
                    .iter()
                    .enumerate()
                    .filter_map(|(slot, frame)| frame.map(|_| slot as u32))
                    .collect()
            })
            .unwrap_or_default()
    }
}

/// 发布包内供管理员手动执行的建库脚本相对路径。
pub const CENTRAL_SCHEMA_SCRIPT: &str = "schema/central-v2.sql";
/// 当前程序只接受的新建中心 PostgreSQL schema 标识。
pub const CENTRAL_SCHEMA_ID: &str = "mysingerserver-rust-v2-central-schema-3";

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
        Self::finish_connect(client, connection).await
    }

    /// 使用分离的基础连接参数连接，避免用户名或密码中的特殊字符被 URL 误解析。
    pub async fn connect_parameters(
        host: &str,
        port: u16,
        database: &str,
        username: &str,
        password: &str,
    ) -> Result<Self, CentralError> {
        let mut config = tokio_postgres::Config::new();
        config
            .host(host)
            .port(port)
            .dbname(database)
            .user(username)
            .password(password);
        let (client, connection) = config.connect(tokio_postgres::NoTls).await?;
        Self::finish_connect(client, connection).await
    }

    /// 启动连接驱动并执行唯一一次 schema 校验。
    async fn finish_connect(
        client: tokio_postgres::Client,
        connection: tokio_postgres::Connection<
            tokio_postgres::Socket,
            tokio_postgres::tls::NoTlsStream,
        >,
    ) -> Result<Self, CentralError> {
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
