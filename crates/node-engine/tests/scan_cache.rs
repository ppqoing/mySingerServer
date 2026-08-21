use std::{future::Future, fs, path::Path, pin::Pin};

use dedup_core::{DisplayPath, MachineId, MediaKind, NormalizedPath};
use dedup_node_engine::{
    scan::{
        FileEnumerator, FileHasher, PipelineFileReader, PipelineLimits, ReadProduct, ScanEngine,
        ScanError, ScanOptions, Stage1BatchResult, Stage1ProcessError, Stage1Processor,
        Stage1Request, md5_bytes,
    },
    worker::{Stage1Frame, Stage1Output},
};
use dedup_node_store::{NodeStore, ScannedPath, TaskItemStatus};
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

#[derive(Clone)]
struct RowsEnumerator {
    rows: Vec<ScannedPath>,
}

impl FileEnumerator for RowsEnumerator {
    fn enumerate(&self, _roots: &[DisplayPath]) -> Result<Vec<ScannedPath>, ScanError> {
        Ok(self.rows.clone())
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
    batch_generation_requests: Vec<Vec<bool>>,
    fail_first_generation: bool,
    failed_generations: usize,
}

impl Stage1Processor for VideoContactSheetProcessor {
    async fn process(&mut self, request: Stage1Request) -> Result<Stage1Output, String> {
        self.calls += 1;
        self.generation_requests
            .push(request.generate_contact_sheet);
        if request.generate_contact_sheet
            && self.fail_first_generation
            && self.failed_generations == 0
        {
            self.failed_generations += 1;
            return Err("controlled leader failure".into());
        }
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

    async fn process_batch(&mut self, requests: Vec<Stage1Request>) -> Vec<Stage1BatchResult> {
        self.batch_generation_requests.push(
            requests
                .iter()
                .map(|request| request.generate_contact_sheet)
                .collect(),
        );
        let mut results = Vec::with_capacity(requests.len());
        for request in requests {
            let item_id = request.item_id.clone();
            results.push(Stage1BatchResult {
                item_id,
                output: self
                    .process(request)
                    .await
                    .map_err(Stage1ProcessError::Processing),
            });
        }
        results
    }

    fn max_in_flight(&self) -> usize {
        4
    }
}

#[tokio::test]
async fn contact_sheet_md5_coalesces_same_digest_to_one_batch_leader() {
    let directory = tempdir().unwrap();
    let path_a = directory.path().join("same-a.bin");
    let path_b = directory.path().join("same-b.bin");
    fs::write(&path_a, b"same-video-content").unwrap();
    fs::write(&path_b, b"same-video-content").unwrap();
    let rows = vec![scanned(&path_a), scanned(&path_b)];
    let root = DisplayPath::new(directory.path()).unwrap();
    let machine = MachineId::parse(&"44".repeat(32)).unwrap();
    let mut store = NodeStore::open_in_memory(machine).unwrap();
    let contact_root = directory.path().join("contact-sheets");
    let md5 = dedup_node_engine::scan::md5_file(&path_a).unwrap();
    let digest = hex_md5(md5);
    let target = contact_root
        .join(&digest[..2])
        .join(format!("{digest}.jpg"));
    let mut engine = ScanEngine::new(
        RowsEnumerator { rows: rows.clone() },
        CountingHasher::default(),
        &contact_root,
    );
    let mut processor = VideoContactSheetProcessor::default();

    let summary = engine
        .run_parallel_with(
            &mut store,
            ScanOptions::new(vec![root]).force_recompute(),
            ImmediateReader,
            &mut processor,
            PipelineLimits::new(4, 4),
            ReadCancellationToken::new(),
            30,
        )
        .await
        .unwrap();

    assert_eq!(processor.encodes, 1, "同批相同 MD5 只能有一个编码 leader");
    assert_eq!(
        processor.batch_generation_requests,
        vec![vec![true], vec![false]],
        "follower 必须等 leader 发布后才以不编码请求进入下一波"
    );
    assert_eq!(fs::read(&target).unwrap(), b"generated-jpeg");
    let items = store.task_items(summary.task_id).unwrap();
    assert_eq!(items.len(), 2);
    assert!(
        items
            .iter()
            .all(|item| item.status == TaskItemStatus::Succeeded)
    );
    let lookups = store.lookup_scanned_paths(&rows).unwrap();
    assert_eq!(lookups[0].content_id, lookups[1].content_id);
    let content_id = lookups[0].content_id.unwrap();
    let relative_path = format!("contact-sheets/{}/{}.jpg", &digest[..2], digest);
    assert_eq!(
        store.contact_sheet_path(content_id).unwrap().as_deref(),
        Some(relative_path.as_str())
    );
}

#[tokio::test]
async fn contact_sheet_md5_promotes_follower_after_leader_failure() {
    let directory = tempdir().unwrap();
    let path_a = directory.path().join("failure-a.bin");
    let path_b = directory.path().join("failure-b.bin");
    fs::write(&path_a, b"retry-same-video").unwrap();
    fs::write(&path_b, b"retry-same-video").unwrap();
    let rows = vec![scanned(&path_a), scanned(&path_b)];
    let root = DisplayPath::new(directory.path()).unwrap();
    let machine = MachineId::parse(&"55".repeat(32)).unwrap();
    let mut store = NodeStore::open_in_memory(machine).unwrap();
    let contact_root = directory.path().join("contact-sheets");
    let md5 = dedup_node_engine::scan::md5_file(&path_a).unwrap();
    let digest = hex_md5(md5);
    let target = contact_root
        .join(&digest[..2])
        .join(format!("{digest}.jpg"));
    let mut engine = ScanEngine::new(
        RowsEnumerator { rows },
        CountingHasher::default(),
        &contact_root,
    );
    let mut processor = VideoContactSheetProcessor {
        fail_first_generation: true,
        ..VideoContactSheetProcessor::default()
    };

    let summary = engine
        .run_parallel_with(
            &mut store,
            ScanOptions::new(vec![root]).force_recompute(),
            ImmediateReader,
            &mut processor,
            PipelineLimits::new(4, 4),
            ReadCancellationToken::new(),
            31,
        )
        .await
        .unwrap();

    assert_eq!(processor.failed_generations, 1);
    assert_eq!(processor.encodes, 1, "提升后的 follower 只编码一次");
    assert_eq!(
        processor.batch_generation_requests,
        vec![vec![true], vec![true]],
        "失败 leader 收尾后才能提升下一 follower"
    );
    assert_eq!(summary.file_failures, 1);
    let items = store.task_items(summary.task_id).unwrap();
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
    assert_eq!(fs::read(&target).unwrap(), b"generated-jpeg");
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
