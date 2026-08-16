package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"dedup/internal/nodetray/agentclient"
	"dedup/internal/nodetray/config"
	"dedup/internal/nodetray/traymodel"
	"dedup/internal/nodetray/windows/elevation"
	nodetask "dedup/internal/nodetray/windows/task"
	"dedup/internal/proto"
	"github.com/vmihailenco/msgpack/v5"
)

type fakeStore struct {
	settings       traymodel.TraySettings
	helper         config.HelperForm
	prepared       config.PreparedWrite
	calls          *[]string
	saveErr        error
	defaultErr     error
	fingerprint    string
	fingerprintErr error
	loadErr        error
	loadCalls      int
	helperFields   []config.FieldError
}

func (f *fakeStore) LoadTraySettings() (traymodel.TraySettings, error) {
	f.loadCalls++
	return f.settings, f.loadErr
}
func (f *fakeStore) SaveTraySettings(v traymodel.TraySettings) error {
	*f.calls = append(*f.calls, "save-settings")
	if f.saveErr == nil {
		f.settings = v
	}
	return f.saveErr
}
func (f *fakeStore) LoadHelperForm() (config.HelperForm, error) { return f.helper, f.loadErr }
func (f *fakeStore) ValidateHelperForm(config.HelperForm) []config.FieldError {
	return append([]config.FieldError(nil), f.helperFields...)
}
func (f *fakeStore) SaveHelperForm(config.HelperForm) (string, error) {
	*f.calls = append(*f.calls, "save-helper")
	if f.saveErr != nil {
		return "", f.saveErr
	}
	return f.prepared.SHA256, nil
}
func (f *fakeStore) PrepareHelperWrite(config.HelperForm) (config.PreparedWrite, error) {
	*f.calls = append(*f.calls, "prepare-helper")
	if f.saveErr != nil {
		return config.PreparedWrite{}, f.saveErr
	}
	return f.prepared, nil
}
func (f *fakeStore) PrepareDefaultHelperWrite() (config.PreparedWrite, error) {
	if f.defaultErr != nil {
		return config.PreparedWrite{}, f.defaultErr
	}
	*f.calls = append(*f.calls, "prepare-default-helper")
	prepared := f.prepared
	prepared.CreateOnly = true
	return prepared, nil
}
func (f *fakeStore) HelperFingerprint() (string, error) {
	*f.calls = append(*f.calls, "read-helper-fingerprint")
	return f.fingerprint, f.fingerprintErr
}

type fakeValidator struct{ helper []config.FieldError }

func (f fakeValidator) ValidateHelper(config.HelperForm) []config.FieldError {
	return append([]config.FieldError(nil), f.helper...)
}

type fakeAgentConfigGateway struct {
	form        config.AgentForm
	fields      []config.FieldError
	result      AgentConfigSaveResult
	err         error
	source      *fakeStore
	calls       *[]string
	callPrefix  string
	loadCtx     context.Context
	validateCtx context.Context
	saveCtx     context.Context
}

type fakeLocalAgentGateway struct {
	requests  []string
	responses map[string]any
	operation string
	control   proto.LocalTaskControlRequest
	err       error
}

type deadlineInspectLocalAgentGateway struct {
	deadlines []time.Time
}

func (f *deadlineInspectLocalAgentGateway) CallLocal(ctx context.Context, _ string, _, _ any) error {
	deadline, ok := ctx.Deadline()
	if ok {
		f.deadlines = append(f.deadlines, deadline)
	} else {
		f.deadlines = append(f.deadlines, time.Time{})
	}
	return errors.New("deadline inspection")
}

type blockingLocalAgentGateway struct{}

func (*blockingLocalAgentGateway) CallLocal(ctx context.Context, _ string, _, _ any) error {
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeLocalAgentGateway) CallLocal(_ context.Context, operation string, request, response any) error {
	f.requests = append(f.requests, operation)
	f.operation = operation
	if control, ok := request.(proto.LocalTaskControlRequest); ok {
		f.control = control
	}
	if f.err != nil {
		return f.err
	}
	value, ok := f.responses[operation]
	if !ok {
		return errors.New("agent_disconnected")
	}
	raw, _ := msgpack.Marshal(value)
	return msgpack.Unmarshal(raw, response)
}

// Break caught: a task action omits the instance or revision and can control a
// newer task attempt that reuses the same task ID.
func TestLocalTaskControlsForwardVersionedIdentityAndSafeSnapshot(t *testing.T) {
	operations := []struct {
		name      string
		operation string
		call      func(*Service, context.Context, traymodel.LocalTaskControl) traymodel.LocalTaskResult
	}{
		{"pause", proto.LocalOperationTaskPause, (*Service).PauseLocalTask},
		{"resume", proto.LocalOperationTaskResume, (*Service).ResumeLocalTask},
		{"cancel", proto.LocalOperationTaskCancel, (*Service).CancelLocalTask},
		{"delete", proto.LocalOperationTaskDelete, (*Service).DeleteLocalTask},
		{"retry", proto.LocalOperationTaskRetry, (*Service).RetryLocalTask},
	}
	for _, test := range operations {
		t.Run(test.name, func(t *testing.T) {
			service, _, _, _, _, _ := serviceFixture(t)
			gateway := &fakeLocalAgentGateway{responses: map[string]any{
				test.operation: encodedTaskControlResponse("paused", 8),
			}}
			service.localAgent = gateway

			result := test.call(service, context.Background(), traymodel.LocalTaskControl{
				TaskID: "task-1", InstanceID: "instance-1", ExpectedRevision: 7,
			})
			if !result.OK || result.Deleted || result.Task.Phase != "analysis" || result.Task.Revision != 8 ||
				!result.Task.ProgressTotalKnown || result.Task.CreatedAt != 100 || result.Task.UpdatedAt != 200 ||
				result.Task.StartedAt != 110 || result.Task.CompletedAt != 0 || result.Task.ErrorCode != "safe_error" ||
				result.Task.ErrorSummary != "safe message" {
				t.Fatalf("result=%#v", result)
			}
			if gateway.operation != test.operation {
				t.Fatalf("operation=%q, want %q", gateway.operation, test.operation)
			}
			if want := (proto.LocalTaskControlRequest{TaskID: "task-1", InstanceID: "instance-1", ExpectedRevision: 7}); gateway.control != want {
				t.Fatalf("control=%#v, want %#v", gateway.control, want)
			}
		})
	}
}

// Break caught: raw transport errors reach the WebView, or malformed versioned
// requests are sent to the Agent before being rejected locally.
func TestLocalTaskControlsRejectInvalidVersionAndRedactGatewayError(t *testing.T) {
	service, _, _, _, _, _ := serviceFixture(t)
	gateway := &fakeLocalAgentGateway{responses: map[string]any{}, err: errors.New("private_socket_failure")}
	service.localAgent = gateway

	invalid := service.PauseLocalTask(context.Background(), traymodel.LocalTaskControl{TaskID: "task-1", InstanceID: "instance-1"})
	if invalid.OK || invalid.ErrorCode != proto.InvalidTaskControlErrorCode || len(gateway.requests) != 0 {
		t.Fatalf("invalid=%#v, requests=%v", invalid, gateway.requests)
	}
	failed := service.PauseLocalTask(context.Background(), traymodel.LocalTaskControl{TaskID: "task-1", InstanceID: "instance-1", ExpectedRevision: 7})
	if failed.OK || failed.ErrorCode != "local_operation_failed" || strings.Contains(fmt.Sprintf("%#v", failed), "private_socket_failure") {
		t.Fatalf("failed=%#v", failed)
	}
}

// Break caught: refreshable optimistic-concurrency codes are hidden from the
// WebView, or arbitrary remote/private error text is exposed as a public code.
func TestLocalTaskControlsExposeAllSafeRemoteCodesWithFixedSummaries(t *testing.T) {
	tests := []struct {
		code    string
		summary string
	}{
		{"stale_task", "任务状态已更新，请刷新后重试"},
		{"task_instance_mismatch", "任务实例已更新，请刷新后重试"},
		{"invalid_task_state", "当前任务状态不支持此操作"},
		{"task_not_found", "任务不存在或已删除"},
		{"local_task_unavailable", "本机任务服务暂不可用"},
		{"task_delete_failed", "删除任务失败，请稍后重试"},
		{"task_control_failed", "任务操作失败，请稍后重试"},
		{"task_instance_required", "任务实例信息缺失，请刷新后重试"},
	}
	for _, test := range tests {
		t.Run(test.code, func(t *testing.T) {
			service, _, _, _, _, _ := serviceFixture(t)
			service.localAgent = &fakeLocalAgentGateway{responses: map[string]any{}, err: fmt.Errorf("wrapped transport secret: %w", &agentclient.RemoteError{Code: test.code})}

			result := service.PauseLocalTask(context.Background(), traymodel.LocalTaskControl{
				TaskID: "task-1", InstanceID: "instance-1", ExpectedRevision: 7,
			})
			if result.OK || result.ErrorCode != test.code || result.ErrorSummary != test.summary {
				t.Fatalf("result=%#v", result)
			}
			if strings.Contains(fmt.Sprintf("%#v", result), "secret") {
				t.Fatalf("result leaked transport text: %#v", result)
			}
		})
	}

	service, _, _, _, _, _ := serviceFixture(t)
	service.localAgent = &fakeLocalAgentGateway{responses: map[string]any{}, err: &agentclient.RemoteError{Code: "private_backend_failure"}}
	result := service.DeleteLocalTask(context.Background(), traymodel.LocalTaskControl{
		TaskID: "task-1", InstanceID: "instance-1", ExpectedRevision: 7,
	})
	if result.OK || result.ErrorCode != "local_operation_failed" || result.ErrorSummary != "本机 Agent 暂不可用，请稍后重试" ||
		strings.Contains(fmt.Sprintf("%#v", result), "private_backend_failure") {
		t.Fatalf("result=%#v", result)
	}
	service.localAgent = &fakeLocalAgentGateway{responses: map[string]any{}, err: errors.New("raw_private_failure")}
	page := service.ListLocalTasks(context.Background(), traymodel.PageRequest{Limit: 50})
	if page.OK || page.ErrorCode != "local_operation_failed" || page.ErrorSummary != "本机 Agent 暂不可用，请稍后重试" ||
		strings.Contains(fmt.Sprintf("%#v", page), "raw_private_failure") {
		t.Fatalf("page leaked raw error: %#v", page)
	}
}

