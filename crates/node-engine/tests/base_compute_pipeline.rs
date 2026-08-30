use std::{
    collections::BTreeMap,
    future::Future,
    io,
    path::{Path, PathBuf},
    pin::Pin,
    sync::{
        Arc, Condvar, Mutex,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    time::Duration,
};

use dedup_core::{ContentKey, DiskReadConfig, DisplayPath, MachineId, MediaKind, NormalizedPath};
use dedup_media::{ImageStage1, PdqHash};
use dedup_media_ffmpeg::MediaProbe;
#[cfg(feature = "test-hooks")]
use dedup_node_engine::actor::run_enumerated_scan_to_base_compute_for_test;
#[cfg(feature = "test-hooks")]
use dedup_node_engine::scan::BaseComputeJoinObservationHooks;
#[cfg(feature = "test-hooks")]
use dedup_node_engine::scan::BasePersistTestController;
#[cfg(feature = "test-hooks")]
use dedup_node_engine::scan::run_scheduled_task_file_scan_for_test;
use dedup_node_engine::{
    DisabledRemoteFeatureCache, RemoteCacheError, RemoteFeatureCache,
    artifact_registry::RegenerableArtifactRegistry,
    disk_full_cleanup::{DiskFullCleaner, SystemArtifactDiskResolver},
    io::{BlockReadError, BlockReader},
    runtime_tasks::{
        RuntimeExecutionConfigUpdate, RuntimeTaskKind, RuntimeTaskRegistry, RuntimeTaskReporter,
    },
    scan::{
        BaseComputeDecision, BaseComputeEngine, PipelineFileReader, PipelineLimits,
        PlannedScannedPath, ReadProduct, ScanOptions, ScheduledFileReader, TaskDiskLane,
        begin_scan_task, md5_bytes,
    },
    worker::{BaseComputeOutput, Stage1Frame, WorkerPool, encode_base_compute_payload},
};
use dedup_node_store::{
    BaseCacheRecord, CompleteStage1, NodeStore, ScannedPath, TaskItemStatus, TaskStatus,
};
use dedup_protocol::proto;
use dedup_protocol::{BASE_MISSING_CONTACT_SHEET, BASE_MISSING_PROBE, BASE_MISSING_STAGE1};
use dedup_windows::{LocalDiskKind, PhysicalDiskId, ReadCancellationToken};
use tempfile::tempdir;
use tokio::sync::{Notify, OwnedSemaphorePermit, Semaphore};

/// 记录 Node Hash 启动数与磁盘许可存活数的可控读取器。
#[derive(Clone)]
struct CountingHashReader {
    /// 每个测试路径对应的固定 MD5，避免依赖调度完成顺序。
    hashes: Arc<BTreeMap<PathBuf, [u8; 16]>>,
    /// 已进入真实读取 future 的文件数。
    started: Arc<AtomicUsize>,
    /// 尚未被 Node 显式释放的 Hash 许可数。
    active_leases: Arc<AtomicUsize>,
}

/// Drop 时归还计数的测试 Hash 许可。
struct DropSpyLease(Arc<AtomicUsize>);

impl Drop for DropSpyLease {
    fn drop(&mut self) {
        self.0.fetch_sub(1, Ordering::AcqRel);
    }
}

impl PipelineFileReader for CountingHashReader {
    type Lease = DropSpyLease;

    fn read(
        &self,
        scanned: ScannedPath,
        _cancellation: ReadCancellationToken,
    ) -> Pin<
        Box<
            dyn Future<
                    Output = Result<ReadProduct<Self::Lease>, dedup_node_engine::io::ReadFailure>,
                > + Send
                + 'static,
        >,
    > {
        let md5 = self.hashes[scanned.display_path.as_path()];
        let started = Arc::clone(&self.started);
        let active_leases = Arc::clone(&self.active_leases);
        Box::pin(async move {
            started.fetch_add(1, Ordering::AcqRel);
            active_leases.fetch_add(1, Ordering::AcqRel);
            Ok(ReadProduct {
                md5,
                lease: DropSpyLease(active_leases),
            })
        })
    }
}

/// 为固定清单分配互不相同的 MD5，并返回可观察的 Node Hash reader。
fn counting_reader_for(
    rows: &[ScannedPath],
) -> (
    CountingHashReader,
    Arc<BTreeMap<PathBuf, [u8; 16]>>,
    Arc<AtomicUsize>,
    Arc<AtomicUsize>,
) {
    let hashes = Arc::new(
        rows.iter()
            .enumerate()
            .map(|(index, row)| {
                (
                    row.display_path.as_path().to_path_buf(),
                    [(index + 1) as u8; 16],
                )
            })
            .collect(),
    );
    let started = Arc::new(AtomicUsize::new(0));
    let active_leases = Arc::new(AtomicUsize::new(0));
    (
        CountingHashReader {
            hashes: Arc::clone(&hashes),
            started: Arc::clone(&started),
            active_leases: Arc::clone(&active_leases),
        },
        hashes,
        started,
        active_leases,
    )
}

/// 按 Worker 当前冻结路径取得 Node 已生成的 MD5。
fn running_md5(
    controller: &dedup_node_engine::worker::ControlledWorkerPool,
    item_id: &str,
    hashes: &BTreeMap<PathBuf, [u8; 16]>,
) -> [u8; 16] {
    let identity = controller
        .running_files()
        .into_iter()
        .find(|(_, running, _)| running == item_id)
        .expect("Worker 运行项应保留冻结路径")
        .2;
    hashes[identity.display_path.as_path()]
}

/// 单个虚拟物理盘的 Hash 槽位与活动计数。
#[derive(Clone)]
struct VirtualDiskState {
    /// 模拟生产每盘读取上限的异步槽位。
    slots: Arc<Semaphore>,
    /// 当前持有虚拟物理盘许可的 Hash 数。
    active: Arc<AtomicUsize>,
    /// 测试期间观察到的最大活动 Hash 数。
    peak: Arc<AtomicUsize>,
}

impl VirtualDiskState {
    /// 创建指定每盘并发上限的受控状态。
    fn new(limit: usize) -> Self {
        Self {
            slots: Arc::new(Semaphore::new(limit)),
            active: Arc::new(AtomicUsize::new(0)),
            peak: Arc::new(AtomicUsize::new(0)),
        }
    }
}

/// Hash 许可析构时同步归还虚拟盘槽位和活动 ownership。
struct VirtualDiskLease {
    /// 持有到 BaseCompute 显式释放的虚拟盘槽位。
    _slot: OwnedSemaphorePermit,
    /// 对应虚拟盘的活动 Hash 计数。
    active: Arc<AtomicUsize>,
}

impl Drop for VirtualDiskLease {
    fn drop(&mut self) {
        self.active.fetch_sub(1, Ordering::AcqRel);
    }
}

/// 按两个规范根映射 PhysicalDisk1/2 并记录成功 Hash 启动顺序。
#[derive(Clone)]
struct DualDiskHashReader {
    /// H 虚拟根，对应 PhysicalDisk1。
    first_root: NormalizedPath,
    /// I 虚拟根，对应 PhysicalDisk2。
    second_root: NormalizedPath,
    /// PhysicalDisk1 的许可与峰值状态。
    first_disk: VirtualDiskState,
    /// PhysicalDisk2 的许可与峰值状态。
    second_disk: VirtualDiskState,
    /// 每个虚拟路径对应的唯一固定 MD5。
    hashes: Arc<BTreeMap<PathBuf, [u8; 16]>>,
    /// 成功取得盘许可后的 Hash 启动路径。
    started_paths: Arc<Mutex<Vec<NormalizedPath>>>,
}

impl PipelineFileReader for DualDiskHashReader {
    type Lease = VirtualDiskLease;

    fn read(
        &self,
        scanned: ScannedPath,
        _cancellation: ReadCancellationToken,
    ) -> Pin<
        Box<
            dyn Future<
                    Output = Result<ReadProduct<Self::Lease>, dedup_node_engine::io::ReadFailure>,
                > + Send
                + 'static,
        >,
    > {
        // 先冻结本次路径所属的虚拟物理盘，避免 future 内再访问 reader。
        let disk = if scanned.normalized_path.is_within(&self.first_root) {
            self.first_disk.clone()
        } else if scanned.normalized_path.is_within(&self.second_root) {
            self.second_disk.clone()
        } else {
            panic!("测试路径不属于任一虚拟物理盘: {}", scanned.normalized_path);
        };
        let path = scanned.normalized_path.clone();
        let display_path = scanned.display_path.as_path().to_path_buf();
        let md5 = self.hashes[&display_path];
        let started_paths = Arc::clone(&self.started_paths);
        Box::pin(async move {
            let slot = disk.slots.acquire_owned().await.map_err(|_| {
                dedup_node_engine::io::ReadFailure::Io {
                    path: display_path,
                    block_offset: 0,
                    source: io::Error::other("虚拟物理盘读取调度器已关闭"),
                }
            })?;
            let active = disk.active.fetch_add(1, Ordering::AcqRel) + 1;
            disk.peak.fetch_max(active, Ordering::AcqRel);
            started_paths.lock().unwrap().push(path);
            tokio::task::yield_now().await;
            Ok(ReadProduct {
                md5,
                lease: VirtualDiskLease {
                    _slot: slot,
                    active: disk.active,
                },
            })
        })
    }
}

/// 创建两个虚拟物理盘共用的确定性 Hash reader 与观察状态。
fn dual_disk_reader_for(
    first_root: NormalizedPath,
    second_root: NormalizedPath,
    rows: &[ScannedPath],
    per_disk_limit: usize,
) -> DualDiskHashReader {
    let hashes = rows
        .iter()
        .enumerate()
        .map(|(index, row)| {
            (
                row.display_path.as_path().to_path_buf(),
                u128::try_from(index + 1).unwrap().to_le_bytes(),
            )
        })
        .collect();
    DualDiskHashReader {
        first_root,
        second_root,
        first_disk: VirtualDiskState::new(per_disk_limit),
        second_disk: VirtualDiskState::new(per_disk_limit),
        hashes: Arc::new(hashes),
        started_paths: Arc::new(Mutex::new(Vec::with_capacity(rows.len()))),
    }
}

/// 在第二个路径缓存批次暂停，供测试观察第一批已经发布的进度。
struct GatedPathCache {
    lookup_calls: Arc<AtomicUsize>,
    second_batch_started: Arc<Notify>,
    release_second_batch: Arc<Notify>,
}

/// 暂停首个 path 查询，使 BaseCompute 在上游仍开放时真实触发一次空 Hash claim。
struct OpenEmptyPathCache {
    /// 首个 path 请求已经进入 resolver 的通知。
    lookup_started: Arc<Notify>,
    /// 发布 path 项并唤醒上游的 gate。
    release_lookup: Arc<Notify>,
}

impl RemoteFeatureCache for OpenEmptyPathCache {
    async fn lookup_paths(
        &self,
        _machine_id: &MachineId,
        paths: &[ScannedPath],
    ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
        self.lookup_started.notify_one();
        self.release_lookup.notified().await;
        Ok(vec![None; paths.len()])
    }

    async fn lookup_contents(
        &self,
        keys: &[ContentKey],
    ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
        Ok(vec![None; keys.len()])
    }

    async fn publish_outbox(
        &mut self,
        _machine_id: &MachineId,
        _batch: &dedup_protocol::proto::SyncChangeBatch,
    ) -> Result<u64, RemoteCacheError> {
        Ok(0)
    }
}

impl RemoteFeatureCache for GatedPathCache {
    async fn lookup_paths(
        &self,
        _machine_id: &MachineId,
        paths: &[ScannedPath],
    ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
        let call = self.lookup_calls.fetch_add(1, Ordering::SeqCst) + 1;
        if call == 2 {
            self.second_batch_started.notify_one();
            self.release_second_batch.notified().await;
        }
        Ok(vec![None; paths.len()])
    }

    async fn lookup_contents(
        &self,
        keys: &[ContentKey],
    ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
        Ok(vec![None; keys.len()])
    }

    async fn publish_outbox(
        &mut self,
        _machine_id: &MachineId,
        _batch: &dedup_protocol::proto::SyncChangeBatch,
    ) -> Result<u64, RemoteCacheError> {
        Ok(0)
    }
}

/// 新基础计算不会在 Node 内读取文件；若测试触发本读取器即说明实现回退了旧方案。
#[derive(Clone, Copy)]
struct NeverRead;

impl BlockReader for NeverRead {
    fn read_at(
        &self,
        _path: &Path,
        _offset: u64,
        _buffer: &mut [u8],
        _timeout: Duration,
        _cancellation: &ReadCancellationToken,
    ) -> Result<usize, BlockReadError> {
        Err(BlockReadError::Io(io::Error::other(
            "Node 不应直接读取基础计算文件",
        )))
    }
}

/// 用固定字节填满调用方缓冲区，供 ScheduledFileReader 生命周期测试完成真实 Hash 读取。
#[derive(Clone, Copy)]
struct FixedBlockReader;

impl BlockReader for FixedBlockReader {
    /// 返回完整目标缓冲区，文件长度仍由测试 fixture 的真实 metadata 决定。
    fn read_at(
        &self,
        _path: &Path,
        _offset: u64,
        buffer: &mut [u8],
        _timeout: Duration,
        _cancellation: &ReadCancellationToken,
    ) -> Result<usize, BlockReadError> {
        buffer.fill(0x5A);
        Ok(buffer.len())
    }
}

/// 阻塞真实 ScheduledFileReader 底层块读取，用于观察配置派生的 Hash 并发上限。
#[derive(Clone)]
struct BlockingBlockReader {
    /// 已进入底层读取的文件数。
    started: Arc<AtomicUsize>,
    /// 测试统一释放所有阻塞读取的门禁。
    gate: Arc<(Mutex<bool>, Condvar)>,
}

/// 确保测试失败或超时时也会解除阻塞块读取，避免遗留 spawn_blocking 线程。
struct BlockingReadRelease(Arc<(Mutex<bool>, Condvar)>);

impl BlockingReadRelease {
    /// 幂等放行当前门禁的全部块读取。
    fn release(&self) {
        let (released, changed) = &*self.0;
        *released.lock().unwrap() = true;
        changed.notify_all();
    }
}

impl Drop for BlockingReadRelease {
    fn drop(&mut self) {
        self.release();
    }
}

impl BlockReader for BlockingBlockReader {
    fn read_at(
        &self,
        path: &Path,
        _offset: u64,
        buffer: &mut [u8],
        _timeout: Duration,
        _cancellation: &ReadCancellationToken,
    ) -> Result<usize, BlockReadError> {
        self.started.fetch_add(1, Ordering::AcqRel);
        let (released, changed) = &*self.gate;
        let mut released = released.lock().unwrap();
        while !*released {
            released = changed.wait(released).unwrap();
        }
        let byte = path
            .file_stem()
            .and_then(|name| name.to_string_lossy().chars().last())
            .and_then(|value| value.to_digit(10))
            .unwrap() as u8;
        buffer.fill(byte);
        Ok(buffer.len())
    }
}

/// 单个测试路径的 Hash 行为，用于覆盖成功、文件失败和取消 drain。
#[derive(Clone)]
enum ScriptedHashBehavior {
    /// 返回固定 Node MD5。
    Success([u8; 16]),
    /// 等所有指定 Hash 都启动后再返回，稳定制造并行中的活动读取。
    SuccessAfterAllStarted { md5: [u8; 16], expected: usize },
    /// 等测试显式释放后返回，用于证明 content 批查不会等待慢 Hash 凑批。
    SuccessAfterRelease {
        /// 本 Hash 已进入等待点的通知。
        entered: Arc<Notify>,
        /// 允许本 Hash 返回的测试门禁。
        release: Arc<Notify>,
        /// 释放后返回的固定 MD5。
        md5: [u8; 16],
    },
    /// 等待可持久观察的原子门禁，允许测试先形成单项 content 批次再放开窗口。
    SuccessAfterFlag {
        /// 本 Hash 已进入等待点的通知。
        entered: Arc<Notify>,
        /// true 后允许本 Hash 返回。
        release: Arc<AtomicBool>,
        /// 释放后返回的固定 MD5。
        md5: [u8; 16],
    },
    /// 模拟取得许可后的普通文件读取失败。
    Fail,
    /// 等待取消，再延迟指定毫秒后归还许可。
    WaitForCancel {
        /// 取消已被慢 Hash 观察到。
        observed: Arc<AtomicUsize>,
        /// 慢 Hash 已完成延迟清理并返回。
        completed: Arc<AtomicUsize>,
        /// 观察取消后的清理延迟毫秒数。
        delay_ms: u64,
    },
}

/// 按路径执行固定 Hash 行为并观察所有许可生命周期。
#[derive(Clone)]
struct ScriptedHashReader {
    /// 路径到测试行为的固定映射。
    behaviors: Arc<BTreeMap<PathBuf, ScriptedHashBehavior>>,
    /// 已开始的读取 future 数。
    started: Arc<AtomicUsize>,
    /// 当前仍存活的读取许可数。
    active_leases: Arc<AtomicUsize>,
}

impl PipelineFileReader for ScriptedHashReader {
    type Lease = DropSpyLease;

    fn read(
        &self,
        scanned: ScannedPath,
        cancellation: ReadCancellationToken,
    ) -> Pin<
        Box<
            dyn Future<
                    Output = Result<ReadProduct<Self::Lease>, dedup_node_engine::io::ReadFailure>,
                > + Send
                + 'static,
        >,
    > {
        let path = scanned.display_path.as_path().to_path_buf();
        let behavior = self.behaviors[&path].clone();
        let started = Arc::clone(&self.started);
        let active_leases = Arc::clone(&self.active_leases);
        Box::pin(async move {
            started.fetch_add(1, Ordering::AcqRel);
            active_leases.fetch_add(1, Ordering::AcqRel);
            let lease = DropSpyLease(active_leases);
            match behavior {
                ScriptedHashBehavior::Success(md5) => Ok(ReadProduct { md5, lease }),
                ScriptedHashBehavior::SuccessAfterAllStarted { md5, expected } => {
                    while started.load(Ordering::Acquire) < expected {
                        tokio::task::yield_now().await;
                    }
                    Ok(ReadProduct { md5, lease })
                }
                ScriptedHashBehavior::SuccessAfterRelease {
                    entered,
                    release,
                    md5,
                } => {
                    entered.notify_one();
                    release.notified().await;
                    Ok(ReadProduct { md5, lease })
                }
                ScriptedHashBehavior::SuccessAfterFlag {
                    entered,
                    release,
                    md5,
                } => {
                    entered.notify_one();
                    while !release.load(Ordering::Acquire) {
                        tokio::task::yield_now().await;
                    }
                    Ok(ReadProduct { md5, lease })
                }
                ScriptedHashBehavior::Fail => Err(dedup_node_engine::io::ReadFailure::Io {
                    path,
                    block_offset: 0,
                    source: io::Error::other("测试 Hash 读取失败"),
                }),
                ScriptedHashBehavior::WaitForCancel {
                    observed,
                    completed,
                    delay_ms,
                } => {
                    while !cancellation.is_cancelled() {
                        tokio::task::yield_now().await;
                    }
                    observed.fetch_add(1, Ordering::AcqRel);
                    tokio::time::sleep(Duration::from_millis(delay_ms)).await;
                    drop(lease);
                    completed.fetch_add(1, Ordering::AcqRel);
                    Err(dedup_node_engine::io::ReadFailure::Cancelled)
                }
            }
        })
    }
}

/// 基础计算媒体许可的可控测试行为。
#[derive(Clone)]
enum MediaPermitBehavior {
    /// 等待共享媒体槽并返回可观察许可。
    Scheduled,
    /// 返回 None，验证没有真实媒体许可时仍经过 ready 归并边界。
    None,
    /// 在 Worker 派发前返回普通文件级 I/O 错误。
    Fail,
    /// 永久等待，供取消路径验证 JoinSet 会中止并析构 future。
    Never {
        /// future 已进入永久等待点的通知。
        entered: Arc<Notify>,
        /// future 被中止并析构的次数。
        dropped: Arc<AtomicUsize>,
    },
}

/// 测试读取器同时提供立即 Hash 和独立媒体许可，模拟 Task 4 的两段读取边界。
#[derive(Clone)]
struct MediaPermitReader {
    /// 每个测试路径对应的固定 MD5。
    hashes: Arc<BTreeMap<PathBuf, [u8; 16]>>,
    /// 每个路径的媒体许可结果。
    behaviors: Arc<BTreeMap<PathBuf, MediaPermitBehavior>>,
    /// 所有媒体请求共享的并发槽位。
    media_slots: Arc<Semaphore>,
    /// 当前由 Worker 活动项持有的媒体许可数。
    active_media: Arc<AtomicUsize>,
}

/// 同一关联类型承载 Hash 空许可和媒体计数许可。
enum MediaTestLease {
    /// Hash 读取只需保持 trait 契约，不计入媒体占用。
    Hash,
    /// 媒体许可同时持有共享槽位并记录活动数。
    Media {
        /// Drop 时自动归还的共享媒体槽位。
        _slot: OwnedSemaphorePermit,
        /// Drop 时递减的媒体活动计数。
        active: Arc<AtomicUsize>,
    },
}

impl Drop for MediaTestLease {
    fn drop(&mut self) {
        if let Self::Media { active, .. } = self {
            active.fetch_sub(1, Ordering::AcqRel);
        }
    }
}

/// 永久等待媒体 future 的析构观察器。
struct PendingAcquireDrop(Arc<AtomicUsize>);

impl Drop for PendingAcquireDrop {
    fn drop(&mut self) {
        self.0.fetch_add(1, Ordering::AcqRel);
    }
}

impl PipelineFileReader for MediaPermitReader {
    type Lease = MediaTestLease;

    fn read(
        &self,
        scanned: ScannedPath,
        _cancellation: ReadCancellationToken,
    ) -> Pin<
        Box<
            dyn Future<
                    Output = Result<ReadProduct<Self::Lease>, dedup_node_engine::io::ReadFailure>,
                > + Send
                + 'static,
        >,
    > {
        let md5 = self.hashes[scanned.display_path.as_path()];
        Box::pin(async move {
            Ok(ReadProduct {
                md5,
                lease: MediaTestLease::Hash,
            })
        })
    }

    fn acquire_media_permit(
        &self,
        scanned: ScannedPath,
        cancellation: ReadCancellationToken,
    ) -> Pin<
        Box<
            dyn Future<Output = Result<Option<Self::Lease>, dedup_node_engine::io::ReadFailure>>
                + Send
                + 'static,
        >,
    > {
        let path = scanned.display_path.as_path().to_path_buf();
        let behavior = self.behaviors[&path].clone();
        let slots = Arc::clone(&self.media_slots);
        let active = Arc::clone(&self.active_media);
        Box::pin(async move {
            match behavior {
                MediaPermitBehavior::Scheduled => {
                    let slot = slots.acquire_owned().await.map_err(|_| {
                        dedup_node_engine::io::ReadFailure::Io {
                            path: path.clone(),
                            block_offset: 0,
                            source: io::Error::other("测试媒体许可调度器已关闭"),
                        }
                    })?;
                    if cancellation.is_cancelled() {
                        return Err(dedup_node_engine::io::ReadFailure::Cancelled);
                    }
                    active.fetch_add(1, Ordering::AcqRel);
                    Ok(Some(MediaTestLease::Media {
                        _slot: slot,
                        active,
                    }))
                }
                MediaPermitBehavior::None => Ok(None),
                MediaPermitBehavior::Fail => Err(dedup_node_engine::io::ReadFailure::Io {
                    path,
                    block_offset: 0,
                    source: io::Error::other("测试媒体许可失败"),
                }),
                MediaPermitBehavior::Never { entered, dropped } => {
                    let _drop_spy = PendingAcquireDrop(dropped);
                    entered.notify_one();
                    std::future::pending().await
                }
            }
        })
    }
}

/// 为固定清单装配独立 Hash/媒体测试读取器及其可观察状态。
fn media_reader_for(
    rows: &[ScannedPath],
    behaviors: Vec<MediaPermitBehavior>,
    media_capacity: usize,
) -> (
    MediaPermitReader,
    Arc<BTreeMap<PathBuf, [u8; 16]>>,
    Arc<AtomicUsize>,
) {
    assert_eq!(rows.len(), behaviors.len());
    let hashes = Arc::new(
        rows.iter()
            .enumerate()
            .map(|(index, row)| {
                (
                    row.display_path.as_path().to_path_buf(),
                    [(index + 1) as u8; 16],
                )
            })
            .collect::<BTreeMap<_, _>>(),
    );
    let behaviors = Arc::new(
        rows.iter()
            .zip(behaviors)
            .map(|(row, behavior)| (row.display_path.as_path().to_path_buf(), behavior))
            .collect::<BTreeMap<_, _>>(),
    );
    let active_media = Arc::new(AtomicUsize::new(0));
    (
        MediaPermitReader {
            hashes: Arc::clone(&hashes),
            behaviors,
            media_slots: Arc::new(Semaphore::new(media_capacity)),
            active_media: Arc::clone(&active_media),
        },
        hashes,
        active_media,
    )
}

/// 验证协调器归并后所有精确阶段与 Task10 credit/control current 均已清零。
fn assert_exact_phase_currents_are_zero(metrics: &proto::RuntimePipelineMetrics) {
    for (name, metric) in [
        ("hash_waiting_permit", metrics.hash_waiting_permit.as_ref()),
        ("hash_reading", metrics.hash_reading.as_ref()),
        (
            "hash_completed_unjoined",
            metrics.hash_completed_unjoined.as_ref(),
        ),
        (
            "media_permit_waiting",
            metrics.media_permit_waiting.as_ref(),
        ),
        ("media_acquire_ready", metrics.media_acquire_ready.as_ref()),
        ("media_permit_ready", metrics.media_permit_ready.as_ref()),
        ("worker_dispatching", metrics.worker_dispatching.as_ref()),
        (
            "worker_start_pending",
            metrics.worker_start_pending.as_ref(),
        ),
        ("worker_decode", metrics.worker_decode.as_ref()),
        ("worker_feature", metrics.worker_feature.as_ref()),
        ("worker_result_wait", metrics.worker_result_wait.as_ref()),
        (
            "worker_phase_unknown",
            metrics.worker_phase_unknown.as_ref(),
        ),
    ] {
        assert_eq!(
            metric.map(|value| value.current),
            Some(Some(0)),
            "{name} 必须归零"
        );
    }
    assert_eq!(
        metrics
            .content_output_credit_owned
            .as_ref()
            .map(|value| value.current),
        Some(Some(0)),
        "Task10 终态必须归还全部 content output credit"
    );
    assert_eq!(
        metrics
            .hash_refill_token_available
            .as_ref()
            .map(|value| value.current),
        Some(Some(0)),
        "Task10 终态必须清空 refill token"
    );
    assert_eq!(
        metrics
            .decode_credit_owned
            .as_ref()
            .map(|value| value.current),
        Some(Some(0)),
        "Task11 终态必须归还全部 decode credit"
    );
}

/// 按本次读取配置启用 runtime pipeline，供逐盘许可测试读取同一份真实快照。
fn configure_disk_metrics(reporter: &RuntimeTaskReporter, config: &DiskReadConfig) {
    reporter
        .configure_pipeline_nowait(RuntimeExecutionConfigUpdate {
            hash_tasks: config.total_threads as u32,
            path_cache_queue_capacity: 1,
            content_cache_queue_capacity: 1,
            decode_queue_capacity: 1,
            persist_queue_capacity: 1,
            worker_slots: 1,
            cpu_budget: 1,
            global_disk_permits: config.total_threads as u32,
            hdd_per_disk_permits: config.hdd_threads_per_disk as u32,
            ssd_per_disk_permits: config.ssd_threads_per_disk as u32,
            unknown_per_disk_permits: config.unknown_threads_per_disk as u32,
        })
        .unwrap();
}

/// 读取当前任务按稳定物理盘 ID 排序的许可生命周期快照。
async fn disk_read_metrics(
    registry: &RuntimeTaskRegistry,
    reporter: &RuntimeTaskReporter,
) -> Vec<proto::RuntimeDiskReadMetrics> {
    registry
        .details(reporter.id())
        .await
        .unwrap()
        .pipeline_metrics
        .unwrap()
        .disk_reads
}

/// 同盘 Hash/Media 竞争时从真实运行时指标读取的活动计数。
#[cfg(feature = "test-hooks")]
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
struct SchedulerActiveCounts {
    /// 全局 Hash 许可数。
    global_hash: u64,
    /// 全局 Media 许可数。
    global_media: u64,
    /// 指定物理盘 Hash 许可数。
    disk_hash: u64,
    /// 指定物理盘 Media 许可数。
    disk_media: u64,
}

#[cfg(feature = "test-hooks")]
impl SchedulerActiveCounts {
    /// 返回真实 scheduler 的全局活动许可总数。
    const fn global_active(self) -> u64 {
        self.global_hash + self.global_media
    }

    /// 返回指定物理盘的活动许可总数。
    const fn disk_active(self) -> u64 {
        self.disk_hash + self.disk_media
    }
}

/// 读取指定物理盘及全局资源的同一份运行时快照。
#[cfg(feature = "test-hooks")]
async fn scheduler_active_counts(
    registry: &RuntimeTaskRegistry,
    reporter: &RuntimeTaskReporter,
    physical_disk_id: &str,
) -> SchedulerActiveCounts {
    let details = registry.details(reporter.id()).await.unwrap();
    let metrics = details.pipeline_metrics.unwrap();
    let disk = metrics
        .disk_reads
        .iter()
        .find(|disk| disk.physical_disk_id == physical_disk_id)
        .expect("真实 scheduler 应发布冻结物理盘指标");
    SchedulerActiveCounts {
        global_hash: metrics.hash_io.and_then(|value| value.current).unwrap(),
        global_media: metrics.media_io.and_then(|value| value.current).unwrap(),
        disk_hash: disk.hash_active.unwrap(),
        disk_media: disk.media_active.unwrap(),
    }
}

/// 持有文件数据库的 IMMEDIATE 写事务，精确阻塞 NodeStore 后续写入。
struct SqliteWriteGate {
    /// 独立 SQLite 连接；事务释放前保持写锁所有权。
    connection: Option<rusqlite::Connection>,
}

impl SqliteWriteGate {
    /// 在引擎完成前置领取后取得 SQLite 写锁。
    fn acquire(database: &Path) -> Self {
        let connection = rusqlite::Connection::open(database).unwrap();
        connection.busy_timeout(Duration::from_secs(5)).unwrap();
        connection.execute_batch("BEGIN IMMEDIATE").unwrap();
        Self {
            connection: Some(connection),
        }
    }

    /// 正常提交空事务并释放写锁。
    fn release(mut self) {
        self.connection
            .as_ref()
            .unwrap()
            .execute_batch("COMMIT")
            .unwrap();
        self.connection.take();
    }

    /// 在同一写锁事务内取消任务后提交，使随后解锁的晚到持久化只能被门禁忽略。
    fn cancel_task_and_release(mut self, task_id: &str, now_ms: i64) {
        let connection = self.connection.as_ref().unwrap();
        let changed = connection
            .execute(
                "UPDATE task_items SET status='cancelled'
                 WHERE task_id=?1 AND status IN ('queued','running')",
                [task_id],
            )
            .unwrap();
        connection
            .execute(
                "UPDATE tasks SET status='cancelled',cancelled=cancelled+?2,
                   event_seq=event_seq+1,updated_at_ms=?3
                 WHERE task_id=?1 AND status IN ('queued','running')",
                rusqlite::params![task_id, changed as i64, now_ms],
            )
            .unwrap();
        connection.execute_batch("COMMIT").unwrap();
        self.connection.take();
    }
}

impl Drop for SqliteWriteGate {
    fn drop(&mut self) {
        if let Some(connection) = self.connection.take() {
            let _ = connection.execute_batch("ROLLBACK");
        }
    }
}

/// SQLite 仍锁定时观察到的补位资源和数据库状态。
#[derive(Debug)]
struct LockedPersistObservation {
    /// WorkerPool 是否已经把唯一槽位补给下一文件。
    available_slots: usize,
    /// 下一 Worker 正在持有的 CPU 权重。
    cpu_in_use: usize,
    /// 下一 Worker 正在持有的媒体读取许可数。
    active_media: usize,
    /// 首项在写锁内是否仍保持 running。
    first_still_running: bool,
    /// 写锁内任务成功计数。
    succeeded: u64,
    /// 写锁内已经出现的 Worker 崩溃故障数。
    fault_count: usize,
}

/// 在最终 outbox 发布边界阻塞，供测试观察 Worker 终态后的媒体许可生命周期。
struct GatedOutboxCache {
    /// outbox 已进入远端发布边界的通知。
    entered: Arc<Notify>,
    /// 允许 outbox ACK 返回的通知。
    release: Arc<Notify>,
}

impl RemoteFeatureCache for GatedOutboxCache {
    async fn lookup_paths(
        &self,
        _machine_id: &MachineId,
        paths: &[ScannedPath],
    ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
        Ok(vec![None; paths.len()])
    }

    async fn lookup_contents(
        &self,
        keys: &[ContentKey],
    ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
        Ok(vec![None; keys.len()])
    }

    async fn publish_outbox(
        &mut self,
        _machine_id: &MachineId,
        batch: &dedup_protocol::proto::SyncChangeBatch,
    ) -> Result<u64, RemoteCacheError> {
        self.entered.notify_one();
        self.release.notified().await;
        Ok(batch.high_seq)
    }
}

/// 在第一次内容缓存查询暂停，供测试观察 Hash 与 Worker 资源。
struct GatedContentCache {
    /// 内容查询已经进入远端等待点。
    entered: Arc<Notify>,
    /// 允许第一次内容查询继续。
    release: Arc<Notify>,
    /// 已执行的内容查询次数。
    calls: Arc<AtomicUsize>,
}

impl RemoteFeatureCache for GatedContentCache {
    async fn lookup_paths(
        &self,
        _machine_id: &MachineId,
        paths: &[ScannedPath],
    ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
        Ok(vec![None; paths.len()])
    }

    async fn lookup_contents(
        &self,
        keys: &[ContentKey],
    ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
        if self.calls.fetch_add(1, Ordering::SeqCst) == 0 {
            self.entered.notify_one();
            self.release.notified().await;
        }
        Ok(vec![None; keys.len()])
    }

    async fn publish_outbox(
        &mut self,
        _machine_id: &MachineId,
        batch: &dedup_protocol::proto::SyncChangeBatch,
    ) -> Result<u64, RemoteCacheError> {
        Ok(batch.high_seq)
    }
}

/// 首次内容查询立即 miss，后续查询统一停在 gate，供测试同时观察既有 Worker 与 Hash 背压。
struct GatedAfterFirstContentCache {
    /// 首个被阻塞的后续内容查询已经进入远端边界。
    entered: Arc<Notify>,
    /// 测试结束时统一释放所有被阻塞的内容查询。
    release: Arc<Notify>,
    /// 内容查询调用序号；第一调用保留给既有 Worker 作业。
    calls: Arc<AtomicUsize>,
    /// 第二批 gate 实际拥有的 content context 数。
    gated_items: Arc<AtomicUsize>,
}

impl RemoteFeatureCache for GatedAfterFirstContentCache {
    async fn lookup_paths(
        &self,
        _machine_id: &MachineId,
        paths: &[ScannedPath],
    ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
        Ok(vec![None; paths.len()])
    }

    async fn lookup_contents(
        &self,
        keys: &[ContentKey],
    ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
        let call = self.calls.fetch_add(1, Ordering::SeqCst) + 1;
        if call >= 2 {
            self.gated_items.store(keys.len(), Ordering::SeqCst);
            self.entered.notify_one();
            self.release.notified().await;
        }
        Ok(vec![None; keys.len()])
    }

    async fn publish_outbox(
        &mut self,
        _machine_id: &MachineId,
        batch: &dedup_protocol::proto::SyncChangeBatch,
    ) -> Result<u64, RemoteCacheError> {
        Ok(batch.high_seq)
    }
}

/// 每个内容查询都返回完整 Other 记录的远程缓存。
struct CompleteContentCache;

impl RemoteFeatureCache for CompleteContentCache {
    async fn lookup_paths(
        &self,
        _machine_id: &MachineId,
        paths: &[ScannedPath],
    ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
        Ok(vec![None; paths.len()])
    }

    async fn lookup_contents(
        &self,
        keys: &[ContentKey],
    ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
        Ok(keys
            .iter()
            .map(|key| {
                Some(BaseCacheRecord {
                    content_id: None,
                    content_key: *key,
                    media_kind: MediaKind::Other,
                    base_complete: true,
                    width: None,
                    height: None,
                    duration_ms: None,
                    stage1: None,
                    image_stage2: None,
                    video_stage2: Box::new([None; 6]),
                    contact_sheet_relative_path: None,
                })
            })
            .collect())
    }

    async fn publish_outbox(
        &mut self,
        _machine_id: &MachineId,
        batch: &dedup_protocol::proto::SyncChangeBatch,
    ) -> Result<u64, RemoteCacheError> {
        Ok(batch.high_seq)
    }
}

/// 首次 content 查询返回 miss，后续批次全部返回完整命中，用于观察 cursor 与 Worker 终态交错。
struct FirstMissThenCompleteBatchCache {
    /// 记录每次 content wire 收到的批量大小，确认命中结果确实来自多项批次。
    batch_sizes: Arc<Mutex<Vec<usize>>>,
    /// 可选地阻塞首个 miss 响应，等所有 ready Hash 先进入 cursor。
    first_lookup_release: Option<Arc<Notify>>,
}

impl RemoteFeatureCache for FirstMissThenCompleteBatchCache {
    async fn lookup_paths(
        &self,
        _machine_id: &MachineId,
        paths: &[ScannedPath],
    ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
        Ok(vec![None; paths.len()])
    }

    async fn lookup_contents(
        &self,
        keys: &[ContentKey],
    ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
        let call = {
            let mut batch_sizes = self.batch_sizes.lock().unwrap();
            batch_sizes.push(keys.len());
            batch_sizes.len()
        };
        if call == 1 {
            if let Some(release) = &self.first_lookup_release {
                release.notified().await;
            }
            return Ok(vec![None; keys.len()]);
        }
        Ok(keys
            .iter()
            .map(|key| {
                Some(BaseCacheRecord {
                    content_id: None,
                    content_key: *key,
                    media_kind: MediaKind::Other,
                    base_complete: true,
                    width: None,
                    height: None,
                    duration_ms: None,
                    stage1: None,
                    image_stage2: None,
                    video_stage2: Box::new([None; 6]),
                    contact_sheet_relative_path: None,
                })
            })
            .collect())
    }

    async fn publish_outbox(
        &mut self,
        _machine_id: &MachineId,
        batch: &dedup_protocol::proto::SyncChangeBatch,
    ) -> Result<u64, RemoteCacheError> {
        Ok(batch.high_seq)
    }
}

/// 记录真实 content 批次，并按 MD5 首字节奇偶返回混合 miss/hit。
struct MixedBatchContentCache {
    /// 每次远端调用收到的完整内容键，供测试校验批量与身份。
    batches: Arc<Mutex<Vec<Vec<ContentKey>>>>,
    /// 可选的首批进入通知，用于验证慢 Hash 前立即发送单项。
    entered: Option<Arc<Notify>>,
}

impl RemoteFeatureCache for MixedBatchContentCache {
    async fn lookup_paths(
        &self,
        _machine_id: &MachineId,
        paths: &[ScannedPath],
    ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
        Ok(vec![None; paths.len()])
    }

    async fn lookup_contents(
        &self,
        keys: &[ContentKey],
    ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
        self.batches.lock().unwrap().push(keys.to_vec());
        if let Some(entered) = &self.entered {
            entered.notify_one();
        }
        Ok(keys
            .iter()
            .map(|key| {
                (key.md5()[0] % 2 == 0).then_some(BaseCacheRecord {
                    content_id: None,
                    content_key: *key,
                    media_kind: MediaKind::Other,
                    base_complete: true,
                    width: None,
                    height: None,
                    duration_ms: None,
                    stage1: None,
                    image_stage2: None,
                    video_stage2: Box::new([None; 6]),
                    contact_sheet_relative_path: None,
                })
            })
            .collect())
    }

    async fn publish_outbox(
        &mut self,
        _machine_id: &MachineId,
        batch: &dedup_protocol::proto::SyncChangeBatch,
    ) -> Result<u64, RemoteCacheError> {
        Ok(batch.high_seq)
    }
}

/// 远端 content 边界的可控异常模式。
enum ContentBoundaryMode {
    /// 返回错误长度，验证 resolver 锁定 local-only。
    LengthMismatch,
    /// 永久等待，验证 actor 取消会关闭 endpoint 并丢弃 future。
    NeverCompletes(Arc<Notify>),
}

/// 记录 content 调用和 outbox 发布次数的异常远端缓存。
struct ContentBoundaryCache {
    /// 本测试启用的远端异常模式。
    mode: ContentBoundaryMode,
    /// 实际进入 content 远端边界的次数。
    content_calls: Arc<AtomicUsize>,
    /// 实际进入 outbox 发布边界的次数。
    publish_calls: Arc<AtomicUsize>,
}

/// 首次 content future panic 的远端缓存，用于覆盖 resolver JoinError 降级传播。
struct PanicFirstContentCache {
    /// 实际进入远端 content wire 的调用次数。
    content_calls: Arc<AtomicUsize>,
    /// 首个 future 即将 panic 的通知。
    panic_entered: Arc<Notify>,
    /// outbox 发布调用次数；降级后必须保持零。
    publish_calls: Arc<AtomicUsize>,
}

impl RemoteFeatureCache for PanicFirstContentCache {
    async fn lookup_paths(
        &self,
        _machine_id: &MachineId,
        paths: &[ScannedPath],
    ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
        Ok(vec![None; paths.len()])
    }

    async fn lookup_contents(
        &self,
        keys: &[ContentKey],
    ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
        let call = self.content_calls.fetch_add(1, Ordering::SeqCst);
        if call == 0 {
            self.panic_entered.notify_one();
            panic!("测试 content future panic");
        }
        Ok(vec![None; keys.len()])
    }

    async fn publish_outbox(
        &mut self,
        _machine_id: &MachineId,
        _batch: &dedup_protocol::proto::SyncChangeBatch,
    ) -> Result<u64, RemoteCacheError> {
        self.publish_calls.fetch_add(1, Ordering::SeqCst);
        Ok(0)
    }
}

impl RemoteFeatureCache for ContentBoundaryCache {
    async fn lookup_paths(
        &self,
        _machine_id: &MachineId,
        paths: &[ScannedPath],
    ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
        Ok(vec![None; paths.len()])
    }

    async fn lookup_contents(
        &self,
        _keys: &[ContentKey],
    ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
        self.content_calls.fetch_add(1, Ordering::SeqCst);
        match &self.mode {
            ContentBoundaryMode::LengthMismatch => Ok(Vec::new()),
            ContentBoundaryMode::NeverCompletes(entered) => {
                entered.notify_one();
                std::future::pending().await
            }
        }
    }

    async fn publish_outbox(
        &mut self,
        _machine_id: &MachineId,
        _batch: &dedup_protocol::proto::SyncChangeBatch,
    ) -> Result<u64, RemoteCacheError> {
        self.publish_calls.fetch_add(1, Ordering::SeqCst);
        Ok(0)
    }
}

/// 构造字段完整的视频基础缓存，供缺失掩码行为测试复用。
fn complete_video() -> BaseCacheRecord {
    let frame = ImageStage1 {
        width: 1920,
        height: 1080,
        pdq: PdqHash::from_bytes([7; 32]),
        quality: 91,
    };
    BaseCacheRecord {
        content_id: None,
        content_key: ContentKey::new([3; 16], 100),
        media_kind: MediaKind::Video,
        base_complete: true,
        width: Some(1920),
        height: Some(1080),
        duration_ms: Some(30_000),
        stage1: Some(CompleteStage1::Video(Box::new([Some(frame); 6]))),
        image_stage2: None,
        video_stage2: Box::new([None; 6]),
        contact_sheet_relative_path: None,
    }
}

#[test]
fn new_content_requests_probe_stage1_and_possible_video_contact_sheet() {
    let decision = BaseComputeDecision::for_cache(None, false, false);

    assert_eq!(
        decision.missing_parts(),
        BASE_MISSING_PROBE | BASE_MISSING_STAGE1 | BASE_MISSING_CONTACT_SHEET
    );
}

#[test]
fn complete_video_only_requests_missing_local_contact_sheet() {
    let cached = complete_video();

    assert_eq!(
        BaseComputeDecision::for_cache(Some(&cached), false, false).missing_parts(),
        BASE_MISSING_CONTACT_SHEET
    );
    assert_eq!(
        BaseComputeDecision::for_cache(Some(&cached), true, false).missing_parts(),
        0
    );
}

#[test]
fn force_recompute_requests_every_base_part_even_on_cache_hit() {
    let cached = complete_video();
    let decision = BaseComputeDecision::for_cache(Some(&cached), true, true);

    assert_eq!(
        decision.missing_parts(),
        BASE_MISSING_PROBE | BASE_MISSING_STAGE1 | BASE_MISSING_CONTACT_SHEET
    );
}

#[test]
fn pipeline_limits_reject_zero_and_values_above_the_product_cap() {
    assert!(std::panic::catch_unwind(|| PipelineLimits::new(0, 1)).is_err());
    assert!(std::panic::catch_unwind(|| PipelineLimits::new(1, 0)).is_err());
    assert!(std::panic::catch_unwind(|| PipelineLimits::new(4_097, 1)).is_err());
    assert!(std::panic::catch_unwind(|| PipelineLimits::new(1, 4_097)).is_err());
}

/// 空输入也必须经历 lookup close 后的最终空 claim，不能把 warmup token 遗留到终态。
#[tokio::test]
async fn empty_input_closes_then_claims_once_before_finalizing_refill() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0xE1; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let (reader, _, started_hashes, active_leases) = counting_reader_for(&[]);
    let (mut pool, mut started_workers, _controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "空输入最终 refill claim")
        .await;
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);

    let summary = tokio::time::timeout(
        Duration::from_secs(1),
        BaseComputeEngine::run_existing(
            &mut store,
            &mut pool,
            DisabledRemoteFeatureCache,
            false,
            task_id,
            options,
            Vec::new(),
            &cache_root.join("contact-sheets"),
            reader,
            PipelineLimits::new(1, 1),
            &DiskReadConfig::default(),
            ReadCancellationToken::new(),
            &reporter,
            &artifacts,
            &cleaner,
            20,
        ),
    )
    .await
    .expect("空输入最终 claim 不得忙等或阻塞")
    .unwrap();

    assert_eq!(summary.total_files, 0);
    assert_eq!(started_hashes.load(Ordering::Acquire), 0);
    assert_eq!(active_leases.load(Ordering::Acquire), 0);
    assert!(started_workers.try_recv().is_err(), "空输入不得派发 Worker");
    let details = registry.details(reporter.id()).await.unwrap();
    let metrics = details.pipeline_metrics.unwrap();
    assert_eq!(
        metrics
            .hash_refill_token_available
            .as_ref()
            .and_then(|value| value.current),
        Some(0),
        "lookup close 后的最终空 claim 必须清空 field25 token"
    );
    assert_exact_phase_currents_are_zero(&metrics);
}

