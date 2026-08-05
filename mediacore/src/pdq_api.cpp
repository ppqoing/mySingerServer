#include <mediacore/mediacore.h>

#include <pdq/cpp/hashing/pdqhashing.h>

#include <cstddef>
#include <cstdint>
#include <new>
#include <vector>

namespace mediacore::pdq {

int hash_u8_gray(
    const uint8_t* gray,
    int32_t width,
    int32_t height,
    uint8_t out_hash[MC_PDQ256_BYTES],
    int32_t* quality) {
    using namespace facebook::pdq::hashing;

    const size_t pixels = static_cast<size_t>(width) * static_cast<size_t>(height);
    std::vector<float> full_buffer_1(pixels);
    std::vector<float> full_buffer_2(pixels);

    fillFloatLumaFromGrey(
        const_cast<uint8_t*>(gray),
        height,
        width,
        width,
        1,
        full_buffer_1.data());

    float buffer_64x64[64][64];
    float buffer_16x64[16][64];
    float buffer_16x16[16][16];
    Hash256 hash;
    int upstream_quality = 0;
    pdqHash256FromFloatLuma(
        full_buffer_1.data(),
        full_buffer_2.data(),
        height,
        width,
        buffer_64x64,
        buffer_16x64,
        buffer_16x16,
        hash,
        upstream_quality);

    for (int i = 0; i < HASH256_NUM_WORDS; ++i) {
        const uint16_t word = static_cast<uint16_t>(
            hash.w[HASH256_NUM_WORDS - 1 - i]);
        out_hash[i * 2] = static_cast<uint8_t>(word >> 8);
        out_hash[i * 2 + 1] = static_cast<uint8_t>(word);
    }
    *quality = upstream_quality;
    return MC_OK;
}

}  // namespace mediacore::pdq