// Break caught: a local request inherited the process lifetime context and
// could leave Wails calls and frontend polling pending forever.
func TestLocalTaskListAndControlUseBoundedRequestContexts(t *testing.T) {
	service, _, _, _, _, _ := serviceFixture(t)
	gateway := &deadlineInspectLocalAgentGateway{}
	service.localAgent = gateway
	started := time.Now()

	_ = service.ListLocalTasks(context.Background(), traymodel.PageRequest{Limit: 50})
	_ = service.PauseLocalTask(context.Background(), traymodel.LocalTaskControl{
		TaskID: "task-1", InstanceID: "instance-1", ExpectedRevision: 7,
	})

	if len(gateway.deadlines) != 2 {
		t.Fatalf("deadline observations=%v", gateway.deadlines)
	}
	for i, deadline := range gateway.deadlines {
		if deadline.IsZero() {
			t.Fatalf("call %d had no deadline", i)
		}
		remaining := deadline.Sub(started)
		if remaining <= 0 || remaining > 11*time.Second {
			t.Fatalf("call %d deadline bound=%v, want <= 11s", i, remaining)
		}
	}
}

func TestLocalTaskListDeadlineCancelsBlockingGateway(t *testing.T) {
	service, _, _, _, _, _ := serviceFixture(t)
	service.localAgent = &blockingLocalAgentGateway{}
	service.localRequestTimeout = 5 * time.Millisecond
	started := time.Now()

	page := service.ListLocalTasks(context.Background(), traymodel.PageRequest{Limit: 50})

	if page.OK || page.ErrorCode != "local_operation_failed" {
		t.Fatalf("page=%#v", page)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("blocking gateway returned after %v", elapsed)
	}
}

