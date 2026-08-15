package localtask

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	"dedup/internal/proto"
	"dedup/internal/store"
)

type CreateRequest = proto.LocalTaskCreateRequest
type Task = proto.LocalTask
type ListRequest = proto.LocalTaskListRequest

type Page[T any] struct {
	Items      []T
	Offset     int
	NextOffset int
}

type TaskRunner interface {
	Run(RunControl, CreateRequest, Task, func(ProgressUpdate) error) error
}

type TaskStore interface {
	CreateOrLoadLocalTask(context.Context, store.LocalTaskCreate) (store.LocalTask, error)
	LoadLocalTask(context.Context, string, string) (store.LocalTask, error)
	ListLocalTasks(context.Context, string, int, int) ([]store.LocalTask, error)
	RecoverLocalTasks(context.Context, string) ([]store.LocalTask, error)
	TransitionLocalTaskLifecycle(context.Context, string, store.LocalTaskControl, string, *string, *string) (store.LocalTask, error)
	UpdateLocalTaskProgress(context.Context, string, store.LocalTaskControl, store.LocalTaskProgressUpdate) (store.LocalTask, error)
	HasLocalTaskDeletionReceipt(context.Context, string, string) (bool, error)
	LoadLocalTaskDeletionReceipt(context.Context, string, string, string) (store.LocalTaskDeleteResult, error)
	DeleteLocalTaskData(context.Context, string, store.LocalTaskControl) (store.LocalTaskDeleteResult, error)
}

type Service interface {
	Create(context.Context, CreateRequest) (Task, error)
	List(context.Context, ListRequest) (Page[Task], error)
	Pause(context.Context, ControlRequest) (Task, error)
	ResumeTask(context.Context, ControlRequest) (Task, error)
	Cancel(context.Context, ControlRequest) (Task, error)
	Delete(context.Context, ControlRequest) (ControlResult, error)
	Retry(context.Context, ControlRequest) (Task, error)
	LegacyCancel(context.Context, string) (Task, error)
	LegacyRetry(context.Context, string) (Task, error)
}

type RecoverableService interface {
	Service
	PrepareRecovery(context.Context) error
	ResumeRecoveredTasks(context.Context) error
	Shutdown(context.Context) error
}

var errServiceClosing = errors.New("localtask: service is closing")

type taskService struct {
	machineID string
	store     TaskStore
	runner    TaskRunner
	options   serviceOptions

	mu       sync.Mutex
	active   map[string]*taskAttempt
	gates    map[string]*taskGate
	recovery []store.LocalTask
	prepared bool
	closing  bool
}

func NewService(machineID string, taskStore TaskStore, runner TaskRunner, serviceOptions ...ServiceOption) RecoverableService {
	options := defaultServiceOptions()
	for _, apply := range serviceOptions {
		if apply != nil {
			apply(&options)
		}
	}
	return &taskService{
		machineID: machineID,
		store:     taskStore,
		runner:    runner,
		options:   options,
		active:    make(map[string]*taskAttempt),
		gates:     make(map[string]*taskGate),
	}
}

func EncodeCreateEnvelope(request CreateRequest) ([]byte, string, error) {
	if err := request.Validate(); err != nil {
		return nil, "", err
	}
	envelope, err := msgpack.Marshal(request)
	if err != nil {
		return nil, "", fmt.Errorf("localtask: encode create envelope: %w", err)
	}
	digest := sha256.Sum256(envelope)
	return envelope, hex.EncodeToString(digest[:]), nil
}

func decodeCreateEnvelope(envelope []byte) (CreateRequest, error) {
	var request CreateRequest
	if len(envelope) == 0 {
		return request, fmt.Errorf("localtask: missing recoverable envelope")
	}
	if err := msgpack.Unmarshal(envelope, &request); err != nil {
		return request, fmt.Errorf("localtask: decode recoverable envelope")
	}
	if err := request.Validate(); err != nil {
		return request, fmt.Errorf("localtask: invalid recoverable envelope")
	}
	return request, nil
}

