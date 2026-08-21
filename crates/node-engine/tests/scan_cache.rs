use std::{future::Future, fs, path::Path, pin::Pin};

use dedup_core::{DisplayPath, MachineId, MediaKind, NormalizedPath};
use dedup_node_engine::{
    scan::{
        FileEnumerator, FileHasher, PipelineFileReader, PipelineLimits, ReadProduct, ScanEngine,
        ScanError, ScanOptions, Stage1Processor, Stage1Request, md5_bytes,
    },
    worker::{Stage1Frame, Stage1Output},
};
use dedup_node_store::{NodeStore, ScannedPath};
use dedup_windows::ReadCancellationToken;
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

#[derive(Clone, Copy)]
struct FixedEnumerator;

impl FileEnumerator for FixedEnumerator {
    fn enumerate(&self, roots: &[DisplayPath]) -> Result<Vec<ScannedPath>, ScanError> {
        let path = roots[0].as_path().join("sample.bin");
        Ok(vec![scanned(&path)])
    }
}

#[derive(Clone, Copy)]
struct ImmediateReader;

impl PipelineFileReader for ImmediateReader {
    type Lease = ();

    fn read(
        &self,
        scanned: ScannedPath,
        cancellation: ReadCancellationToken,
    ) -> Pin<
        Box<
            dyn Future<Output = Result<ReadProduct<Self::Lease>, dedup_node_engine::io::ReadFailure>>
                + Send
                + 'static,
        >,
    > {
        Box::pin(async move {
            if cancellation.is_cancelled() {
                return Err(dedup_node_engine::io::ReadFailure::Cancelled);
            }
            let bytes = fs::read(scanned.display_path.as_path()).map_err(|source| {
                dedup_node_engine::io::ReadFailure::Io {
                    path: scanned.display_path.as_path().to_path_buf(),
                    block_offset: 0,
                    source,
                }
            })?;
            Ok(ReadProduct {
                md5: md5_bytes(&bytes),
                lease: (),
            })
        })
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

#[derive(Default)]
struct VideoContactSheetProcessor {
    calls: usize,
    encodes: usize,
    generation_requests: Vec<bool>,
}

impl Stage1Processor for VideoContactSheetProcessor {
    async fn process(&mut self, request: Stage1Request) -> Result<Stage1Output, String> {
        self.calls += 1;
        self.generation_requests
            .push(request.generate_contact_sheet);
        let contact_sheet_jpeg = request.generate_contact_sheet.then(|| {
            self.encodes += 1;
            b"generated-jpeg".to_vec()
        });
        Ok(Stage1Output {
            media_kind: MediaKind::Video,
            width: 1,
            height: 1,
            duration_ms: Some(1_000),
            frames: (0..6)
                .map(|slot| Stage1Frame {
                    slot,
                    feature: None,
                    error: Some("fixture".into()),
                })
                .collect(),
            contact_sheet_jpeg,
        })
    }
}

#[tokio::test]
async fn contact_sheet_md5_path_reuses_existing_file_and_repairs_reference() {
    let directory = tempdir().unwrap();
    let path = directory.path().join("sample.bin");
    fs::write(&path, b"existing-video").unwrap();
    let root = DisplayPath::new(directory.path()).unwrap();
    let machine = MachineId::parse(&"33".repeat(32)).unwrap();
    let mut store = NodeStore::open_in_memory(machine).unwrap();
    let contact_root = directory.path().join("contact-sheets");
    let existing_md5 = dedup_node_engine::scan::md5_file(&path).unwrap();
    let existing_hex = hex_md5(existing_md5);
    let existing_target = contact_root
        .join(&existing_hex[..2])
        .join(format!("{existing_hex}.jpg"));
    fs::create_dir_all(existing_target.parent().unwrap()).unwrap();
    fs::write(&existing_target, b"existing-jpeg").unwrap();
    let mut engine = ScanEngine::new(FixedEnumerator, CountingHasher::default(), &contact_root);
    let mut processor = VideoContactSheetProcessor::default();

    engine
        .run_parallel_with(
            &mut store,
            ScanOptions::new(vec![root.clone()]).force_recompute(),
            ImmediateReader,
            &mut processor,
            PipelineLimits::new(2, 2),
            ReadCancellationToken::new(),
            20,
        )
        .await
        .unwrap();
    let existing_content = store.lookup_scanned_paths(&[scanned(&path)]).unwrap()[0]
        .content_id
        .unwrap();
    assert_eq!(processor.calls, 1, "强制重算仍须执行 probe 和一筛");
    assert_eq!(processor.encodes, 0, "已有 MD5 联系表不得重复编码");
    assert_eq!(processor.generation_requests, vec![false]);
    assert_eq!(fs::read(&existing_target).unwrap(), b"existing-jpeg");
    let existing_relative = format!(
        "contact-sheets/{}/{}.jpg",
        &existing_hex[..2],
        existing_hex
    );
    assert_eq!(
        store.contact_sheet_path(existing_content).unwrap().as_deref(),
        Some(existing_relative.as_str())
    );

    fs::write(&path, b"new-video-content").unwrap();
    let generated_md5 = dedup_node_engine::scan::md5_file(&path).unwrap();
    let generated_hex = hex_md5(generated_md5);
    let generated_target = contact_root
        .join(&generated_hex[..2])
        .join(format!("{generated_hex}.jpg"));
    engine
        .run_parallel_with(
            &mut store,
            ScanOptions::new(vec![root.clone()]).force_recompute(),
            ImmediateReader,
            &mut processor,
            PipelineLimits::new(2, 2),
            ReadCancellationToken::new(),
            21,
        )
        .await
        .unwrap();
    assert_eq!(processor.encodes, 1, "缺少目标时只编码一次");
    assert_eq!(fs::read(&generated_target).unwrap(), b"generated-jpeg");
    assert!(
        generated_target
            .parent()
            .unwrap()
            .read_dir()
            .unwrap()
            .all(|entry| !entry.unwrap().file_name().to_string_lossy().contains("partial")),
        "发布后同目录不得遗留 partial"
    );

    engine
        .run_parallel_with(
            &mut store,
            ScanOptions::new(vec![root]).force_recompute(),
            ImmediateReader,
            &mut processor,
            PipelineLimits::new(2, 2),
            ReadCancellationToken::new(),
            22,
        )
        .await
        .unwrap();
    assert_eq!(processor.encodes, 1, "同一 MD5 的后续强制重算仍须复用");
    assert_eq!(processor.generation_requests, vec![false, true, false]);
    assert_eq!(generated_hex.len(), 32);
    assert_eq!(generated_hex, generated_hex.to_ascii_lowercase());
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

fn hex_md5(md5: [u8; 16]) -> String {
    md5.iter().map(|byte| format!("{byte:02x}")).collect()
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
