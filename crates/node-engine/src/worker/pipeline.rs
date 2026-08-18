//! 单个 Worker 命令内完成解码、灰度转换、特征计算和视频联系表复用。

use std::{path::Path, time::Duration};

use dedup_core::MediaKind;
use dedup_media::{
    ImageStage1, ImageStage2, Rgb24Image, compute_image_stage2, encode_contact_sheet, pdq_hash,
    rgb24_to_gray, sample_positions,
};
use dedup_media_ffmpeg::{DecodedFrame, Ffmpeg, MediaProbe};
use dedup_protocol::proto::{self, worker_envelope};
use prost::Message;
use thiserror::Error;

/// 联系表单格固定宽度；三列画布为 960 像素。
const CONTACT_CELL_WIDTH: u32 = 320;
/// 联系表单格固定高度；两行画布为 360 像素。
const CONTACT_CELL_HEIGHT: u32 = 180;

/// Worker 流水线依赖的最小媒体解码接口。
///
/// 生产实现只包装固定 DLL 的 `Ffmpeg`；测试实现可以精确记录探测和抽帧次数。
pub trait MediaDecoder {
    /// 读取类型、尺寸和可选视频时长。
    fn probe_media(&self, path: &Path) -> Result<MediaProbe, String>;

    /// 在 `0.0..=1.0` 的归一化位置返回紧凑 RGB24。
    fn decode_frame_at(&self, path: &Path, position: f64) -> Result<DecodedFrame, String>;
}

/// 使用受限 FFmpeg DLL 的生产解码器。
pub struct FfmpegDecoder {
    ffmpeg: Ffmpeg,
}

impl FfmpegDecoder {
    /// 接管已经成功加载并校验符号的 FFmpeg 运行时。
    pub const fn new(ffmpeg: Ffmpeg) -> Self {
        Self { ffmpeg }
    }
}

impl MediaDecoder for FfmpegDecoder {
    fn probe_media(&self, path: &Path) -> Result<MediaProbe, String> {
        self.ffmpeg
            .probe_media(path)
            .map_err(|error| error.to_string())
    }

    fn decode_frame_at(&self, path: &Path, position: f64) -> Result<DecodedFrame, String> {
        self.ffmpeg
            .decode_frame_at(path, position)
            .map_err(|error| error.to_string())
    }
}

/// 一个图片或视频槽位的一筛结果。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Stage1Frame {
    /// 图片固定为 0；视频为 `0..=5` 的六帧槽位。
    pub slot: u8,
    /// 解码成功后的 PDQ、Quality 与尺寸。
    pub feature: Option<ImageStage1>,
    /// 当前槽位解码失败时的简短诊断。
    pub error: Option<String>,
}

/// 一次 ProbeAndStage1 命令的完整拥有所有权结果。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Stage1Output {
    /// 任务声明并用于选择图片/视频流水线的媒体类型。
    pub media_kind: MediaKind,
    /// 探测到的像素宽度。
    pub width: u32,
    /// 探测到的像素高度。
    pub height: u32,
    /// 视频时长；静态图片为 `None`。
    pub duration_ms: Option<u64>,
    /// 图片一个结果或视频六个固定槽位结果。
    pub frames: Vec<Stage1Frame>,
    /// 仅视频生成、且直接复用上述六次抽帧的 JPG 联系表。
    pub contact_sheet_jpeg: Option<Vec<u8>>,
}

/// 一个图片或视频槽位的联合二筛结果。
#[derive(Clone, Debug, PartialEq)]
pub struct Stage2Frame {
    /// 图片固定为 0；视频是请求的六帧槽位。
    pub slot: u8,
    /// 同一次灰度转换共同计算出的九分块 pHash 与 128 维 Sobel。
    pub feature: Option<ImageStage2>,
    /// 当前槽位解码失败时的简短诊断。
    pub error: Option<String>,
}

/// 一次 ComputeStage2 命令的完整结果。
#[derive(Clone, Debug, PartialEq)]
pub struct Stage2Output {
    /// 图片一个结果或视频请求槽位的结果。
    pub frames: Vec<Stage2Frame>,
}

/// 把一筛结果编码为 `Stage1Result.payload` 使用的内部 Protobuf。
pub fn encode_stage1_payload(output: &Stage1Output) -> Vec<u8> {
    Stage1Wire::from(output).encode_to_vec()
}

