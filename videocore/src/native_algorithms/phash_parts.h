#ifndef VIDEOCORE_NATIVE_ALGORITHMS_PHASH_PARTS_H
#define VIDEOCORE_NATIVE_ALGORITHMS_PHASH_PARTS_H

#include "native_algorithms/gray_image.h"

#include <array>
#include <cstdint>

namespace videocore::native {

ImageStatus ComputePHashParts(
    const GrayImage& image,
    std::array<uint64_t, 9>* out_parts) noexcept;

}  // namespace videocore::native

#endif
