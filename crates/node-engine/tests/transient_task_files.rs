use std::{fs, path::PathBuf};

use dedup_core::{DisplayPath, NormalizedPath};
use dedup_node_engine::{
    scan::TaskDiskLane,
    task_files::{
        TaskFileIdentity, TaskFileRecord, TaskWorkKind, TaskWorkMask, TransientTaskFileSet,
    },
};
use dedup_node_store::ScannedPath;
use dedup_windows::{LocalDiskKind, PhysicalDiskId};
use uuid::Uuid;

fn lane(numbers: &[u32], kind: LocalDiskKind, limit: usize) -> TaskDiskLane {
    TaskDiskLane {
        physical_disk_id: PhysicalDiskId::from_disk_numbers(numbers.iter().copied()).unwrap(),
        physical_disk_numbers: numbers.to_vec(),
        disk_kind: kind,
        configured_weight: limit,
        per_disk_limit: limit,
    }
}

fn scanned(name: &str) -> ScannedPath {
    let path = format!(r"C:\media\{name}");
    ScannedPath::new(
        NormalizedPath::new(&path).unwrap(),
        DisplayPath::new(&path).unwrap(),
        42,
    )
}

fn record(name: &str, missing: TaskWorkMask) -> TaskFileRecord {
    TaskFileRecord {
        item_id: Uuid::now_v7(),
        work_kind: TaskWorkKind::Base,
        scanned: scanned(name),
        known_md5: None,
        missing,
    }
}

fn runtime_root() -> tempfile::TempDir {
    tempfile::tempdir().unwrap()
}

fn run_id() -> String {
    Uuid::now_v7().to_string()
}

#[test]
fn task_rows_are_fixed_tsv_without_json_or_bom() {
    let root = runtime_root();
    let id = run_id();
    let task_lane = lane(&[7], LocalDiskKind::Hdd, 1);
    let mut files = TransientTaskFileSet::create(root.path(), &id).unwrap();
    let row = record("one.jpg", TaskWorkMask::from_bits(1 << 3).unwrap());
    files
        .append_batch(&task_lane, std::slice::from_ref(&row))
        .unwrap();

    let bytes = fs::read(files.lane_path(&task_lane).unwrap()).unwrap();
    assert_eq!(bytes[0], b'P');
    assert!(!bytes.starts_with(&[0xEF, 0xBB, 0xBF]));
    assert_eq!(bytes.iter().filter(|byte| **byte == b'\t').count(), 7);
    assert_eq!(bytes.last(), Some(&b'\n'));
    assert!(!bytes.windows(4).any(|part| part == b"json"));
    assert!(
        !root
            .path()
            .join(&id)
            .join("PhysicalDisk7-hdd.tasks.tsv.idx")
            .exists()
    );
}

#[test]
fn rejects_invalid_ids_masks_paths_and_cross_lane_duplicates_before_writing() {
    let root = runtime_root();
    let id = run_id();
    let first_lane = lane(&[7], LocalDiskKind::Hdd, 1);
    let second_lane = lane(&[8, 5, 8], LocalDiskKind::Unknown, 2);
    let mut files = TransientTaskFileSet::create(root.path(), &id).unwrap();

    let empty_mask = record("empty.bin", TaskWorkMask::empty());
    assert!(files.append_batch(&first_lane, &[empty_mask]).is_err());

    let mut bad_path = record("bad\tpath.bin", TaskWorkMask::from_bits(1 << 3).unwrap());
    bad_path.scanned.display_path =
        DisplayPath::new(PathBuf::from("C:\\media\\bad\tpath.bin")).unwrap();
    assert!(files.append_batch(&first_lane, &[bad_path]).is_err());

    let mut invalid_uuid = record("invalid.bin", TaskWorkMask::from_bits(1 << 3).unwrap());
    invalid_uuid.item_id = Uuid::new_v4();
    assert!(files.append_batch(&first_lane, &[invalid_uuid]).is_err());

    let shared = record("shared.bin", TaskWorkMask::from_bits(1 << 3).unwrap());
    let shared_id = shared.item_id;
    files
        .append_batch(&first_lane, std::slice::from_ref(&shared))
        .unwrap();
    let mut duplicate = record("other.bin", TaskWorkMask::from_bits(1 << 3).unwrap());
    duplicate.item_id = shared_id;
    assert!(files.append_batch(&second_lane, &[duplicate]).is_err());

    files.seal().unwrap();
    assert!(
        files
            .append_batch(
                &first_lane,
                &[record(
                    "after-seal.bin",
                    TaskWorkMask::from_bits(1 << 3).unwrap()
                )]
            )
            .is_err()
    );
    assert!(
        !root
            .path()
            .join(&id)
            .join("PhysicalDisk5+8-unknown.tasks.tsv")
            .exists()
    );
}

