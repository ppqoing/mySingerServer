#include <cstdint>
#include <cstring>
#include <iostream>
#include <string>

extern "C" {
#include <libavcodec/version.h>
#include <libavformat/version.h>
#include <libavutil/version.h>
#include <libswscale/version.h>
}

#include "runtime_info.h"
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

struct LiteralVersionProvider final : vc::detail::VersionProvider {
    uint32_t avformat = LIBAVFORMAT_VERSION_INT;
    uint32_t avcodec = LIBAVCODEC_VERSION_INT;
    uint32_t avutil = LIBAVUTIL_VERSION_INT;
    uint32_t swscale = LIBSWSCALE_VERSION_INT;
    const char* build_id = "injected-build";

    uint32_t AvFormatVersion() const noexcept override {
        return avformat;
    }
    uint32_t AvCodecVersion() const noexcept override {
        return avcodec;
    }
    uint32_t AvUtilVersion() const noexcept override {
        return avutil;
    }
    uint32_t SwScaleVersion() const noexcept override {
        return swscale;
    }
    const char* BuildId() const noexcept override {
        return build_id;
    }
};

void TestInjectedMajorMismatchForEveryComponent() {
    struct MismatchCase {
        const char* name;
        uint32_t LiteralVersionProvider::*member;
        uint32_t mismatched_version;
    };
    const MismatchCase cases[] = {
        {"avformat", &LiteralVersionProvider::avformat,
         (LIBAVFORMAT_VERSION_MAJOR - 1u) << 16u},
        {"avcodec", &LiteralVersionProvider::avcodec,
         (LIBAVCODEC_VERSION_MAJOR - 1u) << 16u},
        {"avutil", &LiteralVersionProvider::avutil,
         (LIBAVUTIL_VERSION_MAJOR - 1u) << 16u},
        {"swscale", &LiteralVersionProvider::swscale,
         (LIBSWSCALE_VERSION_MAJOR - 1u) << 16u},
    };

    for (const MismatchCase& test_case : cases) {
        LiteralVersionProvider provider;
        provider.*(test_case.member) = test_case.mismatched_version;
        struct vc_runtime_info info{};
        info.struct_size = sizeof(info);
        info.abi_version = VC_ABI_VERSION;
        std::memset(info.videocore_version_utf8,
                    0x5a,
                    sizeof(info.videocore_version_utf8));
        const struct vc_runtime_info snapshot = info;
        vc_error error = FreshError();

        const int32_t status =
            vc::detail::PopulateRuntimeInfo(&info, &error, provider);
        Check(status == VC_ERR_ABI, test_case.name);
        Check(error.code == VC_ERR_ABI,
              "major mismatch must populate VC_ERR_ABI");
        Check(std::strstr(error.message_utf8, test_case.name) != nullptr,
              "major mismatch must identify the component");
        Check(std::memcmp(&info, &snapshot, sizeof(info)) == 0,
              "major mismatch must not publish partial runtime info");
    }
}

void TestInjectedMatchingVersionsPopulateAllFields() {
    LiteralVersionProvider provider;
    provider.avformat = (LIBAVFORMAT_VERSION_MAJOR << 16u) | 0x0102u;
    provider.avcodec = (LIBAVCODEC_VERSION_MAJOR << 16u) | 0x0304u;
    provider.avutil = (LIBAVUTIL_VERSION_MAJOR << 16u) | 0x0506u;
    provider.swscale = (LIBSWSCALE_VERSION_MAJOR << 16u) | 0x0708u;
    struct vc_runtime_info info{};
    info.struct_size = sizeof(info);
    info.abi_version = VC_ABI_VERSION;
    vc_error error = FreshError();

    const int32_t status =
        vc::detail::PopulateRuntimeInfo(&info, &error, provider);
    Check(status == VC_OK, "matching injected versions must pass");
    Check(error.code == VC_OK, "matching versions must clear error status");
    Check(std::strcmp(info.videocore_version_utf8,
                      VC_VERSION_STRING) == 0,
          "runtime info must contain VideoCore version");
    Check(std::strcmp(info.ffmpeg_build_id_utf8,
                      "injected-build") == 0,
          "runtime info must contain provider build ID");
    Check(info.avformat_header_version == LIBAVFORMAT_VERSION_INT,
          "avformat header version");
    Check(info.avformat_runtime_version == provider.avformat,
          "avformat runtime version");
    Check(info.avcodec_header_version == LIBAVCODEC_VERSION_INT,
          "avcodec header version");
    Check(info.avcodec_runtime_version == provider.avcodec,
          "avcodec runtime version");
    Check(info.avutil_header_version == LIBAVUTIL_VERSION_INT,
          "avutil header version");
    Check(info.avutil_runtime_version == provider.avutil,
          "avutil runtime version");
    Check(info.swscale_header_version == LIBSWSCALE_VERSION_INT,
          "swscale header version");
    Check(info.swscale_runtime_version == provider.swscale,
          "swscale runtime version");
}

