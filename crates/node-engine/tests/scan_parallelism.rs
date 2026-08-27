//! 扫描流水线的跨盘推进、Worker 乱序归并、单写者和枚举背压行为。

use std::{
    collections::BTreeMap,
    future::Future,
    path::{Path, PathBuf},
    pin::Pin,
    sync::{
        Arc, Mutex,
        atomic::{AtomicUsize, Ordering},
    },
};

use dedup_core::{DisplayPath, MachineId, MediaKind, NormalizedPath};
use dedup_media::{ImageStage1, PdqHash};
use dedup_node_engine::{
    io::ReadFailure,
    scan::{
        FileEnumerator, PipelineFileReader, PipelineLimits, ReadProduct, ScanEngine, ScanError,
        ScanOptions, Stage1BatchResult, Stage1Processor, Stage1Request, SystemMd5, md5_bytes,
        publish_contact_sheet_for_test,
    },
    worker::{Stage1Frame, Stage1Output},
};
use dedup_node_store::{
    CompleteStage1, FeatureWrite, ImageStage1Fields, NewTaskItem, NodeStore, ScannedPath,
};
use dedup_windows::ReadCancellationToken;
use tokio::sync::{OwnedSemaphorePermit, Semaphore, mpsc};

#[derive(Clone)]
struct StreamingEnumerator {
    rows: Arc<Vec<ScannedPath>>,
    attempted: Option<mpsc::UnboundedSender<usize>>,
    emitted: Arc<AtomicUsize>,
}

impl FileEnumerator for StreamingEnumerator {
    fn enumerate(&self, _roots: &[DisplayPath]) -> Result<Vec<ScannedPath>, ScanError> {
        Ok(self.rows.as_ref().clone())
    }

    fn enumerate_into(
        &self,
        _roots: &[DisplayPath],
        emit: &mut dyn FnMut(ScannedPath) -> Result<(), ScanError>,
    ) -> Result<(), ScanError> {
        for (index, row) in self.rows.iter().cloned().enumerate() {
            if let Some(attempted) = &self.attempted {
                let _ = attempted.send(index);
            }
            emit(row)?;
            self.emitted.fetch_add(1, Ordering::AcqRel);
        }
        Ok(())
    }
}

#[derive(Clone)]
struct FakePipelineReader {
    data: Arc<BTreeMap<PathBuf, Vec<u8>>>,
    gates: Arc<BTreeMap<PathBuf, Arc<Semaphore>>>,
    completed: mpsc::UnboundedSender<PathBuf>,
    active_leases: Arc<AtomicUsize>,
}

struct TestLease(Arc<AtomicUsize>);

impl Drop for TestLease {
    fn drop(&mut self) {
        self.0.fetch_sub(1, Ordering::AcqRel);
    }
}

#[derive(Clone)]
struct HoldingPipelineReader {
    data: Arc<BTreeMap<PathBuf, Vec<u8>>>,
    gates: Arc<BTreeMap<PathBuf, Arc<Semaphore>>>,
    started: mpsc::UnboundedSender<PathBuf>,
    completed: mpsc::UnboundedSender<PathBuf>,
    active_leases: Arc<AtomicUsize>,
}

/// 模拟同一物理盘只有固定读取许可，读取完成后租约仍随一筛项持有。
#[derive(Clone)]
struct PermitLimitedReader {
    data: Arc<BTreeMap<PathBuf, Vec<u8>>>,
    permits: Arc<Semaphore>,
    completed: mpsc::UnboundedSender<PathBuf>,
}

impl PipelineFileReader for PermitLimitedReader {
    type Lease = OwnedSemaphorePermit;

    fn read(
        &self,
        scanned: ScannedPath,
        _cancellation: ReadCancellationToken,
    ) -> Pin<Box<dyn Future<Output = Result<ReadProduct<Self::Lease>, ReadFailure>> + Send + 'static>>
    {
        let path = scanned.display_path.as_path().to_path_buf();
        let data = self.data.get(&path).unwrap().clone();
        let permits = self.permits.clone();
        let completed = self.completed.clone();
        Box::pin(async move {
            let lease = permits.acquire_owned().await.unwrap();
            let _ = completed.send(path);
            Ok(ReadProduct {
                md5: md5_bytes(&data),
                lease,
            })
        })
    }
}

impl PipelineFileReader for HoldingPipelineReader {
    type Lease = TestLease;

