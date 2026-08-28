//! 启动边界只丢弃运行态，并保留媒体与同步长期事实的行为测试。

use dedup_core::MachineId;
use dedup_node_store::{NewTaskItem, NodeStore, StoreError};
use rusqlite::{Connection, params};

/// 返回固定机器身份，保证重开时通过同一数据库绑定校验。
fn machine() -> MachineId {
    MachineId::from_sha256([0x81; 32])
}

/// 读取长期事实全部字段的稳定快照，确认启动清理不会改写任何长期数据。
fn stable_snapshot(connection: &Connection) -> Vec<String> {
    let projections = [
        (
            "contents",
            "content_id || '|' || hex(md5) || '|' || file_size || '|' || media_kind || '|' || base_complete",
        ),
        (
            "files",
            "machine_id || '|' || normalized_path || '|' || display_path || '|' || file_size || '|' || content_id || '|' || active",
        ),
        (
            "image_stage1",
            "content_id || '|' || width || '|' || height || '|' || hex(pdq) || '|' || quality",
        ),
        (
            "image_stage2",
            "content_id || '|' || hex(phash_parts) || '|' || hex(sobel)",
        ),
        (
            "video_metadata",
            "content_id || '|' || duration_ms || '|' || width || '|' || height",
        ),
        (
            "video_frame_stage1",
            "content_id || '|' || slot || '|' || time_ms || '|' || decoded || '|' || width || '|' || height || '|' || hex(pdq) || '|' || quality",
        ),
        (
            "video_frame_stage2",
            "content_id || '|' || slot || '|' || hex(phash_parts) || '|' || hex(sobel)",
        ),
        ("contact_sheets", "content_id || '|' || relative_path"),
        (
            "file_faults",
            "machine_id || '|' || normalized_path || '|' || display_path || '|' || file_size || '|' || fault_kind || '|' || stage || '|' || COALESCE(CAST(windows_error_code AS TEXT),'NULL') || '|' || COALESCE(CAST(read_offset AS TEXT),'NULL') || '|' || COALESCE(CAST(read_size AS TEXT),'NULL') || '|' || COALESCE(CAST(worker_pid AS TEXT),'NULL') || '|' || COALESCE(CAST(worker_exit_code AS TEXT),'NULL') || '|' || first_seen_at_ms || '|' || last_seen_at_ms || '|' || occurrence_count || '|' || message",
        ),
        (
            "sync_outbox",
            "seq || '|' || entity_kind || '|' || hex(payload)",
        ),
        (
            "sync_state",
            "singleton || '|' || acked_seq || '|' || pruned_through_seq",
        ),
    ];
    projections
        .into_iter()
        .flat_map(|(table, projection)| {
            let sql = format!("SELECT {projection} FROM {table} ORDER BY rowid");
            connection
                .prepare(&sql)
                .unwrap()
                .query_map([], |row| row.get::<_, String>(0))
                .unwrap()
                .map(|row| format!("{table}:{}", row.unwrap()))
                .collect::<Vec<_>>()
        })
        .collect()
}

/// 读取所有运行态表的剩余行数，运行态不应跨进程启动边界存活。
fn transient_snapshot(connection: &Connection) -> Vec<String> {
    let tables = [
        "tasks",
        "task_items",
        "task_scan_roots",
        "task_stages",
        "analysis_runs",
        "analysis_run_stages",
        "analysis_run_inputs",
        "candidate_pairs",
        "duplicate_groups",
        "group_members",
        "review_marks",
        "delete_batches",
        "delete_items",
        "deletion_tombstones",
    ];
    tables
        .into_iter()
        .map(|table| {
            let count: i64 = connection
                .query_row(&format!("SELECT COUNT(*) FROM {table}"), [], |row| {
                    row.get(0)
                })
                .unwrap();
            format!("{table}:{count}")
        })
        .collect()
}

