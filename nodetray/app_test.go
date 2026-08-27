package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	trayapp "dedup/internal/nodetray/app"
	"dedup/internal/nodetray/bootstrap"
	trayconfig "dedup/internal/nodetray/config"
	"dedup/internal/nodetray/traymodel"
	"dedup/internal/nodetray/windows/elevation"
	nodetask "dedup/internal/nodetray/windows/task"
	traynative "dedup/internal/nodetray/windows/tray"
	"dedup/internal/proto"
	"github.com/vmihailenco/msgpack/v5"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

type backendTestRecorder struct {
	mu    sync.Mutex
	calls []string
}

type backendControlGateway struct {
	operation string
	control   proto.LocalTaskControlRequest
}

func (f *backendControlGateway) CallLocal(_ context.Context, operation string, request, response any) error {
	f.operation = operation
	control, ok := request.(proto.LocalTaskControlRequest)
	if !ok {
		return errors.New("unexpected_control_request")
	}
	f.control = control
	raw, err := msgpack.Marshal(encodedBackendTaskControlResponse())
	if err != nil {
		return err
	}
	return msgpack.Unmarshal(raw, response)
}

func encodedBackendTaskControlResponse() proto.LocalTaskControlResponse {
	return proto.LocalTaskControlResponse{Task: &proto.LocalTask{TaskID: "task-1", InstanceID: "instance-1", Revision: 8, Phase: "analysis", ProgressTotalKnown: true, CreatedAt: 100, UpdatedAt: 200, StartedAt: 110}}
}

// Break caught: a Wails-exposed task control wrapper calls a stale operation or
// drops the versioned identity before it reaches the NodeTray service.
func TestBackendLocalTaskControlsForwardVersionedRequest(t *testing.T) {
	operations := []struct {
		name      string
		operation string
		call      func(*Backend, traymodel.LocalTaskControl) traymodel.LocalTaskResult
	}{
		{"pause", proto.LocalOperationTaskPause, (*Backend).PauseLocalTask},
		{"resume", proto.LocalOperationTaskResume, (*Backend).ResumeLocalTask},
		{"cancel", proto.LocalOperationTaskCancel, (*Backend).CancelLocalTask},
		{"delete", proto.LocalOperationTaskDelete, (*Backend).DeleteLocalTask},
		{"retry", proto.LocalOperationTaskRetry, (*Backend).RetryLocalTask},
	}
	for _, test := range operations {
		t.Run(test.name, func(t *testing.T) {
			gateway := &backendControlGateway{}
			backend := NewBackend(trayapp.NewService(trayapp.Dependencies{LocalAgent: gateway}))
			backend.Startup(context.Background())
			t.Cleanup(func() { _ = backend.Shutdown(context.Background()) })

			result := test.call(backend, traymodel.LocalTaskControl{TaskID: "task-1", InstanceID: "instance-1", ExpectedRevision: 7})
			if !result.OK || result.Task.Revision != 8 || result.Task.Phase != "analysis" {
				t.Fatalf("result=%#v", result)
			}
			if gateway.operation != test.operation || gateway.control != (proto.LocalTaskControlRequest{TaskID: "task-1", InstanceID: "instance-1", ExpectedRevision: 7}) {
				t.Fatalf("operation=%q control=%#v", gateway.operation, gateway.control)
			}
		})
	}
}

// Break caught: a valid current task directory is not forwarded to the native
// picker, so users must navigate back to it every time they add a root.
func TestChooseLocalTaskRootUsesWindowsDirectoryDialog(t *testing.T) {
	currentPath := t.TempDir()
	selectedPath := filepath.Join(currentPath, "Photos")
	if err := os.Mkdir(selectedPath, 0o755); err != nil {
		t.Fatal(err)
	}
	original := openDirectoryDialogAdapter
	t.Cleanup(func() { openDirectoryDialogAdapter = original })
	openDirectoryDialogAdapter = func(_ context.Context, options runtime.OpenDialogOptions) (string, error) {
		if options.Title != "选择本地任务扫描目录" || options.DefaultDirectory != currentPath {
			t.Fatalf("options=%#v", options)
		}
		return selectedPath, nil
	}

	backend := NewBackend(nil)
	backend.Startup(context.Background())
	result := backend.ChooseLocalTaskRoot(currentPath)
	if !result.OK || result.Cancelled || result.Path != selectedPath {
		t.Fatalf("result=%#v", result)
	}
}

// Break caught: an unexpected non-directory result from the native boundary
// reaches the frontend and can be submitted as a scan root.
func TestChooseLocalTaskRootRejectsNonDirectorySelection(t *testing.T) {
	selected, err := os.CreateTemp(t.TempDir(), "selected-file")
	if err != nil {
		t.Fatal(err)
	}
	if err := selected.Close(); err != nil {
		t.Fatal(err)
	}
	original := openDirectoryDialogAdapter
	t.Cleanup(func() { openDirectoryDialogAdapter = original })
	openDirectoryDialogAdapter = func(context.Context, runtime.OpenDialogOptions) (string, error) { return selected.Name(), nil }

	backend := NewBackend(nil)
	backend.Startup(context.Background())
	result := backend.ChooseLocalTaskRoot("")
	if result.OK || result.Cancelled || result.Path != "" || result.ErrorCode != "directory_dialog_failed" {
		t.Fatalf("result=%#v", result)
	}
}

// Break caught: closing the native picker changes the pending roots or is
// reported as a failure instead of an explicit successful cancellation.
func TestChooseLocalTaskRootReturnsCancelledForEmptyNativeSelection(t *testing.T) {
	original := openDirectoryDialogAdapter
	t.Cleanup(func() { openDirectoryDialogAdapter = original })
	openDirectoryDialogAdapter = func(context.Context, runtime.OpenDialogOptions) (string, error) { return "", nil }

	backend := NewBackend(nil)
	backend.Startup(context.Background())
	result := backend.ChooseLocalTaskRoot("")
	if !result.OK || !result.Cancelled || result.Path != "" || result.ErrorCode != "" {
		t.Fatalf("result=%#v", result)
	}
}

// Break caught: an operating-system picker error exposes a private filesystem
// path or raw error to the WebView instead of a stable display-safe failure.
func TestChooseLocalTaskRootRedactsDirectoryDialogFailure(t *testing.T) {
	original := openDirectoryDialogAdapter
	t.Cleanup(func() { openDirectoryDialogAdapter = original })
	openDirectoryDialogAdapter = func(_ context.Context, options runtime.OpenDialogOptions) (string, error) {
		if options.DefaultDirectory != "" {
			t.Fatalf("non-directory current path became default directory: %#v", options)
		}
		return "", errors.New(`open C:\private\media: access denied`)
	}

	file, err := os.CreateTemp(t.TempDir(), "not-a-directory")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	backend := NewBackend(nil)
	backend.Startup(context.Background())
	result := backend.ChooseLocalTaskRoot(file.Name())
	if result.OK || result.Cancelled || result.ErrorCode != "directory_dialog_failed" || result.ErrorSummary == "" {
		t.Fatalf("result=%#v", result)
	}
	serialized, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(serialized)), "private") || strings.Contains(strings.ToLower(string(serialized)), "access denied") {
		t.Fatalf("result leaked directory dialog error: %s", serialized)
	}
}

func (r *backendTestRecorder) add(value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, value)
}

