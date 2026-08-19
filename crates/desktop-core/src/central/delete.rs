//! 中心删除计划冻结与节点执行结果落库；物理文件操作始终由对应节点完成。

use dedup_core::{AnalysisRunId, ContentKey, DeleteMode, LocationKey, MachineId, NormalizedPath};
use tokio_postgres::Transaction;
use uuid::Uuid;

use super::{CentralError, CentralStore, pg_i64};

/// 交给某个节点执行的一项不可变删除身份预期。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CentralDeleteItem {
    /// 全局唯一计划项 ID。
    pub item_id: String,
    /// 所属重复组。
    pub group_id: String,
    /// 目标机器与规范路径。
    pub location: LocationKey,
    /// 节点删除前必须复核的 MD5 与大小。
    pub expected: ContentKey,
}

/// 中心一次性冻结后按机器派发的删除批次。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CentralDeletePlan {
    /// 全局唯一批次 ID。
    pub batch_id: String,
    /// 默认回收站或用户显式切换的永久删除。
    pub mode: DeleteMode,
    /// 按组、机器和路径稳定排序的计划项。
    pub items: Vec<CentralDeleteItem>,
}

/// 节点文件系统边界允许返回的四种结果。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum CentralDeleteOutcome {
    /// 成功进入 Windows 回收站。
    Recycled,
    /// 成功永久删除。
    Deleted,
    /// 身份变化或文件不存在，安全跳过。
    Skipped,
    /// 文件操作失败。
    Failed,
}

/// 一个节点删除结果；成功结果会立即从中心重复组移除对应位置。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CentralDeleteResult {
    /// 对应的计划项 ID。
    pub item_id: String,
    /// 固定结果类别。
    pub outcome: CentralDeleteOutcome,
    /// 跳过或失败原因。
    pub message: Option<String>,
}

