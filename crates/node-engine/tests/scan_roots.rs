use std::{fs, path::Path};

use dedup_core::{DisplayPath, MachineId, MediaKind, NormalizedPath};
use dedup_node_engine::{
    scan::{
        FileEnumerator, ScanEngine, ScanError, ScanOptions, Stage1Processor, Stage1Request,
        SystemMd5,
    },
    worker::Stage1Output,
};
use dedup_node_store::{NodeStore, ScannedPath, TaskStatus};

struct Rows(Vec<ScannedPath>);

impl FileEnumerator for Rows {
    fn enumerate(&self, _roots: &[DisplayPath]) -> Result<Vec<ScannedPath>, ScanError> {
        Ok(self.0.clone())
    }
}

struct FailingEnumerator;

impl FileEnumerator for FailingEnumerator {
    fn enumerate(&self, _roots: &[DisplayPath]) -> Result<Vec<ScannedPath>, ScanError> {
        Err(ScanError::Enumeration("fixture failure".into()))
    }
}

struct OtherProcessor;

impl Stage1Processor for OtherProcessor {
    async fn process(&mut self, _request: Stage1Request) -> Result<Stage1Output, String> {
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
async fn successful_partial_root_scan_deactivates_only_exact_component_boundary() {
    let machine = MachineId::parse(&"33".repeat(32)).unwrap();
    let mut store = NodeStore::open_in_memory(machine).unwrap();
    seed(&mut store, r"D:\A\old.jpg", b"old-a");
    seed(&mut store, r"D:\B\keep.jpg", b"keep-b");
    seed(&mut store, r"D:\AB\keep.jpg", b"keep-ab");
    let mut engine = ScanEngine::new(Rows(Vec::new()), SystemMd5, Path::new(r"D:\cache"));
    let mut processor = OtherProcessor;

    let result = engine
        .run(
            &mut store,
            ScanOptions::new(vec![DisplayPath::new(r"D:\A").unwrap()]),
            &mut processor,
            20,
        )
        .await
        .unwrap();

    assert!(
        !store
            .is_location_active(&NormalizedPath::new(r"D:\A\old.jpg").unwrap())
            .unwrap()
    );
    assert!(
        store
            .is_location_active(&NormalizedPath::new(r"D:\B\keep.jpg").unwrap())
            .unwrap()
    );
    assert!(
        store
            .is_location_active(&NormalizedPath::new(r"D:\AB\keep.jpg").unwrap())
            .unwrap()
    );
    assert!(result.outbox_high_seq > 0);
    assert_eq!(
        store.task_snapshot(result.task_id).unwrap().status,
        TaskStatus::Completed
    );
    assert_eq!(result.outbox_high_seq, store.outbox_high_seq().unwrap());
}

#[tokio::test]
async fn failed_enumeration_keeps_all_existing_locations_active() {
    let machine = MachineId::parse(&"44".repeat(32)).unwrap();
    let mut store = NodeStore::open_in_memory(machine).unwrap();
    seed(&mut store, r"D:\A\old.jpg", b"old-a");
    let before = store.outbox_high_seq().unwrap();
    let mut engine = ScanEngine::new(FailingEnumerator, SystemMd5, Path::new(r"D:\cache"));
    let mut processor = OtherProcessor;

    let error = engine
        .run(
            &mut store,
            ScanOptions::new(vec![DisplayPath::new(r"D:\A").unwrap()]),
            &mut processor,
            21,
        )
        .await
        .unwrap_err();

    assert!(matches!(error, ScanError::Enumeration(_)));
    assert!(
        store
            .is_location_active(&NormalizedPath::new(r"D:\A\old.jpg").unwrap())
            .unwrap()
    );
    assert_eq!(store.outbox_high_seq().unwrap(), before);
}

fn seed(store: &mut NodeStore, path: &str, bytes: &[u8]) {
    let md5 = dedup_node_engine::scan::md5_bytes(bytes);
    let scanned = ScannedPath::new(
        NormalizedPath::new(path).unwrap(),
        DisplayPath::new(path).unwrap(),
        bytes.len() as u64,
    );
    store
        .upsert_content_and_location(&scanned, md5, MediaKind::Other)
        .unwrap();
}

#[allow(dead_code)]
fn make_file(path: &Path, bytes: &[u8]) -> ScannedPath {
    fs::write(path, bytes).unwrap();
    ScannedPath::new(
        NormalizedPath::new(path).unwrap(),
        DisplayPath::new(path).unwrap(),
        bytes.len() as u64,
    )
}