#[test]
fn append_seal_take_and_ack_change_only_the_status_byte() {
    let root = runtime_root();
    let id = run_id();
    let task_lane = lane(&[7], LocalDiskKind::Hdd, 1);
    let mut files = TransientTaskFileSet::create(root.path(), &id).unwrap();
    let row = record("one.jpg", TaskWorkMask::from_bits(1 << 3).unwrap());
    let identities = files
        .append_batch(&task_lane, std::slice::from_ref(&row))
        .unwrap();
    let identity = identities[0].clone();
    files.seal().unwrap();

    assert!(!files.all_terminal());
    assert!(files.mark_completed(&identity).is_err());
    assert_eq!(
        fs::read(files.lane_path(&task_lane).unwrap()).unwrap()[0],
        b'P'
    );

    let (taken_identity, taken_row) = files.take_lane(&identity).unwrap().unwrap();
    assert_eq!(taken_identity, identity);
    assert_eq!(taken_row, row);
    assert!(!files.all_terminal());

    let before = fs::read(files.lane_path(&task_lane).unwrap()).unwrap();
    files.mark_completed(&identity).unwrap();
    let after = fs::read(files.lane_path(&task_lane).unwrap()).unwrap();
    assert_eq!(after.len(), before.len());
    assert_eq!(&after[1..], &before[1..]);
    assert_eq!(after[0], b'C');
    files.mark_completed(&identity).unwrap();
    assert!(files.all_terminal());
}

#[test]
fn failed_item_is_marked_f_and_inflight_blocks_terminal_state() {
    let root = runtime_root();
    let id = run_id();
    let task_lane = lane(&[7], LocalDiskKind::Hdd, 1);
    let mut files = TransientTaskFileSet::create(root.path(), &id).unwrap();
    let row = record("failed.jpg", TaskWorkMask::from_bits(1 << 3).unwrap());
    let identity = files
        .append_batch(&task_lane, std::slice::from_ref(&row))
        .unwrap()
        .remove(0);
    files.seal().unwrap();
    let _ = files.take_lane(&identity).unwrap().unwrap();
    assert!(!files.all_terminal());
    files.mark_failed(&identity).unwrap();
    assert_eq!(
        fs::read(files.lane_path(&task_lane).unwrap()).unwrap()[0],
        b'F'
    );
    assert!(files.all_terminal());
}

#[test]
fn finite_prefetch_uses_twice_the_lane_limit_and_waits_for_seal() {
    let root = runtime_root();
    let id = run_id();
    let task_lane = lane(&[7], LocalDiskKind::Hdd, 2);
    let mut files = TransientTaskFileSet::create(root.path(), &id).unwrap();
    let rows = (0..8)
        .map(|index| {
            record(
                &format!("file-{index}.jpg"),
                TaskWorkMask::from_bits(1 << 3).unwrap(),
            )
        })
        .collect::<Vec<_>>();
    files.append_batch(&task_lane, &rows).unwrap();
    assert!(files.peek_lane(&task_lane).unwrap().is_some());
    assert_eq!(files.prefetched_len(&task_lane).unwrap(), 4);
    assert!(!files.all_terminal());
    files.seal().unwrap();
    for _ in 0..rows.len() {
        let identity = files.head_identity(&task_lane).unwrap().unwrap();
        let taken_identity = files.take_lane(&identity).unwrap().unwrap().0;
        files.mark_completed(&taken_identity).unwrap();
    }
    assert!(files.all_terminal());
}

#[test]
fn composite_lane_and_unknown_kind_filename_only_use_disk_identity() {
    let root = runtime_root();
    let id = run_id();
    let task_lane = lane(&[12, 5, 12], LocalDiskKind::Unknown, 1);
    let mut files = TransientTaskFileSet::create(root.path(), &id).unwrap();
    files
        .append_batch(
            &task_lane,
            &[record(
                "stripe.mkv",
                TaskWorkMask::from_bits(1 << 3).unwrap(),
            )],
        )
        .unwrap();
    assert!(
        files
            .lane_path(&task_lane)
            .unwrap()
            .ends_with("PhysicalDisk5+12-unknown.tasks.tsv")
    );
}

