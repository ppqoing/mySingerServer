package supervisor

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"dedup/internal/nodectl"
	"dedup/internal/nodetray/process"
	"dedup/internal/nodetray/traymodel"
)

type Launcher interface {
	Start(ctx context.Context, executable string, args []string, env []string) (process.Identity, error)
}

type ElevatedHelperLauncher interface {
	StartHelper(ctx context.Context, helperExecutable string, helperConfig string) (process.Identity, error)
}

type Terminator interface {
	Terminate(identity process.Identity, exitCode uint32) error
}

type Controller interface {
	Status(ctx context.Context) (nodectl.Status, error)
	Shutdown(ctx context.Context) error
}

type Spec struct {
	Component      nodectl.Component
	ExecutablePath string
	ConfigPath     string
	ExpectedSHA256 string
	ReadyTimeout   time.Duration
	StopTimeout    time.Duration
}

type command struct {
	run   func() any
	reply chan any
}

type exitEvent struct {
	identity process.Identity
	code     int
	err      error
}

type statusResult struct {
	status nodectl.Status
	err    error
}

type operationKind string

const (
	operationStart   operationKind = "start"
	operationStop    operationKind = "stop"
	operationRestart operationKind = "restart"
	operationForce   operationKind = "force-stop"
)

type operationCompletion struct {
	result     traymodel.OperationResult
	stopResult traymodel.OperationResult
}

type activeOperation struct {
	kind          operationKind
	ctx           context.Context
	cancel        context.CancelFunc
	done          chan operationCompletion
	stopRequested bool
	stopResult    traymodel.OperationResult
}

type Supervisor struct {
	spec       Spec
	launcher   Launcher
	inspector  process.Inspector
	controller Controller
	terminator Terminator

	commands chan command
	exits    chan exitEvent

	operationMu sync.Mutex
	active      *activeOperation

	state      traymodel.ComponentState
	launched   process.Identity
	claimed    process.Identity
	claimedSHA string
	waitCancel context.CancelFunc
	subs       map[int]chan traymodel.ComponentState
	nextSubID  int
}

func New(spec Spec, launcher Launcher, inspector process.Inspector, controller Controller, terminator Terminator) *Supervisor {
	s := &Supervisor{
		spec:       spec,
		launcher:   launcher,
		inspector:  inspector,
		controller: controller,
		terminator: terminator,
		commands:   make(chan command),
		exits:      make(chan exitEvent, 4),
		state:      traymodel.ComponentState{Lifecycle: traymodel.Stopped},
		subs:       make(map[int]chan traymodel.ComponentState),
	}
	go s.loop()
	return s
}

func (s *Supervisor) Start(ctx context.Context) traymodel.OperationResult {
	operation, conflict := s.beginOperation(operationStart, ctx)
	if conflict != nil {
		return *conflict
	}
	result := invoke[traymodel.OperationResult](s, func() any { return s.startLocked(operation.ctx, operation) })
	if s.startStopRequested(operation) && result.OK {
		result = invoke[traymodel.OperationResult](s, func() any { return s.cancelStartLocked(operation, s.launched) })
	}
	s.completeOperation(operation, operationCompletion{result: result, stopResult: s.startStopResult(operation)})
	return result
}

func (s *Supervisor) Stop(ctx context.Context) traymodel.OperationResult {
	if completion, handled := s.cancelActiveStart(); handled {
		return completion.stopResult
	}
	operation, conflict := s.beginOperation(operationStop, ctx)
	if conflict != nil {
		return *conflict
	}
	result := invoke[traymodel.OperationResult](s, func() any { return s.stopLocked(operation.ctx) })
	s.completeOperation(operation, operationCompletion{result: result})
	return result
}

func (s *Supervisor) Restart(ctx context.Context) traymodel.OperationResult {
	operation, conflict := s.beginOperation(operationRestart, ctx)
	if conflict != nil {
		return *conflict
	}
	result := invoke[traymodel.OperationResult](s, func() any {
		stopped := s.stopLocked(operation.ctx)
		if !stopped.OK {
			return stopped
		}
		return s.startLocked(operation.ctx, nil)
	})
	s.completeOperation(operation, operationCompletion{result: result})
	return result
}

