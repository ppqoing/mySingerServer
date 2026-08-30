//! 瞬态基础任务的单事件流式协调器。
//!
//! 唯一 owner 同时驱动 Hash、远端缓存、Media Worker 和 SQLite ACK，禁止把 Hash
//! 结果先积累成整批再启动媒体续算。

use std::{fmt, sync::Arc, time::Duration};

use dedup_core::ContentKey;
use dedup_node_store::BaseCacheRecord;
use dedup_windows::ReadCancellationToken;
use tokio::{sync::mpsc::UnboundedReceiver, task::JoinSet, time::sleep};

use super::{
    BaseComputeDecision, HashPermitReader, TaskFileBaseCoordinatorOptions,
    base_compute::cache_rank,
    base_persistence::{BasePersistAck, BaseStoreHandle},
    task_file_base_compute::{HashReadOutcome, TaskFileBaseComputePending, TaskFileHashRuntime},
    task_file_media_compute::TaskFileMediaRuntime,
    task_file_media_persistence::TaskFilePersistRuntime,
};
use crate::{
    RemoteCacheError, RemoteFeatureCache,
    runtime_tasks::RuntimeTaskReporter,
    task_dispatch::{
        TaskDispatchAdmission, TaskDispatchError, TaskDispatchPoll, TaskLanePermitProvider,
    },
    task_files::{TaskFileIdentity, TaskFileRecord, TaskWorkMask},
    worker::WorkerPool,
};

/// 一条远端查询的输入，保留 Hash 结果和首次 SQLite 单键查询快照。
struct RemoteLookupInput {
    /// 当前任务文件身份。
    identity: TaskFileIdentity,
    /// 原始 Hash 行，Media 续算沿用同一行。
    record: TaskFileRecord,
    /// 已计算的内容 MD5。
    md5: [u8; 16],
    /// Hash 完成后立即取得的本地缓存快照。
    local: Option<BaseCacheRecord>,
}

/// 一条单键远端查询的完成结果。
struct RemoteLookupOutput {
    /// 用于恢复本地或继续 Media 的原始输入。
    input: RemoteLookupInput,
    /// 远端单键查询结果；网络错误由本轮统一降级。
    result: Result<Option<BaseCacheRecord>, RemoteCacheError>,
    /// 返回数量或 ContentKey 不匹配；该项仅使用首次 SQLite 快照。
    malformed: bool,
}

/// 流式事件泵失败时仍携带唯一 pending owner。
pub(super) struct TaskFileBaseStreamError<P: TaskLanePermitProvider> {
    /// 面向协调器的错误文本。
    message: String,
    /// 未确认任务行和 dispatcher 的唯一 owner。
    pending: TaskFileBaseComputePending<P>,
}

impl<P: TaskLanePermitProvider> TaskFileBaseStreamError<P> {
    /// 消费错误并取回待收束的唯一任务文件 owner。
    pub(super) fn into_pending(self) -> TaskFileBaseComputePending<P> {
        self.pending
    }
}

impl<P: TaskLanePermitProvider> fmt::Display for TaskFileBaseStreamError<P> {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(&self.message)
    }
}

