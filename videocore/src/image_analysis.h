#ifndef VIDEOCORE_SRC_IMAGE_ANALYSIS_H
#define VIDEOCORE_SRC_IMAGE_ANALYSIS_H

#include <cstdint>
#include <vector>

#include "videocore/videocore.h"

namespace vc::detail {

int32_t AnalyzeImageBytes(const std::vector<uint8_t>& encoded,
                          uint64_t feature_mask,
                          vc_analysis_result* out,
                          vc_error* error) noexcept;
int32_t PublishImageFailure(vc_analysis_result* out,
                            vc_error* error,
                            int32_t code,
                            const char* message) noexcept;

}  // namespace vc::detail

#endif
