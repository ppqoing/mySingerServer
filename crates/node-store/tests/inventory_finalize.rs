//! 扫描清单收尾的 SQLite 事务行为测试。

use std::path::Path;

use dedup_core::{ContentKey, DisplayPath, LocationKey, MachineId, MediaKind, NormalizedPath};
use dedup_node_store::{NodeStore, ResolvedScanFile, ScanFinalizeInput, ScannedPath, StoreError};
use rusqlite::Connection;
use tempfile::TempDir;

fn machine() -> MachineId {
    MachineId::parse("73bdb7a3377f81376a84f316b3ee1555e345afbfa87aa99c77b1bfcc364c4cae").unwrap()
}

fn scanned(path: &str, file_size: u64) -> ScannedPath {
    ScannedPath::new(
        NormalizedPath::new(path).unwrap(),
        DisplayPath::new(path).unwrap(),
        file_size,
    )
}

fn location(path: &str) -> LocationKey {
    LocationKey::new(machine(), NormalizedPath::new(path).unwrap())
}

fn key(byte: u8, file_size: u64) -> ContentKey {
    ContentKey::new([byte; 16], file_size)
}

fn finalize_input(
    roots: &[&str],
    seen_paths: &[&str],
    resolved: &[(&str, u8, u64)],
) -> ScanFinalizeInput {
    ScanFinalizeInput {
        roots: roots
            .iter()
            .map(|path| NormalizedPath::new(path).unwrap())
            .collect(),
        seen_paths: seen_paths
            .iter()
            .map(|path| NormalizedPath::new(path).unwrap())
            .collect(),
        resolved_files: resolved
            .iter()
            .map(|(path, md5_byte, file_size)| ResolvedScanFile {
                scanned: scanned(path, *file_size),
                content: key(*md5_byte, *file_size),
            })
            .collect(),
    }
}

fn seed_complete_content(
    store: &mut NodeStore,
    path: &str,
    md5_byte: u8,
    file_size: u64,
) -> ContentKey {
    let scanned = scanned(path, file_size);
    let content = store
        .upsert_content_and_location(&scanned, [md5_byte; 16], MediaKind::Other)
        .unwrap();
    store.mark_base_complete(content.id).unwrap();
    content.key
}

/// 本轮已经见到但读取失败的旧活动路径仍应保持活动，不能因未解析而误失活。
#[test]
fn seen_but_failed_path_is_not_falsely_deactivated() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    seed_complete_content(&mut store, r"D:\A\broken.mp4", 1, 10);

    let result = store
        .finalize_scan_manifest(&finalize_input(&[r"D:\A"], &[r"D:\A\broken.mp4"], &[]), 10)
        .unwrap();

    assert!(
        store
            .active_file(&location(r"D:\A\broken.mp4"))
            .unwrap()
            .is_some()
    );
    assert_eq!(result.library_revision, 1);
    assert_eq!(result.outbox_high_seq, store.outbox_high_seq().unwrap());
}

/// 完整解析项应在收尾事务中写入活动位置，并为同步产生 file outbox。
#[test]
fn resolved_file_writes_location_and_returns_committed_highwater() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let content = seed_complete_content(&mut store, r"D:\seed\same.bin", 2, 20);
    let before = store.outbox_high_seq().unwrap();

    let result = store
        .finalize_scan_manifest(
            &finalize_input(
                &[r"D:\scan"],
                &[r"D:\scan\same.bin"],
                &[(r"D:\scan\same.bin", 2, 20)],
            ),
            20,
        )
        .unwrap();

    assert!(
        store
            .active_file(&location(r"D:\scan\same.bin"))
            .unwrap()
            .is_some()
    );
    assert_eq!(result.library_revision, 1);
    assert!(result.outbox_high_seq > before);
    assert_eq!(result.outbox_high_seq, store.outbox_high_seq().unwrap());
    let changes = store.pull_changes(before, 100).unwrap();
    assert!(
        changes
            .changes
            .iter()
            .any(|change| change.entity_kind == "file")
    );
    assert_eq!(
        store.content_id_by_key(content).unwrap(),
        store.content_id_by_key(key(2, 20)).unwrap()
    );
}

/// 已经完全相同的活动关系重复收尾时不应重复制造 file outbox。
#[test]
fn unchanged_resolved_location_does_not_emit_duplicate_file_outbox() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    seed_complete_content(&mut store, r"D:\A\same.bin", 10, 100);
    let before = store.outbox_high_seq().unwrap();

    let result = store
        .finalize_scan_manifest(
            &finalize_input(
                &[r"D:\A"],
                &[r"D:\A\same.bin"],
                &[(r"D:\A\same.bin", 10, 100)],
            ),
            25,
        )
        .unwrap();

    assert_eq!(result.outbox_high_seq, before);
    assert_eq!(result.library_revision, 1);
}

/// 根目录按路径组件匹配；D:\A 收尾不能误伤相邻的 D:\AB 或 D:\A2。
#[test]
fn stale_locations_are_deactivated_only_inside_component_root() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    seed_complete_content(&mut store, r"D:\A\old.jpg", 3, 30);
    seed_complete_content(&mut store, r"D:\AB\keep.jpg", 4, 40);
    seed_complete_content(&mut store, r"D:\A2\outside.jpg", 5, 50);

    store
        .finalize_scan_manifest(&finalize_input(&[r"D:\A"], &[], &[]), 30)
        .unwrap();

    assert!(
        !store
            .is_location_active(&NormalizedPath::new(r"D:\A\old.jpg").unwrap())
            .unwrap()
    );
    assert!(
        store
            .is_location_active(&NormalizedPath::new(r"D:\AB\keep.jpg").unwrap())
            .unwrap()
    );
    assert!(
        store
            .is_location_active(&NormalizedPath::new(r"D:\A2\outside.jpg").unwrap())
            .unwrap()
    );
}

