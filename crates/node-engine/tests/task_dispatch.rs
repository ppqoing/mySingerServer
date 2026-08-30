//! 瞬态任务文件 dispatcher 的队首、许可和终态行为测试。

use std::{
    future::Future,
    io::{self, Seek, SeekFrom, Write},
    ops::{Deref, DerefMut},
    pin::Pin,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    task::{Context, Poll, Waker},
    time::Duration,
};

use dedup_core::{DisplayPath, NormalizedPath};
use dedup_node_engine::{
    io::{DiskReadClass, DiskReadScheduler, ReadFailure},
    scan::TaskDiskLane,
    task_dispatch::{
        DispatchedTask, SchedulerTaskLanePermitProvider, TaskDispatchAdmission, TaskDispatchError,
        TaskDispatchPoll, TaskFileDispatcher, TaskLanePermitProvider,
    },
    task_files::{TaskFileRecord, TaskWorkKind, TaskWorkMask, TransientTaskFileSet},
};
use dedup_node_store::ScannedPath;
use dedup_windows::{LocalDiskKind, PhysicalDiskId, ReadCancellationToken};
use uuid::Uuid;

#[derive(Clone, Debug)]
struct FakePermit {
    lane: String,
    sequence: usize,
    active: Arc<AtomicUsize>,
}

#[tokio::test]
async fn scheduler_provider_uses_the_same_frozen_lane_without_a_second_acquire() {
    let config = dedup_core::DiskReadConfig::default();
    let scheduler = DiskReadScheduler::new(&config, 1).unwrap();
    let provider = SchedulerTaskLanePermitProvider::new(scheduler.clone());
    let mut dispatcher = new_dispatcher(provider);
    let task_lane = lane(&[19], LocalDiskKind::Hdd, 1, 1);
    let row = base_record("scheduler.bin", Some([0x33; 16]));
    dispatcher.register_lane(&task_lane).unwrap();
    dispatcher
        .append_batch(&task_lane, std::slice::from_ref(&row))
        .unwrap();
    dispatcher.seal().unwrap();

    let task = dispatcher
        .next(ReadCancellationToken::new())
        .await
        .unwrap()
        .expect("真实 scheduler 应授予单个 lane");
    assert_eq!(
        task.identity.lane_file_name(),
        "PhysicalDisk19-hdd.tasks.tsv"
    );
    assert_eq!(task.permit.physical_disk_id(), "PhysicalDisk19");
    dispatcher.mark_completed(&task.identity).unwrap();
    drop(task);
}

impl Drop for FakePermit {
    fn drop(&mut self) {
        self.active.fetch_sub(1, Ordering::AcqRel);
    }
}

#[derive(Clone, Default)]
struct FakeProvider {
    state: Arc<Mutex<FakeState>>,
    release: Arc<tokio::sync::Notify>,
    active: Arc<AtomicUsize>,
}

#[derive(Default)]
struct FakeState {
    started: Vec<(String, DiskReadClass)>,
    blocked: bool,
    fail_next: bool,
}

impl FakeProvider {
    fn set_blocked(&self, blocked: bool) {
        self.state.lock().unwrap().blocked = blocked;
    }

    fn fail_next(&self) {
        self.state.lock().unwrap().fail_next = true;
    }

    fn started(&self) -> Vec<(String, DiskReadClass)> {
        self.state.lock().unwrap().started.clone()
    }

    fn release_all(&self) {
        self.release.notify_waiters();
    }

    fn active_permits(&self) -> usize {
        self.active.load(Ordering::Acquire)
    }
}

impl TaskLanePermitProvider for FakeProvider {
    type Permit = FakePermit;

    fn acquire(
        &self,
        lane: TaskDiskLane,
        class: DiskReadClass,
        cancellation: ReadCancellationToken,
    ) -> Pin<Box<dyn Future<Output = Result<Self::Permit, ReadFailure>> + Send>> {
        let lane_name = format!(
            "PhysicalDisk{}",
            lane.physical_disk_numbers
                .iter()
                .map(u32::to_string)
                .collect::<Vec<_>>()
                .join("+")
        );
        let (blocked, fail, sequence) = {
            let mut state = self.state.lock().unwrap();
            let fail = state.fail_next;
            state.fail_next = false;
            let sequence = state.started.len();
            state.started.push((lane_name.clone(), class));
            (state.blocked, fail, sequence)
        };
        let release = self.release.clone();
        let active = self.active.clone();
        Box::pin(async move {
            if blocked {
                release.notified().await;
            }
            if cancellation.is_cancelled() {
                return Err(ReadFailure::Cancelled);
            }
            if fail {
                return Err(ReadFailure::Cancelled);
            }
            active.fetch_add(1, Ordering::AcqRel);
            Ok(FakePermit {
                lane: lane_name,
                sequence,
                active,
            })
        })
    }
}

fn lane(numbers: &[u32], kind: LocalDiskKind, limit: usize, weight: usize) -> TaskDiskLane {
    let physical_disk_id = PhysicalDiskId::from_disk_numbers(numbers.iter().copied()).unwrap();
    TaskDiskLane {
        physical_disk_numbers: physical_disk_id.disk_numbers().to_vec(),
        physical_disk_id,
        disk_kind: kind,
        configured_weight: weight,
        per_disk_limit: limit,
    }
}

fn scanned(name: &str, size: u64) -> ScannedPath {
    let path = format!(r"C:\media\{name}");
    ScannedPath::new(
        NormalizedPath::new(&path).unwrap(),
        DisplayPath::new(&path).unwrap(),
        size,
    )
}

fn base_record(name: &str, known_md5: Option<[u8; 16]>) -> TaskFileRecord {
    let missing = if known_md5.is_some() {
        TaskWorkMask::for_base(false, 1).unwrap()
    } else {
        TaskWorkMask::for_base(true, 0).unwrap()
    };
    TaskFileRecord {
        item_id: Uuid::now_v7(),
        work_kind: TaskWorkKind::Base,
        scanned: scanned(name, 42),
        known_md5,
        missing,
    }
}

/// 将 Hash 阶段原始记录派生为仍需媒体字段的内存记录。
fn derived_media_record(record: &TaskFileRecord) -> TaskFileRecord {
    let mut derived = record.clone();
    derived.known_md5 = Some([0x5a; 16]);
    derived.missing = TaskWorkMask::for_base(false, 1).unwrap();
    derived
}

fn poll_once<P: TaskLanePermitProvider>(
    dispatcher: &mut TaskFileDispatcher<P>,
    cancellation: &ReadCancellationToken,
) -> Poll<Result<Option<DispatchedTask<P::Permit>>, TaskDispatchError>> {
    let mut context = Context::from_waker(Waker::noop());
    dispatcher.poll_next(cancellation, &mut context)
}

