package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
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
	aborts      int
	onPrepare   func()
	events      *[]string
}

func (c *fakeGUIRestartCoordinator) Pending() bool {
	return c.pending
}

func (c *fakeGUIRestartCoordinator) Prepare(cfg *config.GUIConfig) (string, error) {
	c.prepared = cfg
	if c.onPrepare != nil {
		c.onPrepare()
	}
	if c.events != nil {
		*c.events = append(*c.events, "prepare")
	}
	if c.prepareErr == nil {
		c.pending = true
	}
	return c.recoveryURL, c.prepareErr
}

func (c *fakeGUIRestartCoordinator) Commit() {
	c.commits++
	if c.events != nil {
		*c.events = append(*c.events, "commit")
	}
}

func (c *fakeGUIRestartCoordinator) Abort() {
	c.aborts++
	c.pending = false
	if c.events != nil {
		*c.events = append(*c.events, "abort")
	}
}

func (c *fakeGUIRestartCoordinator) Begin() bool {
	return !c.pending
}

func (c *fakeGUIRestartCoordinator) End() {}

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

type failingRestartResponseWriter struct {
	header   http.Header
	status   int
	body     bytes.Buffer
	writeErr error
	flushErr error
}

func (w *failingRestartResponseWriter) Header() http.Header {
	return w.header
}

func (w *failingRestartResponseWriter) WriteHeader(status int) {
	w.status = status
}

func (w *failingRestartResponseWriter) Write(data []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return w.body.Write(data)
}

func (w *failingRestartResponseWriter) FlushError() error {
	return w.flushErr
}

type concurrentRestartCoordinator struct {
	requestMu sync.Mutex
	pending   atomic.Bool
	attempted chan struct{}
	release   chan struct{}
	mu        sync.Mutex
	prepared  *config.GUIConfig
}

func (c *concurrentRestartCoordinator) Pending() bool {
	c.attempted <- struct{}{}
	<-c.release
	return c.pending.Load()
}

func (c *concurrentRestartCoordinator) Begin() bool {
	c.attempted <- struct{}{}
	<-c.release
	c.requestMu.Lock()
	if c.pending.Load() {
		c.requestMu.Unlock()
		return false
	}
	return true
}

func (c *concurrentRestartCoordinator) End() {
	c.requestMu.Unlock()
}

func (c *concurrentRestartCoordinator) Prepare(cfg *config.GUIConfig) (string, error) {
	if !c.pending.CompareAndSwap(false, true) {
		return "", errors.New("restart already prepared")
	}
	copy := *cfg
	c.mu.Lock()
	c.prepared = &copy
	c.mu.Unlock()
	return fmt.Sprintf("http://%s/api/restart/health", cfg.ListenAddr), nil
}

func (c *concurrentRestartCoordinator) Commit() {}

func (c *concurrentRestartCoordinator) Abort() {
	c.pending.Store(false)
}

