//! 多 Worker 进程的串行 actor、异常替换、取消与两阶段计划重启。

use std::{
    collections::{BTreeMap, BTreeSet, HashMap, HashSet, VecDeque},
    sync::{Arc, Mutex},
    time::Duration,
};

use dedup_protocol::proto::{self, worker_envelope};
use dedup_windows::{ReadCancellationToken, WorkerJob};
use thiserror::Error;
use tokio::sync::{mpsc, oneshot};

use super::process::{WorkerLaunch, WorkerProcess};

const DEFAULT_READY_TIMEOUT: Duration = Duration::from_secs(15);

/// WorkerPool 的进程数量、可执行文件和 Ready 超时。
#[derive(Clone, Debug)]
pub struct WorkerPoolConfig {
    launch: WorkerLaunch,
    worker_count: usize,
    ready_timeout: Duration,
    result_read_delay: Duration,
}

impl WorkerPoolConfig {
    /// 使用固定 15 秒 Ready 超时创建池配置。
    pub const fn new(launch: WorkerLaunch, worker_count: usize) -> Self {
        Self {
            launch,
            worker_count,
            ready_timeout: DEFAULT_READY_TIMEOUT,
            result_read_delay: Duration::ZERO,
        }
    }

    /// 覆盖进程启动的 Ready 超时，主要用于进程级测试。
    pub const fn with_ready_timeout(mut self, timeout: Duration) -> Self {
        self.ready_timeout = timeout;
        self
    }

    /// 在发送请求后延迟读取响应，用于稳定复现运行中重启/崩溃/取消的进程测试。
    /// 生产配置保持默认零延迟。
    pub const fn with_result_read_delay(mut self, delay: Duration) -> Self {
        self.result_read_delay = delay;
        self
    }
}

/// WorkerPool 交给 NodeEngine 的持久化动作或任务结果。
#[derive(Clone, Debug)]
pub enum WorkerEvent {
    /// Worker 正常返回一个协议结果。
    Completed {
        /// 所属任务 ID。
        task_id: String,
        /// 任务项 ID。
        item_id: String,
        /// Stage1/Stage2/ContactSheet/Failure 响应。
        response: proto::WorkerEnvelope,
    },
    /// Worker 意外退出；NodeEngine 应把当前项记为失败。
    Crashed {
        /// 所属任务 ID。
        task_id: String,
        /// 任务项 ID。
        item_id: String,
        /// 进程或管道诊断。
        message: String,
    },
    /// 用户取消的等待项或运行项。
    Cancelled {
        /// 所属任务 ID。
        task_id: String,
        /// 任务项 ID。
        item_id: String,
    },
    /// Worker 补建失败，池容量暂时低于配置值。
    InfrastructureFailure {
        /// 启动失败诊断。
        message: String,
    },
}

/// 拥有 Worker 进程 actor 的客户端句柄。
pub struct WorkerPool {
    commands: mpsc::Sender<PoolCommand>,
    events: mpsc::Receiver<WorkerEvent>,
    state: Arc<Mutex<PoolState>>,
}

/// 可克隆的 WorkerPool 控制面；唯一事件接收器仍由一个计算 owner 持有。
#[derive(Clone)]
pub struct WorkerPoolHandle {
    commands: mpsc::Sender<PoolCommand>,
    state: Arc<Mutex<PoolState>>,
}

impl WorkerPoolHandle {
    /// 在持久取消提交后同步标记任务，使 slot send 临界区拒绝后续请求。
    pub fn mark_task_cancelled(&self, task_id: &str) {
        self.state
            .lock()
            .unwrap()
            .cancelled_tasks
            .insert(task_id.to_owned());
    }

    /// 取消任务：删除等待项，终止并替换正在执行该任务的 Worker。
    pub async fn cancel_task(&self, task_id: &str) -> Result<(), WorkerPoolError> {
        let (reply_tx, reply_rx) = oneshot::channel();
        self.commands
            .send(PoolCommand::Cancel(task_id.to_owned(), reply_tx))
            .await
            .map_err(|_| WorkerPoolError::Closed)?;
        reply_rx.await.map_err(|_| WorkerPoolError::Closed)?
    }

