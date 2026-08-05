#include <mediacore/mediacore.h>

#include <algorithm>
#include <array>
#include <cmath>
#include <cstring>
#include <new>

namespace {

constexpr int kWorkSize = 128;
constexpr int kGridSize = 4;
constexpr int kBinCount = 8;
constexpr int kCellSize = kWorkSize / kGridSize;
constexpr double kPi = 3.14159265358979323846;

void clear_error(char* errbuf, size_t errbuf_len) noexcept {
    if (errbuf != nullptr && errbuf_len > 0) {
        errbuf[0] = '\0';
    }
}

void set_error(char* errbuf, size_t errbuf_len, const char* message) noexcept {
    if (errbuf == nullptr || errbuf_len == 0) {
        return;
    }
    size_t i = 0;
    while (i + 1 < errbuf_len && message[i] != '\0') {
        errbuf[i] = message[i];
        ++i;
    }
    errbuf[i] = '\0';
}

void resize_to_128(const mc_image& source, float* destination) {
    const double scale_x = static_cast<double>(source.width) / kWorkSize;
    const double scale_y = static_cast<double>(source.height) / kWorkSize;
    for (int y = 0; y < kWorkSize; ++y) {
        const double source_y = (y + 0.5) * scale_y - 0.5;
        int y0 = static_cast<int>(std::floor(source_y));
        double weight_y = source_y - y0;
        if (y0 < 0) {
            y0 = 0;
            weight_y = 0.0;
        }
        const int y1 = (std::min)(y0 + 1, source.height - 1);
        const uint8_t* row0 =
            source.gray + static_cast<size_t>(y0) * source.width;
        const uint8_t* row1 =
            source.gray + static_cast<size_t>(y1) * source.width;
        for (int x = 0; x < kWorkSize; ++x) {
            const double source_x = (x + 0.5) * scale_x - 0.5;
            int x0 = static_cast<int>(std::floor(source_x));
            double weight_x = source_x - x0;
            if (x0 < 0) {
                x0 = 0;
                weight_x = 0.0;
            }
            const int x1 = (std::min)(x0 + 1, source.width - 1);
            const double top =
                row0[x0] + (row0[x1] - row0[x0]) * weight_x;
            const double bottom =
                row1[x0] + (row1[x1] - row1[x0]) * weight_x;
            destination[y * kWorkSize + x] =
                static_cast<float>(top + (bottom - top) * weight_y);
        }
    }
}

bool histogram_is_finite(
    const std::array<float, MC_SOBEL_HIST_DIM>& histogram) noexcept {
    for (float value : histogram) {
        if (!std::isfinite(value)) {
            return false;
        }
    }
    return true;
}

}  // namespace

extern "C" MC_API int mc_sobel_hist(
    const mc_image* image,
    float out_hist[MC_SOBEL_HIST_DIM],
    char* errbuf,
    size_t errbuf_len) {
    clear_error(errbuf, errbuf_len);
    if (out_hist != nullptr) {
        std::memset(out_hist, 0, sizeof(float) * MC_SOBEL_HIST_DIM);
    }
    try {
        if (image == nullptr || image->gray == nullptr || out_hist == nullptr) {
            set_error(errbuf, errbuf_len, "phase-2 Sobel input or output is null");
            return MC_ERR_NULL_ARG;
        }
        if (image->width < 8 || image->height < 8) {
            set_error(errbuf, errbuf_len, "phase-2 Sobel image is smaller than 8x8");
            return MC_ERR_SIZE;
        }

        std::array<float, kWorkSize * kWorkSize> work{};
        std::array<float, MC_SOBEL_HIST_DIM> histogram{};
        resize_to_128(*image, work.data());

        for (int y = 1; y < kWorkSize - 1; ++y) {
            for (int x = 1; x < kWorkSize - 1; ++x) {
                const float top_left = work[(y - 1) * kWorkSize + x - 1];
                const float top_center = work[(y - 1) * kWorkSize + x];
                const float top_right = work[(y - 1) * kWorkSize + x + 1];
                const float middle_left = work[y * kWorkSize + x - 1];
                const float middle_right = work[y * kWorkSize + x + 1];
                const float bottom_left = work[(y + 1) * kWorkSize + x - 1];
                const float bottom_center = work[(y + 1) * kWorkSize + x];
                const float bottom_right = work[(y + 1) * kWorkSize + x + 1];

                const float gradient_x =
                    (top_right + 2.0f * middle_right + bottom_right) -
                    (top_left + 2.0f * middle_left + bottom_left);
                const float gradient_y =
                    (bottom_left + 2.0f * bottom_center + bottom_right) -
                    (top_left + 2.0f * top_center + top_right);
                const float magnitude =
                    std::fabs(gradient_x) + std::fabs(gradient_y);
                if (magnitude < 1e-6f) {
                    continue;
                }

                double orientation =
                    std::atan2(static_cast<double>(gradient_y),
                               static_cast<double>(gradient_x));
                if (orientation < 0.0) {
                    orientation += kPi;
                }
                if (orientation >= kPi) {
                    orientation -= kPi;
                }
                int bin =
                    static_cast<int>(orientation / kPi * kBinCount);
                if (bin >= kBinCount) {
                    bin = kBinCount - 1;
                }
                const int cell_y = y / kCellSize;
                const int cell_x = x / kCellSize;
                histogram[(cell_y * kGridSize + cell_x) * kBinCount + bin] +=
                    magnitude;
            }
        }

        double squared_norm = 0.0;
        for (float value : histogram) {
            squared_norm += static_cast<double>(value) * value;
        }
        const double histogram_norm = std::sqrt(squared_norm);
        if (histogram_norm > 1e-9) {
            for (float& value : histogram) {
                value = static_cast<float>(value / histogram_norm);
            }
        }
        if (!histogram_is_finite(histogram)) {
            set_error(errbuf, errbuf_len, "phase-2 Sobel produced a non-finite value");
            return MC_ERR_INTERNAL;
        }
        std::memcpy(out_hist, histogram.data(), sizeof(histogram));
        return MC_OK;
    } catch (const std::bad_alloc&) {
        set_error(errbuf, errbuf_len, "out of memory computing phase-2 Sobel");
        return MC_ERR_OOM;
    } catch (...) {
        set_error(errbuf, errbuf_len, "unexpected phase-2 Sobel failure");
        return MC_ERR_INTERNAL;
    }
}

extern "C" MC_API int mc_phase2_image(
    const mc_image* image,
    mc_phase2_image_out* out,
    char* errbuf,
    size_t errbuf_len) {
    clear_error(errbuf, errbuf_len);
    if (out != nullptr) {
        std::memset(out, 0, sizeof(*out));
    }
    try {
        if (out == nullptr) {
            set_error(errbuf, errbuf_len, "phase-2 combined output is null");
            return MC_ERR_NULL_ARG;
        }
        mc_phase2_image_out result{};
        int rc = mc_phash_parts(
            image, result.phash_parts, errbuf, errbuf_len);
        if (rc != MC_OK) {
            return rc;
        }
        rc = mc_sobel_hist(
            image, result.sobel_hist, errbuf, errbuf_len);
        if (rc != MC_OK) {
            return rc;
        }
        std::memcpy(out, &result, sizeof(result));
        return MC_OK;
    } catch (const std::bad_alloc&) {
        set_error(errbuf, errbuf_len, "out of memory computing phase-2 image");
        return MC_ERR_OOM;
    } catch (...) {
        set_error(errbuf, errbuf_len, "unexpected phase-2 image failure");
        return MC_ERR_INTERNAL;
    }
}
