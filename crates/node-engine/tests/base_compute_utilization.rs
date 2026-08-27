//! 基础计算旧流水线的可重复性能夹具：观察缓存等待造成的 Worker 空闲和混合负载吞吐。

use std::{
    collections::BTreeMap,
    future::Future,
    path::Path,
    pin::Pin,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    time::{Duration, Instant},
};

use dedup_core::{ContentKey, DiskReadConfig, DisplayPath, MachineId, MediaKind, NormalizedPath};
use dedup_media_ffmpeg::MediaProbe;
use dedup_node_engine::{
    RemoteCacheError, RemoteFeatureCache,
    artifact_registry::RegenerableArtifactRegistry,
    disk_full_cleanup::{DiskFullCleaner, SystemArtifactDiskResolver},
    runtime_tasks::{RuntimeTaskKind, RuntimeTaskRegistry},
    scan::{
        BaseComputeEngine, PipelineFileReader, PipelineLimits, ReadProduct, ScanOptions,
        begin_scan_task,
    },
    worker::{BaseComputeOutput, ControlledWorkerPool, WorkerPool},
};
use dedup_node_store::{BaseCacheRecord, NodeStore, ScannedPath, TaskStatus};
use dedup_windows::ReadCancellationToken;
use tempfile::tempdir;
#[cfg(test)]
use tokio::sync::Notify;
use tokio::sync::{OwnedSemaphorePermit, Semaphore};

/// 固定清单的种子；该夹具不调用随机数，只用它标识可复现输入集。
pub const MIXED_WORKLOAD_SEED: u64 = 0x2026_08_23_C0DE_0000;
/// 混合清单中的文件总数：路径缓存命中 1 个，Hash 后内容缓存命中 1 个，缓存缺失 2 个。
pub const MIXED_WORKLOAD_FILES: usize = 4;
/// 基准运行中固定模拟的路径缓存等待时间。
pub const BENCH_PATH_CACHE_WAIT: Duration = Duration::from_millis(25);
/// 基准运行中固定模拟的 Worker 媒体解码等待时间。
pub const BENCH_DECODE_WAIT: Duration = Duration::from_millis(6);
/// 测试专用的远程 ACK 等待，用来证明测量终点必须晚于真实 SQLite 完成。
#[cfg(test)]
const COMPLETION_ACK_WAIT: Duration = Duration::from_millis(80);

/// 旧架构一次混合负载运行的可比较观测值。
#[derive(Clone, Debug)]
pub struct BaselineMetrics {
    /// 固定随机种子，便于报告与后续实现使用同一清单。
    pub seed: u64,
    /// 固定清单总文件数。
    pub total_files: usize,
    /// 真实 `ScanSummary` 统计出的缓存命中数。
    pub cache_hits: usize,
    /// 实际由 Node Hash reader 完成的文件数量。
    pub hash_sessions: usize,
    /// 实际需要媒体探测/一筛的文件数量。
    pub media_decode_jobs: usize,
    /// 可控路径缓存从开始到完成的实际等待时间。
    pub cache_wait: Duration,
    /// 路径缓存开始后，到第一个 Worker 会话开始前的 Worker 空闲时间。
    pub worker_idle_before_hash: Duration,
    /// 第一个 Worker 续算开始到引擎返回并确认 SQLite 任务完成的阶段跨度。
    pub decode_and_persist: Duration,
    /// 夹具总墙钟时间。
    pub elapsed: Duration,
    /// 总文件数除以总墙钟时间得到的吞吐（文件/秒）。
    pub throughput_files_per_second: f64,
    /// 路径缓存等待期间是否没有发生 Worker 派发。
    pub worker_idle_while_cache_waits: bool,
    /// SQLite 最终任务是否真实完成。
    pub persisted_completed: bool,
    /// 两个缓存缺失 Worker 启动后实际持有的媒体读取许可数。
    pub media_active_before_source_complete: usize,
    /// Worker 仍在 CPU 尾段时实际持有的媒体读取许可数。
    pub media_active_during_cpu_tail: usize,
    /// 释放媒体许可后仍处于运行态的 Worker 数。
    pub busy_workers_during_cpu_tail: usize,
    /// 源读取完成后、终态前仍由 WorkerPool 持有的 CPU 权重。
    pub cpu_in_use_during_cpu_tail: usize,
    /// 两个 Worker 终态均处理后归还的 CPU 权重。
    pub cpu_in_use_after_terminal: usize,
    /// 运行结束时阶段 ownership 与 Task10 credit 是否全部发布为零。
    pub phase_ownership_cleared: bool,
    /// 成功 claim 到 Applied ACK 的 item latency 样本数。
    pub item_completion_samples: u64,
}

