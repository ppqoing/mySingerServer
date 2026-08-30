//! 瞬态任务文件的 Media Worker 派发和资源事件边界。
//!
//! 本模块只负责把已知 MD5 的基础任务行交给 WorkerPool，并保留磁盘许可到
//! `BaseSourceReadComplete`。结果和失败继续由后续 taskless 持久化阶段处理。

use std::{
    collections::{BTreeMap, BTreeSet},
    fmt,
    time::Duration,
};

use dedup_core::{ContentKey, DiskReadConfig, MediaKind};
use dedup_protocol::{proto, proto::worker_envelope};
use dedup_windows::ReadCancellationToken;
use tokio::time::sleep;

use super::{TaskFileBaseContext, task_file_base_compute::TaskFileBaseComputePending};
use crate::{
    io::{DiskReadClass, ReadFailure},
    runtime_tasks::{RuntimeFailureUpdate, RuntimeStage, RuntimeTaskReporter, RuntimeWorkerUpdate},
    scan::base_persistence::BaseStoreHandle,
    task_dispatch::{
        TaskDispatchAdmission, TaskDispatchBlockReason, TaskDispatchError, TaskDispatchPoll,
        TaskLanePermitProvider,
    },
    task_files::{TaskFileIdentity, TaskFileRecord, TaskWorkKind},
    worker::{WorkerEvent, WorkerFileIdentity, WorkerPool},
};

/// 一个已派发媒体项的资源和事件状态；permit 只在源文件读取结束后释放。
struct ActiveMedia<P: TaskLanePermitProvider> {
    /// 任务文件精确身份，终态结果必须原样带回。
    identity: TaskFileIdentity,
    /// 已知 MD5 的媒体任务行。
    record: TaskFileRecord,
    /// 该行在 Hash 阶段冻结的上下文。
    context: TaskFileBaseContext,
    /// dispatcher 交付的唯一磁盘许可；SourceComplete 后变为 None。
    permit: Option<P::Permit>,
    /// WorkerPool Started 确认的逻辑槽位。
    worker_slot: Option<u32>,
    /// Worker 是否已经关闭源文件。
    source_read_complete: bool,
    /// 派发时冻结的完整 Worker 文件身份。
    worker_identity: WorkerFileIdentity,
}

/// Media Worker 正常完成后交给 taskless 持久化阶段的拥有型结果。
pub(crate) struct TaskFileMediaCompleted {
    /// 原始任务文件身份。
    pub(crate) identity: TaskFileIdentity,
    /// 原始媒体任务行。
    pub(crate) record: TaskFileRecord,
    /// Hash/缓存阶段冻结的上下文。
    pub(crate) context: TaskFileBaseContext,
    /// Worker 返回的协议载荷；由后续阶段校验并写入 SQLite。
    pub(crate) response: proto::WorkerEnvelope,
    /// WorkerPool 实际使用的槽位。
    pub(crate) worker_slot: Option<u32>,
}

/// Media Worker 单文件失败；本阶段只返回失败，不把 TSV 改成 F。
pub(crate) struct TaskFileMediaFailure {
    /// 原始任务文件身份。
    pub(crate) identity: TaskFileIdentity,
    /// 原始媒体任务行。
    pub(crate) record: TaskFileRecord,
    /// Hash/缓存阶段冻结的上下文。
    pub(crate) context: TaskFileBaseContext,
    /// 面向日志和持久化的失败说明。
    pub(crate) message: String,
    /// WorkerPool 观测到的槽位。
    pub(crate) worker_slot: Option<u32>,
}

/// 一条 Worker 终态；SQLite 写入与 TSV 状态迁移由持久化运行态继续处理。
pub(super) enum TaskFileMediaTerminal {
    /// Worker 返回了基础计算结果。
    Completed(TaskFileMediaCompleted),
    /// Worker 崩溃或违反事件顺序。
    Failed(TaskFileMediaFailure),
}

/// Media Worker 的可逐事件推进运行态；读取 permit 仅在源文件读取结束后释放。
pub(super) struct TaskFileMediaRuntime<P: TaskLanePermitProvider> {
    /// 已提交给 WorkerPool、尚未收到终态事件的媒体项。
    active: BTreeMap<TaskFileIdentity, ActiveMedia<P>>,
    /// 本轮已经收到终态的身份，取消时用于归还 dispatcher 的 in-flight 所有权。
    settled: BTreeSet<TaskFileIdentity>,
}

impl<P: TaskLanePermitProvider> TaskFileMediaRuntime<P> {
    /// 创建没有已派发 Worker 的空运行态。
    pub(super) fn new() -> Self {
        Self {
            active: BTreeMap::new(),
            settled: BTreeSet::new(),
        }
    }

    /// 返回当前 Worker 窗口是否还可以派发新的媒体项。
    pub(super) fn has_capacity(&self, worker_capacity: usize) -> bool {
        self.active.len() < worker_capacity
    }

    /// 返回是否仍有等待 Worker 事件的媒体项。
    pub(super) fn has_active(&self) -> bool {
        !self.active.is_empty()
    }

    /// 返回活动媒体项数，供单事件泵判断窗口占用。
    pub(super) fn active_len(&self) -> usize {
        self.active.len()
    }

    /// 把一个已领取的 Media 行转换为 Worker 请求并登记为活动项。
    pub(super) async fn dispatch(
        &mut self,
        pending: &mut TaskFileBaseComputePending<P>,
        task: crate::task_dispatch::DispatchedTask<P::Permit>,
        worker_pool: &mut WorkerPool,
        store: &BaseStoreHandle,
        read_config: &DiskReadConfig,
        cancellation: &ReadCancellationToken,
    ) -> Result<(), String> {
        let identity = task.identity.clone();
        let work =
            dispatch_media_task(pending, task, worker_pool, store, read_config, cancellation)
                .await?;
        if self.active.insert(identity, work).is_some() {
            return Err("Media 运行态收到重复活动任务身份".into());
        }
        Ok(())
    }

    /// 消费恰好一个 Worker 事件；Started 和 SourceReadComplete 不产生终态。
    pub(super) async fn handle_event(
        &mut self,
        event: WorkerEvent,
        reporter: Option<&RuntimeTaskReporter>,
    ) -> Result<Option<TaskFileMediaTerminal>, String> {
        if let Some(reporter) = reporter {
            report_media_event(&event, reporter, &self.active).await;
        }
        handle_media_event(event, &mut self.active, &mut self.settled)
    }

    /// 取消 Worker 并释放仍由活动项持有的 permit；调用方随后归还 dispatcher 所有权。
    pub(super) async fn cancel_and_drain(
        &mut self,
        worker_pool: &WorkerPool,
        cancellation: &ReadCancellationToken,
    ) {
        cancellation.cancel();
        let run_ids = self
            .active
            .keys()
            .map(|identity| identity.run_id().to_owned())
            .collect::<BTreeSet<_>>();
        for run_id in run_ids {
            let _ = worker_pool.cancel_task(&run_id).await;
        }
        // 丢弃 ActiveMedia 时会释放尚未收到 SourceReadComplete 的读取 permit。
        self.active.clear();
    }

    /// 返回已经收到终态的身份，供旧包装器在取消时归还 in-flight 行。
    fn settled(&self) -> &BTreeSet<TaskFileIdentity> {
        &self.settled
    }
}

