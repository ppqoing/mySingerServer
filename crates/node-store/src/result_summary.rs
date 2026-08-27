//! 停止 Node 后从 SQLite 只读导出规范结果摘要。

use std::{
    collections::HashSet,
    ffi::OsString,
    fs::{self, File, OpenOptions},
    io::{Read, Write},
    path::{Path, PathBuf},
    sync::atomic::{AtomicU64, Ordering},
};

#[cfg(feature = "acceptance-tools")]
use std::sync::{Mutex, OnceLock};

use rusqlite::{Connection, OpenFlags, OptionalExtension, params};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

/// 单次导出的证据完整性状态。
#[derive(Clone, Copy, Debug, Eq, PartialEq, Deserialize, Serialize)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum ResultSummaryStatus {
    /// 任务、内容、基础特征和必需 artifact 均完整。
    Pass,
    /// 数据库可读，但任务、内容或预期 artifact 不存在。
    Missing,
    /// 数据存在互相矛盾、任务未终态或基础特征不完整。
    Inconclusive,
}

impl ResultSummaryStatus {
    /// 返回 CLI 和 metadata 使用的稳定大写状态名。
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Pass => "PASS",
            Self::Missing => "MISSING",
            Self::Inconclusive => "INCONCLUSIVE",
        }
    }
}

/// 导出文件及整体哈希的稳定返回值。
#[derive(Debug)]
pub struct ResultSummaryExport {
    /// 调用方请求的任务 ID；仅用于诊断 metadata。
    pub task_id: String,
    /// SQLite 中的任务状态，任务不存在时为 `missing`。
    pub task_status: String,
    /// canonical JSONL 行数。
    pub row_count: u64,
    /// 缺少内容或 artifact 的行数。
    pub missing_count: u64,
    /// 任务/特征状态不确定的行数。
    pub inconclusive_count: u64,
    /// 本次导出的完整性状态。
    pub status: ResultSummaryStatus,
    /// canonical JSONL 路径。
    pub output_path: PathBuf,
    /// 诊断 metadata 路径；不参与 canonical hash。
    pub metadata_path: PathBuf,
    /// 包含最终 LF 的 canonical JSONL SHA-256。
    pub sha256: String,
}

