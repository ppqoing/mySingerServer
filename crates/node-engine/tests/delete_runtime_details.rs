use std::{
    fs, io,
    path::{Path, PathBuf},
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
};

use dedup_core::{
    ContentKey, DeleteMode, DisplayPath, LocationKey, MachineId, MediaKind, NormalizedPath,
};
use dedup_node_engine::{
    delete::{DeleteEngine, DeleteFilesystem, SystemDeleteFilesystem},
    runtime_tasks::{RuntimeTaskKind, RuntimeTaskRegistry},
    scan::md5_bytes,
};
use dedup_node_store::{DeleteBatchPlan, DeleteOutcome, NodeStore, PlannedDeleteItem, ScannedPath};
use dedup_protocol::proto::RuntimeStageState;

#[tokio::test]
async fn delete_reports_frozen_plan_without_expanding_selection() {
    let directory = tempfile::tempdir().unwrap();
    let target = directory.path().join("target.bin");
    let untouched = directory.path().join("untouched.bin");
    fs::write(&target, b"target").unwrap();
    fs::write(&untouched, b"untouched").unwrap();
    let machine = MachineId::from_sha256([0x91; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let target_scan = scanned(&target);
    let untouched_scan = scanned(&untouched);
    let target_key = ContentKey::new(md5_bytes(b"target"), target_scan.file_size);
    store
        .upsert_content_and_location(&target_scan, target_key.md5(), MediaKind::Other)
        .unwrap();
    store
        .upsert_content_and_location(&untouched_scan, md5_bytes(b"untouched"), MediaKind::Other)
        .unwrap();
    let plan = DeleteBatchPlan {
        batch_id: "frozen-delete".into(),
        mode: DeleteMode::Permanent,
        items: vec![PlannedDeleteItem {
            item_id: "target-item".into(),
            group_id: "group".into(),
            location: LocationKey::new(machine.clone(), target_scan.normalized_path.clone()),
            expected: target_key,
        }],
    };
    let (registry, reporter) = begin_reporter(&store, "冻结删除").await;

    let results = DeleteEngine::execute_transient_with_runtime_using(
        &mut store,
        &runtime_root(directory.path()),
        &plan,
        &reporter,
        &SystemDeleteFilesystem,
    )
    .await
    .unwrap();

    assert_eq!(results.len(), 1);
    assert_eq!(results[0].outcome, DeleteOutcome::Deleted);
    assert!(!target.exists());
    assert!(untouched.exists(), "删除执行不得重新查询或扩展冻结集合");
    let details = registry.details(reporter.id()).await.unwrap();
    let summary = details.summary.as_ref().unwrap();
    assert!(summary.overall_total_known);
    assert_eq!(summary.overall_total, 1);
    assert_eq!(summary.overall_completed, 1);
    for id in [
        "revalidate_selection",
        "dispatch_nodes",
        "delete_items",
        "summarize",
    ] {
        let stage = details
            .stages
            .iter()
            .find(|stage| stage.stage_id == id)
            .unwrap();
        assert_eq!(stage.unit, "delete_items");
        assert_eq!(stage.total, 1);
        assert_eq!(stage.state, RuntimeStageState::RuntimeStageCompleted as i32);
    }
}

#[tokio::test]
async fn stale_frozen_item_is_skipped_in_revalidate_without_delete_failure() {
    let directory = tempfile::tempdir().unwrap();
    let target = directory.path().join("stale.bin");
    fs::write(&target, b"stale").unwrap();
    let machine = MachineId::from_sha256([0x92; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let target_scan = scanned(&target);
    let target_key = ContentKey::new(md5_bytes(b"stale"), target_scan.file_size);
    store
        .upsert_content_and_location(&target_scan, target_key.md5(), MediaKind::Other)
        .unwrap();
    let plan = DeleteBatchPlan {
        batch_id: "stale-delete".into(),
        mode: DeleteMode::Permanent,
        items: vec![PlannedDeleteItem {
            item_id: "stale-item".into(),
            group_id: "group".into(),
            location: LocationKey::new(machine.clone(), target_scan.normalized_path.clone()),
            expected: target_key,
        }],
    };
    fs::remove_file(&target).unwrap();
    let (registry, reporter) = begin_reporter(&store, "过期删除").await;

    let results = DeleteEngine::execute_transient_with_runtime_using(
        &mut store,
        &runtime_root(directory.path()),
        &plan,
        &reporter,
        &SystemDeleteFilesystem,
    )
    .await
    .unwrap();

    assert_eq!(results[0].outcome, DeleteOutcome::Skipped);
    let details = registry.details(reporter.id()).await.unwrap();
    let revalidate = details
        .stages
        .iter()
        .find(|stage| stage.stage_id == "revalidate_selection")
        .unwrap();
    assert_eq!(
        revalidate.state,
        RuntimeStageState::RuntimeStageCompleted as i32
    );
    assert_eq!(revalidate.skipped, 1);
    let delete = details
        .stages
        .iter()
        .find(|stage| stage.stage_id == "delete_items")
        .unwrap();
    assert_eq!(
        delete.state,
        RuntimeStageState::RuntimeStageCompleted as i32
    );
    assert_eq!(delete.failed, 0);
    assert_eq!(delete.skipped, 1);
}

#[tokio::test]
async fn transient_delete_reports_partial_failure_and_continues() {
    let directory = tempfile::tempdir().unwrap();
    let (mut store, plan, success, failed, outside) = local_fixture(directory.path());
    let (registry, reporter) = begin_reporter(&store, "部分删除").await;
    let filesystem = ControlledDeleteFilesystem {
        fail_path: Some(failed.clone()),
        calls: Arc::new(AtomicUsize::new(0)),
    };

    let results = DeleteEngine::execute_transient_with_runtime_using(
        &mut store,
        &runtime_root(directory.path()),
        &plan,
        &reporter,
        &filesystem,
    )
    .await
    .unwrap();

    assert_eq!(results.len(), 2);
    assert_eq!(
        results
            .iter()
            .filter(|row| row.outcome == DeleteOutcome::Deleted)
            .count(),
        1
    );
    assert_eq!(
        results
            .iter()
            .filter(|row| row.outcome == DeleteOutcome::Failed)
            .count(),
        1
    );
    assert!(!success.exists());
    assert!(failed.exists());
    assert!(outside.exists(), "冻结集合外的活动文件不得扩项删除");
    let details = registry.details(reporter.id()).await.unwrap();
    assert_eq!(details.summary.as_ref().unwrap().state, "failed");
    let delete = details
        .stages
        .iter()
        .find(|stage| stage.stage_id == "delete_items")
        .unwrap();
    assert_eq!(delete.state, RuntimeStageState::RuntimeStageFailed as i32);
    assert_eq!(delete.completed, 1);
    assert_eq!(delete.failed, 1);
    assert_eq!(details.failures.len(), 1);
    assert!(
        details.failures[0]
            .message
            .contains("controlled sharing violation")
    );
}

#[derive(Clone)]
struct ControlledDeleteFilesystem {
    fail_path: Option<PathBuf>,
    calls: Arc<AtomicUsize>,
}

impl DeleteFilesystem for ControlledDeleteFilesystem {
    fn delete(&self, mode: DeleteMode, path: &Path) -> io::Result<DeleteOutcome> {
        assert_eq!(mode, DeleteMode::Permanent);
        self.calls.fetch_add(1, Ordering::SeqCst);
        if self.fail_path.as_deref() == Some(path) {
            return Err(io::Error::new(
                io::ErrorKind::PermissionDenied,
                "controlled sharing violation",
            ));
        }
        fs::remove_file(path)?;
        Ok(DeleteOutcome::Deleted)
    }
}

async fn begin_reporter(
    store: &NodeStore,
    label: &str,
) -> (
    RuntimeTaskRegistry,
    dedup_node_engine::runtime_tasks::RuntimeTaskReporter,
) {
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Delete, store.machine_id().clone(), label)
        .await;
    (registry, reporter)
}

/// 创建当前测试专用的 transient runtime 根。
fn runtime_root(root: &Path) -> PathBuf {
    let runtime_root = root.join("runtime");
    fs::create_dir_all(&runtime_root).unwrap();
    runtime_root
}

fn local_fixture(directory: &Path) -> (NodeStore, DeleteBatchPlan, PathBuf, PathBuf, PathBuf) {
    let success = directory.join("delete-success.bin");
    let failed = directory.join("delete-failed.bin");
    let outside = directory.join("keep-outside.bin");
    fs::write(&success, b"same").unwrap();
    fs::write(&failed, b"same").unwrap();
    fs::write(&outside, b"same").unwrap();
    let machine = MachineId::from_sha256([0x93; 32]);
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let success_scan = scanned(&success);
    let failed_scan = scanned(&failed);
    let outside_scan = scanned(&outside);
    let key = ContentKey::new(md5_bytes(b"same"), 4);
    for row in [&success_scan, &failed_scan, &outside_scan] {
        store
            .upsert_content_and_location(row, key.md5(), MediaKind::Other)
            .unwrap();
    }
    let plan = DeleteBatchPlan {
        batch_id: "partial-delete".into(),
        mode: DeleteMode::Permanent,
        items: vec![
            PlannedDeleteItem {
                item_id: "success-item".into(),
                group_id: "group".into(),
                location: LocationKey::new(machine.clone(), success_scan.normalized_path.clone()),
                expected: key,
            },
            PlannedDeleteItem {
                item_id: "failed-item".into(),
                group_id: "group".into(),
                location: LocationKey::new(machine, failed_scan.normalized_path.clone()),
                expected: key,
            },
        ],
    };
    (store, plan, success, failed, outside)
}

fn scanned(path: &Path) -> ScannedPath {
    ScannedPath::new(
        NormalizedPath::new(path).unwrap(),
        DisplayPath::new(path).unwrap(),
        fs::metadata(path).unwrap().len(),
    )
}
