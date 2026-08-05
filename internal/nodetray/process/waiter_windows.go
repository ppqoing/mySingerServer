//go:build windows

package process

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

const pidWaitSliceMilliseconds = 100

type pidWaitBackend interface {
	OpenForSynchronize(pid int) (uintptr, error)
	Wait(handle uintptr, milliseconds uint32) (uint32, error)
	CloseProcessHandle(handle uintptr)
}

type PIDWaiter struct {
	backend pidWaitBackend
}

func NewPIDWaiter() *PIDWaiter {
	return newPIDWaiter(nativePIDWaitBackend{})
}

func newPIDWaiter(backend pidWaitBackend) *PIDWaiter {
	return &PIDWaiter{backend: backend}
}

func (w *PIDWaiter) WaitPIDGone(ctx context.Context, pid int) error {
	if pid <= 0 {
		return errors.New("tracked pid must be positive")
	}
	if w == nil || w.backend == nil {
		return errors.New("pid waiter is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	handle, err := w.backend.OpenForSynchronize(pid)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open tracked pid %d for wait: %w", pid, err)
	}
	if handle == 0 {
		return fmt.Errorf("open tracked pid %d for wait returned an empty handle", pid)
	}
	defer w.backend.CloseProcessHandle(handle)

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		result, err := w.backend.Wait(handle, pidWaitSliceMilliseconds)
		if err != nil {
			return fmt.Errorf("wait for tracked pid %d: %w", pid, err)
		}
		switch result {
		case uint32(windows.WAIT_OBJECT_0):
			return nil
		case uint32(windows.WAIT_TIMEOUT):
			continue
		default:
			return fmt.Errorf("wait for tracked pid %d returned status %d", pid, result)
		}
	}
}

type nativePIDWaitBackend struct{}

func (nativePIDWaitBackend) OpenForSynchronize(pid int) (uintptr, error) {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return 0, err
	}
	return uintptr(handle), nil
}

func (nativePIDWaitBackend) Wait(handle uintptr, milliseconds uint32) (uint32, error) {
	return windows.WaitForSingleObject(windows.Handle(handle), milliseconds)
}

func (nativePIDWaitBackend) CloseProcessHandle(handle uintptr) {
	_ = windows.CloseHandle(windows.Handle(handle))
}
