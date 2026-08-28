//! 联合二筛缓存复用、持久任务派发和最终判定。

use std::{
    collections::HashMap,
    path::{Path, PathBuf},
    time::{SystemTime, UNIX_EPOCH},
};

use dedup_core::{
    ContentKey, DisplayPath, LocationKey, MediaKind, ScreeningOutcome, TaskId, Thresholds,
};
use dedup_media::{VideoFrameFeatures, score_video_stage2, screen_image_stage2};
use dedup_node_store::{
    BaseCacheRecord, CandidateStatus, CandidateWrite, CompleteStage1, CompleteStage2, ContentId,
    FeatureWrite, NewTaskItem, NodeStore, PairKind, PersistentStageState, TaskItemCompletion,
    TaskStageWrite, VideoFrameStage2Fields, classify_cache_completeness,
};
use dedup_protocol::proto::{self, worker_envelope};
use dedup_protocol::{BASE_MISSING_PROBE, BASE_MISSING_STAGE1};

use crate::contact_sheet_cache::ContactSheetCacheEntry;
use crate::runtime_tasks::{
    RuntimeFailureUpdate, RuntimeProgressUnit, RuntimeStage, RuntimeStageUpdate,
    RuntimeTaskReporter, RuntimeWorkerUpdate,
};
use crate::worker::WorkerFileIdentity;
use crate::worker::{Stage2Output, WorkerEvent, WorkerPool, decode_stage2_payload};
use crate::{NodeRemoteFeatureCache, Stage2CacheLookup};

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
    /// 视频固定 MD5 联系表目标；图片及兼容旧调用为空。
    pub contact_sheet_path: Option<PathBuf>,
}

impl Stage2Request {
    /// 根据媒体类型与本地 MD5 联系表状态选择 Worker 的真实读取来源。
    pub fn source(&self) -> Stage2Source {
        match (self.media_kind, self.contact_sheet_path.as_ref()) {
            (MediaKind::Image, _) => Stage2Source::ImageFile(self.display_path.clone()),
            (MediaKind::Video, Some(target)) if ContactSheetCacheEntry::is_valid_file(target) => {
                Stage2Source::VideoContactSheet(target.clone())
            }
            (MediaKind::Video, Some(target)) => Stage2Source::VideoFallback {
                video: self.display_path.clone(),
                target: target.clone(),
            },
            _ => Stage2Source::ImageFile(self.display_path.clone()),
        }
    }
}

/// 二次特征计算实际使用的本地文件来源。
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum Stage2Source {
    /// 图片始终读取原图片。
    ImageFile(DisplayPath),
    /// 视频直接复用可用的固定 JPEG 联系表。
    VideoContactSheet(PathBuf),
    /// 联系表缺失时从原视频重建到固定目标后再计算。
    VideoFallback {
        /// 只在回退时读取的原视频。
        video: DisplayPath,
        /// 按 MD5 确定的联系表目标。
        target: PathBuf,
    },
}

/// 本地分析调用的联合二筛计算边界。
#[allow(async_fn_in_trait)]
pub trait Stage2Processor {
    /// 对一个唯一 ContentKey 计算缺失的联合特征。
    async fn process(&mut self, request: Stage2Request) -> Result<Stage2Output, String>;

    /// 批量计算二筛；默认实现保持测试处理器兼容，生产 WorkerPool 覆盖为并行派发。
    async fn process_batch(
        &mut self,
        requests: Vec<Stage2Request>,
    ) -> Vec<Result<Stage2Output, String>> {
        let mut output = Vec::with_capacity(requests.len());
        for request in requests {
            output.push(self.process(request).await);
        }
        output
    }

