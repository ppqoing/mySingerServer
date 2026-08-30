//! 瞬态扫描的批量缓存分类、任务文件计算、清单提交和精确清理边界。

use std::{path::PathBuf, sync::Arc};

use dedup_core::{NormalizedPath, TaskId};
use dedup_node_store::{NodeStore, ResolvedScanFile, ScanFinalizeInput};
use dedup_protocol::proto;
use dedup_windows::ReadCancellationToken;

#[cfg(feature = "test-hooks")]
use super::base_persistence::BasePersistTestWaiter;
use super::{
    BaseTaskProducer, HashPermitReader, MAX_BASE_TASK_BATCH, PlannedScannedPath, ScanError,
    TaskFileBaseCoordinatorOptions, TaskFileBaseCoordinatorResult, TaskFileBaseCoordinatorSummary,
    base_persistence::BaseStoreActor, resolve_task_file_cache,
    run_task_file_base_coordinator_with_remote, run_task_file_base_coordinator_with_runtime,
    task_file_base_compute::TaskFileBaseComputePending,
};
use crate::{
    RemoteFeatureCache,
    runtime_tasks::{RuntimeProgressUnit, RuntimeStage, RuntimeStageUpdate, RuntimeTaskReporter},
    task_dispatch::{TaskFileDispatcher, TaskLanePermitProvider},
    task_files::TransientTaskFileSet,
    worker::WorkerPool,
};

/// SQLite 与中心缓存每次处理的固定最大行数。
const SCAN_BATCH_SIZE: usize = MAX_BASE_TASK_BATCH;

/// 当前进程内可供后续本地分析选择的完成扫描快照。
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct CompletedScanSnapshot {
    /// 当前进程有效的扫描任务 ID。
    pub(crate) task_id: TaskId,
    /// 本轮允许失活旧位置的规范扫描根。
    pub(crate) roots: Vec<NormalizedPath>,
    /// 缓存命中或计算成功并已经提交 SQLite 的文件。
    pub(crate) resolved_files: Vec<ResolvedScanFile>,
    /// 扫描收尾事务提交后的真实 outbox 高水位。
    pub(crate) outbox_high_seq: u64,
    /// 扫描收尾事务推进后的文件库版本。
    pub(crate) library_revision: u64,
}

/// 一次瞬态扫描成功完成后的当前进程结果。
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct ScanRunResult {
    /// 任务文件和 SQLite ACK 已确认的基础计算汇总。
    pub(crate) summary: TaskFileBaseCoordinatorSummary,
    /// 后续本地分析只在当前进程内读取的完成快照。
    pub(crate) completed: CompletedScanSnapshot,
    /// 中心缓存不可用时保留本轮第一条降级告警。
    pub(crate) warning: Option<String>,
}

/// 一次瞬态基础扫描运行所需的固定输入。
pub(crate) struct TaskFileScanRunOptions {
    /// 当前进程有效的业务任务 ID，同时作为任务文件 run ID。
    pub(crate) task_id: TaskId,
    /// 枚举前冻结并规范化的扫描根。
    pub(crate) roots: Vec<NormalizedPath>,
    /// 枚举完成后按稳定顺序保存的路径和物理盘 lane。
    pub(crate) planned: Vec<PlannedScannedPath>,
    /// 只存放当前进程瞬态任务目录的根路径。
    pub(crate) runtime_root: PathBuf,
    /// 是否忽略已有缓存并重新计算基础结果。
    pub(crate) force_recompute: bool,
    /// Hash、Worker、磁盘读取与 Media 持久化配置。
    pub(crate) coordinator: TaskFileBaseCoordinatorOptions,
    /// task-local SQLite writer 的有界持久化容量。
    pub(crate) persist_capacity: usize,
    /// 本轮数据库写入使用的毫秒时间戳。
    pub(crate) now_ms: i64,
    /// 本轮开始时中心缓存是否可用。
    pub(crate) remote_available: bool,
    /// 仅测试暂停首条 taskless SQLite 写入，不进入生产结构。
    #[cfg(feature = "test-hooks")]
    pub(crate) first_persist_waiter: Option<BasePersistTestWaiter>,
}

/// 运行完整的瞬态基础扫描；该入口拥有后台 SQLite 连接，结束前必须 join writer。
pub(crate) async fn run_task_file_scan<P, H, R>(
    store: NodeStore,
    worker_pool: &mut WorkerPool,
    provider: P,
    reader: H,
    remote: R,
    options: TaskFileScanRunOptions,
    cancellation: ReadCancellationToken,
) -> Result<ScanRunResult, ScanError>
where
    P: TaskLanePermitProvider,
    H: HashPermitReader<Permit = P::Permit>,
    R: RemoteFeatureCache,
{
    run_task_file_scan_inner(
        store,
        worker_pool,
        provider,
        reader,
        remote,
        options,
        cancellation,
        None,
    )
    .await
}

