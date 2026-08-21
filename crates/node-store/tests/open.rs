//! SQLite schema 版本拒绝边界；旧库必须保持原样。

use std::fs;

use dedup_core::{MachineId, product_id};
use dedup_node_store::{NodeStore, StoreError};
use rusqlite::Connection;

fn machine() -> MachineId {
    MachineId::parse("7373737373737373737373737373737373737373737373737373737373737373")
        .unwrap()
}

#[test]
fn rejects_schema_v1_without_changing_it_and_creates_new_databases_as_v2() {
    let directory = tempfile::tempdir().unwrap();
    let legacy_path = directory.path().join("schema-v1.sqlite3");
    let legacy = Connection::open(&legacy_path).unwrap();
    legacy
        .execute_batch(
            "PRAGMA user_version=1;
             CREATE TABLE metadata(key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT;
             CREATE TABLE sentinel(value TEXT NOT NULL) STRICT;
             INSERT INTO sentinel(value) VALUES('must-stay');",
        )
        .unwrap();
    legacy
        .execute(
            "INSERT INTO metadata(key,value) VALUES('schema_id',?1),('machine_id',?2)",
            (product_id(), machine().as_str()),
        )
        .unwrap();
    drop(legacy);
    let before = fs::read(&legacy_path).unwrap();

    assert!(matches!(
        NodeStore::open(&legacy_path, machine()),
        Err(StoreError::IncompatibleSchema)
    ));
    assert_eq!(fs::read(&legacy_path).unwrap(), before);

    let unchanged = Connection::open(&legacy_path).unwrap();
    assert_eq!(
        unchanged
            .query_row("PRAGMA user_version", [], |row| row.get::<_, i64>(0))
            .unwrap(),
        1
    );
    assert_eq!(
        unchanged
            .query_row("SELECT value FROM sentinel", [], |row| row.get::<_, String>(0))
            .unwrap(),
        "must-stay"
    );
    assert_eq!(
        unchanged
            .query_row(
                "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='file_faults'",
                [],
                |row| row.get::<_, i64>(0),
            )
            .unwrap(),
        0
    );
    drop(unchanged);

    let current_path = directory.path().join("schema-v2.sqlite3");
    let current_store = NodeStore::open(&current_path, machine()).unwrap();
    assert_eq!(current_store.schema_id().unwrap(), product_id());
    drop(current_store);
    let current = Connection::open(current_path).unwrap();
    assert_eq!(
        current
            .query_row("PRAGMA user_version", [], |row| row.get::<_, i64>(0))
            .unwrap(),
        2
    );
}
