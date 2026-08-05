#include <windows.h>
#include <bcrypt.h>

#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <limits>

namespace {

enum class FailureMode {
    none,
    return_error,
    throw_exception,
};

FailureMode hash_data_failure = FailureMode::none;
FailureMode finish_hash_failure = FailureMode::none;
int destroy_hash_calls = 0;
int heap_free_calls = 0;
int close_algorithm_calls = 0;

NTSTATUS WINAPI fake_BCryptOpenAlgorithmProvider(
    BCRYPT_ALG_HANDLE* algorithm, LPCWSTR, LPCWSTR, ULONG) {
    *algorithm = reinterpret_cast<BCRYPT_ALG_HANDLE>(1);
    return 0;
}

NTSTATUS WINAPI fake_BCryptGetProperty(
    BCRYPT_HANDLE, LPCWSTR, PUCHAR output, ULONG, ULONG* result, ULONG) {
    *reinterpret_cast<ULONG*>(output) = 16;
    *result = sizeof(ULONG);
    return 0;
}

NTSTATUS WINAPI fake_BCryptCreateHash(
    BCRYPT_ALG_HANDLE, BCRYPT_HASH_HANDLE* hash, PUCHAR, ULONG, PUCHAR, ULONG, ULONG) {
    *hash = reinterpret_cast<BCRYPT_HASH_HANDLE>(2);
    return 0;
}

NTSTATUS WINAPI fake_BCryptDestroyHash(BCRYPT_HASH_HANDLE) {
    ++destroy_hash_calls;
    return 0;
}

NTSTATUS WINAPI fake_BCryptCloseAlgorithmProvider(BCRYPT_ALG_HANDLE, ULONG) {
    ++close_algorithm_calls;
    return 0;
}

NTSTATUS WINAPI fake_BCryptHashData(BCRYPT_HASH_HANDLE, PUCHAR, ULONG, ULONG) {
    if (hash_data_failure == FailureMode::throw_exception) {
        throw 1;
    }
    return hash_data_failure == FailureMode::return_error ? -1 : 0;
}

NTSTATUS WINAPI fake_BCryptFinishHash(BCRYPT_HASH_HANDLE, PUCHAR output, ULONG len, ULONG) {
    if (finish_hash_failure == FailureMode::throw_exception) {
        throw 1;
    }
    if (finish_hash_failure == FailureMode::return_error) {
        return -1;
    }
    std::memset(output, 0, len);
    return 0;
}

HANDLE WINAPI fake_GetProcessHeap() {
    return reinterpret_cast<HANDLE>(3);
}

LPVOID WINAPI fake_HeapAlloc(HANDLE, DWORD, SIZE_T bytes) {
    return std::malloc(bytes);
}

BOOL WINAPI fake_HeapFree(HANDLE, DWORD, LPVOID memory) {
    ++heap_free_calls;
    std::free(memory);
    return TRUE;
}

void reset_fakes() {
    hash_data_failure = FailureMode::none;
    finish_hash_failure = FailureMode::none;
    destroy_hash_calls = 0;
    heap_free_calls = 0;
    close_algorithm_calls = 0;
}

}  // namespace

#define MEDIACORE_BUILD
#define BCryptOpenAlgorithmProvider fake_BCryptOpenAlgorithmProvider
#define BCryptGetProperty fake_BCryptGetProperty
#define BCryptCreateHash fake_BCryptCreateHash
#define BCryptDestroyHash fake_BCryptDestroyHash
#define BCryptCloseAlgorithmProvider fake_BCryptCloseAlgorithmProvider
#define BCryptHashData fake_BCryptHashData
#define BCryptFinishHash fake_BCryptFinishHash
#define GetProcessHeap fake_GetProcessHeap
#define HeapAlloc fake_HeapAlloc
#define HeapFree fake_HeapFree
#include "../src/api.cpp"
#undef HeapFree
#undef HeapAlloc
#undef GetProcessHeap
#undef BCryptFinishHash
#undef BCryptHashData
#undef BCryptCloseAlgorithmProvider
#undef BCryptDestroyHash
#undef BCryptCreateHash
#undef BCryptGetProperty
#undef BCryptOpenAlgorithmProvider

