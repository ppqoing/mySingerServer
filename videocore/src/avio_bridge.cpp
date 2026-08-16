#include "avio_bridge.h"

#include <cerrno>

extern "C" {
#include <libavutil/error.h>
#include <libavutil/mem.h>
}

#include "error.h"
#include "win_file.h"

namespace vc::detail {
namespace {

constexpr int kAvioBufferSize = 64 * 1024;

int FfmpegFailure(int32_t status) noexcept {
    if (status == VC_ERR_CANCELLED || status == VC_ERR_TIMEOUT) {
        return AVERROR_EXIT;
    }
    if (status == VC_ERR_INVALID_ARG) {
        return AVERROR(EINVAL);
    }
    return AVERROR(EIO);
}

}  // namespace

int ReadPacket(void* opaque_value,
               uint8_t* buffer,
               int size) {
    auto* opaque = static_cast<AvioOpaque*>(opaque_value);
    if (opaque == nullptr || opaque->file == nullptr ||
        buffer == nullptr || size <= 0) {
        return AVERROR(EINVAL);
    }
    int bytes_read = 0;
    const int32_t status = opaque->file->Read(buffer,
                                               size,
                                               &bytes_read,
                                               opaque->cancel,
                                               &opaque->deadline,
                                               nullptr);
    opaque->last_status = status;
    if (status != VC_OK) {
        return FfmpegFailure(status);
    }
    return bytes_read == 0 ? AVERROR_EOF : bytes_read;
}

int64_t SeekPacket(void* opaque_value,
                   int64_t offset,
                   int whence) {
    auto* opaque = static_cast<AvioOpaque*>(opaque_value);
    if (opaque == nullptr || opaque->file == nullptr) {
        if (opaque != nullptr) {
            opaque->last_status = VC_ERR_INVALID_ARG;
        }
        return AVERROR(EINVAL);
    }
    const int origin = whence & ~AVSEEK_FORCE;
    if (origin == AVSEEK_SIZE) {
        const int64_t size = opaque->file->SnapshotSize();
        if (size < 0) {
            opaque->last_status = VC_ERR_IO;
            return AVERROR(EIO);
        }
        opaque->last_status = VC_OK;
        return size;
    }

    DWORD move_method = 0u;
    switch (origin) {
        case SEEK_SET:
            move_method = FILE_BEGIN;
            break;
        case SEEK_CUR:
            move_method = FILE_CURRENT;
            break;
        case SEEK_END:
            move_method = FILE_END;
            break;
        default:
            opaque->last_status = VC_ERR_INVALID_ARG;
            return AVERROR(EINVAL);
    }
    int64_t position = 0;
    const int32_t status = opaque->file->Seek(offset,
                                               move_method,
                                               &position,
                                               opaque->cancel,
                                               &opaque->deadline,
                                               nullptr);
    opaque->last_status = status;
    return status == VC_OK ? position : FfmpegFailure(status);
}

AvioBridge::~AvioBridge() noexcept {
    if (context_ != nullptr) {
        avio_context_free(&context_);
    }
}

int32_t AvioBridge::Create(WinFile* file,
                           const CancelState* cancel,
                           Deadline deadline,
                           std::unique_ptr<AvioBridge>* out,
                           vc_error* error) {
    if (file == nullptr || out == nullptr) {
        SetError(error,
                 VC_ERR_INVALID_ARG,
                 0,
                 0,
                 "AVIO bridge arguments are invalid");
        return VC_ERR_INVALID_ARG;
    }
    std::unique_ptr<AvioBridge> bridge(new AvioBridge());
    bridge->opaque_.file = file;
    bridge->opaque_.cancel = cancel;
    bridge->opaque_.deadline = deadline;
    uint8_t* buffer = static_cast<uint8_t*>(
        av_malloc(kAvioBufferSize));
    if (buffer == nullptr) {
        SetError(error, VC_ERR_OOM, 0, 0, "AVIO buffer allocation failed");
        return VC_ERR_OOM;
    }
    bridge->context_ = avio_alloc_context(buffer,
                                          kAvioBufferSize,
                                          0,
                                          &bridge->opaque_,
                                          &ReadPacket,
                                          nullptr,
                                          &SeekPacket);
    if (bridge->context_ == nullptr) {
        av_free(buffer);
        SetError(error,
                 VC_ERR_OOM,
                 0,
                 0,
                 "AVIO context allocation failed");
        return VC_ERR_OOM;
    }
    bridge->context_->seekable = AVIO_SEEKABLE_NORMAL;
    *out = std::move(bridge);
    SetError(error, VC_OK, 0, 0, "");
    return VC_OK;
}

}  // namespace vc::detail
