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

func TestTaskRegistryCancelLifecycle(t *testing.T) {
	registry := NewTaskRegistry(nil, testLogger())
	if err := registry.Register(&TaskInfo{
		TaskID: "task-cancel", MachineID: "machine-a", Phase: 1,
		Roots: []string{`D:\media`}, Status: "running",
	}); err != nil {
		t.Fatal(err)
	}
	task, already, err := registry.BeginCancel("task-cancel")
	if err != nil || already || task.Status != "cancelling" ||
		task.MachineID != "machine-a" {
		t.Fatalf("BeginCancel = (%#v, %v, %v)", task, already, err)
	}
	// 重复取消幂等：已处于 cancelling 时无需重发消息。
	if _, already, err = registry.BeginCancel("task-cancel"); err != nil || !already {
		t.Fatalf("second BeginCancel = (_, %v, %v)", already, err)
	}
	// 取消展开期间的迟到进度只更新计数，不回退可见状态。
	registry.Dispatch("machine-a", &proto.TaskProgress{
		TaskID: "task-cancel", Done: 3, Total: 9,
	})
	if got := registry.List()[0]; got.Status != "cancelling" || got.Done != 3 {
		t.Fatalf("progress during cancel = %#v", got)
	}
	// 迟到的 accepted 应答同样不能回退 cancelling。
	registry.Dispatch("machine-a", &proto.TaskAck{
		TaskID: "task-cancel", Accepted: true, Reason: "accepted", Total: 9,
	})
	if got := registry.List()[0]; got.Status != "cancelling" {
		t.Fatalf("ack during cancel regressed = %#v", got)
	}
	// 终态回执收口为 failed + ack_reason=cancelled，统计照常落账。
	registry.Dispatch("machine-a", &proto.TaskDone{
		TaskID: "task-cancel", Reason: "cancelled",
		Stats: proto.TaskStats{Total: 9, Done: 3, Skipped: 6},
	})
	got := registry.List()[0]
	if got.Status != "failed" || got.AckReason != "cancelled" ||
		got.Done != 3 || got.Skipped != 6 {
		t.Fatalf("cancelled terminal = %#v", got)
	}
	if got := registry.PendingScans("machine-a"); len(got) != 0 {
		t.Fatalf("cancelled task stayed pending: %#v", got)
	}
}

func TestTaskRegistryCancelValidatesStateAndRollsBack(t *testing.T) {
	registry := NewTaskRegistry(nil, testLogger())
	if _, _, err := registry.BeginCancel("missing"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing BeginCancel err = %v", err)
	}
	if err := registry.Register(&TaskInfo{
		TaskID: "task-done", MachineID: "machine-a", Status: "done",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.BeginCancel("task-done"); !errors.Is(err, ErrTaskTerminal) {
		t.Fatalf("terminal BeginCancel err = %v", err)
	}
	if err := registry.Register(&TaskInfo{
		TaskID: "task-running", MachineID: "machine-a", Status: "running",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.BeginCancel("task-running"); err != nil {
		t.Fatal(err)
	}
	registry.RollbackCancel("task-running")
	for _, got := range registry.List() {
		if got.TaskID == "task-running" && got.Status != "running" {
			t.Fatalf("rollback status = %#v", got)
		}
	}
	// 回滚只针对 cancelling：已收口的终态不被恢复覆盖。
	if _, _, err := registry.BeginCancel("task-running"); err != nil {
		t.Fatal(err)
	}
	registry.Dispatch("machine-a", &proto.TaskDone{
		TaskID: "task-running", Reason: "cancelled",
		Stats: proto.TaskStats{Total: 2, Done: 1},
	})
	registry.RollbackCancel("task-running")
	for _, got := range registry.List() {
		if got.TaskID == "task-running" && got.Status != "failed" {
			t.Fatalf("terminal after cancel was rolled back: %#v", got)
		}
	}
}

// Manager 重启后任务从 PostgreSQL 恢复为 running（无内存 cancelling）；
// agent 已取消的回执凭 TaskDone.Reason 自描述收口。
func TestTaskRegistryCancelledReceiptAppliesWithoutInMemoryState(t *testing.T) {
	registry := NewTaskRegistry(nil, testLogger())
	if err := registry.Register(&TaskInfo{
		TaskID: "task-restored", MachineID: "machine-a", Status: "running",
	}); err != nil {
		t.Fatal(err)
	}
	registry.Dispatch("machine-a", &proto.TaskDone{
		TaskID: "task-restored", Reason: "cancelled",
		Stats: proto.TaskStats{Total: 2, Done: 1},
	})
	got := registry.List()[0]
	if got.Status != "failed" || got.AckReason != "cancelled" || got.Done != 1 {
		t.Fatalf("restored cancel = %#v", got)
	}
}

// 取消请求与自然完成竞态：cancelling 期间到达的无 Reason TaskDone 同样
// 按用户取消意图收口。
func TestTaskRegistryNaturalDoneDuringCancellingMapsToCancelled(t *testing.T) {
	registry := NewTaskRegistry(nil, testLogger())
	if err := registry.Register(&TaskInfo{
		TaskID: "task-race", MachineID: "machine-a", Status: "running",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.BeginCancel("task-race"); err != nil {
		t.Fatal(err)
	}
	registry.Dispatch("machine-a", &proto.TaskDone{
		TaskID: "task-race",
		Stats:  proto.TaskStats{Total: 4, Done: 4},
	})
	got := registry.List()[0]
	if got.Status != "failed" || got.AckReason != "cancelled" {
		t.Fatalf("natural done during cancelling = %#v", got)
	}
}

func TestTaskRegistrySameEnvelopeRetryResetsFailedTask(t *testing.T) {
	registry := NewTaskRegistry(nil, testLogger())
	task := &TaskInfo{
		TaskID: "task-retry", MachineID: "machine-a", Phase: 1,
		Roots: []string{`D:\one`}, Status: "sent",
	}
	if err := registry.Register(task); err != nil {
		t.Fatal(err)
	}
	registry.MarkSendFailed("task-retry", errors.New("connection refused"))
	if got := registry.List()[0]; got.Status != "failed" {
		t.Fatalf("status after send failure = %q", got.Status)
	}
	if err := registry.Register(cloneTask(task)); err != nil {
		t.Fatal(err)
	}
	got := registry.List()[0]
	if got.Status != "sent" || got.LastErr != "" {
		t.Fatalf("retry did not reset the failed task: %#v", got)
	}
	// Receipts must flow again after the reset instead of being swallowed by
	// the terminal status check.
	registry.Dispatch("machine-a", &proto.TaskAck{TaskID: "task-retry", Accepted: true, Total: 3})
	registry.Dispatch("machine-a", &proto.TaskProgress{TaskID: "task-retry", Done: 1, Total: 3})
	got = registry.List()[0]
	if got.Status != "running" || got.Done != 1 || got.Total != 3 {
		t.Fatalf("progress after retry was swallowed: %#v", got)
	}
}
