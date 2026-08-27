//! 只读结果摘要导出的行为验收；fixture 只写入隔离临时 SQLite 和缓存根。

use std::{
    fs,
    path::{Path, PathBuf},
};

use dedup_core::product_id;
use dedup_node_store::result_summary::{
    ResultSummaryCommitTestHook, ResultSummaryError, ResultSummaryReadTestHook,
    ResultSummaryStatus, export_scan_result_summary, pair_commit_lease_path,
    set_result_summary_commit_test_callback, set_result_summary_commit_test_hook,
    set_result_summary_read_test_callback, set_result_summary_read_test_hook,
    sidecar_paths_for_acceptance, validate_result_summary_pair,
};
use rusqlite::{Connection, OpenFlags, OptionalExtension, params};
use serde_json::Value;
use sha2::{Digest, Sha256};
use tempfile::{TempDir, tempdir};

const TASK_ID: &str = "00000000-0000-0000-0000-000000000001";

/// 一个可重复生成的隔离数据库和缓存根。
struct Fixture {
    root: TempDir,
    database: PathBuf,
    cache_root: PathBuf,
}

impl Fixture {
    /// 创建当前 schema 的空数据库，并把所有后续文件限制在临时根内。
    fn new() -> Self {
        let root = tempdir().expect("临时目录");
        let database = root.path().join("node.db");
        let cache_root = root.path().join("cache");
        fs::create_dir(&cache_root).expect("缓存根");
        let connection = Connection::open(&database).expect("创建 SQLite");
        connection
            .execute_batch(include_str!("../src/schema.sql"))
            .expect("创建 schema");
        connection
            .execute(
                "INSERT INTO metadata(key,value) VALUES('schema_id',?1),('machine_id',?2)",
                params![
                    product_id(),
                    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
                ],
            )
            .expect("写入 schema 元数据");
        Self {
            root,
            database,
            cache_root,
        }
    }

    /// 返回一个不会与实际输出混淆的摘要路径。
    fn output(&self, name: &str) -> PathBuf {
        self.root.path().join(name)
    }

    /// 以固定计数创建任务，便于测试任务状态与项状态矛盾。
    fn task(
        &self,
        task_id: &str,
        status: &str,
        total: u32,
        succeeded: u32,
        failed: u32,
        cancelled: u32,
    ) {
        let connection = Connection::open(&self.database).expect("打开 fixture");
        connection
            .execute(
                "INSERT INTO tasks(task_id,kind,status,total_items,succeeded,failed_items,cancelled,created_at_ms,updated_at_ms)
                 VALUES(?1,'base_compute',?2,?3,?4,?5,?6,1,1)",
                params![task_id, status, total, succeeded, failed, cancelled],
            )
            .expect("写入任务");
    }

    /// 写入内容键；摘要只能从 contents 读取 MD5、大小和媒体类型。
    fn content(&self, content_id: i64, md5_byte: u8, size: u64, media_kind: &str, complete: bool) {
        let connection = Connection::open(&self.database).expect("打开 fixture");
        connection
            .execute(
                "INSERT INTO contents(content_id,md5,file_size,media_kind,base_complete)
                 VALUES(?1,?2,?3,?4,?5)",
                params![
                    content_id,
                    vec![md5_byte; 16],
                    i64::try_from(size).expect("SQLite size"),
                    media_kind,
                    complete
                ],
            )
            .expect("写入内容");
    }

    /// 写入一条任务项，允许刻意构造重复路径和坏状态。
    fn item(
        &self,
        task_id: &str,
        item_id: &str,
        machine_id: &str,
        path: &str,
        status: &str,
        content_id: Option<i64>,
    ) {
        let connection = Connection::open(&self.database).expect("打开 fixture");
        let file_size = content_id
            .and_then(|content_id| {
                connection
                    .query_row(
                        "SELECT file_size FROM contents WHERE content_id=?1",
                        [content_id],
                        |row| row.get::<_, i64>(0),
                    )
                    .optional()
                    .expect("读取 fixture 内容大小")
            })
            .unwrap_or(0);
        connection
            .execute(
                "INSERT INTO task_items(item_id,task_id,machine_id,normalized_path,display_path,file_size,content_id,status,stage,error)
                 VALUES(?1,?2,?3,?4,?5,?6,?7,?8,'base',NULL)",
                params![item_id, task_id, machine_id, path, path, file_size, content_id, status],
            )
            .expect("写入任务项");
    }

    /// 直接修改 fixture 的一个 SQLite 字段，用来证明原始 payload 每个字段都参与哈希。
    fn execute(&self, sql: &str, values: impl rusqlite::Params) {
        let connection = Connection::open(&self.database).expect("打开 fixture");
        connection.execute(sql, values).expect("修改 fixture");
    }

    /// 在隔离 fixture 中允许 SQLite 约束被暂时绕过，构造损坏证据行为。
    fn execute_unchecked(&self, sql: &str, values: impl rusqlite::Params) {
        let connection = Connection::open(&self.database).expect("打开 fixture");
        connection
            .execute_batch("PRAGMA ignore_check_constraints=ON;")
            .expect("打开坏证据 fixture 模式");
        connection.execute(sql, values).expect("修改坏证据 fixture");
    }

    /// 返回 SQLite WAL/SHM sidecar 路径，测试导出过程不能改动它们。
    fn sidecar(&self, suffix: &str) -> PathBuf {
        PathBuf::from(format!("{}-{suffix}", self.database.display()))
    }

    /// 切换 fixture 到 WAL 模式，复现首次只读打开可能初始化 SHM 的数据库类型。
    fn enable_wal(&self) {
        let connection = Connection::open(&self.database).expect("打开 WAL fixture");
        let mode: String = connection
            .query_row("PRAGMA journal_mode=WAL", [], |row| row.get(0))
            .expect("切换 WAL 模式");
        assert_eq!(mode, "wal");
    }

    /// 保留真实 WAL 写入连接，令导出器首次读取自然刷新 WAL-index。
    fn prepare_wal_first_read(&self) -> Connection {
        let connection = Connection::open(&self.database).expect("打开首次读取 fixture");
        connection
            .execute_batch(
                "PRAGMA journal_mode=WAL;
                 PRAGMA wal_autocheckpoint=0;
                 UPDATE metadata SET value='first-read-fixture' WHERE key='machine_id';",
            )
            .expect("写入未 checkpoint 的 WAL");

        let [wal, _shm] = sidecar_paths_for_acceptance(&self.database);
        assert!(wal.exists(), "首次读取 fixture 必须保留 WAL");
        connection
    }

