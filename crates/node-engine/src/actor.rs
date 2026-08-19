//! 节点业务 actor：网络和托盘只发送命令，SQLite 与 WorkerPool 保持单一所有者。

use std::{
    collections::BTreeMap,
    fs,
    net::SocketAddr,
    path::{Path, PathBuf},
    time::{SystemTime, UNIX_EPOCH},
};

use dedup_core::{
    AnalysisRunId, ContentKey, CoreError, DeleteMode, DisplayPath, EnumeratorKind, LocationKey,
    MachineId, NodeConfig, TaskId, Thresholds,
};
use dedup_node_store::{
    AnalysisStatus, DeleteBatchPlan, DeleteOutcome, GroupKind, NodeStore, OwnedSnapshot,
    PlannedDeleteItem, ReviewDecision, StoreError, TaskSnapshot, TaskStatus,
};
use dedup_protocol::proto;
use thiserror::Error;
use tokio::{
    sync::{mpsc, oneshot},
    task::JoinHandle,
};
use uuid::Uuid;

use dedup_windows::{AppLayout, machine_id_from_fields, read_physical_machine_fields};

use crate::{
    analysis::{
        LocalAnalysisEngine, Stage2BatchItem, WorkerPoolStage2Processor, dispatch_stage2_batch,
    },
    delete::DeleteEngine,
    preview::{PreviewKind, PreviewService},
    scan::{
        EverythingEnumerator, ScanEngine, ScanOptions, SystemMd5, WindowsWalker,
        WorkerPoolStage1Processor,
    },
    server::{NodeRequestHandler, NodeServer, ServerError},
    worker::{WorkerLaunch, WorkerPool, WorkerPoolConfig, WorkerPoolError},
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
        config.validate()?;
        fs::create_dir_all(layout.node_root())?;
        fs::create_dir_all(layout.node_cache())?;
        fs::create_dir_all(layout.node_logs())?;
        let machine_id = identity.machine_id()?;
        let mut store = NodeStore::open(&layout.node_database(), machine_id)?;
        store.recover_running_items(now_ms())?;
        let worker_pool = WorkerPool::start(WorkerPoolConfig::new(
            WorkerLaunch::new(layout.executable_dir().join("worker.exe")),
            config.worker_count,
        ))
        .await?;
        let listener = tokio::net::TcpListener::bind((config.listen_ip, config.port)).await?;
        let listen_address = listener.local_addr()?;
        let (handle, actor_task) = NodeEngine::spawn(
            store,
            worker_pool,
            listen_address,
            &layout.node_cache(),
            config.enumerator,
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
        spawn_actor(
            store,
            None,
            listen_address,
            cache_root,
            EnumeratorKind::WindowsWalker,
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
        spawn_actor(
            store,
            Some(worker_pool),
            listen_address,
            cache_root,
            enumerator,
        )
    }
}

fn spawn_actor(
    store: NodeStore,
    worker_pool: Option<WorkerPool>,
    listen_address: SocketAddr,
    cache_root: &Path,
    enumerator: EnumeratorKind,
) -> (NodeEngineHandle, JoinHandle<()>) {
    let (commands, receiver) = mpsc::channel(64);
    let handle = NodeEngineHandle { commands };
    let actor = tokio::spawn(run_actor(
        EngineState {
            store,
            worker_pool,
            listen_address,
            cache_root: cache_root.to_path_buf(),
            enumerator,
            restarting: false,
            snapshots: BTreeMap::new(),
        },
        receiver,
    ));
    (handle, actor)
}

struct EngineState {
    store: NodeStore,
    worker_pool: Option<WorkerPool>,
    listen_address: SocketAddr,
    cache_root: std::path::PathBuf,
    enumerator: EnumeratorKind,
    restarting: bool,
    snapshots: BTreeMap<String, OwnedSnapshot>,
}

enum EngineCommand {
    Protocol(proto::Envelope, oneshot::Sender<proto::Envelope>),
    ConnectionClosed,
    Restart(oneshot::Sender<Result<(), String>>),
    Shutdown(oneshot::Sender<()>),
}

async fn run_actor(mut state: EngineState, mut commands: mpsc::Receiver<EngineCommand>) {
    while let Some(command) = commands.recv().await {
        match command {
            EngineCommand::Protocol(request, reply) => {
                let _ = reply.send(state.handle_protocol(request).await);
            }
            EngineCommand::ConnectionClosed => state.snapshots.clear(),
            EngineCommand::Restart(reply) => {
                let _ = reply.send(state.restart_engine().await);
            }
            EngineCommand::Shutdown(reply) => {
                let _ = reply.send(());
                break;
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

    fn node_status(&self) -> ProtocolResult {
        let (queued_items, running_items) =
            self.store.task_activity_counts().map_err(store_error)?;
        let worker_count = self
            .worker_pool
            .as_ref()
            .map_or(0, |pool| pool.worker_process_ids().len());
        let busy_workers = self
            .worker_pool
            .as_ref()
            .map_or(0, WorkerPool::busy_workers);
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
        let pool = self
            .worker_pool
            .as_mut()
            .ok_or_else(|| invalid("测试节点没有 WorkerPool"))?;
        let mut processor = WorkerPoolStage1Processor::new(pool);
        let contact_sheets = self.cache_root.join("contact-sheets");
        let summary = match enumerator {
            EnumeratorKind::WindowsWalker => {
                ScanEngine::new(WindowsWalker, SystemMd5, contact_sheets)
                    .run(&mut self.store, options, &mut processor, now_ms())
                    .await
            }
            EnumeratorKind::Everything => {
                ScanEngine::new(EverythingEnumerator, SystemMd5, contact_sheets)
                    .run(&mut self.store, options, &mut processor, now_ms())
                    .await
            }
        }
        .map_err(internal)?;
        Ok(proto::envelope::Payload::TaskAccepted(
            proto::TaskAccepted {
                task_id: summary.task_id.as_uuid().to_string(),
            },
        ))
    }

    async fn cancel_task(&mut self, request: proto::CancelTask) -> ProtocolResult {
        let task_id = parse_task_id(&request.task_id)?;
        if let Some(pool) = &self.worker_pool {
            pool.cancel_task(&request.task_id).await.map_err(internal)?;
        }
        self.store
            .cancel_task(task_id, now_ms())
            .map_err(store_error)?;
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

    async fn create_local_analysis(
        &mut self,
        request: proto::CreateLocalAnalysis,
    ) -> ProtocolResult {
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
        let pool = self
            .worker_pool
            .as_mut()
            .ok_or_else(|| invalid("测试节点没有 WorkerPool"))?;
        let mut processor = WorkerPoolStage2Processor::new(pool);
        let report = LocalAnalysisEngine::start(
            &mut self.store,
            &tasks,
            thresholds,
            &mut processor,
            now_ms(),
        )
        .await
        .map_err(internal)?;
        Ok(proto::envelope::Payload::QueryAnalysisRun(
            proto::QueryAnalysisRun {
                analysis_run_id: report.run_id.as_uuid().to_string(),
                state: analysis_status_name(report.status).into(),
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
                    active: true,
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
        let pool = self
            .worker_pool
            .as_mut()
            .ok_or_else(|| invalid("测试节点没有 WorkerPool"))?;
        let mut processor = WorkerPoolStage2Processor::new(pool);
        let task_id = dispatch_stage2_batch(&mut self.store, &items, &mut processor, now_ms())
            .await
            .map_err(internal)?;
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
        let external = !request.items.is_empty();
        let plan = if external {
            if request.delete_batch_id.is_empty() {
                return Err(invalid("中心删除批次缺少 ID"));
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
        } else {
            self.store
                .create_delete_batch(
                    parse_analysis_id(&request.analysis_run_id)?,
                    &request.group_ids,
                    mode,
                    now_ms(),
                )
                .map_err(store_error)?
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

    async fn restart_engine(&mut self) -> Result<(), String> {
        self.restarting = true;
        let result = async {
            let pool = self
                .worker_pool
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

type ProtocolResult = Result<proto::envelope::Payload, (proto::ErrorCode, String)>;

fn store_error(error: StoreError) -> (proto::ErrorCode, String) {
    let code = if matches!(error, StoreError::SnapshotRequired { .. }) {
        proto::ErrorCode::SnapshotRequired
    } else {
        proto::ErrorCode::Internal
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

fn error_response(request_id: u64, code: proto::ErrorCode, message: &str) -> proto::Envelope {
    proto::Envelope {
        request_id,
        payload: Some(proto::envelope::Payload::Error(proto::Error {
            code: code as i32,
            message: message.to_owned(),
        })),
    }
}
