package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	trayapp "dedup/internal/nodetray/app"
	"dedup/internal/nodetray/bootstrap"
	trayconfig "dedup/internal/nodetray/config"
	"dedup/internal/nodetray/production"
	"dedup/internal/nodetray/traymodel"
	"dedup/internal/nodetray/windows/elevation"
	"dedup/internal/nodetray/windows/task"
)

type compositionStore struct{}

func (compositionStore) LoadTraySettings() (traymodel.TraySettings, error) {
	return production.DefaultTraySettings(), nil
}
func (compositionStore) SaveTraySettings(traymodel.TraySettings) error { return nil }
func (compositionStore) LoadHelperForm() (trayconfig.HelperForm, error) {
	return trayconfig.HelperForm{}, nil
}
func (compositionStore) PrepareHelperWrite(trayconfig.HelperForm) (trayconfig.PreparedWrite, error) {
	return trayconfig.PreparedWrite{}, nil
}

type compositionValidator struct{}

func (compositionValidator) ValidateHelper(trayconfig.HelperForm) []trayconfig.FieldError { return nil }

type compositionAgentConfig struct {
	loaded trayconfig.AgentForm
	calls  *[]string
}

func (g compositionAgentConfig) LoadAgentForm(context.Context) (trayconfig.AgentForm, error) {
	*g.calls = append(*g.calls, "socket-load-agent")
	return g.loaded, nil
}
func (g compositionAgentConfig) ValidateAgentForm(context.Context, trayconfig.AgentForm) []trayconfig.FieldError {
	*g.calls = append(*g.calls, "socket-validate-agent")
	return nil
}
func (g compositionAgentConfig) SaveAgentForm(context.Context, trayconfig.AgentForm) (trayapp.AgentConfigSaveResult, error) {
	*g.calls = append(*g.calls, "socket-save-agent")
	return trayapp.AgentConfigSaveResult{SHA256: strings.Repeat("c", 64), RestartRequired: true}, nil
}
func (g compositionAgentConfig) PromotePendingEndpoint() {
	*g.calls = append(*g.calls, "socket-promote-agent-endpoint")
}

type compositionComponent struct{}

func (*compositionComponent) Start(context.Context) traymodel.OperationResult {
	return traymodel.OperationResult{OK: true}
}
func (*compositionComponent) Stop(context.Context) traymodel.OperationResult {
	return traymodel.OperationResult{OK: true}
}
func (*compositionComponent) Restart(context.Context) traymodel.OperationResult {
	return traymodel.OperationResult{OK: true}
}
func (*compositionComponent) ForceStopTracked(context.Context) traymodel.OperationResult {
	return traymodel.OperationResult{OK: true}
}
func (*compositionComponent) Refresh(context.Context) traymodel.ComponentState {
	return traymodel.ComponentState{Lifecycle: traymodel.Stopped}
}
func (*compositionComponent) UpdateExpectedSHA256(string) traymodel.OperationResult {
	return traymodel.OperationResult{OK: true}
}
func (*compositionComponent) UpdateExpectedMachineID(string) traymodel.OperationResult {
	return traymodel.OperationResult{OK: true}
}

type compositionTask struct{}

func (compositionTask) Inspect(context.Context) (task.Status, error) { return task.Status{}, nil }
func (compositionTask) Run(context.Context) error                    { return nil }

type compositionElevation struct{}

func (compositionElevation) Invoke(context.Context, elevation.Action, []byte) (elevation.InvocationResult, error) {
	return elevation.InvocationResult{Response: elevation.Response{OK: true}}, nil
}

type compositionLoginStart struct{}

func (compositionLoginStart) Enabled() (bool, string, error) { return false, "", nil }
func (compositionLoginStart) Enable(string) error            { return nil }
func (compositionLoginStart) Disable() error                 { return nil }

type compositionFinalPaths struct{}

func (compositionFinalPaths) Final(path string) (string, error) { return path, nil }

type compositionOpener struct{}

func (compositionOpener) Open(context.Context, string) error { return nil }

type compositionWorkers struct{}

func (compositionWorkers) Snapshot(context.Context) ([]traymodel.WorkerState, error) { return nil, nil }

type compositionProcessWaiter struct{}

