use std::{
    collections::BTreeSet,
    fs,
    future::Future,
    path::Path,
    pin::Pin,
    sync::{
        Arc, Condvar, Mutex,
        atomic::{AtomicU64, Ordering},
    },
    time::{Duration, Instant},
};

use dedup_core::{DisplayPath, MachineId, MediaKind, NormalizedPath};
use dedup_node_engine::{
    io::{BlockReadError, BlockReader, ReadFailure},
    runtime_tasks::{
        RuntimeExecutionConfigUpdate, RuntimeProgressUnit, RuntimeStage, RuntimeTaskClock,
        RuntimeTaskKind, RuntimeTaskRegistry, RuntimeWorkerUpdate,
    },
    scan::{
        FileEnumerator, PipelineFileReader, PipelineLimits, ReadProduct, ScanEngine, ScanError,
        ScanOptions, ScheduledFileReader, Stage1Processor, Stage1Request, SystemMd5,
        WorkerPoolStage1Processor, begin_scan_task, md5_bytes,
    },
    worker::{Stage1Output, WorkerPool},
};
use dedup_node_store::{NodeStore, ScannedPath, TaskStatus};
use dedup_windows::{LocalDiskKind, ReadCancellationToken};

#[derive(Clone, Default)]
struct ReadGate(Arc<(Mutex<GateState>, Condvar)>);
#[derive(Default)]
struct GateState {
    started: BTreeSet<String>,
    released: BTreeSet<String>,
    all_released: bool,
}
impl ReadGate {
    /// 在有界时间内等待文件进入真实块读取，避免失败断言遗留永不结束的阻塞线程。
    fn wait_started_for(&self, path: &Path, timeout: Duration) -> bool {
        let key = path.file_name().unwrap().to_string_lossy().into_owned();
        let deadline = Instant::now() + timeout;
        let (lock, cv) = &*self.0;
        let mut state = lock.lock().unwrap();
        while !state.started.contains(&key) {
            let Some(remaining) = deadline.checked_duration_since(Instant::now()) else {
                return false;
            };
            let (next_state, result) = cv.wait_timeout(state, remaining).unwrap();
            state = next_state;
            if result.timed_out() && !state.started.contains(&key) {
                return false;
            }
        }
        true
    }

    /// 返回当前已经进入真实块读取的文件名，帮助定位调度或测试门未推进的边界。
    fn started_keys(&self) -> Vec<String> {
        self.0.0.lock().unwrap().started.iter().cloned().collect()
    }

    fn release(&self, path: &Path) {
        let (lock, cv) = &*self.0;
        lock.lock()
            .unwrap()
            .released
            .insert(path.file_name().unwrap().to_string_lossy().into_owned());
        cv.notify_all();
    }
    fn release_all(&self) {
        let (lock, cv) = &*self.0;
        lock.lock().unwrap().all_released = true;
        cv.notify_all();
    }
}
impl BlockReader for ReadGate {
    fn read_at(
        &self,
        path: &Path,
        offset: u64,
        buffer: &mut [u8],
        _: Duration,
        _: &ReadCancellationToken,
    ) -> Result<usize, BlockReadError> {
        let (lock, cv) = &*self.0;
        let mut state = lock.lock().unwrap();
        let key = path.file_name().unwrap().to_string_lossy().into_owned();
        state.started.insert(key.clone());
        cv.notify_all();
        while !state.all_released && !state.released.contains(&key) {
            state = cv.wait(state).unwrap();
        }
        drop(state);
        let data = fs::read(path).map_err(BlockReadError::Io)?;
        let offset = offset as usize;
        if offset >= data.len() {
            return Ok(0);
        }
        let len = buffer.len().min(data.len() - offset);
        buffer[..len].copy_from_slice(&data[offset..offset + len]);
        Ok(len)
    }
}

#[derive(Clone)]
struct Rows(Vec<ScannedPath>);
impl FileEnumerator for Rows {
    fn enumerate(&self, _: &[DisplayPath]) -> Result<Vec<ScannedPath>, ScanError> {
        Ok(self.0.clone())
    }

    /// 模拟 Everything 已拿到完整清单后先报告总数，再进入有界交付。
    fn enumerate_into_with_completion(
        &self,
        _: &[DisplayPath],
        complete: &mut dyn FnMut(Option<(u64, u64)>) -> Result<(), ScanError>,
        emit: &mut dyn FnMut(ScannedPath) -> Result<(), ScanError>,
    ) -> Result<(), ScanError> {
        let total_bytes = self
            .0
            .iter()
            .fold(0_u64, |total, row| total.saturating_add(row.file_size));
        complete(Some((self.0.len() as u64, total_bytes)))?;
        for row in self.0.clone() {
            emit(row)?;
        }
        Ok(())
    }
}

/// 手工推进扫描阶段耗时的测试单调时钟。
#[derive(Default)]
struct ManualClock(AtomicU64);

impl ManualClock {
    /// 推进测试观察到的单调时间。
    fn advance(&self, duration: Duration) {
        self.0
            .fetch_add(duration.as_millis().try_into().unwrap(), Ordering::SeqCst);
    }
}

impl RuntimeTaskClock for ManualClock {
    fn now(&self) -> Duration {
        Duration::from_millis(self.0.load(Ordering::SeqCst))
    }
}

/// 在返回唯一文件前停住枚举，用于观察下游阶段尚未开始时的状态。
#[derive(Clone)]
struct HeldRows {
    state: Arc<(Mutex<(bool, bool)>, Condvar)>,
    row: ScannedPath,
}

impl HeldRows {
    /// 等待枚举线程已经进入受控阻塞点。
    fn wait_entered(&self) {
        let (lock, cv) = &*self.state;
        let mut state = lock.lock().unwrap();
        while !state.0 {
            state = cv.wait(state).unwrap();
        }
    }

