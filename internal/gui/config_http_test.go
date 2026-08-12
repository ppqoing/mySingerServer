package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"dedup/internal/config"
)

type fakeGUIConfigStore struct {
	loadSnapshot GUIConfigSnapshot
	loadErr      error
	saveResult   GUIConfigSaveResult
	saveErr      error
	saved        *config.GUIConfig
	saveCalls    int
	restart      guiRestartCoordinator
	events       *[]string
}

func (s *fakeGUIConfigStore) Load() (GUIConfigSnapshot, error) {
	return s.loadSnapshot, s.loadErr
}

func (s *fakeGUIConfigStore) Save(_ context.Context, cfg *config.GUIConfig) (GUIConfigSaveResult, error) {
	s.saveCalls++
	s.saved = cfg
	if s.events != nil {
		*s.events = append(*s.events, "save")
	}
	return s.saveResult, s.saveErr
}

func (s *fakeGUIConfigStore) RestartCoordinator() guiRestartCoordinator {
	return s.restart
}

type fakeGUIRestartCoordinator struct {
	pending     bool
	prepareErr  error
	recoveryURL string
	prepared    *config.GUIConfig
	commits     int
	events      *[]string
}

func (c *fakeGUIRestartCoordinator) Pending() bool {
	return c.pending
}

func (c *fakeGUIRestartCoordinator) Prepare(cfg *config.GUIConfig) (string, error) {
	c.prepared = cfg
	if c.events != nil {
		*c.events = append(*c.events, "prepare")
	}
	return c.recoveryURL, c.prepareErr
}

func (c *fakeGUIRestartCoordinator) Commit() {
	c.commits++
	if c.events != nil {
		*c.events = append(*c.events, "commit")
	}
}

type recordingRestartResponseWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
	wrote  bool
	events *[]string
}

func (w *recordingRestartResponseWriter) Header() http.Header {
	return w.header
}

func (w *recordingRestartResponseWriter) WriteHeader(status int) {
	w.status = status
}

func (w *recordingRestartResponseWriter) Write(data []byte) (int, error) {
	if !w.wrote {
		w.wrote = true
		*w.events = append(*w.events, "write")
	}
	return w.body.Write(data)
}

func (w *recordingRestartResponseWriter) Flush() {
	*w.events = append(*w.events, "flush")
}

func serveGUIConfigRequest(t *testing.T, api *API, method, target string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, bytes.NewReader(body))
	response := httptest.NewRecorder()
	api.Routes().ServeHTTP(response, request)
	return response
}

func TestGUIConfigHTTPGetReturnsDiskSnapshot(t *testing.T) {
	cfg := testGUIConfig()
	store := &fakeGUIConfigStore{loadSnapshot: GUIConfigSnapshot{
		Config:          cfg,
		RestartRequired: true,
	}}
	api := NewAPI(nil, nil, nil)
	api.SetConfigService(store)

	response := serveGUIConfigRequest(t, api, http.MethodGet, "/api/config", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var got GUIConfigSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.RestartRequired || got.Config.PGDSN != cfg.PGDSN || len(got.Config.Agents) != 1 {
		t.Fatalf("snapshot=%#v", got)
	}
}

func TestGUIConfigHTTPPutSavesCompleteConfig(t *testing.T) {
	cfg := testGUIConfig()
	cfg.Agents = append(cfg.Agents, config.AgentEndpoint{
		Addr: "192.168.1.11:9101",
	})
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeGUIConfigStore{saveResult: GUIConfigSaveResult{
		Saved:           true,
		RestartRequired: true,
	}}
	api := NewAPI(nil, nil, nil)
	api.SetConfigService(store)

	response := serveGUIConfigRequest(t, api, http.MethodPut, "/api/config", body)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if store.saveCalls != 1 || store.saved == nil || len(store.saved.Agents) != 2 {
		t.Fatalf("saveCalls=%d saved=%#v", store.saveCalls, store.saved)
	}
	var result GUIConfigSaveResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Saved || !result.RestartRequired {
		t.Fatalf("result=%#v", result)
	}
}

func TestGUIConfigHTTPPutRestartWritesAndFlushesResponseBeforeCommit(t *testing.T) {
	cfg := testGUIConfig()
	cfg.ListenAddr = "127.0.0.1:18081"
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	events := []string{}
	restart := &fakeGUIRestartCoordinator{
		recoveryURL: "http://127.0.0.1:18081/api/restart/health",
		events:      &events,
	}
	store := &fakeGUIConfigStore{
		saveResult: GUIConfigSaveResult{Saved: true, RestartRequired: true},
		restart:    restart,
		events:     &events,
	}
	api := NewAPI(nil, nil, nil)
	api.SetConfigService(store)
	response := &recordingRestartResponseWriter{header: make(http.Header), events: &events}
	request := httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(body))

	api.Routes().ServeHTTP(response, request)

	wantEvents := []string{"save", "prepare", "write", "flush", "commit"}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("events=%v want=%v", events, wantEvents)
	}
	if response.status != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.status, response.body.String())
	}
	var result GUIConfigSaveResult
	if err := json.Unmarshal(response.body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Saved || !result.RestartRequired || !result.Restarting || result.RecoveryURL != restart.recoveryURL {
		t.Fatalf("result=%#v", result)
	}
	if restart.prepared == nil || restart.prepared.ListenAddr != cfg.ListenAddr || restart.commits != 1 {
		t.Fatalf("restart=%#v", restart)
	}
}

