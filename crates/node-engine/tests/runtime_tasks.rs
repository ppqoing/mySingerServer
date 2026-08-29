//! Node 进程内运行任务 registry 的单调时钟、并行阶段和终态契约。

use std::{
    io::{self, Write},
    sync::{
        Arc, Mutex,
        atomic::{AtomicU64, Ordering},
    },
    time::Duration,
};

use dedup_core::{MachineId, MediaKind};
use dedup_node_engine::runtime_tasks::{
    RuntimeDiskReadClass, RuntimeExecutionConfigUpdate, RuntimeFailureUpdate,
    RuntimePipelineControl, RuntimePipelineOwnership, RuntimePipelineQueue,
    RuntimePipelineResource, RuntimeProgressPublisher, RuntimeProgressUnit, RuntimeStage,
    RuntimeStageUpdate, RuntimeTaskClock, RuntimeTaskError, RuntimeTaskKind, RuntimeTaskRegistry,
    RuntimeTaskState, RuntimeWorkerUpdate,
};
use dedup_protocol::{MAX_RUNTIME_FAILURES, proto};
use tracing_subscriber::fmt::MakeWriter;

/// 保存单个运行任务测试的 tracing 输出。
#[derive(Clone, Default)]
struct SharedLogBuffer(Arc<Mutex<Vec<u8>>>);

impl SharedLogBuffer {
    /// 返回当前捕获的 UTF-8 日志文本。
    fn text(&self) -> String {
        String::from_utf8(self.0.lock().unwrap().clone()).unwrap()
    }
}

/// 把 tracing 输出追加到共享测试缓冲区。
struct SharedLogWriter(SharedLogBuffer);

impl Write for SharedLogWriter {
    fn write(&mut self, buffer: &[u8]) -> io::Result<usize> {
        self.0.0.lock().unwrap().extend_from_slice(buffer);
        Ok(buffer.len())
    }

    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

impl<'a> MakeWriter<'a> for SharedLogBuffer {
    type Writer = SharedLogWriter;