/// 导出器只报告数据库、文件、JSON、参数和路径安全错误。
#[derive(Debug, Error)]
pub enum ResultSummaryError {
    /// SQLite 只读连接、查询或 pragma 错误。
    #[error("SQLite 读取失败: {0}")]
    Sqlite(#[from] rusqlite::Error),
    /// 输入数据库、artifact 或输出文件读写错误。
    #[error("文件读取或写入失败: {0}")]
    Io(#[from] std::io::Error),
    /// canonical 行或 metadata 的紧凑 JSON 编码错误。
    #[error("JSON 编码失败: {0}")]
    Json(#[from] serde_json::Error),
    /// canonical 与 metadata 未能作为一个可消费 pair 完成提交。
    #[error("成对摘要提交不完整")]
    OutputCommitIncomplete,
    /// 当前平台没有可证明替换关系的文件身份 API。
    #[error("当前平台不支持文件身份校验")]
    UnsupportedFileIdentity,
    /// 调用方参数为空或输出父目录不存在等参数边界错误。
    #[error("导出参数无效: {0}")]
    InvalidArgument(String),
    /// contact sheet 路径不是 cache root 内的普通相对路径。
    #[error("联系表路径越出隔离缓存根")]
    UnsafeArtifactPath,
}

/// 以 SQLite 只读、无 mutex 的方式打开数据库，并固定查询参数。
fn open_read_only_database(path: &Path) -> Result<Connection, ResultSummaryError> {
    let connection = Connection::open_with_flags(
        path,
        OpenFlags::SQLITE_OPEN_READ_ONLY | OpenFlags::SQLITE_OPEN_NO_MUTEX,
    )?;
    connection.execute_batch("PRAGMA query_only = ON; PRAGMA busy_timeout = 5000;")?;
    Ok(connection)
}

/// 在同一只读连接上建立事务并预读任务头，先完成 SQLite WAL/SHM 初始化。
fn begin_result_summary_read_snapshot(
    connection: &Connection,
    task_id: &str,
) -> Result<Option<TaskHeader>, ResultSummaryError> {
    connection.execute_batch("BEGIN;")?;
    load_task_header(connection, task_id)
}

/// acceptance-tools 可控读取阶段；正式构建不会暴露或执行这些测试注入点。
#[cfg(feature = "acceptance-tools")]
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
#[repr(u8)]
pub enum ResultSummaryReadTestHook {
    /// 不注入 sidecar 变化。
    None = 0,
    /// SQLite 只读连接已建立，但 WAL/SHM 尚未冻结。
    AfterDatabaseOpenBeforeSidecarCapture = 1,
    /// WAL/SHM 已冻结，查询与提交尚未开始。
    AfterSidecarCapture = 2,
}

#[cfg(feature = "acceptance-tools")]
static RESULT_SUMMARY_READ_TEST_HOOK: AtomicU64 = AtomicU64::new(0);

#[cfg(feature = "acceptance-tools")]
static RESULT_SUMMARY_READ_TEST_CALLBACK: OnceLock<Mutex<Option<fn(&Path)>>> = OnceLock::new();

/// 设置下一次导出使用的一次性读取阶段测试注入点。
#[cfg(feature = "acceptance-tools")]
pub fn set_result_summary_read_test_hook(hook: ResultSummaryReadTestHook) {
    RESULT_SUMMARY_READ_TEST_HOOK.store(hook as u64, Ordering::SeqCst);
}

/// 注册仅由验收测试调用的 sidecar 变化回调，生产构建不包含该 API。
#[cfg(feature = "acceptance-tools")]
pub fn set_result_summary_read_test_callback(callback: Option<fn(&Path)>) {
    let slot = RESULT_SUMMARY_READ_TEST_CALLBACK.get_or_init(|| Mutex::new(None));
    *slot.lock().expect("测试回调锁不应中毒") = callback;
}

/// 只消费匹配读取阶段，确保单次故障注入不会泄漏到之后的导出。
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

/// 记录 SQLite 主库及 WAL/SHM sidecar 的存在性和内容哈希。
#[derive(Debug)]
struct SidecarSnapshot {
    database_path: PathBuf,
    database_hash: String,
    entries: Vec<(PathBuf, Option<String>)>,
}

/// 返回 SQLite 的两个 sidecar 文件路径；不存在的文件仍记录为 None。
fn sqlite_sidecar_paths(database_path: &Path) -> [PathBuf; 2] {
    let mut wal = database_path.as_os_str().to_os_string();
    wal.push("-wal");
    let mut shm = database_path.as_os_str().to_os_string();
    shm.push("-shm");
    [PathBuf::from(wal), PathBuf::from(shm)]
}

/// acceptance-tools 测试使用的 sidecar 路径封装，保持原始 OsString 字节/宽字符。
#[cfg(feature = "acceptance-tools")]
pub fn sidecar_paths_for_acceptance(database_path: &Path) -> [PathBuf; 2] {
    sqlite_sidecar_paths(database_path)
}

/// 读取一个 sidecar 的 SHA；NotFound 才表示不存在，其余错误必须向上传递。
fn sidecar_hash(path: &Path) -> Result<Option<String>, ResultSummaryError> {
    match sha256_file_path(path) {
        Ok(hash) => Ok(Some(hash)),
        Err(ResultSummaryError::Io(error)) if error.kind() == std::io::ErrorKind::NotFound => {
            Ok(None)
        }
        Err(error) => Err(error),
    }
}

/// 在只读连接已建立后捕获 sidecar，读错误不得被静默忽略。
fn capture_sidecars(database_path: &Path) -> Result<SidecarSnapshot, ResultSummaryError> {
    let database_hash = sha256_file_path(database_path)?;
    let entries = sqlite_sidecar_paths(database_path)
        .into_iter()
        .map(|path| Ok((path.clone(), sidecar_hash(&path)?)))
        .collect::<Result<Vec<_>, ResultSummaryError>>()?;
    Ok(SidecarSnapshot {
        database_path: database_path.to_owned(),
        database_hash,
        entries,
    })
}

/// 确认 SQLite 只读打开期间没有创建、删除或修改 WAL/SHM。
fn verify_sidecars(snapshot: &SidecarSnapshot) -> Result<(), ResultSummaryError> {
    let actual_database_hash = sha256_file_path(&snapshot.database_path)?;
    if actual_database_hash != snapshot.database_hash {
        return Err(ResultSummaryError::InvalidArgument(
            "SQLite 主数据库在只读导出期间发生变化".into(),
        ));
    }
    for (path, expected_hash) in &snapshot.entries {
        let actual_hash = sidecar_hash(path)?;
        if &actual_hash != expected_hash {
            return Err(ResultSummaryError::InvalidArgument(
                "SQLite WAL/SHM sidecar 在只读导出期间发生变化".into(),
            ));
        }
    }
    Ok(())
}

/// 用标准库元数据提取跨平台文件身份，供 artifact TOCTOU 复核使用。
#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
struct FileIdentity {
    first: u64,
    second: u64,
}

/// Windows GetFileInformationByHandle 的标准 Win32 返回结构。
#[cfg(windows)]
#[repr(C)]
struct WindowsByHandleFileInformation {
    file_attributes: u32,
    creation_time_low: u32,
    creation_time_high: u32,
    last_access_time_low: u32,
    last_access_time_high: u32,
    last_write_time_low: u32,
    last_write_time_high: u32,
    volume_serial_number: u32,
    file_size_high: u32,
    file_size_low: u32,
    number_of_links: u32,
    file_index_high: u32,
    file_index_low: u32,
}

// 通过标准 Windows API 查询已打开句柄的卷与 file index。
#[cfg(windows)]
unsafe extern "system" {
    fn GetFileInformationByHandle(
        handle: std::os::windows::io::RawHandle,
        information: *mut WindowsByHandleFileInformation,
    ) -> i32;
}

/// 从已打开句柄读取文件身份；Windows 使用卷序列号和 file index。
fn file_identity(file: &File) -> Result<FileIdentity, ResultSummaryError> {
    #[cfg(any(unix, not(any(windows, unix))))]
    let metadata = file.metadata()?;
    #[cfg(windows)]
    {
        use std::os::windows::io::AsRawHandle;
        let mut information = WindowsByHandleFileInformation {
            file_attributes: 0,
            creation_time_low: 0,
            creation_time_high: 0,
            last_access_time_low: 0,
            last_access_time_high: 0,
            last_write_time_low: 0,
            last_write_time_high: 0,
            volume_serial_number: 0,
            file_size_high: 0,
            file_size_low: 0,
            number_of_links: 0,
            file_index_high: 0,
            file_index_low: 0,
        };
        let succeeded =
            unsafe { GetFileInformationByHandle(file.as_raw_handle(), &mut information) };
        if succeeded == 0 {
            return Err(ResultSummaryError::Io(std::io::Error::last_os_error()));
        }
        Ok(FileIdentity {
            first: information.volume_serial_number as u64,
            second: ((information.file_index_high as u64) << 32)
                | information.file_index_low as u64,
        })
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::MetadataExt;
        Ok(FileIdentity {
            first: metadata.dev(),
            second: metadata.ino(),
        })
    }
    #[cfg(not(any(windows, unix)))]
    {
        let _ = metadata;
        Err(ResultSummaryError::UnsupportedFileIdentity)
    }
}

/// 通过当前路径重新打开文件并读取其标准库文件身份。
fn path_file_identity(path: &Path) -> Result<FileIdentity, ResultSummaryError> {
    let file = OpenOptions::new().read(true).open(path)?;
    file_identity(&file)
}

/// 停止 Node 后导出稳定 canonical JSONL 和独立诊断 metadata。
pub fn export_scan_result_summary(
    database_path: &Path,
    cache_root: &Path,
    task_id: &str,
    output_path: &Path,
) -> Result<ResultSummaryExport, ResultSummaryError> {
    let metadata_path = metadata_path(output_path);
    validate_arguments(
        database_path,
        cache_root,
        task_id,
        output_path,
        &metadata_path,
    )?;
    let canonical_cache_root = canonical_cache_root(cache_root)?;
    let connection = open_read_only_database(database_path)?;
    #[cfg(feature = "acceptance-tools")]
    run_result_summary_read_test_hook(
        ResultSummaryReadTestHook::AfterDatabaseOpenBeforeSidecarCapture,
        database_path,
    );
    let task = begin_result_summary_read_snapshot(&connection, task_id)?;
    let sidecars = capture_sidecars(database_path)?;
    #[cfg(feature = "acceptance-tools")]
    run_result_summary_read_test_hook(
        ResultSummaryReadTestHook::AfterSidecarCapture,
        database_path,
    );
    let (task, rows, diagnostics, missing_count, inconclusive_count) = {
        let mut diagnostics = Vec::new();
        let mut rows = Vec::new();
        let mut seen_paths = HashSet::new();
        let mut missing_count = 0_u64;
        let mut inconclusive_count = 0_u64;

        let raw_items = load_task_items(&connection, task_id)?;
        for raw_item in raw_items {
            let normalized_path = raw_item
                .normalized_path
                .as_deref()
                .filter(|path| !path.is_empty())
                .ok_or_else(|| {
                    ResultSummaryError::InvalidArgument(
                        "任务项 normalized_path 不能为空或 NULL".into(),
                    )
                })?;
            if !seen_paths.insert(normalized_path.to_owned()) {
                return Err(ResultSummaryError::InvalidArgument(
                    "任务含重复 normalized_path，拒绝生成摘要".into(),
                ));
            }
            let (row, disposition) =
                build_canonical_row(&connection, &canonical_cache_root, &raw_item)?;
            if disposition.missing {
                missing_count += 1;
            }
            if disposition.inconclusive {
                inconclusive_count += 1;
            }
            diagnostics.extend(disposition.diagnostics);
            rows.push(row);
        }

        if task_has_inconclusive_state(task.as_ref(), &rows) && !rows.is_empty() {
            // missing/inconclusive 均定义为“受影响 item 数”，任务级不确定覆盖全部行。
            inconclusive_count = inconclusive_count.max(rows.len() as u64);
        }
        if let Some(task) = task.as_ref()
            && task_has_inconclusive_state(Some(task), &rows)
        {
            diagnostics.push(SummaryDiagnostic {
                kind: "task_state",
                item_id: None,
                machine_id: None,
                normalized_path: None,
                display_path: None,
                file_size: None,
                stage: None,
                error: None,
                content_id: None,
                message: format!(
                    "任务状态或计数不可裁决: status={}, total_items={}, row_count={}",
                    task.status,
                    task.total_items,
                    rows.len()
                ),
            });
        }
        (task, rows, diagnostics, missing_count, inconclusive_count)
    };
    drop(connection);
    verify_sidecars(&sidecars)?;
    let (task_status, status) = classify_summary(
        task.as_ref(),
        rows.len() as u64,
        missing_count,
        inconclusive_count,
        &rows,
    );
    let canonical_bytes = encode_canonical_jsonl(&rows)?;
    let sha256 = sha256_hex(&canonical_bytes);
    let lease_token = new_pair_lease_token();
    let metadata = SummaryMetadata {
        schema_version: 1,
        lease_token: lease_token.clone(),
        canonical_sha256: sha256.clone(),
        task_id: task_id.to_owned(),
        task_status: task_status.clone(),
        status,
        row_count: rows.len() as u64,
        count_definition: "item_count",
        missing_count,
        inconclusive_count,
        diagnostics,
    };
    let mut metadata_bytes = serde_json::to_vec(&metadata)?;
    metadata_bytes.push(b'\n');
    let lease = atomic_write_pair(
        output_path,
        &metadata_path,
        &canonical_bytes,
        &metadata_bytes,
        &lease_token,
        status,
    )?;
    validate_result_summary_pair(output_path)?;
    drop(lease);

    Ok(ResultSummaryExport {
        task_id: task_id.to_owned(),
        task_status,
        row_count: rows.len() as u64,
        missing_count,
        inconclusive_count,
        status,
        output_path: output_path.to_owned(),
        metadata_path,
        sha256,
    })
}

/// 检查不会改变数据库的输入边界，并确保输出不会隐式创建目录。
fn validate_arguments(
    database_path: &Path,
    cache_root: &Path,
    task_id: &str,
    output_path: &Path,
    metadata_path: &Path,
) -> Result<(), ResultSummaryError> {
    if database_path.as_os_str().is_empty() {
        return Err(ResultSummaryError::InvalidArgument(
            "database path 不能为空".into(),
        ));
    }
    if cache_root.as_os_str().is_empty() {
        return Err(ResultSummaryError::InvalidArgument(
            "cache root 不能为空".into(),
        ));
    }
    if task_id.trim().is_empty() {
        return Err(ResultSummaryError::InvalidArgument(
            "task id 不能为空".into(),
        ));
    }
    if output_path.as_os_str().is_empty() {
        return Err(ResultSummaryError::InvalidArgument(
            "output path 不能为空".into(),
        ));
    }
    let parent = output_path
        .parent()
        .ok_or_else(|| ResultSummaryError::InvalidArgument("output path 必须包含父目录".into()))?;
    match fs::metadata(parent) {
        Ok(metadata) if metadata.is_dir() => {}
        Ok(_) => {
            return Err(ResultSummaryError::InvalidArgument(
                "output path 父级必须是目录".into(),
            ));
        }
        Err(error) => return Err(ResultSummaryError::Io(error)),
    }
    if output_path == metadata_path {
        return Err(ResultSummaryError::InvalidArgument(
            "canonical 与 metadata 输出不能是同一路径".into(),
        ));
    }
    let canonical_cache_root = canonical_cache_root(cache_root)?;
    let canonical_database = fs::canonicalize(database_path)?;
    let canonical_output = canonical_target_path(output_path)?;
    let canonical_metadata = canonical_target_path(metadata_path)?;
    for (kind, path) in [
        ("canonical", &canonical_output),
        ("metadata", &canonical_metadata),
    ] {
        if path.starts_with(&canonical_cache_root) {
            return Err(ResultSummaryError::InvalidArgument(format!(
                "{kind} 输出不能位于 cache root 或 artifact 内"
            )));
        }
        if path == &canonical_database {
            return Err(ResultSummaryError::InvalidArgument(format!(
                "{kind} 输出不能与 database 相同"
            )));
        }
    }
    if canonical_output == canonical_metadata {
        return Err(ResultSummaryError::InvalidArgument(
            "canonical 与 metadata 输出不能互为别名".into(),
        ));
    }
    for (kind, path) in [("canonical", output_path), ("metadata", metadata_path)] {
        match fs::symlink_metadata(path) {
            Ok(_) => {
                return Err(ResultSummaryError::InvalidArgument(format!(
                    "{kind} 输出文件已存在，拒绝覆盖"
                )));
            }
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(error) => return Err(ResultSummaryError::Io(error)),
        }
    }
    Ok(())
}

/// 解析“可能尚不存在”的输出路径，仅规范化其已存在父目录。
fn canonical_target_path(path: &Path) -> Result<PathBuf, ResultSummaryError> {
    let parent = path
        .parent()
        .ok_or_else(|| ResultSummaryError::InvalidArgument("输出路径必须包含父目录".into()))?;
    let file_name = path
        .file_name()
        .ok_or_else(|| ResultSummaryError::InvalidArgument("输出路径必须包含文件名".into()))?;
    Ok(fs::canonicalize(parent)?.join(file_name))
}

/// 把 cache root 解析为真实目录，同时拒绝 root 自身是 reparse/symlink。
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

/// 任务头的稳定计数；不写入 canonical，只用于诊断状态。
#[derive(Clone, Debug)]
struct TaskHeader {
    status: String,
    total_items: i64,
    succeeded: i64,
    failed: i64,
    cancelled: i64,
}

/// 主查询返回的任务项；本地 ID 只留在 metadata 诊断边界。
#[derive(Clone, Debug)]
struct RawTaskItem {
    item_id: String,
    machine_id: Option<String>,
    normalized_path: Option<String>,
    display_path: Option<String>,
    file_size: Option<i64>,
    content_id: Option<i64>,
    content_row_exists: Option<i64>,
    content_md5: Option<Vec<u8>>,
    content_file_size: Option<i64>,
    content_media_kind: Option<String>,
    content_base_complete: Option<i64>,
    status: String,
    stage: Option<String>,
    error: Option<String>,
}

/// 一行结果的诊断计数和可追踪原因。
#[derive(Default)]
struct ItemDisposition {
    missing: bool,
    inconclusive: bool,
    diagnostics: Vec<SummaryDiagnostic>,
}

/// 读取任务头，任务不存在通过 `None` 进入 MISSING 而不是伪造错误。
fn load_task_header(
    connection: &Connection,
    task_id: &str,
) -> Result<Option<TaskHeader>, ResultSummaryError> {
    connection
        .query_row(
            "SELECT status,total_items,succeeded,failed_items,cancelled
             FROM tasks WHERE task_id=?1",
            [task_id],
            |row| {
                Ok(TaskHeader {
                    status: row.get(0)?,
                    total_items: row.get(1)?,
                    succeeded: row.get(2)?,
                    failed: row.get(3)?,
                    cancelled: row.get(4)?,
                })
            },
        )
        .optional()
        .map_err(Into::into)
}

/// 按 normalized_path、machine_id、item_id 的 SQLite BINARY 顺序读取任务项。
fn load_task_items(
    connection: &Connection,
    task_id: &str,
) -> Result<Vec<RawTaskItem>, ResultSummaryError> {
    let mut statement = connection.prepare(
        "SELECT
            ti.item_id,
            ti.machine_id,
            ti.normalized_path,
            ti.display_path,
            ti.file_size,
            ti.content_id,
            ti.status,
            ti.stage,
            ti.error,
            c.content_id,
            c.md5,
            c.file_size,
            c.media_kind,
            c.base_complete
         FROM task_items ti
         LEFT JOIN contents c ON c.content_id = ti.content_id
         WHERE ti.task_id = ?1
         ORDER BY ti.normalized_path COLLATE BINARY,
                  ti.machine_id COLLATE BINARY,
                  ti.item_id COLLATE BINARY",
    )?;
    statement
        .query_map([task_id], |row| {
            Ok(RawTaskItem {
                item_id: row.get(0)?,
                machine_id: row.get(1)?,
                normalized_path: row.get(2)?,
                display_path: row.get(3)?,
                file_size: row.get(4)?,
                content_id: row.get(5)?,
                status: row.get(6)?,
                stage: row.get(7)?,
                error: row.get(8)?,
                content_row_exists: row.get(9)?,
                content_md5: row.get(10)?,
                content_file_size: row.get(11)?,
                content_media_kind: row.get(12)?,
                content_base_complete: row.get(13)?,
            })
        })?
        .collect::<Result<Vec<_>, _>>()
        .map_err(Into::into)
}

/// canonical 比较使用的稳定行；不包含任务 ID、item ID、PID、时间或本地 ID。
#[derive(Serialize)]
struct CanonicalResultRow {
    schema_version: u32,
    normalized_path: String,
    status: String,
    file_size: Option<u64>,
    md5: Option<String>,
    media_type: Option<String>,
    base_complete: Option<bool>,
    feature_payloads: FeaturePayloadHashes,
    feature_payload_sha256: Option<String>,
    contact_sheet_sha256: Option<String>,
    thumbnail_sha256: Option<String>,
    thumbnail_state: &'static str,
}

/// 每类数据库 payload 的独立 SHA-256；视频数组固定六个槽位。
#[derive(Serialize)]
struct FeaturePayloadHashes {
    image_stage1: Option<String>,
    image_stage2: Option<String>,
    video_metadata: Option<String>,
    video_frame_stage1: [Option<String>; 6],
    video_frame_stage2: [Option<String>; 6],
}

/// metadata 的单条诊断；canonical 行不携带这些本地和错误信息。
#[derive(Serialize)]
struct SummaryDiagnostic {
    kind: &'static str,
    item_id: Option<String>,
    machine_id: Option<String>,
    normalized_path: Option<String>,
    display_path: Option<String>,
    file_size: Option<i64>,
    stage: Option<String>,
    error: Option<String>,
    content_id: Option<i64>,
    message: String,
}

/// metadata 独立于 canonical JSONL 保存任务诊断和计数。
#[derive(Serialize)]
struct SummaryMetadata {
    schema_version: u32,
    /// 与同目录持久 pair manifest 对应的一次性提交 token。
    lease_token: String,
    /// canonical JSONL 实际字节（含最终 LF）的 SHA-256。
    canonical_sha256: String,
    task_id: String,
    task_status: String,
    status: ResultSummaryStatus,
    row_count: u64,
    /// 两个计数均按“受影响 item 数”统计；同一 item 可同时出现在两类证据计数。
    count_definition: &'static str,
    missing_count: u64,
    inconclusive_count: u64,
    diagnostics: Vec<SummaryDiagnostic>,
}

/// 读取内容和各媒体特征，构造不携带本地 ID 的 canonical 行。
fn build_canonical_row(
    connection: &Connection,
    cache_root: &Path,
    item: &RawTaskItem,
) -> Result<(CanonicalResultRow, ItemDisposition), ResultSummaryError> {
    let mut disposition = ItemDisposition::default();
    let normalized_path = item
        .normalized_path
        .clone()
        .filter(|path| !path.is_empty())
        .ok_or_else(|| {
            ResultSummaryError::InvalidArgument("任务项 normalized_path 不能为空或 NULL".into())
        })?;

    let mut row = CanonicalResultRow {
        schema_version: 1,
        normalized_path,
        status: item.status.clone(),
        file_size: None,
        md5: None,
        media_type: None,
        base_complete: None,
        feature_payloads: empty_feature_payload_hashes(),
        feature_payload_sha256: None,
        contact_sheet_sha256: None,
        thumbnail_sha256: None,
        thumbnail_state: "unsupported_no_thumbnail_artifact",
    };

    if item.status != "succeeded" {
        // 非成功项只保留稳定身份和状态；content、feature、错误只进入 metadata。
        mark_inconclusive(
            &mut disposition,
            item,
            &format!("任务项状态为 {}", item.status),
            "item_status",
        );
        return Ok((row, disposition));
    }

    let Some(content_id) = item.content_id else {
        mark_missing(
            &mut disposition,
            item,
            "任务项未引用 contents",
            "missing_content",
        );
        return Ok((row, disposition));
    };
    if item.content_row_exists.is_none() {
        mark_missing(
            &mut disposition,
            item,
            "contents 行不存在",
            "missing_content",
        );
        return Ok((row, disposition));
    }
    let Some(item_file_size) = item.file_size else {
        mark_inconclusive(
            &mut disposition,
            item,
            "task_items.file_size 不能为 NULL",
            "invalid_content",
        );
        return Ok((row, disposition));
    };
    let Some(content_file_size) = item.content_file_size else {
        mark_inconclusive(
            &mut disposition,
            item,
            "contents.file_size 不能为 NULL",
            "invalid_content",
        );
        return Ok((row, disposition));
    };
    let Some(md5) = item.content_md5.clone() else {
        mark_inconclusive(
            &mut disposition,
            item,
            "contents.md5 不能为 NULL",
            "invalid_content",
        );
        return Ok((row, disposition));
    };
    let Some(media_kind) = item.content_media_kind.clone() else {
        mark_inconclusive(
            &mut disposition,
            item,
            "contents.media_kind 不能为 NULL",
            "invalid_content",
        );
        return Ok((row, disposition));
    };
    let Some(base_complete_raw) = item.content_base_complete else {
        mark_inconclusive(
            &mut disposition,
            item,
            "contents.base_complete 不能为 NULL",
            "invalid_content",
        );
        return Ok((row, disposition));
    };
    if item_file_size < 0 || content_file_size < 0 {
        mark_inconclusive(
            &mut disposition,
            item,
            "task_items.file_size 和 contents.file_size 必须为非负数",
            "invalid_content",
        );
        return Ok((row, disposition));
    }
    if item_file_size != content_file_size {
        mark_inconclusive(
            &mut disposition,
            item,
            "task_items.file_size 与 contents.file_size 不一致",
            "invalid_content",
        );
        return Ok((row, disposition));
    }
    let base_complete = match base_complete_raw {
        0 => false,
        1 => true,
        _ => {
            mark_inconclusive(
                &mut disposition,
                item,
                "contents.base_complete 只能是 0 或 1",
                "invalid_content",
            );
            return Ok((row, disposition));
        }
    };
    let media_type = match media_kind.as_str() {
        "image" | "video" | "other" => media_kind,
        _ => {
            mark_inconclusive(
                &mut disposition,
                item,
                "contents.media_kind 无效",
                "invalid_content",
            );
            return Ok((row, disposition));
        }
    };
    if md5.len() != 16 {
        mark_inconclusive(
            &mut disposition,
            item,
            "contents.md5 必须为 16 字节",
            "invalid_content",
        );
        return Ok((row, disposition));
    }
    row.file_size = Some(item_file_size as u64);
    row.md5 = Some(lower_hex(&md5));
    row.media_type = Some(media_type.clone());
    row.base_complete = Some(base_complete);
    let (feature_payloads, has_required_base) =
        load_feature_payloads(connection, content_id, &media_type)?;
    row.feature_payload_sha256 = Some(hash_serialized(&feature_payloads)?);
    row.feature_payloads = feature_payloads;
    if !base_complete || !has_required_base {
        mark_inconclusive(
            &mut disposition,
            item,
            "基础完成标记或必需特征不完整",
            "incomplete_base_features",
        );
    }

    let contact_sheet = load_contact_sheet(connection, content_id)?;
    match contact_sheet {
        Some(relative_path) => match read_safe_artifact(cache_root, &relative_path)? {
            ArtifactState::Present(hash) => row.contact_sheet_sha256 = Some(hash),
            ArtifactState::Missing => {
                mark_missing(
                    &mut disposition,
                    item,
                    "联系表 artifact 不存在",
                    "missing_artifact",
                );
            }
        },
        None if media_type == "video" => {
            mark_missing(
                &mut disposition,
                item,
                "视频联系表引用不存在",
                "missing_artifact",
            );
        }
        None => {}
    }
    Ok((row, disposition))
}

/// 返回六槽位空值，防止不同媒体类型产生可变数组长度。
fn empty_feature_payload_hashes() -> FeaturePayloadHashes {
    FeaturePayloadHashes {
        image_stage1: None,
        image_stage2: None,
        video_metadata: None,
        video_frame_stage1: [None, None, None, None, None, None],
        video_frame_stage2: [None, None, None, None, None, None],
    }
}

/// 读取所有图片/视频原始字段并计算独立 payload hash。
fn load_feature_payloads(
    connection: &Connection,
    content_id: i64,
    media_kind: &str,
) -> Result<(FeaturePayloadHashes, bool), ResultSummaryError> {
    let mut payloads = empty_feature_payload_hashes();
    let required = match media_kind {
        "image" => {
            let image_stage1: Option<(Option<i64>, Option<i64>, Option<Vec<u8>>, Option<i64>)> =
                connection
                    .query_row(
                        "SELECT width,height,pdq,quality FROM image_stage1 WHERE content_id=?1",
                        [content_id],
                        |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?, row.get(3)?)),
                    )
                    .optional()?;
            if let Some((width, height, pdq, quality)) = image_stage1 {
                payloads.image_stage1 = Some(hash_serialized(&ImageStage1Payload {
                    width,
                    height,
                    pdq: pdq.as_deref().map(lower_hex),
                    quality,
                })?);
                let complete = width.is_some()
                    && height.is_some()
                    && quality.is_some()
                    && pdq.as_ref().is_some_and(|bytes| bytes.len() == 32);
                Some(complete)
            } else {
                None
            }
            .unwrap_or(false)
        }
        "video" => {
            let metadata: Option<(Option<i64>, Option<i64>, Option<i64>)> = connection
                .query_row(
                    "SELECT duration_ms,width,height FROM video_metadata WHERE content_id=?1",
                    [content_id],
                    |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?)),
                )
                .optional()?;
            let metadata_complete = if let Some((duration_ms, width, height)) = metadata {
                payloads.video_metadata = Some(hash_serialized(&VideoMetadataPayload {
                    duration_ms,
                    width,
                    height,
                })?);
                width.is_some() && height.is_some() && duration_ms.is_some()
            } else {
                false
            };
            let mut stage1_count = 0_usize;
            let mut slots_complete = true;
            let mut slot_evidence_valid = true;
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
                if let Some((time_ms, decoded_raw, width, height, pdq, quality)) = frame {
                    payloads.video_frame_stage1[slot as usize] =
                        Some(hash_serialized(&VideoFrameStage1Payload {
                            slot,
                            time_ms,
                            decoded: decoded_raw,
                            width,
                            height,
                            pdq: pdq.as_deref().map(lower_hex),
                            quality,
                        })?);
                    if decoded_raw == 1 {
                        stage1_count += 1;
                        slots_complete &= width.is_some()
                            && height.is_some()
                            && quality.is_some()
                            && pdq.as_ref().is_some_and(|bytes| bytes.len() == 32);
                    } else if decoded_raw != 0 {
                        slot_evidence_valid = false;
                        slots_complete = false;
                    }
                } else {
                    slots_complete = false;
                }
            }
            metadata_complete && slots_complete && stage1_count >= 4 && slot_evidence_valid
        }
        "other" => true,
        _ => false,
    };

    match media_kind {
        "image" => {
            let image_stage2: Option<(Option<Vec<u8>>, Option<Vec<u8>>)> = connection
                .query_row(
                    "SELECT phash_parts,sobel FROM image_stage2 WHERE content_id=?1",
                    [content_id],
                    |row| Ok((row.get(0)?, row.get(1)?)),
                )
                .optional()?;
            if let Some((phash_parts, sobel)) = image_stage2 {
                payloads.image_stage2 = Some(hash_serialized(&ImageStage2Payload {
                    phash_parts: phash_parts.as_deref().map(lower_hex),
                    sobel: sobel.as_deref().map(lower_hex),
                })?);
            }
        }
        "video" => {
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
                    payloads.video_frame_stage2[slot as usize] =
                        Some(hash_serialized(&VideoFrameStage2Payload {
                            slot,
                            phash_parts: phash_parts.as_deref().map(lower_hex),
                            sobel: sobel.as_deref().map(lower_hex),
                        })?);
                }
            }
        }
        _ => {}
    }
    Ok((payloads, required))
}

