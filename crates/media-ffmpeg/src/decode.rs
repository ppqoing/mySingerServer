//! 将 FFmpeg 裸资源约束在短生命周期会话中的探测与 RGB24 解码。

use std::{
    ffi::{CStr, CString, c_void},
    io::{self, SeekFrom},
    panic::{AssertUnwindSafe, catch_unwind},
    path::Path,
    ptr,
};

use dedup_core::MediaKind;

use crate::{
    ffi::{
        AVERROR_EAGAIN, AVERROR_EOF, AVERROR_INVALID, AVERROR_IO, AVFMT_FLAG_CUSTOM_IO,
        AVSEEK_FORCE, AVSEEK_SIZE, AvOptSetInt, AvcodecOpen2, FfmpegApi, SWS_BILINEAR,
        bindings::{
            AV_TIME_BASE, AVCodec, AVCodecContext, AVFormatContext, AVFrame, AVIOContext,
            AVMediaType_AVMEDIA_TYPE_VIDEO, AVPacket, AVPixelFormat_AV_PIX_FMT_RGB24,
            AVSEEK_FLAG_BACKWARD,
        },
    },
    loader::{Ffmpeg, FfmpegError},
};

/// 从封装与视频流头部读取出的稳定媒体信息。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct MediaProbe {
    /// 输入按静态图片还是视频进入后续特征流程。
    pub media_kind: MediaKind,
    /// 视频流编码宽度。
    pub width: u32,
    /// 视频流编码高度。
    pub height: u32,
    /// 视频时长；静态图片为 `None`。
    pub duration_ms: Option<u64>,
}

/// 紧凑、逐行连续的 RGB24 解码帧。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct DecodedFrame {
    /// 帧宽度。
    pub width: u32,
    /// 帧高度。
    pub height: u32,
    /// 长度严格等于 `width * height * 3` 的 RGB 数据。
    pub rgb24: Vec<u8>,
}

/// FFmpeg 自定义 AVIO 使用的可读、可定位媒体来源。
///
/// 实现负责在 `read` 和 `seek` 内完成自身的超时、重试与取消处理；FFmpeg 只看到
/// 同一个逻辑字节流，不再根据路径重新打开文件。
pub trait SeekableMediaSource {
    /// 从当前位置读取数据并推进来源游标。
    fn read(&mut self, buffer: &mut [u8]) -> io::Result<usize>;
    /// 按标准文件语义移动来源游标并返回新位置。
    fn seek(&mut self, position: SeekFrom) -> io::Result<u64>;
    /// 返回当前来源的固定字节长度。
    fn len(&self) -> u64;

    /// 返回来源是否为空。
    fn is_empty(&self) -> bool {
        self.len() == 0
    }
}

impl Ffmpeg {
    /// 打开媒体并返回类型、尺寸与视频时长，不生成缩略图。
    pub fn probe_media(&self, path: &Path) -> Result<MediaProbe, FfmpegError> {
        probe_session(DecoderSession::open_path(&self.api, path, 1)?)
    }

    /// 从调用方持有的同一可定位来源探测媒体，不接收或重新打开文件路径。
    pub fn probe_source(
        &self,
        source: &mut dyn SeekableMediaSource,
    ) -> Result<MediaProbe, FfmpegError> {
        self.probe_source_with_threads(source, 1)
    }

    /// 使用调用方给定的线程预算从同一可定位来源探测媒体。
    pub fn probe_source_with_threads(
        &self,
        source: &mut dyn SeekableMediaSource,
        decoder_threads: u32,
    ) -> Result<MediaProbe, FfmpegError> {
        probe_session(DecoderSession::open_source(
            &self.api,
            source,
            decoder_threads,
        )?)
    }

    /// 在归一化时间位置解码一帧，并转换为紧凑 RGB24。
    pub fn decode_frame_at(
        &self,
        path: &Path,
        normalized_position: f64,
    ) -> Result<DecodedFrame, FfmpegError> {
        validate_position(normalized_position)?;
        let mut session = DecoderSession::open_path(&self.api, path, 1)?;
        let target_timestamp = session.seek(normalized_position)?;
        session.decode_one(target_timestamp)
    }

    /// 从调用方持有的同一可定位来源解码归一化时间位置的一帧。
    pub fn decode_frame_from_source(
        &self,
        source: &mut dyn SeekableMediaSource,
        normalized_position: f64,
    ) -> Result<DecodedFrame, FfmpegError> {
        self.decode_frame_from_source_with_threads(source, normalized_position, 1)
    }

