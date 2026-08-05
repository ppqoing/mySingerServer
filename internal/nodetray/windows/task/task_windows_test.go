//go:build windows

package task

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestServiceInspectMapsFixedTaskStatesAndCancellation(t *testing.T) {
	want := Status{Installed: true, Running: true, LastResult: 23}
	backend := &fakeSchedulerBackend{status: want}
	service := mustFakeService(t, backend, CapabilityUser)

	got, err := service.Inspect(context.Background())
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if got != want {
		t.Fatalf("Inspect = %#v, want %#v", got, want)
	}
	backend.assertOnlyCall(t, "inspect", TaskPath)

	backend.reset()
	backend.inspectErr = ErrTaskNotInstalled
	got, err = service.Inspect(context.Background())
	if err != nil || got != (Status{}) {
		t.Fatalf("Inspect missing = (%#v, %v), want zero, nil", got, err)
	}
	backend.assertOnlyCall(t, "inspect", TaskPath)

	backend.reset()
	backend.inspectErr = ErrAccessDenied
	if _, err := service.Inspect(context.Background()); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("Inspect access denied = %v", err)
	}

	backend.reset()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.Inspect(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Inspect canceled = %v, want context.Canceled", err)
	}
	if len(backend.calls) != 0 {
		t.Fatalf("canceled Inspect made backend calls: %#v", backend.calls)
	}

	backend.reset()
	ctx, cancel = context.WithCancel(context.Background())
	backend.inspectHook = cancel
	if _, err := service.Inspect(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Inspect canceled during backend call = %v, want context.Canceled", err)
	}
	backend.assertOnlyCall(t, "inspect", TaskPath)
}

func TestServiceInstallRequiresElevatedCapabilityAndRegistersFixedDefinition(t *testing.T) {
	definition := validServiceDefinition(t)
	ordinaryBackend := &fakeSchedulerBackend{}
	ordinary := mustFakeService(t, ordinaryBackend, CapabilityUser)
	if err := ordinary.Install(context.Background(), definition); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("ordinary Install = %v, want ErrAccessDenied", err)
	}
	if len(ordinaryBackend.calls) != 0 {
		t.Fatalf("ordinary Install made backend calls: %#v", ordinaryBackend.calls)
	}

	elevatedBackend := &fakeSchedulerBackend{}
	elevated := mustFakeService(t, elevatedBackend, CapabilityElevated)
	if err := elevated.Install(context.Background(), definition); err != nil {
		t.Fatalf("elevated Install: %v", err)
	}
	elevatedBackend.assertOnlyCall(t, "register", TaskPath)
	if elevatedBackend.registered.Path != TaskPath || len(elevatedBackend.registered.Actions) != 1 ||
		elevatedBackend.registered.Principal.UserSID != definition.UserSID {
		t.Fatalf("registered definition drifted: %#v", elevatedBackend.registered)
	}

	elevatedBackend.reset()
	definition.HelperExecutable = filepath.Join(t.TempDir(), "worker.exe")
	if err := elevated.Install(context.Background(), definition); err == nil {
		t.Fatal("Install accepted non-helper executable")
	}
	if len(elevatedBackend.calls) != 0 {
		t.Fatalf("invalid Install made backend calls: %#v", elevatedBackend.calls)
	}
}

func TestServiceRunUsesOnlyFixedTaskAndPreservesStableErrors(t *testing.T) {
	backend := &fakeSchedulerBackend{}
	service := mustFakeService(t, backend, CapabilityUser)
	if err := service.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	backend.assertOnlyCall(t, "run", TaskPath)

	backend.reset()
	backend.runErr = ErrTaskNotInstalled
	if err := service.Run(context.Background()); !errors.Is(err, ErrTaskNotInstalled) {
		t.Fatalf("Run missing = %v, want ErrTaskNotInstalled", err)
	}

	backend.reset()
	backend.runErr = ErrAccessDenied
	if err := service.Run(context.Background()); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("Run access denied = %v, want ErrAccessDenied", err)
	}
}

func TestServiceStopRequiresElevatedCapabilityAndIsIdempotentWhenNotRunning(t *testing.T) {
	ordinaryBackend := &fakeSchedulerBackend{}
	ordinary := mustFakeService(t, ordinaryBackend, CapabilityUser)
	if err := ordinary.Stop(context.Background()); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("ordinary Stop = %v, want ErrAccessDenied", err)
	}
	if len(ordinaryBackend.calls) != 0 {
		t.Fatalf("ordinary Stop made backend calls: %#v", ordinaryBackend.calls)
	}

	backend := &fakeSchedulerBackend{}
	elevated := mustFakeService(t, backend, CapabilityElevated)
	if err := elevated.Stop(context.Background()); err != nil {
		t.Fatalf("elevated Stop: %v", err)
	}
	backend.assertOnlyCall(t, "stop", TaskPath)

	backend.reset()
	backend.stopErr = ErrTaskNotRunning
	if err := elevated.Stop(context.Background()); err != nil {
		t.Fatalf("Stop not running = %v, want nil", err)
	}
	backend.assertOnlyCall(t, "stop", TaskPath)
}

