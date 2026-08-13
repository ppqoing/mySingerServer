package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	trayconfig "dedup/internal/nodetray/config"
	"dedup/internal/nodetray/traymodel"
	"dedup/internal/nodetray/windows/singleinstance"
)

type fakePaths struct {
	calls *[]string
	value Paths
	err   error
}

func (f fakePaths) Resolve(context.Context) (Paths, error) {
	*f.calls = append(*f.calls, "paths")
	return f.value, f.err
}

type fakeFinalPaths struct {
	values map[string]string
	errors map[string]error
	err    error
}

func (f fakeFinalPaths) Final(path string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if err, ok := f.errors[path]; ok {
		return "", err
	}
	if value, ok := f.values[path]; ok {
		return value, nil
	}
	return filepath.Clean(path), nil
}

type fakeSettings struct {
	calls *[]string
	value traymodel.TraySettings
	err   error
}

type fakeHelperConfig struct{ calls *[]string }

func (f fakeHelperConfig) LoadHelperForm() (trayconfig.HelperForm, error) {
	*f.calls = append(*f.calls, "load-helper-config")
	return trayconfig.HelperForm{}, nil
}
func (f fakeHelperConfig) ValidateHelperForm(trayconfig.HelperForm) []trayconfig.FieldError {
	*f.calls = append(*f.calls, "validate-helper-config")
	return nil
}

func (f fakeSettings) LoadTraySettings() (traymodel.TraySettings, error) {
	*f.calls = append(*f.calls, "settings")
	return f.value, f.err
}

type fakeLease struct {
	calls *[]string
	err   error
}

func (f *fakeLease) Close() error { *f.calls = append(*f.calls, "lease-close"); return f.err }

type fakeActivationListener struct {
	calls *[]string
	err   error
}

func (f *fakeActivationListener) Close() error {
	*f.calls = append(*f.calls, "listener-close")
	return f.err
}

type fakeInstance struct {
	calls     *[]string
	lease     Lease
	listener  Closer
	err       error
	listenErr error
	show      func()
}

func (f *fakeInstance) AcquireTray(context.Context) (Lease, error) {
	*f.calls = append(*f.calls, "acquire")
	return f.lease, f.err
}
func (f *fakeInstance) SignalExisting(context.Context) error {
	*f.calls = append(*f.calls, "signal-existing")
	return nil
}
func (f *fakeInstance) ListenActivation(_ context.Context, show func()) (Closer, error) {
	*f.calls = append(*f.calls, "listen-activation")
	f.show = show
	return f.listener, f.listenErr
}

type fakeManaged struct {
	name         string
	calls        *[]string
	adopt, start traymodel.OperationResult
	refreshes    int
}

func (f *fakeManaged) Adopt(context.Context) traymodel.OperationResult {
	*f.calls = append(*f.calls, f.name+"-adopt")
	if f.adopt.ErrorCode == "" && !f.adopt.OK {
		return traymodel.OperationResult{OK: true}
	}
	return f.adopt
}
func (f *fakeManaged) Start(context.Context) traymodel.OperationResult {
	*f.calls = append(*f.calls, f.name+"-start")
	if f.start.ErrorCode == "" && !f.start.OK {
		return traymodel.OperationResult{OK: true}
	}
	return f.start
}
func (f *fakeManaged) Refresh(context.Context) traymodel.ComponentState {
	f.refreshes++
	*f.calls = append(*f.calls, f.name+"-refresh")
	return traymodel.ComponentState{}
}

type fakeFactory struct {
	calls               *[]string
	agent, helper       Managed
	agentErr, helperErr error
	paths               []Paths
}

func (f *fakeFactory) NewAgent(_ context.Context, paths Paths) (Managed, error) {
	*f.calls = append(*f.calls, "new-agent")
	f.paths = append(f.paths, paths)
	return f.agent, f.agentErr
}
func (f *fakeFactory) NewHelper(_ context.Context, paths Paths) (Managed, error) {
	*f.calls = append(*f.calls, "new-helper")
	f.paths = append(f.paths, paths)
	return f.helper, f.helperErr
}

type fakeTask struct {
	calls *[]string
	err   error
}

func (f *fakeTask) Run(context.Context) error { *f.calls = append(*f.calls, "task-run"); return f.err }

type fakeTimer struct {
	calls *[]string
	err   error
}

func (f *fakeTimer) Close() error { *f.calls = append(*f.calls, "timer-close"); return f.err }

type fakeScheduler struct {
	calls   *[]string
	timer   Closer
	err     error
	refresh func(context.Context)
}

