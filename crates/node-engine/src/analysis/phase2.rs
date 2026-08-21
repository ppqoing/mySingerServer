//! 联合二筛缓存复用、持久任务派发和最终判定。

use dedup_core::{
    ContentKey, DisplayPath, LocationKey, MediaKind, ScreeningOutcome, TaskId, Thresholds,
};
use dedup_media::{VideoFrameFeatures, score_video_stage2, screen_image_stage2};
use dedup_node_store::{
    CandidateStatus, CandidateWrite, CompleteStage1, CompleteStage2, ContentId, FeatureWrite,
    NewTaskItem, NodeStore, PairKind, TaskItemCompletion, VideoFrameStage2Fields,
};
use dedup_protocol::proto::{self, worker_envelope};

use crate::runtime_tasks::{
    RuntimeFailureUpdate, RuntimeProgressUnit, RuntimeStage, RuntimeStageUpdate,
    RuntimeTaskReporter, RuntimeWorkerUpdate,
};
use crate::worker::WorkerFileIdentity;
use crate::worker::{Stage2Output, WorkerEvent, WorkerPool, decode_stage2_payload};

use super::AnalysisBlocked;

/// 一个唯一内容的持久二筛任务请求。
#[derive(Clone, Debug)]
pub struct Stage2Request {
    /// 持久任务 ID。
    pub task_id: TaskId,
    /// SQLite 任务项 ID。
    pub item_id: String,
    /// 跨边界内容键。
    pub content: ContentKey,
    /// 本机内容行，仅在节点内部使用。
    pub content_id: ContentId,
    /// 当前活动位置的实际访问路径。
    pub display_path: DisplayPath,
    /// FFmpeg 一筛已经确认的媒体类型。
    pub media_kind: MediaKind,
    /// 图片为空；视频为一筛成功的固定槽位。
    pub frame_slots: Vec<u8>,
}

/// 本地分析调用的联合二筛计算边界。
#[allow(async_fn_in_trait)]
pub trait Stage2Processor {
    /// 对一个唯一 ContentKey 计算缺失的联合特征。
    async fn process(&mut self, request: Stage2Request) -> Result<Stage2Output, String>;
}

/// 串行借用 NodeEngine 所属 WorkerPool 的二筛适配器。
pub struct WorkerPoolStage2Processor<'a> {
    pool: &'a mut WorkerPool,
    runtime: Option<(RuntimeTaskReporter, dedup_core::MachineId)>,
}

impl<'a> WorkerPoolStage2Processor<'a> {
    /// 借用当前节点唯一 WorkerPool。
    pub const fn new(pool: &'a mut WorkerPool) -> Self {
        Self {
            pool,
            runtime: None,
        }
    }

    /// 冻结本机身份并把真实 Worker slot/PID/path/disk 发布给运行时详情。
    pub fn with_runtime_reporter(
        mut self,
        reporter: RuntimeTaskReporter,
        machine_id: dedup_core::MachineId,
    ) -> Self {
        self.runtime = Some((reporter, machine_id));
        self
    }
}

