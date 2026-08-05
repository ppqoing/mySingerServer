#ifndef VIDEOCORE_SRC_RUNTIME_INFO_H
#define VIDEOCORE_SRC_RUNTIME_INFO_H

#include <cstdint>

#include "videocore/videocore.h"

namespace vc::detail {

class VersionProvider {
public:
    virtual ~VersionProvider() = default;
    virtual uint32_t AvFormatVersion() const noexcept = 0;
    virtual uint32_t AvCodecVersion() const noexcept = 0;
    virtual uint32_t AvUtilVersion() const noexcept = 0;
    virtual uint32_t SwScaleVersion() const noexcept = 0;
    virtual const char* BuildId() const noexcept = 0;
};

const VersionProvider& DefaultVersionProvider() noexcept;

int32_t PopulateRuntimeInfo(struct vc_runtime_info* out,
                            vc_error* error,
                            const VersionProvider& provider) noexcept;

}  // namespace vc::detail

#endif
