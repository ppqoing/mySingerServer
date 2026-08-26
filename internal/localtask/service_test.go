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

func TestLocalTaskCreateUsesPersistedEnvelopeCopy(t *testing.T) {
	db := openServiceDB(t)
	runner := &recordingTaskRunner{started: make(chan runRecord, 1), release: make(chan struct{})}
	service := NewService("m", db, runner)
	request := CreateRequest{TaskID: "alias", Roots: []string{`D:\original`}, Extensions: []string{".jpg"}, Mode: proto.LocalTaskModeScanOnly}
	task, err := service.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.Roots[0] = `Z:\mutated`
	request.Extensions[0] = ".exe"
	run := waitRun(t, runner.started)
	if task.Roots[0] != `D:\original` || task.Extensions[0] != ".jpg" || run.request.Roots[0] != `D:\original` || run.request.Extensions[0] != ".jpg" {
		t.Fatalf("response=%#v runner=%#v", task, run.request)
	}
	close(runner.release)
}

func TestLocalTaskCancelThenImmediateRetryWaitsOldAttemptAndStartsOnce(t *testing.T) {
	db := openServiceDB(t)
	runner := &attemptRunner{starts: make(chan int, 4), release: make(chan struct{})}
	service := NewService("m", db, runner)
	_, err := service.Create(context.Background(), CreateRequest{TaskID: "retry", Roots: []string{`D:\media`}, Mode: proto.LocalTaskModeScanOnly})
	if err != nil {
		t.Fatal(err)
	}
	if got := <-runner.starts; got != 1 {
		t.Fatal(got)
	}
	cancelDone := make(chan error, 1)
	go func() { cancelDone <- service.Cancel(context.Background(), "retry") }()
	select {
	case err := <-cancelDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Cancel did not wait old attempt exit")
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := service.Retry(context.Background(), "retry"); results <- err }()
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("successful retries=%d want1", success)
	}
	if got := <-runner.starts; got != 2 {
		t.Fatalf("attempt=%d want2", got)
	}
	close(runner.release)
}