func (compositionProcessWaiter) WaitPIDGone(context.Context, int) error { return nil }

type compositionPaths struct{ paths bootstrap.Paths }

func (p compositionPaths) Resolve(context.Context) (bootstrap.Paths, error) { return p.paths, nil }

type compositionInstance struct{}

func (compositionInstance) AcquireTray(context.Context) (bootstrap.Lease, error) {
	return compositionCloser{}, nil
}
func (compositionInstance) ListenActivation(context.Context, func()) (bootstrap.Closer, error) {
	return compositionCloser{}, nil
}
func (compositionInstance) SignalExisting(context.Context) error { return nil }

type compositionCloser struct{}

func (compositionCloser) Close() error { return nil }

type compositionFactory struct{ component *compositionComponent }

func (f compositionFactory) NewAgent(context.Context, bootstrap.Paths) (bootstrap.Managed, error) {
	return f.component, nil
}
func (f compositionFactory) NewHelper(context.Context, bootstrap.Paths) (bootstrap.Managed, error) {
	return f.component, nil
}

func (*compositionComponent) Adopt(context.Context) traymodel.OperationResult {
	return traymodel.OperationResult{OK: true}
}

type compositionScheduler struct{}

func (compositionScheduler) Start(context.Context, time.Duration, time.Duration, func(context.Context)) (bootstrap.Closer, error) {
	return compositionCloser{}, nil
}

type compositionUI struct{}

func (compositionUI) Ready(context.Context) error { return nil }

func validCompositionInputs() productionCompositionInputs {
	component := &compositionComponent{}
	agentConfigCalls := []string{}
	paths := bootstrap.Paths{
		TraySettings: `C:\Portable\Compute\data\nodetray\tray.json`,
		AgentConfig:  `C:\Portable\Compute\data\agent\agent.json`,
		HelperConfig: `C:\Portable\Compute\data\helper\helper.json`,
	}
	return productionCompositionInputs{
		Store:             compositionStore{},
		Validator:         compositionValidator{},
		AgentConfig:       compositionAgentConfig{loaded: trayconfig.AgentForm{DataDir: "socket-agent"}, calls: &agentConfigCalls},
		MachineID:         "node-" + strings.Repeat("1", 64),
		Agent:             component,
		Helper:            component,
		AgentFingerprint:  component,
		HelperFingerprint: component,
		Task:              compositionTask{},
		Elevation:         compositionElevation{},
		LoginStart:        compositionLoginStart{},
		PortableRoot:      `C:\Portable\Compute`,
		WebViewDataPath:   `C:\Portable\Compute\data\nodetray\webview2`,
		TrayExecutable:    `C:\Portable\Compute\nodetray.exe`,
		TaskDefinition: task.Definition{
			HelperExecutable: `C:\Portable\Compute\helper.exe`,
			HelperConfig:     paths.HelperConfig,
			UserSID:          "S-1-5-21-101-202-303-1001",
		},
		Locations: map[traymodel.LocationKind]trayapp.Location{
			traymodel.AgentLogs:    {Path: `C:\Portable\Compute\data\agent\logs`, Root: `C:\Portable\Compute\data\agent`},
			traymodel.HelperLogs:   {Path: `C:\Portable\Compute\data\helper\logs`, Root: `C:\Portable\Compute\data\helper`},
			traymodel.AgentBackup:  {Path: paths.AgentConfig + ".last-good", Root: `C:\Portable\Compute\data\agent`},
			traymodel.HelperBackup: {Path: paths.HelperConfig + ".last-good", Root: `C:\Portable\Compute\data\helper`},
		},
		FinalPaths:        compositionFinalPaths{},
		Opener:            compositionOpener{},
		Workers:           compositionWorkers{},
		ProcessWaiter:     compositionProcessWaiter{},
		Paths:             compositionPaths{paths: paths},
		Instance:          compositionInstance{},
		Factory:           compositionFactory{component: component},
		Scheduler:         compositionScheduler{},
		UI:                compositionUI{},
		Emitter:           func(context.Context, string, any) {},
		Prepare:           func() error { return nil },
		Show:              func(context.Context) {},
		Quit:              func(context.Context) {},
		CloseAgentControl: func() error { return nil },
	}
}