/// 解包一次已经完成的 dispatcher 结果，失败时保留实际 Poll 诊断。
fn ready_task<Permit>(
    result: Poll<Result<Option<DispatchedTask<Permit>>, TaskDispatchError>>,
) -> DispatchedTask<Permit> {
    match result {
        Poll::Ready(Ok(Some(task))) => task,
        other => panic!("任务应已取得许可，实际为 {other:?}"),
    }
}

fn poll_with_admission<P: TaskLanePermitProvider>(
    dispatcher: &mut TaskFileDispatcher<P>,
    cancellation: &ReadCancellationToken,
    admission: TaskDispatchAdmission,
) -> Poll<Result<TaskDispatchPoll<P::Permit>, TaskDispatchError>> {
    let mut context = Context::from_waker(Waker::noop());
    dispatcher.poll_next_with_admission(cancellation, admission, &mut context)
}

struct TestDispatcher<P: TaskLanePermitProvider> {
    inner: TaskFileDispatcher<P>,
    _root: tempfile::TempDir,
}

impl<P: TaskLanePermitProvider> Deref for TestDispatcher<P> {
    type Target = TaskFileDispatcher<P>;

    fn deref(&self) -> &Self::Target {
        &self.inner
    }
}

impl<P: TaskLanePermitProvider> DerefMut for TestDispatcher<P> {
    fn deref_mut(&mut self) -> &mut Self::Target {
        &mut self.inner
    }
}

fn new_dispatcher<P: TaskLanePermitProvider>(provider: P) -> TestDispatcher<P> {
    let root = tempfile::tempdir().unwrap();
    let files = TransientTaskFileSet::create(root.path(), Uuid::now_v7().to_string()).unwrap();
    // 测试 harness 同时持有 root 与 dispatcher，避免用 mem::forget 泄漏临时目录。
    TestDispatcher {
        inner: TaskFileDispatcher::new(files, provider),
        _root: root,
    }
}

#[test]
fn dispatcher_keeps_one_outstanding_head_per_lane_and_does_not_take_before_permit() {
    let provider = FakeProvider::default();
    provider.set_blocked(true);
    let mut dispatcher = new_dispatcher(provider.clone());
    let first_lane = lane(&[7], LocalDiskKind::Hdd, 1, 1);
    let second_lane = lane(&[8], LocalDiskKind::Ssd, 2, 5);
    dispatcher.register_lane(&first_lane).unwrap();
    dispatcher.register_lane(&second_lane).unwrap();
    dispatcher
        .append_batch(
            &first_lane,
            std::slice::from_ref(&base_record("first.bin", None)),
        )
        .unwrap();
    dispatcher
        .append_batch(
            &second_lane,
            std::slice::from_ref(&base_record("second.bin", None)),
        )
        .unwrap();

    let cancellation = ReadCancellationToken::new();
    assert!(poll_once(&mut dispatcher, &cancellation).is_pending());
    assert_eq!(provider.started().len(), 2);
    assert_eq!(provider.started()[0].1, DiskReadClass::HashSequential);
    assert_eq!(provider.started()[1].1, DiskReadClass::HashSequential);
    assert_eq!(
        std::fs::read(dispatcher.lane_path(&first_lane).unwrap()).unwrap()[0],
        b'P'
    );
    provider.release_all();
    let dispatched = match poll_once(&mut dispatcher, &cancellation) {
        Poll::Ready(Ok(Some(task))) => task,
        other => panic!("许可释放后应交付一个任务，实际为 {other:?}"),
    };
    assert_eq!(dispatched.identity.item_id(), dispatched.record.item_id);
}

#[test]
fn provider_failure_keeps_pending_row_for_retry() {
    let provider = FakeProvider::default();
    let mut dispatcher = new_dispatcher(provider.clone());
    let task_lane = lane(&[9], LocalDiskKind::Hdd, 1, 1);
    let row = base_record("retry.bin", None);
    dispatcher.register_lane(&task_lane).unwrap();
    dispatcher
        .append_batch(&task_lane, std::slice::from_ref(&row))
        .unwrap();
    provider.fail_next();
    let cancellation = ReadCancellationToken::new();
    assert!(matches!(
        poll_once(&mut dispatcher, &cancellation),
        Poll::Ready(Err(TaskDispatchError::Read(ReadFailure::Cancelled)))
    ));
    assert_eq!(
        std::fs::read(dispatcher.lane_path(&task_lane).unwrap()).unwrap()[0],
        b'P'
    );

    let dispatched = match poll_once(&mut dispatcher, &cancellation) {
        Poll::Ready(Ok(Some(task))) => task,
        other => panic!("失败后队首应可重试，实际为 {other:?}"),
    };
    dispatcher.mark_failed(&dispatched.identity).unwrap();
}

#[test]
fn unsealed_empty_lane_waits_and_sealed_empty_lane_finishes() {
    let provider = FakeProvider::default();
    let mut dispatcher = new_dispatcher(provider);
    let task_lane = lane(&[10], LocalDiskKind::Hdd, 1, 1);
    dispatcher.register_lane(&task_lane).unwrap();
    let cancellation = ReadCancellationToken::new();
    assert!(poll_once(&mut dispatcher, &cancellation).is_pending());
    dispatcher.seal().unwrap();
    assert!(matches!(
        poll_once(&mut dispatcher, &cancellation),
        Poll::Ready(Ok(None))
    ));
}

#[test]
fn completed_task_is_returned_once_and_then_dispatcher_finishes() {
    let provider = FakeProvider::default();
    let mut dispatcher = new_dispatcher(provider);
    let task_lane = lane(&[11, 12], LocalDiskKind::Unknown, 2, 2);
    let row = base_record("composite.bin", Some([0x11; 16]));
    dispatcher.register_lane(&task_lane).unwrap();
    dispatcher
        .append_batch(&task_lane, std::slice::from_ref(&row))
        .unwrap();
    dispatcher.seal().unwrap();
    let cancellation = ReadCancellationToken::new();
    let task = match poll_once(&mut dispatcher, &cancellation) {
        Poll::Ready(Ok(Some(task))) => task,
        other => panic!("已发布任务应可派发，实际为 {other:?}"),
    };
    assert_eq!(task.class, DiskReadClass::MediaDecode);
    assert_eq!(
        task.identity.lane_file_name(),
        "PhysicalDisk11+12-unknown.tasks.tsv"
    );
    assert_eq!(task.permit.lane, "PhysicalDisk11+12");
    assert_eq!(task.permit.sequence, 0);
    dispatcher.mark_completed(&task.identity).unwrap();
    assert!(matches!(
        poll_once(&mut dispatcher, &cancellation),
        Poll::Ready(Ok(None))
    ));
}

