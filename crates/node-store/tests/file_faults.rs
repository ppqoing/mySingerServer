//! 节点文件故障的唯一键、更新字段、清理和稳定分页行为。

use dedup_core::{DisplayPath, MachineId, NormalizedPath};
use dedup_node_store::{FileFaultKind, FileFaultRecord, NodeStore};

fn machine(byte: &str) -> MachineId {
    MachineId::parse(&byte.repeat(32)).unwrap()
}

fn fault(
    machine_id: &MachineId,
    path: &str,
    display_path: &str,
    kind: FileFaultKind,
    file_size: u64,
    stage: &str,
    windows_error_code: Option<i32>,
    message: &str,
) -> FileFaultRecord {
    FileFaultRecord {
        machine_id: machine_id.clone(),
        normalized_path: NormalizedPath::new(path).unwrap(),
        display_path: DisplayPath::new(display_path).unwrap(),
        file_size,
        kind,
        stage: stage.to_owned(),
        windows_error_code,
        read_offset: None,
        read_size: None,
        worker_pid: None,
        worker_exit_code: None,
        first_seen_at_ms: 100,
        last_seen_at_ms: 100,
        occurrence_count: 1,
        message: message.to_owned(),
    }
}

#[test]
fn upsert_updates_only_mutable_fault_details_and_clear_removes_all_kinds_for_one_path() {
    let local = machine("11");
    let remote = machine("22");
    let mut store = NodeStore::open_in_memory(local.clone()).unwrap();
    let original = fault(
        &local,
        r"D:\Media\broken.mp4",
        r"D:\Media\broken.mp4",
        FileFaultKind::SuspectedPhysicalRead,
        100,
        "read",
        Some(23),
        "第一次失败",
    );
    store.upsert_file_fault(&original).unwrap();

    let mut replacement = fault(
        &local,
        r"d:\MEDIA\BROKEN.mp4",
        r"D:\DIFFERENT-SPELLING.mp4",
        FileFaultKind::SuspectedPhysicalRead,
        120,
        "hash",
        Some(1117),
        "重试后仍失败",
    );
    replacement.read_offset = Some(4 * 1024 * 1024);
    replacement.read_size = Some(4 * 1024 * 1024);
    replacement.last_seen_at_ms = 200;
    store.upsert_file_fault(&replacement).unwrap();
    store
        .upsert_file_fault(&fault(
            &local,
            r"D:\Media\broken.mp4",
            r"D:\Media\broken.mp4",
            FileFaultKind::WorkerCrash,
            120,
            "probe",
            None,
            "Worker 崩溃",
        ))
        .unwrap();
    store
        .upsert_file_fault(&fault(
            &remote,
            r"D:\Media\broken.mp4",
            r"D:\Media\broken.mp4",
            FileFaultKind::WorkerCrash,
            120,
            "probe",
            None,
            "另一台机器",
        ))
        .unwrap();
    store
        .upsert_file_fault(&fault(
            &local,
            r"D:\Media\healthy.mp4",
            r"D:\Media\healthy.mp4",
            FileFaultKind::WorkerCrash,
            80,
            "probe",
            None,
            "同机器其他路径",
        ))
        .unwrap();

    let page = store.page_file_faults(None, 10).unwrap();
    assert_eq!(page.items.len(), 4);
    let updated = page
        .items
        .iter()
        .find(|item| item.machine_id == local && item.kind == FileFaultKind::SuspectedPhysicalRead)
        .unwrap();
    assert_eq!(
        updated.display_path.as_path(),
        original.display_path.as_path()
    );
    assert_eq!(updated.normalized_path, original.normalized_path);
    assert_eq!(updated.file_size, 120);
    assert_eq!(updated.stage, "hash");
    assert_eq!(updated.windows_error_code, Some(1117));
    assert_eq!(updated.read_offset, Some(4 * 1024 * 1024));
    assert_eq!(updated.read_size, Some(4 * 1024 * 1024));
    assert_eq!(updated.first_seen_at_ms, 100);
    assert_eq!(updated.last_seen_at_ms, 200);
    assert_eq!(updated.occurrence_count, 2);
    assert_eq!(updated.message, "重试后仍失败");

    assert_eq!(
        store
            .clear_file_fault(&local, &original.normalized_path)
            .unwrap(),
        2
    );
    let remaining = store.page_file_faults(None, 10).unwrap();
    assert_eq!(remaining.items.len(), 2);
    assert!(remaining.items.iter().any(|item| item.machine_id == remote));
    assert!(remaining.items.iter().any(|item| {
        item.machine_id == local
            && item.normalized_path == NormalizedPath::new(r"D:\Media\healthy.mp4").unwrap()
    }));
}

#[test]
fn file_fault_pages_follow_stable_machine_path_kind_order_without_duplicates() {
    let first_machine = machine("11");
    let second_machine = machine("22");
    let mut store = NodeStore::open_in_memory(first_machine.clone()).unwrap();
    let fixtures = [
        fault(
            &second_machine,
            r"D:\B.bin",
            r"D:\B.bin",
            FileFaultKind::WorkerCrash,
            4,
            "probe",
            None,
            "four",
        ),
        fault(
            &first_machine,
            r"D:\B.bin",
            r"D:\B.bin",
            FileFaultKind::WorkerCrash,
            3,
            "probe",
            None,
            "three",
        ),
        fault(
            &first_machine,
            r"D:\A.bin",
            r"D:\A.bin",
            FileFaultKind::WorkerCrash,
            2,
            "probe",
            None,
            "two",
        ),
        fault(
            &first_machine,
            r"D:\A.bin",
            r"D:\A.bin",
            FileFaultKind::SuspectedPhysicalRead,
            1,
            "read",
            Some(23),
            "one",
        ),
    ];
    for fixture in &fixtures {
        store.upsert_file_fault(fixture).unwrap();
    }

    let first_page = store.page_file_faults(None, 2).unwrap();
    assert_eq!(
        first_page
            .items
            .iter()
            .map(|item| item.message.as_str())
            .collect::<Vec<_>>(),
        vec!["one", "two"]
    );
    let second_page = store
        .page_file_faults(first_page.next_cursor.as_deref(), 2)
        .unwrap();
    assert_eq!(
        second_page
            .items
            .iter()
            .map(|item| item.message.as_str())
            .collect::<Vec<_>>(),
        vec!["three", "four"]
    );
    assert!(second_page.next_cursor.is_none());
}