func TestGUIConfigHTTPPutRestartInProgressReturnsConflict(t *testing.T) {
	body, err := json.Marshal(testGUIConfig())
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeGUIConfigStore{restart: &fakeGUIRestartCoordinator{pending: true}}
	api := NewAPI(nil, nil, nil)
	api.SetConfigService(store)

	response := serveGUIConfigRequest(t, api, http.MethodPut, "/api/config", body)

	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "restart_in_progress") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if store.saveCalls != 0 {
		t.Fatalf("restart conflict saved config %d times", store.saveCalls)
	}
}

func TestGUIConfigHTTPPutRestartLaunchFailureKeepsSavedResult(t *testing.T) {
	cfg := testGUIConfig()
	cfg.ListenAddr = "127.0.0.1:18081"
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	restart := &fakeGUIRestartCoordinator{prepareErr: errors.New("CreateProcess failed")}
	store := &fakeGUIConfigStore{
		saveResult: GUIConfigSaveResult{Saved: true, RestartRequired: true},
		restart:    restart,
	}
	api := NewAPI(nil, nil, nil)
	api.SetConfigService(store)

	response := serveGUIConfigRequest(t, api, http.MethodPut, "/api/config", body)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var got struct {
		GUIConfigSaveResult
		Error string `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Error != "restart_launch_failed" || !got.Saved || !got.RestartRequired || got.Restarting {
		t.Fatalf("response=%#v", got)
	}
	if store.saveCalls != 1 || restart.commits != 0 {
		t.Fatalf("saveCalls=%d restart=%#v", store.saveCalls, restart)
	}
}

func TestGUIConfigHTTPPutReturnsFieldErrors(t *testing.T) {
	cfg := testGUIConfig()
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeGUIConfigStore{saveErr: &config.GUIValidationError{Fields: []config.FieldError{{
		Field:   "agents[1].addr",
		Code:    "duplicate",
		Message: "Agent 地址不能重复",
	}}}}
	api := NewAPI(nil, nil, nil)
	api.SetConfigService(store)

	response := serveGUIConfigRequest(t, api, http.MethodPut, "/api/config", body)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var got struct {
		Error  string              `json:"error"`
		Fields []config.FieldError `json:"fields"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Error != "config_invalid" || len(got.Fields) != 1 || got.Fields[0].Field != "agents[1].addr" {
		t.Fatalf("response=%#v", got)
	}
}

func TestGUIConfigHTTPPutRejectsUnknownFieldsAndTrailingJSON(t *testing.T) {
	store := &fakeGUIConfigStore{}
	api := NewAPI(nil, nil, nil)
	api.SetConfigService(store)
	valid, err := json.Marshal(testGUIConfig())
	if err != nil {
		t.Fatal(err)
	}
	bodies := [][]byte{
		bytes.Replace(valid, []byte("\"heartbeat_s\":"), []byte("\"unknown\":true,\"heartbeat_s\":"), 1),
		bytes.Replace(valid, []byte("\"addr\":"), []byte("\"machine_id\":\"legacy\",\"addr\":"), 1),
		append(append([]byte{}, valid...), []byte(" {}")...),
	}
	for _, body := range bodies {
		response := serveGUIConfigRequest(t, api, http.MethodPut, "/api/config", body)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_request") {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
	if store.saveCalls != 0 {
		t.Fatalf("invalid requests reached store %d times", store.saveCalls)
	}
}

func TestGUIConfigHTTPReturnsStableReadAndWriteErrorsWithoutDSN(t *testing.T) {
	const secret = "postgres://user:secret-password@127.0.0.1/dedup"
	for _, test := range []struct {
		name       string
		method     string
		body       []byte
		store      *fakeGUIConfigStore
		wantStatus int
		wantCode   string
	}{
		{
			name:       "read",
			method:     http.MethodGet,
			store:      &fakeGUIConfigStore{loadErr: errors.New("cannot read " + secret)},
			wantStatus: http.StatusInternalServerError,
			wantCode:   "config_read_failed",
		},
		{
			name:   "write",
			method: http.MethodPut,
			body: func() []byte {
				cfg := testGUIConfig()
				cfg.PGDSN = secret
				data, marshalErr := json.Marshal(cfg)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				return data
			}(),
			store:      &fakeGUIConfigStore{saveErr: errors.New("cannot write " + secret)},
			wantStatus: http.StatusInternalServerError,
			wantCode:   "config_save_failed",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := NewAPI(nil, nil, nil)
			api.SetConfigService(test.store)
			response := serveGUIConfigRequest(t, api, test.method, "/api/config", test.body)
			if response.Code != test.wantStatus || !strings.Contains(response.Body.String(), test.wantCode) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), secret) || strings.Contains(response.Body.String(), "secret-password") {
				t.Fatalf("response leaked DSN: %s", response.Body.String())
			}
		})
	}
}

func TestGUIConfigHTTPUnavailableWithoutInjectedService(t *testing.T) {
	api := NewAPI(nil, nil, nil)
	response := serveGUIConfigRequest(t, api, http.MethodGet, "/api/config", nil)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "config_unavailable") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
