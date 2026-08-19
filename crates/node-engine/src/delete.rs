//! 删除批次执行器：每项重新核对活动路径、大小和 MD5，再调用固定文件系统边界。

use std::{fs, io};

use dedup_core::DeleteMode;
use dedup_node_store::{DeleteBatchPlan, DeleteOutcome, DeleteResult, NodeStore, StoreError};
use thiserror::Error;

use crate::scan::md5_file;

/// 删除批次整体提交失败；单个文件失败保存在对应 DeleteResult 中。
#[derive(Debug, Error)]
pub enum DeleteError {
    /// SQLite 计划读取或结果事务失败。
    #[error(transparent)]
    Store(#[from] StoreError),
}

/// 在 NodeEngine actor 内串行调用的安全删除执行器。
pub struct DeleteEngine;

impl DeleteEngine {
    /// 执行已冻结计划并一次性提交所有结果；成功项会立即从重复组移除。
    pub fn execute_batch(
        store: &mut NodeStore,
        plan: &DeleteBatchPlan,
    ) -> Result<Vec<DeleteResult>, DeleteError> {
        let results = execute_items(store, plan);
        store.apply_delete_results(&plan.batch_id, &results)?;
        Ok(results)
    }

    /// 执行 PostgreSQL 已冻结并按机器派发的计划，不要求节点存在同 ID 本地分析组。
    pub fn execute_external(
        store: &mut NodeStore,
        plan: &DeleteBatchPlan,
    ) -> Result<Vec<DeleteResult>, DeleteError> {
        let results = execute_items(store, plan);
        store.apply_external_delete_results(plan, &results)?;
        Ok(results)
    }
}

fn execute_items(store: &NodeStore, plan: &DeleteBatchPlan) -> Vec<DeleteResult> {
    plan.items
        .iter()
        .map(|item| {
            let active = match store.active_file(&item.location) {
                Ok(Some(active)) => active,
                Ok(None) => {
                    return DeleteResult::new(
                        item.item_id.clone(),
                        DeleteOutcome::Skipped,
                        Some("文件不存在或已经失活".into()),
                    );
                }
                Err(error) => {
                    return DeleteResult::new(
                        item.item_id.clone(),
                        DeleteOutcome::Failed,
                        Some(error.to_string()),
                    );
                }
            };
            let path = active.display_path.as_path();
            let size = match fs::metadata(path) {
                Ok(metadata) => metadata.len(),
                Err(error) if error.kind() == io::ErrorKind::NotFound => {
                    return DeleteResult::new(
                        item.item_id.clone(),
                        DeleteOutcome::Skipped,
                        Some("文件不存在".into()),
                    );
                }
                Err(error) => return failed(&item.item_id, error),
            };
            if size != item.expected.file_size() {
                return DeleteResult::new(
                    item.item_id.clone(),
                    DeleteOutcome::Skipped,
                    Some("文件大小已变化".into()),
                );
            }
            match md5_file(path) {
                Ok(md5) if md5 == item.expected.md5() => {}
                Ok(_) => {
                    return DeleteResult::new(
                        item.item_id.clone(),
                        DeleteOutcome::Skipped,
                        Some("文件 MD5 已变化".into()),
                    );
                }
                Err(error) => return failed(&item.item_id, error),
            }
            let outcome = match plan.mode {
                DeleteMode::RecycleBin => {
                    dedup_windows::move_to_recycle_bin(path).map(|()| DeleteOutcome::Recycled)
                }
                DeleteMode::Permanent => fs::remove_file(path).map(|()| DeleteOutcome::Deleted),
            };
            match outcome {
                Ok(outcome) => DeleteResult::new(item.item_id.clone(), outcome, None),
                Err(error) => failed(&item.item_id, error),
            }
        })
        .collect()
}

fn failed(item_id: &str, error: impl std::fmt::Display) -> DeleteResult {
    DeleteResult::new(
        item_id.to_owned(),
        DeleteOutcome::Failed,
        Some(error.to_string()),
    )
}