// Break caught: Agent display stats reached Store, but numeric production JSON
// could not be decoded into the string-only Wails projection.
func TestListLocalTasksMapsProductionDisplayStatsJSON(t *testing.T) {
	service, _, _, _, _, _ := serviceFixture(t)
	statsJSON, err := json.Marshal(proto.LocalTaskDisplayStats{
		SchemaVersion: proto.LocalTaskDisplayStatsVersion,
		Speed:         12.5, Failures: 3, DurationMS: 192_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	service.localAgent = &fakeLocalAgentGateway{responses: map[string]any{
		proto.LocalOperationTaskList: proto.LocalTaskListResponse{Tasks: []proto.LocalTask{{
			TaskID: "stats-task", InstanceID: "stats-instance", Revision: 1,
			Mode: proto.LocalTaskModeScanThenAnalysis, Status: "running",
			StatsJSON: string(statsJSON),
		}}},
	}}

	page := service.ListLocalTasks(context.Background(), traymodel.PageRequest{Limit: 50})
	if !page.OK || len(page.Tasks) != 1 {
		t.Fatalf("page=%#v", page)
	}
	task := page.Tasks[0]
	if task.Speed != "12.5 文件/秒" || task.Failures != 3 || task.Duration != "00:03:12" {
		t.Fatalf("mapped task=%#v", task)
	}
}

// Break caught: successful idempotent deletion is rendered as a failed or
// partially populated task response instead of an explicit deletion result.
func TestDeleteLocalTaskReturnsDeletedWithoutTaskSnapshot(t *testing.T) {
	service, _, _, _, _, _ := serviceFixture(t)
	service.localAgent = &fakeLocalAgentGateway{responses: map[string]any{
		proto.LocalOperationTaskDelete: proto.LocalTaskControlResponse{Deleted: true},
	}}

	result := service.DeleteLocalTask(context.Background(), traymodel.LocalTaskControl{TaskID: "task-1", InstanceID: "instance-1", ExpectedRevision: 7})
	if !result.OK || !result.Deleted || !reflect.DeepEqual(result.Task, traymodel.LocalTask{}) {
		t.Fatalf("result=%#v", result)
	}
}

func encodedTaskControlResponse(status string, revision int64) proto.LocalTaskControlResponse {
	return proto.LocalTaskControlResponse{Task: &proto.LocalTask{
		TaskID: "task-1", InstanceID: "instance-1", Revision: revision, Source: "nodetray", Mode: proto.LocalTaskModeScanThenAnalysis,
		Stage: 2, Phase: "analysis", Status: status, Roots: []string{`D:\media`}, ProgressComplete: 4, ProgressTotal: 10,
		ProgressTotalKnown: true, SafeErrorCode: "safe_error", SafeErrorMessage: "safe message", CreatedAt: 100, UpdatedAt: 200, StartedAt: 110,
	}}
}

func TestLocalConsoleUsesSocketAndKeepsDeleteTokenServerSide(t *testing.T) {
	s, _, _, _, _, _ := serviceFixture(t)
	gateway := &fakeLocalAgentGateway{responses: map[string]any{
		proto.LocalOperationTaskCreate:    proto.LocalTaskCreateResponse{Task: proto.LocalTask{TaskID: "task-1", Source: "nodetray", Mode: proto.LocalTaskModeScanThenAnalysis, Stage: 1, Status: "running"}},
		proto.LocalOperationTaskList:      proto.LocalTaskListResponse{Tasks: []proto.LocalTask{{TaskID: "task-1", Source: "nodetray", Stage: 1, Status: "running"}}},
		proto.LocalOperationGroupsList:    proto.LocalGroupListResponse{Groups: []proto.LocalGroup{{RunID: "run-1", GroupID: "group-1", Category: "image", Verdict: "duplicate"}}},
		proto.LocalOperationReviewSave:    proto.LocalReviewSaveResponse{Saved: true},
		proto.LocalOperationDeletePrepare: proto.LocalDeletePreview{BatchID: "batch-1", RunID: "run-1", GroupID: "group-1", Count: 1, SelectionDigest: "digest", Token: "one-time-secret", Files: []proto.LocalDeleteFile{{FileID: 7, Path: `D:\media\a.jpg`, Size: 12}}},
		proto.LocalOperationDeleteExecute: proto.LocalDeleteBatch{BatchID: "batch-1", Status: "complete", Requested: 1, Succeeded: 1},
		proto.LocalOperationPreviewImage:  proto.LocalImagePreviewResponse{MIME: "image/jpeg", Width: 40, Height: 20, Bytes: []byte{1, 2, 3}},
	}}
	s.localAgent = gateway

	created := s.CreateLocalTask(context.Background(), traymodel.LocalTaskCreate{TaskID: "task-1", Roots: []string{`D:\media`}, Mode: proto.LocalTaskModeScanThenAnalysis})
	if !created.OK || created.Task.TaskID != "task-1" {
		t.Fatalf("CreateLocalTask = %#v", created)
	}
	if page := s.ListLocalTasks(context.Background(), traymodel.PageRequest{Limit: 50}); !page.OK || len(page.Tasks) != 1 {
		t.Fatalf("ListLocalTasks = %#v", page)
	}
	if page := s.ListLocalGroups(context.Background(), traymodel.LocalGroupQuery{Limit: 50}); !page.OK || len(page.Groups) != 1 {
		t.Fatalf("ListLocalGroups = %#v", page)
	}
	if result := s.SaveLocalReview(context.Background(), traymodel.LocalReviewSave{RunID: "run-1", GroupID: "group-1", Reviewer: "local", Decisions: []traymodel.LocalReviewDecision{{FileID: 7, Decision: "keep"}}}); !result.OK {
		t.Fatalf("SaveLocalReview = %#v", result)
	}
	preview := s.PrepareLocalDelete(context.Background(), traymodel.LocalDeletePrepare{RunID: "run-1", GroupID: "group-1"})
	if !preview.OK || strings.Contains(fmt.Sprintf("%#v", preview), "one-time-secret") {
		t.Fatalf("PrepareLocalDelete leaked token: %#v", preview)
	}
	batch := s.ExecuteLocalDelete(context.Background(), traymodel.LocalDeleteExecute{BatchID: preview.BatchID, SelectionDigest: preview.SelectionDigest})
	if !batch.OK || batch.Succeeded != 1 {
		t.Fatalf("ExecuteLocalDelete = %#v", batch)
	}
	if second := s.ExecuteLocalDelete(context.Background(), traymodel.LocalDeleteExecute{BatchID: preview.BatchID, SelectionDigest: preview.SelectionDigest}); second.OK {
		t.Fatalf("one-time token was reusable: %#v", second)
	}
	image := s.GetLocalImagePreview(context.Background(), 7)
	if !image.OK || image.DataBase64 != "AQID" || strings.Contains(image.DataBase64, "file://") {
		t.Fatalf("GetLocalImagePreview = %#v", image)
	}

	want := []string{proto.LocalOperationTaskCreate, proto.LocalOperationTaskList, proto.LocalOperationGroupsList, proto.LocalOperationReviewSave, proto.LocalOperationDeletePrepare, proto.LocalOperationDeleteExecute, proto.LocalOperationPreviewImage}
	if !reflect.DeepEqual(gateway.requests, want) {
		t.Fatalf("socket operations = %v, want %v", gateway.requests, want)
	}
}

func (f *fakeAgentConfigGateway) record(operation string) {
	*f.calls = append(*f.calls, f.callPrefix+operation)
}

func (f *fakeAgentConfigGateway) LoadAgentForm(ctx context.Context) (config.AgentForm, error) {
	f.loadCtx = ctx
	if f.callPrefix != "" {
		f.record("load-agent")
	}
	if f.source != nil {
		return f.form, f.source.loadErr
	}
	return f.form, f.err
}

func (f *fakeAgentConfigGateway) ValidateAgentForm(ctx context.Context, _ config.AgentForm) []config.FieldError {
	f.validateCtx = ctx
	if f.callPrefix != "" {
		f.record("validate-agent")
	}
	return append([]config.FieldError(nil), f.fields...)
}

func (f *fakeAgentConfigGateway) SaveAgentForm(ctx context.Context, _ config.AgentForm) (AgentConfigSaveResult, error) {
	f.saveCtx = ctx
	f.record("save-agent")
	if f.source != nil && f.source.saveErr != nil {
		return AgentConfigSaveResult{}, f.source.saveErr
	}
	if f.result == (AgentConfigSaveResult{}) {
		return AgentConfigSaveResult{SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}, f.err
	}
	return f.result, f.err
}

func (f *fakeAgentConfigGateway) PromotePendingEndpoint() {
	f.record("promote-agent-endpoint")
}

type fakeComponent struct {
	name    string
	calls   *[]string
	state   traymodel.ComponentState
	results map[string]traymodel.OperationResult
}

func (f *fakeComponent) result(op string) traymodel.OperationResult {
	*f.calls = append(*f.calls, f.name+"-"+op)
	if value, ok := f.results[op]; ok {
		return value
	}
	return traymodel.OperationResult{OK: true}
}
func (f *fakeComponent) Start(context.Context) traymodel.OperationResult { return f.result("start") }
func (f *fakeComponent) Stop(context.Context) traymodel.OperationResult  { return f.result("stop") }
func (f *fakeComponent) Restart(context.Context) traymodel.OperationResult {
	return f.result("restart")
}
func (f *fakeComponent) ForceStopTracked(context.Context) traymodel.OperationResult {
	return f.result("force")
}
func (f *fakeComponent) Refresh(context.Context) traymodel.ComponentState { return f.state }

type fakeTask struct {
	calls        *[]string
	status       nodetask.Status
	err          error
	inspectCalls int
}

func (f *fakeTask) Inspect(context.Context) (nodetask.Status, error) {
	f.inspectCalls++
	return f.status, f.err
}
func (f *fakeTask) Run(context.Context) error { *f.calls = append(*f.calls, "task-run"); return f.err }
func (f *fakeTask) Stop(context.Context) error {
	*f.calls = append(*f.calls, "task-stop")
	return f.err
}

type fakeElevation struct {
	calls   *[]string
	result  elevation.InvocationResult
	err     error
	actions []elevation.Action
}

func (f *fakeElevation) Invoke(_ context.Context, action elevation.Action, _ []byte) (elevation.InvocationResult, error) {
	*f.calls = append(*f.calls, "elevate-"+string(action))
	f.actions = append(f.actions, action)
	return f.result, f.err
}

type fakeLogin struct {
	calls     *[]string
	enabled   bool
	current   string
	err       error
	readCalls int
}

func (f *fakeLogin) Enabled() (bool, string, error) {
	f.readCalls++
	return f.enabled, f.current, f.err
}
func (f *fakeLogin) Enable(string) error { *f.calls = append(*f.calls, "login-enable"); return f.err }
func (f *fakeLogin) Disable() error      { *f.calls = append(*f.calls, "login-disable"); return f.err }

type fakeResolver struct {
	values map[string]string
	err    error
}

func (f fakeResolver) Final(path string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if value, ok := f.values[path]; ok {
		return value, nil
	}
	return filepath.Clean(path), nil
}

type fakeOpener struct {
	calls *[]string
	err   error
}

func (f fakeOpener) Open(_ context.Context, path string) error {
	*f.calls = append(*f.calls, "open:"+path)
	return f.err
}

type fakeWorkers struct {
	values []traymodel.WorkerState
	err    error
	calls  *[]string
}

type fakeProcessWaiter struct {
	calls *[]string
	errs  map[int]error
}

func (f *fakeProcessWaiter) WaitPIDGone(_ context.Context, pid int) error {
	*f.calls = append(*f.calls, fmt.Sprintf("worker-%d-wait", pid))
	return f.errs[pid]
}

type fakeFingerprintUpdater struct {
	name   string
	calls  *[]string
	values []string
	result traymodel.OperationResult
}

func (f *fakeFingerprintUpdater) UpdateExpectedSHA256(value string) traymodel.OperationResult {
	*f.calls = append(*f.calls, f.name+"-sha")
	f.values = append(f.values, value)
	if f.result == (traymodel.OperationResult{}) {
		return traymodel.OperationResult{OK: true}
	}
	return f.result
}

func (f fakeWorkers) Snapshot(context.Context) ([]traymodel.WorkerState, error) {
	if f.calls != nil {
		*f.calls = append(*f.calls, "workers-snapshot")
	}
	return append([]traymodel.WorkerState(nil), f.values...), f.err
}

func validSettings() traymodel.TraySettings {
	return traymodel.TraySettings{AgentStartMode: traymodel.StartManual, HelperEnabled: true, HelperStartMode: traymodel.StartManual, RefreshIntervalSeconds: 2, NotificationLevel: traymodel.NotifyImportant}
}

func serviceFixture(t *testing.T) (*Service, *[]string, *fakeStore, *fakeComponent, *fakeComponent, *fakeElevation) {
	t.Helper()
	calls := []string{}
	store := &fakeStore{settings: validSettings(), calls: &calls, defaultErr: config.ErrHelperConfigExists, prepared: config.PreparedWrite{TargetPath: `C:\ProgramData\MySingerServer\helper.json`, CanonicalJSON: []byte("{}"), SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	agentConfig := &fakeAgentConfigGateway{source: store, calls: &calls}
	agent := &fakeComponent{name: "agent", calls: &calls, results: map[string]traymodel.OperationResult{}}
	helper := &fakeComponent{name: "helper", calls: &calls, results: map[string]traymodel.OperationResult{}}
	elevated := &fakeElevation{calls: &calls, result: elevation.InvocationResult{Response: elevation.Response{OK: true}}}
	locations := map[traymodel.LocationKind]Location{
		traymodel.AgentLogs:    {Path: `C:\node\agent\logs`, Root: `C:\node\agent`},
		traymodel.HelperLogs:   {Path: `C:\node\helper\logs`, Root: `C:\node\helper`},
		traymodel.AgentBackup:  {Path: `C:\node\agent\backup`, Root: `C:\node\agent`},
		traymodel.HelperBackup: {Path: `C:\node\helper\backup`, Root: `C:\node\helper`},
	}
	s := NewService(Dependencies{
		Store: store, Validator: fakeValidator{}, AgentConfig: agentConfig, Agent: agent, Helper: helper,
		Task: &fakeTask{calls: &calls}, Elevation: elevated,
		LoginStart: &fakeLogin{calls: &calls}, TrayExecutable: `C:\node\nodetray.exe`,
		TaskDefinition:    nodetask.Definition{HelperExecutable: `C:\node\helper.exe`, HelperConfig: store.prepared.TargetPath, UserSID: "S-1-5-21-1"},
		MachineID:         "node-" + strings.Repeat("1", 64),
		AgentFingerprint:  &fakeFingerprintUpdater{name: "agent", calls: &calls},
		HelperFingerprint: &fakeFingerprintUpdater{name: "helper", calls: &calls},
		Locations:         locations, PathResolver: fakeResolver{}, Opener: fakeOpener{calls: &calls}, Workers: fakeWorkers{},
	})
	return s, &calls, store, agent, helper, elevated
}

func TestOverviewIncludesSanitizedWorkerSummaryAndDriftWithoutForms(t *testing.T) {
	s, _, _, agent, helper, _ := serviceFixture(t)
	agent.state = traymodel.ComponentState{Lifecycle: traymodel.Running, ErrorSummary: "password=secret"}
	helper.state = traymodel.ComponentState{Lifecycle: traymodel.Running}
	s.workers = fakeWorkers{values: []traymodel.WorkerState{{Index: 1, Ready: true, CurrentTaskSummary: `D:\media\private\clip.mp4`, LastErrorSummary: "postgres://u:p@db/media"}}}
	overview, err := s.GetOverview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if overview.MachineID != "node-"+strings.Repeat("1", 64) || len(overview.Workers) != 1 {
		t.Fatalf("overview = %#v", overview)
	}
	joined := overview.Agent.ErrorSummary + overview.Workers[0].CurrentTaskSummary + overview.Workers[0].LastErrorSummary
	for _, forbidden := range []string{"secret", `D:\media`, "u:p"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("overview leaked %q in %q", forbidden, joined)
		}
	}
}

func TestOverviewUsesAnEmptyWorkerArrayWhenNoWorkersArePresent(t *testing.T) {
	s, _, _, _, _, _ := serviceFixture(t)
	overview, err := s.GetOverview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if overview.Workers == nil {
		t.Fatal("GetOverview returned a nil worker list; Wails would serialize it as null")
	}
}

func TestOverviewNormalizesDisabledUnavailableHelperAndKeepsTaskDrift(t *testing.T) {
	s, _, store, agent, helper, _ := serviceFixture(t)
	store.settings.HelperEnabled = false
	store.settings.HelperStartMode = traymodel.StartManual
	agent.state = traymodel.ComponentState{
		Lifecycle: traymodel.Failed, ErrorCode: "agent_failed",
		ErrorSummary: "Agent still requires attention", NeedsAttention: true,
	}
	helper.state = traymodel.ComponentState{
		Lifecycle: traymodel.Failed, Healthy: true, Ready: true, PID: 0,
		StartedAtUnixMS: 99, UptimeSeconds: 88, WorkerReady: 1,
		WorkerExpected: 2, ActiveRequests: 3, ErrorCode: "unavailable",
		ErrorSummary: "Helper configuration unavailable", NeedsAttention: true,
		RuntimeConfigSHA256: strings.Repeat("b", 64),
		SavedConfigSHA256:   strings.Repeat("a", 64), NeedsRestart: true,
	}
	s.task = &fakeTask{calls: &[]string{}, status: nodetask.Status{Installed: true}}

	overview, err := s.GetOverview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantHelper := traymodel.ComponentState{
		Lifecycle:         traymodel.Stopped,
		SavedConfigSHA256: strings.Repeat("a", 64),
	}
	if !reflect.DeepEqual(overview.Helper, wantHelper) {
		t.Fatalf("disabled Helper = %#v, want %#v", overview.Helper, wantHelper)
	}
	if overview.HelperTaskDrift {
		t.Fatal("obsolete Helper task affected current direct-start state")
	}
	if overview.Agent.Lifecycle != traymodel.Failed || overview.Agent.ErrorCode != "agent_failed" {
		t.Fatalf("Agent state was changed by Helper normalization: %#v", overview.Agent)
	}
}

func TestOverviewKeepsEnabledUnavailableHelper(t *testing.T) {
	s, _, _, _, helper, _ := serviceFixture(t)
	helper.state = attentionState("unavailable", "Helper configuration unavailable")

	overview, err := s.GetOverview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if overview.Helper.Lifecycle != traymodel.Failed ||
		overview.Helper.ErrorCode != "unavailable" || !overview.Helper.NeedsAttention {
		t.Fatalf("enabled Helper error was hidden: %#v", overview.Helper)
	}
}

func TestOverviewKeepsDisabledHelperWhenRealPIDExists(t *testing.T) {
	s, _, store, _, helper, _ := serviceFixture(t)
	store.settings.HelperEnabled = false
	helper.state = traymodel.ComponentState{
		Lifecycle: traymodel.Running, Healthy: true, Ready: true,
		PID: 4321, StartedAtUnixMS: 123, UptimeSeconds: 10,
		SavedConfigSHA256: strings.Repeat("a", 64),
	}

	overview, err := s.GetOverview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(overview.Helper, helper.state) {
		t.Fatalf("live disabled Helper was hidden: %#v", overview.Helper)
	}
}

func TestNormalizeDisabledHelperStateRejectsInvalidSavedDigest(t *testing.T) {
	got := normalizeDisabledHelperState(false, traymodel.ComponentState{
		Lifecycle:         traymodel.Failed,
		SavedConfigSHA256: strings.Repeat("A", 64),
	})
	if got.SavedConfigSHA256 != "" || got.Lifecycle != traymodel.Stopped {
		t.Fatalf("invalid digest survived normalization: %#v", got)
	}
}

func TestGetterErrorsAreSanitizedBeforeTheyReachUI(t *testing.T) {
	s, _, store, _, _, _ := serviceFixture(t)
	store.loadErr = errors.New("postgres://user:secret@db/media D:\\media\\private\\clip.mp4\r\n")
	for name, load := range map[string]func() error{
		"agent":    func() error { _, err := s.GetAgentForm(context.Background()); return err },
		"helper":   func() error { _, err := s.GetHelperForm(context.Background()); return err },
		"settings": func() error { _, err := s.GetTraySettings(context.Background()); return err },
	} {
		t.Run(name, func(t *testing.T) {
			err := load()
			if err == nil {
				t.Fatal("getter unexpectedly succeeded")
			}
			for _, forbidden := range []string{"secret", `D:\media`, "\r", "\n"} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("error leaked %q in %q", forbidden, err)
				}
			}
		})
	}
}

func TestAgentConfigOperationsUseGatewayAndRuntimeRestartState(t *testing.T) {
	s, calls, store, agent, _, _ := serviceFixture(t)
	store.saveErr = errors.New("local-store-write-must-not-be-used")
	agent.state = traymodel.ComponentState{NeedsRestart: false}
	wantForm := config.AgentForm{DataDir: "socket-form"}
	wantSHA := strings.Repeat("c", 64)
	gateway := &fakeAgentConfigGateway{
		form:   wantForm,
		result: AgentConfigSaveResult{SHA256: wantSHA, RestartRequired: true},
		calls:  calls, callPrefix: "socket-",
	}
	s.agentConfig = gateway
	ctx := context.WithValue(context.Background(), struct{ name string }{"gateway"}, "context-marker")

	gotForm, err := s.GetAgentForm(ctx)
	if err != nil || !reflect.DeepEqual(gotForm, wantForm) {
		t.Fatalf("GetAgentForm = %#v, %v", gotForm, err)
	}
	if fields := s.ValidateAgent(ctx, wantForm); len(fields) != 0 {
		t.Fatalf("ValidateAgent = %#v", fields)
	}
	result := s.SaveAgent(ctx, wantForm)
	if !result.OK || !result.Saved || result.SHA256 != wantSHA || result.NeedsRestart {
		t.Fatalf("SaveAgent = %#v", result)
	}
	if gateway.loadCtx != ctx || gateway.validateCtx != ctx || gateway.saveCtx != ctx {
		t.Fatal("Agent Socket gateway did not receive the Wails request context")
	}
	wantCalls := []string{"socket-load-agent", "socket-validate-agent", "socket-validate-agent", "socket-save-agent", "agent-sha"}
	if !reflect.DeepEqual(*calls, wantCalls) {
		t.Fatalf("calls = %v, want %v", *calls, wantCalls)
	}
}

func TestSaveAgentRejectsInvalidFormBeforeStoreAndKeepsSuccessResultEmpty(t *testing.T) {
	s, calls, store, _, _, _ := serviceFixture(t)
	s.agentConfig.(*fakeAgentConfigGateway).fields = []config.FieldError{{Field: "listenPort", Code: "out_of_range", Message: "bad"}}
	if result := s.SaveAgent(context.Background(), config.AgentForm{}); result.OK || result.ErrorCode != "invalid_config" {
		t.Fatalf("invalid SaveAgent = %#v", result)
	}
	if len(*calls) != 0 {
		t.Fatalf("invalid form wrote config: %v", *calls)
	}

	s.agentConfig.(*fakeAgentConfigGateway).fields = nil
	result := s.SaveAgent(context.Background(), config.AgentForm{})
	if !result.OK || result.ErrorCode != "" || result.ErrorSummary != "" {
		t.Fatalf("valid SaveAgent = %#v", result)
	}
	if !reflect.DeepEqual(*calls, []string{"save-agent", "agent-sha"}) {
		t.Fatalf("calls = %v", *calls)
	}
	_ = store
}

func TestSaveAgentPublishesFingerprintOnlyAfterSuccessfulWrite(t *testing.T) {
	s, calls, store, _, _, _ := serviceFixture(t)
	updater := &fakeFingerprintUpdater{name: "agent", calls: calls}
	s.agentFingerprint = updater
	form := config.AgentForm{DataDir: "node-2"}

	if result := s.SaveAgent(context.Background(), form); !result.OK {
		t.Fatalf("SaveAgent = %#v", result)
	}
	wantSHA := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if !reflect.DeepEqual(*calls, []string{"save-agent", "agent-sha"}) ||
		!reflect.DeepEqual(updater.values, []string{wantSHA}) {
		t.Fatalf("calls=%v fingerprints=%v", *calls, updater.values)
	}

	*calls = nil
	store.saveErr = errors.New("write failed")
	if result := s.SaveAgent(context.Background(), form); result.OK {
		t.Fatalf("failed SaveAgent = %#v", result)
	}
	if !reflect.DeepEqual(*calls, []string{"save-agent"}) || len(updater.values) != 1 {
		t.Fatalf("failed save published updates: calls=%v fingerprints=%v", *calls, updater.values)
	}
}

func TestSaveAgentReturnsFormalDigestAndRuntimeDrift(t *testing.T) {
	s, _, _, agent, _, _ := serviceFixture(t)
	wantSHA := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	agent.state = traymodel.ComponentState{
		Lifecycle:           traymodel.Running,
		RuntimeConfigSHA256: strings.Repeat("a", 64),
		SavedConfigSHA256:   wantSHA,
		NeedsRestart:        true,
	}
	s.agentConfig.(*fakeAgentConfigGateway).result = AgentConfigSaveResult{SHA256: wantSHA, RestartRequired: true}

	result := s.SaveAgent(context.Background(), config.AgentForm{DataDir: "node-2"})

	if !result.OK || !result.Saved || result.Restarted || result.SHA256 != wantSHA || !result.NeedsRestart {
		t.Fatalf("SaveAgent = %#v, want saved formal digest with restart drift", result)
	}
}

func TestSaveAgentFingerprintFailureIsStableAndStopsAfterPublish(t *testing.T) {
	s, calls, _, _, _, _ := serviceFixture(t)
	s.agentFingerprint = &fakeFingerprintUpdater{
		name: "agent", calls: calls,
		result: traymodel.OperationResult{ErrorCode: "fingerprint_update_failed", ErrorSummary: "postgres://u:p@db/media"},
	}

	result := s.SaveAgent(context.Background(), config.AgentForm{DataDir: "node-2"})
	if result.OK || result.ErrorCode != "fingerprint_update_failed" || strings.Contains(result.ErrorSummary, "u:p") {
		t.Fatalf("SaveAgent failure = %#v", result)
	}
	if !reflect.DeepEqual(*calls, []string{"save-agent", "agent-sha"}) {
		t.Fatalf("calls=%v", *calls)
	}
}

func TestSaveAndRestartAgentUsesSaveStopStartOrderAndShortCircuits(t *testing.T) {
	tests := []struct {
		name    string
		saveErr error
		stop    traymodel.OperationResult
		want    []string
	}{
		{name: "success", stop: traymodel.OperationResult{OK: true}, want: []string{"save-agent", "agent-sha", "agent-stop", "promote-agent-endpoint", "agent-start"}},
		{name: "save fails", saveErr: errors.New("postgres://user:secret@db/media\r\n"), stop: traymodel.OperationResult{OK: true}, want: []string{"save-agent"}},
		{name: "stop fails", stop: traymodel.OperationResult{ErrorCode: "stop_timeout", ErrorSummary: "token=secret"}, want: []string{"save-agent", "agent-sha", "agent-stop"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, calls, store, agent, _, _ := serviceFixture(t)
			store.saveErr = tt.saveErr
			agent.results["stop"] = tt.stop
			_ = s.SaveAndRestartAgent(context.Background(), config.AgentForm{})
			if !reflect.DeepEqual(*calls, tt.want) {
				t.Fatalf("calls = %v, want %v", *calls, tt.want)
			}
		})
	}
}

func TestSaveAndRestartAgentReportsSavedWhenRestartFails(t *testing.T) {
	tests := []struct {
		name      string
		stop      traymodel.OperationResult
		start     traymodel.OperationResult
		wantCode  string
		wantCalls []string
	}{
		{
			name:      "stop fails after save",
			stop:      traymodel.OperationResult{ErrorCode: "stop_timeout", ErrorSummary: "token=secret"},
			wantCode:  "stop_timeout",
			wantCalls: []string{"save-agent", "agent-sha", "agent-stop"},
		},
		{
			name:      "start fails after stop",
			stop:      traymodel.OperationResult{OK: true},
			start:     traymodel.OperationResult{ErrorCode: "start_failed", ErrorSummary: "token=secret"},
			wantCode:  "start_failed",
			wantCalls: []string{"save-agent", "agent-sha", "agent-stop", "promote-agent-endpoint", "agent-start"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, calls, _, agent, _, _ := serviceFixture(t)
			agent.results["stop"] = tt.stop
			agent.results["start"] = tt.start

			result := s.SaveAndRestartAgent(context.Background(), config.AgentForm{})

			if result.OK || !result.Saved || result.Restarted || !result.NeedsRestart || result.ErrorCode != tt.wantCode {
				t.Fatalf("result = %#v", result)
			}
			if !reflect.DeepEqual(*calls, tt.wantCalls) {
				t.Fatalf("calls = %v, want %v", *calls, tt.wantCalls)
			}
		})
	}
}

type workflowRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *workflowRecorder) add(event string) {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
}

