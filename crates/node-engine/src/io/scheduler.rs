//! 按物理磁盘 FIFO、盘间 round-robin 和全局上限授予文件读取许可。

use std::{
    cmp::Ordering as CmpOrdering,
    collections::{BTreeMap, BTreeSet, VecDeque},
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
};

use dedup_core::DiskReadConfig;
use dedup_windows::{LocalDiskKind, StorageLocation};
use thiserror::Error;
use tokio::sync::{Notify, OwnedSemaphorePermit, Semaphore, mpsc, oneshot};

/// 文件读取所属的流水线阶段；两类请求共享同一组磁盘硬上限。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum DiskReadClass {
    /// Node 顺序读取完整文件并计算 MD5。
    HashSequential,
    /// Worker 打开媒体源并完成探测或解码。
    MediaDecode,
}

/// 本轮任务冻结的一条物理盘读取 lane；额度和权重分别控制硬上限与调度比例。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct DiskReadLane {
    /// 已在枚举前解析并冻结的物理盘身份与介质类型。
    pub location: StorageLocation,
    /// 该 lane 可同时持有的读取许可上限。
    pub effective_limit: usize,
    /// 全局额度不足时用于加权轮转的配置权重。
    pub configured_weight: usize,
}

/// 同盘 Hash 与媒体均可推进时，连续优先媒体读取的次数。
const MEDIA_WEIGHT: u8 = 3;
/// 老请求被年轻冲突请求绕过后进入保护模式的次数阈值。
const MAX_CONFLICTING_BYPASSES: u8 = 8;

/// 在硬上限下保存 Hash/Media 的软目标；T=1 没有可比较的比例分母。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
struct NominalSeats {
    /// MediaDecode 的名义活动容量。
    media: Option<usize>,
    /// HashSequential 的名义活动容量。
    hash: Option<usize>,
}

impl NominalSeats {
    /// 返回指定类别的名义 seat；T=1 返回 None 并交给轮换规则。
    fn for_class(self, class: DiskReadClass) -> Option<usize> {
        match class {
            DiskReadClass::HashSequential => self.hash,
            DiskReadClass::MediaDecode => self.media,
        }
    }
}

/// 保存候选授予后的 active/nominal 比例，比较时使用 u128 交叉乘法。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
struct PressureRatio {
    numerator: usize,
    denominator: usize,
}

impl PressureRatio {
    /// 不使用浮点，避免大容量配置和舍入造成类别选择漂移。
    fn cmp(self, other: Self) -> std::cmp::Ordering {
        let left = (self.numerator as u128) * (other.denominator as u128);
        let right = (other.numerator as u128) * (self.denominator as u128);
        left.cmp(&right)
    }
}

/// 全局唯一的老化保留身份；只绑定当前队首 key/class/sequence。
#[derive(Clone, Debug, Eq, PartialEq)]
struct AgedReservation {
    key: DiskKey,
    class: DiskReadClass,
    sequence: u64,
}

/// 读取调度器的配置、生命周期或关闭错误。
#[derive(Clone, Debug, Eq, Error, PartialEq)]
pub enum SchedulerError {
    /// 构造参数没有经过有效 Node 配置约束。
    #[error("读取调度配置无效: {0}")]
    InvalidConfiguration(&'static str),
    /// 当前线程不在 Tokio runtime 中，无法启动单 actor。
    #[error("读取调度器必须在 Tokio runtime 中创建")]
    MissingRuntime,
    /// 调度 actor 已经关闭，当前或等待请求不能继续。
    #[error("读取调度器已经关闭")]
    Closed,
}

/// 同时占用一个全局槽和一个物理盘槽的文件级读取许可。
#[must_use = "读取许可必须持有到 Worker 不再访问文件"]
pub struct DiskReadPermit {
    counters: Option<PermitCounters>,
    physical_disk_id: String,
}

impl DiskReadPermit {
    /// 返回当前许可覆盖的稳定物理盘显示身份。
    pub fn physical_disk_id(&self) -> &str {
        &self.physical_disk_id
    }
}

impl Drop for DiskReadPermit {
    fn drop(&mut self) {
        let Some(counters) = self.counters.take() else {
            return;
        };
        // 反向释放：先释放每盘类别与 total，再释放全局类别与 total。
        for disk in counters.disk_counters.iter().rev() {
            disk.class.fetch_sub(1, Ordering::AcqRel);
            disk.total.fetch_sub(1, Ordering::AcqRel);
        }
        counters.global_class.fetch_sub(1, Ordering::AcqRel);
        counters.global_total.fetch_sub(1, Ordering::AcqRel);
        // 只有实际磁盘和全局槽位都释放后才解除 lane 权重冻结，避免暴露中间状态。
        if let Some(active) = counters.lane_active {
            active.fetch_sub(1, Ordering::AcqRel);
        }
        counters.notify.notify_one();
    }
}

struct PermitCounters {
    /// 授予时冻结的全局 total/class 计数。
    global_total: Arc<AtomicUsize>,
    global_class: Arc<AtomicUsize>,
    /// 复合位置中每个底层盘的 total/class 计数。
    disk_counters: Vec<DiskPermitCounters>,
    /// 该 lane 的活动许可计数，用于保持冻结配置直到最后一个 permit 释放。
    lane_active: Option<Arc<AtomicUsize>>,
    notify: Arc<Notify>,
}

/// 一个底层盘在复合许可中的冻结计数引用。
struct DiskPermitCounters {
    total: Arc<AtomicUsize>,
    class: Arc<AtomicUsize>,
}

/// actor 与 permit 共享的 total、Hash、Media 原子计数集合。
#[derive(Clone)]
struct ActiveCounters {
    total: Arc<AtomicUsize>,
    hash: Arc<AtomicUsize>,
    media: Arc<AtomicUsize>,
}

impl ActiveCounters {
    /// 创建归零计数；增加只在 actor 中发生，释放由 permit Drop 完成。
    fn new() -> Self {
        Self {
            total: Arc::new(AtomicUsize::new(0)),
            hash: Arc::new(AtomicUsize::new(0)),
            media: Arc::new(AtomicUsize::new(0)),
        }
    }

    /// 返回指定类别的计数引用，供 permit 冻结所有权。
    fn class(&self, class: DiskReadClass) -> Arc<AtomicUsize> {
        match class {
            DiskReadClass::HashSequential => self.hash.clone(),
            DiskReadClass::MediaDecode => self.media.clone(),
        }
    }

