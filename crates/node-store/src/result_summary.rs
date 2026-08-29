//! 读取节点长期缓存并导出固定 TSV 结果摘要；不读取或写入任何任务运行态表。

use std::{
    fs::{self, File, OpenOptions},
    io::{Read, Write},
    path::{Path, PathBuf},
    sync::atomic::{AtomicU64, Ordering},
};

#[cfg(feature = "acceptance-tools")]
use std::sync::{Mutex, OnceLock};

use dedup_core::NormalizedPath;
use rusqlite::{Connection, OpenFlags, OptionalExtension, params};
use sha2::{Digest, Sha256};
use thiserror::Error;

/// 一次只读结果导出的整体状态。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ResultSummaryStatus {
    /// 所有活动文件的内容和必需基础特征均完整。
    Pass,
    /// 没有匹配媒体根的活动文件，或存在缺失内容/特征/artifact。
    Missing,
    /// 数据库字段或特征 payload 互相矛盾，无法裁决结果。
    Inconclusive,
}

impl ResultSummaryStatus {
    /// 返回 CLI 和 TSV 诊断使用的稳定大写状态名。
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Pass => "PASS",
            Self::Missing => "MISSING",
            Self::Inconclusive => "INCONCLUSIVE",
        }
    }
}

/// 导出结果文件及固定 footer 的统计信息。
#[derive(Debug)]
pub struct ResultSummaryExport {
    /// 本次导出的整体状态。
    pub status: ResultSummaryStatus,
    /// TSV 中 `R` 数据行数。
    pub row_count: u64,
    /// 状态为 `MISSING` 的数据行数。
    pub missing_count: u64,
    /// 状态为 `INCONCLUSIVE` 的数据行数。
    pub inconclusive_count: u64,
    /// 固定 `result-summary.tsv` 输出路径。
    pub output_path: PathBuf,
    /// 完整 TSV 文件（包含 footer 和最终 LF）的 SHA-256。
    pub sha256: String,
}