namespace {

int failures = 0;

void check(bool condition, const char* message) {
    if (!condition) {
        std::fprintf(stderr, "FAIL internal cleanup: %s\n", message);
        ++failures;
    }
}

void expect_resources_released_once(const char* stage) {
    if (destroy_hash_calls != 1 || heap_free_calls != 1 || close_algorithm_calls != 1) {
        std::fprintf(
            stderr,
            "FAIL %s cleanup: destroy=%d heap_free=%d close=%d; want 1/1/1\n",
            stage,
            destroy_hash_calls,
            heap_free_calls,
            close_algorithm_calls);
        ++failures;
    }
}

void test_chunk_size() {
    if (std::numeric_limits<size_t>::max() >
        static_cast<size_t>((std::numeric_limits<ULONG>::max)())) {
        const size_t remaining =
            static_cast<size_t>((std::numeric_limits<ULONG>::max)()) + 17;
        const ULONG first = next_chunk_size(remaining);
        check(first == (std::numeric_limits<ULONG>::max)(),
              "ULONG_MAX+17 first chunk was not ULONG_MAX");
        check(next_chunk_size(remaining - first) == 17,
              "ULONG_MAX+17 remainder chunk was not 17");
    }
}

void test_png_error_mapping() {
    check(
        std::strcmp(png_failure_message(MC_ERR_OOM), "out of memory decoding PNG") == 0,
        "PNG OOM error was mislabeled as corrupt input");
    check(
        std::strcmp(png_failure_message(MC_ERR_SIZE), "PNG dimensions exceed limits") == 0,
        "PNG size error message changed");
    check(
        std::strcmp(png_failure_message(MC_ERR_DECODE), "PNG is corrupt or truncated") == 0,
        "PNG decode error message changed");
}

void test_update_failure_cleanup(FailureMode mode, const char* stage) {
    reset_fakes();
    mc_sha512* ctx = mc_sha512_new();
    check(ctx != nullptr, "fake-backed context construction failed");
    if (ctx == nullptr) {
        return;
    }
    hash_data_failure = mode;
    const uint8_t byte = 'a';
    char errbuf[MC_ERRBUF_LEN];
    check(mc_sha512_update(ctx, &byte, 1, errbuf, sizeof(errbuf)) == MC_ERR_INTERNAL,
          "forced BCryptHashData failure did not return MC_ERR_INTERNAL");
    expect_resources_released_once(stage);
    mc_sha512_free(ctx);
    expect_resources_released_once(stage);
}

void test_final_failure_cleanup(FailureMode mode, const char* stage) {
    reset_fakes();
    mc_sha512* ctx = mc_sha512_new();
    check(ctx != nullptr, "fake-backed context construction failed");
    if (ctx == nullptr) {
        return;
    }
    finish_hash_failure = mode;
    uint8_t out[MC_SHA512_BYTES]{};
    char errbuf[MC_ERRBUF_LEN];
    check(mc_sha512_final(ctx, out, errbuf, sizeof(errbuf)) == MC_ERR_INTERNAL,
          "forced BCryptFinishHash failure did not return MC_ERR_INTERNAL");
    expect_resources_released_once(stage);
    mc_sha512_free(ctx);
    expect_resources_released_once(stage);
}

}  // namespace

int main() {
    test_chunk_size();
    test_png_error_mapping();
    test_update_failure_cleanup(FailureMode::return_error, "HashData status");
    test_update_failure_cleanup(FailureMode::throw_exception, "HashData exception");
    test_final_failure_cleanup(FailureMode::return_error, "FinishHash status");
    test_final_failure_cleanup(FailureMode::throw_exception, "FinishHash exception");

    if (failures != 0) {
        std::fprintf(stderr, "%d internal test assertion(s) failed\n", failures);
        return 1;
    }
    std::printf("mediacore internal SHA-512 tests passed\n");
    return 0;
}
