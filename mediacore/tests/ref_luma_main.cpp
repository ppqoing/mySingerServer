#include <pdq/cpp/hashing/pdqhashing.h>

#include <cstdint>
#include <cstdio>
#include <vector>

int main(int argc, char** argv) {
    if (argc != 2) {
        std::fprintf(stderr, "usage: ref_luma_hasher <file.lumabin>\n");
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
    const int columns = dimensions[0];
    const int rows = dimensions[1];
    std::vector<uint8_t> gray(static_cast<size_t>(columns) * rows);
    if (std::fread(gray.data(), 1, gray.size(), file) != gray.size() ||
        std::fgetc(file) != EOF) {
        std::fclose(file);
        return 2;
    }
    std::fclose(file);

    using namespace facebook::pdq::hashing;
    std::vector<float> full_buffer_1(gray.size());
    std::vector<float> full_buffer_2(gray.size());
    fillFloatLumaFromGrey(
        gray.data(),
        rows,
        columns,
        columns,
        1,
        full_buffer_1.data());
    float buffer_64x64[64][64];
    float buffer_16x64[16][64];
    float buffer_16x16[16][16];
    Hash256 hash;
    int quality = 0;
    pdqHash256FromFloatLuma(
        full_buffer_1.data(),
        full_buffer_2.data(),
        rows,
        columns,
        buffer_64x64,
        buffer_16x64,
        buffer_16x16,
        hash,
        quality);
    std::printf("%s %d\n", hash.format().c_str(), quality);
    return 0;
}
