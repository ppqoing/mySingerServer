//! 本机不依赖 PostgreSQL 的精确重复和两层相似分析状态机。

mod exact;
mod grouping;
mod image;
mod phase2;
/// 本地和跨机二筛的瞬态缓存分类计划。
pub mod stage2_planner;
mod video;

use std::{
    collections::BTreeMap,
    path::Path,
    time::{SystemTime, UNIX_EPOCH},
};

use dedup_core::{AnalysisRunId, ContentKey, MediaKind, TaskId, Thresholds};
use dedup_media::ImageStage1;
use dedup_node_store::{
    AnalysisMode, AnalysisStatus, CompleteStage1, NodeStore, PersistentStageState, StoreError,
    TaskStageWrite, TaskStatus, classify_cache_completeness,
};
use dedup_protocol::{BASE_MISSING_PROBE, BASE_MISSING_STAGE1};
use thiserror::Error;

use crate::NodeRemoteFeatureCache;
use crate::runtime_tasks::{
    RuntimeProgressUnit, RuntimeStage, RuntimeStageUpdate, RuntimeTaskReporter,
};

pub use phase2::{
    Stage2BatchItem, Stage2Processor, Stage2Request, Stage2Source, WorkerPoolStage2Processor,
    dispatch_stage2_batch,
};

