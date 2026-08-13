package localtask

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"dedup/internal/worker"
)

// Break caught: returning from backend.Submit is not terminal completion. A
// dispatcher that treats it as completion can fill the backend's 1024 FIFO
// with scans before local or manager work becomes visible.
func TestFairSchedulerLimitsBackendInflightAndRotatesAfterTerminal(t *testing.T) {
	backend := newBufferedSchedulerPool(1024, 2)
	scheduler := newFairScheduler(backend, 1100)
	t.Cleanup(scheduler.Close)

	results := scheduler.Results()
	var waits sync.WaitGroup
	for id := int64(1); id <= 20; id++ {
		waits.Add(1)
		go func(jobID int64) {
			defer waits.Done()
			_ = scheduler.Submit(&worker.JobMsg{JobID: jobID, Source: worker.JobSourceScan})
		}(id)
	}
	waitForBufferedSubmissions(t, backend, 2)
	time.Sleep(20 * time.Millisecond)
	if got := len(backend.jobIDs()); got != 2 {
		t.Fatalf("backend received %d jobs before terminal, want worker capacity 2", got)
	}

	for _, job := range []worker.JobMsg{
		{JobID: 1001, Source: worker.JobSourceLocal, ScreenStage: worker.ScreenStageTwo},
		{JobID: 1002, Source: worker.JobSourceManager, ScreenStage: worker.ScreenStageThree},
	} {
		waits.Add(1)
		go func(job worker.JobMsg) { defer waits.Done(); _ = scheduler.Submit(&job) }(job)
	}
	waitForSchedulerPending(t, scheduler, 20)
	initial := backend.jobIDs()
	backend.results <- &worker.JobResultMsg{JobID: initial[0]}
	if got := <-results; got.JobID != initial[0] {
		t.Fatalf("forwarded result = %d", got.JobID)
	}
	waitForBufferedSubmissions(t, backend, 3)
	backend.results <- &worker.JobResultMsg{JobID: initial[1]}
	if got := <-results; got.JobID != initial[1] {
		t.Fatalf("forwarded result = %d", got.JobID)
	}
	waitForBufferedSubmissions(t, backend, 4)
	ids := backend.jobIDs()
	if ids[2] != 1001 || ids[3] != 1002 {
		t.Fatalf("first jobs after scan terminals = %v, want local then manager before more scan", ids[:4])
	}
	completed := map[int64]bool{initial[0]: true, initial[1]: true}
	for len(completed) < 22 {
		for _, id := range backend.jobIDs() {
			if !completed[id] {
				completed[id] = true
				backend.results <- &worker.JobResultMsg{JobID: id}
				<-results
			}
		}
		time.Sleep(time.Millisecond)
	}
	waits.Wait()
}

func TestFairSchedulerForwardsCrashOnceAndShutdownWaitsInflight(t *testing.T) {
	backend := newBufferedSchedulerPool(8, 1)
	scheduler := newFairScheduler(backend, 8)
	if err := scheduler.Submit(&worker.JobMsg{JobID: 9, Source: worker.JobSourceLocal}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- scheduler.Shutdown(ctx) }()
	select {
	case err := <-done:
		t.Fatalf("Shutdown returned before terminal: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	backend.crashes <- worker.CrashRecord{JobID: 9, ScanTaskID: "task", File: `D:\media\x`}
	crash := <-scheduler.Crashes()
	if crash.JobID != 9 {
		t.Fatalf("crash=%#v", crash)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case _, open := <-scheduler.Crashes():
		if open {
			t.Fatal("crash channel remained open")
		}
	default:
		t.Fatal("crash channel not closed")
	}
}

// Break caught: cancellation used to return to the caller while leaving a
// queued job that could later reach the backend.
func TestFairSchedulerSubmitContextRemovesCancelledQueuedJob(t *testing.T) {
	backend := newBufferedSchedulerPool(1024, 1)
	scheduler := newFairScheduler(backend, 8)
	t.Cleanup(scheduler.Close)
	if err := scheduler.Submit(&worker.JobMsg{JobID: 1, Source: worker.JobSourceScan}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- scheduler.SubmitContext(ctx, &worker.JobMsg{JobID: 2, Source: worker.JobSourceLocal}) }()
	waitForSchedulerPending(t, scheduler, 1)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("SubmitContext = %v", err)
	}
	backend.results <- &worker.JobResultMsg{JobID: 1}
	<-scheduler.Results()
	time.Sleep(20 * time.Millisecond)
	if ids := backend.jobIDs(); len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("backend jobs = %v", ids)
	}
}

