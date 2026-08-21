//! 删除批次执行器：每项重新核对活动路径、大小和 MD5，再调用固定文件系统边界。

use std::{fs, io};

use dedup_core::DeleteMode;
use dedup_node_store::{DeleteBatchPlan, DeleteOutcome, DeleteResult, NodeStore, StoreError};
use thiserror::Error;

use crate::runtime_tasks::{
    RuntimeProgressUnit, RuntimeStage, RuntimeStageUpdate, RuntimeTaskReporter,
};
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
        execute_batch_inner(store, plan, None, false)
    }

    /// 执行本机已冻结计划并发布真实重校验、删除和提交阶段。
    pub fn execute_batch_with_runtime(
        store: &mut NodeStore,
        plan: &DeleteBatchPlan,
        reporter: &RuntimeTaskReporter,
    ) -> Result<Vec<DeleteResult>, DeleteError> {
        execute_batch_inner(store, plan, Some(reporter), false)
    }

    /// 执行 PostgreSQL 已冻结并按机器派发的计划，不要求节点存在同 ID 本地分析组。
    pub fn execute_external(
        store: &mut NodeStore,
        plan: &DeleteBatchPlan,
    ) -> Result<Vec<DeleteResult>, DeleteError> {
        execute_batch_inner(store, plan, None, true)
    }

    /// 执行中心已冻结并派发到本机的计划，遥测不重新查询或扩大选择集合。
    pub fn execute_external_with_runtime(
        store: &mut NodeStore,
        plan: &DeleteBatchPlan,
        reporter: &RuntimeTaskReporter,
    ) -> Result<Vec<DeleteResult>, DeleteError> {
        execute_batch_inner(store, plan, Some(reporter), true)
    }
}

fn execute_batch_inner(
    store: &mut NodeStore,
    plan: &DeleteBatchPlan,
    reporter: Option<&RuntimeTaskReporter>,
    external: bool,
) -> Result<Vec<DeleteResult>, DeleteError> {
    if let Some(reporter) = reporter {
        let _ = reporter.update_overall_nowait(0, Some(plan.items.len() as u64), 0, 0);
    }
    initialize_delete_stages(reporter, plan.items.len() as u64);
    let executed = execute_items(store, plan, reporter);
    let results = executed
        .iter()
        .map(|item| item.result.clone())
        .collect::<Vec<_>>();
    report_delete_terminal(reporter, &executed);
    report_delete_stage(
        reporter,
        RuntimeStage::Summarize,
        dedup_protocol::proto::RuntimeStageState::RuntimeStageRunning,
        0,
        plan.items.len() as u64,
        0,
        0,
    );
    let applied = if external {
        store.apply_external_delete_results(plan, &results)
    } else {
        store.apply_delete_results(&plan.batch_id, &results)
    };
    match applied {
        Ok(()) => {
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
}

fn execute_items(
    store: &NodeStore,
    plan: &DeleteBatchPlan,
    reporter: Option<&RuntimeTaskReporter>,
) -> Vec<ExecutedDeleteItem> {
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
            let outcome = match plan.mode {
                DeleteMode::RecycleBin => {
                    dedup_windows::move_to_recycle_bin(path).map(|()| DeleteOutcome::Recycled)
                }
                DeleteMode::Permanent => fs::remove_file(path).map(|()| DeleteOutcome::Deleted),
            };
            match outcome {
                Ok(outcome) => ExecutedDeleteItem {
                    result: DeleteResult::new(item.item_id.clone(), outcome, None),
                    revalidation: RevalidationOutcome::Passed,
                    delete_failed: false,
                },
                Err(error) => ExecutedDeleteItem {
                    result: failed(&item.item_id, error),
                    revalidation: RevalidationOutcome::Passed,
                    delete_failed: true,
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
        completed.saturating_sub(revalidation_failed + revalidation_skipped),
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
        total.saturating_sub(revalidation_failed + revalidation_skipped),
        total,
        delete_failed,
        revalidation_failed + revalidation_skipped,
    );
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