/// 运行带阶段遥测的瞬态扫描；Node actor 使用该入口维护真实阶段边界。
pub(crate) async fn run_task_file_scan_with_runtime<P, H, R>(
    store: NodeStore,
    worker_pool: &mut WorkerPool,
    provider: P,
    reader: H,
    remote: R,
    options: TaskFileScanRunOptions,
    cancellation: ReadCancellationToken,
    runtime_reporter: &RuntimeTaskReporter,
) -> Result<ScanRunResult, ScanError>
where
    P: TaskLanePermitProvider,
    H: HashPermitReader<Permit = P::Permit>,
    R: RemoteFeatureCache,
{
    run_task_file_scan_inner(
        store,
        worker_pool,
        provider,
        reader,
        remote,
        options,
        cancellation,
        Some(runtime_reporter),
    )
    .await
}

/// 共享瞬态扫描执行逻辑；阶段报告器可选以保持底层 task-file 单元测试纯粹。
async fn run_task_file_scan_inner<P, H, R>(
    store: NodeStore,
    worker_pool: &mut WorkerPool,
    provider: P,
    reader: H,
    remote: R,
    options: TaskFileScanRunOptions,
    cancellation: ReadCancellationToken,
    runtime_reporter: Option<&RuntimeTaskReporter>,
) -> Result<ScanRunResult, ScanError>
where
    P: TaskLanePermitProvider,
    H: HashPermitReader<Permit = P::Permit>,
    R: RemoteFeatureCache,
{
    let TaskFileScanRunOptions {
        task_id,
        roots,
        planned,
        runtime_root,
        force_recompute,
        coordinator,
        persist_capacity,
        now_ms,
        remote_available,
        #[cfg(feature = "test-hooks")]
        first_persist_waiter,
    } = options;
    let remote = Arc::new(remote);
    let task_files = TransientTaskFileSet::create(&runtime_root, task_id.as_uuid())?;
    let dispatcher = TaskFileDispatcher::new(task_files, provider);
    let mut producer = BaseTaskProducer::new(dispatcher);
    #[cfg(feature = "test-hooks")]
    let (store_actor, store_handle, mut acknowledgements) = match first_persist_waiter {
        Some(waiter) => {
            BaseStoreActor::spawn_with_first_persist_waiter(store, persist_capacity.max(1), waiter)
        }
        None => BaseStoreActor::spawn(store, persist_capacity.max(1)),
    };
    #[cfg(not(feature = "test-hooks"))]
    let (store_actor, store_handle, mut acknowledgements) =
        BaseStoreActor::spawn(store, persist_capacity.max(1));
    let machine_id = store_handle.machine_id().clone();
    let mut warning = remote.startup_warning().map(str::to_owned);
    let mut remote_available = remote_available && warning.is_none();

    if let Some(reporter) = runtime_reporter {
        // 遥测是运行时投影，失败不能越过后续 producer/store actor 的清理路径。
        let _ =
            reporter.start_stage_nowait(RuntimeStage::LookupBaseCache, RuntimeProgressUnit::Files);
    }

    let preparation = async {
        if cancellation.is_cancelled() {
            return Err(ScanError::Cancelled);
        }
        for batch in planned.chunks(SCAN_BATCH_SIZE) {
            if cancellation.is_cancelled() {
                return Err(ScanError::Cancelled);
            }
            let cache = resolve_task_file_cache(
                &store_handle,
                &*remote,
                remote_available,
                &machine_id,
                batch,
                &coordinator.persistence.contact_sheet_root,
                force_recompute,
            )
            .await
            .map_err(|error| ScanError::Stage1(error.to_string()))?;
            remote_available = cache.remote_available;
            if warning.is_none() {
                warning = cache.warning;
            }
            if cancellation.is_cancelled() {
                return Err(ScanError::Cancelled);
            }
            producer
                .append_batch(&cache.inputs)
                .map_err(|error| ScanError::Stage1(error.to_string()))?;
            if let Some(reporter) = runtime_reporter {
                let _ = reporter.advance_stage_nowait(
                    RuntimeStage::LookupBaseCache,
                    RuntimeProgressUnit::Files,
                    batch.len() as u64,
                );
            }
        }
        Ok(())
    }
    .await;

    if let Err(error) = preparation {
        if let Some(reporter) = runtime_reporter {
            let state = if matches!(&error, ScanError::Cancelled) {
                proto::RuntimeStageState::RuntimeStageSkipped
            } else {
                proto::RuntimeStageState::RuntimeStageFailed
            };
            let _ = reporter.finish_stage_nowait(
                RuntimeStage::LookupBaseCache,
                state,
                Some(planned.len() as u64),
            );
        }
        let cleanup = producer.discard().map_err(|cleanup| cleanup.to_string());
        drop(store_handle);
        drop(acknowledgements);
        let writer = store_actor.finish().await.map(|_| ());
        return Err(merge_run_failure(error, cleanup.err(), writer.err()));
    }

    let production = match producer.seal() {
        Ok(production) => production,
        Err(error) => {
            let primary = ScanError::Stage1(error.to_string());
            if let Some(reporter) = runtime_reporter {
                let _ = reporter.finish_stage_nowait(
                    RuntimeStage::LookupBaseCache,
                    proto::RuntimeStageState::RuntimeStageFailed,
                    Some(planned.len() as u64),
                );
            }
            let cleanup = producer.discard().map_err(|cleanup| cleanup.to_string());
            drop(store_handle);
            drop(acknowledgements);
            let writer = store_actor.finish().await.map(|_| ());
            return Err(merge_run_failure(primary, cleanup.err(), writer.err()));
        }
    };

    if let Some(reporter) = runtime_reporter {
        let _ = reporter.finish_stage_nowait(
            RuntimeStage::LookupBaseCache,
            proto::RuntimeStageState::RuntimeStageCompleted,
            Some(planned.len() as u64),
        );
        let _ = reporter.start_stage_nowait(
            RuntimeStage::ComputeBaseFeatures,
            RuntimeProgressUnit::Files,
        );
    }

    // 协调器消费 cancellation clone；原 token 保留到清单提交前，关闭末端取消竞态。
    let finalization_cancellation = cancellation.clone();
    let completed = match if let Some(reporter) = runtime_reporter {
        run_task_file_base_coordinator_with_runtime(
            production,
            reader,
            worker_pool,
            &store_handle,
            &mut acknowledgements,
            coordinator,
            cancellation,
            Arc::clone(&remote),
            &mut remote_available,
            &mut warning,
            reporter,
        )
        .await
    } else {
        run_task_file_base_coordinator_with_remote(
            production,
            reader,
            worker_pool,
            &store_handle,
            &mut acknowledgements,
            coordinator,
            cancellation,
            Arc::clone(&remote),
            &mut remote_available,
            &mut warning,
        )
        .await
    } {
        Ok(completed) => completed,
        Err(error) => {
            let cancelled = error.is_cancelled();
            let message = error.to_string();
            if let Some(reporter) = runtime_reporter {
                let state = if cancelled {
                    proto::RuntimeStageState::RuntimeStageSkipped
                } else {
                    proto::RuntimeStageState::RuntimeStageFailed
                };
                let _ = reporter.finish_stage_nowait(
                    RuntimeStage::ComputeBaseFeatures,
                    state,
                    Some(planned.len() as u64),
                );
            }
            let cleanup = discard_incomplete_scan(error.into_pending())
                .map_err(|cleanup| cleanup.to_string());
            drop(store_handle);
            drop(acknowledgements);
            let writer = store_actor.finish().await.map(|_| ());
            let primary = if cancelled {
                ScanError::Cancelled
            } else {
                ScanError::Stage1(message)
            };
            return Err(merge_run_failure(primary, cleanup.err(), writer.err()));
        }
    };

    let mut remote = Arc::try_unwrap(remote)
        .map_err(|_| ScanError::Stage1("基础流结束时仍有远端缓存查询 owner".into()))?;
    drop(store_handle);
    drop(acknowledgements);
    let writer = store_actor.finish().await;
    let (mut store, completed) =
        accept_completed_after_writer(writer, completed, &finalization_cancellation)?;
    let mut result = finalize_completed_scan(&mut store, task_id, roots, completed, now_ms)?;
    if let Some(reporter) = runtime_reporter {
        let resolved = result.completed.resolved_files.len() as u64;
        let failed = result.summary.file_failures as u64;
        let total = planned.len() as u64;
        // 清单已由 SQLite 收尾事务提交，使用真实 resolved 数量补齐 Compute 阶段进度。
        let _ = reporter.update_overall_nowait(resolved, Some(total), failed, 0);
        let _ = reporter.update_stage_nowait(RuntimeStageUpdate {
            stage: RuntimeStage::ComputeBaseFeatures,
            state: proto::RuntimeStageState::RuntimeStageRunning,
            unit: RuntimeProgressUnit::Files,
            completed: resolved,
            total: Some(total),
            failed,
            skipped: 0,
        });
        // finalize 已提交后，阶段遥测失败也不能把成功结果伪装成可重试失败。
        let _ = reporter.finish_stage_nowait(
            RuntimeStage::ComputeBaseFeatures,
            proto::RuntimeStageState::RuntimeStageCompleted,
            Some(planned.len() as u64),
        );
    }
    publish_final_outbox(&mut store, &mut remote, &mut remote_available, &mut warning).await;
    result.warning = warning;
    Ok(result)
}