    /// 读取指定类别的 active 数，用于压力比较。
    fn load_class(&self, class: DiskReadClass) -> usize {
        self.class(class).load(Ordering::Acquire)
    }
}

/// 可克隆的物理磁盘读取调度入口；所有顺序决策由一个 actor 完成。
#[derive(Clone)]
pub struct DiskReadScheduler {
    commands: mpsc::Sender<Command>,
    queue_slots: Arc<Semaphore>,
    #[cfg(test)]
    request_capacity: usize,
}

impl DiskReadScheduler {
    /// 使用已验证的磁盘读取配置和实际 Worker 数创建调度 actor。
    pub fn new(
        config: &DiskReadConfig,
        effective_worker_count: usize,
    ) -> Result<Self, SchedulerError> {
        validate_config(config, effective_worker_count)?;
        let request_capacity = config
            .total_threads
            .checked_mul(4)
            .and_then(|total| {
                effective_worker_count
                    .checked_mul(2)
                    .map(|workers| total.max(workers))
            })
            .ok_or(SchedulerError::InvalidConfiguration("请求队列容量溢出"))?;
        let runtime =
            tokio::runtime::Handle::try_current().map_err(|_| SchedulerError::MissingRuntime)?;
        let (commands, receiver) = mpsc::channel(request_capacity);
        let notify = Arc::new(Notify::new());
        runtime.spawn(run_actor(
            receiver,
            ActorConfig::from(config, effective_worker_count),
            notify,
        ));
        Ok(Self {
            commands,
            queue_slots: Arc::new(Semaphore::new(request_capacity)),
            #[cfg(test)]
            request_capacity,
        })
    }

    /// 等待一个同时满足全局和当前物理盘上限的读取许可。
    pub async fn acquire(
        &self,
        location: StorageLocation,
        class: DiskReadClass,
    ) -> Result<DiskReadPermit, SchedulerError> {
        self.acquire_key(
            DiskKey::new(location.physical_disk_id().disk_numbers())?,
            location.disk_kind(),
            class,
            None,
            None,
        )
        .await
    }

    /// 按调用方冻结的逐盘上限取得许可；同一 scheduler 仍统一维护全局和逐盘状态。
    pub async fn acquire_with_limit(
        &self,
        location: StorageLocation,
        class: DiskReadClass,
        per_disk_limit: usize,
    ) -> Result<DiskReadPermit, SchedulerError> {
        if per_disk_limit == 0 {
            return Err(SchedulerError::InvalidConfiguration(
                "冻结的逐盘读取额度必须大于零",
            ));
        }
        self.acquire_key(
            DiskKey::new(location.physical_disk_id().disk_numbers())?,
            location.disk_kind(),
            class,
            Some(per_disk_limit),
            None,
        )
        .await
    }

    /// 按任务文件冻结的 lane 申请许可；权重只进入同一个 scheduler actor。
    pub async fn acquire_lane(
        &self,
        lane: DiskReadLane,
        class: DiskReadClass,
    ) -> Result<DiskReadPermit, SchedulerError> {
        if lane.effective_limit == 0 {
            return Err(SchedulerError::InvalidConfiguration(
                "冻结的逐盘读取额度必须大于零",
            ));
        }
        if lane.configured_weight == 0 {
            return Err(SchedulerError::InvalidConfiguration(
                "冻结的物理盘调度权重必须大于零",
            ));
        }
        self.acquire_key(
            DiskKey::new(lane.location.physical_disk_id().disk_numbers())?,
            lane.location.disk_kind(),
            class,
            Some(lane.effective_limit),
            Some(lane.configured_weight),
        )
        .await
    }

    /// 关闭 actor；全部等待请求和后续请求都返回 `Closed`。
    pub async fn shutdown(&self) -> Result<(), SchedulerError> {
        let (reply, response) = oneshot::channel();
        self.commands
            .send(Command::Shutdown(reply))
            .await
            .map_err(|_| SchedulerError::Closed)?;
        response.await.map_err(|_| SchedulerError::Closed)
    }

    async fn acquire_key(
        &self,
        key: DiskKey,
        kind: LocalDiskKind,
        class: DiskReadClass,
        per_disk_limit: Option<usize>,
        configured_weight: Option<usize>,
    ) -> Result<DiskReadPermit, SchedulerError> {
        let queue_slot = self
            .queue_slots
            .clone()
            .acquire_owned()
            .await
            .map_err(|_| SchedulerError::Closed)?;
        let (reply, response) = oneshot::channel();
        self.commands
            .send(Command::Acquire(Waiter {
                key,
                kind,
                per_disk_limit,
                configured_weight,
                class,
                sequence: 0,
                conflicting_bypasses: 0,
                reply,
                _queue_slot: queue_slot,
            }))
            .await
            .map_err(|_| SchedulerError::Closed)?;
        response.await.map_err(|_| SchedulerError::Closed)?
    }

    #[doc(hidden)]
    pub async fn acquire_for_test(
        &self,
        disk_numbers: &[u32],
        kind: LocalDiskKind,
        class: DiskReadClass,
    ) -> Result<DiskReadPermit, SchedulerError> {
        self.acquire_key(DiskKey::new(disk_numbers)?, kind, class, None, None)
            .await
    }

    #[cfg(test)]
    pub(super) const fn request_capacity_for_test(&self) -> usize {
        self.request_capacity
    }

    #[cfg(test)]
    pub(crate) async fn barrier_for_test(&self) -> Result<(), SchedulerError> {
        let (reply, response) = oneshot::channel();
        self.commands
            .send(Command::Barrier(reply))
            .await
            .map_err(|_| SchedulerError::Closed)?;
        response.await.map_err(|_| SchedulerError::Closed)
    }

    #[cfg(test)]
    pub(crate) async fn active_snapshot_for_test(
        &self,
        disk_numbers: &[u32],
    ) -> Result<ActiveSnapshot, SchedulerError> {
        let (reply, response) = oneshot::channel();
        self.commands
            .send(Command::Snapshot {
                disk_numbers: disk_numbers.to_vec(),
                reply,
            })
            .await
            .map_err(|_| SchedulerError::Closed)?;
        response.await.map_err(|_| SchedulerError::Closed)
    }

    /// 测试专用：让下一次队首交付模拟响应端已关闭，验证公平状态只在发送成功后提交。
    #[cfg(test)]
    pub(super) async fn drop_next_reply_for_test(&self) -> Result<(), SchedulerError> {
        let (reply, response) = oneshot::channel();
        self.commands
            .send(Command::DropNextReply(reply))
            .await
            .map_err(|_| SchedulerError::Closed)?;
        response.await.map_err(|_| SchedulerError::Closed)
    }
}

#[derive(Clone, Debug, Eq, Ord, PartialEq, PartialOrd)]
struct DiskKey(Vec<u32>);

impl DiskKey {
    fn new(disk_numbers: &[u32]) -> Result<Self, SchedulerError> {
        if disk_numbers.is_empty() {
            return Err(SchedulerError::InvalidConfiguration("物理盘身份不能为空"));
        }
        let mut numbers = disk_numbers.to_vec();
        numbers.sort_unstable();
        numbers.dedup();
        Ok(Self(numbers))
    }

