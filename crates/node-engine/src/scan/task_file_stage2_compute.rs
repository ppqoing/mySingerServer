//! 瞬态任务文件的联合二筛执行边界。
//!
//! 本模块把已经由缓存查询确认的二筛缺口写入按物理盘划分的 TSV，随后通过同一个
//! `TaskFileDispatcher` 和 `ScheduledFileReader` 取得读取许可。许可一直保留到匹配的
//! `Stage2SourceReadComplete`，SQLite 写入也只由单写 actor 完成并在 ACK 后确认 TSV。

use std::{
    collections::{BTreeMap, BTreeSet},
    fmt,
    path::{Path, PathBuf},
    time::{SystemTime, UNIX_EPOCH},
};

use dedup_core::{ContentKey, DisplayPath, MediaKind};
use dedup_node_store::{
    BaseCacheRecord, FeatureWrite, FileFaultKind, FileFaultRecord, VideoFrameStage2Fields,
    classify_cache_completeness,
};
use dedup_protocol::{proto, proto::worker_envelope};
use dedup_windows::{LocalDiskKind, ReadCancellationToken};
use thiserror::Error;
use tokio::{sync::mpsc::UnboundedReceiver, time::Duration};
use uuid::Uuid;

use super::{PlannedScannedPath, TaskDiskLane};
use crate::{
    io::{DiskReadClass, ReadFailure},
    scan::base_persistence::{
        BasePersistAck, BasePersistIdentity, BasePersistMessage, BasePersistOutcome,
        BasePersistSendError, BaseStoreHandle,
    },
    task_dispatch::{
        DispatchedTask, TaskDispatchAdmission, TaskDispatchError, TaskDispatchPoll,
        TaskFileDispatcher, TaskLanePermitProvider,
    },
    task_files::{
        TaskFileIdentity, TaskFileRecord, TaskWorkKind, TaskWorkMask, TransientTaskFileSet,
    },
    worker::{Stage2Output, WorkerEvent, WorkerFileIdentity, WorkerPool, decode_stage2_payload},
};

/// 一个已完成缓存归并的二筛输入；只携带本地可写入的内容身份。
#[derive(Clone, Debug)]
pub(crate) struct Stage2TaskInput {
    /// 枚举阶段冻结的路径和物理盘 lane。
    pub(crate) planned: PlannedScannedPath,
    /// 已由 SQLite 或中心缓存验证的基础快照。
    pub(crate) cached: BaseCacheRecord,
    /// 视频可复用的本地联系表；不存在时由 Worker 回退原视频。
    pub(crate) contact_sheet_path: Option<PathBuf>,
}

