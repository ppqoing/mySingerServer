use std::{path::Path, time::Duration};

use dedup_core::{DisplayPath, MachineId, MediaKind, NormalizedPath};
use dedup_media::{ImageStage1, ImageStage2, PdqHash};
use dedup_node_engine::{
    scan::{BaseTaskInput, BaseTaskProducer, PlannedScannedPath, TaskDiskLane},
    task_dispatch::{TaskFileDispatcher, TaskLanePermitFuture, TaskLanePermitProvider},
    task_files::{TaskFileRecord, TransientTaskFileSet},
};
use dedup_node_store::{BaseCacheRecord, CompleteStage1, NodeStore, ScannedPath};
use dedup_windows::{LocalDiskKind, PhysicalDiskId, ReadCancellationToken};

#[derive(Clone, Copy, Debug, Default)]
struct ImmediatePermitProvider;

#[derive(Debug)]
struct ImmediatePermit;

impl TaskLanePermitProvider for ImmediatePermitProvider {
    type Permit = ImmediatePermit;

    fn acquire(
        &self,
        _lane: TaskDiskLane,
        _class: dedup_node_engine::io::DiskReadClass,
        _cancellation: ReadCancellationToken,
    ) -> TaskLanePermitFuture<Self::Permit> {
        Box::pin(async { Ok(ImmediatePermit) })
    }
}

fn lane(numbers: &[u32], kind: LocalDiskKind, weight: usize) -> TaskDiskLane {
    TaskDiskLane {
        physical_disk_id: PhysicalDiskId::from_disk_numbers(numbers.iter().copied()).unwrap(),
        physical_disk_numbers: numbers.to_vec(),
        disk_kind: kind,
        configured_weight: weight,
        per_disk_limit: weight,
    }
}

fn scanned(path: &str, file_size: u64) -> ScannedPath {
    ScannedPath::new(
        NormalizedPath::new(path).unwrap(),
        DisplayPath::new(path).unwrap(),
        file_size,
    )
}

fn input(path: ScannedPath, lane: TaskDiskLane, cached: Option<BaseCacheRecord>) -> BaseTaskInput {
    BaseTaskInput {
        planned: PlannedScannedPath {
            scanned: path,
            lane,
        },
        cached,
        contact_sheet_valid: true,
        force_recompute: false,
    }
}

fn producer(root: &Path) -> BaseTaskProducer<ImmediatePermitProvider> {
    let files = TransientTaskFileSet::create(root, "01900000-0000-7000-8000-000000000001").unwrap();
    BaseTaskProducer::new(TaskFileDispatcher::new(files, ImmediatePermitProvider))
}

fn imported_record(path: &ScannedPath, md5: [u8; 16], complete: bool) -> BaseCacheRecord {
    let mut store = NodeStore::open_in_memory(MachineId::from_sha256([0x41; 32])).unwrap();
    let content = store
        .upsert_content_and_location(path, md5, MediaKind::Other)
        .unwrap();
    if complete {
        store.mark_base_complete(content.id).unwrap();
    }
    store.load_base_cache_record(content.id).unwrap()
}

fn zero_image_record(path: &ScannedPath) -> BaseCacheRecord {
    let mut record = imported_record(path, [0x29; 16], true);
    record.media_kind = MediaKind::Image;
    record.width = Some(1);
    record.height = Some(1);
    record.stage1 = Some(CompleteStage1::Image(ImageStage1 {
        width: 1,
        height: 1,
        pdq: PdqHash::from_bytes([0; 32]),
        quality: 0,
    }));
    record.image_stage2 = Some(ImageStage2 {
        phash_parts: [0; 9],
        sobel: [0.0; 128],
    });
    record
}

async fn collect_records(
    mut dispatcher: TaskFileDispatcher<ImmediatePermitProvider>,
) -> Vec<TaskFileRecord> {
    let cancellation = ReadCancellationToken::new();
    let mut records = Vec::new();
    loop {
        let task = tokio::time::timeout(
            Duration::from_secs(1),
            dispatcher.next(cancellation.clone()),
        )
        .await
        .unwrap()
        .unwrap();
        let Some(task) = task else {
            break;
        };
        let identity = task.identity.clone();
        let permit = task.permit;
        records.push(task.record.clone());
        drop(permit);
        dispatcher.mark_completed(&identity).unwrap();
    }
    records
}

