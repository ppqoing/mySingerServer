//! 瞬态基础任务的 Hash、Media 与 SQLite ACK 交错协调边界。
//!
//! 本模块只负责组合已经独立验证的阶段函数；任务 actor、收尾和恢复由上层负责。

use std::{fmt, sync::Arc};

use dedup_core::DiskReadConfig;
use dedup_windows::ReadCancellationToken;
use tokio::sync::mpsc::UnboundedReceiver;

use super::{
    BaseTaskManifest, BaseTaskProduction, HashPermitReader,
    base_persistence::{BasePersistAck, BaseStoreHandle},
    task_file_base_compute::TaskFileBaseComputePending,
    task_file_base_stream::run_task_file_base_stream,
    task_file_media_persistence::TaskFileMediaPersistenceOptions,
};
use crate::{
    RemoteFeatureCache,
    runtime_tasks::RuntimeTaskReporter,
    task_dispatch::{TaskDispatchAdmission, TaskDispatchPoll, TaskLanePermitProvider},
    worker::WorkerPool,
};

/// 瞬态基础任务每轮阶段调用所需的固定容量和持久化配置。
pub(crate) struct TaskFileBaseCoordinatorOptions {
    /// Hash 读取同时在途的最大数量。
    pub(crate) hash_capacity: usize,
    /// Media Worker 同时在途的最大数量。
    pub(crate) worker_capacity: usize,
    /// 物理盘读取和重试配置快照。
    pub(crate) read_config: DiskReadConfig,
    /// Media taskless 持久化使用的联系表和 artifact 配置。
    pub(crate) persistence: TaskFileMediaPersistenceOptions,
}

/// 瞬态基础任务正常完成时返回的累计汇总。
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct TaskFileBaseCoordinatorSummary {
    /// 已确认失败的文件数；只统计单文件 F，不含任务级错误。
    pub(crate) file_failures: usize,
    /// 已由 SQLite ACK 确认的缓存命中数。
    pub(crate) cache_hits: usize,
}

/// 瞬态基础任务正常完成后仍保留的 manifest 和 dispatcher owner。
pub(crate) struct TaskFileBaseCoordinatorResult<P: TaskLanePermitProvider> {
    /// 已排序的扫描清单和成功 resolved 文件。
    pub(crate) manifest: BaseTaskManifest,
    /// 本次协调循环的累计结果统计。
    pub(crate) summary: TaskFileBaseCoordinatorSummary,
    /// 已经真正 Drained 的任务文件 owner；上层负责决定何时 discard。
    pub(crate) dispatcher: crate::task_dispatch::TaskFileDispatcher<P>,
}

/// 协调器取消或基础设施错误时携带精确 pending owner。
pub(crate) struct TaskFileBaseCoordinatorError<P: TaskLanePermitProvider> {
    /// 面向上层日志的阶段错误文本。
    message: String,
    /// 仍拥有未 ACK 任务行和 dispatcher 的 pending。
    pending: TaskFileBaseComputePending<P>,
    /// 是否由取消令牌触发，而非 Store/Worker/dispatcher 错误。
    cancelled: bool,
}

impl<P: TaskLanePermitProvider> TaskFileBaseCoordinatorError<P> {
    /// 返回该错误是否为用户取消。
    pub(crate) const fn is_cancelled(&self) -> bool {
        self.cancelled
    }

    /// 消费错误并取回精确 pending owner，供上层收束或 discard。
    pub(crate) fn into_pending(self) -> TaskFileBaseComputePending<P> {
        self.pending
    }
}

impl<P: TaskLanePermitProvider> fmt::Display for TaskFileBaseCoordinatorError<P> {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(&self.message)
    }
}

impl<P: TaskLanePermitProvider> fmt::Debug for TaskFileBaseCoordinatorError<P> {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("TaskFileBaseCoordinatorError")
            .field("message", &self.message)
            .field("cancelled", &self.cancelled)
            .finish_non_exhaustive()
    }
}

/// 交错运行 Hash、Media 和 taskless SQLite ACK 的基础任务协调入口。
pub(crate) async fn run_task_file_base_coordinator<P, H>(
    production: BaseTaskProduction<P>,
    reader: H,
    worker_pool: &mut WorkerPool,
    store: &BaseStoreHandle,
    acknowledgements: &mut UnboundedReceiver<BasePersistAck>,
    options: TaskFileBaseCoordinatorOptions,
    cancellation: ReadCancellationToken,
) -> Result<TaskFileBaseCoordinatorResult<P>, TaskFileBaseCoordinatorError<P>>
where
    P: TaskLanePermitProvider,
    H: HashPermitReader<Permit = P::Permit>,
{
    let remote = Arc::new(crate::DisabledRemoteFeatureCache);
    let mut remote_available = false;
    let mut warning = None;
    run_task_file_base_coordinator_with_remote(
        production,
        reader,
        worker_pool,
        store,
        acknowledgements,
        options,
        cancellation,
        remote,
        &mut remote_available,
        &mut warning,
    )
    .await
}

/// 运行支持可选远端缓存的瞬态基础任务协调循环。
pub(crate) async fn run_task_file_base_coordinator_with_remote<P, H, R>(
    production: BaseTaskProduction<P>,
    reader: H,
    worker_pool: &mut WorkerPool,
    store: &BaseStoreHandle,
    acknowledgements: &mut UnboundedReceiver<BasePersistAck>,
    options: TaskFileBaseCoordinatorOptions,
    cancellation: ReadCancellationToken,
    remote: Arc<R>,
    remote_available: &mut bool,
    warning: &mut Option<String>,
) -> Result<TaskFileBaseCoordinatorResult<P>, TaskFileBaseCoordinatorError<P>>
where
    P: TaskLanePermitProvider,
    H: HashPermitReader<Permit = P::Permit>,
    R: RemoteFeatureCache,
{
    run_task_file_base_coordinator_inner(
        production,
        reader,
        worker_pool,
        store,
        acknowledgements,
        options,
        cancellation,
        remote,
        remote_available,
        warning,
        None,
    )
    .await
}

