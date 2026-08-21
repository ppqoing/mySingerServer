use std::fs;

use dedup_core::{
    ContentKey, DeleteMode, DisplayPath, LocationKey, MachineId, MediaKind, NormalizedPath,
};
use dedup_node_engine::{
    delete::DeleteEngine,
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
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Delete, machine.clone(), "删除")
        .await;
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
            location: LocationKey::new(machine, target_scan.normalized_path.clone()),
            expected: target_key,
        }],
    };

    let results =
        DeleteEngine::execute_external_with_runtime(&mut store, &plan, &reporter).unwrap();

    assert_eq!(results.len(), 1);
    assert_eq!(results[0].outcome, DeleteOutcome::Deleted);
    assert!(!target.exists());
    assert!(
        untouched.exists(),
        "telemetry must not requery or expand the frozen plan"
    );
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
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Delete, machine.clone(), "删除")
        .await;
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
            location: LocationKey::new(machine, target_scan.normalized_path.clone()),
            expected: target_key,
        }],
    };
    fs::remove_file(&target).unwrap();

    let results =
        DeleteEngine::execute_external_with_runtime(&mut store, &plan, &reporter).unwrap();

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

fn scanned(path: &std::path::Path) -> ScannedPath {
    ScannedPath::new(
        NormalizedPath::new(path).unwrap(),
        DisplayPath::new(path).unwrap(),
        fs::metadata(path).unwrap().len(),
    )
}