    /// 读取首次导出前后的真实 sidecar 哈希，确认生产读取确实刷新了 WAL-index。
    fn wal_sidecar_hashes(&self) -> [String; 2] {
        let [wal, shm] = sidecar_paths_for_acceptance(&self.database);
        assert!(wal.exists(), "WAL sidecar 必须存在");
        assert!(shm.exists(), "SHM sidecar 必须存在");
        [sha256_file(&wal), sha256_file(&shm)]
    }

    /// 导出摘要并返回结果对象。
    fn export(
        &self,
        task_id: &str,
        name: &str,
    ) -> dedup_node_store::result_summary::ResultSummaryExport {
        export_scan_result_summary(
            &self.database,
            &self.cache_root,
            task_id,
            &self.output(name),
        )
        .expect("导出摘要")
    }
}

/// 读取 canonical JSONL 的原始 UTF-8 字节和解析后的行。
fn read_jsonl(path: &Path) -> (Vec<u8>, Vec<Value>) {
    let bytes = fs::read(path).expect("读取 JSONL");
    assert!(bytes.ends_with(b"\n"), "JSONL 必须包含最终 LF");
    assert!(
        !bytes.windows(2).any(|pair| pair == b"\r\n"),
        "JSONL 不得使用 CRLF"
    );
    let rows = String::from_utf8(bytes.clone())
        .expect("UTF-8")
        .lines()
        .map(|line| serde_json::from_str(line).expect("紧凑 JSON 行"))
        .collect();
    (bytes, rows)
}

/// 计算文件的 SHA-256，验证导出前后 SQLite 内容不变。
fn sha256_file(path: &Path) -> String {
    let mut digest = Sha256::new();
    digest.update(fs::read(path).expect("读取摘要文件"));
    format!("{:x}", digest.finalize())
}

/// 写入完整图片特征，包含 stage1、stage2 和联系表 artifact。
fn seed_complete_image(
    fixture: &Fixture,
    task_id: &str,
    item_id: &str,
    content_id: i64,
    path: &str,
) {
    fixture.content(content_id, 0xAB, 1024, "image", true);
    fixture.task(task_id, "completed", 1, 1, 0, 0);
    fixture.item(
        task_id,
        item_id,
        "machine-a",
        path,
        "succeeded",
        Some(content_id),
    );
    fixture.execute(
        "INSERT INTO image_stage1(content_id,width,height,pdq,quality) VALUES(?1,?2,?3,?4,?5)",
        params![content_id, 1920_i64, 1080_i64, vec![0x11_u8; 32], 97_i64],
    );
    fixture.execute(
        "INSERT INTO image_stage2(content_id,phash_parts,sobel) VALUES(?1,?2,?3)",
        params![content_id, vec![0x22_u8; 72], vec![0x33_u8; 512]],
    );
}

/// 写入完整视频特征的六个固定槽位和联系表。
fn seed_complete_video(
    fixture: &Fixture,
    task_id: &str,
    item_id: &str,
    content_id: i64,
    path: &str,
) {
    fixture.content(content_id, 0xCD, 4096, "video", true);
    fixture.task(task_id, "completed", 1, 1, 0, 0);
    fixture.item(
        task_id,
        item_id,
        "machine-a",
        path,
        "succeeded",
        Some(content_id),
    );
    fixture.execute(
        "INSERT INTO video_metadata(content_id,duration_ms,width,height) VALUES(?1,?2,?3,?4)",
        params![content_id, 12_345_i64, 3840_i64, 2160_i64],
    );
    for slot in 0_i64..6 {
        fixture.execute(
            "INSERT INTO video_frame_stage1(content_id,slot,time_ms,decoded,width,height,pdq,quality)
             VALUES(?1,?2,?3,1,?4,?5,?6,?7)",
            params![content_id, slot, slot * 1000, 3840_i64, 2160_i64, vec![slot as u8; 32], 90_i64],
        );
        fixture.execute(
            "INSERT INTO video_frame_stage2(content_id,slot,phash_parts,sobel) VALUES(?1,?2,?3,?4)",
            params![
                content_id,
                slot,
                vec![slot as u8 + 10; 72],
                vec![slot as u8 + 20; 512]
            ],
        );
    }
}

#[test]
fn exports_sorted_compact_rows_without_local_ids_and_with_stable_hash() {
    let fixture = Fixture::new();
    fixture.content(1, 0xAB, 1024, "image", true);
    fixture.content(2, 0xCD, 2048, "image", true);
    fixture.task(TASK_ID, "completed", 2, 2, 0, 0);
    fixture.item(
        TASK_ID,
        "z-item",
        "machine-b",
        r"C:\\Media\\z.jpg",
        "succeeded",
        Some(1),
    );
    fixture.item(
        TASK_ID,
        "a-item",
        "machine-a",
        r"C:\\Media\\a.jpg",
        "succeeded",
        Some(2),
    );
    fixture.execute(
        "INSERT INTO image_stage1(content_id,width,height,pdq,quality) VALUES(1,800,600,?1,80)",
        params![vec![0x01_u8; 32]],
    );
    fixture.execute(
        "INSERT INTO image_stage1(content_id,width,height,pdq,quality) VALUES(2,640,480,?1,85)",
        params![vec![0xEF_u8; 32]],
    );
    let result = fixture.export(TASK_ID, "result-summary.jsonl");
    assert_eq!(result.status, ResultSummaryStatus::Pass);
    assert_eq!(result.row_count, 2);
    assert_eq!(result.missing_count, 0);
    assert_eq!(result.inconclusive_count, 0);
    let (bytes, rows) = read_jsonl(&result.output_path);
    assert_eq!(rows.len(), 2);
    assert_eq!(rows[0]["normalized_path"], r"C:\\Media\\a.jpg");
    assert_eq!(rows[1]["normalized_path"], r"C:\\Media\\z.jpg");
    assert!(rows.iter().all(|row| row.get("task_id").is_none()));
    assert!(rows.iter().all(|row| row.get("item_id").is_none()));
    assert!(rows.iter().all(|row| row.get("content_id").is_none()));
    assert!(rows.iter().all(|row| row.get("created_at_ms").is_none()));
    assert!(bytes.windows(2).all(|pair| pair != b"{}"));
    assert_eq!(result.sha256, format!("{:x}", Sha256::digest(bytes)));

    let metadata: Value =
        serde_json::from_slice(&fs::read(&result.metadata_path).expect("metadata"))
            .expect("metadata JSON");
    assert_eq!(metadata["task_id"], TASK_ID);
    assert_eq!(metadata["status"], "PASS");
}

