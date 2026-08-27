//! 单个 Worker 命令内完成解码、灰度转换、特征计算和视频联系表复用。

use std::{path::Path, time::Duration};

use dedup_core::MediaKind;
use dedup_media::{
    ImageStage1, ImageStage2, Rgb24Image, compute_image_stage2, decode_contact_sheet_slots,
    encode_contact_sheet, pdq_hash, rgb24_to_gray, sample_positions,
};
use dedup_media_ffmpeg::{DecodedFrame, Ffmpeg, MediaProbe, SeekableMediaSource};
use dedup_protocol::proto::{self, worker_envelope};
use dedup_protocol::{BASE_MISSING_CONTACT_SHEET, BASE_MISSING_PROBE, BASE_MISSING_STAGE1};
use prost::Message;
use thiserror::Error;

use super::file_session::{WorkerFileSession, WorkerFileSessionError, WorkerReadLimits};

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

    /// 从 Worker 已打开的同一字节流探测媒体；旧测试解码器可显式选择不支持。
    fn probe_source(
        &self,
        _source: &mut dyn SeekableMediaSource,
        _decoder_threads: u32,
    ) -> Result<MediaProbe, String> {
        Err("当前解码器不支持自定义媒体 source".into())
    }

    /// 从 Worker 已打开的同一字节流解码指定位置；旧测试解码器可显式选择不支持。
    fn decode_frame_from_source(
        &self,
        _source: &mut dyn SeekableMediaSource,
        _position: f64,
        _decoder_threads: u32,
    ) -> Result<DecodedFrame, String> {
        Err("当前解码器不支持自定义媒体 source".into())
    }
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

    fn probe_source(
        &self,
        source: &mut dyn SeekableMediaSource,
        decoder_threads: u32,
    ) -> Result<MediaProbe, String> {
        self.ffmpeg
            .probe_source_with_threads(source, decoder_threads)
            .map_err(|error| error.to_string())
    }

    fn decode_frame_from_source(
        &self,
        source: &mut dyn SeekableMediaSource,
        position: f64,
        decoder_threads: u32,
    ) -> Result<DecodedFrame, String> {
        self.ffmpeg
            .decode_frame_from_source_with_threads(source, position, decoder_threads)
            .map_err(|error| error.to_string())
    }
}

/// Node 在 MD5 缓存判定后要求 Worker 继续计算的缺失部分掩码。
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct BaseMissingParts(u32);

impl BaseMissingParts {
    /// 从 V4 协议位掩码创建并拒绝未知位。
    pub fn from_bits(bits: u32) -> Result<Self, WorkerPipelineError> {
        let known = BASE_MISSING_PROBE | BASE_MISSING_STAGE1 | BASE_MISSING_CONTACT_SHEET;
        if bits & !known != 0 {
            return Err(WorkerPipelineError::InvalidPayload(
                "基础计算缺失掩码包含未知位".into(),
            ));
        }
        Ok(Self(bits))
    }

    /// 返回指定部分是否需要计算。
    pub const fn contains(self, part: u32) -> bool {
        self.0 & part != 0
    }

    /// 返回本次续算是否无需读取媒体内容。
    pub const fn is_empty(self) -> bool {
        self.0 == 0
    }
}

/// Worker 按缺失掩码返回的基础媒体计算结果。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct BaseComputeOutput {
    /// 任一媒体部分需要计算时返回的实际探测结果。
    pub probe: Option<MediaProbe>,
    /// 仅 `BASE_MISSING_STAGE1` 置位时返回的一筛槽位。
    pub stage1_frames: Option<Vec<Stage1Frame>>,
    /// 仅视频且 `BASE_MISSING_CONTACT_SHEET` 置位时返回的固定 JPEG。
    pub contact_sheet_jpeg: Option<Vec<u8>>,
}