func (r *workflowRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

type workflowAgentConfig struct {
	recorder     *workflowRecorder
	entered      chan string
	blockMachine string
	release      <-chan struct{}
	mu           sync.Mutex
	persisted    string
}

func (s *workflowAgentConfig) LoadAgentForm(context.Context) (config.AgentForm, error) {
	return config.AgentForm{}, nil
}
func (s *workflowAgentConfig) ValidateAgentForm(context.Context, config.AgentForm) []config.FieldError {
	return nil
}
func (s *workflowAgentConfig) SaveAgentForm(_ context.Context, value config.AgentForm) (AgentConfigSaveResult, error) {
	s.recorder.add("save:" + value.DataDir)
	s.mu.Lock()
	s.persisted = value.DataDir
	s.mu.Unlock()
	if s.entered != nil {
		s.entered <- value.DataDir
	}
	if value.DataDir == s.blockMachine && s.release != nil {
		<-s.release
	}
	if value.DataDir == "node-a" {
		return AgentConfigSaveResult{SHA256: strings.Repeat("a", 64), RestartRequired: true}, nil
	}
	return AgentConfigSaveResult{SHA256: strings.Repeat("b", 64), RestartRequired: true}, nil
}
func (s *workflowAgentConfig) PromotePendingEndpoint() {
	s.recorder.add("promote")
}
func (s *workflowAgentConfig) persistedMachine() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persisted
}

