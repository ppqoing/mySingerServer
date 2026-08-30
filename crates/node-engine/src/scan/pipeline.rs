//! 有界枚举生产器与按物理盘调度的生产 MD5 读取适配器。

use std::{
    collections::HashMap,
    future::Future,
    io,
    path::{Path, PathBuf},
    pin::Pin,
    sync::Arc,
    time::Instant,
};

use dedup_core::{DiskReadConfig, NodeConfig, NormalizedPath};
use dedup_node_store::ScannedPath;
use dedup_windows::{LocalDiskKind, PhysicalDiskId, ReadCancellationToken, StorageLocation};
use tokio::{sync::mpsc, task::JoinHandle};

use crate::{
    io::{
        BlockReader, DiskReadClass, DiskReadLane, DiskReadLaneGroup, DiskReadPermit,
        DiskReadScheduler, ReadFailure, RetryingFileReader,
    },
    runtime_tasks::{
        RuntimeDiskReadClass, RuntimePipelineResource, RuntimeTaskError, RuntimeTaskReporter,
    },
    task_dispatch::{TaskLanePermitFuture, TaskLanePermitProvider},
};

use super::base_flow_control::HashReadStartedSignal;
use super::{FileEnumerator, PlannedScannedPath, ScanError, TaskDiskLane};

/// 扫描阶段内存队列的产品级硬上限，防止配置乘法放大所有权窗口。
const MAX_PIPELINE_CHANNEL_CAPACITY: usize = 4_096;

/// 枚举源生命周期事件；完成事件与逐项交付共用顺序通道。
pub(super) enum EnumerationEvent {
    /// 完整清单可提前携带权威总数；流式枚举在尾部发送 `None`。
    Completed(Option<(u64, u64)>),
    /// 进入有界读取流水线的单个文件。
    Row(ScannedPath),
}

/// 扫描五段管道共享的有界容量与最大并行读任务数。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct PipelineLimits {
    channel_capacity: usize,
    max_read_tasks: usize,
}

impl PipelineLimits {
    /// 创建测试或已验证配置使用的明确上限。
    pub const fn new(channel_capacity: usize, max_read_tasks: usize) -> Self {
        assert!(channel_capacity > 0, "扫描管道容量必须大于 0");
        assert!(max_read_tasks > 0, "最大读取任务数必须大于 0");
        assert!(
            channel_capacity <= MAX_PIPELINE_CHANNEL_CAPACITY,
            "扫描管道容量超过产品硬上限"
        );
        assert!(
            max_read_tasks <= MAX_PIPELINE_CHANNEL_CAPACITY,
            "最大读取任务数超过产品硬上限"
        );
        Self {
            channel_capacity,
            max_read_tasks,
        }
    }

    pub(super) const fn channel_capacity(self) -> usize {
        self.channel_capacity
    }

    pub(crate) const fn max_read_tasks(self) -> usize {
        self.max_read_tasks
    }
}

/// MD5 与限制本次完整文件读取并发数的磁盘许可。
pub struct ReadProduct<L> {
    /// 完整文件的 16 字节 MD5。
    pub md5: [u8; 16],
    /// 读取完成后交给单写者，并在派发 Worker 前释放的许可。
    pub lease: L,
}