/// 二筛任务文件生产失败；失败前不会留下部分可执行的输入映射。
#[derive(Debug, Error)]
pub(crate) enum Stage2TaskProducerError {
    /// 缓存快照、内容身份或任务掩码不满足二筛契约。
    #[error("二筛任务输入无效: {0}")]
    InvalidInput(String),
    /// 瞬态任务文件无法创建、追加或封闭。
    #[error("二筛任务文件失败: {0}")]
    Io(#[from] std::io::Error),
}

/// 二筛任务文件行的完整内存上下文；不会写入任务 TSV。
#[derive(Clone, Debug, PartialEq)]
struct Stage2TaskContext {
    /// 本行冻结的物理盘 lane。
    lane: TaskDiskLane,
    /// 本行必须写入的内容键。
    content: ContentKey,
    /// 本机 SQLite 内容自增 ID。
    content_id: dedup_node_store::ContentId,
    /// 已由基础阶段确认的实际媒体类型。
    media_kind: MediaKind,
    /// 视频二筛需要计算的固定槽位；图片使用空数组表示 Worker 默认槽位 0。
    frame_slots: Vec<u8>,
    /// 视频联系表候选路径。
    contact_sheet_path: Option<PathBuf>,
    /// 真实读取和诊断使用的显示路径。
    display_path: DisplayPath,
}

/// 已封闭、可交给二筛执行器的瞬态任务文件集合。
pub(crate) struct Stage2TaskProduction<P: TaskLanePermitProvider> {
    /// 按物理盘 lane 派发并持有读取许可的唯一 dispatcher。
    pub(crate) dispatcher: TaskFileDispatcher<P>,
    /// 任务文件身份到 SQLite/Worker 上下文的精确映射。
    contexts: BTreeMap<TaskFileIdentity, Stage2TaskContext>,
}

impl<P: TaskLanePermitProvider> Stage2TaskProduction<P> {
    /// 删除本轮已完成或已失败的任务文件目录。
    pub(crate) fn discard(&mut self) -> Result<(), std::io::Error> {
        self.dispatcher.discard()
    }
}

/// 构造二筛任务文件；只有真实缺失的图片字段或视频槽位才会追加 P 行。
pub(crate) fn build_stage2_task_production<P>(
    runtime_root: &Path,
    run_id: impl ToString,
    provider: P,
    inputs: &[Stage2TaskInput],
) -> Result<Stage2TaskProduction<P>, Stage2TaskProducerError>
where
    P: TaskLanePermitProvider,
{
    let run_id = run_id.to_string();
    let files = TransientTaskFileSet::create(runtime_root, &run_id)?;
    let mut dispatcher = TaskFileDispatcher::new(files, provider);
    let mut grouped =
        BTreeMap::<String, (TaskDiskLane, Vec<TaskFileRecord>, Vec<Stage2TaskContext>)>::new();

    for input in inputs {
        let planned = &input.planned;
        let cached = &input.cached;
        if cached.content_key.file_size() != planned.scanned.file_size {
            return Err(Stage2TaskProducerError::InvalidInput(format!(
                "路径 {} 的二筛缓存大小与枚举结果不一致",
                planned.scanned.normalized_path
            )));
        }
        let contact_sheet_valid = input
            .contact_sheet_path
            .as_deref()
            .is_some_and(dedup_node_engine_contact_sheet_valid);
        let completeness = classify_cache_completeness(cached, contact_sheet_valid);

        // 基础字段未完整时，二筛没有合法输入；该文件留给基础阶段，不在此处伪造 P。
        if completeness.base_missing_parts != 0 {
            continue;
        }
        let (work_kind, missing, frame_slots) = match cached.media_kind {
            MediaKind::Image if completeness.image_stage2_missing => (
                TaskWorkKind::ImageStage2,
                TaskWorkMask::for_image_stage2(),
                Vec::new(),
            ),
            MediaKind::Image => continue,
            MediaKind::Video if completeness.video_stage2_missing_slots != 0 => {
                let slots = completeness.video_stage2_missing_slots;
                let mask = TaskWorkMask::for_video_stage2(slots).ok_or_else(|| {
                    Stage2TaskProducerError::InvalidInput(format!(
                        "路径 {} 的视频二筛槽位掩码无效",
                        planned.scanned.normalized_path
                    ))
                })?;
                (
                    TaskWorkKind::VideoStage2,
                    mask,
                    (0..6).filter(|slot| slots & (1_u8 << slot) != 0).collect(),
                )
            }
            MediaKind::Video => continue,
            MediaKind::Other => continue,
        };
        let content_id = cached.content_id.ok_or_else(|| {
            Stage2TaskProducerError::InvalidInput(format!(
                "路径 {} 的二筛缓存没有本地 content_id",
                planned.scanned.normalized_path
            ))
        })?;

        let row = TaskFileRecord {
            item_id: Uuid::now_v7(),
            work_kind,
            scanned: planned.scanned.clone(),
            known_md5: Some(cached.content_key.md5()),
            missing,
        };
        let context = Stage2TaskContext {
            lane: planned.lane.clone(),
            content: cached.content_key,
            content_id,
            media_kind: cached.media_kind,
            frame_slots,
            contact_sheet_path: input.contact_sheet_path.clone(),
            display_path: planned.scanned.display_path.clone(),
        };
        let key = stage2_lane_key(&planned.lane);
        let entry = grouped
            .entry(key)
            .or_insert_with(|| (planned.lane.clone(), Vec::new(), Vec::new()));
        if entry.0 != planned.lane {
            return Err(Stage2TaskProducerError::InvalidInput(format!(
                "二筛任务 lane 配置冲突: {}",
                planned.scanned.normalized_path
            )));
        }
        entry.1.push(row);
        entry.2.push(context);
    }

    let mut contexts = BTreeMap::new();
    for (_, (lane, rows, row_contexts)) in grouped {
        let identities = dispatcher.append_batch(&lane, &rows)?;
        if identities.len() != row_contexts.len() {
            return Err(Stage2TaskProducerError::InvalidInput(
                "二筛任务身份和上下文数量不一致".into(),
            ));
        }
        for (identity, context) in identities.into_iter().zip(row_contexts) {
            contexts.insert(identity, context);
        }
    }
    dispatcher.seal()?;
    Ok(Stage2TaskProduction {
        dispatcher,
        contexts,
    })
}

/// 二筛执行的统计结果；调用方可对返回的 production 执行 discard。
pub(crate) struct Stage2TaskRunResult<P: TaskLanePermitProvider> {
    /// 所有任务文件终态已经由 ACK 确认的生产集合。
    pub(crate) production: Stage2TaskProduction<P>,
    /// SQLite ACK 成功并迁移为 C 的行数。
    pub(crate) completed: usize,
    /// Worker 失败、协议结果失败并迁移为 F 的行数。
    pub(crate) failed: usize,
}

/// 二筛任务级基础设施错误；未处理的 TSV 行仍保持 P。
pub(crate) struct Stage2TaskComputeError<P: TaskLanePermitProvider> {
    /// 面向上层任务记录的诊断。
    message: String,
    /// 清理在途 owner 后仍拥有 P 行的生产集合。
    production: Stage2TaskProduction<P>,
}

impl<P: TaskLanePermitProvider> Stage2TaskComputeError<P> {
    /// 取回未完成任务文件，供调用方记录并删除。
    pub(crate) fn into_production(self) -> Stage2TaskProduction<P> {
        self.production
    }
}

impl<P: TaskLanePermitProvider> fmt::Display for Stage2TaskComputeError<P> {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(&self.message)
    }
}

impl<P: TaskLanePermitProvider> fmt::Debug for Stage2TaskComputeError<P> {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("Stage2TaskComputeError")
            .field("message", &self.message)
            .finish_non_exhaustive()
    }
}

/// 一个已派发二筛项的读取许可和 Worker 事件状态。
struct ActiveStage2<Permit> {
    /// 任务文件的完整身份。
    identity: TaskFileIdentity,
    /// 已校验的任务文件记录。
    record: TaskFileRecord,
    /// 上游缓存快照和 Worker 请求参数。
    context: Stage2TaskContext,
    /// dispatcher 返回的唯一读取许可。
    permit: Option<Permit>,
    /// WorkerPool Started 确认的槽位。
    worker_slot: Option<u32>,
    /// Worker 已发送源读取完成事件。
    source_read_complete: bool,
    /// dispatch 时冻结的文件身份。
    worker_identity: WorkerFileIdentity,
}

/// Worker 终态对应的 SQLite 操作类型。
enum Stage2Terminal {
    /// 已验证的二筛特征，等待 SQLite ACK。
    Complete {
        /// 要原子提交的二筛特征。
        writes: Vec<FeatureWrite>,
    },
    /// 当前项失败，ACK 后把任务文件迁移为 F。
    Failed {
        /// 面向日志和故障记录的失败说明。
        message: String,
        /// 是否为 Worker 崩溃故障。
        fault: Option<FileFaultRecord>,
    },
}