/// 图片一筛原始字段的固定序列化形状。
#[derive(Serialize)]
struct ImageStage1Payload {
    width: Option<i64>,
    height: Option<i64>,
    pdq: Option<String>,
    quality: Option<i64>,
}

/// 图片二筛原始 BLOB 字段的固定序列化形状。
#[derive(Serialize)]
struct ImageStage2Payload {
    phash_parts: Option<String>,
    sobel: Option<String>,
}

/// 视频整体 metadata 原始字段的固定序列化形状。
#[derive(Serialize)]
struct VideoMetadataPayload {
    duration_ms: Option<i64>,
    width: Option<i64>,
    height: Option<i64>,
}

/// 视频一筛槽位原始字段的固定序列化形状。
#[derive(Serialize)]
struct VideoFrameStage1Payload {
    slot: i64,
    time_ms: i64,
    decoded: i64,
    width: Option<i64>,
    height: Option<i64>,
    pdq: Option<String>,
    quality: Option<i64>,
}

/// 视频二筛槽位原始 BLOB 字段的固定序列化形状。
#[derive(Serialize)]
struct VideoFrameStage2Payload {
    slot: i64,
    phash_parts: Option<String>,
    sobel: Option<String>,
}

/// 读取联系表相对路径；路径字符串本身不进入 canonical payload。
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