    /// 第一阶段冻结调度并返回所有运行项；此调用不终止 Worker。
    pub async fn prepare_planned_restart(&self) -> Result<Vec<String>, WorkerPoolError> {
        let (reply_tx, reply_rx) = oneshot::channel();
        self.commands
            .send(PoolCommand::PrepareRestart(reply_tx))
            .await
            .map_err(|_| WorkerPoolError::Closed)?;
        reply_rx.await.map_err(|_| WorkerPoolError::Closed)?
    }

    /// 调用方写回 SQLite queued 后，执行 Worker 终止、补建和 Ready 等待。
    pub async fn restart_after_requeue(
        &self,
        requeued_item_ids: &[String],
    ) -> Result<(), WorkerPoolError> {
        let (reply_tx, reply_rx) = oneshot::channel();
        self.commands
            .send(PoolCommand::Restart(requeued_item_ids.to_vec(), reply_tx))
            .await
            .map_err(|_| WorkerPoolError::Closed)?;
        reply_rx.await.map_err(|_| WorkerPoolError::Closed)?
    }

    /// 返回按槽位排序的当前 Worker PID。
    pub fn worker_process_ids(&self) -> Vec<u32> {
        self.state
            .lock()
            .unwrap()
            .process_ids
            .values()
            .copied()
            .collect()
    }

    /// 返回已经派发且尚未收回结果的 Worker 数量。
    pub fn busy_workers(&self) -> usize {
        self.state.lock().unwrap().running.len()
    }
}

impl WorkerPool {
    /// 创建 Job Object，启动全部 Worker，并等待每个进程发出 Ready 后返回。
    pub async fn start(config: WorkerPoolConfig) -> Result<Self, WorkerPoolError> {
        if config.worker_count == 0 {
            return Err(WorkerPoolError::EmptyPool);
        }
        let job = WorkerJob::create().map_err(|error| WorkerPoolError::Job(error.to_string()))?;
        let state = Arc::new(Mutex::new(PoolState::default()));
        let (slot_events_tx, slot_events_rx) = mpsc::unbounded_channel();
        let mut slots = BTreeMap::new();
        let mut idle = BTreeSet::new();
        for slot_id in 0..config.worker_count {
            let slot = spawn_slot(slot_id, &config, &job, slot_events_tx.clone()).await?;
            state
                .lock()
                .unwrap()
                .process_ids
                .insert(slot_id, slot.process_id);
            idle.insert(slot_id);
            slots.insert(slot_id, slot);
        }

        let (commands_tx, commands_rx) = mpsc::channel(64);
        let (events_tx, events_rx) = mpsc::channel(256);
        let actor_state = Arc::clone(&state);
        tokio::spawn(run_pool(
            config,
            job,
            slots,
            idle,
            commands_rx,
            slot_events_rx,
            slot_events_tx,
            events_tx,
            actor_state,
        ));
        Ok(Self {
            commands: commands_tx,
            events: events_rx,
            state,
        })
    }

    /// 克隆只发送命令并读取快照的控制面，不转移事件接收所有权。
    pub fn handle(&self) -> WorkerPoolHandle {
        WorkerPoolHandle {
            commands: self.commands.clone(),
            state: Arc::clone(&self.state),
        }
    }

    /// 把一个三类 Worker 请求排入池；响应由 `next_event` 返回。
    pub async fn dispatch(&self, envelope: proto::WorkerEnvelope) -> Result<(), WorkerPoolError> {
        let (reply_tx, reply_rx) = oneshot::channel();
        self.commands
            .send(PoolCommand::Dispatch(envelope, None, reply_tx))
            .await
            .map_err(|_| WorkerPoolError::Closed)?;
        reply_rx.await.map_err(|_| WorkerPoolError::Closed)?
    }

    /// 扫描请求在实际槽位发送前同时检查持久门禁结果和取消标记。
    pub async fn dispatch_scan(
        &self,
        envelope: proto::WorkerEnvelope,
        cancellation: ReadCancellationToken,
        persisted_active: bool,
    ) -> Result<(), WorkerPoolError> {
        let (reply_tx, reply_rx) = oneshot::channel();
        self.commands
            .send(PoolCommand::Dispatch(
                envelope,
                Some(ScanDispatchGuard {
                    cancellation,
                    persisted_active,
                }),
                reply_tx,
            ))
            .await
            .map_err(|_| WorkerPoolError::Closed)?;
        reply_rx.await.map_err(|_| WorkerPoolError::Closed)?
    }

