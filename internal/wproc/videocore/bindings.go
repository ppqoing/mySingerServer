//go:build cgo && windows

package videocore

/*
#cgo CFLAGS: -I${SRCDIR}/../../../videocore/include
#cgo LDFLAGS: -L${SRCDIR} -lvideocore
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <videocore/videocore.h>

extern int32_t go_vc_io_acquire(
	uintptr_t, uint32_t, uint64_t, uint64_t*, uint64_t*, vc_error*);
extern void go_vc_io_report(
	uintptr_t, uint64_t, uint64_t, uint64_t, int32_t);

static int32_t VC_CALL go_vc_io_acquire_bridge(
	uintptr_t context, uint32_t operation, uint64_t requested_bytes,
	uint64_t* lease_id, uint64_t* granted_bytes, vc_error* err) {
	return go_vc_io_acquire(context, operation, requested_bytes,
		lease_id, granted_bytes, err);
}

static void VC_CALL go_vc_io_report_bridge(
	uintptr_t context, uint64_t lease_id, uint64_t actual_bytes,
	uint64_t elapsed_ns, int32_t status) {
	go_vc_io_report(context, lease_id, actual_bytes, elapsed_ns, status);
}

static vc_io_governor* go_vc_new_io_governor(uintptr_t context) {
	vc_io_governor* value = (vc_io_governor*)calloc(1u, sizeof(*value));
	if (value == NULL) return NULL;
	value->struct_size = (uint32_t)sizeof(*value);
	value->abi_version = VC_ABI_VERSION;
	value->context = context;
	value->acquire = &go_vc_io_acquire_bridge;
	value->report = &go_vc_io_report_bridge;
	return value;
}

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

static void go_vc_init_container_info(vc_video_container_info* value) {
	memset(value, 0, sizeof(*value));
	value->struct_size = (uint32_t)sizeof(*value);
	value->abi_version = VC_ABI_VERSION;
}

static void go_vc_init_stream_info(vc_video_stream_info* value) {
	memset(value, 0, sizeof(*value));
	value->struct_size = (uint32_t)sizeof(*value);
	value->abi_version = VC_ABI_VERSION;
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

	"dedup/internal/proto"
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
	var nativeGovernor *C.vc_io_governor
	if options.ioGovernorContext != 0 {
		nativeGovernor = C.go_vc_new_io_governor(C.uintptr_t(options.ioGovernorContext))
		if nativeGovernor == nil {
			return nativeSession{}, &NativeError{Code: StatusOOM, Message: "I/O governor allocation failed"}
		}
		defer C.go_vc_free(unsafe.Pointer(nativeGovernor))
		nativeOptions.io_governor = nativeGovernor
	}
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
	runtime.KeepAlive(options.IOGovernor)
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

func (cgoBridge) videoMetadata(session nativeSession) (*VideoMetadata, error) {
	var container C.vc_video_container_info
	var nativeErr C.vc_error
	C.go_vc_init_container_info(&container)
	C.go_vc_init_error(&nativeErr)
	rc := int32(C.vc_media_container_info(
		(*C.vc_media_session)(session.value), &container, &nativeErr,
	))
	if rc != StatusOK {
		return nil, cgoCallError("media container info", rc, &nativeErr)
	}
	containerTags, err := cgoMetadataJSON(session, -1)
	if err != nil {
		return nil, err
	}
	result := &VideoMetadata{Container: proto.VideoContainerMetadata{
		FormatName:     cgoFixedString(unsafe.Pointer(&container.format_name_utf8[0])),
		FormatLongName: cgoFixedString(unsafe.Pointer(&container.format_long_name_utf8[0])),
		TagsJSON:       containerTags,
		DecoderName:    cgoFixedString(unsafe.Pointer(&container.decoder_name_utf8[0])),
	}}
	containerMask := uint64(container.present_mask)
	if containerMask&uint64(C.VC_CONTAINER_HAS_START_TIME) != 0 {
		result.Container.StartTimeUS = int64Pointer(int64(container.start_time_us))
	}
	if containerMask&uint64(C.VC_CONTAINER_HAS_DURATION) != 0 {
		result.Container.DurationUS = int64Pointer(int64(container.duration_us))
	}
	if containerMask&uint64(C.VC_CONTAINER_HAS_BIT_RATE) != 0 {
		result.Container.BitRate = int64Pointer(int64(container.bit_rate))
	}
	if containerMask&uint64(C.VC_CONTAINER_HAS_FILE_SIZE) != 0 {
		result.Container.FileSize = int64Pointer(int64(container.file_size))
	}
	if containerMask&uint64(C.VC_CONTAINER_HAS_PROBE_SCORE) != 0 {
		result.Container.ProbeScore = int32Pointer(int32(container.probe_score))
	}
	if containerMask&uint64(C.VC_CONTAINER_HAS_PRIMARY_VIDEO) != 0 {
		result.Container.PrimaryVideoStream = int32Pointer(int32(container.primary_video_stream))
	}

	count := uint32(C.vc_media_stream_count((*C.vc_media_session)(session.value)))
	if count > uint32(C.VC_MAX_STREAMS) {
		return nil, &NativeError{Code: StatusOutputTooLarge, Message: "stream count exceeds native limit"}
	}
	result.Streams = make([]proto.VideoStreamMetadata, 0, count)
	for ordinal := uint32(0); ordinal < count; ordinal++ {
		var stream C.vc_video_stream_info
		C.go_vc_init_stream_info(&stream)
		C.go_vc_init_error(&nativeErr)
		rc = int32(C.vc_media_stream_info(
			(*C.vc_media_session)(session.value), C.uint32_t(ordinal),
			&stream, &nativeErr,
		))
		if rc != StatusOK {
			return nil, cgoCallError("media stream info", rc, &nativeErr)
		}
		if uint32(stream.stream_index) > math.MaxInt32 {
			return nil, &NativeError{Code: StatusOutputTooLarge, Message: "stream index exceeds protocol range"}
		}
		tags, tagsErr := cgoMetadataJSON(session, int32(stream.stream_index))
		if tagsErr != nil {
			return nil, tagsErr
		}
		mediaType, mediaErr := cgoStreamMediaType(uint32(stream.media_type))
		if mediaErr != nil {
			return nil, mediaErr
		}
		value := proto.VideoStreamMetadata{
			Index: int32(stream.stream_index), MediaType: mediaType,
			CodecID: int32(stream.codec_id), CodecName: cgoFixedString(unsafe.Pointer(&stream.codec_name_utf8[0])),
			CodecLongName: cgoFixedString(unsafe.Pointer(&stream.codec_long_name_utf8[0])),
			CodecTag:      cgoFixedString(unsafe.Pointer(&stream.codec_tag_utf8[0])),
			Profile:       cgoFixedString(unsafe.Pointer(&stream.profile_utf8[0])),
			TimeBase:      cgoFixedString(unsafe.Pointer(&stream.time_base_utf8[0])),
			Disposition:   uint32(stream.disposition),
			Language:      cgoFixedString(unsafe.Pointer(&stream.language_utf8[0])),
			Title:         cgoFixedString(unsafe.Pointer(&stream.title_utf8[0])), TagsJSON: tags,
			PixelFormat: cgoFixedString(unsafe.Pointer(&stream.pixel_format_utf8[0])),
			SAR:         cgoFixedString(unsafe.Pointer(&stream.sar_utf8[0])), DAR: cgoFixedString(unsafe.Pointer(&stream.dar_utf8[0])),
			AvgFrameRate:   cgoFixedString(unsafe.Pointer(&stream.avg_frame_rate_utf8[0])),
			RealFrameRate:  cgoFixedString(unsafe.Pointer(&stream.real_frame_rate_utf8[0])),
			ColorRange:     cgoFixedString(unsafe.Pointer(&stream.color_range_utf8[0])),
			ColorSpace:     cgoFixedString(unsafe.Pointer(&stream.color_space_utf8[0])),
			ColorTransfer:  cgoFixedString(unsafe.Pointer(&stream.color_transfer_utf8[0])),
			ColorPrimaries: cgoFixedString(unsafe.Pointer(&stream.color_primaries_utf8[0])),
			ChromaLocation: cgoFixedString(unsafe.Pointer(&stream.chroma_location_utf8[0])),
			FieldOrder:     cgoFixedString(unsafe.Pointer(&stream.field_order_utf8[0])),
			SampleFormat:   cgoFixedString(unsafe.Pointer(&stream.sample_format_utf8[0])),
			ChannelLayout:  cgoFixedString(unsafe.Pointer(&stream.channel_layout_utf8[0])),
		}
		streamMask := uint64(stream.present_mask)
		if streamMask&uint64(C.VC_STREAM_HAS_LEVEL) != 0 {
			value.Level = int32Pointer(int32(stream.level))
		}
		if streamMask&uint64(C.VC_STREAM_HAS_START_TIME) != 0 {
			value.StartTimeUS = int64Pointer(int64(stream.start_time_us))
		}
		if streamMask&uint64(C.VC_STREAM_HAS_DURATION) != 0 {
			value.DurationUS = int64Pointer(int64(stream.duration_us))
		}
		if streamMask&uint64(C.VC_STREAM_HAS_BIT_RATE) != 0 {
			value.BitRate = int64Pointer(int64(stream.bit_rate))
		}
		if streamMask&uint64(C.VC_STREAM_HAS_FRAME_COUNT) != 0 {
			value.FrameCount = int64Pointer(int64(stream.frame_count))
		}
		if streamMask&uint64(C.VC_STREAM_HAS_BIT_DEPTH) != 0 {
			value.BitDepth = int32Pointer(int32(stream.bit_depth))
		}
		if streamMask&uint64(C.VC_STREAM_HAS_WIDTH) != 0 {
			value.Width = int32Pointer(int32(stream.width))
		}
		if streamMask&uint64(C.VC_STREAM_HAS_HEIGHT) != 0 {
			value.Height = int32Pointer(int32(stream.height))
		}
		if streamMask&uint64(C.VC_STREAM_HAS_ROTATION) != 0 {
			value.Rotation = int32Pointer(int32(stream.rotation))
		}
		if streamMask&uint64(C.VC_STREAM_HAS_SAMPLE_RATE) != 0 {
			value.SampleRate = int32Pointer(int32(stream.sample_rate))
		}
		if streamMask&uint64(C.VC_STREAM_HAS_CHANNELS) != 0 {
			value.Channels = int32Pointer(int32(stream.channels))
		}
		if streamMask&uint64(C.VC_STREAM_HAS_AUDIO_BIT_DEPTH) != 0 {
			value.AudioBitDepth = int32Pointer(int32(stream.audio_bit_depth))
		}
		result.Streams = append(result.Streams, value)
	}
	if err := proto.ValidateVideoMetadata(&result.Container, result.Streams); err != nil {
		return nil, fmt.Errorf("videocore: invalid native video metadata: %w", err)
	}
	return result, nil
}

func cgoMetadataJSON(session nativeSession, streamIndex int32) (string, error) {
	var required C.uint32_t
	var nativeErr C.vc_error
	C.go_vc_init_error(&nativeErr)
	rc := int32(C.vc_media_metadata_json(
		(*C.vc_media_session)(session.value), C.int32_t(streamIndex),
		nil, 0, &required, &nativeErr,
	))
	if rc != StatusOK {
		return "", cgoCallError("media metadata JSON size", rc, &nativeErr)
	}
	if required == 0 || uint64(required) > (64<<10)+1 {
		return "", &NativeError{Code: StatusOutputTooLarge, Message: "metadata JSON size exceeds protocol limit"}
	}
	buffer := make([]byte, int(required))
	C.go_vc_init_error(&nativeErr)
	rc = int32(C.vc_media_metadata_json(
		(*C.vc_media_session)(session.value), C.int32_t(streamIndex),
		(*C.char)(unsafe.Pointer(unsafe.SliceData(buffer))), required,
		&required, &nativeErr,
	))
	if rc != StatusOK {
		return "", cgoCallError("media metadata JSON", rc, &nativeErr)
	}
	if len(buffer) == 0 || buffer[len(buffer)-1] != 0 {
		return "", &NativeError{Code: StatusInternal, Message: "metadata JSON is not NUL terminated"}
	}
	return string(buffer[:len(buffer)-1]), nil
}

func cgoFixedString(pointer unsafe.Pointer) string {
	return C.GoString((*C.char)(pointer))
}

func cgoStreamMediaType(value uint32) (string, error) {
	switch value {
	case uint32(C.VC_STREAM_MEDIA_TYPE_VIDEO):
		return "video", nil
	case uint32(C.VC_STREAM_MEDIA_TYPE_AUDIO):
		return "audio", nil
	case uint32(C.VC_STREAM_MEDIA_TYPE_SUBTITLE):
		return "subtitle", nil
	case uint32(C.VC_STREAM_MEDIA_TYPE_DATA):
		return "data", nil
	case uint32(C.VC_STREAM_MEDIA_TYPE_ATTACHMENT):
		return "attachment", nil
	default:
		return "", &NativeError{Code: StatusInternal, Message: "native stream media type is unknown"}
	}
}

func int32Pointer(value int32) *int32 { return &value }
func int64Pointer(value int64) *int64 { return &value }

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
		worker.MaskVideoDuration | worker.MaskVideoContactSheet |
		worker.MaskVideoMetadata
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
	if fields&(worker.MaskVideoDuration|worker.MaskVideoMetadata) != 0 {
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
