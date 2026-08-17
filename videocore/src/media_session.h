#ifndef VIDEOCORE_SRC_MEDIA_SESSION_H
#define VIDEOCORE_SRC_MEDIA_SESSION_H

#include <cstddef>
#include <cstdint>

#include "videocore/videocore.h"
#include "win_file.h"

namespace vc::detail {

int32_t CreateMediaSession(const uint16_t* path,
                           uint32_t path_units,
                           const vc_media_open_options& options,
                           vc_cancel_token* cancel,
                           vc_media_session** out,
                           vc_error* error);
int32_t HashMediaSession(
    vc_media_session* session,
    uint8_t out_sha512[VC_SHA512_SIZE],
    vc_error* error);
int32_t AnalyzeMediaSession(
    vc_media_session* session,
    const vc_analysis_request& request,
    vc_analysis_result* out,
    vc_error* error);
int32_t MediaSessionContainerInfo(
    vc_media_session* session,
    vc_video_container_info* out,
    vc_error* error);
uint32_t MediaSessionStreamCount(vc_media_session* session) noexcept;
int32_t MediaSessionStreamInfo(
    vc_media_session* session,
    uint32_t ordinal,
    vc_video_stream_info* out,
    vc_error* error);
int32_t MediaSessionMetadataJson(
    vc_media_session* session,
    int32_t stream_index,
    char* destination,
    uint32_t capacity,
    uint32_t* required,
    vc_error* error);
void CloseMediaSession(vc_media_session* session) noexcept;

#if defined(VC_MEDIA_SESSION_TESTING)
struct MediaSessionTestSnapshot {
    FileIdentity identity{};
    uint64_t file_size = 0u;
    uint64_t last_write_time = 0u;
    WinFileStats io{};
    uint64_t image_cache_size = 0u;
    uint64_t hash_runs = 0u;
    bool hash_cached = false;
    bool has_custom_avio = false;
};

bool GetMediaSessionTestSnapshot(
    vc_media_session* session,
    MediaSessionTestSnapshot* out) noexcept;
bool SetMediaSessionTestIoHook(
    vc_media_session* session,
    IoBoundaryHook hook,
    void* context) noexcept;

enum class MediaSessionTestHashFailure : uint32_t {
    none = 0u,
    bad_alloc = 1u,
    unexpected = 2u,
};

enum class MediaSessionTestSlotState : uint32_t {
    invalid = 0u,
    free = 1u,
    initializing = 2u,
    active = 3u,
    disposed = 4u,
    retired = 5u,
};

enum class MediaSessionTestProtocolEvent : uint32_t {
    lookup_after_first_check = 1u,
    publish_after_claim = 2u,
    publish_before_active = 3u,
    close_after_dispose = 4u,
};

using MediaSessionTestProtocolHook = void (*)(
    MediaSessionTestProtocolEvent event,
    size_t slot_index,
    uint32_t generation,
    void* context) noexcept;

void MediaSessionTestSetNextSlot(size_t slot_index) noexcept;
size_t MediaSessionTestSlotCapacity() noexcept;
uint32_t MediaSessionTestMaximumGeneration() noexcept;
size_t MediaSessionTestHandleSlot(
    const vc_media_session* session) noexcept;
uint32_t MediaSessionTestHandleGeneration(
    const vc_media_session* session) noexcept;
bool MediaSessionTestSlotIsFree(size_t slot_index) noexcept;
uint32_t MediaSessionTestSlotGeneration(
    size_t slot_index) noexcept;
MediaSessionTestSlotState MediaSessionTestSlotStateOf(
    size_t slot_index) noexcept;
bool MediaSessionTestSlotHasMedia(size_t slot_index) noexcept;
bool MediaSessionTestSeedFreeSlot(
    size_t slot_index,
    uint32_t generation) noexcept;
vc_media_session* MediaSessionTestEncodeHandle(
    size_t slot_index,
    uint32_t generation) noexcept;
bool MediaSessionTestLockSlot(size_t slot_index) noexcept;
void MediaSessionTestUnlockSlot(size_t slot_index) noexcept;
void MediaSessionTestSetProtocolHooks(
    MediaSessionTestProtocolHook first,
    void* first_context,
    MediaSessionTestProtocolHook second,
    void* second_context) noexcept;
void MediaSessionTestFailNextPostClaim() noexcept;
bool SetMediaSessionTestHashFailure(
    vc_media_session* session,
    MediaSessionTestHashFailure failure) noexcept;
#endif

}  // namespace vc::detail

#endif
