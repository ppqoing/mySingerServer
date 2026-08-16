//go:build cgo && windows

package videocore

/*
#cgo CFLAGS: -I${SRCDIR}/../../../videocore/include
#cgo LDFLAGS: -L${SRCDIR} -lvideocore
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <videocore/videocore.h>

static uint16_t* go_vc_copy_utf16(const uint16_t* source, uint32_t units) {
	if (source == NULL || units == 0u ||
		(size_t)units > SIZE_MAX / sizeof(uint16_t)) {
		return NULL;
	}
	size_t bytes = (size_t)units * sizeof(uint16_t);
	uint16_t* copy = (uint16_t*)malloc(bytes);
	if (copy == NULL) return NULL;
	memcpy(copy, source, bytes);
	return copy;
}

static void go_vc_free(void* value) {
	free(value);
}

static void go_vc_init_error(vc_error* value) {
	memset(value, 0, sizeof(*value));
	value->struct_size = (uint32_t)sizeof(*value);
	value->abi_version = VC_ABI_VERSION;
}

static void go_vc_init_runtime_info(struct vc_runtime_info* value) {
	memset(value, 0, sizeof(*value));
	value->struct_size = (uint32_t)sizeof(*value);
	value->abi_version = VC_ABI_VERSION;
}

static void go_vc_init_open_options(vc_media_open_options* value) {
	memset(value, 0, sizeof(*value));
	value->struct_size = (uint32_t)sizeof(*value);
	value->abi_version = VC_ABI_VERSION;
}

static void go_vc_init_analysis_request(vc_analysis_request* value) {
	memset(value, 0, sizeof(*value));
	value->struct_size = (uint32_t)sizeof(*value);
	value->abi_version = VC_ABI_VERSION;
}

static void go_vc_init_feature_set(vc_feature_set* value) {
	memset(value, 0, sizeof(*value));
	value->struct_size = (uint32_t)sizeof(*value);
	value->abi_version = VC_ABI_VERSION;
}

static void go_vc_init_analysis_result(vc_analysis_result* value) {
	memset(value, 0, sizeof(*value));
	value->struct_size = (uint32_t)sizeof(*value);
	value->abi_version = VC_ABI_VERSION;
	go_vc_init_feature_set(&value->image_features);
	go_vc_init_feature_set(&value->contact_sheet_features);
	for (uint32_t index = 0; index < VC_VIDEO_FRAME_COUNT; ++index) {
		value->frames[index].struct_size =
			(uint32_t)sizeof(value->frames[index]);
		value->frames[index].abi_version = VC_ABI_VERSION;
		go_vc_init_feature_set(&value->frames[index].features);
	}
}

static const char* go_vc_error_message(const vc_error* value) {
	return value->message_utf8;
}

static const char* go_vc_runtime_version(
	const struct vc_runtime_info* value) {
	return value->videocore_version_utf8;
}

static const char* go_vc_runtime_build_id(
	const struct vc_runtime_info* value) {
	return value->ffmpeg_build_id_utf8;
}
*/
import "C"

import (
	"fmt"
	"math"
	"runtime"
	"time"
	"unsafe"

	"dedup/internal/worker"
)

type cgoBridge struct{}

func platformNativeBridge() nativeBridge { return cgoBridge{} }

func (cgoBridge) runtime() (RuntimeInfo, error) {
	var out C.struct_vc_runtime_info
	var nativeErr C.vc_error
	C.go_vc_init_runtime_info(&out)
	C.go_vc_init_error(&nativeErr)
	rc := int32(C.vc_runtime_info(&out, &nativeErr))
	if rc != StatusOK {
		return RuntimeInfo{}, cgoCallError("runtime info", rc, &nativeErr)
	}
	info := RuntimeInfo{
		ABI:           uint32(C.vc_abi_version()),
		Version:       C.GoString(C.go_vc_runtime_version(&out)),
		FFmpegBuildID: C.GoString(C.go_vc_runtime_build_id(&out)),
		Components: [4]RuntimeComponent{
			{Name: "avformat", HeaderVersion: uint32(out.avformat_header_version), RuntimeVersion: uint32(out.avformat_runtime_version)},
			{Name: "avcodec", HeaderVersion: uint32(out.avcodec_header_version), RuntimeVersion: uint32(out.avcodec_runtime_version)},
			{Name: "avutil", HeaderVersion: uint32(out.avutil_header_version), RuntimeVersion: uint32(out.avutil_runtime_version)},
			{Name: "swscale", HeaderVersion: uint32(out.swscale_header_version), RuntimeVersion: uint32(out.swscale_runtime_version)},
		},
	}
	if uint32(out.abi_version) != info.ABI {
		return RuntimeInfo{}, fmt.Errorf("%w: runtime-info ABI=%d exported ABI=%d", ErrABIMismatch, uint32(out.abi_version), info.ABI)
	}
	return info, nil
}

