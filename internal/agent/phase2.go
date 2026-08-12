package agent

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dedup/internal/proto"
	"dedup/internal/store"
	"dedup/internal/worker"
)

const maxPhase2TaskItems = 5000

type Phase2Manager struct {
	machineID string
	store     Phase2Store
	pool      WorkerPool
	router    *PoolRouter
	resolver  DiskResolver
	log       *slog.Logger

	mu          sync.Mutex
	tasks       map[string]*phase2State
	connections map[*pendingPhase2Binding]struct{}
	run         func(*phase2State)
	closing     bool
	wg          sync.WaitGroup
	retention   time.Duration
	workCtx     context.Context
	cancel      context.CancelCauseFunc
}

type Phase2Store interface {
	Phase2CommittedStateForFields(
		context.Context,
		string,
		string,
		store.MediaKind,
		uint32,
	) (store.Phase2Committed, error)
}

type phase2State struct {
	task proto.Phase2Task

	mu         sync.Mutex
	featureMu  sync.Mutex
	status     string
	stats      proto.TaskStats
	sender     Sender
	disconnect func()
	binding    uint64
	seq        uint64
	start      sync.Once
	finish     sync.Once
	results    []proto.FeatureResult
	terminal   *proto.TaskDone
	timer      *time.Timer
}

type pendingPhase2Binding struct {
	manager    *Phase2Manager
	mu         sync.Mutex
	state      *phase2State
	sender     Sender
	disconnect func()
	binding    uint64
	detached   bool
	closeOnce  sync.Once
}

func NewPhase2Manager(machineID string) *Phase2Manager {
	workCtx, cancel := context.WithCancelCause(context.Background())
	return &Phase2Manager{
		machineID:   machineID,
		tasks:       make(map[string]*phase2State),
		connections: make(map[*pendingPhase2Binding]struct{}),
		retention:   10 * time.Minute,
		workCtx:     workCtx,
		cancel:      cancel,
	}
}

func NewPhase2ManagerWithRuntime(
	machineID string,
	phase2Store Phase2Store,
	pool WorkerPool,
	router *PoolRouter,
	resolver DiskResolver,
	log *slog.Logger,
) *Phase2Manager {
	manager := NewPhase2Manager(machineID)
	manager.store = phase2Store
	manager.pool = pool
	manager.router = router
	manager.resolver = resolver
	if manager.resolver == nil {
		manager.resolver = resolveDisk
	}
	manager.log = log
	manager.run = manager.runTask
	return manager
}

func (m *Phase2Manager) Prepare(
	task proto.Phase2Task,
	sender Sender,
) (proto.TaskAck, func()) {
	ack, start, _ := m.PrepareConnection(task, sender)
	return ack, start
}

func (m *Phase2Manager) PrepareConnection(
	task proto.Phase2Task,
	sender Sender,
) (proto.TaskAck, func(), func()) {
	return m.prepareConnection(task, sender, nil)
}

func (m *Phase2Manager) PrepareConnectionWithDisconnect(
	task proto.Phase2Task,
	sender Sender,
	disconnect func(),
) (proto.TaskAck, func(), func()) {
	return m.prepareConnection(task, sender, disconnect)
}