#[test]
fn stage2_masks_and_known_md5_use_fixed_lowercase_columns() {
    let root = runtime_root();
    let id = run_id();
    let task_lane = lane(&[7], LocalDiskKind::Ssd, 1);
    let mut files = TransientTaskFileSet::create(root.path(), &id).unwrap();
    let image = TaskFileRecord {
        item_id: Uuid::now_v7(),
        work_kind: TaskWorkKind::ImageStage2,
        scanned: scanned("image.jpg"),
        known_md5: Some([0xab; 16]),
        missing: TaskWorkMask::for_image_stage2(),
    };
    let video = TaskFileRecord {
        item_id: Uuid::now_v7(),
        work_kind: TaskWorkKind::VideoStage2,
        scanned: scanned("video.mkv"),
        known_md5: Some([0x01; 16]),
        missing: TaskWorkMask::for_video_stage2(0b10_0001).unwrap(),
    };
    files.append_batch(&task_lane, &[image, video]).unwrap();
    let text = String::from_utf8(fs::read(files.lane_path(&task_lane).unwrap()).unwrap()).unwrap();
    let lines = text.lines().collect::<Vec<_>>();
    assert_eq!(lines.len(), 2);
    let image_fields = lines[0].split('\t').collect::<Vec<_>>();
    assert_eq!(image_fields[2], "image_stage2");
    assert_eq!(image_fields[6], "abababababababababababababababab");
    assert_eq!(image_fields[7], "0000000000000010");
    let video_fields = lines[1].split('\t').collect::<Vec<_>>();
    assert_eq!(video_fields[2], "video_stage2");
    assert_eq!(video_fields[6], "01010101010101010101010101010101");
    assert_eq!(video_fields[7], "0000000000000420");
}

#[test]
fn rejects_invalid_work_kind_combinations_and_noncanonical_run_reuse() {
    let root = runtime_root();
    let id = run_id();
    let task_lane = lane(&[7], LocalDiskKind::Hdd, 1);
    assert!(TransientTaskFileSet::create(root.path(), id.to_uppercase()).is_err());
    let mut files = TransientTaskFileSet::create(root.path(), &id).unwrap();

    let mut no_md5 = record("no-md5.bin", TaskWorkMask::from_bits(1).unwrap());
    no_md5.known_md5 = None;
    assert!(files.append_batch(&task_lane, &[no_md5]).is_err());

    let mut image_without_md5 = TaskFileRecord {
        item_id: Uuid::now_v7(),
        work_kind: TaskWorkKind::ImageStage2,
        scanned: scanned("image.jpg"),
        known_md5: None,
        missing: TaskWorkMask::for_image_stage2(),
    };
    assert!(
        files
            .append_batch(&task_lane, &[image_without_md5.clone()])
            .is_err()
    );
    image_without_md5.known_md5 = Some([0; 16]);
    image_without_md5.missing = TaskWorkMask::for_video_stage2(1).unwrap();
    assert!(
        files
            .append_batch(&task_lane, &[image_without_md5])
            .is_err()
    );

    let mut video = TaskFileRecord {
        item_id: Uuid::now_v7(),
        work_kind: TaskWorkKind::VideoStage2,
        scanned: scanned("video.mkv"),
        known_md5: Some([0; 16]),
        missing: TaskWorkMask::for_video_stage2(1).unwrap(),
    };
    video.missing = TaskWorkMask::empty();
    assert!(files.append_batch(&task_lane, &[video]).is_err());
    assert!(TaskWorkMask::from_bits(1 << 63).is_none());
    assert!(TransientTaskFileSet::create(root.path(), &id).is_err());
}

#[cfg(windows)]
#[test]
fn rejects_non_utf8_display_path_without_lossy_conversion() {
    use std::os::windows::ffi::OsStringExt;

    let root = runtime_root();
    let id = run_id();
    let task_lane = lane(&[7], LocalDiskKind::Hdd, 1);
    let mut files = TransientTaskFileSet::create(root.path(), &id).unwrap();
    let mut wide = "C:\\media\\invalid-".encode_utf16().collect::<Vec<_>>();
    wide.push(0xD800);
    let invalid_display =
        DisplayPath::new(PathBuf::from(std::ffi::OsString::from_wide(&wide))).unwrap();
    let mut row = record("invalid.bin", TaskWorkMask::from_bits(1 << 3).unwrap());
    row.scanned.display_path = invalid_display;
    assert!(files.append_batch(&task_lane, &[row]).is_err());
    assert!(!files.lane_path(&task_lane).unwrap().exists());
}

