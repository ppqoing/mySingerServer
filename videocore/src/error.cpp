#include "error.h"

#include <algorithm>
#include <cstddef>
#include <cstring>

namespace vc::detail {
namespace {

template <typename Value>
void WriteField(vc_error* out,
                uint32_t available,
                size_t offset,
                const Value& value) noexcept {
    if (available < offset + sizeof(Value)) {
        return;
    }
    std::memcpy(reinterpret_cast<uint8_t*>(out) + offset,
                &value,
                sizeof(Value));
}

}  // namespace

void SetError(vc_error* out,
              int32_t code,
              int32_t ffmpeg_code,
              uint32_t win32_code,
              const char* message_utf8) noexcept {
    if (out == nullptr) {
        return;
    }

    const uint32_t available =
        std::min(out->struct_size, static_cast<uint32_t>(sizeof(vc_error)));
    const uint32_t abi_version = VC_ABI_VERSION;
    WriteField(out,
               available,
               offsetof(vc_error, abi_version),
               abi_version);
    WriteField(out, available, offsetof(vc_error, code), code);
    WriteField(out,
               available,
               offsetof(vc_error, ffmpeg_code),
               ffmpeg_code);
    WriteField(out,
               available,
               offsetof(vc_error, win32_code),
               win32_code);

    const size_t message_offset = offsetof(vc_error, message_utf8);
    if (available <= message_offset) {
        return;
    }
    const size_t capacity = available - message_offset;
    char* const destination =
        reinterpret_cast<char*>(out) + message_offset;
    destination[0] = '\0';
    destination[capacity - 1u] = '\0';
    if (message_utf8 == nullptr || capacity == 1u) {
        return;
    }
    const size_t copy_size =
        std::min(std::strlen(message_utf8), capacity - 1u);
    std::memcpy(destination, message_utf8, copy_size);
    destination[copy_size] = '\0';
}

}  // namespace vc::detail