/// 从 Worker `Stage1Result.payload` 恢复拥有所有权的一筛结果。
pub fn decode_stage1_payload(payload: &[u8]) -> Result<Stage1Output, WorkerPipelineError> {
    Stage1Wire::decode(payload)
        .map_err(|error| WorkerPipelineError::InvalidPayload(error.to_string()))?
        .try_into()
}

/// 把联合二筛结果编码为 `Stage2Result.payload` 使用的内部 Protobuf。
pub fn encode_stage2_payload(output: &Stage2Output) -> Vec<u8> {
    Stage2Wire::from(output).encode_to_vec()
}

/// 从 Worker `Stage2Result.payload` 恢复拥有所有权的联合二筛结果。
pub fn decode_stage2_payload(payload: &[u8]) -> Result<Stage2Output, WorkerPipelineError> {
    Stage2Wire::decode(payload)
        .map_err(|error| WorkerPipelineError::InvalidPayload(error.to_string()))?
        .try_into()
}

/// 无数据库状态的纯 Worker 媒体流水线。
pub struct WorkerPipeline<D> {
    decoder: D,
}

impl<D> WorkerPipeline<D>
where
    D: MediaDecoder,
{
    /// 从一个最小解码器装配媒体流水线。
    pub const fn new(decoder: D) -> Self {
        Self { decoder }
    }

    /// 探测并计算一筛；图片解码一次，视频严格按六个固定位置各解码一次。
    pub fn probe_and_stage1(
        &self,
        path: &Path,
        media_kind: MediaKind,
    ) -> Result<Stage1Output, WorkerPipelineError> {
        let probe = self
            .decoder
            .probe_media(path)
            .map_err(WorkerPipelineError::Decoder)?;
        match media_kind {
            MediaKind::Image => {
                let frame = self
                    .decoder
                    .decode_frame_at(path, 0.0)
                    .map_err(WorkerPipelineError::Decoder)?;
                let (_, feature) = image_stage1(frame)?;
                Ok(Stage1Output {
                    media_kind,
                    width: probe.width,
                    height: probe.height,
                    duration_ms: None,
                    frames: vec![Stage1Frame {
                        slot: 0,
                        feature: Some(feature),
                        error: None,
                    }],
                    contact_sheet_jpeg: None,
                })
            }
            MediaKind::Video => self.video_stage1(path, probe),
            MediaKind::Other => Err(WorkerPipelineError::UnsupportedMedia),
        }
    }

    /// 按请求槽位计算联合二筛；每个槽位只解码和转灰度一次。
    pub fn compute_stage2(
        &self,
        path: &Path,
        media_kind: MediaKind,
        frame_slots: &[u8],
    ) -> Result<Stage2Output, WorkerPipelineError> {
        let slots: Vec<u8> = match media_kind {
            MediaKind::Image => vec![0],
            MediaKind::Video => frame_slots.to_vec(),
            MediaKind::Other => return Err(WorkerPipelineError::UnsupportedMedia),
        };
        let positions = normalized_sample_positions();
        let mut frames = Vec::with_capacity(slots.len());
        for slot in slots {
            let position = match media_kind {
                MediaKind::Image => 0.0,
                MediaKind::Video => *positions
                    .get(slot as usize)
                    .ok_or(WorkerPipelineError::InvalidFrameSlot(slot))?,
                MediaKind::Other => unreachable!("other media returned before slot loop"),
            };
            let result = self.decoder.decode_frame_at(path, position);
            match result {
                Ok(frame) => {
                    let rgb = decoded_rgb(frame)?;
                    let gray = rgb24_to_gray(&rgb);
                    frames.push(Stage2Frame {
                        slot,
                        feature: Some(compute_image_stage2(&gray)),
                        error: None,
                    });
                }
                Err(error) => frames.push(Stage2Frame {
                    slot,
                    feature: None,
                    error: Some(error),
                }),
            }
        }
        Ok(Stage2Output { frames })
    }

    /// 按指定视频槽位生成联系表；未指定时生成全部六个槽位。
    pub fn build_contact_sheet(
        &self,
        path: &Path,
        frame_slots: &[u8],
    ) -> Result<Vec<u8>, WorkerPipelineError> {
        let slots: Vec<u8> = if frame_slots.is_empty() {
            (0..6).collect()
        } else {
            frame_slots.to_vec()
        };
        let positions = normalized_sample_positions();
        let mut frames: [Option<Rgb24Image>; 6] = std::array::from_fn(|_| None);
        for slot in slots {
            let position = *positions
                .get(slot as usize)
                .ok_or(WorkerPipelineError::InvalidFrameSlot(slot))?;
            if let Ok(frame) = self.decoder.decode_frame_at(path, position) {
                frames[slot as usize] = Some(decoded_rgb(frame)?);
            }
        }
        encode_contact_sheet(&frames, CONTACT_CELL_WIDTH, CONTACT_CELL_HEIGHT)
            .map_err(|error| WorkerPipelineError::ContactSheet(error.to_string()))
    }

    /// 顺序解码六个固定槽位，失败按槽记录，成功 RGB 直接留给联系表编码。
    fn video_stage1(
        &self,
        path: &Path,
        probe: MediaProbe,
    ) -> Result<Stage1Output, WorkerPipelineError> {
        let mut contact_frames: [Option<Rgb24Image>; 6] = std::array::from_fn(|_| None);
        let mut frames = Vec::with_capacity(6);
        for (slot, position) in normalized_sample_positions().into_iter().enumerate() {
            match self.decoder.decode_frame_at(path, position) {
                Ok(frame) => {
                    let (rgb, feature) = image_stage1(frame)?;
                    contact_frames[slot] = Some(rgb);
                    frames.push(Stage1Frame {
                        slot: slot as u8,
                        feature: Some(feature),
                        error: None,
                    });
                }
                Err(error) => frames.push(Stage1Frame {
                    slot: slot as u8,
                    feature: None,
                    error: Some(error),
                }),
            }
        }
        let jpeg = encode_contact_sheet(&contact_frames, CONTACT_CELL_WIDTH, CONTACT_CELL_HEIGHT)
            .map_err(|error| WorkerPipelineError::ContactSheet(error.to_string()))?;
        Ok(Stage1Output {
            media_kind: MediaKind::Video,
            width: probe.width,
            height: probe.height,
            duration_ms: probe.duration_ms,
            frames,
            contact_sheet_jpeg: Some(jpeg),
        })
    }
}