    /// 判断两个位置是否共享至少一个底层物理盘。
    fn intersects(&self, other: &Self) -> bool {
        self.0
            .iter()
            .any(|disk_number| other.0.binary_search(disk_number).is_ok())
    }
}

enum Command {
    Acquire(Waiter),
    #[cfg(test)]
    Barrier(oneshot::Sender<()>),
    #[cfg(test)]
    Snapshot {
        disk_numbers: Vec<u32>,
        reply: oneshot::Sender<ActiveSnapshot>,
    },
    #[cfg(test)]
    DropNextReply(oneshot::Sender<()>),
    Shutdown(oneshot::Sender<()>),
}

/// 测试专用活动计数快照；生产 API 不暴露调度内部原子计数。
#[cfg(test)]
pub(crate) struct ActiveSnapshot {
    pub(crate) global_total: usize,
    pub(crate) global_hash: usize,
    pub(crate) global_media: usize,
    pub(crate) disks: Vec<(u32, usize, usize, usize)>,
    /// 指定底层盘当前仍在 scheduler FIFO 内的 total、Hash、Media 等待数。
    pub(crate) waiting: Vec<(u32, usize, usize, usize)>,
}

struct Waiter {
    key: DiskKey,
    kind: LocalDiskKind,
    /// 可选的本轮冻结逐盘上限；缺失时使用调度器默认介质配置。
    per_disk_limit: Option<usize>,
    /// 可选的本轮冻结调度权重；缺失时保留旧的等权入口语义。
    configured_weight: Option<usize>,
    /// 请求进入 actor 的读取类别。
    class: DiskReadClass,
    /// 跨位置、跨类别单调递增的入队顺序。
    sequence: u64,
    /// 被更年轻且共享底层盘的请求成功绕过次数。
    conflicting_bypasses: u8,
    reply: oneshot::Sender<Result<DiskReadPermit, SchedulerError>>,
    _queue_slot: OwnedSemaphorePermit,
}

#[derive(Default)]
struct LocationQueue {
    /// 当前位置的 Hash FIFO。
    hash_waiting: VecDeque<Waiter>,
    /// 当前位置的媒体读取 FIFO。
    media_waiting: VecDeque<Waiter>,
}

impl LocationQueue {
    /// 返回指定类别 FIFO 的开放队首。
    fn head(&self, class: DiskReadClass) -> Option<&Waiter> {
        match class {
            DiskReadClass::HashSequential => self.hash_waiting.front(),
            DiskReadClass::MediaDecode => self.media_waiting.front(),
        }
    }

    /// 弹出指定类别 FIFO 的开放队首。
    fn pop_front(&mut self, class: DiskReadClass) -> Option<Waiter> {
        match class {
            DiskReadClass::HashSequential => self.hash_waiting.pop_front(),
            DiskReadClass::MediaDecode => self.media_waiting.pop_front(),
        }
    }

    /// 返回两个类别均无等待项的状态。
    fn is_empty(&self) -> bool {
        self.hash_waiting.is_empty() && self.media_waiting.is_empty()
    }

    /// 判断当前位置是否仍有加权入口等待项。
    fn has_weighted_waiter(&self) -> bool {
        self.hash_waiting
            .iter()
            .chain(self.media_waiting.iter())
            .filter(|waiter| !waiter.reply.is_closed())
            .any(|waiter| waiter.configured_weight.is_some())
    }

    /// 判断当前位置是否仍有旧入口等待项，用于在加权请求离队后重建等权占位。
    fn has_legacy_waiter(&self) -> bool {
        self.hash_waiting
            .iter()
            .chain(self.media_waiting.iter())
            .filter(|waiter| !waiter.reply.is_closed())
            .any(|waiter| waiter.configured_weight.is_none())
    }

    /// 返回当前位置冻结的加权值；入队校验保证多个加权项值一致。
    fn configured_weight(&self) -> Option<usize> {
        self.hash_waiting
            .iter()
            .chain(self.media_waiting.iter())
            .filter(|waiter| !waiter.reply.is_closed())
            .find_map(|waiter| waiter.configured_weight)
    }

    /// 清除已取消的两个类别队首，避免占住真实 FIFO 请求。
    fn prune_closed_heads(&mut self) {
        // retain 保持存活项的 FIFO 顺序，同时释放任意位置已经取消的队列槽位。
        self.hash_waiting.retain(|waiter| !waiter.reply.is_closed());
        self.media_waiting
            .retain(|waiter| !waiter.reply.is_closed());
    }
}

struct UnderlyingDiskState {
    /// 该物理盘所有许可共享的 total/class 活动计数。
    active: ActiveCounters,
    limit: usize,
    /// 按当前最小硬上限和冻结 Worker 数计算的软目标。
    nominal: NominalSeats,
    /// 此盘在真实双类竞争中连续授予媒体读取的次数，竞争 Hash 成功后归零。
    media_streak: u8,
}

/// 单条物理盘 lane 的加权轮转和冻结配置；permit 所有权仍由现有计数器持有。
struct WeightedLaneState {
    /// 本轮配置提供的调度权重。
    configured_weight: usize,
    /// 尚未消费的任务单位，跨全局窗口保留以维持长期比例。
    deficit: usize,
    /// 当前 lane 是否仍有带权等待项；false 表示仅为 legacy 等权入口占位。
    has_weighted_waiter: bool,
    /// 是否已有真实 weighted 配置需要冻结；legacy 占位不设置该标志。
    has_frozen_weight: bool,
    /// 该 lane 当前持有的 permit 数；为零后才可释放冻结配置。
    active_permits: Arc<AtomicUsize>,
}

/// 一次不含队首引用的加权选择结果，供 actor 在释放借用后提交亏欠变化。
struct WeightedChoice {
    /// 被选中的物理盘 lane。
    key: DiskKey,
    /// 被选中的读取类别。
    class: DiskReadClass,
    /// 被选队首的稳定入队序号。
    sequence: u64,
    /// 消费一个任务单位后的 lane 亏欠。
    remaining_deficit: usize,
    /// 当前 lane 额度耗尽后应检查的下一游标。
    next_cursor: DiskKey,
}

/// 一次队首选择；sequence 用于在压力比例相同时保持年龄优先。
struct WaiterSelection {
    /// 被选中的复合磁盘身份。
    key: DiskKey,
    /// 被选中的读取类别。
    class: DiskReadClass,
    /// 选中队首的稳定入队顺序。
    sequence: u64,
    /// 加权状态更新；仅在响应发送成功后由 actor 应用。
    weighted_choice: Option<WeightedChoice>,
}

#[derive(Clone, Copy)]
struct ActorConfig {
    hdd_limit: usize,
    ssd_limit: usize,
    unknown_limit: usize,
    total_limit: usize,
    /// 任务启动时冻结的有效 Worker 数。
    worker_count: usize,
}

impl ActorConfig {
    /// 从配置和冻结 Worker 数复制 actor 只读参数。
    fn from(config: &DiskReadConfig, worker_count: usize) -> Self {
        Self {
            hdd_limit: config.hdd_threads_per_disk,
            ssd_limit: config.ssd_threads_per_disk,
            unknown_limit: config.unknown_threads_per_disk,
            total_limit: config.total_threads,
            worker_count,
        }
    }

    /// 按最新观察到的硬上限重算名义 seat；构造阶段已校验不会溢出。
    fn nominal(self, limit: usize) -> NominalSeats {
        nominal_seats(limit, self.worker_count).expect("配置验证已保证名义 seat 计算不会溢出")
    }