func (s *taskService) Create(ctx context.Context, request CreateRequest) (Task, error) {
	if err := s.ready(); err != nil {
		return Task{}, err
	}
	envelope, digest, err := EncodeCreateEnvelope(request)
	if err != nil {
		return Task{}, err
	}
	gate, err := s.acquireTaskGate(ctx, request.TaskID)
	if err != nil {
		return Task{}, err
	}
	defer gate.release()
	if s.isClosing() {
		return Task{}, errServiceClosing
	}
	taskType := "scan"
	if request.Mode == proto.LocalTaskModeScanThenAnalysis {
		taskType = "analysis"
	}
	persisted, err := s.store.CreateOrLoadLocalTask(ctx, store.LocalTaskCreate{
		TaskID: request.TaskID, MachineID: s.machineID, Source: "local", Type: taskType,
		Stage: 0, EnvelopeDigest: digest, Envelope: envelope,
	})
	if err != nil {
		return Task{}, err
	}
	canonical, err := decodeCreateEnvelope(persisted.Envelope)
	if err != nil {
		return Task{}, err
	}
	if persisted.Status == "pending" {
		s.launchHeld(persisted, canonical)
	}
	return taskFromStore(persisted, canonical), nil
}

func (s *taskService) List(ctx context.Context, request ListRequest) (Page[Task], error) {
	if err := s.ready(); err != nil {
		return Page[Task]{}, err
	}
	if request.Offset < 0 {
		return Page[Task]{}, fmt.Errorf("localtask: invalid offset")
	}
	rows, err := s.store.ListLocalTasks(ctx, s.machineID, request.Offset, request.Limit)
	if err != nil {
		return Page[Task]{}, err
	}
	items := make([]Task, 0, len(rows))
	for _, row := range rows {
		create, _ := decodeCreateEnvelope(row.Envelope)
		items = append(items, taskFromStore(row, create))
	}
	next := request.Offset + len(items)
	return Page[Task]{Items: items, Offset: request.Offset, NextOffset: next}, nil
}

func (s *taskService) Pause(ctx context.Context, request ControlRequest) (Task, error) {
	if err := request.Validate(); err != nil {
		return Task{}, err
	}
	gate, err := s.acquireTaskGate(ctx, request.TaskID)
	if err != nil {
		return Task{}, err
	}
	defer gate.release()
	current, err := s.store.LoadLocalTask(ctx, s.machineID, request.TaskID)
	if err != nil {
		return Task{}, err
	}
	return s.pauseHeld(ctx, current, request)
}

func (s *taskService) pauseHeld(ctx context.Context, current store.LocalTask, request ControlRequest) (Task, error) {
	if err := validateControlVersion(current, request); err != nil {
		return Task{}, err
	}
	create, _ := decodeCreateEnvelope(current.Envelope)
	switch current.Status {
	case "pausing", "paused", "stopping", "cancelled", "deleting", "delete_failed":
		return taskFromStore(current, create), nil
	}
	attempt := s.currentAttempt(current.TaskID)
	if attempt == nil && s.isClosing() {
		return Task{}, errServiceClosing
	}
	if err := s.flushProgressHeld(attempt); err != nil && !errors.Is(err, store.ErrLocalTaskStale) {
		return Task{}, err
	}
	updated, err := s.store.TransitionLocalTaskLifecycle(ctx, s.machineID, controlFromRequest(request), "pausing", nil, nil)
	if err != nil {
		return Task{}, err
	}
	s.signalOrSettleHeld(updated, DrainPause, attempt)
	return taskFromStore(updated, create), nil
}

func (s *taskService) Cancel(ctx context.Context, request ControlRequest) (Task, error) {
	if err := request.Validate(); err != nil {
		return Task{}, err
	}
	gate, err := s.acquireTaskGate(ctx, request.TaskID)
	if err != nil {
		return Task{}, err
	}
	defer gate.release()
	current, err := s.store.LoadLocalTask(ctx, s.machineID, request.TaskID)
	if err != nil {
		return Task{}, err
	}
	return s.cancelHeld(ctx, current, request)
}