func TestProductionCompositionRoutesAgentConfigThroughInjectedSocketGateway(t *testing.T) {
	inputs := validCompositionInputs()
	calls := []string{}
	inputs.AgentConfig = compositionAgentConfig{loaded: trayconfig.AgentForm{DataDir: "socket-only"}, calls: &calls}
	backend, err := composeProductionBackendWith(inputs)
	if err != nil {
		t.Fatal(err)
	}
	form, err := backend.service.GetAgentForm(context.Background())
	if err != nil || form.DataDir != "socket-only" {
		t.Fatalf("GetAgentForm = %#v, %v", form, err)
	}
	result := backend.service.SaveAgent(context.Background(), form)
	if !result.OK || result.SHA256 != strings.Repeat("c", 64) || !result.NeedsRestart {
		t.Fatalf("SaveAgent = %#v", result)
	}
	if want := []string{"socket-load-agent", "socket-validate-agent", "socket-save-agent"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestProductionCompositionDefersPreparationAndWiresActivationAndExit(t *testing.T) {
	inputs := validCompositionInputs()
	var prepareCalls, bootstrapCalls, showCalls, quitCalls, closeAgentCalls atomic.Int32
	inputs.Prepare = func() error { prepareCalls.Add(1); return nil }
	inputs.Show = func(ctx context.Context) {
		if ctx == nil || ctx.Err() != nil {
			t.Error("Show received inactive context")
		}
		showCalls.Add(1)
	}
	inputs.Quit = func(ctx context.Context) {
		if ctx == nil || ctx.Err() != nil {
			t.Error("Quit received inactive context")
		}
		quitCalls.Add(1)
	}
	inputs.CloseAgentControl = func() error { closeAgentCalls.Add(1); return nil }
	inputs.StartBootstrap = func(ctx context.Context, dependencies bootstrap.Dependencies) (*bootstrap.Runtime, error) {
		bootstrapCalls.Add(1)
		if dependencies.Factory == nil || dependencies.Settings == nil || dependencies.Instance == nil || dependencies.Show == nil {
			return nil, errors.New("incomplete bootstrap dependencies")
		}
		dependencies.Show()
		return &bootstrap.Runtime{}, nil
	}

	backend, err := composeProductionBackendWith(inputs)
	if err != nil {
		t.Fatalf("composeProductionBackendWith: %v", err)
	}
	if backend == nil || backend.service == nil || backend.lifecycle == nil {
		t.Fatalf("incomplete backend: %#v", backend)
	}
	if prepareCalls.Load() != 0 || bootstrapCalls.Load() != 0 || showCalls.Load() != 0 || quitCalls.Load() != 0 {
		t.Fatal("composition performed startup work")
	}

	startup := backend.Startup(context.Background())
	if !startup.Ready || prepareCalls.Load() != 1 || bootstrapCalls.Load() != 1 || showCalls.Load() != 1 {
		t.Fatalf("startup=%#v prepare=%d bootstrap=%d show=%d", startup, prepareCalls.Load(), bootstrapCalls.Load(), showCalls.Load())
	}
	if result := backend.ForceExitAll(); !result.OK || quitCalls.Load() != 1 {
		t.Fatalf("ForceExitAll result=%#v quit=%d", result, quitCalls.Load())
	}
	if err := backend.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if closeAgentCalls.Load() != 1 {
		t.Fatalf("shared Agent controller close calls = %d, want 1", closeAgentCalls.Load())
	}
}

func TestProductionCompositionRejectsIncompleteDependencies(t *testing.T) {
	inputs := validCompositionInputs()
	inputs.Elevation = nil
	if backend, err := composeProductionBackendWith(inputs); err == nil || backend != nil {
		t.Fatalf("incomplete composition backend=%#v err=%v", backend, err)
	}
}

func TestContextEventEmitterReturnsWhenContextIsCancelled(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	emitter := newContextEventEmitter(func(context.Context, string, any) {
		close(started)
		<-release
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		emitter(ctx, "component-state", map[string]string{"component": "agent"})
		close(done)
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		close(release)
		t.Fatal("event emitter ignored context cancellation")
	}
	close(release)
}