func (m *Phase2Manager) prepareConnection(
	task proto.Phase2Task,
	sender Sender,
	disconnect func(),
) (proto.TaskAck, func(), func()) {
	if err := validatePhase2Envelope(task, m.machineID); err != nil {
		return rejectedAck(task.TaskID, err.Error()), nil, nil
	}
	envelope := clonePhase2Task(task)

	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return rejectedAck(task.TaskID, "phase2 manager is shutting down"),
			nil,
			nil
	}
	if state, exists := m.tasks[task.TaskID]; exists {
		if !samePhase2Envelope(state.task, envelope) {
			m.mu.Unlock()
			return rejectedAck(task.TaskID, "task_id envelope mismatch"),
				nil,
				nil
		}
		pending := &pendingPhase2Binding{
			manager: m, state: state, sender: sender, disconnect: disconnect,
		}
		m.connections[pending] = struct{}{}
		state.mu.Lock()
		status := state.status
		stats := state.stats
		state.mu.Unlock()
		m.mu.Unlock()
		if status == proto.StatusDone {
			return proto.TaskAck{
				TaskID:   task.TaskID,
				Accepted: true,
				Reason:   "already_done",
				Total:    int64(len(envelope.Items)),
				Stats:    &stats,
			}, m.startTask(state, pending), pending.detach
		}
		return proto.TaskAck{
			TaskID:   task.TaskID,
			Accepted: true,
			Reason:   "resumed",
			Total:    int64(len(envelope.Items)),
		}, m.startTask(state, pending), pending.detach
	}

	state := &phase2State{
		task:   envelope,
		status: proto.StatusPending,
	}
	pending := &pendingPhase2Binding{
		manager: m, state: state, sender: sender, disconnect: disconnect,
	}
	m.connections[pending] = struct{}{}
	m.tasks[task.TaskID] = state
	m.mu.Unlock()
	return proto.TaskAck{
			TaskID:   task.TaskID,
			Accepted: true,
			Reason:   "accepted",
			Total:    int64(len(envelope.Items)),
		}, m.startTask(state, pending),
		pending.detach
}

func (m *Phase2Manager) startTask(
	state *phase2State,
	pending *pendingPhase2Binding,
) func() {
	return func() {
		if pending != nil {
			pending.activate()
		}
		state.start.Do(func() {
			state.mu.Lock()
			state.status = "running"
			state.mu.Unlock()
			if m.run != nil {
				m.wg.Add(1)
				go func() {
					defer m.wg.Done()
					m.run(state)
				}()
			}
		})
	}
}

func (m *Phase2Manager) complete(state *phase2State, stats proto.TaskStats) {
	state.finish.Do(func() {
		done := proto.TaskDone{
			TaskID: state.task.TaskID,
			Stats:  stats,
		}
		state.featureMu.Lock()
		state.mu.Lock()
		state.status = proto.StatusDone
		state.stats = stats
		state.terminal = &done
		state.mu.Unlock()
		if taskPool, ok := m.pool.(interface{ EndTask(string) }); ok {
			taskPool.EndTask(state.task.TaskID)
		}
		copy := done
		state.sendUnlocked(proto.MsgTaskDone, &copy)
		state.featureMu.Unlock()
		m.scheduleRetention(state)
	})
}

func (pending *pendingPhase2Binding) activate() {
	pending.manager.activatePending(pending)
}

func (pending *pendingPhase2Binding) detach() {
	pending.manager.detachPending(pending)
}

func (pending *pendingPhase2Binding) closeConnection() {
	pending.closeOnce.Do(func() {
		if pending.disconnect != nil {
			pending.disconnect()
		}
	})
}

func (m *Phase2Manager) activatePending(pending *pendingPhase2Binding) {
	state := pending.state
	state.featureMu.Lock()
	m.mu.Lock()
	pending.mu.Lock()
	_, tracked := m.connections[pending]
	if pending.detached || pending.binding != 0 || !tracked || m.closing {
		denied := m.closing && !pending.detached
		if denied {
			pending.detached = true
			delete(m.connections, pending)
		}
		pending.mu.Unlock()
		m.mu.Unlock()
		state.featureMu.Unlock()
		if denied {
			pending.closeConnection()
		}
		return
	}
	pending.binding = state.bindSender(
		pending.sender,
		pending.closeConnection,
	)
	binding := pending.binding
	pending.mu.Unlock()
	m.mu.Unlock()
	state.replayBindingLocked(binding)
	state.featureMu.Unlock()
}

func (m *Phase2Manager) detachPending(pending *pendingPhase2Binding) {
	m.mu.Lock()
	pending.mu.Lock()
	pending.detached = true
	binding := pending.binding
	delete(m.connections, pending)
	pending.mu.Unlock()
	m.mu.Unlock()
	if binding != 0 {
		pending.state.detachSender(binding)
	}
}

func (state *phase2State) bindSender(sender Sender, disconnect func()) uint64 {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.sender = sender
	state.disconnect = disconnect
	state.binding++
	return state.binding
}

