#include <pdq/cpp/hashing/pdqhashing.h>

#include <png.h>
#include <turbojpeg.h>
#include <webp/decode.h>
#include <webp/encode.h>
#include <windows.h>
#include <wincodec.h>
#include <wrl/client.h>

#include <algorithm>
#include <cctype>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <limits>
#include <stdexcept>
#include <string>
#include <vector>

namespace {

using facebook::pdq::hashing::Hash256;
using facebook::pdq::hashing::fillFloatLumaFromRGB;
using facebook::pdq::hashing::pdqHash256FromFloatLuma;
using Microsoft::WRL::ComPtr;

uint32_t random_state = 0x5eeda11u;

uint32_t xorshift() {
    random_state ^= random_state << 13;
    random_state ^= random_state >> 17;
    random_state ^= random_state << 5;
    return random_state;
}

std::vector<uint8_t> make_rgb(int width, int height, int pattern, bool two_color) {
    std::vector<uint8_t> rgb(static_cast<size_t>(width) * height * 3);
    for (int y = 0; y < height; ++y) {
        for (int x = 0; x < width; ++x) {
            uint8_t red = 0;
            uint8_t green = 0;
            uint8_t blue = 0;
            if (two_color) {
                const uint8_t value = ((x / 4 + y / 4 + pattern) & 1) != 0 ? 255 : 0;
                red = green = blue = value;
            } else {
                switch (pattern) {
                    case 0:
                        red = static_cast<uint8_t>(xorshift());
                        green = static_cast<uint8_t>(xorshift());
                        blue = static_cast<uint8_t>(xorshift());
                        break;
                    case 1:
                        red = static_cast<uint8_t>((x * 255) / (std::max)(1, width - 1));
                        green = static_cast<uint8_t>((y * 255) / (std::max)(1, height - 1));
                        blue = static_cast<uint8_t>((x + y) * 7);
                        break;
                    case 2:
                        red = static_cast<uint8_t>(((x / 8) + (y / 8)) & 1 ? 240 : 10);
                        green = static_cast<uint8_t>((x * 13 + y * 3) & 255);
                        blue = static_cast<uint8_t>((y * 17 + x * 5) & 255);
                        break;
                    default:
                        red = static_cast<uint8_t>((x * x + y * 11) & 255);
                        green = static_cast<uint8_t>((y * y + x * 19) & 255);
                        blue = static_cast<uint8_t>((x * y + 37) & 255);
                        break;
                }
            }
            const size_t offset = (static_cast<size_t>(y) * width + x) * 3;
            rgb[offset] = red;
            rgb[offset + 1] = green;
            rgb[offset + 2] = blue;
        }
    }
    return rgb;
}

std::vector<uint8_t> encode_jpeg(const std::vector<uint8_t>& rgb, int width, int height) {
    tjhandle handle = tjInitCompress();
    if (handle == nullptr) {
        throw std::runtime_error("tjInitCompress failed");
    }
    uint8_t* output = nullptr;
    unsigned long length = 0;
    if (tjCompress2(
            handle,
            const_cast<uint8_t*>(rgb.data()),
            width,
            0,
            height,
            TJPF_RGB,
            &output,
            &length,
            TJSAMP_444,
            95,
            0) != 0) {
        const std::string error = tjGetErrorStr2(handle);
        tjDestroy(handle);
        throw std::runtime_error(error);
    }
    std::vector<uint8_t> result(output, output + length);
    tjFree(output);
    tjDestroy(handle);
    return result;
}

std::vector<uint8_t> encode_png(const std::vector<uint8_t>& rgb, int width, int height) {
    png_image image{};
    image.version = PNG_IMAGE_VERSION;
    image.width = static_cast<png_uint_32>(width);
    image.height = static_cast<png_uint_32>(height);
    image.format = PNG_FORMAT_RGB;
    png_alloc_size_t length = 0;
    if (png_image_write_to_memory(&image, nullptr, &length, 0, rgb.data(), 0, nullptr) == 0) {
        throw std::runtime_error("PNG size calculation failed");
    }
    std::vector<uint8_t> result(static_cast<size_t>(length));
    if (png_image_write_to_memory(
            &image,
            result.data(),
            &length,
            0,
            rgb.data(),
            0,
            nullptr) == 0) {
        throw std::runtime_error("PNG encode failed");
    }
    result.resize(static_cast<size_t>(length));
    return result;
}

std::vector<uint8_t> encode_webp(const std::vector<uint8_t>& rgb, int width, int height) {
    uint8_t* output = nullptr;
    const size_t length =
        WebPEncodeLosslessRGB(rgb.data(), width, height, width * 3, &output);
    if (length == 0 || output == nullptr) {
        throw std::runtime_error("WebP encode failed");
    }
    std::vector<uint8_t> result(output, output + length);
    WebPFree(output);
    return result;
}

void append_le16(std::vector<uint8_t>& bytes, uint16_t value) {
    bytes.push_back(static_cast<uint8_t>(value));
    bytes.push_back(static_cast<uint8_t>(value >> 8));
}

void append_le32(std::vector<uint8_t>& bytes, uint32_t value) {
    bytes.push_back(static_cast<uint8_t>(value));
    bytes.push_back(static_cast<uint8_t>(value >> 8));
    bytes.push_back(static_cast<uint8_t>(value >> 16));
    bytes.push_back(static_cast<uint8_t>(value >> 24));
}

std::vector<uint8_t> encode_bmp(const std::vector<uint8_t>& rgb, int width, int height) {
    const uint32_t stride = static_cast<uint32_t>((width * 3 + 3) & ~3);
    const uint32_t pixel_bytes = stride * static_cast<uint32_t>(height);
    std::vector<uint8_t> result;
    result.reserve(54 + pixel_bytes);
    result.push_back('B');
    result.push_back('M');
    append_le32(result, 54 + pixel_bytes);
    append_le16(result, 0);
    append_le16(result, 0);
    append_le32(result, 54);
    append_le32(result, 40);
    append_le32(result, static_cast<uint32_t>(width));
    append_le32(result, static_cast<uint32_t>(height));
    append_le16(result, 1);
    append_le16(result, 24);
    append_le32(result, 0);
    append_le32(result, pixel_bytes);
    append_le32(result, 2835);
    append_le32(result, 2835);
    append_le32(result, 0);
    append_le32(result, 0);
    for (int y = height - 1; y >= 0; --y) {
        const size_t row_start = result.size();
        for (int x = 0; x < width; ++x) {
            const size_t offset = (static_cast<size_t>(y) * width + x) * 3;
            result.push_back(rgb[offset + 2]);
            result.push_back(rgb[offset + 1]);
            result.push_back(rgb[offset]);
        }
        while (result.size() - row_start < stride) {
            result.push_back(0);
        }
    }
    return result;
}

void append_gif_code(
    std::vector<uint8_t>& bytes,
    unsigned& bit_count,
    uint32_t& bits,
    uint8_t code) {
    bits |= static_cast<uint32_t>(code) << bit_count;
    bit_count += 3;
    while (bit_count >= 8) {
        bytes.push_back(static_cast<uint8_t>(bits));
        bits >>= 8;
        bit_count -= 8;
    }
}

std::vector<uint8_t> encode_gif(
    const std::vector<uint8_t>& rgb,
    int width,
    int height) {
    std::vector<uint8_t> result = {
        'G', 'I', 'F', '8', '9', 'a',
        static_cast<uint8_t>(width),
        static_cast<uint8_t>(width >> 8),
        static_cast<uint8_t>(height),
        static_cast<uint8_t>(height >> 8),
        0x80, 0, 0,
        0, 0, 0, 255, 255, 255,
        0x2c, 0, 0, 0, 0,
        static_cast<uint8_t>(width),
        static_cast<uint8_t>(width >> 8),
        static_cast<uint8_t>(height),
        static_cast<uint8_t>(height >> 8),
        0,
        2,
    };
    std::vector<uint8_t> lzw;
    unsigned bit_count = 0;
    uint32_t bits = 0;
    for (int y = 0; y < height; ++y) {
        for (int x = 0; x < width; ++x) {
            const size_t offset = (static_cast<size_t>(y) * width + x) * 3;
            append_gif_code(lzw, bit_count, bits, 4);
            append_gif_code(lzw, bit_count, bits, rgb[offset] == 0 ? 0 : 1);
        }
    }
    append_gif_code(lzw, bit_count, bits, 5);
    if (bit_count != 0) {
        lzw.push_back(static_cast<uint8_t>(bits));
    }
    size_t offset = 0;
    while (offset < lzw.size()) {
        const size_t chunk = (std::min)(static_cast<size_t>(255), lzw.size() - offset);
        result.push_back(static_cast<uint8_t>(chunk));
        result.insert(result.end(), lzw.begin() + offset, lzw.begin() + offset + chunk);
        offset += chunk;
    }
    result.push_back(0);
    result.push_back(0x3b);
    return result;
}

Hash256 reference_hash(const std::vector<uint8_t>& rgb, int width, int height, int& quality) {
    std::vector<float> full_buffer_1(static_cast<size_t>(width) * height);
    std::vector<float> full_buffer_2(full_buffer_1.size());
    fillFloatLumaFromRGB(
        const_cast<uint8_t*>(rgb.data()),
        const_cast<uint8_t*>(rgb.data() + 1),
        const_cast<uint8_t*>(rgb.data() + 2),
        height,
        width,
        width * 3,
        3,
        full_buffer_1.data());
    float buffer_64x64[64][64];
    float buffer_16x64[16][64];
    float buffer_16x16[16][16];
    Hash256 hash;
    pdqHash256FromFloatLuma(
        full_buffer_1.data(),
        full_buffer_2.data(),
        height,
        width,
        buffer_64x64,
        buffer_16x64,
        buffer_16x16,
        hash,
        quality);
    return hash;
}

std::vector<uint8_t> decode_reference_wic(
    IWICImagingFactory* factory,
    const std::filesystem::path& path,
    int& width,
    int& height) {
    ComPtr<IWICBitmapDecoder> decoder;
    HRESULT result = factory->CreateDecoderFromFilename(
        path.c_str(),
        nullptr,
        GENERIC_READ,
        WICDecodeMetadataCacheOnLoad,
        &decoder);
    if (FAILED(result)) {
        throw std::runtime_error(
            "WIC decoder creation failed for " + path.string());
    }
    ComPtr<IWICBitmapFrameDecode> frame;
    result = decoder->GetFrame(0, &frame);
    if (FAILED(result)) {
        throw std::runtime_error(
            "WIC frame read failed for " + path.string());
    }
    UINT decoded_width = 0;
    UINT decoded_height = 0;
    result = frame->GetSize(&decoded_width, &decoded_height);
    if (FAILED(result) || decoded_width == 0 || decoded_height == 0 ||
        decoded_width > static_cast<UINT>((std::numeric_limits<int>::max)()) ||
        decoded_height > static_cast<UINT>((std::numeric_limits<int>::max)())) {
        throw std::runtime_error(
            "WIC dimensions invalid for " + path.string());
    }
    ComPtr<IWICFormatConverter> converter;
    result = factory->CreateFormatConverter(&converter);
    if (FAILED(result)) {
        throw std::runtime_error("WIC converter creation failed");
    }
    result = converter->Initialize(
        frame.Get(),
        GUID_WICPixelFormat24bppRGB,
        WICBitmapDitherTypeNone,
        nullptr,
        0.0,
        WICBitmapPaletteTypeCustom);
    if (FAILED(result)) {
        throw std::runtime_error(
            "WIC RGB conversion failed for " + path.string());
    }
    width = static_cast<int>(decoded_width);
    height = static_cast<int>(decoded_height);
    const size_t stride = static_cast<size_t>(width) * 3;
    const size_t byte_count = stride * static_cast<size_t>(height);
    if (stride > (std::numeric_limits<UINT>::max)() ||
        byte_count > (std::numeric_limits<UINT>::max)()) {
        throw std::runtime_error("WIC image too large");
    }
    std::vector<uint8_t> rgb(byte_count);
    result = converter->CopyPixels(
        nullptr,
        static_cast<UINT>(stride),
        static_cast<UINT>(byte_count),
        rgb.data());
    if (FAILED(result)) {
        throw std::runtime_error(
            "WIC pixel read failed for " + path.string());
    }
    return rgb;
}

void write_bytes(const std::filesystem::path& path, const std::vector<uint8_t>& bytes) {
    std::ofstream output(path, std::ios::binary);
    output.write(
        reinterpret_cast<const char*>(bytes.data()),
        static_cast<std::streamsize>(bytes.size()));
    if (!output) {
        throw std::runtime_error("cannot write " + path.string());
    }
}

}  // namespace