func (s *Supervisor) ForceStopTracked(ctx context.Context) traymodel.OperationResult {
	if completion, handled := s.cancelActiveStart(); handled {
		return completion.stopResult
	}
	operation, conflict := s.beginOperation(operationForce, ctx)
	if conflict != nil {
		return *conflict
	}
	result := invoke[traymodel.OperationResult](s, func() any { return s.forceStopTrackedLocked(operation.ctx) })
	s.completeOperation(operation, operationCompletion{result: result})
	return result
}

func (s *Supervisor) Refresh(ctx context.Context) traymodel.ComponentState {
	return invoke[traymodel.ComponentState](s, func() any { return s.refreshLocked(ctx) })
}

func (s *Supervisor) Adopt(ctx context.Context, candidate process.Identity) traymodel.ComponentState {
	return invoke[traymodel.ComponentState](s, func() any { return s.adoptLocked(ctx, candidate) })
}

// UpdateExpectedSHA256 serially changes the fingerprint used by future
// Start/Adopt operations. An already claimed process retains the fingerprint
// that completed its handshake, so it can still be inspected and shut down
// without being represented as running the newly saved configuration.
func (s *Supervisor) UpdateExpectedSHA256(value string) traymodel.OperationResult {
	return invoke[traymodel.OperationResult](s, func() any {
		if !sha256Pattern.MatchString(value) {
			return failure("invalid_config", errors.New("expected config fingerprint must be lower-case SHA-256"))
		}
		s.spec.ExpectedSHA256 = value
		s.setStateLocked(s.state)
		return success()
	})
}

func (s *Supervisor) Subscribe(buffer int) (<-chan traymodel.ComponentState, func()) {
	if buffer < 1 {
		buffer = 1
	}
	type subscription struct {
		id int
		ch chan traymodel.ComponentState
	}
	created := invoke[subscription](s, func() any {
		s.nextSubID++
		created := subscription{id: s.nextSubID, ch: make(chan traymodel.ComponentState, buffer)}
		s.subs[created.id] = created.ch
		created.ch <- s.state
		return created
	})
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			invoke[struct{}](s, func() any {
				if ch, ok := s.subs[created.id]; ok {
					delete(s.subs, created.id)
					close(ch)
				}
				return struct{}{}
			})
		})
	}
	return created.ch, cancel
}

func (s *Supervisor) beginOperation(kind operationKind, ctx context.Context) (*activeOperation, *traymodel.OperationResult) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	if s.active != nil {
		result := failure("operation_conflict", fmt.Errorf("component %s operation conflicts with active %s operation", kind, s.active.kind))
		return nil, &result
	}
	opCtx, cancel := context.WithCancel(ctx)
	operation := &activeOperation{
		kind:   kind,
		ctx:    opCtx,
		cancel: cancel,
		done:   make(chan operationCompletion, 1),
	}
	s.active = operation
	return operation, nil
}

func (s *Supervisor) completeOperation(operation *activeOperation, completion operationCompletion) {
	if operation == nil {
		return
	}
	operation.cancel()
	s.operationMu.Lock()
	if s.active == operation {
		s.active = nil
	}
	s.operationMu.Unlock()
	operation.done <- completion
	close(operation.done)
}

func (s *Supervisor) cancelActiveStart() (operationCompletion, bool) {
	s.operationMu.Lock()
	operation := s.active
	if operation == nil || operation.kind != operationStart {
		s.operationMu.Unlock()
		return operationCompletion{}, false
	}
	operation.stopRequested = true
	operation.cancel()
	done := operation.done
	s.operationMu.Unlock()
	completion := <-done
	if completion.stopResult.ErrorCode == "" && !completion.stopResult.OK {
		completion.stopResult = success()
	}
	return completion, true
}

