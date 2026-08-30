//! 按瞬态任务文件队首申请磁盘读取许可。
//!
//! Dispatcher 只负责在逐盘配置窗口内保存精确任务身份，并把每个物理盘 lane 的
//! 当前队首交给统一读取许可提供者；亏欠、活动计数、类别公平和老化保护全部留在
//! `DiskReadScheduler` 中。

use std::{
    collections::{BTreeMap, BTreeSet},
    future::{Future, poll_fn},
    io,
    pin::Pin,
    task::{Context, Poll},
};

use dedup_windows::{ReadCancellationToken, StorageLocation};
use thiserror::Error;

use crate::{
    io::{DiskReadClass, DiskReadLane, DiskReadPermit, DiskReadScheduler, ReadFailure},
    scan::TaskDiskLane,
    task_files::{TaskFileIdentity, TaskFilePublication, TaskFileRecord, TransientTaskFileSet},
};

/// 任务 lane 许可提供者返回的异步许可请求。
pub type TaskLanePermitFuture<Permit> =
    Pin<Box<dyn Future<Output = Result<Permit, ReadFailure>> + Send>>;

/// 把任务文件队首映射为 Hash 或媒体读取入口。
pub trait TaskLanePermitProvider: Clone + Send + Sync + 'static {
    /// 一次读取许可的所有权类型；释放时归还全局及物理盘额度。
    type Permit: Send + 'static;

    /// 为指定物理盘 lane 和计算类别创建一个许可请求。
    fn acquire(
        &self,
        lane: TaskDiskLane,
        class: DiskReadClass,
        cancellation: ReadCancellationToken,
    ) -> TaskLanePermitFuture<Self::Permit>;
}

/// 任务文件队首或读取许可阶段发生的错误。
#[derive(Debug, Error)]
pub enum TaskDispatchError {
    /// 任务文件状态、身份或读取边界无效。
    #[error("任务文件分发失败: {0}")]
    File(#[source] io::Error),
    /// 磁盘读取许可申请失败。
    #[error("读取许可申请失败: {0}")]
    Read(#[source] ReadFailure),
}

/// 当前 dispatcher 允许启动的读取阶段。
///
/// 该值只做阶段 admission，不保存也不复制 `DiskReadScheduler` 的公平状态；
/// 真正的全局、物理盘和类别额度仍由 scheduler 的 permit 决定。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct TaskDispatchAdmission {
    /// 是否允许为需要 MD5 的任务申请 Hash 读取许可。
    pub allow_hash: bool,
    /// 是否允许为媒体字段或二筛任务申请 Media 读取许可。
    pub allow_media: bool,
}

impl TaskDispatchAdmission {
    /// 同时允许 Hash 与 Media，保持旧 `next/poll_next` 行为。
    pub const fn all() -> Self {
        Self {
            allow_hash: true,
            allow_media: true,
        }
    }

    /// 只允许 Hash，供基础计算的 Hash 批处理使用。
    pub const fn hash_only() -> Self {
        Self {
            allow_hash: true,
            allow_media: false,
        }
    }

    /// 只允许 Media，供基础计算的媒体续算或二筛使用。
    pub const fn media_only() -> Self {
        Self {
            allow_hash: false,
            allow_media: true,
        }
    }

    /// 判断指定读取类别是否通过本轮 admission。
    const fn allows(self, class: DiskReadClass) -> bool {
        match class {
            DiskReadClass::HashSequential => self.allow_hash,
            DiskReadClass::MediaDecode => self.allow_media,
        }
    }
}

impl Default for TaskDispatchAdmission {
    fn default() -> Self {
        Self::all()
    }
}

/// 任务文件已经封闭但当前 admission 无法继续时的阻塞原因。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum TaskDispatchBlockReason {
    /// 仍有需要 Hash 的 `P` 行，但本轮不允许 Hash。
    HashPending,
    /// 仍有 Media `P` 行或 Hash→Media 续算，但本轮不允许 Media。
    MediaPending,
}

/// 一次 admission 轮询的明确结果。
#[derive(Debug)]
pub enum TaskDispatchPoll<Permit> {
    /// 已取得 permit 并精确领取一项任务。
    Task(DispatchedTask<Permit>),
    /// 任务文件已封闭且所有行都进入终态。
    Drained,
    /// 仍有任务，但它们都被当前 admission 阻止；任务行保持 `P`。
    Blocked(TaskDispatchBlockReason),
}

