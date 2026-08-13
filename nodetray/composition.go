package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"

	"dedup/internal/machineid"
	trayapp "dedup/internal/nodetray/app"
	"dedup/internal/nodetray/bootstrap"
	"dedup/internal/nodetray/production"
	"dedup/internal/nodetray/traymodel"
	"dedup/internal/nodetray/windows/task"
)

type productionCompositionInputs struct {
	Store             trayapp.Store
	Validator         trayapp.Validator
	AgentConfig       trayapp.AgentConfigGateway
	LocalAgent        trayapp.LocalAgentGateway
	MachineID         string
	Agent             trayapp.Component
	Helper            trayapp.Component
	AgentFingerprint  trayapp.FingerprintUpdater
	HelperFingerprint trayapp.FingerprintUpdater
	Task              interface {
		trayapp.TaskController
		bootstrap.TaskRunner
	}
	Elevation       trayapp.ElevationClient
	LoginStart      trayapp.LoginStart
	PortableRoot    string
	WebViewDataPath string
	TrayExecutable  string
	TaskDefinition  task.Definition
	Locations       map[traymodel.LocationKind]trayapp.Location
	FinalPaths      interface {
		trayapp.PathResolver
		bootstrap.FinalPathResolver
	}
	Opener            trayapp.LocationOpener
	Workers           trayapp.WorkerProvider
	ProcessWaiter     trayapp.ProcessWaiter
	Paths             bootstrap.PathResolver
	Instance          bootstrap.InstanceService
	Factory           bootstrap.Factory
	Scheduler         bootstrap.RefreshScheduler
	UI                bootstrap.UI
	Emitter           production.EventEmitter
	Prepare           func() error
	Show              func(context.Context)
	Quit              func(context.Context)
	StartBootstrap    production.BootstrapStarter
	CloseAgentControl func() error
}

func composeProductionBackendWith(inputs productionCompositionInputs) (*Backend, error) {
	if err := validateProductionComposition(inputs); err != nil {
		return nil, err
	}
	state := &backendContext{}
	events := trayapp.NewEventBus(16)
	runtimeLifecycle := production.NewRuntime(production.RuntimeDependencies{
		Bootstrap: bootstrap.Dependencies{
			Paths: inputs.Paths, FinalPaths: inputs.FinalPaths, Settings: inputs.Store, HelperConfig: inputs.Store,
			Instance: inputs.Instance, Factory: inputs.Factory, Task: inputs.Task,
			Scheduler: inputs.Scheduler, UI: inputs.UI,
			Show: func() {
				if ctx := state.snapshot(); ctx != nil {
					inputs.Show(ctx)
				}
			},
		},
		Events: events, Emitter: inputs.Emitter, EventBuffer: 16,
		StartBootstrap: inputs.StartBootstrap,
	})
	lifecycle := &preparedRuntimeLifecycle{prepare: inputs.Prepare, runtime: runtimeLifecycle, events: events, closeAgentControl: inputs.CloseAgentControl}
	service := trayapp.NewService(trayapp.Dependencies{
		Store: inputs.Store, Validator: inputs.Validator, AgentConfig: inputs.AgentConfig,
		LocalAgent: inputs.LocalAgent,
		MachineID:  inputs.MachineID,
		Agent:      inputs.Agent, Helper: inputs.Helper,
		AgentFingerprint:  inputs.AgentFingerprint,
		HelperFingerprint: inputs.HelperFingerprint,
		Task:              inputs.Task, Elevation: inputs.Elevation, LoginStart: inputs.LoginStart,
		TrayExecutable: inputs.TrayExecutable, TaskDefinition: inputs.TaskDefinition,
		Locations: inputs.Locations, PathResolver: inputs.FinalPaths,
		Opener: inputs.Opener, Workers: inputs.Workers, ProcessWaiter: inputs.ProcessWaiter,
	})
	return &Backend{ctx: state, service: service, lifecycle: lifecycle, quit: inputs.Quit, webViewDataPath: inputs.WebViewDataPath}, nil
}

