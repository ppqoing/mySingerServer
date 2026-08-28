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
};

use dedup_core::{DisplayPath, NormalizedPath};
use dedup_node_engine::{
    io::{DiskReadClass, DiskReadScheduler, ReadFailure},
    scan::TaskDiskLane,
    task_dispatch::{
        DispatchedTask, SchedulerTaskLanePermitProvider, TaskDispatchError, TaskFileDispatcher,
        TaskLanePermitProvider,
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

fn poll_once<P: TaskLanePermitProvider>(
    dispatcher: &mut TaskFileDispatcher<P>,
    cancellation: &ReadCancellationToken,
) -> Poll<Result<Option<DispatchedTask<P::Permit>>, TaskDispatchError>> {
    let mut context = Context::from_waker(Waker::noop());
    dispatcher.poll_next(cancellation, &mut context)
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