#[test]
fn one_lane_dispatches_up_to_configured_limit_before_any_ack() {
    let provider = FakeProvider::default();
    let mut dispatcher = new_dispatcher(provider.clone());
    let task_lane = lane(&[22], LocalDiskKind::Ssd, 2, 2);
    let rows = [
        base_record("parallel-first.bin", None),
        base_record("parallel-second.bin", None),
        base_record("parallel-third.bin", None),
    ];
    dispatcher.register_lane(&task_lane).unwrap();
    dispatcher.append_batch(&task_lane, &rows).unwrap();
    dispatcher.seal().unwrap();
    let cancellation = ReadCancellationToken::new();

    let first = ready_task(poll_once(&mut dispatcher, &cancellation));
    let second = ready_task(poll_once(&mut dispatcher, &cancellation));
    assert_ne!(first.identity, second.identity);
    assert_eq!(provider.active_permits(), 2);
    assert!(
        poll_once(&mut dispatcher, &cancellation).is_pending(),
        "额度为 2 时第三项必须等待身份窗口释放"
    );

    dispatcher.mark_completed(&second.identity).unwrap();
    drop(second);
    let third = ready_task(poll_once(&mut dispatcher, &cancellation));
    assert_eq!(third.record.item_id, rows[2].item_id);
    dispatcher.mark_completed(&first.identity).unwrap();
    dispatcher.mark_completed(&third.identity).unwrap();
    drop(first);
    drop(third);
    assert!(matches!(
        poll_once(&mut dispatcher, &cancellation),
        Poll::Ready(Ok(None))
    ));
    dispatcher.discard().unwrap();
}

#[test]
fn out_of_order_ack_releases_only_matching_same_lane_identity() {
    let provider = FakeProvider::default();
    let mut dispatcher = new_dispatcher(provider);
    let task_lane = lane(&[24], LocalDiskKind::Ssd, 3, 3);
    let rows = [
        base_record("ack-first.bin", Some([0x31; 16])),
        base_record("ack-second.bin", Some([0x32; 16])),
        base_record("ack-third.bin", Some([0x33; 16])),
    ];
    dispatcher.register_lane(&task_lane).unwrap();
    dispatcher.append_batch(&task_lane, &rows).unwrap();
    dispatcher.seal().unwrap();
    let lane_path = dispatcher.lane_path(&task_lane).unwrap();
    let cancellation = ReadCancellationToken::new();

    let first = ready_task(poll_once(&mut dispatcher, &cancellation));
    let second = ready_task(poll_once(&mut dispatcher, &cancellation));
    let third = ready_task(poll_once(&mut dispatcher, &cancellation));

    dispatcher.mark_completed(&second.identity).unwrap();
    let after_second = std::fs::read(&lane_path).unwrap();
    assert_eq!(after_second[first.identity.line_offset() as usize], b'P');
    assert_eq!(after_second[second.identity.line_offset() as usize], b'C');
    assert_eq!(after_second[third.identity.line_offset() as usize], b'P');

    dispatcher.mark_failed(&first.identity).unwrap();
    dispatcher.mark_completed(&third.identity).unwrap();
    let terminal = std::fs::read(&lane_path).unwrap();
    assert_eq!(terminal[first.identity.line_offset() as usize], b'F');
    assert_eq!(terminal[second.identity.line_offset() as usize], b'C');
    assert_eq!(terminal[third.identity.line_offset() as usize], b'C');
    drop(first);
    drop(second);
    drop(third);
    assert!(matches!(
        poll_once(&mut dispatcher, &cancellation),
        Poll::Ready(Ok(None))
    ));
    dispatcher.discard().unwrap();
}

#[test]
fn hdd_lane_with_limit_one_remains_serial() {
    let provider = FakeProvider::default();
    let mut dispatcher = new_dispatcher(provider.clone());
    let task_lane = lane(&[23], LocalDiskKind::Hdd, 1, 1);
    let rows = [
        base_record("hdd-first.bin", Some([0x41; 16])),
        base_record("hdd-second.bin", Some([0x42; 16])),
    ];
    dispatcher.register_lane(&task_lane).unwrap();
    dispatcher.append_batch(&task_lane, &rows).unwrap();
    dispatcher.seal().unwrap();
    let cancellation = ReadCancellationToken::new();

    let first = ready_task(poll_once(&mut dispatcher, &cancellation));
    assert!(
        poll_once(&mut dispatcher, &cancellation).is_pending(),
        "HDD 额度为 1 时第二身份必须等待首项 ACK"
    );
    assert_eq!(provider.active_permits(), 1);

    dispatcher.mark_completed(&first.identity).unwrap();
    drop(first);
    let second = ready_task(poll_once(&mut dispatcher, &cancellation));
    assert_eq!(second.record.item_id, rows[1].item_id);
    dispatcher.mark_completed(&second.identity).unwrap();
    drop(second);
    assert!(matches!(
        poll_once(&mut dispatcher, &cancellation),
        Poll::Ready(Ok(None))
    ));
    dispatcher.discard().unwrap();
}

#[tokio::test]
async fn scheduler_holds_sixth_independent_lane_on_shared_physical_disk() {
    let mut config = dedup_core::DiskReadConfig::default();
    config.hdd_threads_per_disk = 5;
    config.total_threads = 5;
    let scheduler = DiskReadScheduler::new(&config, 5).unwrap();
    let mut dispatchers = (0..6)
        .map(|index| {
            let provider = SchedulerTaskLanePermitProvider::new(scheduler.clone());
            let mut dispatcher = new_dispatcher(provider);
            let task_lane = lane(&[27], LocalDiskKind::Hdd, 5, 1);
            dispatcher.register_lane(&task_lane).unwrap();
            dispatcher
                .append_batch(
                    &task_lane,
                    std::slice::from_ref(&base_record(&format!("scheduler-{index}.bin"), None)),
                )
                .unwrap();
            dispatcher.seal().unwrap();
            dispatcher
        })
        .collect::<Vec<_>>();
    let token = ReadCancellationToken::new();
    let mut active = Vec::new();
    for dispatcher in dispatchers.iter_mut().take(5) {
        active.push(
            dispatcher
                .next(token.clone())
                .await
                .unwrap()
                .expect("前五项应由真实 scheduler 授予许可"),
        );
    }
    assert_eq!(active.len(), 5);

    let waited = tokio::time::timeout(
        Duration::from_millis(100),
        dispatchers[5].next(token.clone()),
    )
    .await;
    assert!(
        waited.is_err(),
        "共享物理盘的第六个独立 lane 必须等待真实 scheduler 额度"
    );

    // 超时只会丢掉本次 poll；用取消令牌让 dispatcher 丢弃内部等待 future，保持 P。
    token.cancel();
    assert!(matches!(
        dispatchers[5].next(token.clone()).await,
        Err(TaskDispatchError::Read(ReadFailure::Cancelled))
    ));
    for (dispatcher, task) in dispatchers.iter_mut().take(5).zip(&active) {
        dispatcher.mark_completed(&task.identity).unwrap();
    }
    drop(active);

    let sixth = dispatchers[5]
        .next(ReadCancellationToken::new())
        .await
        .unwrap()
        .expect("前五项释放后第六项应继续派发");
    assert!(!sixth.is_continuation());
    dispatchers[5].mark_completed(&sixth.identity).unwrap();
    drop(sixth);
}

