//go:build windows

package singleinstance

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/google/uuid"
)

func TestAcquireTrayRejectsDuplicateAndCanBeReacquiredAfterClose(t *testing.T) {
	useGUIDNamespace(t)
	const sid = "S-1-5-21-101-202-303-1001"

	first, err := AcquireTray(sid)
	if err != nil {
		t.Fatalf("AcquireTray first: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })

	if _, err := AcquireTray(sid); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("AcquireTray duplicate error = %v, want ErrAlreadyExists", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close first lease: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close must be idempotent: %v", err)
	}

	reacquired, err := AcquireTray(sid)
	if err != nil {
		t.Fatalf("AcquireTray after close: %v", err)
	}
	if err := reacquired.Close(); err != nil {
		t.Fatalf("Close reacquired lease: %v", err)
	}
}

func TestAcquireTrayIsolatesDifferentUsers(t *testing.T) {
	useGUIDNamespace(t)
	a, err := AcquireTray("S-1-5-21-101-202-303-1001")
	if err != nil {
		t.Fatalf("AcquireTray user A: %v", err)
	}
	defer a.Close()
	b, err := AcquireTray("S-1-5-21-101-202-303-1002")
	if err != nil {
		t.Fatalf("AcquireTray user B: %v", err)
	}
	defer b.Close()
}

func TestAcquireAgentNormalizesEquivalentMachineIDs(t *testing.T) {
	useGUIDNamespace(t)
	first, err := AcquireAgent("Node-Alpha")
	if err != nil {
		t.Fatalf("AcquireAgent first: %v", err)
	}
	defer first.Close()

	if _, err := AcquireAgent("node-alpha"); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("AcquireAgent equivalent ID error = %v, want ErrAlreadyExists", err)
	}
	other, err := AcquireAgent("node-beta")
	if err != nil {
		t.Fatalf("AcquireAgent other ID: %v", err)
	}
	defer other.Close()
}

func TestAcquireRejectsUnsafeIdentifiers(t *testing.T) {
	useGUIDNamespace(t)
	invalidSIDs := []string{"", " S-1-5-18", "S-1-5-18\\child", "S-1-5-18\n"}
	for _, value := range invalidSIDs {
		if lease, err := AcquireTray(value); err == nil {
			_ = lease.Close()
			t.Fatalf("AcquireTray accepted unsafe SID %q", value)
		}
	}
	invalidMachineIDs := []string{"", " node", "node ", "node/path", `node\path`, "C:", "node\npath", strings.Repeat("x", 129)}
	for _, value := range invalidMachineIDs {
		if lease, err := AcquireAgent(value); err == nil {
			_ = lease.Close()
			t.Fatalf("AcquireAgent accepted unsafe machine ID %q", value)
		}
	}
}

func TestActivationAcceptsOnlyOneFixedFrameAndCancelsPromptly(t *testing.T) {
	useGUIDNamespace(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var shown atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- ListenActivation(ctx, func() { shown.Add(1) })
	}()

	waitForActivationListener(t)
	sendRawActivation(t, []byte("show-window-with-arguments"))
	sendRawActivation(t, []byte(strings.Repeat("x", maxActivationMessageBytes+1)))
	sendRawActivation(t, append(append([]byte(nil), activationMessage...), activationMessage...))
	if got := shown.Load(); got != 0 {
		t.Fatalf("malformed activation called show %d times", got)
	}

	signalCtx, signalCancel := context.WithTimeout(context.Background(), time.Second)
	defer signalCancel()
	if err := SignalExisting(signalCtx); err != nil {
		t.Fatalf("SignalExisting: %v", err)
	}
	waitUntil(t, time.Second, func() bool { return shown.Load() == 1 })

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ListenActivation after cancel = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ListenActivation did not exit promptly after cancellation")
	}
}

func TestSignalExistingReturnsStableErrorsWhenAbsentOrCancelled(t *testing.T) {
	useGUIDNamespace(t)
	backgroundResult := make(chan error, 1)
	go func() { backgroundResult <- SignalExisting(context.Background()) }()
	select {
	case err := <-backgroundResult:
		if !errors.Is(err, ErrNoExistingInstance) {
			t.Fatalf("SignalExisting background absent error = %v, want ErrNoExistingInstance", err)
		}
	case <-time.After(time.Second):
		t.Fatal("SignalExisting without caller deadline did not apply its own deadline")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	if err := SignalExisting(ctx); !errors.Is(err, ErrNoExistingInstance) {
		t.Fatalf("SignalExisting absent error = %v, want ErrNoExistingInstance", err)
	}

	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if err := SignalExisting(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("SignalExisting cancelled error = %v, want context.Canceled", err)
	}
}

func TestSignalExistingAppliesDeadlineWhenActivationPipeIsBusy(t *testing.T) {
	useGUIDNamespace(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ListenActivation(ctx, func() {}) }()
	waitForActivationListener(t)

	blockerCtx, blockerCancel := context.WithTimeout(context.Background(), time.Second)
	blocker, err := winio.DialPipeContext(blockerCtx, activationPipeName())
	blockerCancel()
	if err != nil {
		cancel()
		<-done
		t.Fatalf("dial blocking activation connection: %v", err)
	}

	result := make(chan error, 1)
	go func() { result <- SignalExisting(context.Background()) }()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("SignalExisting unexpectedly succeeded while listener was blocked by an incomplete frame")
		}
	case <-time.After(time.Second):
		_ = blocker.Close()
		cancel()
		<-done
		t.Fatal("SignalExisting did not apply an internal deadline while pipe was busy")
	}

	_ = blocker.Close()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("ListenActivation cleanup = %v, want context.Canceled", err)
	}
}

func useGUIDNamespace(t *testing.T) {
	t.Helper()
	old := instanceNamespace
	instanceNamespace = "test-" + uuid.NewString()
	t.Cleanup(func() { instanceNamespace = old })
}

func waitForActivationListener(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		conn, err := winio.DialPipeContext(ctx, activationPipeName())
		cancel()
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("activation listener did not become ready")
}

func sendRawActivation(t *testing.T, payload []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := winio.DialPipeContext(ctx, activationPipeName())
	if err != nil {
		t.Fatalf("dial activation pipe: %v", err)
	}
	defer conn.Close()
	if err := conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set activation write deadline: %v", err)
	}
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write activation frame: %v", err)
	}
}

func waitUntil(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition was not satisfied before timeout")
}