func (r *backendTestRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

type backendTestStore struct {
	recorder    *backendTestRecorder
	settings    traymodel.TraySettings
	settingsErr error
	helper      trayconfig.HelperForm
}

func (s *backendTestStore) LoadTraySettings() (traymodel.TraySettings, error) {
	s.recorder.add("load-settings")
	return s.settings, s.settingsErr
}

func (s *backendTestStore) SaveTraySettings(traymodel.TraySettings) error {
	s.recorder.add("save-settings")
	return nil
}

func (s *backendTestStore) LoadHelperForm() (trayconfig.HelperForm, error) {
	s.recorder.add("load-helper")
	return s.helper, nil
}
func (s *backendTestStore) ValidateHelperForm(trayconfig.HelperForm) []trayconfig.FieldError {
	s.recorder.add("validate-helper")
	return nil
}
func (s *backendTestStore) SaveHelperForm(trayconfig.HelperForm) (string, error) {
	s.recorder.add("save-helper")
	return strings.Repeat("b", 64), nil
}

type backendTestAgentConfig struct {
	recorder *backendTestRecorder
	form     trayconfig.AgentForm
}

func (g *backendTestAgentConfig) LoadAgentForm(context.Context) (trayconfig.AgentForm, error) {
	g.recorder.add("load-agent")
	return g.form, nil
}
func (g *backendTestAgentConfig) ValidateAgentForm(context.Context, trayconfig.AgentForm) []trayconfig.FieldError {
	g.recorder.add("validate-agent")
	return nil
}
func (g *backendTestAgentConfig) SaveAgentForm(context.Context, trayconfig.AgentForm) (trayapp.AgentConfigSaveResult, error) {
	g.recorder.add("save-agent")
	return trayapp.AgentConfigSaveResult{SHA256: strings.Repeat("a", 64)}, nil
}
func (g *backendTestAgentConfig) PromotePendingEndpoint() { g.recorder.add("promote-agent-endpoint") }

func (s *backendTestStore) PrepareHelperWrite(trayconfig.HelperForm) (trayconfig.PreparedWrite, error) {
	s.recorder.add("prepare-helper")
	return trayconfig.PreparedWrite{
		TargetPath:    `C:\ProgramData\MySingerServer\helper.json`,
		CanonicalJSON: []byte("{}\n"),
		SHA256:        strings.Repeat("b", 64),
	}, nil
}

func (*backendTestStore) PrepareDefaultHelperWrite() (trayconfig.PreparedWrite, error) {
	return trayconfig.PreparedWrite{}, trayconfig.ErrHelperConfigExists
}
func (*backendTestStore) HelperFingerprint() (string, error) { return strings.Repeat("b", 64), nil }

type backendTestValidator struct{ recorder *backendTestRecorder }

func (v backendTestValidator) ValidateHelper(trayconfig.HelperForm) []trayconfig.FieldError {
	v.recorder.add("validate-helper")
	return nil
}

type backendTestComponent struct {
	name         string
	recorder     *backendTestRecorder
	seenCtx      chan context.Context
	refreshCtx   chan context.Context
	blockRefresh bool
	results      map[string]traymodel.OperationResult
}

func (c *backendTestComponent) result(ctx context.Context, operation string) traymodel.OperationResult {
	c.recorder.add(c.name + "-" + operation)
	if c.seenCtx != nil {
		select {
		case c.seenCtx <- ctx:
		default:
		}
	}
	if result, ok := c.results[operation]; ok {
		return result
	}
	return traymodel.OperationResult{OK: true}
}

func (c *backendTestComponent) Start(ctx context.Context) traymodel.OperationResult {
	return c.result(ctx, "start")
}

func (c *backendTestComponent) Stop(ctx context.Context) traymodel.OperationResult {
	return c.result(ctx, "stop")
}

func (c *backendTestComponent) Restart(ctx context.Context) traymodel.OperationResult {
	return c.result(ctx, "restart")
}

func (c *backendTestComponent) ForceStopTracked(ctx context.Context) traymodel.OperationResult {
	return c.result(ctx, "force")
}

func (c *backendTestComponent) Refresh(ctx context.Context) traymodel.ComponentState {
	c.recorder.add(c.name + "-refresh")
	if c.refreshCtx != nil {
		select {
		case c.refreshCtx <- ctx:
		default:
		}
	}
	if c.blockRefresh {
		<-ctx.Done()
	}
	return traymodel.ComponentState{Lifecycle: traymodel.Running, Healthy: true}
}

type backendTestTask struct{ recorder *backendTestRecorder }

func (t backendTestTask) Inspect(context.Context) (nodetask.Status, error) {
	t.recorder.add("task-inspect")
	return nodetask.Status{}, nil
}

func (t backendTestTask) Run(context.Context) error {
	t.recorder.add("task-run")
	return nil
}

type backendTestElevation struct{ recorder *backendTestRecorder }

func (e backendTestElevation) Invoke(_ context.Context, action elevation.Action, _ []byte) (elevation.InvocationResult, error) {
	e.recorder.add("elevate-" + string(action))
	return elevation.InvocationResult{Response: elevation.Response{OK: true}}, nil
}

type backendTestLoginStart struct{ recorder *backendTestRecorder }

func (l backendTestLoginStart) Enabled() (bool, string, error) {
	l.recorder.add("login-enabled")
	return false, "", nil
}

func (l backendTestLoginStart) Enable(string) error {
	l.recorder.add("login-enable")
	return nil
}

func (l backendTestLoginStart) Disable() error {
	l.recorder.add("login-disable")
	return nil
}

type backendTestResolver struct{}

func (backendTestResolver) Final(value string) (string, error) { return filepath.Clean(value), nil }

type backendTestOpener struct{ recorder *backendTestRecorder }

func (o backendTestOpener) Open(context.Context, string) error {
	o.recorder.add("open-location")
	return nil
}

type backendTestWorkers struct{ recorder *backendTestRecorder }

func (w backendTestWorkers) Snapshot(context.Context) ([]traymodel.WorkerState, error) {
	w.recorder.add("workers-snapshot")
	return []traymodel.WorkerState{{Index: 0, Ready: true}}, nil
}

type backendTestUpdater struct{}

func (backendTestUpdater) UpdateExpectedSHA256(string) traymodel.OperationResult {
	return traymodel.OperationResult{OK: true}
}
func (backendTestUpdater) UpdateExpectedMachineID(string) traymodel.OperationResult {
	return traymodel.OperationResult{OK: true}
}

type fakeTrayController struct {
	close  func() error
	notify func(traynative.Event) (bool, error)
}

type fakeTrayMonitorTicker struct {
	ch      chan time.Time
	stopped int
}

func (t *fakeTrayMonitorTicker) C() <-chan time.Time { return t.ch }
func (t *fakeTrayMonitorTicker) Stop()               { t.stopped++ }

func useFakeTrayMonitorTicker(t *testing.T) *fakeTrayMonitorTicker {
	t.Helper()
	original := trayMonitorTickerAdapter
	ticker := &fakeTrayMonitorTicker{ch: make(chan time.Time)}
	trayMonitorTickerAdapter = func(time.Duration) trayMonitorTicker { return ticker }
	t.Cleanup(func() { trayMonitorTickerAdapter = original })
	return ticker
}

func (f *fakeTrayController) Close() error {
	if f.close != nil {
		return f.close()
	}
	return nil
}

func (f *fakeTrayController) Notify(event traynative.Event) (bool, error) {
	if f.notify != nil {
		return f.notify(event)
	}
	return true, nil
}

func backendTestSettings() traymodel.TraySettings {
	return traymodel.TraySettings{
		AgentStartMode:         traymodel.StartManual,
		HelperEnabled:          true,
		HelperStartMode:        traymodel.StartManual,
		CloseToTray:            true,
		RefreshIntervalSeconds: 2,
		NotificationLevel:      traymodel.NotifyImportant,
	}
}

func newBackendTestService(t *testing.T) (*trayapp.Service, *backendTestRecorder, *backendTestComponent) {
	t.Helper()
	recorder := &backendTestRecorder{}
	store := &backendTestStore{
		recorder: recorder,
		settings: backendTestSettings(),
		helper:   trayconfig.HelperForm{PipeName: "helper-pipe"},
	}
	agentConfig := &backendTestAgentConfig{recorder: recorder}
	agent := &backendTestComponent{name: "agent", recorder: recorder}
	helper := &backendTestComponent{name: "helper", recorder: recorder}
	root := t.TempDir()
	return trayapp.NewService(trayapp.Dependencies{
		Store:             store,
		Validator:         backendTestValidator{recorder: recorder},
		AgentConfig:       agentConfig,
		MachineID:         "node-" + strings.Repeat("1", 64),
		Agent:             agent,
		Helper:            helper,
		AgentFingerprint:  backendTestUpdater{},
		HelperFingerprint: backendTestUpdater{},
		Task:              backendTestTask{recorder: recorder},
		Elevation:         backendTestElevation{recorder: recorder},
		LoginStart:        backendTestLoginStart{recorder: recorder},
		TrayExecutable:    filepath.Join(root, "nodetray.exe"),
		TaskDefinition: nodetask.Definition{
			HelperExecutable: filepath.Join(root, "helper.exe"),
			HelperConfig:     filepath.Join(root, "helper.json"),
			UserSID:          "S-1-5-21-1",
		},
		Locations: map[traymodel.LocationKind]trayapp.Location{
			traymodel.AgentLogs: {Path: filepath.Join(root, "agent", "logs"), Root: filepath.Join(root, "agent")},
		},
		PathResolver: backendTestResolver{},
		Opener:       backendTestOpener{recorder: recorder},
		Workers:      backendTestWorkers{recorder: recorder},
	}), recorder, agent
}

func requirePureSuccess(t *testing.T, result traymodel.OperationResult) {
	t.Helper()
	if !result.OK || result.ErrorCode != "" || result.ErrorSummary != "" || result.UACCancelled {
		t.Fatalf("success result leaked metadata: %+v", result)
	}
}

func requireConfigSuccess(t *testing.T, result traymodel.ConfigApplyResult, restarted bool) {
	t.Helper()
	if !result.OK || !result.Saved || result.Restarted != restarted || len(result.SHA256) != 64 ||
		result.ErrorCode != "" || result.ErrorSummary != "" {
		t.Fatalf("config result = %+v, want saved=%v restarted=%v with SHA-256", result, true, restarted)
	}
}

func TestBackendFailsClosedBeforeStartupAndAfterShutdown(t *testing.T) {
	service, _, _ := newBackendTestService(t)
	backend := NewBackend(service)

	if _, err := backend.GetOverview(); err == nil || err.Error() != "backend_not_started" {
		t.Fatalf("GetOverview before Startup error = %v", err)
	}
	if result := backend.StartAgent(); result.OK || result.ErrorCode != "backend_not_started" || result.ErrorSummary != "" {
		t.Fatalf("StartAgent before Startup = %+v", result)
	}
	if got := backend.ValidateAgent(trayconfig.AgentForm{}); !reflect.DeepEqual(got, []trayconfig.FieldError{{Field: "backend", Code: "backend_not_started", Message: "后端尚未启动"}}) {
		t.Fatalf("ValidateAgent before Startup = %#v", got)
	}

	backend.Startup(context.Background())
	backend.Shutdown(context.Background())
	if _, err := backend.GetTraySettings(); err == nil || err.Error() != "backend_not_started" {
		t.Fatalf("GetTraySettings after Shutdown error = %v", err)
	}
	if result := backend.ForceExitAll(); result.OK || result.ErrorCode != "backend_not_started" ||
		!reflect.DeepEqual(result.FailedComponents, []string{"service"}) {
		t.Fatalf("ForceExitAll after Shutdown = %+v", result)
	}
}

func TestBackendForwardsEveryPublicOperationExactlyOnce(t *testing.T) {
	service, recorder, _ := newBackendTestService(t)
	backend := NewBackend(service)
	backend.Startup(context.Background())
	t.Cleanup(func() { backend.Shutdown(context.Background()) })

	if _, err := backend.GetOverview(); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.GetAgentForm(); err != nil {
		t.Fatal(err)
	}
	if got := backend.ValidateAgent(trayconfig.AgentForm{}); len(got) != 0 {
		t.Fatalf("ValidateAgent = %#v", got)
	}
	requireConfigSuccess(t, backend.SaveAgent(trayconfig.AgentForm{}), false)
	requireConfigSuccess(t, backend.SaveAndRestartAgent(trayconfig.AgentForm{}), true)
	requirePureSuccess(t, backend.StartAgent())
	requirePureSuccess(t, backend.StopAgent())
	requirePureSuccess(t, backend.RestartAgent())
	requirePureSuccess(t, backend.ForceStopAgent())
	if _, err := backend.GetHelperForm(); err != nil {
		t.Fatal(err)
	}
	if got := backend.ValidateHelper(trayconfig.HelperForm{}); len(got) != 0 {
		t.Fatalf("ValidateHelper = %#v", got)
	}
	requireConfigSuccess(t, backend.SaveHelper(trayconfig.HelperForm{}), false)
	requirePureSuccess(t, backend.StartHelper())
	requirePureSuccess(t, backend.StopHelper())
	requirePureSuccess(t, backend.RestartHelper())
	requirePureSuccess(t, backend.ForceStopHelper())
	if _, err := backend.GetTraySettings(); err != nil {
		t.Fatal(err)
	}
	settings := backendTestSettings()
	requirePureSuccess(t, backend.SaveTraySettings(settings))
	requirePureSuccess(t, backend.OpenLocation(traymodel.AgentLogs))

	wantCounts := map[string]int{
		"load-settings": 7, "load-agent": 1, "load-helper": 3,
		"agent-refresh": 3, "helper-refresh": 2, "workers-snapshot": 1,
		"login-enabled":  2,
		"validate-agent": 3, "save-agent": 2,
		"promote-agent-endpoint": 2,
		"agent-start":            2, "agent-stop": 2, "agent-restart": 1, "agent-force": 1,
		"validate-helper": 4, "save-helper": 1,
		"helper-start": 1, "helper-stop": 1, "helper-restart": 1, "helper-force": 1,
		"save-settings": 1,
		"open-location": 1,
	}
	gotCounts := make(map[string]int)
	for _, call := range recorder.snapshot() {
		gotCounts[call]++
	}
	if !reflect.DeepEqual(gotCounts, wantCounts) {
		t.Fatalf("forwarding calls = %#v, want %#v", gotCounts, wantCounts)
	}
}

func TestBackendUsesStartupContextAndShutdownCancelsInflightCallWithoutStoppingComponents(t *testing.T) {
	service, recorder, agent := newBackendTestService(t)
	seen := make(chan context.Context, 1)
	agent.seenCtx = seen
	backend := NewBackend(service)
	type markerKey struct{}
	startupContext := context.WithValue(context.Background(), markerKey{}, "wails")
	backend.Startup(startupContext)

	requirePureSuccess(t, backend.StartAgent())
	forwarded := <-seen
	if got := forwarded.Value(markerKey{}); got != "wails" {
		t.Fatalf("forwarded context marker = %v", got)
	}

	backend.Shutdown(context.Background())
	select {
	case <-forwarded.Done():
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not cancel the adapter context")
	}
	for _, call := range recorder.snapshot() {
		if call == "agent-stop" || call == "helper-stop" || call == "agent-force" || call == "helper-force" {
			t.Fatalf("Shutdown controlled a component: %q", call)
		}
	}
}

func TestBackendContextRetainsObservableCancellationAfterDeactivate(t *testing.T) {
	state := &backendContext{}
	state.activate(context.Background())
	done := state.Done()
	if done == nil {
		t.Fatal("active backend context has nil Done")
	}
	state.deactivate()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("deactivate did not close the existing Done channel")
	}
	if state.Done() == nil || !errors.Is(state.Err(), context.Canceled) {
		t.Fatalf("cancelled wrapper lost observable state: Done=%v Err=%v", state.Done(), state.Err())
	}
	if state.snapshot() != nil {
		t.Fatal("cancelled context remained ready")
	}
}