func TestServiceRemoveRequiresElevatedCapabilityAndMissingIsIdempotent(t *testing.T) {
	ordinaryBackend := &fakeSchedulerBackend{}
	ordinary := mustFakeService(t, ordinaryBackend, CapabilityUser)
	if err := ordinary.Remove(context.Background()); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("ordinary Remove = %v, want ErrAccessDenied", err)
	}
	if len(ordinaryBackend.calls) != 0 {
		t.Fatalf("ordinary Remove made backend calls: %#v", ordinaryBackend.calls)
	}

	backend := &fakeSchedulerBackend{}
	elevated := mustFakeService(t, backend, CapabilityElevated)
	backend.deleteErr = ErrTaskNotInstalled
	if err := elevated.Remove(context.Background()); err != nil {
		t.Fatalf("Remove missing = %v, want nil", err)
	}
	backend.assertOnlyCall(t, "delete", TaskPath)
}

func TestServiceRedactsUnexpectedBackendErrors(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret-helper.json")
	backend := &fakeSchedulerBackend{inspectErr: errors.New("COM XML contained " + secret)}
	service := mustFakeService(t, backend, CapabilityUser)
	_, err := service.Inspect(context.Background())
	if !errors.Is(err, ErrBackend) {
		t.Fatalf("Inspect unexpected error = %v, want ErrBackend", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("Inspect error leaked backend detail: %v", err)
	}
}

func TestServiceRejectsUnknownCapability(t *testing.T) {
	if _, err := newServiceWithBackend(&fakeSchedulerBackend{}, Capability(99), identityResolver); err == nil {
		t.Fatal("newServiceWithBackend accepted unknown capability")
	}
}

type backendCall struct {
	operation string
	path      string
}

type fakeSchedulerBackend struct {
	status      Status
	inspectErr  error
	inspectHook func()
	registerErr error
	runErr      error
	stopErr     error
	deleteErr   error
	calls       []backendCall
	registered  taskRegistration
}

func (f *fakeSchedulerBackend) Inspect(_ context.Context, path string) (Status, error) {
	f.calls = append(f.calls, backendCall{operation: "inspect", path: path})
	if f.inspectHook != nil {
		f.inspectHook()
	}
	return f.status, f.inspectErr
}

func (f *fakeSchedulerBackend) Register(_ context.Context, path string, registration taskRegistration) error {
	f.calls = append(f.calls, backendCall{operation: "register", path: path})
	f.registered = registration
	return f.registerErr
}

func (f *fakeSchedulerBackend) Run(_ context.Context, path string) error {
	f.calls = append(f.calls, backendCall{operation: "run", path: path})
	return f.runErr
}

func (f *fakeSchedulerBackend) Stop(_ context.Context, path string) error {
	f.calls = append(f.calls, backendCall{operation: "stop", path: path})
	return f.stopErr
}

func (f *fakeSchedulerBackend) Delete(_ context.Context, path string) error {
	f.calls = append(f.calls, backendCall{operation: "delete", path: path})
	return f.deleteErr
}

func (f *fakeSchedulerBackend) reset() {
	f.calls = nil
	f.registered = taskRegistration{}
	f.inspectErr = nil
	f.inspectHook = nil
	f.registerErr = nil
	f.runErr = nil
	f.stopErr = nil
	f.deleteErr = nil
}

func (f *fakeSchedulerBackend) assertOnlyCall(t *testing.T, operation, path string) {
	t.Helper()
	if len(f.calls) != 1 || f.calls[0] != (backendCall{operation: operation, path: path}) {
		t.Fatalf("backend calls = %#v, want one %s(%q)", f.calls, operation, path)
	}
}

func mustFakeService(t *testing.T, backend schedulerBackend, capability Capability) *service {
	t.Helper()
	value, err := newServiceWithBackend(backend, capability, identityResolver)
	if err != nil {
		t.Fatalf("newServiceWithBackend: %v", err)
	}
	return value
}

func validServiceDefinition(t *testing.T) Definition {
	t.Helper()
	root := t.TempDir()
	return Definition{
		HelperExecutable: filepath.Join(root, "helper.exe"),
		HelperConfig:     filepath.Join(root, "helper.json"),
		UserSID:          "S-1-5-21-100-200-300-1001",
	}
}