    /// 取消任务：删除等待项，终止并替换正在执行该任务的 Worker。
    pub async fn cancel_task(&self, task_id: &str) -> Result<(), WorkerPoolError> {
        let (reply_tx, reply_rx) = oneshot::channel();
        self.commands
            .send(PoolCommand::Cancel(task_id.to_owned(), reply_tx))
            .await
            .map_err(|_| WorkerPoolError::Closed)?;
        reply_rx.await.map_err(|_| WorkerPoolError::Closed)?
    }

    /// 第一阶段冻结调度并返回所有运行项；此调用不终止 Worker。
    pub async fn prepare_planned_restart(&self) -> Result<Vec<String>, WorkerPoolError> {
        let (reply_tx, reply_rx) = oneshot::channel();
        self.commands
            .send(PoolCommand::PrepareRestart(reply_tx))
            .await
            .map_err(|_| WorkerPoolError::Closed)?;
        reply_rx.await.map_err(|_| WorkerPoolError::Closed)?
    }

    /// 调用方把第一阶段返回项写回 SQLite queued 后，执行终止、补建和 Ready 等待。
    pub async fn restart_after_requeue(
        &self,
        requeued_item_ids: &[String],
    ) -> Result<(), WorkerPoolError> {
        let (reply_tx, reply_rx) = oneshot::channel();
        self.commands
            .send(PoolCommand::Restart(requeued_item_ids.to_vec(), reply_tx))
            .await
            .map_err(|_| WorkerPoolError::Closed)?;
        reply_rx.await.map_err(|_| WorkerPoolError::Closed)?
    }

    /// 等待下一条需由 NodeEngine 持久化的结果或进程事件。
    pub async fn next_event(&mut self) -> Option<WorkerEvent> {
        self.events.recv().await
    }

    /// 返回按槽位排序的当前 Worker PID，供状态页和进程级测试使用。
    pub fn worker_process_ids(&self) -> Vec<u32> {
        self.state
            .lock()
            .unwrap()
            .process_ids
            .values()
            .copied()
            .collect()
    }

    /// 返回自池启动后的意外进程退出次数；计划重启和取消不计入。
    pub fn failure_count(&self) -> u64 {
        self.state.lock().unwrap().failure_count
    }

    /// 返回已经派发且尚未收回结果的 Worker 数量。
    pub fn busy_workers(&self) -> usize {
        self.state.lock().unwrap().running.len()
    }

    /// 强制终止指定 Worker 并让池按“意外退出”路径补建，仅供进程级故障测试。
    #[doc(hidden)]
    pub async fn terminate_worker_for_test(&self, process_id: u32) -> Result<(), WorkerPoolError> {
        let (reply_tx, reply_rx) = oneshot::channel();
        self.commands
            .send(PoolCommand::TerminateUnexpected(process_id, reply_tx))
            .await
            .map_err(|_| WorkerPoolError::Closed)?;
        reply_rx.await.map_err(|_| WorkerPoolError::Closed)?
    }

    /// 创建不启动进程的可控池，只供直接集成测试验证发送/取消边界。
    #[doc(hidden)]
    pub fn controlled_for_test() -> (Self, mpsc::Receiver<(String, String)>) {
        let state = Arc::new(Mutex::new(PoolState::default()));
        let actor_state = Arc::clone(&state);
        let (commands, mut command_rx) = mpsc::channel(64);
        let (events, event_rx) = mpsc::channel(256);
        let (started, started_rx) = mpsc::channel(8);
        tokio::spawn(async move {
            let mut active = None::<WorkIdentity>;
            while let Some(command) = command_rx.recv().await {
                match command {
                    PoolCommand::Dispatch(envelope, guard, reply) => {
                        let result = WorkItem::try_from(envelope).map(|mut work| {
                            work.scan_guard = guard;
                            let registry_cancelled = actor_state
                                .lock()
                                .unwrap()
                                .cancelled_tasks
                                .contains(&work.identity.task_id);
                            if registry_cancelled || !work.dispatch_allowed() {
                                let _ = events.try_send(WorkerEvent::Cancelled {
                                    task_id: work.identity.task_id,
                                    item_id: work.identity.item_id,
                                });
                                return;
                            }
                            active = Some(work.identity.clone());
                            actor_state
                                .lock()
                                .unwrap()
                                .running
                                .insert(0, work.identity.clone());
                            let _ = started.try_send((
                                work.identity.task_id.clone(),
                                work.identity.item_id.clone(),
                            ));
                        });
                        let _ = reply.send(result);
                    }
                    PoolCommand::Cancel(task_id, reply) => {
                        actor_state
                            .lock()
                            .unwrap()
                            .cancelled_tasks
                            .insert(task_id.clone());
                        if active.as_ref().is_some_and(|work| work.task_id == task_id) {
                            let work = active.take().expect("活动测试项已经确认存在");
                            actor_state.lock().unwrap().running.clear();
                            let _ = events
                                .send(WorkerEvent::Cancelled {
                                    task_id: work.task_id,
                                    item_id: work.item_id,
                                })
                                .await;
                        }
                        let _ = reply.send(Ok(()));
                    }
                    PoolCommand::PrepareRestart(reply) => {
                        let items = active
                            .iter()
                            .map(|work| work.item_id.clone())
                            .collect::<Vec<_>>();
                        let _ = reply.send(Ok(items));
                    }
                    PoolCommand::Restart(_, reply) => {
                        let _ = reply.send(Ok(()));
                    }
                    PoolCommand::TerminateUnexpected(_, reply) => {
                        let _ = reply.send(Ok(()));
                    }
                }
            }
        });
        (
            Self {
                commands,
                events: event_rx,
                state,
            },
            started_rx,
        )
    }
}