/// 联系表读取结果，Missing 只表示 artifact 不存在，不伪造内容 hash。
enum ArtifactState {
    Present(String),
    Missing,
}

/// 校验相对路径、reparse 组件和 canonical containment 后读取联系表。
fn read_safe_artifact(
    cache_root: &Path,
    relative_path: &str,
) -> Result<ArtifactState, ResultSummaryError> {
    if relative_path.trim().is_empty() || has_unsafe_relative_component(relative_path) {
        return Err(ResultSummaryError::UnsafeArtifactPath);
    }
    let candidate = cache_root.join(relative_path);
    // 逐个检查现有组件；symlink_metadata 的非 NotFound 错误不能被 exists() 吞掉。
    let mut component = candidate.clone();
    let mut missing = false;
    loop {
        match fs::symlink_metadata(&component) {
            Ok(metadata) => {
                if is_reparse_point(&metadata) {
                    return Err(ResultSummaryError::UnsafeArtifactPath);
                }
            }
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                missing = true;
            }
            Err(error) => return Err(ResultSummaryError::Io(error)),
        }
        if component == *cache_root || !component.pop() {
            break;
        }
    }
    if missing {
        return Ok(ArtifactState::Missing);
    }
    let canonical_candidate = fs::canonicalize(&candidate)?;
    if !canonical_candidate.starts_with(cache_root) {
        return Err(ResultSummaryError::UnsafeArtifactPath);
    }
    let mut file = match File::open(&candidate) {
        Ok(file) => file,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            return Err(ResultSummaryError::UnsafeArtifactPath);
        }
        Err(error) => return Err(ResultSummaryError::Io(error)),
    };
    let opened_metadata = file.metadata()?;
    if !opened_metadata.is_file() || is_reparse_point(&opened_metadata) {
        return Err(ResultSummaryError::UnsafeArtifactPath);
    }
    let opened_identity = file_identity(&file)?;
    let mut bytes = Vec::new();
    file.read_to_end(&mut bytes)?;

    // 读取完成后再次走安全路径和句柄身份核对，覆盖替换、重解析和竞态越界。
    let mut current_component = candidate.clone();
    loop {
        match fs::symlink_metadata(&current_component) {
            Ok(metadata) => {
                if is_reparse_point(&metadata) {
                    return Err(ResultSummaryError::UnsafeArtifactPath);
                }
            }
            Err(error) => {
                return Err(if error.kind() == std::io::ErrorKind::NotFound {
                    ResultSummaryError::UnsafeArtifactPath
                } else {
                    ResultSummaryError::Io(error)
                });
            }
        }
        if current_component == *cache_root || !current_component.pop() {
            break;
        }
    }
    let current_canonical = fs::canonicalize(&candidate)?;
    if !current_canonical.starts_with(cache_root) {
        return Err(ResultSummaryError::UnsafeArtifactPath);
    }
    let current_identity = path_file_identity(&candidate)?;
    if current_identity != opened_identity {
        return Err(ResultSummaryError::UnsafeArtifactPath);
    }
    Ok(ArtifactState::Present(sha256_hex(&bytes)))
}

