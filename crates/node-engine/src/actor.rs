//! 节点业务 actor：网络和托盘只发送命令，SQLite 与 WorkerPool 保持单一所有者。

use std::{
    collections::{BTreeMap, BTreeSet},
    fs,
    future::Future,
    net::SocketAddr,
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
    time::{Duration, SystemTime, UNIX_EPOCH},
};

use dedup_core::{
    AnalysisRunId, ContentKey, CoreError, DeleteMode, DiskReadConfig, DisplayPath, EnumeratorKind,
    LocationKey, MachineId, NodeConfig, NodePostgresConfig, NormalizedPath, TaskId, Thresholds,
};
use dedup_node_store::{
    AnalysisStatus, DeleteBatchPlan, DeleteOutcome, GroupKind, NodeStore, OwnedSnapshot,
    PersistentStageState as StorePersistentStageState, PlannedDeleteItem, ReviewDecision,
    ScannedPath, StoreError, TaskStageWrite, classify_cache_completeness,
};
use dedup_protocol::{
    BASE_MISSING_PROBE, BASE_MISSING_STAGE1, MAX_LOCAL_RESULT_WINDOW_ROWS, proto,
};
use thiserror::Error;
#[cfg(feature = "test-hooks")]
use tokio::sync::Notify;
use tokio::{
    sync::{mpsc, oneshot},
    task::JoinHandle,
};
use uuid::Uuid;

use dedup_windows::{
    AppLayout, LocalNodePath, ReadCancellationToken, machine_id_from_fields,
    read_physical_machine_fields,
};

#[cfg(feature = "test-hooks")]
use crate::scan::BasePersistTestWaiter;
use crate::{
    NodeRemoteFeatureCache, RemoteFeatureCache,
    analysis::model::LocalAnalysisRun,
    analysis::{
        AnalysisBlocked, LatestAnalysisReader, LocalAnalysisReport, LocalResultWindowKind,
        Stage2BatchItem, Stage2BatchPlan, Stage2TaskFileRunOptions, begin_stage2_batch,
        evaluate_candidates, missing_stage2_items, prepare_current_scan_analysis,
        publish_local_analysis_result_with_reader, run_stage2_batch_production,
    },
    artifact_registry::RegenerableArtifactRegistry,
    config_repository::{
        ConfigRepositoryError, LoadedNodeConfig, NodeConfigRepository, ResolvedNodePaths,
    },
    delete::DeleteEngine,
    disk_full_cleanup::{DiskFullCleaner, SystemArtifactDiskResolver},
    preview::{PreviewKind, PreviewService},
    review_registry::ReviewRegistry,
    runtime_tasks::{
        RuntimeFailureUpdate, RuntimeProgressPublisher, RuntimeProgressUnit, RuntimeStage,
        RuntimeTaskKind, RuntimeTaskRegistry, RuntimeTaskReporter, RuntimeTaskState,
    },
    scan::{
        BaseComputeEngine, FileEnumerator, FilteredWindowsWalker, MAX_BASE_TASK_BATCH,
        MediaExtensionFilter, PipelineFileReader, PipelineLimits, PlannedScannedPath,
        PreferredEverythingEnumerator, ScanDiskPlan, ScanError, ScanOptions,
        ScanRootStorageResolver, ScanSummary, ScheduledFileReader, SystemScanRootStorageResolver,
        TaskFileBaseCoordinatorOptions, TaskFileMediaPersistenceOptions, TaskFileScanRunOptions,
        begin_scan_task, configure_base_compute_runtime, ensure_everything_ready,
        input_order::interleave_rows_by_root, run_task_file_scan_with_runtime,
    },
    server::{NodeRequestHandler, NodeServer, ServerError},
    worker::{WorkerLaunch, WorkerPool, WorkerPoolConfig, WorkerPoolError, WorkerPoolHandle},
};

/// 节点 actor 命令通道或运行状态错误。
#[derive(Debug, Error)]
pub enum EngineError {
    /// actor 已经完成有序关闭。
    #[error("节点计算引擎已经关闭")]
    Closed,
    /// 三阶段 Worker 重启在 prepare、requeue 或 recreate 中失败。
    #[error("节点计算引擎操作失败: {0}")]
    Operation(String),
}

/// 可克隆的节点业务入口；本类型不暴露 Store 或 WorkerPool 引用。
#[derive(Clone)]
pub struct NodeEngineHandle {
    commands: mpsc::Sender<EngineCommand>,
    runtime_tasks: RuntimeTaskRegistry,
}

impl NodeEngineHandle {
    /// 返回与 actor/服务共享的进程内 registry，仅供直接协议测试驱动任务终态。
    #[doc(hidden)]
    pub fn runtime_tasks_for_test(&self) -> RuntimeTaskRegistry {
        self.runtime_tasks.clone()
    }

    /// 返回 actor 当前进程最近一次完成扫描，仅用于验证终态发布顺序。
    #[cfg(feature = "test-hooks")]
    async fn latest_completed_scan_for_test(&self) -> Option<crate::scan::CompletedScanSnapshot> {
        let (reply, response) = oneshot::channel();
        self.commands
            .send(EngineCommand::LatestCompletedScanForTest(reply))
            .await
            .ok()?;
        response.await.ok().flatten()
    }

    /// 取消当前计算并等待 Worker 收束后重启计算引擎。
    pub async fn restart_engine(&self) -> Result<(), EngineError> {
        let (reply, response) = oneshot::channel();
        self.commands
            .send(EngineCommand::Restart(reply))
            .await
            .map_err(|_| EngineError::Closed)?;
        response
            .await
            .map_err(|_| EngineError::Closed)?
            .map_err(EngineError::Operation)
    }

    /// 请求 actor 完成当前命令并释放 Store、WorkerPool。
    pub async fn shutdown(&self) -> Result<(), EngineError> {
        let (reply, response) = oneshot::channel();
        self.commands
            .send(EngineCommand::Shutdown(reply))
            .await
            .map_err(|_| EngineError::Closed)?;
        response.await.map_err(|_| EngineError::Closed)
    }

    async fn request(&self, request: proto::Envelope) -> proto::Envelope {
        let request_id = request.request_id;
        let (reply, response) = oneshot::channel();
        if self
            .commands
            .send(EngineCommand::Protocol(request, reply))
            .await
            .is_err()
        {
            return error_response(
                request_id,
                proto::ErrorCode::Internal,
                "节点计算引擎已经关闭",
            );
        }
        response.await.unwrap_or_else(|_| {
            error_response(
                request_id,
                proto::ErrorCode::Internal,
                "节点计算引擎没有返回响应",
            )
        })
    }

    async fn connection_closed(&self) {
        let _ = self.commands.send(EngineCommand::ConnectionClosed).await;
    }
}

/// 供 actor 使用且不暴露仓库内部恢复字段的配置状态。
#[doc(hidden)]
#[derive(Clone, Debug, PartialEq)]
pub struct NodeConfigState {
    /// 原样保留的当前 Node 配置。
    pub config: NodeConfig,
    /// 当前完整配置文件的 SHA-256 摘要。
    pub version_sha256: String,
}

impl NodeConfigState {
    /// 构造不携带真实仓库恢复点的测试状态。
    #[doc(hidden)]
    pub fn for_test(config: NodeConfig, version_sha256: impl Into<String>) -> Self {
        Self {
            config,
            version_sha256: version_sha256.into(),
        }
    }

    fn from_loaded(loaded: LoadedNodeConfig) -> Self {
        Self {
            config: loaded.config.clone(),
            version_sha256: loaded.version_sha256.clone(),
        }
    }
}

/// actor 配置协议对原子仓库使用的最小可替换边界。
#[doc(hidden)]
pub trait NodeConfigRepositoryAccess: Send + Sync {
    /// 加载当前原始配置和版本摘要。
    fn snapshot(&self) -> Result<NodeConfigState, ConfigRepositoryError>;

    /// 仅在版本摘要匹配时原子保存配置并返回新状态。
    fn save_if_version(
        &self,
        expected_version_sha256: &str,
        config: &NodeConfig,
    ) -> Result<NodeConfigState, ConfigRepositoryError>;
}

impl NodeConfigRepositoryAccess for NodeConfigRepository {
    fn snapshot(&self) -> Result<NodeConfigState, ConfigRepositoryError> {
        NodeConfigRepository::snapshot(self).map(NodeConfigState::from_loaded)
    }

    fn save_if_version(
        &self,
        expected_version_sha256: &str,
        config: &NodeConfig,
    ) -> Result<NodeConfigState, ConfigRepositoryError> {
        NodeConfigRepository::save_if_version(self, expected_version_sha256, config)
            .map(NodeConfigState::from_loaded)
    }
}

/// 生产启动时取得物理机器 ID 的唯一注入边界。
pub trait IdentityProvider {
    /// 返回从物理 SMBIOS 字段计算的稳定机器 ID。
    fn machine_id(&self) -> Result<MachineId, CoreError>;
}

/// 生产节点使用的 SMBIOS 物理身份提供器。
#[derive(Clone, Copy, Debug, Default)]
pub struct SmbiosIdentityProvider;

impl IdentityProvider for SmbiosIdentityProvider {
    fn machine_id(&self) -> Result<MachineId, CoreError> {
        machine_id_from_fields(&read_physical_machine_fields()?)
    }
}

/// 测试使用的固定机器身份，不进入 NodeConfig 或生产入口。
#[derive(Clone, Debug)]
pub struct FixedIdentityProvider {
    machine_id: MachineId,
}

impl FixedIdentityProvider {
    /// 创建仅供测试装配的固定身份提供器。
    pub const fn new(machine_id: MachineId) -> Self {
        Self { machine_id }
    }
}

impl IdentityProvider for FixedIdentityProvider {
    fn machine_id(&self) -> Result<MachineId, CoreError> {
        Ok(self.machine_id.clone())
    }
}

