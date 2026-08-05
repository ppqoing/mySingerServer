#ifndef VIDEOCORE_SRC_NATIVE_ALGORITHMS_SHA512_H
#define VIDEOCORE_SRC_NATIVE_ALGORITHMS_SHA512_H

#include <array>
#include <cstddef>
#include <cstdint>

namespace vc::native {

class Sha512 {
public:
    using Digest = std::array<uint8_t, 64>;

    Sha512() noexcept;

    void Update(const uint8_t* data, size_t size) noexcept;
    Digest Final() noexcept;

private:
    void Transform(const uint8_t block[128]) noexcept;

    std::array<uint64_t, 8> state_{};
    std::array<uint8_t, 128> buffer_{};
    size_t buffered_ = 0u;
    uint64_t total_bytes_low_ = 0u;
    uint64_t total_bytes_high_ = 0u;
    bool finalized_ = false;
    Digest digest_{};
};

}  // namespace vc::native

#endif
