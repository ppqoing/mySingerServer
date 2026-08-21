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
    },
    worker::{Stage1Frame, Stage1Output},
};
use dedup_node_store::{
    CompleteStage1, FeatureWrite, ImageStage1Fields, NewTaskItem, NodeStore, ScannedPath,
};
use dedup_windows::ReadCancellationToken;
use tokio::sync::{Semaphore, mpsc};

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

impl Stage1Processor for ReversedBatchProcessor {
    async fn process(&mut self, request: Stage1Request) -> Result<Stage1Output, String> {
        Ok(image_output(&request))
    }

    fn max_in_flight(&self) -> usize {
        2
    }

    async fn process_batch(&mut self, requests: Vec<Stage1Request>) -> Vec<Stage1BatchResult> {
        self.batch_sizes.lock().unwrap().push(requests.len());
        assert!(self.active_leases.load(Ordering::Acquire) >= requests.len());
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
