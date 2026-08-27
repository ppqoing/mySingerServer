use std::{
    fs, io,
    path::{Path, PathBuf},
    sync::{
        Arc,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
};

use dedup_core::{
    ContentKey, DeleteMode, DisplayPath, LocationKey, MachineId, MediaKind, NormalizedPath,
};
use dedup_node_engine::{
    delete::{
        DeleteEngine, DeleteFilesystem, DeleteResultCommitter, NodeStoreDeleteResultCommitter,
    },
    runtime_tasks::{RuntimeTaskKind, RuntimeTaskRegistry},
    scan::md5_bytes,
};
use dedup_node_store::{
    AnalysisMode, ConfirmedDeleteItem, DeleteBatchPlan, DeleteOutcome, DeleteResult, GroupKind,
    GroupMemberWrite, GroupWrite, NodeStore, PlannedDeleteItem, ReviewDecision, ScannedPath,
    StoreError,
};
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

    let results = DeleteEngine::execute_external_with_runtime(&mut store, &plan, &reporter)
        .await
        .unwrap();

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

    let results = DeleteEngine::execute_external_with_runtime(&mut store, &plan, &reporter)
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

struct FailingCommitter {
    calls: Arc<AtomicUsize>,
    registry: RuntimeTaskRegistry,
    reporter_id: String,
}

impl DeleteResultCommitter for FailingCommitter {
    async fn apply(
        &self,
        _store: &mut NodeStore,
        _plan: &DeleteBatchPlan,
        _results: &[DeleteResult],
        _external: bool,
    ) -> Result<(), StoreError> {
        assert_precommit_clean(&self.registry, &self.reporter_id).await;
        self.calls.fetch_add(1, Ordering::SeqCst);
        Err(StoreError::InvalidState(
            "controlled summarize failure".into(),
        ))
    }
}

struct ObservingCommitter {
    registry: RuntimeTaskRegistry,
    reporter_id: String,
    observed: Arc<AtomicBool>,
}

impl DeleteResultCommitter for ObservingCommitter {
    async fn apply(
        &self,
        store: &mut NodeStore,
        plan: &DeleteBatchPlan,
        results: &[DeleteResult],
        external: bool,
    ) -> Result<(), StoreError> {
        assert_precommit_clean(&self.registry, &self.reporter_id).await;
        self.observed.store(true, Ordering::SeqCst);
        NodeStoreDeleteResultCommitter
            .apply(store, plan, results, external)
            .await
    }
}

async fn assert_precommit_clean(registry: &RuntimeTaskRegistry, reporter_id: &str) {
    let details = registry.details(reporter_id).await.unwrap();
    let revalidate = details
        .stages
        .iter()
        .find(|stage| stage.stage_id == "revalidate_selection")
        .unwrap();
    assert_eq!(
        revalidate.state,
        RuntimeStageState::RuntimeStageRunning as i32,
        "committer 前重校验 telemetry 必须保持未发布"
    );
    assert_eq!(revalidate.completed, 0);
    assert_eq!(revalidate.failed, 0);
    assert_eq!(revalidate.skipped, 0);
    let delete = details
        .stages
        .iter()
        .find(|stage| stage.stage_id == "delete_items")
        .unwrap();
    assert_eq!(delete.failed, 0, "committer 前不得发布未持久 item failure");
    assert_eq!(delete.completed, 0);
    assert_eq!(delete.skipped, 0);
    assert!(details.failures.is_empty());
}

#[tokio::test]
async fn confirmed_local_batch_reports_partial_delete_after_store_item_terminals() {
    let directory = tempfile::tempdir().unwrap();
    let (mut store, plan, success, failed, outside) = local_confirmed_fixture(directory.path());
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(
            RuntimeTaskKind::Delete,
            store.machine_id().clone(),
            "本地确认删除",
        )
        .await;
    let calls = Arc::new(AtomicUsize::new(0));
    let observed = Arc::new(AtomicBool::new(false));
    let filesystem = ControlledDeleteFilesystem {
        fail_path: Some(failed.clone()),
        calls: Arc::clone(&calls),
    };

    let results = DeleteEngine::execute_batch_with_runtime_using(
        &mut store,
        &plan,
        &reporter,
        &filesystem,
        &ObservingCommitter {
            registry: registry.clone(),
            reporter_id: reporter.id().to_owned(),
            observed: Arc::clone(&observed),
        },
    )
    .await
    .unwrap();

    assert_eq!(calls.load(Ordering::SeqCst), 2);
    assert!(observed.load(Ordering::SeqCst));
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
    let summarize = details
        .stages
        .iter()
        .find(|stage| stage.stage_id == "summarize")
        .unwrap();
    assert_eq!(
        summarize.state,
        RuntimeStageState::RuntimeStageCompleted as i32
    );
    assert_eq!(details.failures.len(), 1);
    assert_eq!(details.failures[0].stage_id, "delete_items");
    assert!(
        details.failures[0]
            .message
            .contains("controlled sharing violation")
    );
}

#[tokio::test]
async fn summarize_store_failure_is_terminal_and_never_repeats_or_expands_deletes() {
    let directory = tempfile::tempdir().unwrap();
    let (mut store, plan, _success, _failed, outside) = local_confirmed_fixture(directory.path());
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(
            RuntimeTaskKind::Delete,
            store.machine_id().clone(),
            "提交失败",
        )
        .await;
    let delete_calls = Arc::new(AtomicUsize::new(0));
    let commit_calls = Arc::new(AtomicUsize::new(0));
    let filesystem = ControlledDeleteFilesystem {
        fail_path: None,
        calls: Arc::clone(&delete_calls),
    };

    let error = DeleteEngine::execute_batch_with_runtime_using(
        &mut store,
        &plan,
        &reporter,
        &filesystem,
        &FailingCommitter {
            calls: Arc::clone(&commit_calls),
            registry: registry.clone(),
            reporter_id: reporter.id().to_owned(),
        },
    )
    .await
    .unwrap_err();

    assert!(error.to_string().contains("controlled summarize failure"));
    assert_eq!(delete_calls.load(Ordering::SeqCst), plan.items.len());
    assert_eq!(commit_calls.load(Ordering::SeqCst), 1);
    assert!(outside.exists());
    let details = registry.details(reporter.id()).await.unwrap();
    assert_eq!(details.summary.as_ref().unwrap().state, "failed");
    let summarize = details
        .stages
        .iter()
        .find(|stage| stage.stage_id == "summarize")
        .unwrap();
    assert_eq!(
        summarize.state,
        RuntimeStageState::RuntimeStageFailed as i32
    );
    for id in ["revalidate_selection", "delete_items"] {
        let stage = details
            .stages
            .iter()
            .find(|stage| stage.stage_id == id)
            .unwrap();
        assert_eq!(stage.state, RuntimeStageState::RuntimeStageCompleted as i32);
        assert_eq!(stage.completed, 0);
        assert_eq!(stage.failed, 0);
        assert_eq!(stage.skipped, plan.items.len() as u64);
    }
    assert!(details.summary.as_ref().unwrap().overall_failed > 0);
    assert_eq!(details.failures.len(), 1);
    assert_eq!(details.failures[0].stage_id, "summarize");
    assert!(
        details.failures[0]
            .message
            .contains("controlled summarize failure")
    );
}

#[tokio::test]
async fn summarize_failure_suppresses_uncommitted_item_failure_telemetry() {
    let directory = tempfile::tempdir().unwrap();
    let (mut store, plan, _success, failed, _outside) = local_confirmed_fixture(directory.path());
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(
            RuntimeTaskKind::Delete,
            store.machine_id().clone(),
            "提交失败覆盖项目失败",
        )
        .await;
    let filesystem = ControlledDeleteFilesystem {
        fail_path: Some(failed),
        calls: Arc::new(AtomicUsize::new(0)),
    };

    DeleteEngine::execute_batch_with_runtime_using(
        &mut store,
        &plan,
        &reporter,
        &filesystem,
        &FailingCommitter {
            calls: Arc::new(AtomicUsize::new(0)),
            registry: registry.clone(),
            reporter_id: reporter.id().to_owned(),
        },
    )
    .await
    .unwrap_err();

    let details = registry.details(reporter.id()).await.unwrap();
    assert_eq!(details.failures.len(), 1);
    assert_eq!(details.failures[0].stage_id, "summarize");
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
    assert_eq!(delete.completed, 0);
    assert_eq!(delete.skipped, plan.items.len() as u64);
}

#[tokio::test]
async fn stale_revalidation_and_store_failure_publish_only_summarize_failure() {
    let directory = tempfile::tempdir().unwrap();
    let (mut store, plan, stale, _other, _outside) = local_confirmed_fixture(directory.path());
    fs::remove_file(&stale).unwrap();
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(
            RuntimeTaskKind::Delete,
            store.machine_id().clone(),
            "重校验和提交同时失败",
        )
        .await;

    DeleteEngine::execute_batch_with_runtime_using(
        &mut store,
        &plan,
        &reporter,
        &ControlledDeleteFilesystem {
            fail_path: None,
            calls: Arc::new(AtomicUsize::new(0)),
        },
        &FailingCommitter {
            calls: Arc::new(AtomicUsize::new(0)),
            registry: registry.clone(),
            reporter_id: reporter.id().to_owned(),
        },
    )
    .await
    .unwrap_err();

    let details = registry.details(reporter.id()).await.unwrap();
    assert_eq!(details.failures.len(), 1);
    assert_eq!(details.failures[0].stage_id, "summarize");
    for id in ["revalidate_selection", "delete_items"] {
        let stage = details
            .stages
            .iter()
            .find(|stage| stage.stage_id == id)
            .unwrap();
        assert_eq!(stage.state, RuntimeStageState::RuntimeStageCompleted as i32);
        assert_eq!(stage.completed, 0);
        assert_eq!(stage.failed, 0);
        assert_eq!(stage.skipped, plan.items.len() as u64);
        assert_eq!(stage.total, plan.items.len() as u64);
    }
}

fn local_confirmed_fixture(
    directory: &Path,
) -> (NodeStore, DeleteBatchPlan, PathBuf, PathBuf, PathBuf) {
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
    let success_location = LocationKey::new(machine.clone(), success_scan.normalized_path.clone());
    let failed_location = LocationKey::new(machine.clone(), failed_scan.normalized_path.clone());
    let outside_location = LocationKey::new(machine, outside_scan.normalized_path.clone());
    let run = store
        .create_analysis_run(AnalysisMode::Local, Default::default(), 1)
        .unwrap();
    let group_id = uuid::Uuid::new_v4().to_string();
    store
        .replace_groups(
            run,
            &[GroupWrite {
                group_id: group_id.clone(),
                kind: GroupKind::Exact,
                representative: key,
                members: vec![
                    GroupMemberWrite::new(outside_location.clone(), key, true),
                    GroupMemberWrite::new(success_location.clone(), key, false),
                    GroupMemberWrite::new(failed_location.clone(), key, false),
                ],
            }],
        )
        .unwrap();
    store
        .save_review_mark(run, &group_id, &outside_location, ReviewDecision::Keep)
        .unwrap();
    for location in [&success_location, &failed_location] {
        store
            .save_review_mark(run, &group_id, location, ReviewDecision::Delete)
            .unwrap();
    }
    let plan = store
        .create_delete_batch(
            run,
            &[
                ConfirmedDeleteItem::new(group_id.clone(), success_location, key),
                ConfirmedDeleteItem::new(group_id, failed_location, key),
            ],
            DeleteMode::Permanent,
            2,
        )
        .unwrap();
    (store, plan, success, failed, outside)
}

fn scanned(path: &std::path::Path) -> ScannedPath {
    ScannedPath::new(
        NormalizedPath::new(path).unwrap(),
        DisplayPath::new(path).unwrap(),
        fs::metadata(path).unwrap().len(),
    )
}