#[test]
fn same_media_with_different_local_ids_has_same_canonical_hash() {
    let first = Fixture::new();
    seed_complete_image(
        &first,
        "task-first",
        "item-first",
        41,
        r"C:\\Media\\same.jpg",
    );
    let first_result = first.export("task-first", "first.jsonl");

    let second = Fixture::new();
    seed_complete_image(
        &second,
        "task-second",
        "item-second",
        777,
        r"C:\\Media\\same.jpg",
    );
    second.execute(
        "UPDATE contents SET md5=?1 WHERE content_id=777",
        params![vec![0xAB_u8; 16]],
    );
    let second_result = second.export("task-second", "second.jsonl");
    assert_eq!(first_result.sha256, second_result.sha256);
}

#[test]
fn duplicate_normalized_path_is_rejected_even_when_machine_and_item_differ() {
    let fixture = Fixture::new();
    fixture.content(1, 0xAB, 1024, "image", true);
    fixture.task(TASK_ID, "completed", 2, 2, 0, 0);
    fixture.item(
        TASK_ID,
        "item-a",
        "machine-a",
        r"C:\\Media\\same.jpg",
        "succeeded",
        Some(1),
    );
    fixture.item(
        TASK_ID,
        "item-b",
        "machine-b",
        r"C:\\Media\\same.jpg",
        "succeeded",
        Some(1),
    );
    let error = export_scan_result_summary(
        &fixture.database,
        &fixture.cache_root,
        TASK_ID,
        &fixture.output("duplicate.jsonl"),
    )
    .expect_err("重复规范路径必须拒绝");
    assert!(
        matches!(error, ResultSummaryError::InvalidArgument(message) if message.contains("normalized_path"))
    );
}

#[test]
fn image_stage_payload_fields_and_thumbnail_contract_are_stable() {
    let fixture = Fixture::new();
    seed_complete_image(&fixture, TASK_ID, "item-image", 1, r"C:\\Media\\image.jpg");
    let baseline = fixture.export(TASK_ID, "image-baseline.jsonl");
    let (_, rows) = read_jsonl(&baseline.output_path);
    assert_eq!(rows[0]["md5"], "abababababababababababababababab");
    assert_eq!(rows[0]["media_type"], "image");
    assert_eq!(rows[0]["thumbnail_sha256"], Value::Null);
    assert_eq!(
        rows[0]["thumbnail_state"],
        "unsupported_no_thumbnail_artifact"
    );
    assert!(
        rows[0]["feature_payloads"]["image_stage1"]
            .as_str()
            .is_some_and(|hash| hash.chars().all(|character| character.is_ascii_hexdigit()))
    );

    let mutations = [
        (
            "UPDATE image_stage1 SET width=1921 WHERE content_id=1",
            "width",
        ),
        (
            "UPDATE image_stage1 SET height=1081 WHERE content_id=1",
            "height",
        ),
        ("UPDATE image_stage1 SET pdq=?1 WHERE content_id=1", "pdq"),
        (
            "UPDATE image_stage1 SET quality=96 WHERE content_id=1",
            "quality",
        ),
        (
            "UPDATE image_stage2 SET phash_parts=?1 WHERE content_id=1",
            "phash",
        ),
        (
            "UPDATE image_stage2 SET sobel=?1 WHERE content_id=1",
            "sobel",
        ),
    ];
    for (sql, field) in mutations {
        if sql.contains("pdq") {
            fixture.execute(sql, params![vec![0x44_u8; 32]]);
        } else if sql.contains("phash") {
            fixture.execute(sql, params![vec![0x55_u8; 72]]);
        } else if sql.contains("sobel") {
            fixture.execute(sql, params![vec![0x66_u8; 512]]);
        } else {
            fixture.execute(sql, []);
        }
        let changed = fixture.export(TASK_ID, &format!("image-{field}.jsonl"));
        assert_ne!(
            baseline.sha256, changed.sha256,
            "{field} 必须影响 canonical hash"
        );
    }
}

#[test]
fn video_payloads_have_six_slots_and_every_raw_field_changes_hash() {
    let fixture = Fixture::new();
    seed_complete_video(&fixture, TASK_ID, "item-video", 1, r"C:\\Media\\video.mp4");
    let sheet = fixture.cache_root.join("contact.jpg");
    fs::write(&sheet, b"contact-sheet-bytes").expect("写联系表");
    fixture.execute(
        "INSERT INTO contact_sheets(content_id,relative_path) VALUES(1,'contact.jpg')",
        [],
    );
    let baseline = fixture.export(TASK_ID, "video-baseline.jsonl");
    let (_, rows) = read_jsonl(&baseline.output_path);
    assert_eq!(
        rows[0]["feature_payloads"]["video_frame_stage1"]
            .as_array()
            .unwrap()
            .len(),
        6
    );
    assert_eq!(
        rows[0]["feature_payloads"]["video_frame_stage2"]
            .as_array()
            .unwrap()
            .len(),
        6
    );
    assert_eq!(
        rows[0]["contact_sheet_sha256"],
        format!("{:x}", Sha256::digest(b"contact-sheet-bytes"))
    );

    let mutations = [
        (
            "UPDATE video_metadata SET duration_ms=12346 WHERE content_id=1",
            "metadata-duration",
        ),
        (
            "UPDATE video_metadata SET width=3841 WHERE content_id=1",
            "metadata-width",
        ),
        (
            "UPDATE video_metadata SET height=2161 WHERE content_id=1",
            "metadata-height",
        ),
    ];
    for (sql, field) in mutations {
        fixture.execute(sql, []);
        let changed = fixture.export(TASK_ID, &format!("video-{field}.jsonl"));
        assert_ne!(baseline.sha256, changed.sha256, "{field} 必须影响 hash");
    }
    for slot in 0_i64..6 {
        fixture.execute(
            "UPDATE video_frame_stage1 SET time_ms=time_ms+1 WHERE content_id=1 AND slot=?1",
            params![slot],
        );
        let changed = fixture.export(TASK_ID, &format!("video-stage1-time-{slot}.jsonl"));
        assert_ne!(
            baseline.sha256, changed.sha256,
            "视频 stage1 槽位 {slot} 必须影响 hash"
        );
    }
    fixture.execute(
        "UPDATE video_frame_stage1 SET decoded=0,width=NULL,height=NULL,pdq=NULL,quality=NULL WHERE content_id=1 AND slot=0",
        [],
    );
    let changed = fixture.export(TASK_ID, "video-stage1-fields.jsonl");
    assert_ne!(baseline.sha256, changed.sha256);
    fixture.execute(
        "UPDATE video_frame_stage2 SET phash_parts=?1,sobel=?2 WHERE content_id=1 AND slot=5",
        params![vec![0x77_u8; 72], vec![0x88_u8; 512]],
    );
    let changed = fixture.export(TASK_ID, "video-stage2-fields.jsonl");
    assert_ne!(baseline.sha256, changed.sha256);
}

