package gui

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"dedup/internal/config"
)

func TestRuntimeHostWaitsForAdmittedRequestBeforeHTTPDrainCompletes(t *testing.T) {
	host := NewRuntimeHost(&fakeGUIConfigStore{}, nil)
	started := make(chan struct{})
	release := make(chan struct{})
	host.static = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	})
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		host.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not enter RuntimeHost")
	}

	host.BeginHTTPShutdown()
	drainDone := make(chan struct{})
	go func() {
		host.WaitForHTTP()
		close(drainDone)
	}()
	select {
	case <-drainDone:
		t.Fatal("HTTP drain completed while an admitted request was active")
	case <-time.After(50 * time.Millisecond):
	}

	rejected := httptest.NewRecorder()
	host.ServeHTTP(rejected, httptest.NewRequest(http.MethodGet, "/", nil))
	if rejected.Code != http.StatusServiceUnavailable {
		t.Fatalf("request admitted after HTTP shutdown: status=%d", rejected.Code)
	}
	close(release)
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("admitted request did not finish")
	}
	select {
	case <-drainDone:
	case <-time.After(time.Second):
		t.Fatal("HTTP drain did not complete after admitted request finished")
	}
}

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
	host := NewRuntimeHost(
		&fakeGUIConfigStore{restart: restart},
		[]config.AgentEndpoint{{Addr: "127.0.0.1:9101"}},
		"replacement-instance-token",
	)
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
	if got := pending.Body.String(); got != "{\"ok\":true,\"restart_token\":\"replacement-instance-token\",\"restarting\":true}\n" {
		t.Fatalf("pending health=%s", got)
	}
	restart.pending = false
	ready := assertRuntimeHostStatus(t, host, http.MethodGet, "/api/restart/health", http.StatusOK)
	if got := ready.Body.String(); got != "{\"ok\":true,\"restart_token\":\"replacement-instance-token\",\"restarting\":false}\n" {
		t.Fatalf("ready health=%s", got)
	}
}

func TestRuntimeHostReportsLiveAgentStatusFromInstalledPool(t *testing.T) {
	const address = "127.0.0.1:9101"
	const machineID = "node-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pool := &Pool{byAddr: map[string]*AgentConn{
		address: {
			ep: config.AgentEndpoint{Addr: address}, machineID: machineID,
			online: true, identityState: IdentityClaimed,
		},
	}}
	host := NewRuntimeHost(&fakeGUIConfigStore{}, []config.AgentEndpoint{{Addr: address}})
	host.Install(NewAPI(pool, NewTaskRegistry(nil, testLogger()), nil))

	response := assertRuntimeHostStatus(t, host, http.MethodGet, "/api/runtime/status", http.StatusOK)
	var status RuntimeStatus
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Agents) != 1 || status.Agents[0].MachineID != machineID ||
		!status.Agents[0].Online || status.Agents[0].IdentityState != IdentityClaimed {
		t.Fatalf("runtime agents=%#v", status.Agents)
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