/// 在 writer join 后重新确认取消状态；失败或取消都先精确删除当前 run。
fn accept_completed_after_writer<P: TaskLanePermitProvider>(
    writer: Result<NodeStore, ScanError>,
    completed: TaskFileBaseCoordinatorResult<P>,
    cancellation: &ReadCancellationToken,
) -> Result<(NodeStore, TaskFileBaseCoordinatorResult<P>), ScanError> {
    let store = match writer {
        Ok(store) => store,
        Err(error) => {
            let cleanup = discard_completed_scan(completed).map_err(|cleanup| cleanup.to_string());
            return Err(merge_run_failure(error, cleanup.err(), None));
        }
    };
    if cancellation.is_cancelled() {
        let cleanup = discard_completed_scan(completed).map_err(|cleanup| cleanup.to_string());
        return Err(merge_run_failure(ScanError::Cancelled, cleanup.err(), None));
    }
    Ok((store, completed))
}

/// 丢弃已经完成协调但尚未提交清单的 owner，不读取或写入 SQLite。
fn discard_completed_scan<P: TaskLanePermitProvider>(
    completed: TaskFileBaseCoordinatorResult<P>,
) -> Result<(), ScanError> {
    let mut dispatcher = completed.dispatcher;
    dispatcher.discard()?;
    Ok(())
}