func (state *phase2State) detachSender(binding uint64) {
	state.mu.Lock()
	if state.binding == binding {
		state.sender = nil
		state.disconnect = nil
		state.binding++
	}
	state.mu.Unlock()
}

func (state *phase2State) send(msgType uint8, value any) {
	state.featureMu.Lock()
	defer state.featureMu.Unlock()
	state.sendUnlocked(msgType, value)
}

func (state *phase2State) sendUnlocked(msgType uint8, value any) {
	state.mu.Lock()
	sender := state.sender
	binding := state.binding
	state.mu.Unlock()
	if sender == nil {
		return
	}
	if err := sender(msgType, value); err != nil {
		state.mu.Lock()
		if state.binding == binding {
			state.sender = nil
			state.disconnect = nil
			state.binding++
		}
		state.mu.Unlock()
	}
}

func (state *phase2State) publish(item proto.FeatureItem) {
	state.featureMu.Lock()
	defer state.featureMu.Unlock()
	state.mu.Lock()
	state.seq++
	sequence := state.seq
	result := proto.FeatureResult{
		TaskID: state.task.TaskID,
		Seq:    sequence,
		Items:  []proto.FeatureItem{cloneFeatureItem(item)},
	}
	state.results = append(state.results, cloneFeatureResult(result))
	state.mu.Unlock()
	outgoing := cloneFeatureResult(result)
	state.sendUnlocked(proto.MsgFeatureResult, &outgoing)
}

func (state *phase2State) replayBindingLocked(binding uint64) {
	state.mu.Lock()
	results := make([]proto.FeatureResult, len(state.results))
	for index, result := range state.results {
		results[index] = cloneFeatureResult(result)
	}
	var terminal *proto.TaskDone
	if state.terminal != nil {
		copy := *state.terminal
		terminal = &copy
	}
	state.mu.Unlock()
	for index := range results {
		if !state.sendBinding(
			binding,
			proto.MsgFeatureResult,
			&results[index],
		) {
			return
		}
	}
	if terminal != nil {
		state.sendBinding(binding, proto.MsgTaskDone, terminal)
	}
}

func (state *phase2State) disconnectSender() {
	state.mu.Lock()
	disconnect := state.disconnect
	state.sender = nil
	state.disconnect = nil
	state.binding++
	state.mu.Unlock()
	if disconnect != nil {
		disconnect()
	}
}

func (state *phase2State) sendBinding(
	binding uint64,
	msgType uint8,
	value any,
) bool {
	state.mu.Lock()
	if state.binding != binding || state.sender == nil {
		state.mu.Unlock()
		return false
	}
	sender := state.sender
	state.mu.Unlock()
	if err := sender(msgType, value); err != nil {
		state.detachSender(binding)
		return false
	}
	return true
}

type phase2Work struct {
	index       int
	item        proto.Phase2Item
	diskNo      int64
	job         *worker.JobMsg
	terminal    <-chan poolTerminal
	cancelRoute func()
}

type phase2Outcome struct {
	index   int
	item    proto.FeatureItem
	crash   *proto.CrashNotice
	skipped bool
}

func (m *Phase2Manager) runTask(state *phase2State) {
	started := time.Now()
	total := len(state.task.Items)
	state.send(proto.MsgTaskProgress, &proto.TaskProgress{
		TaskID: state.task.TaskID,
		Total:  int64(total),
	})
	outcomes := make(chan phase2Outcome, total)
	disks := make(map[int64][]phase2Work)
	for index, item := range state.task.Items {
		work, outcome, ok := m.prepareWork(m.workCtx, index, state.task.Stage, item)
		if !ok {
			outcomes <- outcome
			continue
		}
		disks[work.diskNo] = append(disks[work.diskNo], work)
	}

	diskNumbers := make([]int64, 0, len(disks))
	for diskNo := range disks {
		diskNumbers = append(diskNumbers, diskNo)
	}
	sort.Slice(diskNumbers, func(left, right int) bool {
		return diskNumbers[left] < diskNumbers[right]
	})
	var workers sync.WaitGroup
	for _, diskNo := range diskNumbers {
		diskWork := disks[diskNo]
		workers.Add(1)
		go func() {
			defer workers.Done()
			for _, work := range diskWork {
				outcomes <- m.executeWork(m.workCtx, state, work)
			}
		}()
	}
	go func() {
		workers.Wait()
		close(outcomes)
	}()

	var done, skipped, failed, crashes atomic.Int64
	for outcome := range outcomes {
		if outcome.crash != nil {
			state.send(proto.MsgCrashNotice, outcome.crash)
			crashes.Add(1)
		}
		state.publish(outcome.item)
		done.Add(1)
		if outcome.skipped {
			skipped.Add(1)
		}
		if outcome.item.Status != proto.StatusDone {
			failed.Add(1)
		}
		state.send(proto.MsgTaskProgress, &proto.TaskProgress{
			TaskID: state.task.TaskID,
			Done:   done.Load(),
			Total:  int64(total),
		})
	}
	m.complete(state, proto.TaskStats{
		Total:     int64(total),
		Done:      done.Load(),
		Skipped:   skipped.Load(),
		Failed:    failed.Load(),
		Crashes:   crashes.Load(),
		ElapsedMS: time.Since(started).Milliseconds(),
	})
}