    const fn disk_limit(self, kind: LocalDiskKind) -> usize {
        match kind {
            LocalDiskKind::Hdd => self.hdd_limit,
            LocalDiskKind::Ssd => self.ssd_limit,
            LocalDiskKind::Unknown => self.unknown_limit,
        }
    }
}

struct ActorState {
    config: ActorConfig,
    notify: Arc<Notify>,
    /// 全局 total/class 活动计数；permit 持有同一批原子引用。
    global_active: ActiveCounters,
    /// 全局名义 seat 在任务生命周期内固定不变。
    global_nominal: NominalSeats,
    queues: BTreeMap<DiskKey, LocationQueue>,
    underlying_disks: BTreeMap<u32, UnderlyingDiskState>,
    rotation: VecDeque<DiskKey>,
    in_rotation: BTreeSet<DiskKey>,
    /// 下一个请求的稳定入队序号，用于冲突老化保护。
    next_sequence: u64,
    /// 全局唯一的老化保留身份。
    aged_reservation: Option<AgedReservation>,
    /// 测试专用响应失败开关，不进入生产状态。
    #[cfg(test)]
    drop_next_reply: bool,
    /// 每个加权 lane 的唯一亏欠状态，和现有 active/老化状态同属 actor。
    weighted_lanes: BTreeMap<DiskKey, WeightedLaneState>,
    /// 当前是否存在加权入口；纯旧入口为 false，保持原有等权选择路径。
    weighted_mode: bool,
    /// 当前加权轮转游标；lane 消失后会清除，不累计历史突发额度。
    weighted_cursor: Option<DiskKey>,
}

impl ActorState {
    fn new(config: ActorConfig, notify: Arc<Notify>) -> Self {
        Self {
            global_nominal: config.nominal(config.total_limit),
            config,
            notify,
            global_active: ActiveCounters::new(),
            queues: BTreeMap::new(),
            underlying_disks: BTreeMap::new(),
            rotation: VecDeque::new(),
            in_rotation: BTreeSet::new(),
            next_sequence: 0,
            aged_reservation: None,
            #[cfg(test)]
            drop_next_reply: false,
            weighted_lanes: BTreeMap::new(),
            weighted_mode: false,
            weighted_cursor: None,
        }
    }

    /// 按读取类别进入当前位置 FIFO，并注册复合位置的全部底层盘。
    fn enqueue(&mut self, mut waiter: Waiter) {
        let key = waiter.key.clone();
        let observed_limit = waiter
            .per_disk_limit
            .unwrap_or_else(|| self.config.disk_limit(waiter.kind));

        // 先验证本次冻结上限，避免异常数值进入 actor 后在 nominal 计算处 panic。
        if nominal_seats(observed_limit, self.config.worker_count).is_err() {
            let _ = waiter.reply.send(Err(SchedulerError::InvalidConfiguration(
                "冻结的逐盘读取额度无法计算名义 seat",
            )));
            return;
        }

        // 同一物理盘的加权配置在本轮必须冻结；冲突请求直接失败且不进入队列。
        if let Some(configured_weight) = waiter.configured_weight {
            let queued_weight = self
                .queues
                .get(&key)
                .and_then(LocationQueue::configured_weight);
            let active_weight = self.weighted_lanes.get(&key).and_then(|lane| {
                (lane.has_frozen_weight && lane.active_permits.load(Ordering::Acquire) > 0)
                    .then_some(lane.configured_weight)
            });
            if queued_weight
                .or(active_weight)
                .is_some_and(|existing| existing != configured_weight)
            {
                let _ = waiter.reply.send(Err(SchedulerError::InvalidConfiguration(
                    "同一物理盘的冻结调度权重不能变化",
                )));
                return;
            }
        }

        waiter.sequence = self.next_sequence;
        self.next_sequence = self.next_sequence.wrapping_add(1);
        let had_weighted_waiter = self
            .queues
            .get(&key)
            .is_some_and(LocationQueue::has_weighted_waiter);
        if waiter.configured_weight.is_some() && !self.weighted_mode {
            // 加权入口首次出现时，把已经排队的旧入口按等权 1 纳入同一个 actor 状态。
            self.weighted_mode = true;
            let existing_keys = self.queues.keys().cloned().collect::<Vec<_>>();
            for existing_key in existing_keys {
                self.weighted_lanes
                    .entry(existing_key)
                    .or_insert(WeightedLaneState {
                        configured_weight: 1,
                        deficit: 0,
                        has_weighted_waiter: false,
                        has_frozen_weight: false,
                        active_permits: Arc::new(AtomicUsize::new(0)),
                    });
            }
        }
        if self.weighted_mode {
            let configured_weight = waiter.configured_weight.unwrap_or(1);
            let lane = self
                .weighted_lanes
                .entry(key.clone())
                .or_insert(WeightedLaneState {
                    configured_weight,
                    deficit: 0,
                    has_weighted_waiter: false,
                    has_frozen_weight: false,
                    active_permits: Arc::new(AtomicUsize::new(0)),
                });
            if waiter.configured_weight.is_some() && !had_weighted_waiter {
                // 加权 lane 重新出现时从零开始，不能继承之前已经消费的突发额度。
                lane.configured_weight = configured_weight;
                lane.deficit = 0;
                lane.has_weighted_waiter = true;
                lane.has_frozen_weight = true;
            }
        }
        for disk_number in &key.0 {
            self.underlying_disks
                .entry(*disk_number)
                .and_modify(|disk| {
                    let effective_limit = disk.limit.min(observed_limit);
                    if effective_limit != disk.limit {
                        disk.limit = effective_limit;
                        disk.nominal = self.config.nominal(effective_limit);
                    }
                })
                .or_insert_with(|| UnderlyingDiskState {
                    active: ActiveCounters::new(),
                    limit: observed_limit,
                    nominal: self.config.nominal(observed_limit),
                    media_streak: 0,
                });
        }
        let queue = self.queues.entry(key.clone()).or_default();
        match waiter.class {
            DiskReadClass::HashSequential => queue.hash_waiting.push_back(waiter),
            DiskReadClass::MediaDecode => queue.media_waiting.push_back(waiter),
        }
        self.rotate(key);
    }

    fn rotate(&mut self, key: DiskKey) {
        if self.in_rotation.insert(key.clone()) {
            self.rotation.push_back(key);
        }
    }

    /// 反复授予当前可运行请求，直到全局容量用尽或所有队首均被磁盘冲突阻塞。
    fn grant_waiters(&mut self) {
        loop {
            self.prune_closed_heads();
            if self.global_active.total.load(Ordering::Acquire) >= self.config.total_limit {
                return;
            }
            let Some(selection) = self.select_waiter() else {
                return;
            };
            let WaiterSelection {
                key,
                class,
                sequence,
                weighted_choice,
            } = selection;
            let waiter = self
                .queues
                .get_mut(&key)
                .and_then(|queue| queue.pop_front(class))
                .expect("候选请求必须仍是对应类别的 FIFO 队首");
            self.move_rotation_to_tail(&key);

            // 该授予是否填满最后一个 global seat，供老化 bypass 记录使用。
            let occupies_last_global_seat =
                self.global_active.total.load(Ordering::Acquire) + 1 >= self.config.total_limit;
            let lane_active = self
                .weighted_lanes
                .get(&key)
                .map(|lane| lane.active_permits.clone());
            if let Some(active) = &lane_active {
                active.fetch_add(1, Ordering::AcqRel);
            }
            let (global_class, disk_counters) = self.reserve_all(&key, class);
            let permit = DiskReadPermit {
                physical_disk_id: format!(
                    "PhysicalDisk{}",
                    key.0
                        .iter()
                        .map(u32::to_string)
                        .collect::<Vec<_>>()
                        .join("+")
                ),
                counters: Some(PermitCounters {
                    global_total: self.global_active.total.clone(),
                    global_class,
                    disk_counters,
                    lane_active,
                    notify: self.notify.clone(),
                }),
            };
            let delivered = {
                #[cfg(test)]
                if self.drop_next_reply {
                    self.drop_next_reply = false;
                    drop(waiter.reply);
                    drop(permit);
                    false
                } else {
                    waiter.reply.send(Ok(permit)).is_ok()
                }
                #[cfg(not(test))]
                {
                    waiter.reply.send(Ok(permit)).is_ok()
                }
            };
            if delivered {
                self.note_successful_grant(&key, class, sequence, occupies_last_global_seat);
                if let Some(choice) = weighted_choice.as_ref() {
                    self.apply_weighted_choice(choice);
                }
            }
        }
    }