/// 一轮 Media 派发结果；C/F 迁移必须由后续持久化 ACK 驱动。
pub(crate) struct MediaPassResult<P: TaskLanePermitProvider> {
    /// 继续拥有 dispatcher、上下文和清单的 pending。
    pub(crate) pending: TaskFileBaseComputePending<P>,
    /// 已收到有效 Completed、尚未持久化的结果。
    pub(crate) completed: Vec<TaskFileMediaCompleted>,
    /// 已收到当前项崩溃或协议失败的结果。
    pub(crate) file_failures: Vec<TaskFileMediaFailure>,
    /// 阶段被 Hash 或其他 admission 阻止时的明确原因。
    pub(crate) blocked_reason: Option<TaskDispatchBlockReason>,
    /// 尚未从 dispatcher 领取的 Hash 行数。
    pub(crate) remaining_hash_rows: usize,
    /// 是否由取消令牌触发收束；所有行仍保持 P。
    pub(crate) cancelled: bool,
}

/// Media 阶段发生基础设施错误时返回未完成任务文件所有权。
pub(crate) struct TaskFileMediaComputeError<P: TaskLanePermitProvider> {
    message: String,
    pending: TaskFileBaseComputePending<P>,
}

impl<P: TaskLanePermitProvider> TaskFileMediaComputeError<P> {
    /// 消费错误并取回剩余任务文件所有权。
    pub(crate) fn into_pending(self) -> TaskFileBaseComputePending<P> {
        self.pending
    }
}

impl<P: TaskLanePermitProvider> fmt::Display for TaskFileMediaComputeError<P> {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(&self.message)
    }
}

impl<P: TaskLanePermitProvider> fmt::Debug for TaskFileMediaComputeError<P> {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("TaskFileMediaComputeError")
            .field("message", &self.message)
            .finish_non_exhaustive()
    }
}

/// 运行一轮已知 MD5 的 Media Worker 派发；不在本阶段写入任务文件终态。
pub(crate) async fn run_task_file_media_compute<P>(
    pending: TaskFileBaseComputePending<P>,
    worker_pool: &mut WorkerPool,
    store: &BaseStoreHandle,
    read_config: &DiskReadConfig,
    worker_capacity: usize,
    cancellation: ReadCancellationToken,
) -> Result<MediaPassResult<P>, TaskFileMediaComputeError<P>>
where
    P: TaskLanePermitProvider,
{
    run_task_file_media_compute_inner(
        pending,
        worker_pool,
        store,
        read_config,
        worker_capacity,
        cancellation,
        None,
    )
    .await
}

/// 运行带 Worker 运行时遥测的 Media 派发；Node actor 使用该入口更新 Worker 状态。
pub(crate) async fn run_task_file_media_compute_with_runtime<P>(
    pending: TaskFileBaseComputePending<P>,
    worker_pool: &mut WorkerPool,
    store: &BaseStoreHandle,
    read_config: &DiskReadConfig,
    worker_capacity: usize,
    cancellation: ReadCancellationToken,
    reporter: &RuntimeTaskReporter,
) -> Result<MediaPassResult<P>, TaskFileMediaComputeError<P>>
where
    P: TaskLanePermitProvider,
{
    run_task_file_media_compute_inner(
        pending,
        worker_pool,
        store,
        read_config,
        worker_capacity,
        cancellation,
        Some(reporter),
    )
    .await
}

/// 共享 Media 派发逻辑；报告器只影响运行时投影，不改变任务文件状态机。
async fn run_task_file_media_compute_inner<P>(
    mut pending: TaskFileBaseComputePending<P>,
    worker_pool: &mut WorkerPool,
    store: &BaseStoreHandle,
    read_config: &DiskReadConfig,
    worker_capacity: usize,
    cancellation: ReadCancellationToken,
    reporter: Option<&RuntimeTaskReporter>,
) -> Result<MediaPassResult<P>, TaskFileMediaComputeError<P>>
where
    P: TaskLanePermitProvider,
{
    if worker_capacity == 0 {
        return Err(media_error(pending, "Media Worker 容量必须大于 0"));
    }

    // Hash 阶段产生的上下文仍包含 Hash 行和 Media 行；只有已知 MD5 的行是本轮候选。
    let mut media_candidates = pending
        .contexts
        .len()
        .saturating_sub(pending.remaining_hash_rows);
    let mut active = BTreeMap::<TaskFileIdentity, ActiveMedia<P>>::new();
    let mut settled = BTreeSet::<TaskFileIdentity>::new();
    let mut completed = Vec::new();
    let mut file_failures = Vec::new();
    let mut blocked_reason = None;
    let mut stop_dispatch = false;

    loop {
        if cancellation.is_cancelled() {
            return cancel_media_pass(pending, worker_pool, cancellation, &mut active, &settled)
                .await;
        }

        let can_dispatch = !stop_dispatch && media_candidates > 0 && active.len() < worker_capacity;
        if !can_dispatch && active.is_empty() {
            if media_candidates > 0 {
                // Hash 队首尚未满足读取条件时，Media pass 只返回阻塞状态；保留两类行交给下一轮 Hash/Media 协调。
                if blocked_reason == Some(TaskDispatchBlockReason::HashPending) {
                    break;
                }
                return fail_media_pass(
                    pending,
                    worker_pool,
                    cancellation,
                    &mut active,
                    &settled,
                    "Media dispatcher 在仍有候选行时提前结束",
                )
                .await;
            }
            if pending.remaining_hash_rows > 0 {
                blocked_reason = Some(TaskDispatchBlockReason::HashPending);
            }
            break;
        }

        let dispatch_future = pending
            .dispatcher
            .next_with_admission(cancellation.clone(), TaskDispatchAdmission::media_only());
        tokio::select! {
            biased;
            event = worker_pool.next_event(), if !active.is_empty() => {
                if let Some(event) = event {
                    let telemetry_event = event.clone();
                    if let Some(reporter) = reporter {
                        // 运行时遥测是旁路投影，失败不能改变 Worker/任务文件主状态机。
                        report_media_event(&telemetry_event, reporter, &active).await;
                    }
                    match handle_media_event(event, &mut active, &mut settled) {
                        Ok(Some(TaskFileMediaTerminal::Completed(item))) => completed.push(item),
                        Ok(Some(TaskFileMediaTerminal::Failed(item))) => file_failures.push(item),
                        Ok(None) => {}
                        Err(message) => {
                            return fail_media_pass(
                                pending,
                                worker_pool,
                                cancellation,
                                &mut active,
                                &settled,
                                message,
                            )
                            .await;
                        }
                    }
                } else {
                    return fail_media_pass(
                        pending,
                        worker_pool,
                        cancellation,
                        &mut active,
                        &settled,
                        "WorkerPool 在 Media 运行期间关闭",
                    )
                    .await;
                }
            }
            dispatch = dispatch_future, if can_dispatch => {
                match dispatch {
                    Ok(TaskDispatchPoll::Task(task)) => {
                        let identity = task.identity.clone();
                        media_candidates = media_candidates.saturating_sub(1);
                        match dispatch_media_task(
                            &mut pending,
                            task,
                            worker_pool,
                            store,
                            read_config,
                            &cancellation,
                        ).await {
                            Ok(work) => {
                                active.insert(work.identity.clone(), work);
                            }
                            Err(message) => {
                                let _ = pending.dispatcher.abandon_in_flight(&identity);
                                return fail_media_pass(
                                    pending,
                                    worker_pool,
                                    cancellation,
                                    &mut active,
                                    &settled,
                                    message,
                                )
                                .await;
                            }
                        }
                    }
                    Ok(TaskDispatchPoll::Blocked(reason)) => {
                        blocked_reason = Some(reason);
                        stop_dispatch = true;
                    }
                    Ok(TaskDispatchPoll::Drained) => {
                        stop_dispatch = true;
                    }
                    Err(TaskDispatchError::Read(ReadFailure::Cancelled)) => {
                        return cancel_media_pass(
                            pending,
                            worker_pool,
                            cancellation,
                            &mut active,
                            &settled,
                        )
                        .await;
                    }
                    Err(error) => {
                        return fail_media_pass(
                            pending,
                            worker_pool,
                            cancellation,
                            &mut active,
                            &settled,
                            format!("Media dispatcher 失败: {error}"),
                        )
                        .await;
                    }
                }
            }
            _ = sleep(Duration::from_millis(10)), if can_dispatch || !active.is_empty() => {
                // 轮询取消令牌；下一轮同时继续观察 permit 和 Worker 事件。
            }
        }
    }

    pending.blocked_reason = blocked_reason;
    let remaining_hash_rows = pending.remaining_hash_rows;
    Ok(MediaPassResult {
        pending,
        completed,
        file_failures,
        blocked_reason,
        remaining_hash_rows,
        cancelled: false,
    })
}

