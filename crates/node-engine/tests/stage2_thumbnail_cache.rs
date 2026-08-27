use std::{
    path::Path,
    sync::{Arc, Mutex},
};

use dedup_core::MediaKind;
use dedup_media_ffmpeg::{DecodedFrame, MediaProbe};
use dedup_node_engine::worker::{
    MediaDecoder, WorkerPipeline, decode_stage2_payload, handle_worker_request,
};
use dedup_protocol::proto::{self, worker_envelope};

#[test]
fn reused_and_regenerated_video_thumbnail_produce_the_same_stage2() {
    let directory = tempfile::tempdir().unwrap();
    let thumbnail = directory.path().join("contact.jpg");
    let first_decoder = CountingDecoder::video();
    let first_calls = Arc::clone(&first_decoder.calls);

    let first = run_video_stage2(first_decoder, &thumbnail);
    let jpeg = first
        .regenerated_contact_sheet_jpeg
        .as_ref()
        .expect("首次必须生成联系表");
    std::fs::write(&thumbnail, jpeg).unwrap();
    assert_eq!(first_calls.lock().unwrap().len(), 6);

    let second_decoder = CountingDecoder::video();
    let second_calls = Arc::clone(&second_decoder.calls);
    let second = run_video_stage2(second_decoder, &thumbnail);

    assert_eq!(first.frames, second.frames);
    assert!(second.regenerated_contact_sheet_jpeg.is_none());
    assert_eq!(second_calls.lock().unwrap().len(), 0);
}

#[test]
fn corrupt_video_thumbnail_falls_back_to_original_video() {
    let directory = tempfile::tempdir().unwrap();
    let thumbnail = directory.path().join("contact.jpg");
    std::fs::write(&thumbnail, b"broken-jpeg").unwrap();
    let decoder = CountingDecoder::video();
    let calls = Arc::clone(&decoder.calls);

    let output = run_video_stage2(decoder, &thumbnail);

    assert!(output.regenerated_contact_sheet_jpeg.is_some());
    assert_eq!(calls.lock().unwrap().len(), 6);
}

#[test]
fn image_stage2_always_reads_original_image() {
    let directory = tempfile::tempdir().unwrap();
    let unrelated_thumbnail = directory.path().join("unrelated.jpg");
    std::fs::write(&unrelated_thumbnail, b"not-used").unwrap();
    let decoder = CountingDecoder::image();
    let calls = Arc::clone(&decoder.calls);
    let response = handle_worker_request(
        &WorkerPipeline::new(decoder),
        envelope(Vec::new(), Path::new(r"D:\photo.jpg"), &unrelated_thumbnail),
    );

    let output = stage2_output(response);
    assert_eq!(output.frames.len(), 1);
    assert_eq!(&*calls.lock().unwrap(), &[0.0]);
}

/// 运行一次请求两个视频槽位的 Worker 二筛。
fn run_video_stage2(
    decoder: CountingDecoder,
    thumbnail: &Path,
) -> dedup_node_engine::worker::Stage2Output {
    stage2_output(handle_worker_request(
        &WorkerPipeline::new(decoder),
        envelope(vec![1, 4], Path::new(r"D:\clip.mp4"), thumbnail),
    ))
}

/// 创建带联系表目标的二筛协议消息。
fn envelope(slots: Vec<u32>, source: &Path, thumbnail: &Path) -> proto::WorkerEnvelope {
    proto::WorkerEnvelope {
        payload: Some(worker_envelope::Payload::ComputeStage2(
            proto::ComputeStage2 {
                task_id: "stage2-task".into(),
                item_id: "stage2-item".into(),
                display_path: source.to_string_lossy().into_owned(),
                frame_slots: slots,
                contact_sheet_path: thumbnail.to_string_lossy().into_owned(),
                generate_contact_sheet_if_missing: true,
            },
        )),
    }
}

/// 从 Worker 响应中恢复二筛结果。
fn stage2_output(response: proto::WorkerEnvelope) -> dedup_node_engine::worker::Stage2Output {
    let Some(worker_envelope::Payload::Stage2Result(result)) = response.payload else {
        panic!("Worker 应返回二筛结果");
    };
    decode_stage2_payload(&result.payload).unwrap()
}

/// 记录原媒体解码次数的固定测试解码器。
struct CountingDecoder {
    media_kind: MediaKind,
    calls: Arc<Mutex<Vec<f64>>>,
}

impl CountingDecoder {
    /// 创建固定视频解码器。
    fn video() -> Self {
        Self {
            media_kind: MediaKind::Video,
            calls: Arc::new(Mutex::new(Vec::new())),
        }
    }

    /// 创建固定图片解码器。
    fn image() -> Self {
        Self {
            media_kind: MediaKind::Image,
            calls: Arc::new(Mutex::new(Vec::new())),
        }
    }
}

impl MediaDecoder for CountingDecoder {
    fn probe_media(&self, _path: &Path) -> Result<MediaProbe, String> {
        Ok(MediaProbe {
            media_kind: self.media_kind,
            width: 8,
            height: 8,
            duration_ms: (self.media_kind == MediaKind::Video).then_some(12_000),
        })
    }

    fn decode_frame_at(&self, _path: &Path, position: f64) -> Result<DecodedFrame, String> {
        self.calls.lock().unwrap().push(position);
        let seed = (position * 100.0) as u8;
        let rgb24 = (0..64)
            .flat_map(|index| {
                let value = seed.wrapping_add(index as u8);
                [value, value.wrapping_add(31), value.wrapping_add(67)]
            })
            .collect();
        Ok(DecodedFrame {
            width: 8,
            height: 8,
            rgb24,
        })
    }
}