/// 拒绝绝对、父级和跨平台等价的路径组件，避免 lexical 绕过。
fn has_unsafe_relative_component(relative_path: &str) -> bool {
    let slash_normalized = relative_path.replace('\\', "/");
    if Path::new(relative_path).is_absolute()
        || slash_normalized.starts_with('/')
        || slash_normalized.starts_with("//")
        || (slash_normalized.len() >= 2
            && slash_normalized.as_bytes()[1] == b':'
            && slash_normalized.as_bytes()[0].is_ascii_alphabetic())
    {
        return true;
    }
    slash_normalized
        .split('/')
        .any(|component| component == "..")
}

/// 判断文件系统元数据是否包含 symlink/junction/reparse 标记。
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

/// 根据指定输出名推导稳定 metadata 文件名。
fn metadata_path(output_path: &Path) -> PathBuf {
    if output_path.file_name().and_then(|name| name.to_str()) == Some("result-summary.jsonl") {
        return output_path.with_file_name("result-summary-meta.json");
    }
    let stem = output_path
        .file_stem()
        .and_then(|value| value.to_str())
        .unwrap_or("result-summary");
    output_path.with_file_name(format!("{stem}-meta.json"))
}

/// 对 canonical 行逐行紧凑编码，并保证每行恰好一个 LF。
fn encode_canonical_jsonl(rows: &[CanonicalResultRow]) -> Result<Vec<u8>, ResultSummaryError> {
    let mut bytes = Vec::new();
    for row in rows {
        serde_json::to_writer(&mut bytes, row)?;
        bytes.push(b'\n');
    }
    Ok(bytes)
}