/// 控制路径缓存何时返回，用于固定等待或精确观察旧架构的阶段阻塞。
enum CacheWait {
    /// 基准使用的固定时长等待。
    Fixed(Duration),
    /// 测试使用的显式闸门，只有释放通知到达后才返回缓存结果。
    #[cfg(test)]
    #[allow(dead_code)]
    Gated {
        /// 缓存查询已进入等待的通知。
        entered: Arc<Notify>,
        /// 测试放行缓存查询的通知。
        release: Arc<Notify>,
    },
}

/// 可控远程缓存：路径缓存命中一项，Hash 后内容缓存命中一项，其余均未命中。
struct MixedRemoteCache {
    /// 路径缓存的等待策略。
    wait: CacheWait,
    /// `lookup_paths` 的开始/结束时间，用于计算实际缓存阶段占用。
    path_lookup_window: Arc<Mutex<Option<(Instant, Instant)>>>,
    /// 内容缓存查询次数，保证真实 Hash 后缓存路径被执行。
    content_lookup_calls: Arc<AtomicUsize>,
    /// 在最终 outbox ACK 前固定等待，模拟引擎完成 SQLite 后仍需消费的收尾边界。
    completion_ack_wait: Duration,
}

/// 按 MD5 返回未知、已知图片或已知视频缺失缓存的线程策略测试缓存。
struct ThreadPolicyRemoteCache;