    fn read(
        &self,
        scanned: ScannedPath,
        cancellation: ReadCancellationToken,
    ) -> Pin<Box<dyn Future<Output = Result<ReadProduct<Self::Lease>, ReadFailure>> + Send + 'static>>
    {
        let path = scanned.display_path.as_path().to_path_buf();
        let data = self.data.get(&path).unwrap().clone();
        let gate = self.gates.get(&path).unwrap().clone();
        let started = self.started.clone();
        let completed = self.completed.clone();
        let active_leases = self.active_leases.clone();
        Box::pin(async move {
            active_leases.fetch_add(1, Ordering::AcqRel);
            let lease = TestLease(active_leases);
            let _ = started.send(path.clone());
            gate.acquire().await.unwrap().forget();
            let _ = completed.send(path);
            if cancellation.is_cancelled() {
                drop(lease);
                Err(ReadFailure::Cancelled)
            } else {
                Ok(ReadProduct {
                    md5: md5_bytes(&data),
                    lease,
                })
            }
        })
    }
}

impl PipelineFileReader for FakePipelineReader {
    type Lease = TestLease;

    fn read(
        &self,
        scanned: ScannedPath,
        cancellation: ReadCancellationToken,
    ) -> Pin<Box<dyn Future<Output = Result<ReadProduct<Self::Lease>, ReadFailure>> + Send + 'static>>
    {
        let data = self
            .data
            .get(scanned.display_path.as_path())
            .unwrap()
            .clone();
        let gate = self.gates.get(scanned.display_path.as_path()).cloned();
        let completed = self.completed.clone();
        let active_leases = self.active_leases.clone();
        Box::pin(async move {
            if let Some(gate) = gate {
                gate.acquire().await.unwrap().forget();
            }
            if cancellation.is_cancelled() {
                return Err(ReadFailure::Cancelled);
            }
            let _ = completed.send(scanned.display_path.as_path().to_path_buf());
            active_leases.fetch_add(1, Ordering::AcqRel);
            Ok(ReadProduct {
                md5: md5_bytes(&data),
                lease: TestLease(active_leases),
            })
        })
    }
}

struct ReversedBatchProcessor {
    batch_sizes: Arc<Mutex<Vec<usize>>>,
    active_leases: Arc<AtomicUsize>,
}

struct InvalidVideoProcessor;

/// 模拟 Worker 数明显大于单盘读取许可的生产批处理器。
struct WideBatchProcessor {
    batch_sizes: Arc<Mutex<Vec<usize>>>,
}

/// 首批 Worker 保持运行，供测试观察后续磁盘读取能否并行推进。
struct BlockingFirstBatchProcessor {
    started: mpsc::UnboundedSender<()>,
    release: Arc<Semaphore>,
    blocked_once: bool,
}

impl Stage1Processor for BlockingFirstBatchProcessor {
    async fn process(&mut self, request: Stage1Request) -> Result<Stage1Output, String> {
        Ok(image_output(&request))
    }

    fn max_in_flight(&self) -> usize {
        24
    }

    async fn process_batch(&mut self, requests: Vec<Stage1Request>) -> Vec<Stage1BatchResult> {
        if !self.blocked_once {
            self.blocked_once = true;
            let _ = self.started.send(());
            self.release.acquire().await.unwrap().forget();
        }
        requests
            .into_iter()
            .map(|request| Stage1BatchResult {
                item_id: request.item_id.clone(),
                output: Ok(image_output(&request)),
            })
            .collect()
    }
}

impl Stage1Processor for WideBatchProcessor {
    async fn process(&mut self, request: Stage1Request) -> Result<Stage1Output, String> {
        Ok(image_output(&request))
    }

    fn max_in_flight(&self) -> usize {
        24
    }

    async fn process_batch(&mut self, requests: Vec<Stage1Request>) -> Vec<Stage1BatchResult> {
        self.batch_sizes.lock().unwrap().push(requests.len());
        requests
            .into_iter()
            .map(|request| Stage1BatchResult {
                item_id: request.item_id.clone(),
                output: Ok(image_output(&request)),
            })
            .collect()
    }
}

impl Stage1Processor for InvalidVideoProcessor {
    async fn process(&mut self, _request: Stage1Request) -> Result<Stage1Output, String> {
        Ok(Stage1Output {
            media_kind: MediaKind::Video,
            width: 1,
            height: 1,
            duration_ms: Some(u64::MAX),
            frames: Vec::new(),
            contact_sheet_jpeg: Some(vec![1, 2, 3]),
        })
    }
}

