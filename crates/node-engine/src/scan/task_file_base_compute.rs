//! 瞬态任务文件基础 Hash 阶段的最小协调边界。
//!
//! 本模块只负责已封闭任务文件的 Hash 读取、内容缓存批量查询和 taskless 持久化 ACK。
//! Worker 媒体计算、actor 生命周期和扫描收尾由后续阶段接管。

use std::{
    collections::{BTreeMap, VecDeque},
    fmt,
    time::{SystemTime, UNIX_EPOCH},
};

use dedup_core::{ContentKey, MediaKind};
use dedup_node_store::{FileFaultKind, FileFaultRecord, ResolvedScanFile};
use dedup_windows::ReadCancellationToken;
use tokio::{sync::mpsc::UnboundedReceiver, task::JoinSet};

use super::{
    BaseComputeDecision, BaseTaskManifest, BaseTaskProduction, HashPermitReader,
    base_compute::cache_rank,
};
use crate::{
    RemoteFeatureCache,
    io::ReadFailure,
    scan::base_persistence::{
        BasePersistAck, BasePersistIdentity, BasePersistMessage, BasePersistOutcome,
        BasePersistSendError, BaseStoreHandle,
    },
    task_dispatch::{
        DispatchedTask, TaskDispatchAdmission, TaskDispatchBlockReason, TaskDispatchError,
        TaskDispatchPoll, TaskLanePermitProvider,
    },
    task_files::{TaskFileIdentity, TaskFileRecord, TaskWorkMask},
};

/// 单个 Hash 批次允许提交的最大内容键数量。
const MAX_HASH_LOOKUP_BATCH: usize = 1_000;

/// Hash 阶段结束后仍由后续 Media/收尾阶段拥有的任务文件状态。
pub(crate) struct TaskFileBaseComputePending<P: TaskLanePermitProvider> {
    /// 已封闭的任务文件 dispatcher，继续拥有所有 P/C/F 行状态。
    pub(crate) dispatcher: crate::task_dispatch::TaskFileDispatcher<P>,
    /// 任务文件身份对应的内存上下文；只保留尚未完成的行。
    pub(crate) contexts: BTreeMap<TaskFileIdentity, super::TaskFileBaseContext>,
    /// 当前扫描清单，包含 Hash ACK 后新增的 resolved 文件和命中数。
    pub(crate) manifest: BaseTaskManifest,
    /// 尚未从 dispatcher 领取的 Hash 任务行数量；Media 阶段可据此继续处理。
    pub(crate) remaining_hash_rows: usize,
    /// 本轮 Hash admission 提前停止时的明确原因；正常耗尽为 `None`。
    pub(crate) blocked_reason: Option<TaskDispatchBlockReason>,
}

impl<P: TaskLanePermitProvider> TaskFileBaseComputePending<P> {
    /// 将已暂停的 Hash 阶段状态还原为通用生产结果，便于后续阶段接管。
    pub(crate) fn from_production(production: BaseTaskProduction<P>) -> Self {
        let BaseTaskProduction {
            dispatcher,
            contexts,
            manifest,
        } = production;
        let remaining_hash_rows = contexts
            .keys()
            .filter(|identity| identity.missing().needs_md5())
            .count();
        Self {
            dispatcher,
            contexts,
            manifest,
            remaining_hash_rows,
            blocked_reason: None,
        }
    }
}

/// Hash 阶段发生任务级错误时携带剩余任务文件所有权。
pub(crate) struct TaskFileBaseComputeError<P: TaskLanePermitProvider> {
    message: String,
    pending: TaskFileBaseComputePending<P>,
}

impl<P: TaskLanePermitProvider> TaskFileBaseComputeError<P> {
    /// 消费错误并取回剩余 dispatcher、上下文和清单，供调用方 discard。
    pub(crate) fn into_pending(self) -> TaskFileBaseComputePending<P> {
        self.pending
    }
}

impl<P: TaskLanePermitProvider> fmt::Display for TaskFileBaseComputeError<P> {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(&self.message)
    }
}

impl<P: TaskLanePermitProvider> fmt::Debug for TaskFileBaseComputeError<P> {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("TaskFileBaseComputeError")
            .field("message", &self.message)
            .finish_non_exhaustive()
    }
}

/// 一条 Hash 成功结果，仍保留原始任务文件身份和行记录等待内容归并。
struct HashedTask {
    /// 任务文件返回的完整身份。
    identity: TaskFileIdentity,
    /// 原始 TSV 行；Media 续算沿用同一个身份和 item。
    record: TaskFileRecord,
    /// 本次 Hash 的 16 字节结果。
    md5: [u8; 16],
}

/// 一个并发 Hash future 的顺序化结果；JoinSet 完成顺序不代表任务文件顺序。
pub(super) struct HashReadOutcome {
    /// dispatcher 交付任务时分配的单调序号。
    pub(super) sequence: usize,
    /// 读取任务的完整文件身份。
    pub(super) identity: TaskFileIdentity,
    /// 读取任务的原始行记录。
    pub(super) record: TaskFileRecord,
    /// 读取结果；成功读取在 future 内已释放 dispatcher 交付的 permit。
    pub(super) result: Result<[u8; 16], ReadFailure>,
}

/// Hash 读取的可逐项推进运行态；每次只交付一个已经结束的读取结果。
pub(super) struct TaskFileHashRuntime {
    /// 正在读取的 Hash future；拥有 permit 直到读取 future 释放它。
    reads: JoinSet<HashReadOutcome>,
    /// 尚未由 dispatcher 领取的 Hash 行数。
    unclaimed_rows: usize,
    /// 为完成顺序无关的后续批处理分配稳定序号。
    next_sequence: usize,
    /// 每条读取的取消令牌；取消时先通知 reader 自行收束 permit。
    cancellations: Vec<ReadCancellationToken>,
}

impl TaskFileHashRuntime {
    /// 用当前未领取的 Hash 行数量创建空运行态。
    pub(super) fn new(remaining_hash_rows: usize) -> Self {
        Self {
            reads: JoinSet::new(),
            unclaimed_rows: remaining_hash_rows,
            next_sequence: 0,
            cancellations: Vec::new(),
        }
    }

    /// 返回当前窗口能否再领取一条 Hash 读取任务。
    pub(super) fn can_dispatch(&self, hash_capacity: usize) -> bool {
        self.unclaimed_rows > 0 && self.reads.len() < hash_capacity
    }

    /// 启动一个已领取的 Hash 读取，并把读取 permit 的释放限制在 future 内。
    pub(super) fn spawn<P, H>(
        &mut self,
        task: DispatchedTask<P>,
        reader: H,
        cancellation: ReadCancellationToken,
    ) -> Result<(), String>
    where
        P: Send + 'static,
        H: HashPermitReader<Permit = P>,
    {
        if self.unclaimed_rows == 0 {
            return Err("Hash 运行态没有可领取的任务行".into());
        }
        if task.record.known_md5.is_some() || !task.record.missing.needs_md5() {
            return Err("Hash 运行态收到非 Hash 任务行".into());
        }
        self.unclaimed_rows -= 1;
        let sequence = self.next_sequence;
        self.next_sequence += 1;
        let identity = task.identity;
        let record = task.record;
        let scanned = record.scanned.clone();
        self.cancellations.push(cancellation.clone());
        self.reads.spawn(async move {
            let result = reader
                .read_with_permit(scanned, task.permit, cancellation, None)
                .await
                .map(|product| {
                    let md5 = product.md5;
                    // Hash 完成即释放读取许可，SQLite 查询和 ACK 不占据磁盘窗口。
                    drop(product.lease);
                    md5
                });
            HashReadOutcome {
                sequence,
                identity,
                record,
                result,
            }
        });
        Ok(())
    }

    /// 等待并返回恰好一条 Hash 读取结果，不会排空其它已在窗口内的 future。
    pub(super) async fn join_one(&mut self) -> Result<HashReadOutcome, String> {
        let joined = self
            .reads
            .join_next()
            .await
            .ok_or_else(|| "Hash 运行态没有在途读取".to_owned())?;
        joined.map_err(|error| format!("Hash 读取 future 异常结束: {error}"))
    }

    /// 返回仍在读取窗口内的 future 数量。
    pub(super) fn active_len(&self) -> usize {
        self.reads.len()
    }

    /// 所有 Hash 行均已领取并且所有读取 future 都已结束。
    pub(super) fn is_finished(&self) -> bool {
        self.unclaimed_rows == 0 && self.reads.is_empty()
    }

    /// 请求读取自行取消并回收所有在途 future，确保 permit 在返回前已经释放。
    pub(super) async fn cancel_and_join(&mut self) {
        for cancellation in &self.cancellations {
            cancellation.cancel();
        }
        while self.reads.join_next().await.is_some() {}
        self.cancellations.clear();
    }
}