#[tokio::test]
async fn scheduled_reader_limits_hashing_by_read_threads_not_single_worker() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    let media_root = install_root.join("media");
    std::fs::create_dir_all(&cache_root).unwrap();
    std::fs::create_dir_all(&media_root).unwrap();
    let machine = MachineId::from_sha256([0x5A; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = (1..=4)
        .map(|index| {
            let path = media_root.join(format!("scheduled-{index}.bin"));
            std::fs::write(&path, vec![index as u8; 10]).unwrap();
            ScannedPath::new(
                NormalizedPath::new(&path).unwrap(),
                DisplayPath::new(&path).unwrap(),
                10,
            )
        })
        .collect::<Vec<_>>();
    let started_hashes = Arc::new(AtomicUsize::new(0));
    let gate = Arc::new((Mutex::new(false), Condvar::new()));
    let config = DiskReadConfig {
        total_threads: 3,
        hdd_threads_per_disk: 3,
        ssd_threads_per_disk: 3,
        unknown_threads_per_disk: 3,
        ..DiskReadConfig::default()
    };
    let (reader, limits) = ScheduledFileReader::controlled_for_test(
        &config,
        1,
        BlockingBlockReader {
            started: Arc::clone(&started_hashes),
            gate: Arc::clone(&gate),
        },
        |_| (vec![7], LocalDiskKind::Ssd),
    )
    .unwrap();
    let (mut pool, mut started_workers, controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "读取配置 Hash 上限")
        .await;
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let remote = DisabledRemoteFeatureCache;
    let task_text = task_id.as_uuid().to_string();
    let contact_root = cache_root.join("contact-sheets");
    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        remote,
        false,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        limits,
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    );
    let drive = async {
        tokio::time::timeout(Duration::from_secs(1), async {
            while started_hashes.load(Ordering::Acquire) < 3 {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("读取配置应允许三个 Hash 同时开始");
        tokio::time::sleep(Duration::from_millis(30)).await;
        assert_eq!(started_hashes.load(Ordering::Acquire), 3);
        let (released, changed) = &*gate;
        *released.lock().unwrap() = true;
        changed.notify_all();
        for _ in 0..4 {
            let item_id = started_workers.recv().await.unwrap().1;
            let identity = controller
                .running_files()
                .into_iter()
                .find(|(_, running, _)| running == &item_id)
                .unwrap()
                .2;
            let byte = identity
                .display_path
                .as_path()
                .file_stem()
                .unwrap()
                .to_string_lossy()
                .chars()
                .last()
                .unwrap()
                .to_digit(10)
                .unwrap() as u8;
            controller
                .complete_base(
                    task_text.clone(),
                    item_id,
                    md5_bytes(&vec![byte; 10]),
                    other_output(),
                )
                .await;
        }
    };
    let (summary, ()) = tokio::join!(run, drive);
    assert_eq!(summary.unwrap().hashed, 4);
}

/// 统一流的同盘 Hash/Media 竞争必须遵守冻结上限，并由 SourceReadComplete 释放 Media 许可。
#[cfg(feature = "test-hooks")]
#[tokio::test]
async fn scheduled_same_disk_hash_media_competition_respects_frozen_limits() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    let media_root = install_root.join("media");
    std::fs::create_dir_all(&cache_root).unwrap();
    std::fs::create_dir_all(&media_root).unwrap();
    let machine = MachineId::from_sha256([0x5B; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = [
        ("media-first.bin", vec![0x91; 10]),
        ("hash-1.bin", vec![1; 10]),
    ]
    .into_iter()
    .map(|(name, bytes)| {
        let path = media_root.join(name);
        std::fs::write(&path, &bytes).unwrap();
        ScannedPath::new(
            NormalizedPath::new(&path).unwrap(),
            DisplayPath::new(&path).unwrap(),
            bytes.len() as u64,
        )
    })
    .collect::<Vec<_>>();
    let first_md5 = [0x91; 16];
    let first_path = rows[0].normalized_path.clone();
    store
        .upsert_content_and_location(&rows[0], first_md5, MediaKind::Other)
        .unwrap();
    assert!(
        store.lookup_base_cache_by_paths(&rows).unwrap()[0].is_some(),
        "首条路径必须先形成已知 MD5 的 partial cache"
    );

    let started_hashes = Arc::new(AtomicUsize::new(0));
    let hash_gate = Arc::new((Mutex::new(false), Condvar::new()));
    let hash_gate_release = BlockingReadRelease(Arc::clone(&hash_gate));
    let config = DiskReadConfig {
        total_threads: 2,
        hdd_threads_per_disk: 2,
        ssd_threads_per_disk: 2,
        unknown_threads_per_disk: 2,
        ..DiskReadConfig::default()
    };
    let (mut pool, mut started_workers, controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "同盘 Hash Media 上限")
        .await;
    configure_disk_metrics(&reporter, &config);
    let media_lane = TaskDiskLane {
        physical_disk_id: PhysicalDiskId::from_disk_numbers([7]).unwrap(),
        physical_disk_numbers: vec![7],
        disk_kind: LocalDiskKind::Ssd,
        configured_weight: 2,
        per_disk_limit: 2,
    };
    let hash_lane = TaskDiskLane {
        physical_disk_id: PhysicalDiskId::from_disk_numbers([7, 8]).unwrap(),
        physical_disk_numbers: vec![7, 8],
        disk_kind: LocalDiskKind::Ssd,
        configured_weight: 2,
        per_disk_limit: 2,
    };
    let planned = rows
        .iter()
        .cloned()
        .enumerate()
        .map(|(index, scanned)| PlannedScannedPath {
            scanned,
            lane: if index == 0 {
                media_lane.clone()
            } else {
                hash_lane.clone()
            },
        })
        .collect::<Vec<_>>();
    let (reader, limits) = ScheduledFileReader::controlled_with_planned_rows_for_test(
        &config,
        1,
        BlockingBlockReader {
            started: Arc::clone(&started_hashes),
            gate: Arc::clone(&hash_gate),
        },
        Arc::new(planned.clone()),
    )
    .unwrap();
    let reader = reader.with_runtime_reporter(reporter.clone());
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let task_text = task_id.as_uuid().to_string();
    let contact_root = cache_root.join("contact-sheets");

    let run = run_scheduled_task_file_scan_for_test(
        store,
        &mut pool,
        task_id,
        vec![NormalizedPath::new(&media_root).unwrap()],
        planned,
        install_root.join("task-runtime"),
        contact_root,
        reader,
        limits,
        config.clone(),
        1,
        ReadCancellationToken::new(),
        &reporter,
        Arc::clone(&artifacts),
        cleaner.clone(),
        20,
    );
    let drive = async {
        let first_item = tokio::time::timeout(Duration::from_secs(1), started_workers.recv())
            .await
            .expect("已知 MD5 的 Media 项应先进入 Worker")
            .unwrap()
            .1;
        assert_eq!(
            controller.running_files()[0].2.normalized_path,
            first_path,
            "Worker Started 身份必须对应首条 Media TSV 行"
        );
        tokio::time::timeout(Duration::from_secs(1), async {
            while started_hashes.load(Ordering::Acquire) != 1 {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("Worker Started 后同盘 Hash 应取得另一个真实 scheduler 许可");
        let counts_before_source_complete =
            scheduler_active_counts(&registry, &reporter, "PhysicalDisk7").await;

        controller
            .base_source_read_complete(task_text.clone(), first_item.clone())
            .await;
        let counts_after_source_complete = tokio::time::timeout(Duration::from_secs(1), async {
            loop {
                let counts = scheduler_active_counts(&registry, &reporter, "PhysicalDisk7").await;
                if started_hashes.load(Ordering::Acquire) == 1
                    && counts.global_hash == 1
                    && counts.global_media == 0
                    && counts.disk_hash == 1
                    && counts.disk_media == 0
                {
                    return counts;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("匹配 SourceReadComplete 后应释放 Media 许可并补入第二个 Hash");
        controller
            .complete_base(task_text.clone(), first_item, first_md5, other_output())
            .await;
        hash_gate_release.release();

        for _ in 0..1 {
            let item_id = tokio::time::timeout(Duration::from_secs(1), started_workers.recv())
                .await
                .expect("两个 Hash miss 都应继续进入同一受控 Worker 槽位")
                .unwrap()
                .1;
            let identity = controller
                .running_files()
                .into_iter()
                .find(|(_, running, _)| running == &item_id)
                .expect("Worker 运行项必须保留 Hash TSV 路径身份")
                .2;
            let byte = identity
                .display_path
                .as_path()
                .file_stem()
                .unwrap()
                .to_string_lossy()
                .chars()
                .last()
                .unwrap()
                .to_digit(10)
                .unwrap() as u8;
            controller
                .base_source_read_complete(task_text.clone(), item_id.clone())
                .await;
            controller
                .complete_base(
                    task_text.clone(),
                    item_id,
                    md5_bytes(&vec![byte; 10]),
                    other_output(),
                )
                .await;
        }
        (counts_before_source_complete, counts_after_source_complete)
    };
    let (summary, (counts_before_source_complete, counts_after_source_complete)) =
        tokio::join!(run, drive);
    let summary = summary.unwrap();

    assert_eq!(
        counts_before_source_complete,
        SchedulerActiveCounts {
            global_hash: 1,
            global_media: 1,
            disk_hash: 1,
            disk_media: 1,
        },
        "Worker Started 后 Media 许可必须仍计入同盘和全局上限"
    );
    assert_eq!(counts_before_source_complete.global_active(), 2);
    assert_eq!(counts_before_source_complete.disk_active(), 2);
    assert_eq!(counts_after_source_complete.global_active(), 1);
    assert_eq!(counts_after_source_complete.disk_active(), 1);
    assert_eq!(started_hashes.load(Ordering::Acquire), 1);
    assert_eq!(summary.resolved_files, 2);
    assert_eq!(summary.cache_hits, 0);
    assert_eq!(summary.file_failures, 0);

    let details = registry.details(reporter.id()).await.unwrap();
    let execution = details.execution_config.unwrap();
    assert_eq!(execution.global_disk_permits, Some(2));
    assert_eq!(execution.ssd_per_disk_permits, Some(2));
    let metrics = details.pipeline_metrics.unwrap();
    let disk = metrics
        .disk_reads
        .iter()
        .find(|disk| disk.physical_disk_id == "PhysicalDisk7")
        .cloned()
        .unwrap();
    assert_eq!(disk.capacity, Some(2));
    assert_eq!(disk.hash_granted_total, Some(1));
    assert_eq!(disk.hash_released_total, Some(1));
    assert_eq!(disk.media_granted_total, Some(2));
    assert_eq!(disk.media_released_total, Some(2));
    assert_eq!(metrics.hash_io.unwrap().current, Some(0));
    assert_eq!(metrics.media_io.unwrap().current, Some(0));
    assert_eq!(disk.hash_active, Some(0));
    assert_eq!(disk.media_active, Some(0));
}

#[tokio::test]
async fn scheduled_reader_location_failure_never_creates_partial_disk_metrics() {
    let fixture = tempdir().unwrap();
    let path = fixture.path().join("location-failure.bin");
    std::fs::write(&path, [0x11]).unwrap();
    let row = ScannedPath::new(
        NormalizedPath::new(&path).unwrap(),
        DisplayPath::new(&path).unwrap(),
        1,
    );
    let config = DiskReadConfig::default();
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(
            RuntimeTaskKind::BaseCompute,
            MachineId::from_sha256([0xD7; 32]),
            "位置解析失败不留逐盘状态",
        )
        .await;
    configure_disk_metrics(&reporter, &config);
    let (reader, _) =
        ScheduledFileReader::controlled_for_test(&config, 1, FixedBlockReader, |_| {
            (Vec::new(), LocalDiskKind::Hdd)
        })
        .unwrap();

    let result = reader
        .with_runtime_reporter(reporter.clone())
        .read(row, ReadCancellationToken::new())
        .await;

    assert!(result.is_err());
    assert!(disk_read_metrics(&registry, &reporter).await.is_empty());
}

#[tokio::test]
async fn dropping_pending_scheduled_read_cancels_waiting_without_grant() {
    let fixture = tempdir().unwrap();
    let first_path = fixture.path().join("active.bin");
    let waiting_path = fixture.path().join("waiting.bin");
    std::fs::write(&first_path, [0x21]).unwrap();
    std::fs::write(&waiting_path, [0x22]).unwrap();
    let rows = test_rows(fixture.path(), &["active.bin", "waiting.bin"]);
    let config = DiskReadConfig {
        total_threads: 1,
        hdd_threads_per_disk: 1,
        ..DiskReadConfig::default()
    };
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(
            RuntimeTaskKind::BaseCompute,
            MachineId::from_sha256([0xD8; 32]),
            "等待中取消逐盘许可",
        )
        .await;
    configure_disk_metrics(&reporter, &config);
    let (reader, _) =
        ScheduledFileReader::controlled_for_test(&config, 1, FixedBlockReader, |_| {
            (vec![7], LocalDiskKind::Hdd)
        })
        .unwrap();
    let reader = reader.with_runtime_reporter(reporter.clone());
    let active = reader
        .read(rows[0].clone(), ReadCancellationToken::new())
        .await
        .unwrap();
    let mut waiting = reader.read(rows[1].clone(), ReadCancellationToken::new());
    assert!(
        tokio::time::timeout(Duration::from_millis(30), waiting.as_mut())
            .await
            .is_err()
    );
    let held = disk_read_metrics(&registry, &reporter).await;
    assert_eq!(
        (
            held[0].hash_waiting,
            held[0].hash_active,
            held[0].hash_granted_total,
            held[0].hash_released_total,
        ),
        (Some(1), Some(1), Some(1), Some(0))
    );

    drop(waiting);
    drop(active.lease);
    let released = disk_read_metrics(&registry, &reporter).await;
    assert_eq!(
        (
            released[0].hash_waiting,
            released[0].hash_active,
            released[0].hash_granted_total,
            released[0].hash_released_total,
        ),
        (Some(0), Some(0), Some(1), Some(1))
    );
}

#[cfg(feature = "test-hooks")]
#[tokio::test]
async fn closed_scheduler_cancels_waiting_without_grant_or_release() {
    let fixture = tempdir().unwrap();
    let path = fixture.path().join("closed-scheduler.bin");
    std::fs::write(&path, [0x31]).unwrap();
    let row = test_rows(fixture.path(), &["closed-scheduler.bin"]).remove(0);
    let config = DiskReadConfig::default();
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(
            RuntimeTaskKind::BaseCompute,
            MachineId::from_sha256([0xD9; 32]),
            "调度器关闭取消逐盘等待",
        )
        .await;
    configure_disk_metrics(&reporter, &config);
    let (reader, _) =
        ScheduledFileReader::controlled_for_test(&config, 1, FixedBlockReader, |_| {
            (vec![8], LocalDiskKind::Ssd)
        })
        .unwrap();
    let reader = reader.with_runtime_reporter(reporter.clone());
    reader.shutdown_scheduler_for_test().await.unwrap();

    assert!(
        reader
            .read(row, ReadCancellationToken::new())
            .await
            .is_err()
    );
    let metrics = disk_read_metrics(&registry, &reporter).await;
    assert_eq!(metrics.len(), 1);
    assert_eq!(
        metrics[0].capacity,
        Some(config.ssd_threads_per_disk as u64)
    );
    assert_eq!(
        (
            metrics[0].hash_waiting,
            metrics[0].hash_active,
            metrics[0].hash_granted_total,
            metrics[0].hash_released_total,
        ),
        (Some(0), Some(0), Some(0), Some(0))
    );
}

#[tokio::test]
async fn scheduled_hash_read_failure_releases_the_acquired_disk_permit() {
    let fixture = tempdir().unwrap();
    let path = fixture.path().join("read-failure.bin");
    std::fs::write(&path, [0x41]).unwrap();
    let row = test_rows(fixture.path(), &["read-failure.bin"]).remove(0);
    let config = DiskReadConfig::default();
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(
            RuntimeTaskKind::BaseCompute,
            MachineId::from_sha256([0xDA; 32]),
            "读取失败释放逐盘许可",
        )
        .await;
    configure_disk_metrics(&reporter, &config);
    let (reader, _) = ScheduledFileReader::controlled_for_test(&config, 1, NeverRead, |_| {
        (vec![9], LocalDiskKind::Unknown)
    })
    .unwrap();

    assert!(
        reader
            .with_runtime_reporter(reporter.clone())
            .read(row, ReadCancellationToken::new())
            .await
            .is_err()
    );
    let metrics = disk_read_metrics(&registry, &reporter).await;
    assert_eq!(
        (
            metrics[0].hash_waiting,
            metrics[0].hash_active,
            metrics[0].hash_granted_total,
            metrics[0].hash_released_total,
        ),
        (Some(0), Some(0), Some(1), Some(1))
    );
}

#[tokio::test]
async fn scheduled_hash_permit_reports_active_until_its_normal_drop() {
    let fixture = tempdir().unwrap();
    let path = fixture.path().join("normal-drop.bin");
    std::fs::write(&path, [0x51]).unwrap();
    let row = test_rows(fixture.path(), &["normal-drop.bin"]).remove(0);
    let config = DiskReadConfig::default();
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(
            RuntimeTaskKind::BaseCompute,
            MachineId::from_sha256([0xDB; 32]),
            "正常 Drop 释放逐盘许可",
        )
        .await;
    configure_disk_metrics(&reporter, &config);
    let (reader, _) =
        ScheduledFileReader::controlled_for_test(&config, 1, FixedBlockReader, |_| {
            (vec![10], LocalDiskKind::Hdd)
        })
        .unwrap();
    let product = reader
        .with_runtime_reporter(reporter.clone())
        .read(row, ReadCancellationToken::new())
        .await
        .unwrap();
    let held = disk_read_metrics(&registry, &reporter).await;
    assert_eq!(
        (
            held[0].hash_waiting,
            held[0].hash_active,
            held[0].hash_granted_total,
            held[0].hash_released_total,
        ),
        (Some(0), Some(1), Some(1), Some(0))
    );

    drop(product.lease);
    let released = disk_read_metrics(&registry, &reporter).await;
    assert_eq!(
        (
            released[0].hash_waiting,
            released[0].hash_active,
            released[0].hash_granted_total,
            released[0].hash_released_total,
        ),
        (Some(0), Some(0), Some(1), Some(1))
    );
}

#[tokio::test]
async fn composite_media_permit_updates_each_sorted_unique_underlying_disk() {
    let fixture = tempdir().unwrap();
    let row = test_rows(fixture.path(), &["composite.mp4"]).remove(0);
    let config = DiskReadConfig {
        unknown_threads_per_disk: 2,
        ..DiskReadConfig::default()
    };
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(
            RuntimeTaskKind::BaseCompute,
            MachineId::from_sha256([0xDC; 32]),
            "复合盘媒体许可",
        )
        .await;
    configure_disk_metrics(&reporter, &config);
    let (reader, _) =
        ScheduledFileReader::controlled_for_test(&config, 1, FixedBlockReader, |_| {
            (vec![9, 5, 9], LocalDiskKind::Unknown)
        })
        .unwrap();
    let permit = reader
        .with_runtime_reporter(reporter.clone())
        .acquire_media_permit(row, ReadCancellationToken::new())
        .await
        .unwrap()
        .unwrap();
    let held = disk_read_metrics(&registry, &reporter).await;
    assert_eq!(
        held.iter()
            .map(|metric| (
                metric.physical_disk_id.as_str(),
                metric.capacity,
                metric.media_waiting,
                metric.media_active,
                metric.media_granted_total,
                metric.media_released_total,
            ))
            .collect::<Vec<_>>(),
        vec![
            ("PhysicalDisk5", Some(2), Some(0), Some(1), Some(1), Some(0)),
            ("PhysicalDisk9", Some(2), Some(0), Some(1), Some(1), Some(0)),
        ]
    );

    drop(permit);
    let released = disk_read_metrics(&registry, &reporter).await;
    assert!(released.iter().all(|metric| {
        metric.media_waiting == Some(0)
            && metric.media_active == Some(0)
            && metric.media_granted_total == Some(1)
            && metric.media_released_total == Some(1)
    }));
}

#[tokio::test]
async fn acquired_telemetry_rejection_cancels_every_composite_waiting_entry_atomically() {
    let fixture = tempdir().unwrap();
    let first_path = fixture.path().join("held.bin");
    let rejected_path = fixture.path().join("rejected.bin");
    std::fs::write(&first_path, [0x61]).unwrap();
    std::fs::write(&rejected_path, [0x62]).unwrap();
    let rows = test_rows(fixture.path(), &["held.bin", "rejected.bin"]);
    let high_config = DiskReadConfig {
        total_threads: 2,
        unknown_threads_per_disk: 2,
        ..DiskReadConfig::default()
    };
    let low_config = DiskReadConfig {
        total_threads: 1,
        unknown_threads_per_disk: 1,
        ..DiskReadConfig::default()
    };
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(
            RuntimeTaskKind::BaseCompute,
            MachineId::from_sha256([0xDD; 32]),
            "复合盘遥测转换失败保持原子",
        )
        .await;
    configure_disk_metrics(&reporter, &high_config);
    let (held_reader, _) =
        ScheduledFileReader::controlled_for_test(&high_config, 1, FixedBlockReader, |_| {
            (vec![21], LocalDiskKind::Unknown)
        })
        .unwrap();
    let held = held_reader
        .with_runtime_reporter(reporter.clone())
        .read(rows[0].clone(), ReadCancellationToken::new())
        .await
        .unwrap();
    let (rejected_reader, _) =
        ScheduledFileReader::controlled_for_test(&low_config, 1, FixedBlockReader, |_| {
            (vec![21, 22], LocalDiskKind::Unknown)
        })
        .unwrap();

    let rejected = rejected_reader
        .with_runtime_reporter(reporter.clone())
        .read(rows[1].clone(), ReadCancellationToken::new())
        .await;
    assert!(rejected.is_err(), "active 已满时 acquired 遥测必须拒绝转换");
    let metrics = disk_read_metrics(&registry, &reporter).await;
    assert_eq!(metrics.len(), 2);
    assert_eq!(
        (
            metrics[0].physical_disk_id.as_str(),
            metrics[0].capacity,
            metrics[0].hash_waiting,
            metrics[0].hash_active,
            metrics[0].hash_granted_total,
            metrics[0].hash_released_total,
        ),
        (
            "PhysicalDisk21",
            Some(1),
            Some(0),
            Some(1),
            Some(1),
            Some(0)
        )
    );
    assert_eq!(
        (
            metrics[1].physical_disk_id.as_str(),
            metrics[1].capacity,
            metrics[1].hash_waiting,
            metrics[1].hash_active,
            metrics[1].hash_granted_total,
            metrics[1].hash_released_total,
        ),
        (
            "PhysicalDisk22",
            Some(1),
            Some(0),
            Some(0),
            Some(0),
            Some(0)
        )
    );

    drop(held.lease);
    let released = disk_read_metrics(&registry, &reporter).await;
    assert_eq!(released[0].hash_active, Some(0));
    assert_eq!(released[0].hash_released_total, Some(1));
}

#[tokio::test]
async fn hash_refill_starts_at_most_one_task_per_select_boundary() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x4A; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = (1_u8..=6)
        .map(|index| {
            let path = media_root.join(format!("file-{index}.bin"));
            (
                ScannedPath::new(
                    NormalizedPath::new(&path).unwrap(),
                    DisplayPath::new(&path).unwrap(),
                    10,
                ),
                (path, [index; 16]),
            )
        })
        .collect::<Vec<_>>();
    let hashes = Arc::new(rows.iter().map(|(_, entry)| entry.clone()).collect());
    let rows = rows.into_iter().map(|(row, _)| row).collect::<Vec<_>>();
    let started_hashes = Arc::new(AtomicUsize::new(0));
    let active_leases = Arc::new(AtomicUsize::new(0));
    let reader = CountingHashReader {
        hashes: Arc::clone(&hashes),
        started: Arc::clone(&started_hashes),
        active_leases: Arc::clone(&active_leases),
    };
    let limits = PipelineLimits::new(2, 2);
    let config = DiskReadConfig::default();
    let (mut pool, mut started_workers, controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "独立 Hash 有界窗口")
        .await;
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let contact_root = cache_root.join("contact-sheets");
    let remote = DisabledRemoteFeatureCache;
    let task_text = task_id.as_uuid().to_string();

    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        remote,
        false,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        limits,
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    );
    let drive = async {
        let first = tokio::time::timeout(Duration::from_secs(1), started_workers.recv())
            .await
            .expect("第一个 Node Hash 应派发一次性 Worker")
            .unwrap()
            .1;
        tokio::time::timeout(Duration::from_secs(1), async {
            while started_hashes.load(Ordering::Acquire) < 2 {
                tokio::task::yield_now().await;
            }
        })
        .await
        .unwrap_or_else(|_| {
            panic!(
                "Worker admission 未释放前 Hash 应保持有界: actual={}",
                started_hashes.load(Ordering::Acquire)
            )
        });
        assert_eq!(active_leases.load(Ordering::Acquire), 0);
        tokio::time::sleep(Duration::from_millis(30)).await;
        assert!(
            started_hashes.load(Ordering::Acquire) <= 3,
            "单个真实 MediaRequested departure 最多补出一个 Hash: actual={}",
            started_hashes.load(Ordering::Acquire)
        );

        let mut item_id = first;
        for completed in 0..6 {
            let identity = controller
                .running_files()
                .into_iter()
                .find(|(_, running, _)| running == &item_id)
                .expect("运行项应保留冻结路径")
                .2;
            let md5 = hashes[identity.display_path.as_path()];
            controller
                .complete_base(task_text.clone(), item_id.clone(), md5, other_output())
                .await;
            if completed < 5 {
                item_id = tokio::time::timeout(Duration::from_secs(1), started_workers.recv())
                    .await
                    .expect("Worker 终态后应从有界 Hash 队列继续补位")
                    .unwrap()
                    .1;
            }
        }
    };

    let (summary, ()) = tokio::join!(run, drive);
    let summary = summary.unwrap();
    assert_eq!(summary.hashed, 6);
    assert_eq!(summary.scheduled_stage1, 6);
    assert_eq!(active_leases.load(Ordering::Acquire), 0);
}

#[tokio::test]
async fn hash_refill_does_not_consume_token_without_output_credit() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x4B; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = (1..=2)
        .map(|index| {
            let path = media_root.join(format!("gate-{index}.bin"));
            ScannedPath::new(
                NormalizedPath::new(&path).unwrap(),
                DisplayPath::new(&path).unwrap(),
                10,
            )
        })
        .collect::<Vec<_>>();
    let (reader, hashes, started_hashes, active_leases) = counting_reader_for(&rows);
    let config = DiskReadConfig::default();
    let (mut pool, mut started_workers, controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "缓存等待不占资源")
        .await;
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let entered = Arc::new(Notify::new());
    let release = Arc::new(Notify::new());
    let remote = GatedContentCache {
        entered: Arc::clone(&entered),
        release: Arc::clone(&release),
        calls: Arc::new(AtomicUsize::new(0)),
    };
    let task_text = task_id.as_uuid().to_string();
    let contact_root = cache_root.join("contact-sheets");

    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        remote,
        true,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        PipelineLimits::new(2, 2),
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    );
    let drive = async {
        tokio::time::timeout(Duration::from_secs(1), entered.notified())
            .await
            .expect("内容缓存查询应进入远端闸门");
        tokio::time::timeout(Duration::from_secs(1), async {
            while started_hashes.load(Ordering::Acquire) < 2 {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("远端等待期间另一 Hash 应继续完成");
        assert_eq!(active_leases.load(Ordering::Acquire), 0);
        assert_eq!(controller.available_slots(), 1);
        assert!(
            tokio::time::timeout(Duration::from_millis(30), started_workers.recv())
                .await
                .is_err(),
            "缓存判定前不得占用 Worker"
        );
        release.notify_one();
        for _ in 0..2 {
            let item_id = tokio::time::timeout(Duration::from_secs(1), started_workers.recv())
                .await
                .expect("缓存缺失后应派发一次性 Worker")
                .unwrap()
                .1;
            let md5 = running_md5(&controller, &item_id, &hashes);
            controller
                .complete_base(task_text.clone(), item_id, md5, other_output())
                .await;
        }
    };

    let (summary, ()) = tokio::join!(run, drive);
    assert_eq!(summary.unwrap().scheduled_stage1, 2);
    assert_eq!(active_leases.load(Ordering::Acquire), 0);
}

#[tokio::test]
async fn multiple_ready_hashes_do_not_refill_the_whole_window() {
    // Task10 令牌只随真实 content departure 补位；两 Worker 槽各 departure 一次后达到 4。
    const EXPECTED_STARTED_HASHES: usize = 4;

    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x64; 32]);
    let database = install_root.join("data/node/node.sqlite3");
    let mut store = NodeStore::open(&database, machine.clone()).unwrap();
    let observer = NodeStore::open(&database, machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = (0..9)
        .map(|index| {
            let path = media_root.join(format!("content-gate-{index}.bin"));
            ScannedPath::new(
                NormalizedPath::new(&path).unwrap(),
                DisplayPath::new(&path).unwrap(),
                10,
            )
        })
        .collect::<Vec<_>>();
    let hashes = Arc::new(
        rows.iter()
            .enumerate()
            .map(|(index, row)| {
                (
                    row.display_path.as_path().to_path_buf(),
                    [(index + 1) as u8; 16],
                )
            })
            .collect::<BTreeMap<_, _>>(),
    );
    let started_hashes = Arc::new(AtomicUsize::new(0));
    let active_leases = Arc::new(AtomicUsize::new(0));
    let reader = ScriptedHashReader {
        behaviors: Arc::new(
            rows.iter()
                .enumerate()
                .map(|(index, row)| {
                    (
                        row.display_path.as_path().to_path_buf(),
                        ScriptedHashBehavior::SuccessAfterAllStarted {
                            md5: [(index + 1) as u8; 16],
                            expected: 2,
                        },
                    )
                })
                .collect(),
        ),
        started: Arc::clone(&started_hashes),
        active_leases: Arc::clone(&active_leases),
    };
    let (mut pool, mut started_workers, controller) = WorkerPool::controlled_batch_for_test(2);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "content 等待事件归并")
        .await;
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let entered = Arc::new(Notify::new());
    let release = Arc::new(Notify::new());
    let gated_items = Arc::new(AtomicUsize::new(0));
    let remote_calls = Arc::new(AtomicUsize::new(0));
    let remote = GatedAfterFirstContentCache {
        entered: Arc::clone(&entered),
        release: Arc::clone(&release),
        calls: Arc::clone(&remote_calls),
        gated_items: Arc::clone(&gated_items),
    };
    let cancellation = ReadCancellationToken::new();
    let task_text = task_id.as_uuid().to_string();
    let config = DiskReadConfig::default();
    let contact_root = cache_root.join("contact-sheets");

    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        remote,
        true,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        PipelineLimits::new(2, 2),
        &config,
        cancellation.clone(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    );
    let observe = async {
        if tokio::time::timeout(Duration::from_secs(2), entered.notified())
            .await
            .is_err()
        {
            panic!(
                "第二个内容查询应进入 gate: calls={}, started_hashes={}, worker_slots={}",
                remote_calls.load(Ordering::SeqCst),
                started_hashes.load(Ordering::Acquire),
                controller.available_slots()
            );
        }
        let item_id = started_workers
            .recv()
            .await
            .expect("首个 content miss 应已有 active Worker")
            .1;
        let _held_item = started_workers
            .recv()
            .await
            .expect("第二个 content miss 应保持另一个 active Worker")
            .1;
        let md5 = running_md5(&controller, &item_id, &hashes);
        controller
            .complete_base(task_text, item_id.clone(), md5, other_output())
            .await;
        tokio::time::timeout(Duration::from_secs(1), async {
            loop {
                let succeeded = observer
                    .task_items(task_id)
                    .unwrap()
                    .into_iter()
                    .any(|item| {
                        item.item_id == item_id && item.status == TaskItemStatus::Succeeded
                    });
                if succeeded {
                    break;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("content gate 期间既有 Worker 终态应立即写入 SQLite");
        let hash_fill = tokio::time::timeout(Duration::from_secs(1), async {
            while started_hashes.load(Ordering::Acquire) < EXPECTED_STARTED_HASHES {
                tokio::task::yield_now().await;
            }
        })
        .await;
        assert!(
            hash_fill.is_ok(),
            "content gate 期间后续 Hash 应填满精确有界 ownership: expected={EXPECTED_STARTED_HASHES}, actual={}",
            started_hashes.load(Ordering::Acquire)
        );
        tokio::time::sleep(Duration::from_millis(30)).await;
        let remote_active_items = gated_items.load(Ordering::SeqCst);
        assert!(
            (1..=2).contains(&remote_active_items),
            "单连接 wire 只能观察 C 的 active 子集，且不得超过 C=2: actual={remote_active_items}"
        );
        assert_eq!(
            controller.available_slots(),
            1,
            "两槽池中 A 应精确保留一个 Worker"
        );
        assert_eq!(
            started_hashes.load(Ordering::Acquire),
            EXPECTED_STARTED_HASHES,
            "初始两 token + 两次真实媒体 departure = 4，应阻止第五项启动"
        );
        assert_eq!(active_leases.load(Ordering::Acquire), 0);
        cancellation.cancel();
        release.notify_waiters();
    };

    let (result, ()) = tokio::join!(run, observe);
    assert!(matches!(
        result,
        Err(dedup_node_engine::scan::ScanError::Cancelled)
    ));
}

#[tokio::test]
async fn ready_content_misses_are_batched_and_mixed_hits_keep_exact_identity() {
    const FILE_COUNT: usize = 8;

    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x67; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = (0..FILE_COUNT)
        .map(|index| {
            let path = media_root.join(format!("mixed-batch-{index}.bin"));
            ScannedPath::new(
                NormalizedPath::new(&path).unwrap(),
                DisplayPath::new(&path).unwrap(),
                10,
            )
        })
        .collect::<Vec<_>>();
    let started_hashes = Arc::new(AtomicUsize::new(0));
    let active_leases = Arc::new(AtomicUsize::new(0));
    let hashes = Arc::new(
        rows.iter()
            .enumerate()
            .map(|(index, row)| {
                (
                    row.display_path.as_path().to_path_buf(),
                    [(index + 1) as u8; 16],
                )
            })
            .collect::<BTreeMap<_, _>>(),
    );
    let reader = ScriptedHashReader {
        behaviors: Arc::new(
            rows.iter()
                .enumerate()
                .map(|(index, row)| {
                    (
                        row.display_path.as_path().to_path_buf(),
                        ScriptedHashBehavior::SuccessAfterAllStarted {
                            md5: [(index + 1) as u8; 16],
                            expected: FILE_COUNT,
                        },
                    )
                })
                .collect(),
        ),
        started: Arc::clone(&started_hashes),
        active_leases: Arc::clone(&active_leases),
    };
    let (mut pool, mut started_workers, controller) = WorkerPool::controlled_batch_for_test(2);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "content 真批查与混合身份")
        .await;
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let batches = Arc::new(Mutex::new(Vec::<Vec<ContentKey>>::new()));
    let task_text = task_id.as_uuid().to_string();
    let config = DiskReadConfig::default();
    let contact_root = cache_root.join("contact-sheets");

    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        MixedBatchContentCache {
            batches: Arc::clone(&batches),
            entered: None,
        },
        true,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        PipelineLimits::new(FILE_COUNT, FILE_COUNT),
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    );
    let drive = async {
        let mut worker_md5 = Vec::new();
        for _ in 0..FILE_COUNT / 2 {
            let item_id = tokio::time::timeout(Duration::from_secs(1), started_workers.recv())
                .await
                .expect("远端 miss 应进入 Worker")
                .expect("Worker 启动通道不应关闭")
                .1;
            let md5 = running_md5(&controller, &item_id, &hashes);
            worker_md5.push(md5[0]);
            controller
                .complete_base(task_text.clone(), item_id, md5, other_output())
                .await;
        }
        worker_md5.sort_unstable();
        worker_md5
    };

    let (summary, worker_md5) = tokio::join!(run, drive);
    let summary = summary.unwrap();
    assert_eq!(summary.hashed, FILE_COUNT);
    assert_eq!(summary.reused_contents, FILE_COUNT / 2);
    assert_eq!(summary.scheduled_stage1, FILE_COUNT / 2);
    assert_eq!(worker_md5, vec![1, 3, 5, 7], "只有远端 miss 可进入 Worker");
    assert_eq!(started_hashes.load(Ordering::Acquire), FILE_COUNT);
    assert_eq!(active_leases.load(Ordering::Acquire), 0);
    assert!(
        store
            .task_items(task_id)
            .unwrap()
            .iter()
            .all(|item| item.status == TaskItemStatus::Succeeded),
        "混合 hit/miss 必须按 item 身份全部完成"
    );
    let batches = batches.lock().unwrap();
    assert!(
        batches.iter().any(|keys| keys.len() > 1),
        "多个 ready Hash 必须进入同一次 lookup_contents(keys)"
    );
    assert!(
        batches.len() <= FILE_COUNT / 2,
        "真批查调用数必须显著少于文件数: calls={}, files={FILE_COUNT}",
        batches.len()
    );
    let mut queried = batches
        .iter()
        .flat_map(|keys| keys.iter().map(|key| key.md5()[0]))
        .collect::<Vec<_>>();
    queried.sort_unstable();
    assert_eq!(queried, (1..=FILE_COUNT as u8).collect::<Vec<_>>());
}

/// 兼容旧 cursor 门禁：Hash 窗口足够大时，首个 miss 后的完整命中仍必须是一个批次。
#[tokio::test(flavor = "current_thread")]
async fn current_thread_content_cursor_preserves_singleton_then_full_ready_batch() {
    const HIT_COUNT: usize = 32;
    const FILE_COUNT: usize = HIT_COUNT + 1;
    const WORKER_MD5: [u8; 16] = [0x31; 16];

    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x6A; 32]);
    let database_path = install_root.join("node.sqlite3");
    let mut store = NodeStore::open(&database_path, machine.clone()).unwrap();
    let observer = store.reopen().unwrap();
    let media_root = install_root.join("media");
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = (0..FILE_COUNT)
        .map(|index| {
            let path = media_root.join(format!("cursor-batch-{index}.bin"));
            ScannedPath::new(
                NormalizedPath::new(&path).unwrap(),
                DisplayPath::new(&path).unwrap(),
                10,
            )
        })
        .collect::<Vec<_>>();
    let release_hits = Arc::new(Notify::new());
    let release_first_lookup = Arc::new(Notify::new());
    let hit_hash_entered = Arc::new(Notify::new());
    let started_hashes = Arc::new(AtomicUsize::new(0));
    let active_leases = Arc::new(AtomicUsize::new(0));
    let reader = ScriptedHashReader {
        behaviors: Arc::new(
            rows.iter()
                .enumerate()
                .map(|(index, row)| {
                    let behavior = if index == 0 {
                        ScriptedHashBehavior::Success(WORKER_MD5)
                    } else {
                        ScriptedHashBehavior::SuccessAfterRelease {
                            entered: Arc::clone(&hit_hash_entered),
                            release: Arc::clone(&release_hits),
                            md5: [(index + 1) as u8; 16],
                        }
                    };
                    (row.display_path.as_path().to_path_buf(), behavior)
                })
                .collect(),
        ),
        started: Arc::clone(&started_hashes),
        active_leases: Arc::clone(&active_leases),
    };
    let (mut pool, mut started_workers, controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "cursor full ready batch")
        .await;
    let runtime_id = reporter.id().to_owned();
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let batch_sizes = Arc::new(Mutex::new(Vec::<usize>::new()));
    let task_text = task_id.as_uuid().to_string();
    let config = DiskReadConfig::default();
    let contact_root = cache_root.join("contact-sheets");

    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        FirstMissThenCompleteBatchCache {
            batch_sizes: Arc::clone(&batch_sizes),
            first_lookup_release: Some(Arc::clone(&release_first_lookup)),
        },
        true,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        PipelineLimits::new(FILE_COUNT * 2, FILE_COUNT),
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    );
    let drive = async {
        tokio::time::timeout(Duration::from_secs(1), async {
            while started_hashes.load(Ordering::Acquire) < FILE_COUNT {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("所有命中 Hash 应已进入 gate");
        release_hits.notify_waiters();
        tokio::time::timeout(Duration::from_secs(1), async {
            while active_leases.load(Ordering::Acquire) != 0 {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("释放 gate 后所有 Hash 读取许可应先归还");
        tokio::time::timeout(Duration::from_secs(1), async {
            while batch_sizes.lock().unwrap().len() != 1 {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("首个 content miss 请求应先进入可阻塞 resolver");
        release_first_lookup.notify_one();
        let worker_item = tokio::time::timeout(Duration::from_secs(1), started_workers.recv())
            .await
            .expect("首个 content miss 应进入 active Worker")
            .expect("Worker 启动通道不应关闭")
            .1;

        let hits_when_terminal_sent = tokio::time::timeout(Duration::from_secs(1), async {
            loop {
                let hits = observer
                    .task_items(task_id)
                    .unwrap()
                    .iter()
                    .filter(|item| {
                        item.item_id != worker_item && item.status == TaskItemStatus::Succeeded
                    })
                    .count();
                if hits > 0 {
                    break hits;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("至少一个完整 content hit 应先写入 SQLite");
        controller
            .complete_base(task_text, worker_item.clone(), WORKER_MD5, other_output())
            .await;
        let hits_when_worker_persisted = tokio::time::timeout(Duration::from_secs(1), async {
            loop {
                let items = observer.task_items(task_id).unwrap();
                let worker_persisted = items.iter().any(|item| {
                    item.item_id == worker_item && item.status == TaskItemStatus::Succeeded
                });
                if worker_persisted {
                    break items
                        .iter()
                        .filter(|item| {
                            item.item_id != worker_item && item.status == TaskItemStatus::Succeeded
                        })
                        .count();
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("Worker 终态应及时落入 SQLite");
        (hits_when_terminal_sent, hits_when_worker_persisted)
    };

    let (summary, observed_hits) = tokio::join!(run, drive);
    let summary = summary.unwrap();
    assert_eq!(summary.scheduled_stage1, 1);
    assert_eq!(summary.reused_contents, HIT_COUNT);
    {
        let batch_sizes = batch_sizes.lock().unwrap();
        assert_eq!(
            batch_sizes.as_slice(),
            &[1, HIT_COUNT],
            "旧 cursor 门禁必须保留首项 miss 与完整命中批次"
        );
    }
    assert!((1..HIT_COUNT).contains(&observed_hits.0));
    assert!((1..HIT_COUNT).contains(&observed_hits.1));
    assert_eq!(started_hashes.load(Ordering::Acquire), FILE_COUNT);
    assert_eq!(active_leases.load(Ordering::Acquire), 0);
    let details = registry.details(&runtime_id).await.unwrap();
    assert_exact_phase_currents_are_zero(&details.pipeline_metrics.unwrap());
}

#[tokio::test(flavor = "current_thread")]
async fn all_cache_hits_continue_past_the_initial_hash_window() {
    const HIT_COUNT: usize = 64;
    const FILE_COUNT: usize = HIT_COUNT + 1;
    const WORKER_MD5: [u8; 16] = [0x31; 16];

    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x6A; 32]);
    let database_path = install_root.join("node.sqlite3");
    let mut store = NodeStore::open(&database_path, machine.clone()).unwrap();
    // 独立 SQLite 连接只观察已提交终态，不绕过生产 actor 的单写者边界。
    let observer = store.reopen().unwrap();
    let media_root = install_root.join("media");
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = (0..FILE_COUNT)
        .map(|index| {
            let path = media_root.join(format!("cursor-yield-{index}.bin"));
            ScannedPath::new(
                NormalizedPath::new(&path).unwrap(),
                DisplayPath::new(&path).unwrap(),
                10,
            )
        })
        .collect::<Vec<_>>();
    let release_hits = Arc::new(AtomicBool::new(false));
    let hit_hash_entered = Arc::new(Notify::new());
    let started_hashes = Arc::new(AtomicUsize::new(0));
    let active_leases = Arc::new(AtomicUsize::new(0));
    let reader = ScriptedHashReader {
        behaviors: Arc::new(
            rows.iter()
                .enumerate()
                .map(|(index, row)| {
                    let behavior = if index == 0 {
                        ScriptedHashBehavior::Success(WORKER_MD5)
                    } else {
                        ScriptedHashBehavior::SuccessAfterFlag {
                            entered: Arc::clone(&hit_hash_entered),
                            release: Arc::clone(&release_hits),
                            md5: [(index + 1) as u8; 16],
                        }
                    };
                    (row.display_path.as_path().to_path_buf(), behavior)
                })
                .collect(),
        ),
        started: Arc::clone(&started_hashes),
        active_leases: Arc::clone(&active_leases),
    };
    let (mut pool, mut started_workers, controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(
            RuntimeTaskKind::Scan,
            machine,
            "current-thread cursor cooperative yield",
        )
        .await;
    let runtime_id = reporter.id().to_owned();
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let batch_sizes = Arc::new(Mutex::new(Vec::<usize>::new()));
    let task_text = task_id.as_uuid().to_string();
    let config = DiskReadConfig::default();
    let contact_root = cache_root.join("contact-sheets");

    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        FirstMissThenCompleteBatchCache {
            batch_sizes: Arc::clone(&batch_sizes),
            first_lookup_release: None,
        },
        true,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        PipelineLimits::new(FILE_COUNT, 8),
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    );
    let drive = async {
        let worker_item = tokio::time::timeout(Duration::from_secs(1), started_workers.recv())
            .await
            .expect("首个 content miss 应进入 active Worker")
            .expect("Worker 启动通道不应关闭")
            .1;
        release_hits.store(true, Ordering::Release);
        tokio::time::timeout(Duration::from_secs(2), async {
            while started_hashes.load(Ordering::Acquire) < FILE_COUNT {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("所有命中 Hash 应越过容量为 8 的初始 Hash 窗口并完成读取");

        let hits_when_terminal_sent = tokio::time::timeout(Duration::from_secs(1), async {
            loop {
                let hits = observer
                    .task_items(task_id)
                    .unwrap()
                    .iter()
                    .filter(|item| {
                        item.item_id != worker_item && item.status == TaskItemStatus::Succeeded
                    })
                    .count();
                if hits > 0 {
                    break hits;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("至少一个完整 content hit 应先写入 SQLite");
        controller
            .complete_base(task_text, worker_item.clone(), WORKER_MD5, other_output())
            .await;
        let hits_when_worker_persisted = tokio::time::timeout(Duration::from_secs(1), async {
            loop {
                let items = observer.task_items(task_id).unwrap();
                let worker_persisted = items.iter().any(|item| {
                    item.item_id == worker_item && item.status == TaskItemStatus::Succeeded
                });
                if worker_persisted {
                    break items
                        .iter()
                        .filter(|item| {
                            item.item_id != worker_item && item.status == TaskItemStatus::Succeeded
                        })
                        .count();
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("Worker 终态应及时落入 SQLite");
        (hits_when_terminal_sent, hits_when_worker_persisted)
    };

    let (summary, observed_hits) = tokio::join!(run, drive);
    let summary = summary.unwrap();
    assert_eq!(summary.scheduled_stage1, 1);
    assert_eq!(summary.reused_contents, HIT_COUNT);
    {
        let batch_sizes = batch_sizes.lock().unwrap();
        assert_eq!(batch_sizes.first().copied(), Some(1));
        assert!(
            batch_sizes.len() > 2
                && batch_sizes
                    .iter()
                    .skip(1)
                    .all(|size| (1..=8).contains(size)),
            "容量为 8 时 stable refill 必须继续受 Hash admission 限制: {batch_sizes:?}"
        );
        assert_eq!(
            batch_sizes.iter().sum::<usize>(),
            FILE_COUNT,
            "所有 Hash 结果都必须进入 content 查询"
        );
    }
    assert!(
        observed_hits.0 < HIT_COUNT,
        "Worker producer 获得调度前 cursor 已同步消费完整输入: {}",
        observed_hits.0
    );
    assert!(
        observed_hits.1 < HIT_COUNT,
        "Worker 落库前 cursor 已同步消费完整输入: {}",
        observed_hits.1
    );
    assert_eq!(started_hashes.load(Ordering::Acquire), FILE_COUNT);
    assert_eq!(active_leases.load(Ordering::Acquire), 0);
    let details = registry.details(&runtime_id).await.unwrap();
    assert_exact_phase_currents_are_zero(&details.pipeline_metrics.unwrap());
}

#[tokio::test]
async fn content_batch_sends_single_ready_item_before_slow_hash_finishes() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x68; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let rows = (0..2)
        .map(|index| {
            let path = media_root.join(format!("partial-batch-{index}.bin"));
            ScannedPath::new(
                NormalizedPath::new(&path).unwrap(),
                DisplayPath::new(&path).unwrap(),
                10,
            )
        })
        .collect::<Vec<_>>();
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let slow_entered = Arc::new(Notify::new());
    let release_slow = Arc::new(Notify::new());
    let started_hashes = Arc::new(AtomicUsize::new(0));
    let active_leases = Arc::new(AtomicUsize::new(0));
    let reader = ScriptedHashReader {
        behaviors: Arc::new(BTreeMap::from([
            (
                rows[0].display_path.as_path().to_path_buf(),
                ScriptedHashBehavior::Success([1; 16]),
            ),
            (
                rows[1].display_path.as_path().to_path_buf(),
                ScriptedHashBehavior::SuccessAfterRelease {
                    entered: Arc::clone(&slow_entered),
                    release: Arc::clone(&release_slow),
                    md5: [2; 16],
                },
            ),
        ])),
        started: Arc::clone(&started_hashes),
        active_leases: Arc::clone(&active_leases),
    };
    let (mut pool, mut started_workers, controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "content ready 即发")
        .await;
    let runtime_id = reporter.id().to_owned();
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let first_lookup_entered = Arc::new(Notify::new());
    let batches = Arc::new(Mutex::new(Vec::<Vec<ContentKey>>::new()));
    let task_text = task_id.as_uuid().to_string();
    let config = DiskReadConfig::default();
    let contact_root = cache_root.join("contact-sheets");

    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        MixedBatchContentCache {
            batches: Arc::clone(&batches),
            entered: Some(Arc::clone(&first_lookup_entered)),
        },
        true,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        PipelineLimits::new(2, 2),
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    );
    let drive = async {
        tokio::time::timeout(Duration::from_secs(1), slow_entered.notified())
            .await
            .expect("慢 Hash 应进入 gate");
        let held = tokio::time::timeout(Duration::from_secs(1), async {
            loop {
                let details = registry.details(&runtime_id).await.unwrap();
                let reading = details
                    .pipeline_metrics
                    .as_ref()
                    .and_then(|metrics| metrics.hash_reading.as_ref())
                    .and_then(|metric| metric.current);
                if reading == Some(1) {
                    break details;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("真实 Hash JoinSet future 在 gate 中必须保持 reading");
        assert_eq!(
            held.pipeline_metrics
                .as_ref()
                .unwrap()
                .hash_completed_unjoined
                .as_ref()
                .unwrap()
                .current,
            Some(0),
            "慢 Hash 尚未返回时不得提前计入 completed-unjoined"
        );
        tokio::time::timeout(Duration::from_secs(1), first_lookup_entered.notified())
            .await
            .expect("单个 ready Hash 必须在慢 Hash 完成前发起 content 查询");
        let item_id = started_workers
            .recv()
            .await
            .expect("首个远端 miss 应进入 Worker")
            .1;
        controller
            .complete_base(task_text, item_id, [1; 16], other_output())
            .await;
        release_slow.notify_one();
    };

    let (summary, ()) = tokio::join!(run, drive);
    let summary = summary.unwrap();
    assert_eq!(summary.scheduled_stage1, 1);
    assert_eq!(summary.reused_contents, 1);
    assert_eq!(started_hashes.load(Ordering::Acquire), 2);
    assert_eq!(active_leases.load(Ordering::Acquire), 0);
    let details = registry.details(&runtime_id).await.unwrap();
    assert_exact_phase_currents_are_zero(&details.pipeline_metrics.unwrap());
    let batches = batches.lock().unwrap();
    assert_eq!(batches.first().map(Vec::len), Some(1));
}

#[tokio::test]
async fn content_length_mismatch_locks_local_only_and_later_items_still_complete() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x65; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = (0..3)
        .map(|index| {
            let path = media_root.join(format!("length-mismatch-{index}.bin"));
            ScannedPath::new(
                NormalizedPath::new(&path).unwrap(),
                DisplayPath::new(&path).unwrap(),
                10,
            )
        })
        .collect::<Vec<_>>();
    let (reader, hashes, _, active_leases) = counting_reader_for(&rows);
    let (mut pool, mut started_workers, controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "content 长度异常降级")
        .await;
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let content_calls = Arc::new(AtomicUsize::new(0));
    let publish_calls = Arc::new(AtomicUsize::new(0));
    let remote = ContentBoundaryCache {
        mode: ContentBoundaryMode::LengthMismatch,
        content_calls: Arc::clone(&content_calls),
        publish_calls: Arc::clone(&publish_calls),
    };
    let task_text = task_id.as_uuid().to_string();
    let config = DiskReadConfig::default();
    let contact_root = cache_root.join("contact-sheets");

    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        remote,
        true,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        PipelineLimits::new(2, 2),
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    );
    let drive = async {
        for _ in 0..3 {
            let item_id = tokio::time::timeout(Duration::from_secs(1), started_workers.recv())
                .await
                .expect("local-only 后每个 miss 都应继续派发 Worker")
                .expect("Worker 启动通道不应关闭")
                .1;
            let md5 = running_md5(&controller, &item_id, &hashes);
            controller
                .complete_base(task_text.clone(), item_id, md5, other_output())
                .await;
        }
    };

    let (summary, ()) = tokio::join!(run, drive);
    let summary = summary.unwrap();
    assert_eq!(summary.scheduled_stage1, 3);
    assert_eq!(content_calls.load(Ordering::SeqCst), 1);
    assert_eq!(publish_calls.load(Ordering::SeqCst), 0);
    assert_eq!(active_leases.load(Ordering::Acquire), 0);
    assert_eq!(
        store.task_snapshot(task_id).unwrap().status,
        TaskStatus::Completed
    );
    let failures = registry.details(reporter.id()).await.unwrap().failures;
    assert_eq!(failures.len(), 1, "远端异常只记录一次降级告警");
    assert!(failures[0].message.contains("内容缓存返回数量不匹配"));
}

#[tokio::test]
async fn content_future_panic_warns_once_and_later_hashes_continue_local_only() {
    const FILE_COUNT: usize = 4;

    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x69; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let rows = (0..FILE_COUNT)
        .map(|index| {
            let path = media_root.join(format!("panic-local-only-{index}.bin"));
            ScannedPath::new(
                NormalizedPath::new(&path).unwrap(),
                DisplayPath::new(&path).unwrap(),
                10,
            )
        })
        .collect::<Vec<_>>();
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let started_hashes = Arc::new(AtomicUsize::new(0));
    let active_leases = Arc::new(AtomicUsize::new(0));
    let release_slow = Arc::new(Notify::new());
    let slow_entered = (0..FILE_COUNT - 1)
        .map(|_| Arc::new(Notify::new()))
        .collect::<Vec<_>>();
    let mut behaviors = BTreeMap::from([(
        rows[0].display_path.as_path().to_path_buf(),
        ScriptedHashBehavior::Success([1; 16]),
    )]);
    for index in 1..FILE_COUNT {
        behaviors.insert(
            rows[index].display_path.as_path().to_path_buf(),
            ScriptedHashBehavior::SuccessAfterRelease {
                entered: Arc::clone(&slow_entered[index - 1]),
                release: Arc::clone(&release_slow),
                md5: [(index + 1) as u8; 16],
            },
        );
    }
    let reader = ScriptedHashReader {
        behaviors: Arc::new(behaviors),
        started: Arc::clone(&started_hashes),
        active_leases: Arc::clone(&active_leases),
    };
    let hashes = rows
        .iter()
        .enumerate()
        .map(|(index, row)| {
            (
                row.display_path.as_path().to_path_buf(),
                [(index + 1) as u8; 16],
            )
        })
        .collect::<BTreeMap<_, _>>();
    let (mut pool, mut started_workers, controller) = WorkerPool::controlled_batch_for_test(2);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "content panic 降级")
        .await;
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let content_calls = Arc::new(AtomicUsize::new(0));
    let publish_calls = Arc::new(AtomicUsize::new(0));
    let panic_entered = Arc::new(Notify::new());
    let task_text = task_id.as_uuid().to_string();
    let config = DiskReadConfig::default();
    let contact_root = cache_root.join("contact-sheets");

    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        PanicFirstContentCache {
            content_calls: Arc::clone(&content_calls),
            panic_entered: Arc::clone(&panic_entered),
            publish_calls: Arc::clone(&publish_calls),
        },
        true,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        PipelineLimits::new(FILE_COUNT, FILE_COUNT),
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    );
    let drive = async {
        tokio::time::timeout(Duration::from_secs(1), panic_entered.notified())
            .await
            .expect("首个 content future 应进入 panic 边界");
        for entered in &slow_entered {
            tokio::time::timeout(Duration::from_secs(1), entered.notified())
                .await
                .expect("后续慢 Hash 应在释放前全部进入 gate");
        }
        release_slow.notify_waiters();
        for _ in 0..FILE_COUNT {
            let item_id = tokio::time::timeout(Duration::from_secs(1), started_workers.recv())
                .await
                .expect("local-only miss 应继续进入 Worker")
                .expect("Worker 启动通道不应关闭")
                .1;
            let md5 = running_md5(&controller, &item_id, &hashes);
            controller
                .complete_base(task_text.clone(), item_id, md5, other_output())
                .await;
        }
    };

    let (summary, ()) = tokio::join!(run, drive);
    assert_eq!(summary.unwrap().scheduled_stage1, FILE_COUNT);
    assert_eq!(content_calls.load(Ordering::SeqCst), 1);
    assert_eq!(publish_calls.load(Ordering::SeqCst), 0);
    assert_eq!(started_hashes.load(Ordering::Acquire), FILE_COUNT);
    assert_eq!(active_leases.load(Ordering::Acquire), 0);
    assert_eq!(
        store.task_snapshot(task_id).unwrap().status,
        TaskStatus::Completed
    );
    let failures = registry.details(reporter.id()).await.unwrap().failures;
    assert_eq!(failures.len(), 1, "JoinError 降级告警必须恰好一次");
    assert!(failures[0].message.contains("缓存查询任务异常"));
}

#[tokio::test]
async fn cancellation_closes_never_completing_resolver_without_late_success() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x66; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let path = media_root.join("cancel-gated-content.bin");
    let row = ScannedPath::new(
        NormalizedPath::new(&path).unwrap(),
        DisplayPath::new(&path).unwrap(),
        10,
    );
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let (reader, _, started_hashes, active_leases) =
        counting_reader_for(std::slice::from_ref(&row));
    let (mut pool, mut started_workers, _controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "取消 gated resolver")
        .await;
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let entered = Arc::new(Notify::new());
    let content_calls = Arc::new(AtomicUsize::new(0));
    let publish_calls = Arc::new(AtomicUsize::new(0));
    let remote = ContentBoundaryCache {
        mode: ContentBoundaryMode::NeverCompletes(Arc::clone(&entered)),
        content_calls: Arc::clone(&content_calls),
        publish_calls: Arc::clone(&publish_calls),
    };
    let cancellation = ReadCancellationToken::new();
    let config = DiskReadConfig::default();
    let contact_root = cache_root.join("contact-sheets");

    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        remote,
        true,
        task_id,
        options,
        vec![row],
        &contact_root,
        reader,
        PipelineLimits::new(2, 2),
        &config,
        cancellation.clone(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    );
    let cancel = async {
        tokio::time::timeout(Duration::from_secs(1), entered.notified())
            .await
            .expect("content 查询应进入永久 gate");
        cancellation.cancel();
    };

    let (result, ()) =
        tokio::time::timeout(Duration::from_secs(2), async { tokio::join!(run, cancel) })
            .await
            .expect("actor 取消必须关闭 endpoint，不能等待远端 gate");
    assert!(matches!(
        result,
        Err(dedup_node_engine::scan::ScanError::Cancelled)
    ));
    assert_eq!(content_calls.load(Ordering::SeqCst), 1);
    assert_eq!(publish_calls.load(Ordering::SeqCst), 0);
    assert_eq!(started_hashes.load(Ordering::Acquire), 1);
    assert_eq!(active_leases.load(Ordering::Acquire), 0);
    assert!(started_workers.try_recv().is_err(), "取消后不得派发 Worker");
    assert!(
        store
            .task_items(task_id)
            .unwrap()
            .iter()
            .all(|item| item.status != TaskItemStatus::Succeeded),
        "取消后的迟到缓存结果不得写成功"
    );
}

#[tokio::test]
async fn complete_content_cache_hit_finishes_without_worker_dispatch() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x4C; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let path = install_root.join("media/cache-hit.bin");
    let row = ScannedPath::new(
        NormalizedPath::new(&path).unwrap(),
        DisplayPath::new(&path).unwrap(),
        10,
    );
    let expected_md5 = [0xA5; 16];
    let started_hashes = Arc::new(AtomicUsize::new(0));
    let active_leases = Arc::new(AtomicUsize::new(0));
    let reader = CountingHashReader {
        hashes: Arc::new(BTreeMap::from([(path, expected_md5)])),
        started: Arc::clone(&started_hashes),
        active_leases: Arc::clone(&active_leases),
    };
    let options = ScanOptions::new(vec![DisplayPath::new(install_root.join("media")).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let config = DiskReadConfig::default();
    let (mut pool, mut started_workers, _controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "完整缓存直完成")
        .await;
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let remote = CompleteContentCache;

    let summary = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        remote,
        true,
        task_id,
        options,
        vec![row],
        &cache_root.join("contact-sheets"),
        reader,
        PipelineLimits::new(1, 1),
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    )
    .await
    .unwrap();

    assert_eq!(summary.hashed, 1);
    assert_eq!(summary.reused_contents, 1);
    assert_eq!(summary.scheduled_stage1, 0);
    assert_eq!(started_hashes.load(Ordering::Acquire), 1);
    assert_eq!(active_leases.load(Ordering::Acquire), 0);
    assert!(
        started_workers.try_recv().is_err(),
        "完整内容命中不得派发 Worker"
    );
    assert!(
        store
            .content_id_by_key(ContentKey::new(expected_md5, 10))
            .unwrap()
            .is_some(),
        "Node 生成的 MD5 必须成为持久内容键"
    );
    let metrics = registry
        .details(reporter.id())
        .await
        .unwrap()
        .pipeline_metrics
        .unwrap();
    assert_eq!(
        metrics.decode_credit_owned.unwrap().current,
        Some(0),
        "完整缓存命中不得申请 decode credit"
    );
}

/// 全局排序的 1,001+3 双根输入必须在首个 Hash 窗口同时暴露两块虚拟物理盘。
#[cfg(feature = "test-hooks")]
#[tokio::test(flavor = "current_thread")]
async fn dual_root_hash_window_exposes_both_virtual_disks() {
    const FIRST_ROOT_FILES: usize = 1_001;
    const SECOND_ROOT_FILES: usize = 3;
    const TOTAL_FILES: usize = FIRST_ROOT_FILES + SECOND_ROOT_FILES;

    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0xD5; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let first_root = DisplayPath::new(r"H:\VirtualMedia").unwrap();
    let second_root = DisplayPath::new(r"I:\VirtualMedia").unwrap();
    let options = ScanOptions::new(vec![first_root.clone(), second_root.clone()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let mut rows = (1..=FIRST_ROOT_FILES)
        .map(|index| {
            let path = format!(r"H:\VirtualMedia\h-{index:04}.bin");
            ScannedPath::new(
                NormalizedPath::new(&path).unwrap(),
                DisplayPath::new(&path).unwrap(),
                4_096,
            )
        })
        .collect::<Vec<_>>();
    rows.extend((1..=SECOND_ROOT_FILES).map(|index| {
        let path = format!(r"I:\VirtualMedia\i-{index:04}.bin");
        ScannedPath::new(
            NormalizedPath::new(&path).unwrap(),
            DisplayPath::new(&path).unwrap(),
            4_096,
        )
    }));
    let first_normalized = NormalizedPath::new(first_root.as_path()).unwrap();
    let second_normalized = NormalizedPath::new(second_root.as_path()).unwrap();
    let reader = dual_disk_reader_for(
        first_normalized.clone(),
        second_normalized.clone(),
        &rows,
        2,
    );
    let first_disk = reader.first_disk.clone();
    let second_disk = reader.second_disk.clone();
    let started_paths = Arc::clone(&reader.started_paths);
    let (mut pool, mut started_workers, _controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "双虚拟物理盘可见性")
        .await;
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);

    let summary = run_enumerated_scan_to_base_compute_for_test(
        &mut store,
        &mut pool,
        task_id,
        options,
        rows,
        &cache_root.join("contact-sheets"),
        || async { (CompleteContentCache, true) },
        || async { Ok((reader, PipelineLimits::new(4, 2))) },
        &DiskReadConfig::default(),
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    )
    .await
    .unwrap();

    let started_paths = started_paths.lock().unwrap();
    assert_eq!(started_paths.len(), TOTAL_FILES);
    assert!(
        started_paths
            .iter()
            .take(4)
            .any(|path| path.is_within(&first_normalized))
    );
    assert!(
        started_paths
            .iter()
            .take(4)
            .any(|path| path.is_within(&second_normalized)),
        "首四个成功 Hash 启动必须包含 PhysicalDisk2"
    );
    let first_second_root = started_paths
        .iter()
        .position(|path| path == &NormalizedPath::new(r"I:\VirtualMedia\i-0001.bin").unwrap())
        .unwrap();
    let last_first_root = started_paths
        .iter()
        .position(|path| path == &NormalizedPath::new(r"H:\VirtualMedia\h-1001.bin").unwrap())
        .unwrap();
    assert!(first_second_root < last_first_root);
    assert!((1..=2).contains(&first_disk.peak.load(Ordering::Acquire)));
    assert!((1..=2).contains(&second_disk.peak.load(Ordering::Acquire)));
    assert_eq!(first_disk.active.load(Ordering::Acquire), 0);
    assert_eq!(second_disk.active.load(Ordering::Acquire), 0);
    assert_eq!(summary.total_files, TOTAL_FILES);
    assert_eq!(summary.hashed, TOTAL_FILES);
    assert_eq!(summary.reused_contents, TOTAL_FILES);
    assert_eq!(
        store.task_snapshot(task_id).unwrap().succeeded,
        TOTAL_FILES as u64
    );
    assert!(
        started_workers.try_recv().is_err(),
        "完整缓存命中不得派发 Worker"
    );
    let details = registry.details(reporter.id()).await.unwrap();
    assert_exact_phase_currents_are_zero(&details.pipeline_metrics.unwrap());
}

/// Hash 失败的 output credit 必须随 terminal persist 消息保持到 actor 取得消息。
#[tokio::test]
async fn hash_failure_output_credit_lives_until_persist_actor_boundary() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0xE2; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let row_path = media_root.join("persist-failure.bin");
    let row = ScannedPath::new(
        NormalizedPath::new(&row_path).unwrap(),
        DisplayPath::new(&row_path).unwrap(),
        10,
    );
    let (reader, _, started_hashes, active_leases) = {
        let mut behaviors = BTreeMap::new();
        behaviors.insert(row_path.clone(), ScriptedHashBehavior::Fail);
        let started = Arc::new(AtomicUsize::new(0));
        let active = Arc::new(AtomicUsize::new(0));
        (
            ScriptedHashReader {
                behaviors: Arc::new(behaviors),
                started: Arc::clone(&started),
                active_leases: Arc::clone(&active),
            },
            (),
            started,
            active,
        )
    };
    let (mut pool, mut started_workers, _controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(
            RuntimeTaskKind::Scan,
            machine,
            "Hash failure persist credit",
        )
        .await;
    let runtime_id = reporter.id().to_owned();
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let (persist_control, persist_waiter) = BasePersistTestController::new();
    let contact_root = cache_root.join("contact-sheets");
    let config = DiskReadConfig::default();

    let run = BaseComputeEngine::run_existing_with_first_persist_gate_for_test(
        &mut store,
        &mut pool,
        DisabledRemoteFeatureCache,
        false,
        task_id,
        options,
        vec![row],
        &contact_root,
        reader,
        PipelineLimits::new(1, 1),
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
        persist_waiter,
    );
    let observe = async {
        persist_control.wait_until_entered().await;
        let details = tokio::time::timeout(Duration::from_secs(1), async {
            loop {
                let details = registry.details(&runtime_id).await.unwrap();
                let owned = details
                    .pipeline_metrics
                    .as_ref()
                    .and_then(|metrics| metrics.content_output_credit_owned.as_ref())
                    .and_then(|metric| metric.current);
                if owned == Some(1) {
                    break details;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("terminal persist actor 已取得消息时 credit 必须仍被持有");
        assert_eq!(
            details
                .pipeline_metrics
                .as_ref()
                .unwrap()
                .content_output_credit_owned
                .as_ref()
                .unwrap()
                .current,
            Some(1)
        );
        persist_control.release();
    };

    let (summary, ()) = tokio::join!(run, observe);
    let summary = summary.unwrap();
    assert_eq!(summary.file_failures, 1);
    assert_eq!(started_hashes.load(Ordering::Acquire), 1);
    assert_eq!(active_leases.load(Ordering::Acquire), 0);
    assert!(started_workers.try_recv().is_err());
    let details = registry.details(&runtime_id).await.unwrap();
    assert_exact_phase_currents_are_zero(&details.pipeline_metrics.unwrap());
}

/// 内容缓存命中的 output credit 也必须由 terminal persist 消息持有到 actor 边界。
#[tokio::test]
async fn cache_hit_output_credit_lives_until_persist_actor_boundary() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0xE3; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let path = media_root.join("persist-hit.bin");
    let row = ScannedPath::new(
        NormalizedPath::new(&path).unwrap(),
        DisplayPath::new(&path).unwrap(),
        10,
    );
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let expected_md5 = [1; 16];
    let (reader, _, started_hashes, active_leases) = counting_reader_for(&[row.clone()]);
    let (mut pool, mut started_workers, _controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "Cache hit persist credit")
        .await;
    let runtime_id = reporter.id().to_owned();
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let (persist_control, persist_waiter) = BasePersistTestController::new();
    let contact_root = cache_root.join("contact-sheets");
    let config = DiskReadConfig::default();

    let run = BaseComputeEngine::run_existing_with_first_persist_gate_for_test(
        &mut store,
        &mut pool,
        CompleteContentCache,
        true,
        task_id,
        options,
        vec![row],
        &contact_root,
        reader,
        PipelineLimits::new(1, 1),
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
        persist_waiter,
    );
    let observe = async {
        persist_control.wait_until_entered().await;
        let details = tokio::time::timeout(Duration::from_secs(1), async {
            loop {
                let details = registry.details(&runtime_id).await.unwrap();
                let owned = details
                    .pipeline_metrics
                    .as_ref()
                    .and_then(|metrics| metrics.content_output_credit_owned.as_ref())
                    .and_then(|metric| metric.current);
                if owned == Some(1) {
                    break details;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("cache hit terminal persist actor 已取得消息时 credit 必须仍被持有");
        assert_eq!(
            details
                .pipeline_metrics
                .as_ref()
                .unwrap()
                .content_output_credit_owned
                .as_ref()
                .unwrap()
                .current,
            Some(1)
        );
        persist_control.release();
    };

    let (summary, ()) = tokio::join!(run, observe);
    let summary = summary.unwrap();
    assert_eq!(summary.reused_contents, 1);
    assert_eq!(started_hashes.load(Ordering::Acquire), 1);
    assert_eq!(active_leases.load(Ordering::Acquire), 0);
    assert!(started_workers.try_recv().is_err());
    let details = registry.details(&runtime_id).await.unwrap();
    assert_exact_phase_currents_are_zero(&details.pipeline_metrics.unwrap());
    assert!(
        store
            .content_id_by_key(ContentKey::new(expected_md5, 10))
            .unwrap()
            .is_some()
    );
}

/// capacity=2 时前三项的第三次 claim 必须在无 output credit 下停住，token 不能被偷扣。
#[tokio::test]
async fn no_output_credit_keeps_third_item_unclaimed_with_capacity_two() {
    const FILE_COUNT: usize = 3;

    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0xE4; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = (0..FILE_COUNT)
        .map(|index| {
            let path = media_root.join(format!("no-credit-{index}.bin"));
            ScannedPath::new(
                NormalizedPath::new(&path).unwrap(),
                DisplayPath::new(&path).unwrap(),
                10,
            )
        })
        .collect::<Vec<_>>();
    let (reader, _, started_hashes, active_leases) = counting_reader_for(&rows);
    let (mut pool, mut started_workers, _controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "output credit 无容量")
        .await;
    let runtime_id = reporter.id().to_owned();
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let (persist_control, persist_waiter) = BasePersistTestController::new();
    let contact_root = cache_root.join("contact-sheets");
    let config = DiskReadConfig::default();

    let run = BaseComputeEngine::run_existing_with_first_persist_gate_for_test(
        &mut store,
        &mut pool,
        CompleteContentCache,
        true,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        PipelineLimits::new(2, 2),
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
        persist_waiter,
    );
    let observe = async {
        tokio::time::timeout(Duration::from_secs(1), async {
            while started_hashes.load(Ordering::Acquire) < 2 {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("初始 Hash 窗口必须启动两项");
        persist_control.wait_until_entered().await;
        let token_before = tokio::time::timeout(Duration::from_secs(1), async {
            loop {
                let details = registry.details(&runtime_id).await.unwrap();
                let metrics = details.pipeline_metrics.as_ref().unwrap();
                let owned = metrics
                    .content_output_credit_owned
                    .as_ref()
                    .and_then(|value| value.current);
                let token = metrics
                    .hash_refill_token_available
                    .as_ref()
                    .and_then(|value| value.current);
                if owned == Some(2) && token.is_some_and(|value| value > 0) {
                    break token.unwrap();
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("两项 terminal persist 尚未出队时必须观察到 credit 满且 token 可用");
        tokio::time::sleep(Duration::from_millis(40)).await;
        assert_eq!(
            started_hashes.load(Ordering::Acquire),
            2,
            "第三项不得 claim"
        );
        let details = registry.details(&runtime_id).await.unwrap();
        let metrics = details.pipeline_metrics.as_ref().unwrap();
        assert_eq!(
            metrics
                .content_output_credit_owned
                .as_ref()
                .and_then(|value| value.current),
            Some(2)
        );
        assert_eq!(
            metrics
                .hash_refill_token_available
                .as_ref()
                .and_then(|value| value.current),
            Some(token_before),
            "NoOutputCredit admission 不得消费 refill token"
        );
        persist_control.release();
    };

    let (summary, ()) = tokio::join!(run, observe);
    let summary = summary.unwrap();
    assert_eq!(summary.reused_contents, FILE_COUNT);
    assert_eq!(started_hashes.load(Ordering::Acquire), FILE_COUNT);
    assert_eq!(active_leases.load(Ordering::Acquire), 0);
    assert!(started_workers.try_recv().is_err());
    let details = registry.details(&runtime_id).await.unwrap();
    assert_exact_phase_currents_are_zero(&details.pipeline_metrics.unwrap());
}

/// path 完整缓存命中只能在 SQLite Applied ACK 后推进持久状态和运行时计数。
#[cfg(feature = "test-hooks")]
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn path_complete_cache_hit_waits_for_persist_ack() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let database = install_root.join("node.db");
    let machine = MachineId::from_sha256([0x96; 32]);
    let mut store = NodeStore::open(&database, machine.clone()).unwrap();
    let path = install_root.join("media/path-hit.bin");
    let row = ScannedPath::new(
        NormalizedPath::new(&path).unwrap(),
        DisplayPath::new(&path).unwrap(),
        10,
    );
    let content = store
        .upsert_content_and_location(&row, [0xB6; 16], MediaKind::Other)
        .unwrap();
    store.mark_base_complete(content.id).unwrap();
    let options = ScanOptions::new(vec![DisplayPath::new(install_root.join("media")).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let observer = store.reopen().unwrap();
    let (reader, _, started_hashes, active_leases) = counting_reader_for(&[row.clone()]);
    let (mut pool, mut started_workers, _controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "完整 path 缓存命中")
        .await;
    let runtime_id = reporter.id().to_owned();
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let config = DiskReadConfig::default();
    let (persist_control, persist_waiter) = BasePersistTestController::new();
    let contact_root = cache_root.join("contact-sheets");

    let run = BaseComputeEngine::run_existing_with_first_persist_gate_for_test(
        &mut store,
        &mut pool,
        DisabledRemoteFeatureCache,
        false,
        task_id,
        options,
        vec![row],
        &contact_root,
        reader,
        PipelineLimits::new(1, 1),
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
        persist_waiter,
    );
    let observe_before_ack = async {
        persist_control.wait_until_entered().await;
        let snapshot = observer.task_snapshot(task_id).unwrap();
        assert_eq!(snapshot.status, TaskStatus::Running);
        assert_eq!(snapshot.succeeded, 0, "writer 解锁前不得提交成功");
        let items = observer.task_items(task_id).unwrap();
        assert_eq!(items.len(), 1);
        assert_eq!(
            items[0].status,
            TaskItemStatus::Running,
            "首条持久化事务执行前任务项必须仍为 Running"
        );
        let details = registry.details(&runtime_id).await.unwrap();
        assert_eq!(details.summary.unwrap().overall_completed, 0);
        let compute = details
            .stages
            .iter()
            .find(|stage| stage.stage_id == "compute_base_features")
            .unwrap();
        assert_eq!(
            compute.completed, 0,
            "持久化消息入队不得提前推进 compute reporter"
        );
        assert_eq!(started_hashes.load(Ordering::Acquire), 0);
        assert_eq!(active_leases.load(Ordering::Acquire), 0);
        assert!(
            started_workers.try_recv().is_err(),
            "path 完整缓存命中不得启动 Hash 或 Worker"
        );
        persist_control.release();
        persist_control.release();
    };
    let (summary, ()) = tokio::join!(run, observe_before_ack);
    let summary = summary.unwrap();

    assert_eq!(summary.cache_hits, 1);
    assert_eq!(started_hashes.load(Ordering::Acquire), 0);
    assert_eq!(active_leases.load(Ordering::Acquire), 0);
    assert!(started_workers.try_recv().is_err());
    let snapshot = store.task_snapshot(task_id).unwrap();
    assert_eq!(snapshot.succeeded, 1);
    let details = registry.details(&runtime_id).await.unwrap();
    assert_eq!(details.summary.unwrap().overall_completed, 1);
    let compute = details
        .stages
        .iter()
        .find(|stage| stage.stage_id == "compute_base_features")
        .unwrap();
    assert_eq!(compute.completed, 1);
}

/// 真实 BaseCompute 在 path 上游暂时无项时只空 claim 一次，publish 后恢复一次 item claim。
#[cfg(feature = "test-hooks")]
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn closed_input_empty_claim_stops_future_claims() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0xF6; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let row_path = media_root.join("closed-empty.bin");
    let row = ScannedPath::new(
        NormalizedPath::new(&row_path).unwrap(),
        DisplayPath::new(&row_path).unwrap(),
        10,
    );
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let (reader, hashes, started_hashes, active_leases) = counting_reader_for(&[row.clone()]);
    let (mut pool, mut started_workers, controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "真实 closed-empty claim")
        .await;
    let runtime_id = reporter.id().to_owned();
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let (persist_control, persist_waiter) = BasePersistTestController::new();
    let claim_attempts = Arc::new(AtomicUsize::new(0));
    let _claim_observer =
        BaseComputeEngine::install_claim_observer_for_test(Arc::clone(&claim_attempts));
    let contact_root = cache_root.join("contact-sheets");
    let config = DiskReadConfig::default();
    let task_text = task_id.as_uuid().to_string();

    let run = BaseComputeEngine::run_existing_with_first_persist_gate_for_test(
        &mut store,
        &mut pool,
        DisabledRemoteFeatureCache,
        false,
        task_id,
        options,
        vec![row],
        &contact_root,
        reader,
        PipelineLimits::new(1, 1),
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
        persist_waiter,
    );
    let observe = async {
        tokio::time::timeout(Duration::from_secs(1), async {
            while started_hashes.load(Ordering::Acquire) != 1 {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("真实首条 queued item 必须成功 claim 并进入 Hash");
        assert_eq!(claim_attempts.load(Ordering::Acquire), 1);
        let item_id = tokio::time::timeout(Duration::from_secs(1), started_workers.recv())
            .await
            .expect("Hash 后应进入 Worker")
            .expect("Worker 启动通道不应关闭")
            .1;
        let md5 = running_md5(&controller, &item_id, &hashes);
        controller
            .complete_base(task_text, item_id, md5, other_output())
            .await;
        persist_control.wait_until_entered().await;
        tokio::time::timeout(Duration::from_secs(1), async {
            loop {
                if claim_attempts.load(Ordering::Acquire) == 2 {
                    let details = registry.details(&runtime_id).await.unwrap();
                    let token = details
                        .pipeline_metrics
                        .as_ref()
                        .and_then(|metrics| metrics.hash_refill_token_available.as_ref())
                        .and_then(|metric| metric.current);
                    if token == Some(0) {
                        break;
                    }
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("closed upstream 只能有 item claim 加最后一次权威空 claim");
        tokio::time::sleep(Duration::from_millis(40)).await;
        assert_eq!(claim_attempts.load(Ordering::Acquire), 2);
        persist_control.release();
    };

    let (summary, ()) = tokio::join!(run, observe);
    let summary = summary.unwrap();
    assert_eq!(summary.scheduled_stage1, 1);
    assert_eq!(claim_attempts.load(Ordering::Acquire), 2);
    assert_eq!(active_leases.load(Ordering::Acquire), 0);
    let details = registry.details(&runtime_id).await.unwrap();
    let metrics = details.pipeline_metrics.unwrap();
    assert_eq!(
        metrics.hash_refill_token_available.unwrap().current,
        Some(0)
    );
    assert_exact_phase_currents_are_zero(&metrics);
}

/// 总持久化 ownership 同时包含 writer 执行中、通道排队和协调器本地 pending。
#[cfg(feature = "test-hooks")]
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn persist_queue_reports_executing_channel_and_local_pending_against_one_hard_cap() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x97; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let rows = test_rows(&media_root, &["hit-a.bin", "hit-b.bin", "hit-c.bin"]);
    for (index, row) in rows.iter().enumerate() {
        let content = store
            .upsert_content_and_location(row, [(index + 1) as u8; 16], MediaKind::Other)
            .unwrap();
        store.mark_base_complete(content.id).unwrap();
    }
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let (reader, _, started_hashes, active_leases) = counting_reader_for(&rows);
    let (mut pool, mut started_workers, _controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "持久化总 ownership")
        .await;
    let runtime_id = reporter.id().to_owned();
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let config = DiskReadConfig::default();
    let (persist_control, persist_waiter) = BasePersistTestController::new();
    let contact_root = cache_root.join("contact-sheets");

    let run = BaseComputeEngine::run_existing_with_first_persist_gate_for_test(
        &mut store,
        &mut pool,
        DisabledRemoteFeatureCache,
        false,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        PipelineLimits::new(1, 1),
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
        persist_waiter,
    );
    let observe_gate = async {
        persist_control.wait_until_entered().await;
        let details = tokio::time::timeout(Duration::from_millis(500), async {
            loop {
                let details = registry.details(&runtime_id).await.unwrap();
                let current = details
                    .pipeline_metrics
                    .as_ref()
                    .and_then(|metrics| metrics.persist_queue.as_ref())
                    .and_then(|queue| queue.current);
                if current == Some(3) {
                    break details;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("1 条执行中 + 1 条 writer 通道排队 + 1 条本地 pending 必须统一计为 3");
        let execution = details.execution_config.unwrap();
        assert_eq!(
            execution.persist_queue_capacity,
            Some(1_001),
            "总容量应来自单批 1000 项与 1 个活动 Worker，而不是 writer 通道容量 1"
        );
        let queue = details.pipeline_metrics.unwrap().persist_queue.unwrap();
        assert_eq!((queue.current, queue.peak), (Some(3), Some(3)));
        assert_eq!(queue.capacity, Some(1_001));
        assert!(queue.current.unwrap() <= queue.capacity.unwrap());
        assert_eq!(started_hashes.load(Ordering::Acquire), 0);
        assert_eq!(active_leases.load(Ordering::Acquire), 0);
        assert!(started_workers.try_recv().is_err());
        persist_control.release();
    };
    let (summary, ()) = tokio::join!(run, observe_gate);
    assert_eq!(summary.unwrap().cache_hits, 3);
}

/// item latency 从成功 claim 起算，直到 Applied ACK 才记录；Worker 终态先释放计算资源。
#[cfg(feature = "test-hooks")]
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn item_completion_latency_starts_at_successful_claim_and_ends_at_applied_ack() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x98; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = test_rows(&media_root, &["ack-gate.bin"]);
    let expected_bytes = rows[0].file_size;
    let (reader, hashes, active_media) =
        media_reader_for(&rows, vec![MediaPermitBehavior::Scheduled], 1);
    let (mut pool, mut started, controller) =
        WorkerPool::controlled_batch_with_cpu_budget_for_test(1, 7);
    let expected_worker_slots = pool.worker_process_ids().len() as u32;
    let expected_cpu_budget = pool.cpu_budget() as u32;
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "Worker ACK 资源门禁")
        .await;
    let runtime_id = reporter.id().to_owned();
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let config = DiskReadConfig {
        hdd_threads_per_disk: 3,
        ssd_threads_per_disk: 5,
        unknown_threads_per_disk: 2,
        total_threads: 9,
        ..DiskReadConfig::default()
    };
    let task_text = task_id.as_uuid().to_string();
    let contact_root = cache_root.join("contact-sheets");
    let (persist_control, persist_waiter) = BasePersistTestController::new();

    let run = BaseComputeEngine::run_existing_with_first_persist_gate_for_test(
        &mut store,
        &mut pool,
        DisabledRemoteFeatureCache,
        false,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        PipelineLimits::new(1, 1),
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
        persist_waiter,
    );
    let observe_gate = async {
        let item_id = started.recv().await.unwrap().1;
        let details_before_result = tokio::time::timeout(Duration::from_secs(1), async {
            loop {
                let details = registry.details(&runtime_id).await.unwrap();
                let unknown = details
                    .pipeline_metrics
                    .as_ref()
                    .and_then(|metrics| metrics.worker_phase_unknown.as_ref())
                    .and_then(|metric| metric.current);
                if unknown == Some(1) {
                    break details;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("Started 后未收到权威 phase 时必须投影 unknown");
        assert_eq!(
            details_before_result
                .pipeline_metrics
                .as_ref()
                .unwrap()
                .worker_start_pending
                .as_ref()
                .unwrap()
                .current,
            Some(0)
        );
        let phase_metrics = details_before_result.pipeline_metrics.as_ref().unwrap();
        for (name, metric) in [
            ("worker_decode", phase_metrics.worker_decode.as_ref()),
            ("worker_feature", phase_metrics.worker_feature.as_ref()),
            (
                "worker_result_wait",
                phase_metrics.worker_result_wait.as_ref(),
            ),
        ] {
            assert_eq!(
                metric.unwrap().current,
                Some(0),
                "{name} 不得与 unknown 重叠"
            );
        }

        // 逐一注入权威 phase，验证 Decode/Feature/ResultWait 投影互斥而不靠任务聚合值猜测。
        for phase in [
            proto::RuntimeWorkerPhase::RuntimeWorkerDecode,
            proto::RuntimeWorkerPhase::RuntimeWorkerFeature,
            proto::RuntimeWorkerPhase::RuntimeWorkerResultWait,
        ] {
            controller
                .phase_changed(task_text.clone(), item_id.clone(), phase)
                .await;
            let details = tokio::time::timeout(Duration::from_secs(1), async {
                loop {
                    let details = registry.details(&runtime_id).await.unwrap();
                    let metrics = details.pipeline_metrics.as_ref().unwrap();
                    let current = match phase {
                        proto::RuntimeWorkerPhase::RuntimeWorkerDecode => metrics
                            .worker_decode
                            .as_ref()
                            .and_then(|metric| metric.current),
                        proto::RuntimeWorkerPhase::RuntimeWorkerFeature => metrics
                            .worker_feature
                            .as_ref()
                            .and_then(|metric| metric.current),
                        proto::RuntimeWorkerPhase::RuntimeWorkerResultWait => metrics
                            .worker_result_wait
                            .as_ref()
                            .and_then(|metric| metric.current),
                        _ => None,
                    };
                    if current == Some(1) {
                        break details;
                    }
                    tokio::task::yield_now().await;
                }
            })
            .await
            .expect("权威 Worker phase 应进入对应唯一投影");
            let metrics = details.pipeline_metrics.as_ref().unwrap();
            for (name, metric, expected) in [
                (
                    "worker_decode",
                    metrics.worker_decode.as_ref(),
                    phase == proto::RuntimeWorkerPhase::RuntimeWorkerDecode,
                ),
                (
                    "worker_feature",
                    metrics.worker_feature.as_ref(),
                    phase == proto::RuntimeWorkerPhase::RuntimeWorkerFeature,
                ),
                (
                    "worker_result_wait",
                    metrics.worker_result_wait.as_ref(),
                    phase == proto::RuntimeWorkerPhase::RuntimeWorkerResultWait,
                ),
            ] {
                assert_eq!(
                    metric.and_then(|value| value.current),
                    Some(if expected { 1 } else { 0 }),
                    "{name} 必须与其它权威 Worker phase 互斥"
                );
            }
        }
        let md5 = running_md5(&controller, &item_id, &hashes);
        controller
            .complete_base(task_text, item_id, md5, other_output())
            .await;
        persist_control.wait_until_entered().await;

        let details = registry.details(&runtime_id).await.unwrap();
        let execution = details
            .execution_config
            .as_ref()
            .expect("运行中的基础计算必须公开实际执行配置");
        assert_eq!(execution.hash_tasks, Some(1));
        assert_eq!(execution.path_cache_queue_capacity, Some(2));
        assert_eq!(execution.content_cache_queue_capacity, Some(1));
        assert_eq!(execution.decode_queue_capacity, Some(2));
        assert_eq!(execution.persist_queue_capacity, Some(1_001));
        assert_eq!(execution.worker_slots, Some(expected_worker_slots));
        assert_eq!(execution.cpu_budget, Some(expected_cpu_budget));
        assert_eq!(
            execution.global_disk_permits,
            Some(config.total_threads as u32)
        );
        assert_eq!(
            execution.hdd_per_disk_permits,
            Some(config.hdd_threads_per_disk as u32)
        );
        assert_eq!(
            execution.ssd_per_disk_permits,
            Some(config.ssd_threads_per_disk as u32)
        );
        assert_eq!(
            execution.unknown_per_disk_permits,
            Some(config.unknown_threads_per_disk as u32)
        );
        assert_eq!(details.summary.unwrap().overall_completed, 0);
        assert_eq!(details.workers.len(), 1);
        assert_eq!(
            details.workers[0].phase,
            Some(proto::RuntimeWorkerPhase::RuntimeWorkerIdle as i32)
        );
        assert!(details.workers[0].display_path.is_empty());
        let metrics = details.pipeline_metrics.unwrap();
        assert_exact_phase_currents_are_zero(&metrics);
        assert!(
            metrics
                .worker_start_pending
                .as_ref()
                .unwrap()
                .peak
                .is_some_and(|peak| peak >= 1),
            "dispatch ACK 后必须至少观察到一次 start-pending 峰值"
        );
        assert_eq!(
            metrics.item_completion_latency, None,
            "Applied ACK 到达前不得记录 item latency"
        );
        assert_eq!(metrics.worker_slots.unwrap().current, Some(0));
        assert_eq!(metrics.cpu_weight.unwrap().current, Some(0));
        assert!(
            metrics.media_throughput.is_empty(),
            "持久化事务 Applied 前不得累计吞吐"
        );
        assert_eq!(active_media.load(Ordering::Acquire), 0);
        persist_control.release();
    };
    let (summary, ()) = tokio::join!(run, observe_gate);
    let summary = summary.unwrap();

    assert_eq!(summary.file_failures, 0);
    let details = registry.details(&runtime_id).await.unwrap();
    assert_eq!(details.summary.unwrap().overall_completed, 1);
    let metrics = details.pipeline_metrics.unwrap();
    assert_exact_phase_currents_are_zero(&metrics);
    assert_eq!(
        metrics
            .item_completion_latency
            .as_ref()
            .expect("Applied ACK 必须生成 item latency 样本")
            .count,
        1
    );
    assert_eq!(
        metrics
            .persist_queue
            .as_ref()
            .unwrap()
            .wait_latency
            .as_ref()
            .unwrap()
            .count,
        1
    );
    assert_eq!(
        metrics
            .persist_queue
            .as_ref()
            .unwrap()
            .service_latency
            .as_ref()
            .unwrap()
            .count,
        1
    );
    let throughput = &metrics.media_throughput;
    assert_eq!(throughput.len(), 1);
    assert_eq!(
        throughput[0].media_kind,
        proto::MediaKind::MediaOther as i32
    );
    assert_eq!(throughput[0].size_bucket, "small");
    assert_eq!(throughput[0].files, 1);
    assert_eq!(throughput[0].bytes, expected_bytes);
}

/// 当前进程排队项由 BaseCompute claim 后才开始统计 latency，不能来自 reserve。
#[cfg(feature = "test-hooks")]
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn queued_item_latency_starts_at_base_compute_claim() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let database = install_root.join("node.db");
    let machine = MachineId::from_sha256([0x99; 32]);
    let media_root = install_root.join("media");
    let scanned_path = media_root.join("recovered-latency.bin");
    let scanned = ScannedPath::new(
        NormalizedPath::new(&scanned_path).unwrap(),
        DisplayPath::new(&scanned_path).unwrap(),
        10,
    );
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let mut store = NodeStore::open(&database, machine.clone()).unwrap();
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let item_id = store
        .reserve_scan_path(task_id, &scanned, 11)
        .unwrap()
        .unwrap();
    store.queue_scan_item_for_read(&item_id).unwrap();

    // rows 为空，避免 reserve_scan_path 参与；BaseCompute 只能从恢复后的 queued 项成功 claim。
    let (reader, hashes, active_media) = media_reader_for(
        std::slice::from_ref(&scanned),
        vec![MediaPermitBehavior::Scheduled],
        1,
    );
    let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "恢复项 latency")
        .await;
    let runtime_id = reporter.id().to_owned();
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let contact_root = cache_root.join("contact-sheets");
    let config = DiskReadConfig::default();
    let (persist_control, persist_waiter) = BasePersistTestController::new();
    let task_text = task_id.as_uuid().to_string();
    let run = BaseComputeEngine::run_existing_with_first_persist_gate_for_test(
        &mut store,
        &mut pool,
        DisabledRemoteFeatureCache,
        false,
        task_id,
        options,
        Vec::new(),
        &contact_root,
        reader,
        PipelineLimits::new(1, 1),
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
        persist_waiter,
    );
    let drive = async {
        let started_item = started.recv().await.unwrap().1;
        assert_eq!(
            started_item, item_id,
            "Worker 必须处理权威恢复后的具体 item"
        );
        let md5 = running_md5(&controller, &started_item, &hashes);
        controller
            .complete_base(task_text, started_item, md5, other_output())
            .await;
        persist_control.wait_until_entered().await;
        let before_applied = registry.details(&runtime_id).await.unwrap();
        assert_eq!(
            before_applied
                .pipeline_metrics
                .as_ref()
                .unwrap()
                .item_completion_latency,
            None,
            "ACK 进入 Applied 前不得记录恢复项 latency"
        );
        persist_control.release();
    };
    let (summary, ()) = tokio::join!(run, drive);
    let summary = summary.unwrap();
    assert_eq!(summary.hashed, 1);
    assert_eq!(summary.file_failures, 0);
    assert_eq!(active_media.load(Ordering::Acquire), 0);
    assert_eq!(
        store.task_items(task_id).unwrap()[0].status,
        TaskItemStatus::Succeeded
    );
    assert_eq!(
        store.task_snapshot(task_id).unwrap().status,
        TaskStatus::Completed
    );
    let details = registry.details(&runtime_id).await.unwrap();
    let metrics = details.pipeline_metrics.unwrap();
    assert_exact_phase_currents_are_zero(&metrics);
    assert_eq!(
        metrics
            .item_completion_latency
            .expect("恢复项 Applied ACK 必须产生 latency")
            .count,
        1
    );
}

/// 控制端被取消或 panic 丢弃时必须自动放行 actor，并把原 Store 还给调用方。
#[cfg(feature = "test-hooks")]
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn dropped_first_persist_controller_releases_actor_and_restores_store() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let database = install_root.join("node.db");
    let machine = MachineId::from_sha256([0x97; 32]);
    let mut store = NodeStore::open(&database, machine.clone()).unwrap();
    let path = install_root.join("media/drop-gate-path-hit.bin");
    let row = ScannedPath::new(
        NormalizedPath::new(&path).unwrap(),
        DisplayPath::new(&path).unwrap(),
        10,
    );
    let content = store
        .upsert_content_and_location(&row, [0xB7; 16], MediaKind::Other)
        .unwrap();
    store.mark_base_complete(content.id).unwrap();
    let options = ScanOptions::new(vec![DisplayPath::new(install_root.join("media")).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let (reader, _, started_hashes, active_leases) = counting_reader_for(&[row.clone()]);
    let (mut pool, mut started_workers, _controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "Drop 自动释放持久化 gate")
        .await;
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let config = DiskReadConfig::default();
    let (persist_control, persist_waiter) = BasePersistTestController::new();
    let contact_root = cache_root.join("contact-sheets");

    let run = BaseComputeEngine::run_existing_with_first_persist_gate_for_test(
        &mut store,
        &mut pool,
        DisabledRemoteFeatureCache,
        false,
        task_id,
        options,
        vec![row],
        &contact_root,
        reader,
        PipelineLimits::new(1, 1),
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
        persist_waiter,
    );
    let drop_control = async {
        persist_control.wait_until_entered().await;
        drop(persist_control);
    };
    let (summary, ()) = tokio::time::timeout(Duration::from_secs(3), async {
        tokio::join!(run, drop_control)
    })
    .await
    .expect("控制端 Drop 后 actor 必须退出并归还 Store");
    let summary = summary.unwrap();

    assert_eq!(summary.cache_hits, 1);
    assert_eq!(started_hashes.load(Ordering::Acquire), 0);
    assert_eq!(active_leases.load(Ordering::Acquire), 0);
    assert!(started_workers.try_recv().is_err());
    let snapshot = store.task_snapshot(task_id).unwrap();
    assert_eq!(snapshot.status, TaskStatus::Completed);
    assert_eq!(snapshot.succeeded, 1);
}

#[tokio::test]
async fn hash_failure_returns_one_credit_and_one_replacement_token() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x4D; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = (1..=3)
        .map(|index| {
            let path = media_root.join(format!("hash-{index}.bin"));
            ScannedPath::new(
                NormalizedPath::new(&path).unwrap(),
                DisplayPath::new(&path).unwrap(),
                10,
            )
        })
        .collect::<Vec<_>>();
    let behaviors = rows
        .iter()
        .enumerate()
        .map(|(index, row)| {
            let behavior = if index == 0 {
                ScriptedHashBehavior::Fail
            } else {
                ScriptedHashBehavior::Success([(index + 1) as u8; 16])
            };
            (row.display_path.as_path().to_path_buf(), behavior)
        })
        .collect();
    let started_hashes = Arc::new(AtomicUsize::new(0));
    let active_leases = Arc::new(AtomicUsize::new(0));
    let reader = ScriptedHashReader {
        behaviors: Arc::new(behaviors),
        started: Arc::clone(&started_hashes),
        active_leases: Arc::clone(&active_leases),
    };
    let config = DiskReadConfig::default();
    let (mut pool, mut started_workers, controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "Hash 文件失败补位")
        .await;
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let remote = DisabledRemoteFeatureCache;
    let task_text = task_id.as_uuid().to_string();
    let contact_root = cache_root.join("contact-sheets");

    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        remote,
        false,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        PipelineLimits::new(2, 1),
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    );
    let drive = async {
        for _ in 0..2 {
            let item_id = started_workers.recv().await.unwrap().1;
            let identity = controller
                .running_files()
                .into_iter()
                .find(|(_, running, _)| running == &item_id)
                .unwrap()
                .2;
            let index = identity
                .display_path
                .as_path()
                .file_stem()
                .unwrap()
                .to_string_lossy()
                .chars()
                .last()
                .unwrap()
                .to_digit(10)
                .unwrap() as u8;
            controller
                .complete_base(task_text.clone(), item_id, [index; 16], other_output())
                .await;
        }
    };

    let (summary, ()) = tokio::join!(run, drive);
    let summary = summary.unwrap();
    assert_eq!(summary.hashed, 2);
    assert_eq!(summary.file_failures, 1);
    assert_eq!(started_hashes.load(Ordering::Acquire), 3);
    assert_eq!(active_leases.load(Ordering::Acquire), 0);
    assert_eq!(
        store
            .task_items(task_id)
            .unwrap()
            .iter()
            .filter(|item| item.status == TaskItemStatus::Failed)
            .count(),
        1
    );
}

/// 所有 Hash 都失败时仍必须沿着 terminal departure 逐项补位，不能停在初始窗口。
#[tokio::test]
async fn all_item_failures_continue_past_the_initial_hash_window() {
    const FILE_COUNT: usize = 6;

    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x4F; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = (0..FILE_COUNT)
        .map(|index| {
            let path = media_root.join(format!("always-fail-{index}.bin"));
            ScannedPath::new(
                NormalizedPath::new(&path).unwrap(),
                DisplayPath::new(&path).unwrap(),
                10,
            )
        })
        .collect::<Vec<_>>();
    let behaviors = rows
        .iter()
        .map(|row| {
            (
                row.display_path.as_path().to_path_buf(),
                ScriptedHashBehavior::Fail,
            )
        })
        .collect::<BTreeMap<_, _>>();
    let started_hashes = Arc::new(AtomicUsize::new(0));
    let active_leases = Arc::new(AtomicUsize::new(0));
    let reader = ScriptedHashReader {
        behaviors: Arc::new(behaviors),
        started: Arc::clone(&started_hashes),
        active_leases: Arc::clone(&active_leases),
    };
    let (mut pool, mut started_workers, _controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "所有 Hash 失败仍逐项补位")
        .await;
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let summary = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        DisabledRemoteFeatureCache,
        false,
        task_id,
        options,
        rows,
        &cache_root.join("contact-sheets"),
        reader,
        PipelineLimits::new(2, 2),
        &DiskReadConfig::default(),
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    )
    .await
    .unwrap();

    assert_eq!(summary.hashed, 0);
    assert_eq!(summary.file_failures, FILE_COUNT);
    assert_eq!(started_hashes.load(Ordering::Acquire), FILE_COUNT);
    assert_eq!(active_leases.load(Ordering::Acquire), 0);
    assert!(
        started_workers.try_recv().is_err(),
        "Hash 失败不得派发 Worker"
    );
    assert_eq!(
        store
            .task_items(task_id)
            .unwrap()
            .iter()
            .filter(|item| item.status == TaskItemStatus::Failed)
            .count(),
        FILE_COUNT
    );
}

#[tokio::test]
async fn cancellation_drains_slow_hashes_and_releases_every_lease() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x4E; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = (1..=2)
        .map(|index| {
            let path = media_root.join(format!("cancel-{index}.bin"));
            ScannedPath::new(
                NormalizedPath::new(&path).unwrap(),
                DisplayPath::new(&path).unwrap(),
                10,
            )
        })
        .collect::<Vec<_>>();
    let cancel_observed = Arc::new(AtomicUsize::new(0));
    let cancel_completed = Arc::new(AtomicUsize::new(0));
    let behaviors = rows
        .iter()
        .enumerate()
        .map(|(index, row)| {
            (
                row.display_path.as_path().to_path_buf(),
                ScriptedHashBehavior::WaitForCancel {
                    observed: Arc::clone(&cancel_observed),
                    completed: Arc::clone(&cancel_completed),
                    delay_ms: if index == 0 { 0 } else { 60 },
                },
            )
        })
        .collect();
    let started_hashes = Arc::new(AtomicUsize::new(0));
    let active_leases = Arc::new(AtomicUsize::new(0));
    let reader = ScriptedHashReader {
        behaviors: Arc::new(behaviors),
        started: Arc::clone(&started_hashes),
        active_leases: Arc::clone(&active_leases),
    };
    let config = DiskReadConfig::default();
    let (mut pool, mut started_workers, _controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "取消 drain Hash")
        .await;
    let runtime_id = reporter.id().to_owned();
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let remote = DisabledRemoteFeatureCache;
    let cancellation = ReadCancellationToken::new();
    let contact_root = cache_root.join("contact-sheets");

    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        remote,
        false,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        PipelineLimits::new(2, 2),
        &config,
        cancellation.clone(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    );
    let cancel = async {
        tokio::time::timeout(Duration::from_secs(1), async {
            while started_hashes.load(Ordering::Acquire) < 2
                || active_leases.load(Ordering::Acquire) < 2
            {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("两个阻塞 Hash 都应取得许可");
        cancellation.cancel();
    };

    let (result, ()) = tokio::join!(run, cancel);
    assert!(matches!(
        result,
        Err(dedup_node_engine::scan::ScanError::Cancelled)
    ));
    assert_eq!(active_leases.load(Ordering::Acquire), 0);
    assert_eq!(cancel_observed.load(Ordering::Acquire), 2);
    assert_eq!(cancel_completed.load(Ordering::Acquire), 2);
    assert!(started_workers.try_recv().is_err());
    assert_eq!(
        store
            .task_items(task_id)
            .unwrap()
            .iter()
            .filter(|item| item.status == TaskItemStatus::Succeeded)
            .count(),
        0,
        "取消后的 Hash 结果不得写成功"
    );
    let details = registry.details(&runtime_id).await.unwrap();
    assert_exact_phase_currents_are_zero(
        &details
            .pipeline_metrics
            .expect("取消清理后必须保留 runtime pipeline snapshot"),
    );
}

#[tokio::test]
async fn closed_worker_pool_is_task_error_and_drains_active_hash_before_returning() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x5E; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = (1..=2)
        .map(|index| {
            let path = media_root.join(format!("closed-{index}.bin"));
            ScannedPath::new(
                NormalizedPath::new(&path).unwrap(),
                DisplayPath::new(&path).unwrap(),
                10,
            )
        })
        .collect::<Vec<_>>();
    let cancel_observed = Arc::new(AtomicUsize::new(0));
    let cancel_completed = Arc::new(AtomicUsize::new(0));
    let behaviors = Arc::new(BTreeMap::from([
        (
            rows[0].display_path.as_path().to_path_buf(),
            ScriptedHashBehavior::SuccessAfterAllStarted {
                md5: [1; 16],
                expected: 2,
            },
        ),
        (
            rows[1].display_path.as_path().to_path_buf(),
            ScriptedHashBehavior::WaitForCancel {
                observed: Arc::clone(&cancel_observed),
                completed: Arc::clone(&cancel_completed),
                delay_ms: 40,
            },
        ),
    ]));
    let active_leases = Arc::new(AtomicUsize::new(0));
    let reader = ScriptedHashReader {
        behaviors,
        started: Arc::new(AtomicUsize::new(0)),
        active_leases: Arc::clone(&active_leases),
    };
    let (mut pool, _started, controller) = WorkerPool::controlled_batch_for_test(1);
    drop(controller);
    assert!(
        pool.next_event().await.is_none(),
        "控制端关闭后必须先确认池 actor 与事件通道已经结束"
    );
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "WorkerPool 关闭")
        .await;
    let runtime_id = reporter.id().to_owned();
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let remote = DisabledRemoteFeatureCache;
    let config = DiskReadConfig::default();
    let result = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        remote,
        false,
        task_id,
        options,
        rows,
        &cache_root.join("contact-sheets"),
        reader,
        PipelineLimits::new(2, 2),
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    )
    .await;

    let error = result.expect_err("已关闭 WorkerPool 必须返回原始 Stage1 错误");
    let error_text = error.to_string();
    assert!(
        error_text.contains("WorkerPool 已关闭"),
        "cleanup ownership 投影不得遮蔽原始 WorkerPool 错误: {error_text}"
    );
    assert!(matches!(
        error,
        dedup_node_engine::scan::ScanError::Stage1(_)
    ));
    assert_eq!(
        store.task_snapshot(task_id).unwrap().status,
        TaskStatus::Running,
        "WorkerPool 基础设施错误必须保留持久任务的既有 running 语义"
    );
    assert_eq!(cancel_observed.load(Ordering::Acquire), 1);
    assert_eq!(cancel_completed.load(Ordering::Acquire), 1);
    assert_eq!(active_leases.load(Ordering::Acquire), 0);
    let details = registry.details(&runtime_id).await.unwrap();
    assert_eq!(
        details.summary.as_ref().unwrap().state,
        "running",
        "引擎错误返回不得替持久 runtime task 偷改终态"
    );
    assert_exact_phase_currents_are_zero(
        &details
            .pipeline_metrics
            .expect("WorkerPool 错误清理后必须保留 ownership snapshot"),
    );
}

#[tokio::test]
async fn first_media_request_enters_stable_once() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x42; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let root = DisplayPath::new(install_root.join("media")).unwrap();
    let options = ScanOptions::new(vec![root.clone()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = (1..=3)
        .map(|index| {
            let path = install_root.join(format!("media/file-{index}.bin"));
            ScannedPath::new(
                NormalizedPath::new(&path).unwrap(),
                DisplayPath::new(&path).unwrap(),
                10,
            )
        })
        .collect::<Vec<_>>();
    let config = DiskReadConfig {
        total_threads: 2,
        hdd_threads_per_disk: 1,
        ssd_threads_per_disk: 1,
        unknown_threads_per_disk: 1,
        ..DiskReadConfig::default()
    };
    let (reader, hashes, _, active_leases) = counting_reader_for(&rows);
    let limits = PipelineLimits::new(4, 2);
    let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(2);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "基础计算")
        .await;
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let contact_root = cache_root.join("contact-sheets");
    let remote = DisabledRemoteFeatureCache;
    let task_text = task_id.as_uuid().to_string();

    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        remote,
        false,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        limits,
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    );
    let drive = async {
        let first = started.recv().await.unwrap().1;
        let second = started.recv().await.unwrap().1;
        assert!(
            tokio::time::timeout(Duration::from_millis(20), started.recv())
                .await
                .is_err()
        );
        let first_md5 = running_md5(&controller, &first, &hashes);
        controller
            .complete_base(task_text.clone(), first, first_md5, other_output())
            .await;

        let third = tokio::time::timeout(Duration::from_secs(1), started.recv())
            .await
            .expect("任一文件完成后应立即补入第三个文件")
            .unwrap()
            .1;
        assert_eq!(controller.available_slots(), 0, "另一个槽位仍在运行");
        let third_md5 = running_md5(&controller, &third, &hashes);
        controller
            .complete_base(task_text.clone(), third, third_md5, other_output())
            .await;
        let second_md5 = running_md5(&controller, &second, &hashes);
        controller
            .complete_base(task_text, second, second_md5, other_output())
            .await;
    };

    let (summary, ()) = tokio::join!(run, drive);
    let summary = summary.unwrap();
    assert_eq!(summary.hashed, 3);
    assert_eq!(summary.file_failures, 0);
    assert_eq!(active_leases.load(Ordering::Acquire), 0);
    assert_eq!(
        store.task_snapshot(task_id).unwrap().status,
        TaskStatus::Completed
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn completed_worker_refills_while_first_persist_is_sqlite_blocked() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let database = install_root.join("node.db");
    let machine = MachineId::from_sha256([0x91; 32]);
    let mut store = NodeStore::open(&database, machine.clone()).unwrap();
    let observer = store.reopen().unwrap();
    let media_root = install_root.join("media");
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = (1..=2)
        .map(|index| {
            let path = media_root.join(format!("persist-{index}.bin"));
            ScannedPath::new(
                NormalizedPath::new(&path).unwrap(),
                DisplayPath::new(&path).unwrap(),
                10,
            )
        })
        .collect::<Vec<_>>();
    let (reader, hashes, active_media) = media_reader_for(
        &rows,
        vec![
            MediaPermitBehavior::Scheduled,
            MediaPermitBehavior::Scheduled,
        ],
        1,
    );
    let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "持久化阻塞补位")
        .await;
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let config = DiskReadConfig::default();
    let task_text = task_id.as_uuid().to_string();
    let contact_root = cache_root.join("contact-sheets");
    let drive_active_media = Arc::clone(&active_media);

    let drive = tokio::spawn(async move {
        let first_item = started.recv().await.unwrap().1;
        let gate = SqliteWriteGate::acquire(&database);
        let first_md5 = running_md5(&controller, &first_item, &hashes);
        controller
            .complete_base(
                task_text.clone(),
                first_item.clone(),
                first_md5,
                other_output(),
            )
            .await;

        let locked_second = tokio::time::timeout(Duration::from_secs(1), started.recv())
            .await
            .ok()
            .flatten();
        let observation =
            locked_second.as_ref().map(|_| LockedPersistObservation {
                available_slots: controller.available_slots(),
                cpu_in_use: controller.cpu_in_use(),
                active_media: drive_active_media.load(Ordering::Acquire),
                first_still_running: observer.task_items(task_id).unwrap().into_iter().any(
                    |item| item.item_id == first_item && item.status == TaskItemStatus::Running,
                ),
                succeeded: observer.task_snapshot(task_id).unwrap().succeeded,
                fault_count: observer.page_file_faults(None, 10).unwrap().items.len(),
            });
        gate.release();

        let second_item = match locked_second {
            Some((_, item_id)) => item_id,
            None => {
                tokio::time::timeout(Duration::from_secs(2), started.recv())
                    .await
                    .expect("解除 SQLite 写锁后第二个 Worker 必须启动")
                    .unwrap()
                    .1
            }
        };
        let second_md5 = running_md5(&controller, &second_item, &hashes);
        controller
            .complete_base(task_text, second_item, second_md5, other_output())
            .await;
        (observation, controller)
    });

    let summary = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        DisabledRemoteFeatureCache,
        false,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        PipelineLimits::new(2, 1),
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    )
    .await
    .unwrap();
    let (observation, controller) = drive.await.unwrap();
    let observation = observation.expect("SQLite BEGIN IMMEDIATE 仍持锁时必须已经补入下一 Worker");

    assert_eq!(observation.available_slots, 0);
    assert_eq!(observation.cpu_in_use, 1);
    assert_eq!(observation.active_media, 1);
    assert!(observation.first_still_running);
    assert_eq!(observation.succeeded, 0);
    assert_eq!(observation.fault_count, 0);
    assert_eq!(summary.file_failures, 0);
    assert_eq!(controller.cpu_in_use(), 0);
    assert_eq!(active_media.load(Ordering::Acquire), 0);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn worker_crash_refills_before_fault_persist_unlock() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let database = install_root.join("node.db");
    let machine = MachineId::from_sha256([0x92; 32]);
    let mut store = NodeStore::open(&database, machine.clone()).unwrap();
    let observer = store.reopen().unwrap();
    let media_root = install_root.join("media");
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = (1..=2)
        .map(|index| {
            let path = media_root.join(format!("crash-persist-{index}.mp4"));
            ScannedPath::new(
                NormalizedPath::new(&path).unwrap(),
                DisplayPath::new(&path).unwrap(),
                4_096,
            )
        })
        .collect::<Vec<_>>();
    let (reader, hashes, active_media) = media_reader_for(
        &rows,
        vec![
            MediaPermitBehavior::Scheduled,
            MediaPermitBehavior::Scheduled,
        ],
        1,
    );
    let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "崩溃持久化阻塞补位")
        .await;
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let config = DiskReadConfig::default();
    let task_text = task_id.as_uuid().to_string();
    let contact_root = cache_root.join("contact-sheets");
    let drive_active_media = Arc::clone(&active_media);

    let drive = tokio::spawn(async move {
        let first_item = started.recv().await.unwrap().1;
        let gate = SqliteWriteGate::acquire(&database);
        controller
            .crash(
                task_text.clone(),
                first_item.clone(),
                "Worker 管道断开".into(),
            )
            .await;

        let locked_second = tokio::time::timeout(Duration::from_secs(1), started.recv())
            .await
            .ok()
            .flatten();
        let observation =
            locked_second.as_ref().map(|_| LockedPersistObservation {
                available_slots: controller.available_slots(),
                cpu_in_use: controller.cpu_in_use(),
                active_media: drive_active_media.load(Ordering::Acquire),
                first_still_running: observer.task_items(task_id).unwrap().into_iter().any(
                    |item| item.item_id == first_item && item.status == TaskItemStatus::Running,
                ),
                succeeded: observer.task_snapshot(task_id).unwrap().succeeded,
                fault_count: observer.page_file_faults(None, 10).unwrap().items.len(),
            });
        gate.release();

        let second_item = match locked_second {
            Some((_, item_id)) => item_id,
            None => {
                tokio::time::timeout(Duration::from_secs(2), started.recv())
                    .await
                    .expect("解除 SQLite 写锁后崩溃补槽必须继续派发")
                    .unwrap()
                    .1
            }
        };
        let second_md5 = running_md5(&controller, &second_item, &hashes);
        controller
            .complete_base(task_text, second_item, second_md5, other_output())
            .await;
        (observation, controller)
    });

    let summary = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        DisabledRemoteFeatureCache,
        false,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        PipelineLimits::new(2, 1),
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    )
    .await
    .unwrap();
    let (observation, controller) = drive.await.unwrap();
    let observation = observation.expect("Worker crash fault 事务仍持锁时必须已经补入下一 Worker");

    assert_eq!(observation.available_slots, 0);
    assert_eq!(observation.cpu_in_use, 1);
    assert_eq!(observation.active_media, 1);
    assert!(observation.first_still_running);
    assert_eq!(observation.succeeded, 0);
    assert_eq!(observation.fault_count, 0);
    assert_eq!(summary.file_failures, 1);
    assert_eq!(controller.cpu_in_use(), 0);
    assert_eq!(active_media.load(Ordering::Acquire), 0);
    assert_eq!(store.page_file_faults(None, 10).unwrap().items.len(), 1);
}

#[tokio::test]
async fn video_persist_orders_stage1_outbox_before_contact_sheet() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let database = install_root.join("node.db");
    let machine = MachineId::from_sha256([0x93; 32]);
    let mut store = NodeStore::open(&database, machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = test_rows(&media_root, &["ordered-contact.mp4"]);
    let (reader, hashes, _, _) = counting_reader_for(&rows);
    let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "联系表事务顺序")
        .await;
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let task_text = task_id.as_uuid().to_string();
    let contact_root = cache_root.join("contact-sheets");
    let config = DiskReadConfig::default();
    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        DisabledRemoteFeatureCache,
        false,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        PipelineLimits::new(1, 1),
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    );
    let drive = async {
        let item_id = started.recv().await.unwrap().1;
        let md5 = running_md5(&controller, &item_id, &hashes);
        controller
            .complete_base(task_text, item_id, md5, video_output_with_contact_sheet())
            .await;
    };
    let (summary, ()) = tokio::join!(run, drive);
    summary.unwrap();

    let kinds = store
        .pull_changes(0, 100)
        .unwrap()
        .changes
        .into_iter()
        .map(|change| change.entity_kind)
        .collect::<Vec<_>>();
    let stage1 = &kinds[kinds.len() - 9..];
    assert_eq!(stage1[0], "content");
    assert_eq!(stage1[1], "video_metadata");
    assert!(stage1[2..8].iter().all(|kind| kind == "video_frame_stage1"));
    assert_eq!(stage1[8], "contact_sheet");
}

#[tokio::test]
async fn failed_stage1_transaction_removes_contact_sheet_and_outbox_side_effects() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let database = install_root.join("node.db");
    let machine = MachineId::from_sha256([0x94; 32]);
    let mut store = NodeStore::open(&database, machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = test_rows(&media_root, &["failed-contact.mp4"]);
    let (reader, hashes, _, _) = counting_reader_for(&rows);
    let expected_md5 = hashes[rows[0].display_path.as_path()];
    let final_contact = contact_sheet_path(&cache_root.join("contact-sheets"), expected_md5);
    let trigger = rusqlite::Connection::open(&database).unwrap();
    trigger
        .execute_batch(
            "CREATE TRIGGER abort_last_stage1_frame
             BEFORE INSERT ON video_frame_stage1
             WHEN NEW.slot=5
             BEGIN SELECT RAISE(ABORT, 'controlled stage1 failure'); END;",
        )
        .unwrap();
    drop(trigger);
    let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "联系表事务回滚")
        .await;
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let task_text = task_id.as_uuid().to_string();
    let contact_root = cache_root.join("contact-sheets");
    let config = DiskReadConfig::default();
    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        DisabledRemoteFeatureCache,
        false,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        PipelineLimits::new(1, 1),
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    );
    let drive = async {
        let item_id = started.recv().await.unwrap().1;
        let md5 = running_md5(&controller, &item_id, &hashes);
        controller
            .complete_base(task_text, item_id, md5, video_output_with_contact_sheet())
            .await;
    };
    let (result, ()) = tokio::join!(run, drive);
    assert!(matches!(
        result,
        Err(dedup_node_engine::scan::ScanError::Stage1(_))
    ));

    let observer = rusqlite::Connection::open(&database).unwrap();
    let feature_rows: i64 = observer
        .query_row(
            "SELECT
               (SELECT COUNT(*) FROM video_metadata) +
               (SELECT COUNT(*) FROM video_frame_stage1) +
               (SELECT COUNT(*) FROM contact_sheets)",
            [],
            |row| row.get(0),
        )
        .unwrap();
    let feature_outbox: i64 = observer
        .query_row(
            "SELECT COUNT(*) FROM sync_outbox
             WHERE entity_kind IN ('video_metadata','video_frame_stage1','contact_sheet')",
            [],
            |row| row.get(0),
        )
        .unwrap();
    assert_eq!(feature_rows, 0);
    assert_eq!(feature_outbox, 0);
    assert!(!final_contact.exists(), "失败事务不得保留本轮最终联系表");
    if let Some(parent) = final_contact.parent() {
        assert!(
            !parent.is_dir()
                || std::fs::read_dir(parent).unwrap().all(|entry| !entry
                    .unwrap()
                    .file_name()
                    .to_string_lossy()
                    .contains(".partial")),
            "失败事务不得保留 partial 联系表"
        );
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn cancel_with_full_persist_queue_does_not_deadlock_or_report_late_success() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let database = install_root.join("node.db");
    let machine = MachineId::from_sha256([0x95; 32]);
    let mut store = NodeStore::open(&database, machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = test_rows(
        &media_root,
        &[
            "persist-full-1.bin",
            "persist-full-2.bin",
            "persist-full-3.bin",
        ],
    );
    let (reader, hashes, active_media) = media_reader_for(
        &rows,
        vec![
            MediaPermitBehavior::Scheduled,
            MediaPermitBehavior::Scheduled,
            MediaPermitBehavior::Scheduled,
        ],
        3,
    );
    let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(3);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "满持久化队列取消")
        .await;
    let runtime_id = reporter.id().to_owned();
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let task_text = task_id.as_uuid().to_string();
    let contact_root = cache_root.join("contact-sheets");
    let config = DiskReadConfig::default();
    let cancellation = ReadCancellationToken::new();
    let cancel_drive = cancellation.clone();
    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        DisabledRemoteFeatureCache,
        false,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        // channel_capacity=1 使 persist actor 忙于首项时只能再缓存一条消息。
        PipelineLimits::new(3, 1),
        &config,
        cancellation,
        &reporter,
        &artifacts,
        &cleaner,
        20,
    );
    let drive = async {
        let mut item_ids = Vec::new();
        for _ in 0..3 {
            item_ids.push(started.recv().await.unwrap().1);
        }
        let gate = SqliteWriteGate::acquire(&database);
        for item_id in item_ids {
            let md5 = running_md5(&controller, &item_id, &hashes);
            controller
                .complete_base(task_text.clone(), item_id, md5, other_output())
                .await;
        }
        tokio::time::timeout(Duration::from_secs(1), async {
            while active_media.load(Ordering::Acquire) != 0 {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("三个终态都应先从 active 移除并释放媒体许可");
        cancel_drive.cancel();
        gate.cancel_task_and_release(&task_text, 30);
    };
    let (result, ()) =
        tokio::time::timeout(Duration::from_secs(3), async { tokio::join!(run, drive) })
            .await
            .expect("取消加满持久化队列不得自死锁");
    assert!(matches!(
        result,
        Err(dedup_node_engine::scan::ScanError::Cancelled)
    ));
    let snapshot = store.task_snapshot(task_id).unwrap();
    assert_eq!(snapshot.status, TaskStatus::Cancelled);
    assert_eq!(snapshot.succeeded, 0, "解锁后的晚到消息不得提交成功");
    let details = registry.details(&runtime_id).await.unwrap();
    assert_eq!(details.summary.unwrap().overall_completed, 0);
    let compute = details
        .stages
        .iter()
        .find(|stage| stage.stage_id == "compute_base_features")
        .unwrap();
    assert_eq!(compute.completed, 0, "IgnoredInactive ACK 不得报告晚到成功");
    let metrics = details.pipeline_metrics.unwrap();
    assert!(
        metrics.item_completion_latency.is_none(),
        "Ignored ACK 不得记录 item latency"
    );
    assert!(
        metrics.media_throughput.is_empty(),
        "IgnoredInactive ACK 不得累计媒体吞吐"
    );
}

#[tokio::test]
async fn source_read_complete_releases_media_permit_before_terminal_and_unblocks_same_disk_dispatch()
 {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x81; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = test_rows(&media_root, &["first.mp4", "second.mp4"]);
    let (reader, hashes, active_media) = media_reader_for(
        &rows,
        vec![
            MediaPermitBehavior::Scheduled,
            MediaPermitBehavior::Scheduled,
        ],
        1,
    );
    let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(2);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "媒体读取许可即时复用")
        .await;
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let config = DiskReadConfig::default();
    let task_text = task_id.as_uuid().to_string();
    let contact_root = cache_root.join("contact-sheets");

    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        DisabledRemoteFeatureCache,
        false,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        PipelineLimits::new(4, 2),
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    );
    let drive = async {
        let first = started.recv().await.unwrap().1;
        assert_eq!(active_media.load(Ordering::Acquire), 1);
        assert!(
            tokio::time::timeout(Duration::from_millis(30), started.recv())
                .await
                .is_err(),
            "第一项未报告源读取完成时，同盘第二项不得启动"
        );

        controller
            .base_source_read_complete(task_text.clone(), first.clone())
            .await;
        let second = tokio::time::timeout(Duration::from_secs(1), started.recv())
            .await
            .expect("第一项 SourceReadComplete 后第二项应立即获得同盘许可")
            .unwrap()
            .1;
        assert_eq!(controller.running_files().len(), 2, "第一项仍在 CPU 尾段");
        assert_eq!(active_media.load(Ordering::Acquire), 1);
        let first_md5 = running_md5(&controller, &first, &hashes);
        let second_md5 = running_md5(&controller, &second, &hashes);
        controller
            .base_source_read_complete(task_text.clone(), second.clone())
            .await;
        tokio::time::timeout(Duration::from_secs(1), async {
            while active_media.load(Ordering::Acquire) != 0 {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("第二项 SourceReadComplete 应释放最后一个媒体许可");
        controller
            .complete_base(task_text.clone(), first, first_md5, other_output())
            .await;
        controller
            .complete_base(task_text, second, second_md5, other_output())
            .await;
    };

    let (summary, ()) = tokio::join!(run, drive);
    let summary = summary.unwrap();
    assert_eq!(summary.scheduled_stage1, 2);
    assert_eq!(summary.file_failures, 0);
    assert_eq!(active_media.load(Ordering::Acquire), 0);
}

#[tokio::test]
async fn terminal_without_source_complete_releases_media_permit_before_outbox_wait() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x82; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = test_rows(&media_root, &["terminal-only.mp4"]);
    let (reader, hashes, active_media) =
        media_reader_for(&rows, vec![MediaPermitBehavior::Scheduled], 1);
    let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "终态媒体许可兜底")
        .await;
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let config = DiskReadConfig::default();
    let task_text = task_id.as_uuid().to_string();
    let contact_root = cache_root.join("contact-sheets");
    let outbox_entered = Arc::new(Notify::new());
    let release_outbox = Arc::new(Notify::new());
    let remote = GatedOutboxCache {
        entered: Arc::clone(&outbox_entered),
        release: Arc::clone(&release_outbox),
    };

    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        remote,
        true,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        PipelineLimits::new(2, 1),
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    );
    let drive = async {
        let item_id = started.recv().await.unwrap().1;
        assert_eq!(active_media.load(Ordering::Acquire), 1);
        let md5 = running_md5(&controller, &item_id, &hashes);
        controller
            .complete_base(task_text, item_id, md5, other_output())
            .await;
        tokio::time::timeout(Duration::from_secs(1), outbox_entered.notified())
            .await
            .expect("Worker 终态持久化后应进入 outbox 等待");
        assert_eq!(
            active_media.load(Ordering::Acquire),
            0,
            "outbox 等待期间不得持有媒体读取许可"
        );
        release_outbox.notify_one();
    };

    let (summary, ()) = tokio::join!(run, drive);
    assert_eq!(summary.unwrap().file_failures, 0);
    assert_eq!(active_media.load(Ordering::Acquire), 0);
}

#[tokio::test]
async fn controlled_cancel_releases_cpu_at_worker_stop_but_media_after_ack() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x83; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = test_rows(&media_root, &["active.mp4", "pending.mp4"]);
    let pending_entered = Arc::new(Notify::new());
    let pending_dropped = Arc::new(AtomicUsize::new(0));
    let (reader, _hashes, active_media) = media_reader_for(
        &rows,
        vec![
            MediaPermitBehavior::Scheduled,
            MediaPermitBehavior::Never {
                entered: Arc::clone(&pending_entered),
                dropped: Arc::clone(&pending_dropped),
            },
        ],
        1,
    );
    let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(2);
    let (worker_stopped, release_cancel_ack) = controller.gate_next_cancel_ack_for_test();
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "取消媒体许可")
        .await;
    let runtime_id = reporter.id().to_owned();
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let config = DiskReadConfig::default();
    let cancellation = ReadCancellationToken::new();
    let contact_root = cache_root.join("contact-sheets");

    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        DisabledRemoteFeatureCache,
        false,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        PipelineLimits::new(4, 2),
        &config,
        cancellation.clone(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    );
    let drive = async {
        let _first = started.recv().await.expect("第一项应先启动 Worker");
        assert_eq!(controller.cpu_in_use(), 1, "活动基础计算必须登记 CPU 权重");
        tokio::time::timeout(Duration::from_secs(1), pending_entered.notified())
            .await
            .expect("第二项应进入待取消的媒体许可 future");
        assert_eq!(active_media.load(Ordering::Acquire), 1);
        cancellation.cancel();
        tokio::time::timeout(Duration::from_secs(1), worker_stopped.notified())
            .await
            .expect("可控取消必须先确认活动 Worker 已停止");
        assert!(controller.running_files().is_empty());
        assert_eq!(
            controller.cpu_in_use(),
            0,
            "Worker 停止边界必须先释放 CPU 权重"
        );
        assert_eq!(controller.available_slots(), 2, "取消应恢复全部 idle 槽位");
        assert_eq!(pending_dropped.load(Ordering::Acquire), 1);
        assert_eq!(
            active_media.load(Ordering::Acquire),
            1,
            "Worker 停止但取消 ACK 尚未返回时，active 许可仍由引擎持有"
        );
        release_cancel_ack.notify_one();
        tokio::time::timeout(Duration::from_secs(1), async {
            while active_media.load(Ordering::Acquire) != 0 {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("取消 ACK 返回后引擎必须归还 active 媒体许可");
        assert_eq!(controller.cpu_in_use(), 0, "取消 ACK 不得重复释放 CPU 权重");
        let late = tokio::time::timeout(Duration::from_millis(30), started.recv()).await;
        assert!(
            !matches!(late, Ok(Some(_))),
            "取消后不得迟到启动第二个 Worker"
        );
    };

    let (result, ()) = tokio::join!(run, drive);
    assert!(matches!(
        result,
        Err(dedup_node_engine::scan::ScanError::Cancelled)
    ));
    assert_eq!(pending_dropped.load(Ordering::Acquire), 1);
    assert_eq!(active_media.load(Ordering::Acquire), 0);
    let details = registry.details(&runtime_id).await.unwrap();
    assert_eq!(
        details.summary.as_ref().unwrap().state,
        "running",
        "直接引擎测试在 finish 前即可验证正常 cleanup，不得依赖终态兜底"
    );
    let worker = details
        .workers
        .first()
        .expect("Started 必须建立 Worker 投影");
    assert_eq!(
        worker.phase,
        Some(proto::RuntimeWorkerPhase::RuntimeWorkerIdle as i32)
    );
    assert!(worker.display_path.is_empty());
    let metrics = details.pipeline_metrics.unwrap();
    // 取消仍保持持久任务 running 时，也必须在同一最终 snapshot 证明新阶段 ownership 全归零。
    assert_exact_phase_currents_are_zero(&metrics);
    for current in [
        metrics.hash_queue.unwrap().current,
        metrics.path_cache_queue.unwrap().current,
        metrics.content_cache_queue.unwrap().current,
        metrics.decode_queue.unwrap().current,
        metrics.persist_queue.unwrap().current,
        metrics.hash_io.unwrap().current,
        metrics.media_io.unwrap().current,
        metrics.cpu_weight.as_ref().unwrap().current,
        metrics.worker_slots.as_ref().unwrap().current,
    ] {
        assert_eq!(current, Some(0));
    }
    assert_eq!(metrics.cpu_weight.unwrap().peak, Some(1));
    assert_eq!(metrics.worker_slots.unwrap().peak, Some(1));
    assert_eq!(
        store
            .task_items(task_id)
            .unwrap()
            .iter()
            .filter(|item| item.status == TaskItemStatus::Succeeded)
            .count(),
        0
    );
}

#[tokio::test]
async fn dispatch_ack_moves_dispatching_to_start_pending() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x85; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = test_rows(&media_root, &["active.mp4", "acquiring.mp4", "pending.mp4"]);
    let (reader, _hashes, active_media) = media_reader_for(
        &rows,
        vec![
            MediaPermitBehavior::Scheduled,
            MediaPermitBehavior::Scheduled,
            MediaPermitBehavior::Scheduled,
        ],
        1,
    );
    let (mut pool, mut started, _controller) =
        WorkerPool::controlled_batch_without_started_event_for_test(2);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "解码总 ownership")
        .await;
    let runtime_id = reporter.id().to_owned();
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let config = DiskReadConfig::default();
    let cancellation = ReadCancellationToken::new();
    let cancel_run = cancellation.clone();
    let contact_root = cache_root.join("contact-sheets");

    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        DisabledRemoteFeatureCache,
        false,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        PipelineLimits::new(1, 1),
        &config,
        cancel_run,
        &reporter,
        &artifacts,
        &cleaner,
        20,
    );
    let observe = async {
        started.recv().await.expect("第一项必须已经派发 Worker");
        let details = tokio::time::timeout(Duration::from_millis(500), async {
            loop {
                let details = registry.details(&runtime_id).await.unwrap();
                let current = details
                    .pipeline_metrics
                    .as_ref()
                    .and_then(|metrics| metrics.decode_queue.as_ref())
                    .and_then(|queue| queue.current);
                if current == Some(3) {
                    break details;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("pending + media acquiring + dispatched-before-Started 必须统一计入 2W decode ownership");
        let execution = details.execution_config.unwrap();
        assert_eq!(execution.decode_queue_capacity, Some(4));
        let metrics = details.pipeline_metrics.as_ref().unwrap();
        assert_eq!(
            metrics.worker_dispatching.as_ref().unwrap().current,
            Some(0),
            "dispatch ACK 返回后不得继续占用 dispatching"
        );
        assert_eq!(
            metrics.worker_start_pending.as_ref().unwrap().current,
            Some(1),
            "Started 尚未到达时 active 项必须计入 start-pending"
        );
        assert_eq!(
            metrics.worker_phase_unknown.as_ref().unwrap().current,
            Some(0)
        );
        let queue = metrics.decode_queue.as_ref().unwrap();
        assert_eq!(
            (queue.current, queue.peak, queue.capacity),
            (Some(3), Some(3), Some(4))
        );
        assert_eq!(
            metrics.decode_credit_owned.as_ref().unwrap().current,
            Some(3),
            "decode credit current 必须与 pending/media/start-pending 守恒"
        );
        assert_eq!(active_media.load(Ordering::Acquire), 1);
        cancellation.cancel();
    };
    let (result, ()) = tokio::join!(run, observe);
    assert!(matches!(
        result,
        Err(dedup_node_engine::scan::ScanError::Cancelled)
    ));
    assert_eq!(active_media.load(Ordering::Acquire), 0);
}

/// 本地 Worker candidate 在 2W credit 已满时必须保留 Hash 输出，不消费后续 content 上下文。
#[tokio::test]
async fn local_compute_candidate_waits_without_consuming_context_when_credit_is_full() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x86; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = test_rows(&media_root, &["local-1.mp4", "local-2.mp4", "local-3.mp4"]);
    let (reader, _hashes, started_hashes, _active_leases) = counting_reader_for(&rows);
    let (mut pool, mut started, _controller) =
        WorkerPool::controlled_batch_without_started_event_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "local decode credit 背压")
        .await;
    let runtime_id = reporter.id().to_owned();
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let config = DiskReadConfig::default();
    let cancellation = ReadCancellationToken::new();
    let contact_root = cache_root.join("contact-sheets");
    // 记录本地 content 批量查询次数，验证背压轮询不会重复读取同一批 SQLite 记录。
    let local_lookup_calls = Arc::new(AtomicUsize::new(0));
    let _local_lookup_observer = BaseComputeEngine::install_local_content_lookup_observer_for_test(
        Arc::clone(&local_lookup_calls),
    );
    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        DisabledRemoteFeatureCache,
        false,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        PipelineLimits::new(4, 4),
        &config,
        cancellation.clone(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    );
    let observe = async {
        started.recv().await.expect("第一项必须进入 dispatch ACK");
        let details = tokio::time::timeout(Duration::from_secs(1), async {
            loop {
                let details = registry.details(&runtime_id).await.unwrap();
                let full = details
                    .pipeline_metrics
                    .as_ref()
                    .and_then(|metrics| metrics.decode_credit_owned.as_ref())
                    .and_then(|metric| metric.current)
                    == Some(2);
                if full && started_hashes.load(Ordering::Acquire) == 3 {
                    break details;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("local candidate 必须在 2W 满时保留 content 上下文");
        assert_eq!(
            details
                .pipeline_metrics
                .unwrap()
                .decode_credit_owned
                .unwrap()
                .current,
            Some(2)
        );
        assert_eq!(started_hashes.load(Ordering::Acquire), 3);
        for _ in 0..8 {
            tokio::task::yield_now().await;
        }
        assert_eq!(
            local_lookup_calls.load(Ordering::Acquire),
            1,
            "同一批本地 content 候选在 decode credit 背压时不得重复查询 SQLite"
        );
        cancellation.cancel();
    };
    let (result, ()) = tokio::join!(run, observe);
    assert!(matches!(
        result,
        Err(dedup_node_engine::scan::ScanError::Cancelled)
    ));
}

/// 远端 content cursor 在 decode credit 已满时必须保留队首和上下文，不能偷消费结果。
#[tokio::test]
async fn remote_compute_candidate_waits_without_consuming_cursor_when_credit_is_full() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x87; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = test_rows(
        &media_root,
        &["remote-1.mp4", "remote-2.mp4", "remote-3.mp4"],
    );
    let (reader, _hashes, started_hashes, _active_leases) = counting_reader_for(&rows);
    let (mut pool, mut started, _controller) =
        WorkerPool::controlled_batch_without_started_event_for_test(1);
    let cache_entered = Arc::new(Notify::new());
    let cache_release = Arc::new(Notify::new());
    let remote = GatedContentCache {
        entered: Arc::clone(&cache_entered),
        release: Arc::clone(&cache_release),
        calls: Arc::new(AtomicUsize::new(0)),
    };
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "remote decode credit 背压")
        .await;
    let runtime_id = reporter.id().to_owned();
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let config = DiskReadConfig::default();
    let cancellation = ReadCancellationToken::new();
    let contact_root = cache_root.join("contact-sheets");
    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        remote,
        true,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        PipelineLimits::new(4, 4),
        &config,
        cancellation.clone(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    );
    let observe = async {
        tokio::time::timeout(Duration::from_secs(1), cache_entered.notified())
            .await
            .expect("远端 content 查询必须真实进入 resolver");
        cache_release.notify_one();
        started.recv().await.expect("第一项必须进入 dispatch ACK");
        let details = tokio::time::timeout(Duration::from_secs(1), async {
            loop {
                let details = registry.details(&runtime_id).await.unwrap();
                let full = details
                    .pipeline_metrics
                    .as_ref()
                    .and_then(|metrics| metrics.decode_credit_owned.as_ref())
                    .and_then(|metric| metric.current)
                    == Some(2);
                if full && started_hashes.load(Ordering::Acquire) == 3 {
                    break details;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("remote cursor 必须在 2W 满时保留未消费项");
        assert_eq!(
            details
                .pipeline_metrics
                .unwrap()
                .decode_credit_owned
                .unwrap()
                .current,
            Some(2)
        );
        assert_eq!(started_hashes.load(Ordering::Acquire), 3);
        cancellation.cancel();
    };
    let (result, ()) = tokio::join!(run, observe);
    assert!(matches!(
        result,
        Err(dedup_node_engine::scan::ScanError::Cancelled)
    ));
}

#[cfg(feature = "test-hooks")]
#[tokio::test]
async fn real_join_sets_expose_completed_hash_and_media_ready_windows() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x85; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = test_rows(
        &media_root,
        &["gate-fails.mp4", "gate-none.mp4", "gate-permit.mp4"],
    );
    let (reader, hashes, active_media) = media_reader_for(
        &rows,
        vec![
            MediaPermitBehavior::Fail,
            MediaPermitBehavior::None,
            MediaPermitBehavior::Scheduled,
        ],
        1,
    );
    let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(3);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "真实 JoinSet 阶段窗口")
        .await;
    let runtime_id = reporter.id().to_owned();
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let config = DiskReadConfig::default();
    let contact_root = cache_root.join("contact-sheets");
    let gates = BaseComputeJoinObservationHooks::new(3, 3);
    let run = BaseComputeEngine::run_existing_with_join_observation_for_test(
        &mut store,
        &mut pool,
        DisabledRemoteFeatureCache,
        false,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        PipelineLimits::new(3, 3),
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
        gates.clone(),
    );
    let task_text = task_id.as_uuid().to_string();
    let observe = async {
        gates.wait_for_hash_completed().await;
        let details = registry.details(&runtime_id).await.unwrap();
        let metrics = details.pipeline_metrics.as_ref().unwrap();
        assert_eq!(
            metrics.hash_completed_unjoined.as_ref().unwrap().current,
            Some(3),
            "Hash future 已完成但尚未 join 时必须出现 completed-unjoined 正值窗口"
        );
        assert_eq!(metrics.hash_reading.as_ref().unwrap().current, Some(0));
        gates.release_hash_join();

        gates.wait_for_media_ready().await;
        let details = registry.details(&runtime_id).await.unwrap();
        let metrics = details.pipeline_metrics.as_ref().unwrap();
        assert_eq!(
            metrics.media_acquire_ready.as_ref().unwrap().current,
            Some(3),
            "Error/None/Some 三个 media future 已完成但尚未 join 时必须均计 ready"
        );
        assert_eq!(
            metrics.media_permit_ready.as_ref().unwrap().current,
            Some(1),
            "只有 Some(permit) 进入 permit-ready 子集"
        );
        assert_eq!(
            metrics.decode_credit_owned.as_ref().unwrap().current,
            Some(3),
            "Error/None/Some ready 在协调器 join 前仍必须持有三枚 decode credit"
        );
        gates.release_media_join();

        for _ in 0..2 {
            let item_id = tokio::time::timeout(Duration::from_secs(1), started.recv())
                .await
                .expect("两个未命中项应在 media join 后派发 Worker")
                .unwrap()
                .1;
            let md5 = running_md5(&controller, &item_id, &hashes);
            controller
                .base_source_read_complete(task_text.clone(), item_id.clone())
                .await;
            controller
                .complete_base(task_text.clone(), item_id, md5, other_output())
                .await;
        }
    };
    let (summary, ()) = tokio::join!(run, observe);
    let summary = summary.unwrap();
    assert_eq!(summary.file_failures, 1);
    assert_eq!(summary.scheduled_stage1, 2);
    assert_eq!(active_media.load(Ordering::Acquire), 0);
    let details = registry.details(&runtime_id).await.unwrap();
    assert_exact_phase_currents_are_zero(&details.pipeline_metrics.unwrap());
}

#[tokio::test]
async fn media_ready_counts_error_none_and_real_permit() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x84; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = test_rows(
        &media_root,
        &["permit-fails.mp4", "none-permit.mp4", "continues.mp4"],
    );
    let continued_path = rows[1].display_path.clone();
    let scheduled_path = rows[2].display_path.clone();
    let (reader, hashes, active_media) = media_reader_for(
        &rows,
        vec![
            MediaPermitBehavior::Fail,
            MediaPermitBehavior::None,
            MediaPermitBehavior::Scheduled,
        ],
        1,
    );
    let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "媒体许可文件级失败")
        .await;
    let runtime_id = reporter.id().to_owned();
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let config = DiskReadConfig::default();
    let task_text = task_id.as_uuid().to_string();
    let contact_root = cache_root.join("contact-sheets");

    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        DisabledRemoteFeatureCache,
        false,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        PipelineLimits::new(2, 1),
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    );
    let drive = async {
        for (expected_path, label) in [
            (continued_path, "None 媒体许可"),
            (scheduled_path, "真实媒体许可"),
        ] {
            let item_id = tokio::time::timeout(Duration::from_secs(1), started.recv())
                .await
                .expect("媒体许可完成后应继续补位派发")
                .unwrap()
                .1;
            let running = controller
                .running_files()
                .into_iter()
                .find(|(_, running, _)| running == &item_id)
                .unwrap()
                .2;
            assert_eq!(
                running.display_path, expected_path,
                "{label} 应保持输入顺序"
            );
            let md5 = running_md5(&controller, &item_id, &hashes);
            controller
                .base_source_read_complete(task_text.clone(), item_id.clone())
                .await;
            controller
                .complete_base(task_text.clone(), item_id, md5, other_output())
                .await;
        }
    };

    let (summary, ()) = tokio::join!(run, drive);
    let summary = summary.unwrap();
    assert_eq!(summary.file_failures, 1);
    assert_eq!(summary.scheduled_stage1, 2);
    assert_eq!(active_media.load(Ordering::Acquire), 0);
    assert_eq!(
        store.task_snapshot(task_id).unwrap().status,
        TaskStatus::Completed
    );
    let items = store.task_items(task_id).unwrap();
    assert_eq!(
        items
            .iter()
            .filter(|item| item.status == TaskItemStatus::Failed)
            .count(),
        1
    );
    assert_eq!(
        items
            .iter()
            .filter(|item| item.status == TaskItemStatus::Succeeded)
            .count(),
        2
    );
    let details = registry.details(&runtime_id).await.unwrap();
    let metrics = details.pipeline_metrics.unwrap();
    assert_eq!(
        metrics
            .item_completion_latency
            .as_ref()
            .expect("Applied success/failure ACK 都应记录 item latency")
            .count,
        3,
        "失败、None 成功和真实 permit 成功均从具体 claim 起记录时延"
    );
    assert_exact_phase_currents_are_zero(&metrics);
}

#[tokio::test]
async fn lookup_cache_reports_each_completed_thousand_file_batch() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x43; 32]);
    let database = install_root.join("data/node/node.sqlite3");
    let mut store = NodeStore::open(&database, machine.clone()).unwrap();
    // 同进程观察者只读取已提交阶段，不能再次触发 Node 启动期的 transient 清理。
    let observer = store.reopen().unwrap();
    let root = DisplayPath::new(install_root.join("media")).unwrap();
    let options = ScanOptions::new(vec![root]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = (0..=1_000)
        .map(|index| {
            let path = install_root.join(format!("media/file-{index}.bin"));
            ScannedPath::new(
                NormalizedPath::new(&path).unwrap(),
                DisplayPath::new(&path).unwrap(),
                10,
            )
        })
        .collect::<Vec<_>>();
    let config = DiskReadConfig::default();
    let (reader, limits) = ScheduledFileReader::controlled_for_test(&config, 1, NeverRead, |_| {
        (vec![1], LocalDiskKind::Hdd)
    })
    .unwrap();
    let (mut pool, _started, _controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "基础计算")
        .await;
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let contact_root = cache_root.join("contact-sheets");
    let second_batch_started = Arc::new(Notify::new());
    let release_second_batch = Arc::new(Notify::new());
    let remote = GatedPathCache {
        lookup_calls: Arc::new(AtomicUsize::new(0)),
        second_batch_started: Arc::clone(&second_batch_started),
        release_second_batch: Arc::clone(&release_second_batch),
    };
    let cancellation = ReadCancellationToken::new();
    let cancel_run = cancellation.clone();

    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        remote,
        true,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        limits,
        &config,
        cancel_run,
        &reporter,
        &artifacts,
        &cleaner,
        20,
    );
    let observe = async {
        tokio::time::timeout(Duration::from_secs(2), second_batch_started.notified())
            .await
            .expect("应进入第二个缓存查询批次");
        let details = registry.details(reporter.id()).await.unwrap();
        let lookup = details
            .stages
            .iter()
            .find(|stage| stage.stage_id == "lookup_base_cache")
            .expect("应发布基础缓存查询阶段");
        assert_eq!(lookup.completed, 1_000, "第一批查询完成后应立即更新进度");
        assert_eq!(lookup.total, 1_001);
        let persisted = observer
            .task_stages(task_id)
            .unwrap()
            .into_iter()
            .find(|stage| stage.stage_id == "lookup_base_cache")
            .expect("SQLite 应保存基础缓存查询阶段");
        assert_eq!(persisted.completed, 1_000, "每批进度还应持久化供重连恢复");
        cancellation.cancel();
        release_second_batch.notify_one();
    };

    let (result, ()) = tokio::join!(run, observe);
    assert!(matches!(
        result,
        Err(dedup_node_engine::scan::ScanError::Cancelled)
    ));
}

#[tokio::test]
async fn path_streaming_gate_preserves_worker_progress_before_next_lookup() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x63; 32]);
    let database = install_root.join("data/node/node.sqlite3");
    let mut store = NodeStore::open(&database, machine.clone()).unwrap();
    let observer = NodeStore::open(&database, machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = (0..=1_000)
        .map(|index| {
            let path = media_root.join(format!("streamed-path-{index}.bin"));
            ScannedPath::new(
                NormalizedPath::new(&path).unwrap(),
                DisplayPath::new(&path).unwrap(),
                10,
            )
        })
        .collect::<Vec<_>>();
    let (reader, hashes, started_hashes, active_leases) = counting_reader_for(&rows);
    let (mut pool, mut started_workers, controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "path 批次流水化")
        .await;
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let second_batch_started = Arc::new(Notify::new());
    let release_second_batch = Arc::new(Notify::new());
    let remote = GatedPathCache {
        lookup_calls: Arc::new(AtomicUsize::new(0)),
        second_batch_started: Arc::clone(&second_batch_started),
        release_second_batch: Arc::clone(&release_second_batch),
    };
    let cancellation = ReadCancellationToken::new();
    let task_text = task_id.as_uuid().to_string();
    let config = DiskReadConfig::default();
    let contact_root = cache_root.join("contact-sheets");

    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        remote,
        true,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        PipelineLimits::new(2, 2),
        &config,
        cancellation.clone(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    );
    let observe = async {
        tokio::time::timeout(Duration::from_secs(2), second_batch_started.notified())
            .await
            .expect("应进入被 gate 的第二个 path 批次");
        let item_id = tokio::time::timeout(Duration::from_secs(1), started_workers.recv())
            .await
            .expect("第二个 path 批次等待时，第一批 miss 应已 Hash 并派发 Worker")
            .expect("Worker 启动通道不应关闭")
            .1;
        let md5 = running_md5(&controller, &item_id, &hashes);
        controller
            .complete_base(task_text, item_id.clone(), md5, other_output())
            .await;
        tokio::time::timeout(Duration::from_secs(1), async {
            loop {
                let succeeded = observer
                    .task_items(task_id)
                    .unwrap()
                    .into_iter()
                    .any(|item| {
                        item.item_id == item_id && item.status == TaskItemStatus::Succeeded
                    });
                if succeeded {
                    break;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("Worker 终态应在第二个 path gate 释放前写入 SQLite");
        assert!(started_hashes.load(Ordering::Acquire) >= 1);
        assert_eq!(active_leases.load(Ordering::Acquire), 0);
        cancellation.cancel();
        release_second_batch.notify_one();
    };

    let (result, ()) = tokio::join!(run, observe);
    assert!(matches!(
        result,
        Err(dedup_node_engine::scan::ScanError::Cancelled)
    ));
}

/// 真实 BaseCompute 的 open-empty 必须保留 field25，publish 后才允许下一次 claim。
#[cfg(feature = "test-hooks")]
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn input_open_empty_claim_preserves_token_until_item_publish() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0xF7; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let media_root = install_root.join("media");
    let row_path = media_root.join("open-empty.bin");
    let row = ScannedPath::new(
        NormalizedPath::new(&row_path).unwrap(),
        DisplayPath::new(&row_path).unwrap(),
        10,
    );
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let hash_entered = Arc::new(Notify::new());
    let release_hash = Arc::new(Notify::new());
    let started_hashes = Arc::new(AtomicUsize::new(0));
    let active_leases = Arc::new(AtomicUsize::new(0));
    let reader = ScriptedHashReader {
        behaviors: Arc::new(BTreeMap::from([(
            row_path.clone(),
            ScriptedHashBehavior::SuccessAfterRelease {
                entered: Arc::clone(&hash_entered),
                release: Arc::clone(&release_hash),
                md5: [0xF7; 16],
            },
        )])),
        started: Arc::clone(&started_hashes),
        active_leases: Arc::clone(&active_leases),
    };
    let (mut pool, _started_workers, _controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "真实 open-empty claim")
        .await;
    let runtime_id = reporter.id().to_owned();
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let lookup_started = Arc::new(Notify::new());
    let release_lookup = Arc::new(Notify::new());
    let claim_attempts = Arc::new(AtomicUsize::new(0));
    let _claim_observer =
        BaseComputeEngine::install_claim_observer_for_test(Arc::clone(&claim_attempts));
    let contact_root = cache_root.join("contact-sheets");
    let config = DiskReadConfig::default();
    let cancellation = ReadCancellationToken::new();

    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        OpenEmptyPathCache {
            lookup_started: Arc::clone(&lookup_started),
            release_lookup: Arc::clone(&release_lookup),
        },
        true,
        task_id,
        options,
        vec![row],
        &contact_root,
        reader,
        PipelineLimits::new(1, 1),
        &config,
        cancellation.clone(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    );
    let observe = async {
        tokio::time::timeout(Duration::from_secs(1), lookup_started.notified())
            .await
            .expect("path 上游 gate 必须先进入 resolver");
        tokio::time::timeout(Duration::from_secs(1), async {
            loop {
                if claim_attempts.load(Ordering::Acquire) == 1 {
                    let details = registry.details(&runtime_id).await.unwrap();
                    let token = details
                        .pipeline_metrics
                        .as_ref()
                        .and_then(|metrics| metrics.hash_refill_token_available.as_ref())
                        .and_then(|metric| metric.current);
                    if token == Some(1) {
                        break;
                    }
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("open upstream 首次空 claim 后必须保留 field25 token");
        tokio::time::sleep(Duration::from_millis(40)).await;
        assert_eq!(claim_attempts.load(Ordering::Acquire), 1);
        release_lookup.notify_one();
        tokio::time::timeout(Duration::from_secs(1), async {
            loop {
                if started_hashes.load(Ordering::Acquire) == 1
                    && claim_attempts.load(Ordering::Acquire) == 2
                {
                    break;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("publish 后必须只恢复一次真实 item claim");
        let details = registry.details(&runtime_id).await.unwrap();
        assert_eq!(
            details
                .pipeline_metrics
                .unwrap()
                .hash_refill_token_available
                .unwrap()
                .current,
            Some(0)
        );
        cancellation.cancel();
        release_hash.notify_one();
    };

    let (summary, ()) = tokio::join!(run, observe);
    assert!(matches!(
        summary,
        Err(dedup_node_engine::scan::ScanError::Cancelled)
    ));
    assert_eq!(claim_attempts.load(Ordering::Acquire), 2);
    assert_eq!(active_leases.load(Ordering::Acquire), 0);
    let metrics = registry
        .details(&runtime_id)
        .await
        .unwrap()
        .pipeline_metrics
        .unwrap();
    assert_eq!(
        metrics.hash_refill_token_available.unwrap().current,
        Some(0)
    );
    assert_exact_phase_currents_are_zero(&metrics);
}

#[tokio::test]
async fn worker_crash_after_one_shot_dispatch_fails_one_file_and_task_completes() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("安装目录");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x72; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let media_root = install_root.join("媒体 库");
    let root = DisplayPath::new(&media_root).unwrap();
    let options = ScanOptions::new(vec![root]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let first_path = media_root.join("崩溃文件.mp4");
    let second_path = media_root.join("正常文件.bin");
    let rows = vec![
        ScannedPath::new(
            NormalizedPath::new(&first_path).unwrap(),
            DisplayPath::new(&first_path).unwrap(),
            4096,
        ),
        ScannedPath::new(
            NormalizedPath::new(&second_path).unwrap(),
            DisplayPath::new(&second_path).unwrap(),
            4096,
        ),
    ];
    let config = DiskReadConfig {
        total_threads: 1,
        hdd_threads_per_disk: 1,
        ssd_threads_per_disk: 1,
        unknown_threads_per_disk: 1,
        ..DiskReadConfig::default()
    };
    let (reader, hashes, active_media) = media_reader_for(
        &rows,
        vec![
            MediaPermitBehavior::Scheduled,
            MediaPermitBehavior::Scheduled,
        ],
        1,
    );
    let limits = PipelineLimits::new(2, 1);
    let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "基础计算")
        .await;
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let contact_root = cache_root.join("contact-sheets");
    let remote = DisabledRemoteFeatureCache;
    let task_text = task_id.as_uuid().to_string();

    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        remote,
        false,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        limits,
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    );
    let drive = async {
        let first_item = started.recv().await.unwrap().1;
        assert_eq!(active_media.load(Ordering::Acquire), 1);
        let crashed_path = controller
            .running_files()
            .into_iter()
            .find(|(_, item_id, _)| item_id == &first_item)
            .expect("Node 必须在 Worker 结束前保留当前文件路径")
            .2
            .display_path;
        controller
            .crash(task_text.clone(), first_item, "Worker 管道断开".into())
            .await;

        let second_item = tokio::time::timeout(Duration::from_secs(1), started.recv())
            .await
            .expect("补建槽位必须继续派发后续文件")
            .unwrap()
            .1;
        let second_md5 = running_md5(&controller, &second_item, &hashes);
        controller
            .complete_base(task_text, second_item, second_md5, other_output())
            .await;
        crashed_path
    };

    let (summary, crashed_path) = tokio::join!(run, drive);
    let summary = summary.unwrap();
    assert_eq!(summary.file_failures, 1);
    assert_eq!(
        store.task_snapshot(task_id).unwrap().status,
        TaskStatus::Completed
    );
    let items = store.task_items(task_id).unwrap();
    assert_eq!(
        items
            .iter()
            .filter(|item| item.status == TaskItemStatus::Failed)
            .count(),
        1
    );
    assert_eq!(
        items
            .iter()
            .filter(|item| item.status == TaskItemStatus::Succeeded)
            .count(),
        1
    );
    let fault = store.page_file_faults(None, 10).unwrap().items.remove(0);
    assert_eq!(fault.display_path, crashed_path);
    assert_eq!(fault.stage, "base_compute");
    assert_eq!(fault.kind, dedup_node_store::FileFaultKind::WorkerCrash);
    assert_eq!(
        active_media.load(Ordering::Acquire),
        0,
        "Worker 崩溃兜底必须释放媒体许可并补位后续文件"
    );
    let failures = registry.details(reporter.id()).await.unwrap().failures;
    assert_eq!(
        failures[0].display_path.as_str(),
        crashed_path.as_path().to_string_lossy().as_ref()
    );
}

#[tokio::test]
async fn wrong_worker_md5_is_logged_and_later_file_continues() {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x44; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let root = DisplayPath::new(install_root.join("media")).unwrap();
    let options = ScanOptions::new(vec![root]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = (1..=2)
        .map(|index| {
            let path = install_root.join(format!("media/file-{index}.bin"));
            ScannedPath::new(
                NormalizedPath::new(&path).unwrap(),
                DisplayPath::new(&path).unwrap(),
                10,
            )
        })
        .collect::<Vec<_>>();
    let config = DiskReadConfig {
        total_threads: 1,
        hdd_threads_per_disk: 1,
        ssd_threads_per_disk: 1,
        unknown_threads_per_disk: 1,
        ..DiskReadConfig::default()
    };
    let (reader, hashes, _, _) = counting_reader_for(&rows);
    let limits = PipelineLimits::new(2, 1);
    let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "基础计算")
        .await;
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let contact_root = cache_root.join("contact-sheets");
    let remote = DisabledRemoteFeatureCache;
    let task_text = task_id.as_uuid().to_string();

    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        remote,
        false,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        limits,
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    );
    let drive = async {
        let first = started.recv().await.unwrap().1;
        controller
            .complete_base(task_text.clone(), first, [0xEE; 16], other_output())
            .await;

        let second = tokio::time::timeout(Duration::from_secs(1), started.recv())
            .await
            .expect("单文件结果异常后应继续调度后续文件")
            .unwrap()
            .1;
        let second_md5 = running_md5(&controller, &second, &hashes);
        controller
            .complete_base(task_text, second, second_md5, other_output())
            .await;
    };

    let (summary, ()) = tokio::join!(run, drive);
    let summary = summary.unwrap();
    assert_eq!(summary.file_failures, 1);
    assert_eq!(
        store.task_snapshot(task_id).unwrap().status,
        TaskStatus::Completed
    );
    let items = store.task_items(task_id).unwrap();
    assert_eq!(
        items
            .iter()
            .filter(|item| item.status == TaskItemStatus::Failed)
            .count(),
        1
    );
    assert_eq!(
        items
            .iter()
            .filter(|item| item.status == TaskItemStatus::Succeeded)
            .count(),
        1
    );
    let failures = registry.details(reporter.id()).await.unwrap().failures;
    assert_eq!(failures.len(), 1);
    assert!(failures[0].display_path.ends_with("file-1.bin"));
}

/// 验证一个非法 V5 基础结果只失败当前项，随后合法结果仍可完成。
async fn assert_invalid_v5_output_fails_only_that_item(
    invalid_output: BaseComputeOutput,
    task_name: &str,
) {
    let fixture = tempdir().unwrap();
    let install_root = fixture.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x45; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let root = DisplayPath::new(install_root.join("media")).unwrap();
    let options = ScanOptions::new(vec![root]);
    let task_id = begin_scan_task(&mut store, &options, 10).unwrap();
    let rows = (1..=2)
        .map(|index| {
            let path = install_root.join(format!("media/empty-{index}.bin"));
            ScannedPath::new(
                NormalizedPath::new(&path).unwrap(),
                DisplayPath::new(&path).unwrap(),
                10,
            )
        })
        .collect::<Vec<_>>();
    let (reader, hashes, _, _) = counting_reader_for(&rows);
    let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, task_name)
        .await;
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let remote = DisabledRemoteFeatureCache;
    let config = DiskReadConfig::default();
    let task_text = task_id.as_uuid().to_string();
    let contact_root = cache_root.join("contact-sheets");

    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        remote,
        false,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        PipelineLimits::new(2, 1),
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        20,
    );
    let drive = async {
        let first = started.recv().await.unwrap().1;
        let first_md5 = running_md5(&controller, &first, &hashes);
        controller
            .complete_base(task_text.clone(), first, first_md5, invalid_output)
            .await;
        let second = started.recv().await.unwrap().1;
        let second_md5 = running_md5(&controller, &second, &hashes);
        controller
            .complete_base(task_text, second, second_md5, other_output())
            .await;
    };
    let (summary, ()) = tokio::join!(run, drive);
    let summary = summary.unwrap();

    assert_eq!(summary.file_failures, 1);
    let items = store.task_items(task_id).unwrap();
    assert_eq!(
        items
            .iter()
            .filter(|item| item.status == TaskItemStatus::Failed)
            .count(),
        1
    );
    assert_eq!(
        items
            .iter()
            .filter(|item| item.status == TaskItemStatus::Succeeded)
            .count(),
        1
    );
}

#[tokio::test]
async fn raw_empty_v5_payload_fails_only_that_missing_item_and_later_file_continues() {
    let output = BaseComputeOutput {
        probe: None,
        stage1_frames: None,
        contact_sheet_jpeg: None,
    };
    assert!(
        encode_base_compute_payload(&output).is_empty(),
        "测试输入必须形成原始空 payload"
    );
    assert_invalid_v5_output_fails_only_that_item(output, "原始空 V5 payload").await;
}

#[tokio::test]
async fn decoded_v5_payload_without_probe_fails_only_that_item_and_later_file_continues() {
    let output = BaseComputeOutput {
        probe: None,
        stage1_frames: Some(Vec::new()),
        contact_sheet_jpeg: None,
    };
    assert!(
        !encode_base_compute_payload(&output).is_empty(),
        "测试输入必须形成可解码的非空 payload"
    );
    assert_invalid_v5_output_fails_only_that_item(output, "非空但缺 probe 的 V5 payload").await;
}

/// 构造无需媒体特征写入的 Other 探测结果。
fn other_output() -> BaseComputeOutput {
    BaseComputeOutput {
        probe: Some(MediaProbe {
            media_kind: MediaKind::Other,
            width: 0,
            height: 0,
            duration_ms: None,
        }),
        stage1_frames: Some(Vec::new()),
        contact_sheet_jpeg: None,
    }
}

/// 构造六槽位完整且携带联系表 JPEG 的视频 Worker 结果。
fn video_output_with_contact_sheet() -> BaseComputeOutput {
    let feature = ImageStage1 {
        width: 320,
        height: 180,
        pdq: PdqHash::from_bytes([0x5A; 32]),
        quality: 88,
    };
    BaseComputeOutput {
        probe: Some(MediaProbe {
            media_kind: MediaKind::Video,
            width: 320,
            height: 180,
            duration_ms: Some(6_000),
        }),
        stage1_frames: Some(
            (0..6)
                .map(|slot| Stage1Frame {
                    slot,
                    feature: Some(feature),
                    error: None,
                })
                .collect(),
        ),
        contact_sheet_jpeg: Some(vec![0xFF, 0xD8, 0xFF, 0xD9]),
    }
}

/// 根据固定 MD5 计算联系表最终路径，供事务失败后的文件副作用断言。
fn contact_sheet_path(root: &Path, md5: [u8; 16]) -> PathBuf {
    let digest = md5
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect::<String>();
    root.join(&digest[..2]).join(format!("{digest}.jpg"))
}

/// 按固定文件名创建只读身份清单；测试不创建或修改真实媒体文件。
fn test_rows(media_root: &Path, names: &[&str]) -> Vec<ScannedPath> {
    names
        .iter()
        .map(|name| {
            let path = media_root.join(name);
            ScannedPath::new(
                NormalizedPath::new(&path).unwrap(),
                DisplayPath::new(&path).unwrap(),
                4_096,
            )
        })
        .collect()
}
