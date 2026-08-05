#include <atomic>
#include <chrono>
#include <cstdint>
#include <iostream>
#include <limits>
#include <thread>
#include <vector>

#include "cancel_token.h"
#include "deadline.h"
#include "videocore/videocore.h"

namespace {

int failures = 0;

void Check(bool condition, const char* message) {
    if (!condition) {
        std::cerr << "FAIL: " << message << '\n';
        ++failures;
    }
}

vc_error FreshError() {
    vc_error error{};
    error.struct_size = sizeof(error);
    error.abi_version = VC_ABI_VERSION;
    return error;
}

struct FakeClock {
    vc::detail::Deadline::TimePoint now{};

    static vc::detail::Deadline::TimePoint Read(
        const void* context) noexcept {
        return static_cast<const FakeClock*>(context)->now;
    }
};

void TestCrossThreadRequestAndRetainedLifetime() {
    vc_cancel_token* token = nullptr;
    vc_error error = FreshError();
    Check(vc::detail::CreateCancelToken(&token, &error) == VC_OK,
          "cancel token creation");
    Check(token != nullptr, "cancel token output");
    Check(error.code == VC_OK, "cancel token success error status");

    vc::detail::CancelState* session_state =
        vc::detail::RetainCancelState(token);
    Check(session_state != nullptr, "session must retain cancel state");

    std::atomic<bool> checker_ready{false};
    std::atomic<int32_t> observed{VC_OK};
    std::thread checker([&]() {
        checker_ready.store(true, std::memory_order_release);
        while (observed.load(std::memory_order_relaxed) == VC_OK) {
            observed.store(
                vc::detail::CheckInterrupt(
                    session_state, vc::detail::Deadline::Infinite()),
                std::memory_order_relaxed);
            std::this_thread::yield();
        }
    });
    while (!checker_ready.load(std::memory_order_acquire)) {
        std::this_thread::yield();
    }

    vc::detail::RequestCancel(token);
    vc::detail::RequestCancel(token);
    vc::detail::FreeCancelToken(token);
    checker.join();

    Check(observed.load(std::memory_order_relaxed) == VC_ERR_CANCELLED,
          "another thread must observe cancellation");
    Check(vc::detail::CheckInterrupt(
              session_state, vc::detail::Deadline::Infinite()) ==
              VC_ERR_CANCELLED,
          "session reference must remain valid after caller free");
    vc::detail::ReleaseCancelState(session_state);

    vc::detail::RequestCancel(nullptr);
    vc::detail::RequestCancel(nullptr);
    vc::detail::FreeCancelToken(nullptr);
    vc::detail::FreeCancelToken(nullptr);
}

void TestFakeClockDeadlineIsDeterministic() {
    FakeClock clock;
    const auto expiry =
        vc::detail::Deadline::TimePoint(std::chrono::milliseconds(50));
    const vc::detail::Deadline deadline =
        vc::detail::Deadline::At(expiry, &FakeClock::Read, &clock);

    clock.now = vc::detail::Deadline::TimePoint(
        std::chrono::milliseconds(49));
    Check(vc::detail::CheckInterrupt(nullptr, deadline) == VC_OK,
          "deadline must not expire early");
    clock.now = expiry;
    Check(vc::detail::CheckInterrupt(nullptr, deadline) == VC_ERR_TIMEOUT,
          "deadline expires exactly at the boundary");
    clock.now = vc::detail::Deadline::TimePoint(
        std::chrono::milliseconds(5000));
    Check(vc::detail::CheckInterrupt(
              nullptr, vc::detail::Deadline::Infinite()) == VC_OK,
          "infinite deadline ignores clock progression");
}

void TestFiniteZeroNegativeAndMaximumTimeouts() {
    Check(vc::detail::CheckInterrupt(
              nullptr,
              vc::detail::Deadline::After(std::chrono::milliseconds(0))) ==
              VC_ERR_TIMEOUT,
          "zero timeout must be immediately expired");
    Check(vc::detail::CheckInterrupt(
              nullptr,
              vc::detail::Deadline::After(std::chrono::milliseconds(-1))) ==
              VC_ERR_TIMEOUT,
          "negative timeout is defined as already expired");
    Check(vc::detail::CheckInterrupt(
              nullptr,
              vc::detail::Deadline::After(
                  std::chrono::milliseconds::max())) == VC_OK,
          "maximum timeout must saturate instead of wrapping");
}

void TestDeadlineSaturatesNearMaximumTimePoint() {
    FakeClock clock;
    const auto near_max =
        vc::detail::Deadline::TimePoint::max() -
        std::chrono::duration_cast<
            vc::detail::Deadline::Clock::duration>(
            std::chrono::milliseconds(1));
    clock.now = near_max;
    const vc::detail::Deadline near_max_deadline =
        vc::detail::DeadlineAfterAt(
            std::chrono::milliseconds(2),
            near_max,
            &FakeClock::Read,
            &clock);
    Check(vc::detail::CheckInterrupt(
              nullptr, near_max_deadline) == VC_OK,
          "near-max addition must saturate, not wrap");
    clock.now = vc::detail::Deadline::TimePoint::max();
    Check(vc::detail::CheckInterrupt(
              nullptr, near_max_deadline) == VC_ERR_TIMEOUT,
          "saturated near-max deadline expires at time_point::max");

    clock.now = vc::detail::Deadline::TimePoint{};
    const vc::detail::Deadline maximum_deadline =
        vc::detail::DeadlineAfterAt(
            std::chrono::milliseconds::max(),
            clock.now,
            &FakeClock::Read,
            &clock);
    Check(vc::detail::CheckInterrupt(
              nullptr, maximum_deadline) == VC_OK,
          "maximum milliseconds must saturate to a future deadline");
    clock.now = vc::detail::Deadline::TimePoint::max();
    Check(vc::detail::CheckInterrupt(
              nullptr, maximum_deadline) == VC_ERR_TIMEOUT,
          "maximum timeout expires only at saturated maximum");
}

void TestReferencePinRejectsZeroAndOverflow() {
    std::atomic<uint32_t> zero{0u};
    Check(!vc::detail::TryRetainReference(zero),
          "zero reference count cannot be resurrected");
    Check(zero.load(std::memory_order_relaxed) == 0u,
          "zero reference count remains zero");

    std::atomic<uint32_t> maximum{
        (std::numeric_limits<uint32_t>::max)()};
    Check(!vc::detail::TryRetainReference(maximum),
          "maximum reference count must reject overflow");
    Check(maximum.load(std::memory_order_relaxed) ==
              (std::numeric_limits<uint32_t>::max)(),
          "overflow rejection must preserve reference count");

    std::atomic<uint32_t> available{41u};
    Check(vc::detail::TryRetainReference(available),
          "ordinary non-zero reference count can be retained");
    Check(available.load(std::memory_order_relaxed) == 42u,
          "successful retain increments exactly once");
}

void TestCancellationWinsOverSimultaneousTimeout() {
    vc_cancel_token* token = nullptr;
    vc_error error = FreshError();
    Check(vc::detail::CreateCancelToken(&token, &error) == VC_OK,
          "priority token creation");
    vc::detail::CancelState* state =
        vc::detail::RetainCancelState(token);

    FakeClock clock;
    const auto expiry =
        vc::detail::Deadline::TimePoint(std::chrono::milliseconds(10));
    clock.now = expiry;
    const vc::detail::Deadline deadline =
        vc::detail::Deadline::At(expiry, &FakeClock::Read, &clock);
    vc::detail::RequestCancel(token);

    Check(vc::detail::CheckInterrupt(state, deadline) ==
              VC_ERR_CANCELLED,
          "explicit cancellation must win over timeout");
    vc::detail::FreeCancelToken(token);
    vc::detail::ReleaseCancelState(state);
}

void TestStaleHandleCannotAffectFreshToken() {
    vc_cancel_token* stale = nullptr;
    vc_error error = FreshError();
    Check(vc::detail::CreateCancelToken(&stale, &error) == VC_OK,
          "stale token creation");
    vc::detail::FreeCancelToken(stale);

    vc_cancel_token* fresh = nullptr;
    error = FreshError();
    Check(vc::detail::CreateCancelToken(&fresh, &error) == VC_OK,
          "fresh token creation");
    Check(fresh != stale,
          "reused storage must receive a distinct generation identity");
    vc::detail::CancelState* fresh_state =
        vc::detail::RetainCancelState(fresh);
    Check(fresh_state != nullptr, "fresh state retain");

    vc::detail::RequestCancel(stale);
    Check(vc::detail::RetainCancelState(stale) == nullptr,
          "stale handle retain must be rejected");
    Check(vc::detail::CheckInterrupt(
              fresh_state, vc::detail::Deadline::Infinite()) == VC_OK,
          "stale request must not cancel a newer token");

    if (fresh == stale) {
        vc::detail::FreeCancelToken(fresh);
        vc::detail::ReleaseCancelState(fresh_state);
        return;
    }

    vc::detail::FreeCancelToken(stale);
    Check(vc::detail::CheckInterrupt(
              fresh_state, vc::detail::Deadline::Infinite()) == VC_OK,
          "stale free must not dispose a newer token");
    vc::detail::RequestCancel(fresh);
    Check(vc::detail::CheckInterrupt(
              fresh_state, vc::detail::Deadline::Infinite()) ==
              VC_ERR_CANCELLED,
          "fresh handle remains independently usable");
    vc::detail::FreeCancelToken(fresh);
    vc::detail::ReleaseCancelState(fresh_state);
}

void TestBoundedStorageCanBeReused() {
    for (int iteration = 0; iteration < 20000; ++iteration) {
        vc_cancel_token* token = nullptr;
        vc_error error = FreshError();
        Check(vc::detail::CreateCancelToken(&token, &error) == VC_OK,
              "bounded token churn");
        if (token == nullptr) {
            return;
        }
        vc::detail::FreeCancelToken(token);
    }
}

void TestConcurrentRequestFreeRetainAndRelease() {
    for (int round = 0; round < 64; ++round) {
        vc_cancel_token* token = nullptr;
        vc_error error = FreshError();
        Check(vc::detail::CreateCancelToken(&token, &error) == VC_OK,
              "stress token creation");
        if (token == nullptr) {
            return;
        }

        vc::detail::CancelState* seed =
            vc::detail::RetainCancelState(token);
        vc::detail::CancelState* releases[4] = {
            vc::detail::RetainCancelState(token),
            vc::detail::RetainCancelState(token),
            vc::detail::RetainCancelState(token),
            vc::detail::RetainCancelState(token),
        };
        std::atomic<bool> start{false};
        std::thread requester([&]() {
            while (!start.load(std::memory_order_acquire)) {
                std::this_thread::yield();
            }
            for (int iteration = 0; iteration < 500; ++iteration) {
                vc::detail::RequestCancel(token);
            }
        });
        std::thread freer([&]() {
            while (!start.load(std::memory_order_acquire)) {
                std::this_thread::yield();
            }
            for (int iteration = 0; iteration < 500; ++iteration) {
                vc::detail::FreeCancelToken(token);
            }
        });
        std::thread retainer([&]() {
            while (!start.load(std::memory_order_acquire)) {
                std::this_thread::yield();
            }
            for (int iteration = 0; iteration < 500; ++iteration) {
                vc::detail::CancelState* retained =
                    vc::detail::RetainCancelState(token);
                if (retained != nullptr) {
                    vc::detail::ReleaseCancelState(retained);
                }
            }
        });
        std::thread releaser([&]() {
            while (!start.load(std::memory_order_acquire)) {
                std::this_thread::yield();
            }
            for (vc::detail::CancelState* state : releases) {
                vc::detail::ReleaseCancelState(state);
            }
        });

        start.store(true, std::memory_order_release);
        requester.join();
        freer.join();
        retainer.join();
        releaser.join();

        const int32_t observed = vc::detail::CheckInterrupt(
            seed, vc::detail::Deadline::Infinite());
        Check(observed == VC_OK || observed == VC_ERR_CANCELLED,
              "retained state survives concurrent public disposal");
        Check(vc::detail::RetainCancelState(token) == nullptr,
              "disposed token cannot be retained");
        vc::detail::FreeCancelToken(token);
        vc::detail::FreeCancelToken(token);
        vc::detail::ReleaseCancelState(seed);

        vc_cancel_token* fresh = nullptr;
        error = FreshError();
        Check(vc::detail::CreateCancelToken(&fresh, &error) == VC_OK,
              "post-stress token creation");
        vc::detail::CancelState* fresh_state =
            vc::detail::RetainCancelState(fresh);
        vc::detail::RequestCancel(token);
        vc::detail::FreeCancelToken(token);
        Check(vc::detail::CheckInterrupt(
                  fresh_state, vc::detail::Deadline::Infinite()) == VC_OK,
              "disposed generation cannot affect reused slot");
        vc::detail::FreeCancelToken(fresh);
        vc::detail::ReleaseCancelState(fresh_state);
    }
}

struct PinBarrier {
    size_t target_slot = 0u;
    uint32_t target_generation = 0u;
    std::atomic<bool> pinned{false};
    std::atomic<bool> allow_recheck{false};
    std::atomic<bool> released{false};

