use std::{fs, path::Path};

use dedup_core::{
    AnalysisRunId, ContentKey, DeleteMode, DisplayPath, GroupId, LocationKey, MachineId, MediaKind,
    NormalizedPath, Thresholds,
};
use dedup_node_engine::{
    delete::DeleteEngine,
    preview::{PreviewKind, PreviewService},
    scan::md5_bytes,
};
use dedup_node_store::{
    AnalysisMode, DeleteBatchPlan, DeleteOutcome, FeatureWrite, GroupKind, GroupMemberWrite,
    GroupWrite, NodeStore, PlannedDeleteItem, ReviewDecision, ScannedPath,
};

#[test]
fn same_size_md5_change_is_skipped_and_group_remains() {
    let directory = tempfile::tempdir().unwrap();
    let (mut store, run_id, group_id, target, plan) = delete_fixture(directory.path());
    fs::write(target.display_path.as_path(), b"changed!").unwrap();

    let results = DeleteEngine::execute_batch(&mut store, &plan).unwrap();

    assert_eq!(results[0].outcome, DeleteOutcome::Skipped);
    assert!(target.display_path.as_path().exists());
    assert_eq!(store.page_groups(run_id, None, 10).unwrap().items.len(), 1);
    assert_eq!(
        store
            .page_group_members(run_id, &group_id, None, 10)
            .unwrap()
            .items
            .len(),
        2
    );
}

#[test]
fn permanent_delete_rechecks_identity_and_immediately_updates_group() {
    let directory = tempfile::tempdir().unwrap();
    let (mut store, run_id, _group_id, target, plan) = delete_fixture(directory.path());

    let results = DeleteEngine::execute_batch(&mut store, &plan).unwrap();

    assert_eq!(results[0].outcome, DeleteOutcome::Deleted);
    assert!(!target.display_path.as_path().exists());
    assert!(!store.location_is_active(&target.location).unwrap());
    assert!(
        store
            .page_groups(run_id, None, 10)
            .unwrap()
            .items
            .is_empty()
    );
}

#[test]
fn central_plan_deletes_without_a_local_analysis_and_publishes_tombstone() {
    let directory = tempfile::tempdir().unwrap();
    let target_path = directory.path().join("central-target.bin");
    fs::write(&target_path, b"central").unwrap();
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let scanned = scanned(&target_path);
    let expected = ContentKey::new(md5_bytes(b"central"), scanned.file_size);
    store
        .upsert_content_and_location(&scanned, expected.md5(), MediaKind::Other)
        .unwrap();
    let location = LocationKey::new(machine(), scanned.normalized_path.clone());
    let plan = DeleteBatchPlan {
        batch_id: "central-batch".into(),
        mode: DeleteMode::Permanent,
        items: vec![PlannedDeleteItem {
            item_id: "central-item".into(),
            group_id: "central-group".into(),
            location: location.clone(),
            expected,
        }],
    };

    let results = DeleteEngine::execute_external(&mut store, &plan).unwrap();

    assert_eq!(results[0].outcome, DeleteOutcome::Deleted);
    assert!(!target_path.exists());
    assert!(!store.location_is_active(&location).unwrap());
    let changes = store.pull_changes(0, 100).unwrap();
    assert!(
        changes
            .changes
            .iter()
            .any(|change| change.entity_kind == "deletion_tombstone")
    );
}

#[test]
fn image_preview_streams_original_without_creating_thumbnail_cache() {
    let directory = tempfile::tempdir().unwrap();
    let image_path = directory.path().join("photo.jpg");
    fs::write(&image_path, b"0123456789").unwrap();
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let scanned = scanned(&image_path);
    store
        .upsert_content_and_location(&scanned, md5_bytes(b"0123456789"), MediaKind::Image)
        .unwrap();
    let location = LocationKey::new(machine(), scanned.normalized_path.clone());
    let cache = directory.path().join("cache");
    let preview = PreviewService::new(&cache);

    let first = preview
        .read(&store, &location, PreviewKind::Original, 2, 4)
        .unwrap();

    assert_eq!(first.data, b"2345");
    assert!(!first.eof);
    assert!(!cache.exists(), "图片预览不得创建缩略图或缓存目录");
}