/// 进程内序号；只用于生成唯一 token、run evidence 目录和 staged 文件名。
static TEMP_FILE_COUNTER: AtomicU64 = AtomicU64::new(0);

/// 生成不进入 canonical 的一次性 pair token，跨并发导出保持可区分。
fn new_pair_lease_token() -> String {
    let sequence = TEMP_FILE_COUNTER.fetch_add(1, Ordering::Relaxed);
    format!("{}-{sequence}", std::process::id())
}

/// acceptance-tools 可控提交边界；正式构建不会暴露或触发这些分支。
#[cfg(feature = "acceptance-tools")]
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
#[repr(u8)]
pub enum ResultSummaryCommitTestHook {
    /// 不注入外部竞争。
    None = 0,
    /// metadata 发布后、canonical 发布前注入外部抢占文件。
    BeforeCanonicalExternal = 1,
    /// canonical 发布后注入外部替换，交给 validator 检测。
    AfterCanonicalExternal = 2,
}

#[cfg(feature = "acceptance-tools")]
static RESULT_SUMMARY_COMMIT_TEST_HOOK: AtomicU64 = AtomicU64::new(0);

#[cfg(feature = "acceptance-tools")]
static RESULT_SUMMARY_COMMIT_TEST_CALLBACK: OnceLock<Mutex<Option<fn(&Path)>>> = OnceLock::new();

/// 设置下一次 pair 提交使用的一次性外部竞争阶段。
#[cfg(feature = "acceptance-tools")]
pub fn set_result_summary_commit_test_hook(hook: ResultSummaryCommitTestHook) {
    RESULT_SUMMARY_COMMIT_TEST_HOOK.store(hook as u64, Ordering::SeqCst);
}

/// 注册只由验收测试执行的外部路径替换回调，生产构建不包含该 API。
#[cfg(feature = "acceptance-tools")]
pub fn set_result_summary_commit_test_callback(callback: Option<fn(&Path)>) {
    let slot = RESULT_SUMMARY_COMMIT_TEST_CALLBACK.get_or_init(|| Mutex::new(None));
    *slot.lock().expect("测试回调锁不应中毒") = callback;
}

/// 只消费匹配阶段，避免故障注入泄漏到其它导出调用。
#[cfg(feature = "acceptance-tools")]
fn take_result_summary_commit_test_hook(
    hook: ResultSummaryCommitTestHook,
    output_path: &Path,
) -> bool {
    let is_targeted_output = output_path
        .file_name()
        .and_then(|name| name.to_str())
        .is_some_and(|name| name.starts_with("hook-"));
    if !is_targeted_output {
        return false;
    }
    RESULT_SUMMARY_COMMIT_TEST_HOOK
        .compare_exchange(
            hook as u64,
            ResultSummaryCommitTestHook::None as u64,
            Ordering::SeqCst,
            Ordering::SeqCst,
        )
        .is_ok()
}

/// 消费测试回调；路径修改由测试代码承担，导出器不删除或替换 final。
#[cfg(feature = "acceptance-tools")]
fn take_result_summary_commit_test_callback() -> Option<fn(&Path)> {
    RESULT_SUMMARY_COMMIT_TEST_CALLBACK
        .get()
        .and_then(|slot| slot.lock().expect("测试回调锁不应中毒").take())
}

/// 返回 pair 同目录 lease 路径；OsString 文件名不经过 UTF-8 转换。
fn pair_commit_lease_path_internal(output_path: &Path) -> PathBuf {
    let mut name = output_path
        .file_name()
        .map(OsString::from)
        .unwrap_or_else(|| OsString::from("result-summary"));
    name.push(".pair.lock");
    output_path.with_file_name(name)
}

/// acceptance-tools 测试读取持久 manifest 路径，验证合作 exporter 的互斥边界。
#[cfg(feature = "acceptance-tools")]
pub fn pair_commit_lease_path(output_path: &Path) -> PathBuf {
    pair_commit_lease_path_internal(output_path)
}

/// 持久 pair manifest；成功和失败都保留，validator 以它确认提交身份。
#[derive(Debug, Deserialize, Serialize)]
struct PairLeaseManifest {
    /// manifest 自身版本，防止未来字段解释漂移。
    schema_version: u32,
    /// 与 metadata marker 对齐的一次性提交 token。
    lease_token: String,
    /// staged canonical hard-link 发布后应保持的文件身份。
    expected_canonical_identity: FileIdentity,
    /// staged metadata hard-link 发布后应保持的文件身份。
    expected_metadata_identity: FileIdentity,
    /// 预序列化 canonical 的完整字节 SHA-256。
    expected_canonical_sha256: String,
    /// canonical JSONL 行数，供 validator 复核。
    expected_row_count: u64,
    /// 导出状态，防止 metadata marker 被单独改写。
    expected_status: ResultSummaryStatus,
    /// 保存 staged 文件的唯一 run evidence 目录名。
    run_evidence_dir: String,
}

/// 成对提交期间持有的同目录 manifest，阻止合作 exporter 并发写同一 pair。
struct PairCommitLease {
    _file: File,
}

impl PairCommitLease {
    /// create_new 创建 manifest，写 token/预期身份并同步；文件不会按路径删除。
    fn acquire(
        output_path: &Path,
        lease_token: &str,
        canonical_identity: FileIdentity,
        metadata_identity: FileIdentity,
        canonical_sha256: &str,
        row_count: u64,
        status: ResultSummaryStatus,
        run_evidence_dir: &Path,
    ) -> Result<Self, ResultSummaryError> {
        let path = pair_commit_lease_path_internal(output_path);
        let mut file = match OpenOptions::new().write(true).create_new(true).open(&path) {
            Ok(file) => file,
            Err(error) if error.kind() == std::io::ErrorKind::AlreadyExists => {
                return Err(ResultSummaryError::OutputCommitIncomplete);
            }
            Err(error) => return Err(ResultSummaryError::Io(error)),
        };
        let manifest = PairLeaseManifest {
            schema_version: 1,
            lease_token: lease_token.to_owned(),
            expected_canonical_identity: canonical_identity,
            expected_metadata_identity: metadata_identity,
            expected_canonical_sha256: canonical_sha256.to_owned(),
            expected_row_count: row_count,
            expected_status: status,
            run_evidence_dir: run_evidence_dir
                .file_name()
                .and_then(|name| name.to_str())
                .unwrap_or("non-utf8-run-evidence")
                .to_owned(),
        };
        let mut bytes = serde_json::to_vec(&manifest)?;
        bytes.push(b'\n');
        file.write_all(&bytes)?;
        file.flush()?;
        file.sync_all()?;
        Ok(Self { _file: file })
    }
}

