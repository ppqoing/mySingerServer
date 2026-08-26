//go:build windows

package elevation

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"
	"unsafe"

	nodeprocess "dedup/internal/nodetray/process"
	"golang.org/x/sys/windows"
)

var errParentExited = errors.New("elevation: parent process exited")

type Handler interface {
	Execute(context.Context, Request) Response
}

// HandlerFactory is invoked only after the connected ordinary parent has
// passed the same-final-image identity check. It lets elevated composition
// freeze authority derived from that validated process rather than from the
// elevated administrator token.
type HandlerFactory func(nodeprocess.Identity) (Handler, error)

type serverPlatform interface {
	Dial(context.Context, string) (peerConnection, error)
}

func ServeOnceWithHandlerFactory(ctx context.Context, pipeName, nonce string, inspector nodeprocess.Inspector, factory HandlerFactory) error {
	if inspector == nil {
		inspector = nodeprocess.NewInspector()
	}
	return serveOnceWithBackendFactory(ctx, pipeName, nonce, inspector, nativeServerPlatform{}, factory, defaultOneShotTimeout)
}

func serveOnceWithBackend(
	ctx context.Context,
	pipeName string,
	nonce string,
	inspector nodeprocess.Inspector,
	platform serverPlatform,
	handler Handler,
	timeout time.Duration,
) error {
	if handler == nil {
		return errors.New("elevation: server dependencies are invalid")
	}
	return serveOnceWithBackendFactory(ctx, pipeName, nonce, inspector, platform, func(nodeprocess.Identity) (Handler, error) {
		return handler, nil
	}, timeout)
}

func serveOnceWithBackendFactory(
	ctx context.Context,
	pipeName string,
	nonce string,
	inspector nodeprocess.Inspector,
	platform serverPlatform,
	factory HandlerFactory,
	timeout time.Duration,
) error {
	if ctx == nil || inspector == nil || platform == nil || factory == nil || timeout <= 0 {
		return errors.New("elevation: server dependencies are invalid")
	}
	if err := ValidateNonce(nonce); err != nil {
		return err
	}
	if pipeName != `\\.\pipe\mysingerserver-elevate-`+nonce {
		return errors.New("elevation: pipe name does not match nonce")
	}
	sessionCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	connection, err := platform.Dial(sessionCtx, pipeName)
	if err != nil {
		return stableContextError(sessionCtx, "pipe connection failed")
	}
	defer connection.Close()
	if deadline, ok := sessionCtx.Deadline(); ok {
		if err := connection.SetDeadline(deadline); err != nil {
			return errors.New("elevation: pipe deadline setup failed")
		}
	}

	self, err := inspector.Inspect(os.Getpid())
	if err != nil || self.PID != os.Getpid() || self.StartedAtUnixMS <= 0 || self.ExecutablePath == "" {
		return errors.New("elevation: elevated identity validation failed")
	}
	peerPID, err := connection.PeerPID()
	if err != nil || peerPID <= 0 {
		return errors.New("elevation: parent process identity unavailable")
	}
	parent, err := inspector.Inspect(peerPID)
	if err != nil || parent.StartedAtUnixMS <= 0 || !sameFinalImage(self.ExecutablePath, parent.ExecutablePath) {
		return errors.New("elevation: parent process identity mismatch")
	}
	handler, err := factory(parent)
	if err != nil || handler == nil {
		return errors.New("elevation: action authority unavailable")
	}

	parentExited := make(chan error, 1)
	go func() {
		_, waitErr := inspector.Wait(sessionCtx, parent)
		parentExited <- waitErr
		cancel()
	}()
	guard, err := NewOneShotGuard(nonce)
	if err != nil {
		return err
	}

	requestDone := make(chan struct {
		request Request
		err     error
	}, 1)
	go func() {
		request, readErr := ReadRequestFrame(connection)
		requestDone <- struct {
			request Request
			err     error
		}{request: request, err: readErr}
	}()
	var request Request
	select {
	case waitErr := <-parentExited:
		return parentWaitError(sessionCtx, waitErr)
	case result := <-requestDone:
		if result.err != nil {
			return stableContextError(sessionCtx, "request validation failed")
		}
		request = result.request
	case <-sessionCtx.Done():
		select {
		case waitErr := <-parentExited:
			return parentWaitError(sessionCtx, waitErr)
		default:
			return sessionCtx.Err()
		}
	}
	if err := guard.Consume(request.Nonce); err != nil {
		return err
	}
	select {
	case waitErr := <-parentExited:
		return parentWaitError(sessionCtx, waitErr)
	default:
	}
	if err := sessionCtx.Err(); err != nil {
		return err
	}
	response := handler.Execute(sessionCtx, request)
	select {
	case waitErr := <-parentExited:
		return parentWaitError(sessionCtx, waitErr)
	default:
	}
	if err := sessionCtx.Err(); err != nil {
		return err
	}
	if err := ValidateResponse(request.Nonce, response); err != nil {
		response = Response{
			Version:      ProtocolVersion,
			Nonce:        request.Nonce,
			ErrorCode:    ErrorCodeInternal,
			ErrorSummary: "action failed",
		}
	}
	if err := WriteResponseFrame(connection, response); err != nil {
		return stableContextError(sessionCtx, "response write failed")
	}
	return nil
}

