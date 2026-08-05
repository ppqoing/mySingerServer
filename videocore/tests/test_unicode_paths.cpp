#define WIN32_LEAN_AND_MEAN
#include <windows.h>

#include <array>
#include <cstdint>
#include <cstdlib>
#include <iostream>
#include <string>
#include <vector>

#include "videocore/videocore.h"
#include "url_path.h"

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

vc_media_open_options FreshOptions() {
    vc_media_open_options options{};
    options.struct_size = sizeof(options);
    options.abi_version = VC_ABI_VERSION;
    options.expected_media_type = VC_MEDIA_TYPE_AUTO;
    return options;
}

bool WriteAbc(const std::wstring& path) {
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
    const uint8_t bytes[] = {'a', 'b', 'c'};
    DWORD written = 0;
    const bool ok =
        WriteFile(file, bytes, sizeof(bytes), &written, nullptr) != FALSE &&
        written == sizeof(bytes);
    CloseHandle(file);
    return ok;
}

int32_t OpenUnits(const uint16_t* path,
                  uint32_t units,
                  vc_media_session** out,
                  vc_error* error) {
    const vc_media_open_options options = FreshOptions();
    return vc_media_open_w(path, units, &options, nullptr, out, error);
}

int32_t OpenPath(const std::wstring& path,
                 vc_media_session** out,
                 vc_error* error) {
    return OpenUnits(
        reinterpret_cast<const uint16_t*>(path.data()),
        static_cast<uint32_t>(path.size()),
        out,
        error);
}

std::wstring TemporaryDirectory() {
    wchar_t temp[MAX_PATH]{};
    const DWORD length = GetTempPathW(MAX_PATH, temp);
    Check(length != 0u && length < MAX_PATH,
          "GetTempPathW returns a temporary directory");
    return std::wstring(temp) + L"videocore-unicode-" +
           std::to_wstring(GetCurrentProcessId());
}

bool IsSupportedUncTestPath(const std::wstring& path) {
    constexpr wchar_t extended_unc_prefix[] = L"\\\\?\\UNC\\";
    constexpr size_t extended_unc_prefix_length = 8u;
    auto ascii_upper = [](wchar_t value) noexcept {
        return value >= L'a' && value <= L'z'
                   ? static_cast<wchar_t>(value - L'a' + L'A')
                   : value;
    };
    bool is_extended_unc =
        path.size() >= extended_unc_prefix_length;
    for (size_t index = 0u;
         is_extended_unc && index < extended_unc_prefix_length;
         ++index) {
        is_extended_unc =
            ascii_upper(path[index]) == extended_unc_prefix[index];
    }

    size_t server_start = 0u;
    if (is_extended_unc) {
        server_start = extended_unc_prefix_length;
    } else {
        if (path.size() < 2u || path[0] != L'\\' ||
            path[1] != L'\\') {
            return false;
        }
        if (path.size() >= 4u &&
            (path[2] == L'?' || path[2] == L'.') &&
            path[3] == L'\\') {
            return false;
        }
        server_start = 2u;
    }

    const size_t server_end = path.find(L'\\', server_start);
    if (server_end == std::wstring::npos ||
        server_end == server_start) {
        return false;
    }
    const size_t share_start = server_end + 1u;
    if (share_start >= path.size()) {
        return false;
    }
    const size_t share_end = path.find(L'\\', share_start);
    return share_end == std::wstring::npos ||
           share_end > share_start;
}

void TestSpacesAndEmoji() {
    const std::wstring directory = TemporaryDirectory();
    CreateDirectoryW(directory.c_str(), nullptr);
    const std::wstring path =
        directory + L"\\media space \U0001F600 sample.bin";
    Check(WriteAbc(path), "spaces and emoji fixture write");

    vc_media_session* session = nullptr;
    vc_error error = FreshError();
    const int32_t status = OpenPath(path, &session, &error);
    Check(status == VC_OK,
          "UTF-16 path with spaces and emoji opens");
    if (status == VC_OK) {
        std::array<uint8_t, VC_SHA512_SIZE> digest{};
        error = FreshError();
        Check(vc_media_hash(session, digest.data(), &error) == VC_OK,
              "UTF-16 path hashes");
        vc_media_close(session);
    }
    std::cout << "UNICODE_PATH"
              << " spaces_emoji_units=" << path.size() << '\n';

    DeleteFileW(path.c_str());
    RemoveDirectoryW(directory.c_str());
}

