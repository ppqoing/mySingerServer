//! 扫描清单的瞬态收尾：在一个 SQLite 事务内合并位置并失活未见文件。

use dedup_core::{ContentKey, NormalizedPath};
use rusqlite::{Statement, Transaction, params};

use crate::{
    NodeStore, ScannedPath, StoreError,
    content::encode_file,
    maintenance::bump_library_revision,
    open::{fixed_bytes, sqlite_integer},
    outbox::outbox_high_seq_from,
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
        apply_resolved_files(&transaction, machine_id.as_str())?;
        deactivate_stale_files(&transaction, machine_id.as_str())?;

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
            "CREATE TEMP TABLE IF NOT EXISTS scan_finalize_roots(
                 normalized_path TEXT PRIMARY KEY
             );
             CREATE TEMP TABLE IF NOT EXISTS scan_finalize_seen_paths(
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
        "DELETE FROM scan_finalize_roots;
         DELETE FROM scan_finalize_seen_paths;
         DELETE FROM scan_finalize_resolved_files;",
    )?;

    let mut root_statement = transaction
        .prepare_cached("INSERT INTO scan_finalize_roots(normalized_path) VALUES(?1)")?;
    for chunk in manifest.roots.chunks(MAX_TEMP_ROWS) {
        for root in chunk {
            root_statement.execute([root.as_str()])?;
        }
    }
    drop(root_statement);

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

/// JOIN 后的一条解析项，包含内容完整性和已有位置关系，避免逐项 SELECT。
struct ResolvedManifestRow {
    normalized_path: String,
    display_path: String,
    file_size: i64,
    md5: Vec<u8>,
    content_id: Option<i64>,
    base_complete: Option<i64>,
    existing_display_path: Option<String>,
    existing_file_size: Option<i64>,
    existing_content_id: Option<i64>,
    existing_active: Option<i64>,
}

/// 一条待失活位置，内容键随 JOIN 一并读出，避免逐项查询内容表。
struct StaleFileRow {
    normalized_path: String,
    display_path: String,
    file_size: i64,
    md5: Vec<u8>,
    content_file_size: i64,
}

/// 分批读取并应用解析项；SQLite SELECT 数量只随千行批次增长。
fn apply_resolved_files(transaction: &Transaction<'_>, machine_id: &str) -> Result<(), StoreError> {
    let mut upsert_statement = transaction.prepare(
        "INSERT INTO files(
             machine_id,normalized_path,display_path,file_size,content_id,active
         ) VALUES(?1,?2,?3,?4,?5,1)
         ON CONFLICT(machine_id,normalized_path) DO UPDATE SET
             display_path=excluded.display_path,
             file_size=excluded.file_size,
             content_id=excluded.content_id,
             active=1",
    )?;
    let mut outbox_statement =
        transaction.prepare("INSERT INTO sync_outbox(entity_kind,payload) VALUES(?1,?2)")?;
    let mut after_path = None;
    loop {
        let batch = read_resolved_batch(transaction, machine_id, after_path.as_deref())?;
        let Some(last_path) = batch.last().map(|row| row.normalized_path.clone()) else {
            break;
        };
        apply_resolved_batch(
            transaction,
            machine_id,
            &batch,
            &mut upsert_statement,
            &mut outbox_statement,
        )?;
        after_path = Some(last_path);
    }
    Ok(())
}

/// 以规范路径游标分批读取 resolved JOIN，单批最多一千行。
fn read_resolved_batch(
    transaction: &Transaction<'_>,
    machine_id: &str,
    after_path: Option<&str>,
) -> Result<Vec<ResolvedManifestRow>, StoreError> {
    let mut statement = transaction.prepare(
        "SELECT r.normalized_path,r.display_path,r.file_size,r.md5,
                c.content_id,c.base_complete,
                f.display_path,f.file_size,f.content_id,f.active
         FROM temp.scan_finalize_resolved_files r
         LEFT JOIN contents c ON c.md5=r.md5 AND c.file_size=r.file_size
         LEFT JOIN files f ON f.machine_id=?1 AND f.normalized_path=r.normalized_path
         WHERE (?2 IS NULL OR r.normalized_path>?2)
         ORDER BY r.normalized_path
         LIMIT ?3",
    )?;
    let mut rows = statement.query(params![machine_id, after_path, MAX_TEMP_ROWS as i64])?;
    let mut batch = Vec::with_capacity(MAX_TEMP_ROWS);
    while let Some(row) = rows.next()? {
        batch.push(ResolvedManifestRow {
            normalized_path: row.get(0)?,
            display_path: row.get(1)?,
            file_size: row.get(2)?,
            md5: row.get(3)?,
            content_id: row.get(4)?,
            base_complete: row.get(5)?,
            existing_display_path: row.get(6)?,
            existing_file_size: row.get(7)?,
            existing_content_id: row.get(8)?,
            existing_active: row.get(9)?,
        });
    }
    Ok(batch)
}

