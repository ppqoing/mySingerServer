//! 多 Worker 进程的串行 actor、异常替换、取消与两阶段计划重启。

use std::{
    collections::{BTreeMap, BTreeSet, HashMap, HashSet, VecDeque},
    sync::{
        Arc, Condvar, Mutex,
        atomic::{AtomicUsize, Ordering},
    },
    time::Duration,
};

use dedup_core::{DisplayPath, MachineId, NormalizedPath};
use dedup_protocol::proto::{self, worker_envelope};
use dedup_windows::{ReadCancellationToken, WorkerJob};
use thiserror::Error;
use tokio::sync::{mpsc, oneshot};

use super::process::{WorkerLaunch, WorkerProcess};
use super::{Stage1Output, encode_stage1_payload};

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
    /// 请求已经在真实 slot 的 Run 边界发送。
    Started {
        /// 所属任务 ID。
        task_id: String,
        /// 任务项 ID。
        item_id: String,
        /// 实际槽位。
        slot: u32,
        /// 当前槽位真实 PID；可控池可使用合成 PID。
        process_id: Option<u32>,
        /// dispatch 冻结文件身份。
        identity: WorkerFileIdentity,
    },
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
        /// dispatch 时冻结并随真实运行项返回的文件身份。
        identity: WorkerFileIdentity,
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

/// 扫描 Worker dispatch 时冻结的批准文件身份，不含进程或尝试诊断。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct WorkerFileIdentity {
    /// 节点物理机器 ID。
    pub machine_id: MachineId,
    /// SQLite、去重和故障唯一键使用的规范路径。
    pub normalized_path: NormalizedPath,
    /// Worker 实际访问并供诊断显示的路径。
    pub display_path: DisplayPath,
    /// 枚举/任务项冻结的文件大小。
    pub file_size: u64,
    /// Worker 正在执行的流水线阶段。
    pub stage: String,
    /// 读取许可冻结的物理盘显示身份。
    pub physical_disk_id: String,
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

/// 持久取消前占用的任务发送门禁；未 commit 即 Drop 会回滚 cancelling 标记。
pub struct TaskCancelGate {
    state: Arc<Mutex<PoolState>>,
    commands: mpsc::Sender<PoolCommand>,
    task_id: String,
    finished: bool,
}

impl TaskCancelGate {
    /// 持久取消成功后提交门禁，使后续调度永久拒绝该任务。
    pub fn commit(mut self) {
        let mut state = self.state.lock().unwrap();
        state.cancelling_tasks.remove(&self.task_id);
        state.cancelled_tasks.insert(self.task_id.clone());
        self.finished = true;
    }

    /// 持久取消失败时回滚门禁并唤醒 pool actor 重新调度等待项。
    pub fn rollback(mut self) {
        self.state
            .lock()
            .unwrap()
            .cancelling_tasks
            .remove(&self.task_id);
        let _ = self
            .commands
            .try_send(PoolCommand::CancelRollback(self.task_id.clone()));
        self.finished = true;
    }
}

impl Drop for TaskCancelGate {
    fn drop(&mut self) {
        if !self.finished {
            self.state
                .lock()
                .unwrap()
                .cancelling_tasks
                .remove(&self.task_id);
            let _ = self
                .commands
                .try_send(PoolCommand::CancelRollback(self.task_id.clone()));
        }
    }
}

/// 只供直接竞态测试把调度卡在门禁检查后、slot send 前。
#[doc(hidden)]
#[derive(Clone, Default)]
pub struct WorkerDispatchBarrier {
    state: Arc<(Mutex<(bool, bool)>, Condvar, Condvar)>,
}

/// 可控多槽池的完成/崩溃驱动，只供直接集成测试。
#[doc(hidden)]
#[derive(Clone)]
pub struct ControlledWorkerPool {
    commands: mpsc::Sender<ControlledWorkerCommand>,
    available_slots: Arc<AtomicUsize>,
    state: Arc<Mutex<PoolState>>,
}

impl ControlledWorkerPool {
    /// 让指定运行项按 Worker 崩溃返回并立即补回逻辑槽位。
    pub async fn crash(&self, task_id: String, item_id: String, message: String) {
        let _ = self
            .commands
            .send(ControlledWorkerCommand::Crash {
                task_id,
                item_id,
                message,
            })
            .await;
    }