/// 超过一千条的见到清单必须完整进入同一收尾事务，末尾路径不能被误失活。
#[test]
fn more_than_one_thousand_seen_paths_are_preserved_at_batch_boundary() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    seed_complete_content(&mut store, r"D:\A\zz-keep.bin", 11, 110);
    seed_complete_content(&mut store, r"D:\A\stale.bin", 12, 120);

    let mut seen_paths = (0..1_000)
        .map(|index| NormalizedPath::new(format!(r"D:\A\seen-{index:04}.bin")).unwrap())
        .collect::<Vec<_>>();
    seen_paths.push(NormalizedPath::new(r"D:\A\zz-keep.bin").unwrap());
    store
        .finalize_scan_manifest(
            &ScanFinalizeInput {
                roots: vec![NormalizedPath::new(r"D:\A").unwrap()],
                seen_paths,
                resolved_files: Vec::new(),
            },
            35,
        )
        .unwrap();

    assert!(
        store
            .is_location_active(&NormalizedPath::new(r"D:\A\zz-keep.bin").unwrap())
            .unwrap()
    );
    assert!(
        !store
            .is_location_active(&NormalizedPath::new(r"D:\A\stale.bin").unwrap())
            .unwrap()
    );
}

/// 重复根和相同解析项应只保留一份；同一路径指向不同内容必须拒绝。
#[test]
fn duplicate_inputs_are_deduplicated_but_conflicting_content_is_rejected() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    seed_complete_content(&mut store, r"D:\seed\same.bin", 6, 60);

    store
        .finalize_scan_manifest(
            &finalize_input(
                &[r"D:\A", r"d:\a"],
                &[r"D:\A\same.bin", r"d:\a\same.bin"],
                &[(r"D:\A\same.bin", 6, 60), (r"d:\a\same.bin", 6, 60)],
            ),
            40,
        )
        .unwrap();
    assert!(
        store
            .is_location_active(&NormalizedPath::new(r"D:\A\same.bin").unwrap())
            .unwrap()
    );

    let before_revision = store.library_revision().unwrap();
    let error = store
        .finalize_scan_manifest(
            &finalize_input(
                &[r"D:\A"],
                &[r"D:\A\same.bin"],
                &[(r"D:\A\same.bin", 6, 60), (r"d:\a\same.bin", 7, 60)],
            ),
            50,
        )
        .expect_err("同一路径的不同内容必须拒绝");
    assert!(matches!(error, StoreError::InvalidState(_)));
    assert_eq!(store.library_revision().unwrap(), before_revision);
}

/// 缺失内容键不能伪造解析关系，也不能推进 revision。
#[test]
fn unresolved_content_key_is_rejected_without_business_changes() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let before_revision = store.library_revision().unwrap();
    let before_highwater = store.outbox_high_seq().unwrap();

    let error = store
        .finalize_scan_manifest(
            &finalize_input(
                &[r"D:\A"],
                &[r"D:\A\missing.bin"],
                &[(r"D:\A\missing.bin", 8, 80)],
            ),
            60,
        )
        .expect_err("不存在的完整内容不能写入位置");

    assert!(matches!(error, StoreError::InvalidState(_)));
    assert_eq!(store.library_revision().unwrap(), before_revision);
    assert_eq!(store.outbox_high_seq().unwrap(), before_highwater);
    assert!(
        store
            .active_file(&location(r"D:\A\missing.bin"))
            .unwrap()
            .is_none()
    );
}

/// 收尾事务中途失败时，关系、失活 outbox 与 library_revision 必须整体回滚。
#[test]
fn transaction_failure_rolls_back_location_outbox_and_revision() {
    let directory = TempDir::new().unwrap();
    let database = directory.path().join("node.db");
    let mut store = NodeStore::open(&database, machine()).unwrap();
    seed_complete_content(&mut store, r"D:\A\old.bin", 9, 90);
    let before_revision = store.library_revision().unwrap();
    let before_highwater = store.outbox_high_seq().unwrap();

    install_file_outbox_failure(&database);
    let error = store
        .finalize_scan_manifest(
            &finalize_input(&[r"D:\A"], &[r"D:\A\new.bin"], &[(r"D:\A\new.bin", 9, 90)]),
            70,
        )
        .expect_err("触发器应让正式收尾事务失败");

    assert!(matches!(error, StoreError::Sqlite(_)));
    assert_eq!(store.library_revision().unwrap(), before_revision);
    assert_eq!(store.outbox_high_seq().unwrap(), before_highwater);
    assert!(
        store
            .active_file(&location(r"D:\A\old.bin"))
            .unwrap()
            .is_some()
    );
    assert!(
        store
            .active_file(&location(r"D:\A\new.bin"))
            .unwrap()
            .is_none()
    );
}

fn install_file_outbox_failure(database: &Path) {
    Connection::open(database)
        .unwrap()
        .execute_batch(
            "CREATE TRIGGER fail_inventory_file_outbox
             BEFORE INSERT ON sync_outbox
             WHEN NEW.entity_kind='file'
             BEGIN
                 SELECT RAISE(ABORT, 'injected inventory outbox failure');
             END;",
        )
        .unwrap();
}