/// 应用一个 resolved 批次；缺内容或无变化均在此边界处理。
fn apply_resolved_batch(
    transaction: &Transaction<'_>,
    machine_id: &str,
    batch: &[ResolvedManifestRow],
    upsert_statement: &mut Statement<'_>,
    outbox_statement: &mut Statement<'_>,
) -> Result<(), StoreError> {
    for row in batch {
        let Some(content_id) = row.content_id else {
            return Err(StoreError::InvalidState(format!(
                "解析清单引用了不存在的内容: {}",
                row.normalized_path
            )));
        };
        if row.base_complete != Some(1) {
            return Err(StoreError::InvalidState(format!(
                "解析清单引用了未完成的内容: {}",
                row.normalized_path
            )));
        }
        let file_size = u64::try_from(row.file_size)
            .map_err(|_| StoreError::InvalidState("临时解析清单大小不能为负数".into()))?;
        let content_key = ContentKey::new(
            fixed_bytes(row.md5.clone(), "scan_finalize.md5")?,
            file_size,
        );
        let scanned = ScannedPath::new(
            NormalizedPath::new(&row.normalized_path)?,
            dedup_core::DisplayPath::new(&row.display_path)?,
            file_size,
        );
        let display_path = scanned
            .display_path
            .as_path()
            .to_string_lossy()
            .into_owned();
        let file_size_sql = sqlite_integer(file_size)?;
        let unchanged = row.existing_display_path.as_deref() == Some(display_path.as_str())
            && row.existing_file_size == Some(file_size_sql)
            && row.existing_content_id == Some(content_id)
            && row.existing_active == Some(1);
        if unchanged {
            continue;
        }
        upsert_statement.execute(params![
            machine_id,
            scanned.normalized_path.as_str(),
            display_path,
            file_size_sql,
            content_id,
        ])?;
        append_prepared_sync_change(
            transaction,
            outbox_statement,
            "file",
            encode_file(machine_id, &scanned, content_key, true),
        )?;
    }
    Ok(())
}

/// 在一个已准备的 INSERT 上追加 outbox，避免每条位置重新解析 SQL。
fn append_prepared_sync_change(
    transaction: &Transaction<'_>,
    statement: &mut Statement<'_>,
    entity_kind: &str,
    payload: Vec<u8>,
) -> Result<u64, StoreError> {
    statement.execute(params![entity_kind, payload])?;
    Ok(transaction.last_insert_rowid() as u64)
}

/// 按根组件和 seen 表在 SQL 侧过滤活动位置，并以路径游标流式处理。
fn deactivate_stale_files(
    transaction: &Transaction<'_>,
    machine_id: &str,
) -> Result<(), StoreError> {
    let mut update_statement = transaction.prepare(
        "UPDATE files SET active=0
         WHERE machine_id=?1 AND normalized_path=?2 AND active=1",
    )?;
    let mut outbox_statement =
        transaction.prepare("INSERT INTO sync_outbox(entity_kind,payload) VALUES(?1,?2)")?;
    let mut after_path = None;
    loop {
        let batch = read_stale_batch(transaction, machine_id, after_path.as_deref())?;
        let Some(last_path) = batch.last().map(|row| row.normalized_path.clone()) else {
            break;
        };
        for row in &batch {
            let file_size = u64::try_from(row.file_size)
                .map_err(|_| StoreError::InvalidState("文件库中的大小不能为负数".into()))?;
            let content_file_size = u64::try_from(row.content_file_size)
                .map_err(|_| StoreError::InvalidState("内容库中的大小不能为负数".into()))?;
            let scanned = ScannedPath::new(
                NormalizedPath::new(&row.normalized_path)?,
                dedup_core::DisplayPath::new(&row.display_path)?,
                file_size,
            );
            update_statement.execute(params![machine_id, row.normalized_path])?;
            append_prepared_sync_change(
                transaction,
                &mut outbox_statement,
                "file",
                encode_file(
                    machine_id,
                    &scanned,
                    ContentKey::new(
                        fixed_bytes(row.md5.clone(), "contents.md5")?,
                        content_file_size,
                    ),
                    false,
                ),
            )?;
        }
        after_path = Some(last_path);
    }
    Ok(())
}

