use std::{
    fs, io,
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
    time::Duration,
};

use dedup_core::{
    ContentKey, DeleteMode, DisplayPath, LocationKey, MachineId, MediaKind, NormalizedPath,
};
use dedup_node_engine::{
    actor::NodeEngine,
    analysis::{
        AnalysisResultGroupKind, AnalysisResultHeader, AnalysisResultMode, AnalysisResultRow,
        AnalysisResultWriter,
    },
    delete::{DeleteEngine, DeleteFilesystem},
    runtime_tasks::{RuntimeTaskKind, RuntimeTaskRegistry},
    scan::md5_bytes,
    server::NodeRequestHandler,
    worker::WorkerPool,
};
use dedup_node_store::{DeleteBatchPlan, DeleteOutcome, NodeStore, PlannedDeleteItem, ScannedPath};
use dedup_protocol::{MAX_LOCAL_RESULT_WINDOW_ROWS, proto};

#[derive(Clone, Default)]
struct ControlledFilesystem {
    fail_path: Arc<Mutex<Option<PathBuf>>>,
}

impl DeleteFilesystem for ControlledFilesystem {
    fn delete(&self, mode: DeleteMode, path: &Path) -> io::Result<DeleteOutcome> {
        assert_eq!(mode, DeleteMode::Permanent);
        if self
            .fail_path
            .lock()
            .unwrap()
            .as_deref()
            .is_some_and(|expected| {
                expected
                    .file_name()
                    .and_then(|name| name.to_str())
                    .zip(path.file_name().and_then(|name| name.to_str()))
                    .is_some_and(|(left, right)| left.eq_ignore_ascii_case(right))
            })
        {
            return Err(io::Error::new(
                io::ErrorKind::PermissionDenied,
                "测试删除失败",
            ));
        }
        fs::remove_file(path)?;
        Ok(DeleteOutcome::Deleted)
    }
}

#[tokio::test]
async fn transient_delete_queue_deletes_sequentially_and_commits_only_current_file_facts() {
    let directory = tempfile::tempdir().unwrap();
    let runtime_root = directory.path().join("runtime");
    fs::create_dir_all(&runtime_root).unwrap();
    let database = directory.path().join("node.db");
    let machine = MachineId::from_sha256([0xA1; 32]);
    let first = directory.path().join("first.bin");
    let failed = directory.path().join("failed.bin");
    let last = directory.path().join("last.bin");
    fs::write(&first, b"first").unwrap();
    fs::write(&failed, b"failed").unwrap();
    fs::write(&last, b"last").unwrap();

    let mut store = NodeStore::open(&database, machine.clone()).unwrap();
    let plan_items = [
        plan_item("item-1", &first, b"first", &machine),
        plan_item("item-2", &failed, b"failed", &machine),
        plan_item("item-3", &last, b"last", &machine),
    ];
    for item in &plan_items {
        let scan = scanned(
            item.location.normalized_path().as_str(),
            item.expected.file_size(),
        );
        store
            .upsert_content_and_location(&scan, item.expected.md5(), MediaKind::Other)
            .unwrap();
    }
    let plan = DeleteBatchPlan {
        batch_id: "transient-delete-test".into(),
        mode: DeleteMode::Permanent,
        items: plan_items.to_vec(),
    };
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::Delete, machine, "瞬态删除")
        .await;
    let filesystem = ControlledFilesystem {
        fail_path: Arc::new(Mutex::new(Some(failed.clone()))),
    };

    let results = DeleteEngine::execute_transient_with_runtime_using(
        &mut store,
        &runtime_root,
        &plan,
        &reporter,
        &filesystem,
    )
    .await
    .unwrap();

    assert_eq!(
        results.iter().map(|item| item.outcome).collect::<Vec<_>>(),
        vec![
            DeleteOutcome::Deleted,
            DeleteOutcome::Failed,
            DeleteOutcome::Deleted
        ]
    );
    assert!(!first.exists());
    assert!(failed.exists());
    assert!(!last.exists());
    assert!(!runtime_root.join(&plan.batch_id).exists());
    assert_eq!(store.library_revision().unwrap(), 2);
    let changes = store.pull_changes(0, 100).unwrap();
    assert_eq!(
        changes
            .changes
            .iter()
            .filter(|change| change.entity_kind == "deletion_tombstone")
            .count(),
        0
    );
    drop(store);

    let connection = rusqlite::Connection::open(database).unwrap();
    for table in [
        "review_marks",
        "delete_batches",
        "delete_items",
        "deletion_tombstones",
        "group_members",
    ] {
        let count: i64 = connection
            .query_row(&format!("SELECT COUNT(*) FROM {table}"), [], |row| {
                row.get(0)
            })
            .unwrap();
        assert_eq!(count, 0, "瞬态删除不得写入旧表 {table}");
    }
}