/// 导出器的只读数据库、文件、参数和 TSV 校验错误。
#[derive(Debug, Error)]
pub enum ResultSummaryError {
    /// SQLite 只读连接、事务或查询失败。
    #[error("SQLite 读取失败: {0}")]
    Sqlite(#[from] rusqlite::Error),
    /// 数据库、artifact 或输出文件读写失败。
    #[error("文件读取或写入失败: {0}")]
    Io(#[from] std::io::Error),
    /// 输出路径、媒体根或数据库边界无效。
    #[error("导出参数无效: {0}")]
    InvalidArgument(String),
    /// 输出 TSV 不是固定格式或 footer 校验失败。
    #[error("结果 TSV 校验失败: {0}")]
    InvalidOutput(String),
    /// 当前平台无法证明 artifact 文件身份。
    #[error("当前平台不支持文件身份校验")]
    UnsupportedFileIdentity,
    /// 联系表路径越出隔离缓存根或包含重解析点。
    #[error("联系表路径越出隔离缓存根")]
    UnsafeArtifactPath,
}

const TSV_COLUMNS: [&str; 29] = [
    "record_type",
    "status",
    "machine_id",
    "normalized_path",
    "display_path",
    "file_size",
    "md5",
    "media_type",
    "base_complete",
    "feature_payload_sha256",
    "image_stage1_sha256",
    "image_stage2_sha256",
    "video_metadata_sha256",
    "video_frame_stage1_0_sha256",
    "video_frame_stage1_1_sha256",
    "video_frame_stage1_2_sha256",
    "video_frame_stage1_3_sha256",
    "video_frame_stage1_4_sha256",
    "video_frame_stage1_5_sha256",
    "video_frame_stage2_0_sha256",
    "video_frame_stage2_1_sha256",
    "video_frame_stage2_2_sha256",
    "video_frame_stage2_3_sha256",
    "video_frame_stage2_4_sha256",
    "video_frame_stage2_5_sha256",
    "thumbnail_sha256",
    "thumbnail_state",
    "contact_sheet_sha256",
    "status_reason",
];

/// 固定 TSV 头行；字段顺序是对外协议的一部分。
fn tsv_header() -> String {
    format!("{}\n", TSV_COLUMNS.join("\t"))
}

/// 只读打开 SQLite；打开动作先完成，随后才捕获 WAL/SHM 快照。
fn open_read_only_database(path: &Path) -> Result<Connection, ResultSummaryError> {
    let connection = Connection::open_with_flags(
        path,
        OpenFlags::SQLITE_OPEN_READ_ONLY | OpenFlags::SQLITE_OPEN_NO_MUTEX,
    )?;
    connection.execute_batch("PRAGMA query_only = ON; PRAGMA busy_timeout = 5000;")?;
    Ok(connection)
}

/// acceptance-tools 可控制的读取阶段，验证首次 SQLite 打开不会误报 sidecar 变化。
#[cfg(feature = "acceptance-tools")]
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
#[repr(u8)]
pub enum ResultSummaryReadTestHook {
    /// 不注入 sidecar 变化。
    None = 0,
    /// 只读连接建立后、sidecar 快照建立前。
    AfterDatabaseOpenBeforeSidecarCapture = 1,
    /// sidecar 快照建立后、数据库查询前。
    AfterSidecarCapture = 2,
}

#[cfg(feature = "acceptance-tools")]
static RESULT_SUMMARY_READ_TEST_HOOK: AtomicU64 = AtomicU64::new(0);

#[cfg(feature = "acceptance-tools")]
static RESULT_SUMMARY_READ_TEST_CALLBACK: OnceLock<Mutex<Option<fn(&Path)>>> = OnceLock::new();

/// 设置下一次导出的一次性读取阶段测试 hook。
#[cfg(feature = "acceptance-tools")]
pub fn set_result_summary_read_test_hook(hook: ResultSummaryReadTestHook) {
    RESULT_SUMMARY_READ_TEST_HOOK.store(hook as u64, Ordering::SeqCst);
}

/// 注册仅由验收测试使用的 sidecar 变化回调。
#[cfg(feature = "acceptance-tools")]
pub fn set_result_summary_read_test_callback(callback: Option<fn(&Path)>) {
    let slot = RESULT_SUMMARY_READ_TEST_CALLBACK.get_or_init(|| Mutex::new(None));
    *slot.lock().expect("测试回调锁不应中毒") = callback;
}

/// 只消费匹配读取阶段，避免测试故障泄漏到下一次导出。
#[cfg(feature = "acceptance-tools")]
fn run_result_summary_read_test_hook(hook: ResultSummaryReadTestHook, database_path: &Path) {
    if RESULT_SUMMARY_READ_TEST_HOOK
        .compare_exchange(
            hook as u64,
            ResultSummaryReadTestHook::None as u64,
            Ordering::SeqCst,
            Ordering::SeqCst,
        )
        .is_ok()
        && let Some(callback) = RESULT_SUMMARY_READ_TEST_CALLBACK
            .get()
            .and_then(|slot| slot.lock().expect("测试回调锁不应中毒").take())
    {
        callback(database_path);
    }
}

/// 返回 SQLite 主库和 WAL/SHM sidecar 路径；不存在的 sidecar 保留为 None。
#[cfg(feature = "acceptance-tools")]
pub fn sidecar_paths_for_acceptance(database_path: &Path) -> [PathBuf; 2] {
    sqlite_sidecar_paths(database_path)
}

/// SQLite 主库及 WAL/SHM 的内容快照。
#[derive(Debug)]
struct SidecarSnapshot {
    database_path: PathBuf,
    database_hash: String,
    entries: Vec<(PathBuf, Option<String>)>,
}

/// 组合 SQLite sidecar 文件名而不经过 UTF-8 重编码。
fn sqlite_sidecar_paths(database_path: &Path) -> [PathBuf; 2] {
    let mut wal = database_path.as_os_str().to_os_string();
    wal.push("-wal");
    let mut shm = database_path.as_os_str().to_os_string();
    shm.push("-shm");
    [PathBuf::from(wal), PathBuf::from(shm)]
}

/// 读取 sidecar 内容 hash；只有 NotFound 才表示 sidecar 不存在。
fn sidecar_hash(path: &Path) -> Result<Option<String>, ResultSummaryError> {
    match sha256_file_path(path) {
        Ok(hash) => Ok(Some(hash)),
        Err(ResultSummaryError::Io(error)) if error.kind() == std::io::ErrorKind::NotFound => {
            Ok(None)
        }
        Err(error) => Err(error),
    }
}

/// 在只读连接建立之后捕获数据库和 sidecar 内容。
fn capture_sidecars(database_path: &Path) -> Result<SidecarSnapshot, ResultSummaryError> {
    let entries = sqlite_sidecar_paths(database_path)
        .into_iter()
        .map(|path| Ok((path.clone(), sidecar_hash(&path)?)))
        .collect::<Result<Vec<_>, ResultSummaryError>>()?;
    Ok(SidecarSnapshot {
        database_path: database_path.to_owned(),
        database_hash: sha256_file_path(database_path)?,
        entries,
    })
}

/// 只读查询期间复核主库和 WAL/SHM 内容未被外部修改。
fn verify_sidecars(snapshot: &SidecarSnapshot) -> Result<(), ResultSummaryError> {
    if sha256_file_path(&snapshot.database_path)? != snapshot.database_hash {
        return Err(ResultSummaryError::InvalidArgument(
            "SQLite 主数据库在只读导出期间发生变化".into(),
        ));
    }
    for (path, expected_hash) in &snapshot.entries {
        if &sidecar_hash(path)? != expected_hash {
            return Err(ResultSummaryError::InvalidArgument(
                "SQLite WAL/SHM sidecar 在只读导出期间发生变化".into(),
            ));
        }
    }
    Ok(())
}

/// 以一个或多个规范媒体根导出固定 TSV；不会读取 task/task_items，也不会写 SQLite。
pub fn export_scan_result_summary(
    database_path: &Path,
    cache_root: &Path,
    media_roots: &[PathBuf],
    output_path: &Path,
) -> Result<ResultSummaryExport, ResultSummaryError> {
    let roots = validate_arguments(database_path, cache_root, media_roots, output_path)?;
    let canonical_cache_root = canonical_cache_root(cache_root)?;
    let connection = open_read_only_database(database_path)?;
    #[cfg(feature = "acceptance-tools")]
    run_result_summary_read_test_hook(
        ResultSummaryReadTestHook::AfterDatabaseOpenBeforeSidecarCapture,
        database_path,
    );
    // 先启动只读事务，让 SQLite 完成首次 WAL-index 初始化，再冻结 sidecar。
    connection.execute_batch("BEGIN;")?;
    // 先执行一次真实 schema 读取；仅 BEGIN 不一定触发 WAL-index 完整初始化。
    connection.query_row("SELECT COUNT(*) FROM files WHERE active=1", [], |row| {
        row.get::<_, i64>(0)
    })?;
    let sidecars = capture_sidecars(database_path)?;
    #[cfg(feature = "acceptance-tools")]
    run_result_summary_read_test_hook(
        ResultSummaryReadTestHook::AfterSidecarCapture,
        database_path,
    );

    let mut rows = load_active_files(&connection)?;
    rows.retain(|row| {
        NormalizedPath::new(&row.normalized_path)
            .map(|path| roots.iter().any(|root| path.is_within(root)))
            .unwrap_or(false)
    });
    rows.sort_by(|left, right| {
        left.normalized_path
            .cmp(&right.normalized_path)
            .then_with(|| left.machine_id.cmp(&right.machine_id))
            .then_with(|| left.display_path.cmp(&right.display_path))
    });

    let header = tsv_header();
    let mut data_bytes = header.into_bytes();

    let mut row_count = 0_u64;
    let mut missing_count = 0_u64;
    let mut inconclusive_count = 0_u64;
    for file in &rows {
        let row = build_result_row(&connection, &canonical_cache_root, file)?;
        match row.status.as_str() {
            "MISSING" => missing_count += 1,
            "INCONCLUSIVE" => inconclusive_count += 1,
            _ => {}
        }
        let line = row.to_tsv_line();
        data_bytes.extend_from_slice(line.as_bytes());
        row_count += 1;
    }
    connection.execute_batch("COMMIT;")?;
    drop(connection);
    verify_sidecars(&sidecars)?;

    let data_hash = sha256_hex(&data_bytes);
    let footer = format!("F\t{row_count}\t{data_hash}\n");
    let mut output_bytes = data_bytes;
    output_bytes.extend_from_slice(footer.as_bytes());
    let mut file = OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(output_path)?;
    file.write_all(&output_bytes)?;
    file.sync_all()?;
    drop(file);

    validate_result_summary(output_path)?;
    let status = if inconclusive_count > 0 {
        ResultSummaryStatus::Inconclusive
    } else if missing_count > 0 || row_count == 0 {
        ResultSummaryStatus::Missing
    } else {
        ResultSummaryStatus::Pass
    };
    Ok(ResultSummaryExport {
        status,
        row_count,
        missing_count,
        inconclusive_count,
        output_path: output_path.to_owned(),
        sha256: sha256_hex(&output_bytes),
    })
}

/// 校验媒体根、只读数据库和固定输出文件边界，并返回规范根。
fn validate_arguments(
    database_path: &Path,
    cache_root: &Path,
    media_roots: &[PathBuf],
    output_path: &Path,
) -> Result<Vec<NormalizedPath>, ResultSummaryError> {
    if database_path.as_os_str().is_empty() {
        return Err(ResultSummaryError::InvalidArgument(
            "database path 不能为空".into(),
        ));
    }
    if media_roots.is_empty() {
        return Err(ResultSummaryError::InvalidArgument(
            "至少需要一个 media root".into(),
        ));
    }
    let mut roots = media_roots
        .iter()
        .map(|root| {
            NormalizedPath::new(root).map_err(|_| {
                ResultSummaryError::InvalidArgument(format!(
                    "media root 不是规范绝对路径: {}",
                    root.display()
                ))
            })
        })
        .collect::<Result<Vec<_>, _>>()?;
    roots.sort();
    roots.dedup();

    let cache_root = canonical_cache_root(cache_root)?;
    let canonical_database = fs::canonicalize(database_path)?;
    if output_path.file_name().and_then(|name| name.to_str()) != Some("result-summary.tsv") {
        return Err(ResultSummaryError::InvalidArgument(
            "输出文件名必须是 result-summary.tsv".into(),
        ));
    }
    let parent = output_path
        .parent()
        .ok_or_else(|| ResultSummaryError::InvalidArgument("输出必须包含父目录".into()))?;
    match fs::metadata(parent) {
        Ok(metadata) if metadata.is_dir() => {}
        Ok(_) => {
            return Err(ResultSummaryError::InvalidArgument(
                "输出父级必须是目录".into(),
            ));
        }
        Err(error) => return Err(ResultSummaryError::Io(error)),
    }
    let canonical_output = canonical_target_path(output_path)?;
    if canonical_output.starts_with(&cache_root) {
        return Err(ResultSummaryError::InvalidArgument(
            "输出不能位于 cache root 内".into(),
        ));
    }
    if canonical_output == canonical_database {
        return Err(ResultSummaryError::InvalidArgument(
            "输出不能覆盖 database".into(),
        ));
    }
    match fs::symlink_metadata(output_path) {
        Ok(_) => Err(ResultSummaryError::InvalidArgument(
            "输出文件已存在，拒绝覆盖".into(),
        )),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(roots),
        Err(error) => Err(ResultSummaryError::Io(error)),
    }
}

/// 规范化尚不存在的输出路径，仅解析已经存在的父级目录。
fn canonical_target_path(path: &Path) -> Result<PathBuf, ResultSummaryError> {
    let parent = path
        .parent()
        .ok_or_else(|| ResultSummaryError::InvalidArgument("输出必须包含父级".into()))?;
    let file_name = path
        .file_name()
        .ok_or_else(|| ResultSummaryError::InvalidArgument("输出必须包含文件名".into()))?;
    Ok(fs::canonicalize(parent)?.join(file_name))
}

/// 解析 cache root 并拒绝 root 自身是重解析点。
fn canonical_cache_root(cache_root: &Path) -> Result<PathBuf, ResultSummaryError> {
    let metadata = fs::symlink_metadata(cache_root)?;
    if is_reparse_point(&metadata) {
        return Err(ResultSummaryError::UnsafeArtifactPath);
    }
    if !metadata.is_dir() {
        return Err(ResultSummaryError::InvalidArgument(
            "cache root 必须是目录".into(),
        ));
    }
    Ok(fs::canonicalize(cache_root)?)
}

/// SQLite active file 与 contents 的单行只读投影；不含任何任务 ID。
#[derive(Clone, Debug)]
struct ActiveFile {
    machine_id: String,
    normalized_path: String,
    display_path: String,
    file_size: i64,
    content_id: Option<i64>,
    content_md5: Option<Vec<u8>>,
    content_file_size: Option<i64>,
    media_kind: Option<String>,
    base_complete: Option<i64>,
}

/// 读取所有 active 文件；根筛选在 Rust 中按路径组件完成，避免 LIKE 前缀误匹配。
fn load_active_files(connection: &Connection) -> Result<Vec<ActiveFile>, ResultSummaryError> {
    let mut statement = connection.prepare(
        "SELECT f.machine_id, f.normalized_path, f.display_path, f.file_size,
                c.content_id, c.md5, c.file_size, c.media_kind, c.base_complete
         FROM files AS f
         LEFT JOIN contents AS c ON c.content_id=f.content_id
         WHERE f.active=1
         ORDER BY f.normalized_path COLLATE BINARY,
                  f.machine_id COLLATE BINARY,
                  f.display_path COLLATE BINARY",
    )?;
    statement
        .query_map([], |row| {
            Ok(ActiveFile {
                machine_id: row.get(0)?,
                normalized_path: row.get(1)?,
                display_path: row.get(2)?,
                file_size: row.get(3)?,
                content_id: row.get(4)?,
                content_md5: row.get(5)?,
                content_file_size: row.get(6)?,
                media_kind: row.get(7)?,
                base_complete: row.get(8)?,
            })
        })?
        .collect::<Result<Vec<_>, _>>()
        .map_err(Into::into)
}

/// 结果行的状态严重度；INCONCLUSIVE 覆盖 MISSING，避免不确定值被当成缺失。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum RowStatus {
    Pass,
    Missing,
    Inconclusive,
}

impl RowStatus {
    /// 返回固定 TSV 状态值。
    const fn as_str(self) -> &'static str {
        match self {
            Self::Pass => "PASS",
            Self::Missing => "MISSING",
            Self::Inconclusive => "INCONCLUSIVE",
        }
    }

