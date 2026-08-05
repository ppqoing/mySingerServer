#include <algorithm>
#include <array>
#include <atomic>
#include <cstdlib>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <sstream>
#include <string>
#include <new>
#include <vector>

#if defined(VC_IMAGE_ALGORITHMS_PRESENT)
#include "native_algorithms/gray_image.h"
#include "native_algorithms/image_decode.h"
#include "native_algorithms/pdq.h"
#include "native_algorithms/phash_parts.h"
#include "native_algorithms/sobel_hist.h"
#endif

namespace {
std::atomic<size_t> g_test_fail_allocation_size{0};
}

void* operator new(std::size_t size) {
    size_t expected = size;
    if (size != 0 && g_test_fail_allocation_size.compare_exchange_strong(
            expected, 0, std::memory_order_acq_rel)) {
        throw std::bad_alloc();
    }
    if (void* memory = std::malloc(size == 0 ? 1 : size)) {
        return memory;
    }
    throw std::bad_alloc();
}

void operator delete(void* memory) noexcept {
    std::free(memory);
}

void operator delete(void* memory, std::size_t) noexcept {
    std::free(memory);
}

namespace {

int failures = 0;

void fail(const std::string& test, const std::string& detail) {
    std::fprintf(stderr, "FAIL %s: %s\n", test.c_str(), detail.c_str());
    ++failures;
}

void expect(bool condition, const std::string& test, const std::string& detail) {
    if (!condition) {
        fail(test, detail);
    }
}

std::vector<uint8_t> read_bytes(const std::filesystem::path& path) {
    std::ifstream input(path, std::ios::binary);
    if (!input) {
        return {};
    }
    input.seekg(0, std::ios::end);
    const std::streamoff length = input.tellg();
    if (length < 0) {
        return {};
    }
    input.seekg(0, std::ios::beg);
    std::vector<uint8_t> bytes(static_cast<size_t>(length));
    if (!bytes.empty()) {
        input.read(reinterpret_cast<char*>(bytes.data()), length);
    }
    return input ? bytes : std::vector<uint8_t>{};
}

std::string read_text(const std::filesystem::path& path) {
    std::ifstream input(path, std::ios::binary);
    std::ostringstream text;
    text << input.rdbuf();
    return input ? text.str() : std::string{};
}

int hex_digit(char value) {
    if (value >= '0' && value <= '9') return value - '0';
    if (value >= 'a' && value <= 'f') return value - 'a' + 10;
    if (value >= 'A' && value <= 'F') return value - 'A' + 10;
    return -1;
}

std::vector<uint8_t> parse_hex_bytes(const std::string& text) {
    std::vector<uint8_t> bytes;
    if ((text.size() & 1u) != 0u) return bytes;
    bytes.reserve(text.size() / 2u);
    for (size_t i = 0; i < text.size(); i += 2) {
        const int high = hex_digit(text[i]);
        const int low = hex_digit(text[i + 1]);
        if (high < 0 || low < 0) return {};
        bytes.push_back(static_cast<uint8_t>((high << 4) | low));
    }
    return bytes;
}

uint64_t parse_hex_u64(const std::string& text) {
    uint64_t value = 0;
    for (char digit : text) {
        const int parsed = hex_digit(digit);
        if (parsed < 0) return 0;
        value = (value << 4) | static_cast<uint64_t>(parsed);
    }
    return value;
}

uint32_t parse_hex_u32(const std::string& text) {
    return static_cast<uint32_t>(parse_hex_u64(text));
}

size_t matching_delimiter(
    const std::string& text,
    size_t open,
    char open_char,
    char close_char) {
    int depth = 0;
    bool in_string = false;
    bool escaped = false;
    for (size_t i = open; i < text.size(); ++i) {
        const char value = text[i];
        if (in_string) {
            if (escaped) {
                escaped = false;
            } else if (value == '\\') {
                escaped = true;
            } else if (value == '"') {
                in_string = false;
            }
            continue;
        }
        if (value == '"') {
            in_string = true;
        } else if (value == open_char) {
            ++depth;
        } else if (value == close_char && --depth == 0) {
            return i;
        }
    }
    return std::string::npos;
}

std::string json_fixture_image(
    const std::string& golden,
    const std::string& fixture_path) {
    const std::string marker = "\"path\": \"" + fixture_path + "\"";
    const size_t fixture = golden.find(marker);
    if (fixture == std::string::npos) return {};
    const size_t image_key = golden.find("\"image\"", fixture);
    if (image_key == std::string::npos) return {};
    const size_t open = golden.find('{', image_key);
    const size_t close = matching_delimiter(golden, open, '{', '}');
    if (open == std::string::npos || close == std::string::npos) return {};
    return golden.substr(open, close - open + 1);
}

std::string json_string(const std::string& object, const std::string& key) {
    const size_t key_at = object.find("\"" + key + "\"");
    if (key_at == std::string::npos) return {};
    const size_t colon = object.find(':', key_at);
    const size_t quote = object.find('"', colon);
    const size_t end = object.find('"', quote + 1);
    if (colon == std::string::npos || quote == std::string::npos ||
        end == std::string::npos) return {};
    return object.substr(quote + 1, end - quote - 1);
}

int json_int(const std::string& object, const std::string& key) {
    const size_t key_at = object.find("\"" + key + "\"");
    if (key_at == std::string::npos) return 0;
    const size_t colon = object.find(':', key_at);
    const size_t start = object.find_first_of("-0123456789", colon);
    if (start == std::string::npos) return 0;
    return std::stoi(object.substr(start));
}

std::vector<std::string> json_string_array(
    const std::string& object,
    const std::string& key) {
    std::vector<std::string> values;
    const size_t key_at = object.find("\"" + key + "\"");
    const size_t open = object.find('[', key_at);
    const size_t close = matching_delimiter(object, open, '[', ']');
    if (key_at == std::string::npos || open == std::string::npos ||
        close == std::string::npos) return values;
    size_t cursor = open + 1;
    while (cursor < close) {
        const size_t quote = object.find('"', cursor);
        if (quote == std::string::npos || quote >= close) break;
        const size_t end = object.find('"', quote + 1);
        if (end == std::string::npos || end > close) return {};
        values.push_back(object.substr(quote + 1, end - quote - 1));
        cursor = end + 1;
    }
    return values;
}

#if defined(VC_IMAGE_ALGORITHMS_PRESENT)

using videocore::native::GrayImage;
using videocore::native::ImageStatus;

bool is_zero(const GrayImage& image) {
    return image.width == 0 && image.height == 0 && image.stride == 0 &&
        image.pixels.empty();
}

std::vector<uint8_t> make_fallback_rgb(int width, int height) {
    std::vector<uint8_t> rgb(static_cast<size_t>(width) * height * 3u);
    for (int y = 0; y < height; ++y) {
        for (int x = 0; x < width; ++x) {
            const size_t at = (static_cast<size_t>(y) * width + x) * 3u;
            rgb[at] = static_cast<uint8_t>(x * 17 + y);
            rgb[at + 1] = static_cast<uint8_t>(y * 19 + x);
            rgb[at + 2] = static_cast<uint8_t>((x + y) * 11);
        }
    }
    return rgb;
}

std::vector<uint8_t> make_tga(int width, int height) {
    const std::vector<uint8_t> rgb = make_fallback_rgb(width, height);
    std::vector<uint8_t> encoded(18, 0);
    encoded[2] = 2;
    encoded[12] = static_cast<uint8_t>(width);
    encoded[13] = static_cast<uint8_t>(width >> 8);
    encoded[14] = static_cast<uint8_t>(height);
    encoded[15] = static_cast<uint8_t>(height >> 8);
    encoded[16] = 24;
    encoded[17] = 0x20;
    for (size_t at = 0; at < rgb.size(); at += 3) {
        encoded.push_back(rgb[at + 2]);
        encoded.push_back(rgb[at + 1]);
        encoded.push_back(rgb[at]);
    }
    return encoded;
}

std::vector<uint8_t> make_pnm(int width, int height) {
    const std::string header = "P6\n" + std::to_string(width) + " " +
        std::to_string(height) + "\n255\n";
    std::vector<uint8_t> encoded(header.begin(), header.end());
    const std::vector<uint8_t> rgb = make_fallback_rgb(width, height);
    encoded.insert(encoded.end(), rgb.begin(), rgb.end());
    return encoded;
}

void expect_decode_status_and_empty(
    const std::string& test,
    const std::vector<uint8_t>& encoded,
    ImageStatus expected) {
    GrayImage image{17, 19, 23, std::vector<uint8_t>{0xa5}};
    const ImageStatus actual = videocore::native::DecodeImage(
        encoded.data(), encoded.size(), &image);
    expect(actual == expected, test, "unexpected decode status");
    expect(is_zero(image), test, "failed decode did not clear GrayImage");
}

void check_strict_tga_pnm_validation() {
    for (const auto& entry : std::array<std::pair<const char*, std::vector<uint8_t>>, 2>{
             std::pair<const char*, std::vector<uint8_t>>{"TGA", make_tga(8, 8)},
             std::pair<const char*, std::vector<uint8_t>>{"PNM", make_pnm(8, 8)},
         }) {
        GrayImage image{};
        expect(
            videocore::native::DecodeImage(
                entry.second.data(), entry.second.size(), &image) ==
                ImageStatus::ok,
            std::string("strict/") + entry.first + "/valid",
            "valid fallback image rejected");
        expect(
            image.width == 8 && image.height == 8 && image.stride == 8,
            std::string("strict/") + entry.first + "/valid",
            "valid fallback dimensions mismatch");

        std::vector<uint8_t> truncated = entry.second;
        truncated.pop_back();
        expect_decode_status_and_empty(
            std::string("strict/") + entry.first + "/truncated",
            truncated,
            ImageStatus::decode_error);
    }

    expect_decode_status_and_empty(
        "strict/PNM/too_small",
        make_pnm(7, 8),
        ImageStatus::size_error);

    const std::string huge_pnm_header = "P6\n20001 20001\n255\n";
    expect_decode_status_and_empty(
        "strict/PNM/huge_declared_dimensions",
        std::vector<uint8_t>(huge_pnm_header.begin(), huge_pnm_header.end()),
        ImageStatus::size_error);

    std::vector<uint8_t> huge_tga(18, 0);
    huge_tga[2] = 2;
    huge_tga[12] = 0xff;
    huge_tga[13] = 0xff;
    huge_tga[14] = 0xff;
    huge_tga[15] = 0xff;
    huge_tga[16] = 24;
    expect_decode_status_and_empty(
        "strict/TGA/huge_declared_dimensions",
        huge_tga,
        ImageStatus::size_error);
}

void check_gray_allocation_failure_is_test_local(
    const std::filesystem::path& level_b_root) {
    struct Case {
        const char* name;
        std::vector<uint8_t> encoded;
    };
    const std::array<Case, 3> cases{{
        {"jpeg", read_bytes(level_b_root / "images" / "jpg_3.jpg")},
        {"png", read_bytes(level_b_root / "images" / "png_3.png")},
        {"tga", make_tga(8, 8)},
    }};
    for (const Case& test_case : cases) {
        const std::string test = std::string("allocation_failure/") +
            test_case.name;
        expect(!test_case.encoded.empty(), test, "fixture missing");
        GrayImage baseline{};
        expect(videocore::native::DecodeImage(
                   test_case.encoded.data(), test_case.encoded.size(),
                   &baseline) == ImageStatus::ok,
               test,
               "baseline decode failed");
        const size_t gray_bytes = static_cast<size_t>(baseline.width) *
            baseline.height;
        expect(gray_bytes == baseline.pixels.size(),
               test,
               "baseline gray allocation size mismatch");

        for (int attempt = 0; attempt < 2; ++attempt) {
            GrayImage image{7, 9, 11, std::vector<uint8_t>{0xa5}};
            g_test_fail_allocation_size.store(
                gray_bytes, std::memory_order_release);
            const ImageStatus status = videocore::native::DecodeImage(
                test_case.encoded.data(), test_case.encoded.size(), &image);
            expect(status == ImageStatus::out_of_memory,
                   test,
                   "gray allocation failure was not propagated");
            expect(is_zero(image), test, "failed decode did not clear GrayImage");
            expect(g_test_fail_allocation_size.load(std::memory_order_acquire) == 0,
                   test,
                   "test allocator did not fail the exact gray allocation");
        }

        GrayImage recovered{};
        expect(videocore::native::DecodeImage(
                   test_case.encoded.data(), test_case.encoded.size(),
                   &recovered) == ImageStatus::ok,
               test,
               "decode did not recover after repeated OOM");
        expect(recovered.width == baseline.width &&
                   recovered.height == baseline.height &&
                   recovered.stride == baseline.stride &&
                   recovered.pixels == baseline.pixels,
               test,
               "post-OOM decode changed output");
    }
}

void check_padded_stride_matches_tight_and_preserves_sentinels() {
    constexpr int width = 63;
    constexpr int height = 65;
    constexpr int padded_stride = 80;
    constexpr uint8_t sentinel = 0xd3;
    GrayImage tight{width, height, width, {}};
    tight.pixels.resize(static_cast<size_t>(width) * height);
    GrayImage padded{width, height, padded_stride, {}};
    padded.pixels.assign(static_cast<size_t>(padded_stride) * height, sentinel);
    for (int y = 0; y < height; ++y) {
        for (int x = 0; x < width; ++x) {
            const uint8_t value = static_cast<uint8_t>(
                (x * 37 + y * 53 + (x * y) % 251) & 0xff);
            tight.pixels[static_cast<size_t>(y) * width + x] = value;
            padded.pixels[static_cast<size_t>(y) * padded_stride + x] = value;
        }
    }
    const std::vector<uint8_t> padded_before = padded.pixels;

    std::array<uint8_t, 32> tight_pdq{};
    std::array<uint8_t, 32> padded_pdq{};
    int32_t tight_quality = 0;
    int32_t padded_quality = 0;
    expect(videocore::native::ComputePdq(
               tight, &tight_pdq, &tight_quality) == ImageStatus::ok,
           "stride/pdq",
           "tight PDQ failed");
    expect(videocore::native::ComputePdq(
               padded, &padded_pdq, &padded_quality) == ImageStatus::ok,
           "stride/pdq",
           "padded PDQ failed");
    expect(tight_pdq == padded_pdq && tight_quality == padded_quality,
           "stride/pdq",
           "padded PDQ differs from tight bytes/quality");

    std::array<uint64_t, 9> tight_parts{};
    std::array<uint64_t, 9> padded_parts{};
    expect(videocore::native::ComputePHashParts(tight, &tight_parts) ==
               ImageStatus::ok,
           "stride/phash",
           "tight pHash failed");
    expect(videocore::native::ComputePHashParts(padded, &padded_parts) ==
               ImageStatus::ok,
           "stride/phash",
           "padded pHash failed");
    expect(tight_parts == padded_parts,
           "stride/phash",
           "padded pHash differs bit-for-bit from tight");

    std::array<float, 128> tight_sobel{};
    std::array<float, 128> padded_sobel{};
    expect(videocore::native::ComputeSobelHistogram(tight, &tight_sobel) ==
               ImageStatus::ok,
           "stride/sobel",
           "tight Sobel failed");
    expect(videocore::native::ComputeSobelHistogram(padded, &padded_sobel) ==
               ImageStatus::ok,
           "stride/sobel",
           "padded Sobel failed");
    expect(std::memcmp(
               tight_sobel.data(), padded_sobel.data(), sizeof(tight_sobel)) == 0,
           "stride/sobel",
           "padded Sobel differs in raw float bits from tight");
    expect(padded.pixels == padded_before,
           "stride/sentinel",
           "algorithm modified source pixels or padding sentinels");
}

void check_frozen_image(
    const std::filesystem::path& compat_root,
    const std::string& golden,
    const std::string& relative_path) {
    const std::string test = "frozen/" + relative_path;
    const std::string expected = json_fixture_image(golden, relative_path);
    expect(!expected.empty(), test, "missing image golden object");
    const std::vector<uint8_t> encoded = read_bytes(compat_root / relative_path);
    expect(!encoded.empty(), test, "fixture missing or empty");

    GrayImage image{};
    const ImageStatus decode = videocore::native::DecodeImage(
        encoded.data(), encoded.size(), &image);
    expect(decode == ImageStatus::ok, test, "decode failed");
    if (decode != ImageStatus::ok) return;
    expect(image.width == json_int(expected, "width"), test, "width mismatch");
    expect(image.height == json_int(expected, "height"), test, "height mismatch");
    expect(image.stride == image.width, test, "decoded stride must be tight");

    std::array<uint8_t, 32> pdq{};
    int32_t quality = 0;
    expect(
        videocore::native::ComputePdq(image, &pdq, &quality) == ImageStatus::ok,
        test,
        "PDQ failed");
    const std::vector<uint8_t> expected_pdq =
        parse_hex_bytes(json_string(expected, "pdqHex"));
    expect(expected_pdq.size() == pdq.size(), test, "PDQ golden size");
    if (expected_pdq.size() == pdq.size() &&
        std::memcmp(pdq.data(), expected_pdq.data(), pdq.size()) != 0) {
        fail(test, "PDQ byte mismatch");
    }
    expect(quality == json_int(expected, "quality"), test, "quality mismatch");

    std::array<uint64_t, 9> parts{};
    expect(
        videocore::native::ComputePHashParts(image, &parts) == ImageStatus::ok,
        test,
        "pHash failed");
    const std::vector<std::string> expected_parts =
        json_string_array(expected, "pHashPartsHex");
    expect(expected_parts.size() == parts.size(), test, "pHash golden size");
    for (size_t i = 0; i < parts.size() && i < expected_parts.size(); ++i) {
        if (parts[i] != parse_hex_u64(expected_parts[i])) {
            fail(test, "pHash row-major part mismatch at index " +
                std::to_string(i));
        }
    }

    std::array<float, 128> sobel{};
    expect(
        videocore::native::ComputeSobelHistogram(image, &sobel) ==
            ImageStatus::ok,
        test,
        "Sobel failed");
    const std::vector<std::string> expected_bits =
        json_string_array(expected, "sobelFloatBitsHex");
    expect(expected_bits.size() == sobel.size(), test, "Sobel golden size");
    for (size_t i = 0; i < sobel.size() && i < expected_bits.size(); ++i) {
        uint32_t actual_bits = 0;
        std::memcpy(&actual_bits, &sobel[i], sizeof(actual_bits));
        if (actual_bits != parse_hex_u32(expected_bits[i])) {
            fail(test, "Sobel raw float mismatch at index " +
                std::to_string(i));
        }
    }
}

void check_level_b(
    const std::filesystem::path& root,
    const std::filesystem::path& legacy_golden_path) {
    const std::string test = "level_b/valid";
    std::ifstream rows(legacy_golden_path);
    expect(static_cast<bool>(rows), test, "missing frozen legacy Level B golden");
    std::string line;
    int checked = 0;
    while (std::getline(rows, line)) {
        std::istringstream fields(line);
        std::string filename;
        std::string pdq_hex;
        int quality = -1;
        int width = 0;
        int height = 0;
        if (!std::getline(fields, filename, '\t') ||
            !std::getline(fields, pdq_hex, '\t') ||
            !(fields >> quality) || fields.get() != '\t' ||
            !(fields >> width) || fields.get() != '\t' ||
            !(fields >> height)) {
            fail(test, "malformed frozen golden row");
            continue;
        }
        const std::vector<uint8_t> encoded = read_bytes(root / "images" / filename);
        GrayImage image{};
        std::array<uint8_t, 32> pdq{};
        int32_t actual_quality = 0;
        const ImageStatus decoded = videocore::native::DecodeImage(
            encoded.data(), encoded.size(), &image);
        expect(decoded == ImageStatus::ok, test, "decode failed: " + filename);
        if (decoded != ImageStatus::ok) continue;
        expect(
            videocore::native::ComputePdq(image, &pdq, &actual_quality) ==
                ImageStatus::ok,
            test,
            "PDQ failed: " + filename);
        const std::vector<uint8_t> expected = parse_hex_bytes(pdq_hex);
        if (expected.size() != pdq.size() ||
            std::memcmp(expected.data(), pdq.data(), pdq.size()) != 0) {
            fail(test, "PDQ mismatch: " + filename);
        }
        expect(actual_quality == quality, test, "quality mismatch: " + filename);
        expect(image.width == width && image.height == height,
               test,
               "dimensions mismatch: " + filename);
        ++checked;
    }
    expect(checked == 20, test, "expected exactly 20 valid images");

    const std::vector<uint8_t> wrong_extension = read_bytes(root / "wrongext.png");
    GrayImage wrong_image{};
    expect(
        videocore::native::DecodeImage(
            wrong_extension.data(), wrong_extension.size(), &wrong_image) ==
            ImageStatus::ok,
        "level_b/wrong_extension",
        "content sniffing must ignore extension");

    int corrupt_checked = 0;
    for (const auto& entry : std::filesystem::directory_iterator(root / "corrupt")) {
        if (!entry.is_regular_file()) continue;
        const std::vector<uint8_t> corrupt = read_bytes(entry.path());
        GrayImage seeded{7, 9, 11, std::vector<uint8_t>{1, 2, 3}};
        const ImageStatus status = videocore::native::DecodeImage(
            corrupt.empty() ? nullptr : corrupt.data(), corrupt.size(), &seeded);
        expect(status != ImageStatus::ok,
               "level_b/corrupt",
               "corrupt input accepted: " + entry.path().filename().string());
        expect(is_zero(seeded),
               "level_b/corrupt",
               "decode output not cleared: " + entry.path().filename().string());
        ++corrupt_checked;
    }
    expect(corrupt_checked == 14, "level_b/corrupt", "expected 14 corrupt files");
}

void check_luma_case(
    const std::filesystem::path& path,
    const std::string& expected_hex,
    int32_t expected_quality) {
    const std::string test = "luma/" + path.filename().string();
    const std::vector<uint8_t> bytes = read_bytes(path);
    expect(bytes.size() >= 8, test, "missing or truncated luma case");
    if (bytes.size() < 8) return;
    int32_t width = 0;
    int32_t height = 0;
    std::memcpy(&width, bytes.data(), sizeof(width));
    std::memcpy(&height, bytes.data() + sizeof(width), sizeof(height));
    const size_t pixels = static_cast<size_t>(width) * static_cast<size_t>(height);
    expect(bytes.size() == 8 + pixels, test, "luma byte count mismatch");
    if (bytes.size() != 8 + pixels) return;
    GrayImage image{width, height, width, {}};
    image.pixels.assign(bytes.begin() + 8, bytes.end());
    std::array<uint8_t, 32> pdq{};
    int32_t quality = 0;
    expect(
        videocore::native::ComputePdq(image, &pdq, &quality) == ImageStatus::ok,
        test,
        "PDQ failed");
    const std::vector<uint8_t> expected = parse_hex_bytes(expected_hex);
    if (expected.size() != pdq.size() ||
        std::memcmp(expected.data(), pdq.data(), pdq.size()) != 0) {
        fail(test, "PDQ byte mismatch");
    }
    expect(quality == expected_quality, test, "quality mismatch");
}

void check_invalid_algorithm_outputs() {
    const GrayImage invalid{};
    std::array<uint8_t, 32> pdq;
    pdq.fill(0xa5);
    int32_t quality = 123;
    expect(
        videocore::native::ComputePdq(invalid, &pdq, &quality) != ImageStatus::ok,
        "invalid/pdq",
        "invalid image accepted");
    expect(
        std::all_of(pdq.begin(), pdq.end(), [](uint8_t v) { return v == 0; }) &&
            quality == 0,
        "invalid/pdq",
        "failed PDQ did not clear outputs");

    std::array<uint64_t, 9> parts;
    parts.fill(UINT64_MAX);
    expect(
        videocore::native::ComputePHashParts(invalid, &parts) != ImageStatus::ok,
        "invalid/phash",
        "invalid image accepted");
    expect(
        std::all_of(parts.begin(), parts.end(), [](uint64_t v) { return v == 0; }),
        "invalid/phash",
        "failed pHash did not clear output");

    std::array<float, 128> sobel;
    sobel.fill(1.0f);
    expect(
        videocore::native::ComputeSobelHistogram(invalid, &sobel) !=
            ImageStatus::ok,
        "invalid/sobel",
        "invalid image accepted");
    bool all_zero = true;
    for (float value : sobel) {
        uint32_t bits = 1;
        std::memcpy(&bits, &value, sizeof(bits));
        all_zero = all_zero && bits == 0;
    }
    expect(all_zero, "invalid/sobel", "failed Sobel did not clear output");
}

#endif

}  // namespace