func (cgoBridge) cancelCreate() (nativeCancel, error) {
	var token *C.vc_cancel_token
	var nativeErr C.vc_error
	C.go_vc_init_error(&nativeErr)
	rc := int32(C.vc_cancel_create(&token, &nativeErr))
	if rc != StatusOK {
		return nativeCancel{}, cgoCallError("cancel create", rc, &nativeErr)
	}
	if token == nil {
		return nativeCancel{}, &NativeError{Code: StatusInternal, Message: "cancel create returned nil"}
	}
	return nativeCancel{value: unsafe.Pointer(token)}, nil
}

func (cgoBridge) cancelRequest(token nativeCancel) {
	if token.value != nil {
		C.vc_cancel_request((*C.vc_cancel_token)(token.value))
	}
}

func (cgoBridge) cancelFree(token nativeCancel) {
	if token.value != nil {
		C.vc_cancel_free((*C.vc_cancel_token)(token.value))
	}
}

func (cgoBridge) open(path []uint16, options OpenOptions, cancel nativeCancel) (nativeSession, error) {
	if len(path) == 0 || uint64(len(path)) > math.MaxUint32 {
		return nativeSession{}, ErrInvalidPath
	}
	if options.ImageMemoryBytes < 0 {
		return nativeSession{}, &NativeError{Code: StatusInvalidArg, Message: "image memory limit is negative"}
	}
	timeout, err := durationMilliseconds(options.NativeTimeout)
	if err != nil {
		return nativeSession{}, err
	}
	mediaType, err := nativeMediaType(options.Kind)
	if err != nil {
		return nativeSession{}, err
	}
	var nativeOptions C.vc_media_open_options
	var nativeErr C.vc_error
	C.go_vc_init_open_options(&nativeOptions)
	C.go_vc_init_error(&nativeErr)
	nativeOptions.expected_media_type = C.uint32_t(mediaType)
	nativeOptions.image_max_bytes = C.uint64_t(options.ImageMemoryBytes)
	nativeOptions.operation_timeout_ms = C.uint32_t(timeout)
	var session *C.vc_media_session
	rc := int32(C.vc_media_open_w(
		(*C.uint16_t)(unsafe.Pointer(unsafe.SliceData(path))),
		C.uint32_t(len(path)),
		&nativeOptions,
		(*C.vc_cancel_token)(cancel.value),
		&session,
		&nativeErr,
	))
	runtime.KeepAlive(path)
	if rc != StatusOK {
		return nativeSession{}, cgoCallError("media open", rc, &nativeErr)
	}
	return nativeSession{value: unsafe.Pointer(session)}, nil
}

func (cgoBridge) hash(session nativeSession) ([64]byte, error) {
	var result [64]byte
	var nativeResult [64]C.uint8_t
	var nativeErr C.vc_error
	C.go_vc_init_error(&nativeErr)
	rc := int32(C.vc_media_hash(
		(*C.vc_media_session)(session.value),
		&nativeResult[0],
		&nativeErr,
	))
	if rc != StatusOK {
		return result, cgoCallError("media hash", rc, &nativeErr)
	}
	for index := range result {
		result[index] = byte(nativeResult[index])
	}
	return result, nil
}

func (cgoBridge) analyze(session nativeSession, request AnalysisRequest) (AnalysisResult, error) {
	probeTimeout, err := durationMilliseconds(request.ProbeTimeout)
	if err != nil {
		return AnalysisResult{}, err
	}
	frameTimeout, err := durationMilliseconds(request.FrameTimeout)
	if err != nil {
		return AnalysisResult{}, err
	}
	if request.TileMaxSide < 0 {
		return AnalysisResult{}, &NativeError{Code: StatusInvalidArg, Message: "contact-sheet tile size is negative"}
	}
	featureMask, err := nativeFeatureMask(request.Fields)
	if err != nil {
		return AnalysisResult{}, err
	}
	var temporaryPath []uint16
	if request.TempJPEGPath != "" {
		temporaryPath, err = utf16Path(request.TempJPEGPath)
		if err != nil {
			return AnalysisResult{}, err
		}
	}
	var nativeRequest C.vc_analysis_request
	var nativeResult C.vc_analysis_result
	var nativeErr C.vc_error
	C.go_vc_init_analysis_request(&nativeRequest)
	C.go_vc_init_analysis_result(&nativeResult)
	C.go_vc_init_error(&nativeErr)
	nativeRequest.feature_mask = C.uint64_t(featureMask)
	nativeRequest.frame_mask = C.uint32_t(request.FrameMask)
	nativeRequest.known_duration_ms = C.int64_t(request.KnownDurationMS)
	nativeRequest.probe_timeout_ms = C.uint32_t(probeTimeout)
	nativeRequest.frame_timeout_ms = C.uint32_t(frameTimeout)
	nativeRequest.contact_sheet_tile_max_side = C.uint32_t(request.TileMaxSide)
	var nativeTemporaryPath *C.uint16_t
	if len(temporaryPath) != 0 {
		nativeTemporaryPath = C.go_vc_copy_utf16(
			(*C.uint16_t)(unsafe.Pointer(unsafe.SliceData(temporaryPath))),
			C.uint32_t(len(temporaryPath)),
		)
		runtime.KeepAlive(temporaryPath)
		if nativeTemporaryPath == nil {
			return AnalysisResult{}, &NativeError{
				Code: StatusOOM, Message: "temporary JPEG path allocation failed",
			}
		}
		defer C.go_vc_free(unsafe.Pointer(nativeTemporaryPath))
		nativeRequest.temporary_jpeg_path = nativeTemporaryPath
		nativeRequest.temporary_jpeg_path_units = C.uint32_t(len(temporaryPath))
	}
	rc := int32(C.vc_media_analyze(
		(*C.vc_media_session)(session.value),
		&nativeRequest,
		&nativeResult,
		&nativeErr,
	))
	if rc != StatusOK {
		return AnalysisResult{}, cgoCallError("media analyze", rc, &nativeErr)
	}
	return analysisResultFromC(nativeResult), nil
}