/// 已投递但尚未收到 SQLite ACK 的持久化动作。
enum PersistAction {
    /// 已有完整内容缓存，只需补当前位置并把任务行置 C。
    Complete {
        scanned: dedup_node_store::ScannedPath,
        content_key: ContentKey,
    },
    /// 文件读取失败，ACK 后把任务行置 F。
    Failed,
}

/// 待投递的拥有型消息及其 ACK 后动作。
struct PendingPersist {
    /// 消息对应的完整任务文件身份。
    identity: TaskFileIdentity,
    /// 仍由队列或 actor 持有的消息。
    message: BasePersistMessage,
    /// ACK 应用时执行的任务文件状态迁移。
    action: PersistAction,
}

/// 运行已封闭基础任务的 Hash 批处理阶段。
///
/// 已知 MD5 的 Media 行不会在本阶段申请 permit；Hash 读取完成后只通过一次内容键批量
/// 查询决定“直接完成”或“沿同一身份保留 P 进入 Media”。所有 C/F 迁移都在对应 ACK 后发生。
pub(crate) async fn run_task_file_base_compute<P, H>(
    production: BaseTaskProduction<P>,
    reader: H,
    hash_capacity: usize,
    store: &BaseStoreHandle,
    acknowledgements: &mut UnboundedReceiver<BasePersistAck>,
    cancellation: ReadCancellationToken,
) -> Result<TaskFileBaseComputePending<P>, TaskFileBaseComputeError<P>>
where
    P: TaskLanePermitProvider,
    H: HashPermitReader<Permit = P::Permit>,
{
    run_task_file_hash_pass(
        TaskFileBaseComputePending::from_production(production),
        reader,
        hash_capacity,
        store,
        acknowledgements,
        cancellation,
    )
    .await
}

/// 运行一个已存在 pending 的 Hash pass；不会把 Hash→Media 续算重新计入 Hash。
///
/// 每次调用都从 pending 的 `remaining_hash_rows` 开始，并清除上轮 admission 阻塞原因。
/// 遇到取消或任务级错误时，先取消并等待所有在途 Hash future，再把完整所有权返回给调用方。
pub(crate) async fn run_task_file_hash_pass<P, H>(
    pending: TaskFileBaseComputePending<P>,
    reader: H,
    hash_capacity: usize,
    store: &BaseStoreHandle,
    acknowledgements: &mut UnboundedReceiver<BasePersistAck>,
    cancellation: ReadCancellationToken,
) -> Result<TaskFileBaseComputePending<P>, TaskFileBaseComputeError<P>>
where
    P: TaskLanePermitProvider,
    H: HashPermitReader<Permit = P::Permit>,
{
    let remote = crate::DisabledRemoteFeatureCache;
    let mut remote_available = false;
    let mut warning = None;
    run_task_file_hash_pass_with_remote(
        pending,
        reader,
        hash_capacity,
        store,
        acknowledgements,
        cancellation,
        &remote,
        &mut remote_available,
        &mut warning,
    )
    .await
}

/// 运行支持可选远端内容缓存的 Hash pass；旧入口保持 SQLite-only 行为。
pub(crate) async fn run_task_file_hash_pass_with_remote<P, H, R>(
    pending: TaskFileBaseComputePending<P>,
    reader: H,
    hash_capacity: usize,
    store: &BaseStoreHandle,
    acknowledgements: &mut UnboundedReceiver<BasePersistAck>,
    cancellation: ReadCancellationToken,
    remote: &R,
    remote_available: &mut bool,
    warning: &mut Option<String>,
) -> Result<TaskFileBaseComputePending<P>, TaskFileBaseComputeError<P>>
where
    P: TaskLanePermitProvider,
    H: HashPermitReader<Permit = P::Permit>,
    R: RemoteFeatureCache,
{
    if *remote_available {
        if let Some(startup_warning) = remote.startup_warning() {
            *remote_available = false;
            if warning.is_none() {
                *warning = Some(startup_warning.to_owned());
            }
        }
    }
    let mut pending = pending;
    // pending 可能来自上一轮 Media 阶段；Hash pass 必须重新观察 dispatcher，不能沿用旧阻塞原因。
    pending.blocked_reason = None;
    let TaskFileBaseComputePending {
        dispatcher,
        contexts,
        manifest,
        remaining_hash_rows,
        blocked_reason: _,
    } = pending;
    if hash_capacity == 0 {
        return Err(task_error(
            TaskFileBaseComputePending {
                dispatcher,
                contexts,
                manifest,
                remaining_hash_rows,
                blocked_reason: None,
            },
            "基础 Hash 读取容量必须大于 0",
        ));
    }

    // dispatcher 只负责按 lane 交付 permit；读取 future 放入 JoinSet 后，下一次
    // permit 等待与已有文件读取会由同一个事件循环同时推进。
    let mut dispatcher = dispatcher;
    let contexts = contexts;
    let manifest = manifest;
    let mut runtime = TaskFileHashRuntime::new(remaining_hash_rows);
    let mut outcomes = Vec::<HashReadOutcome>::new();
    let mut stop_reason = None;
    let mut hash_rows_remaining = remaining_hash_rows;
    let mut fatal_error = None;

    'event_loop: while hash_rows_remaining > 0 || runtime.active_len() > 0 {
        if stop_reason.is_none() && hash_rows_remaining > 0 && runtime.can_dispatch(hash_capacity) {
            tokio::select! {
                result = dispatcher.next_with_admission(
                    cancellation.clone(),
                    TaskDispatchAdmission::hash_only(),
                ) => {
                    match result {
                        Ok(TaskDispatchPoll::Task(task)) => {
                            if task.record.known_md5.is_some() || !task.record.missing.needs_md5() {
                                fatal_error = Some("Hash admission 派发了已知 MD5 任务".into());
                                break 'event_loop;
                            }
                            let identity = task.identity.clone();
                            if !contexts.contains_key(&identity) {
                                fatal_error = Some("Hash 任务缺少对应的内存上下文".into());
                                break 'event_loop;
                            }
                            hash_rows_remaining -= 1;
                            if let Err(message) = runtime.spawn(task, reader.clone(), cancellation.clone()) {
                                fatal_error = Some(message);
                                break 'event_loop;
                            }
                        }
                        Ok(TaskDispatchPoll::Blocked(reason)) => {
                            stop_reason = Some(reason);
                        }
                        Ok(TaskDispatchPoll::Drained) => {
                            fatal_error = Some("Hash dispatcher 在剩余 Hash 行前耗尽".into());
                            break 'event_loop;
                        }
                        Err(error) => {
                            fatal_error = Some(dispatch_error_message(error));
                            break 'event_loop;
                        }
                    }
                }
                joined = runtime.join_one(), if runtime.active_len() > 0 => {
                    match joined {
                        Ok(outcome) => outcomes.push(outcome),
                        Err(message) => {
                            fatal_error = Some(message);
                            break 'event_loop;
                        }
                    }
                }
            }
        } else if runtime.active_len() > 0 {
            match runtime.join_one().await {
                Ok(outcome) => outcomes.push(outcome),
                Err(message) => {
                    fatal_error = Some(message);
                    break 'event_loop;
                }
            }
        } else {
            break;
        }
    }

    // 所有 dispatcher 轮询 future 已经释放，下面重新组合 pending 交给 ACK/Media
    // 处理函数；这也确保任何事件循环错误都能携带完整所有权返回。
    if fatal_error.is_some() {
        cancellation.cancel();
        runtime.cancel_and_join().await;
    }
    let mut pending = TaskFileBaseComputePending {
        dispatcher,
        contexts,
        manifest,
        remaining_hash_rows,
        blocked_reason: stop_reason,
    };
    if let Some(message) = fatal_error {
        return Err(task_error(pending, message));
    }

    // 读取完成顺序可能不同于 dispatcher 顺序；按派发序号恢复输入顺序后再做批量查询。
    outcomes.sort_by_key(|outcome| outcome.sequence);
    let mut persist_queue = VecDeque::<PendingPersist>::new();
    let mut persist_in_flight = BTreeMap::<TaskFileIdentity, PersistAction>::new();
    let mut hashed_batch = Vec::with_capacity(MAX_HASH_LOOKUP_BATCH);
    for outcome in outcomes {
        match outcome.result {
            Ok(md5) => {
                hashed_batch.push(HashedTask {
                    identity: outcome.identity,
                    record: outcome.record,
                    md5,
                });
                if hashed_batch.len() == MAX_HASH_LOOKUP_BATCH {
                    if let Err(message) = apply_hash_batch(
                        &mut pending,
                        &mut hashed_batch,
                        &mut persist_queue,
                        &mut persist_in_flight,
                        store,
                        acknowledgements,
                        Some(remote),
                        remote_available,
                        warning,
                    )
                    .await
                    {
                        return Err(task_error(pending, message));
                    }
                }
            }
            Err(ReadFailure::Cancelled) => {
                return Err(task_error(pending, "基础 Hash 阶段已取消"));
            }
            Err(error) => {
                persist_queue.push_back(failed_persist(
                    outcome.identity,
                    outcome.record.scanned,
                    error,
                ));
            }
        }
    }
    if let Err(message) = apply_hash_batch(
        &mut pending,
        &mut hashed_batch,
        &mut persist_queue,
        &mut persist_in_flight,
        store,
        acknowledgements,
        Some(remote),
        remote_available,
        warning,
    )
    .await
    {
        return Err(task_error(pending, message));
    }
    if let Err(message) = flush_persist_queue(
        &mut pending,
        &mut persist_queue,
        &mut persist_in_flight,
        store,
        acknowledgements,
    )
    .await
    {
        return Err(task_error(pending, message));
    }
    pending.remaining_hash_rows = hash_rows_remaining;
    pending.blocked_reason = stop_reason;
    Ok(pending)
}