impl From<io::Error> for TaskDispatchError {
    fn from(error: io::Error) -> Self {
        Self::File(error)
    }
}

impl From<ReadFailure> for TaskDispatchError {
    fn from(error: ReadFailure) -> Self {
        Self::Read(error)
    }
}

/// 已取得唯一读取许可、并已经从任务文件领取的任务。
pub struct DispatchedTask<Permit> {
    /// 任务文件返回的完整行身份；结果提交必须原样回传。
    pub identity: TaskFileIdentity,
    /// 任务文件记录，或 Hash 后仅在内存中派生的 Media 记录。
    pub record: TaskFileRecord,
    /// 本次读取所属的 Hash 或媒体阶段。
    pub class: DiskReadClass,
    /// 必须持有到源文件读取结束的唯一许可。
    pub permit: Permit,
    /// 是否为同一基础任务 Hash 完成后的 Media 续算许可。
    pub continuation: bool,
}

impl<Permit> DispatchedTask<Permit> {
    /// 返回本次任务是否沿用同一 TSV 行进入 Media 阶段。
    pub const fn is_continuation(&self) -> bool {
        self.continuation
    }
}

impl<Permit> std::fmt::Debug for DispatchedTask<Permit> {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("DispatchedTask")
            .field("identity", &self.identity)
            .field("record", &self.record)
            .field("class", &self.class)
            .finish_non_exhaustive()
    }
}

struct PendingPermit<Permit> {
    identity: TaskFileIdentity,
    record: TaskFileRecord,
    class: DiskReadClass,
    /// 普通任务身份使用的冻结逐盘窗口上限。
    per_disk_limit: usize,
    /// 标记该等待项是否是同一 TSV 行的 Media 续算。
    continuation: bool,
    future: TaskLanePermitFuture<Permit>,
}

/// 瞬态任务文件的唯一 dispatcher。
///
/// `TransientTaskFileSet` 只由本对象持有，异步许可 future 只保存拥有型队首快照。
/// 因此文件句柄和预读窗口不会跨 `await` 泄漏；每个 lane 至多等待一个普通队首，
/// 已交付身份数量由本轮冻结的逐盘配置窗口限制。
pub struct TaskFileDispatcher<Provider: TaskLanePermitProvider> {
    files: TransientTaskFileSet,
    provider: Provider,
    /// 按完整身份保存的许可等待项，便于失败和取消时精确收束。
    pending: BTreeMap<TaskFileIdentity, PendingPermit<Provider::Permit>>,
    /// 已登记但尚未取得 Media 许可的同身份续算意图；失败后保留以便重试。
    continuations: BTreeMap<TaskFileIdentity, TaskFileRecord>,
    /// 每个 lane 已交付但尚未 SQLite ACK 的精确身份集合。
    in_flight_by_lane: BTreeMap<String, BTreeSet<TaskFileIdentity>>,
    /// 已经消费过 Hash→Media 入口的身份，防止重复续算。
    continuation_claimed: BTreeSet<TaskFileIdentity>,
    observed_epoch: u64,
    publication_wait: Option<Pin<Box<dyn Future<Output = u64> + Send>>>,
}

impl<Provider: TaskLanePermitProvider> TaskFileDispatcher<Provider> {
    /// 创建拥有任务文件集合和许可提供者的 dispatcher。
    pub fn new(files: TransientTaskFileSet, provider: Provider) -> Self {
        let observed_epoch = files.publication_epoch();
        Self {
            files,
            provider,
            pending: BTreeMap::new(),
            continuations: BTreeMap::new(),
            in_flight_by_lane: BTreeMap::new(),
            continuation_claimed: BTreeSet::new(),
            observed_epoch,
            publication_wait: None,
        }
    }

    /// 注册一个按物理盘身份冻结的任务 lane。
    pub fn register_lane(&mut self, lane: &TaskDiskLane) -> io::Result<()> {
        self.files.register_lane(lane)
    }

    /// 追加一批任务行，并在 flush 后发布给 dispatcher。
    pub fn append_batch(
        &mut self,
        lane: &TaskDiskLane,
        rows: &[TaskFileRecord],
    ) -> io::Result<Vec<TaskFileIdentity>> {
        self.files.append_batch(lane, rows)
    }