/// 把原始阶段错误、精确清理错误和 writer join 错误合并为一个任务级结果。
fn merge_run_failure(
    primary: ScanError,
    cleanup: Option<String>,
    writer: Option<ScanError>,
) -> ScanError {
    if cleanup.is_none() && writer.is_none() {
        return primary;
    }
    let mut message = primary.to_string();
    if let Some(cleanup) = cleanup {
        message.push_str("；任务目录清理失败: ");
        message.push_str(&cleanup);
    }
    if let Some(writer) = writer {
        message.push_str("；SQLite writer 收束失败: ");
        message.push_str(&writer.to_string());
    }
    ScanError::Stage1(message)
}

/// 扫描清单提交后发布包含最终 file 变化的 outbox；失败只降级，不回滚本地成功。
async fn publish_final_outbox<R: RemoteFeatureCache>(
    store: &mut NodeStore,
    remote: &mut R,
    remote_available: &mut bool,
    warning: &mut Option<String>,
) {
    if !*remote_available {
        return;
    }
    let machine_id = store.machine_id().clone();
    let mut after = match store.sync_state() {
        Ok(state) => state.acked_seq,
        Err(error) => {
            record_warning(warning, format!("读取 SQLite 同步游标失败: {error}"));
            return;
        }
    };
    loop {
        let batch = match store.pull_changes(after, SCAN_BATCH_SIZE) {
            Ok(batch) => batch,
            Err(error) => {
                record_warning(warning, format!("读取 SQLite outbox 失败: {error}"));
                return;
            }
        };
        if batch.changes.is_empty() {
            return;
        }
        let protocol_batch = proto::SyncChangeBatch {
            changes: batch.changes,
            high_seq: batch.high_seq,
            pruned_through_seq: batch.pruned_through_seq,
        };
        match remote.publish_outbox(&machine_id, &protocol_batch).await {
            Ok(committed) => {
                if let Err(error) = store.ack_changes(committed) {
                    record_warning(warning, format!("保存 PostgreSQL ACK 失败: {error}"));
                    return;
                }
                after = committed;
            }
            Err(error) => {
                *remote_available = false;
                record_warning(
                    warning,
                    format!("发布 PostgreSQL outbox 失败，保留 SQLite 待重试: {error}"),
                );
                return;
            }
        }
    }
}

/// 只记录本轮第一条降级告警，避免同一故障反复覆盖根因。
fn record_warning(warning: &mut Option<String>, message: String) {
    if warning.is_none() {
        *warning = Some(message);
    }
}

