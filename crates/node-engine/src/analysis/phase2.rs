//! 联合二筛缓存复用、瞬态批次派发和最终判定。

use std::{
    collections::HashMap,
    fmt,
    path::{Path, PathBuf},
    sync::Arc,
};

#[cfg(all(test, feature = "test-hooks"))]
use std::sync::Mutex;

use dedup_core::{
    ContentKey, DiskReadConfig, DisplayPath, LocationKey, MediaKind, ScreeningOutcome, TaskId,
    Thresholds,
};
use dedup_media::{VideoFrameFeatures, score_video_stage2, screen_image_stage2};
use dedup_node_store::{
    BaseCacheRecord, CandidateStatus, CandidateWrite, CompleteStage1, CompleteStage2, ContentId,
    FeatureWrite, NewTaskItem, NodeStore, PairKind, PersistentStageState, ScannedPath,
    TaskItemCompletion, TaskStageWrite, VideoFrameStage2Fields, classify_cache_completeness,
};
use dedup_protocol::proto::{self, worker_envelope};
use dedup_protocol::{BASE_MISSING_PROBE, BASE_MISSING_STAGE1};
use dedup_windows::ReadCancellationToken;
use uuid::Uuid;

use crate::contact_sheet_cache::ContactSheetCacheEntry;
use crate::runtime_tasks::{
    RuntimeFailureUpdate, RuntimeProgressUnit, RuntimeStage, RuntimeStageUpdate,
    RuntimeTaskReporter, RuntimeWorkerUpdate,
};
use crate::worker::WorkerFileIdentity;
use crate::worker::{Stage2Output, WorkerEvent, WorkerPool, decode_stage2_payload};
use crate::{
    NodeRemoteFeatureCache, Stage2CacheLookup,
    scan::{
        BaseStoreActor, ScanDiskPlan, ScanRootStorageResolver, ScheduledFileReader,
        Stage2TaskInput, build_stage2_task_production, run_task_file_stage2,
    },
};

#[cfg(all(test, feature = "test-hooks"))]
use crate::scan::BasePersistTestWaiter;

use super::AnalysisBlocked;
use super::stage2_planner::{
    FrozenStage2Batch, Stage2ActiveSource, Stage2PlanAction, Stage2PlanningInput, Stage2Selection,
    Stage2TransientPlanner,
};

/// 一个唯一内容的当前进程二筛请求。
#[derive(Clone, Debug)]
pub struct Stage2Request {
    /// 本轮运行时任务 ID。
    pub task_id: TaskId,
    /// 本轮运行时任务项 ID。
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

/// 远端导入前冻结的本机二筛缓存；只允许重发调用前已存在的字段。
#[derive(Clone, Copy, Debug, Default)]
struct PreexistingStage2 {
    /// 本机已有完整图片二筛。
    image: bool,
    /// 本机已有完整视频槽位的六位掩码。
    video_slots: u8,
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

/// 当前进程内保存的稳定二筛工作集合；不映射 SQLite 任务表。
pub(crate) struct Stage2BatchPlan {
    /// 运行时使用的临时任务身份，不作为持久任务写入数据库。
    pub(crate) task_id: TaskId,
    /// 已校验来源位置的二筛工作项。
    work: Vec<MissingWork>,
}

/// 二筛生产编排所需的固定目录、读取额度和取消边界。
pub(crate) struct Stage2TaskFileRunOptions<'a> {
    /// 当前进程独占的 runtime 根目录；仅 Compute 时才创建其 run 子目录。
    runtime_root: &'a Path,
    /// 视频联系表缓存根目录；目标始终由内容 MD5 推导。
    contact_sheet_root: &'a Path,
    /// 本轮读取调度器使用的已验证磁盘额度配置。
    read_config: &'a DiskReadConfig,
    /// 当前 WorkerPool 的有效并发槽位数。
    worker_capacity: usize,
    /// SQLite 单写 actor 的有界持久化队列容量。
    persist_capacity: usize,
    /// 当前运行取消后传递给 task-file runner 的同一令牌。
    cancellation: ReadCancellationToken,
    /// 只在缓存查询前建立 ScanDiskPlan 时调用的物理盘解析边界。
    resolver: &'a dyn ScanRootStorageResolver,
    /// 单元测试在首次缓存查询处观察已完成的来源解析；生产构建不保留测试状态。
    #[cfg(test)]
    cache_lookup_observer: Option<Arc<dyn Fn() + Send + Sync>>,
    /// 测试仅将首条 SQLite 持久化暂停，以观测 discard 前 writer 是否已真实 join。
    #[cfg(all(test, feature = "test-hooks"))]
    first_persist_waiter: Option<Arc<Mutex<Option<BasePersistTestWaiter>>>>,
    /// 测试仅在精确 discard 前记录 actor 收束与运行目录状态。
    #[cfg(all(test, feature = "test-hooks"))]
    before_discard_observer: Option<Arc<dyn Fn() + Send + Sync>>,
}

impl<'a> Stage2TaskFileRunOptions<'a> {
    /// 使用节点已解析的目录、读取配置和 Worker 容量创建一次生产运行选项。
    #[allow(clippy::too_many_arguments)]
    pub(crate) fn new(
        runtime_root: &'a Path,
        contact_sheet_root: &'a Path,
        read_config: &'a DiskReadConfig,
        worker_capacity: usize,
        persist_capacity: usize,
        cancellation: ReadCancellationToken,
        resolver: &'a dyn ScanRootStorageResolver,
    ) -> Self {
        Self {
            runtime_root,
            contact_sheet_root,
            read_config,
            worker_capacity,
            persist_capacity,
            cancellation,
            resolver,
            #[cfg(test)]
            cache_lookup_observer: None,
            #[cfg(all(test, feature = "test-hooks"))]
            first_persist_waiter: None,
            #[cfg(all(test, feature = "test-hooks"))]
            before_discard_observer: None,
        }
    }

    /// 仅单元测试：在首个本地批量缓存查询前检查来源物理盘冻结顺序。
    #[cfg(test)]
    fn with_cache_lookup_observer(mut self, observer: Arc<dyn Fn() + Send + Sync>) -> Self {
        self.cache_lookup_observer = Some(observer);
        self
    }

    /// 仅单元测试：把现有 writer join 观测与 discard 前窄观察器接到同一次生产运行。
    #[cfg(all(test, feature = "test-hooks"))]
    fn with_task_file_lifecycle_observer(
        mut self,
        first_persist_waiter: BasePersistTestWaiter,
        before_discard_observer: Arc<dyn Fn() + Send + Sync>,
    ) -> Self {
        self.first_persist_waiter = Some(Arc::new(Mutex::new(Some(first_persist_waiter))));
        self.before_discard_observer = Some(before_discard_observer);
        self
    }

    /// 仅单元测试：在当前 run 目录删除前执行窄观察，不改变生产构建状态。
    #[cfg(all(test, feature = "test-hooks"))]
    fn observe_before_discard(&self) {
        if let Some(observer) = &self.before_discard_observer {
            observer();
        }
    }
}

/// 二筛 task-file 运行成功后交还的 SQLite Store、内容统计和真实 outbox 高水位。
pub(crate) struct Stage2TaskFileRunResult {
    /// 所有 Worker、permit 和单写 actor 收束后归还的原 NodeStore。
    pub(crate) store: NodeStore,
    /// 本轮临时 TSV 目录使用的唯一运行身份。
    pub(crate) run_id: TaskId,
    /// 缓存复用、远端导入或 SQLite ACK 成功的唯一内容数。
    pub(crate) completed: usize,
    /// IncompleteBase 或 task-file 单项失败的唯一内容数。
    pub(crate) failed: usize,
    /// 本次全部成功写入之后从 SQLite 读取的真实 outbox 高水位。
    pub(crate) outbox_high_seq: u64,
}

/// 二筛生产编排失败时保留已恢复 Store 的错误；actor 无法 join 时才可能缺失所有权。
pub(crate) struct Stage2TaskFileRunError {
    /// 可继续交回上层的 Store；任务级错误后的已处理结果仍已提交。
    store: Option<NodeStore>,
    /// 原始失败、精确目录清理或 writer 收束的组合诊断。
    message: String,
}

impl Stage2TaskFileRunError {
    /// 取回任务级失败后仍可用的 SQLite Store，避免调用方遗失唯一所有权。
    pub(crate) fn into_store(self) -> Option<NodeStore> {
        self.store
    }
}

impl fmt::Display for Stage2TaskFileRunError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(&self.message)
    }
}

