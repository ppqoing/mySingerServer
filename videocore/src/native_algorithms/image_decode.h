#ifndef VIDEOCORE_NATIVE_ALGORITHMS_IMAGE_DECODE_H
#define VIDEOCORE_NATIVE_ALGORITHMS_IMAGE_DECODE_H

#include "native_algorithms/gray_image.h"

#include <cstddef>
#include <cstdint>

namespace videocore::native {

ImageStatus DecodeImage(
    const uint8_t* encoded,
    size_t encoded_size,
    GrayImage* out) noexcept;

}  // namespace videocore::native

#endif