    /// 让指定运行项返回正常一筛结果。
    pub async fn complete(&self, task_id: String, item_id: String, output: Stage1Output) {
        let _ = self
            .commands
            .send(ControlledWorkerCommand::Complete {
                task_id,
                item_id,
                output,
            })
            .await;
    }

    /// 返回当前未被运行项占用的逻辑槽位数。
    pub fn available_slots(&self) -> usize {
        self.available_slots.load(Ordering::Acquire)
    }

    /// 返回当前真实 running map 中携带冻结文件身份的项。
    pub fn running_files(&self) -> Vec<(String, String, WorkerFileIdentity)> {
        let mut rows = self
            .state
            .lock()
            .unwrap()
            .running
            .values()
            .filter_map(|work| {
                work.file_identity
                    .clone()
                    .map(|identity| (work.task_id.clone(), work.item_id.clone(), identity))
            })
            .collect::<Vec<_>>();
        rows.sort_by(|left, right| left.1.cmp(&right.1));
        rows
    }
}

enum ControlledWorkerCommand {
    Crash {
        task_id: String,
        item_id: String,
        message: String,
    },
    Complete {
        task_id: String,
        item_id: String,
        output: Stage1Output,
    },
}

impl WorkerDispatchBarrier {
    /// 阻塞等待调度抵达 send 前边界。
    pub fn wait_until_entered(&self) {
        let (state, entered, _) = &*self.state;
        let mut state = state.lock().unwrap();
        while !state.0 {
            state = entered.wait(state).unwrap();
        }
    }

    /// 允许被卡住的调度继续发送。
    pub fn release(&self) {
        let (state, _, released) = &*self.state;
        let mut state = state.lock().unwrap();
        state.1 = true;
        released.notify_all();
    }

    fn block_before_send(&self) {
        let (state, entered, released) = &*self.state;
        let mut state = state.lock().unwrap();
        state.0 = true;
        entered.notify_all();
        while !state.1 {
            state = released.wait(state).unwrap();
        }
    }
}

impl WorkerPoolHandle {
    /// 从可克隆控制面并发派发扫描请求，事件仍由唯一 WorkerPool owner 消费。
    pub async fn dispatch_scan(
        &self,
        envelope: proto::WorkerEnvelope,
        cancellation: ReadCancellationToken,
        persisted_active: bool,
        file_identity: WorkerFileIdentity,
    ) -> Result<(), WorkerPoolError> {
        let (reply_tx, reply_rx) = oneshot::channel();
        self.commands
            .send(PoolCommand::Dispatch(
                envelope,
                Some(ScanDispatchGuard {
                    cancellation,
                    persisted_active,
                    file_identity,
                }),
                reply_tx,
            ))
            .await
            .map_err(|_| WorkerPoolError::Closed)?;
        reply_rx.await.map_err(|_| WorkerPoolError::Closed)?
    }

    /// 在持久取消事务前同步关闭新 slot send，并等待已进入临界区的 send 登记完成。
    pub fn begin_task_cancel(&self, task_id: &str) -> TaskCancelGate {
        self.state
            .lock()
            .unwrap()
            .cancelling_tasks
            .insert(task_id.to_owned());
        TaskCancelGate {
            state: self.state.clone(),
            commands: self.commands.clone(),
            task_id: task_id.to_owned(),
            finished: false,
        }
    }

    /// 在持久取消提交后同步标记任务，使 slot send 临界区拒绝后续请求。
    pub fn mark_task_cancelled(&self, task_id: &str) {
        self.begin_task_cancel(task_id).commit();
    }