impl fmt::Debug for Stage2TaskFileRunError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("Stage2TaskFileRunError")
            .field("message", &self.message)
            .field("has_store", &self.store.is_some())
            .finish()
    }
}

/// 用已冻结来源执行生产二筛：缓存命中直接提交，真实缺失才进入统一 TSV/读取/ACK 闭环。
pub(crate) async fn run_stage2_batch_production(
    mut store: NodeStore,
    plan: Stage2BatchPlan,
    worker_pool: &mut WorkerPool,
    remote: Option<&NodeRemoteFeatureCache>,
    options: &Stage2TaskFileRunOptions<'_>,
) -> Result<Stage2TaskFileRunResult, Stage2TaskFileRunError> {
    let Stage2BatchPlan { task_id, work } = plan;
    let (frozen, planned_rows) = match freeze_task_file_sources(&work, options) {
        Ok(value) => value,
        Err(error) => return Err(task_file_run_error(store, error)),
    };

    #[cfg(test)]
    if let Some(observer) = &options.cache_lookup_observer {
        observer();
    }
    let keys = frozen
        .sources()
        .iter()
        .map(|source| source.content)
        .collect::<Vec<_>>();
    let local = match store.lookup_base_cache_by_keys(&keys) {
        Ok(records) if records.len() == keys.len() => records,
        Ok(_) => {
            return Err(task_file_run_error(
                store,
                "二筛本地缓存批量返回数量不匹配".into(),
            ));
        }
        Err(error) => return Err(task_file_run_error(store, error.to_string())),
    };
    let remote = if needs_remote_task_file_stage2(&frozen, &local) {
        lookup_remote_task_file_stage2(&frozen, remote).await
    } else {
        None
    };
    let transient = match Stage2TransientPlanner::plan(&frozen, &local, remote.as_deref()) {
        Ok(plan) => plan,
        Err(error) => return Err(task_file_run_error(store, error.to_string())),
    };
    let cached_by_content = local
        .into_iter()
        .flatten()
        .map(|cached| (cached.content_key, cached))
        .collect::<HashMap<_, _>>();

    let mut completed = 0;
    let mut failed = 0;
    let mut inputs = Vec::new();
    for item in transient.items() {
        let mut needs_compute = false;
        let mut incomplete_base = false;
        for action in item.actions() {
            match action {
                Stage2PlanAction::RepublishLocal { selection } => {
                    let Some(cached) = cached_by_content.get(&item.source().content) else {
                        return Err(task_file_run_error(
                            store,
                            "二筛本地重发缺少已分类缓存".into(),
                        ));
                    };
                    if let Err(error) = republish_task_file_stage2(&mut store, cached, *selection) {
                        return Err(task_file_run_error(store, error));
                    }
                }
                Stage2PlanAction::ImportRemote {
                    features,
                    selection,
                } => {
                    if let Err(error) =
                        import_task_file_stage2(&mut store, item.source(), features, *selection)
                    {
                        return Err(task_file_run_error(store, error));
                    }
                }
                Stage2PlanAction::Compute(work) => {
                    let Some(cached) = cached_by_content.get(&work.source().content) else {
                        return Err(task_file_run_error(
                            store,
                            "二筛 Worker 计划缺少本地缓存快照".into(),
                        ));
                    };
                    inputs.push(Stage2TaskInput::from_planner_work(
                        work,
                        cached.clone(),
                        task_file_contact_sheet_target(options.contact_sheet_root, work),
                    ));
                    needs_compute = true;
                }
                Stage2PlanAction::IncompleteBase => incomplete_base = true,
            }
        }
        if incomplete_base {
            failed += 1;
        } else if !needs_compute {
            completed += 1;
        }
    }

    if inputs.is_empty() {
        let outbox_high_seq = match store.outbox_high_seq() {
            Ok(highwater) => highwater,
            Err(error) => return Err(task_file_run_error(store, error.to_string())),
        };
        return Ok(Stage2TaskFileRunResult {
            store,
            run_id: task_id,
            completed,
            failed,
            outbox_high_seq,
        });
    }

    let reader = match ScheduledFileReader::new_with_planned_rows(
        options.read_config,
        options.worker_capacity,
        Arc::new(planned_rows),
    ) {
        Ok((reader, _)) => reader,
        Err(error) => return Err(task_file_run_error(store, error.to_string())),
    };
    let production = match build_stage2_task_production(
        options.runtime_root,
        task_id.as_uuid(),
        reader,
        &inputs,
    ) {
        Ok(production) => production,
        Err(error) => {
            let cleanup = discard_unowned_task_file_run(options.runtime_root, task_id)
                .err()
                .map(|cleanup| cleanup.to_string());
            return Err(task_file_run_error(
                store,
                merge_task_file_failure(&error.to_string(), cleanup, String::new()),
            ));
        }
    };
    #[cfg(all(test, feature = "test-hooks"))]
    let first_persist_waiter = options.first_persist_waiter.as_ref().and_then(|waiter| {
        waiter
            .lock()
            .expect("二筛测试 writer waiter 锁不应中毒")
            .take()
    });
    #[cfg(all(test, feature = "test-hooks"))]
    let (store_actor, store_handle, mut acknowledgements) = match first_persist_waiter {
        Some(waiter) => BaseStoreActor::spawn_with_first_persist_waiter(
            store,
            options.persist_capacity.max(1),
            waiter,
        ),
        None => BaseStoreActor::spawn(store, options.persist_capacity.max(1)),
    };
    #[cfg(not(all(test, feature = "test-hooks")))]
    let (store_actor, store_handle, mut acknowledgements) =
        BaseStoreActor::spawn(store, options.persist_capacity.max(1));
    let run = run_task_file_stage2(
        production,
        worker_pool,
        &store_handle,
        &mut acknowledgements,
        options.worker_capacity,
        options.cancellation.clone(),
    )
    .await;

    match run {
        Ok(run) => {
            let stage_completed = run.completed;
            let stage_failed = run.failed;
            let mut production = run.production;
            drop(store_handle);
            drop(acknowledgements);
            let store = match store_actor.finish().await {
                Ok(store) => store,
                Err(error) => {
                    // actor owner 已结束；即使 join 失败也只尽力删除当前 run 目录。
                    #[cfg(all(test, feature = "test-hooks"))]
                    options.observe_before_discard();
                    let cleanup = production
                        .discard()
                        .err()
                        .map(|cleanup| cleanup.to_string());
                    return Err(Stage2TaskFileRunError {
                        store: None,
                        message: merge_task_file_failure(
                            "二筛 SQLite writer 收束失败",
                            cleanup,
                            error.to_string(),
                        ),
                    });
                }
            };
            let outbox_high_seq = match store.outbox_high_seq() {
                Ok(highwater) => highwater,
                Err(error) => {
                    // 已归还 Store 后仍需按当前 run 身份尽力清理。
                    #[cfg(all(test, feature = "test-hooks"))]
                    options.observe_before_discard();
                    let cleanup = production
                        .discard()
                        .err()
                        .map(|cleanup| cleanup.to_string());
                    return Err(task_file_run_error(
                        store,
                        merge_task_file_failure(&error.to_string(), cleanup, String::new()),
                    ));
                }
            };
            // Store、ACK 和真实 highwater 全部收束后，才可精确删除本轮瞬态目录。
            #[cfg(all(test, feature = "test-hooks"))]
            options.observe_before_discard();
            let cleanup = production.discard().err().map(|error| error.to_string());
            if let Some(cleanup) = cleanup {
                return Err(task_file_run_error(
                    store,
                    format!("二筛任务目录清理失败: {cleanup}"),
                ));
            }
            Ok(Stage2TaskFileRunResult {
                store,
                run_id: task_id,
                completed: completed + stage_completed,
                failed: failed + stage_failed,
                outbox_high_seq,
            })
        }
        Err(error) => {
            let message = error.to_string();
            let mut production = error.into_production();
            drop(store_handle);
            drop(acknowledgements);
            match store_actor.finish().await {
                Ok(store) => {
                    // 失败 runner 的 actor 也必须先归还 Store，之后才能清理瞬态目录。
                    #[cfg(all(test, feature = "test-hooks"))]
                    options.observe_before_discard();
                    let cleanup = production
                        .discard()
                        .err()
                        .map(|cleanup| cleanup.to_string());
                    Err(task_file_run_error(
                        store,
                        merge_task_file_failure(&message, cleanup, String::new()),
                    ))
                }
                Err(writer) => {
                    // join 失败后 actor owner 已被消费，保留原始 runner/writer/清理诊断。
                    #[cfg(all(test, feature = "test-hooks"))]
                    options.observe_before_discard();
                    let cleanup = production
                        .discard()
                        .err()
                        .map(|cleanup| cleanup.to_string());
                    Err(Stage2TaskFileRunError {
                        store: None,
                        message: merge_task_file_failure(&message, cleanup, writer.to_string()),
                    })
                }
            }
        }
    }
}

