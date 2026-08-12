package gui

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"

	"dedup/internal/config"
)

func TestRuntimeHostServesConfigurationWhileDatabaseIsUnavailable(t *testing.T) {
	host := NewRuntimeHost(&fakeGUIConfigStore{loadSnapshot: GUIConfigSnapshot{
		Config: testGUIConfig(),
	}}, []config.AgentEndpoint{{Addr: "127.0.0.1:9101"}})
	host.SetDatabaseFailure(&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED})

	assertRuntimeHostStatus(t, host, http.MethodGet, "/", http.StatusOK)
	assertRuntimeHostStatus(t, host, http.MethodGet, "/api/config", http.StatusOK)
	assertRuntimeHostStatus(t, host, http.MethodGet, "/api/runtime/status", http.StatusOK)

	tasks := assertRuntimeHostStatus(t, host, http.MethodGet, "/api/tasks", http.StatusServiceUnavailable)
	if got := tasks.Body.String(); got != "{\"error\":\"database_unavailable\"}\n" {
		t.Fatalf("503 body=%q", got)
	}

	agents := assertRuntimeHostStatus(t, host, http.MethodGet, "/api/agents", http.StatusOK)
	var snapshot []AgentStatus
	if err := json.Unmarshal(agents.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot) != 1 || snapshot[0].Addr != "127.0.0.1:9101" || snapshot[0].Online || snapshot[0].IdentityState != IdentityPending {
		t.Fatalf("offline agents=%#v", snapshot)
	}

	health := assertRuntimeHostStatus(t, host, http.MethodGet, "/api/restart/health", http.StatusOK)
	if got := health.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("health CORS=%q", got)
	}
}

func TestRuntimeHostUsesFrontendRuntimeWireStatesAndRestartHealth(t *testing.T) {
	restart := &fakeGUIRestartCoordinator{}
	host := NewRuntimeHost(&fakeGUIConfigStore{restart: restart}, []config.AgentEndpoint{{Addr: "127.0.0.1:9101"}})
	host.Install(NewAPI(nil, NewTaskRegistry(nil, testLogger()), nil))
	connected := assertRuntimeHostStatus(t, host, http.MethodGet, "/api/runtime/status", http.StatusOK)
	if !strings.Contains(connected.Body.String(), "\"database_state\":\"connected\"") || !strings.Contains(connected.Body.String(), "\"identity_state\":\"pending\"") {
		t.Fatalf("connected status=%s", connected.Body.String())
	}
	host.SetDatabaseFailure(&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED})
	failed := assertRuntimeHostStatus(t, host, http.MethodGet, "/api/runtime/status", http.StatusOK)
	if !strings.Contains(failed.Body.String(), "\"database_state\":\"error\"") {
		t.Fatalf("failed status=%s", failed.Body.String())
	}
	restart.pending = true
	pending := assertRuntimeHostStatus(t, host, http.MethodGet, "/api/restart/health", http.StatusOK)
	if got := pending.Body.String(); got != "{\"ok\":true,\"restarting\":true}\n" {
		t.Fatalf("pending health=%s", got)
	}
	restart.pending = false
	ready := assertRuntimeHostStatus(t, host, http.MethodGet, "/api/restart/health", http.StatusOK)
	if got := ready.Body.String(); got != "{\"ok\":true,\"restarting\":false}\n" {
		t.Fatalf("ready health=%s", got)
	}
}

func TestRuntimeHostInstallsAndSafelyStopsCompleteRuntime(t *testing.T) {
	host := NewRuntimeHost(&fakeGUIConfigStore{}, nil)
	host.BeginAnalysisShutdown()
	host.WaitForAnalysis()

	api := NewAPI(nil, NewTaskRegistry(nil, testLogger()), nil)
	host.Install(api)
	assertRuntimeHostStatus(t, host, http.MethodGet, "/api/tasks", http.StatusOK)
	host.BeginAnalysisShutdown()
	host.WaitForAnalysis()
}

func TestRuntimeHostUsesFixedDelegationSnapshotForSingleRequest(t *testing.T) {
	host := NewRuntimeHost(&fakeGUIConfigStore{}, nil)
	host.Install(NewAPI(nil, NewTaskRegistry(nil, testLogger()), nil))
	host.afterRuntimeSnapshot = func() {
		host.Install(nil)
	}

	assertRuntimeHostStatus(t, host, http.MethodGet, "/api/tasks", http.StatusOK)
	if current := host.current(); current != nil {
		t.Fatalf("runtime after replacement=%#v, want nil", current)
	}
}

func TestRuntimeHostReportsRestartState(t *testing.T) {
	host := NewRuntimeHost(&fakeGUIConfigStore{}, nil)
	host.SetDatabaseConnecting()
	host.SetRestartState(true, "http://127.0.0.1:8080/api/restart/health")

	response := assertRuntimeHostStatus(t, host, http.MethodGet, "/api/runtime/status", http.StatusOK)
	if !strings.Contains(response.Body.String(), "\"database_state\":\"connecting\"") ||
		!strings.Contains(response.Body.String(), "\"restarting\":true") ||
		!strings.Contains(response.Body.String(), "\"recovery_url\":\"http://127.0.0.1:8080/api/restart/health\"") {
		t.Fatalf("status=%s", response.Body.String())
	}
}

func assertRuntimeHostStatus(t *testing.T, host http.Handler, method, target string, want int) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	host.ServeHTTP(response, httptest.NewRequest(method, target, nil))
	if response.Code != want {
		t.Fatalf("%s %s status=%d want=%d body=%s", method, target, response.Code, want, response.Body.String())
	}
	return response
}
