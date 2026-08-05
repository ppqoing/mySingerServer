#define WIN32_LEAN_AND_MEAN
#include <windows.h>

#include <array>
#include <atomic>
#include <chrono>
#include <cstdint>
#include <cstring>
#include <iostream>
#include <memory>
#include <string>
#include <thread>
#include <vector>

#include "avio_bridge.h"
#include "cancel_token.h"
#include "deadline.h"
#include "media_session.h"
#include "native_algorithms/sha512.h"
#include "videocore/videocore.h"
#include "win_file.h"

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

vc_media_open_options FreshOptions(uint32_t media_type) {
    vc_media_open_options options{};
    options.struct_size = sizeof(options);
    options.abi_version = VC_ABI_VERSION;
    options.expected_media_type = media_type;
    options.image_max_bytes = 1024u * 1024u;
    return options;
}

std::wstring MakeTemporaryPath(const wchar_t* suffix) {
    wchar_t directory[MAX_PATH]{};
    const DWORD count = GetTempPathW(MAX_PATH, directory);
    Check(count != 0u && count < MAX_PATH,
          "GetTempPathW must return a usable directory");
    static LONG sequence = 0;
    const LONG id = InterlockedIncrement(&sequence);
    return std::wstring(directory) + L"videocore-session-" +
           std::to_wstring(GetCurrentProcessId()) + L"-" +
           std::to_wstring(id) + suffix;
}

bool WriteBytes(const std::wstring& path,
                const std::vector<uint8_t>& bytes) {
    HANDLE file = CreateFileW(path.c_str(),
                              GENERIC_WRITE,
                              FILE_SHARE_READ | FILE_SHARE_WRITE |
                                  FILE_SHARE_DELETE,
                              nullptr,
                              CREATE_ALWAYS,
                              FILE_ATTRIBUTE_NORMAL,
                              nullptr);
    if (file == INVALID_HANDLE_VALUE) {
        return false;
    }
    DWORD written = 0;
    const bool ok =
        bytes.empty() ||
        (WriteFile(file,
                   bytes.data(),
                   static_cast<DWORD>(bytes.size()),
                   &written,
                   nullptr) != FALSE &&
         written == bytes.size());
    CloseHandle(file);
    return ok;
}

std::array<uint8_t, VC_SHA512_SIZE> ParseDigest(
    const char* hex) {
    std::array<uint8_t, VC_SHA512_SIZE> digest{};
    auto nibble = [](char value) -> uint8_t {
        if (value >= '0' && value <= '9') {
            return static_cast<uint8_t>(value - '0');
        }
        return static_cast<uint8_t>(value - 'a' + 10);
    };
    for (size_t index = 0; index < digest.size(); ++index) {
        digest[index] = static_cast<uint8_t>(
            (nibble(hex[index * 2u]) << 4u) |
            nibble(hex[index * 2u + 1u]));
    }
    return digest;
}

std::string DigestHex(
    const std::array<uint8_t, VC_SHA512_SIZE>& digest) {
    constexpr char digits[] = "0123456789abcdef";
    std::string hex(digest.size() * 2u, '0');
    for (size_t index = 0; index < digest.size(); ++index) {
        hex[index * 2u] = digits[digest[index] >> 4u];
        hex[index * 2u + 1u] = digits[digest[index] & 0x0fu];
    }
    return hex;
}

int32_t Open(const std::wstring& path,
             const vc_media_open_options& options,
             vc_cancel_token* cancel,
             vc_media_session** out,
             vc_error* error) {
    return vc_media_open_w(
        reinterpret_cast<const uint16_t*>(path.data()),
        static_cast<uint32_t>(path.size()),
        &options,
        cancel,
        out,
        error);
}

void TestSha512StandardVectors() {
    struct Vector {
        const char* name;
        const char* bytes;
        const char* digest;
    };
    const Vector vectors[] = {
        {
            "empty",
            "",
            "cf83e1357eefb8bdf1542850d66d8007d620e4050b5715dc83f4a921"
            "d36ce9ce47d0d13c5d85f2b0ff8318d2877eec2f63b931bd47417a8"
            "1a538327af927da3e",
        },
        {
            "abc",
            "abc",
            "ddaf35a193617abacc417349ae20413112e6fa4e89a97ea20a9eeee"
            "64b55d39a2192992a274fc1a836ba3c23a3feebbd454d4423643ce8"
            "0e2a9ac94fa54ca49f",
        },
        {
            "multi-block",
            "abcdefghbcdefghicdefghijdefghijkefghijklfghijklmghijklmn"
            "hijklmnoijklmnopjklmnopqklmnopqrlmnopqrsmnopqrstnopqrstu",
            "8e959b75dae313da8cf4f72814fc143f8f7779c6eb9f7fa17299aead"
            "b6889018501d289e4900f7e4331b99dec4b5433ac7d329eeb6dd2654"
            "5e96e55b874be909",
        },
    };

    for (const Vector& vector : vectors) {
        const std::wstring path = MakeTemporaryPath(L".bin");
        const auto* begin =
            reinterpret_cast<const uint8_t*>(vector.bytes);
        const std::vector<uint8_t> bytes(
            begin, begin + std::strlen(vector.bytes));
        Check(WriteBytes(path, bytes), "SHA fixture write");

        vc_media_open_options options =
            FreshOptions(VC_MEDIA_TYPE_AUTO);
        vc_media_session* session = nullptr;
        vc_error error = FreshError();
        const int32_t open_status =
            Open(path, options, nullptr, &session, &error);
        Check(open_status == VC_OK, vector.name);
        if (open_status == VC_OK) {
            std::array<uint8_t, VC_SHA512_SIZE> actual{};
            error = FreshError();
            Check(vc_media_hash(session, actual.data(), &error) == VC_OK,
                  vector.name);
            Check(actual == ParseDigest(vector.digest), vector.name);
            std::cout << "SHA512_VECTOR"
                      << " name=" << vector.name
                      << " digest=" << DigestHex(actual) << '\n';
            vc_media_close(session);
        }
        DeleteFileW(path.c_str());
    }
}

void TestNativeSha512IncrementalUpdates() {
    vc::native::Sha512 hash;
    const uint8_t first[] = {'a'};
    const uint8_t second[] = {'b', 'c'};
    hash.Update(first, std::size(first));
    hash.Update(second, std::size(second));
    Check(hash.Final() == ParseDigest(
                              "ddaf35a193617abacc417349ae20413112e6fa4e89"
                              "a97ea20a9eeee64b55d39a2192992a274fc1a836ba"
                              "3c23a3feebbd454d4423643ce80e2a9ac94fa54ca4"
                              "9f"),
          "native SHA-512 supports incremental updates");
}

