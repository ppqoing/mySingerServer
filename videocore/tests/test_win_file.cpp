#define WIN32_LEAN_AND_MEAN
#include <windows.h>

#include <array>
#include <chrono>
#include <cstdint>
#include <cstdlib>
#include <iostream>
#include <memory>
#include <string>

#include "deadline.h"
#include "videocore/videocore.h"
#include "win_file.h"

namespace {

using namespace std::chrono_literals;

enum class Event : uint32_t {
    acquire_read,
    acquire_seek,
    before_read,
    after_read,
    before_seek,
    after_seek,
    report,
};

struct Harness {
    vc::detail::Deadline::TimePoint now{};
    std::array<Event, 32> events{};
    size_t event_count = 0;
    std::chrono::nanoseconds acquire_wait = 0ns;
    std::chrono::nanoseconds io_elapsed = 0ns;
    int32_t acquire_status = VC_OK;
    uint64_t next_lease = 40u;
    uint64_t report_lease = 0u;
    uint64_t report_bytes = 0u;
    uint64_t report_elapsed_ns = 0u;
    int32_t report_status = VC_ERR_INTERNAL;

    void Push(Event event) noexcept {
        if (event_count < events.size()) events[event_count++] = event;
    }

    static vc::detail::Deadline::TimePoint Now(
        const void* context) noexcept {
        return static_cast<const Harness*>(context)->now;
    }
};

void Check(bool condition, const char* message) {
    if (!condition) {
        std::cerr << "FAIL: " << message << '\n';
        std::exit(1);
    }
}

vc_error FreshError() {
    vc_error error{};
    error.struct_size = sizeof(error);
    error.abi_version = VC_ABI_VERSION;
    return error;
}

int32_t VC_CALL Acquire(uintptr_t context,
                        uint32_t operation,
                        uint64_t requested_bytes,
                        uint64_t* lease_id,
                        uint64_t* granted_bytes,
                        vc_error*) {
    auto& harness = *reinterpret_cast<Harness*>(context);
    harness.Push(operation == VC_IO_OPERATION_READ
                     ? Event::acquire_read
                     : Event::acquire_seek);
    harness.now += harness.acquire_wait;
    if (harness.acquire_status != VC_OK) return harness.acquire_status;
    *lease_id = ++harness.next_lease;
    *granted_bytes = operation == VC_IO_OPERATION_READ
                         ? requested_bytes
                         : 0u;
    return VC_OK;
}

void VC_CALL Report(uintptr_t context,
                    uint64_t lease_id,
                    uint64_t actual_bytes,
                    uint64_t elapsed_ns,
                    int32_t status) {
    auto& harness = *reinterpret_cast<Harness*>(context);
    harness.Push(Event::report);
    harness.report_lease = lease_id;
    harness.report_bytes = actual_bytes;
    harness.report_elapsed_ns = elapsed_ns;
    harness.report_status = status;
}

void IoHook(vc::detail::IoBoundary boundary, void* context) noexcept {
    auto& harness = *static_cast<Harness*>(context);
    switch (boundary) {
        case vc::detail::IoBoundary::before_read:
            harness.Push(Event::before_read);
            break;
        case vc::detail::IoBoundary::after_read:
            harness.now += harness.io_elapsed;
            harness.Push(Event::after_read);
            break;
        case vc::detail::IoBoundary::before_seek:
            harness.Push(Event::before_seek);
            break;
        case vc::detail::IoBoundary::after_seek:
            harness.now += harness.io_elapsed;
            harness.Push(Event::after_seek);
            break;
        default:
            break;
    }
}

std::wstring MakeFixture() {
    wchar_t directory[MAX_PATH]{};
    wchar_t path[MAX_PATH]{};
    Check(GetTempPathW(MAX_PATH, directory) != 0u,
          "GetTempPathW failed");
    Check(GetTempFileNameW(directory, L"vcg", 0u, path) != 0u,
          "GetTempFileNameW failed");
    HANDLE file = CreateFileW(path, GENERIC_WRITE, 0u, nullptr,
                              CREATE_ALWAYS, FILE_ATTRIBUTE_NORMAL, nullptr);
    Check(file != INVALID_HANDLE_VALUE, "fixture CreateFileW failed");
    const std::array<uint8_t, 4> bytes{1u, 2u, 3u, 4u};
    DWORD written = 0u;
    Check(WriteFile(file, bytes.data(), static_cast<DWORD>(bytes.size()),
                    &written, nullptr) != FALSE && written == bytes.size(),
          "fixture WriteFile failed");
    CloseHandle(file);
    return path;
}

std::unique_ptr<vc::detail::WinFile> OpenFixture(
    const std::wstring& path,
    Harness* harness) {
    vc_io_governor governor{
        sizeof(vc_io_governor), VC_ABI_VERSION,
        reinterpret_cast<uintptr_t>(harness), &Acquire, &Report};
    vc_error error = FreshError();
    std::unique_ptr<vc::detail::WinFile> file;
    const int32_t status = vc::detail::WinFile::Open(
        path, nullptr, vc::detail::Deadline::Infinite(), &file, &error,
        &governor);
    Check(status == VC_OK && file != nullptr, "WinFile::Open failed");
    file->SetIoHook(&IoHook, harness);
    return file;
}

// Break caught: ReadFile/SetFilePointerEx executes before a lease is granted,
// or a rejected grant still touches the source handle.
void TestAcquirePrecedesHandleAndRejectionDoesNotTouchIt() {
    const std::wstring path = MakeFixture();
    Harness harness;
    auto file = OpenFixture(path, &harness);
    vc::detail::Deadline deadline = vc::detail::Deadline::At(
        harness.now + 1s, &Harness::Now, &harness);
    std::array<uint8_t, 4> bytes{};
    int bytes_read = 0;
    vc_error error = FreshError();
    Check(file->Read(bytes.data(), static_cast<int>(bytes.size()),
                     &bytes_read, nullptr, &deadline, &error) == VC_OK,
          "governed read failed");
    Check(harness.events[0] == Event::acquire_read &&
              harness.events[1] == Event::before_read,
          "read acquire did not precede before_read");

    harness.event_count = 0;
    int64_t position = -1;
    Check(file->Seek(0, FILE_BEGIN, &position, nullptr, &deadline, &error) ==
              VC_OK,
          "governed seek failed");
    Check(harness.events[0] == Event::acquire_seek &&
              harness.events[1] == Event::before_seek,
          "seek acquire did not precede before_seek");

    const auto before = file->stats();
    harness.event_count = 0;
    harness.acquire_status = VC_ERR_CANCELLED;
    Check(file->Read(bytes.data(), 1, &bytes_read, nullptr, &deadline,
                     &error) == VC_ERR_CANCELLED,
          "rejected read did not return governor status");
    Check(file->Seek(0, FILE_BEGIN, &position, nullptr, &deadline, &error) ==
              VC_ERR_CANCELLED,
          "rejected seek did not return governor status");
    const auto after = file->stats();
    Check(after.read_calls == before.read_calls &&
              after.seek_calls == before.seek_calls,
          "rejected governor operation touched the handle");
    Check(harness.event_count == 2u &&
              harness.events[0] == Event::acquire_read &&
              harness.events[1] == Event::acquire_seek,
          "rejected operation reached an I/O boundary");
    file.reset();
    DeleteFileW(path.c_str());
}

// Break caught: five seconds of governor waiting consumes a one-second
// operation budget, or reports requested bytes/wall time instead of actual
// transfer bytes and real I/O time.
void TestWaitExtendsSameDeadlineAndReportUsesRealIo() {
    const std::wstring path = MakeFixture();
    Harness harness;
    harness.acquire_wait = 5s;
    harness.io_elapsed = 10ms;
    auto file = OpenFixture(path, &harness);
    vc::detail::Deadline deadline = vc::detail::Deadline::At(
        harness.now + 1s, &Harness::Now, &harness);
    std::array<uint8_t, 8> bytes{};
    int bytes_read = 0;
    vc_error error = FreshError();
    Check(file->Read(bytes.data(), static_cast<int>(bytes.size()),
                     &bytes_read, nullptr, &deadline, &error) == VC_OK,
          "governor wait incorrectly timed out read");
    Check(bytes_read == 4 && harness.report_bytes == 4u,
          "report did not use actual short-read bytes");
    Check(harness.report_elapsed_ns == 10'000'000u &&
              harness.report_status == VC_OK,
          "report did not use real I/O duration/status");

    harness.acquire_wait = 0ns;
    harness.io_elapsed = 2s;
    int64_t position = -1;
    Check(file->Seek(0, FILE_BEGIN, &position, nullptr, &deadline, &error) ==
              VC_ERR_TIMEOUT,
          "real two-second I/O did not time out");
    Check(harness.report_elapsed_ns == 2'000'000'000u &&
              harness.report_status == VC_ERR_TIMEOUT,
          "timed-out I/O report was inaccurate");
    file.reset();
    DeleteFileW(path.c_str());
}

}  // namespace

int main() {
    TestAcquirePrecedesHandleAndRejectionDoesNotTouchIt();
    TestWaitExtendsSameDeadlineAndReportUsesRealIo();
    std::cout << "VIDEOCORE_WIN_FILE_OK\n";
    return 0;
}
