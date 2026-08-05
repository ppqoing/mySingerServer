#include "native_algorithms/sobel_hist.h"

#include <algorithm>
#include <array>
#include <cmath>
#include <new>

namespace videocore::native {
namespace {

constexpr int kWorkSize = 128;
constexpr int kGridSize = 4;
constexpr int kBinCount = 8;
constexpr int kCellSize = kWorkSize / kGridSize;
constexpr double kPi = 3.14159265358979323846;

void resize_to_128(const GrayImage& source, float* destination) {
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
        const uint8_t* row0 = source.pixels.data() +
            static_cast<size_t>(y0) * source.stride;
        const uint8_t* row1 = source.pixels.data() +
            static_cast<size_t>(y1) * source.stride;
        for (int x = 0; x < kWorkSize; ++x) {
            const double source_x = (x + 0.5) * scale_x - 0.5;
            int x0 = static_cast<int>(std::floor(source_x));
            double weight_x = source_x - x0;
            if (x0 < 0) {
                x0 = 0;
                weight_x = 0.0;
            }
            const int x1 = (std::min)(x0 + 1, source.width - 1);
            const double top = row0[x0] + (row0[x1] - row0[x0]) * weight_x;
            const double bottom = row1[x0] +
                (row1[x1] - row1[x0]) * weight_x;
            destination[y * kWorkSize + x] =
                static_cast<float>(top + (bottom - top) * weight_y);
        }
    }
}

}  // namespace

ImageStatus ComputeSobelHistogram(
    const GrayImage& image,
    std::array<float, 128>* out_histogram) noexcept {
    if (out_histogram != nullptr) out_histogram->fill(0.0f);
    if (out_histogram == nullptr || !IsValidGrayImage(image)) {
        return ImageStatus::invalid_argument;
    }
    try {
        std::array<float, kWorkSize * kWorkSize> work{};
        std::array<float, 128> histogram{};
        resize_to_128(image, work.data());
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
                if (magnitude < 1e-6f) continue;
                double orientation = std::atan2(
                    static_cast<double>(gradient_y),
                    static_cast<double>(gradient_x));
                if (orientation < 0.0) orientation += kPi;
                if (orientation >= kPi) orientation -= kPi;
                int bin = static_cast<int>(orientation / kPi * kBinCount);
                if (bin >= kBinCount) bin = kBinCount - 1;
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
        const double norm = std::sqrt(squared_norm);
        if (norm > 1e-9) {
            for (float& value : histogram) {
                value = static_cast<float>(value / norm);
            }
        }
        for (float value : histogram) {
            if (!std::isfinite(value)) return ImageStatus::internal_error;
        }
        *out_histogram = histogram;
        return ImageStatus::ok;
    } catch (const std::bad_alloc&) {
        return ImageStatus::out_of_memory;
    } catch (...) {
        out_histogram->fill(0.0f);
        return ImageStatus::internal_error;
    }
}

}  // namespace videocore::native