/// 可并行派发的文件读取边界。
pub trait PipelineFileReader: Clone + Send + Sync + 'static {
    /// 随 MD5 结果返回、用于限制读取并发数的许可。
    type Lease: Send + 'static;

    /// 读取一个缓存未命中文件；future 必须可在线程池任务中运行。
    fn read(
        &self,
        scanned: ScannedPath,
        cancellation: ReadCancellationToken,
    ) -> Pin<Box<dyn Future<Output = Result<ReadProduct<Self::Lease>, ReadFailure>> + Send + 'static>>;

    /// 测试读取器在 future 开始时进入 reading；生产读取器覆盖为真实许可取得后进入。
    fn read_with_phase(
        &self,
        scanned: ScannedPath,
        cancellation: ReadCancellationToken,
        started: HashReadStartedSignal,
    ) -> Pin<Box<dyn Future<Output = Result<ReadProduct<Self::Lease>, ReadFailure>> + Send + 'static>>
    {
        let future = self.read(scanned, cancellation);
        Box::pin(async move {
            started.mark_reading();
            future.await
        })
    }

    /// 在 Worker 派发前获取独立媒体读取许可；无调度能力的测试 reader 默认不限制媒体。
    fn acquire_media_permit(
        &self,
        _scanned: ScannedPath,
        _cancellation: ReadCancellationToken,
    ) -> Pin<Box<dyn Future<Output = Result<Option<Self::Lease>, ReadFailure>> + Send + 'static>>
    {
        Box::pin(async { Ok(None) })
    }

    /// 根据冻结计划取得路径的物理盘显示身份，不建立可变路径缓存。
    fn physical_disk_id(&self, _path: &Path) -> String {
        String::new()
    }
}

/// 消费外部已经取得的 Hash 读取许可；读取器不得在该入口再次申请 scheduler 许可。
pub trait HashPermitReader: Clone + Send + Sync + 'static {
    /// 外部 dispatcher 交付的读取许可类型。
    type Permit: Send + 'static;

    /// 持有传入许可完成完整 MD5；返回的 `ReadProduct` 由调用方在确认结果后显式释放。
    fn read_with_permit(
        &self,
        scanned: ScannedPath,
        permit: Self::Permit,
        cancellation: ReadCancellationToken,
        started: Option<HashReadStartedSignal>,
    ) -> Pin<
        Box<dyn Future<Output = Result<ReadProduct<Self::Permit>, ReadFailure>> + Send + 'static>,
    >;
}

/// 生产路径使用的物理盘调度、OVERLAPPED 重试 MD5 组合。
#[derive(Clone)]
pub struct ScheduledFileReader {
    scheduler: DiskReadScheduler,
    /// 仅由 `TaskFileDispatcher` 克隆创建的任务级物理盘配额组。
    lane_group: Option<DiskReadLaneGroup>,
    reader: Arc<dyn Md5ReadBackend>,
    lane_source: LaneSource,
    /// 冻结本 reader 的逐盘配置，用于按本次解析介质类型发布观察容量。
    read_config: DiskReadConfig,
    /// 可选任务内存指标；未配置的旧扫描保持无遥测路径。
    reporter: Option<RuntimeTaskReporter>,
}

/// 从成功解析物理盘身份起持有 waiting，未取得许可即析构时自动取消等待。
struct RuntimeDiskWaitGuard {
    /// 可选任务指标；旧调用链不启用逐盘遥测。
    reporter: Option<RuntimeTaskReporter>,
    /// 本次复合位置排序去重后的全部底层盘 ID。
    disk_ids: Vec<String>,
    /// Hash 或媒体真实读取类别。
    class: RuntimeDiskReadClass,
    /// true 表示 registry 仍持有本次 waiting，Drop 必须撤销。
    waiting: bool,
}

impl RuntimeDiskWaitGuard {
    /// 登记逐盘 waiting；registry 原子拒绝时不创建部分守卫状态。
    fn new(
        reporter: Option<RuntimeTaskReporter>,
        disk_ids: Vec<String>,
        class: RuntimeDiskReadClass,
        capacity: u64,
    ) -> Result<Self, RuntimeTaskError> {
        if let Some(reporter) = &reporter {
            reporter.disk_read_waiting_nowait(&disk_ids, class, capacity)?;
        }
        Ok(Self {
            waiting: reporter.is_some(),
            reporter,
            disk_ids,
            class,
        })
    }

    /// 在真实 scheduler 许可取得边界把 waiting 原子转换为 active/granted。
    fn mark_acquired(&mut self) -> Result<(), RuntimeTaskError> {
        if let Some(reporter) = &self.reporter {
            reporter.disk_read_acquired_nowait(&self.disk_ids, self.class)?;
            self.waiting = false;
        }
        Ok(())
    }
}