func TestBackendStartupShutdownAreConcurrentAndIdempotent(t *testing.T) {
	service, _, _ := newBackendTestService(t)
	backend := NewBackend(service)
	var group sync.WaitGroup
	for index := 0; index < 32; index++ {
		group.Add(2)
		go func() {
			defer group.Done()
			backend.Startup(context.Background())
		}()
		go func() {
			defer group.Done()
			backend.Shutdown(context.Background())
		}()
	}
	group.Wait()
	backend.Shutdown(context.Background())
	if result := backend.StartAgent(); result.OK || result.ErrorCode != "backend_not_started" {
		t.Fatalf("backend remained active after final Shutdown: %+v", result)
	}
}

func TestCloseToTrayHidesWithoutExitOrEvent(t *testing.T) {
	service, recorder, _ := newBackendTestService(t)
	backend := NewBackend(service)
	backend.Startup(context.Background())

	originalHide, originalEmit := windowHideAdapter, eventsEmitAdapter
	t.Cleanup(func() { windowHideAdapter, eventsEmitAdapter = originalHide, originalEmit })
	hidden := 0
	emitted := 0
	windowHideAdapter = func(context.Context) { hidden++ }
	eventsEmitAdapter = func(context.Context, string, ...interface{}) { emitted++ }

	if prevent := backend.onBeforeClose(context.Background()); !prevent {
		t.Fatal("close was not prevented")
	}
	if hidden != 1 || emitted != 0 {
		t.Fatalf("hidden=%d emitted=%d", hidden, emitted)
	}
	for _, call := range recorder.snapshot() {
		if call == "exit" || strings.Contains(call, "-stop") || strings.Contains(call, "-force") {
			t.Fatalf("close controlled process lifecycle: %q", call)
		}
	}
}

