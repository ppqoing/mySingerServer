#include "videocore/videocore.h"

#include <cstddef>

#include "cancel_token.h"
#include "error.h"
#include "media_session.h"
#include "runtime_info.h"
#include "url_path.h"

namespace {

int32_t Fail(vc_error* error,
             int32_t code,
             const char* message) noexcept {
    vc::detail::SetError(error, code, 0, 0, message);
    return code;
}

bool ValidateError(vc_error* error) noexcept {
    if (error == nullptr) {
        return true;
    }
    constexpr uint32_t abi_header_size =
        static_cast<uint32_t>(sizeof(uint32_t) * 2u);
    if (error->struct_size < abi_header_size) {
        return false;
    }
    if (error->abi_version != VC_ABI_VERSION) {
        return false;
    }
    if (error->struct_size < sizeof(vc_error)) {
        Fail(error, VC_ERR_ABI, "vc_error ABI mismatch");
        return false;
    }
    return true;
}

template <typename Structure>
bool ValidateStructure(const Structure* value,
                       vc_error* error,
                       const char* message) noexcept {
    if (value == nullptr) {
        Fail(error, VC_ERR_INVALID_ARG, message);
        return false;
    }
    if (value->struct_size < sizeof(Structure) ||
        value->abi_version != VC_ABI_VERSION) {
        Fail(error, VC_ERR_ABI, message);
        return false;
    }
    return true;
}

int32_t ValidateAnalysisResult(const vc_analysis_result* result,
                               vc_error* error) noexcept {
    if (!ValidateStructure(
            &result->image_features,
            error,
            "image feature set ABI mismatch") ||
        !ValidateStructure(
            &result->contact_sheet_features,
            error,
            "contact sheet feature set ABI mismatch")) {
        return VC_ERR_ABI;
    }
    if (result->reserved_flags != 0u ||
        result->image_features.reserved_0 != 0u ||
        result->contact_sheet_features.reserved_0 != 0u) {
        return Fail(error,
                    VC_ERR_INVALID_ARG,
                    "analysis result reserved fields must be zero");
    }
    for (uint32_t index = 0; index < VC_VIDEO_FRAME_COUNT; ++index) {
        const vc_video_frame_result& frame = result->frames[index];
        if (!ValidateStructure(
                &frame, error, "video frame result ABI mismatch") ||
            !ValidateStructure(
                &frame.features,
                error,
                "video frame feature set ABI mismatch")) {
            return VC_ERR_ABI;
        }
        if (frame.features.reserved_0 != 0u) {
            return Fail(error,
                        VC_ERR_INVALID_ARG,
                        "video frame reserved fields must be zero");
        }
    }
    return VC_OK;
}

}  // namespace