/// 运行已经封闭的二筛 TSV；只用 ACK 驱动 C/F，任务表完全不参与。
pub(crate) async fn run_task_file_stage2<P>(
    mut production: Stage2TaskProduction<P>,
    worker_pool: &mut WorkerPool,
    store: &BaseStoreHandle,
    acknowledgements: &mut UnboundedReceiver<BasePersistAck>,
    worker_capacity: usize,
    cancellation: ReadCancellationToken,
) -> Result<Stage2TaskRunResult<P>, Stage2TaskComputeError<P>>
where
    P: TaskLanePermitProvider,
{
    if worker_capacity == 0 {
        return Err(stage2_error(production, "二筛 Worker 容量必须大于 0"));
    }

    let mut active = BTreeMap::<TaskFileIdentity, ActiveStage2<P::Permit>>::new();
    let mut dispatch_drained = false;
    let mut completed = 0;
    let mut failed = 0;

    loop {
        if dispatch_drained && active.is_empty() {
            break;
        }
        if cancellation.is_cancelled() {
            cleanup_stage2_ownership(&mut production, worker_pool, &cancellation, &mut active)
                .await;
            return Err(stage2_error(production, "二筛已取消"));
        }

        let can_dispatch = !dispatch_drained && active.len() < worker_capacity;
        if !can_dispatch && active.is_empty() {
            // 当前没有活动 Worker，但 dispatcher 尚未返回 Drained；再次轮询可得到明确状态。
            dispatch_drained = false;
        }

        tokio::select! {
            biased;
            event = worker_pool.next_event(), if !active.is_empty() => {
                let Some(event) = event else {
                    cleanup_stage2_ownership(&mut production, worker_pool, &cancellation, &mut active).await;
                    return Err(stage2_error(production, "二筛 WorkerPool 已关闭"));
                };
                match stage2_event(
                    event,
                    &mut active,
                    &mut production,
                    store,
                    acknowledgements,
                ).await {
                    Ok(Some(true)) => completed += 1,
                    Ok(Some(false)) => failed += 1,
                    Ok(None) => {}
                    Err(message) => {
                        cleanup_stage2_ownership(&mut production, worker_pool, &cancellation, &mut active).await;
                        return Err(stage2_error(production, message));
                    }
                }
            }
            dispatched = production.dispatcher.next_with_admission(
                cancellation.clone(),
                TaskDispatchAdmission::media_only(),
            ), if can_dispatch => {
                match dispatched {
                    Ok(TaskDispatchPoll::Task(task)) => {
                        let identity = task.identity.clone();
                        match start_stage2_task(
                            &production,
                            task,
                            worker_pool,
                            &cancellation,
                            store.machine_id(),
                        ).await {
                            Ok(active_task) => {
                                active.insert(identity, active_task);
                            }
                            Err(message) => {
                                let _ = production.dispatcher.abandon_in_flight(&identity);
                                cleanup_stage2_ownership(&mut production, worker_pool, &cancellation, &mut active).await;
                                return Err(stage2_error(production, message));
                            }
                        }
                    }
                    Ok(TaskDispatchPoll::Drained) => dispatch_drained = true,
                    Ok(TaskDispatchPoll::Blocked(reason)) => {
                        cleanup_stage2_ownership(&mut production, worker_pool, &cancellation, &mut active).await;
                        return Err(stage2_error(production, format!("二筛 dispatcher 被阻止: {reason:?}")));
                    }
                    Err(TaskDispatchError::Read(ReadFailure::Cancelled)) => {
                        cleanup_stage2_ownership(&mut production, worker_pool, &cancellation, &mut active).await;
                        return Err(stage2_error(production, "二筛读取已取消"));
                    }
                    Err(error) => {
                        cleanup_stage2_ownership(&mut production, worker_pool, &cancellation, &mut active).await;
                        return Err(stage2_error(production, format!("二筛 dispatcher 失败: {error}")));
                    }
                }
            }
            _ = tokio::time::sleep(Duration::from_millis(10)), if can_dispatch || !active.is_empty() => {}
        }
    }

    Ok(Stage2TaskRunResult {
        production,
        completed,
        failed,
    })
}

/// 把 dispatcher 交付的二筛任务发送给 Worker，并把许可放进 active owner。
async fn start_stage2_task<P>(
    production: &Stage2TaskProduction<P>,
    task: DispatchedTask<P::Permit>,
    worker_pool: &WorkerPool,
    cancellation: &ReadCancellationToken,
    machine_id: &dedup_core::MachineId,
) -> Result<ActiveStage2<P::Permit>, String>
where
    P: TaskLanePermitProvider,
{
    if task.class != DiskReadClass::MediaDecode
        || !matches!(
            task.record.work_kind,
            TaskWorkKind::ImageStage2 | TaskWorkKind::VideoStage2
        )
        || task.record.known_md5.is_none()
    {
        return Err("二筛 dispatcher 返回了无效任务行".into());
    }
    let Some(context) = production.contexts.get(&task.identity).cloned() else {
        return Err("二筛任务缺少精确上下文".into());
    };
    if task.record.known_md5 != Some(context.content.md5())
        || task.record.scanned.file_size != context.content.file_size()
    {
        return Err("二筛任务行与缓存内容身份不一致".into());
    }
    let worker_identity = WorkerFileIdentity {
        machine_id: machine_id.clone(),
        normalized_path: task.record.scanned.normalized_path.clone(),
        display_path: task.record.scanned.display_path.clone(),
        file_size: task.record.scanned.file_size,
        stage: "compute_stage2_features".into(),
        physical_disk_id: stage2_physical_disk_display(&context.lane),
    };
    worker_pool
        .dispatch_runtime(
            stage2_envelope(&task.identity, &context),
            worker_identity.clone(),
        )
        .await
        .map_err(|error| format!("二筛 Worker 派发失败: {error}"))?;
    if cancellation.is_cancelled() {
        return Err("二筛在 Worker 派发后被取消".into());
    }
    Ok(ActiveStage2 {
        identity: task.identity,
        record: task.record,
        context,
        permit: Some(task.permit),
        worker_slot: None,
        source_read_complete: false,
        worker_identity,
    })
}