func parentWaitError(sessionCtx context.Context, waitErr error) error {
	if waitErr == nil {
		return errParentExited
	}
	if (errors.Is(waitErr, context.Canceled) || errors.Is(waitErr, context.DeadlineExceeded)) && sessionCtx.Err() != nil {
		return sessionCtx.Err()
	}
	return errors.New("elevation: parent process wait failed")
}

type deadlinePipeFile interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Close() error
	SetDeadline(time.Time) error
}

type windowsPipeConnection struct {
	file     deadlinePipeFile
	handle   windows.Handle
	peerKind uint8
}

const (
	peerIsClient uint8 = iota
	peerIsServer
)

func (connection *windowsPipeConnection) Read(buffer []byte) (int, error) {
	return connection.file.Read(buffer)
}

func (connection *windowsPipeConnection) Write(buffer []byte) (int, error) {
	return connection.file.Write(buffer)
}

func (connection *windowsPipeConnection) Close() error {
	if connection == nil || connection.file == nil {
		return nil
	}
	err := connection.file.Close()
	connection.file = nil
	connection.handle = 0
	return err
}

func (connection *windowsPipeConnection) SetDeadline(deadline time.Time) error {
	return connection.file.SetDeadline(deadline)
}

func (connection *windowsPipeConnection) PeerPID() (int, error) {
	if connection == nil || connection.handle == 0 {
		return 0, errors.New("elevation: pipe handle is closed")
	}
	var pid uint32
	var err error
	if connection.peerKind == peerIsClient {
		err = windows.GetNamedPipeClientProcessId(connection.handle, &pid)
	} else {
		err = windows.GetNamedPipeServerProcessId(connection.handle, &pid)
	}
	if err != nil || pid == 0 {
		return 0, errors.New("elevation: pipe peer PID unavailable")
	}
	return int(pid), nil
}

type windowsPipeListener struct {
	mu       sync.Mutex
	handle   windows.Handle
	pipeName string
}

func newWindowsPipeListener(ctx context.Context, pipeName, sddl string) (*windowsPipeListener, error) {
	if ctx == nil || ctx.Err() != nil || pipeName == "" || sddl == "" {
		return nil, errors.New("elevation: invalid pipe listener request")
	}
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, errors.New("elevation: invalid pipe security descriptor")
	}
	attributes := windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}
	name, err := windows.UTF16PtrFromString(pipeName)
	if err != nil {
		return nil, errors.New("elevation: invalid pipe name")
	}
	handle, err := windows.CreateNamedPipe(
		name,
		windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_FIRST_PIPE_INSTANCE|windows.FILE_FLAG_OVERLAPPED,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
		1,
		uint32(MaxFrameSize+4),
		uint32(MaxFrameSize+4),
		0,
		&attributes,
	)
	if err != nil {
		return nil, errors.New("elevation: create pipe failed")
	}
	return &windowsPipeListener{handle: handle, pipeName: pipeName}, nil
}