void TestWinFileAndAvioShareReadSeekSizeState() {
    const std::wstring path = MakeTemporaryPath(L"-avio.bin");
    Check(WriteBytes(path, {'0', '1', '2', '3', '4', '5'}),
          "AVIO fixture write");
    std::unique_ptr<vc::detail::WinFile> file;
    vc_error error = FreshError();
    Check(vc::detail::WinFile::Open(path,
                                    nullptr,
                                    vc::detail::Deadline::Infinite(),
                                    &file,
                                    &error) == VC_OK,
          "WinFile opens fixture");
    if (file != nullptr) {
        const vc::detail::WinFileSnapshot& snapshot = file->snapshot();
        Check(snapshot.size == 6u, "WinFile snapshots file size");
        Check(snapshot.last_write_time != 0u,
              "WinFile snapshots last-write time");
        Check(snapshot.identity.volume_serial != 0u,
              "WinFile snapshots volume identity");
        std::cout << "FILE_SNAPSHOT"
                  << " volume=" << snapshot.identity.volume_serial
                  << " id_high=" << snapshot.identity.file_id_high
                  << " id_low=" << snapshot.identity.file_id_low
                  << " size=" << snapshot.size
                  << " mtime=" << snapshot.last_write_time << '\n';

        vc::detail::AvioOpaque opaque{
            file.get(),
            nullptr,
            vc::detail::Deadline::Infinite(),
        };
        Check(vc::detail::SeekPacket(&opaque, 0, AVSEEK_SIZE) == 6,
              "AVIO size query returns snapshotted size");
        uint8_t bytes[3]{};
        Check(vc::detail::ReadPacket(&opaque, bytes, 3) == 3,
              "AVIO reads from WinFile handle");
        Check(std::memcmp(bytes, "012", 3u) == 0,
              "AVIO read payload");
        Check(vc::detail::SeekPacket(&opaque, 2, SEEK_SET) == 2,
              "AVIO seeks shared WinFile handle");
        Check(vc::detail::ReadPacket(&opaque, bytes, 2) == 2,
              "AVIO reads after seek");
        Check(std::memcmp(bytes, "23", 2u) == 0,
              "AVIO read after seek payload");
        Check(vc::detail::SeekPacket(
                  &opaque, 0, AVSEEK_SIZE | AVSEEK_FORCE) == 6,
              "forced AVIO size query returns snapshotted size");
        Check(vc::detail::SeekPacket(
                  &opaque, 1, SEEK_SET | AVSEEK_FORCE) == 1,
              "forced AVIO SEEK_SET is normalized");
        Check(vc::detail::SeekPacket(
                  &opaque, 2, SEEK_CUR | AVSEEK_FORCE) == 3,
              "forced AVIO SEEK_CUR is normalized");
        Check(vc::detail::SeekPacket(
                  &opaque, -1, SEEK_END | AVSEEK_FORCE) == 5,
              "forced AVIO SEEK_END is normalized");
        opaque.last_status = VC_OK;
        Check(vc::detail::SeekPacket(&opaque, 0, 0x7f) < 0,
              "invalid AVIO whence returns a negative error");
        Check(opaque.last_status == VC_ERR_INVALID_ARG,
              "invalid AVIO whence records VC_ERR_INVALID_ARG");

        const vc::detail::WinFileStats stats = file->stats();
        Check(stats.create_file_calls == 1u,
              "one WinFile owns exactly one CreateFileW call");
        Check(stats.read_calls == 2u,
              "AVIO read counter uses the WinFile");
        Check(stats.seek_calls == 4u,
              "AVIO seek counter uses the WinFile");
        Check(stats.size_queries == 2u,
              "AVIO size counter uses the WinFile snapshot");
        std::cout << "SESSION_IO_COUNTERS"
                  << " open=" << stats.create_file_calls
                  << " read=" << stats.read_calls
                  << " seek=" << stats.seek_calls
                  << " size=" << stats.size_queries << '\n';
    }
    DeleteFileW(path.c_str());
}

void TestWinFileAllocationFailureClosesCreatedHandle() {
    const std::wstring path =
        MakeTemporaryPath(L"-winfile-allocation.bin");
    Check(WriteBytes(path, {'a', 'b', 'c'}),
          "WinFile allocation fixture write");
    DWORD handles_before = 0u;
    Check(GetProcessHandleCount(
              GetCurrentProcess(), &handles_before) != FALSE,
          "read handle count before WinFile allocation failure");

    vc::detail::WinFileTestFailNextAllocation();
    vc_media_open_options options =
        FreshOptions(VC_MEDIA_TYPE_VIDEO);
    vc_media_session* session =
        reinterpret_cast<vc_media_session*>(0x1357);
    vc_error error = FreshError();
    Check(Open(path, options, nullptr, &session, &error) ==
              VC_ERR_OOM,
          "WinFile allocation failure maps to VC_ERR_OOM");
    Check(error.code == VC_ERR_OOM,
          "WinFile allocation failure populates OOM error");
    Check(session == reinterpret_cast<vc_media_session*>(0x1357),
          "WinFile allocation failure leaves output unchanged");

    DWORD handles_after = 0u;
    Check(GetProcessHandleCount(
              GetCurrentProcess(), &handles_after) != FALSE,
          "read handle count after WinFile allocation failure");
    Check(handles_after == handles_before,
          "WinFile allocation failure closes CreateFileW handle");

    session = nullptr;
    error = FreshError();
    Check(Open(path, options, nullptr, &session, &error) == VC_OK,
          "WinFile allocation fault is deterministic and one-shot");
    vc_media_close(session);
    std::cout << "WINFILE_OOM"
              << " before_handles=" << handles_before
              << " after_handles=" << handles_after
              << " output_unchanged=1\n";
    DeleteFileW(path.c_str());
}

void TestImageCacheBoundAndVideoAutoNoWholeFileCache() {
    const std::wstring path = MakeTemporaryPath(L"-bounded-cache.bin");
    std::vector<uint8_t> bytes(4096u);
    for (size_t index = 0; index < bytes.size(); ++index) {
        bytes[index] = static_cast<uint8_t>(index & 0xffu);
    }
    Check(WriteBytes(path, bytes), "bounded cache fixture write");

    struct CacheCase {
        uint32_t media_type;
        uint64_t image_limit;
        uint64_t expected_cache_size;
        const char* name;
    };
    const CacheCase cases[] = {
        {VC_MEDIA_TYPE_IMAGE, 257u, 257u,
         "image cache is capped at image_max_bytes"},
        {VC_MEDIA_TYPE_VIDEO, 4096u, 0u,
         "video mode never caches whole media"},
        {VC_MEDIA_TYPE_AUTO, 4096u, 0u,
         "auto mode never caches whole media"},
    };
    for (const CacheCase& test_case : cases) {
        vc_media_open_options options =
            FreshOptions(test_case.media_type);
        options.image_max_bytes = test_case.image_limit;
        vc_media_session* session = nullptr;
        vc_error error = FreshError();
        const int32_t open_status =
            Open(path, options, nullptr, &session, &error);
        Check(open_status == VC_OK, test_case.name);
        if (open_status != VC_OK) {
            continue;
        }
        std::array<uint8_t, VC_SHA512_SIZE> digest{};
        error = FreshError();
        Check(vc_media_hash(session, digest.data(), &error) == VC_OK,
              test_case.name);
        vc::detail::MediaSessionTestSnapshot snapshot{};
        Check(vc::detail::GetMediaSessionTestSnapshot(
                  session, &snapshot),
              test_case.name);
        Check(snapshot.image_cache_size ==
                  test_case.expected_cache_size,
              test_case.name);
        Check(snapshot.hash_runs == 1u,
              "hash computation runs once");
        Check(snapshot.has_custom_avio,
              "session owns a custom AVIOContext for the WinFile handle");
        std::cout << "MEDIA_CACHE"
                  << " type=" << test_case.media_type
                  << " limit=" << test_case.image_limit
                  << " cached=" << snapshot.image_cache_size
                  << " hash_runs=" << snapshot.hash_runs
                  << " custom_avio="
                  << (snapshot.has_custom_avio ? 1 : 0) << '\n';
        vc_media_close(session);
    }
    DeleteFileW(path.c_str());
}