/// 取消并等待所有在途 Hash future，确保读取许可在返回 pending 前已经释放。
async fn cancel_and_join_hash_reads(
    reads: &mut JoinSet<HashReadOutcome>,
    cancellation: &ReadCancellationToken,
) {
    if reads.is_empty() {
        return;
    }
    cancellation.cancel();
    while reads.join_next().await.is_some() {}
}

/// 收集一个读取 future 的结果；JoinSet 异常必须升级为任务级错误。
fn collect_hash_read(
    joined: Option<Result<HashReadOutcome, tokio::task::JoinError>>,
    outcomes: &mut Vec<HashReadOutcome>,
) -> Result<(), String> {
    let joined = joined.ok_or_else(|| "Hash 读取集合提前为空".to_owned())?;
    let outcome = joined.map_err(|error| format!("Hash 读取 future 异常结束: {error}"))?;
    outcomes.push(outcome);
    Ok(())
}

/// 对一批已完成 Hash 的结果做本地/远端一次批量查询并登记后续动作。
async fn apply_hash_batch<P, R>(
    pending: &mut TaskFileBaseComputePending<P>,
    hashed: &mut Vec<HashedTask>,
    persist_queue: &mut VecDeque<PendingPersist>,
    persist_in_flight: &mut BTreeMap<TaskFileIdentity, PersistAction>,
    store: &BaseStoreHandle,
    acknowledgements: &mut UnboundedReceiver<BasePersistAck>,
    remote: Option<&R>,
    remote_available: &mut bool,
    warning: &mut Option<String>,
) -> Result<(), String>
where
    P: TaskLanePermitProvider,
    R: RemoteFeatureCache,
{
    if hashed.is_empty() {
        return Ok(());
    }
    let batch = std::mem::take(hashed);
    let keys = batch
        .iter()
        .map(|item| ContentKey::new(item.md5, item.record.scanned.file_size))
        .collect::<Vec<_>>();
    let mut cached = store
        .lookup_base_cache_by_keys(&keys)
        .map_err(|error| error.to_string())?;
    if cached.len() != batch.len() {
        return Err("内容缓存批量查询返回数量不一致".into());
    }

    // 只把本地仍缺少基础字段的项送入一次远端批量查询；完整本地命中不占用网络请求。
    let mut remote_indexes = Vec::new();
    if let Some(remote) = remote.filter(|_| *remote_available) {
        for (index, (hashed, local)) in batch.iter().zip(&cached).enumerate() {
            let context = pending
                .contexts
                .get(&hashed.identity)
                .ok_or_else(|| "Hash 结果缺少对应的内存上下文".to_owned())?;
            let decision = BaseComputeDecision::for_cache(
                local.as_ref(),
                context.contact_sheet_valid,
                context.force_recompute,
            );
            if !context.force_recompute && decision.missing_parts() != 0 {
                remote_indexes.push(index);
            }
        }
        if !remote_indexes.is_empty() {
            let remote_keys = remote_indexes
                .iter()
                .map(|&index| keys[index])
                .collect::<Vec<_>>();
            match remote.lookup_contents(&remote_keys).await {
                Ok(remote_records)
                    if remote_records.len() == remote_keys.len()
                        && remote_records
                            .iter()
                            .zip(&remote_keys)
                            .all(|(record, key)| {
                                record
                                    .as_ref()
                                    .is_none_or(|record| record.content_key == *key)
                            }) =>
                {
                    let mut imported = false;
                    for (&index, remote_record) in remote_indexes.iter().zip(remote_records) {
                        if remote_record.as_ref().is_some_and(|record| {
                            cache_rank(Some(record)) > cache_rank(cached[index].as_ref())
                        }) {
                            store
                                .import_base_cache_record(
                                    &batch[index].record.scanned,
                                    remote_record.as_ref().expect("上方已检查 Some"),
                                )
                                .map_err(|error| error.to_string())?;
                            imported = true;
                        }
                    }
                    if imported {
                        // 远端批量导入完成后只重新查询一次，取得本地 content_id 和合并字段。
                        cached = store
                            .lookup_base_cache_by_keys(&keys)
                            .map_err(|error| error.to_string())?;
                        if cached.len() != batch.len() {
                            return Err("导入后的内容缓存批量查询返回数量不一致".into());
                        }
                    }
                }
                Ok(_) => note_remote_failure(
                    remote_available,
                    warning,
                    "远端内容缓存返回数量或内容键不一致".into(),
                ),
                Err(error) => note_remote_failure(
                    remote_available,
                    warning,
                    format!("远端内容缓存查询失败，本轮降级为 SQLite-only: {error}"),
                ),
            }
        }
    }

    for (hashed, cached) in batch.into_iter().zip(cached) {
        let Some(context) = pending.contexts.get_mut(&hashed.identity) else {
            return Err("Hash 结果缺少对应的内存上下文".into());
        };
        context.cached = cached.clone();
        context.content_id = cached.as_ref().and_then(|record| record.content_id);
        let decision = BaseComputeDecision::for_cache(
            cached.as_ref(),
            context.contact_sheet_valid,
            context.force_recompute,
        );
        if decision.missing_parts() == 0 {
            let Some(cached) = cached else {
                return Err("完整内容缓存缺少记录".into());
            };
            let action = PersistAction::Complete {
                scanned: hashed.record.scanned.clone(),
                content_key: cached.content_key,
            };
            persist_queue.push_back(complete_persist(
                hashed.identity,
                hashed.record.scanned,
                hashed.md5,
                cached.media_kind,
                action,
            ));
        } else {
            let Some(missing) = TaskWorkMask::for_base(false, decision.missing_parts()) else {
                return Err("Hash 后基础缺失掩码无效".into());
            };
            let media_record = TaskFileRecord {
                item_id: hashed.record.item_id,
                work_kind: hashed.record.work_kind,
                scanned: hashed.record.scanned,
                known_md5: Some(hashed.md5),
                missing,
            };
            pending
                .dispatcher
                .request_media_continuation(&hashed.identity, &media_record)
                .map_err(|error| error.to_string())?;
        }
    }
    flush_persist_queue(
        pending,
        persist_queue,
        persist_in_flight,
        store,
        acknowledgements,
    )
    .await
}

/// 记录远端失败并一次性关闭本轮远端入口，后续批次继续 SQLite-only。
fn note_remote_failure(remote_available: &mut bool, warning: &mut Option<String>, message: String) {
    *remote_available = false;
    if warning.is_none() {
        *warning = Some(message);
    }
}

/// 将单文件读取失败包装成只在 ACK 后写 F 的消息。
fn failed_persist(
    identity: TaskFileIdentity,
    scanned: dedup_node_store::ScannedPath,
    error: ReadFailure,
) -> PendingPersist {
    let message = error.to_string();
    let fault_normalized_path = scanned.normalized_path.clone();
    let fault_display_path = scanned.display_path.clone();
    let fault_file_size = scanned.file_size;
    let (windows_error_code, read_offset, read_size) = match error {
        ReadFailure::SuspectedPhysical {
            block_offset,
            block_len,
            raw_os_error,
            ..
        } => (raw_os_error, Some(block_offset), Some(block_len as u64)),
        ReadFailure::Io {
            block_offset,
            source,
            ..
        } => (source.raw_os_error(), Some(block_offset), None),
        ReadFailure::Cancelled => (None, None, None),
    };
    let fault_message = message.clone();
    let fault_seen_at_ms = now_ms();
    let display_path = scanned
        .display_path
        .as_path()
        .to_string_lossy()
        .into_owned();
    let operation_message = message.clone();
    let operation = BasePersistMessage::new_task_file(identity.clone(), move |_store| {
        _store
            .upsert_file_fault(&FileFaultRecord {
                machine_id: _store.machine_id().clone(),
                normalized_path: fault_normalized_path,
                display_path: fault_display_path,
                file_size: fault_file_size,
                kind: FileFaultKind::SuspectedPhysicalRead,
                stage: "base".to_owned(),
                windows_error_code,
                read_offset,
                read_size,
                worker_pid: None,
                worker_exit_code: None,
                first_seen_at_ms: fault_seen_at_ms,
                last_seen_at_ms: fault_seen_at_ms,
                occurrence_count: 1,
                message: fault_message,
            })
            .map_err(|error| error.to_string())?;
        Ok(BasePersistOutcome::Failed {
            display_path,
            message: operation_message,
            worker_slot: None,
            skipped_incomplete: false,
        })
    });
    PendingPersist {
        identity,
        message: operation,
        action: PersistAction::Failed,
    }
}

