//go:build windows

package agentinstance

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"golang.org/x/sys/windows"
)

var ErrAlreadyRunning = errors.New("Agent instance is already running")

type AlreadyRunningError struct{ MachineID string }

func (e *AlreadyRunningError) Error() string {
	return fmt.Sprintf("Agent for machine %q is already running", e.MachineID)
}

func (*AlreadyRunningError) Is(target error) bool { return target == ErrAlreadyRunning }

type instanceLock struct {
	handle windows.Handle
	once   sync.Once
	err    error
}

func AcquireSingleInstance(machineID string) (*instanceLock, error) {
	digest := sha256.Sum256([]byte(machineID))
	name := `Local\mysingerserver-agent-instance-v1-` + hex.EncodeToString(digest[:16])
	name16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, fmt.Errorf("encode Agent instance mutex: %w", err)
	}
	handle, err := windows.CreateMutex(nil, false, name16)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		return nil, &AlreadyRunningError{MachineID: machineID}
	}
	if err != nil {
		return nil, fmt.Errorf("create Agent instance mutex: %w", err)
	}
	return &instanceLock{handle: handle}, nil
}

func (l *instanceLock) Close() error {
	l.once.Do(func() { l.err = windows.CloseHandle(l.handle) })
	return l.err
}
