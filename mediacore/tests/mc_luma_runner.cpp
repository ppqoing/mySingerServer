#include <mediacore/mediacore.h>

#include <cstdint>
#include <cstdio>
#include <limits>
#include <vector>

int main(int argc, char** argv) {
    if (argc != 2) {
        std::fprintf(stderr, "usage: mc_luma_runner <file.lumabin>\n");
        return 2;
    }
    FILE* file = nullptr;
    if (fopen_s(&file, argv[1], "rb") != 0 || file == nullptr) {
        return 2;
    }
    int32_t dimensions[2]{};
    if (std::fread(dimensions, sizeof(int32_t), 2, file) != 2 ||
        dimensions[0] <= 0 || dimensions[1] <= 0 ||
        dimensions[0] > 400000000LL / dimensions[1]) {
        std::fclose(file);
        return 2;
    }
    const size_t pixels =
        static_cast<size_t>(dimensions[0]) * static_cast<size_t>(dimensions[1]);
    std::vector<uint8_t> gray(pixels);
    if (std::fread(gray.data(), 1, gray.size(), file) != gray.size() ||
        std::fgetc(file) != EOF) {
        std::fclose(file);
        return 2;
    }
    std::fclose(file);

    uint8_t hash[MC_PDQ256_BYTES]{};
    int32_t quality = 0;
    char error[MC_ERRBUF_LEN];
    const int rc = mc_pdq256_from_gray(
        gray.data(),
        dimensions[0],
        dimensions[1],
        hash,
        &quality,
        error,
        sizeof(error));
    if (rc != MC_OK) {
        std::fprintf(stderr, "PDQ failed: %s\n", error);
        return 1;
    }
    for (uint8_t byte : hash) {
        std::printf("%02x", byte);
    }
    std::printf(" %d\n", quality);
    return 0;
}