#[test]
fn contact_sheet_paths_reject_absolute_parent_and_escape() {
    for (index, relative_path) in [
        ("absolute", r"C:\\outside.jpg"),
        ("drive-relative", r"C:outside.jpg"),
        ("parent", r"..\\outside.jpg"),
        ("escape", r"nested\\..\\..\\outside.jpg"),
    ] {
        let fixture = Fixture::new();
        seed_complete_video(&fixture, TASK_ID, "item-video", 1, r"C:\\Media\\video.mp4");
        fixture.execute(
            "INSERT INTO contact_sheets(content_id,relative_path) VALUES(1,?1)",
            params![relative_path],
        );
        let error = export_scan_result_summary(
            &fixture.database,
            &fixture.cache_root,
            TASK_ID,
            &fixture.output(&format!("{index}.jsonl")),
        )
        .expect_err("不安全联系表路径必须拒绝");
        assert!(matches!(error, ResultSummaryError::UnsafeArtifactPath));
    }
}

#[test]
fn contact_sheet_symlink_is_rejected_when_platform_allows_creation() {
    let fixture = Fixture::new();
    seed_complete_video(&fixture, TASK_ID, "item-video", 1, r"C:\Media\video.mp4");
    let outside = fixture.root.path().join("outside-contact.jpg");
    let link = fixture.cache_root.join("link-contact.jpg");
    fs::write(&outside, b"outside-contact").expect("写越界联系表");
    let created = {
        #[cfg(windows)]
        {
            std::os::windows::fs::symlink_file(&outside, &link)
        }
        #[cfg(unix)]
        {
            std::os::unix::fs::symlink(&outside, &link)
        }
    };
    if created.is_err() {
        // Windows 无开发者模式时系统拒绝创建 symlink；其余安全路径仍由行为测试覆盖。
        eprintln!("SKIPPED: symlink creation is unavailable under current Windows policy");
        return;
    }
    fixture.execute(
        "INSERT INTO contact_sheets(content_id,relative_path) VALUES(1,'link-contact.jpg')",
        params![],
    );
    let error = export_scan_result_summary(
        &fixture.database,
        &fixture.cache_root,
        TASK_ID,
        &fixture.output("symlink-contact.jsonl"),
    )
    .expect_err("symlink 联系表必须拒绝");
    assert!(matches!(error, ResultSummaryError::UnsafeArtifactPath));
}

#[test]
fn readonly_export_preserves_database_and_write_is_rejected() {
    let fixture = Fixture::new();
    seed_complete_image(&fixture, TASK_ID, "item-image", 1, r"C:\\Media\\image.jpg");
    let before = sha256_file(&fixture.database);
    let result = fixture.export(TASK_ID, "readonly.jsonl");
    let after = sha256_file(&fixture.database);
    assert_eq!(before, after, "导出不能修改 SQLite");
    let connection = Connection::open_with_flags(
        &fixture.database,
        OpenFlags::SQLITE_OPEN_READ_ONLY | OpenFlags::SQLITE_OPEN_NO_MUTEX,
    )
    .expect("只读打开");
    connection
        .execute_batch("PRAGMA query_only=ON; PRAGMA busy_timeout=5000;")
        .expect("只读参数");
    let error = connection
        .execute("DELETE FROM tasks WHERE task_id=?1", [TASK_ID])
        .expect_err("只读连接必须拒绝写语句");
    assert!(error.to_string().to_ascii_lowercase().contains("readonly"));
    assert_eq!(result.status, ResultSummaryStatus::Pass);
}

#[test]
fn status_matrix_distinguishes_missing_from_inconclusive() {
    let missing = Fixture::new();
    let missing_result = missing.export("task-does-not-exist", "missing.jsonl");
    assert_eq!(missing_result.status, ResultSummaryStatus::Missing);

    let empty = Fixture::new();
    empty.task("empty-task", "completed", 0, 0, 0, 0);
    let empty_result = empty.export("empty-task", "empty.jsonl");
    assert_eq!(empty_result.status, ResultSummaryStatus::Missing);

    for (task_status, item_status) in [
        ("running", "succeeded"),
        ("completed", "failed"),
        ("completed", "cancelled"),
        ("completed", "queued"),
    ] {
        let fixture = Fixture::new();
        fixture.task("status-task", task_status, 1, 1, 0, 0);
        fixture.item(
            "status-task",
            "item",
            "machine-a",
            r"C:\\Media\\status",
            item_status,
            None,
        );
        let result = fixture.export("status-task", &format!("{task_status}-{item_status}.jsonl"));
        assert_eq!(result.status, ResultSummaryStatus::Inconclusive);
        assert_eq!(result.inconclusive_count, 1);
    }

    let missing_content = Fixture::new();
    missing_content.task("missing-content", "completed", 1, 1, 0, 0);
    missing_content.item(
        "missing-content",
        "item",
        "machine-a",
        r"C:\\Media\\missing",
        "succeeded",
        None,
    );
    let result = missing_content.export("missing-content", "missing-content.jsonl");
    assert_eq!(result.status, ResultSummaryStatus::Missing);
    assert_eq!(result.missing_count, 1);

    let incomplete = Fixture::new();
    incomplete.content(1, 0xAB, 1, "image", false);
    incomplete.task("incomplete", "completed", 1, 1, 0, 0);
    incomplete.item(
        "incomplete",
        "item",
        "machine-a",
        r"C:\\Media\\incomplete",
        "succeeded",
        Some(1),
    );
    let result = incomplete.export("incomplete", "incomplete.jsonl");
    assert_eq!(result.status, ResultSummaryStatus::Inconclusive);
    assert_eq!(result.inconclusive_count, 1);

    let no_sheet = Fixture::new();
    seed_complete_video(&no_sheet, "no-sheet", "item", 1, r"C:\\Media\\video");
    let result = no_sheet.export("no-sheet", "no-sheet.jsonl");
    assert_eq!(result.status, ResultSummaryStatus::Missing);
    assert_eq!(result.missing_count, 1);
}

