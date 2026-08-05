//go:build windows

package process

import (
	"errors"
	"path/filepath"
	"testing"
)

type recordingTerminatorBackend struct {
	handle       uintptr
	openErr      error
	terminateErr error
	openPIDs     []int
	terminated   []uintptr
	exitCodes    []uint32
	closed       []uintptr
}

func (f *recordingTerminatorBackend) OpenForTerminate(pid int) (uintptr, error) {
	f.openPIDs = append(f.openPIDs, pid)
	return f.handle, f.openErr
}

func (f *recordingTerminatorBackend) Terminate(handle uintptr, exitCode uint32) error {
	f.terminated = append(f.terminated, handle)
	f.exitCodes = append(f.exitCodes, exitCode)
	return f.terminateErr
}

func (f *recordingTerminatorBackend) CloseProcessHandle(handle uintptr) {
	f.closed = append(f.closed, handle)
}

type handleRecordingInspector struct {
	recordingInspector
	handleIdentity Identity
	handleErr      error
	handles        []uintptr
}

func (f *handleRecordingInspector) InspectHandle(handle uintptr) (Identity, error) {
	f.handles = append(f.handles, handle)
	return f.handleIdentity, f.handleErr
}

func TestTrustedTerminatorTerminatesOnlyIdentityBoundToOpenedHandle(t *testing.T) {
	identity := testAgentIdentity(t)
	backend := &recordingTerminatorBackend{handle: 99}
	inspector := &handleRecordingInspector{handleIdentity: identity}
	terminator := newTrustedTerminator(inspector, backend)

	if err := terminator.Terminate(identity, 1); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if got := backend.openPIDs; len(got) != 1 || got[0] != identity.PID {
		t.Fatalf("opened PIDs = %v, want [%d]", got, identity.PID)
	}
	if got := inspector.handles; len(got) != 1 || got[0] != 99 {
		t.Fatalf("inspected handles = %v, want [99]", got)
	}
	if got := backend.terminated; len(got) != 1 || got[0] != 99 || backend.exitCodes[0] != 1 {
		t.Fatalf("terminate calls = handles %v codes %v, want handle 99 code 1", backend.terminated, backend.exitCodes)
	}
	if got := backend.closed; len(got) != 1 || got[0] != 99 {
		t.Fatalf("closed handles = %v, want [99]", got)
	}
}

func TestTrustedTerminatorRejectsIdentityDriftBeforeTerminate(t *testing.T) {
	identity := testAgentIdentity(t)
	cases := []struct {
		name   string
		actual Identity
	}{
		{"pid reuse", Identity{PID: identity.PID, StartedAtUnixMS: identity.StartedAtUnixMS + 1, ExecutablePath: identity.ExecutablePath}},
		{"creation time drift", Identity{PID: identity.PID, StartedAtUnixMS: identity.StartedAtUnixMS - 1, ExecutablePath: identity.ExecutablePath}},
		{"path drift", Identity{PID: identity.PID, StartedAtUnixMS: identity.StartedAtUnixMS, ExecutablePath: filepath.Join(t.TempDir(), "agent.exe")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := &recordingTerminatorBackend{handle: 99}
			inspector := &handleRecordingInspector{handleIdentity: tc.actual}
			terminator := newTrustedTerminator(inspector, backend)
			if err := terminator.Terminate(identity, 1); !errors.Is(err, errTrustedTerminationIdentity) {
				t.Fatalf("Terminate error = %v, want errTrustedTerminationIdentity", err)
			}
			if len(backend.terminated) != 0 {
				t.Fatalf("terminated handles = %v, want none", backend.terminated)
			}
			if got := backend.closed; len(got) != 1 || got[0] != 99 {
				t.Fatalf("closed handles = %v, want [99]", got)
			}
		})
	}
}

func TestTrustedTerminatorFailsClosedAtEveryPreTerminationFailure(t *testing.T) {
	identity := testAgentIdentity(t)
	cases := []struct {
		name      string
		identity  Identity
		inspector Inspector
		backend   *recordingTerminatorBackend
		wantClose bool
	}{
		{"incomplete identity", Identity{}, &handleRecordingInspector{}, &recordingTerminatorBackend{handle: 99}, false},
		{"no handle inspector", identity, &recordingInspector{}, &recordingTerminatorBackend{handle: 99}, false},
		{"open failure", identity, &handleRecordingInspector{}, &recordingTerminatorBackend{openErr: errors.New("denied")}, false},
		{"handle inspect failure", identity, &handleRecordingInspector{handleErr: errors.New("gone")}, &recordingTerminatorBackend{handle: 99}, true},
		{"terminate failure", identity, &handleRecordingInspector{handleIdentity: identity}, &recordingTerminatorBackend{handle: 99, terminateErr: errors.New("denied")}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			terminator := newTrustedTerminator(tc.inspector, tc.backend)
			if err := terminator.Terminate(tc.identity, 1); err == nil {
				t.Fatal("Terminate unexpectedly succeeded")
			}
			if tc.backend.terminateErr == nil && len(tc.backend.terminated) != 0 {
				t.Fatalf("terminated handles = %v, want none", tc.backend.terminated)
			}
			if got := len(tc.backend.closed) > 0; got != tc.wantClose {
				t.Fatalf("handle close = %v, want %v; closed = %v", got, tc.wantClose, tc.backend.closed)
			}
		})
	}
}

func TestDirectTerminatorUsesRecordedPIDWithoutIdentityInspection(t *testing.T) {
	backend := &recordingTerminatorBackend{handle: 88}
	terminator := newDirectTerminator(backend)

	if err := terminator.Terminate(Identity{PID: 321}, 7); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if got := backend.openPIDs; len(got) != 1 || got[0] != 321 {
		t.Fatalf("opened PIDs = %v, want [321]", got)
	}
	if got := backend.terminated; len(got) != 1 || got[0] != 88 || backend.exitCodes[0] != 7 {
		t.Fatalf("terminate calls = handles %v codes %v, want handle 88 code 7", backend.terminated, backend.exitCodes)
	}
	if got := backend.closed; len(got) != 1 || got[0] != 88 {
		t.Fatalf("closed handles = %v, want [88]", got)
	}
}

func TestDirectTerminatorRejectsInvalidPIDWithoutOpeningProcess(t *testing.T) {
	backend := &recordingTerminatorBackend{handle: 88}
	terminator := newDirectTerminator(backend)

	if err := terminator.Terminate(Identity{}, 1); err == nil {
		t.Fatal("Terminate unexpectedly accepted a zero PID")
	}
	if len(backend.openPIDs) != 0 {
		t.Fatalf("opened PIDs = %v, want none", backend.openPIDs)
	}
}

func testAgentIdentity(t *testing.T) Identity {
	t.Helper()
	return Identity{PID: 71, StartedAtUnixMS: 123, ExecutablePath: filepath.Join(t.TempDir(), "agent.exe")}
}