#[tokio::test]
async fn local_delete_uses_transient_review_and_queue_without_legacy_rows() {
    let directory = tempfile::tempdir().unwrap();
    let data_root = directory.path().join("data/node");
    let runtime_root = data_root.join("runtime");
    let results_root = data_root.join("results");
    fs::create_dir_all(&runtime_root).unwrap();
    fs::create_dir_all(&results_root).unwrap();
    let database = directory.path().join("node.db");
    let machine = MachineId::from_sha256([0xA2; 32]);
    let keep = directory.path().join("keep.bin");
    let target = directory.path().join("target.bin");
    fs::write(&keep, b"same").unwrap();
    fs::write(&target, b"same").unwrap();
    let keep_scan = scanned(&keep.to_string_lossy(), 4);
    let target_scan = scanned(&target.to_string_lossy(), 4);
    let expected = ContentKey::new(md5_bytes(b"same"), 4);
    let keep_location = LocationKey::new(machine.clone(), keep_scan.normalized_path.clone());
    let target_location = LocationKey::new(machine.clone(), target_scan.normalized_path.clone());

    let mut store = NodeStore::open(&database, machine.clone()).unwrap();
    store
        .upsert_content_and_location(&keep_scan, expected.md5(), MediaKind::Other)
        .unwrap();
    store
        .upsert_content_and_location(&target_scan, expected.md5(), MediaKind::Other)
        .unwrap();
    let run_id = dedup_core::AnalysisRunId::new();
    let mut writer = AnalysisResultWriter::begin(
        &results_root,
        &AnalysisResultHeader {
            format_version: 1,
            analysis_id: run_id,
            library_revision: 0,
            analysis_mode: AnalysisResultMode::Local,
            created_at_ms: 1,
            thresholds: Default::default(),
        },
    )
    .unwrap();
    for (location, path, representative) in [
        (&keep_location, &keep, true),
        (&target_location, &target, false),
    ] {
        writer
            .write_member(&AnalysisResultRow {
                group_kind: AnalysisResultGroupKind::Exact,
                group_id: "latest-group".into(),
                representative,
                representative_content: expected,
                location: location.clone(),
                display_path: path.to_string_lossy().into_owned(),
                content: expected,
                stage1_score: 1.0,
                phash_passed_parts: None,
                stage2_score: None,
            })
            .unwrap();
    }
    writer.publish().unwrap();
    drop(store);

    let store = NodeStore::open(&database, machine.clone()).unwrap();
    let (pool, _started, _controller) = WorkerPool::controlled_batch_for_test(1);
    let (handle, actor) = NodeEngine::spawn_with_runtime_root_for_test(
        store,
        pool,
        "127.0.0.1:39131".parse().unwrap(),
        &directory.path().join("cache"),
        &runtime_root,
        dedup_core::EnumeratorKind::WindowsWalker,
    );

    for (request_id, location, decision) in [
        (1, keep_location.clone(), proto::ReviewDecision::ReviewKeep),
        (
            2,
            target_location.clone(),
            proto::ReviewDecision::ReviewDelete,
        ),
    ] {
        let response = handle
            .handle(proto::Envelope {
                request_id,
                payload: Some(proto::envelope::Payload::SaveReviewMark(
                    proto::SaveReviewMark {
                        group_id: "latest-group".into(),
                        location: Some((&location).into()),
                        decision: decision as i32,
                        analysis_run_id: run_id.as_uuid().to_string(),
                    },
                )),
            })
            .await;
        assert!(
            matches!(
                response.payload,
                Some(proto::envelope::Payload::SaveReviewMark(_))
            ),
            "保存瞬态复核必须成功: {:?}",
            response.payload
        );
    }
    let review_window = handle
        .handle(proto::Envelope {
            request_id: 30,
            payload: Some(proto::envelope::Payload::ReadLocalResultWindow(
                proto::ReadLocalResultWindow {
                    analysis_run_id: run_id.as_uuid().to_string(),
                    kind: proto::LocalResultWindowKind::LocalResultWindowMembers as i32,
                    group_id: "latest-group".into(),
                    visible_count: 10,
                    ..Default::default()
                },
            )),
        })
        .await;
    let Some(proto::envelope::Payload::ReadLocalResultWindow(review_window)) =
        review_window.payload
    else {
        panic!("最近结果成员窗口必须返回复核状态");
    };
    assert_eq!(
        review_window.members[0].review,
        proto::ReviewDecision::ReviewKeep as i32
    );
    assert_eq!(
        review_window.members[1].review,
        proto::ReviewDecision::ReviewDelete as i32
    );
    let response = handle
        .handle(proto::Envelope {
            request_id: 3,
            payload: Some(proto::envelope::Payload::CreateDeleteBatch(
                proto::CreateDeleteBatch {
                    mode: proto::DeleteMode::DeletePermanent as i32,
                    analysis_run_id: run_id.as_uuid().to_string(),
                    items: vec![proto::DeleteItem {
                        group_id: "latest-group".into(),
                        location: Some((&target_location).into()),
                        expected_content: Some((&expected).into()),
                        ..Default::default()
                    }],
                    ..Default::default()
                },
            )),
        })
        .await;
    assert!(matches!(
        response.payload,
        Some(proto::envelope::Payload::CreateDeleteBatch(_))
    ));
    assert!(!target.exists());
    assert!(keep.exists());
    assert!(!runtime_root.join(response_task_id(&response)).exists());
    handle.shutdown().await.unwrap();
    actor.await.unwrap();

    let connection = rusqlite::Connection::open(database).unwrap();
    for table in [
        "review_marks",
        "delete_batches",
        "delete_items",
        "deletion_tombstones",
        "group_members",
    ] {
        let count: i64 = connection
            .query_row(&format!("SELECT COUNT(*) FROM {table}"), [], |row| {
                row.get(0)
            })
            .unwrap();
        assert_eq!(count, 0, "本地瞬态删除不得写入旧表 {table}");
    }
}

