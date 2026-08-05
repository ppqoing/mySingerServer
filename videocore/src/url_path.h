#pragma once

#include <cstdint>

namespace vc::detail {

inline bool IsAsciiAlpha(uint16_t value) noexcept {
    return (value >= static_cast<uint16_t>('a') &&
            value <= static_cast<uint16_t>('z')) ||
           (value >= static_cast<uint16_t>('A') &&
            value <= static_cast<uint16_t>('Z'));
}

inline bool IsUrlSchemeCharacter(uint16_t value) noexcept {
    return IsAsciiAlpha(value) ||
           (value >= static_cast<uint16_t>('0') &&
            value <= static_cast<uint16_t>('9')) ||
           value == static_cast<uint16_t>('+') ||
           value == static_cast<uint16_t>('-') ||
           value == static_cast<uint16_t>('.');
}

inline bool LooksLikeUrl(const uint16_t* path,
                         uint32_t path_units) noexcept {
    if (path_units < 2u || !IsAsciiAlpha(path[0])) {
        return false;
    }
    for (uint32_t index = 1u; index < path_units; ++index) {
        if (path[index] == static_cast<uint16_t>(':')) {
            if (index != 1u) {
                return true;
            }
            return index + 2u < path_units &&
                   path[index + 1u] == static_cast<uint16_t>('/') &&
                   path[index + 2u] == static_cast<uint16_t>('/');
        }
        if (!IsUrlSchemeCharacter(path[index])) {
            return false;
        }
    }
    return false;
}

}  // namespace vc::detail