    fn make_writer(&'a self) -> Self::Writer {
        SharedLogWriter(self.clone())
    }
}

#[derive(Default)]
struct ManualClock(AtomicU64);

impl ManualClock {
    fn advance(&self, duration: Duration) {
        self.0
            .fetch_add(duration.as_millis().try_into().unwrap(), Ordering::SeqCst);
    }
}

impl RuntimeTaskClock for ManualClock {
    fn now(&self) -> Duration {
        Duration::from_millis(self.0.load(Ordering::SeqCst))
    }
}

#[tokio::test]
async fn compute_task_kinds_and_stage_ids_match_the_three_task_model() {
    let registry = RuntimeTaskRegistry::new();
    let base = registry
        .begin(
            RuntimeTaskKind::BaseCompute,
            MachineId::from_sha256([0x81; 32]),
            "基础计算",
        )
        .await;
    let stage2 = registry
        .begin(
            RuntimeTaskKind::Stage2Compute,
            MachineId::from_sha256([0x82; 32]),
            "二次特征计算",
        )
        .await;
    assert_eq!(
        registry
            .details(base.id())
            .await
            .unwrap()
            .summary
            .unwrap()
            .task_kind,
        "base_compute"
    );
    assert_eq!(
        registry
            .details(stage2.id())
            .await
            .unwrap()
            .summary
            .unwrap()
            .task_kind,
        "stage2_compute"
    );
    assert_eq!(RuntimeStage::EnumerateFiles.id(), "enumerate_files");
    assert_eq!(RuntimeStage::LookupBaseCache.id(), "lookup_base_cache");
    assert_eq!(
        RuntimeStage::ComputeBaseFeatures.id(),
        "compute_base_features"
    );
    assert_eq!(RuntimeStage::LookupStage2Cache.id(), "lookup_stage2_cache");
    assert_eq!(
        RuntimeStage::ComputeStage2Features.id(),
        "compute_stage2_features"
    );
}

#[tokio::test]
async fn freezing_the_same_base_compute_totals_twice_is_idempotent() {
    let registry = RuntimeTaskRegistry::new();
    let task = registry
        .begin(
            RuntimeTaskKind::BaseCompute,
            MachineId::from_sha256([0x84; 32]),
            "基础计算",
        )
        .await;

    task.freeze_base_compute_totals_nowait(14_786).unwrap();
    task.freeze_base_compute_totals_nowait(14_786).unwrap();

    let details = registry.details(task.id()).await.unwrap();
    let enumerate = details
        .stages
        .iter()
        .find(|stage| stage.stage_id == RuntimeStage::EnumerateFiles.id())
        .unwrap();
    assert_eq!(
        enumerate.state,
        proto::RuntimeStageState::RuntimeStageCompleted as i32
    );
    assert_eq!(enumerate.completed, 14_786);
    assert_eq!(enumerate.total, 14_786);
}

#[tokio::test]
async fn running_progress_is_coalesced_until_two_second_tick_and_terminal_is_immediate() {
    let clock = Arc::new(ManualClock::default());
    let registry = RuntimeTaskRegistry::with_clock(clock.clone());
    let publisher = RuntimeProgressPublisher::new(registry.clone());
    let mut events = registry.subscribe();
    let task = registry
        .begin(
            RuntimeTaskKind::BaseCompute,
            MachineId::from_sha256([0x83; 32]),
            "基础计算",
        )
        .await;
    task.configure_pipeline_nowait(RuntimeExecutionConfigUpdate {
        hash_tasks: 3,
        path_cache_queue_capacity: 1,
        content_cache_queue_capacity: 1,
        decode_queue_capacity: 1,
        persist_queue_capacity: 1,
        worker_slots: 1,
        cpu_budget: 1,
        global_disk_permits: 1,
        hdd_per_disk_permits: 1,
        ssd_per_disk_permits: 1,
        unknown_per_disk_permits: 1,
    })
    .unwrap();

    task.update_overall_nowait(0, Some(100), 0, 0).unwrap();
    assert_eq!(events.recv().await.unwrap().state, "running");
    for completed in 1..=100 {
        task.update_overall_nowait(completed, Some(100), 0, 0)
            .unwrap();
    }
    task.update_queue_nowait(RuntimePipelineQueue::Hash, 3)
        .unwrap();
    assert!(events.try_recv().is_err());
    clock.advance(Duration::from_millis(1_900));
    publisher.tick();
    assert!(events.try_recv().is_err());
    clock.advance(Duration::from_millis(100));
    publisher.tick();
    assert_eq!(events.recv().await.unwrap().state, "running");

    task.start_stage_nowait(
        RuntimeStage::ComputeBaseFeatures,
        RuntimeProgressUnit::Files,
    )
    .unwrap();
    task.finish_stage_nowait(
        RuntimeStage::ComputeBaseFeatures,
        proto::RuntimeStageState::RuntimeStageCompleted,
        Some(100),
    )
    .unwrap();
    assert_eq!(events.recv().await.unwrap().state, "running");
}

#[tokio::test]
async fn terminal_transition_writes_one_structured_log() {
    let output = SharedLogBuffer::default();
    let subscriber = tracing_subscriber::fmt()
        .without_time()
        .with_ansi(false)
        .with_target(false)
        .with_writer(output.clone())
        .finish();
    let _guard = tracing::subscriber::set_default(subscriber);
    let registry = RuntimeTaskRegistry::new();
    let task = registry
        .begin(
            RuntimeTaskKind::BaseCompute,
            MachineId::from_sha256([0x84; 32]),
            "终态日志",
        )
        .await;
    task.update_overall_nowait(7, Some(9), 1, 1).unwrap();

    task.finish(RuntimeTaskState::Completed).await.unwrap();
    assert!(
        task.finish(RuntimeTaskState::Completed).await.is_err(),
        "重复终态必须在日志前被拒绝"
    );
    drop(_guard);

    let log = output.text();
    assert_eq!(log.matches("运行任务进入终态").count(), 1);
    assert!(log.contains("state=\"completed\""), "实际日志：{log}");
    assert!(log.contains("overall_completed=7"));
    assert!(log.contains("overall_failed=1"));
    assert!(log.contains("overall_skipped=1"));
    assert!(log.contains("has_pipeline_metrics=false"));
}

#[tokio::test]
async fn activity_counts_use_only_current_non_terminal_runtime_tasks() {
    let registry = RuntimeTaskRegistry::new();
    let first = registry
        .begin(
            RuntimeTaskKind::BaseCompute,
            MachineId::from_sha256([0x91; 32]),
            "活动任务一",
        )
        .await;
    let second = registry
        .begin(
            RuntimeTaskKind::LocalAnalysis,
            MachineId::from_sha256([0x92; 32]),
            "活动任务二",
        )
        .await;

    // 瞬态 registry 不建 queued 状态；当前进程中的两个非终态任务各计一个 running。
    assert_eq!(registry.activity_counts(), (0, 2));

    first.finish(RuntimeTaskState::Completed).await.unwrap();
    second.finish(RuntimeTaskState::Cancelled).await.unwrap();
    // Completed/Cancelled 都是终态，不应继续出现在 NodeStatus 活动计数中。
    assert_eq!(registry.activity_counts(), (0, 0));
}

#[tokio::test]
async fn terminal_outbox_highwater_is_consistent_and_not_restored() {
    let registry = RuntimeTaskRegistry::new();
    let task = registry
        .begin(
            RuntimeTaskKind::BaseCompute,
            MachineId::from_sha256([0x85; 32]),
            "终态高水位",
        )
        .await;
    let mut events = registry.subscribe();

    let running = registry.details(task.id()).await.unwrap();
    assert_eq!(running.summary.unwrap().outbox_high_seq, None);

    task.finish_with_outbox_high_seq(RuntimeTaskState::Completed, 42)
        .await
        .unwrap();

    let listed = registry.list().await;
    assert_eq!(listed.len(), 1);
    assert_eq!(listed[0].outbox_high_seq, Some(42));
    let detailed = registry.details(task.id()).await.unwrap();
    assert_eq!(detailed.summary.unwrap().outbox_high_seq, Some(42));
    let event = events.recv().await.unwrap();
    assert_eq!(event.runtime_task_id, task.id());
    assert_eq!(event.state, "completed");
    assert_eq!(event.outbox_high_seq, Some(42));

    let restarted = RuntimeTaskRegistry::new();
    assert!(restarted.details(task.id()).await.is_none());
}

#[tokio::test]
async fn pipeline_metrics_use_fixed_histogram_buckets_and_exact_peaks() {
    let registry = RuntimeTaskRegistry::new();
    let task = registry
        .begin(
            RuntimeTaskKind::BaseCompute,
            MachineId::from_sha256([0xb1; 32]),
            "流水线遥测",
        )
        .await;
    task.configure_pipeline_nowait(RuntimeExecutionConfigUpdate {
        hash_tasks: 4,
        path_cache_queue_capacity: 2,
        content_cache_queue_capacity: 64,
        decode_queue_capacity: 64,
        persist_queue_capacity: 8,
        worker_slots: 4,
        cpu_budget: 12,
        global_disk_permits: 4,
        hdd_per_disk_permits: 1,
        ssd_per_disk_permits: 2,
        unknown_per_disk_permits: 1,
    })
    .unwrap();

    task.update_queue_nowait(RuntimePipelineQueue::Hash, 2)
        .unwrap();
    task.update_queue_nowait(RuntimePipelineQueue::Hash, 4)
        .unwrap();
    task.update_queue_nowait(RuntimePipelineQueue::Hash, 1)
        .unwrap();
    assert!(
        task.update_queue_nowait(RuntimePipelineQueue::Hash, 5)
            .is_err(),
        "队列 current 超过同一 ownership 容量时必须拒绝更新"
    );
    for millis in [
        1_u64, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1_000, 2_000, 5_000, 10_000, 30_000, 30_001,
    ] {
        task.record_queue_wait_nowait(RuntimePipelineQueue::Hash, Duration::from_millis(millis))
            .unwrap();
    }
    task.update_resource_nowait(RuntimePipelineResource::HashIo, 2)
        .unwrap();
    task.update_resource_nowait(RuntimePipelineResource::HashIo, 4)
        .unwrap();
    task.update_resource_nowait(RuntimePipelineResource::HashIo, 0)
        .unwrap();
    task.record_hash_bytes_nowait(8_192).unwrap();
    task.record_media_throughput_nowait(MediaKind::Video, 256 * 1024 * 1024)
        .unwrap();

    let details = registry.details(task.id()).await.unwrap();
    let metrics = details.pipeline_metrics.unwrap();
    let hash = metrics.hash_queue.unwrap();
    assert_eq!(
        (hash.current, hash.peak, hash.capacity),
        (Some(1), Some(4), Some(4))
    );
    let histogram = hash.wait_latency.unwrap();
    assert_eq!(
        histogram
            .buckets
            .iter()
            .map(|bucket| bucket.upper_bound_ms)
            .collect::<Vec<_>>(),
        vec![
            Some(1),
            Some(2),
            Some(4),
            Some(8),
            Some(16),
            Some(32),
            Some(64),
            Some(128),
            Some(256),
            Some(512),
            Some(1_000),
            Some(2_000),
            Some(5_000),
            Some(10_000),
            Some(30_000),
            None,
        ]
    );
    assert!(histogram.buckets.iter().all(|bucket| bucket.count == 1));
    assert_eq!(histogram.count, 16);
    assert_eq!(histogram.p50_ms, Some(128));
    assert_eq!(histogram.p95_ms, Some(30_001));
    assert_eq!(histogram.p99_ms, Some(30_001));
    assert_eq!(histogram.max_ms, Some(30_001));
    let hash_io = metrics.hash_io.unwrap();
    assert_eq!(
        (hash_io.current, hash_io.peak, hash_io.capacity),
        (Some(0), Some(4), Some(4))
    );
    assert_eq!(metrics.hash_bytes, Some(8_192));
    assert_eq!(metrics.media_throughput.len(), 1);
    assert_eq!(
        metrics.media_throughput[0].media_kind,
        proto::MediaKind::MediaVideo as i32
    );
    assert_eq!(metrics.media_throughput[0].size_bucket, "large");
    assert_eq!(metrics.media_throughput[0].files, 1);
    assert_eq!(metrics.media_throughput[0].bytes, 256 * 1024 * 1024);
}

#[tokio::test]
async fn disk_read_metrics_keep_per_disk_per_class_lifecycle_and_invariants() {
    let registry = RuntimeTaskRegistry::new();
    let task = registry
        .begin(
            RuntimeTaskKind::BaseCompute,
            MachineId::from_sha256([0xb3; 32]),
            "逐盘读取许可",
        )
        .await;
    task.configure_pipeline_nowait(ownership_config()).unwrap();

    // 同一复合盘身份只占用一次，快照必须按物理盘 ID 排序。
    let disks = vec![
        "PhysicalDisk2".to_owned(),
        "PhysicalDisk1".to_owned(),
        "PhysicalDisk2".to_owned(),
    ];
    task.disk_read_waiting_nowait(&disks, RuntimeDiskReadClass::Hash, 1)
        .unwrap();
    let waiting = registry
        .details(task.id())
        .await
        .unwrap()
        .pipeline_metrics
        .unwrap()
        .disk_reads;
    assert_eq!(
        waiting
            .iter()
            .map(|item| {
                (
                    item.physical_disk_id.as_str(),
                    item.capacity,
                    item.hash_waiting,
                    item.hash_active,
                )
            })
            .collect::<Vec<_>>(),
        vec![
            ("PhysicalDisk1", Some(1), Some(1), Some(0)),
            ("PhysicalDisk2", Some(1), Some(1), Some(0)),
        ],
        "waiting 必须在两个底层盘上从 0 变为 1，重复 ID 不得重复计数"
    );

    task.disk_read_acquired_nowait(&disks, RuntimeDiskReadClass::Hash)
        .unwrap();
    task.disk_read_released_nowait(&disks, RuntimeDiskReadClass::Hash)
        .unwrap();
    task.disk_read_waiting_nowait(
        &["PhysicalDisk1".to_owned()],
        RuntimeDiskReadClass::Media,
        1,
    )
    .unwrap();
    task.disk_read_acquired_nowait(&["PhysicalDisk1".to_owned()], RuntimeDiskReadClass::Media)
        .unwrap();
    task.disk_read_released_nowait(&["PhysicalDisk1".to_owned()], RuntimeDiskReadClass::Media)
        .unwrap();
    let released = registry
        .details(task.id())
        .await
        .unwrap()
        .pipeline_metrics
        .unwrap()
        .disk_reads;
    assert_eq!(
        released
            .iter()
            .map(|item| {
                (
                    item.physical_disk_id.as_str(),
                    item.hash_waiting,
                    item.hash_active,
                    item.hash_granted_total,
                    item.hash_released_total,
                    item.media_waiting,
                    item.media_active,
                    item.media_granted_total,
                    item.media_released_total,
                )
            })
            .collect::<Vec<_>>(),
        vec![
            (
                "PhysicalDisk1",
                Some(0),
                Some(0),
                Some(1),
                Some(1),
                Some(0),
                Some(0),
                Some(1),
                Some(1)
            ),
            (
                "PhysicalDisk2",
                Some(0),
                Some(0),
                Some(1),
                Some(1),
                Some(0),
                Some(0),
                Some(0),
                Some(0)
            ),
        ],
        "许可取得和释放必须保持逐盘、逐类别的单调累计与 0→1→0 当前值"
    );

    assert!(matches!(
        task.disk_read_released_nowait(&["PhysicalDisk1".to_owned()], RuntimeDiskReadClass::Media),
        Err(RuntimeTaskError::InvalidTransition)
    ));
    task.disk_read_waiting_nowait(&["PhysicalDisk1".to_owned()], RuntimeDiskReadClass::Hash, 1)
        .unwrap();
    task.disk_read_acquired_nowait(&["PhysicalDisk1".to_owned()], RuntimeDiskReadClass::Hash)
        .unwrap();
    task.disk_read_waiting_nowait(&["PhysicalDisk1".to_owned()], RuntimeDiskReadClass::Hash, 1)
        .unwrap();
    assert!(matches!(
        task.disk_read_acquired_nowait(&["PhysicalDisk1".to_owned()], RuntimeDiskReadClass::Hash),
        Err(RuntimeTaskError::CapacityExceeded)
    ));
    let capacity_rejected = registry
        .details(task.id())
        .await
        .unwrap()
        .pipeline_metrics
        .unwrap()
        .disk_reads;
    let disk1 = capacity_rejected
        .iter()
        .find(|item| item.physical_disk_id == "PhysicalDisk1")
        .unwrap();
    assert_eq!(
        (
            disk1.hash_waiting,
            disk1.hash_active,
            disk1.hash_granted_total
        ),
        (Some(1), Some(1), Some(2)),
        "拒绝 active 超容量时不得部分扣减 waiting 或累计授予"
    );

    task.finish(RuntimeTaskState::Completed).await.unwrap();
    assert!(
        registry
            .details(task.id())
            .await
            .unwrap()
            .pipeline_metrics
            .unwrap()
            .disk_reads
            .iter()
            .all(|item| item.hash_waiting == Some(0)
                && item.hash_active == Some(0)
                && item.media_waiting == Some(0)
                && item.media_active == Some(0)),
        "终态必须归零逐盘 waiting/active，累计值保持不变"
    );
}

#[tokio::test]
async fn disk_read_apis_reject_empty_or_blank_disk_ids_without_creating_metrics() {
    let registry = RuntimeTaskRegistry::new();
    let task = registry
        .begin(
            RuntimeTaskKind::BaseCompute,
            MachineId::from_sha256([0xb6; 32]),
            "无效物理盘 ID",
        )
        .await;
    task.configure_pipeline_nowait(ownership_config()).unwrap();

    for disk_ids in [Vec::new(), vec![String::new()], vec![" \t ".into()]] {
        assert!(matches!(
            task.disk_read_waiting_nowait(&disk_ids, RuntimeDiskReadClass::Hash, 1),
            Err(RuntimeTaskError::InvalidTransition)
        ));
        assert!(matches!(
            task.disk_read_wait_cancelled_nowait(&disk_ids, RuntimeDiskReadClass::Hash),
            Err(RuntimeTaskError::InvalidTransition)
        ));
        assert!(matches!(
            task.disk_read_acquired_nowait(&disk_ids, RuntimeDiskReadClass::Hash),
            Err(RuntimeTaskError::InvalidTransition)
        ));
        assert!(matches!(
            task.disk_read_released_nowait(&disk_ids, RuntimeDiskReadClass::Hash),
            Err(RuntimeTaskError::InvalidTransition)
        ));
    }
    assert!(
        registry
            .details(task.id())
            .await
            .unwrap()
            .pipeline_metrics
            .unwrap()
            .disk_reads
            .is_empty()
    );
}

#[tokio::test]
async fn disk_read_failures_keep_composite_disks_atomic_and_classify_only_acquire_capacity() {
    let registry = RuntimeTaskRegistry::new();
    let task = registry
        .begin(
            RuntimeTaskKind::BaseCompute,
            MachineId::from_sha256([0xb7; 32]),
            "复合盘原子错误",
        )
        .await;
    task.configure_pipeline_nowait(ownership_config()).unwrap();
    let pair = vec!["PhysicalDisk1".to_owned(), "PhysicalDisk2".to_owned()];
    let missing_pair = vec!["PhysicalDisk1".to_owned(), "MissingDisk".to_owned()];

    task.disk_read_waiting_nowait(&pair, RuntimeDiskReadClass::Hash, 1)
        .unwrap();
    assert!(matches!(
        task.disk_read_wait_cancelled_nowait(&missing_pair, RuntimeDiskReadClass::Hash),
        Err(RuntimeTaskError::InvalidTransition)
    ));
    let after_cancel_rejection = registry
        .details(task.id())
        .await
        .unwrap()
        .pipeline_metrics
        .unwrap()
        .disk_reads;
    assert!(
        after_cancel_rejection
            .iter()
            .all(|item| item.hash_waiting == Some(1) && item.hash_active == Some(0))
    );

    task.disk_read_acquired_nowait(&pair, RuntimeDiskReadClass::Hash)
        .unwrap();
    assert!(matches!(
        task.disk_read_released_nowait(&missing_pair, RuntimeDiskReadClass::Hash),
        Err(RuntimeTaskError::InvalidTransition)
    ));
    let after_release_rejection = registry
        .details(task.id())
        .await
        .unwrap()
        .pipeline_metrics
        .unwrap()
        .disk_reads;
    assert!(
        after_release_rejection
            .iter()
            .all(|item| item.hash_waiting == Some(0)
                && item.hash_active == Some(1)
                && item.hash_granted_total == Some(1)
                && item.hash_released_total == Some(0))
    );

    let capacity_task = registry
        .begin(
            RuntimeTaskKind::BaseCompute,
            MachineId::from_sha256([0xb8; 32]),
            "复合盘容量原子错误",
        )
        .await;
    capacity_task
        .configure_pipeline_nowait(ownership_config())
        .unwrap();
    capacity_task
        .disk_read_waiting_nowait(
            &["PhysicalDisk3".to_owned()],
            RuntimeDiskReadClass::Media,
            1,
        )
        .unwrap();
    capacity_task
        .disk_read_acquired_nowait(&["PhysicalDisk3".to_owned()], RuntimeDiskReadClass::Media)
        .unwrap();
    let capacity_pair = vec!["PhysicalDisk3".to_owned(), "PhysicalDisk4".to_owned()];
    capacity_task
        .disk_read_waiting_nowait(&capacity_pair, RuntimeDiskReadClass::Hash, 1)
        .unwrap();
    assert!(matches!(
        capacity_task.disk_read_acquired_nowait(&capacity_pair, RuntimeDiskReadClass::Hash),
        Err(RuntimeTaskError::CapacityExceeded)
    ));
    let after_acquire_rejection = registry
        .details(capacity_task.id())
        .await
        .unwrap()
        .pipeline_metrics
        .unwrap()
        .disk_reads;
    assert_eq!(
        after_acquire_rejection
            .iter()
            .map(|item| {
                (
                    item.physical_disk_id.as_str(),
                    item.hash_waiting,
                    item.hash_active,
                    item.media_active,
                )
            })
            .collect::<Vec<_>>(),
        vec![
            ("PhysicalDisk3", Some(1), Some(0), Some(1)),
            ("PhysicalDisk4", Some(1), Some(0), Some(0)),
        ],
        "一个底层盘容量拒绝时，另一盘不得被部分取得"
    );

    assert!(matches!(
        capacity_task
            .disk_read_released_nowait(&["PhysicalDisk3".to_owned()], RuntimeDiskReadClass::Hash),
        Err(RuntimeTaskError::InvalidTransition)
    ));
}

#[tokio::test]
async fn ownership_metrics_are_lazy_bounded_peak_preserving_and_terminal_safe() {
    let registry = RuntimeTaskRegistry::new();
    let task = registry
        .begin(
            RuntimeTaskKind::BaseCompute,
            MachineId::from_sha256([0xb4; 32]),
            "ownership registry",
        )
        .await;
    task.configure_pipeline_nowait(ownership_config()).unwrap();

    let initial = registry
        .details(task.id())
        .await
        .unwrap()
        .pipeline_metrics
        .unwrap();
    assert_eq!(
        initial.hash_waiting_permit, None,
        "未发布 ownership 必须保持 None"
    );
    assert_eq!(
        initial.decode_credit_owned, None,
        "未发布 credit 必须保持 None"
    );

    task.update_ownership_nowait(RuntimePipelineOwnership::HashWaitingPermit, 2, 3)
        .unwrap();
    task.update_ownership_nowait(RuntimePipelineOwnership::HashWaitingPermit, 1, 3)
        .unwrap();
    task.update_ownership_nowait(RuntimePipelineOwnership::HashWaitingPermit, 0, 3)
        .unwrap();
    let metrics = registry
        .details(task.id())
        .await
        .unwrap()
        .pipeline_metrics
        .unwrap();
    let hash_waiting = metrics.hash_waiting_permit.unwrap();
    assert_eq!(
        (
            hash_waiting.current,
            hash_waiting.peak,
            hash_waiting.capacity
        ),
        (Some(0), Some(2), Some(3))
    );
    assert!(matches!(
        task.update_ownership_nowait(RuntimePipelineOwnership::HashWaitingPermit, 4, 3),
        Err(RuntimeTaskError::CapacityExceeded)
    ));
    let unchanged = registry
        .details(task.id())
        .await
        .unwrap()
        .pipeline_metrics
        .unwrap()
        .hash_waiting_permit
        .unwrap();
    assert_eq!(
        (unchanged.current, unchanged.peak, unchanged.capacity),
        (Some(0), Some(2), Some(3)),
        "ownership 越界更新不得污染旧快照"
    );

    task.update_ownership_nowait(RuntimePipelineOwnership::DecodeCreditOwned, 1, 2)
        .unwrap();
    task.finish(RuntimeTaskState::Completed).await.unwrap();
    let terminal = registry
        .details(task.id())
        .await
        .unwrap()
        .pipeline_metrics
        .unwrap();
    let decode = terminal.decode_credit_owned.unwrap();
    assert_eq!((decode.current, decode.peak), (Some(0), Some(1)));
    assert_eq!(terminal.hash_waiting_permit.unwrap().current, Some(0));
    assert!(task.finish(RuntimeTaskState::Completed).await.is_err());
    assert_eq!(
        registry
            .details(task.id())
            .await
            .unwrap()
            .pipeline_metrics
            .unwrap()
            .decode_credit_owned
            .unwrap()
            .current,
        Some(0),
        "重复终态不得造成 ownership 下溢"
    );
}

#[tokio::test]
async fn control_state_is_lazy_bounded_and_separate_from_ownership() {
    let registry = RuntimeTaskRegistry::new();
    let task = registry
        .begin(
            RuntimeTaskKind::BaseCompute,
            MachineId::from_sha256([0xb5; 32]),
            "control state",
        )
        .await;
    task.configure_pipeline_nowait(ownership_config()).unwrap();

    task.update_control_state_nowait(RuntimePipelineControl::HashRefillTokenAvailable, 1, 1)
        .unwrap();
    let before_rejection = registry
        .details(task.id())
        .await
        .unwrap()
        .pipeline_metrics
        .unwrap()
        .hash_refill_token_available
        .unwrap();
    assert_eq!(
        (
            before_rejection.current,
            before_rejection.peak,
            before_rejection.capacity
        ),
        (Some(1), Some(1), Some(1))
    );
    assert!(matches!(
        task.update_control_state_nowait(RuntimePipelineControl::HashRefillTokenAvailable, 2, 1),
        Err(RuntimeTaskError::CapacityExceeded)
    ));
    let after_rejection = registry
        .details(task.id())
        .await
        .unwrap()
        .pipeline_metrics
        .unwrap()
        .hash_refill_token_available
        .unwrap();
    assert_eq!(
        (
            after_rejection.current,
            after_rejection.peak,
            after_rejection.capacity
        ),
        (Some(1), Some(1), Some(1)),
        "control-state 越界更新不得污染旧快照"
    );
    task.update_control_state_nowait(RuntimePipelineControl::HashRefillTokenAvailable, 0, 1)
        .unwrap();
    let metrics = registry
        .details(task.id())
        .await
        .unwrap()
        .pipeline_metrics
        .unwrap();
    let refill = metrics.hash_refill_token_available.unwrap();
    assert_eq!(
        (refill.current, refill.peak, refill.capacity),
        (Some(0), Some(1), Some(1))
    );
    assert_eq!(
        metrics.hash_waiting_permit, None,
        "control-state 不得伪造 ownership 条目"
    );

    task.finish(RuntimeTaskState::Completed).await.unwrap();
    let terminal = registry
        .details(task.id())
        .await
        .unwrap()
        .pipeline_metrics
        .unwrap();
    assert_eq!(
        terminal.hash_refill_token_available.unwrap().current,
        Some(0)
    );
}

#[tokio::test]
async fn every_pipeline_ownership_kind_maps_to_its_declared_proto_field() {
    let registry = RuntimeTaskRegistry::new();
    let task = registry
        .begin(
            RuntimeTaskKind::BaseCompute,
            MachineId::from_sha256([0xb7; 32]),
            "ownership field mapping",
        )
        .await;
    task.configure_pipeline_nowait(ownership_config()).unwrap();

    let ownership_fields = [
        (
            RuntimePipelineOwnership::HashWaitingPermit,
            "hash_waiting_permit",
        ),
        (RuntimePipelineOwnership::HashReading, "hash_reading"),
        (
            RuntimePipelineOwnership::HashCompletedUnjoined,
            "hash_completed_unjoined",
        ),
        (
            RuntimePipelineOwnership::MediaPermitWaiting,
            "media_permit_waiting",
        ),
        (
            RuntimePipelineOwnership::MediaAcquireReady,
            "media_acquire_ready",
        ),
        (
            RuntimePipelineOwnership::MediaPermitReady,
            "media_permit_ready",
        ),
        (
            RuntimePipelineOwnership::WorkerDispatching,
            "worker_dispatching",
        ),
        (
            RuntimePipelineOwnership::WorkerStartPending,
            "worker_start_pending",
        ),
        (RuntimePipelineOwnership::WorkerDecode, "worker_decode"),
        (RuntimePipelineOwnership::WorkerFeature, "worker_feature"),
        (
            RuntimePipelineOwnership::WorkerResultWait,
            "worker_result_wait",
        ),
        (
            RuntimePipelineOwnership::WorkerPhaseUnknown,
            "worker_phase_unknown",
        ),
        (
            RuntimePipelineOwnership::ContentOutputCreditOwned,
            "content_output_credit_owned",
        ),
        (
            RuntimePipelineOwnership::DecodeCreditOwned,
            "decode_credit_owned",
        ),
    ];
    for (index, (kind, _field)) in ownership_fields.iter().enumerate() {
        let current = (index + 1) as u64;
        task.update_ownership_nowait(*kind, current, 100 + current)
            .unwrap();
    }
    task.update_control_state_nowait(RuntimePipelineControl::HashRefillTokenAvailable, 99, 199)
        .unwrap();

    let metrics = registry
        .details(task.id())
        .await
        .unwrap()
        .pipeline_metrics
        .unwrap();
    for (index, (_kind, field)) in ownership_fields.iter().enumerate() {
        let current = (index + 1) as u64;
        let entry = ownership_proto_field(&metrics, field)
            .unwrap_or_else(|| panic!("ownership kind {field} 未发布到对应协议字段"));
        assert_eq!(
            (entry.current, entry.peak, entry.capacity),
            (Some(current), Some(current), Some(100 + current)),
            "ownership kind {field} 映射串位"
        );
    }
    let control = metrics.hash_refill_token_available.unwrap();
    assert_eq!(
        (control.current, control.peak, control.capacity),
        (Some(99), Some(99), Some(199)),
        "control-state 不得落入 ownership 字段"
    );
}

/// 按协议字段名读取 ownership 快照，集中保持表驱动测试的映射断言。
fn ownership_proto_field<'a>(
    metrics: &'a proto::RuntimePipelineMetrics,
    field: &str,
) -> Option<&'a proto::RuntimeOwnershipMetrics> {
    match field {
        "hash_waiting_permit" => metrics.hash_waiting_permit.as_ref(),
        "hash_reading" => metrics.hash_reading.as_ref(),
        "hash_completed_unjoined" => metrics.hash_completed_unjoined.as_ref(),
        "media_permit_waiting" => metrics.media_permit_waiting.as_ref(),
        "media_acquire_ready" => metrics.media_acquire_ready.as_ref(),
        "media_permit_ready" => metrics.media_permit_ready.as_ref(),
        "worker_dispatching" => metrics.worker_dispatching.as_ref(),
        "worker_start_pending" => metrics.worker_start_pending.as_ref(),
        "worker_decode" => metrics.worker_decode.as_ref(),
        "worker_feature" => metrics.worker_feature.as_ref(),
        "worker_result_wait" => metrics.worker_result_wait.as_ref(),
        "worker_phase_unknown" => metrics.worker_phase_unknown.as_ref(),
        "content_output_credit_owned" => metrics.content_output_credit_owned.as_ref(),
        "decode_credit_owned" => metrics.decode_credit_owned.as_ref(),
        _ => None,
    }
}

