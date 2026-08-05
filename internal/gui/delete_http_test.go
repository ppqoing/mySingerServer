package gui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"dedup/internal/proto"
)

type deleteHTTPStub struct {
	prepareFn func(context.Context, []int64) (DeleteSummary, string, error)
	executeFn func(context.Context, string, string) (string, error)
	statusFn  func(string) (DeleteTaskStatus, bool)

	prepareCalls int
	executeCalls int
	statusCalls  int
	gotIDs       []int64
	gotToken     string
	gotMode      string
}

func (stub *deleteHTTPStub) Prepare(
	ctx context.Context,
	ids []int64,
) (DeleteSummary, string, error) {
	stub.prepareCalls++
	stub.gotIDs = append([]int64(nil), ids...)
	if stub.prepareFn == nil {
		return DeleteSummary{}, "", errors.New("unexpected Prepare call")
	}
	return stub.prepareFn(ctx, ids)
}

func (stub *deleteHTTPStub) Execute(
	ctx context.Context,
	token string,
	mode string,
) (string, error) {
	stub.executeCalls++
	stub.gotToken = token
	stub.gotMode = mode
	if stub.executeFn == nil {
		return "", errors.New("unexpected Execute call")
	}
	return stub.executeFn(ctx, token, mode)
}

func (stub *deleteHTTPStub) Status(taskID string) (DeleteTaskStatus, bool) {
	stub.statusCalls++
	if stub.statusFn == nil {
		return DeleteTaskStatus{}, false
	}
	return stub.statusFn(taskID)
}

func deleteHTTPResponse(
	api *API,
	method string,
	target string,
	contentType string,
	body string,
) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response := httptest.NewRecorder()
	api.Routes().ServeHTTP(response, request)
	return response
}

func assertDeleteJSONResponse(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	if got := response.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type=%q body=%s", got, response.Body.String())
	}
	var value any
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatalf("body is not JSON: %v body=%s", err, response.Body.String())
	}
}

func TestDeleteHTTPRoutesAndMethods(t *testing.T) {
	api := NewAPI(nil, nil, nil)
	const taskID = "b7b0ba1c-1ec1-4be4-b769-cbe40607fe25"
	tests := []struct {
		name        string
		method      string
		target      string
		contentType string
		body        string
		want        int
	}{
		{"prepare registered without service", http.MethodPost, "/api/delete/prepare", "application/json", `{"member_ids":[1]}`, http.StatusServiceUnavailable},
		{"execute registered without service", http.MethodPost, "/api/delete/execute", "application/json", `{"confirm_token":"abcdefghijklmnopqrstuv"}`, http.StatusServiceUnavailable},
		{"status registered without service", http.MethodGet, "/api/delete/tasks/" + taskID, "", "", http.StatusServiceUnavailable},
		{"prepare wrong method", http.MethodGet, "/api/delete/prepare", "", "", http.StatusMethodNotAllowed},
		{"execute wrong method", http.MethodGet, "/api/delete/execute", "", "", http.StatusMethodNotAllowed},
		{"status wrong method", http.MethodPost, "/api/delete/tasks/" + taskID, "", "", http.StatusMethodNotAllowed},
		{"unknown route", http.MethodGet, "/api/delete/nope", "", "", http.StatusNotFound},
		{"status trailing route", http.MethodGet, "/api/delete/tasks/" + taskID + "/extra", "", "", http.StatusNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := deleteHTTPResponse(
				api,
				test.method,
				test.target,
				test.contentType,
				test.body,
			)
			if response.Code != test.want {
				t.Fatalf("status=%d body=%s, want %d", response.Code, response.Body.String(), test.want)
			}
			if test.want == http.StatusServiceUnavailable {
				assertDeleteJSONResponse(t, response)
			}
		})
	}
}

