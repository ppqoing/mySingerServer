//! 有界枚举生产器与按物理盘调度的生产 MD5 读取适配器。

use std::{
    collections::BTreeMap,
    future::Future,
    io,
    path::{Path, PathBuf},
    pin::Pin,
    sync::{Arc, Mutex},
    time::Instant,
};

use dedup_core::{DiskReadConfig, NodeConfig};
use dedup_node_store::ScannedPath;
use dedup_windows::{LocalDiskKind, ReadCancellationToken, resolve_storage_location};
use tokio::{sync::mpsc, task::JoinHandle};

use crate::{
    io::{
        BlockReader, DiskReadClass, DiskReadPermit, DiskReadScheduler, ReadFailure,
        RetryingFileReader,
    },
    runtime_tasks::{
        RuntimeDiskReadClass, RuntimePipelineResource, RuntimeTaskError, RuntimeTaskReporter,
    },
};

use super::base_flow_control::HashReadStartedSignal;
use super::{FileEnumerator, ScanError};

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

    pub(super) const fn max_read_tasks(self) -> usize {
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

    /// 返回最近一次为路径解析的物理盘显示身份。
    fn take_physical_disk_id(&self, _path: &Path) -> String {
        String::new()
    }
}

/// 生产路径使用的物理盘调度、OVERLAPPED 重试 MD5 组合。
#[derive(Clone)]
pub struct ScheduledFileReader {
    scheduler: DiskReadScheduler,
    reader: Arc<dyn Md5ReadBackend>,
    resolver: LocationResolver,
    locations: Arc<Mutex<BTreeMap<PathBuf, String>>>,
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
enum LocationResolver {
    System,
    Injected(Arc<dyn Fn(&Path) -> (Vec<u32>, LocalDiskKind) + Send + Sync>),
}

impl ScheduledFileReader {
    /// 从已验证读取配置和实际 Worker 数创建生产读取器及统一有界容量。
    pub fn new(
        read_config: &DiskReadConfig,
        effective_worker_count: usize,
    ) -> Result<(Self, PipelineLimits), ScanError> {
        let scheduler = DiskReadScheduler::new(read_config, effective_worker_count)
            .map_err(|error| ScanError::Stage1(error.to_string()))?;
        let mut node_config = NodeConfig::default();
        node_config.read = read_config.clone();
        let reader = RetryingFileReader::system(&node_config)
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
                reader: Arc::new(reader),
                resolver: LocationResolver::System,
                locations: Arc::new(Mutex::new(BTreeMap::new())),
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
                reader: Arc::new(reader),
                resolver: LocationResolver::Injected(Arc::new(resolver)),
                locations: Arc::new(Mutex::new(BTreeMap::new())),
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

    /// 按本次解析到的介质类型返回 registry 应观察的单盘硬容量。
    fn observed_disk_capacity(&self, kind: LocalDiskKind) -> u64 {
        let capacity = match kind {
            LocalDiskKind::Hdd => self.read_config.hdd_threads_per_disk,
            LocalDiskKind::Ssd => self.read_config.ssd_threads_per_disk,
            LocalDiskKind::Unknown => self.read_config.unknown_threads_per_disk,
        };
        u64::try_from(capacity).expect("已验证的逐盘读取容量必须可表示为 u64")
    }

    /// 按指定类别取得文件所在物理盘许可，并按需记住 Hash 阶段解析出的盘身份。
    async fn acquire_scheduled_permit(
        &self,
        scanned: &ScannedPath,
        cancellation: &ReadCancellationToken,
        class: DiskReadClass,
        remember_location: bool,
    ) -> Result<ScheduledReadPermit, ReadFailure> {
        let wait_started = Instant::now();
        let path = scanned.display_path.as_path().to_path_buf();
        if cancellation.is_cancelled() {
            return Err(ReadFailure::Cancelled);
        }
        let (mut disk_numbers, kind, system_location) = match &self.resolver {
            LocationResolver::System => {
                let resolved_path = path.clone();
                let storage =
                    tokio::task::spawn_blocking(move || resolve_storage_location(&resolved_path))
                        .await
                        .map_err(|error| join_failure(&path, error.to_string()))?
                        .map_err(|source| ReadFailure::Io {
                            path: path.clone(),
                            block_offset: 0,
                            source,
                        })?;
                let numbers = storage.physical_disk_id().disk_numbers().to_vec();
                let kind = storage.disk_kind();
                (numbers, kind, Some(storage))
            }
            LocationResolver::Injected(resolver) => {
                let (numbers, kind) = resolver(&path);
                (numbers, kind, None)
            }
        };
        disk_numbers.sort_unstable();
        disk_numbers.dedup();
        if disk_numbers.is_empty() {
            return Err(join_failure(&path, "物理盘身份不能为空".into()));
        }
        let disk_ids = disk_numbers
            .iter()
            .map(|disk_number| format!("PhysicalDisk{disk_number}"))
            .collect::<Vec<_>>();
        let runtime_class = runtime_disk_read_class(class);
        let mut wait_guard = RuntimeDiskWaitGuard::new(
            self.reporter.clone(),
            disk_ids.clone(),
            runtime_class,
            self.observed_disk_capacity(kind),
        )
        .map_err(|error| telemetry_failure(&path, error))?;
        let lease = match system_location {
            Some(storage) => self.scheduler.acquire(storage, class).await,
            None => {
                self.scheduler
                    .acquire_for_test(&disk_numbers, kind, class)
                    .await
            }
        }
        .map_err(|error| ReadFailure::Io {
            path: path.clone(),
            block_offset: 0,
            source: io::Error::other(error.to_string()),
        })?;
        wait_guard
            .mark_acquired()
            .map_err(|error| telemetry_failure(&path, error))?;
        let physical_disk_id = format!(
            "PhysicalDisk{}",
            disk_numbers
                .iter()
                .map(u32::to_string)
                .collect::<Vec<_>>()
                .join("+")
        );
        if remember_location {
            self.locations
                .lock()
                .unwrap()
                .insert(path, physical_disk_id);
        }
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

    // acquire_media_permit/take_physical_disk_id 的既有实现位于同一 trait impl，签名和语义不变。
    fn acquire_media_permit(
        &self,
        scanned: ScannedPath,
        cancellation: ReadCancellationToken,
    ) -> Pin<Box<dyn Future<Output = Result<Option<Self::Lease>, ReadFailure>> + Send + 'static>>
    {
        let permit_reader = self.clone();
        Box::pin(async move {
            permit_reader
                .acquire_scheduled_permit(
                    &scanned,
                    &cancellation,
                    DiskReadClass::MediaDecode,
                    false,
                )
                .await
                .map(Some)
        })
    }

    /// 取出 Hash 读取阶段解析出的物理盘显示身份并从缓存中移除。
    fn take_physical_disk_id(&self, path: &Path) -> String {
        self.locations
            .lock()
            .unwrap()
            .remove(path)
            .unwrap_or_default()
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
        let reader = self.reader.clone();
        let permit_reader = self.clone();
        Box::pin(async move {
            let path = scanned.display_path.as_path().to_path_buf();
            let lease = permit_reader
                .acquire_scheduled_permit(
                    &scanned,
                    &cancellation,
                    DiskReadClass::HashSequential,
                    true,
                )
                .await?;
            if let Some(started) = started {
                started.mark_reading();
            }
            let read_path = path.clone();
            let read_cancellation = cancellation.clone();
            let (md5, lease) = tokio::task::spawn_blocking(move || {
                let result = reader.read(&read_path, &read_cancellation, &mut |_| Ok(()));
                (result, lease)
            })
            .await
            .map_err(|error| join_failure(&path, error.to_string()))?;
            let md5 = md5?;
            Ok(ReadProduct { md5, lease })
        })
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