#[tokio::test]
async fn item_completion_latency_accumulates_histogram_statistics() {
    let registry = RuntimeTaskRegistry::new();
    let task = registry
        .begin(
            RuntimeTaskKind::BaseCompute,
            MachineId::from_sha256([0xb6; 32]),
            "item latency",
        )
        .await;
    task.configure_pipeline_nowait(ownership_config()).unwrap();
    let initial = registry
        .details(task.id())
        .await
        .unwrap()
        .pipeline_metrics
        .unwrap();
    assert_eq!(initial.item_completion_latency, None);

    for millis in [1_u64, 2, 4, 8, 32] {
        task.record_item_completion_latency_nowait(Duration::from_millis(millis))
            .unwrap();
    }
    let histogram = registry
        .details(task.id())
        .await
        .unwrap()
        .pipeline_metrics
        .unwrap()
        .item_completion_latency
        .unwrap();
    assert_eq!(histogram.count, 5);
    assert_eq!(histogram.p50_ms, Some(4));
    assert_eq!(histogram.p95_ms, Some(32));
    assert_eq!(histogram.p99_ms, Some(32));
    assert_eq!(histogram.max_ms, Some(32));
}

/// 复用测试所需的最小流水线配置，容量值保持可读且足以覆盖边界。
fn ownership_config() -> RuntimeExecutionConfigUpdate {
    RuntimeExecutionConfigUpdate {
        hash_tasks: 3,
        path_cache_queue_capacity: 1,
        content_cache_queue_capacity: 1,
        decode_queue_capacity: 2,
        persist_queue_capacity: 1,
        worker_slots: 2,
        cpu_budget: 4,
        global_disk_permits: 2,
        hdd_per_disk_permits: 1,
        ssd_per_disk_permits: 1,
        unknown_per_disk_permits: 1,
    }
}

