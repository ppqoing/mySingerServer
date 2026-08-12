package localtask

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"dedup/internal/proto"
	"dedup/internal/store"
)

// Break caught: accepted work bound to a Socket context is cancelled when the
// NodeTray connection closes, despite the task already being durable.
func TestLocalTaskCreateIsIdempotentAndDisconnectDoesNotCancel(t *testing.T) {
	db := openServiceDB(t)
	runner := &recordingTaskRunner{started: make(chan runRecord, 4), release: make(chan struct{})}
	service := NewService("machine-a", db, runner)
	request := CreateRequest{TaskID: "task-1", Roots: []string{`D:\\media`}, Mode: proto.LocalTaskModeScanOnly}
	connection, disconnect := context.WithCancel(context.Background())
	task, err := service.Create(connection, request)
	if err != nil {
		t.Fatal(err)
	}
	disconnect()
	first := waitRun(t, runner.started)
	if first.request.TaskID != task.TaskID || first.stage != 0 {
		t.Fatalf("run = %#v, task = %#v", first, task)
	}
	if _, err := service.Create(context.Background(), request); err != nil {
		t.Fatalf("idempotent Create: %v", err)
	}
	conflict := request
	conflict.Roots = []string{`E:\\other`}
	if _, err := service.Create(context.Background(), conflict); !errors.Is(err, store.ErrLocalTaskConflict) {
		t.Fatalf("conflicting Create error = %v", err)
	}
	close(runner.release)
	waitTaskStatus(t, service, task.TaskID, "succeeded")
}

// Break caught: recovery restarts completed scan work from stage zero or
// guesses an old row's missing roots instead of failing closed.
func TestLocalTaskRecoveryContinuesFromPersistedStageAndRejectsLegacyEnvelope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	request := CreateRequest{TaskID: "recover", Roots: []string{`D:\\media`}, Mode: proto.LocalTaskModeScanThenAnalysis}
	envelope, digest, err := EncodeCreateEnvelope(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateOrLoadLocalTask(context.Background(), store.LocalTaskCreate{TaskID: request.TaskID, MachineID: "machine-a", Source: "local", Type: "analysis", Stage: 1, EnvelopeDigest: digest, Envelope: envelope}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.TransitionLocalTask(context.Background(), "machine-a", request.TaskID, store.LocalTaskUpdate{Status: "running", Stage: 1}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO local_tasks(task_id,machine_id,source,type,stage,status,envelope_digest,envelope,created_at,updated_at) VALUES ('legacy','machine-a','local','scan',0,'running','old',X'',1,1)`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	runner := &recordingTaskRunner{started: make(chan runRecord, 4), release: make(chan struct{})}
	service := NewService("machine-a", db, runner)
	if err := service.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovered := waitRun(t, runner.started)
	if recovered.request.TaskID != "recover" || recovered.stage != 1 {
		t.Fatalf("recovered run = %#v", recovered)
	}
	close(runner.release)
	waitTaskStatus(t, service, "recover", "succeeded")
	waitTaskStatus(t, service, "legacy", "failed")
}

type runRecord struct {
	request CreateRequest
	stage   int
}

type recordingTaskRunner struct {
	started chan runRecord
	release chan struct{}
	once    sync.Once
}

func (r *recordingTaskRunner) Run(ctx context.Context, request CreateRequest, stage int, advance func(int) error) error {
	r.started <- runRecord{request: request, stage: stage}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.release:
	}
	if request.Mode == proto.LocalTaskModeScanThenAnalysis && stage < 1 {
		return advance(1)
	}
	return nil
}

func openServiceDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func waitRun(t *testing.T, started <-chan runRecord) runRecord {
	t.Helper()
	select {
	case record := <-started:
		return record
	case <-time.After(time.Second):
		t.Fatal("task did not start")
		return runRecord{}
	}
}

func waitTaskStatus(t *testing.T, service Service, taskID, status string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		page, err := service.List(context.Background(), ListRequest{Limit: 200})
		if err != nil {
			t.Fatal(err)
		}
		for _, task := range page.Items {
			if task.TaskID == taskID && task.Status == status {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("task %s did not reach %s", taskID, status)
}
