package gui

import (
	"net/http"
	"strings"
	"sync"

	"dedup/internal/config"
)

type RuntimeHost struct {
	mu           sync.RWMutex
	api          *API
	configAPI    http.Handler
	static       http.Handler
	restart      guiRestartCoordinator
	restartToken string
	status       RuntimeStatus
	httpMu       sync.Mutex
	httpStopping bool
	httpActive   sync.WaitGroup

	afterRuntimeSnapshot func()
}

func NewRuntimeHost(config guiConfigStore, configuredAgents []config.AgentEndpoint, restartTokens ...string) *RuntimeHost {
	restartToken := ""
	if len(restartTokens) > 0 {
		restartToken = restartTokens[0]
	}
	agents := make([]AgentStatus, 0, len(configuredAgents))
	for _, agent := range configuredAgents {
		agents = append(agents, AgentStatus{Addr: agent.Addr, Online: false, IdentityState: IdentityPending})
	}
	return &RuntimeHost{
		configAPI:    newConfigHTTP(config),
		static:       http.FileServerFS(webFS()),
		restart:      restartCoordinatorFor(config),
		restartToken: restartToken,
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
		h.status.DatabaseState = "connected"
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
	h.status.DatabaseState = "error"
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

func (h *RuntimeHost) BeginHTTPShutdown() {
	h.httpMu.Lock()
	h.httpStopping = true
	h.httpMu.Unlock()
}

func (h *RuntimeHost) WaitForHTTP() {
	h.httpActive.Wait()
}

func (h *RuntimeHost) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	h.httpMu.Lock()
	if h.httpStopping {
		h.httpMu.Unlock()
		writeJSON(response, http.StatusServiceUnavailable, map[string]string{"error": "server_shutting_down"})
		return
	}
	h.httpActive.Add(1)
	h.httpMu.Unlock()
	defer h.httpActive.Done()

	api := h.current()
	if h.afterRuntimeSnapshot != nil {
		h.afterRuntimeSnapshot()
	}
	switch {
	case request.URL.Path == "/api/config":
		h.configAPI.ServeHTTP(response, request)
	case request.URL.Path == "/api/runtime/status":
		h.handleRuntimeStatus(response, request, api)
	case request.URL.Path == "/api/restart/health":
		h.handleRestartHealth(response, request)
	case request.URL.Path == "/api/agents" && api == nil:
		h.handleOfflineAgents(response, request)
	case strings.HasPrefix(request.URL.Path, "/api/") && api == nil:
		writeJSON(response, http.StatusServiceUnavailable, map[string]string{"error": "database_unavailable"})
	case api != nil:
		api.Routes().ServeHTTP(response, request)
	default:
		h.static.ServeHTTP(response, request)
	}
}

func (h *RuntimeHost) current() *API {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.api
}

func (h *RuntimeHost) handleRuntimeStatus(response http.ResponseWriter, _ *http.Request, api *API) {
	h.mu.RLock()
	status := h.status
	status.Agents = append([]AgentStatus(nil), h.status.Agents...)
	h.mu.RUnlock()
	if api != nil && api.pool != nil {
		status.Agents = api.pool.Status()
	}
	writeJSON(response, http.StatusOK, status)
}

func (h *RuntimeHost) handleRestartHealth(response http.ResponseWriter, _ *http.Request) {
	response.Header().Set("Access-Control-Allow-Origin", "*")
	h.mu.RLock()
	restart := h.restart
	h.mu.RUnlock()
	writeJSON(response, http.StatusOK, struct {
		OK           bool   `json:"ok"`
		RestartToken string `json:"restart_token"`
		Restarting   bool   `json:"restarting"`
	}{
		OK:           true,
		RestartToken: h.restartToken,
		Restarting:   restart != nil && restart.Pending(),
	})
}

func (h *RuntimeHost) handleOfflineAgents(response http.ResponseWriter, _ *http.Request) {
	h.mu.RLock()
	agents := append([]AgentStatus(nil), h.status.Agents...)
	h.mu.RUnlock()
	writeJSON(response, http.StatusOK, agents)
}