/// 执行一个 Worker 请求并始终返回结果或结构化 `WorkerFailure`。
///
/// Worker 主循环只负责 Protobuf 分帧；任务 ID、阶段选择和载荷编码集中在这里，避免
/// `worker.exe` 与进程池各自解释一次协议。
pub fn handle_worker_request<D>(
    pipeline: &WorkerPipeline<D>,
    envelope: proto::WorkerEnvelope,
) -> proto::WorkerEnvelope
where
    D: MediaDecoder,
{
    match envelope.payload {
        Some(worker_envelope::Payload::ProbeAndStage1(command)) => {
            let result = parse_media_kind(command.media_kind).and_then(|media_kind| {
                pipeline.probe_and_stage1(Path::new(&command.display_path), media_kind)
            });
            match result {
                Ok(output) => response(worker_envelope::Payload::Stage1Result(
                    proto::Stage1Result {
                        task_id: command.task_id,
                        item_id: command.item_id,
                        payload: encode_stage1_payload(&output),
                    },
                )),
                Err(error) => failure(command.task_id, command.item_id, "stage1", error),
            }
        }
        Some(worker_envelope::Payload::ComputeStage2(command)) => {
            let slots = parse_slots(&command.frame_slots);
            let result = slots.and_then(|slots| {
                let media_kind = if slots.is_empty() {
                    MediaKind::Image
                } else {
                    MediaKind::Video
                };
                pipeline.compute_stage2(Path::new(&command.display_path), media_kind, &slots)
            });
            match result {
                Ok(output) => response(worker_envelope::Payload::Stage2Result(
                    proto::Stage2Result {
                        task_id: command.task_id,
                        item_id: command.item_id,
                        payload: encode_stage2_payload(&output),
                    },
                )),
                Err(error) => failure(command.task_id, command.item_id, "stage2", error),
            }
        }
        Some(worker_envelope::Payload::BuildContactSheet(command)) => {
            let result = parse_slots(&command.frame_slots).and_then(|slots| {
                pipeline.build_contact_sheet(Path::new(&command.display_path), &slots)
            });
            match result {
                Ok(jpeg) => response(worker_envelope::Payload::ContactSheetResult(
                    proto::ContactSheetResult {
                        task_id: command.task_id,
                        item_id: command.item_id,
                        jpeg,
                        width: CONTACT_CELL_WIDTH * 3,
                        height: CONTACT_CELL_HEIGHT * 2,
                    },
                )),
                Err(error) => failure(command.task_id, command.item_id, "contact_sheet", error),
            }
        }
        _ => failure(
            String::new(),
            String::new(),
            "protocol",
            WorkerPipelineError::InvalidPayload("Worker 只接受三种请求消息".into()),
        ),
    }
}

