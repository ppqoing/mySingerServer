#define STB_IMAGE_IMPLEMENTATION
#define STBI_FAILURE_USERMSG
#define STBI_NO_STDIO
#define STBI_NO_HDR
#define STBI_NO_PIC
#define STBI_NO_PSD
#include <stb/stb_image.h>

#include <cstdint>

namespace videocore::native::stb {

bool Info(
    const uint8_t* buffer,
    int length,
    int* width,
    int* height) noexcept {
    int channels = 0;
    return stbi_info_from_memory(
        buffer, length, width, height, &channels) != 0;
}

uint8_t* LoadRgb(
    const uint8_t* buffer,
    int length,
    int* width,
    int* height) noexcept {
    int channels = 0;
    return stbi_load_from_memory(
        buffer, length, width, height, &channels, 3);
}

void Free(void* image) noexcept {
    stbi_image_free(image);
}

}  // namespace videocore::native::stb
