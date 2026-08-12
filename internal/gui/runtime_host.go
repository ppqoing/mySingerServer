package gui

import (
	"net/http"
	"strings"
	"sync"

	"dedup/internal/config"
)

type RuntimeHost struct {
	mu        sync.RWMutex
	api       *API
	configAPI http.Handler
	static    http.Handler
	status    RuntimeStatus
}

func NewRuntimeHost(config guiConfigStore, configuredAgents []config.AgentEndpoint) *RuntimeHost {
	agents := make([]AgentStatus, 0, len(configuredAgents))
	for _, agent := range configuredAgents {
		agents = append(agents, AgentStatus{Addr: agent.Addr, Online: false})
	}
	return &RuntimeHost{
		configAPI: newConfigHTTP(config),
		static:    http.FileServerFS(webFS()),
		status: RuntimeStatus{
			DatabaseState: "connecting",
			Agents:        agents,
		},
	}
}

func (h *RuntimeHost) Install(api *API) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.api = api
	if api != nil {
		h.status.DatabaseState = "ready"
		h.status.DatabaseErrorCode = ""
	}
}

func (h *RuntimeHost) SetDatabaseConnecting() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.status.DatabaseState = "connecting"
	h.status.DatabaseErrorCode = ""
}

func (h *RuntimeHost) SetDatabaseFailure(err error) {
	failure := ClassifyRuntimeFailure(err)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.status.DatabaseState = "failed"
	h.status.DatabaseErrorCode = failure.Code
}

func (h *RuntimeHost) SetRestartState(restarting bool, recoveryURL string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.status.Restarting = restarting
	h.status.RecoveryURL = recoveryURL
}

func (h *RuntimeHost) BeginAnalysisShutdown() {
	if api := h.current(); api != nil {
		api.BeginAnalysisShutdown()
	}
}

func (h *RuntimeHost) WaitForAnalysis() {
	if api := h.current(); api != nil {
		api.WaitForAnalysis()
	}
}

func (h *RuntimeHost) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	switch {
	case request.URL.Path == "/api/config":
		h.configAPI.ServeHTTP(response, request)
	case request.URL.Path == "/api/runtime/status":
		h.handleRuntimeStatus(response, request)
	case request.URL.Path == "/api/restart/health":
		h.handleRestartHealth(response, request)
	case request.URL.Path == "/api/agents" && h.current() == nil:
		h.handleOfflineAgents(response, request)
	case strings.HasPrefix(request.URL.Path, "/api/") && h.current() == nil:
		writeJSON(response, http.StatusServiceUnavailable, map[string]string{"error": "database_unavailable"})
	case h.current() != nil:
		h.current().Routes().ServeHTTP(response, request)
	default:
		h.static.ServeHTTP(response, request)
	}
}

func (h *RuntimeHost) current() *API {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.api
}

func (h *RuntimeHost) handleRuntimeStatus(response http.ResponseWriter, _ *http.Request) {
	h.mu.RLock()
	status := h.status
	status.Agents = append([]AgentStatus(nil), h.status.Agents...)
	h.mu.RUnlock()
	writeJSON(response, http.StatusOK, status)
}

func (h *RuntimeHost) handleRestartHealth(response http.ResponseWriter, _ *http.Request) {
	response.Header().Set("Access-Control-Allow-Origin", "*")
	writeJSON(response, http.StatusOK, map[string]bool{"ok": true})
}

func (h *RuntimeHost) handleOfflineAgents(response http.ResponseWriter, _ *http.Request) {
	h.mu.RLock()
	agents := append([]AgentStatus(nil), h.status.Agents...)
	h.mu.RUnlock()
	writeJSON(response, http.StatusOK, agents)
}