/// 旧任务、分析、复核和删除记录会在重新打开时清空，长期媒体和同步事实保持不变。
#[test]
fn reopening_discards_transient_runtime_rows_and_preserves_durable_facts() {
    let directory = tempfile::tempdir().unwrap();
    let database = directory.path().join("runtime-boundary.db");
    let machine = machine();
    let store = NodeStore::open(&database, machine.clone()).unwrap();
    assert_eq!(store.library_revision().unwrap(), 0);
    drop(store);

    let connection = Connection::open(&database).unwrap();
    let machine_id = machine.as_str();
    connection
        .execute_batch("PRAGMA foreign_keys = ON;")
        .unwrap();
    connection
        .execute(
            "INSERT INTO contents(content_id,md5,file_size,media_kind,base_complete)
             VALUES(1,?1,64,'image',1)",
            [[0x11_u8; 16].as_slice()],
        )
        .unwrap();
    connection
        .execute(
            "INSERT INTO files(machine_id,normalized_path,display_path,file_size,content_id,active)
             VALUES(?1,'D:\\Media\\stable.jpg','D:\\Media\\stable.jpg',64,1,1)",
            [machine_id],
        )
        .unwrap();
    connection
        .execute(
            "INSERT INTO image_stage1(content_id,width,height,pdq,quality)
             VALUES(1,8,8,?1,80)",
            [[0x12_u8; 32].as_slice()],
        )
        .unwrap();
    connection
        .execute(
            "INSERT INTO image_stage2(content_id,phash_parts,sobel) VALUES(1,?1,?2)",
            params![[0x13_u8; 72].as_slice(), [0x14_u8; 512].as_slice()],
        )
        .unwrap();
    connection
        .execute(
            "INSERT INTO video_metadata(content_id,duration_ms,width,height) VALUES(1,1000,8,8)",
            [],
        )
        .unwrap();
    connection
        .execute(
            "INSERT INTO video_frame_stage1(content_id,slot,time_ms,decoded,width,height,pdq,quality)
             VALUES(1,0,500,1,8,8,?1,80)",
            [[0x15_u8; 32].as_slice()],
        )
        .unwrap();
    connection
        .execute(
            "INSERT INTO video_frame_stage2(content_id,slot,phash_parts,sobel) VALUES(1,0,?1,?2)",
            params![[0x16_u8; 72].as_slice(), [0x17_u8; 512].as_slice()],
        )
        .unwrap();
    connection
        .execute(
            "INSERT INTO contact_sheets(content_id,relative_path) VALUES(1,'sheets/11.jpg')",
            [],
        )
        .unwrap();
    connection
        .execute(
            "INSERT INTO file_faults(machine_id,normalized_path,display_path,file_size,fault_kind,stage,
             first_seen_at_ms,last_seen_at_ms,occurrence_count,message)
             VALUES(?1,'D:\\Media\\stable.jpg','D:\\Media\\stable.jpg',64,'worker_crash','base',1,1,1,'old fault')",
            [machine_id],
        )
        .unwrap();
    connection
        .execute(
            "INSERT INTO sync_outbox(entity_kind,payload) VALUES('content',X'AA')",
            [],
        )
        .unwrap();
    connection
        .execute(
            "UPDATE sync_state SET acked_seq=3,pruned_through_seq=2 WHERE singleton=1",
            [],
        )
        .unwrap();
    connection
        .execute(
            "INSERT INTO deletion_tombstones(machine_id,normalized_path,md5,file_size,outcome)
             VALUES(?1,'D:\\Media\\gone.jpg',?2,64,'deleted')",
            params![machine_id, [0x18_u8; 16].as_slice()],
        )
        .unwrap();

    connection
        .execute(
            "INSERT INTO tasks(task_id,kind,status,total_items,created_at_ms,updated_at_ms)
         VALUES('task-old','scan','running',1,1,1)",
            [],
        )
        .unwrap();
    connection.execute(
        "INSERT INTO task_items(item_id,task_id,status) VALUES('item-old','task-old','running')",
        [],
    ).unwrap();
    connection
        .execute(
            "INSERT INTO task_scan_roots(task_id,normalized_root) VALUES('task-old','D:\\Media')",
            [],
        )
        .unwrap();
    connection
        .execute(
            "INSERT INTO task_stages(task_id,stage_id,state,completed,failed,skipped)
         VALUES('task-old','base','running',0,0,0)",
            [],
        )
        .unwrap();
    connection.execute(
        "INSERT INTO analysis_runs(analysis_run_id,mode,status,thresholds_toml,created_at_ms,updated_at_ms)
         VALUES('run-old','local','screening','thresholds',1,1)",
        [],
    ).unwrap();
    connection.execute(
        "INSERT INTO analysis_run_stages(analysis_run_id,stage_id,state,completed,failed,skipped)
         VALUES('run-old','screen','running',0,0,0)",
        [],
    ).unwrap();
    connection.execute(
        "INSERT INTO analysis_run_inputs(analysis_run_id,md5,file_size,machine_id,normalized_path)
         VALUES('run-old',?1,64,?2,'D:\\Media\\stable.jpg')",
        params![[0x11_u8; 16].as_slice(), machine_id],
    ).unwrap();
    connection.execute(
        "INSERT INTO candidate_pairs(analysis_run_id,pair_kind,left_md5,left_size,right_md5,right_size,
         stage1_score,status) VALUES('run-old','image',?1,64,?2,65,0.9,'stage1_passed')",
        params![[0x11_u8; 16].as_slice(), [0x19_u8; 16].as_slice()],
    ).unwrap();
    connection.execute(
        "INSERT INTO duplicate_groups(analysis_run_id,group_id,group_kind,representative_md5,representative_size)
         VALUES('run-old','group-old','image',?1,64)",
        [[0x11_u8; 16].as_slice()],
    ).unwrap();
    for path in ["D:\\Media\\stable.jpg", "D:\\Media\\other.jpg"] {
        connection.execute(
            "INSERT INTO group_members(analysis_run_id,group_id,machine_id,normalized_path,md5,file_size,
             representative,stage1_score,active)
             VALUES('run-old','group-old',?1,?2,?3,64,0,0.9,1)",
            params![machine_id, path, [0x11_u8; 16].as_slice()],
        ).unwrap();
    }
    connection
        .execute(
            "INSERT INTO review_marks(analysis_run_id,group_id,machine_id,normalized_path,decision)
         VALUES('run-old','group-old',?1,'D:\\Media\\stable.jpg','delete')",
            [machine_id],
        )
        .unwrap();
    connection
        .execute(
            "INSERT INTO delete_batches(delete_batch_id,analysis_run_id,mode,status,created_at_ms)
         VALUES('batch-old','run-old','permanent','running',1)",
            [],
        )
        .unwrap();
    connection.execute(
        "INSERT INTO delete_items(delete_item_id,delete_batch_id,group_id,machine_id,normalized_path,
         expected_md5,expected_size,status) VALUES('delete-old','batch-old','group-old',?1,
         'D:\\Media\\stable.jpg',?2,64,'running')",
        params![machine_id, [0x11_u8; 16].as_slice()],
    ).unwrap();
    let before = stable_snapshot(&connection);
    drop(connection);

    drop(NodeStore::open(&database, machine).unwrap());

    let connection = Connection::open(&database).unwrap();
    assert_eq!(
        transient_snapshot(&connection),
        vec![
            "tasks:0",
            "task_items:0",
            "task_scan_roots:0",
            "task_stages:0",
            "analysis_runs:0",
            "analysis_run_stages:0",
            "analysis_run_inputs:0",
            "candidate_pairs:0",
            "duplicate_groups:0",
            "group_members:0",
            "review_marks:0",
            "delete_batches:0",
            "delete_items:0",
            "deletion_tombstones:0",
        ]
    );
    assert_eq!(stable_snapshot(&connection), before);
}