/// 以路径游标读取一个根范围内的失活批次；SQL 不使用 LIKE，避免通配符误匹配。
fn read_stale_batch(
    transaction: &Transaction<'_>,
    machine_id: &str,
    after_path: Option<&str>,
) -> Result<Vec<StaleFileRow>, StoreError> {
    let mut statement = transaction.prepare(
        "SELECT f.normalized_path,f.display_path,f.file_size,c.md5,c.file_size
         FROM files f
         JOIN contents c ON c.content_id=f.content_id
         WHERE f.machine_id=?1 AND f.active=1
           AND (?2 IS NULL OR f.normalized_path>?2)
           AND NOT EXISTS (
             SELECT 1 FROM temp.scan_finalize_seen_paths seen
             WHERE seen.normalized_path=f.normalized_path
           )
           AND EXISTS (
             SELECT 1 FROM temp.scan_finalize_roots root
             WHERE f.normalized_path=root.normalized_path
                OR (
                   substr(root.normalized_path,-1)=char(92)
                   AND substr(f.normalized_path,1,length(root.normalized_path))=root.normalized_path
                )
                OR (
                   substr(root.normalized_path,-1)<>char(92)
                   AND substr(f.normalized_path,1,length(root.normalized_path)+1)=
                       root.normalized_path||char(92)
                )
           )
         ORDER BY f.normalized_path
         LIMIT ?3",
    )?;
    let mut rows = statement.query(params![machine_id, after_path, MAX_TEMP_ROWS as i64])?;
    let mut batch = Vec::with_capacity(MAX_TEMP_ROWS);
    while let Some(row) = rows.next()? {
        batch.push(StaleFileRow {
            normalized_path: row.get(0)?,
            display_path: row.get(1)?,
            file_size: row.get(2)?,
            md5: row.get(3)?,
            content_file_size: row.get(4)?,
        });
    }
    Ok(batch)
}

#[cfg(test)]
mod tests {
    use std::sync::{LazyLock, Mutex};

    use dedup_core::{DisplayPath, MachineId};
    use rusqlite::{
        params,
        trace::{TraceEvent, TraceEventCodes},
    };

    use super::*;

    static TRACE_ROWS: LazyLock<Mutex<usize>> = LazyLock::new(|| Mutex::new(0));
    static TRACE_SELECTS: LazyLock<Mutex<usize>> = LazyLock::new(|| Mutex::new(0));
    static TRACE_TEST_LOCK: LazyLock<Mutex<()>> = LazyLock::new(|| Mutex::new(()));

    /// 统计收尾事务执行的 SELECT 和返回行数，避免性能测试依赖源代码文本。
    fn trace_inventory(event: TraceEvent<'_>) {
        match event {
            TraceEvent::Stmt(_, sql) if sql.trim_start().starts_with("SELECT") => {
                *TRACE_SELECTS.lock().unwrap() += 1;
            }
            TraceEvent::Row(_) => *TRACE_ROWS.lock().unwrap() += 1,
            _ => {}
        }
    }

    /// 返回行为测试固定使用的物理机器身份。
    fn test_machine() -> MachineId {
        MachineId::parse("73bdb7a3377f81376a84f316b3ee1555e345afbfa87aa99c77b1bfcc364c4cae")
            .unwrap()
    }