/// 将匹配当前 Media 活动项的 Worker 事件投影到进程内运行任务。
async fn report_media_event<P: TaskLanePermitProvider>(
    event: &WorkerEvent,
    reporter: &RuntimeTaskReporter,
    active: &BTreeMap<TaskFileIdentity, ActiveMedia<P>>,
) {
    match event {
        WorkerEvent::Started {
            task_id,
            item_id,
            identity,
            slot,
            process_id,
            cpu_weight,
            decoder_threads,
            ..
        } => {
            let Some(key) = find_active_identity(active, task_id, item_id) else {
                return;
            };
            let Some(work) = active.get(&key) else {
                return;
            };
            if !same_worker_identity(&work.worker_identity, identity, true) {
                return;
            }
            let _ = reporter
                .worker_started(RuntimeWorkerUpdate {
                    slot: *slot,
                    process_id: *process_id,
                    item_id: item_id.clone(),
                    stage: RuntimeStage::ComputeBaseFeatures,
                    display_path: identity
                        .display_path
                        .as_path()
                        .to_string_lossy()
                        .into_owned(),
                    physical_disk_id: identity.physical_disk_id.clone(),
                    completed_files: 0,
                    speed_per_second: 0.0,
                    current_step: "等待 Worker 阶段事件".into(),
                    cache_detail: String::new(),
                    phase: None,
                    cpu_weight: Some(*cpu_weight),
                    decoder_threads: *decoder_threads,
                })
                .await;
        }
        WorkerEvent::PhaseChanged {
            task_id,
            item_id,
            slot,
            phase,
            request_elapsed_us,
        } => {
            let Some(key) = find_active_identity(active, task_id, item_id) else {
                return;
            };
            if active
                .get(&key)
                .is_some_and(|work| work.worker_slot == Some(*slot))
            {
                let _ = reporter.worker_phase_nowait(
                    *slot,
                    item_id,
                    *phase,
                    request_elapsed_us.map(Duration::from_micros),
                );
            }
        }
        WorkerEvent::BaseSourceReadComplete {
            task_id,
            item_id,
            slot,
            request_elapsed_us,
        } => {
            let Some(key) = find_active_identity(active, task_id, item_id) else {
                return;
            };
            if active
                .get(&key)
                .is_some_and(|work| work.worker_slot == Some(*slot))
            {
                let _ = reporter.worker_source_read_complete_nowait(
                    *slot,
                    item_id,
                    request_elapsed_us.map(Duration::from_micros),
                );
            }
        }
        WorkerEvent::Completed {
            task_id, item_id, ..
        } => {
            let Some(key) = find_active_identity(active, task_id, item_id) else {
                return;
            };
            if let Some(slot) = active.get(&key).and_then(|work| work.worker_slot) {
                let _ = reporter.worker_completed(slot).await;
                let _ = reporter.worker_released_nowait(slot, item_id);
            }
        }
        WorkerEvent::Crashed {
            task_id,
            item_id,
            identity,
            process_id,
            exit_code,
            message,
        } => {
            let Some(key) = find_active_identity(active, task_id, item_id) else {
                return;
            };
            let Some(work) = active.get(&key) else {
                return;
            };
            if !same_worker_identity(&work.worker_identity, identity, false) {
                return;
            }
            let _ = reporter.record_failure_nowait(RuntimeFailureUpdate {
                stage: RuntimeStage::ComputeBaseFeatures,
                display_path: identity
                    .display_path
                    .as_path()
                    .to_string_lossy()
                    .into_owned(),
                message: format!(
                    "Worker 崩溃: pid={process_id:?}, exit_code={exit_code:?}: {message}"
                ),
            });
            if let Some(slot) = work.worker_slot {
                let _ = reporter.worker_released_nowait(slot, item_id);
            }
        }
        WorkerEvent::Cancelled { .. }
        | WorkerEvent::InfrastructureFailure { .. }
        // Stage2SourceReadComplete 由后续 Stage2 taskless 流程消费，基础计算阶段只需保留 slot。
        | WorkerEvent::Stage2SourceReadComplete { .. } => {}
    }
}

