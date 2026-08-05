#ifndef VIDEOCORE_SRC_CONTACT_SHEET_H
#define VIDEOCORE_SRC_CONTACT_SHEET_H

#include <array>
#include <cstdint>

#include "deadline.h"
#include "native_algorithms/gray_image.h"
#include "videocore/videocore.h"

namespace vc::detail {

struct CancelState;

struct ContactSheetResult {
    int32_t state = VC_ERR_UNSUPPORTED;
    uint32_t successful_mask = 0u;
    uint32_t placeholder_mask = 0u;
    int32_t width = 0;
    int32_t height = 0;
    int32_t tile_width = 0;
    int32_t tile_height = 0;
    videocore::native::GrayImage canvas;
    videocore::native::ImageFeatures features;
};

int32_t ContactSheetTileDimensions(int32_t source_width,
                                   int32_t source_height,
                                   uint32_t tile_max_side,
                                   int32_t* tile_width,
                                   int32_t* tile_height) noexcept;

int32_t BuildContactSheet(
    const std::array<const videocore::native::GrayImage*,
                     VC_VIDEO_FRAME_COUNT>& frames,
    uint32_t tile_max_side,
    ContactSheetResult* out,
    const CancelState* cancel = nullptr,
    Deadline deadline = Deadline::Infinite()) noexcept;

int32_t WriteContactSheetJpeg(
    const videocore::native::GrayImage& canvas,
    const uint16_t* temporary_path,
    uint32_t temporary_path_units,
    const CancelState* cancel = nullptr,
    Deadline deadline = Deadline::Infinite()) noexcept;

int32_t GenerateContactSheet(
    const std::array<const videocore::native::GrayImage*,
                     VC_VIDEO_FRAME_COUNT>& frames,
    uint32_t tile_max_side,
    const uint16_t* temporary_path,
    uint32_t temporary_path_units,
    ContactSheetResult* out,
    const CancelState* cancel = nullptr,
    Deadline deadline = Deadline::Infinite()) noexcept;

#if defined(VC_RESILIENCE_TESTING)
struct ContactSheetResourceAcquisitions {
    uint64_t scalers = 0u;
    uint64_t turbo_compressors = 0u;
    uint64_t turbo_buffers = 0u;
    uint64_t jpeg_handles = 0u;
};

uint64_t ContactSheetTestLiveResourceCount() noexcept;
ContactSheetResourceAcquisitions
ContactSheetTestResourceAcquisitions() noexcept;
#endif

}  // namespace vc::detail

#endif