func TestCloseWithoutCloseToTrayEmitsRequestWithoutExit(t *testing.T) {
	service, recorder, _ := newBackendTestServiceWithSettings(t, backendTestSettingsWithClose(false), nil)
	backend := NewBackend(service)
	backend.Startup(context.Background())

	originalHide, originalEmit := windowHideAdapter, eventsEmitAdapter
	t.Cleanup(func() { windowHideAdapter, eventsEmitAdapter = originalHide, originalEmit })
	hidden := 0
	var event string
	windowHideAdapter = func(context.Context) { hidden++ }
	eventsEmitAdapter = func(_ context.Context, name string, _ ...interface{}) { event = name }

	if prevent := backend.onBeforeClose(context.Background()); !prevent {
		t.Fatal("close was not prevented")
	}
	if hidden != 0 || event != "window-close-requested" {
		t.Fatalf("hidden=%d event=%q", hidden, event)
	}
	for _, call := range recorder.snapshot() {
		if call == "exit" || strings.Contains(call, "-stop") || strings.Contains(call, "-force") {
			t.Fatalf("close controlled process lifecycle: %q", call)
		}
	}
}

func TestForceExitAllAuthorizesWailsQuitOnlyAfterBackgroundSuccess(t *testing.T) {
	service, recorder, _ := newBackendTestService(t)
	backend := NewBackend(service)
	backend.Startup(context.Background())
	quitCalls := 0
	backend.quit = func(context.Context) { quitCalls++ }

	result := backend.ForceExitAll()

	if !result.OK || quitCalls != 1 {
		t.Fatalf("ForceExitAll = %#v quitCalls=%d", result, quitCalls)
	}
	if prevent := backend.onBeforeClose(context.Background()); prevent {
		t.Fatal("authorized Wails quit was prevented")
	}
	want := []string{"workers-snapshot", "helper-force", "agent-force"}
	if got := recorder.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

func TestForceExitAllFailureKeepsUIOpen(t *testing.T) {
	originalHide := windowHideAdapter
	t.Cleanup(func() { windowHideAdapter = originalHide })
	windowHideAdapter = func(context.Context) {}
	service, recorder, agent := newBackendTestService(t)
	agent.results = map[string]traymodel.OperationResult{}
	agent.results["force"] = traymodel.OperationResult{ErrorCode: "force_exit_failed", ErrorSummary: "still alive"}
	backend := NewBackend(service)
	backend.Startup(context.Background())
	quitCalls := 0
	backend.quit = func(context.Context) { quitCalls++ }

	result := backend.ForceExitAll()

	if result.OK || quitCalls != 0 || !backend.onBeforeClose(context.Background()) {
		t.Fatalf("ForceExitAll = %#v quitCalls=%d", result, quitCalls)
	}
	if got := recorder.snapshot(); !reflect.DeepEqual(got[:3], []string{"workers-snapshot", "helper-force", "agent-force"}) {
		t.Fatalf("calls = %v", got)
	}
}

func TestCloseFailsClosedToHideWhenBackendOrSettingsUnavailable(t *testing.T) {
	originalHide, originalEmit := windowHideAdapter, eventsEmitAdapter
	t.Cleanup(func() { windowHideAdapter, eventsEmitAdapter = originalHide, originalEmit })
	hidden := 0
	emitted := 0
	windowHideAdapter = func(context.Context) { hidden++ }
	eventsEmitAdapter = func(context.Context, string, ...interface{}) { emitted++ }

	if !NewBackend(nil).onBeforeClose(context.Background()) {
		t.Fatal("unstarted close was not prevented")
	}
	service, _, _ := newBackendTestServiceWithSettings(t, backendTestSettings(), errors.New("settings unavailable"))
	backend := NewBackend(service)
	backend.Startup(context.Background())
	if !backend.onBeforeClose(context.Background()) {
		t.Fatal("settings failure close was not prevented")
	}
	if hidden != 2 || emitted != 0 {
		t.Fatalf("hidden=%d emitted=%d", hidden, emitted)
	}
}

func backendTestSettingsWithClose(closeToTray bool) traymodel.TraySettings {
	value := backendTestSettings()
	value.CloseToTray = closeToTray
	return value
}

func newBackendTestServiceWithSettings(t *testing.T, settings traymodel.TraySettings, settingsErr error) (*trayapp.Service, *backendTestRecorder, *backendTestComponent) {
	t.Helper()
	recorder := &backendTestRecorder{}
	store := &backendTestStore{recorder: recorder, settings: settings, settingsErr: settingsErr, helper: trayconfig.HelperForm{PipeName: "helper-pipe"}}
	agentConfig := &backendTestAgentConfig{recorder: recorder}
	agent := &backendTestComponent{name: "agent", recorder: recorder}
	helper := &backendTestComponent{name: "helper", recorder: recorder}
	root := t.TempDir()
	return trayapp.NewService(trayapp.Dependencies{
		Store: store, Validator: backendTestValidator{recorder: recorder}, AgentConfig: agentConfig, Agent: agent, Helper: helper,
		MachineID: "node-" + strings.Repeat("1", 64), AgentFingerprint: backendTestUpdater{}, HelperFingerprint: backendTestUpdater{},
		Task: backendTestTask{recorder: recorder}, Elevation: backendTestElevation{recorder: recorder}, LoginStart: backendTestLoginStart{recorder: recorder},
		TrayExecutable: filepath.Join(root, "nodetray.exe"), TaskDefinition: nodetask.Definition{HelperExecutable: filepath.Join(root, "helper.exe"), HelperConfig: filepath.Join(root, "helper.json"), UserSID: "S-1-5-21-1"},
		Locations:    map[traymodel.LocationKind]trayapp.Location{traymodel.AgentLogs: {Path: filepath.Join(root, "agent", "logs"), Root: filepath.Join(root, "agent")}},
		PathResolver: backendTestResolver{}, Opener: backendTestOpener{recorder: recorder}, Workers: backendTestWorkers{recorder: recorder},
	}), recorder, agent
}

func TestParseLaunchModeAcceptsOnlyFixedElevatedOnceArguments(t *testing.T) {
	nonce := strings.Repeat("a", 64)
	pipe := `\\.\pipe\mysingerserver-elevate-` + nonce

	mode, err := parseLaunchMode(nil)
	if err != nil || mode.elevated || mode.background {
		t.Fatalf("normal mode = %+v, %v", mode, err)
	}
	mode, err = parseLaunchMode([]string{"--background"})
	if err != nil || !mode.background || mode.elevated {
		t.Fatalf("background mode = %+v, %v", mode, err)
	}
	mode, err = parseLaunchMode([]string{"--elevated-once", "--pipe", pipe, "--nonce", nonce})
	if err != nil || !mode.elevated || mode.background || mode.pipe != pipe || mode.nonce != nonce {
		t.Fatalf("elevated mode = %+v, %v", mode, err)
	}

	invalid := [][]string{
		{"--unknown"},
		{"--background", "--elevated-once", "--pipe", pipe, "--nonce", nonce},
		{"--elevated-once", "--pipe", pipe, "--nonce", nonce, "extra"},
		{"--elevated-once", "--nonce", nonce, "--pipe", pipe},
		{"--elevated-once", "--pipe", `\\.\pipe\other`, "--nonce", nonce},
		{"--elevated-once", "--pipe", pipe, "--nonce", strings.Repeat("A", 64)},
		{"--elevated-once", "--pipe", pipe, "--nonce", strings.Repeat("a", 63)},
	}
	for _, args := range invalid {
		if _, err := parseLaunchMode(args); err == nil {
			t.Fatalf("parseLaunchMode(%q) accepted", args)
		}
	}
}

func TestRunPassesBackgroundModeThroughNormalRuntimePath(t *testing.T) {
	original := runNormalTray
	t.Cleanup(func() { runNormalTray = original })
	var modes []bool
	runNormalTray = func(background bool) error {
		modes = append(modes, background)
		return nil
	}
	if err := run(nil); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--background"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(modes, []bool{false, true}) {
		t.Fatalf("normal runtime background modes = %v", modes)
	}
}