    /// 每个结果到达时立即回调，供单写者先落 SQLite 再继续消费下一事件。
    async fn process_batch_each<F>(
        &mut self,
        requests: Vec<Stage2Request>,
        mut completed: F,
    ) -> Result<(), String>
    where
        F: FnMut(usize, Result<Stage2Output, String>) -> Result<(), String>,
    {
        for (index, result) in self.process_batch(requests).await.into_iter().enumerate() {
            completed(index, result)?;
        }
        Ok(())
    }
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
        let mut output = None;
        self.process_batch_each(vec![request], |_, result| {
            output = Some(result);
            Ok(())
        })
        .await?;
        output.expect("单项二筛批次必须返回一个结果")
    }

    async fn process_batch_each<F>(
        &mut self,
        requests: Vec<Stage2Request>,
        mut completed: F,
    ) -> Result<(), String>
    where
        F: FnMut(usize, Result<Stage2Output, String>) -> Result<(), String>,
    {
        let mut indexes = std::collections::HashMap::with_capacity(requests.len());
        let mut finished = std::collections::HashSet::with_capacity(requests.len());
        let mut started_slots = std::collections::HashMap::new();
        for (index, request) in requests.iter().enumerate() {
            indexes.insert(request.item_id.clone(), index);
            let envelope = stage2_envelope(request);
            let dispatched = if let Some((_, machine_id)) = &self.runtime {
                let normalized_path =
                    match dedup_core::NormalizedPath::new(request.display_path.as_path()) {
                        Ok(path) => path,
                        Err(error) => {
                            completed(index, Err(error.to_string()))?;
                            finished.insert(index);
                            continue;
                        }
                    };
                self.pool
                    .dispatch_runtime(
                        envelope,
                        WorkerFileIdentity {
                            machine_id: machine_id.clone(),
                            normalized_path,
                            display_path: request.display_path.clone(),
                            file_size: request.content.file_size(),
                            stage: RuntimeStage::ComputeStage2Features.id().into(),
                            physical_disk_id: physical_disk_id(&request.display_path),
                        },
                    )
                    .await
            } else {
                self.pool.dispatch(envelope).await
            };
            if let Err(error) = dispatched {
                completed(index, Err(error.to_string()))?;
                finished.insert(index);
            }
        }
        let mut remaining = requests.len().saturating_sub(finished.len());
        while remaining > 0 {
            let Some(event) = self.pool.next_event().await else {
                complete_unresolved(
                    &mut finished,
                    requests.len(),
                    "WorkerPool 已关闭",
                    &mut completed,
                )?;
                return Ok(());
            };
            match event {
                WorkerEvent::Started {
                    item_id,
                    slot,
                    process_id,
                    identity,
                    ..
                } if indexes.contains_key(&item_id) => {
                    let cache_detail = indexes
                        .get(&item_id)
                        .map(|index| match requests[*index].source() {
                            Stage2Source::ImageFile(_) => "读取原图",
                            Stage2Source::VideoContactSheet(_) => "复用本地缩略图",
                            Stage2Source::VideoFallback { .. } => "原视频回退并重建缩略图",
                        })
                        .unwrap_or_default();
                    started_slots.insert(item_id.clone(), slot);
                    if let Some((reporter, _)) = &self.runtime {
                        let _ = reporter
                            .worker_started(RuntimeWorkerUpdate {
                                slot,
                                process_id,
                                item_id: item_id.clone(),
                                stage: RuntimeStage::ComputeStage2Features,
                                display_path: identity
                                    .display_path
                                    .as_path()
                                    .to_string_lossy()
                                    .into_owned(),
                                physical_disk_id: identity.physical_disk_id,
                                completed_files: 0,
                                speed_per_second: 0.0,
                                current_step: "计算二次特征".into(),
                                cache_detail: cache_detail.into(),
                                phase: None,
                                cpu_weight: None,
                                decoder_threads: None,
                            })
                            .await;
                    }
                }
                WorkerEvent::Completed {
                    item_id, response, ..
                } if indexes.contains_key(&item_id) => {
                    if let Some(slot) = started_slots.remove(&item_id)
                        && let Some((reporter, _)) = &self.runtime
                    {
                        let _ = reporter.worker_completed(slot).await;
                    }
                    let result = match response.payload {
                        Some(worker_envelope::Payload::Stage2Result(result)) => {
                            decode_stage2_payload(&result.payload)
                                .map_err(|error| error.to_string())
                        }
                        Some(worker_envelope::Payload::WorkerFailure(failure)) => {
                            Err(failure.message)
                        }
                        _ => Err("Worker 返回了非二筛响应".into()),
                    };
                    finish_stage2_result(
                        &indexes,
                        &mut finished,
                        &item_id,
                        result,
                        &mut remaining,
                        &mut completed,
                    )?;
                }
                WorkerEvent::Crashed {
                    item_id, message, ..
                } if indexes.contains_key(&item_id) => {
                    started_slots.remove(&item_id);
                    finish_stage2_result(
                        &indexes,
                        &mut finished,
                        &item_id,
                        Err(format!("Worker 崩溃: {message}")),
                        &mut remaining,
                        &mut completed,
                    )?;
                }
                WorkerEvent::Cancelled { item_id, .. } if indexes.contains_key(&item_id) => {
                    finish_stage2_result(
                        &indexes,
                        &mut finished,
                        &item_id,
                        Err("二筛已取消".into()),
                        &mut remaining,
                        &mut completed,
                    )?;
                }
                WorkerEvent::InfrastructureFailure { message } => {
                    complete_unresolved(&mut finished, requests.len(), &message, &mut completed)?;
                    return Ok(());
                }
                _ => {
                    complete_unresolved(
                        &mut finished,
                        requests.len(),
                        "WorkerPool 返回了其他任务事件",
                        &mut completed,
                    )?;
                    return Ok(());
                }
            }
        }
        Ok(())
    }
}

