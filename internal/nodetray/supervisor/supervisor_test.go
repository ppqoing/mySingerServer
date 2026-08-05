package supervisor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"dedup/internal/nodectl"
	"dedup/internal/nodetray/process"
	"dedup/internal/nodetray/traymodel"
)

type fakeLauncher struct {
	mu         sync.Mutex
	identities []process.Identity
	err        error
	calls      int
	onStart    func(process.Identity)
	order      *[]string
}

func (f *fakeLauncher) Start(_ context.Context, _ string, _ []string, _ []string) (process.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.order != nil {
		*f.order = append(*f.order, "start")
	}
	if f.err != nil {
		return process.Identity{}, f.err
	}
	identity := f.identities[0]
	if len(f.identities) > 1 {
		f.identities = f.identities[1:]
	}
	if f.onStart != nil {
		f.onStart(identity)
	}
	return identity, nil
}

func (f *fakeLauncher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fakeInspector struct {
	mu       sync.Mutex
	current  map[int]process.Identity
	exits    chan fakeExit
	waitRuns int
	inspects int
}

type fakeExit struct {
	identity process.Identity
	code     int
	err      error
}

func newFakeInspector(identities ...process.Identity) *fakeInspector {
	current := make(map[int]process.Identity, len(identities))
	for _, identity := range identities {
		current[identity.PID] = identity
	}
	return &fakeInspector{current: current, exits: make(chan fakeExit, 16)}
}

func (f *fakeInspector) Inspect(pid int) (process.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspects++
	identity, ok := f.current[pid]
	if !ok {
		return process.Identity{}, errors.New("access denied: password=secret postgresql://user:pw@host/db C:\\private\\clip.mp4")
	}
	return identity, nil
}

func (f *fakeInspector) inspectCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inspects
}

func (f *fakeInspector) Wait(ctx context.Context, identity process.Identity) (int, error) {
	f.mu.Lock()
	f.waitRuns++
	f.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case exit := <-f.exits:
			if process.SameProcess(identity, exit.identity) {
				return exit.code, exit.err
			}
		}
	}
}

func (f *fakeInspector) set(identity process.Identity) {
	f.mu.Lock()
	f.current[identity.PID] = identity
	f.mu.Unlock()
}

func (f *fakeInspector) exit(identity process.Identity, code int) {
	f.exits <- fakeExit{identity: identity, code: code}
}

type fakeController struct {
	mu          sync.Mutex
	status      nodectl.Status
	statusErr   error
	shutdownErr error
	shutdowns   int
	statuses    int
	onShutdown  func()
	order       *[]string
}

func (f *fakeController) Status(context.Context) (nodectl.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses++
	return f.status, f.statusErr
}

func (f *fakeController) Shutdown(context.Context) error {
	f.mu.Lock()
	f.shutdowns++
	if f.order != nil {
		*f.order = append(*f.order, "shutdown")
	}
	err := f.shutdownErr
	callback := f.onShutdown
	f.mu.Unlock()
	if callback != nil {
		callback()
	}
	return err
}

func (f *fakeController) setStatus(status nodectl.Status) {
	f.mu.Lock()
	f.status = status
	f.mu.Unlock()
}

func (f *fakeController) shutdownCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.shutdowns
}

func (f *fakeController) statusCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statuses
}

type fakeTerminator struct {
	mu          sync.Mutex
	calls       []process.Identity
	err         error
	onTerminate func(process.Identity)
}

type blockingStatusController struct {
	deadlineSeen chan bool
	release      chan struct{}
}

func (c *blockingStatusController) Status(ctx context.Context) (nodectl.Status, error) {
	_, hasDeadline := ctx.Deadline()
	select {
	case c.deadlineSeen <- hasDeadline:
	default:
	}
	select {
	case <-ctx.Done():
		return nodectl.Status{}, ctx.Err()
	case <-c.release:
		return nodectl.Status{}, errors.New("test released status without deadline")
	}
}

func (*blockingStatusController) Shutdown(context.Context) error { return nil }

type blockingShutdownController struct {
	status           nodectl.Status
	statusDeadline   chan time.Time
	shutdownDeadline chan time.Time
	release          chan struct{}
}