    /// 允许枚举返回完整最终结果。
    fn release(&self) {
        let (lock, cv) = &*self.state;
        lock.lock().unwrap().1 = true;
        cv.notify_all();
    }
}

impl FileEnumerator for HeldRows {
    fn enumerate(&self, _: &[DisplayPath]) -> Result<Vec<ScannedPath>, ScanError> {
        let (lock, cv) = &*self.state;
        let mut state = lock.lock().unwrap();
        state.0 = true;
        cv.notify_all();
        while !state.1 {
            state = cv.wait(state).unwrap();
        }
        Ok(vec![self.row.clone()])
    }
}
#[derive(Clone)]
struct FailingRows;
impl FileEnumerator for FailingRows {
    fn enumerate(&self, _: &[DisplayPath]) -> Result<Vec<ScannedPath>, ScanError> {
        Err(ScanError::Stage1("controlled enumeration failure".into()))
    }
}

#[derive(Clone)]
struct CancelAfterTopCheck {
    state: Arc<(Mutex<(bool, bool)>, Condvar)>,
    cancellation: ReadCancellationToken,
}
impl CancelAfterTopCheck {
    /// 在有界时间内等待枚举器通过主检查点，避免前置失败留下永不结束的阻塞线程。
    fn wait_entered_for(&self, timeout: Duration) -> bool {
        let (lock, cv) = &*self.state;
        let deadline = Instant::now() + timeout;
        let mut state = lock.lock().unwrap();
        while !state.0 {
            let Some(remaining) = deadline.checked_duration_since(Instant::now()) else {
                return false;
            };
            let (next_state, result) = cv.wait_timeout(state, remaining).unwrap();
            state = next_state;
            if result.timed_out() && !state.0 {
                return false;
            }
        }
        true
    }
    fn release(&self) {
        let (lock, cv) = &*self.state;
        lock.lock().unwrap().1 = true;
        cv.notify_all();
    }
}
impl FileEnumerator for CancelAfterTopCheck {
    fn enumerate(&self, _: &[DisplayPath]) -> Result<Vec<ScannedPath>, ScanError> {
        let (lock, cv) = &*self.state;
        let mut state = lock.lock().unwrap();
        state.0 = true;
        cv.notify_all();
        while !state.1 {
            state = cv.wait(state).unwrap();
        }
        if self.cancellation.is_cancelled() {
            Err(ScanError::Cancelled)
        } else {
            Ok(Vec::new())
        }
    }
}

#[derive(Clone)]
struct Reader(dedup_node_engine::runtime_tasks::RuntimeTaskReporter);
impl PipelineFileReader for Reader {
    type Lease = ();
    fn read(
        &self,
        scanned: ScannedPath,
        _: ReadCancellationToken,
    ) -> Pin<Box<dyn Future<Output = Result<ReadProduct<()>, ReadFailure>> + Send>> {
        let _reporter = self.0.clone();
        Box::pin(async move {
            let bytes = fs::read(scanned.display_path.as_path()).unwrap();
            Ok(ReadProduct {
                md5: md5_bytes(&bytes),
                lease: (),
            })
        })
    }
}

