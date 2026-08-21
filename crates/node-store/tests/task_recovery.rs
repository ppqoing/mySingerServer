//! 可恢复任务的状态、计数和持久化事件序号契约。

use dedup_core::{DisplayPath, MachineId, NormalizedPath};
use dedup_node_store::{
    FileFaultKind, FileFaultRecord, NewTaskItem, NodeStore, ScannedPath, TaskItemCompletion,
    TaskItemStatus, TaskStatus,
};

fn machine() -> MachineId {
    MachineId::from_sha256([0x71; 32])
}

/// 模拟进程在一个 item 运行时退出；重开后只允许该 item 回到队列。
#[test]
fn reopening_requeues_only_running_items() {
    let directory = tempfile::tempdir().unwrap();
    let database = directory.path().join("node.db");
    let task_id;
    {
        let mut store = NodeStore::open(&database, machine()).unwrap();
        let items = (0..5)
            .map(|index| NewTaskItem::detached(format!("stage-{index}")))
            .collect::<Vec<_>>();
        task_id = store.create_task("scan", &items, 100).unwrap();

        let succeeded = store.claim_next_item(task_id, 101).unwrap().unwrap();
        let first = store
            .complete_item(
                &succeeded.item_id,
                TaskItemCompletion::Succeeded { content_id: None },
                102,
            )
            .unwrap();
        assert_eq!(first.event_seq, 1);

        let failed = store.claim_next_item(task_id, 103).unwrap().unwrap();
        let second = store
            .complete_item(
                &failed.item_id,
                TaskItemCompletion::Failed("broken fixture".into()),
                104,
            )
            .unwrap();
        assert_eq!(second.event_seq, 2);

        let cancelled = store.claim_next_item(task_id, 105).unwrap().unwrap();
        store
            .complete_item(&cancelled.item_id, TaskItemCompletion::Cancelled, 106)
            .unwrap();

        let running = store.claim_next_item(task_id, 107).unwrap().unwrap();
        assert_eq!(running.status, TaskItemStatus::Running);
    }

    let mut store = NodeStore::open(&database, machine()).unwrap();
    assert_eq!(store.recover_running_items(200).unwrap(), 1);
    let snapshot = store.task_snapshot(task_id).unwrap();
    assert_eq!(snapshot.status, TaskStatus::Queued);
    assert_eq!(snapshot.succeeded, 1);
    assert_eq!(snapshot.failed, 1);
    assert_eq!(snapshot.cancelled, 1);
    assert_eq!(
        store
            .task_items(task_id)
            .unwrap()
            .iter()
            .filter(|item| item.status == TaskItemStatus::Queued)
            .count(),
        2
    );

    for now in [201, 203] {
        let item = store.claim_next_item(task_id, now).unwrap().unwrap();
        store
            .complete_item(
                &item.item_id,
                TaskItemCompletion::Succeeded { content_id: None },
                now + 1,
            )
            .unwrap();
    }
    assert!(store.claim_next_item(task_id, 300).unwrap().is_none());
    let completed = store.task_snapshot(task_id).unwrap();
    assert_eq!(completed.status, TaskStatus::Completed);
    assert_eq!(completed.event_seq, 5);
    assert_eq!(completed.succeeded, 3);
    assert_eq!(completed.failed, 1);
    assert_eq!(completed.cancelled, 1);
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
fn crashed_item_is_failed_with_fault_and_never_requeued_after_reopen() {
    let directory = tempfile::tempdir().unwrap();
    let database = directory.path().join("crashed.db");
    let task_id;
    let item_id;
    let path = NormalizedPath::new(r"D:\Crash\file-a.mp4").unwrap();
    {
        let mut store = NodeStore::open(&database, machine()).unwrap();
        task_id = store
            .create_scan_task(&[NormalizedPath::new(r"D:\Crash").unwrap()], 1)
            .unwrap();
        item_id = store
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
                    message: "Worker 处理文件时崩溃".into(),
                },
                "Worker 处理文件时崩溃",
                3,
            )
            .unwrap();
    }

    let mut store = NodeStore::open(&database, machine()).unwrap();
    assert_eq!(store.recover_running_items(4).unwrap(), 0);
    let item = store
        .task_items(task_id)
        .unwrap()
        .into_iter()
        .find(|item| item.item_id == item_id)
        .unwrap();
    assert_eq!(item.status, TaskItemStatus::Failed);
    assert_eq!(store.task_snapshot(task_id).unwrap().failed, 1);
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