impl Drop for RuntimeDiskWaitGuard {
    /// future 取消、scheduler 关闭或转换失败时撤销尚未授予的 waiting。
    fn drop(&mut self) {
        if !self.waiting {
            return;
        }
        if let Some(reporter) = &self.reporter
            && let Err(error) = reporter.disk_read_wait_cancelled_nowait(&self.disk_ids, self.class)
        {
            tracing::error!(
                error = %error,
                disk_ids = ?self.disk_ids,
                class = ?self.class,
                "逐盘读取 waiting Drop 清理失败"
            );
        }
    }
}

/// 真实磁盘许可与任务资源指标共享生命周期的 RAII 包装。
#[doc(hidden)]
pub struct ScheduledReadPermit {
    /// 先显式 Drop 底层许可，再更新内存占用。
    permit: Option<DiskReadPermit>,
    /// 任务结束或旧路径没有 reporter 时保持缺失。
    reporter: Option<RuntimeTaskReporter>,
    /// 区分 Hash IO 与媒体解码 IO。
    resource: RuntimePipelineResource,
    /// 本许可覆盖的排序去重底层物理盘 ID。
    disk_ids: Vec<String>,
    /// 本许可用于逐盘指标的 Hash 或媒体类别。
    class: RuntimeDiskReadClass,
    /// 取得许可后的单调时刻，用于记录真实持有耗时。
    acquired_at: Instant,
}

impl Drop for ScheduledReadPermit {
    /// 归还调度器许可后同步投影资源释放，避免显示早于真实所有权。
    fn drop(&mut self) {
        let service = self.acquired_at.elapsed();
        drop(self.permit.take());
        if let Some(reporter) = &self.reporter {
            if let Err(error) = reporter.resource_released_nowait(self.resource, service) {
                tracing::error!(error = %error, resource = ?self.resource, "读取资源 Drop 投影失败");
            }
            if let Err(error) = reporter.disk_read_released_nowait(&self.disk_ids, self.class) {
                tracing::error!(
                    error = %error,
                    disk_ids = ?self.disk_ids,
                    class = ?self.class,
                    "逐盘读取 active Drop 清理失败"
                );
            }
        }
    }
}

trait Md5ReadBackend: Send + Sync {
    fn read(
        &self,
        path: &Path,
        cancellation: &ReadCancellationToken,
        progress: &mut dyn FnMut(usize) -> io::Result<()>,
    ) -> Result<[u8; 16], ReadFailure>;
}

impl<R: BlockReader + Send + Sync> Md5ReadBackend for RetryingFileReader<R> {
    fn read(
        &self,
        path: &Path,
        cancellation: &ReadCancellationToken,
        progress: &mut dyn FnMut(usize) -> io::Result<()>,
    ) -> Result<[u8; 16], ReadFailure> {
        self.read_file_md5_with_progress(path, cancellation, progress)
    }
}

#[derive(Clone)]
enum LaneSource {
    /// 生产扫描为每条枚举行建立的只读精确 lane 映射。
    Frozen(Arc<HashMap<NormalizedPath, TaskDiskLane>>),
    /// 仅供已有受控读取器测试注入位置，生产不会启用。
    Injected(Arc<dyn Fn(&Path) -> (Vec<u32>, LocalDiskKind) + Send + Sync>),
}

