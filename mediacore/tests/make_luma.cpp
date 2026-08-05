#include <cstdint>
#include <cstdio>
#include <filesystem>
#include <string>
#include <vector>

namespace {

uint32_t state = 0x12345678u;

uint32_t xorshift() {
    state ^= state << 13;
    state ^= state >> 17;
    state ^= state << 5;
    return state;
}

void write_luma(
    const std::filesystem::path& directory,
    const char* name,
    int width,
    int height,
    int pattern) {
    std::vector<uint8_t> gray(static_cast<size_t>(width) * height);
    for (int y = 0; y < height; ++y) {
        for (int x = 0; x < width; ++x) {
            uint8_t value = 0;
            switch (pattern) {
                case 0:
                    value = static_cast<uint8_t>(xorshift());
                    break;
                case 1:
                    value = static_cast<uint8_t>((x * 255) / (width > 1 ? width - 1 : 1));
                    break;
                case 2:
                    value = static_cast<uint8_t>((y * 255) / (height > 1 ? height - 1 : 1));
                    break;
                case 3:
                    value = 128;
                    break;
                case 4:
                    value = static_cast<uint8_t>(((x / 8) + (y / 8)) % 2 ? 255 : 0);
                    break;
                default:
                    value = static_cast<uint8_t>(x + y);
                    break;
            }
            gray[static_cast<size_t>(y) * width + x] = value;
        }
    }

    const std::filesystem::path path = directory / (std::string(name) + ".lumabin");
    FILE* file = nullptr;
    if (fopen_s(&file, path.string().c_str(), "wb") != 0 || file == nullptr) {
        std::fprintf(stderr, "cannot write %s\n", path.string().c_str());
        std::exit(1);
    }
    const int32_t dimensions[2] = {width, height};
    if (std::fwrite(dimensions, sizeof(int32_t), 2, file) != 2 ||
        std::fwrite(gray.data(), 1, gray.size(), file) != gray.size()) {
        std::fprintf(stderr, "short write %s\n", path.string().c_str());
        std::fclose(file);
        std::exit(1);
    }
    std::fclose(file);
}

}  // namespace

int main(int argc, char** argv) {
    if (argc != 2) {
        std::fprintf(stderr, "usage: mc_make_luma <outdir>\n");
        return 2;
    }
    const std::filesystem::path directory(argv[1]);
    std::filesystem::create_directories(directory);
    static constexpr int sizes[][2] = {
        {8, 8},
        {16, 16},
        {63, 65},
        {64, 64},
        {65, 63},
        {100, 100},
        {127, 255},
        {256, 256},
        {640, 480},
        {1920, 1080},
        {4096, 3072},
        {31, 2048},
    };
    int count = 0;
    for (const auto& size : sizes) {
        for (int pattern = 0; pattern < 6; ++pattern) {
            char name[128];
            std::snprintf(
                name,
                sizeof(name),
                "luma_%dx%d_p%d",
                size[0],
                size[1],
                pattern);
            write_luma(directory, name, size[0], size[1], pattern);
            ++count;
        }
    }
    std::printf("wrote %d luma vectors to %s\n", count, directory.string().c_str());
    return count == 72 ? 0 : 1;
}