/// 返回用于故障表的当前毫秒时间戳。
fn now_ms() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis()
        .try_into()
        .unwrap_or(u64::MAX)
}

/// 将完整内容缓存命中包装成 taskless upsert 消息。
fn complete_persist(
    identity: TaskFileIdentity,
    scanned: dedup_node_store::ScannedPath,
    md5: [u8; 16],
    media_kind: MediaKind,
    action: PersistAction,
) -> PendingPersist {
    let operation_scanned = scanned.clone();
    let operation = BasePersistMessage::new_task_file(identity.clone(), move |store| {
        store
            .upsert_content_and_location(&operation_scanned, md5, media_kind)
            .map(|_| BasePersistOutcome::Succeeded {
                worker_slot: None,
                cache_hit: true,
                media_kind,
                file_size: operation_scanned.file_size,
            })
            .map_err(|error| error.to_string())
    });
    PendingPersist {
        identity,
        message: operation,
        action,
    }
}

/// 尝试投递待持久化消息，并在队列满时消费一个真实 ACK 后重试。
async fn flush_persist_queue<P: TaskLanePermitProvider>(
    pending: &mut TaskFileBaseComputePending<P>,
    queue: &mut VecDeque<PendingPersist>,
    in_flight: &mut BTreeMap<TaskFileIdentity, PersistAction>,
    store: &BaseStoreHandle,
    acknowledgements: &mut UnboundedReceiver<BasePersistAck>,
) -> Result<(), String> {
    while let Some(mut item) = queue.pop_front() {
        let identity = item.identity.clone();
        match store.try_persist(item.message) {
            Ok(()) => {
                in_flight.insert(identity, item.action);
            }
            Err(BasePersistSendError::Full(message)) => {
                item.message = message;
                queue.push_front(item);
                if in_flight.is_empty() {
                    return Err("持久化队列已满且没有可消费的 ACK".into());
                }
                apply_one_ack(pending, in_flight, acknowledgements).await?;
            }
            Err(BasePersistSendError::Closed(_message)) => {
                return Err("基础持久化 actor 已关闭".into());
            }
        }
    }
    while !in_flight.is_empty() {
        apply_one_ack(pending, in_flight, acknowledgements).await?;
    }
    Ok(())
}

/// 消费一条 ACK，只有对应 SQLite 操作成功后才迁移任务文件状态。
async fn apply_one_ack<P: TaskLanePermitProvider>(
    pending: &mut TaskFileBaseComputePending<P>,
    in_flight: &mut BTreeMap<TaskFileIdentity, PersistAction>,
    acknowledgements: &mut UnboundedReceiver<BasePersistAck>,
) -> Result<(), String> {
    let ack = acknowledgements
        .recv()
        .await
        .ok_or_else(|| "基础持久化 actor 未返回 ACK".to_owned())?;
    let identity = match ack.identity {
        BasePersistIdentity::TaskFile(identity) => identity,
        BasePersistIdentity::Legacy(_) => {
            return Err("基础任务文件收到旧任务表持久化 ACK".into());
        }
    };
    let action = in_flight
        .remove(&identity)
        .ok_or_else(|| "收到未知任务文件持久化 ACK".to_owned())?;
    let result = ack.result?;
    match (action, result) {
        (
            PersistAction::Complete {
                scanned,
                content_key,
            },
            BasePersistOutcome::Succeeded { .. },
        ) => {
            pending
                .dispatcher
                .mark_completed(&identity)
                .map_err(|error| error.to_string())?;
            pending.contexts.remove(&identity);
            pending.manifest.resolved_files.push(ResolvedScanFile {
                scanned,
                content: content_key,
            });
            pending.manifest.resolved_files.sort_by(|left, right| {
                left.scanned
                    .normalized_path
                    .cmp(&right.scanned.normalized_path)
            });
            pending.manifest.cache_hits += 1;
        }
        (PersistAction::Failed, BasePersistOutcome::Failed { .. }) => {
            pending
                .dispatcher
                .mark_failed(&identity)
                .map_err(|error| error.to_string())?;
            pending.contexts.remove(&identity);
        }
        (_, BasePersistOutcome::Ignored) => {
            return Err("任务文件持久化 ACK 被忽略".into());
        }
        (_, BasePersistOutcome::Cancelled { .. }) => {
            return Err("任务文件 Hash 阶段收到取消 ACK".into());
        }
        (_, BasePersistOutcome::Succeeded { .. }) => {
            return Err("任务文件持久化 ACK 成功类型不匹配".into());
        }
        (_, BasePersistOutcome::Failed { .. }) => {
            return Err("任务文件持久化 ACK 失败类型不匹配".into());
        }
    }
    Ok(())
}

/// 把 dispatcher 错误转为保留 pending 所有权的任务级文本。
fn dispatch_error_message(error: TaskDispatchError) -> String {
    format!("任务文件 Hash 分发失败: {error}")
}

/// 构造带有剩余任务所有权的任务级错误。
fn task_error<P: TaskLanePermitProvider>(
    pending: TaskFileBaseComputePending<P>,
    message: impl Into<String>,
) -> TaskFileBaseComputeError<P> {
    TaskFileBaseComputeError {
        message: message.into(),
        pending,
    }
}

#[cfg(test)]
mod tests {
    use std::{
        collections::BTreeMap,
        future::Future,
        path::{Path, PathBuf},
        pin::Pin,
        sync::{
            Arc,
            atomic::{AtomicIsize, AtomicUsize, Ordering},
        },
        time::Duration,
    };

    use dedup_core::{ContentKey, DisplayPath, MachineId, MediaKind, NormalizedPath};
    use dedup_node_store::{BaseCacheRecord, NodeStore, ScannedPath};
    use dedup_windows::{LocalDiskKind, PhysicalDiskId, ReadCancellationToken};
    use tokio::{
        sync::{Barrier, Notify},
        time::timeout,
    };

    use crate::{
        RemoteCacheError, RemoteFeatureCache,
        io::{DiskReadClass, ReadFailure},
        scan::{
            BaseTaskInput, BaseTaskProducer, BaseTaskProduction, HashPermitReader,
            PlannedScannedPath, ReadProduct, TaskDiskLane,
        },
        task_dispatch::{
            DispatchedTask, TaskDispatchBlockReason, TaskFileDispatcher, TaskLanePermitFuture,
            TaskLanePermitProvider,
        },
        task_files::{
            TaskFileIdentity, TaskFileRecord, TaskWorkKind, TaskWorkMask, TransientTaskFileSet,
        },
    };

    use super::{
        TaskFileBaseComputePending, TaskFileHashRuntime, run_task_file_base_compute,
        run_task_file_hash_pass,
    };

    const RUN_ID: &str = "01900000-0000-7000-8000-000000000101";

    #[derive(Clone, Copy, Debug)]
    struct TestPermit;

    #[derive(Clone, Default)]
    struct CountingPermitProvider {
        acquires: Arc<AtomicUsize>,
    }

    impl TaskLanePermitProvider for CountingPermitProvider {
        type Permit = TestPermit;

        fn acquire(
            &self,
            _lane: TaskDiskLane,
            _class: DiskReadClass,
            _cancellation: ReadCancellationToken,
        ) -> TaskLanePermitFuture<Self::Permit> {
            self.acquires.fetch_add(1, Ordering::SeqCst);
            Box::pin(async { Ok(TestPermit) })
        }
    }

    #[derive(Clone, Default)]
    struct TestHashReader {
        results: Arc<BTreeMap<String, Result<[u8; 16], String>>>,
    }

    #[derive(Clone)]
    struct TestContentRemote {
        calls: Arc<AtomicUsize>,
        hit: Option<BaseCacheRecord>,
        fail: bool,
    }

    impl RemoteFeatureCache for TestContentRemote {
        async fn lookup_paths(
            &self,
            _machine_id: &MachineId,
            paths: &[ScannedPath],
        ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
            Ok(vec![None; paths.len()])
        }

