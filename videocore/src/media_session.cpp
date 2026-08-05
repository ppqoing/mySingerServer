#include "media_session.h"

#include <algorithm>
#include <array>
#include <atomic>
#include <chrono>
#include <cstring>
#include <limits>
#include <memory>
#include <mutex>
#include <new>
#include <stdexcept>
#include <utility>
#include <vector>

#include "avio_bridge.h"
#include "cancel_token.h"
#include "deadline.h"
#include "error.h"
#include "image_analysis.h"
#include "native_algorithms/sha512.h"
#include "video_analysis.h"

namespace vc::detail {
namespace {

constexpr size_t kHashChunkSize = 64u * 1024u;
constexpr size_t kSessionSlotCount = 4096u;
constexpr uintptr_t kSessionSlotMask = 0xffffu;
constexpr unsigned kSessionGenerationShift = 16u;
constexpr uint32_t kMaximumSessionGeneration = 0x7fffffffu;

enum class SessionSlotState : uint64_t {
    free = 0u,
    initializing = 1u,
    active = 2u,
    disposed = 3u,
};

constexpr uint64_t kSessionStateMask = 0x3u;

class MediaSession {
public:
    MediaSession(std::unique_ptr<WinFile> file,
                 std::unique_ptr<AvioBridge> avio,
                 CancelState* cancel,
                 const vc_media_open_options& options) noexcept
        : file_(std::move(file)),
          avio_(std::move(avio)),
          cancel_(cancel),
          expected_media_type_(options.expected_media_type),
          image_max_bytes_(options.image_max_bytes),
          operation_timeout_ms_(options.operation_timeout_ms) {}

    ~MediaSession() noexcept {
        ReleaseCancelState(cancel_);
    }

    int32_t Hash(uint8_t out[VC_SHA512_SIZE],
                 vc_error* error) {
        OperationGuard guard(operation_active_);
        if (!guard.acquired()) {
            SetError(error,
                     VC_ERR_INVALID_ARG,
                     0,
                     0,
                     "media session operation is already active");
            return VC_ERR_INVALID_ARG;
        }

        if (hash_attempted_) {
            if (hash_status_ == VC_OK) {
                std::memcpy(out, hash_.data(), hash_.size());
                SetError(error, VC_OK, 0, 0, "");
            } else {
                SetError(error,
                         hash_status_,
                         0,
                         hash_win32_code_,
                         "media hash previously failed");
            }
            return hash_status_;
        }
        hash_attempted_ = true;
#if defined(VC_MEDIA_SESSION_TESTING)
        ++test_state_.hash_runs;
#endif
        try {
            return HashOnce(out, error);
        } catch (const std::bad_alloc&) {
            hash_status_ = VC_ERR_OOM;
            hash_win32_code_ = 0u;
            SetError(error,
                     VC_ERR_OOM,
                     0,
                     0,
                     "media hash ran out of memory");
            return VC_ERR_OOM;
        } catch (...) {
            hash_status_ = VC_ERR_INTERNAL;
            hash_win32_code_ = 0u;
            SetError(error,
                     VC_ERR_INTERNAL,
                     0,
                     0,
                     "media hash failed unexpectedly");
            return VC_ERR_INTERNAL;
        }
    }

    int32_t Analyze(const vc_analysis_request& request,
                    vc_analysis_result* out,
                    vc_error* error) {
        const uint64_t feature_mask = request.feature_mask;
        OperationGuard guard(operation_active_);
        if (!guard.acquired()) {
            return FailAnalyze(
                out,
                error,
                VC_ERR_INVALID_ARG,
                "media session operation is already active");
        }
        if (!hash_attempted_ || hash_status_ != VC_OK) {
            return FailAnalyze(
                out,
                error,
                VC_ERR_INVALID_ARG,
                "media analysis requires a completed media hash");
        }
        if (expected_media_type_ == VC_MEDIA_TYPE_VIDEO) {
            return AnalyzeVideo(avio_.get(), cancel_, request, out, error);
        }
        if (expected_media_type_ != VC_MEDIA_TYPE_IMAGE) {
            SetError(error,
                     VC_ERR_UNSUPPORTED,
                     0,
                     0,
                     "media analysis is unavailable for this media type");
            return VC_ERR_UNSUPPORTED;
        }
        return AnalyzeImageBytes(image_bytes_, feature_mask, out, error);
    }