impl CentralStore {
    /// 验证每个选中组至少有一个活动 Keep，并冻结所有明确 Delete 成员。
    pub async fn create_delete_plan(
        &mut self,
        run_id: AnalysisRunId,
        group_ids: &[String],
        mode: DeleteMode,
    ) -> Result<CentralDeletePlan, CentralError> {
        let run_text = run_id.as_uuid().to_string();
        let transaction = self.client.transaction().await?;
        let mut items = Vec::new();
        for group_id in group_ids {
            let keep_count: i64 = transaction
                .query_one(
                    "SELECT COUNT(*)
                     FROM group_members gm
                     JOIN review_marks rm ON rm.analysis_run_id=gm.analysis_run_id
                       AND rm.group_id=gm.group_id AND rm.machine_id=gm.machine_id
                       AND rm.normalized_path=gm.normalized_path AND rm.decision='keep'
                     JOIN file_locations f ON f.machine_id=gm.machine_id
                       AND f.normalized_path=gm.normalized_path AND f.active=TRUE
                     WHERE gm.analysis_run_id=$1 AND gm.group_id=$2 AND gm.active=TRUE",
                    &[&run_text, group_id],
                )
                .await?
                .get(0);
            if keep_count == 0 {
                return Err(CentralError::InvalidState(format!(
                    "重复组 {group_id} 没有活动 Keep 成员"
                )));
            }
            let rows = transaction
                .query(
                    "SELECT gm.machine_id,gm.normalized_path,gm.md5,gm.file_size
                     FROM group_members gm
                     JOIN review_marks rm ON rm.analysis_run_id=gm.analysis_run_id
                       AND rm.group_id=gm.group_id AND rm.machine_id=gm.machine_id
                       AND rm.normalized_path=gm.normalized_path AND rm.decision='delete'
                     JOIN file_locations f ON f.machine_id=gm.machine_id
                       AND f.normalized_path=gm.normalized_path AND f.active=TRUE
                     JOIN contents c ON c.content_id=f.content_id
                       AND c.md5=gm.md5 AND c.file_size=gm.file_size
                     WHERE gm.analysis_run_id=$1 AND gm.group_id=$2 AND gm.active=TRUE
                     ORDER BY gm.machine_id,gm.normalized_path",
                    &[&run_text, group_id],
                )
                .await?;
            for row in rows {
                let machine: String = row.get(0);
                let path: String = row.get(1);
                let md5: Vec<u8> = row.get(2);
                items.push(CentralDeleteItem {
                    item_id: Uuid::now_v7().to_string(),
                    group_id: group_id.clone(),
                    location: LocationKey::new(
                        MachineId::parse(machine.trim_end())?,
                        NormalizedPath::new(path)?,
                    ),
                    expected: ContentKey::new(
                        md5.try_into().map_err(|_| {
                            CentralError::InvalidState("删除项 MD5 长度不是 16".into())
                        })?,
                        u64::try_from(row.get::<_, i64>(3)).map_err(|_| {
                            CentralError::InvalidState("删除项文件大小为负数".into())
                        })?,
                    ),
                });
            }
        }
        if items.is_empty() {
            return Err(CentralError::InvalidState(
                "删除批次没有明确 Delete 成员".into(),
            ));
        }
        let batch_id = Uuid::now_v7().to_string();
        transaction
            .execute(
                "INSERT INTO delete_batches(
                   delete_batch_id,analysis_run_id,mode,status)
                 VALUES($1,$2,$3,'queued')",
                &[&batch_id, &run_text, &delete_mode_name(mode)],
            )
            .await?;
        for item in &items {
            transaction
                .execute(
                    "INSERT INTO delete_items(
                       delete_item_id,delete_batch_id,group_id,machine_id,normalized_path,
                       expected_md5,expected_size,status)
                     VALUES($1,$2,$3,$4,$5,$6,$7,'queued')",
                    &[
                        &item.item_id,
                        &batch_id,
                        &item.group_id,
                        &item.location.machine_id().as_str(),
                        &item.location.normalized_path().as_str(),
                        &item.expected.md5().as_slice(),
                        &pg_i64(item.expected.file_size(), "删除项文件大小")?,
                    ],
                )
                .await?;
        }
        transaction.commit().await?;
        Ok(CentralDeletePlan {
            batch_id,
            mode,
            items,
        })
    }

    /// 原子应用节点结果；成功时直接收缩重复组，文件位置由随后同步的 outbox 更新。
    pub async fn apply_delete_results(
        &mut self,
        batch_id: &str,
        results: &[CentralDeleteResult],
    ) -> Result<(), CentralError> {
        let transaction = self.client.transaction().await?;
        let run_id: String = transaction
            .query_opt(
                "SELECT analysis_run_id FROM delete_batches WHERE delete_batch_id=$1 FOR UPDATE",
                &[&batch_id],
            )
            .await?
            .ok_or_else(|| CentralError::InvalidState("删除批次不存在".into()))?
            .get(0);
        for result in results {
            apply_one_result(&transaction, batch_id, &run_id, result).await?;
        }
        let remaining: i64 = transaction
            .query_one(
                "SELECT COUNT(*) FROM delete_items
                 WHERE delete_batch_id=$1 AND status IN ('queued','running')",
                &[&batch_id],
            )
            .await?
            .get(0);
        transaction
            .execute(
                "UPDATE delete_batches SET status=$2 WHERE delete_batch_id=$1",
                &[
                    &batch_id,
                    &if remaining == 0 {
                        "completed"
                    } else {
                        "running"
                    },
                ],
            )
            .await?;
        transaction.commit().await?;
        Ok(())
    }
}

struct StoredDeleteItem {
    group_id: String,
    machine_id: String,
    normalized_path: String,
    status: String,
}

async fn apply_one_result(
    transaction: &Transaction<'_>,
    batch_id: &str,
    run_id: &str,
    result: &CentralDeleteResult,
) -> Result<(), CentralError> {
    let row = transaction
        .query_opt(
            "SELECT group_id,machine_id,normalized_path,status
             FROM delete_items WHERE delete_item_id=$1 AND delete_batch_id=$2 FOR UPDATE",
            &[&result.item_id, &batch_id],
        )
        .await?
        .ok_or_else(|| CentralError::InvalidState("删除项不存在".into()))?;
    let item = StoredDeleteItem {
        group_id: row.get(0),
        machine_id: row.get(1),
        normalized_path: row.get(2),
        status: row.get(3),
    };
    let target = result.outcome.as_str();
    if item.status == target {
        return Ok(());
    }
    if !matches!(item.status.as_str(), "queued" | "running") {
        return Err(CentralError::InvalidState(format!(
            "删除项 {} 已处于 {}",
            result.item_id, item.status
        )));
    }
    transaction
        .execute(
            "UPDATE delete_items SET status=$2,message=$3 WHERE delete_item_id=$1",
            &[&result.item_id, &target, &result.message],
        )
        .await?;
    if matches!(
        result.outcome,
        CentralDeleteOutcome::Recycled | CentralDeleteOutcome::Deleted
    ) {
        shrink_group(transaction, run_id, &item).await?;
    }
    Ok(())
}