/// 把一项 dispatcher 任务转换为 V5 Media 请求并交给 WorkerPool。
async fn dispatch_media_task<P>(
    pending: &mut TaskFileBaseComputePending<P>,
    task: crate::task_dispatch::DispatchedTask<P::Permit>,
    worker_pool: &WorkerPool,
    store: &BaseStoreHandle,
    read_config: &DiskReadConfig,
    cancellation: &ReadCancellationToken,
) -> Result<ActiveMedia<P>, String>
where
    P: TaskLanePermitProvider,
{
    if task.class != DiskReadClass::MediaDecode
        || task.record.work_kind != TaskWorkKind::Base
        || task.record.known_md5.is_none()
        || task.record.missing.needs_md5()
        || task.record.missing.base_missing_parts() == 0
    {
        return Err("Media dispatcher 返回了无效基础任务行".into());
    }
    let identity = task.identity.clone();
    let Some(mut context) = pending.contexts.get(&identity).cloned() else {
        return Err("Media 任务缺少对应内存上下文".into());
    };
    let md5 = task
        .record
        .known_md5
        .expect("上方已验证 Media 任务携带 known_md5");
    let media_kind = context
        .cached
        .as_ref()
        .map_or(MediaKind::Other, |record| record.media_kind);
    let expected_key = ContentKey::new(md5, task.record.scanned.file_size);
    if context
        .cached
        .as_ref()
        .is_some_and(|cached| cached.content_key != expected_key)
    {
        return Err("Media 缓存上下文 ContentKey 与任务行不一致".into());
    }
    // 每个 Media 行都幂等补写当前位置；context 中的 content_id 只是快照，不能代替
    // 本次路径归属写入，否则 Hash 命中旧内容后换路径会漏掉 files 行。
    let content = match store.upsert_content_and_location(&task.record.scanned, md5, media_kind) {
        Ok(content) => content,
        Err(error) => return Err(format!("Media 内容位置写入失败: {error}")),
    };
    if content.key != expected_key {
        return Err("Media Store 返回的 ContentKey 与任务行不一致".into());
    }
    context.content_id = Some(content.id);
    if let Some(cached) = context.cached.as_mut() {
        cached.content_id = Some(content.id);
    }
    pending.contexts.insert(identity.clone(), context.clone());
    let physical_disk_id = physical_disk_display(&context.lane);
    let worker_identity = WorkerFileIdentity {
        machine_id: store.machine_id().clone(),
        normalized_path: task.record.scanned.normalized_path.clone(),
        display_path: task.record.scanned.display_path.clone(),
        file_size: task.record.scanned.file_size,
        stage: "base_compute".into(),
        physical_disk_id: physical_disk_id.clone(),
    };
    let command = proto::WorkerEnvelope {
        payload: Some(worker_envelope::Payload::ComputeBaseFeatures(
            proto::ComputeBaseFeatures {
                task_id: identity.run_id().to_owned(),
                item_id: identity.item_id().to_string(),
                machine_id: store.machine_id().as_str().to_owned(),
                normalized_path: task.record.scanned.normalized_path.as_str().to_owned(),
                display_path: task
                    .record
                    .scanned
                    .display_path
                    .as_path()
                    .to_string_lossy()
                    .into_owned(),
                file_size: task.record.scanned.file_size,
                physical_disk_id,
                md5: md5.to_vec(),
                media_kind: proto_media_kind(media_kind) as i32,
                missing_parts: task.record.missing.base_missing_parts(),
                block_size_bytes: read_config.block_size_bytes as u64,
                block_timeout_ms: read_config.block_timeout_seconds.saturating_mul(1_000),
                block_retries: read_config.block_retries,
                decoder_threads: worker_pool.decoder_threads_for(media_kind),
            },
        )),
    };
    if let Err(error) = worker_pool
        .dispatch_scan(command, cancellation.clone(), true, worker_identity.clone())
        .await
    {
        return Err(format!("Media Worker 派发失败: {error}"));
    }
    Ok(ActiveMedia {
        identity,
        record: task.record,
        context,
        permit: Some(task.permit),
        worker_slot: None,
        source_read_complete: false,
        worker_identity,
    })
}

/// 处理一条 Worker 事件；外部任务/身份不匹配时完全不触碰当前资源。
fn handle_media_event(
    event: WorkerEvent,
    active: &mut BTreeMap<TaskFileIdentity, ActiveMedia<impl TaskLanePermitProvider>>,
    settled: &mut BTreeSet<TaskFileIdentity>,
) -> Result<Option<TaskFileMediaTerminal>, String> {
    match event {
        WorkerEvent::Started {
            task_id,
            item_id,
            identity,
            slot,
            ..
        } => {
            let Some(key) = find_active_identity(active, &task_id, &item_id) else {
                return Ok(None);
            };
            let Some(work) = active.get_mut(&key) else {
                return Ok(None);
            };
            if !same_worker_identity(&work.worker_identity, &identity, true) {
                return Ok(None);
            }
            work.worker_slot = Some(slot);
        }
        WorkerEvent::BaseSourceReadComplete {
            task_id,
            item_id,
            slot,
            ..
        } => {
            let Some(key) = find_active_identity(active, &task_id, &item_id) else {
                return Ok(None);
            };
            let Some(work) = active.get_mut(&key) else {
                return Ok(None);
            };
            if work.worker_slot == Some(slot) {
                work.source_read_complete = true;
                drop(work.permit.take());
            }
        }
        WorkerEvent::Completed {
            task_id,
            item_id,
            response,
        } => {
            let Some(key) = find_active_identity(active, &task_id, &item_id) else {
                return Ok(None);
            };
            let Some(work) = active.remove(&key) else {
                return Ok(None);
            };
            settled.insert(key.clone());
            let worker_slot = work.worker_slot;
            if !work.source_read_complete {
                return Ok(Some(TaskFileMediaTerminal::Failed(TaskFileMediaFailure {
                    identity: work.identity,
                    record: work.record,
                    context: work.context,
                    message: "Worker Completed 前未收到 BaseSourceReadComplete".into(),
                    worker_slot,
                })));
            } else {
                return Ok(Some(TaskFileMediaTerminal::Completed(TaskFileMediaCompleted {
                    identity: work.identity,
                    record: work.record,
                    context: work.context,
                    response,
                    worker_slot,
                })));
            }
        }
        WorkerEvent::Crashed {
            task_id,
            item_id,
            identity,
            message,
            ..
        } => {
            let Some(key) = find_active_identity(active, &task_id, &item_id) else {
                return Ok(None);
            };
            let Some(work) = active.get(&key) else {
                return Ok(None);
            };
            if !same_worker_identity(&work.worker_identity, &identity, false) {
                return Ok(None);
            }
            let work = active.remove(&key).expect("上方已确认活动项存在");
            settled.insert(key);
            return Ok(Some(TaskFileMediaTerminal::Failed(TaskFileMediaFailure {
                identity: work.identity,
                record: work.record,
                context: work.context,
                message,
                worker_slot: work.worker_slot,
            })));
        }
        WorkerEvent::InfrastructureFailure { message } => {
            return Err(format!("Worker 基础设施失败: {message}"));
        }
        WorkerEvent::Cancelled { task_id, item_id } => {
            if find_active_identity(active, &task_id, &item_id).is_some() {
                return Err("Media Worker 在非取消流程返回 Cancelled".into());
            }
        }
        WorkerEvent::PhaseChanged { .. }
        // 当前基础媒体状态机不消费二筛源读取事件，事件本身已由 Pool 保持非终态所有权。
        | WorkerEvent::Stage2SourceReadComplete { .. } => {}
    }
    Ok(None)
}

/// 按 Worker 事件的 run/item 找到当前活动身份。
fn find_active_identity<P: TaskLanePermitProvider>(
    active: &BTreeMap<TaskFileIdentity, ActiveMedia<P>>,
    task_id: &str,
    item_id: &str,
) -> Option<TaskFileIdentity> {
    active
        .keys()
        .find(|identity| identity.run_id() == task_id && identity.item_id().to_string() == item_id)
        .cloned()
}

/// 比较 Worker 文件身份；Started 要求阶段也精确一致，崩溃允许阶段已更新。
fn same_worker_identity(
    expected: &WorkerFileIdentity,
    actual: &WorkerFileIdentity,
    include_stage: bool,
) -> bool {
    expected.machine_id == actual.machine_id
        && expected.normalized_path == actual.normalized_path
        && expected.display_path == actual.display_path
        && expected.file_size == actual.file_size
        && expected.physical_disk_id == actual.physical_disk_id
        && (!include_stage || expected.stage == actual.stage)
}

/// 把冻结的物理盘 lane 映射成 Worker 协议显示值。
fn physical_disk_display(lane: &super::TaskDiskLane) -> String {
    format!(
        "PhysicalDisk{}",
        lane.physical_disk_numbers
            .iter()
            .map(u32::to_string)
            .collect::<Vec<_>>()
            .join("+")
    )
}

/// 领域媒体类型转换为 Worker 协议枚举。
const fn proto_media_kind(media_kind: MediaKind) -> proto::MediaKind {
    match media_kind {
        MediaKind::Image => proto::MediaKind::MediaImage,
        MediaKind::Video => proto::MediaKind::MediaVideo,
        MediaKind::Other => proto::MediaKind::MediaOther,
    }
}

