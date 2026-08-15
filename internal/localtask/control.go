package localtask

import (
	"context"
	"errors"
	"sync"
	"time"

	"dedup/internal/proto"
	"dedup/internal/store"
)

type DrainReason string

const (
	DrainPause           DrainReason = "pause"
	DrainStop            DrainReason = "stop"
	DrainDelete          DrainReason = "delete"
	DrainProcessShutdown DrainReason = "process_shutdown"
)

var ErrDrainRequested = errors.New("local_task_drain_requested")

type ControlRequest = proto.LocalTaskControlRequest
type ControlResult = proto.LocalTaskControlResponse

type RunControl struct {
	Context context.Context
	Drain   <-chan struct{}
	Reason  func() DrainReason
}

type ProgressUpdate struct {
	Phase              string
	Stage              int
	ProgressComplete   int64
	ProgressTotal      int64
	ProgressTotalKnown bool
	StatsJSON          string
}

type taskAttempt struct {
	mu          sync.RWMutex
	taskID      string
	instance    string
	revision    int64
	reason      DrainReason
	drain       chan struct{}
	drainOnce   sync.Once
	hardCancel  context.CancelFunc
	hardContext context.Context
	done        chan struct{}
	doneOnce    sync.Once
	reporter    *progressReporter
}

func newTaskAttempt(task store.LocalTask) (*taskAttempt, context.Context) {
	ctx, cancel := context.WithCancel(context.Background())
	return &taskAttempt{
		taskID: task.TaskID, instance: task.InstanceID, revision: task.Revision,
		drain: make(chan struct{}), hardCancel: cancel, hardContext: ctx, done: make(chan struct{}),
	}, ctx
}

func (a *taskAttempt) version() store.LocalTaskControl {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return store.LocalTaskControl{
		TaskID: a.taskID, InstanceID: a.instance, ExpectedRevision: a.revision,
	}
}

func (a *taskAttempt) drainReason() DrainReason {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.reason
}

func (a *taskAttempt) setRevision(revision int64) {
	a.mu.Lock()
	if revision > a.revision {
		a.revision = revision
	}
	a.mu.Unlock()
}

func (a *taskAttempt) upgrade(reason DrainReason, revision int64) {
	a.mu.Lock()
	if revision > a.revision {
		a.revision = revision
	}
	if drainPriority(reason) > drainPriority(a.reason) {
		a.reason = reason
	}
	a.mu.Unlock()
	if reason != "" {
		a.drainOnce.Do(func() { close(a.drain) })
	}
}

func (a *taskAttempt) setReporter(reporter *progressReporter) {
	a.mu.Lock()
	a.reporter = reporter
	a.mu.Unlock()
}

func (a *taskAttempt) progressReporter() *progressReporter {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.reporter
}

func (a *taskAttempt) finish() {
	a.hardCancel()
	a.markDone()
}

func (a *taskAttempt) markDone() {
	a.doneOnce.Do(func() { close(a.done) })
}

func drainPriority(reason DrainReason) int {
	switch reason {
	case DrainDelete:
		return 4
	case DrainStop:
		return 3
	case DrainPause:
		return 2
	case DrainProcessShutdown:
		return 1
	default:
		return 0
	}
}

type taskGate struct{ token chan struct{} }

func (g *taskGate) release() { g.token <- struct{}{} }

type progressTicker struct {
	channel <-chan time.Time
	stop    func()
}

type serviceOptions struct {
	logf             func(string, ...any)
	deleteRetryAfter func(time.Duration) <-chan time.Time
	newTicker        func(time.Duration) progressTicker
}

type ServiceOption func(*serviceOptions)

func WithServiceLogf(logf func(string, ...any)) ServiceOption {
	return func(options *serviceOptions) {
		if logf != nil {
			options.logf = logf
		}
	}
}

func WithDeleteRetryAfter(after func(time.Duration) <-chan time.Time) ServiceOption {
	return func(options *serviceOptions) {
		if after != nil {
			options.deleteRetryAfter = after
		}
	}
}