impl ScheduledFileReader {
    /// 从已验证读取配置、Worker 数和枚举行冻结 lane 创建生产读取器。
    pub fn new_with_planned_rows(
        read_config: &DiskReadConfig,
        effective_worker_count: usize,
        planned_rows: Arc<Vec<PlannedScannedPath>>,
    ) -> Result<(Self, PipelineLimits), ScanError> {
        let scheduler = DiskReadScheduler::new(read_config, effective_worker_count)
            .map_err(|error| ScanError::Stage1(error.to_string()))?;
        let mut node_config = NodeConfig::default();
        node_config.read = read_config.clone();
        let reader = RetryingFileReader::system(&node_config)
            .map_err(|error| ScanError::Stage1(error.to_string()))?;
        let lane_map = freeze_lane_map(&planned_rows)?;
        let capacity = read_config
            .total_threads
            .checked_mul(4)
            .and_then(|total| {
                effective_worker_count
                    .checked_mul(2)
                    .map(|workers| total.max(workers))
            })
            .ok_or_else(|| ScanError::Stage1("扫描管道容量溢出".into()))?
            .min(MAX_PIPELINE_CHANNEL_CAPACITY);
        Ok((
            Self {
                scheduler,
                lane_group: None,
                reader: Arc::new(reader),
                lane_source: LaneSource::Frozen(Arc::new(lane_map)),
                read_config: read_config.clone(),
                reporter: None,
            },
            PipelineLimits::new(capacity, read_config.total_threads),
        ))
    }

    /// 使用真实 scheduler/retrying reader 与测试物理盘 resolver 装配可控 reader。
    #[doc(hidden)]
    pub fn controlled_for_test<R, F>(
        read_config: &DiskReadConfig,
        effective_worker_count: usize,
        block_reader: R,
        resolver: F,
    ) -> Result<(Self, PipelineLimits), ScanError>
    where
        R: BlockReader + Send + Sync + 'static,
        F: Fn(&Path) -> (Vec<u32>, LocalDiskKind) + Send + Sync + 'static,
    {
        let scheduler = DiskReadScheduler::new(read_config, effective_worker_count)
            .map_err(|error| ScanError::Stage1(error.to_string()))?;
        let mut config = NodeConfig::default();
        config.read = read_config.clone();
        let reader = RetryingFileReader::new(block_reader, &config)
            .map_err(|error| ScanError::Stage1(error.to_string()))?;
        let capacity = read_config
            .total_threads
            .checked_mul(4)
            .and_then(|total| {
                effective_worker_count
                    .checked_mul(2)
                    .map(|workers| total.max(workers))
            })
            .ok_or_else(|| ScanError::Stage1("扫描管道容量溢出".into()))?
            .min(MAX_PIPELINE_CHANNEL_CAPACITY);
        Ok((
            Self {
                scheduler,
                lane_group: None,
                reader: Arc::new(reader),
                lane_source: LaneSource::Injected(Arc::new(resolver)),
                read_config: read_config.clone(),
                reporter: None,
            },
            PipelineLimits::new(capacity, read_config.total_threads),
        ))
    }

    /// 使用已规划枚举行装配可控读取器，验证 Hash 与 Media 共用同一 lane。
    #[doc(hidden)]
    pub fn controlled_with_planned_rows_for_test<R>(
        read_config: &DiskReadConfig,
        effective_worker_count: usize,
        block_reader: R,
        planned_rows: Arc<Vec<PlannedScannedPath>>,
    ) -> Result<(Self, PipelineLimits), ScanError>
    where
        R: BlockReader + Send + Sync + 'static,
    {
        let scheduler = DiskReadScheduler::new(read_config, effective_worker_count)
            .map_err(|error| ScanError::Stage1(error.to_string()))?;
        let mut config = NodeConfig::default();
        config.read = read_config.clone();
        let reader = RetryingFileReader::new(block_reader, &config)
            .map_err(|error| ScanError::Stage1(error.to_string()))?;
        let lane_map = freeze_lane_map(&planned_rows)?;
        let capacity = read_config
            .total_threads
            .checked_mul(4)
            .and_then(|total| {
                effective_worker_count
                    .checked_mul(2)
                    .map(|workers| total.max(workers))
            })
            .ok_or_else(|| ScanError::Stage1("扫描管道容量溢出".into()))?
            .min(MAX_PIPELINE_CHANNEL_CAPACITY);
        Ok((
            Self {
                scheduler,
                lane_group: None,
                reader: Arc::new(reader),
                lane_source: LaneSource::Frozen(Arc::new(lane_map)),
                read_config: read_config.clone(),
                reporter: None,
            },
            PipelineLimits::new(capacity, read_config.total_threads),
        ))
    }

