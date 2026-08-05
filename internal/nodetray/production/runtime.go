package production

import (
	"context"
	"errors"
	"sync"

	trayapp "dedup/internal/nodetray/app"
	"dedup/internal/nodetray/bootstrap"
	"dedup/internal/nodetray/traymodel"
)

// EventEmitter must return when its context is cancelled. Runtime adapters
// keep Wails calls behind this injected boundary so bridge goroutines can be
// joined without blocking Supervisor or bootstrap refresh paths.
type EventEmitter func(context.Context, string, any)

type BootstrapStarter func(context.Context, bootstrap.Dependencies) (*bootstrap.Runtime, error)

type RuntimeDependencies struct {
	Bootstrap      bootstrap.Dependencies
	Events         *trayapp.EventBus
	Emitter        EventEmitter
	EventBuffer    int
	StartBootstrap BootstrapStarter
}

type Runtime struct {
	dependencies RuntimeDependencies
	startOnce    sync.Once
	started      *bootstrap.Runtime
	bridge       *EventBridge
	startErr     error
	closeOnce    sync.Once
	closeErr     error
}

func NewRuntime(dependencies RuntimeDependencies) *Runtime {
	return &Runtime{dependencies: dependencies}
}

func (r *Runtime) Start(ctx context.Context) (*bootstrap.Runtime, error) {
	if r == nil {
		return nil, errors.New("production_runtime_start_failed")
	}
	r.startOnce.Do(func() { r.start(ctx) })
	return r.started, r.startErr
}

func (r *Runtime) start(ctx context.Context) {
	bridge, err := newEventBridge(ctx, r.dependencies.Events, r.dependencies.Emitter, r.dependencies.EventBuffer)
	if err != nil {
		r.startErr = errors.New("production_runtime_start_failed")
		return
	}
	r.bridge = bridge
	dependencies := r.dependencies.Bootstrap
	if dependencies.Factory != nil {
		dependencies.Factory = &eventFactory{inner: dependencies.Factory, bus: r.dependencies.Events}
	}
	dependencies.Attention = &eventAttentionSink{bus: r.dependencies.Events, upstream: dependencies.Attention}
	starter := r.dependencies.StartBootstrap
	if starter == nil {
		starter = bootstrap.Start
	}
	started, startErr := starter(ctx, dependencies)
	if startErr != nil || started == nil {
		_ = bridge.Close()
		r.bridge = nil
		r.startErr = errors.New("production_runtime_start_failed")
		return
	}
	r.started = started
	if started.Duplicate {
		_ = bridge.Close()
		r.bridge = nil
	}
}

func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		var closeErrors []error
		if r.started != nil {
			if err := r.started.Close(); err != nil {
				closeErrors = append(closeErrors, err)
			}
		}
		if r.bridge != nil {
			if err := r.bridge.Close(); err != nil {
				closeErrors = append(closeErrors, err)
			}
		}
		if len(closeErrors) != 0 {
			r.closeErr = errors.New("production_runtime_close_failed")
		}
	})
	return r.closeErr
}

type EventBridge struct {
	cancel      context.CancelFunc
	unsubscribe func()
	done        chan struct{}
	once        sync.Once
}

func NewEventBridge(bus *trayapp.EventBus, emitter EventEmitter, buffer int) (*EventBridge, error) {
	return newEventBridge(nil, bus, emitter, buffer)
}

func newEventBridge(lifecycleContext context.Context, bus *trayapp.EventBus, emitter EventEmitter, buffer int) (*EventBridge, error) {
	if bus == nil || emitter == nil {
		return nil, errors.New("production event bridge: dependencies unavailable")
	}
	if buffer < 1 {
		buffer = 8
	}
	events, unsubscribe := bus.Subscribe(buffer)
	ctx, cancel := context.WithCancel(context.Background())
	emitContext := lifecycleContext
	if emitContext == nil {
		emitContext = ctx
	}
	bridge := &EventBridge{cancel: cancel, unsubscribe: unsubscribe, done: make(chan struct{})}
	go func() {
		defer close(bridge.done)
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-events:
				if !ok {
					return
				}
				emitRuntimeEvent(emitContext, emitter, event)
			}
		}
	}()
	return bridge, nil
}

