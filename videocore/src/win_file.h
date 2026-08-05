#ifndef VIDEOCORE_SRC_WIN_FILE_H
#define VIDEOCORE_SRC_WIN_FILE_H

#define WIN32_LEAN_AND_MEAN
#include <windows.h>

#include <cstdint>
#include <memory>
#include <string>

#include "deadline.h"
#include "videocore/videocore.h"

namespace vc::detail {

struct CancelState;

struct FileIdentity {
    uint64_t volume_serial = 0u;
    uint64_t file_id_high = 0u;
    uint64_t file_id_low = 0u;
};

struct WinFileSnapshot {
    FileIdentity identity{};
    uint64_t size = 0u;
    uint64_t last_write_time = 0u;
};

#if defined(VC_WIN_FILE_TESTING)
struct WinFileStats {
    uint64_t create_file_calls = 0u;
    uint64_t read_calls = 0u;
    uint64_t seek_calls = 0u;
    uint64_t size_queries = 0u;
};

enum class IoBoundary : uint32_t {
    before_open = 1u,
    after_open = 2u,
    before_read = 3u,
    after_read = 4u,
    before_seek = 5u,
    after_seek = 6u,
};

using IoBoundaryHook =
    void (*)(IoBoundary boundary, void* context) noexcept;
#endif

class WinFile {
public:
    ~WinFile() noexcept;
    WinFile(const WinFile&) = delete;
    WinFile& operator=(const WinFile&) = delete;

    static int32_t Open(const std::wstring& path,
                        const CancelState* cancel,
                        Deadline deadline,
                        std::unique_ptr<WinFile>* out,
                        vc_error* error);

    int32_t Read(uint8_t* buffer,
                 int size,
                 int* bytes_read,
                 const CancelState* cancel,
                 Deadline deadline,
                 vc_error* error) noexcept;
    int32_t Seek(int64_t offset,
                 DWORD origin,
                 int64_t* position,
                 const CancelState* cancel,
                 Deadline deadline,
                 vc_error* error) noexcept;
    int64_t SnapshotSize() noexcept;

    HANDLE handle() const noexcept { return handle_; }
    const WinFileSnapshot& snapshot() const noexcept {
        return snapshot_;
    }
#if defined(VC_WIN_FILE_TESTING)
    WinFileStats stats() const noexcept { return stats_; }
    void SetIoHook(IoBoundaryHook hook, void* context) noexcept;
#endif

private:
    explicit WinFile(HANDLE handle) noexcept;
#if defined(VC_WIN_FILE_TESTING)
    void RunHook(IoBoundary boundary) noexcept;
#endif

    HANDLE handle_ = INVALID_HANDLE_VALUE;
    WinFileSnapshot snapshot_{};
#if defined(VC_WIN_FILE_TESTING)
    WinFileStats stats_{};
    IoBoundaryHook hook_ = nullptr;
    void* hook_context_ = nullptr;
#endif
};

#if defined(VC_WIN_FILE_TESTING)
void WinFileTestFailNextAllocation() noexcept;
#endif

}  // namespace vc::detail

#endif
