//! Worker 媒体流水线的解码次数、六槽位和联合特征契约。

use std::{
    future::Future,
    path::Path,
    pin::Pin,
    sync::{Arc, Mutex},
    time::Duration,
};

use dedup_core::{DisplayPath, MachineId, MediaKind};
use dedup_media::{Rgb24Image, encode_contact_sheet, sample_positions};
use dedup_media_ffmpeg::{DecodedFrame, MediaProbe};
use dedup_node_engine::io::ReadFailure;
use dedup_node_engine::scan::{
    FileEnumerator, PipelineFileReader, PipelineLimits, ReadProduct, ScanEngine, ScanError,
    ScanOptions, SystemMd5, WorkerPoolStage1Processor, md5_bytes,
};
use dedup_node_engine::worker::WorkerPool;
use dedup_node_engine::worker::{
    BaseComputeOutput, MediaDecoder, Stage1Output, WorkerEvent, WorkerFileIdentity, WorkerPipeline,
    WorkerPoolError, decode_stage1_payload, decode_stage2_payload, encode_stage1_payload,
    encode_stage2_payload, handle_worker_request,
};
use dedup_node_store::{NodeStore, ScannedPath, TaskItemStatus};
use dedup_protocol::{
    BASE_MISSING_PROBE,
    proto::{self, worker_envelope},
};
use dedup_windows::ReadCancellationToken;

