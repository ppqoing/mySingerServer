package app

import (
	"sync"

	"dedup/internal/nodetray/traymodel"
)

type EventType string

const (
	EventComponentState    EventType = "component-state"
	EventOperationProgress EventType = "operation-progress"
	EventAttentionRequired EventType = "attention-required"
	EventSettingsChanged   EventType = "settings-changed"
)

type ComponentStateEvent struct {
	Component string                   `json:"component"`
	State     traymodel.ComponentState `json:"state"`
}

type OperationProgressEvent struct {
	Operation string `json:"operation"`
	Summary   string `json:"summary"`
}

type AttentionRequiredEvent struct {
	Component string `json:"component"`
	Code      string `json:"code"`
	Summary   string `json:"summary"`
}

type SettingsChangedEvent struct {
	Summary string `json:"summary"`
}

// Event uses one private, typed payload branch. It deliberately has no map or
// interface field through which arbitrary UI data could enter.
type Event struct {
	Type              EventType               `json:"type"`
	ComponentState    *ComponentStateEvent    `json:"componentState,omitempty"`
	OperationProgress *OperationProgressEvent `json:"operationProgress,omitempty"`
	AttentionRequired *AttentionRequiredEvent `json:"attentionRequired,omitempty"`
	SettingsChanged   *SettingsChangedEvent   `json:"settingsChanged,omitempty"`
}

type eventSubscription struct{ channel chan Event }

type EventBus struct {
	mu        sync.Mutex
	capacity  int
	closed    bool
	nextID    int
	subs      map[int]*eventSubscription
	testHooks eventBusTestHooks
}

type eventBusTestHooks struct{ beforeReplace func(chan Event) }

func NewEventBus(capacity int) *EventBus {
	if capacity < 1 {
		capacity = 1
	}
	return &EventBus{capacity: capacity, subs: make(map[int]*eventSubscription)}
}

func (bus *EventBus) Subscribe(buffer int) (<-chan Event, func()) {
	if bus == nil {
		closed := make(chan Event)
		close(closed)
		return closed, func() {}
	}
	bus.mu.Lock()
	if buffer < 1 {
		buffer = bus.capacity
	}
	if buffer > bus.capacity {
		buffer = bus.capacity
	}
	if bus.closed {
		bus.mu.Unlock()
		closed := make(chan Event)
		close(closed)
		return closed, func() {}
	}
	bus.nextID++
	id := bus.nextID
	subscription := &eventSubscription{channel: make(chan Event, buffer)}
	bus.subs[id] = subscription
	bus.mu.Unlock()
	var once sync.Once
	return subscription.channel, func() {
		once.Do(func() {
			bus.mu.Lock()
			defer bus.mu.Unlock()
			if existing, ok := bus.subs[id]; ok {
				delete(bus.subs, id)
				close(existing.channel)
			}
		})
	}
}

// Publish is non-blocking. A full queue replaces an older component-state for
// the same component. Other event classes are rejected with false so callers
// can surface that delivery failed rather than claiming success.
func (bus *EventBus) Publish(event Event) bool {
	if bus == nil || !validEvent(event) {
		return false
	}
	event = sanitizeEvent(event)
	bus.mu.Lock()
	defer bus.mu.Unlock()
	if bus.closed || len(bus.subs) == 0 {
		return false
	}
	accepted := false
	for _, subscription := range bus.subs {
		select {
		case subscription.channel <- event:
			accepted = true
		default:
			if event.Type == EventComponentState {
				if hook := bus.testHooks.beforeReplace; hook != nil {
					hook(subscription.channel)
				}
				if replaceComponentState(subscription.channel, event) {
					accepted = true
				}
			}
		}
	}
	return accepted
}

func (bus *EventBus) Close() {
	if bus == nil {
		return
	}
	bus.mu.Lock()
	defer bus.mu.Unlock()
	if bus.closed {
		return
	}
	bus.closed = true
	for id, subscription := range bus.subs {
		close(subscription.channel)
		delete(bus.subs, id)
	}
}

func replaceComponentState(channel chan Event, replacement Event) bool {
	queued := make([]Event, 0, cap(channel))
	for {
		select {
		case event := <-channel:
			queued = append(queued, event)
		default:
			goto drained
		}
	}
drained:
	filtered := queued[:0]
	for _, event := range queued {
		if event.Type == EventComponentState && event.ComponentState != nil && replacement.ComponentState != nil && event.ComponentState.Component == replacement.ComponentState.Component {
			continue
		}
		filtered = append(filtered, event)
	}
	for _, event := range filtered {
		select {
		case channel <- event:
		default:
		}
	}
	select {
	case channel <- replacement:
		return true
	default:
		return false
	}
}

func validEvent(event Event) bool {
	count := 0
	if event.ComponentState != nil {
		count++
	}
	if event.OperationProgress != nil {
		count++
	}
	if event.AttentionRequired != nil {
		count++
	}
	if event.SettingsChanged != nil {
		count++
	}
	if count != 1 {
		return false
	}
	switch event.Type {
	case EventComponentState:
		return event.ComponentState != nil && (event.ComponentState.Component == "agent" || event.ComponentState.Component == "helper")
	case EventOperationProgress:
		return event.OperationProgress != nil
	case EventAttentionRequired:
		return event.AttentionRequired != nil
	case EventSettingsChanged:
		return event.SettingsChanged != nil
	default:
		return false
	}
}

func sanitizeEvent(event Event) Event {
	if event.ComponentState != nil {
		value := *event.ComponentState
		value.Component = sanitizeText(value.Component)
		value.State = sanitizeComponentState(value.State)
		event.ComponentState = &value
	}
	if event.OperationProgress != nil {
		value := *event.OperationProgress
		value.Operation = sanitizeText(value.Operation)
		value.Summary = sanitizeText(value.Summary)
		event.OperationProgress = &value
	}
	if event.AttentionRequired != nil {
		value := *event.AttentionRequired
		value.Component = sanitizeText(value.Component)
		value.Code = sanitizeText(value.Code)
		value.Summary = sanitizeText(value.Summary)
		event.AttentionRequired = &value
	}
	if event.SettingsChanged != nil {
		value := *event.SettingsChanged
		value.Summary = sanitizeText(value.Summary)
		event.SettingsChanged = &value
	}
	return event
}
