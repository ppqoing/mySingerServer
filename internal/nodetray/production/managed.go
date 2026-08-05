package production

import (
	"context"
	"errors"
	"sync"
	"time"

	"dedup/internal/nodectl"
	"dedup/internal/nodetray/bootstrap"
	"dedup/internal/nodetray/process"
	"dedup/internal/nodetray/supervisor"
	"dedup/internal/nodetray/traymodel"
)

const (
	ProductionReadyTimeout = 30 * time.Second
	ProductionStopTimeout  = 15 * time.Second
)

type ManagedComponent struct {
	supervisor *supervisor.Supervisor
	controller supervisor.Controller
	inspector  process.Inspector
}

func NewManagedComponent(value *supervisor.Supervisor, controller supervisor.Controller, inspector process.Inspector) *ManagedComponent {
	return &ManagedComponent{supervisor: value, controller: controller, inspector: inspector}
}

func (m *ManagedComponent) Adopt(ctx context.Context) traymodel.OperationResult {
	if m == nil || m.supervisor == nil || m.controller == nil || m.inspector == nil {
		return managedFailure("unavailable", "组件不可用")
	}
	status, err := m.controller.Status(ctx)
	if err != nil {
		return managedFailure("unavailable", "组件状态不可用")
	}
	reported := process.Identity{PID: status.PID, ExecutablePath: status.ExecutablePath}
	actual, err := m.inspector.Inspect(status.PID)
	if err != nil || !process.SamePIDAndExecutable(actual, reported) {
		return managedFailure("identity_mismatch", "组件身份已变化")
	}
	state := m.supervisor.Adopt(ctx, actual)
	if state.Lifecycle == traymodel.Failed || state.PID != actual.PID || state.StartedAtUnixMS != actual.StartedAtUnixMS {
		code := managedAdoptCode(state.ErrorCode)
		return managedFailure(code, "组件认领失败")
	}
	return traymodel.OperationResult{OK: true}
}

func (m *ManagedComponent) Start(ctx context.Context) traymodel.OperationResult {
	if m == nil || m.supervisor == nil {
		return managedFailure("unavailable", "组件不可用")
	}
	return m.supervisor.Start(ctx)
}

func (m *ManagedComponent) Stop(ctx context.Context) traymodel.OperationResult {
	if m == nil || m.supervisor == nil {
		return managedFailure("unavailable", "组件不可用")
	}
	return m.supervisor.Stop(ctx)
}

func (m *ManagedComponent) Restart(ctx context.Context) traymodel.OperationResult {
	if m == nil || m.supervisor == nil {
		return managedFailure("unavailable", "组件不可用")
	}
	return m.supervisor.Restart(ctx)
}

func (m *ManagedComponent) ForceStopTracked(ctx context.Context) traymodel.OperationResult {
	if m == nil || m.supervisor == nil {
		return managedFailure("unavailable", "组件不可用")
	}
	return m.supervisor.ForceStopTracked(ctx)
}

func (m *ManagedComponent) Refresh(ctx context.Context) traymodel.ComponentState {
	if m == nil || m.supervisor == nil {
		return unavailableComponentState()
	}
	return m.supervisor.Refresh(ctx)
}

func (m *ManagedComponent) UpdateExpectedSHA256(value string) traymodel.OperationResult {
	if m == nil || m.supervisor == nil {
		return managedFailure("unavailable", "组件不可用")
	}
	return m.supervisor.UpdateExpectedSHA256(value)
}

func (m *ManagedComponent) UpdateExpectedMachineID(value string) traymodel.OperationResult {
	updater, ok := m.controller.(interface {
		UpdateExpectedMachineID(string) traymodel.OperationResult
	})
	if !ok || updater == nil {
		return managedFailure("unavailable", "组件不可用")
	}
	return updater.UpdateExpectedMachineID(value)
}

func managedAdoptCode(code string) string {
	switch code {
	case "invalid_config", "unclaimed_instance", "identity_mismatch":
		return code
	default:
		return "unclaimed_instance"
	}
}

func managedFailure(code, summary string) traymodel.OperationResult {
	return traymodel.OperationResult{ErrorCode: code, ErrorSummary: summary}
}

func unavailableComponentState() traymodel.ComponentState {
	return traymodel.ComponentState{Lifecycle: traymodel.Failed, ErrorCode: "unavailable", ErrorSummary: "组件不可用", NeedsAttention: true}
}

type FingerprintSource interface {
	AgentFingerprint() (string, error)
	HelperFingerprint() (string, error)
}

type ControllerFactory func(context.Context) (supervisor.Controller, error)

