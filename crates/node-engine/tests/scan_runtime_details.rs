use std::{collections::BTreeSet, future::Future, fs, path::Path, pin::Pin, sync::{Arc, Condvar, Mutex}, time::Duration};

use dedup_core::{DisplayPath, MachineId, MediaKind, NormalizedPath};
use dedup_node_engine::{
    io::{BlockReadError, BlockReader, ReadFailure},
    runtime_tasks::{
        RuntimeProgressUnit, RuntimeStage, RuntimeTaskKind, RuntimeTaskRegistry,
        RuntimeWorkerUpdate,
    },
    scan::{
        FileEnumerator, PipelineFileReader, PipelineLimits, ReadProduct, ScanEngine, ScanError,
        ScanOptions, ScheduledFileReader, Stage1Processor, Stage1Request, SystemMd5, begin_scan_task,
        WorkerPoolStage1Processor, md5_bytes,
    },
    worker::{Stage1Output, WorkerPool},
};
use dedup_node_store::{NodeStore, ScannedPath, TaskStatus};
use dedup_windows::{LocalDiskKind, ReadCancellationToken};

#[derive(Clone, Default)]
struct ReadGate(Arc<(Mutex<GateState>, Condvar)>);
#[derive(Default)]
struct GateState { started: BTreeSet<String>, released: BTreeSet<String>, all_released: bool }
impl ReadGate {
    fn release(&self, path: &Path) {
        let (lock, cv) = &*self.0;
        lock.lock().unwrap().released.insert(path.file_name().unwrap().to_string_lossy().into_owned());
        cv.notify_all();
    }
    fn release_all(&self) {
        let (lock, cv) = &*self.0;
        lock.lock().unwrap().all_released = true;
        cv.notify_all();
    }
}
impl BlockReader for ReadGate {
    fn read_at(&self, path: &Path, offset: u64, buffer: &mut [u8], _: Duration, _: &ReadCancellationToken) -> Result<usize, BlockReadError> {
        let (lock, cv) = &*self.0;
        let mut state = lock.lock().unwrap();
        let key = path.file_name().unwrap().to_string_lossy().into_owned();
        state.started.insert(key.clone());
        cv.notify_all();
        while (key.contains('2') || key.contains('3')) && !state.all_released && !state.released.contains(&key) {
            state = cv.wait(state).unwrap();
        }
        drop(state);
        let data = fs::read(path).map_err(BlockReadError::Io)?;
        let offset = offset as usize;
        if offset >= data.len() { return Ok(0); }
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
    fn wait_entered(&self) {
        let (lock, cv) = &*self.state;
        let mut state = lock.lock().unwrap();
        while !state.0 { state = cv.wait(state).unwrap(); }
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
        while !state.1 { state = cv.wait(state).unwrap(); }
        if self.cancellation.is_cancelled() { Err(ScanError::Cancelled) } else { Ok(Vec::new()) }
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
        let reporter = self.0.clone();
        Box::pin(async move {
            let bytes = fs::read(scanned.display_path.as_path()).unwrap();
            reporter
                .advance_stage_nowait(
                    RuntimeStage::ReadMd5,
                    RuntimeProgressUnit::Bytes,
                    bytes.len() as u64,
                )
                .unwrap();
            Ok(ReadProduct { md5: md5_bytes(&bytes), lease: () })
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
                stage: RuntimeStage::ProbeStage1,
                display_path: request.display_path.as_path().to_string_lossy().into_owned(),
                physical_disk_id: "PhysicalDisk7".into(),
                completed_files: 1,
                speed_per_second: 1.0,
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

#[tokio::test]
async fn scan_reports_real_pipeline_stages_bytes_and_worker_identity() {
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
        .begin(RuntimeTaskKind::Scan, MachineId::from_sha256([0xa1; 32]), "扫描")
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
    for id in ["prepare", "enumerate", "cache_lookup", "read_md5", "probe_stage1", "persist_finalize"] {
        assert!(details.stages.iter().any(|stage| stage.stage_id == id), "缺少 {id}");
    }
    assert_eq!(details.summary.unwrap().overall_total, 1);
    assert_eq!(details.workers[0].process_id, Some(7001));
    assert_eq!(details.workers[0].physical_disk_id, "PhysicalDisk7");
    assert!(details.stages.iter().all(|stage| {
        stage.state != dedup_protocol::proto::RuntimeStageState::RuntimeStageRunning as i32
    }));
    let read = details.stages.iter().find(|stage| stage.stage_id == "read_md5").unwrap();
    assert!(read.total_known);
    assert_eq!(read.total, b"real-block-bytes".len() as u64);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn concurrent_telemetry_updates_are_exact_and_never_become_io_failures() {
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Scan, MachineId::from_sha256([0xa2; 32]), "并发")
        .await;
    reporter
        .update_stage(dedup_node_engine::runtime_tasks::RuntimeStageUpdate::running(
            RuntimeStage::ReadMd5,
            RuntimeProgressUnit::Bytes,
            0,
            Some(800),
        ))
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
    let paths = ["a1.bin", "b1.bin", "a2.bin", "b2.bin", "a3.bin"]
        .map(|name| dir.path().join(name));
    for (index, path) in paths.iter().enumerate() {
        fs::write(path, vec![index as u8 + 1; 16]).unwrap();
    }
    let rows = paths.iter().map(|path| ScannedPath::new(
        NormalizedPath::new(path).unwrap(), DisplayPath::new(path).unwrap(), 16,
    )).collect::<Vec<_>>();
    let gate = ReadGate::default();
    let mut config = dedup_core::DiskReadConfig::default();
    config.hdd_threads_per_disk = 2;
    config.total_threads = 4;
    let (reader, limits) = ScheduledFileReader::controlled_for_test(
        &config, 2, gate.clone(), |path| {
            let disk = if path.file_name().unwrap().to_string_lossy().starts_with('a') { 7 } else { 12 };
            (vec![disk], LocalDiskKind::Hdd)
        },
    ).unwrap();
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry.begin(RuntimeTaskKind::Scan, MachineId::from_sha256([0xa3; 32]), "双盘").await;
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
            control.complete(task_id, item_id, Stage1Output { media_kind: MediaKind::Other, width: 0, height: 0, duration_ms: None, frames: Vec::new(), contact_sheet_jpeg: None }).await;
        }
        for _ in 0..2 {
            let (task_id, item_id) = started.recv().await.unwrap();
            control.complete(task_id, item_id, Stage1Output { media_kind: MediaKind::Other, width: 0, height: 0, duration_ms: None, frames: Vec::new(), contact_sheet_jpeg: None }).await;
        }
        let (task_id, item_id) = started.recv().await.unwrap();
        control.crash(task_id, item_id, "controlled worker crash".into()).await;
    });
    let root = DisplayPath::new(dir.path()).unwrap();
    let sheets = dir.path().join("sheets");
    let run_reporter = reporter.clone();
    let cancellation = ReadCancellationToken::new();
    struct ScenarioGuard { gate: ReadGate, workers: Arc<tokio::sync::Notify>, cancellation: ReadCancellationToken }
    impl Drop for ScenarioGuard {
        fn drop(&mut self) {
            self.cancellation.cancel();
            self.gate.release_all();
            self.workers.notify_waiters();
        }
    }
    let _scenario_guard = ScenarioGuard { gate: gate.clone(), workers: release_workers.clone(), cancellation: cancellation.clone() };
    let run_cancellation = cancellation.clone();
    let run = tokio::spawn(async move {
        let mut pool = pool;
        let mut processor = WorkerPoolStage1Processor::new(&mut pool, ReadCancellationToken::new())
            .with_runtime_reporter(run_reporter.clone());
        let mut engine = ScanEngine::new(Rows(rows), SystemMd5, sheets)
            .with_runtime_reporter(run_reporter);
        let mut store = NodeStore::open_in_memory(MachineId::from_sha256([0xa3; 32])).unwrap();
        let result = engine.run_parallel_with(
            &mut store, ScanOptions::new(vec![root]).force_recompute(), reader,
            &mut processor, limits, run_cancellation, 10,
        ).await;
        (store, result)
    });
    gate.release(&paths[0]); gate.release(&paths[1]);
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
    let read_after_release = released_bytes.stages.iter().find(|stage| stage.stage_id == "read_md5").unwrap();
    assert_eq!(read_after_release.completed, 32, "首批两个真实block只能推进32字节");
    tokio::time::timeout(Duration::from_secs(3), async {
        while registry.details(reporter.id()).await.unwrap().workers.len() != 2 {
            tokio::task::yield_now().await;
        }
    }).await.expect("Started telemetry 必须到达registry");
    let held = registry.details(reporter.id()).await.unwrap();
    let summary = held.summary.as_ref().unwrap();
    assert!(summary.overall_total_known);
    assert_eq!(summary.overall_total, 5);
    let read = held.stages.iter().find(|stage| stage.stage_id == "read_md5").unwrap();
    assert!(read.total_known);
    assert_eq!(read.total, 80);
    assert!(held.stages.iter().any(|s| s.stage_id == "read_md5" && s.state == dedup_protocol::proto::RuntimeStageState::RuntimeStageRunning as i32));
    assert!(held.stages.iter().any(|s| s.stage_id == "probe_stage1" && s.state == dedup_protocol::proto::RuntimeStageState::RuntimeStageRunning as i32));
    assert_eq!(held.workers.len(), 2);
    assert!(held.workers.iter().all(|worker| worker.process_id.is_some() && !worker.physical_disk_id.is_empty()));
    gate.release(&paths[2]); gate.release(&paths[3]);
    gate.release(&paths[4]);
    release_workers.notify_one();
    if tokio::time::timeout(Duration::from_secs(3), controller).await.is_err() {
        cancellation.cancel();
        gate.release_all();
        let _ = tokio::time::timeout(Duration::from_secs(3), run).await;
        panic!("第二批必须复用两个slot并完成");
    }
    let (_store, result) = tokio::time::timeout(Duration::from_secs(3), run)
        .await.expect("扫描必须完成").unwrap();
    result.unwrap();
    let done = registry.details(reporter.id()).await.unwrap();
    assert_eq!(done.workers.iter().map(|worker| worker.completed_files).sum::<u64>(), 4);
    assert!(done.workers.iter().all(|worker| worker.completed_files >= 2));
    assert!(done.workers.iter().all(|worker| worker.speed_per_second > 0.0));
    assert_eq!(done.failures.len(), 1);
    assert!(done.failures[0].message.contains("controlled worker crash"));
    assert_eq!(_store.page_file_faults(None, 10).unwrap().items.len(), 1);
}

#[tokio::test]
async fn enumeration_error_marks_runtime_task_and_all_started_stages_failed() {
    let dir = tempfile::tempdir().unwrap();
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry.begin(RuntimeTaskKind::Scan, MachineId::from_sha256([0xa4; 32]), "枚举失败").await;
    let mut engine = ScanEngine::new(FailingRows, SystemMd5, dir.path().join("sheets"))
        .with_runtime_reporter(reporter.clone());
    let mut store = NodeStore::open_in_memory(MachineId::from_sha256([0xa4; 32])).unwrap();
    let result = engine.run_parallel_with(
        &mut store, ScanOptions::new(vec![DisplayPath::new(dir.path()).unwrap()]),
        Reader(reporter.clone()), &mut Processor(reporter.clone()),
        PipelineLimits::new(2, 2), ReadCancellationToken::new(), 1,
    ).await;
    assert!(result.is_err());
    let details = registry.details(reporter.id()).await.unwrap();
    assert_eq!(details.summary.unwrap().state, "failed");
    assert!(details.stages.iter().all(|stage| {
        stage.state != dedup_protocol::proto::RuntimeStageState::RuntimeStageRunning as i32
    }));
    let enumerate = details.stages.iter().find(|stage| stage.stage_id == "enumerate").unwrap();
    assert_eq!(enumerate.state, dedup_protocol::proto::RuntimeStageState::RuntimeStageFailed as i32);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn cancel_after_successful_enumerator_join_during_read_barrier_returns_cleanly() {
    let dir = tempfile::tempdir().unwrap();
    let paths = [dir.path().join("c2.bin"), dir.path().join("d2.bin")];
    for path in &paths { fs::write(path, b"blocked-read").unwrap(); }
    let rows = paths.iter().map(|path| ScannedPath::new(
        NormalizedPath::new(path).unwrap(), DisplayPath::new(path).unwrap(), 12,
    )).collect();
    let gate = ReadGate::default();
    struct Guard(ReadGate);
    impl Drop for Guard { fn drop(&mut self) { self.0.release_all(); } }
    let _guard = Guard(gate.clone());
    let config = dedup_core::DiskReadConfig::default();
    let (reader, limits) = ScheduledFileReader::controlled_for_test(
        &config, 1, gate.clone(), |_| (vec![20], LocalDiskKind::Unknown),
    ).unwrap();
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry.begin(RuntimeTaskKind::Scan, MachineId::from_sha256([0xa5; 32]), "取消").await;
    let reader = reader.with_runtime_reporter(reporter.clone());
    let cancellation = ReadCancellationToken::new();
    let run_cancel = cancellation.clone();
    let root = DisplayPath::new(dir.path()).unwrap();
    let run_reporter = reporter.clone();
    let run = tokio::spawn(async move {
        let mut engine = ScanEngine::new(Rows(rows), SystemMd5, dir.path().join("sheets"))
            .with_runtime_reporter(run_reporter.clone());
        let mut store = NodeStore::open_in_memory(MachineId::from_sha256([0xa5; 32])).unwrap();
        engine.run_parallel_with(
            &mut store, ScanOptions::new(vec![root]), reader,
            &mut Processor(run_reporter), limits, run_cancel, 1,
        ).await
    });
    tokio::time::timeout(Duration::from_secs(3), async {
        while !registry.list().await[0].overall_total_known { tokio::task::yield_now().await; }
    }).await.expect("producer Ok join后必须立即freeze totals");
    cancellation.cancel();
    gate.release_all();
    let result = tokio::time::timeout(Duration::from_secs(3), run).await
        .expect("取消后background owner必须归还").unwrap();
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
        NormalizedPath::new(&path).unwrap(), DisplayPath::new(&path).unwrap(), 6,
    );
    let content = store.upsert_content_and_location(&scanned, [0x33; 16], MediaKind::Other).unwrap();
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry.begin(RuntimeTaskKind::Scan, machine.clone(), "串行").await;
    let (mut pool, mut started, control) = WorkerPool::controlled_batch_for_test(1);
    let controller = tokio::spawn(async move {
        for _ in 0..2 {
            let (task_id, item_id) = started.recv().await.unwrap();
            control.complete(task_id, item_id, Stage1Output { media_kind: MediaKind::Other, width: 0, height: 0, duration_ms: None, frames: Vec::new(), contact_sheet_jpeg: None }).await;
        }
        let (task_id, item_id) = started.recv().await.unwrap();
        control.crash(task_id, item_id, "serial crash".into()).await;
    });
    let mut processor = WorkerPoolStage1Processor::new(&mut pool, ReadCancellationToken::new())
        .with_runtime_reporter(reporter.clone());
    let task_id = dedup_core::TaskId::from_uuid(uuid::Uuid::new_v4());
    for index in 0..3 {
        let result = processor.process(Stage1Request {
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
        }).await;
        assert_eq!(result.is_ok(), index < 2);
    }
    controller.await.unwrap();
    let worker = registry.details(reporter.id()).await.unwrap().workers.remove(0);
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
    let reporter = registry.begin(RuntimeTaskKind::Scan, machine.clone(), "取消竞态").await;
    let run_reporter = reporter.clone();
    let run_cancel = cancellation.clone();
    let run = tokio::spawn(async move {
        let mut engine = ScanEngine::new(enumerator, SystemMd5, dir.path().join("sheets"))
            .with_runtime_reporter(run_reporter.clone());
        let result = engine.run_existing_parallel_with(
            &mut store, task_id, options, Reader(run_reporter.clone()),
            &mut Processor(run_reporter), PipelineLimits::new(1, 1), run_cancel, 2,
        ).await;
        (store, result)
    });
    let wait = barrier.clone();
    tokio::task::spawn_blocking(move || wait.wait_entered()).await.unwrap();
    let mut control_store = NodeStore::open(&database, machine).unwrap();
    control_store.cancel_task(task_id, 3).unwrap();
    cancellation.cancel();
    barrier.release();
    let (store, result) = run.await.unwrap();
    assert!(matches!(result, Err(ScanError::Cancelled)));
    assert_eq!(store.task_snapshot(task_id).unwrap().status, TaskStatus::Cancelled);
    assert_eq!(registry.details(reporter.id()).await.unwrap().summary.unwrap().state, "cancelled");
}
