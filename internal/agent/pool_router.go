package agent

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"dedup/internal/worker"
)

var errPoolRouterClosed = errors.New("agent: worker pool router closed")

type poolTerminal struct {
	result *worker.JobResultMsg
	crash  *worker.CrashRecord
	err    error
}

type poolRoute struct {
	job      worker.JobMsg
	terminal chan poolTerminal
}

// PoolRouter is the only owner allowed to consume the process-wide Worker
// result and crash channels. Managers register exact job owners and receive a
// private, one-shot terminal channel.
type PoolRouter struct {
	pool WorkerPool
	log  *slog.Logger

	nextJob atomic.Int64
	mu      sync.Mutex
	routes  map[int64]poolRoute
	closed  bool
}

func NewPoolRouter(pool WorkerPool, log *slog.Logger) *PoolRouter {
	router := &PoolRouter{
		pool:   pool,
		log:    log,
		routes: make(map[int64]poolRoute),
	}
	go router.run()
	return router
}

func (r *PoolRouter) NextJobID() int64 {
	return r.nextJob.Add(1)
}

func (r *PoolRouter) Register(
	job *worker.JobMsg,
) (<-chan poolTerminal, func(), error) {
	if job == nil {
		return nil, nil, fmt.Errorf("agent: register nil worker job")
	}
	if job.JobID <= 0 {
		return nil, nil, fmt.Errorf("agent: register invalid worker job ID %d", job.JobID)
	}
	if job.Source == "" {
		if job.Phase == worker.Phase1 {
			job.Source = worker.JobSourceScan
		} else {
			job.Source = worker.JobSourceManager
		}
	}
	route := poolRoute{
		job:      cloneWorkerJob(*job),
		terminal: make(chan poolTerminal, 1),
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, nil, errPoolRouterClosed
	}
	if _, exists := r.routes[job.JobID]; exists {
		r.mu.Unlock()
		return nil, nil, fmt.Errorf(
			"agent: duplicate worker job route %d",
			job.JobID,
		)
	}
	r.routes[job.JobID] = route
	r.mu.Unlock()
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.routes, job.JobID)
			r.mu.Unlock()
		})
	}
	return route.terminal, cancel, nil
}

func (r *PoolRouter) run() {
	results := r.pool.Results()
	crashes := r.pool.Crashes()
	for {
		select {
		case result, open := <-results:
			if !open {
				r.failAll(fmt.Errorf("%w: worker pool closed", errPoolRouterClosed))
				return
			}
			if result != nil {
				r.routeResult(result)
			}
		case crash, open := <-crashes:
			if !open {
				crashes = nil
				continue
			}
			r.routeCrash(crash)
		}
	}
}

func (r *PoolRouter) routeResult(result *worker.JobResultMsg) {
	r.mu.Lock()
	route, exists := r.routes[result.JobID]
	implicitPhaseOneSource := exists && route.job.Phase == worker.Phase1 &&
		result.ScreenStage == worker.ScreenStageLegacy && result.Source == ""
	if !exists ||
		result.ScanTaskID != route.job.ScanTaskID ||
		result.Path != route.job.Path ||
		result.Phase != route.job.Phase ||
		result.Kind != route.job.Kind ||
		(!implicitPhaseOneSource && result.ScreenStage != route.job.ScreenStage) ||
		(!implicitPhaseOneSource && result.Source != route.job.Source) {
		r.mu.Unlock()
		if r.log != nil {
			r.log.Warn(
				"ignored foreign worker result",
				"job_id", result.JobID,
				"task_id", result.ScanTaskID,
				"path", result.Path,
			)
		}
		return
	}
	if implicitPhaseOneSource {
		result.ScreenStage = route.job.ScreenStage
		result.Source = route.job.Source
	}
	delete(r.routes, result.JobID)
	r.mu.Unlock()
	route.terminal <- poolTerminal{result: cloneWorkerResult(result)}
}

func (r *PoolRouter) routeCrash(crash worker.CrashRecord) {
	r.mu.Lock()
	route, exists := r.routes[crash.JobID]
	if !exists ||
		crash.ScanTaskID != route.job.ScanTaskID ||
		crash.File != route.job.Path {
		r.mu.Unlock()
		if r.log != nil {
			r.log.Warn(
				"ignored foreign worker crash",
				"job_id", crash.JobID,
				"task_id", crash.ScanTaskID,
				"path", crash.File,
			)
		}
		return
	}
	delete(r.routes, crash.JobID)
	r.mu.Unlock()
	copy := crash
	route.terminal <- poolTerminal{crash: &copy}
}

func (r *PoolRouter) failAll(cause error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	routes := make([]poolRoute, 0, len(r.routes))
	for jobID, route := range r.routes {
		routes = append(routes, route)
		delete(r.routes, jobID)
	}
	r.mu.Unlock()
	for _, route := range routes {
		route.terminal <- poolTerminal{err: cause}
	}
}

func cloneWorkerJob(job worker.JobMsg) worker.JobMsg {
	job.KnownSHA = append([]byte(nil), job.KnownSHA...)
	return job
}

func cloneWorkerResult(result *worker.JobResultMsg) *worker.JobResultMsg {
	if result == nil {
		return nil
	}
	copy := *result
	copy.SHA512 = append([]byte(nil), result.SHA512...)
	copy.PDQ = append([]byte(nil), result.PDQ...)
	copy.DurationMS = cloneInt64(result.DurationMS)
	copy.ThumbPDQ = append([]byte(nil), result.ThumbPDQ...)
	copy.ThumbQuality = cloneInt32(result.ThumbQuality)
	copy.PHashParts = append([]byte(nil), result.PHashParts...)
	copy.SobelHist = append([]byte(nil), result.SobelHist...)
	copy.Errors = append([]worker.FieldError(nil), result.Errors...)
	copy.Frames = make([]worker.FrameFeature, len(result.Frames))
	for index, frame := range result.Frames {
		copy.Frames[index] = frame
		copy.Frames[index].PDQ256 = append([]byte(nil), frame.PDQ256...)
		copy.Frames[index].PHashParts = append(
			[]byte(nil),
			frame.PHashParts...,
		)
		copy.Frames[index].SobelHist = append(
			[]byte(nil),
			frame.SobelHist...,
		)
	}
	return &copy
}