/// Worker 池启动、请求或两阶段重启错误。
#[derive(Debug, Error)]
pub enum WorkerPoolError {
    /// 配置至少需要一个 Worker。
    #[error("Worker 数量必须大于零")]
    EmptyPool,
    /// Worker 进程创建或 Ready 握手失败。
    #[error("Worker 进程失败: {0}")]
    Process(String),
    /// Job Object 创建失败。
    #[error("创建 Worker Job Object 失败: {0}")]
    Job(String),
    /// actor 已结束。
    #[error("WorkerPool 已关闭")]
    Closed,
    /// 调度已被计划重启冻结。
    #[error("WorkerPool 正在计划重启")]
    Restarting,
    /// Envelope 不是三类 Worker 请求之一。
    #[error("不是有效的 Worker 请求")]
    InvalidRequest,
    /// 第二阶段提交的重新排队项与第一阶段快照不一致。
    #[error("重新排队项与计划重启快照不一致")]
    RequeueMismatch,
    /// 诊断请求指定的 PID 不属于当前池。
    #[error("Worker PID 不存在: {0}")]
    WorkerNotFound(u32),
}

#[derive(Default)]
/// actor 与只读状态 API 共享的最小运行快照；所有写入仍只发生在 actor 中。
struct PoolState {
    running: HashMap<usize, WorkIdentity>,
    process_ids: BTreeMap<usize, u32>,
    restart_items: HashSet<String>,
    failure_count: u64,
    restarting: bool,
    cancelled_tasks: HashSet<String>,
}

#[derive(Clone, Debug)]
/// 不携带路径或特征数据的任务归属，用于崩溃、取消和重启持久化。
struct WorkIdentity {
    task_id: String,
    item_id: String,
}

/// 等待调度的一条完整协议请求及其任务归属。
struct WorkItem {
    identity: WorkIdentity,
    envelope: proto::WorkerEnvelope,
    scan_guard: Option<ScanDispatchGuard>,
}

struct ScanDispatchGuard {
    cancellation: ReadCancellationToken,
    persisted_active: bool,
}

impl WorkItem {
    fn dispatch_allowed(&self) -> bool {
        self.scan_guard
            .as_ref()
            .is_none_or(|guard| guard.persisted_active && !guard.cancellation.is_cancelled())
    }
}

/// 客户端 API 向单 actor 发送的控制命令。
enum PoolCommand {
    Dispatch(
        proto::WorkerEnvelope,
        Option<ScanDispatchGuard>,
        oneshot::Sender<Result<(), WorkerPoolError>>,
    ),
    Cancel(String, oneshot::Sender<Result<(), WorkerPoolError>>),
    PrepareRestart(oneshot::Sender<Result<Vec<String>, WorkerPoolError>>),
    Restart(Vec<String>, oneshot::Sender<Result<(), WorkerPoolError>>),
    TerminateUnexpected(u32, oneshot::Sender<Result<(), WorkerPoolError>>),
}

/// 一个已经 Ready 的槽位，只暴露 PID 与单向控制通道。
struct SlotHandle {
    process_id: u32,
    commands: mpsc::UnboundedSender<SlotCommand>,
}

/// actor 发给单个 Worker 驱动任务的命令。
enum SlotCommand {
    Run(WorkItem),
    Terminate,
}