func TestLocalTaskRetrySupersedesFailedAttemptInActiveDeferWindow(t *testing.T) {
	db := openServiceDB(t)
	blocking := &blockingFailedStore{
		TaskStore:     db,
		failedWritten: make(chan struct{}),
		releaseFailed: make(chan struct{}),
		failedResult:  make(chan error, 1),
	}
	runner := &failFirstAttemptRunner{starts: make(chan int, 4), release: make(chan struct{})}
	service := NewService("m", blocking, runner)
	_, err := service.Create(context.Background(), CreateRequest{TaskID: "failed-retry", Roots: []string{`D:\media`}, Mode: proto.LocalTaskModeScanOnly})
	if err != nil {
		t.Fatal(err)
	}
	if got := <-runner.starts; got != 1 {
		t.Fatalf("attempt=%d want1", got)
	}
	select {
	case <-blocking.failedWritten:
	case err := <-blocking.failedResult:
		t.Fatalf("failed terminal transition: %v", err)
	case <-time.After(time.Second):
		t.Fatal("failed terminal was not persisted")
	}

	results := make(chan error, 1)
	go func() { _, err := service.Retry(context.Background(), "failed-retry"); results <- err }()
	select {
	case err := <-results:
		t.Fatalf("Retry returned before old terminal completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(blocking.releaseFailed)
	if err := <-results; err != nil {
		t.Fatal(err)
	}
	if got := <-runner.starts; got != 2 {
		t.Fatalf("attempt=%d want2", got)
	}
	close(runner.release)
}

// Break caught: per-task serialization used sync.Mutex.Lock, so a Retry whose
// context expired could remain blocked behind terminal commit indefinitely.
func TestLocalTaskRetryGateWaitHonorsContextWithoutChangingFailedState(t *testing.T) {
	db := openServiceDB(t)
	blocking := &blockingFailedStore{TaskStore: db, failedWritten: make(chan struct{}), releaseFailed: make(chan struct{}), failedResult: make(chan error, 1)}
	runner := &failFirstAttemptRunner{starts: make(chan int, 2), release: make(chan struct{})}
	service := NewService("m", blocking, runner)
	_, err := service.Create(context.Background(), CreateRequest{TaskID: "gate-timeout", Roots: []string{`D:\media`}, Mode: proto.LocalTaskModeScanOnly})
	if err != nil {
		t.Fatal(err)
	}
	<-runner.starts
	select {
	case <-blocking.failedWritten:
	case err := <-blocking.failedResult:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("terminal commit did not reach barrier")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := service.Retry(ctx, "gate-timeout"); done <- err }()
	var retryErr error
	select {
	case retryErr = <-done:
	case <-time.After(100 * time.Millisecond):
		close(blocking.releaseFailed)
		t.Fatal("Retry ignored context while waiting per-task gate")
	}
	if !errors.Is(retryErr, context.DeadlineExceeded) {
		t.Fatalf("Retry=%v, want deadline", retryErr)
	}
	close(blocking.releaseFailed)
	waitTaskStatus(t, service, "gate-timeout", "failed")
	select {
	case <-runner.starts:
		t.Fatal("gate timeout launched a replacement attempt")
	default:
	}
}

func TestLocalTaskCancelGateWaitHonorsContextWithoutCancellingCompletedRunner(t *testing.T) {
	db := openServiceDB(t)
	blocking := &blockingFailedStore{TaskStore: db, failedWritten: make(chan struct{}), releaseFailed: make(chan struct{}), failedResult: make(chan error, 1)}
	runner := &failFirstAttemptRunner{starts: make(chan int, 2), release: make(chan struct{})}
	service := NewService("m", blocking, runner)
	_, err := service.Create(context.Background(), CreateRequest{TaskID: "cancel-gate-timeout", Roots: []string{`D:\media`}, Mode: proto.LocalTaskModeScanOnly})
	if err != nil {
		t.Fatal(err)
	}
	<-runner.starts
	select {
	case <-blocking.failedWritten:
	case err := <-blocking.failedResult:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("terminal commit did not reach barrier")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := service.Cancel(ctx, "cancel-gate-timeout"); !errors.Is(err, context.DeadlineExceeded) {
		close(blocking.releaseFailed)
		t.Fatalf("Cancel=%v, want deadline", err)
	}
	close(blocking.releaseFailed)
	waitTaskStatus(t, service, "cancel-gate-timeout", "failed")
}

func TestLocalTaskRetryContextExpiryBeforeTransitionLeavesCancelledAttempt(t *testing.T) {
	db := openServiceDB(t)
	runner := &stubbornAttemptRunner{starts: make(chan int, 2), releaseFirst: make(chan struct{}), releaseSecond: make(chan struct{})}
	service := NewService("m", db, runner)
	_, err := service.Create(context.Background(), CreateRequest{TaskID: "retry-timeout", Roots: []string{`D:\media`}, Mode: proto.LocalTaskModeScanOnly})
	if err != nil {
		t.Fatal(err)
	}
	if got := <-runner.starts; got != 1 {
		t.Fatalf("attempt=%d want1", got)
	}
	cancelCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := service.Cancel(cancelCtx, "retry-timeout"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Cancel=%v, want deadline", err)
	}
	retryCtx, stopRetry := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer stopRetry()
	if _, err := service.Retry(retryCtx, "retry-timeout"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Retry=%v, want deadline", err)
	}
	close(runner.releaseFirst)
	select {
	case got := <-runner.starts:
		t.Fatalf("context-expired Retry launched attempt %d", got)
	case <-time.After(50 * time.Millisecond):
	}
	waitTaskStatus(t, service, "retry-timeout", "cancelled")
}

func TestLocalTaskRetryRejectsInvalidPersistedEnvelopeBeforePendingTransition(t *testing.T) {
	db := openServiceDB(t)
	request := CreateRequest{TaskID: "invalid-retry", Roots: []string{`D:\media`}, Mode: proto.LocalTaskModeScanOnly}
	envelope, digest, err := EncodeCreateEnvelope(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateOrLoadLocalTask(context.Background(), store.LocalTaskCreate{TaskID: request.TaskID, MachineID: "m", Source: "local", Type: "scan", EnvelopeDigest: digest, Envelope: envelope}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.TransitionLocalTask(context.Background(), "m", request.TaskID, store.LocalTaskUpdate{Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.TransitionLocalTask(context.Background(), "m", request.TaskID, store.LocalTaskUpdate{Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	service := NewService("m", &malformedLoadStore{TaskStore: db}, &recordingTaskRunner{started: make(chan runRecord, 1), release: make(chan struct{})})
	if _, err := service.Retry(context.Background(), request.TaskID); err == nil {
		t.Fatal("Retry accepted malformed persisted envelope")
	}
	loaded, err := db.LoadLocalTask(context.Background(), "m", request.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != "failed" {
		t.Fatalf("invalid envelope status=%q, want failed", loaded.Status)
	}
}

type malformedLoadStore struct{ TaskStore }

func (s *malformedLoadStore) LoadLocalTask(ctx context.Context, machineID, taskID string) (store.LocalTask, error) {
	task, err := s.TaskStore.LoadLocalTask(ctx, machineID, taskID)
	if err == nil {
		task.Envelope = []byte{0x80}
	}
	return task, err
}

type stubbornAttemptRunner struct {
	mu            sync.Mutex
	attempt       int
	starts        chan int
	releaseFirst  chan struct{}
	releaseSecond chan struct{}
}

func (r *stubbornAttemptRunner) Run(_ context.Context, _ CreateRequest, _ int, _ func(int) error) error {
	r.mu.Lock()
	r.attempt++
	n := r.attempt
	r.mu.Unlock()
	r.starts <- n
	if n == 1 {
		<-r.releaseFirst
		return context.Canceled
	}
	<-r.releaseSecond
	return nil
}

type blockingFailedStore struct {
	TaskStore
	failedWritten chan struct{}
	releaseFailed chan struct{}
	failedResult  chan error
	once          sync.Once
}

func (s *blockingFailedStore) TransitionLocalTask(ctx context.Context, machineID, taskID string, update store.LocalTaskUpdate) (store.LocalTask, error) {
	task, err := s.TaskStore.TransitionLocalTask(ctx, machineID, taskID, update)
	if update.Status == "failed" {
		if err != nil {
			s.failedResult <- err
		} else {
			s.once.Do(func() { close(s.failedWritten) })
			<-s.releaseFailed
		}
	}
	return task, err
}

type failFirstAttemptRunner struct {
	mu      sync.Mutex
	attempt int
	starts  chan int
	release chan struct{}
}

func (r *failFirstAttemptRunner) Run(_ context.Context, _ CreateRequest, _ int, _ func(int) error) error {
	r.mu.Lock()
	r.attempt++
	n := r.attempt
	r.mu.Unlock()
	r.starts <- n
	if n == 1 {
		return errors.New("failed")
	}
	<-r.release
	return nil
}

type attemptRunner struct {
	mu      sync.Mutex
	attempt int
	starts  chan int
	release chan struct{}
}

func (r *attemptRunner) Run(ctx context.Context, _ CreateRequest, _ int, _ func(int) error) error {
	r.mu.Lock()
	r.attempt++
	n := r.attempt
	r.mu.Unlock()
	r.starts <- n
	if n == 1 {
		<-ctx.Done()
		return ctx.Err()
	}
	<-r.release
	return nil
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
