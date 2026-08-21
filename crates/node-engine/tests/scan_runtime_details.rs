use std::{future::Future, fs, pin::Pin};

use dedup_core::{DisplayPath, MachineId, MediaKind, NormalizedPath};
use dedup_node_engine::{
    io::ReadFailure,
    runtime_tasks::{
        RuntimeProgressUnit, RuntimeStage, RuntimeTaskKind, RuntimeTaskRegistry,
        RuntimeWorkerUpdate,
    },
    scan::{
        FileEnumerator, PipelineFileReader, PipelineLimits, ReadProduct, ScanEngine, ScanError,
        ScanOptions, Stage1Processor, Stage1Request, SystemMd5, md5_bytes,
    },
    worker::Stage1Output,
};
use dedup_node_store::{NodeStore, ScannedPath};
use dedup_windows::ReadCancellationToken;

#[derive(Clone)]
struct Rows(Vec<ScannedPath>);
impl FileEnumerator for Rows {
    fn enumerate(&self, _: &[DisplayPath]) -> Result<Vec<ScannedPath>, ScanError> {
        Ok(self.0.clone())
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
}