func (f *fakeScheduler) Start(_ context.Context, visible, recovery time.Duration, refresh func(context.Context)) (Closer, error) {
	*f.calls = append(*f.calls, fmt.Sprintf("schedule:%s:%s", visible, recovery))
	f.refresh = refresh
	return f.timer, f.err
}

type fakeUI struct {
	calls *[]string
	err   error
}

func (f fakeUI) Ready(context.Context) error { *f.calls = append(*f.calls, "ui"); return f.err }

type fakeAttention struct{ calls *[]string }

func (f fakeAttention) Required(component, code, summary string) {
	*f.calls = append(*f.calls, "attention:"+component+":"+code+":"+summary)
}

func bootstrapPaths(t *testing.T) Paths {
	t.Helper()
	root := t.TempDir()
	return Paths{TraySettings: filepath.Join(root, "tray.json"), AgentConfig: filepath.Join(root, "agent.json"), HelperConfig: filepath.Join(root, "helper", "helper.json")}
}

func bootstrapSettings(agent, helper traymodel.StartMode, login bool) traymodel.TraySettings {
	return traymodel.TraySettings{LoginStartTray: login, AgentStartMode: agent, HelperEnabled: true, HelperStartMode: helper, RefreshIntervalSeconds: 2, NotificationLevel: traymodel.NotifyImportant}
}

func bootstrapFixture(t *testing.T, settings traymodel.TraySettings) (Dependencies, *[]string, *fakeManaged, *fakeManaged, *fakeScheduler) {
	t.Helper()
	calls := []string{}
	agent := &fakeManaged{name: "agent", calls: &calls}
	helper := &fakeManaged{name: "helper", calls: &calls}
	timer := &fakeTimer{calls: &calls}
	scheduler := &fakeScheduler{calls: &calls, timer: timer}
	return Dependencies{
		Paths: fakePaths{calls: &calls, value: bootstrapPaths(t)}, Settings: fakeSettings{calls: &calls, value: settings}, HelperConfig: fakeHelperConfig{calls: &calls},
		FinalPaths: fakeFinalPaths{},
		Instance:   &fakeInstance{calls: &calls, lease: &fakeLease{calls: &calls}, listener: &fakeActivationListener{calls: &calls}},
		Factory:    &fakeFactory{calls: &calls, agent: agent, helper: helper}, Task: &fakeTask{calls: &calls},
		Scheduler: scheduler, UI: fakeUI{calls: &calls}, Attention: fakeAttention{calls: &calls}, Show: func() { calls = append(calls, "show") },
	}, &calls, agent, helper, scheduler
}

func TestStartCoversEightLoginAgentHelperModeCombinationsWithExactOrder(t *testing.T) {
	for _, login := range []bool{false, true} {
		for _, agentMode := range []traymodel.StartMode{traymodel.StartManual, traymodel.StartAutomatic} {
			for _, helperMode := range []traymodel.StartMode{traymodel.StartManual, traymodel.StartAutomatic} {
				name := fmt.Sprintf("login=%t/agent=%s/helper=%s", login, agentMode, helperMode)
				t.Run(name, func(t *testing.T) {
					deps, calls, _, _, _ := bootstrapFixture(t, bootstrapSettings(agentMode, helperMode, login))
					runtime, err := Start(context.Background(), deps)
					if err != nil {
						t.Fatalf("Start: %v", err)
					}
					defer runtime.Close()
					want := []string{"paths", "settings", "acquire", "listen-activation", "new-agent", "new-helper", "agent-adopt", "helper-adopt"}
					if agentMode == traymodel.StartAutomatic {
						want = append(want, "agent-start")
					}
					if helperMode == traymodel.StartAutomatic {
						want = append(want, "load-helper-config", "validate-helper-config", "helper-start")
					}
					want = append(want, "schedule:2s:10s", "ui")
					if !reflect.DeepEqual(*calls, want) {
						t.Fatalf("calls = %v\nwant  = %v", *calls, want)
					}
				})
			}
		}
	}
}

func TestDuplicateInstanceOnlySignalsExistingAndCreatesNoComponents(t *testing.T) {
	deps, calls, _, _, _ := bootstrapFixture(t, bootstrapSettings(traymodel.StartAutomatic, traymodel.StartAutomatic, true))
	deps.Instance = &fakeInstance{calls: calls, err: singleinstance.ErrAlreadyExists}
	runtime, err := Start(context.Background(), deps)
	if err != nil {
		t.Fatalf("Start duplicate: %v", err)
	}
	if runtime == nil || !runtime.Duplicate {
		t.Fatalf("runtime = %#v", runtime)
	}
	if !reflect.DeepEqual(*calls, []string{"paths", "settings", "acquire", "signal-existing"}) {
		t.Fatalf("calls = %v", *calls)
	}
}