    /// 使用调用方给定的线程预算从同一可定位来源解码一帧。
    pub fn decode_frame_from_source_with_threads(
        &self,
        source: &mut dyn SeekableMediaSource,
        normalized_position: f64,
        decoder_threads: u32,
    ) -> Result<DecodedFrame, FfmpegError> {
        validate_position(normalized_position)?;
        let mut session = DecoderSession::open_source(&self.api, source, decoder_threads)?;
        let target_timestamp = session.seek(normalized_position)?;
        session.decode_one(target_timestamp)
    }
}

fn probe_session(session: DecoderSession<'_, '_>) -> Result<MediaProbe, FfmpegError> {
    let (width, height) = session.dimensions()?;
    let duration = session.duration();
    // image2 会为单张图片填入一个帧周期的伪时长，媒体类型必须先看实际
    // 解复用器，不能把所有正 duration 都当作视频。
    let duration_ms = (duration > 0 && !session.is_still_image_demuxer())
        .then(|| ((duration as u128 * 1_000) / AV_TIME_BASE as u128).min(u64::MAX as u128) as u64);
    Ok(MediaProbe {
        media_kind: if duration_ms.is_some() {
            MediaKind::Video
        } else {
            MediaKind::Image
        },
        width,
        height,
        duration_ms,
    })
}

fn validate_position(normalized_position: f64) -> Result<(), FfmpegError> {
    if !normalized_position.is_finite() || !(0.0..=1.0).contains(&normalized_position) {
        Err(FfmpegError::InvalidPosition(normalized_position))
    } else {
        Ok(())
    }
}

/// 在 FFmpeg 回调期间固定持有借用的 Rust 媒体来源。
struct SourceBridge<'source> {
    source: &'source mut (dyn SeekableMediaSource + 'source),
}

const AVIO_BUFFER_SIZE: usize = 32 * 1024;

