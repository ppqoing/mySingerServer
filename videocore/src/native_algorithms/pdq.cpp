#include "native_algorithms/pdq.h"

#include <pdq/cpp/hashing/pdqhashing.h>

#include <cstddef>
#include <new>
#include <vector>

namespace videocore::native {

ImageStatus ComputePdq(
    const GrayImage& image,
    std::array<uint8_t, 32>* out_hash,
    int32_t* out_quality) noexcept {
    if (out_hash != nullptr) out_hash->fill(0);
    if (out_quality != nullptr) *out_quality = 0;
    if (out_hash == nullptr || out_quality == nullptr ||
        !IsValidGrayImage(image)) {
        return ImageStatus::invalid_argument;
    }
    try {
        using namespace facebook::pdq::hashing;
        const size_t pixels = static_cast<size_t>(image.width) * image.height;
        std::vector<float> full_buffer_1(pixels);
        std::vector<float> full_buffer_2(pixels);
        fillFloatLumaFromGrey(
            const_cast<uint8_t*>(image.pixels.data()),
            image.height,
            image.width,
            image.stride,
            1,
            full_buffer_1.data());
        float buffer_64x64[64][64];
        float buffer_16x64[16][64];
        float buffer_16x16[16][16];
        Hash256 hash;
        int quality = 0;
        pdqHash256FromFloatLuma(
            full_buffer_1.data(),
            full_buffer_2.data(),
            image.height,
            image.width,
            buffer_64x64,
            buffer_16x64,
            buffer_16x16,
            hash,
            quality);
        for (int i = 0; i < HASH256_NUM_WORDS; ++i) {
            const uint16_t word = static_cast<uint16_t>(
                hash.w[HASH256_NUM_WORDS - 1 - i]);
            (*out_hash)[i * 2] = static_cast<uint8_t>(word >> 8);
            (*out_hash)[i * 2 + 1] = static_cast<uint8_t>(word);
        }
        *out_quality = quality;
        return ImageStatus::ok;
    } catch (const std::bad_alloc&) {
        return ImageStatus::out_of_memory;
    } catch (...) {
        out_hash->fill(0);
        *out_quality = 0;
        return ImageStatus::internal_error;
    }
}

}  // namespace videocore::native