impl RemoteFeatureCache for ThreadPolicyRemoteCache {
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
            .map(|key| match key.md5()[0] {
                0x22 => Some(incomplete_media_record(key.clone(), MediaKind::Image)),
                0x33 => Some(incomplete_media_record(key.clone(), MediaKind::Video)),
                _ => None,
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

/// 构造仅冻结媒体类型、仍需 Worker 完成基础字段的缓存记录。
fn incomplete_media_record(content_key: ContentKey, media_kind: MediaKind) -> BaseCacheRecord {
    BaseCacheRecord {
        content_id: None,
        content_key,
        media_kind,
        base_complete: false,
        width: None,
        height: None,
        duration_ms: None,
        stage1: None,
    }
}

impl MixedRemoteCache {
    /// 创建使用给定路径缓存等待策略的可控缓存。
    fn new(wait: CacheWait, completion_ack_wait: Duration) -> Self {
        Self {
            wait,
            path_lookup_window: Arc::new(Mutex::new(None)),
            content_lookup_calls: Arc::new(AtomicUsize::new(0)),
            completion_ack_wait,
        }
    }

    /// 返回名字和大小一致的完整 Other 缓存记录。
    fn cached_other(md5: [u8; 16], file_size: u64) -> BaseCacheRecord {
        BaseCacheRecord {
            content_id: None,
            content_key: ContentKey::new(md5, file_size),
            media_kind: MediaKind::Other,
            base_complete: true,
            width: None,
            height: None,
            duration_ms: None,
            stage1: None,
        }
    }
}

impl RemoteFeatureCache for MixedRemoteCache {
    async fn lookup_paths(
        &self,
        _machine_id: &MachineId,
        paths: &[ScannedPath],
    ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
        // 路径缓存是当前旧架构计算阶段之前的独占等待点。
        let started = Instant::now();
        match &self.wait {
            CacheWait::Fixed(wait) => tokio::time::sleep(*wait).await,
            #[cfg(test)]
            CacheWait::Gated { entered, release } => {
                entered.notify_one();
                release.notified().await;
            }
        }
        let finished = Instant::now();
        *self.path_lookup_window.lock().unwrap() = Some((started, finished));
        Ok(paths
            .iter()
            .map(|path| {
                let name = path
                    .display_path
                    .as_path()
                    .file_name()
                    .and_then(|name| name.to_str());
                (name == Some("cached-small.bin"))
                    .then(|| Self::cached_other([0xA1; 16], path.file_size))
            })
            .collect())
    }

    async fn lookup_contents(
        &self,
        keys: &[ContentKey],
    ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
        self.content_lookup_calls.fetch_add(1, Ordering::SeqCst);
        Ok(keys
            .iter()
            .map(|key| {
                (key.md5() == [0xB2; 16]).then(|| Self::cached_other([0xB2; 16], key.file_size()))
            })
            .collect())
    }

    async fn publish_outbox(
        &mut self,
        _machine_id: &MachineId,
        batch: &dedup_protocol::proto::SyncChangeBatch,
    ) -> Result<u64, RemoteCacheError> {
        // 真实远程边界只在确认该批次的最高序号后，Node 才能推进 outbox 游标。
        tokio::time::sleep(self.completion_ack_wait).await;
        Ok(batch.high_seq)
    }
}

/// 固定混合清单使用的 Node Hash reader；不访问真实媒体。
#[derive(Clone)]
struct BaselineHashReader {
    /// 每个冻结路径对应的预期 MD5。
    hashes: Arc<BTreeMap<std::path::PathBuf, [u8; 16]>>,
    /// 两个媒体 miss 可并发取得的独立媒体槽位。
    media_slots: Arc<Semaphore>,
    /// 当前由 Worker 活动项持有的媒体许可数。
    active_media: Arc<AtomicUsize>,
}

/// 混合夹具中区分 Hash 空许可和媒体计数许可的关联类型。
enum BaselineLease {
    /// Hash 读取完成后立即释放，不计入媒体占用。
    Hash,
    /// Worker 源读取完成前持有的媒体许可。
    Media {
        /// Drop 时归还的共享媒体槽位。
        _slot: OwnedSemaphorePermit,
        /// Drop 时递减的活动媒体计数。
        active: Arc<AtomicUsize>,
    },
}

impl Drop for BaselineLease {
    fn drop(&mut self) {
        if let Self::Media { active, .. } = self {
            active.fetch_sub(1, Ordering::AcqRel);
        }
    }
}

impl PipelineFileReader for BaselineHashReader {
    type Lease = BaselineLease;

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
                lease: BaselineLease::Hash,
            })
        })
    }

    fn acquire_media_permit(
        &self,
        _scanned: ScannedPath,
        cancellation: ReadCancellationToken,
    ) -> Pin<
        Box<
            dyn Future<Output = Result<Option<Self::Lease>, dedup_node_engine::io::ReadFailure>>
                + Send
                + 'static,
        >,
    > {
        let slots = Arc::clone(&self.media_slots);
        let active = Arc::clone(&self.active_media);
        Box::pin(async move {
            let slot = slots.acquire_owned().await.map_err(|_| {
                dedup_node_engine::io::ReadFailure::Io {
                    path: std::path::PathBuf::from("mixed-media-permit"),
                    block_offset: 0,
                    source: std::io::Error::other("混合夹具媒体许可调度器已关闭"),
                }
            })?;
            if cancellation.is_cancelled() {
                return Err(dedup_node_engine::io::ReadFailure::Cancelled);
            }
            active.fetch_add(1, Ordering::AcqRel);
            Ok(Some(BaselineLease::Media {
                _slot: slot,
                active,
            }))
        })
    }
}

/// 运行固定混合清单，供 bench 输出同机可比较的旧架构原始指标。
pub async fn run_mixed_baseline() -> BaselineMetrics {
    run_fixture(CacheWait::Fixed(BENCH_PATH_CACHE_WAIT), Duration::ZERO).await
}