#[tokio::test]
async fn classifies_three_items_by_lane_and_appends_only_missing_rows() {
    let root = tempfile::tempdir().unwrap();
    let hdd = lane(&[7], LocalDiskKind::Hdd, 1);
    let ssd = lane(&[8], LocalDiskKind::Ssd, 2);
    let hit_path = scanned(r"C:\media\hit.bin", 42);
    let partial_path = scanned(r"C:\media\partial.bin", 42);
    let miss_path = scanned(r"D:\media\miss.bin", 42);
    let hit = imported_record(&hit_path, [1; 16], true);
    let partial = imported_record(&partial_path, [2; 16], false);

    let mut producer = producer(root.path());
    producer
        .append_batch(&[
            input(hit_path.clone(), hdd.clone(), Some(hit.clone())),
            input(partial_path.clone(), hdd.clone(), Some(partial.clone())),
            input(miss_path.clone(), ssd.clone(), None),
        ])
        .unwrap();
    let output = producer.seal().unwrap();

    assert_eq!(output.manifest.cache_hits, 1);
    assert_eq!(output.manifest.seen_paths.len(), 3);
    assert_eq!(output.manifest.resolved_files.len(), 1);
    assert_eq!(
        output.manifest.resolved_files[0].scanned.normalized_path,
        hit_path.normalized_path
    );
    assert_eq!(output.manifest.resolved_files[0].content, hit.content_key);
    assert!(output.dispatcher.lane_path(&hdd).unwrap().exists());
    assert!(output.dispatcher.lane_path(&ssd).unwrap().exists());

    let records = collect_records(output.dispatcher).await;
    assert_eq!(records.len(), 2);
    assert_eq!(
        records[0].scanned.normalized_path,
        partial_path.normalized_path
    );
    assert_eq!(records[0].known_md5, Some([2; 16]));
    assert_eq!(records[0].missing.base_missing_parts(), 3);
    assert!(!records[0].missing.needs_md5());
    assert_eq!(
        records[1].scanned.normalized_path,
        miss_path.normalized_path
    );
    assert_eq!(records[1].known_md5, None);
    assert!(records[1].missing.needs_md5());
    assert_eq!(records[1].missing.base_missing_parts(), 0);
}

#[tokio::test]
async fn full_cache_hits_are_resolved_without_creating_task_files() {
    let root = tempfile::tempdir().unwrap();
    let hdd = lane(&[7], LocalDiskKind::Hdd, 1);
    let first = scanned(r"C:\media\first.bin", 42);
    let second = scanned(r"C:\media\second.bin", 42);
    let mut producer = producer(root.path());
    producer
        .append_batch(&[
            input(
                first.clone(),
                hdd.clone(),
                Some(imported_record(&first, [3; 16], true)),
            ),
            input(
                second.clone(),
                hdd.clone(),
                Some(imported_record(&second, [4; 16], true)),
            ),
        ])
        .unwrap();
    let output = producer.seal().unwrap();

    assert_eq!(output.manifest.cache_hits, 2);
    assert_eq!(output.manifest.seen_paths.len(), 2);
    assert_eq!(output.manifest.resolved_files.len(), 2);
    assert!(!output.dispatcher.lane_path(&hdd).unwrap().exists());
    let records = collect_records(output.dispatcher).await;
    assert!(records.is_empty());
}

#[tokio::test]
async fn legal_zero_valued_features_remain_a_complete_cache_hit() {
    let root = tempfile::tempdir().unwrap();
    let lane = lane(&[7], LocalDiskKind::Hdd, 1);
    let path = scanned(r"C:\media\zero-image.bin", 42);
    let mut producer = producer(root.path());
    producer
        .append_batch(&[input(
            path,
            lane.clone(),
            Some(zero_image_record(&scanned(r"C:\media\zero-image.bin", 42))),
        )])
        .unwrap();
    let output = producer.seal().unwrap();
    assert_eq!(output.manifest.cache_hits, 1);
    assert!(!output.dispatcher.lane_path(&lane).unwrap().exists());
}