/// 消费一条二筛 Worker 事件；终态时等待精确 SQLite ACK 后才迁移任务行。
async fn stage2_event<P>(
    event: WorkerEvent,
    active: &mut BTreeMap<TaskFileIdentity, ActiveStage2<P::Permit>>,
    production: &mut Stage2TaskProduction<P>,
    store: &BaseStoreHandle,
    acknowledgements: &mut UnboundedReceiver<BasePersistAck>,
) -> Result<Option<bool>, String>
where
    P: TaskLanePermitProvider,
{
    match event {
        WorkerEvent::Started {
            task_id,
            item_id,
            slot,
            identity,
            ..
        } => {
            let Some(key) = find_stage2_identity(active, &task_id, &item_id) else {
                return Ok(None);
            };
            let work = active
                .get_mut(&key)
                .ok_or_else(|| "二筛 Started 活动项消失".to_owned())?;
            if !same_worker_identity(&work.worker_identity, &identity) {
                return Err("二筛 Started 文件身份不匹配".into());
            }
            work.worker_slot = Some(slot);
            Ok(None)
        }
        WorkerEvent::Stage2SourceReadComplete {
            task_id,
            item_id,
            slot,
            ..
        } => {
            let Some(key) = find_stage2_identity(active, &task_id, &item_id) else {
                return Ok(None);
            };
            let work = active
                .get_mut(&key)
                .ok_or_else(|| "二筛源读取完成活动项消失".to_owned())?;
            if work.worker_slot != Some(slot) {
                return Err("二筛源读取完成 slot 不匹配".into());
            }
            work.source_read_complete = true;
            // 只有这个精确身份的源读取事件能释放 dispatcher 提供的 permit。
            drop(work.permit.take());
            Ok(None)
        }
        WorkerEvent::Completed {
            task_id,
            item_id,
            response,
        } => {
            let Some(key) = find_stage2_identity(active, &task_id, &item_id) else {
                return Ok(None);
            };
            let work = active
                .remove(&key)
                .ok_or_else(|| "二筛 Completed 活动项消失".to_owned())?;
            if !work.source_read_complete {
                let result = Stage2Terminal::Failed {
                    message: "Worker Completed 前未收到 Stage2SourceReadComplete".into(),
                    fault: None,
                };
                return persist_stage2_terminal_or_abandon(
                    production,
                    store,
                    acknowledgements,
                    &key,
                    work,
                    result,
                )
                .await
                .map(|_| Some(false));
            }
            let terminal = match decode_stage2_result(&work, response) {
                Ok(writes) => Stage2Terminal::Complete { writes },
                Err(message) => Stage2Terminal::Failed {
                    message,
                    fault: None,
                },
            };
            let success = matches!(&terminal, Stage2Terminal::Complete { .. });
            persist_stage2_terminal_or_abandon(
                production,
                store,
                acknowledgements,
                &key,
                work,
                terminal,
            )
            .await
            .map(|_| Some(success))
        }
        WorkerEvent::Crashed {
            task_id,
            item_id,
            identity,
            process_id,
            exit_code,
            message,
        } => {
            let Some(key) = find_stage2_identity(active, &task_id, &item_id) else {
                return Ok(None);
            };
            let matches_identity = active
                .get(&key)
                .map(|work| same_worker_identity(&work.worker_identity, &identity))
                .unwrap_or(false);
            if !matches_identity {
                return Err("二筛 Crashed 文件身份不匹配".into());
            }
            let work = active
                .remove(&key)
                .ok_or_else(|| "二筛 Crashed 活动项消失".to_owned())?;
            let fault = file_fault(&work, &message, process_id, exit_code);
            persist_stage2_terminal_or_abandon(
                production,
                store,
                acknowledgements,
                &key,
                work,
                Stage2Terminal::Failed {
                    message,
                    fault: Some(fault),
                },
            )
            .await
            .map(|_| Some(false))
        }
        WorkerEvent::Cancelled { task_id, item_id } => {
            let Some(key) = find_stage2_identity(active, &task_id, &item_id) else {
                return Ok(None);
            };
            let work = active
                .remove(&key)
                .ok_or_else(|| "二筛 Cancelled 活动项消失".to_owned())?;
            persist_stage2_terminal_or_abandon(
                production,
                store,
                acknowledgements,
                &key,
                work,
                Stage2Terminal::Failed {
                    message: "Worker 取消二筛任务".into(),
                    fault: None,
                },
            )
            .await
            .map(|_| Some(false))
        }
        WorkerEvent::InfrastructureFailure { message } => {
            Err(format!("二筛 Worker 基础设施失败: {message}"))
        }
        WorkerEvent::PhaseChanged { .. } | WorkerEvent::BaseSourceReadComplete { .. } => Ok(None),
    }
}

/// 持久化出现基础设施错误时归还当前 dispatcher 行，避免留下悬挂的在途身份。
async fn persist_stage2_terminal_or_abandon<P: TaskLanePermitProvider>(
    production: &mut Stage2TaskProduction<P>,
    store: &BaseStoreHandle,
    acknowledgements: &mut UnboundedReceiver<BasePersistAck>,
    identity: &TaskFileIdentity,
    work: ActiveStage2<P::Permit>,
    terminal: Stage2Terminal,
) -> Result<(), String> {
    let result = persist_stage2_terminal(production, store, acknowledgements, work, terminal).await;
    if result.is_err() {
        let _ = production.dispatcher.abandon_in_flight(identity);
    }
    result
}

/// 校验 Stage2Result 的任务身份、槽位和完整 feature，再转换为 SQLite FeatureWrite。
fn decode_stage2_result(
    work: &ActiveStage2<impl Send>,
    response: proto::WorkerEnvelope,
) -> Result<Vec<FeatureWrite>, String> {
    let Some(worker_envelope::Payload::Stage2Result(result)) = response.payload else {
        return Err("Worker 返回了非 Stage2Result 响应".into());
    };
    if result.task_id != work.identity.run_id() {
        return Err("Worker 二筛结果 task_id 不匹配".into());
    }
    if result.item_id != work.identity.item_id().to_string() {
        return Err("Worker 二筛结果 item_id 不匹配".into());
    }
    let output = decode_stage2_payload(&result.payload)
        .map_err(|error| format!("二筛结果解析失败: {error}"))?;
    stage2_writes(&work.context, &output)
}