    /// 合并一个新的缺失或不确定原因。
    fn add_issue(&mut self, issue: Self) {
        if issue == Self::Inconclusive || (*self == Self::Pass && issue == Self::Missing) {
            *self = issue;
        }
    }
}

/// 每个内容表/槽位的独立 SHA-256；None 表示对应表行不存在。
#[derive(Clone, Debug, Default)]
struct FeatureHashes {
    image_stage1: Option<String>,
    image_stage2: Option<String>,
    video_metadata: Option<String>,
    video_frame_stage1: [Option<String>; 6],
    video_frame_stage2: [Option<String>; 6],
}

impl FeatureHashes {
    /// 将所有独立 payload hash 按固定顺序聚合，避免依赖 JSON 序列化。
    fn aggregate(&self) -> Option<String> {
        let mut bytes = Vec::new();
        let mut append = |value: &Option<String>| {
            if let Some(value) = value {
                bytes.extend_from_slice(value.as_bytes());
            }
            bytes.push(0);
        };
        append(&self.image_stage1);
        append(&self.image_stage2);
        append(&self.video_metadata);
        for value in &self.video_frame_stage1 {
            append(value);
        }
        for value in &self.video_frame_stage2 {
            append(value);
        }
        bytes
            .iter()
            .any(|byte| *byte != 0)
            .then(|| sha256_hex(&bytes))
    }
}