type workflowFingerprintUpdater struct {
	recorder *workflowRecorder
	mu       sync.Mutex
	last     string
}

func (u *workflowFingerprintUpdater) UpdateExpectedSHA256(value string) traymodel.OperationResult {
	u.recorder.add("sha:" + value[:1])
	u.mu.Lock()
	u.last = value
	u.mu.Unlock()
	return traymodel.OperationResult{OK: true}
}
func (u *workflowFingerprintUpdater) value() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.last
}

type workflowComponent struct {
	recorder    *workflowRecorder
	stopEntered chan struct{}
	releaseStop <-chan struct{}
	stopOnce    sync.Once
}

func (c *workflowComponent) Start(context.Context) traymodel.OperationResult {
	c.recorder.add("start")
	return traymodel.OperationResult{OK: true}
}
func (c *workflowComponent) Stop(context.Context) traymodel.OperationResult {
	c.recorder.add("stop")
	if c.stopEntered != nil {
		c.stopOnce.Do(func() { close(c.stopEntered) })
	}
	if c.releaseStop != nil {
		<-c.releaseStop
	}
	return traymodel.OperationResult{OK: true}
}
func (c *workflowComponent) Restart(context.Context) traymodel.OperationResult {
	return traymodel.OperationResult{OK: true}
}
func (c *workflowComponent) ForceStopTracked(context.Context) traymodel.OperationResult {
	return traymodel.OperationResult{OK: true}
}
func (c *workflowComponent) Refresh(context.Context) traymodel.ComponentState {
	return traymodel.ComponentState{}
}

func workflowService(agentConfig *workflowAgentConfig, recorder *workflowRecorder, component Component) (*Service, *workflowFingerprintUpdater) {
	fingerprint := &workflowFingerprintUpdater{recorder: recorder}
	return NewService(Dependencies{
		Validator: fakeValidator{}, AgentConfig: agentConfig, Agent: component,
		MachineID: "node-" + strings.Repeat("1", 64), AgentFingerprint: fingerprint,
	}), fingerprint
}

func TestConcurrentAgentSavesPublishTheSameLastVersionAsTheStore(t *testing.T) {
	recorder := &workflowRecorder{}
	releaseA := make(chan struct{})
	store := &workflowAgentConfig{recorder: recorder, entered: make(chan string, 4), blockMachine: "node-a", release: releaseA}
	service, fingerprint := workflowService(store, recorder, &workflowComponent{recorder: recorder})
	results := make(chan traymodel.ConfigApplyResult, 2)
	go func() { results <- service.SaveAgent(context.Background(), config.AgentForm{DataDir: "node-a"}) }()
	if entered := <-store.entered; entered != "node-a" {
		t.Fatalf("first store entry = %q", entered)
	}
	go func() { results <- service.SaveAgent(context.Background(), config.AgentForm{DataDir: "node-b"}) }()
	interleaved := false
	select {
	case entered := <-store.entered:
		interleaved = entered == "node-b"
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseA)
	for range 2 {
		if result := <-results; !result.OK {
			t.Fatalf("SaveAgent = %#v", result)
		}
	}
	if interleaved {
		t.Fatal("second SaveAgent entered Store before the first workflow published")
	}
	if store.persistedMachine() != "node-b" || fingerprint.value() != strings.Repeat("b", 64) {
		t.Fatalf("persisted=%q sha=%q", store.persistedMachine(), fingerprint.value())
	}
}