    int32_t RejectInvalidAnalysisRequest(
        vc_analysis_result* out,
        vc_error* error) const noexcept {
        if (expected_media_type_ == VC_MEDIA_TYPE_IMAGE) {
            return FailAnalyze(
                out,
                error,
                VC_ERR_INVALID_ARG,
                "analysis reserved fields must be zero");
        }
        SetError(error,
                 VC_ERR_INVALID_ARG,
                 0,
                 0,
                 "analysis reserved fields must be zero");
        return VC_ERR_INVALID_ARG;
    }

#if defined(VC_MEDIA_SESSION_TESTING)
    MediaSessionTestSnapshot Snapshot() const noexcept {
        MediaSessionTestSnapshot snapshot;
        snapshot.identity = file_->snapshot().identity;
        snapshot.file_size = file_->snapshot().size;
        snapshot.last_write_time =
            file_->snapshot().last_write_time;
        snapshot.io = file_->stats();
        snapshot.image_cache_size = image_bytes_.size();
        snapshot.hash_runs = test_state_.hash_runs;
        snapshot.hash_cached =
            hash_attempted_ && hash_status_ == VC_OK;
        snapshot.has_custom_avio =
            avio_ != nullptr && avio_->context() != nullptr &&
            avio_->opaque().file == file_.get();
        return snapshot;
    }

    void SetIoHook(IoBoundaryHook hook, void* context) noexcept {
        file_->SetIoHook(hook, context);
    }

    void SetHashFailure(
        MediaSessionTestHashFailure failure) noexcept {
        test_state_.hash_failure = failure;
    }
#endif

private:
    int32_t FailAnalyze(vc_analysis_result* out,
                        vc_error* error,
                        int32_t code,
                        const char* message) const noexcept {
        if (expected_media_type_ == VC_MEDIA_TYPE_IMAGE) {
            return PublishImageFailure(out, error, code, message);
        }
        if (expected_media_type_ == VC_MEDIA_TYPE_VIDEO) {
            return PublishVideoFailure(out, error, code, message);
        }
        SetError(error, code, 0, 0, message);
        return code;
    }

    int32_t HashOnce(uint8_t out[VC_SHA512_SIZE],
                     vc_error* error) {
        const Deadline deadline = OperationDeadline();
        int64_t position = -1;
        int32_t status = file_->Seek(0,
                                     FILE_BEGIN,
                                     &position,
                                     cancel_,
                                     deadline,
                                     error);
        if (status != VC_OK) {
            RememberHashFailure(status, error);
            return status;
        }

        native::Sha512 sha;
        std::vector<uint8_t> image_bytes;
        const bool collect_image =
            expected_media_type_ == VC_MEDIA_TYPE_IMAGE &&
            image_max_bytes_ != 0u;
        std::array<uint8_t, kHashChunkSize> chunk{};
        for (;;) {
            status = CheckOperationBoundary(
                cancel_, deadline, OperationBoundary::hash_read);
            if (status != VC_OK) {
                SetError(error,
                         status,
                         0,
                         0,
                         status == VC_ERR_CANCELLED
                             ? "operation cancelled"
                             : "operation timed out");
                RememberHashFailure(status, error);
                return status;
            }
            int bytes_read = 0;
            status = file_->Read(chunk.data(),
                                 static_cast<int>(chunk.size()),
                                 &bytes_read,
                                 cancel_,
                                 deadline,
                                 error);
            if (status != VC_OK) {
                RememberHashFailure(status, error);
                return status;
            }
            if (bytes_read == 0) {
                break;
            }
            sha.Update(chunk.data(), static_cast<size_t>(bytes_read));
            if (collect_image &&
                image_bytes.size() <
                    static_cast<size_t>((std::min<uint64_t>)(
                        image_max_bytes_,
                        (std::numeric_limits<size_t>::max)()))) {
                const uint64_t remaining =
                    image_max_bytes_ - image_bytes.size();
                const size_t copy_size =
                    static_cast<size_t>((std::min<uint64_t>)(
                        remaining,
                        static_cast<uint64_t>(bytes_read)));
#if defined(VC_MEDIA_SESSION_TESTING)
                const MediaSessionTestHashFailure failure =
                    test_state_.hash_failure;
                test_state_.hash_failure =
                    MediaSessionTestHashFailure::none;
                if (failure ==
                    MediaSessionTestHashFailure::bad_alloc) {
                    throw std::bad_alloc();
                }
                if (failure ==
                    MediaSessionTestHashFailure::unexpected) {
                    throw std::runtime_error(
                        "injected image cache failure");
                }
#endif
                image_bytes.insert(image_bytes.end(),
                                   chunk.begin(),
                                   chunk.begin() + copy_size);
            }
        }

        const auto digest = sha.Final();
        hash_ = digest;
        image_bytes_ = std::move(image_bytes);
        hash_status_ = VC_OK;
        std::memcpy(out, hash_.data(), hash_.size());
        SetError(error, VC_OK, 0, 0, "");
        return VC_OK;
    }

