package videocore

import "time"

const (
	StatusOK             int32 = 0
	StatusInvalidArg     int32 = -1
	StatusABI            int32 = -2
	StatusOOM            int32 = -3
	StatusIO             int32 = -4
	StatusUnsupported    int32 = -5
	StatusDemux          int32 = -6
	StatusDecode         int32 = -7
	StatusEncode         int32 = -8
	StatusNoFrame        int32 = -9
	StatusOutputTooLarge int32 = -10
	StatusCancelled      int32 = -11
	StatusTimeout        int32 = -12
	StatusStale          int32 = -13
	StatusInternal       int32 = -99
)

type AnalysisRequest struct {
	Fields          uint32
	FrameMask       uint8
	KnownDurationMS int64
	ProbeTimeout    time.Duration
	FrameTimeout    time.Duration
	TileMaxSide     int32
	TempJPEGPath    string
}

type FrameResult struct {
	StandardIndex uint32
	Status        int32
	SampleTimeMS  int64
	Features      FeatureSet
}

type AnalysisResult struct {
	MediaType            uint32
	DurationMS           int64
	DurationStatus       int32
	ImageStatus          int32
	ContactSheetStatus   int32
	ContactSheetWidth    uint32
	ContactSheetHeight   uint32
	CompletedFrameMask   uint8
	ImageFeatures        FeatureSet
	ContactSheetFeatures FeatureSet
	Frames               [6]FrameResult
	OperationElapsedMS   uint64
	DecodeElapsedMS      uint64
	ImageWidth           uint32
	ImageHeight          uint32
}

type nativeFrameResult struct {
	standardIndex uint32
	status        int32
	sampleTimeMS  int64
	features      nativeFeatureSet
}

type nativeAnalysisResult struct {
	mediaType          uint32
	durationMS         int64
	durationStatus     int32
	imageStatus        int32
	contactStatus      int32
	contactWidth       uint32
	contactHeight      uint32
	completedFrameMask uint8
	imageFeatures      nativeFeatureSet
	contactFeatures    nativeFeatureSet
	frames             [6]nativeFrameResult
	operationElapsedMS uint64
	decodeElapsedMS    uint64
	imageWidth         uint32
	imageHeight        uint32
}

func analysisResultFromNative(native nativeAnalysisResult) AnalysisResult {
	result := AnalysisResult{
		MediaType:            native.mediaType,
		DurationMS:           native.durationMS,
		DurationStatus:       native.durationStatus,
		ImageStatus:          native.imageStatus,
		ContactSheetStatus:   native.contactStatus,
		ContactSheetWidth:    native.contactWidth,
		ContactSheetHeight:   native.contactHeight,
		CompletedFrameMask:   native.completedFrameMask,
		ImageFeatures:        featureSetFromNative(native.imageFeatures),
		ContactSheetFeatures: featureSetFromNative(native.contactFeatures),
		OperationElapsedMS:   native.operationElapsedMS,
		DecodeElapsedMS:      native.decodeElapsedMS,
		ImageWidth:           native.imageWidth,
		ImageHeight:          native.imageHeight,
	}
	for index := range result.Frames {
		frame := native.frames[index]
		result.Frames[index] = FrameResult{
			StandardIndex: frame.standardIndex,
			Status:        frame.status,
			SampleTimeMS:  frame.sampleTimeMS,
			Features:      featureSetFromNative(frame.features),
		}
	}
	return result
}
