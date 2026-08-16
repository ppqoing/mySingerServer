//go:build windows

package diskmap

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func withDeviceControl(t *testing.T, fn func(windows.Handle, uint32, *byte, uint32, *byte, uint32, *uint32, *windows.Overlapped) error) {
	t.Helper()
	original := deviceIoControl
	deviceIoControl = fn
	t.Cleanup(func() { deviceIoControl = original })
}

func writeDiskExtents(t *testing.T, out *byte, outLen uint32, returned *uint32, diskNos ...uint32) {
	t.Helper()
	need := volumeDiskExtentsHeaderSize + diskExtentSize*len(diskNos)
	if out == nil || int(outLen) < need {
		t.Fatalf("extent output buffer = %d, need %d", outLen, need)
	}
	buffer := unsafe.Slice(out, outLen)
	binary.LittleEndian.PutUint32(buffer[:4], uint32(len(diskNos)))
	for index, diskNo := range diskNos {
		offset := volumeDiskExtentsHeaderSize + index*diskExtentSize
		binary.LittleEndian.PutUint32(buffer[offset+16:offset+20], diskNo)
	}
	*returned = uint32(need)
}

func equalDiskNumbers(got, want []uint32) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

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

func TestDiskIdentityUsesStablePhysicalKeyForSharedDiskNumber(t *testing.T) {
	withDeviceControl(t, func(_ windows.Handle, ioctl uint32, _ *byte, _ uint32, out *byte, outLen uint32, returned *uint32, _ *windows.Overlapped) error {
		if ioctl != ioctlVolumeGetVolumeDiskExtents {
			return errors.New("unexpected IOCTL")
		}
		writeDiskExtents(t, out, outLen, returned, 3)
		return nil
	})

	first := resolveIdentity(0, `\\?\Volume{first}\\`)
	second := resolveIdentity(0, `\\?\Volume{second}\\`)
	if first.Key != "physical:3" || second.Key != first.Key {
		t.Fatalf("shared disk keys = %q, %q; want physical:3", first.Key, second.Key)
	}
}

func TestDiskIdentitySortsMultiDiskExtentNumbers(t *testing.T) {
	withDeviceControl(t, func(_ windows.Handle, ioctl uint32, _ *byte, _ uint32, out *byte, outLen uint32, returned *uint32, _ *windows.Overlapped) error {
		if ioctl != ioctlVolumeGetVolumeDiskExtents {
			return errors.New("unexpected IOCTL")
		}
		writeDiskExtents(t, out, outLen, returned, 9, 2, 9)
		return nil
	})

	identity := resolveIdentity(0, `\\?\Volume{spanned}\\`)
	if identity.Key != "physical-set:2,9" {
		t.Fatalf("key = %q, want physical-set:2,9", identity.Key)
	}
	if got, want := identity.DiskNos, []uint32{2, 9}; !equalDiskNumbers(got, want) {
		t.Fatalf("DiskNos = %#v, want %#v", got, want)
	}
}

func TestDiskIdentityFallsBackToVolumeWhenExtentsFail(t *testing.T) {
	withDeviceControl(t, func(_ windows.Handle, ioctl uint32, _ *byte, _ uint32, _ *byte, _ uint32, _ *uint32, _ *windows.Overlapped) error {
		if ioctl != ioctlVolumeGetVolumeDiskExtents {
			return errors.New("unexpected IOCTL")
		}
		return errors.New("not supported")
	})

	identity := resolveIdentity(0, `\\?\Volume{fallback}\\`)
	if identity.Key != "volume:\\\\?\\Volume{fallback}" {
		t.Fatalf("fallback key = %q", identity.Key)
	}
}

func TestDiskIdentityUsesNetworkKeyForUNC(t *testing.T) {
	info, err := Resolve(`\\server\share\folder`)
	if err != nil {
		t.Fatalf("Resolve UNC: %v", err)
	}
	if info.Identity.Key != "network:server/share" || info.Identity.Local {
		t.Fatalf("UNC identity = %#v", info.Identity)
	}
}

func TestDiskIdentityNormalizesExtendedUNCToClassicNetworkKey(t *testing.T) {
	classic, classicOK := networkIdentity(`\\SERVER\Share\folder`)
	extended, extendedOK := networkIdentity(`\\?\UNC\SERVER\Share\folder`)
	if !classicOK || !extendedOK {
		t.Fatalf("network recognition = classic:%v extended:%v", classicOK, extendedOK)
	}
	if classic.Key != "network:server/share" || extended.Key != classic.Key {
		t.Fatalf("network keys = classic:%q extended:%q, want network:server/share", classic.Key, extended.Key)
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
