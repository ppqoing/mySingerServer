//go:build windows

package loginstart

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestEnableWritesStrictQuotedBackgroundCommandThroughFakeBackend(t *testing.T) {
	executable := makeTrayExecutable(t, "含 空格")
	backend := &fakeRegistryBackend{}
	service, err := newServiceWithBackend(executable, backend)
	if err != nil {
		t.Fatalf("newServiceWithBackend: %v", err)
	}

	if err := service.Enable(executable); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	want := service.expectedRunValue
	if backend.value != want {
		t.Fatalf("registry value = %q, want %q", backend.value, want)
	}
	if !filepath.IsAbs(service.executable) || !strings.EqualFold(filepath.Base(service.executable), "nodetray.exe") {
		t.Fatalf("canonical executable = %q, want absolute nodetray.exe", service.executable)
	}
	if backend.setCalls != 1 {
		t.Fatalf("Set calls = %d, want 1", backend.setCalls)
	}
	enabled, current, err := service.Enabled()
	if err != nil {
		t.Fatalf("Enabled: %v", err)
	}
	if !enabled || current != want {
		t.Fatalf("Enabled = (%v, %q), want (true, %q)", enabled, current, want)
	}
}

func TestEnableRejectsPathDriftAndInjection(t *testing.T) {
	executable := makeTrayExecutable(t, "app")
	other := filepath.Join(filepath.Dir(executable), "other.exe")
	if err := os.WriteFile(other, []byte("test"), 0o600); err != nil {
		t.Fatalf("write other executable: %v", err)
	}
	backend := &fakeRegistryBackend{}
	service, err := newServiceWithBackend(executable, backend)
	if err != nil {
		t.Fatalf("newServiceWithBackend: %v", err)
	}

	invalid := []string{
		"nodetray.exe",
		other,
		strings.TrimSuffix(executable, ".exe") + ".bat",
		executable + " --evil",
		executable + `" --evil`,
		executable + "\n--evil",
	}
	for _, candidate := range invalid {
		if err := service.Enable(candidate); err == nil {
			t.Fatalf("Enable accepted invalid executable %q", candidate)
		}
	}
	if backend.setCalls != 0 {
		t.Fatalf("invalid Enable called Set %d times", backend.setCalls)
	}
}

func TestNewAndEnableRejectFinalExecutableWhoseNameIsNotNodeTray(t *testing.T) {
	link := makeRenamedTraySymlink(t)
	if _, err := newServiceWithBackend(link, &fakeRegistryBackend{}); err == nil {
		t.Fatal("newServiceWithBackend accepted nodetray.exe symlink whose final name is payload.exe")
	}

	executable := makeTrayExecutable(t, "real-app")
	backend := &fakeRegistryBackend{}
	service, err := newServiceWithBackend(executable, backend)
	if err != nil {
		t.Fatalf("newServiceWithBackend valid executable: %v", err)
	}
	if err := service.Enable(link); err == nil {
		t.Fatal("Enable accepted nodetray.exe symlink whose final name is payload.exe")
	}
	if backend.setCalls != 0 {
		t.Fatalf("rejected final executable called Set %d times", backend.setCalls)
	}
}

func TestEnabledReportsMissingAndDriftWithoutChangingRegistry(t *testing.T) {
	executable := makeTrayExecutable(t, "app")
	backend := &fakeRegistryBackend{missing: true}
	service, err := newServiceWithBackend(executable, backend)
	if err != nil {
		t.Fatalf("newServiceWithBackend: %v", err)
	}

	enabled, current, err := service.Enabled()
	if err != nil {
		t.Fatalf("Enabled missing: %v", err)
	}
	if enabled || current != "" {
		t.Fatalf("Enabled missing = (%v, %q), want (false, empty)", enabled, current)
	}

	backend.missing = false
	backend.value = `"C:\Program Files\Moved\nodetray.exe" --background`
	enabled, current, err = service.Enabled()
	if err != nil {
		t.Fatalf("Enabled drift: %v", err)
	}
	if enabled || current != backend.value {
		t.Fatalf("Enabled drift = (%v, %q), want (false, %q)", enabled, current, backend.value)
	}
	if backend.setCalls != 0 || backend.deleteCalls != 0 {
		t.Fatalf("Enabled changed backend: set=%d delete=%d", backend.setCalls, backend.deleteCalls)
	}
}

