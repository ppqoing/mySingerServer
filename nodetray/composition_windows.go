//go:build windows && !bindings

package main

import (
	"context"
	"errors"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"dedup/internal/machineid"
	"dedup/internal/nodectl"
	trayapp "dedup/internal/nodetray/app"
	"dedup/internal/nodetray/bootstrap"
	trayconfig "dedup/internal/nodetray/config"
	"dedup/internal/nodetray/process"
	"dedup/internal/nodetray/production"
	"dedup/internal/nodetray/supervisor"
	"dedup/internal/nodetray/traymodel"
	"dedup/internal/nodetray/windows/elevation"
	"dedup/internal/nodetray/windows/loginstart"
	"dedup/internal/nodetray/windows/singleinstance"
	"dedup/internal/nodetray/windows/task"
)

type windowsProductionStore interface {
	trayapp.Store
	production.FormValidationStore
	production.FingerprintSource
	bootstrap.SettingsLoader
	EnsureTraySettings(traymodel.TraySettings) error
}

type windowsProductionNative struct {
	Store          windowsProductionStore
	Inspector      process.Inspector
	AgentLauncher  supervisor.Launcher
	HelperLauncher supervisor.Launcher
	Terminator     supervisor.Terminator
	Dialer         production.Dialer
	MachineID      string
	Task           task.Service
	Elevation      trayapp.ElevationClient
	LoginStart     trayapp.LoginStart
	Instance       bootstrap.InstanceService
	UI             bootstrap.UI
	Opener         trayapp.LocationOpener
	Emitter        production.EventEmitter
	Show           func(context.Context)
	Quit           func(context.Context)
}

func init() {
	composeBackend = composeWindowsProductionBackend
}

func composeWindowsProductionBackend() (*Backend, error) {
	identity, err := machineid.Current()
	if err != nil {
		return nil, errors.New("production composition: machine identity unavailable")
	}
	for _, warning := range identity.Warnings {
		log.Print("nodetray_machine_identity_warning " + warning)
	}
	inspector := process.NewInspector()
	self, err := inspector.Inspect(os.Getpid())
	if err != nil {
		return nil, errors.New("production composition: current process identity unavailable")
	}
	layout, err := production.ResolvePortableLayout(self.ExecutablePath)
	if err != nil {
		return nil, errors.New("production composition: portable layout unavailable")
	}
	finalTray, err := (bootstrap.OSFinalPathResolver{}).Final(layout.TrayExecutable)
	if err != nil {
		return nil, errors.New("production composition: portable tray executable unavailable")
	}
	expected := self
	expected.ExecutablePath = finalTray
	if !process.SameProcess(expected, self) {
		return nil, errors.New("production composition: current executable is outside portable deployment")
	}
	userSID, err := process.UserSIDForProcess(self)
	if err != nil {
		return nil, errors.New("production composition: current user identity unavailable")
	}
	store, err := trayconfig.NewStore(trayconfig.Paths{
		TraySettings: layout.TraySettings, AgentConfig: layout.AgentConfig, HelperConfig: layout.HelperConfig,
		AgentExecutable: layout.AgentExecutable, HelperExecutable: layout.HelperExecutable,
	})
	if err != nil {
		return nil, errors.New("production composition: configuration store unavailable")
	}
	userTask, err := task.New(task.CapabilityUser)
	if err != nil {
		return nil, errors.New("production composition: task service unavailable")
	}
	login, err := loginstart.New(layout.TrayExecutable)
	if err != nil {
		return nil, errors.New("production composition: login-start service unavailable")
	}
	elevationClient, err := elevation.NewClient(layout.TrayExecutable, inspector)
	if err != nil {
		return nil, errors.New("production composition: elevation client unavailable")
	}
	handleInspector, ok := inspector.(process.HandleInspector)
	if !ok {
		return nil, errors.New("production composition: process handle inspector unavailable")
	}
	native := windowsProductionNative{
		Store: store, Inspector: inspector,
		AgentLauncher:  process.NewAgentLauncher(inspector),
		HelperLauncher: process.NewManualHelperLauncher(nil, handleInspector),
		Terminator:     process.NewDirectTerminator(),
		Dialer:         nodectlDialer{}, MachineID: identity.ID,
		Task: userTask, Elevation: elevationClient, LoginStart: login,
		Instance: &windowsInstanceService{userSID: userSID},
		UI:       windowsCompositionUI{},
		Opener:   production.NewLocationOpener(nil),
		Emitter: newContextEventEmitter(func(ctx context.Context, name string, payload any) {
			eventsEmitAdapter(ctx, name, payload)
		}),
		Show: showNodeWindow,
		Quit: wailsQuitAdapter,
	}
	inputs, err := buildWindowsProductionInputs(layout, userSID, native)
	if err != nil {
		return nil, err
	}
	return composeProductionBackendWith(inputs)
}

