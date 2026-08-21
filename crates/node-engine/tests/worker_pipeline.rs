//! Worker 媒体流水线的解码次数、六槽位和联合特征契约。

use std::{
    future::Future,
    path::Path,
    pin::Pin,
    sync::{Arc, Mutex},
    time::Duration,
};

use dedup_core::{DisplayPath, MachineId, MediaKind};
use dedup_media::sample_positions;
use dedup_media_ffmpeg::{DecodedFrame, MediaProbe};
use dedup_node_engine::io::ReadFailure;
use dedup_node_engine::scan::{
    FileEnumerator, PipelineFileReader, PipelineLimits, ReadProduct, ScanEngine, ScanError,
    ScanOptions, SystemMd5, WorkerPoolStage1Processor, md5_bytes,
};
use dedup_node_engine::worker::WorkerPool;
use dedup_node_engine::worker::{
    MediaDecoder, Stage1Output, WorkerEvent, WorkerFileIdentity, WorkerPipeline,
    decode_stage1_payload, decode_stage2_payload, encode_stage1_payload, encode_stage2_payload,
    handle_worker_request,
};
use dedup_node_store::{NodeStore, ScannedPath, TaskItemStatus};
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
        worker_file_identity(r"D:\cancelled.bin"),
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
            .dispatch_scan(
                first,
                ReadCancellationToken::new(),
                true,
                worker_file_identity(r"D:\before.bin"),
            )
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
        worker_file_identity(r"D:\after.bin"),
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
    let directory = tempfile::tempdir().unwrap();
    let path_a = directory.path().join("a.bin");
    let path_b = directory.path().join("b.bin");
    std::fs::write(&path_a, b"a").unwrap();
    std::fs::write(&path_b, b"b").unwrap();
    let rows = vec![scanned(&path_a), scanned(&path_b)];
    let enumerator = CrashEnumerator { rows };
    let machine = MachineId::from_sha256([0x61; 32]);
    let store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let root = DisplayPath::new(directory.path()).unwrap();
    let sheets = directory.path().join("sheets");
    let task = tokio::spawn(async move {
        let mut store = store;
        let mut pool = pool;
        let mut processor = WorkerPoolStage1Processor::new(&mut pool, ReadCancellationToken::new());
        let mut engine = ScanEngine::new(enumerator, SystemMd5, sheets);
        let result = engine
            .run_parallel_with(
                &mut store,
                ScanOptions::new(vec![root]),
                ImmediatePipelineReader,
                &mut processor,
                PipelineLimits::new(2, 2),
                ReadCancellationToken::new(),
                10,
            )
            .await;
        (store, result)
    });
    let _ = started.recv().await.unwrap();
    let _ = started.recv().await.unwrap();
    let running = control.running_files();
    assert_eq!(running.len(), 2);
    let a = running
        .iter()
        .find(|(_, _, identity)| identity.display_path.as_path() == path_a)
        .unwrap()
        .clone();
    let b = running
        .iter()
        .find(|(_, _, identity)| identity.display_path.as_path() == path_b)
        .unwrap()
        .clone();
    control
        .crash(a.0.clone(), a.1.clone(), "worker crashed".into())
        .await;
    control
        .complete(
            b.0.clone(),
            b.1.clone(),
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
    let (store, summary) = task.await.unwrap();
    let summary = summary.unwrap();
    let items = store.task_items(summary.task_id).unwrap();
    assert_eq!(
        items
            .iter()
            .find(|item| item.item_id == a.1)
            .unwrap()
            .status,
        TaskItemStatus::Failed
    );
    assert_eq!(
        items
            .iter()
            .find(|item| item.item_id == b.1)
            .unwrap()
            .status,
        TaskItemStatus::Succeeded
    );
    let faults = store.page_file_faults(None, 10).unwrap();
    assert_eq!(faults.items.len(), 1);
    assert_eq!(faults.items[0].machine_id, machine);
    assert_eq!(faults.items[0].normalized_path, a.2.normalized_path);
    assert_eq!(faults.items[0].display_path, a.2.display_path);
    assert_eq!(faults.items[0].file_size, a.2.file_size);
    assert_eq!(faults.items[0].stage, a.2.stage);
    assert_eq!(control.available_slots(), 2);
    assert!(started.try_recv().is_err(), "崩溃项不得重新派发");
}

#[derive(Clone)]
struct CrashEnumerator {
    rows: Vec<ScannedPath>,
}

impl FileEnumerator for CrashEnumerator {
    fn enumerate(&self, _roots: &[DisplayPath]) -> Result<Vec<ScannedPath>, ScanError> {
        Ok(self.rows.clone())
    }
}

#[derive(Clone, Copy)]
struct ImmediatePipelineReader;

impl PipelineFileReader for ImmediatePipelineReader {
    type Lease = ();

    fn read(
        &self,
        scanned: ScannedPath,
        cancellation: ReadCancellationToken,
    ) -> Pin<Box<dyn Future<Output = Result<ReadProduct<Self::Lease>, ReadFailure>> + Send + 'static>>
    {
        Box::pin(async move {
            if cancellation.is_cancelled() {
                return Err(ReadFailure::Cancelled);
            }
            let data = std::fs::read(scanned.display_path.as_path()).map_err(|source| {
                ReadFailure::Io {
                    path: scanned.display_path.as_path().to_path_buf(),
                    block_offset: 0,
                    source,
                }
            })?;
            Ok(ReadProduct {
                md5: md5_bytes(&data),
                lease: (),
            })
        })
    }
}

fn scanned(path: &Path) -> ScannedPath {
    ScannedPath::new(
        dedup_core::NormalizedPath::new(path).unwrap(),
        DisplayPath::new(path).unwrap(),
        std::fs::metadata(path).unwrap().len(),
    )
}

/// 把媒体层 Duration 采样定义转换为测试解码器记录的归一化值。
fn normalized_positions() -> [f64; 6] {
    sample_positions(Duration::from_secs(12)).map(|value| value.as_secs_f64() / 12.0)
}

fn worker_file_identity(path: &str) -> WorkerFileIdentity {
    WorkerFileIdentity {
        machine_id: MachineId::from_sha256([0x60; 32]),
        normalized_path: dedup_core::NormalizedPath::new(path).unwrap(),
        display_path: DisplayPath::new(path).unwrap(),
        file_size: 1,
        stage: "probe_stage1".into(),
    }
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