    /// 尝试在不等待的情况下标记取消，只供 send 临界区竞态测试。
    #[doc(hidden)]
    pub fn try_mark_task_cancelled_for_test(&self, task_id: &str) -> bool {
        let Ok(mut state) = self.state.try_lock() else {
            return false;
        };
        state.cancelled_tasks.insert(task_id.to_owned());
        true
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
        file_identity: WorkerFileIdentity,
    ) -> Result<(), WorkerPoolError> {
        let (reply_tx, reply_rx) = oneshot::channel();
        self.commands
            .send(PoolCommand::Dispatch(
                envelope,
                Some(ScanDispatchGuard {
                    cancellation,
                    persisted_active,
                    file_identity,
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

    /// 创建可控多槽池，直接驱动乱序完成与崩溃补槽行为。
    #[doc(hidden)]
    pub fn controlled_batch_for_test(
        worker_count: usize,
    ) -> (Self, mpsc::Receiver<(String, String)>, ControlledWorkerPool) {
        assert!(worker_count > 0);
        let state = Arc::new(Mutex::new(PoolState::default()));
        for slot in 0..worker_count {
            state
                .lock()
                .unwrap()
                .process_ids
                .insert(slot, slot as u32 + 1);
        }
        let actor_state = state.clone();
        let (commands, mut command_rx) = mpsc::channel(64);
        let (events, event_rx) = mpsc::channel(256);
        let (started, started_rx) = mpsc::channel(16);
        let (control_tx, mut control_rx) = mpsc::channel(16);
        let available_slots = Arc::new(AtomicUsize::new(worker_count));
        let actor_available = available_slots.clone();
        tokio::spawn(async move {
            let mut queue = VecDeque::new();
            let mut idle = (0..worker_count).collect::<VecDeque<_>>();
            let mut active = HashMap::<String, (usize, WorkIdentity)>::new();
            loop {
                tokio::select! {
                    command = command_rx.recv() => {
                        let Some(command) = command else { break };
                        match command {
                            PoolCommand::Dispatch(envelope, guard, reply) => {
                                let result = WorkItem::try_from(envelope).map(|mut work| {
                                    work.set_scan_guard(guard);
                                    queue.push_back(work);
                                    controlled_schedule(
                                        &mut queue, &mut idle, &mut active, &started,
                                        &events, &actor_state, &actor_available,
                                    );
                                });
                                let _ = reply.send(result);
                            }
                            PoolCommand::Cancel(task_id, reply) => {
                                actor_state
                                    .lock()
                                    .unwrap()
                                    .cancelled_tasks
                                    .insert(task_id);
                                let _ = reply.send(Ok(()));
                            }
                            PoolCommand::CancelRollback(task_id) => {
                                actor_state
                                    .lock()
                                    .unwrap()
                                    .cancelling_tasks
                                    .remove(&task_id);
                            }
                            PoolCommand::PrepareRestart(reply) => { let _ = reply.send(Ok(Vec::new())); }
                            PoolCommand::Restart(_, reply) => { let _ = reply.send(Ok(())); }
                            PoolCommand::TerminateUnexpected(_, reply) => { let _ = reply.send(Ok(())); }
                        }
                    }
                    control = control_rx.recv() => {
                        let Some(control) = control else { break };
                        let (task_id, item_id) = match &control {
                            ControlledWorkerCommand::Crash { task_id, item_id, .. }
                            | ControlledWorkerCommand::Complete { task_id, item_id, .. } => {
                                (task_id.clone(), item_id.clone())
                            }
                        };
                        let Some((slot, identity)) = active.remove(&item_id) else { continue };
                        if identity.task_id != task_id { continue; }
                        let event = match control {
                            ControlledWorkerCommand::Crash { message, .. } => {
                                match identity.file_identity.clone() {
                                    Some(file_identity) => WorkerEvent::Crashed {
                                        task_id: task_id.clone(),
                                        item_id: item_id.clone(),
                                        identity: file_identity,
                                        message,
                                    },
                                    None => WorkerEvent::InfrastructureFailure {
                                        message: "可控崩溃项缺少冻结文件身份".into(),
                                    },
                                }
                            }
                            ControlledWorkerCommand::Complete { output, .. } => {
                                WorkerEvent::Completed {
                                    task_id: task_id.clone(),
                                    item_id: item_id.clone(),
                                    response: proto::WorkerEnvelope {
                                        payload: Some(worker_envelope::Payload::Stage1Result(
                                            proto::Stage1Result {
                                                task_id: task_id.clone(),
                                                item_id: item_id.clone(),
                                                payload: encode_stage1_payload(&output),
                                            },
                                        )),
                                    },
                                }
                            }
                        };
                        actor_state.lock().unwrap().running.remove(&slot);
                        idle.push_back(slot);
                        actor_available.store(idle.len(), Ordering::Release);
                        let _ = events.send(event).await;
                        controlled_schedule(
                            &mut queue, &mut idle, &mut active, &started,
                            &events, &actor_state, &actor_available,
                        );
                    }
                }
            }
        });
        (
            Self {
                commands,
                events: event_rx,
                state: state.clone(),
            },
            started_rx,
            ControlledWorkerPool {
                commands: control_tx,
                available_slots,
                state,
            },
        )
    }

    /// 创建不启动进程的可控池，只供直接集成测试验证发送/取消边界。
    #[doc(hidden)]
    pub fn controlled_for_test() -> (Self, mpsc::Receiver<(String, String)>) {
        Self::controlled_inner(None)
    }

    /// 创建在门禁检查后、send 前暂停的可控池，只供竞态集成测试。
    #[doc(hidden)]
    pub fn controlled_with_dispatch_barrier_for_test() -> (
        Self,
        mpsc::Receiver<(String, String)>,
        WorkerDispatchBarrier,
    ) {
        let barrier = WorkerDispatchBarrier::default();
        let (pool, started) = Self::controlled_inner(Some(barrier.clone()));
        (pool, started, barrier)
    }

    fn controlled_inner(
        dispatch_barrier: Option<WorkerDispatchBarrier>,
    ) -> (Self, mpsc::Receiver<(String, String)>) {
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
                            work.set_scan_guard(guard);
                            let mut locked = actor_state.lock().unwrap();
                            let registry_blocked =
                                locked.cancelled_tasks.contains(&work.identity.task_id)
                                    || locked.cancelling_tasks.contains(&work.identity.task_id);
                            if registry_blocked || !work.dispatch_allowed() {
                                drop(locked);
                                let _ = events.try_send(WorkerEvent::Cancelled {
                                    task_id: work.identity.task_id,
                                    item_id: work.identity.item_id,
                                });
                                return;
                            }
                            if let Some(barrier) = &dispatch_barrier {
                                barrier.block_before_send();
                            }
                            active = Some(work.identity.clone());
                            locked.running.insert(0, work.identity.clone());
                            let _ = started.try_send((
                                work.identity.task_id.clone(),
                                work.identity.item_id.clone(),
                            ));
                        });
                        let _ = reply.send(result);
                    }
                    PoolCommand::Cancel(task_id, reply) => {
                        {
                            let mut state = actor_state.lock().unwrap();
                            state.cancelling_tasks.remove(&task_id);
                            state.cancelled_tasks.insert(task_id.clone());
                        }
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
                    PoolCommand::CancelRollback(task_id) => {
                        actor_state
                            .lock()
                            .unwrap()
                            .cancelling_tasks
                            .remove(&task_id);
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

fn controlled_schedule(
    queue: &mut VecDeque<WorkItem>,
    idle: &mut VecDeque<usize>,
    active: &mut HashMap<String, (usize, WorkIdentity)>,
    started: &mpsc::Sender<(String, String)>,
    events: &mpsc::Sender<WorkerEvent>,
    state: &Arc<Mutex<PoolState>>,
    available_slots: &Arc<AtomicUsize>,
) {
    while !idle.is_empty() && !queue.is_empty() {
        let slot = idle.pop_front().expect("空闲槽已确认存在");
        let work = queue.pop_front().expect("等待项已确认存在");
        let blocked = {
            let state = state.lock().unwrap();
            state.cancelled_tasks.contains(&work.identity.task_id)
                || state.cancelling_tasks.contains(&work.identity.task_id)
                || !work.dispatch_allowed()
        };
        if blocked {
            idle.push_front(slot);
            let _ = events.try_send(WorkerEvent::Cancelled {
                task_id: work.identity.task_id,
                item_id: work.identity.item_id,
            });
            continue;
        }
        let identity = work.identity;
        state.lock().unwrap().running.insert(slot, identity.clone());
        active.insert(identity.item_id.clone(), (slot, identity.clone()));
        if let Some(file_identity) = identity.file_identity.clone() {
            let process_id = state.lock().unwrap().process_ids.get(&slot).copied();
            let _ = events.try_send(WorkerEvent::Started {
                task_id: identity.task_id.clone(),
                item_id: identity.item_id.clone(),
                slot: slot as u32,
                process_id,
                identity: file_identity,
            });
        }
        let _ = started.try_send((identity.task_id, identity.item_id));
        available_slots.store(idle.len(), Ordering::Release);
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
    cancelling_tasks: HashSet<String>,
}

#[derive(Clone, Debug)]
/// 不携带路径或特征数据的任务归属，用于崩溃、取消和重启持久化。
struct WorkIdentity {
    task_id: String,
    item_id: String,
    file_identity: Option<WorkerFileIdentity>,
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
    file_identity: WorkerFileIdentity,
}

impl WorkItem {
    fn set_scan_guard(&mut self, guard: Option<ScanDispatchGuard>) {
        if let Some(guard) = &guard {
            self.identity.file_identity = Some(guard.file_identity.clone());
        }
        self.scan_guard = guard;
    }

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
    CancelRollback(String),
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
                                    work.set_scan_guard(guard);
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
                        {
                            let mut locked = state.lock().unwrap();
                            locked.cancelling_tasks.remove(&task_id);
                            locked.cancelled_tasks.insert(task_id.clone());
                        }
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
                    PoolCommand::CancelRollback(task_id) => {
                        state
                            .lock()
                            .unwrap()
                            .cancelling_tasks
                            .remove(&task_id);
                        schedule(&mut queue, &mut idle, &slots, &events, &state).await;
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
    enum DispatchDecision {
        Sent,
        Rejected(WorkIdentity),
        Retry(WorkItem),
        Restarting(WorkItem),
        Cancelling(WorkItem),
    }
    if state.lock().unwrap().restarting {
        return;
    }
    while !idle.is_empty() && !queue.is_empty() {
        let work = queue.pop_front().expect("队列已判定非空");
        let slot_id = idle.pop_first().expect("集合已判定非空");
        let identity = work.identity.clone();
        let Some(slot) = slots.get(&slot_id) else {
            queue.push_front(work);
            continue;
        };
        let decision = {
            let mut locked = state.lock().unwrap();
            if locked.restarting {
                DispatchDecision::Restarting(work)
            } else {
                if locked.cancelling_tasks.contains(&identity.task_id) {
                    DispatchDecision::Cancelling(work)
                } else if locked.cancelled_tasks.contains(&identity.task_id)
                    || !work.dispatch_allowed()
                {
                    DispatchDecision::Rejected(identity.clone())
                } else if let Err(error) = slot.commands.send(SlotCommand::Run(work)) {
                    let SlotCommand::Run(work) = error.0 else {
                        unreachable!("schedule only sends Run")
                    };
                    DispatchDecision::Retry(work)
                } else {
                    locked.running.insert(slot_id, identity.clone());
                    DispatchDecision::Sent
                }
            }
        };
        match decision {
            DispatchDecision::Sent => {
                if let Some(identity_file) = identity.file_identity.clone() {
                    let _ = events
                        .send(WorkerEvent::Started {
                            task_id: identity.task_id,
                            item_id: identity.item_id,
                            slot: slot_id as u32,
                            process_id: Some(slot.process_id),
                            identity: identity_file,
                        })
                        .await;
                }
            }
            DispatchDecision::Rejected(identity) => {
                idle.insert(slot_id);
                let _ = events
                    .send(WorkerEvent::Cancelled {
                        task_id: identity.task_id,
                        item_id: identity.item_id,
                    })
                    .await;
            }
            DispatchDecision::Retry(work) => {
                idle.insert(slot_id);
                queue.push_front(work);
            }
            DispatchDecision::Restarting(work) => {
                idle.insert(slot_id);
                queue.push_front(work);
                return;
            }
            DispatchDecision::Cancelling(work) => {
                idle.insert(slot_id);
                queue.push_front(work);
                return;
            }
        }
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
                if let Some(identity) = work.file_identity {
                    let _ = events
                        .send(WorkerEvent::Crashed {
                            task_id: work.task_id,
                            item_id: work.item_id,
                            identity,
                            message,
                        })
                        .await;
                } else {
                    let _ = events
                        .send(WorkerEvent::InfrastructureFailure {
                            message: format!(
                                "Worker 崩溃项缺少冻结文件身份: {}/{}: {message}",
                                work.task_id, work.item_id
                            ),
                        })
                        .await;
                }
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
                file_identity: None,
            },
            Some(worker_envelope::Payload::ComputeStage2(command)) => WorkIdentity {
                task_id: command.task_id.clone(),
                item_id: command.item_id.clone(),
                file_identity: None,
            },
            Some(worker_envelope::Payload::BuildContactSheet(command)) => WorkIdentity {
                task_id: command.task_id.clone(),
                item_id: command.item_id.clone(),
                file_identity: None,
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