/// 把 FFmpeg 顺序读取请求转交给 Worker 持有的 Rust 文件会话。
unsafe extern "C" fn read_source_callback(
    opaque: *mut c_void,
    buffer: *mut u8,
    buffer_size: i32,
) -> i32 {
    if opaque.is_null() || buffer.is_null() || buffer_size <= 0 {
        return AVERROR_INVALID;
    }
    catch_unwind(AssertUnwindSafe(|| {
        // SAFETY: opaque 指向 DecoderSession 持有的 SourceBridge；回调只在该会话内执行。
        let bridge = unsafe { &mut *opaque.cast::<SourceBridge<'static>>() };
        // SAFETY: FFmpeg 保证可写缓冲区至少有 buffer_size 字节且只在本次回调使用。
        let output = unsafe { std::slice::from_raw_parts_mut(buffer, buffer_size as usize) };
        match bridge.source.read(output) {
            Ok(0) => AVERROR_EOF,
            Ok(read) => i32::try_from(read).unwrap_or(AVERROR_INVALID),
            Err(_) => AVERROR_IO,
        }
    }))
    .unwrap_or(AVERROR_IO)
}

/// 把 FFmpeg 定位和长度查询请求转交给同一个 Rust 文件会话。
unsafe extern "C" fn seek_source_callback(opaque: *mut c_void, offset: i64, whence: i32) -> i64 {
    if opaque.is_null() {
        return i64::from(AVERROR_INVALID);
    }
    catch_unwind(AssertUnwindSafe(|| {
        // SAFETY: opaque 指向 DecoderSession 持有的 SourceBridge；回调只在该会话内执行。
        let bridge = unsafe { &mut *opaque.cast::<SourceBridge<'static>>() };
        if whence & AVSEEK_SIZE != 0 {
            return i64::try_from(bridge.source.len()).unwrap_or(i64::from(AVERROR_INVALID));
        }
        let mode = whence & !AVSEEK_FORCE;
        let position = match mode {
            0 => match u64::try_from(offset) {
                Ok(offset) => SeekFrom::Start(offset),
                Err(_) => return i64::from(AVERROR_INVALID),
            },
            1 => SeekFrom::Current(offset),
            2 => SeekFrom::End(offset),
            _ => return i64::from(AVERROR_INVALID),
        };
        match bridge.source.seek(position) {
            Ok(position) => i64::try_from(position).unwrap_or(i64::from(AVERROR_INVALID)),
            Err(_) => i64::from(AVERROR_IO),
        }
    }))
    .unwrap_or(i64::from(AVERROR_IO))
}

/// 一次媒体打开对应一个会话，所有 FFmpeg 分配对象都在 Drop 中成对释放。
struct DecoderSession<'api, 'source> {
    api: &'api FfmpegApi,
    format: *mut AVFormatContext,
    avio: *mut AVIOContext,
    orphan_avio_buffer: *mut u8,
    source: Option<Box<SourceBridge<'source>>>,
    codec: *mut crate::ffi::bindings::AVCodecContext,
    packet: *mut AVPacket,
    frame: *mut AVFrame,
    stream_index: i32,
}

impl<'api, 'source> DecoderSession<'api, 'source> {
    fn empty(api: &'api FfmpegApi) -> Self {
        Self {
            api,
            format: ptr::null_mut(),
            avio: ptr::null_mut(),
            orphan_avio_buffer: ptr::null_mut(),
            source: None,
            codec: ptr::null_mut(),
            packet: ptr::null_mut(),
            frame: ptr::null_mut(),
            stream_index: -1,
        }
    }

    fn open_path(
        api: &'api FfmpegApi,
        path: &Path,
        decoder_threads: u32,
    ) -> Result<Self, FfmpegError> {
        validate_decoder_threads(decoder_threads)?;
        let path_text = path.to_string_lossy();
        let url = CString::new(path_text.as_bytes())
            .map_err(|_| FfmpegError::PathContainsNul(path.to_path_buf()))?;
        let mut session = Self::empty(api);

        // SAFETY: 输出指针属于 session；其余可选参数按 FFmpeg API 允许传空。
        check("avformat_open_input", unsafe {
            (api.avformat_open_input)(
                &mut session.format,
                url.as_ptr(),
                ptr::null(),
                ptr::null_mut(),
            )
        })?;
        session.initialize(decoder_threads)
    }

    fn open_source(
        api: &'api FfmpegApi,
        source: &'source mut (dyn SeekableMediaSource + 'source),
        decoder_threads: u32,
    ) -> Result<Self, FfmpegError> {
        validate_decoder_threads(decoder_threads)?;
        source
            .seek(SeekFrom::Start(0))
            .map_err(FfmpegError::Source)?;
        let mut session = Self::empty(api);
        session.source = Some(Box::new(SourceBridge { source }));
        // SAFETY: 无输入参数的 FFmpeg format 分配函数。
        session.format = unsafe { (api.avformat_alloc_context)() };
        if session.format.is_null() {
            return Err(FfmpegError::InvalidMedia("cannot allocate format context"));
        }
        // SAFETY: 固定大小非零，返回值由 session Drop 释放。
        session.orphan_avio_buffer = unsafe { (api.av_malloc)(AVIO_BUFFER_SIZE) }.cast();
        if session.orphan_avio_buffer.is_null() {
            return Err(FfmpegError::InvalidMedia("cannot allocate AVIO buffer"));
        }
        let opaque = session
            .source
            .as_mut()
            .expect("source bridge was installed")
            .as_mut() as *mut SourceBridge<'source> as *mut std::ffi::c_void;
        // SAFETY: buffer、opaque 和三个回调在整个 avio/session 生命周期内保持有效。
        session.avio = unsafe {
            (api.avio_alloc_context)(
                session.orphan_avio_buffer,
                AVIO_BUFFER_SIZE as i32,
                0,
                opaque,
                Some(read_source_callback),
                None,
                Some(seek_source_callback),
            )
        };
        if session.avio.is_null() {
            return Err(FfmpegError::InvalidMedia("cannot allocate AVIO context"));
        }
        session.orphan_avio_buffer = ptr::null_mut();
        // SAFETY: format 与 avio 均由 session 持有，custom 标志阻止 format 接管 AVIO 所有权。
        unsafe {
            (*session.format).pb = session.avio;
            (*session.format).flags |= AVFMT_FLAG_CUSTOM_IO;
        }
        // SAFETY: 自定义 AVIO 已放入预分配 format；URL 和可选格式均允许为空。
        check("avformat_open_input(custom_io)", unsafe {
            (api.avformat_open_input)(
                &mut session.format,
                ptr::null(),
                ptr::null(),
                ptr::null_mut(),
            )
        })?;
        session.initialize(decoder_threads)
    }

    fn initialize(mut self, decoder_threads: u32) -> Result<Self, FfmpegError> {
        // SAFETY: format 已由成功的 avformat_open_input 初始化。
        check("avformat_find_stream_info", unsafe {
            (self.api.avformat_find_stream_info)(self.format, ptr::null_mut())
        })?;
        // SAFETY: format 有效；-1 表示自动选择视觉流，decoder_ret 可为空。
        self.stream_index = check("av_find_best_stream", unsafe {
            (self.api.av_find_best_stream)(
                self.format,
                AVMediaType_AVMEDIA_TYPE_VIDEO,
                -1,
                -1,
                ptr::null_mut(),
                0,
            )
        })?;

        let codec_parameters = self.codec_parameters()?;
        // SAFETY: codec_parameters 属于当前 format 且在 session 生命周期内有效。
        let decoder = unsafe { (self.api.avcodec_find_decoder)((*codec_parameters).codec_id) };
        if decoder.is_null() {
            return Err(FfmpegError::InvalidMedia("video decoder is unavailable"));
        }
        // SAFETY: decoder 是 FFmpeg 返回的静态描述符。
        self.codec = unsafe { (self.api.avcodec_alloc_context3)(decoder) };
        if self.codec.is_null() {
            return Err(FfmpegError::InvalidMedia("cannot allocate decoder context"));
        }
        // SAFETY: 两个上下文均有效，函数只复制参数。
        check("avcodec_parameters_to_context", unsafe {
            (self.api.avcodec_parameters_to_context)(self.codec, codec_parameters)
        })?;
        open_decoder_with_threads(
            self.codec,
            decoder,
            decoder_threads,
            self.api.av_opt_set_int,
            self.api.avcodec_open2,
        )?;

        // SAFETY: 无输入参数的 FFmpeg 分配函数。
        self.packet = unsafe { (self.api.av_packet_alloc)() };
        // SAFETY: 无输入参数的 FFmpeg 分配函数。
        self.frame = unsafe { (self.api.av_frame_alloc)() };
        if self.packet.is_null() || self.frame.is_null() {
            return Err(FfmpegError::InvalidMedia(
                "cannot allocate decode packet or frame",
            ));
        }
        Ok(self)
    }

    fn codec_parameters(
        &self,
    ) -> Result<*mut crate::ffi::bindings::AVCodecParameters, FfmpegError> {
        if self.format.is_null() || self.stream_index < 0 {
            return Err(FfmpegError::InvalidMedia("video stream is unavailable"));
        }
        // SAFETY: format 有效，字段由 avformat_find_stream_info 填充。
        let format = unsafe { &*self.format };
        if self.stream_index as u32 >= format.nb_streams || format.streams.is_null() {
            return Err(FfmpegError::InvalidMedia("video stream index is invalid"));
        }
        // SAFETY: 已验证索引小于 nb_streams 且数组非空。
        let stream = unsafe { *format.streams.add(self.stream_index as usize) };
        if stream.is_null() {
            return Err(FfmpegError::InvalidMedia("video stream pointer is null"));
        }
        // SAFETY: stream 由当前 format 持有。
        let parameters = unsafe { (*stream).codecpar };
        if parameters.is_null() {
            return Err(FfmpegError::InvalidMedia(
                "video codec parameters are missing",
            ));
        }
        Ok(parameters)
    }

    fn stream(&self) -> Result<*mut crate::ffi::bindings::AVStream, FfmpegError> {
        let format = if self.format.is_null() {
            return Err(FfmpegError::InvalidMedia("format context is unavailable"));
        } else {
            // SAFETY: 非空且在 session 生命周期内由 FFmpeg 持有。
            unsafe { &*self.format }
        };
        if self.stream_index < 0
            || self.stream_index as u32 >= format.nb_streams
            || format.streams.is_null()
        {
            return Err(FfmpegError::InvalidMedia("video stream index is invalid"));
        }
        // SAFETY: 已验证数组和索引。
        let stream = unsafe { *format.streams.add(self.stream_index as usize) };
        (!stream.is_null())
            .then_some(stream)
            .ok_or(FfmpegError::InvalidMedia("video stream pointer is null"))
    }

    fn dimensions(&self) -> Result<(u32, u32), FfmpegError> {
        let parameters = self.codec_parameters()?;
        // SAFETY: codec_parameters 已验证非空且属于当前 format。
        let (width, height) = unsafe { ((*parameters).width, (*parameters).height) };
        if width <= 0 || height <= 0 {
            return Err(FfmpegError::InvalidMedia("video dimensions are invalid"));
        }
        Ok((width as u32, height as u32))
    }

    fn duration(&self) -> i64 {
        // SAFETY: open 成功后的 session 始终持有非空 format。
        unsafe { (*self.format).duration }
    }

    fn is_still_image_demuxer(&self) -> bool {
        // SAFETY: format 和 iformat 均由成功的 avformat_open_input 持有；name 是
        // FFmpeg 静态、NUL 结尾字符串。空指针只按“不是静态图片”处理。
        unsafe {
            let input = (*self.format).iformat;
            if input.is_null() || (*input).name.is_null() {
                return false;
            }
            CStr::from_ptr((*input).name)
                .to_bytes()
                .split(|byte| *byte == b',')
                .any(|name| name == b"image2" || name.ends_with(b"_pipe"))
        }
    }

    fn seek(&mut self, normalized_position: f64) -> Result<Option<i64>, FfmpegError> {
        if normalized_position <= 0.0 {
            return Ok(None);
        }
        let stream = self.stream()?;
        // SAFETY: stream 已验证且属于当前 format。
        let (time_base, stream_duration) = unsafe { ((*stream).time_base, (*stream).duration) };
        if time_base.num <= 0 || time_base.den <= 0 {
            return Err(FfmpegError::InvalidMedia("video time base is invalid"));
        }
        let target_offset = if stream_duration > 0 {
            (stream_duration as f64 * normalized_position).round() as i64
        } else {
            let duration = self.duration();
            if duration <= 0 {
                return Ok(None);
            }
            let target_us = (duration as f64 * normalized_position).round() as i64;
            (target_us as i128 * time_base.den as i128
                / (AV_TIME_BASE as i128 * time_base.num as i128))
                .clamp(i64::MIN as i128, i64::MAX as i128) as i64
        };
        // SAFETY: stream 已验证且字段只读。
        let start_time = unsafe { (*stream).start_time };
        let target = if start_time == i64::MIN {
            target_offset
        } else {
            start_time.saturating_add(target_offset)
        };
        // SAFETY: format/codec 均有效，时间戳使用该 stream 的 time_base。
        check("avformat_seek_file", unsafe {
            (self.api.avformat_seek_file)(
                self.format,
                self.stream_index,
                i64::MIN,
                target,
                i64::MAX,
                AVSEEK_FLAG_BACKWARD as i32,
            )
        })?;
        // SAFETY: codec 上下文已经打开；seek 后按 FFmpeg 要求清空缓冲。
        unsafe { (self.api.avcodec_flush_buffers)(self.codec) };
        Ok(Some(target))
    }

    /// 持续排空每个 packet 产生的帧，直到到达目标 PTS；目标位于末帧之后时回退到最后一帧。
    fn decode_one(&mut self, target_timestamp: Option<i64>) -> Result<DecodedFrame, FfmpegError> {
        let mut last_before_target = None;
        loop {
            match self.receive_frame()? {
                ReceiveFrame::Frame { timestamp, frame } => {
                    if target_timestamp
                        .is_none_or(|target| timestamp != i64::MIN && timestamp >= target)
                    {
                        return Ok(frame);
                    }
                    last_before_target = Some(frame);
                    continue;
                }
                ReceiveFrame::End => {
                    return last_before_target.ok_or(FfmpegError::InvalidMedia(
                        "decoder produced no visual frame",
                    ));
                }
                ReceiveFrame::NeedPacket => {}
            }

            // SAFETY: format 和 packet 均由当前 session 持有。
            let read = unsafe { (self.api.av_read_frame)(self.format, self.packet) };
            if read < 0 {
                // SAFETY: 空 packet 用于在输入结束后冲刷解码器。
                let sent = unsafe { (self.api.avcodec_send_packet)(self.codec, ptr::null()) };
                if sent < 0 && sent != AVERROR_EOF {
                    return Err(api_error("avcodec_send_packet(flush)", sent));
                }
                continue;
            }

            // SAFETY: av_read_frame 成功后 packet 已初始化。
            let is_video = unsafe { (*self.packet).stream_index == self.stream_index };
            if is_video {
                // SAFETY: codec 和 packet 有效；send 不取得 packet 所有权。
                let sent = unsafe { (self.api.avcodec_send_packet)(self.codec, self.packet) };
                // SAFETY: 每次 read 成功后都恰好 unref 一次以复用 packet。
                unsafe { (self.api.av_packet_unref)(self.packet) };
                if sent < 0 && sent != AVERROR_EAGAIN {
                    return Err(api_error("avcodec_send_packet", sent));
                }
                if sent == AVERROR_EAGAIN {
                    return Err(FfmpegError::InvalidMedia(
                        "decoder refused a packet after its output was drained",
                    ));
                }
            } else {
                // SAFETY: 非目标流 packet 不再使用，立即释放其引用。
                unsafe { (self.api.av_packet_unref)(self.packet) };
            }
        }
    }

    fn receive_frame(&mut self) -> Result<ReceiveFrame, FfmpegError> {
        // SAFETY: codec 已打开，frame 由当前 session 持有并可供 FFmpeg 写入。
        let received = unsafe { (self.api.avcodec_receive_frame)(self.codec, self.frame) };
        match received {
            0 => {
                // SAFETY: receive 成功后该字段属于刚取得的视频帧，单位为流 time_base。
                let timestamp = unsafe { (*self.frame).best_effort_timestamp };
                self.rgb24()
                    .map(|frame| ReceiveFrame::Frame { timestamp, frame })
            }
            AVERROR_EAGAIN => Ok(ReceiveFrame::NeedPacket),
            AVERROR_EOF => Ok(ReceiveFrame::End),
            code => Err(api_error("avcodec_receive_frame", code)),
        }
    }

    fn rgb24(&self) -> Result<DecodedFrame, FfmpegError> {
        // SAFETY: 本方法只在 avcodec_receive_frame 成功后调用。
        let frame = unsafe { &*self.frame };
        if frame.width <= 0 || frame.height <= 0 {
            return Err(FfmpegError::InvalidMedia(
                "decoded frame dimensions are invalid",
            ));
        }
        let width = frame.width as usize;
        let height = frame.height as usize;
        let byte_count = width
            .checked_mul(height)
            .and_then(|pixels| pixels.checked_mul(3))
            .ok_or(FfmpegError::InvalidMedia("decoded frame is too large"))?;
        // SAFETY: 所有尺寸和像素格式来自成功解码的 frame；滤镜指针按 API 允许为空。
        let scaler = unsafe {
            (self.api.sws_get_context)(
                frame.width,
                frame.height,
                frame.format,
                frame.width,
                frame.height,
                AVPixelFormat_AV_PIX_FMT_RGB24,
                SWS_BILINEAR,
                ptr::null_mut(),
                ptr::null_mut(),
                ptr::null(),
            )
        };
        if scaler.is_null() {
            return Err(FfmpegError::InvalidMedia("cannot create RGB24 converter"));
        }

        let mut rgb24 = vec![0; byte_count];
        let mut destination = [ptr::null_mut(); 8];
        destination[0] = rgb24.as_mut_ptr();
        let mut destination_stride = [0_i32; 8];
        destination_stride[0] = (width * 3) as i32;
        let source = frame.data.map(|data| data.cast_const());
        // SAFETY: 源平面由 frame 持有；目标缓冲区大小和 stride 与 RGB24 尺寸一致。
        let scaled = unsafe {
            (self.api.sws_scale)(
                scaler,
                source.as_ptr(),
                frame.linesize.as_ptr(),
                0,
                frame.height,
                destination.as_ptr(),
                destination_stride.as_ptr(),
            )
        };
        // SAFETY: scaler 来自成功的 sws_getContext，且只释放一次。
        unsafe { (self.api.sws_free_context)(scaler) };
        if scaled != frame.height {
            return Err(FfmpegError::InvalidMedia(
                "RGB24 conversion returned an incomplete frame",
            ));
        }
        Ok(DecodedFrame {
            width: frame.width as u32,
            height: frame.height as u32,
            rgb24,
        })
    }
}

/// 解码器一次 receive 的三种状态，避免把需要输入与真正 EOF 混为一谈。
enum ReceiveFrame {
    Frame { timestamp: i64, frame: DecodedFrame },
    NeedPacket,
    End,
}

impl Drop for DecoderSession<'_, '_> {
    fn drop(&mut self) {
        if !self.frame.is_null() {
            // SAFETY: frame 来自 av_frame_alloc；free 接受其地址并置空。
            unsafe { (self.api.av_frame_unref)(self.frame) };
            // SAFETY: 同上，且只在 Drop 中释放一次。
            unsafe { (self.api.av_frame_free)(&mut self.frame) };
        }
        if !self.packet.is_null() {
            // SAFETY: packet 来自 av_packet_alloc；先清引用再释放对象。
            unsafe { (self.api.av_packet_unref)(self.packet) };
            // SAFETY: 同上，且只在 Drop 中释放一次。
            unsafe { (self.api.av_packet_free)(&mut self.packet) };
        }
        if !self.codec.is_null() {
            // SAFETY: codec 来自 avcodec_alloc_context3，地址只传入一次。
            unsafe { (self.api.avcodec_free_context)(&mut self.codec) };
        }
        if !self.format.is_null() {
            // SAFETY: format 来自 avformat_open_input，地址只传入一次。
            unsafe { (self.api.avformat_close_input)(&mut self.format) };
        }
        if !self.avio.is_null() {
            // SAFETY: avio 来自 avio_alloc_context；FFmpeg 可能替换 buffer，因此读取当前字段。
            let buffer = unsafe { (*self.avio).buffer };
            if !buffer.is_null() {
                // SAFETY: 当前 AVIO buffer 由 FFmpeg 的 av_malloc 分配且只在此释放一次。
                unsafe { (self.api.av_free)(buffer.cast()) };
                // SAFETY: 防止 avio_context_free 再看到已释放的 buffer。
                unsafe { (*self.avio).buffer = ptr::null_mut() };
            }
            // SAFETY: avio 来自 avio_alloc_context，地址只传入一次并由函数置空。
            unsafe { (self.api.avio_context_free)(&mut self.avio) };
        } else if !self.orphan_avio_buffer.is_null() {
            // SAFETY: AVIO 创建失败前留下的 buffer 来自 av_malloc，且只在此释放一次。
            unsafe { (self.api.av_free)(self.orphan_avio_buffer.cast()) };
            self.orphan_avio_buffer = ptr::null_mut();
        }
        // AVIO 已停止回调后再释放 bridge，确保其中的 Rust 借用不会悬空。
        self.source.take();
    }
}

/// 在任何 FFmpeg 打开动作前拒绝零线程预算。
fn validate_decoder_threads(decoder_threads: u32) -> Result<(), FfmpegError> {
    if decoder_threads == 0 {
        return Err(FfmpegError::InvalidDecoderThreads(decoder_threads));
    }
    Ok(())
}

/// 严格按“设置 threads AVOption → 打开 decoder”执行，设置失败时不允许静默回退。
fn open_decoder_with_threads(
    codec: *mut AVCodecContext,
    decoder: *const AVCodec,
    decoder_threads: u32,
    av_opt_set_int: AvOptSetInt,
    avcodec_open2: AvcodecOpen2,
) -> Result<(), FfmpegError> {
    validate_decoder_threads(decoder_threads)?;
    // SAFETY: codec 是刚分配并已复制参数的 AVCodecContext；option 名为静态 C 字符串。
    check("av_opt_set_int(threads)", unsafe {
        av_opt_set_int(
            codec.cast::<c_void>(),
            c"threads".as_ptr(),
            i64::from(decoder_threads),
            0,
        )
    })?;
    // SAFETY: codec 与 decoder 匹配，options 按 API 允许传空。
    check("avcodec_open2", unsafe {
        avcodec_open2(codec, decoder, ptr::null_mut())
    })?;
    Ok(())
}

fn check(operation: &'static str, code: i32) -> Result<i32, FfmpegError> {
    if code < 0 {
        Err(api_error(operation, code))
    } else {
        Ok(code)
    }
}

fn api_error(operation: &'static str, code: i32) -> FfmpegError {
    FfmpegError::Api { operation, code }
}

#[cfg(all(test, windows))]
mod tests {
    use std::{
        env,
        ffi::{CStr, c_char, c_int, c_void},
        path::PathBuf,
        ptr,
        sync::{
            Mutex,
            atomic::{AtomicUsize, Ordering},
        },
    };

    use super::*;
    use crate::{
        ffi::bindings::{AVCodec, AVCodecContext, AVDictionary},
        required_dlls,
    };

    /// `av_opt_set_int` 与 `avcodec_open2` 的实际调用顺序。
    static DECODER_OPEN_ORDER: Mutex<Vec<&'static str>> = Mutex::new(Vec::new());
    /// 零线程测试中任一 FFmpeg 打开动作的调用次数。
    static ZERO_THREAD_OPEN_CALLS: AtomicUsize = AtomicUsize::new(0);
    /// option 设置失败测试中 decoder open 的调用次数。
    static FAILED_OPTION_OPEN_CALLS: AtomicUsize = AtomicUsize::new(0);

    unsafe extern "C" fn record_thread_option(
        _object: *mut c_void,
        name: *const c_char,
        value: i64,
        flags: c_int,
    ) -> c_int {
        // SAFETY: 生产 seam 必须传入静态 NUL 结尾的 `threads` 名称。
        assert_eq!(unsafe { CStr::from_ptr(name) }.to_bytes(), b"threads");
        assert_eq!(value, 3);
        assert_eq!(flags, 0);
        DECODER_OPEN_ORDER.lock().unwrap().push("set");
        0
    }

    unsafe extern "C" fn record_decoder_open(
        _codec: *mut AVCodecContext,
        _decoder: *const AVCodec,
        _options: *mut *mut AVDictionary,
    ) -> c_int {
        DECODER_OPEN_ORDER.lock().unwrap().push("open");
        0
    }

    unsafe extern "C" fn count_zero_thread_call(
        _object: *mut c_void,
        _name: *const c_char,
        _value: i64,
        _flags: c_int,
    ) -> c_int {
        ZERO_THREAD_OPEN_CALLS.fetch_add(1, Ordering::SeqCst);
        0
    }

    unsafe extern "C" fn count_zero_thread_open(
        _codec: *mut AVCodecContext,
        _decoder: *const AVCodec,
        _options: *mut *mut AVDictionary,
    ) -> c_int {
        ZERO_THREAD_OPEN_CALLS.fetch_add(1, Ordering::SeqCst);
        0
    }

    unsafe extern "C" fn fail_thread_option(
        _object: *mut c_void,
        _name: *const c_char,
        _value: i64,
        _flags: c_int,
    ) -> c_int {
        -22
    }

    unsafe extern "C" fn count_failed_option_open(
        _codec: *mut AVCodecContext,
        _decoder: *const AVCodec,
        _options: *mut *mut AVDictionary,
    ) -> c_int {
        FAILED_OPTION_OPEN_CALLS.fetch_add(1, Ordering::SeqCst);
        0
    }

    #[test]
    fn decoder_threads_option_is_set_before_avcodec_open2() {
        DECODER_OPEN_ORDER.lock().unwrap().clear();
        let codec = ptr::NonNull::<AVCodecContext>::dangling().as_ptr();
        let decoder = ptr::NonNull::<AVCodec>::dangling().as_ptr().cast_const();

        open_decoder_with_threads(codec, decoder, 3, record_thread_option, record_decoder_open)
            .unwrap();

        assert_eq!(*DECODER_OPEN_ORDER.lock().unwrap(), vec!["set", "open"]);

        FAILED_OPTION_OPEN_CALLS.store(0, Ordering::SeqCst);
        let error = open_decoder_with_threads(
            codec,
            decoder,
            3,
            fail_thread_option,
            count_failed_option_open,
        )
        .unwrap_err();
        assert!(matches!(
            error,
            FfmpegError::Api {
                operation: "av_opt_set_int(threads)",
                code: -22
            }
        ));
        assert_eq!(FAILED_OPTION_OPEN_CALLS.load(Ordering::SeqCst), 0);
    }

    #[test]
    fn zero_decoder_threads_is_rejected_before_ffmpeg_open() {
        ZERO_THREAD_OPEN_CALLS.store(0, Ordering::SeqCst);
        let codec = ptr::NonNull::<AVCodecContext>::dangling().as_ptr();
        let decoder = ptr::NonNull::<AVCodec>::dangling().as_ptr().cast_const();

        let error = open_decoder_with_threads(
            codec,
            decoder,
            0,
            count_zero_thread_call,
            count_zero_thread_open,
        )
        .unwrap_err();

        assert!(matches!(error, FfmpegError::InvalidDecoderThreads(0)));
        assert_eq!(ZERO_THREAD_OPEN_CALLS.load(Ordering::SeqCst), 0);
    }

    /// 后向 seek 返回的首帧通常早于目标；解码边界必须继续推进到目标 PTS。
    #[test]
    fn seeked_decode_reaches_the_requested_stream_timestamp() {
        let Some(source) = env::var_os("DEDUP_FFMPEG_TEST_SOURCE_DIR").map(PathBuf::from) else {
            return;
        };
        let runtime = tempfile::tempdir().unwrap();
        let worker = runtime.path().join("worker.exe");
        let dlls = runtime.path().join("runtime").join("ffmpeg");
        std::fs::create_dir_all(&dlls).unwrap();
        std::fs::write(&worker, []).unwrap();
        for name in required_dlls() {
            std::fs::copy(source.join(name), dlls.join(name)).unwrap();
        }
        let ffmpeg = Ffmpeg::load_from_worker_executable(&worker).unwrap();
        let video = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("..")
            .join("..")
            .join("tests")
            .join("fixtures")
            .join("media")
            .join("video-12s.mp4");
        let mut session = DecoderSession::open_path(&ffmpeg.api, &video, 1).unwrap();
        let stream = session.stream().unwrap();
        // SAFETY: stream 属于仍存活的 session，测试只读取时间字段。
        let (time_base, start_time, stream_duration) = unsafe {
            (
                (*stream).time_base,
                (*stream).start_time,
                (*stream).duration,
            )
        };
        let target_offset = if stream_duration > 0 {
            stream_duration / 2
        } else {
            session.duration() * time_base.den as i64
                / (2 * i64::from(AV_TIME_BASE) * i64::from(time_base.num))
        };
        let target = if start_time == i64::MIN {
            target_offset
        } else {
            start_time.saturating_add(target_offset)
        };

        let actual_target = session.seek(0.5).unwrap();
        assert_eq!(actual_target, Some(target));
        session.decode_one(actual_target).unwrap();
        // SAFETY: decode_one 成功后 frame 保存刚返回帧，时间戳属于视频流 time_base。
        let decoded = unsafe { (*session.frame).best_effort_timestamp };
        assert!(
            decoded >= target,
            "后向 seek 后返回了目标之前的帧: decoded={decoded}, target={target}"
        );
    }
}