#[test]
fn base_without_md5_uses_hash_and_never_fabricates_second_task_line() {
    let provider = FakeProvider::default();
    let mut dispatcher = new_dispatcher(provider.clone());
    let task_lane = lane(&[13], LocalDiskKind::Hdd, 1, 1);
    let row = base_record("hash.bin", None);
    dispatcher.register_lane(&task_lane).unwrap();
    dispatcher
        .append_batch(&task_lane, std::slice::from_ref(&row))
        .unwrap();
    dispatcher.seal().unwrap();
    let cancellation = ReadCancellationToken::new();
    let task = match poll_once(&mut dispatcher, &cancellation) {
        Poll::Ready(Ok(Some(task))) => task,
        other => panic!("未知 MD5 的基础任务应派发，实际为 {other:?}"),
    };
    assert_eq!(task.class, DiskReadClass::HashSequential);
    dispatcher.mark_completed(&task.identity).unwrap();
    assert_eq!(provider.started().len(), 1);
    assert!(matches!(
        poll_once(&mut dispatcher, &cancellation),
        Poll::Ready(Ok(None))
    ));
}

#[test]
fn hash_task_can_continue_as_media_with_same_identity_and_one_tsv_row() {
    let provider = FakeProvider::default();
    let mut dispatcher = new_dispatcher(provider.clone());
    let task_lane = lane(&[23], LocalDiskKind::Hdd, 1, 1);
    let row = base_record("hash-to-media.bin", None);
    dispatcher.register_lane(&task_lane).unwrap();
    dispatcher
        .append_batch(&task_lane, std::slice::from_ref(&row))
        .unwrap();
    dispatcher.seal().unwrap();
    let cancellation = ReadCancellationToken::new();
    let hash = match poll_once(&mut dispatcher, &cancellation) {
        Poll::Ready(Ok(Some(task))) => task,
        other => panic!("Hash 任务应先取得许可，实际为 {other:?}"),
    };
    assert_eq!(hash.class, DiskReadClass::HashSequential);
    let identity = hash.identity.clone();
    let record = derived_media_record(&hash.record);
    drop(hash);

    dispatcher
        .request_media_continuation(&identity, &record)
        .unwrap();
    assert!(
        dispatcher
            .request_media_continuation(&identity, &record)
            .is_err()
    );
    assert!(
        dispatcher.mark_completed(&identity).is_err(),
        "续算许可尚未交付时不能提前写入终态"
    );
    let media = match poll_once(&mut dispatcher, &cancellation) {
        Poll::Ready(Ok(Some(task))) => task,
        other => panic!("Hash 后应取得同身份 Media 许可，实际为 {other:?}"),
    };
    assert_eq!(media.class, DiskReadClass::MediaDecode);
    assert!(media.is_continuation());
    assert_eq!(media.identity, identity);
    assert_eq!(media.record, record);
    assert_eq!(provider.started().len(), 2);
    assert_eq!(provider.started()[0].1, DiskReadClass::HashSequential);
    assert_eq!(provider.started()[1].1, DiskReadClass::MediaDecode);
    let bytes = std::fs::read(dispatcher.lane_path(&task_lane).unwrap()).unwrap();
    assert_eq!(bytes.iter().filter(|byte| **byte == b'\n').count(), 1);
    dispatcher.mark_completed(&media.identity).unwrap();
    drop(media);
}

#[test]
fn same_lane_continuation_reuses_identity_window_slot() {
    let provider = FakeProvider::default();
    let mut dispatcher = new_dispatcher(provider.clone());
    let task_lane = lane(&[28], LocalDiskKind::Ssd, 2, 2);
    let rows = [
        base_record("window-hash.bin", None),
        base_record("window-media.bin", Some([0x61; 16])),
        base_record("window-third.bin", Some([0x62; 16])),
    ];
    dispatcher.register_lane(&task_lane).unwrap();
    dispatcher.append_batch(&task_lane, &rows).unwrap();
    dispatcher.seal().unwrap();
    let lane_path = dispatcher.lane_path(&task_lane).unwrap();
    let token = ReadCancellationToken::new();

    let hash = ready_task(poll_once(&mut dispatcher, &token));
    let hash_identity = hash.identity.clone();
    let continuation_record = derived_media_record(&hash.record);
    let second = ready_task(poll_once(&mut dispatcher, &token));
    assert_ne!(hash_identity, second.identity);
    drop(hash);

    dispatcher
        .request_media_continuation(&hash_identity, &continuation_record)
        .unwrap();
    let continuation = ready_task(poll_once(&mut dispatcher, &token));
    assert!(continuation.is_continuation());
    assert_eq!(continuation.identity, hash_identity);
    assert_eq!(provider.started().len(), 3);

    let before_ack = std::fs::read(&lane_path).unwrap();
    assert_eq!(before_ack.iter().filter(|byte| **byte == b'\n').count(), 3);
    assert_eq!(before_ack[hash_identity.line_offset() as usize], b'P');
    assert_eq!(before_ack[second.identity.line_offset() as usize], b'P');
    assert!(
        poll_once(&mut dispatcher, &token).is_pending(),
        "续算复用原身份后，第三个普通身份仍须等待窗口释放"
    );

    dispatcher.mark_completed(&continuation.identity).unwrap();
    drop(continuation);
    let third = ready_task(poll_once(&mut dispatcher, &token));
    assert_eq!(third.record.item_id, rows[2].item_id);
    dispatcher.mark_completed(&second.identity).unwrap();
    dispatcher.mark_completed(&third.identity).unwrap();
    drop(second);
    drop(third);
    assert!(matches!(
        poll_once(&mut dispatcher, &token),
        Poll::Ready(Ok(None))
    ));
    dispatcher.discard().unwrap();
}

#[test]
fn same_lane_pending_request_does_not_block_other_identity_abandon() {
    let provider = FakeProvider::default();
    let mut dispatcher = new_dispatcher(provider.clone());
    let task_lane = lane(&[29], LocalDiskKind::Ssd, 2, 2);
    let rows = [
        base_record("abandon-first.bin", Some([0x71; 16])),
        base_record("abandon-hash.bin", None),
    ];
    dispatcher.register_lane(&task_lane).unwrap();
    dispatcher.append_batch(&task_lane, &rows).unwrap();
    dispatcher.seal().unwrap();
    let token = ReadCancellationToken::new();

    let first = ready_task(poll_once(&mut dispatcher, &token));
    let hash = ready_task(poll_once(&mut dispatcher, &token));
    let first_identity = first.identity.clone();
    let hash_identity = hash.identity.clone();
    let continuation_record = derived_media_record(&hash.record);
    drop(first);
    drop(hash);

    provider.set_blocked(true);
    dispatcher
        .request_media_continuation(&hash_identity, &continuation_record)
        .unwrap();
    assert!(poll_once(&mut dispatcher, &token).is_pending());

    dispatcher.abandon_in_flight(&first_identity).unwrap();
    assert!(
        dispatcher.abandon_in_flight(&hash_identity).is_err(),
        "只有仍持有 pending future 的同一身份应被拒绝"
    );
    token.cancel();
    assert!(matches!(
        poll_once(&mut dispatcher, &token),
        Poll::Ready(Err(TaskDispatchError::Read(ReadFailure::Cancelled)))
    ));
    dispatcher.abandon_in_flight(&hash_identity).unwrap();
    dispatcher.discard().unwrap();
}

