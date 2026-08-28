//! 当前进程任务的状态、计数和持久化事件序号契约。

use dedup_core::{DisplayPath, MachineId, MediaKind, NormalizedPath, TaskId};
use dedup_node_store::{
    FeatureWrite, FileFaultKind, FileFaultRecord, NewTaskItem, NodeStore, ScannedPath,
    TaskItemApplyResult, TaskItemCompletion, TaskItemIdentity, TaskItemStatus, TaskStatus,
};

fn machine() -> MachineId {
    MachineId::from_sha256([0x71; 32])
}

/// 创建一个已经冻结 task/item/content 身份的 running 基础计算项。
fn active_content_item(store: &mut NodeStore, seed: u8) -> (TaskItemIdentity, ScannedPath) {
    let root = NormalizedPath::new(r"D:\Guard").unwrap();
    let normalized = NormalizedPath::new(format!(r"D:\Guard\item-{seed}.jpg")).unwrap();
    let display = DisplayPath::new(format!(r"D:\Guard\item-{seed}.jpg")).unwrap();
    let scanned = ScannedPath::new(normalized, display, u64::from(seed) + 100);
    let task_id = store.create_scan_task(&[root], i64::from(seed)).unwrap();
    let item_id = store
        .reserve_scan_path(task_id, &scanned, i64::from(seed) + 1)
        .unwrap()
        .unwrap();
    let content = store
        .upsert_content_and_location(&scanned, [seed; 16], MediaKind::Image)
        .unwrap();
    store
        .set_running_item_content_and_stage(&item_id, content.id, "base_compute")
        .unwrap();
    (
        TaskItemIdentity {
            task_id,
            item_id,
            content_id: Some(content.id),
        },
        scanned,
    )
}

/// 返回当前 SQLite outbox 行数，用于确认忽略/拒绝分支没有同步副作用。
fn outbox_len(store: &NodeStore) -> usize {
    store.pull_changes(0, 1_000).unwrap().changes.len()
}

/// 构造与持久任务项文件身份一致的 Worker 崩溃记录。
fn crash_fault(scanned: &ScannedPath, now_ms: u64) -> FileFaultRecord {
    FileFaultRecord {
        machine_id: machine(),
        normalized_path: scanned.normalized_path.clone(),
        display_path: scanned.display_path.clone(),
        file_size: scanned.file_size,
        kind: FileFaultKind::WorkerCrash,
        stage: "base_compute".into(),
        windows_error_code: None,
        read_offset: None,
        read_size: None,
        worker_pid: Some(42),
        worker_exit_code: Some(-1),
        first_seen_at_ms: now_ms,
        last_seen_at_ms: now_ms,
        occurrence_count: 1,
        message: "Worker 管道断开".into(),
    }
}

/// 单项失败是任务统计，不阻止其他项完成，也不把整个任务误标为 failed。
#[test]
fn item_failure_still_allows_task_to_complete() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let task_id = store
        .create_task(
            "stage1",
            &[NewTaskItem::detached("a"), NewTaskItem::detached("b")],
            10,
        )
        .unwrap();
    let first = store.claim_next_item(task_id, 11).unwrap().unwrap();
    store
        .complete_item(
            &first.item_id,
            TaskItemCompletion::Failed("decode failed".into()),
            12,
        )
        .unwrap();
    let second = store.claim_next_item(task_id, 13).unwrap().unwrap();
    store
        .complete_item(
            &second.item_id,
            TaskItemCompletion::Succeeded { content_id: None },
            14,
        )
        .unwrap();

    let snapshot = store.task_snapshot(task_id).unwrap();
    assert_eq!(snapshot.status, TaskStatus::Completed);
    assert_eq!((snapshot.succeeded, snapshot.failed), (1, 1));
}

#[test]
fn reopening_preserves_file_fault_after_transient_task_is_discarded() {
    let directory = tempfile::tempdir().unwrap();
    let database = directory.path().join("crashed.db");
    let path = NormalizedPath::new(r"D:\Crash\file-a.mp4").unwrap();
    {
        let mut store = NodeStore::open(&database, machine()).unwrap();
        let task_id = store
            .create_scan_task(&[NormalizedPath::new(r"D:\Crash").unwrap()], 1)
            .unwrap();
        let item_id = store
            .reserve_scan_path(
                task_id,
                &ScannedPath::new(
                    path.clone(),
                    DisplayPath::new(r"D:\Crash\file-a.mp4").unwrap(),
                    1234,
                ),
                2,
            )
            .unwrap()
            .unwrap();
        store
            .fail_running_item_with_file_fault(
                &item_id,
                &FileFaultRecord {
                    machine_id: machine(),
                    normalized_path: path.clone(),
                    display_path: DisplayPath::new(r"D:\Crash\file-a.mp4").unwrap(),
                    file_size: 1234,
                    kind: FileFaultKind::WorkerCrash,
                    stage: "enumerated".into(),
                    windows_error_code: None,
                    read_offset: None,
                    read_size: None,
                    worker_pid: None,
                    worker_exit_code: None,
                    first_seen_at_ms: 3,
                    last_seen_at_ms: 3,
                    occurrence_count: 1,
                    message: "Worker 处理文件时崩溃".into(),
                },
                "Worker 处理文件时崩溃",
                3,
            )
            .unwrap();
    }

    let store = NodeStore::open(&database, machine()).unwrap();
    let faults = store.page_file_faults(None, 10).unwrap();
    assert_eq!(faults.items.len(), 1);
    assert_eq!(faults.items[0].machine_id, machine());
    assert_eq!(faults.items[0].normalized_path, path);
    assert_eq!(faults.items[0].file_size, 1234);
    assert_eq!(faults.items[0].kind, FileFaultKind::WorkerCrash);
    assert_eq!(faults.items[0].stage, "enumerated");
    assert_eq!(faults.items[0].windows_error_code, None);
}