/// 判断本机已有完整基础字段但仍缺请求二筛字段时，是否需要批量查询远端缓存。
fn needs_remote_task_file_stage2(
    frozen: &FrozenStage2Batch,
    local: &[Option<BaseCacheRecord>],
) -> bool {
    frozen.sources().iter().zip(local).any(|(source, cached)| {
        let Some(cached) = cached else { return false };
        let completeness = classify_cache_completeness(cached, true);
        if completeness.base_missing_parts & (BASE_MISSING_PROBE | BASE_MISSING_STAGE1) != 0 {
            return false;
        }
        match source.media_kind {
            MediaKind::Image => completeness.image_stage2_missing,
            MediaKind::Video => source
                .frame_slots
                .iter()
                .any(|slot| completeness.video_stage2_missing_slots & (1_u8 << slot) != 0),
            MediaKind::Other => false,
        }
    })
}

/// 在所有本地或远端缓存查询前把批次来源映射为稳定的物理盘 lane。
fn freeze_task_file_sources(
    work: &[MissingWork],
    options: &Stage2TaskFileRunOptions<'_>,
) -> Result<(FrozenStage2Batch, Vec<crate::scan::PlannedScannedPath>), String> {
    let roots = work
        .iter()
        .map(|item| item.display_path.clone())
        .collect::<Vec<_>>();
    let disk_plan = ScanDiskPlan::build(&roots, options.read_config, options.resolver)
        .map_err(|error| error.to_string())?;
    let scanned = work
        .iter()
        .map(|item| {
            ScannedPath::new(
                item.location.normalized_path().clone(),
                item.display_path.clone(),
                item.content.file_size(),
            )
        })
        .collect::<Vec<_>>();
    let planned = disk_plan
        .assign_all(scanned)
        .map_err(|error| error.to_string())?;
    let inputs = work
        .iter()
        .zip(&planned)
        .map(|(item, planned)| {
            Stage2PlanningInput::from_active(Stage2ActiveSource::new(
                item.content,
                item.content_id,
                item.location.clone(),
                planned.scanned.clone(),
                item.media_kind,
                item.frame_slots.clone(),
                planned.lane.clone(),
            ))
        })
        .collect::<Vec<_>>();
    let frozen = Stage2TransientPlanner::freeze(&inputs).map_err(|error| error.to_string())?;
    Ok((frozen, planned))
}

/// 批量读取远端二筛缓存；连接、长度或查询错误只降级为本地计算。
async fn lookup_remote_task_file_stage2(
    frozen: &FrozenStage2Batch,
    remote: Option<&NodeRemoteFeatureCache>,
) -> Option<Vec<Option<CompleteStage2>>> {
    let remote = remote?;
    let requests = frozen
        .sources()
        .iter()
        .map(|source| Stage2CacheLookup {
            content: source.content,
            media_kind: source.media_kind,
            frame_slots: source.frame_slots.clone(),
        })
        .collect::<Vec<_>>();
    match remote.lookup_stage2(&requests).await {
        Ok(hits) if hits.len() == requests.len() => Some(hits),
        Ok(_) => {
            tracing::warn!("PostgreSQL 二筛缓存返回数量不匹配，本次继续 SQLite-only");
            None
        }
        Err(error) => {
            tracing::warn!(error = %error, "PostgreSQL 二筛缓存查询失败，本次继续 SQLite-only");
            None
        }
    }
}

/// 将本机已完整的选择重新发布到 outbox，不扩大到当前批次以外的槽位。
fn republish_task_file_stage2(
    store: &mut NodeStore,
    cached: &BaseCacheRecord,
    selection: Stage2Selection,
) -> Result<(), String> {
    let republished = match selection {
        Stage2Selection::Image => store
            .republish_complete_stage2_from_cache(cached)
            .map_err(|error| error.to_string())?,
        Stage2Selection::VideoSlots(slots) => store
            .republish_stage2_slots_from_cache(
                cached,
                &(0..6)
                    .filter(|slot| slots & (1_u8 << slot) != 0)
                    .collect::<Vec<_>>(),
            )
            .map_err(|error| error.to_string())?,
    };
    republished
        .then_some(())
        .ok_or_else(|| "二筛本地缓存不完整，无法重发".into())
}

/// 将计划器已验证的远端图片或视频槽位写入 SQLite，并由事务同步写出 outbox。
fn import_task_file_stage2(
    store: &mut NodeStore,
    source: &Stage2ActiveSource,
    features: &CompleteStage2,
    selection: Stage2Selection,
) -> Result<(), String> {
    match (selection, features) {
        (Stage2Selection::Image, CompleteStage2::Image(features)) => store
            .commit_feature_result(
                source.content_id,
                None,
                FeatureWrite::ImageStage2(**features),
            )
            .map(|_| ())
            .map_err(|error| error.to_string()),
        (Stage2Selection::VideoSlots(slots), CompleteStage2::Video(features)) => {
            for slot in 0..6 {
                if slots & (1_u8 << slot) == 0 {
                    continue;
                }
                let feature = features[slot]
                    .as_ref()
                    .ok_or_else(|| "远端二筛缺少计划视频槽位".to_owned())?;
                store
                    .commit_feature_result(
                        source.content_id,
                        None,
                        FeatureWrite::VideoFrameStage2(VideoFrameStage2Fields {
                            slot: slot as u8,
                            features: feature.clone(),
                        }),
                    )
                    .map_err(|error| error.to_string())?;
            }
            Ok(())
        }
        _ => Err("远端二筛类型与计划选择不一致".into()),
    }
}

/// 为视频 Worker 固定推导 MD5 联系表路径；文件不存在时仍保留原视频回退目标。
fn task_file_contact_sheet_target(
    root: &Path,
    work: &super::stage2_planner::Stage2WorkItem,
) -> Option<PathBuf> {
    (work.source().media_kind == MediaKind::Video)
        .then(|| ContactSheetCacheEntry::from_md5(root, work.source().content.md5()))
        .map(|entry| entry.final_path().to_path_buf())
}

/// 用可恢复 Store 构造前 actor 阶段或收束成功后的任务级错误。
fn task_file_run_error(store: NodeStore, message: String) -> Stage2TaskFileRunError {
    Stage2TaskFileRunError {
        store: Some(store),
        message,
    }
}

/// 构建器尚未交还 production owner 时，仅删除已知 run-id 对应的精确目录。
fn discard_unowned_task_file_run(runtime_root: &Path, task_id: TaskId) -> std::io::Result<()> {
    let run = runtime_root.join(task_id.as_uuid().to_string());
    match std::fs::remove_dir_all(run) {
        Ok(()) => Ok(()),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(error),
    }
}

