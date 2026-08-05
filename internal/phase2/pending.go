package phase2

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"dedup/internal/proto"
)

const (
	taskStatusSent    = "sent"
	taskStatusAcked   = "acked"
	taskStatusRunning = "running"
	taskStatusDone    = "done"
	taskStatusFailed  = "failed"
)

type persistedTask struct {
	Envelope RoutedTask
	Status   string
	Stats    proto.TaskStats
	LastErr  string
}

type durableTaskState struct {
	Status  string
	Stats   proto.TaskStats
	LastErr string
}

type taskStateStore interface {
	persistPending(context.Context, persistedTask) (persistedTask, error)
	restorePending(context.Context) ([]persistedTask, error)
	updateTask(
		context.Context,
		string,
		string,
		string,
		proto.TaskStats,
		string,
	) (durableTaskState, error)
}

type pendingTargetAuditor interface {
	auditPendingTargets(context.Context) error
}

type dispatchPendingError struct {
	err             error
	durablyAdmitted bool
}

func (err *dispatchPendingError) Error() string {
	return err.err.Error()
}

func (err *dispatchPendingError) Unwrap() error {
	return err.err
}

func (err *dispatchPendingError) DurablyAdmitted() bool {
	return err.durablyAdmitted
}

type taskMemory struct {
	mu    sync.Mutex
	tasks map[string]*taskEntry
}

type taskEntry struct {
	mu              sync.Mutex
	task            persistedTask
	serial          chan struct{}
	pendingTerminal bool
}

func (d *Dispatcher) stateStore() (taskStateStore, error) {
	store, ok := d.loader.(taskStateStore)
	if !ok {
		return nil, fmt.Errorf("phase2: durable task store is not configured")
	}
	return store, nil
}

func (d *Dispatcher) ensureMemory() *taskMemory {
	d.memoryOnce.Do(func() {
		d.memory = &taskMemory{tasks: make(map[string]*taskEntry)}
	})
	return d.memory
}

// DispatchPending builds both candidate classes, durably admits every
// envelope, and only then attempts delivery to currently online agents.
func (d *Dispatcher) DispatchPending(ctx context.Context) error {
	d.admissionMu.Lock()
	pending, err := d.admitPending(ctx)
	d.admissionMu.Unlock()
	if err != nil {
		return &dispatchPendingError{err: err}
	}

	var sendErrors []error
	for _, task := range pending {
		if !d.sender.IsOnline(task.Envelope.MachineID) {
			continue
		}
		if sendErr := d.sender.Send(
			task.Envelope.MachineID,
			proto.MsgPhase2Task,
			&task.Envelope.Task,
		); sendErr != nil {
			sendErrors = append(sendErrors, fmt.Errorf(
				"phase2: send task %s to %s: %w",
				task.Envelope.Task.TaskID,
				task.Envelope.MachineID,
				sendErr,
			))
		}
	}
	sendErr := errors.Join(sendErrors...)
	if sendErr != nil {
		return &dispatchPendingError{
			err:             sendErr,
			durablyAdmitted: true,
		}
	}
	return nil
}

func (d *Dispatcher) admitPending(
	ctx context.Context,
) ([]persistedTask, error) {
	store, err := d.stateStore()
	if err != nil {
		return nil, err
	}
	var built []RoutedTask
	for _, kind := range []uint8{proto.KindImage, proto.KindVideo} {
		tasks, buildErr := d.BuildTasks(ctx, kind)
		if buildErr != nil {
			return nil, buildErr
		}
		built = append(built, tasks...)
	}
	built, err = d.excludePendingCoverage(built)
	if err != nil {
		return nil, err
	}
	sort.Slice(built, func(i, j int) bool {
		if built[i].MachineID != built[j].MachineID {
			return built[i].MachineID < built[j].MachineID
		}
		return built[i].Task.TaskID < built[j].Task.TaskID
	})

	memory := d.ensureMemory()
	for _, envelope := range built {
		task, persistErr := store.persistPending(ctx, persistedTask{
			Envelope: envelope,
			Status:   taskStatusSent,
		})
		if persistErr != nil {
			return nil, persistErr
		}
		// Retain each successful durable admission locally even if a later
		// envelope fails. No sends occur until the entire batch is admitted.
		upsertMemoryTask(memory, task)
	}
	return d.pendingTasks(""), nil
}

type itemCoverage struct {
	fields uint32
	frames uint8
}