/// 把 Node 二筛请求编码为 Worker V4 消息。
fn stage2_envelope(request: &Stage2Request) -> proto::WorkerEnvelope {
    let (display_path, contact_sheet_path, generate_contact_sheet_if_missing) =
        match request.source() {
            Stage2Source::ImageFile(path) => (
                path.as_path().to_string_lossy().into_owned(),
                String::new(),
                false,
            ),
            Stage2Source::VideoContactSheet(path) => (
                request
                    .display_path
                    .as_path()
                    .to_string_lossy()
                    .into_owned(),
                path.to_string_lossy().into_owned(),
                true,
            ),
            Stage2Source::VideoFallback { video, target } => (
                video.as_path().to_string_lossy().into_owned(),
                target.to_string_lossy().into_owned(),
                true,
            ),
        };
    proto::WorkerEnvelope {
        payload: Some(worker_envelope::Payload::ComputeStage2(
            proto::ComputeStage2 {
                task_id: request.task_id.as_uuid().to_string(),
                item_id: request.item_id.clone(),
                display_path,
                frame_slots: request
                    .frame_slots
                    .iter()
                    .map(|slot| u32::from(*slot))
                    .collect(),
                contact_sheet_path,
                generate_contact_sheet_if_missing,
            },
        )),
    }
}

/// 把一个终态写回原请求序号，并只减少一次未完成计数。
fn finish_stage2_result(
    indexes: &std::collections::HashMap<String, usize>,
    finished: &mut std::collections::HashSet<usize>,
    item_id: &str,
    result: Result<Stage2Output, String>,
    remaining: &mut usize,
    completed: &mut impl FnMut(usize, Result<Stage2Output, String>) -> Result<(), String>,
) -> Result<(), String> {
    if let Some(index) = indexes.get(item_id).copied()
        && finished.insert(index)
    {
        completed(index, result)?;
        *remaining = remaining.saturating_sub(1);
    }
    Ok(())
}

