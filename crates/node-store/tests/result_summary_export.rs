//! 固定 TSV 结果摘要的行为验收；fixture 只使用隔离 SQLite、缓存根和输出目录。

use std::{
    fs,
    path::{Path, PathBuf},
};

use dedup_core::product_id;
use dedup_node_store::result_summary::{
    ResultSummaryError, ResultSummaryReadTestHook, ResultSummaryStatus, export_scan_result_summary,
    set_result_summary_read_test_callback, set_result_summary_read_test_hook,
    sidecar_paths_for_acceptance, validate_result_summary,
};
use rusqlite::{Connection, params};
use sha2::{Digest, Sha256};
use tempfile::{TempDir, tempdir};

/// 创建真实 schema 的隔离数据库，并提供少量写入 helper 构造 active 文件事实。
struct Fixture {
    root: TempDir,
    database: PathBuf,
    cache_root: PathBuf,
}

impl Fixture {
    /// 创建只用于测试的 SQLite/cache 根；生产路径不会被访问。
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

    /// 执行隔离 fixture SQL；导出器本身不能调用该 helper。
    fn execute(&self, sql: &str, values: impl rusqlite::Params) {
        let connection = Connection::open(&self.database).expect("打开 fixture");
        connection.execute(sql, values).expect("写入 fixture");
    }

    /// 写入内容事实；不同 ID 使用不同 md5/size，避免 UNIQUE 冲突。
    fn content(&self, content_id: i64, md5_byte: u8, size: i64, media_kind: &str, complete: i64) {
        self.execute(
            "INSERT INTO contents(content_id,md5,file_size,media_kind,base_complete)
             VALUES(?1,?2,?3,?4,?5)",
            params![content_id, vec![md5_byte; 16], size, media_kind, complete],
        );
    }

    /// 写入当前文件事实；active=0 的记录用于验证过滤边界。
    fn file(&self, machine: &str, path: &str, content_id: i64, size: i64, active: i64) {
        self.execute(
            "INSERT INTO files(machine_id,normalized_path,display_path,file_size,content_id,active)
             VALUES(?1,?2,?2,?3,?4,?5)",
            params![machine, path, size, content_id, active],
        );
    }

    /// 写入图片一筛 payload，允许构造缺字段/坏长度的 INCONCLUSIVE 数据。
    fn image_stage1(
        &self,
        content_id: i64,
        width: Option<i64>,
        height: Option<i64>,
        pdq: Option<Vec<u8>>,
        quality: Option<i64>,
    ) {
        self.execute(
            "INSERT INTO image_stage1(content_id,width,height,pdq,quality)
             VALUES(?1,?2,?3,?4,?5)",
            params![content_id, width, height, pdq, quality],
        );
    }

    /// 写入完整图片二筛 payload。
    fn image_stage2(&self, content_id: i64) {
        self.execute(
            "INSERT INTO image_stage2(content_id,phash_parts,sobel) VALUES(?1,?2,?3)",
            params![content_id, vec![2_u8; 72], vec![3_u8; 512]],
        );
    }

    /// 写入视频 metadata、六个一筛槽位和部分二筛槽位。
    fn video_features(&self, content_id: i64) {
        self.execute(
            "INSERT INTO video_metadata(content_id,duration_ms,width,height) VALUES(?1,1000,1920,1080)",
            params![content_id],
        );
        for slot in 0_i64..6 {
            let decoded = i64::from(slot < 4);
            self.execute(
                "INSERT INTO video_frame_stage1(content_id,slot,time_ms,decoded,width,height,pdq,quality)
                 VALUES(?1,?2,?3,?4,?5,?6,?7,?8)",
                params![
                    content_id,
                    slot,
                    slot * 100,
                    decoded,
                    (decoded == 1).then_some(1920_i64),
                    (decoded == 1).then_some(1080_i64),
                    (decoded == 1).then(|| vec![slot as u8; 32]),
                    (decoded == 1).then_some(90_i64),
                ],
            );
            if slot < 4 {
                self.execute(
                    "INSERT INTO video_frame_stage2(content_id,slot,phash_parts,sobel)
                     VALUES(?1,?2,?3,?4)",
                    params![content_id, slot, vec![slot as u8; 72], vec![4_u8; 512]],
                );
            }
        }
    }

    /// 写入联系表关系及缓存 artifact。
    fn contact_sheet(&self, content_id: i64, relative_path: &str, bytes: &[u8]) {
        let artifact = self.cache_root.join(relative_path);
        if let Some(parent) = artifact.parent() {
            fs::create_dir_all(parent).expect("创建 artifact 目录");
        }
        fs::write(&artifact, bytes).expect("写入 artifact");
        self.execute(
            "INSERT INTO contact_sheets(content_id,relative_path) VALUES(?1,?2)",
            params![content_id, relative_path],
        );
    }