/// 固定 TSV 的一条文件结果；不含 SQLite content_id 或任务 ID。
struct ResultRow {
    status: RowStatus,
    machine_id: String,
    normalized_path: String,
    display_path: String,
    file_size: Option<i64>,
    md5: Option<String>,
    media_type: Option<String>,
    base_complete: Option<bool>,
    feature_hashes: FeatureHashes,
    thumbnail_sha256: Option<String>,
    thumbnail_state: &'static str,
    contact_sheet_sha256: Option<String>,
    reason: String,
}

impl ResultRow {
    /// 编码为固定列数、UTF-8、无 CR 的 TSV 数据行。
    fn to_tsv_line(&self) -> String {
        let mut fields = Vec::with_capacity(TSV_COLUMNS.len());
        fields.push("R".into());
        fields.push(self.status.as_str().into());
        fields.push(escape_tsv(&self.machine_id));
        fields.push(escape_tsv(&self.normalized_path));
        fields.push(escape_tsv(&self.display_path));
        fields.push(
            self.file_size
                .map(|value| value.to_string())
                .unwrap_or_default(),
        );
        fields.push(self.md5.clone().unwrap_or_default());
        fields.push(self.media_type.clone().unwrap_or_default());
        fields.push(
            self.base_complete
                .map(|value| if value { "1" } else { "0" })
                .unwrap_or_default()
                .into(),
        );
        fields.push(self.feature_hashes.aggregate().unwrap_or_default());
        fields.push(self.feature_hashes.image_stage1.clone().unwrap_or_default());
        fields.push(self.feature_hashes.image_stage2.clone().unwrap_or_default());
        fields.push(
            self.feature_hashes
                .video_metadata
                .clone()
                .unwrap_or_default(),
        );
        fields.extend(
            self.feature_hashes
                .video_frame_stage1
                .iter()
                .map(|value| value.clone().unwrap_or_default()),
        );
        fields.extend(
            self.feature_hashes
                .video_frame_stage2
                .iter()
                .map(|value| value.clone().unwrap_or_default()),
        );
        fields.push(self.thumbnail_sha256.clone().unwrap_or_default());
        fields.push(self.thumbnail_state.into());
        fields.push(self.contact_sheet_sha256.clone().unwrap_or_default());
        fields.push(escape_tsv(&self.reason));
        format!("{}\n", fields.join("\t"))
    }
}