#[test]
fn hash_continuation_accepts_derived_md5_and_media_mask_without_rewriting_tsv() {
    let provider = FakeProvider::default();
    let mut dispatcher = new_dispatcher(provider.clone());
    let task_lane = lane(&[27], LocalDiskKind::Hdd, 1, 1);
    let row = base_record("derived-media.bin", None);
    dispatcher.register_lane(&task_lane).unwrap();
    dispatcher
        .append_batch(&task_lane, std::slice::from_ref(&row))
        .unwrap();
    dispatcher.seal().unwrap();
    let lane_path = dispatcher.lane_path(&task_lane).unwrap();
    let original_bytes = std::fs::read(&lane_path).unwrap();
    let cancellation = ReadCancellationToken::new();
    let hash = match poll_once(&mut dispatcher, &cancellation) {
        Poll::Ready(Ok(Some(task))) => task,
        other => panic!("Hash 任务应先取得许可，实际为 {other:?}"),
    };
    assert_eq!(hash.class, DiskReadClass::HashSequential);
    let identity = hash.identity.clone();
    let original_record = hash.record.clone();
    let mut derived_record = original_record.clone();
    derived_record.known_md5 = Some([0x5a; 16]);
    derived_record.missing = TaskWorkMask::for_base(false, 0b101).unwrap();
    drop(hash);

    dispatcher
        .request_media_continuation(&identity, &derived_record)
        .unwrap();
    let media = match poll_once(&mut dispatcher, &cancellation) {
        Poll::Ready(Ok(Some(task))) => task,
        other => panic!("Hash 后应取得同身份 Media 许可，实际为 {other:?}"),
    };
    assert_eq!(media.class, DiskReadClass::MediaDecode);
    assert!(media.is_continuation());
    assert_eq!(media.identity, identity);
    assert_eq!(media.record, derived_record);
    assert_eq!(provider.started().len(), 2);
    assert_eq!(provider.started()[0].1, DiskReadClass::HashSequential);
    assert_eq!(provider.started()[1].1, DiskReadClass::MediaDecode);

    let before_complete = std::fs::read(&lane_path).unwrap();
    assert_eq!(before_complete, original_bytes);
    dispatcher.mark_completed(&media.identity).unwrap();
    drop(media);
    let mut expected_completed = original_bytes;
    expected_completed[0] = b'C';
    assert_eq!(std::fs::read(&lane_path).unwrap(), expected_completed);
    assert_eq!(original_record.known_md5, None);
}

#[test]
fn media_continuation_is_prioritized_over_the_next_same_lane_head() {
    let provider = FakeProvider::default();
    let mut dispatcher = new_dispatcher(provider);
    let task_lane = lane(&[26], LocalDiskKind::Hdd, 2, 1);
    let hash_row = base_record("priority-hash.bin", None);
    let next_row = base_record("priority-next.bin", Some([0x44; 16]));
    dispatcher.register_lane(&task_lane).unwrap();
    dispatcher
        .append_batch(&task_lane, &[hash_row, next_row])
        .unwrap();
    dispatcher.seal().unwrap();
    let token = ReadCancellationToken::new();
    let hash = match poll_once(&mut dispatcher, &token) {
        Poll::Ready(Ok(Some(task))) => task,
        other => panic!("Hash 任务应先取得许可，实际为 {other:?}"),
    };
    let identity = hash.identity.clone();
    let record = derived_media_record(&hash.record);
    drop(hash);
    dispatcher
        .request_media_continuation(&identity, &record)
        .unwrap();

    let continuation = match poll_once(&mut dispatcher, &token) {
        Poll::Ready(Ok(Some(task))) => task,
        other => panic!("续算应优先于普通队首，实际为 {other:?}"),
    };
    assert!(continuation.is_continuation());
    assert_eq!(continuation.identity, identity);
    dispatcher.mark_completed(&continuation.identity).unwrap();
    drop(continuation);

    let next = match poll_once(&mut dispatcher, &token) {
        Poll::Ready(Ok(Some(task))) => task,
        other => panic!("续算完成后普通队首应继续派发，实际为 {other:?}"),
    };
    assert!(!next.is_continuation());
    dispatcher.mark_completed(&next.identity).unwrap();
    drop(next);
}

#[test]
fn failed_media_continuation_keeps_intent_for_retry_and_cancel_keeps_pending_status() {
    let provider = FakeProvider::default();
    let mut dispatcher = new_dispatcher(provider.clone());
    let task_lane = lane(&[24], LocalDiskKind::Hdd, 1, 1);
    let row = base_record("retry-media.bin", None);
    dispatcher.register_lane(&task_lane).unwrap();
    dispatcher
        .append_batch(&task_lane, std::slice::from_ref(&row))
        .unwrap();
    dispatcher.seal().unwrap();
    let token = ReadCancellationToken::new();
    let hash = match poll_once(&mut dispatcher, &token) {
        Poll::Ready(Ok(Some(task))) => task,
        other => panic!("Hash 任务应先取得许可，实际为 {other:?}"),
    };
    let identity = hash.identity.clone();
    let record = derived_media_record(&hash.record);
    drop(hash);

    dispatcher
        .request_media_continuation(&identity, &record)
        .unwrap();
    provider.fail_next();
    assert!(matches!(
        poll_once(&mut dispatcher, &token),
        Poll::Ready(Err(TaskDispatchError::Read(ReadFailure::Cancelled)))
    ));
    assert_eq!(
        std::fs::read(dispatcher.lane_path(&task_lane).unwrap()).unwrap()[0],
        b'P'
    );

    token.cancel();
    assert!(matches!(
        poll_once(&mut dispatcher, &token),
        Poll::Ready(Err(TaskDispatchError::Read(ReadFailure::Cancelled)))
    ));
    assert_eq!(
        std::fs::read(dispatcher.lane_path(&task_lane).unwrap()).unwrap()[0],
        b'P'
    );

    let retry_token = ReadCancellationToken::new();
    let media = match poll_once(&mut dispatcher, &retry_token) {
        Poll::Ready(Ok(Some(task))) => task,
        other => panic!("失败/取消后续算意图应可重试，实际为 {other:?}"),
    };
    assert!(media.is_continuation());
    dispatcher.mark_failed(&media.identity).unwrap();
    drop(media);
}