func (s *taskService) cancelHeld(ctx context.Context, current store.LocalTask, request ControlRequest) (Task, error) {
	if err := validateControlVersion(current, request); err != nil {
		return Task{}, err
	}
	create, _ := decodeCreateEnvelope(current.Envelope)
	switch current.Status {
	case "stopping", "cancelled", "deleting", "delete_failed":
		return taskFromStore(current, create), nil
	}
	attempt := s.currentAttempt(current.TaskID)
	if attempt == nil && s.isClosing() {
		return Task{}, errServiceClosing
	}
	if err := s.flushProgressHeld(attempt); err != nil && !errors.Is(err, store.ErrLocalTaskStale) {
		return Task{}, err
	}
	target := "stopping"
	if current.Status == "paused" {
		target = "cancelled"
	}
	updated, err := s.store.TransitionLocalTaskLifecycle(ctx, s.machineID, controlFromRequest(request), target, nil, nil)
	if err != nil {
		return Task{}, err
	}
	if target == "stopping" {
		s.signalOrSettleHeld(updated, DrainStop, attempt)
	}
	return taskFromStore(updated, create), nil
}

func (s *taskService) Delete(ctx context.Context, request ControlRequest) (ControlResult, error) {
	if err := request.Validate(); err != nil {
		return ControlResult{}, err
	}
	gate, err := s.acquireTaskGate(ctx, request.TaskID)
	if err != nil {
		return ControlResult{}, err
	}
	defer gate.release()
	receipt, err := s.store.LoadLocalTaskDeletionReceipt(ctx, s.machineID, request.TaskID, request.InstanceID)
	if err == nil {
		return ControlResult{Deleted: receipt.Deleted || receipt.AlreadyDeleted}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ControlResult{}, err
	}
	current, err := s.store.LoadLocalTask(ctx, s.machineID, request.TaskID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			receipt, receiptErr := s.store.LoadLocalTaskDeletionReceipt(ctx, s.machineID, request.TaskID, request.InstanceID)
			if receiptErr == nil {
				return ControlResult{Deleted: receipt.Deleted || receipt.AlreadyDeleted}, nil
			}
		}
		return ControlResult{}, err
	}
	if err := validateControlVersion(current, request); err != nil {
		return ControlResult{}, err
	}
	create, _ := decodeCreateEnvelope(current.Envelope)
	if current.Status == "deleting" {
		task := taskFromStore(current, create)
		return ControlResult{Task: &task}, nil
	}
	attempt := s.currentAttempt(current.TaskID)
	if attempt == nil && s.isClosing() {
		return ControlResult{}, errServiceClosing
	}
	if err := s.flushProgressHeld(attempt); err != nil && !errors.Is(err, store.ErrLocalTaskStale) {
		return ControlResult{}, err
	}
	updated, err := s.store.TransitionLocalTaskLifecycle(ctx, s.machineID, controlFromRequest(request), "deleting", nil, nil)
	if err != nil {
		return ControlResult{}, err
	}
	s.signalOrSettleHeld(updated, DrainDelete, attempt)
	task := taskFromStore(updated, create)
	return ControlResult{Task: &task}, nil
}

func (s *taskService) ResumeTask(ctx context.Context, request ControlRequest) (Task, error) {
	if err := request.Validate(); err != nil {
		return Task{}, err
	}
	gate, err := s.acquireTaskGate(ctx, request.TaskID)
	if err != nil {
		return Task{}, err
	}
	defer gate.release()
	if s.isClosing() {
		return Task{}, errServiceClosing
	}
	current, err := s.store.LoadLocalTask(ctx, s.machineID, request.TaskID)
	if err != nil {
		return Task{}, err
	}
	if err := validateControlVersion(current, request); err != nil {
		return Task{}, err
	}
	if current.Status != "paused" {
		return Task{}, fmt.Errorf("%w: %s to pending", store.ErrLocalTaskTransition, current.Status)
	}
	create, err := decodeCreateEnvelope(current.Envelope)
	if err != nil {
		return Task{}, err
	}
	updated, err := s.store.TransitionLocalTaskLifecycle(ctx, s.machineID, controlFromRequest(request), "pending", nil, nil)
	if err != nil {
		return Task{}, err
	}
	s.launchHeld(updated, create)
	return taskFromStore(updated, create), nil
}

