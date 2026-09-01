//! Node 可选直连 PostgreSQL 的基础特征缓存和 outbox 发布边界。

use std::{future::Future, time::Duration};

use dedup_central_store::{
    CentralBaseCacheEntry, CentralBaseStage1, CentralStage2CacheEntry, CentralStore,
};
use dedup_core::{ContentKey, MachineId, MediaKind, NodePostgresConfig};
use dedup_node_store::{BaseCacheRecord, CompleteStage1, CompleteStage2, ScannedPath};
use dedup_protocol::proto;
use thiserror::Error;

/// Node 查询中心缓存或发布本地 outbox 时的可降级错误。
#[derive(Debug, Error)]
pub enum RemoteCacheError {
    /// PostgreSQL 连接超过 Node 配置的等待上限。
    #[error("连接 PostgreSQL 超时")]
    ConnectTimeout,
    /// PostgreSQL 连接、schema、查询或同步失败。
    #[error(transparent)]
    Central(#[from] dedup_central_store::CentralError),
}

/// 基础计算依赖的可选远程缓存；实现失败时调用方继续使用 SQLite。
pub trait RemoteFeatureCache: Send + Sync + 'static {
    /// 返回本任务连接中心失败后的降级告警；正常或显式关闭时为空。
    fn startup_warning(&self) -> Option<&str> {
        None
    }

    /// 按机器、规范路径和文件大小批量查询，结果顺序与输入一致。
    fn lookup_paths<'a>(
        &'a self,
        machine_id: &'a MachineId,
        paths: &'a [ScannedPath],
    ) -> impl Future<Output = Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError>> + Send + 'a;

    /// 按 MD5 与文件大小批量查询，结果顺序与输入一致。
    fn lookup_contents<'a>(
        &'a self,
        keys: &'a [ContentKey],
    ) -> impl Future<Output = Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError>> + Send + 'a;

    /// 把 Node SQLite outbox 的一个连续批次提交到中心库并返回 ACK 游标。
    fn publish_outbox<'a>(
        &'a mut self,
        machine_id: &'a MachineId,
        batch: &'a proto::SyncChangeBatch,
    ) -> impl Future<Output = Result<u64, RemoteCacheError>> + Send + 'a;
}

/// 单机 SQLite 模式的空实现，不建立任何网络连接。
#[derive(Clone, Copy, Debug, Default)]
pub struct DisabledRemoteFeatureCache;

impl RemoteFeatureCache for DisabledRemoteFeatureCache {
    async fn lookup_paths(
        &self,
        _machine_id: &MachineId,
        paths: &[ScannedPath],
    ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
        Ok(vec![None; paths.len()])
    }

    async fn lookup_contents(
        &self,
        keys: &[ContentKey],
    ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
        Ok(vec![None; keys.len()])
    }

    async fn publish_outbox(
        &mut self,
        _machine_id: &MachineId,
        _batch: &proto::SyncChangeBatch,
    ) -> Result<u64, RemoteCacheError> {
        Ok(0)
    }
}

/// 按远端 wire 类型拆分的两个独占 store；content lane 同时承载 Stage2 与 outbox。
struct PostgresFeatureStores<S> {
    /// 只执行 path cache 查询的连接。
    path_store: S,
    /// 执行 content/Stage2 查询并在 resolver 回收后发布 outbox 的连接。
    content_store: S,
}

impl<S> PostgresFeatureStores<S> {
    /// 从两个已经独立建立并校验的连接构造固定路由。
    const fn new(path_store: S, content_store: S) -> Self {
        Self {
            path_store,
            content_store,
        }
    }

    /// 返回 path cache 专用连接。
    const fn path_cache(&self) -> &S {
        &self.path_store
    }

    /// 返回 content cache 专用连接。
    const fn content_cache(&self) -> &S {
        &self.content_store
    }

    /// Stage2 与基础 content 查询共用 content lane，不占 path wire。
    const fn stage2_cache(&self) -> &S {
        &self.content_store
    }

    /// resolver 退出后以独占借用复用 content lane 发布 outbox。
    const fn outbox(&mut self) -> &mut S {
        &mut self.content_store
    }
}

/// 一个任务运行期间复用的 PostgreSQL path/content 双连接。
pub struct PostgresFeatureCache {
    /// 固定分离的生产查询与发布路由。
    stores: PostgresFeatureStores<CentralStore>,
}