/// 在唯一 SQLite writer 已经 join 后提交清单，并删除当前精确任务目录。
pub(crate) fn finalize_completed_scan<P: TaskLanePermitProvider>(
    store: &mut NodeStore,
    task_id: TaskId,
    roots: Vec<NormalizedPath>,
    completed: TaskFileBaseCoordinatorResult<P>,
    now_ms: i64,
) -> Result<ScanRunResult, ScanError> {
    let TaskFileBaseCoordinatorResult {
        manifest,
        summary,
        mut dispatcher,
    } = completed;
    let resolved_files = manifest.resolved_files;
    let finalized = match store.finalize_scan_manifest(
        &ScanFinalizeInput {
            roots: roots.clone(),
            seen_paths: manifest.seen_paths,
            resolved_files: resolved_files.clone(),
        },
        now_ms,
    ) {
        Ok(finalized) => finalized,
        Err(error) => {
            let primary = ScanError::Store(error);
            return match dispatcher.discard() {
                Ok(()) => Err(primary),
                Err(cleanup) => Err(ScanError::Stage1(format!(
                    "{primary}；任务目录清理失败: {cleanup}"
                ))),
            };
        }
    };
    let completed = CompletedScanSnapshot {
        task_id,
        roots,
        resolved_files,
        outbox_high_seq: finalized.outbox_high_seq,
        library_revision: finalized.library_revision,
    };

    // 清单事务已经提交后才删除当前 run；删除失败必须向上返回任务级错误，
    // 不能发布 completed，也不能宽泛清理 runtime 根目录。
    dispatcher.discard()?;
    Ok(ScanRunResult {
        summary,
        completed,
        warning: None,
    })
}

