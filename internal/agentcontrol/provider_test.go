package agentcontrol

import (
	"os"
	"strings"
	"testing"
	"time"

	"dedup/internal/nodectl"
	"dedup/internal/worker"
)

func TestProviderMapsReadyRuntimeAndEffectiveConfigIdentity(t *testing.T) {
	started := time.Date(2026, 8, 2, 8, 30, 0, 123000000, time.UTC)
	workers := &snapshotProvider{snapshot: worker.RuntimeSnapshot{
		Expected: 2,
		Ready:    2,
		Workers: []worker.RuntimeWorkerStatus{
			{Index: 0, PID: 5101, Ready: true, CurrentTaskSummary: "phase=1 task_id=scan-a job_id=1"},
			{Index: 1, PID: 5102, Ready: true},
		},
	}}
	provider := NewProvider(Inputs{
		MachineID:      "machine-provider",
		ExecutablePath: `C:\Program Files\MySingerServer\agent.exe`,
		ConfigSHA256:   strings.Repeat("a", 64),
		StartedAt:      started,
		ListenerReady:  func() bool { return true },
		Workers:        workers,
		SyncHealth:     func() SyncHealth { return SyncHealth{Healthy: true} },
	})

	got := provider.ControlStatus()
	if got.Component != nodectl.ComponentAgent || got.MachineID != "machine-provider" ||
		got.PID != os.Getpid() || got.StartedAtUnixMS != started.UnixMilli() ||
		got.ConfigSHA256 != strings.Repeat("a", 64) {
		t.Fatalf("identity status = %#v", got)
	}
	if !got.ServiceReady || !got.Ready || got.Lifecycle != "running" ||
		got.WorkerExpected != 2 || got.WorkerReady != 2 || len(got.Workers) != 2 ||
		!got.SyncHealthy {
		t.Fatalf("ready status = %#v", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("provider emitted invalid status: %v\n%#v", err, got)
	}
}

func TestProviderStartingStatusRemainsReadableUntilEveryWorkerIsReady(t *testing.T) {
	provider := NewProvider(Inputs{
		MachineID:      "machine-starting",
		ExecutablePath: `C:\agent.exe`,
		ListenerReady:  func() bool { return true },
		Workers: &snapshotProvider{snapshot: worker.RuntimeSnapshot{
			Expected: 2,
			Ready:    1,
			Workers: []worker.RuntimeWorkerStatus{
				{Index: 0, PID: 5201, Ready: true},
				{Index: 1, LastErrorSummary: "worker unavailable; start or respawn pending"},
			},
		}},
		SyncHealth: func() SyncHealth { return SyncHealth{Healthy: true} },
	})

	got := provider.ControlStatus()
	if !got.ServiceReady || got.Ready || got.Lifecycle != "starting" ||
		got.WorkerExpected != 2 || got.WorkerReady != 1 || len(got.Workers) != 2 {
		t.Fatalf("starting status = %#v", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("provider emitted invalid starting status: %v", err)
	}
}

func TestProviderSanitizesWorkerAndSyncDiagnosticsWithoutChangingLifecycle(t *testing.T) {
	secretPath := `D:\private media\folder\secret.mp4`
	secretDSN := "postgres://admin:password@db.example/dedup"
	provider := NewProvider(Inputs{
		MachineID:      "machine-redacted",
		ExecutablePath: `C:\agent.exe`,
		ListenerReady:  func() bool { return true },
		Workers: &snapshotProvider{snapshot: worker.RuntimeSnapshot{
			Expected:         1,
			Ready:            1,
			LastErrorSummary: "env=TOP_SECRET path=" + secretPath,
			Workers: []worker.RuntimeWorkerStatus{{
				Index: 0, PID: 5301, Ready: true,
				CurrentTaskSummary: "phase=2 task_id=scan-secret input=" + secretPath,
				LastErrorSummary:   "dsn=" + secretDSN + " password=hunter2",
			}},
		}},
		SyncHealth: func() SyncHealth {
			return SyncHealth{Healthy: false, ErrorSummary: "sync dsn=" + secretDSN + " media=" + secretPath}
		},
	})

	got := provider.ControlStatus()
	if got.Lifecycle != "running" || !got.ServiceReady || !got.Ready || got.SyncHealthy {
		t.Fatalf("unhealthy sync changed process readiness: %#v", got)
	}
	wire := got.SyncErrorSummary + got.LastErrorSummary +
		got.Workers[0].CurrentTaskSummary + got.Workers[0].LastErrorSummary
	for _, secret := range []string{"TOP_SECRET", secretPath, "admin:password", "hunter2"} {
		if strings.Contains(wire, secret) {
			t.Fatalf("status leaked %q in %q", secret, wire)
		}
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("provider emitted invalid redacted status: %v\n%#v", err, got)
	}
}

func TestProviderSanitizesUNCMediaPathsAcrossPoolWorkerAndSyncSummaries(t *testing.T) {
	unc := `\\fictional-server\fictional-share\private clip.mp4`
	provider := NewProvider(Inputs{
		MachineID:      "machine-unc",
		ExecutablePath: `C:\agent.exe`,
		ListenerReady:  func() bool { return true },
		Workers: &snapshotProvider{snapshot: worker.RuntimeSnapshot{
			Expected: 1, Ready: 1, LastErrorSummary: "pool path=" + unc,
			Workers: []worker.RuntimeWorkerStatus{{
				Index: 0, PID: 5401, Ready: true,
				CurrentTaskSummary: "phase=1 input=" + unc,
				LastErrorSummary:   "worker path=" + unc,
			}},
		}},
		SyncHealth: func() SyncHealth {
			return SyncHealth{Healthy: false, ErrorSummary: "sync path=" + unc}
		},
	})
	got := provider.ControlStatus()
	joined := got.LastErrorSummary + got.SyncErrorSummary +
		got.Workers[0].CurrentTaskSummary + got.Workers[0].LastErrorSummary
	if strings.Contains(joined, "fictional-server") || strings.Contains(joined, "private clip.mp4") {
		t.Fatalf("Provider leaked UNC media path: %q", joined)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("Provider emitted invalid UNC-redacted status: %v", err)
	}
}

type snapshotProvider struct{ snapshot worker.RuntimeSnapshot }

func (p *snapshotProvider) RuntimeSnapshot() worker.RuntimeSnapshot {
	copy := p.snapshot
	copy.Workers = append([]worker.RuntimeWorkerStatus(nil), p.snapshot.Workers...)
	return copy
}
