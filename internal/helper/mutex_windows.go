package helper

import (
	"errors"
	"fmt"
	"sync"

	"golang.org/x/sys/windows"
)

const HelperMutexName = `Local\DedupDeleteHelperMutex`

type windowsInstanceLock struct {
	mu     sync.Mutex
	handle windows.Handle
}

type InstanceLock interface {
	Close() error
}

func AcquireInstanceLock(name string) (InstanceLock, error) {
	if name == "" {
		return nil, fmt.Errorf("helper mutex name must not be empty")
	}

	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, fmt.Errorf("encode helper mutex name: %w", err)
	}

	handle, err := windows.CreateMutex(nil, false, namePtr)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		return nil, fmt.Errorf("helper mutex already exists: %w", err)
	}
	if err != nil {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		return nil, fmt.Errorf("create helper mutex: %w", err)
	}

	return &windowsInstanceLock{handle: handle}, nil
}

func (l *windowsInstanceLock) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.handle == 0 {
		return nil
	}
	handle := l.handle
	l.handle = 0
	if err := windows.CloseHandle(handle); err != nil {
		return fmt.Errorf("close helper mutex: %w", err)
	}
	return nil
}
