#ifndef VIDEOCORE_VIDEOCORE_H
#define VIDEOCORE_VIDEOCORE_H

#include <stdint.h>

#if defined(_WIN32)
#define VC_CALL __cdecl
#if defined(VC_BUILDING_DLL)
#define VC_API __declspec(dllexport)
#else
#define VC_API __declspec(dllimport)
#endif
#else
#define VC_CALL
#if defined(__GNUC__) && __GNUC__ >= 4
#define VC_API __attribute__((visibility("default")))
#else
#define VC_API
#endif
#endif

#define VC_ABI_VERSION 2u
#define VC_VERSION_STRING "2.0.0"

#define VC_SHA512_SIZE 64u
#define VC_PDQ_SIZE 32u
#define VC_PHASH_COUNT 9u
#define VC_SOBEL_HISTOGRAM_SIZE 128u
#define VC_VIDEO_FRAME_COUNT 6u
#define VC_ALL_FRAME_MASK 0x3fu
#define VC_MAX_STREAMS 256u

#define VC_OK 0
#define VC_ERR_INVALID_ARG (-1)
#define VC_ERR_ABI (-2)
#define VC_ERR_OOM (-3)
#define VC_ERR_IO (-4)
#define VC_ERR_UNSUPPORTED (-5)
#define VC_ERR_DEMUX (-6)
#define VC_ERR_DECODE (-7)
#define VC_ERR_ENCODE (-8)
#define VC_ERR_NO_FRAME (-9)
#define VC_ERR_OUTPUT_TOO_LARGE (-10)
#define VC_ERR_CANCELLED (-11)
#define VC_ERR_TIMEOUT (-12)
#define VC_ERR_STALE (-13)
#define VC_ERR_INTERNAL (-99)

#define VC_MEDIA_TYPE_AUTO 0u
#define VC_MEDIA_TYPE_IMAGE 1u
#define VC_MEDIA_TYPE_VIDEO 2u

#define VC_IO_OPERATION_READ 1u
#define VC_IO_OPERATION_SEEK 2u

#define VC_FEATURE_PDQ 0x00000001ull
#define VC_FEATURE_PHASH 0x00000002ull
#define VC_FEATURE_SOBEL 0x00000004ull
#define VC_FEATURE_DURATION 0x00000008ull
#define VC_FEATURE_CONTACT_SHEET 0x00000010ull

#define VC_CONTAINER_HAS_START_TIME       (1ull << 0)
#define VC_CONTAINER_HAS_DURATION         (1ull << 1)
#define VC_CONTAINER_HAS_BIT_RATE         (1ull << 2)
#define VC_CONTAINER_HAS_FILE_SIZE        (1ull << 3)
#define VC_CONTAINER_HAS_PROBE_SCORE      (1ull << 4)
#define VC_CONTAINER_HAS_PRIMARY_VIDEO    (1ull << 5)

#define VC_STREAM_HAS_LEVEL               (1ull << 0)
#define VC_STREAM_HAS_START_TIME          (1ull << 1)
#define VC_STREAM_HAS_DURATION            (1ull << 2)
#define VC_STREAM_HAS_BIT_RATE            (1ull << 3)
#define VC_STREAM_HAS_FRAME_COUNT         (1ull << 4)
#define VC_STREAM_HAS_BIT_DEPTH           (1ull << 5)
#define VC_STREAM_HAS_WIDTH               (1ull << 6)
#define VC_STREAM_HAS_HEIGHT              (1ull << 7)
#define VC_STREAM_HAS_ROTATION            (1ull << 8)
#define VC_STREAM_HAS_SAMPLE_RATE         (1ull << 9)
#define VC_STREAM_HAS_CHANNELS            (1ull << 10)
#define VC_STREAM_HAS_AUDIO_BIT_DEPTH     (1ull << 11)

