//! 真实 worker.exe 的 Ready、任务、重启、崩溃替换和取消进程级测试。

use std::{
    env,
    path::{Path, PathBuf},
    time::Duration,
};

use dedup_core::{DisplayPath, MachineId, NormalizedPath};
use dedup_node_engine::worker::{
    WorkerEvent, WorkerFileIdentity, WorkerLaunch, WorkerPool, WorkerPoolConfig,
    decode_base_compute_payload, decode_stage1_payload,
};
use dedup_protocol::proto::{self, worker_envelope};
use dedup_protocol::{BASE_MISSING_PROBE, BASE_MISSING_STAGE1};
use dedup_windows::wait_for_process_exit;

#[tokio::test]
/// 用真实 DLL 和 worker.exe 覆盖正常结果及三类进程生命周期分支。
async fn real_worker_process_supports_results_restart_crash_and_cancel() {
    let Some(runtime) = runtime_fixture() else {
        return;
    };
    let worker = runtime.path().join("worker.exe");
    let config = WorkerPoolConfig::new(WorkerLaunch::new(worker), 1)
        .with_result_read_delay(Duration::from_millis(500));
    let mut pool = WorkerPool::start(config.clone()).await.unwrap();

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
    pool.shutdown().await.unwrap();
    tokio::task::spawn_blocking(move || wait_for_process_exit(first_pid))
        .await
        .unwrap()
        .unwrap();
    let mut pool = WorkerPool::start(config).await.unwrap();
    assert_ne!(
        pool.worker_process_ids()[0],
        first_pid,
        "新 Pool 必须使用新进程"
    );
    pool.dispatch(stage1_request("task-restarted", "item-restarted"))
        .await
        .unwrap();
    assert!(matches!(
        next_event(&mut pool).await,
        WorkerEvent::Completed { .. }
    ));

    let crash_pid = pool.worker_process_ids()[0];
    let crash_source = media_fixture("video-12s.mp4");
    // 崩溃事件必须由运行时派发边界冻结完整文件身份，普通 dispatch 不携带路径上下文。
    let crash_identity = WorkerFileIdentity {
        machine_id: MachineId::from_sha256([0x74; 32]),
        normalized_path: NormalizedPath::new(&crash_source).unwrap(),
        display_path: DisplayPath::new(&crash_source).unwrap(),
        file_size: std::fs::metadata(&crash_source).unwrap().len(),
        stage: "contact_sheet".into(),
        physical_disk_id: "disk-lifecycle".into(),
    };
    pool.dispatch_runtime(contact_request("task-crash", "item-crash"), crash_identity)
        .await
        .unwrap();
    assert!(matches!(
        next_event(&mut pool).await,
        WorkerEvent::Started {
            ref task_id,
            ref item_id,
            ..
        } if task_id == "task-crash" && item_id == "item-crash"
    ));
    pool.terminate_worker_for_test(crash_pid).await.unwrap();
    let crashed = next_event(&mut pool).await;
    assert!(
        matches!(
            &crashed,
            WorkerEvent::Crashed { item_id, .. } if item_id == "item-crash"
        ),
        "终止运行项后收到意外事件: {crashed:?}"
    );
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
    pool.shutdown().await.unwrap();
}