    /// 每次导出使用独立目录，但固定输出文件名始终是 result-summary.tsv。
    fn output(&self, name: &str) -> PathBuf {
        let directory = self.root.path().join(name);
        fs::create_dir_all(&directory).expect("创建输出目录");
        directory.join("result-summary.tsv")
    }

    /// 按指定媒体根执行一次导出。
    fn export(
        &self,
        roots: &[PathBuf],
        name: &str,
    ) -> dedup_node_store::result_summary::ResultSummaryExport {
        let output = self.output(name);
        export_scan_result_summary(&self.database, &self.cache_root, roots, &output)
            .expect("导出结果摘要")
    }

    /// 让 SQLite 保留 WAL/SHM，模拟首次只读打开会初始化 WAL-index 的节点数据库。
    fn prepare_wal(&self) -> Connection {
        let connection = Connection::open(&self.database).expect("打开 WAL fixture");
        let mode: String = connection
            .query_row("PRAGMA journal_mode=WAL", [], |row| row.get(0))
            .expect("切换 WAL");
        assert_eq!(mode, "wal");
        connection
            .execute_batch(
                "PRAGMA wal_autocheckpoint=0;
                 UPDATE metadata SET value='wal-first-read' WHERE key='machine_id';",
            )
            .expect("写入 WAL");
        connection
    }
}

/// 计算隔离文件 hash，验证只读导出不改数据库或 artifact。
fn sha256_file(path: &Path) -> String {
    let mut digest = Sha256::new();
    digest.update(fs::read(path).expect("读取文件"));
    format!("{:x}", digest.finalize())
}

/// 读取结果 TSV 的文本行，供行为断言检查稳定排序和固定列。
fn read_lines(path: &Path) -> Vec<String> {
    String::from_utf8(fs::read(path).expect("读取 TSV"))
        .expect("TSV UTF-8")
        .split('\n')
        .filter(|line| !line.is_empty())
        .map(str::to_owned)
        .collect()
}

/// 按 header 名称返回一行的字段值。
fn field<'a>(header: &'a [&str], row: &'a [&str], name: &str) -> &'a str {
    row[header
        .iter()
        .position(|value| *value == name)
        .expect("字段必须存在")]
}

#[test]
fn exports_active_files_from_media_roots_without_task_rows_as_sorted_tsv() {
    let fixture = Fixture::new();
    fixture.content(1, 0x01, 101, "other", 1);
    fixture.content(2, 0x02, 102, "other", 1);
    fixture.content(3, 0x03, 103, "other", 1);
    fixture.content(4, 0x04, 104, "other", 1);
    fixture.file("machine-a", r"C:\Media\z.mp3", 1, 101, 1);
    fixture.file("machine-a", r"C:\Media\a.mp3", 2, 102, 1);
    fixture.file("machine-a", r"D:\Other\outside.mp3", 3, 103, 1);
    fixture.file("machine-a", r"C:\Media\inactive.mp3", 4, 104, 0);
    let database_hash = sha256_file(&fixture.database);
    let output = fixture.output("basic");
    let result = fixture.export(
        &[PathBuf::from(r"C:\Media"), PathBuf::from(r"D:\Other")],
        "basic",
    );

    assert_eq!(result.status, ResultSummaryStatus::Pass);
    assert_eq!(result.row_count, 3);
    assert_eq!(result.missing_count, 0);
    assert_eq!(result.inconclusive_count, 0);
    validate_result_summary(&output).expect("固定 TSV 必须可验证");
    let lines = read_lines(&output);
    assert_eq!(lines.len(), 5, "头 + 3 行数据 + footer");
    assert!(lines[0].starts_with("record_type\tstatus\t"));
    assert!(lines[1].contains(r"C:\Media\a.mp3"));
    assert!(lines[2].contains(r"C:\Media\z.mp3"));
    assert!(lines[3].contains(r"D:\Other\outside.mp3"));
    assert!(lines[4].starts_with("F\t3\t"));
    assert!(
        !lines
            .iter()
            .any(|line| line.contains('{') || line.contains("task_id"))
    );
    assert!(!output.with_extension("idx").exists());
    assert_eq!(sha256_file(&fixture.database), database_hash);
    let connection = Connection::open(&fixture.database).expect("读取数据库");
    assert_eq!(
        connection
            .query_row("SELECT COUNT(*) FROM tasks", [], |row| row.get::<_, i64>(0))
            .expect("读取任务数"),
        0
    );
}