func (c *concurrentRestartCoordinator) snapshot() *config.GUIConfig {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.prepared
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

func TestGUIConfigHTTPPutConcurrentRestartOnlyWinnerSavesAndOwnsRecovery(t *testing.T) {
	restart := &concurrentRestartCoordinator{
		attempted: make(chan struct{}, 2),
		release:   make(chan struct{}),
	}
	path := filepath.Join(t.TempDir(), "gui.json")
	runtime := testGUIConfig()
	writeTestGUIConfig(t, path, runtime)
	service, err := NewGUIConfigService(path, runtime)
	if err != nil {
		t.Fatal(err)
	}
	service.SetRestartCoordinator(restart)
	originalReplace := service.replace
	var replaceCalls atomic.Int32
	service.replace = func(source, destination string) error {
		replaceCalls.Add(1)
		return originalReplace(source, destination)
	}
	api := NewAPI(nil, nil, nil)
	api.SetConfigService(service)
	routes := api.Routes()

	makeRequest := func(listenAddr string) *httptest.ResponseRecorder {
		cfg := testGUIConfig()
		cfg.ListenAddr = listenAddr
		body, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		routes.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(body)))
		return response
	}
	firstResult := make(chan *httptest.ResponseRecorder, 1)
	secondResult := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstResult <- makeRequest("127.0.0.1:18082") }()
	go func() { secondResult <- makeRequest("127.0.0.1:18083") }()
	<-restart.attempted
	<-restart.attempted
	close(restart.release)

	first := <-firstResult
	second := <-secondResult
	responses := []*httptest.ResponseRecorder{first, second}
	var winner, loser *httptest.ResponseRecorder
	for _, response := range responses {
		switch response.Code {
		case http.StatusOK:
			winner = response
		case http.StatusConflict:
			loser = response
		}
	}
	if winner == nil || loser == nil || !strings.Contains(loser.Body.String(), "restart_in_progress") {
		t.Fatalf("first=%d %s second=%d %s", first.Code, first.Body.String(), second.Code, second.Body.String())
	}
	disk, err := config.LoadGUI(path)
	if err != nil {
		t.Fatal(err)
	}
	if replaceCalls.Load() != 1 || (disk.ListenAddr != "127.0.0.1:18082" && disk.ListenAddr != "127.0.0.1:18083") {
		t.Fatalf("replaceCalls=%d disk=%#v", replaceCalls.Load(), disk)
	}
	prepared := restart.snapshot()
	if prepared == nil || prepared.ListenAddr != disk.ListenAddr {
		t.Fatalf("disk=%#v prepared=%#v", disk, prepared)
	}
	var result GUIConfigSaveResult
	if err := json.Unmarshal(winner.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	wantRecoveryURL := fmt.Sprintf("http://%s/api/restart/health", disk.ListenAddr)
	if result.RecoveryURL != wantRecoveryURL {
		t.Fatalf("recovery URL=%q", result.RecoveryURL)
	}
}

func TestGUIConfigHTTPPutRestartResponseFailureAbortsWithoutCommit(t *testing.T) {
	for _, test := range []struct {
		name     string
		writeErr error
		flushErr error
	}{
		{name: "write", writeErr: errors.New("client disconnected")},
		{name: "flush", flushErr: errors.New("connection reset")},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := testGUIConfig()
			cfg.ListenAddr = "127.0.0.1:18081"
			body, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			restart := &fakeGUIRestartCoordinator{recoveryURL: "http://127.0.0.1:18081/api/restart/health"}
			store := &fakeGUIConfigStore{
				saveResult: GUIConfigSaveResult{Saved: true, RestartRequired: true},
				restart:    restart,
			}
			api := NewAPI(nil, nil, nil)
			api.SetConfigService(store)
			response := &failingRestartResponseWriter{
				header:   make(http.Header),
				writeErr: test.writeErr,
				flushErr: test.flushErr,
			}

			api.Routes().ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(body)))

			if restart.commits != 0 || restart.aborts != 1 || restart.Pending() {
				t.Fatalf("restart=%#v", restart)
			}
		})
	}
}

func TestGUIConfigHTTPPutRestartCanceledContextAbortsWithoutCommit(t *testing.T) {
	cfg := testGUIConfig()
	cfg.ListenAddr = "127.0.0.1:18081"
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	restart := &fakeGUIRestartCoordinator{
		recoveryURL: "http://127.0.0.1:18081/api/restart/health",
		onPrepare:   cancel,
	}
	store := &fakeGUIConfigStore{
		saveResult: GUIConfigSaveResult{Saved: true, RestartRequired: true},
		restart:    restart,
	}
	api := NewAPI(nil, nil, nil)
	api.SetConfigService(store)
	request := httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(body)).WithContext(ctx)
	response := httptest.NewRecorder()

	api.Routes().ServeHTTP(response, request)

	if restart.commits != 0 || restart.aborts != 1 || restart.Pending() {
		t.Fatalf("restart=%#v", restart)
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
