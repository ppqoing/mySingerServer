//! 不依赖 FFmpeg 的真实 Worker 匿名管道 V5 多响应进程测试。

use std::{
    env, fs,
    path::{Path, PathBuf},
    process, thread,
    time::Duration,
};

use dedup_core::{DisplayPath, MachineId, MediaKind, NormalizedPath};
use dedup_media_ffmpeg::{DecodedFrame, MediaProbe};
use dedup_node_engine::worker::{
    MediaDecoder, WorkerEvent, WorkerFileIdentity, WorkerLaunch, WorkerPipeline, WorkerPool,
    WorkerPoolConfig, WorkerRequestHandler, decode_stage2_payload,
};
use dedup_protocol::BASE_MISSING_PROBE;
use dedup_protocol::proto::{self, worker_envelope};

#[path = "../src/protocol_loop.rs"]
mod protocol_loop;

/// 测试子进程固定文件名；父进程据此让同一测试二进制进入协议服务模式。
const CHILD_FILE_NAME: &str = "worker-protocol-child.exe";

/// `missing_parts=0` 不应调用媒体解码器；若误调用则返回明确失败。
struct ProtocolTestDecoder {
    entered: Option<PathBuf>,
    release: Option<PathBuf>,
    /// 二筛解码开始后通知父进程的 gate 文件。
    stage2_entered: Option<PathBuf>,
    /// 允许二筛解码返回并继续发送源读取完成事件的 gate 文件。
    stage2_release: Option<PathBuf>,
}

impl ProtocolTestDecoder {
    /// 从父进程继承的临时路径创建可阻塞 source probe 的解码器。
    fn from_environment() -> Self {
        Self {
            entered: env::var_os("DEDUP_WORKER_PHASE_ENTERED").map(PathBuf::from),
            release: env::var_os("DEDUP_WORKER_PHASE_RELEASE").map(PathBuf::from),
            stage2_entered: env::var_os("DEDUP_WORKER_STAGE2_ENTERED").map(PathBuf::from),
            stage2_release: env::var_os("DEDUP_WORKER_STAGE2_RELEASE").map(PathBuf::from),
        }
    }
}

impl MediaDecoder for ProtocolTestDecoder {
    fn probe_media(&self, _: &Path) -> Result<MediaProbe, String> {
        Err("missing_parts=0 不应探测媒体".into())
    }

    fn decode_frame_at(&self, path: &Path, _: f64) -> Result<DecodedFrame, String> {
        let entered = self
            .stage2_entered
            .as_ref()
            .ok_or("缺少 stage2 entered gate")?;
        let release = self
            .stage2_release
            .as_ref()
            .ok_or("缺少 stage2 release gate")?;
        fs::write(entered, b"entered").map_err(|error| error.to_string())?;
        while !release.exists() {
            thread::sleep(Duration::from_millis(2));
        }
        // 读取阶段必须在 SourceReadComplete 之前完成；事件后测试会删除该路径。
        let _ = fs::read(path).map_err(|error| format!("事件前读取源文件失败: {error}"))?;
        Ok(DecodedFrame {
            width: 8,
            height: 8,
            rgb24: vec![0x31; 8 * 8 * 3],
        })
    }

    fn probe_source(
        &self,
        _: &mut dyn dedup_media_ffmpeg::SeekableMediaSource,
        _: u32,
    ) -> Result<MediaProbe, String> {
        let entered = self.entered.as_ref().ok_or("缺少 entered gate")?;
        let release = self.release.as_ref().ok_or("缺少 release gate")?;
        fs::write(entered, b"entered").map_err(|error| error.to_string())?;
        while !release.exists() {
            thread::sleep(Duration::from_millis(2));
        }
        Ok(MediaProbe {
            media_kind: MediaKind::Other,
            width: 0,
            height: 0,
            duration_ms: None,
        })
    }
}

/// harnessless 测试入口；复制后的子进程复用生产协议循环，原进程负责真实 WorkerPool 断言。
#[tokio::main(flavor = "multi_thread", worker_threads = 2)]
async fn main() -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let executable = env::current_exe()?;
    if executable.file_name().and_then(|name| name.to_str()) == Some(CHILD_FILE_NAME) {
        return run_child_protocol().await;
    }
    run_parent_assertions(&executable).await?;
    println!("WORKER_PROTOCOL_PROCESS_PASS");
    Ok(())
}

