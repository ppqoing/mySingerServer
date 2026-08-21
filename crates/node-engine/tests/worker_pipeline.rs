//! Worker 媒体流水线的解码次数、六槽位和联合特征契约。

use std::{
    path::Path,
    sync::{Arc, Mutex},
    time::Duration,
};

use dedup_core::{DisplayPath, MachineId, MediaKind, TaskId};
use dedup_media::sample_positions;
use dedup_media_ffmpeg::{DecodedFrame, MediaProbe};
use dedup_node_engine::scan::{
    Stage1ProcessError, Stage1Processor, Stage1Request, WorkerPoolStage1Processor,
};
use dedup_node_engine::worker::WorkerPool;
use dedup_node_engine::worker::{
    MediaDecoder, Stage1Output, WorkerEvent, WorkerPipeline, decode_stage1_payload,
    decode_stage2_payload, encode_stage1_payload, encode_stage2_payload, handle_worker_request,
};
use dedup_node_store::{NodeStore, ScannedPath};
use dedup_protocol::proto::{self, worker_envelope};
use dedup_windows::ReadCancellationToken;

#[test]
/// 图片一筛不得为了预览或特征重复解码。
fn image_stage_one_decodes_once_and_has_no_thumbnail() {
    let decoder = FakeDecoder::image();
    let calls = Arc::clone(&decoder.calls);
    let pipeline = WorkerPipeline::new(decoder);

    let output = pipeline
        .probe_and_stage1(Path::new(r"D:\photo.jpg"), MediaKind::Image)
        .unwrap();

    assert_eq!(*calls.lock().unwrap(), vec![0.0]);
    assert_eq!(output.frames.len(), 1);
    assert!(output.frames[0].feature.is_some());
    assert!(output.contact_sheet_jpeg.is_none());
}

#[test]
/// 视频一筛的六次解码结果同时供特征与联系表使用。
fn video_stage_one_reuses_exactly_six_decoded_frames_for_contact_sheet() {
    let decoder = FakeDecoder::video();
    let calls = Arc::clone(&decoder.calls);
    let pipeline = WorkerPipeline::new(decoder);

    let output = pipeline
        .probe_and_stage1(Path::new(r"D:\clip.mp4"), MediaKind::Video)
        .unwrap();

    assert_eq!(*calls.lock().unwrap(), normalized_positions().to_vec());
    assert_eq!(output.frames.len(), 6);
    assert!(output.frames.iter().all(|frame| frame.feature.is_some()));
    assert!(output.contact_sheet_jpeg.is_some());
}

#[test]
/// 请求的每个二筛槽位只解码一次并同时产生两种联合特征。
fn stage_two_computes_phash_and_sobel_from_each_single_decode() {
    let decoder = FakeDecoder::video();
    let calls = Arc::clone(&decoder.calls);
    let pipeline = WorkerPipeline::new(decoder);

    let output = pipeline
        .compute_stage2(Path::new(r"D:\clip.mp4"), MediaKind::Video, &[1, 4])
        .unwrap();

    let positions = normalized_positions();
    assert_eq!(*calls.lock().unwrap(), vec![positions[1], positions[4]]);
    assert_eq!(output.frames.len(), 2);
    assert!(output.frames.iter().all(|frame| frame.feature.is_some()));
    assert_eq!(
        output.frames[0].feature.as_ref().unwrap().phash_parts.len(),
        9
    );
    assert_eq!(output.frames[0].feature.as_ref().unwrap().sobel.len(), 128);
}

#[test]
/// 内部 Protobuf 必须无损携带数据库持久化所需的全部数组。
fn worker_payloads_round_trip_all_persisted_features() {
    let pipeline = WorkerPipeline::new(FakeDecoder::video());
    let stage1 = pipeline
        .probe_and_stage1(Path::new(r"D:\clip.mp4"), MediaKind::Video)
        .unwrap();
    let stage2 = pipeline
        .compute_stage2(Path::new(r"D:\clip.mp4"), MediaKind::Video, &[0, 5])
        .unwrap();

    assert_eq!(
        decode_stage1_payload(&encode_stage1_payload(&stage1)).unwrap(),
        stage1
    );
    assert_eq!(
        decode_stage2_payload(&encode_stage2_payload(&stage2)).unwrap(),
        stage2
    );
}