std::wstring AddLongPathPrefix(const std::wstring& absolute) {
    if (absolute.rfind(L"\\\\", 0u) == 0u) {
        return L"\\\\?\\UNC\\" + absolute.substr(2u);
    }
    return L"\\\\?\\" + absolute;
}

void TestExplicitLongPath() {
    wchar_t short_temp[MAX_PATH]{};
    const DWORD length = GetTempPathW(MAX_PATH, short_temp);
    Check(length != 0u && length < MAX_PATH,
          "long path temporary root");
    std::wstring root =
        AddLongPathPrefix(std::wstring(short_temp) +
                          L"videocore-long-" +
                          std::to_wstring(GetCurrentProcessId()));
    std::vector<std::wstring> directories;
    directories.push_back(root);
    CreateDirectoryW(root.c_str(), nullptr);
    while (root.size() < 285u) {
        root += L"\\segment-0123456789abcdef";
        directories.push_back(root);
        Check(CreateDirectoryW(root.c_str(), nullptr) != FALSE ||
                  GetLastError() == ERROR_ALREADY_EXISTS,
              "long path directory creation");
    }
    const std::wstring path = root + L"\\long \U0001F600 media.bin";
    Check(path.size() > MAX_PATH, "long path exceeds MAX_PATH");
    Check(WriteAbc(path), "long path fixture write");

    vc_media_session* session = nullptr;
    vc_error error = FreshError();
    const int32_t status = OpenPath(path, &session, &error);
    Check(status == VC_OK, "explicit long path opens");
    if (status == VC_OK) {
        std::array<uint8_t, VC_SHA512_SIZE> digest{};
        error = FreshError();
        Check(vc_media_hash(session, digest.data(), &error) == VC_OK,
              "explicit long path hashes");
        vc_media_close(session);
    }
    std::cout << "LONG_PATH"
              << " prefix=true units=" << path.size() << '\n';

    DeleteFileW(path.c_str());
    for (auto iterator = directories.rbegin();
         iterator != directories.rend();
         ++iterator) {
        RemoveDirectoryW(iterator->c_str());
    }
}

void TestInvalidPathFormsLeaveOutputUnchanged() {
    vc_media_session* const sentinel =
        reinterpret_cast<vc_media_session*>(0x2468);

    vc_media_session* session = sentinel;
    vc_error error = FreshError();
    Check(OpenUnits(nullptr, 0u, &session, &error) ==
              VC_ERR_INVALID_ARG,
          "null empty path is rejected");
    Check(session == sentinel,
          "null empty path leaves session output unchanged");

    const uint16_t embedded[] = {
        static_cast<uint16_t>('C'),
        static_cast<uint16_t>(':'),
        static_cast<uint16_t>('\\'),
        static_cast<uint16_t>('a'),
        0u,
        static_cast<uint16_t>('b'),
    };
    session = sentinel;
    error = FreshError();
    Check(OpenUnits(embedded,
                    static_cast<uint32_t>(std::size(embedded)),
                    &session,
                    &error) == VC_ERR_INVALID_ARG,
          "embedded NUL is rejected");
    Check(session == sentinel,
          "embedded NUL leaves session output unchanged");

    const std::u16string url = u"https://example.invalid/media.bin";
    session = sentinel;
    error = FreshError();
    Check(OpenUnits(
              reinterpret_cast<const uint16_t*>(url.data()),
              static_cast<uint32_t>(url.size()),
              &session,
              &error) == VC_ERR_INVALID_ARG,
          "URL is rejected");
    Check(session == sentinel,
          "URL rejection leaves session output unchanged");

    const std::u16string one_letter_url =
        u"x://host/media.bin";
    session = sentinel;
    error = FreshError();
    Check(OpenUnits(
              reinterpret_cast<const uint16_t*>(
                  one_letter_url.data()),
              static_cast<uint32_t>(one_letter_url.size()),
              &session,
              &error) == VC_ERR_INVALID_ARG,
          "one-letter URL scheme is rejected");
    Check(session == sentinel,
          "one-letter URL rejection leaves session output unchanged");
}

