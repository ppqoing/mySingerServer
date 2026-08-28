use std::{fs, io, path::Path, sync::Arc, time::Duration};

use dedup_core::{DiskReadConfig, DisplayPath, MachineId, NormalizedPath};
use dedup_node_engine::{
    io::{BlockReadError, BlockReader, DiskReadClass},
    runtime_tasks::{RuntimeExecutionConfigUpdate, RuntimeTaskKind, RuntimeTaskRegistry},
    scan::{
        HashPermitReader, PipelineFileReader, PlannedScannedPath, ScheduledFileReader,
        TaskDiskLane, md5_bytes,
    },
    task_dispatch::{TaskFileDispatcher, TaskLanePermitProvider},
    task_files::{TaskFileRecord, TaskWorkKind, TaskWorkMask, TransientTaskFileSet},
};
use dedup_node_store::ScannedPath;
use dedup_windows::{LocalDiskKind, PhysicalDiskId, ReadCancellationToken};
use uuid::Uuid;

/// 让测试读取器直接返回文件字节，验证外部许可读取确实完成了完整 MD5。
#[derive(Clone, Copy)]
struct FixedBlockReader;

impl BlockReader for FixedBlockReader {
    fn read_at(
        &self,
        path: &Path,
        offset: u64,
        buffer: &mut [u8],
        _timeout: Duration,
        _cancellation: &ReadCancellationToken,
    ) -> Result<usize, BlockReadError> {
        let bytes = fs::read(path).map_err(BlockReadError::Io)?;
        let offset = usize::try_from(offset).map_err(|_| {
            BlockReadError::Io(io::Error::new(
                io::ErrorKind::InvalidInput,
                "offset overflow",
            ))
        })?;
        if offset >= bytes.len() {
            return Ok(0);
        }
        let length = buffer.len().min(bytes.len() - offset);
        buffer[..length].copy_from_slice(&bytes[offset..offset + length]);
        Ok(length)
    }
}

fn lane(numbers: &[u32], kind: LocalDiskKind, limit: usize) -> TaskDiskLane {
    TaskDiskLane {
        physical_disk_id: PhysicalDiskId::from_disk_numbers(numbers.iter().copied()).unwrap(),
        physical_disk_numbers: numbers.to_vec(),
        disk_kind: kind,
        configured_weight: limit,
        per_disk_limit: limit,
    }
}

fn scanned(path: &Path) -> ScannedPath {
    ScannedPath::new(
        NormalizedPath::new(path).unwrap(),
        DisplayPath::new(path).unwrap(),
        fs::metadata(path).unwrap().len(),
    )
}

/// 创建测试 reporter 所需的完整流水线容量快照。
fn reporter_config(config: &DiskReadConfig) -> RuntimeExecutionConfigUpdate {
    RuntimeExecutionConfigUpdate {
        hash_tasks: config.total_threads as u32,
        path_cache_queue_capacity: 1,
        content_cache_queue_capacity: 1,
        decode_queue_capacity: 1,
        persist_queue_capacity: 1,
        worker_slots: 1,
        cpu_budget: 1,
        global_disk_permits: config.total_threads as u32,
        hdd_per_disk_permits: config.hdd_threads_per_disk as u32,
        ssd_per_disk_permits: config.ssd_threads_per_disk as u32,
        unknown_per_disk_permits: config.unknown_threads_per_disk as u32,
    }
}

/// 创建只用于本测试的运行指标 reporter，避免复用旧任务状态。
async fn reporter_for_test(
    config: &DiskReadConfig,
) -> (
    RuntimeTaskRegistry,
    dedup_node_engine::runtime_tasks::RuntimeTaskReporter,
) {
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(
            RuntimeTaskKind::BaseCompute,
            MachineId::from_sha256([0x71; 32]),
            "外部 Hash 读取许可",
        )
        .await;
    reporter
        .configure_pipeline_nowait(reporter_config(config))
        .unwrap();
    (registry, reporter)
}