func (s *taskService) Retry(ctx context.Context, request ControlRequest) (Task, error) {
	if err := request.Validate(); err != nil {
		return Task{}, err
	}
	gate, err := s.acquireTaskGate(ctx, request.TaskID)
	if err != nil {
		return Task{}, err
	}
	defer gate.release()
	if s.isClosing() {
		return Task{}, errServiceClosing
	}
	current, err := s.store.LoadLocalTask(ctx, s.machineID, request.TaskID)
	if err != nil {
		return Task{}, err
	}
	return s.retryHeld(ctx, current, request)
}

func (s *taskService) retryHeld(ctx context.Context, current store.LocalTask, request ControlRequest) (Task, error) {
	if err := validateControlVersion(current, request); err != nil {
		return Task{}, err
	}
	if current.Status != "failed" && current.Status != "cancelled" {
		return Task{}, fmt.Errorf("%w: %s to pending", store.ErrLocalTaskTransition, current.Status)
	}
	create, err := decodeCreateEnvelope(current.Envelope)
	if err != nil {
		return Task{}, err
	}
	updated, err := s.store.TransitionLocalTaskLifecycle(ctx, s.machineID, controlFromRequest(request), "pending", nil, nil)
	if err != nil {
		return Task{}, err
	}
	s.launchHeld(updated, create)
	return taskFromStore(updated, create), nil
}

func (s *taskService) LegacyCancel(ctx context.Context, taskID string) (Task, error) {
	if err := (proto.LocalTaskIDRequest{TaskID: taskID}).Validate(); err != nil {
		return Task{}, err
	}
	gate, err := s.acquireTaskGate(ctx, taskID)
	if err != nil {
		return Task{}, err
	}
	defer gate.release()
	deleted, err := s.store.HasLocalTaskDeletionReceipt(ctx, s.machineID, taskID)
	if err != nil {
		return Task{}, err
	}
	if deleted {
		return Task{}, fmt.Errorf("%w: task %s has deletion receipt", store.ErrLocalTaskTransition, taskID)
	}
	current, err := s.store.LoadLocalTask(ctx, s.machineID, taskID)
	if err != nil {
		return Task{}, err
	}
	return s.cancelHeld(ctx, current, requestFromStore(current))
}

func (s *taskService) LegacyRetry(ctx context.Context, taskID string) (Task, error) {
	if err := (proto.LocalTaskIDRequest{TaskID: taskID}).Validate(); err != nil {
		return Task{}, err
	}
	gate, err := s.acquireTaskGate(ctx, taskID)
	if err != nil {
		return Task{}, err
	}
	defer gate.release()
	if s.isClosing() {
		return Task{}, errServiceClosing
	}
	deleted, err := s.store.HasLocalTaskDeletionReceipt(ctx, s.machineID, taskID)
	if err != nil {
		return Task{}, err
	}
	if deleted {
		return Task{}, fmt.Errorf("%w: task %s has deletion receipt", store.ErrLocalTaskTransition, taskID)
	}
	current, err := s.store.LoadLocalTask(ctx, s.machineID, taskID)
	if err != nil {
		return Task{}, err
	}
	return s.retryHeld(ctx, current, requestFromStore(current))
}

