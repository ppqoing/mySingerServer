#define WIN32_LEAN_AND_MEAN
#include <windows.h>

#include <array>
#include <atomic>
#include <chrono>
#include <cstdint>
#include <iostream>
#include <string>
#include <thread>
#include <vector>

#include "deadline.h"
#include "media_session.h"
#include "video_analysis.h"
#include "videocore/videocore.h"
#include "win_file.h"

namespace {

int failures = 0;

void Check(bool condition, const std::string& message) {
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

vc_media_open_options FreshOptions(uint32_t media_type) {
    vc_media_open_options options{};
    options.struct_size = sizeof(options);
    options.abi_version = VC_ABI_VERSION;
    options.expected_media_type = media_type;
    options.image_max_bytes = 1024u * 1024u;
    return options;
}

void InitializeFeatureSet(vc_feature_set* features) {
    features->struct_size = sizeof(*features);
    features->abi_version = VC_ABI_VERSION;
}

vc_analysis_result FreshResult() {
    vc_analysis_result result{};
    result.struct_size = sizeof(result);
    result.abi_version = VC_ABI_VERSION;
    InitializeFeatureSet(&result.image_features);
    InitializeFeatureSet(&result.contact_sheet_features);
    for (auto& frame : result.frames) {
        frame.struct_size = sizeof(frame);
        frame.abi_version = VC_ABI_VERSION;
        InitializeFeatureSet(&frame.features);
    }
    return result;
}

vc_analysis_request FreshVideoRequest() {
    vc_analysis_request request{};
    request.struct_size = sizeof(request);
    request.abi_version = VC_ABI_VERSION;
    request.feature_mask = VC_FEATURE_DURATION | VC_FEATURE_PDQ;
    request.frame_mask = 1u;
    request.probe_timeout_ms = 15000u;
    request.frame_timeout_ms = 20000u;
    return request;
}

std::wstring MakeTemporaryPath(const wchar_t* suffix) {
    wchar_t directory[MAX_PATH]{};
    const DWORD count = GetTempPathW(MAX_PATH, directory);
    Check(count != 0u && count < MAX_PATH,
          "GetTempPathW returns a usable directory");
    static LONG sequence = 0;
    return std::wstring(directory) + L"videocore-resilience-" +
           std::to_wstring(GetCurrentProcessId()) + L"-" +
           std::to_wstring(InterlockedIncrement(&sequence)) + suffix;
}

bool WriteBytes(const std::wstring& path,
                const std::vector<uint8_t>& bytes) {
    HANDLE file = CreateFileW(path.c_str(), GENERIC_WRITE,
                              FILE_SHARE_READ | FILE_SHARE_WRITE |
                                  FILE_SHARE_DELETE,
                              nullptr, CREATE_ALWAYS,
                              FILE_ATTRIBUTE_NORMAL, nullptr);
    if (file == INVALID_HANDLE_VALUE) return false;
    DWORD written = 0u;
    const bool ok = WriteFile(file, bytes.data(),
                              static_cast<DWORD>(bytes.size()),
                              &written, nullptr) != FALSE &&
                    written == bytes.size();
    CloseHandle(file);
    return ok;
}

int32_t Open(const std::wstring& path,
             const vc_media_open_options& options,
             vc_cancel_token* cancel,
             vc_media_session** out,
             vc_error* error) {
    return vc_media_open_w(
        reinterpret_cast<const uint16_t*>(path.data()),
        static_cast<uint32_t>(path.size()),
        &options, cancel, out, error);
}

bool WaitFor(const std::atomic<bool>& value) {
    for (int iteration = 0; iteration < 5000000; ++iteration) {
        if (value.load(std::memory_order_acquire)) return true;
        std::this_thread::yield();
    }
    return false;
}

struct BlockingRead {
    std::atomic<bool> entered{false};
    std::atomic<bool> release{false};

    static void Run(vc::detail::IoBoundary boundary,
                    void* context) noexcept {
        if (boundary != vc::detail::IoBoundary::before_read) return;
        auto* state = static_cast<BlockingRead*>(context);
        state->entered.store(true, std::memory_order_release);
        while (!state->release.load(std::memory_order_acquire)) {
            std::this_thread::yield();
        }
    }
};

struct CancelAtBoundary {
    vc_cancel_token* token = nullptr;
    vc::detail::OperationBoundary target =
        vc::detail::OperationBoundary::open;
    std::atomic<uint32_t> hits{0u};

    static void Run(vc::detail::OperationBoundary boundary,
                    void* context) noexcept {
        auto* state = static_cast<CancelAtBoundary*>(context);
        if (state != nullptr && boundary == state->target &&
            state->hits.fetch_add(1u, std::memory_order_acq_rel) == 0u) {
            vc_cancel_request(state->token);
        }
    }
};

struct FakeClock {
    vc::detail::Deadline::TimePoint now{};

    static vc::detail::Deadline::TimePoint Read(
        const void* context) noexcept {
        return static_cast<const FakeClock*>(context)->now;
    }
};

void TestFakeClockTimeoutAtEveryBoundary() {
    FakeClock clock;
    const auto expiry = vc::detail::Deadline::TimePoint(
        std::chrono::milliseconds(10));
    const auto deadline = vc::detail::Deadline::At(
        expiry, &FakeClock::Read, &clock);
    for (const auto boundary : {
             vc::detail::OperationBoundary::open,
             vc::detail::OperationBoundary::hash_read,
             vc::detail::OperationBoundary::probe,
             vc::detail::OperationBoundary::seek,
             vc::detail::OperationBoundary::packet_read,
             vc::detail::OperationBoundary::decode,
             vc::detail::OperationBoundary::feature,
             vc::detail::OperationBoundary::jpeg_encode}) {
        clock.now = expiry - std::chrono::milliseconds(1);
        Check(vc::detail::CheckOperationBoundary(
                  nullptr, deadline, boundary) == VC_OK,
              "fake clock stays live before boundary deadline");
        clock.now = expiry;
        Check(vc::detail::CheckOperationBoundary(
                  nullptr, deadline, boundary) == VC_ERR_TIMEOUT,
              "fake clock expires exactly at boundary deadline");
    }
}

void TestOpenAndHashInterruptBoundaries(const std::wstring& path) {
    for (const auto boundary : {
             vc::detail::OperationBoundary::open,
             vc::detail::OperationBoundary::hash_read}) {
        vc_cancel_token* token = nullptr;
        vc_error error = FreshError();
        Check(vc_cancel_create(&token, &error) == VC_OK,
              "interrupt token creation");
        CancelAtBoundary hook{token, boundary};
        vc::detail::SetOperationBoundaryTestHook(
            &CancelAtBoundary::Run, &hook);
        vc_media_session* session = nullptr;
        const auto options = FreshOptions(VC_MEDIA_TYPE_IMAGE);
        const int32_t open_status =
            Open(path, options, token, &session, &error);
        if (boundary == vc::detail::OperationBoundary::open) {
            Check(open_status == VC_ERR_CANCELLED,
                  "open boundary observes cancellation");
            Check(session == nullptr,
                  "cancelled open publishes no session");
        } else {
            Check(open_status == VC_OK,
                  "hash-boundary fixture opens before cancellation");
            std::array<uint8_t, VC_SHA512_SIZE> digest{};
            error = FreshError();
            Check(vc_media_hash(session, digest.data(), &error) ==
                      VC_ERR_CANCELLED,
                  "hash read boundary observes cancellation");
        }
        vc_media_close(session);
        vc_cancel_free(token);
        vc::detail::SetOperationBoundaryTestHook(nullptr, nullptr);
        Check(hook.hits.load(std::memory_order_acquire) == 1u,
              "target boundary is reached exactly once before cancel");
    }
}

void TestVideoInterruptBoundaries() {
    const std::wstring path =
        std::wstring(VC_VIDEO_TESTDATA_ROOT) + L"\\h264-standard.mp4";
    const auto boundaries = {
        vc::detail::OperationBoundary::probe,
        vc::detail::OperationBoundary::seek,
        vc::detail::OperationBoundary::packet_read,
        vc::detail::OperationBoundary::decode,
        vc::detail::OperationBoundary::feature,
        vc::detail::OperationBoundary::jpeg_encode,
    };
    for (const auto boundary : boundaries) {
        vc_cancel_token* token = nullptr;
        vc_error error = FreshError();
        Check(vc_cancel_create(&token, &error) == VC_OK,
              "video interrupt token creation");
        vc_media_session* session = nullptr;
        const auto options = FreshOptions(VC_MEDIA_TYPE_VIDEO);
        Check(Open(path, options, token, &session, &error) == VC_OK,
              "video boundary fixture opens");
        std::array<uint8_t, VC_SHA512_SIZE> digest{};
        error = FreshError();
        Check(vc_media_hash(session, digest.data(), &error) == VC_OK,
              "video boundary fixture hashes");

        CancelAtBoundary hook{token, boundary};
        vc::detail::SetOperationBoundaryTestHook(
            &CancelAtBoundary::Run, &hook);
        vc_analysis_request request = FreshVideoRequest();
        const std::wstring jpeg = MakeTemporaryPath(L".jpg");
        if (boundary == vc::detail::OperationBoundary::jpeg_encode) {
            request.feature_mask = VC_FEATURE_CONTACT_SHEET;
            request.frame_mask = 0u;
            request.temporary_jpeg_path =
                reinterpret_cast<const uint16_t*>(jpeg.data());
            request.temporary_jpeg_path_units =
                static_cast<uint32_t>(jpeg.size());
        }
        vc_analysis_result result = FreshResult();
        error = FreshError();
        const int32_t status =
            vc_media_analyze(session, &request, &result, &error);
        Check(status == VC_ERR_CANCELLED,
              "video stage returns VC_ERR_CANCELLED");
        Check(error.code == VC_ERR_CANCELLED,
              "video stage populates cancelled error");
        Check(result.completed_frame_mask == 0u,
              "cancelled video stage publishes no frame payload");
        Check(hook.hits.load(std::memory_order_acquire) == 1u,
              "requested video boundary was reached");
        Check(GetFileAttributesW(jpeg.c_str()) == INVALID_FILE_ATTRIBUTES,
              "cancelled JPEG boundary leaves no output");
        DeleteFileW(jpeg.c_str());
        vc::detail::SetOperationBoundaryTestHook(nullptr, nullptr);
        vc_media_close(session);
        vc_cancel_free(token);
    }
}

void TestSameSessionRejectsSecondCallAndDifferentSessionsRunTogether(
    const std::wstring& path) {
    vc_media_session* first_session = nullptr;
    vc_media_session* second_session = nullptr;
    auto options = FreshOptions(VC_MEDIA_TYPE_IMAGE);
    vc_error error = FreshError();
    Check(Open(path, options, nullptr, &first_session, &error) == VC_OK,
          "first parallel session opens");
    error = FreshError();
    Check(Open(path, options, nullptr, &second_session, &error) == VC_OK,
          "second parallel session opens");
    BlockingRead first_block;
    BlockingRead second_block;
    Check(vc::detail::SetMediaSessionTestIoHook(
              first_session, &BlockingRead::Run, &first_block),
          "first session blocking hook attaches");
    Check(vc::detail::SetMediaSessionTestIoHook(
              second_session, &BlockingRead::Run, &second_block),
          "second session blocking hook attaches");

    std::array<uint8_t, VC_SHA512_SIZE> first_digest{};
    std::array<uint8_t, VC_SHA512_SIZE> second_digest{};
    int32_t first_status = VC_ERR_INTERNAL;
    int32_t second_status = VC_ERR_INTERNAL;
    std::thread first([&]() {
        vc_error local = FreshError();
        first_status = vc_media_hash(
            first_session, first_digest.data(), &local);
    });
    Check(WaitFor(first_block.entered),
          "first session reaches blocking read");

    error = FreshError();
    std::array<uint8_t, VC_SHA512_SIZE> rejected{};
    Check(vc_media_hash(first_session, rejected.data(), &error) ==
              VC_ERR_INVALID_ARG,
          "same session second business call is rejected");

    std::thread second([&]() {
        vc_error local = FreshError();
        second_status = vc_media_hash(
            second_session, second_digest.data(), &local);
    });
    Check(WaitFor(second_block.entered),
          "different session reaches read while first is active");
    first_block.release.store(true, std::memory_order_release);
    second_block.release.store(true, std::memory_order_release);
    first.join();
    second.join();
    Check(first_status == VC_OK && second_status == VC_OK,
          "different sessions complete independently");
    vc_media_close(first_session);
    vc_media_close(second_session);
}

void TestFiveHundredResourceCycles() {
    const std::wstring path =
        std::wstring(VC_VIDEO_TESTDATA_ROOT) + L"\\h264-short.mp4";
    const std::wstring jpeg = MakeTemporaryPath(L"-resource-loop.jpg");
    vc_media_open_options options = FreshOptions(VC_MEDIA_TYPE_VIDEO);
    vc_analysis_request request{};
    request.struct_size = sizeof(request);
    request.abi_version = VC_ABI_VERSION;
    request.feature_mask = VC_FEATURE_CONTACT_SHEET;
    request.frame_mask = VC_ALL_FRAME_MASK;
    request.probe_timeout_ms = 15000u;
    request.frame_timeout_ms = 20000u;
    request.contact_sheet_tile_max_side = 8u;
    request.temporary_jpeg_path =
        reinterpret_cast<const uint16_t*>(jpeg.data());
    request.temporary_jpeg_path_units =
        static_cast<uint32_t>(jpeg.size());

    vc_media_session* warmup = nullptr;
    vc_error error = FreshError();
    Check(Open(path, options, nullptr, &warmup, &error) == VC_OK,
          "resource-loop warmup opens");
    std::array<uint8_t, VC_SHA512_SIZE> digest{};
    error = FreshError();
    Check(vc_media_hash(warmup, digest.data(), &error) == VC_OK,
          "resource-loop warmup hashes");
    vc_analysis_result result = FreshResult();
    error = FreshError();
    Check(vc_media_analyze(warmup, &request, &result, &error) == VC_OK,
          "resource-loop warmup analyzes video and writes JPEG");
    vc_media_close(warmup);
    Check(DeleteFileW(jpeg.c_str()) != FALSE,
          "resource-loop warmup JPEG is removed");

    DWORD handles_before = 0u;
    Check(GetProcessHandleCount(
              GetCurrentProcess(), &handles_before) != FALSE,
          "resource-loop initial handle count");
    const auto acquisitions_before =
        vc::detail::VideoAnalysisTestResourceAcquisitions();
    for (int iteration = 0; iteration < 500; ++iteration) {
        const auto iteration_before =
            vc::detail::VideoAnalysisTestResourceAcquisitions();
        vc_media_session* session = nullptr;
        error = FreshError();
        Check(Open(path, options, nullptr, &session, &error) == VC_OK,
              "500-cycle session opens");
        digest.fill(0u);
        error = FreshError();
        Check(vc_media_hash(session, digest.data(), &error) == VC_OK,
              "500-cycle session hashes");
        result = FreshResult();
        error = FreshError();
        Check(vc_media_analyze(session, &request, &result, &error) == VC_OK,
              "500-cycle session analyzes committed video fixture");
        Check(result.contact_sheet_status == VC_OK,
              "500-cycle contact sheet succeeds");
        Check(GetFileAttributesW(jpeg.c_str()) != INVALID_FILE_ATTRIBUTES,
              "500-cycle JPEG is published");
        vc_media_close(session);
        Check(DeleteFileW(jpeg.c_str()) != FALSE,
              "500-cycle JPEG is removed");
        const auto iteration_after =
            vc::detail::VideoAnalysisTestResourceAcquisitions();
        Check(iteration_after.formats > iteration_before.formats,
              "each cycle acquires AVFormatContext");
        Check(iteration_after.codecs > iteration_before.codecs,
              "each cycle acquires AVCodecContext");
        Check(iteration_after.packets > iteration_before.packets,
              "each cycle acquires AVPacket");
        Check(iteration_after.frames > iteration_before.frames,
              "each cycle acquires AVFrame");
        Check(iteration_after.scalers > iteration_before.scalers,
              "each cycle acquires video SwsContext");
        Check(iteration_after.contact_scalers >
                  iteration_before.contact_scalers,
              "each cycle acquires contact-sheet SwsContext");
        Check(iteration_after.turbo_compressors >
                  iteration_before.turbo_compressors,
              "each cycle acquires TurboJPEG compressor");
        Check(iteration_after.turbo_buffers >
                  iteration_before.turbo_buffers,
              "each cycle acquires TurboJPEG buffer");
        Check(iteration_after.jpeg_handles >
                  iteration_before.jpeg_handles,
              "each cycle acquires JPEG output HANDLE");
        const auto live = vc::detail::VideoAnalysisTestLiveResources();
        Check(live.Total() == 0u,
              "live native resource count returns to zero");
    }
    DWORD handles_after = 0u;
    Check(GetProcessHandleCount(
              GetCurrentProcess(), &handles_after) != FALSE,
          "resource-loop final handle count");
    Check(handles_after == handles_before,
          "500 video hash/analyze/close cycles do not leak HANDLEs");
    const auto acquisitions_after =
        vc::detail::VideoAnalysisTestResourceAcquisitions();
    std::cout << "RESILIENCE_RESOURCE_LOOP iterations=500 handles_before="
              << handles_before << " handles_after=" << handles_after
              << " live_native=0"
              << " formats="
              << acquisitions_after.formats - acquisitions_before.formats
              << " codecs="
              << acquisitions_after.codecs - acquisitions_before.codecs
              << " packets="
              << acquisitions_after.packets - acquisitions_before.packets
              << " frames="
              << acquisitions_after.frames - acquisitions_before.frames
              << " video_sws="
              << acquisitions_after.scalers - acquisitions_before.scalers
              << " contact_sws="
              << acquisitions_after.contact_scalers -
                     acquisitions_before.contact_scalers
              << " turbo="
              << acquisitions_after.turbo_compressors -
                     acquisitions_before.turbo_compressors
              << " turbo_buffers="
              << acquisitions_after.turbo_buffers -
                     acquisitions_before.turbo_buffers
              << " jpeg_handles="
              << acquisitions_after.jpeg_handles -
                     acquisitions_before.jpeg_handles
              << '\n';
}

}  // namespace

int main() {
    const std::wstring path = MakeTemporaryPath(L".bin");
    std::vector<uint8_t> bytes(256u * 1024u, 0x5au);
    Check(WriteBytes(path, bytes), "resilience fixture write");
    TestFakeClockTimeoutAtEveryBoundary();
    TestOpenAndHashInterruptBoundaries(path);
    TestVideoInterruptBoundaries();
    TestSameSessionRejectsSecondCallAndDifferentSessionsRunTogether(path);
    TestFiveHundredResourceCycles();
    DeleteFileW(path.c_str());
    vc::detail::SetOperationBoundaryTestHook(nullptr, nullptr);
    if (failures != 0) {
        std::cerr << failures << " resilience test(s) failed\n";
        return 1;
    }
    std::cout << "videocore resilience tests passed\n";
    return 0;
}
