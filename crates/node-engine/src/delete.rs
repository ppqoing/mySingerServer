//! 删除批次执行器：每项重新核对活动路径、大小和 MD5，再调用固定文件系统边界。

use std::{fs, io, path::Path};

use dedup_core::DeleteMode;
use dedup_node_store::{DeleteBatchPlan, DeleteOutcome, DeleteResult, NodeStore, StoreError};
use thiserror::Error;

use crate::runtime_tasks::{
    RuntimeFailureUpdate, RuntimeProgressUnit, RuntimeStage, RuntimeStageUpdate,
    RuntimeTaskReporter, RuntimeTaskState,
};
use crate::scan::md5_file;

/// 删除批次整体提交失败；单个文件失败保存在对应 DeleteResult 中。
#[derive(Debug, Error)]
pub enum DeleteError {
    /// SQLite 计划读取或结果事务失败。
    #[error(transparent)]
    Store(#[from] StoreError),
}

/// 删除文件系统边界；生产默认调用 Windows 回收站或 `remove_file`，测试可精确注入失败。
#[doc(hidden)]
pub trait DeleteFilesystem {
    /// 删除一项已经通过大小和 MD5 重校验的路径。
    fn delete(&self, mode: DeleteMode, path: &Path) -> io::Result<DeleteOutcome>;
}

/// 生产文件系统删除边界。
#[doc(hidden)]
pub struct SystemDeleteFilesystem;

impl DeleteFilesystem for SystemDeleteFilesystem {
    fn delete(&self, mode: DeleteMode, path: &Path) -> io::Result<DeleteOutcome> {
        match mode {
            DeleteMode::RecycleBin => {
                dedup_windows::move_to_recycle_bin(path).map(|()| DeleteOutcome::Recycled)
            }
            DeleteMode::Permanent => fs::remove_file(path).map(|()| DeleteOutcome::Deleted),
        }
    }
}

/// 删除结果事务边界；测试可在不重复文件删除的前提下注入 summarize 失败。
#[doc(hidden)]
pub trait DeleteResultCommitter {
    /// 原子提交本轮所有结果。
    fn apply(
        &self,
        store: &mut NodeStore,
        plan: &DeleteBatchPlan,
        results: &[DeleteResult],
        external: bool,
    ) -> Result<(), StoreError>;
}

/// 生产 NodeStore 结果事务边界。
#[doc(hidden)]
pub struct NodeStoreDeleteResultCommitter;

impl DeleteResultCommitter for NodeStoreDeleteResultCommitter {
    fn apply(
        &self,
        store: &mut NodeStore,
        plan: &DeleteBatchPlan,
        results: &[DeleteResult],
        external: bool,
    ) -> Result<(), StoreError> {
        if external {
            store.apply_external_delete_results(plan, results)
        } else {
            store.apply_delete_results(&plan.batch_id, results)
        }
    }
}

/// 在 NodeEngine actor 内串行调用的安全删除执行器。
pub struct DeleteEngine;

impl DeleteEngine {
    /// 执行已冻结计划并一次性提交所有结果；成功项会立即从重复组移除。
    pub fn execute_batch(
        store: &mut NodeStore,
        plan: &DeleteBatchPlan,
    ) -> Result<Vec<DeleteResult>, DeleteError> {
        execute_batch_inner(
            store,
            plan,
            None,
            false,
            &SystemDeleteFilesystem,
            &NodeStoreDeleteResultCommitter,
        )
    }

    /// 执行本机已冻结计划并发布真实重校验、删除和提交阶段。
    pub async fn execute_batch_with_runtime(
        store: &mut NodeStore,
        plan: &DeleteBatchPlan,
        reporter: &RuntimeTaskReporter,
    ) -> Result<Vec<DeleteResult>, DeleteError> {
        Self::execute_batch_with_runtime_using(
            store,
            plan,
            reporter,
            &SystemDeleteFilesystem,
            &NodeStoreDeleteResultCommitter,
        )
        .await
    }

