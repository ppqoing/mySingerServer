#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <bcrypt.h>
#include <turbojpeg.h>

#include <algorithm>
#include <array>
#include <cstdint>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <iomanip>
#include <iostream>
#include <limits>
#include <memory>
#include <sstream>
#include <string>
#include <utility>
#include <vector>

#include "contact_sheet.h"
#include "media_session.h"
#include "native_algorithms/gray_image.h"
#include "video_analysis.h"
#include "videocore/videocore.h"

namespace {

int failures = 0;

void Check(bool condition, const char* message) {
    if (!condition) {
        std::cerr << "FAIL: " << message << '\n';
        ++failures;
    }
}

videocore::native::GrayImage ConstantImage(int width,
                                            int height,
                                            uint8_t value) {
    videocore::native::GrayImage image;
    image.width = width;
    image.height = height;
    image.stride = width;
    image.pixels.assign(static_cast<size_t>(width) * height, value);
    return image;
}

bool FeaturesAreZero(const videocore::native::ImageFeatures& features) {
    return std::all_of(features.pdq.begin(), features.pdq.end(),
                       [](uint8_t value) { return value == 0u; }) &&
           features.quality == 0 &&
           std::all_of(features.phash_parts.begin(),
                       features.phash_parts.end(),
                       [](uint64_t value) { return value == 0u; }) &&
           std::all_of(features.sobel_hist.begin(), features.sobel_hist.end(),
                       [](float value) { return value == 0.0f; });
}

bool PublicFeaturesAreZero(const vc_feature_set& features) {
    return std::all_of(std::begin(features.pdq), std::end(features.pdq),
                       [](uint8_t value) { return value == 0u; }) &&
           features.pdq_quality == 0u &&
           std::all_of(std::begin(features.phash), std::end(features.phash),
                       [](uint64_t value) { return value == 0u; }) &&
           std::all_of(std::begin(features.sobel_histogram),
                       std::end(features.sobel_histogram),
                       [](float value) { return value == 0.0f; });
}

std::filesystem::path UniqueTempDirectory() {
    const std::wstring name = L"videocore-contact-联系表-" +
        std::to_wstring(GetCurrentProcessId()) + L"-" +
        std::to_wstring(GetTickCount64());
    const std::filesystem::path path =
        std::filesystem::temp_directory_path() / name;
    std::filesystem::create_directories(path);
    return path;
}

std::vector<uint8_t> ReadBytes(const std::filesystem::path& path) {
    std::ifstream input(path, std::ios::binary);
    return std::vector<uint8_t>(std::istreambuf_iterator<char>(input), {});
}

std::string Sha256(const std::vector<uint8_t>& bytes) {
    BCRYPT_ALG_HANDLE algorithm = nullptr;
    BCRYPT_HASH_HANDLE hash = nullptr;
    std::array<uint8_t, 32> digest{};
    if (BCryptOpenAlgorithmProvider(
            &algorithm, BCRYPT_SHA256_ALGORITHM, nullptr, 0) < 0 ||
        BCryptCreateHash(
            algorithm, &hash, nullptr, 0, nullptr, 0, 0) < 0 ||
        (!bytes.empty() &&
         BCryptHashData(hash,
                        const_cast<PUCHAR>(bytes.data()),
                        static_cast<ULONG>(bytes.size()),
                        0) < 0) ||
        BCryptFinishHash(hash, digest.data(),
                         static_cast<ULONG>(digest.size()), 0) < 0) {
        if (hash != nullptr) BCryptDestroyHash(hash);
        if (algorithm != nullptr) BCryptCloseAlgorithmProvider(algorithm, 0);
        return {};
    }
    BCryptDestroyHash(hash);
    BCryptCloseAlgorithmProvider(algorithm, 0);
    std::ostringstream output;
    output << std::hex << std::setfill('0');
    for (uint8_t byte : digest) {
        output << std::setw(2) << static_cast<unsigned>(byte);
    }
    return output.str();
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

void TestRowMajorLayout() {
    std::array<videocore::native::GrayImage, VC_VIDEO_FRAME_COUNT> images;
    std::array<const videocore::native::GrayImage*, VC_VIDEO_FRAME_COUNT>
        frames{};
    for (uint32_t index = 0; index < VC_VIDEO_FRAME_COUNT; ++index) {
        images[index] = ConstantImage(8, 8,
            static_cast<uint8_t>(10u + index * 20u));
        frames[index] = &images[index];
    }

    vc::detail::ContactSheetResult result;
    const int32_t status =
        vc::detail::BuildContactSheet(frames, 8u, &result);
    Check(status == VC_OK, "six valid frames build a contact sheet");
    Check(result.state == VC_OK, "contact sheet state is success");
    Check(result.successful_mask == VC_ALL_FRAME_MASK,
          "all six slots are successful");
    Check(result.placeholder_mask == 0u, "no placeholder is used");
    Check(result.tile_width == 8 && result.tile_height == 8,
          "tile dimensions preserve an 8x8 source");
    Check(result.width == 24 && result.height == 16,
          "canvas is exactly three tiles by two tiles");

    if (result.canvas.pixels.size() == 24u * 16u) {
        for (uint32_t index = 0; index < VC_VIDEO_FRAME_COUNT; ++index) {
            const int cell_x = static_cast<int>(index % 3u) * 8;
            const int cell_y = static_cast<int>(index / 3u) * 8;
            const uint8_t expected = static_cast<uint8_t>(10u + index * 20u);
            for (int y = 0; y < 8; ++y) {
                for (int x = 0; x < 8; ++x) {
                    const uint8_t actual = result.canvas.pixels[
                        static_cast<size_t>(cell_y + y) * result.canvas.stride +
                        cell_x + x];
                    Check(actual == expected,
                          "row-major tiles have exact pixels and no gaps");
                }
            }
        }
    } else {
        Check(false, "canvas owns all expected pixels");
    }
}

void TestTileDimensions() {
    int width = 0;
    int height = 0;
    Check(vc::detail::ContactSheetTileDimensions(
              16, 8, 10u, &width, &height) == VC_OK &&
              width == 10 && height == 5,
          "landscape tile uses nearest integer scaling");
    Check(vc::detail::ContactSheetTileDimensions(
              8, 16, 10u, &width, &height) == VC_OK &&
              width == 5 && height == 10,
          "portrait tile uses nearest integer scaling");
    Check(vc::detail::ContactSheetTileDimensions(
              8, 4096, 8u, &width, &height) == VC_OK &&
              width == 1 && height == 8,
          "extreme aspect ratio keeps the short edge at least one");
    Check(vc::detail::ContactSheetTileDimensions(
              16, 9, 0u, &width, &height) == VC_OK &&
              width == 256 && height == 144,
          "zero tile side defaults to 256 and rounds to nearest");
    Check(vc::detail::ContactSheetTileDimensions(
              INT32_MAX, INT32_MAX, UINT32_MAX, &width, &height) ==
              VC_ERR_OUTPUT_TOO_LARGE,
          "oversized tile request is rejected without overflow");
    Check(vc::detail::ContactSheetTileDimensions(
              8, 8, 1984u, &width, &height) == VC_OK &&
              width == 1984 && height == 1984,
          "working-set budget accepts the final square tile below 256 MiB");
    Check(vc::detail::ContactSheetTileDimensions(
              8, 8, 1985u, &width, &height) ==
              VC_ERR_OUTPUT_TOO_LARGE,
          "working-set budget rejects the first square tile above 256 MiB");
    Check(vc::detail::ContactSheetTileDimensions(
              8, 12000000, 1500000u, &width, &height) ==
              VC_ERR_OUTPUT_TOO_LARGE,
          "working-set budget includes the feature-only padded gray plane");
}

uint8_t CanvasPixel(const vc::detail::ContactSheetResult& result,
                    int x,
                    int y) {
    return result.canvas.pixels[
        static_cast<size_t>(y) * result.canvas.stride + x];
}

bool PlaceholderHasEightConnectedDiagonal(
    const vc::detail::ContactSheetResult& result,
    uint32_t slot,
    bool descending) {
    if (result.tile_width <= 0 || result.tile_height <= 0 ||
        result.canvas.pixels.empty()) {
        return false;
    }
    const int origin_x = static_cast<int>(slot % 3u) * result.tile_width;
    const int origin_y = static_cast<int>(slot / 3u) * result.tile_height;
    const int start_x = descending ? 0 : result.tile_width - 1;
    const int target_x = descending ? result.tile_width - 1 : 0;
    const int target_y = result.tile_height - 1;
    const auto is_line = [&](int x, int y) {
        return x >= 0 && x < result.tile_width && y >= 0 &&
               y < result.tile_height &&
               CanvasPixel(result, origin_x + x, origin_y + y) == 192u;
    };
    if (!is_line(start_x, 0) || !is_line(target_x, target_y)) return false;
    std::vector<uint8_t> visited(
        static_cast<size_t>(result.tile_width) * result.tile_height, 0u);
    std::vector<std::pair<int, int>> pending{{start_x, 0}};
    visited[static_cast<size_t>(start_x)] = 1u;
    for (size_t cursor = 0u; cursor < pending.size(); ++cursor) {
        const auto [x, y] = pending[cursor];
        if (x == target_x && y == target_y) return true;
        for (int dy = -1; dy <= 1; ++dy) {
            for (int dx = -1; dx <= 1; ++dx) {
                if ((dx == 0 && dy == 0) || !is_line(x + dx, y + dy)) {
                    continue;
                }
                const size_t offset =
                    static_cast<size_t>(y + dy) * result.tile_width + x + dx;
                if (visited[offset] == 0u) {
                    visited[offset] = 1u;
                    pending.emplace_back(x + dx, y + dy);
                }
            }
        }
    }
    return false;
}

void CheckContinuousPlaceholder(int source_width,
                                int source_height,
                                uint32_t tile_max_side,
                                const char* message) {
    auto authority = ConstantImage(source_width, source_height, 17u);
    std::array<const videocore::native::GrayImage*, VC_VIDEO_FRAME_COUNT>
        frames{};
    frames[0] = &authority;
    vc::detail::ContactSheetResult result;
    const int32_t status =
        vc::detail::BuildContactSheet(frames, tile_max_side, &result);
    Check(status == VC_OK &&
              PlaceholderHasEightConnectedDiagonal(result, 1u, true) &&
              PlaceholderHasEightConnectedDiagonal(result, 1u, false),
          message);
}

void TestContinuousPlaceholderRaster() {
    CheckContinuousPlaceholder(
        16, 8, 16u, "16x8 placeholder diagonals are 8-connected");
    CheckContinuousPlaceholder(
        1024, 256, 256u, "256x64 placeholder diagonals are 8-connected");
    CheckContinuousPlaceholder(
        2048, 8, 256u, "256x1 placeholder covers the complete thin edge");
    CheckContinuousPlaceholder(
        8, 2048, 256u, "1x256 placeholder covers the complete thin edge");
}

bool JpegHasDimensions(const std::filesystem::path& path,
                       int expected_width,
                       int expected_height) {
    const auto bytes = ReadBytes(path);
    using TurboHandle = std::unique_ptr<void, decltype(&tjDestroy)>;
    TurboHandle decoder(tjInitDecompress(), &tjDestroy);
    int width = 0;
    int height = 0;
    int subsamp = -1;
    int colorspace = -1;
    return decoder != nullptr && !bytes.empty() &&
           tjDecompressHeader3(decoder.get(), bytes.data(),
                               static_cast<unsigned long>(bytes.size()),
                               &width, &height, &subsamp, &colorspace) == 0 &&
           width == expected_width && height == expected_height &&
           subsamp == TJSAMP_GRAY && colorspace == TJCS_GRAY;
}

void CheckExtremeAspectContact(int source_width,
                               int source_height,
                               uint32_t tile_max_side,
                               int expected_width,
                               int expected_height,
                               const std::filesystem::path& path,
                               const char* message) {
    auto source = ConstantImage(source_width, source_height, 31u);
    std::array<const videocore::native::GrayImage*, VC_VIDEO_FRAME_COUNT>
        frames{};
    frames[0] = &source;
    const std::wstring path_text = path.native();
    vc::detail::ContactSheetResult result;
    const int32_t status = vc::detail::GenerateContactSheet(
        frames, tile_max_side,
        reinterpret_cast<const uint16_t*>(path_text.data()),
        static_cast<uint32_t>(path_text.size()), &result);
    Check(status == VC_OK && result.width == expected_width &&
              result.height == expected_height &&
              result.canvas.width == expected_width &&
              result.canvas.height == expected_height &&
              !FeaturesAreZero(result.features) &&
              JpegHasDimensions(path, expected_width, expected_height),
          message);
}

void TestExtremeAspectUsesFeatureOnlyMinimum() {
    const auto directory = UniqueTempDirectory();
    CheckExtremeAspectContact(1, 512, 0u, 3, 512,
                              directory / L"portrait-normalized.jpg",
                              "normalized 1x512 source keeps a public 3x512 JPEG");
    CheckExtremeAspectContact(512, 1, 0u, 768, 2,
                              directory / L"landscape-normalized.jpg",
                              "normalized 512x1 source keeps a public 768x2 JPEG");
    CheckExtremeAspectContact(8, 4096, 0u, 3, 512,
                              directory / L"portrait-default.jpg",
                              "portrait default keeps a public 3x512 JPEG");
    CheckExtremeAspectContact(8, 4096, 8u, 3, 16,
                              directory / L"portrait-small.jpg",
                              "portrait small tile keeps a public 3x16 JPEG");
    CheckExtremeAspectContact(4096, 8, 0u, 768, 2,
                              directory / L"landscape-default.jpg",
                              "landscape default keeps a public 768x2 JPEG");
    CheckExtremeAspectContact(4096, 8, 8u, 24, 2,
                              directory / L"landscape-small.jpg",
                              "landscape small tile keeps a public 24x2 JPEG");
    std::filesystem::remove_all(directory);
}

void TestContactSourceStrideAndBufferBounds() {
    auto authority = ConstantImage(8, 8, 17u);
    auto bad_stride = ConstantImage(1, 512, 31u);
    bad_stride.stride = 0;
    auto short_buffer = ConstantImage(512, 1, 47u);
    short_buffer.pixels.resize(511u);
    std::array<const videocore::native::GrayImage*, VC_VIDEO_FRAME_COUNT>
        frames{};
    frames[0] = &authority;
    frames[1] = &bad_stride;
    frames[2] = &short_buffer;
    vc::detail::ContactSheetResult result;
    Check(vc::detail::BuildContactSheet(frames, 8u, &result) == VC_OK &&
              result.successful_mask == 0x01u &&
              result.placeholder_mask == 0x3eu,
          "contact source validation rejects bad stride and short buffers");
}

void TestPartialAndPlaceholderPixels() {
    auto first = ConstantImage(32, 32, 23u);
    auto fourth = ConstantImage(32, 32, 77u);
    std::array<const videocore::native::GrayImage*, VC_VIDEO_FRAME_COUNT>
        frames{};
    frames[0] = &first;
    frames[3] = &fourth;
    vc::detail::ContactSheetResult small;
    Check(vc::detail::BuildContactSheet(frames, 32u, &small) == VC_OK,
          "partial contact sheet succeeds");
    Check(small.successful_mask == 0x09u,
          "partial successful mask is exact");
    Check(small.placeholder_mask == 0x36u,
          "partial placeholder mask complements successful slots");
    Check(CanvasPixel(small, 0, 0) == 23u &&
              CanvasPixel(small, 0, 32) == 77u,
          "successful tiles preserve their luma");
    Check(CanvasPixel(small, 32 + 0, 0) == 192u &&
              CanvasPixel(small, 32 + 31, 0) == 192u &&
              CanvasPixel(small, 32 + 16, 0) == 96u,
          "small placeholder uses luma 96 and one-pixel X luma 192");

    auto large_source = ConstantImage(64, 64, 17u);
    frames.fill(nullptr);
    frames[0] = &large_source;
    vc::detail::ContactSheetResult large;
    Check(vc::detail::BuildContactSheet(frames, 64u, &large) == VC_OK,
          "large partial contact sheet succeeds");
    Check(CanvasPixel(large, 64 + 0, 0) == 192u &&
              CanvasPixel(large, 64 + 1, 0) == 192u &&
              CanvasPixel(large, 64 + 2, 0) == 96u,
          "large placeholder X uses exactly two-pixel line width");
}

void TestAllFailedAndPathValidation() {
    std::array<const videocore::native::GrayImage*, VC_VIDEO_FRAME_COUNT>
        none{};
    const auto directory = UniqueTempDirectory();
    const auto absent = directory / L"全失败.jpg";
    const std::wstring absent_text = absent.native();
    vc::detail::ContactSheetResult result;
    Check(vc::detail::GenerateContactSheet(
              none, 32u,
              reinterpret_cast<const uint16_t*>(absent_text.data()),
              static_cast<uint32_t>(absent_text.size()), &result) ==
              VC_ERR_NO_FRAME,
          "six failed frames return no-frame");
    Check(result.state == VC_ERR_NO_FRAME && result.width == 0 &&
              result.height == 0 && result.tile_width == 0 &&
              result.tile_height == 0 && result.successful_mask == 0u &&
              result.placeholder_mask == 0u &&
              result.canvas.pixels.empty() && FeaturesAreZero(result.features),
          "six failed frames publish no contact payload");
    Check(!std::filesystem::exists(absent),
          "six failed frames do not create a JPEG");

    auto image = ConstantImage(8, 8, 42u);
    std::array<const videocore::native::GrayImage*, VC_VIDEO_FRAME_COUNT>
        frames{};
    frames[0] = &image;
    vc::detail::ContactSheetResult invalid;
    Check(vc::detail::GenerateContactSheet(
              frames, 8u, nullptr, 0u, &invalid) == VC_ERR_INVALID_ARG,
          "empty contact path is rejected");
    std::array<uint16_t, 3> embedded{L'a', 0u, L'b'};
    Check(vc::detail::GenerateContactSheet(
              frames, 8u, embedded.data(),
              static_cast<uint32_t>(embedded.size()), &invalid) ==
              VC_ERR_INVALID_ARG,
          "embedded NUL in explicit UTF-16 path is rejected");

    const auto existing = directory / L"已有.jpg";
    const std::array<uint8_t, 4> sentinel{1u, 2u, 3u, 4u};
    {
        std::ofstream output(existing, std::ios::binary);
        output.write(reinterpret_cast<const char*>(sentinel.data()),
                     sentinel.size());
    }
    const std::wstring existing_text = existing.native();
    Check(vc::detail::GenerateContactSheet(
              frames, 8u,
              reinterpret_cast<const uint16_t*>(existing_text.data()),
              static_cast<uint32_t>(existing_text.size()), &invalid) ==
              VC_ERR_IO,
          "CREATE_NEW semantics reject an existing file");
    Check(ReadBytes(existing) ==
              std::vector<uint8_t>(sentinel.begin(), sentinel.end()),
          "existing file bytes are never overwritten");
    std::filesystem::remove_all(directory);
}

void TestDeterministicJpegAndUnicodePath() {
    std::array<videocore::native::GrayImage, VC_VIDEO_FRAME_COUNT> images;
    std::array<const videocore::native::GrayImage*, VC_VIDEO_FRAME_COUNT>
        frames{};
    for (uint32_t index = 0; index < VC_VIDEO_FRAME_COUNT; ++index) {
        images[index] = ConstantImage(16, 8,
            static_cast<uint8_t>(18u + index * 31u));
        frames[index] = &images[index];
    }
    const auto directory = UniqueTempDirectory();
    const auto first_path = directory / L"第一次-联系表.jpg";
    const auto second_path = directory / L"第二次-联系表.jpg";
    const std::wstring first_text = first_path.native();
    const std::wstring second_text = second_path.native();
    vc::detail::ContactSheetResult first;
    vc::detail::ContactSheetResult second;
    Check(vc::detail::GenerateContactSheet(
              frames, 16u,
              reinterpret_cast<const uint16_t*>(first_text.data()),
              static_cast<uint32_t>(first_text.size()), &first) == VC_OK,
          "non-ASCII UTF-16 temporary path succeeds");
    Check(vc::detail::GenerateContactSheet(
              frames, 16u,
              reinterpret_cast<const uint16_t*>(second_text.data()),
              static_cast<uint32_t>(second_text.size()), &second) == VC_OK,
          "second deterministic generation succeeds");
    const auto first_bytes = ReadBytes(first_path);
    const auto second_bytes = ReadBytes(second_path);
    Check(first.canvas.pixels == second.canvas.pixels &&
              first.features.pdq == second.features.pdq &&
              first.features.quality == second.features.quality,
          "repeated canvas and contact features are identical");
    const std::string canvas_sha = Sha256(first.canvas.pixels);
    std::cout << "CONTACT_CANVAS_SHA256|" << canvas_sha << '\n';
    Check(canvas_sha ==
              "58ed90699d51e6213fd40dad6610d0df387af242fc8bb7c378c8c54120ca0742",
          "fixed fixture canvas SHA-256 matches the reviewed literal");
    Check(!first_bytes.empty() && first_bytes == second_bytes,
          "repeated JPEG bytes are identical");
    const std::string jpeg_sha = Sha256(first_bytes);
    std::cout << "CONTACT_JPEG_SHA256|" << jpeg_sha << '\n';
    Check(jpeg_sha ==
              "32aa02904804d08b94f6dee535d4340d60dbb29580b8f262777afdc4e3e3e8a8",
          "fixed fixture JPEG SHA-256 matches the reviewed literal");

    using TurboHandle = std::unique_ptr<void, decltype(&tjDestroy)>;
    TurboHandle decoder(tjInitDecompress(), &tjDestroy);
    int width = 0;
    int height = 0;
    int subsamp = -1;
    int colorspace = -1;
    Check(decoder != nullptr &&
              tjDecompressHeader3(decoder.get(), first_bytes.data(),
                                  static_cast<unsigned long>(first_bytes.size()),
                                  &width, &height, &subsamp, &colorspace) == 0 &&
              width == first.width && height == first.height &&
              subsamp == TJSAMP_GRAY && colorspace == TJCS_GRAY,
          "JPEG decodes as grayscale with exact canvas dimensions");
    Check(!std::filesystem::exists(first_path.wstring() + L".json") &&
              !std::filesystem::exists(second_path.wstring() + L".json"),
          "VideoCore creates no sidecar files");

    std::ifstream reopen(first_path, std::ios::binary);
    Check(reopen.good(), "JPEG handle is closed before return and can reopen");
    reopen.close();
    Check(std::filesystem::remove(first_path),
          "JPEG can be deleted immediately after return");
    std::filesystem::remove_all(directory);
}

std::wstring FixturePath(const wchar_t* name) {
    return std::wstring(VC_VIDEO_TESTDATA_ROOT) + L"\\" + name;
}

bool OpenHashVideo(const std::wstring& path, vc_media_session** session) {
    vc_media_open_options options{};
    options.struct_size = sizeof(options);
    options.abi_version = VC_ABI_VERSION;
    options.expected_media_type = VC_MEDIA_TYPE_VIDEO;
    vc_error error = FreshError();
    if (vc_media_open_w(reinterpret_cast<const uint16_t*>(path.data()),
                        static_cast<uint32_t>(path.size()), &options,
                        nullptr, session, &error) != VC_OK) {
        return false;
    }
    std::array<uint8_t, VC_SHA512_SIZE> digest{};
    error = FreshError();
    return vc_media_hash(*session, digest.data(), &error) == VC_OK;
}

vc_analysis_request ContactOnlyRequest(const std::wstring& path,
                                       uint32_t tile_max_side = 64u) {
    vc_analysis_request request{};
    request.struct_size = sizeof(request);
    request.abi_version = VC_ABI_VERSION;
    request.feature_mask = VC_FEATURE_CONTACT_SHEET;
    request.frame_mask = VC_ALL_FRAME_MASK;
    request.probe_timeout_ms = 15000u;
    request.frame_timeout_ms = 20000u;
    request.contact_sheet_tile_max_side = tile_max_side;
    request.temporary_jpeg_path =
        reinterpret_cast<const uint16_t*>(path.data());
    request.temporary_jpeg_path_units =
        static_cast<uint32_t>(path.size());
    return request;
}

void DelayAtIoBoundary(vc::detail::IoBoundary boundary, void*) noexcept {
    if (boundary == vc::detail::IoBoundary::before_seek ||
        boundary == vc::detail::IoBoundary::before_read) {
        Sleep(20u);
    }
}

void TestContactOnlyFailureIsTopLevelFailure() {
    const auto directory = UniqueTempDirectory();
    const auto existing_path = directory / L"contact-only-existing.jpg";
    const std::array<uint8_t, 3> sentinel{0x49u, 0x4fu, 0x21u};
    {
        std::ofstream output(existing_path, std::ios::binary);
        output.write(reinterpret_cast<const char*>(sentinel.data()),
                     static_cast<std::streamsize>(sentinel.size()));
    }
    vc_media_session* session = nullptr;
    Check(OpenHashVideo(FixturePath(L"h264-standard.mp4"), &session),
          "contact-only IO fixture opens and hashes");
    if (session != nullptr) {
        const std::wstring existing_text = existing_path.native();
        const vc_analysis_request request = ContactOnlyRequest(existing_text);
        vc_analysis_result result = FreshResult();
        vc_error error = FreshError();
        Check(vc_media_analyze(session, &request, &result, &error) ==
                  VC_ERR_IO &&
                  error.code == VC_ERR_IO &&
                  std::strcmp(error.message_utf8,
                              "contact sheet generation failed") == 0 &&
                  result.contact_sheet_status == VC_ERR_IO &&
                  result.completed_frame_mask == VC_ALL_FRAME_MASK &&
                  ReadBytes(existing_path) ==
                      std::vector<uint8_t>(sentinel.begin(), sentinel.end()),
              "contact-only CREATE_NEW failure is the top-level IO failure");
        vc_media_close(session);
    }

    const auto oversized_path = directory / L"contact-only-too-large.jpg";
    const std::wstring oversized_text = oversized_path.native();
    session = nullptr;
    Check(OpenHashVideo(FixturePath(L"h264-standard.mp4"), &session),
          "contact-only size fixture opens and hashes");
    if (session != nullptr) {
        const vc_analysis_request request = ContactOnlyRequest(
            oversized_text, (std::numeric_limits<uint32_t>::max)());
        vc_analysis_result result = FreshResult();
        vc_error error = FreshError();
        Check(vc_media_analyze(session, &request, &result, &error) ==
                  VC_ERR_OUTPUT_TOO_LARGE &&
                  error.code == VC_ERR_OUTPUT_TOO_LARGE &&
                  std::strcmp(error.message_utf8,
                              "contact sheet generation failed") == 0 &&
                  result.contact_sheet_status == VC_ERR_OUTPUT_TOO_LARGE &&
                  result.completed_frame_mask == VC_ALL_FRAME_MASK &&
                  !std::filesystem::exists(oversized_path),
              "contact-only oversized output is the top-level size failure");
        vc_media_close(session);
    }
    std::filesystem::remove_all(directory);
}

void TestEarlyContactInterruptMapping() {
    const auto directory = UniqueTempDirectory();
    const auto cancelled_path = directory / L"early-cancel.jpg";
    const std::wstring cancelled_text = cancelled_path.native();
    vc_cancel_token* token = nullptr;
    vc_error error = FreshError();
    Check(vc_cancel_create(&token, &error) == VC_OK,
          "early cancellation token is created");
    vc_media_session* session = nullptr;
    vc_media_open_options options{};
    options.struct_size = sizeof(options);
    options.abi_version = VC_ABI_VERSION;
    options.expected_media_type = VC_MEDIA_TYPE_VIDEO;
    const std::wstring fixture = FixturePath(L"h264-standard.mp4");
    error = FreshError();
    Check(vc_media_open_w(reinterpret_cast<const uint16_t*>(fixture.data()),
                          static_cast<uint32_t>(fixture.size()), &options,
                          token, &session, &error) == VC_OK,
          "early cancellation fixture opens");
    if (session != nullptr) {
        std::array<uint8_t, VC_SHA512_SIZE> digest{};
        error = FreshError();
        Check(vc_media_hash(session, digest.data(), &error) == VC_OK,
              "early cancellation fixture hashes");
        vc_cancel_request(token);
        const vc_analysis_request request = ContactOnlyRequest(cancelled_text);
        vc_analysis_result result = FreshResult();
        error = FreshError();
        Check(vc_media_analyze(session, &request, &result, &error) ==
                  VC_ERR_CANCELLED &&
                  result.contact_sheet_status == VC_ERR_CANCELLED &&
                  result.contact_sheet_width == 0u &&
                  result.contact_sheet_height == 0u &&
                  PublicFeaturesAreZero(result.contact_sheet_features) &&
                  !std::filesystem::exists(cancelled_path),
              "pre-cancel maps the contact status before container open");
        vc_media_close(session);
    }
    vc_cancel_free(token);

    const auto timeout_path = directory / L"early-timeout.jpg";
    const std::wstring timeout_text = timeout_path.native();
    session = nullptr;
    Check(OpenHashVideo(fixture, &session),
          "early timeout fixture opens and hashes");
    if (session != nullptr) {
        Check(vc::detail::SetMediaSessionTestIoHook(
                  session, &DelayAtIoBoundary, nullptr),
              "early timeout hook attaches to the session");
        vc_analysis_request request = ContactOnlyRequest(timeout_text);
        request.probe_timeout_ms = 1u;
        vc_analysis_result result = FreshResult();
        error = FreshError();
        Check(vc_media_analyze(session, &request, &result, &error) ==
                  VC_ERR_TIMEOUT &&
                  result.contact_sheet_status == VC_ERR_TIMEOUT &&
                  result.contact_sheet_width == 0u &&
                  result.contact_sheet_height == 0u &&
                  PublicFeaturesAreZero(result.contact_sheet_features) &&
                  !std::filesystem::exists(timeout_path),
              "probe timeout maps the early contact status");
        vc_media_close(session);
    }
    std::filesystem::remove_all(directory);
}

struct CancelAtPublishContext {
    vc_cancel_token* token = nullptr;
};

void CancelAtPublish(uint32_t, void* opaque) noexcept {
    auto* context = static_cast<CancelAtPublishContext*>(opaque);
    if (context != nullptr && context->token != nullptr) {
        vc_cancel_request(context->token);
    }
}

void CancelAfterContactWrite(void* opaque) noexcept {
    auto* context = static_cast<CancelAtPublishContext*>(opaque);
    if (context != nullptr && context->token != nullptr) {
        vc_cancel_request(context->token);
    }
}

void TestPostContactWriteInterruptCleanup() {
    const auto directory = UniqueTempDirectory();
    const std::wstring fixture = FixturePath(L"h264-standard.mp4");
    vc_media_open_options options{};
    options.struct_size = sizeof(options);
    options.abi_version = VC_ABI_VERSION;
    options.expected_media_type = VC_MEDIA_TYPE_VIDEO;

    const auto cancel_path = directory / L"post-write-cancel.jpg";
    const std::wstring cancel_text = cancel_path.native();
    vc_cancel_token* token = nullptr;
    vc_error error = FreshError();
    Check(vc_cancel_create(&token, &error) == VC_OK,
          "post-write cancellation token is created");
    vc_media_session* session = nullptr;
    error = FreshError();
    Check(vc_media_open_w(reinterpret_cast<const uint16_t*>(fixture.data()),
                          static_cast<uint32_t>(fixture.size()), &options,
                          token, &session, &error) == VC_OK,
          "post-write cancellation fixture opens");
    if (session != nullptr) {
        std::array<uint8_t, VC_SHA512_SIZE> digest{};
        error = FreshError();
        Check(vc_media_hash(session, digest.data(), &error) == VC_OK,
              "post-write cancellation fixture hashes");
        const vc_analysis_request request = ContactOnlyRequest(cancel_text);
        vc_analysis_result result = FreshResult();
        CancelAtPublishContext context{token};
        vc::detail::VideoAnalysisTestReset();
        vc::detail::VideoAnalysisTestSetAfterContactWriteHook(
            &CancelAfterContactWrite, &context);
        error = FreshError();
        Check(vc_media_analyze(session, &request, &result, &error) ==
                  VC_ERR_CANCELLED &&
                  result.contact_sheet_status == VC_ERR_CANCELLED &&
                  result.completed_frame_mask == 0u &&
                  !std::filesystem::exists(cancel_path),
              "post-write cancel removes JPEG before publishing interruption");
        vc_media_close(session);
    }
    vc_cancel_free(token);

    const auto timeout_path = directory / L"post-write-timeout.jpg";
    const std::wstring timeout_text = timeout_path.native();
    session = nullptr;
    Check(OpenHashVideo(fixture, &session),
          "post-write timeout fixture opens and hashes");
    if (session != nullptr) {
        const vc_analysis_request request = ContactOnlyRequest(timeout_text);
        vc_analysis_result result = FreshResult();
        vc::detail::VideoAnalysisTestReset();
        vc::detail::VideoAnalysisTestForceTimeoutAfterContactWriteOnce();
        error = FreshError();
        Check(vc_media_analyze(session, &request, &result, &error) ==
                  VC_ERR_TIMEOUT &&
                  result.contact_sheet_status == VC_ERR_TIMEOUT &&
                  result.completed_frame_mask == 0u &&
                  !std::filesystem::exists(timeout_path),
              "post-write timeout removes JPEG before publishing interruption");
        vc_media_close(session);
    }

    const auto delete_failure_path = directory / L"delete-failure.jpg";
    const std::wstring delete_failure_text = delete_failure_path.native();
    token = nullptr;
    error = FreshError();
    Check(vc_cancel_create(&token, &error) == VC_OK,
          "delete-failure cancellation token is created");
    session = nullptr;
    error = FreshError();
    Check(vc_media_open_w(reinterpret_cast<const uint16_t*>(fixture.data()),
                          static_cast<uint32_t>(fixture.size()), &options,
                          token, &session, &error) == VC_OK,
          "delete-failure fixture opens");
    if (session != nullptr) {
        std::array<uint8_t, VC_SHA512_SIZE> digest{};
        error = FreshError();
        Check(vc_media_hash(session, digest.data(), &error) == VC_OK,
              "delete-failure fixture hashes");
        const vc_analysis_request request =
            ContactOnlyRequest(delete_failure_text);
        vc_analysis_result result = FreshResult();
        CancelAtPublishContext context{token};
        vc::detail::VideoAnalysisTestReset();
        vc::detail::VideoAnalysisTestSetAfterContactWriteHook(
            &CancelAfterContactWrite, &context);
        vc::detail::VideoAnalysisTestForceContactDeleteFailureOnce();
        error = FreshError();
        Check(vc_media_analyze(session, &request, &result, &error) ==
                  VC_ERR_IO &&
                  error.code == VC_ERR_IO &&
                  std::strcmp(error.message_utf8,
                              "contact sheet generation failed") == 0 &&
                  result.contact_sheet_status == VC_ERR_IO &&
                  result.completed_frame_mask == 0u &&
                  PublicFeaturesAreZero(result.contact_sheet_features) &&
                  std::filesystem::exists(delete_failure_path),
              "delete failure is explicitly IO and never claims atomic cleanup");
        vc_media_close(session);
    }
    vc_cancel_free(token);
    std::filesystem::remove_all(directory);
}

void TestContactCancellationAndTimeoutAreAtomic() {
    const auto directory = UniqueTempDirectory();
    const auto cancelled_path = directory / L"取消.jpg";
    const std::wstring cancelled_text = cancelled_path.native();
    vc_cancel_token* token = nullptr;
    vc_error error = FreshError();
    Check(vc_cancel_create(&token, &error) == VC_OK,
          "contact cancellation token is created");
    vc_media_session* session = nullptr;
    vc_media_open_options options{};
    options.struct_size = sizeof(options);
    options.abi_version = VC_ABI_VERSION;
    options.expected_media_type = VC_MEDIA_TYPE_VIDEO;
    const std::wstring fixture = FixturePath(L"h264-standard.mp4");
    error = FreshError();
    Check(vc_media_open_w(reinterpret_cast<const uint16_t*>(fixture.data()),
                          static_cast<uint32_t>(fixture.size()), &options,
                          token, &session, &error) == VC_OK,
          "contact cancellation fixture opens");
    if (session != nullptr) {
        std::array<uint8_t, VC_SHA512_SIZE> digest{};
        error = FreshError();
        Check(vc_media_hash(session, digest.data(), &error) == VC_OK,
              "contact cancellation fixture hashes");
        vc_analysis_request request{};
        request.struct_size = sizeof(request);
        request.abi_version = VC_ABI_VERSION;
        request.feature_mask = VC_FEATURE_CONTACT_SHEET | VC_FEATURE_PDQ;
        request.frame_mask = VC_ALL_FRAME_MASK;
        request.probe_timeout_ms = 15000u;
        request.frame_timeout_ms = 20000u;
        request.contact_sheet_tile_max_side = 64u;
        request.temporary_jpeg_path =
            reinterpret_cast<const uint16_t*>(cancelled_text.data());
        request.temporary_jpeg_path_units =
            static_cast<uint32_t>(cancelled_text.size());
        vc_analysis_result result = FreshResult();
        CancelAtPublishContext hook{token};
        vc::detail::VideoAnalysisTestReset();
        vc::detail::VideoAnalysisTestSetBeforePublishHook(
            &CancelAtPublish, &hook);
        error = FreshError();
        Check(vc_media_analyze(session, &request, &result, &error) ==
                  VC_ERR_CANCELLED &&
                  result.contact_sheet_status == VC_ERR_CANCELLED &&
                  result.contact_sheet_width == 0u &&
                  result.contact_sheet_height == 0u &&
                  PublicFeaturesAreZero(result.contact_sheet_features) &&
                  result.completed_frame_mask == 0u &&
                  !std::filesystem::exists(cancelled_path),
              "cancel clears contact and frame payload and creates no JPEG");
        vc_media_close(session);
    }
    vc_cancel_free(token);

    const auto timeout_path = directory / L"超时.jpg";
    const std::wstring timeout_text = timeout_path.native();
    session = nullptr;
    Check(OpenHashVideo(fixture, &session),
          "contact timeout fixture opens and hashes");
    if (session != nullptr) {
        vc_analysis_request request{};
        request.struct_size = sizeof(request);
        request.abi_version = VC_ABI_VERSION;
        request.feature_mask = VC_FEATURE_CONTACT_SHEET | VC_FEATURE_PDQ;
        request.frame_mask = VC_ALL_FRAME_MASK;
        request.probe_timeout_ms = 15000u;
        request.frame_timeout_ms = 20000u;
        request.contact_sheet_tile_max_side = 64u;
        request.temporary_jpeg_path =
            reinterpret_cast<const uint16_t*>(timeout_text.data());
        request.temporary_jpeg_path_units =
            static_cast<uint32_t>(timeout_text.size());
        vc_analysis_result result = FreshResult();
        vc::detail::VideoAnalysisTestReset();
        vc::detail::VideoAnalysisTestForceTimeoutBeforePublishOnce();
        error = FreshError();
        Check(vc_media_analyze(session, &request, &result, &error) ==
                  VC_ERR_TIMEOUT &&
                  result.contact_sheet_status == VC_ERR_TIMEOUT &&
                  result.contact_sheet_width == 0u &&
                  result.contact_sheet_height == 0u &&
                  PublicFeaturesAreZero(result.contact_sheet_features) &&
                  result.completed_frame_mask == 0u &&
                  !std::filesystem::exists(timeout_path),
              "timeout clears contact and frame payload and creates no JPEG");
        vc_media_close(session);
    }
    std::filesystem::remove_all(directory);
}

void TestIntegratedSingleSessionAndRequestMasks() {
    const auto directory = UniqueTempDirectory();
    const auto contact_path = directory / L"集成-部分mask.jpg";
    const std::wstring contact_text = contact_path.native();
    vc_media_session* session = nullptr;
    Check(OpenHashVideo(FixturePath(L"h264-standard.mp4"), &session),
          "integration fixture opens and hashes");
    if (session == nullptr) return;
    vc_analysis_request request{};
    request.struct_size = sizeof(request);
    request.abi_version = VC_ABI_VERSION;
    request.feature_mask = VC_FEATURE_CONTACT_SHEET;
    request.frame_mask = 0x12u;
    request.probe_timeout_ms = 15000u;
    request.frame_timeout_ms = 20000u;
    request.contact_sheet_tile_max_side = 64u;
    request.temporary_jpeg_path =
        reinterpret_cast<const uint16_t*>(contact_text.data());
    request.temporary_jpeg_path_units =
        static_cast<uint32_t>(contact_text.size());
    vc_analysis_result result = FreshResult();
    vc_error error = FreshError();
    vc::detail::VideoAnalysisTestReset();
    const int32_t status = vc_media_analyze(session, &request, &result, &error);
    const auto stats = vc::detail::VideoAnalysisTestGetStats();
    vc::detail::MediaSessionTestSnapshot snapshot{};
    Check(vc::detail::GetMediaSessionTestSnapshot(session, &snapshot),
          "integration session exposes test snapshot");
    Check(status == VC_OK && result.contact_sheet_status == VC_OK &&
              result.contact_sheet_width == 192u &&
              result.contact_sheet_height == 72u &&
              std::filesystem::exists(contact_path),
          "contact-only request produces the 3x2 JPEG");
    Check(stats.format_contexts == 1u && stats.codec_contexts == 1u &&
              snapshot.io.create_file_calls == 1u &&
              stats.attempted_frame_mask == VC_ALL_FRAME_MASK,
          "contact reuses one HANDLE, AVIO, format and codec while trying six slots");
    Check(std::all_of(stats.gray_conversion_counts.begin(),
                      stats.gray_conversion_counts.end(),
                      [](uint32_t count) { return count == 1u; }),
          "each successful contact slot performs one gray conversion");
    Check(result.completed_frame_mask == 0x12u,
          "public completed mask follows the partial frame mask");
    for (uint32_t index = 0u; index < VC_VIDEO_FRAME_COUNT; ++index) {
        const bool requested = (0x12u & (1u << index)) != 0u;
        Check(result.frames[index].status ==
                  (requested ? VC_OK : VC_ERR_UNSUPPORTED),
              "contact implicit slots do not publish unrequested frame status");
        Check(PublicFeaturesAreZero(result.frames[index].features),
              "contact-only request leaves all per-frame feature families zero");
    }
    Check(!PublicFeaturesAreZero(result.contact_sheet_features) &&
              std::all_of(std::begin(result.contact_sheet_features.phash),
                          std::end(result.contact_sheet_features.phash),
                          [](uint64_t value) { return value == 0u; }) &&
              std::all_of(
                  std::begin(result.contact_sheet_features.sobel_histogram),
                  std::end(result.contact_sheet_features.sobel_histogram),
                  [](float value) { return value == 0.0f; }),
          "contact computes PDQ/quality only, with pHash and Sobel zero");
    vc_media_close(session);

    const auto existing_path = directory / L"已存在-不覆盖.jpg";
    const std::array<uint8_t, 4> sentinel{0x41u, 0x42u, 0x43u, 0x44u};
    {
        std::ofstream output(existing_path, std::ios::binary);
        output.write(reinterpret_cast<const char*>(sentinel.data()),
                     static_cast<std::streamsize>(sentinel.size()));
    }
    const std::wstring existing_text = existing_path.native();
    session = nullptr;
    Check(OpenHashVideo(FixturePath(L"h264-standard.mp4"), &session),
          "JPEG write-failure fixture opens and hashes");
    if (session != nullptr) {
        request.feature_mask = VC_FEATURE_CONTACT_SHEET | VC_FEATURE_PDQ |
                               VC_FEATURE_PHASH | VC_FEATURE_SOBEL;
        request.frame_mask = VC_ALL_FRAME_MASK;
        request.temporary_jpeg_path =
            reinterpret_cast<const uint16_t*>(existing_text.data());
        request.temporary_jpeg_path_units =
            static_cast<uint32_t>(existing_text.size());
        result = FreshResult();
        error = FreshError();
        Check(vc_media_analyze(session, &request, &result, &error) == VC_OK &&
                  result.completed_frame_mask == VC_ALL_FRAME_MASK &&
                  result.contact_sheet_status == VC_ERR_IO &&
                  result.contact_sheet_width == 0u &&
                  result.contact_sheet_height == 0u &&
                  PublicFeaturesAreZero(result.contact_sheet_features) &&
                  ReadBytes(existing_path) ==
                      std::vector<uint8_t>(sentinel.begin(), sentinel.end()),
              "ordinary JPEG write failure retains frame successes and never overwrites");
        vc_media_close(session);
    }

    const auto partial_path = directory / L"短视频.jpg";
    const std::wstring partial_text = partial_path.native();
    session = nullptr;
    Check(OpenHashVideo(FixturePath(L"h264-short.mp4"), &session),
          "partial fixture opens and hashes");
    if (session != nullptr) {
        request.frame_mask = VC_ALL_FRAME_MASK;
        request.temporary_jpeg_path =
            reinterpret_cast<const uint16_t*>(partial_text.data());
        request.temporary_jpeg_path_units =
            static_cast<uint32_t>(partial_text.size());
        result = FreshResult();
        error = FreshError();
        Check(vc_media_analyze(session, &request, &result, &error) == VC_OK &&
                  result.completed_frame_mask == 0x1fu &&
                  result.contact_sheet_status == VC_OK &&
                  std::filesystem::exists(partial_path),
              "partial frame failure keeps successful frames and writes placeholder JPEG");
        vc_media_close(session);
    }

    const auto all_failed_path = directory / L"纯音频.jpg";
    const std::wstring all_failed_text = all_failed_path.native();
    session = nullptr;
    Check(OpenHashVideo(FixturePath(L"audio-only.m4a"), &session),
          "audio-only fixture opens and hashes as requested video");
    if (session != nullptr) {
        request.temporary_jpeg_path =
            reinterpret_cast<const uint16_t*>(all_failed_text.data());
        request.temporary_jpeg_path_units =
            static_cast<uint32_t>(all_failed_text.size());
        result = FreshResult();
        error = FreshError();
        Check(vc_media_analyze(session, &request, &result, &error) ==
                  VC_ERR_NO_FRAME &&
                  result.contact_sheet_status == VC_ERR_NO_FRAME &&
                  !std::filesystem::exists(all_failed_path),
              "six failed slots publish no contact payload or JPEG");
        vc_media_close(session);
    }

    const auto ignored_path = directory / L"未请求.jpg";
    const std::wstring ignored_text = ignored_path.native();
    session = nullptr;
    Check(OpenHashVideo(FixturePath(L"h264-standard.mp4"), &session),
          "no-contact regression fixture opens and hashes");
    if (session != nullptr) {
        request.feature_mask = VC_FEATURE_DURATION | VC_FEATURE_PDQ |
                               VC_FEATURE_PHASH | VC_FEATURE_SOBEL;
        request.frame_mask = VC_ALL_FRAME_MASK;
        request.temporary_jpeg_path =
            reinterpret_cast<const uint16_t*>(ignored_text.data());
        request.temporary_jpeg_path_units =
            static_cast<uint32_t>(ignored_text.size());
        result = FreshResult();
        error = FreshError();
        Check(vc_media_analyze(session, &request, &result, &error) == VC_OK &&
                  result.completed_frame_mask == VC_ALL_FRAME_MASK &&
                  result.contact_sheet_status == VC_ERR_UNSUPPORTED &&
                  result.contact_sheet_width == 0u &&
                  result.contact_sheet_height == 0u &&
                  PublicFeaturesAreZero(result.contact_sheet_features) &&
                  !std::filesystem::exists(ignored_path),
              "unrequested contact leaves Task 8 payload and filesystem unchanged");
        vc_media_close(session);
    }
    std::filesystem::remove_all(directory);
}

void TestHiddenContactSlotsSkipPerFrameFeatures() {
    const auto directory = UniqueTempDirectory();
    const auto path = directory / L"hidden-feature-counts.jpg";
    const std::wstring path_text = path.native();
    vc_media_session* session = nullptr;
    Check(OpenHashVideo(FixturePath(L"h264-standard.mp4"), &session),
          "hidden feature-count fixture opens and hashes");
    if (session != nullptr) {
        vc_analysis_request request = ContactOnlyRequest(path_text);
        request.feature_mask = VC_FEATURE_CONTACT_SHEET | VC_FEATURE_PDQ |
                               VC_FEATURE_PHASH | VC_FEATURE_SOBEL;
        request.frame_mask = 0x01u;
        vc_analysis_result result = FreshResult();
        vc_error error = FreshError();
        vc::detail::VideoAnalysisTestReset();
        Check(vc_media_analyze(session, &request, &result, &error) == VC_OK,
              "hidden feature-count analysis succeeds");
        const auto stats = vc::detail::VideoAnalysisTestGetStats();
        Check(stats.attempted_frame_mask == VC_ALL_FRAME_MASK &&
                  stats.pdq_compute_counts[0] == 1u &&
                  stats.phash_compute_counts[0] == 1u &&
                  stats.sobel_compute_counts[0] == 1u,
              "the one public slot computes all requested feature families");
        for (uint32_t index = 1u; index < VC_VIDEO_FRAME_COUNT; ++index) {
            Check(stats.pdq_compute_counts[index] == 0u &&
                      stats.phash_compute_counts[index] == 0u &&
                      stats.sobel_compute_counts[index] == 0u,
                  "implicit contact slots decode gray but compute no per-frame features");
        }
        vc_media_close(session);
    }
    std::filesystem::remove_all(directory);
}

}  // namespace

int main() {
    TestRowMajorLayout();
    TestTileDimensions();
    TestContinuousPlaceholderRaster();
    TestExtremeAspectUsesFeatureOnlyMinimum();
    TestContactSourceStrideAndBufferBounds();
    TestPartialAndPlaceholderPixels();
    TestAllFailedAndPathValidation();
    TestDeterministicJpegAndUnicodePath();
    TestContactOnlyFailureIsTopLevelFailure();
    TestEarlyContactInterruptMapping();
    TestPostContactWriteInterruptCleanup();
    TestIntegratedSingleSessionAndRequestMasks();
    TestHiddenContactSlotsSkipPerFrameFeatures();
    TestContactCancellationAndTimeoutAreAtomic();
    if (failures != 0) {
        std::cerr << failures << " contact sheet test(s) failed\n";
        return 1;
    }
    std::cout << "contact sheet tests passed\n";
    return 0;
}