func (s *Supervisor) startStopRequested(operation *activeOperation) bool {
	if operation == nil {
		return false
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	return operation.stopRequested
}

func (s *Supervisor) setStartStopResult(operation *activeOperation, result traymodel.OperationResult) {
	if operation == nil {
		return
	}
	s.operationMu.Lock()
	operation.stopResult = result
	s.operationMu.Unlock()
}

func (s *Supervisor) startStopResult(operation *activeOperation) traymodel.OperationResult {
	if operation == nil {
		return traymodel.OperationResult{}
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	return operation.stopResult
}

func invoke[T any](s *Supervisor, run func() any) T {
	reply := make(chan any, 1)
	s.commands <- command{run: run, reply: reply}
	return (<-reply).(T)
}

func (s *Supervisor) loop() {
	for {
		select {
		case cmd := <-s.commands:
			cmd.reply <- cmd.run()
		case exited := <-s.exits:
			s.handleExitLocked(exited)
		}
	}
}

func (s *Supervisor) startLocked(ctx context.Context, operation *activeOperation) traymodel.OperationResult {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.validate(); err != nil {
		return s.failLocked("invalid_config", err, false)
	}
	if s.state.Lifecycle == traymodel.Starting || s.state.Lifecycle == traymodel.Running || s.state.Lifecycle == traymodel.Stopping {
		return failure("already_running", errors.New("component is already active"))
	}
	if s.launched.PID > 0 {
		if actual, err := s.inspector.Inspect(s.launched.PID); err == nil && process.SameProcess(s.launched, actual) {
			return failure("already_running", errors.New("an existing component process still requires attention"))
		}
		s.clearProcessLocked()
	}
	opCtx, cancel := context.WithTimeout(ctx, s.spec.ReadyTimeout)
	defer cancel()

	identity, err := s.launch(opCtx)
	if err != nil {
		if s.startStopRequested(operation) {
			s.clearProcessLocked()
			s.setStateLocked(traymodel.ComponentState{Lifecycle: traymodel.Stopped})
			s.setStartStopResult(operation, success())
			return failure("start_cancelled", errors.New("component start was cancelled"))
		}
		if process.IsUACCancelled(err) {
			return traymodel.OperationResult{UACCancelled: true, ErrorSummary: nodectl.SanitizeSummary(err.Error())}
		}
		if errors.Is(opCtx.Err(), context.DeadlineExceeded) {
			return s.failLocked("ready_timeout", opCtx.Err(), true)
		}
		return s.failLocked("start_failed", err, false)
	}
	if !identityMatchesSpec(identity, s.spec.ExecutablePath) {
		s.launched = identity
		return s.failLocked("unclaimed_instance", errors.New("launched process identity does not match the configured executable"), true)
	}
	s.launched = identity
	s.claimed = process.Identity{}
	s.setStateLocked(traymodel.ComponentState{
		Lifecycle:       traymodel.Starting,
		PID:             identity.PID,
		StartedAtUnixMS: identity.StartedAtUnixMS,
	})
	s.startWaitLocked(identity)
	if s.startStopRequested(operation) {
		return s.cancelStartLocked(operation, identity)
	}

	deadline, _ := opCtx.Deadline()
	backoffs := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, time.Second}
	probe := 0
	for {
		status, statusErr, exited := s.statusDuringStartLocked(opCtx, identity)
		if s.startStopRequested(operation) {
			return s.cancelStartLocked(operation, identity)
		}
		if exited != nil {
			return s.recordUnexpectedExitLocked(*exited)
		}
		if statusErr == nil {
			if claimErr := statusClaimError(s.spec, identity, status); claimErr != nil {
				return s.failLocked("unclaimed_instance", claimErr, true)
			}
			s.claimed = identity
			s.claimedSHA = s.spec.ExpectedSHA256
			if statusIsReady(s.spec, status) {
				if exited := s.takeMatchingExitLocked(identity); exited != nil {
					return s.recordUnexpectedExitLocked(*exited)
				}
				state := stateFromStatus(s.spec, identity, status, traymodel.Running)
				s.setStateLocked(state)
				return success()
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			if exited := s.takeMatchingExitLocked(identity); exited != nil {
				return s.recordUnexpectedExitLocked(*exited)
			}
			return s.failLocked("ready_timeout", errors.New("component did not become ready before the configured timeout"), true)
		}
		delay := backoffs[probe]
		if probe < len(backoffs)-1 {
			probe++
		}
		if delay > remaining {
			delay = remaining
		}
		exited, waitErr := s.waitStartBackoffLocked(opCtx, identity, delay)
		if exited != nil {
			return s.recordUnexpectedExitLocked(*exited)
		}
		if waitErr != nil {
			if s.startStopRequested(operation) {
				return s.cancelStartLocked(operation, identity)
			}
			if errors.Is(opCtx.Err(), context.DeadlineExceeded) {
				return s.failLocked("ready_timeout", opCtx.Err(), true)
			}
			return s.failLocked("start_failed", opCtx.Err(), true)
		}
	}
}

func (s *Supervisor) cancelStartLocked(operation *activeOperation, identity process.Identity) traymodel.OperationResult {
	startResult := failure("start_cancelled", errors.New("component start was cancelled"))
	if identity.PID == 0 {
		s.clearProcessLocked()
		s.setStateLocked(traymodel.ComponentState{Lifecycle: traymodel.Stopped})
		s.setStartStopResult(operation, success())
		return startResult
	}
	state := s.state
	state.Lifecycle = traymodel.Stopping
	state.Ready = false
	state.Healthy = false
	s.setStateLocked(state)
	if err := s.terminator.Terminate(identity, 1); err != nil {
		stopResult := s.failLocked("shutdown_failed", err, true)
		s.setStartStopResult(operation, stopResult)
		return startResult
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), s.spec.StopTimeout)
	defer cancel()
	for {
		select {
		case <-waitCtx.Done():
			stopResult := s.failLocked("stop_timeout", waitCtx.Err(), true)
			s.setStartStopResult(operation, stopResult)
			return startResult
		case exited := <-s.exits:
			if !process.SameProcess(identity, exited.identity) {
				s.handleExitLocked(exited)
				continue
			}
			s.clearProcessLocked()
			s.setStateLocked(traymodel.ComponentState{Lifecycle: traymodel.Stopped})
			s.setStartStopResult(operation, success())
			return startResult
		}
	}
}

func (s *Supervisor) statusDuringStartLocked(ctx context.Context, identity process.Identity) (nodectl.Status, error, *exitEvent) {
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan statusResult, 1)
	go func() {
		status, err := s.controller.Status(probeCtx)
		result <- statusResult{status: status, err: err}
	}()
	for {
		select {
		case response := <-result:
			if exited := s.takeMatchingExitLocked(identity); exited != nil {
				return nodectl.Status{}, nil, exited
			}
			return response.status, response.err, nil
		case <-ctx.Done():
			if exited := s.takeMatchingExitLocked(identity); exited != nil {
				return nodectl.Status{}, nil, exited
			}
			return nodectl.Status{}, ctx.Err(), nil
		case exited := <-s.exits:
			if process.SameProcess(identity, exited.identity) {
				return nodectl.Status{}, nil, &exited
			}
			s.handleExitLocked(exited)
		}
	}
}