void TestImageCacheExceptionsAreCachedConsistently() {
    const std::wstring path =
        MakeTemporaryPath(L"-image-cache-exception.bin");
    Check(WriteBytes(path, std::vector<uint8_t>(4096u, 0x42u)),
          "image cache exception fixture write");

    struct FailureCase {
        vc::detail::MediaSessionTestHashFailure failure;
        int32_t expected_status;
        const char* name;
    };
    const FailureCase cases[] = {
        {
            vc::detail::MediaSessionTestHashFailure::bad_alloc,
            VC_ERR_OOM,
            "image cache bad_alloc is cached as OOM",
        },
        {
            vc::detail::MediaSessionTestHashFailure::unexpected,
            VC_ERR_INTERNAL,
            "image cache unexpected exception is cached as internal",
        },
    };

    for (const FailureCase& test_case : cases) {
        vc_media_open_options options =
            FreshOptions(VC_MEDIA_TYPE_IMAGE);
        options.image_max_bytes = 4096u;
        vc_media_session* session = nullptr;
        vc_error error = FreshError();
        Check(Open(path, options, nullptr, &session, &error) == VC_OK,
              test_case.name);
        if (session == nullptr) {
            continue;
        }
        Check(vc::detail::SetMediaSessionTestHashFailure(
                  session, test_case.failure),
              test_case.name);

        std::array<uint8_t, VC_SHA512_SIZE> first{};
        first.fill(0x81u);
        const auto first_snapshot = first;
        error = FreshError();
        Check(vc_media_hash(session, first.data(), &error) ==
                  test_case.expected_status,
              test_case.name);
        Check(error.code == test_case.expected_status,
              test_case.name);
        Check(first == first_snapshot,
              "first exceptional hash leaves output unchanged");

        std::array<uint8_t, VC_SHA512_SIZE> second{};
        second.fill(0x92u);
        const auto second_snapshot = second;
        error = FreshError();
        Check(vc_media_hash(session, second.data(), &error) ==
                  test_case.expected_status,
              "repeated exceptional hash returns cached status");
        Check(error.code == test_case.expected_status,
              "repeated exceptional hash populates cached status");
        Check(second == second_snapshot,
              "repeated exceptional hash leaves output unchanged");

        vc::detail::MediaSessionTestSnapshot snapshot{};
        Check(vc::detail::GetMediaSessionTestSnapshot(
                  session, &snapshot),
              test_case.name);
        Check(snapshot.hash_runs == 1u,
              "exceptional hash streaming runs at most once");
        std::cout << "HASH_EXCEPTION_CACHE"
                  << " injected="
                  << static_cast<uint32_t>(test_case.failure)
                  << " status=" << test_case.expected_status
                  << " hash_runs=" << snapshot.hash_runs
                  << " outputs_unchanged=1\n";
        vc_media_close(session);
    }
    DeleteFileW(path.c_str());
}

struct BlockingHook {
    std::atomic<bool> entered{false};
    std::atomic<bool> release{false};

    static void Run(vc::detail::IoBoundary boundary,
                    void* context) noexcept {
        if (boundary != vc::detail::IoBoundary::before_read) {
            return;
        }
        auto* self = static_cast<BlockingHook*>(context);
        self->entered.store(true, std::memory_order_release);
        while (!self->release.load(std::memory_order_acquire)) {
            std::this_thread::yield();
        }
    }
};

struct ProtocolBarrier {
    vc::detail::MediaSessionTestProtocolEvent target =
        vc::detail::MediaSessionTestProtocolEvent::lookup_after_first_check;
    bool block = false;
    std::atomic<bool> entered{false};
    std::atomic<bool> release{false};

    static void Run(
        vc::detail::MediaSessionTestProtocolEvent event,
        size_t,
        uint32_t,
        void* context) noexcept {
        auto* self = static_cast<ProtocolBarrier*>(context);
        if (event != self->target) {
            return;
        }
        self->entered.store(true, std::memory_order_release);
        while (self->block &&
               !self->release.load(std::memory_order_acquire)) {
            std::this_thread::yield();
        }
    }
};

bool WaitFor(std::atomic<bool>& value) {
    for (size_t spin = 0; spin < 1000000u; ++spin) {
        if (value.load(std::memory_order_acquire)) {
            return true;
        }
        std::this_thread::yield();
    }
    return value.load(std::memory_order_acquire);
}

void TestClaimFailureRollsBackSlotAndPublication() {
    const std::wstring path =
        MakeTemporaryPath(L"-claim-rollback.bin");
    Check(WriteBytes(path, {'a', 'b', 'c'}),
          "claim rollback fixture write");
    vc_media_open_options options =
        FreshOptions(VC_MEDIA_TYPE_VIDEO);

    constexpr size_t target_slot = 29u;
    Check(vc::detail::MediaSessionTestSeedFreeSlot(
              target_slot, 100u),
          "seed claim rollback generation");
    vc::detail::MediaSessionTestSetNextSlot(target_slot);
    vc::detail::MediaSessionTestFailNextPostClaim();

    vc_media_session* output =
        reinterpret_cast<vc_media_session*>(0x2468);
    vc_error error = FreshError();
    Check(Open(path, options, nullptr, &output, &error) ==
              VC_ERR_OOM,
          "post-claim failure maps to OOM");
    Check(error.code == VC_ERR_OOM,
          "post-claim failure populates OOM");
    Check(output == reinterpret_cast<vc_media_session*>(0x2468),
          "post-claim failure leaves public output unchanged");
    Check(vc::detail::MediaSessionTestSlotStateOf(target_slot) ==
              vc::detail::MediaSessionTestSlotState::free,
          "post-claim failure returns slot to free");
    Check(vc::detail::MediaSessionTestSlotGeneration(target_slot) ==
              101u,
          "post-claim rollback preserves claimed generation");
    Check(!vc::detail::MediaSessionTestSlotHasMedia(target_slot),
          "post-claim rollback leaves no media");

    vc::detail::MediaSessionTestSetNextSlot(target_slot);
    vc_media_session* recovered = nullptr;
    error = FreshError();
    Check(Open(path,
               options,
               nullptr,
               &recovered,
               &error) == VC_OK,
          "post-claim slot is reusable");
    Check(vc::detail::MediaSessionTestHandleSlot(recovered) ==
              target_slot,
          "post-claim recovery reuses exact slot");
    Check(vc::detail::MediaSessionTestHandleGeneration(recovered) ==
              102u,
          "post-claim recovery advances generation");
    vc_media_close(recovered);

    std::cout << "SESSION_CLAIM_ROLLBACK"
              << " slot=" << target_slot
              << " failed_generation=101"
              << " recovered_generation=102"
              << " output_unchanged=1 media_residue=0\n";
    DeleteFileW(path.c_str());
}

