#ifndef MEDIACORE_H
#define MEDIACORE_H

#include <stddef.h>
#include <stdint.h>

#if defined(_WIN32)
#  if defined(MEDIACORE_BUILD)
#    define MC_API __declspec(dllexport)
#  else
#    define MC_API __declspec(dllimport)
#  endif
#else
#  define MC_API __attribute__((visibility("default")))
#endif

#define MC_VERSION_STRING "1.0.0"

#define MC_PDQ256_BYTES 32
#define MC_SHA512_BYTES 64
#define MC_ERRBUF_LEN 256
#define MC_PHASH_PARTS 9
#define MC_SOBEL_HIST_DIM 128

#define MC_OK 0
#define MC_ERR_NULL_ARG (-1)
#define MC_ERR_OOM (-2)
#define MC_ERR_DECODE (-3)
#define MC_ERR_SIZE (-4)
#define MC_ERR_INTERNAL (-99)

#ifdef __cplusplus
extern "C" {
#endif

MC_API const char* mc_version(void);

typedef struct mc_sha512 mc_sha512;
MC_API mc_sha512* mc_sha512_new(void);
MC_API void mc_sha512_free(mc_sha512* ctx);
MC_API int mc_sha512_update(mc_sha512* ctx, const uint8_t* data, size_t len,
                            char* errbuf, size_t errbuf_len);
MC_API int mc_sha512_final(mc_sha512* ctx, uint8_t out[MC_SHA512_BYTES],
                           char* errbuf, size_t errbuf_len);

typedef struct mc_image {
    int32_t width;
    int32_t height;
    uint8_t* gray;
} mc_image;

typedef struct mc_phase2_image_out {
    uint64_t phash_parts[MC_PHASH_PARTS];
    float sobel_hist[MC_SOBEL_HIST_DIM];
} mc_phase2_image_out;

MC_API int mc_decode_gray(const uint8_t* buf, size_t len, mc_image* out,
                          char* errbuf, size_t errbuf_len);
MC_API void mc_free_image(mc_image* img);

MC_API int mc_pdq256_from_gray(const uint8_t* gray, int32_t width, int32_t height,
                               uint8_t out_hash[MC_PDQ256_BYTES], int32_t* out_quality,
                               char* errbuf, size_t errbuf_len);
MC_API int32_t mc_hamming_distance(const uint8_t a[MC_PDQ256_BYTES],
                                   const uint8_t b[MC_PDQ256_BYTES]);

MC_API int mc_image_phase1(const uint8_t* buf, size_t len,
                           uint8_t out_hash[MC_PDQ256_BYTES], int32_t* out_quality,
                           int32_t* out_w, int32_t* out_h,
                           char* errbuf, size_t errbuf_len);

MC_API int mc_phash_parts(const mc_image* image,
                          uint64_t out_parts[MC_PHASH_PARTS],
                          char* errbuf, size_t errbuf_len);
MC_API int mc_sobel_hist(const mc_image* image,
                         float out_hist[MC_SOBEL_HIST_DIM],
                         char* errbuf, size_t errbuf_len);
MC_API int mc_phase2_image(const mc_image* image,
                           mc_phase2_image_out* out,
                           char* errbuf, size_t errbuf_len);

MC_API void mc_debug_crash(void);
MC_API void mc_debug_sleep_ms(uint32_t ms);

#ifdef __cplusplus
}
#endif

#endif