func (s *Supervisor) waitStartBackoffLocked(ctx context.Context, identity process.Identity, delay time.Duration) (*exitEvent, error) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			return s.takeMatchingExitLocked(identity), nil
		case <-ctx.Done():
			if exited := s.takeMatchingExitLocked(identity); exited != nil {
				return exited, nil
			}
			return nil, ctx.Err()
		case exited := <-s.exits:
			if process.SameProcess(identity, exited.identity) {
				return &exited, nil
			}
			s.handleExitLocked(exited)
		}
	}
}

func (s *Supervisor) takeMatchingExitLocked(identity process.Identity) *exitEvent {
	for {
		select {
		case exited := <-s.exits:
			if process.SameProcess(identity, exited.identity) {
				return &exited
			}
			s.handleExitLocked(exited)
		default:
			return nil
		}
	}
}

func (s *Supervisor) launch(ctx context.Context) (process.Identity, error) {
	if s.spec.Component == nodectl.ComponentHelper {
		if elevated, ok := s.launcher.(ElevatedHelperLauncher); ok {
			return elevated.StartHelper(ctx, s.spec.ExecutablePath, s.spec.ConfigPath)
		}
	}
	return s.launcher.Start(ctx, s.spec.ExecutablePath, []string{"--config", s.spec.ConfigPath}, nil)
}