use grouping::final_groups_with_runtime;
use image::image_candidates_with_runtime;
use phase2::{MissingDispatchReport, dispatch_missing, evaluate_candidates};
#[allow(unused_imports)]
pub(crate) use phase2::{
    Stage2BatchPlan, begin_stage2_batch, run_stage2_batch, run_stage2_batch_with_runtime_cache,
};
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
        initialize_analysis_stages(store, run_id)?;
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
        Self::run_existing_internal(store, run_id, processor, None, None, None, now_ms).await
    }

    /// 从已冻结输入继续分析，并把真实 SQLite/Worker 边界发布到进程内任务详情。
    pub async fn run_existing_with_runtime<P: Stage2Processor>(
        store: &mut NodeStore,
        run_id: AnalysisRunId,
        processor: &mut P,
        reporter: &RuntimeTaskReporter,
        now_ms: i64,
    ) -> Result<LocalAnalysisReport, AnalysisBlocked> {
        Self::run_existing_internal(store, run_id, processor, Some(reporter), None, None, now_ms)
            .await
    }

    /// 从冻结输入继续本地分析，并让视频二筛复用固定 MD5 联系表缓存。
    pub(crate) async fn run_existing_with_runtime_cache<P: Stage2Processor>(
        store: &mut NodeStore,
        run_id: AnalysisRunId,
        processor: &mut P,
        reporter: &RuntimeTaskReporter,
        remote: &mut NodeRemoteFeatureCache,
        contact_sheet_root: &Path,
        now_ms: i64,
    ) -> Result<LocalAnalysisReport, AnalysisBlocked> {
        Self::run_existing_internal(
            store,
            run_id,
            processor,
            Some(reporter),
            Some(remote),
            Some(contact_sheet_root),
            now_ms,
        )
        .await
    }

    async fn run_existing_internal<P: Stage2Processor>(
        store: &mut NodeStore,
        run_id: AnalysisRunId,
        processor: &mut P,
        reporter: Option<&RuntimeTaskReporter>,
        remote: Option<&mut NodeRemoteFeatureCache>,
        contact_sheet_root: Option<&Path>,
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
        let build_started = wall_clock_ms();
        save_analysis_stage(
            store,
            run_id,
            RuntimeStage::BuildCandidates,
            PersistentStageState::Running,
            0,
            None,
            0,
            0,
            Some(build_started),
            None,
        )?;
        report_stage(
            reporter,
            RuntimeStage::BuildCandidates,
            dedup_protocol::proto::RuntimeStageState::RuntimeStageRunning,
            RuntimeProgressUnit::CandidatePairs,
            0,
            None,
            0,
            0,
        );
        let mut images = BTreeMap::<ContentKey, ImageStage1>::new();
        let mut videos = BTreeMap::<ContentKey, Box<[Option<ImageStage1>; 6]>>::new();
        let mut skipped_incomplete = 0;
        let mut keys = inputs.iter().map(|input| input.content).collect::<Vec<_>>();
        keys.sort();
        keys.dedup();
        let cached_records = store.lookup_base_cache_by_keys(&keys)?;
        if cached_records.len() != keys.len() {
            return Err(AnalysisBlocked::InvalidState(
                "分析基础缓存批量返回数量不匹配".into(),
            ));
        }
        for (key, record) in keys.into_iter().zip(cached_records) {
            let Some(record) = record else {
                skipped_incomplete += 1;
                continue;
            };
            let completeness = classify_cache_completeness(&record, true);
            if completeness.base_missing_parts & (BASE_MISSING_PROBE | BASE_MISSING_STAGE1) != 0 {
                skipped_incomplete += 1;
                continue;
            }
            match (record.media_kind, record.stage1) {
                (MediaKind::Image, Some(CompleteStage1::Image(feature))) => {
                    images.insert(key, feature);
                }
                (MediaKind::Video, Some(CompleteStage1::Video(feature))) => {
                    videos.insert(key, feature);
                }
                (MediaKind::Other, None) => {}
                _ => skipped_incomplete += 1,
            }
        }
        store.set_analysis_skipped_incomplete(run_id, skipped_incomplete, now_ms)?;
        let mut candidates = image_candidates_with_runtime(&images, &thresholds, reporter);
        let image_candidate_count = candidates.len() as u64;
        candidates.extend(video_candidates_with_runtime(
            &videos,
            &thresholds,
            reporter,
            image_candidate_count,
        ));
        store.replace_candidates(run_id, &candidates)?;
        let build_finished = wall_clock_ms();
        save_analysis_stage(
            store,
            run_id,
            RuntimeStage::BuildCandidates,
            PersistentStageState::Completed,
            candidates.len() as u64,
            Some(candidates.len() as u64),
            0,
            0,
            Some(build_started),
            Some(build_finished),
        )?;
        report_stage(
            reporter,
            RuntimeStage::BuildCandidates,
            dedup_protocol::proto::RuntimeStageState::RuntimeStageCompleted,
            RuntimeProgressUnit::CandidatePairs,
            candidates.len() as u64,
            Some(candidates.len() as u64),
            0,
            0,
        );
        store.transition_analysis_run(run_id, AnalysisStatus::Phase2Dispatched, now_ms)?;
        let dispatch_started = wall_clock_ms();
        save_analysis_stage(
            store,
            run_id,
            RuntimeStage::DispatchStage2,
            PersistentStageState::Running,
            0,
            None,
            0,
            0,
            Some(dispatch_started),
            None,
        )?;
        report_stage(
            reporter,
            RuntimeStage::DispatchStage2,
            dedup_protocol::proto::RuntimeStageState::RuntimeStageRunning,
            RuntimeProgressUnit::Files,
            0,
            None,
            0,
            0,
        );
        let dispatched = match dispatch_missing(
            store,
            &candidates,
            processor,
            reporter,
            remote,
            contact_sheet_root,
            now_ms,
        )
        .await
        {
            Ok(dispatched) => dispatched,
            Err(error) => {
                let dispatch_finished = wall_clock_ms();
                save_analysis_stage(
                    store,
                    run_id,
                    RuntimeStage::DispatchStage2,
                    PersistentStageState::Failed,
                    0,
                    None,
                    1,
                    0,
                    Some(dispatch_started),
                    Some(dispatch_finished),
                )?;
                report_stage(
                    reporter,
                    RuntimeStage::DispatchStage2,
                    dedup_protocol::proto::RuntimeStageState::RuntimeStageFailed,
                    RuntimeProgressUnit::Files,
                    0,
                    None,
                    1,
                    0,
                );
                return Err(error);
            }
        };
        complete_dispatch_stage(store, run_id, reporter, dispatched, dispatch_started)?;
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
        let dispatch_started = wall_clock_ms();
        save_analysis_stage(
            store,
            run_id,
            RuntimeStage::DispatchStage2,
            PersistentStageState::Running,
            0,
            None,
            0,
            0,
            Some(dispatch_started),
            None,
        )?;
        let dispatched =
            dispatch_missing(store, &candidates, processor, None, None, None, now_ms).await?;
        complete_dispatch_stage(store, run_id, None, dispatched, dispatch_started)?;
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
    dispatched: MissingDispatchReport,
    skipped_incomplete: usize,
    reporter: Option<&RuntimeTaskReporter>,
    now_ms: i64,
) -> Result<LocalAnalysisReport, AnalysisBlocked> {
    let compare_started = wall_clock_ms();
    let thresholds = store.analysis_thresholds(run_id)?;
    let stage1_candidates = store.analysis_candidates(run_id)?;
    save_analysis_stage(
        store,
        run_id,
        RuntimeStage::FinalCompare,
        PersistentStageState::Running,
        0,
        Some(stage1_candidates.len() as u64),
        0,
        0,
        Some(compare_started),
        None,
    )?;
    report_stage(
        reporter,
        RuntimeStage::FinalCompare,
        dedup_protocol::proto::RuntimeStageState::RuntimeStageRunning,
        RuntimeProgressUnit::CandidatePairs,
        0,
        Some(stage1_candidates.len() as u64),
        0,
        0,
    );
    let (candidates, unresolved) = evaluate_candidates(store, &stage1_candidates, &thresholds)?;
    store.replace_candidates(run_id, &candidates)?;
    if unresolved > 0 {
        store.transition_analysis_run(run_id, AnalysisStatus::Partial, now_ms)?;
        let compare_finished = wall_clock_ms();
        save_analysis_stage(
            store,
            run_id,
            RuntimeStage::FinalCompare,
            PersistentStageState::Failed,
            candidates.len().saturating_sub(unresolved) as u64,
            Some(candidates.len() as u64),
            unresolved as u64,
            0,
            Some(compare_started),
            Some(compare_finished),
        )?;
        report_stage(
            reporter,
            RuntimeStage::FinalCompare,
            dedup_protocol::proto::RuntimeStageState::RuntimeStageFailed,
            RuntimeProgressUnit::CandidatePairs,
            candidates.len().saturating_sub(unresolved) as u64,
            Some(candidates.len() as u64),
            unresolved as u64,
            0,
        );
        let input_total = store.analysis_inputs(run_id)?.len() as u64;
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
            phase2_dispatched: dispatched.total as usize,
            unresolved_candidates: unresolved,
        });
    }

    store.transition_analysis_run(run_id, AnalysisStatus::Phase2Synced, now_ms)?;
    store.transition_analysis_run(run_id, AnalysisStatus::Finalizing, now_ms)?;
    let inputs = store.analysis_inputs(run_id)?;
    let (groups, counts) = final_groups_with_runtime(&inputs, &candidates, reporter);
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
    let compare_finished = wall_clock_ms();
    save_analysis_stage(
        store,
        run_id,
        RuntimeStage::FinalCompare,
        PersistentStageState::Completed,
        candidates.len() as u64,
        Some(candidates.len() as u64),
        0,
        0,
        Some(compare_started),
        Some(compare_finished),
    )?;
    report_stage(
        reporter,
        RuntimeStage::FinalCompare,
        dedup_protocol::proto::RuntimeStageState::RuntimeStageCompleted,
        RuntimeProgressUnit::CandidatePairs,
        candidates.len() as u64,
        Some(candidates.len() as u64),
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
        phase2_dispatched: dispatched.total as usize,
        unresolved_candidates: 0,
    })
}