func (d *Dispatcher) excludePendingCoverage(
	built []RoutedTask,
) ([]RoutedTask, error) {
	memory := d.ensureMemory()
	memory.mu.Lock()
	entries := make([]*taskEntry, 0, len(memory.tasks))
	for _, entry := range memory.tasks {
		entries = append(entries, entry)
	}
	memory.mu.Unlock()
	coverage := make(map[string]itemCoverage)
	for _, entry := range entries {
		entry.mu.Lock()
		task := clonePersistedTask(entry.task)
		entry.mu.Unlock()
		if isTerminalTaskStatus(task.Status) {
			continue
		}
		for _, item := range task.Envelope.Task.Items {
			key := coverageKey(item)
			value := coverage[key]
			value.fields |= item.FieldsMask
			frameMask := item.FrameMask
			if item.Kind == proto.KindVideo && frameMask == 0 {
				frameMask = proto.FrameMaskFull
			}
			value.frames |= frameMask
			coverage[key] = value
		}
	}

	filtered := make([]RoutedTask, 0, len(built))
	for _, routed := range built {
		items := make([]proto.Phase2Item, 0, len(routed.Task.Items))
		for _, item := range routed.Task.Items {
			old := coverage[coverageKey(item)]
			if item.Kind == proto.KindVideo {
				item.FrameMask &^= old.frames
				if item.FrameMask == 0 {
					continue
				}
				item.FieldsMask = proto.FieldVideo6F
			} else {
				item.FieldsMask &^= old.fields
				if item.FieldsMask == 0 {
					continue
				}
			}
			if err := item.Validate(); err != nil {
				return nil, fmt.Errorf(
					"phase2: pending coverage produced invalid item: %w",
					err,
				)
			}
			items = append(items, item)
		}
		if len(items) == 0 {
			continue
		}
		routed.Task.Items = items
		if err := finalizeTaskEnvelope(&routed); err != nil {
			return nil, err
		}
		filtered = append(filtered, routed)
	}
	return filtered, nil
}

func coverageKey(item proto.Phase2Item) string {
	return fmt.Sprintf("%d:%s", item.Kind, item.SHA512)
}

// RestorePending restores only durable Phase2 envelopes. The PostgreSQL store
// performs the target discriminator filter.
func (d *Dispatcher) RestorePending(ctx context.Context) error {
	store, err := d.stateStore()
	if err != nil {
		return err
	}
	if auditor, ok := d.loader.(pendingTargetAuditor); ok {
		if err := auditor.auditPendingTargets(ctx); err != nil {
			return err
		}
	}
	tasks, err := store.restorePending(ctx)
	if err != nil {
		return err
	}
	memory := d.ensureMemory()
	for _, task := range tasks {
		taskID := task.Envelope.Task.TaskID
		if taskID == "" || task.Envelope.MachineID == "" {
			return fmt.Errorf("phase2: restored task has incomplete identity")
		}
		upsertMemoryTask(memory, task)
	}
	return nil
}

// DispatchMachinePending retries only the non-terminal Phase2 envelopes owned
// by one newly connected machine.
func (d *Dispatcher) DispatchMachinePending(
	ctx context.Context,
	machineID string,
) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !d.sender.IsOnline(machineID) {
		return nil
	}
	pending := d.pendingTasks(machineID)
	var sendErrors []error
	for _, task := range pending {
		if err := d.sender.Send(
			machineID,
			proto.MsgPhase2Task,
			&task.Envelope.Task,
		); err != nil {
			sendErrors = append(sendErrors, fmt.Errorf(
				"phase2: resend task %s to %s: %w",
				task.Envelope.Task.TaskID,
				machineID,
				err,
			))
		}
	}
	return errors.Join(sendErrors...)
}

func (d *Dispatcher) pendingTasks(machineID string) []persistedTask {
	memory := d.ensureMemory()
	memory.mu.Lock()
	entries := make([]*taskEntry, 0, len(memory.tasks))
	for _, entry := range memory.tasks {
		entries = append(entries, entry)
	}
	memory.mu.Unlock()
	var pending []persistedTask
	for _, entry := range entries {
		entry.mu.Lock()
		task := clonePersistedTask(entry.task)
		terminalTransition := entry.pendingTerminal
		entry.mu.Unlock()
		if (machineID == "" || task.Envelope.MachineID == machineID) &&
			!isTerminalTaskStatus(task.Status) &&
			!terminalTransition {
			pending = append(pending, task)
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].Envelope.MachineID != pending[j].Envelope.MachineID {
			return pending[i].Envelope.MachineID <
				pending[j].Envelope.MachineID
		}
		return pending[i].Envelope.Task.TaskID <
			pending[j].Envelope.Task.TaskID
	})
	return pending
}

// HandleMessage applies an Agent lifecycle message when it belongs to a known
// Phase2 task. It returns false for scan or otherwise unrelated messages so
// the existing GUI registry can continue handling them.
func (d *Dispatcher) HandleMessage(machineID string, message any) bool {
	taskID, status, stats, lastErr, recognized := phase2MessageState(message)
	if !recognized || taskID == "" {
		return false
	}
	if !d.beginLifecycle() {
		return false
	}
	defer d.lifecycleWG.Done()
	memory := d.ensureMemory()
	memory.mu.Lock()
	entry, ok := memory.tasks[taskID]
	memory.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case <-d.lifecycleCtx.Done():
		return false
	case <-entry.serial:
	}
	defer func() { entry.serial <- struct{}{} }()

	entry.mu.Lock()
	current := entry.task
	if current.Envelope.MachineID != machineID {
		entry.mu.Unlock()
		return false
	}
	if isTerminalTaskStatus(current.Status) {
		entry.mu.Unlock()
		return true
	}
	if isTerminalTaskStatus(status) {
		entry.pendingTerminal = true
	}
	entry.mu.Unlock()

	store, err := d.stateStore()
	if err != nil {
		clearPendingTerminal(entry)
		d.logError("update phase2 task", taskID, err)
		return true
	}
	stats = mergeTaskStats(current.Stats, stats, message)
	if lastErr == "" {
		lastErr = current.LastErr
	}
	updateCtx, cancel := context.WithTimeout(d.lifecycleCtx, 5*time.Second)
	defer cancel()
	durable, err := store.updateTask(
		updateCtx,
		taskID,
		machineID,
		status,
		stats,
		lastErr,
	)
	if err != nil {
		clearPendingTerminal(entry)
		d.logError("update phase2 task", taskID, err)
		return true
	}

	entry.mu.Lock()
	current.Status = durable.Status
	current.Stats = durable.Stats
	current.LastErr = durable.LastErr
	entry.task = current
	entry.pendingTerminal = false
	entry.mu.Unlock()
	return true
}

