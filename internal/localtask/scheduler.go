package localtask

import (
	"errors"
	"fmt"
	"sort"
	"sync"

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

// FairScheduler is the single bounded submission boundary shared by scan,
// manager and local analysis work. It rotates active source+stage queues; it
// never creates one goroutine per job.
type FairScheduler struct {
	pool     schedulerPool
	capacity int

	mu       sync.Mutex
	cond     *sync.Cond
	queues   map[schedulerQueueKey][]*scheduledSubmit
	pending  int
	current  *scheduledSubmit
	last     schedulerQueueKey
	haveLast bool
	closed   bool
	done     chan struct{}
	once     sync.Once
}

func NewFairScheduler(pool schedulerPool) *FairScheduler {
	return newFairScheduler(pool, defaultFairSchedulerCapacity)
}

func newFairScheduler(pool schedulerPool, capacity int) *FairScheduler {
	if capacity < 1 {
		capacity = 1
	}
	scheduler := &FairScheduler{
		pool: pool, capacity: capacity,
		queues: make(map[schedulerQueueKey][]*scheduledSubmit),
		done:   make(chan struct{}),
	}
	scheduler.cond = sync.NewCond(&scheduler.mu)
	go scheduler.dispatch()
	return scheduler
}

func (s *FairScheduler) Submit(job *worker.JobMsg) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("%w: missing worker pool", ErrFairSchedulerClosed)
	}
	if job == nil {
		return fmt.Errorf("localtask: submit nil worker job")
	}
	request := &scheduledSubmit{job: cloneScheduledJob(*job), done: make(chan error, 1)}
	key := schedulerKey(request.job)
	s.mu.Lock()
	for s.pending >= s.capacity && !s.closed {
		s.cond.Wait()
	}
	if s.closed {
		s.mu.Unlock()
		return ErrFairSchedulerClosed
	}
	s.queues[key] = append(s.queues[key], request)
	s.pending++
	s.cond.Signal()
	s.mu.Unlock()
	return <-request.done
}

func (s *FairScheduler) dispatch() {
	defer close(s.done)
	for {
		s.mu.Lock()
		for s.pending == 0 && !s.closed {
			s.cond.Wait()
		}
		if s.closed {
			pending := s.takeAllLocked()
			s.mu.Unlock()
			for _, request := range pending {
				request.done <- ErrFairSchedulerClosed
			}
			return
		}
		key, request := s.nextLocked()
		s.last, s.haveLast = key, true
		s.pending--
		s.current = request
		s.cond.Broadcast()
		s.mu.Unlock()

		err := s.pool.Submit(&request.job)
		completeScheduledSubmit(request, err)
		s.mu.Lock()
		if s.current == request {
			s.current = nil
		}
		s.mu.Unlock()
	}
}

func (s *FairScheduler) nextLocked() (schedulerQueueKey, *scheduledSubmit) {
	keys := make([]schedulerQueueKey, 0, len(s.queues))
	for key := range s.queues {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return schedulerKeyLess(keys[i], keys[j]) })
	selected := 0
	if s.haveLast {
		selected = sort.Search(len(keys), func(index int) bool {
			return schedulerKeyLess(s.last, keys[index])
		})
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
	s.pending = 0
	s.cond.Broadcast()
	return result
}

func (s *FairScheduler) Close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		pending := s.takeAllLocked()
		current := s.current
		s.cond.Broadcast()
		s.mu.Unlock()
		for _, request := range pending {
			completeScheduledSubmit(request, ErrFairSchedulerClosed)
		}
		if current != nil {
			completeScheduledSubmit(current, ErrFairSchedulerClosed)
		}
	})
}

func (s *FairScheduler) Results() <-chan *worker.JobResultMsg { return s.pool.Results() }
func (s *FairScheduler) Crashes() <-chan worker.CrashRecord   { return s.pool.Crashes() }
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
