package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveGUIRuntimePathsUsesExecutableDirectoryInsteadOfWorkingDirectory(t *testing.T) {
	paths, err := resolveGUIRuntimePaths(`D:\管理 工具\gui.exe`, "")
	if err != nil {
		t.Fatal(err)
	}
	if paths.Root != `D:\管理 工具` {
		t.Fatalf("root = %q", paths.Root)
	}
	if paths.ConfigPath != `D:\管理 工具\gui.json` {
		t.Fatalf("config = %q", paths.ConfigPath)
	}
	if paths.LogPath != `D:\管理 工具\data\logs\gui.log` {
		t.Fatalf("log = %q", paths.LogPath)
	}
}

func TestResolveGUIRuntimePathsKeepsExplicitConfigOverride(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "gui.exe")
	paths, err := resolveGUIRuntimePaths(exe, `config\custom.json`)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs(`config\custom.json`)
	if err != nil {
		t.Fatal(err)
	}
	if paths.ConfigPath != want {
		t.Fatalf("config = %q, want %q", paths.ConfigPath, want)
	}
}

func TestGUIRuntimeLoggerWritesPortableLogAndConsole(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "data", "logs", "gui.log")
	var console bytes.Buffer
	logger, closeLogger, err := newGUIRuntimeLogger(logPath, &console)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("portable runtime log", "event", "logger-test")
	if err := closeLogger(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(console.String(), "portable runtime log") {
		t.Fatalf("console output = %q", console.String())
	}
	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "portable runtime log") {
		t.Fatalf("log output = %q", content)
	}
}

func TestLocalBrowserURLMapsWildcardListenersToLoopback(t *testing.T) {
	for address, want := range map[string]string{
		"0.0.0.0:8080": "http://127.0.0.1:8080/",
		"[::]:8080":    "http://[::1]:8080/",
	} {
		got, err := localBrowserURL(address)
		if err != nil {
			t.Fatalf("localBrowserURL(%q): %v", address, err)
		}
		if got != want {
			t.Fatalf("localBrowserURL(%q) = %q, want %q", address, got, want)
		}
	}
}