func (m *Phase2Manager) prepareWork(
	ctx context.Context,
	index int,
	stage uint8,
	item proto.Phase2Item,
) (phase2Work, phase2Outcome, bool) {
	work := phase2Work{index: index, item: item}
	if m.resolver == nil {
		return work, failedPhase2Outcome(
			index,
			item,
			0,
			"disk",
			fmt.Errorf("physical disk resolver is unavailable"),
		), false
	}
	diskNo, _, err := m.resolver(item.Path)
	if err != nil {
		return work, failedPhase2Outcome(index, item, 0, "disk", err), false
	}
	work.diskNo = diskNo
	if m.store == nil {
		return work, failedPhase2Outcome(
			index,
			item,
			0,
			"store",
			fmt.Errorf("phase2 local store is unavailable"),
		), false
	}
	kind := store.MediaImage
	workerKind := worker.MediaImage
	if item.Kind == proto.KindVideo {
		kind = store.MediaVideo
		workerKind = worker.MediaVideo
	}
	committed, err := m.store.Phase2CommittedStateForFields(
		ctx,
		m.machineID,
		item.Path,
		kind,
		item.FieldsMask,
	)
	if err != nil {
		return work, failedPhase2Outcome(index, item, 0, "store", err), false
	}
	if committed.SHA512 != item.SHA512 {
		return work, failedPhase2Outcome(
			index,
			item,
			0,
			"stale",
			fmt.Errorf(
				"path now owns SHA-512 %q, accepted %q",
				committed.SHA512,
				item.SHA512,
			),
		), false
	}
	fieldsMask := item.FieldsMask
	frameMask := item.FrameMask
	if item.Kind == proto.KindVideo {
		if frameMask == 0 {
			frameMask = proto.FrameMaskFull
		}
	}
	knownSHA, err := hex.DecodeString(item.SHA512)
	if err != nil || len(knownSHA) != 64 {
		return work, failedPhase2Outcome(
			index,
			item,
			0,
			"proto",
			fmt.Errorf("decode canonical SHA-512: %w", err),
		), false
	}
	if m.router == nil || m.pool == nil {
		return work, failedPhase2Outcome(
			index,
			item,
			fieldsMask,
			"worker",
			fmt.Errorf("worker pool is unavailable"),
		), false
	}
	job := &worker.JobMsg{
		JobID:       m.router.NextJobID(),
		ScanTaskID:  stateTaskIDPlaceholder,
		Path:        item.Path,
		Kind:        workerKind,
		Phase:       worker.Phase2,
		ScreenStage: worker.ScreenStage(stage),
		Source:      worker.JobSourceManager,
		FieldsMask:  fieldsMask,
		Size:        item.Size,
		MTimeMS:     item.MTimeMS,
		KnownSHA:    knownSHA,
		FrameMask:   frameMask,
		DurationMS:  item.DurationMS,
	}
	work.job = job
	return work, phase2Outcome{}, true
}

const stateTaskIDPlaceholder = ""

