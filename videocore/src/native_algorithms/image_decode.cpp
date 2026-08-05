#include "native_algorithms/image_decode.h"

#include <png.h>
#include <turbojpeg.h>
#include <webp/decode.h>

#include <algorithm>
#include <array>
#include <cstdint>
#include <cstdlib>
#include <cstring>
#include <limits>
#include <memory>
#include <new>
#include <vector>

namespace videocore::native::stb {
bool Info(const uint8_t*, int, int*, int*) noexcept;
uint8_t* LoadRgb(const uint8_t*, int, int*, int*) noexcept;
void Free(void*) noexcept;
}  // namespace videocore::native::stb

namespace videocore::native {
namespace {

constexpr int64_t kMaxPixels = 400000000LL;
constexpr int32_t kMinSide = 8;

ImageStatus validate_dimensions(int64_t width, int64_t height) noexcept {
    if (width < kMinSide || height < kMinSide) {
        return ImageStatus::size_error;
    }
    if (width > (std::numeric_limits<int32_t>::max)() ||
        height > (std::numeric_limits<int32_t>::max)() ||
        width > kMaxPixels / height) {
        return ImageStatus::size_error;
    }
    return ImageStatus::ok;
}

ImageStatus rgb_to_gray(
    const uint8_t* rgb,
    int width,
    int height,
    GrayImage* out) {
    const ImageStatus dimensions = validate_dimensions(width, height);
    if (dimensions != ImageStatus::ok) return dimensions;
    const size_t pixel_count = static_cast<size_t>(width) * height;
    GrayImage result;
    result.width = width;
    result.height = height;
    result.stride = width;
    result.pixels.resize(pixel_count);
    for (size_t i = 0; i < pixel_count; ++i) {
        const uint8_t* pixel = rgb + i * 3;
        result.pixels[i] = static_cast<uint8_t>(
            (77 * pixel[0] + 150 * pixel[1] + 29 * pixel[2] + 128) >> 8);
    }
    *out = std::move(result);
    return ImageStatus::ok;
}

struct TurboHandleDeleter {
    void operator()(void* handle) const noexcept {
        if (handle != nullptr) {
            tjDestroy(handle);
        }
    }
};

using TurboHandle = std::unique_ptr<void, TurboHandleDeleter>;

ImageStatus decode_jpeg(
    const uint8_t* encoded,
    size_t encoded_size,
    GrayImage* out) {
    if (encoded_size > (std::numeric_limits<unsigned long>::max)()) {
        return ImageStatus::size_error;
    }
    TurboHandle handle(tjInitDecompress());
    if (!handle) return ImageStatus::decode_error;
    int width = 0;
    int height = 0;
    int subsampling = 0;
    int colorspace = 0;
    if (tjDecompressHeader3(
            handle.get(),
            const_cast<uint8_t*>(encoded),
            static_cast<unsigned long>(encoded_size),
            &width,
            &height,
            &subsampling,
            &colorspace) != 0) {
        return ImageStatus::decode_error;
    }
    const ImageStatus dimensions = validate_dimensions(width, height);
    if (dimensions != ImageStatus::ok) {
        return dimensions;
    }
    const size_t pixel_count = static_cast<size_t>(width) * height;
    const bool is_cmyk = colorspace == TJCS_CMYK || colorspace == TJCS_YCCK;
    std::vector<uint8_t> decoded(pixel_count * (is_cmyk ? 4u : 3u));
    if (tjDecompress2(
            handle.get(),
            const_cast<uint8_t*>(encoded),
            static_cast<unsigned long>(encoded_size),
            decoded.data(),
            width,
            0,
            height,
            is_cmyk ? TJPF_CMYK : TJPF_RGB,
            0) != 0) {
        return ImageStatus::decode_error;
    }
    if (!is_cmyk) return rgb_to_gray(decoded.data(), width, height, out);
    std::vector<uint8_t> rgb(pixel_count * 3u);
    for (size_t i = 0; i < pixel_count; ++i) {
        const uint8_t* cmyk = decoded.data() + i * 4u;
        uint8_t* pixel = rgb.data() + i * 3u;
        pixel[0] = static_cast<uint8_t>(
            (static_cast<unsigned>(cmyk[0]) * cmyk[3] + 127) / 255);
        pixel[1] = static_cast<uint8_t>(
            (static_cast<unsigned>(cmyk[1]) * cmyk[3] + 127) / 255);
        pixel[2] = static_cast<uint8_t>(
            (static_cast<unsigned>(cmyk[2]) * cmyk[3] + 127) / 255);
    }
    return rgb_to_gray(rgb.data(), width, height, out);
}

struct PngState {
    const uint8_t* data;
    size_t length;
    size_t offset;
    uint8_t* rgb;
    png_bytep* rows;
    ImageStatus error_status;
    png_structp png;
    png_infop info;
};

struct PngOwner {
    PngState* state;