#[tokio::test]
async fn external_delete_keeps_protocol_response_without_delete_history() {
    let directory = tempfile::tempdir().unwrap();
    let data_root = directory.path().join("data/node");
    let runtime_root = data_root.join("runtime");
    fs::create_dir_all(&runtime_root).unwrap();
    let database = directory.path().join("node.db");
    let machine = MachineId::from_sha256([0xA3; 32]);
    let target = directory.path().join("external-target.bin");
    fs::write(&target, b"external").unwrap();
    let scan = scanned(&target.to_string_lossy(), 8);
    let expected = ContentKey::new(md5_bytes(b"external"), 8);
    let location = LocationKey::new(machine.clone(), scan.normalized_path.clone());
    let mut store = NodeStore::open(&database, machine.clone()).unwrap();
    store
        .upsert_content_and_location(&scan, expected.md5(), MediaKind::Other)
        .unwrap();
    let (pool, _started, _controller) = WorkerPool::controlled_batch_for_test(1);
    let (handle, actor) = NodeEngine::spawn_with_runtime_root_for_test(
        store,
        pool,
        "127.0.0.1:39132".parse().unwrap(),
        &directory.path().join("cache"),
        &runtime_root,
        dedup_core::EnumeratorKind::WindowsWalker,
    );

    let response = handle
        .handle(proto::Envelope {
            request_id: 50,
            payload: Some(proto::envelope::Payload::CreateDeleteBatch(
                proto::CreateDeleteBatch {
                    delete_batch_id: "external-transient-delete".into(),
                    mode: proto::DeleteMode::DeletePermanent as i32,
                    items: vec![proto::DeleteItem {
                        delete_item_id: "external-item".into(),
                        group_id: "central-group".into(),
                        location: Some((&location).into()),
                        expected_content: Some((&expected).into()),
                        ..Default::default()
                    }],
                    ..Default::default()
                },
            )),
        })
        .await;
    let Some(proto::envelope::Payload::CreateDeleteBatch(batch)) = response.payload else {
        panic!("中心删除必须保持 CreateDeleteBatch 协议响应");
    };
    assert_eq!(batch.delete_batch_id, "external-transient-delete");
    assert_eq!(batch.items[0].outcome, "deleted");
    assert!(!target.exists());
    assert!(!runtime_root.join("external-transient-delete").exists());
    handle.shutdown().await.unwrap();
    actor.await.unwrap();

    let connection = rusqlite::Connection::open(database).unwrap();
    for table in [
        "review_marks",
        "delete_batches",
        "delete_items",
        "deletion_tombstones",
        "group_members",
    ] {
        let count: i64 = connection
            .query_row(&format!("SELECT COUNT(*) FROM {table}"), [], |row| {
                row.get(0)
            })
            .unwrap();
        assert_eq!(count, 0, "中心瞬态删除不得写入旧表 {table}");
    }
}