func TestBackendUnavailableServiceFailsClosedWithoutRawError(t *testing.T) {
	backend := NewBackend(nil)
	backend.Startup(context.Background())
	if _, err := backend.GetOverview(); !errors.Is(err, errBackendUnavailable) || err.Error() != "backend_unavailable" {
		t.Fatalf("GetOverview nil service error = %v", err)
	}
	if result := backend.StartAgent(); result.OK || result.ErrorCode != "backend_unavailable" || result.ErrorSummary != "" {
		t.Fatalf("StartAgent nil service = %+v", result)
	}
}

func TestWailsOptionsUseOnlyEmbeddedAssetsOneBackendAndCurrentUserData(t *testing.T) {
	backend := NewBackend(nil)
	assets := fstest.MapFS{"index.html": {Data: []byte("local")}}
	userData := filepath.Join(t.TempDir(), "WebView2")

	got := newWailsOptions(assets, backend, userData)
	if got.Width != 1080 || got.Height != 720 || got.MinWidth != 860 || got.MinHeight != 600 {
		t.Fatalf("window bounds = %dx%d min %dx%d", got.Width, got.Height, got.MinWidth, got.MinHeight)
	}
	if got.BackgroundColour == nil || got.BackgroundColour.A != 255 {
		t.Fatalf("background = %#v", got.BackgroundColour)
	}
	if got.AssetServer == nil || got.AssetServer.Assets == nil {
		t.Fatal("embedded asset server is not configured")
	}
	data, err := got.AssetServer.Assets.Open("index.html")
	if err != nil {
		t.Fatal(err)
	}
	_ = data.Close()
	if len(got.Bind) != 1 || got.Bind[0] != backend {
		t.Fatalf("bindings = %#v", got.Bind)
	}
	if got.OnStartup == nil || got.OnShutdown == nil || got.OnBeforeClose == nil {
		t.Fatal("Wails lifecycle callbacks are missing")
	}
	if got.HideWindowOnClose || got.EnableDefaultContextMenu || got.BindingsAllowedOrigins != "" {
		t.Fatalf("unsafe generic options = %+v", got)
	}
	if got.Windows == nil || got.Windows.Theme != windows.Light || got.Windows.WebviewIsTransparent || got.Windows.WindowIsTranslucent {
		t.Fatalf("Windows options = %#v", got.Windows)
	}
	if got.Windows.WebviewUserDataPath != userData {
		t.Fatalf("WebView2 user data = %q", got.Windows.WebviewUserDataPath)
	}
}

func TestWailsOptionsProvideChineseWebView2PreflightMessages(t *testing.T) {
	backend := NewBackend(nil)
	assets := fstest.MapFS{"index.html": {Data: []byte("local")}}

	got := newWailsOptions(assets, backend, t.TempDir())
	if got.Windows == nil || got.Windows.Messages == nil {
		t.Fatal("WebView2 preflight messages are missing")
	}
	messages := got.Windows.Messages
	values := []string{
		messages.InstallationRequired,
		messages.UpdateRequired,
		messages.MissingRequirements,
		messages.Webview2NotInstalled,
		messages.Error,
		messages.FailedToInstall,
		messages.DownloadPage,
		messages.PressOKToInstall,
		messages.ContactAdmin,
		messages.InvalidFixedWebview2,
		messages.WebView2ProcessCrash,
	}
	for index, value := range values {
		if strings.TrimSpace(value) == "" || !strings.Contains(value, "WebView2") {
			t.Fatalf("WebView2 message %d = %q", index, value)
		}
		if !strings.ContainsAny(value, "运行安装更新缺少失败下载联系重启错误") {
			t.Fatalf("WebView2 message is not Chinese: %q", value)
		}
	}
	if !strings.Contains(messages.DownloadPage, "微软官方") || !strings.Contains(messages.DownloadPage, "下载") {
		t.Fatalf("download guidance = %q", messages.DownloadPage)
	}
}

func TestMainReportsWailsInitializationFailureWithOnlyStableCode(t *testing.T) {
	service, recorder, _ := newBackendTestService(t)
	rawFailure := errors.New(`postgresql://user:secret@private/db C:\private\webview.log`)

	originalCompose := composeBackend
	originalWailsRun := wailsRunAdapter
	originalNormalTray := runNormalTray
	originalFailureLog := startupFailureLogAdapter
	t.Cleanup(func() {
		composeBackend = originalCompose
		wailsRunAdapter = originalWailsRun
		runNormalTray = originalNormalTray
		startupFailureLogAdapter = originalFailureLog
	})
	composeBackend = func() (*Backend, error) {
		backend := NewBackend(service)
		backend.webViewDataPath = `D:\便携 工具\Compute\data\nodetray\webview2`
		return backend, nil
	}
	wailsRunAdapter = func(*options.App) error { return rawFailure }
	runNormalTray = runNormalWails
	var logs []string
	startupFailureLogAdapter = func(code string) { logs = append(logs, code) }

	if exitCode := executeMain(nil); exitCode != 1 {
		t.Fatalf("exit code = %d", exitCode)
	}
	if !reflect.DeepEqual(logs, []string{"nodetray_start_failed code=wails_run_failed"}) {
		t.Fatalf("stable startup logs = %q", logs)
	}
	serialized := strings.ToLower(strings.Join(logs, " "))
	for _, forbidden := range []string{"postgres", "secret", "private", "webview.log"} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("startup log leaked %q: %q", forbidden, logs)
		}
	}
	if calls := recorder.snapshot(); len(calls) != 0 {
		t.Fatalf("Wails pre-start failure reached component services: %v", calls)
	}
}

func TestRunNormalWailsUsesBackendPortableWebViewData(t *testing.T) {
	service, _, _ := newBackendTestService(t)
	originalCompose, originalWailsRun, originalNormalTray := composeBackend, wailsRunAdapter, runNormalTray
	t.Cleanup(func() {
		composeBackend, wailsRunAdapter, runNormalTray = originalCompose, originalWailsRun, originalNormalTray
	})
	composeBackend = func() (*Backend, error) {
		backend := NewBackend(service)
		backend.webViewDataPath = `D:\便携 工具\Compute\data\nodetray\webview2`
		return backend, nil
	}
	var gotPath string
	wailsRunAdapter = func(app *options.App) error { gotPath = app.Windows.WebviewUserDataPath; return nil }
	runNormalTray = runNormalWails
	if err := runNormalWails(false); err != nil {
		t.Fatalf("runNormalWails: %v", err)
	}
	if gotPath != `D:\便携 工具\Compute\data\nodetray\webview2` {
		t.Fatalf("WebView2 data path = %q", gotPath)
	}
}

func TestMainReportsWhitelistedCompositionFailureCode(t *testing.T) {
	originalCompose := composeBackend
	originalNormalTray := runNormalTray
	originalFailureLog := startupFailureLogAdapter
	t.Cleanup(func() {
		composeBackend = originalCompose
		runNormalTray = originalNormalTray
		startupFailureLogAdapter = originalFailureLog
	})
	composeBackend = func() (*Backend, error) {
		return nil, errors.New("production composition: configuration store unavailable")
	}
	runNormalTray = runNormalWails
	var logs []string
	startupFailureLogAdapter = func(code string) { logs = append(logs, code) }

	if exitCode := executeMain(nil); exitCode != 1 {
		t.Fatalf("exit code = %d", exitCode)
	}
	if !reflect.DeepEqual(logs, []string{"nodetray_start_failed code=configuration_store_unavailable"}) {
		t.Fatalf("stable startup logs = %q", logs)
	}
}