func TestDeleteHTTPNamespaceDistinguishesUnknownPathsFromWrongMethods(t *testing.T) {
	const taskID = "b7b0ba1c-1ec1-4be4-b769-cbe40607fe25"
	api := NewAPI(nil, nil, nil)

	for _, test := range []struct {
		name   string
		method string
		target string
	}{
		{"prepare trailing slash", http.MethodPost, "/api/delete/prepare/"},
		{"prepare trailing segment", http.MethodPost, "/api/delete/prepare/extra"},
		{"execute trailing slash", http.MethodPost, "/api/delete/execute/"},
		{"execute trailing segment", http.MethodPost, "/api/delete/execute/extra"},
		{"status trailing slash", http.MethodPost, "/api/delete/tasks/" + taskID + "/"},
		{"status trailing segment", http.MethodPost, "/api/delete/tasks/" + taskID + "/extra"},
		{"unknown GET", http.MethodGet, "/api/delete/nope"},
		{"unknown POST", http.MethodPost, "/api/delete/nope"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := deleteHTTPResponse(api, test.method, test.target, "", "")
			if response.Code != http.StatusNotFound {
				t.Fatalf(
					"status=%d Allow=%q body=%s, want 404",
					response.Code,
					response.Header().Get("Allow"),
					response.Body.String(),
				)
			}
			if got := response.Header().Get("Allow"); got != "" {
				t.Fatalf("Allow=%q on unknown route", got)
			}
		})
	}

	for _, test := range []struct {
		name      string
		method    string
		target    string
		wantAllow string
	}{
		{"GET prepare", http.MethodGet, "/api/delete/prepare", http.MethodPost},
		{"PUT prepare", http.MethodPut, "/api/delete/prepare", http.MethodPost},
		{"DELETE execute", http.MethodDelete, "/api/delete/execute", http.MethodPost},
		{"POST status", http.MethodPost, "/api/delete/tasks/" + taskID, "GET, HEAD"},
		{"PUT status", http.MethodPut, "/api/delete/tasks/" + taskID, "GET, HEAD"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := deleteHTTPResponse(api, test.method, test.target, "", "")
			if response.Code != http.StatusMethodNotAllowed {
				t.Fatalf(
					"status=%d Allow=%q body=%s, want 405",
					response.Code,
					response.Header().Get("Allow"),
					response.Body.String(),
				)
			}
			if got := response.Header().Get("Allow"); got != test.wantAllow {
				t.Fatalf("Allow=%q, want %q", got, test.wantAllow)
			}
		})
	}
}

func TestDeleteHTTPPostRoutesRequireJSONMediaTypeAndStrictSingleValue(t *testing.T) {
	tests := []struct {
		name        string
		target      string
		contentType string
		body        string
		want        int
	}{
		{"prepare missing media type", "/api/delete/prepare", "", `{"member_ids":[1]}`, http.StatusUnsupportedMediaType},
		{"prepare other media type", "/api/delete/prepare", "text/plain", `{"member_ids":[1]}`, http.StatusUnsupportedMediaType},
		{"prepare malformed media type", "/api/delete/prepare", "application/json; charset", `{"member_ids":[1]}`, http.StatusUnsupportedMediaType},
		{"prepare JSON parameters accepted", "/api/delete/prepare", "application/json; charset=UTF-8", `{"member_ids":[1]}`, http.StatusServiceUnavailable},
		{"execute missing media type", "/api/delete/execute", "", `{"confirm_token":"abcdefghijklmnopqrstuv"}`, http.StatusUnsupportedMediaType},
		{"execute other media type", "/api/delete/execute", "application/problem+json", `{"confirm_token":"abcdefghijklmnopqrstuv"}`, http.StatusUnsupportedMediaType},
		{"execute JSON parameters accepted", "/api/delete/execute", "application/json; charset=utf-8", `{"confirm_token":"abcdefghijklmnopqrstuv"}`, http.StatusServiceUnavailable},
		{"prepare empty body", "/api/delete/prepare", "application/json", "", http.StatusBadRequest},
		{"prepare malformed JSON", "/api/delete/prepare", "application/json", `{`, http.StatusBadRequest},
		{"prepare unknown field", "/api/delete/prepare", "application/json", `{"member_ids":[1],"secret":"raw-db-secret"}`, http.StatusBadRequest},
		{"prepare trailing JSON", "/api/delete/prepare", "application/json", `{"member_ids":[1]} {"member_ids":[2]}`, http.StatusBadRequest},
		{"execute empty body", "/api/delete/execute", "application/json", "", http.StatusBadRequest},
		{"execute malformed JSON", "/api/delete/execute", "application/json", `{`, http.StatusBadRequest},
		{"execute unknown field", "/api/delete/execute", "application/json", `{"confirm_token":"abcdefghijklmnopqrstuv","dsn":"postgres-secret"}`, http.StatusBadRequest},
		{"execute trailing JSON", "/api/delete/execute", "application/json", `{"confirm_token":"abcdefghijklmnopqrstuv"} true`, http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := deleteHTTPResponse(
				NewAPI(nil, nil, nil),
				http.MethodPost,
				test.target,
				test.contentType,
				test.body,
			)
			if response.Code != test.want {
				t.Fatalf("status=%d body=%s, want %d", response.Code, response.Body.String(), test.want)
			}
			assertDeleteJSONResponse(t, response)
			if strings.Contains(response.Body.String(), "raw-db-secret") ||
				strings.Contains(response.Body.String(), "postgres-secret") {
				t.Fatalf("response leaked request marker: %s", response.Body.String())
			}
		})
	}
}

