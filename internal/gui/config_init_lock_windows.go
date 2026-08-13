//go:build windows

package gui

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"runtime"

	"golang.org/x/sys/windows"
)

type guiConfigInitWindowsLock struct {
	handle windows.Handle
	held   bool
}

func lockGUIConfigInit(absolute string) (guiConfigInitLock, error) {
	runtime.LockOSThread()
	lockedThread := true
	defer func() {
		if lockedThread {
			runtime.UnlockOSThread()
		}
	}()

	digest := sha256.Sum256([]byte(absolute))
	name, err := windows.UTF16PtrFromString(fmt.Sprintf(`Local\DedupManagerGUIConfigInit-%x`, digest))
	if err != nil {
		return nil, fmt.Errorf("encode config initialization mutex name: %w", err)
	}
	handle, err := windows.CreateMutex(nil, false, name)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return nil, fmt.Errorf("create config initialization mutex: %w", err)
	}
	if handle == 0 {
		return nil, fmt.Errorf("create config initialization mutex: invalid handle")
	}
	result, err := windows.WaitForSingleObject(handle, windows.INFINITE)
	if err != nil || (result != windows.WAIT_OBJECT_0 && result != windows.WAIT_ABANDONED) {
		_ = windows.CloseHandle(handle)
		if err != nil {
			return nil, fmt.Errorf("wait for config initialization mutex: %w", err)
		}
		return nil, fmt.Errorf("wait for config initialization mutex: status %d", result)
	}
	lockedThread = false
	return &guiConfigInitWindowsLock{handle: handle, held: true}, nil
}

func isGUIConfigInitTransientReadError(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

func (lock *guiConfigInitWindowsLock) Release() error {
	defer runtime.UnlockOSThread()

	var errs []error
	if lock.held {
		if err := windows.ReleaseMutex(lock.handle); err != nil {
			errs = append(errs, fmt.Errorf("release config initialization mutex: %w", err))
		}
		lock.held = false
	}
	if lock.handle != 0 {
		if err := windows.CloseHandle(lock.handle); err != nil {
			errs = append(errs, fmt.Errorf("close config initialization mutex: %w", err))
		}
		lock.handle = 0
	}
	return errors.Join(errs...)
}