    /// 在同一事务中创建已完成内容和活动位置，减少测试夹具自身的 SQL 噪声。
    fn seed_files(store: &mut NodeStore, paths: impl IntoIterator<Item = String>) {
        let transaction = store.connection.transaction().unwrap();
        let machine_id = test_machine();
        for (index, path) in paths.into_iter().enumerate() {
            let md5 = (index as u128).to_le_bytes();
            let normalized = NormalizedPath::new(&path).unwrap();
            transaction
                .execute(
                    "INSERT INTO contents(md5,file_size,media_kind,base_complete)
                     VALUES(?1,?2,'other',1)",
                    params![md5.as_slice(), (index + 1) as i64],
                )
                .unwrap();
            let content_id = transaction.last_insert_rowid();
            transaction
                .execute(
                    "INSERT INTO files(
                         machine_id,normalized_path,display_path,file_size,content_id,active
                     ) VALUES(?1,?2,?3,?4,?5,1)",
                    params![
                        machine_id.as_str(),
                        normalized.as_str(),
                        path,
                        (index + 1) as i64,
                        content_id
                    ],
                )
                .unwrap();
        }
        transaction.commit().unwrap();
    }

    /// 清空本轮 SQLite trace 计数。
    fn reset_trace() {
        *TRACE_ROWS.lock().unwrap() = 0;
        *TRACE_SELECTS.lock().unwrap() = 0;
    }

    /// 大量根外活动位置应由 SQL 根范围先过滤，不能先物化整机结果。
    #[test]
    fn stale_manifest_filters_large_external_set_before_materializing_rows() {
        let _test_lock = TRACE_TEST_LOCK.lock().unwrap();
        let mut store = NodeStore::open_in_memory(test_machine()).unwrap();
        let mut paths = (0..2_001)
            .map(|index| format!(r"D:\AB\outside-{index:04}.bin"))
            .collect::<Vec<_>>();
        paths.push(r"D:\A%\target.bin".into());
        assert!(
            !NormalizedPath::new(r"D:\AB\outside-0000.bin")
                .unwrap()
                .is_within(&NormalizedPath::new(r"D:\A%").unwrap())
        );
        seed_files(&mut store, paths);

        reset_trace();
        store.connection.trace_v2(
            TraceEventCodes::SQLITE_TRACE_STMT | TraceEventCodes::SQLITE_TRACE_ROW,
            Some(trace_inventory),
        );
        store
            .finalize_scan_manifest(
                &ScanFinalizeInput {
                    roots: vec![NormalizedPath::new(r"D:\A%").unwrap()],
                    seen_paths: Vec::new(),
                    resolved_files: Vec::new(),
                },
                100,
            )
            .unwrap();
        store.connection.trace_v2(TraceEventCodes::empty(), None);

        assert!(
            !store
                .is_location_active(&NormalizedPath::new(r"D:\A%\target.bin").unwrap())
                .unwrap()
        );
        assert!(
            store
                .is_location_active(&NormalizedPath::new(r"D:\AB\outside-0000.bin").unwrap())
                .unwrap()
        );
        assert!(
            *TRACE_ROWS.lock().unwrap() < 100,
            "根外位置不应进入收尾查询结果，实际返回 {} 行",
            *TRACE_ROWS.lock().unwrap()
        );
    }

    /// 超过一千条解析项仍应按批次 JOIN 查询，不能对每项追加内容和位置 SELECT。
    #[test]
    fn resolved_manifest_uses_bounded_join_queries_without_n_plus_one_selects() {
        let _test_lock = TRACE_TEST_LOCK.lock().unwrap();
        let mut store = NodeStore::open_in_memory(test_machine()).unwrap();
        let count = 1_001;
        let mut seen_paths = Vec::with_capacity(count);
        let mut resolved_files = Vec::with_capacity(count);
        let transaction = store.connection.transaction().unwrap();
        for index in 0..count {
            let path = format!(r"D:\Resolved\file-{index:04}.bin");
            let md5 = (index as u128).to_le_bytes();
            let file_size = (index + 1) as i64;
            transaction
                .execute(
                    "INSERT INTO contents(md5,file_size,media_kind,base_complete)
                     VALUES(?1,?2,'other',1)",
                    params![md5.as_slice(), file_size],
                )
                .unwrap();
            seen_paths.push(NormalizedPath::new(&path).unwrap());
            resolved_files.push(ResolvedScanFile {
                scanned: ScannedPath::new(
                    NormalizedPath::new(&path).unwrap(),
                    DisplayPath::new(&path).unwrap(),
                    (index + 1) as u64,
                ),
                content: ContentKey::new(md5, (index + 1) as u64),
            });
        }
        transaction.commit().unwrap();

        reset_trace();
        store
            .connection
            .trace_v2(TraceEventCodes::SQLITE_TRACE_STMT, Some(trace_inventory));
        let result = store
            .finalize_scan_manifest(
                &ScanFinalizeInput {
                    roots: vec![NormalizedPath::new(r"D:\Resolved").unwrap()],
                    seen_paths,
                    resolved_files,
                },
                100,
            )
            .unwrap();
        store.connection.trace_v2(TraceEventCodes::empty(), None);

        assert_eq!(result.library_revision, 1);
        assert!(
            *TRACE_SELECTS.lock().unwrap() < 32,
            "resolved JOIN 不应产生 N+1 SELECT，实际执行 {} 条",
            *TRACE_SELECTS.lock().unwrap()
        );
        let resolved_count: i64 = store
            .connection
            .query_row(
                "SELECT COUNT(*) FROM files WHERE machine_id=?1 AND active=1",
                [test_machine().as_str()],
                |row| row.get(0),
            )
            .unwrap();
        assert_eq!(resolved_count, count as i64);
    }
}
