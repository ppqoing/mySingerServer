#include <chrono>
#include <cstdlib>
#include <iostream>

#include "deadline.h"
#include "videocore/videocore.h"

namespace {

using namespace std::chrono_literals;

struct FakeClock {
    vc::detail::Deadline::TimePoint now{};

    static vc::detail::Deadline::TimePoint Read(
        const void* context) noexcept {
        return static_cast<const FakeClock*>(context)->now;
    }
};

void Check(bool condition, const char* message) {
    if (!condition) {
        std::cerr << "FAIL: " << message << '\n';
        std::exit(1);
    }
}

// Break caught: governor queue time is charged to a probe/frame operation
// even though the operation deadline is supposed to measure real work only.
void TestExtendExcludesGovernorWaitButNotRealIo() {
    FakeClock clock;
    vc::detail::Deadline deadline = vc::detail::Deadline::At(
        clock.now + 1s, &FakeClock::Read, &clock);

    const auto wait_started = deadline.Now();
    clock.now += 5s;
    deadline.Extend(deadline.Now() - wait_started);
    clock.now += 10ms;
    Check(vc::detail::CheckInterrupt(nullptr, deadline) == VC_OK,
          "five-second governor wait consumed one-second deadline");

    clock.now += 2s;
    Check(vc::detail::CheckInterrupt(nullptr, deadline) == VC_ERR_TIMEOUT,
          "real I/O time did not consume the extended deadline");
}

// Break caught: extending an infinite or near-maximum deadline wraps it into
// an already-expired time point.
void TestExtendIsInfiniteSafeAndSaturating() {
    FakeClock clock;
    vc::detail::Deadline infinite = vc::detail::Deadline::Infinite();
    infinite.Extend(std::chrono::nanoseconds::max());
    Check(!infinite.Expired(), "infinite deadline became active");

    const auto near_max = vc::detail::Deadline::TimePoint::max() - 1ns;
    vc::detail::Deadline deadline = vc::detail::Deadline::At(
        near_max, &FakeClock::Read, &clock);
    deadline.Extend(10ns);
    clock.now = vc::detail::Deadline::TimePoint::max() - 1ns;
    Check(!deadline.Expired(), "deadline extension overflowed");
    clock.now = vc::detail::Deadline::TimePoint::max();
    Check(deadline.Expired(), "saturated deadline never expires");
}

}  // namespace

int main() {
    TestExtendExcludesGovernorWaitButNotRealIo();
    TestExtendIsInfiniteSafeAndSaturating();
    std::cout << "VIDEOCORE_DEADLINE_OK\n";
    return 0;
}
