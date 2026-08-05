//go:build windows

package diskmap

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMountPointAndResolveCurrentVolume(t *testing.T) {
	path, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mount, err := MountPointOf(path)
	if err != nil {
		t.Fatalf("MountPointOf(%q): %v", path, err)
	}
	volume := strings.ToLower(filepath.VolumeName(path))
	if !strings.HasPrefix(strings.ToLower(mount), volume) {
		t.Fatalf("mount = %q, want volume %q", mount, volume)
	}
	info, err := Resolve(mount)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", mount, err)
	}
	if info.MountPoint == "" || info.VolumeGUID == "" {
		t.Fatalf("incomplete disk info: %#v", info)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("test volume became inaccessible: %v", err)
	}
}

func TestInterpretSeekPenaltyFailureUsesUnknownHDDFallback(t *testing.T) {
	isSSD, known := interpretSeekPenalty([12]byte{}, errors.New("unsupported"))
	if isSSD || known {
		t.Fatalf("failure = isSSD:%v known:%v, want conservative unknown HDD", isSSD, known)
	}
	var descriptor [12]byte
	descriptor[8] = 0
	isSSD, known = interpretSeekPenalty(descriptor, nil)
	if !isSSD || !known {
		t.Fatalf("no seek penalty = isSSD:%v known:%v, want known SSD", isSSD, known)
	}
}
