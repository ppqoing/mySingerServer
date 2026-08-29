use std::{fs, path::Path};

use dedup_core::{
    ContentKey, DeleteMode, DisplayPath, LocationKey, MachineId, MediaKind, NormalizedPath,
};
use dedup_node_engine::{
    delete::{DeleteEngine, SystemDeleteFilesystem},
    preview::{PreviewKind, PreviewService},
    runtime_tasks::{RuntimeTaskKind, RuntimeTaskRegistry},
    scan::md5_bytes,
};
use dedup_node_store::{
    DeleteBatchPlan, DeleteOutcome, DeleteResult, FeatureWrite, NodeStore, PlannedDeleteItem,
    ScannedPath,
};

#[tokio::test]
async fn same_size_md5_change_is_skipped_and_file_remains_active() {
    let directory = tempfile::tempdir().unwrap();
    let (mut store, target, plan) = delete_fixture(directory.path());
    fs::write(target.display_path.as_path(), b"changed!").unwrap();

    let results = execute_transient(&mut store, directory.path(), &plan).await;

    assert_eq!(results[0].outcome, DeleteOutcome::Skipped);
    assert!(target.display_path.as_path().exists());
    assert!(store.location_is_active(&target.location).unwrap());
}

#[tokio::test]
async fn permanent_delete_rechecks_identity_and_updates_current_file_fact() {
    let directory = tempfile::tempdir().unwrap();
    let (mut store, target, plan) = delete_fixture(directory.path());

    let results = execute_transient(&mut store, directory.path(), &plan).await;

    assert_eq!(results[0].outcome, DeleteOutcome::Deleted);
    assert!(!target.display_path.as_path().exists());
    assert!(!store.location_is_active(&target.location).unwrap());
}

#[tokio::test]
async fn central_plan_deletes_without_a_local_analysis_and_publishes_current_file_fact() {
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

    let results = execute_transient(&mut store, directory.path(), &plan).await;

    assert_eq!(results[0].outcome, DeleteOutcome::Deleted);
    assert!(!target_path.exists());
    assert!(!store.location_is_active(&location).unwrap());
    let changes = store.pull_changes(0, 100).unwrap();
    assert!(
        changes
            .changes
            .iter()
            .any(|change| change.entity_kind == "file")
    );
    assert!(
        !changes
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

fn delete_fixture(directory: &Path) -> (NodeStore, Target, DeleteBatchPlan) {
    let target_path = directory.join("target.bin");
    fs::write(&target_path, b"original").unwrap();
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let target_scan = scanned(&target_path);
    let target_key = ContentKey::new(md5_bytes(b"original"), target_scan.file_size);
    store
        .upsert_content_and_location(&target_scan, target_key.md5(), MediaKind::Other)
        .unwrap();
    let target_location = LocationKey::new(machine(), target_scan.normalized_path.clone());
    let plan = DeleteBatchPlan {
        batch_id: "delete-test".into(),
        mode: DeleteMode::Permanent,
        items: vec![PlannedDeleteItem {
            item_id: "target-item".into(),
            group_id: "group".into(),
            location: target_location.clone(),
            expected: target_key,
        }],
    };
    (
        store,
        Target {
            location: target_location,
            display_path: target_scan.display_path,
        },
        plan,
    )
}

/// 通过当前进程瞬态 TSV 队列执行一项删除，供 NodeEngine 行为测试复用。
async fn execute_transient(
    store: &mut NodeStore,
    root: &Path,
    plan: &DeleteBatchPlan,
) -> Vec<DeleteResult> {
    let runtime_root = root.join("runtime");
    fs::create_dir_all(&runtime_root).unwrap();
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(
            RuntimeTaskKind::Delete,
            store.machine_id().clone(),
            "删除测试",
        )
        .await;
    DeleteEngine::execute_transient_with_runtime_using(
        store,
        &runtime_root,
        plan,
        &reporter,
        &SystemDeleteFilesystem,
    )
    .await
    .unwrap()
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