async fn shrink_group(
    transaction: &Transaction<'_>,
    run_id: &str,
    item: &StoredDeleteItem,
) -> Result<(), CentralError> {
    let representative: bool = transaction
        .query_one(
            "SELECT representative FROM group_members
             WHERE analysis_run_id=$1 AND group_id=$2
               AND machine_id=$3 AND normalized_path=$4",
            &[
                &run_id,
                &item.group_id,
                &item.machine_id,
                &item.normalized_path,
            ],
        )
        .await?
        .get(0);
    transaction
        .execute(
            "DELETE FROM group_members
             WHERE analysis_run_id=$1 AND group_id=$2
               AND machine_id=$3 AND normalized_path=$4",
            &[
                &run_id,
                &item.group_id,
                &item.machine_id,
                &item.normalized_path,
            ],
        )
        .await?;
    let remaining: i64 = transaction
        .query_one(
            "SELECT COUNT(*) FROM group_members
             WHERE analysis_run_id=$1 AND group_id=$2 AND active=TRUE",
            &[&run_id, &item.group_id],
        )
        .await?
        .get(0);
    if remaining < 2 {
        transaction
            .execute(
                "DELETE FROM duplicate_groups WHERE analysis_run_id=$1 AND group_id=$2",
                &[&run_id, &item.group_id],
            )
            .await?;
    } else if representative {
        select_new_representative(transaction, run_id, &item.group_id).await?;
    }
    Ok(())
}

async fn select_new_representative(
    transaction: &Transaction<'_>,
    run_id: &str,
    group_id: &str,
) -> Result<(), CentralError> {
    let selected = transaction
        .query_one(
            "SELECT gm.machine_id,gm.normalized_path,gm.md5,gm.file_size
             FROM group_members gm
             JOIN review_marks rm ON rm.analysis_run_id=gm.analysis_run_id
               AND rm.group_id=gm.group_id AND rm.machine_id=gm.machine_id
               AND rm.normalized_path=gm.normalized_path AND rm.decision='keep'
             JOIN file_locations f ON f.machine_id=gm.machine_id
               AND f.normalized_path=gm.normalized_path AND f.active=TRUE
             WHERE gm.analysis_run_id=$1 AND gm.group_id=$2 AND gm.active=TRUE
             ORDER BY gm.machine_id,gm.normalized_path LIMIT 1",
            &[&run_id, &group_id],
        )
        .await?;
    let machine: String = selected.get(0);
    let path: String = selected.get(1);
    let md5: Vec<u8> = selected.get(2);
    let size: i64 = selected.get(3);
    transaction
        .execute(
            "UPDATE group_members SET representative=FALSE
             WHERE analysis_run_id=$1 AND group_id=$2",
            &[&run_id, &group_id],
        )
        .await?;
    transaction
        .execute(
            "UPDATE group_members SET representative=TRUE
             WHERE analysis_run_id=$1 AND group_id=$2
               AND machine_id=$3 AND normalized_path=$4",
            &[&run_id, &group_id, &machine, &path],
        )
        .await?;
    transaction
        .execute(
            "UPDATE duplicate_groups SET representative_md5=$3,representative_size=$4
             WHERE analysis_run_id=$1 AND group_id=$2",
            &[&run_id, &group_id, &md5, &size],
        )
        .await?;
    Ok(())
}

impl CentralDeleteOutcome {
    const fn as_str(self) -> &'static str {
        match self {
            Self::Recycled => "recycled",
            Self::Deleted => "deleted",
            Self::Skipped => "skipped",
            Self::Failed => "failed",
        }
    }
}

const fn delete_mode_name(mode: DeleteMode) -> &'static str {
    match mode {
        DeleteMode::RecycleBin => "recycle_bin",
        DeleteMode::Permanent => "permanent",
    }
}
