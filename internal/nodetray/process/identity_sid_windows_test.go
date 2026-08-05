//go:build windows

package process

import (
	"errors"
	"testing"
)

type fakeSIDBackend struct {
	handle     uintptr
	identities []Identity
	sid        string
	openPID    int
	closeCalls int
}

func (f *fakeSIDBackend) Open(pid int) (uintptr, error) {
	f.openPID = pid
	if f.handle == 0 {
		return 0, errors.New("open failed")
	}
	return f.handle, nil
}

func (f *fakeSIDBackend) Inspect(_ uintptr, _ int) (Identity, error) {
	if len(f.identities) == 0 {
		return Identity{}, errors.New("missing identity")
	}
	value := f.identities[0]
	f.identities = f.identities[1:]
	return value, nil
}

func (f *fakeSIDBackend) UserSID(uintptr) (string, error) { return f.sid, nil }
func (f *fakeSIDBackend) Close(uintptr) error             { f.closeCalls++; return nil }

func TestUserSIDForProcessRevalidatesIdentityAroundTokenRead(t *testing.T) {
	expected := Identity{PID: 42, StartedAtUnixMS: 100, ExecutablePath: `C:\Program Files\MySingerServer\nodetray.exe`}
	backend := &fakeSIDBackend{
		handle:     77,
		identities: []Identity{expected, expected},
		sid:        "S-1-5-21-101-202-303-1001",
	}

	got, err := userSIDForProcessWithBackend(expected, backend)
	if err != nil {
		t.Fatalf("userSIDForProcessWithBackend: %v", err)
	}
	if got != backend.sid || backend.openPID != expected.PID || backend.closeCalls != 1 {
		t.Fatalf("SID result=%q openPID=%d closeCalls=%d", got, backend.openPID, backend.closeCalls)
	}
}

func TestUserSIDForProcessRejectsIdentityDriftAfterTokenRead(t *testing.T) {
	expected := Identity{PID: 42, StartedAtUnixMS: 100, ExecutablePath: `C:\Program Files\MySingerServer\nodetray.exe`}
	drifted := expected
	drifted.StartedAtUnixMS++
	backend := &fakeSIDBackend{
		handle:     77,
		identities: []Identity{expected, drifted},
		sid:        "S-1-5-21-101-202-303-1001",
	}

	if _, err := userSIDForProcessWithBackend(expected, backend); err == nil {
		t.Fatal("identity drift after token read was accepted")
	}
	if backend.closeCalls != 1 {
		t.Fatalf("process handle close calls=%d, want 1", backend.closeCalls)
	}
}