void TestBuildIdBoundaries() {
    struct InvalidCase {
        const char* name;
        const char* value;
    };
    const std::string sixty_four_ascii(64u, 'a');
    const std::string invalid_utf8("\xc3\x28", 2u);
    const InvalidCase invalid_cases[] = {
        {"null build ID", nullptr},
        {"empty build ID", ""},
        {"64-byte build ID", sixty_four_ascii.c_str()},
        {"invalid UTF-8 build ID", invalid_utf8.c_str()},
    };

    for (const InvalidCase& test_case : invalid_cases) {
        LiteralVersionProvider provider;
        provider.build_id = test_case.value;
        struct vc_runtime_info info{};
        info.struct_size = sizeof(info);
        info.abi_version = VC_ABI_VERSION;
        std::memset(info.ffmpeg_build_id_utf8,
                    0x6b,
                    sizeof(info.ffmpeg_build_id_utf8));
        const struct vc_runtime_info snapshot = info;
        vc_error error = FreshError();

        const int32_t status =
            vc::detail::PopulateRuntimeInfo(&info, &error, provider);
        Check(status == VC_ERR_ABI, test_case.name);
        Check(error.code == VC_ERR_ABI,
              "invalid build ID must populate VC_ERR_ABI");
        Check(std::memcmp(&info, &snapshot, sizeof(info)) == 0,
              "invalid build ID must not publish partial output");
    }

    const std::string sixty_three_ascii(63u, 'b');
    LiteralVersionProvider ascii_provider;
    ascii_provider.build_id = sixty_three_ascii.c_str();
    struct vc_runtime_info ascii_info{};
    ascii_info.struct_size = sizeof(ascii_info);
    ascii_info.abi_version = VC_ABI_VERSION;
    vc_error ascii_error = FreshError();
    Check(vc::detail::PopulateRuntimeInfo(
              &ascii_info, &ascii_error, ascii_provider) == VC_OK,
          "63-byte build ID must fit");
    Check(std::memcmp(ascii_info.ffmpeg_build_id_utf8,
                      sixty_three_ascii.data(),
                      sixty_three_ascii.size()) == 0,
          "63-byte build ID payload");
    Check(ascii_info.ffmpeg_build_id_utf8[63] == '\0',
          "63-byte build ID terminator");

    const std::string utf8_exact =
        std::string(60u, 'u') + std::string("\xe2\x98\x83", 3u);
    LiteralVersionProvider utf8_provider;
    utf8_provider.build_id = utf8_exact.c_str();
    struct vc_runtime_info utf8_info{};
    utf8_info.struct_size = sizeof(utf8_info);
    utf8_info.abi_version = VC_ABI_VERSION;
    vc_error utf8_error = FreshError();
    Check(vc::detail::PopulateRuntimeInfo(
              &utf8_info, &utf8_error, utf8_provider) == VC_OK,
          "63-byte UTF-8 build ID must fit without truncation");
    Check(std::memcmp(utf8_info.ffmpeg_build_id_utf8,
                      utf8_exact.data(),
                      utf8_exact.size()) == 0,
          "UTF-8 boundary payload");
    Check(utf8_info.ffmpeg_build_id_utf8[63] == '\0',
          "UTF-8 boundary terminator");

    const std::string utf8_over =
        std::string(61u, 'u') + std::string("\xe2\x98\x83", 3u);
    LiteralVersionProvider utf8_over_provider;
    utf8_over_provider.build_id = utf8_over.c_str();
    struct vc_runtime_info utf8_over_info{};
    utf8_over_info.struct_size = sizeof(utf8_over_info);
    utf8_over_info.abi_version = VC_ABI_VERSION;
    const struct vc_runtime_info utf8_over_snapshot = utf8_over_info;
    vc_error utf8_over_error = FreshError();
    Check(vc::detail::PopulateRuntimeInfo(
              &utf8_over_info,
              &utf8_over_error,
              utf8_over_provider) == VC_ERR_ABI,
          "64-byte UTF-8 build ID must be rejected");
    Check(std::memcmp(&utf8_over_info,
                      &utf8_over_snapshot,
                      sizeof(utf8_over_info)) == 0,
          "over-capacity UTF-8 must leave output unchanged");
}

void TestRealRuntimeProviderThroughPublicAbi() {
    struct vc_runtime_info info{};
    info.struct_size = sizeof(info);
    info.abi_version = VC_ABI_VERSION;
    vc_error error = FreshError();

    const int32_t status = vc_runtime_info(&info, &error);
    Check(status == VC_OK, "real runtime versions must pass the ABI gate");
    Check(error.code == VC_OK, "real runtime info error status");
    Check(info.abi_version == VC_ABI_VERSION, "runtime ABI version");
    Check(info.videocore_version_utf8[0] != '\0',
          "VideoCore version must not be empty");
    Check(info.ffmpeg_build_id_utf8[0] != '\0',
          "FFmpeg build ID must not be empty");
    Check((info.avformat_header_version >> 16u) ==
              (info.avformat_runtime_version >> 16u),
          "real avformat major");
    Check((info.avcodec_header_version >> 16u) ==
              (info.avcodec_runtime_version >> 16u),
          "real avcodec major");
    Check((info.avutil_header_version >> 16u) ==
              (info.avutil_runtime_version >> 16u),
          "real avutil major");
    Check((info.swscale_header_version >> 16u) ==
              (info.swscale_runtime_version >> 16u),
          "real swscale major");

    std::cout << "RUNTIME_VERSIONS"
              << " build_id=" << info.ffmpeg_build_id_utf8
              << " avformat=" << info.avformat_header_version << "/"
              << info.avformat_runtime_version
              << " avcodec=" << info.avcodec_header_version << "/"
              << info.avcodec_runtime_version
              << " avutil=" << info.avutil_header_version << "/"
              << info.avutil_runtime_version
              << " swscale=" << info.swscale_header_version << "/"
              << info.swscale_runtime_version << '\n';
}

}  // namespace

int main() {
    TestInjectedMajorMismatchForEveryComponent();
    TestInjectedMatchingVersionsPopulateAllFields();
    TestBuildIdBoundaries();
    TestRealRuntimeProviderThroughPublicAbi();
    if (failures != 0) {
        std::cerr << failures << " runtime test(s) failed\n";
        return 1;
    }
    std::cout << "videocore runtime tests passed\n";
    return 0;
}
