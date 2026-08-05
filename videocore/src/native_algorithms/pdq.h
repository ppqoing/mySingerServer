#ifndef VIDEOCORE_NATIVE_ALGORITHMS_PDQ_H
#define VIDEOCORE_NATIVE_ALGORITHMS_PDQ_H

#include "native_algorithms/gray_image.h"

#include <array>
#include <cstdint>

namespace videocore::native {

ImageStatus ComputePdq(
    const GrayImage& image,
    std::array<uint8_t, 32>* out_hash,
    int32_t* out_quality) noexcept;

}  // namespace videocore::native

#endif