struct Processor(dedup_node_engine::runtime_tasks::RuntimeTaskReporter);
impl Stage1Processor for Processor {
    async fn process(&mut self, request: Stage1Request) -> Result<Stage1Output, String> {
        self.0
            .update_worker(RuntimeWorkerUpdate {
                slot: 0,
                process_id: Some(7001),
                item_id: request.item_id.clone(),
                stage: RuntimeStage::ProbeStage1,
                display_path: request
                    .display_path
                    .as_path()
                    .to_string_lossy()
                    .into_owned(),
                physical_disk_id: "PhysicalDisk7".into(),
                completed_files: 1,
                speed_per_second: 1.0,
                current_step: "媒体探测".into(),
                cache_detail: String::new(),
                phase: None,
                cpu_weight: None,
                decoder_threads: None,
            })
            .await
            .unwrap();
        Ok(Stage1Output {
            media_kind: MediaKind::Other,
            width: 0,
            height: 0,
            duration_ms: None,
            frames: Vec::new(),
            contact_sheet_jpeg: None,
        })
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn scan_stages_start_their_own_timers_and_enumeration_counts_only_when_complete() {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("held.bin");
    fs::write(&path, b"held-stage-timing").unwrap();
    let row = ScannedPath::new(
        NormalizedPath::new(&path).unwrap(),
        DisplayPath::new(&path).unwrap(),
        fs::metadata(&path).unwrap().len(),
    );
    let enumerator = HeldRows {
        state: Arc::new((Mutex::new((false, false)), Condvar::new())),
        row,
    };
    let wait_barrier = enumerator.clone();
    let release_barrier = enumerator.clone();
    let clock = Arc::new(ManualClock::default());
    let registry = RuntimeTaskRegistry::with_clock(clock.clone());
    let reporter = registry
        .begin(
            RuntimeTaskKind::Scan,
            MachineId::from_sha256([0xa0; 32]),
            "阶段计时",
        )
        .await;
    let runtime_id = reporter.id().to_owned();
    let root = DisplayPath::new(directory.path()).unwrap();
    let run_reporter = reporter.clone();
    let sheets = directory.path().join("sheets");
    let run = tokio::spawn(async move {
        let mut store = NodeStore::open_in_memory(MachineId::from_sha256([0xa0; 32])).unwrap();
        let mut engine = ScanEngine::new(enumerator, SystemMd5, sheets)
            .with_runtime_reporter(run_reporter.clone());
        engine
            .run_parallel_with(
                &mut store,
                ScanOptions::new(vec![root]).force_recompute(),
                Reader(run_reporter.clone()),
                &mut Processor(run_reporter),
                PipelineLimits::new(1, 1),
                ReadCancellationToken::new(),
                1,
            )
            .await
    });

    tokio::task::spawn_blocking(move || wait_barrier.wait_entered())
        .await
        .unwrap();
    clock.advance(Duration::from_secs(60));
    let running = registry.details(&runtime_id).await.unwrap();
    release_barrier.release();
    run.await.unwrap().unwrap();

    let enumerate = running
        .stages
        .iter()
        .find(|stage| stage.stage_id == "enumerate")
        .unwrap();
    assert_eq!(
        enumerate.state,
        dedup_protocol::proto::RuntimeStageState::RuntimeStageRunning as i32
    );
    assert_eq!(enumerate.completed, 0, "枚举完成前不得发布中间数量");
    assert!(!enumerate.total_known);
    assert_eq!(enumerate.elapsed_ms, 60_000);
    for stage_id in [
        "cache_lookup",
        "read_md5",
        "probe_stage1",
        "persist_finalize",
    ] {
        let stage = running
            .stages
            .iter()
            .find(|stage| stage.stage_id == stage_id)
            .unwrap_or_else(|| panic!("缺少等待阶段 {stage_id}"));
        assert_eq!(
            stage.state,
            dedup_protocol::proto::RuntimeStageState::RuntimeStageWaiting as i32,
            "阶段 {stage_id} 尚未开始"
        );
        assert_eq!(stage.elapsed_ms, 0, "等待阶段不得累计任务总耗时");
    }

    let completed = registry.details(&runtime_id).await.unwrap();
    let enumerate = completed
        .stages
        .iter()
        .find(|stage| stage.stage_id == "enumerate")
        .unwrap();
    assert_eq!(enumerate.completed, 1);
    assert_eq!(enumerate.total, 1);
    assert!(enumerate.total_known);
    assert_eq!(enumerate.elapsed_ms, 60_000);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn materialized_enumeration_completes_before_bounded_row_delivery() {
    let directory = tempfile::tempdir().unwrap();
    let paths = [
        directory.path().join("blocked2.bin"),
        directory.path().join("next.bin"),
        directory.path().join("last.bin"),
    ];
    for (index, path) in paths.iter().enumerate() {
        fs::write(path, vec![index as u8; index + 1]).unwrap();
    }
    let rows = paths
        .iter()
        .map(|path| {
            ScannedPath::new(
                NormalizedPath::new(path).unwrap(),
                DisplayPath::new(path).unwrap(),
                fs::metadata(path).unwrap().len(),
            )
        })
        .collect();
    let gate = ReadGate::default();
    struct ReleaseOnDrop(ReadGate);
    impl Drop for ReleaseOnDrop {
        fn drop(&mut self) {
            self.0.release_all();
        }
    }
    let _release_on_drop = ReleaseOnDrop(gate.clone());
    let config = dedup_core::DiskReadConfig::default();
    let (reader, _) = ScheduledFileReader::controlled_for_test(&config, 1, gate.clone(), |_| {
        (vec![31], LocalDiskKind::Unknown)
    })
    .unwrap();
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(
            RuntimeTaskKind::Scan,
            MachineId::from_sha256([0xaf; 32]),
            "物化枚举完成边界",
        )
        .await;
    reporter
        .configure_pipeline_nowait(RuntimeExecutionConfigUpdate {
            hash_tasks: 1,
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
    let runtime_id = reporter.id().to_owned();
    let run_reporter = reporter.clone();
    let root = DisplayPath::new(directory.path()).unwrap();
    let sheets = directory.path().join("sheets");
    let run = tokio::spawn(async move {
        let mut store = NodeStore::open_in_memory(MachineId::from_sha256([0xaf; 32])).unwrap();
        let mut engine = ScanEngine::new(Rows(rows), SystemMd5, sheets)
            .with_runtime_reporter(run_reporter.clone());
        engine
            .run_parallel_with(
                &mut store,
                ScanOptions::new(vec![root]).force_recompute(),
                reader.with_runtime_reporter(run_reporter.clone()),
                &mut Processor(run_reporter),
                PipelineLimits::new(1, 1),
                ReadCancellationToken::new(),
                1,
            )
            .await
    });

    let wait_gate = gate.clone();
    let blocked_path = paths[0].clone();
    let started = tokio::time::timeout(
        Duration::from_secs(3),
        tokio::task::spawn_blocking(move || {
            wait_gate.wait_started_for(&blocked_path, Duration::from_secs(2))
        }),
    )
    .await
    .expect("首项读取必须进入真实读取门")
    .expect("读取门等待线程不应失败");
    assert!(
        started,
        "首项未进入真实读取门，当前已开始: {:?}",
        gate.started_keys()
    );
    let blocked = registry.details(&runtime_id).await.unwrap();
    gate.release_all();
    tokio::time::timeout(Duration::from_secs(3), run)
        .await
        .expect("释放读取后扫描必须完成")
        .unwrap()
        .unwrap();

    let enumerate = blocked
        .stages
        .iter()
        .find(|stage| stage.stage_id == "enumerate")
        .unwrap();
    assert_eq!(
        enumerate.state,
        dedup_protocol::proto::RuntimeStageState::RuntimeStageCompleted as i32,
        "Everything 已生成完整清单时，枚举阶段不得等待有界下游逐项交付"
    );
    assert!(enumerate.total_known);
    assert_eq!(enumerate.completed, 3);
    assert_eq!(enumerate.total, 3);
    let cache_lookup = blocked
        .stages
        .iter()
        .find(|stage| stage.stage_id == "cache_lookup")
        .unwrap();
    assert_eq!(
        cache_lookup.state,
        dedup_protocol::proto::RuntimeStageState::RuntimeStageCompleted as i32,
        "全部文件查询一次计算完成状态后，缓存阶段不得等待 MD5 读取"
    );
    assert!(cache_lookup.total_known);
    assert_eq!(cache_lookup.completed, 3);
    assert_eq!(cache_lookup.total, 3);
    let read_md5 = blocked
        .stages
        .iter()
        .find(|stage| stage.stage_id == "read_md5")
        .unwrap();
    assert_eq!(read_md5.unit, "files");
    assert_eq!(
        read_md5.completed, 0,
        "缓存未命中文件完成完整 MD5 前不得增加文件数"
    );
    assert_eq!(read_md5.total, 3);
    assert_eq!(blocked.summary.unwrap().overall_total, 3);
}

#[tokio::test]
async fn scan_reports_real_pipeline_stage_files_and_worker_identity() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("media.bin");
    fs::write(&path, b"real-block-bytes").unwrap();
    let scanned = ScannedPath::new(
        NormalizedPath::new(&path).unwrap(),
        DisplayPath::new(&path).unwrap(),
        fs::metadata(&path).unwrap().len(),
    );
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(
            RuntimeTaskKind::Scan,
            MachineId::from_sha256([0xa1; 32]),
            "扫描",
        )
        .await;
    let mut engine = ScanEngine::new(Rows(vec![scanned]), SystemMd5, dir.path().join("sheets"))
        .with_runtime_reporter(reporter.clone());
    let mut store = NodeStore::open_in_memory(MachineId::from_sha256([0xa1; 32])).unwrap();
    engine
        .run_parallel_with(
            &mut store,
            ScanOptions::new(vec![DisplayPath::new(dir.path()).unwrap()]).force_recompute(),
            Reader(reporter.clone()),
            &mut Processor(reporter.clone()),
            PipelineLimits::new(2, 2),
            ReadCancellationToken::new(),
            1,
        )
        .await
        .unwrap();
    let details = registry.details(reporter.id()).await.unwrap();
    for id in [
        "prepare",
        "enumerate",
        "cache_lookup",
        "read_md5",
        "probe_stage1",
        "persist_finalize",
    ] {
        assert!(
            details.stages.iter().any(|stage| stage.stage_id == id),
            "缺少 {id}"
        );
    }
    assert_eq!(details.summary.unwrap().overall_total, 1);
    assert_eq!(details.workers[0].process_id, Some(7001));
    assert_eq!(details.workers[0].physical_disk_id, "PhysicalDisk7");
    assert!(details.stages.iter().all(|stage| {
        stage.state != dedup_protocol::proto::RuntimeStageState::RuntimeStageRunning as i32
    }));
    let read = details
        .stages
        .iter()
        .find(|stage| stage.stage_id == "read_md5")
        .unwrap();
    assert!(read.total_known);
    assert_eq!(read.unit, "files");
    assert_eq!(read.completed, 1);
    assert_eq!(read.total, 1);
}

/// ScheduledFileReader 必须按真实许可生命周期采集 Hash/Media IO，而不是估算占用。
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn scheduled_reader_reports_real_hash_and_media_permit_lifetimes() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("held2.bin");
    fs::write(&path, b"scheduled-reader-telemetry").unwrap();
    let scanned = ScannedPath::new(
        NormalizedPath::new(&path).unwrap(),
        DisplayPath::new(&path).unwrap(),
        fs::metadata(&path).unwrap().len(),
    );
    let gate = ReadGate::default();
    let config = dedup_core::DiskReadConfig::default();
    let (reader, _) = ScheduledFileReader::controlled_for_test(&config, 1, gate.clone(), |_| {
        (vec![41], LocalDiskKind::Unknown)
    })
    .unwrap();
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(
            RuntimeTaskKind::Scan,
            MachineId::from_sha256([0xb1; 32]),
            "真实磁盘许可指标",
        )
        .await;
    reporter
        .configure_pipeline_nowait(RuntimeExecutionConfigUpdate {
            hash_tasks: 1,
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
    let reader = reader.with_runtime_reporter(reporter.clone());
    /// 断言失败时也必须解除测试块读取，避免测试进程遗留阻塞线程。
    struct ReadGateRelease(ReadGate);
    impl Drop for ReadGateRelease {
        fn drop(&mut self) {
            self.0.release_all();
        }
    }
    let _release = ReadGateRelease(gate.clone());
    let read = tokio::spawn(
        reader
            .clone()
            .read(scanned.clone(), ReadCancellationToken::new()),
    );
    let wait_gate = gate.clone();
    let wait_path = path.clone();
    let started = tokio::time::timeout(
        Duration::from_secs(3),
        tokio::task::spawn_blocking(move || {
            wait_gate.wait_started_for(&wait_path, Duration::from_secs(2))
        }),
    )
    .await
    .expect("Hash 读取必须进入真实读取门")
    .expect("读取门等待线程不应失败");
    assert!(
        started,
        "Hash 读取未进入真实读取门，当前已开始: {:?}",
        gate.started_keys()
    );

    let held = registry.details(reporter.id()).await.unwrap();
    let hash_io = held.pipeline_metrics.unwrap().hash_io.unwrap();
    assert_eq!(hash_io.current, Some(1));
    assert_eq!(hash_io.peak, Some(1));
    assert_eq!(hash_io.wait_latency.unwrap().count, 1);

    gate.release(&path);
    let product = read.await.unwrap().unwrap();
    assert_eq!(
        registry
            .details(reporter.id())
            .await
            .unwrap()
            .pipeline_metrics
            .unwrap()
            .hash_io
            .unwrap()
            .current,
        Some(1),
        "Hash 结果仍拥有许可时不得提前归零"
    );
    drop(product);
    let released = registry.details(reporter.id()).await.unwrap();
    let hash_io = released.pipeline_metrics.unwrap().hash_io.unwrap();
    assert_eq!(hash_io.current, Some(0));
    assert_eq!(hash_io.service_latency.unwrap().count, 1);

    let media_permit = reader
        .acquire_media_permit(scanned, ReadCancellationToken::new())
        .await
        .unwrap()
        .unwrap();
    let held = registry.details(reporter.id()).await.unwrap();
    let media_io = held.pipeline_metrics.unwrap().media_io.unwrap();
    assert_eq!(media_io.current, Some(1));
    assert_eq!(media_io.peak, Some(1));
    assert_eq!(media_io.wait_latency.unwrap().count, 1);
    drop(media_permit);
    let released = registry.details(reporter.id()).await.unwrap();
    let media_io = released.pipeline_metrics.unwrap().media_io.unwrap();
    assert_eq!(media_io.current, Some(0));
    assert_eq!(media_io.service_latency.unwrap().count, 1);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn read_md5_progress_starts_with_cache_hits_then_counts_completed_files() {
    let directory = tempfile::tempdir().unwrap();
    let paths = [
        directory.path().join("cached1.bin"),
        directory.path().join("missing2.bin"),
    ];
    fs::write(&paths[0], b"cached").unwrap();
    fs::write(&paths[1], b"missing").unwrap();
    let rows = paths
        .iter()
        .map(|path| {
            ScannedPath::new(
                NormalizedPath::new(path).unwrap(),
                DisplayPath::new(path).unwrap(),
                fs::metadata(path).unwrap().len(),
            )
        })
        .collect::<Vec<_>>();
    let gate = ReadGate::default();
    struct ReleaseOnDrop(ReadGate);
    impl Drop for ReleaseOnDrop {
        fn drop(&mut self) {
            self.0.release_all();
        }
    }
    let _release_on_drop = ReleaseOnDrop(gate.clone());
    let config = dedup_core::DiskReadConfig::default();
    let (reader, _) = ScheduledFileReader::controlled_for_test(&config, 1, gate.clone(), |_| {
        (vec![31], LocalDiskKind::Unknown)
    })
    .unwrap();
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(
            RuntimeTaskKind::Scan,
            MachineId::from_sha256([0xb0; 32]),
            "缓存命中起始进度",
        )
        .await;
    reporter
        .configure_pipeline_nowait(RuntimeExecutionConfigUpdate {
            hash_tasks: config.total_threads as u32,
            path_cache_queue_capacity: config.total_threads.saturating_mul(4) as u32,
            content_cache_queue_capacity: config.total_threads as u32,
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
    let runtime_id = reporter.id().to_owned();
    let run_reporter = reporter.clone();
    let root = DisplayPath::new(directory.path()).unwrap();
    let sheets = directory.path().join("sheets");
    let cached = rows[0].clone();
    let run = tokio::spawn(async move {
        let mut store = NodeStore::open_in_memory(MachineId::from_sha256([0xb0; 32])).unwrap();
        store
            .upsert_content_and_location(&cached, md5_bytes(b"cached"), MediaKind::Other)
            .unwrap();
        let mut engine = ScanEngine::new(Rows(rows), SystemMd5, sheets)
            .with_runtime_reporter(run_reporter.clone());
        engine
            .run_parallel_with(
                &mut store,
                ScanOptions::new(vec![root]),
                reader.with_runtime_reporter(run_reporter.clone()),
                &mut Processor(run_reporter),
                PipelineLimits::new(1, 1),
                ReadCancellationToken::new(),
                1,
            )
            .await
    });

    let wait_gate = gate.clone();
    let missing_path = paths[1].clone();
    let started = tokio::time::timeout(
        Duration::from_secs(3),
        tokio::task::spawn_blocking(move || {
            wait_gate.wait_started_for(&missing_path, Duration::from_secs(2))
        }),
    )
    .await
    .expect("缓存未命中项必须进入真实读取门")
    .expect("读取门等待线程不应失败");
    assert!(
        started,
        "缓存未命中项未进入真实读取门，当前已开始: {:?}",
        gate.started_keys()
    );
    let initial = registry.details(&runtime_id).await.unwrap();
    let read_md5 = initial
        .stages
        .iter()
        .find(|stage| stage.stage_id == "read_md5")
        .unwrap();
    assert_eq!(read_md5.unit, "files");
    assert_eq!(read_md5.completed, 1, "缓存命中应成为读取阶段的初始完成数");
    assert_eq!(read_md5.total, 2);

    gate.release_all();
    tokio::time::timeout(Duration::from_secs(3), run)
        .await
        .expect("释放读取后扫描必须完成")
        .unwrap()
        .unwrap();
    let completed = registry.details(&runtime_id).await.unwrap();
    let read_md5 = completed
        .stages
        .iter()
        .find(|stage| stage.stage_id == "read_md5")
        .unwrap();
    assert_eq!(read_md5.completed, 2, "每个完整 MD5 成功后只增加一个文件");
    assert_eq!(read_md5.total, 2);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn concurrent_telemetry_updates_are_exact_and_never_become_io_failures() {
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(
            RuntimeTaskKind::Scan,
            MachineId::from_sha256([0xa2; 32]),
            "并发",
        )
        .await;
    reporter
        .update_stage(
            dedup_node_engine::runtime_tasks::RuntimeStageUpdate::running(
                RuntimeStage::ReadMd5,
                RuntimeProgressUnit::Bytes,
                0,
                Some(800),
            ),
        )
        .await
        .unwrap();
    let mut joins = Vec::new();
    for _ in 0..8 {
        let reporter = reporter.clone();
        joins.push(tokio::spawn(async move {
            for _ in 0..100 {
                reporter
                    .advance_stage_nowait(RuntimeStage::ReadMd5, RuntimeProgressUnit::Bytes, 1)
                    .unwrap();
            }
        }));
    }
    for join in joins {
        join.await.unwrap();
    }
    assert_eq!(
        registry.details(reporter.id()).await.unwrap().stages[0].completed,
        800
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn controlled_two_disks_and_actual_two_worker_slots_expose_live_known_totals() {
    let dir = tempfile::tempdir().unwrap();
    let paths =
        ["a1.bin", "b1.bin", "a2.bin", "b2.bin", "a3.bin"].map(|name| dir.path().join(name));
    for (index, path) in paths.iter().enumerate() {
        fs::write(path, vec![index as u8 + 1; 16]).unwrap();
    }
    let rows = paths
        .iter()
        .map(|path| {
            ScannedPath::new(
                NormalizedPath::new(path).unwrap(),
                DisplayPath::new(path).unwrap(),
                16,
            )
        })
        .collect::<Vec<_>>();
    let gate = ReadGate::default();
    let mut config = dedup_core::DiskReadConfig::default();
    config.hdd_threads_per_disk = 2;
    config.total_threads = 4;
    let (reader, limits) =
        ScheduledFileReader::controlled_for_test(&config, 2, gate.clone(), |path| {
            let disk = if path.file_name().unwrap().to_string_lossy().starts_with('a') {
                7
            } else {
                12
            };
            (vec![disk], LocalDiskKind::Hdd)
        })
        .unwrap();
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(
            RuntimeTaskKind::Scan,
            MachineId::from_sha256([0xa3; 32]),
            "双盘",
        )
        .await;
    reporter
        .configure_pipeline_nowait(RuntimeExecutionConfigUpdate {
            hash_tasks: config.total_threads as u32,
            path_cache_queue_capacity: config.total_threads.saturating_mul(4) as u32,
            content_cache_queue_capacity: config.total_threads as u32,
            decode_queue_capacity: 2,
            persist_queue_capacity: 2,
            worker_slots: 2,
            cpu_budget: 2,
            global_disk_permits: config.total_threads as u32,
            hdd_per_disk_permits: config.hdd_threads_per_disk as u32,
            ssd_per_disk_permits: config.ssd_threads_per_disk as u32,
            unknown_per_disk_permits: config.unknown_threads_per_disk as u32,
        })
        .unwrap();
    let reader = reader.with_runtime_reporter(reporter.clone());
    let (pool, mut started, control) = WorkerPool::controlled_batch_for_test(2);
    let (first_started_tx, first_started_rx) = tokio::sync::oneshot::channel();
    let release_workers = Arc::new(tokio::sync::Notify::new());
    let controller_release = release_workers.clone();
    let controller = tokio::spawn(async move {
        let first = [started.recv().await.unwrap(), started.recv().await.unwrap()];
        let _ = first_started_tx.send(());
        controller_release.notified().await;
        for (task_id, item_id) in first {
            control
                .complete(
                    task_id,
                    item_id,
                    Stage1Output {
                        media_kind: MediaKind::Other,
                        width: 0,
                        height: 0,
                        duration_ms: None,
                        frames: Vec::new(),
                        contact_sheet_jpeg: None,
                    },
                )
                .await;
        }
        for _ in 0..2 {
            let (task_id, item_id) = started.recv().await.unwrap();
            control
                .complete(
                    task_id,
                    item_id,
                    Stage1Output {
                        media_kind: MediaKind::Other,
                        width: 0,
                        height: 0,
                        duration_ms: None,
                        frames: Vec::new(),
                        contact_sheet_jpeg: None,
                    },
                )
                .await;
        }
        let (task_id, item_id) = started.recv().await.unwrap();
        control
            .crash(task_id, item_id, "controlled worker crash".into())
            .await;
    });
    let root = DisplayPath::new(dir.path()).unwrap();
    let sheets = dir.path().join("sheets");
    let run_reporter = reporter.clone();
    let cancellation = ReadCancellationToken::new();
    struct ScenarioGuard {
        gate: ReadGate,
        workers: Arc<tokio::sync::Notify>,
        cancellation: ReadCancellationToken,
    }
    impl Drop for ScenarioGuard {
        fn drop(&mut self) {
            self.cancellation.cancel();
            self.gate.release_all();
            self.workers.notify_waiters();
        }
    }
    let _scenario_guard = ScenarioGuard {
        gate: gate.clone(),
        workers: release_workers.clone(),
        cancellation: cancellation.clone(),
    };
    let run_cancellation = cancellation.clone();
    let run = tokio::spawn(async move {
        let mut pool = pool;
        let mut processor = WorkerPoolStage1Processor::new(&mut pool, ReadCancellationToken::new())
            .with_runtime_reporter(run_reporter.clone());
        let mut engine =
            ScanEngine::new(Rows(rows), SystemMd5, sheets).with_runtime_reporter(run_reporter);
        let mut store = NodeStore::open_in_memory(MachineId::from_sha256([0xa3; 32])).unwrap();
        let result = engine
            .run_parallel_with(
                &mut store,
                ScanOptions::new(vec![root]).force_recompute(),
                reader,
                &mut processor,
                limits,
                run_cancellation,
                10,
            )
            .await;
        (store, result)
    });
    for path in &paths[..2] {
        let wait_gate = gate.clone();
        let wait_path = path.clone();
        let started = tokio::time::timeout(
            Duration::from_secs(3),
            tokio::task::spawn_blocking(move || {
                wait_gate.wait_started_for(&wait_path, Duration::from_secs(2))
            }),
        )
        .await
        .expect("两盘首批读取必须先进入真实读取门")
        .expect("读取门等待线程不应失败");
        assert!(
            started,
            "指定文件未进入真实读取门，当前已开始: {:?}",
            gate.started_keys()
        );
    }
    gate.release(&paths[0]);
    gate.release(&paths[1]);
    let first_ready = tokio::time::timeout(Duration::from_secs(3), first_started_rx).await;
    if first_ready.is_err() {
        cancellation.cancel();
        gate.release_all();
        release_workers.notify_one();
        let _ = tokio::time::timeout(Duration::from_secs(3), controller).await;
        let _ = tokio::time::timeout(Duration::from_secs(3), run).await;
        panic!("两盘首批读取必须进入两个真实Worker slot");
    }
    let released_bytes = registry.details(reporter.id()).await.unwrap();
    let read_after_release = released_bytes
        .stages
        .iter()
        .find(|stage| stage.stage_id == "read_md5")
        .unwrap();
    assert_eq!(
        read_after_release.completed, 2,
        "首批两个完整 MD5 只能推进两个文件"
    );
    tokio::time::timeout(Duration::from_secs(3), async {
        while registry.details(reporter.id()).await.unwrap().workers.len() != 2 {
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("Started telemetry 必须到达registry");
    let held = registry.details(reporter.id()).await.unwrap();
    let summary = held.summary.as_ref().unwrap();
    assert!(summary.overall_total_known);
    assert_eq!(summary.overall_total, 5);
    let read = held
        .stages
        .iter()
        .find(|stage| stage.stage_id == "read_md5")
        .unwrap();
    assert!(read.total_known);
    assert_eq!(read.unit, "files");
    assert_eq!(read.total, 5);
    assert!(held.stages.iter().any(|s| s.stage_id == "read_md5"
        && s.state == dedup_protocol::proto::RuntimeStageState::RuntimeStageRunning as i32));
    assert!(held.stages.iter().any(|s| s.stage_id == "probe_stage1"
        && s.state == dedup_protocol::proto::RuntimeStageState::RuntimeStageRunning as i32));
    assert_eq!(held.workers.len(), 2);
    assert!(
        held.workers
            .iter()
            .all(|worker| worker.process_id.is_some() && !worker.physical_disk_id.is_empty())
    );
    gate.release(&paths[2]);
    gate.release(&paths[3]);
    gate.release(&paths[4]);
    release_workers.notify_one();
    if tokio::time::timeout(Duration::from_secs(3), controller)
        .await
        .is_err()
    {
        cancellation.cancel();
        gate.release_all();
        let _ = tokio::time::timeout(Duration::from_secs(3), run).await;
        panic!("第二批必须复用两个slot并完成");
    }
    let (_store, result) = tokio::time::timeout(Duration::from_secs(3), run)
        .await
        .expect("扫描必须完成")
        .unwrap();
    result.unwrap();
    let done = registry.details(reporter.id()).await.unwrap();
    assert_eq!(
        done.workers
            .iter()
            .map(|worker| worker.completed_files)
            .sum::<u64>(),
        4
    );
    assert!(
        done.workers
            .iter()
            .all(|worker| worker.completed_files >= 2)
    );
    assert!(
        done.workers
            .iter()
            .all(|worker| worker.speed_per_second > 0.0)
    );
    assert_eq!(done.failures.len(), 1);
    assert!(done.failures[0].message.contains("controlled worker crash"));
    assert_eq!(_store.page_file_faults(None, 10).unwrap().items.len(), 1);
}

#[tokio::test]
async fn enumeration_error_marks_runtime_task_and_all_started_stages_failed() {
    let dir = tempfile::tempdir().unwrap();
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(
            RuntimeTaskKind::Scan,
            MachineId::from_sha256([0xa4; 32]),
            "枚举失败",
        )
        .await;
    let mut engine = ScanEngine::new(FailingRows, SystemMd5, dir.path().join("sheets"))
        .with_runtime_reporter(reporter.clone());
    let mut store = NodeStore::open_in_memory(MachineId::from_sha256([0xa4; 32])).unwrap();
    let result = engine
        .run_parallel_with(
            &mut store,
            ScanOptions::new(vec![DisplayPath::new(dir.path()).unwrap()]),
            Reader(reporter.clone()),
            &mut Processor(reporter.clone()),
            PipelineLimits::new(2, 2),
            ReadCancellationToken::new(),
            1,
        )
        .await;
    assert!(result.is_err());
    let details = registry.details(reporter.id()).await.unwrap();
    assert_eq!(details.summary.unwrap().state, "failed");
    assert!(details.stages.iter().all(|stage| {
        stage.state != dedup_protocol::proto::RuntimeStageState::RuntimeStageRunning as i32
    }));
    let enumerate = details
        .stages
        .iter()
        .find(|stage| stage.stage_id == "enumerate")
        .unwrap();
    assert_eq!(
        enumerate.state,
        dedup_protocol::proto::RuntimeStageState::RuntimeStageFailed as i32
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn cancel_after_successful_enumerator_join_during_read_barrier_returns_cleanly() {
    let dir = tempfile::tempdir().unwrap();
    let paths = [dir.path().join("c2.bin"), dir.path().join("d2.bin")];
    for path in &paths {
        fs::write(path, b"blocked-read").unwrap();
    }
    let rows = paths
        .iter()
        .map(|path| {
            ScannedPath::new(
                NormalizedPath::new(path).unwrap(),
                DisplayPath::new(path).unwrap(),
                12,
            )
        })
        .collect();
    let gate = ReadGate::default();
    struct Guard(ReadGate);
    impl Drop for Guard {
        fn drop(&mut self) {
            self.0.release_all();
        }
    }
    let _guard = Guard(gate.clone());
    let config = dedup_core::DiskReadConfig::default();
    let (reader, limits) =
        ScheduledFileReader::controlled_for_test(&config, 1, gate.clone(), |_| {
            (vec![20], LocalDiskKind::Unknown)
        })
        .unwrap();
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(
            RuntimeTaskKind::Scan,
            MachineId::from_sha256([0xa5; 32]),
            "取消",
        )
        .await;
    reporter
        .configure_pipeline_nowait(RuntimeExecutionConfigUpdate {
            hash_tasks: config.total_threads as u32,
            path_cache_queue_capacity: config.total_threads.saturating_mul(4) as u32,
            content_cache_queue_capacity: config.total_threads as u32,
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
    let reader = reader.with_runtime_reporter(reporter.clone());
    let cancellation = ReadCancellationToken::new();
    let run_cancel = cancellation.clone();
    let root = DisplayPath::new(dir.path()).unwrap();
    let run_reporter = reporter.clone();
    let run = tokio::spawn(async move {
        let mut engine = ScanEngine::new(Rows(rows), SystemMd5, dir.path().join("sheets"))
            .with_runtime_reporter(run_reporter.clone());
        let mut store = NodeStore::open_in_memory(MachineId::from_sha256([0xa5; 32])).unwrap();
        engine
            .run_parallel_with(
                &mut store,
                ScanOptions::new(vec![root]),
                reader,
                &mut Processor(run_reporter),
                limits,
                run_cancel,
                1,
            )
            .await
    });
    tokio::time::timeout(Duration::from_secs(3), async {
        while !registry.list().await[0].overall_total_known {
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("producer Ok join后必须立即freeze totals");
    cancellation.cancel();
    gate.release_all();
    let result = tokio::time::timeout(Duration::from_secs(3), run)
        .await
        .expect("取消后background owner必须归还")
        .unwrap();
    assert!(matches!(result, Err(ScanError::Cancelled)));
}

#[tokio::test]
async fn serial_processor_consumes_started_and_counts_only_matching_completions() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("serial.bin");
    fs::write(&path, b"serial").unwrap();
    let machine = MachineId::from_sha256([0xa6; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let scanned = ScannedPath::new(
        NormalizedPath::new(&path).unwrap(),
        DisplayPath::new(&path).unwrap(),
        6,
    );
    let content = store
        .upsert_content_and_location(&scanned, [0x33; 16], MediaKind::Other)
        .unwrap();
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine.clone(), "串行")
        .await;
    let (mut pool, mut started, control) = WorkerPool::controlled_batch_for_test(1);
    let controller = tokio::spawn(async move {
        for _ in 0..2 {
            let (task_id, item_id) = started.recv().await.unwrap();
            control
                .complete(
                    task_id,
                    item_id,
                    Stage1Output {
                        media_kind: MediaKind::Other,
                        width: 0,
                        height: 0,
                        duration_ms: None,
                        frames: Vec::new(),
                        contact_sheet_jpeg: None,
                    },
                )
                .await;
        }
        let (task_id, item_id) = started.recv().await.unwrap();
        control.crash(task_id, item_id, "serial crash".into()).await;
    });
    let mut processor = WorkerPoolStage1Processor::new(&mut pool, ReadCancellationToken::new())
        .with_runtime_reporter(reporter.clone());
    let task_id = dedup_core::TaskId::from_uuid(uuid::Uuid::new_v4());
    for index in 0..3 {
        let result = processor
            .process(Stage1Request {
                task_id,
                item_id: format!("serial-{index}"),
                machine_id: machine.clone(),
                normalized_path: scanned.normalized_path.clone(),
                display_path: scanned.display_path.clone(),
                file_size: 6,
                stage: "probe_stage1".into(),
                content_id: content.id,
                physical_disk_id: "PhysicalDisk9".into(),
                generate_contact_sheet: false,
            })
            .await;
        assert_eq!(result.is_ok(), index < 2);
    }
    controller.await.unwrap();
    let worker = registry
        .details(reporter.id())
        .await
        .unwrap()
        .workers
        .remove(0);
    assert_eq!(worker.completed_files, 2);
    assert!(worker.speed_per_second > 0.0);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn producer_cancel_after_main_top_check_never_turns_cancelled_task_failed() {
    let dir = tempfile::tempdir().unwrap();
    let database = dir.path().join("node.db");
    let machine = MachineId::from_sha256([0xa7; 32]);
    let mut store = NodeStore::open(&database, machine.clone()).unwrap();
    let root = DisplayPath::new(dir.path()).unwrap();
    let options = ScanOptions::new(vec![root]);
    let task_id = begin_scan_task(&mut store, &options, 1).unwrap();
    let cancellation = ReadCancellationToken::new();
    let enumerator = CancelAfterTopCheck {
        state: Arc::new((Mutex::new((false, false)), Condvar::new())),
        cancellation: cancellation.clone(),
    };
    let barrier = enumerator.clone();
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, machine.clone(), "取消竞态")
        .await;
    // 该测试需要在同一运行期间使用独立控制连接；reopen 不会重复清理瞬态任务表。
    let mut control_store = store.reopen().unwrap();
    let run_reporter = reporter.clone();
    let run_cancel = cancellation.clone();
    let run = tokio::spawn(async move {
        let mut engine = ScanEngine::new(enumerator, SystemMd5, dir.path().join("sheets"))
            .with_runtime_reporter(run_reporter.clone());
        let result = engine
            .run_existing_parallel_with(
                &mut store,
                task_id,
                options,
                Reader(run_reporter.clone()),
                &mut Processor(run_reporter),
                PipelineLimits::new(1, 1),
                run_cancel,
                2,
            )
            .await;
        (store, result)
    });
    let wait = barrier.clone();
    let entered = tokio::time::timeout(
        Duration::from_secs(3),
        tokio::task::spawn_blocking(move || wait.wait_entered_for(Duration::from_secs(2))),
    )
    .await
    .expect("取消竞态枚举器必须进入主检查点")
    .expect("取消竞态屏障等待线程不应失败");
    assert!(entered, "取消竞态枚举器未进入主检查点");
    control_store.cancel_task(task_id, 3).unwrap();
    cancellation.cancel();
    barrier.release();
    let (store, result) = run.await.unwrap();
    assert!(matches!(result, Err(ScanError::Cancelled)));
    assert_eq!(
        store.task_snapshot(task_id).unwrap().status,
        TaskStatus::Cancelled
    );
    assert_eq!(
        registry
            .details(reporter.id())
            .await
            .unwrap()
            .summary
            .unwrap()
            .state,
        "cancelled"
    );
}
