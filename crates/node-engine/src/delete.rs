//! 删除批次执行器：每项重新核对活动路径、大小和 MD5，再调用固定文件系统边界。

use std::{fs, io, path::Path};

use dedup_core::DeleteMode;
use dedup_node_store::{
    DeleteBatchPlan, DeleteOutcome, DeleteResult, NodeStore, StoreError, VerifiedDeletedFile,
};
use thiserror::Error;

use crate::delete_queue::TransientDeleteQueue;
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
    /// 删除队列创建、状态 ACK 或终态清理失败。
    #[error(transparent)]
    Io(#[from] io::Error),
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

/// 在 NodeEngine actor 内串行调用的安全删除执行器。
pub struct DeleteEngine;

impl DeleteEngine {
    /// 用当前进程专用 TSV 顺序执行删除；成功项逐项提交 SQLite 当前文件事实。
    ///
    /// 队列是本次执行唯一的调度来源。文件系统删除成功后先调用
    /// `deactivate_deleted_files`，只有 SQLite 提交成功才把 TSV 行从 `P` 改为 `C`；
    /// 身份变化、文件缺失和文件系统失败改为 `F` 并继续后续行。
    pub async fn execute_transient_with_runtime_using<F>(
        store: &mut NodeStore,
        runtime_root: &Path,
        plan: &DeleteBatchPlan,
        reporter: &RuntimeTaskReporter,
        filesystem: &F,
    ) -> Result<Vec<DeleteResult>, DeleteError>
    where
        F: DeleteFilesystem,
    {
        let result = execute_transient_inner(store, runtime_root, plan, reporter, filesystem).await;
        match result {
            Ok(results) => match store.outbox_high_seq() {
                Ok(highwater) => {
                    let state = if results
                        .iter()
                        .all(|row| row.outcome != DeleteOutcome::Failed)
                    {
                        RuntimeTaskState::Completed
                    } else {
                        RuntimeTaskState::Failed
                    };
                    let _ = reporter.finish_with_outbox_high_seq(state, highwater).await;
                    Ok(results)
                }
                Err(error) => {
                    let _ = reporter.finish(RuntimeTaskState::Failed).await;
                    Err(error.into())
                }
            },
            Err(error) => {
                let _ = reporter.finish(RuntimeTaskState::Failed).await;
                Err(error)
            }
        }
    }
}

/// 顺序消费删除 TSV，并在每一个文件成功后提交一次当前文件事实。
async fn execute_transient_inner<F>(
    store: &mut NodeStore,
    runtime_root: &Path,
    plan: &DeleteBatchPlan,
    reporter: &RuntimeTaskReporter,
    filesystem: &F,
) -> Result<Vec<DeleteResult>, DeleteError>
where
    F: DeleteFilesystem,
{
    let mut queue =
        TransientDeleteQueue::create_new(runtime_root, &plan.batch_id, plan.mode, &plan.items)?;
    let total = queue.len() as u64;
    let _ = reporter.update_overall_nowait(0, Some(total), 0, 0);
    initialize_delete_stages(Some(reporter), total);
    let mut executed = Vec::with_capacity(queue.len());

    loop {
        let entry = match queue.next_pending_entry() {
            Ok(entry) => entry,
            Err(error) => return Err(cleanup_after_error(&mut queue, error.into())),
        };
        let Some(entry) = entry else { break };
        let item = entry.item;
        let one_item_plan = DeleteBatchPlan {
            batch_id: plan.batch_id.clone(),
            mode: entry.mode,
            items: vec![item.clone()],
        };
        let one = match execute_items(store, &one_item_plan, filesystem)
            .into_iter()
            .next()
        {
            Some(one) => one,
            None => {
                return Err(cleanup_after_error(
                    &mut queue,
                    io::Error::other("删除队列项没有执行结果").into(),
                ));
            }
        };
        if matches!(
            one.result.outcome,
            DeleteOutcome::Deleted | DeleteOutcome::Recycled
        ) {
            // 文件系统已成功后，SQLite 再次验证当前活动位置和内容身份。
            if let Err(error) = store.deactivate_deleted_files(
                &[VerifiedDeletedFile::new(
                    item.location.clone(),
                    item.expected,
                )],
                current_time_ms(),
            ) {
                report_uncommitted_terminal(Some(reporter), total);
                let _ = reporter.record_failure_nowait(RuntimeFailureUpdate {
                    stage: RuntimeStage::Summarize,
                    display_path: one.display_path.clone(),
                    message: error.to_string(),
                });
                return Err(cleanup_after_error(&mut queue, error.into()));
            }
            if let Err(error) = queue.ack_sqlite(&item.item_id) {
                return Err(cleanup_after_error(&mut queue, error.into()));
            }
        } else {
            // 失败或跳过只影响当前行，后续项仍按冻结顺序继续。
            if let Err(error) = queue.mark_failed(&item.item_id) {
                return Err(cleanup_after_error(&mut queue, error.into()));
            }
        }
        executed.push(one);
    }

    if let Err(error) = queue.cleanup() {
        return Err(error.into());
    }
    report_revalidation_terminal(Some(reporter), &executed);
    record_item_failures(Some(reporter), &executed, RuntimeStage::RevalidateSelection);
    report_delete_terminal(Some(reporter), &executed);
    record_item_failures(Some(reporter), &executed, RuntimeStage::DeleteItems);
    let results = executed
        .iter()
        .map(|item| item.result.clone())
        .collect::<Vec<_>>();
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
    let _ = reporter.update_overall_nowait(completed, Some(total), failed, skipped);
    report_delete_stage(
        Some(reporter),
        RuntimeStage::Summarize,
        dedup_protocol::proto::RuntimeStageState::RuntimeStageCompleted,
        total,
        total,
        0,
        0,
    );
    Ok(results)
}

/// 发生基础设施错误时立即尝试精确清理本批目录；清理失败也必须让任务失败。
fn cleanup_after_error(queue: &mut TransientDeleteQueue, primary: DeleteError) -> DeleteError {
    match queue.cleanup() {
        Ok(()) => primary,
        Err(cleanup) => DeleteError::Io(io::Error::other(format!(
            "删除执行失败: {primary}; 删除队列清理失败: {cleanup}"
        ))),
    }
}

/// 返回当前 Unix 毫秒；删除当前事实接口只把它作为审计时间输入。
fn current_time_ms() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |duration| {
            duration.as_millis().min(i64::MAX as u128) as i64
        })
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
    filesystem: &F,
) -> Vec<ExecutedDeleteItem>
where
    F: DeleteFilesystem,
{
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
                    continue;
                }
                Err(error) => {
                    output.push(ExecutedDeleteItem {
                        result: failed(&item.item_id, error),
                        revalidation: RevalidationOutcome::Failed,
                        delete_failed: false,
                        display_path: path.to_string_lossy().into_owned(),
                    });
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
                    continue;
                }
                Err(error) => {
                    output.push(ExecutedDeleteItem {
                        result: failed(&item.item_id, error),
                        revalidation: RevalidationOutcome::Failed,
                        delete_failed: false,
                        display_path: path.to_string_lossy().into_owned(),
                    });
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
        output.push(executed);
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

fn report_revalidation_terminal(
    reporter: Option<&RuntimeTaskReporter>,
    executed: &[ExecutedDeleteItem],
) {
    let total = executed.len() as u64;
    let revalidation_failed = executed
        .iter()
        .filter(|item| matches!(item.revalidation, RevalidationOutcome::Failed))
        .count() as u64;
    let revalidation_skipped = executed
        .iter()
        .filter(|item| matches!(item.revalidation, RevalidationOutcome::Skipped))
        .count() as u64;
    report_delete_stage(
        reporter,
        RuntimeStage::RevalidateSelection,
        if revalidation_failed == 0 {
            dedup_protocol::proto::RuntimeStageState::RuntimeStageCompleted
        } else {
            dedup_protocol::proto::RuntimeStageState::RuntimeStageFailed
        },
        total.saturating_sub(revalidation_failed + revalidation_skipped),
        total,
        revalidation_failed,
        revalidation_skipped,
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

fn report_uncommitted_terminal(reporter: Option<&RuntimeTaskReporter>, total: u64) {
    for stage in [RuntimeStage::RevalidateSelection, RuntimeStage::DeleteItems] {
        report_delete_stage(
            reporter,
            stage,
            dedup_protocol::proto::RuntimeStageState::RuntimeStageCompleted,
            0,
            total,
            0,
            total,
        );
    }
}

fn record_item_failures(
    reporter: Option<&RuntimeTaskReporter>,
    executed: &[ExecutedDeleteItem],
    stage: RuntimeStage,
) {
    let Some(reporter) = reporter else { return };
    for item in executed.iter().filter(|item| {
        item.result.outcome == DeleteOutcome::Failed
            && match stage {
                RuntimeStage::DeleteItems => item.delete_failed,
                RuntimeStage::RevalidateSelection => !item.delete_failed,
                _ => false,
            }
    }) {
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
