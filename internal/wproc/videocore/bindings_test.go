package videocore

import (
	"context"
	"errors"
	"sync"
	"testing"
	"unsafe"

	"dedup/internal/worker"
)

type fakeNativeBridge struct {
	runtimeInfo RuntimeInfo
	runtimeErr  error

	mu              sync.Mutex
	openCalls       int
	closeCalls      int
	cancelCreates   int
	cancelRequests  int
	cancelFrees     int
	analyzeStarted  chan struct{}
	analyzeRelease  chan struct{}
	analyzeResult   AnalysisResult
	analyzeErr      error
	openErr         error
	openPanic       any
	openEntered     chan struct{}
	openRelease     chan struct{}
	lastOpenOptions OpenOptions
}

func (f *fakeNativeBridge) runtime() (RuntimeInfo, error) {
	return f.runtimeInfo, f.runtimeErr
}

func (f *fakeNativeBridge) cancelCreate() (nativeCancel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelCreates++
	return nativeCancel{value: unsafe.Pointer(new(byte))}, nil
}

func (f *fakeNativeBridge) cancelRequest(nativeCancel) {
	f.mu.Lock()
	f.cancelRequests++
	f.mu.Unlock()
}

func (f *fakeNativeBridge) cancelFree(nativeCancel) {
	f.mu.Lock()
	f.cancelFrees++
	f.mu.Unlock()
}

func (f *fakeNativeBridge) open(_ []uint16, options OpenOptions, _ nativeCancel) (nativeSession, error) {
	f.mu.Lock()
	f.openCalls++
	f.lastOpenOptions = options
	f.mu.Unlock()
	if f.openEntered != nil {
		close(f.openEntered)
	}
	if f.openRelease != nil {
		<-f.openRelease
	}
	if f.openPanic != nil {
		panic(f.openPanic)
	}
	if f.openErr != nil {
		return nativeSession{}, f.openErr
	}
	return nativeSession{value: unsafe.Pointer(new(byte))}, nil
}

func (f *fakeNativeBridge) hash(nativeSession) ([64]byte, error) {
	return [64]byte{}, nil
}

func (f *fakeNativeBridge) analyze(nativeSession, AnalysisRequest) (AnalysisResult, error) {
	if f.analyzeStarted != nil {
		close(f.analyzeStarted)
	}
	if f.analyzeRelease != nil {
		<-f.analyzeRelease
	}
	return f.analyzeResult, f.analyzeErr
}

func (f *fakeNativeBridge) close(nativeSession) {
	f.mu.Lock()
	f.closeCalls++
	f.mu.Unlock()
}

func (f *fakeNativeBridge) counts() (closeCalls, cancelRequests, cancelFrees int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeCalls, f.cancelRequests, f.cancelFrees
}

func TestRuntimeRejectsMajorMismatch(t *testing.T) {
	bridge := &fakeNativeBridge{runtimeInfo: RuntimeInfo{
		ABI:     ABIVersion,
		Version: "2.0.0",
		Components: [4]RuntimeComponent{
			{Name: "avformat", HeaderVersion: 61 << 16, RuntimeVersion: 60 << 16},
			{Name: "avcodec", HeaderVersion: 61 << 16, RuntimeVersion: 61 << 16},
			{Name: "avutil", HeaderVersion: 59 << 16, RuntimeVersion: 59 << 16},
			{Name: "swscale", HeaderVersion: 8 << 16, RuntimeVersion: 8 << 16},
		},
	}}

	if _, err := runtimeWith(bridge); !errors.Is(err, ErrABIMismatch) {
		t.Fatalf("runtimeWith major mismatch error = %v, want ErrABIMismatch", err)
	}
}

func TestRuntimeRejectsV1(t *testing.T) {
	bridge := &fakeNativeBridge{runtimeInfo: RuntimeInfo{ABI: 1, Version: "1.0.0"}}
	if _, err := runtimeWith(bridge); !errors.Is(err, ErrABIMismatch) {
		t.Fatalf("runtimeWith v1 error = %v, want ErrABIMismatch", err)
	}
}

