package helpercontrol

import (
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dedup/internal/nodectl"
)

func TestProviderMapsDeleteListenerAndActiveRequestsWithoutWorkerState(t *testing.T) {
	started := time.Date(2026, 8, 2, 9, 15, 0, 456000000, time.UTC)
	deletes := &deleteServiceSnapshot{}
	deletes.listening.Store(true)
	deletes.active.Store(1)
	provider := NewProvider(Inputs{
		MachineID:      "helper-machine",
		ExecutablePath: `C:\Program Files\MySingerServer\helper.exe`,
		ConfigSHA256:   strings.Repeat("A", 64),
		StartedAt:      started,
		DeleteService:  deletes,
	})

	got := provider.ControlStatus()
	if got.Component != nodectl.ComponentHelper || got.MachineID != "helper-machine" ||
		got.PID != os.Getpid() || got.StartedAtUnixMS != started.UnixMilli() ||
		got.ExecutablePath != `C:\Program Files\MySingerServer\helper.exe` ||
		got.ConfigSHA256 != strings.Repeat("a", 64) {
		t.Fatalf("identity status = %#v", got)
	}
	if !got.ServiceReady || !got.Ready || got.Lifecycle != "running" || got.ActiveRequests != 1 {
		t.Fatalf("ready delete status = %#v", got)
	}
	if got.WorkerExpected != 0 || got.WorkerReady != 0 || len(got.Workers) != 0 ||
		got.SyncHealthy || got.SyncErrorSummary != "" {
		t.Fatalf("Helper leaked Agent-only state: %#v", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("provider emitted invalid status: %v\n%#v", err, got)
	}
}

func TestProviderReadyTracksDeleteListenerAndClampsInvalidCounter(t *testing.T) {
	deletes := &deleteServiceSnapshot{}
	deletes.active.Store(-1)
	provider := NewProvider(Inputs{
		MachineID:      "helper-starting",
		ExecutablePath: `C:\helper.exe`,
		StartedAt:      time.Unix(1, 0),
		DeleteService:  deletes,
	})

	starting := provider.ControlStatus()
	if starting.ServiceReady || starting.Ready || starting.Lifecycle != "starting" ||
		starting.ActiveRequests != 0 {
		t.Fatalf("non-listening status = %#v", starting)
	}
	if err := starting.Validate(); err != nil {
		t.Fatalf("provider emitted invalid starting status: %v", err)
	}

	deletes.active.Store(2)
	deletes.listening.Store(true)
	running := provider.ControlStatus()
	if !running.ServiceReady || !running.Ready || running.Lifecycle != "running" ||
		running.ActiveRequests != 2 {
		t.Fatalf("updated delete status = %#v", running)
	}
}

type deleteServiceSnapshot struct {
	active    atomic.Int64
	listening atomic.Bool
}

func (s *deleteServiceSnapshot) ActiveRequests() int { return int(s.active.Load()) }
func (s *deleteServiceSnapshot) Listening() bool     { return s.listening.Load() }