/// 单一 Hash/远端/Media/SQLite ACK 事件泵。
pub(super) async fn run_task_file_base_stream<P, H, R>(
    mut pending: TaskFileBaseComputePending<P>,
    reader: H,
    worker_pool: &mut WorkerPool,
    store: &BaseStoreHandle,
    acknowledgements: &mut UnboundedReceiver<BasePersistAck>,
    options: &TaskFileBaseCoordinatorOptions,
    cancellation: ReadCancellationToken,
    remote: Arc<R>,
    remote_available: &mut bool,
    warning: &mut Option<String>,
    reporter: Option<&RuntimeTaskReporter>,
) -> Result<TaskFileBaseComputePending<P>, TaskFileBaseStreamError<P>>
where
    P: TaskLanePermitProvider,
    H: HashPermitReader<Permit = P::Permit>,
    R: RemoteFeatureCache,
{
    if options.hash_capacity == 0 {
        return Err(stream_error(pending, "基础 Hash 读取容量必须大于 0"));
    }
    if options.worker_capacity == 0 {
        return Err(stream_error(pending, "Media Worker 容量必须大于 0"));
    }
    if *remote_available {
        if let Some(startup_warning) = remote.startup_warning() {
            *remote_available = false;
            if warning.is_none() {
                *warning = Some(startup_warning.to_owned());
            }
        }
    }

    pending.blocked_reason = None;
    let mut hash = TaskFileHashRuntime::new(pending.remaining_hash_rows);
    let mut media = TaskFileMediaRuntime::new();
    let mut persist = TaskFilePersistRuntime::new();
    let mut remote_lookups = JoinSet::<RemoteLookupOutput>::new();
    let mut failure = None;

    loop {
        if cancellation.is_cancelled() {
            failure = Some("基础任务已取消".to_owned());
            break;
        }
        if let Err(message) = persist.try_submit_one(store) {
            failure = Some(message);
            break;
        }
        if hash.is_finished()
            && remote_lookups.is_empty()
            && !media.has_active()
            && persist.is_empty()
            && pending.contexts.is_empty()
        {
            break;
        }

        // Hash 和远端查询共用 hash_capacity；Media 只受独立 worker_capacity 限制。
        let allow_hash = hash.can_dispatch(options.hash_capacity)
            && hash.active_len().saturating_add(remote_lookups.len()) < options.hash_capacity
            // 已派发 Media 的同 lane P 行必须先等 SQLite ACK，避免越过原行读取后续 Hash。
            && !media.has_active();
        let allow_media = media.has_capacity(options.worker_capacity);
        let admission = TaskDispatchAdmission {
            allow_hash,
            allow_media,
        };

        tokio::select! {
            biased;
            ack = acknowledgements.recv(), if persist.has_in_flight() => {
                match ack {
                    Some(ack) => {
                        if let Err(message) = persist.apply_ack(&mut pending, ack) {
                            failure = Some(message);
                            break;
                        }
                    }
                    None => {
                        failure = Some("基础持久化 actor 未返回 ACK".into());
                        break;
                    }
                }
            }
            event = worker_pool.next_event(), if media.has_active() => {
                match event {
                    Some(event) => match media.handle_event(event, reporter).await {
                        Ok(Some(terminal)) => {
                            if let Err(message) = persist.enqueue_media_terminal(terminal, &options.persistence) {
                                failure = Some(message);
                                break;
                            }
                        }
                        Ok(None) => {}
                        Err(message) => {
                            failure = Some(message);
                            break;
                        }
                    },
                    None => {
                        failure = Some("WorkerPool 在 Media 运行期间关闭".into());
                        break;
                    }
                }
            }
            joined = remote_lookups.join_next(), if !remote_lookups.is_empty() => {
                match joined {
                    Some(Ok(output)) => {
                        if let Err(message) = apply_remote_output(
                            &mut pending,
                            &mut persist,
                            store,
                            output,
                            remote_available,
                            warning,
                        ) {
                            failure = Some(message);
                            break;
                        }
                    }
                    Some(Err(error)) => {
                        failure = Some(format!("远端内容缓存 future 异常结束: {error}"));
                        break;
                    }
                    None => {
                        failure = Some("远端内容缓存运行态提前为空".into());
                        break;
                    }
                }
            }
            joined = hash.join_one(), if hash.active_len() > 0 => {
                match joined {
                    Ok(outcome) => {
                        if let Err(message) = apply_hash_outcome(
                            &mut pending,
                            &mut persist,
                            &mut remote_lookups,
                            store,
                            remote.clone(),
                            remote_available,
                            outcome,
                        ) {
                            failure = Some(message);
                            break;
                        }
                    }
                    Err(message) => {
                        failure = Some(message);
                        break;
                    }
                }
            }
            dispatch = pending.dispatcher.next_with_admission(cancellation.clone(), admission), if persist.is_empty() && (allow_hash || allow_media) => {
                match dispatch {
                    Ok(TaskDispatchPoll::Task(task)) => {
                        let identity = task.identity.clone();
                        match task.class {
                            crate::io::DiskReadClass::HashSequential => {
                                if !allow_hash || task.record.known_md5.is_some() || !task.record.missing.needs_md5() {
                                    drop(task.permit);
                                    let _ = pending.dispatcher.abandon_in_flight(&identity);
                                    failure = Some("流式 Hash admission 返回了无效任务".into());
                                    break;
                                }
                                if !pending.contexts.contains_key(&identity) || pending.remaining_hash_rows == 0 {
                                    drop(task.permit);
                                    let _ = pending.dispatcher.abandon_in_flight(&identity);
                                    failure = Some("流式 Hash 任务缺少对应上下文".into());
                                    break;
                                }
                                pending.remaining_hash_rows -= 1;
                                if let Err(message) = hash.spawn(task, reader.clone(), cancellation.clone()) {
                                    let _ = pending.dispatcher.abandon_in_flight(&identity);
                                    failure = Some(message);
                                    break;
                                }
                            }
                            crate::io::DiskReadClass::MediaDecode => {
                                if !allow_media {
                                    drop(task.permit);
                                    let _ = pending.dispatcher.abandon_in_flight(&identity);
                                    failure = Some("流式 Media admission 超出 Worker 容量".into());
                                    break;
                                }
                                if let Err(message) = media.dispatch(
                                    &mut pending,
                                    task,
                                    worker_pool,
                                    store,
                                    &options.read_config,
                                    &cancellation,
                                ).await {
                                    let _ = pending.dispatcher.abandon_in_flight(&identity);
                                    failure = Some(message);
                                    break;
                                }
                            }
                        }
                    }
                    Ok(TaskDispatchPoll::Blocked(reason)) => pending.blocked_reason = Some(reason),
                    Ok(TaskDispatchPoll::Drained) => {}
                    Err(TaskDispatchError::Read(crate::io::ReadFailure::Cancelled)) => {
                        failure = Some("基础任务已取消".into());
                        break;
                    }
                    Err(error) => {
                        failure = Some(format!("流式 dispatcher 失败: {error}"));
                        break;
                    }
                }
            }
            _ = sleep(Duration::from_millis(10)) => {}
        }
    }

    if let Some(message) = failure {
        cleanup_stream(
            &mut pending,
            &mut hash,
            &mut media,
            &mut remote_lookups,
            worker_pool,
            &mut persist,
            &cancellation,
        )
        .await;
        return Err(stream_error(pending, message));
    }
    Ok(pending)
}

