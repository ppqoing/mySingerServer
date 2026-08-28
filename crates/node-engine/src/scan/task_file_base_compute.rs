//! 瞬态任务文件基础 Hash 阶段的最小协调边界。
//!
//! 本模块只负责已封闭任务文件的 Hash 读取、内容缓存批量查询和 taskless 持久化 ACK。
//! Worker 媒体计算、actor 生命周期和扫描收尾由后续阶段接管。

use std::{
    collections::{BTreeMap, VecDeque},
    fmt,
};

use dedup_core::{ContentKey, MediaKind};
use dedup_node_store::ResolvedScanFile;
use dedup_windows::ReadCancellationToken;
use tokio::sync::mpsc::UnboundedReceiver;

use super::{
    BaseComputeDecision, BaseTaskManifest, BaseTaskProduction, HashPermitReader, ReadProduct,
};
use crate::{
    io::ReadFailure,
    scan::base_persistence::{
        BasePersistAck, BasePersistIdentity, BasePersistMessage, BasePersistOutcome,
        BasePersistSendError, BaseStoreHandle,
    },
    task_dispatch::{
        TaskDispatchAdmission, TaskDispatchError, TaskDispatchPoll, TaskLanePermitProvider,
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
}

impl<P: TaskLanePermitProvider> TaskFileBaseComputePending<P> {
    /// 将已暂停的 Hash 阶段状态还原为通用生产结果，便于后续阶段接管。
    pub(crate) fn from_production(production: BaseTaskProduction<P>) -> Self {
        let BaseTaskProduction {
            dispatcher,
            contexts,
            manifest,
        } = production;
        Self {
            dispatcher,
            contexts,
            manifest,
        }
    }

    /// 把 pending 状态重新交回基础任务生产结果所有权。
    pub(crate) fn into_production(self) -> BaseTaskProduction<P> {
        BaseTaskProduction {
            dispatcher: self.dispatcher,
            manifest: self.manifest,
            contexts: self.contexts,
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
    store: &BaseStoreHandle,
    acknowledgements: &mut UnboundedReceiver<BasePersistAck>,
    cancellation: ReadCancellationToken,
) -> Result<TaskFileBaseComputePending<P>, TaskFileBaseComputeError<P>>
where
    P: TaskLanePermitProvider,
    H: HashPermitReader<Permit = P::Permit>,
{
    let mut pending = TaskFileBaseComputePending::from_production(production);
    let mut persist_queue = VecDeque::<PendingPersist>::new();
    let mut persist_in_flight = BTreeMap::<TaskFileIdentity, PersistAction>::new();
    // Identity 的 needs_md5 位是生产端写入的唯一 Hash 工作计数；这样在最后一项
    // 读取完成、但尚未收到 SQLite ACK 时也能结束 Hash 阶段，不向 dispatcher 等待
    // 已经没有队首的任务文件发布通知。
    let mut hash_rows_remaining = pending
        .contexts
        .keys()
        .filter(|identity| identity.missing().needs_md5())
        .count();

    while hash_rows_remaining > 0 {
        if cancellation.is_cancelled() {
            return Err(task_error(pending, "基础 Hash 阶段已取消"));
        }

        let mut hashed = Vec::new();
        let mut stop_after_batch = false;
        while hash_rows_remaining > 0 && hashed.len() < MAX_HASH_LOOKUP_BATCH {
            let dispatch = pending
                .dispatcher
                .next_with_admission(cancellation.clone(), TaskDispatchAdmission::hash_only())
                .await;
            match dispatch {
                Ok(TaskDispatchPoll::Task(task)) => {
                    if task.record.known_md5.is_some() || !task.record.missing.needs_md5() {
                        return Err(task_error(pending, "Hash admission 派发了已知 MD5 任务"));
                    }
                    hash_rows_remaining -= 1;
                    let identity = task.identity.clone();
                    if !pending.contexts.contains_key(&identity) {
                        return Err(task_error(pending, "Hash 任务缺少对应的内存上下文"));
                    }
                    let record = task.record.clone();
                    let scanned = record.scanned.clone();
                    let read = reader
                        .read_with_permit(scanned.clone(), task.permit, cancellation.clone(), None)
                        .await;
                    match read {
                        Ok(ReadProduct { md5, lease }) => {
                            // Hash 读取完成后立即释放调度 permit，查询和 SQLite ACK 不占用读额度。
                            drop(lease);
                            hashed.push(HashedTask {
                                identity,
                                record,
                                md5,
                            });
                        }
                        Err(ReadFailure::Cancelled) => {
                            return Err(task_error(pending, "基础 Hash 阶段已取消"));
                        }
                        Err(error) => {
                            persist_queue.push_back(failed_persist(
                                identity,
                                scanned,
                                error.to_string(),
                            ));
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
                        }
                    }
                }
                Ok(TaskDispatchPoll::Drained) | Ok(TaskDispatchPoll::Blocked(_)) => {
                    stop_after_batch = true;
                    break;
                }
                Err(error) => {
                    return Err(task_error(pending, dispatch_error_message(error)));
                }
            }
        }

        if !hashed.is_empty() {
            let keys = hashed
                .iter()
                .map(|item| ContentKey::new(item.md5, item.record.scanned.file_size))
                .collect::<Vec<_>>();
            let cached = match store.lookup_base_cache_by_keys(&keys) {
                Ok(records) => records,
                Err(error) => return Err(task_error(pending, error.to_string())),
            };
            if cached.len() != hashed.len() {
                return Err(task_error(pending, "内容缓存批量查询返回数量不一致"));
            }
            for (hashed, cached) in hashed.into_iter().zip(cached) {
                let Some(context) = pending.contexts.get_mut(&hashed.identity) else {
                    return Err(task_error(pending, "Hash 结果缺少对应的内存上下文"));
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
                        return Err(task_error(pending, "完整内容缓存缺少记录"));
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
                    let Some(missing) = TaskWorkMask::for_base(false, decision.missing_parts())
                    else {
                        return Err(task_error(pending, "Hash 后基础缺失掩码无效"));
                    };
                    let media_record = TaskFileRecord {
                        item_id: hashed.record.item_id,
                        work_kind: hashed.record.work_kind,
                        scanned: hashed.record.scanned,
                        known_md5: Some(hashed.md5),
                        missing,
                    };
                    if let Err(error) = pending
                        .dispatcher
                        .request_media_continuation(&hashed.identity, &media_record)
                    {
                        return Err(task_error(pending, error.to_string()));
                    }
                }
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
        }

        if stop_after_batch {
            break;
        }
    }

    while !persist_in_flight.is_empty() || !persist_queue.is_empty() {
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
    }
    Ok(pending)
}

/// 将单文件读取失败包装成只在 ACK 后写 F 的消息。
fn failed_persist(
    identity: TaskFileIdentity,
    scanned: dedup_node_store::ScannedPath,
    message: String,
) -> PendingPersist {
    let display_path = scanned
        .display_path
        .as_path()
        .to_string_lossy()
        .into_owned();
    let operation_message = message.clone();
    let operation = BasePersistMessage::new_task_file(identity.clone(), move |_store| {
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
        path::Path,
        pin::Pin,
        sync::{
            Arc,
            atomic::{AtomicUsize, Ordering},
        },
    };

    use dedup_core::{DisplayPath, MachineId, MediaKind, NormalizedPath};
    use dedup_node_store::{BaseCacheRecord, NodeStore, ScannedPath};
    use dedup_windows::{LocalDiskKind, PhysicalDiskId, ReadCancellationToken};

    use crate::{
        io::{DiskReadClass, ReadFailure},
        scan::{
            BaseTaskInput, BaseTaskProducer, BaseTaskProduction, HashPermitReader,
            PlannedScannedPath, ReadProduct, TaskDiskLane,
        },
        task_dispatch::{TaskFileDispatcher, TaskLanePermitFuture, TaskLanePermitProvider},
        task_files::TransientTaskFileSet,
    };

    use super::{TaskFileBaseComputePending, run_task_file_base_compute};

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

    fn production(
        root: &Path,
        inputs: &[BaseTaskInput],
        provider: CountingPermitProvider,
    ) -> BaseTaskProduction<CountingPermitProvider> {
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

    fn cleanup_pending(mut pending: TaskFileBaseComputePending<CountingPermitProvider>) {
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
        actor.finish().await.unwrap();
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