func TestSecondSaveCannotEnterSaveAndRestartBetweenOldStopAndNewStart(t *testing.T) {
	recorder := &workflowRecorder{}
	store := &workflowAgentConfig{recorder: recorder, entered: make(chan string, 4)}
	stopEntered := make(chan struct{})
	releaseStop := make(chan struct{})
	component := &workflowComponent{recorder: recorder, stopEntered: stopEntered, releaseStop: releaseStop}
	service, _ := workflowService(store, recorder, component)
	restartResult := make(chan traymodel.ConfigApplyResult, 1)
	go func() {
		restartResult <- service.SaveAndRestartAgent(context.Background(), config.AgentForm{DataDir: "node-a"})
	}()
	<-stopEntered
	if entered := <-store.entered; entered != "node-a" {
		t.Fatalf("restart store entry = %q", entered)
	}
	saveResult := make(chan traymodel.ConfigApplyResult, 1)
	go func() { saveResult <- service.SaveAgent(context.Background(), config.AgentForm{DataDir: "node-b"}) }()
	interleaved := false
	select {
	case entered := <-store.entered:
		if entered == "node-b" {
			interleaved = true
		}
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseStop)
	if result := <-restartResult; !result.OK {
		t.Fatalf("SaveAndRestartAgent = %#v", result)
	}
	if result := <-saveResult; !result.OK {
		t.Fatalf("SaveAgent = %#v", result)
	}
	if interleaved {
		t.Fatal("second SaveAgent entered during save-stop-start workflow")
	}
	want := []string{
		"save:node-a", "sha:a", "stop", "promote", "start",
		"save:node-b", "sha:b",
	}
	if got := recorder.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events=%v, want %v", got, want)
	}
}

func TestSaveHelperWritesLocallyWithoutElevation(t *testing.T) {
	s, calls, _, _, _, elevated := serviceFixture(t)
	result := s.SaveHelper(context.Background(), config.HelperForm{})
	if !result.OK {
		t.Fatalf("SaveHelper = %#v", result)
	}
	if !reflect.DeepEqual(*calls, []string{"save-helper", "helper-sha"}) {
		t.Fatalf("calls = %v", *calls)
	}
	if len(elevated.actions) != 0 {
		t.Fatalf("elevation calls = %d", len(elevated.actions))
	}
}

func TestSaveHelperPublishesFingerprintOnlyAfterLocalWrite(t *testing.T) {
	s, calls, store, _, _, elevated := serviceFixture(t)
	updater := &fakeFingerprintUpdater{name: "helper", calls: calls}
	s.helperFingerprint = updater

	if result := s.SaveHelper(context.Background(), config.HelperForm{}); !result.OK {
		t.Fatalf("SaveHelper = %#v", result)
	}
	wantSHA := strings.Repeat("a", 64)
	if !reflect.DeepEqual(*calls, []string{"save-helper", "helper-sha"}) || !reflect.DeepEqual(updater.values, []string{wantSHA}) {
		t.Fatalf("calls=%v values=%v", *calls, updater.values)
	}

	*calls = nil
	store.saveErr = errors.New("write failed")
	if result := s.SaveHelper(context.Background(), config.HelperForm{}); result.OK {
		t.Fatalf("failed SaveHelper = %#v", result)
	}
	if !reflect.DeepEqual(*calls, []string{"save-helper"}) || len(updater.values) != 1 || len(elevated.actions) != 0 {
		t.Fatalf("failed local write published fingerprint or elevated: calls=%v values=%v elevation=%v", *calls, updater.values, elevated.actions)
	}
}

func TestHelperManualAndAutomaticModesBothUseElevatedComponentLauncher(t *testing.T) {
	s, calls, store, _, _, _ := serviceFixture(t)
	_ = s.StartHelper(context.Background())
	_ = s.StopHelper(context.Background())
	_ = s.RestartHelper(context.Background())
	if !reflect.DeepEqual(*calls, []string{"helper-start", "helper-stop", "helper-restart"}) {
		t.Fatalf("manual calls = %v", *calls)
	}

	*calls = nil
	store.settings.HelperStartMode = traymodel.StartAutomatic
	_ = s.StartHelper(context.Background())
	_ = s.StopHelper(context.Background())
	_ = s.RestartHelper(context.Background())
	if !reflect.DeepEqual(*calls, []string{"helper-start", "helper-stop", "helper-restart"}) {
		t.Fatalf("automatic calls = %v", *calls)
	}
}

func TestInvalidHelperConfigDoesNotInvokeElevatedLauncher(t *testing.T) {
	s, calls, store, _, _, elevated := serviceFixture(t)
	store.helperFields = []config.FieldError{{Field: "allowedRoots", Code: "required", Message: "至少配置一个目录"}}
	if result := s.StartHelper(context.Background()); result.OK || result.ErrorCode != "helper_config_invalid" {
		t.Fatalf("StartHelper = %#v", result)
	}
	if len(*calls) != 0 || len(elevated.actions) != 0 {
		t.Fatalf("invalid config started or elevated: calls=%v elevation=%v", *calls, elevated.actions)
	}
}

func TestExplicitForceStopOperationsRemainIndependent(t *testing.T) {
	s, calls, _, _, _, _ := serviceFixture(t)
	_ = s.ForceStopAgent(context.Background())
	_ = s.ForceStopHelper(context.Background())
	if !reflect.DeepEqual(*calls, []string{"agent-force", "helper-force"}) {
		t.Fatalf("calls = %v", *calls)
	}
}

func TestForceExitAllForcesEveryBackgroundComponentBeforeSuccess(t *testing.T) {
	s, calls, _, _, _, _ := serviceFixture(t)
	s.workers = fakeWorkers{values: []traymodel.WorkerState{{PID: 0}, {PID: 41}, {PID: 42}}, calls: calls}
	s.processWaiter = &fakeProcessWaiter{calls: calls, errs: map[int]error{}}

	result := s.ForceExitAll(context.Background())

	if !result.OK || len(result.FailedComponents) != 0 {
		t.Fatalf("ForceExitAll = %#v", result)
	}
	want := []string{"workers-snapshot", "helper-force", "agent-force", "worker-41-wait", "worker-42-wait"}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("calls = %v, want %v", *calls, want)
	}
}

func TestForceExitAllIgnoresWorkerSnapshotFailureWhenTrackedComponentsExit(t *testing.T) {
	s, calls, _, agent, helper, _ := serviceFixture(t)
	s.workers = fakeWorkers{err: errors.New("control unavailable"), calls: calls}
	s.processWaiter = &fakeProcessWaiter{calls: calls, errs: map[int]error{}}
	agent.results["force"] = traymodel.OperationResult{OK: true}
	helper.results["force"] = traymodel.OperationResult{OK: true}

	result := s.ForceExitAll(context.Background())
	if !result.OK || len(result.FailedComponents) != 0 {
		t.Fatalf("ForceExitAll = %#v", result)
	}
	if !reflect.DeepEqual(*calls, []string{"workers-snapshot", "helper-force", "agent-force"}) {
		t.Fatalf("calls = %v", *calls)
	}
}

func TestForceExitAllSnapshotFailureAndAgentFailureReportsOnlyAgent(t *testing.T) {
	s, _, _, agent, helper, _ := serviceFixture(t)
	s.workers = fakeWorkers{err: errors.New("control unavailable")}
	helper.results["force"] = traymodel.OperationResult{OK: true}
	agent.results["force"] = traymodel.OperationResult{ErrorCode: "force_exit_failed"}

	result := s.ForceExitAll(context.Background())
	if result.OK || !reflect.DeepEqual(result.FailedComponents, []string{"agent"}) {
		t.Fatalf("ForceExitAll = %#v", result)
	}
}

func TestForceExitAllContinuesAfterFailureAndAggregatesSurvivors(t *testing.T) {
	s, calls, _, agent, helper, _ := serviceFixture(t)
	helper.results["force"] = traymodel.OperationResult{ErrorCode: "force_exit_failed", ErrorSummary: "helper alive"}
	agent.results["force"] = traymodel.OperationResult{OK: true}
	s.workers = fakeWorkers{values: []traymodel.WorkerState{{PID: 41}}, calls: calls}
	s.processWaiter = &fakeProcessWaiter{calls: calls, errs: map[int]error{41: errors.New("still alive")}}

	result := s.ForceExitAll(context.Background())

	if result.OK || result.ErrorCode != "force_exit_failed" ||
		!reflect.DeepEqual(result.FailedComponents, []string{"helper", "worker:41"}) {
		t.Fatalf("ForceExitAll = %#v", result)
	}
	if !reflect.DeepEqual(*calls, []string{"workers-snapshot", "helper-force", "agent-force", "worker-41-wait"}) {
		t.Fatalf("calls = %v", *calls)
	}
}

