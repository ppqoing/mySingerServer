//! 按物理磁盘 FIFO、盘间 round-robin 和全局上限授予文件读取许可。

use std::{
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
}

impl Drop for DiskReadPermit {
    fn drop(&mut self) {
        let Some(counters) = self.counters.take() else {
            return;
        };
        for active in counters.disk_actives {
            active.fetch_sub(1, Ordering::AcqRel);
        }
        counters.global_active.fetch_sub(1, Ordering::AcqRel);
        counters.notify.notify_one();
    }
}

struct PermitCounters {
    global_active: Arc<AtomicUsize>,
    disk_actives: Vec<Arc<AtomicUsize>>,
    notify: Arc<Notify>,
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
        runtime.spawn(run_actor(receiver, ActorConfig::from(config), notify));
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
    ) -> Result<DiskReadPermit, SchedulerError> {
        self.acquire_key(
            DiskKey::new(location.physical_disk_id().disk_numbers())?,
            location.disk_kind(),
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
                reply,
                _queue_slot: queue_slot,
            }))
            .await
            .map_err(|_| SchedulerError::Closed)?;
        response.await.map_err(|_| SchedulerError::Closed)
    }

    #[cfg(test)]
    pub(super) async fn acquire_for_test(
        &self,
        disk_numbers: &[u32],
        kind: LocalDiskKind,
    ) -> Result<DiskReadPermit, SchedulerError> {
        self.acquire_key(DiskKey::new(disk_numbers)?, kind).await
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
}

enum Command {
    Acquire(Waiter),
    #[cfg(test)]
    Barrier(oneshot::Sender<()>),
    Shutdown(oneshot::Sender<()>),
}

struct Waiter {
    key: DiskKey,
    kind: LocalDiskKind,
    reply: oneshot::Sender<DiskReadPermit>,
    _queue_slot: OwnedSemaphorePermit,
}

#[derive(Default)]
struct LocationQueue {
    waiting: VecDeque<Waiter>,
}

struct UnderlyingDiskState {
    active: Arc<AtomicUsize>,
    limit: usize,
}

#[derive(Clone, Copy)]
struct ActorConfig {
    hdd_limit: usize,
    ssd_limit: usize,
    unknown_limit: usize,
    total_limit: usize,
}

impl From<&DiskReadConfig> for ActorConfig {
    fn from(config: &DiskReadConfig) -> Self {
        Self {
            hdd_limit: config.hdd_threads_per_disk,
            ssd_limit: config.ssd_threads_per_disk,
            unknown_limit: config.unknown_threads_per_disk,
            total_limit: config.total_threads,
        }
    }
}

impl ActorConfig {
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
    global_active: Arc<AtomicUsize>,
    queues: BTreeMap<DiskKey, LocationQueue>,
    underlying_disks: BTreeMap<u32, UnderlyingDiskState>,
    rotation: VecDeque<DiskKey>,
    in_rotation: BTreeSet<DiskKey>,
}

impl ActorState {
    fn new(config: ActorConfig, notify: Arc<Notify>) -> Self {
        Self {
            config,
            notify,
            global_active: Arc::new(AtomicUsize::new(0)),
            queues: BTreeMap::new(),
            underlying_disks: BTreeMap::new(),
            rotation: VecDeque::new(),
            in_rotation: BTreeSet::new(),
        }
    }

    fn enqueue(&mut self, waiter: Waiter) {
        let key = waiter.key.clone();
        let observed_limit = self.config.disk_limit(waiter.kind);
        for disk_number in &key.0 {
            self.underlying_disks
                .entry(*disk_number)
                .and_modify(|disk| disk.limit = disk.limit.min(observed_limit))
                .or_insert_with(|| UnderlyingDiskState {
                    active: Arc::new(AtomicUsize::new(0)),
                    limit: observed_limit,
                });
        }
        self.queues
            .entry(key.clone())
            .or_default()
            .waiting
            .push_back(waiter);
        self.rotate(key);
    }

    fn rotate(&mut self, key: DiskKey) {
        if self.in_rotation.insert(key.clone()) {
            self.rotation.push_back(key);
        }
    }

    fn grant_waiters(&mut self) {
        loop {
            if self.global_active.load(Ordering::Acquire) >= self.config.total_limit
                || self.rotation.is_empty()
            {
                return;
            }
            let round_len = self.rotation.len();
            let mut granted_in_round = false;
            for _ in 0..round_len {
                if self.global_active.load(Ordering::Acquire) >= self.config.total_limit {
                    break;
                }
                let key = self.rotation.pop_front().expect("round_len 已冻结");
                self.in_rotation.remove(&key);
                let has_waiter = {
                    let queue = self
                        .queues
                        .get_mut(&key)
                        .expect("rotation 只保存已知位置队列");
                    while queue
                        .waiting
                        .front()
                        .is_some_and(|item| item.reply.is_closed())
                    {
                        queue.waiting.pop_front();
                    }
                    !queue.waiting.is_empty()
                };
                let can_grant = has_waiter && self.can_reserve_all(&key);
                let (waiter, still_waiting) = {
                    let queue = self
                        .queues
                        .get_mut(&key)
                        .expect("rotation 只保存已知位置队列");
                    let waiter =
                        can_grant.then(|| queue.waiting.pop_front().expect("front 刚刚存在"));
                    (waiter, !queue.waiting.is_empty())
                };
                if still_waiting {
                    self.rotate(key.clone());
                }
                let Some(waiter) = waiter else {
                    continue;
                };
                let disk_actives = self.reserve_all(&key);
                self.global_active.fetch_add(1, Ordering::AcqRel);
                let permit = DiskReadPermit {
                    counters: Some(PermitCounters {
                        global_active: self.global_active.clone(),
                        disk_actives,
                        notify: self.notify.clone(),
                    }),
                };
                let _ = waiter.reply.send(permit);
                granted_in_round = true;
            }
            if !granted_in_round {
                return;
            }
        }
    }

    fn can_reserve_all(&self, key: &DiskKey) -> bool {
        key.0.iter().all(|disk_number| {
            let disk = self
                .underlying_disks
                .get(disk_number)
                .expect("enqueue 已注册每个底层物理盘");
            disk.active.load(Ordering::Acquire) < disk.limit
        })
    }

    fn reserve_all(&self, key: &DiskKey) -> Vec<Arc<AtomicUsize>> {
        debug_assert!(self.can_reserve_all(key));
        key.0
            .iter()
            .map(|disk_number| {
                let active = self
                    .underlying_disks
                    .get(disk_number)
                    .expect("enqueue 已注册每个底层物理盘")
                    .active
                    .clone();
                active.fetch_add(1, Ordering::AcqRel);
                active
            })
            .collect()
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
    Ok(())
}