/// 运行带 Worker 运行时遥测的瞬态基础协调循环；底层状态机与 SQLite ACK 不变。
pub(crate) async fn run_task_file_base_coordinator_with_runtime<P, H, R>(
    production: BaseTaskProduction<P>,
    reader: H,
    worker_pool: &mut WorkerPool,
    store: &BaseStoreHandle,
    acknowledgements: &mut UnboundedReceiver<BasePersistAck>,
    options: TaskFileBaseCoordinatorOptions,
    cancellation: ReadCancellationToken,
    remote: Arc<R>,
    remote_available: &mut bool,
    warning: &mut Option<String>,
    reporter: &RuntimeTaskReporter,
) -> Result<TaskFileBaseCoordinatorResult<P>, TaskFileBaseCoordinatorError<P>>
where
    P: TaskLanePermitProvider,
    H: HashPermitReader<Permit = P::Permit>,
    R: RemoteFeatureCache,
{
    run_task_file_base_coordinator_inner(
        production,
        reader,
        worker_pool,
        store,
        acknowledgements,
        options,
        cancellation,
        remote,
        remote_available,
        warning,
        Some(reporter),
    )
    .await
}

/// 共享瞬态基础协调循环；报告器只增加运行时投影，不改变任务文件所有权。
async fn run_task_file_base_coordinator_inner<P, H, R>(
    production: BaseTaskProduction<P>,
    reader: H,
    worker_pool: &mut WorkerPool,
    store: &BaseStoreHandle,
    acknowledgements: &mut UnboundedReceiver<BasePersistAck>,
    options: TaskFileBaseCoordinatorOptions,
    cancellation: ReadCancellationToken,
    remote: Arc<R>,
    remote_available: &mut bool,
    warning: &mut Option<String>,
    reporter: Option<&RuntimeTaskReporter>,
) -> Result<TaskFileBaseCoordinatorResult<P>, TaskFileBaseCoordinatorError<P>>
where
    P: TaskLanePermitProvider,
    H: HashPermitReader<Permit = P::Permit>,
    R: RemoteFeatureCache,
{
    let pending = TaskFileBaseComputePending::from_production(production);
    let pending = run_task_file_base_stream(
        pending,
        reader,
        worker_pool,
        store,
        acknowledgements,
        &options,
        cancellation.clone(),
        remote,
        remote_available,
        warning,
        reporter,
    )
    .await
    .map_err(|error| {
        let message = error.to_string();
        coordinator_error(error.into_pending(), message, cancellation.is_cancelled())
    })?;
    finish_coordinator(pending, cancellation).await
}

/// 确认所有任务行已经进入终态，再把 dispatcher owner 交回上层。
async fn finish_coordinator<P: TaskLanePermitProvider>(
    mut pending: TaskFileBaseComputePending<P>,
    cancellation: ReadCancellationToken,
) -> Result<TaskFileBaseCoordinatorResult<P>, TaskFileBaseCoordinatorError<P>> {
    if cancellation.is_cancelled() {
        return Err(coordinator_error(pending, "基础任务已取消", true));
    }
    match pending
        .dispatcher
        .next_with_admission(cancellation.clone(), TaskDispatchAdmission::all())
        .await
    {
        Ok(TaskDispatchPoll::Drained) => {
            let TaskFileBaseComputePending {
                dispatcher,
                contexts,
                manifest,
                remaining_hash_rows,
                blocked_reason,
            } = pending;
            debug_assert!(contexts.is_empty());
            debug_assert_eq!(remaining_hash_rows, 0);
            debug_assert!(blocked_reason.is_none());
            let file_failures = manifest
                .seen_paths
                .len()
                .saturating_sub(manifest.resolved_files.len());
            let summary = TaskFileBaseCoordinatorSummary {
                file_failures,
                cache_hits: manifest.cache_hits,
            };
            Ok(TaskFileBaseCoordinatorResult {
                manifest,
                summary,
                dispatcher,
            })
        }
        Ok(TaskDispatchPoll::Task(task)) => {
            let identity = task.identity.clone();
            drop(task.permit);
            let _ = pending.dispatcher.abandon_in_flight(&identity);
            Err(coordinator_error(
                pending,
                "任务文件在上下文为空时仍返回未完成任务",
                false,
            ))
        }
        Ok(TaskDispatchPoll::Blocked(reason)) => Err(coordinator_error(
            pending,
            format!("任务文件最终收束仍被阻塞: {reason:?}"),
            false,
        )),
        Err(error) => Err(coordinator_error(
            pending,
            format!("任务文件最终收束失败: {error}"),
            cancellation.is_cancelled(),
        )),
    }
}