#[tokio::test]
async fn duplicate_path_is_emitted_once_and_keeps_stable_manifest() {
    let root = tempfile::tempdir().unwrap();
    let lane = lane(&[7], LocalDiskKind::Hdd, 1);
    let path = scanned(r"C:\media\duplicate.bin", 42);
    let mut producer = producer(root.path());
    producer
        .append_batch(&[
            input(path.clone(), lane.clone(), None),
            input(path.clone(), lane.clone(), None),
        ])
        .unwrap();
    let output = producer.seal().unwrap();
    assert_eq!(
        output.manifest.seen_paths,
        vec![path.normalized_path.clone()]
    );
    let records = collect_records(output.dispatcher).await;
    assert_eq!(records.len(), 1);
    assert_eq!(records[0].scanned.normalized_path, path.normalized_path);
}

#[tokio::test]
async fn unimported_cache_is_rejected_without_publishing_any_row() {
    let root = tempfile::tempdir().unwrap();
    let lane = lane(&[7], LocalDiskKind::Hdd, 1);
    let path = scanned(r"C:\media\remote-only.bin", 42);
    let mut record = imported_record(&path, [5; 16], true);
    record.content_id = None;
    let mut producer = producer(root.path());
    assert!(
        producer
            .append_batch(&[input(path, lane.clone(), Some(record))])
            .is_err()
    );
    let output = producer.seal().unwrap();
    assert!(output.manifest.seen_paths.is_empty());
    assert_eq!(output.manifest.cache_hits, 0);
    assert!(!output.dispatcher.lane_path(&lane).unwrap().exists());
}

#[tokio::test]
async fn same_path_with_different_content_keys_is_rejected_before_publish() {
    let root = tempfile::tempdir().unwrap();
    let lane = lane(&[7], LocalDiskKind::Hdd, 1);
    let path = scanned(r"C:\media\conflict.bin", 42);
    let first = imported_record(&path, [6; 16], true);
    let second = imported_record(&path, [7; 16], true);
    let mut producer = producer(root.path());
    assert!(
        producer
            .append_batch(&[
                input(path.clone(), lane.clone(), Some(first)),
                input(path, lane.clone(), Some(second)),
            ])
            .is_err()
    );
    let output = producer.seal().unwrap();
    assert!(!output.dispatcher.lane_path(&lane).unwrap().exists());
    assert!(output.manifest.seen_paths.is_empty());
}

#[tokio::test]
async fn same_path_size_or_lane_conflict_is_rejected_before_publish() {
    let root = tempfile::tempdir().unwrap();
    let first_lane = lane(&[7], LocalDiskKind::Hdd, 1);
    let second_lane = lane(&[8], LocalDiskKind::Ssd, 1);
    let path = r"C:\media\identity.bin";

    let mut size_producer = producer(root.path());
    assert!(
        size_producer
            .append_batch(&[
                input(scanned(path, 42), first_lane.clone(), None),
                input(scanned(path, 43), first_lane.clone(), None),
            ])
            .is_err()
    );
    let size_output = size_producer.seal().unwrap();
    assert!(size_output.manifest.seen_paths.is_empty());

    let second_root = tempfile::tempdir().unwrap();
    let mut lane_producer = producer(second_root.path());
    assert!(
        lane_producer
            .append_batch(&[
                input(scanned(path, 42), first_lane.clone(), None),
                input(scanned(path, 42), second_lane.clone(), None),
            ])
            .is_err()
    );
    let lane_output = lane_producer.seal().unwrap();
    assert!(lane_output.manifest.seen_paths.is_empty());
}

#[tokio::test]
async fn batches_over_one_thousand_are_rejected_without_partial_publish() {
    let root = tempfile::tempdir().unwrap();
    let lane = lane(&[7], LocalDiskKind::Hdd, 1);
    let rows = (0..=1_000)
        .map(|index| {
            input(
                scanned(&format!(r"C:\bulk\item-{index}.bin"), 42),
                lane.clone(),
                None,
            )
        })
        .collect::<Vec<_>>();
    let mut producer = producer(root.path());
    assert!(producer.append_batch(&rows).is_err());
    let output = producer.seal().unwrap();
    assert!(output.manifest.seen_paths.is_empty());
    assert!(!output.dispatcher.lane_path(&lane).unwrap().exists());
}