/// 创建同目录唯一 run evidence 目录；失败后目录和 staged 证据不原地清理。
fn create_run_evidence_dir(output_path: &Path) -> Result<PathBuf, ResultSummaryError> {
    let parent = output_path
        .parent()
        .ok_or_else(|| ResultSummaryError::InvalidArgument("输出路径必须包含父目录".into()))?;
    let output_name = output_path
        .file_name()
        .map(OsString::from)
        .unwrap_or_else(|| OsString::from("result-summary"));
    for _ in 0..64 {
        let sequence = TEMP_FILE_COUNTER.fetch_add(1, Ordering::Relaxed);
        let mut name = OsString::from(".");
        name.push(output_name.clone());
        name.push(format!(".run-{}-{sequence}", std::process::id()));
        let directory = parent.join(name);
        match fs::create_dir(&directory) {
            Ok(()) => return Ok(directory),
            Err(error) if error.kind() == std::io::ErrorKind::AlreadyExists => continue,
            Err(error) => return Err(ResultSummaryError::Io(error)),
        }
    }
    Err(ResultSummaryError::InvalidArgument(
        "无法创建唯一 run evidence 目录".into(),
    ))
}

/// 在 run evidence 目录内 create_new 写入并同步 staged 文件，失败也保留证据。
fn write_temp_file(
    run_evidence_dir: &Path,
    role: &str,
    bytes: &[u8],
) -> Result<PathBuf, ResultSummaryError> {
    for _ in 0..64 {
        let sequence = TEMP_FILE_COUNTER.fetch_add(1, Ordering::Relaxed);
        let temporary = run_evidence_dir.join(format!(
            ".stage-{role}-{}-{sequence}.tmp",
            std::process::id()
        ));
        let mut file = match OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&temporary)
        {
            Ok(file) => file,
            Err(error) if error.kind() == std::io::ErrorKind::AlreadyExists => continue,
            Err(error) => return Err(ResultSummaryError::Io(error)),
        };
        file.write_all(bytes)?;
        file.flush()?;
        file.sync_all()?;
        return Ok(temporary);
    }
    Err(ResultSummaryError::InvalidArgument(
        "无法创建唯一 staged 摘要文件".into(),
    ))
}

/// 以不可覆盖硬链接发布一个 staged 文件；成功后不删除 staged 名称。
fn commit_temp_file(temporary: &Path, target: &Path) -> Result<(), ResultSummaryError> {
    match fs::hard_link(temporary, target) {
        Ok(()) => Ok(()),
        Err(error) if error.kind() == std::io::ErrorKind::AlreadyExists => {
            Err(ResultSummaryError::OutputCommitIncomplete)
        }
        Err(error) => Err(ResultSummaryError::Io(error)),
    }
}

/// 以句柄、身份和重解析点复核一个 pair 文件，避免路径别名冒充普通文件。
fn open_verified_pair_file(path: &Path) -> Result<(File, FileIdentity), ResultSummaryError> {
    let path_metadata = fs::symlink_metadata(path)?;
    if !path_metadata.is_file() || is_reparse_point(&path_metadata) {
        return Err(ResultSummaryError::UnsafeArtifactPath);
    }
    let file = OpenOptions::new().read(true).open(path)?;
    let opened_metadata = file.metadata()?;
    if !opened_metadata.is_file() || is_reparse_point(&opened_metadata) {
        return Err(ResultSummaryError::UnsafeArtifactPath);
    }
    let opened_identity = file_identity(&file)?;
    let current_identity = path_file_identity(path)?;
    if current_identity != opened_identity {
        return Err(ResultSummaryError::UnsafeArtifactPath);
    }
    let current_metadata = fs::symlink_metadata(path)?;
    if !current_metadata.is_file() || is_reparse_point(&current_metadata) {
        return Err(ResultSummaryError::UnsafeArtifactPath);
    }
    let final_identity = path_file_identity(path)?;
    if final_identity != opened_identity {
        return Err(ResultSummaryError::UnsafeArtifactPath);
    }
    Ok((file, opened_identity))
}

/// validator 将缺失、替换、JSON 损坏统一归为不可消费；平台身份能力单独保留。
fn pair_validation_error(error: ResultSummaryError) -> ResultSummaryError {
    match error {
        ResultSummaryError::UnsupportedFileIdentity => ResultSummaryError::UnsupportedFileIdentity,
        _ => ResultSummaryError::OutputCommitIncomplete,
    }
}

/// metadata 中必须存在的提交 marker 字段；诊断字段不参与消费判定。
#[derive(Debug, Deserialize)]
struct MetadataCommitMarker {
    /// metadata schema 版本。
    schema_version: u32,
    /// 与持久 manifest 对齐的 token。
    lease_token: String,
    /// canonical 实际字节 hash。
    canonical_sha256: String,
    /// canonical JSONL 行数。
    row_count: u64,
    /// 导出完整性状态。
    status: ResultSummaryStatus,
}

/// 校验 canonical JSONL 的 UTF-8/LF/JSON 行数，供 pair marker 复核使用。
fn canonical_row_count(bytes: &[u8]) -> Result<u64, ResultSummaryError> {
    if bytes.is_empty() {
        return Ok(0);
    }
    if !bytes.ends_with(b"\n") || bytes.windows(2).any(|pair| pair == b"\r\n") {
        return Err(ResultSummaryError::OutputCommitIncomplete);
    }
    let mut row_count = 0_u64;
    for line in bytes.split(|byte| *byte == b'\n') {
        if line.is_empty() {
            continue;
        }
        serde_json::from_slice::<serde_json::Value>(line)
            .map_err(|_| ResultSummaryError::OutputCommitIncomplete)?;
        row_count += 1;
    }
    Ok(row_count)
}

/// 验证 canonical、metadata 和持久 lease 三件套，单个 JSONL 永远不可单独消费。
pub fn validate_result_summary_pair(output_path: &Path) -> Result<(), ResultSummaryError> {
    let metadata_path = metadata_path(output_path);
    let lease_path = pair_commit_lease_path_internal(output_path);
    let (mut lease_file, _lease_identity) =
        open_verified_pair_file(&lease_path).map_err(pair_validation_error)?;
    let mut lease_bytes = Vec::new();
    lease_file
        .read_to_end(&mut lease_bytes)
        .map_err(|error| pair_validation_error(ResultSummaryError::Io(error)))?;
    let lease_manifest: PairLeaseManifest = serde_json::from_slice(&lease_bytes)
        .map_err(|_| ResultSummaryError::OutputCommitIncomplete)?;
    if lease_manifest.schema_version != 1 || lease_manifest.lease_token.is_empty() {
        return Err(ResultSummaryError::OutputCommitIncomplete);
    }

    let (mut metadata_file, metadata_identity) =
        open_verified_pair_file(&metadata_path).map_err(pair_validation_error)?;
    if metadata_identity != lease_manifest.expected_metadata_identity {
        return Err(ResultSummaryError::OutputCommitIncomplete);
    }
    let mut metadata_bytes = Vec::new();
    metadata_file
        .read_to_end(&mut metadata_bytes)
        .map_err(|error| pair_validation_error(ResultSummaryError::Io(error)))?;
    let marker: MetadataCommitMarker = serde_json::from_slice(&metadata_bytes)
        .map_err(|_| ResultSummaryError::OutputCommitIncomplete)?;
    if marker.schema_version != 1
        || marker.lease_token != lease_manifest.lease_token
        || marker.canonical_sha256 != lease_manifest.expected_canonical_sha256
        || marker.row_count != lease_manifest.expected_row_count
        || marker.status != lease_manifest.expected_status
        || marker.canonical_sha256.len() != 64
        || !marker
            .canonical_sha256
            .bytes()
            .all(|byte| byte.is_ascii_hexdigit())
    {
        return Err(ResultSummaryError::OutputCommitIncomplete);
    }
    let _status = marker.status;

    let (mut canonical_file, canonical_identity) =
        open_verified_pair_file(output_path).map_err(pair_validation_error)?;
    if canonical_identity != lease_manifest.expected_canonical_identity {
        return Err(ResultSummaryError::OutputCommitIncomplete);
    }
    let mut canonical_bytes = Vec::new();
    canonical_file
        .read_to_end(&mut canonical_bytes)
        .map_err(|error| pair_validation_error(ResultSummaryError::Io(error)))?;
    let actual_hash = sha256_hex(&canonical_bytes);
    let actual_row_count = canonical_row_count(&canonical_bytes)?;
    if actual_hash != marker.canonical_sha256 || actual_row_count != marker.row_count {
        return Err(ResultSummaryError::OutputCommitIncomplete);
    }
    Ok(())
}