func (m *Phase2Manager) executeWork(
	ctx context.Context,
	state *phase2State,
	work phase2Work,
) phase2Outcome {
	work.job.ScanTaskID = state.task.TaskID
	terminal, cancelRoute, err := m.router.Register(work.job)
	if err != nil {
		return failedPhase2Outcome(
			work.index,
			work.item,
			work.job.FieldsMask,
			"worker",
			err,
		)
	}
	work.terminal = terminal
	work.cancelRoute = cancelRoute
	if err := m.pool.Submit(work.job); err != nil {
		cancelRoute()
		return failedPhase2Outcome(
			work.index,
			work.item,
			work.job.FieldsMask,
			"worker",
			err,
		)
	}
	var terminalValue poolTerminal
	select {
	case terminalValue = <-terminal:
		cancelRoute()
	case <-ctx.Done():
		cancelRoute()
		return failedPhase2Outcome(
			work.index,
			work.item,
			work.job.FieldsMask,
			"shutdown",
			context.Cause(ctx),
		)
	}
	if terminalValue.err != nil {
		return failedPhase2Outcome(
			work.index,
			work.item,
			work.job.FieldsMask,
			"worker",
			terminalValue.err,
		)
	}
	if terminalValue.crash != nil {
		crash := terminalValue.crash
		item := basePhase2Feature(work.item, proto.StatusCrash)
		item.Err = crash.Reason
		item.FieldErrors = []proto.FieldError{{
			Field: work.job.FieldsMask,
			Stage: "worker",
			Msg:   crash.Reason,
		}}
		return phase2Outcome{
			index: work.index,
			item:  item,
			crash: &proto.CrashNotice{
				TaskID:   state.task.TaskID,
				PID:      crash.PID,
				Path:     crash.File,
				ExitCode: int(crash.ExitCode),
			},
		}
	}
	return phase2Outcome{
		index: work.index,
		item: phase2FeatureFromWorker(
			work.item,
			work.job,
			terminalValue.result,
		),
	}
}

func failedPhase2Outcome(
	index int,
	item proto.Phase2Item,
	field uint32,
	stage string,
	err error,
) phase2Outcome {
	feature := basePhase2Feature(item, proto.StatusFailed)
	feature.Err = err.Error()
	feature.FieldErrors = []proto.FieldError{{
		Field: field,
		Stage: stage,
		Msg:   err.Error(),
	}}
	return phase2Outcome{index: index, item: feature}
}

func basePhase2Feature(
	item proto.Phase2Item,
	status string,
) proto.FeatureItem {
	return proto.FeatureItem{
		Path:       item.Path,
		SHA512:     item.SHA512,
		Size:       item.Size,
		MTime:      item.MTimeMS,
		Status:     status,
		DurationMS: phase2Duration(item),
	}
}

func phase2Duration(item proto.Phase2Item) *int64 {
	if item.Kind != proto.KindVideo {
		return nil
	}
	duration := item.DurationMS
	return &duration
}

