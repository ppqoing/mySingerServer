//! 固定 FFmpeg 8.0.1 ABI 的内部类型与动态函数表。
//!
//! 类型来自已提交的 bindgen 产物；函数签名在这里显式列出，避免构建时依赖
//! LLVM，也让实际使用的 FFmpeg API 保持在一个可审计的小集合内。

use std::ffi::{c_char, c_double, c_int, c_void};

#[allow(
    clippy::type_complexity,
    dead_code,
    missing_docs,
    non_camel_case_types,
    non_snake_case,
    non_upper_case_globals
)]
pub(crate) mod bindings {
    include!("bindings_8_0_1.rs");
}

use bindings::{
    AVCodec, AVCodecContext, AVCodecID, AVDictionary, AVFormatContext, AVFrame, AVIOContext,
    AVInputFormat, AVMediaType, AVPacket, AVPixelFormat, SwsContext,
};

pub(crate) type AvformatAllocContext = unsafe extern "C" fn() -> *mut AVFormatContext;

pub(crate) type AvformatOpenInput = unsafe extern "C" fn(
    *mut *mut AVFormatContext,
    *const c_char,
    *const AVInputFormat,
    *mut *mut AVDictionary,
) -> c_int;
pub(crate) type AvformatFindStreamInfo =
    unsafe extern "C" fn(*mut AVFormatContext, *mut *mut AVDictionary) -> c_int;
pub(crate) type AvFindBestStream = unsafe extern "C" fn(
    *mut AVFormatContext,
    AVMediaType,
    c_int,
    c_int,
    *mut *const AVCodec,
    c_int,
) -> c_int;
pub(crate) type AvformatSeekFile =
    unsafe extern "C" fn(*mut AVFormatContext, c_int, i64, i64, i64, c_int) -> c_int;
pub(crate) type AvReadFrame = unsafe extern "C" fn(*mut AVFormatContext, *mut AVPacket) -> c_int;
pub(crate) type AvformatCloseInput = unsafe extern "C" fn(*mut *mut AVFormatContext);

pub(crate) type AvioReadPacket = unsafe extern "C" fn(*mut c_void, *mut u8, c_int) -> c_int;
pub(crate) type AvioWritePacket = unsafe extern "C" fn(*mut c_void, *const u8, c_int) -> c_int;
pub(crate) type AvioSeek = unsafe extern "C" fn(*mut c_void, i64, c_int) -> i64;
pub(crate) type AvioAllocContext = unsafe extern "C" fn(
    *mut u8,
    c_int,
    c_int,
    *mut c_void,
    Option<AvioReadPacket>,
    Option<AvioWritePacket>,
    Option<AvioSeek>,
) -> *mut AVIOContext;
pub(crate) type AvioContextFree = unsafe extern "C" fn(*mut *mut AVIOContext);
pub(crate) type AvMalloc = unsafe extern "C" fn(usize) -> *mut c_void;
pub(crate) type AvFree = unsafe extern "C" fn(*mut c_void);

pub(crate) type AvcodecFindDecoder = unsafe extern "C" fn(AVCodecID) -> *const AVCodec;
pub(crate) type AvcodecAllocContext3 = unsafe extern "C" fn(*const AVCodec) -> *mut AVCodecContext;
pub(crate) type AvcodecParametersToContext =
    unsafe extern "C" fn(*mut AVCodecContext, *const bindings::AVCodecParameters) -> c_int;
pub(crate) type AvcodecOpen2 =
    unsafe extern "C" fn(*mut AVCodecContext, *const AVCodec, *mut *mut AVDictionary) -> c_int;
pub(crate) type AvOptSetInt = unsafe extern "C" fn(*mut c_void, *const c_char, i64, c_int) -> c_int;
pub(crate) type AvcodecSendPacket =
    unsafe extern "C" fn(*mut AVCodecContext, *const AVPacket) -> c_int;
pub(crate) type AvcodecReceiveFrame =
    unsafe extern "C" fn(*mut AVCodecContext, *mut AVFrame) -> c_int;
pub(crate) type AvcodecFlushBuffers = unsafe extern "C" fn(*mut AVCodecContext);
pub(crate) type AvcodecFreeContext = unsafe extern "C" fn(*mut *mut AVCodecContext);

