#ifndef VIDEOCORE_SRC_AVIO_BRIDGE_H
#define VIDEOCORE_SRC_AVIO_BRIDGE_H

#include <cstdint>
#include <memory>

extern "C" {
#include <libavformat/avio.h>
}

#include "deadline.h"
#include "videocore/videocore.h"

namespace vc::detail {

struct CancelState;
class WinFile;

struct AvioOpaque {
    WinFile* file = nullptr;
    const CancelState* cancel = nullptr;
    Deadline deadline = Deadline::Infinite();
    int32_t last_status = VC_OK;
};

int ReadPacket(void* opaque, uint8_t* buffer, int size);
int64_t SeekPacket(void* opaque, int64_t offset, int whence);

class AvioBridge {
public:
    ~AvioBridge() noexcept;
    AvioBridge(const AvioBridge&) = delete;
    AvioBridge& operator=(const AvioBridge&) = delete;

    static int32_t Create(WinFile* file,
                          const CancelState* cancel,
                          Deadline deadline,
                          std::unique_ptr<AvioBridge>* out,
                          vc_error* error);

    AVIOContext* context() const noexcept { return context_; }
    AvioOpaque& opaque() noexcept { return opaque_; }

private:
    AvioBridge() = default;

    AvioOpaque opaque_{};
    AVIOContext* context_ = nullptr;
};

}  // namespace vc::detail

#endif
