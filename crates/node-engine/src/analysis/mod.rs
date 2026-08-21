//! 本机不依赖 PostgreSQL 的精确重复和两层相似分析状态机。

mod exact;
mod grouping;
mod image;
mod phase2;
mod video;

use std::collections::BTreeMap;

use dedup_core::{AnalysisRunId, ContentKey, MediaKind, TaskId, Thresholds};
use dedup_media::ImageStage1;
use dedup_node_store::{
    AnalysisMode, AnalysisStatus, CompleteStage1, NodeStore, StoreError, TaskStatus,
};
use thiserror::Error;

use crate::runtime_tasks::{
    RuntimeProgressUnit, RuntimeStage, RuntimeStageUpdate, RuntimeTaskReporter,
};

pub use phase2::{
    Stage2BatchItem, Stage2Processor, Stage2Request, WorkerPoolStage2Processor,
    dispatch_stage2_batch,
};

use grouping::final_groups_with_runtime;
use image::image_candidates_with_runtime;
#[allow(unused_imports)]
pub(crate) use phase2::{
    Stage2BatchPlan, begin_stage2_batch, run_stage2_batch, run_stage2_batch_with_runtime,
};
use phase2::{dispatch_missing, evaluate_candidates};
use video::video_candidates_with_runtime;

/// 节点当前状态不允许开始或继续筛选。
#[derive(Debug, Error)]
pub enum AnalysisBlocked {
    /// 节点仍有 queued/running 扫描或媒体计算任务。
    #[error("节点仍在计算，必须等待全部任务结束")]
    ComputationRunning,
    /// 用户选择的任务是 failed/cancelled，必须重试或重新选择。
    #[error("所选任务 {task_id:?} 状态为 {status:?}，需要重试或重新选择")]
    SelectedTaskNeedsAttention {
        /// 未完成任务。
        task_id: TaskId,
        /// 持久化任务状态。
        status: TaskStatus,
    },
    /// 分析内部状态与已提交数据不一致。
    #[error("本地分析状态无效: {0}")]
    InvalidState(String),
    /// SQLite 边界失败。
    #[error(transparent)]
    Store(#[from] StoreError),
}

/// 一次本地分析完成或停在 partial 后的界面摘要。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct LocalAnalysisReport {
    /// 持久化分析运行。
    pub run_id: AnalysisRunId,
    /// Completed 或 Partial。
    pub status: AnalysisStatus,
    /// 精确重复组数。
    pub exact_groups: usize,
    /// 相似图片组数。
    pub image_groups: usize,
    /// 相似视频组数。
    pub video_groups: usize,
    /// 一筛数据不完整而跳过的唯一媒体内容数。
    pub skipped_incomplete: usize,
    /// 本次实际创建二筛任务项的唯一内容数。
    pub phase2_dispatched: usize,
    /// 缺失任一端完整二筛结果的候选数。
    pub unresolved_candidates: usize,
}

/// 按确认状态链执行一次纯 SQLite 本地分析。
pub struct LocalAnalysisEngine;

impl LocalAnalysisEngine {
    /// 通过开始门禁，持久化真实运行 ID 并冻结输入；媒体工作可在后台继续。
    pub fn begin(
        store: &mut NodeStore,
        selected_tasks: &[TaskId],
        thresholds: Thresholds,
        now_ms: i64,
    ) -> Result<AnalysisRunId, AnalysisBlocked> {
        ensure_start_gate(store, selected_tasks)?;
        let run_id = store.create_analysis_run(AnalysisMode::Local, thresholds, now_ms)?;
        store.freeze_analysis_inputs(run_id, selected_tasks, now_ms)?;
        Ok(run_id)
    }

    /// 冻结所选 completed 任务并完成精确、一筛、按需二筛和最终分组。
    pub async fn start<P: Stage2Processor>(
        store: &mut NodeStore,
        selected_tasks: &[TaskId],
        thresholds: Thresholds,
        processor: &mut P,
        now_ms: i64,
    ) -> Result<LocalAnalysisReport, AnalysisBlocked> {
        let run_id = Self::begin(store, selected_tasks, thresholds, now_ms)?;
        Self::run_existing(store, run_id, processor, now_ms).await
    }

    /// 从已持久化且已冻结输入的本地运行继续筛选、二筛和最终分组。
    pub async fn run_existing<P: Stage2Processor>(
        store: &mut NodeStore,
        run_id: AnalysisRunId,
        processor: &mut P,
        now_ms: i64,
    ) -> Result<LocalAnalysisReport, AnalysisBlocked> {
        Self::run_existing_internal(store, run_id, processor, None, now_ms).await
    }