    /// 封闭生产端；封闭后只继续派发已发布的任务。
    pub fn seal(&mut self) -> io::Result<()> {
        self.files.seal()
    }

    /// 在 SQLite 成功确认后把已领取行标记为完成。
    pub fn mark_completed(&mut self, identity: &TaskFileIdentity) -> io::Result<()> {
        self.ensure_identity_not_waiting(identity)?;
        self.files.mark_completed(identity)?;
        self.continuations.remove(identity);
        self.continuation_claimed.remove(identity);
        self.release_in_flight_identity(identity);
        Ok(())
    }

    /// 在单文件读取或 Worker 失败后把已领取行标记为失败。
    pub fn mark_failed(&mut self, identity: &TaskFileIdentity) -> io::Result<()> {
        self.ensure_identity_not_waiting(identity)?;
        self.files.mark_failed(identity)?;
        self.continuations.remove(identity);
        self.continuation_claimed.remove(identity);
        self.release_in_flight_identity(identity);
        Ok(())
    }

    /// 登记同一基础任务的 Hash→Media 续算；不改写 TSV，也不追加第二行。
    ///
    /// 入口接受仍在途、仍为 `P` 的原始 Base 行及其 Hash 后的 Media 派生记录。
    /// 普通队首若正在等待许可会被撤下，续算意图优先；被撤下的普通队首仍保持
    /// `P`，后续会重新观察。
    pub fn request_media_continuation(
        &mut self,
        identity: &TaskFileIdentity,
        record: &TaskFileRecord,
    ) -> Result<(), TaskDispatchError> {
        if self.continuation_claimed.contains(identity) || self.continuations.contains_key(identity)
        {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "同一任务身份不能重复请求 Media 续算",
            )
            .into());
        }
        self.files
            .validate_media_continuation(identity, record)
            .map_err(TaskDispatchError::File)?;
        let lane_key = identity.lane_file_name().to_owned();
        let ordinary_pending = self
            .pending
            .iter()
            .find(|(_, pending)| {
                !pending.continuation && pending.identity.lane_file_name() == lane_key
            })
            .map(|(pending_identity, _)| pending_identity.clone());
        if let Some(pending_identity) = ordinary_pending {
            // 取消正在等待的普通队首许可，让同 lane 续算先行；队首仍是 P。
            self.pending.remove(&pending_identity);
        }
        self.continuation_claimed.insert(identity.clone());
        self.continuations.insert(identity.clone(), record.clone());
        Ok(())
    }

    /// 检查身份是否仍有等待中的续算或许可，避免提前写入终态。
    fn ensure_identity_not_waiting(&self, identity: &TaskFileIdentity) -> io::Result<()> {
        if self.pending.contains_key(identity) || self.continuations.contains_key(identity) {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "任务仍有等待中的读取或续算许可",
            ));
        }
        Ok(())
    }

    /// 任务级取消时丢弃全部 dispatcher 自有的许可等待 future。
    ///
    /// 本方法不改写 TSV、续算意图或在途身份；调用方须在外部读取 owner 收束后调用，
    /// 再按 `in_flight_identities` 快照逐项 abandon。
    pub(crate) fn cancel_pending_permit_requests(&mut self) {
        self.pending.clear();
    }

    /// 在取消收束后放弃一项精确在途任务，不写入 `F`。
    ///
    /// 调用方应先调用 `cancel_pending_permit_requests` 收束等待 future；本方法随后
    /// 一并清理同身份的续算意图，再释放在途身份。
    pub fn abandon_in_flight(&mut self, identity: &TaskFileIdentity) -> io::Result<()> {
        if self.pending.contains_key(identity) {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "同一任务身份仍有读取许可请求在途，不能 abandon",
            ));
        }
        self.files.abandon_in_flight(identity)?;
        // 取消轮询已经丢弃等待 future；此处同时收掉同身份的续算意图，
        // 使调用方可以在保持 P 的前提下精确结束整个任务 run。
        self.continuations.remove(identity);
        self.continuation_claimed.remove(identity);
        self.release_in_flight_identity(identity);
        Ok(())
    }

    /// 返回 dispatcher 当前未 ACK 的精确在途身份快照，供任务级 cleanup 逐项收束。
    pub(crate) fn in_flight_identities(&self) -> Vec<TaskFileIdentity> {
        self.in_flight_by_lane
            .values()
            .flat_map(|identities| identities.iter().cloned())
            .collect()
    }

    /// 删除本次运行创建的任务文件目录。
    pub fn discard(&mut self) -> io::Result<()> {
        if !self.pending.is_empty()
            || !self.continuations.is_empty()
            || !self.in_flight_by_lane.is_empty()
        {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "仍有读取许可或续算请求在途，不能 discard 任务文件",
            ));
        }
        // 内部等待 future 也持有 publication owner；删除前先解除，避免自身阻止清理。
        self.publication_wait = None;
        self.files.discard()
    }

    /// 返回指定 lane 的任务文件路径，供诊断和测试读取状态字节。
    pub fn lane_path(&self, lane: &TaskDiskLane) -> io::Result<std::path::PathBuf> {
        self.files.lane_path(lane)
    }

    /// 返回指定 lane 当前已预读的任务数量。
    pub fn prefetched_len(&mut self, lane: &TaskDiskLane) -> io::Result<usize> {
        self.files.prefetched_len(lane)
    }

    /// 返回任务文件发布通知句柄，供外部生产者观察追加和 seal。
    pub fn publication(&self) -> TaskFilePublication {
        self.files.publication()
    }

    /// 返回任务文件集合是否已通过健康检查。
    pub fn health(&self) -> io::Result<()> {
        self.files.health()
    }

    /// 返回 lane 已占用的不同任务身份数；普通 pending 会预占一个窗口位置。
    fn lane_identity_count(&self, lane_key: &str) -> usize {
        let active = self
            .in_flight_by_lane
            .get(lane_key)
            .map_or(0, BTreeSet::len);
        let ordinary_pending = self
            .pending
            .values()
            .filter(|pending| {
                !pending.continuation
                    && pending.identity.lane_file_name() == lane_key
                    && !self
                        .in_flight_by_lane
                        .get(lane_key)
                        .is_some_and(|identities| identities.contains(&pending.identity))
            })
            .count();
        active.saturating_add(ordinary_pending)
    }

    /// 判断 lane 是否已经有一个普通队首许可请求。
    fn has_pending_ordinary(&self, lane_key: &str) -> bool {
        self.pending
            .values()
            .any(|pending| !pending.continuation && pending.identity.lane_file_name() == lane_key)
    }

    /// 从 lane 集合释放一个精确身份，并清理已经为空的 lane 键。
    fn release_in_flight_identity(&mut self, identity: &TaskFileIdentity) {
        let lane_key = identity.lane_file_name().to_owned();
        let remove_lane = self
            .in_flight_by_lane
            .get_mut(&lane_key)
            .is_some_and(|identities| {
                identities.remove(identity);
                identities.is_empty()
            });
        if remove_lane {
            self.in_flight_by_lane.remove(&lane_key);
        }
    }

    /// 判断当前 admission 下是否存在可交付任务，供事件泵避开已知阻塞的轮询。
    pub(crate) fn has_admitted_work(
        &mut self,
        admission: TaskDispatchAdmission,
    ) -> Result<bool, TaskDispatchError> {
        if self
            .pending
            .values()
            .any(|pending| admission.allows(pending.class))
        {
            return Ok(true);
        }
        if admission.allow_media && !self.continuations.is_empty() {
            return Ok(true);
        }
        for (lane, head) in self.files.lane_heads()? {
            let key = self
                .files
                .lane_key(&lane)
                .map_err(TaskDispatchError::File)?;
            if self.lane_identity_count(&key) >= lane.per_disk_limit {
                continue;
            }
            if admission.allows(dispatch_class(&head.record)?) {
                return Ok(true);
            }
        }
        Ok(false)
    }

    /// 异步等待并返回下一个已取得许可的任务。
    pub async fn next(
        &mut self,
        cancellation: ReadCancellationToken,
    ) -> Result<Option<DispatchedTask<Provider::Permit>>, TaskDispatchError> {
        poll_fn(|context| self.poll_next(&cancellation, context)).await
    }

    /// 按指定阶段 admission 异步等待下一项；被禁止的剩余任务会明确返回 `Blocked`。
    pub async fn next_with_admission(
        &mut self,
        cancellation: ReadCancellationToken,
        admission: TaskDispatchAdmission,
    ) -> Result<TaskDispatchPoll<Provider::Permit>, TaskDispatchError> {
        poll_fn(|context| self.poll_next_with_admission(&cancellation, admission, context)).await
    }

    /// 非阻塞推进一次队首许可申请；生产循环可用它接入自身事件循环。
    pub fn poll_next(
        &mut self,
        cancellation: &ReadCancellationToken,
        context: &mut Context<'_>,
    ) -> Poll<Result<Option<DispatchedTask<Provider::Permit>>, TaskDispatchError>> {
        match self.poll_next_with_admission(cancellation, TaskDispatchAdmission::all(), context) {
            Poll::Ready(Ok(TaskDispatchPoll::Task(task))) => Poll::Ready(Ok(Some(task))),
            Poll::Ready(Ok(TaskDispatchPoll::Drained)) => Poll::Ready(Ok(None)),
            // 默认 admission 同时允许两类，只有调用者在所有任务仍在途时才可能
            // 观察到该分支；保留旧 API 的 Pending 语义。
            Poll::Ready(Ok(TaskDispatchPoll::Blocked(_))) => Poll::Pending,
            Poll::Ready(Err(error)) => Poll::Ready(Err(error)),
            Poll::Pending => Poll::Pending,
        }
    }

    /// 非阻塞推进指定阶段 admission；不会复制 scheduler 的公平状态。
    pub fn poll_next_with_admission(
        &mut self,
        cancellation: &ReadCancellationToken,
        admission: TaskDispatchAdmission,
        context: &mut Context<'_>,
    ) -> Poll<Result<TaskDispatchPoll<Provider::Permit>, TaskDispatchError>> {
        loop {
            if cancellation.is_cancelled() {
                // 取消只丢弃等待 future，磁盘文件仍保持 P，调用方可以在收束后整体删除 run。
                self.pending.clear();
                self.publication_wait = None;
                return Poll::Ready(Err(ReadFailure::Cancelled.into()));
            }

            if let Err(error) = self.files.health() {
                return Poll::Ready(Err(error.into()));
            }
            if let Err(error) = self.start_lane_requests(cancellation, admission) {
                return Poll::Ready(Err(error));
            }
            if let Poll::Ready(result) = self.poll_lane_requests(context) {
                return Poll::Ready(result.map(|task| match task {
                    Some(task) => TaskDispatchPoll::Task(task),
                    None => TaskDispatchPoll::Drained,
                }));
            }
            if self.files.all_terminal() {
                return Poll::Ready(Ok(TaskDispatchPoll::Drained));
            }
            if self.files.production_sealed() {
                if let Some(reason) = self.blocked_by_admission(admission)? {
                    // 阶段切换后不能复用旧等待通知；下一轮 admission 应立即重新观察队首。
                    self.publication_wait = None;
                    return Poll::Ready(Ok(TaskDispatchPoll::Blocked(reason)));
                }
            }

            let current_epoch = self.files.publication_epoch();
            if current_epoch != self.observed_epoch {
                self.observed_epoch = current_epoch;
                self.publication_wait = None;
                continue;
            }
            if self.publication_wait.is_none() {
                let publication = self.files.publication();
                let observed = self.observed_epoch;
                self.publication_wait = Some(Box::pin(async move {
                    publication.wait_for_change(observed).await
                }));
            }
            let wait = self
                .publication_wait
                .as_mut()
                .expect("publication future 已建立");
            match wait.as_mut().poll(context) {
                Poll::Pending => return Poll::Pending,
                Poll::Ready(epoch) => {
                    self.observed_epoch = epoch;
                    self.publication_wait = None;
                }
            }
        }
    }

    fn start_lane_requests(
        &mut self,
        cancellation: &ReadCancellationToken,
        admission: TaskDispatchAdmission,
    ) -> Result<(), TaskDispatchError> {
        let forbidden = self
            .pending
            .iter()
            .filter(|(_, pending)| !admission.allows(pending.class))
            .map(|(identity, _)| identity.clone())
            .collect::<Vec<_>>();
        for identity in forbidden {
            // 当前阶段禁止的 future 尚未向调用方交付 permit；丢弃它即可释放其
            // 内部资源，任务文件仍保持 P，切回允许阶段时会按同一身份重试。
            self.pending.remove(&identity);
        }

        let heads = self
            .files
            .lane_heads()?
            .into_iter()
            .map(|(lane, head)| (head.identity.lane_file_name().to_owned(), (lane, head)))
            .collect::<BTreeMap<_, _>>();
        // 每个 lane 至多保留一个等待 scheduler 的 future；续算优先于普通队首。
        for lane in self.files.lanes() {
            let key = self
                .files
                .lane_key(&lane)
                .map_err(TaskDispatchError::File)?;
            if self
                .pending
                .values()
                .any(|pending| pending.identity.lane_file_name() == key)
            {
                continue;
            }
            let (identity, record, class, continuation) = if let Some((identity, record)) = self
                .continuations
                .iter()
                .find(|(identity, _)| {
                    admission.allow_media
                        && identity.lane_file_name() == key
                        && !self.pending.contains_key(*identity)
                })
                .map(|(identity, record)| (identity.clone(), record.clone()))
            {
                (identity, record, DiskReadClass::MediaDecode, true)
            } else {
                if self.has_pending_ordinary(&key)
                    || self.lane_identity_count(&key) >= lane.per_disk_limit
                {
                    continue;
                }
                let Some((_, head)) = heads.get(&key) else {
                    continue;
                };
                let class = dispatch_class(&head.record)?;
                (head.identity.clone(), head.record.clone(), class, false)
            };
            if !admission.allows(class) {
                continue;
            }
            let future = self
                .provider
                .acquire(lane.clone(), class, cancellation.clone());
            self.pending.insert(
                identity.clone(),
                PendingPermit {
                    identity,
                    record,
                    class,
                    per_disk_limit: lane.per_disk_limit,
                    continuation,
                    future,
                },
            );
        }
        Ok(())
    }

    /// 找出封闭任务中被当前 admission 阻止的队首或续算类别。
    fn blocked_by_admission(
        &mut self,
        admission: TaskDispatchAdmission,
    ) -> Result<Option<TaskDispatchBlockReason>, TaskDispatchError> {
        // 允许类别的 future 仍在等待 scheduler 时必须继续等待；否则禁止类别的队首
        // 会把本应可继续的读取误报成 Blocked。已完成的 future 会在前面的 poll 中交付。
        if self
            .pending
            .values()
            .any(|pending| admission.allows(pending.class))
        {
            return Ok(None);
        }

        let mut hash_pending = false;
        let mut media_pending = false;

        if !admission.allow_media && !self.continuations.is_empty() {
            media_pending = true;
        }
        for pending in self.pending.values() {
            match pending.class {
                DiskReadClass::HashSequential if !admission.allow_hash => hash_pending = true,
                DiskReadClass::MediaDecode if !admission.allow_media => media_pending = true,
                _ => {}
            }
        }
        for (_, head) in self.files.lane_heads()? {
            match dispatch_class(&head.record)? {
                DiskReadClass::HashSequential if !admission.allow_hash => hash_pending = true,
                DiskReadClass::MediaDecode if !admission.allow_media => media_pending = true,
                _ => {}
            }
        }

        if hash_pending {
            Ok(Some(TaskDispatchBlockReason::HashPending))
        } else if media_pending {
            Ok(Some(TaskDispatchBlockReason::MediaPending))
        } else {
            Ok(None)
        }
    }

    fn poll_lane_requests(
        &mut self,
        context: &mut Context<'_>,
    ) -> Poll<Result<Option<DispatchedTask<Provider::Permit>>, TaskDispatchError>> {
        let identities = self.pending.keys().cloned().collect::<Vec<_>>();
        for pending_identity in identities {
            let outcome = {
                let pending = self
                    .pending
                    .get_mut(&pending_identity)
                    .expect("队首请求在本次 poll 中不会被外部移除");
                pending.future.as_mut().poll(context)
            };
            match outcome {
                Poll::Pending => {}
                Poll::Ready(Err(error)) => {
                    // 只移除失败 future；任务行仍是 P，下一次 poll 会重新申请许可。
                    self.pending.remove(&pending_identity);
                    return Poll::Ready(Err(error.into()));
                }
                Poll::Ready(Ok(permit)) => {
                    let pending = self
                        .pending
                        .remove(&pending_identity)
                        .expect("已轮询的队首请求必须仍存在");
                    let lane_key = pending.identity.lane_file_name().to_owned();
                    let (identity, record) = if pending.continuation {
                        self.files
                            .validate_media_continuation(&pending.identity, &pending.record)?;
                        if !self
                            .in_flight_by_lane
                            .get(&lane_key)
                            .is_some_and(|identities| identities.contains(&pending.identity))
                        {
                            return Poll::Ready(Err(io::Error::other(
                                "Media 续算身份不在对应 lane 的在途集合中",
                            )
                            .into()));
                        }
                        self.continuations.remove(&pending.identity);
                        (pending.identity, pending.record)
                    } else {
                        if self
                            .in_flight_by_lane
                            .get(&lane_key)
                            .is_some_and(|identities| identities.len() >= pending.per_disk_limit)
                        {
                            return Poll::Ready(Err(io::Error::other(
                                "普通任务取得许可后超过冻结的逐盘身份窗口",
                            )
                            .into()));
                        }
                        let taken = self
                            .files
                            .take_lane_exact(&pending.identity, &pending.record)?;
                        let Some((identity, record)) = taken else {
                            // permit 未能与精确队首绑定时立即释放，禁止把它交给错误任务。
                            return Poll::Ready(Err(io::Error::other(
                                "读取许可成功后任务队首无法按身份领取",
                            )
                            .into()));
                        };
                        self.in_flight_by_lane
                            .entry(lane_key.clone())
                            .or_default()
                            .insert(identity.clone());
                        (identity, record)
                    };
                    return Poll::Ready(Ok(Some(DispatchedTask {
                        identity,
                        record,
                        class: pending.class,
                        permit,
                        continuation: pending.continuation,
                    })));
                }
            }
        }
        Poll::Pending
    }
}

