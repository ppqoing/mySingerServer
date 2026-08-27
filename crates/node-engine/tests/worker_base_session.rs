//! Worker 一次性基础计算的单句柄、Node MD5 与有序响应契约。

#![cfg(windows)]

use std::{
    io::SeekFrom,
    path::{Path, PathBuf},
    sync::{
        Arc, Mutex,
        atomic::{AtomicUsize, Ordering},
    },
};

use dedup_core::{DisplayPath, MachineId, MediaKind, NormalizedPath};
use dedup_media_ffmpeg::{DecodedFrame, MediaProbe, SeekableMediaSource};
use dedup_node_engine::worker::{
    BaseComputeOutput, MediaDecoder, WorkerEvent, WorkerFileIdentity, WorkerPipeline, WorkerPool,
    WorkerRequestHandler, decode_base_compute_payload,
};
use dedup_protocol::{
    BASE_MISSING_PROBE, BASE_MISSING_STAGE1,
    proto::{self, WorkerEnvelope, worker_envelope},
};

/// 只使用 Worker 已打开 source 的测试解码器，并在探测阶段重命名原文件。
struct RenameDuringDecode {
    /// 原始路径；重命名发生后任何按路径二次打开都会失败。
    original: PathBuf,
    /// 重命名目标路径，用于证明打开句柄在路径变化后仍可继续读取。
    moved: PathBuf,
    /// 自定义 source 实际被探测的次数。
    source_probes: Arc<AtomicUsize>,
}

impl MediaDecoder for RenameDuringDecode {
    fn probe_media(&self, _: &Path) -> Result<MediaProbe, String> {
        Err("不允许重新按路径打开文件".into())
    }

    fn decode_frame_at(&self, _: &Path, _: f64) -> Result<DecodedFrame, String> {
        Err("不允许重新按路径打开文件".into())
    }

    fn probe_source(
        &self,
        source: &mut dyn SeekableMediaSource,
        _decoder_threads: u32,
    ) -> Result<MediaProbe, String> {
        self.source_probes.fetch_add(1, Ordering::SeqCst);
        std::fs::rename(&self.original, &self.moved).map_err(|error| error.to_string())?;
        read_marker(source)?;
        Ok(MediaProbe {
            media_kind: MediaKind::Image,
            width: 2,
            height: 2,
            duration_ms: None,
        })
    }

    fn decode_frame_from_source(
        &self,
        source: &mut dyn SeekableMediaSource,
        _: f64,
        _decoder_threads: u32,
    ) -> Result<DecodedFrame, String> {
        let marker = read_marker(source)?;
        Ok(DecodedFrame {
            width: 2,
            height: 2,
            rgb24: vec![marker; 12],
        })
    }
}

/// 若一次性请求错误触发媒体访问，立即返回可识别失败的测试解码器。
struct RejectMediaRead;

impl MediaDecoder for RejectMediaRead {
    fn probe_media(&self, _: &Path) -> Result<MediaProbe, String> {
        Err("不应按路径访问媒体".into())
    }

    fn decode_frame_at(&self, _: &Path, _: f64) -> Result<DecodedFrame, String> {
        Err("不应按路径访问媒体".into())
    }

    fn probe_source(
        &self,
        _: &mut dyn SeekableMediaSource,
        _decoder_threads: u32,
    ) -> Result<MediaProbe, String> {
        Err("不应访问媒体 source".into())
    }
}

/// 读取一个字节后故意失败，用于验证失败请求不得伪称源读取完成。
struct FailAfterSourceRead;

impl MediaDecoder for FailAfterSourceRead {
    fn probe_media(&self, _: &Path) -> Result<MediaProbe, String> {
        Err("不允许路径回退".into())
    }

    fn decode_frame_at(&self, _: &Path, _: f64) -> Result<DecodedFrame, String> {
        Err("不允许路径回退".into())
    }

    fn probe_source(
        &self,
        source: &mut dyn SeekableMediaSource,
        _decoder_threads: u32,
    ) -> Result<MediaProbe, String> {
        read_marker(source)?;
        Err("测试解码失败".into())
    }
}

