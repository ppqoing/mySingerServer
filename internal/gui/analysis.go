package gui

import (
	"fmt"
	"net/http"
	"sync"

	"dedup/internal/firstscreen"
)

const analysisUnavailable = "firstscreen analysis unavailable"

// AnalysisRunner is the narrow boundary between the GUI HTTP lifecycle and
// one configured first-screen analysis run.
type AnalysisRunner interface {
	Run() (*firstscreen.RunStats, error)
}

type analysisStatus struct {
	Running bool                  `json:"running"`
	Last    *firstscreen.RunStats `json:"last"`
	LastErr string                `json:"last_err"`
}

// AnalysisHandlers owns the single-run admission gate and immutable status
// snapshots exposed by the GUI HTTP API.
type AnalysisHandlers struct {
	runner AnalysisRunner
	hook   func() error

	mu      sync.Mutex
	closing bool
	running bool
	runDone chan struct{}
	last    *firstscreen.RunStats
	lastErr string
}

func NewAnalysisHandlers(
	runner AnalysisRunner,
	hooks ...func() error,
) *AnalysisHandlers {
	var hook func() error
	if len(hooks) > 0 {
		hook = hooks[0]
	}
	return &AnalysisHandlers{runner: runner, hook: hook}
}

func (h *AnalysisHandlers) SetSuccessHook(hook func() error) {
	h.mu.Lock()
	h.hook = hook
	h.mu.Unlock()
}

func (h *AnalysisHandlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/analysis/firstscreen/run", h.handleRun)
	mux.HandleFunc("GET /api/analysis/firstscreen/status", h.handleStatus)
}

func (h *AnalysisHandlers) handleRun(response http.ResponseWriter, _ *http.Request) {
	h.mu.Lock()
	if h.runner == nil {
		h.mu.Unlock()
		writeJSON(response, http.StatusServiceUnavailable, map[string]string{
			"error": analysisUnavailable,
		})
		return
	}
	if h.closing {
		h.mu.Unlock()
		writeJSON(response, http.StatusServiceUnavailable, map[string]string{
			"error": "firstscreen analysis shutting down",
		})
		return
	}
	if h.running {
		h.mu.Unlock()
		writeJSON(response, http.StatusConflict, map[string]string{
			"error": "firstscreen already running",
		})
		return
	}
	h.running = true
	h.runDone = make(chan struct{})
	h.mu.Unlock()

	go h.execute()
	writeJSON(response, http.StatusAccepted, map[string]string{
		"status": "started",
	})
}

func (h *AnalysisHandlers) execute() {
	var (
		stats *firstscreen.RunStats
		err   error
	)
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("firstscreen runner panic: %v", recovered)
			stats = nil
		}

		h.mu.Lock()
		defer h.mu.Unlock()
		h.running = false
		h.last = cloneRunStats(stats)
		if err != nil {
			h.lastErr = err.Error()
		} else {
			h.lastErr = ""
		}
		close(h.runDone)
		h.runDone = nil
	}()

	stats, err = h.runner.Run()
	if err != nil {
		return
	}
	h.mu.Lock()
	hook := h.hook
	hookAdmitted := hook != nil && !h.closing
	h.mu.Unlock()
	if hookAdmitted {
		err = hook()
	}
}

func (h *AnalysisHandlers) handleStatus(response http.ResponseWriter, _ *http.Request) {
	h.mu.Lock()
	status := analysisStatus{
		Running: h.running,
		Last:    cloneRunStats(h.last),
		LastErr: h.lastErr,
	}
	runnerConfigured := h.runner != nil
	h.mu.Unlock()

	if !runnerConfigured {
		status.LastErr = analysisUnavailable
		writeJSON(response, http.StatusServiceUnavailable, status)
		return
	}
	writeJSON(response, http.StatusOK, status)
}

func (h *AnalysisHandlers) BeginShutdown() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closing = true
}

func (h *AnalysisHandlers) Wait() {
	h.mu.Lock()
	done := h.runDone
	h.mu.Unlock()
	if done != nil {
		<-done
	}
}

func cloneRunStats(stats *firstscreen.RunStats) *firstscreen.RunStats {
	if stats == nil {
		return nil
	}
	cloned := *stats
	if stats.StageElapsedMs != nil {
		cloned.StageElapsedMs = make(map[string]int64, len(stats.StageElapsedMs))
		for stage, elapsed := range stats.StageElapsedMs {
			cloned.StageElapsedMs[stage] = elapsed
		}
	}
	return &cloned
}