/// 使用 fake decoder 和生产 `protocol_loop` 驱动 stdin/stdout 匿名管道。
async fn run_child_protocol() -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let pipeline = WorkerPipeline::new(ProtocolTestDecoder::from_environment());
    let mut handler = WorkerRequestHandler::new(pipeline);
    protocol_loop::run_worker_protocol(
        tokio::io::stdin(),
        tokio::io::stdout(),
        &mut handler,
        process::id(),
    )
    .await
}

/// 启动真实 WorkerPool，验证两帧响应的槽位生命周期及同一进程复用。
async fn run_parent_assertions(
    executable: &Path,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let directory = tempfile::tempdir()?;
    let child = directory.path().join(CHILD_FILE_NAME);
    std::fs::copy(executable, &child)?;
    let source = directory.path().join("cache-hit.bin");
    std::fs::write(&source, b"protocol-process")?;
    let entered = directory.path().join("probe-entered.gate");
    let release = directory.path().join("probe-release.gate");
    // 此 harnessless 测试独占进程环境，子进程只读取两个临时 gate 路径。
    unsafe {
        env::set_var("DEDUP_WORKER_PHASE_ENTERED", &entered);
        env::set_var("DEDUP_WORKER_PHASE_RELEASE", &release);
    }
    // 二筛测试源文件在收到 SourceReadComplete 后删除，验证后续只消费内存帧。
    let stage2_source = directory.path().join("stage2-image.bin");
    fs::write(&stage2_source, b"stage2-source")?;
    let stage2_entered = directory.path().join("stage2-entered.gate");
    let stage2_release = directory.path().join("stage2-release.gate");
    unsafe {
        env::set_var("DEDUP_WORKER_STAGE2_ENTERED", &stage2_entered);
        env::set_var("DEDUP_WORKER_STAGE2_RELEASE", &stage2_release);
    }

    let config = WorkerPoolConfig::new(WorkerLaunch::new(child), 1)
        .with_ready_timeout(Duration::from_secs(10));
    let mut pool = WorkerPool::start(config).await?;
    let first_pid = run_one_item(&mut pool, &source, "item-1", [0x31; 16]).await?;
    let second_pid = run_one_item(&mut pool, &source, "item-2", [0x32; 16]).await?;
    assert_eq!(second_pid, first_pid, "终态后应复用同一 Worker slot 进程");
    run_blocked_source_item(&mut pool, &source, &entered, &release).await?;
    run_stage2_source_item(&mut pool, &stage2_source, &stage2_entered, &stage2_release).await?;
    Ok(())
}

/// 派发一个 V5 请求并依次验证 Started、非终态读取完成和终态结果。
async fn run_one_item(
    pool: &mut WorkerPool,
    source: &Path,
    item_id: &str,
    md5: [u8; 16],
) -> Result<u32, Box<dyn std::error::Error + Send + Sync>> {
    pool.dispatch_runtime(
        compute_request(source, item_id, md5),
        worker_identity(source),
    )
    .await?;

    let started = next_event(pool).await?;
    let WorkerEvent::Started {
        task_id,
        item_id: started_item,
        process_id: Some(process_id),
        ..
    } = started
    else {
        panic!("第一条池事件必须是 Started");
    };
    assert_eq!(task_id, "task-v5");
    assert_eq!(started_item, item_id);

    assert_phase(
        next_event(pool).await?,
        item_id,
        proto::RuntimeWorkerPhase::RuntimeWorkerDecode,
    );
    let source_complete = next_event(pool).await?;
    assert!(matches!(
        source_complete,
        WorkerEvent::BaseSourceReadComplete {
            task_id,
            item_id: completed_item,
            request_elapsed_us: Some(_),
            ..
        } if task_id == "task-v5" && completed_item == item_id
    ));
    assert_eq!(pool.busy_workers(), 1, "非终态事件不得释放 Worker slot");

    assert_phase(
        next_event(pool).await?,
        item_id,
        proto::RuntimeWorkerPhase::RuntimeWorkerFeature,
    );
    assert_phase(
        next_event(pool).await?,
        item_id,
        proto::RuntimeWorkerPhase::RuntimeWorkerResultWait,
    );

    let terminal = next_event(pool).await?;
    let WorkerEvent::Completed {
        task_id,
        item_id: completed_item,
        response,
    } = terminal
    else {
        panic!("第二条 Worker 响应必须归并为 Completed");
    };
    assert_eq!(task_id, "task-v5");
    assert_eq!(completed_item, item_id);
    let Some(worker_envelope::Payload::BaseComputeResult(result)) = response.payload else {
        panic!("终态必须是 BaseComputeResult");
    };
    assert_eq!(result.task_id, "task-v5");
    assert_eq!(result.item_id, item_id);
    assert_eq!(result.md5, md5);
    assert!(result.payload.is_empty());
    assert_eq!(pool.busy_workers(), 0, "终态结果必须释放 Worker slot");
    Ok(process_id)
}