func (s *Supervisor) stopLocked(ctx context.Context) traymodel.OperationResult {
	if ctx == nil {
		ctx = context.Background()
	}
	if s.state.Lifecycle == traymodel.Stopped && s.claimed.PID == 0 {
		return success()
	}
	if s.claimed.PID == 0 {
		return s.failLocked("unclaimed_instance", errors.New("component has not completed a trusted control handshake"), true)
	}
	opCtx, cancel := context.WithTimeout(ctx, s.spec.StopTimeout)
	defer cancel()
	actual, err := s.inspector.Inspect(s.claimed.PID)
	if err != nil || !process.SameProcess(s.claimed, actual) {
		return s.failLocked("unclaimed_instance", errors.New("claimed process identity is no longer valid"), true)
	}
	status, err := s.controller.Status(opCtx)
	if errors.Is(opCtx.Err(), context.DeadlineExceeded) {
		return s.failLocked("stop_timeout", opCtx.Err(), true)
	}
	claimSpec := s.claimedSpecLocked()
	if err != nil {
		return s.failLocked("unclaimed_instance", errors.New("control handshake no longer identifies the claimed process"), true)
	}
	if claimErr := statusClaimError(claimSpec, s.claimed, status); claimErr != nil {
		return s.failLocked("unclaimed_instance", claimErr, true)
	}
	stopping := stateFromStatus(claimSpec, s.claimed, status, traymodel.Stopping)
	stopping.Ready = false
	stopping.Healthy = false
	s.setStateLocked(stopping)
	if err := s.controller.Shutdown(opCtx); err != nil {
		if errors.Is(opCtx.Err(), context.DeadlineExceeded) {
			return s.failLocked("stop_timeout", opCtx.Err(), true)
		}
		return s.failLocked("shutdown_failed", err, true)
	}

	for {
		select {
		case <-opCtx.Done():
			return s.failLocked("stop_timeout", opCtx.Err(), true)
		case exited := <-s.exits:
			if !process.SameProcess(s.claimed, exited.identity) {
				continue
			}
			s.clearProcessLocked()
			s.setStateLocked(traymodel.ComponentState{Lifecycle: traymodel.Stopped})
			return success()
		}
	}
}

