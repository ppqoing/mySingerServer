//go:build windows

package main

import "testing"

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