extern "C" {

VC_API uint32_t VC_CALL vc_abi_version(void) {
    return VC_ABI_VERSION;
}

VC_API const char* VC_CALL vc_version(void) {
    return VC_VERSION_STRING;
}

VC_API int32_t VC_CALL vc_runtime_info(
    struct vc_runtime_info* out,
    vc_error* err) {
    return vc::detail::Guard(err, [out, err]() -> int32_t {
        if (!ValidateError(err)) {
            return VC_ERR_ABI;
        }
        if (!ValidateStructure(out, err, "vc_runtime_info ABI mismatch")) {
            return out == nullptr ? VC_ERR_INVALID_ARG : VC_ERR_ABI;
        }
        return vc::detail::PopulateRuntimeInfo(
            out, err, vc::detail::DefaultVersionProvider());
    });
}

VC_API int32_t VC_CALL vc_cancel_create(
    vc_cancel_token** out,
    vc_error* err) {
    return vc::detail::Guard(err, [out, err]() -> int32_t {
        if (!ValidateError(err)) {
            return VC_ERR_ABI;
        }
        if (out == nullptr) {
            return Fail(err,
                        VC_ERR_INVALID_ARG,
                        "cancel token output is null");
        }
        return vc::detail::CreateCancelToken(out, err);
    });
}

VC_API void VC_CALL vc_cancel_request(vc_cancel_token* token) {
    vc::detail::RequestCancel(token);
}

VC_API void VC_CALL vc_cancel_free(vc_cancel_token* token) {
    vc::detail::FreeCancelToken(token);
}

VC_API int32_t VC_CALL vc_media_open_w(
    const uint16_t* path,
    uint32_t path_units,
    const vc_media_open_options* options,
    vc_cancel_token* cancel,
    vc_media_session** out,
    vc_error* err) {
    return vc::detail::Guard(
        err,
        [path, path_units, options, cancel, out, err]() -> int32_t {
            if (!ValidateError(err)) {
                return VC_ERR_ABI;
            }
            if (out == nullptr) {
                return Fail(err,
                            VC_ERR_INVALID_ARG,
                            "media session output is null");
            }
            if (path == nullptr || path_units == 0u) {
                return Fail(err,
                            VC_ERR_INVALID_ARG,
                            "media path is empty");
            }
            if (!ValidateStructure(
                    options, err, "media open options ABI mismatch")) {
                return options == nullptr ? VC_ERR_INVALID_ARG : VC_ERR_ABI;
            }
            if (options->reserved_flags != 0u ||
                options->reserved_0 != 0u) {
                return Fail(err,
                            VC_ERR_INVALID_ARG,
                            "media open reserved fields must be zero");
            }
            if (options->expected_media_type > VC_MEDIA_TYPE_VIDEO) {
                return Fail(err,
                            VC_ERR_INVALID_ARG,
                            "expected media type is out of range");
            }
            for (uint32_t index = 0; index < path_units; ++index) {
                if (path[index] == 0u) {
                    return Fail(err,
                                VC_ERR_INVALID_ARG,
                                "media path contains embedded NUL");
                }
            }
            if (vc::detail::LooksLikeUrl(path, path_units)) {
                return Fail(err,
                            VC_ERR_INVALID_ARG,
                            "media path must not be a URL");
            }
            return vc::detail::CreateMediaSession(
                path, path_units, *options, cancel, out, err);
        });
}

VC_API int32_t VC_CALL vc_media_hash(
    vc_media_session* session,
    uint8_t out_sha512[VC_SHA512_SIZE],
    vc_error* err) {
    return vc::detail::Guard(
        err,
        [session, out_sha512, err]() -> int32_t {
            if (!ValidateError(err)) {
                return VC_ERR_ABI;
            }
            if (session == nullptr || out_sha512 == nullptr) {
                return Fail(err,
                            VC_ERR_INVALID_ARG,
                            "media hash arguments are null");
            }
            return vc::detail::HashMediaSession(
                session, out_sha512, err);
        });
}

VC_API int32_t VC_CALL vc_media_analyze(
    vc_media_session* session,
    const vc_analysis_request* request,
    vc_analysis_result* out,
    vc_error* err) {
    return vc::detail::Guard(
        err,
        [session, request, out, err]() -> int32_t {
            if (!ValidateError(err)) {
                return VC_ERR_ABI;
            }
            if (session == nullptr) {
                return Fail(err,
                            VC_ERR_INVALID_ARG,
                            "media session is null");
            }
            if (!ValidateStructure(
                    request, err, "analysis request ABI mismatch")) {
                return request == nullptr ? VC_ERR_INVALID_ARG : VC_ERR_ABI;
            }
            if (!ValidateStructure(
                    out, err, "analysis result ABI mismatch")) {
                return out == nullptr ? VC_ERR_INVALID_ARG : VC_ERR_ABI;
            }
            const vc_analysis_request request_value = *request;
            const int32_t result_status =
                ValidateAnalysisResult(out, err);
            if (result_status != VC_OK) {
                return result_status;
            }
            return vc::detail::AnalyzeMediaSession(
                session, request_value, out, err);
        });
}

VC_API void VC_CALL vc_media_close(vc_media_session* session) {
    try {
        vc::detail::CloseMediaSession(session);
    } catch (...) {
    }
}

}  // extern "C"