/// 把一个结果 oneof 包装为完整 WorkerEnvelope。
fn response(payload: worker_envelope::Payload) -> proto::WorkerEnvelope {
    proto::WorkerEnvelope {
        payload: Some(payload),
    }
}

/// 保留请求 task/item ID，把流水线错误转换为可持久化的 WorkerFailure。
fn failure(
    task_id: String,
    item_id: String,
    stage: &'static str,
    error: WorkerPipelineError,
) -> proto::WorkerEnvelope {
    response(worker_envelope::Payload::WorkerFailure(
        proto::WorkerFailure {
            task_id,
            item_id,
            stage: stage.into(),
            message: error.to_string(),
        },
    ))
}

/// 在进程协议边界把 Protobuf u32 槽位收窄为内部 u8。
fn parse_slots(values: &[u32]) -> Result<Vec<u8>, WorkerPipelineError> {
    values
        .iter()
        .map(|value| {
            u8::try_from(*value)
                .map_err(|_| WorkerPipelineError::InvalidPayload("帧槽位超过 u8".into()))
        })
        .collect()
}

/// Worker 流水线在进程协议边界返回的错误。
#[derive(Debug, Error)]
pub enum WorkerPipelineError {
    /// 固定 FFmpeg 探测或解码失败。
    #[error("媒体解码失败: {0}")]
    Decoder(String),
    /// 解码器返回的 RGB24 尺寸与缓冲长度不一致。
    #[error(transparent)]
    InvalidPixels(#[from] dedup_media::MediaError),
    /// 联系表 JPG 编码失败。
    #[error("联系表编码失败: {0}")]
    ContactSheet(String),
    /// Other 文件不进入媒体 Worker 特征计算。
    #[error("普通文件不支持媒体特征计算")]
    UnsupportedMedia,
    /// 视频槽位必须位于固定六帧范围。
    #[error("视频帧槽位无效: {0}")]
    InvalidFrameSlot(u8),
    /// Worker 返回的内部 Protobuf 缺字段或特征长度不符合固定契约。
    #[error("Worker 结果载荷无效: {0}")]
    InvalidPayload(String),
}

/// 从一次解码同时保留 RGB 联系表输入并生成 PDQ/Quality 一筛特征。
fn image_stage1(frame: DecodedFrame) -> Result<(Rgb24Image, ImageStage1), WorkerPipelineError> {
    let rgb = decoded_rgb(frame)?;
    let gray = rgb24_to_gray(&rgb);
    let pdq = pdq_hash(&gray);
    let feature = ImageStage1 {
        width: rgb.width(),
        height: rgb.height(),
        pdq: pdq.hash,
        quality: pdq.quality,
    };
    Ok((rgb, feature))
}

/// 把 FFmpeg 输出转为在构造时一次验证长度的媒体层 RGB 值对象。
fn decoded_rgb(frame: DecodedFrame) -> Result<Rgb24Image, WorkerPipelineError> {
    Ok(Rgb24Image::new(frame.width, frame.height, frame.rgb24)?)
}

/// 复用媒体层时间定义并转换为 FFmpeg 解码接口使用的归一化位置。
fn normalized_sample_positions() -> [f64; 6] {
    const REFERENCE_SECONDS: f64 = 12.0;
    sample_positions(Duration::from_secs(REFERENCE_SECONDS as u64))
        .map(|position| position.as_secs_f64() / REFERENCE_SECONDS)
}

#[derive(Clone, PartialEq, Message)]
/// `Stage1Result.payload` 的私有 Protobuf 根消息。
struct Stage1Wire {
    #[prost(int32, tag = "1")]
    media_kind: i32,
    #[prost(uint32, tag = "2")]
    width: u32,
    #[prost(uint32, tag = "3")]
    height: u32,
    #[prost(uint64, optional, tag = "4")]
    duration_ms: Option<u64>,
    #[prost(message, repeated, tag = "5")]
    frames: Vec<Stage1FrameWire>,
    #[prost(bytes = "vec", optional, tag = "6")]
    contact_sheet_jpeg: Option<Vec<u8>>,
}

#[derive(Clone, PartialEq, Message)]
/// 一个一筛槽位的私有 Protobuf 表示。
struct Stage1FrameWire {
    #[prost(uint32, tag = "1")]
    slot: u32,
    #[prost(message, optional, tag = "2")]
    feature: Option<ImageStage1Wire>,
    #[prost(string, optional, tag = "3")]
    error: Option<String>,
}

#[derive(Clone, PartialEq, Message)]
/// 图片 PDQ/Quality 与尺寸的私有 Protobuf 表示。
struct ImageStage1Wire {
    #[prost(uint32, tag = "1")]
    width: u32,
    #[prost(uint32, tag = "2")]
    height: u32,
    #[prost(bytes = "vec", tag = "3")]
    pdq: Vec<u8>,
    #[prost(uint32, tag = "4")]
    quality: u32,
}

#[derive(Clone, PartialEq, Message)]
/// `Stage2Result.payload` 的私有 Protobuf 根消息。
struct Stage2Wire {
    #[prost(message, repeated, tag = "1")]
    frames: Vec<Stage2FrameWire>,
}

#[derive(Clone, PartialEq, Message)]
/// 一个联合二筛槽位的私有 Protobuf 表示。
struct Stage2FrameWire {
    #[prost(uint32, tag = "1")]
    slot: u32,
    #[prost(message, optional, tag = "2")]
    feature: Option<ImageStage2Wire>,
    #[prost(string, optional, tag = "3")]
    error: Option<String>,
}

#[derive(Clone, PartialEq, Message)]
/// 九块 pHash 与 128 维 Sobel 的私有 Protobuf 表示。
struct ImageStage2Wire {
    #[prost(uint64, repeated, packed = "true", tag = "1")]
    phash_parts: Vec<u64>,
    #[prost(float, repeated, packed = "true", tag = "2")]
    sobel: Vec<f32>,
}

impl From<&Stage1Output> for Stage1Wire {
    fn from(output: &Stage1Output) -> Self {
        Self {
            media_kind: media_kind_number(output.media_kind),
            width: output.width,
            height: output.height,
            duration_ms: output.duration_ms,
            frames: output.frames.iter().map(Stage1FrameWire::from).collect(),
            contact_sheet_jpeg: output.contact_sheet_jpeg.clone(),
        }
    }
}

impl TryFrom<Stage1Wire> for Stage1Output {
    type Error = WorkerPipelineError;

