#ifndef VIDEOCORE_NATIVE_ALGORITHMS_RGB_IMAGE_H
#define VIDEOCORE_NATIVE_ALGORITHMS_RGB_IMAGE_H

#include <cstdint>
#include <vector>

namespace videocore::native {

struct RgbImage {
    int32_t width = 0;
    int32_t height = 0;
    int32_t stride = 0;
    std::vector<uint8_t> pixels;
};

}  // namespace videocore::native

#endif
