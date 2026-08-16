package videocore

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime/cgo"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	"dedup/internal/worker"
)

const (
	ABIVersion uint32 = 2
	Version           = "2.0.0"
)

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
	Kind              worker.MediaKind
	ImageMemoryBytes  int64
	NativeTimeout     time.Duration
	IOGovernor        IOGovernor
	ioGovernorContext uintptr
}

type IOGovernor interface {
	BeforeRead(context.Context, int) (leaseID uint64, granted int, err error)
	AfterRead(leaseID uint64, bytes int, elapsed time.Duration, err error)
	BeforeSeek(context.Context) (leaseID uint64, err error)
	AfterSeek(leaseID uint64, elapsed time.Duration, err error)
}

const (
	ioOperationRead uint32 = 1
	ioOperationSeek uint32 = 2
)

type ioGovernorHandle struct {
	handle           cgo.Handle
	ctx              context.Context
	governor         IOGovernor
	once             sync.Once
	mu               sync.Mutex
	pendingLease     uint64
	pendingOperation uint32
}

func newIOGovernorHandle(ctx context.Context, governor IOGovernor) *ioGovernorHandle {
	if governor == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	owner := &ioGovernorHandle{ctx: ctx, governor: governor}
	owner.handle = cgo.NewHandle(owner)
	return owner
}

func (owner *ioGovernorHandle) Value() uintptr {
	if owner == nil {
		return 0
	}
	return uintptr(owner.handle)
}

func (owner *ioGovernorHandle) Delete() {
	if owner != nil {
		owner.once.Do(func() { owner.handle.Delete() })
	}
}

func invokeIOAcquire(contextValue uintptr, operation uint32, requested uint64) (
	leaseID uint64, granted uint64, status int32, message string,
) {
	status = StatusIO
	message = "I/O governor unavailable"
	defer func() {
		if recover() != nil {
			leaseID, granted = 0, 0
			status = StatusIO
			message = "I/O governor callback failed"
		}
	}()
	owner, ok := cgo.Handle(contextValue).Value().(*ioGovernorHandle)
	if !ok || owner == nil || owner.governor == nil {
		return
	}
	switch operation {
	case ioOperationRead:
		if requested == 0 || requested > uint64(math.MaxInt) {
			return 0, 0, StatusIO, "I/O governor read request is invalid"
		}
		id, allowed, err := owner.governor.BeforeRead(owner.ctx, int(requested))
		if err != nil {
			return 0, 0, governorErrorStatus(err), governorErrorMessage(err)
		}
		if id == 0 || allowed <= 0 || uint64(allowed) > requested {
			return 0, 0, StatusIO, "I/O governor returned an invalid grant"
		}
		owner.rememberOperation(id, operation)
		return id, uint64(allowed), StatusOK, ""
	case ioOperationSeek:
		id, err := owner.governor.BeforeSeek(owner.ctx)
		if err != nil {
			return 0, 0, governorErrorStatus(err), governorErrorMessage(err)
		}
		if id == 0 {
			return 0, 0, StatusIO, "I/O governor returned an invalid grant"
		}
		owner.rememberOperation(id, operation)
		return id, 0, StatusOK, ""
	default:
		return 0, 0, StatusIO, "I/O governor operation is invalid"
	}
}

func invokeIOReport(contextValue uintptr, leaseID, actualBytes, elapsedNS uint64, status int32) {
	defer func() { _ = recover() }()
	owner, ok := cgo.Handle(contextValue).Value().(*ioGovernorHandle)
	if !ok || owner == nil || owner.governor == nil {
		return
	}
	operation := owner.takeOperation(leaseID)
	elapsed := time.Duration(elapsedNS)
	if elapsedNS > uint64(math.MaxInt64) {
		elapsed = time.Duration(math.MaxInt64)
	}
	operationErr := governorOperationError(status)
	switch operation {
	case ioOperationRead:
		bytes := int(actualBytes)
		if actualBytes > uint64(math.MaxInt) {
			bytes = 0
			operationErr = &NativeError{Code: StatusIO, Message: "source I/O byte count is invalid"}
		}
		owner.governor.AfterRead(leaseID, bytes, elapsed, operationErr)
	case ioOperationSeek:
		owner.governor.AfterSeek(leaseID, elapsed, operationErr)
	}
}

func (owner *ioGovernorHandle) rememberOperation(leaseID uint64, operation uint32) {
	owner.mu.Lock()
	owner.pendingLease = leaseID
	owner.pendingOperation = operation
	owner.mu.Unlock()
}

func (owner *ioGovernorHandle) takeOperation(leaseID uint64) uint32 {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.pendingLease != leaseID {
		return 0
	}
	operation := owner.pendingOperation
	owner.pendingLease = 0
	owner.pendingOperation = 0
	return operation
}

func governorErrorStatus(err error) int32 {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return StatusCancelled
	}
	return StatusIO
}

func governorErrorMessage(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "I/O governor cancelled"
	}
	return "I/O governor unavailable"
}

func governorOperationError(status int32) error {
	switch status {
	case StatusOK:
		return nil
	case StatusCancelled:
		return context.Canceled
	case StatusTimeout:
		return context.DeadlineExceeded
	default:
		return &NativeError{Code: status, Message: "source I/O failed"}
	}
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
	if err != nil || major != 2 {
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
	governor := newIOGovernorHandle(ctx, options.IOGovernor)
	if governor != nil {
		options.ioGovernorContext = governor.Value()
	}
	keepGovernor := false
	defer func() {
		if !keepGovernor {
			governor.Delete()
		}
	}()
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
	keepGovernor = true
	return newSession(handle, cancel, bridge, governor), nil
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
	mu       sync.Mutex
	handle   nativeSession
	cancel   nativeCancel
	bridge   nativeBridge
	governor *ioGovernorHandle
}

func newSession(handle nativeSession, cancel nativeCancel, bridge nativeBridge, governors ...*ioGovernorHandle) *Session {
	var governor *ioGovernorHandle
	if len(governors) != 0 {
		governor = governors[0]
	}
	return &Session{handle: handle, cancel: cancel, bridge: bridge, governor: governor}
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
	handle, cancel, bridge, governor := s.handle, s.cancel, s.bridge, s.governor
	s.handle = nativeSession{}
	s.cancel = nativeCancel{}
	s.governor = nil
	defer governor.Delete()
	defer bridge.cancelFree(cancel)
	bridge.close(handle)
	return nil
}
