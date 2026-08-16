#include "image_analysis.h"

#include <algorithm>
#include <array>
#include <cstring>
#include <new>

#include "error.h"
#include "native_algorithms/image_decode.h"
#include "native_algorithms/pdq.h"
#include "native_algorithms/phash_parts.h"
#include "native_algorithms/sobel_hist.h"

namespace vc::detail {

#if defined(VC_IMAGE_ANALYSIS_TESTING)
void ImageAnalysisTestRecordDecode() noexcept;
bool ImageAnalysisTestConsumeAlgorithmFailure() noexcept;
#endif

namespace {

constexpr uint64_t kImageFeatureMask =
    VC_FEATURE_PDQ | VC_FEATURE_PHASH | VC_FEATURE_SOBEL;

void ClearImagePayload(vc_feature_set* features) noexcept {
    std::memset(features->pdq, 0, sizeof(features->pdq));
    features->pdq_quality = 0u;
    std::memset(features->phash, 0, sizeof(features->phash));
    std::memset(features->sobel_histogram,
                0,
                sizeof(features->sobel_histogram));
}

int32_t MapImageStatus(videocore::native::ImageStatus status) noexcept {
    using videocore::native::ImageStatus;
    switch (status) {
        case ImageStatus::ok:
            return VC_OK;
        case ImageStatus::invalid_argument:
            return VC_ERR_INVALID_ARG;
        case ImageStatus::out_of_memory:
            return VC_ERR_OOM;
        case ImageStatus::decode_error:
        case ImageStatus::size_error:
            return VC_ERR_DECODE;
        case ImageStatus::internal_error:
            return VC_ERR_INTERNAL;
    }
    return VC_ERR_INTERNAL;
}

videocore::native::ImageStatus DecodeImageForAnalysis(
    const std::vector<uint8_t>& encoded,
    videocore::native::GrayImage* out) noexcept {
#if defined(VC_IMAGE_ANALYSIS_TESTING)
    ImageAnalysisTestRecordDecode();
#endif
    return videocore::native::DecodeImage(
        encoded.data(), encoded.size(), out);
}

videocore::native::ImageStatus PrepareFeatureImage(
    const videocore::native::GrayImage& decoded,
    videocore::native::GrayImage* expanded,
    const videocore::native::GrayImage** feature_image) noexcept {
    if (expanded == nullptr || feature_image == nullptr ||
        decoded.width <= 0 || decoded.height <= 0 ||
        decoded.stride < decoded.width) {
        return videocore::native::ImageStatus::invalid_argument;
    }
    const uint64_t decoded_bytes = static_cast<uint64_t>(decoded.stride) *
                                   static_cast<uint64_t>(decoded.height);
    if (decoded_bytes > decoded.pixels.size()) {
        return videocore::native::ImageStatus::invalid_argument;
    }
    if (decoded.width >= 8 && decoded.height >= 8) {
        *feature_image = &decoded;
        return videocore::native::ImageStatus::ok;
    }
    const int32_t width = (std::max)(decoded.width, 8);
    const int32_t height = (std::max)(decoded.height, 8);
    try {
        expanded->width = width;
        expanded->height = height;
        expanded->stride = width;
        expanded->pixels.resize(static_cast<size_t>(width) * height);
        for (int32_t y = 0; y < height; ++y) {
            const int32_t source_y = y * decoded.height / height;
            for (int32_t x = 0; x < width; ++x) {
                const int32_t source_x = x * decoded.width / width;
                expanded->pixels[static_cast<size_t>(y) * width + x] =
                    decoded.pixels[static_cast<size_t>(source_y) *
                                       decoded.stride +
                                   source_x];
            }
        }
    } catch (const std::bad_alloc&) {
        return videocore::native::ImageStatus::out_of_memory;
    } catch (...) {
        return videocore::native::ImageStatus::internal_error;
    }
    *feature_image = expanded;
    return videocore::native::ImageStatus::ok;
}

}  // namespace

int32_t PublishImageFailure(vc_analysis_result* out,
                            vc_error* error,
                            int32_t code,
                            const char* message) noexcept {
    ClearImagePayload(&out->image_features);
    out->media_type = VC_MEDIA_TYPE_IMAGE;
    out->image_status = code;
    out->contact_sheet_width = 0u;
    out->contact_sheet_height = 0u;
    out->completed_frame_mask = 0u;
    SetError(error, code, 0, 0, message);
    return code;
}

int32_t AnalyzeImageBytes(const std::vector<uint8_t>& encoded,
                          uint64_t feature_mask,
                          vc_analysis_result* out,
                          vc_error* error) noexcept {
    if (feature_mask == 0u ||
        (feature_mask & ~kImageFeatureMask) != 0u) {
        return PublishImageFailure(
            out,
            error,
            VC_ERR_UNSUPPORTED,
            "image analysis feature is unavailable");
    }

    videocore::native::GrayImage gray;
    videocore::native::ImageStatus image_status =
        DecodeImageForAnalysis(encoded, &gray);
    if (image_status != videocore::native::ImageStatus::ok) {
        return PublishImageFailure(out,
                                   error,
                                   MapImageStatus(image_status),
                                   "image decode failed");
    }

    // The feature algorithms require an 8x8 input. Preserve the decoded
    // dimensions in the result, but expand smaller valid images by nearest
    // neighbour so every analysis sample still comes from decoded pixels.
    videocore::native::GrayImage expanded;
    const videocore::native::GrayImage* feature_image = nullptr;
    image_status = PrepareFeatureImage(gray, &expanded, &feature_image);
    if (image_status != videocore::native::ImageStatus::ok) {
        return PublishImageFailure(out,
                                   error,
                                   MapImageStatus(image_status),
                                   "image feature input preparation failed");
    }

    std::array<uint8_t, VC_PDQ_SIZE> pdq{};
    int32_t pdq_quality = 0;
    std::array<uint64_t, VC_PHASH_COUNT> phash{};
    std::array<float, VC_SOBEL_HISTOGRAM_SIZE> sobel{};
#if defined(VC_IMAGE_ANALYSIS_TESTING)
    if (ImageAnalysisTestConsumeAlgorithmFailure()) {
        image_status = videocore::native::ImageStatus::internal_error;
    }
#endif
    if (image_status == videocore::native::ImageStatus::ok &&
        (feature_mask & VC_FEATURE_PDQ) != 0u) {
        image_status = videocore::native::ComputePdq(
            *feature_image, &pdq, &pdq_quality);
    }
    if (image_status == videocore::native::ImageStatus::ok &&
        (feature_mask & VC_FEATURE_PHASH) != 0u) {
        image_status = videocore::native::ComputePHashParts(
            *feature_image, &phash);
    }
    if (image_status == videocore::native::ImageStatus::ok &&
        (feature_mask & VC_FEATURE_SOBEL) != 0u) {
        image_status = videocore::native::ComputeSobelHistogram(
            *feature_image, &sobel);
    }
    if (image_status != videocore::native::ImageStatus::ok) {
        return PublishImageFailure(out,
                                   error,
                                   MapImageStatus(image_status),
                                   "image feature computation failed");
    }

    ClearImagePayload(&out->image_features);
    if ((feature_mask & VC_FEATURE_PDQ) != 0u) {
        std::memcpy(out->image_features.pdq, pdq.data(), pdq.size());
        out->image_features.pdq_quality =
            static_cast<uint32_t>(pdq_quality);
    }
    if ((feature_mask & VC_FEATURE_PHASH) != 0u) {
        std::memcpy(out->image_features.phash,
                    phash.data(),
                    sizeof(out->image_features.phash));
    }
    if ((feature_mask & VC_FEATURE_SOBEL) != 0u) {
        std::memcpy(out->image_features.sobel_histogram,
                    sobel.data(),
                    sizeof(out->image_features.sobel_histogram));
    }
    out->media_type = VC_MEDIA_TYPE_IMAGE;
    out->image_status = VC_OK;
    // ABI 1 has one media-dimension pair. For image results it carries the
    // decoded image dimensions; for video results it carries contact-sheet
    // dimensions. The Go boundary maps it according to media_type.
    out->contact_sheet_width = static_cast<uint32_t>(gray.width);
    out->contact_sheet_height = static_cast<uint32_t>(gray.height);
    out->completed_frame_mask = 0u;
    SetError(error, VC_OK, 0, 0, "");
    return VC_OK;
}

}  // namespace vc::detail
