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
        counters.notify.notify_one();
    }
}

struct PermitCounters {
    /// 授予时冻结的全局 total/class 计数。
    global_total: Arc<AtomicUsize>,
    global_class: Arc<AtomicUsize>,
    /// 复合位置中每个底层盘的 total/class 计数。
    disk_counters: Vec<DiskPermitCounters>,
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
                class,
                sequence: 0,
                conflicting_bypasses: 0,
                reply,
                _queue_slot: queue_slot,
            }))
            .await
            .map_err(|_| SchedulerError::Closed)?;
        response.await.map_err(|_| SchedulerError::Closed)
    }

    #[doc(hidden)]
    pub async fn acquire_for_test(
        &self,
        disk_numbers: &[u32],
        kind: LocalDiskKind,
        class: DiskReadClass,
    ) -> Result<DiskReadPermit, SchedulerError> {
        self.acquire_key(DiskKey::new(disk_numbers)?, kind, class)
            .await
    }

    #[cfg(test)]
    pub(super) const fn request_capacity_for_test(&self) -> usize {
        self.request_capacity
    }

    #[cfg(test)]
    pub(super) async fn barrier_for_test(&self) -> Result<(), SchedulerError> {
        let (reply, response) = oneshot::channel();
        self.commands
            .send(Command::Barrier(reply))
            .await
            .map_err(|_| SchedulerError::Closed)?;
        response.await.map_err(|_| SchedulerError::Closed)
    }

    #[cfg(test)]
    pub(super) async fn active_snapshot_for_test(
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
    Shutdown(oneshot::Sender<()>),
}

/// 测试专用活动计数快照；生产 API 不暴露调度内部原子计数。
#[cfg(test)]
pub(super) struct ActiveSnapshot {
    pub(super) global_total: usize,
    pub(super) global_hash: usize,
    pub(super) global_media: usize,
    pub(super) disks: Vec<(u32, usize, usize, usize)>,
}

struct Waiter {
    key: DiskKey,
    kind: LocalDiskKind,
    /// 请求进入 actor 的读取类别。
    class: DiskReadClass,
    /// 跨位置、跨类别单调递增的入队顺序。
    sequence: u64,
    /// 被更年轻且共享底层盘的请求成功绕过次数。
    conflicting_bypasses: u8,
    reply: oneshot::Sender<DiskReadPermit>,
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

    /// 清除已取消的两个类别队首，避免占住真实 FIFO 请求。
    fn prune_closed_heads(&mut self) {
        while self
            .hash_waiting
            .front()
            .is_some_and(|waiter| waiter.reply.is_closed())
        {
            self.hash_waiting.pop_front();
        }
        while self
            .media_waiting
            .front()
            .is_some_and(|waiter| waiter.reply.is_closed())
        {
            self.media_waiting.pop_front();
        }
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

/// 一次队首选择；sequence 用于在压力比例相同时保持年龄优先。
struct WaiterSelection {
    /// 被选中的复合磁盘身份。
    key: DiskKey,
    /// 被选中的读取类别。
    class: DiskReadClass,
    /// 选中队首的稳定入队顺序。
    sequence: u64,
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
        }
    }

    /// 按读取类别进入当前位置 FIFO，并注册复合位置的全部底层盘。
    fn enqueue(&mut self, mut waiter: Waiter) {
        waiter.sequence = self.next_sequence;
        self.next_sequence = self.next_sequence.wrapping_add(1);
        let key = waiter.key.clone();
        let observed_limit = self.config.disk_limit(waiter.kind);
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
                    notify: self.notify.clone(),
                }),
            };
            match waiter.reply.send(permit) {
                Ok(()) => {
                    self.note_successful_grant(&key, class, sequence, occupies_last_global_seat)
                }
                Err(returned) => drop(returned),
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
                return Some(self.make_selection(&heads[index]));
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

        // 先保留每个同盘 T=1 争用组的代表，再让跨盘代表参加全局裁决。
        let capacity_candidates = self.select_capacity_one_representatives(&heads, &eligible);
        let index = self.select_pressure_or_rotation(&heads, &capacity_candidates);
        Some(self.make_selection(&heads[index]))
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