/// 证明 Worker 在 source probe 阻塞前已经即时写出 DECODE，而不是计算结束后批量伪造事件。
async fn run_blocked_source_item(
    pool: &mut WorkerPool,
    source: &Path,
    entered: &Path,
    release: &Path,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    pool.dispatch_runtime(
        compute_request_with_missing(source, "item-blocked", [0x41; 16], BASE_MISSING_PROBE),
        worker_identity(source),
    )
    .await?;
    assert!(matches!(
        next_event(pool).await?,
        WorkerEvent::Started { .. }
    ));
    assert_phase(
        next_event(pool).await?,
        "item-blocked",
        proto::RuntimeWorkerPhase::RuntimeWorkerDecode,
    );
    tokio::time::timeout(Duration::from_secs(3), async {
        while !entered.exists() {
            tokio::task::yield_now().await;
        }
    })
    .await?;
    assert!(
        tokio::time::timeout(Duration::from_millis(100), pool.next_event())
            .await
            .is_err(),
        "source 仍阻塞时不得提前收到 SourceComplete/FEATURE/terminal"
    );
    fs::write(release, b"release")?;
    assert!(matches!(
        next_event(pool).await?,
        WorkerEvent::BaseSourceReadComplete { item_id, .. } if item_id == "item-blocked"
    ));
    assert_phase(
        next_event(pool).await?,
        "item-blocked",
        proto::RuntimeWorkerPhase::RuntimeWorkerFeature,
    );
    assert_phase(
        next_event(pool).await?,
        "item-blocked",
        proto::RuntimeWorkerPhase::RuntimeWorkerResultWait,
    );
    assert!(matches!(
        next_event(pool).await?,
        WorkerEvent::Completed { item_id, .. } if item_id == "item-blocked"
    ));
    Ok(())
}

/// 证明真实二筛进程在源路径读取结束后发出事件，事件后仅使用已拥有的内存帧。
async fn run_stage2_source_item(
    pool: &mut WorkerPool,
    source: &Path,
    entered: &Path,
    release: &Path,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let item_id = "item-stage2-source";
    pool.dispatch_runtime(stage2_request(source, item_id), stage2_identity(source))
        .await?;
    assert!(matches!(
        next_event(pool).await?,
        WorkerEvent::Started {
            task_id,
            item_id: actual_item,
            ..
        } if task_id == "task-stage2" && actual_item == item_id
    ));
    assert_phase_for_task(
        next_event(pool).await?,
        "task-stage2",
        item_id,
        proto::RuntimeWorkerPhase::RuntimeWorkerDecode,
    );
    tokio::time::timeout(Duration::from_secs(3), async {
        while !entered.exists() {
            tokio::task::yield_now().await;
        }
    })
    .await
    .map_err(|_| "二筛解码器未进入源读取 gate")?;
    assert!(
        tokio::time::timeout(Duration::from_millis(100), pool.next_event())
            .await
            .is_err(),
        "二筛源读取阻塞时不得提前收到 SourceComplete/FEATURE/terminal"
    );
    fs::write(release, b"release")?;
    assert!(matches!(
        next_event(pool).await?,
        WorkerEvent::Stage2SourceReadComplete {
            task_id,
            item_id: actual_item,
            request_elapsed_us: Some(_),
            ..
        } if task_id == "task-stage2" && actual_item == item_id
    ));
    fs::remove_file(source)?;
    assert_eq!(pool.busy_workers(), 1, "二筛源读取事件不得释放 Worker slot");
    assert_phase_for_task(
        next_event(pool).await?,
        "task-stage2",
        item_id,
        proto::RuntimeWorkerPhase::RuntimeWorkerFeature,
    );
    assert_phase_for_task(
        next_event(pool).await?,
        "task-stage2",
        item_id,
        proto::RuntimeWorkerPhase::RuntimeWorkerResultWait,
    );
    let WorkerEvent::Completed { response, .. } = next_event(pool).await? else {
        panic!("二筛源事件后必须返回 Stage2Result 终态");
    };
    let Some(worker_envelope::Payload::Stage2Result(result)) = response.payload else {
        panic!("二筛源事件后必须返回 Stage2Result 终态");
    };
    let output = decode_stage2_payload(&result.payload)?;
    assert_eq!(output.frames.len(), 1);
    assert!(
        output.frames[0].feature.is_some(),
        "事件后必须完成二筛 CPU 特征"
    );
    assert_eq!(pool.busy_workers(), 0, "二筛终态结果必须释放 Worker slot");
    Ok(())
}