/// 根据任务记录选择本次唯一的读取类别，并拒绝非法掩码组合。
fn dispatch_class(record: &TaskFileRecord) -> Result<DiskReadClass, TaskDispatchError> {
    record
        .missing
        .validate_for(record.work_kind, record.known_md5)
        .map_err(TaskDispatchError::File)?;
    Ok(match record.work_kind {
        crate::task_files::TaskWorkKind::Base if record.missing.needs_md5() => {
            DiskReadClass::HashSequential
        }
        crate::task_files::TaskWorkKind::Base => DiskReadClass::MediaDecode,
        crate::task_files::TaskWorkKind::ImageStage2
        | crate::task_files::TaskWorkKind::VideoStage2 => DiskReadClass::MediaDecode,
    })
}

/// 将冻结的任务 lane 转换为唯一 `DiskReadScheduler` 的许可提供者。
#[derive(Clone)]
pub struct SchedulerTaskLanePermitProvider {
    scheduler: DiskReadScheduler,
}

impl SchedulerTaskLanePermitProvider {
    /// 使用现有 scheduler 创建薄适配层；不会复制任何调度状态。
    pub fn new(scheduler: DiskReadScheduler) -> Self {
        Self { scheduler }
    }
}

impl TaskLanePermitProvider for SchedulerTaskLanePermitProvider {
    type Permit = DiskReadPermit;