func phase2FeatureFromWorker(
	accepted proto.Phase2Item,
	job *worker.JobMsg,
	result *worker.JobResultMsg,
) proto.FeatureItem {
	item := basePhase2Feature(accepted, proto.StatusFailed)
	if result == nil {
		item.Err = "worker returned no result"
		return item
	}
	item.FieldsDone = result.FieldsDone & job.FieldsMask
	if item.FieldsDone&proto.FieldPHashParts != 0 {
		item.PHashParts = append([]byte(nil), result.PHashParts...)
	}
	if item.FieldsDone&proto.FieldSobelHist != 0 {
		item.SobelHist = append([]byte(nil), result.SobelHist...)
	}
	item.FieldErrors = make([]proto.FieldError, len(result.Errors))
	for index, fieldError := range result.Errors {
		item.FieldErrors[index] = proto.FieldError{
			Field: fieldError.Field,
			Stage: fieldError.Stage,
			Msg:   fieldError.Msg,
		}
	}
	effectiveFrameMask := job.FrameMask
	if effectiveFrameMask == 0 {
		effectiveFrameMask = proto.FrameMaskFull
	}
	workerFrames := result.Frames
	if len(workerFrames) == 0 && job.Kind == worker.MediaVideo &&
		job.FieldsMask&(worker.MaskVideo6F|worker.MaskVideo6FPHash|worker.MaskVideo6FSobel) != 0 {
		workerFrames = make([]worker.FrameFeature, 0, 6)
		for index, frame := range result.FrameResults {
			bit := uint8(1 << uint(index))
			if effectiveFrameMask&bit == 0 {
				continue
			}
			converted := worker.FrameFeature{
				FrameIdx: index, TimeMS: frame.TimeMS,
				PDQ256: append([]byte(nil), frame.PDQ256...), Quality: frame.Quality,
				PHashParts: append([]byte(nil), frame.PHashParts...),
				SobelHist:  append([]byte(nil), frame.SobelHist...),
			}
			if result.FramesDone&bit == 0 {
				converted.Error = fmt.Sprintf("native_status_%d", frame.Status)
			}
			workerFrames = append(workerFrames, converted)
		}
	}
	item.Frames = make([]proto.FrameFeature, 0, len(workerFrames))
	validFrames := 0
	for _, frame := range workerFrames {
		if frame.FrameIdx < 0 || frame.FrameIdx >= 6 ||
			effectiveFrameMask&(1<<uint(frame.FrameIdx)) == 0 {
			continue
		}
		mapped := proto.FrameFeature{
			FrameIdx: frame.FrameIdx,
			TimeMS:   frame.TimeMS,
			PDQ256:   append([]byte(nil), frame.PDQ256...),
			Quality:  frame.Quality,
			PHashParts: append(
				[]byte(nil),
				frame.PHashParts...,
			),
			SobelHist: append([]byte(nil), frame.SobelHist...),
			Error:     frame.Error,
		}
		switch job.ScreenStage {
		case worker.ScreenStageTwo:
			mapped.PDQ256, mapped.Quality, mapped.SobelHist = nil, 0, nil
		case worker.ScreenStageThree:
			mapped.PDQ256, mapped.Quality, mapped.PHashParts = nil, 0, nil
		}
		item.Frames = append(item.Frames, mapped)
		valid := mapped.Error == ""
		switch job.ScreenStage {
		case worker.ScreenStageTwo:
			valid = valid && len(mapped.PHashParts) != 0
		case worker.ScreenStageThree:
			valid = valid && len(mapped.SobelHist) != 0
		default:
			valid = valid && len(mapped.PDQ256) != 0 &&
				len(mapped.PHashParts) != 0 && len(mapped.SobelHist) != 0
		}
		if valid {
			validFrames++
		}
	}
	if len(item.FieldErrors) != 0 {
		parts := make([]string, 0, len(item.FieldErrors))
		for _, fieldError := range item.FieldErrors {
			if fieldError.Stage == "" {
				parts = append(parts, fieldError.Msg)
			} else {
				parts = append(parts, fieldError.Stage+": "+fieldError.Msg)
			}
		}
		item.Err = strings.Join(parts, "; ")
	}
	switch {
	case len(item.FieldErrors) == 0 &&
		item.FieldsDone == job.FieldsMask:
		item.Status = proto.StatusDone
	case item.FieldsDone != 0 || validFrames != 0:
		item.Status = proto.StatusPartial
	default:
		item.Status = proto.StatusFailed
	}
	return item
}

func cloneFeatureItem(item proto.FeatureItem) proto.FeatureItem {
	item.FieldErrors = append([]proto.FieldError(nil), item.FieldErrors...)
	item.PHashParts = append([]byte(nil), item.PHashParts...)
	item.SobelHist = append([]byte(nil), item.SobelHist...)
	item.DurationMS = cloneInt64(item.DurationMS)
	frames := item.Frames
	item.Frames = make([]proto.FrameFeature, len(frames))
	for index, frame := range frames {
		item.Frames[index] = frame
		item.Frames[index].PDQ256 = append([]byte(nil), frame.PDQ256...)
		item.Frames[index].PHashParts = append(
			[]byte(nil),
			frame.PHashParts...,
		)
		item.Frames[index].SobelHist = append(
			[]byte(nil),
			frame.SobelHist...,
		)
	}
	return item
}

