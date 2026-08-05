//go:build windows

package process

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type recordingShellExecuteBackend struct {
	request ShellExecuteRequest
	handle  uintptr
	err     error
	calls   int
	closed  uintptr
}

func (f *recordingShellExecuteBackend) Execute(_ context.Context, request ShellExecuteRequest) (uintptr, error) {
	f.calls++
	f.request = request
	return f.handle, f.err
}

func (f *recordingShellExecuteBackend) CloseProcessHandle(handle uintptr) {
	f.closed = handle
}

type fixedHandleInspector struct {
	identity Identity
	handle   uintptr
	calls    int
}

func (f *fixedHandleInspector) InspectHandle(handle uintptr) (Identity, error) {
	f.calls++
	f.handle = handle
	return f.identity, nil
}

func TestManualHelperLauncherUsesOnlyFixedRunasContract(t *testing.T) {
	root := t.TempDir()
	helper := filepath.Join(root, "helper.exe")
	config := filepath.Join(root, "helper.json")
	if err := os.WriteFile(helper, []byte("test fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := &recordingShellExecuteBackend{handle: 99}
	handleInspector := &fixedHandleInspector{identity: Identity{PID: 77, StartedAtUnixMS: 123, ExecutablePath: helper}}
	launcher := NewManualHelperLauncher(backend, handleInspector)

	identity, err := launcher.StartHelper(context.Background(), helper, config)
	if err != nil {
		t.Fatalf("StartHelper: %v", err)
	}
	if identity != handleInspector.identity {
		t.Fatalf("identity = %+v, want %+v", identity, handleInspector.identity)
	}
	if backend.calls != 1 || handleInspector.calls != 1 || handleInspector.handle != 99 {
		t.Fatalf("backend calls=%d inspector calls=%d handle=%d", backend.calls, handleInspector.calls, handleInspector.handle)
	}
	if backend.closed != 99 {
		t.Fatalf("returned process handle was not closed: got %d", backend.closed)
	}
	if backend.request.Verb != "runas" {
		t.Fatalf("verb = %q, want runas", backend.request.Verb)
	}
	wantHelper, _ := filepath.Abs(helper)
	if !strings.EqualFold(backend.request.File, filepath.Clean(wantHelper)) {
		t.Fatalf("file = %q, want canonical helper %q", backend.request.File, wantHelper)
	}
	wantConfig, _ := filepath.Abs(config)
	wantParameters := `--config "` + filepath.Clean(wantConfig) + `"`
	if backend.request.Parameters != wantParameters {
		t.Fatalf("parameters = %q, want %q", backend.request.Parameters, wantParameters)
	}
	if backend.request.Mask&SeeMaskNoCloseProcess == 0 {
		t.Fatalf("mask %#x does not contain SEE_MASK_NOCLOSEPROCESS", backend.request.Mask)
	}
}

func TestManualHelperLauncherRejectsNonHelperExecutable(t *testing.T) {
	backend := &recordingShellExecuteBackend{}
	launcher := NewManualHelperLauncher(backend, &fixedHandleInspector{})
	_, err := launcher.StartHelper(context.Background(), filepath.Join(t.TempDir(), "agent.exe"), filepath.Join(t.TempDir(), "helper.json"))
	if err == nil {
		t.Fatal("non-helper executable was accepted")
	}
	if backend.calls != 0 {
		t.Fatalf("ShellExecute was called %d times for invalid executable", backend.calls)
	}
}

func TestManualHelperLauncherMapsWindowsCancellationToTypedError(t *testing.T) {
	root := t.TempDir()
	helper := filepath.Join(root, "helper.exe")
	if err := os.WriteFile(helper, []byte("test fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := &recordingShellExecuteBackend{err: windowsCancelledError()}
	launcher := NewManualHelperLauncher(backend, &fixedHandleInspector{})
	_, err := launcher.StartHelper(context.Background(), helper, filepath.Join(root, "helper.json"))
	var cancelled *ErrUACCancelled
	if !errors.As(err, &cancelled) {
		t.Fatalf("error = %T %v, want *ErrUACCancelled", err, err)
	}
}

func TestManualHelperLauncherFitsSupervisorLauncherWithoutAcceptingBroadArguments(t *testing.T) {
	root := t.TempDir()
	helper := filepath.Join(root, "helper.exe")
	config := filepath.Join(root, "helper.json")
	if err := os.WriteFile(helper, []byte("test fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := &recordingShellExecuteBackend{handle: 101}
	launcher := NewManualHelperLauncher(backend, &fixedHandleInspector{identity: Identity{PID: 78, StartedAtUnixMS: 124, ExecutablePath: helper}})
	var supervisorLauncher interface {
		Start(context.Context, string, []string, []string) (Identity, error)
	} = launcher

	if _, err := supervisorLauncher.Start(context.Background(), helper, []string{"--config", config}, nil); err != nil {
		t.Fatalf("fixed Supervisor launch contract failed: %v", err)
	}
	if _, err := supervisorLauncher.Start(context.Background(), helper, []string{"--config", config}, []string{"SECRET=value"}); err == nil {
		t.Fatal("Helper Launcher accepted an environment")
	}
	if _, err := supervisorLauncher.Start(context.Background(), helper, []string{"--other", config}, nil); err == nil {
		t.Fatal("Helper Launcher accepted arbitrary arguments")
	}
	if backend.calls != 1 {
		t.Fatalf("ShellExecute calls = %d, want only the fixed launch", backend.calls)
	}
}