void TestLookupRejectsDisposedBetweenControlChecks() {
    const std::wstring path =
        MakeTemporaryPath(L"-lookup-disposed.bin");
    Check(WriteBytes(path, {'a', 'b', 'c'}),
          "lookup disposed fixture write");
    vc_media_open_options options =
        FreshOptions(VC_MEDIA_TYPE_VIDEO);
    constexpr size_t target_slot = 31u;
    vc::detail::MediaSessionTestSetNextSlot(target_slot);
    vc_media_session* session = nullptr;
    vc_error error = FreshError();
    Check(Open(path, options, nullptr, &session, &error) == VC_OK,
          "lookup disposed session open");

    ProtocolBarrier lookup_barrier;
    lookup_barrier.target =
        vc::detail::MediaSessionTestProtocolEvent::
            lookup_after_first_check;
    ProtocolBarrier close_barrier;
    close_barrier.target =
        vc::detail::MediaSessionTestProtocolEvent::
            close_after_dispose;
    close_barrier.block = true;
    vc::detail::MediaSessionTestSetProtocolHooks(
        &ProtocolBarrier::Run,
        &lookup_barrier,
        &ProtocolBarrier::Run,
        &close_barrier);
    Check(vc::detail::MediaSessionTestLockSlot(target_slot),
          "hold lookup slot mutex");

    std::array<uint8_t, VC_SHA512_SIZE> digest{};
    digest.fill(0x55u);
    const auto digest_snapshot = digest;
    int32_t hash_status = VC_ERR_INTERNAL;
    std::thread hasher([&]() {
        vc_error worker_error = FreshError();
        hash_status =
            vc_media_hash(session, digest.data(), &worker_error);
    });
    Check(WaitFor(lookup_barrier.entered),
          "lookup passed first active control check");

    std::thread closer([&]() { vc_media_close(session); });
    Check(WaitFor(close_barrier.entered),
          "close atomically published disposed before lookup recheck");
    vc::detail::MediaSessionTestUnlockSlot(target_slot);
    hasher.join();
    close_barrier.release.store(true, std::memory_order_release);
    closer.join();
    vc::detail::MediaSessionTestSetProtocolHooks(
        nullptr, nullptr, nullptr, nullptr);

    Check(hash_status == VC_ERR_UNSUPPORTED,
          "lookup rejects disposed generation at second check");
    Check(digest == digest_snapshot,
          "disposed-between-checks hash leaves output unchanged");
    Check(vc::detail::MediaSessionTestSlotIsFree(target_slot),
          "disposed-between-checks close completes");
    std::cout << "SESSION_LOOKUP_RECHECK"
              << " slot=" << target_slot
              << " first_check=active mutex_wait=1"
              << " second_check=disposed rejected=1\n";
    DeleteFileW(path.c_str());
}

void TestLookupRejectsInitializingUntilPublication() {
    const std::wstring path =
        MakeTemporaryPath(L"-initializing.bin");
    Check(WriteBytes(path, {'a', 'b', 'c'}),
          "initializing fixture write");
    vc_media_open_options options =
        FreshOptions(VC_MEDIA_TYPE_VIDEO);
    constexpr size_t target_slot = 33u;
    vc::detail::MediaSessionTestSetNextSlot(target_slot);

    ProtocolBarrier publication;
    publication.target =
        vc::detail::MediaSessionTestProtocolEvent::
            publish_before_active;
    publication.block = true;
    vc::detail::MediaSessionTestSetProtocolHooks(
        &ProtocolBarrier::Run,
        &publication,
        nullptr,
        nullptr);
    vc_media_session* published =
        reinterpret_cast<vc_media_session*>(0x3579);
    int32_t open_status = VC_ERR_INTERNAL;
    std::thread opener([&]() {
        vc_error worker_error = FreshError();
        open_status =
            Open(path,
                 options,
                 nullptr,
                 &published,
                 &worker_error);
    });
    Check(WaitFor(publication.entered),
          "publication stops before active store");
    Check(vc::detail::MediaSessionTestSlotStateOf(target_slot) ==
              vc::detail::MediaSessionTestSlotState::initializing,
          "claimed slot remains initializing before publication");
    Check(published == reinterpret_cast<vc_media_session*>(0x3579),
          "initializing session is not partially published");

    const uint32_t generation =
        vc::detail::MediaSessionTestSlotGeneration(target_slot);
    vc_media_session* initializing =
        vc::detail::MediaSessionTestEncodeHandle(
            target_slot, generation);
    std::array<uint8_t, VC_SHA512_SIZE> before{};
    before.fill(0x64u);
    const auto before_snapshot = before;
    vc_error error = FreshError();
    Check(vc_media_hash(initializing, before.data(), &error) ==
              VC_ERR_UNSUPPORTED,
          "lookup rejects initializing slot");
    Check(before == before_snapshot,
          "initializing lookup leaves output unchanged");

    publication.release.store(true, std::memory_order_release);
    opener.join();
    vc::detail::MediaSessionTestSetProtocolHooks(
        nullptr, nullptr, nullptr, nullptr);
    Check(open_status == VC_OK,
          "publication succeeds after active store");
    Check(published == initializing,
          "published handle matches claimed slot and generation");
    std::array<uint8_t, VC_SHA512_SIZE> after{};
    error = FreshError();
    Check(vc_media_hash(published, after.data(), &error) == VC_OK,
          "lookup succeeds after active publication");
    Check(after == ParseDigest(
                       "ddaf35a193617abacc417349ae20413112e6fa4e89"
                       "a97ea20a9eeee64b55d39a2192992a274fc1a836ba"
                       "3c23a3feebbd454d4423643ce80e2a9ac94fa54ca4"
                       "9f"),
          "published session hashes its file");
    vc_media_close(published);
    std::cout << "SESSION_PUBLICATION"
              << " slot=" << target_slot
              << " initializing_rejected=1 active_succeeded=1\n";
    DeleteFileW(path.c_str());
}