#[test]
fn output_parent_and_arguments_are_real_errors_not_missing_summaries() {
    let fixture = Fixture::new();
    let error = export_scan_result_summary(
        &fixture.database,
        &fixture.cache_root,
        "",
        &fixture.output("invalid.jsonl"),
    )
    .expect_err("空 task id 必须拒绝");
    assert!(matches!(error, ResultSummaryError::InvalidArgument(_)));

    let output = fixture.root.path().join("not-there").join("result.jsonl");
    let error =
        export_scan_result_summary(&fixture.database, &fixture.cache_root, "missing", &output)
            .expect_err("输出父目录不存在必须失败");
    assert!(matches!(error, ResultSummaryError::Io(_)));
}

#[test]
fn non_succeeded_rows_are_identity_only_and_hash_stable() {
    let fixture = Fixture::new();
    fixture.content(1, 0xAB, 1024, "image", true);
    fixture.task("identity-only", "completed", 4, 0, 1, 1);
    for (index, status) in ["failed", "cancelled", "queued", "running"]
        .into_iter()
        .enumerate()
    {
        fixture.item(
            "identity-only",
            &format!("item-{index}"),
            "machine-a",
            &format!(r"C:\\Media\\identity-{index}.jpg"),
            status,
            Some(1),
        );
    }
    fixture.execute(
        "INSERT INTO image_stage1(content_id,width,height,pdq,quality) VALUES(1,1920,1080,?1,97)",
        params![vec![0x11_u8; 32]],
    );

    let first = fixture.export("identity-only", "identity-only-first.jsonl");
    let (_, rows) = read_jsonl(&first.output_path);
    assert_eq!(rows.len(), 4);
    for row in &rows {
        assert!(row["normalized_path"].is_string());
        assert!(row["status"].is_string());
        assert!(row["file_size"].is_null());
        assert!(row["md5"].is_null());
        assert!(row["media_type"].is_null());
        assert!(row["base_complete"].is_null());
        assert!(row["feature_payload_sha256"].is_null());
        assert!(row["contact_sheet_sha256"].is_null());
        assert!(row["thumbnail_sha256"].is_null());
        assert!(row["feature_payloads"]["image_stage1"].is_null());
        assert!(row["feature_payloads"]["image_stage2"].is_null());
        assert!(row["feature_payloads"]["video_metadata"].is_null());
        for slot in row["feature_payloads"]["video_frame_stage1"]
            .as_array()
            .expect("固定六槽位")
        {
            assert!(slot.is_null());
        }
        for slot in row["feature_payloads"]["video_frame_stage2"]
            .as_array()
            .expect("固定六槽位")
        {
            assert!(slot.is_null());
        }
    }
    let metadata: Value =
        serde_json::from_slice(&fs::read(&first.metadata_path).expect("读取诊断 metadata"))
            .expect("解析诊断 metadata");
    assert!(
        metadata["diagnostics"]
            .as_array()
            .expect("诊断数组")
            .iter()
            .filter(|diagnostic| diagnostic["kind"] == "item_status")
            .all(|diagnostic| diagnostic["content_id"].as_i64() == Some(1))
    );

    // content_id 和 feature payload 变化只能影响 metadata，不能改变非成功行的身份摘要。
    fixture.execute(
        "UPDATE contents SET md5=?1 WHERE content_id=1",
        params![vec![0xEE_u8; 16]],
    );
    fixture.execute(
        "UPDATE image_stage1 SET width=3840,pdq=?1 WHERE content_id=1",
        params![vec![0x77_u8; 32]],
    );
    let second = fixture.export("identity-only", "identity-only-second.jsonl");
    let (first_bytes, _) = read_jsonl(&first.output_path);
    let (second_bytes, _) = read_jsonl(&second.output_path);
    assert_eq!(first_bytes, second_bytes);
    assert_eq!(first.sha256, second.sha256);
}

