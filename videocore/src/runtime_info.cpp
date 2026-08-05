#include "runtime_info.h"

#include <algorithm>
#include <cstring>

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/avutil.h>
#include <libswscale/swscale.h>
}

#include "error.h"

namespace vc::detail {
namespace {

class FfmpegVersionProvider final : public VersionProvider {
public:
    uint32_t AvFormatVersion() const noexcept override {
        return avformat_version();
    }

    uint32_t AvCodecVersion() const noexcept override {
        return avcodec_version();
    }

    uint32_t AvUtilVersion() const noexcept override {
        return avutil_version();
    }

    uint32_t SwScaleVersion() const noexcept override {
        return swscale_version();
    }

    const char* BuildId() const noexcept override {
        return av_version_info();
    }
};

constexpr uint32_t Major(uint32_t version) noexcept {
    return version >> 16u;
}

template <size_t Capacity>
bool ValidateUtf8String(const char* source,
                        size_t& length) noexcept {
    if (source == nullptr) {
        return false;
    }
    length = 0u;
    while (length < Capacity && source[length] != '\0') {
        ++length;
    }
    if (length == 0u || length == Capacity) {
        return false;
    }

    size_t index = 0u;
    while (index < length) {
        const uint8_t first =
            static_cast<uint8_t>(source[index]);
        if (first <= 0x7fu) {
            ++index;
            continue;
        }

        size_t count = 0u;
        uint8_t second_min = 0x80u;
        uint8_t second_max = 0xbfu;
        if (first >= 0xc2u && first <= 0xdfu) {
            count = 2u;
        } else if (first >= 0xe0u && first <= 0xefu) {
            count = 3u;
            if (first == 0xe0u) {
                second_min = 0xa0u;
            } else if (first == 0xedu) {
                second_max = 0x9fu;
            }
        } else if (first >= 0xf0u && first <= 0xf4u) {
            count = 4u;
            if (first == 0xf0u) {
                second_min = 0x90u;
            } else if (first == 0xf4u) {
                second_max = 0x8fu;
            }
        } else {
            return false;
        }
        if (index + count > length) {
            return false;
        }
        const uint8_t second =
            static_cast<uint8_t>(source[index + 1u]);
        if (second < second_min || second > second_max) {
            return false;
        }
        for (size_t continuation = 2u;
             continuation < count;
             ++continuation) {
            const uint8_t byte =
                static_cast<uint8_t>(
                    source[index + continuation]);
            if (byte < 0x80u || byte > 0xbfu) {
                return false;
            }
        }
        index += count;
    }
    return true;
}

template <size_t Capacity>
void CopyValidatedString(char (&destination)[Capacity],
                         const char* source,
                         size_t length) noexcept {
    std::memcpy(destination, source, length);
    destination[length] = '\0';
}

int32_t RejectMismatch(vc_error* error,
                       const char* message) noexcept {
    SetError(error, VC_ERR_ABI, 0, 0, message);
    return VC_ERR_ABI;
}

}  // namespace

const VersionProvider& DefaultVersionProvider() noexcept {
    static const FfmpegVersionProvider provider;
    return provider;
}

int32_t PopulateRuntimeInfo(struct vc_runtime_info* out,
                            vc_error* error,
                            const VersionProvider& provider) noexcept {
    if (out == nullptr) {
        SetError(error,
                 VC_ERR_INVALID_ARG,
                 0,
                 0,
                 "runtime info output is null");
        return VC_ERR_INVALID_ARG;
    }
    if (out->struct_size < sizeof(*out) ||
        out->abi_version != VC_ABI_VERSION) {
        SetError(error,
                 VC_ERR_ABI,
                 0,
                 0,
                 "vc_runtime_info ABI mismatch");
        return VC_ERR_ABI;
    }

    const uint32_t avformat_runtime = provider.AvFormatVersion();
    const uint32_t avcodec_runtime = provider.AvCodecVersion();
    const uint32_t avutil_runtime = provider.AvUtilVersion();
    const uint32_t swscale_runtime = provider.SwScaleVersion();
    const char* const build_id = provider.BuildId();

    if (Major(avformat_runtime) !=
        Major(LIBAVFORMAT_VERSION_INT)) {
        return RejectMismatch(error,
                              "avformat header/runtime major mismatch");
    }
    if (Major(avcodec_runtime) !=
        Major(LIBAVCODEC_VERSION_INT)) {
        return RejectMismatch(error,
                              "avcodec header/runtime major mismatch");
    }
    if (Major(avutil_runtime) != Major(LIBAVUTIL_VERSION_INT)) {
        return RejectMismatch(error,
                              "avutil header/runtime major mismatch");
    }
    if (Major(swscale_runtime) !=
        Major(LIBSWSCALE_VERSION_INT)) {
        return RejectMismatch(error,
                              "swscale header/runtime major mismatch");
    }

    size_t videocore_version_length = 0u;
    if (!ValidateUtf8String<
            sizeof(vc_runtime_info::videocore_version_utf8)>(
            VC_VERSION_STRING, videocore_version_length)) {
        return RejectMismatch(error,
                              "VideoCore version string is invalid");
    }
    size_t build_id_length = 0u;
    if (!ValidateUtf8String<
            sizeof(vc_runtime_info::ffmpeg_build_id_utf8)>(
            build_id, build_id_length)) {
        return RejectMismatch(error,
                              "FFmpeg build ID string is invalid");
    }

    struct vc_runtime_info candidate{};
    candidate.struct_size = sizeof(candidate);
    candidate.abi_version = VC_ABI_VERSION;
    CopyValidatedString(candidate.videocore_version_utf8,
                        VC_VERSION_STRING,
                        videocore_version_length);
    CopyValidatedString(candidate.ffmpeg_build_id_utf8,
                        build_id,
                        build_id_length);
    candidate.avformat_header_version = LIBAVFORMAT_VERSION_INT;
    candidate.avformat_runtime_version = avformat_runtime;
    candidate.avcodec_header_version = LIBAVCODEC_VERSION_INT;
    candidate.avcodec_runtime_version = avcodec_runtime;
    candidate.avutil_header_version = LIBAVUTIL_VERSION_INT;
    candidate.avutil_runtime_version = avutil_runtime;
    candidate.swscale_header_version = LIBSWSCALE_VERSION_INT;
    candidate.swscale_runtime_version = swscale_runtime;
    *out = candidate;
    SetError(error, VC_OK, 0, 0, "");
    return VC_OK;
}

}  // namespace vc::detail