func (listener *windowsPipeListener) Accept(ctx context.Context) (peerConnection, error) {
	if ctx == nil {
		return nil, errors.New("elevation: accept context is invalid")
	}
	listener.mu.Lock()
	handle := listener.handle
	listener.mu.Unlock()
	if handle == 0 {
		return nil, errors.New("elevation: listener is closed")
	}
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return nil, errors.New("elevation: create accept event failed")
	}
	defer windows.CloseHandle(event)
	overlapped := windows.Overlapped{HEvent: event}
	connectErr := windows.ConnectNamedPipe(handle, &overlapped)
	switch {
	case connectErr == nil, errors.Is(connectErr, windows.ERROR_PIPE_CONNECTED):
	case errors.Is(connectErr, windows.ERROR_IO_PENDING):
		if err := waitForPipeConnection(ctx, handle, &overlapped); err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("elevation: accept pipe failed")
	}
	listener.mu.Lock()
	if listener.handle != handle {
		listener.mu.Unlock()
		return nil, errors.New("elevation: listener closed during accept")
	}
	listener.handle = 0
	listener.mu.Unlock()
	file := os.NewFile(uintptr(handle), listener.pipeName)
	if file == nil {
		windows.CloseHandle(handle)
		return nil, errors.New("elevation: wrap pipe handle failed")
	}
	return &windowsPipeConnection{file: file, handle: handle, peerKind: peerIsClient}, nil
}

func waitForPipeConnection(ctx context.Context, handle windows.Handle, overlapped *windows.Overlapped) error {
	const pollIntervalMS = 20
	for {
		status, err := windows.WaitForSingleObject(overlapped.HEvent, pollIntervalMS)
		if err != nil {
			_ = windows.CancelIoEx(handle, overlapped)
			return errors.New("elevation: accept wait failed")
		}
		switch status {
		case windows.WAIT_OBJECT_0:
			var transferred uint32
			if err := windows.GetOverlappedResult(handle, overlapped, &transferred, false); err != nil {
				return errors.New("elevation: accept pipe failed")
			}
			return nil
		case uint32(windows.WAIT_TIMEOUT):
			select {
			case <-ctx.Done():
				cancelErr := windows.CancelIoEx(handle, overlapped)
				if cancelErr != nil && !errors.Is(cancelErr, windows.ERROR_NOT_FOUND) {
					return errors.New("elevation: accept cancellation failed")
				}
				_, _ = windows.WaitForSingleObject(overlapped.HEvent, windows.INFINITE)
				var transferred uint32
				_ = windows.GetOverlappedResult(handle, overlapped, &transferred, false)
				return ctx.Err()
			default:
			}
		default:
			_ = windows.CancelIoEx(handle, overlapped)
			return errors.New("elevation: accept wait failed")
		}
	}
}

func (listener *windowsPipeListener) Close() error {
	if listener == nil {
		return nil
	}
	listener.mu.Lock()
	defer listener.mu.Unlock()
	if listener.handle == 0 {
		return nil
	}
	err := windows.CloseHandle(listener.handle)
	listener.handle = 0
	return err
}

type nativeDialBackend interface {
	Open(pipeName string, flags uint32) (deadlinePipeFile, windows.Handle, error)
}

type nativeServerPlatform struct {
	backend nativeDialBackend
}

func (platform nativeServerPlatform) Dial(ctx context.Context, pipeName string) (peerConnection, error) {
	if ctx == nil || pipeName == "" {
		return nil, errors.New("elevation: invalid pipe dial request")
	}
	backend := platform.backend
	if backend == nil {
		backend = nativeDialBackendImpl{}
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		file, handle, openErr := backend.Open(pipeName, windows.FILE_FLAG_OVERLAPPED)
		if openErr == nil {
			if file == nil || handle == 0 {
				if file != nil {
					_ = file.Close()
				}
				return nil, errors.New("elevation: wrap pipe handle failed")
			}
			return &windowsPipeConnection{file: file, handle: handle, peerKind: peerIsServer}, nil
		}
		if !errors.Is(openErr, windows.ERROR_PIPE_BUSY) && !errors.Is(openErr, windows.ERROR_FILE_NOT_FOUND) {
			return nil, errors.New("elevation: open pipe failed")
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

type nativeDialBackendImpl struct{}

func (nativeDialBackendImpl) Open(pipeName string, flags uint32) (deadlinePipeFile, windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(pipeName)
	if err != nil {
		return nil, 0, errors.New("elevation: invalid pipe name")
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		0,
		nil,
		windows.OPEN_EXISTING,
		flags,
		0,
	)
	if err != nil {
		return nil, 0, err
	}
	file := os.NewFile(uintptr(handle), pipeName)
	if file == nil {
		windows.CloseHandle(handle)
		return nil, 0, errors.New("elevation: wrap pipe handle failed")
	}
	return file, handle, nil
}