/// 取消或任务级失败时删除精确 run，不提交扫描清单，也不伪造剩余 `P` 行终态。
pub(crate) fn discard_incomplete_scan<P: TaskLanePermitProvider>(
    pending: TaskFileBaseComputePending<P>,
) -> Result<(), ScanError> {
    let mut dispatcher = pending.dispatcher;
    dispatcher.discard()?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use std::path::Path;

    use std::{future::Future, pin::Pin};

    use dedup_core::{
        ContentKey, DiskReadConfig, DisplayPath, LocationKey, MachineId, MediaKind, NormalizedPath,
        TaskId,
    };
    use dedup_node_store::{NodeStore, ResolvedScanFile, ScannedPath};
    use dedup_windows::{LocalDiskKind, PhysicalDiskId, ReadCancellationToken};

    use super::{
        TaskFileScanRunOptions, accept_completed_after_writer, discard_incomplete_scan,
        finalize_completed_scan, run_task_file_scan,
    };
    use crate::{
        DisabledRemoteFeatureCache,
        scan::{
            BaseTaskInput, BaseTaskManifest, BaseTaskProducer, HashPermitReader,
            PlannedScannedPath, ReadProduct, TaskDiskLane, TaskFileBaseCoordinatorOptions,
            TaskFileBaseCoordinatorResult, TaskFileBaseCoordinatorSummary,
            TaskFileMediaPersistenceOptions, task_file_base_compute::TaskFileBaseComputePending,
        },
        task_dispatch::{TaskFileDispatcher, TaskLanePermitFuture, TaskLanePermitProvider},
        task_files::TransientTaskFileSet,
        worker::WorkerPool,
    };

    /// 空任务文件运行仍通过真实 owner 验证精确目录删除。
    #[derive(Clone, Copy)]
    struct EmptyPermitProvider;

    impl TaskLanePermitProvider for EmptyPermitProvider {
        type Permit = ();

        fn acquire(
            &self,
            _lane: crate::scan::TaskDiskLane,
            _class: crate::io::DiskReadClass,
            _cancellation: ReadCancellationToken,
        ) -> TaskLanePermitFuture<Self::Permit> {
            Box::pin(async { Ok(()) })
        }
    }

    /// 全缓存命中测试不应触发 Hash；若误触发则立即暴露实现错误。
    #[derive(Clone, Copy)]
    struct NeverHashReader;

    impl HashPermitReader for NeverHashReader {
        type Permit = ();

        fn read_with_permit(
            &self,
            _scanned: ScannedPath,
            _permit: Self::Permit,
            _cancellation: ReadCancellationToken,
            _started: Option<crate::scan::HashReadStartedSignal>,
        ) -> Pin<
            Box<
                dyn Future<Output = Result<ReadProduct<Self::Permit>, crate::io::ReadFailure>>
                    + Send
                    + 'static,
            >,
        > {
            Box::pin(async { panic!("完整缓存命中不得启动 Hash") })
        }
    }

    /// 构造已经写入完整基础缓存的真实扫描文件。
    fn seed_complete_file(store: &mut NodeStore, path: &str, size: u64) -> ResolvedScanFile {
        let scanned = ScannedPath::new(
            NormalizedPath::new(path).unwrap(),
            DisplayPath::new(path).unwrap(),
            size,
        );
        let content = store
            .upsert_content_and_location(&scanned, [0x71; 16], MediaKind::Other)
            .unwrap();
        store.mark_base_complete(content.id).unwrap();
        ResolvedScanFile {
            scanned,
            content: ContentKey::new([0x71; 16], size),
        }
    }

    /// 完整缓存命中也必须提交 revision、返回快照，并在提交后删除精确 run 目录。
    #[test]
    fn completed_manifest_is_finalized_before_exact_run_discard() {
        let machine = MachineId::from_sha256([0x72; 32]);
        let mut store = NodeStore::open_in_memory(machine).unwrap();
        let resolved = seed_complete_file(&mut store, r"C:\media\hit.bin", 42);
        let runtime = tempfile::tempdir().unwrap();
        let task_id = TaskId::new();
        let run_dir = runtime.path().join(task_id.as_uuid().to_string());
        let files = TransientTaskFileSet::create(runtime.path(), task_id.as_uuid()).unwrap();
        assert!(run_dir.is_dir());
        let completed = TaskFileBaseCoordinatorResult {
            manifest: BaseTaskManifest {
                seen_paths: vec![resolved.scanned.normalized_path.clone()],
                resolved_files: vec![resolved.clone()],
                cache_hits: 1,
            },
            summary: TaskFileBaseCoordinatorSummary {
                file_failures: 0,
                cache_hits: 1,
            },
            dispatcher: TaskFileDispatcher::new(files, EmptyPermitProvider),
        };

        let result = finalize_completed_scan(
            &mut store,
            task_id,
            vec![NormalizedPath::new(r"C:\media").unwrap()],
            completed,
            100,
        )
        .unwrap();

        assert_eq!(store.library_revision().unwrap(), 1);
        assert_eq!(result.completed.task_id, task_id);
        assert_eq!(result.completed.resolved_files, vec![resolved]);
        assert_eq!(result.completed.library_revision, 1);
        assert_eq!(result.summary.cache_hits, 1);
        assert!(!Path::new(&run_dir).exists());
    }

    /// 收尾事务拒绝无效清单时也必须清掉当前精确 run，且 revision 保持不变。
    #[test]
    fn rejected_manifest_is_rolled_back_and_exact_run_is_discarded() {
        let mut store = NodeStore::open_in_memory(MachineId::from_sha256([0x75; 32])).unwrap();
        let runtime = tempfile::tempdir().unwrap();
        let task_id = TaskId::new();
        let run_dir = runtime.path().join(task_id.as_uuid().to_string());
        let files = TransientTaskFileSet::create(runtime.path(), task_id.as_uuid()).unwrap();
        let completed = TaskFileBaseCoordinatorResult {
            manifest: BaseTaskManifest {
                seen_paths: vec![NormalizedPath::new(r"D:\outside\file.bin").unwrap()],
                resolved_files: Vec::new(),
                cache_hits: 0,
            },
            summary: TaskFileBaseCoordinatorSummary {
                file_failures: 1,
                cache_hits: 0,
            },
            dispatcher: TaskFileDispatcher::new(files, EmptyPermitProvider),
        };

        let error = finalize_completed_scan(
            &mut store,
            task_id,
            vec![NormalizedPath::new(r"C:\media").unwrap()],
            completed,
            100,
        )
        .expect_err("根外 seen 路径必须使收尾事务失败");

        assert!(!error.to_string().is_empty());
        assert_eq!(store.library_revision().unwrap(), 0);
        assert!(!run_dir.exists());
    }

    /// 取消只清理当前 run，未计算的 P 行不会导致 revision 或清单事务推进。
    #[test]
    fn cancelled_pending_run_is_discarded_without_manifest_finalize() {
        let store = NodeStore::open_in_memory(MachineId::from_sha256([0x73; 32])).unwrap();
        let runtime = tempfile::tempdir().unwrap();
        let task_id = TaskId::new();
        let run_dir = runtime.path().join(task_id.as_uuid().to_string());
        let files = TransientTaskFileSet::create(runtime.path(), task_id.as_uuid()).unwrap();
        let dispatcher = TaskFileDispatcher::new(files, EmptyPermitProvider);
        let mut producer = BaseTaskProducer::new(dispatcher);
        let path = r"C:\media\pending.bin";
        producer
            .append_batch(&[BaseTaskInput {
                planned: PlannedScannedPath {
                    scanned: ScannedPath::new(
                        NormalizedPath::new(path).unwrap(),
                        DisplayPath::new(path).unwrap(),
                        43,
                    ),
                    lane: TaskDiskLane {
                        physical_disk_id: PhysicalDiskId::from_disk_numbers([7]).unwrap(),
                        physical_disk_numbers: vec![7],
                        disk_kind: LocalDiskKind::Hdd,
                        configured_weight: 1,
                        per_disk_limit: 1,
                    },
                },
                cached: None,
                contact_sheet_valid: false,
                force_recompute: false,
            }])
            .unwrap();
        let pending = TaskFileBaseComputePending::from_production(producer.seal().unwrap());

        discard_incomplete_scan(pending).unwrap();

        assert_eq!(store.library_revision().unwrap(), 0);
        assert!(!Path::new(&run_dir).exists());
    }

    /// 完整 runner 必须用批量缓存命中直接收尾，不能创建任务行或启动 Worker。
    #[tokio::test]
    async fn full_cache_hit_runner_finishes_without_hash_or_worker() {
        let directory = tempfile::tempdir().unwrap();
        let database = directory.path().join("node.db");
        let runtime = directory.path().join("runtime");
        let contacts = directory.path().join("contact-sheets");
        std::fs::create_dir_all(&contacts).unwrap();
        let machine = MachineId::from_sha256([0x74; 32]);
        let mut store = NodeStore::open(&database, machine.clone()).unwrap();
        let resolved = seed_complete_file(&mut store, r"C:\media\cached.bin", 44);
        let task_id = TaskId::new();
        let lane = TaskDiskLane {
            physical_disk_id: PhysicalDiskId::from_disk_numbers([7]).unwrap(),
            physical_disk_numbers: vec![7],
            disk_kind: LocalDiskKind::Hdd,
            configured_weight: 1,
            per_disk_limit: 1,
        };
        let (mut worker_pool, mut started, _controller) = WorkerPool::controlled_batch_for_test(1);
        let remote = DisabledRemoteFeatureCache;

        let result = run_task_file_scan(
            store,
            &mut worker_pool,
            EmptyPermitProvider,
            NeverHashReader,
            remote,
            TaskFileScanRunOptions {
                task_id,
                roots: vec![NormalizedPath::new(r"C:\media").unwrap()],
                planned: vec![PlannedScannedPath {
                    scanned: resolved.scanned.clone(),
                    lane,
                }],
                runtime_root: runtime.clone(),
                force_recompute: false,
                coordinator: TaskFileBaseCoordinatorOptions {
                    hash_capacity: 1,
                    worker_capacity: 1,
                    read_config: DiskReadConfig::default(),
                    persistence: TaskFileMediaPersistenceOptions {
                        contact_sheet_root: contacts,
                        artifact_registry: None,
                        disk_full_cleaner: None,
                    },
                },
                persist_capacity: 1,
                now_ms: 100,
                remote_available: false,
                #[cfg(feature = "test-hooks")]
                first_persist_waiter: None,
            },
            ReadCancellationToken::new(),
        )
        .await
        .unwrap();

        assert_eq!(result.completed.task_id, task_id);
        assert_eq!(result.completed.resolved_files, vec![resolved]);
        assert_eq!(result.completed.library_revision, 1);
        assert_eq!(result.summary.cache_hits, 1);
        assert!(started.try_recv().is_err());
        assert!(!runtime.join(task_id.as_uuid().to_string()).exists());
        assert_eq!(
            NodeStore::open(&database, machine)
                .unwrap()
                .library_revision()
                .unwrap(),
            1
        );
    }

    /// runner 在进入缓存查询前已被取消时必须删除精确 run，不能推进文件库版本。
    #[tokio::test]
    async fn pre_cancelled_runner_discards_exact_run_without_finalize() {
        let directory = tempfile::tempdir().unwrap();
        let database = directory.path().join("node.db");
        let runtime = directory.path().join("runtime");
        let contacts = directory.path().join("contact-sheets");
        std::fs::create_dir_all(&contacts).unwrap();
        let machine = MachineId::from_sha256([0x76; 32]);
        let store = NodeStore::open(&database, machine.clone()).unwrap();
        let task_id = TaskId::new();
        let cancellation = ReadCancellationToken::new();
        cancellation.cancel();
        let (mut worker_pool, mut started, _controller) = WorkerPool::controlled_batch_for_test(1);
        let remote = DisabledRemoteFeatureCache;

        let error = run_task_file_scan(
            store,
            &mut worker_pool,
            EmptyPermitProvider,
            NeverHashReader,
            remote,
            TaskFileScanRunOptions {
                task_id,
                roots: vec![NormalizedPath::new(r"C:\media").unwrap()],
                planned: Vec::new(),
                runtime_root: runtime.clone(),
                force_recompute: false,
                coordinator: TaskFileBaseCoordinatorOptions {
                    hash_capacity: 1,
                    worker_capacity: 1,
                    read_config: DiskReadConfig::default(),
                    persistence: TaskFileMediaPersistenceOptions {
                        contact_sheet_root: contacts,
                        artifact_registry: None,
                        disk_full_cleaner: None,
                    },
                },
                persist_capacity: 1,
                now_ms: 100,
                remote_available: false,
                #[cfg(feature = "test-hooks")]
                first_persist_waiter: None,
            },
            cancellation,
        )
        .await
        .expect_err("预取消必须返回取消结果");

        assert!(matches!(error, crate::scan::ScanError::Cancelled));
        assert!(started.try_recv().is_err());
        assert!(!runtime.join(task_id.as_uuid().to_string()).exists());
        assert_eq!(
            NodeStore::open(&database, machine)
                .unwrap()
                .library_revision()
                .unwrap(),
            0
        );
    }

    /// 本轮见到但计算失败的旧活动文件仍保持活动；成功邻项进入当前进程快照。
    #[test]
    fn seen_file_failure_stays_active_while_successful_neighbor_finalizes() {
        let machine = MachineId::from_sha256([0x77; 32]);
        let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
        let broken = seed_complete_file(&mut store, r"C:\media\broken.bin", 45);
        let resolved = seed_complete_file(&mut store, r"C:\media\ready.bin", 46);
        let runtime = tempfile::tempdir().unwrap();
        let task_id = TaskId::new();
        let run_dir = runtime.path().join(task_id.as_uuid().to_string());
        let files = TransientTaskFileSet::create(runtime.path(), task_id.as_uuid()).unwrap();
        let completed = TaskFileBaseCoordinatorResult {
            manifest: BaseTaskManifest {
                seen_paths: vec![
                    broken.scanned.normalized_path.clone(),
                    resolved.scanned.normalized_path.clone(),
                ],
                resolved_files: vec![resolved.clone()],
                cache_hits: 1,
            },
            summary: TaskFileBaseCoordinatorSummary {
                file_failures: 1,
                cache_hits: 1,
            },
            dispatcher: TaskFileDispatcher::new(files, EmptyPermitProvider),
        };

        let result = finalize_completed_scan(
            &mut store,
            task_id,
            vec![NormalizedPath::new(r"C:\media").unwrap()],
            completed,
            100,
        )
        .unwrap();

        assert!(
            store
                .active_file(&LocationKey::new(
                    machine,
                    broken.scanned.normalized_path.clone(),
                ))
                .unwrap()
                .is_some()
        );
        assert_eq!(result.summary.file_failures, 1);
        assert_eq!(result.completed.resolved_files, vec![resolved]);
        assert_eq!(result.completed.library_revision, 1);
        assert!(!run_dir.exists());
    }

    /// 协调器完成后、清单提交前收到取消时仍必须删除 run，且不得推进 revision。
    #[test]
    fn cancellation_after_coordination_discards_before_manifest_finalize() {
        let directory = tempfile::tempdir().unwrap();
        let database = directory.path().join("node.db");
        let machine = MachineId::from_sha256([0x78; 32]);
        let store = NodeStore::open(&database, machine.clone()).unwrap();
        let task_id = TaskId::new();
        let run_dir = directory.path().join(task_id.as_uuid().to_string());
        let files = TransientTaskFileSet::create(directory.path(), task_id.as_uuid()).unwrap();
        let completed = TaskFileBaseCoordinatorResult {
            manifest: BaseTaskManifest {
                seen_paths: Vec::new(),
                resolved_files: Vec::new(),
                cache_hits: 0,
            },
            summary: TaskFileBaseCoordinatorSummary {
                file_failures: 0,
                cache_hits: 0,
            },
            dispatcher: TaskFileDispatcher::new(files, EmptyPermitProvider),
        };
        let cancellation = ReadCancellationToken::new();
        cancellation.cancel();

        let error = match accept_completed_after_writer(Ok(store), completed, &cancellation) {
            Err(error) => error,
            Ok(_) => panic!("清单提交前的取消必须阻止收尾"),
        };

        assert!(matches!(error, crate::scan::ScanError::Cancelled));
        assert!(!run_dir.exists());
        assert_eq!(
            NodeStore::open(&database, machine)
                .unwrap()
                .library_revision()
                .unwrap(),
            0
        );
    }

    /// SQLite writer join 失败时不能因提前返回而遗留当前精确 run。
    #[test]
    fn writer_join_failure_discards_exact_run_before_returning_error() {
        let directory = tempfile::tempdir().unwrap();
        let task_id = TaskId::new();
        let run_dir = directory.path().join(task_id.as_uuid().to_string());
        let files = TransientTaskFileSet::create(directory.path(), task_id.as_uuid()).unwrap();
        let completed = TaskFileBaseCoordinatorResult {
            manifest: BaseTaskManifest {
                seen_paths: Vec::new(),
                resolved_files: Vec::new(),
                cache_hits: 0,
            },
            summary: TaskFileBaseCoordinatorSummary {
                file_failures: 0,
                cache_hits: 0,
            },
            dispatcher: TaskFileDispatcher::new(files, EmptyPermitProvider),
        };

        let error = match accept_completed_after_writer(
            Err(crate::scan::ScanError::Stage1("writer join failed".into())),
            completed,
            &ReadCancellationToken::new(),
        ) {
            Err(error) => error,
            Ok(_) => panic!("writer join 失败必须返回任务级错误"),
        };

        assert!(error.to_string().contains("writer join failed"));
        assert!(!run_dir.exists());
    }
}
