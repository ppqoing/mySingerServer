//! 删除批次保护条件，以及成功后位置墓碑和重复组即时收缩事务。

use dedup_core::{AnalysisRunId, ContentKey, DeleteMode, DisplayPath, LocationKey, NormalizedPath};
use rusqlite::{OptionalExtension, Transaction, params};
use uuid::Uuid;

use crate::{
    NodeStore, ScannedPath, StoreError,
    content::encode_file,
    open::{fixed_bytes, sqlite_integer},
    outbox::append_sync_change,
    rows::RowEncoder,
};

/// 删除执行器可提交的四种固定结果。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum DeleteOutcome {
    /// 文件成功进入 Windows 回收站。
    Recycled,
    /// 文件成功永久删除。
    Deleted,
    /// 身份变化或文件不存在，按规则跳过。
    Skipped,
    /// 文件系统操作失败。
    Failed,
}

/// 一个删除执行结果；重复提交相同成功结果是幂等操作。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct DeleteResult {
    /// 计划项 ID。
    pub item_id: String,
    /// 执行结果。
    pub outcome: DeleteOutcome,
    /// 失败或跳过的简短原因。
    pub message: Option<String>,
}

impl DeleteResult {
    /// 创建一个由节点文件系统边界返回的结果。
    pub const fn new(item_id: String, outcome: DeleteOutcome, message: Option<String>) -> Self {
        Self {
            item_id,
            outcome,
            message,
        }
    }
}

/// 创建批次后交给删除执行器的一项不可变身份预期。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct PlannedDeleteItem {
    /// 计划项 ID。
    pub item_id: String,
    /// 所属重复组。
    pub group_id: String,
    /// 当前本机文件位置。
    pub location: LocationKey,
    /// 删除前必须重新验证的 MD5 和大小。
    pub expected: ContentKey,
}

/// 一次删除批次的执行计划。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct DeleteBatchPlan {
    /// UUID v7 字符串形式的批次 ID。
    pub batch_id: String,
    /// 默认回收站或用户明确切换的永久删除。
    pub mode: DeleteMode,
    /// 按组和位置稳定排序的删除项。
    pub items: Vec<PlannedDeleteItem>,
}