#[test]
fn abandon_after_cancellation_allows_exact_run_discard() {
    let provider = FakeProvider::default();
    let mut dispatcher = new_dispatcher(provider.clone());
    let task_lane = lane(&[25], LocalDiskKind::Hdd, 1, 1);
    let row = base_record("cancel-abandon.bin", None);
    dispatcher.register_lane(&task_lane).unwrap();
    dispatcher
        .append_batch(&task_lane, std::slice::from_ref(&row))
        .unwrap();
    dispatcher.seal().unwrap();
    let token = ReadCancellationToken::new();
    let task = match poll_once(&mut dispatcher, &token) {
        Poll::Ready(Ok(Some(task))) => task,
        other => panic!("任务应取得许可，实际为 {other:?}"),
    };
    let identity = task.identity.clone();
    let record = derived_media_record(&task.record);
    drop(task);

    provider.set_blocked(true);
    dispatcher
        .request_media_continuation(&identity, &record)
        .unwrap();
    assert!(poll_once(&mut dispatcher, &token).is_pending());
    token.cancel();
    assert!(matches!(
        poll_once(&mut dispatcher, &token),
        Poll::Ready(Err(TaskDispatchError::Read(ReadFailure::Cancelled)))
    ));
    dispatcher.abandon_in_flight(&identity).unwrap();
    dispatcher.discard().unwrap();
}

/// 只接收一次通知的测试唤醒器，用来验证 publication 没有漏唤醒。
struct WakeFlag(Arc<AtomicBool>);

impl std::task::Wake for WakeFlag {
    fn wake(self: Arc<Self>) {
        self.0.store(true, Ordering::Release);
    }

    fn wake_by_ref(self: &Arc<Self>) {
        self.0.store(true, Ordering::Release);
    }
}

#[test]
fn unsealed_empty_lane_is_woken_by_a_later_append() {
    let provider = FakeProvider::default();
    let mut dispatcher = new_dispatcher(provider);
    let task_lane = lane(&[14], LocalDiskKind::Hdd, 1, 1);
    dispatcher.register_lane(&task_lane).unwrap();
    let cancellation = ReadCancellationToken::new();
    let woke = Arc::new(AtomicBool::new(false));
    let waker = Waker::from(Arc::new(WakeFlag(woke.clone())));
    let mut context = Context::from_waker(&waker);

    assert!(
        dispatcher
            .poll_next(&cancellation, &mut context)
            .is_pending()
    );
    dispatcher
        .append_batch(
            &task_lane,
            std::slice::from_ref(&base_record("later.bin", None)),
        )
        .unwrap();
    assert!(woke.load(Ordering::Acquire));
    let task = match dispatcher.poll_next(&cancellation, &mut context) {
        Poll::Ready(Ok(Some(task))) => task,
        other => panic!("追加后应唤醒并派发任务，实际为 {other:?}"),
    };
    dispatcher.mark_completed(&task.identity).unwrap();
}

#[test]
fn cancellation_drops_one_pending_request_and_preserves_pending_status() {
    let provider = FakeProvider::default();
    provider.set_blocked(true);
    let mut dispatcher = new_dispatcher(provider.clone());
    let task_lane = lane(&[15], LocalDiskKind::Hdd, 1, 1);
    dispatcher.register_lane(&task_lane).unwrap();
    dispatcher
        .append_batch(
            &task_lane,
            std::slice::from_ref(&base_record("cancel.bin", None)),
        )
        .unwrap();
    let cancellation = ReadCancellationToken::new();
    assert!(poll_once(&mut dispatcher, &cancellation).is_pending());
    assert_eq!(provider.started().len(), 1);
    cancellation.cancel();
    assert!(matches!(
        poll_once(&mut dispatcher, &cancellation),
        Poll::Ready(Err(TaskDispatchError::Read(ReadFailure::Cancelled)))
    ));
    assert_eq!(provider.started().len(), 1);
    assert_eq!(
        std::fs::read(dispatcher.lane_path(&task_lane).unwrap()).unwrap()[0],
        b'P'
    );

    provider.set_blocked(false);
    let retry_token = ReadCancellationToken::new();
    let task = match poll_once(&mut dispatcher, &retry_token) {
        Poll::Ready(Ok(Some(task))) => task,
        other => panic!("取消后新的 token 应可重试，实际为 {other:?}"),
    };
    dispatcher.mark_failed(&task.identity).unwrap();
}

#[test]
fn stage2_kinds_use_media_and_composite_lane_acquires_once() {
    let provider = FakeProvider::default();
    let mut dispatcher = new_dispatcher(provider.clone());
    let task_lane = lane(&[16, 17], LocalDiskKind::Unknown, 2, 2);
    let image = TaskFileRecord {
        item_id: Uuid::now_v7(),
        work_kind: TaskWorkKind::ImageStage2,
        scanned: scanned("stage2.jpg", 42),
        known_md5: Some([0x22; 16]),
        missing: TaskWorkMask::for_image_stage2(),
    };
    dispatcher.register_lane(&task_lane).unwrap();
    dispatcher
        .append_batch(&task_lane, std::slice::from_ref(&image))
        .unwrap();
    dispatcher.seal().unwrap();
    let cancellation = ReadCancellationToken::new();
    let task = match poll_once(&mut dispatcher, &cancellation) {
        Poll::Ready(Ok(Some(task))) => task,
        other => panic!("图片二筛应派发，实际为 {other:?}"),
    };
    assert_eq!(task.class, DiskReadClass::MediaDecode);
    assert_eq!(provider.started().len(), 1);
    assert_eq!(provider.started()[0].0, "PhysicalDisk16+17");
    assert_eq!(provider.active_permits(), 1);
    drop(task);
    assert_eq!(provider.active_permits(), 0);
}

#[test]
fn discard_after_waiting_is_clean_and_does_not_leak_the_temp_directory() {
    let provider = FakeProvider::default();
    let mut dispatcher = new_dispatcher(provider);
    let task_lane = lane(&[18], LocalDiskKind::Hdd, 1, 1);
    dispatcher.register_lane(&task_lane).unwrap();
    let cancellation = ReadCancellationToken::new();
    assert!(poll_once(&mut dispatcher, &cancellation).is_pending());
    dispatcher.discard().unwrap();
}

#[test]
fn discard_rejects_an_unacknowledged_dispatched_task() {
    let provider = FakeProvider::default();
    let mut dispatcher = new_dispatcher(provider);
    let task_lane = lane(&[20], LocalDiskKind::Hdd, 1, 1);
    let row = base_record("discard-in-flight.bin", None);
    dispatcher.register_lane(&task_lane).unwrap();
    dispatcher
        .append_batch(&task_lane, std::slice::from_ref(&row))
        .unwrap();
    dispatcher.seal().unwrap();

    let cancellation = ReadCancellationToken::new();
    let task = match poll_once(&mut dispatcher, &cancellation) {
        Poll::Ready(Ok(Some(task))) => task,
        other => panic!("已发布任务应可派发，实际为 {other:?}"),
    };
    let error = dispatcher
        .discard()
        .expect_err("未 ACK 的任务不能删除运行目录");
    assert_eq!(error.kind(), io::ErrorKind::InvalidInput);
    assert!(dispatcher.lane_path(&task_lane).unwrap().exists());

    dispatcher.mark_completed(&task.identity).unwrap();
    drop(task);
    dispatcher.discard().unwrap();
}

