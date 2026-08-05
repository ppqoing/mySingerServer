#include <mediacore/mediacore.h>

#include <windows.h>
#include <bcrypt.h>

#include <png.h>
#include <turbojpeg.h>
#include <webp/decode.h>

#include <array>
#include <cstdarg>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <limits>
#include <new>
#include <vector>

bool mc_stb_info(const uint8_t* buf, int len, int* width, int* height, const char** reason);
uint8_t* mc_stb_load_rgb(
    const uint8_t* buf,
    int len,
    int* width,
    int* height,
    const char** reason);
void mc_stb_free(void* image);

namespace mediacore::pdq {
int hash_u8_gray(
    const uint8_t* gray,
    int32_t width,
    int32_t height,
    uint8_t out_hash[MC_PDQ256_BYTES],
    int32_t* quality);
}

struct mc_sha512 {
    BCRYPT_ALG_HANDLE algorithm = nullptr;
    BCRYPT_HASH_HANDLE hash = nullptr;
    PUCHAR hash_object = nullptr;
    bool finalized = false;
    bool failed = false;
};

namespace {

constexpr int64_t kMaxPixels = 400000000LL;
constexpr int32_t kMinSide = 8;

void clear_error(char* errbuf, size_t errbuf_len) noexcept {
    if (errbuf != nullptr && errbuf_len > 0) {
        errbuf[0] = '\0';
    }
}

void set_error(char* errbuf, size_t errbuf_len, const char* message) noexcept {
    if (errbuf == nullptr || errbuf_len == 0) {
        return;
    }

    size_t i = 0;
    while (i + 1 < errbuf_len && message[i] != '\0') {
        errbuf[i] = message[i];
        ++i;
    }
    errbuf[i] = '\0';
}

void set_errorf(char* errbuf, size_t errbuf_len, const char* format, ...) noexcept {
    if (errbuf == nullptr || errbuf_len == 0) {
        return;
    }
    va_list args;
    va_start(args, format);
    std::vsnprintf(errbuf, errbuf_len, format, args);
    va_end(args);
    errbuf[errbuf_len - 1] = '\0';
}

bool failed(NTSTATUS status) noexcept {
    return status < 0;
}

ULONG next_chunk_size(size_t remaining) noexcept {
    constexpr size_t max_chunk =
        static_cast<size_t>((std::numeric_limits<ULONG>::max)());
    return remaining > max_chunk
               ? (std::numeric_limits<ULONG>::max)()
               : static_cast<ULONG>(remaining);
}

void release_cng_resources(mc_sha512* ctx) noexcept {
    if (ctx == nullptr) {
        return;
    }
    if (ctx->hash != nullptr) {
        BCryptDestroyHash(ctx->hash);
        ctx->hash = nullptr;
    }
    if (ctx->hash_object != nullptr) {
        HeapFree(GetProcessHeap(), 0, ctx->hash_object);
        ctx->hash_object = nullptr;
    }
    if (ctx->algorithm != nullptr) {
        BCryptCloseAlgorithmProvider(ctx->algorithm, 0);
        ctx->algorithm = nullptr;
    }
}

void release_context(mc_sha512* ctx) noexcept {
    if (ctx == nullptr) {
        return;
    }
    release_cng_resources(ctx);
    delete ctx;
}

int validate_dimensions(int64_t width, int64_t height, char* errbuf, size_t errbuf_len) noexcept {
    if (width < kMinSide || height < kMinSide) {
        set_errorf(
            errbuf,
            errbuf_len,
            "image too small: %lldx%lld",
            static_cast<long long>(width),
            static_cast<long long>(height));
        return MC_ERR_SIZE;
    }
    if (width > (std::numeric_limits<int32_t>::max)() ||
        height > (std::numeric_limits<int32_t>::max)() ||
        width > kMaxPixels / height) {
        set_errorf(
            errbuf,
            errbuf_len,
            "image too large: %lldx%lld",
            static_cast<long long>(width),
            static_cast<long long>(height));
        return MC_ERR_SIZE;
    }
    return MC_OK;
}

int rgb_to_gray(
    const uint8_t* rgb,
    int width,
    int height,
    mc_image* out,
    char* errbuf,
    size_t errbuf_len) noexcept {
    const int dimensions = validate_dimensions(width, height, errbuf, errbuf_len);
    if (dimensions != MC_OK) {
        return dimensions;
    }
    const size_t pixels = static_cast<size_t>(width) * static_cast<size_t>(height);
    uint8_t* gray = static_cast<uint8_t*>(std::malloc(pixels));
    if (gray == nullptr) {
        set_error(errbuf, errbuf_len, "out of memory allocating gray plane");
        return MC_ERR_OOM;
    }
    for (size_t i = 0; i < pixels; ++i) {
        const uint8_t* pixel = rgb + i * 3;
        gray[i] = static_cast<uint8_t>(
            (77 * pixel[0] + 150 * pixel[1] + 29 * pixel[2] + 128) >> 8);
    }
    out->width = width;
    out->height = height;
    out->gray = gray;
    return MC_OK;
}

int decode_jpeg(
    const uint8_t* buf,
    size_t len,
    mc_image* out,
    char* errbuf,
    size_t errbuf_len) {
    if (len > (std::numeric_limits<unsigned long>::max)()) {
        set_error(errbuf, errbuf_len, "JPEG input is too large");
        return MC_ERR_SIZE;
    }
    tjhandle handle = tjInitDecompress();
    if (handle == nullptr) {
        set_error(errbuf, errbuf_len, "tjInitDecompress failed");
        return MC_ERR_DECODE;
    }
    int width = 0;
    int height = 0;
    int subsampling = 0;
    int colorspace = 0;
    if (tjDecompressHeader3(
            handle,
            const_cast<uint8_t*>(buf),
            static_cast<unsigned long>(len),
            &width,
            &height,
            &subsampling,
            &colorspace) != 0) {
        set_errorf(errbuf, errbuf_len, "JPEG header: %s", tjGetErrorStr2(handle));
        tjDestroy(handle);
        return MC_ERR_DECODE;
    }
    const int dimensions = validate_dimensions(width, height, errbuf, errbuf_len);
    if (dimensions != MC_OK) {
        tjDestroy(handle);
        return dimensions;
    }
    const size_t pixels = static_cast<size_t>(width) * static_cast<size_t>(height);
    std::vector<uint8_t> rgb;
    const bool is_cmyk = colorspace == TJCS_CMYK || colorspace == TJCS_YCCK;
    try {
        rgb.resize(pixels * (is_cmyk ? 4 : 3));
    } catch (const std::bad_alloc&) {
        tjDestroy(handle);
        set_error(errbuf, errbuf_len, "out of memory decoding JPEG");
        return MC_ERR_OOM;
    }
    if (tjDecompress2(
            handle,
            const_cast<uint8_t*>(buf),
            static_cast<unsigned long>(len),
            rgb.data(),
            width,
            0,
            height,
            is_cmyk ? TJPF_CMYK : TJPF_RGB,
            0) != 0) {
        set_errorf(errbuf, errbuf_len, "JPEG decode: %s", tjGetErrorStr2(handle));
        tjDestroy(handle);
        return MC_ERR_DECODE;
    }
    tjDestroy(handle);
    if (is_cmyk) {
        std::vector<uint8_t> converted;
        try {
            converted.resize(pixels * 3);
        } catch (const std::bad_alloc&) {
            set_error(errbuf, errbuf_len, "out of memory converting CMYK JPEG");
            return MC_ERR_OOM;
        }
        for (size_t i = 0; i < pixels; ++i) {
            const uint8_t* cmyk = rgb.data() + i * 4;
            uint8_t* pixel = converted.data() + i * 3;
            pixel[0] = static_cast<uint8_t>(
                (static_cast<unsigned>(cmyk[0]) * cmyk[3] + 127) / 255);
            pixel[1] = static_cast<uint8_t>(
                (static_cast<unsigned>(cmyk[1]) * cmyk[3] + 127) / 255);
            pixel[2] = static_cast<uint8_t>(
                (static_cast<unsigned>(cmyk[2]) * cmyk[3] + 127) / 255);
        }
        return rgb_to_gray(
            converted.data(),
            width,
            height,
            out,
            errbuf,
            errbuf_len);
    }
    return rgb_to_gray(rgb.data(), width, height, out, errbuf, errbuf_len);
}

struct PngState {
    const uint8_t* data;
    size_t length;
    size_t offset;
    uint8_t* rgb;
    png_bytep* rows;
    int error_code;
};

const char* png_failure_message(int error_code) noexcept {
    switch (error_code) {
        case MC_ERR_OOM:
            return "out of memory decoding PNG";
        case MC_ERR_SIZE:
            return "PNG dimensions exceed limits";
        default:
            return "PNG is corrupt or truncated";
    }
}

void png_read_memory(png_structp png, png_bytep destination, png_size_t count) {
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

int decode_png(
    const uint8_t* buf,
    size_t len,
    mc_image* out,
    char* errbuf,
    size_t errbuf_len) {
    PngState* state = static_cast<PngState*>(std::calloc(1, sizeof(PngState)));
    if (state == nullptr) {
        set_error(errbuf, errbuf_len, "out of memory creating PNG state");
        return MC_ERR_OOM;
    }
    state->data = buf;
    state->length = len;
    state->error_code = MC_ERR_DECODE;

    png_structp png = png_create_read_struct(
        PNG_LIBPNG_VER_STRING,
        nullptr,
        png_error_quiet,
        png_warning_quiet);
    if (png == nullptr) {
        std::free(state);
        set_error(errbuf, errbuf_len, "png_create_read_struct failed");
        return MC_ERR_DECODE;
    }
    png_infop info = png_create_info_struct(png);
    if (info == nullptr) {
        png_destroy_read_struct(&png, nullptr, nullptr);
        std::free(state);
        set_error(errbuf, errbuf_len, "out of memory creating PNG info");
        return MC_ERR_OOM;
    }

    if (setjmp(png_jmpbuf(png)) != 0) {
        const int error_code = state->error_code;
        std::free(state->rows);
        std::free(state->rgb);
        png_destroy_read_struct(&png, &info, nullptr);
        std::free(state);
        set_error(errbuf, errbuf_len, png_failure_message(error_code));
        return error_code;
    }

    png_set_read_fn(png, state, png_read_memory);
    png_read_info(png, info);
    png_uint_32 width = 0;
    png_uint_32 height = 0;
    int bit_depth = 0;
    int color_type = 0;
    png_get_IHDR(
        png,
        info,
        &width,
        &height,
        &bit_depth,
        &color_type,
        nullptr,
        nullptr,
        nullptr);
    if (validate_dimensions(width, height, nullptr, 0) != MC_OK) {
        state->error_code = MC_ERR_SIZE;
        png_error(png, "PNG dimensions exceed limits");
    }
    if (bit_depth == 16) {
        png_set_strip_16(png);
    }
    if (color_type == PNG_COLOR_TYPE_PALETTE) {
        png_set_palette_to_rgb(png);
    }
    if (color_type == PNG_COLOR_TYPE_GRAY && bit_depth < 8) {
        png_set_expand_gray_1_2_4_to_8(png);
    }
    if (png_get_valid(png, info, PNG_INFO_tRNS) != 0) {
        png_set_tRNS_to_alpha(png);
    }
    if ((color_type & PNG_COLOR_MASK_ALPHA) != 0 ||
        png_get_valid(png, info, PNG_INFO_tRNS) != 0) {
        png_set_strip_alpha(png);
    }
    if (color_type == PNG_COLOR_TYPE_GRAY ||
        color_type == PNG_COLOR_TYPE_GRAY_ALPHA) {
        png_set_gray_to_rgb(png);
    }
    png_read_update_info(png, info);
    const size_t row_bytes = png_get_rowbytes(png, info);
    if (row_bytes != static_cast<size_t>(width) * 3) {
        png_error(png, "unexpected PNG row layout");
    }
    if (row_bytes > (std::numeric_limits<size_t>::max)() / height) {
        state->error_code = MC_ERR_SIZE;
        png_error(png, "PNG byte size overflow");
    }
    state->rgb = static_cast<uint8_t*>(std::malloc(row_bytes * height));
    state->rows = static_cast<png_bytep*>(std::malloc(sizeof(png_bytep) * height));
    if (state->rgb == nullptr || state->rows == nullptr) {
        state->error_code = MC_ERR_OOM;
        png_error(png, "out of memory decoding PNG");
    }
    for (png_uint_32 y = 0; y < height; ++y) {
        state->rows[y] = state->rgb + static_cast<size_t>(y) * row_bytes;
    }
    png_read_image(png, state->rows);
    png_read_end(png, nullptr);
    png_destroy_read_struct(&png, &info, nullptr);

    const int result = rgb_to_gray(
        state->rgb,
        static_cast<int>(width),
        static_cast<int>(height),
        out,
        errbuf,
        errbuf_len);
    std::free(state->rows);
    std::free(state->rgb);
    std::free(state);
    return result;
}

int decode_webp(
    const uint8_t* buf,
    size_t len,
    mc_image* out,
    char* errbuf,
    size_t errbuf_len) {
    int width = 0;
    int height = 0;
    if (WebPGetInfo(buf, len, &width, &height) == 0) {
        set_error(errbuf, errbuf_len, "invalid WebP header");
        return MC_ERR_DECODE;
    }
    const int dimensions = validate_dimensions(width, height, errbuf, errbuf_len);
    if (dimensions != MC_OK) {
        return dimensions;
    }
    const size_t pixels = static_cast<size_t>(width) * static_cast<size_t>(height);
    std::vector<uint8_t> rgb;
    try {
        rgb.resize(pixels * 3);
    } catch (const std::bad_alloc&) {
        set_error(errbuf, errbuf_len, "out of memory decoding WebP");
        return MC_ERR_OOM;
    }
    if (WebPDecodeRGBInto(
            buf,
            len,
            rgb.data(),
            rgb.size(),
            width * 3) == nullptr) {
        set_error(errbuf, errbuf_len, "WebP decode failed");
        return MC_ERR_DECODE;
    }
    return rgb_to_gray(rgb.data(), width, height, out, errbuf, errbuf_len);
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

int validate_bmp(
    const uint8_t* buf,
    size_t len,
    char* errbuf,
    size_t errbuf_len) noexcept {
    if (len < 26) {
        set_error(errbuf, errbuf_len, "BMP file header is truncated");
        return MC_ERR_DECODE;
    }
    const size_t pixel_offset = read_le32(buf + 10);
    const uint32_t dib_size = read_le32(buf + 14);
    if ((dib_size != 12 && dib_size != 40 && dib_size != 56 &&
         dib_size != 108 && dib_size != 124) ||
        !fits_range(14, dib_size, len) ||
        pixel_offset < 14 + dib_size || pixel_offset > len) {
        set_error(errbuf, errbuf_len, "BMP header is invalid");
        return MC_ERR_DECODE;
    }

    int64_t width = 0;
    int64_t height = 0;
    uint16_t planes = 0;
    uint16_t bits_per_pixel = 0;
    uint32_t compression = 0;
    uint32_t palette_entries = 0;
    size_t palette_entry_size = 0;
    if (dib_size == 12) {
        width = read_le16(buf + 18);
        height = read_le16(buf + 20);
        planes = read_le16(buf + 22);
        bits_per_pixel = read_le16(buf + 24);
        palette_entry_size = 3;
    } else {
        width = static_cast<int32_t>(read_le32(buf + 18));
        const int32_t signed_height =
            static_cast<int32_t>(read_le32(buf + 22));
        height = signed_height < 0
            ? -static_cast<int64_t>(signed_height)
            : signed_height;
        planes = read_le16(buf + 26);
        bits_per_pixel = read_le16(buf + 28);
        compression = read_le32(buf + 30);
        palette_entries = read_le32(buf + 46);
        palette_entry_size = 4;
    }

    const bool supported_depth =
        bits_per_pixel == 1 || bits_per_pixel == 4 ||
        bits_per_pixel == 8 || bits_per_pixel == 16 ||
        bits_per_pixel == 24 || bits_per_pixel == 32;
    if (planes != 1 || width <= 0 || height <= 0 ||
        !supported_depth) {
        set_error(errbuf, errbuf_len, "BMP dimensions or bit depth are invalid");
        return MC_ERR_DECODE;
    }
    const int dimensions =
        validate_dimensions(width, height, errbuf, errbuf_len);
    if (dimensions != MC_OK) {
        return dimensions;
    }

    if (bits_per_pixel <= 8) {
        if (palette_entries == 0) {
            palette_entries = 1u << bits_per_pixel;
        }
        const size_t palette_bytes =
            static_cast<size_t>(palette_entries) * palette_entry_size;
        if (!fits_range(14 + dib_size, palette_bytes, pixel_offset)) {
            set_error(errbuf, errbuf_len, "BMP palette is truncated");
            return MC_ERR_DECODE;
        }
    }

    if (compression == 0 ||
        (compression == 3 &&
         (bits_per_pixel == 16 || bits_per_pixel == 32))) {
        if (compression == 3 && dib_size == 40) {
            const size_t mask_bytes = 12;
            if (!fits_range(14 + dib_size, mask_bytes, pixel_offset)) {
                set_error(errbuf, errbuf_len, "BMP bit masks are truncated");
                return MC_ERR_DECODE;
            }
        }
        const uint64_t row_bits =
            static_cast<uint64_t>(width) * bits_per_pixel;
        const uint64_t row_bytes = ((row_bits + 31) / 32) * 4;
        const uint64_t pixel_bytes = row_bytes * static_cast<uint64_t>(height);
        if (pixel_bytes > (std::numeric_limits<size_t>::max)() ||
            !fits_range(
                pixel_offset,
                static_cast<size_t>(pixel_bytes),
                len)) {
            set_error(errbuf, errbuf_len, "BMP pixel array is truncated");
            return MC_ERR_DECODE;
        }
        return MC_OK;
    }

    set_error(errbuf, errbuf_len, "unsupported BMP compression");
    return MC_ERR_DECODE;
}

bool validate_gif_lzw(
    const std::vector<uint8_t>& compressed,
    uint8_t minimum_code_size,
    size_t expected_pixels) noexcept {
    if (minimum_code_size < 2 || minimum_code_size > 8) {
        return false;
    }
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
        if (code == end_code) {
            return output_pixels == expected_pixels;
        }

        uint16_t sequence_length = 0;
        uint8_t sequence_first = 0;
        if (code < next_code && lengths[code] != 0) {
            sequence_length = lengths[code];
            sequence_first = first[code];
        } else if (code == next_code && previous >= 0) {
            if (lengths[previous] == (std::numeric_limits<uint16_t>::max)()) {
                return false;
            }
            sequence_length =
                static_cast<uint16_t>(lengths[previous] + 1);
            sequence_first = first[previous];
        } else {
            return false;
        }
        if (sequence_length > expected_pixels - output_pixels) {
            return false;
        }
        output_pixels += sequence_length;

        if (previous >= 0 && next_code < 4096) {
            if (lengths[previous] == (std::numeric_limits<uint16_t>::max)()) {
                return false;
            }
            lengths[next_code] =
                static_cast<uint16_t>(lengths[previous] + 1);
            first[next_code] = first[previous];
            ++next_code;
            if (next_code == (1u << code_size) && code_size < 12) {
                ++code_size;
            }
        }
        previous = code;
        (void)sequence_first;
    }
    return false;
}

bool read_gif_subblocks(
    const uint8_t* buf,
    size_t len,
    size_t& offset,
    std::vector<uint8_t>* contents) {
    while (offset < len) {
        const size_t block_length = buf[offset++];
        if (block_length == 0) {
            return true;
        }
        if (!fits_range(offset, block_length, len)) {
            return false;
        }
        if (contents != nullptr) {
            contents->insert(
                contents->end(),
                buf + offset,
                buf + offset + block_length);
        }
        offset += block_length;
    }
    return false;
}

int validate_gif(
    const uint8_t* buf,
    size_t len,
    char* errbuf,
    size_t errbuf_len) {
    if (len < 13 ||
        (std::memcmp(buf, "GIF87a", 6) != 0 &&
         std::memcmp(buf, "GIF89a", 6) != 0)) {
        set_error(errbuf, errbuf_len, "GIF header is invalid");
        return MC_ERR_DECODE;
    }
    size_t offset = 13;
    const uint8_t logical_flags = buf[10];
    if ((logical_flags & 0x80) != 0) {
        const size_t table_bytes =
            3u * (2u << (logical_flags & 0x07));
        if (!fits_range(offset, table_bytes, len)) {
            set_error(errbuf, errbuf_len, "GIF global color table is truncated");
            return MC_ERR_DECODE;
        }
        offset += table_bytes;
    }

    bool saw_image = false;
    while (offset < len) {
        const uint8_t marker = buf[offset++];
        if (marker == 0x3b) {
            if (offset != len || !saw_image) {
                set_error(errbuf, errbuf_len, "GIF trailer is misplaced");
                return MC_ERR_DECODE;
            }
            return MC_OK;
        }
        if (marker == 0x21) {
            if (offset >= len) {
                break;
            }
            ++offset;
            if (!read_gif_subblocks(buf, len, offset, nullptr)) {
                break;
            }
            continue;
        }
        if (marker != 0x2c || !fits_range(offset, 9, len)) {
            break;
        }
        const uint16_t width = read_le16(buf + offset + 4);
        const uint16_t height = read_le16(buf + offset + 6);
        const uint8_t image_flags = buf[offset + 8];
        offset += 9;
        if (width == 0 || height == 0) {
            break;
        }
        if ((image_flags & 0x80) != 0) {
            const size_t table_bytes =
                3u * (2u << (image_flags & 0x07));
            if (!fits_range(offset, table_bytes, len)) {
                break;
            }
            offset += table_bytes;
        }
        if (offset >= len) {
            break;
        }
        const uint8_t minimum_code_size = buf[offset++];
        std::vector<uint8_t> compressed;
        if (!read_gif_subblocks(buf, len, offset, &compressed) ||
            !validate_gif_lzw(
                compressed,
                minimum_code_size,
                static_cast<size_t>(width) * height)) {
            break;
        }
        saw_image = true;
    }
    set_error(errbuf, errbuf_len, "GIF structure or image data is truncated");
    return MC_ERR_DECODE;
}

int decode_stb(
    const uint8_t* buf,
    size_t len,
    mc_image* out,
    char* errbuf,
    size_t errbuf_len) {
    if (len > static_cast<size_t>((std::numeric_limits<int>::max)())) {
        set_error(errbuf, errbuf_len, "input is too large for stb");
        return MC_ERR_SIZE;
    }
    int width = 0;
    int height = 0;
    const char* reason = nullptr;
    if (!mc_stb_info(buf, static_cast<int>(len), &width, &height, &reason)) {
        set_errorf(errbuf, errbuf_len, "unsupported image: %s", reason == nullptr ? "unknown" : reason);
        return MC_ERR_DECODE;
    }
    const int dimensions = validate_dimensions(width, height, errbuf, errbuf_len);
    if (dimensions != MC_OK) {
        return dimensions;
    }
    if (len >= 2 && buf[0] == 'B' && buf[1] == 'M') {
        const int validation = validate_bmp(buf, len, errbuf, errbuf_len);
        if (validation != MC_OK) {
            return validation;
        }
    } else if (len >= 6 && std::memcmp(buf, "GIF8", 4) == 0) {
        const int validation = validate_gif(buf, len, errbuf, errbuf_len);
        if (validation != MC_OK) {
            return validation;
        }
    } else if (len >= 18 && (buf[2] == 2 || buf[2] == 3)) {
        const uint16_t color_map_entries =
            static_cast<uint16_t>(buf[5]) |
            (static_cast<uint16_t>(buf[6]) << 8);
        const size_t color_map_bytes =
            (static_cast<size_t>(color_map_entries) * buf[7] + 7) / 8;
        const size_t pixel_bytes =
            static_cast<size_t>(width) * static_cast<size_t>(height) *
            ((static_cast<size_t>(buf[16]) + 7) / 8);
        const size_t data_offset = 18 + static_cast<size_t>(buf[0]) + color_map_bytes;
        if (data_offset > len || pixel_bytes > len - data_offset) {
            set_error(errbuf, errbuf_len, "TGA is truncated");
            return MC_ERR_DECODE;
        }
    } else if (len >= 2 && buf[0] == 'P' && buf[1] >= '1' && buf[1] <= '6') {
        const size_t pixels = static_cast<size_t>(width) * static_cast<size_t>(height);
        if (len < pixels) {
            set_error(errbuf, errbuf_len, "PNM is truncated");
            return MC_ERR_DECODE;
        }
    }
    uint8_t* rgb = mc_stb_load_rgb(
        buf,
        static_cast<int>(len),
        &width,
        &height,
        &reason);
    if (rgb == nullptr) {
        set_errorf(errbuf, errbuf_len, "stb decode: %s", reason == nullptr ? "unknown" : reason);
        return MC_ERR_DECODE;
    }
    const int result = rgb_to_gray(rgb, width, height, out, errbuf, errbuf_len);
    mc_stb_free(rgb);
    return result;
}

}  // namespace

extern "C" MC_API const char* mc_version(void) {
    try {
        return MC_VERSION_STRING;
    } catch (...) {
        return "";
    }
}

extern "C" MC_API mc_sha512* mc_sha512_new(void) {
    mc_sha512* ctx = nullptr;
    try {
        ctx = new (std::nothrow) mc_sha512();
        if (ctx == nullptr) {
            return nullptr;
        }

        if (failed(BCryptOpenAlgorithmProvider(
                &ctx->algorithm, BCRYPT_SHA512_ALGORITHM, nullptr, 0))) {
            release_context(ctx);
            return nullptr;
        }

        ULONG object_length = 0;
        ULONG result_length = 0;
        if (failed(BCryptGetProperty(
                ctx->algorithm,
                BCRYPT_OBJECT_LENGTH,
                reinterpret_cast<PUCHAR>(&object_length),
                sizeof(object_length),
                &result_length,
                0)) ||
            result_length != sizeof(object_length) || object_length == 0) {
            release_context(ctx);
            return nullptr;
        }

        ctx->hash_object = static_cast<PUCHAR>(
            HeapAlloc(GetProcessHeap(), 0, object_length));
        if (ctx->hash_object == nullptr) {
            release_context(ctx);
            return nullptr;
        }

        if (failed(BCryptCreateHash(
                ctx->algorithm,
                &ctx->hash,
                ctx->hash_object,
                object_length,
                nullptr,
                0,
                0))) {
            release_context(ctx);
            return nullptr;
        }

        return ctx;
    } catch (...) {
        release_context(ctx);
        return nullptr;
    }
}

extern "C" MC_API void mc_sha512_free(mc_sha512* ctx) {
    try {
        release_context(ctx);
    } catch (...) {
    }
}

extern "C" MC_API int mc_sha512_update(
    mc_sha512* ctx,
    const uint8_t* data,
    size_t len,
    char* errbuf,
    size_t errbuf_len) {
    clear_error(errbuf, errbuf_len);
    try {
        if (ctx == nullptr) {
            set_error(errbuf, errbuf_len, "SHA-512 context is null");
            return MC_ERR_NULL_ARG;
        }
        if (len != 0 && data == nullptr) {
            set_error(errbuf, errbuf_len, "SHA-512 input is null");
            return MC_ERR_NULL_ARG;
        }
        if (ctx->finalized || ctx->failed) {
            set_error(errbuf, errbuf_len, "SHA-512 context is not active");
            return MC_ERR_INTERNAL;
        }
        if (len == 0) {
            return MC_OK;
        }

        size_t offset = 0;
        while (offset < len) {
            const size_t remaining = len - offset;
            const ULONG chunk = next_chunk_size(remaining);
            if (failed(BCryptHashData(
                    ctx->hash,
                    const_cast<PUCHAR>(data + offset),
                    chunk,
                    0))) {
                ctx->failed = true;
                release_cng_resources(ctx);
                set_error(errbuf, errbuf_len, "BCryptHashData failed");
                return MC_ERR_INTERNAL;
            }
            offset += static_cast<size_t>(chunk);
        }
        return MC_OK;
    } catch (...) {
        if (ctx != nullptr) {
            ctx->failed = true;
            release_cng_resources(ctx);
        }
        set_error(errbuf, errbuf_len, "unexpected SHA-512 update failure");
        return MC_ERR_INTERNAL;
    }
}

extern "C" MC_API int mc_sha512_final(
    mc_sha512* ctx,
    uint8_t out[MC_SHA512_BYTES],
    char* errbuf,
    size_t errbuf_len) {
    clear_error(errbuf, errbuf_len);
    try {
        if (ctx == nullptr) {
            set_error(errbuf, errbuf_len, "SHA-512 context is null");
            return MC_ERR_NULL_ARG;
        }
        if (out == nullptr) {
            set_error(errbuf, errbuf_len, "SHA-512 output is null");
            return MC_ERR_NULL_ARG;
        }
        if (ctx->finalized || ctx->failed) {
            set_error(errbuf, errbuf_len, "SHA-512 context is not active");
            return MC_ERR_INTERNAL;
        }

        ctx->finalized = true;
        if (failed(BCryptFinishHash(
                ctx->hash,
                out,
                MC_SHA512_BYTES,
                0))) {
            ctx->failed = true;
            release_cng_resources(ctx);
            set_error(errbuf, errbuf_len, "BCryptFinishHash failed");
            return MC_ERR_INTERNAL;
        }
        return MC_OK;
    } catch (...) {
        if (ctx != nullptr) {
            ctx->finalized = true;
            ctx->failed = true;
            release_cng_resources(ctx);
        }
        set_error(errbuf, errbuf_len, "unexpected SHA-512 final failure");
        return MC_ERR_INTERNAL;
    }
}

extern "C" MC_API int mc_decode_gray(
    const uint8_t* buf,
    size_t len,
    mc_image* out,
    char* errbuf,
    size_t errbuf_len) {
    try {
        clear_error(errbuf, errbuf_len);
        if (out != nullptr) {
            out->width = 0;
            out->height = 0;
            out->gray = nullptr;
        }
        if (buf == nullptr || out == nullptr) {
            set_error(errbuf, errbuf_len, "image input or output is null");
            return MC_ERR_NULL_ARG;
        }
        if (len >= 3 && buf[0] == 0xff && buf[1] == 0xd8 && buf[2] == 0xff) {
            return decode_jpeg(buf, len, out, errbuf, errbuf_len);
        }
        static constexpr uint8_t png_magic[] = {0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a};
        if (len >= sizeof(png_magic) && std::memcmp(buf, png_magic, sizeof(png_magic)) == 0) {
            return decode_png(buf, len, out, errbuf, errbuf_len);
        }
        if (len >= 12 && std::memcmp(buf, "RIFF", 4) == 0 &&
            std::memcmp(buf + 8, "WEBP", 4) == 0) {
            return decode_webp(buf, len, out, errbuf, errbuf_len);
        }
        return decode_stb(buf, len, out, errbuf, errbuf_len);
    } catch (const std::bad_alloc&) {
        if (out != nullptr) {
            mc_free_image(out);
        }
        set_error(errbuf, errbuf_len, "out of memory decoding image");
        return MC_ERR_OOM;
    } catch (...) {
        if (out != nullptr) {
            mc_free_image(out);
        }
        set_error(errbuf, errbuf_len, "unexpected image decode failure");
        return MC_ERR_INTERNAL;
    }
}

extern "C" MC_API void mc_free_image(mc_image* image) {
    try {
        if (image == nullptr) {
            return;
        }
        std::free(image->gray);
        image->gray = nullptr;
        image->width = 0;
        image->height = 0;
    } catch (...) {
        if (image != nullptr) {
            image->gray = nullptr;
            image->width = 0;
            image->height = 0;
        }
    }
}

extern "C" MC_API int mc_pdq256_from_gray(
    const uint8_t* gray,
    int32_t width,
    int32_t height,
    uint8_t out_hash[MC_PDQ256_BYTES],
    int32_t* out_quality,
    char* errbuf,
    size_t errbuf_len) {
    try {
        clear_error(errbuf, errbuf_len);
        if (gray == nullptr || out_hash == nullptr || out_quality == nullptr) {
            set_error(errbuf, errbuf_len, "PDQ input or output is null");
            return MC_ERR_NULL_ARG;
        }
        const int dimensions =
            validate_dimensions(width, height, errbuf, errbuf_len);
        if (dimensions != MC_OK) {
            return dimensions;
        }
        return mediacore::pdq::hash_u8_gray(
            gray,
            width,
            height,
            out_hash,
            out_quality);
    } catch (const std::bad_alloc&) {
        set_error(errbuf, errbuf_len, "out of memory computing PDQ");
        return MC_ERR_OOM;
    } catch (...) {
        set_error(errbuf, errbuf_len, "unexpected PDQ failure");
        return MC_ERR_INTERNAL;
    }
}

extern "C" MC_API int32_t mc_hamming_distance(
    const uint8_t a[MC_PDQ256_BYTES],
    const uint8_t b[MC_PDQ256_BYTES]) {
    try {
        if (a == nullptr || b == nullptr) {
            return -1;
        }
        int32_t distance = 0;
        for (size_t i = 0; i < MC_PDQ256_BYTES; ++i) {
            uint8_t value = static_cast<uint8_t>(a[i] ^ b[i]);
            while (value != 0) {
                value = static_cast<uint8_t>(value & (value - 1));
                ++distance;
            }
        }
        return distance;
    } catch (...) {
        return -1;
    }
}

extern "C" MC_API int mc_image_phase1(
    const uint8_t* buf,
    size_t len,
    uint8_t out_hash[MC_PDQ256_BYTES],
    int32_t* out_quality,
    int32_t* out_width,
    int32_t* out_height,
    char* errbuf,
    size_t errbuf_len) {
    mc_image image{};
    try {
        clear_error(errbuf, errbuf_len);
        if (buf == nullptr || out_hash == nullptr || out_quality == nullptr ||
            out_width == nullptr || out_height == nullptr) {
            set_error(errbuf, errbuf_len, "phase1 input or output is null");
            return MC_ERR_NULL_ARG;
        }
        const int decode_result =
            mc_decode_gray(buf, len, &image, errbuf, errbuf_len);
        if (decode_result != MC_OK) {
            return decode_result;
        }
        *out_width = image.width;
        *out_height = image.height;
        const int hash_result = mc_pdq256_from_gray(
            image.gray,
            image.width,
            image.height,
            out_hash,
            out_quality,
            errbuf,
            errbuf_len);
        mc_free_image(&image);
        return hash_result;
    } catch (const std::bad_alloc&) {
        mc_free_image(&image);
        set_error(errbuf, errbuf_len, "out of memory computing image phase1");
        return MC_ERR_OOM;
    } catch (...) {
        mc_free_image(&image);
        set_error(errbuf, errbuf_len, "unexpected image phase1 failure");
        return MC_ERR_INTERNAL;
    }
}

DWORD WINAPI mc_debug_crash_thread(LPVOID) {
    volatile uint32_t* invalid =
        reinterpret_cast<volatile uint32_t*>(static_cast<uintptr_t>(1));
    *invalid = 0x4d324156u;
    return 0;
}

extern "C" MC_API void mc_debug_crash(void) {
    // Trigger on an unmanaged native thread. Go's cgo entry thread installs
    // its own fault translation and would otherwise convert the AV to exit
    // code 2; a pure Win32 thread follows the OS unhandled-exception path and
    // terminates with STATUS_ACCESS_VIOLATION (0xC0000005).
    HANDLE thread = CreateThread(
        nullptr,
        0,
        mc_debug_crash_thread,
        nullptr,
        0,
        nullptr);
    if (thread == nullptr) {
        TerminateProcess(GetCurrentProcess(), 0xC0000005u);
    }
    WaitForSingleObject(thread, INFINITE);
    CloseHandle(thread);
    // The thread cannot return after the invalid write. Keep the acceptance
    // contract fail-closed if an unusual debugger swallows the exception.
    TerminateProcess(GetCurrentProcess(), 0xC0000005u);
}

extern "C" MC_API void mc_debug_sleep_ms(uint32_t milliseconds) {
    Sleep(milliseconds);
}
