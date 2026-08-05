#include <algorithm>
#include <array>
#include <cstddef>
#include <cstdint>
#include <cstring>
#include <iostream>
#include <new>
#include <stdexcept>
#include <string>
#include <type_traits>
#include <vector>

#if __has_include("videocore/videocore.h")
#include "videocore/videocore.h"
#define VC_TESTING_REAL_HEADER 1
#else
#define VC_TESTING_REAL_HEADER 0
#define VC_ABI_VERSION 1u
#define VC_VERSION_STRING "1.0.0"
#define VC_SHA512_SIZE 64u
#define VC_PDQ_SIZE 32u
#define VC_PHASH_COUNT 9u
#define VC_SOBEL_HISTOGRAM_SIZE 128u
#define VC_VIDEO_FRAME_COUNT 6u
#define VC_ALL_FRAME_MASK 0x3fu
#define VC_OK 0
#define VC_ERR_INVALID_ARG -1
#define VC_ERR_ABI -2
#define VC_ERR_OOM -3
#define VC_ERR_IO -4
#define VC_ERR_UNSUPPORTED -5
#define VC_ERR_DEMUX -6
#define VC_ERR_DECODE -7
#define VC_ERR_ENCODE -8
#define VC_ERR_NO_FRAME -9
#define VC_ERR_OUTPUT_TOO_LARGE -10
#define VC_ERR_CANCELLED -11
#define VC_ERR_TIMEOUT -12
#define VC_ERR_STALE -13
#define VC_ERR_INTERNAL -99
#define VC_CALL __cdecl

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
} vc_analysis_result;

typedef struct vc_cancel_token vc_cancel_token;
typedef struct vc_media_session vc_media_session;

extern "C" {
uint32_t VC_CALL vc_abi_version(void);
const char* VC_CALL vc_version(void);
int32_t VC_CALL vc_runtime_info(struct vc_runtime_info* out, vc_error* err);
int32_t VC_CALL vc_cancel_create(vc_cancel_token** out, vc_error* err);
void VC_CALL vc_cancel_request(vc_cancel_token* token);
void VC_CALL vc_cancel_free(vc_cancel_token* token);
int32_t VC_CALL vc_media_open_w(const uint16_t* path,
                                uint32_t path_units,
                                const vc_media_open_options* options,
                                vc_cancel_token* cancel,
                                vc_media_session** out,
                                vc_error* err);
int32_t VC_CALL vc_media_hash(vc_media_session* session,
                              uint8_t out_sha512[VC_SHA512_SIZE],
                              vc_error* err);
int32_t VC_CALL vc_media_analyze(vc_media_session* session,
                                 const vc_analysis_request* request,
                                 vc_analysis_result* out,
                                 vc_error* err);
void VC_CALL vc_media_close(vc_media_session* session);
}
#endif

#if VC_TESTING_REAL_HEADER && __has_include("../src/error.h")
#include "../src/error.h"
#define VC_TESTING_ERROR_BOUNDARY 1
#else
#define VC_TESTING_ERROR_BOUNDARY 0
#endif

typedef struct vc_runtime_info vc_runtime_info_layout;

#ifdef _WIN32
#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#endif