    /// 删除取消队首并同步位置轮转集合。
    fn prune_closed_heads(&mut self) {
        for queue in self.queues.values_mut() {
            queue.prune_closed_heads();
        }
        let empty = self
            .queues
            .iter()
            .filter_map(|(key, queue)| queue.is_empty().then_some(key.clone()))
            .collect::<Vec<_>>();
        for key in empty {
            self.rotation.retain(|queued| queued != &key);
            self.in_rotation.remove(&key);
            let release_frozen = self
                .weighted_lanes
                .get(&key)
                .is_none_or(|lane| lane.active_permits.load(Ordering::Acquire) == 0);
            if let Some(lane) = self.weighted_lanes.get_mut(&key) {
                // 没有等待项时 deficit 已失去意义；冻结配置仍需等待活动 permit 归零。
                lane.deficit = 0;
                lane.has_weighted_waiter = false;
            }
            if release_frozen {
                self.weighted_lanes.remove(&key);
            }
            if self
                .weighted_cursor
                .as_ref()
                .is_some_and(|cursor| cursor == &key)
            {
                self.weighted_cursor = None;
            }
        }
        // 加权项全部离开但 legacy 仍在排队时，保留一个全新的等权占位，
        // 清掉旧 lane 的 deficit，避免后续重新出现加权请求继承历史突发。
        let legacy_only = self
            .queues
            .iter()
            .filter_map(|(key, queue)| {
                (queue.has_legacy_waiter() && !queue.has_weighted_waiter()).then_some(key.clone())
            })
            .collect::<Vec<_>>();
        for key in legacy_only {
            if let Some(lane) = self.weighted_lanes.get_mut(&key) {
                lane.deficit = 0;
                lane.has_weighted_waiter = false;
                if lane.active_permits.load(Ordering::Acquire) == 0 {
                    lane.configured_weight = 1;
                    lane.has_frozen_weight = false;
                }
            }
        }
        // weighted_mode 只反映当前仍开放的 weighted waiter，不由状态 map 是否为空决定。
        self.weighted_mode = self.queues.values().any(LocationQueue::has_weighted_waiter);
        if !self.weighted_mode {
            self.weighted_cursor = None;
        }
    }

    /// 按老化保留、T=1 轮换、活动 seat 压力和位置轮转选择原子队首。
    fn select_waiter(&mut self) -> Option<WaiterSelection> {
        self.refresh_aged_reservation();
        let heads = self.open_heads();
        let grantable = heads
            .iter()
            .enumerate()
            .filter_map(|(index, (key, _, _))| self.can_reserve_all(key).then_some(index))
            .collect::<Vec<_>>();
        if grantable.is_empty() {
            return None;
        }

        let eligible = if let Some(reservation) = &self.aged_reservation {
            if let Some(index) = grantable.iter().copied().find(|index| {
                let (key, class, waiter) = &heads[*index];
                key == &reservation.key
                    && *class == reservation.class
                    && waiter.sequence == reservation.sequence
            }) {
                let mut selection = self.make_selection(&heads[index]);
                // 老化直通也是一次真实授予；提交后清零该 lane 的 deficit，
                // 避免保护性放行紧接着制造新的突发。
                selection.weighted_choice = self.aged_weighted_choice(&heads, &grantable, index);
                return Some(selection);
            }
            // 保留暂时不可授予时，只冻结真正相交或会占用最后全局 seat 的年轻请求。
            grantable
                .iter()
                .copied()
                .filter(|index| {
                    let (key, _, _) = &heads[*index];
                    !key.intersects(&reservation.key) && !self.occupies_last_global_seat()
                })
                .collect::<Vec<_>>()
        } else {
            grantable
        };
        if eligible.is_empty() {
            return None;
        }

        // 先按物理 lane 权重选 lane，再在选中 lane 内执行 T=1 与类别裁决。
        if let Some(choice) = self.select_weighted_lane(&heads, &eligible) {
            let selection = WaiterSelection {
                key: choice.key.clone(),
                class: choice.class,
                sequence: choice.sequence,
                weighted_choice: Some(choice),
            };
            return Some(selection);
        }
        // 旧等权入口仍沿用原有的 T=1 争用组压缩。
        let capacity_candidates = self.select_capacity_one_representatives(&heads, &eligible);
        let index = self.select_pressure_or_rotation(&heads, &capacity_candidates);
        Some(self.make_selection(&heads[index]))
    }

    /// 在真实可授予的 lane 之间先补齐当前权重席位，再消费长期亏欠并沿用类别规则。
    fn select_weighted_lane(
        &self,
        heads: &[(DiskKey, DiskReadClass, &Waiter)],
        candidates: &[usize],
    ) -> Option<WeightedChoice> {
        if !self.weighted_mode {
            return None;
        }
        let keys = candidates
            .iter()
            .map(|index| heads[*index].0.clone())
            .collect::<BTreeSet<_>>()
            .into_iter()
            .collect::<Vec<_>>();
        if keys.is_empty() {
            return None;
        }

        // 当前窗口优先选择 active/weight 最小的盘。Ready 盘都从 0 开始时会先各得一个；
        // Ready 盘多于全局席位时，后续释放从零压力盘继续轮转，不预占正在执行的 permit。
        let least_pressure_key = keys
            .iter()
            .min_by(|left, right| self.compare_weighted_active_pressure(left, right))?;
        let weight_divisor = keys
            .iter()
            .map(|key| self.effective_weight(key))
            .reduce(greatest_common_divisor)
            .unwrap_or(1);

        // 游标仍在全部 Ready lane 上移动；压力较高的盘只跳过本次，不丢失长期轮转位置。
        let start = self
            .weighted_cursor
            .as_ref()
            .and_then(|cursor| keys.iter().position(|key| key == cursor))
            .unwrap_or_else(|| {
                // 新一轮从当前欠配额集合中的最高权重开始，之后由游标和亏欠维持比例。
                keys.iter()
                    .enumerate()
                    .filter(|(_, key)| {
                        self.compare_weighted_active_pressure(key, least_pressure_key)
                            == CmpOrdering::Equal
                    })
                    .max_by_key(|(_, key)| self.effective_weight(key))
                    .map_or(0, |(index, _)| index)
            });
        for offset in 0..keys.len() {
            let key_index = (start + offset) % keys.len();
            let key = &keys[key_index];
            if self.compare_weighted_active_pressure(key, least_pressure_key) != CmpOrdering::Equal
            {
                continue;
            }
            let Some(lane) = self.weighted_lanes.get(key) else {
                continue;
            };
            // 权重表达比例而非连续突发长度；16:16 与 1:1 必须产生同样的轮转。
            let configured_weight = self.effective_weight(key) / weight_divisor;
            let deficit = if lane.deficit == 0 {
                configured_weight
            } else {
                // Ready 集合变化会改变约分因子，旧亏欠不能超过当前归一化额度。
                lane.deficit.min(configured_weight)
            };
            if deficit == 0 {
                continue;
            }
            let remaining_deficit = deficit - 1;
            let next_cursor = if remaining_deficit == 0 {
                keys[(key_index + 1) % keys.len()].clone()
            } else {
                key.clone()
            };

            let lane_candidates = candidates
                .iter()
                .copied()
                .filter(|index| heads[*index].0 == *key)
                .collect::<Vec<_>>();
            let capacity_candidates =
                self.select_capacity_one_representatives(heads, &lane_candidates);
            let index = self.select_pressure_or_rotation(heads, &capacity_candidates);
            return Some(WeightedChoice {
                key: key.clone(),
                class: heads[index].1,
                sequence: heads[index].2.sequence,
                remaining_deficit,
                next_cursor,
            });
        }
        None
    }