pub(crate) type AvPacketAlloc = unsafe extern "C" fn() -> *mut AVPacket;
pub(crate) type AvPacketUnref = unsafe extern "C" fn(*mut AVPacket);
pub(crate) type AvPacketFree = unsafe extern "C" fn(*mut *mut AVPacket);
pub(crate) type AvFrameAlloc = unsafe extern "C" fn() -> *mut AVFrame;
pub(crate) type AvFrameUnref = unsafe extern "C" fn(*mut AVFrame);
pub(crate) type AvFrameFree = unsafe extern "C" fn(*mut *mut AVFrame);

pub(crate) type SwsGetContext = unsafe extern "C" fn(
    c_int,
    c_int,
    AVPixelFormat,
    c_int,
    c_int,
    AVPixelFormat,
    c_int,
    *mut std::ffi::c_void,
    *mut std::ffi::c_void,
    *const c_double,
) -> *mut SwsContext;
pub(crate) type SwsScale = unsafe extern "C" fn(
    *mut SwsContext,
    *const *const u8,
    *const c_int,
    c_int,
    c_int,
    *const *mut u8,
    *const c_int,
) -> c_int;
pub(crate) type SwsFreeContext = unsafe extern "C" fn(*mut SwsContext);

/// FFmpeg 8.0.1 中 `EAGAIN` 经 `AVERROR` 转换后的值。
pub(crate) const AVERROR_EAGAIN: c_int = -11;
/// FFmpeg 公共 ABI 中的流结束错误值。
pub(crate) const AVERROR_EOF: c_int = -541_478_725;
/// FFmpeg `AVERROR(EIO)`；自定义 I/O 回调把 Rust I/O 错误映射为此值。
pub(crate) const AVERROR_IO: c_int = -5;
/// FFmpeg `AVERROR(EINVAL)`；自定义 I/O 回调拒绝无效指针、长度或 seek 模式。
pub(crate) const AVERROR_INVALID: c_int = -22;
/// 自定义 AVIO seek 回调查询输入总长度的标志。
pub(crate) const AVSEEK_SIZE: c_int = 0x10000;
/// 自定义 AVIO seek 回调可能附带的强制定位标志。
pub(crate) const AVSEEK_FORCE: c_int = 0x20000;
/// 告知 `AVFormatContext` 的 I/O 由调用方持有和释放。
pub(crate) const AVFMT_FLAG_CUSTOM_IO: c_int = 0x0080;
/// `sws_getContext` 的双线性缩放标志；当前只做等尺寸像素格式转换。
pub(crate) const SWS_BILINEAR: c_int = 2;

/// 运行期解析出的最小函数表。
///
/// 所有字段都在 `Ffmpeg::load_from_worker_executable` 中一次性解析；创建成功后，
/// 上层不会再接触 DLL 句柄或裸符号名。
#[derive(Clone, Copy)]
pub(crate) struct FfmpegApi {
    pub avformat_alloc_context: AvformatAllocContext,
    pub avformat_open_input: AvformatOpenInput,
    pub avformat_find_stream_info: AvformatFindStreamInfo,
    pub av_find_best_stream: AvFindBestStream,
    pub avformat_seek_file: AvformatSeekFile,
    pub av_read_frame: AvReadFrame,
    pub avformat_close_input: AvformatCloseInput,
    pub avio_alloc_context: AvioAllocContext,
    pub avio_context_free: AvioContextFree,
    pub av_malloc: AvMalloc,
    pub av_free: AvFree,
    pub avcodec_find_decoder: AvcodecFindDecoder,
    pub avcodec_alloc_context3: AvcodecAllocContext3,
    pub avcodec_parameters_to_context: AvcodecParametersToContext,
    pub av_opt_set_int: AvOptSetInt,
    pub avcodec_open2: AvcodecOpen2,
    pub avcodec_send_packet: AvcodecSendPacket,
    pub avcodec_receive_frame: AvcodecReceiveFrame,
    pub avcodec_flush_buffers: AvcodecFlushBuffers,
    pub avcodec_free_context: AvcodecFreeContext,
    pub av_packet_alloc: AvPacketAlloc,
    pub av_packet_unref: AvPacketUnref,
    pub av_packet_free: AvPacketFree,
    pub av_frame_alloc: AvFrameAlloc,
    pub av_frame_unref: AvFrameUnref,
    pub av_frame_free: AvFrameFree,
    pub sws_get_context: SwsGetContext,
    pub sws_scale: SwsScale,
    pub sws_free_context: SwsFreeContext,
}