/// 取消或基础设施错误前清空 dispatcher 等待项，并保持所有 TSV 行为 P。
async fn cleanup_media_ownership<P: TaskLanePermitProvider>(
    pending: &mut TaskFileBaseComputePending<P>,
    worker_pool: &WorkerPool,
    cancellation: &ReadCancellationToken,
    active: &mut BTreeMap<TaskFileIdentity, ActiveMedia<P>>,
    settled: &BTreeSet<TaskFileIdentity>,
) {
    cancellation.cancel();
    let _ = pending
        .dispatcher
        .next_with_admission(cancellation.clone(), TaskDispatchAdmission::media_only())
        .await;
    let run_id = active
        .keys()
        .next()
        .or_else(|| pending.contexts.keys().next())
        .map(|identity| identity.run_id().to_owned());
    if let Some(run_id) = run_id {
        let _ = worker_pool.cancel_task(&run_id).await;
    }
    let mut identities = settled.clone();
    identities.extend(active.keys().cloned());
    identities.extend(pending.contexts.keys().cloned());
    let active = std::mem::take(active);
    for (identity, work) in active {
        drop(work.permit);
        let _ = pending.dispatcher.abandon_in_flight(&identity);
    }
    for identity in identities {
        let _ = pending.dispatcher.abandon_in_flight(&identity);
    }
}

/// 返回取消结果；不把尚未 ACK 的结果伪造成 C/F。
async fn cancel_media_pass<P: TaskLanePermitProvider>(
    mut pending: TaskFileBaseComputePending<P>,
    worker_pool: &WorkerPool,
    cancellation: ReadCancellationToken,
    active: &mut BTreeMap<TaskFileIdentity, ActiveMedia<P>>,
    settled: &BTreeSet<TaskFileIdentity>,
) -> Result<MediaPassResult<P>, TaskFileMediaComputeError<P>> {
    cleanup_media_ownership(&mut pending, worker_pool, &cancellation, active, settled).await;
    let remaining_hash_rows = pending.remaining_hash_rows;
    Ok(MediaPassResult {
        pending,
        completed: Vec::new(),
        file_failures: Vec::new(),
        blocked_reason: Some(TaskDispatchBlockReason::MediaPending),
        remaining_hash_rows,
        cancelled: true,
    })
}

/// 发生基础设施错误时归还完整 pending 所有权，供外层决定 discard 或重试。
async fn fail_media_pass<P: TaskLanePermitProvider>(
    mut pending: TaskFileBaseComputePending<P>,
    worker_pool: &WorkerPool,
    cancellation: ReadCancellationToken,
    active: &mut BTreeMap<TaskFileIdentity, ActiveMedia<P>>,
    settled: &BTreeSet<TaskFileIdentity>,
    message: impl Into<String>,
) -> Result<MediaPassResult<P>, TaskFileMediaComputeError<P>> {
    cleanup_media_ownership(&mut pending, worker_pool, &cancellation, active, settled).await;
    Err(media_error(pending, message))
}

/// 构造携带剩余任务文件所有权的 Media 错误。
fn media_error<P: TaskLanePermitProvider>(
    pending: TaskFileBaseComputePending<P>,
    message: impl Into<String>,
) -> TaskFileMediaComputeError<P> {
    TaskFileMediaComputeError {
        message: message.into(),
        pending,
    }
}

#[cfg(test)]
mod tests {
    use std::{
        collections::{BTreeMap, BTreeSet},
        future::Future,
        path::Path,
        pin::Pin,
        sync::{
            Arc,
            atomic::{AtomicIsize, Ordering},
        },
        time::Duration,
    };

    use dedup_core::{
        ContentKey, DiskReadConfig, DisplayPath, LocationKey, MachineId, MediaKind, NormalizedPath,
    };
    use dedup_node_store::{BaseCacheRecord, NodeStore, ScannedPath};
    use dedup_protocol::proto;
    use dedup_windows::{LocalDiskKind, PhysicalDiskId, ReadCancellationToken};

    use crate::{
        io::{DiskReadClass, ReadFailure},
        scan::{
            BaseTaskInput, BaseTaskProducer, BaseTaskProduction, HashPermitReader,
            PlannedScannedPath, ReadProduct, TaskDiskLane,
        },
        task_dispatch::{TaskFileDispatcher, TaskLanePermitFuture, TaskLanePermitProvider},
        task_files::{
            TaskFileIdentity, TaskFileRecord, TaskWorkKind, TaskWorkMask, TransientTaskFileSet,
        },
        worker::{BaseComputeOutput, WorkerPool},
    };

    use super::super::task_file_base_compute::run_task_file_base_compute;
    use super::{
        ActiveMedia, TaskFileMediaRuntime, TaskFileMediaTerminal, WorkerEvent, WorkerFileIdentity,
        handle_media_event, physical_disk_display, run_task_file_media_compute,
    };

    const RUN_ID: &str = "01900000-0000-7000-8000-000000000202";

    #[derive(Clone)]
    struct TrackedPermit {
        live: Arc<AtomicIsize>,
    }

    impl Drop for TrackedPermit {
        fn drop(&mut self) {
            self.live.fetch_sub(1, Ordering::SeqCst);
        }
    }

    #[derive(Clone)]
    struct TestProvider {
        live: Arc<AtomicIsize>,
    }

    impl TaskLanePermitProvider for TestProvider {
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

    #[derive(Clone, Default)]
    struct TestHashReader {
        results: Arc<BTreeMap<String, [u8; 16]>>,
    }

    impl HashPermitReader for TestHashReader {
        type Permit = TrackedPermit;