    /// 使用受控删除和结果事务边界执行本地确认批次；仅供直接行为测试。
    #[doc(hidden)]
    pub async fn execute_batch_with_runtime_using<F, C>(
        store: &mut NodeStore,
        plan: &DeleteBatchPlan,
        reporter: &RuntimeTaskReporter,
        filesystem: &F,
        committer: &C,
    ) -> Result<Vec<DeleteResult>, DeleteError>
    where
        F: DeleteFilesystem,
        C: DeleteResultCommitter,
    {
        let result = execute_batch_inner(store, plan, Some(reporter), false, filesystem, committer);
        finish_runtime(reporter, &result).await;
        result
    }

    /// 执行 PostgreSQL 已冻结并按机器派发的计划，不要求节点存在同 ID 本地分析组。
    pub fn execute_external(
        store: &mut NodeStore,
        plan: &DeleteBatchPlan,
    ) -> Result<Vec<DeleteResult>, DeleteError> {
        execute_batch_inner(
            store,
            plan,
            None,
            true,
            &SystemDeleteFilesystem,
            &NodeStoreDeleteResultCommitter,
        )
    }

    /// 执行中心已冻结并派发到本机的计划，遥测不重新查询或扩大选择集合。
    pub async fn execute_external_with_runtime(
        store: &mut NodeStore,
        plan: &DeleteBatchPlan,
        reporter: &RuntimeTaskReporter,
    ) -> Result<Vec<DeleteResult>, DeleteError> {
        let result = execute_batch_inner(
            store,
            plan,
            Some(reporter),
            true,
            &SystemDeleteFilesystem,
            &NodeStoreDeleteResultCommitter,
        );
        finish_runtime(reporter, &result).await;
        result
    }
}

async fn finish_runtime(
    reporter: &RuntimeTaskReporter,
    result: &Result<Vec<DeleteResult>, DeleteError>,
) {
    let state = match result {
        Ok(results)
            if results
                .iter()
                .all(|row| row.outcome != DeleteOutcome::Failed) =>
        {
            RuntimeTaskState::Completed
        }
        _ => RuntimeTaskState::Failed,
    };
    let _ = reporter.finish(state).await;
}

fn execute_batch_inner<F, C>(
    store: &mut NodeStore,
    plan: &DeleteBatchPlan,
    reporter: Option<&RuntimeTaskReporter>,
    external: bool,
    filesystem: &F,
    committer: &C,
) -> Result<Vec<DeleteResult>, DeleteError>
where
    F: DeleteFilesystem,
    C: DeleteResultCommitter,
{
    if let Some(reporter) = reporter {
        let _ = reporter.update_overall_nowait(0, Some(plan.items.len() as u64), 0, 0);
    }
    initialize_delete_stages(reporter, plan.items.len() as u64);
    let executed = execute_items(store, plan, reporter, filesystem);
    let results = executed
        .iter()
        .map(|item| item.result.clone())
        .collect::<Vec<_>>();
    report_delete_stage(
        reporter,
        RuntimeStage::Summarize,
        dedup_protocol::proto::RuntimeStageState::RuntimeStageRunning,
        0,
        plan.items.len() as u64,
        0,
        0,
    );
    let applied = committer.apply(store, plan, &results, external);
    match applied {
        Ok(()) => {
            report_delete_terminal(reporter, &executed);
            record_delete_failures(reporter, &executed);
            if let Some(reporter) = reporter {
                let completed = results
                    .iter()
                    .filter(|result| {
                        matches!(
                            result.outcome,
                            DeleteOutcome::Deleted | DeleteOutcome::Recycled
                        )
                    })
                    .count() as u64;
                let failed = results
                    .iter()
                    .filter(|result| result.outcome == DeleteOutcome::Failed)
                    .count() as u64;
                let skipped = results
                    .iter()
                    .filter(|result| result.outcome == DeleteOutcome::Skipped)
                    .count() as u64;
                let _ = reporter.update_overall_nowait(
                    completed,
                    Some(plan.items.len() as u64),
                    failed,
                    skipped,
                );
            }
            report_delete_stage(
                reporter,
                RuntimeStage::Summarize,
                dedup_protocol::proto::RuntimeStageState::RuntimeStageCompleted,
                plan.items.len() as u64,
                plan.items.len() as u64,
                0,
                0,
            );
            Ok(results)
        }
        Err(error) => {
            if let Some(reporter) = reporter {
                let _ = reporter.update_overall_nowait(0, Some(plan.items.len() as u64), 1, 0);
            }
            report_delete_stage(
                reporter,
                RuntimeStage::Summarize,
                dedup_protocol::proto::RuntimeStageState::RuntimeStageFailed,
                0,
                plan.items.len() as u64,
                1,
                0,
            );
            if let Some(reporter) = reporter {
                let _ = reporter.record_failure_nowait(RuntimeFailureUpdate {
                    stage: RuntimeStage::Summarize,
                    display_path: String::new(),
                    message: error.to_string(),
                });
            }
            Err(error.into())
        }
    }
}

#[derive(Clone, Copy)]
enum RevalidationOutcome {
    Passed,
    Skipped,
    Failed,
}

struct ExecutedDeleteItem {
    result: DeleteResult,
    revalidation: RevalidationOutcome,
    delete_failed: bool,
    display_path: String,
}

fn execute_items<F>(
    store: &NodeStore,
    plan: &DeleteBatchPlan,
    reporter: Option<&RuntimeTaskReporter>,
    filesystem: &F,
) -> Vec<ExecutedDeleteItem>
where
    F: DeleteFilesystem,
{
    let total = plan.items.len() as u64;
    let mut completed = 0_u64;
    let mut revalidation_failed = 0_u64;
    let mut revalidation_skipped = 0_u64;
    let mut delete_failed = 0_u64;
    let mut output = Vec::with_capacity(plan.items.len());
    for item in &plan.items {
        let executed = {
            let active = match store.active_file(&item.location) {
                Ok(Some(active)) => active,
                Ok(None) => {
                    output.push(ExecutedDeleteItem {
                        result: DeleteResult::new(
                            item.item_id.clone(),
                            DeleteOutcome::Skipped,
                            Some("文件不存在或已经失活".into()),
                        ),
                        revalidation: RevalidationOutcome::Skipped,
                        delete_failed: false,
                        display_path: item.location.normalized_path().as_str().to_owned(),
                    });
                    completed += 1;
                    revalidation_skipped += 1;
                    report_delete_progress(
                        reporter,
                        total,
                        completed,
                        revalidation_failed,
                        revalidation_skipped,
                        delete_failed,
                    );
                    continue;
                }
                Err(error) => {
                    output.push(ExecutedDeleteItem {
                        result: DeleteResult::new(
                            item.item_id.clone(),
                            DeleteOutcome::Failed,
                            Some(error.to_string()),
                        ),
                        revalidation: RevalidationOutcome::Failed,
                        delete_failed: false,
                        display_path: item.location.normalized_path().as_str().to_owned(),
                    });
                    completed += 1;
                    revalidation_failed += 1;
                    report_delete_progress(
                        reporter,
                        total,
                        completed,
                        revalidation_failed,
                        revalidation_skipped,
                        delete_failed,
                    );
                    continue;
                }
            };
            let path = active.display_path.as_path();
            let size = match fs::metadata(path) {
                Ok(metadata) => metadata.len(),
                Err(error) if error.kind() == io::ErrorKind::NotFound => {
                    output.push(ExecutedDeleteItem {
                        result: DeleteResult::new(
                            item.item_id.clone(),
                            DeleteOutcome::Skipped,
                            Some("文件不存在".into()),
                        ),
                        revalidation: RevalidationOutcome::Skipped,
                        delete_failed: false,
                        display_path: path.to_string_lossy().into_owned(),
                    });
                    completed += 1;
                    revalidation_skipped += 1;
                    report_delete_progress(
                        reporter,
                        total,
                        completed,
                        revalidation_failed,
                        revalidation_skipped,
                        delete_failed,
                    );
                    continue;
                }
                Err(error) => {
                    output.push(ExecutedDeleteItem {
                        result: failed(&item.item_id, error),
                        revalidation: RevalidationOutcome::Failed,
                        delete_failed: false,
                        display_path: path.to_string_lossy().into_owned(),
                    });
                    completed += 1;
                    revalidation_failed += 1;
                    report_delete_progress(
                        reporter,
                        total,
                        completed,
                        revalidation_failed,
                        revalidation_skipped,
                        delete_failed,
                    );
                    continue;
                }
            };
            if size != item.expected.file_size() {
                output.push(ExecutedDeleteItem {
                    result: DeleteResult::new(
                        item.item_id.clone(),
                        DeleteOutcome::Skipped,
                        Some("文件大小已变化".into()),
                    ),
                    revalidation: RevalidationOutcome::Skipped,
                    delete_failed: false,
                    display_path: path.to_string_lossy().into_owned(),
                });
                completed += 1;
                revalidation_skipped += 1;
                report_delete_progress(
                    reporter,
                    total,
                    completed,
                    revalidation_failed,
                    revalidation_skipped,
                    delete_failed,
                );
                continue;
            }
            match md5_file(path) {
                Ok(md5) if md5 == item.expected.md5() => {}
                Ok(_) => {
                    output.push(ExecutedDeleteItem {
                        result: DeleteResult::new(
                            item.item_id.clone(),
                            DeleteOutcome::Skipped,
                            Some("文件 MD5 已变化".into()),
                        ),
                        revalidation: RevalidationOutcome::Skipped,
                        delete_failed: false,
                        display_path: path.to_string_lossy().into_owned(),
                    });
                    completed += 1;
                    revalidation_skipped += 1;
                    report_delete_progress(
                        reporter,
                        total,
                        completed,
                        revalidation_failed,
                        revalidation_skipped,
                        delete_failed,
                    );
                    continue;
                }
                Err(error) => {
                    output.push(ExecutedDeleteItem {
                        result: failed(&item.item_id, error),
                        revalidation: RevalidationOutcome::Failed,
                        delete_failed: false,
                        display_path: path.to_string_lossy().into_owned(),
                    });
                    completed += 1;
                    revalidation_failed += 1;
                    report_delete_progress(
                        reporter,
                        total,
                        completed,
                        revalidation_failed,
                        revalidation_skipped,
                        delete_failed,
                    );
                    continue;
                }
            }
            let outcome = filesystem.delete(plan.mode, path);
            match outcome {
                Ok(outcome) => ExecutedDeleteItem {
                    result: DeleteResult::new(item.item_id.clone(), outcome, None),
                    revalidation: RevalidationOutcome::Passed,
                    delete_failed: false,
                    display_path: path.to_string_lossy().into_owned(),
                },
                Err(error) => ExecutedDeleteItem {
                    result: failed(&item.item_id, error),
                    revalidation: RevalidationOutcome::Passed,
                    delete_failed: true,
                    display_path: path.to_string_lossy().into_owned(),
                },
            }
        };
        completed += 1;
        delete_failed += u64::from(executed.delete_failed);
        output.push(executed);
        report_delete_progress(
            reporter,
            total,
            completed,
            revalidation_failed,
            revalidation_skipped,
            delete_failed,
        );
    }
    output
}

fn initialize_delete_stages(reporter: Option<&RuntimeTaskReporter>, total: u64) {
    for stage in [
        RuntimeStage::RevalidateSelection,
        RuntimeStage::DispatchNodes,
        RuntimeStage::DeleteItems,
        RuntimeStage::Summarize,
    ] {
        report_delete_stage(
            reporter,
            stage,
            dedup_protocol::proto::RuntimeStageState::RuntimeStageWaiting,
            0,
            total,
            0,
            0,
        );
    }
    report_delete_stage(
        reporter,
        RuntimeStage::DispatchNodes,
        dedup_protocol::proto::RuntimeStageState::RuntimeStageCompleted,
        total,
        total,
        0,
        0,
    );
    for stage in [RuntimeStage::RevalidateSelection, RuntimeStage::DeleteItems] {
        report_delete_stage(
            reporter,
            stage,
            dedup_protocol::proto::RuntimeStageState::RuntimeStageRunning,
            0,
            total,
            0,
            0,
        );
    }
}

fn report_delete_progress(
    reporter: Option<&RuntimeTaskReporter>,
    total: u64,
    completed: u64,
    revalidation_failed: u64,
    revalidation_skipped: u64,
    delete_failed: u64,
) {
    report_delete_stage(
        reporter,
        RuntimeStage::RevalidateSelection,
        dedup_protocol::proto::RuntimeStageState::RuntimeStageRunning,
        completed,
        total,
        revalidation_failed,
        revalidation_skipped,
    );
    report_delete_stage(
        reporter,
        RuntimeStage::DeleteItems,
        dedup_protocol::proto::RuntimeStageState::RuntimeStageRunning,
        completed.saturating_sub(revalidation_failed + revalidation_skipped + delete_failed),
        total,
        delete_failed,
        revalidation_skipped + revalidation_failed,
    );
}

fn report_delete_terminal(reporter: Option<&RuntimeTaskReporter>, executed: &[ExecutedDeleteItem]) {
    let total = executed.len() as u64;
    let revalidation_failed = executed
        .iter()
        .filter(|item| matches!(item.revalidation, RevalidationOutcome::Failed))
        .count() as u64;
    let revalidation_skipped = executed
        .iter()
        .filter(|item| matches!(item.revalidation, RevalidationOutcome::Skipped))
        .count() as u64;
    let delete_failed = executed.iter().filter(|item| item.delete_failed).count() as u64;
    report_delete_stage(
        reporter,
        RuntimeStage::RevalidateSelection,
        if revalidation_failed == 0 {
            dedup_protocol::proto::RuntimeStageState::RuntimeStageCompleted
        } else {
            dedup_protocol::proto::RuntimeStageState::RuntimeStageFailed
        },
        total,
        total,
        revalidation_failed,
        revalidation_skipped,
    );
    report_delete_stage(
        reporter,
        RuntimeStage::DeleteItems,
        if delete_failed == 0 {
            dedup_protocol::proto::RuntimeStageState::RuntimeStageCompleted
        } else {
            dedup_protocol::proto::RuntimeStageState::RuntimeStageFailed
        },
        total.saturating_sub(revalidation_failed + revalidation_skipped + delete_failed),
        total,
        delete_failed,
        revalidation_failed + revalidation_skipped,
    );
}

fn record_delete_failures(reporter: Option<&RuntimeTaskReporter>, executed: &[ExecutedDeleteItem]) {
    let Some(reporter) = reporter else { return };
    for item in executed
        .iter()
        .filter(|item| item.result.outcome == DeleteOutcome::Failed)
    {
        let stage = if item.delete_failed {
            RuntimeStage::DeleteItems
        } else {
            RuntimeStage::RevalidateSelection
        };
        let _ = reporter.record_failure_nowait(RuntimeFailureUpdate {
            stage,
            display_path: item.display_path.clone(),
            message: item
                .result
                .message
                .clone()
                .unwrap_or_else(|| "删除失败".into()),
        });
    }
}

fn report_delete_stage(
    reporter: Option<&RuntimeTaskReporter>,
    stage: RuntimeStage,
    state: dedup_protocol::proto::RuntimeStageState,
    completed: u64,
    total: u64,
    failed: u64,
    skipped: u64,
) {
    if let Some(reporter) = reporter {
        let _ = reporter.update_stage_nowait(RuntimeStageUpdate {
            stage,
            state,
            unit: RuntimeProgressUnit::DeleteItems,
            completed,
            total: Some(total),
            failed,
            skipped,
        });
    }
}

fn failed(item_id: &str, error: impl std::fmt::Display) -> DeleteResult {
    DeleteResult::new(
        item_id.to_owned(),
        DeleteOutcome::Failed,
        Some(error.to_string()),
    )
}