impl Stage1Processor for ReversedBatchProcessor {
    async fn process(&mut self, request: Stage1Request) -> Result<Stage1Output, String> {
        Ok(image_output(&request))
    }

    fn max_in_flight(&self) -> usize {
        2
    }

    async fn process_batch(&mut self, requests: Vec<Stage1Request>) -> Vec<Stage1BatchResult> {
        self.batch_sizes.lock().unwrap().push(requests.len());
        assert_eq!(
            self.active_leases.load(Ordering::Acquire),
            0,
            "磁盘读取许可不得延伸到 Worker 计算阶段"
        );
        requests
            .into_iter()
            .rev()
            .map(|request| Stage1BatchResult {
                item_id: request.item_id.clone(),
                output: Ok(image_output(&request)),
            })
            .collect()
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn completed_reads_flush_before_same_disk_permits_block_the_worker_batch() {
    let directory = tempfile::tempdir().unwrap();
    let paths = [
        directory.path().join("one.bin"),
        directory.path().join("two.bin"),
        directory.path().join("three.bin"),
    ];
    let mut data = BTreeMap::new();
    for (index, path) in paths.iter().enumerate() {
        let bytes = vec![index as u8 + 1; index + 1];
        std::fs::write(path, &bytes).unwrap();
        data.insert(path.clone(), bytes);
    }
    let rows = paths.iter().map(|path| scanned(path)).collect::<Vec<_>>();
    let enumerator = StreamingEnumerator {
        rows: Arc::new(rows),
        attempted: None,
        emitted: Arc::new(AtomicUsize::new(0)),
    };
    let (completed, _completed_rx) = mpsc::unbounded_channel();
    let reader = PermitLimitedReader {
        data: Arc::new(data),
        permits: Arc::new(Semaphore::new(2)),
        completed,
    };
    let batch_sizes = Arc::new(Mutex::new(Vec::new()));
    let mut processor = WideBatchProcessor {
        batch_sizes: batch_sizes.clone(),
    };
    let root = DisplayPath::new(directory.path()).unwrap();
    let mut store = NodeStore::open_in_memory(MachineId::parse(&"43".repeat(32)).unwrap()).unwrap();
    let mut engine = ScanEngine::new(enumerator, SystemMd5, directory.path().join("sheets"));

    let summary = tokio::time::timeout(
        std::time::Duration::from_secs(1),
        engine.run_parallel_with(
            &mut store,
            ScanOptions::new(vec![root]).force_recompute(),
            reader,
            &mut processor,
            PipelineLimits::new(48, 48),
            ReadCancellationToken::new(),
            1,
        ),
    )
    .await
    .expect("已完成的读取必须先进入一筛并释放单盘许可，不能等待凑满 24 个 Worker")
    .unwrap();

    assert_eq!(summary.scheduled_stage1, 3);
    assert_eq!(batch_sizes.lock().unwrap().iter().sum::<usize>(), 3);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn same_disk_reads_continue_while_the_first_worker_batch_is_running() {
    let directory = tempfile::tempdir().unwrap();
    let paths = [
        directory.path().join("one.bin"),
        directory.path().join("two.bin"),
        directory.path().join("three.bin"),
    ];
    let mut data = BTreeMap::new();
    for (index, path) in paths.iter().enumerate() {
        let bytes = vec![index as u8 + 1; index + 1];
        std::fs::write(path, &bytes).unwrap();
        data.insert(path.clone(), bytes);
    }
    let rows = paths.iter().map(|path| scanned(path)).collect::<Vec<_>>();
    let enumerator = StreamingEnumerator {
        rows: Arc::new(rows),
        attempted: None,
        emitted: Arc::new(AtomicUsize::new(0)),
    };
    let (completed_tx, mut completed_rx) = mpsc::unbounded_channel();
    let reader = PermitLimitedReader {
        data: Arc::new(data),
        permits: Arc::new(Semaphore::new(2)),
        completed: completed_tx,
    };
    let (started_tx, mut started_rx) = mpsc::unbounded_channel();
    let worker_release = Arc::new(Semaphore::new(0));
    let release_after_observation = worker_release.clone();
    let mut processor = BlockingFirstBatchProcessor {
        started: started_tx,
        release: worker_release,
        blocked_once: false,
    };
    let root = DisplayPath::new(directory.path()).unwrap();
    let mut store = NodeStore::open_in_memory(MachineId::parse(&"44".repeat(32)).unwrap()).unwrap();
    let mut engine = ScanEngine::new(enumerator, SystemMd5, directory.path().join("sheets"));
    let scan = tokio::spawn(async move {
        engine
            .run_parallel_with(
                &mut store,
                ScanOptions::new(vec![root]).force_recompute(),
                reader,
                &mut processor,
                PipelineLimits::new(48, 48),
                ReadCancellationToken::new(),
                1,
            )
            .await
    });

    started_rx.recv().await.unwrap();
    let all_reads_finished = tokio::time::timeout(std::time::Duration::from_secs(1), async {
        for _ in 0..3 {
            completed_rx.recv().await.unwrap();
        }
    })
    .await;
    release_after_observation.add_permits(1);
    all_reads_finished.expect("Worker 处理首批时，同盘后续 MD5 读取必须继续填充流水线");
    tokio::time::timeout(std::time::Duration::from_secs(1), scan)
        .await
        .expect("释放首批 Worker 后扫描必须完成")
        .unwrap()
        .unwrap();
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn disk_b_finishes_while_a_is_blocked_and_reversed_workers_persist_correct_identity() {
    let directory = tempfile::tempdir().unwrap();
    let path_a = directory.path().join("a.bin");
    let path_b = directory.path().join("b.bin");
    let data_a = b"disk-a".repeat(100);
    let data_b = b"disk-b".repeat(101);
    std::fs::write(&path_a, &data_a).unwrap();
    std::fs::write(&path_b, &data_b).unwrap();
    let rows = vec![scanned(&path_a), scanned(&path_b)];
    let enumerator = StreamingEnumerator {
        rows: Arc::new(rows.clone()),
        attempted: None,
        emitted: Arc::new(AtomicUsize::new(0)),
    };
    let gate_a = Arc::new(Semaphore::new(0));
    let (completed_tx, mut completed_rx) = mpsc::unbounded_channel();
    let active_leases = Arc::new(AtomicUsize::new(0));
    let reader = FakePipelineReader {
        data: Arc::new(BTreeMap::from([
            (path_a.clone(), data_a),
            (path_b.clone(), data_b),
        ])),
        gates: Arc::new(BTreeMap::from([(path_a.clone(), gate_a.clone())])),
        completed: completed_tx,
        active_leases: active_leases.clone(),
    };
    let batch_sizes = Arc::new(Mutex::new(Vec::new()));
    let processor = ReversedBatchProcessor {
        batch_sizes: batch_sizes.clone(),
        active_leases: active_leases.clone(),
    };
    let root = DisplayPath::new(directory.path()).unwrap();
    let machine = MachineId::parse(&"41".repeat(32)).unwrap();
    let store = NodeStore::open_in_memory(machine).unwrap();
    let cancellation = ReadCancellationToken::new();
    let task = tokio::spawn(async move {
        let mut store = store;
        let mut engine = ScanEngine::new(enumerator, SystemMd5, directory.path().join("sheets"));
        let mut processor = processor;
        let result = engine
            .run_parallel_with(
                &mut store,
                ScanOptions::new(vec![root]),
                reader,
                &mut processor,
                PipelineLimits::new(4, 4),
                cancellation,
                1,
            )
            .await;
        (store, result)
    });

    assert_eq!(completed_rx.recv().await.unwrap(), path_b);
    gate_a.add_permits(1);
    assert_eq!(completed_rx.recv().await.unwrap(), path_a);
    let (store, summary) = task.await.unwrap();
    assert_eq!(summary.unwrap().scheduled_stage1, 2);
    assert_eq!(*batch_sizes.lock().unwrap(), vec![2]);
    assert_eq!(active_leases.load(Ordering::Acquire), 0);

    for (row, expected_quality) in rows.iter().zip([11, 22]) {
        let lookup = store
            .lookup_scanned_paths(std::slice::from_ref(row))
            .unwrap();
        let content_id = lookup[0].content_id.unwrap();
        let Some(CompleteStage1::Image(feature)) = store.load_complete_stage1(content_id).unwrap()
        else {
            panic!("图片一筛必须按原 item/content 身份写回");
        };
        assert_eq!(feature.quality, expected_quality);
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn bounded_enumerator_stops_emitting_when_downstream_capacity_is_full() {
    let directory = tempfile::tempdir().unwrap();
    let rows = (0..4)
        .map(|index| {
            let path = directory.path().join(format!("{index}.bin"));
            std::fs::write(&path, [index as u8]).unwrap();
            scanned(&path)
        })
        .collect::<Vec<_>>();
    let (attempted_tx, mut attempted_rx) = mpsc::unbounded_channel();
    let emitted = Arc::new(AtomicUsize::new(0));
    let enumerator = StreamingEnumerator {
        rows: Arc::new(rows.clone()),
        attempted: Some(attempted_tx),
        emitted: emitted.clone(),
    };
    let gate = Arc::new(Semaphore::new(0));
    let (completed_tx, _completed_rx) = mpsc::unbounded_channel();
    let reader = FakePipelineReader {
        data: Arc::new(
            rows.iter()
                .map(|row| (row.display_path.as_path().to_path_buf(), vec![1]))
                .collect(),
        ),
        gates: Arc::new(
            rows.iter()
                .map(|row| (row.display_path.as_path().to_path_buf(), gate.clone()))
                .collect(),
        ),
        completed: completed_tx,
        active_leases: Arc::new(AtomicUsize::new(0)),
    };
    let root = DisplayPath::new(directory.path()).unwrap();
    let machine = MachineId::parse(&"42".repeat(32)).unwrap();
    let store = NodeStore::open_in_memory(machine).unwrap();
    let cancellation = ReadCancellationToken::new();
    let task_cancellation = cancellation.clone();
    let task = tokio::spawn(async move {
        let mut store = store;
        let mut engine = ScanEngine::new(enumerator, SystemMd5, directory.path().join("sheets"));
        let mut processor = ReversedBatchProcessor {
            batch_sizes: Arc::new(Mutex::new(Vec::new())),
            active_leases: Arc::new(AtomicUsize::new(0)),
        };
        engine
            .run_parallel_with(
                &mut store,
                ScanOptions::new(vec![root]),
                reader,
                &mut processor,
                PipelineLimits::new(1, 1),
                task_cancellation,
                2,
            )
            .await
    });

    assert_eq!(attempted_rx.recv().await, Some(0));
    assert_eq!(attempted_rx.recv().await, Some(1));
    assert_eq!(attempted_rx.recv().await, Some(2));
    assert_eq!(emitted.load(Ordering::Acquire), 2);
    cancellation.cancel();
    gate.add_permits(1);
    assert!(matches!(task.await.unwrap(), Err(ScanError::Cancelled)));
}

#[tokio::test]
async fn duplicate_enumeration_is_deduplicated_by_the_single_sqlite_writer() {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("duplicate.bin");
    let data = b"duplicate".repeat(31);
    std::fs::write(&path, &data).unwrap();
    let row = scanned(&path);
    let enumerator = StreamingEnumerator {
        rows: Arc::new(vec![row.clone(), row.clone()]),
        attempted: None,
        emitted: Arc::new(AtomicUsize::new(0)),
    };
    let (completed, _completed_rx) = mpsc::unbounded_channel();
    let active_leases = Arc::new(AtomicUsize::new(0));
    let reader = FakePipelineReader {
        data: Arc::new(BTreeMap::from([(path.clone(), data)])),
        gates: Arc::new(BTreeMap::new()),
        completed,
        active_leases: active_leases.clone(),
    };
    let mut processor = ReversedBatchProcessor {
        batch_sizes: Arc::new(Mutex::new(Vec::new())),
        active_leases,
    };
    let root = DisplayPath::new(directory.path()).unwrap();
    let machine = MachineId::parse(&"44".repeat(32)).unwrap();
    let mut store = NodeStore::open_in_memory(machine).unwrap();
    let mut engine = ScanEngine::new(enumerator, SystemMd5, directory.path().join("sheets"));

    let summary = engine
        .run_parallel_with(
            &mut store,
            ScanOptions::new(vec![root]),
            reader,
            &mut processor,
            PipelineLimits::new(2, 2),
            ReadCancellationToken::new(),
            4,
        )
        .await
        .unwrap();

    assert_eq!(summary.total_files, 1);
    assert_eq!(summary.hashed, 1);
    assert_eq!(summary.scheduled_stage1, 1);
    assert_eq!(store.task_snapshot(summary.task_id).unwrap().total_items, 1);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn cancellation_drains_blocking_reads_before_their_leases_are_dropped() {
    let directory = tempfile::tempdir().unwrap();
    let path_a = directory.path().join("holding-a.bin");
    let path_b = directory.path().join("holding-b.bin");
    std::fs::write(&path_a, b"a").unwrap();
    std::fs::write(&path_b, b"b").unwrap();
    let rows = vec![scanned(&path_a), scanned(&path_b)];
    let enumerator = StreamingEnumerator {
        rows: Arc::new(rows),
        attempted: None,
        emitted: Arc::new(AtomicUsize::new(0)),
    };
    let gate_a = Arc::new(Semaphore::new(0));
    let gate_b = Arc::new(Semaphore::new(0));
    let (started, mut started_rx) = mpsc::unbounded_channel();
    let (completed, mut completed_rx) = mpsc::unbounded_channel();
    let active_leases = Arc::new(AtomicUsize::new(0));
    let reader = HoldingPipelineReader {
        data: Arc::new(BTreeMap::from([
            (path_a.clone(), vec![b'a']),
            (path_b.clone(), vec![b'b']),
        ])),
        gates: Arc::new(BTreeMap::from([
            (path_a.clone(), gate_a.clone()),
            (path_b.clone(), gate_b.clone()),
        ])),
        started,
        completed,
        active_leases: active_leases.clone(),
    };
    let machine = MachineId::parse(&"45".repeat(32)).unwrap();
    let store = NodeStore::open_in_memory(machine).unwrap();
    let root = DisplayPath::new(directory.path()).unwrap();
    let cancellation = ReadCancellationToken::new();
    let task_cancellation = cancellation.clone();
    let task = tokio::spawn(async move {
        let mut store = store;
        let mut engine = ScanEngine::new(enumerator, SystemMd5, directory.path().join("sheets"));
        let mut processor = ReversedBatchProcessor {
            batch_sizes: Arc::new(Mutex::new(Vec::new())),
            active_leases: Arc::new(AtomicUsize::new(0)),
        };
        engine
            .run_parallel_with(
                &mut store,
                ScanOptions::new(vec![root]),
                reader,
                &mut processor,
                PipelineLimits::new(2, 2),
                task_cancellation,
                5,
            )
            .await
    });
    let mut started_paths = vec![
        started_rx.recv().await.unwrap(),
        started_rx.recv().await.unwrap(),
    ];
    started_paths.sort();
    assert_eq!(started_paths, vec![path_a.clone(), path_b.clone()]);
    assert_eq!(active_leases.load(Ordering::Acquire), 2);

    cancellation.cancel();
    gate_b.add_permits(1);
    assert_eq!(completed_rx.recv().await.unwrap(), path_b);
    tokio::task::yield_now().await;
    assert!(!task.is_finished(), "另一个 blocking read 尚未 drain");
    assert_eq!(active_leases.load(Ordering::Acquire), 1);

    gate_a.add_permits(1);
    assert_eq!(completed_rx.recv().await.unwrap(), path_a);
    assert!(matches!(task.await.unwrap(), Err(ScanError::Cancelled)));
    assert_eq!(active_leases.load(Ordering::Acquire), 0);
}

#[tokio::test]
async fn rejected_main_stage1_transaction_never_publishes_a_final_contact_sheet() {
    let directory = tempfile::tempdir().unwrap();
    let media = directory.path().join("video.bin");
    std::fs::write(&media, b"video").unwrap();
    let row = scanned(&media);
    let enumerator = StreamingEnumerator {
        rows: Arc::new(vec![row]),
        attempted: None,
        emitted: Arc::new(AtomicUsize::new(0)),
    };
    let (completed, _completed_rx) = mpsc::unbounded_channel();
    let reader = FakePipelineReader {
        data: Arc::new(BTreeMap::from([(media, b"video".to_vec())])),
        gates: Arc::new(BTreeMap::new()),
        completed,
        active_leases: Arc::new(AtomicUsize::new(0)),
    };
    let sheets = directory.path().join("sheets");
    let machine = MachineId::parse(&"46".repeat(32)).unwrap();
    let mut store = NodeStore::open_in_memory(machine).unwrap();
    let mut engine = ScanEngine::new(enumerator, SystemMd5, sheets.clone());
    let root = DisplayPath::new(directory.path()).unwrap();

    let result = engine
        .run_parallel_with(
            &mut store,
            ScanOptions::new(vec![root]),
            reader,
            &mut InvalidVideoProcessor,
            PipelineLimits::new(1, 1),
            ReadCancellationToken::new(),
            6,
        )
        .await;

    assert!(result.is_err());
    assert!(!contains_jpg(&sheets));
}

#[test]
fn contact_reference_failure_removes_only_a_final_owned_by_this_publish_attempt() {
    let directory = tempfile::tempdir().unwrap();
    let temp = directory.path().join("item.partial");
    let final_path = directory.path().join("content.jpg");
    std::fs::write(&temp, b"new").unwrap();
    let error = publish_contact_sheet_for_test(&temp, &final_path, || {
        Err(ScanError::Stage1("ref failed".into()))
    })
    .unwrap_err();
    assert!(error.to_string().contains("ref failed"));
    assert!(!temp.exists());
    assert!(!final_path.exists());

    std::fs::write(&final_path, b"existing").unwrap();
    std::fs::write(&temp, b"newer").unwrap();
    assert!(
        publish_contact_sheet_for_test(&temp, &final_path, || {
            Err(ScanError::Stage1("ref failed again".into()))
        })
        .is_err()
    );
    assert_eq!(std::fs::read(&final_path).unwrap(), b"existing");
    assert!(!temp.exists());
}

#[test]
fn cancelled_item_rejects_late_stage1_without_feature_side_effects() {
    let machine = MachineId::parse(&"43".repeat(32)).unwrap();
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let task = store
        .create_scan_task(&[NormalizedPath::new(r"D:\Late").unwrap()], 1)
        .unwrap();
    let row = ScannedPath::new(
        NormalizedPath::new(r"D:\Late\a.jpg").unwrap(),
        DisplayPath::new(r"D:\Late\a.jpg").unwrap(),
        7,
    );
    let content = store
        .upsert_content_and_location(&row, [7; 16], MediaKind::Other)
        .unwrap();
    let item_id = store
        .append_task_item(
            task,
            &NewTaskItem::for_content(
                dedup_core::LocationKey::new(machine, row.normalized_path),
                row.display_path,
                row.file_size,
                content.id,
                "probe_stage1",
            ),
            1,
        )
        .unwrap();
    assert_eq!(
        store.claim_next_item(task, 1).unwrap().unwrap().item_id,
        item_id
    );
    store.cancel_task(task, 2).unwrap();

    let committed = store
        .commit_scan_stage1_if_running(
            &item_id,
            content.id,
            MediaKind::Image,
            vec![FeatureWrite::ImageStage1(ImageStage1Fields {
                width: Some(1),
                height: Some(1),
                pdq: Some(PdqHash::from_bytes([9; 32])),
                quality: Some(99),
            })],
            3,
        )
        .unwrap();

    assert!(!committed);
    assert_eq!(
        store.content_media_kind(content.id).unwrap(),
        MediaKind::Other
    );
    assert!(store.load_complete_stage1(content.id).unwrap().is_none());
}

fn image_output(request: &Stage1Request) -> Stage1Output {
    let is_a = request.display_path.as_path().file_name().unwrap() == "a.bin";
    Stage1Output {
        media_kind: MediaKind::Image,
        width: 1,
        height: 1,
        duration_ms: None,
        frames: vec![Stage1Frame {
            slot: 0,
            feature: Some(ImageStage1 {
                width: 1,
                height: 1,
                pdq: PdqHash::from_bytes([if is_a { 11 } else { 22 }; 32]),
                quality: if is_a { 11 } else { 22 },
            }),
            error: None,
        }],
        contact_sheet_jpeg: None,
    }
}

fn scanned(path: &Path) -> ScannedPath {
    ScannedPath::new(
        NormalizedPath::new(path).unwrap(),
        DisplayPath::new(path).unwrap(),
        std::fs::metadata(path).unwrap().len(),
    )
}

fn contains_jpg(root: &Path) -> bool {
    if !root.exists() {
        return false;
    }
    let mut pending = vec![root.to_path_buf()];
    while let Some(directory) = pending.pop() {
        for entry in std::fs::read_dir(directory).unwrap() {
            let path = entry.unwrap().path();
            if path.is_dir() {
                pending.push(path);
            } else if path.extension().is_some_and(|extension| extension == "jpg") {
                return true;
            }
        }
    }
    false
}
