//! 扫描清单的瞬态收尾：在一个 SQLite 事务内合并位置并失活未见文件。

use dedup_core::{ContentKey, NormalizedPath};
use rusqlite::{OptionalExtension, Transaction, params};

use crate::{
    NodeStore, ScannedPath, StoreError,
    content::{content_key_in_transaction, encode_file},
    maintenance::bump_library_revision,
    open::{fixed_bytes, sqlite_integer},
    outbox::{append_sync_change, outbox_high_seq_from},
};

const MAX_TEMP_ROWS: usize = 1_000;

/// 本轮已经得到完整内容键的扫描文件。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ResolvedScanFile {
    /// 本轮枚举时保留的路径、显示文本和文件大小。
    pub scanned: ScannedPath,
    /// 已经在 SQLite 中写入完整特征的跨边界内容键。
    pub content: ContentKey,
}

/// 当前扫描收尾所需的全部内存清单。
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct ScanFinalizeInput {
    /// 本轮允许失活位置的规范扫描根。
    pub roots: Vec<NormalizedPath>,
    /// 本轮成功枚举到的全部规范路径，包含读取失败的路径。
    pub seen_paths: Vec<NormalizedPath>,
    /// 本轮已经完成缓存命中或计算提交的路径。
    pub resolved_files: Vec<ResolvedScanFile>,
}

/// 扫描清单事务提交后的同步边界。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ScanFinalizeResult {
    /// 本次事务提交时 SQLite outbox 的真实最高序号。
    pub outbox_high_seq: u64,
    /// 本次成功收尾后推进一次的文件库版本。
    pub library_revision: u64,
}

/// 已排序、去重且通过路径关系校验的收尾清单。
struct NormalizedManifest {
    roots: Vec<NormalizedPath>,
    seen_paths: Vec<NormalizedPath>,
    resolved_files: Vec<ResolvedScanFile>,
}

