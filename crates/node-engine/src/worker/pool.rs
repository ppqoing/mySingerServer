//! 多 Worker 进程的串行 actor、异常替换、取消与可等待关闭。

use std::{
    collections::{BTreeMap, HashMap, HashSet, VecDeque},
    future::Future,
    pin::Pin,
    sync::{
        Arc, Condvar, Mutex,
        atomic::{AtomicUsize, Ordering},
    },
    time::{Duration, Instant},
};

use dedup_core::{DisplayPath, MachineId, MediaKind, NormalizedPath};
use dedup_protocol::proto::{self, worker_envelope};
use dedup_windows::{ReadCancellationToken, WorkerJob};
use thiserror::Error;
use tokio::sync::{Notify, mpsc, oneshot};

use super::process::{WorkerLaunch, WorkerProcess};
use super::{
    BaseComputeOutput, Stage1Output, Stage2Output, encode_base_compute_payload,
    encode_stage1_payload, encode_stage2_payload,
};

const DEFAULT_READY_TIMEOUT: Duration = Duration::from_secs(15);
/// 关闭 Pool 等待 slot 退出和 driver 收束的最大时间。
const DEFAULT_SHUTDOWN_TIMEOUT: Duration = Duration::from_secs(15);
/// 等待项被其他任务绕过达到该次数后，为最老任务保留后续 CPU 预算。
const CPU_AGING_BYPASS_LIMIT: usize = 8;
/// 对外 WorkerEvent 通道和内部待发送事件队列的固定容量，避免事件无界增长。
const WORKER_EVENT_CAPACITY: usize = 256;
/// WorkerPool 控制命令通道的固定容量，等待队列上界据此吸收已入站命令。
const POOL_COMMAND_CAPACITY: usize = 64;

/// 将事件排入池 actor 独立的 FIFO 发送队列；实现者不得丢弃或重排事件。
trait WorkerEventSink {
    /// 把事件交给发送队列；等待的只是有限 outbox 空间，不是外部消费速度。
    fn send_event<'a>(
        &'a self,
        event: WorkerEvent,
    ) -> Pin<Box<dyn Future<Output = ()> + Send + 'a>>;
}

/// 独立转发 WorkerEvent 的有限 FIFO outbox，解除控制命令与外部事件通道的直接耦合。
#[derive(Clone)]
struct WorkerEventOutbox {
    /// 外层事件通道前的固定容量 FIFO。
    pending: mpsc::Sender<WorkerEvent>,
    /// 内层消费或外层恢复写入时唤醒 actor。
    progress: Arc<Notify>,
}

impl WorkerEventOutbox {
    /// 创建固定容量 outbox，并启动唯一 drain 保持事件顺序和完整性。
    fn new(events: mpsc::Sender<WorkerEvent>) -> Self {
        let (pending, mut receiver) = mpsc::channel(WORKER_EVENT_CAPACITY);
        let progress = Arc::new(Notify::new());
        let drain_progress = Arc::clone(&progress);
        tokio::spawn(async move {
            while let Some(event) = receiver.recv().await {
                // 内层 sender 已取走一个事件，actor 本地暂存可以尝试继续入队。
                drain_progress.notify_one();
                if events.send(event).await.is_err() {
                    break;
                }
                // 外层成功接收后再次唤醒，覆盖外层从满到可写的进度。
                drain_progress.notify_one();
            }
            drain_progress.notify_one();
        });
        Self { pending, progress }
    }

    /// 非阻塞地把事件送入内层有限队列，满时把事件交还给调用方保留。
    fn try_send_event(
        &self,
        event: WorkerEvent,
    ) -> Result<(), mpsc::error::TrySendError<WorkerEvent>> {
        self.pending.try_send(event)
    }

    /// 等待 outbox 消费进度，避免 actor 通过定时器忙轮询。
    async fn wait_for_progress(&self) {
        self.progress.notified().await;
    }
}

impl WorkerEventSink for WorkerEventOutbox {
    /// 将事件放入内部有限队列，由唯一 drain 按入队顺序交付给 owner。
    fn send_event<'a>(
        &'a self,
        event: WorkerEvent,
    ) -> Pin<Box<dyn Future<Output = ()> + Send + 'a>> {
        Box::pin(async move {
            let _ = self.pending.send(event).await;
        })
    }
}

impl WorkerEventSink for mpsc::Sender<WorkerEvent> {
    /// 直接发送仅供底层单元测试使用；生产 actor 使用独立 outbox。
    fn send_event<'a>(
        &'a self,
        event: WorkerEvent,
    ) -> Pin<Box<dyn Future<Output = ()> + Send + 'a>> {
        Box::pin(async move {
            let _ = self.send(event).await;
        })
    }
}

/// actor 私有的有界 FIFO 事件暂存；外层/内层满时仍不丢弃任何事件。
struct PendingWorkerEvents {
    /// actor 独占的待发送事件，只有 sink 与 flush 路径会访问。
    queue: Mutex<VecDeque<WorkerEvent>>,
    /// 由 Worker 数量和控制命令容量推导出的硬上限。
    capacity: usize,
}

impl PendingWorkerEvents {
    /// 按 Worker/命令状态计算的固定容量创建本地暂存。
    fn new(capacity: usize) -> Self {
        assert!(capacity > 0, "Worker 事件暂存容量必须大于零");
        Self {
            queue: Mutex::new(VecDeque::new()),
            capacity,
        }
    }

    /// 尝试追加一条事件；容量满时原样返回，调用方负责等待进度。
    fn try_push(&self, event: WorkerEvent) -> Result<(), WorkerEvent> {
        let mut queue = self.queue.lock().unwrap();
        if queue.len() >= self.capacity {
            return Err(event);
        }
        queue.push_back(event);
        Ok(())
    }

    /// 将本地 FIFO 尽可能送入 outbox；失败事件放回队首，保证顺序和所有权。
    fn try_flush(&self, outbox: &WorkerEventOutbox) {
        loop {
            let Some(event) = self.queue.lock().unwrap().pop_front() else {
                return;
            };
            match outbox.try_send_event(event) {
                Ok(()) => {}
                Err(mpsc::error::TrySendError::Full(event))
                | Err(mpsc::error::TrySendError::Closed(event)) => {
                    self.queue.lock().unwrap().push_front(event);
                    return;
                }
            }
        }
    }

    /// 判断本地是否仍有待交付事件，作为 actor 进度分支的启用条件。
    fn has_pending(&self) -> bool {
        !self.queue.lock().unwrap().is_empty()
    }
}

/// 使用 actor 本地有界暂存的事件 sink；控制路径只等待暂存空间，不等待外部消费者。
#[derive(Clone)]
struct ActorWorkerEventSink {
    /// 本 actor 的有界事件暂存。
    pending: Arc<PendingWorkerEvents>,
    /// 保持与外部事件 owner 相同的 FIFO 出口。
    outbox: WorkerEventOutbox,
}

impl ActorWorkerEventSink {
    /// 绑定唯一 actor 的暂存与有序 outbox。
    fn new(pending: Arc<PendingWorkerEvents>, outbox: WorkerEventOutbox) -> Self {
        Self { pending, outbox }
    }
}

impl WorkerEventSink for ActorWorkerEventSink {
    /// 事件先进入 actor 私有 FIFO；暂存满时等待 outbox 进度而非忙轮询。
    fn send_event<'a>(
        &'a self,
        event: WorkerEvent,
    ) -> Pin<Box<dyn Future<Output = ()> + Send + 'a>> {
        let pending = Arc::clone(&self.pending);
        let outbox = self.outbox.clone();
        Box::pin(async move {
            let mut event = event;
            loop {
                match pending.try_push(event) {
                    Ok(()) => return,
                    Err(returned) => event = returned,
                }
                let progress = outbox.wait_for_progress();
                pending.try_flush(&outbox);
                match pending.try_push(event) {
                    Ok(()) => return,
                    Err(returned) => event = returned,
                }
                progress.await;
            }
        })
    }
}

/// 等待队列上界：Worker 槽位与已进入控制通道的命令共同决定最大积压。
fn max_waiting_work_items(worker_count: usize) -> usize {
    worker_count.max(1).saturating_add(POOL_COMMAND_CAPACITY)
}

/// 本地事件上界：运行槽位终态 + 等待项取消 + 一批控制命令各自最多产生一条事件。
fn actor_event_buffer_capacity(worker_count: usize) -> usize {
    let worker_count = worker_count.max(1);
    worker_count
        .saturating_add(max_waiting_work_items(worker_count))
        .saturating_add(POOL_COMMAND_CAPACITY)
}

/// WorkerPool 的进程数量、可执行文件和 Ready 超时。
#[derive(Clone, Debug)]
pub struct WorkerPoolConfig {
    launch: WorkerLaunch,
    worker_count: usize,
    cpu_budget: usize,
    ready_timeout: Duration,
    result_read_delay: Duration,
    /// 关闭时等待每个 slot 退出和 driver join 的统一上限。
    shutdown_timeout: Duration,
}

impl WorkerPoolConfig {
    /// 使用固定 15 秒 Ready 超时创建池配置。
    pub const fn new(launch: WorkerLaunch, worker_count: usize) -> Self {
        Self {
            launch,
            worker_count,
            cpu_budget: worker_count,
            ready_timeout: DEFAULT_READY_TIMEOUT,
            result_read_delay: Duration::ZERO,
            shutdown_timeout: DEFAULT_SHUTDOWN_TIMEOUT,
        }
    }

