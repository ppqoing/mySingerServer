package localtask

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"dedup/internal/worker"
)

var ErrFairSchedulerClosed = errors.New("localtask: fair scheduler closed")

const defaultFairSchedulerCapacity = 1024

type schedulerPool interface {
	Submit(*worker.JobMsg) error
	Results() <-chan *worker.JobResultMsg
	Crashes() <-chan worker.CrashRecord
	Metrics() worker.MetricsSnapshot
}

type schedulerQueueKey struct {
	source worker.JobSource
	stage  worker.ScreenStage
}

type scheduledSubmit struct {
	job  worker.JobMsg
	done chan error
}

// FairScheduler owns the backlog in front of the Worker pool. A job remains
// in-flight until its result or crash is forwarded, so the pool's larger FIFO
// cannot hide competing source+stage queues.
type FairScheduler struct {
	pool     schedulerPool
	capacity int

	mu       sync.Mutex
	cond     *sync.Cond
	queues   map[schedulerQueueKey][]*scheduledSubmit
	pending  int
	inflight map[int64]struct{}
	last     schedulerQueueKey
	haveLast bool
	closed   bool

	results       chan *worker.JobResultMsg
	crashes       chan worker.CrashRecord
	admission     chan struct{}
	admissionStop chan struct{}
	stop          chan struct{}
	done          chan struct{}
	forwardDone   chan struct{}
	stopOnce      sync.Once
	admissionOnce sync.Once
}

func NewFairScheduler(pool schedulerPool) *FairScheduler {
	return newFairScheduler(pool, defaultFairSchedulerCapacity)
}

func newFairScheduler(pool schedulerPool, capacity int) *FairScheduler {
	if capacity < 1 {
		capacity = 1
	}
	s := &FairScheduler{pool: pool, capacity: capacity, queues: make(map[schedulerQueueKey][]*scheduledSubmit), inflight: make(map[int64]struct{}), results: make(chan *worker.JobResultMsg, capacity), crashes: make(chan worker.CrashRecord, capacity), admission: make(chan struct{}, capacity), admissionStop: make(chan struct{}), stop: make(chan struct{}), done: make(chan struct{}), forwardDone: make(chan struct{})}
	s.cond = sync.NewCond(&s.mu)
	go s.dispatch()
	go s.forwardTerminals()
	return s
}

func (s *FairScheduler) Submit(job *worker.JobMsg) error {
	return s.SubmitContext(context.Background(), job)
}

func (s *FairScheduler) SubmitContext(ctx context.Context, job *worker.JobMsg) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("%w: missing worker pool", ErrFairSchedulerClosed)
	}
	if ctx == nil {
		return fmt.Errorf("localtask: submit context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("localtask: submit nil worker job")
	}
	request := &scheduledSubmit{job: cloneScheduledJob(*job), done: make(chan error, 1)}
	key := schedulerKey(request.job)
	select {
	case s.admission <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.admissionStop:
		return ErrFairSchedulerClosed
	}
	s.mu.Lock()
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		s.releaseAdmission()
		return err
	}
	if s.closed {
		s.mu.Unlock()
		s.releaseAdmission()
		return ErrFairSchedulerClosed
	}
	s.queues[key] = append(s.queues[key], request)
	s.pending++
	s.cond.Broadcast()
	s.mu.Unlock()
	select {
	case err := <-request.done:
		return err
	case <-ctx.Done():
		if s.removePending(request) {
			return ctx.Err()
		}
		select {
		case err := <-request.done:
			return err
		default:
			return ctx.Err()
		}
	}
}

func (s *FairScheduler) removePending(target *scheduledSubmit) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, queue := range s.queues {
		for i, request := range queue {
			if request != target {
				continue
			}
			s.queues[key] = append(queue[:i], queue[i+1:]...)
			if len(s.queues[key]) == 0 {
				delete(s.queues, key)
			}
			s.pending--
			s.releaseAdmission()
			s.cond.Broadcast()
			return true
		}
	}
	return false
}

func (s *FairScheduler) dispatch() {
	defer close(s.done)
	for {
		s.mu.Lock()
		for !s.closed && (s.pending == 0 || len(s.inflight) >= s.workerCapacityLocked()) {
			s.cond.Wait()
		}
		if s.closed {
			pending := s.takeAllLocked()
			s.mu.Unlock()
			for _, request := range pending {
				completeScheduledSubmit(request, ErrFairSchedulerClosed)
			}
			return
		}
		key, request := s.nextLocked()
		s.last, s.haveLast = key, true
		s.pending--
		s.releaseAdmission()
		s.inflight[request.job.JobID] = struct{}{}
		s.cond.Broadcast()
		s.mu.Unlock()
		err := s.pool.Submit(&request.job)
		if err != nil {
			s.release(request.job.JobID)
		}
		completeScheduledSubmit(request, err)
	}
}

