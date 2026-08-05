#ifndef VIDEOCORE_NATIVE_ALGORITHMS_GRAY_IMAGE_H
#define VIDEOCORE_NATIVE_ALGORITHMS_GRAY_IMAGE_H

#include <array>
#include <cstdint>
#include <vector>

namespace videocore::native {

enum class ImageStatus : int32_t {
    ok = 0,
    invalid_argument = -1,
    out_of_memory = -2,
    decode_error = -3,
    size_error = -4,
    internal_error = -99,
};

struct GrayImage {
    int32_t width = 0;
    int32_t height = 0;
    int32_t stride = 0;
    std::vector<uint8_t> pixels;
};

struct ImageFeatures {
    std::array<uint8_t, 32> pdq{};
    int32_t quality = 0;
    std::array<uint64_t, 9> phash_parts{};
    std::array<float, 128> sobel_hist{};
};

inline bool IsValidGrayImage(const GrayImage& image) noexcept {
    if (image.width < 8 || image.height < 8 || image.stride < image.width) {
        return false;
    }
    const uint64_t required = static_cast<uint64_t>(image.stride) *
        static_cast<uint64_t>(image.height);
    return required <= image.pixels.size();
}

}  // namespace videocore::native

#endif