/// 记录每次自定义 source 探测和抽帧收到的显式解码线程预算。
struct RecordingThreadBudgetDecoder {
    /// 按真实调用顺序保存 `(阶段, 线程数)`。
    calls: Arc<Mutex<Vec<(&'static str, u32)>>>,
}

impl MediaDecoder for RecordingThreadBudgetDecoder {
    fn probe_media(&self, _: &Path) -> Result<MediaProbe, String> {
        Err("一次性基础计算不得回退路径探测".into())
    }

    fn decode_frame_at(&self, _: &Path, _: f64) -> Result<DecodedFrame, String> {
        Err("一次性基础计算不得回退路径抽帧".into())
    }

    fn probe_source(
        &self,
        _: &mut dyn SeekableMediaSource,
        decoder_threads: u32,
    ) -> Result<MediaProbe, String> {
        self.calls.lock().unwrap().push(("probe", decoder_threads));
        Ok(MediaProbe {
            media_kind: MediaKind::Video,
            width: 2,
            height: 2,
            duration_ms: Some(6_000),
        })
    }

    fn decode_frame_from_source(
        &self,
        _: &mut dyn SeekableMediaSource,
        _: f64,
        decoder_threads: u32,
    ) -> Result<DecodedFrame, String> {
        self.calls.lock().unwrap().push(("decode", decoder_threads));
        Ok(DecodedFrame {
            width: 2,
            height: 2,
            rgb24: vec![0x55; 12],
        })
    }
}

#[test]
fn one_shot_compute_uses_supplied_md5_and_orders_source_event_before_terminal_result() {
    let directory = tempfile::tempdir().unwrap();
    let original = directory.path().join("image.bin");
    let moved = directory.path().join("renamed.bin");
    std::fs::write(&original, b"same-open-handle").unwrap();
    let source_probes = Arc::new(AtomicUsize::new(0));
    let decoder = RenameDuringDecode {
        original: original.clone(),
        moved: moved.clone(),
        source_probes: Arc::clone(&source_probes),
    };
    let mut handler = WorkerRequestHandler::new(WorkerPipeline::new(decoder));
    let supplied_md5 = [0x5a; 16];

    let responses = handler.handle(compute_request(
        &original,
        supplied_md5,
        BASE_MISSING_PROBE | BASE_MISSING_STAGE1,
        2,
    ));

    assert_eq!(responses.len(), 2);
    let Some(worker_envelope::Payload::BaseSourceReadComplete(source_complete)) =
        responses[0].payload.as_ref()
    else {
        panic!("第一条响应必须是 BaseSourceReadComplete");
    };
    assert_eq!(source_complete.task_id, "task-a");
    assert_eq!(source_complete.item_id, "item-a");
    let Some(worker_envelope::Payload::BaseComputeResult(result)) = responses[1].payload.as_ref()
    else {
        panic!("第二条响应必须是终态 BaseComputeResult");
    };
    assert_eq!(result.md5, supplied_md5);
    let output = decode_base_compute_payload(&result.payload).unwrap();
    assert_eq!(output.probe.unwrap().media_kind, MediaKind::Image);
    assert_eq!(output.stage1_frames.unwrap().len(), 1);
    assert_eq!(source_probes.load(Ordering::SeqCst), 1);
    assert!(!original.exists());
    assert!(moved.exists());
}

#[test]
fn missing_zero_validates_file_boundary_without_media_compute() {
    let directory = tempfile::tempdir().unwrap();
    let source = directory.path().join("cache-hit.bin");
    std::fs::write(&source, b"cache-hit").unwrap();
    let supplied_md5 = [0x2c; 16];
    let mut handler = WorkerRequestHandler::new(WorkerPipeline::new(RejectMediaRead));

    let responses = handler.handle(compute_request(&source, supplied_md5, 0, 1));

    assert!(matches!(
        responses[0].payload,
        Some(worker_envelope::Payload::BaseSourceReadComplete(_))
    ));
    let Some(worker_envelope::Payload::BaseComputeResult(result)) = responses[1].payload.as_ref()
    else {
        panic!("missing_parts=0 仍应正常终结");
    };
    assert_eq!(result.md5, supplied_md5);
    assert!(result.payload.is_empty());
}

#[test]
fn invalid_one_shot_inputs_return_exactly_one_structured_failure() {
    let directory = tempfile::tempdir().unwrap();
    let source = directory.path().join("invalid.bin");
    std::fs::write(&source, b"invalid-input").unwrap();
    let valid = compute_command(&source, [0x11; 16], BASE_MISSING_PROBE, 1);
    let mut invalid = Vec::new();

    let mut short_md5 = valid.clone();
    short_md5.md5.pop();
    invalid.push(short_md5);
    let mut unknown_missing_bit = valid.clone();
    unknown_missing_bit.missing_parts |= 1 << 31;
    invalid.push(unknown_missing_bit);
    let mut zero_block_size = valid.clone();
    zero_block_size.block_size_bytes = 0;
    invalid.push(zero_block_size);
    let mut zero_timeout = valid.clone();
    zero_timeout.block_timeout_ms = 0;
    invalid.push(zero_timeout);
    let mut zero_decoder_threads = valid.clone();
    zero_decoder_threads.decoder_threads = 0;
    invalid.push(zero_decoder_threads);
    let mut invalid_media_kind = valid.clone();
    invalid_media_kind.media_kind = i32::MAX;
    invalid.push(invalid_media_kind);

    let mut handler = WorkerRequestHandler::new(WorkerPipeline::new(RejectMediaRead));
    for command in invalid {
        assert_single_failure(handler.handle(WorkerEnvelope {
            payload: Some(worker_envelope::Payload::ComputeBaseFeatures(command)),
        }));
    }
}

#[test]
fn length_mismatch_or_decoder_error_never_emits_source_complete() {
    let directory = tempfile::tempdir().unwrap();
    let source = directory.path().join("failure.bin");
    std::fs::write(&source, b"source-failure").unwrap();

    let mut wrong_length = compute_command(&source, [0x33; 16], 0, 1);
    wrong_length.file_size += 1;
    let mut boundary_handler = WorkerRequestHandler::new(WorkerPipeline::new(RejectMediaRead));
    assert_single_failure(boundary_handler.handle(WorkerEnvelope {
        payload: Some(worker_envelope::Payload::ComputeBaseFeatures(wrong_length)),
    }));

    let mut decoder_handler = WorkerRequestHandler::new(WorkerPipeline::new(FailAfterSourceRead));
    assert_single_failure(decoder_handler.handle(compute_request(
        &source,
        [0x44; 16],
        BASE_MISSING_PROBE,
        1,
    )));
}

#[test]
fn worker_pipeline_forwards_decoder_threads_to_every_source_decode_session() {
    let directory = tempfile::tempdir().unwrap();
    let source = directory.path().join("thread-budget-video.bin");
    std::fs::write(&source, b"video-source").unwrap();
    let calls = Arc::new(Mutex::new(Vec::new()));
    let decoder = RecordingThreadBudgetDecoder {
        calls: Arc::clone(&calls),
    };
    let mut handler = WorkerRequestHandler::new(WorkerPipeline::new(decoder));
    let mut command = compute_command(
        &source,
        [0x7B; 16],
        BASE_MISSING_PROBE | BASE_MISSING_STAGE1,
        3,
    );
    command.media_kind = proto::MediaKind::MediaVideo as i32;

    let responses = handler.handle(WorkerEnvelope {
        payload: Some(worker_envelope::Payload::ComputeBaseFeatures(command)),
    });

    assert_eq!(responses.len(), 2);
    let calls = calls.lock().unwrap();
    assert_eq!(calls.len(), 7, "视频应执行一次探测和六次顺序抽帧");
    assert_eq!(calls[0], ("probe", 3));
    assert!(calls[1..].iter().all(|call| *call == ("decode", 3)));
}

#[tokio::test]
async fn worker_pool_keeps_one_shot_slot_until_terminal_response() {
    let directory = tempfile::tempdir().unwrap();
    let source = directory.path().join("pool.bin");
    std::fs::write(&source, b"pool").unwrap();
    let (mut pool, _started, control) = WorkerPool::controlled_batch_for_test(1);
    pool.dispatch_runtime(
        compute_request(&source, [0x61; 16], BASE_MISSING_PROBE, 1),
        worker_identity(&source),
    )
    .await
    .unwrap();
    assert!(matches!(
        pool.next_event().await,
        Some(WorkerEvent::Started { .. })
    ));

    control
        .base_source_read_complete("task-a".into(), "item-a".into())
        .await;
    assert!(matches!(
        pool.next_event().await,
        Some(WorkerEvent::BaseSourceReadComplete { task_id, item_id, .. })
            if task_id == "task-a" && item_id == "item-a"
    ));
    assert_eq!(control.available_slots(), 0);
    assert_eq!(pool.busy_workers(), 1);

    control
        .complete_base(
            "task-a".into(),
            "item-a".into(),
            [0x61; 16],
            BaseComputeOutput {
                probe: None,
                stage1_frames: None,
                contact_sheet_jpeg: None,
            },
        )
        .await;
    assert!(matches!(
        pool.next_event().await,
        Some(WorkerEvent::Completed { .. })
    ));
    assert_eq!(control.available_slots(), 1);
    assert_eq!(pool.busy_workers(), 0);
}

/// 从自定义 source 的起点读取一个字节，证明解码实际使用打开句柄。
fn read_marker(source: &mut dyn SeekableMediaSource) -> Result<u8, String> {
    source
        .seek(SeekFrom::Start(0))
        .map_err(|error| error.to_string())?;
    let mut marker = [0_u8; 1];
    let read = source
        .read(&mut marker)
        .map_err(|error| error.to_string())?;
    if read != marker.len() {
        return Err("自定义 source 提前结束".into());
    }
    Ok(marker[0])
}

/// 构造一次性基础计算协议封包。
fn compute_request(
    path: &Path,
    md5: [u8; 16],
    missing_parts: u32,
    decoder_threads: u32,
) -> WorkerEnvelope {
    WorkerEnvelope {
        payload: Some(worker_envelope::Payload::ComputeBaseFeatures(
            compute_command(path, md5, missing_parts, decoder_threads),
        )),
    }
}

/// 构造带固定文件身份和读取预算的一次性基础计算命令。
fn compute_command(
    path: &Path,
    md5: [u8; 16],
    missing_parts: u32,
    decoder_threads: u32,
) -> proto::ComputeBaseFeatures {
    proto::ComputeBaseFeatures {
        task_id: "task-a".into(),
        item_id: "item-a".into(),
        machine_id: "machine-a".into(),
        normalized_path: "i:/media/image.bin".into(),
        display_path: path.to_string_lossy().into_owned(),
        file_size: std::fs::metadata(path).unwrap().len(),
        physical_disk_id: "disk-a".into(),
        md5: md5.to_vec(),
        media_kind: proto::MediaKind::MediaImage as i32,
        missing_parts,
        block_size_bytes: 4,
        block_timeout_ms: 3_000,
        block_retries: 2,
        decoder_threads,
    }
}

/// 验证失败请求只有一个终态响应，并保留任务身份。
fn assert_single_failure(responses: Vec<WorkerEnvelope>) {
    assert_eq!(responses.len(), 1);
    let Some(worker_envelope::Payload::WorkerFailure(failure)) = responses[0].payload.as_ref()
    else {
        panic!("无效请求必须返回 WorkerFailure");
    };
    assert_eq!(failure.task_id, "task-a");
    assert_eq!(failure.item_id, "item-a");
    assert!(!failure.stage.is_empty());
    assert!(!failure.message.is_empty());
}

/// 构造池调度与崩溃恢复所需的冻结文件身份。
fn worker_identity(path: &Path) -> WorkerFileIdentity {
    WorkerFileIdentity {
        machine_id: MachineId::from_sha256([0x71; 32]),
        normalized_path: NormalizedPath::new(r"I:\media\pool.bin").unwrap(),
        display_path: DisplayPath::new(path).unwrap(),
        file_size: std::fs::metadata(path).unwrap().len(),
        stage: "base_compute".into(),
        physical_disk_id: "disk-a".into(),
    }
}