int main(int argc, char** argv) {
    if (argc != 3) {
        std::fprintf(
            stderr,
            "usage: mc_make_level_b <outdir> <pinned-pdq-data-root>\n");
        return 2;
    }
    const HRESULT com_result =
        CoInitializeEx(nullptr, COINIT_MULTITHREADED);
    if (FAILED(com_result)) {
        std::fprintf(stderr, "COM initialization failed\n");
        return 1;
    }
    try {
        ComPtr<IWICImagingFactory> wic_factory;
        HRESULT factory_result = CoCreateInstance(
            CLSID_WICImagingFactory,
            nullptr,
            CLSCTX_INPROC_SERVER,
            IID_PPV_ARGS(&wic_factory));
        if (FAILED(factory_result)) {
            throw std::runtime_error("WIC factory creation failed");
        }
        const std::filesystem::path root = std::filesystem::absolute(argv[1]);
        const std::filesystem::path official_root =
            std::filesystem::absolute(argv[2]);
        const std::filesystem::path images = root / "images";
        const std::filesystem::path corrupt = root / "corrupt";
        std::filesystem::create_directories(images);
        std::filesystem::create_directories(corrupt);
        std::ofstream golden(root / "level_b.tsv", std::ios::binary);
        if (!golden) {
            throw std::runtime_error("cannot create Level B golden");
        }

        struct Size {
            int width;
            int height;
        };
        static constexpr Size sizes[] = {
            {128, 96},
            {96, 128},
            {257, 129},
            {32, 16},
        };
        static constexpr const char* formats[] = {"jpg", "png", "webp", "bmp", "gif"};
        std::vector<std::pair<std::string, std::vector<uint8_t>>> first_by_format;
        int count = 0;
        for (const char* format : formats) {
            for (int index = 0; index < 4; ++index) {
                const Size size = sizes[index];
                const bool gif = std::strcmp(format, "gif") == 0;
                const std::vector<uint8_t> rgb =
                    make_rgb(size.width, size.height, index, gif);
                std::vector<uint8_t> encoded;
                if (std::strcmp(format, "jpg") == 0) {
                    encoded = encode_jpeg(rgb, size.width, size.height);
                } else if (std::strcmp(format, "png") == 0) {
                    encoded = encode_png(rgb, size.width, size.height);
                } else if (std::strcmp(format, "webp") == 0) {
                    encoded = encode_webp(rgb, size.width, size.height);
                } else if (std::strcmp(format, "bmp") == 0) {
                    encoded = encode_bmp(rgb, size.width, size.height);
                } else {
                    encoded = encode_gif(rgb, size.width, size.height);
                }
                const std::string filename =
                    std::string(format) + "_" + std::to_string(index) + "." + format;
                const std::filesystem::path path = images / filename;
                write_bytes(path, encoded);
                int decoded_width = 0;
                int decoded_height = 0;
                const std::vector<uint8_t> decoded =
                    decode_reference_wic(
                        wic_factory.Get(),
                        path,
                        decoded_width,
                        decoded_height);
                if (decoded_width != size.width || decoded_height != size.height) {
                    throw std::runtime_error("reference decoder dimensions differ");
                }
                int quality = 0;
                const Hash256 hash =
                    reference_hash(decoded, decoded_width, decoded_height, quality);
                golden << path.generic_string() << '\t' << hash.format() << '\t'
                       << quality << '\n';
                if (index == 0) {
                    first_by_format.emplace_back(format, encoded);
                }
                ++count;
            }
        }
        golden.close();

        std::ofstream official_golden(
            root / "level_b_official.tsv",
            std::ios::binary);
        if (!official_golden) {
            throw std::runtime_error("cannot create official Level B golden");
        }
        int official_count = 0;
        for (const auto& entry :
             std::filesystem::recursive_directory_iterator(official_root)) {
            if (!entry.is_regular_file()) {
                continue;
            }
            std::string extension = entry.path().extension().string();
            std::transform(
                extension.begin(),
                extension.end(),
                extension.begin(),
                [](unsigned char value) {
                    return static_cast<char>(std::tolower(value));
                });
            if (extension != ".jpg" && extension != ".jpeg" &&
                extension != ".png" && extension != ".webp") {
                continue;
            }
            int official_width = 0;
            int official_height = 0;
            const std::vector<uint8_t> official_rgb =
                decode_reference_wic(
                    wic_factory.Get(),
                    entry.path(),
                    official_width,
                    official_height);
            int official_quality = 0;
            const Hash256 official_hash = reference_hash(
                official_rgb,
                official_width,
                official_height,
                official_quality);
            official_golden << entry.path().generic_string() << '\t'
                            << official_hash.format() << '\t'
                            << official_quality << '\n';
            ++official_count;
        }
        official_golden.close();
        if (official_count != 49) {
            throw std::runtime_error(
                "expected 49 supported official images, found " +
                std::to_string(official_count));
        }

        write_bytes(corrupt / "invalid_empty.bin", {});
        write_bytes(
            corrupt / "invalid_text.bin",
            std::vector<uint8_t>{'n', 'o', 't', ' ', 'a', 'n', ' ', 'i', 'm', 'a', 'g', 'e'});
        for (const auto& item : first_by_format) {
            const size_t keep =
                item.first == "bmp" ? 54 :
                item.first == "gif" ? 30 :
                item.second.size() / 2;
            write_bytes(
                corrupt / ("invalid_truncated." + item.first),
                std::vector<uint8_t>(
                    item.second.begin(),
                    item.second.begin() + (std::min)(keep, item.second.size())));
        }
        const std::vector<uint8_t>& jpeg_seed = first_by_format[0].second;
        write_bytes(
            corrupt / "invalid_jpeg_trunc50.jpg",
            std::vector<uint8_t>(
                jpeg_seed.begin(),
                jpeg_seed.begin() + jpeg_seed.size() / 2));
        write_bytes(
            corrupt / "invalid_jpeg_trunc95.jpg",
            std::vector<uint8_t>(
                jpeg_seed.begin(),
                jpeg_seed.begin() + jpeg_seed.size() * 95 / 100));
        std::vector<uint8_t> zeroed_mid = jpeg_seed;
        const size_t zero_start = zeroed_mid.size() / 2;
        const size_t zero_end =
            (std::min)(zeroed_mid.size(), zero_start + 4096);
        std::fill(
            zeroed_mid.begin() + zero_start,
            zeroed_mid.begin() + zero_end,
            0);
        write_bytes(corrupt / "invalid_jpeg_zeroed_mid.jpg", zeroed_mid);
        std::vector<uint8_t> bad_magic = jpeg_seed;
        bad_magic[0] = 0;
        bad_magic[1] = 0x11;
        bad_magic[2] = 0x22;
        write_bytes(corrupt / "invalid_jpeg_badmagic.jpg", bad_magic);
        std::vector<uint8_t> random_bytes(4096);
        uint32_t corrupt_state = 0xc0ffee11u;
        for (uint8_t& value : random_bytes) {
            corrupt_state ^= corrupt_state << 13;
            corrupt_state ^= corrupt_state >> 17;
            corrupt_state ^= corrupt_state << 5;
            value = static_cast<uint8_t>(corrupt_state);
        }
        write_bytes(corrupt / "invalid_random.bin", random_bytes);

        const std::vector<uint8_t>& bmp_seed = first_by_format[3].second;
        std::vector<uint8_t> forged_bmp(
            bmp_seed.begin(),
            bmp_seed.begin() + bmp_seed.size() / 2);
        const uint32_t forged_bmp_length =
            static_cast<uint32_t>(forged_bmp.size());
        forged_bmp[2] = static_cast<uint8_t>(forged_bmp_length);
        forged_bmp[3] = static_cast<uint8_t>(forged_bmp_length >> 8);
        forged_bmp[4] = static_cast<uint8_t>(forged_bmp_length >> 16);
        forged_bmp[5] = static_cast<uint8_t>(forged_bmp_length >> 24);
        write_bytes(corrupt / "invalid_bmp_forged_size.bmp", forged_bmp);

        const std::vector<uint8_t>& gif_seed = first_by_format[4].second;
        std::vector<uint8_t> forged_gif(
            gif_seed.begin(),
            gif_seed.begin() + gif_seed.size() / 2);
        forged_gif.push_back(0x3b);
        write_bytes(corrupt / "invalid_gif_forged_trailer.gif", forged_gif);
        write_bytes(root / "wrongext.png", first_by_format.front().second);
        std::printf(
            "wrote %d local and %d official Level B samples to %s\n",
            count,
            official_count,
            root.string().c_str());
        CoUninitialize();
        return count == 20 && official_count == 49 ? 0 : 1;
    } catch (const std::exception& error) {
        std::fprintf(stderr, "Level B generation failed: %s\n", error.what());
        CoUninitialize();
        return 1;
    }
}