func TestNilServiceComponentMethodsFailClosedWithoutPanic(t *testing.T) {
	var service *Service
	for name, call := range map[string]func() traymodel.OperationResult{
		"start-agent":       func() traymodel.OperationResult { return service.StartAgent(context.Background()) },
		"stop-agent":        func() traymodel.OperationResult { return service.StopAgent(context.Background()) },
		"restart-agent":     func() traymodel.OperationResult { return service.RestartAgent(context.Background()) },
		"force-stop-agent":  func() traymodel.OperationResult { return service.ForceStopAgent(context.Background()) },
		"force-stop-helper": func() traymodel.OperationResult { return service.ForceStopHelper(context.Background()) },
	} {
		t.Run(name, func(t *testing.T) {
			result := call()
			if result.OK || result.ErrorCode != "unavailable" {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestOpenLocationAcceptsOnlyFrozenFinalPathsUnderRoots(t *testing.T) {
	s, calls, _, _, _, _ := serviceFixture(t)
	for _, kind := range []traymodel.LocationKind{traymodel.AgentLogs, traymodel.HelperLogs, traymodel.AgentBackup, traymodel.HelperBackup} {
		if result := s.OpenLocation(context.Background(), kind); !result.OK {
			t.Fatalf("OpenLocation(%q) = %#v", kind, result)
		}
	}
	if len(*calls) != 4 {
		t.Fatalf("open calls = %v", *calls)
	}
	if result := s.OpenLocation(context.Background(), traymodel.LocationKind("arbitrary")); result.OK {
		t.Fatal("unknown location accepted")
	}

	s.locations[traymodel.AgentLogs] = Location{Path: `C:\node\agent\logs`, Root: `C:\node\agent`}
	s.pathResolver = fakeResolver{values: map[string]string{`C:\node\agent\logs`: `D:\escape`, `C:\node\agent`: `C:\node\agent`}}
	if result := s.OpenLocation(context.Background(), traymodel.AgentLogs); result.OK {
		t.Fatal("reparse escape accepted")
	}
}

func TestOpenLocationRejectsUnknownKindEvenWhenInjectedMapContainsIt(t *testing.T) {
	s, calls, _, _, _, _ := serviceFixture(t)
	unknown := traymodel.LocationKind("injected-location")
	s.locations[unknown] = Location{Path: `C:\node\agent\logs`, Root: `C:\node\agent`}
	if result := s.OpenLocation(context.Background(), unknown); result.OK || result.ErrorCode != "invalid_location" {
		t.Fatalf("OpenLocation(unknown) = %#v", result)
	}
	if len(*calls) != 0 {
		t.Fatalf("unknown location opened: %v", *calls)
	}
}

func TestOverviewTreatsStaleLoginValueAsDriftWhenDesiredDisabled(t *testing.T) {
	s, _, store, _, _, _ := serviceFixture(t)
	store.settings.LoginStartTray = false
	s.loginStart = &fakeLogin{calls: &[]string{}, enabled: false, current: `"C:\stale\nodetray.exe" --background`}
	overview, err := s.GetOverview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !overview.LoginStartDrift {
		t.Fatalf("stale login value was not reported: %#v", overview)
	}
}

func TestSanitizeOperationCanonicalizesSuccessAndStabilizesFailure(t *testing.T) {
	success := sanitizeOperation(traymodel.OperationResult{OK: true, ErrorCode: "ignored", ErrorSummary: "password=secret", UACCancelled: true})
	if !reflect.DeepEqual(success, traymodel.OperationResult{OK: true}) {
		t.Fatalf("success = %#v", success)
	}
	failure := sanitizeOperation(traymodel.OperationResult{ErrorSummary: "postgres://u:p@db/media"})
	if failure.OK || failure.ErrorCode == "" || strings.Contains(failure.ErrorSummary, "u:p") {
		t.Fatalf("failure = %#v", failure)
	}
}

func TestSaveTraySettingsOrdinaryChangeSkipsLoginWritesAndElevation(t *testing.T) {
	s, calls, _, _, _, elevated := serviceFixture(t)
	value := validSettings()
	value.RefreshIntervalSeconds++

	result := s.SaveTraySettings(context.Background(), value)

	if !result.OK {
		t.Fatalf("SaveTraySettings = %#v", result)
	}
	if !reflect.DeepEqual(*calls, []string{"save-settings"}) {
		t.Fatalf("calls = %v", *calls)
	}
	if len(elevated.actions) != 0 {
		t.Fatalf("elevation calls = %d, want 0", len(elevated.actions))
	}
}

func TestSaveTraySettingsOrdinaryChangeDoesNotInspectTask(t *testing.T) {
	s, calls, store, _, _, elevated := serviceFixture(t)
	task := s.task.(*fakeTask)
	task.err = errors.New("scheduler unavailable")
	value := store.settings
	value.RefreshIntervalSeconds++

	result := s.SaveTraySettings(context.Background(), value)

	if !result.OK || store.settings.RefreshIntervalSeconds != value.RefreshIntervalSeconds {
		t.Fatalf("result=%#v persisted=%#v", result, store.settings)
	}
	if !reflect.DeepEqual(*calls, []string{"save-settings"}) || task.inspectCalls != 0 || len(elevated.actions) != 0 {
		t.Fatalf("calls=%v inspect=%d elevation=%v", *calls, task.inspectCalls, elevated.actions)
	}
}

func TestSaveAgentMapsFormalRereadFailureToStableVerifyCode(t *testing.T) {
	s, _, store, _, _, _ := serviceFixture(t)
	store.saveErr = config.ErrSaveVerify

	result := s.SaveAgent(context.Background(), config.AgentForm{})

	if result.OK || result.ErrorCode != "save_verify_failed" || result.Saved {
		t.Fatalf("result = %#v", result)
	}
}

func TestSaveTraySettingsAppliesOnlyChangedLoginSettingBeforeDiskCommit(t *testing.T) {
	s, calls, _, _, _, elevated := serviceFixture(t)
	value := validSettings()
	value.LoginStartTray = true

	result := s.SaveTraySettings(context.Background(), value)

	if !result.OK || !reflect.DeepEqual(*calls, []string{"login-enable", "save-settings"}) {
		t.Fatalf("result=%#v calls=%v", result, *calls)
	}
	if len(elevated.actions) != 0 {
		t.Fatalf("elevation calls = %d, want 0", len(elevated.actions))
	}
}

func TestSaveTraySettingsHelperPolicyChangeOnlyPersistsSettings(t *testing.T) {
	s, calls, _, _, _, elevated := serviceFixture(t)
	value := validSettings()
	value.HelperStartMode = traymodel.StartAutomatic

	result := s.SaveTraySettings(context.Background(), value)

	if !result.OK || !reflect.DeepEqual(*calls, []string{"save-settings"}) {
		t.Fatalf("result=%#v calls=%v", result, *calls)
	}
	if len(elevated.actions) != 0 {
		t.Fatalf("elevation actions = %v", elevated.actions)
	}
}

func TestSaveTraySettingsManualHelperEnableSkipsTaskRemovalWhenTaskAbsent(t *testing.T) {
	s, calls, store, _, _, elevated := serviceFixture(t)
	store.settings.HelperEnabled = false
	store.settings.HelperStartMode = traymodel.StartManual
	task := s.task.(*fakeTask)
	task.status = nodetask.Status{Installed: false}
	value := store.settings
	value.HelperEnabled = true

	result := s.SaveTraySettings(context.Background(), value)

	if !result.OK || !store.settings.HelperEnabled {
		t.Fatalf("result=%#v persisted=%#v", result, store.settings)
	}
	if !reflect.DeepEqual(*calls, []string{"save-settings"}) {
		t.Fatalf("calls=%v", *calls)
	}
	if task.inspectCalls != 0 || len(elevated.actions) != 0 {
		t.Fatalf("inspect=%d elevation=%v", task.inspectCalls, elevated.actions)
	}
}

func TestSaveTraySettingsHelperTaskAlreadyMatchesTargetSkipsElevation(t *testing.T) {
	s, calls, store, _, _, elevated := serviceFixture(t)
	task := s.task.(*fakeTask)
	task.status = nodetask.Status{Installed: true}
	value := store.settings
	value.HelperStartMode = traymodel.StartAutomatic

	result := s.SaveTraySettings(context.Background(), value)

	if !result.OK || store.settings.HelperStartMode != traymodel.StartAutomatic {
		t.Fatalf("result=%#v persisted=%#v", result, store.settings)
	}
	if !reflect.DeepEqual(*calls, []string{"save-settings"}) || len(elevated.actions) != 0 {
		t.Fatalf("calls=%v elevation=%v", *calls, elevated.actions)
	}
}

func TestSaveTraySettingsIgnoresObsoleteHelperTaskInspectFailure(t *testing.T) {
	s, calls, store, _, _, elevated := serviceFixture(t)
	store.settings.HelperEnabled = false
	task := s.task.(*fakeTask)
	task.err = errors.New("scheduler unavailable")
	value := store.settings
	value.HelperEnabled = true

	result := s.SaveTraySettings(context.Background(), value)

	if !result.OK || !store.settings.HelperEnabled {
		t.Fatalf("result=%#v persisted=%#v", result, store.settings)
	}
	if !reflect.DeepEqual(*calls, []string{"save-settings"}) || len(elevated.actions) != 0 {
		t.Fatalf("calls=%v elevation=%v", *calls, elevated.actions)
	}
}

func TestSaveTraySettingsDoesNotMutateObsoleteHelperTask(t *testing.T) {
	tests := []struct {
		name    string
		current traymodel.TraySettings
		value   traymodel.TraySettings
	}{
		{
			name: "automatic to manual",
			current: traymodel.TraySettings{
				AgentStartMode: traymodel.StartManual, HelperEnabled: true, HelperStartMode: traymodel.StartAutomatic,
				RefreshIntervalSeconds: 2, NotificationLevel: traymodel.NotifyImportant,
			},
			value: validSettings(),
		},
		{
			name:    "enabled to disabled",
			current: validSettings(),
			value: traymodel.TraySettings{
				AgentStartMode: traymodel.StartManual, HelperEnabled: false, HelperStartMode: traymodel.StartManual,
				RefreshIntervalSeconds: 2, NotificationLevel: traymodel.NotifyImportant,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, calls, store, _, _, elevated := serviceFixture(t)
			store.settings = tt.current
			task := s.task.(*fakeTask)
			task.status = nodetask.Status{Installed: true}

			result := s.SaveTraySettings(context.Background(), tt.value)

			if !result.OK || !reflect.DeepEqual(store.settings, tt.value) {
				t.Fatalf("result=%#v persisted=%#v, want %#v", result, store.settings, tt.value)
			}
			if !reflect.DeepEqual(*calls, []string{"save-settings"}) {
				t.Fatalf("calls=%v", *calls)
			}
			if len(elevated.actions) != 0 {
				t.Fatalf("elevation actions=%v", elevated.actions)
			}
		})
	}
}

func TestSaveTraySettingsDoesNotRequestUAC(t *testing.T) {
	s, calls, store, _, _, elevated := serviceFixture(t)
	elevated.result = elevation.InvocationResult{UACCancelled: true}
	value := validSettings()
	value.HelperStartMode = traymodel.StartAutomatic

	result := s.SaveTraySettings(context.Background(), value)

	if !result.OK || result.UACCancelled {
		t.Fatalf("result = %#v", result)
	}
	if !reflect.DeepEqual(*calls, []string{"save-settings"}) || store.settings.HelperStartMode != traymodel.StartAutomatic || len(elevated.actions) != 0 {
		t.Fatalf("calls=%v persisted=%#v", *calls, store.settings)
	}
	if store.loadCalls != 2 {
		t.Fatalf("settings loads = %d, want initial and verification loads", store.loadCalls)
	}
}

func TestSaveTraySettingsDoesNotReadOrImportHelperDefaults(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*fakeStore, *fakeElevation)
	}{
		{"invalid", func(store *fakeStore, _ *fakeElevation) { store.defaultErr = errors.New("invalid") }},
		{"uac", func(store *fakeStore, elevated *fakeElevation) {
			store.defaultErr = nil
			elevated.result = elevation.InvocationResult{UACCancelled: true}
		}},
		{"write", func(store *fakeStore, elevated *fakeElevation) {
			store.defaultErr = nil
			elevated.result.Response = elevation.Response{OK: false, ErrorCode: elevation.ErrorCodeWriteFailed, ErrorSummary: "configuration write failed"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, calls, store, _, _, elevated := serviceFixture(t)
			store.settings.HelperEnabled = false
			store.settings.HelperStartMode = traymodel.StartManual
			test.setup(store, elevated)
			value := store.settings
			value.HelperEnabled = true
			value.HelperStartMode = traymodel.StartAutomatic
			result := s.SaveTraySettings(context.Background(), value)
			if !result.OK {
				t.Fatalf("result = %#v", result)
			}
			if !store.settings.HelperEnabled || store.settings.HelperStartMode != traymodel.StartAutomatic {
				t.Fatalf("settings saved: %#v", store.settings)
			}
			joined := strings.Join(*calls, ",")
			if joined != "save-settings" || len(elevated.actions) != 0 {
				t.Fatalf("unsafe calls: %v", *calls)
			}
		})
	}
}

func TestSaveTraySettingsLateFailureReturnsPartiallyAppliedAndReloadsActualState(t *testing.T) {
	s, calls, store, _, _, _ := serviceFixture(t)
	store.saveErr = errors.New("disk unavailable")
	value := validSettings()
	value.LoginStartTray = true

	result := s.SaveTraySettings(context.Background(), value)

	if result.OK || result.ErrorCode != "settings_partially_applied" {
		t.Fatalf("result = %#v", result)
	}
	if !reflect.DeepEqual(*calls, []string{"login-enable", "save-settings"}) {
		t.Fatalf("calls = %v", *calls)
	}
	if store.loadCalls != 2 {
		t.Fatalf("settings loads = %d, want initial and actual-state reload", store.loadCalls)
	}
}

func TestEventBusMergesSameComponentStateAndNeverBlocksSlowSubscriber(t *testing.T) {
	bus := NewEventBus(1)
	stream, cancel := bus.Subscribe(1)
	defer cancel()
	first := Event{Type: EventComponentState, ComponentState: &ComponentStateEvent{Component: "agent", State: traymodel.ComponentState{Lifecycle: traymodel.Starting}}}
	latest := Event{Type: EventComponentState, ComponentState: &ComponentStateEvent{Component: "agent", State: traymodel.ComponentState{Lifecycle: traymodel.Running}}}
	if !bus.Publish(first) || !bus.Publish(latest) {
		t.Fatal("component state was not accepted")
	}
	done := make(chan struct{})
	go func() { bus.Publish(latest); close(done) }()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("slow subscriber blocked publisher")
	}
	got := <-stream
	if got.ComponentState == nil || got.ComponentState.State.Lifecycle != traymodel.Running {
		t.Fatalf("merged event = %#v", got)
	}
}

func TestEventBusReportsDroppedNonStateEventsAndCloseIsIdempotent(t *testing.T) {
	bus := NewEventBus(1)
	stream, cancel := bus.Subscribe(1)
	event := Event{Type: EventAttentionRequired, AttentionRequired: &AttentionRequiredEvent{Component: "helper", Code: "bad", Summary: "password=secret\r\n"}}
	if !bus.Publish(event) {
		t.Fatal("first attention event rejected")
	}
	if bus.Publish(event) {
		t.Fatal("full queue silently accepted a dropped attention event")
	}
	got := <-stream
	if strings.Contains(got.AttentionRequired.Summary, "secret") || strings.ContainsAny(got.AttentionRequired.Summary, "\r\n") {
		t.Fatalf("event leaked text: %#v", got)
	}
	cancel()
	cancel()
	bus.Close()
	bus.Close()
	if _, ok := <-stream; ok {
		t.Fatal("subscription channel remained open")
	}
}

func TestEventBusRejectsUnknownOrMismatchedTypedPayload(t *testing.T) {
	bus := NewEventBus(1)
	defer bus.Close()
	if bus.Publish(Event{Type: EventType("arbitrary")}) {
		t.Fatal("unknown event accepted")
	}
	if bus.Publish(Event{Type: EventSettingsChanged, OperationProgress: &OperationProgressEvent{Operation: "save", Summary: "ok"}}) {
		t.Fatal("mismatched payload accepted")
	}
}

func TestEventBusPublishRequiresAtLeastOneSubscriberAcceptance(t *testing.T) {
	empty := NewEventBus(1)
	if empty.Publish(Event{Type: EventSettingsChanged, SettingsChanged: &SettingsChangedEvent{Summary: "saved"}}) {
		t.Fatal("zero-subscriber publish reported acceptance")
	}

	bus := NewEventBus(1)
	blocked, cancelBlocked := bus.Subscribe(1)
	free, cancelFree := bus.Subscribe(1)
	defer cancelBlocked()
	defer cancelFree()
	attention := Event{Type: EventAttentionRequired, AttentionRequired: &AttentionRequiredEvent{Component: "agent", Code: "failed", Summary: "failed"}}
	if !bus.Publish(attention) {
		t.Fatal("initial publish was not accepted")
	}
	_ = blocked
	<-free
	if !bus.Publish(attention) {
		t.Fatal("one accepting subscriber was masked by one full subscriber")
	}
}

func TestEventBusRetriesLatestStateWhenSubscriberConsumesDuringReplacement(t *testing.T) {
	bus := NewEventBus(1)
	stream, cancel := bus.Subscribe(1)
	defer cancel()
	starting := Event{Type: EventComponentState, ComponentState: &ComponentStateEvent{Component: "agent", State: traymodel.ComponentState{Lifecycle: traymodel.Starting}}}
	running := Event{Type: EventComponentState, ComponentState: &ComponentStateEvent{Component: "agent", State: traymodel.ComponentState{Lifecycle: traymodel.Running}}}
	if !bus.Publish(starting) {
		t.Fatal("starting state was not accepted")
	}
	bus.testHooks.beforeReplace = func(channel chan Event) { <-channel }
	if !bus.Publish(running) {
		t.Fatal("latest state was dropped during concurrent consume")
	}
	got := <-stream
	if got.ComponentState == nil || got.ComponentState.State.Lifecycle != traymodel.Running {
		t.Fatalf("latest event = %#v", got)
	}
}