impl NodeStore {
    /// 用当前扫描清单原子更新位置、失活旧位置并推进同步版本。
    ///
    /// 临时表只保存本次调用的输入；正式表、file outbox、outbox 高水位和
    /// `library_revision` 都在同一事务中提交，任何一步失败都会整体回滚。
    pub fn finalize_scan_manifest(
        &mut self,
        input: &ScanFinalizeInput,
        _now_ms: i64,
    ) -> Result<ScanFinalizeResult, StoreError> {
        let manifest = normalize_manifest(input)?;
        self.ensure_temp_manifest_tables()?;

        let machine_id = self.machine_id().clone();
        let transaction = self.connection.transaction()?;
        insert_temp_manifest(&transaction, &manifest)?;
        let resolved_rows = read_resolved_temp_rows(&transaction)?;
        for (normalized_path, display_path, file_size, md5) in resolved_rows {
            let content_key = ContentKey::new(md5, file_size);
            let content_id: Option<i64> = transaction
                .query_row(
                    "SELECT content_id FROM contents
                     WHERE md5=?1 AND file_size=?2 AND base_complete=1",
                    params![content_key.md5().as_slice(), sqlite_integer(file_size)?],
                    |row| row.get(0),
                )
                .optional()?;
            let Some(content_id) = content_id else {
                return Err(StoreError::InvalidState(format!(
                    "解析清单引用了不存在或未完成的内容: {normalized_path}"
                )));
            };

            let scanned = ScannedPath::new(
                NormalizedPath::new(&normalized_path)?,
                dedup_core::DisplayPath::new(display_path)?,
                file_size,
            );
            let file_size = sqlite_integer(scanned.file_size)?;
            let display_path = scanned.display_path.as_path().to_string_lossy();
            let existing: Option<(String, i64, i64, i64)> = transaction
                .query_row(
                    "SELECT display_path,file_size,content_id,active FROM files
                     WHERE machine_id=?1 AND normalized_path=?2",
                    params![machine_id.as_str(), scanned.normalized_path.as_str()],
                    |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?, row.get(3)?)),
                )
                .optional()?;
            let unchanged = existing.is_some_and(|(old_display, old_size, old_content, active)| {
                old_display == display_path
                    && old_size == file_size
                    && old_content == content_id
                    && active == 1
            });
            if !unchanged {
                transaction.execute(
                    "INSERT INTO files(machine_id,normalized_path,display_path,file_size,content_id,active)
                     VALUES(?1,?2,?3,?4,?5,1)
                     ON CONFLICT(machine_id,normalized_path) DO UPDATE SET
                       display_path=excluded.display_path,
                       file_size=excluded.file_size,
                       content_id=excluded.content_id,
                       active=1",
                    params![
                        machine_id.as_str(),
                        scanned.normalized_path.as_str(),
                        display_path.as_ref(),
                        file_size,
                        content_id,
                    ],
                )?;
                append_sync_change(
                    &transaction,
                    "file",
                    encode_file(machine_id.as_str(), &scanned, content_key, true),
                )?;
            }
        }

        let stale_rows = read_stale_rows(&transaction, machine_id.as_str())?;
        for (normalized_path, display_path, file_size, content_id) in stale_rows {
            let normalized_path_value = NormalizedPath::new(&normalized_path)?;
            if !manifest
                .roots
                .iter()
                .any(|root| normalized_path_value.is_within(root))
            {
                continue;
            }
            transaction.execute(
                "UPDATE files SET active=0
                 WHERE machine_id=?1 AND normalized_path=?2 AND active=1",
                params![machine_id.as_str(), normalized_path],
            )?;
            let file_size = u64::try_from(file_size)
                .map_err(|_| StoreError::InvalidState("文件库中的大小不能为负数".into()))?;
            let scanned = ScannedPath::new(
                normalized_path_value,
                dedup_core::DisplayPath::new(display_path)?,
                file_size,
            );
            let content_key =
                content_key_in_transaction(&transaction, crate::ContentId::from_i64(content_id))?;
            append_sync_change(
                &transaction,
                "file",
                encode_file(machine_id.as_str(), &scanned, content_key, false),
            )?;
        }

        let library_revision = bump_library_revision(&transaction)?;
        let outbox_high_seq = outbox_high_seq_from(&transaction)?;
        transaction.commit()?;
        Ok(ScanFinalizeResult {
            outbox_high_seq,
            library_revision,
        })
    }

    /// 确保当前连接拥有扫描收尾所需的临时表；数据刷新由业务事务完成。
    fn ensure_temp_manifest_tables(&self) -> Result<(), StoreError> {
        self.connection.execute_batch(
            "CREATE TEMP TABLE IF NOT EXISTS scan_finalize_seen_paths(
                 normalized_path TEXT PRIMARY KEY
             );
             CREATE TEMP TABLE IF NOT EXISTS scan_finalize_resolved_files(
                 normalized_path TEXT PRIMARY KEY,
                 display_path TEXT NOT NULL,
                 file_size INTEGER NOT NULL,
                 md5 BLOB NOT NULL CHECK(length(md5)=16)
             );",
        )?;
        Ok(())
    }
}

/// 在正式收尾事务内刷新本次输入的临时表；每批最多写入一千行。
fn insert_temp_manifest(
    transaction: &Transaction<'_>,
    manifest: &NormalizedManifest,
) -> Result<(), StoreError> {
    transaction.execute_batch(
        "DELETE FROM scan_finalize_seen_paths;
         DELETE FROM scan_finalize_resolved_files;",
    )?;

    let mut seen_statement = transaction
        .prepare_cached("INSERT INTO scan_finalize_seen_paths(normalized_path) VALUES(?1)")?;
    for chunk in manifest.seen_paths.chunks(MAX_TEMP_ROWS) {
        for path in chunk {
            seen_statement.execute([path.as_str()])?;
        }
    }
    drop(seen_statement);

    let mut resolved_statement = transaction.prepare_cached(
        "INSERT INTO scan_finalize_resolved_files(
             normalized_path,display_path,file_size,md5
         ) VALUES(?1,?2,?3,?4)",
    )?;
    for chunk in manifest.resolved_files.chunks(MAX_TEMP_ROWS) {
        for resolved in chunk {
            resolved_statement.execute(params![
                resolved.scanned.normalized_path.as_str(),
                resolved
                    .scanned
                    .display_path
                    .as_path()
                    .to_string_lossy()
                    .as_ref(),
                sqlite_integer(resolved.scanned.file_size)?,
                resolved.content.md5().as_slice(),
            ])?;
        }
    }
    Ok(())
}

