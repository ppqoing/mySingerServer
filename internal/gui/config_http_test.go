package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
}

func (s *fakeGUIConfigStore) Load() (GUIConfigSnapshot, error) {
	return s.loadSnapshot, s.loadErr
}

func (s *fakeGUIConfigStore) Save(_ context.Context, cfg *config.GUIConfig) (GUIConfigSaveResult, error) {
	s.saveCalls++
	s.saved = cfg
	return s.saveResult, s.saveErr
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
