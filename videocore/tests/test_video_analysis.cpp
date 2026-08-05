#define WIN32_LEAN_AND_MEAN
#include <windows.h>

#include <algorithm>
#include <array>
#include <cstdint>
#include <cstring>
#include <iostream>
#include <fstream>
#include <filesystem>
#include <iomanip>
#include <limits>
#include <memory>
#include <sstream>
#include <string>

extern "C" {
#include <libavformat/avformat.h>
#include <libavutil/frame.h>
#include <libavutil/pixfmt.h>
#include <libswscale/swscale.h>
}

#include "media_session.h"
#include "native_algorithms/gray_image.h"
#include "native_algorithms/pdq.h"
#include "native_algorithms/phash_parts.h"
#include "native_algorithms/sobel_hist.h"
#include "native_algorithms/sha512.h"
#include "video_analysis.h"
#include "videocore/videocore.h"

namespace {

int failures = 0;

void Check(bool condition, const std::string& message) {
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

vc_analysis_request FreshRequest(uint32_t frame_mask = 0u) {
    vc_analysis_request request{};
    request.struct_size = sizeof(request);
    request.abi_version = VC_ABI_VERSION;
    request.feature_mask = VC_FEATURE_DURATION | VC_FEATURE_PDQ |
                           VC_FEATURE_PHASH | VC_FEATURE_SOBEL;
    request.frame_mask = frame_mask;
    request.probe_timeout_ms = 15000u;
    request.frame_timeout_ms = 20000u;
    return request;
}

std::wstring FixturePath(const wchar_t* name) {
    return std::wstring(VC_VIDEO_TESTDATA_ROOT) + L"\\" + name;
}

bool AnyFeature(const vc_feature_set& features) {
    return std::any_of(std::begin(features.pdq), std::end(features.pdq),
                       [](uint8_t value) { return value != 0u; }) &&
           std::any_of(std::begin(features.phash), std::end(features.phash),
                       [](uint64_t value) { return value != 0u; }) &&
           std::any_of(std::begin(features.sobel_histogram),
                       std::end(features.sobel_histogram),
                       [](float value) { return value != 0.0f; });
}

bool FeatureSetIsZero(const vc_feature_set& features) {
    return std::all_of(std::begin(features.pdq), std::end(features.pdq),
                       [](uint8_t value) { return value == 0u; }) &&
           features.pdq_quality == 0u &&
           std::all_of(std::begin(features.phash), std::end(features.phash),
                       [](uint64_t value) { return value == 0u; }) &&
           std::all_of(std::begin(features.sobel_histogram),
                       std::end(features.sobel_histogram),
                       [](float value) { return value == 0.0f; });
}

bool HasPdq(const vc_feature_set& features) {
    return std::any_of(std::begin(features.pdq), std::end(features.pdq),
                       [](uint8_t value) { return value != 0u; }) &&
           features.pdq_quality > 0u;
}

bool HasPHash(const vc_feature_set& features) {
    return std::any_of(std::begin(features.phash), std::end(features.phash),
                       [](uint64_t value) { return value != 0u; });
}

bool HasSobel(const vc_feature_set& features) {
    return std::any_of(std::begin(features.sobel_histogram),
                       std::end(features.sobel_histogram),
                       [](float value) { return value != 0.0f; });
}

std::string HexBytes(const uint8_t* bytes, size_t size) {
    std::ostringstream output;
    output << std::hex << std::setfill('0');
    for (size_t index = 0; index < size; ++index) {
        output << std::setw(2) << static_cast<unsigned>(bytes[index]);
    }
    return output.str();
}

std::string PHashHex(const vc_feature_set& features) {
    std::ostringstream output;
    output << std::hex << std::setfill('0');
    for (size_t index = 0; index < VC_PHASH_COUNT; ++index) {
        if (index != 0u) output << ',';
        output << std::setw(16) << features.phash[index];
    }
    return output.str();
}

std::string SobelBitsHex(const vc_feature_set& features) {
    std::ostringstream output;
    output << std::hex << std::setfill('0');
    for (size_t index = 0; index < VC_SOBEL_HISTOGRAM_SIZE; ++index) {
        uint32_t bits = 0u;
        std::memcpy(&bits, &features.sobel_histogram[index], sizeof(bits));
        if (index != 0u) output << ',';
        output << std::setw(8) << bits;
    }
    return output.str();
}

std::string NarrowFixtureName(const wchar_t* name) {
    std::string result;
    while (*name != L'\0') {
        result.push_back(static_cast<char>(*name));
        ++name;
    }
    return result;
}

struct FixtureExpectation {
    const wchar_t* name;
    int64_t duration_ms;
    std::array<int64_t, VC_VIDEO_FRAME_COUNT> sample_times;
    int32_t top_status;
    uint32_t completed_mask;
    int32_t display_width;
    int32_t display_height;
};

const FixtureExpectation fixtures[] = {
    {L"h264-standard.mp4", 2417, {201, 604, 1007, 1409, 1812, 2215}, VC_OK, 0x3f, 512, 288},
    {L"h264-bframes.mp4", 2417, {201, 604, 1007, 1409, 1812, 2215}, VC_OK, 0x3f, 512, 279},
    {L"h264-rotate90.mp4", 1800, {150, 450, 750, 1050, 1350, 1650}, VC_OK, 0x3f, 288, 512},
    {L"h264-sar-4x3.mp4", 1800, {150, 450, 750, 1050, 1350, 1650}, VC_OK, 0x3f, 512, 256},
    {L"h264-short.mp4", 417, {34, 104, 173, 243, 312, 382}, VC_OK, 0x1f, 512, 288},
    {L"truncated-container.mp4", 2417, {201, 604, 1007, 1409, 1812, 2215}, VC_OK, 0x0f, 512, 288},
    {L"corrupt-packet.ts", 2000, {166, 500, 833, 1166, 1500, 1833}, VC_OK, 0x3f, 512, 288},
    {L"audio-only.m4a", 1400, {116, 350, 583, 816, 1050, 1283}, VC_ERR_NO_FRAME, 0x00, 0, 0},
};

template <typename Value>
void HashValue(vc::native::Sha512* hash, const Value& value) {
    hash->Update(reinterpret_cast<const uint8_t*>(&value), sizeof(value));
}

std::string AnalysisDigest(
    const vc_analysis_result& result,
    const vc::detail::VideoAnalysisTestStats& stats) {
    vc::native::Sha512 hash;
    HashValue(&hash, result.duration_ms);
    HashValue(&hash, result.duration_status);
    HashValue(&hash, result.completed_frame_mask);
    for (uint32_t index = 0u; index < VC_VIDEO_FRAME_COUNT; ++index) {
        const auto& frame = result.frames[index];
        HashValue(&hash, frame.standard_index);
        HashValue(&hash, frame.status);
        HashValue(&hash, frame.sample_time_ms);
        hash.Update(frame.features.pdq, sizeof(frame.features.pdq));
        HashValue(&hash, frame.features.pdq_quality);
        hash.Update(reinterpret_cast<const uint8_t*>(frame.features.phash),
                    sizeof(frame.features.phash));
        hash.Update(
            reinterpret_cast<const uint8_t*>(frame.features.sobel_histogram),
            sizeof(frame.features.sobel_histogram));
        HashValue(&hash, stats.display_widths[index]);
        HashValue(&hash, stats.display_heights[index]);
    }
    const auto digest = hash.Final();
    std::ostringstream text;
    text << std::hex << std::setfill('0');
    for (uint8_t byte : digest) text << std::setw(2) << unsigned(byte);
    return text.str();
}

void TestFixture(const FixtureExpectation& expected) {
    const std::wstring path = FixturePath(expected.name);
    vc_media_open_options options{};
    options.struct_size = sizeof(options);
    options.abi_version = VC_ABI_VERSION;
    options.expected_media_type = VC_MEDIA_TYPE_VIDEO;

    vc_media_session* session = nullptr;
    vc_error error = FreshError();
    const int32_t open_status = vc_media_open_w(
        reinterpret_cast<const uint16_t*>(path.data()),
        static_cast<uint32_t>(path.size()), &options, nullptr, &session, &error);
    Check(open_status == VC_OK, "fixture opens");
    if (open_status != VC_OK) return;

    std::array<uint8_t, VC_SHA512_SIZE> digest{};
    error = FreshError();
    Check(vc_media_hash(session, digest.data(), &error) == VC_OK,
          "fixture hashes before analyze");

    vc::detail::MediaSessionTestSnapshot before{};
    Check(vc::detail::GetMediaSessionTestSnapshot(session, &before),
          "session snapshot before video analyze");
    vc_analysis_request request = FreshRequest();
    vc_analysis_result result = FreshResult();
    error = FreshError();
    vc::detail::VideoAnalysisTestReset();
    const int32_t status = vc_media_analyze(session, &request, &result, &error);
    const vc::detail::VideoAnalysisTestStats video_stats =
        vc::detail::VideoAnalysisTestGetStats();
    const std::string analysis_digest = AnalysisDigest(result, video_stats);
    Check(status == expected.top_status, "top-level fixture status");
    Check(result.media_type == VC_MEDIA_TYPE_VIDEO, "video media type");
    Check(result.duration_status == VC_OK, "duration probe succeeds");
    Check(result.duration_ms == expected.duration_ms, "rounded duration matches manifest");
    Check(result.completed_frame_mask == expected.completed_mask,
          "completed frame mask matches manifest");

    for (uint32_t index = 0; index < VC_VIDEO_FRAME_COUNT; ++index) {
        const auto& frame = result.frames[index];
        Check(frame.standard_index == index, "standard frame index");
        Check(frame.sample_time_ms == expected.sample_times[index],
              "overflow-safe sample time");
        const bool completed = (expected.completed_mask & (1u << index)) != 0u;
        Check(frame.status == (completed ? VC_OK : VC_ERR_NO_FRAME),
              "per-frame status matches manifest");
        if (completed) {
            Check(AnyFeature(frame.features),
                  "successful frame computes all Task 6 features");
            Check(video_stats.display_widths[index] == expected.display_width &&
                      video_stats.display_heights[index] == expected.display_height,
                  "rotation and SAR corrected display dimensions");
        }
        std::cout << "LEGACY_FRAME|" << NarrowFixtureName(expected.name)
                  << '|' << index
                  << '|' << (frame.sample_time_ms * 1000)
                  << '|' << frame.status
                  << '|' << video_stats.selected_decode_ordinals[index]
                  << '|' << video_stats.selected_pts[index]
                  << '|' << video_stats.selected_pts_time_micros[index]
                  << '|' << unsigned(video_stats.selected_key_frames[index])
                  << '|' << video_stats.selected_picture_types[index]
                  << '|' << video_stats.display_widths[index]
                  << '|' << video_stats.display_heights[index]
                  << '|' << HexBytes(frame.features.pdq, sizeof(frame.features.pdq))
                  << '|' << frame.features.pdq_quality
                  << '|' << PHashHex(frame.features)
                  << '|' << SobelBitsHex(frame.features)
                  << '\n';
    }

    vc::detail::MediaSessionTestSnapshot after{};
    Check(vc::detail::GetMediaSessionTestSnapshot(session, &after),
          "session snapshot after video analyze");
    Check(after.io.create_file_calls == 1u &&
              before.io.create_file_calls == 1u,
          "video analyze reuses the single file handle");
    Check(video_stats.format_contexts == 1u,
          "video analyze creates exactly one AVFormatContext");
    Check(video_stats.codec_contexts ==
              (expected.top_status == VC_ERR_NO_FRAME ? 0u : 1u),
          "video analyze creates at most one shared codec context");
    std::wcout << L"VIDEO_DIFF " << expected.name
               << L" status=" << status
               << L" duration=" << result.duration_ms
               << L" mask=0x" << std::hex << result.completed_frame_mask
               << std::dec << L" open=" << after.io.create_file_calls
               << L" format=" << video_stats.format_contexts
               << L" codec=" << video_stats.codec_contexts
               << L" analysis_sha512="
               << std::wstring(analysis_digest.begin(), analysis_digest.end())
               << L'\n';
    vc_media_close(session);
}

void TestFrameMaskZeroAndSparseMask() {
    const std::wstring path = FixturePath(L"h264-standard.mp4");
    vc_media_open_options options{};
    options.struct_size = sizeof(options);
    options.abi_version = VC_ABI_VERSION;
    options.expected_media_type = VC_MEDIA_TYPE_VIDEO;
    vc_media_session* session = nullptr;
    vc_error error = FreshError();
    Check(vc_media_open_w(reinterpret_cast<const uint16_t*>(path.data()),
                          static_cast<uint32_t>(path.size()), &options,
                          nullptr, &session, &error) == VC_OK,
          "mask fixture opens");
    if (session == nullptr) return;
    std::array<uint8_t, VC_SHA512_SIZE> digest{};
    error = FreshError();
    Check(vc_media_hash(session, digest.data(), &error) == VC_OK,
          "mask fixture hashes");

    vc_analysis_result all = FreshResult();
    vc_analysis_request all_request = FreshRequest(0u);
    error = FreshError();
    Check(vc_media_analyze(session, &all_request, &all, &error) == VC_OK &&
              all.completed_frame_mask == VC_ALL_FRAME_MASK,
          "frame_mask zero normalizes to all six slots");

    vc_analysis_result sparse = FreshResult();
    vc_analysis_request sparse_request = FreshRequest(0x12u);
    error = FreshError();
    vc::detail::VideoAnalysisTestReset();
    Check(vc_media_analyze(session, &sparse_request, &sparse, &error) == VC_OK &&
              sparse.completed_frame_mask == 0x12u,
          "nonzero frame mask decodes only requested slots");
    const auto sparse_stats = vc::detail::VideoAnalysisTestGetStats();
    Check(sparse_stats.attempted_frame_mask == 0x12u,
          "sparse mask attempts seek/decode/features only for slots 1 and 4");
    for (uint32_t index = 0; index < VC_VIDEO_FRAME_COUNT; ++index) {
        if ((0x12u & (1u << index)) == 0u) {
            Check(sparse.frames[index].status == VC_ERR_UNSUPPORTED,
                  "unrequested slot is not decoded");
        }
    }
    vc_media_close(session);
}

vc_feature_set ReferenceRotatedRgbFeatures(const AVFrame& frame,
                                           int32_t* out_width,
                                           int32_t* out_height) {
    using videocore::native::GrayImage;
    GrayImage native;
    native.width = frame.width;
    native.height = frame.height;
    native.stride = frame.width;
    native.pixels.resize(static_cast<size_t>(frame.width) * frame.height);
    std::unique_ptr<SwsContext, decltype(&sws_freeContext)> converter(
        sws_getContext(frame.width, frame.height,
                       static_cast<AVPixelFormat>(frame.format),
                       frame.width, frame.height, AV_PIX_FMT_GRAY8,
                       SWS_BICUBIC, nullptr, nullptr, nullptr),
        &sws_freeContext);
    const int* bt709 = sws_getCoefficients(SWS_CS_ITU709);
    Check(converter != nullptr && bt709 != nullptr &&
              sws_setColorspaceDetails(converter.get(), bt709, 1,
                                       bt709, 1, 0, 1 << 16,
                                       1 << 16) >= 0,
          "independent RGB BT.709 full-range conversion configures");
    const uint8_t* source[4]{frame.data[0], nullptr, nullptr, nullptr};
    int source_stride[4]{frame.linesize[0], 0, 0, 0};
    uint8_t* destination[4]{native.pixels.data(), nullptr, nullptr, nullptr};
    int destination_stride[4]{native.stride, 0, 0, 0};
    Check(converter != nullptr &&
              sws_scale(converter.get(), source, source_stride, 0,
                        frame.height, destination, destination_stride) ==
                  frame.height,
          "independent RGB negative-stride conversion succeeds");

    GrayImage rotated;
    rotated.width = native.height;
    rotated.height = native.width;
    rotated.stride = rotated.width;
    rotated.pixels.resize(
        static_cast<size_t>(rotated.width) * rotated.height);
    for (int y = 0; y < native.height; ++y) {
        for (int x = 0; x < native.width; ++x) {
            const int dx = native.height - 1 - y;
            const int dy = x;
            rotated.pixels[static_cast<size_t>(dy) * rotated.stride + dx] =
                native.pixels[static_cast<size_t>(y) * native.stride + x];
        }
    }
    GrayImage scaled;
    scaled.width = 384;
    scaled.height = 512;
    scaled.stride = scaled.width;
    scaled.pixels.resize(
        static_cast<size_t>(scaled.width) * scaled.height);
    std::unique_ptr<SwsContext, decltype(&sws_freeContext)> scaler(
        sws_getContext(rotated.width, rotated.height, AV_PIX_FMT_GRAY8,
                       scaled.width, scaled.height, AV_PIX_FMT_GRAY8,
                       SWS_BICUBIC, nullptr, nullptr, nullptr),
        &sws_freeContext);
    Check(scaler != nullptr && bt709 != nullptr &&
              sws_setColorspaceDetails(scaler.get(), bt709, 1,
                                       bt709, 1, 0, 1 << 16,
                                       1 << 16) >= 0,
          "independent rotated gray full-range scale configures");
    const uint8_t* gray_source[4]{
        rotated.pixels.data(), nullptr, nullptr, nullptr};
    int gray_source_stride[4]{rotated.stride, 0, 0, 0};
    uint8_t* gray_destination[4]{
        scaled.pixels.data(), nullptr, nullptr, nullptr};
    int gray_destination_stride[4]{scaled.stride, 0, 0, 0};
    Check(scaler != nullptr &&
              sws_scale(scaler.get(), gray_source, gray_source_stride, 0,
                        rotated.height, gray_destination,
                        gray_destination_stride) == scaled.height,
          "independent rotated bicubic scale succeeds");

    vc_feature_set features{};
    std::array<uint8_t, VC_PDQ_SIZE> pdq{};
    int32_t quality = 0;
    std::array<uint64_t, VC_PHASH_COUNT> phash{};
    std::array<float, VC_SOBEL_HISTOGRAM_SIZE> sobel{};
    Check(videocore::native::ComputePdq(scaled, &pdq, &quality) ==
              videocore::native::ImageStatus::ok,
          "reference RGB PDQ succeeds");
    Check(videocore::native::ComputePHashParts(scaled, &phash) ==
              videocore::native::ImageStatus::ok,
          "reference RGB pHash succeeds");
    Check(videocore::native::ComputeSobelHistogram(scaled, &sobel) ==
              videocore::native::ImageStatus::ok,
          "reference RGB Sobel succeeds");
    std::memcpy(features.pdq, pdq.data(), pdq.size());
    features.pdq_quality = static_cast<uint32_t>(quality);
    std::memcpy(features.phash, phash.data(), sizeof(features.phash));
    std::memcpy(features.sobel_histogram, sobel.data(),
                sizeof(features.sobel_histogram));
    *out_width = scaled.width;
    *out_height = scaled.height;
    return features;
}

vc_feature_set ReferenceUnrotatedBt709Features(const AVFrame& frame,
                                               int32_t scaled_width,
                                               int32_t scaled_height) {
    using videocore::native::GrayImage;
    const int source_range =
        frame.color_range == AVCOL_RANGE_JPEG ? 1 : 0;
    const int* bt709 = sws_getCoefficients(SWS_CS_ITU709);
    GrayImage native;
    native.width = frame.width;
    native.height = frame.height;
    native.stride = frame.width;
    native.pixels.resize(static_cast<size_t>(native.width) * native.height);
    std::unique_ptr<SwsContext, decltype(&sws_freeContext)> converter(
        sws_getContext(frame.width, frame.height,
                       static_cast<AVPixelFormat>(frame.format),
                       frame.width, frame.height, AV_PIX_FMT_GRAY8,
                       SWS_BICUBIC, nullptr, nullptr, nullptr),
        &sws_freeContext);
    Check(converter != nullptr && bt709 != nullptr &&
              sws_setColorspaceDetails(converter.get(), bt709, source_range,
                                       bt709, source_range, 0, 1 << 16,
                                       1 << 16) >= 0,
          "independent unrotated BT.709 conversion configures");
    uint8_t* native_destination[4]{
        native.pixels.data(), nullptr, nullptr, nullptr};
    int native_destination_stride[4]{native.stride, 0, 0, 0};
    Check(converter != nullptr &&
              sws_scale(converter.get(), frame.data, frame.linesize, 0,
                        frame.height, native_destination,
                        native_destination_stride) == frame.height,
          "independent unrotated BT.709 conversion succeeds");

    GrayImage scaled;
    scaled.width = scaled_width;
    scaled.height = scaled_height;
    scaled.stride = scaled_width;
    scaled.pixels.resize(
        static_cast<size_t>(scaled.width) * scaled.height);
    std::unique_ptr<SwsContext, decltype(&sws_freeContext)> scaler(
        sws_getContext(native.width, native.height, AV_PIX_FMT_GRAY8,
                       scaled.width, scaled.height, AV_PIX_FMT_GRAY8,
                       SWS_BICUBIC, nullptr, nullptr, nullptr),
        &sws_freeContext);
    Check(scaler != nullptr && bt709 != nullptr &&
              sws_setColorspaceDetails(scaler.get(), bt709, source_range,
                                       bt709, 1, 0, 1 << 16,
                                       1 << 16) >= 0,
          "independent unrotated gray range scale configures");
    const uint8_t* gray_source[4]{
        native.pixels.data(), nullptr, nullptr, nullptr};
    int gray_source_stride[4]{native.stride, 0, 0, 0};
    uint8_t* gray_destination[4]{
        scaled.pixels.data(), nullptr, nullptr, nullptr};
    int gray_destination_stride[4]{scaled.stride, 0, 0, 0};
    Check(scaler != nullptr &&
              sws_scale(scaler.get(), gray_source, gray_source_stride, 0,
                        native.height, gray_destination,
                        gray_destination_stride) == scaled.height,
          "independent unrotated bicubic scale succeeds");

    vc_feature_set features{};
    std::array<uint8_t, VC_PDQ_SIZE> pdq{};
    int32_t quality = 0;
    std::array<uint64_t, VC_PHASH_COUNT> phash{};
    std::array<float, VC_SOBEL_HISTOGRAM_SIZE> sobel{};
    Check(videocore::native::ComputePdq(scaled, &pdq, &quality) ==
              videocore::native::ImageStatus::ok,
          "reference unrotated PDQ succeeds");
    Check(videocore::native::ComputePHashParts(scaled, &phash) ==
              videocore::native::ImageStatus::ok,
          "reference unrotated pHash succeeds");
    Check(videocore::native::ComputeSobelHistogram(scaled, &sobel) ==
              videocore::native::ImageStatus::ok,
          "reference unrotated Sobel succeeds");
    std::memcpy(features.pdq, pdq.data(), pdq.size());
    features.pdq_quality = static_cast<uint32_t>(quality);
    std::memcpy(features.phash, phash.data(), sizeof(features.phash));
    std::memcpy(features.sobel_histogram, sobel.data(),
                sizeof(features.sobel_histogram));
    return features;
}

void CheckUnrotatedFrameMatchesExplicitBt709Reference(
    AVFrame* frame,
    const std::string& label) {
    constexpr int32_t expected_width = 512;
    constexpr int32_t expected_height = 341;
    const vc_feature_set expected = ReferenceUnrotatedBt709Features(
        *frame, expected_width, expected_height);
    vc_feature_set actual{};
    int32_t actual_width = 0;
    int32_t actual_height = 0;
    const int32_t status = vc::detail::VideoAnalysisTestFrameToFeatures(
        frame, 0, 1, 1, &actual, &actual_width, &actual_height);
    Check(status == VC_OK, label + " converts through production pipeline");
    Check(actual_width == expected_width &&
              actual_height == expected_height,
          label + " dimensions match independent reference");
    Check(std::memcmp(actual.pdq, expected.pdq, sizeof(actual.pdq)) == 0 &&
              actual.pdq_quality == expected.pdq_quality &&
              std::memcmp(actual.phash, expected.phash,
                          sizeof(actual.phash)) == 0 &&
              std::memcmp(actual.sobel_histogram,
                          expected.sobel_histogram,
                          sizeof(actual.sobel_histogram)) == 0,
          label + " features match explicit BT.709/range reference");
}

void TestUnrotatedFramesUseExplicitColorspaceAndRangeConversion() {
    constexpr int width = 12;
    constexpr int height = 8;

    AVFrame* rgb = av_frame_alloc();
    Check(rgb != nullptr, "unrotated RGB frame allocates");
    if (rgb != nullptr) {
        rgb->format = AV_PIX_FMT_RGB24;
        rgb->width = width;
        rgb->height = height;
        rgb->color_range = AVCOL_RANGE_JPEG;
        rgb->colorspace = AVCOL_SPC_BT709;
        Check(av_frame_get_buffer(rgb, 32) >= 0,
              "unrotated RGB buffer allocates");
        for (int y = 0; y < height; ++y) {
            uint8_t* row = rgb->data[0] + y * rgb->linesize[0];
            for (int x = 0; x < width; ++x) {
                row[x * 3 + 0] = static_cast<uint8_t>(13 + x * 17 + y * 3);
                row[x * 3 + 1] = static_cast<uint8_t>(29 + x * 5 + y * 19);
                row[x * 3 + 2] = static_cast<uint8_t>(239 - x * 11 - y * 7);
            }
        }
        CheckUnrotatedFrameMatchesExplicitBt709Reference(
            rgb, "unrotated full-range RGB BT.709");
        av_frame_free(&rgb);
    }

    AVFrame* yuv = av_frame_alloc();
    Check(yuv != nullptr, "unrotated YUV frame allocates");
    if (yuv != nullptr) {
        yuv->format = AV_PIX_FMT_YUV420P;
        yuv->width = width;
        yuv->height = height;
        yuv->color_range = AVCOL_RANGE_MPEG;
        yuv->colorspace = AVCOL_SPC_BT709;
        Check(av_frame_get_buffer(yuv, 32) >= 0,
              "unrotated YUV buffer allocates");
        for (int y = 0; y < height; ++y) {
            uint8_t* row = yuv->data[0] + y * yuv->linesize[0];
            for (int x = 0; x < width; ++x) {
                row[x] = static_cast<uint8_t>(16 + ((x * 17 + y * 23) % 220));
            }
        }
        for (int y = 0; y < height / 2; ++y) {
            uint8_t* u = yuv->data[1] + y * yuv->linesize[1];
            uint8_t* v = yuv->data[2] + y * yuv->linesize[2];
            for (int x = 0; x < width / 2; ++x) {
                u[x] = static_cast<uint8_t>(64 + x * 19 + y * 7);
                v[x] = static_cast<uint8_t>(192 - x * 13 - y * 11);
            }
        }
        CheckUnrotatedFrameMatchesExplicitBt709Reference(
            yuv, "unrotated limited-range YUV BT.709");
        av_frame_free(&yuv);
    }
}

void TestRotatedRgbNegativeStrideUsesPixelFormatConversion() {
    constexpr int width = 8;
    constexpr int height = 6;
    constexpr int row_bytes = width * 3;
    std::array<uint8_t, row_bytes * height> storage{};
    AVFrame* frame = av_frame_alloc();
    Check(frame != nullptr, "RGB negative-stride frame allocates");
    if (frame == nullptr) return;
    frame->format = AV_PIX_FMT_RGB24;
    frame->width = width;
    frame->height = height;
    frame->color_range = AVCOL_RANGE_JPEG;
    frame->colorspace = AVCOL_SPC_BT709;
    frame->data[0] = storage.data() + row_bytes * (height - 1);
    frame->linesize[0] = -row_bytes;
    for (int y = 0; y < height; ++y) {
        uint8_t* row = frame->data[0] + y * frame->linesize[0];
        for (int x = 0; x < width; ++x) {
            row[x * 3 + 0] = static_cast<uint8_t>(17 + x * 21 + y * 7);
            row[x * 3 + 1] = static_cast<uint8_t>(31 + x * 5 + y * 29);
            row[x * 3 + 2] = static_cast<uint8_t>(223 - x * 13 - y * 9);
        }
    }
    int32_t expected_width = 0;
    int32_t expected_height = 0;
    const vc_feature_set expected =
        ReferenceRotatedRgbFeatures(*frame, &expected_width, &expected_height);
    vc_feature_set actual{};
    int32_t actual_width = 0;
    int32_t actual_height = 0;
    const int32_t status = vc::detail::VideoAnalysisTestFrameToFeatures(
        frame, 90, 1, 1, &actual, &actual_width, &actual_height);
    Check(status == VC_OK,
          "rotated RGB negative-stride frame converts through swscale");
    Check(actual_width == expected_width && actual_height == expected_height,
          "rotated RGB dimensions match independent reference");
    Check(std::memcmp(actual.pdq, expected.pdq, sizeof(actual.pdq)) == 0 &&
              actual.pdq_quality == expected.pdq_quality &&
              std::memcmp(actual.phash, expected.phash,
                          sizeof(actual.phash)) == 0 &&
              std::memcmp(actual.sobel_histogram, expected.sobel_histogram,
                          sizeof(actual.sobel_histogram)) == 0,
          "rotated RGB features match independent format-aware reference");
    av_frame_free(&frame);
}

void TestTimestampSaturationAndNormalization() {
    using Limits = std::numeric_limits<int64_t>;
    Check(vc::detail::VideoAnalysisTestTargetTimestamp(50, -100) == -50,
          "negative start target uses ordinary signed addition");
    Check(vc::detail::VideoAnalysisTestTargetTimestamp(-50, 100) == 50,
          "positive start and negative relative target add safely");
    Check(vc::detail::VideoAnalysisTestTargetTimestamp(1, (Limits::max)()) ==
              (Limits::max)(),
          "positive target overflow saturates high");
    Check(vc::detail::VideoAnalysisTestTargetTimestamp(-1, (Limits::min)()) ==
              (Limits::min)(),
          "negative target overflow saturates low");
    const int64_t selected_absolute = -50;
    Check(vc::detail::VideoAnalysisTestNormalizedTimestamp(
              selected_absolute, -100) == 50 &&
              selected_absolute >=
                  vc::detail::VideoAnalysisTestTargetTimestamp(50, -100),
          "negative-start selected identity normalizes deterministically");
}

void TestNegativeStartUsesDecoderPrerollWindowAndSelectedIdentity() {
    const std::wstring path = FixturePath(L"h264-standard.mp4");
    vc_media_open_options options{};
    options.struct_size = sizeof(options);
    options.abi_version = VC_ABI_VERSION;
    options.expected_media_type = VC_MEDIA_TYPE_VIDEO;
    vc_media_session* session = nullptr;
    vc_error error = FreshError();
    Check(vc_media_open_w(reinterpret_cast<const uint16_t*>(path.data()),
                          static_cast<uint32_t>(path.size()), &options,
                          nullptr, &session, &error) == VC_OK,
          "negative-start fixture opens");
    if (session == nullptr) return;
    std::array<uint8_t, VC_SHA512_SIZE> digest{};
    Check(vc_media_hash(session, digest.data(), &error) == VC_OK,
          "negative-start fixture hashes");
    vc::detail::VideoAnalysisTestReset();
    vc::detail::VideoAnalysisTestOverrideStreamStart(-1);
    vc_analysis_request request = FreshRequest(0x01u);
    vc_analysis_result result = FreshResult();
    error = FreshError();
    Check(vc_media_analyze(session, &request, &result, &error) == VC_OK &&
              result.completed_frame_mask == 0x01u,
          "negative-start decoder-preroll analysis publishes slot zero");
    const auto stats = vc::detail::VideoAnalysisTestGetStats();
    Check(stats.recovery_seek_call_count == 1u &&
              stats.seek_call_count == 1u,
          "negative-start analysis executes one decoder-preroll seek");
    Check(stats.recovery_seek_stream_index == 0 &&
              stats.recovery_seek_min == -12289 &&
              stats.recovery_seek_target == -12289 &&
              stats.recovery_seek_max == -1 &&
              stats.recovery_seek_flags == AVSEEK_FLAG_BACKWARD,
          "negative-start decoder-preroll uses derived target and real-start upper bound");
    Check(stats.selected_decode_ordinals[0] == 3 &&
              stats.selected_pts[0] == 3073 &&
              stats.selected_key_frames[0] == 0u &&
              stats.selected_picture_types[0] == 'P',
          "negative-start production path selects the frozen absolute frame");
    vc_media_close(session);
}

void TestRecoverableReadErrorsContinueWithoutReseek() {
    const std::wstring path = FixturePath(L"h264-standard.mp4");
    vc_media_open_options options{};
    options.struct_size = sizeof(options);
    options.abi_version = VC_ABI_VERSION;
    options.expected_media_type = VC_MEDIA_TYPE_VIDEO;
    vc_media_session* session = nullptr;
    vc_error error = FreshError();
    Check(vc_media_open_w(reinterpret_cast<const uint16_t*>(path.data()),
                          static_cast<uint32_t>(path.size()), &options,
                          nullptr, &session, &error) == VC_OK,
          "recoverable read fixture opens");
    if (session == nullptr) return;
    std::array<uint8_t, VC_SHA512_SIZE> digest{};
    Check(vc_media_hash(session, digest.data(), &error) == VC_OK,
          "recoverable read fixture hashes");
    for (int32_t injected : {AVERROR(EAGAIN), AVERROR_INVALIDDATA}) {
        vc::detail::VideoAnalysisTestReset();
        vc::detail::VideoAnalysisTestInjectReadError(0u, injected, 1u);
        vc_analysis_request request = FreshRequest(0x01u);
        vc_analysis_result result = FreshResult();
        error = FreshError();
        Check(vc_media_analyze(session, &request, &result, &error) == VC_OK &&
                  result.completed_frame_mask == 0x01u,
              injected == AVERROR(EAGAIN)
                  ? "transient read EAGAIN retries then succeeds"
                  : "recoverable invalid packet skips then succeeds");
        const auto stats = vc::detail::VideoAnalysisTestGetStats();
        Check(stats.injected_read_error_count == 1u &&
                  stats.seek_call_count == 1u,
              injected == AVERROR(EAGAIN)
                  ? "transient read EAGAIN does not trigger a reseek"
                  : "recoverable invalid packet does not trigger a reseek");
    }
    std::array<int32_t, 17> nonconsecutive_eagain{};
    for (size_t index = 0; index < nonconsecutive_eagain.size(); ++index) {
        nonconsecutive_eagain[index] =
            (index % 2u) == 0u ? AVERROR(EAGAIN) : 0;
    }
    vc::detail::VideoAnalysisTestReset();
    vc::detail::VideoAnalysisTestInjectReadPlan(
        5u,
        nonconsecutive_eagain.data(),
        static_cast<uint32_t>(nonconsecutive_eagain.size()));
    vc_analysis_request separated_request = FreshRequest(0x20u);
    vc_analysis_result separated_result = FreshResult();
    error = FreshError();
    Check(vc_media_analyze(session, &separated_request, &separated_result,
                           &error) == VC_OK &&
              separated_result.completed_frame_mask == 0x20u,
          "successful packets reset the consecutive read EAGAIN budget");
    const auto separated_stats = vc::detail::VideoAnalysisTestGetStats();
    Check(separated_stats.injected_read_error_count == 9u &&
              separated_stats.planned_successful_read_count == 8u,
          "nonconsecutive EAGAIN plan executes nine errors separated by eight packets");
    vc::detail::VideoAnalysisTestReset();
    vc::detail::VideoAnalysisTestInjectReadError(
        1u, AVERROR(EAGAIN), 20u);
    vc_analysis_request bounded_request = FreshRequest();
    vc_analysis_result bounded_result = FreshResult();
    error = FreshError();
    Check(vc_media_analyze(session, &bounded_request, &bounded_result,
                           &error) == VC_OK &&
              bounded_result.completed_frame_mask == 0x01u &&
              bounded_result.frames[1].status == VC_ERR_DEMUX,
          "persistent read EAGAIN exhausts its bound and preserves earlier success");
    const auto bounded_stats = vc::detail::VideoAnalysisTestGetStats();
    Check(bounded_stats.injected_read_error_count == 9u &&
              bounded_stats.attempted_frame_mask == 0x03u,
          "persistent read EAGAIN performs eight retries then stops later work");
    vc_media_close(session);
}

void TestHardFailureStopsRemainingWork(bool seek_failure) {
    const std::wstring path = FixturePath(L"h264-standard.mp4");
    vc_media_open_options options{};
    options.struct_size = sizeof(options);
    options.abi_version = VC_ABI_VERSION;
    options.expected_media_type = VC_MEDIA_TYPE_VIDEO;
    vc_media_session* session = nullptr;
    vc_error error = FreshError();
    Check(vc_media_open_w(reinterpret_cast<const uint16_t*>(path.data()),
                          static_cast<uint32_t>(path.size()), &options,
                          nullptr, &session, &error) == VC_OK,
          "hard failure fixture opens");
    if (session == nullptr) return;
    std::array<uint8_t, VC_SHA512_SIZE> digest{};
    Check(vc_media_hash(session, digest.data(), &error) == VC_OK,
          "hard failure fixture hashes");
    vc::detail::VideoAnalysisTestReset();
    if (seek_failure) {
        vc::detail::VideoAnalysisTestInjectSeekError(
            1u, AVERROR(ENOSYS), 1u);
    } else {
        vc::detail::VideoAnalysisTestInjectReadError(
            1u, AVERROR(EIO), 1u);
    }
    vc_analysis_request request = FreshRequest();
    vc_analysis_result result = FreshResult();
    error = FreshError();
    Check(vc_media_analyze(session, &request, &result, &error) == VC_OK,
          "hard failure retains ordinary partial-success top status");
    const int32_t expected_error = seek_failure ? VC_ERR_IO : VC_ERR_DEMUX;
    Check(result.completed_frame_mask == 0x01u,
          "hard failure retains only the earlier successful slot");
    for (uint32_t index = 1; index < VC_VIDEO_FRAME_COUNT; ++index) {
        Check(result.frames[index].status == expected_error,
              "hard failure is copied to every remaining requested slot");
    }
    const auto stats = vc::detail::VideoAnalysisTestGetStats();
    Check(stats.attempted_frame_mask == 0x03u,
          "hard failure enters failing slot then stops later work");
    Check(stats.hard_failure_count == 1u,
          "hard failure is not retried inside or after the failing slot");
    vc_media_close(session);
}

void TestRequestedFeatureFamiliesPublishIndependently() {
    const std::wstring path = FixturePath(L"h264-standard.mp4");
    vc_media_open_options options{};
    options.struct_size = sizeof(options);
    options.abi_version = VC_ABI_VERSION;
    options.expected_media_type = VC_MEDIA_TYPE_VIDEO;
    vc_media_session* session = nullptr;
    vc_error error = FreshError();
    Check(vc_media_open_w(reinterpret_cast<const uint16_t*>(path.data()),
                          static_cast<uint32_t>(path.size()), &options,
                          nullptr, &session, &error) == VC_OK,
          "feature-family fixture opens");
    if (session == nullptr) return;
    std::array<uint8_t, VC_SHA512_SIZE> digest{};
    Check(vc_media_hash(session, digest.data(), &error) == VC_OK,
          "feature-family fixture hashes");
    for (uint64_t feature : {uint64_t{VC_FEATURE_PDQ},
                             uint64_t{VC_FEATURE_PHASH},
                             uint64_t{VC_FEATURE_SOBEL}}) {
        vc_analysis_request request = FreshRequest(0x01u);
        request.feature_mask = VC_FEATURE_DURATION | feature;
        vc_analysis_result result = FreshResult();
        error = FreshError();
        Check(vc_media_analyze(session, &request, &result, &error) == VC_OK &&
                  result.completed_frame_mask == 0x01u,
              "single requested feature family succeeds");
        const auto& features = result.frames[0].features;
        Check(HasPdq(features) == (feature == VC_FEATURE_PDQ),
              "PDQ and quality publish only when requested");
        Check(HasPHash(features) == (feature == VC_FEATURE_PHASH),
              "pHash publishes only when requested");
        Check(HasSobel(features) == (feature == VC_FEATURE_SOBEL),
              "Sobel publishes only when requested");
    }
    vc_media_close(session);
}

struct CancelBeforePublishContext {
    vc_cancel_token* token = nullptr;
    uint32_t cancel_at_index = 0u;
};

void CancelBeforePublish(uint32_t frame_index, void* opaque) noexcept {
    auto* context = static_cast<CancelBeforePublishContext*>(opaque);
    if (context != nullptr && frame_index == context->cancel_at_index) {
        vc_cancel_request(context->token);
    }
}

void TestInterruptBeforePublishClearsAllFrames(uint32_t frame_mask,
                                               uint32_t cancel_at_index,
                                               const char* label) {
    const std::wstring path = FixturePath(L"h264-standard.mp4");
    vc_cancel_token* token = nullptr;
    vc_error error = FreshError();
    Check(vc_cancel_create(&token, &error) == VC_OK,
          std::string(label) + " cancel token creates");
    vc_media_open_options options{};
    options.struct_size = sizeof(options);
    options.abi_version = VC_ABI_VERSION;
    options.expected_media_type = VC_MEDIA_TYPE_VIDEO;
    vc_media_session* session = nullptr;
    error = FreshError();
    Check(vc_media_open_w(reinterpret_cast<const uint16_t*>(path.data()),
                          static_cast<uint32_t>(path.size()), &options,
                          token, &session, &error) == VC_OK,
          std::string(label) + " fixture opens");
    if (session != nullptr) {
        std::array<uint8_t, VC_SHA512_SIZE> digest{};
        error = FreshError();
        Check(vc_media_hash(session, digest.data(), &error) == VC_OK,
              std::string(label) + " fixture hashes");
        CancelBeforePublishContext context{token, cancel_at_index};
        vc::detail::VideoAnalysisTestReset();
        vc::detail::VideoAnalysisTestSetBeforePublishHook(
            &CancelBeforePublish, &context);
        vc_analysis_request request = FreshRequest(frame_mask);
        vc_analysis_result result = FreshResult();
        error = FreshError();
        const int32_t status =
            vc_media_analyze(session, &request, &result, &error);
        Check(status == VC_ERR_CANCELLED,
              std::string(label) + " returns cancellation");
        Check(result.completed_frame_mask == 0u,
              std::string(label) + " clears completed mask");
        for (const auto& frame : result.frames) {
            Check(FeatureSetIsZero(frame.features),
                  std::string(label) + " clears every frame payload");
        }
        vc_media_close(session);
    }
    vc_cancel_free(token);
}

void TestSendPacketEagainResendsSamePacket() {
    const std::wstring path = FixturePath(L"h264-bframes.mp4");
    vc_media_open_options options{};
    options.struct_size = sizeof(options);
    options.abi_version = VC_ABI_VERSION;
    options.expected_media_type = VC_MEDIA_TYPE_VIDEO;
    vc_media_session* session = nullptr;
    vc_error error = FreshError();
    Check(vc_media_open_w(reinterpret_cast<const uint16_t*>(path.data()),
                          static_cast<uint32_t>(path.size()), &options,
                          nullptr, &session, &error) == VC_OK,
          "EAGAIN fixture opens");
    if (session == nullptr) return;
    std::array<uint8_t, VC_SHA512_SIZE> digest{};
    error = FreshError();
    Check(vc_media_hash(session, digest.data(), &error) == VC_OK,
          "EAGAIN fixture hashes");
    vc::detail::VideoAnalysisTestReset();
    vc::detail::VideoAnalysisTestForceSendEagainOnce();
    vc_analysis_request request = FreshRequest(0x01u);
    vc_analysis_result result = FreshResult();
    error = FreshError();
    Check(vc_media_analyze(session, &request, &result, &error) == VC_OK,
          "EAGAIN recovery still produces requested frame");
    const auto stats = vc::detail::VideoAnalysisTestGetStats();
    Check(stats.forced_send_eagain == 1u && stats.same_packet_resends == 1u,
          "EAGAIN resends the same packet before it is unreferenced");
    vc_media_close(session);
}


void TestTimeoutBeforePublishClearsAllFrames() {
    const std::wstring path = FixturePath(L"h264-standard.mp4");
    vc_media_open_options options{};
    options.struct_size = sizeof(options);
    options.abi_version = VC_ABI_VERSION;
    options.expected_media_type = VC_MEDIA_TYPE_VIDEO;
    vc_media_session* session = nullptr;
    vc_error error = FreshError();
    Check(vc_media_open_w(reinterpret_cast<const uint16_t*>(path.data()),
                          static_cast<uint32_t>(path.size()), &options,
                          nullptr, &session, &error) == VC_OK,
          "pre-publish timeout fixture opens");
    if (session == nullptr) return;
    std::array<uint8_t, VC_SHA512_SIZE> digest{};
    error = FreshError();
    Check(vc_media_hash(session, digest.data(), &error) == VC_OK,
          "pre-publish timeout fixture hashes");
    vc::detail::VideoAnalysisTestReset();
    vc::detail::VideoAnalysisTestForceTimeoutBeforePublishOnce();
    vc_analysis_request request = FreshRequest(0x01u);
    vc_analysis_result result = FreshResult();
    error = FreshError();
    Check(vc_media_analyze(session, &request, &result, &error) ==
              VC_ERR_TIMEOUT,
          "post-I/O pre-publish timeout returns timeout");
    Check(result.completed_frame_mask == 0u,
          "post-I/O pre-publish timeout clears completed mask");
    Check(FeatureSetIsZero(result.frames[0].features),
          "post-I/O pre-publish timeout clears frame payload");
    vc_media_close(session);
}

void DelayAtIoBoundary(vc::detail::IoBoundary boundary, void*) noexcept {
    if (boundary == vc::detail::IoBoundary::before_seek ||
        boundary == vc::detail::IoBoundary::before_read) {
        Sleep(20u);
    }
}

void TestProbeDeadlineAndOverflowSafeSampling() {
    const std::wstring path = FixturePath(L"h264-standard.mp4");
    vc_media_open_options options{};
    options.struct_size = sizeof(options);
    options.abi_version = VC_ABI_VERSION;
    options.expected_media_type = VC_MEDIA_TYPE_VIDEO;

    vc_media_session* timeout_session = nullptr;
    vc_error error = FreshError();
    Check(vc_media_open_w(reinterpret_cast<const uint16_t*>(path.data()),
                          static_cast<uint32_t>(path.size()), &options,
                          nullptr, &timeout_session, &error) == VC_OK,
          "timeout fixture opens");
    if (timeout_session != nullptr) {
        std::array<uint8_t, VC_SHA512_SIZE> digest{};
        error = FreshError();
        Check(vc_media_hash(timeout_session, digest.data(), &error) == VC_OK,
              "timeout fixture hashes");
        Check(vc::detail::SetMediaSessionTestIoHook(
                  timeout_session, &DelayAtIoBoundary, nullptr),
              "timeout hook attaches to the session handle");
        vc_analysis_request request = FreshRequest(0x01u);
        request.probe_timeout_ms = 1u;
        vc_analysis_result result = FreshResult();
        error = FreshError();
        Check(vc_media_analyze(timeout_session, &request, &result, &error) ==
                  VC_ERR_TIMEOUT,
              "probe deadline returns VC_ERR_TIMEOUT");
        Check(result.duration_status == VC_ERR_TIMEOUT &&
                  result.completed_frame_mask == 0u,
              "probe timeout publishes no partial frame payload");
        vc_media_close(timeout_session);
    }

    vc_media_session* overflow_session = nullptr;
    error = FreshError();
    Check(vc_media_open_w(reinterpret_cast<const uint16_t*>(path.data()),
                          static_cast<uint32_t>(path.size()), &options,
                          nullptr, &overflow_session, &error) == VC_OK,
          "overflow fixture opens");
    if (overflow_session != nullptr) {
        std::array<uint8_t, VC_SHA512_SIZE> digest{};
        error = FreshError();
        Check(vc_media_hash(overflow_session, digest.data(), &error) == VC_OK,
              "overflow fixture hashes");
        vc_analysis_request request = FreshRequest(0x20u);
        request.known_duration_ms = (std::numeric_limits<int64_t>::max)();
        vc_analysis_result result = FreshResult();
        error = FreshError();
        (void)vc_media_analyze(overflow_session, &request, &result, &error);
        const int64_t duration = request.known_duration_ms;
        const int64_t expected =
            (duration / 12) * 11 + ((duration % 12) * 11) / 12;
        Check(result.frames[5].sample_time_ms == expected && expected > 0,
              "sampling formula avoids duration multiplication overflow");
        vc_media_close(overflow_session);
    }
}

}  // namespace

int main() {
    for (const auto& fixture : fixtures) TestFixture(fixture);
    TestFrameMaskZeroAndSparseMask();
    TestRotatedRgbNegativeStrideUsesPixelFormatConversion();
    TestUnrotatedFramesUseExplicitColorspaceAndRangeConversion();
    TestTimestampSaturationAndNormalization();
    TestNegativeStartUsesDecoderPrerollWindowAndSelectedIdentity();
    TestRecoverableReadErrorsContinueWithoutReseek();
    TestHardFailureStopsRemainingWork(false);
    TestHardFailureStopsRemainingWork(true);
    TestRequestedFeatureFamiliesPublishIndependently();
    TestInterruptBeforePublishClearsAllFrames(
        0x03u, 1u, "later-slot cancellation");
    TestInterruptBeforePublishClearsAllFrames(
        0x01u, 0u, "post-I/O pre-publish cancellation");
    TestSendPacketEagainResendsSamePacket();
    TestTimeoutBeforePublishClearsAllFrames();
    TestProbeDeadlineAndOverflowSafeSampling();
    if (failures != 0) {
        std::cerr << failures << " video analysis test(s) failed\n";
        return 1;
    }
    std::cout << "videocore video analysis tests passed\n";
    return 0;
}