#[tokio::test]
async fn source_complete_does_not_change_phase_and_stale_release_cannot_idle_next_item() {
    let registry = RuntimeTaskRegistry::new();
    let task = registry
        .begin(
            RuntimeTaskKind::BaseCompute,
            MachineId::from_sha256([0xb2; 32]),
            "Worker 阶段",
        )
        .await;
    task.worker_started(RuntimeWorkerUpdate {
        slot: 0,
        process_id: Some(7001),
        item_id: "item-1".into(),
        stage: RuntimeStage::ComputeBaseFeatures,
        display_path: r"D:\Media\one.mp4".into(),
        physical_disk_id: "PhysicalDisk7".into(),
        completed_files: 0,
        speed_per_second: 0.0,
        current_step: "解码".into(),
        cache_detail: String::new(),
        phase: Some(proto::RuntimeWorkerPhase::RuntimeWorkerDecode),
        cpu_weight: Some(3),
        decoder_threads: Some(3),
    })
    .await
    .unwrap();
    task.worker_source_read_complete_nowait(0, "item-1", Some(Duration::from_millis(8)))
        .unwrap();
    assert_eq!(
        registry.details(task.id()).await.unwrap().workers[0].phase,
        Some(proto::RuntimeWorkerPhase::RuntimeWorkerDecode as i32),
        "SourceReadComplete 只释放媒体资源，不得推断 Worker phase"
    );
    task.worker_phase_nowait(
        0,
        "item-1",
        proto::RuntimeWorkerPhase::RuntimeWorkerFeature,
        Some(Duration::from_millis(9)),
    )
    .unwrap();
    task.worker_released_nowait(0, "item-1").unwrap();
    task.worker_started(RuntimeWorkerUpdate {
        slot: 0,
        process_id: Some(7001),
        item_id: "item-2".into(),
        stage: RuntimeStage::ComputeBaseFeatures,
        display_path: r"D:\Media\two.mp4".into(),
        physical_disk_id: "PhysicalDisk7".into(),
        completed_files: 0,
        speed_per_second: 0.0,
        current_step: "解码".into(),
        cache_detail: String::new(),
        phase: Some(proto::RuntimeWorkerPhase::RuntimeWorkerDecode),
        cpu_weight: Some(2),
        decoder_threads: Some(2),
    })
    .await
    .unwrap();
    task.worker_released_nowait(0, "item-1").unwrap();
    let worker = &registry.details(task.id()).await.unwrap().workers[0];
    assert_eq!(worker.display_path, r"D:\Media\two.mp4");
    assert_eq!(
        worker.phase,
        Some(proto::RuntimeWorkerPhase::RuntimeWorkerDecode as i32),
        "旧终态不得把已运行下一项的 slot 设为 idle"
    );
}