/// Worker 驱动回报给池 actor 的正常响应或进程结束。
enum SlotEvent {
    Response {
        slot_id: usize,
        work: WorkIdentity,
        response: proto::WorkerEnvelope,
    },
    Exited {
        slot_id: usize,
        work: Option<WorkIdentity>,
        message: String,
    },
}

#[allow(clippy::too_many_arguments)]
/// 独占队列、槽位和 Job 的池 actor；外部方法只通过消息读取或改变状态。
async fn run_pool(
    config: WorkerPoolConfig,
    job: WorkerJob,
    mut slots: BTreeMap<usize, SlotHandle>,
    mut idle: BTreeSet<usize>,
    mut commands: mpsc::Receiver<PoolCommand>,
    mut slot_events: mpsc::UnboundedReceiver<SlotEvent>,
    slot_events_tx: mpsc::UnboundedSender<SlotEvent>,
    events: mpsc::Sender<WorkerEvent>,
    state: Arc<Mutex<PoolState>>,
) {
    let mut queue = VecDeque::new();
    loop {
        tokio::select! {
            command = commands.recv() => {
                let Some(command) = command else { break };
                match command {
                    PoolCommand::Dispatch(envelope, guard, reply) => {
                        let result = if state.lock().unwrap().restarting {
                            Err(WorkerPoolError::Restarting)
                        } else {
                            match WorkItem::try_from(envelope) {
                                Ok(mut work) => {
                                    work.scan_guard = guard;
                                    queue.push_back(work);
                                    schedule(&mut queue, &mut idle, &slots, &events, &state).await;
                                    Ok(())
                                }
                                Err(error) => Err(error),
                            }
                        };
                        let _ = reply.send(result);
                    }
                    PoolCommand::PrepareRestart(reply) => {
                        let mut locked = state.lock().unwrap();
                        if locked.restarting {
                            let _ = reply.send(Err(WorkerPoolError::Restarting));
                            continue;
                        }
                        locked.restarting = true;
                        let mut items: Vec<_> = locked
                            .running
                            .values()
                            .map(|work| work.item_id.clone())
                            .collect();
                        items.sort();
                        locked.restart_items = items.iter().cloned().collect();
                        let _ = reply.send(Ok(items));
                    }
                    PoolCommand::Restart(items, reply) => {
                        let expected: HashSet<_> = items.into_iter().collect();
                        if expected != state.lock().unwrap().restart_items {
                            let _ = reply.send(Err(WorkerPoolError::RequeueMismatch));
                            continue;
                        }
                        let result = restart_all(
                            &config,
                            &job,
                            &mut slots,
                            &mut idle,
                            &mut slot_events,
                            &slot_events_tx,
                            &state,
                        ).await;
                        if result.is_ok() {
                            {
                                let mut locked = state.lock().unwrap();
                                locked.restarting = false;
                                locked.restart_items.clear();
                            }
                            schedule(&mut queue, &mut idle, &slots, &events, &state).await;
                        }
                        let _ = reply.send(result);
                    }
                    PoolCommand::Cancel(task_id, reply) => {
                        state
                            .lock()
                            .unwrap()
                            .cancelled_tasks
                            .insert(task_id.clone());
                        let result = cancel_task_items(
                            &task_id,
                            &config,
                            &job,
                            &mut queue,
                            &mut slots,
                            &mut idle,
                            &mut slot_events,
                            &slot_events_tx,
                            &events,
                            &state,
                        ).await;
                        schedule(&mut queue, &mut idle, &slots, &events, &state).await;
                        let _ = reply.send(result);
                    }
                    PoolCommand::TerminateUnexpected(process_id, reply) => {
                        let result = slots
                            .values()
                            .find(|slot| slot.process_id == process_id)
                            .ok_or(WorkerPoolError::WorkerNotFound(process_id))
                            .and_then(|slot| {
                                slot.commands
                                    .send(SlotCommand::Terminate)
                                    .map_err(|_| WorkerPoolError::Closed)
                            });
                        let _ = reply.send(result);
                    }
                }
            }
            event = slot_events.recv() => {
                let Some(event) = event else { break };
                handle_slot_event(
                    event,
                    &config,
                    &job,
                    &mut slots,
                    &mut idle,
                    &slot_events_tx,
                    &events,
                    &state,
                ).await;
                schedule(&mut queue, &mut idle, &slots, &events, &state).await;
            }
        }
    }
}