func withProgressTicker(factory func(time.Duration) progressTicker) ServiceOption {
	return func(options *serviceOptions) {
		if factory != nil {
			options.newTicker = factory
		}
	}
}

func defaultServiceOptions() serviceOptions {
	return serviceOptions{
		logf:             func(string, ...any) {},
		deleteRetryAfter: time.After,
		newTicker: func(interval time.Duration) progressTicker {
			ticker := time.NewTicker(interval)
			return progressTicker{channel: ticker.C, stop: ticker.Stop}
		},
	}
}

type progressReporter struct {
	service  *taskService
	attempt  *taskAttempt
	ticker   progressTicker
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once

	mu        sync.Mutex
	persisted ProgressUpdate
	pending   *ProgressUpdate
	reported  bool
	accepting bool
	stale     bool
}

func newProgressReporter(service *taskService, attempt *taskAttempt, task Task) *progressReporter {
	reporter := &progressReporter{
		service: service,
		attempt: attempt,
		ticker:  service.options.newTicker(time.Second),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		persisted: ProgressUpdate{
			Phase: task.Phase, Stage: task.Stage, ProgressComplete: task.ProgressComplete,
			ProgressTotal: task.ProgressTotal, ProgressTotalKnown: task.ProgressTotalKnown,
			StatsJSON: task.StatsJSON,
		},
		accepting: true,
	}
	go reporter.run()
	return reporter
}

func (r *progressReporter) run() {
	defer close(r.done)
	defer r.ticker.stop()
	for {
		select {
		case <-r.ticker.channel:
			_ = r.flush()
		case <-r.stop:
			return
		}
	}
}

func (r *progressReporter) report(update ProgressUpdate) error {
	r.mu.Lock()
	if !r.accepting || r.stale {
		r.mu.Unlock()
		return store.ErrLocalTaskStale
	}
	copy := update
	immediate := !r.reported || update.Phase != r.persisted.Phase
	r.reported = true
	r.pending = &copy
	r.mu.Unlock()
	if immediate {
		return r.flush()
	}
	return nil
}

func (r *progressReporter) stopAndFlush() error {
	r.mu.Lock()
	r.accepting = false
	r.mu.Unlock()
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
	return r.flush()
}

func (r *progressReporter) flush() error {
	gate, err := r.service.acquireTaskGate(context.Background(), r.attempt.version().TaskID)
	if err != nil {
		return err
	}
	defer gate.release()
	if r.service.currentAttempt(r.attempt.version().TaskID) != r.attempt {
		r.markStale()
		return store.ErrLocalTaskStale
	}
	return r.flushHeld()
}

// flushHeld requires the task gate. It deliberately retains a failed pending
// update so the next ticker edge or terminal flush can retry it.
func (r *progressReporter) flushHeld() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stale {
		return store.ErrLocalTaskStale
	}
	if r.pending == nil {
		return nil
	}
	update := *r.pending
	_, err := r.service.store.UpdateLocalTaskProgress(
		context.Background(), r.service.machineID, r.attempt.version(),
		store.LocalTaskProgressUpdate{
			Phase: update.Phase, Stage: update.Stage,
			ProgressComplete: update.ProgressComplete, ProgressTotal: update.ProgressTotal,
			ProgressTotalKnown: update.ProgressTotalKnown, StatsJSON: update.StatsJSON,
		},
	)
	if err == nil {
		r.persisted = update
		r.pending = nil
		return nil
	}
	if errors.Is(err, store.ErrLocalTaskStale) || errors.Is(err, store.ErrLocalTaskInstanceMismatch) {
		r.stale = true
		return store.ErrLocalTaskStale
	}
	r.service.options.logf("localtask: progress update failed for %s: %v", r.attempt.version().TaskID, err)
	return nil
}

func (r *progressReporter) markStale() {
	r.mu.Lock()
	r.stale = true
	r.accepting = false
	r.mu.Unlock()
}
