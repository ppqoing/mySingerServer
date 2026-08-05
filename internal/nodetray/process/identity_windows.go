//go:build windows

package process

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsInspector struct{}

type processSIDBackend interface {
	Open(pid int) (uintptr, error)
	Inspect(handle uintptr, pid int) (Identity, error)
	UserSID(handle uintptr) (string, error)
	Close(handle uintptr) error
}

type nativeProcessSIDBackend struct{}

func NewInspector() Inspector { return windowsInspector{} }

// UserSIDForProcess reads the user SID from the token of an already validated
// process. The immutable process identity is checked on the same handle before
// and after the token query so PID reuse cannot substitute another user.
func UserSIDForProcess(identity Identity) (string, error) {
	return userSIDForProcessWithBackend(identity, nativeProcessSIDBackend{})
}

func userSIDForProcessWithBackend(identity Identity, backend processSIDBackend) (string, error) {
	if identity.PID <= 0 || identity.StartedAtUnixMS <= 0 || identity.ExecutablePath == "" || backend == nil {
		return "", errors.New("process user SID identity is invalid")
	}
	handle, err := backend.Open(identity.PID)
	if err != nil || handle == 0 {
		return "", errors.New("open process user token source failed")
	}
	defer backend.Close(handle)
	before, err := backend.Inspect(handle, identity.PID)
	if err != nil || !SameProcess(identity, before) {
		return "", errors.New("process identity changed before user token query")
	}
	sid, err := backend.UserSID(handle)
	if err != nil {
		return "", errors.New("process user token query failed")
	}
	parsed, err := windows.StringToSid(sid)
	if err != nil || parsed == nil || !strings.EqualFold(parsed.String(), sid) {
		return "", errors.New("process user SID is invalid")
	}
	after, err := backend.Inspect(handle, identity.PID)
	if err != nil || !SameProcess(identity, after) {
		return "", errors.New("process identity changed after user token query")
	}
	return parsed.String(), nil
}

func (nativeProcessSIDBackend) Open(pid int) (uintptr, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	return uintptr(handle), err
}

func (nativeProcessSIDBackend) Inspect(raw uintptr, pid int) (Identity, error) {
	if raw == 0 {
		return Identity{}, errors.New("process handle is missing")
	}
	return inspectWindowsHandle(windows.Handle(raw), pid)
}

func (nativeProcessSIDBackend) UserSID(raw uintptr) (string, error) {
	if raw == 0 {
		return "", errors.New("process handle is missing")
	}
	var token windows.Token
	if err := windows.OpenProcessToken(windows.Handle(raw), windows.TOKEN_QUERY, &token); err != nil {
		return "", err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return "", errors.New("process token user is unavailable")
	}
	return user.User.Sid.String(), nil
}

func (nativeProcessSIDBackend) Close(raw uintptr) error {
	if raw == 0 {
		return nil
	}
	return windows.CloseHandle(windows.Handle(raw))
}

func (windowsInspector) Inspect(pid int) (Identity, error) {
	if pid <= 0 {
		return Identity{}, errors.New("pid must be positive")
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return Identity{}, fmt.Errorf("open process identity: %w", err)
	}
	defer windows.CloseHandle(handle)
	return inspectWindowsHandle(handle, pid)
}

func (windowsInspector) InspectHandle(raw uintptr) (Identity, error) {
	if raw == 0 {
		return Identity{}, errors.New("process handle is missing")
	}
	rawPID, err := windows.GetProcessId(windows.Handle(raw))
	if err != nil {
		return Identity{}, fmt.Errorf("query process handle pid: %w", err)
	}
	pid := int(rawPID)
	if pid <= 0 {
		return Identity{}, errors.New("process handle has no pid")
	}
	return inspectWindowsHandle(windows.Handle(raw), pid)
}