    /// 从已冻结输入继续分析，并把真实 SQLite/Worker 边界发布到进程内任务详情。
    pub async fn run_existing_with_runtime<P: Stage2Processor>(
        store: &mut NodeStore,
        run_id: AnalysisRunId,
        processor: &mut P,
        reporter: &RuntimeTaskReporter,
        now_ms: i64,
    ) -> Result<LocalAnalysisReport, AnalysisBlocked> {
        Self::run_existing_internal(store, run_id, processor, Some(reporter), now_ms).await
    }

    async fn run_existing_internal<P: Stage2Processor>(
        store: &mut NodeStore,
        run_id: AnalysisRunId,
        processor: &mut P,
        reporter: Option<&RuntimeTaskReporter>,
        now_ms: i64,
    ) -> Result<LocalAnalysisReport, AnalysisBlocked> {
        initialize_runtime_stages(reporter);
        store.transition_analysis_run(run_id, AnalysisStatus::Stage1Synced, now_ms)?;
        store.transition_analysis_run(run_id, AnalysisStatus::Screening, now_ms)?;

        let thresholds = store.analysis_thresholds(run_id)?;
        let inputs = store.analysis_inputs(run_id)?;
        if let Some(reporter) = reporter {
            let _ = reporter.update_overall_nowait(0, Some(inputs.len() as u64), 0, 0);
        }
        report_stage(
            reporter,
            RuntimeStage::FreezeInputs,
            dedup_protocol::proto::RuntimeStageState::RuntimeStageCompleted,
            RuntimeProgressUnit::Files,
            inputs.len() as u64,
            Some(inputs.len() as u64),
            0,
            0,
        );
        let mut images = BTreeMap::<ContentKey, ImageStage1>::new();
        let mut videos = BTreeMap::<ContentKey, Box<[Option<ImageStage1>; 6]>>::new();
        let mut skipped_incomplete = 0;
        let mut keys = inputs.iter().map(|input| input.content).collect::<Vec<_>>();
        keys.sort();
        keys.dedup();
        report_stage(
            reporter,
            RuntimeStage::LoadFeatures,
            dedup_protocol::proto::RuntimeStageState::RuntimeStageRunning,
            RuntimeProgressUnit::Files,
            0,
            Some(keys.len() as u64),
            0,
            0,
        );
        let mut loaded = 0_u64;
        for key in keys {
            let Some(content_id) = store.content_id_by_key(key)? else {
                skipped_incomplete += 1;
                loaded += 1;
                report_load_progress(reporter, loaded, skipped_incomplete);
                continue;
            };
            match store.content_media_kind(content_id)? {
                MediaKind::Image => match store.load_complete_stage1(content_id)? {
                    Some(CompleteStage1::Image(feature)) => {
                        images.insert(key, feature);
                    }
                    _ => skipped_incomplete += 1,
                },
                MediaKind::Video => match store.load_complete_stage1(content_id)? {
                    Some(CompleteStage1::Video(feature)) => {
                        videos.insert(key, feature);
                    }
                    _ => skipped_incomplete += 1,
                },
                MediaKind::Other => {}
            }
            loaded += 1;
            report_load_progress(reporter, loaded, skipped_incomplete);
        }
        store.set_analysis_skipped_incomplete(run_id, skipped_incomplete, now_ms)?;
        report_stage(
            reporter,
            RuntimeStage::LoadFeatures,
            dedup_protocol::proto::RuntimeStageState::RuntimeStageCompleted,
            RuntimeProgressUnit::Files,
            loaded,
            Some(loaded),
            0,
            skipped_incomplete as u64,
        );
        report_stage(
            reporter,
            RuntimeStage::Stage1Candidates,
            dedup_protocol::proto::RuntimeStageState::RuntimeStageRunning,
            RuntimeProgressUnit::CandidatePairs,
            0,
            None,
            0,
            0,
        );
        let mut candidates = image_candidates_with_runtime(&images, &thresholds, reporter);
        let image_candidate_count = candidates.len() as u64;
        candidates.extend(video_candidates_with_runtime(
            &videos,
            &thresholds,
            reporter,
            image_candidate_count,
        ));
        store.replace_candidates(run_id, &candidates)?;
        report_stage(
            reporter,
            RuntimeStage::Stage1Candidates,
            dedup_protocol::proto::RuntimeStageState::RuntimeStageCompleted,
            RuntimeProgressUnit::CandidatePairs,
            candidates.len() as u64,
            Some(candidates.len() as u64),
            0,
            0,
        );
        store.transition_analysis_run(run_id, AnalysisStatus::Phase2Dispatched, now_ms)?;
        report_stage(
            reporter,
            RuntimeStage::FillStage2,
            dedup_protocol::proto::RuntimeStageState::RuntimeStageRunning,
            RuntimeProgressUnit::CandidatePairs,
            0,
            Some(candidates.len() as u64),
            0,
            0,
        );
        let dispatched =
            match dispatch_missing(store, &candidates, processor, reporter, now_ms).await {
                Ok(dispatched) => dispatched,
                Err(error) => {
                    report_stage(
                        reporter,
                        RuntimeStage::FillStage2,
                        dedup_protocol::proto::RuntimeStageState::RuntimeStageFailed,
                        RuntimeProgressUnit::CandidatePairs,
                        0,
                        Some(candidates.len() as u64),
                        1,
                        0,
                    );
                    return Err(error);
                }
            };
        finish_run(
            store,
            run_id,
            dispatched,
            skipped_incomplete,
            reporter,
            now_ms,
        )
    }