        fn read_with_permit(
            &self,
            scanned: ScannedPath,
            permit: Self::Permit,
            _cancellation: ReadCancellationToken,
            _started: Option<crate::scan::HashReadStartedSignal>,
        ) -> Pin<
            Box<
                dyn Future<Output = Result<ReadProduct<Self::Permit>, ReadFailure>>
                    + Send
                    + 'static,
            >,
        > {
            let result = self.results.get(scanned.normalized_path.as_str()).copied();
            let path = scanned.display_path.as_path().to_path_buf();
            Box::pin(async move {
                result.map_or_else(
                    || {
                        let _ = permit;
                        Err(ReadFailure::Io {
                            path,
                            block_offset: 0,
                            source: std::io::Error::other("测试 Hash 路径不存在"),
                        })
                    },
                    |md5| Ok(ReadProduct { md5, lease: permit }),
                )
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

    fn seed_record(
        store: &mut NodeStore,
        path: &str,
        md5: [u8; 16],
        file_size: u64,
    ) -> BaseCacheRecord {
        let content = store
            .upsert_content_and_location(&scanned(path, file_size), md5, MediaKind::Other)
            .unwrap();
        store.load_base_cache_record(content.id).unwrap()
    }

    fn input(
        path: ScannedPath,
        lane: TaskDiskLane,
        cached: Option<BaseCacheRecord>,
    ) -> BaseTaskInput {
        BaseTaskInput {
            planned: PlannedScannedPath {
                scanned: path,
                lane,
            },
            cached,
            contact_sheet_valid: true,
            force_recompute: false,
        }
    }

    fn production(
        root: &Path,
        inputs: &[BaseTaskInput],
        provider: TestProvider,
    ) -> BaseTaskProduction<TestProvider> {
        let files = TransientTaskFileSet::create(root, RUN_ID).unwrap();
        let mut producer = BaseTaskProducer::new(TaskFileDispatcher::new(files, provider));
        producer.append_batch(inputs).unwrap();
        producer.seal().unwrap()
    }

    fn empty_output() -> BaseComputeOutput {
        BaseComputeOutput {
            probe: None,
            stage1_frames: None,
            contact_sheet_jpeg: None,
        }
    }

    #[tokio::test]
    async fn media_runtime_handles_started_source_complete_and_terminal_separately() {
        let live = Arc::new(AtomicIsize::new(1));
        let lane = lane(7);
        let missing = TaskWorkMask::for_base(false, 1).unwrap();
        let item_id = uuid::Uuid::parse_str("01900000-0000-7000-8000-000000000221").unwrap();
        let identity = TaskFileIdentity::new(RUN_ID, &lane, item_id, 0, 80, missing).unwrap();
        let record = TaskFileRecord {
            item_id,
            work_kind: TaskWorkKind::Base,
            scanned: scanned(r"C:\runtime-media.bin", 32),
            known_md5: Some([0x51; 16]),
            missing,
        };
        let worker_identity = WorkerFileIdentity {
            machine_id: MachineId::from_sha256([0x52; 32]),
            normalized_path: record.scanned.normalized_path.clone(),
            display_path: record.scanned.display_path.clone(),
            file_size: record.scanned.file_size,
            stage: "base_compute".into(),
            physical_disk_id: physical_disk_display(&lane),
        };
        let mut runtime = TaskFileMediaRuntime::<TestProvider>::new();
        runtime.active.insert(
            identity.clone(),
            ActiveMedia {
                identity: identity.clone(),
                record,
                context: super::super::TaskFileBaseContext {
                    lane,
                    content_id: None,
                    cached: None,
                    contact_sheet_valid: true,
                    force_recompute: false,
                },
                permit: Some(TrackedPermit {
                    live: Arc::clone(&live),
                }),
                worker_slot: None,
                source_read_complete: false,
                worker_identity: worker_identity.clone(),
            },
        );

        assert!(
            runtime
                .handle_event(
                    WorkerEvent::Started {
                        task_id: identity.run_id().into(),
                        item_id: identity.item_id().to_string(),
                        identity: worker_identity,
                        slot: 3,
                        process_id: None,
                        cpu_weight: 1,
                        decoder_threads: Some(1),
                        queue_wait_us: 0,
                    },
                    None,
                )
                .await
                .unwrap()
                .is_none()
        );
        assert_eq!(runtime.active_len(), 1);
        assert_eq!(live.load(Ordering::SeqCst), 1);

        assert!(
            runtime
                .handle_event(
                    WorkerEvent::BaseSourceReadComplete {
                        task_id: identity.run_id().into(),
                        item_id: identity.item_id().to_string(),
                        slot: 3,
                        request_elapsed_us: None,
                    },
                    None,
                )
                .await
                .unwrap()
                .is_none()
        );
        assert_eq!(runtime.active_len(), 1);
        assert_eq!(live.load(Ordering::SeqCst), 0);

        let terminal = runtime
            .handle_event(
                WorkerEvent::Completed {
                    task_id: identity.run_id().into(),
                    item_id: identity.item_id().to_string(),
                    response: proto::WorkerEnvelope { payload: None },
                },
                None,
            )
            .await
            .unwrap();
        assert!(matches!(
            terminal,
            Some(TaskFileMediaTerminal::Completed(_))
        ));
        assert_eq!(runtime.active_len(), 0);
    }

    #[tokio::test]
    async fn media_permit_is_held_until_matching_source_read_complete() {
        let root = tempfile::tempdir().unwrap();
        let machine = MachineId::from_sha256([0x61; 32]);
        let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
        let cached = seed_record(&mut store, r"C:\seed-media.bin", [0x11; 16], 42);
        let live = Arc::new(AtomicIsize::new(0));
        let pending =
            super::super::task_file_base_compute::TaskFileBaseComputePending::from_production(
                production(
                    root.path(),
                    &[input(scanned(r"C:\media.bin", 42), lane(7), Some(cached))],
                    TestProvider {
                        live: Arc::clone(&live),
                    },
                ),
            );
        let (actor, handle, acknowledgements) =
            super::super::base_persistence::BaseStoreActor::spawn(store, 4);
        let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
        let config = DiskReadConfig::default();
        let handle_for_run = handle.clone();
        let cancellation = ReadCancellationToken::new();
        let join = tokio::spawn(async move {
            let result = run_task_file_media_compute(
                pending,
                &mut pool,
                &handle_for_run,
                &config,
                1,
                cancellation,
            )
            .await;
            let shutdown = pool.shutdown().await;
            (result, shutdown)
        });

        let (task_id, item_id) = tokio::time::timeout(Duration::from_secs(1), started.recv())
            .await
            .unwrap()
            .unwrap();
        assert_eq!(task_id, RUN_ID);
        assert_eq!(live.load(Ordering::SeqCst), 1);
        controller
            .base_source_read_complete(task_id.clone(), item_id.clone())
            .await;
        tokio::time::timeout(Duration::from_secs(1), async {
            while live.load(Ordering::SeqCst) != 0 {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("匹配 SourceReadComplete 后必须立即释放媒体许可");
        controller
            .complete_base(task_id, item_id, [0x11; 16], empty_output())
            .await;

        let (result, shutdown) = join.await.unwrap();
        assert!(shutdown.is_ok());
        let result = result.unwrap();
        assert_eq!(result.completed.len(), 1);
        assert!(result.file_failures.is_empty());
        assert_eq!(result.pending.contexts.len(), 1);
        let path = result.pending.dispatcher.lane_path(&lane(7)).unwrap();
        assert_eq!(std::fs::read(path).unwrap()[0], b'P');
        drop(handle);
        drop(acknowledgements);
        let store = actor.finish().await.unwrap();
        let location = LocationKey::new(
            machine.clone(),
            NormalizedPath::new(r"C:\media.bin").unwrap(),
        );
        let active = store
            .active_file(&location)
            .unwrap()
            .expect("Media 派发必须幂等补写当前路径位置");
        assert_eq!(
            active.content_key,
            ContentKey::new([0x11; 16], 42),
            "补写的位置必须绑定任务行的 ContentKey"
        );
    }

    #[tokio::test]
    async fn worker_crash_only_fails_current_item_and_other_lane_completes() {
        let root = tempfile::tempdir().unwrap();
        let machine = MachineId::from_sha256([0x62; 32]);
        let mut store = NodeStore::open_in_memory(machine).unwrap();
        let cached_a = seed_record(&mut store, r"C:\seed-a.bin", [0x21; 16], 11);
        let cached_b = seed_record(&mut store, r"C:\seed-b.bin", [0x22; 16], 12);
        let live = Arc::new(AtomicIsize::new(0));
        let provider = TestProvider {
            live: Arc::clone(&live),
        };
        let production = production(
            root.path(),
            &[
                input(scanned(r"C:\media-a.bin", 11), lane(7), Some(cached_a)),
                input(scanned(r"C:\media-b.bin", 12), lane(8), Some(cached_b)),
            ],
            provider,
        );
        let pending =
            super::super::task_file_base_compute::TaskFileBaseComputePending::from_production(
                production,
            );
        let (actor, handle, acknowledgements) =
            super::super::base_persistence::BaseStoreActor::spawn(store, 4);
        let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(2);
        let config = DiskReadConfig::default();
        let cancellation = ReadCancellationToken::new();
        let handle_for_run = handle.clone();
        let join = tokio::spawn(async move {
            let result = run_task_file_media_compute(
                pending,
                &mut pool,
                &handle_for_run,
                &config,
                2,
                cancellation,
            )
            .await;
            let shutdown = pool.shutdown().await;
            (result, shutdown)
        });

        let first = tokio::time::timeout(Duration::from_secs(1), started.recv())
            .await
            .unwrap()
            .unwrap();
        let second = tokio::time::timeout(Duration::from_secs(1), started.recv())
            .await
            .unwrap()
            .unwrap();
        assert_eq!(live.load(Ordering::SeqCst), 2);
        controller
            .crash(first.0.clone(), first.1.clone(), "测试 Worker 崩溃".into())
            .await;
        controller
            .base_source_read_complete(second.0.clone(), second.1.clone())
            .await;
        controller
            .complete_base(second.0, second.1, [0x22; 16], empty_output())
            .await;

        let (result, shutdown) = join.await.unwrap();
        assert!(shutdown.is_ok());
        let result = result.unwrap();
        assert_eq!(result.completed.len(), 1);
        assert_eq!(result.file_failures.len(), 1);
        assert_eq!(live.load(Ordering::SeqCst), 0);
        assert!(
            result.completed[0]
                .record
                .scanned
                .normalized_path
                .as_str()
                .ends_with("MEDIA-B.BIN")
        );
        assert!(
            result.file_failures[0]
                .record
                .scanned
                .normalized_path
                .as_str()
                .ends_with("MEDIA-A.BIN")
        );
        assert!(result.pending.contexts.len() == 2);
        drop(handle);
        drop(acknowledgements);
        actor.finish().await.unwrap();
    }

    #[tokio::test]
    async fn hash_to_media_continuation_keeps_the_same_identity() {
        let root = tempfile::tempdir().unwrap();
        let machine = MachineId::from_sha256([0x63; 32]);
        let store = NodeStore::open_in_memory(machine).unwrap();
        let live = Arc::new(AtomicIsize::new(0));
        let original = production(
            root.path(),
            &[input(scanned(r"C:\hash-then-media.bin", 13), lane(7), None)],
            TestProvider {
                live: Arc::clone(&live),
            },
        );
        let (actor, handle, mut acknowledgements) =
            super::super::base_persistence::BaseStoreActor::spawn(store, 4);
        let hash_key = NormalizedPath::new(r"C:\hash-then-media.bin")
            .unwrap()
            .as_str()
            .to_owned();
        let pending = run_task_file_base_compute(
            original,
            TestHashReader {
                results: Arc::new(BTreeMap::from([(hash_key, [0x31; 16])])),
            },
            1,
            &handle,
            &mut acknowledgements,
            ReadCancellationToken::new(),
        )
        .await
        .unwrap();
        let continuation_identity = pending.contexts.keys().next().unwrap().clone();

        let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
        let config = DiskReadConfig::default();
        let handle_for_run = handle.clone();
        let join = tokio::spawn(async move {
            let result = run_task_file_media_compute(
                pending,
                &mut pool,
                &handle_for_run,
                &config,
                1,
                ReadCancellationToken::new(),
            )
            .await;
            let shutdown = pool.shutdown().await;
            (result, shutdown)
        });
        let (task_id, item_id) = started.recv().await.unwrap();
        controller
            .base_source_read_complete(task_id.clone(), item_id.clone())
            .await;
        controller
            .complete_base(task_id, item_id, [0x31; 16], empty_output())
            .await;
        let (result, shutdown) = join.await.unwrap();
        assert!(shutdown.is_ok());
        let result = result.unwrap();
        assert_eq!(result.completed.len(), 1);
        assert_eq!(result.completed[0].identity, continuation_identity);
        drop(handle);
        actor.finish().await.unwrap();
    }

    #[tokio::test]
    async fn cancellation_keeps_pending_rows_and_allows_discard() {
        let root = tempfile::tempdir().unwrap();
        let machine = MachineId::from_sha256([0x64; 32]);
        let mut store = NodeStore::open_in_memory(machine).unwrap();
        let cached = seed_record(&mut store, r"C:\seed-cancel.bin", [0x41; 16], 14);
        let live = Arc::new(AtomicIsize::new(0));
        let pending =
            super::super::task_file_base_compute::TaskFileBaseComputePending::from_production(
                production(
                    root.path(),
                    &[input(scanned(r"C:\cancel.bin", 14), lane(7), Some(cached))],
                    TestProvider {
                        live: Arc::clone(&live),
                    },
                ),
            );
        let (actor, handle, acknowledgements) =
            super::super::base_persistence::BaseStoreActor::spawn(store, 4);
        let (mut pool, mut started, _controller) = WorkerPool::controlled_batch_for_test(1);
        let cancellation = ReadCancellationToken::new();
        let cancellation_for_run = cancellation.clone();
        let handle_for_run = handle.clone();
        let config = DiskReadConfig::default();
        let join = tokio::spawn(async move {
            let result = run_task_file_media_compute(
                pending,
                &mut pool,
                &handle_for_run,
                &config,
                1,
                cancellation_for_run,
            )
            .await;
            let shutdown = pool.shutdown().await;
            (result, shutdown)
        });
        let (_task_id, _item_id) = started.recv().await.unwrap();
        assert_eq!(live.load(Ordering::SeqCst), 1);
        cancellation.cancel();
        let (result, shutdown) = tokio::time::timeout(Duration::from_secs(1), join)
            .await
            .unwrap()
            .unwrap();
        assert!(shutdown.is_ok());
        let mut result = result.unwrap();
        assert!(result.cancelled);
        assert!(result.completed.is_empty());
        assert!(result.file_failures.is_empty());
        assert_eq!(result.pending.contexts.len(), 1);
        assert_eq!(live.load(Ordering::SeqCst), 0);
        result.pending.dispatcher.discard().unwrap();
        drop(handle);
        drop(acknowledgements);
        actor.finish().await.unwrap();
    }

    #[tokio::test]
    async fn media_first_returns_explicit_hash_block_without_faking_completion() {
        let root = tempfile::tempdir().unwrap();
        let machine = MachineId::from_sha256([0x65; 32]);
        let mut store = NodeStore::open_in_memory(machine).unwrap();
        let cached = seed_record(&mut store, r"C:\seed-mixed.bin", [0x51; 16], 15);
        let live = Arc::new(AtomicIsize::new(0));
        let pending =
            super::super::task_file_base_compute::TaskFileBaseComputePending::from_production(
                production(
                    root.path(),
                    &[
                        input(scanned(r"C:\media-first.bin", 15), lane(7), Some(cached)),
                        input(scanned(r"C:\hash-next.bin", 16), lane(7), None),
                    ],
                    TestProvider {
                        live: Arc::clone(&live),
                    },
                ),
            );
        assert_eq!(pending.remaining_hash_rows, 1);
        let (actor, handle, acknowledgements) =
            super::super::base_persistence::BaseStoreActor::spawn(store, 4);
        let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
        let handle_for_run = handle.clone();
        let config = DiskReadConfig::default();
        let join = tokio::spawn(async move {
            let result = run_task_file_media_compute(
                pending,
                &mut pool,
                &handle_for_run,
                &config,
                1,
                ReadCancellationToken::new(),
            )
            .await;
            let shutdown = pool.shutdown().await;
            (result, shutdown)
        });
        let (task_id, item_id) = started.recv().await.unwrap();
        controller
            .base_source_read_complete(task_id.clone(), item_id.clone())
            .await;
        controller
            .complete_base(task_id, item_id, [0x51; 16], empty_output())
            .await;
        let (result, shutdown) = join.await.unwrap();
        assert!(shutdown.is_ok());
        let result = result.unwrap();
        assert_eq!(result.completed.len(), 1);
        assert!(result.file_failures.is_empty());
        assert_eq!(
            result.blocked_reason,
            Some(crate::task_dispatch::TaskDispatchBlockReason::HashPending)
        );
        assert_eq!(result.remaining_hash_rows, 1);
        assert!(!result.cancelled);
        assert_eq!(result.pending.contexts.len(), 2);
        assert_eq!(live.load(Ordering::SeqCst), 0);
        drop(handle);
        drop(acknowledgements);
        actor.finish().await.unwrap();
    }

    #[tokio::test]
    async fn media_blocked_by_hash_head_returns_blocked_without_worker() {
        let root = tempfile::tempdir().unwrap();
        let machine = MachineId::from_sha256([0x67; 32]);
        let mut store = NodeStore::open_in_memory(machine).unwrap();
        let cached = seed_record(&mut store, r"C:\seed-media-behind-hash.bin", [0x71; 16], 16);
        let live = Arc::new(AtomicIsize::new(0));
        let pending =
            super::super::task_file_base_compute::TaskFileBaseComputePending::from_production(
                production(
                    root.path(),
                    &[
                        input(scanned(r"C:\hash-head.bin", 15), lane(7), None),
                        input(
                            scanned(r"C:\media-behind-hash.bin", 16),
                            lane(7),
                            Some(cached),
                        ),
                    ],
                    TestProvider {
                        live: Arc::clone(&live),
                    },
                ),
            );
        let (actor, handle, acknowledgements) =
            super::super::base_persistence::BaseStoreActor::spawn(store, 4);
        let (mut pool, mut started, _controller) = WorkerPool::controlled_batch_for_test(1);
        let mut result = run_task_file_media_compute(
            pending,
            &mut pool,
            &handle,
            &DiskReadConfig::default(),
            1,
            ReadCancellationToken::new(),
        )
        .await
        .expect("Hash 队首阻塞时 Media pass 必须返回正常阻塞结果");
        assert_eq!(
            result.blocked_reason,
            Some(crate::task_dispatch::TaskDispatchBlockReason::HashPending)
        );
        assert!(!result.cancelled);
        assert!(result.completed.is_empty());
        assert!(result.file_failures.is_empty());
        assert_eq!(result.pending.contexts.len(), 2);
        assert_eq!(live.load(Ordering::SeqCst), 0);
        assert!(
            started.try_recv().is_err(),
            "Hash 队首阻塞时不得启动 Media Worker"
        );
        let lane_path = result.pending.dispatcher.lane_path(&lane(7)).unwrap();
        assert!(
            std::fs::read(lane_path)
                .unwrap()
                .split(|byte| *byte == b'\n')
                .filter(|line| !line.is_empty())
                .all(|line| line[0] == b'P')
        );
        result.pending.dispatcher.discard().unwrap();
        assert!(pool.shutdown().await.is_ok());
        drop(handle);
        drop(acknowledgements);
        actor.finish().await.unwrap();
    }

    #[tokio::test]
    async fn mismatched_started_or_source_event_does_not_release_media_permit() {
        let root = tempfile::tempdir().unwrap();
        let machine = MachineId::from_sha256([0x66; 32]);
        let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
        let cached = seed_record(&mut store, r"C:\seed-mismatch.bin", [0x61; 16], 17);
        let live = Arc::new(AtomicIsize::new(0));
        let mut pending =
            super::super::task_file_base_compute::TaskFileBaseComputePending::from_production(
                production(
                    root.path(),
                    &[input(
                        scanned(r"C:\mismatch.bin", 17),
                        lane(7),
                        Some(cached),
                    )],
                    TestProvider {
                        live: Arc::clone(&live),
                    },
                ),
            );
        let task = match pending
            .dispatcher
            .next_with_admission(
                ReadCancellationToken::new(),
                crate::task_dispatch::TaskDispatchAdmission::media_only(),
            )
            .await
            .unwrap()
        {
            crate::task_dispatch::TaskDispatchPoll::Task(task) => task,
            _ => panic!("测试任务必须取得 Media permit"),
        };
        assert_eq!(live.load(Ordering::SeqCst), 1);
        let identity = task.identity.clone();
        let record = task.record.clone();
        let context = pending.contexts.get(&identity).unwrap().clone();
        let expected_worker_identity = WorkerFileIdentity {
            machine_id: machine,
            normalized_path: record.scanned.normalized_path.clone(),
            display_path: record.scanned.display_path.clone(),
            file_size: record.scanned.file_size,
            stage: "base_compute".into(),
            physical_disk_id: physical_disk_display(&context.lane),
        };
        let mut active = BTreeMap::from([(
            identity.clone(),
            ActiveMedia::<TestProvider> {
                identity: identity.clone(),
                record,
                context,
                permit: Some(task.permit),
                worker_slot: None,
                source_read_complete: false,
                worker_identity: expected_worker_identity.clone(),
            },
        )]);
        let mut settled = BTreeSet::new();
        let mismatch = WorkerFileIdentity {
            physical_disk_id: "PhysicalDisk999".into(),
            ..expected_worker_identity.clone()
        };
        handle_media_event(
            WorkerEvent::Started {
                task_id: identity.run_id().into(),
                item_id: identity.item_id().to_string(),
                slot: 1,
                process_id: None,
                identity: mismatch,
                cpu_weight: 1,
                decoder_threads: Some(1),
                queue_wait_us: 0,
            },
            &mut active,
            &mut settled,
        )
        .unwrap();
        handle_media_event(
            WorkerEvent::BaseSourceReadComplete {
                task_id: identity.run_id().into(),
                item_id: identity.item_id().to_string(),
                slot: 1,
                request_elapsed_us: None,
            },
            &mut active,
            &mut settled,
        )
        .unwrap();
        assert_eq!(live.load(Ordering::SeqCst), 1);
        handle_media_event(
            WorkerEvent::Started {
                task_id: identity.run_id().into(),
                item_id: identity.item_id().to_string(),
                slot: 1,
                process_id: None,
                identity: expected_worker_identity,
                cpu_weight: 1,
                decoder_threads: Some(1),
                queue_wait_us: 0,
            },
            &mut active,
            &mut settled,
        )
        .unwrap();
        handle_media_event(
            WorkerEvent::BaseSourceReadComplete {
                task_id: identity.run_id().into(),
                item_id: identity.item_id().to_string(),
                slot: 1,
                request_elapsed_us: None,
            },
            &mut active,
            &mut settled,
        )
        .unwrap();
        assert_eq!(live.load(Ordering::SeqCst), 0);
        drop(active);
    }
}