/// 规范化清单，确保同一路径不会在一个事务内指向两个内容。
fn normalize_manifest(input: &ScanFinalizeInput) -> Result<NormalizedManifest, StoreError> {
    let mut roots = input.roots.clone();
    roots.sort();
    roots.dedup();
    if roots.is_empty() {
        return Err(StoreError::InvalidState(
            "扫描收尾至少需要一个根目录".into(),
        ));
    }

    let mut seen_paths = input.seen_paths.clone();
    seen_paths.sort();
    seen_paths.dedup();
    for path in &seen_paths {
        if !roots.iter().any(|root| path.is_within(root)) {
            return Err(StoreError::InvalidState(format!(
                "已见路径不属于扫描根: {path}"
            )));
        }
    }

    let mut resolved_files = input.resolved_files.clone();
    resolved_files.sort_by(|left, right| {
        left.scanned
            .normalized_path
            .cmp(&right.scanned.normalized_path)
            .then_with(|| left.content.cmp(&right.content))
            .then_with(|| {
                left.scanned
                    .display_path
                    .as_path()
                    .to_string_lossy()
                    .cmp(&right.scanned.display_path.as_path().to_string_lossy())
            })
    });
    let mut unique_resolved: Vec<ResolvedScanFile> = Vec::with_capacity(resolved_files.len());
    for resolved in resolved_files {
        if resolved.content.file_size() != resolved.scanned.file_size {
            return Err(StoreError::InvalidState(format!(
                "解析清单文件大小与内容键不一致: {}",
                resolved.scanned.normalized_path
            )));
        }
        if !seen_paths
            .binary_search(&resolved.scanned.normalized_path)
            .is_ok()
        {
            return Err(StoreError::InvalidState(format!(
                "解析路径不在已见清单中: {}",
                resolved.scanned.normalized_path
            )));
        }
        if !roots
            .iter()
            .any(|root| resolved.scanned.normalized_path.is_within(root))
        {
            return Err(StoreError::InvalidState(format!(
                "解析路径不属于扫描根: {}",
                resolved.scanned.normalized_path
            )));
        }
        if let Some(previous) = unique_resolved.last() {
            if previous.scanned.normalized_path == resolved.scanned.normalized_path {
                if previous.content != resolved.content {
                    return Err(StoreError::InvalidState(format!(
                        "同一路径对应多个内容: {}",
                        resolved.scanned.normalized_path
                    )));
                }
                continue;
            }
        }
        unique_resolved.push(resolved);
    }

    Ok(NormalizedManifest {
        roots,
        seen_paths,
        resolved_files: unique_resolved,
    })
}

/// 读取临时解析表，保留已验证的显示路径和内容键。
fn read_resolved_temp_rows(
    transaction: &Transaction<'_>,
) -> Result<Vec<(String, String, u64, [u8; 16])>, StoreError> {
    let mut statement = transaction.prepare(
        "SELECT normalized_path,display_path,file_size,md5
         FROM temp.scan_finalize_resolved_files ORDER BY normalized_path",
    )?;
    statement
        .query_map([], |row| {
            Ok((row.get(0)?, row.get(1)?, row.get(2)?, row.get(3)?))
        })?
        .map(|row| {
            let (path, display, file_size, md5): (String, String, i64, Vec<u8>) = row?;
            let file_size = u64::try_from(file_size)
                .map_err(|_| StoreError::InvalidState("临时解析清单大小不能为负数".into()))?;
            Ok((
                path,
                display,
                file_size,
                fixed_bytes(md5, "scan_finalize.md5")?,
            ))
        })
        .collect()
}

/// 读取当前机器所有未被本轮见到的活动位置，根组件边界在调用方确认。
fn read_stale_rows(
    transaction: &Transaction<'_>,
    machine_id: &str,
) -> Result<Vec<(String, String, i64, i64)>, StoreError> {
    let mut statement = transaction.prepare(
        "SELECT normalized_path,display_path,file_size,content_id
         FROM files
         WHERE machine_id=?1 AND active=1
           AND NOT EXISTS (
             SELECT 1 FROM temp.scan_finalize_seen_paths seen
             WHERE seen.normalized_path=files.normalized_path
           )",
    )?;
    statement
        .query_map([machine_id], |row| {
            Ok((row.get(0)?, row.get(1)?, row.get(2)?, row.get(3)?))
        })?
        .collect::<Result<Vec<_>, _>>()
        .map_err(StoreError::from)
}