#define VC_STREAM_MEDIA_TYPE_VIDEO 1u
#define VC_STREAM_MEDIA_TYPE_AUDIO 2u
#define VC_STREAM_MEDIA_TYPE_SUBTITLE 3u
#define VC_STREAM_MEDIA_TYPE_DATA 4u
#define VC_STREAM_MEDIA_TYPE_ATTACHMENT 5u

#ifdef __cplusplus
extern "C" {
#endif

typedef struct vc_error {
    uint32_t struct_size;
    uint32_t abi_version;
    int32_t code;
    int32_t ffmpeg_code;
    uint32_t win32_code;
    char message_utf8[512];
} vc_error;

typedef int32_t (VC_CALL *vc_io_acquire_fn)(
    uintptr_t context,
    uint32_t operation,
    uint64_t requested_bytes,
    uint64_t* lease_id,
    uint64_t* granted_bytes,
    vc_error* err);
typedef void (VC_CALL *vc_io_report_fn)(
    uintptr_t context,
    uint64_t lease_id,
    uint64_t actual_bytes,
    uint64_t elapsed_ns,
    int32_t status);

typedef struct vc_io_governor {
    uint32_t struct_size;
    uint32_t abi_version;
    uintptr_t context;
    vc_io_acquire_fn acquire;
    vc_io_report_fn report;
} vc_io_governor;

struct vc_runtime_info {
    uint32_t struct_size;
    uint32_t abi_version;
    char videocore_version_utf8[32];
    char ffmpeg_build_id_utf8[64];
    uint32_t avformat_header_version;
    uint32_t avformat_runtime_version;
    uint32_t avcodec_header_version;
    uint32_t avcodec_runtime_version;
    uint32_t avutil_header_version;
    uint32_t avutil_runtime_version;
    uint32_t swscale_header_version;
    uint32_t swscale_runtime_version;
};

typedef struct vc_media_open_options {
    uint32_t struct_size;
    uint32_t abi_version;
    uint32_t expected_media_type;
    uint32_t reserved_flags;
    uint64_t image_max_bytes;
    uint32_t operation_timeout_ms;
    uint32_t reserved_0;
    const vc_io_governor* io_governor;
} vc_media_open_options;

typedef struct vc_feature_set {
    uint32_t struct_size;
    uint32_t abi_version;
    uint8_t pdq[VC_PDQ_SIZE];
    uint32_t pdq_quality;
    uint32_t reserved_0;
    uint64_t phash[VC_PHASH_COUNT];
    float sobel_histogram[VC_SOBEL_HISTOGRAM_SIZE];
} vc_feature_set;

typedef struct vc_video_frame_result {
    uint32_t struct_size;
    uint32_t abi_version;
    uint32_t standard_index;
    int32_t status;
    int64_t sample_time_ms;
    vc_feature_set features;
} vc_video_frame_result;

typedef struct vc_video_container_info {
    uint32_t struct_size;
    uint32_t abi_version;
    uint64_t present_mask;
    int64_t start_time_us;
    int64_t duration_us;
    int64_t bit_rate;
    int64_t file_size;
    int32_t probe_score;
    int32_t primary_video_stream;
    char format_name_utf8[128];
    char format_long_name_utf8[256];
    char decoder_name_utf8[128];
} vc_video_container_info;

typedef struct vc_video_stream_info {
    uint32_t struct_size;
    uint32_t abi_version;
    uint64_t present_mask;
    uint32_t stream_index;
    uint32_t media_type;
    int32_t codec_id;
    int32_t level;
    int64_t start_time_us;
    int64_t duration_us;
    int64_t bit_rate;
    int64_t frame_count;
    uint32_t disposition;
    int32_t bit_depth;
    int32_t width;
    int32_t height;
    int32_t rotation;
    int32_t sample_rate;
    int32_t channels;
    int32_t audio_bit_depth;
    char codec_name_utf8[128];
    char codec_long_name_utf8[256];
    char codec_tag_utf8[32];
    char profile_utf8[128];
    char time_base_utf8[32];
    char language_utf8[64];
    char title_utf8[256];
    char pixel_format_utf8[64];
    char sar_utf8[32];
    char dar_utf8[32];
    char avg_frame_rate_utf8[32];
    char real_frame_rate_utf8[32];
    char color_range_utf8[32];
    char color_space_utf8[32];
    char color_transfer_utf8[32];
    char color_primaries_utf8[32];
    char chroma_location_utf8[32];
    char field_order_utf8[32];
    char sample_format_utf8[64];
    char channel_layout_utf8[128];
} vc_video_stream_info;

typedef struct vc_analysis_request {
    uint32_t struct_size;
    uint32_t abi_version;
    uint64_t feature_mask;
    uint32_t frame_mask;
    uint32_t reserved_flags;
    int64_t known_duration_ms;
    uint32_t probe_timeout_ms;
    uint32_t frame_timeout_ms;
    uint32_t contact_sheet_tile_max_side;
    uint32_t reserved_0;
    const uint16_t* temporary_jpeg_path;
    uint32_t temporary_jpeg_path_units;
    uint32_t reserved_1;
} vc_analysis_request;

typedef struct vc_analysis_result {
    uint32_t struct_size;
    uint32_t abi_version;
    uint32_t media_type;
    uint32_t reserved_flags;
    int64_t duration_ms;
    int32_t duration_status;
    int32_t image_status;
    int32_t contact_sheet_status;
    /* Image media: decoded image size. Video media: contact-sheet size. */
    uint32_t contact_sheet_width;
    uint32_t contact_sheet_height;
    uint32_t completed_frame_mask;
    vc_feature_set image_features;
    vc_feature_set contact_sheet_features;
    vc_video_frame_result frames[VC_VIDEO_FRAME_COUNT];
    uint64_t operation_elapsed_ms;
    uint64_t decode_elapsed_ms;
} vc_analysis_result;

typedef struct vc_cancel_token vc_cancel_token;
typedef struct vc_media_session vc_media_session;

VC_API uint32_t VC_CALL vc_abi_version(void);
VC_API const char* VC_CALL vc_version(void);
VC_API int32_t VC_CALL vc_runtime_info(
    struct vc_runtime_info* out,
    vc_error* err);

VC_API int32_t VC_CALL vc_cancel_create(
    vc_cancel_token** out,
    vc_error* err);
VC_API void VC_CALL vc_cancel_request(vc_cancel_token* token);
VC_API void VC_CALL vc_cancel_free(vc_cancel_token* token);

VC_API int32_t VC_CALL vc_media_open_w(
    const uint16_t* path,
    uint32_t path_units,
    const vc_media_open_options* options,
    vc_cancel_token* cancel,
    vc_media_session** out,
    vc_error* err);

VC_API int32_t VC_CALL vc_media_hash(
    vc_media_session* session,
    uint8_t out_sha512[VC_SHA512_SIZE],
    vc_error* err);

VC_API int32_t VC_CALL vc_media_analyze(
    vc_media_session* session,
    const vc_analysis_request* request,
    vc_analysis_result* out,
    vc_error* err);

VC_API int32_t VC_CALL vc_media_container_info(
    vc_media_session* session,
    vc_video_container_info* out,
    vc_error* err);
VC_API uint32_t VC_CALL vc_media_stream_count(vc_media_session* session);
VC_API int32_t VC_CALL vc_media_stream_info(
    vc_media_session* session,
    uint32_t ordinal,
    vc_video_stream_info* out,
    vc_error* err);
VC_API int32_t VC_CALL vc_media_metadata_json(
    vc_media_session* session,
    int32_t stream_index,
    char* dst,
    uint32_t capacity,
    uint32_t* required,
    vc_error* err);

VC_API void VC_CALL vc_media_close(vc_media_session* session);

#ifdef __cplusplus
}
#endif

#endif