    /// 装配任务内存指标；文件完成数仍由单一归并边界统一写入。
    pub fn with_runtime_reporter(mut self, reporter: RuntimeTaskReporter) -> Self {
        self.reporter = Some(reporter);
        self
    }

    /// 测试专用关闭边界，用于证明 scheduler 拒绝请求时 waiting 由 RAII 清零。
    #[cfg(feature = "test-hooks")]
    #[doc(hidden)]
    pub async fn shutdown_scheduler_for_test(&self) -> Result<(), crate::io::SchedulerError> {
        self.scheduler.shutdown().await
    }

    /// 按冻结 lane 取得实际 scheduler 位置；生产路径不再访问 Windows 存储 API。
    fn resolve_lane(&self, path: &Path) -> io::Result<(TaskDiskLane, Option<StorageLocation>)> {
        match &self.lane_source {
            LaneSource::Frozen(lanes) => {
                let normalized = dedup_core::NormalizedPath::new(path).map_err(io::Error::other)?;
                let lane = lanes.get(&normalized).cloned().ok_or_else(|| {
                    io::Error::new(io::ErrorKind::NotFound, "路径没有对应的冻结物理盘 lane")
                })?;
                let location =
                    StorageLocation::from_parts(lane.physical_disk_id.clone(), lane.disk_kind);
                Ok((lane, Some(location)))
            }
            LaneSource::Injected(resolver) => {
                let (numbers, kind) = resolver(path);
                let disk_id =
                    PhysicalDiskId::from_disk_numbers(numbers).map_err(io::Error::other)?;
                let lane = TaskDiskLane {
                    physical_disk_numbers: disk_id.disk_numbers().to_vec(),
                    physical_disk_id: disk_id,
                    disk_kind: kind,
                    configured_weight: configured_limit(&self.read_config, kind),
                    per_disk_limit: configured_limit(&self.read_config, kind),
                };
                Ok((lane, None))
            }
        }
    }

    /// 按冻结 lane 取得指定类别的文件许可。
    async fn acquire_scheduled_permit(
        &self,
        scanned: &ScannedPath,
        cancellation: &ReadCancellationToken,
        class: DiskReadClass,
    ) -> Result<ScheduledReadPermit, ReadFailure> {
        let path = scanned.display_path.as_path().to_path_buf();
        if cancellation.is_cancelled() {
            return Err(ReadFailure::Cancelled);
        }
        let (lane, system_location) =
            self.resolve_lane(&path).map_err(|source| ReadFailure::Io {
                path: path.clone(),
                block_offset: 0,
                source,
            })?;
        self.acquire_permit_for_lane(&lane, cancellation, class, path, system_location, false)
            .await
    }