    /// 从 partial 只收集未解决候选仍缺失的 ContentKey 并再次完成同一运行。
    pub async fn retry_phase2<P: Stage2Processor>(
        store: &mut NodeStore,
        run_id: AnalysisRunId,
        processor: &mut P,
        now_ms: i64,
    ) -> Result<LocalAnalysisReport, AnalysisBlocked> {
        if store.has_active_computation_tasks()? {
            return Err(AnalysisBlocked::ComputationRunning);
        }
        let snapshot = store.analysis_run_snapshot(run_id)?;
        if snapshot.status != AnalysisStatus::Partial {
            return Err(AnalysisBlocked::InvalidState(
                "只有 partial 运行可以重试二筛".into(),
            ));
        }
        store.transition_analysis_run(run_id, AnalysisStatus::Phase2Dispatched, now_ms)?;
        let candidates = store.analysis_candidates(run_id)?;
        let dispatched = dispatch_missing(store, &candidates, processor, None, now_ms).await?;
        finish_run(
            store,
            run_id,
            dispatched,
            snapshot.skipped_incomplete as usize,
            None,
            now_ms,
        )
    }
}

fn ensure_start_gate(store: &NodeStore, selected_tasks: &[TaskId]) -> Result<(), AnalysisBlocked> {
    if store.has_active_computation_tasks()? {
        return Err(AnalysisBlocked::ComputationRunning);
    }
    for task_id in selected_tasks {
        let status = store.task_snapshot(*task_id)?.status;
        if status != TaskStatus::Completed {
            return Err(AnalysisBlocked::SelectedTaskNeedsAttention {
                task_id: *task_id,
                status,
            });
        }
    }
    Ok(())
}