type ComponentDefinition struct {
	Component      nodectl.Component
	ExecutablePath string
	Launcher       supervisor.Launcher
	Inspector      process.Inspector
	Controller     ControllerFactory
	Terminator     supervisor.Terminator
}

type SupervisorFactory func(supervisor.Spec, supervisor.Launcher, process.Inspector, supervisor.Controller, supervisor.Terminator) *supervisor.Supervisor

type FactoryOptions struct {
	ReadyTimeout  time.Duration
	StopTimeout   time.Duration
	Fingerprints  FingerprintSource
	Agent         ComponentDefinition
	Helper        ComponentDefinition
	NewSupervisor SupervisorFactory
}

type Factory struct {
	readyTimeout  time.Duration
	stopTimeout   time.Duration
	fingerprints  FingerprintSource
	agentDef      ComponentDefinition
	helperDef     ComponentDefinition
	newSupervisor SupervisorFactory
	agent         *SharedComponent
	helper        *SharedComponent
}

func NewFactory(options FactoryOptions) *Factory {
	newSupervisor := options.NewSupervisor
	if newSupervisor == nil {
		newSupervisor = supervisor.New
	}
	return &Factory{
		readyTimeout: options.ReadyTimeout, stopTimeout: options.StopTimeout,
		fingerprints: options.Fingerprints, agentDef: options.Agent, helperDef: options.Helper,
		newSupervisor: newSupervisor, agent: &SharedComponent{}, helper: &SharedComponent{},
	}
}

func (f *Factory) Agent() *SharedComponent {
	if f == nil {
		return nil
	}
	return f.agent
}

func (f *Factory) Helper() *SharedComponent {
	if f == nil {
		return nil
	}
	return f.helper
}

func (f *Factory) NewAgent(ctx context.Context, paths bootstrap.Paths) (bootstrap.Managed, error) {
	if f == nil || f.agent == nil {
		return nil, errors.New("production factory: Agent unavailable")
	}
	f.agent.configure(func(fingerprint, pendingMachineID string) (*ManagedComponent, error) {
		return f.build(ctx, f.agentDef, paths.AgentConfig, fingerprint, pendingMachineID)
	})
	if f.agent.snapshot() != nil {
		return f.agent, nil
	}
	if f.fingerprints == nil {
		return f.agent, errors.New("production factory: Agent fingerprint unavailable")
	}
	fingerprint, err := f.fingerprints.AgentFingerprint()
	if err != nil {
		return f.agent, errors.New("production factory: Agent configuration unavailable")
	}
	shared, err := f.agent.initialize(fingerprint)
	if err != nil || shared == nil {
		return f.agent, err
	}
	return shared, nil
}

func (f *Factory) NewHelper(ctx context.Context, paths bootstrap.Paths) (bootstrap.Managed, error) {
	if f == nil || f.helper == nil {
		return nil, errors.New("production factory: Helper unavailable")
	}
	f.helper.configure(func(fingerprint, pendingMachineID string) (*ManagedComponent, error) {
		return f.build(ctx, f.helperDef, paths.HelperConfig, fingerprint, pendingMachineID)
	})
	if f.helper.snapshot() != nil {
		return f.helper, nil
	}
	if f.fingerprints == nil {
		return f.helper, errors.New("production factory: Helper fingerprint unavailable")
	}
	fingerprint, err := f.fingerprints.HelperFingerprint()
	if err != nil {
		return f.helper, errors.New("production factory: Helper configuration unavailable")
	}
	shared, err := f.helper.initialize(fingerprint)
	if err != nil || shared == nil {
		return f.helper, err
	}
	return shared, nil
}

func (f *Factory) build(ctx context.Context, definition ComponentDefinition, configPath, fingerprint, pendingMachineID string) (*ManagedComponent, error) {
	if definition.Controller == nil || definition.Launcher == nil || definition.Inspector == nil || definition.Terminator == nil ||
		definition.ExecutablePath == "" || configPath == "" || f.readyTimeout <= 0 || f.stopTimeout <= 0 {
		return nil, errors.New("production factory: component dependencies unavailable")
	}
	controller, err := definition.Controller(ctx)
	if err != nil || controller == nil {
		return nil, errors.New("production factory: component controller unavailable")
	}
	if pendingMachineID != "" {
		updater, ok := controller.(interface {
			UpdateExpectedMachineID(string) traymodel.OperationResult
		})
		if !ok || !updater.UpdateExpectedMachineID(pendingMachineID).OK {
			return nil, errors.New("production factory: Agent identity unavailable")
		}
	}
	value := f.newSupervisor(supervisor.Spec{
		Component: definition.Component, ExecutablePath: definition.ExecutablePath, ConfigPath: configPath,
		ExpectedSHA256: fingerprint, ReadyTimeout: f.readyTimeout, StopTimeout: f.stopTimeout,
	}, definition.Launcher, definition.Inspector, controller, definition.Terminator)
	if value == nil {
		return nil, errors.New("production factory: component supervisor unavailable")
	}
	return NewManagedComponent(value, controller, definition.Inspector), nil
}