#[test]
fn malformed_identity_and_content_evidence_never_passes() {
    for (name, sql) in [
        (
            "null-path",
            "UPDATE task_items SET normalized_path=NULL WHERE item_id='item'",
        ),
        (
            "empty-path",
            "UPDATE task_items SET normalized_path='' WHERE item_id='item'",
        ),
    ] {
        let fixture = Fixture::new();
        fixture.content(1, 0xAB, 1024, "image", true);
        fixture.task(name, "completed", 1, 1, 0, 0);
        fixture.item(
            name,
            "item",
            "machine-a",
            r"C:\Media\bad.jpg",
            "succeeded",
            Some(1),
        );
        fixture.execute_unchecked(sql, params![]);
        let error = export_scan_result_summary(
            &fixture.database,
            &fixture.cache_root,
            name,
            &fixture.output(&format!("{name}.jsonl")),
        )
        .expect_err("NULL/空 normalized_path 必须拒绝");
        assert!(matches!(error, ResultSummaryError::InvalidArgument(_)));
    }

    let duplicate_null = Fixture::new();
    duplicate_null.content(1, 0xAB, 1024, "image", true);
    duplicate_null.task("duplicate-null", "completed", 2, 2, 0, 0);
    duplicate_null.item(
        "duplicate-null",
        "item-a",
        "machine-a",
        r"C:\Media\a.jpg",
        "succeeded",
        Some(1),
    );
    duplicate_null.item(
        "duplicate-null",
        "item-b",
        "machine-b",
        r"C:\Media\b.jpg",
        "succeeded",
        Some(1),
    );
    duplicate_null.execute_unchecked(
        "UPDATE task_items SET normalized_path=NULL WHERE task_id='duplicate-null'",
        params![],
    );
    let error = export_scan_result_summary(
        &duplicate_null.database,
        &duplicate_null.cache_root,
        "duplicate-null",
        &duplicate_null.output("duplicate-null.jsonl"),
    )
    .expect_err("重复 NULL normalized_path 不能绕过身份校验");
    assert!(matches!(error, ResultSummaryError::InvalidArgument(_)));

    for (name, sql, values) in [
        (
            "task-size-mismatch",
            "UPDATE task_items SET file_size=2048 WHERE item_id='item'",
            None,
        ),
        (
            "task-size-null",
            "UPDATE task_items SET file_size=NULL WHERE item_id='item'",
            None,
        ),
        (
            "negative-task-size",
            "UPDATE task_items SET file_size=-1 WHERE item_id='item'",
            None,
        ),
        (
            "negative-content-size",
            "UPDATE contents SET file_size=-1 WHERE content_id=1",
            None,
        ),
        (
            "invalid-md5",
            "UPDATE contents SET md5=?1 WHERE content_id=1",
            Some(vec![0xAB_u8; 15]),
        ),
        (
            "invalid-media",
            "UPDATE contents SET media_kind='invalid' WHERE content_id=1",
            None,
        ),
        (
            "invalid-bool",
            "UPDATE contents SET base_complete=2 WHERE content_id=1",
            None,
        ),
    ] {
        let fixture = Fixture::new();
        fixture.content(1, 0xAB, 1024, "image", true);
        fixture.task(name, "completed", 1, 1, 0, 0);
        fixture.item(
            name,
            "item",
            "machine-a",
            r"C:\Media\bad.jpg",
            "succeeded",
            Some(1),
        );
        if let Some(value) = values {
            fixture.execute_unchecked(sql, params![value]);
        } else {
            fixture.execute_unchecked(sql, params![]);
        }
        let result = export_scan_result_summary(
            &fixture.database,
            &fixture.cache_root,
            name,
            &fixture.output(&format!("{name}.jsonl")),
        );
        let summary = result.expect("已存在内容的坏证据应生成 INCONCLUSIVE 摘要");
        assert_eq!(summary.status, ResultSummaryStatus::Inconclusive, "{name}");
        assert_eq!(summary.missing_count, 0, "{name} 不应计为 MISSING");
        assert_eq!(summary.inconclusive_count, 1, "{name}");
        let metadata: Value =
            serde_json::from_slice(&fs::read(&summary.metadata_path).expect("读取坏证据 metadata"))
                .expect("解析坏证据 metadata");
        assert!(
            metadata["diagnostics"]
                .as_array()
                .expect("诊断数组")
                .iter()
                .any(|diagnostic| diagnostic["kind"] == "invalid_content"
                    && diagnostic["message"].is_string())
        );
    }
}

#[test]
fn task_level_counts_are_item_counts() {
    let fixture = Fixture::new();
    fixture.content(1, 0xAB, 1024, "image", true);
    fixture.task("running-many", "running", 3, 3, 0, 0);
    for index in 0..3 {
        fixture.item(
            "running-many",
            &format!("item-{index}"),
            "machine-a",
            &format!(r"C:\\Media\\running-{index}.jpg"),
            "succeeded",
            Some(1),
        );
    }
    let result = fixture.export("running-many", "running-many.jsonl");
    assert_eq!(result.status, ResultSummaryStatus::Inconclusive);
    assert_eq!(result.row_count, 3);
    assert_eq!(result.inconclusive_count, 3);
    let metadata: Value =
        serde_json::from_slice(&fs::read(&result.metadata_path).expect("读取 running metadata"))
            .expect("解析 running metadata");
    assert_eq!(metadata["count_definition"], "item_count");
    assert_eq!(metadata["inconclusive_count"], 3);
}

#[test]
fn output_pair_is_atomic_and_never_overwrites_existing_files() {
    let fixture = Fixture::new();
    seed_complete_image(&fixture, TASK_ID, "item-image", 1, r"C:\Media\image.jpg");

    let existing_output = fixture.output("existing.jsonl");
    let existing_bytes = b"user-owned-output";
    fs::write(&existing_output, existing_bytes).expect("创建用户既有输出");
    let error = export_scan_result_summary(
        &fixture.database,
        &fixture.cache_root,
        TASK_ID,
        &existing_output,
    )
    .expect_err("默认不能覆盖既有 canonical");
    assert!(matches!(error, ResultSummaryError::InvalidArgument(_)));
    assert_eq!(
        fs::read(&existing_output).expect("保留用户输出"),
        existing_bytes
    );
    assert!(
        !existing_output
            .with_file_name("existing-meta.json")
            .exists()
    );

    let pair_output = fixture.output("pair.jsonl");
    let pair_metadata = pair_output.with_file_name("pair-meta.json");
    let pair_metadata_bytes = b"user-owned-metadata";
    fs::write(&pair_metadata, pair_metadata_bytes).expect("创建用户既有 metadata");
    let error = export_scan_result_summary(
        &fixture.database,
        &fixture.cache_root,
        TASK_ID,
        &pair_output,
    )
    .expect_err("metadata 冲突不能留下单份 canonical");
    assert!(matches!(error, ResultSummaryError::InvalidArgument(_)));
    assert!(!pair_output.exists());
    assert_eq!(
        fs::read(&pair_metadata).expect("保留用户 metadata"),
        pair_metadata_bytes
    );

    let inside_cache = fixture.cache_root.join("inside.jsonl");
    let error = export_scan_result_summary(
        &fixture.database,
        &fixture.cache_root,
        TASK_ID,
        &inside_cache,
    )
    .expect_err("输出不能位于 cache root");
    assert!(matches!(error, ResultSummaryError::InvalidArgument(_)));
    assert!(!inside_cache.exists());

    let error = export_scan_result_summary(
        &fixture.database,
        &fixture.cache_root,
        TASK_ID,
        &fixture.database,
    )
    .expect_err("输出不能覆盖数据库");
    assert!(matches!(error, ResultSummaryError::InvalidArgument(_)));
}

#[test]
fn sqlite_sidecars_keep_existence_and_hash_before_after_export() {
    let fixture = Fixture::new();
    seed_complete_image(&fixture, TASK_ID, "item-image", 1, r"C:\Media\image.jpg");
    let wal = fixture.sidecar("wal");
    let shm = fixture.sidecar("shm");
    fs::write(&wal, b"").expect("创建 WAL sidecar");
    fs::write(&shm, b"").expect("创建 SHM sidecar");
    let before = [
        sha256_file(&fixture.database),
        sha256_file(&wal),
        sha256_file(&shm),
    ];
    let result = fixture.export(TASK_ID, "sidecars.jsonl");
    assert_eq!(result.status, ResultSummaryStatus::Pass);
    assert!(wal.exists());
    assert!(shm.exists());
    assert_eq!(
        before,
        [
            sha256_file(&fixture.database),
            sha256_file(&wal),
            sha256_file(&shm)
        ]
    );
}