func (s *taskService) PrepareRecovery(ctx context.Context) error {
	if err := s.ready(); err != nil {
		return err
	}
	if s.isClosing() {
		return errServiceClosing
	}
	s.mu.Lock()
	if s.prepared {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	recovered, err := s.store.RecoverLocalTasks(ctx, s.machineID)
	if err != nil {
		return err
	}
	recovery := make([]store.LocalTask, 0, len(recovered))
	seen := make(map[string]struct{}, len(recovered))
	for _, task := range recovered {
		recovery = append(recovery, task)
		seen[task.TaskID] = struct{}{}
	}
	for offset := 0; ; {
		rows, err := s.store.ListLocalTasks(ctx, s.machineID, offset, 200)
		if err != nil {
			return err
		}
		for _, task := range rows {
			if task.Status != "waiting_recovery" && task.Status != "deleting" {
				continue
			}
			if _, exists := seen[task.TaskID]; exists {
				continue
			}
			recovery = append(recovery, task)
			seen[task.TaskID] = struct{}{}
		}
		if len(rows) < 200 {
			break
		}
		offset += len(rows)
	}
	s.mu.Lock()
	if !s.prepared {
		s.recovery = recovery
		s.prepared = true
	}
	s.mu.Unlock()
	return nil
}

func (s *taskService) ResumeRecoveredTasks(ctx context.Context) error {
	if err := s.PrepareRecovery(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	recovered := append([]store.LocalTask(nil), s.recovery...)
	s.recovery = nil
	s.mu.Unlock()
	for _, recoveredTask := range recovered {
		gate, err := s.acquireTaskGate(ctx, recoveredTask.TaskID)
		if err != nil {
			return err
		}
		current, err := s.store.LoadLocalTask(ctx, s.machineID, recoveredTask.TaskID)
		if err != nil {
			gate.release()
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return err
		}
		switch current.Status {
		case "pending", "running", "waiting_recovery":
			create, decodeErr := decodeCreateEnvelope(current.Envelope)
			if decodeErr != nil {
				code := "recovery_envelope_invalid"
				_, err = s.store.TransitionLocalTaskLifecycle(ctx, s.machineID, controlForStoreTask(current), "failed", &code, nil)
			} else {
				s.launchHeld(current, create)
			}
		case "pausing":
			_, err = s.store.TransitionLocalTaskLifecycle(ctx, s.machineID, controlForStoreTask(current), "paused", nil, nil)
		case "stopping":
			_, err = s.store.TransitionLocalTaskLifecycle(ctx, s.machineID, controlForStoreTask(current), "cancelled", nil, nil)
		case "deleting":
			s.signalOrSettleHeld(current, DrainDelete, nil)
		}
		gate.release()
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *taskService) Shutdown(ctx context.Context) error {
	if err := s.ready(); err != nil {
		return err
	}
	s.mu.Lock()
	s.closing = true
	attempts := make([]*taskAttempt, 0, len(s.active))
	for _, attempt := range s.active {
		attempts = append(attempts, attempt)
	}
	s.mu.Unlock()
	for _, attempt := range attempts {
		gate, err := s.acquireTaskGate(ctx, attempt.version().TaskID)
		if err != nil {
			for _, current := range attempts {
				current.hardCancel()
			}
			return err
		}
		if s.currentAttempt(attempt.version().TaskID) == attempt && attempt.drainReason() == "" {
			_ = s.flushProgressHeld(attempt)
			attempt.upgrade(DrainProcessShutdown, attempt.version().ExpectedRevision)
		}
		gate.release()
	}
	allDone := make(chan struct{})
	go func() {
		for _, attempt := range attempts {
			<-attempt.done
		}
		close(allDone)
	}()
	select {
	case <-allDone:
		return nil
	case <-ctx.Done():
		for _, attempt := range attempts {
			attempt.hardCancel()
		}
		return ctx.Err()
	}
}

func (s *taskService) launchHeld(persisted store.LocalTask, request CreateRequest) {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	if _, exists := s.active[persisted.TaskID]; exists {
		s.mu.Unlock()
		return
	}
	attempt, hardContext := newTaskAttempt(persisted)
	s.active[persisted.TaskID] = attempt
	s.mu.Unlock()
	go s.runAttempt(hardContext, persisted, request, attempt)
}

func (s *taskService) runAttempt(hardContext context.Context, persisted store.LocalTask, request CreateRequest, attempt *taskAttempt) {
	gate, err := s.acquireTaskGate(context.Background(), persisted.TaskID)
	if err != nil {
		attempt.finish()
		return
	}
	if s.currentAttempt(persisted.TaskID) != attempt {
		gate.release()
		attempt.finish()
		return
	}
	current, err := s.store.LoadLocalTask(context.Background(), s.machineID, persisted.TaskID)
	if err != nil {
		s.cleanupAttemptHeld(attempt)
		gate.release()
		return
	}
	if attempt.drainReason() != "" {
		gate.release()
		s.completeAttempt(attempt, nil)
		return
	}
	if current.Status == "pending" || current.Status == "waiting_recovery" {
		current, err = s.store.TransitionLocalTaskLifecycle(context.Background(), s.machineID, controlForStoreTask(current), "running", nil, nil)
		if err != nil {
			s.cleanupAttemptHeld(attempt)
			gate.release()
			return
		}
		attempt.setRevision(current.Revision)
	} else if current.Status != "running" {
		s.cleanupAttemptHeld(attempt)
		gate.release()
		return
	}
	runnerTask := taskFromStore(current, request)
	reporter := newProgressReporter(s, attempt, runnerTask)
	attempt.setReporter(reporter)
	gate.release()
	runErr := s.runner.Run(RunControl{
		Context: hardContext,
		Drain:   attempt.drain,
		Reason:  attempt.drainReason,
	}, request, runnerTask, reporter.report)
	_ = reporter.stopAndFlush()
	s.completeAttempt(attempt, runErr)
}

func (s *taskService) signalOrSettleHeld(updated store.LocalTask, reason DrainReason, attempt *taskAttempt) {
	if attempt != nil && s.currentAttempt(updated.TaskID) == attempt {
		attempt.upgrade(reason, updated.Revision)
		return
	}
	settler, _ := newTaskAttempt(updated)
	settler.upgrade(reason, updated.Revision)
	s.mu.Lock()
	if current := s.active[updated.TaskID]; current != nil {
		s.mu.Unlock()
		current.upgrade(reason, updated.Revision)
		return
	}
	s.active[updated.TaskID] = settler
	s.mu.Unlock()
	go s.completeAttempt(settler, nil)
}

func (s *taskService) completeAttempt(attempt *taskAttempt, runErr error) {
	gate, err := s.acquireTaskGate(context.Background(), attempt.version().TaskID)
	if err != nil {
		attempt.finish()
		return
	}
	if s.currentAttempt(attempt.version().TaskID) != attempt {
		gate.release()
		attempt.finish()
		return
	}
	reason := attempt.drainReason()
	if reason == DrainDelete {
		attempt.markDone()
		gate.release()
		<-attempt.done
		s.reconcileDelete(attempt)
		return
	}
	target := "succeeded"
	var code *string
	switch reason {
	case DrainPause:
		target = "paused"
	case DrainStop:
		target = "cancelled"
	case DrainProcessShutdown:
		target = "waiting_recovery"
	default:
		if runErr != nil {
			target = "failed"
			value := "task_failed"
			code = &value
		}
	}
	_, transitionErr := s.store.TransitionLocalTaskLifecycle(context.Background(), s.machineID, attempt.version(), target, code, nil)
	if transitionErr != nil && !errors.Is(transitionErr, store.ErrLocalTaskStale) && !errors.Is(transitionErr, store.ErrLocalTaskInstanceMismatch) {
		s.options.logf("localtask: terminal transition failed for %s: %v", attempt.version().TaskID, transitionErr)
	}
	s.cleanupAttemptHeld(attempt)
	gate.release()
}

func (s *taskService) reconcileDelete(attempt *taskAttempt) {
	delays := [...]time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second}
	var deleteErr error
	for call := 0; call <= len(delays); call++ {
		deleteErr = s.deleteAttempt(attempt)
		if deleteErr == nil {
			return
		}
		if !errors.Is(deleteErr, store.ErrLocalTaskDeleteRetryable) || call == len(delays) {
			break
		}
		select {
		case <-s.options.deleteRetryAfter(delays[call]):
		case <-attempt.hardContext.Done():
			deleteErr = attempt.hardContext.Err()
			call = len(delays)
		}
	}
	code := "delete_failed"
	if errors.Is(deleteErr, store.ErrLocalTaskDeleteRetryable) {
		code = "delete_retry_exhausted"
	}
	s.finishDeleteAttempt(attempt, code)
}

func (s *taskService) deleteAttempt(attempt *taskAttempt) error {
	gate, err := s.acquireTaskGate(attempt.hardContext, attempt.version().TaskID)
	if err != nil {
		return err
	}
	defer gate.release()
	if s.currentAttempt(attempt.version().TaskID) != attempt {
		return store.ErrLocalTaskStale
	}
	_, err = s.store.DeleteLocalTaskData(attempt.hardContext, s.machineID, attempt.version())
	if err == nil {
		s.cleanupAttemptHeld(attempt)
	}
	return err
}

func (s *taskService) finishDeleteAttempt(attempt *taskAttempt, failureCode string) {
	gate, err := s.acquireTaskGate(context.Background(), attempt.version().TaskID)
	if err != nil {
		attempt.finish()
		return
	}
	if s.currentAttempt(attempt.version().TaskID) != attempt {
		gate.release()
		attempt.finish()
		return
	}
	if failureCode != "" {
		code := failureCode
		_, err := s.store.TransitionLocalTaskLifecycle(context.Background(), s.machineID, attempt.version(), "delete_failed", &code, nil)
		if err != nil {
			s.options.logf("localtask: delete failure transition failed for %s: %v", attempt.version().TaskID, err)
		}
	}
	s.cleanupAttemptHeld(attempt)
	gate.release()
}

func (s *taskService) flushProgressHeld(attempt *taskAttempt) error {
	if attempt == nil {
		return nil
	}
	reporter := attempt.progressReporter()
	if reporter == nil {
		return nil
	}
	return reporter.flushHeld()
}

func (s *taskService) cleanupAttemptHeld(attempt *taskAttempt) {
	s.mu.Lock()
	if s.active[attempt.version().TaskID] == attempt {
		delete(s.active, attempt.version().TaskID)
	}
	s.mu.Unlock()
	attempt.finish()
}

func (s *taskService) acquireTaskGate(ctx context.Context, taskID string) (*taskGate, error) {
	s.mu.Lock()
	gate := s.gates[taskID]
	if gate == nil {
		gate = &taskGate{token: make(chan struct{}, 1)}
		gate.token <- struct{}{}
		s.gates[taskID] = gate
	}
	s.mu.Unlock()
	select {
	case <-gate.token:
		return gate, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *taskService) currentAttempt(taskID string) *taskAttempt {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active[taskID]
}

func (s *taskService) isClosing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closing
}

func (s *taskService) ready() error {
	if s == nil || s.machineID == "" || s.store == nil || s.runner == nil {
		return fmt.Errorf("localtask: service dependencies are required")
	}
	return nil
}

func validateControlVersion(task store.LocalTask, request ControlRequest) error {
	if task.InstanceID != request.InstanceID {
		return fmt.Errorf("%w: task %s", store.ErrLocalTaskInstanceMismatch, request.TaskID)
	}
	if task.Revision != request.ExpectedRevision {
		return fmt.Errorf("%w: task %s", store.ErrLocalTaskStale, request.TaskID)
	}
	return nil
}

func controlFromRequest(request ControlRequest) store.LocalTaskControl {
	return store.LocalTaskControl{TaskID: request.TaskID, InstanceID: request.InstanceID, ExpectedRevision: request.ExpectedRevision}
}

func requestFromStore(task store.LocalTask) ControlRequest {
	return ControlRequest{TaskID: task.TaskID, InstanceID: task.InstanceID, ExpectedRevision: task.Revision}
}

func controlForStoreTask(task store.LocalTask) store.LocalTaskControl {
	return store.LocalTaskControl{TaskID: task.TaskID, InstanceID: task.InstanceID, ExpectedRevision: task.Revision}
}

func taskFromStore(task store.LocalTask, request CreateRequest) Task {
	result := Task{
		TaskID: task.TaskID, InstanceID: task.InstanceID, Revision: task.Revision,
		Source: task.Source, Mode: request.Mode, Stage: task.Stage, Phase: task.Phase,
		Status: task.Status, Roots: append([]string(nil), request.Roots...),
		Rescan: request.Rescan, Extensions: append([]string(nil), request.Extensions...),
		ProgressComplete: task.ProgressComplete, ProgressTotal: task.ProgressTotal,
		ProgressTotalKnown: task.ProgressTotalKnown, StatsJSON: task.StatsJSON,
		CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt,
	}
	if task.SafeErrorCode != nil {
		result.SafeErrorCode = *task.SafeErrorCode
	}
	if task.SafeErrorMessage != nil {
		result.SafeErrorMessage = *task.SafeErrorMessage
	}
	if task.StartedAt != nil {
		result.StartedAt = *task.StartedAt
	}
	if task.CompletedAt != nil {
		result.CompletedAt = *task.CompletedAt
	}
	return result
}