/// 按最小槽位号把等待项发送给空闲 Worker，并原子更新共享运行快照。
async fn schedule(
    queue: &mut VecDeque<WorkItem>,
    idle: &mut BTreeSet<usize>,
    slots: &BTreeMap<usize, SlotHandle>,
    events: &mpsc::Sender<WorkerEvent>,
    state: &Arc<Mutex<PoolState>>,
) {
    if state.lock().unwrap().restarting {
        return;
    }
    while !idle.is_empty() && !queue.is_empty() {
        let work = queue.pop_front().expect("队列已判定非空");
        let registry_cancelled = state
            .lock()
            .unwrap()
            .cancelled_tasks
            .contains(&work.identity.task_id);
        if registry_cancelled || !work.dispatch_allowed() {
            let _ = events
                .send(WorkerEvent::Cancelled {
                    task_id: work.identity.task_id,
                    item_id: work.identity.item_id,
                })
                .await;
            continue;
        }
        let slot_id = idle.pop_first().expect("集合已判定非空");
        let identity = work.identity.clone();
        let Some(slot) = slots.get(&slot_id) else {
            queue.push_front(work);
            continue;
        };
        if let Err(error) = slot.commands.send(SlotCommand::Run(work)) {
            let SlotCommand::Run(work) = error.0 else {
                unreachable!("schedule only sends Run")
            };
            queue.push_front(work);
            continue;
        }
        state.lock().unwrap().running.insert(slot_id, identity);
    }
}

/// 启动一个 Worker、完成 Ready 握手，再创建独占该进程的异步驱动任务。
async fn spawn_slot(
    slot_id: usize,
    config: &WorkerPoolConfig,
    job: &WorkerJob,
    events: mpsc::UnboundedSender<SlotEvent>,
) -> Result<SlotHandle, WorkerPoolError> {
    let process = WorkerProcess::spawn(&config.launch, job, config.ready_timeout)
        .await
        .map_err(|error| WorkerPoolError::Process(error.to_string()))?;
    let process_id = process.process_id();
    let (commands_tx, commands_rx) = mpsc::unbounded_channel();
    tokio::spawn(run_slot(
        slot_id,
        process,
        commands_rx,
        events,
        config.result_read_delay,
    ));
    Ok(SlotHandle {
        process_id,
        commands: commands_tx,
    })
}

/// 串行驱动一个 Worker；运行中可被取消或重启命令抢占并终止。
async fn run_slot(
    slot_id: usize,
    mut process: WorkerProcess,
    mut commands: mpsc::UnboundedReceiver<SlotCommand>,
    events: mpsc::UnboundedSender<SlotEvent>,
    result_read_delay: Duration,
) {
    while let Some(command) = commands.recv().await {
        match command {
            SlotCommand::Terminate => {
                let message = process
                    .terminate()
                    .await
                    .err()
                    .map_or_else(|| "planned termination".into(), |error| error.to_string());
                let _ = events.send(SlotEvent::Exited {
                    slot_id,
                    work: None,
                    message,
                });
                return;
            }
            SlotCommand::Run(work) => {
                if let Err(error) = process.send(&work.envelope).await {
                    let _ = events.send(SlotEvent::Exited {
                        slot_id,
                        work: Some(work.identity),
                        message: error.to_string(),
                    });
                    return;
                }
                if !result_read_delay.is_zero() {
                    tokio::select! {
                        _ = tokio::time::sleep(result_read_delay) => {}
                        command = commands.recv() => {
                            if matches!(command, Some(SlotCommand::Terminate)) {
                                let message = process
                                    .terminate()
                                    .await
                                    .err()
                                    .map_or_else(|| "planned termination".into(), |error| error.to_string());
                                let _ = events.send(SlotEvent::Exited {
                                    slot_id,
                                    work: Some(work.identity.clone()),
                                    message,
                                });
                            }
                            return;
                        }
                    }
                }
                tokio::select! {
                    response = process.receive() => {
                        match response {
                            Ok(response) => {
                                let _ = events.send(SlotEvent::Response {
                                    slot_id,
                                    work: work.identity,
                                    response,
                                });
                            }
                            Err(error) => {
                                let _ = events.send(SlotEvent::Exited {
                                    slot_id,
                                    work: Some(work.identity),
                                    message: error.to_string(),
                                });
                                return;
                            }
                        }
                    }
                    command = commands.recv() => {
                        if matches!(command, Some(SlotCommand::Terminate)) {
                            let message = process
                                .terminate()
                                .await
                                .err()
                                .map_or_else(|| "planned termination".into(), |error| error.to_string());
                            let _ = events.send(SlotEvent::Exited {
                                slot_id,
                                work: Some(work.identity),
                                message,
                            });
                        }
                        return;
                    }
                }
            }
        }
    }
}