// Break caught: admission waited on a condition variable that cancellation
// never signalled, so a caller blocked behind a full scheduler backlog could
// not observe its own context until unrelated work completed.
func TestFairSchedulerAdmissionCancellationDoesNotWaitForTerminalOrEnterQueue(t *testing.T) {
	backend := newBufferedSchedulerPool(1024, 1)
	scheduler := newFairScheduler(backend, 1)
	if err := scheduler.Submit(&worker.JobMsg{JobID: 1, Source: worker.JobSourceScan}); err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan error, 1)
	go func() { done2 <- scheduler.SubmitContext(ctx2, &worker.JobMsg{JobID: 2, Source: worker.JobSourceScan}) }()
	waitForSchedulerPending(t, scheduler, 1)

	ctx3, cancel3 := context.WithCancel(context.Background())
	done3 := make(chan error, 1)
	go func() {
		done3 <- scheduler.SubmitContext(ctx3, &worker.JobMsg{JobID: 3, Source: worker.JobSourceLocal})
	}()
	cancel3()
	returnedWithoutWake := false
	select {
	case err := <-done3:
		returnedWithoutWake = errors.Is(err, context.Canceled)
	case <-time.After(50 * time.Millisecond):
	}

	cancel2()
	if err := <-done2; !errors.Is(err, context.Canceled) {
		t.Fatalf("pending SubmitContext=%v", err)
	}
	if !returnedWithoutWake {
		select {
		case <-done3:
		case <-time.After(time.Second):
			t.Fatal("admission caller remained blocked after cleanup wake")
		}
	}
	backend.results <- &worker.JobResultMsg{JobID: 1}
	<-scheduler.Results()
	shutdownCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := scheduler.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	if !returnedWithoutWake {
		t.Fatal("admission cancellation waited for an unrelated backlog wake")
	}
	if ids := backend.jobIDs(); len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("cancelled admission reached backend: %v", ids)
	}
}

func TestFairSchedulerStopAcceptingReachesBackendAndCloseStopsLoops(t *testing.T) {
	backend := newBufferedSchedulerPool(1024, 1)
	scheduler := newFairScheduler(backend, 8)
	scheduler.StopAccepting()
	if !backend.stopped() {
		t.Fatal("backend StopAccepting was not called")
	}
	if err := scheduler.Submit(&worker.JobMsg{JobID: 1}); !errors.Is(err, ErrFairSchedulerClosed) {
		t.Fatalf("Submit=%v", err)
	}
	scheduler.Close()
	select {
	case <-scheduler.done:
	default:
		t.Fatal("dispatch loop still running")
	}
	select {
	case <-scheduler.forwardDone:
	default:
		t.Fatal("forward loop still running")
	}
}