    ~PngOwner() noexcept {
        if (state == nullptr) return;
        if (state->png != nullptr) {
            png_destroy_read_struct(
                &state->png,
                state->info == nullptr ? nullptr : &state->info,
                nullptr);
        }
        std::free(state->rows);
        std::free(state->rgb);
        std::free(state);
    }
};

void png_read_memory(
    png_structp png,
    png_bytep destination,
    png_size_t count) {
    PngState* state = static_cast<PngState*>(png_get_io_ptr(png));
    if (state == nullptr || count > state->length - state->offset) {
        png_error(png, "truncated PNG");
        return;
    }
    std::memcpy(destination, state->data + state->offset, count);
    state->offset += count;
}

void png_error_quiet(png_structp png, png_const_charp) {
    longjmp(png_jmpbuf(png), 1);
}

void png_warning_quiet(png_structp, png_const_charp) {
}

ImageStatus decode_png(
    const uint8_t* encoded,
    size_t encoded_size,
    GrayImage* out) {
    PngState* state = static_cast<PngState*>(
        std::calloc(1, sizeof(PngState)));
    if (state == nullptr) return ImageStatus::out_of_memory;
    PngOwner owner{state};
    state->data = encoded;
    state->length = encoded_size;
    state->error_status = ImageStatus::decode_error;
    state->png = png_create_read_struct(
        PNG_LIBPNG_VER_STRING, nullptr, png_error_quiet, png_warning_quiet);
    if (state->png == nullptr) return ImageStatus::decode_error;
    state->info = png_create_info_struct(state->png);
    if (state->info == nullptr) return ImageStatus::out_of_memory;
    if (setjmp(png_jmpbuf(state->png)) != 0) {
        return state->error_status;
    }
    png_set_read_fn(state->png, state, png_read_memory);
    png_read_info(state->png, state->info);
    png_uint_32 width = 0;
    png_uint_32 height = 0;
    int bit_depth = 0;
    int color_type = 0;
    png_get_IHDR(
        state->png,
        state->info,
        &width,
        &height,
        &bit_depth,
        &color_type,
        nullptr,
        nullptr,
        nullptr);
    if (validate_dimensions(width, height) != ImageStatus::ok) {
        state->error_status = ImageStatus::size_error;
        png_error(state->png, "PNG dimensions exceed limits");
    }
    if (bit_depth == 16) png_set_strip_16(state->png);
    if (color_type == PNG_COLOR_TYPE_PALETTE) png_set_palette_to_rgb(state->png);
    if (color_type == PNG_COLOR_TYPE_GRAY && bit_depth < 8) {
        png_set_expand_gray_1_2_4_to_8(state->png);
    }
    if (png_get_valid(state->png, state->info, PNG_INFO_tRNS) != 0) {
        png_set_tRNS_to_alpha(state->png);
    }
    if ((color_type & PNG_COLOR_MASK_ALPHA) != 0 ||
        png_get_valid(state->png, state->info, PNG_INFO_tRNS) != 0) {
        png_set_strip_alpha(state->png);
    }
    if (color_type == PNG_COLOR_TYPE_GRAY ||
        color_type == PNG_COLOR_TYPE_GRAY_ALPHA) {
        png_set_gray_to_rgb(state->png);
    }
    png_read_update_info(state->png, state->info);
    const size_t row_bytes = png_get_rowbytes(state->png, state->info);
    if (row_bytes != static_cast<size_t>(width) * 3) {
        png_error(state->png, "unexpected PNG row layout");
    }
    if (row_bytes > (std::numeric_limits<size_t>::max)() / height) {
        state->error_status = ImageStatus::size_error;
        png_error(state->png, "PNG byte size overflow");
    }
    state->rgb = static_cast<uint8_t*>(std::malloc(row_bytes * height));
    state->rows = static_cast<png_bytep*>(
        std::malloc(sizeof(png_bytep) * height));
    if (state->rgb == nullptr || state->rows == nullptr) {
        state->error_status = ImageStatus::out_of_memory;
        png_error(state->png, "out of memory decoding PNG");
    }
    for (png_uint_32 y = 0; y < height; ++y) {
        state->rows[y] = state->rgb + static_cast<size_t>(y) * row_bytes;
    }
    png_read_image(state->png, state->rows);
    png_read_end(state->png, nullptr);
    const ImageStatus result = rgb_to_gray(
        state->rgb,
        static_cast<int>(width),
        static_cast<int>(height),
        out);
    return result;
}

ImageStatus decode_webp(
    const uint8_t* encoded,
    size_t encoded_size,
    GrayImage* out) {
    int width = 0;
    int height = 0;
    if (WebPGetInfo(encoded, encoded_size, &width, &height) == 0) {
        return ImageStatus::decode_error;
    }
    const ImageStatus dimensions = validate_dimensions(width, height);
    if (dimensions != ImageStatus::ok) return dimensions;
    const size_t pixel_count = static_cast<size_t>(width) * height;
    std::vector<uint8_t> rgb(pixel_count * 3u);
    if (WebPDecodeRGBInto(
            encoded,
            encoded_size,
            rgb.data(),
            rgb.size(),
            width * 3) == nullptr) {
        return ImageStatus::decode_error;
    }
    return rgb_to_gray(rgb.data(), width, height, out);
}

uint16_t read_le16(const uint8_t* bytes) noexcept {
    return static_cast<uint16_t>(bytes[0]) |
        (static_cast<uint16_t>(bytes[1]) << 8);
}

uint32_t read_le32(const uint8_t* bytes) noexcept {
    return static_cast<uint32_t>(bytes[0]) |
        (static_cast<uint32_t>(bytes[1]) << 8) |
        (static_cast<uint32_t>(bytes[2]) << 16) |
        (static_cast<uint32_t>(bytes[3]) << 24);
}

bool fits_range(size_t offset, size_t count, size_t length) noexcept {
    return offset <= length && count <= length - offset;
}

bool validate_bmp(const uint8_t* encoded, size_t encoded_size) noexcept {
    if (encoded_size < 26) {
        return false;
    }
    const size_t pixel_offset = read_le32(encoded + 10);
    const uint32_t dib_size = read_le32(encoded + 14);
    if ((dib_size != 12 && dib_size != 40 && dib_size != 56 &&
         dib_size != 108 && dib_size != 124) ||
        !fits_range(14, dib_size, encoded_size) ||
        pixel_offset < 14 + dib_size || pixel_offset > encoded_size) {
        return false;
    }

    int64_t width = 0;
    int64_t height = 0;
    uint16_t planes = 0;
    uint16_t bits_per_pixel = 0;
    uint32_t compression = 0;
    uint32_t palette_entries = 0;
    size_t palette_entry_size = 0;
    if (dib_size == 12) {
        width = read_le16(encoded + 18);
        height = read_le16(encoded + 20);
        planes = read_le16(encoded + 22);
        bits_per_pixel = read_le16(encoded + 24);
        palette_entry_size = 3;
    } else {
        width = static_cast<int32_t>(read_le32(encoded + 18));
        const int32_t signed_height =
            static_cast<int32_t>(read_le32(encoded + 22));
        height = signed_height < 0
            ? -static_cast<int64_t>(signed_height)
            : signed_height;
        planes = read_le16(encoded + 26);
        bits_per_pixel = read_le16(encoded + 28);
        compression = read_le32(encoded + 30);
        palette_entries = read_le32(encoded + 46);
        palette_entry_size = 4;
    }
    const bool supported_depth =
        bits_per_pixel == 1 || bits_per_pixel == 4 ||
        bits_per_pixel == 8 || bits_per_pixel == 16 ||
        bits_per_pixel == 24 || bits_per_pixel == 32;
    if (planes != 1 || width <= 0 || height <= 0 || !supported_depth ||
        validate_dimensions(width, height) != ImageStatus::ok) {
        return false;
    }

    if (bits_per_pixel <= 8) {
        if (palette_entries == 0) palette_entries = 1u << bits_per_pixel;
        const size_t palette_bytes =
            static_cast<size_t>(palette_entries) * palette_entry_size;
        if (!fits_range(14 + dib_size, palette_bytes, pixel_offset)) {
            return false;
        }
    }
    if (compression == 0 ||
        (compression == 3 &&
         (bits_per_pixel == 16 || bits_per_pixel == 32))) {
        if (compression == 3 && dib_size == 40 &&
            !fits_range(14 + dib_size, 12, pixel_offset)) {
            return false;
        }
        const uint64_t row_bits =
            static_cast<uint64_t>(width) * bits_per_pixel;
        const uint64_t row_bytes = ((row_bits + 31) / 32) * 4;
        const uint64_t pixel_bytes =
            row_bytes * static_cast<uint64_t>(height);
        return pixel_bytes <= (std::numeric_limits<size_t>::max)() &&
            fits_range(
                pixel_offset,
                static_cast<size_t>(pixel_bytes),
                encoded_size);
    }
    return false;
}

bool validate_gif_lzw(
    const std::vector<uint8_t>& compressed,
    uint8_t minimum_code_size,
    size_t expected_pixels) noexcept {
    if (minimum_code_size < 2 || minimum_code_size > 8) return false;
    const uint16_t clear_code = static_cast<uint16_t>(1u << minimum_code_size);
    const uint16_t end_code = clear_code + 1;
    uint16_t next_code = end_code + 1;
    unsigned code_size = minimum_code_size + 1;
    std::array<uint16_t, 4096> lengths{};
    std::array<uint8_t, 4096> first{};
    for (uint16_t code = 0; code < clear_code; ++code) {
        lengths[code] = 1;
        first[code] = static_cast<uint8_t>(code);
    }
    size_t bit_offset = 0;
    size_t output_pixels = 0;
    int previous = -1;
    while (bit_offset + code_size <= compressed.size() * 8) {
        uint16_t code = 0;
        for (unsigned bit = 0; bit < code_size; ++bit) {
            const size_t absolute_bit = bit_offset + bit;
            if ((compressed[absolute_bit / 8] &
                 (1u << (absolute_bit % 8))) != 0) {
                code |= static_cast<uint16_t>(1u << bit);
            }
        }
        bit_offset += code_size;
        if (code == clear_code) {
            next_code = end_code + 1;
            code_size = minimum_code_size + 1;
            previous = -1;
            continue;
        }
        if (code == end_code) return output_pixels == expected_pixels;
        uint16_t sequence_length = 0;
        if (code < next_code && lengths[code] != 0) {
            sequence_length = lengths[code];
        } else if (code == next_code && previous >= 0) {
            if (lengths[previous] ==
                (std::numeric_limits<uint16_t>::max)()) return false;
            sequence_length = static_cast<uint16_t>(lengths[previous] + 1);
        } else {
            return false;
        }
        if (sequence_length > expected_pixels - output_pixels) return false;
        output_pixels += sequence_length;
        if (previous >= 0 && next_code < 4096) {
            if (lengths[previous] ==
                (std::numeric_limits<uint16_t>::max)()) return false;
            lengths[next_code] = static_cast<uint16_t>(lengths[previous] + 1);
            first[next_code] = first[previous];
            ++next_code;
            if (next_code == (1u << code_size) && code_size < 12) ++code_size;
        }
        previous = code;
    }
    return false;
}

bool read_gif_subblocks(
    const uint8_t* encoded,
    size_t encoded_size,
    size_t& offset,
    std::vector<uint8_t>* contents) {
    while (offset < encoded_size) {
        const size_t block_length = encoded[offset++];
        if (block_length == 0) return true;
        if (!fits_range(offset, block_length, encoded_size)) return false;
        if (contents != nullptr) {
            contents->insert(
                contents->end(),
                encoded + offset,
                encoded + offset + block_length);
        }
        offset += block_length;
    }
    return false;
}

bool validate_gif(const uint8_t* encoded, size_t encoded_size) {
    if (encoded_size < 13 ||
        (std::memcmp(encoded, "GIF87a", 6) != 0 &&
         std::memcmp(encoded, "GIF89a", 6) != 0)) {
        return false;
    }
    size_t offset = 13;
    const uint8_t logical_flags = encoded[10];
    if ((logical_flags & 0x80) != 0) {
        const size_t table_bytes = 3u * (2u << (logical_flags & 0x07));
        if (!fits_range(offset, table_bytes, encoded_size)) return false;
        offset += table_bytes;
    }
    bool saw_image = false;
    while (offset < encoded_size) {
        const uint8_t marker = encoded[offset++];
        if (marker == 0x3b) return offset == encoded_size && saw_image;
        if (marker == 0x21) {
            if (offset >= encoded_size) break;
            ++offset;
            if (!read_gif_subblocks(encoded, encoded_size, offset, nullptr)) break;
            continue;
        }
        if (marker != 0x2c || !fits_range(offset, 9, encoded_size)) break;
        const uint16_t width = read_le16(encoded + offset + 4);
        const uint16_t height = read_le16(encoded + offset + 6);
        const uint8_t image_flags = encoded[offset + 8];
        offset += 9;
        if (width == 0 || height == 0) break;
        if ((image_flags & 0x80) != 0) {
            const size_t table_bytes = 3u * (2u << (image_flags & 0x07));
            if (!fits_range(offset, table_bytes, encoded_size)) break;
            offset += table_bytes;
        }
        if (offset >= encoded_size) break;
        const uint8_t minimum_code_size = encoded[offset++];
        std::vector<uint8_t> compressed;
        if (!read_gif_subblocks(
                encoded, encoded_size, offset, &compressed) ||
            !validate_gif_lzw(
                compressed,
                minimum_code_size,
                static_cast<size_t>(width) * height)) {
            break;
        }
        saw_image = true;
    }
    return false;
}

ImageStatus decode_stb(
    const uint8_t* encoded,
    size_t encoded_size,
    GrayImage* out) {
    if (encoded_size > static_cast<size_t>((std::numeric_limits<int>::max)())) {
        return ImageStatus::size_error;
    }
    if (encoded_size >= 2 && encoded[0] == 'B' && encoded[1] == 'M' &&
        !validate_bmp(encoded, encoded_size)) {
        return ImageStatus::decode_error;
    }
    if (encoded_size >= 3 && std::memcmp(encoded, "GIF", 3) == 0 &&
        !validate_gif(encoded, encoded_size)) {
        return ImageStatus::decode_error;
    }
    int width = 0;
    int height = 0;
    const int length = static_cast<int>(encoded_size);
    if (!stb::Info(encoded, length, &width, &height)) {
        return ImageStatus::decode_error;
    }
    const ImageStatus dimensions = validate_dimensions(width, height);
    if (dimensions != ImageStatus::ok) return dimensions;
    if (encoded_size >= 18 && (encoded[2] == 2 || encoded[2] == 3)) {
        const uint16_t color_map_entries = read_le16(encoded + 5);
        const size_t color_map_bytes =
            (static_cast<size_t>(color_map_entries) * encoded[7] + 7u) / 8u;
        const size_t bytes_per_pixel =
            (static_cast<size_t>(encoded[16]) + 7u) / 8u;
        const size_t pixel_bytes = static_cast<size_t>(width) * height *
            bytes_per_pixel;
        const size_t data_offset = 18u + static_cast<size_t>(encoded[0]) +
            color_map_bytes;
        if (data_offset > encoded_size ||
            pixel_bytes > encoded_size - data_offset) {
            return ImageStatus::decode_error;
        }
    } else if (encoded_size >= 2 && encoded[0] == 'P' &&
        encoded[1] >= '1' && encoded[1] <= '6') {
        const size_t pixels = static_cast<size_t>(width) * height;
        if (encoded_size < pixels) return ImageStatus::decode_error;
    }
    struct StbDeleter {
        void operator()(uint8_t* image) const noexcept {
            if (image != nullptr) {
                stb::Free(image);
            }
        }
    };
    std::unique_ptr<uint8_t, StbDeleter> rgb(
        stb::LoadRgb(encoded, length, &width, &height));
    if (!rgb) return ImageStatus::decode_error;
    return rgb_to_gray(rgb.get(), width, height, out);
}

}  // namespace

ImageStatus DecodeImage(
    const uint8_t* encoded,
    size_t encoded_size,
    GrayImage* out) noexcept {
    if (out != nullptr) *out = GrayImage{};
    if (encoded == nullptr || encoded_size == 0 || out == nullptr) {
        return ImageStatus::invalid_argument;
    }
    try {
        ImageStatus status = ImageStatus::decode_error;
        if (encoded_size >= 3 && encoded[0] == 0xff &&
            encoded[1] == 0xd8 && encoded[2] == 0xff) {
            status = decode_jpeg(encoded, encoded_size, out);
        } else {
            static constexpr uint8_t kPngMagic[] = {
                0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a};
            if (encoded_size >= sizeof(kPngMagic) &&
                std::memcmp(encoded, kPngMagic, sizeof(kPngMagic)) == 0) {
                status = decode_png(encoded, encoded_size, out);
            } else if (encoded_size >= 12 &&
                std::memcmp(encoded, "RIFF", 4) == 0 &&
                std::memcmp(encoded + 8, "WEBP", 4) == 0) {
                status = decode_webp(encoded, encoded_size, out);
            } else {
                status = decode_stb(encoded, encoded_size, out);
            }
        }
        if (status != ImageStatus::ok) *out = GrayImage{};
        return status;
    } catch (const std::bad_alloc&) {
        *out = GrayImage{};
        return ImageStatus::out_of_memory;
    } catch (...) {
        *out = GrayImage{};
        return ImageStatus::internal_error;
    }
}

}  // namespace videocore::native