    static void OnEvent(vc::detail::CancelTestEvent event,
                        size_t slot_index,
                        uint32_t generation,
                        void* context) noexcept {
        PinBarrier& barrier =
            *static_cast<PinBarrier*>(context);
        if (slot_index != barrier.target_slot ||
            generation != barrier.target_generation) {
            return;
        }
        if (event ==
            vc::detail::CancelTestEvent::pin_acquired) {
            barrier.pinned.store(true, std::memory_order_release);
            while (!barrier.allow_recheck.load(
                std::memory_order_acquire)) {
                std::this_thread::yield();
            }
        } else if (
            event ==
            vc::detail::CancelTestEvent::slot_released) {
            barrier.released.store(true, std::memory_order_release);
        }
    }
};

bool WaitFor(const std::atomic<bool>& value) {
    for (int iteration = 0; iteration < 5000000; ++iteration) {
        if (value.load(std::memory_order_acquire)) {
            return true;
        }
        std::this_thread::yield();
    }
    return false;
}

void TestBarrierPinFreeLastReleaseAndSameSlotReuse() {
    vc::detail::CancelTestReset();
    vc_cancel_token* stale = nullptr;
    vc_error error = FreshError();
    Check(vc::detail::CreateCancelToken(&stale, &error) == VC_OK,
          "barrier stale token creation");
    const size_t target_slot =
        vc::detail::CancelTestHandleSlot(stale);
    const uint32_t stale_generation =
        vc::detail::CancelTestHandleGeneration(stale);

    PinBarrier barrier;
    barrier.target_slot = target_slot;
    barrier.target_generation = stale_generation;
    vc::detail::CancelTestSetHook(
        &PinBarrier::OnEvent, &barrier);
    std::thread requester(
        [&]() { vc::detail::RequestCancel(stale); });
    const bool pinned = WaitFor(barrier.pinned);
    Check(pinned, "request must stop after in-flight pin");

    vc::detail::FreeCancelToken(stale);
    Check(vc::detail::RetainCancelState(stale) == nullptr,
          "disposed token rejects retain while pin is held");

    vc::detail::CancelTestSetNextSlot(target_slot);
    vc_cancel_token* while_pinned = nullptr;
    error = FreshError();
    Check(vc::detail::CreateCancelToken(
              &while_pinned, &error) == VC_OK,
          "create while old pin is held");
    Check(vc::detail::CancelTestHandleSlot(while_pinned) !=
              target_slot,
          "in-flight pin prevents premature same-slot reuse");
    vc::detail::FreeCancelToken(while_pinned);

    barrier.allow_recheck.store(true, std::memory_order_release);
    requester.join();
    Check(barrier.released.load(std::memory_order_acquire),
          "in-flight pin performs the last release");
    vc::detail::CancelTestSetHook(nullptr, nullptr);

    vc::detail::CancelTestSetNextSlot(target_slot);
    vc_cancel_token* fresh = nullptr;
    error = FreshError();
    Check(vc::detail::CreateCancelToken(&fresh, &error) == VC_OK,
          "same-slot fresh token creation");
    Check(vc::detail::CancelTestHandleSlot(fresh) == target_slot,
          "fresh token must reuse the released slot");
    Check(vc::detail::CancelTestHandleGeneration(fresh) ==
              stale_generation + 1u,
          "same slot must receive the next generation");
    vc::detail::CancelState* fresh_state =
        vc::detail::RetainCancelState(fresh);

    vc::detail::RequestCancel(stale);
    vc::detail::FreeCancelToken(stale);
    Check(vc::detail::RetainCancelState(stale) == nullptr,
          "stale same-slot generation cannot be retained");
    Check(vc::detail::CheckInterrupt(
              fresh_state, vc::detail::Deadline::Infinite()) == VC_OK,
          "stale same-slot operations cannot affect fresh generation");

    vc::detail::FreeCancelToken(fresh);
    vc::detail::ReleaseCancelState(fresh_state);
    vc::detail::CancelTestReset();
}

void TestExactCapacityAndRecovery() {
    vc::detail::CancelTestReset();
    Check(vc::detail::CancelTestSlotCapacity() == 4096u,
          "cancel table capacity is exactly 4096");
    std::vector<vc_cancel_token*> tokens;
    tokens.reserve(4096u);
    for (size_t index = 0; index < 4096u; ++index) {
        vc_cancel_token* token = nullptr;
        vc_error error = FreshError();
        Check(vc::detail::CreateCancelToken(&token, &error) == VC_OK,
              "all 4096 slots can be simultaneously active");
        if (token == nullptr) {
            return;
        }
        tokens.push_back(token);
    }

    vc_cancel_token* overflow =
        reinterpret_cast<vc_cancel_token*>(1);
    vc_error overflow_error = FreshError();
    Check(vc::detail::CreateCancelToken(
              &overflow, &overflow_error) == VC_ERR_OOM,
          "4097th active token returns VC_ERR_OOM");
    Check(overflow_error.code == VC_ERR_OOM,
          "capacity error populates VC_ERR_OOM");
    Check(overflow == reinterpret_cast<vc_cancel_token*>(1),
          "capacity failure leaves output handle unchanged");

    const size_t reuse_index = 173u;
    vc_cancel_token* stale = tokens[reuse_index];
    const size_t stale_slot =
        vc::detail::CancelTestHandleSlot(stale);
    const uint32_t stale_generation =
        vc::detail::CancelTestHandleGeneration(stale);
    vc::detail::FreeCancelToken(stale);
    tokens[reuse_index] = nullptr;

    vc_cancel_token* replacement = nullptr;
    vc_error replacement_error = FreshError();
    Check(vc::detail::CreateCancelToken(
              &replacement, &replacement_error) == VC_OK,
          "capacity recovers after one release");
    Check(vc::detail::CancelTestHandleSlot(replacement) ==
              stale_slot,
          "only released slot is deterministically reused");
    Check(vc::detail::CancelTestHandleGeneration(replacement) ==
              stale_generation + 1u,
          "capacity recovery increments generation");
    vc::detail::CancelState* replacement_state =
        vc::detail::RetainCancelState(replacement);

    vc::detail::RequestCancel(stale);
    vc::detail::FreeCancelToken(stale);
    Check(vc::detail::RetainCancelState(stale) == nullptr,
          "capacity stale generation remains disposed");
    Check(vc::detail::CheckInterrupt(
              replacement_state,
              vc::detail::Deadline::Infinite()) == VC_OK,
          "capacity stale operations do not affect replacement");

    for (vc_cancel_token* token : tokens) {
        vc::detail::FreeCancelToken(token);
    }
    vc::detail::FreeCancelToken(replacement);
    vc::detail::ReleaseCancelState(replacement_state);

    vc_cancel_token* recovered = nullptr;
    vc_error recovered_error = FreshError();
    Check(vc::detail::CreateCancelToken(
              &recovered, &recovered_error) == VC_OK,
          "capacity fully recovers after release");
    vc::detail::FreeCancelToken(recovered);
    vc::detail::CancelTestReset();
}

void TestMaximumGenerationRetiresWithoutWrap() {
    vc::detail::CancelTestReset();
    Check(vc::detail::CancelTestMaximumGeneration() ==
              0x7fffffffu,
          "maximum generation fixture");
    Check(vc::detail::CancelTestSeedFreeSlot(
              0u, 0x7ffffffeu),
          "seed max-1 free generation");
    vc::detail::CancelTestSetNextSlot(0u);

    vc_cancel_token* maximum = nullptr;
    vc_error error = FreshError();
    Check(vc::detail::CreateCancelToken(&maximum, &error) == VC_OK,
          "max generation token creation");
    Check(vc::detail::CancelTestHandleSlot(maximum) == 0u,
          "max generation uses seeded slot");
    Check(vc::detail::CancelTestHandleGeneration(maximum) ==
              0x7fffffffu,
          "max-1 advances exactly to max");
    vc::detail::CancelState* maximum_state =
        vc::detail::RetainCancelState(maximum);
    vc::detail::FreeCancelToken(maximum);
    vc::detail::ReleaseCancelState(maximum_state);
    Check(vc::detail::CancelTestSlotIsFree(0u),
          "max generation reaches free terminal state");
    Check(vc::detail::CancelTestSlotGeneration(0u) ==
              0x7fffffffu,
          "retired slot preserves max generation");

    vc::detail::CancelTestSetNextSlot(0u);
    vc_cancel_token* fresh = nullptr;
    error = FreshError();
    Check(vc::detail::CreateCancelToken(&fresh, &error) == VC_OK,
          "creation skips retired slot");
    Check(vc::detail::CancelTestHandleSlot(fresh) != 0u,
          "max generation slot never wraps or aliases");
    vc::detail::CancelState* fresh_state =
        vc::detail::RetainCancelState(fresh);

    vc::detail::RequestCancel(maximum);
    vc::detail::FreeCancelToken(maximum);
    Check(vc::detail::RetainCancelState(maximum) == nullptr,
          "retired max-generation handle stays stale");
    Check(vc::detail::CheckInterrupt(
              fresh_state, vc::detail::Deadline::Infinite()) == VC_OK,
          "retired stale handle cannot affect fresh token");
    Check(vc::detail::CancelTestSlotGeneration(0u) ==
              0x7fffffffu &&
              vc::detail::CancelTestSlotIsFree(0u),
          "stale operations do not revive retired slot");

    vc::detail::FreeCancelToken(fresh);
    vc::detail::ReleaseCancelState(fresh_state);
    vc::detail::CancelTestReset();
}

}  // namespace

int main() {
    static_assert(std::atomic<uint32_t>::is_always_lock_free,
                  "cancel request/check require lock-free atomics");
    static_assert(std::atomic<uint64_t>::is_always_lock_free,
                  "cancel identity pinning requires lock-free atomics");
    TestCrossThreadRequestAndRetainedLifetime();
    TestFakeClockDeadlineIsDeterministic();
    TestFiniteZeroNegativeAndMaximumTimeouts();
    TestDeadlineSaturatesNearMaximumTimePoint();
    TestReferencePinRejectsZeroAndOverflow();
    TestCancellationWinsOverSimultaneousTimeout();
    TestStaleHandleCannotAffectFreshToken();
    TestBoundedStorageCanBeReused();
    TestConcurrentRequestFreeRetainAndRelease();
    TestBarrierPinFreeLastReleaseAndSameSlotReuse();
    TestExactCapacityAndRecovery();
    TestMaximumGenerationRetiresWithoutWrap();
    if (failures != 0) {
        std::cerr << failures << " cancel test(s) failed\n";
        return 1;
    }
    std::cout << "videocore cancel tests passed\n";
    return 0;
}
