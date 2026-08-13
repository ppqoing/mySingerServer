package enum

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestAutoStartEnumeratorStartsOnceAndWaitsUntilReady(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	primary := newAvailabilityEnumerator(ErrIPC, ErrIndexNotReady, nil)
	fallback := newAvailabilityEnumerator(nil)
	polls := make(chan struct{})
	started := make(chan struct{}, 1)
	startCalls := 0
	enumr := NewAutoStartEnumerator(AutoStartOptions{
		Context:  ctx,
		Primary:  primary,
		Fallback: fallback,
		StartClient: func() error {
			startCalls++
			started <- struct{}{}
			return nil
		},
		Poll: channelPoll(polls),
	})

	result := make(chan error, 1)
	go func() {
		result <- enumr.Enum(`D:\media`, func(FileRecord) error { return nil })
	}()

	waitForAvailabilityCall(t, primary, 1)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("Everything client was not started")
	}
	polls <- struct{}{}
	waitForAvailabilityCall(t, primary, 2)
	assertStillWaiting(t, result)
	polls <- struct{}{}
	waitForAvailabilityCall(t, primary, 3)

	if err := waitForResult(t, result); err != nil {
		t.Fatalf("Enum: %v", err)
	}
	if startCalls != 1 {
		t.Fatalf("StartClient calls = %d, want 1", startCalls)
	}
	if primary.enumCallCount() != 1 {
		t.Fatalf("primary Enum calls = %d, want 1", primary.enumCallCount())
	}
	if fallback.enumCallCount() != 0 {
		t.Fatalf("fallback Enum calls = %d, want 0", fallback.enumCallCount())
	}
}

func TestAutoStartEnumeratorDoesNotStartWhenAlreadyReady(t *testing.T) {
	primary := newAvailabilityEnumerator(nil)
	fallback := newAvailabilityEnumerator(nil)
	startCalls := 0
	enumr := NewAutoStartEnumerator(AutoStartOptions{
		Context:  context.Background(),
		Primary:  primary,
		Fallback: fallback,
		StartClient: func() error {
			startCalls++
			return nil
		},
	})

	if err := enumr.Enum(`D:\media`, func(FileRecord) error { return nil }); err != nil {
		t.Fatalf("Enum: %v", err)
	}
	if startCalls != 0 {
		t.Fatalf("StartClient calls = %d, want 0", startCalls)
	}
	if primary.enumCallCount() != 1 || fallback.enumCallCount() != 0 {
		t.Fatalf(
			"Enum calls primary=%d fallback=%d, want primary only",
			primary.enumCallCount(),
			fallback.enumCallCount(),
		)
	}
}

func TestAutoStartEnumeratorWaitsWithoutTimeoutWhileIndexLoads(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	primary := newAvailabilityEnumerator(ErrIndexNotReady)
	fallback := newAvailabilityEnumerator(nil)
	polls := make(chan struct{})
	startCalls := 0
	enumr := NewAutoStartEnumerator(AutoStartOptions{
		Context:  ctx,
		Primary:  primary,
		Fallback: fallback,
		StartClient: func() error {
			startCalls++
			return nil
		},
		Poll: channelPoll(polls),
	})

	result := make(chan error, 1)
	go func() {
		result <- enumr.Enum(`D:\media`, func(FileRecord) error { return nil })
	}()
	waitForAvailabilityCall(t, primary, 1)
	for call := 2; call <= 4; call++ {
		polls <- struct{}{}
		waitForAvailabilityCall(t, primary, call)
	}
	assertStillWaiting(t, result)
	if startCalls != 0 {
		t.Fatalf("StartClient calls = %d, want 0 while database loads", startCalls)
	}
	if fallback.enumCallCount() != 0 {
		t.Fatalf("fallback Enum calls = %d, want 0", fallback.enumCallCount())
	}

	cancel()
	if err := waitForResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("Enum error = %v, want context.Canceled", err)
	}
}

func TestAutoStartEnumeratorCancelsWaitingWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	primary := newAvailabilityEnumerator(ErrIndexNotReady)
	enumr := NewAutoStartEnumerator(AutoStartOptions{
		Context:  ctx,
		Primary:  primary,
		Fallback: newAvailabilityEnumerator(nil),
		Poll: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
	})

	result := make(chan error, 1)
	go func() {
		result <- enumr.Enum(`D:\media`, func(FileRecord) error { return nil })
	}()
	waitForAvailabilityCall(t, primary, 1)
	cancel()
	if err := waitForResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("Enum error = %v, want context.Canceled", err)
	}
}

