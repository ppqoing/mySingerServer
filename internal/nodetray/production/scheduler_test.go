package production

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeRuntimeTicker struct {
	ch       chan time.Time
	mu       sync.Mutex
	stopCall int
}

func newFakeRuntimeTicker() *fakeRuntimeTicker {
	return &fakeRuntimeTicker{ch: make(chan time.Time, 4)}
}
func (t *fakeRuntimeTicker) C() <-chan time.Time { return t.ch }
func (t *fakeRuntimeTicker) Stop() {
	t.mu.Lock()
	t.stopCall++
	t.mu.Unlock()
}

func TestSchedulerUsesInjectedVisibleAndRecoveryTickersAndClosesOnce(t *testing.T) {
	visible := newFakeRuntimeTicker()
	recovery := newFakeRuntimeTicker()
	var durations []time.Duration
	scheduler := NewScheduler(func(duration time.Duration) RuntimeTicker {
		durations = append(durations, duration)
		if len(durations) == 1 {
			return visible
		}
		return recovery
	})
	refreshed := make(chan struct{}, 4)
	closer, err := scheduler.Start(context.Background(), 2*time.Second, 10*time.Second, func(context.Context) { refreshed <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	if len(durations) != 2 || durations[0] != 2*time.Second || durations[1] != 10*time.Second {
		t.Fatalf("ticker durations = %v", durations)
	}
	visible.ch <- time.Now()
	recovery.ch <- time.Now()
	for i := 0; i < 2; i++ {
		select {
		case <-refreshed:
		case <-time.After(time.Second):
			t.Fatal("scheduler did not refresh from both injected tickers")
		}
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	visible.ch <- time.Now()
	select {
	case <-refreshed:
		t.Fatal("scheduler refreshed after Close")
	case <-time.After(25 * time.Millisecond):
	}
	visible.mu.Lock()
	visibleStops := visible.stopCall
	visible.mu.Unlock()
	recovery.mu.Lock()
	recoveryStops := recovery.stopCall
	recovery.mu.Unlock()
	if visibleStops != 1 || recoveryStops != 1 {
		t.Fatalf("ticker Stop calls = %d/%d", visibleStops, recoveryStops)
	}
}