int main() {
#if !defined(VC_IMAGE_ALGORITHMS_PRESENT)
    std::fprintf(
        stderr,
        "RED: VideoCore image algorithms are absent; compatibility cannot run\n");
    return 1;
#else
    const std::filesystem::path compat_root = VC_COMPAT_ROOT;
    const std::filesystem::path testdata_root = VC_IMAGE_TESTDATA_ROOT;
    const std::string golden = read_text(compat_root / "legacy-golden.json");
    expect(!golden.empty(), "golden", "legacy-golden.json missing");
    check_frozen_image(
        compat_root, golden, "images/synthetic-pattern.jpg");
    check_frozen_image(
        compat_root, golden, "images/synthetic-bars.png");
    check_frozen_image(
        compat_root, golden, "images/synthetic-portrait.webp");
    check_level_b(
        testdata_root / "level_b",
        std::filesystem::path(VC_LEVEL_B_LEGACY_GOLDEN));
    check_luma_case(
        testdata_root / "luma" / "luma_8x8_p0.lumabin",
        "76484f06c07e7398046ff258f9b0d39a013face5064f8d27fb900ce73f81b0f9",
        100);
    check_luma_case(
        testdata_root / "luma" / "luma_63x65_p3.lumabin",
        "000000002c4b11342c4b2c4b0000554b00002c4b113411342c4b585e2c4b017e",
        0);
    check_luma_case(
        testdata_root / "luma" / "luma_4096x3072_p5.lumabin",
        "000055555510555555505555555155550000ffff5515ffff5110ffffc1127fdf",
        100);
    check_invalid_algorithm_outputs();
    check_strict_tga_pnm_validation();
    check_gray_allocation_failure_is_test_local(
        testdata_root / "level_b");
    check_padded_stride_matches_tight_and_preserves_sentinels();
    if (failures == 0) {
        std::puts("IMAGE_COMPAT PASS frozen=3 level_b=20 corrupt=14 luma=3");
    }
    return failures == 0 ? 0 : 1;
#endif
}