#[test]
/// Worker 请求转换保留 task/item ID 并返回可解析的一筛响应。
fn worker_envelope_maps_probe_command_to_stage_one_result() {
    let pipeline = WorkerPipeline::new(FakeDecoder::image());
    let response = handle_worker_request(
        &pipeline,
        proto::WorkerEnvelope {
            payload: Some(worker_envelope::Payload::ProbeAndStage1(
                proto::ProbeAndStage1 {
                    task_id: "task-1".into(),
                    item_id: "item-1".into(),
                    display_path: r"D:\photo.jpg".into(),
                    // 扩展名或调用方只能提供候选提示；最终类型必须来自 FFmpeg probe。
                    media_kind: proto::MediaKind::MediaOther as i32,
                },
            )),
        },
    );

    let Some(worker_envelope::Payload::Stage1Result(result)) = response.payload else {
        panic!("expected Stage1Result");
    };
    assert_eq!(result.task_id, "task-1");
    assert_eq!(result.item_id, "item-1");
    assert_eq!(
        decode_stage1_payload(&result.payload).unwrap().media_kind,
        MediaKind::Image
    );
}

#[tokio::test]
async fn cancelled_scan_is_rejected_at_the_worker_pool_send_boundary() {
    let (mut pool, mut started) = WorkerPool::controlled_for_test();
    let cancellation = ReadCancellationToken::new();
    pool.handle().mark_task_cancelled("cancelled-task");
    pool.dispatch_scan(
        proto::WorkerEnvelope {
            payload: Some(worker_envelope::Payload::ProbeAndStage1(
                proto::ProbeAndStage1 {
                    task_id: "cancelled-task".into(),
                    item_id: "cancelled-item".into(),
                    display_path: r"D:\cancelled.bin".into(),
                    media_kind: proto::MediaKind::MediaOther as i32,
                },
            )),
        },
        cancellation,
        true,
    )
    .await
    .unwrap();

    assert!(matches!(
        started.try_recv(),
        Err(tokio::sync::mpsc::error::TryRecvError::Empty)
    ));
    assert!(matches!(
        pool.next_event().await,
        Some(WorkerEvent::Cancelled { task_id, item_id })
            if task_id == "cancelled-task" && item_id == "cancelled-item"
    ));
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn cancel_gate_cannot_cross_the_registry_check_to_slot_send_window() {
    let (pool, mut started, barrier) = WorkerPool::controlled_with_dispatch_barrier_for_test();
    let control = pool.handle();
    let first = proto::WorkerEnvelope {
        payload: Some(worker_envelope::Payload::ProbeAndStage1(
            proto::ProbeAndStage1 {
                task_id: "race-task".into(),
                item_id: "before-cancel".into(),
                display_path: r"D:\before.bin".into(),
                media_kind: proto::MediaKind::MediaOther as i32,
            },
        )),
    };
    let dispatch = tokio::spawn(async move {
        let result = pool
            .dispatch_scan(first, ReadCancellationToken::new(), true)
            .await;
        (pool, result)
    });
    let wait_barrier = barrier.clone();
    tokio::task::spawn_blocking(move || wait_barrier.wait_until_entered())
        .await
        .unwrap();
    let crossed_send_window = control.try_mark_task_cancelled_for_test("race-task");
    barrier.release();
    let (mut pool, result) = dispatch.await.unwrap();
    result.unwrap();
    if !crossed_send_window {
        control.mark_task_cancelled("race-task");
    }

    assert!(
        !crossed_send_window,
        "cancel gate 必须等待已进入的 check+send 临界区收束"
    );
    assert_eq!(
        started.recv().await,
        Some(("race-task".into(), "before-cancel".into()))
    );

    pool.dispatch_scan(
        proto::WorkerEnvelope {
            payload: Some(worker_envelope::Payload::ProbeAndStage1(
                proto::ProbeAndStage1 {
                    task_id: "race-task".into(),
                    item_id: "after-cancel".into(),
                    display_path: r"D:\after.bin".into(),
                    media_kind: proto::MediaKind::MediaOther as i32,
                },
            )),
        },
        ReadCancellationToken::new(),
        true,
    )
    .await
    .unwrap();
    assert!(matches!(
        started.try_recv(),
        Err(tokio::sync::mpsc::error::TryRecvError::Empty)
    ));
    assert!(matches!(
        pool.next_event().await,
        Some(WorkerEvent::Cancelled { item_id, .. }) if item_id == "after-cancel"
    ));
}

#[tokio::test]
async fn crash_fault_fails_file_a_once_while_file_b_completes_and_slot_is_replaced() {
    let (pool, mut started, control) = WorkerPool::controlled_batch_for_test(2);
    let machine = MachineId::from_sha256([0x61; 32]);
    let mut store = NodeStore::open_in_memory(machine).unwrap();
    let task_id = TaskId::new();
    let a = store
        .upsert_content_and_location(
            &ScannedPath::new(
                dedup_core::NormalizedPath::new(r"D:\Crash\a.bin").unwrap(),
                DisplayPath::new(r"D:\Crash\a.bin").unwrap(),
                1,
            ),
            [1; 16],
            MediaKind::Other,
        )
        .unwrap();
    let b = store
        .upsert_content_and_location(
            &ScannedPath::new(
                dedup_core::NormalizedPath::new(r"D:\Crash\b.bin").unwrap(),
                DisplayPath::new(r"D:\Crash\b.bin").unwrap(),
                1,
            ),
            [2; 16],
            MediaKind::Other,
        )
        .unwrap();
    let requests = vec![
        Stage1Request {
            task_id,
            item_id: "file-a".into(),
            display_path: DisplayPath::new(r"D:\Crash\a.bin").unwrap(),
            content_id: a.id,
        },
        Stage1Request {
            task_id,
            item_id: "file-b".into(),
            display_path: DisplayPath::new(r"D:\Crash\b.bin").unwrap(),
            content_id: b.id,
        },
    ];
    let task = tokio::spawn(async move {
        let mut pool = pool;
        let mut processor = WorkerPoolStage1Processor::new(&mut pool, ReadCancellationToken::new());
        processor.process_batch(requests).await
    });
    let mut dispatched = vec![started.recv().await.unwrap(), started.recv().await.unwrap()];
    dispatched.sort();
    assert_eq!(
        dispatched,
        vec![
            (task_id.as_uuid().to_string(), "file-a".into()),
            (task_id.as_uuid().to_string(), "file-b".into())
        ]
    );
    control
        .crash(
            task_id.as_uuid().to_string(),
            "file-a".into(),
            "worker crashed".into(),
        )
        .await;
    control
        .complete(
            task_id.as_uuid().to_string(),
            "file-b".into(),
            Stage1Output {
                media_kind: MediaKind::Other,
                width: 0,
                height: 0,
                duration_ms: None,
                frames: Vec::new(),
                contact_sheet_jpeg: None,
            },
        )
        .await;
    let results = task.await.unwrap();
    let crash = results
        .iter()
        .find(|result| result.item_id == "file-a")
        .unwrap();
    assert!(matches!(
        &crash.output,
        Err(Stage1ProcessError::WorkerCrash(message)) if message == "worker crashed"
    ));
    assert!(
        results
            .iter()
            .find(|result| result.item_id == "file-b")
            .unwrap()
            .output
            .is_ok()
    );
    assert_eq!(control.available_slots(), 2);
    assert!(started.try_recv().is_err(), "崩溃项不得重新派发");
}

/// 把媒体层 Duration 采样定义转换为测试解码器记录的归一化值。
fn normalized_positions() -> [f64; 6] {
    sample_positions(Duration::from_secs(12)).map(|value| value.as_secs_f64() / 12.0)
}

/// 返回固定 8×8 RGB 的可计数解码器。
struct FakeDecoder {
    probe: MediaProbe,
    calls: Arc<Mutex<Vec<f64>>>,
}

impl FakeDecoder {
    /// 创建静态图片探测结果。
    fn image() -> Self {
        Self {
            probe: MediaProbe {
                media_kind: MediaKind::Image,
                width: 8,
                height: 8,
                duration_ms: None,
            },
            calls: Arc::new(Mutex::new(Vec::new())),
        }
    }

    /// 创建十二秒视频探测结果。
    fn video() -> Self {
        Self {
            probe: MediaProbe {
                media_kind: MediaKind::Video,
                width: 8,
                height: 8,
                duration_ms: Some(12_000),
            },
            calls: Arc::new(Mutex::new(Vec::new())),
        }
    }
}

impl MediaDecoder for FakeDecoder {
    fn probe_media(&self, _path: &Path) -> Result<MediaProbe, String> {
        Ok(self.probe.clone())
    }

    fn decode_frame_at(&self, _path: &Path, position: f64) -> Result<DecodedFrame, String> {
        self.calls.lock().unwrap().push(position);
        let rgb24 = (0..8 * 8)
            .flat_map(|index| {
                let value = (index * 3) as u8;
                [value, value.wrapping_add(17), value.wrapping_add(31)]
            })
            .collect();
        Ok(DecodedFrame {
            width: 8,
            height: 8,
            rgb24,
        })
    }
}
