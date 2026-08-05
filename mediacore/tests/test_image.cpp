#include <mediacore/mediacore.h>

#include <png.h>
#include <turbojpeg.h>
#include <webp/encode.h>

#include <algorithm>
#include <array>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <string>
#include <tuple>
#include <vector>

namespace {

int failures = 0;

void expect(bool condition, const char* message) {
    if (!condition) {
        std::fprintf(stderr, "FAIL: %s\n", message);
        ++failures;
    }
}

std::vector<uint8_t> rgb_pattern(int width, int height) {
    std::vector<uint8_t> rgb(static_cast<size_t>(width) * height * 3);
    for (int y = 0; y < height; ++y) {
        for (int x = 0; x < width; ++x) {
            const size_t i = (static_cast<size_t>(y) * width + x) * 3;
            rgb[i] = static_cast<uint8_t>(x * 17 + y);
            rgb[i + 1] = static_cast<uint8_t>(y * 19 + x);
            rgb[i + 2] = static_cast<uint8_t>((x + y) * 11);
        }
    }
    return rgb;
}

std::vector<uint8_t> make_jpeg(const std::vector<uint8_t>& rgb, int width, int height) {
    tjhandle handle = tjInitCompress();
    expect(handle != nullptr, "test JPEG encoder initializes");
    if (handle == nullptr) {
        return {};
    }
    unsigned char* compressed = nullptr;
    unsigned long compressed_size = 0;
    const int rc = tjCompress2(
        handle,
        const_cast<unsigned char*>(rgb.data()),
        width,
        0,
        height,
        TJPF_RGB,
        &compressed,
        &compressed_size,
        TJSAMP_444,
        95,
        0);
    expect(rc == 0, "test JPEG encoder succeeds");
    std::vector<uint8_t> result;
    if (rc == 0) {
        result.assign(compressed, compressed + compressed_size);
    }
    tjFree(compressed);
    tjDestroy(handle);
    return result;
}

std::vector<uint8_t> make_png(const std::vector<uint8_t>& rgb, int width, int height) {
    png_image image{};
    image.version = PNG_IMAGE_VERSION;
    image.width = static_cast<png_uint_32>(width);
    image.height = static_cast<png_uint_32>(height);
    image.format = PNG_FORMAT_RGB;
    png_alloc_size_t size = 0;
    expect(png_image_write_to_memory(&image, nullptr, &size, 0, rgb.data(), 0, nullptr) != 0,
           "test PNG encoder sizes output");
    std::vector<uint8_t> result(static_cast<size_t>(size));
    expect(png_image_write_to_memory(&image, result.data(), &size, 0, rgb.data(), 0, nullptr) != 0,
           "test PNG encoder succeeds");
    result.resize(static_cast<size_t>(size));
    return result;
}

std::vector<uint8_t> make_webp(const std::vector<uint8_t>& rgb, int width, int height) {
    uint8_t* output = nullptr;
    const size_t size = WebPEncodeLosslessRGB(rgb.data(), width, height, width * 3, &output);
    expect(size != 0 && output != nullptr, "test WebP encoder succeeds");
    std::vector<uint8_t> result;
    if (output != nullptr) {
        result.assign(output, output + size);
    }
    WebPFree(output);
    return result;
}

std::vector<uint8_t> make_ppm(const std::vector<uint8_t>& rgb, int width, int height) {
    const std::string header =
        "P6\n" + std::to_string(width) + " " + std::to_string(height) + "\n255\n";
    std::vector<uint8_t> result(header.begin(), header.end());
    result.insert(result.end(), rgb.begin(), rgb.end());
    return result;
}

void append_le16(std::vector<uint8_t>& out, uint16_t value) {
    out.push_back(static_cast<uint8_t>(value));
    out.push_back(static_cast<uint8_t>(value >> 8));
}

void append_le32(std::vector<uint8_t>& out, uint32_t value) {
    out.push_back(static_cast<uint8_t>(value));
    out.push_back(static_cast<uint8_t>(value >> 8));
    out.push_back(static_cast<uint8_t>(value >> 16));
    out.push_back(static_cast<uint8_t>(value >> 24));
}

std::vector<uint8_t> make_bmp(const std::vector<uint8_t>& rgb, int width, int height) {
    const uint32_t stride = static_cast<uint32_t>((width * 3 + 3) & ~3);
    const uint32_t pixels_size = stride * static_cast<uint32_t>(height);
    std::vector<uint8_t> out;
    out.reserve(54 + pixels_size);
    out.push_back('B');
    out.push_back('M');
    append_le32(out, 54 + pixels_size);
    append_le16(out, 0);
    append_le16(out, 0);
    append_le32(out, 54);
    append_le32(out, 40);
    append_le32(out, static_cast<uint32_t>(width));
    append_le32(out, static_cast<uint32_t>(height));
    append_le16(out, 1);
    append_le16(out, 24);
    append_le32(out, 0);
    append_le32(out, pixels_size);
    append_le32(out, 2835);
    append_le32(out, 2835);
    append_le32(out, 0);
    append_le32(out, 0);
    for (int y = height - 1; y >= 0; --y) {
        const size_t row_start = out.size();
        for (int x = 0; x < width; ++x) {
            const size_t i = (static_cast<size_t>(y) * width + x) * 3;
            out.push_back(rgb[i + 2]);
            out.push_back(rgb[i + 1]);
            out.push_back(rgb[i]);
        }
        while (out.size() - row_start < stride) {
            out.push_back(0);
        }
    }
    return out;
}

std::vector<uint8_t> make_palette_bmp(int width, int height) {
    const uint32_t stride = static_cast<uint32_t>((width + 3) & ~3);
    const uint32_t pixels_size = stride * static_cast<uint32_t>(height);
    std::vector<uint8_t> out;
    out.reserve(62 + pixels_size);
    out.push_back('B');
    out.push_back('M');
    append_le32(out, 62 + pixels_size);
    append_le16(out, 0);
    append_le16(out, 0);
    append_le32(out, 62);
    append_le32(out, 40);
    append_le32(out, static_cast<uint32_t>(width));
    append_le32(out, static_cast<uint32_t>(height));
    append_le16(out, 1);
    append_le16(out, 8);
    append_le32(out, 0);
    append_le32(out, pixels_size);
    append_le32(out, 2835);
    append_le32(out, 2835);
    append_le32(out, 2);
    append_le32(out, 2);
    out.insert(out.end(), {0, 0, 0, 0, 255, 255, 255, 0});
    for (int y = height - 1; y >= 0; --y) {
        const size_t row_start = out.size();
        for (int x = 0; x < width; ++x) {
            out.push_back(static_cast<uint8_t>((x + y) & 1));
        }
        while (out.size() - row_start < stride) {
            out.push_back(0);
        }
    }
    return out;
}

std::vector<uint8_t> make_tga(const std::vector<uint8_t>& rgb, int width, int height) {
    std::vector<uint8_t> out(18, 0);
    out[2] = 2;
    out[12] = static_cast<uint8_t>(width);
    out[13] = static_cast<uint8_t>(width >> 8);
    out[14] = static_cast<uint8_t>(height);
    out[15] = static_cast<uint8_t>(height >> 8);
    out[16] = 24;
    out[17] = 0x20;
    for (int y = 0; y < height; ++y) {
        for (int x = 0; x < width; ++x) {
            const size_t i = (static_cast<size_t>(y) * width + x) * 3;
            out.push_back(rgb[i + 2]);
            out.push_back(rgb[i + 1]);
            out.push_back(rgb[i]);
        }
    }
    return out;
}

void append_gif_code(std::vector<uint8_t>& bytes, unsigned& bit_count, uint32_t& bits, uint8_t code) {
    bits |= static_cast<uint32_t>(code) << bit_count;
    bit_count += 3;
    while (bit_count >= 8) {
        bytes.push_back(static_cast<uint8_t>(bits));
        bits >>= 8;
        bit_count -= 8;
    }
}

std::vector<uint8_t> make_gif() {
    std::vector<uint8_t> out = {
        'G', 'I', 'F', '8', '9', 'a',
        8, 0, 8, 0, 0x80, 0, 0,
        0, 0, 0, 255, 255, 255,
        0x2c, 0, 0, 0, 0, 8, 0, 8, 0, 0,
        2,
    };
    std::vector<uint8_t> lzw;
    unsigned bit_count = 0;
    uint32_t bits = 0;
    for (int i = 0; i < 64; ++i) {
        append_gif_code(lzw, bit_count, bits, 4);
        append_gif_code(lzw, bit_count, bits, static_cast<uint8_t>(i & 1));
    }
    append_gif_code(lzw, bit_count, bits, 5);
    if (bit_count != 0) {
        lzw.push_back(static_cast<uint8_t>(bits));
    }
    out.push_back(static_cast<uint8_t>(lzw.size()));
    out.insert(out.end(), lzw.begin(), lzw.end());
    out.push_back(0);
    out.push_back(0x3b);
    return out;
}

std::vector<uint8_t> add_gif_comment(const std::vector<uint8_t>& gif) {
    std::vector<uint8_t> result = gif;
    result.insert(
        result.begin() + 19,
        {0x21, 0xfe, 0x03, 'p', 'd', 'q', 0x00});
    return result;
}

void check_decode(const char* name, const std::vector<uint8_t>& bytes, int width, int height) {
    mc_image image{-1, -1, reinterpret_cast<uint8_t*>(1)};
    char error[MC_ERRBUF_LEN];
    const int rc = mc_decode_gray(bytes.data(), bytes.size(), &image, error, sizeof(error));
    if (rc != MC_OK) {
        std::fprintf(stderr, "FAIL: %s decode rc=%d error=%s\n", name, rc, error);
        ++failures;
        return;
    }
    expect(image.width == width, "decoded width matches");
    expect(image.height == height, "decoded height matches");
    expect(image.gray != nullptr, "decoded gray plane is owned memory");
    if (image.gray != nullptr) {
        expect(image.gray[0] == 0, "BT.601 first black pixel is exact");
    }
    mc_free_image(&image);
    expect(image.width == 0 && image.height == 0 && image.gray == nullptr,
           "mc_free_image clears the caller-visible owner");
    mc_free_image(&image);
    expect(image.width == 0 && image.height == 0 && image.gray == nullptr,
           "mc_free_image is idempotent");
}

void check_rejected(const char* name, const std::vector<uint8_t>& bytes) {
    mc_image image{99, 99, reinterpret_cast<uint8_t*>(1)};
    char error[MC_ERRBUF_LEN];
    const int rc = mc_decode_gray(bytes.data(), bytes.size(), &image, error, sizeof(error));
    if (rc == MC_OK) {
        std::fprintf(stderr, "FAIL: corrupt %s unexpectedly decoded\n", name);
        ++failures;
        mc_free_image(&image);
        return;
    }
    expect(image.width == 0 && image.height == 0 && image.gray == nullptr,
           "rejected decode leaves an empty owner");
    expect(error[0] != '\0', "rejected decode reports a stable error");
}

}  // namespace