    /// 返回 lane 当前参与全局分配的权重；加权请求离队后的 legacy 入口按 1 处理。
    fn effective_weight(&self, key: &DiskKey) -> usize {
        self.weighted_lanes.get(key).map_or(1, |lane| {
            if lane.has_weighted_waiter {
                lane.configured_weight
            } else {
                1
            }
        })
    }

    /// 交叉相乘比较两个 lane 的 active/weight，避免浮点误差和整数除法截断。
    fn compare_weighted_active_pressure(&self, left: &DiskKey, right: &DiskKey) -> CmpOrdering {
        let left_active = self
            .weighted_lanes
            .get(left)
            .map_or(0, |lane| lane.active_permits.load(Ordering::Acquire));
        let right_active = self
            .weighted_lanes
            .get(right)
            .map_or(0, |lane| lane.active_permits.load(Ordering::Acquire));
        let left_weight = self.effective_weight(left);
        let right_weight = self.effective_weight(right);

        (left_active as u128 * right_weight as u128)
            .cmp(&(right_active as u128 * left_weight as u128))
    }

    /// 应用一次已经完成队首判定的加权选择，避免在借用队首时修改 actor。
    fn apply_weighted_choice(&mut self, choice: &WeightedChoice) {
        if let Some(lane) = self.weighted_lanes.get_mut(&choice.key) {
            lane.deficit = choice.remaining_deficit;
        }
        self.weighted_cursor = Some(choice.next_cursor.clone());
    }

    /// 为老化直通准备一次延迟提交的加权状态；不向该 lane 额外补发 deficit。
    fn aged_weighted_choice(
        &self,
        heads: &[(DiskKey, DiskReadClass, &Waiter)],
        candidates: &[usize],
        selected: usize,
    ) -> Option<WeightedChoice> {
        if !self.weighted_mode {
            return None;
        }
        let key = &heads[selected].0;
        let keys = candidates
            .iter()
            .map(|index| heads[*index].0.clone())
            .collect::<BTreeSet<_>>()
            .into_iter()
            .collect::<Vec<_>>();
        let key_index = keys.iter().position(|candidate| candidate == key)?;
        Some(WeightedChoice {
            key: key.clone(),
            class: heads[selected].1,
            sequence: heads[selected].2.sequence,
            // 老化属于一次保护性成功，不把已积累的额度带入下一次轮转。
            remaining_deficit: 0,
            next_cursor: keys[(key_index + 1) % keys.len()].clone(),
        })
    }

    /// 刷新唯一老化保留；取消或队首身份变化时清除旧身份。
    fn refresh_aged_reservation(&mut self) {
        let (still_head, oldest_aged) = {
            let heads = self.open_heads();
            let still_head = self.aged_reservation.as_ref().is_none_or(|reservation| {
                heads.iter().any(|(key, class, waiter)| {
                    key == &reservation.key
                        && *class == reservation.class
                        && waiter.sequence == reservation.sequence
                })
            });
            let oldest_aged = heads
                .iter()
                .filter(|(_, _, waiter)| waiter.conflicting_bypasses >= MAX_CONFLICTING_BYPASSES)
                .min_by_key(|(_, _, waiter)| waiter.sequence)
                .map(|(key, class, waiter)| AgedReservation {
                    key: key.clone(),
                    class: *class,
                    sequence: waiter.sequence,
                });
            (still_head, oldest_aged)
        };
        if !still_head {
            self.aged_reservation = None;
        }
        if self.aged_reservation.is_none() {
            self.aged_reservation = oldest_aged;
        }
    }

    /// 计算候选涉及的 T=1 资源；T=1 不参与比例压力。
    fn has_capacity_one(&self, key: &DiskKey) -> bool {
        key.0.iter().any(|disk_number| {
            self.underlying_disks
                .get(disk_number)
                .is_some_and(|disk| disk.limit == 1)
        })
    }

    /// 仅判断两个候选是否共享同一个 T=1 底层盘，供冲突组压缩使用。
    fn capacity_one_intersects(&self, left: &DiskKey, right: &DiskKey) -> bool {
        left.0.iter().any(|disk_number| {
            right.0.binary_search(disk_number).is_ok()
                && self
                    .underlying_disks
                    .get(disk_number)
                    .is_some_and(|disk| disk.limit == 1)
        })
    }

    /// 返回复合 T=1 位置的共同偏好；底层盘偏好冲突时返回 None。
    fn capacity_one_preference(&self, key: &DiskKey) -> Option<DiskReadClass> {
        let mut preference = None;
        for disk_number in &key.0 {
            let Some(disk) = self.underlying_disks.get(disk_number) else {
                continue;
            };
            if disk.limit != 1 {
                continue;
            }
            let current = if disk.media_streak < MEDIA_WEIGHT {
                DiskReadClass::MediaDecode
            } else {
                DiskReadClass::HashSequential
            };
            if preference.is_some_and(|previous| previous != current) {
                return None;
            }
            preference = Some(current);
        }
        preference
    }