impl NodeStore {
    /// 在删除边界一次性验证每组至少一个活动 Keep，并冻结所有 Delete 身份。
    pub fn create_delete_batch(
        &mut self,
        run_id: AnalysisRunId,
        group_ids: &[String],
        mode: DeleteMode,
        now_ms: i64,
    ) -> Result<DeleteBatchPlan, StoreError> {
        let transaction = self.connection.transaction()?;
        let mut planned = Vec::new();
        for group_id in group_ids {
            let keep_count: i64 = transaction.query_row(
                "SELECT COUNT(*)
                 FROM group_members gm
                 JOIN review_marks rm ON rm.analysis_run_id=gm.analysis_run_id
                   AND rm.group_id=gm.group_id AND rm.machine_id=gm.machine_id
                   AND rm.normalized_path=gm.normalized_path AND rm.decision='keep'
                 JOIN files f ON f.machine_id=gm.machine_id
                   AND f.normalized_path=gm.normalized_path AND f.active=1
                 WHERE gm.analysis_run_id=?1 AND gm.group_id=?2 AND gm.active=1",
                params![run_id.as_uuid().to_string(), group_id],
                |row| row.get(0),
            )?;
            if keep_count == 0 {
                return Err(StoreError::MissingKeep(group_id.clone()));
            }
            let rows = {
                let mut statement = transaction.prepare(
                    "SELECT gm.machine_id,gm.normalized_path,gm.md5,gm.file_size
                     FROM group_members gm
                     JOIN review_marks rm ON rm.analysis_run_id=gm.analysis_run_id
                       AND rm.group_id=gm.group_id AND rm.machine_id=gm.machine_id
                       AND rm.normalized_path=gm.normalized_path AND rm.decision='delete'
                     JOIN files f ON f.machine_id=gm.machine_id
                       AND f.normalized_path=gm.normalized_path AND f.active=1
                     JOIN contents c ON c.content_id=f.content_id
                       AND c.md5=gm.md5 AND c.file_size=gm.file_size
                     WHERE gm.analysis_run_id=?1 AND gm.group_id=?2 AND gm.active=1
                     ORDER BY gm.machine_id,gm.normalized_path",
                )?;
                statement
                    .query_map(params![run_id.as_uuid().to_string(), group_id], |row| {
                        Ok((
                            row.get::<_, String>(0)?,
                            row.get::<_, String>(1)?,
                            row.get::<_, Vec<u8>>(2)?,
                            row.get::<_, i64>(3)?,
                        ))
                    })?
                    .collect::<Result<Vec<_>, _>>()?
            };
            for (machine, path, md5, size) in rows {
                planned.push(PlannedDeleteItem {
                    item_id: Uuid::now_v7().to_string(),
                    group_id: group_id.clone(),
                    location: LocationKey::new(
                        dedup_core::MachineId::parse(&machine)?,
                        NormalizedPath::new(path)?,
                    ),
                    expected: ContentKey::new(fixed_bytes(md5, "group_members.md5")?, size as u64),
                });
            }
        }
        if planned.is_empty() {
            return Err(StoreError::InvalidState(
                "删除批次没有明确 Delete 成员".into(),
            ));
        }
        let batch_id = Uuid::now_v7().to_string();
        transaction.execute(
            "INSERT INTO delete_batches(
               delete_batch_id,analysis_run_id,mode,status,created_at_ms)
             VALUES(?1,?2,?3,'queued',?4)",
            params![
                batch_id,
                run_id.as_uuid().to_string(),
                delete_mode_name(mode),
                now_ms
            ],
        )?;
        for item in &planned {
            transaction.execute(
                "INSERT INTO delete_items(
                   delete_item_id,delete_batch_id,group_id,machine_id,normalized_path,
                   expected_md5,expected_size,status)
                 VALUES(?1,?2,?3,?4,?5,?6,?7,'queued')",
                params![
                    item.item_id,
                    batch_id,
                    item.group_id,
                    item.location.machine_id().as_str(),
                    item.location.normalized_path().as_str(),
                    item.expected.md5().as_slice(),
                    sqlite_integer(item.expected.file_size())?
                ],
            )?;
        }
        transaction.commit()?;
        Ok(DeleteBatchPlan {
            batch_id,
            mode,
            items: planned,
        })
    }

    /// 原子保存执行结果；只有 recycled/deleted 才写墓碑并立即更新重复组。
    pub fn apply_delete_results(
        &mut self,
        batch_id: &str,
        results: &[DeleteResult],
    ) -> Result<(), StoreError> {
        let transaction = self.connection.transaction()?;
        let run_id: String = transaction.query_row(
            "SELECT analysis_run_id FROM delete_batches WHERE delete_batch_id=?1",
            [batch_id],
            |row| row.get(0),
        )?;
        for result in results {
            apply_one_result(&transaction, batch_id, &run_id, result)?;
        }
        let remaining: i64 = transaction.query_row(
            "SELECT COUNT(*) FROM delete_items
             WHERE delete_batch_id=?1 AND status IN ('queued','running')",
            [batch_id],
            |row| row.get(0),
        )?;
        transaction.execute(
            "UPDATE delete_batches SET status=?2 WHERE delete_batch_id=?1",
            params![
                batch_id,
                if remaining == 0 {
                    "completed"
                } else {
                    "running"
                }
            ],
        )?;
        transaction.commit()?;
        Ok(())
    }

    /// 判断一个位置是否仍是当前节点的活动文件。
    pub fn location_is_active(&self, location: &LocationKey) -> Result<bool, StoreError> {
        let active: Option<i64> = self
            .connection
            .query_row(
                "SELECT active FROM files WHERE machine_id=?1 AND normalized_path=?2",
                params![
                    location.machine_id().as_str(),
                    location.normalized_path().as_str()
                ],
                |row| row.get(0),
            )
            .optional()?;
        Ok(active == Some(1))
    }
}

struct StoredDeleteItem {
    group_id: String,
    machine_id: String,
    normalized_path: String,
    expected_md5: [u8; 16],
    expected_size: u64,
    status: String,
}

fn apply_one_result(
    transaction: &Transaction<'_>,
    batch_id: &str,
    run_id: &str,
    result: &DeleteResult,
) -> Result<(), StoreError> {
    let raw: (String, String, String, Vec<u8>, i64, String) = transaction.query_row(
        "SELECT group_id,machine_id,normalized_path,expected_md5,expected_size,status
         FROM delete_items WHERE delete_item_id=?1 AND delete_batch_id=?2",
        params![result.item_id, batch_id],
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
    )?;
    let item = StoredDeleteItem {
        group_id: raw.0,
        machine_id: raw.1,
        normalized_path: raw.2,
        expected_md5: fixed_bytes(raw.3, "delete_items.expected_md5")?,
        expected_size: raw.4 as u64,
        status: raw.5,
    };
    let target_status = result.outcome.as_str();
    if item.status == target_status {
        return Ok(());
    }
    if !matches!(item.status.as_str(), "queued" | "running") {
        return Err(StoreError::InvalidState(format!(
            "删除项 {} 已处于 {}",
            result.item_id, item.status
        )));
    }
    transaction.execute(
        "UPDATE delete_items SET status=?2,message=?3 WHERE delete_item_id=?1",
        params![result.item_id, target_status, result.message],
    )?;
    if matches!(
        result.outcome,
        DeleteOutcome::Recycled | DeleteOutcome::Deleted
    ) {
        apply_successful_delete(transaction, run_id, &item, result.outcome)?;
    }
    Ok(())
}