fn finish_run(
    store: &mut NodeStore,
    run_id: AnalysisRunId,
    dispatched: usize,
    skipped_incomplete: usize,
    reporter: Option<&RuntimeTaskReporter>,
    now_ms: i64,
) -> Result<LocalAnalysisReport, AnalysisBlocked> {
    let thresholds = store.analysis_thresholds(run_id)?;
    let stage1_candidates = store.analysis_candidates(run_id)?;
    let (candidates, unresolved) = evaluate_candidates(store, &stage1_candidates, &thresholds)?;
    store.replace_candidates(run_id, &candidates)?;
    if unresolved > 0 {
        store.transition_analysis_run(run_id, AnalysisStatus::Partial, now_ms)?;
        report_stage(
            reporter,
            RuntimeStage::FillStage2,
            dedup_protocol::proto::RuntimeStageState::RuntimeStageFailed,
            RuntimeProgressUnit::CandidatePairs,
            candidates.len().saturating_sub(unresolved) as u64,
            Some(candidates.len() as u64),
            unresolved as u64,
            0,
        );
        let input_total = store.analysis_inputs(run_id)?.len() as u64;
        skip_analysis_tail(reporter, candidates.len() as u64, input_total);
        if let Some(reporter) = reporter {
            let _ = reporter.update_overall_nowait(
                0,
                Some(input_total),
                unresolved as u64,
                skipped_incomplete as u64,
            );
        }
        return Ok(LocalAnalysisReport {
            run_id,
            status: AnalysisStatus::Partial,
            exact_groups: 0,
            image_groups: 0,
            video_groups: 0,
            skipped_incomplete,
            phase2_dispatched: dispatched,
            unresolved_candidates: unresolved,
        });
    }

    report_stage(
        reporter,
        RuntimeStage::FillStage2,
        dedup_protocol::proto::RuntimeStageState::RuntimeStageCompleted,
        RuntimeProgressUnit::CandidatePairs,
        candidates.len() as u64,
        Some(candidates.len() as u64),
        0,
        0,
    );
    store.transition_analysis_run(run_id, AnalysisStatus::Phase2Synced, now_ms)?;
    store.transition_analysis_run(run_id, AnalysisStatus::Finalizing, now_ms)?;
    let inputs = store.analysis_inputs(run_id)?;
    report_stage(
        reporter,
        RuntimeStage::Cluster,
        dedup_protocol::proto::RuntimeStageState::RuntimeStageRunning,
        RuntimeProgressUnit::CandidatePairs,
        0,
        Some(candidates.len() as u64),
        0,
        0,
    );
    let (groups, counts) = final_groups_with_runtime(&inputs, &candidates, reporter);
    report_stage(
        reporter,
        RuntimeStage::Cluster,
        dedup_protocol::proto::RuntimeStageState::RuntimeStageCompleted,
        RuntimeProgressUnit::CandidatePairs,
        candidates.len() as u64,
        Some(candidates.len() as u64),
        0,
        0,
    );
    report_stage(
        reporter,
        RuntimeStage::SaveResults,
        dedup_protocol::proto::RuntimeStageState::RuntimeStageRunning,
        RuntimeProgressUnit::Files,
        0,
        Some(inputs.len() as u64),
        0,
        0,
    );
    store.replace_groups(run_id, &groups)?;
    store.transition_analysis_run(run_id, AnalysisStatus::Completed, now_ms)?;
    if let Some(reporter) = reporter {
        let _ = reporter.update_overall_nowait(
            inputs.len().saturating_sub(skipped_incomplete) as u64,
            Some(inputs.len() as u64),
            0,
            skipped_incomplete as u64,
        );
    }
    report_stage(
        reporter,
        RuntimeStage::SaveResults,
        dedup_protocol::proto::RuntimeStageState::RuntimeStageCompleted,
        RuntimeProgressUnit::Files,
        inputs.len() as u64,
        Some(inputs.len() as u64),
        0,
        0,
    );
    Ok(LocalAnalysisReport {
        run_id,
        status: AnalysisStatus::Completed,
        exact_groups: counts.exact,
        image_groups: counts.image,
        video_groups: counts.video,
        skipped_incomplete,
        phase2_dispatched: dispatched,
        unresolved_candidates: 0,
    })
}

fn initialize_runtime_stages(reporter: Option<&RuntimeTaskReporter>) {
    for (stage, unit) in [
        (RuntimeStage::FreezeInputs, RuntimeProgressUnit::Files),
        (RuntimeStage::LoadFeatures, RuntimeProgressUnit::Files),
        (
            RuntimeStage::Stage1Candidates,
            RuntimeProgressUnit::CandidatePairs,
        ),
        (
            RuntimeStage::FillStage2,
            RuntimeProgressUnit::CandidatePairs,
        ),
        (RuntimeStage::Cluster, RuntimeProgressUnit::CandidatePairs),
        (RuntimeStage::SaveResults, RuntimeProgressUnit::Files),
    ] {
        report_stage(
            reporter,
            stage,
            dedup_protocol::proto::RuntimeStageState::RuntimeStageWaiting,
            unit,
            0,
            None,
            0,
            0,
        );
    }
}

fn report_load_progress(reporter: Option<&RuntimeTaskReporter>, completed: u64, skipped: usize) {
    report_stage(
        reporter,
        RuntimeStage::LoadFeatures,
        dedup_protocol::proto::RuntimeStageState::RuntimeStageRunning,
        RuntimeProgressUnit::Files,
        completed,
        None,
        0,
        skipped as u64,
    );
}

fn skip_analysis_tail(
    reporter: Option<&RuntimeTaskReporter>,
    candidate_total: u64,
    input_total: u64,
) {
    for (stage, unit, total) in [
        (
            RuntimeStage::Cluster,
            RuntimeProgressUnit::CandidatePairs,
            candidate_total,
        ),
        (
            RuntimeStage::SaveResults,
            RuntimeProgressUnit::Files,
            input_total,
        ),
    ] {
        report_stage(
            reporter,
            stage,
            dedup_protocol::proto::RuntimeStageState::RuntimeStageSkipped,
            unit,
            0,
            Some(total),
            0,
            total,
        );
    }
}

#[allow(clippy::too_many_arguments)]
fn report_stage(
    reporter: Option<&RuntimeTaskReporter>,
    stage: RuntimeStage,
    state: dedup_protocol::proto::RuntimeStageState,
    unit: RuntimeProgressUnit,
    completed: u64,
    total: Option<u64>,
    failed: u64,
    skipped: u64,
) {
    if let Some(reporter) = reporter {
        let _ = reporter.update_stage_nowait(RuntimeStageUpdate {
            stage,
            state,
            unit,
            completed,
            total,
            failed,
            skipped,
        });
    }
}