func TestMainDoesNotLeakUnknownCompositionFailure(t *testing.T) {
	rawFailure := errors.New(`production composition: postgresql://user:secret@private/db C:\private\startup.log`)
	originalCompose := composeBackend
	originalNormalTray := runNormalTray
	originalFailureLog := startupFailureLogAdapter
	t.Cleanup(func() {
		composeBackend = originalCompose
		runNormalTray = originalNormalTray
		startupFailureLogAdapter = originalFailureLog
	})
	composeBackend = func() (*Backend, error) { return nil, rawFailure }
	runNormalTray = runNormalWails
	var logs []string
	startupFailureLogAdapter = func(code string) { logs = append(logs, code) }

	if exitCode := executeMain(nil); exitCode != 1 {
		t.Fatalf("exit code = %d", exitCode)
	}
	if !reflect.DeepEqual(logs, []string{"nodetray_start_failed code=composition_unavailable"}) {
		t.Fatalf("stable startup logs = %q", logs)
	}
	serialized := strings.ToLower(strings.Join(logs, " "))
	for _, forbidden := range []string{"postgres", "secret", "private", "startup.log"} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("startup log leaked %q: %q", forbidden, logs)
		}
	}
}

func TestWailsLifecycleStartsTrayAfterBackendAndClosesTrayBeforeBackend(t *testing.T) {
	useFakeTrayMonitorTicker(t)
	service, recorder, _ := newBackendTestService(t)
	backend := NewBackend(service)
	assets := fstest.MapFS{"index.html": {Data: []byte("local")}}

	originalStart := trayStartAdapter
	t.Cleanup(func() { trayStartAdapter = originalStart })
	var captured traynative.Options
	closedWhileBackendReady := false
	trayStartAdapter = func(options traynative.Options) (traynative.Controller, error) {
		if _, err := backend.GetOverview(); err != nil {
			t.Fatalf("tray started before backend: %v", err)
		}
		captured = options
		return &fakeTrayController{close: func() error {
			_, err := backend.GetOverview()
			closedWhileBackendReady = err == nil
			return nil
		}}, nil
	}

	app := newWailsOptions(assets, backend, t.TempDir())
	app.OnStartup(context.Background())
	if captured.Snapshot == nil || captured.Handle == nil || captured.ShowConsole == nil || captured.OnError == nil {
		t.Fatalf("tray options are incomplete: %+v", captured)
	}
	if snapshot := captured.Snapshot(); snapshot.MachineID != "node-"+strings.Repeat("1", 64) || snapshot.HelperStartMode != traymodel.StartManual {
		t.Fatalf("tray snapshot = %+v", snapshot)
	}
	app.OnShutdown(context.Background())
	if !closedWhileBackendReady {
		t.Fatal("tray was not closed before backend shutdown")
	}
	if result := backend.StartAgent(); result.OK || result.ErrorCode != "backend_not_started" {
		t.Fatalf("backend remained active after shutdown: %+v", result)
	}
	for _, call := range recorder.snapshot() {
		if call == "agent-stop" || call == "helper-stop" || call == "agent-force" || call == "helper-force" {
			t.Fatalf("tray lifecycle controlled a component: %q", call)
		}
	}
}

func TestTrayHandlersMapOnlyFixedCommandsAndUseStableAttention(t *testing.T) {
	useFakeTrayMonitorTicker(t)
	service, recorder, _ := newBackendTestService(t)
	backend := NewBackend(service)
	assets := fstest.MapFS{"index.html": {Data: []byte("local")}}

	originalStart := trayStartAdapter
	originalShow, originalUnminimise, originalCenter := windowShowAdapter, windowUnminimiseAdapter, windowCenterAdapter
	originalLog, originalEmit := logWarningAdapter, eventsEmitAdapter
	t.Cleanup(func() {
		trayStartAdapter = originalStart
		windowShowAdapter, windowUnminimiseAdapter, windowCenterAdapter = originalShow, originalUnminimise, originalCenter
		logWarningAdapter, eventsEmitAdapter = originalLog, originalEmit
	})
	var captured traynative.Options
	trayStartAdapter = func(options traynative.Options) (traynative.Controller, error) {
		captured = options
		return &fakeTrayController{}, nil
	}
	shown, unminimised, centered := 0, 0, 0
	windowShowAdapter = func(context.Context) { shown++ }
	windowUnminimiseAdapter = func(context.Context) { unminimised++ }
	windowCenterAdapter = func(context.Context) { centered++ }
	var logs []string
	logWarningAdapter = func(_ context.Context, message string) { logs = append(logs, message) }
	type emittedEvent struct {
		name string
		data []interface{}
	}
	var emitted []emittedEvent
	eventsEmitAdapter = func(_ context.Context, name string, data ...interface{}) {
		emitted = append(emitted, emittedEvent{name: name, data: data})
	}

	app := newWailsOptions(assets, backend, t.TempDir())
	app.OnStartup(context.Background())
	captured.ShowConsole()
	for _, command := range []traynative.Command{
		traynative.StartAgent, traynative.RestartAgent, traynative.StopAgent,
		traynative.StartHelper, traynative.StopHelper, traynative.OpenLogs,
		traynative.OpenSettings,
	} {
		captured.Handle(command)
	}
	captured.OnError(`password=hunter2 C:\secret\tray.log`)
	app.OnShutdown(context.Background())

	if shown != 3 || unminimised != 3 || centered != 3 {
		t.Fatalf("window show calls = %d/%d/%d", shown, unminimised, centered)
	}
	wantCalls := []string{"agent-start", "agent-restart", "agent-stop", "helper-start", "helper-stop", "open-location"}
	for _, want := range wantCalls {
		if countString(recorder.snapshot(), want) != 1 {
			t.Fatalf("%s call count in %v", want, recorder.snapshot())
		}
	}
	for _, call := range recorder.snapshot() {
		if call == "exit" || strings.Contains(call, "-force") {
			t.Fatalf("tray command performed unsafe lifecycle action: %q", call)
		}
	}
	if len(logs) != 1 || logs[0] != "tray_lifecycle_failed" {
		t.Fatalf("stable logs = %v", logs)
	}
	if len(emitted) != 2 || emitted[0].name != "open-settings-requested" || emitted[1].name != "attention-required" {
		t.Fatalf("events = %+v", emitted)
	}
	encoded, err := json.Marshal(emitted[1].data)
	if err != nil {
		t.Fatal(err)
	}
	serialized := strings.ToLower(string(encoded))
	if strings.Contains(serialized, "hunter2") || strings.Contains(serialized, "secret") {
		t.Fatalf("attention event leaked raw error: %+v", emitted[1])
	}
}

func TestTrayExitOnlyShowsUIAndEmitsUnifiedForceExitRequest(t *testing.T) {
	service, recorder, _ := newBackendTestService(t)
	backend := NewBackend(service)
	backend.Startup(context.Background())
	t.Cleanup(func() { _ = backend.Shutdown(context.Background()) })

	originalEmit := eventsEmitAdapter
	t.Cleanup(func() { eventsEmitAdapter = originalEmit })
	var emitted []string
	eventsEmitAdapter = func(_ context.Context, name string, _ ...interface{}) {
		emitted = append(emitted, name)
	}

	shown := 0
	handleTrayCommand(context.Background(), backend, func() { shown++ }, traynative.ExitTray, nil)

	if got := recorder.snapshot(); len(got) != 0 {
		t.Fatalf("tray exit controlled background processes: %v", got)
	}
	if shown != 1 || !reflect.DeepEqual(emitted, []string{"force-exit-requested"}) {
		t.Fatalf("shown=%d events=%v", shown, emitted)
	}
}

func TestTrayInitializationFailureKeepsWindowAndDoesNotFailWailsStartup(t *testing.T) {
	service, recorder, _ := newBackendTestService(t)
	backend := NewBackend(service)
	assets := fstest.MapFS{"index.html": {Data: []byte("local")}}

	originalStart := trayStartAdapter
	originalShow, originalUnminimise, originalCenter := windowShowAdapter, windowUnminimiseAdapter, windowCenterAdapter
	originalLog, originalEmit := logWarningAdapter, eventsEmitAdapter
	t.Cleanup(func() {
		trayStartAdapter = originalStart
		windowShowAdapter, windowUnminimiseAdapter, windowCenterAdapter = originalShow, originalUnminimise, originalCenter
		logWarningAdapter, eventsEmitAdapter = originalLog, originalEmit
	})
	trayStartAdapter = func(traynative.Options) (traynative.Controller, error) {
		return nil, errors.New(`password=hunter2 C:\secret\tray.log`)
	}
	shown := 0
	windowShowAdapter = func(context.Context) { shown++ }
	windowUnminimiseAdapter = func(context.Context) { shown++ }
	windowCenterAdapter = func(context.Context) { shown++ }
	var logValue, eventName string
	logWarningAdapter = func(_ context.Context, value string) { logValue = value }
	eventsEmitAdapter = func(_ context.Context, name string, _ ...interface{}) { eventName = name }

	app := newWailsOptions(assets, backend, t.TempDir())
	app.OnStartup(context.Background())
	if shown != 3 || logValue != "tray_unavailable" || eventName != "attention-required" {
		t.Fatalf("shown=%d log=%q event=%q", shown, logValue, eventName)
	}
	if result := backend.StartAgent(); !result.OK {
		t.Fatalf("backend unavailable after tray failure: %+v", result)
	}
	app.OnShutdown(context.Background())
	for _, call := range recorder.snapshot() {
		if call == "agent-stop" || call == "helper-stop" || strings.Contains(call, "-force") {
			t.Fatalf("tray initialization failure controlled a component: %q", call)
		}
	}
}