        async fn lookup_contents(
            &self,
            keys: &[ContentKey],
        ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            if self.fail {
                return Err(RemoteCacheError::ConnectTimeout);
            }
            Ok(keys.iter().map(|_| self.hit.clone()).collect())
        }

        async fn publish_outbox(
            &mut self,
            _machine_id: &MachineId,
            _batch: &dedup_protocol::proto::SyncChangeBatch,
        ) -> Result<u64, RemoteCacheError> {
            Ok(0)
        }
    }

    impl HashPermitReader for TestHashReader {
        type Permit = TestPermit;

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
            let result = self
                .results
                .get(scanned.normalized_path.as_str())
                .cloned()
                .unwrap_or_else(|| Err("测试读取器缺少路径".into()));
            let path = scanned.display_path.as_path().to_path_buf();
            let file_size = scanned.file_size;
            Box::pin(async move {
                match result {
                    Ok(md5) => Ok(ReadProduct { md5, lease: permit }),
                    Err(message) => {
                        let _ = permit;
                        Err(ReadFailure::Io {
                            path,
                            block_offset: 0,
                            source: std::io::Error::other(message),
                        })
                    }
                }
                .map_err(|error| match error {
                    ReadFailure::Io {
                        path,
                        block_offset,
                        source,
                    } => ReadFailure::Io {
                        path,
                        block_offset: block_offset.min(file_size),
                        source,
                    },
                    other => other,
                })
            })
        }
    }

    /// 一个首项立即完成、次项等待显式放行的 Hash 读取器。
    #[derive(Clone)]
    struct GatedHashReader {
        /// 次项等待的测试门闩。
        gate: Arc<Notify>,
    }

    impl HashPermitReader for GatedHashReader {
        type Permit = TestPermit;

        fn read_with_permit(
            &self,
            scanned: ScannedPath,
            permit: Self::Permit,
            cancellation: ReadCancellationToken,
            _started: Option<crate::scan::HashReadStartedSignal>,
        ) -> Pin<
            Box<
                dyn Future<Output = Result<ReadProduct<Self::Permit>, ReadFailure>>
                    + Send
                    + 'static,
            >,
        > {
            let first = scanned
                .normalized_path
                .as_str()
                .to_ascii_lowercase()
                .ends_with("runtime-first.bin");
            let gate = Arc::clone(&self.gate);
            Box::pin(async move {
                if first {
                    return Ok(ReadProduct {
                        md5: [0x41; 16],
                        lease: permit,
                    });
                }
                loop {
                    if cancellation.is_cancelled() {
                        let _ = permit;
                        return Err(ReadFailure::Cancelled);
                    }
                    tokio::select! {
                        _ = gate.notified() => return Ok(ReadProduct { md5: [0x42; 16], lease: permit }),
                        _ = tokio::time::sleep(Duration::from_millis(2)) => {}
                    }
                }
            })
        }
    }

    /// 构造两条已经领取的 Hash 行，验证运行态不会为了第一条结果排空整个窗口。
    fn two_dispatched_hash_tasks() -> (DispatchedTask<TestPermit>, DispatchedTask<TestPermit>) {
        let lane = lane();
        let missing = TaskWorkMask::for_base(true, 0).expect("Hash 行必须携带 needs_md5");
        let first_item = uuid::Uuid::parse_str("01900000-0000-7000-8000-000000000111").unwrap();
        let second_item = uuid::Uuid::parse_str("01900000-0000-7000-8000-000000000112").unwrap();
        let task = |item_id, offset, path| DispatchedTask {
            identity: TaskFileIdentity::new(RUN_ID, &lane, item_id, offset, 80, missing).unwrap(),
            record: TaskFileRecord {
                item_id,
                work_kind: TaskWorkKind::Base,
                scanned: scanned(path, 16),
                known_md5: None,
                missing,
            },
            class: DiskReadClass::HashSequential,
            permit: TestPermit,
            continuation: false,
        };
        (
            task(first_item, 0, r"C:\runtime-first.bin"),
            task(second_item, 80, r"C:\runtime-second.bin"),
        )
    }

    #[tokio::test]
    async fn hash_runtime_returns_one_ready_item_without_draining_window() {
        let gate = Arc::new(Notify::new());
        let mut runtime = TaskFileHashRuntime::new(2);
        let (first, second) = two_dispatched_hash_tasks();
        let reader = GatedHashReader {
            gate: Arc::clone(&gate),
        };
        runtime
            .spawn(first, reader.clone(), ReadCancellationToken::new())
            .unwrap();
        runtime
            .spawn(second, reader, ReadCancellationToken::new())
            .unwrap();

        let first = timeout(Duration::from_secs(1), runtime.join_one())
            .await
            .unwrap()
            .unwrap();
        assert_eq!(first.result.unwrap(), [0x41; 16]);
        assert_eq!(runtime.active_len(), 1);
        gate.notify_waiters();
        runtime.cancel_and_join().await;
    }

    /// 让两个真实 Hash future 在释放前都进入读取，用于验证显式并发上限。
    #[derive(Clone)]
    struct ConcurrentHashReader {
        results: Arc<BTreeMap<String, [u8; 16]>>,
        entered: Arc<AtomicUsize>,
        barrier: Arc<Barrier>,
    }

    impl HashPermitReader for ConcurrentHashReader {
        type Permit = TestPermit;

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
            let key = scanned.normalized_path.as_str().to_owned();
            let path = scanned.display_path.as_path().to_path_buf();
            let result = self.results.get(&key).copied();
            let entered = Arc::clone(&self.entered);
            let barrier = Arc::clone(&self.barrier);
            Box::pin(async move {
                entered.fetch_add(1, Ordering::SeqCst);
                barrier.wait().await;
                match result {
                    Some(md5) => Ok(ReadProduct { md5, lease: permit }),
                    None => {
                        let _ = permit;
                        Err(ReadFailure::Io {
                            path,
                            block_offset: 0,
                            source: std::io::Error::other("测试读取器缺少路径"),
                        })
                    }
                }
            })
        }
    }

    /// 取消感知的阻塞读取器，用于确认 Hash pass 返回前已收束所有读取 future。
    #[derive(Clone)]
    struct CancellationAwareHashReader {
        entered: Arc<AtomicUsize>,
        cancellation_seen: Arc<AtomicUsize>,
        slow_sequence: Option<usize>,
    }

    impl HashPermitReader for CancellationAwareHashReader {
        type Permit = TrackedPermit;

        fn read_with_permit(
            &self,
            _scanned: ScannedPath,
            permit: Self::Permit,
            cancellation: ReadCancellationToken,
            _started: Option<crate::scan::HashReadStartedSignal>,
        ) -> Pin<
            Box<
                dyn Future<Output = Result<ReadProduct<Self::Permit>, ReadFailure>>
                    + Send
                    + 'static,
            >,
        > {
            let sequence = self.entered.fetch_add(1, Ordering::SeqCst);
            let cancellation_seen = Arc::clone(&self.cancellation_seen);
            let slow = self.slow_sequence == Some(sequence);
            Box::pin(async move {
                loop {
                    if cancellation.is_cancelled() {
                        cancellation_seen.fetch_add(1, Ordering::SeqCst);
                        drop(permit);
                        return Err(ReadFailure::Cancelled);
                    }
                    tokio::time::sleep(if slow {
                        Duration::from_millis(150)
                    } else {
                        Duration::from_millis(2)
                    })
                    .await;
                }
            })
        }
    }

    /// 只在前两次取得许可，第三次返回任务级读取错误的 provider。
    #[derive(Clone)]
    struct ErrorAfterPermitProvider {
        attempts: Arc<AtomicUsize>,
        active: Arc<AtomicIsize>,
        successful_acquires: usize,
    }

    impl TaskLanePermitProvider for ErrorAfterPermitProvider {
        type Permit = TrackedPermit;

        fn acquire(
            &self,
            _lane: TaskDiskLane,
            _class: DiskReadClass,
            _cancellation: ReadCancellationToken,
        ) -> TaskLanePermitFuture<Self::Permit> {
            let attempt = self.attempts.fetch_add(1, Ordering::SeqCst);
            let active = Arc::clone(&self.active);
            let successful_acquires = self.successful_acquires;
            Box::pin(async move {
                if attempt >= successful_acquires {
                    return Err(ReadFailure::Io {
                        path: PathBuf::from(r"C:\hash-dispatch-error.bin"),
                        block_offset: 0,
                        source: std::io::Error::other("测试 dispatcher 读取许可失败"),
                    });
                }
                active.fetch_add(1, Ordering::SeqCst);
                Ok(TrackedPermit { active })
            })
        }
    }

    /// 能观察 permit 是否已在 Hash future 结束时释放的读取许可。
    #[derive(Clone)]
    struct TrackedPermit {
        active: Arc<AtomicIsize>,
    }

    impl Drop for TrackedPermit {
        fn drop(&mut self) {
            self.active.fetch_sub(1, Ordering::SeqCst);
        }
    }

    fn lane() -> TaskDiskLane {
        TaskDiskLane {
            physical_disk_id: PhysicalDiskId::from_disk_numbers([7]).unwrap(),
            physical_disk_numbers: vec![7],
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

    fn production<P: TaskLanePermitProvider>(
        root: &Path,
        inputs: &[BaseTaskInput],
        provider: P,
    ) -> BaseTaskProduction<P> {
        let files = TransientTaskFileSet::create(root, RUN_ID).unwrap();
        let mut producer = BaseTaskProducer::new(TaskFileDispatcher::new(files, provider));
        producer.append_batch(inputs).unwrap();
        producer.seal().unwrap()
    }

    fn seed_record(
        store: &mut NodeStore,
        reference_path: &str,
        md5: [u8; 16],
        file_size: u64,
        complete: bool,
    ) -> BaseCacheRecord {
        let path = scanned(reference_path, file_size);
        let content = store
            .upsert_content_and_location(&path, md5, MediaKind::Other)
            .unwrap();
        if complete {
            store.mark_base_complete(content.id).unwrap();
        }
        store.load_base_cache_record(content.id).unwrap()
    }

    fn reader(results: &[(&str, Result<[u8; 16], &str>)]) -> TestHashReader {
        TestHashReader {
            results: Arc::new(
                results
                    .iter()
                    .map(|(path, result)| {
                        (
                            NormalizedPath::new(path).unwrap().as_str().to_owned(),
                            result.clone().map_err(str::to_owned),
                        )
                    })
                    .collect(),
            ),
        }
    }

    fn first_status(
        pending: &TaskFileBaseComputePending<CountingPermitProvider>,
        lane: &TaskDiskLane,
    ) -> u8 {
        std::fs::read(pending.dispatcher.lane_path(lane).unwrap())
            .unwrap()
            .into_iter()
            .next()
            .unwrap()
    }

    fn cleanup_pending<P: TaskLanePermitProvider>(mut pending: TaskFileBaseComputePending<P>) {
        let identities = pending.contexts.keys().cloned().collect::<Vec<_>>();
        for identity in identities {
            let _ = pending.dispatcher.abandon_in_flight(&identity);
        }
        pending.dispatcher.discard().unwrap();
    }

    #[tokio::test]
    async fn hashes_two_rows_with_one_key_lookup_batch() {
        let root = tempfile::tempdir().unwrap();
        let machine = MachineId::from_sha256([0x51; 32]);
        let mut store = NodeStore::open_in_memory(machine).unwrap();
        let first = seed_record(&mut store, r"C:\seed-first.bin", [1; 16], 11, true);
        let second = seed_record(&mut store, r"C:\seed-second.bin", [2; 16], 12, true);
        let first_path = scanned(r"C:\scan-first.bin", 11);
        let second_path = scanned(r"C:\scan-second.bin", 12);
        let acquires = Arc::new(AtomicUsize::new(0));
        let provider = CountingPermitProvider {
            acquires: Arc::clone(&acquires),
        };
        let pending = production(
            root.path(),
            &[
                input(first_path.clone(), lane(), None),
                input(second_path.clone(), lane(), None),
            ],
            provider,
        );
        let (actor, handle, mut acknowledgements) =
            super::super::base_persistence::BaseStoreActor::spawn(store, 4);
        let pending = run_task_file_base_compute(
            pending,
            reader(&[
                (r"C:\scan-first.bin", Ok(first.content_key.md5())),
                (r"C:\scan-second.bin", Ok(second.content_key.md5())),
            ]),
            1,
            &handle,
            &mut acknowledgements,
            ReadCancellationToken::new(),
        )
        .await
        .unwrap();
        assert_eq!(acquires.load(Ordering::SeqCst), 2);
        assert_eq!(handle.lookup_key_batch_sizes_for_test(), vec![2]);
        assert_eq!(pending.manifest.cache_hits, 2);
        assert!(pending.contexts.is_empty());
        pending.dispatcher.health().unwrap();
        drop(handle);
        drop(acknowledgements);
        actor.finish().await.unwrap();
    }

    #[tokio::test]
    async fn complete_remote_content_hit_uses_one_lookup_and_marks_row_completed() {
        let root = tempfile::tempdir().unwrap();
        let machine = MachineId::from_sha256([0x5C; 32]);
        let store = NodeStore::open_in_memory(machine).unwrap();
        let remote_record = BaseCacheRecord {
            content_id: None,
            content_key: ContentKey::new([0xAA; 16], 23),
            media_kind: MediaKind::Other,
            base_complete: true,
            width: None,
            height: None,
            duration_ms: None,
            stage1: None,
            image_stage2: None,
            video_stage2: Box::new([None; 6]),
            contact_sheet_relative_path: None,
        };
        let remote = TestContentRemote {
            calls: Arc::new(AtomicUsize::new(0)),
            hit: Some(remote_record.clone()),
            fail: false,
        };
        let pending = production(
            root.path(),
            &[input(scanned(r"C:\remote-hit.bin", 23), lane(), None)],
            CountingPermitProvider::default(),
        );
        let (actor, handle, mut acknowledgements) =
            super::super::base_persistence::BaseStoreActor::spawn(store, 4);
        let mut remote_available = true;
        let mut warning = None;
        let pending = super::run_task_file_hash_pass_with_remote(
            TaskFileBaseComputePending::from_production(pending),
            reader(&[(r"C:\remote-hit.bin", Ok([0xAA; 16]))]),
            1,
            &handle,
            &mut acknowledgements,
            ReadCancellationToken::new(),
            &remote,
            &mut remote_available,
            &mut warning,
        )
        .await
        .unwrap();

        assert_eq!(remote.calls.load(Ordering::SeqCst), 1);
        assert_eq!(handle.lookup_key_batch_sizes_for_test(), vec![1, 1]);
        assert!(remote_available);
        assert!(warning.is_none());
        assert_eq!(pending.manifest.cache_hits, 1);
        assert!(pending.contexts.is_empty());
        assert_eq!(first_status(&pending, &lane()), b'C');

        drop(handle);
        drop(acknowledgements);
        actor.finish().await.unwrap();
    }

    #[tokio::test]
    async fn remote_content_failure_falls_back_to_media_with_warning() {
        let root = tempfile::tempdir().unwrap();
        let machine = MachineId::from_sha256([0x5D; 32]);
        let store = NodeStore::open_in_memory(machine).unwrap();
        let remote = TestContentRemote {
            calls: Arc::new(AtomicUsize::new(0)),
            hit: None,
            fail: true,
        };
        let pending = production(
            root.path(),
            &[input(scanned(r"C:\remote-fallback.bin", 24), lane(), None)],
            CountingPermitProvider::default(),
        );
        let (actor, handle, mut acknowledgements) =
            super::super::base_persistence::BaseStoreActor::spawn(store, 4);
        let mut remote_available = true;
        let mut warning = None;
        let pending = super::run_task_file_hash_pass_with_remote(
            TaskFileBaseComputePending::from_production(pending),
            reader(&[(r"C:\remote-fallback.bin", Ok([0xAB; 16]))]),
            1,
            &handle,
            &mut acknowledgements,
            ReadCancellationToken::new(),
            &remote,
            &mut remote_available,
            &mut warning,
        )
        .await
        .unwrap();

        assert_eq!(remote.calls.load(Ordering::SeqCst), 1);
        assert!(!remote_available);
        assert!(warning.is_some());
        assert_eq!(pending.contexts.len(), 1);
        assert_eq!(first_status(&pending, &lane()), b'P');
        cleanup_pending(pending);
        drop(handle);
        drop(acknowledgements);
        actor.finish().await.unwrap();
    }

    #[tokio::test]
    async fn hash_pass_does_not_rehash_existing_media_continuation() {
        let root = tempfile::tempdir().unwrap();
        let machine = MachineId::from_sha256([0x59; 32]);
        let mut store = NodeStore::open_in_memory(machine).unwrap();
        let cached = seed_record(&mut store, r"C:\seed-continuation.bin", [12; 16], 22, false);
        let provider = CountingPermitProvider::default();
        let pending = production(
            root.path(),
            &[input(scanned(r"C:\continuation.bin", 22), lane(), None)],
            provider.clone(),
        );
        let (actor, handle, mut acknowledgements) =
            super::super::base_persistence::BaseStoreActor::spawn(store, 4);
        let pending = run_task_file_base_compute(
            pending,
            reader(&[(r"C:\continuation.bin", Ok(cached.content_key.md5()))]),
            1,
            &handle,
            &mut acknowledgements,
            ReadCancellationToken::new(),
        )
        .await
        .unwrap();
        assert_eq!(pending.remaining_hash_rows, 0);
        assert_eq!(pending.contexts.len(), 1);

        let mut pending = pending;
        pending.blocked_reason = Some(TaskDispatchBlockReason::MediaPending);
        let pending = run_task_file_hash_pass(
            pending,
            TestHashReader::default(),
            1,
            &handle,
            &mut acknowledgements,
            ReadCancellationToken::new(),
        )
        .await
        .unwrap();

        assert_eq!(provider.acquires.load(Ordering::SeqCst), 1);
        assert_eq!(pending.remaining_hash_rows, 0);
        assert_eq!(pending.blocked_reason, None);
        cleanup_pending(pending);
        drop(handle);
        drop(acknowledgements);
        actor.finish().await.unwrap();
    }

    #[tokio::test]
    async fn hash_pass_joins_in_flight_reads_before_cancellation_returns() {
        let root = tempfile::tempdir().unwrap();
        let provider_active = Arc::new(AtomicIsize::new(0));
        let provider = ErrorAfterPermitProvider {
            attempts: Arc::new(AtomicUsize::new(0)),
            active: Arc::clone(&provider_active),
            successful_acquires: 4,
        };
        let pending = production(
            root.path(),
            &[
                input(scanned(r"C:\cancel-first.bin", 31), lane(), None),
                input(scanned(r"C:\cancel-second.bin", 32), lane(), None),
                input(scanned(r"C:\cancel-third.bin", 33), lane(), None),
            ],
            provider,
        );
        let store = NodeStore::open_in_memory(MachineId::from_sha256([0x5A; 32])).unwrap();
        let (actor, handle, mut acknowledgements) =
            super::super::base_persistence::BaseStoreActor::spawn(store, 4);
        let entered = Arc::new(AtomicUsize::new(0));
        let cancellation_seen = Arc::new(AtomicUsize::new(0));
        let reader = CancellationAwareHashReader {
            entered: Arc::clone(&entered),
            cancellation_seen: Arc::clone(&cancellation_seen),
            slow_sequence: Some(1),
        };
        let cancellation = ReadCancellationToken::new();
        let cancellation_for_run = cancellation.clone();
        let join = tokio::spawn(async move {
            run_task_file_hash_pass(
                TaskFileBaseComputePending::from_production(pending),
                reader,
                2,
                &handle,
                &mut acknowledgements,
                cancellation_for_run,
            )
            .await
        });
        tokio::time::timeout(Duration::from_secs(2), async {
            while entered.load(Ordering::SeqCst) < 2 {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("取消测试必须先启动两个 Hash 读取");
        cancellation.cancel();
        let result = tokio::time::timeout(Duration::from_secs(2), join)
            .await
            .expect("取消后的 Hash pass 必须收束")
            .unwrap();
        let error = match result {
            Ok(_) => panic!("取消必须返回任务级错误"),
            Err(error) => error,
        };
        let pending = error.into_pending();
        assert_eq!(provider_active.load(Ordering::SeqCst), 0);
        assert_eq!(cancellation_seen.load(Ordering::SeqCst), 2);
        cleanup_pending(pending);
        actor.finish().await.unwrap();
    }

    #[tokio::test]
    async fn hash_pass_joins_in_flight_reads_before_dispatch_error_returns() {
        let root = tempfile::tempdir().unwrap();
        let active = Arc::new(AtomicIsize::new(0));
        let provider = ErrorAfterPermitProvider {
            attempts: Arc::new(AtomicUsize::new(0)),
            active: Arc::clone(&active),
            successful_acquires: 2,
        };
        let pending = production(
            root.path(),
            &[
                input(scanned(r"C:\error-first.bin", 41), lane(), None),
                input(scanned(r"C:\error-second.bin", 42), lane(), None),
                input(scanned(r"C:\error-third.bin", 43), lane(), None),
            ],
            provider,
        );
        let store = NodeStore::open_in_memory(MachineId::from_sha256([0x5B; 32])).unwrap();
        let (actor, handle, mut acknowledgements) =
            super::super::base_persistence::BaseStoreActor::spawn(store, 4);
        let entered = Arc::new(AtomicUsize::new(0));
        let cancellation_seen = Arc::new(AtomicUsize::new(0));
        let entered_for_reader = Arc::clone(&entered);
        let cancellation_seen_for_reader = Arc::clone(&cancellation_seen);
        let join = tokio::spawn(async move {
            run_task_file_hash_pass(
                TaskFileBaseComputePending::from_production(pending),
                CancellationAwareHashReader {
                    entered: entered_for_reader,
                    cancellation_seen: cancellation_seen_for_reader,
                    slow_sequence: None,
                },
                3,
                &handle,
                &mut acknowledgements,
                ReadCancellationToken::new(),
            )
            .await
        });
        let result = tokio::time::timeout(Duration::from_secs(2), join)
            .await
            .expect("dispatcher 错误必须返回")
            .unwrap();
        let error = match result {
            Ok(_) => panic!("第三次许可失败必须返回任务级错误"),
            Err(error) => error,
        };
        let pending = error.into_pending();
        assert_eq!(entered.load(Ordering::SeqCst), 2);
        assert_eq!(active.load(Ordering::SeqCst), 0);
        assert_eq!(cancellation_seen.load(Ordering::SeqCst), 2);
        cleanup_pending(pending);
        actor.finish().await.unwrap();
    }

    #[tokio::test]
    async fn hash_reads_enter_concurrently_up_to_explicit_capacity() {
        let root = tempfile::tempdir().unwrap();
        let machine = MachineId::from_sha256([0x57; 32]);
        let mut store = NodeStore::open_in_memory(machine).unwrap();
        let first = seed_record(
            &mut store,
            r"C:\seed-concurrent-first.bin",
            [8; 16],
            18,
            true,
        );
        let second = seed_record(
            &mut store,
            r"C:\seed-concurrent-second.bin",
            [9; 16],
            19,
            true,
        );
        let entered = Arc::new(AtomicUsize::new(0));
        let barrier = Arc::new(Barrier::new(3));
        let reader = ConcurrentHashReader {
            results: Arc::new(BTreeMap::from([
                (
                    NormalizedPath::new(r"C:\scan-concurrent-first.bin")
                        .unwrap()
                        .as_str()
                        .to_owned(),
                    first.content_key.md5(),
                ),
                (
                    NormalizedPath::new(r"C:\scan-concurrent-second.bin")
                        .unwrap()
                        .as_str()
                        .to_owned(),
                    second.content_key.md5(),
                ),
            ])),
            entered: Arc::clone(&entered),
            barrier: Arc::clone(&barrier),
        };
        let pending = production(
            root.path(),
            &[
                input(scanned(r"C:\scan-concurrent-first.bin", 18), lane(), None),
                input(scanned(r"C:\scan-concurrent-second.bin", 19), lane(), None),
            ],
            CountingPermitProvider::default(),
        );
        let (actor, handle, mut acknowledgements) =
            super::super::base_persistence::BaseStoreActor::spawn(store, 4);
        let handle_for_run = handle.clone();
        let join = tokio::spawn(async move {
            run_task_file_base_compute(
                pending,
                reader,
                2,
                &handle_for_run,
                &mut acknowledgements,
                ReadCancellationToken::new(),
            )
            .await
        });
        tokio::time::timeout(Duration::from_secs(2), async {
            while entered.load(Ordering::SeqCst) < 2 {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("两个 Hash 必须在任一释放前都进入读取");
        barrier.wait().await;
        let pending = join.await.unwrap().unwrap();
        assert_eq!(pending.manifest.cache_hits, 2);
        assert_eq!(pending.remaining_hash_rows, 0);
        assert_eq!(pending.blocked_reason, None);
        drop(handle);
        actor.finish().await.unwrap();
    }

    #[tokio::test]
    async fn media_first_blocks_hash_and_reports_remaining_hash() {
        let root = tempfile::tempdir().unwrap();
        let machine = MachineId::from_sha256([0x58; 32]);
        let mut store = NodeStore::open_in_memory(machine).unwrap();
        let cached = seed_record(&mut store, r"C:\seed-media-first.bin", [10; 16], 20, false);
        let provider = CountingPermitProvider::default();
        let pending = production(
            root.path(),
            &[
                input(scanned(r"C:\media-first.bin", 20), lane(), Some(cached)),
                input(scanned(r"C:\hash-after-media.bin", 21), lane(), None),
            ],
            provider.clone(),
        );
        let (actor, handle, mut acknowledgements) =
            super::super::base_persistence::BaseStoreActor::spawn(store, 4);
        let pending = run_task_file_base_compute(
            pending,
            reader(&[(r"C:\hash-after-media.bin", Ok([11; 16]))]),
            1,
            &handle,
            &mut acknowledgements,
            ReadCancellationToken::new(),
        )
        .await
        .unwrap();
        assert_eq!(provider.acquires.load(Ordering::SeqCst), 0);
        assert_eq!(pending.remaining_hash_rows, 1);
        assert_eq!(
            pending.blocked_reason,
            Some(TaskDispatchBlockReason::MediaPending)
        );
        cleanup_pending(pending);
        drop(handle);
        drop(acknowledgements);
        actor.finish().await.unwrap();
    }

    #[cfg(feature = "test-hooks")]
    #[tokio::test]
    async fn complete_content_stays_pending_until_ack_then_becomes_completed() {
        let root = tempfile::tempdir().unwrap();
        let lane = lane();
        let machine = MachineId::from_sha256([0x52; 32]);
        let mut store = NodeStore::open_in_memory(machine).unwrap();
        let cached = seed_record(&mut store, r"C:\seed-complete.bin", [3; 16], 13, true);
        let provider = CountingPermitProvider::default();
        let pending = production(
            root.path(),
            &[input(
                scanned(r"C:\scan-complete.bin", 13),
                lane.clone(),
                None,
            )],
            provider,
        );
        let (controller, waiter) = super::super::base_persistence::BasePersistTestController::new();
        let (actor, handle, mut acknowledgements) =
            super::super::base_persistence::BaseStoreActor::spawn_with_first_persist_waiter(
                store, 4, waiter,
            );
        let reader = reader(&[(r"C:\scan-complete.bin", Ok(cached.content_key.md5()))]);
        let handle_for_run = handle.clone();
        let join = tokio::spawn(async move {
            run_task_file_base_compute(
                pending,
                reader,
                1,
                &handle_for_run,
                &mut acknowledgements,
                ReadCancellationToken::new(),
            )
            .await
        });
        controller.wait_until_entered().await;
        assert_eq!(
            std::fs::read(root.path().join(RUN_ID).join("PhysicalDisk7-hdd.tasks.tsv")).unwrap()[0],
            b'P'
        );
        controller.release();
        let pending = join.await.unwrap().unwrap();
        assert_eq!(
            std::fs::read(pending.dispatcher.lane_path(&lane).unwrap()).unwrap()[0],
            b'C'
        );
        assert_eq!(pending.manifest.cache_hits, 1);
        assert_eq!(pending.manifest.resolved_files.len(), 1);
        drop(handle);
        actor.finish().await.unwrap();
    }

    #[tokio::test]
    async fn partial_content_keeps_same_identity_pending_for_media() {
        let root = tempfile::tempdir().unwrap();
        let machine = MachineId::from_sha256([0x53; 32]);
        let mut store = NodeStore::open_in_memory(machine).unwrap();
        let cached = seed_record(&mut store, r"C:\seed-partial.bin", [4; 16], 14, false);
        let provider = CountingPermitProvider::default();
        let pending = production(
            root.path(),
            &[input(scanned(r"C:\scan-partial.bin", 14), lane(), None)],
            provider.clone(),
        );
        let (actor, handle, mut acknowledgements) =
            super::super::base_persistence::BaseStoreActor::spawn(store, 4);
        let pending = run_task_file_base_compute(
            pending,
            reader(&[(r"C:\scan-partial.bin", Ok(cached.content_key.md5()))]),
            1,
            &handle,
            &mut acknowledgements,
            ReadCancellationToken::new(),
        )
        .await
        .unwrap();
        assert_eq!(provider.acquires.load(Ordering::SeqCst), 1);
        assert_eq!(pending.contexts.len(), 1);
        let (identity, context) = pending.contexts.iter().next().unwrap();
        assert_eq!(context.content_id, cached.content_id);
        assert_eq!(context.cached, Some(cached));
        assert_eq!(context.lane, lane());
        assert_eq!(first_status(&pending, &lane()), b'P');
        assert_eq!(identity.run_id(), RUN_ID);
        cleanup_pending(pending);
        drop(handle);
        drop(acknowledgements);
        actor.finish().await.unwrap();
    }

    #[cfg(feature = "test-hooks")]
    #[tokio::test]
    async fn read_failure_is_p_until_ack_then_marks_f_and_continues() {
        let root = tempfile::tempdir().unwrap();
        let lane = lane();
        let machine = MachineId::from_sha256([0x54; 32]);
        let mut store = NodeStore::open_in_memory(machine).unwrap();
        let cached = seed_record(&mut store, r"C:\seed-after-failure.bin", [5; 16], 16, true);
        let provider = CountingPermitProvider::default();
        let pending = production(
            root.path(),
            &[
                input(scanned(r"C:\scan-failure.bin", 15), lane.clone(), None),
                input(
                    scanned(r"C:\scan-after-failure.bin", 16),
                    lane.clone(),
                    None,
                ),
            ],
            provider,
        );
        let (controller, waiter) = super::super::base_persistence::BasePersistTestController::new();
        let (actor, handle, mut acknowledgements) =
            super::super::base_persistence::BaseStoreActor::spawn_with_first_persist_waiter(
                store, 4, waiter,
            );
        let reader = reader(&[
            (r"C:\scan-failure.bin", Err("read failed")),
            (r"C:\scan-after-failure.bin", Ok(cached.content_key.md5())),
        ]);
        let handle_for_run = handle.clone();
        let join = tokio::spawn(async move {
            run_task_file_base_compute(
                pending,
                reader,
                1,
                &handle_for_run,
                &mut acknowledgements,
                ReadCancellationToken::new(),
            )
            .await
        });
        controller.wait_until_entered().await;
        assert_eq!(
            std::fs::read(root.path().join(RUN_ID).join("PhysicalDisk7-hdd.tasks.tsv")).unwrap()[0],
            b'P'
        );
        controller.release();
        let pending = join.await.unwrap().unwrap();
        let bytes = std::fs::read(pending.dispatcher.lane_path(&lane).unwrap()).unwrap();
        assert_eq!(bytes[0], b'F');
        let second_line = bytes.iter().position(|byte| *byte == b'\n').unwrap() + 1;
        assert_eq!(bytes[second_line], b'C');
        drop(handle);
        let persisted_store = actor.finish().await.unwrap();
        let faults = persisted_store.page_file_faults(None, 10).unwrap();
        assert_eq!(faults.items.len(), 1);
        assert_eq!(
            faults.items[0].kind,
            dedup_node_store::FileFaultKind::SuspectedPhysicalRead
        );
        assert_eq!(
            faults.items[0].normalized_path.as_str(),
            r"C:\SCAN-FAILURE.BIN"
        );
    }

    #[tokio::test]
    async fn known_md5_partial_is_pending_without_second_provider_acquire() {
        let root = tempfile::tempdir().unwrap();
        let machine = MachineId::from_sha256([0x55; 32]);
        let mut store = NodeStore::open_in_memory(machine).unwrap();
        let path = scanned(r"C:\known-partial.bin", 17);
        let cached = seed_record(&mut store, r"C:\seed-known-partial.bin", [6; 16], 17, false);
        let provider = CountingPermitProvider::default();
        let pending = production(
            root.path(),
            &[input(path, lane(), Some(cached))],
            provider.clone(),
        );
        let (actor, handle, mut acknowledgements) =
            super::super::base_persistence::BaseStoreActor::spawn(store, 4);
        let pending = run_task_file_base_compute(
            pending,
            TestHashReader::default(),
            1,
            &handle,
            &mut acknowledgements,
            ReadCancellationToken::new(),
        )
        .await
        .unwrap();
        assert_eq!(provider.acquires.load(Ordering::SeqCst), 0);
        assert_eq!(pending.contexts.len(), 1);
        cleanup_pending(pending);
        drop(handle);
        drop(acknowledgements);
        actor.finish().await.unwrap();
    }

    #[tokio::test]
    async fn store_error_returns_remaining_pending_row_without_marking_terminal() {
        let root = tempfile::tempdir().unwrap();
        let path = scanned(r"C:\store-error.bin", u64::MAX);
        let provider = CountingPermitProvider::default();
        let pending = production(root.path(), &[input(path, lane(), None)], provider);
        let store = NodeStore::open_in_memory(MachineId::from_sha256([0x56; 32])).unwrap();
        let (actor, handle, mut acknowledgements) =
            super::super::base_persistence::BaseStoreActor::spawn(store, 4);
        let error = match run_task_file_base_compute(
            pending,
            reader(&[(r"C:\store-error.bin", Ok([7; 16]))]),
            1,
            &handle,
            &mut acknowledgements,
            ReadCancellationToken::new(),
        )
        .await
        {
            Ok(_) => panic!("超大文件大小应让 SQLite 批量查询返回任务级错误"),
            Err(error) => error,
        };
        let pending = error.into_pending();
        assert_eq!(first_status(&pending, &lane()), b'P');
        cleanup_pending(pending);
        drop(handle);
        drop(acknowledgements);
        actor.finish().await.unwrap();
    }
}