func (s *Supervisor) forceStopTrackedLocked(ctx context.Context) traymodel.OperationResult {
	identity := s.launched
	if identity.PID == 0 {
		identity = s.claimed
	}
	if identity.PID == 0 {
		s.clearProcessLocked()
		s.setStateLocked(traymodel.ComponentState{Lifecycle: traymodel.Stopped})
		return success()
	}
	state := s.state
	state.Lifecycle = traymodel.Stopping
	state.Ready = false
	state.Healthy = false
	s.setStateLocked(state)
	if err := s.terminator.Terminate(identity, 1); err != nil {
		return s.failLocked("force_exit_failed", err, true)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	waitCtx, cancel := context.WithTimeout(ctx, s.spec.StopTimeout)
	defer cancel()
	for {
		select {
		case <-waitCtx.Done():
			return s.failLocked("force_exit_timeout", waitCtx.Err(), true)
		case exited := <-s.exits:
			if !process.SameProcess(identity, exited.identity) {
				s.handleExitLocked(exited)
				continue
			}
			s.clearProcessLocked()
			s.setStateLocked(traymodel.ComponentState{Lifecycle: traymodel.Stopped})
			return success()
		}
	}
}

func (s *Supervisor) refreshLocked(ctx context.Context) traymodel.ComponentState {
	if s.claimed.PID == 0 {
		return s.state
	}
	actual, err := s.inspector.Inspect(s.claimed.PID)
	if err != nil || !process.SameProcess(s.claimed, actual) {
		s.failLocked("unclaimed_instance", errors.New("claimed process identity is no longer valid"), true)
		return s.state
	}
	status, err := s.controller.Status(ctx)
	claimSpec := s.claimedSpecLocked()
	if err != nil {
		s.failLocked("unclaimed_instance", errors.New("control handshake no longer identifies the claimed process"), true)
		return s.state
	}
	if claimErr := statusClaimError(claimSpec, s.claimed, status); claimErr != nil {
		s.failLocked("unclaimed_instance", claimErr, true)
		return s.state
	}
	if s.state.Lifecycle == traymodel.Failed && !statusIsReady(claimSpec, status) {
		return s.state
	}
	lifecycle := traymodel.Starting
	if statusIsReady(claimSpec, status) {
		lifecycle = traymodel.Running
	}
	s.setStateLocked(stateFromStatus(claimSpec, s.claimed, status, lifecycle))
	return s.state
}

func (s *Supervisor) adoptLocked(ctx context.Context, candidate process.Identity) traymodel.ComponentState {
	if err := s.validate(); err != nil {
		s.failLocked("invalid_config", err, false)
		return s.state
	}
	actual, err := s.inspector.Inspect(candidate.PID)
	if err != nil || !process.SameProcess(candidate, actual) || !identityMatchesSpec(candidate, s.spec.ExecutablePath) {
		s.failLocked("unclaimed_instance", errors.New("candidate process identity could not be verified"), true)
		return s.state
	}
	status, err := s.controller.Status(ctx)
	if err != nil {
		s.failLocked("unclaimed_instance", errors.New("candidate control handshake does not match"), true)
		return s.state
	}
	if claimErr := statusClaimError(s.spec, candidate, status); claimErr != nil {
		s.failLocked("unclaimed_instance", claimErr, true)
		return s.state
	}
	s.clearProcessLocked()
	s.launched = candidate
	s.claimed = candidate
	s.claimedSHA = s.spec.ExpectedSHA256
	lifecycle := traymodel.Starting
	if statusIsReady(s.spec, status) {
		lifecycle = traymodel.Running
	}
	s.setStateLocked(stateFromStatus(s.spec, candidate, status, lifecycle))
	s.startWaitLocked(candidate)
	return s.state
}

func (s *Supervisor) startWaitLocked(identity process.Identity) {
	if s.waitCancel != nil {
		s.waitCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.waitCancel = cancel
	go func() {
		code, err := s.inspector.Wait(ctx, identity)
		if errors.Is(err, context.Canceled) {
			return
		}
		s.exits <- exitEvent{identity: identity, code: code, err: err}
	}()
}

func (s *Supervisor) handleExitLocked(exited exitEvent) {
	if s.launched.PID == 0 || !process.SameProcess(s.launched, exited.identity) {
		return
	}
	wasActive := s.state.Lifecycle == traymodel.Starting || s.state.Lifecycle == traymodel.Running
	if wasActive {
		s.recordUnexpectedExitLocked(exited)
		return
	}
	s.clearProcessLocked()
}

func (s *Supervisor) recordUnexpectedExitLocked(exited exitEvent) traymodel.OperationResult {
	s.clearProcessLocked()
	return s.failLocked("unexpected_exit", fmt.Errorf("component exited unexpectedly with code %d", exited.code), true)
}

func (s *Supervisor) clearProcessLocked() {
	if s.waitCancel != nil {
		s.waitCancel()
		s.waitCancel = nil
	}
	s.launched = process.Identity{}
	s.claimed = process.Identity{}
	s.claimedSHA = ""
}

func (s *Supervisor) claimedSpecLocked() Spec {
	spec := s.spec
	if s.claimedSHA != "" {
		spec.ExpectedSHA256 = s.claimedSHA
	}
	return spec
}

func (s *Supervisor) setStateLocked(state traymodel.ComponentState) {
	state.RuntimeConfigSHA256 = s.claimedSHA
	state.SavedConfigSHA256 = s.spec.ExpectedSHA256
	state.NeedsRestart = state.Lifecycle == traymodel.Running &&
		s.claimedSHA != "" && s.spec.ExpectedSHA256 != "" &&
		!strings.EqualFold(s.claimedSHA, s.spec.ExpectedSHA256)
	if state == s.state {
		return
	}
	s.state = state
	for _, ch := range s.subs {
		select {
		case ch <- state:
		default:
		}
	}
}

func (s *Supervisor) failLocked(code string, err error, attention bool) traymodel.OperationResult {
	result := failure(code, err)
	state := s.state
	state.Lifecycle = traymodel.Failed
	state.Healthy = false
	state.Ready = false
	state.ErrorCode = result.ErrorCode
	state.ErrorSummary = result.ErrorSummary
	state.NeedsAttention = attention
	s.setStateLocked(state)
	return result
}

func success() traymodel.OperationResult { return traymodel.OperationResult{OK: true} }

func failure(code string, err error) traymodel.OperationResult {
	summary := "operation failed"
	if err != nil {
		summary = err.Error()
	}
	return traymodel.OperationResult{ErrorCode: code, ErrorSummary: nodectl.SanitizeSummary(summary)}
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (s *Supervisor) validate() error {
	if s.spec.Component != nodectl.ComponentAgent && s.spec.Component != nodectl.ComponentHelper {
		return errors.New("unsupported component")
	}
	if s.launcher == nil || s.inspector == nil || s.controller == nil || s.terminator == nil {
		return errors.New("supervisor dependencies are incomplete")
	}
	if !filepath.IsAbs(s.spec.ExecutablePath) || !filepath.IsAbs(s.spec.ConfigPath) {
		return errors.New("component executable and config paths must be absolute")
	}
	if !sha256Pattern.MatchString(s.spec.ExpectedSHA256) {
		return errors.New("expected config fingerprint must be lower-case SHA-256")
	}
	if s.spec.ReadyTimeout <= 0 || s.spec.StopTimeout <= 0 {
		return errors.New("ready and stop timeouts must be positive")
	}
	return nil
}

func identityMatchesSpec(identity process.Identity, executable string) bool {
	expected := identity
	expected.ExecutablePath = executable
	return process.SameProcess(expected, identity)
}
