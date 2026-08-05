#include "native_algorithms/phash_parts.h"

#include <algorithm>
#include <array>
#include <cmath>
#include <new>

namespace videocore::native {
namespace {

constexpr int kWorkSize = 96;
constexpr int kPartSize = 32;
constexpr int kDctSize = 8;
constexpr double kPi = 3.14159265358979323846;

struct CosTable {
    std::array<double, kPartSize * kDctSize> values{};
    CosTable() {
        for (int position = 0; position < kPartSize; ++position) {
            for (int frequency = 0; frequency < kDctSize; ++frequency) {
                values[position * kDctSize + frequency] = std::cos(
                    (2.0 * position + 1.0) * frequency * kPi /
                    (2.0 * kPartSize));
            }
        }
    }
};

const CosTable& cos_table() {
    static const CosTable table;
    return table;
}

void resize_to_96(const GrayImage& source, float* destination) {
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

uint64_t hash_part(
    const float* work,
    int part_row,
    int part_column,
    const CosTable& table) {
    std::array<float, kDctSize * kDctSize> coefficients{};
    const int base_y = part_row * kPartSize;
    const int base_x = part_column * kPartSize;
    for (int v = 0; v < kDctSize; ++v) {
        for (int u = 0; u < kDctSize; ++u) {
            double sum = 0.0;
            for (int y = 0; y < kPartSize; ++y) {
                const float* row = work +
                    (base_y + y) * kWorkSize + base_x;
                double row_sum = 0.0;
                for (int x = 0; x < kPartSize; ++x) {
                    row_sum += row[x] * table.values[x * kDctSize + u];
                }
                sum += row_sum * table.values[y * kDctSize + v];
            }
            const double normalize_u =
                u == 0 ? 1.0 / std::sqrt(2.0) : 1.0;
            const double normalize_v =
                v == 0 ? 1.0 / std::sqrt(2.0) : 1.0;
            coefficients[v * kDctSize + u] = static_cast<float>(
                0.25 * normalize_u * normalize_v * sum);
        }
    }
    auto ordered = coefficients;
    std::nth_element(
        ordered.begin(), ordered.begin() + ordered.size() / 2, ordered.end());
    const float median = ordered[ordered.size() / 2];
    uint64_t result = 0;
    for (size_t i = 0; i < coefficients.size(); ++i) {
        if (coefficients[i] > median) result |= UINT64_C(1) << i;
    }
    return result;
}

}  // namespace

ImageStatus ComputePHashParts(
    const GrayImage& image,
    std::array<uint64_t, 9>* out_parts) noexcept {
    if (out_parts != nullptr) out_parts->fill(0);
    if (out_parts == nullptr || !IsValidGrayImage(image)) {
        return ImageStatus::invalid_argument;
    }
    try {
        std::array<float, kWorkSize * kWorkSize> work{};
        std::array<uint64_t, 9> result{};
        resize_to_96(image, work.data());
        const CosTable& table = cos_table();
        for (int row = 0; row < 3; ++row) {
            for (int column = 0; column < 3; ++column) {
                result[row * 3 + column] =
                    hash_part(work.data(), row, column, table);
            }
        }
        *out_parts = result;
        return ImageStatus::ok;
    } catch (const std::bad_alloc&) {
        return ImageStatus::out_of_memory;
    } catch (...) {
        out_parts->fill(0);
        return ImageStatus::internal_error;
    }
}

}  // namespace videocore::native