/// 合并原始任务失败、精确 run 目录清理和 SQLite writer 收束的诊断。
fn merge_task_file_failure(primary: &str, cleanup: Option<String>, writer: String) -> String {
    let mut message = primary.to_owned();
    if let Some(cleanup) = cleanup {
        message.push_str("；二筛任务目录清理失败: ");
        message.push_str(&cleanup);
    }
    if !writer.is_empty() {
        message.push_str("；SQLite writer 收束失败: ");
        message.push_str(&writer);
    }
    message
}

/// 执行一个中心二筛批次；先复用并重新发布本机缓存，只有真正缺失时才调用 Worker。
///
/// 整个批次只在当前进程保留工作集合；缓存命中直接重发，缺失项交给处理器计算。
/// 返回的 ID 仅用于运行时事件和 Worker 身份，不创建 `tasks/task_items/task_stages` 行。
pub async fn dispatch_stage2_batch<P: Stage2Processor>(
    store: &mut NodeStore,
    items: &[Stage2BatchItem],
    processor: &mut P,
    now_ms: i64,
) -> Result<TaskId, AnalysisBlocked> {
    let plan = begin_stage2_batch(store, items, now_ms)?;
    run_stage2_batch(store, plan, processor, now_ms).await
}

/// 校验来源并冻结当前进程的二筛工作集合，不写入旧任务表。
pub(crate) fn begin_stage2_batch(
    store: &mut NodeStore,
    items: &[Stage2BatchItem],
    _now_ms: i64,
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
    Ok(Stage2BatchPlan {
        task_id: TaskId::new(),
        work,
    })
}

/// 从当前进程的二筛工作集合继续缓存重发和 Worker 计算。
pub(crate) async fn run_stage2_batch<P: Stage2Processor>(
    store: &mut NodeStore,
    plan: Stage2BatchPlan,
    processor: &mut P,
    now_ms: i64,
) -> Result<TaskId, AnalysisBlocked> {
    run_stage2_batch_internal(store, plan, processor, None, None, None, now_ms).await
}