namespace {

int failures = 0;
using RuntimeInfoLayout = vc_runtime_info_layout;

void Check(bool condition, const char* message) {
    if (!condition) {
        std::cerr << "FAIL: " << message << '\n';
        ++failures;
    }
}

void CheckCase(bool condition,
               const char* case_name,
               const char* expectation) {
    if (!condition) {
        std::cerr << "FAIL: " << case_name << ": " << expectation << '\n';
        ++failures;
    }
}

vc_error FreshError() {
    vc_error error{};
    error.struct_size = sizeof(error);
    error.abi_version = VC_ABI_VERSION;
    std::memset(error.message_utf8, 'x', sizeof(error.message_utf8));
    return error;
}

vc_media_open_options FreshOpenOptions() {
    vc_media_open_options options{};
    options.struct_size = sizeof(options);
    options.abi_version = VC_ABI_VERSION;
    return options;
}

vc_analysis_request FreshAnalysisRequest() {
    vc_analysis_request request{};
    request.struct_size = sizeof(request);
    request.abi_version = VC_ABI_VERSION;
    return request;
}

void InitializeFeatureSet(vc_feature_set& features) {
    features.struct_size = sizeof(features);
    features.abi_version = VC_ABI_VERSION;
}

vc_analysis_result FreshAnalysisResult() {
    vc_analysis_result result{};
    result.struct_size = sizeof(result);
    result.abi_version = VC_ABI_VERSION;
    InitializeFeatureSet(result.image_features);
    InitializeFeatureSet(result.contact_sheet_features);
    for (uint32_t index = 0; index < VC_VIDEO_FRAME_COUNT; ++index) {
        result.frames[index].struct_size = sizeof(result.frames[index]);
        result.frames[index].abi_version = VC_ABI_VERSION;
        InitializeFeatureSet(result.frames[index].features);
    }
    return result;
}

template <typename Value>
struct GuardedValue {
    std::array<uint8_t, 16> before;
    Value value;
    std::array<uint8_t, 16> after;
};

template <typename Value>
GuardedValue<Value> MakeGuarded(const Value& value) {
    GuardedValue<Value> guarded{};
    guarded.before.fill(0x5au);
    guarded.value = value;
    guarded.after.fill(0xc3u);
    return guarded;
}

template <typename Value>
void CheckGuardedUnchanged(const GuardedValue<Value>& guarded,
                           const Value& snapshot,
                           const char* message) {
    Check(std::all_of(guarded.before.begin(),
                      guarded.before.end(),
                      [](uint8_t byte) { return byte == 0x5au; }),
          message);
    Check(std::memcmp(&guarded.value, &snapshot, sizeof(Value)) == 0,
          message);
    Check(std::all_of(guarded.after.begin(),
                      guarded.after.end(),
                      [](uint8_t byte) { return byte == 0xc3u; }),
          message);
}

template <typename Value>
void CheckGuardsIntact(const GuardedValue<Value>& guarded,
                       const char* message) {
    Check(std::all_of(guarded.before.begin(),
                      guarded.before.end(),
                      [](uint8_t byte) { return byte == 0x5au; }),
          message);
    Check(std::all_of(guarded.after.begin(),
                      guarded.after.end(),
                      [](uint8_t byte) { return byte == 0xc3u; }),
          message);
}

void TestConstants() {
    Check(VC_ABI_VERSION == 1u, "VC_ABI_VERSION must be 1");
    Check(std::strcmp(VC_VERSION_STRING, "1.0.0") == 0,
          "VC_VERSION_STRING must be 1.0.0");
    Check(VC_SHA512_SIZE == 64u, "SHA-512 size must be 64");
    Check(VC_PDQ_SIZE == 32u, "PDQ size must be 32");
    Check(VC_PHASH_COUNT == 9u, "pHash count must be 9");
    Check(VC_SOBEL_HISTOGRAM_SIZE == 128u,
          "Sobel histogram size must be 128");
    Check(VC_VIDEO_FRAME_COUNT == 6u, "video frame count must be 6");
    Check(VC_ALL_FRAME_MASK == 0x3fu, "all-frame mask must be 0x3f");

    const int32_t statuses[] = {
        VC_OK,
        VC_ERR_INVALID_ARG,
        VC_ERR_ABI,
        VC_ERR_OOM,
        VC_ERR_IO,
        VC_ERR_UNSUPPORTED,
        VC_ERR_DEMUX,
        VC_ERR_DECODE,
        VC_ERR_ENCODE,
        VC_ERR_NO_FRAME,
        VC_ERR_OUTPUT_TOO_LARGE,
        VC_ERR_CANCELLED,
        VC_ERR_TIMEOUT,
        VC_ERR_STALE,
        VC_ERR_INTERNAL,
    };
    const int32_t expected[] = {
        0, -1, -2, -3, -4, -5, -6, -7,
        -8, -9, -10, -11, -12, -13, -99,
    };
    Check(std::equal(std::begin(statuses), std::end(statuses),
                     std::begin(expected)),
          "status values must exactly match ABI 1");
}

void TestLayouts() {
    static_assert(std::is_standard_layout<vc_error>::value);
    static_assert(std::is_standard_layout<vc_media_open_options>::value);
    static_assert(std::is_standard_layout<vc_feature_set>::value);
    static_assert(std::is_standard_layout<vc_video_frame_result>::value);
    static_assert(std::is_standard_layout<vc_analysis_request>::value);
    static_assert(std::is_standard_layout<vc_analysis_result>::value);
    static_assert(std::is_standard_layout<struct vc_runtime_info>::value);

    static_assert(alignof(vc_error) == 4u);
    static_assert(offsetof(vc_error, struct_size) == 0u);
    static_assert(offsetof(vc_error, abi_version) == 4u);
    static_assert(offsetof(vc_error, code) == 8u);
    static_assert(offsetof(vc_error, ffmpeg_code) == 12u);
    static_assert(offsetof(vc_error, win32_code) == 16u);
    static_assert(offsetof(vc_error, message_utf8) == 20u);

    static_assert(alignof(RuntimeInfoLayout) == 4u);
    static_assert(offsetof(RuntimeInfoLayout, struct_size) == 0u);
    static_assert(offsetof(RuntimeInfoLayout, abi_version) == 4u);
    static_assert(offsetof(RuntimeInfoLayout, videocore_version_utf8) == 8u);
    static_assert(offsetof(RuntimeInfoLayout, ffmpeg_build_id_utf8) == 40u);
    static_assert(offsetof(RuntimeInfoLayout, avformat_header_version) == 104u);
    static_assert(offsetof(RuntimeInfoLayout, avformat_runtime_version) == 108u);
    static_assert(offsetof(RuntimeInfoLayout, avcodec_header_version) == 112u);
    static_assert(offsetof(RuntimeInfoLayout, avcodec_runtime_version) == 116u);
    static_assert(offsetof(RuntimeInfoLayout, avutil_header_version) == 120u);
    static_assert(offsetof(RuntimeInfoLayout, avutil_runtime_version) == 124u);
    static_assert(offsetof(RuntimeInfoLayout, swscale_header_version) == 128u);
    static_assert(offsetof(RuntimeInfoLayout, swscale_runtime_version) == 132u);

    static_assert(alignof(vc_media_open_options) == 8u);
    static_assert(offsetof(vc_media_open_options, struct_size) == 0u);
    static_assert(offsetof(vc_media_open_options, abi_version) == 4u);
    static_assert(offsetof(vc_media_open_options, expected_media_type) == 8u);
    static_assert(offsetof(vc_media_open_options, reserved_flags) == 12u);
    static_assert(offsetof(vc_media_open_options, image_max_bytes) == 16u);
    static_assert(offsetof(vc_media_open_options, operation_timeout_ms) == 24u);
    static_assert(offsetof(vc_media_open_options, reserved_0) == 28u);

    static_assert(alignof(vc_feature_set) == 8u);
    static_assert(offsetof(vc_feature_set, struct_size) == 0u);
    static_assert(offsetof(vc_feature_set, abi_version) == 4u);
    static_assert(offsetof(vc_feature_set, pdq) == 8u);
    static_assert(offsetof(vc_feature_set, pdq_quality) == 40u);
    static_assert(offsetof(vc_feature_set, reserved_0) == 44u);
    static_assert(offsetof(vc_feature_set, phash) == 48u);
    static_assert(offsetof(vc_feature_set, sobel_histogram) == 120u);

    static_assert(alignof(vc_video_frame_result) == 8u);
    static_assert(offsetof(vc_video_frame_result, struct_size) == 0u);
    static_assert(offsetof(vc_video_frame_result, abi_version) == 4u);
    static_assert(offsetof(vc_video_frame_result, standard_index) == 8u);
    static_assert(offsetof(vc_video_frame_result, status) == 12u);
    static_assert(offsetof(vc_video_frame_result, sample_time_ms) == 16u);
    static_assert(offsetof(vc_video_frame_result, features) == 24u);

    static_assert(alignof(vc_analysis_request) == 8u);
    static_assert(offsetof(vc_analysis_request, struct_size) == 0u);
    static_assert(offsetof(vc_analysis_request, abi_version) == 4u);
    static_assert(offsetof(vc_analysis_request, feature_mask) == 8u);
    static_assert(offsetof(vc_analysis_request, frame_mask) == 16u);
    static_assert(offsetof(vc_analysis_request, reserved_flags) == 20u);
    static_assert(offsetof(vc_analysis_request, known_duration_ms) == 24u);
    static_assert(offsetof(vc_analysis_request, probe_timeout_ms) == 32u);
    static_assert(offsetof(vc_analysis_request, frame_timeout_ms) == 36u);
    static_assert(
        offsetof(vc_analysis_request, contact_sheet_tile_max_side) == 40u);
    static_assert(offsetof(vc_analysis_request, reserved_0) == 44u);
    static_assert(offsetof(vc_analysis_request, temporary_jpeg_path) == 48u);
    static_assert(
        offsetof(vc_analysis_request, temporary_jpeg_path_units) == 56u);
    static_assert(offsetof(vc_analysis_request, reserved_1) == 60u);

    static_assert(alignof(vc_analysis_result) == 8u);
    static_assert(offsetof(vc_analysis_result, struct_size) == 0u);
    static_assert(offsetof(vc_analysis_result, abi_version) == 4u);
    static_assert(offsetof(vc_analysis_result, media_type) == 8u);
    static_assert(offsetof(vc_analysis_result, reserved_flags) == 12u);
    static_assert(offsetof(vc_analysis_result, duration_ms) == 16u);
    static_assert(offsetof(vc_analysis_result, duration_status) == 24u);
    static_assert(offsetof(vc_analysis_result, image_status) == 28u);
    static_assert(offsetof(vc_analysis_result, contact_sheet_status) == 32u);
    static_assert(offsetof(vc_analysis_result, contact_sheet_width) == 36u);
    static_assert(offsetof(vc_analysis_result, contact_sheet_height) == 40u);
    static_assert(offsetof(vc_analysis_result, completed_frame_mask) == 44u);
    static_assert(offsetof(vc_analysis_result, image_features) == 48u);
    static_assert(
        offsetof(vc_analysis_result, contact_sheet_features) == 680u);
    static_assert(offsetof(vc_analysis_result, frames) == 1312u);
    static_assert(
        offsetof(vc_analysis_result, operation_elapsed_ms) == 5248u);
    static_assert(offsetof(vc_analysis_result, decode_elapsed_ms) == 5256u);

    Check(sizeof(vc_error) == 532u, "vc_error size");
    Check(offsetof(vc_error, struct_size) == 0u, "vc_error struct_size offset");
    Check(offsetof(vc_error, abi_version) == 4u, "vc_error abi_version offset");
    Check(offsetof(vc_error, message_utf8) == 20u, "vc_error message offset");

    Check(sizeof(struct vc_runtime_info) == 136u, "runtime info size");
    Check(offsetof(RuntimeInfoLayout, struct_size) == 0u,
          "runtime info struct_size offset");
    Check(offsetof(RuntimeInfoLayout, abi_version) == 4u,
          "runtime info abi_version offset");
    Check(offsetof(RuntimeInfoLayout, videocore_version_utf8) == 8u,
          "runtime info version offset");
    Check(offsetof(RuntimeInfoLayout, ffmpeg_build_id_utf8) == 40u,
          "runtime info FFmpeg ID offset");
    Check(offsetof(RuntimeInfoLayout, avformat_header_version) == 104u,
          "runtime info component offset");

    Check(sizeof(vc_media_open_options) == 32u, "open options size");
    Check(offsetof(vc_media_open_options, struct_size) == 0u,
          "open options struct_size offset");
    Check(offsetof(vc_media_open_options, abi_version) == 4u,
          "open options abi_version offset");
    Check(offsetof(vc_media_open_options, image_max_bytes) == 16u,
          "open options image limit offset");

    Check(sizeof(((vc_feature_set*)nullptr)->pdq) == 32u,
          "feature PDQ array size");
    Check(sizeof(((vc_feature_set*)nullptr)->phash) == 72u,
          "feature pHash array size");
    Check(sizeof(((vc_feature_set*)nullptr)->sobel_histogram) == 512u,
          "feature Sobel array size");
    Check(offsetof(vc_feature_set, struct_size) == 0u,
          "feature set struct_size offset");
    Check(offsetof(vc_feature_set, abi_version) == 4u,
          "feature set abi_version offset");
    Check(sizeof(vc_feature_set) == 632u, "feature set size");

    Check(offsetof(vc_video_frame_result, struct_size) == 0u,
          "frame result struct_size offset");
    Check(offsetof(vc_video_frame_result, abi_version) == 4u,
          "frame result abi_version offset");
    Check(sizeof(vc_video_frame_result) == 656u, "frame result size");

    Check(offsetof(vc_analysis_request, struct_size) == 0u,
          "analysis request struct_size offset");
    Check(offsetof(vc_analysis_request, abi_version) == 4u,
          "analysis request abi_version offset");
    Check(sizeof(vc_analysis_request) == 64u, "analysis request size");

    Check(offsetof(vc_analysis_result, struct_size) == 0u,
          "analysis result struct_size offset");
    Check(offsetof(vc_analysis_result, abi_version) == 4u,
          "analysis result abi_version offset");
    Check(sizeof(((vc_analysis_result*)nullptr)->frames) /
              sizeof(vc_video_frame_result) ==
              6u,
          "analysis result frame slot count");
    Check(sizeof(vc_analysis_result) == 5264u, "analysis result size");

    std::cout << "ABI_LAYOUT"
              << " vc_error=" << sizeof(vc_error)
              << " runtime_info=" << sizeof(struct vc_runtime_info)
              << " open_options=" << sizeof(vc_media_open_options)
              << " feature_set=" << sizeof(vc_feature_set)
              << " frame_result=" << sizeof(vc_video_frame_result)
              << " analysis_request=" << sizeof(vc_analysis_request)
              << " analysis_result=" << sizeof(vc_analysis_result)
              << " message_offset=" << offsetof(vc_error, message_utf8)
              << " open_header_offsets="
              << offsetof(vc_media_open_options, struct_size) << '/'
              << offsetof(vc_media_open_options, abi_version)
              << " feature_header_offsets="
              << offsetof(vc_feature_set, struct_size) << '/'
              << offsetof(vc_feature_set, abi_version)
              << " frame_header_offsets="
              << offsetof(vc_video_frame_result, struct_size) << '/'
              << offsetof(vc_video_frame_result, abi_version)
              << " request_header_offsets="
              << offsetof(vc_analysis_request, struct_size) << '/'
              << offsetof(vc_analysis_request, abi_version)
              << " result_header_offsets="
              << offsetof(vc_analysis_result, struct_size) << '/'
              << offsetof(vc_analysis_result, abi_version)
              << " runtime_versions_offset="
              << offsetof(RuntimeInfoLayout, avformat_header_version)
              << '\n';
}

void TestCallingConventionAndVersions() {
    using AbiVersionFunction = uint32_t(VC_CALL*)(void);
    using VersionFunction = const char*(VC_CALL*)(void);
    using RuntimeInfoFunction = int32_t(VC_CALL*)(
        struct vc_runtime_info*, vc_error*);
    using CancelCreateFunction = int32_t(VC_CALL*)(
        vc_cancel_token**, vc_error*);
    using CancelVoidFunction = void(VC_CALL*)(vc_cancel_token*);
    using MediaOpenFunction = int32_t(VC_CALL*)(
        const uint16_t*,
        uint32_t,
        const vc_media_open_options*,
        vc_cancel_token*,
        vc_media_session**,
        vc_error*);
    using MediaHashFunction = int32_t(VC_CALL*)(
        vc_media_session*, uint8_t*, vc_error*);
    using MediaAnalyzeFunction = int32_t(VC_CALL*)(
        vc_media_session*,
        const vc_analysis_request*,
        vc_analysis_result*,
        vc_error*);
    using MediaCloseFunction = void(VC_CALL*)(vc_media_session*);
    static_assert(std::is_same<decltype(&vc_abi_version),
                               AbiVersionFunction>::value);
    static_assert(std::is_same<decltype(&vc_version), VersionFunction>::value);
    static_assert(std::is_same<decltype(&vc_runtime_info),
                               RuntimeInfoFunction>::value);
    static_assert(std::is_same<decltype(&vc_cancel_create),
                               CancelCreateFunction>::value);
    static_assert(std::is_same<decltype(&vc_cancel_request),
                               CancelVoidFunction>::value);
    static_assert(std::is_same<decltype(&vc_cancel_free),
                               CancelVoidFunction>::value);
    static_assert(std::is_same<decltype(&vc_media_open_w),
                               MediaOpenFunction>::value);
    static_assert(std::is_same<decltype(&vc_media_hash),
                               MediaHashFunction>::value);
    static_assert(std::is_same<decltype(&vc_media_analyze),
                               MediaAnalyzeFunction>::value);
    static_assert(std::is_same<decltype(&vc_media_close),
                               MediaCloseFunction>::value);

    Check(vc_abi_version() == 1u, "vc_abi_version result");
    Check(std::strcmp(vc_version(), "1.0.0") == 0, "vc_version result");
}

void TestSafeFailureShells() {
    vc_error error = FreshError();
    struct vc_runtime_info runtime{};
    runtime.struct_size = sizeof(runtime) - 1u;
    runtime.abi_version = VC_ABI_VERSION;
    Check(vc_runtime_info(&runtime, &error) == VC_ERR_ABI,
          "undersized runtime info must be rejected");
    Check(error.code == VC_ERR_ABI, "undersized runtime error code");
    Check(error.message_utf8[sizeof(error.message_utf8) - 1u] == '\0',
          "error message must always be NUL terminated");

    alignas(vc_error) uint8_t bounded_error_storage[sizeof(vc_error) + 8u];
    std::memset(bounded_error_storage, 0xa5, sizeof(bounded_error_storage));
    auto* bounded_error =
        reinterpret_cast<vc_error*>(bounded_error_storage);
    bounded_error->struct_size =
        static_cast<uint32_t>(offsetof(vc_error, message_utf8) + 1u);
    bounded_error->abi_version = VC_ABI_VERSION;
    runtime.struct_size = sizeof(runtime);
    runtime.abi_version = VC_ABI_VERSION;
    Check(vc_runtime_info(&runtime, bounded_error) == VC_ERR_ABI,
          "undersized error structure must be rejected");
    Check(bounded_error->message_utf8[0] == '\0',
          "bounded error message must be NUL terminated");
    Check(bounded_error_storage[bounded_error->struct_size] == 0xa5,
          "error writer must not exceed caller-declared structure size");

    error = FreshError();
    error.abi_version = VC_ABI_VERSION + 1u;
    const vc_error wrong_error_snapshot = error;
    Check(vc_runtime_info(&runtime, &error) == VC_ERR_ABI,
          "wrong error ABI version must be rejected");
    Check(std::memcmp(&error, &wrong_error_snapshot, sizeof(error)) == 0,
          "unknown error ABI buffer must remain byte-for-byte unchanged");

    error = FreshError();
    runtime.abi_version = VC_ABI_VERSION + 1u;
    Check(vc_runtime_info(&runtime, &error) == VC_ERR_ABI,
          "wrong runtime info ABI version must be rejected");
    Check(error.code == VC_ERR_ABI, "wrong runtime ABI code");

    error = FreshError();
    runtime.abi_version = VC_ABI_VERSION;
    Check(vc_runtime_info(&runtime, &error) == VC_OK,
          "Task 4 runtime info must succeed");
    Check(error.code == VC_OK,
          "runtime info success error code");

    vc_media_open_options options = FreshOpenOptions();
    options.reserved_flags = 1u;
    vc_media_session* session = reinterpret_cast<vc_media_session*>(1);
    const uint16_t path[] = {'x'};
    error = FreshError();
    Check(vc_media_open_w(path, 1u, &options, nullptr, &session, &error) ==
              VC_ERR_INVALID_ARG,
          "nonzero open reserved flags must be rejected");
    Check(session == reinterpret_cast<vc_media_session*>(1),
          "failed media open must leave output session unchanged");
    Check(error.code == VC_ERR_INVALID_ARG,
          "reserved flags error code");
    Check(error.message_utf8[sizeof(error.message_utf8) - 1u] == '\0',
          "reserved flags message must be NUL terminated");

    options.reserved_flags = 0u;
    options.reserved_0 = 0u;
    session = reinterpret_cast<vc_media_session*>(1);
    error = FreshError();
    Check(vc_media_open_w(path, 1u, &options, nullptr, &session, &error) ==
              VC_ERR_IO,
          "Task 5 media open reports missing files as IO failures");
    Check(error.code == VC_ERR_IO,
          "missing media open populates IO error");
    Check(session == reinterpret_cast<vc_media_session*>(1),
          "failed media open must leave session unchanged");

    options.abi_version = VC_ABI_VERSION + 1u;
    session = reinterpret_cast<vc_media_session*>(1);
    error = FreshError();
    Check(vc_media_open_w(path, 1u, &options, nullptr, &session, &error) ==
              VC_ERR_ABI,
          "wrong open options ABI version must be rejected");
    Check(session == reinterpret_cast<vc_media_session*>(1),
          "ABI-rejected media open must leave session unchanged");
    options.abi_version = VC_ABI_VERSION;

    vc_cancel_token* token = nullptr;
    error = FreshError();
    Check(vc_cancel_create(&token, &error) == VC_OK,
          "Task 4 cancel create must succeed");
    Check(token != nullptr,
          "cancel create must return a token");
    vc_cancel_request(token);
    vc_cancel_free(token);
    vc_cancel_free(token);
    vc_cancel_request(token);

    std::array<uint8_t, VC_SHA512_SIZE> sha_payload{};
    sha_payload.fill(0x6du);
    auto guarded_sha = MakeGuarded(sha_payload);
    const auto sha_snapshot = guarded_sha.value;
    error = FreshError();
    Check(vc_media_hash(nullptr, guarded_sha.value.data(), &error) ==
              VC_ERR_INVALID_ARG,
          "media hash rejects null session");
    Check(error.code == VC_ERR_INVALID_ARG,
          "media hash invalid argument error code");
    CheckGuardedUnchanged(guarded_sha,
                          sha_snapshot,
                          "invalid media hash output");

    guarded_sha = MakeGuarded(sha_payload);
    error = FreshError();
    Check(vc_media_hash(reinterpret_cast<vc_media_session*>(1),
                        guarded_sha.value.data(),
                        &error) == VC_ERR_UNSUPPORTED,
          "Task 3 media hash is a safe failure shell");
    Check(error.code == VC_ERR_UNSUPPORTED,
          "media hash unsupported error code");
    CheckGuardedUnchanged(guarded_sha,
                          sha_snapshot,
                          "unsupported media hash output");

    vc_analysis_request request = FreshAnalysisRequest();
    request.reserved_flags = 1u;
    vc_analysis_result result = FreshAnalysisResult();
    error = FreshError();
    Check(vc_media_analyze(reinterpret_cast<vc_media_session*>(1),
                           &request,
                           &result,
                           &error) == VC_ERR_INVALID_ARG,
          "analysis rejects nonzero reserved flags");

    request.reserved_flags = 0u;
    request.abi_version = VC_ABI_VERSION + 1u;
    error = FreshError();
    Check(vc_media_analyze(reinterpret_cast<vc_media_session*>(1),
                           &request,
                           &result,
                           &error) == VC_ERR_ABI,
          "wrong analysis request ABI version must be rejected");

    request.abi_version = VC_ABI_VERSION;
    result.abi_version = VC_ABI_VERSION + 1u;
    error = FreshError();
    Check(vc_media_analyze(reinterpret_cast<vc_media_session*>(1),
                           &request,
                           &result,
                           &error) == VC_ERR_ABI,
          "wrong analysis result ABI version must be rejected");

    result.abi_version = VC_ABI_VERSION;
    error = FreshError();
    Check(vc_media_analyze(reinterpret_cast<vc_media_session*>(1),
                           &request,
                           &result,
                           &error) == VC_ERR_UNSUPPORTED,
          "Task 3 media analyze is a safe failure shell");
    Check(error.code == VC_ERR_UNSUPPORTED,
          "media analyze unsupported error code");

    vc_cancel_request(nullptr);
    vc_cancel_free(nullptr);
    vc_media_close(nullptr);
}

void TestDynamicBoundaryMatrix() {
    const uint16_t path_unit = static_cast<uint16_t>('x');
    struct vc_runtime_info valid_runtime{};
    valid_runtime.struct_size = sizeof(valid_runtime);
    valid_runtime.abi_version = VC_ABI_VERSION;

    struct ErrorHeaderCase {
        const char* name;
        uint32_t struct_size;
        uint32_t abi_version;
    };
    const ErrorHeaderCase error_header_cases[] = {
        {"error fixed header too small", 7u, VC_ABI_VERSION},
        {"error wrong ABI", sizeof(vc_error), VC_ABI_VERSION + 1u},
    };
    for (const auto& test_case : error_header_cases) {
        vc_error invalid_error = FreshError();
        invalid_error.struct_size = test_case.struct_size;
        invalid_error.abi_version = test_case.abi_version;
        auto guarded_error = MakeGuarded(invalid_error);
        const vc_error error_snapshot = guarded_error.value;
        auto guarded_runtime = MakeGuarded(valid_runtime);
        const RuntimeInfoLayout runtime_snapshot = guarded_runtime.value;

        const int32_t status =
            vc_runtime_info(&guarded_runtime.value, &guarded_error.value);
        CheckCase(status == VC_ERR_ABI,
                  test_case.name,
                  "must return VC_ERR_ABI");
        CheckGuardedUnchanged(guarded_error,
                              error_snapshot,
                              test_case.name);
        CheckGuardedUnchanged(guarded_runtime,
                              runtime_snapshot,
                              test_case.name);
    }

    {
        vc_error small_abi1_error = FreshError();
        small_abi1_error.struct_size = sizeof(vc_error) - 1u;
        auto guarded_error = MakeGuarded(small_abi1_error);
        auto guarded_runtime = MakeGuarded(valid_runtime);
        const RuntimeInfoLayout runtime_snapshot = guarded_runtime.value;
        const int32_t status =
            vc_runtime_info(&guarded_runtime.value, &guarded_error.value);
        CheckCase(status == VC_ERR_ABI,
                  "error sizeof-1",
                  "must return VC_ERR_ABI");
        CheckCase(guarded_error.value.code == VC_ERR_ABI,
                  "error sizeof-1",
                  "known ABI 1 may receive bounded error details");
        CheckCase(guarded_error.value.message_utf8[510] == '\0',
                  "error sizeof-1",
                  "last caller-declared message byte must be NUL");
        CheckGuardsIntact(guarded_error, "error sizeof-1");
        CheckGuardedUnchanged(guarded_runtime,
                              runtime_snapshot,
                              "error sizeof-1 runtime output");
    }

    struct RuntimeCase {
        const char* name;
        void (*mutate)(RuntimeInfoLayout&);
    };
    const RuntimeCase runtime_cases[] = {
        {"runtime sizeof-1",
         [](RuntimeInfoLayout& value) {
             value.struct_size = sizeof(RuntimeInfoLayout) - 1u;
         }},
        {"runtime wrong ABI",
         [](RuntimeInfoLayout& value) {
             value.abi_version = VC_ABI_VERSION + 1u;
         }},
    };
    for (const auto& test_case : runtime_cases) {
        auto guarded_runtime = MakeGuarded(valid_runtime);
        test_case.mutate(guarded_runtime.value);
        const RuntimeInfoLayout runtime_snapshot = guarded_runtime.value;
        auto guarded_error = MakeGuarded(FreshError());
        const int32_t status =
            vc_runtime_info(&guarded_runtime.value, &guarded_error.value);
        CheckCase(status == VC_ERR_ABI,
                  test_case.name,
                  "must return VC_ERR_ABI");
        CheckCase(guarded_error.value.code == VC_ERR_ABI,
                  test_case.name,
                  "must report VC_ERR_ABI");
        CheckGuardedUnchanged(guarded_runtime,
                              runtime_snapshot,
                              test_case.name);
        CheckGuardsIntact(guarded_error, test_case.name);
    }

    struct OpenCase {
        const char* name;
        void (*mutate)(vc_media_open_options&);
        int32_t expected_status;
    };
    const OpenCase open_cases[] = {
        {"open sizeof-1",
         [](vc_media_open_options& value) {
             value.struct_size = sizeof(vc_media_open_options) - 1u;
         },
         VC_ERR_ABI},
        {"open wrong ABI",
         [](vc_media_open_options& value) {
             value.abi_version = VC_ABI_VERSION + 1u;
         },
         VC_ERR_ABI},
        {"open reserved_flags",
         [](vc_media_open_options& value) { value.reserved_flags = 1u; },
         VC_ERR_INVALID_ARG},
        {"open reserved_0",
         [](vc_media_open_options& value) { value.reserved_0 = 1u; },
         VC_ERR_INVALID_ARG},
    };
    for (const auto& test_case : open_cases) {
        auto guarded_options = MakeGuarded(FreshOpenOptions());
        test_case.mutate(guarded_options.value);
        const vc_media_open_options options_snapshot = guarded_options.value;
        auto guarded_path = MakeGuarded(path_unit);
        const uint16_t path_snapshot = guarded_path.value;
        auto guarded_session =
            MakeGuarded(reinterpret_cast<vc_media_session*>(1));
        vc_media_session* const session_snapshot = guarded_session.value;
        auto guarded_error = MakeGuarded(FreshError());

        const int32_t status =
            vc_media_open_w(&guarded_path.value,
                            1u,
                            &guarded_options.value,
                            nullptr,
                            &guarded_session.value,
                            &guarded_error.value);
        CheckCase(status == test_case.expected_status,
                  test_case.name,
                  "wrong status");
        CheckCase(guarded_error.value.code == test_case.expected_status,
                  test_case.name,
                  "wrong error code");
        CheckGuardedUnchanged(guarded_options,
                              options_snapshot,
                              test_case.name);
        CheckGuardedUnchanged(guarded_path,
                              path_snapshot,
                              test_case.name);
        CheckGuardedUnchanged(guarded_session,
                              session_snapshot,
                              test_case.name);
        CheckGuardsIntact(guarded_error, test_case.name);
    }

    struct AnalysisCase {
        const char* name;
        void (*mutate)(vc_analysis_request&, vc_analysis_result&);
        int32_t expected_status;
    };
    const AnalysisCase analysis_cases[] = {
        {"request sizeof-1",
         [](vc_analysis_request& request, vc_analysis_result&) {
             request.struct_size = sizeof(vc_analysis_request) - 1u;
         },
         VC_ERR_ABI},
        {"request wrong ABI",
         [](vc_analysis_request& request, vc_analysis_result&) {
             request.abi_version = VC_ABI_VERSION + 1u;
         },
         VC_ERR_ABI},
        {"request reserved_flags",
         [](vc_analysis_request& request, vc_analysis_result&) {
             request.reserved_flags = 1u;
         },
         VC_ERR_INVALID_ARG},
        {"request reserved_0",
         [](vc_analysis_request& request, vc_analysis_result&) {
             request.reserved_0 = 1u;
         },
         VC_ERR_INVALID_ARG},
        {"request reserved_1",
         [](vc_analysis_request& request, vc_analysis_result&) {
             request.reserved_1 = 1u;
         },
         VC_ERR_INVALID_ARG},
        {"request frame_mask high bit",
         [](vc_analysis_request& request, vc_analysis_result&) {
             request.frame_mask = VC_ALL_FRAME_MASK | 0x40u;
         },
         VC_ERR_INVALID_ARG},
        {"result sizeof-1",
         [](vc_analysis_request&, vc_analysis_result& result) {
             result.struct_size = sizeof(vc_analysis_result) - 1u;
         },
         VC_ERR_ABI},
        {"result wrong ABI",
         [](vc_analysis_request&, vc_analysis_result& result) {
             result.abi_version = VC_ABI_VERSION + 1u;
         },
         VC_ERR_ABI},
        {"result reserved_flags",
         [](vc_analysis_request&, vc_analysis_result& result) {
             result.reserved_flags = 1u;
         },
         VC_ERR_INVALID_ARG},
        {"image features sizeof-1",
         [](vc_analysis_request&, vc_analysis_result& result) {
             result.image_features.struct_size =
                 sizeof(vc_feature_set) - 1u;
         },
         VC_ERR_ABI},
        {"image features wrong ABI",
         [](vc_analysis_request&, vc_analysis_result& result) {
             result.image_features.abi_version = VC_ABI_VERSION + 1u;
         },
         VC_ERR_ABI},
        {"image features reserved_0",
         [](vc_analysis_request&, vc_analysis_result& result) {
             result.image_features.reserved_0 = 1u;
         },
         VC_ERR_INVALID_ARG},
        {"contact features sizeof-1",
         [](vc_analysis_request&, vc_analysis_result& result) {
             result.contact_sheet_features.struct_size =
                 sizeof(vc_feature_set) - 1u;
         },
         VC_ERR_ABI},
        {"contact features wrong ABI",
         [](vc_analysis_request&, vc_analysis_result& result) {
             result.contact_sheet_features.abi_version =
                 VC_ABI_VERSION + 1u;
         },
         VC_ERR_ABI},
        {"contact features reserved_0",
         [](vc_analysis_request&, vc_analysis_result& result) {
             result.contact_sheet_features.reserved_0 = 1u;
         },
         VC_ERR_INVALID_ARG},
        {"frame sizeof-1",
         [](vc_analysis_request&, vc_analysis_result& result) {
             result.frames[0].struct_size =
                 sizeof(vc_video_frame_result) - 1u;
         },
         VC_ERR_ABI},
        {"frame wrong ABI",
         [](vc_analysis_request&, vc_analysis_result& result) {
             result.frames[0].abi_version = VC_ABI_VERSION + 1u;
         },
         VC_ERR_ABI},
        {"frame features sizeof-1",
         [](vc_analysis_request&, vc_analysis_result& result) {
             result.frames[0].features.struct_size =
                 sizeof(vc_feature_set) - 1u;
         },
         VC_ERR_ABI},
        {"frame features wrong ABI",
         [](vc_analysis_request&, vc_analysis_result& result) {
             result.frames[0].features.abi_version =
                 VC_ABI_VERSION + 1u;
         },
         VC_ERR_ABI},
        {"frame features reserved_0",
         [](vc_analysis_request&, vc_analysis_result& result) {
             result.frames[0].features.reserved_0 = 1u;
         },
         VC_ERR_INVALID_ARG},
        {"last frame wrong ABI",
         [](vc_analysis_request&, vc_analysis_result& result) {
             result.frames[VC_VIDEO_FRAME_COUNT - 1u].abi_version =
                 VC_ABI_VERSION + 1u;
         },
         VC_ERR_ABI},
        {"last frame features sizeof-1",
         [](vc_analysis_request&, vc_analysis_result& result) {
             result.frames[VC_VIDEO_FRAME_COUNT - 1u]
                 .features.struct_size = sizeof(vc_feature_set) - 1u;
         },
         VC_ERR_ABI},
        {"last frame features wrong ABI",
         [](vc_analysis_request&, vc_analysis_result& result) {
             result.frames[VC_VIDEO_FRAME_COUNT - 1u]
                 .features.abi_version = VC_ABI_VERSION + 1u;
         },
         VC_ERR_ABI},
        {"last frame features reserved_0",
         [](vc_analysis_request&, vc_analysis_result& result) {
             result.frames[VC_VIDEO_FRAME_COUNT - 1u]
                 .features.reserved_0 = 1u;
         },
         VC_ERR_INVALID_ARG},
    };
    for (const auto& test_case : analysis_cases) {
        auto guarded_request = MakeGuarded(FreshAnalysisRequest());
        auto guarded_result = MakeGuarded(FreshAnalysisResult());
        test_case.mutate(guarded_request.value, guarded_result.value);
        const vc_analysis_request request_snapshot = guarded_request.value;
        const vc_analysis_result result_snapshot = guarded_result.value;
        auto guarded_error = MakeGuarded(FreshError());

        const int32_t status =
            vc_media_analyze(reinterpret_cast<vc_media_session*>(1),
                             &guarded_request.value,
                             &guarded_result.value,
                             &guarded_error.value);
        CheckCase(status == test_case.expected_status,
                  test_case.name,
                  "wrong status");
        CheckCase(guarded_error.value.code == test_case.expected_status,
                  test_case.name,
                  "wrong error code");
        CheckGuardedUnchanged(guarded_request,
                              request_snapshot,
                              test_case.name);
        CheckGuardedUnchanged(guarded_result,
                              result_snapshot,
                              test_case.name);
        CheckGuardsIntact(guarded_error, test_case.name);
    }
}

#if VC_TESTING_ERROR_BOUNDARY
void TestExceptionBoundary() {
    vc_error error = FreshError();
    Check(vc::detail::Guard(
              &error,
              []() -> int32_t { throw std::bad_alloc(); }) == VC_ERR_OOM,
          "bad_alloc must map to VC_ERR_OOM");
    Check(error.code == VC_ERR_OOM, "bad_alloc error code");
    Check(error.message_utf8[sizeof(error.message_utf8) - 1u] == '\0',
          "bad_alloc message must be NUL terminated");

    error = FreshError();
    const std::string long_message(900u, 'z');
    Check(vc::detail::Guard(
              &error,
              [&long_message]() -> int32_t {
                  throw std::runtime_error(long_message);
              }) == VC_ERR_INTERNAL,
          "other exceptions must map to VC_ERR_INTERNAL");
    Check(error.code == VC_ERR_INTERNAL, "internal error code");
    Check(error.message_utf8[sizeof(error.message_utf8) - 1u] == '\0',
          "truncated exception message must be NUL terminated");
}
#endif

#ifdef _WIN32
void TestExactExports() {
    HMODULE module = GetModuleHandleW(L"videocore.dll");
    Check(module != nullptr, "videocore.dll must be loaded");
    if (module == nullptr) {
        return;
    }

    const auto* base = reinterpret_cast<const uint8_t*>(module);
    const auto* dos = reinterpret_cast<const IMAGE_DOS_HEADER*>(base);
    Check(dos->e_magic == IMAGE_DOS_SIGNATURE, "DLL DOS signature");
    if (dos->e_magic != IMAGE_DOS_SIGNATURE) {
        return;
    }
    const auto* nt = reinterpret_cast<const IMAGE_NT_HEADERS*>(
        base + static_cast<size_t>(dos->e_lfanew));
    Check(nt->Signature == IMAGE_NT_SIGNATURE, "DLL PE signature");
    if (nt->Signature != IMAGE_NT_SIGNATURE) {
        return;
    }
    const auto& directory =
        nt->OptionalHeader.DataDirectory[IMAGE_DIRECTORY_ENTRY_EXPORT];
    Check(directory.VirtualAddress != 0u, "DLL export directory");
    if (directory.VirtualAddress == 0u) {
        return;
    }
    const auto* exports = reinterpret_cast<const IMAGE_EXPORT_DIRECTORY*>(
        base + directory.VirtualAddress);
    Check(exports->NumberOfFunctions == 10u,
          "DLL export function table must contain exactly ten entries");
    Check(exports->NumberOfNames == 10u,
          "DLL export name table must contain exactly ten entries");
    const auto* name_rvas =
        reinterpret_cast<const uint32_t*>(base + exports->AddressOfNames);
    std::vector<std::string> actual;
    for (uint32_t index = 0; index < exports->NumberOfNames; ++index) {
        actual.emplace_back(reinterpret_cast<const char*>(
            base + name_rvas[index]));
    }
    std::sort(actual.begin(), actual.end());

    std::vector<std::string> expected{
        "vc_abi_version",
        "vc_cancel_create",
        "vc_cancel_free",
        "vc_cancel_request",
        "vc_media_analyze",
        "vc_media_close",
        "vc_media_hash",
        "vc_media_open_w",
        "vc_runtime_info",
        "vc_version",
    };
    Check(actual == expected, "DLL must export exactly ten vc_* names");

    std::cout << "ABI_EXPORTS";
    for (const auto& name : actual) {
        std::cout << ' ' << name;
    }
    std::cout << '\n';
}
#endif

}  // namespace

int main() {
    TestConstants();
    TestLayouts();
    TestCallingConventionAndVersions();
    TestSafeFailureShells();
    TestDynamicBoundaryMatrix();
#if VC_TESTING_ERROR_BOUNDARY
    TestExceptionBoundary();
#endif
#ifdef _WIN32
    TestExactExports();
#endif
    if (failures != 0) {
        std::cerr << failures << " ABI test(s) failed\n";
        return 1;
    }
    std::cout << "videocore ABI tests passed\n";
    return 0;
}