    fn acquire(
        &self,
        lane: TaskDiskLane,
        class: DiskReadClass,
        cancellation: ReadCancellationToken,
    ) -> TaskLanePermitFuture<Self::Permit> {
        let scheduler = self.scheduler.clone();
        let disk_lane = DiskReadLane {
            location: StorageLocation::from_parts(lane.physical_disk_id.clone(), lane.disk_kind),
            effective_limit: lane.per_disk_limit,
            configured_weight: lane.configured_weight,
        };
        let path = format!(
            "PhysicalDisk{}",
            lane.physical_disk_numbers
                .iter()
                .map(u32::to_string)
                .collect::<Vec<_>>()
                .join("+")
        );
        Box::pin(async move {
            if cancellation.is_cancelled() {
                return Err(ReadFailure::Cancelled);
            }
            let permit = scheduler
                .acquire_lane(disk_lane, class)
                .await
                .map_err(|error| ReadFailure::Io {
                    path: path.into(),
                    block_offset: 0,
                    source: io::Error::other(error),
                })?;
            if cancellation.is_cancelled() {
                drop(permit);
                return Err(ReadFailure::Cancelled);
            }
            Ok(permit)
        })
    }
}

/// 便于按显式名称构造 scheduler provider 的别名。
pub type DiskSchedulerPermitProvider = SchedulerTaskLanePermitProvider;