void TestUncPathClassification() {
    struct PathCase {
        const wchar_t* path;
        bool expected;
        const char* name;
    };
    const PathCase cases[] = {
        {L"\\\\server\\share\\media.bin", true,
         "standard UNC file path is accepted"},
        {L"\\\\?\\UNC\\server\\share\\media.bin", true,
         "extended UNC file path is accepted"},
        {L"\\\\?\\C:\\media.bin", false,
         "extended local path is rejected"},
        {L"\\\\.\\PhysicalDrive0", false,
         "device path is rejected"},
        {L"\\\\", false,
         "bare double backslash is rejected"},
        {L"\\\\\\share\\media.bin", false,
         "UNC path with empty server is rejected"},
        {L"\\\\server", false,
         "UNC path without share is rejected"},
        {L"\\\\server\\\\media.bin", false,
         "UNC path with empty share is rejected"},
        {L"\\\\?\\UNC\\", false,
         "extended UNC path without server or share is rejected"},
        {L"\\\\?\\UNC\\\\share\\media.bin", false,
         "extended UNC path with empty server is rejected"},
        {L"\\\\?\\UNC\\server\\\\media.bin", false,
         "extended UNC path with empty share is rejected"},
    };

    for (const PathCase& test_case : cases) {
        Check(IsSupportedUncTestPath(test_case.path) ==
                  test_case.expected,
              test_case.name);
    }
}

void TestUrlPathClassification() {
    struct PathCase {
        const char16_t* path;
        bool expected;
        const char* name;
    };
    const PathCase cases[] = {
        {u"x://host/media.bin", true,
         "one-letter URL scheme is classified as URL"},
        {u"https://example.invalid/media.bin", true,
         "multi-letter URL scheme is classified as URL"},
        {u"C:\\media.bin", false,
         "backslash drive path is not classified as URL"},
        {u"C:/media.bin", false,
         "slash drive path is not classified as URL"},
        {u"C:relative", false,
         "drive-relative path is not classified as URL"},
    };

    for (const PathCase& test_case : cases) {
        const std::u16string path(test_case.path);
        Check(vc::detail::LooksLikeUrl(
                  reinterpret_cast<const uint16_t*>(path.data()),
                  static_cast<uint32_t>(path.size())) ==
                  test_case.expected,
              test_case.name);
    }
}

void TestOptionalUncGate() {
    wchar_t path[32768]{};
    const DWORD length =
        GetEnvironmentVariableW(L"VC_TEST_UNC_FILE",
                                path,
                                static_cast<DWORD>(std::size(path)));
    if (length == 0u) {
        std::cout
            << "UNC_GATE NOT_RUN reason=VC_TEST_UNC_FILE_missing\n";
        return;
    }
    if (length >= std::size(path)) {
        Check(false, "VC_TEST_UNC_FILE is too long");
        return;
    }
    const std::wstring unc(path, length);
    if (!IsSupportedUncTestPath(unc)) {
        Check(false, "VC_TEST_UNC_FILE must be a UNC path");
        return;
    }
    vc_media_session* session = nullptr;
    vc_error error = FreshError();
    const int32_t status = OpenPath(unc, &session, &error);
    Check(status == VC_OK, "configured UNC path opens");
    if (status == VC_OK) {
        vc_media_close(session);
        std::cout << "UNC_GATE PASS configured_file=true\n";
    }
}

}  // namespace

int main() {
    TestSpacesAndEmoji();
    TestExplicitLongPath();
    TestInvalidPathFormsLeaveOutputUnchanged();
    TestUncPathClassification();
    TestUrlPathClassification();
    TestOptionalUncGate();
    if (failures != 0) {
        std::cerr << failures << " unicode path test(s) failed\n";
        return 1;
    }
    std::cout << "videocore unicode tests passed\n";
    return 0;
}