    fn try_from(wire: Stage1Wire) -> Result<Self, Self::Error> {
        Ok(Self {
            media_kind: parse_media_kind(wire.media_kind)?,
            width: wire.width,
            height: wire.height,
            duration_ms: wire.duration_ms,
            frames: wire
                .frames
                .into_iter()
                .map(Stage1Frame::try_from)
                .collect::<Result<_, _>>()?,
            contact_sheet_jpeg: wire.contact_sheet_jpeg,
        })
    }
}

impl From<&Stage1Frame> for Stage1FrameWire {
    fn from(frame: &Stage1Frame) -> Self {
        Self {
            slot: u32::from(frame.slot),
            feature: frame.feature.as_ref().map(ImageStage1Wire::from),
            error: frame.error.clone(),
        }
    }
}

impl TryFrom<Stage1FrameWire> for Stage1Frame {
    type Error = WorkerPipelineError;

    fn try_from(wire: Stage1FrameWire) -> Result<Self, Self::Error> {
        Ok(Self {
            slot: u8::try_from(wire.slot).map_err(|_| invalid_payload("一筛槽位超过 u8"))?,
            feature: wire.feature.map(ImageStage1::try_from).transpose()?,
            error: wire.error,
        })
    }
}

impl From<&ImageStage1> for ImageStage1Wire {
    fn from(feature: &ImageStage1) -> Self {
        Self {
            width: feature.width,
            height: feature.height,
            pdq: feature.pdq.as_bytes().to_vec(),
            quality: u32::from(feature.quality),
        }
    }
}

impl TryFrom<ImageStage1Wire> for ImageStage1 {
    type Error = WorkerPipelineError;

