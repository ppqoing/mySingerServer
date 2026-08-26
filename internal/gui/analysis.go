package gui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"dedup/internal/firstscreen"
)

const analysisUnavailable = "firstscreen analysis unavailable"

// AnalysisRunner is the narrow boundary between the GUI HTTP lifecycle and
// one configured first-screen analysis run. The ctx is the per-run
// cancellation scope: POST /api/analysis/firstscreen/cancel cancels it, and
// implementations layer it on top of the process shutdown context.
type AnalysisRunner interface {
	Run(ctx context.Context) (*firstscreen.RunStats, error)
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
	cancel  context.CancelFunc
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
	mux.HandleFunc("POST /api/analysis/firstscreen/cancel", h.handleCancel)
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
	runCtx, cancelRun := context.WithCancel(context.Background())
	h.cancel = cancelRun
	h.mu.Unlock()

	go h.execute(runCtx)
	writeJSON(response, http.StatusAccepted, map[string]string{
		"status": "started",
	})
}

// handleCancel 取消当前运行的分析：无运行中任务 409，runner 未配置 503。
// 取消幂等——运行未结束前重复调用重复触发同一取消信号。
func (h *AnalysisHandlers) handleCancel(response http.ResponseWriter, _ *http.Request) {
	h.mu.Lock()
	cancel := h.cancel
	running := h.running
	runnerConfigured := h.runner != nil
	h.mu.Unlock()
	if !runnerConfigured {
		writeJSON(response, http.StatusServiceUnavailable, map[string]string{
			"error": analysisUnavailable,
		})
		return
	}
	if !running || cancel == nil {
		writeJSON(response, http.StatusConflict, map[string]string{
			"error": "没有正在运行的分析",
		})
		return
	}
	cancel()
	writeJSON(response, http.StatusOK, map[string]string{
		"status": "cancelling",
	})
}

func (h *AnalysisHandlers) execute(ctx context.Context) {
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
		h.cancel = nil
		h.last = cloneRunStats(stats)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				h.lastErr = "已取消"
			} else {
				h.lastErr = err.Error()
			}
		} else {
			h.lastErr = ""
		}
		close(h.runDone)
		h.runDone = nil
	}()

	stats, err = h.runner.Run(ctx)
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