/// 严格把 Worker 结果限制为任务行声明的图片或视频缺失槽位。
fn stage2_writes(
    context: &Stage2TaskContext,
    output: &Stage2Output,
) -> Result<Vec<FeatureWrite>, String> {
    let expected = match context.media_kind {
        MediaKind::Image => vec![0],
        MediaKind::Video => context.frame_slots.clone(),
        MediaKind::Other => return Err("Other 文件不能执行二筛".into()),
    };
    if output.frames.len() != expected.len() {
        return Err("二筛结果槽位数量与任务缺口不一致".into());
    }
    let mut seen = BTreeSet::new();
    let mut writes = Vec::with_capacity(expected.len());
    for frame in &output.frames {
        if !expected.contains(&frame.slot) || !seen.insert(frame.slot) {
            return Err("二筛结果包含未知或重复槽位".into());
        }
        let Some(feature) = frame.feature else {
            return Err(frame
                .error
                .as_deref()
                .filter(|message| !message.trim().is_empty())
                .unwrap_or("二筛槽位计算失败")
                .to_owned());
        };
        if frame.error.is_some() {
            return Err("二筛结果槽位同时包含 feature 和 error".into());
        }
        if context.media_kind == MediaKind::Image {
            writes.push(FeatureWrite::ImageStage2(feature));
        } else {
            writes.push(FeatureWrite::VideoFrameStage2(VideoFrameStage2Fields {
                slot: frame.slot,
                features: feature,
            }));
        }
    }
    if expected.iter().any(|slot| !seen.contains(slot)) {
        return Err("二筛结果缺少任务声明的槽位".into());
    }
    Ok(writes)
}

/// 在唯一单写 actor 中提交特征或故障，ACK 身份精确一致后迁移 TSV 状态。
async fn persist_stage2_terminal<P: TaskLanePermitProvider>(
    production: &mut Stage2TaskProduction<P>,
    store: &BaseStoreHandle,
    acknowledgements: &mut UnboundedReceiver<BasePersistAck>,
    work: ActiveStage2<P::Permit>,
    terminal: Stage2Terminal,
) -> Result<(), String> {
    let mut work = work;
    // 终态已经到达，读取许可不应跨越 SQLite 写入；正常路径在 source-complete 时已为 None。
    drop(work.permit.take());
    let identity = work.identity.clone();
    let display_path = work
        .record
        .scanned
        .display_path
        .as_path()
        .to_string_lossy()
        .into_owned();
    let worker_slot = work.worker_slot;
    let file_size = work.record.scanned.file_size;
    let (operation, expectation) = match terminal {
        Stage2Terminal::Complete { writes } => {
            let content_id = work.context.content_id;
            let media_kind = work.context.media_kind;
            let operation = BasePersistMessage::new_task_file(identity.clone(), move |store| {
                store
                    .commit_stage2_taskless(content_id, media_kind, writes)
                    .map(|_| BasePersistOutcome::Succeeded {
                        worker_slot,
                        cache_hit: false,
                        media_kind,
                        file_size,
                    })
                    .map_err(|error| error.to_string())
            });
            (
                operation,
                PersistExpectation::Complete {
                    media_kind,
                    file_size,
                    worker_slot,
                },
            )
        }
        Stage2Terminal::Failed { message, fault } => {
            let operation_path = display_path.clone();
            let operation_message = message.clone();
            let operation = BasePersistMessage::new_task_file(identity.clone(), move |store| {
                if let Some(fault) = fault {
                    store
                        .upsert_file_fault(&fault)
                        .map_err(|error| error.to_string())?;
                }
                Ok(BasePersistOutcome::Failed {
                    display_path: operation_path,
                    message: operation_message,
                    worker_slot,
                    skipped_incomplete: false,
                })
            });
            (
                operation,
                PersistExpectation::Failed {
                    display_path,
                    worker_slot,
                },
            )
        }
    };

    let mut message = operation;
    loop {
        match store.try_persist(message) {
            Ok(()) => break,
            Err(BasePersistSendError::Full(returned)) => {
                message = returned;
                tokio::task::yield_now().await;
            }
            Err(BasePersistSendError::Closed(_)) => return Err("二筛单写 actor 已关闭".into()),
        }
    }
    let ack = acknowledgements
        .recv()
        .await
        .ok_or_else(|| "二筛单写 actor 未返回 ACK".to_owned())?;
    let ack_identity = match ack.identity {
        BasePersistIdentity::TaskFile(value) => value,
        BasePersistIdentity::Legacy(_) => return Err("二筛收到旧任务表 ACK".into()),
    };
    if ack_identity != identity {
        return Err("二筛收到未知或错配的 SQLite ACK".into());
    }
    let outcome = ack
        .result
        .map_err(|error| format!("二筛 SQLite 提交失败: {error}"))?;
    match expectation {
        PersistExpectation::Complete {
            media_kind,
            file_size,
            worker_slot,
        } => {
            if !matches!(
                outcome,
                BasePersistOutcome::Succeeded {
                    worker_slot: actual_slot,
                    cache_hit: false,
                    media_kind: actual_kind,
                    file_size: actual_size,
                } if actual_slot == worker_slot && actual_kind == media_kind && actual_size == file_size
            ) {
                return Err("二筛成功 ACK 字段不匹配".into());
            }
            production
                .dispatcher
                .mark_completed(&identity)
                .map_err(|error| format!("二筛 C 状态写入失败: {error}"))?;
            production.contexts.remove(&identity);
        }
        PersistExpectation::Failed {
            display_path,
            worker_slot,
        } => {
            if !matches!(
                outcome,
                BasePersistOutcome::Failed {
                    display_path: actual_path,
                    worker_slot: actual_slot,
                    skipped_incomplete: false,
                    ..
                } if actual_path == display_path && actual_slot == worker_slot
            ) {
                return Err("二筛失败 ACK 字段不匹配".into());
            }
            production
                .dispatcher
                .mark_failed(&identity)
                .map_err(|error| format!("二筛 F 状态写入失败: {error}"))?;
            production.contexts.remove(&identity);
        }
    }
    Ok(())
}