// Break caught: scheduler shutdown can strand Submit callers forever or lose
// the backend rejection that PoolRouter needs to clean a registered route.
func TestFairSchedulerPropagatesSubmitFailureAndUnblocksOnClose(t *testing.T) {
	sentinel := errors.New("pool rejected")
	backend := newBlockedSchedulerPool()
	backend.submitErr = sentinel
	scheduler := NewFairScheduler(backend)
	backend.allow <- struct{}{}
	if err := scheduler.Submit(&worker.JobMsg{JobID: 1, Source: worker.JobSourceLocal, ScreenStage: worker.ScreenStageTwo}); !errors.Is(err, sentinel) {
		t.Fatalf("Submit error = %v, want %v", err, sentinel)
	}
	backend.submitErr = nil

	blocked := make(chan error, 1)
	go func() {
		blocked <- scheduler.Submit(&worker.JobMsg{JobID: 2, Source: worker.JobSourceScan})
	}()
	backend.waitForWaiting(t, 1)
	scheduler.Close()
	select {
	case err := <-blocked:
		if !errors.Is(err, ErrFairSchedulerClosed) && !errors.Is(err, worker.ErrPoolClosed) {
			t.Fatalf("blocked Submit error = %v, want ErrFairSchedulerClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked Submit was not released by Close")
	}
}

func waitForSchedulerPending(t *testing.T, scheduler *FairScheduler, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		scheduler.mu.Lock()
		queued := scheduler.pending
		scheduler.mu.Unlock()
		if queued == count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("scheduler did not reach %d pending submissions", count)
}

type blockedSchedulerPool struct {
	mu        sync.Mutex
	submitted []worker.JobMsg
	allow     chan struct{}
	waiting   chan struct{}
	results   chan *worker.JobResultMsg
	crashes   chan worker.CrashRecord
	submitErr error
	stopped   bool
}

type bufferedSchedulerPool struct {
	mu        sync.Mutex
	jobs      chan *worker.JobMsg
	results   chan *worker.JobResultMsg
	crashes   chan worker.CrashRecord
	ready     int64
	stop      bool
	submitted []int64
}

func newBufferedSchedulerPool(capacity int, ready int64) *bufferedSchedulerPool {
	return &bufferedSchedulerPool{jobs: make(chan *worker.JobMsg, capacity), results: make(chan *worker.JobResultMsg, capacity), crashes: make(chan worker.CrashRecord, capacity), ready: ready}
}
func (p *bufferedSchedulerPool) Submit(job *worker.JobMsg) error {
	p.mu.Lock()
	if p.stop {
		p.mu.Unlock()
		return worker.ErrPoolClosed
	}
	p.submitted = append(p.submitted, job.JobID)
	p.mu.Unlock()
	p.jobs <- job
	return nil
}
func (p *bufferedSchedulerPool) Results() <-chan *worker.JobResultMsg { return p.results }
func (p *bufferedSchedulerPool) Crashes() <-chan worker.CrashRecord   { return p.crashes }
func (p *bufferedSchedulerPool) Metrics() worker.MetricsSnapshot {
	return worker.MetricsSnapshot{ReadyWorkers: p.ready}
}
func (p *bufferedSchedulerPool) StopAccepting() { p.mu.Lock(); p.stop = true; p.mu.Unlock() }
func (p *bufferedSchedulerPool) stopped() bool  { p.mu.Lock(); defer p.mu.Unlock(); return p.stop }
func (p *bufferedSchedulerPool) jobIDs() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int64(nil), p.submitted...)
}

func waitForBufferedSubmissions(t *testing.T, p *bufferedSchedulerPool, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(p.jobIDs()) >= count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("backend did not receive %d jobs: %v", count, p.jobIDs())
}

func newBlockedSchedulerPool() *blockedSchedulerPool {
	return &blockedSchedulerPool{
		allow: make(chan struct{}, 32), waiting: make(chan struct{}, 32),
		results: make(chan *worker.JobResultMsg), crashes: make(chan worker.CrashRecord),
	}
}

func (p *blockedSchedulerPool) Submit(job *worker.JobMsg) error {
	p.waiting <- struct{}{}
	<-p.allow
	p.mu.Lock()
	stopped := p.stopped
	p.mu.Unlock()
	if stopped {
		return worker.ErrPoolClosed
	}
	if p.submitErr != nil {
		return p.submitErr
	}
	copy := *job
	p.mu.Lock()
	p.submitted = append(p.submitted, copy)
	p.mu.Unlock()
	return nil
}

func (p *blockedSchedulerPool) Results() <-chan *worker.JobResultMsg { return p.results }
func (p *blockedSchedulerPool) Crashes() <-chan worker.CrashRecord   { return p.crashes }
func (p *blockedSchedulerPool) Metrics() worker.MetricsSnapshot {
	return worker.MetricsSnapshot{ReadyWorkers: 32}
}
func (p *blockedSchedulerPool) StopAccepting() {
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
	select {
	case p.allow <- struct{}{}:
	default:
	}
}

func (p *blockedSchedulerPool) waitForWaiting(t *testing.T, count int) {
	t.Helper()
	for index := 0; index < count; index++ {
		select {
		case <-p.waiting:
		case <-time.After(time.Second):
			t.Fatalf("backend received %d/%d expected submissions", index, count)
		}
	}
}

func (p *blockedSchedulerPool) jobIDs() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := make([]int64, len(p.submitted))
	for index, job := range p.submitted {
		ids[index] = job.JobID
	}
	return ids
}