fn rewrite_task_file(path: &std::path::Path, bytes: &[u8]) {
    let mut file = std::fs::OpenOptions::new().write(true).open(path).unwrap();
    file.seek(SeekFrom::Start(0)).unwrap();
    file.write_all(bytes).unwrap();
    file.flush().unwrap();
}

#[test]
fn record_mismatch_after_permit_keeps_the_head_retryable() {
    let provider = FakeProvider::default();
    provider.set_blocked(true);
    let mut dispatcher = new_dispatcher(provider.clone());
    let task_lane = lane(&[21], LocalDiskKind::Hdd, 1, 1);
    dispatcher.register_lane(&task_lane).unwrap();
    dispatcher
        .append_batch(
            &task_lane,
            std::slice::from_ref(&base_record("mismatch.bin", None)),
        )
        .unwrap();

    let cancellation = ReadCancellationToken::new();
    assert!(poll_once(&mut dispatcher, &cancellation).is_pending());
    let path = dispatcher.lane_path(&task_lane).unwrap();
    let original = std::fs::read(&path).unwrap();
    let mut changed = original.clone();
    let marker = changed
        .windows(5)
        .position(|window| window == b"media")
        .expect("测试任务行应包含可替换的路径片段");
    changed[marker] = b'n';
    rewrite_task_file(&path, &changed);
    provider.release_all();

    assert!(matches!(
        poll_once(&mut dispatcher, &cancellation),
        Poll::Ready(Err(TaskDispatchError::File(_)))
    ));
    assert_eq!(provider.active_permits(), 0, "领取失败后许可必须自动释放");

    rewrite_task_file(&path, &original);
    provider.set_blocked(false);
    let retried = match poll_once(&mut dispatcher, &cancellation) {
        Poll::Ready(Ok(Some(task))) => task,
        other => panic!("记录恢复后队首应可重新领取，实际为 {other:?}"),
    };
    dispatcher.mark_failed(&retried.identity).unwrap();
}

#[test]
fn hash_only_admission_blocks_media_without_acquiring_a_permit() {
    let provider = FakeProvider::default();
    let mut dispatcher = new_dispatcher(provider.clone());
    let task_lane = lane(&[28], LocalDiskKind::Hdd, 1, 1);
    let row = base_record("media-only.bin", Some([0x28; 16]));
    dispatcher.register_lane(&task_lane).unwrap();
    dispatcher
        .append_batch(&task_lane, std::slice::from_ref(&row))
        .unwrap();
    dispatcher.seal().unwrap();

    let cancellation = ReadCancellationToken::new();
    assert!(matches!(
        poll_with_admission(
            &mut dispatcher,
            &cancellation,
            TaskDispatchAdmission::hash_only()
        ),
        Poll::Ready(Ok(TaskDispatchPoll::Blocked(
            dedup_node_engine::task_dispatch::TaskDispatchBlockReason::MediaPending
        )))
    ));
    assert!(
        provider.started().is_empty(),
        "Hash-only 不得申请 Media permit"
    );
    assert_eq!(
        std::fs::read(dispatcher.lane_path(&task_lane).unwrap()).unwrap()[0],
        b'P'
    );
}

#[test]
fn hash_only_admission_dispatches_a_needs_md5_row() {
    let provider = FakeProvider::default();
    let mut dispatcher = new_dispatcher(provider.clone());
    let task_lane = lane(&[29], LocalDiskKind::Hdd, 1, 1);
    dispatcher.register_lane(&task_lane).unwrap();
    dispatcher
        .append_batch(
            &task_lane,
            std::slice::from_ref(&base_record("hash-only.bin", None)),
        )
        .unwrap();
    dispatcher.seal().unwrap();

    let cancellation = ReadCancellationToken::new();
    let task = match poll_with_admission(
        &mut dispatcher,
        &cancellation,
        TaskDispatchAdmission::hash_only(),
    ) {
        Poll::Ready(Ok(TaskDispatchPoll::Task(task))) => task,
        other => panic!("Hash-only 应派发 Hash，实际为 {other:?}"),
    };
    assert_eq!(task.class, DiskReadClass::HashSequential);
    dispatcher.mark_failed(&task.identity).unwrap();
    drop(task);
}

#[test]
fn hash_only_admission_dispatches_hash_then_reports_media_pending() {
    let provider = FakeProvider::default();
    let mut dispatcher = new_dispatcher(provider.clone());
    let hash_lane = lane(&[30], LocalDiskKind::Hdd, 1, 1);
    let media_lane = lane(&[31], LocalDiskKind::Hdd, 1, 1);
    dispatcher.register_lane(&hash_lane).unwrap();
    dispatcher.register_lane(&media_lane).unwrap();
    dispatcher
        .append_batch(
            &hash_lane,
            std::slice::from_ref(&base_record("hash-first.bin", None)),
        )
        .unwrap();
    dispatcher
        .append_batch(
            &media_lane,
            std::slice::from_ref(&base_record("media-later.bin", Some([0x31; 16]))),
        )
        .unwrap();
    dispatcher.seal().unwrap();

    let cancellation = ReadCancellationToken::new();
    let hash = match poll_with_admission(
        &mut dispatcher,
        &cancellation,
        TaskDispatchAdmission::hash_only(),
    ) {
        Poll::Ready(Ok(TaskDispatchPoll::Task(task))) => task,
        other => panic!("Hash-only 首次应派发 Hash，实际为 {other:?}"),
    };
    assert_eq!(hash.class, DiskReadClass::HashSequential);
    dispatcher.mark_completed(&hash.identity).unwrap();
    drop(hash);

    assert!(matches!(
        poll_with_admission(
            &mut dispatcher,
            &cancellation,
            TaskDispatchAdmission::hash_only()
        ),
        Poll::Ready(Ok(TaskDispatchPoll::Blocked(
            dedup_node_engine::task_dispatch::TaskDispatchBlockReason::MediaPending
        )))
    ));
    assert_eq!(
        provider.started().len(),
        1,
        "不得在 Hash-only 下申请 Media permit"
    );
    assert_eq!(
        std::fs::read(dispatcher.lane_path(&media_lane).unwrap()).unwrap()[0],
        b'P'
    );
}

#[tokio::test]
async fn default_next_still_dispatches_media() {
    let provider = FakeProvider::default();
    let mut dispatcher = new_dispatcher(provider);
    let task_lane = lane(&[32], LocalDiskKind::Hdd, 1, 1);
    dispatcher.register_lane(&task_lane).unwrap();
    dispatcher
        .append_batch(
            &task_lane,
            std::slice::from_ref(&base_record("default-media.bin", Some([0x32; 16]))),
        )
        .unwrap();
    dispatcher.seal().unwrap();

    let task = dispatcher
        .next(ReadCancellationToken::new())
        .await
        .unwrap()
        .expect("默认 next 必须保留 Media 派发行为");
    assert_eq!(task.class, DiskReadClass::MediaDecode);
    dispatcher.mark_failed(&task.identity).unwrap();
    drop(task);
}

