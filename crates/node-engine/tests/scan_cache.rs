use std::{fs, path::Path};

use dedup_core::{DisplayPath, MachineId, MediaKind, NormalizedPath};
use dedup_node_engine::{
    scan::{
        FileEnumerator, FileHasher, ScanEngine, ScanError, ScanOptions, Stage1Processor,
        Stage1Request,
    },
    worker::{Stage1Frame, Stage1Output},
};
use dedup_node_store::{NodeStore, ScannedPath};
use tempfile::tempdir;

#[derive(Default)]
struct CountingHasher {
    reads: usize,
}

impl FileHasher for CountingHasher {
    fn md5(&mut self, path: &Path) -> Result<[u8; 16], ScanError> {
        self.reads += 1;
        dedup_node_engine::scan::md5_file(path)
    }
}

struct FixedEnumerator;

impl FileEnumerator for FixedEnumerator {
    fn enumerate(&self, roots: &[DisplayPath]) -> Result<Vec<ScannedPath>, ScanError> {
        let path = roots[0].as_path().join("sample.bin");
        Ok(vec![scanned(&path)])
    }
}

#[derive(Default)]
struct OtherProcessor {
    calls: usize,
}

impl Stage1Processor for OtherProcessor {
    async fn process(&mut self, _request: Stage1Request) -> Result<Stage1Output, String> {
        self.calls += 1;
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
async fn path_size_cache_skips_md5_and_force_recompute_bypasses_it() {
    let directory = tempdir().unwrap();
    let path = directory.path().join("sample.bin");
    fs::write(&path, b"first").unwrap();
    let root = DisplayPath::new(directory.path()).unwrap();
    let machine = MachineId::parse(&"11".repeat(32)).unwrap();
    let mut store = NodeStore::open_in_memory(machine).unwrap();
    let mut engine = ScanEngine::new(
        FixedEnumerator,
        CountingHasher::default(),
        directory.path().join("contact-sheets"),
    );
    let mut processor = OtherProcessor::default();

    let first = engine
        .run(
            &mut store,
            ScanOptions::new(vec![root.clone()]),
            &mut processor,
            1,
        )
        .await
        .unwrap();
    assert_eq!(engine.hasher().reads, 1);
    assert_eq!(processor.calls, 1);
    assert_eq!(first.hashed, 1);
    let initial_key = store.lookup_scanned_paths(&[scanned(&path)]).unwrap()[0]
        .content_key()
        .unwrap();

    let second = engine
        .run(
            &mut store,
            ScanOptions::new(vec![root.clone()]),
            &mut processor,
            2,
        )
        .await
        .unwrap();
    assert_eq!(
        engine.hasher().reads,
        1,
        "路径、大小、机器均命中时不得读文件"
    );
    assert_eq!(processor.calls, 1);
    assert_eq!(second.cache_hits, 1);

    fs::write(&path, b"longer").unwrap();
    engine
        .run(
            &mut store,
            ScanOptions::new(vec![root.clone()]),
            &mut processor,
            3,
        )
        .await
        .unwrap();
    assert_eq!(engine.hasher().reads, 2, "大小变化必须重新计算 MD5");
    let key_before_same_size_replacement = store.lookup_scanned_paths(&[scanned(&path)]).unwrap()
        [0]
    .content_key()
    .unwrap();
    assert_ne!(key_before_same_size_replacement, initial_key);

    fs::write(&path, b"second").unwrap();
    engine
        .run(
            &mut store,
            ScanOptions::new(vec![root.clone()]),
            &mut processor,
            4,
        )
        .await
        .unwrap();
    assert_eq!(
        engine.hasher().reads,
        2,
        "同大小替换的普通扫描按已确认规则复用"
    );
    assert_eq!(
        store.lookup_scanned_paths(&[scanned(&path)]).unwrap()[0].content_key(),
        Some(key_before_same_size_replacement)
    );

    let forced = engine
        .run(
            &mut store,
            ScanOptions::new(vec![root]).force_recompute(),
            &mut processor,
            5,
        )
        .await
        .unwrap();
    assert_eq!(engine.hasher().reads, 3);
    assert_eq!(processor.calls, 3);
    assert_eq!(forced.scheduled_stage1, 1);
    assert_ne!(
        store.lookup_scanned_paths(&[scanned(&path)]).unwrap()[0].content_key(),
        Some(key_before_same_size_replacement),
        "强制重算必须把同路径引用更新到实际新内容"
    );
}

#[tokio::test]
async fn reused_incomplete_media_is_skipped_until_force_recompute() {
    let directory = tempdir().unwrap();
    let existing_path = directory.path().join("existing.bin");
    let path = directory.path().join("sample.bin");
    fs::write(&existing_path, b"not-really-an-image").unwrap();
    fs::write(&path, b"not-really-an-image").unwrap();
    let root = DisplayPath::new(directory.path()).unwrap();
    let machine = MachineId::parse(&"22".repeat(32)).unwrap();
    let mut store = NodeStore::open_in_memory(machine).unwrap();
    let md5 = dedup_node_engine::scan::md5_file(&existing_path).unwrap();
    store
        .upsert_content_and_location(&scanned(&existing_path), md5, MediaKind::Image)
        .unwrap();
    let mut engine = ScanEngine::new(
        FixedEnumerator,
        CountingHasher::default(),
        directory.path().join("contact-sheets"),
    );
    let mut processor = OtherProcessor::default();

    let normal = engine
        .run(
            &mut store,
            ScanOptions::new(vec![root.clone()]),
            &mut processor,
            10,
        )
        .await
        .unwrap();
    assert_eq!(normal.skipped_incomplete, 1);
    assert_eq!(processor.calls, 0);
    assert_eq!(
        engine.hasher().reads,
        1,
        "新路径需要 MD5 后才能命中内容索引"
    );

    engine
        .run(
            &mut store,
            ScanOptions::new(vec![root]).force_recompute(),
            &mut processor,
            11,
        )
        .await
        .unwrap();
    assert_eq!(processor.calls, 1);
    assert_eq!(engine.hasher().reads, 2);
}

fn scanned(path: &Path) -> ScannedPath {
    ScannedPath::new(
        NormalizedPath::new(path).unwrap(),
        DisplayPath::new(path).unwrap(),
        fs::metadata(path).unwrap().len(),
    )
}

#[allow(dead_code)]
fn image_output() -> Stage1Output {
    Stage1Output {
        media_kind: MediaKind::Image,
        width: 1,
        height: 1,
        duration_ms: None,
        frames: vec![Stage1Frame {
            slot: 0,
            feature: None,
            error: Some("fixture".into()),
        }],
        contact_sheet_jpeg: None,
    }
}