/// 验证缓存等待时 Worker 槽位保持空闲，再完整跑完同一固定混合清单。
#[cfg(test)]
#[allow(dead_code)]
async fn run_gated_fixture() -> BaselineMetrics {
    let entered = Arc::new(Notify::new());
    let release = Arc::new(Notify::new());
    run_fixture(CacheWait::Gated { entered, release }, COMPLETION_ACK_WAIT).await
}

/// 装配真实基础计算引擎、真实 SQLite 和可控的缓存/Worker 边界。
async fn run_fixture(wait: CacheWait, completion_ack_wait: Duration) -> BaselineMetrics {
    // 临时安装根仅保存 SQLite 与可再生缓存；不创建或改写真实媒体。
    let directory = tempdir().unwrap();
    let install_root = directory.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x4D; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let media_root = install_root.join("readonly-media-fixture");
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 100).unwrap();
    // 该顺序固定表达小文件、大文件、路径命中、内容命中与缓存缺失。
    let rows = mixed_rows(&media_root);
    let config = DiskReadConfig {
        total_threads: 2,
        hdd_threads_per_disk: 2,
        ssd_threads_per_disk: 2,
        unknown_threads_per_disk: 2,
        ..DiskReadConfig::default()
    };
    let active_media = Arc::new(AtomicUsize::new(0));
    let reader = BaselineHashReader {
        hashes: Arc::new(
            rows.iter()
                .filter_map(|row| {
                    hash_for_path(row.display_path.as_path())
                        .map(|md5| (row.display_path.as_path().to_path_buf(), md5))
                })
                .collect(),
        ),
        media_slots: Arc::new(Semaphore::new(2)),
        active_media: Arc::clone(&active_media),
    };
    let limits = PipelineLimits::new(4, 2);
    let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(2);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "Task 0 基础计算基线")
        .await;
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let contact_root = cache_root.join("contact-sheets");
    // 将闸门通知复制给驱动器，避免它同时借用正在被引擎使用的远程缓存。
    #[cfg(test)]
    let cache_gate = match &wait {
        CacheWait::Fixed(_) => None,
        CacheWait::Gated { entered, release } => Some((Arc::clone(entered), Arc::clone(release))),
    };
    #[cfg(not(test))]
    let cache_gate: Option<()> = None;
    let remote = MixedRemoteCache::new(wait, completion_ack_wait);
    let path_lookup_window = Arc::clone(&remote.path_lookup_window);
    let content_lookup_calls = Arc::clone(&remote.content_lookup_calls);
    let task_text = task_id.as_uuid().to_string();
    let overall_started = Instant::now();
    let cache_blocked_worker = Arc::new(AtomicBool::new(false));
    let driver_blocked_worker = Arc::clone(&cache_blocked_worker);

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
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        100,
    );
    let drive = async {
        #[cfg(test)]
        if let Some((entered, release)) = cache_gate {
            // 先等待真实 `lookup_paths` 进入，再证明这个等待点尚未派发任何 Hash 会话。
            entered.notified().await;
            let started_while_waiting =
                tokio::time::timeout(Duration::from_millis(30), started.recv()).await;
            if started_while_waiting.is_err() {
                driver_blocked_worker.store(true, Ordering::SeqCst);
            } else {
                return Err("路径缓存等待期间意外派发了 Worker Hash".to_owned());
            }
            release.notify_one();
        }
        #[cfg(not(test))]
        let _ = cache_gate;
        drive_mixed_worker(
            &mut started,
            &controller,
            task_text,
            Arc::clone(&active_media),
        )
        .await
    };

    let (summary, driver) = tokio::join!(run, drive);
    let driver = driver.unwrap();
    let summary = summary.unwrap();
    assert!(
        content_lookup_calls.load(Ordering::SeqCst) >= 1,
        "至少一个 HashReady 批次应进入真实内容缓存查询"
    );
    let (cache_started, cache_finished) = path_lookup_window
        .lock()
        .unwrap()
        .expect("路径缓存应记录实际等待窗口");
    let persisted_completed = store.task_snapshot(task_id).unwrap().status == TaskStatus::Completed;
    assert!(persisted_completed, "真实 SQLite 任务必须完成");
    // 只有引擎完成事件消费、SQLite 写入、最终 outbox ACK 并返回后，才冻结持久化终点。
    let persistence_completed_at = Instant::now();
    let elapsed = overall_started.elapsed();
    assert_eq!(
        summary.cache_hits, 1,
        "路径缓存命中仅计入 ScanSummary.cache_hits"
    );
    assert_eq!(summary.reused_contents, 1, "Hash 后内容缓存命中单独计数");
    assert_eq!(summary.hashed, 3, "只有路径命中项不进入 Worker Hash");
    assert_eq!(summary.scheduled_stage1, 2, "内容缓存命中项不触发媒体解码");
    let details = registry
        .details(reporter.id())
        .await
        .expect("运行结束后必须能读取基础计算详情");
    let metrics = details
        .pipeline_metrics
        .expect("基础计算必须发布阶段 ownership 详情");
    let worker_capacity = details
        .execution_config
        .as_ref()
        .and_then(|config| config.worker_slots)
        .expect("运行时必须发布 Worker 容量") as u64;
    let decode_capacity = worker_capacity
        .checked_mul(2)
        .expect("测试 Worker 容量不应溢出 2W");
    assert_eq!(
        details
            .execution_config
            .as_ref()
            .and_then(|config| config.decode_queue_capacity),
        Some(decode_capacity as u32),
        "运行时 decode_queue_capacity 必须精确为 2W"
    );
    assert_eq!(
        metrics
            .decode_credit_owned
            .as_ref()
            .map(|value| value.capacity),
        Some(Some(decode_capacity)),
        "字段26 capacity 必须精确绑定 2W"
    );
    let phase_ownership_cleared = [
        metrics.hash_waiting_permit.as_ref(),
        metrics.hash_reading.as_ref(),
        metrics.hash_completed_unjoined.as_ref(),
        metrics.media_permit_waiting.as_ref(),
        metrics.media_acquire_ready.as_ref(),
        metrics.media_permit_ready.as_ref(),
        metrics.worker_dispatching.as_ref(),
        metrics.worker_start_pending.as_ref(),
        metrics.worker_decode.as_ref(),
        metrics.worker_feature.as_ref(),
        metrics.worker_result_wait.as_ref(),
        metrics.worker_phase_unknown.as_ref(),
    ]
    .into_iter()
    .all(|metric| metric.is_some_and(|value| value.current == Some(0)))
        && metrics
            .content_output_credit_owned
            .as_ref()
            .is_some_and(|value| value.current == Some(0))
        && metrics
            .hash_refill_token_available
            .as_ref()
            .is_some_and(|value| value.current == Some(0))
        && metrics
            .decode_credit_owned
            .as_ref()
            .is_some_and(|value| value.current == Some(0));
    let item_completion_samples = metrics
        .item_completion_latency
        .as_ref()
        .map_or(0, |histogram| histogram.count);
    BaselineMetrics {
        seed: MIXED_WORKLOAD_SEED,
        total_files: MIXED_WORKLOAD_FILES,
        cache_hits: summary.cache_hits + summary.reused_contents,
        hash_sessions: driver.hash_sessions,
        media_decode_jobs: summary.scheduled_stage1,
        cache_wait: cache_finished.duration_since(cache_started),
        worker_idle_before_hash: driver.first_worker_started.duration_since(cache_started),
        decode_and_persist: persistence_completed_at.duration_since(driver.first_decode_started),
        elapsed,
        throughput_files_per_second: MIXED_WORKLOAD_FILES as f64 / elapsed.as_secs_f64(),
        worker_idle_while_cache_waits: cache_blocked_worker.load(Ordering::SeqCst),
        persisted_completed,
        media_active_before_source_complete: driver.media_active_before_source_complete,
        media_active_during_cpu_tail: driver.media_active_during_cpu_tail,
        busy_workers_during_cpu_tail: driver.busy_workers_during_cpu_tail,
        cpu_in_use_during_cpu_tail: driver.cpu_in_use_during_cpu_tail,
        cpu_in_use_after_terminal: driver.cpu_in_use_after_terminal,
        phase_ownership_cleared,
        item_completion_samples,
    }
}