/// 运行瞬态二筛，并为视频启用固定 MD5 联系表复用与重建。
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
    _now_ms: i64,
) -> Result<TaskId, AnalysisBlocked> {
    let Stage2BatchPlan { task_id, work } = plan;
    let total = work.len() as u64;
    report_stage2_stage(
        reporter,
        RuntimeStage::LookupStage2Cache,
        proto::RuntimeStageState::RuntimeStageRunning,
        0,
        total,
        0,
        0,
    );
    let initial_records = store
        .lookup_base_cache_by_keys(&work.iter().map(|item| item.content).collect::<Vec<_>>())?;
    if initial_records.len() != work.len() {
        return Err(AnalysisBlocked::InvalidState(
            "二筛远端导入前的基础缓存批量返回数量不匹配".into(),
        ));
    }
    let preexisting_by_content = work
        .iter()
        .zip(initial_records.iter())
        .filter_map(|(item, record)| {
            record.as_ref().and_then(|record| {
                record
                    .content_id
                    .map(|content_id| (content_id, preexisting_stage2(item, record)))
            })
        })
        .collect::<HashMap<_, _>>();
    let cache_warning =
        resolve_remote_stage2(store, &work, remote, &preexisting_by_content).await?;
    report_stage2_stage(
        reporter,
        RuntimeStage::LookupStage2Cache,
        proto::RuntimeStageState::RuntimeStageCompleted,
        total,
        total,
        0,
        0,
    );
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
    for expected in &work {
        let cached = cached_by_content.get(&expected.content_id);
        if cached.is_some_and(|record| {
            classify_cache_completeness(record, true).base_missing_parts
                & (BASE_MISSING_PROBE | BASE_MISSING_STAGE1)
                != 0
        }) {
            completed += 1;
            skipped += 1;
            continue;
        }
        let preexisting = preexisting_by_content
            .get(&expected.content_id)
            .copied()
            .unwrap_or_default();
        if expected.media_kind == MediaKind::Image
            && cached.is_some_and(|record| stage2_complete_for_expected(expected, record))
        {
            if preexisting.image {
                let cached = cached.expect("图片二筛完整度判断已确认记录存在");
                if !store.republish_complete_stage2_from_cache(cached)? {
                    return Err(AnalysisBlocked::InvalidState(
                        "图片已有二筛缓存但无法重发".into(),
                    ));
                }
            }
            completed += 1;
            continue;
        }
        let frame_slots = missing_stage2_slots(expected, cached);
        let existing_slots = expected
            .frame_slots
            .iter()
            .copied()
            .filter(|slot| preexisting.video_slots & (1_u8 << slot) != 0)
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
            completed += 1;
            continue;
        }
        let item_id = Uuid::now_v7().to_string();
        let request = Stage2Request {
            task_id,
            item_id: item_id.clone(),
            content: expected.content,
            content_id: expected.content_id,
            display_path: expected.display_path.clone(),
            media_kind: expected.media_kind,
            frame_slots,
            contact_sheet_path: contact_sheet_target(contact_sheet_root, expected),
        };
        pending.push((
            item_id,
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
    processor
        .process_batch_each(requests, |index, result| {
            let Some((item_id, expected, contact_sheet_path)) = pending.get(index) else {
                return Err("二筛处理器返回序号越界".into());
            };
            let mut runtime_failure = None;
            match result {
                Ok(output) => {
                    let persisted = persist_stage2(
                        store,
                        expected,
                        item_id,
                        contact_sheet_path.as_deref(),
                        output,
                    )
                    .map_err(|error| error.to_string())?;
                    if !persisted {
                        failed += 1;
                        runtime_failure = Some("二筛结果不完整".into());
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
                }
            }
            // completed 表示已经收到处理器终态；failed/skipped 再从中区分成功数量。
            completed += 1;
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
    let mut preexisting_by_content = HashMap::new();
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
                // 保留全部成功槽位作为本次中心要求；远端导入前据此冻结已有槽位，
                // 真正交给 Worker 的集合仍在刷新缓存后由缺失掩码计算。
                let frame_slots = frames
                    .iter()
                    .enumerate()
                    .filter_map(|(slot, frame)| frame.map(|_| slot as u8))
                    .collect();
                (MediaKind::Video, frame_slots)
            }
            MediaKind::Video => continue,
            MediaKind::Other => continue,
        };
        let item = MissingWork {
            content,
            content_id,
            location,
            display_path,
            media_kind,
            frame_slots,
        };
        preexisting_by_content.insert(content_id, preexisting_stage2(&item, &record));
        work.push(item);
    }
    if work.is_empty() {
        return Ok(MissingDispatchReport::default());
    }
    let _ = resolve_remote_stage2(store, &work, remote, &preexisting_by_content).await?;
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
        let cached = cached_by_content.get(&expected.content_id);
        let preexisting = preexisting_by_content
            .get(&expected.content_id)
            .copied()
            .unwrap_or_default();
        if cached.is_some_and(|record| stage2_complete_for_expected(expected, record)) {
            if expected.media_kind == MediaKind::Image && preexisting.image {
                if !store.republish_complete_stage2_from_cache(
                    cached.expect("图片二筛完整度判断已确认记录存在"),
                )? {
                    return Err(AnalysisBlocked::InvalidState(
                        "图片已有二筛缓存但无法重发".into(),
                    ));
                }
            } else if expected.media_kind == MediaKind::Video {
                let existing_slots = expected
                    .frame_slots
                    .iter()
                    .copied()
                    .filter(|slot| preexisting.video_slots & (1_u8 << slot) != 0)
                    .collect::<Vec<_>>();
                if !existing_slots.is_empty()
                    && !store.republish_stage2_slots_from_cache(
                        cached.expect("视频二筛完整度判断已确认记录存在"),
                        &existing_slots,
                    )?
                {
                    return Err(AnalysisBlocked::InvalidState(
                        "视频已有二筛槽位但无法重发".into(),
                    ));
                }
            }
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
        let frame_slots = missing_stage2_slots(expected, cached);
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

/// 冻结远端导入前已经存在且可重新发布的二筛字段。
fn preexisting_stage2(expected: &MissingWork, cached: &BaseCacheRecord) -> PreexistingStage2 {
    let completeness = classify_cache_completeness(cached, true);
    if completeness.base_missing_parts & (BASE_MISSING_PROBE | BASE_MISSING_STAGE1) != 0 {
        return PreexistingStage2::default();
    }
    match expected.media_kind {
        MediaKind::Image => PreexistingStage2 {
            image: !completeness.image_stage2_missing,
            video_slots: 0,
        },
        MediaKind::Video => PreexistingStage2 {
            image: false,
            video_slots: expected
                .frame_slots
                .iter()
                .filter(|slot| completeness.video_stage2_missing_slots & (1_u8 << *slot) == 0)
                .fold(0, |mask, slot| mask | (1_u8 << *slot)),
        },
        MediaKind::Other => PreexistingStage2::default(),
    }
}

/// 判断缓存是否已经覆盖当前批次要求的图片或视频槽位。
fn stage2_complete_for_expected(expected: &MissingWork, cached: &BaseCacheRecord) -> bool {
    let completeness = classify_cache_completeness(cached, true);
    match expected.media_kind {
        MediaKind::Image => !completeness.image_stage2_missing,
        MediaKind::Video => missing_stage2_slots(expected, Some(cached)).is_empty(),
        MediaKind::Other => false,
    }
}

/// 批量查询 PostgreSQL 二次缓存并导入 SQLite；失败只降级到本地计算。
async fn resolve_remote_stage2(
    store: &mut NodeStore,
    work: &[MissingWork],
    remote: Option<&mut NodeRemoteFeatureCache>,
    preexisting_by_content: &HashMap<ContentId, PreexistingStage2>,
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
        let preexisting = preexisting_by_content
            .get(&item.content_id)
            .copied()
            .unwrap_or_default();
        persist_cached_stage2(store, item, hit, preexisting)?;
    }
    Ok(startup_warning)
}

/// 把中心二次缓存的请求槽位写入 SQLite 与 outbox，跳过导入前已存在的字段。
fn persist_cached_stage2(
    store: &mut NodeStore,
    item: &MissingWork,
    cached: CompleteStage2,
    preexisting: PreexistingStage2,
) -> Result<(), AnalysisBlocked> {
    match cached {
        CompleteStage2::Image(feature) => {
            if !preexisting.image {
                store.commit_feature_result(
                    item.content_id,
                    None,
                    FeatureWrite::ImageStage2(*feature),
                )?;
            }
        }
        CompleteStage2::Video(frames) => {
            for slot in item.frame_slots.iter().copied() {
                let index = usize::from(slot);
                if preexisting.video_slots & (1_u8 << slot) == 0
                    && let Some(feature) = frames[index]
                {
                    store.commit_feature_result(
                        item.content_id,
                        None,
                        FeatureWrite::VideoFrameStage2(VideoFrameStage2Fields {
                            slot,
                            features: feature,
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

#[cfg(test)]
mod tests {
    use super::*;
    use std::{
        sync::{
            Arc,
            atomic::{AtomicBool, AtomicUsize, Ordering},
        },
        time::Duration,
    };

    use dedup_core::DiskReadConfig;
    use dedup_core::{DisplayPath, LocationKey, MachineId, NormalizedPath};
    use dedup_media::{ImageStage1, ImageStage2, PdqHash};
    use dedup_node_store::{
        FeatureWrite, ImageStage1Fields, ScannedPath, VideoFrameStage1Fields,
        VideoFrameStage2Fields, VideoMetadataFields,
    };
    use dedup_windows::{LocalDiskKind, PhysicalDiskId, ReadCancellationToken};

    use crate::{
        scan::{BasePersistTestController, ResolvedScanRootStorage, ScanRootStorageResolver},
        worker::Stage2Frame,
    };

    #[tokio::test]
    async fn production_task_file_stage2_uses_full_local_cache_without_runtime_or_worker() {
        let runtime = tempfile::tempdir().unwrap();
        let mut store = test_store();
        let (content, source, content_id) = seed_image(&mut store, [0x61; 16], 61, true);
        let plan = begin_stage2_batch(
            &mut store,
            &[Stage2BatchItem {
                content,
                source,
                frame_slots: Vec::new(),
            }],
            61,
        )
        .unwrap();
        let run_id = plan.task_id;
        let resolver = FixedLaneResolver::new(1);
        let read_config = DiskReadConfig::default();
        let options = Stage2TaskFileRunOptions::new(
            runtime.path(),
            runtime.path(),
            &read_config,
            1,
            2,
            ReadCancellationToken::new(),
            &resolver,
        );
        let before = store.outbox_high_seq().unwrap();
        let (mut pool, mut started, _) = WorkerPool::controlled_batch_for_test(1);

        let result = run_stage2_batch_production(store, plan, &mut pool, None, &options)
            .await
            .unwrap();

        assert_eq!(result.completed, 1);
        assert_eq!(result.failed, 0);
        assert!(
            started.try_recv().is_err(),
            "完整本地缓存不能下发二筛 Worker"
        );
        assert!(
            !runtime.path().join(run_id.as_uuid().to_string()).exists(),
            "没有 Compute 时不能创建本轮 TSV 目录"
        );
        assert!(
            result.store.page_tasks(None, 20).unwrap().items.is_empty(),
            "生产二筛不能回退写入旧任务表"
        );
        assert!(result.store.outbox_high_seq().unwrap() > before);
        assert_eq!(
            result.outbox_high_seq,
            result.store.outbox_high_seq().unwrap()
        );
        assert!(
            result
                .store
                .load_complete_stage2(content_id)
                .unwrap()
                .is_some()
        );
    }

    #[cfg(feature = "test-hooks")]
    #[tokio::test]
    async fn production_task_file_stage2_finishes_writer_before_discarding_successful_run() {
        let runtime = tempfile::tempdir().unwrap();
        let mut store = test_store();
        let (content, source, content_id) = seed_image(&mut store, [0x67; 16], 67, false);
        let plan = begin_stage2_batch(
            &mut store,
            &[Stage2BatchItem {
                content,
                source,
                frame_slots: Vec::new(),
            }],
            67,
        )
        .unwrap();
        let run_id = plan.task_id;
        let run_directory = runtime.path().join(run_id.as_uuid().to_string());
        let resolver = FixedLaneResolver::new(1);
        let read_config = DiskReadConfig::default();
        let (persist_control, persist_waiter) = BasePersistTestController::new();
        let persist_control = Arc::new(persist_control);
        let observed_control = Arc::clone(&persist_control);
        let discard_seen = Arc::new(AtomicBool::new(false));
        let joined_at_discard = Arc::new(AtomicBool::new(false));
        let directory_alive_at_discard = Arc::new(AtomicBool::new(false));
        let observed_discard = Arc::clone(&discard_seen);
        let observed_joined = Arc::clone(&joined_at_discard);
        let observed_directory = Arc::clone(&directory_alive_at_discard);
        let options = Stage2TaskFileRunOptions::new(
            runtime.path(),
            runtime.path(),
            &read_config,
            1,
            2,
            ReadCancellationToken::new(),
            &resolver,
        )
        .with_task_file_lifecycle_observer(
            persist_waiter,
            Arc::new(move || {
                observed_discard.store(true, Ordering::SeqCst);
                observed_joined.store(observed_control.writer_joined(), Ordering::SeqCst);
                observed_directory.store(run_directory.exists(), Ordering::SeqCst);
            }),
        );
        let (mut pool, mut started, worker_control) = WorkerPool::controlled_batch_for_test(1);
        let run = run_stage2_batch_production(store, plan, &mut pool, None, &options);
        let control = async {
            let (task_id, item_id) = started.recv().await.unwrap();
            worker_control
                .stage2_source_read_complete(task_id.clone(), item_id.clone())
                .await;
            worker_control
                .complete_stage2(task_id, item_id, stage2_output(9))
                .await;
            persist_control.wait_until_entered().await;
            persist_control.release();
        };
        let (result, ()) =
            tokio::time::timeout(Duration::from_secs(2), async { tokio::join!(run, control) })
                .await
                .expect("成功二筛必须在有限时间内收束 writer 并删除运行目录");
        pool.shutdown().await.unwrap();
        let result = result.unwrap();

        assert!(
            discard_seen.load(Ordering::SeqCst),
            "成功路径必须精确 discard 本轮目录"
        );
        assert!(
            joined_at_discard.load(Ordering::SeqCst),
            "成功路径必须先真实 join SQLite writer，最后才 discard"
        );
        assert!(
            directory_alive_at_discard.load(Ordering::SeqCst),
            "discard 前当前 run 目录必须仍存在，避免观察到其他目录"
        );
        assert!(
            !runtime
                .path()
                .join(result.run_id.as_uuid().to_string())
                .exists(),
            "writer 收束后的 discard 只能删除当前 run 目录"
        );
        assert!(
            result
                .store
                .load_complete_stage2(content_id)
                .unwrap()
                .is_some(),
            "真实 SQLite ACK 必须在 writer 收束前已提交二筛结果"
        );
    }

    #[cfg(feature = "test-hooks")]
    #[tokio::test]
    async fn production_task_file_stage2_finishes_writer_before_discarding_failed_run() {
        let runtime = tempfile::tempdir().unwrap();
        let mut store = test_store();
        let (content, source, _) = seed_image(&mut store, [0x68; 16], 68, false);
        let plan = begin_stage2_batch(
            &mut store,
            &[Stage2BatchItem {
                content,
                source,
                frame_slots: Vec::new(),
            }],
            68,
        )
        .unwrap();
        let run_id = plan.task_id;
        let run_directory = runtime.path().join(run_id.as_uuid().to_string());
        let resolver = FixedLaneResolver::new(1);
        let read_config = DiskReadConfig::default();
        let (persist_control, persist_waiter) = BasePersistTestController::new();
        let persist_control = Arc::new(persist_control);
        let observed_control = Arc::clone(&persist_control);
        let discard_seen = Arc::new(AtomicBool::new(false));
        let joined_at_discard = Arc::new(AtomicBool::new(false));
        let directory_alive_at_discard = Arc::new(AtomicBool::new(false));
        let observed_discard = Arc::clone(&discard_seen);
        let observed_joined = Arc::clone(&joined_at_discard);
        let observed_directory = Arc::clone(&directory_alive_at_discard);
        let cancellation = ReadCancellationToken::new();
        let options = Stage2TaskFileRunOptions::new(
            runtime.path(),
            runtime.path(),
            &read_config,
            1,
            2,
            cancellation.clone(),
            &resolver,
        )
        .with_task_file_lifecycle_observer(
            persist_waiter,
            Arc::new(move || {
                observed_discard.store(true, Ordering::SeqCst);
                observed_joined.store(observed_control.writer_joined(), Ordering::SeqCst);
                observed_directory.store(run_directory.exists(), Ordering::SeqCst);
            }),
        );
        let (mut pool, mut started, _worker_control) = WorkerPool::controlled_batch_for_test(1);
        let run = run_stage2_batch_production(store, plan, &mut pool, None, &options);
        let cancel = async {
            let _ = started.recv().await.unwrap();
            cancellation.cancel();
        };
        let (error, ()) =
            tokio::time::timeout(Duration::from_secs(2), async { tokio::join!(run, cancel) })
                .await
                .expect("取消二筛必须在有限时间内收束 writer 并删除运行目录");
        pool.shutdown().await.unwrap();
        let error = match error {
            Ok(_) => panic!("取消运行中的二筛必须返回任务级错误"),
            Err(error) => error,
        };

        assert!(
            error.into_store().is_some(),
            "runner 失败后 writer 收束成功时必须归还 Store"
        );
        assert!(
            discard_seen.load(Ordering::SeqCst),
            "失败路径必须精确 discard 本轮目录"
        );
        assert!(
            joined_at_discard.load(Ordering::SeqCst),
            "失败路径必须先真实 join SQLite writer，最后才 discard"
        );
        assert!(
            directory_alive_at_discard.load(Ordering::SeqCst),
            "失败 discard 前当前 run 目录必须仍存在"
        );
        assert!(
            !runtime.path().join(run_id.as_uuid().to_string()).exists(),
            "失败清理只能删除当前 run 目录"
        );
    }

    #[cfg(feature = "test-hooks")]
    #[tokio::test]
    async fn production_task_file_stage2_keeps_other_frozen_lane_running_after_failure() {
        let runtime = tempfile::tempdir().unwrap();
        let mut store = test_store();
        let (first_content, first_source, first_id) = seed_image(&mut store, [0x62; 16], 62, false);
        let (second_content, second_source, second_id) =
            seed_image_at(&mut store, r"D:\\phase2-second.jpg", [0x63; 16], 63, false);
        let plan = begin_stage2_batch(
            &mut store,
            &[
                Stage2BatchItem {
                    content: first_content,
                    source: first_source,
                    frame_slots: Vec::new(),
                },
                Stage2BatchItem {
                    content: second_content,
                    source: second_source,
                    frame_slots: Vec::new(),
                },
            ],
            62,
        )
        .unwrap();
        let resolver = FixedLaneResolver::new(2);
        let read_config = DiskReadConfig::default();
        let options = Stage2TaskFileRunOptions::new(
            runtime.path(),
            runtime.path(),
            &read_config,
            1,
            2,
            ReadCancellationToken::new(),
            &resolver,
        );
        let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
        let run = run_stage2_batch_production(store, plan, &mut pool, None, &options);
        let control = async {
            let (first_task, first_item) = started.recv().await.unwrap();
            controller
                .crash(first_task, first_item, "测试二筛 Worker 崩溃".into())
                .await;

            let (second_task, second_item) = started.recv().await.unwrap();
            controller
                .stage2_source_read_complete(second_task.clone(), second_item.clone())
                .await;
            controller
                .complete_stage2(second_task, second_item, stage2_output(7))
                .await;
        };
        let (result, ()) =
            tokio::time::timeout(Duration::from_secs(2), async { tokio::join!(run, control) })
                .await
                .expect("不同冻结 lane 的二筛任务必须在有限时间内收敛");
        pool.shutdown().await.unwrap();
        let result = result.unwrap();

        assert_eq!(result.completed, 1);
        assert_eq!(result.failed, 1);
        assert_eq!(resolver.calls(), 2, "所有来源必须在缓存查询前冻结 lane");
        assert!(
            result
                .store
                .load_complete_stage2(first_id)
                .unwrap()
                .is_some()
                ^ result
                    .store
                    .load_complete_stage2(second_id)
                    .unwrap()
                    .is_some(),
            "一个 lane 失败后另一个 lane 仍必须通过 SQLite ACK 写入二筛特征"
        );
        assert!(
            !runtime
                .path()
                .join(result.run_id.as_uuid().to_string())
                .exists(),
            "所有 ACK 与 Worker 收束后必须只删除当前运行目录"
        );
    }

    #[cfg(feature = "test-hooks")]
    #[tokio::test]
    async fn production_task_file_stage2_runs_only_the_missing_video_slot() {
        let runtime = tempfile::tempdir().unwrap();
        let mut store = test_store();
        let (content, source, content_id) =
            seed_video(&mut store, [0x66; 16], &[1, 2, 3, 4, 5], 66);
        let plan = begin_stage2_batch(
            &mut store,
            &[Stage2BatchItem {
                content,
                source,
                frame_slots: vec![0],
            }],
            66,
        )
        .unwrap();
        let resolver = FixedLaneResolver::new(1);
        let read_config = DiskReadConfig::default();
        let options = Stage2TaskFileRunOptions::new(
            runtime.path(),
            runtime.path(),
            &read_config,
            1,
            2,
            ReadCancellationToken::new(),
            &resolver,
        );
        let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
        let run = run_stage2_batch_production(store, plan, &mut pool, None, &options);
        let control = async {
            let (task_id, item_id) = started.recv().await.unwrap();
            controller
                .stage2_source_read_complete(task_id.clone(), item_id.clone())
                .await;
            controller
                .complete_stage2(task_id, item_id, stage2_output(8))
                .await;
        };
        let (result, ()) =
            tokio::time::timeout(Duration::from_secs(2), async { tokio::join!(run, control) })
                .await
                .expect("缺失视频槽位必须由 task-file runner 在有限时间内完成");
        pool.shutdown().await.unwrap();
        let result = result.unwrap();

        assert_eq!(result.completed, 1);
        assert_eq!(result.failed, 0);
        assert!(
            result
                .store
                .load_complete_stage2(content_id)
                .unwrap()
                .is_some(),
            "原有五槽加本次唯一缺槽应共同构成完整视频二筛"
        );
    }

    #[tokio::test]
    async fn production_task_file_stage2_resolves_all_sources_before_first_cache_lookup() {
        let runtime = tempfile::tempdir().unwrap();
        let mut store = test_store();
        let (first_content, first_source, _) = seed_image(&mut store, [0x64; 16], 64, true);
        let (second_content, second_source, _) =
            seed_image_at(&mut store, r"D:\\phase2-order.jpg", [0x65; 16], 65, true);
        let plan = begin_stage2_batch(
            &mut store,
            &[
                Stage2BatchItem {
                    content: first_content,
                    source: first_source,
                    frame_slots: Vec::new(),
                },
                Stage2BatchItem {
                    content: second_content,
                    source: second_source,
                    frame_slots: Vec::new(),
                },
            ],
            64,
        )
        .unwrap();
        let resolver = FixedLaneResolver::new(2);
        let observed_resolver = resolver.clone();
        let read_config = DiskReadConfig::default();
        let options = Stage2TaskFileRunOptions::new(
            runtime.path(),
            runtime.path(),
            &read_config,
            1,
            2,
            ReadCancellationToken::new(),
            &resolver,
        )
        .with_cache_lookup_observer(Arc::new(move || {
            assert_eq!(
                observed_resolver.calls(),
                2,
                "首个二筛缓存查询前必须已经解析全部有效来源"
            );
        }));
        let (mut pool, _, _) = WorkerPool::controlled_batch_for_test(1);

        let result = run_stage2_batch_production(store, plan, &mut pool, None, &options)
            .await
            .unwrap();

        assert_eq!(result.completed, 2);
        assert_eq!(result.failed, 0);
    }

    #[tokio::test]
    async fn complete_stage2_cache_does_not_create_legacy_task_rows() {
        let mut store = test_store();
        let (content, source, _) = seed_video(&mut store, [0; 16], &[0], 100);
        let mut processor = CountingStage2::default();

        let _runtime_id = dispatch_stage2_batch(
            &mut store,
            &[Stage2BatchItem {
                content,
                source,
                frame_slots: vec![0],
            }],
            &mut processor,
            101,
        )
        .await
        .unwrap();

        assert_eq!(processor.calls, 0, "完整二筛缓存命中不能启动 Worker");
        assert!(
            store.page_tasks(None, 20).unwrap().items.is_empty(),
            "瞬态二筛不能创建旧 tasks/task_items/task_stages 行"
        );
    }

    #[tokio::test]
    async fn remote_stage2_import_only_publishes_requested_video_slot() {
        let mut store = test_store();
        let (content, source, _) = seed_video(&mut store, [1; 16], &[], 100);
        let before = store.outbox_high_seq().unwrap();
        let plan = begin_stage2_batch(
            &mut store,
            &[Stage2BatchItem {
                content,
                source,
                frame_slots: vec![0],
            }],
            101,
        )
        .unwrap();
        let mut remote = crate::NodeRemoteFeatureCache::test_with_stage2(vec![Some(
            CompleteStage2::Video(Box::new(full_stage2_frames())),
        )]);
        let mut processor = CountingStage2::default();

        let _runtime_id = run_stage2_batch_internal(
            &mut store,
            plan,
            &mut processor,
            None,
            Some(&mut remote),
            None,
            102,
        )
        .await
        .unwrap();

        assert_eq!(processor.calls, 0);
        assert!(
            store.page_tasks(None, 20).unwrap().items.is_empty(),
            "瞬态二筛导入不能创建旧任务行"
        );
        assert_eq!(
            decode_video_stage2_changes(&store.pull_changes(before, 100).unwrap().changes),
            vec![DecodedVideoStage2 {
                key: content,
                slot: 0,
                phash_parts: encoded_stage2(&remote_stage2()).0,
                sobel: encoded_stage2(&remote_stage2()).1,
            }]
        );
    }

    #[tokio::test]
    async fn remote_stage2_import_and_selective_republish_are_each_once() {
        let mut store = test_store();
        let (content, source, _) = seed_video(&mut store, [2; 16], &[0], 200);
        let before = store.outbox_high_seq().unwrap();
        let plan = begin_stage2_batch(
            &mut store,
            &[Stage2BatchItem {
                content,
                source,
                frame_slots: vec![0, 1],
            }],
            201,
        )
        .unwrap();
        let mut remote = crate::NodeRemoteFeatureCache::test_with_stage2(vec![Some(
            CompleteStage2::Video(Box::new(full_stage2_frames())),
        )]);
        let mut processor = CountingStage2::default();

        let _runtime_id = run_stage2_batch_internal(
            &mut store,
            plan,
            &mut processor,
            None,
            Some(&mut remote),
            None,
            202,
        )
        .await
        .unwrap();

        assert_eq!(processor.calls, 0);
        assert!(
            store.page_tasks(None, 20).unwrap().items.is_empty(),
            "瞬态二筛重发不能创建旧任务行"
        );
        let mut changes =
            decode_video_stage2_changes(&store.pull_changes(before, 100).unwrap().changes);
        changes.sort_by_key(|change| change.slot);
        assert_eq!(changes.len(), 2);
        assert_eq!(
            changes[0],
            DecodedVideoStage2 {
                key: content,
                slot: 0,
                phash_parts: encoded_stage2(&local_stage2()).0,
                sobel: encoded_stage2(&local_stage2()).1,
            }
        );
        assert_eq!(
            changes[1],
            DecodedVideoStage2 {
                key: content,
                slot: 1,
                phash_parts: encoded_stage2(&remote_stage2()).0,
                sobel: encoded_stage2(&remote_stage2()).1,
            }
        );
    }

    #[derive(Default)]
    struct CountingStage2 {
        calls: usize,
    }

    impl Stage2Processor for CountingStage2 {
        async fn process(&mut self, _request: Stage2Request) -> Result<Stage2Output, String> {
            self.calls += 1;
            Ok(Stage2Output {
                frames: Vec::new(),
                regenerated_contact_sheet_jpeg: None,
            })
        }
    }

    fn test_store() -> NodeStore {
        NodeStore::open_in_memory(MachineId::parse(&"ab".repeat(32)).unwrap()).unwrap()
    }

    /// 为生产编排测试提供稳定的物理盘解析结果，并记录全部解析次数。
    #[derive(Clone)]
    struct FixedLaneResolver {
        /// 测试期望的全部来源解析次数。
        expected_calls: usize,
        /// 生产编排实际请求解析器的次数。
        calls: Arc<AtomicUsize>,
    }

    impl FixedLaneResolver {
        /// 按传入数量创建测试解析器；C/D 路径会冻结到不同的物理盘 lane。
        fn new(expected_calls: usize) -> Self {
            Self {
                expected_calls,
                calls: Arc::new(AtomicUsize::new(0)),
            }
        }

        /// 返回当前已经完成的来源物理盘解析数量。
        fn calls(&self) -> usize {
            self.calls.load(Ordering::SeqCst)
        }
    }

    impl ScanRootStorageResolver for FixedLaneResolver {
        fn resolve(&self, root: &std::path::Path) -> std::io::Result<ResolvedScanRootStorage> {
            let call = self.calls.fetch_add(1, Ordering::SeqCst) + 1;
            assert!(
                call <= self.expected_calls,
                "生产编排不能为同一批次重复解析物理盘"
            );
            let disk = if root
                .to_string_lossy()
                .to_ascii_uppercase()
                .starts_with("D:")
            {
                2
            } else {
                1
            };
            Ok(ResolvedScanRootStorage {
                normalized_root: NormalizedPath::new(root).map_err(std::io::Error::other)?,
                physical_disk_id: PhysicalDiskId::from_disk_numbers(vec![disk])
                    .map_err(std::io::Error::other)?,
                disk_kind: LocalDiskKind::Ssd,
            })
        }
    }

    /// 写入一张具备完整基础特征的图片；按参数选择是否预置完整二筛缓存。
    fn seed_image(
        store: &mut NodeStore,
        md5: [u8; 16],
        size: u64,
        with_stage2: bool,
    ) -> (ContentKey, LocationKey, ContentId) {
        seed_image_at(
            store,
            &format!(r"C:\\phase2-{}.jpg", md5[0]),
            md5,
            size,
            with_stage2,
        )
    }

    /// 把测试图片固定到指定盘符，以验证编排保留预先冻结的物理盘 lane。
    fn seed_image_at(
        store: &mut NodeStore,
        path: &str,
        md5: [u8; 16],
        size: u64,
        with_stage2: bool,
    ) -> (ContentKey, LocationKey, ContentId) {
        let scanned = ScannedPath::new(
            NormalizedPath::new(path).unwrap(),
            DisplayPath::new(path).unwrap(),
            size,
        );
        let content = store
            .upsert_content_and_location(&scanned, md5, MediaKind::Image)
            .unwrap();
        store
            .commit_feature_result(
                content.id,
                None,
                FeatureWrite::ImageStage1(ImageStage1Fields::from(stage1())),
            )
            .unwrap();
        if with_stage2 {
            store
                .commit_feature_result(content.id, None, FeatureWrite::ImageStage2(local_stage2()))
                .unwrap();
        }
        store.mark_base_complete(content.id).unwrap();
        let location = LocationKey::new(store.machine_id().clone(), scanned.normalized_path);
        (content.key, location, content.id)
    }

    #[cfg(feature = "test-hooks")]
    /// 生成一个图片或单槽视频都可消费的受控 Worker 二筛成功结果。
    fn stage2_output(seed: u64) -> Stage2Output {
        Stage2Output {
            frames: vec![Stage2Frame {
                slot: 0,
                feature: Some(ImageStage2 {
                    phash_parts: [seed; 9],
                    sobel: [seed as f32; 128],
                }),
                error: None,
            }],
            regenerated_contact_sheet_jpeg: None,
        }
    }

    fn seed_video(
        store: &mut NodeStore,
        md5: [u8; 16],
        stage2_slots: &[u8],
        size: u64,
    ) -> (ContentKey, LocationKey, ContentId) {
        let path = format!(r"D:\remote-{}.mp4", md5[0]);
        let scanned = ScannedPath::new(
            NormalizedPath::new(&path).unwrap(),
            DisplayPath::new(&path).unwrap(),
            size,
        );
        let content = store
            .upsert_content_and_location(&scanned, md5, MediaKind::Video)
            .unwrap();
        store
            .commit_feature_result(
                content.id,
                None,
                FeatureWrite::VideoMetadata(VideoMetadataFields {
                    duration_ms: Some(12_000),
                    width: Some(100),
                    height: Some(100),
                }),
            )
            .unwrap();
        for slot in 0..6 {
            let feature = stage1();
            store
                .commit_feature_result(
                    content.id,
                    None,
                    FeatureWrite::VideoFrameStage1(VideoFrameStage1Fields {
                        slot,
                        time_ms: u64::from(slot) * 2_000 + 1_000,
                        decoded: true,
                        width: Some(feature.width),
                        height: Some(feature.height),
                        pdq: Some(feature.pdq),
                        quality: Some(feature.quality),
                    }),
                )
                .unwrap();
            if stage2_slots.contains(&slot) {
                store
                    .commit_feature_result(
                        content.id,
                        None,
                        FeatureWrite::VideoFrameStage2(VideoFrameStage2Fields {
                            slot,
                            features: local_stage2(),
                        }),
                    )
                    .unwrap();
            }
        }
        store.mark_base_complete(content.id).unwrap();
        let location = LocationKey::new(store.machine_id().clone(), scanned.normalized_path);
        (content.key, location, content.id)
    }

    fn full_stage2_frames() -> [Option<ImageStage2>; 6] {
        std::array::from_fn(|_| Some(remote_stage2()))
    }

    fn stage1() -> ImageStage1 {
        ImageStage1 {
            width: 100,
            height: 100,
            pdq: PdqHash::from_bytes([0; 32]),
            quality: 100,
        }
    }

    fn local_stage2() -> ImageStage2 {
        ImageStage2 {
            phash_parts: [0x1111_1111_1111_1111; 9],
            sobel: [0.25; 128],
        }
    }

    fn remote_stage2() -> ImageStage2 {
        ImageStage2 {
            phash_parts: [0x2222_2222_2222_2222; 9],
            sobel: [0.5; 128],
        }
    }

    fn encoded_stage2(features: &ImageStage2) -> (Vec<u8>, Vec<u8>) {
        let phash_parts = features
            .phash_parts
            .iter()
            .flat_map(|part| part.to_le_bytes())
            .collect();
        let sobel = features
            .sobel
            .iter()
            .flat_map(|value| value.to_le_bytes())
            .collect();
        (phash_parts, sobel)
    }

    #[derive(Debug, Eq, PartialEq)]
    struct DecodedVideoStage2 {
        key: ContentKey,
        slot: u8,
        phash_parts: Vec<u8>,
        sobel: Vec<u8>,
    }

    fn decode_video_stage2_changes(changes: &[proto::SyncChange]) -> Vec<DecodedVideoStage2> {
        changes
            .iter()
            .filter(|change| change.entity_kind == "video_frame_stage2")
            .map(decode_video_stage2_change)
            .collect()
    }

    fn decode_video_stage2_change(change: &proto::SyncChange) -> DecodedVideoStage2 {
        assert_eq!(change.entity_kind, "video_frame_stage2");
        assert_eq!(change.payload.len(), 622);
        let mut reader = PayloadReader::new(&change.payload);
        assert_eq!(reader.u8(), 1);
        let md5 = reader.bytes();
        assert_eq!(md5.len(), 16);
        let key = ContentKey::new(md5.try_into().unwrap(), reader.u64());
        let slot = reader.u8();
        let phash_parts = reader.bytes();
        let sobel = reader.bytes();
        assert_eq!(phash_parts.len(), 72);
        assert_eq!(sobel.len(), 512);
        assert_eq!(reader.remaining(), 0);
        DecodedVideoStage2 {
            key,
            slot,
            phash_parts,
            sobel,
        }
    }

    struct PayloadReader<'a> {
        bytes: &'a [u8],
        offset: usize,
    }

    impl<'a> PayloadReader<'a> {
        fn new(bytes: &'a [u8]) -> Self {
            Self { bytes, offset: 0 }
        }

        fn u8(&mut self) -> u8 {
            let value = *self.bytes.get(self.offset).unwrap();
            self.offset += 1;
            value
        }

        fn u64(&mut self) -> u64 {
            let end = self.offset + 8;
            let value = u64::from_be_bytes(self.bytes[self.offset..end].try_into().unwrap());
            self.offset = end;
            value
        }

        fn bytes(&mut self) -> Vec<u8> {
            let end = self.offset + 4;
            let length =
                u32::from_be_bytes(self.bytes[self.offset..end].try_into().unwrap()) as usize;
            self.offset = end;
            let end = self.offset + length;
            let value = self.bytes[self.offset..end].to_vec();
            self.offset = end;
            value
        }

        fn remaining(&self) -> usize {
            self.bytes.len() - self.offset
        }
    }
}
