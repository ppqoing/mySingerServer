package localtask

import (
	"errors"
	"sync"
	"testing"
	"time"

	"dedup/internal/worker"
)

// Break caught: a single FIFO lets one source/stage backlog run before every
// competing queue, starving local analysis behind scan or manager work.
func TestFairSchedulerRotatesAcrossSourceAndStage(t *testing.T) {
	backend := newBlockedSchedulerPool()
	scheduler := NewFairScheduler(backend)
	t.Cleanup(scheduler.Close)

	jobs := []worker.JobMsg{
		{JobID: 1, Source: worker.JobSourceScan},
		{JobID: 2, Source: worker.JobSourceScan},
		{JobID: 3, Source: worker.JobSourceScan},
		{JobID: 4, Source: worker.JobSourceManager, ScreenStage: worker.ScreenStageTwo},
		{JobID: 5, Source: worker.JobSourceManager, ScreenStage: worker.ScreenStageThree},
		{JobID: 6, Source: worker.JobSourceLocal, ScreenStage: worker.ScreenStageTwo},
		{JobID: 7, Source: worker.JobSourceLocal, ScreenStage: worker.ScreenStageThree},
	}

	backend.allow <- struct{}{}
	if err := scheduler.Submit(&jobs[0]); err != nil {
		t.Fatalf("Submit first scan: %v", err)
	}

	start := make(chan struct{})
	errs := make(chan error, len(jobs)-1)
	for index := 1; index < len(jobs); index++ {
		job := &jobs[index]
		go func() {
			<-start
			errs <- scheduler.Submit(job)
		}()
	}
	close(start)
	waitForSchedulerPending(t, scheduler, len(jobs)-1)
	for range jobs[1:] {
		backend.allow <- struct{}{}
	}
	for range jobs[1:] {
		if err := <-errs; err != nil {
			t.Fatalf("queued Submit: %v", err)
		}
	}

	got := backend.jobIDs()
	if len(got) != len(jobs) || got[0] != 1 {
		t.Fatalf("submitted IDs = %v, want first ID 1 and %d total", got, len(jobs))
	}
	firstFairRound := make(map[schedulerQueueKey]bool)
	byID := make(map[int64]worker.JobMsg, len(jobs))
	for _, job := range jobs {
		byID[job.JobID] = job
	}
	for _, id := range got[1:6] {
		job := byID[id]
		firstFairRound[schedulerKey(job)] = true
	}
	if len(firstFairRound) != 5 {
		t.Fatalf("first full round with all queues waiting covers %d queues, want all 5: order=%v", len(firstFairRound), got)
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

	blocked := make(chan error, 1)
	go func() {
		blocked <- scheduler.Submit(&worker.JobMsg{JobID: 2, Source: worker.JobSourceScan})
	}()
	backend.waitForWaiting(t, 1)
	scheduler.Close()
	select {
	case err := <-blocked:
		if !errors.Is(err, ErrFairSchedulerClosed) {
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
		if scheduler.current != nil {
			queued++
		}
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
func (p *blockedSchedulerPool) Metrics() worker.MetricsSnapshot      { return worker.MetricsSnapshot{} }

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