    class OperationGuard {
    public:
        explicit OperationGuard(std::atomic_flag& active) noexcept
            : active_(active),
              acquired_(!active_.test_and_set(
                  std::memory_order_acquire)) {}
        ~OperationGuard() noexcept {
            if (acquired_) {
                active_.clear(std::memory_order_release);
            }
        }
        bool acquired() const noexcept { return acquired_; }

    private:
        std::atomic_flag& active_;
        bool acquired_;
    };

    Deadline OperationDeadline() const noexcept {
        return operation_timeout_ms_ == 0u
                   ? Deadline::Infinite()
                   : Deadline::After(std::chrono::milliseconds(
                         operation_timeout_ms_));
    }

    void RememberHashFailure(int32_t status,
                             const vc_error* error) noexcept {
        hash_status_ = status;
        hash_win32_code_ = error == nullptr ? 0u : error->win32_code;
    }

    std::unique_ptr<WinFile> file_;
    std::unique_ptr<AvioBridge> avio_;
    CancelState* cancel_ = nullptr;
    uint32_t expected_media_type_ = VC_MEDIA_TYPE_AUTO;
    uint64_t image_max_bytes_ = 0u;
    uint32_t operation_timeout_ms_ = 0u;
    std::atomic_flag operation_active_ = ATOMIC_FLAG_INIT;
    bool hash_attempted_ = false;
    int32_t hash_status_ = VC_ERR_INTERNAL;
    uint32_t hash_win32_code_ = 0u;
#if defined(VC_MEDIA_SESSION_TESTING)
    struct TestState {
        uint64_t hash_runs = 0u;
        MediaSessionTestHashFailure hash_failure =
            MediaSessionTestHashFailure::none;
    };
    TestState test_state_{};
#endif
    std::array<uint8_t, VC_SHA512_SIZE> hash_{};
    std::vector<uint8_t> image_bytes_;
};

#if !defined(VC_MEDIA_SESSION_TESTING) && defined(_MSC_VER) && \
    _ITERATOR_DEBUG_LEVEL == 0
static_assert(
    sizeof(MediaSession) == 152u,
    "MSVC x64 production MediaSession contains unexpected state");
#endif

struct SessionSlot {
    std::atomic<uint64_t> control{0u};
    std::mutex mutex;
    std::shared_ptr<MediaSession> media;
};

struct DecodedSessionHandle {
    SessionSlot* slot = nullptr;
    size_t slot_index = kSessionSlotCount;
    uint32_t generation = 0u;
};

std::array<SessionSlot, kSessionSlotCount> session_slots;
std::atomic<uint32_t> next_session_slot{0u};

#if defined(VC_MEDIA_SESSION_TESTING)
struct SessionTestState {
    MediaSessionTestProtocolHook first_hook = nullptr;
    void* first_context = nullptr;
    MediaSessionTestProtocolHook second_hook = nullptr;
    void* second_context = nullptr;
    std::atomic<bool> fail_next_post_claim{false};
};
SessionTestState session_test_state;
#endif

constexpr uint64_t MakeSessionControl(
    uint32_t generation,
    SessionSlotState state) noexcept {
    return (static_cast<uint64_t>(generation) << 2u) |
           static_cast<uint64_t>(state);
}

constexpr uint32_t SessionControlGeneration(
    uint64_t control) noexcept {
    return static_cast<uint32_t>(control >> 2u);
}

constexpr SessionSlotState SessionControlState(
    uint64_t control) noexcept {
    return static_cast<SessionSlotState>(
        control & kSessionStateMask);
}

vc_media_session* EncodeSessionHandle(
    size_t slot_index,
    uint32_t generation) noexcept {
    const uintptr_t value =
        (static_cast<uintptr_t>(generation)
         << kSessionGenerationShift) |
        static_cast<uintptr_t>(slot_index + 1u);
    return reinterpret_cast<vc_media_session*>(value);
}

DecodedSessionHandle DecodeSessionHandle(
    const vc_media_session* handle) noexcept {
    const uintptr_t value = reinterpret_cast<uintptr_t>(handle);
    const uintptr_t encoded_slot = value & kSessionSlotMask;
    const uintptr_t encoded_generation =
        value >> kSessionGenerationShift;
    if (encoded_slot == 0u ||
        encoded_slot > kSessionSlotCount ||
        encoded_generation == 0u ||
        encoded_generation > kMaximumSessionGeneration) {
        return {};
    }
    const size_t slot_index =
        static_cast<size_t>(encoded_slot - 1u);
    return {
        &session_slots[slot_index],
        slot_index,
        static_cast<uint32_t>(encoded_generation),
    };
}

#if defined(VC_MEDIA_SESSION_TESTING)
void RunSessionTestProtocolHooks(
    MediaSessionTestProtocolEvent event,
    size_t slot_index,
    uint32_t generation) noexcept {
    if (session_test_state.first_hook != nullptr) {
        session_test_state.first_hook(
            event,
            slot_index,
            generation,
            session_test_state.first_context);
    }
    if (session_test_state.second_hook != nullptr) {
        session_test_state.second_hook(
            event,
            slot_index,
            generation,
            session_test_state.second_context);
    }
}
#endif

std::shared_ptr<MediaSession> Lookup(
    vc_media_session* handle) noexcept {
    const DecodedSessionHandle decoded =
        DecodeSessionHandle(handle);
    if (decoded.slot == nullptr) {
        return {};
    }
    const uint64_t expected = MakeSessionControl(
        decoded.generation, SessionSlotState::active);
    if (decoded.slot->control.load(std::memory_order_acquire) !=
        expected) {
        return {};
    }
#if defined(VC_MEDIA_SESSION_TESTING)
    RunSessionTestProtocolHooks(
        MediaSessionTestProtocolEvent::lookup_after_first_check,
        decoded.slot_index,
        decoded.generation);
#endif
    try {
        std::lock_guard<std::mutex> lock(decoded.slot->mutex);
        if (decoded.slot->control.load(std::memory_order_acquire) !=
            expected) {
            return {};
        }
        return decoded.slot->media;
    } catch (...) {
        return {};
    }
}

int32_t PublishSession(
    std::shared_ptr<MediaSession> media,
    vc_media_session** out,
    vc_error* error) {
    const uint32_t start =
        next_session_slot.fetch_add(1u, std::memory_order_relaxed);
    for (size_t offset = 0u;
         offset < kSessionSlotCount;
         ++offset) {
        const size_t index =
            (static_cast<size_t>(start) + offset) %
            kSessionSlotCount;
        SessionSlot& slot = session_slots[index];
        uint64_t control =
            slot.control.load(std::memory_order_acquire);
        if (SessionControlState(control) !=
            SessionSlotState::free) {
            continue;
        }
        const uint32_t old_generation =
            SessionControlGeneration(control);
        if (old_generation == kMaximumSessionGeneration) {
            continue;
        }
        const uint32_t generation = old_generation + 1u;
        if (!slot.control.compare_exchange_strong(
                control,
                MakeSessionControl(
                    generation,
                    SessionSlotState::initializing),
                std::memory_order_acq_rel,
                std::memory_order_acquire)) {
            continue;
        }
        try {
#if defined(VC_MEDIA_SESSION_TESTING)
            RunSessionTestProtocolHooks(
                MediaSessionTestProtocolEvent::publish_after_claim,
                index,
                generation);
            if (session_test_state.fail_next_post_claim.exchange(
                    false, std::memory_order_acq_rel)) {
                throw std::bad_alloc();
            }
#endif
            std::lock_guard<std::mutex> lock(slot.mutex);
            slot.media = std::move(media);
        } catch (...) {
            try {
                std::lock_guard<std::mutex> lock(slot.mutex);
                slot.media.reset();
            } catch (...) {
            }
            slot.control.store(
                MakeSessionControl(
                    generation, SessionSlotState::free),
                std::memory_order_release);
            throw;
        }
#if defined(VC_MEDIA_SESSION_TESTING)
        RunSessionTestProtocolHooks(
            MediaSessionTestProtocolEvent::publish_before_active,
            index,
            generation);
#endif
        slot.control.store(
            MakeSessionControl(
                generation, SessionSlotState::active),
            std::memory_order_release);
        *out = EncodeSessionHandle(index, generation);
        SetError(error, VC_OK, 0, 0, "");
        return VC_OK;
    }
    SetError(error,
             VC_ERR_OOM,
             0,
             0,
             "media session capacity exhausted");
    return VC_ERR_OOM;
}

Deadline OpenDeadline(uint32_t timeout_ms) noexcept {
    return timeout_ms == 0u
               ? Deadline::Infinite()
               : Deadline::After(
                     std::chrono::milliseconds(timeout_ms));
}

}  // namespace

static_assert(sizeof(uintptr_t) >= sizeof(uint64_t),
              "encoded media handles require a 64-bit target");
static_assert(std::atomic<uint64_t>::is_always_lock_free,
              "media slot control requires lock-free uint64 atomics");

int32_t CreateMediaSession(const uint16_t* path,
                           uint32_t path_units,
                           const vc_media_open_options& options,
                           vc_cancel_token* cancel,
                           vc_media_session** out,
                           vc_error* error) {
    CancelState* cancel_state = RetainCancelState(cancel);
    if (cancel != nullptr && cancel_state == nullptr) {
        SetError(error,
                 VC_ERR_INVALID_ARG,
                 0,
                 0,
                 "cancel token is invalid");
        return VC_ERR_INVALID_ARG;
    }
    struct CancelOwner {
        CancelState* state;
        ~CancelOwner() { ReleaseCancelState(state); }
    } cancel_owner{cancel_state};

    static_assert(sizeof(wchar_t) == sizeof(uint16_t));
    const std::wstring wide_path(
        reinterpret_cast<const wchar_t*>(path),
        static_cast<size_t>(path_units));
    std::unique_ptr<WinFile> file;
    const Deadline deadline =
        OpenDeadline(options.operation_timeout_ms);
    int32_t status = WinFile::Open(
        wide_path, cancel_state, deadline, &file, error);
    if (status != VC_OK) {
        return status;
    }

    std::unique_ptr<AvioBridge> avio;
    status = AvioBridge::Create(file.get(),
                                cancel_state,
                                Deadline::Infinite(),
                                &avio,
                                error);
    if (status != VC_OK) {
        return status;
    }

    std::shared_ptr<MediaSession> media =
        std::make_shared<MediaSession>(
            std::move(file),
            std::move(avio),
            cancel_state,
            options);
    cancel_owner.state = nullptr;
    return PublishSession(std::move(media), out, error);
}

int32_t HashMediaSession(
    vc_media_session* session,
    uint8_t out_sha512[VC_SHA512_SIZE],
    vc_error* error) {
    const std::shared_ptr<MediaSession> media = Lookup(session);
    if (!media) {
        SetError(error,
                 VC_ERR_UNSUPPORTED,
                 0,
                 0,
                 "media session is not active");
        return VC_ERR_UNSUPPORTED;
    }
    return media->Hash(out_sha512, error);
}

int32_t AnalyzeMediaSession(
    vc_media_session* session,
    const vc_analysis_request& request,
    vc_analysis_result* out,
    vc_error* error) {
    const bool request_semantics_invalid =
        request.reserved_flags != 0u ||
        request.reserved_0 != 0u ||
        request.reserved_1 != 0u ||
        (request.frame_mask & ~VC_ALL_FRAME_MASK) != 0u;
    const std::shared_ptr<MediaSession> media = Lookup(session);
    if (request_semantics_invalid) {
        if (media) {
            return media->RejectInvalidAnalysisRequest(out, error);
        }
        SetError(error,
                 VC_ERR_INVALID_ARG,
                 0,
                 0,
                 "analysis reserved fields must be zero");
        return VC_ERR_INVALID_ARG;
    }
    if (!media) {
        SetError(error,
                 VC_ERR_UNSUPPORTED,
                 0,
                 0,
                 "media session is not active");
        return VC_ERR_UNSUPPORTED;
    }
    return media->Analyze(request, out, error);
}

void CloseMediaSession(vc_media_session* session) noexcept {
    if (session == nullptr) {
        return;
    }
    const DecodedSessionHandle decoded =
        DecodeSessionHandle(session);
    if (decoded.slot == nullptr) {
        return;
    }
    uint64_t active = MakeSessionControl(
        decoded.generation, SessionSlotState::active);
    if (!decoded.slot->control.compare_exchange_strong(
            active,
            MakeSessionControl(
                decoded.generation,
                SessionSlotState::disposed),
            std::memory_order_acq_rel,
            std::memory_order_acquire)) {
        return;
    }
#if defined(VC_MEDIA_SESSION_TESTING)
    RunSessionTestProtocolHooks(
        MediaSessionTestProtocolEvent::close_after_dispose,
        decoded.slot_index,
        decoded.generation);
#endif
    try {
        std::shared_ptr<MediaSession> removed;
        {
            std::lock_guard<std::mutex> lock(decoded.slot->mutex);
            removed = std::move(decoded.slot->media);
        }
        decoded.slot->control.store(
            MakeSessionControl(
                decoded.generation, SessionSlotState::free),
            std::memory_order_release);
    } catch (...) {
        decoded.slot->control.store(
            MakeSessionControl(
                decoded.generation, SessionSlotState::free),
            std::memory_order_release);
    }
}

#if defined(VC_MEDIA_SESSION_TESTING)
bool GetMediaSessionTestSnapshot(
    vc_media_session* session,
    MediaSessionTestSnapshot* out) noexcept {
    if (out == nullptr) {
        return false;
    }
    const std::shared_ptr<MediaSession> media = Lookup(session);
    if (!media) {
        return false;
    }
    *out = media->Snapshot();
    return true;
}

bool SetMediaSessionTestIoHook(
    vc_media_session* session,
    IoBoundaryHook hook,
    void* context) noexcept {
    const std::shared_ptr<MediaSession> media = Lookup(session);
    if (!media) {
        return false;
    }
    media->SetIoHook(hook, context);
    return true;
}

void MediaSessionTestSetNextSlot(size_t slot_index) noexcept {
    next_session_slot.store(
        static_cast<uint32_t>(
            slot_index % kSessionSlotCount),
        std::memory_order_relaxed);
}

size_t MediaSessionTestSlotCapacity() noexcept {
    return kSessionSlotCount;
}

uint32_t MediaSessionTestMaximumGeneration() noexcept {
    return kMaximumSessionGeneration;
}

size_t MediaSessionTestHandleSlot(
    const vc_media_session* session) noexcept {
    return DecodeSessionHandle(session).slot_index;
}

uint32_t MediaSessionTestHandleGeneration(
    const vc_media_session* session) noexcept {
    return DecodeSessionHandle(session).generation;
}

bool MediaSessionTestSlotIsFree(size_t slot_index) noexcept {
    if (slot_index >= kSessionSlotCount) {
        return false;
    }
    SessionSlot& slot = session_slots[slot_index];
    if (SessionControlState(
            slot.control.load(std::memory_order_acquire)) !=
        SessionSlotState::free) {
        return false;
    }
    try {
        std::lock_guard<std::mutex> lock(slot.mutex);
        return !slot.media;
    } catch (...) {
        return false;
    }
}

uint32_t MediaSessionTestSlotGeneration(
    size_t slot_index) noexcept {
    if (slot_index >= kSessionSlotCount) {
        return 0u;
    }
    return SessionControlGeneration(
        session_slots[slot_index].control.load(
            std::memory_order_acquire));
}

MediaSessionTestSlotState MediaSessionTestSlotStateOf(
    size_t slot_index) noexcept {
    if (slot_index >= kSessionSlotCount) {
        return MediaSessionTestSlotState::invalid;
    }
    const uint64_t control =
        session_slots[slot_index].control.load(
            std::memory_order_acquire);
    const SessionSlotState state = SessionControlState(control);
    if (state == SessionSlotState::free &&
        SessionControlGeneration(control) ==
            kMaximumSessionGeneration) {
        return MediaSessionTestSlotState::retired;
    }
    switch (state) {
        case SessionSlotState::free:
            return MediaSessionTestSlotState::free;
        case SessionSlotState::initializing:
            return MediaSessionTestSlotState::initializing;
        case SessionSlotState::active:
            return MediaSessionTestSlotState::active;
        case SessionSlotState::disposed:
            return MediaSessionTestSlotState::disposed;
    }
    return MediaSessionTestSlotState::invalid;
}

bool MediaSessionTestSlotHasMedia(size_t slot_index) noexcept {
    if (slot_index >= kSessionSlotCount) {
        return false;
    }
    try {
        SessionSlot& slot = session_slots[slot_index];
        std::lock_guard<std::mutex> lock(slot.mutex);
        return static_cast<bool>(slot.media);
    } catch (...) {
        return false;
    }
}

bool MediaSessionTestSeedFreeSlot(
    size_t slot_index,
    uint32_t generation) noexcept {
    if (slot_index >= kSessionSlotCount ||
        generation > kMaximumSessionGeneration) {
        return false;
    }
    SessionSlot& slot = session_slots[slot_index];
    try {
        std::lock_guard<std::mutex> lock(slot.mutex);
        const uint64_t control =
            slot.control.load(std::memory_order_acquire);
        if (SessionControlState(control) !=
                SessionSlotState::free ||
            slot.media) {
            return false;
        }
        slot.control.store(
            MakeSessionControl(
                generation, SessionSlotState::free),
            std::memory_order_release);
        return true;
    } catch (...) {
        return false;
    }
}

vc_media_session* MediaSessionTestEncodeHandle(
    size_t slot_index,
    uint32_t generation) noexcept {
    if (slot_index >= kSessionSlotCount ||
        generation == 0u ||
        generation > kMaximumSessionGeneration) {
        return nullptr;
    }
    return EncodeSessionHandle(slot_index, generation);
}

bool MediaSessionTestLockSlot(size_t slot_index) noexcept {
    if (slot_index >= kSessionSlotCount) {
        return false;
    }
    try {
        session_slots[slot_index].mutex.lock();
        return true;
    } catch (...) {
        return false;
    }
}

void MediaSessionTestUnlockSlot(size_t slot_index) noexcept {
    if (slot_index < kSessionSlotCount) {
        session_slots[slot_index].mutex.unlock();
    }
}

void MediaSessionTestSetProtocolHooks(
    MediaSessionTestProtocolHook first,
    void* first_context,
    MediaSessionTestProtocolHook second,
    void* second_context) noexcept {
    session_test_state.first_hook = first;
    session_test_state.first_context = first_context;
    session_test_state.second_hook = second;
    session_test_state.second_context = second_context;
}

void MediaSessionTestFailNextPostClaim() noexcept {
    session_test_state.fail_next_post_claim.store(
        true, std::memory_order_release);
}

bool SetMediaSessionTestHashFailure(
    vc_media_session* session,
    MediaSessionTestHashFailure failure) noexcept {
    const std::shared_ptr<MediaSession> media = Lookup(session);
    if (!media) {
        return false;
    }
    media->SetHashFailure(failure);
    return true;
}
#endif

}  // namespace vc::detail
