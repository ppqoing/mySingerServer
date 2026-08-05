//go:build windows

package process

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const SeeMaskNoCloseProcess uint32 = 0x00000040

type ShellExecuteRequest struct {
	Verb       string
	File       string
	Parameters string
	Mask       uint32
}

type ShellExecuteBackend interface {
	Execute(ctx context.Context, request ShellExecuteRequest) (processHandle uintptr, err error)
}

type HandleInspector interface {
	InspectHandle(processHandle uintptr) (Identity, error)
}

type ManualHelperLauncher struct {
	backend   ShellExecuteBackend
	inspector HandleInspector
}

func NewManualHelperLauncher(backend ShellExecuteBackend, inspector HandleInspector) *ManualHelperLauncher {
	if backend == nil {
		backend = nativeShellExecuteBackend{}
	}
	if inspector == nil {
		inspector = windowsInspector{}
	}
	return &ManualHelperLauncher{backend: backend, inspector: inspector}
}

// Start also satisfies the Supervisor's ordinary Launcher shape, but only for
// its fixed Helper invocation. The elevated-specific method remains the
// canonical path selected by Supervisor.
func (l *ManualHelperLauncher) Start(ctx context.Context, executable string, args []string, env []string) (Identity, error) {
	if len(env) != 0 || len(args) != 2 || args[0] != "--config" {
		return Identity{}, errors.New("Helper launch only accepts --config and no environment")
	}
	return l.StartHelper(ctx, executable, args[1])
}

func (l *ManualHelperLauncher) StartHelper(ctx context.Context, helperExecutable string, helperConfig string) (Identity, error) {
	helper, err := canonicalHelperPath(helperExecutable)
	if err != nil {
		return Identity{}, err
	}
	config, err := filepath.Abs(helperConfig)
	if err != nil || strings.Contains(config, `"`) {
		return Identity{}, errors.New("Helper config path is invalid")
	}
	config = filepath.Clean(config)
	handle, err := l.backend.Execute(ctx, ShellExecuteRequest{
		Verb:       "runas",
		File:       helper,
		Parameters: `--config "` + config + `"`,
		Mask:       SeeMaskNoCloseProcess,
	})
	if err != nil {
		if errors.Is(err, windows.ERROR_CANCELLED) {
			return Identity{}, &ErrUACCancelled{}
		}
		return Identity{}, fmt.Errorf("start elevated Helper: %w", err)
	}
	if handle == 0 {
		return Identity{}, errors.New("elevated Helper returned no process handle")
	}
	if closer, ok := l.backend.(interface{ CloseProcessHandle(uintptr) }); ok {
		defer closer.CloseProcessHandle(handle)
	}
	identity, err := l.inspector.InspectHandle(handle)
	if err != nil {
		return Identity{}, fmt.Errorf("inspect elevated Helper: %w", err)
	}
	return identity, nil
}

func canonicalHelperPath(value string) (string, error) {
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", errors.New("Helper executable path is invalid")
	}
	abs = filepath.Clean(abs)
	if !strings.EqualFold(filepath.Base(abs), "helper.exe") {
		return "", errors.New("Helper executable must be helper.exe")
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = filepath.Clean(resolved)
	}
	return abs, nil
}

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

type nativeShellExecuteBackend struct{}

var shellExecuteExW = windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")

func (nativeShellExecuteBackend) Execute(ctx context.Context, request ShellExecuteRequest) (uintptr, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	default:
	}
	verb, _ := windows.UTF16PtrFromString(request.Verb)
	file, _ := windows.UTF16PtrFromString(request.File)
	parameters, _ := windows.UTF16PtrFromString(request.Parameters)
	info := shellExecuteInfo{
		Size:       uint32(unsafe.Sizeof(shellExecuteInfo{})),
		Mask:       request.Mask,
		Verb:       verb,
		File:       file,
		Parameters: parameters,
		Show:       1,
	}
	ok, _, callErr := shellExecuteExW.Call(uintptr(unsafe.Pointer(&info)))
	if ok == 0 {
		if callErr == windows.ERROR_CANCELLED {
			return 0, windows.ERROR_CANCELLED
		}
		return 0, callErr
	}
	return uintptr(info.Process), nil
}

func (nativeShellExecuteBackend) CloseProcessHandle(handle uintptr) {
	_ = windows.CloseHandle(windows.Handle(handle))
}

func windowsCancelledError() error { return windows.ERROR_CANCELLED }