#[test]
/// 图片一筛不得为了预览或特征重复解码。
fn image_stage_one_decodes_once_and_has_no_thumbnail() {
    let decoder = FakeDecoder::image();
    let calls = Arc::clone(&decoder.calls);
    let pipeline = WorkerPipeline::new(decoder);

    let output = pipeline
        .probe_and_stage1(Path::new(r"D:\photo.jpg"), MediaKind::Image, true)
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
        .probe_and_stage1(Path::new(r"D:\clip.mp4"), MediaKind::Video, true)
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
/// 视频二筛有可用联系表时只读取联系表，不再解码原视频。
fn stage_two_reuses_existing_contact_sheet_without_video_decode() {
    let directory = tempfile::tempdir().unwrap();
    let contact_path = directory.path().join("cached.jpg");
    let frames: [Option<Rgb24Image>; 6] = std::array::from_fn(|slot| {
        Some(Rgb24Image::new(8, 8, vec![20 + slot as u8 * 20; 8 * 8 * 3]).unwrap())
    });
    std::fs::write(
        &contact_path,
        encode_contact_sheet(&frames, 320, 180).unwrap(),
    )
    .unwrap();
    let decoder = FakeDecoder::video();
    let calls = Arc::clone(&decoder.calls);

    let response = handle_worker_request(
        &WorkerPipeline::new(decoder),
        proto::WorkerEnvelope {
            payload: Some(worker_envelope::Payload::ComputeStage2(
                proto::ComputeStage2 {
                    task_id: "task-contact-hit".into(),
                    item_id: "item-contact-hit".into(),
                    display_path: r"D:\missing-video.mp4".into(),
                    frame_slots: vec![1, 4],
                    contact_sheet_path: contact_path.to_string_lossy().into_owned(),
                    generate_contact_sheet_if_missing: true,
                },
            )),
        },
    );

    let Some(worker_envelope::Payload::Stage2Result(result)) = response.payload else {
        panic!("expected Stage2Result");
    };
    let output = decode_stage2_payload(&result.payload).unwrap();
    assert_eq!(output.frames.len(), 2);
    assert!(output.regenerated_contact_sheet_jpeg.is_none());
    assert!(calls.lock().unwrap().is_empty());
}

#[test]
/// 视频联系表缺失时从原视频重建一次，再基于新联系表计算二筛。
fn stage_two_rebuilds_missing_contact_sheet_before_feature_compute() {
    let directory = tempfile::tempdir().unwrap();
    let contact_path = directory.path().join("missing.jpg");
    let decoder = FakeDecoder::video();
    let calls = Arc::clone(&decoder.calls);

    let response = handle_worker_request(
        &WorkerPipeline::new(decoder),
        proto::WorkerEnvelope {
            payload: Some(worker_envelope::Payload::ComputeStage2(
                proto::ComputeStage2 {
                    task_id: "task-contact-miss".into(),
                    item_id: "item-contact-miss".into(),
                    display_path: r"D:\clip.mp4".into(),
                    frame_slots: vec![1, 4],
                    contact_sheet_path: contact_path.to_string_lossy().into_owned(),
                    generate_contact_sheet_if_missing: true,
                },
            )),
        },
    );

    let Some(worker_envelope::Payload::Stage2Result(result)) = response.payload else {
        panic!("expected Stage2Result");
    };
    let output = decode_stage2_payload(&result.payload).unwrap();
    assert_eq!(output.frames.len(), 2);
    assert!(output.regenerated_contact_sheet_jpeg.is_some());
    assert_eq!(*calls.lock().unwrap(), normalized_positions().to_vec());
}

#[test]
/// 内部 Protobuf 必须无损携带数据库持久化所需的全部数组。
fn worker_payloads_round_trip_all_persisted_features() {
    let pipeline = WorkerPipeline::new(FakeDecoder::video());
    let stage1 = pipeline
        .probe_and_stage1(Path::new(r"D:\clip.mp4"), MediaKind::Video, true)
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
                    generate_contact_sheet: true,
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

#[test]
fn skips_contact_sheet_encoding_when_probe_request_disables_it() {
    let decoder = FakeDecoder::video();
    let calls = Arc::clone(&decoder.calls);
    let response = handle_worker_request(
        &WorkerPipeline::new(decoder),
        proto::WorkerEnvelope {
            payload: Some(worker_envelope::Payload::ProbeAndStage1(
                proto::ProbeAndStage1 {
                    task_id: "task-no-sheet".into(),
                    item_id: "item-no-sheet".into(),
                    display_path: r"D:\clip.mp4".into(),
                    media_kind: proto::MediaKind::MediaOther as i32,
                    generate_contact_sheet: false,
                },
            )),
        },
    );

    let Some(worker_envelope::Payload::Stage1Result(result)) = response.payload else {
        panic!("expected Stage1Result");
    };
    let output = decode_stage1_payload(&result.payload).unwrap();
    assert_eq!(*calls.lock().unwrap(), normalized_positions().to_vec());
    assert_eq!(output.frames.len(), 6);
    assert!(output.contact_sheet_jpeg.is_none());
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
                    generate_contact_sheet: true,
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
                generate_contact_sheet: true,
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
                    generate_contact_sheet: true,
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
async fn base_source_complete_keeps_retained_crash_context_until_terminal_event() {
    let (mut pool, _started, control) = WorkerPool::controlled_batch_for_test(1);
    let full_path = r"I:\媒体库\歌手 A\现场\崩溃样本.mp4";
    let identity = WorkerFileIdentity {
        machine_id: MachineId::from_sha256([0x71; 32]),
        normalized_path: dedup_core::NormalizedPath::new(full_path).unwrap(),
        display_path: DisplayPath::new(full_path).unwrap(),
        file_size: 4096,
        stage: "base_compute".into(),
        physical_disk_id: "disk-7".into(),
    };
    pool.dispatch_runtime(
        base_compute_request("task-base", "item-base", full_path),
        identity,
    )
    .await
    .unwrap();
    assert!(matches!(
        pool.next_event().await,
        Some(WorkerEvent::Started { .. })
    ));

    control
        .base_source_read_complete("task-base".into(), "item-base".into())
        .await;
    assert!(matches!(
        pool.next_event().await,
        Some(WorkerEvent::BaseSourceReadComplete { .. })
    ));

    let running = control.running_files();
    assert_eq!(running.len(), 1);
    assert_eq!(running[0].2.display_path.as_path(), Path::new(full_path));
    assert_eq!(
        running[0].2.stage, "base_compute",
        "SourceComplete 不得推断 Worker phase"
    );

    control
        .crash(
            "task-base".into(),
            "item-base".into(),
            "worker exited".into(),
        )
        .await;
    let Some(WorkerEvent::Crashed { identity, .. }) = pool.next_event().await else {
        panic!("一次性 Worker 崩溃必须返回文件级事件");
    };
    assert_eq!(identity.display_path.as_path(), Path::new(full_path));
    assert_eq!(identity.stage, "base_compute");
    assert!(control.running_files().is_empty());
}

#[tokio::test]
async fn base_compute_crash_keeps_dispatch_path_until_terminal_event() {
    let (mut pool, _started, control) = WorkerPool::controlled_batch_for_test(1);
    let full_path = r"I:\媒体库\歌手 B\首段崩溃.mp4";
    let identity = WorkerFileIdentity {
        machine_id: MachineId::from_sha256([0x72; 32]),
        normalized_path: dedup_core::NormalizedPath::new(full_path).unwrap(),
        display_path: DisplayPath::new(full_path).unwrap(),
        file_size: 4096,
        stage: "base_compute".into(),
        physical_disk_id: "disk-8".into(),
    };
    pool.dispatch_runtime(
        base_compute_request("task-compute", "item-compute", full_path),
        identity,
    )
    .await
    .unwrap();
    assert!(matches!(
        pool.next_event().await,
        Some(WorkerEvent::Started { .. })
    ));

    control
        .crash(
            "task-compute".into(),
            "item-compute".into(),
            "worker exited".into(),
        )
        .await;
    let Some(WorkerEvent::Crashed { identity, .. }) = pool.next_event().await else {
        panic!("一次性基础计算崩溃必须返回文件级事件");
    };
    assert_eq!(identity.display_path.as_path(), Path::new(full_path));
    assert_eq!(identity.stage, "base_compute");
    assert!(control.running_files().is_empty());
}

#[tokio::test]
async fn weighted_scheduler_never_exceeds_cpu_budget_and_acquires_slot_atomically() {
    let (mut pool, mut started, control) =
        WorkerPool::controlled_batch_with_cpu_budget_for_test(4, 3);

    dispatch_weighted_base(
        &pool,
        "weighted",
        "video",
        8_000,
        2,
        proto::MediaKind::MediaVideo,
    )
    .await
    .unwrap();
    dispatch_weighted_base(
        &pool,
        "weighted",
        "image",
        1_000,
        1,
        proto::MediaKind::MediaImage,
    )
    .await
    .unwrap();
    dispatch_weighted_base(
        &pool,
        "weighted",
        "waiting",
        500,
        1,
        proto::MediaKind::MediaOther,
    )
    .await
    .unwrap();

    assert_eq!(started.recv().await.unwrap().1, "video");
    assert_eq!(started.recv().await.unwrap().1, "image");
    assert!(started.try_recv().is_err(), "预算已满时第三项必须留在队列");
    assert_eq!(control.cpu_in_use(), 3);
    assert_eq!(control.available_slots(), 2);

    control
        .complete_base(
            "weighted".into(),
            "video".into(),
            [1; 16],
            empty_base_output(),
        )
        .await;
    assert_eq!(started.recv().await.unwrap().1, "waiting");
    assert!(control.cpu_in_use() <= control.cpu_budget());
    let _ = pool.next_event().await;
}

#[tokio::test]
async fn manual_worker_slots_do_not_expand_cpu_budget() {
    let (pool, mut started, control) = WorkerPool::controlled_batch_with_cpu_budget_for_test(8, 2);
    for index in 0..4 {
        dispatch_weighted_base(
            &pool,
            "manual-slots",
            &format!("item-{index}"),
            1_000,
            1,
            proto::MediaKind::MediaOther,
        )
        .await
        .unwrap();
    }

    let _ = started.recv().await.unwrap();
    let _ = started.recv().await.unwrap();
    assert!(started.try_recv().is_err());
    assert_eq!(control.cpu_in_use(), 2);
    assert_eq!(
        control.available_slots(),
        6,
        "CPU 预算而非进程槽成为当前限制"
    );
}

#[tokio::test]
async fn cost_scheduler_orders_by_weight_file_size_then_sequence() {
    let (pool, mut started, control) = WorkerPool::controlled_batch_with_cpu_budget_for_test(1, 4);
    dispatch_weighted_base(&pool, "cost", "blocker", 1, 1, proto::MediaKind::MediaOther)
        .await
        .unwrap();
    assert_eq!(started.recv().await.unwrap().1, "blocker");

    for (item_id, file_size, threads, media_kind) in [
        ("heavy", 10, 2, proto::MediaKind::MediaVideo),
        ("large-light", 20, 1, proto::MediaKind::MediaOther),
        ("small-first", 10, 1, proto::MediaKind::MediaOther),
        ("small-second", 10, 1, proto::MediaKind::MediaOther),
    ] {
        dispatch_weighted_base(&pool, "cost", item_id, file_size, threads, media_kind)
            .await
            .unwrap();
    }

    let expected = ["small-first", "small-second", "large-light", "heavy"];
    let mut previous = "blocker".to_owned();
    for item_id in expected {
        control
            .complete_base("cost".into(), previous, [2; 16], empty_base_output())
            .await;
        assert_eq!(started.recv().await.unwrap().1, item_id);
        previous = item_id.to_owned();
    }
}

#[tokio::test]
async fn cost_scheduler_remains_work_conserving_when_queue_head_does_not_fit() {
    let (pool, mut started, control) = WorkerPool::controlled_batch_with_cpu_budget_for_test(2, 3);
    dispatch_weighted_base(&pool, "fit", "anchor", 1, 2, proto::MediaKind::MediaVideo)
        .await
        .unwrap();
    assert_eq!(started.recv().await.unwrap().1, "anchor");
    dispatch_weighted_base(&pool, "fit", "head", 1, 2, proto::MediaKind::MediaVideo)
        .await
        .unwrap();
    dispatch_weighted_base(&pool, "fit", "light", 1, 1, proto::MediaKind::MediaOther)
        .await
        .unwrap();

    assert_eq!(started.recv().await.unwrap().1, "light");
    assert_eq!(control.cpu_in_use(), 3);
    assert!(control.running_files().iter().all(|row| row.1 != "head"));
}

#[tokio::test]
async fn aging_reservation_eventually_runs_heavy_job_under_small_job_stream() {
    let (pool, mut started, control) = WorkerPool::controlled_batch_with_cpu_budget_for_test(2, 2);
    dispatch_weighted_base(&pool, "aging", "anchor", 1, 1, proto::MediaKind::MediaOther)
        .await
        .unwrap();
    assert_eq!(started.recv().await.unwrap().1, "anchor");
    dispatch_weighted_base(&pool, "aging", "heavy", 1, 2, proto::MediaKind::MediaVideo)
        .await
        .unwrap();
    dispatch_weighted_base(
        &pool,
        "aging",
        "light-0",
        1,
        1,
        proto::MediaKind::MediaOther,
    )
    .await
    .unwrap();
    assert_eq!(started.recv().await.unwrap().1, "light-0");

    let mut running_light = "light-0".to_owned();
    for index in 1..=7 {
        let next_light = format!("light-{index}");
        dispatch_weighted_base(
            &pool,
            "aging",
            &next_light,
            1,
            1,
            proto::MediaKind::MediaOther,
        )
        .await
        .unwrap();
        control
            .complete_base("aging".into(), running_light, [3; 16], empty_base_output())
            .await;
        assert_eq!(started.recv().await.unwrap().1, next_light);
        running_light = next_light;
    }

    dispatch_weighted_base(
        &pool,
        "aging",
        "light-8",
        1,
        1,
        proto::MediaKind::MediaOther,
    )
    .await
    .unwrap();
    control
        .complete_base("aging".into(), running_light, [4; 16], empty_base_output())
        .await;
    tokio::task::yield_now().await;
    assert!(
        started.try_recv().is_err(),
        "老化重任务保留后不得继续启动轻任务"
    );

    control
        .complete_base(
            "aging".into(),
            "anchor".into(),
            [5; 16],
            empty_base_output(),
        )
        .await;
    assert_eq!(started.recv().await.unwrap().1, "heavy");
}

#[tokio::test]
async fn source_read_complete_keeps_cpu_until_terminal() {
    let (mut pool, mut started, control) =
        WorkerPool::controlled_batch_with_cpu_budget_for_test(1, 2);
    dispatch_weighted_base(&pool, "source", "video", 1, 2, proto::MediaKind::MediaVideo)
        .await
        .unwrap();
    let _ = started.recv().await.unwrap();
    assert_eq!(control.cpu_in_use(), 2);
    assert!(matches!(
        pool.next_event().await,
        Some(WorkerEvent::Started { .. })
    ));

    control
        .base_source_read_complete("source".into(), "video".into())
        .await;
    assert!(matches!(
        pool.next_event().await,
        Some(WorkerEvent::BaseSourceReadComplete { .. })
    ));
    assert_eq!(control.cpu_in_use(), 2, "源读取完成不能释放 CPU 尾段预算");

    control
        .complete_base(
            "source".into(),
            "video".into(),
            [6; 16],
            empty_base_output(),
        )
        .await;
    assert!(matches!(
        pool.next_event().await,
        Some(WorkerEvent::Completed { .. })
    ));
    assert_eq!(control.cpu_in_use(), 0);
}

#[tokio::test]
async fn worker_crash_releases_cpu_weight_once_and_dispatches_waiter() {
    let (mut pool, mut started, control) =
        WorkerPool::controlled_batch_with_cpu_budget_for_test(1, 2);
    dispatch_weighted_base(
        &pool,
        "crash-cpu",
        "first",
        1,
        2,
        proto::MediaKind::MediaVideo,
    )
    .await
    .unwrap();
    dispatch_weighted_base(
        &pool,
        "crash-cpu",
        "waiter",
        1,
        2,
        proto::MediaKind::MediaVideo,
    )
    .await
    .unwrap();
    assert_eq!(started.recv().await.unwrap().1, "first");
    assert!(matches!(
        pool.next_event().await,
        Some(WorkerEvent::Started { .. })
    ));
    assert_eq!(control.cpu_in_use(), 2);

    control
        .crash("crash-cpu".into(), "first".into(), "simulated exit".into())
        .await;
    assert!(matches!(
        pool.next_event().await,
        Some(WorkerEvent::Crashed { .. })
    ));
    assert_eq!(started.recv().await.unwrap().1, "waiter");
    assert_eq!(control.cpu_in_use(), 2);

    control
        .crash("crash-cpu".into(), "first".into(), "duplicate exit".into())
        .await;
    tokio::task::yield_now().await;
    assert_eq!(control.cpu_in_use(), 2, "重复旧退出不得再次扣减当前项 CPU");
}

#[tokio::test]
async fn cancelling_active_work_releases_slots_before_a_new_pool_dispatches() {
    let (pool, mut started, control) = WorkerPool::controlled_batch_with_cpu_budget_for_test(2, 3);
    dispatch_weighted_base(&pool, "restart", "old", 1, 2, proto::MediaKind::MediaVideo)
        .await
        .unwrap();
    assert_eq!(started.recv().await.unwrap().1, "old");
    pool.cancel_task("restart").await.unwrap();
    assert_eq!(control.cpu_in_use(), 0);
    assert_eq!(control.available_slots(), 2);
    let (next_pool, mut next_started, next_control) =
        WorkerPool::controlled_batch_with_cpu_budget_for_test(2, 3);
    dispatch_weighted_base(
        &next_pool,
        "restart-next",
        "new",
        1,
        3,
        proto::MediaKind::MediaVideo,
    )
    .await
    .unwrap();
    assert_eq!(next_started.recv().await.unwrap().1, "new");
    assert_eq!(next_control.cpu_in_use(), 3);
}

#[tokio::test]
async fn invalid_or_oversized_decoder_weight_is_rejected_before_queueing() {
    let (pool, mut started, control) = WorkerPool::controlled_batch_with_cpu_budget_for_test(2, 2);
    let zero = dispatch_weighted_base(&pool, "invalid", "zero", 1, 0, proto::MediaKind::MediaVideo)
        .await
        .unwrap_err();
    assert!(matches!(
        zero,
        WorkerPoolError::InvalidDecoderThreads { .. }
    ));

    let oversized = dispatch_weighted_base(
        &pool,
        "invalid",
        "oversized",
        1,
        3,
        proto::MediaKind::MediaVideo,
    )
    .await
    .unwrap_err();
    assert!(matches!(
        oversized,
        WorkerPoolError::CpuWeightExceedsBudget {
            weight: 3,
            budget: 2
        }
    ));

    let image = dispatch_weighted_base(
        &pool,
        "invalid",
        "image-many",
        1,
        2,
        proto::MediaKind::MediaImage,
    )
    .await
    .unwrap_err();
    assert!(matches!(
        image,
        WorkerPoolError::InvalidDecoderThreads { .. }
    ));
    assert!(started.try_recv().is_err());
    assert_eq!(control.cpu_in_use(), 0);
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
        physical_disk_id: "disk-test".into(),
    }
}

/// 构造只用于 WorkerPool 状态测试的一次性基础计算请求。
fn base_compute_request(task_id: &str, item_id: &str, path: &str) -> proto::WorkerEnvelope {
    proto::WorkerEnvelope {
        payload: Some(worker_envelope::Payload::ComputeBaseFeatures(
            proto::ComputeBaseFeatures {
                task_id: task_id.into(),
                item_id: item_id.into(),
                machine_id: "71".repeat(32),
                normalized_path: path.into(),
                display_path: path.into(),
                file_size: 4096,
                physical_disk_id: "disk-7".into(),
                md5: vec![7; 16],
                media_kind: proto::MediaKind::MediaVideo as i32,
                missing_parts: BASE_MISSING_PROBE,
                block_size_bytes: 64 * 1024,
                block_timeout_ms: 3_000,
                block_retries: 2,
                decoder_threads: 1,
            },
        )),
    }
}

/// 构造带显式文件成本和解码线程预算的基础计算请求。
fn weighted_base_compute_request(
    task_id: &str,
    item_id: &str,
    file_size: u64,
    decoder_threads: u32,
    media_kind: proto::MediaKind,
) -> proto::WorkerEnvelope {
    let path = format!(r"I:\task5\{item_id}.bin");
    let mut envelope = base_compute_request(task_id, item_id, &path);
    let Some(worker_envelope::Payload::ComputeBaseFeatures(command)) = envelope.payload.as_mut()
    else {
        unreachable!("基础请求夹具必须生成 ComputeBaseFeatures")
    };
    command.file_size = file_size;
    command.decoder_threads = decoder_threads;
    command.media_kind = media_kind as i32;
    envelope
}

/// 通过真实 WorkerPool 命令边界派发一个带权基础任务。
async fn dispatch_weighted_base(
    pool: &WorkerPool,
    task_id: &str,
    item_id: &str,
    file_size: u64,
    decoder_threads: u32,
    media_kind: proto::MediaKind,
) -> Result<(), WorkerPoolError> {
    let path = format!(r"I:\task5\{item_id}.bin");
    let mut identity = worker_file_identity(&path);
    identity.file_size = file_size;
    pool.dispatch_runtime(
        weighted_base_compute_request(task_id, item_id, file_size, decoder_threads, media_kind),
        identity,
    )
    .await
}

/// 返回不携带媒体结果的最小终态，供 CPU 生命周期测试释放 Worker。
fn empty_base_output() -> BaseComputeOutput {
    BaseComputeOutput {
        probe: None,
        stage1_frames: None,
        contact_sheet_jpeg: None,
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