void TestExactSessionCapacityAndRecovery() {
    const std::wstring path =
        MakeTemporaryPath(L"-capacity.bin");
    Check(WriteBytes(path, {}),
          "capacity fixture write");
    vc_media_open_options options =
        FreshOptions(VC_MEDIA_TYPE_VIDEO);
    const size_t capacity =
        vc::detail::MediaSessionTestSlotCapacity();
    Check(capacity == 4096u,
          "media session capacity is exactly 4096");

    auto fill_capacity =
        [&](std::vector<vc_media_session*>& handles) {
            handles.assign(capacity, nullptr);
            for (size_t index = 0; index < capacity; ++index) {
                vc_error error = FreshError();
                if (Open(path,
                         options,
                         nullptr,
                         &handles[index],
                         &error) != VC_OK) {
                    Check(false,
                          "all 4096 media session slots open");
                    return false;
                }
            }
            return true;
        };
    auto close_all =
        [](std::vector<vc_media_session*>& handles) {
            for (vc_media_session*& handle : handles) {
                vc_media_close(handle);
                handle = nullptr;
            }
        };

    std::vector<vc_media_session*> handles;
    if (!fill_capacity(handles)) {
        close_all(handles);
        DeleteFileW(path.c_str());
        return;
    }
    vc_media_session* overflow =
        reinterpret_cast<vc_media_session*>(0x468a);
    vc_error error = FreshError();
    Check(Open(path, options, nullptr, &overflow, &error) ==
              VC_ERR_OOM,
          "4097th media session returns OOM");
    Check(error.code == VC_ERR_OOM,
          "capacity failure populates OOM");
    Check(overflow == reinterpret_cast<vc_media_session*>(0x468a),
          "capacity failure leaves output unchanged");

    constexpr size_t release_index = 173u;
    vc_media_session* stale = handles[release_index];
    const size_t released_slot =
        vc::detail::MediaSessionTestHandleSlot(stale);
    const uint32_t released_generation =
        vc::detail::MediaSessionTestHandleGeneration(stale);
    vc_media_close(stale);
    handles[release_index] = nullptr;
    vc::detail::MediaSessionTestSetNextSlot(released_slot);
    vc_media_session* replacement = nullptr;
    error = FreshError();
    Check(Open(path,
               options,
               nullptr,
               &replacement,
               &error) == VC_OK,
          "capacity recovers after one release");
    Check(vc::detail::MediaSessionTestHandleSlot(replacement) ==
              released_slot,
          "capacity recovery reuses released slot");
    Check(vc::detail::MediaSessionTestHandleGeneration(replacement) ==
              released_generation + 1u,
          "capacity recovery advances generation");
    vc_media_close(stale);
    close_all(handles);
    vc_media_close(replacement);

    std::vector<vc_media_session*> recovered;
    const bool fully_recovered = fill_capacity(recovered);
    Check(fully_recovered,
          "all 4096 slots recover after cleanup");
    if (fully_recovered) {
        overflow = reinterpret_cast<vc_media_session*>(0x579b);
        error = FreshError();
        Check(Open(path, options, nullptr, &overflow, &error) ==
                  VC_ERR_OOM,
              "recovered capacity still has exact 4096 limit");
        Check(overflow ==
                  reinterpret_cast<vc_media_session*>(0x579b),
              "recovered overflow leaves output unchanged");
    }
    close_all(recovered);
    std::cout << "SESSION_CAPACITY"
              << " active=4096 overflow_status=" << VC_ERR_OOM
              << " same_slot_recovery=1 full_recovery=1\n";
    DeleteFileW(path.c_str());
}

void TestMaximumGenerationRetiresWithoutWrap() {
    const std::wstring path =
        MakeTemporaryPath(L"-max-generation.bin");
    Check(WriteBytes(path, {'a', 'b', 'c'}),
          "max generation fixture write");
    vc_media_open_options options =
        FreshOptions(VC_MEDIA_TYPE_VIDEO);
    constexpr size_t target_slot = 73u;
    const uint32_t maximum =
        vc::detail::MediaSessionTestMaximumGeneration();
    Check(maximum == 0x7fffffffu,
          "media session maximum generation fixture");
    Check(vc::detail::MediaSessionTestSeedFreeSlot(
              target_slot, maximum - 1u),
          "seed max-1 free media slot");
    vc::detail::MediaSessionTestSetNextSlot(target_slot);

    vc_media_session* maximum_handle = nullptr;
    vc_error error = FreshError();
    Check(Open(path,
               options,
               nullptr,
               &maximum_handle,
               &error) == VC_OK,
          "maximum generation session opens");
    Check(vc::detail::MediaSessionTestHandleSlot(maximum_handle) ==
              target_slot,
          "maximum generation uses seeded slot");
    Check(vc::detail::MediaSessionTestHandleGeneration(
              maximum_handle) == maximum,
          "max-1 advances exactly to max");
    vc_media_close(maximum_handle);
    Check(vc::detail::MediaSessionTestSlotStateOf(target_slot) ==
              vc::detail::MediaSessionTestSlotState::retired,
          "maximum generation free slot is retired");

    vc::detail::MediaSessionTestSetNextSlot(target_slot);
    vc_media_session* fresh = nullptr;
    error = FreshError();
    Check(Open(path, options, nullptr, &fresh, &error) == VC_OK,
          "open skips retired maximum-generation slot");
    Check(vc::detail::MediaSessionTestHandleSlot(fresh) !=
              target_slot,
          "retired slot never wraps or aliases");

    std::array<uint8_t, VC_SHA512_SIZE> stale_output{};
    stale_output.fill(0x73u);
    const auto stale_snapshot = stale_output;
    error = FreshError();
    Check(vc_media_hash(maximum_handle,
                        stale_output.data(),
                        &error) == VC_ERR_UNSUPPORTED,
          "stale maximum-generation handle cannot hash");
    Check(stale_output == stale_snapshot,
          "stale maximum-generation hash leaves output unchanged");
    vc_media_close(maximum_handle);

    std::array<uint8_t, VC_SHA512_SIZE> fresh_digest{};
    error = FreshError();
    Check(vc_media_hash(fresh, fresh_digest.data(), &error) == VC_OK,
          "stale maximum-generation close cannot affect fresh session");
    Check(fresh_digest == ParseDigest(
                              "ddaf35a193617abacc417349ae20413112e6fa4e89"
                              "a97ea20a9eeee64b55d39a2192992a274fc1a836ba"
                              "3c23a3feebbd454d4423643ce80e2a9ac94fa54ca4"
                              "9f"),
          "fresh session remains correctly bound");
    vc_media_close(fresh);
    std::cout << "SESSION_MAX_GENERATION"
              << " slot=" << target_slot
              << " maximum=" << maximum
              << " retired=1 stale_rejected=1 no_wrap=1\n";
    DeleteFileW(path.c_str());
}