#[allow(clippy::too_many_arguments)]
/// 处理正常响应或非计划退出；后者增加失败计数并补建同槽位 Worker。
async fn handle_slot_event(
    event: SlotEvent,
    config: &WorkerPoolConfig,
    job: &WorkerJob,
    slots: &mut BTreeMap<usize, SlotHandle>,
    idle: &mut BTreeSet<usize>,
    slot_events: &mpsc::UnboundedSender<SlotEvent>,
    events: &mpsc::Sender<WorkerEvent>,
    state: &Arc<Mutex<PoolState>>,
) {
    match event {
        SlotEvent::Response {
            slot_id,
            work,
            response,
        } => {
            state.lock().unwrap().running.remove(&slot_id);
            idle.insert(slot_id);
            if !state.lock().unwrap().restart_items.contains(&work.item_id) {
                let _ = events
                    .send(WorkerEvent::Completed {
                        task_id: work.task_id,
                        item_id: work.item_id,
                        response,
                    })
                    .await;
            }
        }
        SlotEvent::Exited {
            slot_id,
            work,
            message,
        } => {
            slots.remove(&slot_id);
            idle.remove(&slot_id);
            let work = work.or_else(|| state.lock().unwrap().running.get(&slot_id).cloned());
            {
                let mut locked = state.lock().unwrap();
                locked.running.remove(&slot_id);
                locked.process_ids.remove(&slot_id);
                locked.failure_count += 1;
            }
            if let Some(work) = work {
                let _ = events
                    .send(WorkerEvent::Crashed {
                        task_id: work.task_id,
                        item_id: work.item_id,
                        message,
                    })
                    .await;
            }
            replace_slot(
                slot_id,
                config,
                job,
                slots,
                idle,
                slot_events,
                events,
                state,
            )
            .await;
        }
    }
}

#[allow(clippy::too_many_arguments)]
/// 为已退出槽位启动替代 Worker；失败只报告基础设施事件，不伪造任务结果。
async fn replace_slot(
    slot_id: usize,
    config: &WorkerPoolConfig,
    job: &WorkerJob,
    slots: &mut BTreeMap<usize, SlotHandle>,
    idle: &mut BTreeSet<usize>,
    slot_events: &mpsc::UnboundedSender<SlotEvent>,
    events: &mpsc::Sender<WorkerEvent>,
    state: &Arc<Mutex<PoolState>>,
) {
    match spawn_slot(slot_id, config, job, slot_events.clone()).await {
        Ok(slot) => {
            state
                .lock()
                .unwrap()
                .process_ids
                .insert(slot_id, slot.process_id);
            idle.insert(slot_id);
            slots.insert(slot_id, slot);
        }
        Err(error) => {
            let _ = events
                .send(WorkerEvent::InfrastructureFailure {
                    message: error.to_string(),
                })
                .await;
        }
    }
}

/// 计划重启第二阶段：终止全部旧进程、等待退出、补建并等待 Ready。
async fn restart_all(
    config: &WorkerPoolConfig,
    job: &WorkerJob,
    slots: &mut BTreeMap<usize, SlotHandle>,
    idle: &mut BTreeSet<usize>,
    slot_events: &mut mpsc::UnboundedReceiver<SlotEvent>,
    slot_events_tx: &mpsc::UnboundedSender<SlotEvent>,
    state: &Arc<Mutex<PoolState>>,
) -> Result<(), WorkerPoolError> {
    let mut expected: HashSet<usize> = slots.keys().copied().collect();
    for slot in slots.values() {
        let _ = slot.commands.send(SlotCommand::Terminate);
    }
    while !expected.is_empty() {
        match slot_events.recv().await {
            Some(SlotEvent::Exited { slot_id, .. }) => {
                expected.remove(&slot_id);
            }
            Some(SlotEvent::Response { .. }) => {}
            None => return Err(WorkerPoolError::Closed),
        }
    }
    slots.clear();
    idle.clear();
    {
        let mut locked = state.lock().unwrap();
        locked.running.clear();
        locked.process_ids.clear();
    }
    for slot_id in 0..config.worker_count {
        let slot = spawn_slot(slot_id, config, job, slot_events_tx.clone()).await?;
        state
            .lock()
            .unwrap()
            .process_ids
            .insert(slot_id, slot.process_id);
        idle.insert(slot_id);
        slots.insert(slot_id, slot);
    }
    Ok(())
}

