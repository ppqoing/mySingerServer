#include <mediacore/mediacore.h>

#include <cstdint>
#include <cstdio>
#include <cstring>
#include <string>
#include <vector>

namespace {

bool read_all(const char* path, std::vector<uint8_t>& bytes) {
    FILE* file = nullptr;
    if (fopen_s(&file, path, "rb") != 0 || file == nullptr) {
        return false;
    }
    if (std::fseek(file, 0, SEEK_END) != 0) {
        std::fclose(file);
        return false;
    }
    const long length = std::ftell(file);
    if (length < 0 || std::fseek(file, 0, SEEK_SET) != 0) {
        std::fclose(file);
        return false;
    }
    bytes.resize(static_cast<size_t>(length));
    const bool ok = bytes.empty() ||
        std::fread(bytes.data(), 1, bytes.size(), file) == bytes.size();
    std::fclose(file);
    return ok;
}

int hex_value(char value) {
    if (value >= '0' && value <= '9') {
        return value - '0';
    }
    if (value >= 'a' && value <= 'f') {
        return value - 'a' + 10;
    }
    if (value >= 'A' && value <= 'F') {
        return value - 'A' + 10;
    }
    return -1;
}

bool from_hex(const char* text, uint8_t output[MC_PDQ256_BYTES]) {
    if (std::strlen(text) != MC_PDQ256_BYTES * 2) {
        return false;
    }
    for (size_t i = 0; i < MC_PDQ256_BYTES; ++i) {
        const int high = hex_value(text[i * 2]);
        const int low = hex_value(text[i * 2 + 1]);
        if (high < 0 || low < 0) {
            return false;
        }
        output[i] = static_cast<uint8_t>((high << 4) | low);
    }
    return true;
}

int sha512(const uint8_t* data, size_t length, uint8_t output[MC_SHA512_BYTES]) {
    mc_sha512* context = mc_sha512_new();
    if (context == nullptr) {
        return MC_ERR_INTERNAL;
    }
    char error[MC_ERRBUF_LEN];
    size_t offset = 0;
    while (offset < length) {
        const size_t chunk = (std::min)(length - offset, static_cast<size_t>(4u << 20));
        const int rc = mc_sha512_update(
            context,
            data + offset,
            chunk,
            error,
            sizeof(error));
        if (rc != MC_OK) {
            mc_sha512_free(context);
            return rc;
        }
        offset += chunk;
    }
    const int rc = mc_sha512_final(context, output, error, sizeof(error));
    mc_sha512_free(context);
    return rc;
}

void print_hex(const uint8_t* bytes, size_t length) {
    for (size_t i = 0; i < length; ++i) {
        std::printf("%02x", bytes[i]);
    }
}

}  // namespace

int main(int argc, char** argv) {
    if (argc == 3 && std::strcmp(argv[1], "hash") == 0) {
        std::vector<uint8_t> bytes;
        if (!read_all(argv[2], bytes)) {
            std::fprintf(stderr, "cannot read %s\n", argv[2]);
            return 2;
        }
        uint8_t hash[MC_PDQ256_BYTES]{};
        int32_t quality = 0;
        int32_t width = 0;
        int32_t height = 0;
        char error[MC_ERRBUF_LEN];
        const int rc = mc_image_phase1(
            bytes.data(),
            bytes.size(),
            hash,
            &quality,
            &width,
            &height,
            error,
            sizeof(error));
        if (rc != MC_OK) {
            std::fprintf(stderr, "decode/PDQ failed: %s\n", error);
            return 1;
        }
        print_hex(hash, sizeof(hash));
        std::printf(" %d %d %d\n", quality, width, height);
        return 0;
    }
    if (argc == 4 && std::strcmp(argv[1], "hd") == 0) {
        uint8_t left[MC_PDQ256_BYTES]{};
        uint8_t right[MC_PDQ256_BYTES]{};
        if (!from_hex(argv[2], left) || !from_hex(argv[3], right)) {
            return 2;
        }
        std::printf("%d\n", mc_hamming_distance(left, right));
        return 0;
    }
    if (argc == 3 && std::strcmp(argv[1], "sha512str") == 0) {
        uint8_t output[MC_SHA512_BYTES]{};
        if (sha512(
                reinterpret_cast<const uint8_t*>(argv[2]),
                std::strlen(argv[2]),
                output) != MC_OK) {
            return 1;
        }
        print_hex(output, sizeof(output));
        std::putchar('\n');
        return 0;
    }
    if (argc == 3 && std::strcmp(argv[1], "sha512file") == 0) {
        std::vector<uint8_t> bytes;
        if (!read_all(argv[2], bytes)) {
            return 2;
        }
        uint8_t output[MC_SHA512_BYTES]{};
        if (sha512(bytes.data(), bytes.size(), output) != MC_OK) {
            return 1;
        }
        print_hex(output, sizeof(output));
        std::putchar('\n');
        return 0;
    }
    std::fprintf(
        stderr,
        "usage: mc_endtoend hash <image> | hd <hex> <hex> | sha512str <s> | sha512file <file>\n");
    return 2;
}
