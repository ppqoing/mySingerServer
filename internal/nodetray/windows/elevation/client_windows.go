//go:build windows

package elevation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	nodeprocess "dedup/internal/nodetray/process"
	"golang.org/x/sys/windows"
)

const defaultOneShotTimeout = 30 * time.Second

var errPlatformUACCancelled = errors.New("elevation: UAC was cancelled")

type InvocationResult struct {
	Response     Response
	UACCancelled bool
}

type peerConnection interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Close() error
	SetDeadline(time.Time) error
	PeerPID() (int, error)
}

type oneShotListener interface {
	Accept(context.Context) (peerConnection, error)
	Close() error
}

type processHandle interface {
	RawHandle() uintptr
	Close() error
}

type clientPlatform interface {
	Listen(ctx context.Context, pipeName, sddl string) (oneShotListener, error)
	LaunchRunas(verb, executable, arguments string, requestProcessHandle bool) (processHandle, error)
}

type Client struct {
	executable string
	self       nodeprocess.Identity
	inspector  nodeprocess.Inspector
	handles    nodeprocess.HandleInspector
	platform   clientPlatform
	timeout    time.Duration
}

func NewClient(executable string, inspector nodeprocess.Inspector) (*Client, error) {
	if inspector == nil {
		inspector = nodeprocess.NewInspector()
	}
	return newClientWithBackend(executable, inspector, nativeClientPlatform{}, defaultOneShotTimeout)
}

func newClientWithBackend(executable string, inspector nodeprocess.Inspector, platform clientPlatform, timeout time.Duration) (*Client, error) {
	if inspector == nil || platform == nil || timeout <= 0 {
		return nil, errors.New("elevation: client dependencies are invalid")
	}
	handleInspector, ok := inspector.(nodeprocess.HandleInspector)
	if !ok {
		return nil, errors.New("elevation: process handle inspector is required")
	}
	cleaned, err := filepath.Abs(executable)
	if err != nil || !strings.EqualFold(filepath.Base(cleaned), "nodetray.exe") {
		return nil, errors.New("elevation: executable must be nodetray.exe")
	}
	cleaned = filepath.Clean(cleaned)
	self, err := inspector.Inspect(os.Getpid())
	if err != nil || self.PID != os.Getpid() || self.StartedAtUnixMS <= 0 ||
		!sameFinalImage(cleaned, self.ExecutablePath) {
		return nil, errors.New("elevation: current process identity validation failed")
	}
	return &Client{executable: cleaned, self: self, inspector: inspector, handles: handleInspector, platform: platform, timeout: timeout}, nil
}

func (client *Client) Invoke(ctx context.Context, action Action, payload []byte) (InvocationResult, error) {
	if client == nil || ctx == nil {
		return InvocationResult{}, errors.New("elevation: client and context are required")
	}
	nonce, err := NewNonce()
	if err != nil {
		return InvocationResult{}, err
	}
	request := Request{Version: ProtocolVersion, Nonce: nonce, Action: action, Payload: append([]byte(nil), payload...)}
	if err := ValidateRequest(request); err != nil {
		return InvocationResult{}, err
	}
	sessionCtx, cancel := context.WithTimeout(ctx, client.timeout)
	defer cancel()

	pipeName := `\\.\pipe\mysingerserver-elevate-` + nonce
	sddl, err := currentUserPipeSDDL()
	if err != nil {
		return InvocationResult{}, errors.New("elevation: pipe security setup failed")
	}
	listener, err := client.platform.Listen(sessionCtx, pipeName, sddl)
	if err != nil {
		return InvocationResult{}, stableContextError(sessionCtx, "pipe listener failed")
	}
	defer listener.Close()

	arguments := `--elevated-once --pipe "` + pipeName + `" --nonce "` + nonce + `"`
	childHandle, err := client.platform.LaunchRunas("runas", client.executable, arguments, true)
	if errors.Is(err, errPlatformUACCancelled) {
		return InvocationResult{
			UACCancelled: true,
			Response: Response{
				Version:      ProtocolVersion,
				Nonce:        nonce,
				ErrorCode:    ErrorCodeUACCancelled,
				ErrorSummary: "request cancelled",
			},
		}, nil
	}
	if err != nil || childHandle == nil || childHandle.RawHandle() == 0 {
		return InvocationResult{}, stableContextError(sessionCtx, "elevated process launch failed")
	}
	defer childHandle.Close()
	child, err := client.handles.InspectHandle(childHandle.RawHandle())
	if err != nil || child.StartedAtUnixMS <= 0 || !sameFinalImage(client.self.ExecutablePath, child.ExecutablePath) {
		return InvocationResult{}, errors.New("elevation: elevated process identity validation failed")
	}

	waitDone := make(chan error, 1)
	go func() {
		_, waitErr := client.inspector.Wait(sessionCtx, child)
		waitDone <- waitErr
	}()

	connection, err := listener.Accept(sessionCtx)
	if err != nil {
		return InvocationResult{}, stableContextError(sessionCtx, "elevated connection failed")
	}
	defer connection.Close()
	if deadline, ok := sessionCtx.Deadline(); ok {
		if err := connection.SetDeadline(deadline); err != nil {
			return InvocationResult{}, errors.New("elevation: pipe deadline setup failed")
		}
	}
	peerPID, err := connection.PeerPID()
	if err != nil {
		return InvocationResult{}, errors.New("elevation: peer process identity unavailable")
	}
	peer, err := client.inspector.Inspect(peerPID)
	if err != nil || !nodeprocess.SameProcess(child, peer) {
		return InvocationResult{}, errors.New("elevation: peer process identity mismatch")
	}
	if err := WriteRequestFrame(connection, request); err != nil {
		return InvocationResult{}, stableContextError(sessionCtx, "request write failed")
	}
	response, err := ReadResponseFrame(connection, nonce)
	if err != nil {
		return InvocationResult{}, stableContextError(sessionCtx, "response validation failed")
	}
	select {
	case waitErr := <-waitDone:
		if waitErr != nil && !errors.Is(waitErr, context.Canceled) {
			return InvocationResult{}, errors.New("elevation: elevated process wait failed")
		}
	case <-sessionCtx.Done():
		return InvocationResult{}, sessionCtx.Err()
	}
	return InvocationResult{Response: response}, nil
}