#[tokio::test]
async fn queued_started_and_terminal_events_preserve_exact_slot_and_cpu_peaks() {
    let registry = RuntimeTaskRegistry::new();
    let task = registry
        .begin(
            RuntimeTaskKind::BaseCompute,
            MachineId::from_sha256([0xb3; 32]),
            "积压 Worker 资源事件",
        )
        .await;
    task.configure_pipeline_nowait(RuntimeExecutionConfigUpdate {
        hash_tasks: 1,
        path_cache_queue_capacity: 2,
        content_cache_queue_capacity: 1,
        decode_queue_capacity: 3,
        persist_queue_capacity: 3,
        worker_slots: 2,
        cpu_budget: 5,
        global_disk_permits: 1,
        hdd_per_disk_permits: 1,
        ssd_per_disk_permits: 1,
        unknown_per_disk_permits: 1,
    })
    .unwrap();

    for (slot, item_id, cpu_weight) in [(0, "item-a", 2), (1, "item-b", 3)] {
        task.worker_started(RuntimeWorkerUpdate {
            slot,
            process_id: Some(8000 + slot),
            item_id: item_id.into(),
            stage: RuntimeStage::ComputeBaseFeatures,
            display_path: format!(r"D:\Media\{item_id}.mp4"),
            physical_disk_id: "PhysicalDisk8".into(),
            completed_files: 0,
            speed_per_second: 0.0,
            current_step: "解码".into(),
            cache_detail: String::new(),
            phase: Some(proto::RuntimeWorkerPhase::RuntimeWorkerDecode),
            cpu_weight: Some(cpu_weight),
            decoder_threads: Some(cpu_weight),
        })
        .await
        .unwrap();
    }
    // 模拟消费事件时 Pool 已经处理完终态并回到空闲；峰值只能来自事件身份。
    task.worker_released_nowait(0, "item-a").unwrap();
    task.worker_released_nowait(1, "item-b").unwrap();

    let metrics = registry
        .details(task.id())
        .await
        .unwrap()
        .pipeline_metrics
        .unwrap();
    let slots = metrics.worker_slots.unwrap();
    assert_eq!((slots.current, slots.peak), (Some(0), Some(2)));
    let cpu = metrics.cpu_weight.unwrap();
    assert_eq!((cpu.current, cpu.peak), (Some(0), Some(5)));
}