func buildWindowsProductionInputs(layout production.Layout, userSID string, native windowsProductionNative) (productionCompositionInputs, error) {
	if native.Store == nil || native.Inspector == nil || native.AgentLauncher == nil || native.HelperLauncher == nil ||
		native.Terminator == nil || native.Dialer == nil || !machineid.Valid(native.MachineID) || native.Task == nil ||
		native.Elevation == nil || native.LoginStart == nil || native.Instance == nil || native.UI == nil ||
		native.Opener == nil || native.Emitter == nil || native.Show == nil || native.Quit == nil ||
		userSID == "" || strings.TrimSpace(userSID) != userSID || !strings.HasPrefix(userSID, "S-1-") {
		return productionCompositionInputs{}, errors.New("production composition: Windows dependencies unavailable")
	}
	agentController := func(context.Context) (supervisor.Controller, error) {
		return production.NewAgentController(native.Dialer, native.MachineID)
	}
	helperController := func(context.Context) (supervisor.Controller, error) {
		return production.NewHelperController(native.Dialer, native.MachineID)
	}
	factory := production.NewFactory(production.FactoryOptions{
		ReadyTimeout: production.ProductionReadyTimeout,
		StopTimeout:  production.ProductionStopTimeout,
		Fingerprints: native.Store,
		Agent: production.ComponentDefinition{
			Component: nodectl.ComponentAgent, ExecutablePath: layout.AgentExecutable,
			Launcher: native.AgentLauncher, Inspector: native.Inspector,
			Controller: agentController, Terminator: native.Terminator,
		},
		Helper: production.ComponentDefinition{
			Component: nodectl.ComponentHelper, ExecutablePath: layout.HelperExecutable,
			Launcher: native.HelperLauncher, Inspector: native.Inspector,
			Controller: helperController, Terminator: native.Terminator,
		},
	})
	if factory.Agent() == nil || factory.Helper() == nil {
		return productionCompositionInputs{}, errors.New("production composition: shared component factory unavailable")
	}
	finalPaths := bootstrap.OSFinalPathResolver{}
	return productionCompositionInputs{
		Store: native.Store, Validator: production.NewValidator(native.Store),
		MachineID: native.MachineID,
		Agent:     factory.Agent(), Helper: factory.Helper(),
		AgentFingerprint: factory.Agent(), HelperFingerprint: factory.Helper(),
		Task: native.Task, Elevation: native.Elevation, LoginStart: native.LoginStart,
		PortableRoot: layout.Root, WebViewDataPath: layout.WebViewData,
		TrayExecutable: layout.TrayExecutable,
		TaskDefinition: task.Definition{HelperExecutable: layout.HelperExecutable, HelperConfig: layout.HelperConfig, UserSID: userSID},
		Locations:      fixedCompositionLocations(layout),
		FinalPaths:     finalPaths, Opener: native.Opener,
		Workers:       &lazyAgentWorkers{dialer: native.Dialer, machineID: native.MachineID},
		ProcessWaiter: process.NewPIDWaiter(),
		Paths: fixedCompositionPaths{paths: bootstrap.Paths{
			TraySettings: layout.TraySettings, AgentConfig: layout.AgentConfig, HelperConfig: layout.HelperConfig,
		}},
		Instance: native.Instance, Factory: factory, Scheduler: production.NewScheduler(nil), UI: native.UI,
		Emitter: native.Emitter,
		Prepare: func() error { return native.Store.EnsureTraySettings(production.DefaultTraySettings()) },
		Show:    native.Show, Quit: native.Quit,
	}, nil
}

func fixedCompositionLocations(layout production.Layout) map[traymodel.LocationKind]trayapp.Location {
	agentRoot := filepath.Dir(layout.AgentConfig)
	helperRoot := filepath.Dir(layout.HelperConfig)
	return map[traymodel.LocationKind]trayapp.Location{
		traymodel.AgentLogs:    {Path: layout.AgentLogs, Root: agentRoot},
		traymodel.HelperLogs:   {Path: layout.HelperLogs, Root: helperRoot},
		traymodel.AgentBackup:  {Path: layout.AgentConfig + ".last-good", Root: agentRoot},
		traymodel.HelperBackup: {Path: layout.HelperConfig + ".last-good", Root: helperRoot},
	}
}

type fixedCompositionPaths struct{ paths bootstrap.Paths }

func (p fixedCompositionPaths) Resolve(context.Context) (bootstrap.Paths, error) { return p.paths, nil }

type nodectlDialer struct{}

func (nodectlDialer) Dial(ctx context.Context, name string) (net.Conn, error) {
	return nodectl.Dial(ctx, name)
}

type lazyAgentWorkers struct {
	dialer    production.Dialer
	machineID string
}

func (p *lazyAgentWorkers) Snapshot(ctx context.Context) ([]traymodel.WorkerState, error) {
	controller, err := production.NewAgentController(p.dialer, p.machineID)
	if err != nil {
		return nil, errors.New("production workers: Agent controller unavailable")
	}
	return production.NewWorkerProvider(controller).Snapshot(ctx)
}

type windowsCompositionUI struct{}

func (windowsCompositionUI) Ready(ctx context.Context) error {
	if ctx == nil || ctx.Err() != nil {
		return errors.New("production composition: Wails context unavailable")
	}
	windowShowAdapter(ctx)
	return nil
}

type windowsInstanceService struct{ userSID string }

func (s *windowsInstanceService) AcquireTray(ctx context.Context) (bootstrap.Lease, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, errors.New("production composition: instance context unavailable")
	}
	return singleinstance.AcquireTray(s.userSID)
}

func (*windowsInstanceService) SignalExisting(ctx context.Context) error {
	return singleinstance.SignalExisting(ctx)
}

func (*windowsInstanceService) ListenActivation(parent context.Context, show func()) (bootstrap.Closer, error) {
	if parent == nil || parent.Err() != nil || show == nil {
		return nil, errors.New("production composition: activation dependencies unavailable")
	}
	ctx, cancel := context.WithCancel(parent)
	closer := &activationListenerCloser{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(closer.done)
		closer.err = singleinstance.ListenActivation(ctx, show)
	}()
	return closer, nil
}

type activationListenerCloser struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
	err    error
}

func (c *activationListenerCloser) Close() error {
	if c == nil {
		return nil
	}
	c.once.Do(func() {
		c.cancel()
		<-c.done
	})
	if errors.Is(c.err, context.Canceled) {
		return nil
	}
	return c.err
}