func sameFinalImage(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}

func stableContextError(ctx context.Context, fallback string) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return errors.New("elevation: " + fallback)
}

func currentUserPipeSDDL() (string, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return "", err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", err
	}
	return "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + user.User.Sid.String() + ")", nil
}

type nativeClientPlatform struct{}

func (nativeClientPlatform) Listen(ctx context.Context, pipeName, sddl string) (oneShotListener, error) {
	return newWindowsPipeListener(ctx, pipeName, sddl)
}

const seeMaskNoCloseProcess uint32 = 0x00000040

type shellExecuteInfo struct {
	Size       uint32
	Mask       uint32
	Window     windows.Handle
	Verb       *uint16
	File       *uint16
	Parameters *uint16
	Directory  *uint16
	Show       int32
	Instance   windows.Handle
	IDList     uintptr
	Class      *uint16
	ClassKey   windows.Handle
	HotKey     uint32
	Icon       windows.Handle
	Process    windows.Handle
}

var shellExecuteExW = windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")

func (nativeClientPlatform) LaunchRunas(verbValue, executable, arguments string, requestProcessHandle bool) (processHandle, error) {
	verb, err := windows.UTF16PtrFromString(verbValue)
	if err != nil {
		return nil, err
	}
	file, err := windows.UTF16PtrFromString(executable)
	if err != nil {
		return nil, err
	}
	parameters, err := windows.UTF16PtrFromString(arguments)
	if err != nil {
		return nil, err
	}
	mask := uint32(0)
	if requestProcessHandle {
		mask = seeMaskNoCloseProcess
	}
	info := shellExecuteInfo{
		Size:       uint32(unsafe.Sizeof(shellExecuteInfo{})),
		Mask:       mask,
		Verb:       verb,
		File:       file,
		Parameters: parameters,
		Show:       windows.SW_SHOWNORMAL,
	}
	ok, _, callErr := shellExecuteExW.Call(uintptr(unsafe.Pointer(&info)))
	if ok == 0 {
		if callErr == windows.ERROR_CANCELLED {
			return nil, errPlatformUACCancelled
		}
		return nil, errors.New("ShellExecuteExW failed")
	}
	if !requestProcessHandle || info.Process == 0 {
		return nil, errors.New("ShellExecuteExW returned no process handle")
	}
	return &windowsProcessHandle{handle: info.Process}, nil
}

type windowsProcessHandle struct {
	handle windows.Handle
}

func (handle *windowsProcessHandle) RawHandle() uintptr {
	if handle == nil {
		return 0
	}
	return uintptr(handle.handle)
}

func (handle *windowsProcessHandle) Close() error {
	if handle == nil || handle.handle == 0 {
		return nil
	}
	err := windows.CloseHandle(handle.handle)
	handle.handle = 0
	return err
}
