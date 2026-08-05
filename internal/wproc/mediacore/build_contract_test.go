package mediacore

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLegacyCGOBindingsRequireExplicitBuildTag(t *testing.T) {
	t.Parallel()

	dir := packageDirectory(t)
	for _, name := range []string{"bindings.go", "phase2.go"} {
		source, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(source), "//go:build cgo && windows && legacy_mediacore") {
			t.Errorf("%s can enter a default Windows+cgo worker build", name)
		}
	}

	stub, err := os.ReadFile(filepath.Join(dir, "bindings_stub.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stub), "!legacy_mediacore") {
		t.Error("bindings_stub.go does not cover the default non-legacy build")
	}
}

func packageDirectory(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(file)
}
