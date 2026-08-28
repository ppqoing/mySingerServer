//! 节点业务 actor：网络和托盘只发送命令，SQLite 与 WorkerPool 保持单一所有者。

use std::{
    collections::BTreeMap,
    fs,
    future::Future,
    net::SocketAddr,
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
    time::{Duration, SystemTime, UNIX_EPOCH},
};

use dedup_core::{
    AnalysisRunId, ContentKey, CoreError, DeleteMode, DiskReadConfig, DisplayPath, EnumeratorKind,
    LocationKey, MachineId, NodeConfig, NodePostgresConfig, TaskId, Thresholds,
};
use dedup_node_store::{
    AnalysisStatus, ConfirmedDeleteItem, DeleteBatchPlan, DeleteOutcome, GroupKind, NodeStore,
    OwnedSnapshot, PersistentStageState as StorePersistentStageState, PlannedDeleteItem,
    ReviewDecision, ScannedPath, StoreError, TaskSnapshot, TaskStageWrite, TaskStatus,
};
use dedup_protocol::proto;
use thiserror::Error;
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
    analysis::{
        LocalAnalysisEngine, Stage2BatchItem, Stage2BatchPlan, WorkerPoolStage2Processor,
        begin_stage2_batch, run_stage2_batch_with_runtime_cache,
    },
    artifact_registry::RegenerableArtifactRegistry,
    config_repository::{
        ConfigRepositoryError, LoadedNodeConfig, NodeConfigRepository, ResolvedNodePaths,
    },
    delete::DeleteEngine,
    disk_full_cleanup::{DiskFullCleaner, SystemArtifactDiskResolver},
    preview::{PreviewKind, PreviewService},
    runtime_tasks::{
        RuntimeFailureUpdate, RuntimeProgressPublisher, RuntimeProgressUnit, RuntimeStage,
        RuntimeTaskKind, RuntimeTaskRegistry, RuntimeTaskReporter, RuntimeTaskState,
    },
    scan::{
        BaseComputeEngine, FileEnumerator, PipelineFileReader, PipelineLimits, PlannedScannedPath,
        PreferredEverythingEnumerator, ScanDiskPlan, ScanError, ScanOptions,
        ScanRootStorageResolver, ScanSummary, ScheduledFileReader, SystemScanRootStorageResolver,
        WindowsWalker, begin_scan_task, ensure_everything_ready,
        input_order::interleave_rows_by_root,
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
        let disk_full_cleaner =
            DiskFullCleaner::new(Arc::clone(&artifact_registry), SystemArtifactDiskResolver);
        let (handle, actor_task) = spawn_actor(
            store,
            Some(worker_pool),
            listen_address,
            &paths.cache_path,
            config.enumerator,
            config.read.clone(),
            config.postgres.clone(),
            effective_worker_count,
            Some(Box::new(repository)),
            artifact_registry,
            disk_full_cleaner,
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

/// Actor 测试注入点；默认构建是零大小类型且没有运行时分支。
#[derive(Default)]
struct ActorTestHooks {
    /// 仅 feature 测试把首条 Base persist 暂停在 SQLite 事务前。
    #[cfg(feature = "test-hooks")]
    first_persist_waiter: Option<BasePersistTestWaiter>,
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
            EnumeratorKind::WindowsWalker,
            DiskReadConfig::default(),
            NodePostgresConfig::default(),
            1,
            None,
            artifact_registry,
            disk_full_cleaner,
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
            EnumeratorKind::WindowsWalker,
            DiskReadConfig::default(),
            NodePostgresConfig::default(),
            1,
            Some(repository),
            artifact_registry,
            disk_full_cleaner,
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
        let (artifact_registry, disk_full_cleaner) = test_artifact_cleanup(cache_root);
        spawn_actor(
            store,
            Some(worker_pool),
            listen_address,
            cache_root,
            enumerator,
            DiskReadConfig::default(),
            NodePostgresConfig::default(),
            effective_worker_count,
            None,
            artifact_registry,
            disk_full_cleaner,
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
            enumerator,
            DiskReadConfig::default(),
            NodePostgresConfig::default(),
            effective_worker_count,
            None,
            artifact_registry,
            disk_full_cleaner,
            None,
            ActorTestHooks {
                first_persist_waiter: Some(first_persist_waiter),
            },
        )
    }
}

fn spawn_actor(
    store: NodeStore,
    worker_pool: Option<WorkerPool>,
    listen_address: SocketAddr,
    cache_root: &Path,
    enumerator: EnumeratorKind,
    read_config: DiskReadConfig,
    postgres_config: NodePostgresConfig,
    effective_worker_count: usize,
    config_repository: Option<Box<dyn NodeConfigRepositoryAccess>>,
    artifact_registry: Arc<RegenerableArtifactRegistry>,
    disk_full_cleaner: DiskFullCleaner,
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
            enumerator,
            read_config,
            postgres_config,
            effective_worker_count,
            restarting: false,
            snapshots: BTreeMap::new(),
            active_job: None,
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

fn test_artifact_cleanup(cache_root: &Path) -> (Arc<RegenerableArtifactRegistry>, DiskFullCleaner) {
    fs::create_dir_all(cache_root).expect("Node test cache root must be creatable");
    let install_root = cache_root
        .parent()
        .expect("Node test cache root must have a distinct install parent");
    let registry = Arc::new(
        RegenerableArtifactRegistry::new(install_root, cache_root)
            .expect("Node test artifact roots must be absolute and nested"),
    );
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
    enumerator: EnumeratorKind,
    read_config: DiskReadConfig,
    postgres_config: NodePostgresConfig,
    effective_worker_count: usize,
    restarting: bool,
    snapshots: BTreeMap<String, OwnedSnapshot>,
    active_job: Option<ActiveJob>,
    commands: mpsc::WeakSender<EngineCommand>,
    config_repository: Option<Box<dyn NodeConfigRepositoryAccess>>,
    artifact_registry: Arc<RegenerableArtifactRegistry>,
    disk_full_cleaner: DiskFullCleaner,
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
    BackgroundFinished { identity: JobIdentity },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum JobIdentity {
    Task(TaskId),
    Analysis(AnalysisRunId),
}

struct ActiveJob {
    identity: JobIdentity,
    /// `run_background_job` 已完整返回，包含 task-local writer join 与 Store 恢复。
    completion: oneshot::Receiver<()>,
    cancellation: Option<ReadCancellationToken>,
    /// 后台任务收束后归还的唯一 Pool 所有权，只有 actor 可以取回。
    returned_pool: Arc<Mutex<Option<WorkerPool>>>,
}

enum BackgroundJob {
    Scan {
        task_id: TaskId,
        options: ScanOptions,
        enumerator: EnumeratorKind,
        contact_sheets: PathBuf,
        read_config: DiskReadConfig,
        postgres_config: NodePostgresConfig,
        effective_worker_count: usize,
        cancellation: ReadCancellationToken,
        runtime_reporter: RuntimeTaskReporter,
        artifact_registry: Arc<RegenerableArtifactRegistry>,
        disk_full_cleaner: DiskFullCleaner,
        /// 仅 feature 测试暂停首条 SQLite persist。
        #[cfg(feature = "test-hooks")]
        first_persist_waiter: Option<BasePersistTestWaiter>,
    },
    LocalAnalysis {
        run_id: AnalysisRunId,
        runtime_reporter: RuntimeTaskReporter,
        contact_sheets: PathBuf,
        postgres_config: NodePostgresConfig,
    },
    Stage2 {
        plan: Stage2BatchPlan,
        runtime_reporter: RuntimeTaskReporter,
        contact_sheets: PathBuf,
        postgres_config: NodePostgresConfig,
    },
}

impl BackgroundJob {
    const fn identity(&self) -> JobIdentity {
        match self {
            Self::Scan { task_id, .. } => JobIdentity::Task(*task_id),
            Self::LocalAnalysis { run_id, .. } => JobIdentity::Analysis(*run_id),
            Self::Stage2 { plan, .. } => JobIdentity::Task(plan.task_id),
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
            EngineCommand::BackgroundFinished { identity } => state.finish_background(identity),
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
            Some(proto::envelope::Payload::QueryTask(query)) => self.query_task(query),
            Some(proto::envelope::Payload::ListTasks(query)) => self.list_tasks(query),
            Some(proto::envelope::Payload::BrowsePaths(query)) => browse_paths(query),
            Some(proto::envelope::Payload::CreateLocalAnalysis(create)) => {
                self.create_local_analysis(create).await
            }
            Some(proto::envelope::Payload::QueryAnalysisRun(query)) => {
                self.query_analysis_run(query)
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
        let (queued_items, running_items) =
            self.store.task_activity_counts().map_err(store_error)?;
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
        let task_id = begin_scan_task(&mut self.store, &options, now_ms()).map_err(internal)?;
        initialize_base_task_stages(&mut self.store, task_id).map_err(internal)?;
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
            contact_sheets,
            read_config: self.read_config.clone(),
            postgres_config: self.postgres_config.clone(),
            effective_worker_count: self.effective_worker_count,
            cancellation,
            runtime_reporter,
            artifact_registry: Arc::clone(&self.artifact_registry),
            disk_full_cleaner: self.disk_full_cleaner.clone(),
            #[cfg(feature = "test-hooks")]
            first_persist_waiter,
        }) {
            let _ = self.store.fail_task(task_id, now_ms());
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
        let cancel_gate = self
            .worker_control
            .as_ref()
            .map(|pool| pool.begin_task_cancel(&request.task_id));
        if let Err(error) = self.store.cancel_task(task_id, now_ms()) {
            if let Some(cancel_gate) = cancel_gate {
                cancel_gate.rollback();
            }
            return Err(store_error(error));
        }
        if let Some(cancel_gate) = cancel_gate {
            cancel_gate.commit();
        }
        if let Some(active) = &self.active_job
            && active.identity == JobIdentity::Task(task_id)
        {
            if let Some(cancellation) = &active.cancellation {
                cancellation.cancel();
            }
        }
        if let Some(pool) = &self.worker_control {
            pool.cancel_task(&request.task_id).await.map_err(internal)?;
        }
        Ok(proto::envelope::Payload::CancelTask(request))
    }

    fn query_task(&self, request: proto::QueryTask) -> ProtocolResult {
        let snapshot = self
            .store
            .task_snapshot(parse_task_id(&request.task_id)?)
            .map_err(store_error)?;
        Ok(proto::envelope::Payload::QueryTask(proto::QueryTask {
            task_id: request.task_id,
            task: Some(task_summary(snapshot)),
        }))
    }

    fn list_tasks(&self, request: proto::ListTasks) -> ProtocolResult {
        let page = self
            .store
            .page_tasks(cursor(&request.cursor), request.limit as usize)
            .map_err(store_error)?;
        Ok(proto::envelope::Payload::ListTasks(proto::ListTasks {
            cursor: request.cursor,
            limit: request.limit,
            tasks: page.items.into_iter().map(task_summary).collect(),
            next_cursor: page.next_cursor.unwrap_or_default(),
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
        let run_id = LocalAnalysisEngine::begin(&mut self.store, &tasks, thresholds, now_ms())
            .map_err(internal)?;
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
        if let Err(error) = self.start_background(BackgroundJob::LocalAnalysis {
            run_id,
            runtime_reporter,
            contact_sheets: self.cache_root.join("contact-sheets"),
            postgres_config: self.postgres_config.clone(),
        }) {
            let _ = self
                .store
                .transition_analysis_run(run_id, AnalysisStatus::Partial, now_ms());
            let _ = runtime_failure.finish(RuntimeTaskState::Failed).await;
            return Err(internal(error));
        }
        Ok(proto::envelope::Payload::QueryAnalysisRun(
            proto::QueryAnalysisRun {
                analysis_run_id: run_id.as_uuid().to_string(),
                state: analysis_status_name(AnalysisStatus::CollectingStage1).into(),
                input_count: 0,
                candidate_count: 0,
                error_text: String::new(),
            },
        ))
    }

    fn query_analysis_run(&self, request: proto::QueryAnalysisRun) -> ProtocolResult {
        let run_id = parse_analysis_id(&request.analysis_run_id)?;
        let snapshot = self
            .store
            .analysis_run_snapshot(run_id)
            .map_err(store_error)?;
        let input_count = self
            .store
            .analysis_inputs(run_id)
            .map_err(store_error)?
            .len() as u64;
        let candidate_count = self
            .store
            .analysis_candidates(run_id)
            .map_err(store_error)?
            .len() as u64;
        Ok(proto::envelope::Payload::QueryAnalysisRun(
            proto::QueryAnalysisRun {
                analysis_run_id: request.analysis_run_id,
                state: analysis_status_name(snapshot.status).into(),
                input_count,
                candidate_count,
                error_text: String::new(),
            },
        ))
    }

    fn list_groups(&self, request: proto::ListGroups) -> ProtocolResult {
        let kind = group_filter(request.group_kind)?;
        let page = self
            .store
            .page_groups_filtered(
                parse_analysis_id(&request.analysis_run_id)?,
                kind,
                cursor(&request.cursor),
                request.limit as usize,
            )
            .map_err(store_error)?;
        Ok(proto::envelope::Payload::ListGroups(proto::ListGroups {
            analysis_run_id: request.analysis_run_id,
            group_kind: request.group_kind,
            cursor: request.cursor,
            limit: request.limit,
            groups: page
                .items
                .into_iter()
                .map(|group| proto::DuplicateGroup {
                    group_id: group.group_id,
                    kind: wire_group_kind(group.kind) as i32,
                    representative: Some((&group.representative).into()),
                    member_count: group.member_count,
                    reclaimable_bytes: group.reclaimable_bytes,
                })
                .collect(),
            next_cursor: page.next_cursor.unwrap_or_default(),
        }))
    }

    fn list_group_members(&self, request: proto::ListGroupMembers) -> ProtocolResult {
        let run_id = parse_analysis_id(&request.analysis_run_id)?;
        let page = self
            .store
            .page_group_members(
                run_id,
                &request.group_id,
                cursor(&request.cursor),
                request.limit as usize,
            )
            .map_err(store_error)?;
        let members = page
            .items
            .into_iter()
            .map(|member| {
                let review = self
                    .store
                    .review_mark(run_id, &request.group_id, &member.location)
                    .map_err(store_error)?
                    .map_or(proto::ReviewDecision::ReviewUndecided, wire_review);
                Ok(proto::GroupMember {
                    location: Some((&member.location).into()),
                    content: Some((&member.content).into()),
                    representative: member.representative,
                    stage1_score: member.stage1_score as f32,
                    phash_passed_parts: member.phash_passed_parts.map_or(0, u32::from),
                    stage2_score: member.stage2_score.unwrap_or_default() as f32,
                    review: review as i32,
                    active: member.active,
                    width: member.width.unwrap_or_default(),
                    height: member.height.unwrap_or_default(),
                    quality: member.quality.map_or(0, u32::from),
                })
            })
            .collect::<Result<Vec<_>, (proto::ErrorCode, String)>>()?;
        Ok(proto::envelope::Payload::ListGroupMembers(
            proto::ListGroupMembers {
                group_id: request.group_id,
                cursor: request.cursor,
                limit: request.limit,
                members,
                next_cursor: page.next_cursor.unwrap_or_default(),
                analysis_run_id: request.analysis_run_id,
            },
        ))
    }

    fn save_review_mark(&mut self, request: proto::SaveReviewMark) -> ProtocolResult {
        let location = request
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
        self.store
            .save_review_mark(
                parse_analysis_id(&request.analysis_run_id)?,
                &request.group_id,
                &location,
                decision,
            )
            .map_err(store_error)?;
        Ok(proto::envelope::Payload::SaveReviewMark(request))
    }

    fn prepare_analysis_input(&self, request: proto::PrepareAnalysisInput) -> ProtocolResult {
        let rows = if request.scan_task_ids.is_empty() {
            let run_id = parse_analysis_id(&request.analysis_run_id)?;
            self.store.analysis_inputs(run_id).map_err(store_error)?
        } else {
            let tasks = request
                .scan_task_ids
                .iter()
                .map(|task_id| parse_task_id(task_id))
                .collect::<Result<Vec<_>, _>>()?;
            self.store
                .analysis_inputs_for_tasks(&tasks)
                .map_err(store_error)?
        };
        let mut grouped = BTreeMap::<ContentKey, Vec<LocationKey>>::new();
        for row in rows {
            grouped.entry(row.content).or_default().push(row.location);
        }
        let start = if request.cursor.is_empty() {
            0
        } else {
            request.cursor.parse::<usize>().map_err(invalid)?
        };
        let limit = request.limit as usize;
        let entries = grouped.into_iter().collect::<Vec<_>>();
        let end = start.saturating_add(limit).min(entries.len());
        let mut inputs = Vec::with_capacity(end.saturating_sub(start));
        for (content, locations) in &entries[start..end] {
            let content_id = self
                .store
                .content_id_by_key(*content)
                .map_err(store_error)?
                .ok_or_else(|| internal("分析输入内容不存在"))?;
            let kind = self
                .store
                .content_media_kind(content_id)
                .map_err(store_error)?;
            inputs.push(proto::AnalysisInput {
                content: Some(content.into()),
                locations: locations.iter().map(Into::into).collect(),
                media_kind: wire_media_kind(kind) as i32,
                stage1_complete: self
                    .store
                    .load_complete_stage1(content_id)
                    .map_err(store_error)?
                    .is_some(),
                stage2_complete: self
                    .store
                    .load_complete_stage2(content_id)
                    .map_err(store_error)?
                    .is_some(),
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
        if let Err(error) = self.start_background(BackgroundJob::Stage2 {
            plan,
            runtime_reporter,
            contact_sheets: self.cache_root.join("contact-sheets"),
            postgres_config: self.postgres_config.clone(),
        }) {
            let _ = self.store.fail_task(task_id, now_ms());
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

    async fn create_delete_batch(&mut self, request: proto::CreateDeleteBatch) -> ProtocolResult {
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
            let confirmed = request
                .items
                .iter()
                .map(|item| {
                    Ok(ConfirmedDeleteItem::new(
                        item.group_id.clone(),
                        item.location
                            .clone()
                            .ok_or_else(|| invalid("本地删除项缺少位置"))?
                            .try_into()
                            .map_err(invalid)?,
                        item.expected_content
                            .clone()
                            .ok_or_else(|| invalid("本地删除项缺少内容键"))?
                            .try_into()
                            .map_err(invalid)?,
                    ))
                })
                .collect::<Result<Vec<_>, (proto::ErrorCode, String)>>()?;
            self.store
                .create_delete_batch(
                    parse_analysis_id(&request.analysis_run_id)?,
                    &confirmed,
                    mode,
                    now_ms(),
                )
                .map_err(store_error)?
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
            DeleteEngine::execute_external_with_runtime(&mut self.store, &plan, &runtime_reporter)
                .await
        } else {
            DeleteEngine::execute_batch_with_runtime(&mut self.store, &plan, &runtime_reporter)
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
        let mut store = self.store.reopen().map_err(|error| error.to_string())?;
        let mut worker_pool = self
            .worker_pool
            .take()
            .ok_or_else(|| "后台 WorkerPool owner 不可用".to_owned())?;
        let identity = job.identity();
        let cancellation = match &job {
            BackgroundJob::Scan { cancellation, .. } => Some(cancellation.clone()),
            _ => None,
        };
        let commands = self
            .commands
            .upgrade()
            .ok_or_else(|| "节点计算引擎已经关闭".to_owned())?;
        let (completion_sender, completion) = oneshot::channel();
        let returned_pool = Arc::new(Mutex::new(None));
        let background_pool = Arc::clone(&returned_pool);
        tokio::spawn(async move {
            run_background_job(&mut store, &mut worker_pool, job).await;
            // 必须先确认 BaseStoreActor join、Store 恢复和终态发布全部结束，再触发 shutdown/归还 Pool。
            *background_pool.lock().expect("后台 Pool 归还锁未中毒") = Some(worker_pool);
            let _ = completion_sender.send(());
            let _ = commands
                .send(EngineCommand::BackgroundFinished { identity })
                .await;
        });
        self.active_job = Some(ActiveJob {
            identity,
            completion,
            cancellation,
            returned_pool,
        });
        Ok(())
    }

    fn finish_background(&mut self, identity: JobIdentity) {
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
    }

    /// 取消活动任务并等待后台完全收束，返回被 actor 独占的旧 Pool。
    async fn stop_background_for_shutdown(&mut self) -> Option<WorkerPool> {
        let Some(active) = self.active_job.take() else {
            return self.worker_pool.take();
        };
        let pool_task_id = match active.identity {
            JobIdentity::Task(task_id) => task_id.as_uuid().to_string(),
            JobIdentity::Analysis(run_id) => run_id.as_uuid().to_string(),
        };
        match active.identity {
            JobIdentity::Task(task_id) => {
                finish_running_task_stages(
                    &mut self.store,
                    task_id,
                    StorePersistentStageState::Skipped,
                );
                let _ = self.store.cancel_task(task_id, now_ms());
            }
            JobIdentity::Analysis(run_id) => {
                finish_running_analysis_stages(
                    &mut self.store,
                    run_id,
                    StorePersistentStageState::Skipped,
                );
                let _ =
                    self.store
                        .transition_analysis_run(run_id, AnalysisStatus::Cancelled, now_ms());
            }
        }
        if let Some(cancellation) = &active.cancellation {
            cancellation.cancel();
        }
        if let Some(pool) = &self.worker_control
            && let Err(error) = pool.cancel_task(&pool_task_id).await
        {
            tracing::warn!(task_id = %pool_task_id, error = %error, "关机取消 WorkerPool 任务失败");
        }
        if active.completion.await.is_err() {
            // 发送端只会在后台 panic/异常销毁时消失；此处禁止伪造终态或指标清零。
            tracing::error!(task_id = %pool_task_id, "关机等待后台完整收束失败，未伪造运行终态");
        }
        self.worker_control = None;
        active
            .returned_pool
            .lock()
            .expect("后台 Pool 归还锁未中毒")
            .take()
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
    store: &mut NodeStore,
    worker_pool: &mut WorkerPool,
    job: BackgroundJob,
) {
    match job {
        BackgroundJob::Scan {
            task_id,
            options,
            enumerator,
            contact_sheets,
            read_config,
            postgres_config,
            effective_worker_count,
            cancellation,
            runtime_reporter,
            artifact_registry,
            disk_full_cleaner,
            #[cfg(feature = "test-hooks")]
            first_persist_waiter,
        } => {
            let enumerate_started = now_ms() as u64;
            let _ = runtime_reporter
                .start_stage_nowait(RuntimeStage::EnumerateFiles, RuntimeProgressUnit::Files);
            let _ = store.save_task_stage(
                task_id,
                stored_stage(
                    RuntimeStage::EnumerateFiles,
                    StorePersistentStageState::Running,
                    0,
                    None,
                    0,
                    0,
                    Some(enumerate_started),
                    None,
                    None,
                ),
            );
            let enumerator =
                resolve_scan_enumerator_with(enumerator, ensure_everything_ready).await;
            // 物理存储计划必须先于第一次 Everything/Walker enumerate 建立；失败时不枚举。
            let enumerated = match enumerator {
                EnumeratorKind::WindowsWalker => enumerate_with_frozen_plan(
                    &options.roots,
                    &read_config,
                    &SystemScanRootStorageResolver,
                    |roots| WindowsWalker.enumerate(roots),
                ),
                EnumeratorKind::Everything => enumerate_with_frozen_plan(
                    &options.roots,
                    &read_config,
                    &SystemScanRootStorageResolver,
                    |roots| PreferredEverythingEnumerator.enumerate(roots),
                ),
            };
            let (rows, planned_rows_for_reader) = match enumerated {
                Ok((rows, planned_rows)) => (Ok(rows), Some(planned_rows)),
                Err(error) => (Err(error), None),
            };
            let result = run_enumerated_scan_to_base_compute(
                store,
                worker_pool,
                task_id,
                options,
                rows,
                enumerate_started,
                &contact_sheets,
                || NodeRemoteFeatureCache::from_config(&postgres_config),
                || async {
                    let Some(planned_rows) = planned_rows_for_reader else {
                        return Err(ScanError::Stage1("扫描根计划缺失，不能创建读取器".into()));
                    };
                    ScheduledFileReader::new_with_planned_rows(
                        &read_config,
                        effective_worker_count,
                        planned_rows,
                    )
                    .map(|(reader, limits)| {
                        (
                            reader.with_runtime_reporter(runtime_reporter.clone()),
                            limits,
                        )
                    })
                },
                &read_config,
                cancellation,
                &runtime_reporter,
                &artifact_registry,
                &disk_full_cleaner,
                now_ms(),
                #[cfg(feature = "test-hooks")]
                first_persist_waiter,
            )
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
            match result {
                Ok(_) => {}
                Err(ScanError::Cancelled) => {
                    finish_running_task_stages(store, task_id, StorePersistentStageState::Skipped);
                    let _ = store.cancel_task(task_id, now_ms());
                }
                Err(_) => {
                    finish_running_task_stages(store, task_id, StorePersistentStageState::Failed);
                    let _ = store.fail_task(task_id, now_ms());
                }
            }
            // Base、WorkerPool、SQLite task-local writer 和持久阶段全部收束后才冻结终态。
            let _ = runtime_reporter.finish(runtime_state).await;
        }
        BackgroundJob::LocalAnalysis {
            run_id,
            runtime_reporter,
            contact_sheets,
            postgres_config,
        } => {
            let mut processor = WorkerPoolStage2Processor::new(worker_pool)
                .with_runtime_reporter(runtime_reporter.clone(), store.machine_id().clone());
            let (mut remote, _) = NodeRemoteFeatureCache::from_config(&postgres_config).await;
            let result = LocalAnalysisEngine::run_existing_with_runtime_cache(
                store,
                run_id,
                &mut processor,
                &runtime_reporter,
                &mut remote,
                &contact_sheets,
                now_ms(),
            )
            .await;
            let state = match &result {
                Ok(report) if report.status == AnalysisStatus::Completed => {
                    RuntimeTaskState::Completed
                }
                _ => RuntimeTaskState::Failed,
            };
            let _ = runtime_reporter.finish(state).await;
            if result.is_err() {
                finish_running_analysis_stages(store, run_id, StorePersistentStageState::Failed);
                let _ = store.transition_analysis_run(run_id, AnalysisStatus::Partial, now_ms());
            }
        }
        BackgroundJob::Stage2 {
            plan,
            runtime_reporter,
            contact_sheets,
            postgres_config,
        } => {
            let task_id = plan.task_id;
            let mut processor = WorkerPoolStage2Processor::new(worker_pool)
                .with_runtime_reporter(runtime_reporter.clone(), store.machine_id().clone());
            let (mut remote, _) = NodeRemoteFeatureCache::from_config(&postgres_config).await;
            let result = run_stage2_batch_with_runtime_cache(
                store,
                plan,
                &mut processor,
                &runtime_reporter,
                &mut remote,
                &contact_sheets,
                now_ms(),
            )
            .await;
            let completed = result.is_ok()
                && store.task_snapshot(task_id).is_ok_and(|snapshot| {
                    snapshot.status == TaskStatus::Completed && snapshot.failed == 0
                });
            let _ = runtime_reporter
                .finish(if completed {
                    RuntimeTaskState::Completed
                } else {
                    RuntimeTaskState::Failed
                })
                .await;
            if result.is_err() {
                finish_running_task_stages(store, task_id, StorePersistentStageState::Failed);
                let _ = store.fail_task(task_id, now_ms());
            }
        }
    }
}

/// 把异常退出时仍运行的 Node 任务阶段关闭，避免重启后持续显示旧计时器。
fn finish_running_task_stages(
    store: &mut NodeStore,
    task_id: TaskId,
    state: StorePersistentStageState,
) {
    let Ok(stages) = store.task_stages(task_id) else {
        return;
    };
    for mut stage in stages
        .into_iter()
        .filter(|stage| stage.state == StorePersistentStageState::Running)
    {
        stage.state = state;
        stage.finished_at_ms = Some(now_ms() as u64);
        let _ = store.save_task_stage(task_id, stage);
    }
}

/// 把异常退出时仍运行的单机清单阶段关闭，保留它自己的开始时间和计数。
fn finish_running_analysis_stages(
    store: &mut NodeStore,
    run_id: AnalysisRunId,
    state: StorePersistentStageState,
) {
    let Ok(stages) = store.analysis_stages(run_id) else {
        return;
    };
    for mut stage in stages
        .into_iter()
        .filter(|stage| stage.state == StorePersistentStageState::Running)
    {
        stage.state = state;
        stage.finished_at_ms = Some(now_ms() as u64);
        let _ = store.save_analysis_stage(run_id, stage);
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

fn task_summary(task: TaskSnapshot) -> proto::TaskSummary {
    proto::TaskSummary {
        task_id: task.task_id.as_uuid().to_string(),
        task_kind: task.kind,
        state: wire_task_status(task.status) as i32,
        total_items: task.total_items,
        completed_items: task.succeeded + task.failed + task.cancelled,
        failed_items: task.failed,
        skipped_items: 0,
        outbox_high_seq: task.outbox_high_seq,
    }
}

const fn wire_task_status(status: TaskStatus) -> proto::TaskState {
    match status {
        TaskStatus::Queued => proto::TaskState::TaskQueued,
        TaskStatus::Running => proto::TaskState::TaskRunning,
        TaskStatus::Completed => proto::TaskState::TaskCompleted,
        TaskStatus::Failed => proto::TaskState::TaskFailed,
        TaskStatus::Cancelled => proto::TaskState::TaskCancelled,
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
    use std::{
        io::{self, Write},
        sync::{Arc, Mutex},
    };

    use dedup_media::{ImageStage1, PdqHash};
    #[cfg(feature = "test-hooks")]
    use dedup_media_ffmpeg::MediaProbe;
    use dedup_node_store::{FeatureWrite, ImageStage1Fields, ScannedPath};
    #[cfg(feature = "test-hooks")]
    use tracing_subscriber::fmt::MakeWriter;

    use super::*;
    #[cfg(feature = "test-hooks")]
    use crate::DisabledRemoteFeatureCache;
    #[cfg(feature = "test-hooks")]
    use crate::{scan::BasePersistTestController, worker::BaseComputeOutput};

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
        let Some(proto::envelope::Payload::QueryTask(query)) = query.payload else {
            panic!("持久任务必须可查询");
        };
        assert_eq!(
            query.task.unwrap().state,
            proto::TaskState::TaskRunning as i32
        );

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

        let final_query = handle
            .handle(proto::Envelope {
                request_id: 4,
                payload: Some(proto::envelope::Payload::QueryTask(proto::QueryTask {
                    task_id: accepted.task_id,
                    task: None,
                })),
            })
            .await;
        let Some(proto::envelope::Payload::QueryTask(final_query)) = final_query.payload else {
            panic!("取消后的任务必须可查询");
        };
        assert_eq!(
            final_query.task.unwrap().state,
            proto::TaskState::TaskCancelled as i32
        );

        handle.shutdown().await.unwrap();
        actor.await.unwrap();
    }

    #[tokio::test]
    async fn cancel_task_publishes_terminal_only_after_all_pipeline_ownership_is_released() {
        let directory = tempfile::tempdir().unwrap();
        let scan_root = directory.path().join("scan");
        fs::create_dir(&scan_root).unwrap();
        fs::write(scan_root.join("held.mp4"), b"held worker input").unwrap();
        let machine = MachineId::parse(&"c2".repeat(32)).unwrap();
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
    }

    #[tokio::test]
    async fn shutdown_releases_real_worker_before_publishing_cancelled_terminal() {
        let directory = tempfile::tempdir().unwrap();
        let scan_root = directory.path().join("scan");
        fs::create_dir(&scan_root).unwrap();
        fs::write(scan_root.join("shutdown.mp4"), b"shutdown worker input").unwrap();
        let machine = MachineId::parse(&"c4".repeat(32)).unwrap();
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
            let persisted = observer
                .task_snapshot(TaskId::from_uuid(
                    Uuid::parse_str(&accepted.task_id).unwrap(),
                ))
                .unwrap();

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
            assert_eq!(persisted.status, TaskStatus::Cancelled);
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
    async fn scan_background_error_is_persisted_as_failed() {
        let directory = tempfile::tempdir().unwrap();
        let machine = MachineId::parse(&"c3".repeat(32)).unwrap();
        let store = NodeStore::open(&directory.path().join("node.db"), machine).unwrap();
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
            panic!("后台枚举失败前必须先返回持久任务 ID");
        };

        let mut state = proto::TaskState::TaskQueued as i32;
        for request_id in 21..1021 {
            let response = handle
                .handle(proto::Envelope {
                    request_id,
                    payload: Some(proto::envelope::Payload::QueryTask(proto::QueryTask {
                        task_id: accepted.task_id.clone(),
                        task: None,
                    })),
                })
                .await;
            let Some(proto::envelope::Payload::QueryTask(query)) = response.payload else {
                panic!("失败任务必须保持可查询");
            };
            state = query.task.unwrap().state;
            if state == proto::TaskState::TaskFailed as i32 {
                break;
            }
            tokio::task::yield_now().await;
        }
        assert_eq!(state, proto::TaskState::TaskFailed as i32);

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
        assert!(
            handle
                .runtime_tasks_for_test()
                .list()
                .await
                .iter()
                .all(|task| { task.runtime_task_id != running_task_id || task.state != "running" }),
            "重启收束后不得遗留运行中的旧任务"
        );

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
