#define STB_IMAGE_IMPLEMENTATION
#define STBI_FAILURE_USERMSG
#define STBI_NO_STDIO
#define STBI_NO_HDR
#define STBI_NO_PIC
#define STBI_NO_PSD
#include <stb/stb_image.h>

#include <cstdint>

bool mc_stb_info(const uint8_t* buf, int len, int* width, int* height, const char** reason) {
    int channels = 0;
    if (stbi_info_from_memory(buf, len, width, height, &channels) == 0) {
        if (reason != nullptr) {
            *reason = stbi_failure_reason();
        }
        return false;
    }
    return true;
}

uint8_t* mc_stb_load_rgb(
    const uint8_t* buf,
    int len,
    int* width,
    int* height,
    const char** reason) {
    int channels = 0;
    stbi_uc* image = stbi_load_from_memory(buf, len, width, height, &channels, 3);
    if (image == nullptr && reason != nullptr) {
        *reason = stbi_failure_reason();
    }
    return image;
}

void mc_stb_free(void* image) {
    stbi_image_free(image);
}
