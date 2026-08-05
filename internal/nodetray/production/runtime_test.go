package production

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	trayapp "dedup/internal/nodetray/app"
	"dedup/internal/nodetray/bootstrap"
	"dedup/internal/nodetray/traymodel"
)

type runtimeManaged struct {
	name  string
	calls *[]string
	state traymodel.ComponentState
}

type runtimeFactory struct {
	agent bootstrap.Managed
	err   error
}

func (f runtimeFactory) NewAgent(context.Context, bootstrap.Paths) (bootstrap.Managed, error) {
	return f.agent, f.err
}
func (f runtimeFactory) NewHelper(context.Context, bootstrap.Paths) (bootstrap.Managed, error) {
	return nil, errors.New("unused")
}

func (m *runtimeManaged) Adopt(context.Context) traymodel.OperationResult {
	return traymodel.OperationResult{OK: true}
}
func (m *runtimeManaged) Start(context.Context) traymodel.OperationResult {
	return traymodel.OperationResult{OK: true}
}
func (m *runtimeManaged) Refresh(context.Context) traymodel.ComponentState {
	*m.calls = append(*m.calls, m.name+"-refresh")
	return m.state
}

type emittedRuntimeEvent struct {
	name    string
	payload any
}

func TestEventingRefreshPublishesExactSanitizedComponentAndAttentionPayloadsInOrder(t *testing.T) {
	bus := trayapp.NewEventBus(8)
	events := make(chan emittedRuntimeEvent, 4)
	bridge, err := NewEventBridge(bus, func(_ context.Context, name string, payload any) {
		events <- emittedRuntimeEvent{name: name, payload: payload}
	}, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	calls := []string{}
	state := traymodel.ComponentState{Lifecycle: traymodel.Failed, PID: 42, ErrorCode: "unexpected_exit", ErrorSummary: `password=hunter2 C:\private\agent.log`, NeedsAttention: true}
	wrapped := newEventManaged("agent", &runtimeManaged{name: "agent", calls: &calls, state: state}, bus)
	got := wrapped.Refresh(context.Background())
	if got != state {
		t.Fatalf("Refresh state = %#v", got)
	}
	if !reflect.DeepEqual(calls, []string{"agent-refresh"}) {
		t.Fatalf("Refresh calls = %v", calls)
	}
	wantNames := []string{"component-state", "attention-required"}
	for index, wantName := range wantNames {
		select {
		case event := <-events:
			if event.name != wantName {
				t.Fatalf("event[%d] name = %q, want %q", index, event.name, wantName)
			}
			switch payload := event.payload.(type) {
			case trayapp.ComponentStateEvent:
				if payload.Component != "agent" || payload.State.ErrorSummary != "[REDACTED] [REDACTED_PATH]" {
					t.Fatalf("component payload = %#v", payload)
				}
			case trayapp.AttentionRequiredEvent:
				if payload.Component != "agent" || payload.Code != "unexpected_exit" || payload.Summary != "[REDACTED] [REDACTED_PATH]" {
					t.Fatalf("attention payload = %#v", payload)
				}
			default:
				t.Fatalf("event[%d] payload type = %T", index, event.payload)
			}
		case <-time.After(time.Second):
			t.Fatalf("missing %s event", wantName)
		}
	}
	select {
	case event := <-events:
		t.Fatalf("unexpected Worker or operation event: %#v", event)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestEventFactoryPreservesUnavailableSharedComponentForRefresh(t *testing.T) {
	bus := trayapp.NewEventBus(2)
	events, unsubscribe := bus.Subscribe(2)
	defer unsubscribe()
	calls := []string{}
	shared := &runtimeManaged{name: "agent", calls: &calls, state: traymodel.ComponentState{Lifecycle: traymodel.Failed, ErrorCode: "unavailable", NeedsAttention: true}}
	factory := &eventFactory{inner: runtimeFactory{agent: shared, err: errors.New("configuration unavailable")}, bus: bus}
	component, err := factory.NewAgent(context.Background(), bootstrap.Paths{})
	if component == nil || err == nil {
		t.Fatalf("event factory unavailable component=%#v err=%v", component, err)
	}
	component.Refresh(context.Background())
	select {
	case event := <-events:
		if event.Type != trayapp.EventComponentState || event.ComponentState == nil || event.ComponentState.State.ErrorCode != "unavailable" {
			t.Fatalf("unavailable refresh event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("unavailable component refresh was not published")
	}
}

func TestSlowEmitterNeverBlocksRefreshAndCloseStopsFutureEmission(t *testing.T) {
	bus := trayapp.NewEventBus(2)
	entered := make(chan struct{}, 1)
	bridge, err := NewEventBridge(bus, func(ctx context.Context, _ string, _ any) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
	}, 2)
	if err != nil {
		t.Fatal(err)
	}
	calls := []string{}
	wrapped := newEventManaged("helper", &runtimeManaged{name: "helper", calls: &calls, state: traymodel.ComponentState{Lifecycle: traymodel.Running}}, bus)
	returned := make(chan struct{})
	go func() {
		wrapped.Refresh(context.Background())
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("slow emitter blocked component refresh")
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("emitter was not reached")
	}
	if err := bridge.Close(); err != nil {
		t.Fatal(err)
	}
	if err := bridge.Close(); err != nil {
		t.Fatal(err)
	}
	wrapped.Refresh(context.Background())
	select {
	case <-entered:
		t.Fatal("emitted after Close")
	case <-time.After(25 * time.Millisecond):
	}
}

type runtimeStarter struct {
	mu        sync.Mutex
	calls     int
	duplicate bool
	err       error
	seen      bootstrap.Dependencies
}

func (s *runtimeStarter) Start(_ context.Context, dependencies bootstrap.Dependencies) (*bootstrap.Runtime, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.seen = dependencies
	if s.err != nil {
		return nil, s.err
	}
	return &bootstrap.Runtime{Duplicate: s.duplicate}, nil
}

func TestRuntimeConstructionHasNoSideEffectsAndDuplicateStartsNoEmitter(t *testing.T) {
	starter := &runtimeStarter{duplicate: true}
	emits := 0
	runtime := NewRuntime(RuntimeDependencies{
		Bootstrap:      bootstrap.Dependencies{},
		Events:         trayapp.NewEventBus(2),
		Emitter:        func(context.Context, string, any) { emits++ },
		StartBootstrap: starter.Start,
	})
	if starter.calls != 0 || emits != 0 {
		t.Fatalf("constructor side effects starter=%d emits=%d", starter.calls, emits)
	}
	started, err := runtime.Start(context.Background())
	if err != nil || started == nil || !started.Duplicate {
		t.Fatalf("duplicate Start = %#v, %v", started, err)
	}
	if starter.calls != 1 || emits != 0 {
		t.Fatalf("duplicate side effects starter=%d emits=%d", starter.calls, emits)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeEventsUseTheExactLifecycleContext(t *testing.T) {
	type lifecycleContextKey struct{}
	wailsContext := context.WithValue(context.Background(), lifecycleContextKey{}, "exact-wails-context")
	bus := trayapp.NewEventBus(2)
	emittedContexts := make(chan context.Context, 1)
	runtime := NewRuntime(RuntimeDependencies{
		Bootstrap: bootstrap.Dependencies{},
		Events:    bus,
		Emitter: func(ctx context.Context, _ string, _ any) {
			emittedContexts <- ctx
		},
		StartBootstrap: (&runtimeStarter{}).Start,
	})
	started, err := runtime.Start(wailsContext)
	if err != nil || started == nil {
		t.Fatalf("Start = %#v, %v", started, err)
	}
	defer runtime.Close()
	bus.Publish(trayapp.Event{
		Type: trayapp.EventAttentionRequired,
		AttentionRequired: &trayapp.AttentionRequiredEvent{
			Component: "agent", Code: "unavailable", Summary: "Agent 尚未配置",
		},
	})
	select {
	case got := <-emittedContexts:
		if got != wailsContext {
			t.Fatalf("event emitter received derived context %T instead of exact lifecycle context", got)
		}
	case <-time.After(time.Second):
		t.Fatal("runtime event was not emitted")
	}
}

func TestRuntimeStartupErrorsAreStableAndRedacted(t *testing.T) {
	starter := &runtimeStarter{err: errors.New(`postgres://user:secret@private/db C:\private\tray.json`)}
	runtime := NewRuntime(RuntimeDependencies{Events: trayapp.NewEventBus(2), Emitter: func(context.Context, string, any) {}, StartBootstrap: starter.Start})
	started, err := runtime.Start(context.Background())
	if started != nil || err == nil || err.Error() != "production_runtime_start_failed" {
		t.Fatalf("Start failure = %#v, %v", started, err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "secret") || strings.Contains(strings.ToLower(err.Error()), "private") {
		t.Fatalf("startup error leaked raw data: %v", err)
	}
}