    /// T=1 采用 Media×3→Hash×1；复合偏好冲突时选最老可原子请求。
    fn select_capacity_one(
        &self,
        heads: &[(DiskKey, DiskReadClass, &Waiter)],
        candidates: &[usize],
    ) -> Option<usize> {
        let capacity_one = candidates
            .iter()
            .copied()
            .filter(|index| self.has_capacity_one(&heads[*index].0))
            .collect::<Vec<_>>();
        if capacity_one.is_empty() {
            return None;
        }

        // T=1 轮换只能处理同一争用连通分量；跨盘候选必须留给全局年龄/压力裁决。
        if capacity_one.len() != candidates.len() {
            return None;
        }
        let mut related = vec![capacity_one[0]];
        loop {
            let previous_len = related.len();
            for index in capacity_one.iter().copied() {
                if !related.contains(&index)
                    && related.iter().any(|related_index| {
                        self.capacity_one_intersects(&heads[*related_index].0, &heads[index].0)
                    })
                {
                    related.push(index);
                }
            }
            if related.len() == previous_len {
                break;
            }
        }
        if related.len() != capacity_one.len() {
            return None;
        }

        if related
            .iter()
            .copied()
            .any(|index| self.capacity_one_preference(&heads[index].0).is_none())
        {
            return Some(self.oldest_head(heads, &related));
        }
        let preferred = related
            .iter()
            .copied()
            .filter(|index| {
                self.capacity_one_preference(&heads[*index].0)
                    .is_some_and(|preference| preference == heads[*index].1)
            })
            .collect::<Vec<_>>();
        if !preferred.is_empty() {
            // 同一 T=1 争用分量沿用 rotation，避免单个位置长 FIFO 垄断进度。
            return Some(preferred[0]);
        }
        Some(related[0])
    }

    /// 将同一 T=1 争用组压缩为轮换代表，并保留跨盘候选参与全局裁决。
    fn select_capacity_one_representatives(
        &self,
        heads: &[(DiskKey, DiskReadClass, &Waiter)],
        candidates: &[usize],
    ) -> Vec<usize> {
        let capacity_one = candidates
            .iter()
            .copied()
            .filter(|index| self.has_capacity_one(&heads[*index].0))
            .collect::<Vec<_>>();
        let mut assigned = BTreeSet::new();
        let mut representatives = Vec::with_capacity(candidates.len());

        for index in candidates.iter().copied() {
            if !self.has_capacity_one(&heads[index].0) {
                representatives.push(index);
                continue;
            }
            if !assigned.insert(index) {
                continue;
            }

            let mut related = vec![index];
            loop {
                let previous_len = related.len();
                for other in capacity_one.iter().copied() {
                    if !related.contains(&other)
                        && related.iter().any(|related_index| {
                            self.capacity_one_intersects(&heads[*related_index].0, &heads[other].0)
                        })
                    {
                        related.push(other);
                    }
                }
                if related.len() == previous_len {
                    break;
                }
            }
            assigned.extend(related.iter().copied());
            let representative = self
                .select_capacity_one(heads, &related)
                .expect("T=1 争用连通组必须产生轮换代表");
            representatives.push(representative);
        }
        representatives
    }

    /// 两类均可授予时选择授予后压力最低者；单类保留位置轮转顺序。
    fn select_pressure_or_rotation(
        &self,
        heads: &[(DiskKey, DiskReadClass, &Waiter)],
        candidates: &[usize],
    ) -> usize {
        let has_hash = candidates
            .iter()
            .any(|index| heads[*index].1 == DiskReadClass::HashSequential);
        let has_media = candidates
            .iter()
            .any(|index| heads[*index].1 == DiskReadClass::MediaDecode);
        if !(has_hash && has_media) {
            return candidates[0];
        }

        let mut best = candidates[0];
        let Some(mut best_pressure) = self.candidate_pressure(&heads[best].0, heads[best].1) else {
            // global=1 或 T=1 没有比例分母，跨盘候选按稳定入队年龄裁决。
            return self.oldest_head(heads, candidates);
        };
        for index in candidates.iter().copied().skip(1) {
            let Some(pressure) = self.candidate_pressure(&heads[index].0, heads[index].1) else {
                // 混合 T=1/T>=2 时仍保留缺少压力分母的候选，改用全局年龄。
                return self.oldest_head(heads, candidates);
            };
            match pressure.cmp(best_pressure) {
                CmpOrdering::Less => {
                    best = index;
                    best_pressure = pressure;
                }
                CmpOrdering::Equal if heads[index].2.sequence < heads[best].2.sequence => {
                    best = index;
                    best_pressure = pressure;
                }
                _ => {}
            }
        }
        best
    }

    /// 计算候选授予后的最大 global/底层盘类别压力。
    fn candidate_pressure(&self, key: &DiskKey, class: DiskReadClass) -> Option<PressureRatio> {
        let mut maximum = None;
        if self.config.total_limit >= 2 {
            maximum = Some(PressureRatio {
                numerator: self.global_active.load_class(class).saturating_add(1),
                denominator: self
                    .global_nominal
                    .for_class(class)
                    .expect("T>=2 全局名义 seat 必须存在"),
            });
        }
        for disk_number in &key.0 {
            let disk = self
                .underlying_disks
                .get(disk_number)
                .expect("enqueue 已注册每个底层物理盘");
            if disk.limit < 2 {
                continue;
            }
            let ratio = PressureRatio {
                numerator: disk.active.load_class(class).saturating_add(1),
                denominator: disk
                    .nominal
                    .for_class(class)
                    .expect("T>=2 底层盘名义 seat 必须存在"),
            };
            maximum = Some(match maximum {
                Some(current) if ratio.cmp(current) == CmpOrdering::Greater => ratio,
                Some(current) => current,
                None => ratio,
            });
        }
        maximum
    }

    /// 压力相同时用入队年龄；年龄相同时候选数组保持既有 rotation 顺序。
    fn oldest_head(
        &self,
        heads: &[(DiskKey, DiskReadClass, &Waiter)],
        candidates: &[usize],
    ) -> usize {
        candidates
            .iter()
            .copied()
            .min_by_key(|index| heads[*index].2.sequence)
            .expect("候选集合不能为空")
    }

    /// 以位置轮转顺序返回两个类别的开放 FIFO 队首。
    fn open_heads(&self) -> Vec<(DiskKey, DiskReadClass, &Waiter)> {
        let mut heads = Vec::new();
        for key in &self.rotation {
            let Some(queue) = self.queues.get(key) else {
                continue;
            };
            for class in [DiskReadClass::HashSequential, DiskReadClass::MediaDecode] {
                if let Some(waiter) = queue.head(class) {
                    heads.push((key.clone(), class, waiter));
                }
            }
        }
        heads
    }

    /// 成功交付后更新 T=1 轮换、老化 bypass，并清除已交付的老化保留。
    /// `occupies_last_global_seat` 为真时，所有更老队首都记录一次全局绕过。
    fn note_successful_grant(
        &mut self,
        key: &DiskKey,
        class: DiskReadClass,
        granted_sequence: u64,
        occupies_last_global_seat: bool,
    ) {
        for disk_number in &key.0 {
            let disk = self
                .underlying_disks
                .get_mut(disk_number)
                .expect("enqueue 已注册每个底层物理盘");
            if disk.limit != 1 {
                continue;
            }
            disk.media_streak = match class {
                DiskReadClass::HashSequential => 0,
                DiskReadClass::MediaDecode => disk.media_streak.saturating_add(1).min(MEDIA_WEIGHT),
            };
        }
        for queue in self.queues.values_mut() {
            for waiter in [
                queue.hash_waiting.front_mut(),
                queue.media_waiting.front_mut(),
            ]
            .into_iter()
            .flatten()
            {
                if waiter.sequence < granted_sequence
                    && (occupies_last_global_seat || waiter.key.intersects(key))
                {
                    waiter.conflicting_bypasses = waiter.conflicting_bypasses.saturating_add(1);
                }
            }
        }
        if self.aged_reservation.as_ref().is_some_and(|reservation| {
            reservation.key == *key
                && reservation.class == class
                && reservation.sequence == granted_sequence
        }) {
            self.aged_reservation = None;
        }
    }

