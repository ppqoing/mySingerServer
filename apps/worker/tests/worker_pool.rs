//! 真实 worker.exe 的 Ready、任务、重启、崩溃替换和取消进程级测试。

use std::{
    env,
    path::{Path, PathBuf},
    time::Duration,
};

use dedup_node_engine::worker::{
    WorkerEvent, WorkerLaunch, WorkerPool, WorkerPoolConfig, decode_stage1_payload,
};
use dedup_protocol::proto::{self, worker_envelope};

#[tokio::test]
/// 用真实 DLL 和 worker.exe 覆盖正常结果及三类进程生命周期分支。
async fn real_worker_process_supports_results_restart_crash_and_cancel() {
    let Some(runtime) = runtime_fixture() else {
        return;
    };
    let worker = runtime.path().join("worker.exe");
    let config = WorkerPoolConfig::new(WorkerLaunch::new(worker), 1)
        .with_result_read_delay(Duration::from_millis(500));
    let mut pool = WorkerPool::start(config).await.unwrap();

    pool.dispatch(stage1_request("task-result", "item-result"))
        .await
        .unwrap();
    let completed = next_event(&mut pool).await;
    let WorkerEvent::Completed { response, .. } = completed else {
        panic!("expected completed event");
    };
    let Some(worker_envelope::Payload::Stage1Result(result)) = response.payload else {
        panic!("expected stage-one result");
    };
    assert_eq!(
        decode_stage1_payload(&result.payload).unwrap().frames.len(),
        1
    );

    let first_pid = pool.worker_process_ids()[0];
    pool.dispatch(contact_request("task-restart", "item-restart"))
        .await
        .unwrap();
    assert_eq!(pool.busy_workers(), 1);
    let running = pool.prepare_planned_restart().await.unwrap();
    assert_eq!(running, vec!["item-restart"]);
    let mut store = FakeStore::default();
    store.requeue_items(&running);
    pool.restart_after_requeue(&running).await.unwrap();
    assert_eq!(store.queued, running);
    assert_eq!(pool.failure_count(), 0);
    assert_ne!(pool.worker_process_ids()[0], first_pid);

    let crash_pid = pool.worker_process_ids()[0];
    pool.dispatch(contact_request("task-crash", "item-crash"))
        .await
        .unwrap();
    pool.terminate_worker_for_test(crash_pid).await.unwrap();
    let crashed = next_event(&mut pool).await;
    assert!(matches!(
        crashed,
        WorkerEvent::Crashed { ref item_id, .. } if item_id == "item-crash"
    ));
    assert_eq!(pool.failure_count(), 1);
    wait_for_replacement(&pool, crash_pid).await;

    pool.dispatch(contact_request("task-cancel", "item-cancel"))
        .await
        .unwrap();
    pool.cancel_task("task-cancel").await.unwrap();
    let cancelled = next_event(&mut pool).await;
    assert!(matches!(
        cancelled,
        WorkerEvent::Cancelled { ref item_id, .. } if item_id == "item-cancel"
    ));
    assert_eq!(pool.failure_count(), 1);
}

/// 给进程事件设置明确上限，避免测试在协议断开时永久等待。
async fn next_event(pool: &mut WorkerPool) -> WorkerEvent {
    tokio::time::timeout(Duration::from_secs(15), pool.next_event())
        .await
        .expect("worker event timeout")
        .expect("worker event channel closed")
}

/// 等待异步补建完成，并确认新槽位没有复用旧 PID。
async fn wait_for_replacement(pool: &WorkerPool, previous_pid: u32) {
    tokio::time::timeout(Duration::from_secs(15), async {
        loop {
            if pool
                .worker_process_ids()
                .first()
                .is_some_and(|process_id| *process_id != previous_pid)
            {
                return;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .expect("replacement Worker timeout");
}

/// 构造固定 JPEG 的一筛请求。
fn stage1_request(task_id: &str, item_id: &str) -> proto::WorkerEnvelope {
    proto::WorkerEnvelope {
        payload: Some(worker_envelope::Payload::ProbeAndStage1(
            proto::ProbeAndStage1 {
                task_id: task_id.into(),
                item_id: item_id.into(),
                display_path: media_fixture("image.jpg").to_string_lossy().into_owned(),
                media_kind: proto::MediaKind::MediaImage as i32,
                generate_contact_sheet: true,
            },
        )),
    }
}

/// 构造需要六次解码、足以保持 Worker running 的联系表请求。
fn contact_request(task_id: &str, item_id: &str) -> proto::WorkerEnvelope {
    proto::WorkerEnvelope {
        payload: Some(worker_envelope::Payload::BuildContactSheet(
            proto::BuildContactSheet {
                task_id: task_id.into(),
                item_id: item_id.into(),
                display_path: media_fixture("video-12s.mp4")
                    .to_string_lossy()
                    .into_owned(),
                frame_slots: vec![0, 1, 2, 3, 4, 5],
            },
        )),
    }
}

/// 返回仓库固定媒体夹具的绝对路径。
fn media_fixture(name: &str) -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("..")
        .join("..")
        .join("tests")
        .join("fixtures")
        .join("media")
        .join(name)
}

/// 把测试二进制和五个 DLL 发布到临时生产相对目录；未配置 DLL 来源时跳过。
fn runtime_fixture() -> Option<tempfile::TempDir> {
    let source = env::var_os("DEDUP_FFMPEG_TEST_SOURCE_DIR").map(PathBuf::from)?;
    let directory = tempfile::tempdir().unwrap();
    let worker = PathBuf::from(env!("CARGO_BIN_EXE_worker"));
    std::fs::copy(worker, directory.path().join("worker.exe")).unwrap();
    let ffmpeg = directory.path().join("runtime").join("ffmpeg");
    std::fs::create_dir_all(&ffmpeg).unwrap();
    for name in dedup_media_ffmpeg::required_dlls() {
        std::fs::copy(source.join(name), ffmpeg.join(name)).unwrap();
    }
    Some(directory)
}

#[derive(Default)]
/// 模拟 NodeStore 两阶段重启中唯一需要的重新排队动作。
struct FakeStore {
    queued: Vec<String>,
}

impl FakeStore {
    /// 记录第一阶段返回且已在事务中改回 queued 的任务项。
    fn requeue_items(&mut self, items: &[String]) {
        self.queued.extend_from_slice(items);
    }
}