/// 组装固定文件清单；路径只是身份，不对应真实文件。
fn mixed_rows(media_root: &Path) -> Vec<ScannedPath> {
    [
        ("cached-small.bin", 4 * 1024_u64),
        ("small-miss.bin", 8 * 1024_u64),
        ("large-content-hit.bin", 64 * 1024 * 1024_u64),
        ("large-miss.bin", 96 * 1024 * 1024_u64),
    ]
    .into_iter()
    .map(|(name, size)| {
        let path = media_root.join(name);
        ScannedPath::new(
            NormalizedPath::new(&path).unwrap(),
            DisplayPath::new(&path).unwrap(),
            size,
        )
    })
    .collect()
}

/// 可控 Worker 运行中一次会话调度的边界时间与数量。
struct WorkerDriverMetrics {
    /// Node Hash reader 真正处理的文件数。
    hash_sessions: usize,
    /// 第一个 Worker 任务开始的时刻。
    first_worker_started: Instant,
    /// 第一个一次性 Worker 作业开始解码的时刻。
    first_decode_started: Instant,
    /// 两个 Worker 启动后、源读取完成事件前的媒体许可数。
    media_active_before_source_complete: usize,
    /// 固定 CPU 尾段期间的媒体许可数。
    media_active_during_cpu_tail: usize,
    /// 固定 CPU 尾段期间仍运行的 Worker 数。
    busy_workers_during_cpu_tail: usize,
    /// 固定 CPU 尾段期间仍登记的 CPU 权重。
    cpu_in_use_during_cpu_tail: usize,
    /// 终态处理完毕后剩余的 CPU 权重。
    cpu_in_use_after_terminal: usize,
}