/// 基础设施终止时让全部未决项进入可持久化失败终态。
fn complete_unresolved(
    finished: &mut std::collections::HashSet<usize>,
    total: usize,
    message: &str,
    completed: &mut impl FnMut(usize, Result<Stage2Output, String>) -> Result<(), String>,
) -> Result<(), String> {
    for index in 0..total {
        if finished.insert(index) {
            completed(index, Err(message.to_owned()))?;
        }
    }
    Ok(())
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

#[derive(Clone)]
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
        if active.media_kind == MediaKind::Video && frame_slots.is_empty() {
            return Err(AnalysisBlocked::InvalidState(
                "视频二筛至少需要一个成功槽位".into(),
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
    let task_id = store.create_task("stage2_compute", &task_items, now_ms)?;
    initialize_stage2_stages(store, task_id)?;
    Ok(Stage2BatchPlan { task_id, work })
}

/// 从已持久化批次继续缓存重发、Worker 计算和任务项终态提交。
pub(crate) async fn run_stage2_batch<P: Stage2Processor>(
    store: &mut NodeStore,
    plan: Stage2BatchPlan,
    processor: &mut P,
    now_ms: i64,
) -> Result<TaskId, AnalysisBlocked> {
    run_stage2_batch_internal(store, plan, processor, None, None, None, now_ms).await
}

/// 从已持久批次继续二筛，并为视频启用固定 MD5 联系表复用与重建。
pub(crate) async fn run_stage2_batch_with_runtime_cache<P: Stage2Processor>(
    store: &mut NodeStore,
    plan: Stage2BatchPlan,
    processor: &mut P,
    reporter: &RuntimeTaskReporter,
    remote: &mut NodeRemoteFeatureCache,
    contact_sheet_root: &Path,
    now_ms: i64,
) -> Result<TaskId, AnalysisBlocked> {
    run_stage2_batch_internal(
        store,
        plan,
        processor,
        Some(reporter),
        Some(remote),
        Some(contact_sheet_root),
        now_ms,
    )
    .await
}

async fn run_stage2_batch_internal<P: Stage2Processor>(
    store: &mut NodeStore,
    plan: Stage2BatchPlan,
    processor: &mut P,
    reporter: Option<&RuntimeTaskReporter>,
    remote: Option<&mut NodeRemoteFeatureCache>,
    contact_sheet_root: Option<&Path>,
    now_ms: i64,
) -> Result<TaskId, AnalysisBlocked> {
    let Stage2BatchPlan { task_id, work } = plan;
    let total = work.len() as u64;
    let lookup_started = wall_clock_ms();
    report_stage2_stage(
        reporter,
        RuntimeStage::LookupStage2Cache,
        proto::RuntimeStageState::RuntimeStageRunning,
        0,
        total,
        0,
        0,
    );
    save_stage2_stage(
        store,
        task_id,
        RuntimeStage::LookupStage2Cache,
        PersistentStageState::Running,
        0,
        Some(total),
        0,
        0,
        Some(lookup_started),
        None,
        None,
    )?;
    let cache_warning = resolve_remote_stage2(store, &work, remote).await?;
    report_stage2_stage(
        reporter,
        RuntimeStage::LookupStage2Cache,
        proto::RuntimeStageState::RuntimeStageCompleted,
        total,
        total,
        0,
        0,
    );
    save_stage2_stage(
        store,
        task_id,
        RuntimeStage::LookupStage2Cache,
        PersistentStageState::Completed,
        total,
        Some(total),
        0,
        0,
        Some(lookup_started),
        Some(wall_clock_ms()),
        cache_warning.clone(),
    )?;
    if let (Some(reporter), Some(warning)) = (reporter, cache_warning) {
        let _ = reporter.record_failure_nowait(RuntimeFailureUpdate {
            stage: RuntimeStage::LookupStage2Cache,
            display_path: String::new(),
            message: format!("警告: {warning}"),
        });
    }
    let cached_records = store
        .lookup_base_cache_by_keys(&work.iter().map(|item| item.content).collect::<Vec<_>>())?;
    if cached_records.len() != work.len() {
        return Err(AnalysisBlocked::InvalidState(
            "二筛任务基础缓存批量返回数量不匹配".into(),
        ));
    }
    let cached_by_content = cached_records
        .into_iter()
        .flatten()
        .filter_map(|record| record.content_id.map(|id| (id, record)))
        .collect::<HashMap<_, _>>();
    if let Some(reporter) = reporter {
        let _ = reporter.update_overall_nowait(0, Some(work.len() as u64), 0, 0);
    }
    let compute_started = wall_clock_ms();
    report_batch_stage(
        reporter,
        proto::RuntimeStageState::RuntimeStageRunning,
        0,
        work.len() as u64,
        0,
        0,
    );
    let mut completed = 0_u64;
    let mut failed = 0_u64;
    let mut skipped = 0_u64;
    let mut pending = Vec::new();
    let mut requests = Vec::new();
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
        let cached = cached_by_content.get(&expected.content_id);
        if cached.is_some_and(|record| {
            classify_cache_completeness(record, true).base_missing_parts
                & (BASE_MISSING_PROBE | BASE_MISSING_STAGE1)
                != 0
        }) {
            store.complete_item(&item.item_id, TaskItemCompletion::Cancelled, now_ms)?;
            completed += 1;
            skipped += 1;
            continue;
        }
        if cached.is_some_and(cached_stage2_is_complete)
            && store.republish_complete_stage2_from_cache(
                cached.expect("缓存完整度判断已确认记录存在"),
            )?
        {
            store.complete_item(
                &item.item_id,
                TaskItemCompletion::Succeeded {
                    content_id: Some(expected.content_id),
                },
                now_ms,
            )?;
            completed += 1;
            continue;
        }
        let frame_slots = missing_stage2_slots(expected, cached);
        let existing_slots = expected
            .frame_slots
            .iter()
            .copied()
            .filter(|slot| !frame_slots.contains(slot))
            .collect::<Vec<_>>();
        if !existing_slots.is_empty() {
            let Some(cached) = cached else {
                return Err(AnalysisBlocked::InvalidState(
                    "二筛已有槽位缺少本机缓存记录".into(),
                ));
            };
            if !store.republish_stage2_slots_from_cache(cached, &existing_slots)? {
                return Err(AnalysisBlocked::InvalidState(
                    "二筛已有槽位缓存不完整".into(),
                ));
            }
        }
        if expected.media_kind == MediaKind::Video && frame_slots.is_empty() {
            store.complete_item(
                &item.item_id,
                TaskItemCompletion::Succeeded {
                    content_id: Some(expected.content_id),
                },
                now_ms,
            )?;
            completed += 1;
            continue;
        }
        let request = Stage2Request {
            task_id,
            item_id: item.item_id.clone(),
            content: expected.content,
            content_id: expected.content_id,
            display_path: expected.display_path.clone(),
            media_kind: expected.media_kind,
            frame_slots,
            contact_sheet_path: contact_sheet_target(contact_sheet_root, expected),
        };
        pending.push((
            item.item_id,
            expected.clone(),
            request.contact_sheet_path.clone(),
        ));
        requests.push(request);
    }
    if let Some(reporter) = reporter {
        let _ = reporter.update_overall_nowait(completed, Some(work.len() as u64), 0, 0);
    }
    report_batch_stage(
        reporter,
        proto::RuntimeStageState::RuntimeStageRunning,
        completed,
        work.len() as u64,
        failed,
        skipped,
    );
    save_stage2_stage(
        store,
        task_id,
        RuntimeStage::ComputeStage2Features,
        PersistentStageState::Running,
        completed,
        Some(total),
        failed,
        skipped,
        Some(compute_started),
        None,
        None,
    )?;
    processor
        .process_batch_each(requests, |index, result| {
            let Some((item_id, expected, contact_sheet_path)) = pending.get(index) else {
                return Err("二筛处理器返回序号越界".into());
            };
            let mut runtime_failure = None;
            let completion = match result {
                Ok(output) => {
                    if persist_stage2(
                        store,
                        expected,
                        item_id,
                        contact_sheet_path.as_deref(),
                        output,
                    )
                    .map_err(|error| error.to_string())?
                    {
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
                    let worker_crash = error.starts_with("Worker 崩溃:");
                    if worker_crash {
                        skipped += 1;
                    } else {
                        failed += 1;
                    }
                    runtime_failure = Some(error.clone());
                    if worker_crash {
                        TaskItemCompletion::Cancelled
                    } else {
                        TaskItemCompletion::Failed(error)
                    }
                }
            };
            store
                .complete_item(item_id, completion, now_ms)
                .map_err(|error| error.to_string())?;
            if let (Some(reporter), Some(message)) = (reporter, runtime_failure) {
                let _ = reporter.record_failure_nowait(RuntimeFailureUpdate {
                    stage: RuntimeStage::ComputeStage2Features,
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
                    completed.saturating_sub(failed).saturating_sub(skipped),
                    Some(work.len() as u64),
                    failed,
                    skipped,
                );
            }
            report_batch_stage(
                reporter,
                proto::RuntimeStageState::RuntimeStageRunning,
                completed,
                work.len() as u64,
                failed,
                skipped,
            );
            Ok(())
        })
        .await
        .map_err(AnalysisBlocked::InvalidState)?;
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
        skipped,
    );
    save_stage2_stage(
        store,
        task_id,
        RuntimeStage::ComputeStage2Features,
        if failed == 0 {
            PersistentStageState::Completed
        } else {
            PersistentStageState::Failed
        },
        completed,
        Some(total),
        failed,
        skipped,
        Some(compute_started),
        Some(wall_clock_ms()),
        None,
    )?;
    Ok(task_id)
}

fn report_batch_stage(
    reporter: Option<&RuntimeTaskReporter>,
    state: proto::RuntimeStageState,
    completed: u64,
    total: u64,
    failed: u64,
    skipped: u64,
) {
    report_stage2_stage(
        reporter,
        RuntimeStage::ComputeStage2Features,
        state,
        completed,
        total,
        failed,
        skipped,
    );
}

/// 初始化一个 Node 二次特征任务的固定两阶段。
fn initialize_stage2_stages(store: &mut NodeStore, task_id: TaskId) -> Result<(), AnalysisBlocked> {
    for stage in [
        RuntimeStage::LookupStage2Cache,
        RuntimeStage::ComputeStage2Features,
    ] {
        save_stage2_stage(
            store,
            task_id,
            stage,
            PersistentStageState::Waiting,
            0,
            None,
            0,
            0,
            None,
            None,
            None,
        )?;
    }
    Ok(())
}

/// 把二次任务阶段同步到运行详情。
#[allow(clippy::too_many_arguments)]
fn report_stage2_stage(
    reporter: Option<&RuntimeTaskReporter>,
    stage: RuntimeStage,
    state: proto::RuntimeStageState,
    completed: u64,
    total: u64,
    failed: u64,
    skipped: u64,
) {
    if let Some(reporter) = reporter {
        let _ = reporter.update_stage_nowait(RuntimeStageUpdate {
            stage,
            state,
            unit: RuntimeProgressUnit::Files,
            completed,
            total: Some(total),
            failed,
            skipped,
        });
    }
}

/// 把二次任务阶段同步到 Node SQLite，供进程重启恢复。
#[allow(clippy::too_many_arguments)]
fn save_stage2_stage(
    store: &mut NodeStore,
    task_id: TaskId,
    stage: RuntimeStage,
    state: PersistentStageState,
    completed: u64,
    total: Option<u64>,
    failed: u64,
    skipped: u64,
    started_at_ms: Option<u64>,
    finished_at_ms: Option<u64>,
    warning_text: Option<String>,
) -> Result<(), AnalysisBlocked> {
    store.save_task_stage(
        task_id,
        TaskStageWrite {
            stage_id: stage.id().into(),
            state,
            completed,
            total,
            failed,
            skipped,
            started_at_ms,
            finished_at_ms,
            warning_text,
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

/// 对未解决候选收集缺失 ContentKey，一次创建完整批次后逐项等待终态。
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub(crate) struct MissingDispatchReport {
    /// 本阶段涉及的唯一内容数。
    pub(crate) total: u64,
    /// 已复用或成功计算二次特征的内容数。
    pub(crate) completed: u64,
    /// 正常计算失败的内容数。
    pub(crate) failed: u64,
    /// Worker 崩溃后跳过的内容数。
    pub(crate) skipped: u64,
}

/// 查询候选所需的二次特征，复用缓存并持续填充 Worker，返回内容级终态计数。
pub(crate) async fn dispatch_missing<P: Stage2Processor>(
    store: &mut NodeStore,
    candidates: &[CandidateWrite],
    processor: &mut P,
    reporter: Option<&RuntimeTaskReporter>,
    remote: Option<&mut NodeRemoteFeatureCache>,
    contact_sheet_root: Option<&Path>,
    now_ms: i64,
) -> Result<MissingDispatchReport, AnalysisBlocked> {
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
    let cached_records = store.lookup_base_cache_by_keys(&keys)?;
    if cached_records.len() != keys.len() {
        return Err(AnalysisBlocked::InvalidState(
            "二筛基础缓存批量返回数量不匹配".into(),
        ));
    }
    let mut work = Vec::new();
    for (content, record) in keys.into_iter().zip(cached_records) {
        let Some(record) = record else {
            continue;
        };
        let completeness = classify_cache_completeness(&record, true);
        if completeness.base_missing_parts & (BASE_MISSING_PROBE | BASE_MISSING_STAGE1) != 0 {
            continue;
        }
        let Some(content_id) = record.content_id else {
            continue;
        };
        let Some((location, display_path)) = store.active_location_for_content(content_id)? else {
            continue;
        };
        let (media_kind, frame_slots) = match record.media_kind {
            MediaKind::Image if completeness.image_stage2_missing => (MediaKind::Image, Vec::new()),
            MediaKind::Image => continue,
            MediaKind::Video if completeness.video_stage2_missing_slots != 0 => {
                let Some(CompleteStage1::Video(frames)) = record.stage1.as_ref() else {
                    continue;
                };
                let frame_slots = frames
                    .iter()
                    .enumerate()
                    .filter_map(|(slot, frame)| {
                        (completeness.video_stage2_missing_slots & (1_u8 << slot) != 0)
                            .then_some(frame.map(|_| slot as u8))
                            .flatten()
                    })
                    .collect();
                (MediaKind::Video, frame_slots)
            }
            MediaKind::Video => continue,
            MediaKind::Other => continue,
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
        return Ok(MissingDispatchReport::default());
    }
    let _ = resolve_remote_stage2(store, &work, remote).await?;
    let refreshed_records = store
        .lookup_base_cache_by_keys(&work.iter().map(|item| item.content).collect::<Vec<_>>())?;
    if refreshed_records.len() != work.len() {
        return Err(AnalysisBlocked::InvalidState(
            "二筛远端导入后的基础缓存批量返回数量不匹配".into(),
        ));
    }
    let cached_by_content = refreshed_records
        .into_iter()
        .flatten()
        .filter_map(|record| record.content_id.map(|id| (id, record)))
        .collect::<HashMap<_, _>>();
    let total = work.len() as u64;
    let mut completed = 0_u64;
    let mut failed = 0_u64;
    let mut skipped = 0_u64;
    if let Some(reporter) = reporter {
        let _ = reporter.update_stage_nowait(RuntimeStageUpdate {
            stage: RuntimeStage::DispatchStage2,
            state: proto::RuntimeStageState::RuntimeStageRunning,
            unit: RuntimeProgressUnit::Files,
            completed,
            total: Some(total),
            failed,
            skipped,
        });
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
    let task_id = store.create_task("stage2_compute", &items, now_ms)?;
    initialize_stage2_stages(store, task_id)?;
    let mut pending = Vec::with_capacity(work.len());
    let mut requests = Vec::with_capacity(work.len());
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
        if cached_by_content
            .get(&expected.content_id)
            .is_some_and(cached_stage2_is_complete)
            && store.republish_complete_stage2_from_cache(
                cached_by_content
                    .get(&expected.content_id)
                    .expect("缓存完整度判断已确认记录存在"),
            )?
        {
            store.complete_item(
                &item.item_id,
                TaskItemCompletion::Succeeded {
                    content_id: Some(expected.content_id),
                },
                now_ms,
            )?;
            completed += 1;
            if let Some(reporter) = reporter {
                let _ = reporter.update_stage_nowait(RuntimeStageUpdate {
                    stage: RuntimeStage::DispatchStage2,
                    state: proto::RuntimeStageState::RuntimeStageRunning,
                    unit: RuntimeProgressUnit::Files,
                    completed,
                    total: Some(total),
                    failed,
                    skipped,
                });
            }
            continue;
        }
        let request = Stage2Request {
            task_id,
            item_id: item.item_id.clone(),
            content: expected.content,
            content_id: expected.content_id,
            display_path: expected.display_path.clone(),
            media_kind: expected.media_kind,
            frame_slots: expected.frame_slots.clone(),
            contact_sheet_path: contact_sheet_target(contact_sheet_root, expected),
        };
        pending.push((
            item.item_id,
            expected.clone(),
            request.contact_sheet_path.clone(),
        ));
        requests.push(request);
    }
    processor
        .process_batch_each(requests, |index, result| {
            let Some((item_id, expected, contact_sheet_path)) = pending.get(index) else {
                return Err("二筛处理器返回序号越界".into());
            };
            let mut runtime_failure = None;
            let (completion, item_failed, item_skipped) = match result {
                Ok(output) => {
                    if persist_stage2(
                        store,
                        expected,
                        item_id,
                        contact_sheet_path.as_deref(),
                        output,
                    )
                    .map_err(|error| error.to_string())?
                    {
                        (
                            TaskItemCompletion::Succeeded {
                                content_id: Some(expected.content_id),
                            },
                            false,
                            false,
                        )
                    } else {
                        runtime_failure = Some("二筛结果不完整".into());
                        (
                            TaskItemCompletion::Failed("二筛结果不完整".into()),
                            true,
                            false,
                        )
                    }
                }
                Err(error) => {
                    runtime_failure = Some(error.clone());
                    if error.starts_with("Worker 崩溃:") {
                        (TaskItemCompletion::Cancelled, false, true)
                    } else {
                        (TaskItemCompletion::Failed(error), true, false)
                    }
                }
            };
            store
                .complete_item(item_id, completion, now_ms)
                .map_err(|error| error.to_string())?;
            if item_failed {
                failed += 1;
            } else if item_skipped {
                skipped += 1;
            } else {
                completed += 1;
            }
            if let Some(reporter) = reporter {
                let _ = reporter.update_stage_nowait(RuntimeStageUpdate {
                    stage: RuntimeStage::DispatchStage2,
                    state: proto::RuntimeStageState::RuntimeStageRunning,
                    unit: RuntimeProgressUnit::Files,
                    completed,
                    total: Some(total),
                    failed,
                    skipped,
                });
            }
            if let (Some(reporter), Some(message)) = (reporter, runtime_failure) {
                let _ = reporter.record_failure_nowait(RuntimeFailureUpdate {
                    stage: RuntimeStage::DispatchStage2,
                    display_path: expected
                        .display_path
                        .as_path()
                        .to_string_lossy()
                        .into_owned(),
                    message,
                });
            }
            Ok(())
        })
        .await
        .map_err(AnalysisBlocked::InvalidState)?;
    Ok(MissingDispatchReport {
        total,
        completed,
        failed,
        skipped,
    })
}

/// 判断批量缓存记录是否已经具备可直接重发的二次特征。
fn cached_stage2_is_complete(record: &dedup_node_store::BaseCacheRecord) -> bool {
    let completeness = classify_cache_completeness(record, true);
    completeness.base_missing_parts & (BASE_MISSING_PROBE | BASE_MISSING_STAGE1) == 0
        && !completeness.image_stage2_missing
        && completeness.video_stage2_missing_slots == 0
}

/// 将中心要求的成功槽位与本机结构分类得到的二筛缺失掩码求交集。
fn missing_stage2_slots(expected: &MissingWork, cached: Option<&BaseCacheRecord>) -> Vec<u8> {
    if expected.media_kind != MediaKind::Video {
        return Vec::new();
    }
    let missing_mask = cached
        .filter(|record| {
            classify_cache_completeness(record, true).base_missing_parts
                & (BASE_MISSING_PROBE | BASE_MISSING_STAGE1)
                == 0
        })
        .map(|record| classify_cache_completeness(record, true).video_stage2_missing_slots)
        .unwrap_or_else(|| {
            expected
                .frame_slots
                .iter()
                .fold(0_u8, |mask, slot| mask | (1_u8 << slot))
        });
    expected
        .frame_slots
        .iter()
        .copied()
        .filter(|slot| missing_mask & (1_u8 << slot) != 0)
        .collect()
}

/// 批量查询 PostgreSQL 二次缓存并导入 SQLite；失败只降级到本地计算。
async fn resolve_remote_stage2(
    store: &mut NodeStore,
    work: &[MissingWork],
    remote: Option<&mut NodeRemoteFeatureCache>,
) -> Result<Option<String>, AnalysisBlocked> {
    let Some(remote) = remote else {
        return Ok(None);
    };
    let startup_warning = remote.startup_warning().map(str::to_owned);
    if work.is_empty() {
        return Ok(startup_warning);
    }
    let requests = work
        .iter()
        .map(|item| Stage2CacheLookup {
            content: item.content,
            media_kind: item.media_kind,
            frame_slots: item.frame_slots.clone(),
        })
        .collect::<Vec<_>>();
    let hits = match remote.lookup_stage2(&requests).await {
        Ok(hits) if hits.len() == requests.len() => hits,
        Ok(_) => {
            tracing::warn!("PostgreSQL 二筛缓存返回数量不匹配，本次继续本地计算");
            return Ok(Some(
                "PostgreSQL 二筛缓存返回数量不匹配，本次继续 SQLite-only".into(),
            ));
        }
        Err(error) => {
            tracing::warn!(error = %error, "PostgreSQL 二筛缓存查询失败，本次继续本地计算");
            return Ok(Some(format!("PostgreSQL 二筛缓存降级: {error}")));
        }
    };
    for (item, hit) in work.iter().zip(hits) {
        let Some(hit) = hit else { continue };
        persist_cached_stage2(store, item.content_id, hit)?;
    }
    Ok(startup_warning)
}

/// 把一份完整中心二次缓存写入 SQLite 与 outbox。
fn persist_cached_stage2(
    store: &mut NodeStore,
    content_id: ContentId,
    cached: CompleteStage2,
) -> Result<(), AnalysisBlocked> {
    match cached {
        CompleteStage2::Image(feature) => {
            store.commit_feature_result(content_id, None, FeatureWrite::ImageStage2(*feature))?;
        }
        CompleteStage2::Video(frames) => {
            for (slot, feature) in frames.iter().enumerate() {
                if let Some(feature) = feature {
                    store.commit_feature_result(
                        content_id,
                        None,
                        FeatureWrite::VideoFrameStage2(VideoFrameStage2Fields {
                            slot: slot as u8,
                            features: *feature,
                        }),
                    )?;
                }
            }
        }
    }
    Ok(())
}

fn persist_stage2(
    store: &mut NodeStore,
    work: &MissingWork,
    item_id: &str,
    contact_sheet_path: Option<&Path>,
    output: Stage2Output,
) -> Result<bool, AnalysisBlocked> {
    if work.media_kind == MediaKind::Video
        && let Some(contact_sheet_path) = contact_sheet_path
    {
        let target = ContactSheetCacheEntry::from_md5(
            contact_sheet_path
                .parent()
                .and_then(Path::parent)
                .ok_or_else(|| AnalysisBlocked::InvalidState("联系表路径缺少缓存根".into()))?,
            work.content.md5(),
        );
        if target.final_path() != contact_sheet_path {
            return Err(AnalysisBlocked::InvalidState(
                "联系表 MD5 路径不一致".into(),
            ));
        }
        if let Some(jpeg) = output.regenerated_contact_sheet_jpeg.as_deref() {
            target
                .publish_rebuilt(item_id, jpeg, store, work.content_id)
                .map_err(|error| AnalysisBlocked::InvalidState(error.to_string()))?;
        } else if ContactSheetCacheEntry::is_valid_file(target.final_path()) {
            target
                .repair_reference(store, work.content_id)
                .map_err(|error| AnalysisBlocked::InvalidState(error.to_string()))?;
        }
    }
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

/// 仅视频且调用方提供缓存根时返回固定 MD5 联系表绝对路径。
fn contact_sheet_target(root: Option<&Path>, work: &MissingWork) -> Option<PathBuf> {
    (work.media_kind == MediaKind::Video)
        .then(|| root.map(|root| ContactSheetCacheEntry::from_md5(root, work.content.md5())))
        .flatten()
        .map(|entry| entry.final_path().to_path_buf())
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