/// 成对提交 metadata 后 canonical；返回仍持有的 lease，调用方验证后再结束函数。
fn atomic_write_pair(
    output_path: &Path,
    metadata_path: &Path,
    canonical_bytes: &[u8],
    metadata_bytes: &[u8],
    lease_token: &str,
    status: ResultSummaryStatus,
) -> Result<PairCommitLease, ResultSummaryError> {
    let run_evidence_dir = create_run_evidence_dir(output_path)?;
    let canonical_temp = write_temp_file(&run_evidence_dir, "canonical", canonical_bytes)?;
    let metadata_temp = write_temp_file(&run_evidence_dir, "metadata", metadata_bytes)?;
    let expected_canonical_identity = path_file_identity(&canonical_temp)?;
    let expected_metadata_identity = path_file_identity(&metadata_temp)?;
    let canonical_sha256 = sha256_hex(canonical_bytes);
    let row_count = canonical_row_count(canonical_bytes)?;
    let lease = PairCommitLease::acquire(
        output_path,
        lease_token,
        expected_canonical_identity,
        expected_metadata_identity,
        &canonical_sha256,
        row_count,
        status,
        &run_evidence_dir,
    )?;
    atomic_write_pair_with_lease(
        output_path,
        metadata_path,
        &canonical_temp,
        &metadata_temp,
        expected_canonical_identity,
        expected_metadata_identity,
    )?;
    Ok(lease)
}

/// 在持久 lease 内按 metadata→canonical 顺序发布，并且不按路径回滚任何文件。
fn atomic_write_pair_with_lease(
    output_path: &Path,
    metadata_path: &Path,
    canonical_temp: &Path,
    metadata_temp: &Path,
    expected_canonical_identity: FileIdentity,
    expected_metadata_identity: FileIdentity,
) -> Result<(), ResultSummaryError> {
    commit_temp_file(metadata_temp, metadata_path)?;
    let metadata_identity = path_file_identity(metadata_path).map_err(pair_validation_error)?;
    if metadata_identity != expected_metadata_identity {
        return Err(ResultSummaryError::OutputCommitIncomplete);
    }

    #[cfg(feature = "acceptance-tools")]
    if take_result_summary_commit_test_hook(
        ResultSummaryCommitTestHook::BeforeCanonicalExternal,
        output_path,
    ) {
        if let Some(callback) = take_result_summary_commit_test_callback() {
            callback(output_path);
        }
    }

    commit_temp_file(canonical_temp, output_path)?;
    let canonical_identity = path_file_identity(output_path).map_err(pair_validation_error)?;
    if canonical_identity != expected_canonical_identity {
        return Err(ResultSummaryError::OutputCommitIncomplete);
    }

    #[cfg(feature = "acceptance-tools")]
    if take_result_summary_commit_test_hook(
        ResultSummaryCommitTestHook::AfterCanonicalExternal,
        output_path,
    ) {
        if let Some(callback) = take_result_summary_commit_test_callback() {
            callback(output_path);
        }
    }
    Ok(())
}

/// 在诊断 metadata 中标记缺失内容或 artifact。
fn mark_missing(
    disposition: &mut ItemDisposition,
    item: &RawTaskItem,
    message: &str,
    kind: &'static str,
) {
    disposition.missing = true;
    disposition.diagnostics.push(SummaryDiagnostic {
        kind,
        item_id: Some(item.item_id.clone()),
        machine_id: item.machine_id.clone(),
        normalized_path: item.normalized_path.clone(),
        display_path: item.display_path.clone(),
        file_size: item.file_size,
        stage: item.stage.clone(),
        error: item.error.clone(),
        content_id: item.content_id,
        message: message.into(),
    });
}

/// 在诊断 metadata 中标记任务状态、计数或特征矛盾。
fn mark_inconclusive(
    disposition: &mut ItemDisposition,
    item: &RawTaskItem,
    message: &str,
    kind: &'static str,
) {
    disposition.inconclusive = true;
    disposition.diagnostics.push(SummaryDiagnostic {
        kind,
        item_id: Some(item.item_id.clone()),
        machine_id: item.machine_id.clone(),
        normalized_path: item.normalized_path.clone(),
        display_path: item.display_path.clone(),
        file_size: item.file_size,
        stage: item.stage.clone(),
        error: item.error.clone(),
        content_id: item.content_id,
        message: message.into(),
    });
}

/// 综合任务终态、计数和每行诊断，严格选择 PASS/MISSING/INCONCLUSIVE。
fn classify_summary(
    task: Option<&TaskHeader>,
    row_count: u64,
    missing_count: u64,
    inconclusive_count: u64,
    rows: &[CanonicalResultRow],
) -> (String, ResultSummaryStatus) {
    let Some(task) = task else {
        return ("missing".into(), ResultSummaryStatus::Missing);
    };
    if task.status != "completed" {
        return (task.status.clone(), ResultSummaryStatus::Inconclusive);
    }
    if row_count == 0 {
        if task.total_items == 0 && task.succeeded == 0 && task.failed == 0 && task.cancelled == 0 {
            return (task.status.clone(), ResultSummaryStatus::Missing);
        }
        return (task.status.clone(), ResultSummaryStatus::Inconclusive);
    }
    let succeeded = rows.iter().filter(|row| row.status == "succeeded").count() as i64;
    let failed = rows.iter().filter(|row| row.status == "failed").count() as i64;
    let cancelled = rows.iter().filter(|row| row.status == "cancelled").count() as i64;
    let counts_match = task.total_items == row_count as i64
        && task.succeeded == succeeded
        && task.failed == failed
        && task.cancelled == cancelled;
    if !counts_match || rows.iter().any(|row| row.status != "succeeded") || inconclusive_count > 0 {
        return (task.status.clone(), ResultSummaryStatus::Inconclusive);
    }
    if missing_count > 0 {
        return (task.status.clone(), ResultSummaryStatus::Missing);
    }
    (task.status.clone(), ResultSummaryStatus::Pass)
}

/// 判断任务级状态是否已经使非空结果集合不可裁决。
fn task_has_inconclusive_state(task: Option<&TaskHeader>, rows: &[CanonicalResultRow]) -> bool {
    let Some(task) = task else {
        return false;
    };
    if task.status != "completed" {
        return true;
    }
    if task.total_items != rows.len() as i64 {
        return true;
    }
    let succeeded = rows.iter().filter(|row| row.status == "succeeded").count() as i64;
    let failed = rows.iter().filter(|row| row.status == "failed").count() as i64;
    let cancelled = rows.iter().filter(|row| row.status == "cancelled").count() as i64;
    task.succeeded != succeeded
        || task.failed != failed
        || task.cancelled != cancelled
        || rows.iter().any(|row| row.status != "succeeded")
}

/// 对任意稳定 serde payload 做紧凑 JSON SHA-256。
fn hash_serialized<T: Serialize>(value: &T) -> Result<String, ResultSummaryError> {
    Ok(sha256_hex(&serde_json::to_vec(value)?))
}

/// 对原始字节计算全小写 SHA-256 十六进制。
fn sha256_hex(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}

/// 以固定小缓冲区流式计算文件 SHA-256，避免把数据库整体载入内存。
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
    Ok(format!("{:x}", digest.finalize()))
}

/// 把 SQLite BLOB 以不丢失原始字节的全小写十六进制表示写入 JSON payload。
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

    /// 验证 Windows/Unix 使用真实平台身份，其他平台明确拒绝而不降级为长度/mtime。
    #[test]
    fn file_identity_platform_contract() {
        let executable = std::env::current_exe().expect("测试进程路径");
        let file = File::open(executable).expect("打开测试进程");
        let result = file_identity(&file);
        #[cfg(any(windows, unix))]
        assert!(result.is_ok(), "受支持平台必须提供文件身份");
        #[cfg(not(any(windows, unix)))]
        assert!(matches!(
            result,
            Err(ResultSummaryError::UnsupportedFileIdentity)
        ));
    }
}
