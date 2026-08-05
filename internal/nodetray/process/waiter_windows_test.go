//go:build windows

package process

import (
	"context"
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

type recordingPIDWaitBackend struct {
	handle      uintptr
	openErr     error
	waitResults []uint32
	waitErr     error
	openPIDs    []int
	closed      []uintptr
}

func (f *recordingPIDWaitBackend) OpenForSynchronize(pid int) (uintptr, error) {
	f.openPIDs = append(f.openPIDs, pid)
	return f.handle, f.openErr
}

func (f *recordingPIDWaitBackend) Wait(handle uintptr, _ uint32) (uint32, error) {
	if f.waitErr != nil {
		return 0, f.waitErr
	}
	if len(f.waitResults) == 0 {
		return uint32(windows.WAIT_TIMEOUT), nil
	}
	result := f.waitResults[0]
	f.waitResults = f.waitResults[1:]
	return result, nil
}

func (f *recordingPIDWaitBackend) CloseProcessHandle(handle uintptr) {
	f.closed = append(f.closed, handle)
}

func TestPIDWaiterReturnsOnlyAfterTrackedPIDHandleIsSignaled(t *testing.T) {
	backend := &recordingPIDWaitBackend{
		handle:      77,
		waitResults: []uint32{uint32(windows.WAIT_TIMEOUT), uint32(windows.WAIT_OBJECT_0)},
	}
	waiter := newPIDWaiter(backend)

	if err := waiter.WaitPIDGone(context.Background(), 654); err != nil {
		t.Fatalf("WaitPIDGone: %v", err)
	}
	if got := backend.openPIDs; len(got) != 1 || got[0] != 654 {
		t.Fatalf("opened PIDs = %v, want [654]", got)
	}
	if got := backend.closed; len(got) != 1 || got[0] != 77 {
		t.Fatalf("closed handles = %v, want [77]", got)
	}
}

func TestPIDWaiterTreatsMissingPIDAsExited(t *testing.T) {
	backend := &recordingPIDWaitBackend{openErr: windows.ERROR_INVALID_PARAMETER}
	waiter := newPIDWaiter(backend)

	if err := waiter.WaitPIDGone(context.Background(), 654); err != nil {
		t.Fatalf("WaitPIDGone: %v", err)
	}
}

func TestPIDWaiterStopsOnContextCancellation(t *testing.T) {
	backend := &recordingPIDWaitBackend{handle: 77}
	waiter := newPIDWaiter(backend)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := waiter.WaitPIDGone(ctx, 654)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitPIDGone error = %v, want context.Canceled", err)
	}
}