fn initialize_runtime_stages(reporter: Option<&RuntimeTaskReporter>) {
    for (stage, unit) in [
        (
            RuntimeStage::BuildCandidates,
            RuntimeProgressUnit::CandidatePairs,
        ),
        (RuntimeStage::DispatchStage2, RuntimeProgressUnit::Files),
        (
            RuntimeStage::FinalCompare,
            RuntimeProgressUnit::CandidatePairs,
        ),
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

/// 按固定产品顺序创建单机重复文件清单的三个等待阶段。
fn initialize_analysis_stages(
    store: &mut NodeStore,
    run_id: AnalysisRunId,
) -> Result<(), AnalysisBlocked> {
    for stage in [
        RuntimeStage::BuildCandidates,
        RuntimeStage::DispatchStage2,
        RuntimeStage::FinalCompare,
    ] {
        save_analysis_stage(
            store,
            run_id,
            stage,
            PersistentStageState::Waiting,
            0,
            None,
            0,
            0,
            None,
            None,
        )?;
    }
    Ok(())
}

/// 将二次特征派发阶段的内容级终态同时写入 SQLite 和运行详情。
fn complete_dispatch_stage(
    store: &mut NodeStore,
    run_id: AnalysisRunId,
    reporter: Option<&RuntimeTaskReporter>,
    dispatched: MissingDispatchReport,
    started_at_ms: u64,
) -> Result<(), AnalysisBlocked> {
    let finished_at_ms = wall_clock_ms();
    save_analysis_stage(
        store,
        run_id,
        RuntimeStage::DispatchStage2,
        PersistentStageState::Completed,
        dispatched.completed,
        Some(dispatched.total),
        dispatched.failed,
        dispatched.skipped,
        Some(started_at_ms),
        Some(finished_at_ms),
    )?;
    report_stage(
        reporter,
        RuntimeStage::DispatchStage2,
        dedup_protocol::proto::RuntimeStageState::RuntimeStageCompleted,
        RuntimeProgressUnit::Files,
        dispatched.completed,
        Some(dispatched.total),
        dispatched.failed,
        dispatched.skipped,
    );
    Ok(())
}

/// 保存一个单机分析阶段；开始时间只在阶段实际进入运行态时提供。
#[allow(clippy::too_many_arguments)]
fn save_analysis_stage(
    store: &mut NodeStore,
    run_id: AnalysisRunId,
    stage: RuntimeStage,
    state: PersistentStageState,
    completed: u64,
    total: Option<u64>,
    failed: u64,
    skipped: u64,
    started_at_ms: Option<u64>,
    finished_at_ms: Option<u64>,
) -> Result<(), AnalysisBlocked> {
    store.save_analysis_stage(
        run_id,
        TaskStageWrite {
            stage_id: stage.id().into(),
            state,
            completed,
            total,
            failed,
            skipped,
            started_at_ms,
            finished_at_ms,
            warning_text: None,
        },
    )?;
    Ok(())
}

/// 返回阶段持久化使用的当前墙钟毫秒。
fn wall_clock_ms() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as u64
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