type untilCancelledStatusController struct {
	called chan struct{}
	once   sync.Once
}

func (c *untilCancelledStatusController) Status(ctx context.Context) (nodectl.Status, error) {
	c.once.Do(func() { close(c.called) })
	<-ctx.Done()
	return nodectl.Status{}, ctx.Err()
}

func (*untilCancelledStatusController) Shutdown(context.Context) error { return nil }

func (c *blockingShutdownController) Status(ctx context.Context) (nodectl.Status, error) {
	if deadline, ok := ctx.Deadline(); ok {
		select {
		case c.statusDeadline <- deadline:
		default:
		}
	} else {
		select {
		case c.statusDeadline <- time.Time{}:
		default:
		}
	}
	return c.status, nil
}

func (c *blockingShutdownController) Shutdown(ctx context.Context) error {
	if deadline, ok := ctx.Deadline(); ok {
		select {
		case c.shutdownDeadline <- deadline:
		default:
		}
	} else {
		select {
		case c.shutdownDeadline <- time.Time{}:
		default:
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.release:
		return errors.New("test released shutdown without deadline")
	}
}

func (f *fakeTerminator) Terminate(identity process.Identity, _ uint32) error {
	f.mu.Lock()
	f.calls = append(f.calls, identity)
	err := f.err
	callback := f.onTerminate
	f.mu.Unlock()
	if callback != nil {
		callback(identity)
	}
	return err
}

func (f *fakeTerminator) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func TestStartPublishesStoppedStartingRunning(t *testing.T) {
	spec := testAgentSpec()
	identity := testIdentity(spec.ExecutablePath, 1001, 123456)
	launcher := &fakeLauncher{identities: []process.Identity{identity}}
	inspector := newFakeInspector(identity)
	controller := &fakeController{status: readyAgentStatus(spec, identity)}
	s := New(spec, launcher, inspector, controller, &fakeTerminator{})
	states, cancel := s.Subscribe(8)
	defer cancel()

	if result := s.Start(context.Background()); !result.OK {
		t.Fatalf("Start = %+v", result)
	}
	want := []traymodel.Lifecycle{traymodel.Stopped, traymodel.Starting, traymodel.Running}
	for i, lifecycle := range want {
		state := receiveState(t, states)
		if state.Lifecycle != lifecycle {
			t.Fatalf("state[%d] lifecycle = %q, want %q", i, state.Lifecycle, lifecycle)
		}
	}
}

func TestStartAcceptsSelfReportedTimeDriftAndPublishesInspectorTime(t *testing.T) {
	spec := testAgentSpec()
	identity := testIdentity(spec.ExecutablePath, 1001, 123456)
	status := readyAgentStatus(spec, identity)
	status.StartedAtUnixMS += 250
	inspector := newFakeInspector(identity)
	s := New(spec, &fakeLauncher{identities: []process.Identity{identity}}, inspector,
		&fakeController{status: status}, &fakeTerminator{})
	states, cancel := s.Subscribe(4)
	defer cancel()

	if result := s.Start(context.Background()); !result.OK {
		t.Fatalf("Start = %#v", result)
	}
	var running traymodel.ComponentState
	for range 3 {
		state := receiveState(t, states)
		if state.Lifecycle == traymodel.Running {
			running = state
		}
	}
	if running.StartedAtUnixMS != identity.StartedAtUnixMS {
		t.Fatalf("running state used self-reported identity: %#v", running)
	}
}

func TestStartTimeoutLeavesLiveProcessFailedAndDoesNotTerminate(t *testing.T) {
	spec := testAgentSpec()
	spec.ReadyTimeout = 20 * time.Millisecond
	identity := testIdentity(spec.ExecutablePath, 1001, 123456)
	status := readyAgentStatus(spec, identity)
	status.Ready = false
	status.ServiceReady = false
	terminator := &fakeTerminator{}
	s := New(spec, &fakeLauncher{identities: []process.Identity{identity}}, newFakeInspector(identity), &fakeController{status: status}, terminator)

	result := s.Start(context.Background())
	if result.OK || result.ErrorCode != "ready_timeout" {
		t.Fatalf("Start = %+v, want ready_timeout", result)
	}
	state := s.Refresh(context.Background())
	if state.Lifecycle != traymodel.Failed || !state.NeedsAttention {
		t.Fatalf("state = %+v, want failed/needs-attention", state)
	}
	if terminator.callCount() != 0 {
		t.Fatal("ready timeout implicitly terminated the process")
	}
}

func TestReadyTimeoutBoundsBlockingStatusWithDeadlineContext(t *testing.T) {
	spec := testAgentSpec()
	spec.ReadyTimeout = 30 * time.Millisecond
	identity := testIdentity(spec.ExecutablePath, 1001, 123456)
	controller := &blockingStatusController{deadlineSeen: make(chan bool, 1), release: make(chan struct{})}
	s := New(spec, &fakeLauncher{identities: []process.Identity{identity}}, newFakeInspector(identity), controller, &fakeTerminator{})
	started := time.Now()
	resultCh := make(chan traymodel.OperationResult, 1)
	go func() { resultCh <- s.Start(context.Background()) }()

	var hasDeadline bool
	select {
	case hasDeadline = <-controller.deadlineSeen:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Controller.Status was not called")
	}
	if !hasDeadline {
		close(controller.release)
	}
	result := receiveResult(t, resultCh, 200*time.Millisecond)
	if !hasDeadline {
		t.Fatal("Controller.Status received a Background context without ReadyTimeout deadline")
	}
	if result.OK || result.ErrorCode != "ready_timeout" {
		t.Fatalf("blocking Status result = %+v, want ready_timeout", result)
	}
	if elapsed := time.Since(started); elapsed > 150*time.Millisecond {
		t.Fatalf("Start exceeded ReadyTimeout budget: %v", elapsed)
	}
}

func TestStopCancelsStartingHandshakeWithoutWaitingForReadyTimeout(t *testing.T) {
	spec := testAgentSpec()
	spec.ReadyTimeout = 2 * time.Second
	identity := testIdentity(spec.ExecutablePath, 1001, 123456)
	inspector := newFakeInspector(identity)
	controller := &untilCancelledStatusController{called: make(chan struct{})}
	terminator := &fakeTerminator{onTerminate: func(target process.Identity) {
		inspector.exit(target, 1)
	}}
	s := New(spec, &fakeLauncher{identities: []process.Identity{identity}}, inspector, controller, terminator)
	startResults := make(chan traymodel.OperationResult, 1)
	go func() { startResults <- s.Start(context.Background()) }()

	select {
	case <-controller.called:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("start handshake did not begin")
	}
	started := time.Now()
	stop := s.Stop(context.Background())
	start := receiveResult(t, startResults, 200*time.Millisecond)

	if !stop.OK {
		t.Fatalf("Stop = %+v, want success", stop)
	}
	if start.OK || start.ErrorCode != "start_cancelled" {
		t.Fatalf("Start = %+v, want start_cancelled", start)
	}
	if elapsed := time.Since(started); elapsed > 150*time.Millisecond {
		t.Fatalf("Stop waited for ReadyTimeout: %v", elapsed)
	}
	if terminator.callCount() != 1 {
		t.Fatalf("terminator calls = %d, want 1", terminator.callCount())
	}
	if state := s.Refresh(context.Background()); state.Lifecycle != traymodel.Stopped {
		t.Fatalf("state = %+v, want stopped", state)
	}
}

func TestStopWaitsForClaimedExitAndNeverImplicitlyForceStops(t *testing.T) {
	spec := testAgentSpec()
	identity := testIdentity(spec.ExecutablePath, 1001, 123456)
	inspector := newFakeInspector(identity)
	controller := &fakeController{status: readyAgentStatus(spec, identity)}
	controller.onShutdown = func() { inspector.exit(identity, 0) }
	terminator := &fakeTerminator{}
	s := New(spec, &fakeLauncher{identities: []process.Identity{identity}}, inspector, controller, terminator)
	if result := s.Start(context.Background()); !result.OK {
		t.Fatal(result)
	}
	if result := s.Stop(context.Background()); !result.OK {
		t.Fatalf("Stop = %+v", result)
	}
	if state := s.Refresh(context.Background()); state.Lifecycle != traymodel.Stopped {
		t.Fatalf("state = %+v, want stopped", state)
	}
	if terminator.callCount() != 0 {
		t.Fatal("normal Stop called Terminator")
	}
}

func TestStopTimeoutNeedsExplicitForceStopOfStillMatchingClaim(t *testing.T) {
	spec := testAgentSpec()
	spec.StopTimeout = 20 * time.Millisecond
	identity := testIdentity(spec.ExecutablePath, 1001, 123456)
	inspector := newFakeInspector(identity)
	terminator := &fakeTerminator{onTerminate: func(target process.Identity) {
		inspector.exit(target, 1)
	}}
	s := New(spec, &fakeLauncher{identities: []process.Identity{identity}}, inspector, &fakeController{status: readyAgentStatus(spec, identity)}, terminator)
	if result := s.Start(context.Background()); !result.OK {
		t.Fatal(result)
	}
	result := s.Stop(context.Background())
	if result.OK || result.ErrorCode != "stop_timeout" || terminator.callCount() != 0 {
		t.Fatalf("Stop = %+v terminator=%d", result, terminator.callCount())
	}
	if result := s.ForceStopTracked(context.Background()); !result.OK {
		t.Fatalf("ForceStopTracked = %+v", result)
	}
	if terminator.callCount() != 1 {
		t.Fatalf("terminator calls = %d, want 1", terminator.callCount())
	}
}

func TestStopTimeoutIsOneDeadlineAcrossStatusShutdownAndExitWait(t *testing.T) {
	spec := testAgentSpec()
	spec.StopTimeout = 30 * time.Millisecond
	identity := testIdentity(spec.ExecutablePath, 1001, 123456)
	controller := &blockingShutdownController{
		status:           readyAgentStatus(spec, identity),
		statusDeadline:   make(chan time.Time, 2),
		shutdownDeadline: make(chan time.Time, 1),
		release:          make(chan struct{}),
	}
	s := New(spec, &fakeLauncher{identities: []process.Identity{identity}}, newFakeInspector(identity), controller, &fakeTerminator{})
	if result := s.Start(context.Background()); !result.OK {
		t.Fatalf("Start = %+v", result)
	}
	// Discard the startup Status deadline; this assertion targets Stop's budget.
	select {
	case <-controller.statusDeadline:
	default:
	}
	started := time.Now()
	resultCh := make(chan traymodel.OperationResult, 1)
	go func() { resultCh <- s.Stop(context.Background()) }()
	statusDeadline := receiveDeadline(t, controller.statusDeadline)
	shutdownDeadline := receiveDeadline(t, controller.shutdownDeadline)
	if statusDeadline.IsZero() || shutdownDeadline.IsZero() {
		close(controller.release)
	}
	result := receiveResult(t, resultCh, 200*time.Millisecond)
	if statusDeadline.IsZero() || shutdownDeadline.IsZero() {
		t.Fatalf("Stop controller contexts lacked deadline: status=%v shutdown=%v", statusDeadline, shutdownDeadline)
	}
	if !statusDeadline.Equal(shutdownDeadline) {
		t.Fatalf("Stop used separate budgets: status=%v shutdown=%v", statusDeadline, shutdownDeadline)
	}
	if result.OK || result.ErrorCode != "stop_timeout" {
		t.Fatalf("blocking Shutdown result = %+v, want stop_timeout", result)
	}
	if elapsed := time.Since(started); elapsed > 150*time.Millisecond {
		t.Fatalf("Stop exceeded one StopTimeout budget: %v", elapsed)
	}
}

func TestForceStopTrackedSkipsIdentityReinspectionAndWaitsForExit(t *testing.T) {
	spec := testAgentSpec()
	identity := testIdentity(spec.ExecutablePath, 1001, 123456)
	inspector := newFakeInspector(identity)
	terminator := &fakeTerminator{onTerminate: func(target process.Identity) {
		inspector.exit(target, 1)
	}}
	s := New(spec, &fakeLauncher{identities: []process.Identity{identity}}, inspector, &fakeController{status: readyAgentStatus(spec, identity)}, terminator)
	if result := s.Start(context.Background()); !result.OK {
		t.Fatal(result)
	}
	inspectsBeforeForce := inspector.inspectCount()
	reused := identity
	reused.StartedAtUnixMS++
	inspector.set(reused)

	result := s.ForceStopTracked(context.Background())

	if !result.OK {
		t.Fatalf("ForceStopTracked = %+v", result)
	}
	if inspector.inspectCount() != inspectsBeforeForce {
		t.Fatalf("ForceStopTracked reinspected PID: before=%d after=%d", inspectsBeforeForce, inspector.inspectCount())
	}
	if terminator.callCount() != 1 {
		t.Fatalf("terminator calls = %d, want 1", terminator.callCount())
	}
	if state := s.Refresh(context.Background()); state.Lifecycle != traymodel.Stopped {
		t.Fatalf("state = %+v, want stopped", state)
	}
}

func TestFailedCanStartAgainAndConcurrentStartsLaunchOnlyOnce(t *testing.T) {
	spec := testAgentSpec()
	identity := testIdentity(spec.ExecutablePath, 1001, 123456)
	launcher := &fakeLauncher{identities: []process.Identity{identity}}
	launcher.err = errors.New("first failure")
	controller := &fakeController{status: readyAgentStatus(spec, identity)}
	s := New(spec, launcher, newFakeInspector(identity), controller, &fakeTerminator{})
	if result := s.Start(context.Background()); result.ErrorCode != "start_failed" {
		t.Fatalf("first Start = %+v", result)
	}
	launcher.err = nil
	const callers = 8
	results := make(chan traymodel.OperationResult, callers)
	for range callers {
		go func() { results <- s.Start(context.Background()) }()
	}
	ok := 0
	for range callers {
		if (<-results).OK {
			ok++
		}
	}
	if ok != 1 || launcher.callCount() != 2 {
		t.Fatalf("successful starts=%d launcher calls=%d, want 1 and 2 total", ok, launcher.callCount())
	}
}

func TestRestartCompletesStopBeforeStartingAgain(t *testing.T) {
	spec := testAgentSpec()
	first := testIdentity(spec.ExecutablePath, 1001, 123456)
	second := testIdentity(spec.ExecutablePath, 1002, 123457)
	order := []string{}
	inspector := newFakeInspector(first, second)
	controller := &fakeController{status: readyAgentStatus(spec, first), order: &order}
	launcher := &fakeLauncher{identities: []process.Identity{first, second}, order: &order}
	launcher.onStart = func(identity process.Identity) { controller.setStatus(readyAgentStatus(spec, identity)) }
	controller.onShutdown = func() { inspector.exit(first, 0) }
	s := New(spec, launcher, inspector, controller, &fakeTerminator{})
	if result := s.Start(context.Background()); !result.OK {
		t.Fatal(result)
	}
	order = order[:0]
	launcher.order, controller.order = &order, &order
	if result := s.Restart(context.Background()); !result.OK {
		t.Fatalf("Restart = %+v", result)
	}
	if strings.Join(order, ",") != "shutdown,start" {
		t.Fatalf("operation order = %v, want shutdown then start", order)
	}
}

func TestUnexpectedExitPublishesOneFailureAndDoesNotRestart(t *testing.T) {
	spec := testAgentSpec()
	identity := testIdentity(spec.ExecutablePath, 1001, 123456)
	launcher := &fakeLauncher{identities: []process.Identity{identity}}
	inspector := newFakeInspector(identity)
	s := New(spec, launcher, inspector, &fakeController{status: readyAgentStatus(spec, identity)}, &fakeTerminator{})
	states, cancel := s.Subscribe(16)
	defer cancel()
	if result := s.Start(context.Background()); !result.OK {
		t.Fatal(result)
	}
	for range 3 {
		_ = receiveState(t, states)
	}
	inspector.exit(identity, 17)
	state := receiveState(t, states)
	if state.Lifecycle != traymodel.Failed || state.ErrorCode != "unexpected_exit" {
		t.Fatalf("exit state = %+v", state)
	}
	select {
	case extra := <-states:
		t.Fatalf("unexpected duplicate notification: %+v", extra)
	case <-time.After(30 * time.Millisecond):
	}
	if launcher.callCount() != 1 {
		t.Fatalf("unexpected exit auto-restarted %d times", launcher.callCount()-1)
	}
}

func TestStartingProcessExitInterruptsBlockingStatusImmediately(t *testing.T) {
	spec := testAgentSpec()
	spec.ReadyTimeout = 120 * time.Millisecond
	identity := testIdentity(spec.ExecutablePath, 1001, 123456)
	inspector := newFakeInspector(identity)
	launcher := &fakeLauncher{identities: []process.Identity{identity}}
	launcher.onStart = func(process.Identity) { inspector.exit(identity, 23) }
	controller := &untilCancelledStatusController{called: make(chan struct{})}
	s := New(spec, launcher, inspector, controller, &fakeTerminator{})
	states, cancel := s.Subscribe(8)
	defer cancel()
	started := time.Now()
	result := s.Start(context.Background())
	if result.OK || result.ErrorCode != "unexpected_exit" {
		t.Fatalf("Start after process exit = %+v, want unexpected_exit", result)
	}
	if elapsed := time.Since(started); elapsed > 80*time.Millisecond {
		t.Fatalf("matching exit waited for ReadyTimeout: %v", elapsed)
	}
	want := []traymodel.Lifecycle{traymodel.Stopped, traymodel.Starting, traymodel.Failed}
	for i, lifecycle := range want {
		state := receiveState(t, states)
		if state.Lifecycle != lifecycle {
			t.Fatalf("state[%d] = %+v, want lifecycle %s", i, state, lifecycle)
		}
	}
	select {
	case state := <-states:
		t.Fatalf("starting exit published duplicate state: %+v", state)
	case <-time.After(30 * time.Millisecond):
	}
	if launcher.callCount() != 1 {
		t.Fatalf("starting exit auto-restarted: launcher calls=%d", launcher.callCount())
	}
}

func TestStartingProcessExitInterruptsHandshakeBackoffImmediately(t *testing.T) {
	spec := testAgentSpec()
	spec.ReadyTimeout = 200 * time.Millisecond
	identity := testIdentity(spec.ExecutablePath, 1001, 123456)
	inspector := newFakeInspector(identity)
	status := readyAgentStatus(spec, identity)
	status.Ready = false
	status.ServiceReady = false
	launcher := &fakeLauncher{identities: []process.Identity{identity}}
	launcher.onStart = func(process.Identity) {
		time.AfterFunc(10*time.Millisecond, func() { inspector.exit(identity, 24) })
	}
	s := New(spec, launcher, inspector, &fakeController{status: status}, &fakeTerminator{})
	started := time.Now()
	result := s.Start(context.Background())
	if result.OK || result.ErrorCode != "unexpected_exit" {
		t.Fatalf("Start during backoff exit = %+v, want unexpected_exit", result)
	}
	if elapsed := time.Since(started); elapsed > 80*time.Millisecond {
		t.Fatalf("matching exit did not interrupt 100ms backoff: %v", elapsed)
	}
}

func TestHandshakeMismatchIsUnclaimedAndNeverReceivesShutdown(t *testing.T) {
	spec := testAgentSpec()
	identity := testIdentity(spec.ExecutablePath, 1001, 123456)
	status := readyAgentStatus(spec, identity)
	status.ConfigSHA256 = strings.Repeat("b", 64)
	controller := &fakeController{status: status}
	s := New(spec, &fakeLauncher{identities: []process.Identity{identity}}, newFakeInspector(identity), controller, &fakeTerminator{})
	if result := s.Start(context.Background()); result.OK || result.ErrorCode != "unclaimed_instance" {
		t.Fatalf("Start = %+v, want unclaimed_instance", result)
	}
	if result := s.Stop(context.Background()); result.OK || result.ErrorCode != "unclaimed_instance" {
		t.Fatalf("Stop = %+v, want unclaimed_instance", result)
	}
	if controller.shutdownCount() != 0 {
		t.Fatal("shutdown was sent to an unclaimed instance")
	}
}

func TestUpdateExpectedSHA256AppliesToNextStartWithoutRewritingCurrentClaim(t *testing.T) {
	oldSHA := strings.Repeat("a", 64)
	newSHA := strings.Repeat("b", 64)
	spec := testAgentSpec()
	spec.ExpectedSHA256 = oldSHA
	first := testIdentity(spec.ExecutablePath, 1001, 123456)
	second := testIdentity(spec.ExecutablePath, 1002, 123457)
	inspector := newFakeInspector(first, second)
	controller := &fakeController{status: readyAgentStatus(spec, first)}
	launcher := &fakeLauncher{identities: []process.Identity{first, second}}
	launcher.onStart = func(identity process.Identity) {
		statusSpec := spec
		if identity.PID == second.PID {
			statusSpec.ExpectedSHA256 = newSHA
		}
		controller.setStatus(readyAgentStatus(statusSpec, identity))
	}
	controller.onShutdown = func() {
		if controller.shutdownCount() == 1 {
			inspector.exit(first, 0)
		} else {
			inspector.exit(second, 0)
		}
	}
	s := New(spec, launcher, inspector, controller, &fakeTerminator{})

	if result := s.Start(context.Background()); !result.OK {
		t.Fatalf("first Start = %+v", result)
	}
	if result := s.UpdateExpectedSHA256(newSHA); !result.OK {
		t.Fatalf("UpdateExpectedSHA256 = %+v", result)
	}
	if state := s.Refresh(context.Background()); state.Lifecycle != traymodel.Running ||
		state.RuntimeConfigSHA256 != oldSHA || state.SavedConfigSHA256 != newSHA || !state.NeedsRestart {
		t.Fatalf("current claim drift state = %+v, want running old runtime/new saved/needs-restart", state)
	}
	if result := s.Stop(context.Background()); !result.OK {
		t.Fatalf("Stop old claim = %+v", result)
	}
	if result := s.Start(context.Background()); !result.OK {
		t.Fatalf("second Start = %+v", result)
	}
}

func TestUpdateExpectedSHA256RejectsInvalidValueWithoutChangingExpectation(t *testing.T) {
	spec := testAgentSpec()
	identity := testIdentity(spec.ExecutablePath, 1001, 123456)
	s := New(spec, &fakeLauncher{identities: []process.Identity{identity}}, newFakeInspector(identity), &fakeController{status: readyAgentStatus(spec, identity)}, &fakeTerminator{})

	result := s.UpdateExpectedSHA256(strings.ToUpper(spec.ExpectedSHA256))
	if result.OK || result.ErrorCode != "invalid_config" {
		t.Fatalf("invalid update = %+v", result)
	}
	if result := s.Start(context.Background()); !result.OK {
		t.Fatalf("invalid update changed valid expectation: %+v", result)
	}
}

func TestAdoptRequiresInspectorAndHandshakeIdentity(t *testing.T) {
	spec := testHelperSpec()
	identity := testIdentity(spec.ExecutablePath, 1002, 123457)
	inspector := newFakeInspector(identity)
	controller := &fakeController{status: readyHelperStatus(spec, identity)}
	s := New(spec, &fakeLauncher{}, inspector, controller, &fakeTerminator{})
	state := s.Adopt(context.Background(), identity)
	if state.Lifecycle != traymodel.Running || !state.Ready {
		t.Fatalf("adopted state = %+v", state)
	}
	drift := identity
	drift.ExecutablePath = `C:\drift\helper.exe`
	state = s.Adopt(context.Background(), drift)
	if state.ErrorCode != "unclaimed_instance" {
		t.Fatalf("drift adoption state = %+v", state)
	}
}

func TestAdoptRejectsInvalidSpecBeforeInspectingOrClaiming(t *testing.T) {
	spec := testHelperSpec()
	spec.ExpectedSHA256 = "not-a-lowercase-sha256"
	identity := testIdentity(spec.ExecutablePath, 1002, 123457)
	inspector := newFakeInspector(identity)
	controller := &fakeController{status: readyHelperStatus(spec, identity)}
	terminator := &fakeTerminator{}
	s := New(spec, &fakeLauncher{}, inspector, controller, terminator)

	state := s.Adopt(context.Background(), identity)
	if state.Lifecycle != traymodel.Failed || state.ErrorCode != "invalid_config" {
		t.Fatalf("Adopt invalid spec state = %+v, want failed/invalid_config", state)
	}
	if inspector.inspectCount() != 0 || controller.statusCount() != 0 {
		t.Fatalf("invalid spec touched process/control plane: inspect=%d status=%d", inspector.inspectCount(), controller.statusCount())
	}
	if result := s.ForceStopTracked(context.Background()); !result.OK {
		t.Fatalf("force stop with no tracked PID = %+v, want success", result)
	}
	if result := s.Stop(context.Background()); !result.OK {
		t.Fatalf("Stop after confirming no tracked PID = %+v", result)
	}
	if controller.shutdownCount() != 0 || terminator.callCount() != 0 {
		t.Fatalf("invalid spec controlled a process: shutdown=%d terminate=%d", controller.shutdownCount(), terminator.callCount())
	}
}

func TestUACCancellationPreservesOldStateAndSensitiveErrorsAreSanitized(t *testing.T) {
	spec := testHelperSpec()
	launcher := &fakeLauncher{err: &process.ErrUACCancelled{}}
	s := New(spec, launcher, newFakeInspector(), &fakeController{}, &fakeTerminator{})
	result := s.Start(context.Background())
	if !result.UACCancelled || result.OK {
		t.Fatalf("cancelled Start = %+v", result)
	}
	if state := s.Refresh(context.Background()); state.Lifecycle != traymodel.Stopped || state.ErrorCode != "" {
		t.Fatalf("UAC cancellation changed state: %+v", state)
	}

	launcher.err = errors.New(`password=hunter2 dsn=postgresql://user:secret@db/x C:\private\sample.mp4`)
	result = s.Start(context.Background())
	if result.ErrorCode != "start_failed" || strings.Contains(result.ErrorSummary, "hunter2") || strings.Contains(result.ErrorSummary, "secret@") || strings.Contains(result.ErrorSummary, "sample.mp4") {
		t.Fatalf("unsanitized failure = %+v", result)
	}
}

func testAgentSpec() Spec {
	return Spec{
		Component:      nodectl.ComponentAgent,
		ExecutablePath: `C:\Node\agent.exe`,
		ConfigPath:     `C:\Node\agent.json`,
		ExpectedSHA256: strings.Repeat("a", 64),
		ReadyTimeout:   200 * time.Millisecond,
		StopTimeout:    200 * time.Millisecond,
	}
}

func testHelperSpec() Spec {
	spec := testAgentSpec()
	spec.Component = nodectl.ComponentHelper
	spec.ExecutablePath = `C:\Node\helper.exe`
	spec.ConfigPath = `C:\Node\helper.json`
	return spec
}

func readyAgentStatus(spec Spec, identity process.Identity) nodectl.Status {
	return nodectl.Status{
		Component:       spec.Component,
		PID:             identity.PID,
		StartedAtUnixMS: identity.StartedAtUnixMS,
		ExecutablePath:  identity.ExecutablePath,
		ConfigSHA256:    spec.ExpectedSHA256,
		Lifecycle:       "running",
		ServiceReady:    true,
		Ready:           true,
		WorkerExpected:  2,
		WorkerReady:     2,
		Workers: []nodectl.WorkerStatus{
			{Index: 0, PID: 2001, Ready: true},
			{Index: 1, PID: 2002, Ready: true},
		},
		SyncHealthy: true,
	}
}

func readyHelperStatus(spec Spec, identity process.Identity) nodectl.Status {
	return nodectl.Status{
		Component:       spec.Component,
		PID:             identity.PID,
		StartedAtUnixMS: identity.StartedAtUnixMS,
		ExecutablePath:  identity.ExecutablePath,
		ConfigSHA256:    spec.ExpectedSHA256,
		Lifecycle:       "running",
		ServiceReady:    true,
		Ready:           true,
	}
}

func receiveState(t *testing.T, states <-chan traymodel.ComponentState) traymodel.ComponentState {
	t.Helper()
	select {
	case state := <-states:
		return state
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for supervisor state")
		return traymodel.ComponentState{}
	}
}

func receiveResult(t *testing.T, results <-chan traymodel.OperationResult, timeout time.Duration) traymodel.OperationResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(timeout):
		t.Fatal("timed out waiting for Supervisor operation")
		return traymodel.OperationResult{}
	}
}

func receiveDeadline(t *testing.T, deadlines <-chan time.Time) time.Time {
	t.Helper()
	select {
	case deadline := <-deadlines:
		return deadline
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out waiting for Controller context deadline")
		return time.Time{}
	}
}
