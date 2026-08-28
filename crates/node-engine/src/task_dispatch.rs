//! 按瞬态任务文件队首申请唯一磁盘读取许可。
//!
//! Dispatcher 只负责把每个物理盘 lane 的一个队首交给统一的读取许可提供者；
//! 亏欠、活动计数、类别公平和老化保护全部留在 `DiskReadScheduler` 中。

use std::{
    collections::BTreeMap,
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
    /// 任务文件中的计算记录。
    pub record: TaskFileRecord,
    /// 本次读取所属的 Hash 或媒体阶段。
    pub class: DiskReadClass,
    /// 必须持有到源文件读取结束的唯一许可。
    pub permit: Permit,
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
    future: TaskLanePermitFuture<Permit>,
}

/// 瞬态任务文件的唯一 dispatcher。
///
/// `TransientTaskFileSet` 只由本对象持有，异步许可 future 只保存拥有型队首快照。
/// 因此文件句柄和预读窗口不会跨 `await` 泄漏，且每个 lane 同时最多存在一个请求。
pub struct TaskFileDispatcher<Provider: TaskLanePermitProvider> {
    files: TransientTaskFileSet,
    provider: Provider,
    pending: BTreeMap<String, PendingPermit<Provider::Permit>>,
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
        self.files.mark_completed(identity)
    }

    /// 在单文件读取或 Worker 失败后把已领取行标记为失败。
    pub fn mark_failed(&mut self, identity: &TaskFileIdentity) -> io::Result<()> {
        self.files.mark_failed(identity)
    }

    /// 删除本次运行创建的任务文件目录。
    pub fn discard(&mut self) -> io::Result<()> {
        if !self.pending.is_empty() {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "仍有读取许可请求在途，不能 discard 任务文件",
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

    /// 异步等待并返回下一个已取得许可的任务。
    pub async fn next(
        &mut self,
        cancellation: ReadCancellationToken,
    ) -> Result<Option<DispatchedTask<Provider::Permit>>, TaskDispatchError> {
        poll_fn(|context| self.poll_next(&cancellation, context)).await
    }

    /// 非阻塞推进一次队首许可申请；生产循环可用它接入自身事件循环。
    pub fn poll_next(
        &mut self,
        cancellation: &ReadCancellationToken,
        context: &mut Context<'_>,
    ) -> Poll<Result<Option<DispatchedTask<Provider::Permit>>, TaskDispatchError>> {
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
            if let Err(error) = self.start_lane_requests(cancellation) {
                return Poll::Ready(Err(error));
            }
            if let Poll::Ready(result) = self.poll_lane_requests(context) {
                return Poll::Ready(result);
            }
            if self.files.all_terminal() {
                return Poll::Ready(Ok(None));
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
    ) -> Result<(), TaskDispatchError> {
        // lane_heads 只返回每条 lane 的一个拥有型队首，不会把文件预读为全量 Vec。
        for (lane, head) in self.files.lane_heads()? {
            let key = head.identity.lane_file_name().to_owned();
            if self.pending.contains_key(&key) {
                continue;
            }
            let class = dispatch_class(&head.record)?;
            let future = self
                .provider
                .acquire(lane.clone(), class, cancellation.clone());
            self.pending.insert(
                key,
                PendingPermit {
                    identity: head.identity,
                    record: head.record,
                    class,
                    future,
                },
            );
        }
        Ok(())
    }

    fn poll_lane_requests(
        &mut self,
        context: &mut Context<'_>,
    ) -> Poll<Result<Option<DispatchedTask<Provider::Permit>>, TaskDispatchError>> {
        let keys = self.pending.keys().cloned().collect::<Vec<_>>();
        for key in keys {
            let outcome = {
                let pending = self
                    .pending
                    .get_mut(&key)
                    .expect("队首请求在本次 poll 中不会被外部移除");
                pending.future.as_mut().poll(context)
            };
            match outcome {
                Poll::Pending => {}
                Poll::Ready(Err(error)) => {
                    // 只移除失败 future；任务行仍是 P，下一次 poll 会重新申请许可。
                    self.pending.remove(&key);
                    return Poll::Ready(Err(error.into()));
                }
                Poll::Ready(Ok(permit)) => {
                    let pending = self
                        .pending
                        .remove(&key)
                        .expect("已轮询的队首请求必须仍存在");
                    let taken = self.files.take_lane(&pending.identity)?;
                    let Some((identity, record)) = taken else {
                        // permit 未能与精确队首绑定时立即释放，禁止把它交给错误任务。
                        return Poll::Ready(Err(io::Error::other(
                            "读取许可成功后任务队首无法按身份领取",
                        )
                        .into()));
                    };
                    if record != pending.record {
                        return Poll::Ready(Err(io::Error::new(
                            io::ErrorKind::InvalidData,
                            "读取许可成功后任务记录发生变化",
                        )
                        .into()));
                    }
                    return Poll::Ready(Ok(Some(DispatchedTask {
                        identity,
                        record,
                        class: pending.class,
                        permit,
                    })));
                }
            }
        }
        if self.pending.is_empty() {
            Poll::Pending
        } else {
            Poll::Pending
        }
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