#[test]
fn video_preview_reads_only_the_cached_contact_sheet() {
    let directory = tempfile::tempdir().unwrap();
    let video_path = directory.path().join("clip.mp4");
    fs::write(&video_path, b"video-bytes").unwrap();
    let cache = directory.path().join("cache");
    fs::create_dir_all(cache.join("contact-sheets")).unwrap();
    fs::write(
        cache.join("contact-sheets").join("sheet.jpg"),
        b"jpeg-sheet",
    )
    .unwrap();
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let scanned = scanned(&video_path);
    let content = store
        .upsert_content_and_location(&scanned, md5_bytes(b"video-bytes"), MediaKind::Video)
        .unwrap();
    store
        .commit_feature_result(
            content.id,
            None,
            FeatureWrite::ContactSheet("contact-sheets/sheet.jpg".into()),
        )
        .unwrap();
    let location = LocationKey::new(machine(), scanned.normalized_path.clone());

    let chunk = PreviewService::new(&cache)
        .read(&store, &location, PreviewKind::ContactSheet, 0, 1_048_576)
        .unwrap();

    assert_eq!(chunk.data, b"jpeg-sheet");
    assert!(chunk.eof);
}

struct Target {
    location: LocationKey,
    display_path: DisplayPath,
}

fn delete_fixture(
    directory: &Path,
) -> (
    NodeStore,
    AnalysisRunId,
    String,
    Target,
    dedup_node_store::DeleteBatchPlan,
) {
    let target_path = directory.join("target.bin");
    let keep_path = directory.join("keep.bin");
    fs::write(&target_path, b"original").unwrap();
    fs::write(&keep_path, b"keep").unwrap();
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let target_scan = scanned(&target_path);
    let keep_scan = scanned(&keep_path);
    let target_key = ContentKey::new(md5_bytes(b"original"), target_scan.file_size);
    let keep_key = ContentKey::new(md5_bytes(b"keep"), keep_scan.file_size);
    store
        .upsert_content_and_location(&target_scan, target_key.md5(), MediaKind::Other)
        .unwrap();
    store
        .upsert_content_and_location(&keep_scan, keep_key.md5(), MediaKind::Other)
        .unwrap();
    let target_location = LocationKey::new(machine(), target_scan.normalized_path.clone());
    let keep_location = LocationKey::new(machine(), keep_scan.normalized_path.clone());
    let run_id = store
        .create_analysis_run(AnalysisMode::Local, Thresholds::default(), 1)
        .unwrap();
    let group_id = GroupId::new().as_uuid().to_string();
    store
        .replace_groups(
            run_id,
            &[GroupWrite {
                group_id: group_id.clone(),
                kind: GroupKind::Image,
                representative: keep_key,
                members: vec![
                    GroupMemberWrite::new(keep_location.clone(), keep_key, true),
                    GroupMemberWrite::new(target_location.clone(), target_key, false),
                ],
            }],
        )
        .unwrap();
    store
        .save_review_mark(run_id, &group_id, &keep_location, ReviewDecision::Keep)
        .unwrap();
    store
        .save_review_mark(run_id, &group_id, &target_location, ReviewDecision::Delete)
        .unwrap();
    let plan = store
        .create_delete_batch(
            run_id,
            std::slice::from_ref(&group_id),
            DeleteMode::Permanent,
            2,
        )
        .unwrap();
    (
        store,
        run_id,
        group_id,
        Target {
            location: target_location,
            display_path: target_scan.display_path,
        },
        plan,
    )
}

fn scanned(path: &Path) -> ScannedPath {
    ScannedPath::new(
        NormalizedPath::new(path).unwrap(),
        DisplayPath::new(path).unwrap(),
        fs::metadata(path).unwrap().len(),
    )
}

fn machine() -> MachineId {
    MachineId::parse(&"77".repeat(32)).unwrap()
}