    /// 按已冻结 lane 取得一次读取许可；任务文件 dispatcher 直接调用此入口。
    async fn acquire_permit_for_lane(
        &self,
        lane: &TaskDiskLane,
        cancellation: &ReadCancellationToken,
        class: DiskReadClass,
        path: PathBuf,
        system_location: Option<StorageLocation>,
        use_frozen_scheduler_lane: bool,
    ) -> Result<ScheduledReadPermit, ReadFailure> {
        let wait_started = Instant::now();
        let disk_ids = lane
            .physical_disk_numbers
            .iter()
            .map(|disk_number| format!("PhysicalDisk{disk_number}"))
            .collect::<Vec<_>>();
        let runtime_class = runtime_disk_read_class(class);
        let mut wait_guard = RuntimeDiskWaitGuard::new(
            self.reporter.clone(),
            disk_ids.clone(),
            runtime_class,
            u64::try_from(lane.per_disk_limit).expect("已验证的逐盘读取容量必须可表示为 u64"),
        )
        .map_err(|error| telemetry_failure(&path, error))?;
        let lease = if use_frozen_scheduler_lane {
            let disk_lane = scheduled_disk_lane(lane);
            match &self.lane_group {
                Some(group) => {
                    self.scheduler
                        .acquire_lane_in_group(disk_lane, class, group.clone())
                        .await
                }
                None => self.scheduler.acquire_lane(disk_lane, class).await,
            }
        } else {
            match system_location {
                Some(storage) => {
                    self.scheduler
                        .acquire_with_limit(storage, class, lane.per_disk_limit)
                        .await
                }
                None => {
                    self.scheduler
                        .acquire_for_test(&lane.physical_disk_numbers, lane.disk_kind, class)
                        .await
                }
            }
        }
        .map_err(|error| ReadFailure::Io {
            path: path.clone(),
            block_offset: 0,
            source: io::Error::other(error.to_string()),
        })?;
        if cancellation.is_cancelled() {
            drop(lease);
            return Err(ReadFailure::Cancelled);
        }
        wait_guard
            .mark_acquired()
            .map_err(|error| telemetry_failure(&path, error))?;
        let resource = match class {
            DiskReadClass::HashSequential => RuntimePipelineResource::HashIo,
            DiskReadClass::MediaDecode => RuntimePipelineResource::MediaIo,
        };
        if let Some(reporter) = &self.reporter {
            let _ = reporter.resource_acquired_nowait(resource, wait_started.elapsed());
        }
        Ok(ScheduledReadPermit {
            permit: Some(lease),
            reporter: self.reporter.clone(),
            resource,
            disk_ids,
            class: runtime_class,
            acquired_at: Instant::now(),
        })
    }

    /// 仅消费调用方交付的 permit 完成 Hash；本函数不触碰 scheduler。
    fn read_hash_with_permit(
        &self,
        scanned: ScannedPath,
        permit: ScheduledReadPermit,
        cancellation: ReadCancellationToken,
        started: Option<HashReadStartedSignal>,
    ) -> Pin<
        Box<
            dyn Future<Output = Result<ReadProduct<ScheduledReadPermit>, ReadFailure>>
                + Send
                + 'static,
        >,
    > {
        let reader = self.reader.clone();
        Box::pin(async move {
            let path = scanned.display_path.as_path().to_path_buf();
            if cancellation.is_cancelled() {
                drop(permit);
                return Err(ReadFailure::Cancelled);
            }
            if let Some(started) = started {
                started.mark_reading();
            }
            let read_path = path.clone();
            let read_cancellation = cancellation.clone();
            let (md5, permit) = tokio::task::spawn_blocking(move || {
                let result = reader.read(&read_path, &read_cancellation, &mut |_| Ok(()));
                (result, permit)
            })
            .await
            .map_err(|error| join_failure(&path, error.to_string()))?;
            let md5 = md5?;
            Ok(ReadProduct { md5, lease: permit })
        })
    }
}

impl PipelineFileReader for ScheduledFileReader {
    type Lease = ScheduledReadPermit;

    fn read(
        &self,
        scanned: ScannedPath,
        cancellation: ReadCancellationToken,
    ) -> Pin<Box<dyn Future<Output = Result<ReadProduct<Self::Lease>, ReadFailure>> + Send + 'static>>
    {
        self.read_scheduled(scanned, cancellation, None)
    }

    /// 在 Hash 调度器真实许可取得后、spawn_blocking 读取开始前标记 reading。
    fn read_with_phase(
        &self,
        scanned: ScannedPath,
        cancellation: ReadCancellationToken,
        started: HashReadStartedSignal,
    ) -> Pin<Box<dyn Future<Output = Result<ReadProduct<Self::Lease>, ReadFailure>> + Send + 'static>>
    {
        self.read_scheduled(scanned, cancellation, Some(started))
    }