#[test]
fn overlapping_media_roots_do_not_duplicate_an_active_file() {
    let fixture = Fixture::new();
    fixture.content(1, 0x11, 111, "other", 1);
    fixture.file("machine-a", r"C:\Media\Sub\one.bin", 1, 111, 1);
    let result = fixture.export(
        &[PathBuf::from(r"C:\Media"), PathBuf::from(r"C:\Media\Sub")],
        "overlap",
    );
    assert_eq!(result.status, ResultSummaryStatus::Pass);
    assert_eq!(result.row_count, 1);
    assert_eq!(read_lines(&result.output_path).len(), 3);
}

#[test]
fn missing_and_inconclusive_feature_rows_are_reported_without_fake_hashes() {
    let fixture = Fixture::new();
    fixture.content(10, 0x10, 110, "image", 1);
    fixture.content(11, 0x11, 111, "image", 1);
    fixture.file("machine-a", r"C:\Media\missing.jpg", 10, 110, 1);
    fixture.file("machine-a", r"C:\Media\partial.jpg", 11, 111, 1);
    fixture.image_stage1(11, None, Some(20), Some(vec![1; 32]), Some(80));
    let result = fixture.export(&[PathBuf::from(r"C:\Media")], "states");
    assert_eq!(result.status, ResultSummaryStatus::Inconclusive);
    assert_eq!(result.row_count, 2);
    assert_eq!(result.missing_count, 1);
    assert_eq!(result.inconclusive_count, 1);
    let lines = read_lines(&result.output_path);
    let header = lines[0].split('\t').collect::<Vec<_>>();
    let missing = lines
        .iter()
        .find(|line| line.contains("missing.jpg"))
        .expect("缺失行");
    let partial = lines
        .iter()
        .find(|line| line.contains("partial.jpg"))
        .expect("不确定行");
    let missing = missing.split('\t').collect::<Vec<_>>();
    let partial = partial.split('\t').collect::<Vec<_>>();
    assert_eq!(field(&header, &missing, "status"), "MISSING");
    assert_eq!(field(&header, &partial, "status"), "INCONCLUSIVE");
    assert!(field(&header, &partial, "image_stage1_sha256").len() == 64);
    assert_eq!(field(&header, &missing, "md5").len(), 32);
}

#[test]
fn exports_all_feature_payload_hashes_and_contact_sheet_hash() {
    let fixture = Fixture::new();
    fixture.content(20, 0x20, 120, "image", 1);
    fixture.content(21, 0x21, 121, "video", 1);
    fixture.file("machine-a", r"C:\Media\image.jpg", 20, 120, 1);
    fixture.file("machine-a", r"C:\Media\video.mp4", 21, 121, 1);
    fixture.image_stage1(20, Some(1920), Some(1080), Some(vec![1; 32]), Some(80));
    fixture.image_stage2(20);
    fixture.contact_sheet(20, "contact-sheets/image.jpg", b"image-contact");
    fixture.video_features(21);
    fixture.contact_sheet(21, "contact-sheets/video.jpg", b"video-contact");
    let result = fixture.export(&[PathBuf::from(r"C:\Media")], "features");
    assert_eq!(result.status, ResultSummaryStatus::Pass);
    let lines = read_lines(&result.output_path);
    let header = lines[0].split('\t').collect::<Vec<_>>();
    let image = lines
        .iter()
        .find(|line| line.contains("image.jpg"))
        .expect("图片行")
        .split('\t')
        .map(str::to_owned)
        .collect::<Vec<_>>();
    let video = lines
        .iter()
        .find(|line| line.contains("video.mp4"))
        .expect("视频行")
        .split('\t')
        .map(str::to_owned)
        .collect::<Vec<_>>();
    for name in [
        "feature_payload_sha256",
        "image_stage1_sha256",
        "image_stage2_sha256",
        "contact_sheet_sha256",
    ] {
        assert_eq!(
            field(
                &header,
                &image.iter().map(String::as_str).collect::<Vec<_>>(),
                name
            )
            .len(),
            64
        );
    }
    assert_eq!(
        field(
            &header,
            &image.iter().map(String::as_str).collect::<Vec<_>>(),
            "thumbnail_sha256"
        ),
        ""
    );
    assert_eq!(
        field(
            &header,
            &image.iter().map(String::as_str).collect::<Vec<_>>(),
            "thumbnail_state"
        ),
        "unsupported_no_thumbnail_artifact"
    );
    for name in [
        "feature_payload_sha256",
        "video_metadata_sha256",
        "video_frame_stage1_0_sha256",
        "video_frame_stage2_0_sha256",
        "contact_sheet_sha256",
    ] {
        assert_eq!(
            field(
                &header,
                &video.iter().map(String::as_str).collect::<Vec<_>>(),
                name
            )
            .len(),
            64
        );
    }
}