#[tokio::test]
/// 源读取完成事件后槽位继续繁忙，并由同一一次性请求返回终态。
async fn one_shot_base_compute_keeps_slot_busy_until_terminal_result() {
    let Some(runtime) = runtime_fixture() else {
        return;
    };
    let source = runtime.path().join("base-image.jpg");
    std::fs::copy(media_fixture("image.jpg"), &source).unwrap();
    let worker = runtime.path().join("worker.exe");
    let mut pool = WorkerPool::start(WorkerPoolConfig::new(WorkerLaunch::new(worker), 1))
        .await
        .unwrap();

    pool.dispatch(base_compute_request(&source)).await.unwrap();
    assert!(matches!(
        next_event(&mut pool).await,
        WorkerEvent::PhaseChanged {
            ref task_id,
            ref item_id,
            phase: proto::RuntimeWorkerPhase::RuntimeWorkerDecode,
            request_elapsed_us: Some(_),
            ..
        } if task_id == "task-base" && item_id == "item-base"
    ));
    let source_event = next_event(&mut pool).await;
    let WorkerEvent::BaseSourceReadComplete {
        task_id, item_id, ..
    } = source_event
    else {
        panic!("应先收到 BaseSourceReadComplete")
    };
    assert_eq!(task_id, "task-base");
    assert_eq!(item_id, "item-base");
    assert_eq!(pool.busy_workers(), 1);

    for phase in [
        proto::RuntimeWorkerPhase::RuntimeWorkerFeature,
        proto::RuntimeWorkerPhase::RuntimeWorkerResultWait,
    ] {
        assert!(matches!(
            next_event(&mut pool).await,
            WorkerEvent::PhaseChanged {
                ref task_id,
                ref item_id,
                phase: actual,
                request_elapsed_us: Some(_),
                ..
            } if task_id == "task-base" && item_id == "item-base" && actual == phase
        ));
        assert_eq!(pool.busy_workers(), 1, "非终态阶段不得释放 Worker slot");
    }

    let completed = next_event(&mut pool).await;
    let WorkerEvent::Completed { response, .. } = completed else {
        panic!("一次性基础计算应产生最终完成事件")
    };
    let Some(worker_envelope::Payload::BaseComputeResult(result)) = response.payload else {
        panic!("一次性基础计算应返回 BaseComputeResult")
    };
    assert_eq!(result.md5, vec![0x5a; 16]);
    assert_eq!(
        decode_base_compute_payload(&result.payload)
            .unwrap()
            .stage1_frames
            .unwrap()
            .len(),
        1
    );
    assert_eq!(pool.busy_workers(), 0);
}

#[tokio::test]
/// 真实 Worker 在一次性基础计算期间退出时必须返回完整路径，并由池补建新进程。
async fn real_base_compute_crash_keeps_full_path_and_replaces_worker() {
    let Some(runtime) = runtime_fixture() else {
        return;
    };
    let media_root = runtime.path().join("媒体 库");
    std::fs::create_dir_all(&media_root).unwrap();
    let source = media_root.join("崩溃文件.jpg");
    std::fs::copy(media_fixture("image.jpg"), &source).unwrap();
    let worker = runtime.path().join("worker.exe");
    let config = WorkerPoolConfig::new(WorkerLaunch::new(worker), 1)
        .with_result_read_delay(Duration::from_millis(500));
    let mut pool = WorkerPool::start(config).await.unwrap();
    let crash_pid = pool.worker_process_ids()[0];
    let identity = WorkerFileIdentity {
        machine_id: MachineId::from_sha256([0x73; 32]),
        normalized_path: NormalizedPath::new(&source).unwrap(),
        display_path: DisplayPath::new(&source).unwrap(),
        file_size: std::fs::metadata(&source).unwrap().len(),
        stage: "base_compute".into(),
        physical_disk_id: "disk-real".into(),
    };
    pool.dispatch_runtime(base_compute_request(&source), identity)
        .await
        .unwrap();
    assert!(matches!(
        next_event(&mut pool).await,
        WorkerEvent::Started { .. }
    ));

    pool.terminate_worker_for_test(crash_pid).await.unwrap();

    let WorkerEvent::Crashed {
        identity,
        process_id,
        ..
    } = next_event(&mut pool).await
    else {
        panic!("一次性基础计算期间退出必须产生文件级崩溃事件");
    };
    assert_eq!(identity.display_path.as_path(), source.as_path());
    assert_eq!(identity.stage, "base_compute");
    assert_eq!(process_id, Some(crash_pid));
    wait_for_replacement(&pool, crash_pid).await;
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

/// 构造 Worker 一次性基础计算请求。
fn base_compute_request(path: &Path) -> proto::WorkerEnvelope {
    proto::WorkerEnvelope {
        payload: Some(worker_envelope::Payload::ComputeBaseFeatures(
            proto::ComputeBaseFeatures {
                task_id: "task-base".into(),
                item_id: "item-base".into(),
                machine_id: "machine-base".into(),
                normalized_path: "i:/media/base-image.jpg".into(),
                display_path: path.to_string_lossy().into_owned(),
                file_size: std::fs::metadata(path).unwrap().len(),
                physical_disk_id: "disk-base".into(),
                md5: vec![0x5a; 16],
                media_kind: proto::MediaKind::MediaImage as i32,
                missing_parts: BASE_MISSING_PROBE | BASE_MISSING_STAGE1,
                block_size_bytes: 64 * 1024,
                block_timeout_ms: 3_000,
                block_retries: 2,
                decoder_threads: 1,
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
