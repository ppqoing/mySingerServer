package finalpath

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveExistingReturnsAbsoluteExistingFilePath(t *testing.T) {
	file := filepath.Join(t.TempDir(), "gui.exe")
	if err := os.WriteFile(file, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveExisting(file)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(filepath.Clean(got), filepath.Clean(want)) {
		t.Fatalf("final path = %q, want %q", got, want)
	}
}