void TestSessionHandleGenerationPreventsAba() {
    const std::wstring old_path =
        MakeTemporaryPath(L"-aba-old.bin");
    const std::wstring new_path =
        MakeTemporaryPath(L"-aba-new.bin");
    Check(WriteBytes(old_path, {'a', 'b', 'c'}),
          "ABA old fixture write");
    Check(WriteBytes(new_path, {}),
          "ABA new fixture write");
    vc_media_open_options options =
        FreshOptions(VC_MEDIA_TYPE_VIDEO);

    constexpr size_t target_slot = 37u;
    vc::detail::MediaSessionTestSetNextSlot(target_slot);
    vc_media_session* stale = nullptr;
    vc_error error = FreshError();
    Check(Open(old_path, options, nullptr, &stale, &error) == VC_OK,
          "ABA old session open");
    Check(vc::detail::MediaSessionTestHandleSlot(stale) ==
              target_slot,
          "ABA old session uses forced slot");
    const uint32_t stale_generation =
        vc::detail::MediaSessionTestHandleGeneration(stale);
    vc_media_close(stale);
    Check(vc::detail::MediaSessionTestSlotIsFree(target_slot),
          "ABA close retires forced slot");

    vc::detail::MediaSessionTestSetNextSlot(target_slot);
    vc_media_session* fresh = nullptr;
    error = FreshError();
    Check(Open(new_path, options, nullptr, &fresh, &error) == VC_OK,
          "ABA replacement session open");
    Check(vc::detail::MediaSessionTestHandleSlot(fresh) ==
              target_slot,
          "ABA replacement deterministically reuses slot");
    const uint32_t fresh_generation =
        vc::detail::MediaSessionTestHandleGeneration(fresh);
    Check(fresh_generation != stale_generation,
          "same-slot replacement advances generation");
    Check(fresh != stale,
          "encoded handle identity includes generation");

    std::array<uint8_t, VC_SHA512_SIZE> stale_output{};
    stale_output.fill(0x39u);
    const auto stale_snapshot = stale_output;
    error = FreshError();
    Check(vc_media_hash(stale, stale_output.data(), &error) ==
              VC_ERR_UNSUPPORTED,
          "stale same-slot handle cannot hash replacement");
    Check(stale_output == stale_snapshot,
          "stale ABA hash leaves output unchanged");
    vc_media_close(stale);
    vc_media_close(stale);

    std::array<uint8_t, VC_SHA512_SIZE> fresh_digest{};
    error = FreshError();
    Check(vc_media_hash(fresh, fresh_digest.data(), &error) == VC_OK,
          "stale repeated close cannot retire replacement");
    Check(fresh_digest == ParseDigest(
                              "cf83e1357eefb8bdf1542850d66d8007d620e405"
                              "0b5715dc83f4a921d36ce9ce47d0d13c5d85f2b"
                              "0ff8318d2877eec2f63b931bd47417a81a538327a"
                              "f927da3e"),
          "replacement session hashes replacement file");
    vc_media_close(fresh);
    Check(vc::detail::MediaSessionTestSlotIsFree(target_slot),
          "replacement close frees forced slot");

    std::cout << "SESSION_ABA"
              << " slot=" << target_slot
              << " stale_generation=" << stale_generation
              << " fresh_generation=" << fresh_generation
              << " stale_rejected=1 repeated_close_safe=1\n";
    DeleteFileW(old_path.c_str());
    DeleteFileW(new_path.c_str());
}

void TestConcurrentHashCloseAndSameSlotReuse() {
    const std::wstring old_path =
        MakeTemporaryPath(L"-close-race-old.bin");
    const std::wstring new_path =
        MakeTemporaryPath(L"-close-race-new.bin");
    Check(WriteBytes(old_path, {'a', 'b', 'c'}),
          "close race old fixture write");
    Check(WriteBytes(new_path, {}),
          "close race new fixture write");
    vc_media_open_options options =
        FreshOptions(VC_MEDIA_TYPE_VIDEO);

    constexpr size_t target_slot = 41u;
    vc::detail::MediaSessionTestSetNextSlot(target_slot);
    vc_media_session* old_session = nullptr;
    vc_error error = FreshError();
    Check(Open(old_path,
               options,
               nullptr,
               &old_session,
               &error) == VC_OK,
          "close race old session open");
    BlockingHook hook;
    Check(vc::detail::SetMediaSessionTestIoHook(
              old_session, &BlockingHook::Run, &hook),
          "close race hook install");

    std::array<uint8_t, VC_SHA512_SIZE> old_digest{};
    int32_t old_status = VC_ERR_INTERNAL;
    std::thread hasher([&]() {
        vc_error worker_error = FreshError();
        old_status =
            vc_media_hash(old_session,
                          old_digest.data(),
                          &worker_error);
    });
    for (size_t spin = 0;
         spin < 1000000u &&
         !hook.entered.load(std::memory_order_acquire);
         ++spin) {
        std::this_thread::yield();
    }
    Check(hook.entered.load(std::memory_order_acquire),
          "close race hash owns a retained session");

    vc_media_close(old_session);
    Check(vc::detail::MediaSessionTestSlotIsFree(target_slot),
          "close retires slot while in-flight hash retains object");

    vc::detail::MediaSessionTestSetNextSlot(target_slot);
    vc_media_session* replacement = nullptr;
    error = FreshError();
    Check(Open(new_path,
               options,
               nullptr,
               &replacement,
               &error) == VC_OK,
          "close race replacement opens during old hash");
    Check(vc::detail::MediaSessionTestHandleSlot(replacement) ==
              target_slot,
          "close race replacement reuses exact slot");
    Check(vc::detail::MediaSessionTestHandleGeneration(replacement) !=
              vc::detail::MediaSessionTestHandleGeneration(old_session),
          "close race replacement has a new generation");

    std::array<uint8_t, VC_SHA512_SIZE> stale_output{};
    stale_output.fill(0x71u);
    const auto stale_snapshot = stale_output;
    error = FreshError();
    Check(vc_media_hash(
              old_session, stale_output.data(), &error) ==
              VC_ERR_UNSUPPORTED,
          "closed handle cannot enter replacement during old hash");
    Check(stale_output == stale_snapshot,
          "concurrent stale hash leaves output unchanged");
    vc_media_close(old_session);

    hook.release.store(true, std::memory_order_release);
    hasher.join();
    Check(old_status == VC_OK,
          "in-flight hash safely completes after close");
    Check(old_digest == ParseDigest(
                            "ddaf35a193617abacc417349ae20413112e6fa4e89"
                            "a97ea20a9eeee64b55d39a2192992a274fc1a836ba"
                            "3c23a3feebbd454d4423643ce80e2a9ac94fa54ca4"
                            "9f"),
          "in-flight hash stays bound to old file");

    std::array<uint8_t, VC_SHA512_SIZE> replacement_digest{};
    error = FreshError();
    Check(vc_media_hash(replacement,
                        replacement_digest.data(),
                        &error) == VC_OK,
          "replacement survives stale close");
    Check(replacement_digest == ParseDigest(
                                    "cf83e1357eefb8bdf1542850d66d8007d620e405"
                                    "0b5715dc83f4a921d36ce9ce47d0d13c5d85f2b"
                                    "0ff8318d2877eec2f63b931bd47417a81a538327a"
                                    "f927da3e"),
          "replacement hash stays bound to new file");
    vc_media_close(replacement);

    std::cout << "SESSION_CLOSE_RACE"
              << " slot=" << target_slot
              << " old_hash=ok replacement_hash=ok"
              << " stale_rejected=1\n";
    DeleteFileW(old_path.c_str());
    DeleteFileW(new_path.c_str());
}

