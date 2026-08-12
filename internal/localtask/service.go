package localtask

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

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

type Service interface {
	Create(context.Context, CreateRequest) (Task, error)
	List(context.Context, ListRequest) (Page[Task], error)
	Cancel(context.Context, string) error
	Retry(context.Context, string) (Task, error)
	Resume(context.Context) error
}

type RecoverableService interface {
	Service
	PrepareRecovery(context.Context) error
}

type TaskStore interface {
	CreateOrLoadLocalTask(context.Context, store.LocalTaskCreate) (store.LocalTask, error)
	LoadLocalTask(context.Context, string, string) (store.LocalTask, error)
	ListLocalTasks(context.Context, string, int, int) ([]store.LocalTask, error)
	CancelLocalTask(context.Context, string, string) error
	RetryLocalTask(context.Context, string, string) (store.LocalTask, error)
	RecoverLocalTasks(context.Context, string) ([]store.LocalTask, error)
	TransitionLocalTask(context.Context, string, string, store.LocalTaskUpdate) (store.LocalTask, error)
}

type TaskRunner interface {
	Run(context.Context, CreateRequest, int, func(int) error) error
}

type taskService struct {
	machineID string
	store     TaskStore
	runner    TaskRunner
	ctx       context.Context
	cancel    context.CancelFunc

	mu       sync.Mutex
	active   map[string]*taskAttempt
	gates    map[string]*taskGate
	recovery []store.LocalTask
	prepared bool
}

type taskAttempt struct {
	cancel     context.CancelFunc
	done       chan struct{}
	superseded bool
}

type taskGate struct{ token chan struct{} }