    /// 将刚被考虑的位置移动到 round-robin 尾部；仍有等待项时继续参与轮转。
    fn move_rotation_to_tail(&mut self, key: &DiskKey) {
        self.rotation.retain(|queued| queued != key);
        self.in_rotation.remove(key);
        if self.queues.get(key).is_some_and(|queue| !queue.is_empty()) {
            self.rotate(key.clone());
        }
    }

    /// 将借用候选转换成不携带队首借用的选择结果。
    fn make_selection(&self, head: &(DiskKey, DiskReadClass, &Waiter)) -> WaiterSelection {
        WaiterSelection {
            key: head.0.clone(),
            class: head.1,
            sequence: head.2.sequence,
            weighted_choice: None,
        }
    }

    /// 判断当前授予是否会占用最后一个全局 seat。
    fn occupies_last_global_seat(&self) -> bool {
        self.global_active.total.load(Ordering::Acquire) + 1 >= self.config.total_limit
    }

    fn can_reserve_all(&self, key: &DiskKey) -> bool {
        if self.global_active.total.load(Ordering::Acquire) >= self.config.total_limit {
            return false;
        }
        key.0.iter().all(|disk_number| {
            let disk = self
                .underlying_disks
                .get(disk_number)
                .expect("enqueue 已注册每个底层物理盘");
            disk.active.total.load(Ordering::Acquire) < disk.limit
        })
    }

    /// 原子递增 global total/class 与所有底层盘 total/class，返回 permit 冻结引用。
    fn reserve_all(
        &self,
        key: &DiskKey,
        class: DiskReadClass,
    ) -> (Arc<AtomicUsize>, Vec<DiskPermitCounters>) {
        debug_assert!(self.can_reserve_all(key));
        let global_class = self.global_active.class(class);
        // 计数增加顺序固定为 global total → global class → 每盘 total → 每盘 class。
        self.global_active.total.fetch_add(1, Ordering::AcqRel);
        global_class.fetch_add(1, Ordering::AcqRel);
        let mut disk_counters = Vec::with_capacity(key.0.len());
        for disk_number in &key.0 {
            let active = &self
                .underlying_disks
                .get(disk_number)
                .expect("enqueue 已注册每个底层物理盘")
                .active;
            let total = active.total.clone();
            let disk_class = active.class(class);
            total.fetch_add(1, Ordering::AcqRel);
            disk_class.fetch_add(1, Ordering::AcqRel);
            disk_counters.push(DiskPermitCounters {
                total,
                class: disk_class,
            });
        }
        (global_class, disk_counters)
    }

    /// 测试专用快照，验证 global 与每盘 class/total Drop 守恒。
    #[cfg(test)]
    fn active_snapshot(&self, disk_numbers: &[u32]) -> ActiveSnapshot {
        ActiveSnapshot {
            global_total: self.global_active.total.load(Ordering::Acquire),
            global_hash: self.global_active.hash.load(Ordering::Acquire),
            global_media: self.global_active.media.load(Ordering::Acquire),
            disks: disk_numbers
                .iter()
                .filter_map(|disk_number| {
                    self.underlying_disks.get(disk_number).map(|disk| {
                        (
                            *disk_number,
                            disk.active.total.load(Ordering::Acquire),
                            disk.active.hash.load(Ordering::Acquire),
                            disk.active.media.load(Ordering::Acquire),
                        )
                    })
                })
                .collect(),
            waiting: disk_numbers
                .iter()
                .map(|disk_number| {
                    let mut total = 0;
                    let mut hash = 0;
                    let mut media = 0;
                    for (key, queue) in &self.queues {
                        if !key.0.contains(disk_number) {
                            continue;
                        }
                        hash += queue.hash_waiting.len();
                        media += queue.media_waiting.len();
                        total += queue.hash_waiting.len() + queue.media_waiting.len();
                    }
                    (*disk_number, total, hash, media)
                })
                .collect(),
        }
    }
}

async fn run_actor(
    mut commands: mpsc::Receiver<Command>,
    config: ActorConfig,
    notify: Arc<Notify>,
) {
    let mut state = ActorState::new(config, notify.clone());
    loop {
        state.grant_waiters();
        tokio::select! {
            command = commands.recv() => match command {
                Some(Command::Acquire(waiter)) => state.enqueue(waiter),
                #[cfg(test)]
                Some(Command::Barrier(reply)) => { let _ = reply.send(()); }
                #[cfg(test)]
                Some(Command::Snapshot { disk_numbers, reply }) => {
                    let _ = reply.send(state.active_snapshot(&disk_numbers));
                }
                #[cfg(test)]
                Some(Command::DropNextReply(reply)) => {
                    state.drop_next_reply = true;
                    let _ = reply.send(());
                }
                Some(Command::Shutdown(reply)) => {
                    let _ = reply.send(());
                    break;
                }
                None => break,
            },
            _ = notify.notified() => {}
        }
    }
}

/// 计算两个正权重的最大公约数，用于把配置值约分为最小整数比例。
fn greatest_common_divisor(mut left: usize, mut right: usize) -> usize {
    while right != 0 {
        let remainder = left % right;
        left = right;
        right = remainder;
    }
    left.max(1)
}

fn validate_config(
    config: &DiskReadConfig,
    effective_worker_count: usize,
) -> Result<(), SchedulerError> {
    if config.hdd_threads_per_disk == 0
        || config.ssd_threads_per_disk == 0
        || config.unknown_threads_per_disk == 0
        || config.total_threads == 0
    {
        return Err(SchedulerError::InvalidConfiguration(
            "磁盘和全局读取许可必须大于零",
        ));
    }
    if effective_worker_count == 0 {
        return Err(SchedulerError::InvalidConfiguration(
            "实际 Worker 数必须大于零",
        ));
    }
    nominal_seats(config.total_threads, effective_worker_count)?;
    nominal_seats(config.hdd_threads_per_disk, effective_worker_count)?;
    nominal_seats(config.ssd_threads_per_disk, effective_worker_count)?;
    nominal_seats(config.unknown_threads_per_disk, effective_worker_count)?;
    Ok(())
}

/// 按计划公式计算名义 seat；T=1 返回无分母的特殊值。
fn nominal_seats(limit: usize, worker_count: usize) -> Result<NominalSeats, SchedulerError> {
    if limit == 1 {
        return Ok(NominalSeats {
            media: None,
            hash: None,
        });
    }
    let three_quarters = limit
        .checked_mul(3)
        .ok_or(SchedulerError::InvalidConfiguration("名义 seat 计算溢出"))?
        / 4;
    let media = worker_count.min(limit - 1).min(three_quarters);
    Ok(NominalSeats {
        media: Some(media),
        hash: Some(limit - media),
    })
}
