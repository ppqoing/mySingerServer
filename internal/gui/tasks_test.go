package gui

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"dedup/internal/proto"
)

func TestTaskRegistryDispatchesCompleteStateMachine(t *testing.T) {
	registry := NewTaskRegistry(nil, testLogger())
	registry.Register(&TaskInfo{
		TaskID: "task-1", MachineID: "machine-a", Phase: 1,
		Roots: []string{`D:\media`}, Status: "sent", UpdatedAt: time.Now(),
	})
	registry.Dispatch("machine-a", &proto.TaskAck{
		TaskID: "task-1", Accepted: true, Reason: "accepted", Total: -1,
	})
	registry.Dispatch("machine-a", &proto.TaskProgress{
		TaskID: "task-1", Done: 5, Total: 10, Speed: 2.5,
	})
	items := make([]proto.FeatureItem, 60)
	for index := range items {
		items[index] = proto.FeatureItem{Path: string(rune('a' + index))}
	}
	registry.Dispatch("machine-a", &proto.FeatureResult{
		TaskID: "task-1", Seq: 1, Items: items,
	})
	registry.Dispatch("machine-a", &proto.Error{
		TaskID: "task-1", Stage: "hash", Msg: "one file failed",
	})
	registry.Dispatch("machine-a", &proto.TaskDone{
		TaskID: "task-1",
		Stats:  proto.TaskStats{Total: 10, Done: 10, Failed: 1},
	})

	list := registry.List()
	if len(list) != 1 {
		t.Fatalf("tasks = %d, want 1", len(list))
	}
	got := list[0]
	if got.Status != "done" || got.Done != 10 || got.Total != 10 ||
		got.LastErr != "one file failed" || len(got.Recent) != 50 {
		t.Fatalf("task = %#v", got)
	}
	if got.Recent[0].Path != items[10].Path {
		t.Fatalf("recent first = %q, want %q", got.Recent[0].Path, items[10].Path)
	}
}

func TestTaskRegistryDetectsFeatureSequenceGap(t *testing.T) {
	registry := NewTaskRegistry(nil, testLogger())
	registry.Register(&TaskInfo{TaskID: "task-1", Status: "sent"})
	registry.Dispatch("machine-a", &proto.FeatureResult{
		TaskID: "task-1", Seq: 2,
	})
	got := registry.List()[0]
	if got.LastErr == "" {
		t.Fatal("feature sequence gap was not recorded")
	}
}

func TestRejectedAckMarksTaskFailed(t *testing.T) {
	registry := NewTaskRegistry(nil, testLogger())
	registry.Register(&TaskInfo{TaskID: "task-1", Status: "sent"})
	registry.Dispatch("machine-a", &proto.TaskAck{
		TaskID: "task-1", Accepted: false, Reason: "rejected:bad roots",
	})
	got := registry.List()[0]
	if got.Status != "failed" || got.LastErr != "rejected:bad roots" {
		t.Fatalf("task = %#v", got)
	}
}

func TestTaskRegistryCompletesFromAlreadyDoneAck(t *testing.T) {
	registry := NewTaskRegistry(nil, testLogger())
	registry.Register(&TaskInfo{
		TaskID: "task-finished-offline", MachineID: "machine-a",
		Phase: 1, Roots: []string{`D:\media`}, Status: "sent",
	})
	stats := proto.TaskStats{
		Total: 10, Done: 8, Skipped: 2, Failed: 1, ElapsedMS: 345,
	}
	registry.Dispatch("machine-a", &proto.TaskAck{
		TaskID: "task-finished-offline", Accepted: true,
		Reason: "already_done", Total: 10, Stats: &stats,
	})

	got := registry.List()[0]
	if got.Status != "done" || got.Total != 10 || got.Done != 8 ||
		got.Skipped != 2 || got.Failed != 1 || got.ElapsedMS != 345 {
		t.Fatalf("restored completion = %#v", got)
	}
}

func TestTaskRegistryReturnsPendingScanEnvelopes(t *testing.T) {
	registry := NewTaskRegistry(nil, testLogger())
	for _, task := range []*TaskInfo{
		{
			TaskID: "task-running", MachineID: "machine-a", Phase: 1,
			Roots: []string{`D:\one`}, Rescan: true, Status: "running",
		},
		{
			TaskID: "task-sent", MachineID: "machine-a", Phase: 1,
			Roots: []string{`D:\two`}, Status: "sent",
		},
		{
			TaskID: "task-done", MachineID: "machine-a", Phase: 1,
			Roots: []string{`D:\done`}, Status: "done",
		},
		{
			TaskID: "task-other-machine", MachineID: "machine-b", Phase: 1,
			Roots: []string{`E:\media`}, Status: "running",
		},
	} {
		registry.Register(task)
	}

	got := registry.PendingScans("machine-a")
	if len(got) != 2 {
		t.Fatalf("pending scans = %#v, want 2", got)
	}
	if got[0].TaskID != "task-running" || !got[0].Options.Rescan ||
		got[0].Roots[0] != `D:\one` {
		t.Fatalf("first pending scan = %#v", got[0])
	}
	if got[1].TaskID != "task-sent" || got[1].Options.Rescan ||
		got[1].Roots[0] != `D:\two` {
		t.Fatalf("second pending scan = %#v", got[1])
	}
}