    // acquire_media_permit/physical_disk_id 的实现位于同一 trait impl，读取和媒体阶段共用冻结 lane。
    fn acquire_media_permit(
        &self,
        scanned: ScannedPath,
        cancellation: ReadCancellationToken,
    ) -> Pin<Box<dyn Future<Output = Result<Option<Self::Lease>, ReadFailure>> + Send + 'static>>
    {
        let permit_reader = self.clone();
        Box::pin(async move {
            permit_reader
                .acquire_scheduled_permit(&scanned, &cancellation, DiskReadClass::MediaDecode)
                .await
                .map(Some)
        })
    }

    /// 从冻结 lane 计算物理盘显示身份，不访问 Windows 存储解析器或建立路径缓存。
    fn physical_disk_id(&self, path: &Path) -> String {
        let Ok((lane, _)) = self.resolve_lane(path) else {
            return String::new();
        };
        format!(
            "PhysicalDisk{}",
            lane.physical_disk_numbers
                .iter()
                .map(u32::to_string)
                .collect::<Vec<_>>()
                .join("+")
        )
    }
}

impl ScheduledFileReader {
    /// 复用生产 Hash 读取逻辑；阶段信号只改变观测边界，不改变许可与读取顺序。
    fn read_scheduled(
        &self,
        scanned: ScannedPath,
        cancellation: ReadCancellationToken,
        started: Option<HashReadStartedSignal>,
    ) -> Pin<
        Box<
            dyn Future<Output = Result<ReadProduct<ScheduledReadPermit>, ReadFailure>>
                + Send
                + 'static,
        >,
    > {
        let permit_reader = self.clone();
        Box::pin(async move {
            let lease = permit_reader
                .acquire_scheduled_permit(&scanned, &cancellation, DiskReadClass::HashSequential)
                .await?;
            permit_reader
                .read_hash_with_permit(scanned, lease, cancellation, started)
                .await
        })
    }
}

impl HashPermitReader for ScheduledFileReader {
    type Permit = ScheduledReadPermit;

    fn read_with_permit(
        &self,
        scanned: ScannedPath,
        permit: Self::Permit,
        cancellation: ReadCancellationToken,
        started: Option<HashReadStartedSignal>,
    ) -> Pin<
        Box<dyn Future<Output = Result<ReadProduct<Self::Permit>, ReadFailure>> + Send + 'static>,
    > {
        self.read_hash_with_permit(scanned, permit, cancellation, started)
    }
}

impl TaskLanePermitProvider for ScheduledFileReader {
    type Permit = ScheduledReadPermit;

    fn begin_task_group(&self) -> Self {
        let mut reader = self.clone();
        reader.lane_group = Some(self.scheduler.new_lane_group());
        reader
    }

    fn configure_task_lanes(&self, lanes: &[TaskDiskLane]) -> io::Result<()> {
        let Some(group) = &self.lane_group else {
            return Err(io::Error::other("任务 dispatcher 未创建物理盘配额组"));
        };
        for lane in lanes {
            group
                .register(scheduled_disk_lane(lane))
                .map_err(io::Error::other)?;
        }
        Ok(())
    }

    fn freeze_task_lanes(&self) -> io::Result<()> {
        let Some(group) = &self.lane_group else {
            return Err(io::Error::other("任务 dispatcher 未创建物理盘配额组"));
        };
        group.freeze();
        Ok(())
    }

    fn unregister_task_lane(&self, lane: &TaskDiskLane) -> io::Result<()> {
        let Some(group) = &self.lane_group else {
            return Err(io::Error::other("任务 dispatcher 未创建物理盘配额组"));
        };
        group
            .unregister(&scheduled_disk_lane(lane))
            .map_err(io::Error::other)
    }

    fn clear_task_lanes(&self) {
        if let Some(group) = &self.lane_group {
            group.clear();
        }
    }

