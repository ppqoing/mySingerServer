//go:build !cgo || !windows

package videocore

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUnavailableRuntime(t *testing.T) {
	if _, err := Runtime(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Runtime() error = %v, want ErrUnavailable", err)
	}
}

func TestUnavailableOpen(t *testing.T) {
	session, err := Open(context.Background(), `D:\media\sample.mp4`, OpenOptions{})
	if session != nil {
		_ = session.Close()
		t.Fatalf("Open() returned session %#v", session)
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Open() error = %v, want ErrUnavailable", err)
	}
}

func TestCGOSourceInitializesEveryABIStructure(t *testing.T) {
	repo := testRepoRoot(t)
	source, err := os.ReadFile(filepath.Join(repo, "internal", "wproc", "videocore", "bindings.go"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, required := range []string{
		"//go:build cgo && windows",
		`import "C"`,
		"go_vc_init_error",
		"go_vc_init_runtime_info",
		"go_vc_init_open_options",
		"go_vc_init_analysis_request",
		"go_vc_init_analysis_result",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("CGO binding is missing ABI initializer %q", required)
		}
	}
}

func TestCGOScriptVideoCoreModeFailsClosedWithoutImportLibrary(t *testing.T) {
	repo := testRepoRoot(t)
	command := exec.Command(
		testPowerShell(t), "-NoProfile", "-File", filepath.Join(repo, "scripts", "test-cgo.ps1"),
		"-Mode", "VideoCore", "-DllDir", filepath.Join("videocore", "build", "Release"),
		"-Packages", "./internal/wproc/videocore",
	)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatal("VideoCore CGO mode succeeded without libvideocore.a")
	}
	if !strings.Contains(string(output), "libvideocore.a") {
		t.Fatalf("VideoCore fail-closed output = %s, want missing libvideocore.a", output)
	}
}

func TestCGOScriptMediaCoreModeRemainsCompatible(t *testing.T) {
	repo := testRepoRoot(t)
	fakeGo := filepath.Join(t.TempDir(), "fake-go.cmd")
	if err := os.WriteFile(fakeGo, []byte("@echo off\r\nexit /b 0\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(
		testPowerShell(t), "-NoProfile", "-File", filepath.Join(repo, "scripts", "test-cgo.ps1"),
		"-Mode", "MediaCore", "-Go", fakeGo, "-DllDir", "bin",
		"-Packages", "./internal/wproc/mediacore",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("legacy MediaCore mode failed: %v\n%s", err, output)
	}
}

func testRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

func testPowerShell(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("pwsh.exe")
	if err != nil {
		t.Skip("pwsh.exe is unavailable")
	}
	return path
}