/// SQLite 写入动作和 ACK 必须匹配的关键字段。
enum PersistExpectation {
    /// 提交二筛结果后期待 Succeeded ACK。
    Complete {
        media_kind: MediaKind,
        file_size: u64,
        worker_slot: Option<u32>,
    },
    /// 保存故障后期待 Failed ACK。
    Failed {
        display_path: String,
        worker_slot: Option<u32>,
    },
}

/// 取消或任务级错误时取消 Worker 并释放所有 permit，未处理行保持 P。
async fn cleanup_stage2_ownership<P: TaskLanePermitProvider>(
    production: &mut Stage2TaskProduction<P>,
    worker_pool: &WorkerPool,
    cancellation: &ReadCancellationToken,
    active: &mut BTreeMap<TaskFileIdentity, ActiveStage2<P::Permit>>,
) {
    cancellation.cancel();
    let _ = production
        .dispatcher
        .next_with_admission(cancellation.clone(), TaskDispatchAdmission::media_only())
        .await;
    if let Some(run_id) = active
        .keys()
        .next()
        .or_else(|| production.contexts.keys().next())
        .map(|identity| identity.run_id().to_owned())
    {
        let _ = worker_pool.cancel_task(&run_id).await;
    }
    for (identity, work) in std::mem::take(active) {
        drop(work.permit);
        let _ = production.dispatcher.abandon_in_flight(&identity);
    }
}

/// 构造携带未完成 production 的任务级错误。
fn stage2_error<P: TaskLanePermitProvider>(
    production: Stage2TaskProduction<P>,
    message: impl Into<String>,
) -> Stage2TaskComputeError<P> {
    Stage2TaskComputeError {
        message: message.into(),
        production,
    }
}

/// 按任务文件身份查找当前活动 Worker。
fn find_stage2_identity<Permit>(
    active: &BTreeMap<TaskFileIdentity, ActiveStage2<Permit>>,
    task_id: &str,
    item_id: &str,
) -> Option<TaskFileIdentity> {
    active
        .keys()
        .find(|identity| identity.run_id() == task_id && identity.item_id().to_string() == item_id)
        .cloned()
}

/// Started/Crashed 的文件身份比较；路径、大小、机器和物理盘必须完全一致。
fn same_worker_identity(expected: &WorkerFileIdentity, actual: &WorkerFileIdentity) -> bool {
    expected.machine_id == actual.machine_id
        && expected.normalized_path == actual.normalized_path
        && expected.display_path == actual.display_path
        && expected.file_size == actual.file_size
        && expected.stage == actual.stage
        && expected.physical_disk_id == actual.physical_disk_id
}