/// 驱动真实 WorkerPool 的控制边界：只为两个缓存缺失项返回一次性媒体结果。
async fn drive_mixed_worker(
    started: &mut tokio::sync::mpsc::Receiver<(String, String)>,
    controller: &ControlledWorkerPool,
    task_text: String,
    active_media: Arc<AtomicUsize>,
) -> Result<WorkerDriverMetrics, String> {
    let mut pending = Vec::new();
    // 以第一个真实 Started 通道项计时，而不是缓存放行后驱动器开始等待的时刻。
    let mut first_worker_started = None;
    while pending.len() < 2 {
        let (_, item_id) = tokio::time::timeout(Duration::from_secs(1), started.recv())
            .await
            .map_err(|_| "Worker 未在缓存判定后开始一次性媒体计算".to_owned())?
            .ok_or_else(|| "可控 Worker 启动通道提前关闭".to_owned())?;
        first_worker_started.get_or_insert_with(Instant::now);
        let md5 = hash_for_item(controller, &item_id)?;
        pending.push((item_id, md5));
    }
    let first_decode_started = Instant::now();
    let media_active_before_source_complete = active_media.load(Ordering::Acquire);
    for (item_id, _) in &pending {
        controller
            .base_source_read_complete(task_text.clone(), item_id.clone())
            .await;
    }
    tokio::time::timeout(Duration::from_secs(1), async {
        while active_media.load(Ordering::Acquire) != 0 {
            tokio::task::yield_now().await;
        }
    })
    .await
    .map_err(|_| "SourceReadComplete 后媒体许可未及时归还".to_owned())?;
    let media_active_during_cpu_tail = active_media.load(Ordering::Acquire);
    let busy_workers_during_cpu_tail = controller.running_files().len();
    let cpu_in_use_during_cpu_tail = controller.cpu_in_use();
    tokio::time::sleep(BENCH_DECODE_WAIT).await;
    for (item_id, md5) in pending {
        controller
            .complete_base(task_text.clone(), item_id, md5, other_output())
            .await;
    }
    tokio::time::timeout(Duration::from_secs(1), async {
        while controller.cpu_in_use() != 0 {
            tokio::task::yield_now().await;
        }
    })
    .await
    .map_err(|_| "Worker 终态后 CPU 权重未及时归还".to_owned())?;
    let cpu_in_use_after_terminal = controller.cpu_in_use();
    Ok(WorkerDriverMetrics {
        hash_sessions: 3,
        first_worker_started: first_worker_started.expect("至少应收到一个一次性 Worker 作业"),
        first_decode_started,
        media_active_before_source_complete,
        media_active_during_cpu_tail,
        busy_workers_during_cpu_tail,
        cpu_in_use_during_cpu_tail,
        cpu_in_use_after_terminal,
    })
}