impl Stage2Processor for WorkerPoolStage2Processor<'_> {
    async fn process(&mut self, request: Stage2Request) -> Result<Stage2Output, String> {
        let task_id = request.task_id.as_uuid().to_string();
        let item_id = request.item_id.clone();
        let envelope = proto::WorkerEnvelope {
            payload: Some(worker_envelope::Payload::ComputeStage2(
                proto::ComputeStage2 {
                    task_id: task_id.clone(),
                    item_id: item_id.clone(),
                    display_path: request
                        .display_path
                        .as_path()
                        .to_string_lossy()
                        .into_owned(),
                    frame_slots: request
                        .frame_slots
                        .iter()
                        .map(|slot| u32::from(*slot))
                        .collect(),
                },
            )),
        };
        if let Some((_, machine_id)) = &self.runtime {
            let physical_disk_id = physical_disk_id(&request.display_path);
            let normalized_path = dedup_core::NormalizedPath::new(request.display_path.as_path())
                .map_err(|error| error.to_string())?;
            self.pool
                .dispatch_runtime(
                    envelope,
                    WorkerFileIdentity {
                        machine_id: machine_id.clone(),
                        normalized_path,
                        display_path: request.display_path.clone(),
                        file_size: request.content.file_size(),
                        stage: RuntimeStage::FillStage2.id().into(),
                        physical_disk_id,
                    },
                )
                .await
                .map_err(|error| error.to_string())?;
        } else {
            self.pool
                .dispatch(envelope)
                .await
                .map_err(|error| error.to_string())?;
        }
        let mut started_slot = None;
        loop {
            match self.pool.next_event().await {
                Some(WorkerEvent::Started {
                    task_id: event_task,
                    item_id: event_item,
                    slot,
                    process_id,
                    identity,
                }) if event_task == task_id && event_item == item_id => {
                    started_slot = Some(slot);
                    if let Some((reporter, _)) = &self.runtime {
                        let _ = reporter
                            .worker_started(RuntimeWorkerUpdate {
                                slot,
                                process_id,
                                stage: RuntimeStage::FillStage2,
                                display_path: identity
                                    .display_path
                                    .as_path()
                                    .to_string_lossy()
                                    .into_owned(),
                                physical_disk_id: identity.physical_disk_id,
                                completed_files: 0,
                                speed_per_second: 0.0,
                            })
                            .await;
                    }
                }
                Some(WorkerEvent::Completed {
                    task_id: event_task,
                    item_id: event_item,
                    response,
                }) if event_task == task_id && event_item == item_id => {
                    if let (Some(slot), Some((reporter, _))) = (started_slot, &self.runtime) {
                        let _ = reporter.worker_completed(slot).await;
                    }
                    return match response.payload {
                        Some(worker_envelope::Payload::Stage2Result(result)) => {
                            decode_stage2_payload(&result.payload)
                                .map_err(|error| error.to_string())
                        }
                        Some(worker_envelope::Payload::WorkerFailure(failure)) => {
                            Err(failure.message)
                        }
                        _ => Err("Worker 返回了非二筛响应".into()),
                    };
                }
                Some(WorkerEvent::Crashed {
                    task_id: event_task,
                    item_id: event_item,
                    message,
                    ..
                }) if event_task == task_id && event_item == item_id => {
                    return Err(message);
                }
                Some(WorkerEvent::Cancelled {
                    task_id: event_task,
                    item_id: event_item,
                }) if event_task == task_id && event_item == item_id => {
                    return Err("二筛已取消".into());
                }
                Some(WorkerEvent::InfrastructureFailure { message }) => return Err(message),
                Some(_) => return Err("WorkerPool 在串行二筛中返回了其他任务事件".into()),
                None => return Err("WorkerPool 已关闭".into()),
            }
        }
    }
}

fn physical_disk_id(path: &DisplayPath) -> String {
    dedup_windows::resolve_storage_location(path.as_path()).map_or_else(
        |_| "Unknown".into(),
        |location| {
            format!(
                "PhysicalDisk{}",
                location
                    .physical_disk_id()
                    .disk_numbers()
                    .iter()
                    .map(u32::to_string)
                    .collect::<Vec<_>>()
                    .join("+")
            )
        },
    )
}

struct MissingWork {
    content: ContentKey,
    content_id: ContentId,
    location: LocationKey,
    display_path: DisplayPath,
    media_kind: MediaKind,
    frame_slots: Vec<u8>,
}

/// 中心协调器派给一个节点的唯一内容及固定来源位置。
#[derive(Clone, Debug)]
pub struct Stage2BatchItem {
    /// 需要联合二筛的跨数据库内容键。
    pub content: ContentKey,
    /// 中心从冻结输入中选择的本机活动位置。
    pub source: LocationKey,
    /// 图片为空；视频只列出双方一筛可能使用的成功槽位。
    pub frame_slots: Vec<u8>,
}

/// 已持久化任务及其稳定二筛工作集合。
pub(crate) struct Stage2BatchPlan {
    pub(crate) task_id: TaskId,
    work: Vec<MissingWork>,
}

