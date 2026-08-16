#define WIN32_LEAN_AND_MEAN
#include <windows.h>

#include <algorithm>
#include <array>
#include <atomic>
#include <cstddef>
#include <cstdint>
#include <cstring>
#include <iostream>
#include <string>
#include <vector>

#include "media_session.h"
#include "videocore/videocore.h"

namespace vc::detail {

std::atomic<uint64_t> image_analysis_test_decode_runs{0u};
std::atomic<bool> image_analysis_test_fail_algorithm{false};

void ImageAnalysisTestRecordDecode() noexcept {
    image_analysis_test_decode_runs.fetch_add(1u, std::memory_order_relaxed);
}

bool ImageAnalysisTestConsumeAlgorithmFailure() noexcept {
    return image_analysis_test_fail_algorithm.exchange(
        false, std::memory_order_acq_rel);
}

uint64_t ImageAnalysisTestDecodeRuns() noexcept {
    return image_analysis_test_decode_runs.load(std::memory_order_relaxed);
}

void ImageAnalysisTestFailNextAlgorithm() noexcept {
    image_analysis_test_fail_algorithm.store(true, std::memory_order_release);
}

}  // namespace vc::detail

namespace {

int failures = 0;

void Check(bool condition, const char* message) {
    if (!condition) {
        std::cerr << "FAIL: " << message << '\n';
        ++failures;
    }
}

vc_error FreshError() {
    vc_error error{};
    error.struct_size = sizeof(error);
    error.abi_version = VC_ABI_VERSION;
    return error;
}

vc_media_open_options FreshImageOptions() {
    vc_media_open_options options{};
    options.struct_size = sizeof(options);
    options.abi_version = VC_ABI_VERSION;
    options.expected_media_type = VC_MEDIA_TYPE_IMAGE;
    options.image_max_bytes = 1024u * 1024u;
    return options;
}

vc_analysis_request FreshRequest(uint64_t feature_mask) {
    vc_analysis_request request{};
    request.struct_size = sizeof(request);
    request.abi_version = VC_ABI_VERSION;
    request.feature_mask = feature_mask;
    return request;
}

void InitializeFeatureSet(vc_feature_set* features) {
    features->struct_size = sizeof(*features);
    features->abi_version = VC_ABI_VERSION;
}

vc_analysis_result FreshResult() {
    vc_analysis_result result{};
    result.struct_size = sizeof(result);
    result.abi_version = VC_ABI_VERSION;
    InitializeFeatureSet(&result.image_features);
    InitializeFeatureSet(&result.contact_sheet_features);
    for (auto& frame : result.frames) {
        frame.struct_size = sizeof(frame);
        frame.abi_version = VC_ABI_VERSION;
        InitializeFeatureSet(&frame.features);
    }
    return result;
}

vc_analysis_result PoisonedResult() {
    vc_analysis_result result = FreshResult();
    result.media_type = VC_MEDIA_TYPE_VIDEO;
    result.image_status = 0x13572468;
    result.contact_sheet_width = 0x12345678u;
    result.contact_sheet_height = 0x87654321u;
    result.completed_frame_mask = VC_ALL_FRAME_MASK;
    std::memset(result.image_features.pdq,
                0xa5,
                sizeof(result.image_features.pdq));
    result.image_features.pdq_quality = 0xdecafbadU;
    std::fill(std::begin(result.image_features.phash),
              std::end(result.image_features.phash),
              0xdeadbeefdeadbeefULL);
    std::fill(std::begin(result.image_features.sobel_histogram),
              std::end(result.image_features.sobel_histogram),
              -1234.5f);
    return result;
}

bool ImagePayloadIsZero(const vc_feature_set& features) {
    return std::all_of(
               std::begin(features.pdq),
               std::end(features.pdq),
               [](uint8_t value) { return value == 0u; }) &&
           features.pdq_quality == 0u &&
           std::all_of(
               std::begin(features.phash),
               std::end(features.phash),
               [](uint64_t value) { return value == 0u; }) &&
           std::all_of(
               std::begin(features.sobel_histogram),
               std::end(features.sobel_histogram),
               [](float value) { return value == 0.0f; });
}

void CheckSafeImageFailure(const vc_analysis_result& result,
                           int32_t expected_status,
                           const char* message) {
    Check(result.media_type == VC_MEDIA_TYPE_IMAGE, message);
    Check(result.image_status == expected_status, message);
    Check(result.contact_sheet_width == 0u &&
              result.contact_sheet_height == 0u,
          message);
    Check(result.completed_frame_mask == 0u, message);
    Check(ImagePayloadIsZero(result.image_features), message);
}

std::wstring MakeTemporaryPath() {
    wchar_t directory[MAX_PATH]{};
    const DWORD count = GetTempPathW(MAX_PATH, directory);
    Check(count != 0u && count < MAX_PATH, "temporary directory exists");
    return std::wstring(directory) + L"videocore-image-analysis-" +
           std::to_wstring(GetCurrentProcessId()) + L".pgm";
}

bool WriteFixture(const std::wstring& path) {
    std::vector<uint8_t> bytes{'P', '5', '\n', '8', ' ', '8', '\n',
                               '2', '5', '5', '\n'};
    for (uint32_t y = 0u; y < 8u; ++y) {
        for (uint32_t x = 0u; x < 8u; ++x) {
            bytes.push_back(((x + y) & 1u) == 0u ? 0x11u : 0xeeu);
        }
    }
    HANDLE file = CreateFileW(path.c_str(), GENERIC_WRITE,
                              FILE_SHARE_READ | FILE_SHARE_WRITE |
                                  FILE_SHARE_DELETE,
                              nullptr, CREATE_ALWAYS,
                              FILE_ATTRIBUTE_NORMAL, nullptr);
    if (file == INVALID_HANDLE_VALUE) return false;
    DWORD written = 0u;
    const bool ok = WriteFile(file, bytes.data(),
                              static_cast<DWORD>(bytes.size()), &written,
                              nullptr) != FALSE &&
                    written == bytes.size();
    CloseHandle(file);
    return ok;
}

bool WriteInvalidFixture(const std::wstring& path) {
    const std::array<uint8_t, 3> bytes{'b', 'a', 'd'};
    HANDLE file = CreateFileW(path.c_str(), GENERIC_WRITE,
                              FILE_SHARE_READ | FILE_SHARE_WRITE |
                                  FILE_SHARE_DELETE,
                              nullptr, CREATE_ALWAYS,
                              FILE_ATTRIBUTE_NORMAL, nullptr);
    if (file == INVALID_HANDLE_VALUE) return false;
    DWORD written = 0u;
    const bool ok = WriteFile(file, bytes.data(),
                              static_cast<DWORD>(bytes.size()), &written,
                              nullptr) != FALSE &&
                    written == bytes.size();
    CloseHandle(file);
    return ok;
}

bool WriteTinyWebPFixture(const std::wstring& path) {
    static constexpr std::array<uint8_t, 46> bytes{
        0x52, 0x49, 0x46, 0x46, 0x22, 0x00, 0x00, 0x00,
        0x57, 0x45, 0x42, 0x50, 0x56, 0x50, 0x38, 0x20,
        0x16, 0x00, 0x00, 0x00, 0x30, 0x01, 0x00, 0x9d,
        0x01, 0x2a, 0x01, 0x00, 0x01, 0x00, 0x01, 0x40,
        0x26, 0x25, 0xa4, 0x00, 0x03, 0x70, 0x00, 0xfe,
        0xff, 0x3d, 0x58, 0x00, 0x00, 0x00,
    };
    HANDLE file = CreateFileW(path.c_str(), GENERIC_WRITE,
                              FILE_SHARE_READ | FILE_SHARE_WRITE |
                                  FILE_SHARE_DELETE,
                              nullptr, CREATE_ALWAYS,
                              FILE_ATTRIBUTE_NORMAL, nullptr);
    if (file == INVALID_HANDLE_VALUE) return false;
    DWORD written = 0u;
    const bool ok = WriteFile(file, bytes.data(),
                              static_cast<DWORD>(bytes.size()), &written,
                              nullptr) != FALSE &&
                    written == bytes.size();
    CloseHandle(file);
    return ok;
}

int32_t Open(const std::wstring& path, vc_media_session** session,
             vc_error* error) {
    const vc_media_open_options options = FreshImageOptions();
    return vc_media_open_w(reinterpret_cast<const uint16_t*>(path.data()),
                           static_cast<uint32_t>(path.size()), &options,
                           nullptr, session, error);
}

void TestImageAnalysisUsesOneCachedDecodeAndHonorsMasks() {
    const std::wstring path = MakeTemporaryPath();
    Check(WriteFixture(path), "image fixture write");

    vc_media_session* session = nullptr;
    vc_error error = FreshError();
    const int32_t open_status = Open(path, &session, &error);
    Check(open_status == VC_OK, "image session opens");
    if (open_status != VC_OK) {
        DeleteFileW(path.c_str());
        return;
    }

    vc_analysis_request request = FreshRequest(
        VC_FEATURE_PDQ | VC_FEATURE_PHASH | VC_FEATURE_SOBEL);
    vc_analysis_result before_hash = PoisonedResult();
    error = FreshError();
    Check(vc_media_analyze(session, &request, &before_hash, &error) ==
              VC_ERR_INVALID_ARG,
          "image analysis rejects a session whose hash has not completed");
    CheckSafeImageFailure(before_hash,
                          VC_ERR_INVALID_ARG,
                          "hash prerequisite failure publishes safe image state");

    std::array<uint8_t, VC_SHA512_SIZE> digest{};
    error = FreshError();
    Check(vc_media_hash(session, digest.data(), &error) == VC_OK,
          "image hash populates bounded bytes");
    vc::detail::MediaSessionTestSnapshot hashed{};
    Check(vc::detail::GetMediaSessionTestSnapshot(session, &hashed),
          "snapshot after hash is available");

    vc_analysis_result full = FreshResult();
    const uint64_t full_decode_before =
        vc::detail::ImageAnalysisTestDecodeRuns();
    error = FreshError();
    Check(vc_media_analyze(session, &request, &full, &error) == VC_OK,
          "image analysis fulfills all requested image features");
    Check(full.media_type == VC_MEDIA_TYPE_IMAGE,
          "image analysis reports image media type");
    Check(full.image_status == VC_OK,
          "image analysis marks fulfilled image item successful");
    Check(full.contact_sheet_width == 8u &&
              full.contact_sheet_height == 8u,
          "image analysis returns decoded image dimensions");
    Check(full.completed_frame_mask == 0u,
          "image analysis never reports video frame completion");
    const uint64_t full_decode_after =
        vc::detail::ImageAnalysisTestDecodeRuns();
    vc::detail::MediaSessionTestSnapshot analyzed{};
    Check(vc::detail::GetMediaSessionTestSnapshot(session, &analyzed),
          "snapshot after analysis is available");
    Check(analyzed.io.read_calls == hashed.io.read_calls,
          "image analysis does not read the file after hashing");
    Check(full_decode_after - full_decode_before == 1u,
          "all requested features share exactly one image decode");
    std::cout << "IMAGE_ANALYSIS full_decode="
              << (full_decode_after - full_decode_before)
              << " hash_reads=" << hashed.io.read_calls
              << " analyze_reads=" << analyzed.io.read_calls
              << " fulfilled=0x" << std::hex << full.completed_frame_mask
              << std::dec << '\n';

    vc_analysis_request repeated_request = FreshRequest(VC_FEATURE_PDQ);
    vc_analysis_result repeated = FreshResult();
    const uint64_t repeated_before =
        vc::detail::ImageAnalysisTestDecodeRuns();
    error = FreshError();
    Check(vc_media_analyze(session,
                           &repeated_request,
                           &repeated,
                           &error) == VC_OK,
          "repeated image analysis succeeds");
    const uint64_t repeated_after =
        vc::detail::ImageAnalysisTestDecodeRuns();
    Check(repeated_after - repeated_before == 1u,
          "each repeated analysis performs exactly one real decode");

    vc_media_close(session);
    DeleteFileW(path.c_str());
}

void TestImageAnalysisLeavesUnrequestedFeaturesZero() {
    const std::wstring path = MakeTemporaryPath();
    Check(WriteFixture(path), "partial image fixture write");
    vc_media_session* session = nullptr;
    vc_error error = FreshError();
    const int32_t open_status = Open(path, &session, &error);
    Check(open_status == VC_OK, "partial image session opens");
    if (open_status == VC_OK) {
        std::array<uint8_t, VC_SHA512_SIZE> digest{};
        error = FreshError();
        Check(vc_media_hash(session, digest.data(), &error) == VC_OK,
              "partial image hash succeeds");
        vc_analysis_request request = FreshRequest(VC_FEATURE_PDQ);
        vc_analysis_result result = PoisonedResult();
        const uint64_t decode_before =
            vc::detail::ImageAnalysisTestDecodeRuns();
        error = FreshError();
        Check(vc_media_analyze(session, &request, &result, &error) == VC_OK,
              "partial image analysis succeeds");
        Check(result.completed_frame_mask == 0u,
              "partial image analysis never reports video frames");
        Check(std::all_of(std::begin(result.image_features.phash),
                          std::end(result.image_features.phash),
                          [](uint64_t value) { return value == 0u; }) &&
                  std::all_of(std::begin(result.image_features.sobel_histogram),
                              std::end(result.image_features.sobel_histogram),
                              [](float value) { return value == 0.0f; }),
              "unrequested image feature payloads remain zero");
        const uint64_t decode_after =
            vc::detail::ImageAnalysisTestDecodeRuns();
        Check(decode_after - decode_before == 1u,
              "partial request still decodes once");
        std::cout << "IMAGE_ANALYSIS partial_decode="
                  << (decode_after - decode_before) << " fulfilled=0x"
                  << std::hex << result.completed_frame_mask << std::dec
                  << '\n';
        vc_media_close(session);
    }
    DeleteFileW(path.c_str());
}

void TestTinyWebPUsesDecodedPixelForFeatures() {
    const std::wstring path = MakeTemporaryPath() + L".webp";
    Check(WriteTinyWebPFixture(path), "tiny WebP fixture write");
    vc_media_session* session = nullptr;
    vc_error error = FreshError();
    const int32_t open_status = Open(path, &session, &error);
    Check(open_status == VC_OK, "tiny WebP session opens");
    if (open_status == VC_OK) {
        std::array<uint8_t, VC_SHA512_SIZE> digest{};
        error = FreshError();
        Check(vc_media_hash(session, digest.data(), &error) == VC_OK,
              "tiny WebP hashes its exact bytes");
        vc_analysis_request request = FreshRequest(VC_FEATURE_PDQ);
        vc_analysis_result result = FreshResult();
        error = FreshError();
        Check(vc_media_analyze(session, &request, &result, &error) == VC_OK,
              "tiny WebP computes PDQ from its decoded pixel");
        Check(result.image_status == VC_OK &&
                  result.contact_sheet_width == 1u &&
                  result.contact_sheet_height == 1u,
              "tiny WebP preserves its decoded dimensions");
        vc_media_close(session);
    }
    DeleteFileW(path.c_str());
}

void TestInvalidAndEmptyMasksDoNotDecodeAndPublishSafeFailure() {
    const std::wstring path = MakeTemporaryPath();
    Check(WriteFixture(path), "mask failure fixture write");
    vc_media_session* session = nullptr;
    vc_error error = FreshError();
    const int32_t open_status = Open(path, &session, &error);
    Check(open_status == VC_OK, "mask failure session opens");
    if (open_status == VC_OK) {
        std::array<uint8_t, VC_SHA512_SIZE> digest{};
        error = FreshError();
        Check(vc_media_hash(session, digest.data(), &error) == VC_OK,
              "mask failure fixture hash succeeds");
        const uint64_t masks[] = {0u, 1ull << 63u};
        for (const uint64_t mask : masks) {
            vc_analysis_request request = FreshRequest(mask);
            vc_analysis_result result = PoisonedResult();
            const uint64_t before =
                vc::detail::ImageAnalysisTestDecodeRuns();
            error = FreshError();
            Check(vc_media_analyze(session, &request, &result, &error) ==
                      VC_ERR_UNSUPPORTED,
                  "invalid image feature mask is rejected");
            const uint64_t after =
                vc::detail::ImageAnalysisTestDecodeRuns();
            Check(after - before == 0u,
                  "invalid image feature mask does not decode");
            CheckSafeImageFailure(
                result,
                VC_ERR_UNSUPPORTED,
                "invalid feature mask publishes safe image state");
        }
        vc_media_close(session);
    }
    DeleteFileW(path.c_str());
}

void TestInvalidRequestSemanticsDoNotDecodeAndPublishSafeFailure() {
    const std::wstring path = MakeTemporaryPath();
    Check(WriteFixture(path), "request semantics fixture write");
    vc_media_session* session = nullptr;
    vc_error error = FreshError();
    const int32_t open_status = Open(path, &session, &error);
    Check(open_status == VC_OK, "request semantics session opens");
    if (open_status == VC_OK) {
        std::array<uint8_t, VC_SHA512_SIZE> digest{};
        error = FreshError();
        Check(vc_media_hash(session, digest.data(), &error) == VC_OK,
              "request semantics fixture hash succeeds");

        std::array<vc_analysis_request, 4> invalid_requests{
            FreshRequest(VC_FEATURE_PDQ),
            FreshRequest(VC_FEATURE_PDQ),
            FreshRequest(VC_FEATURE_PDQ),
            FreshRequest(VC_FEATURE_PDQ),
        };
        invalid_requests[0].reserved_flags = 1u;
        invalid_requests[1].reserved_0 = 1u;
        invalid_requests[2].reserved_1 = 1u;
        invalid_requests[3].frame_mask = VC_ALL_FRAME_MASK | 0x40u;
        const char* case_names[] = {
            "reserved_flags failure publishes safe image state",
            "reserved_0 failure publishes safe image state",
            "reserved_1 failure publishes safe image state",
            "invalid frame_mask failure publishes safe image state",
        };

        for (size_t index = 0u; index < invalid_requests.size(); ++index) {
            vc_analysis_result result = PoisonedResult();
            const uint64_t before =
                vc::detail::ImageAnalysisTestDecodeRuns();
            error = FreshError();
            Check(vc_media_analyze(session,
                                   &invalid_requests[index],
                                   &result,
                                   &error) == VC_ERR_INVALID_ARG,
                  "invalid request semantic field is rejected");
            const uint64_t after =
                vc::detail::ImageAnalysisTestDecodeRuns();
            Check(after - before == 0u,
                  "invalid request semantic field does not decode");
            CheckSafeImageFailure(result,
                                  VC_ERR_INVALID_ARG,
                                  case_names[index]);
        }

        static_assert(offsetof(vc_analysis_request, temporary_jpeg_path) ==
                      offsetof(vc_analysis_result, image_features));
        static_assert(sizeof(uintptr_t) == sizeof(uint64_t));
        vc_analysis_result aliased = PoisonedResult();
        vc_analysis_request alias_request = FreshRequest(VC_FEATURE_PDQ);
        alias_request.struct_size = sizeof(vc_analysis_result);
        alias_request.reserved_flags = 1u;
        const uintptr_t image_header =
            static_cast<uintptr_t>(sizeof(vc_feature_set)) |
            (static_cast<uintptr_t>(VC_ABI_VERSION) << 32u);
        alias_request.temporary_jpeg_path =
            reinterpret_cast<const uint16_t*>(image_header);
        std::memcpy(&aliased, &alias_request, sizeof(alias_request));
        auto* request_alias =
            reinterpret_cast<vc_analysis_request*>(&aliased);
        const uint64_t alias_before =
            vc::detail::ImageAnalysisTestDecodeRuns();
        error = FreshError();
        Check(vc_media_analyze(session,
                               request_alias,
                               &aliased,
                               &error) == VC_ERR_INVALID_ARG,
              "aliased invalid reserved_flags is rejected");
        const uint64_t alias_after =
            vc::detail::ImageAnalysisTestDecodeRuns();
        Check(alias_after - alias_before == 0u,
              "aliased invalid reserved_flags does not decode");
        CheckSafeImageFailure(
            aliased,
            VC_ERR_INVALID_ARG,
            "aliased reserved_flags failure publishes safe image state");

        vc_media_close(session);
    }
    DeleteFileW(path.c_str());
}

void TestNonImageRequestSemanticFailureLeavesResultUnchanged() {
    const std::wstring path = MakeTemporaryPath();
    Check(WriteFixture(path), "non-image semantics fixture write");
    vc_media_open_options options = FreshImageOptions();
    options.expected_media_type = VC_MEDIA_TYPE_VIDEO;
    vc_media_session* session = nullptr;
    vc_error error = FreshError();
    const int32_t open_status = vc_media_open_w(
        reinterpret_cast<const uint16_t*>(path.data()),
        static_cast<uint32_t>(path.size()),
        &options,
        nullptr,
        &session,
        &error);
    Check(open_status == VC_OK, "non-image semantics session opens");
    if (open_status == VC_OK) {
        vc_analysis_request request = FreshRequest(VC_FEATURE_PDQ);
        request.reserved_flags = 1u;
        vc_analysis_result result = PoisonedResult();
        const vc_analysis_result snapshot = result;
        const uint64_t before = vc::detail::ImageAnalysisTestDecodeRuns();
        error = FreshError();
        Check(vc_media_analyze(session, &request, &result, &error) ==
                  VC_ERR_INVALID_ARG,
              "non-image invalid request semantic field is rejected");
        const uint64_t after = vc::detail::ImageAnalysisTestDecodeRuns();
        Check(after - before == 0u,
              "non-image invalid request semantic field does not decode");
        Check(std::memcmp(&result, &snapshot, sizeof(result)) == 0,
              "non-image request semantic failure leaves result unchanged");
        vc_media_close(session);
    }
    DeleteFileW(path.c_str());
}

void TestDecodeAndAlgorithmFailuresAreAtomic() {
    const std::wstring invalid_path = MakeTemporaryPath();
    Check(WriteInvalidFixture(invalid_path), "decode failure fixture write");
    vc_media_session* invalid_session = nullptr;
    vc_error error = FreshError();
    const int32_t invalid_open = Open(invalid_path, &invalid_session, &error);
    Check(invalid_open == VC_OK, "decode failure session opens");
    if (invalid_open == VC_OK) {
        std::array<uint8_t, VC_SHA512_SIZE> digest{};
        error = FreshError();
        Check(vc_media_hash(invalid_session, digest.data(), &error) == VC_OK,
              "decode failure fixture hash succeeds");
        vc_analysis_request request = FreshRequest(VC_FEATURE_PDQ);
        vc_analysis_result result = PoisonedResult();
        const uint64_t before = vc::detail::ImageAnalysisTestDecodeRuns();
        error = FreshError();
        Check(vc_media_analyze(invalid_session, &request, &result, &error) ==
                  VC_ERR_DECODE,
              "malformed image returns decode failure");
        const uint64_t after = vc::detail::ImageAnalysisTestDecodeRuns();
        Check(after - before == 1u,
              "decode failure counts the real decode attempt");
        CheckSafeImageFailure(result,
                              VC_ERR_DECODE,
                              "decode failure publishes no partial payload");
        vc_media_close(invalid_session);
    }
    DeleteFileW(invalid_path.c_str());

    const std::wstring valid_path = MakeTemporaryPath();
    Check(WriteFixture(valid_path), "algorithm failure fixture write");
    vc_media_session* valid_session = nullptr;
    error = FreshError();
    const int32_t valid_open = Open(valid_path, &valid_session, &error);
    Check(valid_open == VC_OK, "algorithm failure session opens");
    if (valid_open == VC_OK) {
        std::array<uint8_t, VC_SHA512_SIZE> digest{};
        error = FreshError();
        Check(vc_media_hash(valid_session, digest.data(), &error) == VC_OK,
              "algorithm failure fixture hash succeeds");
        vc_analysis_request request = FreshRequest(
            VC_FEATURE_PDQ | VC_FEATURE_PHASH | VC_FEATURE_SOBEL);
        vc_analysis_result result = PoisonedResult();
        vc::detail::ImageAnalysisTestFailNextAlgorithm();
        const uint64_t before = vc::detail::ImageAnalysisTestDecodeRuns();
        error = FreshError();
        Check(vc_media_analyze(valid_session, &request, &result, &error) ==
                  VC_ERR_INTERNAL,
              "injected algorithm failure is returned");
        const uint64_t after = vc::detail::ImageAnalysisTestDecodeRuns();
        Check(after - before == 1u,
              "algorithm failure occurs after exactly one real decode");
        CheckSafeImageFailure(
            result,
            VC_ERR_INTERNAL,
            "algorithm failure publishes no partial payload");
        vc_media_close(valid_session);
    }
    DeleteFileW(valid_path.c_str());
}

void TestOutputSizeValidationAndRequestOutputAliasing() {
    const std::wstring path = MakeTemporaryPath();
    Check(WriteFixture(path), "alias fixture write");
    vc_media_session* session = nullptr;
    vc_error error = FreshError();
    const int32_t open_status = Open(path, &session, &error);
    Check(open_status == VC_OK, "alias session opens");
    if (open_status == VC_OK) {
        vc_analysis_request request = FreshRequest(VC_FEATURE_PDQ);

        vc_analysis_request short_request = request;
        short_request.struct_size = sizeof(short_request) - 1u;
        vc_analysis_result short_request_result = PoisonedResult();
        const vc_analysis_result short_request_snapshot =
            short_request_result;
        error = FreshError();
        Check(vc_media_analyze(session,
                               &short_request,
                               &short_request_result,
                               &error) == VC_ERR_ABI,
              "short request is rejected");
        Check(std::memcmp(&short_request_result,
                          &short_request_snapshot,
                          sizeof(short_request_result)) == 0,
              "short request does not write the result");

        vc_analysis_request bad_request_abi = request;
        bad_request_abi.abi_version = VC_ABI_VERSION + 1u;
        vc_analysis_result bad_request_abi_result = PoisonedResult();
        const vc_analysis_result bad_request_abi_snapshot =
            bad_request_abi_result;
        error = FreshError();
        Check(vc_media_analyze(session,
                               &bad_request_abi,
                               &bad_request_abi_result,
                               &error) == VC_ERR_ABI,
              "request with unknown ABI is rejected");
        Check(std::memcmp(&bad_request_abi_result,
                          &bad_request_abi_snapshot,
                          sizeof(bad_request_abi_result)) == 0,
              "request ABI failure does not write the result");

        vc_analysis_result short_result = PoisonedResult();
        short_result.struct_size = sizeof(short_result) - 1u;
        const vc_analysis_result short_snapshot = short_result;
        error = FreshError();
        Check(vc_media_analyze(session, &request, &short_result, &error) ==
                  VC_ERR_ABI,
              "short top-level result is rejected");
        Check(std::memcmp(&short_result,
                          &short_snapshot,
                          sizeof(short_result)) == 0,
              "short top-level result is not written");

        vc_analysis_result bad_result_abi = PoisonedResult();
        bad_result_abi.abi_version = VC_ABI_VERSION + 1u;
        const vc_analysis_result bad_result_abi_snapshot = bad_result_abi;
        error = FreshError();
        Check(vc_media_analyze(session,
                               &request,
                               &bad_result_abi,
                               &error) == VC_ERR_ABI,
              "result with unknown ABI is rejected");
        Check(std::memcmp(&bad_result_abi,
                          &bad_result_abi_snapshot,
                          sizeof(bad_result_abi)) == 0,
              "result ABI failure is not written");

        vc_analysis_result short_nested = PoisonedResult();
        short_nested.image_features.struct_size =
            sizeof(short_nested.image_features) - 1u;
        const vc_analysis_result nested_snapshot = short_nested;
        error = FreshError();
        Check(vc_media_analyze(session, &request, &short_nested, &error) ==
                  VC_ERR_ABI,
              "short nested image result is rejected");
        Check(std::memcmp(&short_nested,
                          &nested_snapshot,
                          sizeof(short_nested)) == 0,
              "short nested image result is not written");

        std::array<uint8_t, VC_SHA512_SIZE> digest{};
        error = FreshError();
        Check(vc_media_hash(session, digest.data(), &error) == VC_OK,
              "alias fixture hash succeeds");

        static_assert(offsetof(vc_analysis_request, temporary_jpeg_path) ==
                      offsetof(vc_analysis_result, image_features));
        static_assert(sizeof(uintptr_t) == sizeof(uint64_t));
        vc_analysis_result aliased = PoisonedResult();
        vc_analysis_request alias_request = FreshRequest(VC_FEATURE_PDQ);
        alias_request.struct_size = sizeof(vc_analysis_result);
        const uintptr_t image_header =
            static_cast<uintptr_t>(sizeof(vc_feature_set)) |
            (static_cast<uintptr_t>(VC_ABI_VERSION) << 32u);
        alias_request.temporary_jpeg_path =
            reinterpret_cast<const uint16_t*>(image_header);
        std::memcpy(&aliased, &alias_request, sizeof(alias_request));
        auto* request_alias =
            reinterpret_cast<vc_analysis_request*>(&aliased);
        const uint64_t before = vc::detail::ImageAnalysisTestDecodeRuns();
        error = FreshError();
        Check(vc_media_analyze(session,
                               request_alias,
                               &aliased,
                               &error) == VC_OK,
              "aliased request and output succeed");
        const uint64_t after = vc::detail::ImageAnalysisTestDecodeRuns();
        Check(after - before == 1u,
              "aliased request performs exactly one decode");
        Check(aliased.media_type == VC_MEDIA_TYPE_IMAGE &&
                  aliased.image_status == VC_OK &&
                  aliased.completed_frame_mask == 0u,
              "aliased request publishes the image success state");
        vc_media_close(session);
    }
    DeleteFileW(path.c_str());
}

}  // namespace

int main() {
    TestImageAnalysisUsesOneCachedDecodeAndHonorsMasks();
    TestImageAnalysisLeavesUnrequestedFeaturesZero();
    TestTinyWebPUsesDecodedPixelForFeatures();
    TestInvalidAndEmptyMasksDoNotDecodeAndPublishSafeFailure();
    TestInvalidRequestSemanticsDoNotDecodeAndPublishSafeFailure();
    TestNonImageRequestSemanticFailureLeavesResultUnchanged();
    TestDecodeAndAlgorithmFailuresAreAtomic();
    TestOutputSizeValidationAndRequestOutputAliasing();
    if (failures != 0) {
        std::cerr << failures << " image analysis test(s) failed\n";
        return 1;
    }
    std::cout << "videocore image analysis tests passed\n";
    return 0;
}