#[tokio::test]
async fn registry_tracks_parallel_stages_speed_workers_failures_and_one_terminal_event() {
    let clock = Arc::new(ManualClock::default());
    let registry = RuntimeTaskRegistry::with_clock(clock.clone());
    let mut events = registry.subscribe();
    let task = registry
        .begin(
            RuntimeTaskKind::Scan,
            MachineId::from_sha256([0x91; 32]),
            "扫描",
        )
        .await;
    task.update_overall(0, None, 0, 0).await.unwrap();
    task.update_stage(RuntimeStageUpdate::running(
        RuntimeStage::ReadMd5,
        RuntimeProgressUnit::Bytes,
        0,
        Some(100),
    ))
    .await
    .unwrap();
    task.update_stage(RuntimeStageUpdate::running(
        RuntimeStage::ProbeStage1,
        RuntimeProgressUnit::Files,
        0,
        None,
    ))
    .await
    .unwrap();

    let summary = &registry.list().await[0];
    assert_eq!(summary.machine_id.len(), 64);
    assert_eq!(summary.state, "running");
    assert_eq!(summary.stage_summary, "读取与 MD5 / 媒体探测与一筛并行");
    assert!(!summary.overall_total_known);

    clock.advance(Duration::from_secs(5));
    task.update_stage(RuntimeStageUpdate::running(
        RuntimeStage::ReadMd5,
        RuntimeProgressUnit::Bytes,
        50,
        Some(100),
    ))
    .await
    .unwrap();
    let stage = &registry.details(task.id()).await.unwrap().stages[0];
    assert_eq!(stage.speed_per_second, 10.0);
    assert_eq!(stage.eta_ms, Some(5_000));
    assert!(stage.total_known);

    task.update_stage(RuntimeStageUpdate::running(
        RuntimeStage::ReadMd5,
        RuntimeProgressUnit::Bytes,
        55,
        Some(100),
    ))
    .await
    .unwrap();
    assert!(
        registry.details(task.id()).await.unwrap().stages[0]
            .speed_per_second
            .is_finite()
    );
    clock.advance(Duration::from_secs(11));
    task.update_stage(RuntimeStageUpdate::running(
        RuntimeStage::ReadMd5,
        RuntimeProgressUnit::Bytes,
        5,
        Some(100),
    ))
    .await
    .unwrap();
    let reset = &registry.details(task.id()).await.unwrap().stages[0];
    assert_eq!(reset.speed_per_second, 0.0, "counter 回退必须重置速度窗口");
    assert_eq!(reset.eta_ms, None);

    task.update_worker(RuntimeWorkerUpdate {
        slot: 3,
        process_id: Some(9001),
        item_id: "legacy-worker-item".into(),
        stage: RuntimeStage::ProbeStage1,
        display_path: r"D:\Media\clip.mp4".into(),
        physical_disk_id: "PhysicalDisk7".into(),
        completed_files: 18,
        speed_per_second: 3.5,
        current_step: "生成缩略图".into(),
        cache_detail: "复用本地缩略图".into(),
        phase: None,
        cpu_weight: None,
        decoder_threads: None,
    })
    .await
    .unwrap();
    for index in 0..25 {
        task.record_failure(RuntimeFailureUpdate {
            stage: RuntimeStage::ReadMd5,
            display_path: format!(r"D:\Media\broken-{index}.bin"),
            message: format!("failure-{index}"),
        })
        .await
        .unwrap();
    }
    let details = registry.details(task.id()).await.unwrap();
    assert_eq!(details.workers[0].slot, 3);
    assert_eq!(details.workers[0].process_id, Some(9001));
    assert_eq!(details.failures.len(), MAX_RUNTIME_FAILURES);
    assert!(details.failures[0].display_path.ends_with("broken-5.bin"));

    while events.try_recv().is_ok() {}
    task.finish(RuntimeTaskState::Completed).await.unwrap();
    let event = events.recv().await.unwrap();
    assert_eq!(event.runtime_task_id, task.id());
    assert_eq!(event.state, "completed");
    assert!(task.finish(RuntimeTaskState::Completed).await.is_err());
    assert!(
        task.update_stage(RuntimeStageUpdate {
            stage: RuntimeStage::ReadMd5,
            state: proto::RuntimeStageState::RuntimeStageRunning,
            unit: RuntimeProgressUnit::Bytes,
            completed: 99,
            total: Some(100),
            failed: 0,
            skipped: 0,
        })
        .await
        .is_err(),
        "终态后不得倒退到 Running"
    );
    assert!(events.try_recv().is_err(), "终态只能广播一次");
}