type SharedComponent struct {
	buildMu          sync.Mutex
	mu               sync.RWMutex
	build            func(string, string) (*ManagedComponent, error)
	component        *ManagedComponent
	err              error
	pendingMachineID string
}

func (s *SharedComponent) configure(build func(string, string) (*ManagedComponent, error)) {
	if s == nil || build == nil {
		return
	}
	s.mu.Lock()
	if s.build == nil {
		s.build = build
	}
	s.mu.Unlock()
}

func (s *SharedComponent) initialize(fingerprint string) (*SharedComponent, error) {
	if s == nil {
		return nil, errors.New("production factory: shared component unavailable")
	}
	s.buildMu.Lock()
	defer s.buildMu.Unlock()
	return s.initializeLocked(fingerprint)
}

func (s *SharedComponent) initializeLocked(fingerprint string) (*SharedComponent, error) {
	if s.snapshot() != nil {
		return s, nil
	}
	s.mu.RLock()
	build := s.build
	s.mu.RUnlock()
	if build == nil {
		return nil, errors.New("production factory: shared component unavailable")
	}
	s.mu.RLock()
	pendingMachineID := s.pendingMachineID
	s.mu.RUnlock()
	component, err := build(fingerprint, pendingMachineID)
	s.mu.Lock()
	s.component = component
	s.err = err
	s.mu.Unlock()
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.err != nil || s.component == nil {
		return nil, s.err
	}
	return s, nil
}

func (s *SharedComponent) snapshot() *ManagedComponent {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.component
}

func (s *SharedComponent) Adopt(ctx context.Context) traymodel.OperationResult {
	if value := s.snapshot(); value != nil {
		return value.Adopt(ctx)
	}
	return managedFailure("unavailable", "组件不可用")
}
func (s *SharedComponent) Start(ctx context.Context) traymodel.OperationResult {
	if value := s.snapshot(); value != nil {
		return value.Start(ctx)
	}
	return managedFailure("unavailable", "组件不可用")
}
func (s *SharedComponent) Stop(ctx context.Context) traymodel.OperationResult {
	if value := s.snapshot(); value != nil {
		return value.Stop(ctx)
	}
	return managedFailure("unavailable", "组件不可用")
}
func (s *SharedComponent) Restart(ctx context.Context) traymodel.OperationResult {
	if value := s.snapshot(); value != nil {
		return value.Restart(ctx)
	}
	return managedFailure("unavailable", "组件不可用")
}
func (s *SharedComponent) ForceStopTracked(ctx context.Context) traymodel.OperationResult {
	if value := s.snapshot(); value != nil {
		return value.ForceStopTracked(ctx)
	}
	return traymodel.OperationResult{OK: true}
}
func (s *SharedComponent) Refresh(ctx context.Context) traymodel.ComponentState {
	if value := s.snapshot(); value != nil {
		return value.Refresh(ctx)
	}
	return unavailableComponentState()
}
func (s *SharedComponent) UpdateExpectedSHA256(value string) traymodel.OperationResult {
	if s == nil {
		return managedFailure("unavailable", "组件不可用")
	}
	s.buildMu.Lock()
	defer s.buildMu.Unlock()
	if component := s.snapshot(); component != nil {
		return component.UpdateExpectedSHA256(value)
	}
	if _, err := s.initializeLocked(value); err != nil {
		return managedFailure("unavailable", "组件不可用")
	}
	return traymodel.OperationResult{OK: true}
}
func (s *SharedComponent) UpdateExpectedMachineID(value string) traymodel.OperationResult {
	if s == nil {
		return managedFailure("unavailable", "组件不可用")
	}
	s.buildMu.Lock()
	defer s.buildMu.Unlock()
	if component := s.snapshot(); component != nil {
		return component.UpdateExpectedMachineID(value)
	}
	if nodectl.ValidateControlIdentity(value, "fixed.exe") != nil {
		return managedFailure("invalid_config", "组件身份无效")
	}
	s.mu.Lock()
	s.pendingMachineID = value
	s.mu.Unlock()
	return traymodel.OperationResult{OK: true}
}