fn apply_successful_delete(
    transaction: &Transaction<'_>,
    run_id: &str,
    item: &StoredDeleteItem,
    outcome: DeleteOutcome,
) -> Result<(), StoreError> {
    let display_path: String = transaction.query_row(
        "SELECT display_path FROM files
         WHERE machine_id=?1 AND normalized_path=?2 AND active=1",
        params![item.machine_id, item.normalized_path],
        |row| row.get(0),
    )?;
    transaction.execute(
        "UPDATE files SET active=0 WHERE machine_id=?1 AND normalized_path=?2",
        params![item.machine_id, item.normalized_path],
    )?;
    let scanned = ScannedPath::new(
        NormalizedPath::new(&item.normalized_path)?,
        DisplayPath::new(display_path)?,
        item.expected_size,
    );
    append_sync_change(
        transaction,
        "file",
        encode_file(
            &item.machine_id,
            &scanned,
            ContentKey::new(item.expected_md5, item.expected_size),
            false,
        ),
    )?;
    transaction.execute(
        "INSERT INTO deletion_tombstones(machine_id,normalized_path,md5,file_size,outcome)
         VALUES(?1,?2,?3,?4,?5)
         ON CONFLICT(machine_id,normalized_path) DO UPDATE SET
           md5=excluded.md5,file_size=excluded.file_size,outcome=excluded.outcome",
        params![
            item.machine_id,
            item.normalized_path,
            item.expected_md5.as_slice(),
            sqlite_integer(item.expected_size)?,
            outcome.as_str()
        ],
    )?;
    append_sync_change(
        transaction,
        "deletion_tombstone",
        encode_deletion_tombstone(
            &item.machine_id,
            &item.normalized_path,
            ContentKey::new(item.expected_md5, item.expected_size),
            outcome,
        ),
    )?;

    let was_representative: i64 = transaction.query_row(
        "SELECT representative FROM group_members
         WHERE analysis_run_id=?1 AND group_id=?2
           AND machine_id=?3 AND normalized_path=?4",
        params![run_id, item.group_id, item.machine_id, item.normalized_path],
        |row| row.get(0),
    )?;
    transaction.execute(
        "DELETE FROM group_members
         WHERE analysis_run_id=?1 AND group_id=?2
           AND machine_id=?3 AND normalized_path=?4",
        params![run_id, item.group_id, item.machine_id, item.normalized_path],
    )?;
    let remaining: i64 = transaction.query_row(
        "SELECT COUNT(*) FROM group_members
         WHERE analysis_run_id=?1 AND group_id=?2 AND active=1",
        params![run_id, item.group_id],
        |row| row.get(0),
    )?;
    if remaining < 2 {
        transaction.execute(
            "DELETE FROM duplicate_groups WHERE analysis_run_id=?1 AND group_id=?2",
            params![run_id, item.group_id],
        )?;
    } else if was_representative != 0 {
        select_new_representative(transaction, run_id, &item.group_id)?;
    }
    Ok(())
}

fn select_new_representative(
    transaction: &Transaction<'_>,
    run_id: &str,
    group_id: &str,
) -> Result<(), StoreError> {
    let selected: (String, String, Vec<u8>, i64) = transaction.query_row(
        "SELECT gm.machine_id,gm.normalized_path,gm.md5,gm.file_size
         FROM group_members gm
         JOIN review_marks rm ON rm.analysis_run_id=gm.analysis_run_id
           AND rm.group_id=gm.group_id AND rm.machine_id=gm.machine_id
           AND rm.normalized_path=gm.normalized_path AND rm.decision='keep'
         JOIN files f ON f.machine_id=gm.machine_id
           AND f.normalized_path=gm.normalized_path AND f.active=1
         WHERE gm.analysis_run_id=?1 AND gm.group_id=?2 AND gm.active=1
         ORDER BY gm.machine_id,gm.normalized_path LIMIT 1",
        params![run_id, group_id],
        |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?, row.get(3)?)),
    )?;
    transaction.execute(
        "UPDATE group_members SET representative=0
         WHERE analysis_run_id=?1 AND group_id=?2",
        params![run_id, group_id],
    )?;
    transaction.execute(
        "UPDATE group_members SET representative=1
         WHERE analysis_run_id=?1 AND group_id=?2
           AND machine_id=?3 AND normalized_path=?4",
        params![run_id, group_id, selected.0, selected.1],
    )?;
    transaction.execute(
        "UPDATE duplicate_groups SET representative_md5=?3,representative_size=?4
         WHERE analysis_run_id=?1 AND group_id=?2",
        params![run_id, group_id, selected.2, selected.3],
    )?;
    Ok(())
}

impl DeleteOutcome {
    pub(crate) const fn as_str(self) -> &'static str {
        match self {
            Self::Recycled => "recycled",
            Self::Deleted => "deleted",
            Self::Skipped => "skipped",
            Self::Failed => "failed",
        }
    }
}

pub(crate) fn encode_deletion_tombstone(
    machine_id: &str,
    normalized_path: &str,
    content: ContentKey,
    outcome: DeleteOutcome,
) -> Vec<u8> {
    RowEncoder::new(1)
        .text(machine_id)
        .text(normalized_path)
        .bytes(&content.md5())
        .u64(content.file_size())
        .text(outcome.as_str())
        .finish()
}

const fn delete_mode_name(mode: DeleteMode) -> &'static str {
    match mode {
        DeleteMode::RecycleBin => "recycle_bin",
        DeleteMode::Permanent => "permanent",
    }
}