/// 生产任务按配置选择的 SQLite-only 或 PostgreSQL 远程缓存。
pub enum NodeRemoteFeatureCache {
    /// 单机模式，不访问网络。
    Disabled {
        /// 保持与通用缓存 trait 一致的空实现。
        cache: DisabledRemoteFeatureCache,
        /// 启用 PostgreSQL 但连接失败时的可观察降级原因。
        warning: Option<String>,
    },
    /// 多机模式，复用一个已校验 PostgreSQL 连接。
    Postgres(PostgresFeatureCache),
    /// 单元测试使用的中心缓存响应；只模拟二筛读取，不访问网络。
    #[cfg(test)]
    Test {
        /// 按请求顺序返回预设的完整二筛结果。
        hits: Vec<Option<CompleteStage2>>,
    },
}

/// Node 批量查询二次特征缓存所需的内容类型与视频槽位。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Stage2CacheLookup {
    /// MD5 与文件大小组成的内容键。
    pub content: ContentKey,
    /// 一筛确认的图片或视频类型。
    pub media_kind: MediaKind,
    /// 图片为空；视频为必须全部命中的一筛成功槽位。
    pub frame_slots: Vec<u8>,
}

impl NodeRemoteFeatureCache {
    /// 按 Node 配置连接；连接失败返回降级告警和 SQLite-only 实例。
    pub async fn from_config(config: &NodePostgresConfig) -> (Self, bool) {
        if !config.enabled {
            return (
                Self::Disabled {
                    cache: DisabledRemoteFeatureCache,
                    warning: None,
                },
                false,
            );
        }
        match PostgresFeatureCache::connect(config).await {
            Ok(cache) => (Self::Postgres(cache), true),
            Err(error) => {
                tracing::warn!(
                    event = "central_store_degraded",
                    operation = "connect_feature_cache",
                    fallback = "sqlite_only",
                    error = %error,
                    "PostgreSQL 不可用，本次任务降级为 SQLite-only"
                );
                (
                    Self::Disabled {
                        cache: DisabledRemoteFeatureCache,
                        warning: Some(format!("PostgreSQL 缓存降级: {error}")),
                    },
                    false,
                )
            }
        }
    }

    /// 返回连接阶段产生的 SQLite-only 降级告警。
    pub fn startup_warning(&self) -> Option<&str> {
        match self {
            Self::Disabled { warning, .. } => warning.as_deref(),
            Self::Postgres(_) => None,
            #[cfg(test)]
            Self::Test { .. } => None,
        }
    }

    /// 批量查询完整二次特征；关闭或降级模式按输入长度返回未命中。
    pub async fn lookup_stage2(
        &self,
        requests: &[Stage2CacheLookup],
    ) -> Result<Vec<Option<CompleteStage2>>, RemoteCacheError> {
        #[cfg(test)]
        if let Self::Test { hits } = self {
            return Ok(requests
                .iter()
                .enumerate()
                .map(|(index, _)| hits.get(index).cloned().flatten())
                .collect());
        }
        let Self::Postgres(cache) = self else {
            return Ok(vec![None; requests.len()]);
        };
        let values = requests
            .iter()
            .map(|request| {
                (
                    request.content,
                    request.media_kind,
                    request.frame_slots.clone(),
                )
            })
            .collect::<Vec<_>>();
        Ok(cache
            .stores
            .stage2_cache()
            .lookup_stage2_contents(&values)
            .await?
            .into_iter()
            .map(|entry| {
                entry.map(|entry| match entry {
                    CentralStage2CacheEntry::Image(feature) => CompleteStage2::Image(feature),
                    CentralStage2CacheEntry::Video(frames) => CompleteStage2::Video(frames),
                })
            })
            .collect())
    }

    /// 构造只供二筛行为测试使用的远端响应缓存。
    #[cfg(test)]
    pub(crate) fn test_with_stage2(hits: Vec<Option<CompleteStage2>>) -> Self {
        Self::Test { hits }
    }
}

impl RemoteFeatureCache for NodeRemoteFeatureCache {
    fn startup_warning(&self) -> Option<&str> {
        Self::startup_warning(self)
    }

    async fn lookup_paths(
        &self,
        machine_id: &MachineId,
        paths: &[ScannedPath],
    ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
        match self {
            Self::Disabled { cache, .. } => cache.lookup_paths(machine_id, paths).await,
            Self::Postgres(cache) => cache.lookup_paths(machine_id, paths).await,
            #[cfg(test)]
            Self::Test { .. } => Ok(vec![None; paths.len()]),
        }
    }

