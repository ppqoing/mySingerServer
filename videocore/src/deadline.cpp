#include "deadline.h"

#include <atomic>

#include "cancel_token.h"
#include "videocore/videocore.h"

namespace vc::detail {

#if defined(VC_RESILIENCE_TESTING)
namespace {
std::atomic<OperationBoundaryTestHook> boundary_test_hook{nullptr};
std::atomic<void*> boundary_test_context{nullptr};
}
#endif

Deadline Deadline::Infinite() noexcept {
    return Deadline{};
}

Deadline Deadline::At(TimePoint expiry,
                      NowProvider now,
                      const void* context) noexcept {
    Deadline deadline;
    deadline.active_ = true;
    deadline.expiry_ = expiry;
    deadline.now_ = now == nullptr ? &SteadyNow : now;
    deadline.context_ = context;
    return deadline;
}

Deadline Deadline::After(std::chrono::milliseconds timeout) noexcept {
    const TimePoint now = Clock::now();
    return DeadlineAfterAt(timeout, now, &SteadyNow, nullptr);
}

bool Deadline::Expired() const noexcept {
    return active_ && now_(context_) >= expiry_;
}

Deadline::TimePoint Deadline::SteadyNow(const void*) noexcept {
    return Clock::now();
}

int32_t CheckInterrupt(const CancelState* state,
                       Deadline deadline) noexcept {
    if (IsCancellationRequested(state)) {
        return VC_ERR_CANCELLED;
    }
    if (deadline.Expired()) {
        return VC_ERR_TIMEOUT;
    }
    return VC_OK;
}

int32_t CheckOperationBoundary(const CancelState* state,
                               Deadline deadline,
                               OperationBoundary boundary) noexcept {
#if defined(VC_RESILIENCE_TESTING)
    const OperationBoundaryTestHook hook =
        boundary_test_hook.load(std::memory_order_acquire);
    if (hook != nullptr) {
        hook(boundary,
             boundary_test_context.load(std::memory_order_acquire));
    }
#else
    (void)boundary;
#endif
    return CheckInterrupt(state, deadline);
}

#if defined(VC_RESILIENCE_TESTING)
void SetOperationBoundaryTestHook(
    OperationBoundaryTestHook hook,
    void* context) noexcept {
    boundary_test_context.store(context, std::memory_order_release);
    boundary_test_hook.store(hook, std::memory_order_release);
}
#endif

Deadline DeadlineAfterAt(std::chrono::milliseconds timeout,
                         Deadline::TimePoint now,
                         Deadline::NowProvider provider,
                         const void* context) noexcept {
    if (timeout.count() <= 0) {
        return Deadline::At(now, provider, context);
    }

    using FloatMilliseconds =
        std::chrono::duration<long double, std::milli>;
    const auto remaining =
        Deadline::TimePoint::max() - now;
    const long double timeout_milliseconds =
        static_cast<long double>(timeout.count());
    const long double remaining_milliseconds =
        std::chrono::duration_cast<FloatMilliseconds>(
            remaining).count();
    if (timeout_milliseconds >= remaining_milliseconds) {
        return Deadline::At(
            Deadline::TimePoint::max(), provider, context);
    }

    const auto converted =
        std::chrono::duration_cast<Deadline::Clock::duration>(
            timeout);
    if (converted > remaining) {
        return Deadline::At(
            Deadline::TimePoint::max(), provider, context);
    }
    return Deadline::At(now + converted, provider, context);
}

}  // namespace vc::detail
