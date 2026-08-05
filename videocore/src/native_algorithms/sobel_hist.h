#ifndef VIDEOCORE_NATIVE_ALGORITHMS_SOBEL_HIST_H
#define VIDEOCORE_NATIVE_ALGORITHMS_SOBEL_HIST_H

#include "native_algorithms/gray_image.h"

#include <array>

namespace videocore::native {

ImageStatus ComputeSobelHistogram(
    const GrayImage& image,
    std::array<float, 128>* out_histogram) noexcept;

}  // namespace videocore::native

#endif