/// 断言一个即时 Worker 阶段事件及其身份。
fn assert_phase(event: WorkerEvent, item_id: &str, phase: proto::RuntimeWorkerPhase) {
    assert!(matches!(
        event,
        WorkerEvent::PhaseChanged {
            task_id,
            item_id: actual_item,
            phase: actual_phase,
            request_elapsed_us: Some(_),
            ..
        } if task_id == "task-v5" && actual_item == item_id && actual_phase == phase
    ));
}

/// 断言指定任务的 Worker 阶段，避免二筛测试误接收其他请求的同名事件。
fn assert_phase_for_task(
    event: WorkerEvent,
    task_id: &str,
    item_id: &str,
    phase: proto::RuntimeWorkerPhase,
) {
    assert!(matches!(
        event,
        WorkerEvent::PhaseChanged {
            task_id: actual_task,
            item_id: actual_item,
            phase: actual_phase,
            request_elapsed_us: Some(_),
            ..
        } if actual_task == task_id && actual_item == item_id && actual_phase == phase
    ));
}

/// 给真实进程事件设置上限，协议卡住时返回明确失败而不是永久等待。
async fn next_event(
    pool: &mut WorkerPool,
) -> Result<WorkerEvent, Box<dyn std::error::Error + Send + Sync>> {
    Ok(
        tokio::time::timeout(Duration::from_secs(10), pool.next_event())
            .await?
            .ok_or("WorkerPool 事件通道提前关闭")?,
    )
}

/// 构造不需要媒体计算、但仍执行打开与长度校验的一次性 V5 请求。
fn compute_request(source: &Path, item_id: &str, md5: [u8; 16]) -> proto::WorkerEnvelope {
    compute_request_with_missing(source, item_id, md5, 0)
}

/// 构造带显式缺失掩码的一次性 V5 请求。
fn compute_request_with_missing(
    source: &Path,
    item_id: &str,
    md5: [u8; 16],
    missing_parts: u32,
) -> proto::WorkerEnvelope {
    proto::WorkerEnvelope {
        payload: Some(worker_envelope::Payload::ComputeBaseFeatures(
            proto::ComputeBaseFeatures {
                task_id: "task-v5".into(),
                item_id: item_id.into(),
                machine_id: "machine-v5".into(),
                normalized_path: NormalizedPath::new(source).unwrap().to_string(),
                display_path: source.to_string_lossy().into_owned(),
                file_size: std::fs::metadata(source).unwrap().len(),
                physical_disk_id: "disk-v5".into(),
                md5: md5.to_vec(),
                media_kind: proto::MediaKind::MediaOther as i32,
                missing_parts,
                block_size_bytes: 4,
                block_timeout_ms: 3_000,
                block_retries: 1,
                decoder_threads: 1,
            },
        )),
    }
}

/// 构造真实 Pool 的冻结文件上下文，供 Started 与故障路径保留身份。
fn worker_identity(source: &Path) -> WorkerFileIdentity {
    WorkerFileIdentity {
        machine_id: MachineId::from_sha256([0x71; 32]),
        normalized_path: NormalizedPath::new(source).unwrap(),
        display_path: DisplayPath::new(source).unwrap(),
        file_size: std::fs::metadata(source).unwrap().len(),
        stage: "base_compute".into(),
        physical_disk_id: "disk-v5".into(),
    }
}

/// 构造使用真实协议循环的图片二筛请求，源读取结束后才允许释放文件路径。
fn stage2_request(source: &Path, item_id: &str) -> proto::WorkerEnvelope {
    proto::WorkerEnvelope {
        payload: Some(worker_envelope::Payload::ComputeStage2(
            proto::ComputeStage2 {
                task_id: "task-stage2".into(),
                item_id: item_id.into(),
                display_path: source.to_string_lossy().into_owned(),
                frame_slots: Vec::new(),
                contact_sheet_path: String::new(),
                generate_contact_sheet_if_missing: false,
            },
        )),
    }
}

/// 构造二筛 Worker 身份，让 WorkerPool 按二筛阶段保留同一槽位。
fn stage2_identity(source: &Path) -> WorkerFileIdentity {
    WorkerFileIdentity {
        machine_id: MachineId::from_sha256([0x73; 32]),
        normalized_path: NormalizedPath::new(source).unwrap(),
        display_path: DisplayPath::new(source).unwrap(),
        file_size: fs::metadata(source).unwrap().len(),
        stage: "compute_stage2_features".into(),
        physical_disk_id: "disk-stage2-process".into(),
    }
}
