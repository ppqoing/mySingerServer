//go:build windows

package process

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

type recordingAgentStarter struct {
	executable string
	args       []string
	pid        int
	err        error
	calls      int
}

func (f *recordingAgentStarter) Start(_ context.Context, executable string, args []string) (int, error) {
	f.calls++
	f.executable = executable
	f.args = append([]string(nil), args...)
	return f.pid, f.err
}

type recordingInspector struct {
	identity Identity
	err      error
	pids     []int
}

func (f *recordingInspector) Inspect(pid int) (Identity, error) {
	f.pids = append(f.pids, pid)
	return f.identity, f.err
}

func (f *recordingInspector) Wait(context.Context, Identity) (int, error) { return 0, nil }

func TestAgentLauncherStartsOnlyFixedVerifiedAgent(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "agent.exe")
	config := filepath.Join(root, "agent.json")
	starter := &recordingAgentStarter{pid: 71}
	inspector := &recordingInspector{identity: Identity{PID: 71, StartedAtUnixMS: 123, ExecutablePath: executable}}
	launcher := newAgentLauncher(inspector, starter)

	identity, err := launcher.Start(context.Background(), executable, []string{"--config", config}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if identity != inspector.identity {
		t.Fatalf("identity = %+v, want %+v", identity, inspector.identity)
	}
	if starter.calls != 1 || starter.executable != executable {
		t.Fatalf("starter call = %d executable = %q, want one call for %q", starter.calls, starter.executable, executable)
	}
	if got, want := len(starter.args), 2; got != want || starter.args[0] != "--config" || starter.args[1] != config {
		t.Fatalf("starter args = %q, want [--config %q]", starter.args, config)
	}
	if len(inspector.pids) != 1 || inspector.pids[0] != 71 {
		t.Fatalf("inspected PIDs = %v, want [71]", inspector.pids)
	}
}

func TestAgentLauncherRejectsAnythingOutsideFixedContract(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "agent.exe")
	config := filepath.Join(root, "agent.json")
	cases := []struct {
		name       string
		executable string
		args       []string
		env        []string
	}{
		{"relative executable", "agent.exe", []string{"--config", config}, nil},
		{"wrong basename", filepath.Join(root, "helper.exe"), []string{"--config", config}, nil},
		{"wrong flag", executable, []string{"--other", config}, nil},
		{"extra argument", executable, []string{"--config", config, "extra"}, nil},
		{"relative config", executable, []string{"--config", "agent.json"}, nil},
		{"environment", executable, []string{"--config", config}, []string{"X=1"}},
		{"control character", executable, []string{"--config", config + "\n"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			starter := &recordingAgentStarter{pid: 71}
			launcher := newAgentLauncher(&recordingInspector{}, starter)
			if _, err := launcher.Start(context.Background(), tc.executable, tc.args, tc.env); !errors.Is(err, errAgentLaunchArguments) {
				t.Fatalf("Start error = %v, want errAgentLaunchArguments", err)
			}
			if starter.calls != 0 {
				t.Fatalf("starter calls = %d, want 0", starter.calls)
			}
		})
	}
}

func TestAgentLauncherRejectsUnverifiedStartedProcess(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "agent.exe")
	config := filepath.Join(root, "agent.json")
	starter := &recordingAgentStarter{pid: 71}
	inspector := &recordingInspector{identity: Identity{PID: 72, StartedAtUnixMS: 123, ExecutablePath: executable}}
	launcher := newAgentLauncher(inspector, starter)

	if _, err := launcher.Start(context.Background(), executable, []string{"--config", config}, nil); !errors.Is(err, errAgentLaunchIdentity) {
		t.Fatalf("Start error = %v, want errAgentLaunchIdentity", err)
	}
	if starter.calls != 1 || len(inspector.pids) != 1 {
		t.Fatalf("starter calls = %d inspector calls = %d, want one each", starter.calls, len(inspector.pids))
	}
}