/// 读取一条 active file 的内容、所有已存在特征和联系表。
fn build_result_row(
    connection: &Connection,
    cache_root: &Path,
    file: &ActiveFile,
) -> Result<ResultRow, ResultSummaryError> {
    let mut status = RowStatus::Pass;
    let mut reasons: Vec<String> = Vec::new();
    let mut row = ResultRow {
        status,
        machine_id: file.machine_id.clone(),
        normalized_path: file.normalized_path.clone(),
        display_path: file.display_path.clone(),
        file_size: None,
        md5: None,
        media_type: None,
        base_complete: None,
        feature_hashes: FeatureHashes::default(),
        thumbnail_sha256: None,
        thumbnail_state: "unsupported_no_thumbnail_artifact",
        contact_sheet_sha256: None,
        reason: String::new(),
    };

    if file.file_size < 0 {
        status.add_issue(RowStatus::Inconclusive);
        reasons.push("file_size_invalid".into());
    } else {
        row.file_size = Some(file.file_size);
    }
    let Some(content_id) = file.content_id else {
        status.add_issue(RowStatus::Missing);
        reasons.push("missing_content".into());
        row.status = status;
        row.reason = reasons.join(";");
        return Ok(row);
    };
    let (Some(content_md5), Some(content_file_size), Some(media_type), Some(base_complete_raw)) = (
        file.content_md5.as_ref(),
        file.content_file_size,
        file.media_kind.as_ref(),
        file.base_complete,
    ) else {
        status.add_issue(RowStatus::Missing);
        reasons.push("missing_content".into());
        row.status = status;
        row.reason = reasons.join(";");
        return Ok(row);
    };
    if content_file_size < 0 || file.file_size != content_file_size {
        status.add_issue(RowStatus::Inconclusive);
        reasons.push("content_size_mismatch".into());
    }
    if content_md5.len() != 16 {
        status.add_issue(RowStatus::Inconclusive);
        reasons.push("md5_invalid".into());
    } else {
        row.md5 = Some(lower_hex(content_md5));
    }
    let valid_media_type = matches!(media_type.as_str(), "image" | "video" | "other");
    if !valid_media_type {
        status.add_issue(RowStatus::Inconclusive);
        reasons.push("media_type_invalid".into());
    } else {
        row.media_type = Some(media_type.clone());
    }
    match base_complete_raw {
        0 => {
            status.add_issue(RowStatus::Missing);
            reasons.push("base_features_missing".into());
            row.base_complete = Some(false);
        }
        1 => row.base_complete = Some(true),
        _ => {
            status.add_issue(RowStatus::Inconclusive);
            reasons.push("base_complete_invalid".into());
        }
    }

    if valid_media_type {
        let (feature_hashes, feature_status, feature_reasons) =
            load_feature_payloads(connection, content_id, media_type)?;
        row.feature_hashes = feature_hashes;
        status.add_issue(feature_status);
        reasons.extend(feature_reasons);
    }

    let contact_sheet = load_contact_sheet(connection, content_id)?;
    match contact_sheet {
        Some(relative_path) => match read_safe_artifact(cache_root, &relative_path)? {
            ArtifactState::Present(hash) => row.contact_sheet_sha256 = Some(hash),
            ArtifactState::Missing => {
                status.add_issue(RowStatus::Missing);
                reasons.push("contact_sheet_missing".into());
            }
        },
        None if media_type == "video" => {
            status.add_issue(RowStatus::Missing);
            reasons.push("contact_sheet_missing".into());
        }
        None => {}
    }
    row.status = status;
    row.reason = if reasons.is_empty() {
        "complete".into()
    } else {
        reasons.join(";")
    };
    Ok(row)
}

