//! 瞬态删除提交的 SQLite 当前事实测试。

use std::path::Path;

use dedup_core::{ContentKey, DisplayPath, LocationKey, MachineId, MediaKind, NormalizedPath};
use dedup_node_store::{NodeStore, ScannedPath, VerifiedDeletedFile};
use rusqlite::Connection;
use tempfile::TempDir;

fn machine() -> MachineId {
    MachineId::from_sha256([0x73; 32])
}

fn location(path: &str) -> LocationKey {
    LocationKey::new(machine(), NormalizedPath::new(path).unwrap())
}

fn scanned(path: &str, file_size: u64) -> ScannedPath {
    ScannedPath::new(
        NormalizedPath::new(path).unwrap(),
        DisplayPath::new(path).unwrap(),
        file_size,
    )
}

fn key(byte: u8, file_size: u64) -> ContentKey {
    ContentKey::new([byte; 16], file_size)
}

/// 在真实文件 SQLite 中建立一个活动位置，并返回其内容身份。
fn seed_file(store: &mut NodeStore, path: &str, md5_byte: u8, file_size: u64) -> ContentKey {
    store
        .upsert_content_and_location(&scanned(path, file_size), [md5_byte; 16], MediaKind::Other)
        .unwrap()
        .key
}

/// 读取测试数据库中的固定表行数，确认新 API 没有触碰运行态历史表。
fn table_count(connection: &Connection, table: &str) -> i64 {
    connection
        .query_row(&format!("SELECT COUNT(*) FROM {table}"), [], |row| {
            row.get(0)
        })
        .unwrap()
}

/// 打开独立只读观察连接，避免测试依赖 NodeStore 的私有连接字段。
fn observe(database: &Path) -> Connection {
    Connection::open(database).unwrap()
}

/// 成功删除只提交当前文件事实、file outbox 和一次 library revision。
#[test]
fn successful_deactivation_updates_current_file_without_delete_history() {
    let temp = TempDir::new().unwrap();
    let database = temp.path().join("node.db");
    let mut store = NodeStore::open(&database, machine()).unwrap();
    let content = seed_file(&mut store, r"D:\Delete\one.bin", 7, 70);
    let before_revision = store.library_revision().unwrap();
    let before_outbox = store.outbox_high_seq().unwrap();

    let revision = store
        .deactivate_deleted_files(
            &[VerifiedDeletedFile::new(
                location(r"D:\Delete\one.bin"),
                content,
            )],
            100,
        )
        .unwrap();

    assert_eq!(revision, before_revision + 1);
    assert_eq!(store.library_revision().unwrap(), before_revision + 1);
    assert_eq!(store.outbox_high_seq().unwrap(), before_outbox + 1);

    drop(store);
    let connection = observe(&database);
    let active: i64 = connection
        .query_row("SELECT active FROM files LIMIT 1", [], |row| row.get(0))
        .unwrap();
    assert_eq!(active, 0);
    assert_eq!(table_count(&connection, "review_marks"), 0);
    assert_eq!(table_count(&connection, "delete_batches"), 0);
    assert_eq!(table_count(&connection, "delete_items"), 0);
    assert_eq!(table_count(&connection, "deletion_tombstones"), 0);
    assert_eq!(table_count(&connection, "group_members"), 0);
    let entity_kind: String = connection
        .query_row(
            "SELECT entity_kind FROM sync_outbox ORDER BY seq DESC LIMIT 1",
            [],
            |row| row.get(0),
        )
        .unwrap();
    assert_eq!(entity_kind, "file");
}

/// 任一项身份不匹配时必须整批回滚，之前已经验证的项也不能被失活。
#[test]
fn mismatched_identity_rolls_back_every_deactivation() {
    let temp = TempDir::new().unwrap();
    let database = temp.path().join("node.db");
    let mut store = NodeStore::open(&database, machine()).unwrap();
    let first = seed_file(&mut store, r"D:\Delete\first.bin", 1, 10);
    let _second = seed_file(&mut store, r"D:\Delete\second.bin", 2, 20);
    let before_revision = store.library_revision().unwrap();
    let before_outbox = store.outbox_high_seq().unwrap();

    let result = store.deactivate_deleted_files(
        &[
            VerifiedDeletedFile::new(location(r"D:\Delete\first.bin"), first),
            VerifiedDeletedFile::new(location(r"D:\Delete\second.bin"), key(0xff, 20)),
        ],
        100,
    );

    assert!(result.is_err());
    assert_eq!(store.library_revision().unwrap(), before_revision);
    assert_eq!(store.outbox_high_seq().unwrap(), before_outbox);
    assert!(
        store
            .active_file(&location(r"D:\Delete\first.bin"))
            .unwrap()
            .is_some()
    );
    assert!(
        store
            .active_file(&location(r"D:\Delete\second.bin"))
            .unwrap()
            .is_some()
    );
    drop(store);
    let connection = observe(&database);
    assert_eq!(table_count(&connection, "deletion_tombstones"), 0);
}

/// 空输入是无操作，不产生 revision 或 outbox。
#[test]
fn empty_deactivation_does_not_advance_revision() {
    let temp = TempDir::new().unwrap();
    let database = temp.path().join("node.db");
    let mut store = NodeStore::open(&database, machine()).unwrap();
    let before_revision = store.library_revision().unwrap();
    let before_outbox = store.outbox_high_seq().unwrap();

    let revision = store.deactivate_deleted_files(&[], 100).unwrap();

    assert_eq!(revision, before_revision);
    assert_eq!(store.library_revision().unwrap(), before_revision);
    assert_eq!(store.outbox_high_seq().unwrap(), before_outbox);
}