int main() {
    constexpr int width = 8;
    constexpr int height = 8;
    const std::vector<uint8_t> rgb = rgb_pattern(width, height);
    const auto jpeg = make_jpeg(rgb, width, height);
    const auto png = make_png(rgb, width, height);
    const auto webp = make_webp(rgb, width, height);
    const auto ppm = make_ppm(rgb, width, height);
    const auto bmp = make_bmp(rgb, width, height);
    const auto tga = make_tga(rgb, width, height);
    const auto gif = make_gif();
    const auto palette_bmp = make_palette_bmp(width, height);
    const auto gif_with_comment = add_gif_comment(gif);

    check_decode("JPEG magic", jpeg, width, height);
    check_decode("PNG magic", png, width, height);
    check_decode("WebP magic", webp, width, height);
    check_decode("PNM fallback", ppm, width, height);
    check_decode("BMP fallback", bmp, width, height);
    check_decode("TGA fallback", tga, width, height);
    check_decode("GIF fallback", gif, width, height);
    check_decode("palette BMP fallback", palette_bmp, width, height);
    check_decode("GIF extension fallback", gif_with_comment, width, height);

    for (const auto& entry : std::array<std::tuple<const char*, const std::vector<uint8_t>*, size_t>, 7>{{
             {"JPEG", &jpeg, jpeg.size() / 2},
             {"PNG", &png, png.size() / 2},
             {"WebP", &webp, webp.size() / 2},
             {"PNM", &ppm, 15},
             {"BMP", &bmp, 54},
             {"TGA", &tga, 18},
             {"GIF", &gif, 30},
         }}) {
        const auto* bytes = std::get<1>(entry);
        const size_t keep = (std::min)(std::get<2>(entry), bytes->size());
        std::vector<uint8_t> truncated(bytes->begin(), bytes->begin() + keep);
        check_rejected(std::get<0>(entry), truncated);
    }
    check_rejected("unsupported", std::vector<uint8_t>{'n', 'o', 't', ' ', 'a', 'n', ' ', 'i', 'm', 'a', 'g', 'e'});

    std::vector<uint8_t> forged_bmp(bmp.begin(), bmp.begin() + bmp.size() / 2);
    const uint32_t forged_bmp_size = static_cast<uint32_t>(forged_bmp.size());
    forged_bmp[2] = static_cast<uint8_t>(forged_bmp_size);
    forged_bmp[3] = static_cast<uint8_t>(forged_bmp_size >> 8);
    forged_bmp[4] = static_cast<uint8_t>(forged_bmp_size >> 16);
    forged_bmp[5] = static_cast<uint8_t>(forged_bmp_size >> 24);
    check_rejected("truncated BMP with forged file size", forged_bmp);

    std::vector<uint8_t> forged_gif(gif.begin(), gif.begin() + gif.size() / 2);
    forged_gif.push_back(0x3b);
    check_rejected("truncated GIF with forged trailer", forged_gif);

    const auto too_small = make_ppm(rgb_pattern(7, 8), 7, 8);
    mc_image small{};
    char error[MC_ERRBUF_LEN];
    expect(mc_decode_gray(too_small.data(), too_small.size(), &small, error, sizeof(error)) == MC_ERR_SIZE,
           "dimensions below 8 pixels are rejected");

    std::vector<uint8_t> huge = make_bmp({}, 0, 0);
    huge[18] = 0x21;
    huge[19] = 0x4e;
    huge[20] = 0;
    huge[21] = 0;
    huge[22] = 0x21;
    huge[23] = 0x4e;
    huge[24] = 0;
    huge[25] = 0;
    check_rejected("declared 20001x20001 BMP", huge);

    std::array<uint8_t, MC_PDQ256_BYTES> hash1{};
    std::array<uint8_t, MC_PDQ256_BYTES> hash2{};
    int32_t quality1 = -1;
    int32_t quality2 = -1;
    int32_t out_width = -1;
    int32_t out_height = -1;
    expect(mc_image_phase1(png.data(), png.size(), hash1.data(), &quality1,
                           &out_width, &out_height, error, sizeof(error)) == MC_OK,
           "phase1 decodes and hashes an image");
    expect(mc_image_phase1(png.data(), png.size(), hash2.data(), &quality2,
                           &out_width, &out_height, error, sizeof(error)) == MC_OK,
           "phase1 is repeatable");
    expect(hash1 == hash2 && quality1 == quality2, "PDQ and quality are deterministic");
    expect(out_width == width && out_height == height, "phase1 reports image dimensions");
    expect(mc_hamming_distance(hash1.data(), hash2.data()) == 0, "identical PDQ distance is zero");
    hash2[31] ^= 1;
    expect(mc_hamming_distance(hash1.data(), hash2.data()) == 1, "single-bit PDQ distance is one");

    expect(mc_decode_gray(nullptr, 1, &small, error, sizeof(error)) == MC_ERR_NULL_ARG,
           "null decode input is rejected");
    expect(mc_decode_gray(png.data(), png.size(), nullptr, error, sizeof(error)) == MC_ERR_NULL_ARG,
           "null decode owner is rejected");

    if (failures != 0) {
        std::fprintf(stderr, "%d image test(s) failed\n", failures);
        return 1;
    }
    std::puts("image decoder and PDQ tests passed");
    return 0;
}