func TestDisableDeletesOnlyFixedValueAndIsIdempotent(t *testing.T) {
	executable := makeTrayExecutable(t, "app")
	backend := &fakeRegistryBackend{value: `"C:\Old\nodetray.exe" --background`}
	service, err := newServiceWithBackend(executable, backend)
	if err != nil {
		t.Fatalf("newServiceWithBackend: %v", err)
	}

	if err := service.Disable(); err != nil {
		t.Fatalf("Disable existing: %v", err)
	}
	if err := service.Disable(); err != nil {
		t.Fatalf("Disable missing: %v", err)
	}
	if backend.deleteCalls != 2 || backend.setCalls != 0 {
		t.Fatalf("backend calls after Disable: delete=%d set=%d", backend.deleteCalls, backend.setCalls)
	}
}

func TestBackendErrorsAreReturnedWithoutFallingBackToRealRegistry(t *testing.T) {
	executable := makeTrayExecutable(t, "app")
	backend := &fakeRegistryBackend{getErr: errors.New("fake read failure")}
	service, err := newServiceWithBackend(executable, backend)
	if err != nil {
		t.Fatalf("newServiceWithBackend: %v", err)
	}
	if _, _, err := service.Enabled(); err == nil {
		t.Fatal("Enabled accepted fake backend read failure")
	}
	if backend.getCalls != 1 {
		t.Fatalf("fake Get calls = %d, want 1", backend.getCalls)
	}
}

type fakeRegistryBackend struct {
	value       string
	missing     bool
	getErr      error
	setCalls    int
	deleteCalls int
	getCalls    int
}

func (f *fakeRegistryBackend) Get() (string, error) {
	f.getCalls++
	if f.getErr != nil {
		return "", f.getErr
	}
	if f.missing || f.value == "" {
		return "", errValueNotFound
	}
	return f.value, nil
}

func (f *fakeRegistryBackend) Set(value string) error {
	f.setCalls++
	f.value = value
	f.missing = false
	return nil
}

func (f *fakeRegistryBackend) Delete() error {
	f.deleteCalls++
	if f.missing || f.value == "" {
		return errValueNotFound
	}
	f.value = ""
	f.missing = true
	return nil
}

func makeTrayExecutable(t *testing.T, child string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), child)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create executable directory: %v", err)
	}
	path := filepath.Join(dir, "nodetray.exe")
	if err := os.WriteFile(path, []byte("test executable"), 0o600); err != nil {
		t.Fatalf("write test executable: %v", err)
	}
	return path
}

func makeRenamedTraySymlink(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	payload := filepath.Join(dir, "payload.exe")
	if err := os.WriteFile(payload, []byte("payload"), 0o600); err != nil {
		t.Fatalf("write payload executable: %v", err)
	}
	link := filepath.Join(dir, "nodetray.exe")
	if err := os.Symlink(payload, link); err == nil {
		return link
	}
	if err := os.WriteFile(link, []byte("link fallback"), 0o600); err != nil {
		t.Fatalf("write fallback nodetray executable: %v", err)
	}
	directory, err := os.Open(dir)
	if err != nil {
		t.Fatalf("open fallback directory: %v", err)
	}
	finalDirectory, err := finalDOSPath(windows.Handle(directory.Fd()))
	closeErr := directory.Close()
	if err != nil {
		t.Fatalf("resolve fallback directory: %v", err)
	}
	if closeErr != nil {
		t.Fatalf("close fallback directory: %v", closeErr)
	}
	finalDirectory = strings.TrimPrefix(finalDirectory, `\\?\`)
	finalLink := filepath.Join(finalDirectory, filepath.Base(link))
	oldResolver := resolveFinalExecutablePath
	resolveFinalExecutablePath = func(handle windows.Handle) (string, error) {
		actual, err := finalDOSPath(handle)
		if err != nil {
			return "", err
		}
		if strings.EqualFold(filepath.Clean(strings.TrimPrefix(actual, `\\?\`)), filepath.Clean(finalLink)) {
			return payload, nil
		}
		return actual, nil
	}
	t.Cleanup(func() { resolveFinalExecutablePath = oldResolver })
	return link
}