/// 完整 node.exe 运行时的启动或有序关闭错误。
#[derive(Debug, Error)]
pub enum RuntimeError {
    /// 应用目录、配置或机器身份无效。
    #[error(transparent)]
    Core(#[from] CoreError),
    /// 创建数据目录或 TCP listener 失败。
    #[error(transparent)]
    Io(#[from] std::io::Error),
    /// SQLite 创建、校验或恢复失败。
    #[error(transparent)]
    Store(#[from] StoreError),
    /// WorkerPool 创建或关闭失败。
    #[error(transparent)]
    Worker(#[from] WorkerPoolError),
    /// 节点 TCP 服务退出并返回错误。
    #[error(transparent)]
    Server(#[from] ServerError),
    /// actor 命令通道已经关闭。
    #[error(transparent)]
    Engine(#[from] EngineError),
    /// Tokio 任务异常终止。
    #[error("节点后台任务异常终止: {0}")]
    Join(String),
}

/// 同时拥有 listener、NodeEngine actor 和 WorkerPool 生命周期的节点运行时。
pub struct NodeRuntime {
    handle: NodeEngineHandle,
    listen_address: SocketAddr,
    server_shutdown: Option<oneshot::Sender<()>>,
    server_task: JoinHandle<Result<(), ServerError>>,
    actor_task: JoinHandle<()>,
}

impl NodeRuntime {
    /// 从应用相对目录、已验证配置和物理身份启动 SQLite、Worker、actor 与 TCP。
    pub async fn start<I>(
        layout: &AppLayout,
        config: &NodeConfig,
        identity: &I,
    ) -> Result<Self, RuntimeError>
    where
        I: IdentityProvider,
    {
        let paths = resolve_runtime_paths(layout, config)?;
        Self::start_inner(layout, config, &paths, identity).await
    }

    /// 使用启动时已经解析并校验的配置路径启动 Node；生产入口应传入仓库快照中的路径。
    pub async fn start_with_paths<I>(
        layout: &AppLayout,
        config: &NodeConfig,
        paths: &ResolvedNodePaths,
        identity: &I,
    ) -> Result<Self, RuntimeError>
    where
        I: IdentityProvider,
    {
        Self::start_inner(layout, config, paths, identity).await
    }

    async fn start_inner<I>(
        layout: &AppLayout,
        config: &NodeConfig,
        paths: &ResolvedNodePaths,
        identity: &I,
    ) -> Result<Self, RuntimeError>
    where
        I: IdentityProvider,
    {
        config.validate()?;
        fs::create_dir_all(&paths.data_path)?;
        fs::create_dir_all(&paths.cache_path)?;
        fs::create_dir_all(&paths.log_path)?;
        let runtime_root = paths.data_path.join("runtime");
        reset_transient_runtime_root(&runtime_root)?;
        let machine_id = identity.machine_id()?;
        let store = NodeStore::open(&paths.data_path.join("node.db"), machine_id)?;
        let logical_cpu_count = std::thread::available_parallelism()
            .map(usize::from)
            .unwrap_or(1);
        let effective_worker_count = config.worker.effective_worker_count(logical_cpu_count);
        let effective_cpu_budget = config.worker.effective_cpu_budget(logical_cpu_count);
        let worker_pool_config = WorkerPoolConfig::new(
            WorkerLaunch::new(layout.executable_dir().join("worker.exe")),
            effective_worker_count,
        )
        .with_cpu_budget(effective_cpu_budget);
        let worker_pool = WorkerPool::start(worker_pool_config.clone()).await?;
        let listener = tokio::net::TcpListener::bind((config.listen_ip, config.port)).await?;
        let listen_address = listener.local_addr()?;
        let repository = NodeConfigRepository::from_layout(layout);
        let artifact_registry = Arc::new(RegenerableArtifactRegistry::new(
            layout.executable_dir(),
            &paths.cache_path,
        )?);
        // 生产 Node 不启用磁盘满自动清理；写入失败按普通持久化错误返回。
        let (handle, actor_task) = spawn_actor(
            store,
            Some(worker_pool),
            listen_address,
            &paths.cache_path,
            &runtime_root,
            config.enumerator,
            MediaExtensionFilter::from_config(config),
            config.read.clone(),
            config.postgres.clone(),
            effective_worker_count,
            Some(Box::new(repository)),
            artifact_registry,
            None,
            Some(worker_pool_config),
            ActorTestHooks::default(),
        );
        let (server_shutdown, shutdown) = oneshot::channel();
        let server_task = tokio::spawn(NodeServer::serve_until(listener, handle.clone(), shutdown));
        Ok(Self {
            handle,
            listen_address,
            server_shutdown: Some(server_shutdown),
            server_task,
            actor_task,
        })
    }

    /// 返回供托盘与进程入口发送计算引擎重启/关闭命令的 actor 句柄。
    pub fn handle(&self) -> &NodeEngineHandle {
        &self.handle
    }

    /// 返回 listener 实际绑定地址。
    pub const fn listen_address(&self) -> SocketAddr {
        self.listen_address
    }

    /// 先停止接受管理连接，再提交 actor 关闭并等待 Worker Job 释放。
    pub async fn shutdown(mut self) -> Result<(), RuntimeError> {
        if let Some(shutdown) = self.server_shutdown.take() {
            let _ = shutdown.send(());
        }
        self.server_task
            .await
            .map_err(|error| RuntimeError::Join(error.to_string()))??;
        self.handle.shutdown().await?;
        self.actor_task
            .await
            .map_err(|error| RuntimeError::Join(error.to_string()))?;
        Ok(())
    }
}

/// 把 Node 配置中的路径解析为实际 IO 使用的路径，避免回退到 AppLayout 默认目录。
fn resolve_runtime_paths(
    layout: &AppLayout,
    config: &NodeConfig,
) -> Result<ResolvedNodePaths, RuntimeError> {
    let executable_dir = layout.executable_dir();
    let resolve = |raw: &str| {
        LocalNodePath::validate(executable_dir, raw).map(|path| path.resolved().to_path_buf())
    };
    Ok(ResolvedNodePaths {
        data_path: resolve(&config.paths.data_path)?,
        config_path: resolve(&config.paths.config_path)?,
        log_path: resolve(&config.paths.log_path)?,
        cache_path: resolve(&config.paths.cache_path)?,
    })
}

impl NodeRequestHandler for NodeEngineHandle {
    fn handle(
        &self,
        request: proto::Envelope,
    ) -> impl std::future::Future<Output = proto::Envelope> + Send {
        let handle = self.clone();
        async move { handle.request(request).await }
    }

    fn connection_closed(&self) -> impl std::future::Future<Output = ()> + Send {
        let handle = self.clone();
        async move { handle.connection_closed().await }
    }

    fn subscribe_runtime_events(
        &self,
    ) -> Option<tokio::sync::broadcast::Receiver<proto::RuntimeTaskChanged>> {
        Some(self.runtime_tasks.subscribe())
    }
}

/// 创建并运行单一 NodeEngine actor 的工厂。
pub struct NodeEngine;

/// 后台结果已收束、但尚未投递完成命令时的测试控制端。
#[cfg(feature = "test-hooks")]
#[doc(hidden)]
#[cfg_attr(not(test), allow(dead_code))]
pub(crate) struct BackgroundOutcomeTestController {
    /// 控制端与后台 waiter 共享的同步原语。
    shared: Arc<BackgroundOutcomeTestState>,
}

/// 后台专用的完成命令投递 waiter。
#[cfg(feature = "test-hooks")]
pub(crate) struct BackgroundOutcomeTestWaiter {
    /// 与测试控制端共享的同步原语。
    shared: Arc<BackgroundOutcomeTestState>,
}

/// 测试 gate 的进入和放行通知。
#[cfg(feature = "test-hooks")]
#[derive(Default)]
struct BackgroundOutcomeTestState {
    /// 后台已经保存 outcome 并归还 Pool。
    entered: Notify,
    /// 测试允许投递 BackgroundFinished。
    released: Notify,
}

#[cfg(feature = "test-hooks")]
#[cfg_attr(not(test), allow(dead_code))]
impl BackgroundOutcomeTestController {
    /// 创建分离的控制端和后台 waiter。
    fn new() -> (Self, BackgroundOutcomeTestWaiter) {
        let shared = Arc::new(BackgroundOutcomeTestState::default());
        (
            Self {
                shared: Arc::clone(&shared),
            },
            BackgroundOutcomeTestWaiter { shared },
        )
    }

    /// 等待后台已经完整收束、但尚未投递完成命令。
    async fn wait_until_entered(&self) {
        self.shared.entered.notified().await;
    }

    /// 放行完成命令投递。
    fn release(&self) {
        self.shared.released.notify_one();
    }
}

#[cfg(feature = "test-hooks")]
impl Drop for BackgroundOutcomeTestController {
    /// 测试异常退出时避免后台 task 永久停在测试 gate。
    fn drop(&mut self) {
        self.release();
    }
}

#[cfg(feature = "test-hooks")]
impl BackgroundOutcomeTestWaiter {
    /// 通知测试 outcome 已可消费，并等待其指定命令投递时机。
    async fn wait_before_background_finished(self) {
        self.shared.entered.notify_one();
        self.shared.released.notified().await;
    }
}

/// 本地分析发布顺序测试的控制端。
#[cfg(feature = "test-hooks")]
#[doc(hidden)]
pub(crate) struct AnalysisPublishTestController {
    /// 控制端与 actor 内 waiter 共享的同步原语。
    shared: Arc<AnalysisPublishTestState>,
}

/// 本地分析发布顺序测试的 actor 内 waiter。
#[cfg(feature = "test-hooks")]
pub(crate) struct AnalysisPublishTestWaiter {
    /// 与控制端共享的同步原语。
    shared: Arc<AnalysisPublishTestState>,
}

/// 等待 actor 安装分析结果后再放行终态事件。
#[cfg(feature = "test-hooks")]
#[derive(Default)]
struct AnalysisPublishTestState {
    /// actor 已安装 latest_analysis。
    installed: Notify,
    /// 测试允许发布 Runtime completed。
    released: Notify,
}

#[cfg(feature = "test-hooks")]
impl AnalysisPublishTestController {
    /// 创建分离的控制端和 actor 内 waiter。
    pub(crate) fn new() -> (Self, AnalysisPublishTestWaiter) {
        let shared = Arc::new(AnalysisPublishTestState::default());
        (
            Self {
                shared: Arc::clone(&shared),
            },
            AnalysisPublishTestWaiter { shared },
        )
    }

    /// 等待 actor 已安装最近一次本地分析结果。
    pub(crate) async fn wait_until_installed(&self) {
        self.shared.installed.notified().await;
    }

    /// 放行本地分析的 Runtime completed 发布。
    pub(crate) fn release(&self) {
        self.shared.released.notify_one();
    }
}

#[cfg(feature = "test-hooks")]
impl Drop for AnalysisPublishTestController {
    /// 测试异常退出时避免 actor 永久停在发布顺序 gate。
    fn drop(&mut self) {
        self.release();
    }
}

#[cfg(feature = "test-hooks")]
impl AnalysisPublishTestWaiter {
    /// 通知测试已安装结果，并等待终态发布许可。
    async fn wait_after_install(self) {
        self.shared.installed.notify_one();
        self.shared.released.notified().await;
    }
}

/// 本地分析最终评估前取消测试的控制端。
#[cfg(feature = "test-hooks")]
#[doc(hidden)]
pub(crate) struct AnalysisBeforePublishTestController {
    /// 控制端与后台 waiter 共享的同步原语。
    shared: Arc<AnalysisBeforePublishState>,
}

/// 本地分析最终评估前取消测试的后台 waiter。
#[cfg(feature = "test-hooks")]
struct AnalysisBeforePublishTestWaiter {
    /// 与控制端共享的同步原语。
    shared: Arc<AnalysisBeforePublishState>,
}

/// 二筛 ACK 收束后、最终评估开始前的测试 gate。
#[cfg(feature = "test-hooks")]
#[derive(Default)]
struct AnalysisBeforePublishState {
    /// 后台已完成二筛 runner 收束。
    entered: Notify,
    /// 测试允许后台继续最终评估。
    released: Notify,
}

#[cfg(feature = "test-hooks")]
impl AnalysisBeforePublishTestController {
    /// 创建测试控制端和后台 waiter。
    fn new() -> (Self, AnalysisBeforePublishTestWaiter) {
        let shared = Arc::new(AnalysisBeforePublishState::default());
        (
            Self {
                shared: Arc::clone(&shared),
            },
            AnalysisBeforePublishTestWaiter { shared },
        )
    }

    /// 等待二筛 runner 收束到最终评估前。
    async fn wait_until_entered(&self) {
        self.shared.entered.notified().await;
    }

    /// 放行最终评估和结果发布。
    fn release(&self) {
        self.shared.released.notify_one();
    }
}

#[cfg(feature = "test-hooks")]
impl Drop for AnalysisBeforePublishTestController {
    /// 测试提前退出时放行后台，避免遗留挂起任务。
    fn drop(&mut self) {
        self.release();
    }
}

#[cfg(feature = "test-hooks")]
impl AnalysisBeforePublishTestWaiter {
    /// 通知测试 gate 已进入，并等待放行。
    async fn wait_before_publish(self) {
        self.shared.entered.notify_one();
        self.shared.released.notified().await;
    }
}

/// Actor 测试注入点；默认构建是零大小类型且没有运行时分支。
#[derive(Default)]
struct ActorTestHooks {
    /// 仅 feature 测试把首条 Base persist 暂停在 SQLite 事务前。
    #[cfg(feature = "test-hooks")]
    first_persist_waiter: Option<BasePersistTestWaiter>,
    /// 仅 feature 测试暂停完成命令，制造 restart 与扫描 outcome 的受控交错。
    #[cfg(feature = "test-hooks")]
    background_outcome_waiter: Option<BackgroundOutcomeTestWaiter>,
    /// 仅 feature 测试在安装本地分析结果后暂停 Runtime 终态发布。
    #[cfg(feature = "test-hooks")]
    analysis_publish_waiter: Option<AnalysisPublishTestWaiter>,
    /// 仅 feature 测试在二筛收束后、最终评估前暂停本地分析。
    #[cfg(feature = "test-hooks")]
    analysis_before_publish_waiter: Option<AnalysisBeforePublishTestWaiter>,
}

/// 测试 actor 统一使用默认媒体扩展名，避免每个夹具重复构造配置。
fn default_media_extension_filter() -> MediaExtensionFilter {
    MediaExtensionFilter::from_config(&NodeConfig::default())
}

impl NodeEngine {
    /// 创建不启动 Worker 的测试 actor；协议、SQLite 和关闭路径与生产相同。
    #[doc(hidden)]
    pub fn spawn_for_test(
        store: NodeStore,
        listen_address: SocketAddr,
        cache_root: &Path,
    ) -> (NodeEngineHandle, JoinHandle<()>) {
        let (artifact_registry, disk_full_cleaner) = test_artifact_cleanup(cache_root);
        spawn_actor(
            store,
            None,
            listen_address,
            cache_root,
            &cache_root.join("runtime"),
            EnumeratorKind::WindowsWalker,
            default_media_extension_filter(),
            DiskReadConfig::default(),
            NodePostgresConfig::default(),
            1,
            None,
            artifact_registry,
            Some(disk_full_cleaner),
            None,
            ActorTestHooks::default(),
        )
    }

    /// 创建注入配置仓库的测试 actor。
    #[doc(hidden)]
    pub fn spawn_with_config_repository_for_test(
        store: NodeStore,
        listen_address: SocketAddr,
        cache_root: &Path,
        repository: Box<dyn NodeConfigRepositoryAccess>,
    ) -> (NodeEngineHandle, JoinHandle<()>) {
        let (artifact_registry, disk_full_cleaner) = test_artifact_cleanup(cache_root);
        spawn_actor(
            store,
            None,
            listen_address,
            cache_root,
            &cache_root.join("runtime"),
            EnumeratorKind::WindowsWalker,
            default_media_extension_filter(),
            DiskReadConfig::default(),
            NodePostgresConfig::default(),
            1,
            Some(repository),
            artifact_registry,
            Some(disk_full_cleaner),
            None,
            ActorTestHooks::default(),
        )
    }

    /// 创建持有真实 WorkerPool 的生产 actor。
    pub fn spawn(
        store: NodeStore,
        worker_pool: WorkerPool,
        listen_address: SocketAddr,
        cache_root: &Path,
        enumerator: EnumeratorKind,
    ) -> (NodeEngineHandle, JoinHandle<()>) {
        let effective_worker_count = worker_pool.worker_process_ids().len().max(1);
        let artifact_registry = test_artifact_registry(cache_root);
        spawn_actor(
            store,
            Some(worker_pool),
            listen_address,
            cache_root,
            &cache_root.join("runtime"),
            enumerator,
            default_media_extension_filter(),
            DiskReadConfig::default(),
            NodePostgresConfig::default(),
            effective_worker_count,
            None,
            artifact_registry,
            None,
            None,
            ActorTestHooks::default(),
        )
    }

    #[cfg(feature = "test-hooks")]
    #[doc(hidden)]
    /// 创建首条 Base persist 可暂停的真实 actor，只供 shutdown 生命周期测试。
    pub fn spawn_with_first_persist_gate_for_test(
        store: NodeStore,
        worker_pool: WorkerPool,
        listen_address: SocketAddr,
        cache_root: &Path,
        enumerator: EnumeratorKind,
        first_persist_waiter: BasePersistTestWaiter,
    ) -> (NodeEngineHandle, JoinHandle<()>) {
        let effective_worker_count = worker_pool.worker_process_ids().len().max(1);
        let (artifact_registry, disk_full_cleaner) = test_artifact_cleanup(cache_root);
        spawn_actor(
            store,
            Some(worker_pool),
            listen_address,
            cache_root,
            &cache_root.join("runtime"),
            enumerator,
            default_media_extension_filter(),
            DiskReadConfig::default(),
            NodePostgresConfig::default(),
            effective_worker_count,
            None,
            artifact_registry,
            Some(disk_full_cleaner),
            None,
            ActorTestHooks {
                first_persist_waiter: Some(first_persist_waiter),
                background_outcome_waiter: None,
                analysis_publish_waiter: None,
                analysis_before_publish_waiter: None,
            },
        )
    }

    #[cfg(feature = "test-hooks")]
    #[doc(hidden)]
    /// 创建在后台 outcome 收束后可暂停完成命令投递的真实 actor。
    #[cfg_attr(not(test), allow(dead_code))]
    pub(crate) fn spawn_with_background_outcome_gate_for_test(
        store: NodeStore,
        worker_pool: WorkerPool,
        listen_address: SocketAddr,
        cache_root: &Path,
        enumerator: EnumeratorKind,
        background_outcome_waiter: BackgroundOutcomeTestWaiter,
    ) -> (NodeEngineHandle, JoinHandle<()>) {
        let effective_worker_count = worker_pool.worker_process_ids().len().max(1);
        let (artifact_registry, disk_full_cleaner) = test_artifact_cleanup(cache_root);
        spawn_actor(
            store,
            Some(worker_pool),
            listen_address,
            cache_root,
            &cache_root.join("runtime"),
            enumerator,
            default_media_extension_filter(),
            DiskReadConfig::default(),
            NodePostgresConfig::default(),
            effective_worker_count,
            None,
            artifact_registry,
            Some(disk_full_cleaner),
            None,
            ActorTestHooks {
                first_persist_waiter: None,
                background_outcome_waiter: Some(background_outcome_waiter),
                analysis_publish_waiter: None,
                analysis_before_publish_waiter: None,
            },
        )
    }

    #[cfg(feature = "test-hooks")]
    #[doc(hidden)]
    /// 创建在安装本地分析结果后可暂停 Runtime 终态发布的真实 actor。
    pub(crate) fn spawn_with_analysis_publish_gate_for_test(
        store: NodeStore,
        worker_pool: WorkerPool,
        listen_address: SocketAddr,
        cache_root: &Path,
        runtime_root: &Path,
        enumerator: EnumeratorKind,
        analysis_publish_waiter: AnalysisPublishTestWaiter,
    ) -> (NodeEngineHandle, JoinHandle<()>) {
        let effective_worker_count = worker_pool.worker_process_ids().len().max(1);
        let (artifact_registry, disk_full_cleaner) = test_artifact_cleanup(cache_root);
        spawn_actor(
            store,
            Some(worker_pool),
            listen_address,
            cache_root,
            runtime_root,
            enumerator,
            default_media_extension_filter(),
            DiskReadConfig::default(),
            NodePostgresConfig::default(),
            effective_worker_count,
            None,
            artifact_registry,
            Some(disk_full_cleaner),
            None,
            ActorTestHooks {
                first_persist_waiter: None,
                background_outcome_waiter: None,
                analysis_publish_waiter: Some(analysis_publish_waiter),
                analysis_before_publish_waiter: None,
            },
        )
    }

    #[cfg(feature = "test-hooks")]
    #[doc(hidden)]
    /// 创建在二筛收束、最终评估前可暂停本地分析的真实 actor。
    fn spawn_with_analysis_before_publish_gate_for_test(
        store: NodeStore,
        worker_pool: WorkerPool,
        listen_address: SocketAddr,
        cache_root: &Path,
        runtime_root: &Path,
        enumerator: EnumeratorKind,
        analysis_before_publish_waiter: AnalysisBeforePublishTestWaiter,
    ) -> (NodeEngineHandle, JoinHandle<()>) {
        let effective_worker_count = worker_pool.worker_process_ids().len().max(1);
        let (artifact_registry, disk_full_cleaner) = test_artifact_cleanup(cache_root);
        spawn_actor(
            store,
            Some(worker_pool),
            listen_address,
            cache_root,
            runtime_root,
            enumerator,
            default_media_extension_filter(),
            DiskReadConfig::default(),
            NodePostgresConfig::default(),
            effective_worker_count,
            None,
            artifact_registry,
            Some(disk_full_cleaner),
            None,
            ActorTestHooks {
                first_persist_waiter: None,
                background_outcome_waiter: None,
                analysis_publish_waiter: None,
                analysis_before_publish_waiter: Some(analysis_before_publish_waiter),
            },
        )
    }

    /// 使用独立数据运行目录创建受控 actor，验证瞬态任务不会落入缓存目录。
    #[doc(hidden)]
    pub fn spawn_with_runtime_root_for_test(
        store: NodeStore,
        worker_pool: WorkerPool,
        listen_address: SocketAddr,
        cache_root: &Path,
        runtime_root: &Path,
        enumerator: EnumeratorKind,
    ) -> (NodeEngineHandle, JoinHandle<()>) {
        let effective_worker_count = worker_pool.worker_process_ids().len().max(1);
        let (artifact_registry, disk_full_cleaner) = test_artifact_cleanup(cache_root);
        spawn_actor(
            store,
            Some(worker_pool),
            listen_address,
            cache_root,
            runtime_root,
            enumerator,
            default_media_extension_filter(),
            DiskReadConfig::default(),
            NodePostgresConfig::default(),
            effective_worker_count,
            None,
            artifact_registry,
            Some(disk_full_cleaner),
            None,
            ActorTestHooks::default(),
        )
    }
}

fn spawn_actor(
    store: NodeStore,
    worker_pool: Option<WorkerPool>,
    listen_address: SocketAddr,
    cache_root: &Path,
    runtime_root: &Path,
    enumerator: EnumeratorKind,
    media_extensions: MediaExtensionFilter,
    read_config: DiskReadConfig,
    postgres_config: NodePostgresConfig,
    effective_worker_count: usize,
    config_repository: Option<Box<dyn NodeConfigRepositoryAccess>>,
    artifact_registry: Arc<RegenerableArtifactRegistry>,
    disk_full_cleaner: Option<DiskFullCleaner>,
    // 生产 Node 重启时用于创建新 Pool 的不可变配置；可控测试池没有进程启动配置。
    worker_pool_config: Option<WorkerPoolConfig>,
    test_hooks: ActorTestHooks,
) -> (NodeEngineHandle, JoinHandle<()>) {
    let (commands, receiver) = mpsc::channel(64);
    let runtime_tasks = RuntimeTaskRegistry::new();
    let handle = NodeEngineHandle {
        commands: commands.clone(),
        runtime_tasks: runtime_tasks.clone(),
    };
    let worker_control = worker_pool.as_ref().map(WorkerPool::handle);
    let actor = tokio::spawn(run_actor(
        EngineState {
            store,
            worker_pool,
            worker_control,
            listen_address,
            cache_root: cache_root.to_path_buf(),
            runtime_root: runtime_root.to_path_buf(),
            enumerator,
            media_extensions,
            read_config,
            postgres_config,
            effective_worker_count,
            restarting: false,
            snapshots: BTreeMap::new(),
            active_job: None,
            latest_completed_scan: None,
            active_analysis: None,
            latest_analysis: load_latest_analysis(runtime_root),
            review_registry: ReviewRegistry::default(),
            commands: commands.downgrade(),
            config_repository,
            artifact_registry,
            disk_full_cleaner,
            worker_pool_config,
            runtime_tasks,
            test_hooks,
        },
        receiver,
    ));
    (handle, actor)
}

/// 创建测试用 artifact 注册表；不隐式启用生产磁盘满清理。
fn test_artifact_registry(cache_root: &Path) -> Arc<RegenerableArtifactRegistry> {
    fs::create_dir_all(cache_root).expect("Node test cache root must be creatable");
    let install_root = cache_root
        .parent()
        .expect("Node test cache root must have a distinct install parent");
    Arc::new(
        RegenerableArtifactRegistry::new(install_root, cache_root)
            .expect("Node test artifact roots must be absolute and nested"),
    )
}

fn test_artifact_cleanup(cache_root: &Path) -> (Arc<RegenerableArtifactRegistry>, DiskFullCleaner) {
    let registry = test_artifact_registry(cache_root);
    let cleaner = DiskFullCleaner::new(Arc::clone(&registry), SystemArtifactDiskResolver);
    (registry, cleaner)
}

struct EngineState {
    store: NodeStore,
    worker_pool: Option<WorkerPool>,
    worker_control: Option<WorkerPoolHandle>,
    /// 生产 Node 用于销毁旧 Pool 后创建替代 Pool 的启动参数。
    worker_pool_config: Option<WorkerPoolConfig>,
    listen_address: SocketAddr,
    cache_root: std::path::PathBuf,
    runtime_root: std::path::PathBuf,
    enumerator: EnumeratorKind,
    /// 启动时配置冻结的图片和视频扩展名并集，整次扫描保持不变。
    media_extensions: MediaExtensionFilter,
    read_config: DiskReadConfig,
    postgres_config: NodePostgresConfig,
    effective_worker_count: usize,
    restarting: bool,
    snapshots: BTreeMap<String, OwnedSnapshot>,
    active_job: Option<ActiveJob>,
    /// 当前进程最近一次成功收尾的扫描快照；失败和取消不会覆盖它。
    latest_completed_scan: Option<crate::scan::CompletedScanSnapshot>,
    /// 当前进程正在运行或刚刚结束的本地分析摘要，不持久化到 SQLite。
    active_analysis: Option<AnalysisRuntimeSummary>,
    /// 当前进程最近一次成功发布的本地分析及其结果元数据。
    latest_analysis: Option<LatestAnalysis>,
    /// 当前最近结果的进程内复核标记，不写入 SQLite 或 PostgreSQL。
    review_registry: ReviewRegistry,
    commands: mpsc::WeakSender<EngineCommand>,
    config_repository: Option<Box<dyn NodeConfigRepositoryAccess>>,
    artifact_registry: Arc<RegenerableArtifactRegistry>,
    disk_full_cleaner: Option<DiskFullCleaner>,
    runtime_tasks: RuntimeTaskRegistry,
    /// 默认构建为空的测试注入点。
    #[cfg_attr(not(feature = "test-hooks"), allow(dead_code))]
    test_hooks: ActorTestHooks,
}

enum EngineCommand {
    Protocol(proto::Envelope, oneshot::Sender<proto::Envelope>),
    ConnectionClosed,
    Restart(oneshot::Sender<Result<(), String>>),
    Shutdown(oneshot::Sender<()>),
    /// 返回当前进程完成扫描快照，仅供行为测试观察 actor 内部提交顺序。
    #[cfg(feature = "test-hooks")]
    LatestCompletedScanForTest(oneshot::Sender<Option<crate::scan::CompletedScanSnapshot>>),
    BackgroundFinished {
        identity: JobIdentity,
    },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum JobIdentity {
    /// 基础扫描只存在于当前进程，不对应 SQLite task 表。
    TransientScan(TaskId),
    /// 外部二筛只存在于当前进程，不对应 SQLite task 表。
    TransientStage2(TaskId),
    Analysis(AnalysisRunId),
}

/// 后台任务资源收束后交给 actor 发布的一次终态，避免状态与高水位错位。
struct BackgroundTerminal {
    /// 对应当前运行 ID 的唯一进程内状态入口。
    reporter: RuntimeTaskReporter,
    /// 任务的不可逆终态。
    state: RuntimeTaskState,
    /// 仅成功完成时携带的真实 SQLite outbox 高水位。
    outbox_high_seq: Option<u64>,
}

/// 当前进程本地分析可查询的轻量摘要；输入和候选仍由后台运行对象独占。
#[derive(Clone, Copy)]
struct AnalysisRuntimeSummary {
    /// 本次瞬态分析的 UUID v7 标识。
    run_id: AnalysisRunId,
    /// 当前进程内可观察的分析状态。
    status: AnalysisStatus,
    /// 冻结输入位置数。
    input_count: u64,
    /// 一筛候选对数。
    candidate_count: u64,
}

/// 当前最近结果的有效或损坏状态；失败和取消不会覆盖已有状态。
enum LatestAnalysis {
    /// 已通过验真的结果及其进程内摘要。
    Ready {
        /// 最近一次结果文件的进程内偏移读取器。
        reader: LatestAnalysisReader,
        /// 同一进程本地分析的摘要；重启恢复的结果没有该摘要。
        summary: Option<AnalysisRuntimeSummary>,
    },
    /// 结果文件存在但无法展示，保留错误以便窗口请求明确报告。
    Invalid {
        /// 结果验真失败的可读原因。
        message: String,
    },
}

/// 后台成功发布本地分析后交给 actor 安装的拥有型元数据。
struct BackgroundAnalysisOutcome {
    /// 发布前完成顺序校验的结果读取器。
    reader: LatestAnalysisReader,
    /// 最终分析报告及分组计数。
    report: LocalAnalysisReport,
    /// 冻结输入位置数。
    input_count: u64,
    /// 最终评估候选对数。
    candidate_count: u64,
}

/// 后台任务收束后的唯一交接值，扫描快照必须与终态一起被 actor 消费。
#[derive(Default)]
struct BackgroundOutcome {
    /// 成功扫描的清单快照；二筛和失败扫描均为空。
    completed_scan: Option<crate::scan::CompletedScanSnapshot>,
    /// 成功发布的瞬态本地分析；失败、取消和旧 SQLite 分析不填充。
    analysis: Option<BackgroundAnalysisOutcome>,
    /// 收束后才允许发布的运行终态。
    terminal: Option<BackgroundTerminal>,
}

struct ActiveJob {
    identity: JobIdentity,
    /// `run_background_job` 已完整返回，包含 task-local writer join 与 Store 恢复。
    completion: oneshot::Receiver<()>,
    cancellation: Option<ReadCancellationToken>,
    /// 后台任务收束后归还的唯一 Pool 所有权，只有 actor 可以取回。
    returned_pool: Arc<Mutex<Option<WorkerPool>>>,
    /// 后台任务收束后的唯一 outcome，完成和关机路径只能消费一次。
    outcome: Arc<Mutex<Option<BackgroundOutcome>>>,
}

enum BackgroundJob {
    Scan {
        task_id: TaskId,
        options: ScanOptions,
        enumerator: EnumeratorKind,
        /// 当前 Node 启动配置冻结的媒体扩展名过滤器。
        media_extensions: MediaExtensionFilter,
        contact_sheets: PathBuf,
        read_config: DiskReadConfig,
        postgres_config: NodePostgresConfig,
        effective_worker_count: usize,
        cancellation: ReadCancellationToken,
        runtime_reporter: RuntimeTaskReporter,
        artifact_registry: Arc<RegenerableArtifactRegistry>,
        /// 测试可注入磁盘满清理器；生产入口保持为空。
        disk_full_cleaner: Option<DiskFullCleaner>,
        /// 当前 Node 进程的瞬态任务目录根，不与缓存目录混用。
        runtime_root: PathBuf,
        /// 仅 feature 测试暂停首条 SQLite persist。
        #[cfg(feature = "test-hooks")]
        first_persist_waiter: Option<BasePersistTestWaiter>,
    },
    LocalAnalysis {
        /// 当前扫描快照构造出的拥有型瞬态分析运行。
        run: LocalAnalysisRun,
        /// 后续二筛实现可选地携带已冻结批次；空值代表全缓存命中。
        stage2_plan: Option<Stage2BatchPlan>,
        /// 本地分析和 task-file runner 共用的取消令牌。
        cancellation: ReadCancellationToken,
        /// 当前进程瞬态 task-file 根目录。
        runtime_root: PathBuf,
        /// 本次二筛使用的磁盘读取配置。
        read_config: DiskReadConfig,
        /// 最近一次结果文件所在目录。
        results_root: PathBuf,
        runtime_reporter: RuntimeTaskReporter,
        contact_sheets: PathBuf,
        postgres_config: NodePostgresConfig,
        effective_worker_count: usize,
        /// 仅 feature 测试在二筛收束后、最终评估前暂停。
        #[cfg(feature = "test-hooks")]
        analysis_before_publish_waiter: Option<AnalysisBeforePublishTestWaiter>,
    },
    Stage2 {
        plan: Stage2BatchPlan,
        runtime_reporter: RuntimeTaskReporter,
        contact_sheets: PathBuf,
        postgres_config: NodePostgresConfig,
        /// 当前 Node 进程的瞬态 task-file 根目录。
        runtime_root: PathBuf,
        /// 本次二筛冻结的磁盘读取额度。
        read_config: DiskReadConfig,
        /// 当前 WorkerPool 的有效并发槽位。
        effective_worker_count: usize,
        /// 外部取消和关闭共享的唯一取消令牌。
        cancellation: ReadCancellationToken,
    },
}

impl BackgroundJob {
    const fn identity(&self) -> JobIdentity {
        match self {
            Self::Scan { task_id, .. } => JobIdentity::TransientScan(*task_id),
            Self::LocalAnalysis { run, .. } => JobIdentity::Analysis(run.run_id),
            Self::Stage2 { plan, .. } => JobIdentity::TransientStage2(plan.task_id),
        }
    }
}

async fn run_actor(mut state: EngineState, mut commands: mpsc::Receiver<EngineCommand>) {
    let progress_publisher = RuntimeProgressPublisher::new(state.runtime_tasks.clone());
    let mut progress_tick = tokio::time::interval(Duration::from_secs(2));
    progress_tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    progress_tick.tick().await;
    loop {
        let command = tokio::select! {
            command = commands.recv() => command,
            _ = progress_tick.tick() => {
                progress_publisher.tick();
                continue;
            }
        };
        let Some(command) = command else {
            break;
        };
        match command {
            EngineCommand::Protocol(request, reply) => {
                let _ = reply.send(state.handle_protocol(request).await);
            }
            EngineCommand::ConnectionClosed => state.snapshots.clear(),
            EngineCommand::Restart(reply) => {
                let _ = reply.send(state.restart_engine().await);
            }
            EngineCommand::Shutdown(reply) => {
                if let Some(pool) = state.stop_background_for_shutdown().await {
                    if let Err(error) = pool.shutdown().await {
                        tracing::error!(error = %error, "关闭 WorkerPool 失败");
                    }
                }
                let _ = reply.send(());
                break;
            }
            #[cfg(feature = "test-hooks")]
            EngineCommand::LatestCompletedScanForTest(reply) => {
                let _ = reply.send(state.latest_completed_scan.clone());
            }
            EngineCommand::BackgroundFinished { identity } => {
                state.finish_background(identity).await;
            }
        }
    }
}

impl EngineState {
    async fn handle_protocol(&mut self, request: proto::Envelope) -> proto::Envelope {
        let request_id = request.request_id;
        let result = match request.payload {
            Some(proto::envelope::Payload::Ping(ping)) => Ok(proto::envelope::Payload::Ping(ping)),
            Some(proto::envelope::Payload::NodeStatus(_)) => self.node_status(),
            Some(proto::envelope::Payload::CreateScan(scan)) => self.create_scan(scan).await,
            Some(proto::envelope::Payload::CancelTask(cancel)) => self.cancel_task(cancel).await,
            Some(proto::envelope::Payload::QueryTask(query)) => {
                let runtime_tasks = self.runtime_tasks.clone();
                let latest_completed_scan = self.latest_completed_scan.clone();
                Self::query_task(runtime_tasks, latest_completed_scan, query).await
            }
            Some(proto::envelope::Payload::ListTasks(query)) => {
                let runtime_tasks = self.runtime_tasks.clone();
                let latest_completed_scan = self.latest_completed_scan.clone();
                Self::list_tasks(runtime_tasks, latest_completed_scan, query).await
            }
            Some(proto::envelope::Payload::BrowsePaths(query)) => browse_paths(query),
            Some(proto::envelope::Payload::CreateLocalAnalysis(create)) => {
                self.create_local_analysis(create).await
            }
            Some(proto::envelope::Payload::QueryAnalysisRun(query)) => {
                self.query_analysis_run(query)
            }
            Some(proto::envelope::Payload::ReadLocalResultWindow(query)) => {
                self.read_local_result_window(query)
            }
            Some(proto::envelope::Payload::ListGroups(query)) => self.list_groups(query),
            Some(proto::envelope::Payload::ListGroupMembers(query)) => {
                self.list_group_members(query)
            }
            Some(proto::envelope::Payload::SaveReviewMark(mark)) => self.save_review_mark(mark),
            Some(proto::envelope::Payload::PrepareAnalysisInput(query)) => {
                self.prepare_analysis_input(query)
            }
            Some(proto::envelope::Payload::DispatchStage2(dispatch)) => {
                self.dispatch_stage2(dispatch).await
            }
            Some(proto::envelope::Payload::PullChanges(pull)) => self.pull_changes(pull),
            Some(proto::envelope::Payload::SyncAck(ack)) => self.sync_ack(ack),
            Some(proto::envelope::Payload::BeginSnapshot(_)) => self.begin_snapshot(),
            Some(proto::envelope::Payload::ReadSnapshotPage(page)) => self.read_snapshot_page(page),
            Some(proto::envelope::Payload::ReadFile(read)) => self.read_file(read),
            Some(proto::envelope::Payload::GetNodeConfig(_)) => self.get_node_config(),
            Some(proto::envelope::Payload::SaveNodeConfig(save)) => self.save_node_config(save),
            Some(proto::envelope::Payload::ListRuntimeTasks(query)) => {
                self.list_runtime_tasks(query).await
            }
            Some(proto::envelope::Payload::GetRuntimeTaskDetails(query)) => {
                self.get_runtime_task_details(query).await
            }
            Some(proto::envelope::Payload::CreateDeleteBatch(create)) => {
                self.create_delete_batch(create).await
            }
            Some(_) => Err((
                proto::ErrorCode::InvalidRequest,
                "该节点命令尚未由 actor 接入".to_owned(),
            )),
            None => Err((
                proto::ErrorCode::InvalidRequest,
                "Envelope 缺少 payload".to_owned(),
            )),
        };
        match result {
            Ok(payload) => proto::Envelope {
                request_id,
                payload: Some(payload),
            },
            Err((code, message)) => error_response(request_id, code, &message),
        }
    }

    fn get_node_config(&self) -> ProtocolResult {
        let repository = self
            .config_repository
            .as_ref()
            .ok_or_else(|| (proto::ErrorCode::Internal, "节点未装配配置仓库".to_owned()))?;
        let loaded = repository.snapshot().map_err(config_repository_error)?;
        let logical_cpu_count = std::thread::available_parallelism()
            .map(usize::from)
            .unwrap_or(1);
        let effective_worker_count = loaded
            .config
            .worker
            .effective_worker_count(logical_cpu_count);
        let config = proto::NodeConfigValue::try_from(&loaded.config).map_err(internal)?;
        Ok(proto::envelope::Payload::NodeConfigSnapshot(
            proto::NodeConfigSnapshot {
                machine_id: self.store.machine_id().as_str().to_owned(),
                version_sha256: loaded.version_sha256,
                config: Some(config),
                logical_cpu_count: u32::try_from(logical_cpu_count).unwrap_or(u32::MAX),
                effective_worker_count: u32::try_from(effective_worker_count).unwrap_or(u32::MAX),
            },
        ))
    }

    fn save_node_config(&self, save: proto::SaveNodeConfig) -> ProtocolResult {
        let repository = self
            .config_repository
            .as_ref()
            .ok_or_else(|| (proto::ErrorCode::Internal, "节点未装配配置仓库".to_owned()))?;
        let config = save
            .config
            .ok_or_else(|| invalid("保存请求缺少 config"))?
            .try_into()
            .map_err(invalid)?;
        let saved = repository
            .save_if_version(&save.expected_version_sha256, &config)
            .map_err(config_repository_error)?;
        Ok(proto::envelope::Payload::NodeConfigSaved(
            proto::NodeConfigSaved {
                machine_id: self.store.machine_id().as_str().to_owned(),
                saved_version_sha256: saved.version_sha256,
            },
        ))
    }

    fn node_status(&self) -> ProtocolResult {
        // NodeStatus 只反映本进程 registry；旧 SQLite 任务行不再代表当前活动任务。
        let (queued_items, running_items) = self.runtime_tasks.activity_counts();
        let worker_count = self
            .worker_control
            .as_ref()
            .map_or(0, |pool| pool.worker_process_ids().len());
        let busy_workers = self
            .worker_control
            .as_ref()
            .map_or(0, WorkerPoolHandle::busy_workers);
        Ok(proto::envelope::Payload::NodeStatus(proto::NodeStatus {
            machine_id: self.store.machine_id().as_str().to_owned(),
            listen_address: self.listen_address.to_string(),
            worker_count: worker_count as u32,
            busy_workers: busy_workers as u32,
            queued_items,
            running_items,
            outbox_high_seq: self.store.outbox_high_seq().map_err(store_error)?,
            engine_restarting: self.restarting,
        }))
    }

    fn pull_changes(&self, pull: proto::PullChanges) -> ProtocolResult {
        let batch = self
            .store
            .pull_changes(pull.after_seq, pull.limit as usize)
            .map_err(store_error)?;
        Ok(proto::envelope::Payload::SyncChangeBatch(
            proto::SyncChangeBatch {
                changes: batch.changes,
                high_seq: batch.high_seq,
                pruned_through_seq: batch.pruned_through_seq,
            },
        ))
    }

    fn sync_ack(&mut self, ack: proto::SyncAck) -> ProtocolResult {
        self.store
            .ack_changes(ack.committed_seq)
            .map_err(store_error)?;
        Ok(proto::envelope::Payload::SyncAck(ack))
    }

    fn begin_snapshot(&mut self) -> ProtocolResult {
        self.snapshots.clear();
        let snapshot = self.store.begin_owned_snapshot().map_err(store_error)?;
        let high_seq = snapshot.high_seq();
        let token = Uuid::now_v7().to_string();
        self.snapshots.insert(token.clone(), snapshot);
        Ok(proto::envelope::Payload::BeginSnapshot(
            proto::BeginSnapshot {
                snapshot_token: token,
                snapshot_high_seq: high_seq,
            },
        ))
    }

    fn read_snapshot_page(&mut self, request: proto::ReadSnapshotPage) -> ProtocolResult {
        let page = self
            .snapshots
            .get(&request.snapshot_token)
            .ok_or_else(|| {
                (
                    proto::ErrorCode::NotFound,
                    "快照 token 不存在或连接已经结束".to_owned(),
                )
            })?
            .read_page(&request.table_name, &request.cursor, request.limit as usize)
            .map_err(store_error)?;
        let finished_snapshot = request.table_name == "deletion_tombstones" && page.done;
        let response = proto::ReadSnapshotPage {
            snapshot_token: request.snapshot_token.clone(),
            table_name: page.table_name,
            cursor: request.cursor,
            limit: request.limit,
            rows: page.rows.into_iter().map(|row| row.payload).collect(),
            next_cursor: page.next_cursor.unwrap_or_default(),
            done: page.done,
        };
        if finished_snapshot {
            self.snapshots.remove(&request.snapshot_token);
        }
        Ok(proto::envelope::Payload::ReadSnapshotPage(response))
    }

    async fn create_scan(&mut self, request: proto::CreateScan) -> ProtocolResult {
        self.ensure_job_idle()?;
        let roots = request
            .roots
            .iter()
            .map(DisplayPath::new)
            .collect::<Result<Vec<_>, _>>()
            .map_err(invalid)?;
        if roots.is_empty() {
            return Err(invalid("扫描任务至少需要一个根目录"));
        }
        let mut options = ScanOptions::new(roots);
        if request.force_recalculate {
            options = options.force_recompute();
        }
        let enumerator = match request.enumerator.as_str() {
            "" => self.enumerator,
            "windows_walker" => EnumeratorKind::WindowsWalker,
            "everything" => EnumeratorKind::Everything,
            _ => return Err(invalid("未知文件枚举器")),
        };
        let contact_sheets = self.cache_root.join("contact-sheets");
        let task_id = TaskId::new();
        let runtime_root = self.runtime_root.clone();
        let runtime_reporter = self
            .runtime_tasks
            .begin_with_id(
                task_id.as_uuid().to_string(),
                RuntimeTaskKind::BaseCompute,
                self.store.machine_id().clone(),
                "基础计算",
            )
            .await;
        let runtime_failure = runtime_reporter.clone();
        let cancellation = ReadCancellationToken::new();
        #[cfg(feature = "test-hooks")]
        let first_persist_waiter = self.test_hooks.first_persist_waiter.take();
        if let Err(error) = self.start_background(BackgroundJob::Scan {
            task_id,
            options,
            enumerator,
            media_extensions: self.media_extensions.clone(),
            contact_sheets,
            read_config: self.read_config.clone(),
            postgres_config: self.postgres_config.clone(),
            effective_worker_count: self.effective_worker_count,
            cancellation,
            runtime_reporter,
            artifact_registry: Arc::clone(&self.artifact_registry),
            disk_full_cleaner: self.disk_full_cleaner.clone(),
            runtime_root,
            #[cfg(feature = "test-hooks")]
            first_persist_waiter,
        }) {
            let _ = runtime_failure.finish(RuntimeTaskState::Failed).await;
            return Err(internal(error));
        }
        Ok(proto::envelope::Payload::TaskAccepted(
            proto::TaskAccepted {
                task_id: task_id.as_uuid().to_string(),
            },
        ))
    }

    async fn cancel_task(&mut self, request: proto::CancelTask) -> ProtocolResult {
        let task_id = parse_task_id(&request.task_id)?;
        let Some(active) = self.active_job.as_ref() else {
            return Err(not_found("当前进程没有可取消的瞬态任务"));
        };
        let active_identity = active.identity;
        let matches_active = match active_identity {
            JobIdentity::TransientScan(id) | JobIdentity::TransientStage2(id) => id == task_id,
            JobIdentity::Analysis(run_id) => run_id.as_uuid() == task_id.as_uuid(),
        };
        if !matches_active {
            return Err(not_found("任务不属于当前进程或已经结束"));
        }

        // 取消只作用于当前进程确实持有的瞬态任务，绝不回写旧 tasks 表。
        if let Some(cancellation) = &active.cancellation {
            cancellation.cancel();
        }
        if matches!(active_identity, JobIdentity::Analysis(_)) {
            // 本地分析只有进程内取消令牌；二筛 runner 会按自身 task_id 清理 Worker。
            return Ok(proto::envelope::Payload::CancelTask(request));
        }

        let cancel_gate = self
            .worker_control
            .as_ref()
            .map(|pool| pool.begin_task_cancel(&request.task_id));
        if let Some(cancel_gate) = cancel_gate {
            cancel_gate.commit();
        }
        if let Some(pool) = &self.worker_control {
            pool.cancel_task(&request.task_id).await.map_err(internal)?;
        }
        Ok(proto::envelope::Payload::CancelTask(request))
    }

    /// 查询当前进程仍可见的运行扫描或最近一次完成扫描，不读取 SQLite 任务表。
    async fn query_task(
        runtime_tasks: RuntimeTaskRegistry,
        latest_completed_scan: Option<crate::scan::CompletedScanSnapshot>,
        request: proto::QueryTask,
    ) -> ProtocolResult {
        let task_id = parse_task_id(&request.task_id)?;
        let task_id_text = task_id.as_uuid().to_string();
        let details = runtime_tasks
            .details(&task_id_text)
            .await
            .ok_or_else(|| not_found(format!("运行任务不存在: {}", request.task_id)))?;
        let summary = details
            .summary
            .ok_or_else(|| not_found("运行任务摘要不存在"))?;
        let is_latest_completed = latest_completed_scan
            .as_ref()
            .is_some_and(|scan| scan.task_id == task_id && summary.state == "completed");
        let has_runtime_highwater = summary.outbox_high_seq.is_some();
        if summary.state != "running" && !is_latest_completed && !has_runtime_highwater {
            return Err(not_found(format!("运行任务不存在: {}", request.task_id)));
        }
        let outbox_high_seq = summary.outbox_high_seq.unwrap_or_else(|| {
            latest_completed_scan
                .as_ref()
                .filter(|scan| scan.task_id == task_id)
                .map_or(0, |scan| scan.outbox_high_seq)
        });
        Ok(proto::envelope::Payload::QueryTask(proto::QueryTask {
            task_id: request.task_id,
            task: Some(runtime_task_summary(summary, outbox_high_seq)),
        }))
    }

    /// 列出当前进程 registry 中的运行任务，保留原协议分页字段但不查询 SQLite 任务表。
    async fn list_tasks(
        runtime_tasks: RuntimeTaskRegistry,
        latest_completed_scan: Option<crate::scan::CompletedScanSnapshot>,
        request: proto::ListTasks,
    ) -> ProtocolResult {
        let runtime_tasks = runtime_tasks.list().await;
        let start = if request.cursor.is_empty() {
            0
        } else {
            runtime_tasks
                .iter()
                .position(|task| task.runtime_task_id == request.cursor)
                .map(|index| index + 1)
                .ok_or_else(|| invalid("任务分页游标不存在"))?
        };
        let limit = if request.limit == 0 {
            100
        } else {
            request.limit.min(1_000) as usize
        };
        let end = start.saturating_add(limit).min(runtime_tasks.len());
        let tasks = runtime_tasks[start..end]
            .iter()
            .cloned()
            .map(|summary| {
                let outbox_high_seq = summary.outbox_high_seq.unwrap_or_else(|| {
                    latest_completed_scan
                        .as_ref()
                        .filter(|scan| {
                            scan.task_id.as_uuid().to_string() == summary.runtime_task_id
                                && summary.state == "completed"
                        })
                        .map_or(0, |scan| scan.outbox_high_seq)
                });
                runtime_task_summary(summary, outbox_high_seq)
            })
            .collect::<Vec<_>>();
        let next_cursor = if end < runtime_tasks.len() {
            runtime_tasks[end - 1].runtime_task_id.clone()
        } else {
            String::new()
        };
        Ok(proto::envelope::Payload::ListTasks(proto::ListTasks {
            cursor: request.cursor,
            limit: request.limit,
            tasks,
            next_cursor,
        }))
    }

    async fn list_runtime_tasks(&mut self, request: proto::ListRuntimeTasks) -> ProtocolResult {
        let tasks = self.runtime_tasks.list().await;
        let start = if request.cursor.is_empty() {
            0
        } else {
            tasks
                .iter()
                .position(|task| task.runtime_task_id == request.cursor)
                .map(|index| index + 1)
                .ok_or_else(|| invalid("运行任务分页游标不存在"))?
        };
        let limit = if request.limit == 0 {
            100
        } else {
            request.limit.min(1_000) as usize
        };
        let end = start.saturating_add(limit).min(tasks.len());
        let page = tasks[start..end].to_vec();
        let next_cursor = if end < tasks.len() {
            page.last()
                .map(|task| task.runtime_task_id.clone())
                .unwrap_or_default()
        } else {
            String::new()
        };
        Ok(proto::envelope::Payload::ListRuntimeTasks(
            proto::ListRuntimeTasks {
                cursor: request.cursor,
                limit: request.limit,
                tasks: page,
                next_cursor,
            },
        ))
    }

    async fn get_runtime_task_details(
        &mut self,
        request: proto::GetRuntimeTaskDetails,
    ) -> ProtocolResult {
        let details = self
            .runtime_tasks
            .details(&request.runtime_task_id)
            .await
            .ok_or_else(|| {
                (
                    proto::ErrorCode::NotFound,
                    format!("运行任务不存在: {}", request.runtime_task_id),
                )
            })?;
        Ok(proto::envelope::Payload::GetRuntimeTaskDetails(
            proto::GetRuntimeTaskDetails {
                runtime_task_id: request.runtime_task_id,
                details: Some(details),
            },
        ))
    }

    async fn create_local_analysis(
        &mut self,
        request: proto::CreateLocalAnalysis,
    ) -> ProtocolResult {
        self.ensure_job_idle()?;
        if request.group_kind == proto::GroupKind::Unspecified as i32 {
            return Err(invalid("必须选择分析结果类型"));
        }
        let tasks = request
            .scan_task_ids
            .iter()
            .map(|value| parse_task_id(value))
            .collect::<Result<Vec<_>, _>>()?;
        let thresholds = request
            .thresholds
            .map(Thresholds::try_from)
            .transpose()
            .map_err(invalid)?
            .unwrap_or_default();
        let snapshot = self
            .latest_completed_scan
            .clone()
            .ok_or_else(|| not_found("当前进程没有最近一次完成扫描"))?;
        let created_at_ms = u64::try_from(now_ms()).unwrap_or_default();
        let run = prepare_current_scan_analysis(
            &self.store,
            snapshot.task_id,
            snapshot.library_revision,
            &snapshot.resolved_files,
            &tasks,
            thresholds,
            created_at_ms,
        )
        .map_err(internal)?;
        let missing = missing_stage2_items(&self.store, &run).map_err(internal)?;
        let stage2_plan = if missing.is_empty() {
            None
        } else {
            Some(begin_stage2_batch(&mut self.store, &missing, now_ms()).map_err(internal)?)
        };
        let run_id = run.run_id;
        let input_count = run.inputs.len() as u64;
        let candidate_count = run.candidates.len() as u64;
        let summary = AnalysisRuntimeSummary {
            run_id,
            status: AnalysisStatus::CollectingStage1,
            input_count,
            candidate_count,
        };
        self.active_analysis = Some(summary);
        let runtime_reporter = self
            .runtime_tasks
            .begin_with_id(
                run_id.as_uuid().to_string(),
                RuntimeTaskKind::LocalAnalysis,
                self.store.machine_id().clone(),
                "重复文件清单",
            )
            .await;
        let runtime_failure = runtime_reporter.clone();
        let cancellation = ReadCancellationToken::new();
        #[cfg(feature = "test-hooks")]
        let analysis_before_publish_waiter = self.test_hooks.analysis_before_publish_waiter.take();
        if let Err(error) = self.start_background(BackgroundJob::LocalAnalysis {
            run,
            stage2_plan,
            cancellation,
            runtime_root: self.runtime_root.clone(),
            read_config: self.read_config.clone(),
            results_root: results_root_from_runtime(&self.runtime_root).map_err(internal)?,
            runtime_reporter,
            contact_sheets: self.cache_root.join("contact-sheets"),
            postgres_config: self.postgres_config.clone(),
            effective_worker_count: self.effective_worker_count,
            #[cfg(feature = "test-hooks")]
            analysis_before_publish_waiter,
        }) {
            if let Some(active) = &mut self.active_analysis {
                if active.run_id == run_id {
                    active.status = AnalysisStatus::Partial;
                }
            }
            let _ = runtime_failure.finish(RuntimeTaskState::Failed).await;
            return Err(internal(error));
        }
        Ok(proto::envelope::Payload::QueryAnalysisRun(
            proto::QueryAnalysisRun {
                analysis_run_id: run_id.as_uuid().to_string(),
                state: analysis_status_name(summary.status).into(),
                input_count: summary.input_count,
                candidate_count: summary.candidate_count,
                error_text: String::new(),
            },
        ))
    }

    fn query_analysis_run(&self, request: proto::QueryAnalysisRun) -> ProtocolResult {
        let run_id = parse_analysis_id(&request.analysis_run_id)?;
        let summary = self
            .active_analysis
            .filter(|summary| summary.run_id == run_id)
            .or_else(|| {
                self.latest_analysis
                    .as_ref()
                    .and_then(|latest| match latest {
                        LatestAnalysis::Ready { reader, summary }
                            if reader.metadata().run_id == run_id =>
                        {
                            *summary
                        }
                        _ => None,
                    })
            })
            .ok_or_else(|| not_found(format!("当前进程分析不存在: {}", request.analysis_run_id)))?;
        Ok(proto::envelope::Payload::QueryAnalysisRun(
            proto::QueryAnalysisRun {
                analysis_run_id: request.analysis_run_id,
                state: analysis_status_name(summary.status).into(),
                input_count: summary.input_count,
                candidate_count: summary.candidate_count,
                error_text: String::new(),
            },
        ))
    }

    /// 从当前进程最近结果读取一个只读组或成员窗口。
    fn read_local_result_window(
        &mut self,
        request: proto::ReadLocalResultWindow,
    ) -> ProtocolResult {
        if request.visible_count > MAX_LOCAL_RESULT_WINDOW_ROWS {
            return Err(invalid(format!(
                "本地结果窗口最多返回 {MAX_LOCAL_RESULT_WINDOW_ROWS} 行"
            )));
        }
        let run_id = parse_analysis_id(&request.analysis_run_id)?;
        let latest = self
            .latest_analysis
            .as_mut()
            .ok_or_else(|| not_found("当前进程没有最近一次成功结果"))?;
        let reader = match latest {
            LatestAnalysis::Ready { reader, .. } => reader,
            LatestAnalysis::Invalid { message } => {
                return Err((proto::ErrorCode::InvalidResult, message.clone()));
            }
        };
        if reader.metadata().run_id != run_id {
            return Err(not_found("分析结果不存在或已被替换"));
        }
        let kind = match proto::LocalResultWindowKind::try_from(request.kind) {
            Ok(proto::LocalResultWindowKind::LocalResultWindowGroups) => {
                LocalResultWindowKind::Groups(required_group_kind(request.group_kind)?)
            }
            Ok(proto::LocalResultWindowKind::LocalResultWindowMembers) => {
                if request.group_id.is_empty() {
                    return Err(invalid("成员窗口必须指定组 ID"));
                }
                LocalResultWindowKind::Members {
                    group_id: request.group_id.clone(),
                }
            }
            _ => return Err(invalid("本地结果窗口类型无效")),
        };
        let window = reader
            .read_window(kind, request.start_index, request.visible_count)
            .map_err(|error| match error {
                crate::analysis::AnalysisResultError::Io(error) => {
                    (proto::ErrorCode::Internal, error.to_string())
                }
                crate::analysis::AnalysisResultError::InvalidHeader(message)
                | crate::analysis::AnalysisResultError::InvalidRow(message)
                | crate::analysis::AnalysisResultError::InvalidFormat(message) => {
                    (proto::ErrorCode::InvalidResult, message)
                }
            })?;
        let current_revision = self.store.library_revision().map_err(store_error)?;
        let metadata = reader.metadata();
        let is_group_window =
            request.kind == proto::LocalResultWindowKind::LocalResultWindowGroups as i32;
        let (groups, members) = if is_group_window {
            (
                window
                    .groups
                    .into_iter()
                    .map(|group| proto::DuplicateGroup {
                        group_id: group.group_id,
                        kind: wire_group_kind(group.kind) as i32,
                        representative: Some((&group.representative).into()),
                        member_count: group.member_count,
                        reclaimable_bytes: group.reclaimable_bytes,
                    })
                    .collect(),
                Vec::new(),
            )
        } else {
            let keys = window
                .members
                .iter()
                .map(|member| member.content)
                .collect::<BTreeSet<_>>();
            let keys = keys.into_iter().collect::<Vec<_>>();
            let records = self
                .store
                .lookup_base_cache_by_keys(&keys)
                .map_err(store_error)?;
            if records.len() != keys.len() {
                return Err(internal("本地结果窗口基础缓存返回长度不匹配"));
            }
            let cache = keys.into_iter().zip(records).collect::<BTreeMap<_, _>>();
            let members = window
                .members
                .into_iter()
                .map(|member| {
                    let cached = cache.get(&member.content).and_then(Option::as_ref);
                    proto::GroupMember {
                        location: Some((&member.location).into()),
                        content: Some((&member.content).into()),
                        representative: member.representative,
                        stage1_score: member.stage1_score as f32,
                        phash_passed_parts: member.phash_passed_parts.map_or(0, u32::from),
                        stage2_score: member.stage2_score.unwrap_or_default() as f32,
                        review: wire_review(self.review_registry.get(
                            metadata.run_id,
                            metadata.library_revision,
                            &member.group_id,
                            &member.location,
                        )) as i32,
                        active: true,
                        width: cached.and_then(|record| record.width).unwrap_or_default(),
                        height: cached.and_then(|record| record.height).unwrap_or_default(),
                        quality: cached
                            .and_then(|record| record.stage1.as_ref())
                            .and_then(|stage1| match stage1 {
                                dedup_node_store::CompleteStage1::Image(image) => {
                                    Some(image.quality)
                                }
                                dedup_node_store::CompleteStage1::Video(_) => None,
                            })
                            .map_or(0, u32::from),
                        display_path: member.display_path,
                    }
                })
                .collect();
            (Vec::new(), members)
        };
        Ok(proto::envelope::Payload::ReadLocalResultWindow(
            proto::ReadLocalResultWindow {
                analysis_run_id: request.analysis_run_id,
                kind: request.kind,
                group_id: request.group_id,
                start_index: request.start_index,
                visible_count: request.visible_count,
                total_rows: window.total_rows,
                stale: metadata.library_revision != current_revision,
                result_revision: metadata.library_revision,
                current_revision,
                groups,
                members,
                group_kind: request.group_kind,
            },
        ))
    }

    fn list_groups(&self, _request: proto::ListGroups) -> ProtocolResult {
        // 结果组只从当前进程的本地结果窗口读取，禁止回查旧 SQLite 分组历史。
        Err(invalid("旧分组查询已停用，请使用 ReadLocalResultWindow"))
    }

    fn list_group_members(&self, _request: proto::ListGroupMembers) -> ProtocolResult {
        // 组成员同样只通过本地结果窗口提供，避免暴露旧 SQLite review/group 表。
        Err(invalid(
            "旧分组成员查询已停用，请使用 ReadLocalResultWindow",
        ))
    }

    fn save_review_mark(&mut self, request: proto::SaveReviewMark) -> ProtocolResult {
        let location: LocationKey = request
            .location
            .clone()
            .ok_or_else(|| invalid("复核请求缺少位置"))?
            .try_into()
            .map_err(invalid)?;
        let decision = match proto::ReviewDecision::try_from(request.decision) {
            Ok(proto::ReviewDecision::ReviewUndecided) => ReviewDecision::Undecided,
            Ok(proto::ReviewDecision::ReviewKeep) => ReviewDecision::Keep,
            Ok(proto::ReviewDecision::ReviewDelete) => ReviewDecision::Delete,
            Err(_) => return Err(invalid("复核决定无效")),
        };
        if request.group_id.is_empty() {
            return Err(invalid("复核请求缺少组 ID"));
        }
        if location.machine_id() != self.store.machine_id() {
            return Err(invalid("复核位置不属于当前节点"));
        }
        let run_id = parse_analysis_id(&request.analysis_run_id)?;
        let current_revision = self.store.library_revision().map_err(store_error)?;
        let (result_revision, member_exists) = {
            let latest = self
                .latest_analysis
                .as_mut()
                .ok_or_else(|| not_found("当前进程没有最近一次成功结果"))?;
            let reader = match latest {
                LatestAnalysis::Ready { reader, .. } => reader,
                LatestAnalysis::Invalid { message } => {
                    return Err((proto::ErrorCode::InvalidResult, message.clone()));
                }
            };
            if reader.metadata().run_id != run_id {
                return Err(not_found("分析结果不存在或已被替换"));
            }
            let result_revision = reader.metadata().library_revision;
            if result_revision != current_revision {
                return Err(invalid("最近分析结果已经过期，不能保存复核"));
            }
            (
                result_revision,
                reader
                    .find_member(&request.group_id, &location)
                    .map_err(map_local_result_error)?
                    .is_some(),
            )
        };
        if !member_exists {
            return Err(invalid("复核位置不属于最近结果的指定组"));
        }
        self.review_registry.set(
            run_id,
            result_revision,
            request.group_id.clone(),
            location,
            decision,
        );
        Ok(proto::envelope::Payload::SaveReviewMark(request))
    }

    /// 从当前进程最近完成扫描的快照批量构造分析输入，不读取历史任务或分析输入表。
    fn prepare_analysis_input(&self, request: proto::PrepareAnalysisInput) -> ProtocolResult {
        if request.scan_task_ids.len() != 1 {
            return Err(invalid("分析输入必须且只能选择最近一次完成扫描"));
        }
        let requested_task_id = parse_task_id(&request.scan_task_ids[0])?;
        let snapshot = self
            .latest_completed_scan
            .as_ref()
            .filter(|snapshot| snapshot.task_id == requested_task_id)
            .ok_or_else(|| not_found("最近一次完成扫描不存在"))?;

        let mut grouped = BTreeMap::<ContentKey, BTreeSet<LocationKey>>::new();
        for resolved in &snapshot.resolved_files {
            grouped
                .entry(resolved.content)
                .or_default()
                .insert(LocationKey::new(
                    self.store.machine_id().clone(),
                    resolved.scanned.normalized_path.clone(),
                ));
        }
        let keys = grouped.keys().copied().collect::<Vec<_>>();
        let records = self
            .store
            .lookup_base_cache_by_keys(&keys)
            .map_err(store_error)?;
        let start = if request.cursor.is_empty() {
            0
        } else {
            request.cursor.parse::<usize>().map_err(invalid)?
        };
        let limit = request.limit as usize;
        let entries = grouped.into_iter().collect::<Vec<_>>();
        let end = start.saturating_add(limit).min(entries.len());
        let mut inputs = Vec::with_capacity(end.saturating_sub(start));
        for (index, (content, locations)) in entries[start..end].iter().enumerate() {
            let cached = records
                .get(start + index)
                .and_then(Option::as_ref)
                .ok_or_else(|| internal("分析输入内容缓存不存在"))?;
            let completeness = classify_cache_completeness(cached, true);
            let stage1_complete =
                completeness.base_missing_parts & (BASE_MISSING_PROBE | BASE_MISSING_STAGE1) == 0;
            let stage2_complete = match cached.media_kind {
                dedup_core::MediaKind::Image => !completeness.image_stage2_missing,
                dedup_core::MediaKind::Video => {
                    stage1_complete && completeness.video_stage2_missing_slots == 0
                }
                dedup_core::MediaKind::Other => false,
            };
            inputs.push(proto::AnalysisInput {
                content: Some(content.into()),
                locations: locations.iter().map(Into::into).collect(),
                media_kind: wire_media_kind(cached.media_kind) as i32,
                stage1_complete,
                stage2_complete,
            });
        }
        Ok(proto::envelope::Payload::PrepareAnalysisInput(
            proto::PrepareAnalysisInput {
                analysis_run_id: request.analysis_run_id,
                cursor: request.cursor,
                limit: request.limit,
                inputs,
                next_cursor: if end < entries.len() {
                    end.to_string()
                } else {
                    String::new()
                },
                scan_task_ids: request.scan_task_ids,
            },
        ))
    }

    async fn dispatch_stage2(&mut self, request: proto::DispatchStage2) -> ProtocolResult {
        self.ensure_job_idle()?;
        parse_analysis_id(&request.analysis_run_id)?;
        let items = request
            .items
            .into_iter()
            .map(|item| {
                let content = item
                    .content
                    .ok_or_else(|| invalid("二筛项缺少内容键"))?
                    .try_into()
                    .map_err(invalid)?;
                let source = item
                    .source
                    .ok_or_else(|| invalid("二筛项缺少来源位置"))?
                    .try_into()
                    .map_err(invalid)?;
                let frame_slots = item
                    .frame_slots
                    .into_iter()
                    .map(|slot| u8::try_from(slot).map_err(invalid))
                    .collect::<Result<Vec<_>, _>>()?;
                Ok(Stage2BatchItem {
                    content,
                    source,
                    frame_slots,
                })
            })
            .collect::<Result<Vec<_>, (proto::ErrorCode, String)>>()?;
        let plan = begin_stage2_batch(&mut self.store, &items, now_ms()).map_err(internal)?;
        let task_id = plan.task_id;
        let runtime_reporter = self
            .runtime_tasks
            .begin_with_id(
                task_id.as_uuid().to_string(),
                RuntimeTaskKind::Stage2Compute,
                self.store.machine_id().clone(),
                "二次特征计算",
            )
            .await;
        let runtime_failure = runtime_reporter.clone();
        let cancellation = ReadCancellationToken::new();
        if let Err(error) = self.start_background(BackgroundJob::Stage2 {
            plan,
            runtime_reporter,
            contact_sheets: self.cache_root.join("contact-sheets"),
            postgres_config: self.postgres_config.clone(),
            runtime_root: self.runtime_root.clone(),
            read_config: self.read_config.clone(),
            effective_worker_count: self.effective_worker_count,
            cancellation,
        }) {
            let _ = runtime_failure.finish(RuntimeTaskState::Failed).await;
            return Err(internal(error));
        }
        Ok(proto::envelope::Payload::TaskAccepted(
            proto::TaskAccepted {
                task_id: task_id.as_uuid().to_string(),
            },
        ))
    }

    fn read_file(&self, request: proto::ReadFile) -> ProtocolResult {
        let location = request
            .location
            .ok_or_else(|| invalid("预览请求缺少位置"))?
            .try_into()
            .map_err(invalid)?;
        let kind = match request.file_kind.as_str() {
            "original" => PreviewKind::Original,
            "contact_sheet" => PreviewKind::ContactSheet,
            _ => return Err(invalid("未知预览类型")),
        };
        let chunk = PreviewService::new(&self.cache_root)
            .read(
                &self.store,
                &location,
                kind,
                request.offset,
                request.max_bytes as usize,
            )
            .map_err(internal)?;
        Ok(proto::envelope::Payload::FileChunk(proto::FileChunk {
            offset: chunk.offset,
            data: chunk.data,
            eof: chunk.eof,
        }))
    }

    /// 根据当前最近结果和内存复核标记冻结本地删除集合，不写旧删除历史表。
    fn build_local_delete_plan(
        &mut self,
        run_id: AnalysisRunId,
        items: &[proto::DeleteItem],
        mode: DeleteMode,
    ) -> Result<DeleteBatchPlan, (proto::ErrorCode, String)> {
        if items.is_empty() {
            return Err(invalid("本地删除没有确认项"));
        }
        let current_revision = self.store.library_revision().map_err(store_error)?;
        let group_ids = items
            .iter()
            .map(|item| item.group_id.clone())
            .collect::<BTreeSet<_>>();
        if group_ids.iter().any(String::is_empty) {
            return Err(invalid("本地删除项缺少组 ID"));
        }

        let mut requested = Vec::with_capacity(items.len());
        let mut locations = BTreeSet::new();
        for item in items {
            let location: LocationKey = item
                .location
                .clone()
                .ok_or_else(|| invalid("本地删除项缺少位置"))?
                .try_into()
                .map_err(invalid)?;
            let expected: ContentKey = item
                .expected_content
                .clone()
                .ok_or_else(|| invalid("本地删除项缺少内容键"))?
                .try_into()
                .map_err(invalid)?;
            if location.machine_id() != self.store.machine_id() {
                return Err(invalid("本地删除位置不属于当前节点"));
            }
            if !locations.insert(location.clone()) {
                return Err(invalid("本地删除包含重复位置"));
            }
            requested.push((item.group_id.clone(), location, expected));
        }

        let (result_revision, verified) = {
            let latest = self
                .latest_analysis
                .as_mut()
                .ok_or_else(|| not_found("当前进程没有最近一次成功结果"))?;
            let reader = match latest {
                LatestAnalysis::Ready { reader, .. } => reader,
                LatestAnalysis::Invalid { message } => {
                    return Err((proto::ErrorCode::InvalidResult, message.clone()));
                }
            };
            if reader.metadata().run_id != run_id {
                return Err(not_found("分析结果不存在或已被替换"));
            }
            let result_revision = reader.metadata().library_revision;
            if result_revision != current_revision {
                return Err(invalid("最近分析结果已经过期，不能创建删除队列"));
            }
            let mut verified = Vec::with_capacity(requested.len());
            for (group_id, location, expected) in &requested {
                let Some(member) = reader
                    .find_member(group_id, location)
                    .map_err(map_local_result_error)?
                else {
                    return Err(invalid("本地删除位置不属于最近结果的指定组"));
                };
                if member.content != *expected {
                    return Err(invalid("本地删除内容身份与最近结果不一致"));
                }
                verified.push((group_id.clone(), location.clone(), *expected));
            }
            (result_revision, verified)
        };

        let mut keep_locations = BTreeMap::new();
        for group_id in &group_ids {
            let marked = self.review_registry.locations_with_decision(
                run_id,
                result_revision,
                group_id,
                ReviewDecision::Keep,
            );
            let mut active = Vec::with_capacity(marked.len());
            for location in marked {
                if self
                    .store
                    .location_is_active(&location)
                    .map_err(store_error)?
                {
                    active.push(location);
                }
            }
            keep_locations.insert(group_id.clone(), active);
        }

        {
            let latest = self
                .latest_analysis
                .as_mut()
                .ok_or_else(|| not_found("当前进程没有最近一次成功结果"))?;
            let reader = match latest {
                LatestAnalysis::Ready { reader, .. } => reader,
                LatestAnalysis::Invalid { message } => {
                    return Err((proto::ErrorCode::InvalidResult, message.clone()));
                }
            };
            for (group_id, candidates) in &keep_locations {
                let mut has_keep = false;
                for location in candidates {
                    if reader
                        .find_member(group_id, location)
                        .map_err(map_local_result_error)?
                        .is_some()
                    {
                        has_keep = true;
                        break;
                    }
                }
                if !has_keep {
                    return Err(invalid(format!(
                        "重复组 {group_id} 必须至少保留一个活动 Keep"
                    )));
                }
            }
        }

        let mut planned = Vec::with_capacity(verified.len());
        for (group_id, location, expected) in verified {
            if self
                .review_registry
                .get(run_id, result_revision, &group_id, &location)
                != ReviewDecision::Delete
            {
                return Err(invalid("本地删除项必须先标记为 Delete"));
            }
            planned.push(PlannedDeleteItem {
                item_id: Uuid::now_v7().to_string(),
                group_id,
                location,
                expected,
            });
        }
        planned.sort_by(|left, right| {
            (
                &left.group_id,
                left.location.machine_id().as_str(),
                left.location.normalized_path(),
            )
                .cmp(&(
                    &right.group_id,
                    right.location.machine_id().as_str(),
                    right.location.normalized_path(),
                ))
        });
        Ok(DeleteBatchPlan {
            batch_id: Uuid::now_v7().to_string(),
            mode,
            items: planned,
        })
    }

    async fn create_delete_batch(&mut self, request: proto::CreateDeleteBatch) -> ProtocolResult {
        // 删除与扫描、分析、二筛共享同一后台资源；先统一检查空闲状态，避免删除后被扫描收尾复活。
        self.ensure_job_idle()?;
        let mode = match proto::DeleteMode::try_from(request.mode) {
            Ok(proto::DeleteMode::DeleteRecycleBin) => DeleteMode::RecycleBin,
            Ok(proto::DeleteMode::DeletePermanent) => DeleteMode::Permanent,
            _ => return Err(invalid("删除模式无效")),
        };
        let local = !request.analysis_run_id.is_empty();
        let external = !local;
        let plan = if local {
            if !request.delete_batch_id.is_empty() || !request.group_ids.is_empty() {
                return Err(invalid("本地删除只接受确认的精确成员集合"));
            }
            self.build_local_delete_plan(
                parse_analysis_id(&request.analysis_run_id)?,
                &request.items,
                mode,
            )?
        } else {
            if request.delete_batch_id.is_empty() {
                return Err(invalid("中心删除批次缺少 ID"));
            }
            if !request.group_ids.is_empty() {
                return Err(invalid("中心删除不接受组范围"));
            }
            let items = request
                .items
                .iter()
                .map(|item| {
                    Ok(PlannedDeleteItem {
                        item_id: item.delete_item_id.clone(),
                        group_id: item.group_id.clone(),
                        location: item
                            .location
                            .clone()
                            .ok_or_else(|| invalid("中心删除项缺少位置"))?
                            .try_into()
                            .map_err(invalid)?,
                        expected: item
                            .expected_content
                            .clone()
                            .ok_or_else(|| invalid("中心删除项缺少内容键"))?
                            .try_into()
                            .map_err(invalid)?,
                    })
                })
                .collect::<Result<Vec<_>, (proto::ErrorCode, String)>>()?;
            DeleteBatchPlan {
                batch_id: request.delete_batch_id.clone(),
                mode,
                items,
            }
        };
        let runtime_reporter = self
            .runtime_tasks
            .begin_with_id(
                plan.batch_id.clone(),
                RuntimeTaskKind::Delete,
                self.store.machine_id().clone(),
                "删除",
            )
            .await;
        let results = if external {
            DeleteEngine::execute_transient_with_runtime_using(
                &mut self.store,
                &self.runtime_root,
                &plan,
                &runtime_reporter,
                &crate::delete::SystemDeleteFilesystem,
            )
            .await
        } else {
            DeleteEngine::execute_transient_with_runtime_using(
                &mut self.store,
                &self.runtime_root,
                &plan,
                &runtime_reporter,
                &crate::delete::SystemDeleteFilesystem,
            )
            .await
        }
        .map_err(internal)?;
        let items = plan
            .items
            .iter()
            .map(|item| {
                let result = results.iter().find(|result| result.item_id == item.item_id);
                proto::DeleteItem {
                    delete_item_id: item.item_id.clone(),
                    group_id: item.group_id.clone(),
                    location: Some((&item.location).into()),
                    expected_content: Some((&item.expected).into()),
                    outcome: result
                        .map_or("failed", |result| delete_outcome_name(result.outcome))
                        .into(),
                    message: result
                        .and_then(|result| result.message.clone())
                        .unwrap_or_default(),
                }
            })
            .collect();
        Ok(proto::envelope::Payload::CreateDeleteBatch(
            proto::CreateDeleteBatch {
                delete_batch_id: plan.batch_id,
                mode: request.mode,
                items,
                analysis_run_id: request.analysis_run_id,
                group_ids: request.group_ids,
            },
        ))
    }

    fn ensure_job_idle(&self) -> Result<(), (proto::ErrorCode, String)> {
        if self.worker_control.is_none() {
            return Err(invalid("测试节点没有 WorkerPool"));
        }
        if self.active_job.is_some() || self.worker_pool.is_none() {
            return Err(invalid("节点已有媒体任务正在后台运行"));
        }
        Ok(())
    }

    fn start_background(&mut self, job: BackgroundJob) -> Result<(), String> {
        if self.active_job.is_some() {
            return Err("节点已有媒体任务正在后台运行".into());
        }
        let store = self.store.reopen().map_err(|error| error.to_string())?;
        let mut worker_pool = self
            .worker_pool
            .take()
            .ok_or_else(|| "后台 WorkerPool owner 不可用".to_owned())?;
        let identity = job.identity();
        let cancellation = match &job {
            BackgroundJob::Scan { cancellation, .. }
            | BackgroundJob::Stage2 { cancellation, .. }
            | BackgroundJob::LocalAnalysis { cancellation, .. } => Some(cancellation.clone()),
        };
        #[cfg(feature = "test-hooks")]
        let background_outcome_waiter = self.test_hooks.background_outcome_waiter.take();
        let commands = self
            .commands
            .upgrade()
            .ok_or_else(|| "节点计算引擎已经关闭".to_owned())?;
        let (completion_sender, completion) = oneshot::channel();
        let returned_pool = Arc::new(Mutex::new(None));
        let background_pool = Arc::clone(&returned_pool);
        let outcome = Arc::new(Mutex::new(None));
        let background_outcome = Arc::clone(&outcome);
        tokio::spawn(async move {
            let outcome = run_background_job(store, &mut worker_pool, job).await;
            // 必须先确认 BaseStoreActor join、Store 恢复和终态准备全部结束，再触发 shutdown/归还 Pool。
            *background_pool.lock().expect("后台 Pool 归还锁未中毒") = Some(worker_pool);
            *background_outcome
                .lock()
                .expect("后台 outcome 归还锁未中毒") = Some(outcome);
            let _ = completion_sender.send(());
            #[cfg(feature = "test-hooks")]
            if let Some(waiter) = background_outcome_waiter {
                waiter.wait_before_background_finished().await;
            }
            let _ = commands
                .send(EngineCommand::BackgroundFinished { identity })
                .await;
        });
        self.active_job = Some(ActiveJob {
            identity,
            completion,
            cancellation,
            returned_pool,
            outcome,
        });
        Ok(())
    }

    async fn finish_background(&mut self, identity: JobIdentity) {
        let Some(active) = self
            .active_job
            .take_if(|active| active.identity == identity)
        else {
            return;
        };
        if let Some(worker_pool) = active
            .returned_pool
            .lock()
            .expect("后台 Pool 归还锁未中毒")
            .take()
        {
            self.worker_control = Some(worker_pool.handle());
            self.worker_pool = Some(worker_pool);
        }
        let outcome = active
            .outcome
            .lock()
            .expect("后台 outcome 归还锁未中毒")
            .take();
        self.publish_background_outcome(identity, outcome).await;
    }

    /// Pool、Store 和 task-file 目录均收束后，按快照、终态的固定顺序消费唯一 outcome。
    async fn publish_background_outcome(
        &mut self,
        identity: JobIdentity,
        outcome: Option<BackgroundOutcome>,
    ) {
        let Some(outcome) = outcome else {
            return;
        };
        if matches!(identity, JobIdentity::TransientScan(_))
            && let Some(completed_scan) = outcome.completed_scan
        {
            self.latest_completed_scan = Some(completed_scan);
        }
        #[cfg(feature = "test-hooks")]
        let analysis_publish_waiter = if matches!(identity, JobIdentity::Analysis(_)) {
            self.test_hooks.analysis_publish_waiter.take()
        } else {
            None
        };
        let has_analysis = outcome.analysis.is_some();
        if let Some(analysis) = outcome.analysis {
            let summary = AnalysisRuntimeSummary {
                run_id: analysis.report.run_id,
                status: analysis.report.status,
                input_count: analysis.input_count,
                candidate_count: analysis.candidate_count,
            };
            // 先安装成功结果和摘要，再向 RuntimeTaskReporter 发布 completed 事件。
            self.review_registry.clear();
            self.active_analysis = Some(summary);
            self.latest_analysis = Some(LatestAnalysis::Ready {
                reader: analysis.reader,
                summary: Some(summary),
            });
            #[cfg(feature = "test-hooks")]
            if let Some(waiter) = analysis_publish_waiter {
                waiter.wait_after_install().await;
            }
        }
        if matches!(identity, JobIdentity::Analysis(_)) && !has_analysis {
            if let Some(active) = &mut self.active_analysis {
                if let JobIdentity::Analysis(run_id) = identity
                    && active.run_id == run_id
                {
                    active.status =
                        outcome
                            .terminal
                            .as_ref()
                            .map_or(AnalysisStatus::Partial, |terminal| match terminal.state {
                                RuntimeTaskState::Cancelled => AnalysisStatus::Cancelled,
                                _ => AnalysisStatus::Partial,
                            });
                }
            }
        }
        if let Some(terminal) = outcome.terminal {
            let _ = match terminal.outbox_high_seq {
                Some(highwater) => {
                    terminal
                        .reporter
                        .finish_with_outbox_high_seq(terminal.state, highwater)
                        .await
                }
                None => terminal.reporter.finish(terminal.state).await,
            };
        }
    }

    /// 取消活动任务并等待后台完全收束，返回被 actor 独占的旧 Pool。
    async fn stop_background_for_shutdown(&mut self) -> Option<WorkerPool> {
        let Some(active) = self.active_job.take() else {
            return self.worker_pool.take();
        };
        let pool_task_id = match active.identity {
            JobIdentity::TransientScan(task_id) | JobIdentity::TransientStage2(task_id) => {
                task_id.as_uuid().to_string()
            }
            JobIdentity::Analysis(_) => String::new(),
        };
        if let Some(cancellation) = &active.cancellation {
            cancellation.cancel();
        }
        if !matches!(active.identity, JobIdentity::Analysis(_))
            && let Some(pool) = &self.worker_control
            && let Err(error) = pool.cancel_task(&pool_task_id).await
        {
            tracing::warn!(task_id = %pool_task_id, error = %error, "关机取消 WorkerPool 任务失败");
        }
        if active.completion.await.is_err() {
            // 发送端只会在后台 panic/异常销毁时消失；此处禁止伪造终态或指标清零。
            tracing::error!(task_id = %pool_task_id, "关机等待后台完整收束失败，未伪造运行终态");
        }
        self.worker_control = None;
        let worker_pool = active
            .returned_pool
            .lock()
            .expect("后台 Pool 归还锁未中毒")
            .take();
        let outcome = active
            .outcome
            .lock()
            .expect("后台 outcome 归还锁未中毒")
            .take();
        self.publish_background_outcome(active.identity, outcome)
            .await;
        worker_pool
    }

    async fn restart_engine(&mut self) -> Result<(), String> {
        self.restarting = true;
        // 活动任务先走与进程关闭相同的取消门禁，确保 Worker 事件和后台 Store 完整收束。
        let old_pool = self.stop_background_for_shutdown().await;
        self.worker_control = None;
        // 关闭失败也代表旧 Job 已释放；仍要尝试用保留的配置恢复新的计算引擎。
        let shutdown_error = match old_pool {
            Some(pool) => pool.shutdown().await.map_err(|error| error.to_string()),
            None => Ok(()),
        }
        .err();
        let result = match self.worker_pool_config.clone() {
            Some(config) => match WorkerPool::start(config).await {
                Ok(worker_pool) => {
                    self.worker_control = Some(worker_pool.handle());
                    self.worker_pool = Some(worker_pool);
                    match shutdown_error {
                        Some(error) => Err(format!(
                            "旧 WorkerPool 关闭异常但新计算引擎已经重建: {error}"
                        )),
                        None => Ok(()),
                    }
                }
                Err(error) => match shutdown_error {
                    Some(shutdown) => Err(format!(
                        "旧 WorkerPool 关闭异常且新计算引擎启动失败: {shutdown}; {error}"
                    )),
                    None => Err(error.to_string()),
                },
            },
            // 可控测试池已经关闭，不能冒充生产重建；测试只验证取消和收束边界。
            None => match shutdown_error {
                Some(error) => Err(format!("测试 WorkerPool 关闭异常: {error}")),
                None => Err("测试节点没有可重建的 WorkerPool".into()),
            },
        };
        self.restarting = false;
        result
    }
}

/// 将完整枚举结果按扫描根轮转后交给同一 BaseCompute 生产入口。
///
/// `rows` 必须是枚举器完成后的原始全局排序清单；该边界负责在冻结枚举总数前
/// 进行轮转，因此 actor 和受控集成测试不会各自维护一份排序逻辑。
#[allow(clippy::too_many_arguments)]
async fn run_enumerated_scan_to_base_compute<R, F, RF, RFuture, FF, FFuture>(
    store: &mut NodeStore,
    worker_pool: &mut WorkerPool,
    task_id: TaskId,
    options: ScanOptions,
    rows: Result<Vec<ScannedPath>, ScanError>,
    enumerate_started: u64,
    contact_sheet_root: &Path,
    remote_factory: RF,
    reader_factory: FF,
    read_config: &DiskReadConfig,
    cancellation: ReadCancellationToken,
    runtime_reporter: &RuntimeTaskReporter,
    artifact_registry: &Arc<RegenerableArtifactRegistry>,
    disk_full_cleaner: &DiskFullCleaner,
    now_ms: i64,
    #[cfg(feature = "test-hooks")] first_persist_waiter: Option<BasePersistTestWaiter>,
) -> Result<ScanSummary, ScanError>
where
    R: RemoteFeatureCache,
    F: PipelineFileReader,
    RF: FnOnce() -> RFuture,
    RFuture: Future<Output = (R, bool)>,
    FF: FnOnce() -> FFuture,
    FFuture: Future<Output = Result<(F, PipelineLimits), ScanError>>,
{
    // 轮转失败属于枚举边界错误，必须在冻结总数前持久化 EnumerateFiles 失败。
    let rows = match rows.and_then(|rows| interleave_rows_by_root(&options.roots, rows)) {
        Ok(rows) => rows,
        Err(error) => {
            let _ = store.save_task_stage(
                task_id,
                stored_stage(
                    RuntimeStage::EnumerateFiles,
                    StorePersistentStageState::Failed,
                    0,
                    None,
                    1,
                    0,
                    Some(enumerate_started),
                    Some(now_ms as u64),
                    None,
                ),
            );
            return Err(error);
        }
    };
    // 轮转只改变执行副本，枚举总数和字节总量保持原清单不变。
    let total = rows.len() as u64;
    let _ = runtime_reporter.freeze_base_compute_totals_nowait(total);
    let _ = store.save_task_stage(
        task_id,
        stored_stage(
            RuntimeStage::EnumerateFiles,
            StorePersistentStageState::Completed,
            total,
            Some(total),
            0,
            0,
            Some(enumerate_started),
            Some(now_ms as u64),
            None,
        ),
    );

    // 仅在枚举边界成功后创建远端缓存和读取器，保持错误路径无副作用。
    let (remote, remote_available) = remote_factory().await;
    let (reader, limits) = reader_factory().await?;
    #[cfg(feature = "test-hooks")]
    if let Some(first_persist_waiter) = first_persist_waiter {
        return BaseComputeEngine::run_existing_with_first_persist_gate_for_test(
            store,
            worker_pool,
            remote,
            remote_available,
            task_id,
            options,
            rows,
            contact_sheet_root,
            reader,
            limits,
            read_config,
            cancellation,
            runtime_reporter,
            artifact_registry,
            disk_full_cleaner,
            now_ms,
            first_persist_waiter,
        )
        .await;
    }
    BaseComputeEngine::run_existing(
        store,
        worker_pool,
        remote,
        remote_available,
        task_id,
        options,
        rows,
        contact_sheet_root,
        reader,
        limits,
        read_config,
        cancellation,
        runtime_reporter,
        artifact_registry,
        disk_full_cleaner,
        now_ms,
    )
    .await
}

#[cfg(feature = "test-hooks")]
#[doc(hidden)]
/// 让受控集成测试把原始枚举清单直接送入 actor 使用的生产边界。
#[allow(clippy::too_many_arguments)]
pub async fn run_enumerated_scan_to_base_compute_for_test<R, F, RF, RFuture, FF, FFuture>(
    store: &mut NodeStore,
    worker_pool: &mut WorkerPool,
    task_id: TaskId,
    options: ScanOptions,
    rows: Vec<ScannedPath>,
    contact_sheet_root: &Path,
    remote_factory: RF,
    reader_factory: FF,
    read_config: &DiskReadConfig,
    cancellation: ReadCancellationToken,
    runtime_reporter: &RuntimeTaskReporter,
    artifact_registry: &Arc<RegenerableArtifactRegistry>,
    disk_full_cleaner: &DiskFullCleaner,
    now_ms: i64,
) -> Result<ScanSummary, ScanError>
where
    R: RemoteFeatureCache,
    F: PipelineFileReader,
    RF: FnOnce() -> RFuture,
    RFuture: Future<Output = (R, bool)>,
    FF: FnOnce() -> FFuture,
    FFuture: Future<Output = Result<(F, PipelineLimits), ScanError>>,
{
    run_enumerated_scan_to_base_compute(
        store,
        worker_pool,
        task_id,
        options,
        Ok(rows),
        now_ms as u64,
        contact_sheet_root,
        remote_factory,
        reader_factory,
        read_config,
        cancellation,
        runtime_reporter,
        artifact_registry,
        disk_full_cleaner,
        now_ms,
        None,
    )
    .await
}

/// 先冻结全部根的物理盘 lane，再调用一次枚举器并保留逐行 lane 所有权。
fn enumerate_with_frozen_plan<F>(
    roots: &[DisplayPath],
    read_config: &DiskReadConfig,
    resolver: &dyn ScanRootStorageResolver,
    enumerate: F,
) -> Result<(Vec<ScannedPath>, Arc<Vec<PlannedScannedPath>>), ScanError>
where
    F: FnOnce(&[DisplayPath]) -> Result<Vec<ScannedPath>, ScanError>,
{
    let plan = ScanDiskPlan::build(roots, read_config, resolver)?;
    let planned_rows = plan.assign_all(enumerate(roots)?)?;
    let planned_rows = Arc::new(planned_rows);
    let rows = planned_rows
        .iter()
        .map(|planned| planned.scanned.clone())
        .collect();
    Ok((rows, planned_rows))
}

/// 受控测试使用的真实“先建计划、再枚举”生产边界。
#[cfg(feature = "test-hooks")]
#[doc(hidden)]
pub fn enumerate_with_frozen_plan_for_test<E: FileEnumerator>(
    enumerator: &E,
    roots: &[DisplayPath],
    read_config: &DiskReadConfig,
    resolver: &dyn ScanRootStorageResolver,
) -> Result<(Vec<ScannedPath>, Arc<Vec<PlannedScannedPath>>), ScanError> {
    enumerate_with_frozen_plan(roots, read_config, resolver, |roots| {
        enumerator.enumerate(roots)
    })
}

async fn run_background_job(
    store: NodeStore,
    worker_pool: &mut WorkerPool,
    job: BackgroundJob,
) -> BackgroundOutcome {
    match job {
        BackgroundJob::Scan {
            task_id,
            options,
            enumerator,
            media_extensions,
            contact_sheets,
            read_config,
            postgres_config,
            effective_worker_count,
            cancellation,
            runtime_reporter,
            artifact_registry,
            disk_full_cleaner,
            runtime_root,
            #[cfg(feature = "test-hooks")]
            first_persist_waiter,
        } => {
            let _ = runtime_reporter
                .start_stage_nowait(RuntimeStage::EnumerateFiles, RuntimeProgressUnit::Files);
            let enumerator =
                resolve_scan_enumerator_with(enumerator, ensure_everything_ready).await;
            // 物理存储计划必须先于第一次 Everything/Walker enumerate 建立；失败时不枚举。
            let enumerated = match enumerator {
                EnumeratorKind::WindowsWalker => enumerate_with_frozen_plan(
                    &options.roots,
                    &read_config,
                    &SystemScanRootStorageResolver,
                    |roots| FilteredWindowsWalker::new(media_extensions.clone()).enumerate(roots),
                ),
                EnumeratorKind::Everything => enumerate_with_frozen_plan(
                    &options.roots,
                    &read_config,
                    &SystemScanRootStorageResolver,
                    |roots| PreferredEverythingEnumerator::new(media_extensions).enumerate(roots),
                ),
            };
            let result = async {
                let (_, planned_rows) = match enumerated {
                    Ok(enumerated) => enumerated,
                    Err(error) => {
                        let state = if cancellation.is_cancelled() {
                            proto::RuntimeStageState::RuntimeStageSkipped
                        } else {
                            proto::RuntimeStageState::RuntimeStageFailed
                        };
                        let _ = runtime_reporter.finish_stage_nowait(
                            RuntimeStage::EnumerateFiles,
                            state,
                            None,
                        );
                        return Err(error);
                    }
                };
                let roots = options
                    .roots
                    .iter()
                    .map(|root| NormalizedPath::new(root.as_path()))
                    .collect::<Result<Vec<_>, _>>()
                    .map_err(|error| ScanError::InvalidResult(error.to_string()))?;
                let planned = Arc::try_unwrap(planned_rows)
                    .unwrap_or_else(|planned_rows| planned_rows.as_ref().clone());
                if cancellation.is_cancelled() {
                    let _ = runtime_reporter.finish_stage_nowait(
                        RuntimeStage::EnumerateFiles,
                        proto::RuntimeStageState::RuntimeStageSkipped,
                        Some(planned.len() as u64),
                    );
                    return Err(ScanError::Cancelled);
                }
                let _ = runtime_reporter.freeze_base_compute_totals_nowait(planned.len() as u64);
                let (reader, limits) = ScheduledFileReader::new_with_planned_rows(
                    &read_config,
                    effective_worker_count,
                    Arc::new(planned.clone()),
                )?;
                configure_base_compute_runtime(
                    &runtime_reporter,
                    worker_pool.worker_process_ids().len(),
                    worker_pool.cpu_budget(),
                    limits,
                    &read_config,
                )?;
                let reader = reader.with_runtime_reporter(runtime_reporter.clone());
                let (remote, remote_available) =
                    NodeRemoteFeatureCache::from_config(&postgres_config).await;
                run_task_file_scan_with_runtime(
                    store,
                    worker_pool,
                    reader.clone(),
                    reader,
                    remote,
                    TaskFileScanRunOptions {
                        task_id,
                        roots,
                        planned,
                        runtime_root,
                        force_recompute: options.force_recompute,
                        coordinator: TaskFileBaseCoordinatorOptions {
                            hash_capacity: limits.max_read_tasks(),
                            worker_capacity: effective_worker_count,
                            read_config: read_config.clone(),
                            persistence: TaskFileMediaPersistenceOptions {
                                contact_sheet_root: contact_sheets,
                                artifact_registry: Some(artifact_registry),
                                disk_full_cleaner,
                            },
                        },
                        persist_capacity: MAX_BASE_TASK_BATCH
                            .saturating_add(effective_worker_count),
                        now_ms: now_ms(),
                        remote_available,
                        #[cfg(feature = "test-hooks")]
                        first_persist_waiter,
                    },
                    cancellation,
                    &runtime_reporter,
                )
                .await
            }
            .await;
            if let Err(error) = &result
                && !matches!(error, ScanError::Cancelled)
            {
                tracing::error!(task_id = %task_id.as_uuid(), error = %error, "基础计算任务因基础设施错误停止");
                let _ = runtime_reporter.record_failure_nowait(RuntimeFailureUpdate {
                    stage: RuntimeStage::ComputeBaseFeatures,
                    display_path: String::new(),
                    message: error.to_string(),
                });
            }
            let runtime_state = match &result {
                Ok(_) => RuntimeTaskState::Completed,
                Err(ScanError::Cancelled) => RuntimeTaskState::Cancelled,
                Err(_) => RuntimeTaskState::Failed,
            };
            // 成功扫描由 actor 先安装最新快照，再发布带真实高水位的 Completed。
            match result {
                Ok(result) => {
                    let completed = result.completed;
                    BackgroundOutcome {
                        completed_scan: Some(completed.clone()),
                        analysis: None,
                        terminal: Some(BackgroundTerminal {
                            reporter: runtime_reporter,
                            state: RuntimeTaskState::Completed,
                            outbox_high_seq: Some(completed.outbox_high_seq),
                        }),
                    }
                }
                Err(_) => {
                    let _ = runtime_reporter.finish(runtime_state).await;
                    BackgroundOutcome::default()
                }
            }
        }
        BackgroundJob::LocalAnalysis {
            mut run,
            stage2_plan,
            cancellation,
            runtime_root,
            read_config,
            results_root,
            runtime_reporter,
            contact_sheets,
            postgres_config,
            effective_worker_count,
            #[cfg(feature = "test-hooks")]
            analysis_before_publish_waiter,
        } => {
            let mut store = store;
            let input_count = run.inputs.len() as u64;
            let candidate_count = run.candidates.len() as u64;
            let result = match stage2_plan {
                Some(plan) => {
                    let (remote, _) = NodeRemoteFeatureCache::from_config(&postgres_config).await;
                    let options = Stage2TaskFileRunOptions::new(
                        &runtime_root,
                        &contact_sheets,
                        &read_config,
                        effective_worker_count,
                        MAX_BASE_TASK_BATCH.saturating_add(effective_worker_count),
                        cancellation.clone(),
                        &SystemScanRootStorageResolver,
                    )
                    .with_runtime_reporter(&runtime_reporter);
                    match run_stage2_batch_production(
                        store,
                        plan,
                        worker_pool,
                        Some(&remote),
                        &options,
                    )
                    .await
                    {
                        Ok(stage2) => {
                            store = stage2.store;
                            #[cfg(feature = "test-hooks")]
                            if !cancellation.is_cancelled()
                                && let Some(waiter) = analysis_before_publish_waiter
                            {
                                // 二筛 runner 已收束；测试在此处发出取消，验证最终评估不会越过取消线性化点。
                                waiter.wait_before_publish().await;
                            }
                            let evaluation = if cancellation.is_cancelled() {
                                Err(AnalysisBlocked::InvalidState("本地分析已取消".into()))
                            } else {
                                evaluate_candidates(&store, &run.candidates, &run.thresholds)
                            };
                            match evaluation {
                                Ok((candidates, unresolved)) => {
                                    run.candidates = candidates;
                                    if unresolved != 0 {
                                        Err(AnalysisBlocked::Stage2Incomplete { unresolved })
                                    } else if cancellation.is_cancelled() {
                                        Err(AnalysisBlocked::InvalidState("本地分析已取消".into()))
                                    } else {
                                        publish_local_analysis_result_with_reader(
                                            &store,
                                            run,
                                            &results_root,
                                        )
                                    }
                                }
                                Err(error) => Err(error),
                            }
                        }
                        Err(error) => {
                            let message = error.to_string();
                            let _ = error.into_store();
                            Err(AnalysisBlocked::InvalidState(message))
                        }
                    }
                }
                None => {
                    let evaluation = if cancellation.is_cancelled() {
                        Err(AnalysisBlocked::InvalidState("本地分析已取消".into()))
                    } else {
                        evaluate_candidates(&store, &run.candidates, &run.thresholds)
                    };
                    match evaluation {
                        Ok((candidates, unresolved)) => {
                            run.candidates = candidates;
                            if unresolved != 0 {
                                Err(AnalysisBlocked::Stage2Incomplete { unresolved })
                            } else if cancellation.is_cancelled() {
                                Err(AnalysisBlocked::InvalidState("本地分析已取消".into()))
                            } else {
                                publish_local_analysis_result_with_reader(
                                    &store,
                                    run,
                                    &results_root,
                                )
                            }
                        }
                        Err(error) => Err(error),
                    }
                }
            };
            match result {
                Ok((_published, report, reader)) => {
                    let _ = runtime_reporter.update_overall_nowait(
                        input_count,
                        Some(input_count),
                        0,
                        report.skipped_incomplete as u64,
                    );
                    BackgroundOutcome {
                        completed_scan: None,
                        analysis: Some(BackgroundAnalysisOutcome {
                            reader,
                            report,
                            input_count,
                            candidate_count,
                        }),
                        terminal: Some(BackgroundTerminal {
                            reporter: runtime_reporter,
                            state: RuntimeTaskState::Completed,
                            outbox_high_seq: None,
                        }),
                    }
                }
                Err(error) => {
                    let _ = runtime_reporter.record_failure_nowait(RuntimeFailureUpdate {
                        stage: RuntimeStage::FinalCompare,
                        display_path: String::new(),
                        message: error.to_string(),
                    });
                    BackgroundOutcome {
                        completed_scan: None,
                        analysis: None,
                        terminal: Some(BackgroundTerminal {
                            reporter: runtime_reporter,
                            state: if cancellation.is_cancelled() {
                                RuntimeTaskState::Cancelled
                            } else {
                                RuntimeTaskState::Failed
                            },
                            outbox_high_seq: None,
                        }),
                    }
                }
            }
        }
        BackgroundJob::Stage2 {
            plan,
            runtime_reporter,
            contact_sheets,
            postgres_config,
            runtime_root,
            read_config,
            effective_worker_count,
            cancellation,
        } => {
            let (remote, _) = NodeRemoteFeatureCache::from_config(&postgres_config).await;
            let options = Stage2TaskFileRunOptions::new(
                &runtime_root,
                &contact_sheets,
                &read_config,
                effective_worker_count,
                MAX_BASE_TASK_BATCH.saturating_add(effective_worker_count),
                cancellation.clone(),
                &SystemScanRootStorageResolver,
            )
            .with_runtime_reporter(&runtime_reporter);
            let result =
                run_stage2_batch_production(store, plan, worker_pool, Some(&remote), &options)
                    .await;
            match result {
                Ok(result) => {
                    let completed = result.completed as u64;
                    let failed = result.failed as u64;
                    let highwater = result.outbox_high_seq;
                    let _ = result.run_id;
                    // 先释放 task-file runner 归还的独占连接，再记录终态前的最终统计。
                    drop(result.store);
                    let _ = runtime_reporter.update_overall_nowait(
                        completed,
                        Some(completed.saturating_add(failed)),
                        failed,
                        0,
                    );
                    BackgroundOutcome {
                        completed_scan: None,
                        analysis: None,
                        terminal: Some(BackgroundTerminal {
                            reporter: runtime_reporter,
                            state: RuntimeTaskState::Completed,
                            outbox_high_seq: Some(highwater),
                        }),
                    }
                }
                Err(error) => {
                    let message = error.to_string();
                    let _ = error.into_store();
                    let _ = runtime_reporter.record_failure_nowait(RuntimeFailureUpdate {
                        stage: RuntimeStage::ComputeStage2Features,
                        display_path: String::new(),
                        message,
                    });
                    BackgroundOutcome {
                        completed_scan: None,
                        analysis: None,
                        terminal: Some(BackgroundTerminal {
                            reporter: runtime_reporter,
                            state: if cancellation.is_cancelled() {
                                RuntimeTaskState::Cancelled
                            } else {
                                RuntimeTaskState::Failed
                            },
                            outbox_high_seq: None,
                        }),
                    }
                }
            }
        }
    }
}

async fn resolve_scan_enumerator_with<Ensure, EnsureFuture>(
    requested: EnumeratorKind,
    mut ensure_everything: Ensure,
) -> EnumeratorKind
where
    Ensure: FnMut() -> EnsureFuture,
    EnsureFuture: std::future::Future<Output = bool>,
{
    match requested {
        EnumeratorKind::WindowsWalker => EnumeratorKind::WindowsWalker,
        EnumeratorKind::Everything if ensure_everything().await => EnumeratorKind::Everything,
        EnumeratorKind::Everything => EnumeratorKind::WindowsWalker,
    }
}

type ProtocolResult = Result<proto::envelope::Payload, (proto::ErrorCode, String)>;

fn store_error(error: StoreError) -> (proto::ErrorCode, String) {
    let code = if matches!(error, StoreError::SnapshotRequired { .. }) {
        proto::ErrorCode::SnapshotRequired
    } else {
        proto::ErrorCode::Internal
    };
    (code, error.to_string())
}

fn config_repository_error(error: ConfigRepositoryError) -> (proto::ErrorCode, String) {
    let code = match &error {
        ConfigRepositoryError::VersionConflict { .. } => proto::ErrorCode::Conflict,
        ConfigRepositoryError::Core(_) | ConfigRepositoryError::RepositoryControlPath { .. } => {
            proto::ErrorCode::InvalidRequest
        }
        _ => proto::ErrorCode::Internal,
    };
    (code, error.to_string())
}

fn invalid(error: impl std::fmt::Display) -> (proto::ErrorCode, String) {
    (proto::ErrorCode::InvalidRequest, error.to_string())
}

/// 返回协议层统一的“当前进程不存在该任务”错误。
fn not_found(error: impl std::fmt::Display) -> (proto::ErrorCode, String) {
    (proto::ErrorCode::NotFound, error.to_string())
}

fn internal(error: impl std::fmt::Display) -> (proto::ErrorCode, String) {
    (proto::ErrorCode::Internal, error.to_string())
}

fn parse_task_id(value: &str) -> Result<TaskId, (proto::ErrorCode, String)> {
    Uuid::parse_str(value)
        .map(TaskId::from_uuid)
        .map_err(invalid)
}

fn parse_analysis_id(value: &str) -> Result<AnalysisRunId, (proto::ErrorCode, String)> {
    Uuid::parse_str(value)
        .map(AnalysisRunId::from_uuid)
        .map_err(invalid)
}

fn cursor(value: &str) -> Option<&str> {
    (!value.is_empty()).then_some(value)
}

/// 把当前进程 registry 的运行摘要转换为旧任务查询协议，避免重新读取 SQLite 任务表。
fn runtime_task_summary(
    summary: proto::RuntimeTaskSummary,
    outbox_high_seq: u64,
) -> proto::TaskSummary {
    let state = wire_runtime_task_status(&summary.state);
    proto::TaskSummary {
        task_id: summary.runtime_task_id,
        task_kind: summary.task_kind,
        state: state as i32,
        total_items: summary.overall_total,
        completed_items: summary.overall_completed,
        failed_items: summary.overall_failed,
        skipped_items: summary.overall_skipped,
        outbox_high_seq,
    }
}

/// 把 registry 的运行态字符串转换为兼容的任务状态枚举。
fn wire_runtime_task_status(state: &str) -> proto::TaskState {
    match state {
        "running" => proto::TaskState::TaskRunning,
        "completed" => proto::TaskState::TaskCompleted,
        "failed" => proto::TaskState::TaskFailed,
        "cancelled" => proto::TaskState::TaskCancelled,
        _ => proto::TaskState::Unspecified,
    }
}

const fn analysis_status_name(status: AnalysisStatus) -> &'static str {
    match status {
        AnalysisStatus::CollectingStage1 => "collecting_stage1",
        AnalysisStatus::Stage1Synced => "stage1_synced",
        AnalysisStatus::Screening => "screening",
        AnalysisStatus::Phase2Dispatched => "phase2_dispatched",
        AnalysisStatus::Phase2Synced => "phase2_synced",
        AnalysisStatus::Finalizing => "finalizing",
        AnalysisStatus::Completed => "completed",
        AnalysisStatus::Partial => "partial",
        AnalysisStatus::Cancelled => "cancelled",
    }
}

fn group_filter(value: i32) -> Result<Option<GroupKind>, (proto::ErrorCode, String)> {
    match proto::GroupKind::try_from(value) {
        Ok(proto::GroupKind::Unspecified) => Ok(None),
        Ok(proto::GroupKind::GroupExact) => Ok(Some(GroupKind::Exact)),
        Ok(proto::GroupKind::GroupSimilarImage) => Ok(Some(GroupKind::Image)),
        Ok(proto::GroupKind::GroupSimilarVideo) => Ok(Some(GroupKind::Video)),
        Err(_) => Err(invalid("重复组类型无效")),
    }
}

/// 将窗口请求中的组类型解析为必填的 Node 分组枚举。
fn required_group_kind(value: i32) -> Result<GroupKind, (proto::ErrorCode, String)> {
    group_filter(value)?.ok_or_else(|| invalid("组窗口必须指定分组类型"))
}

const fn wire_group_kind(kind: GroupKind) -> proto::GroupKind {
    match kind {
        GroupKind::Exact => proto::GroupKind::GroupExact,
        GroupKind::Image => proto::GroupKind::GroupSimilarImage,
        GroupKind::Video => proto::GroupKind::GroupSimilarVideo,
    }
}

const fn wire_review(review: ReviewDecision) -> proto::ReviewDecision {
    match review {
        ReviewDecision::Undecided => proto::ReviewDecision::ReviewUndecided,
        ReviewDecision::Keep => proto::ReviewDecision::ReviewKeep,
        ReviewDecision::Delete => proto::ReviewDecision::ReviewDelete,
    }
}

/// 将最近结果文件读取错误映射为 Node 协议错误，不把损坏结果当作空结果。
fn map_local_result_error(
    error: crate::analysis::AnalysisResultError,
) -> (proto::ErrorCode, String) {
    match error {
        crate::analysis::AnalysisResultError::Io(error) => {
            (proto::ErrorCode::Internal, error.to_string())
        }
        crate::analysis::AnalysisResultError::InvalidHeader(message)
        | crate::analysis::AnalysisResultError::InvalidRow(message)
        | crate::analysis::AnalysisResultError::InvalidFormat(message) => {
            (proto::ErrorCode::InvalidResult, message)
        }
    }
}

const fn wire_media_kind(kind: dedup_core::MediaKind) -> proto::MediaKind {
    match kind {
        dedup_core::MediaKind::Image => proto::MediaKind::MediaImage,
        dedup_core::MediaKind::Video => proto::MediaKind::MediaVideo,
        dedup_core::MediaKind::Other => proto::MediaKind::MediaOther,
    }
}

const fn delete_outcome_name(outcome: DeleteOutcome) -> &'static str {
    match outcome {
        DeleteOutcome::Recycled => "recycled",
        DeleteOutcome::Deleted => "deleted",
        DeleteOutcome::Skipped => "skipped",
        DeleteOutcome::Failed => "failed",
    }
}

fn browse_paths(request: proto::BrowsePaths) -> ProtocolResult {
    let mut paths = if request.parent_path.is_empty() {
        (b'A'..=b'Z')
            .map(|letter| PathBuf::from(format!("{}:\\", letter as char)))
            .filter(|path| path.exists())
            .collect::<Vec<_>>()
    } else {
        fs::read_dir(&request.parent_path)
            .map_err(internal)?
            .map(|entry| entry.map(|entry| entry.path()))
            .collect::<Result<Vec<_>, _>>()
            .map_err(internal)?
    };
    paths.sort();
    let start = if request.cursor.is_empty() {
        0
    } else {
        request.cursor.parse::<usize>().map_err(invalid)?
    };
    let end = start
        .saturating_add(request.limit as usize)
        .min(paths.len());
    let entries = paths[start..end]
        .iter()
        .map(|path| proto::PathEntry {
            display_path: path.to_string_lossy().into_owned(),
            is_directory: path.is_dir(),
        })
        .collect();
    Ok(proto::envelope::Payload::BrowsePaths(proto::BrowsePaths {
        parent_path: request.parent_path,
        cursor: request.cursor,
        limit: request.limit,
        entries,
        next_cursor: if end < paths.len() {
            end.to_string()
        } else {
            String::new()
        },
    }))
}

fn now_ms() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as i64
}

/// 从已解析的瞬态运行目录推导固定结果目录，不回退到 cache、当前目录或用户目录。
fn results_root_from_runtime(runtime_root: &Path) -> Result<PathBuf, &'static str> {
    runtime_root
        .parent()
        .map(|parent| parent.join("results"))
        .ok_or("瞬态运行目录必须存在结果目录父级")
}

/// 启动前精确清空当前 Node 的 transient runtime 根，并删除唯一的未完成分析文件。
///
/// 这里只删除 `latest-analysis.partial.tsv`，保留已经验证过的
/// `latest-analysis.result.tsv`，也不递归触碰 results 目录中的其它内容。
fn reset_transient_runtime_root(runtime_root: &Path) -> std::io::Result<()> {
    let reset_result = match fs::symlink_metadata(runtime_root) {
        Ok(metadata) => {
            if !metadata.is_dir() {
                return Err(std::io::Error::new(
                    std::io::ErrorKind::InvalidInput,
                    "Node runtime 路径不是目录",
                ));
            }
            #[cfg(windows)]
            {
                use std::os::windows::fs::MetadataExt;
                const FILE_ATTRIBUTE_REPARSE_POINT: u32 = 0x0400;
                if metadata.file_attributes() & FILE_ATTRIBUTE_REPARSE_POINT != 0 {
                    return Err(std::io::Error::new(
                        std::io::ErrorKind::InvalidInput,
                        "Node runtime 目录不能是重解析点",
                    ));
                }
            }
            #[cfg(not(windows))]
            if metadata.file_type().is_symlink() {
                return Err(std::io::Error::new(
                    std::io::ErrorKind::InvalidInput,
                    "Node runtime 目录不能是符号链接",
                ));
            }
            fs::remove_dir_all(runtime_root)?;
            fs::create_dir(runtime_root)
        }
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            fs::create_dir_all(runtime_root)
        }
        Err(error) => Err(error),
    };
    reset_result?;

    let results_root = results_root_from_runtime(runtime_root)
        .map_err(|message| std::io::Error::new(std::io::ErrorKind::InvalidInput, message))?;
    match fs::remove_file(results_root.join("latest-analysis.partial.tsv")) {
        Ok(()) => Ok(()),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(error),
    }
}

/// 启动时校验并恢复最近结果读取器；损坏文件只禁用窗口，不回退旧分组表。
fn load_latest_analysis(runtime_root: &Path) -> Option<LatestAnalysis> {
    let result_path = results_root_from_runtime(runtime_root)
        .ok()?
        .join("latest-analysis.result.tsv");
    match fs::metadata(&result_path) {
        Ok(metadata) if metadata.is_file() => {}
        Ok(_) => {
            return Some(LatestAnalysis::Invalid {
                message: format!("最近本地分析结果不是普通文件: {}", result_path.display()),
            });
        }
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return None,
        Err(error) => {
            return Some(LatestAnalysis::Invalid {
                message: format!("最近本地分析结果无法访问: {error}"),
            });
        }
    }
    match LatestAnalysisReader::open_verified(&result_path) {
        Ok(reader) => Some(LatestAnalysis::Ready {
            reader,
            summary: None,
        }),
        Err(error) => {
            tracing::warn!(path = %result_path.display(), %error, "最近本地分析结果校验失败");
            Some(LatestAnalysis::Invalid {
                message: format!("最近本地分析结果无效: {error}"),
            })
        }
    }
}

/// 初始化基础计算固定三阶段，保证刚创建的任务即可完整展示。
fn initialize_base_task_stages(store: &mut NodeStore, task_id: TaskId) -> Result<(), StoreError> {
    for stage in [
        RuntimeStage::EnumerateFiles,
        RuntimeStage::LookupBaseCache,
        RuntimeStage::ComputeBaseFeatures,
    ] {
        store.save_task_stage(
            task_id,
            stored_stage(
                stage,
                StorePersistentStageState::Waiting,
                0,
                None,
                0,
                0,
                None,
                None,
                None,
            ),
        )?;
    }
    Ok(())
}

/// 构造 Node SQLite 使用的固定阶段快照。
#[allow(clippy::too_many_arguments)]
fn stored_stage(
    stage: RuntimeStage,
    state: StorePersistentStageState,
    completed: u64,
    total: Option<u64>,
    failed: u64,
    skipped: u64,
    started_at_ms: Option<u64>,
    finished_at_ms: Option<u64>,
    warning_text: Option<String>,
) -> TaskStageWrite {
    TaskStageWrite {
        stage_id: stage.id().into(),
        state,
        completed,
        total,
        failed,
        skipped,
        started_at_ms,
        finished_at_ms,
        warning_text,
    }
}

fn error_response(request_id: u64, code: proto::ErrorCode, message: &str) -> proto::Envelope {
    proto::Envelope {
        request_id,
        payload: Some(proto::envelope::Payload::Error(proto::Error {
            code: code as i32,
            message: message.to_owned(),
        })),
    }
}

#[cfg(test)]
mod tests {
    use std::{cell::Cell, fs, future, time::Duration};

    #[cfg(feature = "test-hooks")]
    use rusqlite::Connection;

    #[cfg(feature = "test-hooks")]
    use std::{
        io::{self, Write},
        sync::{Arc, Mutex},
    };

    #[cfg(feature = "test-hooks")]
    use dedup_core::NormalizedPath;
    use dedup_media::{ImageStage1, ImageStage2, PdqHash};
    #[cfg(feature = "test-hooks")]
    use dedup_media_ffmpeg::MediaProbe;
    use dedup_node_store::{FeatureWrite, ImageStage1Fields, ScannedPath};
    #[cfg(feature = "test-hooks")]
    use tracing_subscriber::fmt::MakeWriter;

    use super::*;
    #[cfg(feature = "test-hooks")]
    use crate::DisabledRemoteFeatureCache;
    use crate::analysis::{
        AnalysisResultGroupKind, AnalysisResultHeader, AnalysisResultMode, AnalysisResultRow,
        AnalysisResultWriter,
    };
    use crate::worker::{Stage1Frame, Stage2Frame, Stage2Output};
    #[cfg(feature = "test-hooks")]
    use crate::{scan::BasePersistTestController, worker::BaseComputeOutput};

    #[tokio::test]
    async fn actor_loads_latest_result_reader_and_serves_window() {
        let directory = tempfile::tempdir().unwrap();
        let cache_root = directory.path().join("cache");
        let results_root = cache_root.join("results");
        fs::create_dir_all(&results_root).unwrap();
        let machine = MachineId::parse(&"ab".repeat(32)).unwrap();
        let header = AnalysisResultHeader {
            format_version: 1,
            analysis_id: AnalysisRunId::new(),
            library_revision: 42,
            analysis_mode: AnalysisResultMode::Local,
            created_at_ms: 1,
            thresholds: Thresholds::default(),
        };
        let mut writer = AnalysisResultWriter::begin(&results_root, &header).unwrap();
        writer
            .write_member(&AnalysisResultRow {
                group_kind: AnalysisResultGroupKind::Exact,
                group_id: "group-1".into(),
                representative: true,
                representative_content: ContentKey::new([1; 16], 1),
                location: LocationKey::new(
                    machine.clone(),
                    NormalizedPath::new(r"D:\Media\one.bin").unwrap(),
                ),
                display_path: r"D:\Media\one.bin".into(),
                content: ContentKey::new([1; 16], 1),
                stage1_score: 1.0,
                phash_passed_parts: None,
                stage2_score: None,
            })
            .unwrap();
        let published = writer.publish().unwrap();
        let (handle, actor) = NodeEngine::spawn_for_test(
            NodeStore::open_in_memory(machine).unwrap(),
            "127.0.0.1:39091".parse().unwrap(),
            &cache_root,
        );

        let response = handle
            .handle(proto::Envelope {
                request_id: 1,
                payload: Some(proto::envelope::Payload::ReadLocalResultWindow(
                    proto::ReadLocalResultWindow {
                        analysis_run_id: published.run_id.as_uuid().to_string(),
                        kind: proto::LocalResultWindowKind::LocalResultWindowGroups as i32,
                        start_index: 0,
                        visible_count: 10,
                        group_kind: proto::GroupKind::GroupExact as i32,
                        ..Default::default()
                    },
                )),
            })
            .await;
        let Some(proto::envelope::Payload::ReadLocalResultWindow(window)) = response.payload else {
            panic!("最近结果窗口必须返回窗口响应");
        };
        assert_eq!(window.total_rows, 1);
        assert!(window.stale);
        assert_eq!(window.groups[0].group_id, "group-1");
        assert_eq!(window.result_revision, 42);
        assert_eq!(window.current_revision, 0);

        let member_response = handle
            .handle(proto::Envelope {
                request_id: 3,
                payload: Some(proto::envelope::Payload::ReadLocalResultWindow(
                    proto::ReadLocalResultWindow {
                        analysis_run_id: published.run_id.as_uuid().to_string(),
                        kind: proto::LocalResultWindowKind::LocalResultWindowMembers as i32,
                        group_id: "group-1".into(),
                        visible_count: 10,
                        ..Default::default()
                    },
                )),
            })
            .await;
        let Some(proto::envelope::Payload::ReadLocalResultWindow(member_window)) =
            member_response.payload
        else {
            panic!("最近结果成员窗口必须返回窗口响应");
        };
        assert_eq!(member_window.total_rows, 1);
        assert_eq!(member_window.members[0].display_path, r"D:\Media\one.bin");

        let missing = handle
            .handle(proto::Envelope {
                request_id: 2,
                payload: Some(proto::envelope::Payload::ReadLocalResultWindow(
                    proto::ReadLocalResultWindow {
                        analysis_run_id: AnalysisRunId::new().as_uuid().to_string(),
                        kind: proto::LocalResultWindowKind::LocalResultWindowGroups as i32,
                        group_kind: proto::GroupKind::GroupExact as i32,
                        ..Default::default()
                    },
                )),
            })
            .await;
        assert!(matches!(
            missing.payload,
            Some(proto::envelope::Payload::Error(proto::Error {
                code,
                ..
            })) if code == proto::ErrorCode::NotFound as i32
        ));

        handle.shutdown().await.unwrap();
        actor.await.unwrap();
    }

    #[tokio::test]
    async fn actor_reports_invalid_result_for_corrupt_startup_file() {
        let directory = tempfile::tempdir().unwrap();
        let cache_root = directory.path().join("cache");
        let results_root = cache_root.join("results");
        fs::create_dir_all(&results_root).unwrap();
        fs::write(
            results_root.join("latest-analysis.result.tsv"),
            b"not-a-valid-result",
        )
        .unwrap();
        let machine = MachineId::parse(&"cd".repeat(32)).unwrap();
        let (handle, actor) = NodeEngine::spawn_for_test(
            NodeStore::open_in_memory(machine).unwrap(),
            "127.0.0.1:39092".parse().unwrap(),
            &cache_root,
        );

        let response = handle
            .handle(proto::Envelope {
                request_id: 4,
                payload: Some(proto::envelope::Payload::ReadLocalResultWindow(
                    proto::ReadLocalResultWindow {
                        analysis_run_id: AnalysisRunId::new().as_uuid().to_string(),
                        kind: proto::LocalResultWindowKind::LocalResultWindowGroups as i32,
                        group_kind: proto::GroupKind::GroupExact as i32,
                        ..Default::default()
                    },
                )),
            })
            .await;
        let Some(proto::envelope::Payload::Error(error)) = response.payload else {
            panic!("损坏结果必须返回错误响应");
        };
        assert_eq!(error.code, 7);
        assert!(!matches!(
            proto::ErrorCode::try_from(error.code),
            Ok(proto::ErrorCode::Internal | proto::ErrorCode::NotFound)
        ));

        handle.shutdown().await.unwrap();
        actor.await.unwrap();
    }

    /// 自定义配置路径必须成为运行时实际目录，不能回退到 AppLayout 默认目录。
    #[test]
    fn runtime_paths_follow_configured_data_cache_and_log_directories() {
        let directory = tempfile::tempdir().unwrap();
        let executable = directory.path().join("portable/node.exe");
        let layout = AppLayout::from_executable(&executable).unwrap();
        let mut config = NodeConfig::default();
        config.paths.data_path = "custom/data".into();
        config.paths.config_path = "custom/config.toml".into();
        config.paths.log_path = "custom/logs".into();
        config.paths.cache_path = "custom/cache".into();

        let paths = resolve_runtime_paths(&layout, &config).unwrap();

        assert_eq!(paths.data_path, layout.executable_dir().join("custom/data"));
        assert_eq!(
            paths.config_path,
            layout.executable_dir().join("custom/config.toml")
        );
        assert_eq!(paths.log_path, layout.executable_dir().join("custom/logs"));
        assert_eq!(
            paths.cache_path,
            layout.executable_dir().join("custom/cache")
        );
    }

    /// Node 启动只清理本次 runtime 子目录，最近一次结果文件必须继续保留。
    #[test]
    fn runtime_root_reset_preserves_latest_result_file() {
        let directory = tempfile::tempdir().unwrap();
        let data_root = directory.path().join("data/node");
        let runtime_root = data_root.join("runtime");
        let results_root = data_root.join("results");
        fs::create_dir_all(runtime_root.join("old-run")).unwrap();
        fs::create_dir_all(&results_root).unwrap();
        fs::write(results_root.join("latest-analysis.result.tsv"), b"latest").unwrap();
        fs::write(results_root.join("latest-analysis.partial.tsv"), b"partial").unwrap();
        fs::write(runtime_root.join("old-run/delete.tasks.tsv"), b"old").unwrap();

        reset_transient_runtime_root(&runtime_root).unwrap();

        assert!(runtime_root.is_dir());
        assert!(!runtime_root.join("old-run").exists());
        assert!(
            !results_root.join("latest-analysis.partial.tsv").exists(),
            "启动清理必须删除未完成分析 staging 文件"
        );
        assert_eq!(
            fs::read(results_root.join("latest-analysis.result.tsv")).unwrap(),
            b"latest"
        );
    }

    /// 保存 shutdown 生命周期测试的结构化 tracing 输出。
    #[cfg(feature = "test-hooks")]
    #[derive(Clone, Default)]
    struct SharedLogBuffer(Arc<Mutex<Vec<u8>>>);

    #[cfg(feature = "test-hooks")]
    impl SharedLogBuffer {
        /// 返回当前捕获的 UTF-8 日志文本。
        fn text(&self) -> String {
            String::from_utf8(self.0.lock().unwrap().clone()).unwrap()
        }
    }

    /// 把 tracing 字节追加到单测试共享缓冲区。
    #[cfg(feature = "test-hooks")]
    struct SharedLogWriter(SharedLogBuffer);

    #[cfg(feature = "test-hooks")]
    impl Write for SharedLogWriter {
        fn write(&mut self, buffer: &[u8]) -> io::Result<usize> {
            self.0.0.lock().unwrap().extend_from_slice(buffer);
            Ok(buffer.len())
        }

        fn flush(&mut self) -> io::Result<()> {
            Ok(())
        }
    }

    #[cfg(feature = "test-hooks")]
    impl<'a> MakeWriter<'a> for SharedLogBuffer {
        type Writer = SharedLogWriter;

        fn make_writer(&'a self) -> Self::Writer {
            SharedLogWriter(self.clone())
        }
    }

    /// 排空事件接收器并统计非 running 的终态事件。
    #[cfg(feature = "test-hooks")]
    fn drain_terminal_events(
        events: &mut tokio::sync::broadcast::Receiver<proto::RuntimeTaskChanged>,
    ) -> usize {
        let mut terminals = 0;
        while let Ok(event) = events.try_recv() {
            terminals += usize::from(event.state != "running");
        }
        terminals
    }

    /// 只统计指定 runtime task 的结构化终态日志，忽略并行 actor 测试噪声。
    #[cfg(feature = "test-hooks")]
    fn terminal_log_count(output: &SharedLogBuffer, runtime_task_id: &str) -> usize {
        output
            .text()
            .lines()
            .filter(|line| line.contains("运行任务进入终态") && line.contains(runtime_task_id))
            .count()
    }

    /// 构造受控 Worker 可提交的完整图片二筛结果。
    fn stage2_success_output(seed: u64) -> Stage2Output {
        Stage2Output {
            frames: vec![Stage2Frame {
                slot: 0,
                feature: Some(ImageStage2 {
                    phash_parts: [seed; 9],
                    sobel: [seed as f32; 128],
                }),
                error: None,
            }],
            regenerated_contact_sheet_jpeg: None,
        }
    }

    /// 构造两个相同一筛图片使用的基础 Worker 结果，让分析真实产生二筛候选。
    #[cfg(feature = "test-hooks")]
    fn image_base_output() -> BaseComputeOutput {
        BaseComputeOutput {
            probe: Some(MediaProbe {
                media_kind: dedup_core::MediaKind::Image,
                width: 2,
                height: 2,
                duration_ms: None,
            }),
            stage1_frames: Some(vec![Stage1Frame {
                slot: 0,
                feature: Some(ImageStage1 {
                    width: 2,
                    height: 2,
                    pdq: PdqHash::from_bytes([0; 32]),
                    quality: 100,
                }),
                error: None,
            }]),
            contact_sheet_jpeg: None,
        }
    }

    /// 读取本地分析相关旧表行数，验证瞬态分析不会写入历史状态表。
    #[cfg(feature = "test-hooks")]
    fn legacy_analysis_table_counts(connection: &Connection) -> Vec<(&'static str, i64)> {
        [
            "tasks",
            "task_items",
            "task_stages",
            "analysis_runs",
            "analysis_run_stages",
            "analysis_run_inputs",
            "candidate_pairs",
            "duplicate_groups",
            "group_members",
            "review_marks",
        ]
        .into_iter()
        .map(|table| {
            let count: i64 = connection
                .query_row(&format!("SELECT COUNT(*) FROM {table}"), [], |row| {
                    row.get(0)
                })
                .unwrap();
            (table, count)
        })
        .collect()
    }

    /// 构造枚举错误的 actor 边界夹具，确认基础计算依赖不会提前创建。
    #[cfg(feature = "test-hooks")]
    async fn assert_enumerated_error_stops_before_dependencies(
        rows: Result<Vec<ScannedPath>, ScanError>,
    ) {
        let directory = tempfile::tempdir().unwrap();
        let cache_root = directory.path().join("data/node/cache");
        let (artifacts, cleaner) = test_artifact_cleanup(&cache_root);
        let machine = MachineId::from_sha256([0xE7; 32]);
        let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
        let options = ScanOptions::new(vec![
            DisplayPath::new(r"H:\VirtualMedia").unwrap(),
            DisplayPath::new(r"I:\VirtualMedia").unwrap(),
        ]);
        let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
        initialize_base_task_stages(&mut store, task_id).unwrap();
        let (mut worker_pool, _started, _controller) = WorkerPool::controlled_batch_for_test(1);
        let registry = RuntimeTaskRegistry::new();
        let reporter = registry
            .begin(RuntimeTaskKind::Scan, machine, "枚举错误边界")
            .await;
        let remote_calls = Arc::new(Mutex::new(0usize));
        let remote_factory = {
            let remote_calls = Arc::clone(&remote_calls);
            move || -> future::Ready<(DisabledRemoteFeatureCache, bool)> {
                *remote_calls.lock().unwrap() += 1;
                panic!("枚举错误时不得创建 remote");
            }
        };
        let reader_calls = Arc::new(Mutex::new(0usize));
        let reader_factory = {
            let reader_calls = Arc::clone(&reader_calls);
            move || -> future::Ready<Result<(ScheduledFileReader, PipelineLimits), ScanError>> {
                *reader_calls.lock().unwrap() += 1;
                panic!("枚举错误时不得创建 reader");
            }
        };

        let result = run_enumerated_scan_to_base_compute(
            &mut store,
            &mut worker_pool,
            task_id,
            options,
            rows,
            10,
            &cache_root.join("contact-sheets"),
            remote_factory,
            reader_factory,
            &DiskReadConfig::default(),
            ReadCancellationToken::new(),
            &reporter,
            &artifacts,
            &cleaner,
            20,
            None,
        )
        .await;
        assert!(matches!(
            result,
            Err(ScanError::Enumeration(_)) | Err(ScanError::InvalidResult(_))
        ));
        assert_eq!(*remote_calls.lock().unwrap(), 0);
        assert_eq!(*reader_calls.lock().unwrap(), 0);
        let enumerate = store
            .task_stages(task_id)
            .unwrap()
            .into_iter()
            .find(|stage| stage.stage_id == "enumerate_files")
            .expect("枚举阶段必须持久化");
        assert_eq!(enumerate.state, StorePersistentStageState::Failed);
        assert_eq!(enumerate.failed, 1);
    }

    #[cfg(feature = "test-hooks")]
    #[tokio::test]
    async fn enumerated_rows_error_stops_before_base_dependencies() {
        assert_enumerated_error_stops_before_dependencies(Err(ScanError::Enumeration(
            "测试枚举失败".into(),
        )))
        .await;
    }

    #[cfg(feature = "test-hooks")]
    #[tokio::test]
    async fn enumerated_interleave_error_stops_before_base_dependencies() {
        let path = r"J:\Outside\item.bin";
        let row = ScannedPath::new(
            NormalizedPath::new(path).unwrap(),
            DisplayPath::new(path).unwrap(),
            1,
        );
        assert_enumerated_error_stops_before_dependencies(Ok(vec![row])).await;
    }

    #[tokio::test]
    async fn everything_readiness_is_checked_only_for_everything_scan_requests() {
        let checks = Cell::new(0);
        let selected = resolve_scan_enumerator_with(EnumeratorKind::WindowsWalker, || {
            checks.set(checks.get() + 1);
            future::ready(true)
        })
        .await;
        assert_eq!(selected, EnumeratorKind::WindowsWalker);
        assert_eq!(checks.get(), 0);

        let selected = resolve_scan_enumerator_with(EnumeratorKind::Everything, || {
            checks.set(checks.get() + 1);
            future::ready(true)
        })
        .await;
        assert_eq!(selected, EnumeratorKind::Everything);
        assert_eq!(checks.get(), 1);

        let selected = resolve_scan_enumerator_with(EnumeratorKind::Everything, || {
            checks.set(checks.get() + 1);
            future::ready(false)
        })
        .await;
        assert_eq!(selected, EnumeratorKind::WindowsWalker);
        assert_eq!(checks.get(), 2);
    }

    #[tokio::test]
    async fn scan_create_query_and_cancel_stay_responsive_while_worker_is_held() {
        let directory = tempfile::tempdir().unwrap();
        let scan_root = directory.path().join("scan");
        fs::create_dir(&scan_root).unwrap();
        fs::write(scan_root.join("held.bin"), b"held worker input").unwrap();
        let machine = MachineId::parse(&"c1".repeat(32)).unwrap();
        let store = NodeStore::open(&directory.path().join("node.db"), machine).unwrap();
        let observer = store.reopen().unwrap();
        let (pool, mut started) = WorkerPool::controlled_for_test();
        let (handle, actor) = NodeEngine::spawn(
            store,
            pool,
            "127.0.0.1:39091".parse().unwrap(),
            directory.path(),
            EnumeratorKind::WindowsWalker,
        );

        let create_handle = handle.clone();
        let root = scan_root.to_string_lossy().into_owned();
        let create = tokio::spawn(async move {
            create_handle
                .handle(proto::Envelope {
                    request_id: 1,
                    payload: Some(proto::envelope::Payload::CreateScan(proto::CreateScan {
                        roots: vec![root],
                        force_recalculate: false,
                        enumerator: "windows_walker".into(),
                    })),
                })
                .await
        });
        let (running_task_id, _) = tokio::time::timeout(Duration::from_secs(2), started.recv())
            .await
            .expect("扫描必须到达可控 Worker 屏障")
            .expect("可控 Worker 不应提前关闭");

        let accepted = tokio::time::timeout(Duration::from_secs(2), create)
            .await
            .expect("CreateScan 必须在 Worker 屏障释放前返回")
            .unwrap();
        let Some(proto::envelope::Payload::TaskAccepted(accepted)) = accepted.payload else {
            panic!("CreateScan 必须返回真实任务 ID");
        };
        assert_eq!(accepted.task_id, running_task_id);
        let registry = handle.runtime_tasks_for_test();

        let busy = handle
            .handle(proto::Envelope {
                request_id: 5,
                payload: Some(proto::envelope::Payload::CreateScan(proto::CreateScan {
                    roots: vec![scan_root.to_string_lossy().into_owned()],
                    force_recalculate: false,
                    enumerator: "windows_walker".into(),
                })),
            })
            .await;
        assert!(matches!(
            busy.payload,
            Some(proto::envelope::Payload::Error(proto::Error { code, .. }))
                if code == proto::ErrorCode::InvalidRequest as i32
        ));

        let query = handle
            .handle(proto::Envelope {
                request_id: 2,
                payload: Some(proto::envelope::Payload::QueryTask(proto::QueryTask {
                    task_id: accepted.task_id.clone(),
                    task: None,
                })),
            })
            .await;
        assert!(matches!(
            query.payload,
            Some(proto::envelope::Payload::QueryTask(proto::QueryTask {
                task: Some(proto::TaskSummary { state, .. }),
                ..
            })) if state == proto::TaskState::TaskRunning as i32
        ));

        let list = handle
            .handle(proto::Envelope {
                request_id: 6,
                payload: Some(proto::envelope::Payload::ListTasks(proto::ListTasks {
                    cursor: String::new(),
                    limit: 100,
                    tasks: Vec::new(),
                    next_cursor: String::new(),
                })),
            })
            .await;
        assert!(matches!(
            list.payload,
            Some(proto::envelope::Payload::ListTasks(proto::ListTasks { tasks, .. }))
                if tasks.iter().any(|task| task.task_id == running_task_id
                    && task.state == proto::TaskState::TaskRunning as i32)
        ));

        let cancel = tokio::time::timeout(
            Duration::from_secs(2),
            handle.handle(proto::Envelope {
                request_id: 3,
                payload: Some(proto::envelope::Payload::CancelTask(proto::CancelTask {
                    task_id: accepted.task_id.clone(),
                })),
            }),
        )
        .await
        .expect("CancelTask 必须在 Worker 仍被屏障控制时返回");
        assert!(matches!(
            cancel.payload,
            Some(proto::envelope::Payload::CancelTask(_))
        ));

        tokio::time::timeout(Duration::from_secs(2), async {
            loop {
                let finished = registry
                    .details(&accepted.task_id)
                    .await
                    .is_some_and(|details| {
                        details
                            .summary
                            .is_some_and(|summary| summary.state != "running")
                    });
                if finished {
                    break;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("取消后运行任务必须进入终态");

        let final_query = handle
            .handle(proto::Envelope {
                request_id: 4,
                payload: Some(proto::envelope::Payload::QueryTask(proto::QueryTask {
                    task_id: accepted.task_id,
                    task: None,
                })),
            })
            .await;
        assert!(matches!(
            final_query.payload,
            Some(proto::envelope::Payload::Error(proto::Error { code, .. }))
                if code == proto::ErrorCode::NotFound as i32
        ));

        handle.shutdown().await.unwrap();
        actor.await.unwrap();
        assert!(observer.page_tasks(None, 100).unwrap().items.is_empty());
    }

    #[tokio::test]
    async fn create_scan_uses_transient_runtime_and_does_not_write_task_tables() {
        let directory = tempfile::tempdir().unwrap();
        let scan_root = directory.path().join("scan");
        fs::create_dir(&scan_root).unwrap();
        fs::write(scan_root.join("held.mp4"), b"held worker input").unwrap();
        let database = directory.path().join("node.db");
        let cache_root = directory.path().join("cache");
        let data_root = directory.path().join("data");
        let runtime_root = data_root.join("runtime");
        let machine = MachineId::parse(&"c6".repeat(32)).unwrap();
        let store = NodeStore::open(&database, machine).unwrap();
        let observer = store.reopen().unwrap();
        let (pool, mut started) = WorkerPool::controlled_for_test();
        let (handle, actor) = NodeEngine::spawn_with_runtime_root_for_test(
            store,
            pool,
            "127.0.0.1:39091".parse().unwrap(),
            &cache_root,
            &runtime_root,
            EnumeratorKind::WindowsWalker,
        );

        let response = handle
            .handle(proto::Envelope {
                request_id: 60,
                payload: Some(proto::envelope::Payload::CreateScan(proto::CreateScan {
                    roots: vec![scan_root.to_string_lossy().into_owned()],
                    force_recalculate: false,
                    enumerator: "windows_walker".into(),
                })),
            })
            .await;
        let Some(proto::envelope::Payload::TaskAccepted(accepted)) = response.payload else {
            panic!("瞬态扫描必须返回业务任务 ID");
        };
        if tokio::time::timeout(Duration::from_secs(2), started.recv())
            .await
            .is_err()
        {
            let tasks = handle.runtime_tasks_for_test().list().await;
            let details = match tasks.first() {
                Some(task) => {
                    handle
                        .runtime_tasks_for_test()
                        .details(&task.runtime_task_id)
                        .await
                }
                None => None,
            };
            panic!(
                "扫描必须进入可控 Worker: tasks={tasks:?}, details={details:?}, runtime={:?}",
                runtime_root
            );
        }

        assert!(observer.page_tasks(None, 100).unwrap().items.is_empty());
        let run_id = TaskId::from_uuid(Uuid::parse_str(&accepted.task_id).unwrap());
        assert!(runtime_root.join(run_id.as_uuid().to_string()).is_dir());
        assert!(!cache_root.join(run_id.as_uuid().to_string()).exists());
        assert!(
            !cache_root
                .join("runtime")
                .join(run_id.as_uuid().to_string())
                .exists()
        );

        let cancel = handle
            .handle(proto::Envelope {
                request_id: 61,
                payload: Some(proto::envelope::Payload::CancelTask(proto::CancelTask {
                    task_id: accepted.task_id,
                })),
            })
            .await;
        assert!(matches!(
            cancel.payload,
            Some(proto::envelope::Payload::CancelTask(_))
        ));
        handle.shutdown().await.unwrap();
        actor.await.unwrap();
        assert!(observer.page_tasks(None, 100).unwrap().items.is_empty());
        assert!(!runtime_root.join(run_id.as_uuid().to_string()).exists());
    }

    #[cfg(feature = "test-hooks")]
    #[tokio::test]
    async fn completed_event_is_published_after_latest_scan_snapshot_is_saved() {
        let directory = tempfile::tempdir().unwrap();
        let scan_root = directory.path().join("scan");
        fs::create_dir(&scan_root).unwrap();
        fs::write(scan_root.join("completed.bin"), b"abc").unwrap();
        let machine = MachineId::parse(&"c7".repeat(32)).unwrap();
        let store = NodeStore::open(&directory.path().join("node.db"), machine).unwrap();
        let (pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
        let (handle, actor) = NodeEngine::spawn(
            store,
            pool,
            "127.0.0.1:39091".parse().unwrap(),
            directory.path(),
            EnumeratorKind::WindowsWalker,
        );
        let registry = handle.runtime_tasks_for_test();
        let mut events = registry.subscribe();

        let accepted = handle
            .handle(proto::Envelope {
                request_id: 62,
                payload: Some(proto::envelope::Payload::CreateScan(proto::CreateScan {
                    roots: vec![scan_root.to_string_lossy().into_owned()],
                    force_recalculate: false,
                    enumerator: "windows_walker".into(),
                })),
            })
            .await;
        let Some(proto::envelope::Payload::TaskAccepted(accepted)) = accepted.payload else {
            panic!("瞬态扫描必须返回业务任务 ID");
        };
        let (_, item_id) = tokio::time::timeout(Duration::from_secs(2), started.recv())
            .await
            .expect("扫描必须进入真实 Worker")
            .expect("可控 Worker 不应提前关闭");
        controller
            .base_source_read_complete(accepted.task_id.clone(), item_id.clone())
            .await;
        controller
            .complete_base(
                accepted.task_id.clone(),
                item_id,
                [
                    0x90, 0x01, 0x50, 0x98, 0x3c, 0xd2, 0x4f, 0xb0, 0xd6, 0x96, 0x3f, 0x7d, 0x28,
                    0xe1, 0x7f, 0x72,
                ],
                BaseComputeOutput {
                    probe: Some(MediaProbe {
                        media_kind: dedup_core::MediaKind::Other,
                        width: 0,
                        height: 0,
                        duration_ms: None,
                    }),
                    stage1_frames: Some(Vec::new()),
                    contact_sheet_jpeg: None,
                },
            )
            .await;

        tokio::time::timeout(Duration::from_secs(2), async {
            loop {
                let event = events.recv().await.expect("运行任务事件通道不应关闭");
                if event.runtime_task_id == accepted.task_id && event.state == "completed" {
                    break;
                }
            }
        })
        .await
        .expect("扫描成功必须发布 completed 终态");

        let snapshot = handle
            .latest_completed_scan_for_test()
            .await
            .expect("completed 事件可见时 actor 必须已经保存扫描快照");
        assert_eq!(snapshot.task_id.as_uuid().to_string(), accepted.task_id);
        assert_eq!(snapshot.roots.len(), 1);

        let query = handle
            .handle(proto::Envelope {
                request_id: 63,
                payload: Some(proto::envelope::Payload::QueryTask(proto::QueryTask {
                    task_id: accepted.task_id.clone(),
                    task: None,
                })),
            })
            .await;
        assert!(matches!(
            query.payload,
            Some(proto::envelope::Payload::QueryTask(proto::QueryTask {
                task: Some(proto::TaskSummary {
                    state,
                    outbox_high_seq,
                    ..
                }),
                ..
            })) if state == proto::TaskState::TaskCompleted as i32
                && outbox_high_seq == snapshot.outbox_high_seq
        ));

        let analysis_input = handle
            .handle(proto::Envelope {
                request_id: 64,
                payload: Some(proto::envelope::Payload::PrepareAnalysisInput(
                    proto::PrepareAnalysisInput {
                        analysis_run_id: AnalysisRunId::new().as_uuid().to_string(),
                        cursor: String::new(),
                        limit: 100,
                        inputs: Vec::new(),
                        next_cursor: String::new(),
                        scan_task_ids: vec![accepted.task_id.clone()],
                    },
                )),
            })
            .await;
        let Some(proto::envelope::Payload::PrepareAnalysisInput(analysis_input)) =
            analysis_input.payload
        else {
            panic!("最新完成扫描必须可用于分析输入");
        };
        assert_eq!(analysis_input.inputs.len(), 1);

        let stale_query = handle
            .handle(proto::Envelope {
                request_id: 65,
                payload: Some(proto::envelope::Payload::QueryTask(proto::QueryTask {
                    task_id: TaskId::new().as_uuid().to_string(),
                    task: None,
                })),
            })
            .await;
        assert!(matches!(
            stale_query.payload,
            Some(proto::envelope::Payload::Error(proto::Error { code, .. }))
                if code == proto::ErrorCode::NotFound as i32
        ));

        handle.shutdown().await.unwrap();
        actor.await.unwrap();
    }

    /// 当前进程最近扫描可以直接创建本地分析，并且分析不新增旧 SQLite 运行态行。
    #[cfg(feature = "test-hooks")]
    #[tokio::test]
    async fn local_analysis_uses_latest_scan_and_publishes_result_without_legacy_rows() {
        let directory = tempfile::tempdir().unwrap();
        let scan_root = directory.path().join("scan");
        fs::create_dir(&scan_root).unwrap();
        fs::write(scan_root.join("latest.bin"), b"latest scan input").unwrap();
        let database = directory.path().join("node.db");
        let cache_root = directory.path().join("cache");
        let runtime_root = directory.path().join("data/node/runtime");
        let results_root = directory.path().join("data/node/results");
        let machine = MachineId::parse(&"d1".repeat(32)).unwrap();
        let store = NodeStore::open(&database, machine).unwrap();
        let (pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
        let (handle, actor) = NodeEngine::spawn_with_runtime_root_for_test(
            store,
            pool,
            "127.0.0.1:39091".parse().unwrap(),
            &cache_root,
            &runtime_root,
            EnumeratorKind::WindowsWalker,
        );
        let mut events = handle.runtime_tasks_for_test().subscribe();

        let scan = handle
            .handle(proto::Envelope {
                request_id: 100,
                payload: Some(proto::envelope::Payload::CreateScan(proto::CreateScan {
                    roots: vec![scan_root.to_string_lossy().into_owned()],
                    force_recalculate: false,
                    enumerator: "windows_walker".into(),
                })),
            })
            .await;
        let Some(proto::envelope::Payload::TaskAccepted(scan)) = scan.payload else {
            panic!("扫描必须返回当前进程任务 ID");
        };
        let (_, item_id) = tokio::time::timeout(Duration::from_secs(2), started.recv())
            .await
            .expect("扫描必须进入受控 Worker")
            .expect("受控 Worker 不应提前关闭");
        controller
            .base_source_read_complete(scan.task_id.clone(), item_id.clone())
            .await;
        controller
            .complete_base(
                scan.task_id.clone(),
                item_id,
                [
                    0x37, 0x5a, 0x25, 0x89, 0x24, 0x28, 0xfc, 0x70, 0x64, 0x81, 0x68, 0xeb, 0xed,
                    0x07, 0xcd, 0x0c,
                ],
                BaseComputeOutput {
                    probe: Some(MediaProbe {
                        media_kind: dedup_core::MediaKind::Other,
                        width: 0,
                        height: 0,
                        duration_ms: None,
                    }),
                    stage1_frames: Some(Vec::new()),
                    contact_sheet_jpeg: None,
                },
            )
            .await;
        tokio::time::timeout(Duration::from_secs(2), async {
            loop {
                if let Ok(event) = events.recv().await
                    && event.runtime_task_id == scan.task_id
                    && event.state == "completed"
                {
                    break;
                }
            }
        })
        .await
        .expect("扫描完成后才能创建本地分析");
        let baseline_connection = Connection::open(&database).unwrap();
        let baseline_counts = [
            "tasks",
            "task_items",
            "task_stages",
            "analysis_runs",
            "analysis_run_stages",
            "analysis_run_inputs",
            "candidate_pairs",
            "duplicate_groups",
            "group_members",
            "review_marks",
        ]
        .into_iter()
        .map(|table| {
            let count: i64 = baseline_connection
                .query_row(&format!("SELECT COUNT(*) FROM {table}"), [], |row| {
                    row.get(0)
                })
                .unwrap();
            (table, count)
        })
        .collect::<Vec<_>>();
        drop(baseline_connection);

        let analysis = handle
            .handle(proto::Envelope {
                request_id: 101,
                payload: Some(proto::envelope::Payload::CreateLocalAnalysis(
                    proto::CreateLocalAnalysis {
                        scan_task_ids: vec![scan.task_id.clone()],
                        group_kind: proto::GroupKind::GroupExact as i32,
                        thresholds: None,
                    },
                )),
            })
            .await;
        let Some(proto::envelope::Payload::QueryAnalysisRun(accepted)) = analysis.payload else {
            panic!("当前扫描快照必须接受本地分析");
        };
        assert_eq!(accepted.state, "collecting_stage1");

        tokio::time::timeout(Duration::from_secs(2), async {
            loop {
                let details = handle
                    .runtime_tasks_for_test()
                    .details(&accepted.analysis_run_id)
                    .await;
                if details
                    .and_then(|details| details.summary)
                    .is_some_and(|summary| summary.state != "running")
                {
                    break;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("本地分析必须进入终态");

        let query = handle
            .handle(proto::Envelope {
                request_id: 102,
                payload: Some(proto::envelope::Payload::QueryAnalysisRun(
                    proto::QueryAnalysisRun {
                        analysis_run_id: accepted.analysis_run_id.clone(),
                        state: String::new(),
                        input_count: 0,
                        candidate_count: 0,
                        error_text: String::new(),
                    },
                )),
            })
            .await;
        let Some(proto::envelope::Payload::QueryAnalysisRun(query)) = query.payload else {
            panic!("当前本地分析必须可查询");
        };
        assert_eq!(query.state, "completed");
        assert_eq!(query.input_count, 1);
        assert!(results_root.join("latest-analysis.result.tsv").exists());

        drop(handle);
        actor.await.unwrap();
        let connection = Connection::open(&database).unwrap();
        for (table, baseline_count) in baseline_counts {
            let count: i64 = connection
                .query_row(&format!("SELECT COUNT(*) FROM {table}"), [], |row| {
                    row.get(0)
                })
                .unwrap();
            assert_eq!(count, baseline_count, "本地分析不应新增 {table} 行");
        }
    }

    /// 本地分析必须先安装内存结果，再向 Runtime 发布 completed 终态。
    #[cfg(feature = "test-hooks")]
    #[tokio::test]
    async fn local_analysis_installs_latest_result_before_runtime_completed() {
        let directory = tempfile::tempdir().unwrap();
        let scan_root = directory.path().join("scan");
        fs::create_dir(&scan_root).unwrap();
        fs::write(scan_root.join("latest.bin"), b"latest scan input").unwrap();
        let cache_root = directory.path().join("cache");
        let runtime_root = directory.path().join("data/node/runtime");
        let results_root = directory.path().join("data/node/results");
        let machine = MachineId::parse(&"d1".repeat(32)).unwrap();
        let store = NodeStore::open(&directory.path().join("node.db"), machine).unwrap();
        let (publish_control, publish_waiter) = AnalysisPublishTestController::new();
        let (pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
        let (handle, actor) = NodeEngine::spawn_with_analysis_publish_gate_for_test(
            store,
            pool,
            "127.0.0.1:39091".parse().unwrap(),
            &cache_root,
            &runtime_root,
            EnumeratorKind::WindowsWalker,
            publish_waiter,
        );
        let registry = handle.runtime_tasks_for_test();

        let scan = handle
            .handle(proto::Envelope {
                request_id: 103,
                payload: Some(proto::envelope::Payload::CreateScan(proto::CreateScan {
                    roots: vec![scan_root.to_string_lossy().into_owned()],
                    force_recalculate: false,
                    enumerator: "windows_walker".into(),
                })),
            })
            .await;
        let Some(proto::envelope::Payload::TaskAccepted(scan)) = scan.payload else {
            panic!("扫描必须返回当前进程任务 ID");
        };
        let (_, item_id) = tokio::time::timeout(Duration::from_secs(2), started.recv())
            .await
            .expect("扫描必须进入受控 Worker")
            .expect("受控 Worker 不应提前关闭");
        controller
            .base_source_read_complete(scan.task_id.clone(), item_id.clone())
            .await;
        controller
            .complete_base(
                scan.task_id.clone(),
                item_id,
                [
                    0x37, 0x5a, 0x25, 0x89, 0x24, 0x28, 0xfc, 0x70, 0x64, 0x81, 0x68, 0xeb, 0xed,
                    0x07, 0xcd, 0x0c,
                ],
                BaseComputeOutput {
                    probe: Some(MediaProbe {
                        media_kind: dedup_core::MediaKind::Other,
                        width: 0,
                        height: 0,
                        duration_ms: None,
                    }),
                    stage1_frames: Some(Vec::new()),
                    contact_sheet_jpeg: None,
                },
            )
            .await;
        tokio::time::timeout(Duration::from_secs(2), async {
            loop {
                let Some(details) = registry.details(&scan.task_id).await else {
                    tokio::task::yield_now().await;
                    continue;
                };
                if details
                    .summary
                    .is_some_and(|summary| summary.state == "completed")
                {
                    break;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("扫描完成后才能创建本地分析");

        let mut analysis_events = registry.subscribe();
        let analysis = handle
            .handle(proto::Envelope {
                request_id: 104,
                payload: Some(proto::envelope::Payload::CreateLocalAnalysis(
                    proto::CreateLocalAnalysis {
                        scan_task_ids: vec![scan.task_id.clone()],
                        group_kind: proto::GroupKind::GroupExact as i32,
                        thresholds: None,
                    },
                )),
            })
            .await;
        let Some(proto::envelope::Payload::QueryAnalysisRun(accepted)) = analysis.payload else {
            panic!("当前扫描快照必须接受本地分析");
        };
        tokio::time::timeout(
            Duration::from_secs(2),
            publish_control.wait_until_installed(),
        )
        .await
        .expect("本地分析必须在 Runtime completed 前安装结果");

        let details = registry
            .details(&accepted.analysis_run_id)
            .await
            .expect("安装结果前必须已经存在运行摘要");
        assert_eq!(details.summary.expect("运行摘要必须存在").state, "running");
        while let Ok(event) = analysis_events.try_recv() {
            assert!(
                !(event.runtime_task_id == accepted.analysis_run_id && event.state == "completed")
            );
        }

        publish_control.release();
        tokio::time::timeout(Duration::from_secs(2), async {
            loop {
                let Some(details) = registry.details(&accepted.analysis_run_id).await else {
                    tokio::task::yield_now().await;
                    continue;
                };
                if details
                    .summary
                    .is_some_and(|summary| summary.state == "completed")
                {
                    break;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("放行后本地分析必须发布 completed");

        let query = handle
            .handle(proto::Envelope {
                request_id: 105,
                payload: Some(proto::envelope::Payload::QueryAnalysisRun(
                    proto::QueryAnalysisRun {
                        analysis_run_id: accepted.analysis_run_id.clone(),
                        state: String::new(),
                        input_count: 0,
                        candidate_count: 0,
                        error_text: String::new(),
                    },
                )),
            })
            .await;
        let Some(proto::envelope::Payload::QueryAnalysisRun(query)) = query.payload else {
            panic!("当前本地分析必须可查询");
        };
        assert_eq!(query.state, "completed");
        assert_eq!(query.input_count, 1);
        assert!(results_root.join("latest-analysis.result.tsv").exists());

        handle.shutdown().await.unwrap();
        actor.await.unwrap();
    }

    /// 缺失二筛必须经瞬态 task-file Worker 完成后再发布最近一次分析 TSV。
    #[cfg(feature = "test-hooks")]
    #[tokio::test]
    async fn local_analysis_runs_missing_stage2_and_publishes_after_worker_ack() {
        let directory = tempfile::tempdir().unwrap();
        let scan_root = directory.path().join("scan");
        fs::create_dir(&scan_root).unwrap();
        fs::write(scan_root.join("left.jpg"), b"left image").unwrap();
        fs::write(scan_root.join("right.jpg"), b"right image").unwrap();
        let database = directory.path().join("node.db");
        let cache_root = directory.path().join("cache");
        let runtime_root = directory.path().join("data/node/runtime");
        let results_root = directory.path().join("data/node/results");
        let machine = MachineId::parse(&"d2".repeat(32)).unwrap();
        let store = NodeStore::open(&database, machine).unwrap();
        let (pool, mut started, controller) = WorkerPool::controlled_batch_for_test(2);
        let (handle, actor) = NodeEngine::spawn_with_runtime_root_for_test(
            store,
            pool,
            "127.0.0.1:39091".parse().unwrap(),
            &cache_root,
            &runtime_root,
            EnumeratorKind::WindowsWalker,
        );
        let registry = handle.runtime_tasks_for_test();

        let scan = handle
            .handle(proto::Envelope {
                request_id: 106,
                payload: Some(proto::envelope::Payload::CreateScan(proto::CreateScan {
                    roots: vec![scan_root.to_string_lossy().into_owned()],
                    force_recalculate: false,
                    enumerator: "windows_walker".into(),
                })),
            })
            .await;
        let Some(proto::envelope::Payload::TaskAccepted(scan)) = scan.payload else {
            panic!("二筛夹具的基础扫描必须返回任务 ID");
        };
        for md5 in [
            [
                0xef, 0x0a, 0x61, 0x89, 0x37, 0xda, 0x9f, 0x0e, 0x44, 0x75, 0x2e, 0x30, 0x85, 0xaa,
                0x02, 0xb3,
            ],
            [
                0x3b, 0xad, 0xd8, 0xe0, 0x3f, 0xc2, 0xf0, 0x99, 0x97, 0xcd, 0x06, 0xd7, 0x4a, 0x8b,
                0xe3, 0x3b,
            ],
        ] {
            let (_, item_id) = tokio::time::timeout(Duration::from_secs(2), started.recv())
                .await
                .expect("基础扫描必须进入受控 Worker")
                .expect("受控 Worker 不应提前关闭");
            controller
                .base_source_read_complete(scan.task_id.clone(), item_id.clone())
                .await;
            controller
                .complete_base(scan.task_id.clone(), item_id, md5, image_base_output())
                .await;
        }
        tokio::time::timeout(Duration::from_secs(2), async {
            loop {
                if registry
                    .details(&scan.task_id)
                    .await
                    .and_then(|details| details.summary)
                    .is_some_and(|summary| summary.state == "completed")
                {
                    break;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("基础扫描完成后才能创建瞬态本地分析");
        let scan_details = registry
            .details(&scan.task_id)
            .await
            .expect("基础扫描运行详情必须存在");
        assert_eq!(
            scan_details
                .summary
                .as_ref()
                .map(|summary| summary.overall_completed),
            Some(2),
            "两张图片必须都完成基础计算: {scan_details:?}"
        );
        let snapshot = handle
            .latest_completed_scan_for_test()
            .await
            .expect("基础扫描完成后必须保存清单快照");
        assert_eq!(snapshot.resolved_files.len(), 2);

        let baseline_connection = Connection::open(&database).unwrap();
        let baseline_counts = legacy_analysis_table_counts(&baseline_connection);
        let analysis = handle
            .handle(proto::Envelope {
                request_id: 107,
                payload: Some(proto::envelope::Payload::CreateLocalAnalysis(
                    proto::CreateLocalAnalysis {
                        scan_task_ids: vec![scan.task_id.clone()],
                        group_kind: proto::GroupKind::GroupSimilarImage as i32,
                        thresholds: None,
                    },
                )),
            })
            .await;
        let Some(proto::envelope::Payload::QueryAnalysisRun(accepted)) = analysis.payload else {
            panic!("缺失二筛的本地分析必须返回运行 ID");
        };
        assert_eq!(accepted.input_count, 2);
        assert_eq!(accepted.candidate_count, 1);
        let mut stage2_task_id = None;
        for seed in [9, 10] {
            let (task_id, item_id) = tokio::time::timeout(Duration::from_secs(2), started.recv())
                .await
                .expect("缺失二筛必须为每个缺失内容进入瞬态 Worker")
                .expect("受控 Worker 不应提前关闭");
            assert_ne!(
                task_id, accepted.analysis_run_id,
                "二筛 Worker 必须使用内部瞬态任务身份，不能复用分析 ID"
            );
            if let Some(expected_task_id) = stage2_task_id.as_ref() {
                assert_eq!(
                    &task_id, expected_task_id,
                    "同一批二筛缺失内容必须共享一个内部 task_id"
                );
            } else {
                stage2_task_id = Some(task_id.clone());
            }
            controller
                .stage2_source_read_complete(task_id.clone(), item_id.clone())
                .await;
            controller
                .complete_stage2(task_id, item_id, stage2_success_output(seed))
                .await;
        }

        tokio::time::timeout(Duration::from_secs(2), async {
            loop {
                if registry
                    .details(&accepted.analysis_run_id)
                    .await
                    .and_then(|details| details.summary)
                    .is_some_and(|summary| summary.state == "completed")
                {
                    break;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("二筛 ACK 后本地分析必须发布 completed");

        let query = handle
            .handle(proto::Envelope {
                request_id: 108,
                payload: Some(proto::envelope::Payload::QueryAnalysisRun(
                    proto::QueryAnalysisRun {
                        analysis_run_id: accepted.analysis_run_id,
                        state: String::new(),
                        input_count: 0,
                        candidate_count: 0,
                        error_text: String::new(),
                    },
                )),
            })
            .await;
        let Some(proto::envelope::Payload::QueryAnalysisRun(query)) = query.payload else {
            panic!("完成的瞬态本地分析必须可查询");
        };
        assert_eq!(query.state, "completed");
        assert_eq!(query.input_count, 2);
        assert_eq!(query.candidate_count, 1);
        assert!(results_root.join("latest-analysis.result.tsv").is_file());

        handle.shutdown().await.unwrap();
        actor.await.unwrap();
        let connection = Connection::open(&database).unwrap();
        assert_eq!(legacy_analysis_table_counts(&connection), baseline_counts);
    }

    /// 取消缺失二筛只能结束当前瞬态运行，并保留上一份结果及旧历史表行。
    #[cfg(feature = "test-hooks")]
    #[tokio::test]
    async fn cancelled_local_analysis_preserves_previous_result_and_legacy_rows() {
        let directory = tempfile::tempdir().unwrap();
        let scan_root = directory.path().join("scan");
        fs::create_dir(&scan_root).unwrap();
        fs::write(scan_root.join("left.jpg"), b"left image").unwrap();
        fs::write(scan_root.join("right.jpg"), b"right image").unwrap();
        let database = directory.path().join("node.db");
        let cache_root = directory.path().join("cache");
        let runtime_root = directory.path().join("data/node/runtime");
        let results_root = directory.path().join("data/node/results");
        fs::create_dir_all(&results_root).unwrap();
        let result_path = results_root.join("latest-analysis.result.tsv");
        let previous_result = b"previous analysis result\n";
        fs::write(&result_path, previous_result).unwrap();
        let machine = MachineId::parse(&"d3".repeat(32)).unwrap();
        let store = NodeStore::open(&database, machine).unwrap();
        let (before_publish_control, before_publish_waiter) =
            AnalysisBeforePublishTestController::new();
        let (pool, mut started, controller) = WorkerPool::controlled_batch_for_test(2);
        let (handle, actor) = NodeEngine::spawn_with_analysis_before_publish_gate_for_test(
            store,
            pool,
            "127.0.0.1:39091".parse().unwrap(),
            &cache_root,
            &runtime_root,
            EnumeratorKind::WindowsWalker,
            before_publish_waiter,
        );
        let registry = handle.runtime_tasks_for_test();

        let scan = handle
            .handle(proto::Envelope {
                request_id: 109,
                payload: Some(proto::envelope::Payload::CreateScan(proto::CreateScan {
                    roots: vec![scan_root.to_string_lossy().into_owned()],
                    force_recalculate: false,
                    enumerator: "windows_walker".into(),
                })),
            })
            .await;
        let Some(proto::envelope::Payload::TaskAccepted(scan)) = scan.payload else {
            panic!("取消夹具的基础扫描必须返回任务 ID");
        };
        for md5 in [
            [
                0xef, 0x0a, 0x61, 0x89, 0x37, 0xda, 0x9f, 0x0e, 0x44, 0x75, 0x2e, 0x30, 0x85, 0xaa,
                0x02, 0xb3,
            ],
            [
                0x3b, 0xad, 0xd8, 0xe0, 0x3f, 0xc2, 0xf0, 0x99, 0x97, 0xcd, 0x06, 0xd7, 0x4a, 0x8b,
                0xe3, 0x3b,
            ],
        ] {
            let (_, item_id) = tokio::time::timeout(Duration::from_secs(2), started.recv())
                .await
                .expect("基础扫描必须进入受控 Worker")
                .expect("受控 Worker 不应提前关闭");
            controller
                .base_source_read_complete(scan.task_id.clone(), item_id.clone())
                .await;
            controller
                .complete_base(scan.task_id.clone(), item_id, md5, image_base_output())
                .await;
        }
        tokio::time::timeout(Duration::from_secs(2), async {
            loop {
                if registry
                    .details(&scan.task_id)
                    .await
                    .and_then(|details| details.summary)
                    .is_some_and(|summary| summary.state == "completed")
                {
                    break;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("基础扫描完成后才能创建取消分析");

        let baseline_connection = Connection::open(&database).unwrap();
        let baseline_counts = legacy_analysis_table_counts(&baseline_connection);
        let analysis = handle
            .handle(proto::Envelope {
                request_id: 110,
                payload: Some(proto::envelope::Payload::CreateLocalAnalysis(
                    proto::CreateLocalAnalysis {
                        scan_task_ids: vec![scan.task_id],
                        group_kind: proto::GroupKind::GroupSimilarImage as i32,
                        thresholds: None,
                    },
                )),
            })
            .await;
        let Some(proto::envelope::Payload::QueryAnalysisRun(accepted)) = analysis.payload else {
            panic!("取消分析必须返回运行 ID");
        };
        let mut stage2_task_id = None;
        for seed in [9, 10] {
            let (task_id, item_id) = tokio::time::timeout(Duration::from_secs(2), started.recv())
                .await
                .expect("取消分析必须先进入瞬态 Worker")
                .expect("受控 Worker 不应提前关闭");
            if let Some(expected_task_id) = stage2_task_id.as_ref() {
                assert_eq!(
                    &task_id, expected_task_id,
                    "同一批二筛缺失内容必须共享一个内部 task_id"
                );
            } else {
                stage2_task_id = Some(task_id.clone());
            }
            controller
                .stage2_source_read_complete(task_id.clone(), item_id.clone())
                .await;
            controller
                .complete_stage2(task_id, item_id, stage2_success_output(seed))
                .await;
        }
        let stage2_task_id = stage2_task_id.expect("二筛必须至少启动一个 Worker");
        tokio::time::timeout(
            Duration::from_secs(2),
            before_publish_control.wait_until_entered(),
        )
        .await
        .expect("二筛 ACK 后必须停在最终评估前");

        let cancel = handle
            .handle(proto::Envelope {
                request_id: 111,
                payload: Some(proto::envelope::Payload::CancelTask(proto::CancelTask {
                    task_id: accepted.analysis_run_id.clone(),
                })),
            })
            .await;
        assert!(matches!(
            cancel.payload,
            Some(proto::envelope::Payload::CancelTask(_))
        ));
        before_publish_control.release();
        tokio::time::timeout(Duration::from_secs(2), async {
            loop {
                if registry
                    .details(&accepted.analysis_run_id)
                    .await
                    .and_then(|details| details.summary)
                    .is_some_and(|summary| summary.state == "cancelled")
                {
                    break;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("取消必须等待瞬态二筛 Worker 和任务目录收束");
        assert_eq!(fs::read(&result_path).unwrap(), previous_result);
        assert!(!runtime_root.join(stage2_task_id).exists());

        let query = handle
            .handle(proto::Envelope {
                request_id: 112,
                payload: Some(proto::envelope::Payload::QueryAnalysisRun(
                    proto::QueryAnalysisRun {
                        analysis_run_id: accepted.analysis_run_id,
                        state: String::new(),
                        input_count: 0,
                        candidate_count: 0,
                        error_text: String::new(),
                    },
                )),
            })
            .await;
        let Some(proto::envelope::Payload::QueryAnalysisRun(query)) = query.payload else {
            panic!("取消后的瞬态分析必须可查询");
        };
        assert_eq!(query.state, "cancelled");

        handle.shutdown().await.unwrap();
        actor.await.unwrap();
        let connection = Connection::open(&database).unwrap();
        assert_eq!(legacy_analysis_table_counts(&connection), baseline_counts);
    }

    #[cfg(feature = "test-hooks")]
    #[tokio::test]
    async fn restart_installs_successful_scan_snapshot_before_background_finished_command() {
        let directory = tempfile::tempdir().unwrap();
        let scan_root = directory.path().join("scan");
        fs::create_dir(&scan_root).unwrap();
        fs::write(scan_root.join("restart-interleave.bin"), b"abc").unwrap();
        let machine = MachineId::parse(&"c8".repeat(32)).unwrap();
        let store = NodeStore::open(&directory.path().join("node.db"), machine).unwrap();
        let (outcome_control, outcome_waiter) = BackgroundOutcomeTestController::new();
        let (pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
        let (handle, actor) = NodeEngine::spawn_with_background_outcome_gate_for_test(
            store,
            pool,
            "127.0.0.1:39091".parse().unwrap(),
            directory.path(),
            EnumeratorKind::WindowsWalker,
            outcome_waiter,
        );
        let registry = handle.runtime_tasks_for_test();

        let response = handle
            .handle(proto::Envelope {
                request_id: 66,
                payload: Some(proto::envelope::Payload::CreateScan(proto::CreateScan {
                    roots: vec![scan_root.to_string_lossy().into_owned()],
                    force_recalculate: false,
                    enumerator: "windows_walker".into(),
                })),
            })
            .await;
        let Some(proto::envelope::Payload::TaskAccepted(accepted)) = response.payload else {
            panic!("瞬态扫描必须返回业务任务 ID");
        };
        let (_, item_id) = tokio::time::timeout(Duration::from_secs(2), started.recv())
            .await
            .expect("扫描必须进入真实 Worker")
            .expect("可控 Worker 不应提前关闭");
        controller
            .base_source_read_complete(accepted.task_id.clone(), item_id.clone())
            .await;
        controller
            .complete_base(
                accepted.task_id.clone(),
                item_id,
                [
                    0x90, 0x01, 0x50, 0x98, 0x3c, 0xd2, 0x4f, 0xb0, 0xd6, 0x96, 0x3f, 0x7d, 0x28,
                    0xe1, 0x7f, 0x72,
                ],
                BaseComputeOutput {
                    probe: Some(MediaProbe {
                        media_kind: dedup_core::MediaKind::Other,
                        width: 0,
                        height: 0,
                        duration_ms: None,
                    }),
                    stage1_frames: Some(Vec::new()),
                    contact_sheet_jpeg: None,
                },
            )
            .await;
        tokio::time::timeout(Duration::from_secs(2), outcome_control.wait_until_entered())
            .await
            .expect("扫描成功后必须先形成可消费的后台 outcome");

        let restart = tokio::time::timeout(Duration::from_secs(2), handle.restart_engine())
            .await
            .expect("restart 必须在 outcome 已收束时返回");
        assert!(matches!(restart, Err(EngineError::Operation(_))));

        let details = registry.details(&accepted.task_id).await.unwrap();
        let summary = details.summary.expect("restart 必须发布扫描终态");
        assert_eq!(summary.state, "completed");
        let runtime_highwater = summary
            .outbox_high_seq
            .expect("成功扫描终态必须携带真实 highwater");

        let snapshot = handle.latest_completed_scan_for_test().await;
        let query = handle
            .handle(proto::Envelope {
                request_id: 67,
                payload: Some(proto::envelope::Payload::QueryTask(proto::QueryTask {
                    task_id: accepted.task_id.clone(),
                    task: None,
                })),
            })
            .await;
        let query_highwater = match query.payload {
            Some(proto::envelope::Payload::QueryTask(proto::QueryTask {
                task:
                    Some(proto::TaskSummary {
                        state,
                        outbox_high_seq,
                        ..
                    }),
                ..
            })) if state == proto::TaskState::TaskCompleted as i32 => outbox_high_seq,
            other => panic!("restart 后 QueryTask 必须返回 Completed: {other:?}"),
        };

        outcome_control.release();
        handle.shutdown().await.unwrap();
        actor.await.unwrap();

        let snapshot = snapshot.expect("restart 消费 outcome 时必须先保存扫描快照");
        assert_eq!(snapshot.task_id.as_uuid().to_string(), accepted.task_id);
        assert_eq!(snapshot.outbox_high_seq, runtime_highwater);
        assert_eq!(query_highwater, runtime_highwater);
    }

    #[tokio::test]
    async fn cancel_task_publishes_terminal_only_after_all_pipeline_ownership_is_released() {
        let directory = tempfile::tempdir().unwrap();
        let scan_root = directory.path().join("scan");
        fs::create_dir(&scan_root).unwrap();
        fs::write(scan_root.join("held.mp4"), b"held worker input").unwrap();
        let machine = MachineId::parse(&"c2".repeat(32)).unwrap();
        let store = NodeStore::open(&directory.path().join("node.db"), machine).unwrap();
        let observer = store.reopen().unwrap();
        let (pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
        let (handle, actor) = NodeEngine::spawn(
            store,
            pool,
            "127.0.0.1:39091".parse().unwrap(),
            directory.path(),
            EnumeratorKind::WindowsWalker,
        );
        let registry = handle.runtime_tasks_for_test();
        let mut events = registry.subscribe();

        let accepted = handle
            .handle(proto::Envelope {
                request_id: 10,
                payload: Some(proto::envelope::Payload::CreateScan(proto::CreateScan {
                    roots: vec![scan_root.to_string_lossy().into_owned()],
                    force_recalculate: false,
                    enumerator: "windows_walker".into(),
                })),
            })
            .await;
        let Some(proto::envelope::Payload::TaskAccepted(accepted)) = accepted.payload else {
            panic!("CreateScan 必须返回任务身份");
        };
        let (_, item_id) = tokio::time::timeout(Duration::from_secs(2), started.recv())
            .await
            .expect("扫描必须进入持有媒体许可的 Worker")
            .expect("可控 Worker 不应提前关闭");
        controller
            .phase_changed(
                accepted.task_id.clone(),
                item_id,
                proto::RuntimeWorkerPhase::RuntimeWorkerDecode,
            )
            .await;

        let runtime_id = tokio::time::timeout(Duration::from_secs(2), async {
            loop {
                let tasks = registry.list().await;
                let Some(task) = tasks.first() else {
                    tokio::task::yield_now().await;
                    continue;
                };
                let details = registry.details(&task.runtime_task_id).await.unwrap();
                let worker_decode = details.workers.first().is_some_and(|worker| {
                    worker.phase == Some(proto::RuntimeWorkerPhase::RuntimeWorkerDecode as i32)
                });
                let media_held = details
                    .pipeline_metrics
                    .as_ref()
                    .and_then(|metrics| metrics.media_io.as_ref())
                    .and_then(|resource| resource.current)
                    == Some(1);
                if worker_decode && media_held {
                    break task.runtime_task_id.clone();
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("取消前必须观察到 DECODE 与媒体许可 ownership");

        let cancel = handle
            .handle(proto::Envelope {
                request_id: 11,
                payload: Some(proto::envelope::Payload::CancelTask(proto::CancelTask {
                    task_id: accepted.task_id,
                })),
            })
            .await;
        assert!(matches!(
            cancel.payload,
            Some(proto::envelope::Payload::CancelTask(_))
        ));

        let details = tokio::time::timeout(Duration::from_secs(2), async {
            loop {
                let details = registry.details(&runtime_id).await.unwrap();
                if details.summary.as_ref().unwrap().state == "cancelled" {
                    break details;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("后台清理后必须发布取消终态");
        let worker = details
            .workers
            .first()
            .expect("Started 必须建立 Worker 投影");
        assert_eq!(
            worker.phase,
            Some(proto::RuntimeWorkerPhase::RuntimeWorkerIdle as i32)
        );
        assert!(worker.display_path.is_empty());
        assert_eq!(worker.current_step, "空闲");
        let metrics = details.pipeline_metrics.expect("基础计算必须有运行指标");
        for queue in [
            metrics.hash_queue,
            metrics.path_cache_queue,
            metrics.content_cache_queue,
            metrics.decode_queue,
            metrics.persist_queue,
        ] {
            let queue = queue.expect("五段队列必须完整");
            assert_eq!(queue.current, Some(0));
            assert!(queue.peak.unwrap_or_default() <= queue.capacity.unwrap());
        }
        for resource in [
            metrics.hash_io,
            metrics.media_io,
            metrics.cpu_weight,
            metrics.worker_slots,
        ] {
            let resource = resource.expect("四类资源必须完整");
            assert_eq!(resource.current, Some(0));
            assert!(resource.peak.unwrap_or_default() > 0, "取消不得抹掉峰值");
        }
        let mut terminal_events = 0;
        while let Ok(event) = events.try_recv() {
            terminal_events +=
                usize::from(event.runtime_task_id == runtime_id && event.state == "cancelled");
        }
        assert_eq!(terminal_events, 1, "同一任务只能广播一次取消终态");

        handle.shutdown().await.unwrap();
        actor.await.unwrap();
        assert!(observer.page_tasks(None, 100).unwrap().items.is_empty());
    }

    #[tokio::test]
    async fn shutdown_releases_real_worker_before_publishing_cancelled_terminal() {
        let directory = tempfile::tempdir().unwrap();
        let scan_root = directory.path().join("scan");
        fs::create_dir(&scan_root).unwrap();
        fs::write(scan_root.join("shutdown.mp4"), b"shutdown worker input").unwrap();
        let machine = MachineId::parse(&"c4".repeat(32)).unwrap();
        let store = NodeStore::open(&directory.path().join("node.db"), machine).unwrap();
        let observer = store.reopen().unwrap();
        let (pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
        let (handle, actor) = NodeEngine::spawn(
            store,
            pool,
            "127.0.0.1:39091".parse().unwrap(),
            directory.path(),
            EnumeratorKind::WindowsWalker,
        );
        let registry = handle.runtime_tasks_for_test();
        let accepted = handle
            .handle(proto::Envelope {
                request_id: 30,
                payload: Some(proto::envelope::Payload::CreateScan(proto::CreateScan {
                    roots: vec![scan_root.to_string_lossy().into_owned()],
                    force_recalculate: false,
                    enumerator: "windows_walker".into(),
                })),
            })
            .await;
        let Some(proto::envelope::Payload::TaskAccepted(accepted)) = accepted.payload else {
            panic!("CreateScan 必须返回任务身份");
        };
        let (_, item_id) = started.recv().await.expect("关机前 Worker 必须已启动");
        controller
            .phase_changed(
                accepted.task_id,
                item_id,
                proto::RuntimeWorkerPhase::RuntimeWorkerDecode,
            )
            .await;
        let runtime_id = registry.list().await[0].runtime_task_id.clone();
        tokio::time::timeout(Duration::from_secs(2), async {
            loop {
                let details = registry.details(&runtime_id).await.unwrap();
                if details.workers.first().is_some_and(|worker| {
                    worker.phase == Some(proto::RuntimeWorkerPhase::RuntimeWorkerDecode as i32)
                }) {
                    break;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("关机前必须收到 DECODE 阶段");

        handle.shutdown().await.unwrap();
        actor.await.unwrap();
        assert!(controller.running_files().is_empty());
        assert_eq!(controller.cpu_in_use(), 0);
        assert_eq!(
            controller.available_slots(),
            0,
            "关闭后的可控 Pool 不得保留旧 slot"
        );
        assert!(observer.page_tasks(None, 100).unwrap().items.is_empty());
        let details = registry.details(&runtime_id).await.unwrap();
        assert_eq!(details.summary.unwrap().state, "cancelled");
        assert_eq!(
            details.workers[0].phase,
            Some(proto::RuntimeWorkerPhase::RuntimeWorkerIdle as i32)
        );
        let metrics = details.pipeline_metrics.unwrap();
        for current in [
            metrics.hash_queue.unwrap().current,
            metrics.path_cache_queue.unwrap().current,
            metrics.content_cache_queue.unwrap().current,
            metrics.decode_queue.unwrap().current,
            metrics.persist_queue.unwrap().current,
            metrics.hash_io.unwrap().current,
            metrics.media_io.unwrap().current,
            metrics.cpu_weight.unwrap().current,
            metrics.worker_slots.unwrap().current,
        ] {
            assert_eq!(current, Some(0));
        }
    }

    #[cfg(feature = "test-hooks")]
    #[test]
    fn shutdown_waits_for_task_local_writer_join_before_terminal() {
        let output = SharedLogBuffer::default();
        let subscriber = tracing_subscriber::fmt()
            .without_time()
            .with_ansi(false)
            .with_target(false)
            .with_writer(output.clone())
            .finish();
        tracing::subscriber::set_global_default(subscriber).unwrap();
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap();

        let runtime_id = runtime.block_on(async {
            let directory = tempfile::tempdir().unwrap();
            let scan_root = directory.path().join("scan");
            fs::create_dir(&scan_root).unwrap();
            fs::write(scan_root.join("writer-gate.bin"), b"abc").unwrap();
            let machine = MachineId::parse(&"c5".repeat(32)).unwrap();
            let database = directory.path().join("node.db");
            let store = NodeStore::open(&database, machine.clone()).unwrap();
            let observer = store.reopen().unwrap();
            let (persist_control, persist_waiter) = BasePersistTestController::new();
            let (pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
            let (handle, actor) = NodeEngine::spawn_with_first_persist_gate_for_test(
                store,
                pool,
                "127.0.0.1:39091".parse().unwrap(),
                directory.path(),
                EnumeratorKind::WindowsWalker,
                persist_waiter,
            );
            let registry = handle.runtime_tasks_for_test();
            let mut events = registry.subscribe();

            let accepted = handle
                .handle(proto::Envelope {
                    request_id: 40,
                    payload: Some(proto::envelope::Payload::CreateScan(proto::CreateScan {
                        roots: vec![scan_root.to_string_lossy().into_owned()],
                        force_recalculate: false,
                        enumerator: "windows_walker".into(),
                    })),
                })
                .await;
            let Some(proto::envelope::Payload::TaskAccepted(accepted)) = accepted.payload else {
                panic!("CreateScan 必须返回任务身份");
            };
            let (_, item_id) = tokio::time::timeout(Duration::from_secs(2), started.recv())
                .await
                .expect("扫描必须进入真实 Worker")
                .expect("可控 Worker 不应提前关闭");
            controller
                .phase_changed(
                    accepted.task_id.clone(),
                    item_id.clone(),
                    proto::RuntimeWorkerPhase::RuntimeWorkerDecode,
                )
                .await;
            controller
                .complete_base(
                    accepted.task_id.clone(),
                    item_id,
                    [
                        0x90, 0x01, 0x50, 0x98, 0x3c, 0xd2, 0x4f, 0xb0, 0xd6, 0x96, 0x3f, 0x7d,
                        0x28, 0xe1, 0x7f, 0x72,
                    ],
                    BaseComputeOutput {
                        probe: Some(MediaProbe {
                            media_kind: dedup_core::MediaKind::Other,
                            width: 0,
                            height: 0,
                            duration_ms: None,
                        }),
                        stage1_frames: Some(Vec::new()),
                        contact_sheet_jpeg: None,
                    },
                )
                .await;
            tokio::time::timeout(Duration::from_secs(2), persist_control.wait_until_entered())
                .await
                .expect("真实 BaseStoreActor 必须停在首条 SQLite 事务前");

            let runtime_id = registry.list().await[0].runtime_task_id.clone();
            let before = registry.details(&runtime_id).await.unwrap();
            let before_metrics = before.pipeline_metrics.unwrap();
            let before_queue_peaks = [
                before_metrics.hash_queue.as_ref().unwrap().peak,
                before_metrics.path_cache_queue.as_ref().unwrap().peak,
                before_metrics.content_cache_queue.as_ref().unwrap().peak,
                before_metrics.decode_queue.as_ref().unwrap().peak,
                before_metrics.persist_queue.as_ref().unwrap().peak,
            ];
            let before_resource_peaks = [
                before_metrics.hash_io.as_ref().unwrap().peak,
                before_metrics.media_io.as_ref().unwrap().peak,
                before_metrics.cpu_weight.as_ref().unwrap().peak,
                before_metrics.worker_slots.as_ref().unwrap().peak,
            ];
            assert!(
                before_resource_peaks
                    .iter()
                    .all(|peak| peak.unwrap_or_default() > 0),
                "测试必须先真实占用四类资源"
            );
            assert!(!persist_control.writer_joined());
            assert_eq!(drain_terminal_events(&mut events), 0);
            assert_eq!(terminal_log_count(&output, &runtime_id), 0);

            let mut shutdown = Box::pin(handle.shutdown());
            let first_poll =
                tokio::time::timeout(Duration::from_millis(100), shutdown.as_mut()).await;
            let shutdown_was_pending = first_poll.is_err();
            if let Ok(result) = first_poll {
                result.unwrap();
            }
            let terminal_before_release = drain_terminal_events(&mut events);
            let log_before_release = terminal_log_count(&output, &runtime_id);
            let joined_before_release = persist_control.writer_joined();

            persist_control.release();
            if shutdown_was_pending {
                shutdown.await.unwrap();
            }
            actor.await.unwrap();

            let details = registry.details(&runtime_id).await.unwrap();
            assert_eq!(details.summary.as_ref().unwrap().state, "cancelled");
            let worker = details
                .workers
                .first()
                .expect("Started 必须留下 Worker 投影");
            assert_eq!(
                worker.phase,
                Some(proto::RuntimeWorkerPhase::RuntimeWorkerIdle as i32)
            );
            assert!(worker.display_path.is_empty());
            let metrics = details.pipeline_metrics.unwrap();
            let after_queue_peaks = [
                metrics.hash_queue.as_ref().unwrap().peak,
                metrics.path_cache_queue.as_ref().unwrap().peak,
                metrics.content_cache_queue.as_ref().unwrap().peak,
                metrics.decode_queue.as_ref().unwrap().peak,
                metrics.persist_queue.as_ref().unwrap().peak,
            ];
            let after_resource_peaks = [
                metrics.hash_io.as_ref().unwrap().peak,
                metrics.media_io.as_ref().unwrap().peak,
                metrics.cpu_weight.as_ref().unwrap().peak,
                metrics.worker_slots.as_ref().unwrap().peak,
            ];
            for current in [
                metrics.hash_queue.unwrap().current,
                metrics.path_cache_queue.unwrap().current,
                metrics.content_cache_queue.unwrap().current,
                metrics.decode_queue.unwrap().current,
                metrics.persist_queue.unwrap().current,
                metrics.hash_io.unwrap().current,
                metrics.media_io.unwrap().current,
                metrics.cpu_weight.unwrap().current,
                metrics.worker_slots.unwrap().current,
            ] {
                assert_eq!(current, Some(0));
            }
            let terminal_after_release = drain_terminal_events(&mut events);
            let persisted_tasks = observer.page_tasks(None, 100).unwrap();

            assert!(
                shutdown_was_pending,
                "writer gate 未释放时 shutdown future 必须保持 pending"
            );
            assert_eq!(terminal_before_release, 0, "gate 前不得发布终态事件");
            assert_eq!(log_before_release, 0, "gate 前不得写结构化终态日志");
            assert!(!joined_before_release, "gate 前 writer 不可能完成 join");
            assert!(
                persist_control.writer_joined(),
                "shutdown 返回前必须真实 join writer"
            );
            assert_eq!(terminal_after_release, 1, "释放后只能发布一次终态事件");
            assert_eq!(before_queue_peaks, after_queue_peaks, "队列峰值必须保留");
            assert_eq!(
                before_resource_peaks, after_resource_peaks,
                "资源峰值必须保留"
            );
            assert!(persisted_tasks.items.is_empty());
            assert!(controller.running_files().is_empty());
            assert_eq!(controller.cpu_in_use(), 0);
            assert_eq!(
                controller.available_slots(),
                0,
                "关闭后的可控 Pool 不得保留旧 slot"
            );
            runtime_id
        });

        let log = output.text();
        assert_eq!(terminal_log_count(&output, &runtime_id), 1);
        let terminal_line = log
            .lines()
            .find(|line| line.contains("运行任务进入终态") && line.contains(&runtime_id))
            .expect("必须捕获本任务结构化终态日志");
        assert!(
            terminal_line.contains("state=\"cancelled\""),
            "实际日志：{terminal_line}"
        );
    }

    #[tokio::test]
    async fn scan_background_error_is_runtime_failed_without_persisted_task() {
        let directory = tempfile::tempdir().unwrap();
        let machine = MachineId::parse(&"c3".repeat(32)).unwrap();
        let store = NodeStore::open(&directory.path().join("node.db"), machine).unwrap();
        let observer = store.reopen().unwrap();
        let (pool, _) = WorkerPool::controlled_for_test();
        let (handle, actor) = NodeEngine::spawn(
            store,
            pool,
            "127.0.0.1:39091".parse().unwrap(),
            directory.path(),
            EnumeratorKind::WindowsWalker,
        );
        let response = handle
            .handle(proto::Envelope {
                request_id: 20,
                payload: Some(proto::envelope::Payload::CreateScan(proto::CreateScan {
                    roots: vec![
                        directory
                            .path()
                            .join("missing")
                            .to_string_lossy()
                            .into_owned(),
                    ],
                    force_recalculate: false,
                    enumerator: "windows_walker".into(),
                })),
            })
            .await;
        let Some(proto::envelope::Payload::TaskAccepted(accepted)) = response.payload else {
            panic!("后台枚举失败前必须先返回运行任务 ID");
        };
        let registry = handle.runtime_tasks_for_test();
        tokio::time::timeout(Duration::from_secs(2), async {
            loop {
                let Some(details) = registry.details(&accepted.task_id).await else {
                    tokio::task::yield_now().await;
                    continue;
                };
                if details
                    .summary
                    .as_ref()
                    .is_some_and(|summary| summary.state == "failed")
                {
                    break;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("后台枚举失败必须发布运行任务 failed 终态");

        handle.shutdown().await.unwrap();
        actor.await.unwrap();
        assert!(observer.page_tasks(None, 100).unwrap().items.is_empty());
    }

    #[tokio::test]
    async fn dispatch_stage2_uses_transient_task_files_and_publishes_runtime_highwater() {
        let directory = tempfile::tempdir().unwrap();
        let database = directory.path().join("node.db");
        let cache_root = directory.path().join("cache");
        let runtime_root = directory.path().join("data/node/runtime");
        let machine = MachineId::parse(&"c8".repeat(32)).unwrap();
        let mut store = NodeStore::open(&database, machine).unwrap();
        let media_path = directory.path().join("stage2.jpg");
        fs::write(&media_path, b"stage2 fixture").unwrap();
        let scanned = ScannedPath::new(
            NormalizedPath::new(&media_path).unwrap(),
            DisplayPath::new(&media_path).unwrap(),
            14,
        );
        let content = store
            .upsert_content_and_location(&scanned, [0x81; 16], dedup_core::MediaKind::Image)
            .unwrap();
        store
            .commit_feature_result(
                content.id,
                None,
                FeatureWrite::ImageStage1(ImageStage1Fields::from(ImageStage1 {
                    width: 10,
                    height: 10,
                    pdq: PdqHash::from_bytes([0; 32]),
                    quality: 100,
                })),
            )
            .unwrap();
        store.mark_base_complete(content.id).unwrap();
        let before_highwater = store.outbox_high_seq().unwrap();
        let location = LocationKey::new(store.machine_id().clone(), scanned.normalized_path);
        let observer = store.reopen().unwrap();
        let (pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
        let (handle, actor) = NodeEngine::spawn_with_runtime_root_for_test(
            store,
            pool,
            "127.0.0.1:39091".parse().unwrap(),
            &cache_root,
            &runtime_root,
            EnumeratorKind::WindowsWalker,
        );
        let registry = handle.runtime_tasks_for_test();

        let response = handle
            .handle(proto::Envelope {
                request_id: 70,
                payload: Some(proto::envelope::Payload::DispatchStage2(
                    proto::DispatchStage2 {
                        analysis_run_id: AnalysisRunId::new().as_uuid().to_string(),
                        items: vec![proto::Stage2WorkItem {
                            content: Some((&content.key).into()),
                            source: Some((&location).into()),
                            frame_slots: Vec::new(),
                        }],
                    },
                )),
            })
            .await;
        let Some(proto::envelope::Payload::TaskAccepted(accepted)) = response.payload else {
            panic!("外部二筛必须返回瞬态运行 ID");
        };
        let (worker_task_id, item_id) =
            tokio::time::timeout(Duration::from_secs(2), started.recv())
                .await
                .expect("外部二筛必须进入 task-file Worker")
                .expect("可控 Worker 不应提前关闭");
        assert_eq!(worker_task_id, accepted.task_id);
        let run_directory = runtime_root.join(&accepted.task_id);
        assert!(run_directory.is_dir(), "Worker 持有时必须保留本轮 TSV 目录");
        assert!(
            observer.page_tasks(None, 100).unwrap().items.is_empty(),
            "外部二筛不得写入旧 tasks/task_items/task_stages"
        );

        controller
            .stage2_source_read_complete(accepted.task_id.clone(), item_id.clone())
            .await;
        controller
            .complete_stage2(accepted.task_id.clone(), item_id, stage2_success_output(8))
            .await;
        let details = tokio::time::timeout(Duration::from_secs(2), async {
            loop {
                let details = registry.details(&accepted.task_id).await.unwrap();
                if details
                    .summary
                    .as_ref()
                    .is_some_and(|summary| summary.state == "completed")
                {
                    break details;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("Worker、SQLite 与 TSV 收束后必须发布 Completed");
        let runtime_highwater = details
            .summary
            .and_then(|summary| summary.outbox_high_seq)
            .expect("Completed 必须携带真实 outbox 高水位");
        assert!(runtime_highwater > before_highwater);
        assert!(
            !run_directory.exists(),
            "Completed 前必须精确删除本轮 TSV 目录"
        );
        assert!(observer.page_tasks(None, 100).unwrap().items.is_empty());

        let query = handle
            .handle(proto::Envelope {
                request_id: 71,
                payload: Some(proto::envelope::Payload::QueryTask(proto::QueryTask {
                    task_id: accepted.task_id.clone(),
                    task: None,
                })),
            })
            .await;
        assert!(matches!(
            query.payload,
            Some(proto::envelope::Payload::QueryTask(proto::QueryTask {
                task: Some(proto::TaskSummary { state, outbox_high_seq, .. }),
                ..
            })) if state == proto::TaskState::TaskCompleted as i32
                && outbox_high_seq == runtime_highwater
        ));
        let list = handle
            .handle(proto::Envelope {
                request_id: 72,
                payload: Some(proto::envelope::Payload::ListTasks(proto::ListTasks {
                    cursor: String::new(),
                    limit: 100,
                    tasks: Vec::new(),
                    next_cursor: String::new(),
                })),
            })
            .await;
        assert!(matches!(
            list.payload,
            Some(proto::envelope::Payload::ListTasks(proto::ListTasks { tasks, .. }))
                if tasks.iter().any(|task| task.task_id == accepted.task_id
                    && task.state == proto::TaskState::TaskCompleted as i32
                    && task.outbox_high_seq == runtime_highwater)
        ));

        handle.shutdown().await.unwrap();
        actor.await.unwrap();
    }

    #[tokio::test]
    async fn dispatch_stage2_cache_hit_finishes_without_worker_or_runtime_directory() {
        let directory = tempfile::tempdir().unwrap();
        let database = directory.path().join("node.db");
        let cache_root = directory.path().join("cache");
        let runtime_root = directory.path().join("data/node/runtime");
        let machine = MachineId::parse(&"c9".repeat(32)).unwrap();
        let mut store = NodeStore::open(&database, machine).unwrap();
        let media_path = directory.path().join("cached-stage2.jpg");
        fs::write(&media_path, b"cached stage2").unwrap();
        let scanned = ScannedPath::new(
            NormalizedPath::new(&media_path).unwrap(),
            DisplayPath::new(&media_path).unwrap(),
            13,
        );
        let content = store
            .upsert_content_and_location(&scanned, [0x82; 16], dedup_core::MediaKind::Image)
            .unwrap();
        store
            .commit_feature_result(
                content.id,
                None,
                FeatureWrite::ImageStage1(ImageStage1Fields::from(ImageStage1 {
                    width: 10,
                    height: 10,
                    pdq: PdqHash::from_bytes([0; 32]),
                    quality: 100,
                })),
            )
            .unwrap();
        store
            .commit_feature_result(
                content.id,
                None,
                FeatureWrite::ImageStage2(ImageStage2 {
                    phash_parts: [9; 9],
                    sobel: [9.0; 128],
                }),
            )
            .unwrap();
        store.mark_base_complete(content.id).unwrap();
        let before_highwater = store.outbox_high_seq().unwrap();
        let location = LocationKey::new(store.machine_id().clone(), scanned.normalized_path);
        let observer = store.reopen().unwrap();
        let (pool, mut started, _) = WorkerPool::controlled_batch_for_test(1);
        let (handle, actor) = NodeEngine::spawn_with_runtime_root_for_test(
            store,
            pool,
            "127.0.0.1:39091".parse().unwrap(),
            &cache_root,
            &runtime_root,
            EnumeratorKind::WindowsWalker,
        );
        let registry = handle.runtime_tasks_for_test();

        let response = handle
            .handle(proto::Envelope {
                request_id: 73,
                payload: Some(proto::envelope::Payload::DispatchStage2(
                    proto::DispatchStage2 {
                        analysis_run_id: AnalysisRunId::new().as_uuid().to_string(),
                        items: vec![proto::Stage2WorkItem {
                            content: Some((&content.key).into()),
                            source: Some((&location).into()),
                            frame_slots: Vec::new(),
                        }],
                    },
                )),
            })
            .await;
        let Some(proto::envelope::Payload::TaskAccepted(accepted)) = response.payload else {
            panic!("缓存重发必须返回瞬态运行 ID");
        };
        let details = tokio::time::timeout(Duration::from_secs(2), async {
            loop {
                let details = registry.details(&accepted.task_id).await.unwrap();
                if details
                    .summary
                    .as_ref()
                    .is_some_and(|summary| summary.state == "completed")
                {
                    break details;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("完整缓存命中仍必须以 Completed 收束");
        let highwater = details
            .summary
            .and_then(|summary| summary.outbox_high_seq)
            .expect("缓存重发 Completed 必须携带真实高水位");
        assert!(highwater > before_highwater);
        assert!(started.try_recv().is_err(), "完整缓存命中不得启动 Worker");
        assert!(
            !runtime_root.join(&accepted.task_id).exists(),
            "完整缓存命中不得创建瞬态任务目录"
        );
        assert!(observer.page_tasks(None, 100).unwrap().items.is_empty());

        handle.shutdown().await.unwrap();
        actor.await.unwrap();
    }

    #[tokio::test]
    async fn cancel_stage2_waits_for_transient_run_cleanup_without_legacy_task_rows() {
        let directory = tempfile::tempdir().unwrap();
        let database = directory.path().join("node.db");
        let cache_root = directory.path().join("cache");
        let runtime_root = directory.path().join("data/node/runtime");
        let machine = MachineId::parse(&"ca".repeat(32)).unwrap();
        let mut store = NodeStore::open(&database, machine).unwrap();
        let media_path = directory.path().join("cancel-stage2.jpg");
        fs::write(&media_path, b"cancel stage2").unwrap();
        let scanned = ScannedPath::new(
            NormalizedPath::new(&media_path).unwrap(),
            DisplayPath::new(&media_path).unwrap(),
            13,
        );
        let content = store
            .upsert_content_and_location(&scanned, [0x83; 16], dedup_core::MediaKind::Image)
            .unwrap();
        store
            .commit_feature_result(
                content.id,
                None,
                FeatureWrite::ImageStage1(ImageStage1Fields::from(ImageStage1 {
                    width: 10,
                    height: 10,
                    pdq: PdqHash::from_bytes([0; 32]),
                    quality: 100,
                })),
            )
            .unwrap();
        store.mark_base_complete(content.id).unwrap();
        let location = LocationKey::new(store.machine_id().clone(), scanned.normalized_path);
        let observer = store.reopen().unwrap();
        let (pool, mut started, _controller) = WorkerPool::controlled_batch_for_test(1);
        let (handle, actor) = NodeEngine::spawn_with_runtime_root_for_test(
            store,
            pool,
            "127.0.0.1:39091".parse().unwrap(),
            &cache_root,
            &runtime_root,
            EnumeratorKind::WindowsWalker,
        );
        let registry = handle.runtime_tasks_for_test();

        let response = handle
            .handle(proto::Envelope {
                request_id: 74,
                payload: Some(proto::envelope::Payload::DispatchStage2(
                    proto::DispatchStage2 {
                        analysis_run_id: AnalysisRunId::new().as_uuid().to_string(),
                        items: vec![proto::Stage2WorkItem {
                            content: Some((&content.key).into()),
                            source: Some((&location).into()),
                            frame_slots: Vec::new(),
                        }],
                    },
                )),
            })
            .await;
        let Some(proto::envelope::Payload::TaskAccepted(accepted)) = response.payload else {
            panic!("可取消二筛必须返回瞬态运行 ID");
        };
        let _ = tokio::time::timeout(Duration::from_secs(2), started.recv())
            .await
            .expect("可取消二筛必须进入 Worker")
            .expect("可控 Worker 不应提前关闭");
        let run_directory = runtime_root.join(&accepted.task_id);
        assert!(run_directory.is_dir());

        let cancel = handle
            .handle(proto::Envelope {
                request_id: 75,
                payload: Some(proto::envelope::Payload::CancelTask(proto::CancelTask {
                    task_id: accepted.task_id.clone(),
                })),
            })
            .await;
        assert!(matches!(
            cancel.payload,
            Some(proto::envelope::Payload::CancelTask(_))
        ));
        let details = tokio::time::timeout(Duration::from_secs(2), async {
            loop {
                let details = registry.details(&accepted.task_id).await.unwrap();
                if details
                    .summary
                    .as_ref()
                    .is_some_and(|summary| summary.state == "cancelled")
                {
                    break details;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("取消必须在 Worker、permit、writer 与 TSV 收束后发布 Cancelled");
        assert!(
            details
                .summary
                .is_some_and(|summary| summary.outbox_high_seq.is_none()),
            "取消不得伪造 outbox 高水位"
        );
        assert!(!run_directory.exists());
        assert!(observer.page_tasks(None, 100).unwrap().items.is_empty());

        handle.shutdown().await.unwrap();
        actor.await.unwrap();
    }

    #[tokio::test]
    async fn dispatch_stage2_keeps_batch_completed_when_one_file_fails() {
        let directory = tempfile::tempdir().unwrap();
        let database = directory.path().join("node.db");
        let cache_root = directory.path().join("cache");
        let runtime_root = directory.path().join("data/node/runtime");
        let machine = MachineId::parse(&"cb".repeat(32)).unwrap();
        let mut store = NodeStore::open(&database, machine).unwrap();
        let mut items = Vec::new();
        for (name, md5) in [
            ("failed-stage2.jpg", [0x84; 16]),
            ("next-stage2.jpg", [0x85; 16]),
        ] {
            let media_path = directory.path().join(name);
            fs::write(&media_path, b"stage2 fixture").unwrap();
            let scanned = ScannedPath::new(
                NormalizedPath::new(&media_path).unwrap(),
                DisplayPath::new(&media_path).unwrap(),
                14,
            );
            let content = store
                .upsert_content_and_location(&scanned, md5, dedup_core::MediaKind::Image)
                .unwrap();
            store
                .commit_feature_result(
                    content.id,
                    None,
                    FeatureWrite::ImageStage1(ImageStage1Fields::from(ImageStage1 {
                        width: 10,
                        height: 10,
                        pdq: PdqHash::from_bytes([0; 32]),
                        quality: 100,
                    })),
                )
                .unwrap();
            store.mark_base_complete(content.id).unwrap();
            items.push(proto::Stage2WorkItem {
                content: Some((&content.key).into()),
                source: Some(
                    (&LocationKey::new(store.machine_id().clone(), scanned.normalized_path)).into(),
                ),
                frame_slots: Vec::new(),
            });
        }
        let observer = store.reopen().unwrap();
        let (pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
        let (handle, actor) = NodeEngine::spawn_with_runtime_root_for_test(
            store,
            pool,
            "127.0.0.1:39091".parse().unwrap(),
            &cache_root,
            &runtime_root,
            EnumeratorKind::WindowsWalker,
        );
        let registry = handle.runtime_tasks_for_test();

        let response = handle
            .handle(proto::Envelope {
                request_id: 76,
                payload: Some(proto::envelope::Payload::DispatchStage2(
                    proto::DispatchStage2 {
                        analysis_run_id: AnalysisRunId::new().as_uuid().to_string(),
                        items,
                    },
                )),
            })
            .await;
        let Some(proto::envelope::Payload::TaskAccepted(accepted)) = response.payload else {
            panic!("含单文件失败的二筛必须返回瞬态运行 ID");
        };
        let (failed_task_id, failed_item_id) =
            tokio::time::timeout(Duration::from_secs(2), started.recv())
                .await
                .expect("首个二筛必须进入 Worker")
                .expect("可控 Worker 不应提前关闭");
        controller
            .crash(
                failed_task_id,
                failed_item_id,
                "测试二筛 Worker 崩溃".into(),
            )
            .await;
        let (next_task_id, next_item_id) =
            tokio::time::timeout(Duration::from_secs(2), started.recv())
                .await
                .expect("首个文件失败后下一项必须继续")
                .expect("可控 Worker 不应提前关闭");
        controller
            .stage2_source_read_complete(next_task_id.clone(), next_item_id.clone())
            .await;
        controller
            .complete_stage2(next_task_id, next_item_id, stage2_success_output(10))
            .await;
        let details = tokio::time::timeout(Duration::from_secs(2), async {
            loop {
                let details = registry.details(&accepted.task_id).await.unwrap();
                if details
                    .summary
                    .as_ref()
                    .is_some_and(|summary| summary.state == "completed")
                {
                    break details;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("单文件 F 不得将整个瞬态二筛伪装为 Failed");
        let summary = details.summary.unwrap();
        assert_eq!(summary.overall_total, 2);
        assert_eq!(summary.overall_completed, 1);
        assert_eq!(summary.overall_failed, 1);
        assert!(summary.outbox_high_seq.is_some());
        assert!(observer.page_tasks(None, 100).unwrap().items.is_empty());

        handle.shutdown().await.unwrap();
        actor.await.unwrap();
    }

    #[tokio::test]
    async fn stage2_create_and_shutdown_stay_responsive_while_worker_is_held() {
        let directory = tempfile::tempdir().unwrap();
        let machine = MachineId::parse(&"c2".repeat(32)).unwrap();
        let mut store = NodeStore::open(&directory.path().join("node.db"), machine).unwrap();
        let media_path = directory.path().join("held.jpg");
        fs::write(&media_path, b"stage2 fixture").unwrap();
        let scanned = ScannedPath::new(
            dedup_core::NormalizedPath::new(&media_path).unwrap(),
            DisplayPath::new(&media_path).unwrap(),
            14,
        );
        let content = store
            .upsert_content_and_location(&scanned, [0x42; 16], dedup_core::MediaKind::Image)
            .unwrap();
        store
            .commit_feature_result(
                content.id,
                None,
                FeatureWrite::ImageStage1(ImageStage1Fields::from(ImageStage1 {
                    width: 10,
                    height: 10,
                    pdq: PdqHash::from_bytes([0; 32]),
                    quality: 100,
                })),
            )
            .unwrap();
        store.mark_base_complete(content.id).unwrap();
        let location =
            LocationKey::new(store.machine_id().clone(), scanned.normalized_path.clone());
        let observer = store.reopen().unwrap();
        let (pool, mut started) = WorkerPool::controlled_for_test();
        let (handle, actor) = NodeEngine::spawn(
            store,
            pool,
            "127.0.0.1:39091".parse().unwrap(),
            directory.path(),
            EnumeratorKind::WindowsWalker,
        );

        let dispatch_handle = handle.clone();
        let dispatch = tokio::spawn(async move {
            dispatch_handle
                .handle(proto::Envelope {
                    request_id: 10,
                    payload: Some(proto::envelope::Payload::DispatchStage2(
                        proto::DispatchStage2 {
                            analysis_run_id: AnalysisRunId::new().as_uuid().to_string(),
                            items: vec![proto::Stage2WorkItem {
                                content: Some((&content.key).into()),
                                source: Some((&location).into()),
                                frame_slots: Vec::new(),
                            }],
                        },
                    )),
                })
                .await
        });
        let (running_task_id, _) = tokio::time::timeout(Duration::from_secs(2), started.recv())
            .await
            .expect("二筛必须到达可控 Worker 屏障")
            .expect("可控 Worker 不应提前关闭");

        let accepted = tokio::time::timeout(Duration::from_secs(2), dispatch)
            .await
            .expect("DispatchStage2 必须在 Worker 屏障释放前返回")
            .unwrap();
        let Some(proto::envelope::Payload::TaskAccepted(accepted)) = accepted.payload else {
            panic!("DispatchStage2 必须返回真实任务 ID");
        };
        assert_eq!(accepted.task_id, running_task_id);

        let restart = tokio::time::timeout(Duration::from_secs(2), handle.restart_engine())
            .await
            .expect("运行中重启必须等待 Worker 收束");
        assert!(
            matches!(restart, Err(EngineError::Operation(_))),
            "可控测试池必须在收束后明确拒绝冒充生产 Pool 重建"
        );
        let task = handle
            .runtime_tasks_for_test()
            .list()
            .await
            .into_iter()
            .find(|task| task.runtime_task_id == running_task_id)
            .expect("重启收束后必须保留瞬态任务终态");
        assert_eq!(task.state, "cancelled");
        assert!(task.outbox_high_seq.is_none());
        assert!(observer.page_tasks(None, 100).unwrap().items.is_empty());

        tokio::time::timeout(Duration::from_secs(2), handle.shutdown())
            .await
            .expect("Shutdown 必须在二筛 Worker 仍被控制时返回")
            .unwrap();
        tokio::time::timeout(Duration::from_secs(2), actor)
            .await
            .expect("actor 关闭不能等待长二筛")
            .unwrap();
    }
}