#[test]
fn identity_errors_and_corruption_never_change_a_pending_row() {
    let root = runtime_root();
    let id = run_id();
    let task_lane = lane(&[7], LocalDiskKind::Hdd, 1);
    let other_lane = lane(&[8], LocalDiskKind::Ssd, 1);
    let mut files = TransientTaskFileSet::create(root.path(), &id).unwrap();
    let row = record("one.jpg", TaskWorkMask::from_bits(1 << 3).unwrap());
    let identity = files
        .append_batch(&task_lane, std::slice::from_ref(&row))
        .unwrap()
        .remove(0);
    files.seal().unwrap();

    let wrong_run = TaskFileIdentity::new(
        run_id(),
        &task_lane,
        identity.item_id(),
        identity.line_offset(),
        identity.line_length(),
        identity.missing(),
    )
    .unwrap();
    assert!(files.mark_completed(&wrong_run).is_err());
    let wrong_lane = TaskFileIdentity::new(
        id.clone(),
        &other_lane,
        identity.item_id(),
        identity.line_offset(),
        identity.line_length(),
        identity.missing(),
    )
    .unwrap();
    assert!(files.mark_completed(&wrong_lane).is_err());
    let wrong_offset = TaskFileIdentity::new(
        id.clone(),
        &task_lane,
        identity.item_id(),
        identity.line_offset() + 1,
        identity.line_length(),
        identity.missing(),
    )
    .unwrap();
    assert!(files.mark_completed(&wrong_offset).is_err());
    let wrong_mask = TaskFileIdentity::new(
        id,
        &task_lane,
        identity.item_id(),
        identity.line_offset(),
        identity.line_length(),
        TaskWorkMask::for_image_stage2(),
    )
    .unwrap();
    assert!(files.mark_completed(&wrong_mask).is_err());

    let _ = files.take_lane(&identity).unwrap().unwrap();
    let path = files.lane_path(&task_lane).unwrap();
    #[cfg(windows)]
    let damaged = std::fs::OpenOptions::new().write(true).open(&path).unwrap();
    #[cfg(not(windows))]
    let mut damaged = std::fs::OpenOptions::new().write(true).open(&path).unwrap();
    #[cfg(windows)]
    {
        use std::os::windows::fs::FileExt;
        damaged
            .seek_write(b"X", identity.line_offset() + 2)
            .unwrap();
    }
    #[cfg(not(windows))]
    {
        use std::io::{Seek, SeekFrom, Write};
        damaged
            .seek(SeekFrom::Start(identity.line_offset() + 2))
            .unwrap();
        damaged.write_all(b"X").unwrap();
    }
    assert!(files.mark_completed(&identity).is_err());
    assert!(!files.all_terminal());
}

#[test]
fn terminal_transitions_reject_opposite_status_and_published_overflow() {
    let root = runtime_root();
    let id = run_id();
    let task_lane = lane(&[7], LocalDiskKind::Hdd, 1);
    let mut files = TransientTaskFileSet::create(root.path(), &id).unwrap();
    let first = record("first.jpg", TaskWorkMask::from_bits(1 << 3).unwrap());
    let second = record("second.jpg", TaskWorkMask::from_bits(1 << 3).unwrap());
    let identities = files.append_batch(&task_lane, &[first, second]).unwrap();
    files.seal().unwrap();
    let too_far = TaskFileIdentity::new(
        id.clone(),
        &task_lane,
        identities[0].item_id(),
        u64::MAX - 1,
        10,
        identities[0].missing(),
    )
    .unwrap();
    assert!(files.mark_completed(&too_far).is_err());

    let first_identity = identities[0].clone();
    files.take_lane(&first_identity).unwrap().unwrap();
    files.mark_completed(&first_identity).unwrap();
    assert!(files.mark_failed(&first_identity).is_err());
    let second_identity = identities[1].clone();
    files.take_lane(&second_identity).unwrap().unwrap();
    files.mark_failed(&second_identity).unwrap();
    assert!(files.mark_completed(&second_identity).is_err());
    assert!(files.all_terminal());
}
