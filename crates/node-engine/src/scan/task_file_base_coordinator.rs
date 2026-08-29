//! 瞬态基础任务的 Hash、Media 与 SQLite ACK 交错协调边界。
//!
//! 本模块只负责组合已经独立验证的阶段函数；任务 actor、收尾和恢复由上层负责。

use std::fmt;

use dedup_core::DiskReadConfig;
use dedup_windows::ReadCancellationToken;
use tokio::sync::mpsc::UnboundedReceiver;

use super::{
    BaseTaskManifest, BaseTaskProduction, HashPermitReader,
    base_persistence::{BasePersistAck, BaseStoreHandle},
    task_file_base_compute::{TaskFileBaseComputePending, run_task_file_hash_pass},
    task_file_media_compute::run_task_file_media_compute,
    task_file_media_persistence::{
        TaskFileMediaPersistenceOptions, persist_task_file_media_results,
    },
};
use crate::{
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
    let TaskFileBaseCoordinatorOptions {
        hash_capacity,
        worker_capacity,
        read_config,
        persistence,
    } = options;
    let mut pending = TaskFileBaseComputePending::from_production(production);

    loop {
        if cancellation.is_cancelled() {
            return Err(coordinator_error(pending, "基础任务已取消", true));
        }

        // 每一轮先让已知 MD5 的 Media 前进；Hash 队首阻塞时由 Media pass 明确交回 Hash。
        let media = match run_task_file_media_compute(
            pending,
            worker_pool,
            store,
            &read_config,
            worker_capacity,
            cancellation.clone(),
        )
        .await
        {
            Ok(media) => media,
            Err(error) => {
                let message = error.to_string();
                let pending = error.into_pending();
                return Err(coordinator_error(
                    pending,
                    message,
                    cancellation.is_cancelled(),
                ));
            }
        };
        let media_cancelled = media.cancelled;
        let pending_after_media = match persist_task_file_media_results(
            media,
            store,
            acknowledgements,
            persistence.clone(),
        )
        .await
        {
            Ok(pending) => pending,
            Err(error) => {
                let message = error.to_string();
                let pending = error.into_pending();
                return Err(coordinator_error(
                    pending,
                    message,
                    cancellation.is_cancelled(),
                ));
            }
        };
        pending = pending_after_media;
        if media_cancelled {
            return Err(coordinator_error(pending, "基础任务已取消", true));
        }

        if pending.contexts.is_empty() && pending.remaining_hash_rows == 0 {
            return finish_coordinator(pending, cancellation).await;
        }

        if pending.remaining_hash_rows > 0 {
            pending = match run_task_file_hash_pass(
                pending,
                reader.clone(),
                hash_capacity,
                store,
                acknowledgements,
                cancellation.clone(),
            )
            .await
            {
                Ok(pending) => pending,
                Err(error) => {
                    let message = error.to_string();
                    let pending = error.into_pending();
                    return Err(coordinator_error(
                        pending,
                        message,
                        cancellation.is_cancelled(),
                    ));
                }
            };
        }
    }
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

    use dedup_core::{DiskReadConfig, DisplayPath, MachineId, MediaKind, NormalizedPath};
    use dedup_media::{ImageStage1, PdqHash};
    use dedup_media_ffmpeg::MediaProbe;
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
        worker::{BaseComputeOutput, Stage1Frame, WorkerPool},
    };

    use super::{TaskFileBaseCoordinatorOptions, run_task_file_base_coordinator};

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