func NewService(machineID string, taskStore TaskStore, runner TaskRunner) RecoverableService {
	ctx, cancel := context.WithCancel(context.Background())
	return &taskService{machineID: machineID, store: taskStore, runner: runner, ctx: ctx, cancel: cancel, active: make(map[string]*taskAttempt), gates: make(map[string]*taskGate)}
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

func (s *taskService) Create(_ context.Context, request CreateRequest) (Task, error) {
	if err := s.ready(); err != nil {
		return Task{}, err
	}
	envelope, digest, err := EncodeCreateEnvelope(request)
	if err != nil {
		return Task{}, err
	}
	taskType := "scan"
	if request.Mode == proto.LocalTaskModeScanThenAnalysis {
		taskType = "analysis"
	}
	persisted, err := s.store.CreateOrLoadLocalTask(s.ctx, store.LocalTaskCreate{TaskID: request.TaskID, MachineID: s.machineID, Source: "local", Type: taskType, Stage: 0, EnvelopeDigest: digest, Envelope: envelope})
	if err != nil {
		return Task{}, err
	}
	canonical, decodeErr := decodeCreateEnvelope(persisted.Envelope)
	if decodeErr != nil {
		return Task{}, decodeErr
	}
	if persisted.Status == "pending" {
		s.launch(persisted, canonical)
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
		request, _ := decodeCreateEnvelope(row.Envelope)
		items = append(items, taskFromStore(row, request))
	}
	next := request.Offset + len(items)
	return Page[Task]{Items: items, Offset: request.Offset, NextOffset: next}, nil
}

func (s *taskService) Cancel(ctx context.Context, taskID string) error {
	if err := (proto.LocalTaskIDRequest{TaskID: taskID}).Validate(); err != nil {
		return err
	}
	gate, err := s.acquireTaskGate(ctx, taskID)
	if err != nil {
		return err
	}
	attempt := s.currentAttempt(taskID)
	if err := s.store.CancelLocalTask(ctx, s.machineID, taskID); err != nil {
		gate.release()
		return err
	}
	s.mu.Lock()
	if attempt != nil && s.active[taskID] == attempt {
		attempt.superseded = true
	}
	s.mu.Unlock()
	if attempt != nil {
		attempt.cancel()
	}
	gate.release()
	if attempt != nil {
		return waitAttempt(ctx, attempt)
	}
	return nil
}

func (s *taskService) Retry(ctx context.Context, taskID string) (Task, error) {
	if err := (proto.LocalTaskIDRequest{TaskID: taskID}).Validate(); err != nil {
		return Task{}, err
	}
	if err := ctx.Err(); err != nil {
		return Task{}, err
	}
	loaded, err := s.store.LoadLocalTask(ctx, s.machineID, taskID)
	if err != nil {
		return Task{}, err
	}
	if _, err := decodeCreateEnvelope(loaded.Envelope); err != nil {
		return Task{}, err
	}
	gate, err := s.acquireTaskGate(ctx, taskID)
	if err != nil {
		return Task{}, err
	}
	loaded, err = s.store.LoadLocalTask(ctx, s.machineID, taskID)
	if err != nil {
		gate.release()
		return Task{}, err
	}
	request, err := decodeCreateEnvelope(loaded.Envelope)
	if err != nil {
		gate.release()
		return Task{}, err
	}
	attempt := s.currentAttempt(taskID)
	if attempt != nil && (loaded.Status == "failed" || loaded.Status == "cancelled") {
		s.mu.Lock()
		if s.active[taskID] == attempt {
			attempt.superseded = true
		}
		s.mu.Unlock()
		attempt.cancel()
		gate.release()
		if err := waitAttempt(ctx, attempt); err != nil {
			return Task{}, err
		}
		gate, err = s.acquireTaskGate(ctx, taskID)
		if err != nil {
			return Task{}, err
		}
		loaded, err = s.store.LoadLocalTask(ctx, s.machineID, taskID)
		if err != nil {
			gate.release()
			return Task{}, err
		}
		request, err = decodeCreateEnvelope(loaded.Envelope)
		if err != nil {
			gate.release()
			return Task{}, err
		}
	}
	persisted, err := s.store.RetryLocalTask(ctx, s.machineID, taskID)
	if err != nil {
		gate.release()
		return Task{}, err
	}
	s.launchHeld(persisted, request)
	gate.release()
	return taskFromStore(persisted, request), nil
}

func (s *taskService) PrepareRecovery(ctx context.Context) error {
	if err := s.ready(); err != nil {
		return err
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
	s.mu.Lock()
	if !s.prepared {
		s.recovery = recovered
		s.prepared = true
	}
	s.mu.Unlock()
	return nil
}

func (s *taskService) Resume(ctx context.Context) error {
	if err := s.PrepareRecovery(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	recovered := append([]store.LocalTask(nil), s.recovery...)
	s.recovery = nil
	s.mu.Unlock()
	for _, persisted := range recovered {
		request, err := decodeCreateEnvelope(persisted.Envelope)
		if err != nil {
			code := "recovery_envelope_invalid"
			_, transitionErr := s.store.TransitionLocalTask(ctx, s.machineID, persisted.TaskID, store.LocalTaskUpdate{Status: "failed", Stage: persisted.Stage, ProgressComplete: persisted.ProgressComplete, ProgressTotal: persisted.ProgressTotal, StatsJSON: persisted.StatsJSON, SafeErrorCode: &code})
			if transitionErr != nil {
				return transitionErr
			}
			continue
		}
		s.launch(persisted, request)
	}
	return nil
}

func (s *taskService) launch(persisted store.LocalTask, request CreateRequest) {
	gate, err := s.acquireTaskGate(context.Background(), persisted.TaskID)
	if err != nil {
		return
	}
	s.launchHeld(persisted, request)
	gate.release()
}

func (s *taskService) launchHeld(persisted store.LocalTask, request CreateRequest) {
	s.mu.Lock()
	if _, exists := s.active[persisted.TaskID]; exists {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	attempt := &taskAttempt{cancel: cancel, done: make(chan struct{})}
	s.active[persisted.TaskID] = attempt
	s.mu.Unlock()
	go s.run(ctx, persisted, request, attempt)
}

func (s *taskService) run(ctx context.Context, persisted store.LocalTask, request CreateRequest, attempt *taskAttempt) {
	defer func() {
		gate, err := s.acquireTaskGate(context.Background(), persisted.TaskID)
		if err != nil {
			close(attempt.done)
			return
		}
		s.mu.Lock()
		if s.active[persisted.TaskID] == attempt {
			delete(s.active, persisted.TaskID)
		}
		s.mu.Unlock()
		close(attempt.done)
		gate.release()
	}()
	gate, err := s.acquireTaskGate(ctx, persisted.TaskID)
	if err != nil {
		return
	}
	if s.currentAttempt(persisted.TaskID) != attempt || attempt.superseded {
		gate.release()
		return
	}
	current, err := s.store.TransitionLocalTask(ctx, s.machineID, persisted.TaskID, store.LocalTaskUpdate{Status: "running", Stage: persisted.Stage, ProgressComplete: persisted.ProgressComplete, ProgressTotal: persisted.ProgressTotal, StatsJSON: persisted.StatsJSON})
	gate.release()
	if err != nil {
		return
	}
	advance := func(stage int) error {
		updated, err := s.store.TransitionLocalTask(ctx, s.machineID, persisted.TaskID, store.LocalTaskUpdate{Status: "running", Stage: stage, ProgressComplete: current.ProgressComplete, ProgressTotal: current.ProgressTotal, StatsJSON: current.StatsJSON})
		if err == nil {
			current = updated
		}
		return err
	}
	err = s.runner.Run(ctx, request, current.Stage, advance)
	status := "succeeded"
	var code *string
	if err != nil {
		status = "failed"
		value := "task_failed"
		if errors.Is(err, context.Canceled) {
			value, status = "task_cancelled", "cancelled"
		}
		code = &value
	}
	gate, gateErr := s.acquireTaskGate(context.Background(), persisted.TaskID)
	if gateErr != nil {
		return
	}
	isCurrent := s.currentAttempt(persisted.TaskID) == attempt && !attempt.superseded
	if isCurrent {
		_, _ = s.store.TransitionLocalTask(context.Background(), s.machineID, persisted.TaskID, store.LocalTaskUpdate{Status: status, Stage: current.Stage, ProgressComplete: current.ProgressComplete, ProgressTotal: current.ProgressTotal, StatsJSON: current.StatsJSON, SafeErrorCode: code})
	}
	gate.release()
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

func (g *taskGate) release() { g.token <- struct{}{} }

func (s *taskService) currentAttempt(taskID string) *taskAttempt {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active[taskID]
}

func waitAttempt(ctx context.Context, attempt *taskAttempt) error {
	select {
	case <-attempt.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *taskService) ready() error {
	if s == nil || s.machineID == "" || s.store == nil || s.runner == nil {
		return fmt.Errorf("localtask: service dependencies are required")
	}
	return nil
}

func taskFromStore(task store.LocalTask, request CreateRequest) Task {
	result := Task{TaskID: task.TaskID, Source: task.Source, Mode: request.Mode, Stage: task.Stage, Status: task.Status, Roots: append([]string(nil), request.Roots...), Rescan: request.Rescan, Extensions: append([]string(nil), request.Extensions...), ProgressComplete: task.ProgressComplete, ProgressTotal: task.ProgressTotal, StatsJSON: task.StatsJSON, CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt}
	if task.SafeErrorCode != nil {
		result.SafeErrorCode = *task.SafeErrorCode
	}
	if task.SafeErrorMessage != nil {
		result.SafeErrorMessage = *task.SafeErrorMessage
	}
	return result
}