func (s *FairScheduler) workerCapacityLocked() int {
	ready := s.pool.Metrics().ReadyWorkers
	if ready < 1 {
		return 1
	}
	return int(ready)
}

func (s *FairScheduler) forwardTerminals() {
	defer close(s.forwardDone)
	defer close(s.results)
	defer close(s.crashes)
	results, crashes := s.pool.Results(), s.pool.Crashes()
	for {
		select {
		case <-s.stop:
			return
		case result, open := <-results:
			if !open {
				return
			}
			if result == nil {
				continue
			}
			select {
			case s.results <- result:
				s.release(result.JobID)
			case <-s.stop:
				return
			}
		case crash, open := <-crashes:
			if !open {
				crashes = nil
				continue
			}
			select {
			case s.crashes <- crash:
				s.release(crash.JobID)
			case <-s.stop:
				return
			}
		}
	}
}

func (s *FairScheduler) release(jobID int64) {
	s.mu.Lock()
	delete(s.inflight, jobID)
	s.cond.Broadcast()
	s.mu.Unlock()
}

func (s *FairScheduler) nextLocked() (schedulerQueueKey, *scheduledSubmit) {
	keys := make([]schedulerQueueKey, 0, len(s.queues))
	for key := range s.queues {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return schedulerKeyLess(keys[i], keys[j]) })
	selected := 0
	if s.haveLast {
		selected = sort.Search(len(keys), func(i int) bool { return schedulerKeyLess(s.last, keys[i]) })
		if selected == len(keys) {
			selected = 0
		}
	}
	key := keys[selected]
	queue := s.queues[key]
	request := queue[0]
	if len(queue) == 1 {
		delete(s.queues, key)
	} else {
		s.queues[key] = queue[1:]
	}
	return key, request
}

func (s *FairScheduler) takeAllLocked() []*scheduledSubmit {
	result := make([]*scheduledSubmit, 0, s.pending)
	for _, queue := range s.queues {
		result = append(result, queue...)
	}
	s.queues = make(map[schedulerQueueKey][]*scheduledSubmit)
	for range s.pending {
		s.releaseAdmission()
	}
	s.pending = 0
	s.cond.Broadcast()
	return result
}

func (s *FairScheduler) StopAccepting() {
	if s == nil {
		return
	}
	s.mu.Lock()
	already := s.closed
	s.closed = true
	s.cond.Broadcast()
	s.mu.Unlock()
	if !already {
		s.admissionOnce.Do(func() { close(s.admissionStop) })
		if pool, ok := s.pool.(interface{ StopAccepting() }); ok {
			pool.StopAccepting()
		}
	}
}

func (s *FairScheduler) Close() {
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.Shutdown(ctx)
}

func (s *FairScheduler) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		return fmt.Errorf("localtask: shutdown context is required")
	}
	s.StopAccepting()
	select {
	case <-s.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		inflight := len(s.inflight)
		s.mu.Unlock()
		if inflight == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	s.stopOnce.Do(func() { close(s.stop) })
	select {
	case <-s.forwardDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *FairScheduler) Results() <-chan *worker.JobResultMsg { return s.results }
func (s *FairScheduler) Crashes() <-chan worker.CrashRecord   { return s.crashes }
func (s *FairScheduler) Metrics() worker.MetricsSnapshot      { return s.pool.Metrics() }
func (s *FairScheduler) EndTask(taskID string) {
	if pool, ok := s.pool.(interface{ EndTask(string) }); ok {
		pool.EndTask(taskID)
	}
}
func schedulerKey(job worker.JobMsg) schedulerQueueKey {
	return schedulerQueueKey{source: job.Source, stage: job.ScreenStage}
}
func schedulerKeyLess(left, right schedulerQueueKey) bool {
	if left.source != right.source {
		return left.source < right.source
	}
	return left.stage < right.stage
}
func cloneScheduledJob(job worker.JobMsg) worker.JobMsg {
	job.KnownSHA = append([]byte(nil), job.KnownSHA...)
	return job
}
func completeScheduledSubmit(request *scheduledSubmit, err error) {
	select {
	case request.done <- err:
	default:
	}
}

func (s *FairScheduler) releaseAdmission() {
	select {
	case <-s.admission:
	default:
	}
}