func TestTaskRegistryMarksScanLevelCompletionAsFailed(t *testing.T) {
	registry := NewTaskRegistry(nil, testLogger())
	registry.Register(&TaskInfo{
		TaskID: "task-scan-error", MachineID: "machine-a", Status: "running",
	})
	registry.Dispatch("machine-a", &proto.TaskDone{
		TaskID: "task-scan-error",
		Stats: proto.TaskStats{
			Total: 0, Done: 0, Failed: 1, ScanErrors: 1,
		},
	})
	got := registry.List()[0]
	if got.Status != "failed" || got.ScanErrors != 1 {
		t.Fatalf("task = %#v, want failed scan-level completion", got)
	}
}

func TestTaskRegistryTerminalStateIgnoresLateAckAndProgress(t *testing.T) {
	registry := NewTaskRegistry(nil, testLogger())
	registry.Register(&TaskInfo{
		TaskID: "task-terminal", MachineID: "machine-a", Status: "running",
	})
	registry.Dispatch("machine-a", &proto.TaskDone{
		TaskID: "task-terminal",
		Stats: proto.TaskStats{
			Total: 10, Done: 7, Failed: 1, ScanErrors: 1,
		},
	})
	registry.Dispatch("machine-a", &proto.TaskAck{
		TaskID: "task-terminal", Accepted: true, Reason: "accepted", Total: -1,
	})
	registry.Dispatch("machine-a", &proto.TaskProgress{
		TaskID: "task-terminal", Done: 1, Total: 2, Speed: 3,
	})

	got := registry.List()[0]
	if got.Status != "failed" || got.Total != 10 || got.Done != 7 ||
		got.ScanErrors != 1 {
		t.Fatalf("terminal task regressed = %#v", got)
	}
}

func TestTaskRegistryAlreadyDoneWithoutStatsIsTerminal(t *testing.T) {
	registry := NewTaskRegistry(nil, testLogger())
	registry.Register(&TaskInfo{
		TaskID: "task-old-agent", MachineID: "machine-a", Status: "sent",
	})
	registry.Dispatch("machine-a", &proto.TaskAck{
		TaskID: "task-old-agent", Accepted: true,
		Reason: "already_done", Total: 10,
	})

	got := registry.List()[0]
	if got.Status != "done" || got.Total != 10 {
		t.Fatalf("old-agent completion = %#v, want terminal done", got)
	}
	if pending := registry.PendingScans("machine-a"); len(pending) != 0 {
		t.Fatalf("already_done task remained pending: %#v", pending)
	}
}

func TestTaskRegistryRejectsTaskIDEnvelopeConflict(t *testing.T) {
	registry := NewTaskRegistry(nil, testLogger())
	if err := registry.Register(&TaskInfo{
		TaskID: "task-conflict", MachineID: "machine-a", Phase: 1,
		Roots: []string{`D:\one`}, Status: "sent",
	}); err != nil {
		t.Fatal(err)
	}
	err := registry.Register(&TaskInfo{
		TaskID: "task-conflict", MachineID: "machine-a", Phase: 1,
		Roots: []string{`D:\two`}, Status: "sent",
	})
	if !errors.Is(err, ErrTaskEnvelopeConflict) {
		t.Fatalf("Register conflict error = %v, want ErrTaskEnvelopeConflict", err)
	}
	got := registry.List()[0]
	if len(got.Roots) != 1 || got.Roots[0] != `D:\one` {
		t.Fatalf("original task envelope was overwritten: %#v", got)
	}
}

func TestTaskRegistryDoesNotRegisterWhenInitialPersistenceFails(t *testing.T) {
	pool, err := pgxpool.New(
		context.Background(),
		"postgres://dedup:dedup@127.0.0.1:1/dedup?sslmode=disable",
	)
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	registry := NewTaskRegistry(pool, testLogger())

	err = registry.Register(&TaskInfo{
		TaskID: "task-not-durable", MachineID: "machine-a", Phase: 1,
		Roots: []string{`D:\media`}, Status: "sent",
	})
	if err == nil {
		t.Fatal("Register returned nil after PostgreSQL pool was closed")
	}
	if got := registry.List(); len(got) != 0 {
		t.Fatalf("non-durable task was registered in memory: %#v", got)
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
}
