//! 节点业务 actor：网络和托盘只发送命令，SQLite 与 WorkerPool 保持单一所有者。

use std::{
    collections::BTreeMap,
    fs,
    net::SocketAddr,
    path::{Path, PathBuf},
    sync::Arc,
    time::{SystemTime, UNIX_EPOCH},
};

use dedup_core::{
    AnalysisRunId, ContentKey, CoreError, DeleteMode, DiskReadConfig, DisplayPath, EnumeratorKind,
    LocationKey, MachineId, NodeConfig, NormalizedPath, TaskId, Thresholds,
};
use dedup_node_store::{
    AnalysisStatus, ConfirmedDeleteItem, DeleteBatchPlan, DeleteOutcome, FileFaultKind, GroupKind,
    NodeStore, OwnedSnapshot, PlannedDeleteItem, ReviewDecision, StoreError, TaskSnapshot,
    TaskStatus,
};
use dedup_protocol::proto;
use thiserror::Error;
use tokio::{
    sync::{mpsc, oneshot},
    task::JoinHandle,
};
use uuid::Uuid;

use dedup_windows::{
    AppLayout, ReadCancellationToken, machine_id_from_fields, read_physical_machine_fields,
};

use crate::{
    analysis::{
        LocalAnalysisEngine, Stage2BatchItem, Stage2BatchPlan, WorkerPoolStage2Processor,
        begin_stage2_batch, run_stage2_batch,
    },
    artifact_registry::RegenerableArtifactRegistry,
    config_repository::{
        ConfigRepositoryError, LoadedNodeConfig, NodeConfigRepository, ResolvedNodePaths,
    },
    delete::DeleteEngine,
    disk_full_cleanup::{DiskFullCleaner, SystemArtifactDiskResolver},
    host_control::NodeHostControl,
    preview::{PreviewKind, PreviewService},
    runtime_tasks::{RuntimeTaskKind, RuntimeTaskRegistry, RuntimeTaskReporter, RuntimeTaskState},
    scan::{
        PreferredEverythingEnumerator, ScanEngine, ScanOptions, ScheduledFileReader, SystemMd5,
        WindowsWalker, WorkerPoolStage1Processor, begin_scan_task, ensure_everything_ready,
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
}

impl NodeEngineHandle {
    /// 严格执行 WorkerPool prepare、SQLite requeue、WorkerPool restart 三阶段重启。
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

    async fn response_flushed(&self, request_id: u64) -> Result<(), String> {
        let (reply, response) = oneshot::channel();
        self.commands
            .send(EngineCommand::ResponseFlushed(request_id, reply))
            .await
            .map_err(|_| "节点计算引擎已经关闭".to_owned())?;
        response
            .await
            .map_err(|_| "节点计算引擎没有返回刷出确认".to_owned())?
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
    repository_snapshot: Option<LoadedNodeConfig>,
}

impl NodeConfigState {
    /// 构造不携带真实仓库恢复点的测试状态。
    #[doc(hidden)]
    pub fn for_test(config: NodeConfig, version_sha256: impl Into<String>) -> Self {
        Self {
            config,
            version_sha256: version_sha256.into(),
            repository_snapshot: None,
        }
    }

    fn from_loaded(loaded: LoadedNodeConfig) -> Self {
        Self {
            config: loaded.config.clone(),
            version_sha256: loaded.version_sha256.clone(),
            repository_snapshot: Some(loaded),
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

    /// 仅在当前仍为新摘要时恢复保存前冻结的完整状态。
    fn restore_if_version(
        &self,
        expected_new_version_sha256: &str,
        previous: &NodeConfigState,
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

    fn restore_if_version(
        &self,
        expected_new_version_sha256: &str,
        previous: &NodeConfigState,
    ) -> Result<NodeConfigState, ConfigRepositoryError> {
        let snapshot = previous.repository_snapshot.as_ref().ok_or(
            ConfigRepositoryError::InvalidJournal("配置快照缺少仓库恢复数据"),
        )?;
        NodeConfigRepository::restore_if_version(self, expected_new_version_sha256, snapshot)
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
        let paths = ResolvedNodePaths {
            data_path: layout.node_root().to_path_buf(),
            config_path: layout.node_config(),
            log_path: layout.node_logs(),
            cache_path: layout.node_cache(),
        };
        Self::start_inner(layout, config, &paths, identity, None).await
    }

    /// 从仓库解析路径启动运行时，并注入应用入口拥有的替代进程宿主。
    pub async fn start_with_host<I, H>(
        layout: &AppLayout,
        config: &NodeConfig,
        paths: &ResolvedNodePaths,
        identity: &I,
        host_control: H,
    ) -> Result<Self, RuntimeError>
    where
        I: IdentityProvider,
        H: NodeHostControl + 'static,
    {
        Self::start_inner(
            layout,
            config,
            paths,
            identity,
            Some(Box::new(host_control)),
        )
        .await
    }

    async fn start_inner<I>(
        layout: &AppLayout,
        config: &NodeConfig,
        paths: &ResolvedNodePaths,
        identity: &I,
        host_control: Option<Box<dyn NodeHostControl>>,
    ) -> Result<Self, RuntimeError>
    where
        I: IdentityProvider,
    {
        config.validate()?;
        fs::create_dir_all(&paths.data_path)?;
        fs::create_dir_all(&paths.cache_path)?;
        fs::create_dir_all(&paths.log_path)?;
        let machine_id = identity.machine_id()?;
        let mut store = NodeStore::open(&paths.data_path.join("node.db"), machine_id)?;
        store.recover_running_items(now_ms())?;
        let logical_cpu_count = std::thread::available_parallelism()
            .map(usize::from)
            .unwrap_or(1);
        let effective_worker_count = config.worker.effective_worker_count(logical_cpu_count);
        let worker_pool = WorkerPool::start(WorkerPoolConfig::new(
            WorkerLaunch::new(layout.executable_dir().join("worker.exe")),
            effective_worker_count,
        ))
        .await?;
        let listener = tokio::net::TcpListener::bind((config.listen_ip, config.port)).await?;
        let listen_address = listener.local_addr()?;
        let repository = NodeConfigRepository::from_layout(layout);
        let artifact_registry = Arc::new(RegenerableArtifactRegistry::new(
            layout.executable_dir(),
            &paths.cache_path,
        )?);
        let disk_full_cleaner = DiskFullCleaner::new(
            Arc::clone(&artifact_registry),
            SystemArtifactDiskResolver,
        );
        let (handle, actor_task) = spawn_actor(
            store,
            Some(worker_pool),
            listen_address,
            &paths.cache_path,
            config.enumerator,
            config.read.clone(),
            effective_worker_count,
            Some(Box::new(repository)),
            host_control,
            artifact_registry,
            disk_full_cleaner,
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

    /// 返回供托盘与进程入口发送重启/关闭命令的 actor 句柄。
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

    fn response_flushed(
        &self,
        request_id: u64,
    ) -> impl std::future::Future<Output = Result<(), String>> + Send {
        let handle = self.clone();
        async move { handle.response_flushed(request_id).await }
    }
}

/// 创建并运行单一 NodeEngine actor 的工厂。
pub struct NodeEngine;

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
            1,
            None,
            None,
            artifact_registry,
            disk_full_cleaner,
        )
    }

    /// 创建注入配置仓库和可选宿主控制器的测试 actor。
    #[doc(hidden)]
    pub fn spawn_with_remote_config_for_test(
        store: NodeStore,
        listen_address: SocketAddr,
        cache_root: &Path,
        repository: Box<dyn NodeConfigRepositoryAccess>,
        host_control: Option<Box<dyn NodeHostControl>>,
    ) -> (NodeEngineHandle, JoinHandle<()>) {
        let (artifact_registry, disk_full_cleaner) = test_artifact_cleanup(cache_root);
        spawn_actor(
            store,
            None,
            listen_address,
            cache_root,
            EnumeratorKind::WindowsWalker,
            DiskReadConfig::default(),
            1,
            Some(repository),
            host_control,
            artifact_registry,
            disk_full_cleaner,
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
            effective_worker_count,
            None,
            None,
            artifact_registry,
            disk_full_cleaner,
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
    effective_worker_count: usize,
    config_repository: Option<Box<dyn NodeConfigRepositoryAccess>>,
    host_control: Option<Box<dyn NodeHostControl>>,
    artifact_registry: Arc<RegenerableArtifactRegistry>,
    disk_full_cleaner: DiskFullCleaner,
) -> (NodeEngineHandle, JoinHandle<()>) {
    let (commands, receiver) = mpsc::channel(64);
    let handle = NodeEngineHandle {
        commands: commands.clone(),
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
            effective_worker_count,
            restarting: false,
            snapshots: BTreeMap::new(),
            active_job: None,
            commands: commands.downgrade(),
            config_repository,
            host_control,
            pending_restart_request_id: None,
            artifact_registry,
            disk_full_cleaner,
            runtime_tasks: RuntimeTaskRegistry::new(),
        },
        receiver,
    ));
    (handle, actor)
}

fn test_artifact_cleanup(
    cache_root: &Path,
) -> (Arc<RegenerableArtifactRegistry>, DiskFullCleaner) {
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
    listen_address: SocketAddr,
    cache_root: std::path::PathBuf,
    enumerator: EnumeratorKind,
    read_config: DiskReadConfig,
    effective_worker_count: usize,
    restarting: bool,
    snapshots: BTreeMap<String, OwnedSnapshot>,
    active_job: Option<ActiveJob>,
    commands: mpsc::WeakSender<EngineCommand>,
    config_repository: Option<Box<dyn NodeConfigRepositoryAccess>>,
    host_control: Option<Box<dyn NodeHostControl>>,
    pending_restart_request_id: Option<u64>,
    artifact_registry: Arc<RegenerableArtifactRegistry>,
    disk_full_cleaner: DiskFullCleaner,
    runtime_tasks: RuntimeTaskRegistry,
}

enum EngineCommand {
    Protocol(proto::Envelope, oneshot::Sender<proto::Envelope>),
    ConnectionClosed,
    ResponseFlushed(u64, oneshot::Sender<Result<(), String>>),
    Restart(oneshot::Sender<Result<(), String>>),
    Shutdown(oneshot::Sender<()>),
    BackgroundFinished {
        identity: JobIdentity,
        worker_pool: WorkerPool,
    },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum JobIdentity {
    Task(TaskId),
    Analysis(AnalysisRunId),
}

struct ActiveJob {
    identity: JobIdentity,
    abort: tokio::task::AbortHandle,
    cancellation: Option<ReadCancellationToken>,
    runtime_reporter: Option<RuntimeTaskReporter>,
}

enum BackgroundJob {
    Scan {
        task_id: TaskId,
        options: ScanOptions,
        enumerator: EnumeratorKind,
        contact_sheets: PathBuf,
        read_config: DiskReadConfig,
        effective_worker_count: usize,
        cancellation: ReadCancellationToken,
        runtime_reporter: RuntimeTaskReporter,
        artifact_registry: Arc<RegenerableArtifactRegistry>,
        disk_full_cleaner: DiskFullCleaner,
    },
    LocalAnalysis {
        run_id: AnalysisRunId,
    },
    Stage2 {
        plan: Stage2BatchPlan,
    },
}

impl BackgroundJob {
    const fn identity(&self) -> JobIdentity {
        match self {
            Self::Scan { task_id, .. } => JobIdentity::Task(*task_id),
            Self::LocalAnalysis { run_id } => JobIdentity::Analysis(*run_id),
            Self::Stage2 { plan } => JobIdentity::Task(plan.task_id),
        }
    }
}

async fn run_actor(mut state: EngineState, mut commands: mpsc::Receiver<EngineCommand>) {
    while let Some(command) = commands.recv().await {
        match command {
            EngineCommand::Protocol(request, reply) => {
                let _ = reply.send(state.handle_protocol(request).await);
            }
            EngineCommand::ConnectionClosed => state.snapshots.clear(),
            EngineCommand::ResponseFlushed(request_id, reply) => {
                let result = state.response_flushed(request_id);
                let _ = reply.send(result);
            }
            EngineCommand::Restart(reply) => {
                let _ = reply.send(state.restart_engine().await);
            }
            EngineCommand::Shutdown(reply) => {
                state.stop_background_for_shutdown().await;
                let _ = reply.send(());
                break;
            }
            EngineCommand::BackgroundFinished {
                identity,
                worker_pool,
            } => state.finish_background(identity, worker_pool),
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
                self.create_local_analysis(create)
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
                self.dispatch_stage2(dispatch)
            }
            Some(proto::envelope::Payload::PullChanges(pull)) => self.pull_changes(pull),
            Some(proto::envelope::Payload::SyncAck(ack)) => self.sync_ack(ack),
            Some(proto::envelope::Payload::BeginSnapshot(_)) => self.begin_snapshot(),
            Some(proto::envelope::Payload::ReadSnapshotPage(page)) => self.read_snapshot_page(page),
            Some(proto::envelope::Payload::ReadFile(read)) => self.read_file(read),
            Some(proto::envelope::Payload::GetNodeConfig(_)) => self.get_node_config(),
            Some(proto::envelope::Payload::SaveNodeConfigAndRestart(save)) => {
                self.save_node_config_and_restart(request_id, save)
            }
            Some(proto::envelope::Payload::ListFileFaults(query)) => {
                self.list_file_faults(query)
            }
            Some(proto::envelope::Payload::ClearFileFault(clear)) => {
                self.clear_file_fault(clear)
            }
            Some(proto::envelope::Payload::CreateDeleteBatch(create)) => {
                self.create_delete_batch(create)
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

    fn list_file_faults(&self, query: proto::ListFileFaults) -> ProtocolResult {
        let limit = usize::try_from(query.limit).map_err(invalid)?;
        let page = self
            .store
            .page_file_faults((!query.cursor.is_empty()).then_some(query.cursor.as_str()), limit)
            .map_err(store_error)?;
        let faults = page
            .items
            .into_iter()
            .map(|fault| proto::FileFault {
                machine_id: fault.machine_id.as_str().to_owned(),
                normalized_path: fault.normalized_path.as_str().to_owned(),
                display_path: fault.display_path.as_path().to_string_lossy().into_owned(),
                file_size: fault.file_size,
                fault_kind: wire_file_fault_kind(fault.kind) as i32,
                stage: fault.stage,
                error_code: fault.windows_error_code,
                message: fault.message,
            })
            .collect();
        let cleanup_summary = self.disk_full_cleaner.recent_summary().map(|summary| {
            proto::DiskFullCleanupSummary {
                triggered_at_unix_ms: summary.triggered_at_unix_ms,
                deleted_files: summary.deleted_files.try_into().unwrap_or(u64::MAX),
                deleted_bytes: summary.deleted_bytes,
                skipped_active: summary.skipped_active.try_into().unwrap_or(u64::MAX),
                skipped_other_disk: summary
                    .skipped_other_disk
                    .try_into()
                    .unwrap_or(u64::MAX),
                failed_files: summary.failed_files.try_into().unwrap_or(u64::MAX),
            }
        });
        Ok(proto::envelope::Payload::ListFileFaults(
            proto::ListFileFaults {
                cursor: query.cursor,
                limit: query.limit,
                faults,
                next_cursor: page.next_cursor.unwrap_or_default(),
                cleanup_summary,
            },
        ))
    }

    fn clear_file_fault(&mut self, request: proto::ClearFileFault) -> ProtocolResult {
        let machine_id = MachineId::parse(&request.machine_id).map_err(invalid)?;
        let normalized_path = NormalizedPath::new(&request.normalized_path).map_err(invalid)?;
        let wire_kind = proto::FileFaultKind::try_from(request.fault_kind)
            .map_err(|_| invalid("未知文件故障类别"))?;
        let kind = store_file_fault_kind(wire_kind)?;
        let cleared = self
            .store
            .clear_file_fault_kind(&machine_id, &normalized_path, kind)
            .map_err(store_error)?;
        Ok(proto::envelope::Payload::ClearFileFault(
            proto::ClearFileFault {
                machine_id: request.machine_id,
                normalized_path: request.normalized_path,
                fault_kind: request.fault_kind,
                cleared: cleared.try_into().unwrap_or(u32::MAX),
            },
        ))
    }

    fn save_node_config_and_restart(
        &mut self,
        request_id: u64,
        save: proto::SaveNodeConfigAndRestart,
    ) -> ProtocolResult {
        let host_control = self.host_control.as_ref().ok_or_else(|| {
            (
                proto::ErrorCode::Internal,
                "节点宿主尚未支持远程配置重启".to_owned(),
            )
        })?;
        if self.pending_restart_request_id.is_some() {
            return Err((
                proto::ErrorCode::Conflict,
                "节点已有等待响应刷出的重启请求".to_owned(),
            ));
        }
        let repository = self
            .config_repository
            .as_ref()
            .ok_or_else(|| (proto::ErrorCode::Internal, "节点未装配配置仓库".to_owned()))?;
        let config = save
            .config
            .ok_or_else(|| invalid("保存请求缺少 config"))?
            .try_into()
            .map_err(invalid)?;
        let previous = repository.snapshot().map_err(config_repository_error)?;
        let saved = repository
            .save_if_version(&save.expected_version_sha256, &config)
            .map_err(config_repository_error)?;
        if let Err(prepare_error) = host_control.prepare_replacement(&saved.version_sha256) {
            return match repository.restore_if_version(&saved.version_sha256, &previous) {
                Ok(_) => Err(internal(prepare_error)),
                Err(restore_error) => Err((
                    proto::ErrorCode::Internal,
                    format!(
                        "{prepare_error}; 配置回滚失败: {restore_error}; 已保存配置可能仍生效"
                    ),
                )),
            };
        }
        self.pending_restart_request_id = Some(request_id);
        Ok(proto::envelope::Payload::NodeRestartAccepted(
            proto::NodeRestartAccepted {
                machine_id: self.store.machine_id().as_str().to_owned(),
                saved_version_sha256: saved.version_sha256,
            },
        ))
    }

    fn response_flushed(&mut self, request_id: u64) -> Result<(), String> {
        if self.pending_restart_request_id != Some(request_id) {
            return Ok(());
        }
        let host_control = self
            .host_control
            .as_ref()
            .ok_or_else(|| "节点宿主控制器已经不可用".to_owned())?;
        host_control
            .commit_exit_after_response()
            .map_err(|error| error.to_string())?;
        self.pending_restart_request_id = None;
        Ok(())
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
        let runtime_reporter = self
            .runtime_tasks
            .begin(
                RuntimeTaskKind::Scan,
                self.store.machine_id().clone(),
                "扫描",
            )
            .await;
        let cancellation = ReadCancellationToken::new();
        if let Err(error) = self.start_background(BackgroundJob::Scan {
            task_id,
            options,
            enumerator,
            contact_sheets,
            read_config: self.read_config.clone(),
            effective_worker_count: self.effective_worker_count,
            cancellation,
            runtime_reporter,
            artifact_registry: Arc::clone(&self.artifact_registry),
            disk_full_cleaner: self.disk_full_cleaner.clone(),
        }) {
            let _ = self.store.fail_task(task_id, now_ms());
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
            if let Some(reporter) = &active.runtime_reporter {
                let _ = reporter.finish(RuntimeTaskState::Cancelled).await;
            }
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

    fn create_local_analysis(&mut self, request: proto::CreateLocalAnalysis) -> ProtocolResult {
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
        if let Err(error) = self.start_background(BackgroundJob::LocalAnalysis { run_id }) {
            let _ = self
                .store
                .transition_analysis_run(run_id, AnalysisStatus::Partial, now_ms());
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

    fn dispatch_stage2(&mut self, request: proto::DispatchStage2) -> ProtocolResult {
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
        if let Err(error) = self.start_background(BackgroundJob::Stage2 { plan }) {
            let _ = self.store.fail_task(task_id, now_ms());
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

    fn create_delete_batch(&mut self, request: proto::CreateDeleteBatch) -> ProtocolResult {
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
        let results = if external {
            DeleteEngine::execute_external(&mut self.store, &plan)
        } else {
            DeleteEngine::execute_batch(&mut self.store, &plan)
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
        let runtime_reporter = match &job {
            BackgroundJob::Scan { runtime_reporter, .. } => Some(runtime_reporter.clone()),
            _ => None,
        };
        let commands = self
            .commands
            .upgrade()
            .ok_or_else(|| "节点计算引擎已经关闭".to_owned())?;
        let task = tokio::spawn(async move {
            run_background_job(&mut store, &mut worker_pool, job).await;
            let _ = commands
                .send(EngineCommand::BackgroundFinished {
                    identity,
                    worker_pool,
                })
                .await;
        });
        self.active_job = Some(ActiveJob {
            identity,
            abort: task.abort_handle(),
            cancellation,
            runtime_reporter,
        });
        Ok(())
    }

    fn finish_background(&mut self, identity: JobIdentity, worker_pool: WorkerPool) {
        if self
            .active_job
            .as_ref()
            .is_some_and(|active| active.identity == identity)
        {
            self.active_job = None;
        }
        self.worker_pool = Some(worker_pool);
    }

    async fn stop_background_for_shutdown(&mut self) {
        let Some(active) = self.active_job.take() else {
            return;
        };
        match active.identity {
            JobIdentity::Task(task_id) => {
                let _ = self.store.cancel_task(task_id, now_ms());
            }
            JobIdentity::Analysis(run_id) => {
                let _ =
                    self.store
                        .transition_analysis_run(run_id, AnalysisStatus::Cancelled, now_ms());
            }
        }
        active.abort.abort();
    }

    async fn restart_engine(&mut self) -> Result<(), String> {
        if self.active_job.is_some() {
            return Err("节点仍有后台媒体任务，不能重启计算引擎".into());
        }
        self.restarting = true;
        let result = async {
            let pool = self
                .worker_control
                .as_ref()
                .ok_or_else(|| "测试节点没有 WorkerPool".to_owned())?;
            let items = pool
                .prepare_planned_restart()
                .await
                .map_err(|error| error.to_string())?;
            self.store
                .requeue_planned_items(&items, now_ms())
                .map_err(|error| error.to_string())?;
            pool.restart_after_requeue(&items)
                .await
                .map_err(|error| error.to_string())
        }
        .await;
        self.restarting = false;
        result
    }
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
            effective_worker_count,
            cancellation,
            runtime_reporter,
            artifact_registry,
            disk_full_cleaner,
        } => {
            let mut processor = WorkerPoolStage1Processor::new(worker_pool, cancellation.clone())
                .with_runtime_reporter(runtime_reporter.clone());
            let enumerator =
                resolve_scan_enumerator_with(enumerator, ensure_everything_ready).await;
            let result = match ScheduledFileReader::new(&read_config, effective_worker_count) {
                Err(error) => Err(error),
                Ok((reader, limits)) => {
                    let reader = reader.with_runtime_reporter(runtime_reporter.clone());
                    match enumerator {
                    EnumeratorKind::WindowsWalker => {
                        ScanEngine::new(WindowsWalker, SystemMd5, contact_sheets)
                            .with_disk_full_cleanup(
                                Arc::clone(&artifact_registry),
                                disk_full_cleaner.clone(),
                            )
                            .with_runtime_reporter(runtime_reporter.clone())
                            .run_existing_parallel_with(
                                store,
                                task_id,
                                options,
                                reader,
                                &mut processor,
                                limits,
                                cancellation,
                                now_ms(),
                            )
                            .await
                    }
                    EnumeratorKind::Everything => {
                        ScanEngine::new(PreferredEverythingEnumerator, SystemMd5, contact_sheets)
                            .with_disk_full_cleanup(artifact_registry, disk_full_cleaner)
                            .with_runtime_reporter(runtime_reporter.clone())
                            .run_existing_parallel_with(
                                store,
                                task_id,
                                options,
                                reader,
                                &mut processor,
                                limits,
                                cancellation,
                                now_ms(),
                            )
                            .await
                    }
                    }
                }
            };
            let _ = runtime_reporter
                .finish(if result.is_ok() {
                    RuntimeTaskState::Completed
                } else {
                    RuntimeTaskState::Failed
                })
                .await;
            if result.is_err() {
                let _ = store.fail_task(task_id, now_ms());
            }
        }
        BackgroundJob::LocalAnalysis { run_id } => {
            let mut processor = WorkerPoolStage2Processor::new(worker_pool);
            if LocalAnalysisEngine::run_existing(store, run_id, &mut processor, now_ms())
                .await
                .is_err()
            {
                let _ = store.transition_analysis_run(run_id, AnalysisStatus::Partial, now_ms());
            }
        }
        BackgroundJob::Stage2 { plan } => {
            let task_id = plan.task_id;
            let mut processor = WorkerPoolStage2Processor::new(worker_pool);
            if run_stage2_batch(store, plan, &mut processor, now_ms())
                .await
                .is_err()
            {
                let _ = store.fail_task(task_id, now_ms());
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

const fn wire_file_fault_kind(kind: FileFaultKind) -> proto::FileFaultKind {
    match kind {
        FileFaultKind::SuspectedPhysicalRead => proto::FileFaultKind::SuspectedPhysicalRead,
        FileFaultKind::WorkerCrash => proto::FileFaultKind::WorkerCrash,
    }
}

fn store_file_fault_kind(
    kind: proto::FileFaultKind,
) -> Result<FileFaultKind, (proto::ErrorCode, String)> {
    match kind {
        proto::FileFaultKind::SuspectedPhysicalRead => Ok(FileFaultKind::SuspectedPhysicalRead),
        proto::FileFaultKind::WorkerCrash => Ok(FileFaultKind::WorkerCrash),
        proto::FileFaultKind::Unspecified => Err(invalid("文件故障类别不能为空")),
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

    use dedup_media::{ImageStage1, PdqHash};
    use dedup_node_store::{FeatureWrite, ImageStage1Fields, ScannedPath};

    use super::*;

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
            .expect("运行中重启必须返回明确错误，不能排在长任务之后");
        assert!(matches!(restart, Err(EngineError::Operation(_))));

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
