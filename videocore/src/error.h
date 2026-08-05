#ifndef VIDEOCORE_SRC_ERROR_H
#define VIDEOCORE_SRC_ERROR_H

#include <cstdint>
#include <exception>
#include <new>
#include <utility>

#include "videocore/videocore.h"

namespace vc::detail {

void SetError(vc_error* out,
              int32_t code,
              int32_t ffmpeg_code,
              uint32_t win32_code,
              const char* message_utf8) noexcept;

template <typename Callable>
int32_t Guard(vc_error* out, Callable&& callable) noexcept {
    try {
        return std::forward<Callable>(callable)();
    } catch (const std::bad_alloc&) {
        SetError(out, VC_ERR_OOM, 0, 0, "out of memory");
        return VC_ERR_OOM;
    } catch (const std::exception& exception) {
        SetError(out, VC_ERR_INTERNAL, 0, 0, exception.what());
        return VC_ERR_INTERNAL;
    } catch (...) {
        SetError(out, VC_ERR_INTERNAL, 0, 0, "unknown internal exception");
        return VC_ERR_INTERNAL;
    }
}

}  // namespace vc::detail

#endif