/// 创建二筛 Worker V4 请求；图片空槽位由 Worker 解释为槽位 0。
fn stage2_envelope(
    identity: &TaskFileIdentity,
    context: &Stage2TaskContext,
) -> proto::WorkerEnvelope {
    let (display_path, contact_sheet_path, generate_contact_sheet_if_missing) =
        match (context.media_kind, context.contact_sheet_path.as_ref()) {
            (MediaKind::Image, _) => (
                context
                    .display_path
                    .as_path()
                    .to_string_lossy()
                    .into_owned(),
                String::new(),
                false,
            ),
            (MediaKind::Video, Some(path)) if dedup_node_engine_contact_sheet_valid(path) => (
                context
                    .display_path
                    .as_path()
                    .to_string_lossy()
                    .into_owned(),
                path.to_string_lossy().into_owned(),
                true,
            ),
            (MediaKind::Video, Some(path)) => (
                context
                    .display_path
                    .as_path()
                    .to_string_lossy()
                    .into_owned(),
                path.to_string_lossy().into_owned(),
                true,
            ),
            (MediaKind::Video, None) | (MediaKind::Other, None) | (MediaKind::Other, Some(_)) => (
                context
                    .display_path
                    .as_path()
                    .to_string_lossy()
                    .into_owned(),
                String::new(),
                false,
            ),
        };
    proto::WorkerEnvelope {
        payload: Some(worker_envelope::Payload::ComputeStage2(
            proto::ComputeStage2 {
                task_id: identity.run_id().to_owned(),
                item_id: identity.item_id().to_string(),
                display_path,
                frame_slots: context
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

/// 生成稳定的 Worker 物理盘显示值。
fn stage2_physical_disk_display(lane: &TaskDiskLane) -> String {
    format!(
        "PhysicalDisk{}",
        lane.physical_disk_numbers
            .iter()
            .map(u32::to_string)
            .collect::<Vec<_>>()
            .join("+")
    )
}

/// 生成仅用于分组的 lane 键；实际文件名仍由 TransientTaskFileSet 校验。
fn stage2_lane_key(lane: &TaskDiskLane) -> String {
    let kind = match lane.disk_kind {
        LocalDiskKind::Hdd => "hdd",
        LocalDiskKind::Ssd => "ssd",
        LocalDiskKind::Unknown => "unknown",
    };
    format!(
        "{}-{kind}",
        lane.physical_disk_numbers
            .iter()
            .map(u32::to_string)
            .collect::<Vec<_>>()
            .join("+")
    )
}

/// 判断联系表路径是否为可复用的固定 JPEG。
fn dedup_node_engine_contact_sheet_valid(path: &Path) -> bool {
    crate::contact_sheet_cache::ContactSheetCacheEntry::is_valid_file(path)
}

/// 构造二筛 Worker 崩溃的稳定故障记录。
fn file_fault(
    work: &ActiveStage2<impl Send>,
    message: &str,
    process_id: Option<u32>,
    exit_code: Option<i32>,
) -> FileFaultRecord {
    let now = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis()
        .try_into()
        .unwrap_or(u64::MAX);
    FileFaultRecord {
        machine_id: work.worker_identity.machine_id.clone(),
        normalized_path: work.record.scanned.normalized_path.clone(),
        display_path: work.record.scanned.display_path.clone(),
        file_size: work.record.scanned.file_size,
        kind: FileFaultKind::WorkerCrash,
        stage: "stage2".into(),
        windows_error_code: None,
        read_offset: None,
        read_size: None,
        worker_pid: process_id,
        worker_exit_code: exit_code,
        first_seen_at_ms: now,
        last_seen_at_ms: now,
        occurrence_count: 1,
        message: message.into(),
    }
}

#[cfg(test)]
mod tests {
    use std::{
        path::Path,
        sync::{
            Arc,
            atomic::{AtomicIsize, Ordering},
        },
    };

    #[cfg(feature = "test-hooks")]
    use std::time::Duration;

    use dedup_core::{DisplayPath, MachineId, MediaKind, NormalizedPath};
    use dedup_media::{ImageStage1, ImageStage2, PdqHash};
    use dedup_node_store::{FeatureWrite, ImageStage1Fields, NodeStore, ScannedPath};
    use dedup_windows::{LocalDiskKind, PhysicalDiskId, ReadCancellationToken};

    use super::*;
    use crate::{io::DiskReadClass, task_dispatch::TaskLanePermitFuture};

    #[cfg(feature = "test-hooks")]
    use crate::{
        scan::base_persistence::BaseStoreActor,
        worker::{Stage2Frame, WorkerPool},
    };

    const RUN_ID: &str = "01900000-0000-7000-8000-000000000302";

    /// 记录当前仍由 dispatcher/Worker 持有的读取许可数量。
    #[derive(Clone)]
    struct TrackedPermit {
        live: Arc<AtomicIsize>,
    }

    impl Drop for TrackedPermit {
        fn drop(&mut self) {
            self.live.fetch_sub(1, Ordering::SeqCst);
        }
    }

    /// 不阻塞读取、只用 RAII 计数验证许可生命周期的测试 provider。
    #[derive(Clone)]
    struct TrackingProvider {
        live: Arc<AtomicIsize>,
    }

    impl TaskLanePermitProvider for TrackingProvider {
        type Permit = TrackedPermit;

        fn acquire(
            &self,
            _lane: TaskDiskLane,
            _class: DiskReadClass,
            _cancellation: ReadCancellationToken,
        ) -> TaskLanePermitFuture<Self::Permit> {
            let live = Arc::clone(&self.live);
            Box::pin(async move {
                live.fetch_add(1, Ordering::SeqCst);
                Ok(TrackedPermit { live })
            })
        }
    }

    fn lane(disk: u32) -> TaskDiskLane {
        TaskDiskLane {
            physical_disk_id: PhysicalDiskId::from_disk_numbers([disk]).unwrap(),
            physical_disk_numbers: vec![disk],
            disk_kind: LocalDiskKind::Hdd,
            configured_weight: 1,
            per_disk_limit: 1,
        }
    }

    fn scanned(path: &str, file_size: u64) -> ScannedPath {
        ScannedPath::new(
            NormalizedPath::new(path).unwrap(),
            DisplayPath::new(path).unwrap(),
            file_size,
        )
    }

    fn image_stage1() -> ImageStage1Fields {
        ImageStage1Fields::from(ImageStage1 {
            width: 2,
            height: 2,
            pdq: PdqHash::from_bytes([7; 32]),
            quality: 80,
        })
    }

    fn seed_image_cache(
        store: &mut NodeStore,
        path: &str,
        md5: [u8; 16],
        file_size: u64,
        stage2: Option<ImageStage2>,
    ) -> BaseCacheRecord {
        let scanned = scanned(path, file_size);
        let content = store
            .upsert_content_and_location(&scanned, md5, MediaKind::Other)
            .unwrap();
        store
            .commit_scan_stage1_taskless(
                content.id,
                MediaKind::Image,
                vec![FeatureWrite::ImageStage1(image_stage1())],
            )
            .unwrap();
        if let Some(features) = stage2 {
            store
                .commit_stage2_taskless(
                    content.id,
                    MediaKind::Image,
                    vec![FeatureWrite::ImageStage2(features)],
                )
                .unwrap();
        }
        store.load_base_cache_record(content.id).unwrap()
    }

    fn input(
        path: &str,
        file_size: u64,
        lane: TaskDiskLane,
        cached: BaseCacheRecord,
    ) -> Stage2TaskInput {
        Stage2TaskInput {
            planned: PlannedScannedPath {
                scanned: scanned(path, file_size),
                lane,
            },
            cached,
            contact_sheet_path: None,
        }
    }

    #[cfg(feature = "test-hooks")]
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

    fn statuses(path: &Path) -> Vec<u8> {
        std::fs::read(path)
            .unwrap()
            .split(|byte| *byte == b'\n')
            .filter(|line| !line.is_empty())
            .map(|line| line[0])
            .collect()
    }

    /// 只有基础完整且确实缺二筛的内容才生成 P 行，完整二筛命中不进入 TSV。
    #[test]
    fn production_contains_only_missing_stage2_items() {
        let root = tempfile::tempdir().unwrap();
        let machine = MachineId::from_sha256([0x71; 32]);
        let mut store = NodeStore::open_in_memory(machine).unwrap();
        let missing = seed_image_cache(&mut store, r"C:\stage2-missing.jpg", [0x11; 16], 11, None);
        let complete = seed_image_cache(
            &mut store,
            r"C:\stage2-complete.jpg",
            [0x12; 16],
            12,
            Some(ImageStage2 {
                phash_parts: [1; 9],
                sobel: [1.0; 128],
            }),
        );
        let disk = lane(7);
        let provider = TrackingProvider {
            live: Arc::new(AtomicIsize::new(0)),
        };
        let mut production = build_stage2_task_production(
            root.path(),
            RUN_ID,
            provider,
            &[
                input(r"C:\stage2-missing.jpg", 11, disk.clone(), missing),
                input(r"C:\stage2-complete.jpg", 12, disk.clone(), complete),
            ],
        )
        .unwrap();
        let path = production.dispatcher.lane_path(&disk).unwrap();
        assert_eq!(statuses(&path), vec![b'P']);
        assert_eq!(production.contexts.len(), 1);
        production.discard().unwrap();
    }

    #[cfg(feature = "test-hooks")]
    #[tokio::test]
    /// 源读取完成前保持 permit，匹配事件释放后，SQLite ACK 前仍为 P，ACK 后才为 C。
    async fn permit_and_tsv_status_follow_source_and_persist_ack_boundaries() {
        let root = tempfile::tempdir().unwrap();
        let machine = MachineId::from_sha256([0x72; 32]);
        let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
        let cached = seed_image_cache(&mut store, r"C:\stage2-ack.jpg", [0x21; 16], 21, None);
        let disk = lane(8);
        let live = Arc::new(AtomicIsize::new(0));
        let provider = TrackingProvider {
            live: Arc::clone(&live),
        };
        let production = build_stage2_task_production(
            root.path(),
            RUN_ID,
            provider,
            &[input(r"C:\stage2-ack.jpg", 21, disk.clone(), cached)],
        )
        .unwrap();
        let lane_path = production.dispatcher.lane_path(&disk).unwrap();
        let content_id = production.contexts.values().next().unwrap().content_id;

        let (persist_control, persist_waiter) =
            crate::scan::base_persistence::BasePersistTestController::new();
        let (actor, handle, acknowledgements) =
            BaseStoreActor::spawn_with_first_persist_waiter(store, 2, persist_waiter);
        let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
        let mut acknowledgements = acknowledgements;
        let run = run_task_file_stage2(
            production,
            &mut pool,
            &handle,
            &mut acknowledgements,
            1,
            ReadCancellationToken::new(),
        );
        // Dispatcher 内的异步 permit future 只保证 Send，不保证 Sync；测试在同一任务内
        // 交错控制事件和 runner，避免把非 Sync 生产集合送入 tokio::spawn。
        let control = async {
            let (task_id, item_id) = started.recv().await.unwrap();
            assert_eq!(
                live.load(Ordering::SeqCst),
                1,
                "源读取完成前 permit 必须保持"
            );
            assert_eq!(statuses(&lane_path), vec![b'P']);

            controller
                .stage2_source_read_complete(task_id.clone(), item_id.clone())
                .await;
            tokio::time::timeout(Duration::from_secs(1), async {
                while live.load(Ordering::SeqCst) != 0 {
                    tokio::task::yield_now().await;
                }
            })
            .await
            .expect("匹配 Stage2SourceReadComplete 后必须释放 permit");

            controller
                .complete_stage2(task_id, item_id, stage2_output(3))
                .await;
            persist_control.wait_until_entered().await;
            assert_eq!(
                statuses(&lane_path),
                vec![b'P'],
                "SQLite ACK 前不能迁移为 C"
            );
            persist_control.release();
        };
        let (result, ()) =
            tokio::time::timeout(Duration::from_secs(2), async { tokio::join!(run, control) })
                .await
                .expect("runner 与控制事件必须在有限时间内收敛");
        let shutdown = pool.shutdown().await;
        assert!(shutdown.is_ok());
        let mut result = result.unwrap();
        assert_eq!(result.completed, 1);
        assert_eq!(result.failed, 0);
        assert_eq!(statuses(&lane_path), vec![b'C']);
        result.production.discard().unwrap();
        drop(handle);
        let store = actor.finish().await.unwrap();
        assert!(store.load_complete_stage2(content_id).unwrap().is_some());
    }

    #[cfg(feature = "test-hooks")]
    #[tokio::test]
    /// 单项 Worker 崩溃只把当前行写成 F，后续 P 行仍继续执行并在 ACK 后写成 C。
    async fn crashed_item_becomes_failed_and_next_item_continues() {
        let root = tempfile::tempdir().unwrap();
        let machine = MachineId::from_sha256([0x73; 32]);
        let mut store = NodeStore::open_in_memory(machine).unwrap();
        let first = seed_image_cache(&mut store, r"C:\stage2-failed.jpg", [0x31; 16], 31, None);
        let second = seed_image_cache(&mut store, r"C:\stage2-next.jpg", [0x32; 16], 32, None);
        let disk = lane(9);
        let live = Arc::new(AtomicIsize::new(0));
        let production = build_stage2_task_production(
            root.path(),
            RUN_ID,
            TrackingProvider {
                live: Arc::clone(&live),
            },
            &[
                input(r"C:\stage2-failed.jpg", 31, disk.clone(), first),
                input(r"C:\stage2-next.jpg", 32, disk.clone(), second),
            ],
        )
        .unwrap();
        let lane_path = production.dispatcher.lane_path(&disk).unwrap();
        let (actor, handle, acknowledgements) = BaseStoreActor::spawn(store, 2);
        let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
        let mut acknowledgements = acknowledgements;
        let run = run_task_file_stage2(
            production,
            &mut pool,
            &handle,
            &mut acknowledgements,
            1,
            ReadCancellationToken::new(),
        );
        let control = async {
            let (first_task, first_item) = started.recv().await.unwrap();
            assert_eq!(live.load(Ordering::SeqCst), 1);
            controller
                .crash(first_task, first_item, "测试二筛 Worker 崩溃".into())
                .await;

            let (second_task, second_item) = started.recv().await.unwrap();
            controller
                .stage2_source_read_complete(second_task.clone(), second_item.clone())
                .await;
            controller
                .complete_stage2(second_task, second_item, stage2_output(4))
                .await;
        };
        let (result, ()) =
            tokio::time::timeout(Duration::from_secs(2), async { tokio::join!(run, control) })
                .await
                .expect("失败项和后续项必须在有限时间内收敛");
        let shutdown = pool.shutdown().await;
        assert!(shutdown.is_ok());
        let mut result = result.unwrap();
        assert_eq!(result.completed, 1);
        assert_eq!(result.failed, 1);
        assert_eq!(statuses(&lane_path), vec![b'F', b'C']);
        assert_eq!(live.load(Ordering::SeqCst), 0);
        result.production.discard().unwrap();
        drop(handle);
        actor.finish().await.unwrap();
    }
}