    fn acquire(
        &self,
        lane: TaskDiskLane,
        class: DiskReadClass,
        cancellation: ReadCancellationToken,
    ) -> TaskLanePermitFuture<Self::Permit> {
        let reader = self.clone();
        let path = PathBuf::from(format!(
            "PhysicalDisk{}",
            lane.physical_disk_numbers
                .iter()
                .map(u32::to_string)
                .collect::<Vec<_>>()
                .join("+")
        ));
        Box::pin(async move {
            let location =
                StorageLocation::from_parts(lane.physical_disk_id.clone(), lane.disk_kind);
            reader
                .acquire_permit_for_lane(&lane, &cancellation, class, path, Some(location), true)
                .await
        })
    }
}

/// 把任务文件 lane 转成读取调度器使用的拥有型配置。
fn scheduled_disk_lane(lane: &TaskDiskLane) -> DiskReadLane {
    DiskReadLane {
        location: StorageLocation::from_parts(lane.physical_disk_id.clone(), lane.disk_kind),
        effective_limit: lane.per_disk_limit,
        configured_weight: lane.configured_weight,
    }
}

/// 为生产读取器建立路径到冻结 lane 的只读精确索引。
fn freeze_lane_map(
    planned_rows: &[PlannedScannedPath],
) -> Result<HashMap<NormalizedPath, TaskDiskLane>, ScanError> {
    let mut lanes = HashMap::with_capacity(planned_rows.len());
    for planned in planned_rows {
        let path = planned.scanned.normalized_path.clone();
        if let Some(existing) = lanes.get(&path)
            && existing != &planned.lane
        {
            return Err(ScanError::InvalidResult(format!(
                "同一枚举路径关联多个冻结物理盘 lane: {path}"
            )));
        }
        lanes.entry(path).or_insert_with(|| planned.lane.clone());
    }
    Ok(lanes)
}

/// 返回指定介质类型对应的已验证逐盘读取额度。
fn configured_limit(read_config: &DiskReadConfig, kind: LocalDiskKind) -> usize {
    match kind {
        LocalDiskKind::Hdd => read_config.hdd_threads_per_disk,
        LocalDiskKind::Ssd => read_config.ssd_threads_per_disk,
        LocalDiskKind::Unknown => read_config.unknown_threads_per_disk,
    }
}

pub(super) fn spawn_bounded_enumeration<E>(
    enumerator: E,
    roots: Vec<dedup_core::DisplayPath>,
    capacity: usize,
    cancellation: ReadCancellationToken,
) -> (
    mpsc::Receiver<EnumerationEvent>,
    JoinHandle<Result<(), ScanError>>,
)
where
    E: FileEnumerator + Send + 'static,
{
    let (sender, receiver) = mpsc::channel(capacity.max(1));
    let task = tokio::task::spawn_blocking(move || {
        let send = |event| {
            if cancellation.is_cancelled() {
                return Err(ScanError::Cancelled);
            }
            sender.blocking_send(event).map_err(|_| {
                if cancellation.is_cancelled() {
                    ScanError::Cancelled
                } else {
                    ScanError::Stage1("扫描枚举下游已经关闭".into())
                }
            })
        };
        enumerator.enumerate_into_with_completion(
            &roots,
            &mut |totals| send(EnumerationEvent::Completed(totals)),
            &mut |row| send(EnumerationEvent::Row(row)),
        )
    });
    (receiver, task)
}

fn join_failure(path: &PathBuf, message: String) -> ReadFailure {
    ReadFailure::Io {
        path: path.clone(),
        block_offset: 0,
        source: io::Error::other(message),
    }
}

/// 把 scheduler 读取类别映射到逐盘 registry 类别，保持两个指标边界一致。
const fn runtime_disk_read_class(class: DiskReadClass) -> RuntimeDiskReadClass {
    match class {
        DiskReadClass::HashSequential => RuntimeDiskReadClass::Hash,
        DiskReadClass::MediaDecode => RuntimeDiskReadClass::Media,
    }
}

/// 把逐盘遥测转换错误投影为当前文件读取失败，不允许带着半状态继续读取。
fn telemetry_failure(path: &PathBuf, error: RuntimeTaskError) -> ReadFailure {
    join_failure(path, format!("记录逐盘读取许可失败: {error}"))
}