#[test]
fn crashed_item_accepts_diagnostic_stage_and_keeps_full_display_path() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let root = NormalizedPath::new(r"I:\媒体库").unwrap();
    let normalized = NormalizedPath::new(r"I:\媒体库\歌手 A\现场\崩溃样本.mp4").unwrap();
    let display = DisplayPath::new(r"I:\媒体库\歌手 A\现场\崩溃样本.mp4").unwrap();
    let task = store.create_scan_task(&[root], 10).unwrap();
    let item_id = store
        .reserve_scan_path(
            task,
            &ScannedPath::new(normalized.clone(), display.clone(), 4096),
            11,
        )
        .unwrap()
        .unwrap();

    store
        .fail_running_item_with_file_fault(
            &item_id,
            &FileFaultRecord {
                machine_id: machine(),
                normalized_path: normalized,
                display_path: display.clone(),
                file_size: 4096,
                kind: FileFaultKind::WorkerCrash,
                stage: "base_compute".into(),
                windows_error_code: None,
                read_offset: None,
                read_size: None,
                worker_pid: Some(10528),
                worker_exit_code: Some(0xc000_0374_u32 as i32),
                first_seen_at_ms: 12,
                last_seen_at_ms: 12,
                occurrence_count: 1,
                message: "Worker 管道断开".into(),
            },
            "Worker 管道断开",
            12,
        )
        .unwrap();

    let fault = store.page_file_faults(None, 10).unwrap().items.remove(0);
    assert_eq!(fault.display_path.as_path(), display.as_path());
    assert_eq!(fault.stage, "base_compute");
    assert_eq!(fault.worker_pid, Some(10528));
    assert_eq!(fault.worker_exit_code, Some(0xc000_0374_u32 as i32));
}

#[test]
fn crashed_item_rejects_mismatched_fault_identity_without_side_effects() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let task = store
        .create_scan_task(&[NormalizedPath::new(r"D:\Crash").unwrap()], 10)
        .unwrap();
    let item_id = store
        .reserve_scan_path(
            task,
            &ScannedPath::new(
                NormalizedPath::new(r"D:\Crash\file-a.mp4").unwrap(),
                DisplayPath::new(r"D:\Crash\file-a.mp4").unwrap(),
                10,
            ),
            11,
        )
        .unwrap()
        .unwrap();

    let result = store.fail_running_item_with_file_fault(
        &item_id,
        &FileFaultRecord {
            machine_id: machine(),
            normalized_path: NormalizedPath::new(r"D:\Crash\file-b.mp4").unwrap(),
            display_path: DisplayPath::new(r"D:\Crash\file-b.mp4").unwrap(),
            file_size: 99,
            kind: FileFaultKind::WorkerCrash,
            stage: "different-stage".into(),
            windows_error_code: None,
            read_offset: None,
            read_size: None,
            worker_pid: None,
            worker_exit_code: None,
            first_seen_at_ms: 12,
            last_seen_at_ms: 12,
            occurrence_count: 1,
            message: "mismatched".into(),
        },
        "mismatched",
        12,
    );

    assert!(result.is_err());
    let item = store
        .task_items(task)
        .unwrap()
        .into_iter()
        .find(|item| item.item_id == item_id)
        .unwrap();
    assert_eq!(item.status, TaskItemStatus::Running);
    assert_eq!(store.task_snapshot(task).unwrap().failed, 0);
    assert!(store.page_file_faults(None, 10).unwrap().items.is_empty());
}