/// 模拟安全快照完成后的 WAL 篡改；导出器必须拒绝且不得提交结果文件。
fn mutate_wal_after_sidecar_capture(database_path: &Path) {
    let [wal, _] = sidecar_paths_for_acceptance(database_path);
    fs::write(wal, b"sidecar-mutated-after-capture").expect("修改已冻结 WAL");
}

#[test]
fn first_read_open_sidecar_initialization_is_captured_before_export() {
    let fixture = Fixture::new();
    seed_complete_image(&fixture, TASK_ID, "item-image", 1, r"C:\\Media\\image.jpg");
    let _wal_keeper = fixture.prepare_wal_first_read();
    let sidecars_before_read = fixture.wal_sidecar_hashes();
    let output = fixture.output("first-read-open.jsonl");

    let result =
        export_scan_result_summary(&fixture.database, &fixture.cache_root, TASK_ID, &output)
            .expect("首次只读打开初始化的 SHM 必须在快照内被接受");

    assert_eq!(result.status, ResultSummaryStatus::Pass);
    assert!(output.exists());
    assert!(output.with_file_name("first-read-open-meta.json").exists());
    assert!(pair_commit_lease_path(&output).exists());
    validate_result_summary_pair(&output).expect("首次打开后的完整三件套必须可验证");
    assert_ne!(
        sidecars_before_read,
        fixture.wal_sidecar_hashes(),
        "首次真实 header 读取必须刷新至少一个 WAL/SHM sidecar"
    );
}

#[test]
fn sidecar_mutation_after_capture_is_rejected_without_output_pair() {
    let fixture = Fixture::new();
    fixture.enable_wal();
    seed_complete_image(&fixture, TASK_ID, "item-image", 1, r"C:\\Media\\image.jpg");
    let output = fixture.output("after-sidecar-capture.jsonl");
    set_result_summary_read_test_callback(Some(mutate_wal_after_sidecar_capture));
    set_result_summary_read_test_hook(ResultSummaryReadTestHook::AfterSidecarCapture);

    let error =
        export_scan_result_summary(&fixture.database, &fixture.cache_root, TASK_ID, &output)
            .expect_err("快照后的 WAL 修改必须拒绝");

    set_result_summary_read_test_hook(ResultSummaryReadTestHook::None);
    set_result_summary_read_test_callback(None);
    assert!(matches!(error, ResultSummaryError::InvalidArgument(_)));
    assert!(!output.exists());
    assert!(
        !output
            .with_file_name("after-sidecar-capture-meta.json")
            .exists()
    );
    assert!(!pair_commit_lease_path(&output).exists());
}

#[test]
fn sidecar_paths_preserve_non_utf8_os_strings() {
    #[cfg(windows)]
    {
        use std::os::windows::ffi::OsStringExt;
        let database = PathBuf::from(std::ffi::OsString::from_wide(&[0x4E2D, 0xD800, 0x0062]));
        let [wal, shm] = sidecar_paths_for_acceptance(&database);
        let mut expected_wal = database.as_os_str().to_os_string();
        expected_wal.push("-wal");
        let mut expected_shm = database.as_os_str().to_os_string();
        expected_shm.push("-shm");
        assert_eq!(wal.as_os_str(), expected_wal.as_os_str());
        assert_eq!(shm.as_os_str(), expected_shm.as_os_str());
    }
    #[cfg(unix)]
    {
        use std::os::unix::ffi::{OsStrExt, OsStringExt};
        let database = PathBuf::from(std::ffi::OsString::from_vec(vec![b'n', 0xFF, b'd']));
        let [wal, shm] = sidecar_paths_for_acceptance(&database);
        assert_eq!(wal.as_os_str().as_bytes(), b"n\xFFd-wal");
        assert_eq!(shm.as_os_str().as_bytes(), b"n\xFFd-shm");
    }
}

/// 在 metadata 已发布后抢占 canonical 名称，模拟外部 exporter/进程竞争。
fn create_external_canonical(path: &Path) {
    let mut file = fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(path)
        .expect("外部 canonical 抢占必须只新建文件");
    std::io::Write::write_all(&mut file, b"external-canonical\n").expect("写外部 canonical");
}

/// 在 canonical 发布后把它移到旁证名并新建替换文件，模拟外部恶意替换。
fn replace_external_canonical(path: &Path) {
    let backup = path.with_file_name("hook-replaced-original.jsonl");
    fs::rename(path, &backup).expect("外部替换必须保留原 canonical");
    fs::write(path, b"external-replacement\n").expect("写外部替换文件");
}

#[test]
fn successful_pair_has_persistent_manifest_and_validator() {
    let fixture = Fixture::new();
    seed_complete_image(&fixture, TASK_ID, "item-image", 1, r"C:\Media\image.jpg");
    let output = fixture.output("pair-success.jsonl");
    let result = fixture.export(TASK_ID, "pair-success.jsonl");
    let metadata = output.with_file_name("pair-success-meta.json");
    let lease = pair_commit_lease_path(&output);
    assert!(output.exists());
    assert!(metadata.exists());
    assert!(lease.exists(), "成功 pair 必须留下持久 manifest lease");
    validate_result_summary_pair(&output).expect("完整三件套必须可验证");
    let marker: Value = serde_json::from_slice(&fs::read(&metadata).expect("读取 metadata"))
        .expect("metadata JSON");
    assert!(!marker["lease_token"].as_str().unwrap().is_empty());
    assert_eq!(marker["canonical_sha256"], result.sha256);
    assert_eq!(marker["row_count"], result.row_count);
    assert_eq!(marker["schema_version"], 1);
    assert!(
        fs::read_dir(fixture.root.path())
            .expect("读取 run evidence 根")
            .filter_map(Result::ok)
            .any(|entry| entry
                .file_name()
                .to_string_lossy()
                .starts_with(".pair-success.jsonl.run-")),
        "成功导出也必须保留唯一 run evidence 目录"
    );
}

