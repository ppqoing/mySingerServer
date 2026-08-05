package videocore

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	"dedup/internal/worker"
)

const ABIVersion uint32 = 1

var (
	ErrUnavailable   = errors.New("videocore: cgo Windows binding unavailable")
	ErrABIMismatch   = errors.New("videocore: ABI or component major version mismatch")
	ErrInvalidPath   = errors.New("videocore: path is empty or contains embedded NUL")
	ErrSessionClosed = errors.New("videocore: media session is closed")
)

type NativeError struct {
	Code       int32
	FFmpegCode int32
	Win32Code  uint32
	Message    string
}

func (e *NativeError) Error() string {
	if e == nil {
		return "<nil>"
	}
	message := e.Message
	if message == "" {
		message = "native error"
	}
	return fmt.Sprintf("videocore: native error code=%d ffmpeg=%d win32=%d: %s", e.Code, e.FFmpegCode, e.Win32Code, message)
}

func nativeCallError(code, ffmpegCode int32, win32Code uint32, message string) error {
	if code == StatusOK {
		return nil
	}
	return &NativeError{
		Code:       code,
		FFmpegCode: ffmpegCode,
		Win32Code:  win32Code,
		Message:    message,
	}
}

type RuntimeComponent struct {
	Name           string
	HeaderVersion  uint32
	RuntimeVersion uint32
}

type RuntimeInfo struct {
	ABI           uint32
	Version       string
	FFmpegBuildID string
	Components    [4]RuntimeComponent
}

type OpenOptions struct {
	Kind             worker.MediaKind
	ImageMemoryBytes int64
	NativeTimeout    time.Duration
}

type nativeSession struct{ value unsafe.Pointer }
type nativeCancel struct{ value unsafe.Pointer }

type nativeBridge interface {
	runtime() (RuntimeInfo, error)
	cancelCreate() (nativeCancel, error)
	cancelRequest(nativeCancel)
	cancelFree(nativeCancel)
	open([]uint16, OpenOptions, nativeCancel) (nativeSession, error)
	hash(nativeSession) ([64]byte, error)
	analyze(nativeSession, AnalysisRequest) (AnalysisResult, error)
	close(nativeSession)
}

var defaultNative nativeBridge = platformNativeBridge()

func Runtime() (RuntimeInfo, error) {
	return runtimeWith(defaultNative)
}

func runtimeWith(bridge nativeBridge) (RuntimeInfo, error) {
	info, err := bridge.runtime()
	if err != nil {
		return RuntimeInfo{}, err
	}
	if info.ABI != ABIVersion {
		return RuntimeInfo{}, fmt.Errorf("%w: runtime ABI=%d, binding ABI=%d", ErrABIMismatch, info.ABI, ABIVersion)
	}
	major, err := versionMajor(info.Version)
	if err != nil || major != 1 {
		return RuntimeInfo{}, fmt.Errorf("%w: runtime version %q", ErrABIMismatch, info.Version)
	}
	for _, component := range info.Components {
		if component.Name == "" || component.HeaderVersion>>16 != component.RuntimeVersion>>16 {
			return RuntimeInfo{}, fmt.Errorf(
				"%w: component %q header=%#x runtime=%#x",
				ErrABIMismatch, component.Name, component.HeaderVersion, component.RuntimeVersion,
			)
		}
	}
	return info, nil
}

func versionMajor(version string) (uint64, error) {
	part, _, _ := strings.Cut(version, ".")
	return strconv.ParseUint(part, 10, 32)
}

func Open(ctx context.Context, path string, options OpenOptions) (*Session, error) {
	return openWith(ctx, path, options, defaultNative)
}

func openWith(ctx context.Context, path string, options OpenOptions, bridge nativeBridge) (*Session, error) {
	units, err := utf16Path(path)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cancel, err := bridge.cancelCreate()
	if err != nil {
		return nil, err
	}
	keepCancel := false
	defer func() {
		if !keepCancel {
			bridge.cancelFree(cancel)
		}
	}()

	nativeDone := make(chan struct{})
	watchDone := make(chan struct{})
	var requestOnce sync.Once
	requestCancel := func() { requestOnce.Do(func() { bridge.cancelRequest(cancel) }) }
	go func() {
		select {
		case <-ctx.Done():
			requestCancel()
		case <-nativeDone:
		}
		close(watchDone)
	}()
	handle, nativeErr := bridge.open(units, options, cancel)
	close(nativeDone)
	<-watchDone
	if contextErr := ctx.Err(); contextErr != nil {
		requestCancel()
		if handle.value != nil {
			bridge.close(handle)
		}
		return nil, contextErr
	}
	if nativeErr != nil {
		if handle.value != nil {
			bridge.close(handle)
		}
		return nil, nativeErr
	}
	if handle.value == nil {
		return nil, &NativeError{Code: StatusInternal, Message: "native open returned a nil session"}
	}
	keepCancel = true
	return newSession(handle, cancel, bridge), nil
}

func utf16Path(path string) ([]uint16, error) {
	if path == "" || strings.IndexByte(path, 0) >= 0 {
		return nil, ErrInvalidPath
	}
	units := utf16.Encode([]rune(path))
	if len(units) == 0 || uint64(len(units)) > uint64(^uint32(0)) {
		return nil, ErrInvalidPath
	}
	return units, nil
}

type Session struct {
	mu     sync.Mutex
	handle nativeSession
	cancel nativeCancel
	bridge nativeBridge
}

func newSession(handle nativeSession, cancel nativeCancel, bridge nativeBridge) *Session {
	return &Session{handle: handle, cancel: cancel, bridge: bridge}
}

func (s *Session) Hash() ([64]byte, error) {
	var empty [64]byte
	if s == nil {
		return empty, ErrSessionClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handle.value == nil {
		return empty, ErrSessionClosed
	}
	return s.bridge.hash(s.handle)
}

func (s *Session) Analyze(ctx context.Context, request AnalysisRequest) (AnalysisResult, error) {
	if s == nil {
		return AnalysisResult{}, ErrSessionClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handle.value == nil {
		return AnalysisResult{}, ErrSessionClosed
	}
	if err := ctx.Err(); err != nil {
		return AnalysisResult{}, err
	}
	if request.TempJPEGPath != "" {
		if _, err := utf16Path(request.TempJPEGPath); err != nil {
			return AnalysisResult{}, err
		}
	}

	nativeDone := make(chan struct{})
	watchDone := make(chan struct{})
	var requestOnce sync.Once
	requestCancel := func() { requestOnce.Do(func() { s.bridge.cancelRequest(s.cancel) }) }
	go func() {
		select {
		case <-ctx.Done():
			requestCancel()
		case <-nativeDone:
		}
		close(watchDone)
	}()
	result, nativeErr := s.bridge.analyze(s.handle, request)
	close(nativeDone)
	<-watchDone
	if contextErr := ctx.Err(); contextErr != nil {
		requestCancel()
		return AnalysisResult{}, contextErr
	}
	return result, nativeErr
}

func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handle.value == nil {
		return nil
	}
	s.bridge.close(s.handle)
	s.bridge.cancelFree(s.cancel)
	s.handle = nativeSession{}
	s.cancel = nativeCancel{}
	return nil
}
