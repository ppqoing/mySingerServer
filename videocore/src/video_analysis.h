#ifndef VIDEOCORE_SRC_VIDEO_ANALYSIS_H
#define VIDEOCORE_SRC_VIDEO_ANALYSIS_H

#include <array>
#include <cstdint>

#include "videocore/videocore.h"

struct AVFrame;

namespace vc::detail {

class AvioBridge;
struct CancelState;

int32_t AnalyzeVideo(AvioBridge* avio,
                     const CancelState* cancel,
                     const vc_analysis_request& request,
                     vc_analysis_result* out,
                     vc_error* error) noexcept;
int32_t PublishVideoFailure(vc_analysis_result* out,
                            vc_error* error,
                            int32_t code,
                            const char* message) noexcept;

#if defined(VC_VIDEO_ANALYSIS_TESTING)
struct VideoAnalysisTestStats {
    uint64_t format_contexts = 0u;
    uint64_t codec_contexts = 0u;
    uint32_t attempted_frame_mask = 0u;
    uint32_t send_packet_attempts = 0u;
    uint32_t forced_send_eagain = 0u;
    uint32_t same_packet_resends = 0u;
    uint32_t hard_failure_count = 0u;
    uint32_t seek_call_count = 0u;
    uint32_t recovery_seek_call_count = 0u;
    uint32_t injected_read_error_count = 0u;
    uint32_t planned_successful_read_count = 0u;
    uint32_t injected_seek_error_count = 0u;
    int32_t recovery_seek_stream_index = -1;
    int32_t recovery_seek_flags = 0;
    int64_t recovery_seek_min = 0;
    int64_t recovery_seek_target = 0;
    int64_t recovery_seek_max = 0;
    std::array<int32_t, VC_VIDEO_FRAME_COUNT> display_widths{};
    std::array<int32_t, VC_VIDEO_FRAME_COUNT> display_heights{};
    std::array<uint32_t, VC_VIDEO_FRAME_COUNT> gray_conversion_counts{};
    std::array<uint32_t, VC_VIDEO_FRAME_COUNT> pdq_compute_counts{};
    std::array<uint32_t, VC_VIDEO_FRAME_COUNT> phash_compute_counts{};
    std::array<uint32_t, VC_VIDEO_FRAME_COUNT> sobel_compute_counts{};
    std::array<int64_t, VC_VIDEO_FRAME_COUNT> selected_pts{};
    std::array<int64_t, VC_VIDEO_FRAME_COUNT> selected_pts_time_micros{};
    std::array<int32_t, VC_VIDEO_FRAME_COUNT> selected_decode_ordinals{};
    std::array<uint8_t, VC_VIDEO_FRAME_COUNT> selected_key_frames{};
    std::array<char, VC_VIDEO_FRAME_COUNT> selected_picture_types{};
};

using VideoAnalysisBeforePublishHook = void (*)(uint32_t frame_index,
                                                void* context) noexcept;
using VideoAnalysisAfterContactWriteHook = void (*)(void* context) noexcept;

void VideoAnalysisTestReset() noexcept;
VideoAnalysisTestStats VideoAnalysisTestGetStats() noexcept;
void VideoAnalysisTestForceSendEagainOnce() noexcept;
void VideoAnalysisTestForceTimeoutBeforePublishOnce() noexcept;
void VideoAnalysisTestForceTimeoutAfterContactWriteOnce() noexcept;
void VideoAnalysisTestForceContactDeleteFailureOnce() noexcept;
void VideoAnalysisTestForceHardReadFailureAt(uint32_t frame_index) noexcept;
void VideoAnalysisTestForceHardSeekFailureAt(uint32_t frame_index) noexcept;
void VideoAnalysisTestInjectReadError(uint32_t frame_index,
                                      int32_t ffmpeg_error,
                                      uint32_t repetitions) noexcept;
void VideoAnalysisTestInjectReadPlan(uint32_t frame_index,
                                     const int32_t* ffmpeg_errors,
                                     uint32_t count) noexcept;
void VideoAnalysisTestInjectSeekError(uint32_t frame_index,
                                      int32_t ffmpeg_error,
                                      uint32_t repetitions) noexcept;
void VideoAnalysisTestOverrideStreamStart(int64_t start) noexcept;
int64_t VideoAnalysisTestTargetTimestamp(int64_t relative,
                                         int64_t start) noexcept;
int64_t VideoAnalysisTestNormalizedTimestamp(int64_t timestamp,
                                             int64_t start) noexcept;
int32_t VideoAnalysisTestFrameToFeatures(
    const AVFrame* frame,
    int rotation,
    int sar_num,
    int sar_den,
    vc_feature_set* features,
    int32_t* width,
    int32_t* height) noexcept;
void VideoAnalysisTestSetBeforePublishHook(
    VideoAnalysisBeforePublishHook hook,
    void* context) noexcept;
void VideoAnalysisTestSetAfterContactWriteHook(
    VideoAnalysisAfterContactWriteHook hook,
    void* context) noexcept;
#endif

#if defined(VC_RESILIENCE_TESTING)
struct VideoAnalysisLiveResources {
    uint64_t formats = 0u;
    uint64_t codecs = 0u;
    uint64_t packets = 0u;
    uint64_t frames = 0u;
    uint64_t scalers = 0u;
    uint64_t contact = 0u;

    uint64_t Total() const noexcept {
        return formats + codecs + packets + frames + scalers + contact;
    }
};

struct VideoAnalysisResourceAcquisitions {
    uint64_t formats = 0u;
    uint64_t codecs = 0u;
    uint64_t packets = 0u;
    uint64_t frames = 0u;
    uint64_t scalers = 0u;
    uint64_t contact_scalers = 0u;
    uint64_t turbo_compressors = 0u;
    uint64_t turbo_buffers = 0u;
    uint64_t jpeg_handles = 0u;
};

VideoAnalysisLiveResources VideoAnalysisTestLiveResources() noexcept;
VideoAnalysisResourceAcquisitions
VideoAnalysisTestResourceAcquisitions() noexcept;
#endif

}  // namespace vc::detail

#endif
