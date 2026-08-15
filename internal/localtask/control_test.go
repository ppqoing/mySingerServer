package localtask

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"dedup/internal/proto"
	"dedup/internal/store"
)

type controlledRun struct {
	Control  RunControl
	Task     Task
	Report   func(ProgressUpdate) error
	Release  chan error
	Returned chan struct{}
}

type blockingControlRunner struct {
	Runs chan *controlledRun
}

func newBlockingControlRunner() *blockingControlRunner {
	return &blockingControlRunner{Runs: make(chan *controlledRun, 16)}
}

func (r *blockingControlRunner) Run(control RunControl, _ CreateRequest, task Task, report func(ProgressUpdate) error) error {
	run := &controlledRun{
		Control:  control,
		Task:     task,
		Report:   report,
		Release:  make(chan error, 1),
		Returned: make(chan struct{}),
	}
	r.Runs <- run
	err := <-run.Release
	close(run.Returned)
	return err
}

type watchedTaskStore struct {
	TaskStore
	transitions   chan store.LocalTask
	progress      chan store.LocalTask
	deleteEntered chan store.LocalTaskControl
	deleteDone    chan error
	deleteRelease <-chan struct{}

	mu             sync.Mutex
	deleteFailures int
	deleteCalls    int
}

func watchTaskStore(taskStore TaskStore) *watchedTaskStore {
	return &watchedTaskStore{
		TaskStore:     taskStore,
		transitions:   make(chan store.LocalTask, 64),
		progress:      make(chan store.LocalTask, 64),
		deleteEntered: make(chan store.LocalTaskControl, 64),
		deleteDone:    make(chan error, 64),
	}
}

func (s *watchedTaskStore) TransitionLocalTaskLifecycle(ctx context.Context, machineID string, control store.LocalTaskControl, status string, code, message *string) (store.LocalTask, error) {
	task, err := s.TaskStore.TransitionLocalTaskLifecycle(ctx, machineID, control, status, code, message)
	if err == nil {
		s.transitions <- task
	}
	return task, err
}

func (s *watchedTaskStore) UpdateLocalTaskProgress(ctx context.Context, machineID string, control store.LocalTaskControl, update store.LocalTaskProgressUpdate) (store.LocalTask, error) {
	task, err := s.TaskStore.UpdateLocalTaskProgress(ctx, machineID, control, update)
	if err == nil {
		s.progress <- task
	}
	return task, err
}

func (s *watchedTaskStore) DeleteLocalTaskData(ctx context.Context, machineID string, control store.LocalTaskControl) (store.LocalTaskDeleteResult, error) {
	s.mu.Lock()
	s.deleteCalls++
	if s.deleteFailures > 0 {
		s.deleteFailures--
		s.mu.Unlock()
		s.deleteEntered <- control
		err := store.ErrLocalTaskDeleteRetryable
		s.deleteDone <- err
		return store.LocalTaskDeleteResult{}, err
	}
	s.mu.Unlock()
	s.deleteEntered <- control
	if s.deleteRelease != nil {
		select {
		case <-s.deleteRelease:
		case <-ctx.Done():
			s.deleteDone <- ctx.Err()
			return store.LocalTaskDeleteResult{}, ctx.Err()
		}
	}
	result, err := s.TaskStore.DeleteLocalTaskData(ctx, machineID, control)
	s.deleteDone <- err
	return result, err
}

func (s *watchedTaskStore) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteCalls
}

type controlFixture struct {
	t       *testing.T
	DB      *store.DB
	Store   *watchedTaskStore
	Runner  *blockingControlRunner
	Service RecoverableService
}

func newControlFixture(t *testing.T, options ...ServiceOption) *controlFixture {
	t.Helper()
	db := openServiceDB(t)
	observed := watchTaskStore(db)
	runner := newBlockingControlRunner()
	return &controlFixture{
		t: t, DB: db, Store: observed, Runner: runner,
		Service: NewService("machine-a", observed, runner, options...),
	}
}

func (f *controlFixture) createAndWaitStarted(taskID string) *controlledRun {
	f.t.Helper()
	if _, err := f.Service.Create(context.Background(), CreateRequest{
		TaskID: taskID,
		Roots:  []string{`D:\media`},
		Mode:   proto.LocalTaskModeScanOnly,
	}); err != nil {
		f.t.Fatal(err)
	}
	return receiveRun(f.t, f.Runner.Runs)
}

