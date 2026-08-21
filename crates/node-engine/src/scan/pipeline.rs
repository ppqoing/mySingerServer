//! 有界枚举生产器与按物理盘调度的生产 MD5 读取适配器。

use std::{collections::BTreeMap, future::Future, io, path::{Path, PathBuf}, pin::Pin, sync::{Arc, Mutex}};

use dedup_core::{DiskReadConfig, NodeConfig};
use dedup_node_store::ScannedPath;
use dedup_windows::{OverlappedFileReader, ReadCancellationToken, resolve_storage_location};
use tokio::{sync::mpsc, task::JoinHandle};

use crate::{
    io::{DiskReadPermit, DiskReadScheduler, ReadFailure, RetryingFileReader},
    runtime_tasks::{RuntimeStage, RuntimeTaskReporter},
};

use super::{FileEnumerator, ScanError};

/// 扫描五段管道共享的有界容量与最大并行读任务数。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct PipelineLimits {
    channel_capacity: usize,
    max_read_tasks: usize,
}

impl PipelineLimits {
    /// 创建测试或已验证配置使用的明确上限。
    pub const fn new(channel_capacity: usize, max_read_tasks: usize) -> Self {
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

/// MD5 与必须持有到 SQLite 写回后的文件访问租约。
pub struct ReadProduct<L> {
    /// 完整文件的 16 字节 MD5。
    pub md5: [u8; 16],
    /// 覆盖读取、Worker FFmpeg 访问和 SQLite 持久化的租约。
    pub lease: L,
}

/// 可并行派发的文件读取边界。
pub trait PipelineFileReader: Clone + Send + Sync + 'static {
    /// 随读取结果继续流向 Worker 和 SQLite writer 的租约。
    type Lease: Send + 'static;

    /// 读取一个缓存未命中文件；future 必须可在线程池任务中运行。
    fn read(
        &self,
        scanned: ScannedPath,
        cancellation: ReadCancellationToken,
    ) -> Pin<Box<dyn Future<Output = Result<ReadProduct<Self::Lease>, ReadFailure>> + Send + 'static>>;

    /// 返回最近一次为路径解析的物理盘显示身份。
    fn physical_disk_id(&self, _path: &Path) -> String {
        String::new()
    }
}

/// 生产路径使用的物理盘调度、OVERLAPPED 重试 MD5 组合。
#[derive(Clone)]
pub struct ScheduledFileReader {
    scheduler: DiskReadScheduler,
    reader: Arc<RetryingFileReader<OverlappedFileReader>>,
    reporter: Option<RuntimeTaskReporter>,
    locations: Arc<Mutex<BTreeMap<PathBuf, String>>>,
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
            .ok_or_else(|| ScanError::Stage1("扫描管道容量溢出".into()))?;
        Ok((
            Self {
                scheduler,
                reader: Arc::new(reader),
                reporter: None,
                locations: Arc::new(Mutex::new(BTreeMap::new())),
            },
            PipelineLimits::new(capacity, capacity),
        ))
    }

    /// 接入扫描运行时读取字节 reporter。
    pub fn with_runtime_reporter(mut self, reporter: RuntimeTaskReporter) -> Self {
        self.reporter = Some(reporter);
        self
    }
}

impl PipelineFileReader for ScheduledFileReader {
    type Lease = DiskReadPermit;

    fn read(
        &self,
        scanned: ScannedPath,
        cancellation: ReadCancellationToken,
    ) -> Pin<Box<dyn Future<Output = Result<ReadProduct<Self::Lease>, ReadFailure>> + Send + 'static>>
    {
        let scheduler = self.scheduler.clone();
        let reader = self.reader.clone();
        let reporter = self.reporter.clone();
        let locations = self.locations.clone();
        Box::pin(async move {
            let path = scanned.display_path.as_path().to_path_buf();
            if cancellation.is_cancelled() {
                return Err(ReadFailure::Cancelled);
            }
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
            let physical_disk_id = format!(
                "PhysicalDisk{}",
                storage
                    .physical_disk_id()
                    .disk_numbers()
                    .iter()
                    .map(u32::to_string)
                    .collect::<Vec<_>>()
                    .join("+")
            );
            locations.lock().unwrap().insert(path.clone(), physical_disk_id);
            let lease = scheduler
                .acquire(storage)
                .await
                .map_err(|error| ReadFailure::Io {
                    path: path.clone(),
                    block_offset: 0,
                    source: io::Error::other(error.to_string()),
                })?;
            let read_path = path.clone();
            let read_cancellation = cancellation.clone();
            let (md5, lease) = tokio::task::spawn_blocking(move || {
                let result = reader.read_file_md5_with_progress(
                    &read_path,
                    &read_cancellation,
                    |bytes| {
                        if let Some(reporter) = &reporter {
                            let _ = reporter.advance_stage_nowait(
                                RuntimeStage::ReadMd5,
                                crate::runtime_tasks::RuntimeProgressUnit::Bytes,
                                bytes as u64,
                            );
                        }
                        Ok(())
                    },
                );
                (result, lease)
            })
            .await
            .map_err(|error| join_failure(&path, error.to_string()))?;
            let md5 = md5?;
            Ok(ReadProduct { md5, lease })
        })
    }

    fn physical_disk_id(&self, path: &Path) -> String {
        self.locations
            .lock()
            .unwrap()
            .get(path)
            .cloned()
            .unwrap_or_default()
    }
}

pub(super) fn spawn_bounded_enumeration<E>(
    enumerator: E,
    roots: Vec<dedup_core::DisplayPath>,
    capacity: usize,
    cancellation: ReadCancellationToken,
) -> (
    mpsc::Receiver<ScannedPath>,
    JoinHandle<Result<(), ScanError>>,
)
where
    E: FileEnumerator + Send + 'static,
{
    let (sender, receiver) = mpsc::channel(capacity.max(1));
    let task = tokio::task::spawn_blocking(move || {
        enumerator.enumerate_into(&roots, &mut |row| {
            if cancellation.is_cancelled() {
                return Err(ScanError::Cancelled);
            }
            sender.blocking_send(row).map_err(|_| {
                if cancellation.is_cancelled() {
                    ScanError::Cancelled
                } else {
                    ScanError::Stage1("扫描枚举下游已经关闭".into())
                }
            })
        })
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