func TestOpenRejectsEmbeddedNUL(t *testing.T) {
	bridge := &fakeNativeBridge{}
	got, err := openWith(context.Background(), `D:\media\bad`+"\x00"+`.mp4`, OpenOptions{
		Kind: worker.MediaVideo,
	}, bridge)
	if got != nil {
		_ = got.Close()
		t.Fatalf("openWith embedded NUL returned session %#v", got)
	}
	if !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("openWith embedded NUL error = %v, want ErrInvalidPath", err)
	}
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if bridge.openCalls != 0 || bridge.cancelCreates != 0 {
		t.Fatalf("embedded NUL reached native boundary: open=%d cancel_create=%d", bridge.openCalls, bridge.cancelCreates)
	}
}

func TestAnalysisResultMapsFixedSixFrames(t *testing.T) {
	native := nativeAnalysisResult{
		mediaType:          2,
		durationMS:         12_345,
		durationStatus:     StatusOK,
		imageStatus:        StatusUnsupported,
		contactStatus:      StatusOK,
		contactWidth:       960,
		contactHeight:      540,
		completedFrameMask: 0x25,
		operationElapsedMS: 321,
		decodeElapsedMS:    123,
	}
	for index := range native.frames {
		native.frames[index] = nativeFrameResult{
			standardIndex: uint32(index),
			status:        int32(-index),
			sampleTimeMS:  int64(index) * 1_000,
			features: nativeFeatureSet{
				pdqQuality: uint32(80 + index),
				pdq:        [PDQBytes]byte{byte(index + 1)},
				phash:      [PHashCount]uint64{uint64(index + 10)},
				sobel:      [SobelHistCount]float32{float32(index) + 0.5},
			},
		}
	}

	got := analysisResultFromNative(native)
	if got.DurationMS != 12_345 || got.CompletedFrameMask != 0x25 || len(got.Frames) != 6 {
		t.Fatalf("top-level result = %#v", got)
	}
	for index, frame := range got.Frames {
		if frame.StandardIndex != uint32(index) || frame.Status != int32(-index) || frame.SampleTimeMS != int64(index)*1_000 {
			t.Fatalf("frame slot %d mapping = %#v", index, frame)
		}
		if frame.Features.PDQ[0] != byte(index+1) || frame.Features.PHash[0] != uint64(index+10) || frame.Features.SobelHistogram[0] != float32(index)+0.5 {
			t.Fatalf("frame slot %d feature mapping = %#v", index, frame.Features)
		}
	}
}

func TestNativeErrorPreservesAllNativeCodes(t *testing.T) {
	err := nativeCallError(StatusDecode, -1_099_499_552, 32, "decoder failed")
	var nativeErr *NativeError
	if !errors.As(err, &nativeErr) {
		t.Fatalf("nativeCallError type = %T, want *NativeError", err)
	}
	if nativeErr.Code != StatusDecode || nativeErr.FFmpegCode != -1_099_499_552 || nativeErr.Win32Code != 32 || nativeErr.Message != "decoder failed" {
		t.Fatalf("NativeError fields = %#v", nativeErr)
	}
}

func TestUTF16PathUsesExplicitCodeUnitLength(t *testing.T) {
	units, err := utf16Path(`D:\媒体\😀.mp4`)
	if err != nil {
		t.Fatal(err)
	}
	// The non-BMP rune occupies exactly two UTF-16 code units. No trailing
	// terminator is included because the ABI carries an explicit unit count.
	if got, want := len(units), 12; got != want {
		t.Fatalf("UTF-16 units = %d, want %d (%#v)", got, want, units)
	}
	if units[len(units)-1] == 0 {
		t.Fatal("UTF-16 path includes a trailing NUL despite explicit length")
	}
}