/// 处理一条 Hash 终态；成功后立即执行单键 SQLite 查询，不等待其他 Hash。
fn apply_hash_outcome<P, R>(
    pending: &mut TaskFileBaseComputePending<P>,
    persist: &mut TaskFilePersistRuntime,
    remote_lookups: &mut JoinSet<RemoteLookupOutput>,
    store: &BaseStoreHandle,
    remote: Arc<R>,
    remote_available: &mut bool,
    outcome: HashReadOutcome,
) -> Result<(), String>
where
    P: TaskLanePermitProvider,
    R: RemoteFeatureCache,
{
    let HashReadOutcome {
        identity,
        record,
        result,
        ..
    } = outcome;
    match result {
        Ok(md5) => {
            let key = ContentKey::new(md5, record.scanned.file_size);
            let local = store
                .lookup_base_cache_by_key(&key)
                .map_err(|error| error.to_string())?;
            let input = RemoteLookupInput {
                identity,
                record,
                md5,
                local,
            };
            let needs_remote = input.local.as_ref().is_some_and(|cached| {
                pending
                    .contexts
                    .get(&input.identity)
                    .is_some_and(|context| {
                        !context.force_recompute
                            && BaseComputeDecision::for_cache(
                                Some(cached),
                                context.contact_sheet_valid,
                                context.force_recompute,
                            )
                            .missing_parts()
                                != 0
                    })
            }) || input.local.is_none()
                && pending
                    .contexts
                    .get(&input.identity)
                    .is_some_and(|context| !context.force_recompute);
            if needs_remote && *remote_available {
                remote_lookups.spawn(async move {
                    let key = ContentKey::new(input.md5, input.record.scanned.file_size);
                    match remote.lookup_contents(&[key]).await {
                        Ok(mut records) if records.len() == 1 => {
                            let record = records.pop().expect("长度已验证为一");
                            let malformed = record
                                .as_ref()
                                .is_some_and(|record| record.content_key != key);
                            RemoteLookupOutput {
                                input,
                                result: Ok(record),
                                malformed,
                            }
                        }
                        Ok(_) => RemoteLookupOutput {
                            input,
                            result: Ok(None),
                            malformed: true,
                        },
                        Err(error) => RemoteLookupOutput {
                            input,
                            result: Err(error),
                            malformed: false,
                        },
                    }
                });
            } else {
                let local = input.local.clone();
                apply_cached_result(pending, persist, input, local)?;
            }
        }
        Err(crate::io::ReadFailure::Cancelled) => return Err("基础 Hash 阶段已取消".into()),
        Err(error) => persist.enqueue_hash_failure(identity, record.scanned, error)?,
    }
    Ok(())
}