func TestTrayStatusMonitorPublishesOnlyFixedAttentionTransitions(t *testing.T) {
	monitor := &trayStatusMonitor{}
	started := time.Unix(1_750_000_000, 0)
	events := make([]traynative.Event, 0, 4)
	notify := func(event traynative.Event) { events = append(events, event) }
	healthy := traymodel.Overview{
		Agent: traymodel.ComponentState{
			Lifecycle:      traymodel.Running,
			Healthy:        true,
			Ready:          true,
			WorkerReady:    2,
			WorkerExpected: 2,
		},
		Helper:        traymodel.ComponentState{Lifecycle: traymodel.Running, Healthy: true, Ready: true},
		HelperEnabled: true,
	}

	monitor.Observe(started, healthy, nil, notify)
	monitor.Observe(started.Add(time.Second), traymodel.Overview{}, errors.New(`password=hunter2 C:\secret\config.json`), notify)
	monitor.Observe(started.Add(2*time.Second), traymodel.Overview{}, errors.New("still unavailable"), notify)

	drift := healthy
	drift.HelperTaskDrift = true
	monitor.Observe(started.Add(3*time.Second), drift, nil, notify)
	monitor.Observe(started.Add(4*time.Second), drift, nil, notify)
	monitor.Observe(started.Add(5*time.Second), healthy, nil, notify)

	failed := healthy
	failed.Agent.Lifecycle = traymodel.Failed
	monitor.Observe(started.Add(6*time.Second), failed, nil, notify)

	notReady := healthy
	notReady.Agent.WorkerReady = 1
	monitor.Observe(started.Add(7*time.Second), notReady, nil, notify)
	monitor.Observe(started.Add(36*time.Second), notReady, nil, notify)
	monitor.Observe(started.Add(37*time.Second), notReady, nil, notify)
	monitor.Observe(started.Add(40*time.Second), notReady, nil, notify)

	want := []traynative.Event{
		{Component: "config", Code: traynative.CodeConfigCorrupt},
		{Component: "config", Code: traynative.CodeConfigDrift},
		{Component: "agent", Code: traynative.CodeUnexpectedExit},
		{Component: "worker", Code: traynative.CodeWorkersNotReady},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("notification transitions = %#v, want %#v", events, want)
	}
	encoded, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	serialized := strings.ToLower(string(encoded))
	for _, forbidden := range []string{"hunter2", `c:\secret`, "still unavailable"} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("notifications leaked raw data %q: %s", forbidden, serialized)
		}
	}
}

func TestTrayMonitorStopCancelsBlockedRefreshWithoutRealTicker(t *testing.T) {
	service, _, agent := newBackendTestService(t)
	backend := NewBackend(service)
	backend.Startup(context.Background())
	t.Cleanup(func() { backend.Shutdown(context.Background()) })
	ticker := useFakeTrayMonitorTicker(t)
	agent.refreshCtx = make(chan context.Context, 1)
	agent.blockRefresh = true
	monitor := startTrayStatusMonitor(context.Background(), backend, func(traynative.Event) {})
	select {
	case refreshCtx := <-agent.refreshCtx:
		if refreshCtx == nil || refreshCtx.Err() != nil {
			t.Fatalf("refresh context = %v", refreshCtx)
		}
	case <-time.After(time.Second):
		t.Fatal("monitor did not begin initial refresh")
	}
	stopped := make(chan struct{})
	go func() {
		monitor.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("monitor Stop did not cancel blocked refresh")
	}
	if ticker.stopped != 1 {
		t.Fatalf("fake ticker stop count = %d", ticker.stopped)
	}
}

func TestTrayStartCommandsPublishFixedStartFailureAndManualUAC(t *testing.T) {
	service, _, agent := newBackendTestService(t)
	agent.results = map[string]traymodel.OperationResult{
		"start": {OK: false, ErrorCode: "private_error", ErrorSummary: `password=hunter2 C:\secret\agent.log`},
	}
	backend := NewBackend(service)
	backend.Startup(context.Background())
	t.Cleanup(func() { backend.Shutdown(context.Background()) })

	var events []traynative.Event
	notify := func(event traynative.Event) { events = append(events, event) }
	handleTrayCommand(context.Background(), backend, func() {}, traynative.StartAgent, notify)
	handleTrayCommand(context.Background(), backend, func() {}, traynative.StartHelper, notify)

	want := []traynative.Event{
		{Component: "agent", Code: traynative.CodeStartFailed},
		{Component: "helper", Code: traynative.CodeUACRequired},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("command notifications = %#v, want %#v", events, want)
	}
	encoded, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	serialized := strings.ToLower(string(encoded))
	if strings.Contains(serialized, "hunter2") || strings.Contains(serialized, "secret") || strings.Contains(serialized, "private_error") {
		t.Fatalf("command notification leaked operation details: %s", serialized)
	}
}