void TestSessionRejectsConcurrentOperation() {
    const std::wstring path = MakeTemporaryPath(L"-operation-guard.bin");
    Check(WriteBytes(path, std::vector<uint8_t>(256u * 1024u, 0x5au)),
          "operation guard fixture write");
    vc_media_open_options options =
        FreshOptions(VC_MEDIA_TYPE_VIDEO);
    vc_media_session* session = nullptr;
    vc_error error = FreshError();
    const int32_t open_status =
        Open(path, options, nullptr, &session, &error);
    Check(open_status == VC_OK, "operation guard session open");
    if (open_status == VC_OK) {
        BlockingHook hook;
        Check(vc::detail::SetMediaSessionTestIoHook(
                  session, &BlockingHook::Run, &hook),
              "install operation guard hook");
        std::array<uint8_t, VC_SHA512_SIZE> first{};
        int32_t first_status = VC_ERR_INTERNAL;
        std::thread worker([&]() {
            vc_error worker_error = FreshError();
            first_status =
                vc_media_hash(session, first.data(), &worker_error);
        });
        for (size_t spin = 0;
             spin < 1000000u &&
             !hook.entered.load(std::memory_order_acquire);
             ++spin) {
            std::this_thread::yield();
        }
        Check(hook.entered.load(std::memory_order_acquire),
              "first operation reaches deterministic read boundary");
        std::array<uint8_t, VC_SHA512_SIZE> second{};
        second.fill(0x6bu);
        const auto snapshot = second;
        error = FreshError();
        Check(vc_media_hash(session, second.data(), &error) ==
                  VC_ERR_INVALID_ARG,
              "concurrent operation is explicitly rejected");
        Check(second == snapshot,
              "rejected concurrent operation leaves output unchanged");
        hook.release.store(true, std::memory_order_release);
        worker.join();
        Check(first_status == VC_OK,
              "original operation completes after exclusion test");
        vc_media_close(session);
    }
    DeleteFileW(path.c_str());
}

struct InterruptAfterReadHook {
    vc_cancel_token* token = nullptr;
    uint32_t delay_ms = 0u;
    bool request_cancel = false;
    std::atomic<bool> fired{false};

    static void Run(vc::detail::IoBoundary boundary,
                    void* context) noexcept {
        if (boundary != vc::detail::IoBoundary::after_read) {
            return;
        }
        auto* self =
            static_cast<InterruptAfterReadHook*>(context);
        if (self->fired.exchange(true, std::memory_order_acq_rel)) {
            return;
        }
        if (self->delay_ms != 0u) {
            Sleep(self->delay_ms);
        }
        if (self->request_cancel) {
            vc_cancel_request(self->token);
        }
    }
};

void TestPostBlockingInterruptChecksAndCancellationPriority() {
    const std::wstring path = MakeTemporaryPath(L"-interrupt.bin");
    Check(WriteBytes(path, std::vector<uint8_t>(128u * 1024u, 0x31u)),
          "interrupt fixture write");

    struct InterruptCase {
        bool cancel;
        uint32_t timeout_ms;
        uint32_t delay_ms;
        int32_t expected_status;
        const char* name;
    };
    const InterruptCase cases[] = {
        {true, 0u, 0u, VC_ERR_CANCELLED,
         "post-read cancellation is observed"},
        {false, 5u, 20u, VC_ERR_TIMEOUT,
         "post-read timeout is observed"},
        {true, 5u, 20u, VC_ERR_CANCELLED,
         "cancellation wins when timeout also expires"},
    };

    for (const InterruptCase& test_case : cases) {
        vc_cancel_token* token = nullptr;
        vc_error error = FreshError();
        Check(vc_cancel_create(&token, &error) == VC_OK,
              test_case.name);
        vc_media_open_options options =
            FreshOptions(VC_MEDIA_TYPE_VIDEO);
        options.operation_timeout_ms = test_case.timeout_ms;
        vc_media_session* session = nullptr;
        error = FreshError();
        const int32_t open_status =
            Open(path, options, token, &session, &error);
        Check(open_status == VC_OK, test_case.name);
        if (open_status == VC_OK) {
            InterruptAfterReadHook hook;
            hook.token = token;
            hook.delay_ms = test_case.delay_ms;
            hook.request_cancel = test_case.cancel;
            Check(vc::detail::SetMediaSessionTestIoHook(
                      session, &InterruptAfterReadHook::Run, &hook),
                  test_case.name);
            std::array<uint8_t, VC_SHA512_SIZE> digest{};
            digest.fill(0x7cu);
            const auto snapshot = digest;
            error = FreshError();
            Check(vc_media_hash(session, digest.data(), &error) ==
                      test_case.expected_status,
                  test_case.name);
            Check(error.code == test_case.expected_status,
                  test_case.name);
            Check(digest == snapshot,
                  "interrupted hash leaves output unchanged");
            std::array<uint8_t, VC_SHA512_SIZE> repeated{};
            repeated.fill(0x4du);
            const auto repeated_snapshot = repeated;
            error = FreshError();
            Check(vc_media_hash(session, repeated.data(), &error) ==
                      test_case.expected_status,
                  "failed hash attempt is not streamed twice");
            Check(repeated == repeated_snapshot,
                  "cached failure leaves repeated output unchanged");
            vc::detail::MediaSessionTestSnapshot state{};
            Check(vc::detail::GetMediaSessionTestSnapshot(
                      session, &state),
                  test_case.name);
            Check(state.hash_runs == 1u,
                  "hash streaming runs at most once after failure");
            std::cout << "INTERRUPT_BOUNDARY"
                      << " expected=" << test_case.expected_status
                      << " actual=" << error.code
                      << " hash_runs=" << state.hash_runs << '\n';
            vc_media_close(session);
        }
        vc_cancel_free(token);
    }
    DeleteFileW(path.c_str());
}