/// 运行期 WAL 观察连接不是启动边界，不得清空当前 actor 正在管理的任务。
#[test]
fn background_reopen_keeps_current_runtime_rows() {
    let directory = tempfile::tempdir().unwrap();
    let database = directory.path().join("background-observer.db");
    let mut store = NodeStore::open(&database, machine()).unwrap();
    let task = store
        .create_task("base_compute", &[NewTaskItem::detached("file")], 1)
        .unwrap();

    let observer = store.reopen().unwrap();

    assert_eq!(store.task_snapshot(task).unwrap().total_items, 1);
    assert_eq!(observer.task_snapshot(task).unwrap().total_items, 1);
}

/// 读取文件数据库中已经持久化的 library revision 原始字段。
fn raw_library_revision(database: &std::path::Path) -> String {
    let connection = Connection::open(database).unwrap();
    connection
        .query_row(
            "SELECT value FROM metadata WHERE key='library_revision'",
            [],
            |row| row.get(0),
        )
        .unwrap()
}

/// 新库初始化 revision，合法旧库补齐缺失 key，并拒绝非法 revision 元数据。
#[test]
fn opening_validates_and_initializes_library_revision_metadata() {
    let directory = tempfile::tempdir().unwrap();
    let database = directory.path().join("library-revision.db");
    let machine = machine();

    let store = NodeStore::open(&database, machine.clone()).unwrap();
    assert_eq!(store.library_revision().unwrap(), 0);
    drop(store);
    assert_eq!(raw_library_revision(&database), "0");

    let connection = Connection::open(&database).unwrap();
    connection
        .execute("DELETE FROM metadata WHERE key='library_revision'", [])
        .unwrap();
    drop(connection);
    let store = NodeStore::open(&database, machine.clone()).unwrap();
    assert_eq!(store.library_revision().unwrap(), 0);
    drop(store);
    assert_eq!(raw_library_revision(&database), "0");

    for invalid in ["", "-1", "1.5", "18446744073709551616"] {
        let connection = Connection::open(&database).unwrap();
        connection
            .execute(
                "UPDATE metadata SET value=?1 WHERE key='library_revision'",
                [invalid],
            )
            .unwrap();
        drop(connection);
        assert!(matches!(
            NodeStore::open(&database, machine.clone()),
            Err(StoreError::IncompatibleSchema)
        ));
    }
}