func (windowsInspector) Wait(ctx context.Context, identity Identity) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(identity.PID))
	if err != nil {
		return 0, fmt.Errorf("open process wait handle: %w", err)
	}
	defer windows.CloseHandle(handle)
	actual, err := inspectWindowsHandle(handle, identity.PID)
	if err != nil {
		return 0, err
	}
	if !SameProcess(identity, actual) {
		return 0, errors.New("process identity changed before wait")
	}

	if ctx.Done() == nil {
		if _, err := windows.WaitForSingleObject(handle, windows.INFINITE); err != nil {
			return 0, fmt.Errorf("wait process: %w", err)
		}
	} else {
		cancelEvent, err := windows.CreateEvent(nil, 1, 0, nil)
		if err != nil {
			return 0, fmt.Errorf("create cancellation event: %w", err)
		}
		defer windows.CloseHandle(cancelEvent)
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-ctx.Done():
				_ = windows.SetEvent(cancelEvent)
			case <-done:
			}
		}()
		which, err := windows.WaitForMultipleObjects([]windows.Handle{handle, cancelEvent}, false, windows.INFINITE)
		if err != nil {
			return 0, fmt.Errorf("wait process or cancellation: %w", err)
		}
		if which == windows.WAIT_OBJECT_0+1 {
			return 0, ctx.Err()
		}
		if which != windows.WAIT_OBJECT_0 {
			return 0, fmt.Errorf("unexpected wait result %#x", which)
		}
	}

	var code uint32
	if err := windows.GetExitCodeProcess(handle, &code); err != nil {
		return 0, fmt.Errorf("query process exit code: %w", err)
	}
	return int(code), nil
}

func inspectWindowsHandle(handle windows.Handle, pid int) (Identity, error) {
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return Identity{}, fmt.Errorf("query process creation time: %w", err)
	}
	image := make([]uint16, 32768)
	size := uint32(len(image))
	if err := windows.QueryFullProcessImageName(handle, 0, &image[0], &size); err != nil {
		return Identity{}, fmt.Errorf("query process image: %w", err)
	}
	finalPath, err := resolveFinalPath(windows.UTF16ToString(image[:size]))
	if err != nil {
		return Identity{}, err
	}
	return Identity{PID: pid, StartedAtUnixMS: created.Nanoseconds() / 1_000_000, ExecutablePath: finalPath}, nil
}

func resolveFinalPath(image string) (string, error) {
	path, err := windows.UTF16PtrFromString(image)
	if err != nil {
		return "", errors.New("process image path is invalid")
	}
	file, err := windows.CreateFile(path, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return "", fmt.Errorf("open process image final path: %w", err)
	}
	defer windows.CloseHandle(file)
	buffer := make([]uint16, 32768)
	n, err := windows.GetFinalPathNameByHandle(file, &buffer[0], uint32(len(buffer)), 0)
	if err != nil {
		return "", fmt.Errorf("query process image final path: %w", err)
	}
	if n == 0 || n >= uint32(len(buffer)) {
		return "", errors.New("process image final path exceeds supported length")
	}
	return stripExtendedPrefix(windows.UTF16ToString(buffer[:n])), nil
}

func stripExtendedPrefix(value string) string {
	if strings.HasPrefix(value, `\\?\UNC\`) {
		return `\\` + strings.TrimPrefix(value, `\\?\UNC\`)
	}
	return strings.TrimPrefix(value, `\\?\`)
}

var compareStringOrdinal = windows.NewLazySystemDLL("kernel32.dll").NewProc("CompareStringOrdinal")

func sameExecutablePath(left, right string) bool {
	l, err := windows.UTF16FromString(left)
	if err != nil {
		return false
	}
	r, err := windows.UTF16FromString(right)
	if err != nil {
		return false
	}
	result, _, _ := compareStringOrdinal.Call(
		uintptr(unsafe.Pointer(&l[0])), uintptr(len(l)-1),
		uintptr(unsafe.Pointer(&r[0])), uintptr(len(r)-1),
		1,
	)
	return result == 2
}