func receiveRun(t *testing.T, runs <-chan *controlledRun) *controlledRun {
	t.Helper()
	select {
	case run := <-runs:
		return run
	case <-time.After(time.Second):
		t.Fatal("runner did not start")
		return nil
	}
}

func receiveTransition(t *testing.T, transitions <-chan store.LocalTask, status string) store.LocalTask {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case task := <-transitions:
			if task.Status == status {
				return task
			}
		case <-deadline:
			t.Fatalf("did not observe %q transition", status)
			return store.LocalTask{}
		}
	}
}

func requireOpen(t *testing.T, channel <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-channel:
		t.Fatalf("%s closed early", label)
	default:
	}
}

func requireNoRun(t *testing.T, runs <-chan *controlledRun) {
	t.Helper()
	select {
	case run := <-runs:
		t.Fatalf("unexpected runner attempt: %#v", run.Task)
	default:
	}
}

// Break caught: pause used context cancellation, blocked its response on the
// runner, or lost the last durable progress snapshot while draining.
func TestLocalTaskPauseAcceptsImmediatelyThenPersistsPausedAfterDrain(t *testing.T) {
	fixture := newControlFixture(t)
	run := fixture.createAndWaitStarted("pause")
	if err := run.Report(ProgressUpdate{
		Phase: "scan", Stage: 0, ProgressComplete: 3,
		ProgressTotal: 10, ProgressTotalKnown: true, StatsJSON: `{"seen":3}`,
	}); err != nil {
		t.Fatal(err)
	}
	accepted, err := fixture.Service.Pause(context.Background(), controlRequest(run.Task))
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Status != "pausing" || accepted.Revision != run.Task.Revision+1 {
		t.Fatalf("accepted=(%q,%d), running revision=%d", accepted.Status, accepted.Revision, run.Task.Revision)
	}
	select {
	case <-run.Control.Drain:
	default:
		t.Fatal("pause did not close drain signal")
	}
	requireOpen(t, run.Control.Context.Done(), "hard-cancel context")
	requireOpen(t, run.Returned, "runner return")
	requireNoRun(t, fixture.Runner.Runs)

	run.Release <- ErrDrainRequested
	paused := receiveTransition(t, fixture.Store.transitions, "paused")
	if paused.ProgressComplete != 3 || paused.ProgressTotal != 10 || !paused.ProgressTotalKnown || paused.StatsJSON != `{"seen":3}` {
		t.Fatalf("paused progress=%#v", paused)
	}
}

// Break caught: resume created a replacement instance or launched duplicate
// attempts instead of a single new revision of the paused instance.
func TestLocalTaskResumeUsesSameInstanceAndStartsExactlyOneNewAttempt(t *testing.T) {
	fixture := newControlFixture(t)
	first := fixture.createAndWaitStarted("resume")
	pausedAccepted, err := fixture.Service.Pause(context.Background(), controlRequest(first.Task))
	if err != nil {
		t.Fatal(err)
	}
	first.Release <- ErrDrainRequested
	paused := receiveTransition(t, fixture.Store.transitions, "paused")
	if paused.Revision <= pausedAccepted.Revision {
		t.Fatalf("paused revision=%d accepted=%d", paused.Revision, pausedAccepted.Revision)
	}

	pending, err := fixture.Service.ResumeTask(context.Background(), controlRequestStore(paused))
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != "pending" || pending.InstanceID != first.Task.InstanceID {
		t.Fatalf("resume snapshot=%#v first=%#v", pending, first.Task)
	}
	second := receiveRun(t, fixture.Runner.Runs)
	if second.Task.InstanceID != first.Task.InstanceID || second.Task.Revision != pending.Revision+1 {
		t.Fatalf("second=%#v pending=%#v", second.Task, pending)
	}
	requireNoRun(t, fixture.Runner.Runs)
	second.Release <- nil
	receiveTransition(t, fixture.Store.transitions, "succeeded")
}

// Break caught: stop returned only after cancellation or hard-cancelled worker
// work instead of draining it to the durable cancelled state.
func TestLocalTaskStopAcceptsThenCancelsAfterExactAttemptReturns(t *testing.T) {
	fixture := newControlFixture(t)
	run := fixture.createAndWaitStarted("stop")
	accepted, err := fixture.Service.Cancel(context.Background(), controlRequest(run.Task))
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Status != "stopping" {
		t.Fatalf("status=%q", accepted.Status)
	}
	requireOpen(t, run.Control.Context.Done(), "hard-cancel context")
	requireOpen(t, run.Returned, "runner return")
	run.Release <- ErrDrainRequested
	receiveTransition(t, fixture.Store.transitions, "cancelled")
}

