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
            && hash.active_len().saturating_add(remote_lookups.len()) < options.hash_capacity;
        let allow_media = media.has_capacity(options.worker_capacity);
        let admission = TaskDispatchAdmission {
            allow_hash,
            allow_media,
        };
        let dispatch_ready = match pending.dispatcher.has_admitted_work(admission) {
            Ok(ready) => ready,
            Err(error) => {
                failure = Some(format!("流式 dispatcher admission 检查失败: {error}"));
                break;
            }
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
            dispatch = pending.dispatcher.next_with_admission(cancellation.clone(), admission), if persist.is_empty() && dispatch_ready => {
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
        let cleanup_diagnostics = cleanup_stream(
            &mut pending,
            &mut hash,
            &mut media,
            &mut remote_lookups,
            worker_pool,
            &mut persist,
            &cancellation,
        )
        .await;
        return Err(stream_error(
            pending,
            append_cleanup_diagnostics(message, cleanup_diagnostics),
        ));
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
        Ok(remote_record) if *remote_available && !malformed => {
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
        Ok(_) if *remote_available => note_remote_failure(
            remote_available,
            warning,
            "远端内容缓存返回数量或内容键不一致".into(),
        ),
        // 另一在途请求已经使本轮远端降级；必须只保留该项发起时的 SQLite 快照。
        Ok(_) => {}
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

/// 错误或取消前按 owner 依赖顺序收束所有外部资源，并使未 ACK 行保持 P。
async fn cleanup_stream<P: TaskLanePermitProvider>(
    pending: &mut TaskFileBaseComputePending<P>,
    hash: &mut TaskFileHashRuntime,
    media: &mut TaskFileMediaRuntime<P>,
    remote_lookups: &mut JoinSet<RemoteLookupOutput>,
    worker_pool: &mut WorkerPool,
    persist: &mut TaskFilePersistRuntime,
    cancellation: &ReadCancellationToken,
) -> Vec<String> {
    let mut diagnostics = Vec::new();
    cancellation.cancel();
    hash.cancel_and_join().await;
    remote_lookups.abort_all();
    while remote_lookups.join_next().await.is_some() {}
    if let Err(message) = media.cancel_and_drain(worker_pool, cancellation).await {
        diagnostics.push(message);
    }
    persist.drop_unacknowledged();
    if let Err(message) = abandon_all_in_flight(pending) {
        diagnostics.push(message);
    }
    diagnostics
}

/// 在所有 permit、future 与 Worker 都已释放后，逐项解除 dispatcher 的 in-flight owner。
fn abandon_all_in_flight<P: TaskLanePermitProvider>(
    pending: &mut TaskFileBaseComputePending<P>,
) -> Result<(), String> {
    let identities = pending.contexts.keys().cloned().collect::<Vec<_>>();
    for identity in identities {
        pending
            .dispatcher
            .abandon_in_flight(&identity)
            .map_err(|error| format!("取消后归还任务文件 in-flight owner 失败: {error}"))?;
    }
    Ok(())
}

/// 在首个权威错误后附加 cleanup 诊断，避免收束问题覆盖真实失败原因。
fn append_cleanup_diagnostics(message: String, diagnostics: Vec<String>) -> String {
    if diagnostics.is_empty() {
        message
    } else {
        format!("{message}; cleanup 诊断: {}", diagnostics.join("; "))
    }
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

#[cfg(test)]
mod tests {
    use std::{
        future::Future,
        path::{Path, PathBuf},
        pin::Pin,
        sync::{
            Arc,
            atomic::{AtomicUsize, Ordering},
        },
        time::Duration,
    };

    use dedup_core::{
        ContentKey, DiskReadConfig, DisplayPath, MachineId, MediaKind, NormalizedPath,
    };
    use dedup_node_store::{BaseCacheRecord, NodeStore, ScannedPath};
    use dedup_windows::{LocalDiskKind, PhysicalDiskId, ReadCancellationToken};
    use tokio::sync::Notify;

    use super::{TaskFileBaseComputePending, run_task_file_base_stream};
    use crate::{
        RemoteCacheError, RemoteFeatureCache,
        io::{DiskReadClass, ReadFailure},
        scan::{
            BaseTaskInput, BaseTaskProducer, HashPermitReader, PlannedScannedPath, ReadProduct,
            TaskDiskLane, TaskFileBaseCoordinatorOptions, TaskFileMediaPersistenceOptions,
        },
        task_dispatch::{TaskFileDispatcher, TaskLanePermitFuture, TaskLanePermitProvider},
        task_files::TransientTaskFileSet,
        worker::WorkerPool,
    };

    const RUN_ID: &str = "01900000-0000-7000-8000-0000000004d4";

    /// 统计 dispatcher 发出的真实读取许可，Drop 即表示外部 owner 已归还。
    struct CountingPermit(Arc<AtomicUsize>);

    impl Drop for CountingPermit {
        fn drop(&mut self) {
            self.0.fetch_sub(1, Ordering::SeqCst);
        }
    }

    /// 为 Hash 和 Media 共用的可观测许可提供者。
    #[derive(Clone)]
    struct CountingProvider(Arc<AtomicUsize>);

    impl TaskLanePermitProvider for CountingProvider {
        type Permit = CountingPermit;

        fn acquire(
            &self,
            _lane: TaskDiskLane,
            _class: DiskReadClass,
            _cancellation: ReadCancellationToken,
        ) -> TaskLanePermitFuture<Self::Permit> {
            let live = Arc::clone(&self.0);
            Box::pin(async move {
                live.fetch_add(1, Ordering::SeqCst);
                Ok(CountingPermit(live))
            })
        }
    }

    /// 前两次交付真实 permit，第三次返回 dispatcher 读取错误。
    #[derive(Clone)]
    struct ErrorAfterPermitProvider {
        /// 已尝试获取许可的次数。
        attempts: Arc<AtomicUsize>,
        /// 当前仍由 Hash future 持有的 permit 数量。
        active: Arc<AtomicUsize>,
    }

    impl TaskLanePermitProvider for ErrorAfterPermitProvider {
        type Permit = CountingPermit;

        fn acquire(
            &self,
            _lane: TaskDiskLane,
            _class: DiskReadClass,
            _cancellation: ReadCancellationToken,
        ) -> TaskLanePermitFuture<Self::Permit> {
            let attempt = self.attempts.fetch_add(1, Ordering::SeqCst);
            let active = Arc::clone(&self.active);
            Box::pin(async move {
                if attempt >= 2 {
                    return Err(ReadFailure::Io {
                        path: PathBuf::from(r"C:\dispatch-error.bin"),
                        block_offset: 0,
                        source: std::io::Error::other("测试 dispatcher 读取许可失败"),
                    });
                }
                active.fetch_add(1, Ordering::SeqCst);
                Ok(CountingPermit(active))
            })
        }
    }

    /// 只门控目标 Hash，使取消前能够同时保留一个真实 Hash future。
    #[derive(Clone)]
    struct GatedHashReader {
        /// 进入门控读取时置位，避免通知先后造成竞态。
        entered: Arc<AtomicUsize>,
        /// 未取消时才允许 Hash 返回的闸门。
        gate: Arc<Notify>,
        /// 需要被门控的规范路径。
        gated_path: String,
        /// 两条测试 Hash 共用的固定 MD5。
        md5: [u8; 16],
    }

    impl HashPermitReader for GatedHashReader {
        type Permit = CountingPermit;

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
            let entered = Arc::clone(&self.entered);
            let gate = Arc::clone(&self.gate);
            let gated_path = self.gated_path.clone();
            let md5 = self.md5;
            Box::pin(async move {
                if scanned.normalized_path.as_str() == gated_path {
                    entered.store(1, Ordering::SeqCst);
                    gate.notified().await;
                }
                Ok(ReadProduct { md5, lease: permit })
            })
        }
    }

    /// 永不自行结束的 Hash 读取器，用于证明 dispatcher 错误会收束既有 owner。
    #[derive(Clone)]
    struct HoldingHashReader {
        /// 已进入读取的 future 数量。
        entered: Arc<AtomicUsize>,
    }

    impl HashPermitReader for HoldingHashReader {
        type Permit = CountingPermit;

        fn read_with_permit(
            &self,
            _scanned: ScannedPath,
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
            let entered = Arc::clone(&self.entered);
            Box::pin(async move {
                entered.fetch_add(1, Ordering::SeqCst);
                let _permit = permit;
                std::future::pending::<Result<ReadProduct<CountingPermit>, ReadFailure>>().await
            })
        }
    }

    /// 让一个单键远端查询保持在途，并在 future Drop 时归还测试 owner。
    #[derive(Clone)]
    struct GatedRemote {
        /// 已进入远端查询的持久状态。
        entered: Arc<AtomicUsize>,
        /// 当前仍在途的远端 future 数量。
        active: Arc<AtomicUsize>,
        /// 仅供非取消路径放行的闸门。
        gate: Arc<Notify>,
    }

    impl RemoteFeatureCache for GatedRemote {
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
            assert_eq!(keys.len(), 1, "流式远端查询必须保持单键");
            self.entered.store(1, Ordering::SeqCst);
            self.active.fetch_add(1, Ordering::SeqCst);
            struct RemoteOwner(Arc<AtomicUsize>);
            impl Drop for RemoteOwner {
                fn drop(&mut self) {
                    self.0.fetch_sub(1, Ordering::SeqCst);
                }
            }
            let _owner = RemoteOwner(Arc::clone(&self.active));
            self.gate.notified().await;
            Ok(vec![None])
        }

        async fn publish_outbox(
            &mut self,
            _machine_id: &MachineId,
            _batch: &dedup_protocol::proto::SyncChangeBatch,
        ) -> Result<u64, RemoteCacheError> {
            Ok(0)
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

    fn input(
        scanned: ScannedPath,
        lane: TaskDiskLane,
        cached: Option<BaseCacheRecord>,
    ) -> BaseTaskInput {
        BaseTaskInput {
            planned: PlannedScannedPath { scanned, lane },
            cached,
            contact_sheet_valid: true,
            force_recompute: false,
        }
    }

    fn production<P: TaskLanePermitProvider>(
        root: &Path,
        provider: P,
        inputs: &[BaseTaskInput],
    ) -> crate::scan::BaseTaskProduction<P> {
        let files = TransientTaskFileSet::create(root, RUN_ID).unwrap();
        let mut producer = BaseTaskProducer::new(TaskFileDispatcher::new(files, provider));
        producer.append_batch(inputs).unwrap();
        producer.seal().unwrap()
    }

    fn options(root: &Path) -> TaskFileBaseCoordinatorOptions {
        TaskFileBaseCoordinatorOptions {
            hash_capacity: 2,
            worker_capacity: 1,
            read_config: DiskReadConfig::default(),
            persistence: TaskFileMediaPersistenceOptions {
                contact_sheet_root: root.join("contacts"),
                ..Default::default()
            },
        }
    }

    /// 取消必须先收回 Hash、远端和 Worker owner，才允许任务文件 dispatcher 被丢弃。
    #[tokio::test]
    async fn cancellation_drains_hash_remote_media_and_preserves_unacked_rows() {
        let root = tempfile::tempdir().unwrap();
        let md5 = [0xD4; 16];
        let machine = MachineId::from_sha256([0xD5; 32]);
        let mut store = NodeStore::open_in_memory(machine).unwrap();
        let cached = store
            .upsert_content_and_location(&scanned(r"C:\seed-media.bin", 10), md5, MediaKind::Image)
            .unwrap();
        let cached = store.load_base_cache_record(cached.id).unwrap();
        let permits = Arc::new(AtomicUsize::new(0));
        let media_path = scanned(r"C:\media-active.bin", 10);
        let hash_path = scanned(r"C:\hash-gated.bin", 10);
        let remote_path = scanned(r"C:\remote-gated.bin", 10);
        let pending = TaskFileBaseComputePending::from_production(production(
            root.path(),
            CountingProvider(Arc::clone(&permits)),
            &[
                input(media_path, lane(41), Some(cached)),
                input(hash_path.clone(), lane(42), None),
                input(remote_path.clone(), lane(43), None),
            ],
        ));
        let hash_entered = Arc::new(AtomicUsize::new(0));
        let remote_entered = Arc::new(AtomicUsize::new(0));
        let remote_active = Arc::new(AtomicUsize::new(0));
        let remote = Arc::new(GatedRemote {
            entered: Arc::clone(&remote_entered),
            active: Arc::clone(&remote_active),
            gate: Arc::new(Notify::new()),
        });
        let reader = GatedHashReader {
            entered: Arc::clone(&hash_entered),
            gate: Arc::new(Notify::new()),
            gated_path: hash_path.normalized_path.as_str().to_owned(),
            md5,
        };
        let (actor, handle, mut acknowledgements) =
            crate::scan::base_persistence::BaseStoreActor::spawn(store, 2);
        let (mut pool, mut started, _controller) = WorkerPool::controlled_batch_for_test(1);
        let cancellation = ReadCancellationToken::new();
        let cancellation_for_run = cancellation.clone();
        let run_root = root.path().to_path_buf();
        let run_handle = handle.clone();
        let join = tokio::spawn(async move {
            let mut remote_available = true;
            let mut warning = None;
            let result = run_task_file_base_stream(
                pending,
                reader,
                &mut pool,
                &run_handle,
                &mut acknowledgements,
                &options(&run_root),
                cancellation_for_run,
                remote,
                &mut remote_available,
                &mut warning,
                None,
            )
            .await;
            let busy_workers = pool.busy_workers();
            let shutdown = pool.shutdown().await;
            (result, busy_workers, shutdown)
        });

        tokio::time::timeout(Duration::from_secs(1), started.recv())
            .await
            .expect("Media A 必须先进入 Worker")
            .expect("Worker 启动通道不得关闭");
        tokio::time::timeout(Duration::from_secs(1), async {
            while hash_entered.load(Ordering::SeqCst) == 0
                || remote_entered.load(Ordering::SeqCst) == 0
            {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("取消前必须同时存在 gated Hash 与 gated remote");
        cancellation.cancel();

        let (result, busy_workers, shutdown) = tokio::time::timeout(Duration::from_secs(2), join)
            .await
            .expect("取消必须在两秒内收束所有 owner")
            .unwrap();
        assert!(shutdown.is_ok());
        let error = match result {
            Ok(_) => panic!("取消必须返回携带唯一 pending owner 的错误"),
            Err(error) => error,
        };
        assert_eq!(
            permits.load(Ordering::SeqCst),
            0,
            "Hash/Media permit 必须归还"
        );
        assert_eq!(
            remote_active.load(Ordering::SeqCst),
            0,
            "远端 future 必须 drain"
        );
        assert_eq!(busy_workers, 0, "Worker 必须回到 idle");
        let mut pending = error.into_pending();
        for disk in [41, 42, 43] {
            assert_eq!(
                std::fs::read(pending.dispatcher.lane_path(&lane(disk)).unwrap()).unwrap()[0],
                b'P',
                "未 ACK 行必须保持 P"
            );
        }
        pending
            .dispatcher
            .discard()
            .expect("所有外部 owner 清空后 dispatcher 必须可以 discard");
        drop(handle);
        actor.finish().await.unwrap();
    }

    /// dispatcher 失败必须先回收已在途 Hash permit，再归还可 discard 的唯一 pending owner。
    #[tokio::test]
    async fn dispatch_error_releases_active_hash_owners_before_returning_pending() {
        let root = tempfile::tempdir().unwrap();
        let active = Arc::new(AtomicUsize::new(0));
        let entered = Arc::new(AtomicUsize::new(0));
        let provider = ErrorAfterPermitProvider {
            attempts: Arc::new(AtomicUsize::new(0)),
            active: Arc::clone(&active),
        };
        let inputs = [
            input(scanned(r"C:\dispatch-first.bin", 11), lane(51), None),
            input(scanned(r"C:\dispatch-second.bin", 12), lane(52), None),
            input(scanned(r"C:\dispatch-third.bin", 13), lane(53), None),
        ];
        let pending =
            TaskFileBaseComputePending::from_production(production(root.path(), provider, &inputs));
        let store = NodeStore::open_in_memory(MachineId::from_sha256([0xD6; 32])).unwrap();
        let (actor, handle, mut acknowledgements) =
            crate::scan::base_persistence::BaseStoreActor::spawn(store, 2);
        let (mut pool, _started, _controller) = WorkerPool::controlled_batch_for_test(1);
        let reader = HoldingHashReader {
            entered: Arc::clone(&entered),
        };
        let mut stream_options = options(root.path());
        stream_options.hash_capacity = 3;
        let mut remote_available = false;
        let mut warning = None;
        let result = tokio::time::timeout(
            Duration::from_secs(2),
            run_task_file_base_stream(
                pending,
                reader,
                &mut pool,
                &handle,
                &mut acknowledgements,
                &stream_options,
                ReadCancellationToken::new(),
                Arc::new(crate::DisabledRemoteFeatureCache),
                &mut remote_available,
                &mut warning,
                None,
            ),
        )
        .await
        .expect("dispatcher 错误必须在收束 Hash owner 后返回");
        let error = match result {
            Ok(_) => panic!("第三次许可失败必须返回任务级错误"),
            Err(error) => error,
        };
        assert!(entered.load(Ordering::SeqCst) <= 2);
        assert_eq!(active.load(Ordering::SeqCst), 0, "Hash permit 必须全部释放");
        let mut pending = error.into_pending();
        for disk in [51, 52, 53] {
            assert_eq!(
                std::fs::read(pending.dispatcher.lane_path(&lane(disk)).unwrap()).unwrap()[0],
                b'P',
                "未 ACK 行必须保持 P"
            );
        }
        pending
            .dispatcher
            .discard()
            .expect("dispatcher 错误清理后必须允许 discard");
        assert!(pool.shutdown().await.is_ok());
        drop(handle);
        actor.finish().await.unwrap();
    }
}
