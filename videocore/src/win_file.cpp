#include "win_file.h"

#include <atomic>
#include <cstring>
#include <limits>
#include <new>

#include "cancel_token.h"
#include "error.h"

namespace vc::detail {
namespace {

class ScopedHandle {
public:
    explicit ScopedHandle(HANDLE handle) noexcept : handle_(handle) {}
    ~ScopedHandle() noexcept {
        if (handle_ != INVALID_HANDLE_VALUE) {
            CloseHandle(handle_);
        }
    }
    ScopedHandle(const ScopedHandle&) = delete;
    ScopedHandle& operator=(const ScopedHandle&) = delete;

    HANDLE get() const noexcept { return handle_; }
    HANDLE release() noexcept {
        const HANDLE handle = handle_;
        handle_ = INVALID_HANDLE_VALUE;
        return handle;
    }

private:
    HANDLE handle_;
};

#if defined(VC_WIN_FILE_TESTING)
std::atomic<bool> fail_next_win_file_allocation{false};
#endif

int32_t Interrupted(const CancelState* cancel,
                    Deadline deadline,
                    vc_error* error) noexcept {
    const int32_t status = CheckInterrupt(cancel, deadline);
    if (status == VC_ERR_CANCELLED) {
        SetError(error, status, 0, 0, "operation cancelled");
    } else if (status == VC_ERR_TIMEOUT) {
        SetError(error, status, 0, 0, "operation timed out");
    }
    return status;
}

int32_t InterruptedAt(const CancelState* cancel,
                      Deadline deadline,
                      OperationBoundary boundary,
                      vc_error* error) noexcept {
    const int32_t status =
        CheckOperationBoundary(cancel, deadline, boundary);
    if (status == VC_ERR_CANCELLED) {
        SetError(error, status, 0, 0, "operation cancelled");
    } else if (status == VC_ERR_TIMEOUT) {
        SetError(error, status, 0, 0, "operation timed out");
    }
    return status;
}

int32_t IoFailure(vc_error* error,
                  DWORD win32_code,
                  const char* message) noexcept {
    SetError(error, VC_ERR_IO, 0, win32_code, message);
    return VC_ERR_IO;
}

}  // namespace

#if !defined(VC_WIN_FILE_TESTING) && defined(_MSC_VER) && \
    defined(_WIN64) && _ITERATOR_DEBUG_LEVEL == 0
static_assert(
    sizeof(WinFile) == 48u,
    "MSVC x64 production WinFile contains unexpected state");
#endif

WinFile::WinFile(HANDLE handle) noexcept : handle_(handle) {
#if defined(VC_WIN_FILE_TESTING)
    stats_.create_file_calls = 1u;
#endif
}

WinFile::~WinFile() noexcept {
    if (handle_ != INVALID_HANDLE_VALUE) {
        CloseHandle(handle_);
    }
}

int32_t WinFile::Open(const std::wstring& path,
                      const CancelState* cancel,
                      Deadline deadline,
                      std::unique_ptr<WinFile>* out,
                      vc_error* error) {
    if (out == nullptr) {
        SetError(error,
                 VC_ERR_INVALID_ARG,
                 0,
                 0,
                 "WinFile output is null");
        return VC_ERR_INVALID_ARG;
    }
    int32_t status = InterruptedAt(
        cancel, deadline, OperationBoundary::open, error);
    if (status != VC_OK) {
        return status;
    }

    HANDLE handle = CreateFileW(path.c_str(),
                                GENERIC_READ,
                                FILE_SHARE_READ | FILE_SHARE_WRITE |
                                    FILE_SHARE_DELETE,
                                nullptr,
                                OPEN_EXISTING,
                                FILE_ATTRIBUTE_NORMAL,
                                nullptr);
    if (handle == INVALID_HANDLE_VALUE) {
        return IoFailure(error,
                         GetLastError(),
                         "CreateFileW failed");
    }
    ScopedHandle handle_owner(handle);
#if defined(VC_WIN_FILE_TESTING)
    if (fail_next_win_file_allocation.exchange(
            false, std::memory_order_acq_rel)) {
        throw std::bad_alloc();
    }
#endif
    std::unique_ptr<WinFile> candidate(
        new WinFile(handle_owner.get()));
    handle_owner.release();
    status = Interrupted(cancel, deadline, error);
    if (status != VC_OK) {
        return status;
    }

    FILE_STANDARD_INFO standard{};
    if (GetFileInformationByHandleEx(
            handle,
            FileStandardInfo,
            &standard,
            sizeof(standard)) == FALSE) {
        return IoFailure(error,
                         GetLastError(),
                         "file size snapshot failed");
    }
    if (standard.EndOfFile.QuadPart < 0) {
        return IoFailure(error,
                         ERROR_FILE_INVALID,
                         "negative file size");
    }
    candidate->snapshot_.size =
        static_cast<uint64_t>(standard.EndOfFile.QuadPart);

    FILE_BASIC_INFO basic{};
    if (GetFileInformationByHandleEx(
            handle,
            FileBasicInfo,
            &basic,
            sizeof(basic)) == FALSE) {
        return IoFailure(error,
                         GetLastError(),
                         "file timestamp snapshot failed");
    }
    candidate->snapshot_.last_write_time =
        static_cast<uint64_t>(basic.LastWriteTime.QuadPart);

    FILE_ID_INFO identity{};
    if (GetFileInformationByHandleEx(
            handle,
            FileIdInfo,
            &identity,
            sizeof(identity)) == FALSE) {
        return IoFailure(error,
                         GetLastError(),
                         "file identity snapshot failed");
    }
    candidate->snapshot_.identity.volume_serial =
        identity.VolumeSerialNumber;
    static_assert(sizeof(identity.FileId.Identifier) == 16u);
    std::memcpy(
        &candidate->snapshot_.identity.file_id_low,
        identity.FileId.Identifier,
        sizeof(uint64_t));
    std::memcpy(
        &candidate->snapshot_.identity.file_id_high,
        identity.FileId.Identifier + sizeof(uint64_t),
        sizeof(uint64_t));

    status = Interrupted(cancel, deadline, error);
    if (status != VC_OK) {
        return status;
    }
    *out = std::move(candidate);
    SetError(error, VC_OK, 0, 0, "");
    return VC_OK;
}

int32_t WinFile::Read(uint8_t* buffer,
                      int size,
                      int* bytes_read,
                      const CancelState* cancel,
                      Deadline deadline,
                      vc_error* error) noexcept {
    if (buffer == nullptr || size < 0 || bytes_read == nullptr) {
        SetError(error,
                 VC_ERR_INVALID_ARG,
                 0,
                 0,
                 "read arguments are invalid");
        return VC_ERR_INVALID_ARG;
    }
    int32_t status = Interrupted(cancel, deadline, error);
    if (status != VC_OK) {
        return status;
    }
#if defined(VC_WIN_FILE_TESTING)
    RunHook(IoBoundary::before_read);
#endif
    status = Interrupted(cancel, deadline, error);
    if (status != VC_OK) {
        return status;
    }

    DWORD transferred = 0u;
#if defined(VC_WIN_FILE_TESTING)
    ++stats_.read_calls;
#endif
    if (ReadFile(handle_,
                 buffer,
                 static_cast<DWORD>(size),
                 &transferred,
                 nullptr) == FALSE) {
        return IoFailure(error, GetLastError(), "ReadFile failed");
    }
#if defined(VC_WIN_FILE_TESTING)
    RunHook(IoBoundary::after_read);
#endif
    status = Interrupted(cancel, deadline, error);
    if (status != VC_OK) {
        return status;
    }
    *bytes_read = static_cast<int>(transferred);
    SetError(error, VC_OK, 0, 0, "");
    return VC_OK;
}

int32_t WinFile::Seek(int64_t offset,
                      DWORD origin,
                      int64_t* position,
                      const CancelState* cancel,
                      Deadline deadline,
                      vc_error* error) noexcept {
    if (position == nullptr ||
        (origin != FILE_BEGIN &&
         origin != FILE_CURRENT &&
         origin != FILE_END)) {
        SetError(error,
                 VC_ERR_INVALID_ARG,
                 0,
                 0,
                 "seek arguments are invalid");
        return VC_ERR_INVALID_ARG;
    }
    int32_t status = Interrupted(cancel, deadline, error);
    if (status != VC_OK) {
        return status;
    }
#if defined(VC_WIN_FILE_TESTING)
    RunHook(IoBoundary::before_seek);
#endif
    status = Interrupted(cancel, deadline, error);
    if (status != VC_OK) {
        return status;
    }

    LARGE_INTEGER distance{};
    distance.QuadPart = offset;
    LARGE_INTEGER new_position{};
#if defined(VC_WIN_FILE_TESTING)
    ++stats_.seek_calls;
#endif
    if (SetFilePointerEx(
            handle_, distance, &new_position, origin) == FALSE) {
        return IoFailure(error,
                         GetLastError(),
                         "SetFilePointerEx failed");
    }
#if defined(VC_WIN_FILE_TESTING)
    RunHook(IoBoundary::after_seek);
#endif
    status = Interrupted(cancel, deadline, error);
    if (status != VC_OK) {
        return status;
    }
    *position = new_position.QuadPart;
    SetError(error, VC_OK, 0, 0, "");
    return VC_OK;
}

int64_t WinFile::SnapshotSize() noexcept {
#if defined(VC_WIN_FILE_TESTING)
    ++stats_.size_queries;
#endif
    if (snapshot_.size >
        static_cast<uint64_t>((std::numeric_limits<int64_t>::max)())) {
        return -1;
    }
    return static_cast<int64_t>(snapshot_.size);
}

#if defined(VC_WIN_FILE_TESTING)
void WinFile::SetIoHook(IoBoundaryHook hook,
                        void* context) noexcept {
    hook_ = hook;
    hook_context_ = context;
}

void WinFile::RunHook(IoBoundary boundary) noexcept {
    if (hook_ != nullptr) {
        hook_(boundary, hook_context_);
    }
}
#endif

#if defined(VC_WIN_FILE_TESTING)
void WinFileTestFailNextAllocation() noexcept {
    fail_next_win_file_allocation.store(
        true, std::memory_order_release);
}
#endif

}  // namespace vc::detail