#[allow(clippy::too_many_arguments)]
/// 删除目标任务等待项，终止其运行槽位，并以新 Worker 补齐池容量。
async fn cancel_task_items(
    task_id: &str,
    config: &WorkerPoolConfig,
    job: &WorkerJob,
    queue: &mut VecDeque<WorkItem>,
    slots: &mut BTreeMap<usize, SlotHandle>,
    idle: &mut BTreeSet<usize>,
    slot_events: &mut mpsc::UnboundedReceiver<SlotEvent>,
    slot_events_tx: &mpsc::UnboundedSender<SlotEvent>,
    events: &mpsc::Sender<WorkerEvent>,
    state: &Arc<Mutex<PoolState>>,
) -> Result<(), WorkerPoolError> {
    let mut kept = VecDeque::with_capacity(queue.len());
    while let Some(work) = queue.pop_front() {
        if work.identity.task_id == task_id {
            let _ = events
                .send(WorkerEvent::Cancelled {
                    task_id: work.identity.task_id,
                    item_id: work.identity.item_id,
                })
                .await;
        } else {
            kept.push_back(work);
        }
    }
    *queue = kept;

    let targets: HashMap<usize, WorkIdentity> = state
        .lock()
        .unwrap()
        .running
        .iter()
        .filter(|(_, work)| work.task_id == task_id)
        .map(|(slot, work)| (*slot, work.clone()))
        .collect();
    let mut expected: HashSet<usize> = targets.keys().copied().collect();
    let mut deferred = Vec::new();
    for slot_id in &expected {
        if let Some(slot) = slots.get(slot_id) {
            let _ = slot.commands.send(SlotCommand::Terminate);
        }
    }
    while !expected.is_empty() {
        match slot_events.recv().await {
            Some(SlotEvent::Exited { slot_id, .. }) if expected.remove(&slot_id) => {}
            Some(SlotEvent::Response { slot_id, .. }) if expected.contains(&slot_id) => {}
            Some(event) => deferred.push(event),
            None => return Err(WorkerPoolError::Closed),
        }
    }
    for (slot_id, work) in targets {
        slots.remove(&slot_id);
        idle.remove(&slot_id);
        {
            let mut locked = state.lock().unwrap();
            locked.running.remove(&slot_id);
            locked.process_ids.remove(&slot_id);
        }
        let _ = events
            .send(WorkerEvent::Cancelled {
                task_id: work.task_id,
                item_id: work.item_id,
            })
            .await;
        let slot = spawn_slot(slot_id, config, job, slot_events_tx.clone()).await?;
        state
            .lock()
            .unwrap()
            .process_ids
            .insert(slot_id, slot.process_id);
        idle.insert(slot_id);
        slots.insert(slot_id, slot);
    }
    for event in deferred {
        handle_slot_event(
            event,
            config,
            job,
            slots,
            idle,
            slot_events_tx,
            events,
            state,
        )
        .await;
    }
    Ok(())
}

impl TryFrom<proto::WorkerEnvelope> for WorkItem {
    type Error = WorkerPoolError;

    /// 只接受三种请求消息，并在进入队列前提取稳定 task/item ID。
    fn try_from(envelope: proto::WorkerEnvelope) -> Result<Self, Self::Error> {
        let identity = match envelope.payload.as_ref() {
            Some(worker_envelope::Payload::ProbeAndStage1(command)) => WorkIdentity {
                task_id: command.task_id.clone(),
                item_id: command.item_id.clone(),
            },
            Some(worker_envelope::Payload::ComputeStage2(command)) => WorkIdentity {
                task_id: command.task_id.clone(),
                item_id: command.item_id.clone(),
            },
            Some(worker_envelope::Payload::BuildContactSheet(command)) => WorkIdentity {
                task_id: command.task_id.clone(),
                item_id: command.item_id.clone(),
            },
            _ => return Err(WorkerPoolError::InvalidRequest),
        };
        Ok(Self {
            identity,
            envelope,
            scan_guard: None,
        })
    }
}