/// 处理一个单键远端结果；导入后最多只复查一次 SQLite 以取得本地 content_id。
fn apply_remote_output<P>(
    pending: &mut TaskFileBaseComputePending<P>,
    persist: &mut TaskFilePersistRuntime,
    store: &BaseStoreHandle,
    output: RemoteLookupOutput,
    remote_available: &mut bool,
    warning: &mut Option<String>,
) -> Result<(), String>
where
    P: TaskLanePermitProvider,
{
    let RemoteLookupOutput {
        input,
        result,
        malformed,
    } = output;
    let mut local = input.local.clone();
    match result {
        Ok(remote_record) if !malformed => {
            if remote_record
                .as_ref()
                .is_some_and(|record| cache_rank(Some(record)) > cache_rank(local.as_ref()))
            {
                store
                    .import_base_cache_record(
                        &input.record.scanned,
                        remote_record.as_ref().expect("上方已检查 Some"),
                    )
                    .map_err(|error| error.to_string())?;
                let key = ContentKey::new(input.md5, input.record.scanned.file_size);
                local = store
                    .lookup_base_cache_by_key(&key)
                    .map_err(|error| error.to_string())?;
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
    apply_cached_result(pending, persist, input, local)
}

/// 用当前缓存决定 ACK 命中或同身份 Media 续算。
fn apply_cached_result<P>(
    pending: &mut TaskFileBaseComputePending<P>,
    persist: &mut TaskFilePersistRuntime,
    input: RemoteLookupInput,
    cached: Option<BaseCacheRecord>,
) -> Result<(), String>
where
    P: TaskLanePermitProvider,
{
    let context = pending
        .contexts
        .get_mut(&input.identity)
        .ok_or_else(|| "Hash 结果缺少对应的内存上下文".to_owned())?;
    context.cached = cached.clone();
    context.content_id = cached.as_ref().and_then(|record| record.content_id);
    let decision = BaseComputeDecision::for_cache(
        cached.as_ref(),
        context.contact_sheet_valid,
        context.force_recompute,
    );
    if decision.missing_parts() == 0 {
        let cached = cached.ok_or_else(|| "完整内容缓存缺少记录".to_owned())?;
        persist.enqueue_hash_complete(
            input.identity,
            input.record.scanned,
            input.md5,
            cached.media_kind,
            cached.content_key,
        )?;
    } else {
        let missing = TaskWorkMask::for_base(false, decision.missing_parts())
            .ok_or_else(|| "Hash 后基础缺失掩码无效".to_owned())?;
        let media_record = TaskFileRecord {
            item_id: input.record.item_id,
            work_kind: input.record.work_kind,
            scanned: input.record.scanned,
            known_md5: Some(input.md5),
            missing,
        };
        pending
            .dispatcher
            .request_media_continuation(&input.identity, &media_record)
            .map_err(|error| error.to_string())?;
    }
    Ok(())
}

/// 关闭本轮远端入口并保留第一条可见告警。
fn note_remote_failure(remote_available: &mut bool, warning: &mut Option<String>, message: String) {
    *remote_available = false;
    if warning.is_none() {
        *warning = Some(message);
    }
}

/// 错误或取消前收束所有 future，并使未 ACK 行仍保持 P。
async fn cleanup_stream<P: TaskLanePermitProvider>(
    pending: &mut TaskFileBaseComputePending<P>,
    hash: &mut TaskFileHashRuntime,
    media: &mut TaskFileMediaRuntime<P>,
    remote_lookups: &mut JoinSet<RemoteLookupOutput>,
    worker_pool: &WorkerPool,
    persist: &mut TaskFilePersistRuntime,
    cancellation: &ReadCancellationToken,
) {
    cancellation.cancel();
    hash.cancel_and_join().await;
    media.cancel_and_drain(worker_pool, cancellation).await;
    remote_lookups.abort_all();
    while remote_lookups.join_next().await.is_some() {}
    persist.drop_unacknowledged();
    let _ = pending
        .dispatcher
        .next_with_admission(cancellation.clone(), TaskDispatchAdmission::all())
        .await;
}

/// 构造仍携带唯一任务文件 owner 的流式错误。
fn stream_error<P: TaskLanePermitProvider>(
    pending: TaskFileBaseComputePending<P>,
    message: impl Into<String>,
) -> TaskFileBaseStreamError<P> {
    TaskFileBaseStreamError {
        message: message.into(),
        pending,
    }
}
