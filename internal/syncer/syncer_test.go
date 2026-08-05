package syncer

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"dedup/internal/proto"
	"dedup/internal/store"
)

type healthRecorder struct {
	mu      sync.Mutex
	updates []HealthUpdate
}

func (r *healthRecorder) observe(update HealthUpdate) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updates = append(r.updates, update)
}

func (r *healthRecorder) snapshot() []HealthUpdate {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]HealthUpdate(nil), r.updates...)
}

func TestSyncHealthRecoversAfterTerminalFailureThenSuccessfulCommit(t *testing.T) {
	ctx := context.Background()
	local, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	seedSyncedFiles(t, local, 1)

	recorder := &healthRecorder{}
	remote := &transactionalRemote{failStage: "commit"}
	uploader := NewWithRemote(local, remote, Config{
		UpsertBatch: 10,
		OnHealth:    recorder.observe,
	}, discardLogger())
	uploader.syncOnce(ctx)

	updates := recorder.snapshot()
	if len(updates) == 0 || updates[len(updates)-1].Healthy {
		t.Fatalf("updates after terminal failure = %#v, want final unhealthy", updates)
	}
	if updates[len(updates)-1].ErrorSummary == "" {
		t.Fatalf("terminal failure update = %#v, want bounded diagnostic", updates[len(updates)-1])
	}

	remote.failStage = ""
	uploader.syncOnce(ctx)
	updates = recorder.snapshot()
	if len(updates) == 0 || !updates[len(updates)-1].Healthy {
		t.Fatalf("updates after recovery = %#v, want final healthy", updates)
	}
	if updates[len(updates)-1].ErrorSummary != "" {
		t.Fatalf("healthy update retained error = %q", updates[len(updates)-1].ErrorSummary)
	}
}

func TestSyncHealthHealthyNoopReportsRecovery(t *testing.T) {
	local, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()

	recorder := &healthRecorder{}
	uploader := NewWithRemote(local, &transactionalRemote{}, Config{
		OnHealth: recorder.observe,
	}, discardLogger())
	uploader.syncOnce(context.Background())

	updates := recorder.snapshot()
	if len(updates) != 1 || !updates[0].Healthy || updates[0].ErrorSummary != "" {
		t.Fatalf("healthy no-op updates = %#v, want one clean healthy update", updates)
	}
}

func TestSyncHealthHandledMalformedRowEndsHealthyWithoutUnhealthyNotification(t *testing.T) {
	local := &scriptedLocal{pending: []store.SyncQueueRow{{
		TableName:  "image_features",
		RowPK:      strings.Repeat("A", 128),
		Generation: 1,
	}}}
	recorder := &healthRecorder{}
	uploader := &Syncer{
		local:  local,
		remote: &transactionalRemote{},
		cfg: Config{
			UpsertBatch: 10,
			OnHealth:    recorder.observe,
		},
		log: discardLogger(),
	}
	uploader.syncOnce(context.Background())

	if len(local.quarantined) != 1 {
		t.Fatalf("quarantined rows = %d, want 1", len(local.quarantined))
	}
	updates := recorder.snapshot()
	if len(updates) == 0 || !updates[len(updates)-1].Healthy {
		t.Fatalf("handled malformed row updates = %#v, want final healthy", updates)
	}
	for _, update := range updates {
		if !update.Healthy {
			t.Fatalf("handled malformed row emitted unhealthy update: %#v", updates)
		}
	}
}

func TestSyncOnceMarksRowsOnlyAfterRemoteSuccess(t *testing.T) {
	ctx := context.Background()
	local, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	seedSyncedFiles(t, local, 3)

	remote := &transactionalRemote{failStage: "commit"}
	syncer := NewWithRemote(local, remote, Config{
		Interval: time.Minute, TriggerRows: 50_000, UpsertBatch: 2,
	}, discardLogger())
	syncer.syncOnce(ctx)
	if count, err := local.PendingSyncCount(ctx); err != nil || count != 3 {
		t.Fatalf("pending after failure = %d, err=%v; want 3", count, err)
	}

	remote.failStage = ""
	syncer.syncOnce(ctx)
	if count, err := local.PendingSyncCount(ctx); err != nil || count != 0 {
		t.Fatalf("pending after success = %d, err=%v; want 0", count, err)
	}
	if len(remote.committed) != 2 || len(remote.committed[0].files) != 2 ||
		len(remote.committed[1].files) != 1 {
		t.Fatalf("remote batches = %#v, want [2,1]", remote.committed)
	}
}

func TestSyncOnceCanBeRepeatedWithoutResendingSyncedRows(t *testing.T) {
	ctx := context.Background()
	local, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	seedSyncedFiles(t, local, 1)
	remote := &transactionalRemote{}
	syncer := NewWithRemote(local, remote, Config{
		Interval: time.Minute, TriggerRows: 50_000, UpsertBatch: 10,
	}, discardLogger())
	syncer.syncOnce(ctx)
	syncer.syncOnce(ctx)
	if len(remote.committed) != 1 {
		t.Fatalf("remote calls = %d, want 1", len(remote.committed))
	}
}

func TestSyncOnceDoesNotClearUpdateEnqueuedDuringRemoteUpsert(t *testing.T) {
	ctx := context.Background()
	local, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	seedSyncedFiles(t, local, 1)

	remote := &transactionalRemote{failBeginAt: 2}
	remote.commitHook = func() {
		if err := local.ApplyHashResults(ctx, "machine-a", []store.HashResult{{
			Path: `D:\media\a.bin`, SHA512: "new-hash",
		}}); err != nil {
			t.Fatalf("advance generation: %v", err)
		}
	}
	uploader := NewWithRemote(local, remote, Config{
		Interval: time.Minute, TriggerRows: 50_000, UpsertBatch: 10,
	}, discardLogger())
	uploader.syncOnce(ctx)

	if remote.begins != 2 {
		t.Fatalf("remote begins = %d, want retry of the newer generation", remote.begins)
	}
	if count, err := local.PendingSyncCount(ctx); err != nil || count != 1 {
		t.Fatalf("pending newer generation = %d, err=%v; want 1", count, err)
	}
}

func seedSyncedFiles(t *testing.T, local *store.DB, count int) {
	t.Helper()
	ctx := context.Background()
	records := make([]store.EnumUpsert, count)
	results := make([]store.HashResult, count)
	for index := 0; index < count; index++ {
		path := `D:\media\` + string(rune('a'+index)) + ".bin"
		records[index] = store.EnumUpsert{
			MachineID: "machine-a", DiskNo: 1, Path: path,
			Size: int64(index + 1), MTime: 100,
			MissingBase: proto.FieldSHA512,
		}
		results[index] = store.HashResult{Path: path, SHA512: "hash"}
	}
	if err := local.UpsertEnumerated(ctx, records); err != nil {
		t.Fatal(err)
	}
	if err := local.ApplyHashResults(ctx, "machine-a", results); err != nil {
		t.Fatal(err)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}