    fn try_from(wire: ImageStage1Wire) -> Result<Self, Self::Error> {
        let pdq: [u8; 32] = wire
            .pdq
            .try_into()
            .map_err(|_| invalid_payload("PDQ 必须为 32 字节"))?;
        Ok(Self {
            width: wire.width,
            height: wire.height,
            pdq: dedup_media::PdqHash::from_bytes(pdq),
            quality: u8::try_from(wire.quality)
                .map_err(|_| invalid_payload("PDQ Quality 超过 u8"))?,
        })
    }
}

impl From<&Stage2Output> for Stage2Wire {
    fn from(output: &Stage2Output) -> Self {
        Self {
            frames: output.frames.iter().map(Stage2FrameWire::from).collect(),
        }
    }
}

impl TryFrom<Stage2Wire> for Stage2Output {
    type Error = WorkerPipelineError;

    fn try_from(wire: Stage2Wire) -> Result<Self, Self::Error> {
        Ok(Self {
            frames: wire
                .frames
                .into_iter()
                .map(Stage2Frame::try_from)
                .collect::<Result<_, _>>()?,
        })
    }
}

impl From<&Stage2Frame> for Stage2FrameWire {
    fn from(frame: &Stage2Frame) -> Self {
        Self {
            slot: u32::from(frame.slot),
            feature: frame.feature.as_ref().map(ImageStage2Wire::from),
            error: frame.error.clone(),
        }
    }
}

impl TryFrom<Stage2FrameWire> for Stage2Frame {
    type Error = WorkerPipelineError;

    fn try_from(wire: Stage2FrameWire) -> Result<Self, Self::Error> {
        Ok(Self {
            slot: u8::try_from(wire.slot).map_err(|_| invalid_payload("二筛槽位超过 u8"))?,
            feature: wire.feature.map(ImageStage2::try_from).transpose()?,
            error: wire.error,
        })
    }
}

impl From<&ImageStage2> for ImageStage2Wire {
    fn from(feature: &ImageStage2) -> Self {
        Self {
            phash_parts: feature.phash_parts.to_vec(),
            sobel: feature.sobel.to_vec(),
        }
    }
}

impl TryFrom<ImageStage2Wire> for ImageStage2 {
    type Error = WorkerPipelineError;

    fn try_from(wire: ImageStage2Wire) -> Result<Self, Self::Error> {
        let phash_parts = wire
            .phash_parts
            .try_into()
            .map_err(|_| invalid_payload("pHash 必须包含九块"))?;
        let sobel: [f32; 128] = wire
            .sobel
            .try_into()
            .map_err(|_| invalid_payload("Sobel 必须包含 128 维"))?;
        if sobel.iter().any(|value| !value.is_finite()) {
            return Err(invalid_payload("Sobel 只允许有限值"));
        }
        Ok(Self { phash_parts, sobel })
    }
}

/// 将领域媒体类型映射到已冻结的内部载荷数值。
const fn media_kind_number(kind: MediaKind) -> i32 {
    match kind {
        MediaKind::Image => 1,
        MediaKind::Video => 2,
        MediaKind::Other => 3,
    }
}

/// 从内部载荷或 Worker 命令解析领域媒体类型。
fn parse_media_kind(value: i32) -> Result<MediaKind, WorkerPipelineError> {
    match value {
        1 => Ok(MediaKind::Image),
        2 => Ok(MediaKind::Video),
        3 => Ok(MediaKind::Other),
        _ => Err(invalid_payload("media_kind 无效")),
    }
}

/// 统一构造内部 Protobuf 契约错误，避免各转换层重复样板。
fn invalid_payload(message: impl Into<String>) -> WorkerPipelineError {
    WorkerPipelineError::InvalidPayload(message.into())
}