#[tokio::test]
async fn delete_is_rejected_while_scan_is_running() {
    let directory = tempfile::tempdir().unwrap();
    let scan_root = directory.path().join("scan");
    fs::create_dir_all(&scan_root).unwrap();
    fs::write(scan_root.join("held.bin"), b"scan worker input").unwrap();

    let target = directory.path().join("delete-during-scan.bin");
    fs::write(&target, b"delete target").unwrap();
    let database = directory.path().join("node.db");
    let machine = MachineId::from_sha256([0xA5; 32]);
    let target_scan = scanned(&target.to_string_lossy(), b"delete target".len() as u64);
    let expected = ContentKey::new(md5_bytes(b"delete target"), b"delete target".len() as u64);
    let target_location = LocationKey::new(machine.clone(), target_scan.normalized_path.clone());
    let mut store = NodeStore::open(&database, machine.clone()).unwrap();
    store
        .upsert_content_and_location(&target_scan, expected.md5(), MediaKind::Other)
        .unwrap();

    let runtime_root = directory.path().join("data/node/runtime");
    let cache_root = directory.path().join("data/node/cache");
    let (pool, mut started) = WorkerPool::controlled_for_test();
    let (handle, actor) = NodeEngine::spawn_with_runtime_root_for_test(
        store,
        pool,
        "127.0.0.1:39135".parse().unwrap(),
        &cache_root,
        &runtime_root,
        dedup_core::EnumeratorKind::WindowsWalker,
    );

    let scan = handle
        .handle(proto::Envelope {
            request_id: 1,
            payload: Some(proto::envelope::Payload::CreateScan(proto::CreateScan {
                roots: vec![scan_root.to_string_lossy().into_owned()],
                force_recalculate: false,
                enumerator: "windows_walker".into(),
            })),
        })
        .await;
    let Some(proto::envelope::Payload::TaskAccepted(scan)) = scan.payload else {
        panic!("扫描必须返回任务身份");
    };
    tokio::time::timeout(Duration::from_secs(2), started.recv())
        .await
        .expect("扫描必须先占用 Worker，才能验证删除互斥")
        .expect("可控 Worker 不应提前关闭");

    let delete = handle
        .handle(proto::Envelope {
            request_id: 2,
            payload: Some(proto::envelope::Payload::CreateDeleteBatch(
                proto::CreateDeleteBatch {
                    delete_batch_id: "delete-during-scan".into(),
                    mode: proto::DeleteMode::DeletePermanent as i32,
                    items: vec![proto::DeleteItem {
                        delete_item_id: "delete-item".into(),
                        group_id: "central-group".into(),
                        location: Some((&target_location).into()),
                        expected_content: Some((&expected).into()),
                        ..Default::default()
                    }],
                    ..Default::default()
                },
            )),
        })
        .await;

    let rejected = matches!(
        delete.payload.as_ref(),
        Some(proto::envelope::Payload::Error(proto::Error { code, .. }))
            if *code == proto::ErrorCode::InvalidRequest as i32
    );
    assert!(
        rejected,
        "后台扫描期间删除必须返回 Busy/InvalidRequest，旧实现响应为: {:?}",
        delete.payload
    );
    assert!(target.exists(), "扫描运行期间删除不得改变文件事实");

    handle
        .handle(proto::Envelope {
            request_id: 3,
            payload: Some(proto::envelope::Payload::CancelTask(proto::CancelTask {
                task_id: scan.task_id,
            })),
        })
        .await;
    tokio::time::timeout(Duration::from_secs(2), handle.shutdown())
        .await
        .expect("取消扫描后 actor 必须收束")
        .unwrap();
    actor.await.unwrap();
}