/// 特征 payload 查询结果，状态只反映必需基础数据，不强制已有二筛。
fn load_feature_payloads(
    connection: &Connection,
    content_id: i64,
    media_type: &str,
) -> Result<(FeatureHashes, RowStatus, Vec<String>), ResultSummaryError> {
    let mut hashes = FeatureHashes::default();
    let mut status = RowStatus::Pass;
    let mut reasons = Vec::new();
    match media_type {
        "image" => {
            let stage1: Option<(Option<i64>, Option<i64>, Option<Vec<u8>>, Option<i64>)> =
                connection
                    .query_row(
                        "SELECT width,height,pdq,quality FROM image_stage1 WHERE content_id=?1",
                        [content_id],
                        |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?, row.get(3)?)),
                    )
                    .optional()?;
            match stage1 {
                Some((width, height, pdq, quality)) => {
                    hashes.image_stage1 =
                        Some(hash_image_stage1(width, height, pdq.as_deref(), quality));
                    if width.is_none()
                        || height.is_none()
                        || quality.is_none()
                        || pdq.as_ref().is_none_or(|value| value.len() != 32)
                    {
                        status.add_issue(RowStatus::Inconclusive);
                        reasons.push("image_stage1_incomplete".into());
                    }
                }
                None => {
                    status.add_issue(RowStatus::Missing);
                    reasons.push("image_stage1_missing".into());
                }
            }
            let stage2: Option<(Option<Vec<u8>>, Option<Vec<u8>>)> = connection
                .query_row(
                    "SELECT phash_parts,sobel FROM image_stage2 WHERE content_id=?1",
                    [content_id],
                    |row| Ok((row.get(0)?, row.get(1)?)),
                )
                .optional()?;
            if let Some((phash_parts, sobel)) = stage2 {
                hashes.image_stage2 =
                    Some(hash_image_stage2(phash_parts.as_deref(), sobel.as_deref()));
                if phash_parts.as_ref().is_some_and(|value| value.len() != 72)
                    || sobel.as_ref().is_some_and(|value| value.len() != 512)
                {
                    status.add_issue(RowStatus::Inconclusive);
                    reasons.push("image_stage2_invalid".into());
                }
            }
        }
        "video" => {
            let metadata: Option<(Option<i64>, Option<i64>, Option<i64>)> = connection
                .query_row(
                    "SELECT duration_ms,width,height FROM video_metadata WHERE content_id=?1",
                    [content_id],
                    |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?)),
                )
                .optional()?;
            match metadata {
                Some((duration_ms, width, height)) => {
                    hashes.video_metadata = Some(hash_video_metadata(duration_ms, width, height));
                    if duration_ms.is_none() || width.is_none() || height.is_none() {
                        status.add_issue(RowStatus::Inconclusive);
                        reasons.push("video_metadata_incomplete".into());
                    }
                }
                None => {
                    status.add_issue(RowStatus::Missing);
                    reasons.push("video_metadata_missing".into());
                }
            }
            let mut decoded_count = 0_usize;
            for slot in 0_i64..6 {
                let frame: Option<(
                    i64,
                    i64,
                    Option<i64>,
                    Option<i64>,
                    Option<Vec<u8>>,
                    Option<i64>,
                )> = connection
                    .query_row(
                        "SELECT time_ms,decoded,width,height,pdq,quality
                         FROM video_frame_stage1 WHERE content_id=?1 AND slot=?2",
                        params![content_id, slot],
                        |row| {
                            Ok((
                                row.get(0)?,
                                row.get(1)?,
                                row.get(2)?,
                                row.get(3)?,
                                row.get(4)?,
                                row.get(5)?,
                            ))
                        },
                    )
                    .optional()?;
                match frame {
                    Some((time_ms, decoded, width, height, pdq, quality)) => {
                        hashes.video_frame_stage1[slot as usize] = Some(hash_video_frame_stage1(
                            slot,
                            time_ms,
                            decoded,
                            width,
                            height,
                            pdq.as_deref(),
                            quality,
                        ));
                        if decoded == 1 {
                            decoded_count += 1;
                            if width.is_none()
                                || height.is_none()
                                || quality.is_none()
                                || pdq.as_ref().is_none_or(|value| value.len() != 32)
                            {
                                status.add_issue(RowStatus::Inconclusive);
                                reasons.push(format!("video_frame_stage1_{slot}_incomplete"));
                            }
                        } else if decoded != 0 {
                            status.add_issue(RowStatus::Inconclusive);
                            reasons.push(format!("video_frame_stage1_{slot}_invalid"));
                        }
                    }
                    None => {
                        status.add_issue(RowStatus::Missing);
                        reasons.push(format!("video_frame_stage1_{slot}_missing"));
                    }
                }
            }
            if decoded_count < 4 {
                status.add_issue(RowStatus::Missing);
                reasons.push("video_stage1_success_count_low".into());
            }
            for slot in 0_i64..6 {
                let frame: Option<(Option<Vec<u8>>, Option<Vec<u8>>)> = connection
                    .query_row(
                        "SELECT phash_parts,sobel FROM video_frame_stage2
                         WHERE content_id=?1 AND slot=?2",
                        params![content_id, slot],
                        |row| Ok((row.get(0)?, row.get(1)?)),
                    )
                    .optional()?;
                if let Some((phash_parts, sobel)) = frame {
                    hashes.video_frame_stage2[slot as usize] = Some(hash_video_frame_stage2(
                        slot,
                        phash_parts.as_deref(),
                        sobel.as_deref(),
                    ));
                    if phash_parts.as_ref().is_some_and(|value| value.len() != 72)
                        || sobel.as_ref().is_some_and(|value| value.len() != 512)
                    {
                        status.add_issue(RowStatus::Inconclusive);
                        reasons.push(format!("video_frame_stage2_{slot}_invalid"));
                    }
                }
            }
        }
        "other" => {}
        _ => unreachable!("调用方已校验媒体类型"),
    }
    Ok((hashes, status, reasons))
}