#[test]
fn export_ignores_legacy_task_rows_and_does_not_write_database_or_cache() {
    let fixture = Fixture::new();
    fixture.content(30, 0x30, 130, "other", 1);
    fixture.file("machine-a", r"C:\Media\legacy-independent.bin", 30, 130, 1);
    fixture.execute(
        "INSERT INTO tasks(task_id,kind,status,total_items,created_at_ms,updated_at_ms)
         VALUES('legacy-task','base_compute','running',1,1,1)",
        [],
    );
    let database_hash = sha256_file(&fixture.database);
    let result = fixture.export(&[PathBuf::from(r"C:\Media")], "legacy");
    assert_eq!(result.status, ResultSummaryStatus::Pass);
    assert_eq!(sha256_file(&fixture.database), database_hash);
    let connection = Connection::open(&fixture.database).expect("读取数据库");
    assert_eq!(
        connection
            .query_row(
                "SELECT status FROM tasks WHERE task_id='legacy-task'",
                [],
                |row| row.get::<_, String>(0)
            )
            .expect("任务仍存在"),
        "running"
    );
}

#[test]
fn first_read_open_wal_sidecar_initialization_is_not_reported_as_external_change() {
    let fixture = Fixture::new();
    fixture.content(40, 0x40, 140, "other", 1);
    fixture.file("machine-a", r"C:\Media\wal.bin", 40, 140, 1);
    let writer = fixture.prepare_wal();
    let [wal, shm] = sidecar_paths_for_acceptance(&fixture.database);
    assert!(wal.exists(), "WAL 必须存在");
    assert!(shm.exists(), "SHM 必须存在");
    let result = fixture.export(&[PathBuf::from(r"C:\Media")], "wal");
    drop(writer);
    assert_eq!(result.status, ResultSummaryStatus::Pass);
    validate_result_summary(&result.output_path).expect("首次打开摘要必须有效");
}

/// 在 sidecar 捕获后注入文件变化，验证导出不会发布不一致 TSV。
fn mutate_wal_after_capture(database_path: &Path) {
    let [wal, _] = sidecar_paths_for_acceptance(database_path);
    fs::write(wal, b"sidecar-mutated-after-capture").expect("修改 WAL");
}

#[test]
fn sidecar_mutation_after_capture_is_rejected_before_output_publish() {
    let fixture = Fixture::new();
    fixture.content(50, 0x50, 150, "other", 1);
    fixture.file("machine-a", r"C:\Media\sidecar.bin", 50, 150, 1);
    let writer = fixture.prepare_wal();
    let output = fixture.output("sidecar-mutated");
    set_result_summary_read_test_callback(Some(mutate_wal_after_capture));
    set_result_summary_read_test_hook(ResultSummaryReadTestHook::AfterSidecarCapture);
    let error = export_scan_result_summary(
        &fixture.database,
        &fixture.cache_root,
        &[PathBuf::from(r"C:\Media")],
        &output,
    )
    .expect_err("sidecar 变化必须拒绝发布");
    set_result_summary_read_test_hook(ResultSummaryReadTestHook::None);
    set_result_summary_read_test_callback(None);
    drop(writer);
    assert!(matches!(error, ResultSummaryError::InvalidArgument(_)));
    assert!(!output.exists(), "拒绝时不得发布半成品 TSV");
}

#[test]
fn validator_rejects_bom_and_footer_tampering() {
    let fixture = Fixture::new();
    fixture.content(60, 0x60, 160, "other", 1);
    fixture.file("machine-a", r"C:\Media\valid.bin", 60, 160, 1);
    let result = fixture.export(&[PathBuf::from(r"C:\Media")], "validator");
    let original = fs::read(&result.output_path).expect("读取原 TSV");
    fs::write(
        &result.output_path,
        [&[0xEF, 0xBB, 0xBF][..], &original[..]].concat(),
    )
    .expect("写 BOM");
    assert!(matches!(
        validate_result_summary(&result.output_path),
        Err(ResultSummaryError::InvalidOutput(_))
    ));
    fs::write(&result.output_path, original).expect("恢复 TSV");
    let mut tampered = fs::read(&result.output_path).expect("读取 TSV");
    let last = tampered.len() - 2;
    tampered[last] = if tampered[last] == b'0' { b'1' } else { b'0' };
    fs::write(&result.output_path, tampered).expect("篡改 footer");
    assert!(matches!(
        validate_result_summary(&result.output_path),
        Err(ResultSummaryError::InvalidOutput(_))
    ));
}