#[test]
fn validator_rejects_singletons_and_manifest_tampering() {
    let fixture = Fixture::new();
    let canonical_only = fixture.output("canonical-only.jsonl");
    fs::write(&canonical_only, b"{}\n").expect("写 canonical-only");
    assert!(matches!(
        validate_result_summary_pair(&canonical_only),
        Err(ResultSummaryError::OutputCommitIncomplete)
    ));

    let metadata_only = fixture.output("metadata-only.jsonl");
    fs::write(
        metadata_only.with_file_name("metadata-only-meta.json"),
        b"{}\n",
    )
    .expect("写 metadata-only");
    assert!(matches!(
        validate_result_summary_pair(&metadata_only),
        Err(ResultSummaryError::OutputCommitIncomplete)
    ));

    seed_complete_image(&fixture, TASK_ID, "item-image", 1, r"C:\Media\image.jpg");
    let output = fixture.output("validator-tamper.jsonl");
    fixture.export(TASK_ID, "validator-tamper.jsonl");
    let metadata = output.with_file_name("validator-tamper-meta.json");
    let lease = pair_commit_lease_path(&output);
    validate_result_summary_pair(&output).expect("初始 pair 必须有效");

    let lease_backup = output.with_file_name("validator-tamper-lease.backup");
    fs::rename(&lease, &lease_backup).expect("模拟 lease 缺失");
    assert!(matches!(
        validate_result_summary_pair(&output),
        Err(ResultSummaryError::OutputCommitIncomplete)
    ));
    fs::rename(&lease_backup, &lease).expect("恢复 lease");

    let original_metadata = fs::read(&metadata).expect("metadata bytes");
    let mut tampered_metadata: Value = serde_json::from_slice(&original_metadata).unwrap();
    tampered_metadata["lease_token"] = Value::String("wrong-token".into());
    fs::write(&metadata, serde_json::to_vec(&tampered_metadata).unwrap()).expect("改写 token");
    assert!(matches!(
        validate_result_summary_pair(&output),
        Err(ResultSummaryError::OutputCommitIncomplete)
    ));
    fs::write(&metadata, original_metadata).expect("恢复 metadata");

    let original_lease = fs::read(&lease).expect("lease bytes");
    let mut tampered_lease: Value = serde_json::from_slice(&original_lease).unwrap();
    tampered_lease["expected_canonical_identity"]["first"] = Value::from(0_u64);
    fs::write(&lease, serde_json::to_vec(&tampered_lease).unwrap()).expect("改写身份");
    assert!(matches!(
        validate_result_summary_pair(&output),
        Err(ResultSummaryError::OutputCommitIncomplete)
    ));
    fs::write(&lease, original_lease).expect("恢复 lease");

    let original_canonical = fs::read(&output).expect("canonical bytes");
    fs::write(&output, b"tampered\n").expect("改写 canonical");
    assert!(matches!(
        validate_result_summary_pair(&output),
        Err(ResultSummaryError::OutputCommitIncomplete)
    ));
    fs::write(&output, original_canonical).expect("恢复 canonical");
    validate_result_summary_pair(&output).expect("恢复后 pair 必须有效");
}

#[test]
fn metadata_first_and_external_replacement_leave_incomplete_evidence() {
    let fixture = Fixture::new();
    seed_complete_image(&fixture, TASK_ID, "item-image", 1, r"C:\Media\image.jpg");
    let output = fixture.output("hook-before-canonical.jsonl");
    set_result_summary_commit_test_callback(Some(create_external_canonical));
    set_result_summary_commit_test_hook(ResultSummaryCommitTestHook::BeforeCanonicalExternal);
    let error =
        export_scan_result_summary(&fixture.database, &fixture.cache_root, TASK_ID, &output)
            .expect_err("canonical 抢占必须返回不完整 pair");
    set_result_summary_commit_test_hook(ResultSummaryCommitTestHook::None);
    set_result_summary_commit_test_callback(None);
    assert!(matches!(error, ResultSummaryError::OutputCommitIncomplete));
    assert_eq!(fs::read(&output).unwrap(), b"external-canonical\n");
    assert!(
        output
            .with_file_name("hook-before-canonical-meta.json")
            .exists()
    );
    assert!(pair_commit_lease_path(&output).exists());
    assert!(matches!(
        validate_result_summary_pair(&output),
        Err(ResultSummaryError::OutputCommitIncomplete)
    ));

    let replacement_fixture = Fixture::new();
    seed_complete_image(
        &replacement_fixture,
        TASK_ID,
        "item-image",
        1,
        r"C:\Media\image.jpg",
    );
    let replacement_output = replacement_fixture.output("hook-after-canonical.jsonl");
    set_result_summary_commit_test_callback(Some(replace_external_canonical));
    set_result_summary_commit_test_hook(ResultSummaryCommitTestHook::AfterCanonicalExternal);
    let error = export_scan_result_summary(
        &replacement_fixture.database,
        &replacement_fixture.cache_root,
        TASK_ID,
        &replacement_output,
    )
    .expect_err("canonical 外部替换必须由 validator 拒绝");
    set_result_summary_commit_test_hook(ResultSummaryCommitTestHook::None);
    set_result_summary_commit_test_callback(None);
    assert!(matches!(error, ResultSummaryError::OutputCommitIncomplete));
    assert_eq!(
        fs::read(&replacement_output).expect("保留外部替换文件"),
        b"external-replacement\n"
    );
    assert!(
        !fs::read(replacement_output.with_file_name("hook-replaced-original.jsonl"))
            .expect("保留原 canonical 旁证")
            .is_empty()
    );
    assert!(
        replacement_output
            .with_file_name("hook-after-canonical-meta.json")
            .exists()
    );
    assert!(pair_commit_lease_path(&replacement_output).exists());
    assert!(matches!(
        validate_result_summary_pair(&replacement_output),
        Err(ResultSummaryError::OutputCommitIncomplete)
    ));
}

#[test]
fn pair_commit_lease_rejects_cooperating_concurrent_exporter_and_persists() {
    let fixture = Fixture::new();
    seed_complete_image(&fixture, TASK_ID, "item-image", 1, r"C:\Media\image.jpg");
    let output = fixture.output("lease.jsonl");
    let lease = pair_commit_lease_path(&output);
    fs::write(&lease, b"held-by-other-exporter").expect("创建占用 lease");
    let error =
        export_scan_result_summary(&fixture.database, &fixture.cache_root, TASK_ID, &output)
            .expect_err("已有 lease 必须拒绝并发 exporter");
    assert!(matches!(error, ResultSummaryError::OutputCommitIncomplete));
    assert_eq!(
        fs::read(&lease).expect("保留 lease"),
        b"held-by-other-exporter"
    );
    assert!(!output.exists());
}