/// 构造仍携带任务文件 owner 的协调器错误。
fn coordinator_error<P: TaskLanePermitProvider>(
    mut pending: TaskFileBaseComputePending<P>,
    message: impl Into<String>,
    cancelled: bool,
) -> TaskFileBaseCoordinatorError<P> {
    // 阶段函数已经收回 Worker/读取许可；这里再按上下文逐项解除真正领取过的行，
    // 让上层拿到的 owner 可以精确 discard，而不会把 P 行误写成 F。
    let identities = pending.contexts.keys().cloned().collect::<Vec<_>>();
    for identity in identities {
        let _ = pending.dispatcher.abandon_in_flight(&identity);
    }
    TaskFileBaseCoordinatorError {
        message: message.into(),
        pending,
        cancelled,
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
        time::Duration,
    };
    use tokio::sync::Notify;

    use dedup_core::{
        ContentKey, DiskReadConfig, DisplayPath, MachineId, MediaKind, NormalizedPath,
    };
    use dedup_media::{ImageStage1, PdqHash};
    use dedup_media_ffmpeg::MediaProbe;
    use dedup_node_store::{BaseCacheRecord, CompleteStage1, NodeStore, ScannedPath};
    use dedup_windows::{LocalDiskKind, PhysicalDiskId, ReadCancellationToken};

    use crate::{
        RemoteCacheError, RemoteFeatureCache,
        io::{DiskReadClass, ReadFailure},
        scan::{
            BaseTaskInput, BaseTaskProducer, BaseTaskProduction, HashPermitReader,
            PlannedScannedPath, ReadProduct, TaskDiskLane,
        },
        task_dispatch::{TaskFileDispatcher, TaskLanePermitFuture, TaskLanePermitProvider},
        task_files::TransientTaskFileSet,
        worker::{BaseComputeOutput, Stage1Frame, WorkerPool},
    };

    use super::{
        TaskFileBaseCoordinatorOptions, run_task_file_base_coordinator,
        run_task_file_base_coordinator_with_remote,
    };

    const RUN_ID: &str = "01900000-0000-7000-8000-0000000003c3";

    #[derive(Clone, Copy, Debug)]
    struct TestPermit;

    #[derive(Clone, Default)]
    struct TestProvider;

    impl TaskLanePermitProvider for TestProvider {
        type Permit = TestPermit;

        fn acquire(
            &self,
            _lane: TaskDiskLane,
            _class: DiskReadClass,
            _cancellation: ReadCancellationToken,
        ) -> TaskLanePermitFuture<Self::Permit> {
            Box::pin(async { Ok(TestPermit) })
        }
    }

    #[derive(Clone, Default)]
    struct TestHashReader {
        results: Arc<BTreeMap<String, [u8; 16]>>,
        calls: Arc<AtomicUsize>,
    }

    /// 用于验证 Hash 完成不能等待同窗口后续读取的受控读取器。
    #[derive(Clone)]
    struct GatedHashReader {
        /// 立即完成的首个路径。
        first_path: String,
        /// 被门控的后续路径。
        later_path: String,
        /// 后续读取已实际进入的持久状态；通知建立较晚也不会丢失。
        later_entered: Arc<AtomicUsize>,
        /// 允许后续读取返回的门控通知。
        later_gate: Arc<Notify>,
        /// 两个路径各自返回的 MD5。
        md5: [u8; 16],
    }

    impl HashPermitReader for GatedHashReader {
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
            let path = scanned.normalized_path.as_str().to_owned();
            let first_path = self.first_path.clone();
            let later_path = self.later_path.clone();
            let later_entered = Arc::clone(&self.later_entered);
            let later_gate = Arc::clone(&self.later_gate);
            let md5 = self.md5;
            Box::pin(async move {
                if path == later_path {
                    later_entered.store(1, Ordering::SeqCst);
                    // 让测试先发送门控事件，验证 notify_one 的持久 permit 不会丢失。
                    tokio::task::yield_now().await;
                    later_gate.notified().await;
                } else if path != first_path {
                    return Err(ReadFailure::Io {
                        path: scanned.display_path.as_path().to_path_buf(),
                        block_offset: 0,
                        source: std::io::Error::other("测试 Hash 路径不匹配"),
                    });
                }
                Ok(ReadProduct { md5, lease: permit })
            })
        }
    }

    /// 两个单键远端请求分别受控，用于验证失败后的在途结果不会污染 SQLite。
    #[derive(Clone)]
    struct FailingThenSuccessfulRemote {
        /// 返回错误的内容键。
        failing_key: ContentKey,
        /// 返回更完整记录的内容键。
        succeeding_key: ContentKey,
        /// 成功请求的远端完整记录。
        succeeding_record: BaseCacheRecord,
        /// 两个请求均已进入的持久计数。
        entered: Arc<AtomicUsize>,
        /// 放行失败请求。
        failure_gate: Arc<Notify>,
        /// 放行成功请求。
        success_gate: Arc<Notify>,
    }

    impl RemoteFeatureCache for FailingThenSuccessfulRemote {
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
            assert_eq!(keys.len(), 1, "远端流必须保持单键查询");
            self.entered.fetch_add(1, Ordering::SeqCst);
            if keys[0] == self.failing_key {
                self.failure_gate.notified().await;
                Err(RemoteCacheError::ConnectTimeout)
            } else if keys[0] == self.succeeding_key {
                self.success_gate.notified().await;
                Ok(vec![Some(self.succeeding_record.clone())])
            } else {
                Ok(vec![None])
            }
        }

        async fn publish_outbox(
            &mut self,
            _machine_id: &MachineId,
            _batch: &dedup_protocol::proto::SyncChangeBatch,
        ) -> Result<u64, RemoteCacheError> {
            Ok(0)
        }
    }

    /// 首项和次项可控、第三项立即完成的读取器，用于跨 lane 公平性测试。
    #[derive(Clone)]
    struct FairHashReader {
        /// 首项路径。
        first_path: String,
        /// 次项路径。
        second_path: String,
        /// 第三项路径。
        third_path: String,
        /// 首项放行门闩。
        first_gate: Arc<Notify>,
        /// 次项已进入的持久状态。
        second_entered: Arc<AtomicUsize>,
        /// 次项放行门闩。
        second_gate: Arc<Notify>,
        /// 第三项已进入的持久状态。
        third_entered: Arc<AtomicUsize>,
        /// 三项使用的测试 MD5。
        md5: [u8; 16],
    }

    impl HashPermitReader for FairHashReader {
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
            let path = scanned.normalized_path.as_str().to_owned();
            let first_path = self.first_path.clone();
            let second_path = self.second_path.clone();
            let third_path = self.third_path.clone();
            let first_gate = Arc::clone(&self.first_gate);
            let second_entered = Arc::clone(&self.second_entered);
            let second_gate = Arc::clone(&self.second_gate);
            let third_entered = Arc::clone(&self.third_entered);
            let md5 = self.md5;
            Box::pin(async move {
                if path == first_path {
                    first_gate.notified().await;
                } else if path == second_path {
                    second_entered.store(1, Ordering::SeqCst);
                    second_gate.notified().await;
                } else if path == third_path {
                    third_entered.store(1, Ordering::SeqCst);
                } else {
                    return Err(ReadFailure::Io {
                        path: scanned.display_path.as_path().to_path_buf(),
                        block_offset: 0,
                        source: std::io::Error::other("公平性测试 Hash 路径不匹配"),
                    });
                }
                Ok(ReadProduct { md5, lease: permit })
            })
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
            let key = scanned.normalized_path.as_str().to_owned();
            let result = self.results.get(&key).copied();
            let path = scanned.display_path.as_path().to_path_buf();
            let calls = Arc::clone(&self.calls);
            Box::pin(async move {
                calls.fetch_add(1, Ordering::SeqCst);
                result.map_or_else(
                    || {
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

    fn seed_partial(
        store: &mut NodeStore,
        path: &str,
        md5: [u8; 16],
        file_size: u64,
        media_kind: MediaKind,
    ) -> BaseCacheRecord {
        let content = store
            .upsert_content_and_location(&scanned(path, file_size), md5, media_kind)
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

    fn production(root: &Path, inputs: &[BaseTaskInput]) -> BaseTaskProduction<TestProvider> {
        let files = TransientTaskFileSet::create(root, RUN_ID).unwrap();
        let mut producer = BaseTaskProducer::new(TaskFileDispatcher::new(files, TestProvider));
        producer.append_batch(inputs).unwrap();
        producer.seal().unwrap()
    }

    fn image_output() -> BaseComputeOutput {
        BaseComputeOutput {
            probe: Some(MediaProbe {
                media_kind: MediaKind::Image,
                width: 2,
                height: 2,
                duration_ms: None,
            }),
            stage1_frames: Some(vec![Stage1Frame {
                slot: 0,
                feature: Some(ImageStage1 {
                    width: 2,
                    height: 2,
                    pdq: PdqHash::from_bytes([0; 32]),
                    quality: 100,
                }),
                error: None,
            }]),
            contact_sheet_jpeg: None,
        }
    }

    /// 构造只存在于远端的完整图片缓存，导入后会把本地 partial 升级为完整命中。
    fn complete_remote_image(md5: [u8; 16], file_size: u64) -> BaseCacheRecord {
        BaseCacheRecord {
            content_id: None,
            content_key: ContentKey::new(md5, file_size),
            media_kind: MediaKind::Image,
            base_complete: true,
            width: Some(2),
            height: Some(2),
            duration_ms: None,
            stage1: Some(CompleteStage1::Image(ImageStage1 {
                width: 2,
                height: 2,
                pdq: PdqHash::from_bytes([0; 32]),
                quality: 100,
            })),
            image_stage2: None,
            video_stage2: Box::new([None; 6]),
            contact_sheet_relative_path: None,
        }
    }

    fn options(root: &Path) -> TaskFileBaseCoordinatorOptions {
        TaskFileBaseCoordinatorOptions {
            hash_capacity: 1,
            worker_capacity: 1,
            read_config: DiskReadConfig::default(),
            persistence: crate::scan::TaskFileMediaPersistenceOptions {
                contact_sheet_root: root.join("contacts"),
                ..Default::default()
            },
        }
    }

    /// 读取 lane 文件的终态字节，确认只有 ACK 后才从 P 迁移到 C/F。
    fn statuses<P: TaskLanePermitProvider>(
        dispatcher: &crate::task_dispatch::TaskFileDispatcher<P>,
        lane: &TaskDiskLane,
    ) -> Vec<u8> {
        let path = dispatcher.lane_path(lane).unwrap();
        std::fs::read_to_string(path)
            .unwrap()
            .lines()
            .filter(|line| !line.is_empty())
            .map(|line| line.as_bytes()[0])
            .collect()
    }

    #[cfg(feature = "test-hooks")]
    #[tokio::test]
    async fn media_is_persisted_before_hash_starts() {
        let root = tempfile::tempdir().unwrap();
        let md5 = [0xA1; 16];
        let machine = MachineId::from_sha256([0xA2; 32]);
        let mut store = NodeStore::open_in_memory(machine).unwrap();
        let cached = seed_partial(&mut store, r"C:\seed-image.bin", md5, 10, MediaKind::Image);
        let hash_calls = Arc::new(AtomicUsize::new(0));
        let production = production(
            root.path(),
            &[
                input(scanned(r"C:\media-first.bin", 10), lane(7), Some(cached)),
                input(scanned(r"C:\hash-next.bin", 10), lane(7), None),
            ],
        );
        let hash_key = NormalizedPath::new(r"C:\hash-next.bin")
            .unwrap()
            .as_str()
            .to_owned();
        let reader = TestHashReader {
            results: Arc::new(BTreeMap::from([(hash_key, md5)])),
            calls: Arc::clone(&hash_calls),
        };
        let (persist_control, persist_waiter) =
            crate::scan::base_persistence::BasePersistTestController::new();
        let (actor, handle, mut acknowledgements) =
            crate::scan::base_persistence::BaseStoreActor::spawn_with_first_persist_waiter(
                store,
                2,
                persist_waiter,
            );
        let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
        let handle_for_run = handle.clone();
        let cancellation = ReadCancellationToken::new();
        let run_root = root.path().to_path_buf();
        let join = tokio::spawn(async move {
            let result = run_task_file_base_coordinator(
                production,
                reader,
                &mut pool,
                &handle_for_run,
                &mut acknowledgements,
                options(&run_root),
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
        assert_eq!(hash_calls.load(Ordering::SeqCst), 0);
        controller
            .base_source_read_complete(task_id.clone(), item_id.clone())
            .await;
        controller
            .complete_base(task_id, item_id, md5, image_output())
            .await;
        persist_control.wait_until_entered().await;
        assert_eq!(hash_calls.load(Ordering::SeqCst), 0);
        assert!(
            started.try_recv().is_err(),
            "Media ACK 前不得启动 Hash 后续 Worker"
        );
        persist_control.release();

        let (result, shutdown) = join.await.unwrap();
        assert!(shutdown.is_ok());
        let mut result = result.unwrap();
        assert_eq!(result.summary.file_failures, 0);
        assert_eq!(result.summary.cache_hits, 1);
        assert_eq!(result.manifest.resolved_files.len(), 2);
        assert!(hash_calls.load(Ordering::SeqCst) > 0);
        assert_eq!(statuses(&result.dispatcher, &lane(7)), vec![b'C', b'C']);
        result.dispatcher.discard().unwrap();
        drop(handle);
        actor.finish().await.unwrap();
    }

    /// 首个 Hash miss 必须在同窗口后续 Hash 完成前进入 Worker，避免 Hash 全批屏障。
    #[tokio::test]
    async fn first_hashed_media_miss_enters_worker_before_later_hash_finishes() {
        let root = tempfile::tempdir().unwrap();
        let md5 = [0xA8; 16];
        let machine = MachineId::from_sha256([0xA9; 32]);
        let store = NodeStore::open_in_memory(machine).unwrap();
        let first_path = scanned(r"C:\stream-first.bin", 10);
        let later_path = scanned(r"C:\stream-later.bin", 10);
        let production = production(
            root.path(),
            &[
                input(first_path.clone(), lane(31), None),
                input(later_path.clone(), lane(32), None),
            ],
        );
        let later_entered = Arc::new(AtomicUsize::new(0));
        let later_gate = Arc::new(Notify::new());
        let reader = GatedHashReader {
            first_path: first_path.normalized_path.as_str().to_owned(),
            later_path: later_path.normalized_path.as_str().to_owned(),
            later_entered: Arc::clone(&later_entered),
            later_gate: Arc::clone(&later_gate),
            md5,
        };
        let (actor, handle, mut acknowledgements) =
            crate::scan::base_persistence::BaseStoreActor::spawn(store, 2);
        let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
        let handle_for_run = handle.clone();
        let run_root = root.path().to_path_buf();
        let cancellation = ReadCancellationToken::new();
        let mut coordinator_options = options(&run_root);
        coordinator_options.hash_capacity = 2;
        let join = tokio::spawn(async move {
            let result = run_task_file_base_coordinator(
                production,
                reader,
                &mut pool,
                &handle_for_run,
                &mut acknowledgements,
                coordinator_options,
                cancellation,
            )
            .await;
            let shutdown = pool.shutdown().await;
            (result, shutdown)
        });

        tokio::time::timeout(Duration::from_secs(1), async {
            while later_entered.load(Ordering::SeqCst) == 0 {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("第二个 Hash 应进入门控读取；即使先发生也不能丢失信号");
        let first_worker = tokio::time::timeout(Duration::from_secs(1), started.recv())
            .await
            .expect("首个 Hash 缺失项不得等待后续 Hash")
            .unwrap();
        assert_eq!(handle.lookup_key_batch_sizes_for_test(), vec![1]);
        later_gate.notify_one();
        tokio::time::timeout(Duration::from_secs(1), async {
            loop {
                if handle.lookup_key_batch_sizes_for_test() == vec![1, 1] {
                    break;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("第二个 Hash 应在首个 Media 未完成时执行单项查询");
        assert!(!first_worker.1.is_empty());

        controller
            .base_source_read_complete(first_worker.0.clone(), first_worker.1.clone())
            .await;
        controller
            .complete_base(first_worker.0, first_worker.1, md5, image_output())
            .await;
        let second_worker = tokio::time::timeout(Duration::from_secs(1), started.recv())
            .await
            .unwrap()
            .unwrap();
        controller
            .base_source_read_complete(second_worker.0.clone(), second_worker.1.clone())
            .await;
        controller
            .complete_base(second_worker.0, second_worker.1, md5, image_output())
            .await;

        let (result, shutdown) = join.await.unwrap();
        assert!(shutdown.is_ok());
        let mut result = result.unwrap();
        assert_eq!(result.manifest.resolved_files.len(), 2);
        assert_eq!(
            statuses(&result.dispatcher, &lane(31)),
            vec![b'C'],
            "首行只可在 SQLite ACK 后完成"
        );
        assert_eq!(statuses(&result.dispatcher, &lane(32)), vec![b'C']);
        assert_eq!(handle.lookup_key_batch_sizes_for_test(), vec![1, 1]);
        result.dispatcher.discard().unwrap();
        drop(handle);
        actor.finish().await.unwrap();
    }

    /// 远端失败后，已在途的成功结果也必须退回其发起时的 SQLite 快照。
    #[tokio::test]
    async fn remote_success_in_flight_after_failure_stays_local_only() {
        let root = tempfile::tempdir().unwrap();
        let first_md5 = [0xB8; 16];
        let second_md5 = [0xB9; 16];
        let machine = MachineId::from_sha256([0xBA; 32]);
        let mut store = NodeStore::open_in_memory(machine).unwrap();
        let first_local = seed_partial(
            &mut store,
            r"C:\remote-local-first.bin",
            first_md5,
            10,
            MediaKind::Image,
        );
        let second_local = seed_partial(
            &mut store,
            r"C:\remote-local-second.bin",
            second_md5,
            10,
            MediaKind::Image,
        );
        let first_path = scanned(r"C:\remote-first.bin", 10);
        let second_path = scanned(r"C:\remote-second.bin", 10);
        let production = production(
            root.path(),
            &[
                input(first_path.clone(), lane(33), None),
                input(second_path.clone(), lane(34), None),
            ],
        );
        let remote = FailingThenSuccessfulRemote {
            failing_key: first_local.content_key,
            succeeding_key: second_local.content_key,
            succeeding_record: complete_remote_image(second_md5, 10),
            entered: Arc::new(AtomicUsize::new(0)),
            failure_gate: Arc::new(Notify::new()),
            success_gate: Arc::new(Notify::new()),
        };
        let remote_entered = Arc::clone(&remote.entered);
        let failure_gate = Arc::clone(&remote.failure_gate);
        let success_gate = Arc::clone(&remote.success_gate);
        let reader = TestHashReader {
            results: Arc::new(BTreeMap::from([
                (first_path.normalized_path.as_str().to_owned(), first_md5),
                (second_path.normalized_path.as_str().to_owned(), second_md5),
            ])),
            calls: Arc::new(AtomicUsize::new(0)),
        };
        let (actor, handle, mut acknowledgements) =
            crate::scan::base_persistence::BaseStoreActor::spawn(store, 2);
        let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(2);
        let handle_for_run = handle.clone();
        let run_root = root.path().to_path_buf();
        let mut coordinator_options = options(&run_root);
        coordinator_options.hash_capacity = 2;
        coordinator_options.worker_capacity = 2;
        let mut remote_available = true;
        let mut warning = None;
        let join = tokio::spawn(async move {
            let result = run_task_file_base_coordinator_with_remote(
                production,
                reader,
                &mut pool,
                &handle_for_run,
                &mut acknowledgements,
                coordinator_options,
                ReadCancellationToken::new(),
                Arc::new(remote),
                &mut remote_available,
                &mut warning,
            )
            .await;
            let shutdown = pool.shutdown().await;
            (result, shutdown, remote_available, warning)
        });

        tokio::time::timeout(Duration::from_secs(1), async {
            while remote_entered.load(Ordering::SeqCst) != 2 {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("两个单键远端 future 都应先进入");
        failure_gate.notify_one();
        let first_worker = tokio::time::timeout(Duration::from_secs(1), started.recv())
            .await
            .expect("远端失败必须让首项退回本地缺失掩码")
            .unwrap();
        success_gate.notify_one();
        let second_worker = tokio::time::timeout(Duration::from_secs(1), started.recv())
            .await
            .expect("故障后的在途远端成功不得导入或消除第二项缺失")
            .unwrap();

        controller
            .base_source_read_complete(first_worker.0.clone(), first_worker.1.clone())
            .await;
        controller
            .complete_base(first_worker.0, first_worker.1, first_md5, image_output())
            .await;
        controller
            .base_source_read_complete(second_worker.0.clone(), second_worker.1.clone())
            .await;
        controller
            .complete_base(second_worker.0, second_worker.1, second_md5, image_output())
            .await;
        let (result, shutdown, remote_available, warning) = join.await.unwrap();
        assert!(shutdown.is_ok());
        let mut result = result.unwrap();
        assert!(!remote_available);
        assert!(warning.is_some(), "远端失败只保留一条降级告警");
        assert_eq!(result.summary.cache_hits, 0);
        assert_eq!(result.manifest.resolved_files.len(), 2);
        assert_eq!(handle.lookup_key_batch_sizes_for_test(), vec![1, 1]);
        result.dispatcher.discard().unwrap();
        drop(handle);
        actor.finish().await.unwrap();
    }

    /// 首个 Media active 时，另一物理盘的后续 Hash 仍必须继续进入单键缓存查询。
    #[tokio::test]
    async fn active_media_does_not_block_later_hash_on_another_lane() {
        let root = tempfile::tempdir().unwrap();
        let md5 = [0xC8; 16];
        let machine = MachineId::from_sha256([0xC9; 32]);
        let store = NodeStore::open_in_memory(machine).unwrap();
        let first_path = scanned(r"C:\fair-first.bin", 10);
        let second_path = scanned(r"C:\fair-second.bin", 10);
        let third_path = scanned(r"C:\fair-third.bin", 10);
        let production = production(
            root.path(),
            &[
                input(first_path.clone(), lane(35), None),
                input(second_path.clone(), lane(36), None),
                input(third_path.clone(), lane(37), None),
            ],
        );
        let first_gate = Arc::new(Notify::new());
        let second_entered = Arc::new(AtomicUsize::new(0));
        let second_gate = Arc::new(Notify::new());
        let third_entered = Arc::new(AtomicUsize::new(0));
        let reader = FairHashReader {
            first_path: first_path.normalized_path.as_str().to_owned(),
            second_path: second_path.normalized_path.as_str().to_owned(),
            third_path: third_path.normalized_path.as_str().to_owned(),
            first_gate: Arc::clone(&first_gate),
            second_entered: Arc::clone(&second_entered),
            second_gate: Arc::clone(&second_gate),
            third_entered: Arc::clone(&third_entered),
            md5,
        };
        let (actor, handle, mut acknowledgements) =
            crate::scan::base_persistence::BaseStoreActor::spawn(store, 2);
        let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
        let handle_for_run = handle.clone();
        let run_root = root.path().to_path_buf();
        let cancellation = ReadCancellationToken::new();
        let cancellation_for_run = cancellation.clone();
        let mut coordinator_options = options(&run_root);
        coordinator_options.hash_capacity = 2;
        let mut join = Some(tokio::spawn(async move {
            let result = run_task_file_base_coordinator(
                production,
                reader,
                &mut pool,
                &handle_for_run,
                &mut acknowledgements,
                coordinator_options,
                cancellation_for_run,
            )
            .await;
            let shutdown = pool.shutdown().await;
            (result, shutdown)
        }));

        let initial_window = tokio::time::timeout(Duration::from_secs(1), async {
            while second_entered.load(Ordering::SeqCst) == 0 {
                tokio::task::yield_now().await;
            }
        })
        .await;
        if initial_window.is_err() {
            cancellation.cancel();
            join.take().expect("失败清理前 join 必须存在").abort();
        }
        initial_window.expect("初始 Hash window 应先启动第二条 lane");
        first_gate.notify_one();
        let first_worker = tokio::time::timeout(Duration::from_secs(1), started.recv()).await;
        if first_worker.is_err() {
            cancellation.cancel();
            join.take().expect("失败清理前 join 必须存在").abort();
        }
        let first_worker = first_worker.expect("首个 Hash miss 应启动 Media").unwrap();
        second_gate.notify_one();
        let second_hash = tokio::time::timeout(Duration::from_secs(1), async {
            while handle.lookup_key_batch_sizes_for_test() != vec![1, 1] {
                tokio::task::yield_now().await;
            }
        })
        .await;
        if second_hash.is_err() {
            cancellation.cancel();
            join.take().expect("失败清理前 join 必须存在").abort();
        }
        second_hash.expect("第二条 Hash 必须在首个 Media active 时完成单键查询");
        let third_hash = tokio::time::timeout(Duration::from_secs(1), async {
            while third_entered.load(Ordering::SeqCst) == 0
                || handle.lookup_key_batch_sizes_for_test() != vec![1, 1, 1]
            {
                tokio::task::yield_now().await;
            }
        })
        .await;
        if third_hash.is_err() {
            cancellation.cancel();
            join.take().expect("失败清理前 join 必须存在").abort();
        }
        third_hash.expect("跨 lane 后续 Hash 不得等待首个 Worker terminal 或 ACK");

        controller
            .base_source_read_complete(first_worker.0.clone(), first_worker.1.clone())
            .await;
        controller
            .complete_base(first_worker.0, first_worker.1, md5, image_output())
            .await;
        for _ in 0..2 {
            let worker = tokio::time::timeout(Duration::from_secs(1), started.recv())
                .await
                .unwrap()
                .unwrap();
            controller
                .base_source_read_complete(worker.0.clone(), worker.1.clone())
                .await;
            controller
                .complete_base(worker.0, worker.1, md5, image_output())
                .await;
        }
        let (result, shutdown) = join.take().expect("成功路径 join 必须存在").await.unwrap();
        assert!(shutdown.is_ok());
        let mut result = result.unwrap();
        assert_eq!(result.manifest.resolved_files.len(), 3);
        assert_eq!(handle.lookup_key_batch_sizes_for_test(), vec![1, 1, 1]);
        result.dispatcher.discard().unwrap();
        drop(handle);
        actor.finish().await.unwrap();
    }

    /// Hash 后的 Media 必须沿用同一任务身份，不能为了续算追加第二行。
    #[tokio::test]
    async fn hash_creates_one_media_continuation_with_same_identity() {
        let root = tempfile::tempdir().unwrap();
        let md5 = [0xB1; 16];
        let machine = MachineId::from_sha256([0xB2; 32]);
        let store = NodeStore::open_in_memory(machine).unwrap();
        let path = scanned(r"C:\hash-then-media.bin", 10);
        let production = production(root.path(), &[input(path.clone(), lane(8), None)]);
        let expected_identity = production.contexts.keys().next().unwrap().clone();
        let hash_key = path.normalized_path.as_str().to_owned();
        let reader = TestHashReader {
            results: Arc::new(BTreeMap::from([(hash_key, md5)])),
            calls: Arc::new(AtomicUsize::new(0)),
        };
        let (actor, handle, mut acknowledgements) =
            crate::scan::base_persistence::BaseStoreActor::spawn(store, 2);
        let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
        let handle_for_run = handle.clone();
        let run_root = root.path().to_path_buf();
        let cancellation = ReadCancellationToken::new();
        let cancellation_for_run = cancellation.clone();
        let join = tokio::spawn(async move {
            let result = run_task_file_base_coordinator(
                production,
                reader,
                &mut pool,
                &handle_for_run,
                &mut acknowledgements,
                options(&run_root),
                cancellation_for_run,
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
        assert_eq!(item_id, expected_identity.item_id().to_string());
        controller
            .base_source_read_complete(task_id.clone(), item_id.clone())
            .await;
        controller
            .complete_base(task_id, item_id, md5, image_output())
            .await;

        let (result, shutdown) = join.await.unwrap();
        assert!(shutdown.is_ok());
        let mut result = result.unwrap();
        assert_eq!(result.summary.file_failures, 0);
        assert_eq!(result.manifest.resolved_files.len(), 1);
        assert_eq!(statuses(&result.dispatcher, &lane(8)), vec![b'C']);
        assert_eq!(
            result.dispatcher.lane_path(&lane(8)).unwrap().exists(),
            true,
            "完成后仍需由上层持有 owner 决定何时删除任务文件"
        );
        result.dispatcher.discard().unwrap();
        drop(handle);
        actor.finish().await.unwrap();
        cancellation.cancel();
    }

    /// 单文件 Worker 崩溃只写 F，其他行仍须继续完成并写 C。
    #[tokio::test]
    async fn one_media_failure_does_not_stop_the_next_file() {
        let root = tempfile::tempdir().unwrap();
        let md5 = [0xC1; 16];
        let machine = MachineId::from_sha256([0xC2; 32]);
        let mut store = NodeStore::open_in_memory(machine).unwrap();
        let cached = seed_partial(
            &mut store,
            r"C:\seed-failure.bin",
            md5,
            10,
            MediaKind::Image,
        );
        let production = production(
            root.path(),
            &[
                input(
                    scanned(r"C:\failure.bin", 10),
                    lane(9),
                    Some(cached.clone()),
                ),
                input(scanned(r"C:\success.bin", 10), lane(9), Some(cached)),
            ],
        );
        let reader = TestHashReader::default();
        let (actor, handle, mut acknowledgements) =
            crate::scan::base_persistence::BaseStoreActor::spawn(store, 2);
        let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
        let handle_for_run = handle.clone();
        let run_root = root.path().to_path_buf();
        let join = tokio::spawn(async move {
            let result = run_task_file_base_coordinator(
                production,
                reader,
                &mut pool,
                &handle_for_run,
                &mut acknowledgements,
                options(&run_root),
                ReadCancellationToken::new(),
            )
            .await;
            let shutdown = pool.shutdown().await;
            (result, shutdown)
        });

        let (task_id, failed_item) = tokio::time::timeout(Duration::from_secs(1), started.recv())
            .await
            .unwrap()
            .unwrap();
        controller
            .crash(task_id.clone(), failed_item, "测试 Worker 崩溃".into())
            .await;
        let (same_task, success_item) =
            tokio::time::timeout(Duration::from_secs(1), started.recv())
                .await
                .unwrap()
                .unwrap();
        assert_eq!(same_task, RUN_ID);
        controller
            .base_source_read_complete(same_task.clone(), success_item.clone())
            .await;
        controller
            .complete_base(same_task, success_item, md5, image_output())
            .await;

        let (result, shutdown) = join.await.unwrap();
        assert!(shutdown.is_ok());
        let mut result = result.unwrap();
        assert_eq!(result.summary.file_failures, 1);
        assert_eq!(result.manifest.resolved_files.len(), 1);
        assert_eq!(statuses(&result.dispatcher, &lane(9)), vec![b'F', b'C']);
        result.dispatcher.discard().unwrap();
        drop(handle);
        actor.finish().await.unwrap();
    }

    /// 取消必须返回精确 pending owner；未收到 ACK 的行保持 P，禁止协调器擅自删除。
    #[tokio::test]
    async fn cancellation_returns_pending_owner_without_acknowledging_rows() {
        let root = tempfile::tempdir().unwrap();
        let md5 = [0xD1; 16];
        let machine = MachineId::from_sha256([0xD2; 32]);
        let mut store = NodeStore::open_in_memory(machine).unwrap();
        let cached = seed_partial(&mut store, r"C:\seed-cancel.bin", md5, 10, MediaKind::Image);
        let production = production(
            root.path(),
            &[input(scanned(r"C:\cancel.bin", 10), lane(10), Some(cached))],
        );
        let reader = TestHashReader::default();
        let (actor, handle, mut acknowledgements) =
            crate::scan::base_persistence::BaseStoreActor::spawn(store, 2);
        let (mut pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
        let handle_for_run = handle.clone();
        let run_root = root.path().to_path_buf();
        let cancellation = ReadCancellationToken::new();
        let cancellation_for_run = cancellation.clone();
        let join = tokio::spawn(async move {
            let result = run_task_file_base_coordinator(
                production,
                reader,
                &mut pool,
                &handle_for_run,
                &mut acknowledgements,
                options(&run_root),
                cancellation_for_run,
            )
            .await;
            let shutdown = pool.shutdown().await;
            (result, shutdown)
        });

        let (task_id, item_id) = tokio::time::timeout(Duration::from_secs(1), started.recv())
            .await
            .unwrap()
            .unwrap();
        cancellation.cancel();

        let (result, shutdown) = join.await.unwrap();
        assert!(shutdown.is_ok());
        let error = match result {
            Ok(_) => panic!("取消必须返回携带 pending 的错误"),
            Err(error) => error,
        };
        assert!(error.is_cancelled());
        let mut pending = error.into_pending();
        assert_eq!(pending.contexts.len(), 1);
        assert_eq!(pending.remaining_hash_rows, 0);
        assert_eq!(statuses(&pending.dispatcher, &lane(10)), vec![b'P']);
        assert_eq!(task_id, RUN_ID);
        assert!(!item_id.is_empty());
        pending.dispatcher.discard().unwrap();
        drop(controller);
        drop(handle);
        actor.finish().await.unwrap();
    }
}