func TestWailsLifecycleStartsProductionNotificationMonitor(t *testing.T) {
	useFakeTrayMonitorTicker(t)
	service, _, _ := newBackendTestServiceWithSettings(t, backendTestSettings(), errors.New(`password=hunter2 C:\secret\settings.json`))
	backend := NewBackend(service)
	assets := fstest.MapFS{"index.html": {Data: []byte("local")}}

	originalStart := trayStartAdapter
	t.Cleanup(func() { trayStartAdapter = originalStart })
	events := make(chan traynative.Event, 1)
	trayStartAdapter = func(traynative.Options) (traynative.Controller, error) {
		return &fakeTrayController{notify: func(event traynative.Event) (bool, error) {
			select {
			case events <- event:
			default:
			}
			return true, nil
		}}, nil
	}

	app := newWailsOptions(assets, backend, t.TempDir())
	app.OnStartup(context.Background())
	select {
	case event := <-events:
		if event != (traynative.Event{Component: "config", Code: traynative.CodeConfigCorrupt}) {
			t.Fatalf("production notification = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("production notification monitor was not connected to Controller.Notify")
	}
	app.OnShutdown(context.Background())
}

type fakeBackendLifecycle struct {
	recorder    *backendTestRecorder
	duplicate   bool
	startErr    error
	closeErr    error
	startedWith context.Context
	onStart     func()
	onClose     func()
}

func (l *fakeBackendLifecycle) Start(ctx context.Context) (*bootstrap.Runtime, error) {
	l.recorder.add("runtime-start")
	l.startedWith = ctx
	if l.onStart != nil {
		l.onStart()
	}
	if l.startErr != nil {
		return nil, l.startErr
	}
	return &bootstrap.Runtime{Duplicate: l.duplicate}, nil
}

func (l *fakeBackendLifecycle) Close() error {
	l.recorder.add("runtime-close")
	if l.onClose != nil {
		l.onClose()
	}
	return l.closeErr
}

func TestBackendLifecycleHasZeroConstructionSideEffectsAndFixedStartupShutdownOrder(t *testing.T) {
	service, recorder, _ := newBackendTestService(t)
	lifecycle := &fakeBackendLifecycle{recorder: recorder}
	backend := NewBackend(service, lifecycle)
	if calls := recorder.snapshot(); len(calls) != 0 {
		t.Fatalf("constructor side effects = %v", calls)
	}
	lifecycle.onStart = func() {
		state := backend.ctx.(*backendContext)
		if state.snapshot() == nil {
			t.Fatal("runtime started before backend context activation")
		}
	}
	lifecycle.onClose = func() {
		state := backend.ctx.(*backendContext)
		if state.snapshot() == nil {
			t.Fatal("backend context cancelled before runtime Close")
		}
	}
	type lifecycleContextKey struct{}
	wailsContext := context.WithValue(context.Background(), lifecycleContextKey{}, "exact-wails-context")
	started := backend.Startup(wailsContext)
	if !started.Ready || started.Duplicate || started.ErrorCode != "" {
		t.Fatalf("Startup = %#v", started)
	}
	if lifecycle.startedWith != wailsContext {
		t.Fatalf("runtime received wrapped context %T instead of exact Wails lifecycle context", lifecycle.startedWith)
	}
	backend.Shutdown(context.Background())
	backend.Shutdown(context.Background())
	if countString(recorder.snapshot(), "runtime-start") != 1 || countString(recorder.snapshot(), "runtime-close") != 1 {
		t.Fatalf("lifecycle calls = %v", recorder.snapshot())
	}
	if state := backend.ctx.(*backendContext).snapshot(); state != nil {
		t.Fatal("backend context remained active after runtime Close")
	}
	for _, call := range recorder.snapshot() {
		if strings.Contains(call, "-stop") || strings.Contains(call, "-force") {
			t.Fatalf("Backend lifecycle controlled component: %v", recorder.snapshot())
		}
	}
}

func TestBackendCloseErrorIsStableAndRedacted(t *testing.T) {
	service, recorder, _ := newBackendTestService(t)
	backend := NewBackend(service, &fakeBackendLifecycle{recorder: recorder, closeErr: errors.New(`password=hunter2 C:\private\runtime.log`)})
	if started := backend.Startup(context.Background()); !started.Ready {
		t.Fatalf("Startup = %#v", started)
	}
	first := backend.Shutdown(context.Background())
	second := backend.Shutdown(context.Background())
	if first == nil || first.Error() != "runtime_close_failed" || second == nil || second.Error() != "runtime_close_failed" {
		t.Fatalf("stable close errors = %v / %v", first, second)
	}
	if countString(recorder.snapshot(), "runtime-close") != 1 {
		t.Fatalf("runtime close calls = %v", recorder.snapshot())
	}
}

func TestWailsDuplicateOnlyQuitsWithoutTrayMonitorOrWindow(t *testing.T) {
	service, recorder, _ := newBackendTestService(t)
	backend := NewBackend(service, &fakeBackendLifecycle{recorder: recorder, duplicate: true})
	assets := fstest.MapFS{"index.html": {Data: []byte("local")}}
	originalTray, originalQuit, originalHide := trayStartAdapter, wailsQuitAdapter, windowHideAdapter
	t.Cleanup(func() {
		trayStartAdapter, wailsQuitAdapter, windowHideAdapter = originalTray, originalQuit, originalHide
	})
	trayStarts, quits, hides := 0, 0, 0
	trayStartAdapter = func(traynative.Options) (traynative.Controller, error) {
		trayStarts++
		return &fakeTrayController{}, nil
	}
	wailsQuitAdapter = func(context.Context) { quits++ }
	windowHideAdapter = func(context.Context) { hides++ }

	app := newWailsOptions(assets, backend, t.TempDir(), true)
	app.OnStartup(context.Background())
	if trayStarts != 0 || quits != 1 || hides != 0 {
		t.Fatalf("duplicate side effects tray=%d quit=%d hide=%d", trayStarts, quits, hides)
	}
	if !reflect.DeepEqual(recorder.snapshot(), []string{"runtime-start"}) {
		t.Fatalf("duplicate reached component or monitor: %v", recorder.snapshot())
	}
	app.OnShutdown(context.Background())
}

func TestWailsStartupFailureUsesOnlyFixedAttentionAndNeverStartsTrayOrHides(t *testing.T) {
	service, recorder, _ := newBackendTestService(t)
	backend := NewBackend(service, &fakeBackendLifecycle{recorder: recorder, startErr: errors.New(`postgres://user:secret@private/db C:\private\tray.json`)})
	assets := fstest.MapFS{"index.html": {Data: []byte("local")}}
	originalTray, originalHide := trayStartAdapter, windowHideAdapter
	originalLog, originalEmit := logWarningAdapter, eventsEmitAdapter
	t.Cleanup(func() {
		trayStartAdapter, windowHideAdapter = originalTray, originalHide
		logWarningAdapter, eventsEmitAdapter = originalLog, originalEmit
	})
	trayStarts, hides := 0, 0
	trayStartAdapter = func(traynative.Options) (traynative.Controller, error) {
		trayStarts++
		return &fakeTrayController{}, nil
	}
	windowHideAdapter = func(context.Context) { hides++ }
	var logCode, eventName string
	var payload []interface{}
	logWarningAdapter = func(_ context.Context, value string) { logCode = value }
	eventsEmitAdapter = func(_ context.Context, name string, values ...interface{}) { eventName, payload = name, values }

	app := newWailsOptions(assets, backend, t.TempDir(), true)
	app.OnStartup(context.Background())
	if trayStarts != 0 || hides != 0 || logCode != "runtime_start_failed" || eventName != "attention-required" {
		t.Fatalf("failure effects tray=%d hide=%d log=%q event=%q", trayStarts, hides, logCode, eventName)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	serialized := strings.ToLower(string(encoded))
	for _, forbidden := range []string{"postgres", "secret", "private", "tray.json"} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("startup attention leaked %q: %s", forbidden, serialized)
		}
	}
	app.OnShutdown(context.Background())
}

func TestBackgroundHidesOnlyAfterSuccessfulRuntimeAndTrayStartup(t *testing.T) {
	useFakeTrayMonitorTicker(t)
	service, recorder, _ := newBackendTestService(t)
	backend := NewBackend(service, &fakeBackendLifecycle{recorder: recorder})
	assets := fstest.MapFS{"index.html": {Data: []byte("local")}}
	originalTray, originalHide := trayStartAdapter, windowHideAdapter
	t.Cleanup(func() { trayStartAdapter, windowHideAdapter = originalTray, originalHide })
	trayStartAdapter = func(traynative.Options) (traynative.Controller, error) {
		recorder.add("tray-start")
		return &fakeTrayController{}, nil
	}
	windowHideAdapter = func(context.Context) { recorder.add("window-hide") }
	app := newWailsOptions(assets, backend, t.TempDir(), true)
	app.OnStartup(context.Background())
	calls := recorder.snapshot()
	if len(calls) < 3 || !reflect.DeepEqual(calls[:3], []string{"runtime-start", "tray-start", "window-hide"}) {
		t.Fatalf("background startup order = %v", calls)
	}
	app.OnShutdown(context.Background())
}

func TestWailsShutdownClosesTrayBeforeRuntimeThenCancelsBackendContext(t *testing.T) {
	useFakeTrayMonitorTicker(t)
	service, recorder, _ := newBackendTestService(t)
	lifecycle := &fakeBackendLifecycle{recorder: recorder}
	backend := NewBackend(service, lifecycle)
	assets := fstest.MapFS{"index.html": {Data: []byte("local")}}
	originalTray := trayStartAdapter
	t.Cleanup(func() { trayStartAdapter = originalTray })
	trayStartAdapter = func(traynative.Options) (traynative.Controller, error) {
		return &fakeTrayController{close: func() error {
			recorder.add("tray-close")
			if backend.ctx.(*backendContext).snapshot() == nil {
				t.Fatal("backend context cancelled before tray close")
			}
			return nil
		}}, nil
	}
	app := newWailsOptions(assets, backend, t.TempDir())
	app.OnStartup(context.Background())
	app.OnShutdown(context.Background())
	calls := recorder.snapshot()
	trayIndex, runtimeIndex := -1, -1
	for index, call := range calls {
		if call == "tray-close" {
			trayIndex = index
		}
		if call == "runtime-close" {
			runtimeIndex = index
		}
	}
	if trayIndex < 0 || runtimeIndex < 0 || trayIndex >= runtimeIndex {
		t.Fatalf("shutdown order = %v", calls)
	}
	if backend.ctx.(*backendContext).snapshot() != nil {
		t.Fatal("backend context remained active after shutdown")
	}
}

func countString(values []string, want string) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}
