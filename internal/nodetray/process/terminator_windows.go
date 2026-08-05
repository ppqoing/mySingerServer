//go:build windows

package process

import (
	"errors"
	"fmt"
	"path/filepath"

	"golang.org/x/sys/windows"
)

var (
	errTrustedTerminationIdentity    = errors.New("trusted termination identity could not be verified")
	errTrustedTerminationUnavailable = errors.New("trusted termination is unavailable")
	errTrustedTerminationOpen        = errors.New("trusted termination process handle could not be opened")
	errTrustedTerminationProcess     = errors.New("trusted termination process could not be terminated")
)

type terminatorBackend interface {
	OpenForTerminate(pid int) (uintptr, error)
	Terminate(handle uintptr, exitCode uint32) error
	CloseProcessHandle(handle uintptr)
}

// TrustedTerminator terminates only a process whose currently opened handle
// still proves the supplied immutable identity.
type TrustedTerminator struct {
	handleInspector HandleInspector
	backend         terminatorBackend
}

// DirectTerminator terminates the PID already recorded by the supervisor.
// It intentionally performs no identity, path, or creation-time recheck.
type DirectTerminator struct {
	backend terminatorBackend
}

func NewDirectTerminator() *DirectTerminator {
	return newDirectTerminator(nativeDirectTerminatorBackend{})
}

func newDirectTerminator(backend terminatorBackend) *DirectTerminator {
	return &DirectTerminator{backend: backend}
}

func (t *DirectTerminator) Terminate(identity Identity, exitCode uint32) error {
	if identity.PID <= 0 {
		return errors.New("tracked pid must be positive")
	}
	if t == nil || t.backend == nil {
		return errors.New("direct termination is unavailable")
	}
	handle, err := t.backend.OpenForTerminate(identity.PID)
	if err != nil {
		return fmt.Errorf("open tracked pid %d: %w", identity.PID, err)
	}
	if handle == 0 {
		return fmt.Errorf("open tracked pid %d returned an empty handle", identity.PID)
	}
	defer t.backend.CloseProcessHandle(handle)
	if err := t.backend.Terminate(handle, exitCode); err != nil {
		return fmt.Errorf("terminate tracked pid %d: %w", identity.PID, err)
	}
	return nil
}

func NewTrustedTerminator(inspector Inspector) *TrustedTerminator {
	return newTrustedTerminator(inspector, nativeTerminatorBackend{})
}

func newTrustedTerminator(inspector Inspector, backend terminatorBackend) *TrustedTerminator {
	handleInspector, _ := inspector.(HandleInspector)
	return &TrustedTerminator{handleInspector: handleInspector, backend: backend}
}

func (t *TrustedTerminator) Terminate(identity Identity, exitCode uint32) error {
	if !validTerminationIdentity(identity) {
		return errTrustedTerminationIdentity
	}
	if t == nil || t.handleInspector == nil || t.backend == nil {
		return errTrustedTerminationUnavailable
	}
	handle, err := t.backend.OpenForTerminate(identity.PID)
	if err != nil || handle == 0 {
		return errTrustedTerminationOpen
	}
	defer t.backend.CloseProcessHandle(handle)
	actual, err := t.handleInspector.InspectHandle(handle)
	if err != nil || !SameProcess(identity, actual) {
		return errTrustedTerminationIdentity
	}
	if err := t.backend.Terminate(handle, exitCode); err != nil {
		return errTrustedTerminationProcess
	}
	return nil
}

func validTerminationIdentity(identity Identity) bool {
	return identity.PID > 0 &&
		identity.StartedAtUnixMS > 0 &&
		identity.ExecutablePath != "" &&
		filepath.IsAbs(identity.ExecutablePath) &&
		!hasControlCharacter(identity.ExecutablePath)
}

type nativeTerminatorBackend struct{}

type nativeDirectTerminatorBackend struct{}

func (nativeDirectTerminatorBackend) OpenForTerminate(pid int) (uintptr, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return 0, err
	}
	return uintptr(handle), nil
}

func (nativeDirectTerminatorBackend) Terminate(handle uintptr, exitCode uint32) error {
	return windows.TerminateProcess(windows.Handle(handle), exitCode)
}

func (nativeDirectTerminatorBackend) CloseProcessHandle(handle uintptr) {
	_ = windows.CloseHandle(windows.Handle(handle))
}

func (nativeTerminatorBackend) OpenForTerminate(pid int) (uintptr, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return 0, err
	}
	return uintptr(handle), nil
}

func (nativeTerminatorBackend) Terminate(handle uintptr, exitCode uint32) error {
	return windows.TerminateProcess(windows.Handle(handle), exitCode)
}

func (nativeTerminatorBackend) CloseProcessHandle(handle uintptr) {
	_ = windows.CloseHandle(windows.Handle(handle))
}