/// 执行一个中心二筛批次；先复用并重新发布本机缓存，只有真正缺失时才调用 Worker。
///
/// 整个批次先持久化任务和任务项，再按稳定项顺序执行。返回的任务 ID 可由管理端查询其
/// `outbox_high_seq`，并作为 phase2 的固定同步门禁。
pub async fn dispatch_stage2_batch<P: Stage2Processor>(
    store: &mut NodeStore,
    items: &[Stage2BatchItem],
    processor: &mut P,
    now_ms: i64,
) -> Result<TaskId, AnalysisBlocked> {
    let plan = begin_stage2_batch(store, items, now_ms)?;
    run_stage2_batch(store, plan, processor, now_ms).await
}

/// 校验来源并一次持久化真实二筛任务，Worker 计算可随后在后台继续。
pub(crate) fn begin_stage2_batch(
    store: &mut NodeStore,
    items: &[Stage2BatchItem],
    now_ms: i64,
) -> Result<Stage2BatchPlan, AnalysisBlocked> {
    if items.is_empty() {
        return Err(AnalysisBlocked::InvalidState("二筛批次不能为空".into()));
    }
    let mut seen = std::collections::BTreeSet::new();
    let mut work = Vec::with_capacity(items.len());
    for requested in items {
        if !seen.insert(requested.content) {
            return Err(AnalysisBlocked::InvalidState(
                "同一二筛批次不能包含重复内容".into(),
            ));
        }
        let active = store
            .active_file(&requested.source)?
            .ok_or_else(|| AnalysisBlocked::InvalidState("二筛来源位置已失效".into()))?;
        if active.content_key != requested.content {
            return Err(AnalysisBlocked::InvalidState(
                "二筛来源位置的内容已经变化".into(),
            ));
        }
        let frame_slots = requested
            .frame_slots
            .iter()
            .copied()
            .collect::<std::collections::BTreeSet<_>>()
            .into_iter()
            .collect::<Vec<_>>();
        if frame_slots.iter().any(|slot| *slot > 5) {
            return Err(AnalysisBlocked::InvalidState(
                "视频二筛槽位必须位于 0..=5".into(),
            ));
        }
        work.push(MissingWork {
            content: requested.content,
            content_id: active.content_id,
            location: requested.source.clone(),
            display_path: active.display_path,
            media_kind: active.media_kind,
            frame_slots,
        });
    }
    let task_items = work
        .iter()
        .map(|item| {
            NewTaskItem::for_content(
                item.location.clone(),
                item.display_path.clone(),
                item.content.file_size(),
                item.content_id,
                "stage2",
            )
        })
        .collect::<Vec<_>>();
    let task_id = store.create_task("analysis_stage2", &task_items, now_ms)?;
    Ok(Stage2BatchPlan { task_id, work })
}

/// 从已持久化批次继续缓存重发、Worker 计算和任务项终态提交。
pub(crate) async fn run_stage2_batch<P: Stage2Processor>(
    store: &mut NodeStore,
    plan: Stage2BatchPlan,
    processor: &mut P,
    now_ms: i64,
) -> Result<TaskId, AnalysisBlocked> {
    run_stage2_batch_internal(store, plan, processor, None, now_ms).await
}

/// 从已持久批次继续二筛，并按真实任务项终态推进运行时详情。
pub(crate) async fn run_stage2_batch_with_runtime<P: Stage2Processor>(
    store: &mut NodeStore,
    plan: Stage2BatchPlan,
    processor: &mut P,
    reporter: &RuntimeTaskReporter,
    now_ms: i64,
) -> Result<TaskId, AnalysisBlocked> {
    run_stage2_batch_internal(store, plan, processor, Some(reporter), now_ms).await
}