// Break caught: delete waited on ctx.Done (which must remain open) rather than
// the exact attempt return, or deleted task-owned data more than once.
func TestLocalTaskDeleteWaitsExactAttemptDoneAndDeletesOnce(t *testing.T) {
	releaseDelete := make(chan struct{})
	fixture := newControlFixture(t)
	fixture.Store.deleteRelease = releaseDelete
	run := fixture.createAndWaitStarted("delete-running")
	attempt := fixture.Service.(*taskService).currentAttempt(run.Task.TaskID)
	if attempt == nil {
		t.Fatal("missing active attempt")
	}
	request := controlRequest(run.Task)
	accepted, err := fixture.Service.Delete(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Task == nil || accepted.Task.Status != "deleting" {
		t.Fatalf("accepted=%#v", accepted)
	}
	requireOpen(t, run.Control.Context.Done(), "hard-cancel context")
	select {
	case <-fixture.Store.deleteEntered:
		t.Fatal("delete began before exact runner returned")
	default:
	}

	run.Release <- ErrDrainRequested
	select {
	case <-run.Returned:
	case <-time.After(time.Second):
		t.Fatal("runner did not return")
	}
	select {
	case <-fixture.Store.deleteEntered:
	case <-time.After(time.Second):
		t.Fatal("delete did not begin after runner returned")
	}
	select {
	case <-attempt.done:
	default:
		t.Fatal("delete began before exact attempt done closed")
	}
	close(releaseDelete)
	select {
	case err := <-fixture.Store.deleteDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("delete did not finish")
	}

	repeated, err := fixture.Service.Delete(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !repeated.Deleted || repeated.Task != nil {
		t.Fatalf("repeat delete=%#v", repeated)
	}
	if fixture.Store.callCount() != 1 {
		t.Fatalf("DeleteLocalTaskData calls=%d", fixture.Store.callCount())
	}
}

// Break caught: deleting a stable task fabricated another Runner attempt.
func TestLocalTaskDeleteStableTaskSkipsRunnerDrain(t *testing.T) {
	fixture := newControlFixture(t)
	run := fixture.createAndWaitStarted("delete-stable")
	run.Release <- nil
	succeeded := receiveTransition(t, fixture.Store.transitions, "succeeded")
	result, err := fixture.Service.Delete(context.Background(), controlRequestStore(succeeded))
	if err != nil {
		t.Fatal(err)
	}
	if result.Task == nil || result.Task.Status != "deleting" {
		t.Fatalf("delete=%#v", result)
	}
	requireNoRun(t, fixture.Runner.Runs)
	select {
	case err := <-fixture.Store.deleteDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("stable delete did not finish")
	}
}

// Break caught: a weaker intent overwrote a stronger intent, or repeated
// controls incremented the revision more than once.
func TestLocalTaskControlPriorityAndIdempotentSnapshots(t *testing.T) {
	fixture := newControlFixture(t)
	run := fixture.createAndWaitStarted("priority")
	pausing, err := fixture.Service.Pause(context.Background(), controlRequest(run.Task))
	if err != nil {
		t.Fatal(err)
	}
	repeatedPause, err := fixture.Service.Pause(context.Background(), controlRequest(pausing))
	if err != nil || repeatedPause.Revision != pausing.Revision || repeatedPause.Status != "pausing" {
		t.Fatalf("repeat pause=(%#v,%v), first=%#v", repeatedPause, err, pausing)
	}
	stopping, err := fixture.Service.Cancel(context.Background(), controlRequest(pausing))
	if err != nil {
		t.Fatal(err)
	}
	if stopping.Status != "stopping" || stopping.Revision != pausing.Revision+1 {
		t.Fatalf("stopping=%#v pausing=%#v", stopping, pausing)
	}
	repeatedStop, err := fixture.Service.Cancel(context.Background(), controlRequest(stopping))
	if err != nil || repeatedStop.Revision != stopping.Revision || repeatedStop.Status != "stopping" {
		t.Fatalf("repeat stop=(%#v,%v), first=%#v", repeatedStop, err, stopping)
	}
	deleting, err := fixture.Service.Delete(context.Background(), controlRequest(stopping))
	if err != nil {
		t.Fatal(err)
	}
	if deleting.Task == nil || deleting.Task.Status != "deleting" || deleting.Task.Revision != stopping.Revision+1 {
		t.Fatalf("deleting=%#v stopping=%#v", deleting, stopping)
	}
	repeatedDelete, err := fixture.Service.Delete(context.Background(), controlRequest(*deleting.Task))
	if err != nil || repeatedDelete.Task == nil || repeatedDelete.Task.Revision != deleting.Task.Revision {
		t.Fatalf("repeat delete=(%#v,%v), first=%#v", repeatedDelete, err, deleting)
	}
	if got := run.Control.Reason(); got != DrainDelete {
		t.Fatalf("drain reason=%q, want delete", got)
	}
	run.Release <- ErrDrainRequested
	select {
	case err := <-fixture.Store.deleteDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("upgraded delete did not finish")
	}
}

// Break caught: two same-revision controls both persisted, or a stale control
// notified the current in-memory attempt before durable validation.
func TestLocalTaskConcurrentPauseHasOneWinnerAndStaleControlDoesNotDrain(t *testing.T) {
	fixture := newControlFixture(t)
	run := fixture.createAndWaitStarted("pause-race")
	stale := controlRequest(run.Task)
	stale.ExpectedRevision--
	if _, err := fixture.Service.Pause(context.Background(), stale); !errors.Is(err, store.ErrLocalTaskStale) {
		t.Fatalf("stale pause=%v", err)
	}
	select {
	case <-run.Control.Drain:
		t.Fatal("stale control notified current attempt")
	default:
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	request := controlRequest(run.Task)
	for range 2 {
		go func() {
			<-start
			_, err := fixture.Service.Pause(context.Background(), request)
			results <- err
		}()
	}
	close(start)
	wins, staleResults := 0, 0
	for range 2 {
		if err := <-results; err == nil {
			wins++
		} else if errors.Is(err, store.ErrLocalTaskStale) {
			staleResults++
		} else {
			t.Fatalf("pause error=%v", err)
		}
	}
	if wins != 1 || staleResults != 1 {
		t.Fatalf("wins=%d stale=%d", wins, staleResults)
	}
	run.Release <- ErrDrainRequested
	receiveTransition(t, fixture.Store.transitions, "paused")
}

// Break caught: process shutdown persisted cancelled, or user drain and hard
// cancellation shared the same signal.
func TestLocalTaskShutdownDrainsToWaitingRecoveryWithoutHardCancel(t *testing.T) {
	fixture := newControlFixture(t)
	run := fixture.createAndWaitStarted("shutdown")
	done := make(chan error, 1)
	go func() { done <- fixture.Service.Shutdown(context.Background()) }()
	select {
	case <-run.Control.Drain:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not request drain")
	}
	if got := run.Control.Reason(); got != DrainProcessShutdown {
		t.Fatalf("reason=%q", got)
	}
	requireOpen(t, run.Control.Context.Done(), "hard-cancel context")
	requireOpen(t, run.Returned, "runner return")
	run.Release <- ErrDrainRequested
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	receiveTransition(t, fixture.Store.transitions, "waiting_recovery")
}

// Break caught: shutdown hard-cancelled before its bounded context expired.
func TestLocalTaskShutdownHardCancelsOnlyAfterTimeout(t *testing.T) {
	fixture := newControlFixture(t)
	run := fixture.createAndWaitStarted("shutdown-timeout")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- fixture.Service.Shutdown(ctx) }()
	select {
	case <-run.Control.Drain:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not request drain")
	}
	requireOpen(t, run.Control.Context.Done(), "hard-cancel context")
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Shutdown=%v", err)
	}
	select {
	case <-run.Control.Context.Done():
	case <-time.After(time.Second):
		t.Fatal("shutdown timeout did not hard-cancel")
	}
	run.Release <- run.Control.Context.Err()
}

// Break caught: a reporter captured by an old attempt could write through the
// current revision after retry and overwrite the replacement attempt.
func TestLocalTaskOldReporterIsStaleAfterRetry(t *testing.T) {
	fixture := newControlFixture(t)
	first := fixture.createAndWaitStarted("stale-reporter")
	if err := first.Report(ProgressUpdate{Phase: "scan", ProgressComplete: 1, ProgressTotal: 10, ProgressTotalKnown: true, StatsJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	first.Release <- errors.New("first attempt failed")
	failed := receiveTransition(t, fixture.Store.transitions, "failed")
	pending, err := fixture.Service.Retry(context.Background(), controlRequestStore(failed))
	if err != nil {
		t.Fatal(err)
	}
	second := receiveRun(t, fixture.Runner.Runs)
	if second.Task.InstanceID != first.Task.InstanceID || second.Task.Revision != pending.Revision+1 {
		t.Fatalf("second revision=%d pending=%d", second.Task.Revision, pending.Revision)
	}
	if err := first.Report(ProgressUpdate{Phase: "scan", ProgressComplete: 9, ProgressTotal: 10, ProgressTotalKnown: true, StatsJSON: `{"old":true}`}); !errors.Is(err, store.ErrLocalTaskStale) {
		t.Fatalf("old reporter error=%v", err)
	}
	if err := second.Report(ProgressUpdate{Phase: "scan", ProgressComplete: 2, ProgressTotal: 10, ProgressTotalKnown: true, StatsJSON: `{"new":true}`}); err != nil {
		t.Fatal(err)
	}
	current, err := fixture.DB.LoadLocalTask(context.Background(), "machine-a", second.Task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if current.ProgressComplete != 2 || current.StatsJSON != `{"new":true}` {
		t.Fatalf("current progress=%#v", current)
	}
	second.Release <- nil
	receiveTransition(t, fixture.Store.transitions, "succeeded")
}

// Break caught: delayed cleanup from an old attempt used an unconditional map
// delete and removed the replacement attempt for the same task ID.
func TestLocalTaskOldAttemptCleanupKeepsReplacementActiveEntry(t *testing.T) {
	oldTask := store.LocalTask{TaskID: "replacement", InstanceID: "instance", Revision: 2}
	newTask := oldTask
	newTask.Revision = 3
	oldAttempt, _ := newTaskAttempt(oldTask)
	replacement, _ := newTaskAttempt(newTask)
	service := &taskService{active: map[string]*taskAttempt{oldTask.TaskID: replacement}}
	service.cleanupAttemptHeld(oldAttempt)
	if got := service.currentAttempt(oldTask.TaskID); got != replacement {
		t.Fatalf("active replacement=%p, want %p", got, replacement)
	}
}

// Break caught: retryable deletion either slept in tests, used the wrong
// schedule, or performed fewer/more than six automatic retries.
func TestLocalTaskDeleteRetriesSixTimesThenSucceeds(t *testing.T) {
	durations := make(chan time.Duration, 8)
	fixture := newControlFixture(t, WithDeleteRetryAfter(func(duration time.Duration) <-chan time.Time {
		durations <- duration
		ready := make(chan time.Time, 1)
		ready <- time.Time{}
		return ready
	}))
	fixture.Store.mu.Lock()
	fixture.Store.deleteFailures = 6
	fixture.Store.mu.Unlock()
	run := fixture.createAndWaitStarted("delete-retry")
	run.Release <- nil
	succeeded := receiveTransition(t, fixture.Store.transitions, "succeeded")
	if _, err := fixture.Service.Delete(context.Background(), controlRequestStore(succeeded)); err != nil {
		t.Fatal(err)
	}
	for range 7 {
		select {
		case <-fixture.Store.deleteDone:
		case <-time.After(time.Second):
			t.Fatal("delete retry did not run")
		}
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second}
	for index, expected := range want {
		select {
		case got := <-durations:
			if got != expected {
				t.Fatalf("delay[%d]=%s want %s", index, got, expected)
			}
		default:
			t.Fatalf("missing delay[%d]", index)
		}
	}
	if fixture.Store.callCount() != 7 {
		t.Fatalf("delete calls=%d", fixture.Store.callCount())
	}
	awaitTaskAbsent(t, fixture.Service, "delete-retry")
}

// Break caught: retry exhaustion left a task forever deleting or exposed a
// raw storage error instead of the stable delete_retry_exhausted code.
func TestLocalTaskDeleteRetryExhaustionPersistsDeleteFailed(t *testing.T) {
	fixture := newControlFixture(t, WithDeleteRetryAfter(func(time.Duration) <-chan time.Time {
		ready := make(chan time.Time, 1)
		ready <- time.Time{}
		return ready
	}))
	fixture.Store.mu.Lock()
	fixture.Store.deleteFailures = 7
	fixture.Store.mu.Unlock()
	run := fixture.createAndWaitStarted("delete-exhausted")
	run.Release <- nil
	succeeded := receiveTransition(t, fixture.Store.transitions, "succeeded")
	if _, err := fixture.Service.Delete(context.Background(), controlRequestStore(succeeded)); err != nil {
		t.Fatal(err)
	}
	failed := receiveTransition(t, fixture.Store.transitions, "delete_failed")
	if failed.SafeErrorCode == nil || *failed.SafeErrorCode != "delete_retry_exhausted" {
		t.Fatalf("safe error=%v", failed.SafeErrorCode)
	}
	if fixture.Store.callCount() != 7 {
		t.Fatalf("delete calls=%d", fixture.Store.callCount())
	}
}

// Break caught: startup relaunched paused work or abandoned a durable
// deleting task instead of reconciling it without invoking Runner.
func TestLocalTaskRecoveryKeepsPausedAndReconcilesDeleting(t *testing.T) {
	db := openServiceDB(t)
	paused := seedServiceTask(t, db, "machine-a", "recover-paused")
	var err error
	paused, err = db.TransitionLocalTaskLifecycle(context.Background(), "machine-a", controlForStoreTask(paused), "running", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	paused, err = db.TransitionLocalTaskLifecycle(context.Background(), "machine-a", controlForStoreTask(paused), "pausing", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	deleting := seedServiceTask(t, db, "machine-a", "recover-deleting")
	deleting, err = db.TransitionLocalTaskLifecycle(context.Background(), "machine-a", controlForStoreTask(deleting), "deleting", nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	observed := watchTaskStore(db)
	runner := newBlockingControlRunner()
	service := NewService("machine-a", observed, runner)
	if err := service.PrepareRecovery(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.ResumeRecoveredTasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	currentPaused, err := db.LoadLocalTask(context.Background(), "machine-a", paused.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if currentPaused.Status != "paused" {
		t.Fatalf("paused recovery status=%q", currentPaused.Status)
	}
	requireNoRun(t, runner.Runs)
	select {
	case err := <-observed.deleteDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("deleting recovery was not reconciled")
	}
	awaitTaskAbsent(t, service, deleting.TaskID)
}

// Break caught: controlling durable pending work with no active attempt
// fabricated a Runner invocation instead of converging in the background.
func TestLocalTaskPauseIdlePendingDoesNotFabricateRunner(t *testing.T) {
	db := openServiceDB(t)
	pending := seedServiceTask(t, db, "machine-a", "idle-pending")
	observed := watchTaskStore(db)
	runner := newBlockingControlRunner()
	service := NewService("machine-a", observed, runner)
	accepted, err := service.Pause(context.Background(), controlRequestStore(pending))
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Status != "pausing" {
		t.Fatalf("accepted status=%q", accepted.Status)
	}
	receiveTransition(t, observed.transitions, "paused")
	requireNoRun(t, runner.Runs)
}

func seedServiceTask(t *testing.T, db *store.DB, machineID, taskID string) store.LocalTask {
	t.Helper()
	request := CreateRequest{TaskID: taskID, Roots: []string{`D:\media`}, Mode: proto.LocalTaskModeScanOnly}
	envelope, digest, err := EncodeCreateEnvelope(request)
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.CreateOrLoadLocalTask(context.Background(), store.LocalTaskCreate{
		TaskID: taskID, MachineID: machineID, Source: "local", Type: "scan",
		EnvelopeDigest: digest, Envelope: envelope,
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func controlRequest(task Task) ControlRequest {
	return ControlRequest{TaskID: task.TaskID, InstanceID: task.InstanceID, ExpectedRevision: task.Revision}
}

func controlRequestStore(task store.LocalTask) ControlRequest {
	return ControlRequest{TaskID: task.TaskID, InstanceID: task.InstanceID, ExpectedRevision: task.Revision}
}

func awaitTaskAbsent(t *testing.T, service Service, taskID string) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		page, err := service.List(context.Background(), ListRequest{Limit: 200})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, task := range page.Items {
			found = found || task.TaskID == taskID
		}
		if !found {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("task %q remained visible", taskID)
		default:
			runtime.Gosched()
		}
	}
}