/// 源文件已关闭、只剩 CPU 特征整理的基础计算中间结果。
struct DecodedBaseInput {
    /// 任一媒体部分需要计算时得到的真实 probe。
    probe: Option<MediaProbe>,
    /// 已经从源读取到内存的图片或视频槽位。
    frames: Option<Vec<DecodedBaseFrame>>,
    /// 是否需要使用成功槽位生成联系表。
    needs_contact_sheet: bool,
    /// 是否需要把解码槽位转换为一筛特征。
    needs_stage1: bool,
}

/// 一个已经结束源读取的拥有型解码槽位。
struct DecodedBaseFrame {
    /// 图片固定为 0，视频为 0 到 5。
    slot: u8,
    /// 成功解码的 RGB 原始帧。
    frame: Option<DecodedFrame>,
    /// 当前槽位解码失败时的原始诊断。
    error: Option<String>,
}

/// 协议循环在 SourceComplete 前后传递的拥有型一次性请求。
pub struct PreparedBaseCompute {
    /// 原请求任务身份。
    task_id: String,
    /// 原请求任务项身份。
    item_id: String,
    /// Node 提供且终态必须原样返回的 MD5。
    md5: [u8; 16],
    /// 决定空 payload 语义的原始缺失掩码。
    missing_parts: u32,
    /// 已关闭源文件后的 CPU-only 输入。
    decoded: DecodedBaseInput,
}

impl PreparedBaseCompute {
    /// 返回协议事件使用的任务身份。
    pub fn task_id(&self) -> &str {
        &self.task_id
    }