async fn run_stage2_batch_internal<P: Stage2Processor>(
    store: &mut NodeStore,
    plan: Stage2BatchPlan,
    processor: &mut P,
    reporter: Option<&RuntimeTaskReporter>,
    now_ms: i64,
) -> Result<TaskId, AnalysisBlocked> {
    let Stage2BatchPlan { task_id, work } = plan;
    if let Some(reporter) = reporter {
        let _ = reporter.update_overall_nowait(0, Some(work.len() as u64), 0, 0);
    }
    report_batch_stage(
        reporter,
        proto::RuntimeStageState::RuntimeStageRunning,
        0,
        work.len() as u64,
        0,
    );
    let mut completed = 0_u64;
    let mut failed = 0_u64;
    for _ in 0..work.len() {
        let item = store
            .claim_next_item(task_id, now_ms)?
            .ok_or_else(|| AnalysisBlocked::InvalidState("二筛任务项不足".into()))?;
        let item_content = item
            .content_id
            .ok_or_else(|| AnalysisBlocked::InvalidState("二筛任务项缺少内容 ID".into()))?;
        let expected = work
            .iter()
            .find(|candidate| candidate.content_id == item_content)
            .ok_or_else(|| AnalysisBlocked::InvalidState("二筛任务项不属于当前批次".into()))?;
        let mut runtime_failure = None;
        let completion = if store.republish_complete_stage2(expected.content_id)? {
            TaskItemCompletion::Succeeded {
                content_id: Some(expected.content_id),
            }
        } else {
            let request = Stage2Request {
                task_id,
                item_id: item.item_id.clone(),
                content: expected.content,
                content_id: expected.content_id,
                display_path: expected.display_path.clone(),
                media_kind: expected.media_kind,
                frame_slots: expected.frame_slots.clone(),
            };
            match processor.process(request).await {
                Ok(output) => {
                    if persist_stage2(store, expected, output)? {
                        TaskItemCompletion::Succeeded {
                            content_id: Some(expected.content_id),
                        }
                    } else {
                        failed += 1;
                        runtime_failure = Some("二筛结果不完整".into());
                        TaskItemCompletion::Failed("二筛结果不完整".into())
                    }
                }
                Err(error) => {
                    failed += 1;
                    runtime_failure = Some(error.clone());
                    TaskItemCompletion::Failed(error)
                }
            }
        };
        store.complete_item(&item.item_id, completion, now_ms)?;
        if let (Some(reporter), Some(message)) = (reporter, runtime_failure) {
            let _ = reporter.record_failure_nowait(RuntimeFailureUpdate {
                stage: RuntimeStage::FillStage2,
                display_path: expected
                    .display_path
                    .as_path()
                    .to_string_lossy()
                    .into_owned(),
                message,
            });
        }
        completed += 1;
        if let Some(reporter) = reporter {
            let _ = reporter.update_overall_nowait(
                completed.saturating_sub(failed),
                Some(work.len() as u64),
                failed,
                0,
            );
        }
        report_batch_stage(
            reporter,
            proto::RuntimeStageState::RuntimeStageRunning,
            completed,
            work.len() as u64,
            failed,
        );
    }
    report_batch_stage(
        reporter,
        if failed == 0 {
            proto::RuntimeStageState::RuntimeStageCompleted
        } else {
            proto::RuntimeStageState::RuntimeStageFailed
        },
        completed,
        work.len() as u64,
        failed,
    );
    Ok(task_id)
}

fn report_batch_stage(
    reporter: Option<&RuntimeTaskReporter>,
    state: proto::RuntimeStageState,
    completed: u64,
    total: u64,
    failed: u64,
) {
    if let Some(reporter) = reporter {
        let _ = reporter.update_stage_nowait(RuntimeStageUpdate {
            stage: RuntimeStage::FillStage2,
            state,
            unit: RuntimeProgressUnit::Files,
            completed,
            total: Some(total),
            failed,
            skipped: 0,
        });
    }
}