#[test]
fn switching_to_media_admission_releases_a_hash_only_block_without_losing_p() {
    let provider = FakeProvider::default();
    let mut dispatcher = new_dispatcher(provider.clone());
    let task_lane = lane(&[33], LocalDiskKind::Hdd, 1, 1);
    dispatcher.register_lane(&task_lane).unwrap();
    dispatcher
        .append_batch(
            &task_lane,
            std::slice::from_ref(&base_record("switch-media.bin", Some([0x33; 16]))),
        )
        .unwrap();
    dispatcher.seal().unwrap();
    let cancellation = ReadCancellationToken::new();

    assert!(matches!(
        poll_with_admission(
            &mut dispatcher,
            &cancellation,
            TaskDispatchAdmission::hash_only()
        ),
        Poll::Ready(Ok(TaskDispatchPoll::Blocked(_)))
    ));
    assert_eq!(
        std::fs::read(dispatcher.lane_path(&task_lane).unwrap()).unwrap()[0],
        b'P'
    );

    let media =
        match poll_with_admission(&mut dispatcher, &cancellation, TaskDispatchAdmission::all()) {
            Poll::Ready(Ok(TaskDispatchPoll::Task(task))) => task,
            other => panic!("切换为允许 Media 后应派发原 P 行，实际为 {other:?}"),
        };
    assert_eq!(media.class, DiskReadClass::MediaDecode);
    assert_eq!(provider.started().len(), 1);
    dispatcher.mark_failed(&media.identity).unwrap();
    drop(media);
}

#[test]
fn switching_admission_drops_forbidden_pending_future_and_retries_same_p_row() {
    let provider = FakeProvider::default();
    provider.set_blocked(true);
    let mut dispatcher = new_dispatcher(provider.clone());
    let task_lane = lane(&[35], LocalDiskKind::Hdd, 1, 1);
    dispatcher.register_lane(&task_lane).unwrap();
    dispatcher
        .append_batch(
            &task_lane,
            std::slice::from_ref(&base_record("switch-pending.bin", Some([0x35; 16]))),
        )
        .unwrap();
    dispatcher.seal().unwrap();
    let cancellation = ReadCancellationToken::new();

    assert!(
        poll_with_admission(&mut dispatcher, &cancellation, TaskDispatchAdmission::all())
            .is_pending()
    );
    assert_eq!(provider.started().len(), 1);

    assert!(matches!(
        poll_with_admission(
            &mut dispatcher,
            &cancellation,
            TaskDispatchAdmission::hash_only()
        ),
        Poll::Ready(Ok(TaskDispatchPoll::Blocked(
            dedup_node_engine::task_dispatch::TaskDispatchBlockReason::MediaPending
        )))
    ));
    assert_eq!(
        provider.started().len(),
        1,
        "切换 admission 不得重复启动 Media future"
    );
    assert_eq!(
        std::fs::read(dispatcher.lane_path(&task_lane).unwrap()).unwrap()[0],
        b'P'
    );

    provider.set_blocked(false);
    let media =
        match poll_with_admission(&mut dispatcher, &cancellation, TaskDispatchAdmission::all()) {
            Poll::Ready(Ok(TaskDispatchPoll::Task(task))) => task,
            other => panic!("切回允许 Media 后应重新申请同一 P 行，实际为 {other:?}"),
        };
    assert_eq!(provider.started().len(), 2);
    dispatcher.mark_failed(&media.identity).unwrap();
    drop(media);
}

#[test]
fn hash_only_admission_cancellation_keeps_pending_row_as_p() {
    let provider = FakeProvider::default();
    provider.set_blocked(true);
    let mut dispatcher = new_dispatcher(provider.clone());
    let task_lane = lane(&[34], LocalDiskKind::Hdd, 1, 1);
    dispatcher.register_lane(&task_lane).unwrap();
    dispatcher
        .append_batch(
            &task_lane,
            std::slice::from_ref(&base_record("cancel-hash.bin", None)),
        )
        .unwrap();
    dispatcher.seal().unwrap();
    let cancellation = ReadCancellationToken::new();
    assert!(
        poll_with_admission(
            &mut dispatcher,
            &cancellation,
            TaskDispatchAdmission::hash_only()
        )
        .is_pending()
    );
    cancellation.cancel();
    assert!(matches!(
        poll_with_admission(
            &mut dispatcher,
            &cancellation,
            TaskDispatchAdmission::hash_only()
        ),
        Poll::Ready(Err(TaskDispatchError::Read(ReadFailure::Cancelled)))
    ));
    assert_eq!(
        std::fs::read(dispatcher.lane_path(&task_lane).unwrap()).unwrap()[0],
        b'P'
    );
    assert_eq!(provider.started().len(), 1);
}

#[test]
fn allowed_pending_media_prevents_false_hash_admission_block() {
    let provider = FakeProvider::default();
    provider.set_blocked(true);
    let mut dispatcher = new_dispatcher(provider.clone());
    let media_lane = lane(&[36], LocalDiskKind::Hdd, 1, 1);
    let hash_lane = lane(&[37], LocalDiskKind::Hdd, 1, 1);
    dispatcher.register_lane(&media_lane).unwrap();
    dispatcher.register_lane(&hash_lane).unwrap();
    dispatcher
        .append_batch(
            &media_lane,
            std::slice::from_ref(&base_record("admitted-media.bin", Some([0x36; 16]))),
        )
        .unwrap();
    dispatcher
        .append_batch(
            &hash_lane,
            std::slice::from_ref(&base_record("blocked-hash.bin", None)),
        )
        .unwrap();
    dispatcher.seal().unwrap();
    let cancellation = ReadCancellationToken::new();

    assert!(
        poll_with_admission(
            &mut dispatcher,
            &cancellation,
            TaskDispatchAdmission::media_only()
        )
        .is_pending(),
        "允许的 Media future 尚未完成时必须继续等待，不能误报 HashPending"
    );
    assert_eq!(
        provider.started().len(),
        1,
        "禁止 Hash 不得申请 provider future"
    );

    provider.release_all();
    let media = match poll_with_admission(
        &mut dispatcher,
        &cancellation,
        TaskDispatchAdmission::media_only(),
    ) {
        Poll::Ready(Ok(TaskDispatchPoll::Task(task))) => task,
        other => panic!("允许的 Media future 完成后应正常派发，实际为 {other:?}"),
    };
    assert_eq!(media.class, DiskReadClass::MediaDecode);
    dispatcher.mark_failed(&media.identity).unwrap();
    drop(media);
}