/// 固定字段编码，避免 JSON 序列化和解析成本。
fn payload_bytes(tag: &[u8], fields: &[PayloadField<'_>]) -> Vec<u8> {
    let mut bytes = Vec::new();
    bytes.extend_from_slice(tag);
    bytes.push(0);
    for field in fields {
        match field {
            PayloadField::Int(value) => match value {
                Some(value) => {
                    bytes.push(1);
                    bytes.extend_from_slice(&value.to_le_bytes());
                }
                None => bytes.push(0),
            },
            PayloadField::Bytes(value) => match value {
                Some(value) => {
                    bytes.push(1);
                    bytes.extend_from_slice(&(value.len() as u64).to_le_bytes());
                    bytes.extend_from_slice(value);
                }
                None => bytes.push(0),
            },
        }
    }
    bytes
}

/// payload 的可选整数或原始 BLOB 字段。
enum PayloadField<'a> {
    Int(Option<i64>),
    Bytes(Option<&'a [u8]>),
}

/// 计算图片一筛 payload hash。
fn hash_image_stage1(
    width: Option<i64>,
    height: Option<i64>,
    pdq: Option<&[u8]>,
    quality: Option<i64>,
) -> String {
    sha256_hex(&payload_bytes(
        b"image_stage1",
        &[
            PayloadField::Int(width),
            PayloadField::Int(height),
            PayloadField::Bytes(pdq),
            PayloadField::Int(quality),
        ],
    ))
}

/// 计算图片二筛 payload hash。
fn hash_image_stage2(phash_parts: Option<&[u8]>, sobel: Option<&[u8]>) -> String {
    sha256_hex(&payload_bytes(
        b"image_stage2",
        &[PayloadField::Bytes(phash_parts), PayloadField::Bytes(sobel)],
    ))
}

/// 计算视频 metadata payload hash。
fn hash_video_metadata(
    duration_ms: Option<i64>,
    width: Option<i64>,
    height: Option<i64>,
) -> String {
    sha256_hex(&payload_bytes(
        b"video_metadata",
        &[
            PayloadField::Int(duration_ms),
            PayloadField::Int(width),
            PayloadField::Int(height),
        ],
    ))
}

/// 计算视频一筛槽位 payload hash。
fn hash_video_frame_stage1(
    slot: i64,
    time_ms: i64,
    decoded: i64,
    width: Option<i64>,
    height: Option<i64>,
    pdq: Option<&[u8]>,
    quality: Option<i64>,
) -> String {
    sha256_hex(&payload_bytes(
        b"video_frame_stage1",
        &[
            PayloadField::Int(Some(slot)),
            PayloadField::Int(Some(time_ms)),
            PayloadField::Int(Some(decoded)),
            PayloadField::Int(width),
            PayloadField::Int(height),
            PayloadField::Bytes(pdq),
            PayloadField::Int(quality),
        ],
    ))
}

/// 计算视频二筛槽位 payload hash。
fn hash_video_frame_stage2(slot: i64, phash_parts: Option<&[u8]>, sobel: Option<&[u8]>) -> String {
    sha256_hex(&payload_bytes(
        b"video_frame_stage2",
        &[
            PayloadField::Int(Some(slot)),
            PayloadField::Bytes(phash_parts),
            PayloadField::Bytes(sobel),
        ],
    ))
}

/// 读取 contact sheet 关联路径。
fn load_contact_sheet(
    connection: &Connection,
    content_id: i64,
) -> Result<Option<String>, ResultSummaryError> {
    connection
        .query_row(
            "SELECT relative_path FROM contact_sheets WHERE content_id=?1",
            [content_id],
            |row| row.get(0),
        )
        .optional()
        .map_err(Into::into)
}

/// 联系表读取状态；缺失不伪造 hash。
enum ArtifactState {
    Present(String),
    Missing,
}

/// 校验 cache root 内的相对 artifact 路径并流式读取 hash。
fn read_safe_artifact(
    cache_root: &Path,
    relative_path: &str,
) -> Result<ArtifactState, ResultSummaryError> {
    if relative_path.trim().is_empty() || has_unsafe_relative_component(relative_path) {
        return Err(ResultSummaryError::UnsafeArtifactPath);
    }
    let candidate = cache_root.join(relative_path);
    let mut component = candidate.clone();
    loop {
        match fs::symlink_metadata(&component) {
            Ok(metadata) if is_reparse_point(&metadata) => {
                return Err(ResultSummaryError::UnsafeArtifactPath);
            }
            Ok(_) => {}
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                return Ok(ArtifactState::Missing);
            }
            Err(error) => return Err(ResultSummaryError::Io(error)),
        }
        if component == *cache_root || !component.pop() {
            break;
        }
    }
    let canonical_candidate = fs::canonicalize(&candidate)?;
    if !canonical_candidate.starts_with(cache_root) {
        return Err(ResultSummaryError::UnsafeArtifactPath);
    }
    let metadata = fs::metadata(&candidate)?;
    if !metadata.is_file() {
        return Err(ResultSummaryError::UnsafeArtifactPath);
    }
    Ok(ArtifactState::Present(sha256_file_path(&candidate)?))
}

/// 拒绝绝对路径、父级组件和盘符绕过。
fn has_unsafe_relative_component(relative_path: &str) -> bool {
    let normalized = relative_path.replace('\\', "/");
    if Path::new(relative_path).is_absolute()
        || normalized.starts_with('/')
        || (normalized.len() >= 2
            && normalized.as_bytes()[1] == b':'
            && normalized.as_bytes()[0].is_ascii_alphabetic())
    {
        return true;
    }
    normalized.split('/').any(|part| part == "..")
}

/// 判断路径是否带有 Windows reparse point 或 Unix symlink。
fn is_reparse_point(metadata: &fs::Metadata) -> bool {
    #[cfg(windows)]
    {
        use std::os::windows::fs::MetadataExt;
        const FILE_ATTRIBUTE_REPARSE_POINT: u32 = 0x0400;
        metadata.file_attributes() & FILE_ATTRIBUTE_REPARSE_POINT != 0
    }
    #[cfg(not(windows))]
    {
        metadata.file_type().is_symlink()
    }
}