/// 一筛提交必须在同一事务内区分正确身份、非活动项和身份错配。
#[test]
fn stage1_commit_classifies_identity_and_activity_atomically() {
    enum Case {
        Applied,
        Cancelled,
        WrongTask,
        WrongContent,
    }

    for (index, case) in [
        Case::Applied,
        Case::Cancelled,
        Case::WrongTask,
        Case::WrongContent,
    ]
    .into_iter()
    .enumerate()
    {
        let mut store = NodeStore::open_in_memory(machine()).unwrap();
        let seed = u8::try_from(index + 1).unwrap();
        let (mut identity, _) = active_content_item(&mut store, seed);

        match case {
            Case::Applied => {}
            Case::Cancelled => {
                store.cancel_task(identity.task_id, 20).unwrap();
            }
            Case::WrongTask => identity.task_id = TaskId::new(),
            Case::WrongContent => {
                let other = ScannedPath::new(
                    NormalizedPath::new(format!(r"D:\Guard\other-{seed}.jpg")).unwrap(),
                    DisplayPath::new(format!(r"D:\Guard\other-{seed}.jpg")).unwrap(),
                    u64::from(seed) + 200,
                );
                identity.content_id = Some(
                    store
                        .upsert_content_and_location(
                            &other,
                            [seed.saturating_add(32); 16],
                            MediaKind::Image,
                        )
                        .unwrap()
                        .id,
                );
            }
        }

        let before_task = store.task_snapshot(identity.task_id).ok();
        let before_outbox = outbox_len(&store);
        let persisted_content = store
            .content_id_by_key(dedup_core::ContentKey::new(
                [seed; 16],
                u64::from(seed) + 100,
            ))
            .unwrap()
            .unwrap();
        let contact_path = format!("contact/{seed}.jpg");
        let result = store
            .commit_scan_stage1_guarded(
                &identity,
                MediaKind::Image,
                vec![FeatureWrite::ContactSheet(contact_path.clone())],
                30,
            )
            .unwrap();

        match case {
            Case::Applied => {
                let TaskItemApplyResult::Applied(event) = result else {
                    panic!("正确身份必须提交成功，实际为 {result:?}");
                };
                assert_eq!(event.task_id, identity.task_id);
                assert_eq!(event.item_id, identity.item_id);
                assert_eq!(event.item_status, TaskItemStatus::Succeeded);
                assert_eq!(
                    store.contact_sheet_path(persisted_content).unwrap(),
                    Some(contact_path)
                );
                assert!(outbox_len(&store) > before_outbox);
                assert_eq!(store.task_snapshot(identity.task_id).unwrap().succeeded, 1);
            }
            Case::Cancelled => {
                assert_eq!(result, TaskItemApplyResult::IgnoredInactive);
                assert_eq!(store.task_snapshot(identity.task_id).ok(), before_task);
                assert_eq!(outbox_len(&store), before_outbox);
                assert_eq!(store.contact_sheet_path(persisted_content).unwrap(), None);
            }
            Case::WrongTask | Case::WrongContent => {
                assert_eq!(result, TaskItemApplyResult::IdentityMismatch);
                assert_eq!(store.task_snapshot(identity.task_id).ok(), before_task);
                assert_eq!(outbox_len(&store), before_outbox);
                assert_eq!(store.contact_sheet_path(persisted_content).unwrap(), None);
            }
        }
    }
}

/// 普通完成和 Worker 崩溃也必须经过相同的 task/item/content 身份门禁。
#[test]
fn guarded_complete_and_crash_require_matching_identity() {
    let mut complete_store = NodeStore::open_in_memory(machine()).unwrap();
    let (complete_identity, _) = active_content_item(&mut complete_store, 41);
    let mut wrong_content = complete_identity.clone();
    wrong_content.content_id = None;
    let before_complete = complete_store
        .task_snapshot(complete_identity.task_id)
        .unwrap();
    assert_eq!(
        complete_store
            .complete_item_guarded(
                &wrong_content,
                TaskItemCompletion::Succeeded {
                    content_id: complete_identity.content_id,
                },
                50,
            )
            .unwrap(),
        TaskItemApplyResult::IdentityMismatch
    );
    assert_eq!(
        complete_store
            .task_snapshot(complete_identity.task_id)
            .unwrap(),
        before_complete
    );
    assert!(matches!(
        complete_store
            .complete_item_guarded(
                &complete_identity,
                TaskItemCompletion::Succeeded {
                    content_id: complete_identity.content_id,
                },
                51,
            )
            .unwrap(),
        TaskItemApplyResult::Applied(_)
    ));

    let mut crash_store = NodeStore::open_in_memory(machine()).unwrap();
    let (crash_identity, scanned) = active_content_item(&mut crash_store, 42);
    let mut wrong_task = crash_identity.clone();
    wrong_task.task_id = TaskId::new();
    let before_crash = crash_store.task_snapshot(crash_identity.task_id).unwrap();
    assert_eq!(
        crash_store
            .fail_running_item_with_file_fault_guarded(
                &wrong_task,
                &crash_fault(&scanned, 60),
                "Worker 管道断开",
                60,
            )
            .unwrap(),
        TaskItemApplyResult::IdentityMismatch
    );
    assert_eq!(
        crash_store.task_snapshot(crash_identity.task_id).unwrap(),
        before_crash
    );
    assert!(
        crash_store
            .page_file_faults(None, 10)
            .unwrap()
            .items
            .is_empty()
    );
    assert!(matches!(
        crash_store
            .fail_running_item_with_file_fault_guarded(
                &crash_identity,
                &crash_fault(&scanned, 61),
                "Worker 管道断开",
                61,
            )
            .unwrap(),
        TaskItemApplyResult::Applied(_)
    ));
    assert_eq!(
        crash_store
            .task_snapshot(crash_identity.task_id)
            .unwrap()
            .failed,
        1
    );
    assert_eq!(
        crash_store.page_file_faults(None, 10).unwrap().items.len(),
        1
    );
}