    /// 返回协议事件使用的任务项身份。
    pub fn item_id(&self) -> &str {
        &self.item_id
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
    /// 视频联系表缺失或损坏时重建的 JPEG；Node 负责安全发布和写入引用。
    pub regenerated_contact_sheet_jpeg: Option<Vec<u8>>,
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

/// 把两步基础计算的媒体结果编码为 `BaseComputeResult.payload`。
pub fn encode_base_compute_payload(output: &BaseComputeOutput) -> Vec<u8> {
    BaseComputeWire::from(output).encode_to_vec()
}

/// 从 `BaseComputeResult.payload` 恢复基础探测、一筛和联系表结果。
pub fn decode_base_compute_payload(
    payload: &[u8],
) -> Result<BaseComputeOutput, WorkerPipelineError> {
    BaseComputeWire::decode(payload)
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

    /// 探测实际媒体类型并计算一筛；图片解码一次，视频严格按六个固定位置各解码一次。
    pub fn probe_and_stage1(
        &self,
        path: &Path,
        _requested_media_kind: MediaKind,
        generate_contact_sheet: bool,
    ) -> Result<Stage1Output, WorkerPipelineError> {
        let probe = self
            .decoder
            .probe_media(path)
            .map_err(WorkerPipelineError::Decoder)?;
        match probe.media_kind {
            MediaKind::Image => {
                let frame = self
                    .decoder
                    .decode_frame_at(path, 0.0)
                    .map_err(WorkerPipelineError::Decoder)?;
                let (_, feature) = image_stage1(frame)?;
                Ok(Stage1Output {
                    media_kind: MediaKind::Image,
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
            MediaKind::Video => self.video_stage1(path, probe, generate_contact_sheet),
            MediaKind::Other => Ok(Stage1Output {
                media_kind: MediaKind::Other,
                width: probe.width,
                height: probe.height,
                duration_ms: None,
                frames: Vec::new(),
                contact_sheet_jpeg: None,
            }),
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
        Ok(Stage2Output {
            frames,
            regenerated_contact_sheet_jpeg: None,
        })
    }

    /// 一次解码联系表 JPEG，并从请求槽位生成视频二筛联合特征。
    pub fn compute_stage2_from_contact_sheet(
        &self,
        jpeg: &[u8],
        frame_slots: &[u8],
    ) -> Result<Stage2Output, WorkerPipelineError> {
        let decoded = decode_contact_sheet_slots(jpeg, frame_slots)
            .map_err(|error| WorkerPipelineError::ContactSheet(error.to_string()))?;
        if decoded.iter().any(|(_, frame)| {
            frame.width() != CONTACT_CELL_WIDTH || frame.height() != CONTACT_CELL_HEIGHT
        }) {
            return Err(WorkerPipelineError::ContactSheet(
                "联系表必须为固定 960x360 画布".into(),
            ));
        }
        let frames = decoded
            .into_iter()
            .map(|(slot, rgb)| Stage2Frame {
                slot,
                feature: Some(compute_image_stage2(&rgb24_to_gray(&rgb))),
                error: None,
            })
            .collect();
        Ok(Stage2Output {
            frames,
            regenerated_contact_sheet_jpeg: None,
        })
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

    /// 通过 Worker 持有的同一文件会话完成解码，再在源关闭后计算一筛和联系表。
    pub fn compute_base_from_source(
        &self,
        source: &mut dyn SeekableMediaSource,
        _requested_media_kind: MediaKind,
        missing: BaseMissingParts,
        decoder_threads: u32,
    ) -> Result<BaseComputeOutput, WorkerPipelineError> {
        let decoded = self.decode_base_from_source(source, missing, decoder_threads)?;
        self.finish_base_features(decoded)
    }

    /// 只执行会访问源媒体的 probe/解码，返回完整拥有所有权的内存帧。
    fn decode_base_from_source(
        &self,
        source: &mut dyn SeekableMediaSource,
        missing: BaseMissingParts,
        decoder_threads: u32,
    ) -> Result<DecodedBaseInput, WorkerPipelineError> {
        if missing.is_empty() {
            return Ok(DecodedBaseInput {
                probe: None,
                frames: None,
                needs_contact_sheet: false,
                needs_stage1: false,
            });
        }
        let probe = self
            .decoder
            .probe_source(source, decoder_threads)
            .map_err(WorkerPipelineError::Decoder)?;
        let needs_stage1 = missing.contains(BASE_MISSING_STAGE1);
        let needs_contact_sheet =
            missing.contains(BASE_MISSING_CONTACT_SHEET) && probe.media_kind == MediaKind::Video;
        let frames = match probe.media_kind {
            MediaKind::Image if needs_stage1 => {
                let frame = self
                    .decoder
                    .decode_frame_from_source(source, 0.0, decoder_threads)
                    .map_err(WorkerPipelineError::Decoder)?;
                Some(vec![DecodedBaseFrame {
                    slot: 0,
                    frame: Some(frame),
                    error: None,
                }])
            }
            MediaKind::Video if needs_stage1 || needs_contact_sheet => {
                let mut frames = Vec::with_capacity(6);
                for (slot, position) in normalized_sample_positions().into_iter().enumerate() {
                    match self
                        .decoder
                        .decode_frame_from_source(source, position, decoder_threads)
                    {
                        Ok(frame) => frames.push(DecodedBaseFrame {
                            slot: slot as u8,
                            frame: Some(frame),
                            error: None,
                        }),
                        Err(error) => frames.push(DecodedBaseFrame {
                            slot: slot as u8,
                            frame: None,
                            error: Some(error),
                        }),
                    }
                }
                Some(frames)
            }
            _ => None,
        };
        Ok(DecodedBaseInput {
            probe: Some(probe),
            frames,
            needs_contact_sheet,
            needs_stage1,
        })
    }

    /// 在源文件关闭后从拥有型帧计算一筛和联系表，不再访问媒体 source。
    fn finish_base_features(
        &self,
        decoded: DecodedBaseInput,
    ) -> Result<BaseComputeOutput, WorkerPipelineError> {
        let mut stage1_frames = decoded.needs_stage1.then(Vec::new);
        let mut contact_frames: [Option<Rgb24Image>; 6] = std::array::from_fn(|_| None);
        if let Some(frames) = decoded.frames {
            for frame in frames {
                match frame.frame {
                    Some(value) => {
                        let (rgb, feature) = image_stage1(value)?;
                        if decoded.needs_contact_sheet {
                            contact_frames[frame.slot as usize] = Some(rgb);
                        }
                        if let Some(results) = stage1_frames.as_mut() {
                            results.push(Stage1Frame {
                                slot: frame.slot,
                                feature: Some(feature),
                                error: None,
                            });
                        }
                    }
                    None => {
                        if let Some(results) = stage1_frames.as_mut() {
                            results.push(Stage1Frame {
                                slot: frame.slot,
                                feature: None,
                                error: frame.error,
                            });
                        }
                    }
                }
            }
        }
        let contact_sheet_jpeg = decoded
            .needs_contact_sheet
            .then(|| {
                encode_contact_sheet(&contact_frames, CONTACT_CELL_WIDTH, CONTACT_CELL_HEIGHT)
                    .map_err(|error| WorkerPipelineError::ContactSheet(error.to_string()))
            })
            .transpose()?;
        Ok(BaseComputeOutput {
            probe: decoded.probe,
            stage1_frames,
            contact_sheet_jpeg,
        })
    }

    /// 顺序解码六个固定槽位，失败按槽记录，成功 RGB 直接留给联系表编码。
    fn video_stage1(
        &self,
        path: &Path,
        probe: MediaProbe,
        generate_contact_sheet: bool,
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
        let contact_sheet_jpeg = generate_contact_sheet
            .then(|| {
                encode_contact_sheet(&contact_frames, CONTACT_CELL_WIDTH, CONTACT_CELL_HEIGHT)
                    .map_err(|error| WorkerPipelineError::ContactSheet(error.to_string()))
            })
            .transpose()?;
        Ok(Stage1Output {
            media_kind: MediaKind::Video,
            width: probe.width,
            height: probe.height,
            duration_ms: probe.duration_ms,
            frames,
            contact_sheet_jpeg,
        })
    }
}

/// Worker 进程内的一次性请求处理器。
pub struct WorkerRequestHandler<D> {
    /// 无数据库状态的媒体计算流水线。
    pipeline: WorkerPipeline<D>,
}

impl<D> WorkerRequestHandler<D>
where
    D: MediaDecoder,
{
    /// 创建一次只处理一条完整请求的 Worker 请求处理器。
    pub const fn new(pipeline: WorkerPipeline<D>) -> Self {
        Self { pipeline }
    }

    /// 处理一条 Worker 请求并返回有序响应。
    ///
    /// 一次性基础计算成功时先返回源读取完成事件，再返回唯一终态结果；失败时只返回
    /// 一个结构化失败，其他请求保持单响应。
    pub fn handle(&mut self, envelope: proto::WorkerEnvelope) -> Vec<proto::WorkerEnvelope> {
        match envelope.payload.as_ref() {
            Some(worker_envelope::Payload::ComputeBaseFeatures(command)) => {
                match self.prepare_base_features(command.clone()) {
                    Ok(prepared) => {
                        let source_complete =
                            response(worker_envelope::Payload::BaseSourceReadComplete(
                                proto::BaseSourceReadComplete {
                                    task_id: prepared.task_id().into(),
                                    item_id: prepared.item_id().into(),
                                    request_elapsed_us: None,
                                },
                            ));
                        vec![source_complete, self.finish_base_features(prepared)]
                    }
                    Err(failure) => vec![failure],
                }
            }
            _ => vec![handle_worker_request(&self.pipeline, envelope)],
        }
    }

    /// 执行会访问源媒体的 probe/解码并显式关闭文件，供协议循环即时发送 SourceComplete。
    pub fn prepare_base_features(
        &self,
        command: proto::ComputeBaseFeatures,
    ) -> Result<PreparedBaseCompute, proto::WorkerEnvelope> {
        let task_id = command.task_id.clone();
        let item_id = command.item_id.clone();
        let result = (|| {
            validate_base_identity(&command)?;
            let md5: [u8; 16] = command.md5.as_slice().try_into().map_err(|_| {
                WorkerPipelineError::InvalidPayload("基础计算 MD5 必须为 16 字节".into())
            })?;
            let missing = BaseMissingParts::from_bits(command.missing_parts)?;
            let _media_kind = parse_media_kind(command.media_kind)?;
            if command.decoder_threads == 0 {
                return Err(WorkerPipelineError::InvalidPayload(
                    "解码线程预算必须至少为 1".into(),
                ));
            }
            let block_size = usize::try_from(command.block_size_bytes).map_err(|_| {
                WorkerPipelineError::InvalidPayload("读取块大小超过当前平台 usize".into())
            })?;
            let limits =
                WorkerReadLimits::new(block_size, command.block_timeout_ms, command.block_retries)?;
            let mut session = WorkerFileSession::open(Path::new(&command.display_path), limits)?;
            if session.len() != command.file_size {
                return Err(WorkerPipelineError::InvalidPayload(
                    "文件长度与枚举身份不一致".into(),
                ));
            }
            let decoded = self.pipeline.decode_base_from_source(
                session.media_source(),
                missing,
                command.decoder_threads,
            )?;
            // 显式结束源文件生命周期；后续只处理拥有所有权的内存帧。
            drop(session);
            Ok(PreparedBaseCompute {
                task_id: task_id.clone(),
                item_id: item_id.clone(),
                md5,
                missing_parts: command.missing_parts,
                decoded,
            })
        })();
        match result {
            Ok(prepared) => Ok(prepared),
            Err(error) => Err(failure(task_id, item_id, "base_compute", error)),
        }
    }

    /// 在源已关闭后完成 CPU 特征和结果编码，返回唯一终态响应。
    pub fn finish_base_features(&self, prepared: PreparedBaseCompute) -> proto::WorkerEnvelope {
        let task_id = prepared.task_id;
        let item_id = prepared.item_id;
        match self.pipeline.finish_base_features(prepared.decoded) {
            Ok(output) => {
                let payload = if prepared.missing_parts == 0 {
                    Vec::new()
                } else {
                    encode_base_compute_payload(&output)
                };
                response(worker_envelope::Payload::BaseComputeResult(
                    proto::BaseComputeResult {
                        task_id,
                        item_id,
                        md5: prepared.md5.to_vec(),
                        payload,
                    },
                ))
            }
            Err(error) => failure(task_id, item_id, "base_compute", error),
        }
    }
}

/// 校验一次性基础计算跨进程所需的任务、文件和调度身份字段。
fn validate_base_identity(command: &proto::ComputeBaseFeatures) -> Result<(), WorkerPipelineError> {
    let required = [
        (&command.task_id, "task_id"),
        (&command.item_id, "item_id"),
        (&command.machine_id, "machine_id"),
        (&command.normalized_path, "normalized_path"),
        (&command.display_path, "display_path"),
        (&command.physical_disk_id, "physical_disk_id"),
    ];
    if let Some((_, name)) = required.iter().find(|(value, _)| value.is_empty()) {
        return Err(WorkerPipelineError::InvalidPayload(format!(
            "基础计算字段 {name} 不能为空"
        )));
    }
    Ok(())
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
                pipeline.probe_and_stage1(
                    Path::new(&command.display_path),
                    media_kind,
                    command.generate_contact_sheet,
                )
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
                if media_kind == MediaKind::Image || command.contact_sheet_path.is_empty() {
                    return pipeline.compute_stage2(
                        Path::new(&command.display_path),
                        media_kind,
                        &slots,
                    );
                }
                let cached = std::fs::read(&command.contact_sheet_path)
                    .ok()
                    .and_then(|jpeg| {
                        pipeline
                            .compute_stage2_from_contact_sheet(&jpeg, &slots)
                            .ok()
                    });
                if let Some(output) = cached {
                    return Ok(output);
                }
                if !command.generate_contact_sheet_if_missing {
                    return Err(WorkerPipelineError::ContactSheet(
                        "联系表缺失或损坏，且未允许重建".into(),
                    ));
                }
                let jpeg = pipeline.build_contact_sheet(Path::new(&command.display_path), &[])?;
                let mut output = pipeline.compute_stage2_from_contact_sheet(&jpeg, &slots)?;
                output.regenerated_contact_sheet_jpeg = Some(jpeg);
                Ok(output)
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
    /// Worker 单句柄文件会话读取、超时或身份校验失败。
    #[error(transparent)]
    FileSession(#[from] WorkerFileSessionError),
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
/// `BaseComputeResult.payload` 的私有 Protobuf 根消息。
struct BaseComputeWire {
    /// 实际媒体探测结果。
    #[prost(message, optional, tag = "1")]
    probe: Option<MediaProbeWire>,
    /// 区分“未请求一筛”和“请求后得到零个槽位”。
    #[prost(bool, tag = "2")]
    has_stage1: bool,
    /// 请求一筛时返回的槽位结果。
    #[prost(message, repeated, tag = "3")]
    stage1_frames: Vec<Stage1FrameWire>,
    /// 请求视频联系表时返回的 JPEG。
    #[prost(bytes = "vec", optional, tag = "4")]
    contact_sheet_jpeg: Option<Vec<u8>>,
}

#[derive(Clone, PartialEq, Message)]
/// 基础计算实际媒体探测结果的私有 Protobuf 表示。
struct MediaProbeWire {
    /// 领域媒体类型数值。
    #[prost(int32, tag = "1")]
    media_kind: i32,
    /// 探测宽度。
    #[prost(uint32, tag = "2")]
    width: u32,
    /// 探测高度。
    #[prost(uint32, tag = "3")]
    height: u32,
    /// 视频时长；图片为空。
    #[prost(uint64, optional, tag = "4")]
    duration_ms: Option<u64>,
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
    /// Worker 重建后交给 Node 安全发布的联系表 JPEG。
    #[prost(bytes = "vec", optional, tag = "2")]
    regenerated_contact_sheet_jpeg: Option<Vec<u8>>,
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

impl From<&BaseComputeOutput> for BaseComputeWire {
    fn from(output: &BaseComputeOutput) -> Self {
        Self {
            probe: output.probe.as_ref().map(MediaProbeWire::from),
            has_stage1: output.stage1_frames.is_some(),
            stage1_frames: output
                .stage1_frames
                .as_deref()
                .unwrap_or_default()
                .iter()
                .map(Stage1FrameWire::from)
                .collect(),
            contact_sheet_jpeg: output.contact_sheet_jpeg.clone(),
        }
    }
}

impl TryFrom<BaseComputeWire> for BaseComputeOutput {
    type Error = WorkerPipelineError;

    fn try_from(wire: BaseComputeWire) -> Result<Self, Self::Error> {
        let frames = wire
            .stage1_frames
            .into_iter()
            .map(Stage1Frame::try_from)
            .collect::<Result<Vec<_>, _>>()?;
        Ok(Self {
            probe: wire.probe.map(MediaProbe::try_from).transpose()?,
            stage1_frames: wire.has_stage1.then_some(frames),
            contact_sheet_jpeg: wire.contact_sheet_jpeg,
        })
    }
}

impl From<&MediaProbe> for MediaProbeWire {
    fn from(probe: &MediaProbe) -> Self {
        Self {
            media_kind: media_kind_number(probe.media_kind),
            width: probe.width,
            height: probe.height,
            duration_ms: probe.duration_ms,
        }
    }
}

impl TryFrom<MediaProbeWire> for MediaProbe {
    type Error = WorkerPipelineError;

    fn try_from(wire: MediaProbeWire) -> Result<Self, Self::Error> {
        Ok(Self {
            media_kind: parse_media_kind(wire.media_kind)?,
            width: wire.width,
            height: wire.height,
            duration_ms: wire.duration_ms,
        })
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
            regenerated_contact_sheet_jpeg: output.regenerated_contact_sheet_jpeg.clone(),
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
            regenerated_contact_sheet_jpeg: wire.regenerated_contact_sheet_jpeg,
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
