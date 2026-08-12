package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"dedup/internal/nodetray/config"
	"dedup/internal/nodetray/traymodel"
	"dedup/internal/nodetray/windows/elevation"
	nodetask "dedup/internal/nodetray/windows/task"
)

type fakeStore struct {
	settings  traymodel.TraySettings
	helper    config.HelperForm
	prepared  config.PreparedWrite
	calls     *[]string
	saveErr   error
	loadErr   error
	loadCalls int
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
func (f *fakeStore) PrepareHelperWrite(config.HelperForm) (config.PreparedWrite, error) {
	*f.calls = append(*f.calls, "prepare-helper")
	if f.saveErr != nil {
		return config.PreparedWrite{}, f.saveErr
	}
	return f.prepared, nil
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
	store := &fakeStore{settings: validSettings(), calls: &calls, prepared: config.PreparedWrite{TargetPath: `C:\ProgramData\MySingerServer\helper.json`, CanonicalJSON: []byte("{}"), SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
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
	if !overview.HelperTaskDrift {
		t.Fatal("installed Helper task drift was hidden by disabled normalization")
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

func TestAgentConfigOperationsUseContextAwareSocketGatewayAndReturnedRestartState(t *testing.T) {
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
	if !result.OK || !result.Saved || result.SHA256 != wantSHA || !result.NeedsRestart {
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

func TestSaveHelperInvokesOneShotExactlyOnceAndUACCancelDoesNotTouchSupervisor(t *testing.T) {
	s, calls, _, _, _, elevated := serviceFixture(t)
	elevated.result = elevation.InvocationResult{UACCancelled: true, Response: elevation.Response{ErrorCode: elevation.ErrorCodeUACCancelled, ErrorSummary: "password=secret\r\n"}}
	result := s.SaveHelper(context.Background(), config.HelperForm{})
	if result.OK || result.ErrorCode != elevation.ErrorCodeUACCancelled {
		t.Fatalf("SaveHelper = %#v", result)
	}
	if !reflect.DeepEqual(*calls, []string{"prepare-helper", "elevate-write_helper_config"}) {
		t.Fatalf("calls = %v", *calls)
	}
	if len(elevated.actions) != 1 {
		t.Fatalf("elevation calls = %d", len(elevated.actions))
	}
}

func TestSaveHelperPublishesPreparedFingerprintOnlyAfterElevatedSuccess(t *testing.T) {
	s, calls, _, _, _, elevated := serviceFixture(t)
	updater := &fakeFingerprintUpdater{name: "helper", calls: calls}
	s.helperFingerprint = updater

	if result := s.SaveHelper(context.Background(), config.HelperForm{}); !result.OK {
		t.Fatalf("SaveHelper = %#v", result)
	}
	wantSHA := strings.Repeat("a", 64)
	if !reflect.DeepEqual(*calls, []string{"prepare-helper", "elevate-write_helper_config", "helper-sha"}) || !reflect.DeepEqual(updater.values, []string{wantSHA}) {
		t.Fatalf("calls=%v values=%v", *calls, updater.values)
	}

	*calls = nil
	elevated.result = elevation.InvocationResult{Response: elevation.Response{OK: false, ErrorCode: "write_failed"}}
	if result := s.SaveHelper(context.Background(), config.HelperForm{}); result.OK {
		t.Fatalf("failed SaveHelper = %#v", result)
	}
	if !reflect.DeepEqual(*calls, []string{"prepare-helper", "elevate-write_helper_config"}) || len(updater.values) != 1 {
		t.Fatalf("failed elevated write published fingerprint: calls=%v values=%v", *calls, updater.values)
	}
}

func TestHelperManualAndAutomaticOperationsUseExclusiveRoutes(t *testing.T) {
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
	if !reflect.DeepEqual(*calls, []string{"task-run", "helper-stop", "helper-stop", "task-run"}) {
		t.Fatalf("automatic calls = %v", *calls)
	}
}

func TestAutomaticHelperRestartShortCircuitsBeforeTaskRunWhenControlledStopFails(t *testing.T) {
	s, calls, store, _, helper, _ := serviceFixture(t)
	store.settings.HelperStartMode = traymodel.StartAutomatic
	helper.results["stop"] = traymodel.OperationResult{ErrorCode: "stop_timeout", ErrorSummary: "password=secret"}
	result := s.RestartHelper(context.Background())
	if result.OK || result.ErrorCode != "stop_timeout" {
		t.Fatalf("RestartHelper = %#v", result)
	}
	if !reflect.DeepEqual(*calls, []string{"helper-stop"}) {
		t.Fatalf("calls = %v", *calls)
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

func TestSaveTraySettingsHelperPolicyChangeRunsElevationBeforeDiskCommit(t *testing.T) {
	s, calls, _, _, _, elevated := serviceFixture(t)
	value := validSettings()
	value.HelperStartMode = traymodel.StartAutomatic

	result := s.SaveTraySettings(context.Background(), value)

	if !result.OK || !reflect.DeepEqual(*calls, []string{"elevate-install_helper_task", "save-settings"}) {
		t.Fatalf("result=%#v calls=%v", result, *calls)
	}
	if !reflect.DeepEqual(elevated.actions, []elevation.Action{elevation.ActionInstallHelperTask}) {
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
	if task.inspectCalls != 2 || len(elevated.actions) != 0 {
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

func TestSaveTraySettingsHelperTaskInspectFailureDoesNotPersistPolicy(t *testing.T) {
	s, calls, store, _, _, elevated := serviceFixture(t)
	store.settings.HelperEnabled = false
	task := s.task.(*fakeTask)
	task.err = errors.New("scheduler unavailable")
	value := store.settings
	value.HelperEnabled = true

	result := s.SaveTraySettings(context.Background(), value)

	if result.OK || result.ErrorCode != "task_failed" || store.settings.HelperEnabled {
		t.Fatalf("result=%#v persisted=%#v", result, store.settings)
	}
	if len(*calls) != 0 || len(elevated.actions) != 0 {
		t.Fatalf("calls=%v elevation=%v", *calls, elevated.actions)
	}
}

func TestSaveTraySettingsRemovesInstalledHelperTaskBeforePersistingManualOrDisabledPolicy(t *testing.T) {
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
			if !reflect.DeepEqual(*calls, []string{"elevate-remove_helper_task", "save-settings"}) {
				t.Fatalf("calls=%v", *calls)
			}
			if !reflect.DeepEqual(elevated.actions, []elevation.Action{elevation.ActionRemoveHelperTask}) {
				t.Fatalf("elevation actions=%v", elevated.actions)
			}
		})
	}
}

func TestSaveTraySettingsUACCancelDoesNotPersistRequestedPolicy(t *testing.T) {
	s, calls, store, _, _, elevated := serviceFixture(t)
	elevated.result = elevation.InvocationResult{UACCancelled: true}
	value := validSettings()
	value.HelperStartMode = traymodel.StartAutomatic

	result := s.SaveTraySettings(context.Background(), value)

	if result.OK || !result.UACCancelled || result.ErrorCode != elevation.ErrorCodeUACCancelled {
		t.Fatalf("result = %#v", result)
	}
	if !reflect.DeepEqual(*calls, []string{"elevate-install_helper_task"}) || store.settings.HelperStartMode != traymodel.StartManual {
		t.Fatalf("calls=%v persisted=%#v", *calls, store.settings)
	}
	if store.loadCalls != 1 {
		t.Fatalf("settings loads = %d, want initial load only", store.loadCalls)
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