void TestSessionKeepsOnePathIndependentHandle() {
    const std::wstring original = MakeTemporaryPath(L"-original.bin");
    const std::wstring renamed = MakeTemporaryPath(L"-renamed.bin");
    Check(WriteBytes(original, {'a', 'b', 'c'}),
          "one-handle fixture write");

    vc_media_open_options options =
        FreshOptions(VC_MEDIA_TYPE_VIDEO);
    vc_media_session* session = nullptr;
    vc_error error = FreshError();
    const int32_t open_status =
        Open(original, options, nullptr, &session, &error);
    Check(open_status == VC_OK, "one-handle session open");
    if (open_status == VC_OK) {
        Check(MoveFileExW(original.c_str(),
                          renamed.c_str(),
                          MOVEFILE_REPLACE_EXISTING) != FALSE,
              "open file must permit rename");
        std::array<uint8_t, VC_SHA512_SIZE> actual{};
        error = FreshError();
        Check(vc_media_hash(session, actual.data(), &error) == VC_OK,
              "hash must use handle retained before rename");
        Check(actual == ParseDigest(
                            "ddaf35a193617abacc417349ae20413112e6fa4e89"
                            "a97ea20a9eeee64b55d39a2192992a274fc1a836ba"
                            "3c23a3feebbd454d4423643ce80e2a9ac94fa54ca4"
                            "9f"),
              "retained handle hashes original bytes");
        vc_media_close(session);
    }
    DeleteFileW(original.c_str());
    DeleteFileW(renamed.c_str());
}

void TestSuccessfulHashIsCached() {
    const std::wstring path = MakeTemporaryPath(L"-cache.bin");
    Check(WriteBytes(path, {'a', 'b', 'c'}), "cache fixture write");

    vc_media_open_options options =
        FreshOptions(VC_MEDIA_TYPE_IMAGE);
    vc_media_session* session = nullptr;
    vc_error error = FreshError();
    const int32_t open_status =
        Open(path, options, nullptr, &session, &error);
    Check(open_status == VC_OK, "cache session open");
    if (open_status == VC_OK) {
        std::array<uint8_t, VC_SHA512_SIZE> first{};
        error = FreshError();
        Check(vc_media_hash(session, first.data(), &error) == VC_OK,
              "first hash succeeds");
        Check(WriteBytes(path, {'c', 'h', 'a', 'n', 'g', 'e', 'd'}),
              "cache fixture mutation");
        std::array<uint8_t, VC_SHA512_SIZE> second{};
        error = FreshError();
        Check(vc_media_hash(session, second.data(), &error) == VC_OK,
              "cached hash succeeds");
        Check(second == first,
              "second hash returns the successful cached digest");
        vc::detail::MediaSessionTestSnapshot state{};
        Check(vc::detail::GetMediaSessionTestSnapshot(
                  session, &state),
              "cached hash state is observable");
        Check(state.hash_runs == 1u,
              "successful hash streams the file at most once");
        Check(state.io.seek_calls == 1u,
              "cached hash does not seek the file twice");
        std::cout << "HASH_CACHE"
                  << " runs=" << state.hash_runs
                  << " seeks=" << state.io.seek_calls
                  << " reads=" << state.io.read_calls
                  << " bytes=" << state.file_size << '\n';
        vc_media_close(session);
    }
    DeleteFileW(path.c_str());
}

void TestCancellationAndRetainedLifetime() {
    const std::wstring path = MakeTemporaryPath(L"-cancel.bin");
    Check(WriteBytes(path, {'a', 'b', 'c'}), "cancel fixture write");
    vc_media_open_options options =
        FreshOptions(VC_MEDIA_TYPE_AUTO);

    vc_cancel_token* cancelled = nullptr;
    vc_error error = FreshError();
    Check(vc_cancel_create(&cancelled, &error) == VC_OK,
          "cancel token creation");
    vc_cancel_request(cancelled);
    vc_media_session* untouched =
        reinterpret_cast<vc_media_session*>(0x1234);
    error = FreshError();
    Check(Open(path, options, cancelled, &untouched, &error) ==
              VC_ERR_CANCELLED,
          "pre-cancelled open must be cancelled");
    Check(untouched == reinterpret_cast<vc_media_session*>(0x1234),
          "cancelled open leaves output unchanged");
    vc_cancel_free(cancelled);

    vc_cancel_token* retained = nullptr;
    error = FreshError();
    Check(vc_cancel_create(&retained, &error) == VC_OK,
          "retained token creation");
    vc_media_session* session = nullptr;
    error = FreshError();
    const int32_t open_status =
        Open(path, options, retained, &session, &error);
    Check(open_status == VC_OK, "session with cancellation owner");
    vc_cancel_free(retained);
    if (open_status == VC_OK) {
        std::array<uint8_t, VC_SHA512_SIZE> digest{};
        error = FreshError();
        Check(vc_media_hash(session, digest.data(), &error) == VC_OK,
              "session retains cancellation lifetime");
        vc_media_close(session);
    }
    DeleteFileW(path.c_str());
}

void TestOpenValidationAndNoPartialPublish() {
    const std::wstring path = MakeTemporaryPath(L"-validation.bin");
    Check(WriteBytes(path, {'x'}), "validation fixture write");

    vc_media_open_options options =
        FreshOptions(VC_MEDIA_TYPE_VIDEO + 1u);
    vc_media_session* session =
        reinterpret_cast<vc_media_session*>(0x5678);
    vc_error error = FreshError();
    Check(Open(path, options, nullptr, &session, &error) ==
              VC_ERR_INVALID_ARG,
          "out-of-range expected media type is rejected");
    Check(error.code == VC_ERR_INVALID_ARG,
          "media type range error is populated");
    Check(session == reinterpret_cast<vc_media_session*>(0x5678),
          "invalid media type leaves output unchanged");

    options = FreshOptions(VC_MEDIA_TYPE_AUTO);
    options.reserved_flags = 1u;
    error = FreshError();
    Check(Open(path, options, nullptr, &session, &error) ==
              VC_ERR_INVALID_ARG,
          "reserved flags are rejected");
    Check(session == reinterpret_cast<vc_media_session*>(0x5678),
          "reserved failure leaves output unchanged");
    DeleteFileW(path.c_str());
}

}  // namespace

int main() {
    TestSha512StandardVectors();
    TestNativeSha512IncrementalUpdates();
    TestWinFileAndAvioShareReadSeekSizeState();
    TestWinFileAllocationFailureClosesCreatedHandle();
    TestImageCacheBoundAndVideoAutoNoWholeFileCache();
    TestImageCacheExceptionsAreCachedConsistently();
    TestClaimFailureRollsBackSlotAndPublication();
    TestLookupRejectsDisposedBetweenControlChecks();
    TestLookupRejectsInitializingUntilPublication();
    TestSessionHandleGenerationPreventsAba();
    TestConcurrentHashCloseAndSameSlotReuse();
    TestSessionRejectsConcurrentOperation();
    TestPostBlockingInterruptChecksAndCancellationPriority();
    TestSessionKeepsOnePathIndependentHandle();
    TestSuccessfulHashIsCached();
    TestCancellationAndRetainedLifetime();
    TestOpenValidationAndNoPartialPublish();
    TestExactSessionCapacityAndRecovery();
    TestMaximumGenerationRetiresWithoutWrap();
    if (failures != 0) {
        std::cerr << failures << " media session test(s) failed\n";
        return 1;
    }
    std::cout << "videocore session tests passed\n";
    return 0;
}
