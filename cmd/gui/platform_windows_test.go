//go:build windows

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/sys/windows"
)

type fakeGUIReplacementProcess struct {
	events *[]string
}

func (p *fakeGUIReplacementProcess) Release() error {
	*p.events = append(*p.events, "release")
	return nil
}

func (p *fakeGUIReplacementProcess) Kill() error {
	*p.events = append(*p.events, "kill")
	return nil
}

func (p *fakeGUIReplacementProcess) Wait() (*os.ProcessState, error) {
	*p.events = append(*p.events, "wait")
	return nil, nil
}

func TestResolveGUIExecutablePathUsesFinalImageInsteadOfLaunchAlias(t *testing.T) {
	alias := `D:\junction\manager\gui.exe`
	final := `E:\portable\MySingerServer-Manager\gui.exe`
	var inspected string
	got, err := resolveGUIExecutablePath(
		func() (string, error) { return alias, nil },
		func(path string) (string, error) {
			inspected = path
			return final, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if inspected != alias {
		t.Fatalf("final-path resolver inspected %q, want launch alias %q", inspected, alias)
	}
	if got != final {
		t.Fatalf("executable = %q, want final image %q", got, final)
	}
	paths, err := resolveGUIRuntimePaths(got, "")
	if err != nil {
		t.Fatal(err)
	}
	if paths.Root != `E:\portable\MySingerServer-Manager` ||
		paths.ConfigPath != `E:\portable\MySingerServer-Manager\gui.json` ||
		paths.LogPath != `E:\portable\MySingerServer-Manager\data\logs\gui.log` {
		t.Fatalf("runtime paths used alias instead of final image: %#v", paths)
	}
}

func TestResolveGUIExecutablePathRejectsFinalUNCImage(t *testing.T) {
	got, err := resolveGUIExecutablePath(
		func() (string, error) { return `Z:\manager\gui.exe`, nil },
		func(string) (string, error) { return `\\server\share\manager\gui.exe`, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if got != `\\server\share\manager\gui.exe` {
		t.Fatalf("executable = %q", got)
	}
	if _, err := resolveGUIRuntimePaths(got, ""); err == nil {
		t.Fatal("runtime paths accepted an executable whose final image is UNC")
	}
}

func TestGUIRestartStartsAbsoluteExecutableAndReleasesHandle(t *testing.T) {
	originalStart := guiStartProcess
	defer func() { guiStartProcess = originalStart }()
	events := []string{}
	executable := filepath.Join(t.TempDir(), "gui.exe")
	wantArgs := []string{"-config", filepath.Join(t.TempDir(), "gui.json"), "-no-browser"}
	guiStartProcess = func(exe string, args []string) (guiReplacementProcess, error) {
		events = append(events, "start")
		if exe != executable {
			t.Fatalf("executable=%q want=%q", exe, executable)
		}
		if !reflect.DeepEqual(args, wantArgs) {
			t.Fatalf("args=%q want=%q", args, wantArgs)
		}
		return &fakeGUIReplacementProcess{events: &events}, nil
	}

	if err := guiStartReplacement(executable, wantArgs); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"start", "release"}) {
		t.Fatalf("events=%v", events)
	}
}

func TestGUIPreparedReplacementAbortKillsAndWaitsWithoutSleep(t *testing.T) {
	originalStart := guiStartProcess
	defer func() { guiStartProcess = originalStart }()
	events := []string{}
	guiStartProcess = func(string, []string) (guiReplacementProcess, error) {
		events = append(events, "start")
		return &fakeGUIReplacementProcess{events: &events}, nil
	}

	replacement, err := guiPrepareReplacement(filepath.Join(t.TempDir(), "gui.exe"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := replacement.Abort(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"start", "kill", "wait"}) {
		t.Fatalf("events=%v", events)
	}
}

func TestGUIWaitForParentUsesWindowsProcessSynchronization(t *testing.T) {
	originalOpen := guiOpenProcess
	originalWait := guiWaitForSingleObject
	originalClose := guiCloseHandle
	defer func() {
		guiOpenProcess = originalOpen
		guiWaitForSingleObject = originalWait
		guiCloseHandle = originalClose
	}()
	events := []string{}
	const (
		pid    = 4321
		handle = windows.Handle(99)
	)
	guiOpenProcess = func(access uint32, inherit bool, gotPID uint32) (windows.Handle, error) {
		events = append(events, "open")
		if access != windows.SYNCHRONIZE || inherit || gotPID != pid {
			t.Fatalf("OpenProcess(access=%#x inherit=%t pid=%d)", access, inherit, gotPID)
		}
		return handle, nil
	}
	guiWaitForSingleObject = func(gotHandle windows.Handle, milliseconds uint32) (uint32, error) {
		events = append(events, "wait")
		if gotHandle != handle || milliseconds != windows.INFINITE {
			t.Fatalf("WaitForSingleObject(handle=%d milliseconds=%d)", gotHandle, milliseconds)
		}
		return windows.WAIT_OBJECT_0, nil
	}
	guiCloseHandle = func(gotHandle windows.Handle) error {
		events = append(events, "close")
		if gotHandle != handle {
			t.Fatalf("CloseHandle(handle=%d)", gotHandle)
		}
		return nil
	}

	if err := guiWaitForParent(pid); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"open", "wait", "close"}) {
		t.Fatalf("events=%v", events)
	}
}