func cloneFeatureResult(result proto.FeatureResult) proto.FeatureResult {
	items := result.Items
	result.Items = make([]proto.FeatureItem, len(items))
	for index, item := range items {
		result.Items[index] = cloneFeatureItem(item)
	}
	return result
}

func (m *Phase2Manager) scheduleRetention(state *phase2State) {
	if m.retention <= 0 {
		return
	}
	timer := time.AfterFunc(m.retention, func() {
		m.mu.Lock()
		if m.tasks[state.task.TaskID] == state {
			delete(m.tasks, state.task.TaskID)
		}
		m.mu.Unlock()
	})
	state.mu.Lock()
	state.timer = timer
	state.mu.Unlock()
}

func (m *Phase2Manager) Shutdown(ctx context.Context) error {
	type connectionToClose struct {
		pending *pendingPhase2Binding
		binding uint64
	}
	m.mu.Lock()
	m.closing = true
	states := make([]*phase2State, 0, len(m.tasks))
	for _, state := range m.tasks {
		states = append(states, state)
	}
	connections := make([]connectionToClose, 0, len(m.connections))
	for pending := range m.connections {
		pending.mu.Lock()
		pending.detached = true
		connections = append(connections, connectionToClose{
			pending: pending,
			binding: pending.binding,
		})
		pending.mu.Unlock()
		delete(m.connections, pending)
	}
	m.mu.Unlock()
	for _, connection := range connections {
		if connection.binding != 0 {
			connection.pending.state.detachSender(connection.binding)
		}
		connection.pending.closeConnection()
	}
	for _, state := range states {
		state.disconnectSender()
		m.startTask(state, nil)()
	}

	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	var shutdownErr error
	select {
	case <-done:
	case <-ctx.Done():
		shutdownErr = ctx.Err()
		if stopper, ok := m.pool.(interface{ StopAccepting() }); ok {
			stopper.StopAccepting()
		}
		if m.cancel != nil {
			m.cancel(shutdownErr)
		}
		<-done
	}

	m.mu.Lock()
	for taskID, state := range m.tasks {
		state.mu.Lock()
		if state.timer != nil {
			state.timer.Stop()
			state.timer = nil
		}
		state.results = nil
		state.terminal = nil
		state.mu.Unlock()
		delete(m.tasks, taskID)
	}
	m.mu.Unlock()
	return shutdownErr
}

func validatePhase2Envelope(task proto.Phase2Task, machineID string) error {
	if task.TaskID == "" {
		return fmt.Errorf("empty task_id")
	}
	if len(task.Items) == 0 {
		return fmt.Errorf("empty items")
	}
	if err := task.Validate(); err != nil {
		return err
	}
	if len(task.Items) > maxPhase2TaskItems {
		return fmt.Errorf(
			"item count %d exceeds shard limit %d",
			len(task.Items),
			maxPhase2TaskItems,
		)
	}

	pathToSHA := make(map[string]string, len(task.Items))
	shaToPath := make(map[string]string, len(task.Items))
	for index, item := range task.Items {
		if item.MachineID != machineID {
			return fmt.Errorf(
				"item[%d] machine_id %q does not match local machine %q",
				index,
				item.MachineID,
				machineID,
			)
		}
		path := pathKey(item.Path)
		if _, exists := pathToSHA[path]; exists {
			return fmt.Errorf(
				"duplicate (machine_id,path) at item[%d]",
				index,
			)
		}
		pathToSHA[path] = item.SHA512
		if priorPath, exists := shaToPath[item.SHA512]; exists &&
			priorPath != path {
			return fmt.Errorf(
				"conflicting duplicate SHA/path identity at item[%d]",
				index,
			)
		}
		shaToPath[item.SHA512] = path
	}
	return nil
}

func clonePhase2Task(task proto.Phase2Task) proto.Phase2Task {
	task.Items = append([]proto.Phase2Item(nil), task.Items...)
	return task
}

func samePhase2Envelope(left, right proto.Phase2Task) bool {
	if left.TaskID != right.TaskID || left.Stage != right.Stage || len(left.Items) != len(right.Items) {
		return false
	}
	for index := range left.Items {
		if left.Items[index] != right.Items[index] {
			return false
		}
	}
	return true
}