/// 对未解决候选收集缺失 ContentKey，一次创建完整批次后逐项等待终态。
pub(crate) async fn dispatch_missing<P: Stage2Processor>(
    store: &mut NodeStore,
    candidates: &[CandidateWrite],
    processor: &mut P,
    reporter: Option<&RuntimeTaskReporter>,
    now_ms: i64,
) -> Result<usize, AnalysisBlocked> {
    let mut keys = candidates
        .iter()
        .filter(|candidate| {
            matches!(
                candidate.status,
                CandidateStatus::Stage1Passed | CandidateStatus::Incomplete
            )
        })
        .flat_map(|candidate| [candidate.left, candidate.right])
        .collect::<Vec<_>>();
    keys.sort();
    keys.dedup();
    let mut work = Vec::new();
    for content in keys {
        let Some(content_id) = store.content_id_by_key(content)? else {
            continue;
        };
        if store.load_complete_stage2(content_id)?.is_some() {
            continue;
        }
        let Some((location, display_path)) = store.active_location_for_content(content_id)? else {
            continue;
        };
        let media_kind = store.content_media_kind(content_id)?;
        let frame_slots = match store.load_complete_stage1(content_id)? {
            Some(CompleteStage1::Image(_)) => Vec::new(),
            Some(CompleteStage1::Video(frames)) => frames
                .iter()
                .enumerate()
                .filter_map(|(slot, frame)| frame.map(|_| slot as u8))
                .collect(),
            None => continue,
        };
        work.push(MissingWork {
            content,
            content_id,
            location,
            display_path,
            media_kind,
            frame_slots,
        });
    }
    if work.is_empty() {
        return Ok(0);
    }
    let items = work
        .iter()
        .map(|item| {
            NewTaskItem::for_content(
                item.location.clone(),
                item.display_path.clone(),
                item.content.file_size(),
                item.content_id,
                "stage2",
            )
        })
        .collect::<Vec<_>>();
    let task_id = store.create_task("analysis_stage2", &items, now_ms)?;
    for _ in 0..work.len() {
        let item = store
            .claim_next_item(task_id, now_ms)?
            .ok_or_else(|| AnalysisBlocked::InvalidState("二筛任务项不足".into()))?;
        let item_content = item
            .content_id
            .ok_or_else(|| AnalysisBlocked::InvalidState("二筛任务项缺少内容 ID".into()))?;
        let expected = work
            .iter()
            .find(|candidate| candidate.content_id == item_content)
            .ok_or_else(|| AnalysisBlocked::InvalidState("二筛任务项不属于当前批次".into()))?;
        let request = Stage2Request {
            task_id,
            item_id: item.item_id.clone(),
            content: expected.content,
            content_id: expected.content_id,
            display_path: expected.display_path.clone(),
            media_kind: expected.media_kind,
            frame_slots: expected.frame_slots.clone(),
        };
        let mut runtime_failure = None;
        let completion = match processor.process(request).await {
            Ok(output) => {
                if persist_stage2(store, expected, output)? {
                    TaskItemCompletion::Succeeded {
                        content_id: Some(expected.content_id),
                    }
                } else {
                    runtime_failure = Some("二筛结果不完整".into());
                    TaskItemCompletion::Failed("二筛结果不完整".into())
                }
            }
            Err(error) => {
                runtime_failure = Some(error.clone());
                TaskItemCompletion::Failed(error)
            }
        };
        store.complete_item(&item.item_id, completion, now_ms)?;
        if let (Some(reporter), Some(message)) = (reporter, runtime_failure) {
            let _ = reporter.record_failure_nowait(RuntimeFailureUpdate {
                stage: RuntimeStage::FillStage2,
                display_path: expected
                    .display_path
                    .as_path()
                    .to_string_lossy()
                    .into_owned(),
                message,
            });
        }
    }
    Ok(work.len())
}

fn persist_stage2(
    store: &mut NodeStore,
    work: &MissingWork,
    output: Stage2Output,
) -> Result<bool, AnalysisBlocked> {
    match work.media_kind {
        MediaKind::Image => {
            let Some(feature) = output
                .frames
                .iter()
                .find(|frame| frame.slot == 0)
                .and_then(|frame| frame.feature)
            else {
                return Ok(false);
            };
            store.commit_feature_result(
                work.content_id,
                None,
                FeatureWrite::ImageStage2(feature),
            )?;
            Ok(true)
        }
        MediaKind::Video => {
            let mut complete = true;
            for slot in &work.frame_slots {
                let feature = output
                    .frames
                    .iter()
                    .find(|frame| frame.slot == *slot)
                    .and_then(|frame| frame.feature);
                let Some(feature) = feature else {
                    complete = false;
                    continue;
                };
                store.commit_feature_result(
                    work.content_id,
                    None,
                    FeatureWrite::VideoFrameStage2(VideoFrameStage2Fields {
                        slot: *slot,
                        features: feature,
                    }),
                )?;
            }
            Ok(complete)
        }
        MediaKind::Other => Ok(false),
    }
}