/// 根据运行项的真实冻结路径返回固定 Hash，避免依赖派发顺序。
fn hash_for_item(controller: &ControlledWorkerPool, item_id: &str) -> Result<[u8; 16], String> {
    let running = controller.running_files();
    let (_, _, identity) = running
        .into_iter()
        .find(|(_, running_item_id, _)| running_item_id == item_id)
        .ok_or_else(|| format!("未找到运行项 {item_id} 的真实文件身份"))?;
    hash_for_path(identity.display_path.as_path())
        .ok_or_else(|| format!("收到非混合清单的运行项: {:?}", identity.display_path))
}

/// 根据混合清单文件名返回固定 Node MD5。
fn hash_for_path(path: &Path) -> Option<[u8; 16]> {
    let name = path.file_name().and_then(|name| name.to_str());
    match name {
        Some("small-miss.bin") => Some([0xC3; 16]),
        Some("large-content-hit.bin") => Some([0xB2; 16]),
        Some("large-miss.bin") => Some([0xD4; 16]),
        _ => None,
    }
}

/// 构造 Other 媒体的完整基础结果，让真实 SQLite 持久化路径执行。
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

/// 验证夹具确实观测到旧架构在路径缓存等待期间的 Worker 空闲边界。
#[tokio::test]
async fn mixed_baseline_fixture_observes_cache_blocked_worker_idle_and_persistence() {
    let metrics = run_gated_fixture().await;

    assert_eq!(metrics.seed, MIXED_WORKLOAD_SEED);
    assert_eq!(metrics.total_files, MIXED_WORKLOAD_FILES);
    assert_eq!(metrics.cache_hits, 2);
    assert_eq!(metrics.hash_sessions, 3);
    assert_eq!(metrics.media_decode_jobs, 2);
    assert!(
        metrics.worker_idle_while_cache_waits,
        "缓存闸门期间不应派发 Worker Hash"
    );
    assert!(metrics.worker_idle_before_hash >= metrics.cache_wait);
    assert!(
        metrics.persisted_completed,
        "所有结果必须经真实 SQLite 完成"
    );
    assert!(
        metrics.decode_and_persist >= COMPLETION_ACK_WAIT,
        "解码/持久化跨度必须覆盖引擎完成 SQLite 后的最终 ACK 收尾"
    );
    assert!(
        metrics.phase_ownership_cleared,
        "基础计算结束后 12–23 阶段 ownership 必须全部归零"
    );
    assert_eq!(
        metrics.item_completion_samples, 3,
        "只有成功 claim 的三项才应产生 item latency；path reserve 不应计时"
    );
}

#[tokio::test]
async fn source_read_complete_releases_media_but_keeps_cpu_until_terminal() {
    let metrics = run_gated_fixture().await;

    assert_eq!(metrics.seed, MIXED_WORKLOAD_SEED);
    assert_eq!(metrics.total_files, MIXED_WORKLOAD_FILES);
    assert_eq!(metrics.cache_hits, 2);
    assert_eq!(metrics.hash_sessions, 3);
    assert_eq!(metrics.media_decode_jobs, 2);
    assert_eq!(
        metrics.media_active_before_source_complete, 2,
        "两个缓存 miss Worker 启动后必须各自持有媒体读取许可"
    );
    assert_eq!(
        metrics.busy_workers_during_cpu_tail, 2,
        "源读取完成后两个 Worker 应继续执行 CPU 尾段"
    );
    assert_eq!(
        metrics.media_active_during_cpu_tail, 0,
        "CPU 特征计算尾段不得继续占用媒体读取许可"
    );
    assert_eq!(
        metrics.cpu_in_use_during_cpu_tail, 2,
        "SourceReadComplete 后两个 Worker 的 CPU 权重必须保留到终态"
    );
    assert_eq!(metrics.cpu_in_use_after_terminal, 0);
    assert!(metrics.persisted_completed);
}