func validateProductionComposition(inputs productionCompositionInputs) error {
	if inputs.Store == nil || inputs.Validator == nil || inputs.AgentConfig == nil || inputs.LocalAgent == nil || inputs.Agent == nil || inputs.Helper == nil ||
		!machineid.Valid(inputs.MachineID) || inputs.AgentFingerprint == nil || inputs.HelperFingerprint == nil ||
		inputs.Task == nil || inputs.Elevation == nil || inputs.LoginStart == nil || inputs.FinalPaths == nil ||
		inputs.Opener == nil || inputs.Workers == nil || inputs.ProcessWaiter == nil || inputs.Paths == nil || inputs.Instance == nil ||
		inputs.Factory == nil || inputs.Scheduler == nil || inputs.UI == nil || inputs.Emitter == nil ||
		inputs.Prepare == nil || inputs.Show == nil || inputs.Quit == nil || inputs.CloseAgentControl == nil {
		return errors.New("production composition: required dependency unavailable")
	}
	if !validCompositionExecutable(inputs.TrayExecutable, "nodetray.exe") ||
		!validCompositionExecutable(inputs.TaskDefinition.HelperExecutable, "helper.exe") ||
		!validCompositionFile(inputs.TaskDefinition.HelperConfig) ||
		inputs.TaskDefinition.UserSID == "" || strings.TrimSpace(inputs.TaskDefinition.UserSID) != inputs.TaskDefinition.UserSID ||
		!strings.HasPrefix(inputs.TaskDefinition.UserSID, "S-1-") {
		return errors.New("production composition: fixed authority invalid")
	}
	if !validCompositionDirectory(inputs.PortableRoot) || !strictlyWithinCompositionRoot(inputs.WebViewDataPath, inputs.PortableRoot) {
		return errors.New("production composition: portable data invalid")
	}
	for _, kind := range []traymodel.LocationKind{
		traymodel.AgentLogs, traymodel.HelperLogs, traymodel.AgentBackup, traymodel.HelperBackup,
	} {
		location, ok := inputs.Locations[kind]
		if !ok || !validCompositionFile(location.Path) || !validCompositionFile(location.Root) {
			return errors.New("production composition: fixed locations invalid")
		}
	}
	return nil
}

func validCompositionExecutable(value, base string) bool {
	return validCompositionFile(value) && strings.EqualFold(filepath.Base(value), base)
}

func validCompositionFile(value string) bool {
	return value != "" && filepath.IsAbs(value) && filepath.Clean(value) == value && filepath.Base(value) != "."
}

func validCompositionDirectory(value string) bool { return validCompositionFile(value) }

func strictlyWithinCompositionRoot(path, root string) bool {
	if !validCompositionFile(path) || !validCompositionDirectory(root) {
		return false
	}
	relative, err := filepath.Rel(strings.ToLower(filepath.Clean(root)), strings.ToLower(filepath.Clean(path)))
	return err == nil && relative != "." && relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

type preparedRuntimeLifecycle struct {
	prepare           func() error
	runtime           *production.Runtime
	events            *trayapp.EventBus
	closeAgentControl func() error
	prepareOnce       sync.Once
	prepareErr        error
	closeOnce         sync.Once
	closeErr          error
}

func (l *preparedRuntimeLifecycle) Start(ctx context.Context) (*bootstrap.Runtime, error) {
	if l == nil || l.prepare == nil || l.runtime == nil {
		return nil, errors.New("production composition: runtime unavailable")
	}
	l.prepareOnce.Do(func() { l.prepareErr = l.prepare() })
	if l.prepareErr != nil {
		return nil, errors.New("production composition: tray settings unavailable")
	}
	return l.runtime.Start(ctx)
}

func (l *preparedRuntimeLifecycle) Close() error {
	if l == nil {
		return nil
	}
	l.closeOnce.Do(func() {
		if l.runtime != nil {
			l.closeErr = l.runtime.Close()
		}
		if l.events != nil {
			l.events.Close()
		}
		if l.closeAgentControl != nil {
			if err := l.closeAgentControl(); l.closeErr == nil {
				l.closeErr = err
			}
		}
	})
	return l.closeErr
}

func newContextEventEmitter(emit func(context.Context, string, any)) production.EventEmitter {
	return func(ctx context.Context, name string, payload any) {
		if emit == nil || ctx == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			emit(ctx, name, payload)
		}()
		select {
		case <-ctx.Done():
		case <-done:
		}
	}
}