/// 读取已持久化联合特征，对所有候选给出 Passed/Rejected/Incomplete。
pub(crate) fn evaluate_candidates(
    store: &NodeStore,
    candidates: &[CandidateWrite],
    thresholds: &Thresholds,
) -> Result<(Vec<CandidateWrite>, usize), AnalysisBlocked> {
    let mut output = Vec::with_capacity(candidates.len());
    let mut unresolved = 0;
    for candidate in candidates {
        let left_id = store.content_id_by_key(candidate.left)?;
        let right_id = store.content_id_by_key(candidate.right)?;
        let evaluated = match (left_id, right_id) {
            (Some(left_id), Some(right_id)) => match candidate.kind {
                PairKind::Image => {
                    evaluate_image(store, *candidate, left_id, right_id, thresholds)?
                }
                PairKind::Video => {
                    evaluate_video(store, *candidate, left_id, right_id, thresholds)?
                }
            },
            _ => incomplete(*candidate),
        };
        if evaluated.status == CandidateStatus::Incomplete {
            unresolved += 1;
        }
        output.push(evaluated);
    }
    Ok((output, unresolved))
}

fn evaluate_image(
    store: &NodeStore,
    candidate: CandidateWrite,
    left_id: ContentId,
    right_id: ContentId,
    thresholds: &Thresholds,
) -> Result<CandidateWrite, AnalysisBlocked> {
    let (Some(CompleteStage2::Image(left)), Some(CompleteStage2::Image(right))) = (
        store.load_complete_stage2(left_id)?,
        store.load_complete_stage2(right_id)?,
    ) else {
        return Ok(incomplete(candidate));
    };
    let score = screen_image_stage2(&left, &right, thresholds);
    Ok(CandidateWrite {
        phash_passed_parts: Some(score.phash_passed_parts),
        stage2_score: Some(f64::from(score.sobel_score)),
        status: if score.passed {
            CandidateStatus::Passed
        } else {
            CandidateStatus::Rejected
        },
        ..candidate
    })
}

fn evaluate_video(
    store: &NodeStore,
    candidate: CandidateWrite,
    left_id: ContentId,
    right_id: ContentId,
    thresholds: &Thresholds,
) -> Result<CandidateWrite, AnalysisBlocked> {
    let (
        Some(CompleteStage1::Video(left_stage1)),
        Some(CompleteStage1::Video(right_stage1)),
        Some(CompleteStage2::Video(left_stage2)),
        Some(CompleteStage2::Video(right_stage2)),
    ) = (
        store.load_complete_stage1(left_id)?,
        store.load_complete_stage1(right_id)?,
        store.load_complete_stage2(left_id)?,
        store.load_complete_stage2(right_id)?,
    )
    else {
        return Ok(incomplete(candidate));
    };
    let left = std::array::from_fn(|slot| VideoFrameFeatures {
        stage1: left_stage1[slot],
        stage2: left_stage2[slot],
    });
    let right = std::array::from_fn(|slot| VideoFrameFeatures {
        stage1: right_stage1[slot],
        stage2: right_stage2[slot],
    });
    let score = score_video_stage2(&left, &right, thresholds);
    let status = match score.outcome {
        ScreeningOutcome::Passed => CandidateStatus::Passed,
        ScreeningOutcome::Rejected => CandidateStatus::Rejected,
        ScreeningOutcome::Incomplete => CandidateStatus::Incomplete,
    };
    Ok(CandidateWrite {
        phash_passed_parts: None,
        stage2_score: (status != CandidateStatus::Incomplete).then_some(f64::from(score.average)),
        status,
        ..candidate
    })
}

fn incomplete(candidate: CandidateWrite) -> CandidateWrite {
    CandidateWrite {
        phash_passed_parts: None,
        stage2_score: None,
        status: CandidateStatus::Incomplete,
        ..candidate
    }
}