#[tokio::test]
async fn local_review_and_delete_accept_members_beyond_result_window_limit() {
    let directory = tempfile::tempdir().unwrap();
    let data_root = directory.path().join("data/node");
    let runtime_root = data_root.join("runtime");
    let results_root = data_root.join("results");
    fs::create_dir_all(&runtime_root).unwrap();
    fs::create_dir_all(&results_root).unwrap();
    let database = directory.path().join("node.db");
    let machine = MachineId::from_sha256([0xA4; 32]);
    let target = directory.path().join("target-large-group.bin");
    let keep = directory.path().join("keep-large-group.bin");
    fs::write(&target, b"same").unwrap();
    fs::write(&keep, b"same").unwrap();
    let target_scan = scanned(&target.to_string_lossy(), 4);
    let keep_scan = scanned(&keep.to_string_lossy(), 4);
    let expected = ContentKey::new(md5_bytes(b"same"), 4);
    let target_location = LocationKey::new(machine.clone(), target_scan.normalized_path.clone());
    let keep_location = LocationKey::new(machine.clone(), keep_scan.normalized_path.clone());

    let mut store = NodeStore::open(&database, machine.clone()).unwrap();
    store
        .upsert_content_and_location(&target_scan, expected.md5(), MediaKind::Other)
        .unwrap();
    store
        .upsert_content_and_location(&keep_scan, expected.md5(), MediaKind::Other)
        .unwrap();
    let run_id = dedup_core::AnalysisRunId::new();
    let mut writer = AnalysisResultWriter::begin(
        &results_root,
        &AnalysisResultHeader {
            format_version: 1,
            analysis_id: run_id,
            library_revision: 0,
            analysis_mode: AnalysisResultMode::Local,
            created_at_ms: 1,
            thresholds: Default::default(),
        },
    )
    .unwrap();
    for index in 0..=MAX_LOCAL_RESULT_WINDOW_ROWS {
        let (location, display_path, representative) = if index == 0 {
            (target_location.clone(), target.display().to_string(), true)
        } else if index == MAX_LOCAL_RESULT_WINDOW_ROWS {
            (keep_location.clone(), keep.display().to_string(), false)
        } else {
            let path = directory.path().join(format!("synthetic-{index:03}.bin"));
            (
                LocationKey::new(machine.clone(), NormalizedPath::new(&path).unwrap()),
                path.display().to_string(),
                false,
            )
        };
        writer
            .write_member(&AnalysisResultRow {
                group_kind: AnalysisResultGroupKind::Exact,
                group_id: "large-group".into(),
                representative,
                representative_content: expected,
                location,
                display_path,
                content: expected,
                stage1_score: 1.0,
                phash_passed_parts: None,
                stage2_score: None,
            })
            .unwrap();
    }
    writer.publish().unwrap();
    drop(store);

    let store = NodeStore::open(&database, machine.clone()).unwrap();
    let (pool, _started, _controller) = WorkerPool::controlled_batch_for_test(1);
    let (handle, actor) = NodeEngine::spawn_with_runtime_root_for_test(
        store,
        pool,
        "127.0.0.1:39133".parse().unwrap(),
        &directory.path().join("cache"),
        &runtime_root,
        dedup_core::EnumeratorKind::WindowsWalker,
    );

    let keep_response = handle
        .handle(proto::Envelope {
            request_id: 1,
            payload: Some(proto::envelope::Payload::SaveReviewMark(
                proto::SaveReviewMark {
                    group_id: "large-group".into(),
                    location: Some((&keep_location).into()),
                    decision: proto::ReviewDecision::ReviewKeep as i32,
                    analysis_run_id: run_id.as_uuid().to_string(),
                },
            )),
        })
        .await;
    assert!(
        matches!(
            keep_response.payload,
            Some(proto::envelope::Payload::SaveReviewMark(_))
        ),
        "窗口外的 Keep 也必须可以复核: {:?}",
        keep_response.payload
    );

    let delete_response = handle
        .handle(proto::Envelope {
            request_id: 2,
            payload: Some(proto::envelope::Payload::SaveReviewMark(
                proto::SaveReviewMark {
                    group_id: "large-group".into(),
                    location: Some((&target_location).into()),
                    decision: proto::ReviewDecision::ReviewDelete as i32,
                    analysis_run_id: run_id.as_uuid().to_string(),
                },
            )),
        })
        .await;
    assert!(matches!(
        delete_response.payload,
        Some(proto::envelope::Payload::SaveReviewMark(_))
    ));

    let response = handle
        .handle(proto::Envelope {
            request_id: 3,
            payload: Some(proto::envelope::Payload::CreateDeleteBatch(
                proto::CreateDeleteBatch {
                    mode: proto::DeleteMode::DeletePermanent as i32,
                    analysis_run_id: run_id.as_uuid().to_string(),
                    items: vec![proto::DeleteItem {
                        group_id: "large-group".into(),
                        location: Some((&target_location).into()),
                        expected_content: Some((&expected).into()),
                        ..Default::default()
                    }],
                    ..Default::default()
                },
            )),
        })
        .await;
    assert!(matches!(
        response.payload,
        Some(proto::envelope::Payload::CreateDeleteBatch(_))
    ));
    assert!(!target.exists());
    assert!(keep.exists());
    handle.shutdown().await.unwrap();
    actor.await.unwrap();
}

fn response_task_id(response: &proto::Envelope) -> String {
    match response.payload.as_ref() {
        Some(proto::envelope::Payload::CreateDeleteBatch(batch)) => batch.delete_batch_id.clone(),
        _ => String::new(),
    }
}

fn plan_item(item_id: &str, path: &Path, bytes: &[u8], machine: &MachineId) -> PlannedDeleteItem {
    let normalized = NormalizedPath::new(path).unwrap();
    PlannedDeleteItem {
        item_id: item_id.into(),
        group_id: "group-1".into(),
        location: LocationKey::new(machine.clone(), normalized),
        expected: ContentKey::new(md5_bytes(bytes), bytes.len() as u64),
    }
}

fn scanned(path: &str, file_size: u64) -> ScannedPath {
    let normalized = NormalizedPath::new(path).unwrap();
    ScannedPath::new(
        normalized.clone(),
        DisplayPath::new(path).unwrap(),
        file_size,
    )
}