func TestAutoStartEnumeratorFallsBackOnExecutableStartFailure(t *testing.T) {
	want := errors.New("start Everything failed")
	primary := newAvailabilityEnumerator(ErrIPC)
	fallback := newAvailabilityEnumerator(nil)
	var fallbackCause error
	enumr := NewAutoStartEnumerator(AutoStartOptions{
		Context:     context.Background(),
		Primary:     primary,
		Fallback:    fallback,
		StartClient: func() error { return want },
		OnFallback:  func(err error) { fallbackCause = err },
	})

	if err := enumr.Enum(`D:\media`, func(FileRecord) error { return nil }); err != nil {
		t.Fatalf("Enum: %v", err)
	}
	if !errors.Is(fallbackCause, want) {
		t.Fatalf("fallback cause = %v, want %v", fallbackCause, want)
	}
	if fallback.enumCallCount() != 1 || primary.enumCallCount() != 0 {
		t.Fatalf(
			"Enum calls primary=%d fallback=%d, want fallback only",
			primary.enumCallCount(),
			fallback.enumCallCount(),
		)
	}
}

func TestAutoStartEnumeratorFallsBackOnPermanentSDKFailure(t *testing.T) {
	want := errors.New("load Everything64.dll failed")
	primary := newAvailabilityEnumerator(want)
	fallback := newAvailabilityEnumerator(nil)
	startCalls := 0
	var fallbackCause error
	enumr := NewAutoStartEnumerator(AutoStartOptions{
		Context:  context.Background(),
		Primary:  primary,
		Fallback: fallback,
		StartClient: func() error {
			startCalls++
			return nil
		},
		OnFallback: func(err error) { fallbackCause = err },
	})

	if err := enumr.Enum(`D:\media`, func(FileRecord) error { return nil }); err != nil {
		t.Fatalf("Enum: %v", err)
	}
	if !errors.Is(fallbackCause, want) {
		t.Fatalf("fallback cause = %v, want %v", fallbackCause, want)
	}
	if startCalls != 0 {
		t.Fatalf("StartClient calls = %d, want 0", startCalls)
	}
	if fallback.enumCallCount() != 1 || primary.enumCallCount() != 0 {
		t.Fatalf(
			"Enum calls primary=%d fallback=%d, want fallback only",
			primary.enumCallCount(),
			fallback.enumCallCount(),
		)
	}
}

type availabilityEnumerator struct {
	mu           sync.Mutex
	availability []error
	available    int
	enumCalls    int
	callObserved chan int
}

func newAvailabilityEnumerator(availability ...error) *availabilityEnumerator {
	return &availabilityEnumerator{
		availability: availability,
		callObserved: make(chan int, 16),
	}
}

func (e *availabilityEnumerator) Name() string { return "scripted" }

func (e *availabilityEnumerator) Available() error {
	e.mu.Lock()
	index := e.available
	e.available++
	call := e.available
	var err error
	if len(e.availability) > 0 {
		if index >= len(e.availability) {
			index = len(e.availability) - 1
		}
		err = e.availability[index]
	}
	e.mu.Unlock()
	e.callObserved <- call
	return err
}

func (e *availabilityEnumerator) Enum(
	_ string,
	visit func(FileRecord) error,
) error {
	e.mu.Lock()
	e.enumCalls++
	e.mu.Unlock()
	return visit(FileRecord{Path: `D:\media\ready.bin`})
}

func (e *availabilityEnumerator) enumCallCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.enumCalls
}

func waitForAvailabilityCall(
	t *testing.T,
	enumr *availabilityEnumerator,
	want int,
) {
	t.Helper()
	for {
		select {
		case got := <-enumr.callObserved:
			if got >= want {
				return
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for Available call %d", want)
		}
	}
}

func channelPoll(polls <-chan struct{}) func(context.Context) error {
	return func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-polls:
			return nil
		}
	}
}

func assertStillWaiting(t *testing.T, result <-chan error) {
	t.Helper()
	select {
	case err := <-result:
		t.Fatalf("Enum completed while Everything was not ready: %v", err)
	default:
	}
}

func waitForResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Enum result")
		return nil
	}
}