func TestComponentFailuresBecomeAttentionAndDoNotPreventUI(t *testing.T) {
	deps, calls, agent, _, _ := bootstrapFixture(t, bootstrapSettings(traymodel.StartAutomatic, traymodel.StartAutomatic, false))
	agent.start = traymodel.OperationResult{ErrorCode: "start_failed", ErrorSummary: "password=secret\r\n"}
	helper := deps.Factory.(*fakeFactory).helper.(*fakeManaged)
	helper.start = traymodel.OperationResult{ErrorCode: "start_failed", ErrorSummary: "postgres://u:p@db/media"}
	runtime, err := Start(context.Background(), deps)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer runtime.Close()
	if (*calls)[len(*calls)-1] != "ui" {
		t.Fatalf("UI was not started: %v", *calls)
	}
	wantAttention := 0
	for _, call := range *calls {
		if len(call) >= len("attention:") && call[:len("attention:")] == "attention:" {
			wantAttention++
			if call == "attention:agent:start_failed:password=secret\r\n" {
				t.Fatal("attention leaked raw secret")
			}
		}
	}
	if wantAttention != 2 {
		t.Fatalf("attention count = %d, calls=%v", wantAttention, *calls)
	}
}

func TestRefreshAndCloseNeverRestartOrStopComponents(t *testing.T) {
	deps, calls, agent, helper, scheduler := bootstrapFixture(t, bootstrapSettings(traymodel.StartManual, traymodel.StartManual, false))
	runtime, err := Start(context.Background(), deps)
	if err != nil {
		t.Fatal(err)
	}
	scheduler.refresh(context.Background())
	if agent.refreshes != 1 || helper.refreshes != 1 {
		t.Fatalf("refreshes agent=%d helper=%d", agent.refreshes, helper.refreshes)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	for _, call := range *calls {
		if call == "agent-start" || call == "helper-start" || call == "agent-stop" || call == "helper-stop" {
			t.Fatalf("implicit component action: %v", *calls)
		}
	}
	if !reflect.DeepEqual((*calls)[len(*calls)-3:], []string{"timer-close", "listener-close", "lease-close"}) {
		t.Fatalf("close order = %v", *calls)
	}
}

func TestFirstInstanceListensBeforeFactoryAndActivationShowsWindow(t *testing.T) {
	deps, calls, _, _, _ := bootstrapFixture(t, bootstrapSettings(traymodel.StartManual, traymodel.StartManual, false))
	instance := deps.Instance.(*fakeInstance)
	runtime, err := Start(context.Background(), deps)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	instance.show()
	wantPrefix := []string{"paths", "settings", "acquire", "listen-activation", "new-agent"}
	if !reflect.DeepEqual((*calls)[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("startup prefix = %v, want %v", (*calls)[:len(wantPrefix)], wantPrefix)
	}
	if (*calls)[len(*calls)-1] != "show" {
		t.Fatalf("activation did not call injected show: %v", *calls)
	}
}

func TestMissingComponentTargetsUseFinalParentsAndStillReachUIReady(t *testing.T) {
	deps, calls, _, _, _ := bootstrapFixture(t, bootstrapSettings(traymodel.StartManual, traymodel.StartManual, false))
	paths, err := deps.Paths.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	*calls = nil
	finalRoot := filepath.Join(t.TempDir(), "final")
	agentParent := filepath.Join(finalRoot, "agent")
	helperParent := filepath.Join(finalRoot, "helper")
	deps.FinalPaths = fakeFinalPaths{
		values: map[string]string{
			filepath.Dir(paths.TraySettings): agentParent,
			filepath.Dir(paths.HelperConfig): helperParent,
		},
		errors: map[string]error{paths.TraySettings: os.ErrNotExist, paths.AgentConfig: os.ErrNotExist, paths.HelperConfig: os.ErrNotExist},
	}
	factory := deps.Factory.(*fakeFactory)
	factory.agentErr = os.ErrNotExist
	factory.helperErr = os.ErrNotExist
	runtime, err := Start(context.Background(), deps)
	if err != nil {
		t.Fatalf("Start with missing component targets: %v", err)
	}
	defer runtime.Close()
	if len(factory.paths) != 2 {
		t.Fatalf("factory paths = %#v", factory.paths)
	}
	want := Paths{
		TraySettings: filepath.Join(agentParent, filepath.Base(paths.TraySettings)),
		AgentConfig:  filepath.Join(agentParent, filepath.Base(paths.AgentConfig)),
		HelperConfig: filepath.Join(helperParent, filepath.Base(paths.HelperConfig)),
	}
	for i, got := range factory.paths {
		if got != want {
			t.Fatalf("factory paths[%d] = %#v, want %#v", i, got, want)
		}
	}
	if (*calls)[len(*calls)-1] != "ui" {
		t.Fatalf("missing component config blocked UI Ready: %v", *calls)
	}
}

func TestCloseJoinsTimerListenerAndLeaseErrorsOnceWithoutComponentStop(t *testing.T) {
	deps, calls, _, _, scheduler := bootstrapFixture(t, bootstrapSettings(traymodel.StartManual, traymodel.StartManual, false))
	scheduler.timer.(*fakeTimer).err = errors.New(`password=hunter2 C:\private\timer.log`)
	instance := deps.Instance.(*fakeInstance)
	instance.listener.(*fakeActivationListener).err = errors.New(`postgres://user:secret@private/listener`)
	instance.lease.(*fakeLease).err = errors.New(`C:\private\lease.handle`)
	runtime, err := Start(context.Background(), deps)
	if err != nil {
		t.Fatal(err)
	}
	first := runtime.Close()
	second := runtime.Close()
	if first == nil || second == nil || first.Error() != second.Error() {
		t.Fatalf("idempotent joined close errors = %v / %v", first, second)
	}
	for _, summary := range []string{"timer_close_failed", "activation_close_failed", "lease_close_failed"} {
		if !strings.Contains(first.Error(), summary) {
			t.Fatalf("joined close error %q missing %q", first, summary)
		}
	}
	for _, forbidden := range []string{"hunter2", "postgres", "secret", "private", "timer.log", "lease.handle"} {
		if strings.Contains(strings.ToLower(first.Error()), forbidden) {
			t.Fatalf("joined close error leaked %q: %v", forbidden, first)
		}
	}
	if count := countCalls(*calls, "timer-close"); count != 1 {
		t.Fatalf("timer close count = %d", count)
	}
	if count := countCalls(*calls, "listener-close"); count != 1 {
		t.Fatalf("listener close count = %d", count)
	}
	if count := countCalls(*calls, "lease-close"); count != 1 {
		t.Fatalf("lease close count = %d", count)
	}
	for _, call := range *calls {
		if strings.Contains(call, "stop") || strings.Contains(call, "force") {
			t.Fatalf("Close controlled component: %v", *calls)
		}
	}
}

func TestPartiallyReturnedResourcesAreClosedOnErrorPaths(t *testing.T) {
	t.Run("acquire", func(t *testing.T) {
		deps, calls, _, _, _ := bootstrapFixture(t, bootstrapSettings(traymodel.StartManual, traymodel.StartManual, false))
		deps.Instance.(*fakeInstance).err = errors.New("acquire failed")
		if runtime, err := Start(context.Background(), deps); runtime != nil || err == nil {
			t.Fatalf("Start runtime=%#v err=%v", runtime, err)
		}
		if countCalls(*calls, "lease-close") != 1 {
			t.Fatalf("partially acquired lease was not closed: %v", *calls)
		}
	})
	t.Run("listener", func(t *testing.T) {
		deps, calls, _, _, _ := bootstrapFixture(t, bootstrapSettings(traymodel.StartManual, traymodel.StartManual, false))
		deps.Instance.(*fakeInstance).listenErr = errors.New("listen failed")
		if runtime, err := Start(context.Background(), deps); runtime != nil || err == nil {
			t.Fatalf("Start runtime=%#v err=%v", runtime, err)
		}
		if countCalls(*calls, "listener-close") != 1 || countCalls(*calls, "lease-close") != 1 {
			t.Fatalf("partial listener cleanup = %v", *calls)
		}
	})
	t.Run("scheduler", func(t *testing.T) {
		deps, calls, _, _, scheduler := bootstrapFixture(t, bootstrapSettings(traymodel.StartManual, traymodel.StartManual, false))
		scheduler.err = errors.New("schedule failed")
		runtime, err := Start(context.Background(), deps)
		if err != nil {
			t.Fatal(err)
		}
		if err := runtime.Close(); err != nil {
			t.Fatal(err)
		}
		if countCalls(*calls, "timer-close") != 1 {
			t.Fatalf("partial timer cleanup = %v", *calls)
		}
	})
}

func countCalls(calls []string, want string) int {
	count := 0
	for _, call := range calls {
		if call == want {
			count++
		}
	}
	return count
}

func TestStartRejectsInvalidOrMissingFixedDependenciesWithoutSideEffects(t *testing.T) {
	deps, calls, _, _, _ := bootstrapFixture(t, bootstrapSettings(traymodel.StartManual, traymodel.StartManual, false))
	deps.Paths = fakePaths{calls: calls, value: Paths{TraySettings: "relative", AgentConfig: "also-relative", HelperConfig: "bad"}}
	if runtime, err := Start(context.Background(), deps); err == nil || runtime != nil {
		t.Fatalf("invalid paths runtime=%#v err=%v", runtime, err)
	}
	if !reflect.DeepEqual(*calls, []string{"paths"}) {
		t.Fatalf("side effects after invalid paths: %v", *calls)
	}
}

func TestStartRejectsLexicalAndFinalPathOverlapBeforeAcquiringInstance(t *testing.T) {
	t.Run("lexical parent child", func(t *testing.T) {
		deps, calls, _, _, _ := bootstrapFixture(t, bootstrapSettings(traymodel.StartManual, traymodel.StartManual, false))
		root := t.TempDir()
		deps.Paths = fakePaths{calls: calls, value: Paths{TraySettings: root, AgentConfig: filepath.Join(root, "agent.json"), HelperConfig: filepath.Join(root, "helper.json")}}
		if runtime, err := Start(context.Background(), deps); err == nil || runtime != nil {
			t.Fatalf("runtime=%#v err=%v", runtime, err)
		}
		if !reflect.DeepEqual(*calls, []string{"paths"}) {
			t.Fatalf("calls = %v", *calls)
		}
	})

	t.Run("reparse overlap", func(t *testing.T) {
		deps, calls, _, _, _ := bootstrapFixture(t, bootstrapSettings(traymodel.StartManual, traymodel.StartManual, false))
		paths, _ := deps.Paths.Resolve(context.Background())
		*calls = nil
		finalRoot := t.TempDir()
		deps.FinalPaths = fakeFinalPaths{values: map[string]string{
			filepath.Dir(paths.TraySettings): finalRoot,
			paths.TraySettings:               filepath.Join(finalRoot, "same.json"),
			paths.AgentConfig:                filepath.Join(finalRoot, "same.json"),
		}}
		if runtime, err := Start(context.Background(), deps); err == nil || runtime != nil {
			t.Fatalf("runtime=%#v err=%v", runtime, err)
		}
		if !reflect.DeepEqual(*calls, []string{"paths"}) {
			t.Fatalf("calls = %v", *calls)
		}
	})
}

func TestStartRejectsFinalPathEscapeAndResolverFailure(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(Dependencies) Dependencies
	}{
		{name: "escape", configure: func(deps Dependencies) Dependencies {
			paths, _ := deps.Paths.Resolve(context.Background())
			deps.FinalPaths = fakeFinalPaths{values: map[string]string{paths.AgentConfig: filepath.Join(t.TempDir(), "escaped-agent.json")}}
			return deps
		}},
		{name: "resolver failure", configure: func(deps Dependencies) Dependencies {
			deps.FinalPaths = fakeFinalPaths{err: errors.New("unavailable")}
			return deps
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			deps, calls, _, _, _ := bootstrapFixture(t, bootstrapSettings(traymodel.StartManual, traymodel.StartManual, false))
			deps = test.configure(deps)
			*calls = nil
			if runtime, err := Start(context.Background(), deps); err == nil || runtime != nil {
				t.Fatalf("runtime=%#v err=%v", runtime, err)
			}
			if !reflect.DeepEqual(*calls, []string{"paths"}) {
				t.Fatalf("calls = %v", *calls)
			}
		})
	}
}

func TestDefaultFinalPathResolverAcceptsFirstRunMissingComponentParentsAndTargets(t *testing.T) {
	deps, _, _, _, _ := bootstrapFixture(t, bootstrapSettings(traymodel.StartManual, traymodel.StartManual, false))
	root := t.TempDir()
	paths := Paths{
		TraySettings: filepath.Join(root, "local", "NodeTray", "tray.json"),
		AgentConfig:  filepath.Join(root, "program-data", "Node", "agent.json"),
		HelperConfig: filepath.Join(root, "program-data", "Helper", "helper.json"),
	}
	deps.Paths = fakePaths{calls: deps.Paths.(fakePaths).calls, value: paths}
	if err := os.MkdirAll(filepath.Dir(paths.TraySettings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.TraySettings, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	deps.FinalPaths = nil
	runtime, err := Start(context.Background(), deps)
	if err != nil {
		t.Fatalf("Start with missing component targets: %v", err)
	}
	defer runtime.Close()
}
