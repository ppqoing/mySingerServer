use std::{fs, io::Read};

use dedup_core::{ContentKey, DeleteMode, LocationKey, MachineId, NormalizedPath};
use dedup_node_engine::delete_queue::{DeleteQueueStatus, TransientDeleteQueue};
use dedup_node_store::PlannedDeleteItem;
use tempfile::tempdir;

fn item(id: &str, path: &str, byte: u8) -> PlannedDeleteItem {
    let normalized = NormalizedPath::new(path).unwrap();
    PlannedDeleteItem {
        item_id: id.to_owned(),
        group_id: "group-a".to_owned(),
        location: LocationKey::new(MachineId::from_sha256([byte; 32]), normalized),
        expected: ContentKey::new([byte; 16], u64::from(byte)),
    }
}

#[test]
fn queue_writes_fixed_tsv_and_reads_planned_items_in_order() {
    let root = tempdir().unwrap();
    let first = item("item-1", r"C:\Media\one.jpg", 1);
    let second = item("item-2", r"D:\Media\two.jpg", 2);
    let queue = TransientDeleteQueue::create_new(
        root.path(),
        "run-1",
        DeleteMode::Permanent,
        &[first.clone(), second.clone()],
    )
    .unwrap();

    let mut bytes = Vec::new();
    std::fs::File::open(queue.path())
        .unwrap()
        .read_to_end(&mut bytes)
        .unwrap();
    assert!(!bytes.starts_with(&[0xef, 0xbb, 0xbf]));
    assert!(bytes.ends_with(b"\n"));
    assert!(!bytes.contains(&b'\r'));
    assert!(bytes.starts_with(b"P\titem-1\tgroup-a\t"));
    assert!(String::from_utf8(bytes).unwrap().contains("\tpermanent\n"));

    drop(queue);
    let mut queue = TransientDeleteQueue::open_existing(root.path(), "run-1").unwrap();
    assert_eq!(
        queue.next_pending_entry().unwrap().unwrap().mode,
        DeleteMode::Permanent
    );
    assert_eq!(queue.next_pending().unwrap(), Some(first.clone()));
    assert_eq!(queue.next_pending().unwrap(), Some(first));
    queue.ack_sqlite("item-1").unwrap();
    assert_eq!(
        queue.status("item-1").unwrap(),
        Some(DeleteQueueStatus::Completed)
    );
    assert_eq!(queue.next_pending().unwrap(), Some(second.clone()));
    queue.mark_failed("item-2").unwrap();
    assert_eq!(
        queue.status("item-2").unwrap(),
        Some(DeleteQueueStatus::Failed)
    );
    assert_eq!(queue.next_pending().unwrap(), None);
    let content = fs::read_to_string(queue.path()).unwrap();
    assert!(content.starts_with("C\titem-1\t"));
    assert!(content.contains("\nF\titem-2\t"));

    let run_dir = queue.run_dir().to_path_buf();
    queue.cleanup().unwrap();
    assert!(!run_dir.exists());
    assert!(root.path().exists());
}

#[test]
fn queue_rejects_duplicate_identity_and_paths_outside_runtime_root() {
    let root = tempdir().unwrap();
    let first = item("item-1", r"C:\Media\one.jpg", 1);
    let duplicate = item("item-1", r"C:\Media\two.jpg", 2);
    assert!(
        TransientDeleteQueue::create_new(
            root.path(),
            "run-duplicate",
            DeleteMode::Permanent,
            &[first.clone(), duplicate],
        )
        .is_err()
    );

    let outside = root.path().join("outside");
    std::fs::create_dir(&outside).unwrap();
    assert!(TransientDeleteQueue::open_existing(root.path(), "..\\outside").is_err());
    assert!(TransientDeleteQueue::open_existing(&outside, "run-1").is_err());
}

#[test]
fn queue_requires_explicit_sqlite_ack_for_completed_state() {
    let root = tempdir().unwrap();
    let first = item("item-1", r"C:\Media\one.jpg", 1);
    let mut queue =
        TransientDeleteQueue::create_new(root.path(), "run-ack", DeleteMode::RecycleBin, &[first])
            .unwrap();
    queue.next_pending().unwrap();
    assert_eq!(
        queue.status("item-1").unwrap(),
        Some(DeleteQueueStatus::Pending)
    );
    assert!(queue.cleanup().is_err());
    queue.ack_sqlite("item-1").unwrap();
    assert_eq!(
        queue.status("item-1").unwrap(),
        Some(DeleteQueueStatus::Completed)
    );
}

#[test]
fn queue_rejects_bom_crlf_invalid_columns_and_invalid_fields() {
    let root = tempdir().unwrap();
    let machine = "0".repeat(64);
    let valid = format!(
        "P\titem-1\tgroup-a\t{machine}\tC:\\MEDIA\\ONE.JPG\t{}\t1\tpermanent\n",
        "01".repeat(16)
    );
    let cases = [
        ("bom", format!("\u{feff}{valid}")),
        ("crlf", valid.replace('\n', "\r\n")),
        (
            "columns",
            valid
                .split_once('\n')
                .unwrap()
                .0
                .split('\t')
                .take(7)
                .collect::<Vec<_>>()
                .join("\t"),
        ),
        ("mode", valid.replace("permanent", "archive")),
        ("md5", valid.replace(&"01".repeat(16), "00")),
    ];
    for (name, content) in cases {
        let run_id = format!("run-{name}");
        let run_dir = root.path().join(&run_id);
        fs::create_dir(&run_dir).unwrap();
        fs::write(run_dir.join("delete.tasks.tsv"), content.as_bytes()).unwrap();
        assert!(
            TransientDeleteQueue::open_existing(root.path(), &run_id).is_err(),
            "非法队列 {name} 不应被接受"
        );
    }
}