func clearPendingTerminal(entry *taskEntry) {
	entry.mu.Lock()
	entry.pendingTerminal = false
	entry.mu.Unlock()
}

func upsertMemoryTask(memory *taskMemory, task persistedTask) {
	taskID := task.Envelope.Task.TaskID
	memory.mu.Lock()
	entry := memory.tasks[taskID]
	if entry == nil {
		entry = &taskEntry{
			task:   clonePersistedTask(task),
			serial: make(chan struct{}, 1),
		}
		entry.serial <- struct{}{}
		memory.tasks[taskID] = entry
		memory.mu.Unlock()
		return
	}
	memory.mu.Unlock()
	entry.mu.Lock()
	entry.task = mergeAdmission(entry.task, task)
	entry.mu.Unlock()
}

func mergeAdmission(current, incoming persistedTask) persistedTask {
	if isTerminalTaskStatus(current.Status) {
		return current
	}
	if taskStatusRank(current.Status) > taskStatusRank(incoming.Status) {
		return current
	}
	if taskStatusRank(current.Status) == taskStatusRank(incoming.Status) {
		if current.Stats != (proto.TaskStats{}) {
			incoming.Stats = current.Stats
		}
		if current.LastErr != "" {
			incoming.LastErr = current.LastErr
		}
	}
	return clonePersistedTask(incoming)
}

func taskStatusRank(status string) int {
	switch status {
	case taskStatusSent:
		return 0
	case taskStatusAcked:
		return 1
	case taskStatusRunning:
		return 2
	case taskStatusDone, taskStatusFailed:
		return 3
	default:
		return -1
	}
}

func mergeTaskStats(
	current proto.TaskStats,
	incoming proto.TaskStats,
	message any,
) proto.TaskStats {
	switch message.(type) {
	case *proto.TaskDone:
		return incoming
	case *proto.TaskProgress:
		current.Total = incoming.Total
		current.Done = incoming.Done
		return current
	case *proto.TaskAck:
		if ack, ok := message.(*proto.TaskAck); ok &&
			ack.Reason == "already_done" &&
			ack.Stats != nil {
			return incoming
		}
		if incoming.Total >= 0 {
			current.Total = incoming.Total
		}
		return current
	default:
		return current
	}
}

func phase2MessageState(
	message any,
) (string, string, proto.TaskStats, string, bool) {
	switch value := message.(type) {
	case *proto.TaskAck:
		if !value.Accepted {
			return value.TaskID, taskStatusFailed, proto.TaskStats{}, value.Reason, true
		}
		switch value.Reason {
		case "already_done":
			stats := proto.TaskStats{Total: value.Total}
			if value.Stats != nil {
				stats = *value.Stats
			}
			return value.TaskID, taskStatusRunning, stats, "", true
		case "resumed":
			return value.TaskID, taskStatusRunning, proto.TaskStats{Total: value.Total}, "", true
		default:
			return value.TaskID, taskStatusAcked, proto.TaskStats{Total: value.Total}, "", true
		}
	case *proto.TaskProgress:
		return value.TaskID, taskStatusRunning, proto.TaskStats{
			Total: value.Total,
			Done:  value.Done,
		}, "", true
	case *proto.FeatureResult:
		return value.TaskID, taskStatusRunning, proto.TaskStats{}, "", true
	case *proto.Error:
		if value.TaskID == "" {
			return "", "", proto.TaskStats{}, "", false
		}
		return value.TaskID, taskStatusRunning, proto.TaskStats{}, value.Msg, true
	case *proto.TaskDone:
		return value.TaskID, taskStatusDone, value.Stats, "", true
	default:
		return "", "", proto.TaskStats{}, "", false
	}
}

func clonePersistedTask(task persistedTask) persistedTask {
	task.Envelope.Task.Items = append(
		[]proto.Phase2Item(nil),
		task.Envelope.Task.Items...,
	)
	return task
}

func isTerminalTaskStatus(status string) bool {
	return status == taskStatusDone || status == taskStatusFailed
}

func (d *Dispatcher) logError(message, taskID string, err error) {
	if d.log != nil {
		d.log.Error(message, "task_id", taskID, "err", err)
	}
}