func TestDeleteHTTPPostRoutesEnforceExactOneMiBBodyCap(t *testing.T) {
	tests := []struct {
		name   string
		target string
		prefix string
	}{
		{"prepare", "/api/delete/prepare", `{"member_ids":[1]}`},
		{"execute", "/api/delete/execute", `{"confirm_token":"abcdefghijklmnopqrstuv"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			exactBody := test.prefix + strings.Repeat(" ", int(maxDeleteRequestBytes)-len(test.prefix))
			exact := deleteHTTPResponse(
				NewAPI(nil, nil, nil),
				http.MethodPost,
				test.target,
				"application/json",
				exactBody,
			)
			if exact.Code != http.StatusServiceUnavailable {
				t.Fatalf("exact cap status=%d body=%s", exact.Code, exact.Body.String())
			}

			overflow := deleteHTTPResponse(
				NewAPI(nil, nil, nil),
				http.MethodPost,
				test.target,
				"application/json",
				exactBody+" ",
			)
			if overflow.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("overflow status=%d body=%s", overflow.Code, overflow.Body.String())
			}
			assertDeleteJSONResponse(t, overflow)
		})
	}
}

func TestDeleteHTTPPrepareValidatesSelectionBeforeService(t *testing.T) {
	oversized := make([]int64, 10001)
	for index := range oversized {
		oversized[index] = int64(index + 1)
	}
	oversizedJSON, err := json.Marshal(map[string]any{"member_ids": oversized})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		body string
	}{
		{"empty", `{"member_ids":[]}`},
		{"duplicate", `{"member_ids":[1,1]}`},
		{"zero", `{"member_ids":[0]}`},
		{"negative", `{"member_ids":[-1]}`},
		{"ten thousand and one", string(oversizedJSON)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stub := &deleteHTTPStub{}
			api := NewAPI(nil, nil, nil)
			api.delete = stub
			response := deleteHTTPResponse(
				api,
				http.MethodPost,
				"/api/delete/prepare",
				"application/json",
				test.body,
			)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if stub.prepareCalls != 0 {
				t.Fatalf("Prepare called %d times", stub.prepareCalls)
			}
		})
	}
}

func TestDeleteHTTPPrepareMapsSafeErrorsAndReturnsSixtySecondSummary(t *testing.T) {
	const secret = "postgres://user:raw-db-secret@example/db"
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"selection conflict", ErrDeleteSelection, http.StatusConflict},
		{"unavailable", ErrDeleteUnavailable, http.StatusServiceUnavailable},
		{"wrapped unavailable", errors.Join(ErrDeleteUnavailable, errors.New(secret)), http.StatusServiceUnavailable},
		{"unexpected", errors.New(secret), http.StatusInternalServerError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stub := &deleteHTTPStub{
				prepareFn: func(context.Context, []int64) (DeleteSummary, string, error) {
					return DeleteSummary{}, "", test.err
				},
			}
			api := NewAPI(nil, nil, nil)
			api.delete = stub
			response := deleteHTTPResponse(
				api,
				http.MethodPost,
				"/api/delete/prepare",
				"application/json",
				`{"member_ids":[3,1,2]}`,
			)
			if response.Code != test.want {
				t.Fatalf("status=%d body=%s, want %d", response.Code, response.Body.String(), test.want)
			}
			assertDeleteJSONResponse(t, response)
			if strings.Contains(response.Body.String(), secret) {
				t.Fatalf("response leaked service error: %s", response.Body.String())
			}
			if !reflect.DeepEqual(stub.gotIDs, []int64{3, 1, 2}) {
				t.Fatalf("Prepare IDs=%v", stub.gotIDs)
			}
		})
	}

	wantSummary := DeleteSummary{
		TotalFiles: 2,
		TotalBytes: 30,
		ByMachine:  map[string]int64{"machine-a": 2},
		Samples:    []string{`D:\one`, `D:\two`},
	}
	stub := &deleteHTTPStub{
		prepareFn: func(context.Context, []int64) (DeleteSummary, string, error) {
			return wantSummary, "abcdefghijklmnopqrstuv", nil
		},
	}
	api := NewAPI(nil, nil, nil)
	api.delete = stub
	response := deleteHTTPResponse(
		api,
		http.MethodPost,
		"/api/delete/prepare",
		"application/json",
		`{"member_ids":[1,2]}`,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		ConfirmToken     string        `json:"confirm_token"`
		ExpiresInSeconds int           `json:"expires_in_seconds"`
		Summary          DeleteSummary `json:"summary"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ConfirmToken != "abcdefghijklmnopqrstuv" ||
		payload.ExpiresInSeconds != 60 ||
		!reflect.DeepEqual(payload.Summary, wantSummary) {
		t.Fatalf("payload=%#v", payload)
	}
}

func TestDeleteHTTPExecuteValidatesBeforeServiceAndPreservesModeContract(t *testing.T) {
	const token = "abcdefghijklmnopqrstuv"
	success := func(context.Context, string, string) (string, error) {
		return "b7b0ba1c-1ec1-4be4-b769-cbe40607fe25", nil
	}
	tests := []struct {
		name     string
		body     string
		wantMode string
	}{
		{"omitted mode passes empty for soft default", `{"confirm_token":"` + token + `"}`, ""},
		{"empty mode passes empty for soft default", `{"confirm_token":"` + token + `","mode":""}`, ""},
		{"soft mode", `{"confirm_token":"` + token + `","mode":"soft"}`, proto.ModeSoft},
		{"hard mode", `{"confirm_token":"` + token + `","mode":"hard"}`, proto.ModeHard},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stub := &deleteHTTPStub{executeFn: success}
			api := NewAPI(nil, nil, nil)
			api.delete = stub
			response := deleteHTTPResponse(
				api,
				http.MethodPost,
				"/api/delete/execute",
				"application/json",
				test.body,
			)
			if response.Code != http.StatusAccepted {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var payload map[string]string
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(payload, map[string]string{
				"task_id": "b7b0ba1c-1ec1-4be4-b769-cbe40607fe25",
			}) {
				t.Fatalf("payload=%v", payload)
			}
			if stub.gotToken != token || stub.gotMode != test.wantMode {
				t.Fatalf("Execute token=%q mode=%q", stub.gotToken, stub.gotMode)
			}
		})
	}

	for _, test := range []struct {
		name string
		body string
	}{
		{"empty token", `{"confirm_token":""}`},
		{"missing token", `{}`},
		{"bad mode", `{"confirm_token":"` + token + `","mode":"SOFT"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			stub := &deleteHTTPStub{executeFn: success}
			api := NewAPI(nil, nil, nil)
			api.delete = stub
			response := deleteHTTPResponse(
				api,
				http.MethodPost,
				"/api/delete/execute",
				"application/json",
				test.body,
			)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if stub.executeCalls != 0 {
				t.Fatalf("Execute called %d times", stub.executeCalls)
			}
		})
	}
}

func TestDeleteHTTPExecuteMapsConfirmationErrorsWithoutTokenLeak(t *testing.T) {
	const token = "abcdefghijklmnopqrstuv"
	const secret = "raw-db-secret"
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"invalid", ErrConfirmationInvalid, http.StatusBadRequest},
		{"expired", ErrConfirmationExpired, http.StatusBadRequest},
		{"invalid mode", ErrDeleteMode, http.StatusBadRequest},
		{"consumed", ErrConfirmationConsumed, http.StatusConflict},
		{"unavailable", errors.Join(ErrDeleteUnavailable, errors.New(secret)), http.StatusServiceUnavailable},
		{"unexpected", errors.New(secret), http.StatusInternalServerError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stub := &deleteHTTPStub{
				executeFn: func(context.Context, string, string) (string, error) {
					return "", test.err
				},
			}
			api := NewAPI(nil, nil, nil)
			api.delete = stub
			response := deleteHTTPResponse(
				api,
				http.MethodPost,
				"/api/delete/execute",
				"application/json",
				`{"confirm_token":"`+token+`"}`,
			)
			if response.Code != test.want {
				t.Fatalf("status=%d body=%s, want %d", response.Code, response.Body.String(), test.want)
			}
			assertDeleteJSONResponse(t, response)
			if strings.Contains(response.Body.String(), token) ||
				strings.Contains(response.Body.String(), secret) {
				t.Fatalf("response leaked sensitive marker: %s", response.Body.String())
			}
		})
	}
}

func TestDeleteHTTPExecuteReturnsAcceptedForPartialOfflineDispatch(t *testing.T) {
	transport := &deleteTestTransport{
		online: map[string]bool{
			"machine-a": true,
			"machine-b": false,
		},
		onlineCalls: make(map[string]int),
		sendErrors:  make(map[string]error),
	}
	service, token := newDeleteTestService(t, []DeleteMember{
		{FileID: 1, MachineID: "machine-a", Path: `D:\online`, Size: 10},
		{FileID: 2, MachineID: "machine-b", Path: `E:\offline`, Size: 20},
	}, transport)
	api := NewAPI(nil, nil, nil)
	api.SetDeleteService(service)

	response := deleteHTTPResponse(
		api,
		http.MethodPost,
		"/api/delete/execute",
		"application/json",
		`{"confirm_token":"`+token+`"}`,
	)

	if response.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload) != 1 || payload["task_id"] == "" {
		t.Fatalf("payload=%v", payload)
	}
	status, ok := service.Status(payload["task_id"])
	if !ok || status.Complete || status.Pending != 1 ||
		status.Failed != 1 || status.ErrorCodes[proto.DeleteErrHelperLost] != 1 {
		t.Fatalf("partial status=%#v ok=%v", status, ok)
	}
}

func TestDeleteHTTPStatusValidatesCanonicalTaskAndReturnsFullSafeStatus(t *testing.T) {
	const taskID = "b7b0ba1c-1ec1-4be4-b769-cbe40607fe25"
	stub := &deleteHTTPStub{}
	api := NewAPI(nil, nil, nil)
	api.delete = stub
	for _, badID := range []string{
		"",
		"not-a-uuid",
		"B7B0BA1C-1EC1-4BE4-B769-CBE40607FE25",
		"{b7b0ba1c-1ec1-4be4-b769-cbe40607fe25}",
	} {
		response := deleteHTTPResponse(
			api,
			http.MethodGet,
			"/api/delete/tasks/"+badID,
			"",
			"",
		)
		if response.Code != http.StatusNotFound {
			t.Fatalf("task %q status=%d body=%s", badID, response.Code, response.Body.String())
		}
		assertDeleteJSONResponse(t, response)
	}
	if stub.statusCalls != 0 {
		t.Fatalf("Status called %d times for malformed IDs", stub.statusCalls)
	}

	response := deleteHTTPResponse(
		api,
		http.MethodGet,
		"/api/delete/tasks/"+taskID,
		"",
		"",
	)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown status=%d body=%s", response.Code, response.Body.String())
	}

	want := DeleteTaskStatus{
		TaskID:            taskID,
		Mode:              proto.ModeHard,
		Total:             3,
		OK:                1,
		Failed:            2,
		Uncertain:         1,
		Pending:           0,
		Complete:          true,
		StateSyncFailures: 1,
		ByMachine: map[string]DeleteMachineStatus{
			"machine-a": {
				MachineID:         "machine-a",
				Total:             3,
				OK:                1,
				Failed:            2,
				Uncertain:         1,
				Complete:          true,
				StateSyncFailures: 1,
				Sequences: map[uint32]DeleteSequenceStatus{
					0: {Sequence: 0, LastSeq: 1, Received: true, Total: 2, OK: 1, Failed: 1},
					1: {Sequence: 1, LastSeq: 1, Received: true, Total: 1, Failed: 1, Uncertain: 1},
				},
			},
		},
		ErrorCodes: map[string]int64{
			proto.DeleteErrDeleteFailed: 1,
			proto.DeleteErrHelperLost:   1,
		},
		Problems: []DeleteProblemItem{
			{
				MachineID:    "machine-a",
				Sequence:     0,
				Path:         `D:\failed`,
				ErrorCode:    proto.DeleteErrDeleteFailed,
				ErrorMessage: "delete item failed",
			},
			{
				MachineID:    "machine-a",
				Sequence:     1,
				Path:         `D:\uncertain`,
				ErrorCode:    proto.DeleteErrHelperLost,
				ErrorMessage: "delete item failed",
				Uncertain:    true,
				StateSyncErr: "delete state synchronization failed",
			},
		},
	}
	stub.statusFn = func(gotTaskID string) (DeleteTaskStatus, bool) {
		if gotTaskID != taskID {
			t.Fatalf("Status taskID=%q", gotTaskID)
		}
		return want, true
	}
	response = deleteHTTPResponse(
		api,
		http.MethodGet,
		"/api/delete/tasks/"+taskID,
		"",
		"",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var got DeleteTaskStatus
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("status=%#v, want %#v", got, want)
	}
	if strings.Contains(response.Body.String(), "confirm_token") {
		t.Fatalf("status contains confirmation token field: %s", response.Body.String())
	}
}

func TestDeleteHTTPStatusUnavailableAndSanitizesInjectedErrors(t *testing.T) {
	const taskID = "b7b0ba1c-1ec1-4be4-b769-cbe40607fe25"
	nilResponse := deleteHTTPResponse(
		NewAPI(nil, nil, nil),
		http.MethodGet,
		"/api/delete/tasks/"+taskID,
		"",
		"",
	)
	if nilResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil service status=%d body=%s", nilResponse.Code, nilResponse.Body.String())
	}

	const secret = "postgres://user:raw-db-secret@example/db"
	source := DeleteTaskStatus{
		TaskID: taskID,
		ErrorCodes: map[string]int64{
			secret: 1,
		},
		Problems: []DeleteProblemItem{{
			Path:         `D:\failed`,
			ErrorCode:    secret,
			ErrorMessage: secret,
			StateSyncErr: secret,
		}},
	}
	stub := &deleteHTTPStub{
		statusFn: func(string) (DeleteTaskStatus, bool) {
			return source, true
		},
	}
	api := NewAPI(nil, nil, nil)
	api.delete = stub
	response := deleteHTTPResponse(
		api,
		http.MethodGet,
		"/api/delete/tasks/"+taskID,
		"",
		"",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), secret) {
		t.Fatalf("status leaked injected secret: %s", response.Body.String())
	}
	if source.Problems[0].ErrorMessage != secret ||
		source.Problems[0].StateSyncErr != secret {
		t.Fatalf("handler mutated service status: %#v", source)
	}
}

func TestDeleteHTTPSetDeleteServiceNilClearsAllRoutes(t *testing.T) {
	const taskID = "b7b0ba1c-1ec1-4be4-b769-cbe40607fe25"
	api := NewAPI(nil, nil, nil)
	api.SetDeleteService(NewDeleteService(nil, nil, nil, nil))
	api.SetDeleteService(nil)

	for _, test := range []struct {
		name        string
		method      string
		target      string
		contentType string
		body        string
	}{
		{
			"prepare",
			http.MethodPost,
			"/api/delete/prepare",
			"application/json",
			`{"member_ids":[1]}`,
		},
		{
			"execute",
			http.MethodPost,
			"/api/delete/execute",
			"application/json",
			`{"confirm_token":"synthetic-non-secret"}`,
		},
		{
			"status",
			http.MethodGet,
			"/api/delete/tasks/" + taskID,
			"",
			"",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := deleteHTTPResponse(
				api,
				test.method,
				test.target,
				test.contentType,
				test.body,
			)
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			assertDeleteJSONResponse(t, response)
			var payload map[string]string
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(payload, map[string]string{
				"error": "delete service unavailable",
			}) {
				t.Fatalf("payload=%v", payload)
			}
		})
	}
}