#[tokio::test]
async fn base_compute_assigns_one_thread_to_unknown_and_image_and_effective_threads_to_known_video()
{
    let directory = tempdir().unwrap();
    let install_root = directory.path().join("install");
    let cache_root = install_root.join("data/node/cache");
    std::fs::create_dir_all(&cache_root).unwrap();
    let machine = MachineId::from_sha256([0x5A; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let media_root = install_root.join("readonly-thread-policy");
    let options = ScanOptions::new(vec![DisplayPath::new(&media_root).unwrap()]);
    let task_id = begin_scan_task(&mut store, &options, 3).unwrap();
    let rows = [
        ("unknown.bin", 0x11_u8),
        ("image.bin", 0x22_u8),
        ("video.bin", 0x33_u8),
    ]
    .into_iter()
    .map(|(name, marker)| {
        let path = media_root.join(name);
        (
            ScannedPath::new(
                NormalizedPath::new(&path).unwrap(),
                DisplayPath::new(&path).unwrap(),
                4_096,
            ),
            marker,
        )
    })
    .collect::<Vec<_>>();
    let hashes = Arc::new(
        rows.iter()
            .map(|(row, marker)| (row.display_path.as_path().to_path_buf(), [*marker; 16]))
            .collect(),
    );
    let active_media = Arc::new(AtomicUsize::new(0));
    let reader = BaselineHashReader {
        hashes,
        media_slots: Arc::new(Semaphore::new(3)),
        active_media,
    };
    let rows = rows.into_iter().map(|(row, _)| row).collect::<Vec<_>>();
    let (mut pool, mut started, controller) =
        WorkerPool::controlled_batch_with_cpu_budget_for_test(2, 8);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine, "Task 5 解码线程策略")
        .await;
    let artifacts = Arc::new(RegenerableArtifactRegistry::new(&install_root, &cache_root).unwrap());
    let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
    let config = DiskReadConfig::default();
    let task_text = task_id.as_uuid().to_string();
    let contact_root = cache_root.join("contact-sheets");

    let run = BaseComputeEngine::run_existing(
        &mut store,
        &mut pool,
        ThreadPolicyRemoteCache,
        true,
        task_id,
        options,
        rows,
        &contact_root,
        reader,
        PipelineLimits::new(3, 2),
        &config,
        ReadCancellationToken::new(),
        &reporter,
        &artifacts,
        &cleaner,
        30,
    );
    let drive = async {
        let mut commands = BTreeMap::new();
        while commands.len() < 3 {
            let (_, item_id) = tokio::time::timeout(Duration::from_secs(1), started.recv())
                .await
                .expect("三种媒体线程策略都应派发")
                .expect("Worker 启动通道不应提前关闭");
            let command = controller
                .started_base_commands()
                .into_iter()
                .find(|command| command.item_id == item_id)
                .expect("Started 必须保留对应 ComputeBaseFeatures 命令");
            let name = Path::new(&command.display_path)
                .file_name()
                .unwrap()
                .to_string_lossy()
                .into_owned();
            commands.insert(name, command.decoder_threads);
            controller
                .crash(task_text.clone(), item_id, "线程策略测试结束运行项".into())
                .await;
        }
        commands
    };

    let (summary, commands) = tokio::join!(run, drive);
    assert_eq!(summary.unwrap().scheduled_stage1, 3);
    assert_eq!(commands["unknown.bin"], 1);
    assert_eq!(commands["image.bin"], 1);
    assert_eq!(commands["video.bin"], 4, "8预算/2可活动Worker应钳制为4线程");
}
