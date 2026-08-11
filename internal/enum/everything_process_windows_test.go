//go:build windows

package enum

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStartEverythingClientAtRejectsMissingExecutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Everything.exe")
	err := StartEverythingClientAt(path)
	if err == nil {
		t.Fatal("StartEverythingClientAt returned nil for a missing executable")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error %q does not contain executable path %q", err, path)
	}
}

func TestEverythingClientCommandUsesStartupAndHiddenWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Everything.exe")
	if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	command, err := newEverythingClientCommand(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(command.Args) != 2 || command.Args[1] != "-startup" {
		t.Fatalf("Everything command arguments = %q, want [-startup]", command.Args)
	}
	if command.SysProcAttr == nil || !command.SysProcAttr.HideWindow {
		t.Fatal("Everything command must start with its window hidden")
	}
}
