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

#define VC_ABI_VERSION 1u
#define VC_VERSION_STRING "1.0.0"

#define VC_SHA512_SIZE 64u
#define VC_PDQ_SIZE 32u
#define VC_PHASH_COUNT 9u
#define VC_SOBEL_HISTOGRAM_SIZE 128u
#define VC_VIDEO_FRAME_COUNT 6u
#define VC_ALL_FRAME_MASK 0x3fu

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

#define VC_FEATURE_PDQ 0x00000001ull
#define VC_FEATURE_PHASH 0x00000002ull
#define VC_FEATURE_SOBEL 0x00000004ull
#define VC_FEATURE_DURATION 0x00000008ull
#define VC_FEATURE_CONTACT_SHEET 0x00000010ull

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
    uint32_t contact_sheet_width;
    uint32_t contact_sheet_height;
    uint32_t completed_frame_mask;
    vc_feature_set image_features;
    vc_feature_set contact_sheet_features;
    vc_video_frame_result frames[VC_VIDEO_FRAME_COUNT];
    uint64_t operation_elapsed_ms;
    uint64_t decode_elapsed_ms;
    uint32_t image_width;
    uint32_t image_height;
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

VC_API void VC_CALL vc_media_close(vc_media_session* session);

#ifdef __cplusplus
}
#endif

#endif
