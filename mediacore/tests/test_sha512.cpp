#include <mediacore/mediacore.h>

#include <array>
#include <cstdio>
#include <cstring>
#include <string>
#include <vector>

namespace {

int failures = 0;

void fail(const char* test, const std::string& detail) {
    std::fprintf(stderr, "FAIL %s: %s\n", test, detail.c_str());
    ++failures;
}

std::string hex_of(const uint8_t* bytes, size_t len) {
    static constexpr char digits[] = "0123456789abcdef";
    std::string result;
    result.reserve(len * 2);
    for (size_t i = 0; i < len; ++i) {
        result.push_back(digits[bytes[i] >> 4]);
        result.push_back(digits[bytes[i] & 0x0f]);
    }
    return result;
}

bool digest(const uint8_t* data, size_t len, std::array<uint8_t, MC_SHA512_BYTES>& out,
            const char* test) {
    mc_sha512* ctx = mc_sha512_new();
    if (ctx == nullptr) {
        fail(test, "mc_sha512_new returned null");
        return false;
    }

    char errbuf[MC_ERRBUF_LEN];
    const int update_rc = mc_sha512_update(ctx, data, len, errbuf, sizeof(errbuf));
    if (update_rc != MC_OK) {
        fail(test, "update rc=" + std::to_string(update_rc) + " err=" + errbuf);
        mc_sha512_free(ctx);
        return false;
    }

    const int final_rc = mc_sha512_final(ctx, out.data(), errbuf, sizeof(errbuf));
    if (final_rc != MC_OK) {
        fail(test, "final rc=" + std::to_string(final_rc) + " err=" + errbuf);
        mc_sha512_free(ctx);
        return false;
    }
    mc_sha512_free(ctx);
    return true;
}

void expect_digest(const char* test, const uint8_t* data, size_t len, const char* expected) {
    std::array<uint8_t, MC_SHA512_BYTES> out{};
    if (!digest(data, len, out, test)) {
        return;
    }
    const std::string got = hex_of(out.data(), out.size());
    if (got != expected) {
        fail(test, "digest mismatch: got=" + got + " want=" + expected);
    }
}

void test_version() {
    const char* version = mc_version();
    if (version == nullptr) {
        fail("version", "mc_version returned null; want 1.0.0");
    } else if (std::strcmp(version, "1.0.0") != 0) {
        fail("version", "got=" + std::string(version) + " want=1.0.0");
    }
}

void test_known_vectors() {
    expect_digest(
        "sha512 empty", nullptr, 0,
        "cf83e1357eefb8bdf1542850d66d8007d620e4050b5715dc83f4a921d36ce9ce47d0d13c5d85f2b0ff8318d2877eec2f63b931bd47417a81a538327af927da3e");

    static constexpr uint8_t abc[] = {'a', 'b', 'c'};
    expect_digest(
        "sha512 abc", abc, sizeof(abc),
        "ddaf35a193617abacc417349ae20413112e6fa4e89a97ea20a9eeee64b55d39a2192992a274fc1a836ba3c23a3feebbd454d4423643ce80e2a9ac94fa54ca49f");

    const std::vector<uint8_t> million_a(1000000, static_cast<uint8_t>('a'));
    expect_digest(
        "sha512 one million a", million_a.data(), million_a.size(),
        "e718483d0ce769644e2e42c7bc15b4638e1f98b13b2044285632a803afa973ebde0ff244877ea60a4cb0432ce577c31beb009c5c2c49aa2e4eadb217ad8cc09b");
}

void test_multi_update() {
    mc_sha512* ctx = mc_sha512_new();
    if (ctx == nullptr) {
        fail("multi-update", "mc_sha512_new returned null");
        return;
    }

    char errbuf[MC_ERRBUF_LEN];
    for (const uint8_t byte : std::array<uint8_t, 3>{'a', 'b', 'c'}) {
        const int rc = mc_sha512_update(ctx, &byte, 1, errbuf, sizeof(errbuf));
        if (rc != MC_OK) {
            fail("multi-update", "update rc=" + std::to_string(rc) + " err=" + errbuf);
            mc_sha512_free(ctx);
            return;
        }
    }

    std::array<uint8_t, MC_SHA512_BYTES> multi{};
    const int rc = mc_sha512_final(ctx, multi.data(), errbuf, sizeof(errbuf));
    mc_sha512_free(ctx);
    if (rc != MC_OK) {
        fail("multi-update", "final rc=" + std::to_string(rc) + " err=" + errbuf);
        return;
    }

    static constexpr uint8_t abc[] = {'a', 'b', 'c'};
    std::array<uint8_t, MC_SHA512_BYTES> one_shot{};
    if (!digest(abc, sizeof(abc), one_shot, "multi-update one-shot")) {
        return;
    }
    if (multi != one_shot) {
        fail("multi-update", "digest differs from one-shot abc: multi=" +
                                 hex_of(multi.data(), multi.size()) + " one-shot=" +
                                 hex_of(one_shot.data(), one_shot.size()));
    }
}

void test_argument_contracts() {
    char errbuf[MC_ERRBUF_LEN];
    uint8_t out[MC_SHA512_BYTES]{};

    int rc = mc_sha512_update(nullptr, nullptr, 0, errbuf, sizeof(errbuf));
    if (rc != MC_ERR_NULL_ARG) {
        fail("null update context", "rc=" + std::to_string(rc) +
                                        " want=" + std::to_string(MC_ERR_NULL_ARG));
    }

    rc = mc_sha512_final(nullptr, out, errbuf, sizeof(errbuf));
    if (rc != MC_ERR_NULL_ARG) {
        fail("null final context", "rc=" + std::to_string(rc) +
                                       " want=" + std::to_string(MC_ERR_NULL_ARG));
    }

    mc_sha512* ctx = mc_sha512_new();
    if (ctx == nullptr) {
        fail("argument contracts", "mc_sha512_new returned null");
        return;
    }

    rc = mc_sha512_update(ctx, nullptr, 0, errbuf, sizeof(errbuf));
    if (rc != MC_OK) {
        fail("empty null update", "rc=" + std::to_string(rc) +
                                      " want=" + std::to_string(MC_OK) + " err=" + errbuf);
    }

    char short_err[4] = {'X', 'X', 'X', 'X'};
    rc = mc_sha512_update(ctx, nullptr, 1, short_err, sizeof(short_err));
    if (rc != MC_ERR_NULL_ARG) {
        fail("nonzero null update", "rc=" + std::to_string(rc) +
                                        " want=" + std::to_string(MC_ERR_NULL_ARG));
    }
    if (short_err[sizeof(short_err) - 1] != '\0') {
        fail("nonzero null update", "error buffer is not NUL-terminated");
    }

    rc = mc_sha512_final(ctx, nullptr, errbuf, sizeof(errbuf));
    if (rc != MC_ERR_NULL_ARG) {
        fail("null final output", "rc=" + std::to_string(rc) +
                                      " want=" + std::to_string(MC_ERR_NULL_ARG));
    }

    rc = mc_sha512_final(ctx, out, errbuf, sizeof(errbuf));
    if (rc != MC_OK) {
        fail("first final", "rc=" + std::to_string(rc) +
                                " want=" + std::to_string(MC_OK) + " err=" + errbuf);
    }

    rc = mc_sha512_final(ctx, out, errbuf, sizeof(errbuf));
    if (rc != MC_ERR_INTERNAL) {
        fail("second final", "rc=" + std::to_string(rc) +
                                 " want=" + std::to_string(MC_ERR_INTERNAL));
    }
    mc_sha512_free(ctx);
    mc_sha512_free(nullptr);
}

}  // namespace

int main() {
    test_version();
    test_known_vectors();
    test_multi_update();
    test_argument_contracts();

    if (failures != 0) {
        std::fprintf(stderr, "%d SHA-512 test assertion(s) failed\n", failures);
        return 1;
    }
    std::printf("mediacore SHA-512 tests passed\n");
    return 0;
}
