#define WIN32_LEAN_AND_MEAN
#include <windows.h>

#include "video_analysis.h"

#include <algorithm>
#include <array>
#include <atomic>
#include <chrono>
#include <cmath>
#include <cstring>
#include <limits>
#include <memory>
#include <string>

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/display.h>
#include <libavutil/error.h>
#include <libavutil/imgutils.h>
#include <libavutil/mathematics.h>
#include <libswscale/swscale.h>
}

#include "avio_bridge.h"
#include "contact_sheet.h"
#include "deadline.h"
#include "error.h"
#include "native_algorithms/gray_image.h"
#include "native_algorithms/pdq.h"
#include "native_algorithms/phash_parts.h"
#include "native_algorithms/sobel_hist.h"

namespace vc::detail {

namespace {

#if defined(VC_RESILIENCE_TESTING)
std::atomic<uint64_t> live_formats{0u};
std::atomic<uint64_t> live_codecs{0u};
std::atomic<uint64_t> live_packets{0u};
std::atomic<uint64_t> live_frames{0u};
std::atomic<uint64_t> live_scalers{0u};
std::atomic<uint64_t> acquired_formats{0u};
std::atomic<uint64_t> acquired_codecs{0u};
std::atomic<uint64_t> acquired_packets{0u};
std::atomic<uint64_t> acquired_frames{0u};
std::atomic<uint64_t> acquired_scalers{0u};
#endif

int64_t SaturatingAdd(int64_t left, int64_t right) noexcept {
    constexpr int64_t maximum = (std::numeric_limits<int64_t>::max)();
    constexpr int64_t minimum = (std::numeric_limits<int64_t>::min)();
    if (right > 0 && left > maximum - right) return maximum;
    if (right < 0 && left < minimum - right) return minimum;
    return left + right;
}

int64_t SaturatingSubtract(int64_t left, int64_t right) noexcept {
    constexpr int64_t maximum = (std::numeric_limits<int64_t>::max)();
    constexpr int64_t minimum = (std::numeric_limits<int64_t>::min)();
    if (right > 0 && left < minimum + right) return minimum;
    if (right < 0 && left > maximum + right) return maximum;
    return left - right;
}

}  // namespace

#if defined(VC_VIDEO_ANALYSIS_TESTING)
namespace {
VideoAnalysisTestStats video_test_stats;
bool video_test_force_send_eagain_once = false;
bool video_test_force_timeout_before_publish_once = false;
bool video_test_force_timeout_after_contact_write_once = false;
bool video_test_force_contact_delete_failure_once = false;
int32_t video_test_hard_read_failure_slot = -1;
int32_t video_test_hard_seek_failure_slot = -1;
int32_t video_test_read_error = 0;
int32_t video_test_seek_error = 0;
uint32_t video_test_read_error_repetitions = 0u;
uint32_t video_test_seek_error_repetitions = 0u;
constexpr uint32_t kVideoTestReadPlanCapacity = 32u;
std::array<int32_t, kVideoTestReadPlanCapacity> video_test_read_plan{};
uint32_t video_test_read_plan_size = 0u;
uint32_t video_test_read_plan_cursor = 0u;
int32_t video_test_read_plan_slot = -1;
bool video_test_stream_start_override_enabled = false;
int64_t video_test_stream_start_override = 0;
VideoAnalysisBeforePublishHook video_test_before_publish_hook = nullptr;
void* video_test_before_publish_context = nullptr;
VideoAnalysisAfterContactWriteHook video_test_after_contact_write_hook = nullptr;
void* video_test_after_contact_write_context = nullptr;
}

void VideoAnalysisTestReset() noexcept {
    video_test_stats = {};
    video_test_force_send_eagain_once = false;
    video_test_force_timeout_before_publish_once = false;
    video_test_force_timeout_after_contact_write_once = false;
    video_test_force_contact_delete_failure_once = false;
    video_test_hard_read_failure_slot = -1;
    video_test_hard_seek_failure_slot = -1;
    video_test_read_error = 0;
    video_test_seek_error = 0;
    video_test_read_error_repetitions = 0u;
    video_test_seek_error_repetitions = 0u;
    video_test_read_plan.fill(0);
    video_test_read_plan_size = 0u;
    video_test_read_plan_cursor = 0u;
    video_test_read_plan_slot = -1;
    video_test_stream_start_override_enabled = false;
    video_test_stream_start_override = 0;
    video_test_before_publish_hook = nullptr;
    video_test_before_publish_context = nullptr;
    video_test_after_contact_write_hook = nullptr;
    video_test_after_contact_write_context = nullptr;
}

VideoAnalysisTestStats VideoAnalysisTestGetStats() noexcept {
    return video_test_stats;
}

void VideoAnalysisTestForceSendEagainOnce() noexcept {
    video_test_force_send_eagain_once = true;
}

void VideoAnalysisTestForceTimeoutBeforePublishOnce() noexcept {
    video_test_force_timeout_before_publish_once = true;
}

void VideoAnalysisTestForceTimeoutAfterContactWriteOnce() noexcept {
    video_test_force_timeout_after_contact_write_once = true;
}

void VideoAnalysisTestForceContactDeleteFailureOnce() noexcept {
    video_test_force_contact_delete_failure_once = true;
}

void VideoAnalysisTestForceHardReadFailureAt(uint32_t frame_index) noexcept {
    video_test_hard_read_failure_slot = static_cast<int32_t>(frame_index);
}

void VideoAnalysisTestForceHardSeekFailureAt(uint32_t frame_index) noexcept {
    video_test_hard_seek_failure_slot = static_cast<int32_t>(frame_index);
}

void VideoAnalysisTestInjectReadError(uint32_t frame_index,
                                      int32_t ffmpeg_error,
                                      uint32_t repetitions) noexcept {
    video_test_hard_read_failure_slot = static_cast<int32_t>(frame_index);
    video_test_read_error = ffmpeg_error;
    video_test_read_error_repetitions = repetitions;
}

void VideoAnalysisTestInjectReadPlan(uint32_t frame_index,
                                     const int32_t* ffmpeg_errors,
                                     uint32_t count) noexcept {
    video_test_read_plan.fill(0);
    video_test_read_plan_size =
        (std::min)(count, kVideoTestReadPlanCapacity);
    if (ffmpeg_errors != nullptr && video_test_read_plan_size > 0u) {
        std::copy_n(ffmpeg_errors,
                    video_test_read_plan_size,
                    video_test_read_plan.begin());
    } else {
        video_test_read_plan_size = 0u;
    }
    video_test_read_plan_cursor = 0u;
    video_test_read_plan_slot = static_cast<int32_t>(frame_index);
}

void VideoAnalysisTestInjectSeekError(uint32_t frame_index,
                                      int32_t ffmpeg_error,
                                      uint32_t repetitions) noexcept {
    video_test_hard_seek_failure_slot = static_cast<int32_t>(frame_index);
    video_test_seek_error = ffmpeg_error;
    video_test_seek_error_repetitions = repetitions;
}

void VideoAnalysisTestOverrideStreamStart(int64_t start) noexcept {
    video_test_stream_start_override_enabled = true;
    video_test_stream_start_override = start;
}

int64_t VideoAnalysisTestTargetTimestamp(int64_t relative,
                                         int64_t start) noexcept {
    return SaturatingAdd(relative, start);
}

int64_t VideoAnalysisTestNormalizedTimestamp(int64_t timestamp,
                                             int64_t start) noexcept {
    return SaturatingSubtract(timestamp, start);
}

void VideoAnalysisTestSetBeforePublishHook(
    VideoAnalysisBeforePublishHook hook,
    void* context) noexcept {
    video_test_before_publish_hook = hook;
    video_test_before_publish_context = context;
}

void VideoAnalysisTestSetAfterContactWriteHook(
    VideoAnalysisAfterContactWriteHook hook,
    void* context) noexcept {
    video_test_after_contact_write_hook = hook;
    video_test_after_contact_write_context = context;
}
#endif

namespace {

using GrayImage = videocore::native::GrayImage;
using ImageStatus = videocore::native::ImageStatus;

constexpr int kFrameMaxSide = 512;
constexpr uint32_t kMaxTransientSeekRetries = 8u;
constexpr uint32_t kMaxTransientReadRetries = 8u;
constexpr uint32_t kMaxInvalidReadSkips = 64u;
constexpr uint64_t kVideoFeatures =
    VC_FEATURE_PDQ | VC_FEATURE_PHASH | VC_FEATURE_SOBEL |
    VC_FEATURE_DURATION | VC_FEATURE_CONTACT_SHEET;
constexpr uint64_t kPerFrameFeatures =
    VC_FEATURE_PDQ | VC_FEATURE_PHASH | VC_FEATURE_SOBEL;
constexpr std::array<int64_t, VC_VIDEO_FRAME_COUNT> kNumerators{
    1, 3, 5, 7, 9, 11,
};

void ClearFeaturePayload(vc_feature_set* features) noexcept {
    std::memset(features->pdq, 0, sizeof(features->pdq));
    features->pdq_quality = 0u;
    std::memset(features->phash, 0, sizeof(features->phash));
    std::memset(features->sobel_histogram,
                0,
                sizeof(features->sobel_histogram));
}

void InitializeVideoResult(vc_analysis_result* out) noexcept {
    out->media_type = VC_MEDIA_TYPE_VIDEO;
    out->duration_ms = 0;
    out->duration_status = VC_ERR_DEMUX;
    out->image_status = VC_ERR_UNSUPPORTED;
    out->contact_sheet_status = VC_ERR_UNSUPPORTED;
    out->contact_sheet_width = 0u;
    out->contact_sheet_height = 0u;
    out->completed_frame_mask = 0u;
    ClearFeaturePayload(&out->image_features);
    ClearFeaturePayload(&out->contact_sheet_features);
    out->operation_elapsed_ms = 0u;
    out->decode_elapsed_ms = 0u;
    for (uint32_t index = 0u; index < VC_VIDEO_FRAME_COUNT; ++index) {
        vc_video_frame_result& frame = out->frames[index];
        frame.standard_index = index;
        frame.status = VC_ERR_UNSUPPORTED;
        frame.sample_time_ms = 0;
        ClearFeaturePayload(&frame.features);
    }
}

int32_t InterruptCallback(void* opaque) noexcept {
    auto* state = static_cast<AvioOpaque*>(opaque);
    if (state == nullptr) return 0;
    const int32_t status = CheckInterrupt(state->cancel, state->deadline);
    if (status != VC_OK) state->last_status = status;
    return status == VC_OK ? 0 : 1;
}

int32_t BoundaryStatus(AvioOpaque& opaque,
                       int ffmpeg_status,
                       int32_t fallback) noexcept {
    const int32_t interrupted = CheckInterrupt(opaque.cancel, opaque.deadline);
    if (interrupted != VC_OK) return interrupted;
    if (opaque.last_status == VC_ERR_CANCELLED ||
        opaque.last_status == VC_ERR_TIMEOUT) {
        return opaque.last_status;
    }
    (void)ffmpeg_status;
    return fallback;
}

int64_t DurationMilliseconds(const AVFormatContext* format,
                             int64_t known_duration_ms) noexcept {
    if (known_duration_ms > 0) return known_duration_ms;
    if (format == nullptr || format->duration == AV_NOPTS_VALUE ||
        format->duration <= 0) {
        return 0;
    }
    return av_rescale_q(format->duration,
                        AV_TIME_BASE_Q,
                        AVRational{1, 1000});
}

int64_t SampleMilliseconds(int64_t duration_ms,
                           int64_t numerator) noexcept {
    const int64_t quotient = duration_ms / 12;
    const int64_t remainder = duration_ms % 12;
    return quotient * numerator + (remainder * numerator) / 12;
}

int NormalizedClockwiseRotation(const AVCodecParameters* parameters) noexcept {
    if (parameters == nullptr) return 0;
    const AVPacketSideData* side_data = av_packet_side_data_get(
        parameters->coded_side_data,
        parameters->nb_coded_side_data,
        AV_PKT_DATA_DISPLAYMATRIX);
    if (side_data == nullptr || side_data->size < 9u * sizeof(int32_t)) {
        return 0;
    }
    const double counter_clockwise = av_display_rotation_get(
        reinterpret_cast<const int32_t*>(side_data->data));
    if (!std::isfinite(counter_clockwise)) return 0;
    int clockwise = static_cast<int>(std::llround(-counter_clockwise));
    clockwise %= 360;
    if (clockwise < 0) clockwise += 360;
    if (clockwise < 45 || clockwise >= 315) return 0;
    if (clockwise < 135) return 90;
    if (clockwise < 225) return 180;
    return 270;
}

AVRational ValidSar(AVFormatContext* format,
                    AVStream* stream,
                    const AVFrame* frame) noexcept {
    AVRational sar = av_guess_sample_aspect_ratio(
        format, stream, const_cast<AVFrame*>(frame));
    if (sar.num <= 0 || sar.den <= 0) sar = AVRational{1, 1};
    return sar;
}

bool ScaleDimensions(int width,
                     int height,
                     AVRational sar,
                     int* out_width,
                     int* out_height) noexcept {
    if (width <= 0 || height <= 0 || sar.num <= 0 || sar.den <= 0 ||
        out_width == nullptr || out_height == nullptr) {
        return false;
    }
    const long double display_width =
        static_cast<long double>(width) * sar.num / sar.den;
    const long double display_height = height;
    if (display_width >= display_height) {
        *out_width = kFrameMaxSide;
        *out_height = (std::max)(
            1, static_cast<int>(std::floor(
                   kFrameMaxSide * display_height / display_width)));
    } else {
        *out_height = kFrameMaxSide;
        *out_width = (std::max)(
            1, static_cast<int>(std::floor(
                   kFrameMaxSide * display_width / display_height)));
    }
    return true;
}

bool RotateGray(const GrayImage& source,
                int clockwise,
                GrayImage* out) {
    if (out == nullptr) return false;
    if (clockwise == 0) {
        *out = source;
        return true;
    }
    const bool swaps = clockwise == 90 || clockwise == 270;
    out->width = swaps ? source.height : source.width;
    out->height = swaps ? source.width : source.height;
    out->stride = out->width;
    const uint64_t size = static_cast<uint64_t>(out->stride) * out->height;
    if (size > (std::numeric_limits<size_t>::max)()) return false;
    out->pixels.assign(static_cast<size_t>(size), 0u);
    for (int y = 0; y < source.height; ++y) {
        for (int x = 0; x < source.width; ++x) {
            int dx = x;
            int dy = y;
            if (clockwise == 90) {
                dx = source.height - 1 - y;
                dy = x;
            } else if (clockwise == 180) {
                dx = source.width - 1 - x;
                dy = source.height - 1 - y;
            } else if (clockwise == 270) {
                dx = y;
                dy = source.width - 1 - x;
            }
            out->pixels[static_cast<size_t>(dy) * out->stride + dx] =
                source.pixels[static_cast<size_t>(y) * source.stride + x];
        }
    }
    return true;
}

int SwsColorspace(AVColorSpace colorspace) noexcept {
    switch (colorspace) {
        case AVCOL_SPC_BT709: return SWS_CS_ITU709;
        case AVCOL_SPC_FCC: return SWS_CS_FCC;
        case AVCOL_SPC_BT470BG:
        case AVCOL_SPC_SMPTE170M: return SWS_CS_ITU601;
        case AVCOL_SPC_SMPTE240M: return SWS_CS_SMPTE240M;
        case AVCOL_SPC_BT2020_NCL:
        case AVCOL_SPC_BT2020_CL: return SWS_CS_BT2020;
        default: return SWS_CS_DEFAULT;
    }
}

struct VideoSwsDeleter {
    void operator()(SwsContext* context) const noexcept {
        if (context == nullptr) return;
        sws_freeContext(context);
#if defined(VC_RESILIENCE_TESTING)
        live_scalers.fetch_sub(1u, std::memory_order_acq_rel);
#endif
    }
};

using VideoSwsOwner = std::unique_ptr<SwsContext, VideoSwsDeleter>;

bool ConfigureGrayConversion(SwsContext* context,
                             AVColorSpace colorspace,
                             int source_range,
                             int destination_range) noexcept {
    if (context == nullptr) return false;
    const int* coefficients = sws_getCoefficients(SwsColorspace(colorspace));
    return coefficients != nullptr &&
           sws_setColorspaceDetails(context,
                                    coefficients,
                                    source_range,
                                    coefficients,
                                    destination_range,
                                    0,
                                    1 << 16,
                                    1 << 16) >= 0;
}

ImageStatus FrameToGray(AVFormatContext* format,
                        AVStream* stream,
                        const AVFrame* frame,
                        int rotation,
                        GrayImage* out) {
    if (frame == nullptr || out == nullptr || frame->width <= 0 ||
        frame->height <= 0 || frame->format < 0) {
        return ImageStatus::invalid_argument;
    }
    GrayImage native_gray;
    native_gray.width = frame->width;
    native_gray.height = frame->height;
    native_gray.stride = frame->width;
    try {
        native_gray.pixels.resize(
            static_cast<size_t>(frame->width) * frame->height);
    } catch (const std::bad_alloc&) {
        return ImageStatus::out_of_memory;
    }
    VideoSwsOwner converter(
        sws_getContext(frame->width,
                       frame->height,
                       static_cast<AVPixelFormat>(frame->format),
                       frame->width,
                       frame->height,
                       AV_PIX_FMT_GRAY8,
                       SWS_BICUBIC,
                       nullptr,
                       nullptr,
                       nullptr));
    if (converter) {
#if defined(VC_RESILIENCE_TESTING)
        live_scalers.fetch_add(1u, std::memory_order_acq_rel);
        acquired_scalers.fetch_add(1u, std::memory_order_acq_rel);
#endif
    }
    const int source_range =
        frame->color_range == AVCOL_RANGE_JPEG ? 1 : 0;
    if (!converter ||
        !ConfigureGrayConversion(converter.get(),
                                 frame->colorspace,
                                 source_range,
                                 source_range)) {
        return ImageStatus::decode_error;
    }
    uint8_t* native_destination[4]{
        native_gray.pixels.data(), nullptr, nullptr, nullptr};
    int native_destination_stride[4]{native_gray.stride, 0, 0, 0};
    if (sws_scale(converter.get(),
                  frame->data,
                  frame->linesize,
                  0,
                  frame->height,
                  native_destination,
                  native_destination_stride) != frame->height) {
        return ImageStatus::decode_error;
    }

    GrayImage rotated;
    try {
        if (!RotateGray(native_gray, rotation, &rotated)) {
            return ImageStatus::size_error;
        }
    } catch (const std::bad_alloc&) {
        return ImageStatus::out_of_memory;
    }

    AVRational rotated_sar = ValidSar(format, stream, frame);
    if (rotation == 90 || rotation == 270) {
        std::swap(rotated_sar.num, rotated_sar.den);
    }
    int scaled_width = 0;
    int scaled_height = 0;
    if (!ScaleDimensions(rotated.width,
                         rotated.height,
                         rotated_sar,
                         &scaled_width,
                         &scaled_height)) {
        return ImageStatus::size_error;
    }
    VideoSwsOwner scaler(
        sws_getContext(rotated.width,
                       rotated.height,
                       AV_PIX_FMT_GRAY8,
                       scaled_width,
                       scaled_height,
                       AV_PIX_FMT_GRAY8,
                       SWS_BICUBIC,
                       nullptr,
                       nullptr,
                       nullptr));
    if (scaler) {
#if defined(VC_RESILIENCE_TESTING)
        live_scalers.fetch_add(1u, std::memory_order_acq_rel);
        acquired_scalers.fetch_add(1u, std::memory_order_acq_rel);
#endif
    }
    if (!scaler ||
        !ConfigureGrayConversion(scaler.get(),
                                 frame->colorspace,
                                 source_range,
                                 1)) {
        return ImageStatus::decode_error;
    }
    out->width = scaled_width;
    out->height = scaled_height;
    out->stride = scaled_width;
    try {
        out->pixels.resize(
            static_cast<size_t>(scaled_width) * scaled_height);
    } catch (const std::bad_alloc&) {
        return ImageStatus::out_of_memory;
    }
    const uint8_t* rotated_source[4]{
        rotated.pixels.data(), nullptr, nullptr, nullptr};
    int rotated_source_stride[4]{rotated.stride, 0, 0, 0};
    uint8_t* scaled_destination[4]{
        out->pixels.data(), nullptr, nullptr, nullptr};
    int scaled_destination_stride[4]{out->stride, 0, 0, 0};
    return sws_scale(scaler.get(),
                     rotated_source,
                     rotated_source_stride,
                     0,
                     rotated.height,
                     scaled_destination,
                     scaled_destination_stride) == scaled_height
               ? ImageStatus::ok
               : ImageStatus::decode_error;
}

int32_t MapImageStatus(ImageStatus status) noexcept {
    switch (status) {
        case ImageStatus::ok: return VC_OK;
        case ImageStatus::invalid_argument: return VC_ERR_INVALID_ARG;
        case ImageStatus::out_of_memory: return VC_ERR_OOM;
        case ImageStatus::decode_error:
        case ImageStatus::size_error: return VC_ERR_DECODE;
        case ImageStatus::internal_error: return VC_ERR_INTERNAL;
    }
    return VC_ERR_INTERNAL;
}

int32_t ComputeFeatures(const GrayImage& gray,
                        uint64_t feature_mask,
                        vc_feature_set* out,
                        uint32_t frame_index) noexcept {
    std::array<uint8_t, VC_PDQ_SIZE> pdq{};
    int32_t quality = 0;
    std::array<uint64_t, VC_PHASH_COUNT> phash{};
    std::array<float, VC_SOBEL_HISTOGRAM_SIZE> sobel{};
    ImageStatus status = ImageStatus::ok;
    if ((feature_mask & VC_FEATURE_PDQ) != 0u) {
#if defined(VC_VIDEO_ANALYSIS_TESTING)
        if (frame_index < VC_VIDEO_FRAME_COUNT) {
            ++video_test_stats.pdq_compute_counts[frame_index];
        }
#endif
        status = videocore::native::ComputePdq(gray, &pdq, &quality);
    }
    if (status == ImageStatus::ok &&
        (feature_mask & VC_FEATURE_PHASH) != 0u) {
#if defined(VC_VIDEO_ANALYSIS_TESTING)
        if (frame_index < VC_VIDEO_FRAME_COUNT) {
            ++video_test_stats.phash_compute_counts[frame_index];
        }
#endif
        status = videocore::native::ComputePHashParts(gray, &phash);
    }
    if (status == ImageStatus::ok &&
        (feature_mask & VC_FEATURE_SOBEL) != 0u) {
#if defined(VC_VIDEO_ANALYSIS_TESTING)
        if (frame_index < VC_VIDEO_FRAME_COUNT) {
            ++video_test_stats.sobel_compute_counts[frame_index];
        }
#endif
        status = videocore::native::ComputeSobelHistogram(gray, &sobel);
    }
    if (status != ImageStatus::ok) return MapImageStatus(status);
    ClearFeaturePayload(out);
    if ((feature_mask & VC_FEATURE_PDQ) != 0u) {
        std::memcpy(out->pdq, pdq.data(), pdq.size());
        out->pdq_quality = static_cast<uint32_t>((std::max)(quality, 0));
    }
    if ((feature_mask & VC_FEATURE_PHASH) != 0u) {
        std::memcpy(out->phash, phash.data(), sizeof(out->phash));
    }
    if ((feature_mask & VC_FEATURE_SOBEL) != 0u) {
        std::memcpy(out->sobel_histogram,
                    sobel.data(),
                    sizeof(out->sobel_histogram));
    }
    return VC_OK;
}

struct FormatOwner {
    AVFormatContext* value = nullptr;
    bool tracked = false;
    void Track() noexcept {
#if defined(VC_RESILIENCE_TESTING)
        if (value != nullptr && !tracked) {
            tracked = true;
            live_formats.fetch_add(1u, std::memory_order_acq_rel);
            acquired_formats.fetch_add(1u, std::memory_order_acq_rel);
        }
#endif
    }
    ~FormatOwner() {
        if (value != nullptr) avformat_close_input(&value);
#if defined(VC_RESILIENCE_TESTING)
        if (tracked) {
            live_formats.fetch_sub(1u, std::memory_order_acq_rel);
        }
#endif
    }
};

struct CodecOwner {
    AVCodecContext* value = nullptr;
    bool tracked = false;
    void Track() noexcept {
#if defined(VC_RESILIENCE_TESTING)
        if (value != nullptr && !tracked) {
            tracked = true;
            live_codecs.fetch_add(1u, std::memory_order_acq_rel);
            acquired_codecs.fetch_add(1u, std::memory_order_acq_rel);
        }
#endif
    }
    ~CodecOwner() {
        if (value != nullptr) avcodec_free_context(&value);
#if defined(VC_RESILIENCE_TESTING)
        if (tracked) {
            live_codecs.fetch_sub(1u, std::memory_order_acq_rel);
        }
#endif
    }
};

struct PacketOwner {
    AVPacket* value = av_packet_alloc();
    PacketOwner() noexcept {
#if defined(VC_RESILIENCE_TESTING)
        if (value != nullptr) {
            live_packets.fetch_add(1u, std::memory_order_acq_rel);
            acquired_packets.fetch_add(1u, std::memory_order_acq_rel);
        }
#endif
    }
    ~PacketOwner() {
        const bool allocated = value != nullptr;
        av_packet_free(&value);
#if defined(VC_RESILIENCE_TESTING)
        if (allocated) {
            live_packets.fetch_sub(1u, std::memory_order_acq_rel);
        }
#endif
    }
};

struct FrameOwner {
    AVFrame* value = av_frame_alloc();
    FrameOwner() noexcept {
#if defined(VC_RESILIENCE_TESTING)
        if (value != nullptr) {
            live_frames.fetch_add(1u, std::memory_order_acq_rel);
            acquired_frames.fetch_add(1u, std::memory_order_acq_rel);
        }
#endif
    }
    ~FrameOwner() {
        const bool allocated = value != nullptr;
        av_frame_free(&value);
#if defined(VC_RESILIENCE_TESTING)
        if (allocated) {
            live_frames.fetch_sub(1u, std::memory_order_acq_rel);
        }
#endif
    }
};

int32_t ReceiveUntilTarget(AVCodecContext* codec,
                           AVFrame* frame,
                           AvioOpaque& opaque,
                           int64_t target_timestamp,
                           AVFormatContext* format,
                           AVStream* stream,
                           int rotation,
                           uint64_t feature_mask,
                           vc_feature_set* features,
                           int32_t* width,
                           int32_t* height,
                           GrayImage* selected_gray,
                           uint32_t frame_index,
                           int32_t* decode_ordinal) noexcept {
    for (;;) {
        const int32_t before_decode = CheckOperationBoundary(
            opaque.cancel, opaque.deadline, OperationBoundary::decode);
        if (before_decode != VC_OK) return before_decode;
        const int status = avcodec_receive_frame(codec, frame);
        if (status == AVERROR(EAGAIN) || status == AVERROR_EOF) {
            return VC_ERR_NO_FRAME;
        }
        if (status < 0) return VC_ERR_DECODE;
        if (decode_ordinal != nullptr) ++(*decode_ordinal);
        const int64_t timestamp = frame->best_effort_timestamp;
        if (timestamp == AV_NOPTS_VALUE || timestamp < target_timestamp) {
            av_frame_unref(frame);
            continue;
        }
#if defined(VC_VIDEO_ANALYSIS_TESTING)
        const int64_t stream_start = stream->start_time == AV_NOPTS_VALUE
                                         ? 0
                                         : stream->start_time;
        const int64_t selected_pts =
            SaturatingSubtract(frame->pts, stream_start);
        const int64_t selected_pts_time_micros =
            av_rescale_q(selected_pts, stream->time_base,
                         AVRational{1, 1000000});
        const int32_t selected_decode_ordinal =
            decode_ordinal == nullptr ? -1 : *decode_ordinal;
        const uint8_t selected_key_frame =
            static_cast<uint8_t>((frame->flags & AV_FRAME_FLAG_KEY) != 0);
        const char selected_picture_type =
            av_get_picture_type_char(frame->pict_type);
#endif
        GrayImage gray;
        const ImageStatus gray_status =
            FrameToGray(format, stream, frame, rotation, &gray);
        av_frame_unref(frame);
        if (gray_status != ImageStatus::ok) return MapImageStatus(gray_status);
#if defined(VC_VIDEO_ANALYSIS_TESTING)
        ++video_test_stats.gray_conversion_counts[frame_index];
#endif
        const int32_t before_feature = CheckOperationBoundary(
            opaque.cancel, opaque.deadline, OperationBoundary::feature);
        if (before_feature != VC_OK) return before_feature;
        const int32_t feature_status =
            ComputeFeatures(gray, feature_mask, features, frame_index);
        if (feature_status != VC_OK) return feature_status;
        *width = gray.width;
        *height = gray.height;
        if (selected_gray != nullptr) *selected_gray = std::move(gray);
#if defined(VC_VIDEO_ANALYSIS_TESTING)
        video_test_stats.selected_pts[frame_index] = selected_pts;
        video_test_stats.selected_pts_time_micros[frame_index] =
            selected_pts_time_micros;
        video_test_stats.selected_decode_ordinals[frame_index] =
            selected_decode_ordinal;
        video_test_stats.selected_key_frames[frame_index] =
            selected_key_frame;
        video_test_stats.selected_picture_types[frame_index] =
            selected_picture_type;
#else
        (void)frame_index;
#endif
        return VC_OK;
    }
}

int32_t DecodeSample(AVFormatContext* format,
                     AVCodecContext* codec,
                     AVStream* stream,
                     int stream_index,
                     int64_t sample_ms,
                     int rotation,
                     uint64_t feature_mask,
                     AvioOpaque& opaque,
                     vc_feature_set* features,
                     int32_t* width,
                     int32_t* height,
                     GrayImage* selected_gray,
                     bool* format_failed,
                     uint32_t frame_index,
                     bool seek_from_stream_start) noexcept {
    const int64_t relative_timestamp = av_rescale_q(
        sample_ms, AVRational{1, 1000}, stream->time_base);
    const int64_t start = stream->start_time == AV_NOPTS_VALUE
                              ? 0
                              : stream->start_time;
    const int64_t target = SaturatingAdd(relative_timestamp, start);
    const int64_t seek_target = seek_from_stream_start ? start : target;
    const int64_t recovery_preroll = av_rescale_q(
        1, AVRational{1, 1}, stream->time_base);
    const int64_t recovery_min =
        SaturatingSubtract(seek_target, recovery_preroll);
    // This is decoder-preroll recovery, not a seek-to-start operation.
    // The real stream start anchors the upper bound while the requested
    // timestamp is the earlier decode point needed for B-frame identity.
    int seek_status = 0;
    for (uint32_t seek_attempt = 0u;; ++seek_attempt) {
        const int32_t before_seek = CheckOperationBoundary(
            opaque.cancel, opaque.deadline, OperationBoundary::seek);
        if (before_seek != VC_OK) {
            *format_failed = true;
            return before_seek;
        }
#if defined(VC_VIDEO_ANALYSIS_TESTING)
        ++video_test_stats.seek_call_count;
        if (seek_from_stream_start) {
            ++video_test_stats.recovery_seek_call_count;
            video_test_stats.recovery_seek_stream_index = stream_index;
            video_test_stats.recovery_seek_flags = AVSEEK_FLAG_BACKWARD;
            video_test_stats.recovery_seek_min = recovery_min;
            video_test_stats.recovery_seek_target = recovery_min;
            video_test_stats.recovery_seek_max = seek_target;
        }
        const bool generalized_seek_injection = video_test_seek_error != 0;
        if (video_test_hard_seek_failure_slot ==
                static_cast<int32_t>(frame_index) &&
            (!generalized_seek_injection ||
             video_test_seek_error_repetitions > 0u)) {
            seek_status = generalized_seek_injection
                              ? video_test_seek_error
                              : AVERROR(EIO);
            if (generalized_seek_injection) {
                --video_test_seek_error_repetitions;
                ++video_test_stats.injected_seek_error_count;
            }
            if (seek_status != AVERROR(EAGAIN) &&
                seek_status != AVERROR_INVALIDDATA) {
                ++video_test_stats.hard_failure_count;
            }
        } else
#endif
        {
            seek_status = seek_from_stream_start
                              ? avformat_seek_file(format,
                                                   stream_index,
                                                   recovery_min,
                                                   recovery_min,
                                                   seek_target,
                                                   AVSEEK_FLAG_BACKWARD)
                              : av_seek_frame(format,
                                              stream_index,
                                              seek_target,
                                              AVSEEK_FLAG_BACKWARD);
        }
        if (seek_status != AVERROR(EAGAIN)) break;
        const int32_t interrupted = CheckInterrupt(opaque.cancel,
                                                   opaque.deadline);
        if (interrupted != VC_OK) {
            *format_failed = true;
            return interrupted;
        }
        if (seek_attempt + 1u >= kMaxTransientSeekRetries) {
            *format_failed = true;
            return VC_ERR_IO;
        }
    }
    if (seek_status < 0) {
        const int32_t interrupted =
            BoundaryStatus(opaque, seek_status, VC_ERR_NO_FRAME);
        if (interrupted == VC_ERR_CANCELLED ||
            interrupted == VC_ERR_TIMEOUT) {
            *format_failed = true;
            return interrupted;
        }
        *format_failed = true;
        if (seek_status == AVERROR(ENOMEM)) {
            return VC_ERR_OOM;
        }
        return VC_ERR_IO;
    }
    avcodec_flush_buffers(codec);
    PacketOwner packet;
    FrameOwner frame;
    if (packet.value == nullptr || frame.value == nullptr) return VC_ERR_OOM;
    int32_t decode_ordinal = -1;
    uint32_t transient_read_retries = 0u;
    uint32_t invalid_read_skips = 0u;

    for (;;) {
        const int32_t before_read = CheckOperationBoundary(
            opaque.cancel, opaque.deadline, OperationBoundary::packet_read);
        if (before_read != VC_OK) {
            *format_failed = true;
            return before_read;
        }
        int read_status = 0;
#if defined(VC_VIDEO_ANALYSIS_TESTING)
        const bool planned_read =
            video_test_read_plan_slot == static_cast<int32_t>(frame_index) &&
            video_test_read_plan_cursor < video_test_read_plan_size;
        const bool generalized_read_injection = video_test_read_error != 0;
        if (planned_read) {
            const int32_t planned_status =
                video_test_read_plan[video_test_read_plan_cursor++];
            if (planned_status == 0) {
                read_status = av_read_frame(format, packet.value);
                if (read_status >= 0) {
                    ++video_test_stats.planned_successful_read_count;
                }
            } else {
                read_status = planned_status;
                ++video_test_stats.injected_read_error_count;
            }
        } else if (video_test_hard_read_failure_slot ==
                static_cast<int32_t>(frame_index) &&
            (!generalized_read_injection ||
             video_test_read_error_repetitions > 0u)) {
            read_status = generalized_read_injection
                              ? video_test_read_error
                              : AVERROR(EIO);
            if (generalized_read_injection) {
                --video_test_read_error_repetitions;
                ++video_test_stats.injected_read_error_count;
            }
            if (read_status != AVERROR(EAGAIN) &&
                read_status != AVERROR_INVALIDDATA) {
                ++video_test_stats.hard_failure_count;
            }
        } else
#endif
        {
            read_status = av_read_frame(format, packet.value);
        }
        if (read_status < 0) {
            if (read_status != AVERROR_EOF) {
                const int32_t mapped =
                    BoundaryStatus(opaque, read_status, VC_ERR_DEMUX);
                if (mapped == VC_ERR_CANCELLED ||
                    mapped == VC_ERR_TIMEOUT) {
                    *format_failed = true;
                    return mapped;
                }
                if (read_status == AVERROR(EAGAIN)) {
                    if (++transient_read_retries <=
                        kMaxTransientReadRetries) {
                        continue;
                    }
                    *format_failed = true;
                    return VC_ERR_DEMUX;
                }
                if (read_status == AVERROR_INVALIDDATA) {
                    if (++invalid_read_skips <= kMaxInvalidReadSkips) {
                        continue;
                    }
                    *format_failed = true;
                    return VC_ERR_DEMUX;
                }
                *format_failed = true;
                if (read_status == AVERROR(ENOMEM)) return VC_ERR_OOM;
                return mapped;
            }
            const int32_t before_decode = CheckOperationBoundary(
                opaque.cancel, opaque.deadline, OperationBoundary::decode);
            if (before_decode != VC_OK) return before_decode;
            const int send_status = avcodec_send_packet(codec, nullptr);
            if (send_status < 0 && send_status != AVERROR_EOF) {
                return BoundaryStatus(opaque, send_status, VC_ERR_DECODE);
            }
            return ReceiveUntilTarget(codec,
                                      frame.value,
                                      opaque,
                                      target,
                                      format,
                                      stream,
                                      rotation,
                                      feature_mask,
                                      features,
                                      width,
                                      height,
                                      selected_gray,
                                      frame_index,
                                      &decode_ordinal);
        }
        transient_read_retries = 0u;
        if (packet.value->stream_index != stream_index) {
            av_packet_unref(packet.value);
            continue;
        }
        bool retrying_same_packet = false;
        bool target_ready = false;
        for (;;) {
        int send_status = 0;
        const int32_t before_decode = CheckOperationBoundary(
            opaque.cancel, opaque.deadline, OperationBoundary::decode);
        if (before_decode != VC_OK) {
            av_packet_unref(packet.value);
            return before_decode;
        }
#if defined(VC_VIDEO_ANALYSIS_TESTING)
        ++video_test_stats.send_packet_attempts;
        if (retrying_same_packet) {
            ++video_test_stats.same_packet_resends;
        }
        if (video_test_force_send_eagain_once) {
            video_test_force_send_eagain_once = false;
            ++video_test_stats.forced_send_eagain;
            send_status = AVERROR(EAGAIN);
        } else {
            send_status = avcodec_send_packet(codec, packet.value);
        }
#else
        send_status = avcodec_send_packet(codec, packet.value);
#endif
        if (send_status == AVERROR(EAGAIN)) {
            retrying_same_packet = true;
            if (target_ready) {
                const int drain_status =
                    avcodec_receive_frame(codec, frame.value);
                if (drain_status == AVERROR(EAGAIN) ||
                    drain_status == AVERROR_EOF) {
                    av_packet_unref(packet.value);
                    return BoundaryStatus(
                        opaque, drain_status, VC_ERR_DECODE);
                }
                if (drain_status < 0) {
                    av_packet_unref(packet.value);
                    return BoundaryStatus(
                        opaque, drain_status, VC_ERR_DECODE);
                }
                av_frame_unref(frame.value);
                continue;
            }
            const int32_t drain_status = ReceiveUntilTarget(codec,
                                                             frame.value,
                                                             opaque,
                                                             target,
                                                             format,
                                                             stream,
                                                             rotation,
                                                             feature_mask,
                                                             features,
                                                             width,
                                                             height,
                                                             selected_gray,
                                                             frame_index,
                                                             &decode_ordinal);
            if (drain_status == VC_OK) {
                target_ready = true;
            } else if (drain_status != VC_ERR_NO_FRAME) {
                av_packet_unref(packet.value);
                return drain_status;
            }
            continue;
        }
        if (send_status < 0) {
            av_packet_unref(packet.value);
            if (send_status == AVERROR_INVALIDDATA) break;
            return BoundaryStatus(opaque, send_status, VC_ERR_DECODE);
        }
        av_packet_unref(packet.value);
        if (target_ready) return VC_OK;
        const int32_t receive_status = ReceiveUntilTarget(codec,
                                                           frame.value,
                                                           opaque,
                                                           target,
                                                           format,
                                                           stream,
                                                           rotation,
                                                           feature_mask,
                                                           features,
                                                           width,
                                                           height,
                                                           selected_gray,
                                                           frame_index,
                                                           &decode_ordinal);
        if (receive_status == VC_OK) return VC_OK;
        if (receive_status != VC_ERR_NO_FRAME) return receive_status;
        break;
        }
    }
}

}  // namespace

int32_t PublishVideoFailure(vc_analysis_result* out,
                            vc_error* error,
                            int32_t code,
                            const char* message) noexcept {
    InitializeVideoResult(out);
    out->duration_status = code;
    for (auto& frame : out->frames) frame.status = code;
    SetError(error, code, 0, 0, message);
    return code;
}

int32_t AnalyzeVideo(AvioBridge* avio,
                     const CancelState* cancel,
                     const vc_analysis_request& request,
                     vc_analysis_result* out,
                     vc_error* error) noexcept {
    const auto operation_start = std::chrono::steady_clock::now();
    InitializeVideoResult(out);
    if (avio == nullptr || avio->context() == nullptr) {
        return PublishVideoFailure(
            out, error, VC_ERR_INTERNAL, "video AVIO is unavailable");
    }
    if (request.feature_mask == 0u ||
        (request.feature_mask & ~kVideoFeatures) != 0u) {
        return PublishVideoFailure(
            out, error, VC_ERR_UNSUPPORTED, "video feature is unavailable");
    }
    const bool contact_requested =
        (request.feature_mask & VC_FEATURE_CONTACT_SHEET) != 0u;
    const bool contact_only =
        request.feature_mask == VC_FEATURE_CONTACT_SHEET;
    if (contact_requested &&
        (request.temporary_jpeg_path == nullptr ||
         request.temporary_jpeg_path_units == 0u ||
         std::find(request.temporary_jpeg_path,
                   request.temporary_jpeg_path +
                       request.temporary_jpeg_path_units,
                   uint16_t{0}) != request.temporary_jpeg_path +
                                      request.temporary_jpeg_path_units)) {
        const int32_t status = PublishVideoFailure(
            out, error, VC_ERR_INVALID_ARG,
            "contact sheet temporary path is invalid");
        out->contact_sheet_status = VC_ERR_INVALID_ARG;
        return status;
    }
    std::wstring contact_temporary_path;
    if (contact_requested) {
        try {
            contact_temporary_path.assign(
                reinterpret_cast<const wchar_t*>(
                    request.temporary_jpeg_path),
                request.temporary_jpeg_path_units);
        } catch (const std::bad_alloc&) {
            const int32_t status = PublishVideoFailure(
                out, error, VC_ERR_OOM,
                "contact sheet temporary path allocation failed");
            out->contact_sheet_status = VC_ERR_OOM;
            return status;
        } catch (...) {
            const int32_t status = PublishVideoFailure(
                out, error, VC_ERR_INTERNAL,
                "contact sheet temporary path allocation failed");
            out->contact_sheet_status = VC_ERR_INTERNAL;
            return status;
        }
    }
    const auto publish_early_failure =
        [&](int32_t code, const char* message) noexcept {
            const int32_t status =
                PublishVideoFailure(out, error, code, message);
            if (contact_requested &&
                (code == VC_ERR_CANCELLED || code == VC_ERR_TIMEOUT ||
                 code == VC_ERR_STALE)) {
                out->contact_sheet_status = code;
            }
            return status;
        };

    AvioOpaque& opaque = avio->opaque();
    opaque.cancel = cancel;
    const uint32_t probe_timeout =
        request.probe_timeout_ms == 0u ? 15000u : request.probe_timeout_ms;
    opaque.deadline = Deadline::After(
        std::chrono::milliseconds(probe_timeout));
    opaque.last_status = VC_OK;
    int32_t boundary_status = CheckOperationBoundary(
        cancel, opaque.deadline, OperationBoundary::seek);
    if (boundary_status != VC_OK) {
        return publish_early_failure(
            boundary_status, "video input rewind interrupted");
    }
    if (SeekPacket(&opaque, 0, SEEK_SET) < 0) {
        return publish_early_failure(
            BoundaryStatus(opaque, AVERROR(EIO), VC_ERR_IO),
            "video input rewind failed");
    }
    avio_flush(avio->context());
    avio->context()->pos = 0;
    avio->context()->eof_reached = 0;
    avio->context()->error = 0;

    FormatOwner format;
    format.value = avformat_alloc_context();
    if (format.value == nullptr) {
        return publish_early_failure(
            VC_ERR_OOM, "video format allocation failed");
    }
    format.Track();
#if defined(VC_VIDEO_ANALYSIS_TESTING)
    ++video_test_stats.format_contexts;
#endif
    format.value->pb = avio->context();
    format.value->flags |= AVFMT_FLAG_CUSTOM_IO;
    format.value->interrupt_callback.callback = &InterruptCallback;
    format.value->interrupt_callback.opaque = &opaque;
    boundary_status = CheckOperationBoundary(
        cancel, opaque.deadline, OperationBoundary::probe);
    if (boundary_status != VC_OK) {
        return publish_early_failure(
            boundary_status, "video container open interrupted");
    }
    int ffmpeg_status =
        avformat_open_input(&format.value, nullptr, nullptr, nullptr);
    if (ffmpeg_status < 0) {
        const int32_t status =
            BoundaryStatus(opaque, ffmpeg_status, VC_ERR_DEMUX);
        return publish_early_failure(status, "video container open failed");
    }
    boundary_status = CheckOperationBoundary(
        cancel, opaque.deadline, OperationBoundary::probe);
    if (boundary_status != VC_OK) {
        return publish_early_failure(
            boundary_status, "video stream probe interrupted");
    }
    ffmpeg_status = avformat_find_stream_info(format.value, nullptr);
    if (ffmpeg_status < 0) {
        const int32_t status =
            BoundaryStatus(opaque, ffmpeg_status, VC_ERR_DEMUX);
        return publish_early_failure(status, "video stream probe failed");
    }

    const int64_t duration_ms =
        DurationMilliseconds(format.value, request.known_duration_ms);
    if (duration_ms <= 0) {
        return publish_early_failure(
            VC_ERR_DEMUX, "video duration is unavailable");
    }
    out->duration_ms = duration_ms;
    out->duration_status = VC_OK;
    const uint32_t requested_mask =
        request.frame_mask == 0u ? VC_ALL_FRAME_MASK : request.frame_mask;
    const uint32_t decode_mask =
        contact_requested ? VC_ALL_FRAME_MASK : requested_mask;
    for (uint32_t index = 0u; index < VC_VIDEO_FRAME_COUNT; ++index) {
        out->frames[index].sample_time_ms =
            SampleMilliseconds(duration_ms, kNumerators[index]);
        out->frames[index].status =
            (requested_mask & (1u << index)) != 0u
                ? VC_ERR_NO_FRAME
                : VC_ERR_UNSUPPORTED;
    }

    const AVCodec* decoder = nullptr;
    const int stream_index = av_find_best_stream(
        format.value, AVMEDIA_TYPE_VIDEO, -1, -1, &decoder, 0);
    if (stream_index < 0 || decoder == nullptr) {
        out->operation_elapsed_ms = static_cast<uint64_t>(
            std::chrono::duration_cast<std::chrono::milliseconds>(
                std::chrono::steady_clock::now() - operation_start)
                .count());
        SetError(error, VC_ERR_NO_FRAME, stream_index, 0,
                 "media has no decodable video stream");
        if (contact_requested) out->contact_sheet_status = VC_ERR_NO_FRAME;
        return VC_ERR_NO_FRAME;
    }
    AVStream* stream = format.value->streams[stream_index];
#if defined(VC_VIDEO_ANALYSIS_TESTING)
    if (video_test_stream_start_override_enabled) {
        stream->start_time = video_test_stream_start_override;
    }
#endif
    CodecOwner codec;
    codec.value = avcodec_alloc_context3(decoder);
    if (codec.value == nullptr) {
        return PublishVideoFailure(
            out, error, VC_ERR_OOM, "video codec allocation failed");
    }
    codec.Track();
#if defined(VC_VIDEO_ANALYSIS_TESTING)
    ++video_test_stats.codec_contexts;
#endif
    ffmpeg_status =
        avcodec_parameters_to_context(codec.value, stream->codecpar);
    if (ffmpeg_status < 0) {
        return PublishVideoFailure(
            out, error, VC_ERR_DECODE, "video codec parameters failed");
    }
    codec.value->thread_count = 1;
    codec.value->thread_type = 0;
    ffmpeg_status = avcodec_open2(codec.value, decoder, nullptr);
    if (ffmpeg_status < 0) {
        return PublishVideoFailure(
            out, error, VC_ERR_DECODE, "video decoder open failed");
    }

    const int rotation = NormalizedClockwiseRotation(stream->codecpar);
    const auto decode_start = std::chrono::steady_clock::now();
    bool format_failed = false;
    uint32_t successes = 0u;
    std::array<GrayImage, VC_VIDEO_FRAME_COUNT> contact_grays{};
    std::array<bool, VC_VIDEO_FRAME_COUNT> contact_gray_valid{};
    int32_t last_failure = VC_ERR_NO_FRAME;
    auto clear_frames_for_interrupt = [&](int32_t interrupt_status) noexcept {
        out->completed_frame_mask = 0u;
        successes = 0u;
        for (uint32_t frame_index = 0u;
             frame_index < VC_VIDEO_FRAME_COUNT;
             ++frame_index) {
            ClearFeaturePayload(&out->frames[frame_index].features);
            if ((requested_mask & (1u << frame_index)) != 0u) {
                out->frames[frame_index].status = interrupt_status;
            }
        }
    };
    int32_t terminal_interrupt = VC_OK;
    for (uint32_t index = 0u; index < VC_VIDEO_FRAME_COUNT; ++index) {
        const bool publicly_requested =
            (requested_mask & (1u << index)) != 0u;
        const uint64_t per_frame_feature_mask =
            publicly_requested
                ? request.feature_mask & kPerFrameFeatures
                : 0u;
        if ((decode_mask & (1u << index)) == 0u) continue;
        if (format_failed) {
            if (publicly_requested) out->frames[index].status = last_failure;
            continue;
        }
#if defined(VC_VIDEO_ANALYSIS_TESTING)
        video_test_stats.attempted_frame_mask |= 1u << index;
#endif
        const uint32_t frame_timeout =
            request.frame_timeout_ms == 0u ? 20000u : request.frame_timeout_ms;
        opaque.deadline = Deadline::After(
            std::chrono::milliseconds(frame_timeout));
        opaque.last_status = VC_OK;
        vc_feature_set local = out->frames[index].features;
        ClearFeaturePayload(&local);
        GrayImage selected_gray;
        int32_t width = 0;
        int32_t height = 0;
        const int32_t status = DecodeSample(format.value,
                                            codec.value,
                                            stream,
                                            stream_index,
                                            out->frames[index].sample_time_ms,
                                            rotation,
                                            per_frame_feature_mask,
                                            opaque,
                                            &local,
                                            &width,
                                            &height,
                                            &selected_gray,
                                            &format_failed,
                                            index,
                                            true);
        int32_t recovered_status = status;
        if (!format_failed && status != VC_OK &&
            status != VC_ERR_CANCELLED && status != VC_ERR_TIMEOUT) {
            ClearFeaturePayload(&local);
            selected_gray = {};
            width = 0;
            height = 0;
            recovered_status = DecodeSample(format.value,
                                            codec.value,
                                            stream,
                                            stream_index,
                                            out->frames[index].sample_time_ms,
                                            rotation,
                                            per_frame_feature_mask,
                                            opaque,
                                            &local,
                                            &width,
                                            &height,
                                            &selected_gray,
                                            &format_failed,
                                            index,
                                            false);
        }
        if (publicly_requested) {
            out->frames[index].status = recovered_status;
        }
        if (recovered_status == VC_ERR_CANCELLED ||
            recovered_status == VC_ERR_TIMEOUT) {
            terminal_interrupt = recovered_status;
            clear_frames_for_interrupt(recovered_status);
            break;
        }
        if (recovered_status == VC_OK) {
#if defined(VC_VIDEO_ANALYSIS_TESTING)
            if (video_test_before_publish_hook != nullptr) {
                video_test_before_publish_hook(
                    index, video_test_before_publish_context);
            }
            if (video_test_force_timeout_before_publish_once) {
                video_test_force_timeout_before_publish_once = false;
                opaque.deadline = Deadline::After(
                    std::chrono::milliseconds(-1));
            }
#endif
            const int32_t before_publish =
                CheckInterrupt(cancel, opaque.deadline);
            if (before_publish == VC_ERR_CANCELLED ||
                before_publish == VC_ERR_TIMEOUT) {
                terminal_interrupt = before_publish;
                clear_frames_for_interrupt(before_publish);
                break;
            }
            if (contact_requested) {
                contact_grays[index] = std::move(selected_gray);
                contact_gray_valid[index] = true;
            }
            if (publicly_requested) {
                out->frames[index].features = local;
                out->completed_frame_mask |= 1u << index;
                ++successes;
            }
#if defined(VC_VIDEO_ANALYSIS_TESTING)
            video_test_stats.display_widths[index] = width;
            video_test_stats.display_heights[index] = height;
#endif
        } else {
            if (publicly_requested) {
                ClearFeaturePayload(&out->frames[index].features);
            }
            last_failure = recovered_status;
        }
    }
    int32_t contact_status = VC_ERR_UNSUPPORTED;
    bool contact_success = false;
    if (contact_requested && terminal_interrupt != VC_OK) {
        contact_status = terminal_interrupt;
        out->contact_sheet_status = terminal_interrupt;
        out->contact_sheet_width = 0u;
        out->contact_sheet_height = 0u;
        ClearFeaturePayload(&out->contact_sheet_features);
    } else if (contact_requested) {
        const int32_t before_contact = CheckInterrupt(cancel, opaque.deadline);
        if (before_contact == VC_ERR_CANCELLED ||
            before_contact == VC_ERR_TIMEOUT) {
            terminal_interrupt = before_contact;
            clear_frames_for_interrupt(before_contact);
            contact_status = before_contact;
        } else {
            std::array<const GrayImage*, VC_VIDEO_FRAME_COUNT> contact_frames{};
            for (uint32_t index = 0u; index < VC_VIDEO_FRAME_COUNT; ++index) {
                if (contact_gray_valid[index]) {
                    contact_frames[index] = &contact_grays[index];
                }
            }
            ContactSheetResult contact;
            contact_status = GenerateContactSheet(
                contact_frames,
                request.contact_sheet_tile_max_side,
                request.temporary_jpeg_path,
                request.temporary_jpeg_path_units,
                &contact,
                cancel,
                opaque.deadline);
            if (contact_status == VC_ERR_CANCELLED ||
                contact_status == VC_ERR_TIMEOUT) {
                terminal_interrupt = contact_status;
                clear_frames_for_interrupt(contact_status);
            } else if (contact_status == VC_OK) {
#if defined(VC_VIDEO_ANALYSIS_TESTING)
                if (video_test_after_contact_write_hook != nullptr) {
                    video_test_after_contact_write_hook(
                        video_test_after_contact_write_context);
                }
                if (video_test_force_timeout_after_contact_write_once) {
                    video_test_force_timeout_after_contact_write_once = false;
                    opaque.deadline = Deadline::After(
                        std::chrono::milliseconds(-1));
                }
#endif
                const int32_t after_contact =
                    CheckInterrupt(cancel, opaque.deadline);
                if (after_contact == VC_ERR_CANCELLED ||
                    after_contact == VC_ERR_TIMEOUT) {
                    bool deleted = false;
#if defined(VC_VIDEO_ANALYSIS_TESTING)
                    if (video_test_force_contact_delete_failure_once) {
                        video_test_force_contact_delete_failure_once = false;
                    } else
#endif
                    {
                        deleted = DeleteFileW(
                                      contact_temporary_path.c_str()) != FALSE;
                    }
                    terminal_interrupt =
                        deleted ? after_contact : VC_ERR_IO;
                    clear_frames_for_interrupt(after_contact);
                    contact_status =
                        deleted ? after_contact : VC_ERR_IO;
                } else {
                    out->contact_sheet_status = VC_OK;
                    out->contact_sheet_width =
                        static_cast<uint32_t>(contact.width);
                    out->contact_sheet_height =
                        static_cast<uint32_t>(contact.height);
                    ClearFeaturePayload(&out->contact_sheet_features);
                    std::memcpy(out->contact_sheet_features.pdq,
                                contact.features.pdq.data(),
                                contact.features.pdq.size());
                    out->contact_sheet_features.pdq_quality =
                        static_cast<uint32_t>((std::max)(
                            contact.features.quality, 0));
                    contact_success = true;
                }
            }
        }
        if (!contact_success) {
            out->contact_sheet_status = contact_status;
            out->contact_sheet_width = 0u;
            out->contact_sheet_height = 0u;
            ClearFeaturePayload(&out->contact_sheet_features);
        }
    }
    out->decode_elapsed_ms = static_cast<uint64_t>(
        std::chrono::duration_cast<std::chrono::milliseconds>(
            std::chrono::steady_clock::now() - decode_start)
            .count());
    out->operation_elapsed_ms = static_cast<uint64_t>(
        std::chrono::duration_cast<std::chrono::milliseconds>(
            std::chrono::steady_clock::now() - operation_start)
            .count());
    opaque.deadline = Deadline::Infinite();
    int32_t final_status = VC_OK;
    if (terminal_interrupt != VC_OK) {
        final_status = terminal_interrupt;
    } else if (contact_only && !contact_success) {
        final_status = contact_status;
    } else if (successes == 0u && !contact_success) {
        final_status = contact_requested ? contact_status : last_failure;
    }
    const bool terminal_was_interrupted =
        terminal_interrupt == VC_ERR_CANCELLED ||
        terminal_interrupt == VC_ERR_TIMEOUT ||
        terminal_interrupt == VC_ERR_STALE;
    const char* final_message = "";
    if (final_status != VC_OK) {
        final_message = !terminal_was_interrupted && contact_only &&
                                !contact_success
                            ? "contact sheet generation failed"
                            : "video frame decode failed";
    }
    SetError(error,
             final_status,
             final_status == VC_OK ? 0 : ffmpeg_status,
             0,
             final_message);
    return final_status;
}

#if defined(VC_VIDEO_ANALYSIS_TESTING)
int32_t VideoAnalysisTestFrameToFeatures(
    const AVFrame* frame,
    int rotation,
    int sar_num,
    int sar_den,
    vc_feature_set* features,
    int32_t* width,
    int32_t* height) noexcept {
    if (frame == nullptr || features == nullptr || width == nullptr ||
        height == nullptr) {
        return VC_ERR_INVALID_ARG;
    }
    FormatOwner format;
    format.value = avformat_alloc_context();
    if (format.value == nullptr) return VC_ERR_OOM;
    format.Track();
    AVStream* stream = avformat_new_stream(format.value, nullptr);
    if (stream == nullptr) return VC_ERR_OOM;
    stream->sample_aspect_ratio = AVRational{sar_num, sar_den};
    AVFrame* mutable_frame = const_cast<AVFrame*>(frame);
    const AVRational saved_sar = mutable_frame->sample_aspect_ratio;
    mutable_frame->sample_aspect_ratio = AVRational{sar_num, sar_den};
    GrayImage gray;
    const ImageStatus gray_status =
        FrameToGray(format.value, stream, frame, rotation, &gray);
    mutable_frame->sample_aspect_ratio = saved_sar;
    if (gray_status != ImageStatus::ok) return MapImageStatus(gray_status);
    const int32_t status = ComputeFeatures(
        gray, VC_FEATURE_PDQ | VC_FEATURE_PHASH | VC_FEATURE_SOBEL, features,
        VC_VIDEO_FRAME_COUNT);
    if (status == VC_OK) {
        *width = gray.width;
        *height = gray.height;
    }
    return status;
}
#endif

#if defined(VC_RESILIENCE_TESTING)
VideoAnalysisLiveResources VideoAnalysisTestLiveResources() noexcept {
    VideoAnalysisLiveResources resources;
    resources.formats = live_formats.load(std::memory_order_acquire);
    resources.codecs = live_codecs.load(std::memory_order_acquire);
    resources.packets = live_packets.load(std::memory_order_acquire);
    resources.frames = live_frames.load(std::memory_order_acquire);
    resources.scalers = live_scalers.load(std::memory_order_acquire);
    resources.contact = ContactSheetTestLiveResourceCount();
    return resources;
}

VideoAnalysisResourceAcquisitions
VideoAnalysisTestResourceAcquisitions() noexcept {
    VideoAnalysisResourceAcquisitions resources;
    resources.formats = acquired_formats.load(std::memory_order_acquire);
    resources.codecs = acquired_codecs.load(std::memory_order_acquire);
    resources.packets = acquired_packets.load(std::memory_order_acquire);
    resources.frames = acquired_frames.load(std::memory_order_acquire);
    resources.scalers = acquired_scalers.load(std::memory_order_acquire);
    const auto contact = ContactSheetTestResourceAcquisitions();
    resources.contact_scalers = contact.scalers;
    resources.turbo_compressors = contact.turbo_compressors;
    resources.turbo_buffers = contact.turbo_buffers;
    resources.jpeg_handles = contact.jpeg_handles;
    return resources;
}
#endif

}  // namespace vc::detail
