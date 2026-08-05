#define WIN32_LEAN_AND_MEAN
#include <windows.h>

#include "contact_sheet.h"

#include <turbojpeg.h>

#include <algorithm>
#include <array>
#include <atomic>
#include <cstddef>
#include <cstdint>
#include <cstdlib>
#include <limits>
#include <memory>
#include <new>
#include <string>

extern "C" {
#include <libavutil/pixfmt.h>
#include <libswscale/swscale.h>
}

#include "native_algorithms/pdq.h"

namespace vc::detail {

namespace {

using GrayImage = videocore::native::GrayImage;
using ImageFeatures = videocore::native::ImageFeatures;
using ImageStatus = videocore::native::ImageStatus;

constexpr uint32_t kDefaultTileMaxSide = 256u;
constexpr uint64_t kMaxContactWorkingSetBytes = 256ull * 1024ull * 1024ull;
constexpr uint64_t kContactWorkingSetSafetyBytes = 8ull * 1024ull * 1024ull;
constexpr uint8_t kPlaceholderBackground = 96u;
constexpr uint8_t kPlaceholderLine = 192u;
// FFmpeg q:v=3 expresses a high-quality visually near-lossless JPEG intent.
// libjpeg-turbo's 0..100 scale has no exact algebraic mapping; 90 is the
// named deterministic equivalent used by the fixed Task 9 fixture.
constexpr int kContactSheetJpegQuality = 90;

#if defined(VC_RESILIENCE_TESTING)
std::atomic<uint64_t> contact_live_sws{0u};
std::atomic<uint64_t> contact_live_turbo{0u};
std::atomic<uint64_t> contact_live_buffers{0u};
std::atomic<uint64_t> contact_live_handles{0u};
std::atomic<uint64_t> contact_acquired_sws{0u};
std::atomic<uint64_t> contact_acquired_turbo{0u};
std::atomic<uint64_t> contact_acquired_buffers{0u};
std::atomic<uint64_t> contact_acquired_handles{0u};
#endif

struct SwsDeleter {
    void operator()(SwsContext* context) const noexcept {
#if defined(VC_RESILIENCE_TESTING)
        if (context != nullptr) {
            contact_live_sws.fetch_sub(1u, std::memory_order_acq_rel);
        }
#endif
        sws_freeContext(context);
    }
};

struct TurboDeleter {
    void operator()(void* handle) const noexcept {
        if (handle != nullptr) tjDestroy(handle);
#if defined(VC_RESILIENCE_TESTING)
        if (handle != nullptr) {
            contact_live_turbo.fetch_sub(1u, std::memory_order_acq_rel);
        }
#endif
    }
};

struct TurboBufferDeleter {
    void operator()(unsigned char* buffer) const noexcept {
        if (buffer != nullptr) tjFree(buffer);
#if defined(VC_RESILIENCE_TESTING)
        if (buffer != nullptr) {
            contact_live_buffers.fetch_sub(1u, std::memory_order_acq_rel);
        }
#endif
    }
};

struct HandleDeleter {
    void operator()(void* handle) const noexcept {
        if (handle != nullptr && handle != INVALID_HANDLE_VALUE) {
            CloseHandle(static_cast<HANDLE>(handle));
#if defined(VC_RESILIENCE_TESTING)
            contact_live_handles.fetch_sub(1u, std::memory_order_acq_rel);
#endif
        }
    }
};

using SwsOwner = std::unique_ptr<SwsContext, SwsDeleter>;
using TurboOwner = std::unique_ptr<void, TurboDeleter>;
using TurboBuffer = std::unique_ptr<unsigned char, TurboBufferDeleter>;
using HandleOwner = std::unique_ptr<void, HandleDeleter>;

void ResetResult(ContactSheetResult* out, int32_t state) noexcept {
    if (out == nullptr) return;
    out->state = state;
    out->successful_mask = 0u;
    out->placeholder_mask = 0u;
    out->width = 0;
    out->height = 0;
    out->tile_width = 0;
    out->tile_height = 0;
    out->canvas = {};
    out->features = {};
}

bool ValidSource(const GrayImage* image) noexcept {
    if (image == nullptr || image->width <= 0 || image->height <= 0 ||
        image->stride < image->width) {
        return false;
    }
    const uint64_t required =
        static_cast<uint64_t>(image->stride) * image->height;
    return required <= image->pixels.size();
}

bool SafeCanvasSize(int32_t tile_width,
                    int32_t tile_height,
                    int32_t* canvas_width,
                    int32_t* canvas_height,
                    size_t* canvas_bytes) noexcept {
    if (tile_width <= 0 || tile_height <= 0 || canvas_width == nullptr ||
        canvas_height == nullptr || canvas_bytes == nullptr) {
        return false;
    }
    const uint64_t width = static_cast<uint64_t>(tile_width) * 3u;
    const uint64_t height = static_cast<uint64_t>(tile_height) * 2u;
    const uint64_t bytes = width * height;
    if (width > static_cast<uint64_t>((std::numeric_limits<int32_t>::max)()) ||
        height > static_cast<uint64_t>((std::numeric_limits<int32_t>::max)()) ||
        bytes > static_cast<uint64_t>((std::numeric_limits<size_t>::max)())) {
        return false;
    }
    const uint64_t feature_width = (std::max)(width, uint64_t{8});
    const uint64_t feature_height = (std::max)(height, uint64_t{8});
    if (feature_width > (std::numeric_limits<uint64_t>::max)() /
                            feature_height) {
        return false;
    }
    const uint64_t feature_pixels = feature_width * feature_height;
    if (feature_pixels > (std::numeric_limits<uint64_t>::max)() /
                             (sizeof(float) * 2u)) {
        return false;
    }
    const uint64_t pdq_bytes =
        feature_pixels * sizeof(float) * 2u;
    const uint64_t feature_gray_bytes =
        (width < 8u || height < 8u) ? feature_pixels : 0u;
    const unsigned long jpeg_bytes = tjBufSize(
        static_cast<int>(width), static_cast<int>(height), TJSAMP_GRAY);
    if (jpeg_bytes == (std::numeric_limits<unsigned long>::max)()) {
        return false;
    }
    uint64_t total = kContactWorkingSetSafetyBytes;
    const auto add_to_budget = [&](uint64_t value) noexcept {
        if (value > kMaxContactWorkingSetBytes - total) return false;
        total += value;
        return true;
    };
    if (!add_to_budget(bytes) || !add_to_budget(feature_gray_bytes) ||
        !add_to_budget(pdq_bytes) ||
        !add_to_budget(jpeg_bytes)) {
        return false;
    }
    *canvas_width = static_cast<int32_t>(width);
    *canvas_height = static_cast<int32_t>(height);
    *canvas_bytes = static_cast<size_t>(bytes);
    return true;
}

bool FitInsideTile(int32_t source_width,
                   int32_t source_height,
                   int32_t tile_width,
                   int32_t tile_height,
                   int32_t* fitted_width,
                   int32_t* fitted_height) noexcept {
    if (source_width <= 0 || source_height <= 0 || tile_width <= 0 ||
        tile_height <= 0 || fitted_width == nullptr ||
        fitted_height == nullptr) {
        return false;
    }
    const uint64_t width_limited_height =
        (static_cast<uint64_t>(source_height) * tile_width +
         static_cast<uint64_t>(source_width) / 2u) /
        static_cast<uint64_t>(source_width);
    if (width_limited_height <= static_cast<uint64_t>(tile_height)) {
        *fitted_width = tile_width;
        *fitted_height = static_cast<int32_t>((std::max)(
            uint64_t{1}, width_limited_height));
        return true;
    }
    const uint64_t height_limited_width =
        (static_cast<uint64_t>(source_width) * tile_height +
         static_cast<uint64_t>(source_height) / 2u) /
        static_cast<uint64_t>(source_height);
    *fitted_height = tile_height;
    *fitted_width = static_cast<int32_t>((std::max)(
        uint64_t{1}, height_limited_width));
    return true;
}

int32_t ScaleIntoTile(const GrayImage& source,
                      int32_t tile_width,
                      int32_t tile_height,
                      GrayImage* canvas,
                      int32_t tile_x,
                      int32_t tile_y) noexcept {
    int32_t fitted_width = 0;
    int32_t fitted_height = 0;
    if (!FitInsideTile(source.width, source.height,
                       tile_width, tile_height,
                       &fitted_width, &fitted_height)) {
        return VC_ERR_INVALID_ARG;
    }
    const int32_t offset_x = tile_x + (tile_width - fitted_width) / 2;
    const int32_t offset_y = tile_y + (tile_height - fitted_height) / 2;
    SwsOwner scaler(sws_getContext(source.width,
                                   source.height,
                                   AV_PIX_FMT_GRAY8,
                                   fitted_width,
                                   fitted_height,
                                   AV_PIX_FMT_GRAY8,
                                   SWS_BICUBIC,
                                   nullptr,
                                   nullptr,
                                   nullptr));
    if (!scaler) return VC_ERR_ENCODE;
#if defined(VC_RESILIENCE_TESTING)
    contact_live_sws.fetch_add(1u, std::memory_order_acq_rel);
    contact_acquired_sws.fetch_add(1u, std::memory_order_acq_rel);
#endif
    const uint8_t* source_planes[4]{
        source.pixels.data(), nullptr, nullptr, nullptr};
    int source_strides[4]{source.stride, 0, 0, 0};
    uint8_t* destination_planes[4]{
        canvas->pixels.data() +
            static_cast<size_t>(offset_y) * canvas->stride + offset_x,
        nullptr, nullptr, nullptr};
    int destination_strides[4]{canvas->stride, 0, 0, 0};
    return sws_scale(scaler.get(),
                     source_planes,
                     source_strides,
                     0,
                     source.height,
                     destination_planes,
                     destination_strides) == fitted_height
               ? VC_OK
               : VC_ERR_ENCODE;
}

void DrawPlaceholder(GrayImage* canvas,
                     int32_t tile_x,
                     int32_t tile_y,
                     int32_t tile_width,
                     int32_t tile_height) noexcept {
    const int32_t line_width =
        (std::min)(tile_width, tile_height) < 64 ? 1 : 2;
    const auto plot = [&](int32_t x, int32_t y) noexcept {
        const int32_t brush_x = (std::min)(
            (std::max)(x, 0), tile_width - line_width);
        const int32_t brush_y = (std::min)(
            (std::max)(y, 0), tile_height - line_width);
        for (int32_t dy = 0; dy < line_width; ++dy) {
            for (int32_t dx = 0; dx < line_width; ++dx) {
                canvas->pixels[
                    static_cast<size_t>(tile_y + brush_y + dy) *
                        canvas->stride +
                    tile_x + brush_x + dx] = kPlaceholderLine;
            }
        }
    };
    const auto draw_line = [&](int32_t start_x,
                               int32_t start_y,
                               int32_t end_x,
                               int32_t end_y) noexcept {
        int32_t x = start_x;
        int32_t y = start_y;
        const int32_t dx = std::abs(end_x - start_x);
        const int32_t sx = start_x < end_x ? 1 : -1;
        const int32_t dy = -std::abs(end_y - start_y);
        const int32_t sy = start_y < end_y ? 1 : -1;
        int32_t error = dx + dy;
        for (;;) {
            plot(x, y);
            if (x == end_x && y == end_y) break;
            const int32_t twice_error = error * 2;
            if (twice_error >= dy) {
                error += dy;
                x += sx;
            }
            if (twice_error <= dx) {
                error += dx;
                y += sy;
            }
        }
    };
    draw_line(0, 0, tile_width - 1, tile_height - 1);
    draw_line(tile_width - 1, 0, 0, tile_height - 1);
}

bool ValidContactCanvas(const GrayImage& canvas) noexcept {
    if (canvas.width <= 0 || canvas.height <= 0 ||
        canvas.stride < canvas.width) {
        return false;
    }
    const uint64_t required =
        static_cast<uint64_t>(canvas.stride) * canvas.height;
    return required <= canvas.pixels.size();
}

int32_t ComputeContactFeatures(const GrayImage& canvas,
                               ImageFeatures* out) noexcept {
    if (out == nullptr) return VC_ERR_INVALID_ARG;
    *out = {};
    if (!ValidContactCanvas(canvas)) return VC_ERR_INVALID_ARG;
    const GrayImage* feature_image = &canvas;
    GrayImage padded;
    if (canvas.width < 8 || canvas.height < 8) {
        try {
            padded.width = (std::max)(canvas.width, 8);
            padded.height = (std::max)(canvas.height, 8);
            padded.stride = padded.width;
            padded.pixels.assign(
                static_cast<size_t>(padded.width) * padded.height,
                kPlaceholderBackground);
            const int32_t offset_x = (padded.width - canvas.width) / 2;
            const int32_t offset_y = (padded.height - canvas.height) / 2;
            for (int32_t y = 0; y < canvas.height; ++y) {
                std::copy_n(
                    canvas.pixels.data() +
                        static_cast<size_t>(y) * canvas.stride,
                    canvas.width,
                    padded.pixels.data() +
                        static_cast<size_t>(offset_y + y) * padded.stride +
                        offset_x);
            }
            feature_image = &padded;
        } catch (const std::bad_alloc&) {
            return VC_ERR_OOM;
        } catch (...) {
            return VC_ERR_INTERNAL;
        }
    }
    const ImageStatus status =
        videocore::native::ComputePdq(
            *feature_image, &out->pdq, &out->quality);
    switch (status) {
        case ImageStatus::ok: return VC_OK;
        case ImageStatus::invalid_argument: return VC_ERR_INVALID_ARG;
        case ImageStatus::out_of_memory: return VC_ERR_OOM;
        case ImageStatus::decode_error:
        case ImageStatus::size_error: return VC_ERR_ENCODE;
        case ImageStatus::internal_error: return VC_ERR_INTERNAL;
    }
    return VC_ERR_INTERNAL;
}

bool ValidUtf16Path(const uint16_t* path, uint32_t units) noexcept {
    if (path == nullptr || units == 0u) return false;
    return std::find(path, path + units, uint16_t{0}) == path + units;
}

}  // namespace

int32_t ContactSheetTileDimensions(int32_t source_width,
                                   int32_t source_height,
                                   uint32_t tile_max_side,
                                   int32_t* tile_width,
                                   int32_t* tile_height) noexcept {
    if (source_width <= 0 || source_height <= 0 || tile_width == nullptr ||
        tile_height == nullptr) {
        return VC_ERR_INVALID_ARG;
    }
    const uint32_t maximum =
        tile_max_side == 0u ? kDefaultTileMaxSide : tile_max_side;
    if (maximum > static_cast<uint32_t>(
                      (std::numeric_limits<int32_t>::max)())) {
        return VC_ERR_OUTPUT_TOO_LARGE;
    }
    if (source_width >= source_height) {
        *tile_width = static_cast<int32_t>(maximum);
        const uint64_t rounded =
            (static_cast<uint64_t>(source_height) * maximum +
             static_cast<uint64_t>(source_width) / 2u) /
            static_cast<uint64_t>(source_width);
        *tile_height = static_cast<int32_t>((std::max)(uint64_t{1}, rounded));
    } else {
        *tile_height = static_cast<int32_t>(maximum);
        const uint64_t rounded =
            (static_cast<uint64_t>(source_width) * maximum +
             static_cast<uint64_t>(source_height) / 2u) /
            static_cast<uint64_t>(source_height);
        *tile_width = static_cast<int32_t>((std::max)(uint64_t{1}, rounded));
    }
    int32_t canvas_width = 0;
    int32_t canvas_height = 0;
    size_t bytes = 0u;
    if (!SafeCanvasSize(*tile_width, *tile_height,
                        &canvas_width, &canvas_height, &bytes)) {
        *tile_width = 0;
        *tile_height = 0;
        return VC_ERR_OUTPUT_TOO_LARGE;
    }
    return VC_OK;
}

int32_t BuildContactSheet(
    const std::array<const GrayImage*, VC_VIDEO_FRAME_COUNT>& frames,
    uint32_t tile_max_side,
    ContactSheetResult* out,
    const CancelState* cancel,
    Deadline deadline) noexcept {
    if (out == nullptr) return VC_ERR_INVALID_ARG;
    ResetResult(out, VC_ERR_NO_FRAME);
    const GrayImage* authority = nullptr;
    for (const GrayImage* frame : frames) {
        if (ValidSource(frame)) {
            authority = frame;
            break;
        }
    }
    if (authority == nullptr) return VC_ERR_NO_FRAME;

    int32_t tile_width = 0;
    int32_t tile_height = 0;
    const int32_t dimensions = ContactSheetTileDimensions(
        authority->width, authority->height, tile_max_side,
        &tile_width, &tile_height);
    if (dimensions != VC_OK) {
        ResetResult(out, dimensions);
        return dimensions;
    }
    int32_t canvas_width = 0;
    int32_t canvas_height = 0;
    size_t canvas_bytes = 0u;
    if (!SafeCanvasSize(tile_width, tile_height,
                        &canvas_width, &canvas_height, &canvas_bytes)) {
        ResetResult(out, VC_ERR_OUTPUT_TOO_LARGE);
        return VC_ERR_OUTPUT_TOO_LARGE;
    }
    try {
        out->canvas.width = canvas_width;
        out->canvas.height = canvas_height;
        out->canvas.stride = canvas_width;
        out->canvas.pixels.assign(canvas_bytes, kPlaceholderBackground);
    } catch (const std::bad_alloc&) {
        ResetResult(out, VC_ERR_OOM);
        return VC_ERR_OOM;
    } catch (...) {
        ResetResult(out, VC_ERR_INTERNAL);
        return VC_ERR_INTERNAL;
    }

    for (uint32_t index = 0u; index < VC_VIDEO_FRAME_COUNT; ++index) {
        const int32_t tile_x = static_cast<int32_t>(index % 3u) * tile_width;
        const int32_t tile_y = static_cast<int32_t>(index / 3u) * tile_height;
        if (ValidSource(frames[index])) {
            const int32_t scale = ScaleIntoTile(
                *frames[index], tile_width, tile_height,
                &out->canvas, tile_x, tile_y);
            if (scale != VC_OK) {
                ResetResult(out, scale);
                return scale;
            }
            out->successful_mask |= 1u << index;
        } else {
            DrawPlaceholder(&out->canvas, tile_x, tile_y,
                            tile_width, tile_height);
            out->placeholder_mask |= 1u << index;
        }
    }
    const int32_t before_feature = CheckOperationBoundary(
        cancel, deadline, OperationBoundary::feature);
    if (before_feature != VC_OK) {
        ResetResult(out, before_feature);
        return before_feature;
    }
    const int32_t feature_status =
        ComputeContactFeatures(out->canvas, &out->features);
    if (feature_status != VC_OK) {
        ResetResult(out, feature_status);
        return feature_status;
    }
    out->state = VC_OK;
    out->width = canvas_width;
    out->height = canvas_height;
    out->tile_width = tile_width;
    out->tile_height = tile_height;
    return VC_OK;
}

int32_t WriteContactSheetJpeg(const GrayImage& canvas,
                              const uint16_t* temporary_path,
                              uint32_t temporary_path_units,
                              const CancelState* cancel,
                              Deadline deadline) noexcept {
    if (!ValidUtf16Path(temporary_path, temporary_path_units) ||
        !ValidContactCanvas(canvas)) {
        return VC_ERR_INVALID_ARG;
    }
    const int32_t before_encode = CheckOperationBoundary(
        cancel, deadline, OperationBoundary::jpeg_encode);
    if (before_encode != VC_OK) return before_encode;
    TurboOwner compressor(tjInitCompress());
    if (!compressor) return VC_ERR_ENCODE;
#if defined(VC_RESILIENCE_TESTING)
    contact_live_turbo.fetch_add(1u, std::memory_order_acq_rel);
    contact_acquired_turbo.fetch_add(1u, std::memory_order_acq_rel);
#endif
    unsigned char* encoded_raw = nullptr;
    unsigned long encoded_size = 0u;
    const int encoded = tjCompress2(compressor.get(),
                                    canvas.pixels.data(),
                                    canvas.width,
                                    canvas.stride,
                                    canvas.height,
                                    TJPF_GRAY,
                                    &encoded_raw,
                                    &encoded_size,
                                    TJSAMP_GRAY,
                                    kContactSheetJpegQuality,
                                    TJFLAG_ACCURATEDCT);
    TurboBuffer encoded_owner(encoded_raw);
#if defined(VC_RESILIENCE_TESTING)
    if (encoded_raw != nullptr) {
        contact_live_buffers.fetch_add(1u, std::memory_order_acq_rel);
        contact_acquired_buffers.fetch_add(1u, std::memory_order_acq_rel);
    }
#endif
    if (encoded != 0 || encoded_raw == nullptr || encoded_size == 0u) {
        return VC_ERR_ENCODE;
    }

    std::wstring path;
    try {
        path.assign(reinterpret_cast<const wchar_t*>(temporary_path),
                    temporary_path_units);
    } catch (const std::bad_alloc&) {
        return VC_ERR_OOM;
    } catch (...) {
        return VC_ERR_INTERNAL;
    }
    HandleOwner file(CreateFileW(path.c_str(),
                                 GENERIC_WRITE,
                                 0,
                                 nullptr,
                                 CREATE_NEW,
                                 FILE_ATTRIBUTE_NORMAL,
                                 nullptr));
    if (file.get() == INVALID_HANDLE_VALUE) {
        file.release();
        return VC_ERR_IO;
    }
#if defined(VC_RESILIENCE_TESTING)
    contact_live_handles.fetch_add(1u, std::memory_order_acq_rel);
    contact_acquired_handles.fetch_add(1u, std::memory_order_acq_rel);
#endif
    size_t offset = 0u;
    bool write_ok = true;
    int32_t write_interrupt = VC_OK;
    while (offset < encoded_size) {
        write_interrupt = CheckOperationBoundary(
            cancel, deadline, OperationBoundary::jpeg_encode);
        if (write_interrupt != VC_OK) {
            write_ok = false;
            break;
        }
        const DWORD remaining = static_cast<DWORD>((std::min)(
            static_cast<unsigned long>(MAXDWORD),
            encoded_size - static_cast<unsigned long>(offset)));
        DWORD written = 0u;
        if (!WriteFile(static_cast<HANDLE>(file.get()),
                       encoded_raw + offset,
                       remaining,
                       &written,
                       nullptr) || written == 0u) {
            write_ok = false;
            break;
        }
        offset += written;
    }
    file.reset();
    if (!write_ok) {
        DeleteFileW(path.c_str());
        return write_interrupt == VC_OK ? VC_ERR_IO : write_interrupt;
    }
    return VC_OK;
}

int32_t GenerateContactSheet(
    const std::array<const GrayImage*, VC_VIDEO_FRAME_COUNT>& frames,
    uint32_t tile_max_side,
    const uint16_t* temporary_path,
    uint32_t temporary_path_units,
    ContactSheetResult* out,
    const CancelState* cancel,
    Deadline deadline) noexcept {
    if (out == nullptr) return VC_ERR_INVALID_ARG;
    if (!ValidUtf16Path(temporary_path, temporary_path_units)) {
        ResetResult(out, VC_ERR_INVALID_ARG);
        return VC_ERR_INVALID_ARG;
    }
    const int32_t build = BuildContactSheet(
        frames, tile_max_side, out, cancel, deadline);
    if (build != VC_OK) return build;
    const int32_t write = WriteContactSheetJpeg(
        out->canvas, temporary_path, temporary_path_units,
        cancel, deadline);
    if (write != VC_OK) {
        ResetResult(out, write);
        return write;
    }
    return VC_OK;
}

#if defined(VC_RESILIENCE_TESTING)
uint64_t ContactSheetTestLiveResourceCount() noexcept {
    return contact_live_sws.load(std::memory_order_acquire) +
           contact_live_turbo.load(std::memory_order_acquire) +
           contact_live_buffers.load(std::memory_order_acquire) +
           contact_live_handles.load(std::memory_order_acquire);
}

ContactSheetResourceAcquisitions
ContactSheetTestResourceAcquisitions() noexcept {
    ContactSheetResourceAcquisitions resources;
    resources.scalers =
        contact_acquired_sws.load(std::memory_order_acquire);
    resources.turbo_compressors =
        contact_acquired_turbo.load(std::memory_order_acquire);
    resources.turbo_buffers =
        contact_acquired_buffers.load(std::memory_order_acquire);
    resources.jpeg_handles =
        contact_acquired_handles.load(std::memory_order_acquire);
    return resources;
}
#endif

}  // namespace vc::detail