    async fn lookup_contents(
        &self,
        keys: &[ContentKey],
    ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
        match self {
            Self::Disabled { cache, .. } => cache.lookup_contents(keys).await,
            Self::Postgres(cache) => cache.lookup_contents(keys).await,
            #[cfg(test)]
            Self::Test { .. } => Ok(vec![None; keys.len()]),
        }
    }

    async fn publish_outbox(
        &mut self,
        machine_id: &MachineId,
        batch: &proto::SyncChangeBatch,
    ) -> Result<u64, RemoteCacheError> {
        match self {
            Self::Disabled { cache, .. } => cache.publish_outbox(machine_id, batch).await,
            Self::Postgres(cache) => cache.publish_outbox(machine_id, batch).await,
            #[cfg(test)]
            Self::Test { .. } => Ok(0),
        }
    }
}

impl PostgresFeatureCache {
    /// 在同一个总超时内并发建立 path/content 两连接；任一失败则整体降级。
    pub async fn connect(config: &NodePostgresConfig) -> Result<Self, RemoteCacheError> {
        let connect = async {
            tokio::try_join!(
                CentralStore::connect_parameters(
                    &config.host,
                    config.port,
                    &config.database,
                    &config.username,
                    &config.password,
                ),
                CentralStore::connect_parameters(
                    &config.host,
                    config.port,
                    &config.database,
                    &config.username,
                    &config.password,
                )
            )
        };
        let (path_store, content_store) =
            tokio::time::timeout(Duration::from_secs(config.connect_timeout_seconds), connect)
                .await
                .map_err(|_| RemoteCacheError::ConnectTimeout)??;
        Ok(Self {
            stores: PostgresFeatureStores::new(path_store, content_store),
        })
    }
}

impl RemoteFeatureCache for PostgresFeatureCache {
    async fn lookup_paths(
        &self,
        machine_id: &MachineId,
        paths: &[ScannedPath],
    ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
        let paths = paths
            .iter()
            .map(|path| (path.normalized_path.as_str().to_owned(), path.file_size))
            .collect::<Vec<_>>();
        Ok(self
            .stores
            .path_cache()
            .lookup_base_paths(machine_id, &paths)
            .await?
            .into_iter()
            .map(|entry| entry.map(convert_entry))
            .collect())
    }

    async fn lookup_contents(
        &self,
        keys: &[ContentKey],
    ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
        Ok(self
            .stores
            .content_cache()
            .lookup_base_contents(keys)
            .await?
            .into_iter()
            .map(|entry| entry.map(convert_entry))
            .collect())
    }

    async fn publish_outbox(
        &mut self,
        machine_id: &MachineId,
        batch: &proto::SyncChangeBatch,
    ) -> Result<u64, RemoteCacheError> {
        Ok(self
            .stores
            .outbox()
            .apply_sync_batch(machine_id, batch)
            .await?)
    }
}

/// 把不包含 SQLite 自增 ID 的中心缓存转为 Node 本地导入值。
fn convert_entry(entry: CentralBaseCacheEntry) -> BaseCacheRecord {
    BaseCacheRecord {
        content_id: None,
        content_key: entry.content_key,
        media_kind: entry.media_kind,
        base_complete: entry.base_complete,
        width: entry.width,
        height: entry.height,
        duration_ms: entry.duration_ms,
        stage1: entry.stage1.map(|stage1| match stage1 {
            CentralBaseStage1::Image(feature) => CompleteStage1::Image(feature),
            CentralBaseStage1::Video(frames) => CompleteStage1::Video(frames),
        }),
        image_stage2: None,
        video_stage2: Box::new([None; 6]),
        contact_sheet_relative_path: None,
    }
}

#[cfg(test)]
mod tests {
    use super::PostgresFeatureStores;

    #[test]
    fn postgres_feature_stores_keep_path_and_content_wire_lanes_independent() {
        let mut stores = PostgresFeatureStores::new("path-wire", "content-wire");

        assert_eq!(*stores.path_cache(), "path-wire");
        assert_eq!(*stores.content_cache(), "content-wire");
        assert_eq!(*stores.stage2_cache(), "content-wire");
        assert_eq!(*stores.outbox(), "content-wire");
    }
}