/// 为真实任务文件生成一个只缺少 MD5 的基础计算行。
fn hash_record(scanned: ScannedPath) -> TaskFileRecord {
    TaskFileRecord {
        item_id: Uuid::now_v7(),
        work_kind: TaskWorkKind::Base,
        scanned,
        known_md5: None,
        missing: TaskWorkMask::for_base(true, 0).unwrap(),
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn dispatcher_permit_can_be_consumed_by_external_hash_read_without_second_scheduler_acquire()
{
    let task_root = tempfile::tempdir().unwrap();
    let media_root = tempfile::tempdir().unwrap();
    let path = media_root.path().join("single.bin");
    let content = b"one scheduler permit, one complete hash read";
    fs::write(&path, content).unwrap();
    let scanned = scanned(&path);
    let task_lane = lane(&[7], LocalDiskKind::Hdd, 1);
    let planned = Arc::new(vec![PlannedScannedPath {
        scanned: scanned.clone(),
        lane: task_lane.clone(),
    }]);

    let config = DiskReadConfig {
        total_threads: 1,
        hdd_threads_per_disk: 1,
        ssd_threads_per_disk: 1,
        unknown_threads_per_disk: 1,
        ..DiskReadConfig::default()
    };
    let (reader, _) = ScheduledFileReader::controlled_with_planned_rows_for_test(
        &config,
        1,
        FixedBlockReader,
        planned,
    )
    .unwrap();
    let mut dispatcher = TaskFileDispatcher::new(
        TransientTaskFileSet::create(task_root.path(), Uuid::now_v7().to_string()).unwrap(),
        reader.clone(),
    );
    let row = TaskFileRecord {
        item_id: Uuid::now_v7(),
        work_kind: TaskWorkKind::Base,
        scanned: scanned.clone(),
        known_md5: None,
        missing: TaskWorkMask::for_base(true, 0).unwrap(),
    };
    dispatcher.append_batch(&task_lane, &[row]).unwrap();
    dispatcher.seal().unwrap();

    let cancellation = ReadCancellationToken::new();
    let dispatched = tokio::time::timeout(
        Duration::from_secs(1),
        dispatcher.next(cancellation.clone()),
    )
    .await
    .expect("dispatcher 应在全局/逐盘各为 1 时取得唯一 Hash 许可")
    .unwrap()
    .unwrap();
    assert_eq!(dispatched.class, DiskReadClass::HashSequential);

    let identity = dispatched.identity.clone();
    let product = tokio::time::timeout(
        Duration::from_secs(1),
        reader.read_with_permit(
            dispatched.record.scanned.clone(),
            dispatched.permit,
            cancellation,
            None,
        ),
    )
    .await
    .expect("外部许可读取不能再次申请同一个 scheduler 槽位")
    .unwrap();
    assert_eq!(product.md5, md5_bytes(content));
    drop(product);
    dispatcher.mark_completed(&identity).unwrap();
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn external_hash_permit_reports_one_hash_lifetime_and_releases_after_result_drop() {
    let media_root = tempfile::tempdir().unwrap();
    let path = media_root.path().join("telemetry.bin");
    let content = b"hash telemetry must follow the one permit";
    fs::write(&path, content).unwrap();
    let scanned = scanned(&path);
    let task_lane = lane(&[11], LocalDiskKind::Hdd, 1);
    let planned = Arc::new(vec![PlannedScannedPath {
        scanned: scanned.clone(),
        lane: task_lane.clone(),
    }]);
    let config = DiskReadConfig {
        total_threads: 1,
        hdd_threads_per_disk: 1,
        ssd_threads_per_disk: 1,
        unknown_threads_per_disk: 1,
        ..DiskReadConfig::default()
    };
    let (registry, reporter) = reporter_for_test(&config).await;
    let reporter_id = reporter.id().to_owned();
    let reader = ScheduledFileReader::controlled_with_planned_rows_for_test(
        &config,
        1,
        FixedBlockReader,
        planned,
    )
    .unwrap()
    .0
    .with_runtime_reporter(reporter.clone());

    let permit = reader
        .acquire(
            task_lane.clone(),
            DiskReadClass::HashSequential,
            ReadCancellationToken::new(),
        )
        .await
        .unwrap();
    let held = registry.details(&reporter_id).await.unwrap();
    let metrics = held.pipeline_metrics.unwrap();
    let hash_io = metrics.hash_io.unwrap();
    assert_eq!(hash_io.current, Some(1));
    assert_eq!(
        metrics
            .disk_reads
            .iter()
            .find(|item| item.physical_disk_id == "PhysicalDisk11")
            .map(|item| (item.hash_granted_total, item.hash_released_total)),
        Some((Some(1), Some(0)))
    );

    let product = reader
        .read_with_permit(scanned, permit, ReadCancellationToken::new(), None)
        .await
        .unwrap();
    assert_eq!(product.md5, md5_bytes(content));
    let still_held = registry.details(&reporter_id).await.unwrap();
    assert_eq!(
        still_held
            .pipeline_metrics
            .unwrap()
            .hash_io
            .unwrap()
            .current,
        Some(1)
    );
    drop(product);
    let released = registry.details(&reporter_id).await.unwrap();
    let metrics = released.pipeline_metrics.unwrap();
    assert_eq!(metrics.hash_io.unwrap().current, Some(0));
    assert_eq!(
        metrics
            .disk_reads
            .iter()
            .find(|item| item.physical_disk_id == "PhysicalDisk11")
            .map(|item| (item.hash_granted_total, item.hash_released_total)),
        Some((Some(1), Some(1)))
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn external_media_permit_reports_media_io_until_drop() {
    let media_root = tempfile::tempdir().unwrap();
    let path = media_root.path().join("media.bin");
    fs::write(&path, b"media permit").unwrap();
    let scanned = scanned(&path);
    let task_lane = lane(&[12], LocalDiskKind::Ssd, 1);
    let planned = Arc::new(vec![PlannedScannedPath {
        scanned,
        lane: task_lane.clone(),
    }]);
    let config = DiskReadConfig {
        total_threads: 1,
        hdd_threads_per_disk: 1,
        ssd_threads_per_disk: 1,
        unknown_threads_per_disk: 1,
        ..DiskReadConfig::default()
    };
    let (registry, reporter) = reporter_for_test(&config).await;
    let reporter_id = reporter.id().to_owned();
    let reader = ScheduledFileReader::controlled_with_planned_rows_for_test(
        &config,
        1,
        FixedBlockReader,
        planned,
    )
    .unwrap()
    .0
    .with_runtime_reporter(reporter);

    let permit = reader
        .acquire(
            task_lane,
            DiskReadClass::MediaDecode,
            ReadCancellationToken::new(),
        )
        .await
        .unwrap();
    let held = registry.details(&reporter_id).await.unwrap();
    assert_eq!(
        held.pipeline_metrics.unwrap().media_io.unwrap().current,
        Some(1)
    );
    drop(permit);
    let released = registry.details(&reporter_id).await.unwrap();
    assert_eq!(
        released.pipeline_metrics.unwrap().media_io.unwrap().current,
        Some(0)
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn cancelled_dispatch_wait_clears_metrics_and_keeps_tsv_rows_pending() {
    let task_root = tempfile::tempdir().unwrap();
    let media_root = tempfile::tempdir().unwrap();
    let paths = [
        media_root.path().join("wait-1.bin"),
        media_root.path().join("wait-2.bin"),
    ];
    for path in &paths {
        fs::write(path, b"waiting hash").unwrap();
    }
    let rows = paths.iter().map(|path| scanned(path)).collect::<Vec<_>>();
    let task_lane = lane(&[13], LocalDiskKind::Hdd, 1);
    let planned = Arc::new(
        rows.iter()
            .cloned()
            .map(|scanned| PlannedScannedPath {
                scanned,
                lane: task_lane.clone(),
            })
            .collect::<Vec<_>>(),
    );
    let config = DiskReadConfig {
        total_threads: 1,
        hdd_threads_per_disk: 1,
        ssd_threads_per_disk: 1,
        unknown_threads_per_disk: 1,
        ..DiskReadConfig::default()
    };
    let (registry, reporter) = reporter_for_test(&config).await;
    let reporter_id = reporter.id().to_owned();
    let reader = ScheduledFileReader::controlled_with_planned_rows_for_test(
        &config,
        1,
        FixedBlockReader,
        planned,
    )
    .unwrap()
    .0
    .with_runtime_reporter(reporter);
    let mut dispatcher = TaskFileDispatcher::new(
        TransientTaskFileSet::create(task_root.path(), Uuid::now_v7().to_string()).unwrap(),
        reader,
    );
    let identities = dispatcher
        .append_batch(
            &task_lane,
            &rows.into_iter().map(hash_record).collect::<Vec<_>>(),
        )
        .unwrap();
    dispatcher.seal().unwrap();
    let before = fs::read(dispatcher.lane_path(&task_lane).unwrap()).unwrap();
    let cancellation = ReadCancellationToken::new();
    let first = dispatcher
        .next(cancellation.clone())
        .await
        .unwrap()
        .unwrap();
    assert_eq!(first.identity, identities[0]);

    let mut context = std::task::Context::from_waker(std::task::Waker::noop());
    assert!(
        dispatcher
            .poll_next(&cancellation, &mut context)
            .is_pending()
    );
    let waiting = registry.details(&reporter_id).await.unwrap();
    assert_eq!(
        waiting
            .pipeline_metrics
            .unwrap()
            .disk_reads
            .iter()
            .find(|item| item.physical_disk_id == "PhysicalDisk13")
            .map(|item| item.hash_waiting),
        Some(Some(1))
    );

    cancellation.cancel();
    assert!(matches!(
        dispatcher.poll_next(&cancellation, &mut context),
        std::task::Poll::Ready(Err(_))
    ));
    assert_eq!(
        fs::read(dispatcher.lane_path(&task_lane).unwrap()).unwrap(),
        before,
        "取消等待不能改写 TSV 的 P 状态"
    );
    let released_wait = registry.details(&reporter_id).await.unwrap();
    assert_eq!(
        released_wait
            .pipeline_metrics
            .unwrap()
            .disk_reads
            .iter()
            .find(|item| item.physical_disk_id == "PhysicalDisk13")
            .map(|item| item.hash_waiting),
        Some(Some(0))
    );
    dispatcher.mark_failed(&first.identity).unwrap();
    drop(first);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn scheduled_reader_provider_uses_the_task_lane_not_path_resolution() {
    let task_root = tempfile::tempdir().unwrap();
    let media_root = tempfile::tempdir().unwrap();
    let path = media_root.path().join("frozen-lane.bin");
    let content = b"frozen lane provider";
    fs::write(&path, content).unwrap();
    let scanned = scanned(&path);
    let reader_lane = lane(&[17], LocalDiskKind::Hdd, 1);
    let task_lane = lane(&[18], LocalDiskKind::Hdd, 1);
    let planned = Arc::new(vec![PlannedScannedPath {
        scanned: scanned.clone(),
        lane: reader_lane,
    }]);
    let config = DiskReadConfig {
        total_threads: 1,
        hdd_threads_per_disk: 1,
        ssd_threads_per_disk: 1,
        unknown_threads_per_disk: 1,
        ..DiskReadConfig::default()
    };
    let (registry, reporter) = reporter_for_test(&config).await;
    let reporter_id = reporter.id().to_owned();
    let reader = ScheduledFileReader::controlled_with_planned_rows_for_test(
        &config,
        1,
        FixedBlockReader,
        planned,
    )
    .unwrap()
    .0
    .with_runtime_reporter(reporter);
    let mut dispatcher = TaskFileDispatcher::new(
        TransientTaskFileSet::create(task_root.path(), Uuid::now_v7().to_string()).unwrap(),
        reader.clone(),
    );
    dispatcher
        .append_batch(&task_lane, &[hash_record(scanned.clone())])
        .unwrap();
    dispatcher.seal().unwrap();
    let dispatched = dispatcher
        .next(ReadCancellationToken::new())
        .await
        .unwrap()
        .unwrap();
    let product = reader
        .read_with_permit(
            scanned,
            dispatched.permit,
            ReadCancellationToken::new(),
            None,
        )
        .await
        .unwrap();
    assert_eq!(product.md5, md5_bytes(content));
    drop(product);
    let details = registry.details(&reporter_id).await.unwrap();
    assert!(
        details
            .pipeline_metrics
            .unwrap()
            .disk_reads
            .iter()
            .any(|item| item.physical_disk_id == "PhysicalDisk18")
    );
    dispatcher.mark_completed(&dispatched.identity).unwrap();
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn scheduled_reader_old_read_api_still_acquires_and_returns_hash() {
    let media_root = tempfile::tempdir().unwrap();
    let path = media_root.path().join("legacy-read.bin");
    let content = b"legacy reader API";
    fs::write(&path, content).unwrap();
    let scanned = scanned(&path);
    let task_lane = lane(&[19], LocalDiskKind::Unknown, 1);
    let reader = ScheduledFileReader::controlled_with_planned_rows_for_test(
        &DiskReadConfig {
            total_threads: 1,
            hdd_threads_per_disk: 1,
            ssd_threads_per_disk: 1,
            unknown_threads_per_disk: 1,
            ..DiskReadConfig::default()
        },
        1,
        FixedBlockReader,
        Arc::new(vec![PlannedScannedPath {
            scanned: scanned.clone(),
            lane: task_lane,
        }]),
    )
    .unwrap()
    .0;
    let product = reader
        .read(scanned, ReadCancellationToken::new())
        .await
        .unwrap();
    assert_eq!(product.md5, md5_bytes(content));
    drop(product);
}
