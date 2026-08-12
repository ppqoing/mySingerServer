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
	retrying map[string]struct{}
	recovery []store.LocalTask
	prepared bool
}

type taskAttempt struct {
	cancel     context.CancelFunc
	done       chan struct{}
	superseded bool
	terminalMu sync.Mutex
}

func NewService(machineID string, taskStore TaskStore, runner TaskRunner) RecoverableService {
	ctx, cancel := context.WithCancel(context.Background())
	return &taskService{machineID: machineID, store: taskStore, runner: runner, ctx: ctx, cancel: cancel, active: make(map[string]*taskAttempt), retrying: make(map[string]struct{})}
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
	s.mu.Lock()
	attempt := s.active[taskID]
	s.mu.Unlock()
	if attempt != nil {
		attempt.terminalMu.Lock()
	}
	if err := s.store.CancelLocalTask(ctx, s.machineID, taskID); err != nil {
		if attempt != nil {
			attempt.terminalMu.Unlock()
		}
		return err
	}
	s.mu.Lock()
	if attempt != nil && s.active[taskID] == attempt {
		attempt.superseded = true
	}
	s.mu.Unlock()
	if attempt != nil {
		attempt.cancel()
		attempt.terminalMu.Unlock()
		select {
		case <-attempt.done:
		case <-ctx.Done():
			return ctx.Err()
		}
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
	s.mu.Lock()
	if _, claimed := s.retrying[taskID]; claimed {
		s.mu.Unlock()
		return Task{}, fmt.Errorf("localtask: retry already in progress")
	}
	s.retrying[taskID] = struct{}{}
	attempt := s.active[taskID]
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.retrying, taskID)
		s.mu.Unlock()
	}()
	if attempt != nil {
		attempt.terminalMu.Lock()
	}
	persisted, err := s.store.RetryLocalTask(ctx, s.machineID, taskID)
	if err != nil {
		if attempt != nil {
			attempt.terminalMu.Unlock()
		}
		return Task{}, err
	}
	if attempt != nil {
		s.mu.Lock()
		if s.active[taskID] == attempt {
			attempt.superseded = true
		}
		s.mu.Unlock()
		attempt.cancel()
		attempt.terminalMu.Unlock()
		request, decodeErr := decodeCreateEnvelope(persisted.Envelope)
		if decodeErr != nil {
			return Task{}, decodeErr
		}
		select {
		case <-attempt.done:
		case <-ctx.Done():
			go func() {
				<-attempt.done
				s.launch(persisted, request)
			}()
			return Task{}, ctx.Err()
		}
		s.launch(persisted, request)
		return taskFromStore(persisted, request), nil
	}
	request, err := decodeCreateEnvelope(persisted.Envelope)
	if err != nil {
		return Task{}, err
	}
	s.launch(persisted, request)
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
		s.mu.Lock()
		if s.active[persisted.TaskID] == attempt {
			delete(s.active, persisted.TaskID)
		}
		s.mu.Unlock()
		close(attempt.done)
	}()
	current, err := s.store.TransitionLocalTask(ctx, s.machineID, persisted.TaskID, store.LocalTaskUpdate{Status: "running", Stage: persisted.Stage, ProgressComplete: persisted.ProgressComplete, ProgressTotal: persisted.ProgressTotal, StatsJSON: persisted.StatsJSON})
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
	attempt.terminalMu.Lock()
	defer attempt.terminalMu.Unlock()
	s.mu.Lock()
	isCurrent := s.active[persisted.TaskID] == attempt && !attempt.superseded
	s.mu.Unlock()
	if isCurrent {
		_, _ = s.store.TransitionLocalTask(context.Background(), s.machineID, persisted.TaskID, store.LocalTaskUpdate{Status: status, Stage: current.Stage, ProgressComplete: current.ProgressComplete, ProgressTotal: current.ProgressTotal, StatsJSON: current.StatsJSON, SafeErrorCode: code})
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