#[tokio::test]
async fn recreated_registry_is_empty_and_never_restores_process_history() {
    let clock = Arc::new(ManualClock::default());
    let registry = RuntimeTaskRegistry::with_clock(clock.clone());
    registry
        .begin(
            RuntimeTaskKind::Delete,
            MachineId::from_sha256([0x92; 32]),
            "删除",
        )
        .await;
    assert_eq!(registry.list().await.len(), 1);
    drop(registry);

    let recreated = RuntimeTaskRegistry::with_clock(clock);
    assert!(recreated.list().await.is_empty());
}

#[tokio::test]
async fn every_terminal_stage_state_rejects_all_later_terminal_replacements_without_mutation() {
    let transitions = [
        (
            proto::RuntimeStageState::RuntimeStageCompleted,
            proto::RuntimeStageState::RuntimeStageFailed,
        ),
        (
            proto::RuntimeStageState::RuntimeStageFailed,
            proto::RuntimeStageState::RuntimeStageSkipped,
        ),
        (
            proto::RuntimeStageState::RuntimeStageSkipped,
            proto::RuntimeStageState::RuntimeStageCompleted,
        ),
    ];
    for (terminal, replacement) in transitions {
        let clock = Arc::new(ManualClock::default());
        let registry = RuntimeTaskRegistry::with_clock(clock.clone());
        let task = registry
            .begin(
                RuntimeTaskKind::Scan,
                MachineId::from_sha256([terminal as u8; 32]),
                "阶段终态",
            )
            .await;
        task.update_stage(RuntimeStageUpdate::running(
            RuntimeStage::ReadMd5,
            RuntimeProgressUnit::Bytes,
            10,
            Some(100),
        ))
        .await
        .unwrap();
        clock.advance(Duration::from_secs(2));
        task.update_stage(RuntimeStageUpdate {
            stage: RuntimeStage::ReadMd5,
            state: terminal,
            unit: RuntimeProgressUnit::Bytes,
            completed: 80,
            total: Some(100),
            failed: 2,
            skipped: 3,
        })
        .await
        .expect("Running 必须能合法进入任一 terminal");
        let before = registry.details(task.id()).await.unwrap().stages[0].clone();
        clock.advance(Duration::from_secs(5));
        assert!(
            task.update_stage(RuntimeStageUpdate {
                stage: RuntimeStage::ReadMd5,
                state: replacement,
                unit: RuntimeProgressUnit::Files,
                completed: 99,
                total: Some(999),
                failed: 9,
                skipped: 9,
            })
            .await
            .is_err()
        );
        let after = registry.details(task.id()).await.unwrap().stages[0].clone();
        assert_eq!(
            after, before,
            "拒绝后 state/count/speed/elapsed/ETA 必须完全不变"
        );
    }
}