/// 对 TSV 字段中的控制字符做固定转义，保持一行一条记录。
fn escape_tsv(value: &str) -> String {
    let mut escaped = String::with_capacity(value.len());
    for character in value.chars() {
        match character {
            '\t' => escaped.push_str("\\x09"),
            '\r' => escaped.push_str("\\x0d"),
            '\n' => escaped.push_str("\\x0a"),
            '\0' => escaped.push_str("\\x00"),
            _ => escaped.push(character),
        }
    }
    escaped
}

/// 校验固定 TSV 的 UTF-8、列数、footer 行数和前置数据 hash。
pub fn validate_result_summary(output_path: &Path) -> Result<(), ResultSummaryError> {
    let bytes = fs::read(output_path)?;
    if bytes.starts_with(&[0xEF, 0xBB, 0xBF]) {
        return Err(ResultSummaryError::InvalidOutput("禁止 UTF-8 BOM".into()));
    }
    if !bytes.ends_with(b"\n") || bytes.windows(2).any(|pair| pair == b"\r\n") {
        return Err(ResultSummaryError::InvalidOutput(
            "TSV 必须使用 LF 且包含最终换行".into(),
        ));
    }
    let text = std::str::from_utf8(&bytes)
        .map_err(|_| ResultSummaryError::InvalidOutput("TSV 不是 UTF-8".into()))?;
    let mut lines = text.split('\n').collect::<Vec<_>>();
    lines.pop();
    if lines.is_empty() || lines[0] != TSV_COLUMNS.join("\t") {
        return Err(ResultSummaryError::InvalidOutput("TSV 头行不匹配".into()));
    }
    let footer = lines
        .pop()
        .ok_or_else(|| ResultSummaryError::InvalidOutput("缺少 footer".into()))?;
    let footer_fields = footer.split('\t').collect::<Vec<_>>();
    if footer_fields.len() != 3 || footer_fields[0] != "F" {
        return Err(ResultSummaryError::InvalidOutput("footer 列不匹配".into()));
    }
    let expected_rows = footer_fields[1]
        .parse::<u64>()
        .map_err(|_| ResultSummaryError::InvalidOutput("footer row_count 无效".into()))?;
    if footer_fields[2].len() != 64
        || !footer_fields[2]
            .bytes()
            .all(|byte| byte.is_ascii_hexdigit())
    {
        return Err(ResultSummaryError::InvalidOutput(
            "footer SHA-256 无效".into(),
        ));
    }
    let mut data_rows = 0_u64;
    for line in &lines[1..] {
        let fields = line.split('\t').collect::<Vec<_>>();
        if fields.len() != TSV_COLUMNS.len() || fields[0] != "R" {
            return Err(ResultSummaryError::InvalidOutput(
                "数据行记录类型或列数无效".into(),
            ));
        }
        if !matches!(fields[1], "PASS" | "MISSING" | "INCONCLUSIVE") {
            return Err(ResultSummaryError::InvalidOutput("数据行状态无效".into()));
        }
        data_rows += 1;
    }
    if data_rows != expected_rows {
        return Err(ResultSummaryError::InvalidOutput(
            "footer row_count 与数据行不一致".into(),
        ));
    }
    let footer_start = bytes[..bytes.len() - 1]
        .iter()
        .rposition(|byte| *byte == b'\n')
        .map(|position| position + 1)
        .ok_or_else(|| ResultSummaryError::InvalidOutput("footer 起点无效".into()))?;
    let actual_data_hash = sha256_hex(&bytes[..footer_start]);
    if actual_data_hash != footer_fields[2] {
        return Err(ResultSummaryError::InvalidOutput(
            "footer 数据 hash 不匹配".into(),
        ));
    }
    Ok(())
}

/// 保留旧调用名，但现在只校验一个固定 TSV，不创建 pair/JSON metadata。
pub fn validate_result_summary_pair(output_path: &Path) -> Result<(), ResultSummaryError> {
    validate_result_summary(output_path)
}

/// 流式计算文件 SHA-256，避免把数据库或媒体 artifact 整体放入内存。
fn sha256_file_path(path: &Path) -> Result<String, ResultSummaryError> {
    let mut file = File::open(path)?;
    let mut digest = Sha256::new();
    let mut buffer = [0_u8; 64 * 1024];
    loop {
        let read = file.read(&mut buffer)?;
        if read == 0 {
            break;
        }
        digest.update(&buffer[..read]);
    }
    Ok(hex_digest(digest.finalize()))
}

/// 计算字节的全小写十六进制 SHA-256。
fn sha256_hex(bytes: &[u8]) -> String {
    hex_digest(Sha256::digest(bytes))
}

/// 将 digest 统一编码为小写十六进制。
fn hex_digest(digest: impl AsRef<[u8]>) -> String {
    digest
        .as_ref()
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect()
}

/// 把 SQLite BLOB 编码为固定小写十六进制。
fn lower_hex(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut output = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        output.push(HEX[(byte >> 4) as usize] as char);
        output.push(HEX[(byte & 0x0f) as usize] as char);
    }
    output
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 文件身份相关平台边界只要求当前平台能正常读出自身 executable。
    #[test]
    fn executable_hash_is_readable() {
        let executable = std::env::current_exe().expect("测试进程路径");
        assert!(sha256_file_path(&executable).is_ok());
    }
}