    /// 覆盖 WorkerPool 的统一 CPU 权重预算；生产入口应传入扣除保留核心后的值。
    pub const fn with_cpu_budget(mut self, cpu_budget: usize) -> Self {
        self.cpu_budget = cpu_budget;
        self
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

    /// 覆盖关闭收束上限，只供生命周期故障测试缩短虚拟时间。
    #[doc(hidden)]
    pub const fn with_shutdown_timeout(mut self, timeout: Duration) -> Self {
        self.shutdown_timeout = timeout;
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
        /// 实际原子登记的 CPU 权重。
        cpu_weight: u32,
        /// 一次性基础媒体请求显式使用的解码线程数。
        decoder_threads: Option<u32>,
        /// 从进入 WorkerPool 有界队列到真实发送 Run 的等待微秒。
        queue_wait_us: u64,
    },
    /// Worker 进程在真实执行边界即时发出的非终态阶段。
    PhaseChanged {
        /// 所属任务 ID。
        task_id: String,
        /// 任务项 ID。
        item_id: String,
        /// 实际槽位。
        slot: u32,
        /// 仅接受协议定义的 idle/decode/feature/result_wait。
        phase: proto::RuntimeWorkerPhase,
        /// 从 Worker 收到请求起累计的微秒。
        request_elapsed_us: Option<u64>,
    },
    /// 一次性基础计算已结束全部源文件读取，但当前任务仍等待终态结果。
    BaseSourceReadComplete {
        /// 所属任务 ID。
        task_id: String,
        /// 任务项 ID。
        item_id: String,
        /// 实际槽位。
        slot: u32,
        /// 从 Worker 收到请求到关闭源文件的微秒。
        request_elapsed_us: Option<u64>,
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
        /// 发生崩溃的 Worker PID。
        process_id: Option<u32>,
        /// 操作系统可提供时的 Worker 退出码。
        exit_code: Option<i32>,
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

/// 扫描 Worker dispatch 时冻结的批准文件上下文；路径身份固定，阶段由 Node 随运行更新。
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
    /// Node 记录的 Worker 当前流水线阶段；非终态事件可更新该值。
    pub stage: String,
    /// 读取许可冻结的物理盘显示身份。
    pub physical_disk_id: String,
}

/// 拥有 Worker 进程 actor 的客户端句柄。
pub struct WorkerPool {
    commands: mpsc::Sender<PoolCommand>,
    events: mpsc::Receiver<WorkerEvent>,
    state: Arc<Mutex<PoolState>>,
    /// 唯一池 actor 的收束句柄；关闭必须等待其释放所有 slot 和 Job。
    actor: tokio::task::JoinHandle<()>,
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
    /// 已实际取得 slot 与 CPU 的基础命令快照，仅供 Node 调度行为测试读取。
    started_base_commands: Arc<Mutex<Vec<proto::ComputeBaseFeatures>>>,
    /// 下一次可控取消在清除 Worker 后、返回 ACK 前使用的测试闸门。
    cancel_gate: Arc<Mutex<Option<(Arc<Notify>, Arc<Notify>)>>>,
}

impl ControlledWorkerPool {
    /// 让指定运行项上报一个真实 Worker 阶段，只供 Node 运行时投影行为测试。
    #[doc(hidden)]
    pub async fn phase_changed(
        &self,
        task_id: String,
        item_id: String,
        phase: proto::RuntimeWorkerPhase,
    ) {
        let _ = self
            .commands
            .send(ControlledWorkerCommand::PhaseChanged {
                task_id,
                item_id,
                phase,
            })
            .await;
    }

    /// 让一次性基础计算返回源读取完成事件，但继续占用同一逻辑槽位等待终态。
    pub async fn base_source_read_complete(&self, task_id: String, item_id: String) {
        let _ = self
            .commands
            .send(ControlledWorkerCommand::BaseSourceReadComplete { task_id, item_id })
            .await;
    }

    /// 让一次性基础计算返回最终媒体结果。
    pub async fn complete_base(
        &self,
        task_id: String,
        item_id: String,
        md5: [u8; 16],
        output: BaseComputeOutput,
    ) {
        let _ = self
            .commands
            .send(ControlledWorkerCommand::CompleteBase {
                task_id,
                item_id,
                md5,
                output,
            })
            .await;
    }

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

    /// 让指定运行项返回正常二筛结果。
    #[doc(hidden)]
    pub async fn complete_stage2(&self, task_id: String, item_id: String, output: Stage2Output) {
        let _ = self
            .commands
            .send(ControlledWorkerCommand::CompleteStage2 {
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

    /// 返回当前已被运行项原子登记的 CPU 权重。
    pub fn cpu_in_use(&self) -> usize {
        self.state.lock().unwrap().cpu_in_use
    }

    /// 返回该可控池的 CPU 权重硬上限。
    pub fn cpu_budget(&self) -> usize {
        self.state.lock().unwrap().cpu_budget
    }

    /// 返回已实际开始的基础命令副本，供线程策略测试核对协议预算。
    pub fn started_base_commands(&self) -> Vec<proto::ComputeBaseFeatures> {
        self.started_base_commands.lock().unwrap().clone()
    }

    /// 给下一次可控取消安装“Worker 已停止”和“允许 ACK”两个通知，只供生命周期测试。
    #[doc(hidden)]
    pub fn gate_next_cancel_ack_for_test(&self) -> (Arc<Notify>, Arc<Notify>) {
        let stopped = Arc::new(Notify::new());
        let release_ack = Arc::new(Notify::new());
        *self.cancel_gate.lock().unwrap() = Some((Arc::clone(&stopped), Arc::clone(&release_ack)));
        (stopped, release_ack)
    }

    /// 返回当前真实 running map 中携带完整路径和当前阶段的项。
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
    /// 注入一个非终态 Worker 阶段事件。
    PhaseChanged {
        task_id: String,
        item_id: String,
        phase: proto::RuntimeWorkerPhase,
    },
    BaseSourceReadComplete {
        task_id: String,
        item_id: String,
    },
    CompleteBase {
        task_id: String,
        item_id: String,
        md5: [u8; 16],
        output: BaseComputeOutput,
    },
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
    CompleteStage2 {
        task_id: String,
        item_id: String,
        output: Stage2Output,
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
                    cancellation: Some(cancellation),
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
        if config.cpu_budget == 0 {
            return Err(WorkerPoolError::EmptyCpuBudget);
        }
        let job = WorkerJob::create().map_err(|error| WorkerPoolError::Job(error.to_string()))?;
        let state = Arc::new(Mutex::new(PoolState::new(config.cpu_budget)));
        let (slot_events_tx, slot_events_rx) = mpsc::unbounded_channel();
        let mut slots = BTreeMap::new();
        let mut idle = VecDeque::<usize>::new();
        for slot_id in 0..config.worker_count {
            let slot = spawn_slot(slot_id, &config, &job, slot_events_tx.clone()).await?;
            state
                .lock()
                .unwrap()
                .process_ids
                .insert(slot_id, slot.process_id);
            idle.push_back(slot_id);
            slots.insert(slot_id, slot);
        }

        let (commands_tx, commands_rx) = mpsc::channel(POOL_COMMAND_CAPACITY);
        let (events_tx, events_rx) = mpsc::channel(WORKER_EVENT_CAPACITY);
        let event_outbox = WorkerEventOutbox::new(events_tx);
        let actor_state = Arc::clone(&state);
        let actor = tokio::spawn(run_pool(
            config,
            job,
            slots,
            idle,
            commands_rx,
            slot_events_rx,
            slot_events_tx,
            event_outbox,
            actor_state,
        ));
        Ok(Self {
            commands: commands_tx,
            events: events_rx,
            state,
            actor,
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

    /// 普通二筛请求冻结运行时文件/物理盘身份，使真实 slot send 发布 Started。
    pub async fn dispatch_runtime(
        &self,
        envelope: proto::WorkerEnvelope,
        file_identity: WorkerFileIdentity,
    ) -> Result<(), WorkerPoolError> {
        let (reply_tx, reply_rx) = oneshot::channel();
        self.commands
            .send(PoolCommand::Dispatch(
                envelope,
                Some(ScanDispatchGuard {
                    cancellation: None,
                    persisted_active: true,
                    file_identity,
                }),
                reply_tx,
            ))
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
                    cancellation: Some(cancellation),
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

    /// 终止全部 Worker 并等待 actor、slot 与 Job 全部释放；调用后不得继续使用旧 Pool。
    pub async fn shutdown(self) -> Result<(), WorkerPoolError> {
        let Self {
            commands,
            events,
            state: _,
            actor,
        } = self;
        // 丢弃外部接收器，关闭不必等待旧任务事件被业务消费者读取。
        drop(events);
        let (reply_tx, reply_rx) = oneshot::channel();
        let shutdown_result = match commands.send(PoolCommand::Shutdown(reply_tx)).await {
            Ok(()) => reply_rx.await.unwrap_or(Err(WorkerPoolError::Closed)),
            Err(_) => Err(WorkerPoolError::Closed),
        };
        // 无论关闭过程是否报错，都必须等待池 actor 返回，保证 Job 和残余 driver 已释放。
        let actor_result = actor.await.map_err(|_| WorkerPoolError::Closed);
        shutdown_result.and(actor_result)
    }

    /// 等待下一条需由 NodeEngine 持久化的结果或进程事件。
    pub async fn next_event(&mut self) -> Option<WorkerEvent> {
        self.events.recv().await
    }

    /// 非阻塞取得当前已经排队的下一事件，供 MD5 完成项合并为一次缓存查询。
    pub fn try_next_event(&mut self) -> Option<WorkerEvent> {
        self.events.try_recv().ok()
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

    /// 返回当前池已经原子登记的 CPU 权重，供调度行为和后续遥测读取。
    pub fn cpu_in_use(&self) -> usize {
        self.state.lock().unwrap().cpu_in_use
    }

    /// 返回生产池实际采用的统一 CPU 权重硬上限。
    pub fn cpu_budget(&self) -> usize {
        self.state.lock().unwrap().cpu_budget
    }

    /// 根据媒体类型返回单项显式解码线程数；未知、图片和其他媒体固定为一。
    pub fn decoder_threads_for(&self, media_kind: MediaKind) -> u32 {
        let state = self.state.lock().unwrap();
        decoder_threads_for_state(&state, media_kind)
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
        Self::controlled_batch_with_cpu_budget_for_test(worker_count, worker_count)
    }

    /// 创建带显式 CPU 权重预算的可控多槽池，验证加权调度和资源生命周期。
    #[doc(hidden)]
    pub fn controlled_batch_with_cpu_budget_for_test(
        worker_count: usize,
        cpu_budget: usize,
    ) -> (Self, mpsc::Receiver<(String, String)>, ControlledWorkerPool) {
        Self::controlled_batch_inner(worker_count, cpu_budget, true)
    }

    /// 创建延迟 Started 遥测的可控池，精确观察已派发但尚未登记 slot 的解码 ownership。
    #[doc(hidden)]
    pub fn controlled_batch_without_started_event_for_test(
        worker_count: usize,
    ) -> (Self, mpsc::Receiver<(String, String)>, ControlledWorkerPool) {
        Self::controlled_batch_inner(worker_count, worker_count, false)
    }

    /// 复用可控池实现，并允许测试选择是否发送 Started 遥测事件。
    fn controlled_batch_inner(
        worker_count: usize,
        cpu_budget: usize,
        emit_started_events: bool,
    ) -> (Self, mpsc::Receiver<(String, String)>, ControlledWorkerPool) {
        assert!(worker_count > 0);
        assert!(cpu_budget > 0);
        let state = Arc::new(Mutex::new(PoolState::new(cpu_budget)));
        for slot in 0..worker_count {
            state
                .lock()
                .unwrap()
                .process_ids
                .insert(slot, slot as u32 + 1);
        }
        let actor_state = state.clone();
        let (commands, mut command_rx) = mpsc::channel(64);
        // 覆盖每个槽位的 Started 与终态，避免可控池用 try_send 丢失 Worker 详情事件。
        let (events, event_rx) = mpsc::channel(worker_count.saturating_mul(4).max(64));
        let (started, started_rx) = mpsc::channel(16);
        let (control_tx, mut control_rx) = mpsc::channel(16);
        let available_slots = Arc::new(AtomicUsize::new(worker_count));
        let actor_available = available_slots.clone();
        let started_base_commands = Arc::new(Mutex::new(Vec::new()));
        let actor_started_base_commands = Arc::clone(&started_base_commands);
        let cancel_gate: Arc<Mutex<Option<(Arc<Notify>, Arc<Notify>)>>> =
            Arc::new(Mutex::new(None));
        let actor_cancel_gate = Arc::clone(&cancel_gate);
        let actor = tokio::spawn(async move {
            let mut queue = VecDeque::new();
            let mut idle = (0..worker_count).collect::<VecDeque<_>>();
            let mut active = HashMap::<String, (usize, WorkIdentity)>::new();
            let mut next_enqueue_sequence = 0_u64;
            loop {
                tokio::select! {
                    command = command_rx.recv() => {
                        let Some(command) = command else { break };
                        match command {
                            PoolCommand::Dispatch(envelope, guard, reply) => {
                                let result = WorkItem::try_from(envelope).and_then(|mut work| {
                                    work.set_scan_guard(guard);
                                    work.prepare_for_queue(next_enqueue_sequence, cpu_budget)?;
                                    next_enqueue_sequence = next_enqueue_sequence.wrapping_add(1);
                                    queue.push_back(work);
                                    controlled_schedule(
                                        &mut queue, &mut idle, &mut active, &started,
                                        &events, &actor_state, &actor_available,
                                        &actor_started_base_commands, emit_started_events,
                                    );
                                    Ok(())
                                });
                                let _ = reply.send(result);
                            }
                            PoolCommand::Cancel(task_id, reply) => {
                                let mut cancelled = Vec::new();
                                queue.retain(|work| {
                                    if work.identity.task_id == task_id {
                                        cancelled.push((
                                            work.identity.task_id.clone(),
                                            work.identity.item_id.clone(),
                                        ));
                                        false
                                    } else {
                                        true
                                    }
                                });
                                let active_items = active
                                    .iter()
                                    .filter(|(_, (_, identity))| identity.task_id == task_id)
                                    .map(|(item_id, (slot, identity))| {
                                        (item_id.clone(), *slot, identity.task_id.clone())
                                    })
                                    .collect::<Vec<_>>();
                                {
                                    let mut state = actor_state.lock().unwrap();
                                    state.cancelled_tasks.insert(task_id.clone());
                                    for (_, slot, _) in &active_items {
                                        release_running_work(&mut state, *slot);
                                    }
                                }
                                for (item_id, slot, event_task) in active_items {
                                    active.remove(&item_id);
                                    if !idle.contains(&slot) {
                                        idle.push_back(slot);
                                    }
                                    cancelled.push((event_task, item_id));
                                }
                                actor_available.store(idle.len(), Ordering::Release);
                                for (event_task, item_id) in cancelled {
                                    let _ = events
                                        .send(WorkerEvent::Cancelled {
                                            task_id: event_task,
                                            item_id,
                                        })
                                        .await;
                                }
                                controlled_schedule(
                                    &mut queue, &mut idle, &mut active, &started,
                                    &events, &actor_state, &actor_available,
                                    &actor_started_base_commands, emit_started_events,
                                );
                                let gate = actor_cancel_gate.lock().unwrap().take();
                                if let Some((stopped, release_ack)) = gate {
                                    stopped.notify_one();
                                    release_ack.notified().await;
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
                            PoolCommand::TerminateUnexpected(_, reply) => { let _ = reply.send(Ok(())); }
                            PoolCommand::Shutdown(reply) => {
                                queue.clear();
                                active.clear();
                                idle.clear();
                                let mut state = actor_state.lock().unwrap();
                                state.running.clear();
                                state.process_ids.clear();
                                state.cpu_in_use = 0;
                                actor_available.store(0, Ordering::Release);
                                let _ = reply.send(Ok(()));
                                break;
                            }
                        }
                    }
                    control = control_rx.recv() => {
                        let Some(control) = control else { break };
                        let (task_id, item_id) = match &control {
                            ControlledWorkerCommand::PhaseChanged { task_id, item_id, .. }
                            | ControlledWorkerCommand::BaseSourceReadComplete { task_id, item_id }
                            | ControlledWorkerCommand::CompleteBase { task_id, item_id, .. }
                            | ControlledWorkerCommand::Crash { task_id, item_id, .. }
                            | ControlledWorkerCommand::Complete { task_id, item_id, .. }
                            | ControlledWorkerCommand::CompleteStage2 { task_id, item_id, .. } => {
                                (task_id.clone(), item_id.clone())
                            }
                        };
                        let Some((slot, identity)) = active.get(&item_id).cloned() else { continue };
                        if identity.task_id != task_id { continue; }
                        let terminal = !matches!(
                            &control,
                            ControlledWorkerCommand::PhaseChanged { .. }
                                | ControlledWorkerCommand::BaseSourceReadComplete { .. }
                        );
                        let event = match control {
                            ControlledWorkerCommand::PhaseChanged { phase, .. } => {
                                WorkerEvent::PhaseChanged {
                                    task_id: task_id.clone(),
                                    item_id: item_id.clone(),
                                    slot: slot as u32,
                                    phase,
                                    request_elapsed_us: None,
                                }
                            }
                            ControlledWorkerCommand::BaseSourceReadComplete { .. } => {
                                WorkerEvent::BaseSourceReadComplete {
                                    task_id: task_id.clone(),
                                    item_id: item_id.clone(),
                                    slot: slot as u32,
                                    request_elapsed_us: None,
                                }
                            }
                            ControlledWorkerCommand::CompleteBase { md5, output, .. } => {
                                WorkerEvent::Completed {
                                    task_id: task_id.clone(),
                                    item_id: item_id.clone(),
                                    response: proto::WorkerEnvelope {
                                        payload: Some(worker_envelope::Payload::BaseComputeResult(
                                            proto::BaseComputeResult {
                                                task_id: task_id.clone(),
                                                item_id: item_id.clone(),
                                                md5: md5.to_vec(),
                                                payload: encode_base_compute_payload(&output),
                                            },
                                        )),
                                    },
                                }
                            }
                            ControlledWorkerCommand::Crash { message, .. } => {
                                match identity.file_identity.clone() {
                                    Some(file_identity) => WorkerEvent::Crashed {
                                        task_id: task_id.clone(),
                                        item_id: item_id.clone(),
                                        identity: file_identity,
                                        process_id: Some(slot as u32 + 1),
                                        exit_code: None,
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
                            ControlledWorkerCommand::CompleteStage2 { output, .. } => {
                                WorkerEvent::Completed {
                                    task_id: task_id.clone(),
                                    item_id: item_id.clone(),
                                    response: proto::WorkerEnvelope {
                                        payload: Some(worker_envelope::Payload::Stage2Result(
                                            proto::Stage2Result {
                                                task_id: task_id.clone(),
                                                item_id: item_id.clone(),
                                                payload: encode_stage2_payload(&output),
                                            },
                                        )),
                                    },
                                }
                            }
                        };
                        if terminal {
                            active.remove(&item_id);
                            let mut state = actor_state.lock().unwrap();
                            release_running_work(&mut state, slot);
                            drop(state);
                            idle.push_back(slot);
                            actor_available.store(idle.len(), Ordering::Release);
                        }
                        let _ = events.send(event).await;
                        if terminal {
                            controlled_schedule(
                                &mut queue, &mut idle, &mut active, &started,
                                &events, &actor_state, &actor_available,
                                &actor_started_base_commands, emit_started_events,
                            );
                        }
                    }
                }
            }
        });
        (
            Self {
                commands,
                events: event_rx,
                state: state.clone(),
                actor,
            },
            started_rx,
            ControlledWorkerPool {
                commands: control_tx,
                available_slots,
                state,
                started_base_commands,
                cancel_gate,
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
        let state = Arc::new(Mutex::new(PoolState::new(1)));
        // 单槽可控池必须登记一个测试进程，避免调用方把真实单 Worker 夹具误判为零容量。
        state.lock().unwrap().process_ids.insert(0, 1);
        let actor_state = Arc::clone(&state);
        let (commands, mut command_rx) = mpsc::channel(64);
        let (events, event_rx) = mpsc::channel(256);
        let (started, started_rx) = mpsc::channel(8);
        let actor = tokio::spawn(async move {
            let mut active = None::<WorkIdentity>;
            while let Some(command) = command_rx.recv().await {
                match command {
                    PoolCommand::Dispatch(envelope, guard, reply) => {
                        let result = WorkItem::try_from(envelope).and_then(|mut work| {
                            work.set_scan_guard(guard);
                            work.prepare_for_queue(0, 1)?;
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
                                return Ok(());
                            }
                            if let Some(barrier) = &dispatch_barrier {
                                barrier.block_before_send();
                            }
                            active = Some(work.identity.clone());
                            register_running_work(&mut locked, 0, work.identity.clone());
                            let _ = started.try_send((
                                work.identity.task_id.clone(),
                                work.identity.item_id.clone(),
                            ));
                            Ok(())
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
                            {
                                let mut locked = actor_state.lock().unwrap();
                                release_running_work(&mut locked, 0);
                            }
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
                    PoolCommand::TerminateUnexpected(_, reply) => {
                        let _ = reply.send(Ok(()));
                    }
                    PoolCommand::Shutdown(reply) => {
                        let mut state = actor_state.lock().unwrap();
                        state.running.clear();
                        state.process_ids.clear();
                        state.cpu_in_use = 0;
                        let _ = reply.send(Ok(()));
                        break;
                    }
                }
            }
        });
        (
            Self {
                commands,
                events: event_rx,
                state,
                actor,
            },
            started_rx,
        )
    }
}

/// 按老化保留与 `(CPU 权重, 文件大小, 入队序号)` 选择当前可容纳的等待项。
fn take_next_schedulable_work(
    queue: &mut VecDeque<WorkItem>,
    cpu_available: usize,
) -> Option<WorkItem> {
    let protected = queue
        .iter()
        .enumerate()
        .filter(|(_, work)| work.cost.bypass_count >= CPU_AGING_BYPASS_LIMIT)
        .min_by_key(|(_, work)| work.cost.enqueue_sequence)
        .map(|(index, _)| index);
    let selected = if let Some(index) = protected {
        if queue[index].cost.cpu_weight > cpu_available {
            return None;
        }
        index
    } else {
        queue
            .iter()
            .enumerate()
            .filter(|(_, work)| work.cost.cpu_weight <= cpu_available)
            .min_by_key(|(_, work)| {
                (
                    work.cost.cpu_weight,
                    work.cost.file_size,
                    work.cost.enqueue_sequence,
                )
            })
            .map(|(index, _)| index)?
    };
    queue.remove(selected)
}

/// 一项成功取得 slot 与 CPU 后，给仍在等待的所有任务累计一次绕过。
fn mark_waiters_bypassed(queue: &mut VecDeque<WorkItem>) {
    for waiting in queue.iter_mut() {
        waiting.cost.bypass_count = waiting.cost.bypass_count.saturating_add(1);
    }
}

/// 让可控池与真实池共享成本选择、CPU 登记和释放规则。
fn controlled_schedule(
    queue: &mut VecDeque<WorkItem>,
    idle: &mut VecDeque<usize>,
    active: &mut HashMap<String, (usize, WorkIdentity)>,
    started: &mpsc::Sender<(String, String)>,
    events: &mpsc::Sender<WorkerEvent>,
    state: &Arc<Mutex<PoolState>>,
    available_slots: &Arc<AtomicUsize>,
    started_base_commands: &Arc<Mutex<Vec<proto::ComputeBaseFeatures>>>,
    emit_started_events: bool,
) {
    while !idle.is_empty() && !queue.is_empty() {
        let cpu_available = {
            let state = state.lock().unwrap();
            state.cpu_budget - state.cpu_in_use
        };
        let Some(work) = take_next_schedulable_work(queue, cpu_available) else {
            break;
        };
        let blocked = {
            let state = state.lock().unwrap();
            state.cancelled_tasks.contains(&work.identity.task_id)
                || state.cancelling_tasks.contains(&work.identity.task_id)
                || !work.dispatch_allowed()
        };
        if blocked {
            let _ = events.try_send(WorkerEvent::Cancelled {
                task_id: work.identity.task_id,
                item_id: work.identity.item_id,
            });
            continue;
        }
        let slot = idle.pop_front().expect("空闲槽已确认存在");
        if let Some(worker_envelope::Payload::ComputeBaseFeatures(command)) =
            work.envelope.payload.as_ref()
        {
            started_base_commands.lock().unwrap().push(command.clone());
        }
        let queue_wait_us = work
            .cost
            .enqueued_at
            .elapsed()
            .as_micros()
            .try_into()
            .unwrap_or(u64::MAX);
        let identity = work.identity;
        register_running_work(&mut state.lock().unwrap(), slot, identity.clone());
        mark_waiters_bypassed(queue);
        active.insert(identity.item_id.clone(), (slot, identity.clone()));
        if emit_started_events && let Some(file_identity) = identity.file_identity.clone() {
            let process_id = state.lock().unwrap().process_ids.get(&slot).copied();
            let _ = events.try_send(WorkerEvent::Started {
                task_id: identity.task_id.clone(),
                item_id: identity.item_id.clone(),
                slot: slot as u32,
                process_id,
                identity: file_identity,
                cpu_weight: identity.cpu_weight.try_into().unwrap_or(u32::MAX),
                decoder_threads: identity.decoder_threads,
                queue_wait_us,
            });
        }
        let _ = started.try_send((identity.task_id, identity.item_id));
        available_slots.store(idle.len(), Ordering::Release);
    }
}

/// Worker 池启动、请求或关闭收束错误。
#[derive(Debug, Error)]
pub enum WorkerPoolError {
    /// 配置至少需要一个 Worker。
    #[error("Worker 数量必须大于零")]
    EmptyPool,
    /// CPU 权重预算必须至少为一。
    #[error("Worker CPU 权重预算必须大于零")]
    EmptyCpuBudget,
    /// Worker 进程创建或 Ready 握手失败。
    #[error("Worker 进程失败: {0}")]
    Process(String),
    /// Job Object 创建失败。
    #[error("创建 Worker Job Object 失败: {0}")]
    Job(String),
    /// actor 已结束。
    #[error("WorkerPool 已关闭")]
    Closed,
    /// Envelope 不是三类 Worker 请求之一。
    #[error("不是有效的 Worker 请求")]
    InvalidRequest,
    /// 请求的媒体类型和 FFmpeg 解码线程数不构成有效 CPU 权重。
    #[error("无效的解码线程数: media_kind={media_kind}, decoder_threads={decoder_threads}")]
    InvalidDecoderThreads {
        /// 协议媒体枚举原值。
        media_kind: i32,
        /// 请求携带的线程数。
        decoder_threads: u32,
    },
    /// 单项 CPU 权重大于池总预算，永远无法得到调度。
    #[error("请求 CPU 权重 {weight} 超过池预算 {budget}")]
    CpuWeightExceedsBudget {
        /// 请求 CPU 权重。
        weight: usize,
        /// 池 CPU 总预算。
        budget: usize,
    },
    /// Worker 等待队列已达到由 Worker/命令容量推导出的边界。
    #[error("Worker 等待队列已满: capacity={capacity}")]
    QueueFull {
        /// 等待队列允许的最大项数。
        capacity: usize,
    },
    /// 诊断请求指定的 PID 不属于当前池。
    #[error("Worker PID 不存在: {0}")]
    WorkerNotFound(u32),
    /// 关闭时等待 slot 的退出事件或 driver 收束超过上限。
    #[error("WorkerPool 关闭超时: {timeout:?}")]
    ShutdownTimeout {
        /// 固定关闭上限，超时后会终止全部残余 driver。
        timeout: Duration,
    },
    /// slot 控制通道已经关闭，无法发送终止命令。
    #[error("Worker slot 控制通道已关闭: slot={slot_id}")]
    ShutdownSlotClosed {
        /// 无法正常停止的槽位编号。
        slot_id: usize,
    },
    /// slot driver 异常退出或 join 失败。
    #[error("Worker slot driver 收束失败: {0}")]
    ShutdownDriver(String),
}

/// actor 与只读状态 API 共享的最小运行快照；所有写入仍只发生在 actor 中。
struct PoolState {
    running: HashMap<usize, WorkIdentity>,
    process_ids: BTreeMap<usize, u32>,
    cpu_budget: usize,
    cpu_in_use: usize,
    failure_count: u64,
    cancelled_tasks: HashSet<String>,
    cancelling_tasks: HashSet<String>,
}

impl PoolState {
    /// 使用非零 CPU 预算创建完整状态，避免测试或生产得到不可调度的零值池。
    fn new(cpu_budget: usize) -> Self {
        assert!(cpu_budget > 0, "CPU 权重预算必须大于零");
        Self {
            running: HashMap::new(),
            process_ids: BTreeMap::new(),
            cpu_budget,
            cpu_in_use: 0,
            failure_count: 0,
            cancelled_tasks: HashSet::new(),
            cancelling_tasks: HashSet::new(),
        }
    }
}

/// 在发送 Run 成功后同时登记槽位和 CPU 权重；调用方必须持有状态锁。
fn register_running_work(state: &mut PoolState, slot_id: usize, identity: WorkIdentity) {
    let next_cpu = state
        .cpu_in_use
        .checked_add(identity.cpu_weight)
        .expect("CPU 权重计数溢出");
    assert!(next_cpu <= state.cpu_budget, "调度不得突破 CPU 权重预算");
    state.cpu_in_use = next_cpu;
    state.running.insert(slot_id, identity);
}

/// 仅在槽位仍登记运行项时释放一次 CPU 权重，并返回该项身份。
fn release_running_work(state: &mut PoolState, slot_id: usize) -> Option<WorkIdentity> {
    let work = state.running.remove(&slot_id)?;
    state.cpu_in_use = state
        .cpu_in_use
        .checked_sub(work.cpu_weight)
        .expect("CPU 权重释放发生下溢");
    Some(work)
}

/// 根据池进程数和 CPU 预算计算已知视频的单项线程数。
fn decoder_threads_for_state(state: &PoolState, media_kind: MediaKind) -> u32 {
    if media_kind != MediaKind::Video {
        return 1;
    }
    let worker_count = state.process_ids.len().max(1);
    let max_active = worker_count.min(state.cpu_budget).max(1);
    u32::try_from((state.cpu_budget / max_active).clamp(1, 4)).unwrap_or(4)
}

#[derive(Clone, Debug)]
/// Worker 运行项归属，持续携带文件路径上下文直到终态事件取得所有权。
struct WorkIdentity {
    task_id: String,
    item_id: String,
    cpu_weight: usize,
    /// 一次性基础媒体请求的显式解码线程数；其他请求缺失。
    decoder_threads: Option<u32>,
    file_identity: Option<WorkerFileIdentity>,
}

/// 更新 Node 保存的 Worker 当前处理阶段；文件路径及大小身份保持冻结。
fn update_work_stage(work: &mut WorkIdentity, stage: &str) {
    if let Some(identity) = work.file_identity.as_mut() {
        identity.stage = stage.to_owned();
    }
}

/// 只用显式 Worker phase 更新槽位崩溃上下文；SourceComplete 不推断阶段。
fn update_slot_work_from_phase_response(work: &mut WorkIdentity, response: &proto::WorkerEnvelope) {
    let Some(worker_envelope::Payload::WorkerPhaseChanged(event)) = response.payload.as_ref()
    else {
        return;
    };
    if event.task_id != work.task_id || event.item_id != work.item_id {
        return;
    }
    if let Some(phase) = valid_worker_phase(event.phase) {
        update_work_stage(work, worker_phase_stage(phase));
    }
}

/// 将显式 Worker phase 映射到崩溃文件上下文使用的稳定阶段名。
const fn worker_phase_stage(phase: proto::RuntimeWorkerPhase) -> &'static str {
    match phase {
        proto::RuntimeWorkerPhase::RuntimeWorkerIdle => "idle",
        proto::RuntimeWorkerPhase::RuntimeWorkerDecode => "base_decode",
        proto::RuntimeWorkerPhase::RuntimeWorkerFeature => "base_feature",
        proto::RuntimeWorkerPhase::RuntimeWorkerResultWait => "base_result_wait",
        proto::RuntimeWorkerPhase::Unspecified => "base_compute",
    }
}

/// 等待调度的一条完整协议请求及其任务归属。
struct WorkItem {
    identity: WorkIdentity,
    envelope: proto::WorkerEnvelope,
    scan_guard: Option<ScanDispatchGuard>,
    cost: WorkCost,
}

/// 单项调度成本；序号和绕过次数只由池 actor 更新。
struct WorkCost {
    cpu_weight: usize,
    file_size: u64,
    enqueue_sequence: u64,
    bypass_count: usize,
    /// 请求进入 WorkerPool 有界等待队列的真实单调时刻。
    enqueued_at: Instant,
}

struct ScanDispatchGuard {
    cancellation: Option<ReadCancellationToken>,
    persisted_active: bool,
    file_identity: WorkerFileIdentity,
}

impl WorkItem {
    fn set_scan_guard(&mut self, guard: Option<ScanDispatchGuard>) {
        if let Some(guard) = &guard {
            self.identity.file_identity = Some(guard.file_identity.clone());
            if self.cost.file_size == 0 {
                self.cost.file_size = guard.file_identity.file_size;
            }
        }
        self.scan_guard = guard;
    }

    /// 在真正入队前分配稳定序号并拒绝永远无法容纳的 CPU 权重。
    fn prepare_for_queue(
        &mut self,
        enqueue_sequence: u64,
        cpu_budget: usize,
    ) -> Result<(), WorkerPoolError> {
        if self.cost.cpu_weight > cpu_budget {
            return Err(WorkerPoolError::CpuWeightExceedsBudget {
                weight: self.cost.cpu_weight,
                budget: cpu_budget,
            });
        }
        self.cost.enqueue_sequence = enqueue_sequence;
        Ok(())
    }

    fn dispatch_allowed(&self) -> bool {
        self.scan_guard.as_ref().is_none_or(|guard| {
            guard.persisted_active
                && guard
                    .cancellation
                    .as_ref()
                    .is_none_or(|cancellation| !cancellation.is_cancelled())
        })
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
    TerminateUnexpected(u32, oneshot::Sender<Result<(), WorkerPoolError>>),
    /// 关闭命令独占 actor，完成全部 slot 退出和 driver 收束后才回复。
    Shutdown(oneshot::Sender<Result<(), WorkerPoolError>>),
}

/// 一个已经 Ready 的槽位，只暴露 PID 与单向控制通道。
struct SlotHandle {
    process_id: u32,
    commands: mpsc::UnboundedSender<SlotCommand>,
    /// 唯一 driver 任务；Pool 关闭必须等待它返回，不能只依赖 Exited 事件。
    driver: tokio::task::JoinHandle<()>,
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
        process_id: Option<u32>,
        exit_code: Option<i32>,
        message: String,
    },
}

#[allow(clippy::too_many_arguments)]
/// 独占队列、槽位和 Job 的池 actor；外部方法只通过消息读取或改变状态。
async fn run_pool(
    config: WorkerPoolConfig,
    job: WorkerJob,
    slots: BTreeMap<usize, SlotHandle>,
    idle: VecDeque<usize>,
    commands: mpsc::Receiver<PoolCommand>,
    slot_events: mpsc::UnboundedReceiver<SlotEvent>,
    slot_events_tx: mpsc::UnboundedSender<SlotEvent>,
    events: WorkerEventOutbox,
    state: Arc<Mutex<PoolState>>,
) {
    let replacement_config = config.clone();
    let replacement_job = Arc::new(job);
    let replacement_events = slot_events_tx.clone();
    let factory_config = replacement_config.clone();
    let factory_job = Arc::clone(&replacement_job);
    let factory_events = replacement_events.clone();
    let replacement_factory = move |slot_id| {
        let config = factory_config.clone();
        let job = Arc::clone(&factory_job);
        let events = factory_events.clone();
        async move { spawn_slot(slot_id, &config, &job, events).await }
    };
    run_pool_with_replacement(
        config,
        replacement_job,
        slots,
        idle,
        commands,
        slot_events,
        slot_events_tx,
        events,
        state,
        replacement_factory,
    )
    .await;
}

#[allow(clippy::too_many_arguments)]
/// 独占池状态的 actor 实现；替换工厂可注入确定性测试槽位。
async fn run_pool_with_replacement<F, Fut>(
    config: WorkerPoolConfig,
    job: Arc<WorkerJob>,
    mut slots: BTreeMap<usize, SlotHandle>,
    mut idle: VecDeque<usize>,
    mut commands: mpsc::Receiver<PoolCommand>,
    mut slot_events: mpsc::UnboundedReceiver<SlotEvent>,
    slot_events_tx: mpsc::UnboundedSender<SlotEvent>,
    events: WorkerEventOutbox,
    state: Arc<Mutex<PoolState>>,
    mut replacement_factory: F,
) where
    F: FnMut(usize) -> Fut,
    Fut: Future<Output = Result<SlotHandle, WorkerPoolError>>,
{
    let mut queue = VecDeque::new();
    let mut next_enqueue_sequence = 0_u64;
    let pending_events = Arc::new(PendingWorkerEvents::new(actor_event_buffer_capacity(
        config.worker_count,
    )));
    let actor_events = ActorWorkerEventSink::new(Arc::clone(&pending_events), events.clone());
    loop {
        pending_events.try_flush(&events);
        tokio::select! {
            biased;
            command = commands.recv() => {
                let Some(command) = command else { break };
                match command {
                    PoolCommand::Dispatch(envelope, guard, reply) => {
                        let result = match WorkItem::try_from(envelope) {
                                Ok(mut work) => {
                                    work.set_scan_guard(guard);
                                    let cpu_budget = state.lock().unwrap().cpu_budget;
                                    schedule(&mut queue, &mut idle, &slots, &actor_events, &state).await;
                                    let queue_capacity = max_waiting_work_items(config.worker_count);
                                    if queue.len() >= queue_capacity {
                                        Err(WorkerPoolError::QueueFull {
                                            capacity: queue_capacity,
                                        })
                                    } else {
                                        match work.prepare_for_queue(next_enqueue_sequence, cpu_budget) {
                                            Ok(()) => {
                                                next_enqueue_sequence = next_enqueue_sequence.wrapping_add(1);
                                                queue.push_back(work);
                                                schedule(&mut queue, &mut idle, &slots, &actor_events, &state).await;
                                                Ok(())
                                            }
                                            Err(error) => Err(error),
                                        }
                                    }
                                }
                                Err(error) => Err(error),
                            };
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
                            job.as_ref(),
                            &mut queue,
                            &mut slots,
                            &mut idle,
                            &mut slot_events,
                            &slot_events_tx,
                            &actor_events,
                            &state,
                            &mut replacement_factory,
                        ).await;
                        schedule(&mut queue, &mut idle, &slots, &actor_events, &state).await;
                        let _ = reply.send(result);
                    }
                    PoolCommand::CancelRollback(task_id) => {
                        state
                            .lock()
                            .unwrap()
                            .cancelling_tasks
                            .remove(&task_id);
                        schedule(&mut queue, &mut idle, &slots, &actor_events, &state).await;
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
                    PoolCommand::Shutdown(reply) => {
                        let result = shutdown_slots(
                            &mut slots,
                            &mut slot_events,
                            &state,
                            config.shutdown_timeout,
                        )
                        .await;
                        let _ = reply.send(result);
                        break;
                    }
                }
            }
            event = slot_events.recv() => {
                let Some(event) = event else { break };
                handle_slot_event_and_schedule_with_replacement(
                    event,
                    &mut queue,
                    &mut slots,
                    &mut idle,
                    &actor_events,
                    &state,
                    config.shutdown_timeout,
                    &mut replacement_factory,
                ).await;
            }
            _ = events.wait_for_progress(), if pending_events.has_pending() => {
                pending_events.try_flush(&events);
            }
        }
    }
}

/// 终止并回收所有 slot；必须同时收到退出事件并 join driver，才允许池 actor 退出。
async fn shutdown_slots(
    slots: &mut BTreeMap<usize, SlotHandle>,
    slot_events: &mut mpsc::UnboundedReceiver<SlotEvent>,
    state: &Arc<Mutex<PoolState>>,
    timeout: Duration,
) -> Result<(), WorkerPoolError> {
    let mut retiring = std::mem::take(slots);
    let deadline = tokio::time::Instant::now() + timeout;
    let mut waiting = HashSet::new();
    let mut result = Ok(());
    for (slot_id, slot) in &retiring {
        if slot.commands.send(SlotCommand::Terminate).is_ok() {
            waiting.insert(*slot_id);
        } else {
            result = Err(WorkerPoolError::ShutdownSlotClosed { slot_id: *slot_id });
            break;
        }
    }
    while result.is_ok() && !waiting.is_empty() {
        match tokio::time::timeout_at(deadline, slot_events.recv()).await {
            Ok(Some(SlotEvent::Exited { slot_id, .. })) if waiting.remove(&slot_id) => {
                let mut locked = state.lock().unwrap();
                release_running_work(&mut locked, slot_id);
                locked.process_ids.remove(&slot_id);
            }
            Ok(Some(_)) => {}
            Ok(None) => result = Err(WorkerPoolError::Closed),
            Err(_) => result = Err(WorkerPoolError::ShutdownTimeout { timeout }),
        }
    }
    let slot_ids = retiring.keys().copied().collect::<Vec<_>>();
    for slot_id in slot_ids {
        if result.is_err() {
            break;
        }
        let joined = {
            let slot = retiring.get_mut(&slot_id).expect("待关闭 slot 必须存在");
            tokio::time::timeout_at(deadline, &mut slot.driver).await
        };
        match joined {
            Ok(Ok(())) => {
                retiring.remove(&slot_id);
            }
            Ok(Err(error)) => result = Err(WorkerPoolError::ShutdownDriver(error.to_string())),
            Err(_) => result = Err(WorkerPoolError::ShutdownTimeout { timeout }),
        }
    }
    // 异常路径不能留下 detached driver：先中止，再等待每个 JoinHandle 收束。
    if result.is_err() {
        for slot in retiring.values() {
            slot.driver.abort();
        }
    }
    for (_, slot) in retiring {
        let _ = slot.driver.await;
    }
    let mut locked = state.lock().unwrap();
    locked.running.clear();
    locked.process_ids.clear();
    locked.cpu_in_use = 0;
    result
}

/// 在给定 deadline 前等待退出 driver；超时或异常时中止并等待，禁止 Drop 后 detached。
async fn join_retired_slot_until(
    mut slot: SlotHandle,
    deadline: tokio::time::Instant,
    timeout: Duration,
) -> Result<(), WorkerPoolError> {
    match tokio::time::timeout_at(deadline, &mut slot.driver).await {
        Ok(Ok(())) => Ok(()),
        Ok(Err(error)) => Err(WorkerPoolError::ShutdownDriver(error.to_string())),
        Err(_) => {
            slot.driver.abort();
            let _ = slot.driver.await;
            Err(WorkerPoolError::ShutdownTimeout { timeout })
        }
    }
}

/// 无法继续等待退出事件时立即收束一个 driver，仍必须 await 避免遗留 detached task。
async fn abort_retired_slot(slot: SlotHandle) {
    slot.driver.abort();
    let _ = slot.driver.await;
}

/// 按空闲完成顺序 round-robin 派发等待项，并原子更新共享运行快照。
async fn schedule<E: WorkerEventSink>(
    queue: &mut VecDeque<WorkItem>,
    idle: &mut VecDeque<usize>,
    slots: &BTreeMap<usize, SlotHandle>,
    events: &E,
    state: &Arc<Mutex<PoolState>>,
) {
    enum DispatchDecision {
        Sent,
        Rejected(WorkIdentity),
        Retry(WorkItem),
        Cancelling(WorkItem),
    }
    while !idle.is_empty() && !queue.is_empty() {
        let cpu_available = {
            let locked = state.lock().unwrap();
            locked.cpu_budget - locked.cpu_in_use
        };
        let Some(work) = take_next_schedulable_work(queue, cpu_available) else {
            break;
        };
        let queue_wait_us = work
            .cost
            .enqueued_at
            .elapsed()
            .as_micros()
            .try_into()
            .unwrap_or(u64::MAX);
        let slot_id = idle.pop_front().expect("空闲槽已确认存在");
        let identity = work.identity.clone();
        let Some(slot) = slots.get(&slot_id) else {
            queue.push_front(work);
            continue;
        };
        let decision = {
            let mut locked = state.lock().unwrap();
            if locked.cancelling_tasks.contains(&identity.task_id) {
                DispatchDecision::Cancelling(work)
            } else if locked.cancelled_tasks.contains(&identity.task_id) || !work.dispatch_allowed()
            {
                DispatchDecision::Rejected(identity.clone())
            } else if let Err(error) = slot.commands.send(SlotCommand::Run(work)) {
                let SlotCommand::Run(work) = error.0 else {
                    unreachable!("schedule only sends Run")
                };
                DispatchDecision::Retry(work)
            } else {
                register_running_work(&mut locked, slot_id, identity.clone());
                DispatchDecision::Sent
            }
        };
        match decision {
            DispatchDecision::Sent => {
                mark_waiters_bypassed(queue);
                if let Some(identity_file) = identity.file_identity.clone() {
                    events
                        .send_event(WorkerEvent::Started {
                            task_id: identity.task_id,
                            item_id: identity.item_id,
                            slot: slot_id as u32,
                            process_id: Some(slot.process_id),
                            identity: identity_file,
                            cpu_weight: identity.cpu_weight.try_into().unwrap_or(u32::MAX),
                            decoder_threads: identity.decoder_threads,
                            queue_wait_us,
                        })
                        .await;
                }
            }
            DispatchDecision::Rejected(identity) => {
                idle.push_back(slot_id);
                events
                    .send_event(WorkerEvent::Cancelled {
                        task_id: identity.task_id,
                        item_id: identity.item_id,
                    })
                    .await;
            }
            DispatchDecision::Retry(work) => {
                // 命令接收端关闭表示该槽已失效；等待 Exited 统一移除并补建，不能再次进入 idle。
                queue.push_front(work);
            }
            DispatchDecision::Cancelling(work) => {
                idle.push_back(slot_id);
                queue.push_front(work);
                return;
            }
        }
    }
}

/// 从空闲 FIFO 中移除指定槽位，保留其余槽位原有轮转顺序。
fn remove_idle_slot(idle: &mut VecDeque<usize>, slot_id: usize) {
    if let Some(position) = idle.iter().position(|candidate| *candidate == slot_id) {
        idle.remove(position);
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
    let driver = tokio::spawn(run_slot(
        slot_id,
        process,
        commands_rx,
        events,
        config.result_read_delay,
    ));
    Ok(SlotHandle {
        process_id,
        commands: commands_tx,
        driver,
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
        let (mut work, envelope) = match command {
            SlotCommand::Terminate => {
                let process_id = Some(process.process_id());
                let termination = process.terminate().await;
                let exit_code = termination.as_ref().ok().copied().flatten();
                let message = termination
                    .err()
                    .map_or_else(|| "planned termination".into(), |error| error.to_string());
                let _ = events.send(SlotEvent::Exited {
                    slot_id,
                    work: None,
                    process_id,
                    exit_code,
                    message,
                });
                return;
            }
            SlotCommand::Run(work) => {
                let WorkItem {
                    identity, envelope, ..
                } = work;
                (identity, envelope)
            }
        };

        if let Err(error) = process.send(&envelope).await {
            let process_id = Some(process.process_id());
            let exit_code = process.stop_after_failure().await;
            let _ = events.send(SlotEvent::Exited {
                slot_id,
                work: Some(work),
                process_id,
                exit_code,
                message: error.to_string(),
            });
            return;
        }
        if !result_read_delay.is_zero() {
            tokio::select! {
                _ = tokio::time::sleep(result_read_delay) => {}
                command = commands.recv() => {
                    if matches!(command, Some(SlotCommand::Terminate)) {
                        let process_id = Some(process.process_id());
                        let termination = process.terminate().await;
                        let exit_code = termination.as_ref().ok().copied().flatten();
                        let message = termination.err().map_or_else(
                            || "planned termination".into(), |error| error.to_string()
                        );
                        let _ = events.send(SlotEvent::Exited {
                            slot_id,
                            work: Some(work.clone()),
                            process_id,
                            exit_code,
                            message,
                        });
                    }
                    return;
                }
            }
        }
        loop {
            tokio::select! {
                response = process.receive() => {
                    match response {
                        Ok(response) => {
                            let keep_receiving = matches!(
                                response.payload.as_ref(),
                                Some(worker_envelope::Payload::BaseSourceReadComplete(source))
                                    if source.task_id == work.task_id
                                        && source.item_id == work.item_id
                            ) || matches!(
                                response.payload.as_ref(),
                                Some(worker_envelope::Payload::WorkerPhaseChanged(phase))
                                    if phase.task_id == work.task_id
                                        && phase.item_id == work.item_id
                                        && valid_worker_phase(phase.phase).is_some()
                            );
                            update_slot_work_from_phase_response(&mut work, &response);
                            let _ = events.send(SlotEvent::Response {
                                slot_id,
                                work: work.clone(),
                                response,
                            });
                            if !keep_receiving {
                                break;
                            }
                        }
                        Err(error) => {
                            let process_id = Some(process.process_id());
                            let exit_code = process.stop_after_failure().await;
                            let _ = events.send(SlotEvent::Exited {
                                slot_id,
                                work: Some(work),
                                process_id,
                                exit_code,
                                message: error.to_string(),
                            });
                            return;
                        }
                    }
                }
                command = commands.recv() => {
                    if matches!(command, Some(SlotCommand::Terminate)) {
                        let process_id = Some(process.process_id());
                        let termination = process.terminate().await;
                        let exit_code = termination.as_ref().ok().copied().flatten();
                        let message = termination.err().map_or_else(
                            || "planned termination".into(), |error| error.to_string()
                        );
                        let _ = events.send(SlotEvent::Exited {
                            slot_id,
                            work: Some(work),
                            process_id,
                            exit_code,
                            message,
                        });
                    }
                    return;
                }
            }
        }
    }
}

#[allow(clippy::too_many_arguments)]
/// 处理正常响应或非计划退出；后者增加失败计数并补建同槽位 Worker。
async fn handle_slot_event<E: WorkerEventSink>(
    event: SlotEvent,
    config: &WorkerPoolConfig,
    job: &WorkerJob,
    slots: &mut BTreeMap<usize, SlotHandle>,
    idle: &mut VecDeque<usize>,
    slot_events: &mpsc::UnboundedSender<SlotEvent>,
    events: &E,
    state: &Arc<Mutex<PoolState>>,
) {
    let mut replacement_factory = |slot_id| spawn_slot(slot_id, config, job, slot_events.clone());
    handle_slot_event_with_replacement(
        event,
        slots,
        idle,
        events,
        state,
        config.shutdown_timeout,
        &mut replacement_factory,
    )
    .await;
}

#[allow(clippy::too_many_arguments)]
/// 让生产 actor 与测试共用 Exited 清理、补槽和随后自动调度的完整转换。
async fn handle_slot_event_and_schedule_with_replacement<E, F, Fut>(
    event: SlotEvent,
    queue: &mut VecDeque<WorkItem>,
    slots: &mut BTreeMap<usize, SlotHandle>,
    idle: &mut VecDeque<usize>,
    events: &E,
    state: &Arc<Mutex<PoolState>>,
    retire_timeout: Duration,
    replacement_factory: &mut F,
) where
    E: WorkerEventSink,
    F: FnMut(usize) -> Fut,
    Fut: Future<Output = Result<SlotHandle, WorkerPoolError>>,
{
    handle_slot_event_with_replacement(
        event,
        slots,
        idle,
        events,
        state,
        retire_timeout,
        replacement_factory,
    )
    .await;
    schedule(queue, idle, slots, events, state).await;
}

#[allow(clippy::too_many_arguments)]
/// 处理槽位事件，并通过可注入工厂执行与生产相同的替换安装逻辑。
async fn handle_slot_event_with_replacement<E, F, Fut>(
    event: SlotEvent,
    slots: &mut BTreeMap<usize, SlotHandle>,
    idle: &mut VecDeque<usize>,
    events: &E,
    state: &Arc<Mutex<PoolState>>,
    retire_timeout: Duration,
    replacement_factory: &mut F,
) where
    E: WorkerEventSink,
    F: FnMut(usize) -> Fut,
    Fut: Future<Output = Result<SlotHandle, WorkerPoolError>>,
{
    match event {
        SlotEvent::Response {
            slot_id,
            work,
            mut response,
        } => {
            if !response_matches_work(&response, &work) {
                response = proto::WorkerEnvelope {
                    payload: Some(worker_envelope::Payload::WorkerFailure(
                        proto::WorkerFailure {
                            task_id: work.task_id.clone(),
                            item_id: work.item_id.clone(),
                            stage: "protocol".into(),
                            message: "Worker 响应的任务或任务项身份不匹配".into(),
                        },
                    )),
                };
            }
            if let Some(worker_envelope::Payload::WorkerPhaseChanged(phase_event)) =
                response.payload.as_ref()
            {
                let phase_value = phase_event.phase;
                let request_elapsed_us = phase_event.request_elapsed_us;
                if let Some(phase) = valid_worker_phase(phase_value) {
                    {
                        let mut locked = state.lock().unwrap();
                        let mut phase_work = work.clone();
                        update_work_stage(&mut phase_work, worker_phase_stage(phase));
                        locked.running.insert(slot_id, phase_work);
                    }
                    events
                        .send_event(WorkerEvent::PhaseChanged {
                            task_id: work.task_id,
                            item_id: work.item_id,
                            slot: slot_id as u32,
                            phase,
                            request_elapsed_us,
                        })
                        .await;
                    return;
                }
                response = proto::WorkerEnvelope {
                    payload: Some(worker_envelope::Payload::WorkerFailure(
                        proto::WorkerFailure {
                            task_id: work.task_id.clone(),
                            item_id: work.item_id.clone(),
                            stage: "protocol".into(),
                            message: format!("Worker 返回非法阶段值: {phase_value}"),
                        },
                    )),
                };
            }
            if let Some(worker_envelope::Payload::BaseSourceReadComplete(source)) =
                response.payload.as_ref()
            {
                events
                    .send_event(WorkerEvent::BaseSourceReadComplete {
                        task_id: work.task_id,
                        item_id: work.item_id,
                        slot: slot_id as u32,
                        request_elapsed_us: source.request_elapsed_us,
                    })
                    .await;
                return;
            }
            {
                let mut locked = state.lock().unwrap();
                release_running_work(&mut locked, slot_id);
            }
            idle.push_back(slot_id);
            events
                .send_event(WorkerEvent::Completed {
                    task_id: work.task_id,
                    item_id: work.item_id,
                    response,
                })
                .await;
        }
        SlotEvent::Exited {
            slot_id,
            work,
            process_id,
            exit_code,
            message,
        } => {
            let retired = slots.remove(&slot_id);
            remove_idle_slot(idle, slot_id);
            let driver_error = match retired {
                Some(slot) => join_retired_slot_until(
                    slot,
                    tokio::time::Instant::now() + retire_timeout,
                    retire_timeout,
                )
                .await
                .err(),
                None => None,
            };
            let work = work.or_else(|| state.lock().unwrap().running.get(&slot_id).cloned());
            {
                let mut locked = state.lock().unwrap();
                release_running_work(&mut locked, slot_id);
                locked.process_ids.remove(&slot_id);
                locked.failure_count += 1;
            }
            if let Some(work) = work {
                if let Some(identity) = work.file_identity {
                    events
                        .send_event(WorkerEvent::Crashed {
                            task_id: work.task_id,
                            item_id: work.item_id,
                            identity,
                            process_id,
                            exit_code,
                            message,
                        })
                        .await;
                } else {
                    events
                        .send_event(WorkerEvent::InfrastructureFailure {
                            message: format!(
                                "Worker 崩溃项缺少冻结文件身份: {}/{}: {message}",
                                work.task_id, work.item_id
                            ),
                        })
                        .await;
                }
            }
            if let Some(error) = driver_error {
                events
                    .send_event(WorkerEvent::InfrastructureFailure {
                        message: format!("Worker driver 收束异常: {error}"),
                    })
                    .await;
            }
            replace_slot_with_factory(slot_id, slots, idle, events, state, replacement_factory)
                .await;
        }
    }
}

/// 校验 Worker 响应仍归属于当前占用槽位的任务项。
fn response_matches_work(response: &proto::WorkerEnvelope, work: &WorkIdentity) -> bool {
    let identity = match response.payload.as_ref() {
        Some(worker_envelope::Payload::BaseSourceReadComplete(value)) => {
            Some((&value.task_id, &value.item_id))
        }
        Some(worker_envelope::Payload::WorkerPhaseChanged(value)) => {
            Some((&value.task_id, &value.item_id))
        }
        Some(worker_envelope::Payload::BaseComputeResult(value)) => {
            Some((&value.task_id, &value.item_id))
        }
        Some(worker_envelope::Payload::Stage1Result(value)) => {
            Some((&value.task_id, &value.item_id))
        }
        Some(worker_envelope::Payload::Stage2Result(value)) => {
            Some((&value.task_id, &value.item_id))
        }
        Some(worker_envelope::Payload::ContactSheetResult(value)) => {
            Some((&value.task_id, &value.item_id))
        }
        Some(worker_envelope::Payload::WorkerFailure(value)) => {
            Some((&value.task_id, &value.item_id))
        }
        _ => None,
    };
    identity.is_some_and(|(task_id, item_id)| task_id == &work.task_id && item_id == &work.item_id)
}

/// 只接受业务阶段；协议默认值和未来未知值都不得进入 Worker 投影。
fn valid_worker_phase(value: i32) -> Option<proto::RuntimeWorkerPhase> {
    proto::RuntimeWorkerPhase::try_from(value)
        .ok()
        .filter(|phase| *phase != proto::RuntimeWorkerPhase::Unspecified)
}

#[allow(clippy::too_many_arguments)]
/// 通过生产或测试工厂创建替代槽并原子安装；失败只报告基础设施事件。
async fn replace_slot_with_factory<E, F, Fut>(
    slot_id: usize,
    slots: &mut BTreeMap<usize, SlotHandle>,
    idle: &mut VecDeque<usize>,
    events: &E,
    state: &Arc<Mutex<PoolState>>,
    replacement_factory: &mut F,
) where
    E: WorkerEventSink,
    F: FnMut(usize) -> Fut,
    Fut: Future<Output = Result<SlotHandle, WorkerPoolError>>,
{
    match replacement_factory(slot_id).await {
        Ok(slot) => {
            state
                .lock()
                .unwrap()
                .process_ids
                .insert(slot_id, slot.process_id);
            idle.push_back(slot_id);
            slots.insert(slot_id, slot);
        }
        Err(error) => {
            events
                .send_event(WorkerEvent::InfrastructureFailure {
                    message: error.to_string(),
                })
                .await;
        }
    }
}

#[allow(clippy::too_many_arguments)]
/// 删除目标任务等待项，终止其运行槽位，并以新 Worker 补齐池容量。
async fn cancel_task_items<E, F, Fut>(
    task_id: &str,
    config: &WorkerPoolConfig,
    job: &WorkerJob,
    queue: &mut VecDeque<WorkItem>,
    slots: &mut BTreeMap<usize, SlotHandle>,
    idle: &mut VecDeque<usize>,
    slot_events: &mut mpsc::UnboundedReceiver<SlotEvent>,
    slot_events_tx: &mpsc::UnboundedSender<SlotEvent>,
    events: &E,
    state: &Arc<Mutex<PoolState>>,
    replacement_factory: &mut F,
) -> Result<(), WorkerPoolError>
where
    E: WorkerEventSink,
    F: FnMut(usize) -> Fut,
    Fut: Future<Output = Result<SlotHandle, WorkerPoolError>>,
{
    let mut kept = VecDeque::with_capacity(queue.len());
    while let Some(work) = queue.pop_front() {
        if work.identity.task_id == task_id {
            events
                .send_event(WorkerEvent::Cancelled {
                    task_id: work.identity.task_id,
                    item_id: work.identity.item_id,
                })
                .await;
        } else {
            kept.push_back(work);
        }
    }
    *queue = kept;

    let mut targets: Vec<(usize, WorkIdentity)> = state
        .lock()
        .unwrap()
        .running
        .iter()
        .filter(|(_, work)| work.task_id == task_id)
        .map(|(slot, work)| (*slot, work.clone()))
        .collect();
    // HashMap 的遍历顺序不稳定；按 slot 排序后，取消终态与运行身份保持 FIFO。
    targets.sort_by_key(|(slot_id, _)| *slot_id);
    let target_slots = targets
        .iter()
        .map(|(slot_id, _)| *slot_id)
        .collect::<Vec<_>>();
    let mut expected: HashSet<usize> = targets.iter().map(|(slot_id, _)| *slot_id).collect();
    let mut deferred = Vec::new();
    let deadline = tokio::time::Instant::now() + config.shutdown_timeout;
    let mut cancellation_error = None;
    for (slot_id, _) in &targets {
        match slots.get(slot_id) {
            Some(slot) if slot.commands.send(SlotCommand::Terminate).is_ok() => {}
            _ => {
                cancellation_error =
                    Some(WorkerPoolError::ShutdownSlotClosed { slot_id: *slot_id });
                break;
            }
        }
    }
    while cancellation_error.is_none() && !expected.is_empty() {
        match tokio::time::timeout_at(deadline, slot_events.recv()).await {
            Ok(Some(SlotEvent::Exited { slot_id, .. })) if expected.remove(&slot_id) => {}
            Ok(Some(SlotEvent::Response { slot_id, .. })) if expected.contains(&slot_id) => {}
            Ok(Some(event)) => deferred.push(event),
            Ok(None) => cancellation_error = Some(WorkerPoolError::Closed),
            Err(_) => {
                cancellation_error = Some(WorkerPoolError::ShutdownTimeout {
                    timeout: config.shutdown_timeout,
                });
            }
        }
    }
    for (slot_id, work) in &targets {
        let retired = slots.remove(slot_id);
        remove_idle_slot(idle, *slot_id);
        let driver_error = match retired {
            Some(slot) if cancellation_error.is_none() => {
                join_retired_slot_until(slot, deadline, config.shutdown_timeout)
                    .await
                    .err()
            }
            Some(slot) => {
                abort_retired_slot(slot).await;
                None
            }
            None => Some(WorkerPoolError::ShutdownSlotClosed { slot_id: *slot_id }),
        };
        if cancellation_error.is_none() {
            cancellation_error = driver_error;
        }
        {
            let mut locked = state.lock().unwrap();
            release_running_work(&mut locked, *slot_id);
            locked.process_ids.remove(slot_id);
        }
        events
            .send_event(WorkerEvent::Cancelled {
                task_id: work.task_id.clone(),
                item_id: work.item_id.clone(),
            })
            .await;
    }
    if cancellation_error.is_none() {
        for slot_id in target_slots {
            let slot = replacement_factory(slot_id).await?;
            state
                .lock()
                .unwrap()
                .process_ids
                .insert(slot_id, slot.process_id);
            idle.push_back(slot_id);
            slots.insert(slot_id, slot);
        }
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
    cancellation_error.map_or(Ok(()), Err)
}

impl TryFrom<proto::WorkerEnvelope> for WorkItem {
    type Error = WorkerPoolError;

    /// 只接受可新占用 slot 的请求，并在进入队列前提取稳定 task/item ID。
    fn try_from(envelope: proto::WorkerEnvelope) -> Result<Self, Self::Error> {
        let (task_id, item_id, cpu_weight, decoder_threads, file_size) = match envelope
            .payload
            .as_ref()
        {
            Some(worker_envelope::Payload::ProbeAndStage1(command)) => {
                (command.task_id.clone(), command.item_id.clone(), 1, None, 0)
            }
            Some(worker_envelope::Payload::ComputeStage2(command)) => {
                (command.task_id.clone(), command.item_id.clone(), 1, None, 0)
            }
            Some(worker_envelope::Payload::BuildContactSheet(command)) => {
                (command.task_id.clone(), command.item_id.clone(), 1, None, 0)
            }
            Some(worker_envelope::Payload::ComputeBaseFeatures(command)) => {
                let media_kind = proto::MediaKind::try_from(command.media_kind).map_err(|_| {
                    WorkerPoolError::InvalidDecoderThreads {
                        media_kind: command.media_kind,
                        decoder_threads: command.decoder_threads,
                    }
                })?;
                let cpu_weight = match media_kind {
                    proto::MediaKind::MediaVideo if (1..=4).contains(&command.decoder_threads) => {
                        command.decoder_threads as usize
                    }
                    proto::MediaKind::MediaImage | proto::MediaKind::MediaOther
                        if command.decoder_threads == 1 =>
                    {
                        1
                    }
                    _ => {
                        return Err(WorkerPoolError::InvalidDecoderThreads {
                            media_kind: command.media_kind,
                            decoder_threads: command.decoder_threads,
                        });
                    }
                };
                (
                    command.task_id.clone(),
                    command.item_id.clone(),
                    cpu_weight,
                    Some(command.decoder_threads),
                    command.file_size,
                )
            }
            _ => return Err(WorkerPoolError::InvalidRequest),
        };
        let identity = WorkIdentity {
            task_id,
            item_id,
            cpu_weight,
            decoder_threads,
            file_identity: None,
        };
        Ok(Self {
            identity,
            envelope,
            scan_guard: None,
            cost: WorkCost {
                cpu_weight,
                file_size,
                enqueue_sequence: 0,
                bypass_count: 0,
                enqueued_at: Instant::now(),
            },
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 构造已完成的假 driver，使纯调度测试仍遵守每个 slot 必须拥有 JoinHandle 的所有权。
    fn completed_slot_driver() -> tokio::task::JoinHandle<()> {
        tokio::spawn(async {})
    }

    /// 创建带一个可控 driver 的真实 pool actor，专门验证关闭的事件与 join 双重门禁。
    fn spawn_shutdown_test_pool<F>(
        config: WorkerPoolConfig,
        driver_factory: F,
    ) -> (WorkerPool, Arc<Mutex<PoolState>>)
    where
        F: FnOnce(
            mpsc::UnboundedReceiver<SlotCommand>,
            mpsc::UnboundedSender<SlotEvent>,
        ) -> tokio::task::JoinHandle<()>,
    {
        let state = Arc::new(Mutex::new(PoolState::new(1)));
        state.lock().unwrap().process_ids.insert(0, 1);
        let (slot_commands, slot_receiver) = mpsc::unbounded_channel();
        let (slot_events, slot_events_receiver) = mpsc::unbounded_channel();
        let driver = driver_factory(slot_receiver, slot_events.clone());
        let slots = BTreeMap::from([(
            0,
            SlotHandle {
                process_id: 1,
                commands: slot_commands,
                driver,
            },
        )]);
        let (commands, command_receiver) = mpsc::channel(8);
        let (events, event_receiver) = mpsc::channel(8);
        let job = WorkerJob::create().expect("关闭测试需要可用 Worker Job");
        let actor = tokio::spawn(run_pool(
            config,
            job,
            slots,
            VecDeque::from([0]),
            command_receiver,
            slot_events_receiver,
            slot_events,
            WorkerEventOutbox::new(events),
            Arc::clone(&state),
        ));
        (
            WorkerPool {
                commands,
                events: event_receiver,
                state: Arc::clone(&state),
                actor,
            },
            state,
        )
    }

    #[tokio::test]
    async fn shutdown_waits_for_driver_join_after_exited_event() {
        let exited = Arc::new(Notify::new());
        let release = Arc::new(Notify::new());
        let driver_exited = Arc::clone(&exited);
        let driver_release = Arc::clone(&release);
        let config = WorkerPoolConfig::new(WorkerLaunch::new("unused-worker.exe"), 1)
            .with_shutdown_timeout(Duration::from_secs(1));
        let (pool, _state) = spawn_shutdown_test_pool(config, move |mut commands, events| {
            tokio::spawn(async move {
                assert!(matches!(
                    commands.recv().await,
                    Some(SlotCommand::Terminate)
                ));
                events
                    .send(SlotEvent::Exited {
                        slot_id: 0,
                        work: None,
                        process_id: Some(1),
                        exit_code: Some(0),
                        message: "test exited before driver returns".into(),
                    })
                    .unwrap();
                driver_exited.notify_one();
                driver_release.notified().await;
            })
        });
        let shutdown = tokio::spawn(pool.shutdown());
        exited.notified().await;
        assert!(
            !shutdown.is_finished(),
            "仅收到 Exited 不能越过仍被 gate 阻塞的 driver"
        );
        release.notify_one();
        assert!(shutdown.await.unwrap().is_ok());
    }

    #[tokio::test(start_paused = true)]
    async fn shutdown_timeout_aborts_driver_and_clears_runtime_state() {
        let terminated = Arc::new(Notify::new());
        let driver_terminated = Arc::clone(&terminated);
        let timeout = Duration::from_secs(1);
        let config = WorkerPoolConfig::new(WorkerLaunch::new("unused-worker.exe"), 1)
            .with_shutdown_timeout(timeout);
        let (pool, state) = spawn_shutdown_test_pool(config, move |mut commands, _events| {
            tokio::spawn(async move {
                assert!(matches!(
                    commands.recv().await,
                    Some(SlotCommand::Terminate)
                ));
                driver_terminated.notify_one();
                std::future::pending::<()>().await;
            })
        });
        let shutdown = tokio::spawn(pool.shutdown());
        terminated.notified().await;
        tokio::time::advance(timeout).await;
        assert!(matches!(
            shutdown.await.unwrap(),
            Err(WorkerPoolError::ShutdownTimeout { timeout: actual }) if actual == timeout
        ));
        let locked = state.lock().unwrap();
        assert!(locked.running.is_empty());
        assert!(locked.process_ids.is_empty());
        assert_eq!(locked.cpu_in_use, 0);
    }

    #[tokio::test]
    async fn exited_event_waits_for_driver_before_installing_replacement() {
        let at_gate = Arc::new(Notify::new());
        let release = Arc::new(Notify::new());
        let driver_gate = Arc::clone(&at_gate);
        let driver_release = Arc::clone(&release);
        let driver = tokio::spawn(async move {
            driver_gate.notify_one();
            driver_release.notified().await;
        });
        let state = Arc::new(Mutex::new(PoolState::new(1)));
        state.lock().unwrap().process_ids.insert(0, 1);
        let (commands, _receiver) = mpsc::unbounded_channel();
        let slots = BTreeMap::from([(
            0,
            SlotHandle {
                process_id: 1,
                commands,
                driver,
            },
        )]);
        let (events, _event_receiver) = mpsc::channel(8);
        let factory_calls = Arc::new(AtomicUsize::new(0));
        let factory_counter = Arc::clone(&factory_calls);
        let action_started = Arc::new(Notify::new());
        let action_signal = Arc::clone(&action_started);
        let transition = tokio::spawn(async move {
            let mut slots = slots;
            let mut idle = VecDeque::new();
            let (replacement_commands, _replacement_receiver) = mpsc::unbounded_channel();
            let mut replacement_factory = move |_| {
                factory_counter.fetch_add(1, Ordering::AcqRel);
                std::future::ready(Ok(SlotHandle {
                    process_id: 2,
                    commands: replacement_commands.clone(),
                    driver: completed_slot_driver(),
                }))
            };
            action_signal.notify_one();
            handle_slot_event_with_replacement(
                SlotEvent::Exited {
                    slot_id: 0,
                    work: None,
                    process_id: Some(1),
                    exit_code: Some(0),
                    message: "exited before driver return".into(),
                },
                &mut slots,
                &mut idle,
                &events,
                &state,
                DEFAULT_SHUTDOWN_TIMEOUT,
                &mut replacement_factory,
            )
            .await;
            slots
        });
        action_started.notified().await;
        at_gate.notified().await;
        tokio::task::yield_now().await;
        assert_eq!(
            factory_calls.load(Ordering::Acquire),
            0,
            "旧 driver 未返回时不得安装 replacement"
        );
        release.notify_one();
        let slots = transition.await.unwrap();
        assert_eq!(factory_calls.load(Ordering::Acquire), 1);
        assert_eq!(slots.get(&0).unwrap().process_id, 2);
    }

    #[tokio::test(start_paused = true)]
    async fn cancel_timeout_aborts_driver_and_keeps_pool_shutdown_reachable() {
        let terminated = Arc::new(Notify::new());
        let driver_terminated = Arc::clone(&terminated);
        let timeout = Duration::from_secs(1);
        let config = WorkerPoolConfig::new(WorkerLaunch::new("unused-worker.exe"), 1)
            .with_shutdown_timeout(timeout);
        let state = Arc::new(Mutex::new(PoolState::new(1)));
        state.lock().unwrap().process_ids.insert(0, 1);
        let work = scheduler_work("cancel-timeout", 0).identity;
        register_running_work(&mut state.lock().unwrap(), 0, work.clone());
        let (slot_commands, mut slot_receiver) = mpsc::unbounded_channel();
        let driver = tokio::spawn(async move {
            assert!(matches!(
                slot_receiver.recv().await,
                Some(SlotCommand::Terminate)
            ));
            driver_terminated.notify_one();
            std::future::pending::<()>().await;
        });
        let slots = BTreeMap::from([(
            0,
            SlotHandle {
                process_id: 1,
                commands: slot_commands,
                driver,
            },
        )]);
        let (slot_events, slot_events_receiver) = mpsc::unbounded_channel();
        let (commands, command_receiver) = mpsc::channel(8);
        let (events, event_receiver) = mpsc::channel(8);
        let actor = tokio::spawn(run_pool(
            config,
            WorkerJob::create().expect("取消超时测试需要 Worker Job"),
            slots,
            VecDeque::new(),
            command_receiver,
            slot_events_receiver,
            slot_events,
            WorkerEventOutbox::new(events),
            Arc::clone(&state),
        ));
        let pool = WorkerPool {
            commands,
            events: event_receiver,
            state: Arc::clone(&state),
            actor,
        };
        {
            let cancel = pool.cancel_task("closed-slot");
            tokio::pin!(cancel);
            tokio::select! {
                result = &mut cancel => panic!("取消不应提前完成: {result:?}"),
                _ = terminated.notified() => {}
            }
            tokio::time::advance(timeout).await;
            assert!(matches!(
                cancel.await,
                Err(WorkerPoolError::ShutdownTimeout { timeout: actual }) if actual == timeout
            ));
        }
        let locked = state.lock().unwrap();
        assert!(locked.running.is_empty());
        assert!(locked.process_ids.is_empty());
        assert_eq!(locked.cpu_in_use, 0);
        drop(locked);
        assert!(
            pool.shutdown().await.is_ok(),
            "取消超时后仍必须能关闭 Pool actor"
        );
    }

    /// 构造只占一个 CPU 权重的调度项，供关闭槽位行为测试直接驱动生产调度函数。
    fn scheduler_work(item_id: &str, enqueue_sequence: u64) -> WorkItem {
        let envelope = proto::WorkerEnvelope {
            payload: Some(worker_envelope::Payload::ComputeBaseFeatures(
                proto::ComputeBaseFeatures {
                    task_id: "closed-slot".into(),
                    item_id: item_id.into(),
                    machine_id: "91".repeat(32),
                    normalized_path: format!(r"I:\task5\{item_id}.bin"),
                    display_path: format!(r"I:\task5\{item_id}.bin"),
                    file_size: 4_096,
                    physical_disk_id: "disk-closed-slot".into(),
                    md5: vec![9; 16],
                    media_kind: proto::MediaKind::MediaOther as i32,
                    missing_parts: 0,
                    block_size_bytes: 64 * 1_024,
                    block_timeout_ms: 3_000,
                    block_retries: 2,
                    decoder_threads: 1,
                },
            )),
        };
        let mut work = WorkItem::try_from(envelope).expect("调度夹具必须是合法请求");
        work.prepare_for_queue(enqueue_sequence, 1)
            .expect("单权重任务必须可进入单核预算队列");
        work
    }

    #[tokio::test]
    async fn closed_slot_is_quarantined_without_losing_work_and_replacement_resumes_dispatch() {
        let state = Arc::new(Mutex::new(PoolState::new(1)));
        let (events, _event_rx) = mpsc::channel(8);

        // 先用健康槽兜底取得原任务，使旧实现能确定性返回并暴露 stale idle 残留。
        let (closed_commands, closed_receiver) = mpsc::unbounded_channel();
        drop(closed_receiver);
        let (healthy_commands, mut healthy_receiver) = mpsc::unbounded_channel();
        let slots = BTreeMap::from([
            (
                0,
                SlotHandle {
                    process_id: 100,
                    commands: closed_commands,
                    driver: completed_slot_driver(),
                },
            ),
            (
                1,
                SlotHandle {
                    process_id: 101,
                    commands: healthy_commands,
                    driver: completed_slot_driver(),
                },
            ),
        ]);
        let mut idle = VecDeque::from([0, 1]);
        let mut queue = VecDeque::from([scheduler_work("first", 0)]);

        schedule(&mut queue, &mut idle, &slots, &events, &state).await;

        assert!(queue.is_empty(), "发送失败的任务必须保留并转给健康槽");
        assert!(idle.is_empty(), "命令通道已关闭的槽位不得重新进入 idle");
        let SlotCommand::Run(first) = healthy_receiver
            .try_recv()
            .expect("健康槽必须收到失败后保留的任务")
        else {
            panic!("健康槽只能收到 Run")
        };
        assert_eq!(first.identity.item_id, "first");
        {
            let locked = state.lock().unwrap();
            assert_eq!(locked.cpu_in_use, 1);
            assert!(!locked.running.contains_key(&0), "关闭槽不得登记 CPU");
            assert_eq!(locked.running.get(&1).unwrap().item_id, "first");
        }
        release_running_work(&mut state.lock().unwrap(), 1);

        // 唯一槽关闭时 schedule 必须返回，把原任务留给随后到达的 Exited/替换流程。
        let (stale_commands, stale_receiver) = mpsc::unbounded_channel();
        drop(stale_receiver);
        let mut replacement_slots = BTreeMap::from([(
            2,
            SlotHandle {
                process_id: 102,
                commands: stale_commands,
                driver: completed_slot_driver(),
            },
        )]);
        let mut replacement_idle = VecDeque::from([2]);
        let mut retained_queue = VecDeque::from([scheduler_work("retained", 1)]);
        state.lock().unwrap().process_ids.insert(2, 102);

        schedule(
            &mut retained_queue,
            &mut replacement_idle,
            &replacement_slots,
            &events,
            &state,
        )
        .await;

        assert_eq!(retained_queue.len(), 1, "关闭槽不得吞掉等待任务");
        assert_eq!(retained_queue[0].identity.item_id, "retained");
        assert!(replacement_idle.is_empty(), "关闭槽必须等待 Exited 后替换");
        assert_eq!(state.lock().unwrap().cpu_in_use, 0);

        let (replacement_commands, mut replacement_receiver) = mpsc::unbounded_channel();
        let replacement_calls = Arc::new(AtomicUsize::new(0));
        let factory_calls = Arc::clone(&replacement_calls);
        let mut replacement_factory = move |slot_id: usize| {
            assert_eq!(slot_id, 2);
            factory_calls.fetch_add(1, Ordering::AcqRel);
            std::future::ready(Ok(SlotHandle {
                process_id: 202,
                commands: replacement_commands.clone(),
                driver: completed_slot_driver(),
            }))
        };
        handle_slot_event_and_schedule_with_replacement(
            SlotEvent::Exited {
                slot_id: 2,
                work: None,
                process_id: Some(102),
                exit_code: Some(1),
                message: "closed command receiver".into(),
            },
            &mut retained_queue,
            &mut replacement_slots,
            &mut replacement_idle,
            &events,
            &state,
            DEFAULT_SHUTDOWN_TIMEOUT,
            &mut replacement_factory,
        )
        .await;

        let SlotCommand::Run(retained) = replacement_receiver
            .try_recv()
            .expect("替换槽必须继续收到原等待任务")
        else {
            panic!("替换槽只能收到 Run")
        };
        assert_eq!(retained.identity.item_id, "retained");
        assert!(retained_queue.is_empty());
        assert!(replacement_idle.is_empty());
        assert_eq!(replacement_calls.load(Ordering::Acquire), 1);
        assert_eq!(replacement_slots.len(), 1);
        assert_eq!(replacement_slots.get(&2).unwrap().process_id, 202);
        let locked = state.lock().unwrap();
        assert_eq!(locked.failure_count, 1);
        assert_eq!(locked.process_ids.get(&2), Some(&202));
        assert_eq!(locked.cpu_in_use, 1);
        assert_eq!(locked.running.get(&2).unwrap().item_id, "retained");
    }

    #[tokio::test]
    async fn unspecified_worker_phase_becomes_terminal_failure_and_releases_cpu_slot() {
        let state = Arc::new(Mutex::new(PoolState::new(1)));
        let work = scheduler_work("invalid-phase", 0).identity;
        register_running_work(&mut state.lock().unwrap(), 0, work.clone());
        let mut slots = BTreeMap::new();
        let mut idle = VecDeque::new();
        let (events, mut event_rx) = mpsc::channel(2);
        let mut replacement_factory = |_| std::future::ready(Err(WorkerPoolError::Closed));

        handle_slot_event_with_replacement(
            SlotEvent::Response {
                slot_id: 0,
                work,
                response: proto::WorkerEnvelope {
                    payload: Some(worker_envelope::Payload::WorkerPhaseChanged(
                        proto::WorkerPhaseChanged {
                            task_id: "closed-slot".into(),
                            item_id: "invalid-phase".into(),
                            phase: proto::RuntimeWorkerPhase::Unspecified as i32,
                            request_elapsed_us: None,
                        },
                    )),
                },
            },
            &mut slots,
            &mut idle,
            &events,
            &state,
            DEFAULT_SHUTDOWN_TIMEOUT,
            &mut replacement_factory,
        )
        .await;

        let locked = state.lock().unwrap();
        assert_eq!(locked.cpu_in_use, 0);
        assert!(locked.running.is_empty());
        drop(locked);
        assert_eq!(idle, VecDeque::from([0]));
        let WorkerEvent::Completed { response, .. } = event_rx.recv().await.unwrap() else {
            panic!("非法 phase 必须转换成终态失败")
        };
        let Some(worker_envelope::Payload::WorkerFailure(failure)) = response.payload else {
            panic!("非法 phase 必须返回协议失败")
        };
        assert_eq!(failure.stage, "protocol");
    }

    #[test]
    fn source_complete_does_not_infer_slot_work_phase() {
        let mut work = scheduler_work("phase-boundary", 0).identity;
        work.file_identity = Some(WorkerFileIdentity {
            machine_id: MachineId::from_sha256([0x95; 32]),
            normalized_path: NormalizedPath::new(r"I:\phase-boundary.bin").unwrap(),
            display_path: DisplayPath::new(r"I:\phase-boundary.bin").unwrap(),
            file_size: 4_096,
            stage: "base_compute".into(),
            physical_disk_id: "disk-phase".into(),
        });
        let source = proto::WorkerEnvelope {
            payload: Some(worker_envelope::Payload::BaseSourceReadComplete(
                proto::BaseSourceReadComplete {
                    task_id: work.task_id.clone(),
                    item_id: work.item_id.clone(),
                    request_elapsed_us: Some(8_000),
                },
            )),
        };
        update_slot_work_from_phase_response(&mut work, &source);
        assert_eq!(work.file_identity.as_ref().unwrap().stage, "base_compute");

        let feature = proto::WorkerEnvelope {
            payload: Some(worker_envelope::Payload::WorkerPhaseChanged(
                proto::WorkerPhaseChanged {
                    task_id: work.task_id.clone(),
                    item_id: work.item_id.clone(),
                    phase: proto::RuntimeWorkerPhase::RuntimeWorkerFeature as i32,
                    request_elapsed_us: Some(9_000),
                },
            )),
        };
        update_slot_work_from_phase_response(&mut work, &feature);
        assert_eq!(work.file_identity.as_ref().unwrap().stage, "base_feature");
    }

    /// 创建不启动真实进程的 run_pool actor，供控制命令 ACK 背压行为测试使用。
    fn spawn_ack_backpressure_pool(
        idle: VecDeque<usize>,
    ) -> (
        WorkerPool,
        mpsc::Sender<WorkerEvent>,
        mpsc::UnboundedReceiver<SlotCommand>,
        mpsc::UnboundedSender<SlotEvent>,
    ) {
        let state = Arc::new(Mutex::new(PoolState::new(1)));
        state.lock().unwrap().process_ids.insert(0, 1);
        let (slot_commands, slot_receiver) = mpsc::unbounded_channel();
        let slots = BTreeMap::from([(
            0,
            SlotHandle {
                process_id: 1,
                commands: slot_commands,
                driver: completed_slot_driver(),
            },
        )]);
        let (slot_events, slot_events_receiver) = mpsc::unbounded_channel();
        let (commands, command_receiver) = mpsc::channel(8);
        let (events, event_receiver) = mpsc::channel(256);
        let actor_state = Arc::clone(&state);
        let config = WorkerPoolConfig::new(WorkerLaunch::new("unused-worker.exe"), 1);
        let job = WorkerJob::create().expect("测试 actor 需要一个可用 Worker Job");
        let event_outbox = WorkerEventOutbox::new(events.clone());
        let actor = tokio::spawn(run_pool(
            config,
            job,
            slots,
            idle,
            command_receiver,
            slot_events_receiver,
            slot_events.clone(),
            event_outbox,
            actor_state,
        ));
        (
            WorkerPool {
                commands,
                events: event_receiver,
                state,
                actor,
            },
            events,
            slot_receiver,
            slot_events,
        )
    }

    /// 创建可同时预填外层与内层事件队列的测试池，保留两层 sender 供真实 actor 背压测试使用。
    fn spawn_ack_backpressure_pool_with_inner(
        idle: VecDeque<usize>,
    ) -> (
        WorkerPool,
        mpsc::Sender<WorkerEvent>,
        mpsc::Sender<WorkerEvent>,
        mpsc::UnboundedReceiver<SlotCommand>,
        mpsc::UnboundedSender<SlotEvent>,
    ) {
        let state = Arc::new(Mutex::new(PoolState::new(1)));
        state.lock().unwrap().process_ids.insert(0, 1);
        let (slot_commands, slot_receiver) = mpsc::unbounded_channel();
        let slots = BTreeMap::from([(
            0,
            SlotHandle {
                process_id: 1,
                commands: slot_commands,
                driver: completed_slot_driver(),
            },
        )]);
        let (slot_events, slot_events_receiver) = mpsc::unbounded_channel();
        let (commands, command_receiver) = mpsc::channel(8);
        let (events, event_receiver) = mpsc::channel(256);
        let actor_state = Arc::clone(&state);
        let config = WorkerPoolConfig::new(WorkerLaunch::new("unused-worker.exe"), 1);
        let job = WorkerJob::create().expect("测试 actor 需要一个可用 Worker Job");
        let event_outbox = WorkerEventOutbox::new(events.clone());
        let inner = event_outbox.pending.clone();
        let actor = tokio::spawn(run_pool(
            config,
            job,
            slots,
            idle,
            command_receiver,
            slot_events_receiver,
            slot_events.clone(),
            event_outbox,
            actor_state,
        ));
        (
            WorkerPool {
                commands,
                events: event_receiver,
                state,
                actor,
            },
            events,
            inner,
            slot_receiver,
            slot_events,
        )
    }

    /// 构造可进入真实 run_pool schedule 的带路径基础计算请求。
    fn ack_backpressure_request(task_id: &str, item_id: &str) -> proto::WorkerEnvelope {
        let mut work = scheduler_work(item_id, 0);
        if let Some(worker_envelope::Payload::ComputeBaseFeatures(command)) =
            work.envelope.payload.as_mut()
        {
            command.task_id = task_id.to_owned();
            command.item_id = item_id.to_owned();
        }
        work.envelope
    }

    /// 构造 Started 事件所需的冻结文件身份，确保测试覆盖完整 dispatch 路径。
    fn ack_backpressure_identity(item_id: &str) -> WorkerFileIdentity {
        let path = format!(r"I:\ack-backpressure\{item_id}.bin");
        WorkerFileIdentity {
            machine_id: MachineId::from_sha256([0xa1; 32]),
            normalized_path: NormalizedPath::new(&path).unwrap(),
            display_path: DisplayPath::new(&path).unwrap(),
            file_size: 1,
            stage: "base_compute".into(),
            physical_disk_id: "disk-ack".into(),
        }
    }

    #[tokio::test]
    async fn dispatch_ack_does_not_wait_for_full_worker_event_channel() {
        let (mut pool, events, mut slot_receiver, slot_events) =
            spawn_ack_backpressure_pool(VecDeque::from([0]));
        for index in 0..256 {
            events
                .send(WorkerEvent::InfrastructureFailure {
                    message: format!("prefill-{index}"),
                })
                .await
                .unwrap();
        }

        let result = tokio::time::timeout(
            Duration::from_millis(200),
            pool.dispatch_runtime(
                ack_backpressure_request("dispatch-ack", "dispatch-item"),
                ack_backpressure_identity("dispatch-item"),
            ),
        )
        .await;
        assert!(result.is_ok(), "dispatch ACK 不得等待 WorkerEvent 通道消费");
        assert!(result.unwrap().is_ok());

        let SlotCommand::Run(work) = slot_receiver
            .recv()
            .await
            .expect("dispatch 必须到达假 slot")
        else {
            panic!("假 slot 只能收到 Run")
        };
        let identity = work.identity;
        let task_id = identity.task_id.clone();
        let item_id = identity.item_id.clone();
        slot_events
            .send(SlotEvent::Response {
                slot_id: 0,
                work: identity,
                response: proto::WorkerEnvelope {
                    payload: Some(worker_envelope::Payload::WorkerFailure(
                        proto::WorkerFailure {
                            task_id,
                            item_id,
                            stage: "test-terminal".into(),
                            message: "terminal".into(),
                        },
                    )),
                },
            })
            .unwrap();
        tokio::time::timeout(Duration::from_secs(1), async {
            loop {
                if pool.busy_workers() == 0 && pool.cpu_in_use() == 0 {
                    break;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("终态响应必须释放 Worker 与 CPU");
        assert_eq!(pool.busy_workers(), 0);
        assert_eq!(pool.cpu_in_use(), 0);

        for index in 0..256 {
            let event = tokio::time::timeout(Duration::from_secs(1), pool.next_event())
                .await
                .expect("预填事件必须可读")
                .expect("事件通道不得提前关闭");
            let WorkerEvent::InfrastructureFailure { message } = event else {
                panic!("预填事件顺序被破坏: index={index}")
            };
            assert_eq!(message, format!("prefill-{index}"));
        }
        assert!(matches!(
            tokio::time::timeout(Duration::from_secs(1), pool.next_event())
                .await
                .expect("Started 事件必须可读"),
            Some(WorkerEvent::Started { task_id, item_id, .. })
                if task_id == "dispatch-ack" && item_id == "dispatch-item"
        ));
        assert!(matches!(
            tokio::time::timeout(Duration::from_secs(1), pool.next_event())
                .await
                .expect("终态事件必须可读"),
            Some(WorkerEvent::Completed { task_id, item_id, .. })
                if task_id == "dispatch-ack" && item_id == "dispatch-item"
        ));
    }

    #[tokio::test]
    async fn dispatch_ack_survives_saturated_outer_and_inner_event_queues() {
        let (mut pool, events, inner, mut slot_receiver, slot_events) =
            spawn_ack_backpressure_pool_with_inner(VecDeque::from([0]));
        for index in 0..256 {
            events
                .send(WorkerEvent::InfrastructureFailure {
                    message: format!("outer-{index}"),
                })
                .await
                .unwrap();
        }
        for index in 0..256 {
            inner
                .send(WorkerEvent::InfrastructureFailure {
                    message: format!("inner-{index}"),
                })
                .await
                .unwrap();
        }

        let result = tokio::time::timeout(
            Duration::from_millis(200),
            pool.dispatch_runtime(
                ack_backpressure_request("dispatch-both", "dispatch-both-item"),
                ack_backpressure_identity("dispatch-both-item"),
            ),
        )
        .await
        .expect("外层和内层同时饱和时 Dispatch ACK 仍必须完成");
        result.unwrap();

        let SlotCommand::Run(work) = slot_receiver
            .recv()
            .await
            .expect("dispatch 必须到达假 slot")
        else {
            panic!("假 slot 只能收到 Run")
        };
        let identity = work.identity;
        let task_id = identity.task_id.clone();
        let item_id = identity.item_id.clone();
        slot_events
            .send(SlotEvent::Response {
                slot_id: 0,
                work: identity,
                response: proto::WorkerEnvelope {
                    payload: Some(worker_envelope::Payload::WorkerFailure(
                        proto::WorkerFailure {
                            task_id,
                            item_id,
                            stage: "test-terminal".into(),
                            message: "terminal".into(),
                        },
                    )),
                },
            })
            .unwrap();
        tokio::time::timeout(Duration::from_secs(1), async {
            loop {
                if pool.busy_workers() == 0 && pool.cpu_in_use() == 0 {
                    break;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("终态响应必须释放 Worker 与 CPU");

        for index in 0..256 {
            let event = tokio::time::timeout(Duration::from_secs(1), pool.next_event())
                .await
                .expect("外层预填事件必须可读")
                .expect("事件通道不得提前关闭");
            let WorkerEvent::InfrastructureFailure { message } = event else {
                panic!("外层预填事件顺序被破坏: index={index}")
            };
            assert_eq!(message, format!("outer-{index}"));
        }
        for index in 0..256 {
            let event = tokio::time::timeout(Duration::from_secs(1), pool.next_event())
                .await
                .expect("内层预填事件必须可读")
                .expect("事件通道不得提前关闭");
            let WorkerEvent::InfrastructureFailure { message } = event else {
                panic!("内层预填事件顺序被破坏: index={index}")
            };
            assert_eq!(message, format!("inner-{index}"));
        }
        assert!(matches!(
            tokio::time::timeout(Duration::from_secs(1), pool.next_event())
                .await
                .expect("Started 事件必须可读"),
            Some(WorkerEvent::Started { task_id, item_id, .. })
                if task_id == "dispatch-both" && item_id == "dispatch-both-item"
        ));
        assert!(matches!(
            tokio::time::timeout(Duration::from_secs(1), pool.next_event())
                .await
                .expect("终态事件必须可读"),
            Some(WorkerEvent::Completed { task_id, item_id, .. })
                if task_id == "dispatch-both" && item_id == "dispatch-both-item"
        ));
        assert!(matches!(
            pool.events.try_recv(),
            Err(mpsc::error::TryRecvError::Empty)
        ));
    }

    #[tokio::test]
    async fn cancel_ack_does_not_wait_for_full_worker_event_channel() {
        let (mut pool, events, _slot_receiver, _slot_events) =
            spawn_ack_backpressure_pool(VecDeque::new());
        for index in 0..256 {
            events
                .send(WorkerEvent::InfrastructureFailure {
                    message: format!("prefill-{index}"),
                })
                .await
                .unwrap();
        }

        pool.dispatch_runtime(
            ack_backpressure_request("cancel-ack", "cancel-item"),
            ack_backpressure_identity("cancel-item"),
        )
        .await
        .unwrap();
        let result =
            tokio::time::timeout(Duration::from_millis(200), pool.cancel_task("cancel-ack")).await;
        assert!(result.is_ok(), "cancel ACK 不得等待 WorkerEvent 通道消费");
        assert!(result.unwrap().is_ok());
        assert_eq!(pool.busy_workers(), 0);
        assert_eq!(pool.cpu_in_use(), 0);

        for index in 0..256 {
            let event = tokio::time::timeout(Duration::from_secs(1), pool.next_event())
                .await
                .expect("预填事件必须可读")
                .expect("事件通道不得提前关闭");
            let WorkerEvent::InfrastructureFailure { message } = event else {
                panic!("预填事件顺序被破坏: index={index}")
            };
            assert_eq!(message, format!("prefill-{index}"));
        }
        assert!(matches!(
            tokio::time::timeout(Duration::from_secs(1), pool.next_event())
                .await
                .expect("Cancelled 事件必须可读"),
            Some(WorkerEvent::Cancelled { task_id, item_id })
                if task_id == "cancel-ack" && item_id == "cancel-item"
        ));
    }

    #[tokio::test]
    async fn cancel_command_ack_survives_256_worker_event_burst() {
        let worker_count = 256;
        let state = Arc::new(Mutex::new(PoolState::new(worker_count)));
        let mut slots = BTreeMap::new();
        // 取消测试模拟真实 slot 的控制接收端仍存活，不能把发送失败误当作 Exited。
        let mut command_receivers = Vec::with_capacity(worker_count);
        for slot in 0..worker_count {
            state
                .lock()
                .unwrap()
                .process_ids
                .insert(slot, slot as u32 + 1);
            state.lock().unwrap().running.insert(
                slot,
                WorkIdentity {
                    task_id: "cancel-command-256".into(),
                    item_id: format!("item-{slot:03}"),
                    cpu_weight: 1,
                    decoder_threads: None,
                    file_identity: None,
                },
            );
            let (commands, receiver) = mpsc::unbounded_channel();
            command_receivers.push(receiver);
            slots.insert(
                slot,
                SlotHandle {
                    process_id: slot as u32 + 1,
                    commands,
                    driver: completed_slot_driver(),
                },
            );
        }
        state.lock().unwrap().cpu_in_use = worker_count;

        let (events, event_receiver) = mpsc::channel(256);
        let event_outbox = WorkerEventOutbox::new(events.clone());
        let inner = event_outbox.pending.clone();
        for index in 0..256 {
            events
                .send(WorkerEvent::InfrastructureFailure {
                    message: format!("outer-{index}"),
                })
                .await
                .unwrap();
        }
        for index in 0..256 {
            inner
                .send(WorkerEvent::InfrastructureFailure {
                    message: format!("inner-{index}"),
                })
                .await
                .unwrap();
        }

        let (slot_events, slot_events_receiver) = mpsc::unbounded_channel();
        let (commands, command_receiver) = mpsc::channel(8);
        let job = Arc::new(WorkerJob::create().expect("取消测试需要 Worker Job"));
        let config = WorkerPoolConfig::new(WorkerLaunch::new("unused-worker.exe"), worker_count);
        let replacement_commands = mpsc::unbounded_channel().0;
        let replacement_factory = move |slot_id: usize| {
            std::future::ready(Ok::<SlotHandle, WorkerPoolError>(SlotHandle {
                process_id: slot_id as u32 + 10_000,
                commands: replacement_commands.clone(),
                driver: completed_slot_driver(),
            }))
        };
        let actor_state = Arc::clone(&state);
        tokio::spawn(run_pool_with_replacement(
            config,
            job,
            slots,
            VecDeque::new(),
            command_receiver,
            slot_events_receiver,
            slot_events.clone(),
            event_outbox,
            actor_state,
            replacement_factory,
        ));

        // 先排入 Cancel 命令，再排入 Exited，确保 actor 的命令 ACK 测试覆盖真实取消路径。
        let (reply, reply_receiver) = oneshot::channel();
        commands
            .send(PoolCommand::Cancel("cancel-command-256".into(), reply))
            .await
            .unwrap();
        for slot in 0..worker_count {
            slot_events
                .send(SlotEvent::Exited {
                    slot_id: slot,
                    work: None,
                    process_id: Some(slot as u32 + 1),
                    exit_code: Some(0),
                    message: "cancelled by test".into(),
                })
                .unwrap();
        }

        let result = tokio::time::timeout(Duration::from_millis(200), reply_receiver)
            .await
            .expect("256 Worker 取消突发不得等待事件容量")
            .expect("actor 必须返回 Cancel ACK");
        result.expect("可控替换工厂不应失败");
        assert_eq!(state.lock().unwrap().cpu_in_use, 0);
        assert!(state.lock().unwrap().running.is_empty());

        let mut event_receiver = event_receiver;
        for index in 0..256 {
            let event = event_receiver.recv().await.expect("外层事件必须完整保留");
            let WorkerEvent::InfrastructureFailure { message } = event else {
                panic!("外层事件顺序被破坏: index={index}")
            };
            assert_eq!(message, format!("outer-{index}"));
        }
        for index in 0..256 {
            let event = event_receiver.recv().await.expect("内层事件必须完整保留");
            let WorkerEvent::InfrastructureFailure { message } = event else {
                panic!("内层事件顺序被破坏: index={index}")
            };
            assert_eq!(message, format!("inner-{index}"));
        }
        for slot in 0..worker_count {
            let event = event_receiver
                .recv()
                .await
                .expect("Cancelled 事件必须完整保留");
            let WorkerEvent::Cancelled { task_id, item_id } = event else {
                panic!("终态事件类型被破坏: slot={slot}")
            };
            assert_eq!(task_id, "cancel-command-256");
            assert_eq!(item_id, format!("item-{slot:03}"));
        }
        assert!(matches!(
            event_receiver.try_recv(),
            Err(mpsc::error::TryRecvError::Empty)
        ));
    }
}
