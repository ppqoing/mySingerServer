#ifndef VIDEOCORE_SRC_DEADLINE_H
#define VIDEOCORE_SRC_DEADLINE_H

#include <chrono>
#include <cstdint>

namespace vc::detail {

struct CancelState;

enum class OperationBoundary : uint32_t {
    open = 1u,
    hash_read = 2u,
    probe = 3u,
    seek = 4u,
    packet_read = 5u,
    decode = 6u,
    feature = 7u,
    jpeg_encode = 8u,
};

class Deadline {
public:
    using Clock = std::chrono::steady_clock;
    using TimePoint = Clock::time_point;
    using NowProvider = TimePoint (*)(const void*) noexcept;

    static Deadline Infinite() noexcept;
    static Deadline At(TimePoint expiry,
                       NowProvider now,
                       const void* context) noexcept;
    static Deadline After(std::chrono::milliseconds timeout) noexcept;

    bool Expired() const noexcept;

private:
    static TimePoint SteadyNow(const void*) noexcept;

    bool active_ = false;
    TimePoint expiry_{};
    NowProvider now_ = &SteadyNow;
    const void* context_ = nullptr;
};

int32_t CheckInterrupt(const CancelState* state,
                       Deadline deadline) noexcept;
int32_t CheckOperationBoundary(const CancelState* state,
                               Deadline deadline,
                               OperationBoundary boundary) noexcept;
Deadline DeadlineAfterAt(std::chrono::milliseconds timeout,
                         Deadline::TimePoint now,
                         Deadline::NowProvider provider,
                         const void* context) noexcept;

#if defined(VC_RESILIENCE_TESTING)
using OperationBoundaryTestHook = void (*)(
    OperationBoundary boundary,
    void* context) noexcept;
void SetOperationBoundaryTestHook(
    OperationBoundaryTestHook hook,
    void* context) noexcept;
#endif

}  // namespace vc::detail

#endif