func (b *EventBridge) Close() error {
	if b == nil {
		return nil
	}
	b.once.Do(func() {
		b.cancel()
		b.unsubscribe()
		<-b.done
	})
	return nil
}

func emitRuntimeEvent(ctx context.Context, emitter EventEmitter, event trayapp.Event) {
	switch event.Type {
	case trayapp.EventComponentState:
		if event.ComponentState != nil {
			emitter(ctx, string(trayapp.EventComponentState), *event.ComponentState)
		}
	case trayapp.EventAttentionRequired:
		if event.AttentionRequired != nil {
			emitter(ctx, string(trayapp.EventAttentionRequired), *event.AttentionRequired)
		}
	}
}

type eventFactory struct {
	inner bootstrap.Factory
	bus   *trayapp.EventBus
}

func (f *eventFactory) NewAgent(ctx context.Context, paths bootstrap.Paths) (bootstrap.Managed, error) {
	value, err := f.inner.NewAgent(ctx, paths)
	if value == nil {
		return nil, err
	}
	return newEventManaged("agent", value, f.bus), err
}

func (f *eventFactory) NewHelper(ctx context.Context, paths bootstrap.Paths) (bootstrap.Managed, error) {
	value, err := f.inner.NewHelper(ctx, paths)
	if value == nil {
		return nil, err
	}
	return newEventManaged("helper", value, f.bus), err
}

type eventManaged struct {
	component string
	inner     bootstrap.Managed
	bus       *trayapp.EventBus
}

func newEventManaged(component string, inner bootstrap.Managed, bus *trayapp.EventBus) *eventManaged {
	return &eventManaged{component: component, inner: inner, bus: bus}
}

func (m *eventManaged) Adopt(ctx context.Context) traymodel.OperationResult {
	return m.inner.Adopt(ctx)
}
func (m *eventManaged) Start(ctx context.Context) traymodel.OperationResult {
	return m.inner.Start(ctx)
}
func (m *eventManaged) Refresh(ctx context.Context) traymodel.ComponentState {
	state := m.inner.Refresh(ctx)
	m.bus.Publish(trayapp.Event{Type: trayapp.EventComponentState, ComponentState: &trayapp.ComponentStateEvent{Component: m.component, State: state}})
	if state.NeedsAttention {
		m.bus.Publish(trayapp.Event{Type: trayapp.EventAttentionRequired, AttentionRequired: &trayapp.AttentionRequiredEvent{
			Component: m.component, Code: fixedAttentionCode(state.ErrorCode), Summary: state.ErrorSummary,
		}})
	}
	return state
}

type eventAttentionSink struct {
	bus      *trayapp.EventBus
	upstream bootstrap.AttentionSink
}

func (s *eventAttentionSink) Required(component, code, summary string) {
	if s == nil {
		return
	}
	if s.upstream != nil {
		s.upstream.Required(component, code, summary)
	}
	if !fixedAttentionComponent(component) {
		component = "tray"
	}
	s.bus.Publish(trayapp.Event{Type: trayapp.EventAttentionRequired, AttentionRequired: &trayapp.AttentionRequiredEvent{
		Component: component, Code: fixedAttentionCode(code), Summary: summary,
	}})
}

func fixedAttentionComponent(component string) bool {
	switch component {
	case "agent", "helper", "tray", "config":
		return true
	default:
		return false
	}
}

func fixedAttentionCode(code string) string {
	switch code {
	case "unavailable", "invalid_config", "identity_mismatch", "unclaimed_instance", "task_unavailable", "task_failed",
		"start_failed", "ready_timeout", "unexpected_exit", "refresh_failed", "operation_failed", "tray_unavailable":
		return code
	default:
		return "operation_failed"
	}
}