func (cgoBridge) close(session nativeSession) {
	if session.value != nil {
		C.vc_media_close((*C.vc_media_session)(session.value))
	}
}

func cgoCallError(operation string, rc int32, nativeErr *C.vc_error) error {
	code := int32(nativeErr.code)
	if code == StatusOK {
		code = rc
	}
	err := nativeCallError(
		code,
		int32(nativeErr.ffmpeg_code),
		uint32(nativeErr.win32_code),
		C.GoString(C.go_vc_error_message(nativeErr)),
	)
	return fmt.Errorf("videocore: %s: %w", operation, err)
}

func durationMilliseconds(value time.Duration) (uint32, error) {
	if value < 0 {
		return 0, &NativeError{Code: StatusInvalidArg, Message: "timeout is negative"}
	}
	if value == 0 {
		return 0, nil
	}
	milliseconds := uint64(value / time.Millisecond)
	if value%time.Millisecond != 0 {
		milliseconds++
	}
	if milliseconds > math.MaxUint32 {
		return 0, &NativeError{Code: StatusInvalidArg, Message: "timeout exceeds native range"}
	}
	return uint32(milliseconds), nil
}

func nativeMediaType(kind worker.MediaKind) (uint32, error) {
	switch kind {
	case 0:
		return 0, nil
	case worker.MediaImage:
		return 1, nil
	case worker.MediaVideo:
		return 2, nil
	default:
		return 0, &NativeError{Code: StatusInvalidArg, Message: "unsupported media kind"}
	}
}

func nativeFeatureMask(fields uint32) (uint64, error) {
	const supported = worker.MaskImagePDQ | worker.MaskPHashParts |
		worker.MaskSobelHist | worker.MaskVideo6F |
		worker.MaskVideoDuration | worker.MaskVideoContactSheet
	if fields&^supported != 0 {
		return 0, &NativeError{Code: StatusInvalidArg, Message: "unsupported analysis field mask"}
	}
	var mask uint64
	if fields&(worker.MaskImagePDQ|worker.MaskVideo6F|worker.MaskVideoContactSheet) != 0 {
		mask |= 1 << 0
	}
	if fields&(worker.MaskPHashParts|worker.MaskVideo6F|worker.MaskVideoContactSheet) != 0 {
		mask |= 1 << 1
	}
	if fields&(worker.MaskSobelHist|worker.MaskVideo6F|worker.MaskVideoContactSheet) != 0 {
		mask |= 1 << 2
	}
	if fields&worker.MaskVideoDuration != 0 {
		mask |= 1 << 3
	}
	if fields&worker.MaskVideoContactSheet != 0 {
		mask |= 1 << 4
	}
	return mask, nil
}

func featureSetFromC(native C.vc_feature_set) nativeFeatureSet {
	var result nativeFeatureSet
	for index := range result.pdq {
		result.pdq[index] = byte(native.pdq[index])
	}
	result.pdqQuality = uint32(native.pdq_quality)
	for index := range result.phash {
		result.phash[index] = uint64(native.phash[index])
	}
	for index := range result.sobel {
		result.sobel[index] = float32(native.sobel_histogram[index])
	}
	return result
}

func analysisResultFromC(native C.vc_analysis_result) AnalysisResult {
	result := nativeAnalysisResult{
		mediaType:          uint32(native.media_type),
		durationMS:         int64(native.duration_ms),
		durationStatus:     int32(native.duration_status),
		imageStatus:        int32(native.image_status),
		contactStatus:      int32(native.contact_sheet_status),
		contactWidth:       uint32(native.contact_sheet_width),
		contactHeight:      uint32(native.contact_sheet_height),
		completedFrameMask: uint8(native.completed_frame_mask),
		imageFeatures:      featureSetFromC(native.image_features),
		contactFeatures:    featureSetFromC(native.contact_sheet_features),
		operationElapsedMS: uint64(native.operation_elapsed_ms),
		decodeElapsedMS:    uint64(native.decode_elapsed_ms),
	}
	for index := range result.frames {
		frame := native.frames[index]
		result.frames[index] = nativeFrameResult{
			standardIndex: uint32(frame.standard_index),
			status:        int32(frame.status),
			sampleTimeMS:  int64(frame.sample_time_ms),
			features:      featureSetFromC(frame.features),
		}
	}
	return analysisResultFromNative(result)
}
