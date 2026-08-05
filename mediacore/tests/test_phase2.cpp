#include <mediacore/mediacore.h>

#include <algorithm>
#include <array>
#include <cmath>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <string>
#include <vector>

namespace {

int failures = 0;

void fail(const char* test, const std::string& detail) {
    std::fprintf(stderr, "FAIL %s: %s\n", test, detail.c_str());
    ++failures;
}

void expect(bool condition, const char* test, const char* detail) {
    if (!condition) {
        fail(test, detail);
    }
}

mc_image wrap(std::vector<uint8_t>& pixels, int32_t width, int32_t height) {
    return mc_image{width, height, pixels.data()};
}

std::vector<uint8_t> make_structured(int width, int height) {
    std::vector<uint8_t> pixels(static_cast<size_t>(width) * height);
    for (int y = 0; y < height; ++y) {
        for (int x = 0; x < width; ++x) {
            const int wave = static_cast<int>(
                42.0 * std::sin(x * 0.13) + 31.0 * std::cos(y * 0.09));
            const int blocks = ((x / 19 + y / 13) & 1) != 0 ? 24 : -24;
            pixels[static_cast<size_t>(y) * width + x] =
                static_cast<uint8_t>((std::clamp)(128 + wave + blocks, 0, 255));
        }
    }
    return pixels;
}

enum class Structure {
    Horizontal,
    Vertical,
    Checker,
};

std::vector<uint8_t> make_unrelated(int width, int height, Structure structure) {
    std::vector<uint8_t> pixels(static_cast<size_t>(width) * height);
    for (int y = 0; y < height; ++y) {
        for (int x = 0; x < width; ++x) {
            uint8_t value = 0;
            if (structure == Structure::Horizontal) {
                value = static_cast<uint8_t>(((y / 8) & 1) != 0 ? 224 : 24);
            } else if (structure == Structure::Vertical) {
                value = static_cast<uint8_t>(((x / 8) & 1) != 0 ? 224 : 24);
            } else {
                value = static_cast<uint8_t>(((x / 8 + y / 8) & 1) != 0 ? 224 : 24);
            }
            pixels[static_cast<size_t>(y) * width + x] = value;
        }
    }
    return pixels;
}

std::vector<uint8_t> make_vertical_step(bool inverted) {
    constexpr int width = 128;
    constexpr int height = 128;
    std::vector<uint8_t> pixels(width * height);
    for (int y = 0; y < height; ++y) {
        for (int x = 0; x < width; ++x) {
            const bool high_side = x >= 64;
            const bool high = inverted ? !high_side : high_side;
            pixels[static_cast<size_t>(y) * width + x] =
                static_cast<uint8_t>(high ? 224 : 24);
        }
    }
    return pixels;
}

std::vector<uint8_t> make_horizontal_step(bool inverted) {
    constexpr int width = 128;
    constexpr int height = 128;
    std::vector<uint8_t> pixels(width * height);
    for (int y = 0; y < height; ++y) {
        for (int x = 0; x < width; ++x) {
            const bool high_side = y >= 64;
            const bool high = inverted ? !high_side : high_side;
            pixels[static_cast<size_t>(y) * width + x] =
                static_cast<uint8_t>(high ? 224 : 24);
        }
    }
    return pixels;
}

std::array<uint64_t, MC_PHASH_PARTS> reference_phash(const mc_image& image) {
    // Independent, deliberately slow specification oracle:
    // 1. Pixel-center bilinear interpolation is evaluated directly.
    // 2. Every DCT coefficient uses the literal four-loop 2-D formula rather
    //    than production's cached/separable row accumulation.
    // 3. The explicit destination index is v*8+u, so transposed bit layouts
    //    cannot agree accidentally.
    constexpr int work_size = 96;
    constexpr int part_size = 32;
    constexpr int dct_size = 8;
    constexpr double pi = 3.14159265358979323846;
    std::array<float, work_size * work_size> resized{};
    for (int destination_y = 0; destination_y < work_size; ++destination_y) {
        double source_y =
            (destination_y + 0.5) * image.height / work_size - 0.5;
        int y0 = static_cast<int>(std::floor(source_y));
        double y_fraction = source_y - y0;
        if (y0 < 0) {
            y0 = 0;
            y_fraction = 0.0;
        }
        const int y1 = (std::min)(y0 + 1, image.height - 1);
        for (int destination_x = 0; destination_x < work_size; ++destination_x) {
            double source_x =
                (destination_x + 0.5) * image.width / work_size - 0.5;
            int x0 = static_cast<int>(std::floor(source_x));
            double x_fraction = source_x - x0;
            if (x0 < 0) {
                x0 = 0;
                x_fraction = 0.0;
            }
            const int x1 = (std::min)(x0 + 1, image.width - 1);
            const double top_left =
                image.gray[static_cast<size_t>(y0) * image.width + x0];
            const double top_right =
                image.gray[static_cast<size_t>(y0) * image.width + x1];
            const double bottom_left =
                image.gray[static_cast<size_t>(y1) * image.width + x0];
            const double bottom_right =
                image.gray[static_cast<size_t>(y1) * image.width + x1];
            resized[destination_y * work_size + destination_x] =
                static_cast<float>(
                    top_left * (1.0 - x_fraction) * (1.0 - y_fraction) +
                    top_right * x_fraction * (1.0 - y_fraction) +
                    bottom_left * (1.0 - x_fraction) * y_fraction +
                    bottom_right * x_fraction * y_fraction);
        }
    }

    std::array<uint64_t, MC_PHASH_PARTS> result{};
    for (int part_row = 0; part_row < 3; ++part_row) {
        for (int part_column = 0; part_column < 3; ++part_column) {
            std::array<float, dct_size * dct_size> coefficients{};
            for (int v = 0; v < dct_size; ++v) {
                for (int u = 0; u < dct_size; ++u) {
                    double sum = 0.0;
                    for (int y = 0; y < part_size; ++y) {
                        for (int x = 0; x < part_size; ++x) {
                            const float pixel =
                                resized[(part_row * part_size + y) * work_size +
                                        part_column * part_size + x];
                            sum += pixel *
                                std::cos((2.0 * x + 1.0) * u * pi / 64.0) *
                                std::cos((2.0 * y + 1.0) * v * pi / 64.0);
                        }
                    }
                    const double normalize_u =
                        u == 0 ? 1.0 / std::sqrt(2.0) : 1.0;
                    const double normalize_v =
                        v == 0 ? 1.0 / std::sqrt(2.0) : 1.0;
                    coefficients[v * dct_size + u] =
                        static_cast<float>(
                            0.25 * normalize_u * normalize_v * sum);
                }
            }
            auto ordered = coefficients;
            std::nth_element(
                ordered.begin(),
                ordered.begin() + ordered.size() / 2,
                ordered.end());
            const float median = ordered[ordered.size() / 2];
            uint64_t hash = 0;
            for (int v = 0; v < dct_size; ++v) {
                for (int u = 0; u < dct_size; ++u) {
                    const int bit = v * dct_size + u;
                    if (coefficients[bit] > median) {
                        hash |= UINT64_C(1) << bit;
                    }
                }
            }
            result[part_row * 3 + part_column] = hash;
        }
    }
    return result;
}

std::array<float, MC_SOBEL_HIST_DIM> vertical_step_golden() {
    // Identity resize. The edge affects x=63 (cell column 1) and x=64
    // (cell column 2). Interior y counts per cell row are 31,32,32,31.
    // Both contrast directions are the same unsigned 0-degree orientation,
    // therefore every nonzero entry is bin 0. The common magnitude cancels.
    constexpr std::array<int, 4> counts{31, 32, 32, 31};
    const double denominator = std::sqrt(7940.0);
    std::array<float, MC_SOBEL_HIST_DIM> expected{};
    for (int cell_y = 0; cell_y < 4; ++cell_y) {
        const float value =
            static_cast<float>(counts[cell_y] / denominator);
        expected[(cell_y * 4 + 1) * 8] = value;
        expected[(cell_y * 4 + 2) * 8] = value;
    }
    return expected;
}

std::array<float, MC_SOBEL_HIST_DIM> horizontal_step_golden() {
    // Identity resize. The edge affects y=63 (cell row 1) and y=64
    // (cell row 2). Interior x counts per cell column are 31,32,32,31.
    // Both contrast directions have unsigned orientation pi/2, which is bin 4.
    constexpr std::array<int, 4> counts{31, 32, 32, 31};
    const double denominator = std::sqrt(7940.0);
    std::array<float, MC_SOBEL_HIST_DIM> expected{};
    for (int cell_x = 0; cell_x < 4; ++cell_x) {
        const float value =
            static_cast<float>(counts[cell_x] / denominator);
        expected[(1 * 4 + cell_x) * 8 + 4] = value;
        expected[(2 * 4 + cell_x) * 8 + 4] = value;
    }
    return expected;
}

void expect_hist_near(
    const float actual[MC_SOBEL_HIST_DIM],
    const std::array<float, MC_SOBEL_HIST_DIM>& expected,
    const char* test,
    const char* detail) {
    for (int i = 0; i < MC_SOBEL_HIST_DIM; ++i) {
        if (std::fabs(actual[i] - expected[i]) > 1e-6f) {
            fail(
                test,
                std::string(detail) + " index=" + std::to_string(i) +
                    " got=" + std::to_string(actual[i]) +
                    " want=" + std::to_string(expected[i]));
            return;
        }
    }
}

int popcount64(uint64_t value) {
    int count = 0;
    while (value != 0) {
        value &= value - 1;
        ++count;
    }
    return count;
}

int passing_parts(
    const uint64_t a[MC_PHASH_PARTS],
    const uint64_t b[MC_PHASH_PARTS]) {
    int passing = 0;
    for (int i = 0; i < MC_PHASH_PARTS; ++i) {
        if (popcount64(a[i] ^ b[i]) <= 10) {
            ++passing;
        }
    }
    return passing;
}

double norm(const float values[MC_SOBEL_HIST_DIM]) {
    double sum = 0.0;
    for (int i = 0; i < MC_SOBEL_HIST_DIM; ++i) {
        sum += static_cast<double>(values[i]) * values[i];
    }
    return std::sqrt(sum);
}

double cosine(
    const float a[MC_SOBEL_HIST_DIM],
    const float b[MC_SOBEL_HIST_DIM]) {
    double dot = 0.0;
    for (int i = 0; i < MC_SOBEL_HIST_DIM; ++i) {
        dot += static_cast<double>(a[i]) * b[i];
    }
    const double denominator = norm(a) * norm(b);
    return denominator == 0.0 ? 0.0 : dot / denominator;
}

bool all_zero(const uint64_t values[MC_PHASH_PARTS]) {
    for (int i = 0; i < MC_PHASH_PARTS; ++i) {
        if (values[i] != 0) {
            return false;
        }
    }
    return true;
}

bool all_zero(const float values[MC_SOBEL_HIST_DIM]) {
    for (int i = 0; i < MC_SOBEL_HIST_DIM; ++i) {
        if (values[i] != 0.0f) {
            return false;
        }
    }
    return true;
}

bool finite(const float values[MC_SOBEL_HIST_DIM]) {
    for (int i = 0; i < MC_SOBEL_HIST_DIM; ++i) {
        if (!std::isfinite(values[i])) {
            return false;
        }
    }
    return true;
}

void test_deterministic_and_input_unchanged() {
    constexpr char test[] = "deterministic-and-input-unchanged";
    auto pixels = make_structured(211, 173);
    mc_image image = wrap(pixels, 211, 173);
    const int32_t original_width = image.width;
    const int32_t original_height = image.height;
    uint8_t* const original_gray = image.gray;

    mc_phase2_image_out first{};
    mc_phase2_image_out second{};
    char error[MC_ERRBUF_LEN];
    int rc = mc_phase2_image(&image, &first, error, sizeof(error));
    expect(rc == MC_OK, test, "first combined call succeeds");
    rc = mc_phase2_image(&image, &second, error, sizeof(error));
    expect(rc == MC_OK, test, "second combined call succeeds");
    expect(std::memcmp(&first, &second, sizeof(first)) == 0,
           test, "repeated combined outputs are byte-identical");
    expect(image.width == original_width && image.height == original_height &&
               image.gray == original_gray,
           test, "combined call does not mutate the input image descriptor");

    std::array<uint64_t, MC_PHASH_PARTS> direct_parts{};
    std::array<float, MC_SOBEL_HIST_DIM> direct_hist{};
    expect(mc_phash_parts(&image, direct_parts.data(), error, sizeof(error)) == MC_OK,
           test, "direct pHash succeeds");
    expect(mc_sobel_hist(&image, direct_hist.data(), error, sizeof(error)) == MC_OK,
           test, "direct Sobel succeeds");
    expect(std::memcmp(direct_parts.data(), first.phash_parts, sizeof(first.phash_parts)) == 0,
           test, "direct pHash exactly equals combined output");
    expect(std::memcmp(direct_hist.data(), first.sobel_hist, sizeof(first.sobel_hist)) == 0,
           test, "direct Sobel exactly equals combined output");
    expect(image.width == original_width && image.height == original_height &&
               image.gray == original_gray,
           test, "direct primitives do not mutate the input image descriptor");
}

void test_argument_contracts_and_output_clearing() {
    constexpr char test[] = "argument-contracts-and-output-clearing";
    char error[MC_ERRBUF_LEN];
    std::array<uint64_t, MC_PHASH_PARTS> parts;
    std::array<float, MC_SOBEL_HIST_DIM> hist;
    parts.fill(UINT64_MAX);
    hist.fill(1.0f);

    expect(mc_phash_parts(nullptr, parts.data(), error, sizeof(error)) == MC_ERR_NULL_ARG,
           test, "pHash rejects null image");
    expect(all_zero(parts.data()), test, "pHash clears output for null image");
    expect(mc_sobel_hist(nullptr, hist.data(), error, sizeof(error)) == MC_ERR_NULL_ARG,
           test, "Sobel rejects null image");
    expect(all_zero(hist.data()), test, "Sobel clears output for null image");

    mc_image null_gray{8, 8, nullptr};
    parts.fill(UINT64_MAX);
    hist.fill(1.0f);
    expect(mc_phash_parts(&null_gray, parts.data(), error, sizeof(error)) == MC_ERR_NULL_ARG,
           test, "pHash rejects null gray plane");
    expect(all_zero(parts.data()), test, "pHash clears output for null gray plane");
    expect(mc_sobel_hist(&null_gray, hist.data(), error, sizeof(error)) == MC_ERR_NULL_ARG,
           test, "Sobel rejects null gray plane");
    expect(all_zero(hist.data()), test, "Sobel clears output for null gray plane");

    auto pixels = make_structured(8, 8);
    mc_image valid = wrap(pixels, 8, 8);
    expect(mc_phash_parts(&valid, nullptr, error, sizeof(error)) == MC_ERR_NULL_ARG,
           test, "pHash rejects null output");
    expect(mc_sobel_hist(&valid, nullptr, error, sizeof(error)) == MC_ERR_NULL_ARG,
           test, "Sobel rejects null output");
    expect(mc_phase2_image(&valid, nullptr, error, sizeof(error)) == MC_ERR_NULL_ARG,
           test, "combined call rejects null output");

    for (const std::array<int32_t, 2> dimensions :
         {std::array<int32_t, 2>{7, 8}, std::array<int32_t, 2>{8, 7}}) {
        auto small_pixels =
            make_structured(dimensions[0], dimensions[1]);
        mc_image small = wrap(small_pixels, dimensions[0], dimensions[1]);
        parts.fill(UINT64_MAX);
        hist.fill(1.0f);
        expect(mc_phash_parts(&small, parts.data(), error, sizeof(error)) == MC_ERR_SIZE,
               test, "pHash rejects a dimension below eight");
        expect(all_zero(parts.data()), test, "pHash clears output for undersized input");
        expect(mc_sobel_hist(&small, hist.data(), error, sizeof(error)) == MC_ERR_SIZE,
               test, "Sobel rejects a dimension below eight");
        expect(all_zero(hist.data()), test, "Sobel clears output for undersized input");

        mc_phase2_image_out combined;
        std::memset(&combined, 0xff, sizeof(combined));
        expect(mc_phase2_image(&small, &combined, error, sizeof(error)) == MC_ERR_SIZE,
               test, "combined call rejects a dimension below eight");
        expect(all_zero(combined.phash_parts) && all_zero(combined.sobel_hist),
               test, "combined call clears all output for undersized input");
    }
}

void test_constant_image() {
    constexpr char test[] = "constant-image";
    std::vector<uint8_t> pixels(96 * 80, 137);
    mc_image image = wrap(pixels, 96, 80);
    mc_phase2_image_out first{};
    mc_phase2_image_out second{};
    char error[MC_ERRBUF_LEN];
    expect(mc_phase2_image(&image, &first, error, sizeof(error)) == MC_OK,
           test, "first constant image call succeeds");
    expect(mc_phase2_image(&image, &second, error, sizeof(error)) == MC_OK,
           test, "second constant image call succeeds");
    expect(std::memcmp(first.phash_parts, second.phash_parts, sizeof(first.phash_parts)) == 0,
           test, "constant image pHash is deterministic");
    expect(finite(first.sobel_hist), test, "constant image Sobel values are finite");
    expect(all_zero(first.sobel_hist), test, "constant image Sobel histogram is zero");
}

void test_structured_histogram() {
    constexpr char test[] = "structured-histogram";
    auto pixels = make_structured(257, 193);
    mc_image image = wrap(pixels, 257, 193);
    std::array<float, MC_SOBEL_HIST_DIM> hist{};
    char error[MC_ERRBUF_LEN];
    expect(mc_sobel_hist(&image, hist.data(), error, sizeof(error)) == MC_OK,
           test, "structured image Sobel succeeds");
    expect(finite(hist.data()), test, "structured histogram is finite");
    expect(std::fabs(norm(hist.data()) - 1.0) <= 1e-5,
           test, "structured histogram has unit L2 norm");
}

void test_phash_matches_independent_reference() {
    constexpr char test[] = "phash-matches-independent-reference";
    constexpr int width = 109;
    constexpr int height = 83;
    std::vector<uint8_t> pixels(static_cast<size_t>(width) * height);
    for (int y = 0; y < height; ++y) {
        for (int x = 0; x < width; ++x) {
            pixels[static_cast<size_t>(y) * width + x] =
                static_cast<uint8_t>(
                    (x * 29 + y * 47 + ((x * y) % 31) * 7) & 0xff);
        }
    }
    mc_image image = wrap(pixels, width, height);
    const auto expected = reference_phash(image);
    std::array<uint64_t, MC_PHASH_PARTS> actual{};
    char error[MC_ERRBUF_LEN];
    expect(mc_phash_parts(&image, actual.data(), error, sizeof(error)) == MC_OK,
           test, "production pHash succeeds");
    for (int part = 0; part < MC_PHASH_PARTS; ++part) {
        if (actual[part] != expected[part]) {
            fail(
                test,
                "part=" + std::to_string(part) +
                    " got=" + std::to_string(actual[part]) +
                    " want=" + std::to_string(expected[part]));
        }
    }
}

void test_contrast_inverted_vertical_step_is_unsigned_bin_zero() {
    constexpr char test[] = "contrast-inverted-vertical-step-is-unsigned-bin-zero";
    auto positive = make_vertical_step(false);
    auto inverted = make_vertical_step(true);
    mc_image positive_image = wrap(positive, 128, 128);
    mc_image inverted_image = wrap(inverted, 128, 128);
    std::array<float, MC_SOBEL_HIST_DIM> positive_hist{};
    std::array<float, MC_SOBEL_HIST_DIM> inverted_hist{};
    char error[MC_ERRBUF_LEN];
    expect(mc_sobel_hist(
               &positive_image, positive_hist.data(), error, sizeof(error)) == MC_OK,
           test, "positive vertical step succeeds");
    expect(mc_sobel_hist(
               &inverted_image, inverted_hist.data(), error, sizeof(error)) == MC_OK,
           test, "contrast-inverted vertical step succeeds");
    const auto expected = vertical_step_golden();
    expect_hist_near(
        positive_hist.data(), expected, test,
        "positive vertical step differs from exact bin/cell golden");
    expect_hist_near(
        inverted_hist.data(), expected, test,
        "inverted vertical step differs from exact unsigned bin/cell golden");
    expect(std::memcmp(
               positive_hist.data(), inverted_hist.data(),
               sizeof(float) * MC_SOBEL_HIST_DIM) == 0,
           test, "contrast inversion preserves the byte-identical unsigned histogram");
}

void test_horizontal_step_matches_bin_four_and_cell_golden() {
    constexpr char test[] = "horizontal-step-matches-bin-four-and-cell-golden";
    auto positive = make_horizontal_step(false);
    auto inverted = make_horizontal_step(true);
    mc_image positive_image = wrap(positive, 128, 128);
    mc_image inverted_image = wrap(inverted, 128, 128);
    std::array<float, MC_SOBEL_HIST_DIM> positive_hist{};
    std::array<float, MC_SOBEL_HIST_DIM> inverted_hist{};
    char error[MC_ERRBUF_LEN];
    expect(mc_sobel_hist(
               &positive_image, positive_hist.data(), error, sizeof(error)) == MC_OK,
           test, "positive horizontal step succeeds");
    expect(mc_sobel_hist(
               &inverted_image, inverted_hist.data(), error, sizeof(error)) == MC_OK,
           test, "contrast-inverted horizontal step succeeds");
    const auto expected = horizontal_step_golden();
    expect_hist_near(
        positive_hist.data(), expected, test,
        "positive horizontal step differs from exact bin/cell golden");
    expect_hist_near(
        inverted_hist.data(), expected, test,
        "inverted horizontal step differs from exact unsigned bin/cell golden");
}

void test_small_perturbation_is_similar() {
    constexpr char test[] = "small-perturbation-is-similar";
    auto original = make_structured(256, 192);
    auto perturbed = original;
    for (uint8_t& value : perturbed) {
        value = static_cast<uint8_t>((std::min)(255, static_cast<int>(value) + 1));
    }
    const size_t changed = static_cast<size_t>(91) * 256 + 127;
    perturbed[changed] =
        static_cast<uint8_t>((std::min)(255, static_cast<int>(perturbed[changed]) + 5));

    mc_image original_image = wrap(original, 256, 192);
    mc_image perturbed_image = wrap(perturbed, 256, 192);
    mc_phase2_image_out a{};
    mc_phase2_image_out b{};
    char error[MC_ERRBUF_LEN];
    expect(mc_phase2_image(&original_image, &a, error, sizeof(error)) == MC_OK,
           test, "original image computation succeeds");
    expect(mc_phase2_image(&perturbed_image, &b, error, sizeof(error)) == MC_OK,
           test, "perturbed image computation succeeds");
    expect(passing_parts(a.phash_parts, b.phash_parts) >= 8,
           test, "at least eight pHash parts have Hamming distance at most ten");
    expect(cosine(a.sobel_hist, b.sobel_hist) >= 0.85,
           test, "Sobel cosine is at least 0.85");
}

void test_unrelated_structures_do_not_all_match() {
    constexpr char test[] = "unrelated-structures-do-not-all-match";
    auto horizontal = make_unrelated(256, 192, Structure::Horizontal);
    auto vertical = make_unrelated(256, 192, Structure::Vertical);
    auto checker = make_unrelated(256, 192, Structure::Checker);
    mc_image images[] = {
        wrap(horizontal, 256, 192),
        wrap(vertical, 256, 192),
        wrap(checker, 256, 192),
    };
    std::array<mc_phase2_image_out, 3> outputs{};
    char error[MC_ERRBUF_LEN];
    for (size_t i = 0; i < outputs.size(); ++i) {
        expect(mc_phase2_image(&images[i], &outputs[i], error, sizeof(error)) == MC_OK,
               test, "unrelated structure computation succeeds");
    }
    for (size_t i = 0; i < outputs.size(); ++i) {
        for (size_t j = i + 1; j < outputs.size(); ++j) {
            const bool phash_pass =
                passing_parts(outputs[i].phash_parts, outputs[j].phash_parts) >= 8;
            const bool sobel_pass =
                cosine(outputs[i].sobel_hist, outputs[j].sobel_hist) >= 0.85;
            expect(!(phash_pass && sobel_pass), test,
                   "each unrelated pair must fail at least one threshold");
        }
    }
}

}  // namespace

int main() {
    test_deterministic_and_input_unchanged();
    test_argument_contracts_and_output_clearing();
    test_constant_image();
    test_structured_histogram();
    test_phash_matches_independent_reference();
    test_contrast_inverted_vertical_step_is_unsigned_bin_zero();
    test_horizontal_step_matches_bin_four_and_cell_golden();
    test_small_perturbation_is_similar();
    test_unrelated_structures_do_not_all_match();

    if (failures != 0) {
        std::fprintf(stderr, "%d phase-2 test assertion(s) failed\n", failures);
        return 1;
    }
    std::puts("mediacore phase-2 pHash and Sobel tests passed");
    return 0;
}
