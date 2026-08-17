package agent

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dedup/internal/config"
	"dedup/internal/diskio"
	"dedup/internal/diskmap"
	fileenum "dedup/internal/enum"
	"dedup/internal/proto"
	"dedup/internal/store"
	"dedup/internal/worker"
)

type Sender func(msgType uint8, value any) error

type DiskResolver func(root string) (diskio.Identity, error)

type WorkerPool interface {
	Submit(*worker.JobMsg) error
	Results() <-chan *worker.JobResultMsg
	Crashes() <-chan worker.CrashRecord
	Metrics() worker.MetricsSnapshot
}

type contextWorkerPool interface {
	SubmitContext(context.Context, *worker.JobMsg) error
}

func submitScanJob(ctx context.Context, pool WorkerPool, job *worker.JobMsg) error {
	if contextual, ok := pool.(contextWorkerPool); ok {
		return contextual.SubmitContext(ctx, job)
	}
	return pool.Submit(job)
}

type ScanManager struct {
	cfg             *config.AgentConfig
	st              *store.DB
	enumr           fileenum.Enumerator
	hasher          Hasher
	log             *slog.Logger
	errLog          *slog.Logger
	resolver        DiskResolver
	pool            WorkerPool
	router          *PoolRouter
	limiter         *byteLimiter
	observer        ScanObserver
	ioController    diskio.Controller
	ioIdentities    map[diskio.DiskKey]diskio.Identity
	pendingSnapshot func(context.Context, string) (map[int64][]store.PendingFile, error)
	progressTicks   func() (<-chan time.Time, func())
	pendingJobsFull func()

	mu    sync.Mutex
	tasks map[scanIdentity]*ScanState
	disks map[int64]diskio.Identity
	runMu sync.Mutex
}

type scanIdentity struct {
	taskID     string
	instanceID string
}

func scanTaskIdentity(task proto.ScanTask) scanIdentity {
	return scanIdentity{taskID: task.TaskID, instanceID: task.InstanceID}
}

type ScanState struct {
	Task   proto.ScanTask
	Status string
	Stats  proto.TaskStats

	mu           sync.Mutex
	featureMu    sync.Mutex
	sender       Sender
	binding      uint64
	seq          uint64
	start        sync.Once
	control      sync.Once
	stopOnce     sync.Once
	dispatchMu   sync.Mutex
	workCtx      context.Context
	abortWork    context.CancelFunc
	dispatchCtx  context.Context
	stopDispatch context.CancelFunc
	stopNew      chan struct{}
	drainReason  proto.TaskDrainReason
	startedAt    time.Time

	total               atomic.Int64
	done                atomic.Int64
	failed              atomic.Int64
	scanErrors          atomic.Int64
	speedWin            *speedWindow
	poolStart           worker.MetricsSnapshot
	enumerationComplete atomic.Bool
}

var errScanDrainRequested = errors.New("agent: scan drain requested")

func NewScanManager(
	cfg *config.AgentConfig,
	st *store.DB,
	enumr fileenum.Enumerator,
	hasher Hasher,
	log *slog.Logger,
	errLog *slog.Logger,
) *ScanManager {
	return NewScanManagerWithResolver(
		cfg,
		st,
		enumr,
		hasher,
		log,
		errLog,
		resolveDisk,
	)
}

func NewScanManagerWithPool(
	cfg *config.AgentConfig,
	st *store.DB,
	enumr fileenum.Enumerator,
	hasher Hasher,
	pool WorkerPool,
	log *slog.Logger,
	errLog *slog.Logger,
) *ScanManager {
	manager := NewScanManager(cfg, st, enumr, hasher, log, errLog)
	manager.pool = pool
	manager.router = NewPoolRouter(pool, log)
	return manager
}

func NewScanManagerWithPoolRouter(
	cfg *config.AgentConfig,
	st *store.DB,
	enumr fileenum.Enumerator,
	hasher Hasher,
	pool WorkerPool,
	router *PoolRouter,
	log *slog.Logger,
	errLog *slog.Logger,
) *ScanManager {
	manager := NewScanManager(cfg, st, enumr, hasher, log, errLog)
	manager.pool = pool
	manager.router = router
	return manager
}

func NewScanManagerWithResolver(
	cfg *config.AgentConfig,
	st *store.DB,
	enumr fileenum.Enumerator,
	hasher Hasher,
	log *slog.Logger,
	errLog *slog.Logger,
	resolver DiskResolver,
) *ScanManager {
	manager := &ScanManager{
		cfg:      cfg,
		st:       st,
		enumr:    enumr,
		hasher:   hasher,
		log:      log,
		errLog:   errLog,
		resolver: resolver,
		tasks:    make(map[scanIdentity]*ScanState),
		disks:    make(map[int64]diskio.Identity),
		limiter:  newByteLimiter(int64(cfg.Tuning.PendingBytesMB) << 20),
	}
	manager.pendingSnapshot = st.PendingSnapshot
	manager.progressTicks = func() (<-chan time.Time, func()) {
		ticker := time.NewTicker(time.Second)
		return ticker.C, ticker.Stop
	}
	return manager
}

func (m *ScanManager) SetObserver(observer ScanObserver) {
	m.mu.Lock()
	m.observer = observer
	m.mu.Unlock()
}

func (m *ScanManager) SetIOController(
	controller diskio.Controller,
	identities ...map[diskio.DiskKey]diskio.Identity,
) {
	m.mu.Lock()
	m.ioController = controller
	if len(identities) != 0 {
		m.ioIdentities = identities[0]
	}
	m.mu.Unlock()
}

func (m *ScanManager) currentObserver() ScanObserver {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.observer
}

func resolveDisk(root string) (diskio.Identity, error) {
	mountPoint, err := diskmap.MountPointOf(root)
	if err != nil {
		return diskio.Identity{}, err
	}
	info, err := diskmap.Resolve(mountPoint)
	if err != nil {
		return diskio.Identity{}, err
	}
	if !info.MediaTypeKnown {
		logUnknownDiskMedia(slog.Default(), root, mountPoint, info.DeviceNumber)
	}
	return info.Identity, nil
}

func diskNumber(identity diskio.Identity) int64 {
	if len(identity.DiskNos) == 0 {
		return 0
	}
	return int64(identity.DiskNos[0])
}

func resolvePhase2Disk(root string) (int64, bool, error) {
	identity, err := resolveDisk(root)
	if err != nil {
		return 0, false, err
	}
	return diskNumber(identity), identity.KnownSSD && identity.SSD, nil
}

func logUnknownDiskMedia(logger *slog.Logger, root, _ string, deviceNumber uint32) {
	logger.Warn("disk media type unavailable, using HDD scheduling", "path_id", worker.PathID(root), "error_code", "disk_media_type_unknown", "device_number", deviceNumber)
}

func (m *ScanManager) Handle(task proto.ScanTask, sender Sender) proto.TaskAck {
	ack, start := m.Prepare(task, sender)
	if start != nil {
		start()
	}
	return ack
}

func (m *ScanManager) Prepare(
	task proto.ScanTask,
	sender Sender,
) (proto.TaskAck, func()) {
	if task.TaskID == "" {
		return rejectedAck(task.TaskID, "empty task_id"), nil
	}
	if task.Phase != 1 {
		return rejectedAck(task.TaskID, "only phase=1 is supported in M1"), nil
	}
	if len(task.Roots) == 0 {
		return rejectedAck(task.TaskID, "empty roots"), nil
	}

	m.mu.Lock()
	identity := scanTaskIdentity(task)
	if state, exists := m.tasks[identity]; exists {
		if !sameScanEnvelope(state.Task, task) {
			m.mu.Unlock()
			return rejectedAck(task.TaskID, "task_id envelope mismatch"), nil
		}
		state.bindSender(sender)
		state.mu.Lock()
		status := state.Status
		stats := state.Stats
		drainReason := state.drainReason
		state.mu.Unlock()
		if status == "done" && drainReason != "" {
			resumed := newScanState(task)
			resumed.bindSender(sender)
			m.tasks[identity] = resumed
			m.mu.Unlock()
			return proto.TaskAck{
				TaskID: task.TaskID, Accepted: true,
				Reason: "resumed", Total: -1,
			}, m.startScan(resumed)
		}
		m.mu.Unlock()
		if status == "done" {
			return proto.TaskAck{
				TaskID: task.TaskID, Accepted: true,
				Reason: "already_done", Total: stats.Total, Stats: &stats,
			}, nil
		}
		return proto.TaskAck{
			TaskID: task.TaskID, Accepted: true,
			Reason: "resumed", Total: state.total.Load(),
		}, m.startScan(state)
	}

	state := newScanState(task)
	state.bindSender(sender)
	m.tasks[identity] = state
	m.mu.Unlock()
	return proto.TaskAck{
		TaskID: task.TaskID, Accepted: true, Reason: "accepted", Total: -1,
	}, m.startScan(state)
}

func newScanState(task proto.ScanTask) *ScanState {
	return &ScanState{
		Task: task, Status: "running", speedWin: newSpeedWindow(10 * time.Second),
	}
}

func (m *ScanManager) startScan(state *ScanState) func() {
	return func() {
		state.start.Do(func() {
			go func() {
				// M1 serializes scans so two tasks cannot hash the same pending
				// SQLite snapshot concurrently. M2 replaces this with SHA
				// single-flight.
				m.runMu.Lock()
				defer m.runMu.Unlock()
				m.run(state)
			}()
		})
	}
}

func sameScanEnvelope(left, right proto.ScanTask) bool {
	if left.TaskID != right.TaskID || left.InstanceID != right.InstanceID || left.Phase != right.Phase ||
		left.Options.Rescan != right.Options.Rescan ||
		len(left.Roots) != len(right.Roots) ||
		len(left.Options.Extensions) != len(right.Options.Extensions) {
		return false
	}
	for index := range left.Roots {
		if left.Roots[index] != right.Roots[index] {
			return false
		}
	}
	for index := range left.Options.Extensions {
		if left.Options.Extensions[index] != right.Options.Extensions[index] {
			return false
		}
	}
	return true
}

func rejectedAck(taskID, reason string) proto.TaskAck {
	return proto.TaskAck{
		TaskID: taskID, Accepted: false,
		Reason: "rejected:" + reason, Total: -1,
	}
}

func (state *ScanState) send(msgType uint8, value any) {
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
			state.binding++
		}
		state.mu.Unlock()
	}
}

func (state *ScanState) bindSender(sender Sender) {
	state.mu.Lock()
	state.sender = sender
	state.binding++
	state.mu.Unlock()
}

func (state *ScanState) ensureControl() {
	state.control.Do(func() {
		state.workCtx, state.abortWork = context.WithCancel(context.Background())
		state.dispatchCtx, state.stopDispatch = context.WithCancel(context.Background())
		state.stopNew = make(chan struct{})
	})
}

func (state *ScanState) stopRequested() bool {
	state.ensureControl()
	select {
	case <-state.stopNew:
		return true
	default:
		return false
	}
}

func scanDrainPriority(reason proto.TaskDrainReason) int {
	switch reason {
	case proto.TaskDrainDelete:
		return 4
	case proto.TaskDrainStop:
		return 3
	case proto.TaskDrainPause:
		return 2
	case proto.TaskDrainProcessShutdown:
		return 1
	default:
		return 0
	}
}

// Drain preserves the task-ID-only legacy scan contract. Instance-aware local
// tasks must use DrainInstance so an old cached instance cannot control a new
// task that reused the same task ID.
func (m *ScanManager) Drain(taskID string, reason proto.TaskDrainReason) (bool, *proto.TaskStats) {
	return m.DrainInstance(taskID, "", reason)
}

func (m *ScanManager) DrainInstance(taskID, instanceID string, reason proto.TaskDrainReason) (bool, *proto.TaskStats) {
	if taskID == "" || scanDrainPriority(reason) == 0 {
		return false, nil
	}
	m.mu.Lock()
	state := m.tasks[scanIdentity{taskID: taskID, instanceID: instanceID}]
	m.mu.Unlock()
	if state == nil {
		return false, nil
	}
	state.ensureControl()
	state.mu.Lock()
	if state.Status != "done" && scanDrainPriority(reason) > scanDrainPriority(state.drainReason) {
		state.drainReason = reason
	}
	stats := state.Stats
	if state.Status != "done" {
		stats.Total = state.total.Load()
		stats.Done = state.done.Load()
		stats.Failed = state.failed.Load()
		stats.ScanErrors = state.scanErrors.Load()
		if !state.startedAt.IsZero() {
			stats.ElapsedMS = time.Since(state.startedAt).Milliseconds()
		}
	}
	state.mu.Unlock()
	state.stopOnce.Do(func() { close(state.stopNew) })
	state.stopDispatch()
	m.mu.Lock()
	controller := m.ioController
	m.mu.Unlock()
	if controller != nil {
		controller.CancelTask(taskID, instanceID)
	}
	state.dispatchMu.Lock()
	state.dispatchMu.Unlock()
	return true, &stats
}

// Abort preserves the task-ID-only legacy scan contract. See Drain.
func (m *ScanManager) Abort(taskID string) bool {
	return m.AbortInstance(taskID, "")
}

func (m *ScanManager) AbortInstance(taskID, instanceID string) bool {
	m.mu.Lock()
	state := m.tasks[scanIdentity{taskID: taskID, instanceID: instanceID}]
	m.mu.Unlock()
	if state == nil {
		return false
	}
	state.ensureControl()
	state.stopOnce.Do(func() { close(state.stopNew) })
	state.stopDispatch()
	m.mu.Lock()
	controller := m.ioController
	m.mu.Unlock()
	if controller != nil {
		controller.CancelTask(taskID, instanceID)
	}
	state.dispatchMu.Lock()
	state.abortWork()
	state.dispatchMu.Unlock()
	return true
}

func (m *ScanManager) Cancel(taskID string) bool {
	accepted, _ := m.Drain(taskID, proto.TaskDrainStop)
	return accepted
}

func (state *ScanState) publishFeatures(items []proto.FeatureItem) {
	if len(items) == 0 {
		return
	}
	state.featureMu.Lock()
	defer state.featureMu.Unlock()
	state.mu.Lock()
	state.seq++
	sequence := state.seq
	state.mu.Unlock()
	state.send(proto.MsgFeatureResult, &proto.FeatureResult{
		TaskID: state.Task.TaskID,
		Seq:    sequence,
		Items:  items,
	})
}

func (m *ScanManager) run(state *ScanState) {
	started := time.Now()
	state.ensureControl()
	state.mu.Lock()
	state.startedAt = started
	state.mu.Unlock()
	ctx := state.workCtx
	if m.pool != nil {
		state.poolStart = m.pool.Metrics()
	}
	progressDone := make(chan struct{})
	progressExited := make(chan struct{})
	go func() {
		defer close(progressExited)
		m.progressLoop(state, progressDone)
	}()
	finishRun := func(enumerated int64) {
		close(progressDone)
		<-progressExited
		m.finish(state, started, enumerated)
	}
	var enumerated int64
	seen := make(map[string]diskio.Identity)
	drained := false

	for _, root := range state.Task.Roots {
		if state.stopRequested() {
			drained = true
			break
		}
		identity, err := m.resolver(root)
		if err != nil {
			state.failed.Add(1)
			state.scanErrors.Add(1)
			m.reportErr(state, root, "enum", err)
			continue
		}
		diskNo := diskNumber(identity)
		m.mu.Lock()
		m.disks[diskNo] = identity
		controllerConfigured := m.ioController != nil
		if m.ioIdentities != nil {
			m.ioIdentities[identity.Key] = identity
		}
		m.mu.Unlock()
		if controllerConfigured {
			initial := m.cfg.IO.HDDInitial
			if identity.KnownSSD && identity.SSD {
				initial = m.cfg.IO.SSDInitial
			}
			m.log.Info(
				"disk I/O configured",
				"disk_key", identity.Key,
				"initial_concurrency", initial,
				"max_concurrency", max(1, min(m.cfg.IO.MaxPerDisk, m.cfg.Worker.Count)),
			)
		}

		buffer := make([]store.EnumUpsert, 0, 10_000)
		flush := func() error {
			if len(buffer) == 0 {
				return nil
			}
			if err := m.st.UpsertEnumerated(ctx, buffer); err != nil {
				return err
			}
			for _, record := range buffer {
				seen[pathKey(record.Path)] = identity
			}
			buffer = buffer[:0]
			return nil
		}
		err = m.enumr.Enum(root, func(record fileenum.FileRecord) error {
			if state.stopRequested() {
				return errScanDrainRequested
			}
			if len(state.Task.Options.Extensions) > 0 &&
				!extIn(record.Path, state.Task.Options.Extensions) {
				return nil
			}
			enumerated++
			state.total.Store(enumerated)
			buffer = append(buffer, store.EnumUpsert{
				MachineID: m.cfg.MachineID,
				DiskNo:    diskNo,
				Path:      record.Path,
				Size:      record.Size,
				MTime:     record.MTime,
				MissingBase: MissingBaseWithExtensions(
					record.Path,
					m.cfg.Scan.ImageExts,
					m.cfg.Scan.VideoExts,
				),
				Force: state.Task.Options.Rescan,
			})
			if len(buffer) == cap(buffer) {
				return flush()
			}
			return nil
		})
		flushErr := flush()
		if errors.Is(err, errScanDrainRequested) || state.stopRequested() {
			drained = true
			if flushErr != nil {
				state.failed.Add(1)
				state.scanErrors.Add(1)
				m.reportErr(state, root, "enum", flushErr)
			}
			break
		}
		if err == nil {
			err = flushErr
		}
		if err != nil {
			state.failed.Add(1)
			state.scanErrors.Add(1)
			m.reportErr(state, root, "enum", err)
		}
	}
	if drained {
		state.send(proto.MsgTaskProgress, m.scanProgress(state))
		finishRun(enumerated)
		return
	}
	state.enumerationComplete.Store(true)

	pending, err := m.pendingSnapshot(ctx, m.cfg.MachineID)
	if err != nil {
		state.failed.Add(1)
		state.scanErrors.Add(1)
		m.reportErr(state, "", "enum", err)
		finishRun(enumerated)
		return
	}
	pending = filterPendingSeen(pending, seen)
	pendingWork := m.pendingWorkCount(pending)
	work, _ := m.preparePending(state, pending, seen)
	cached := enumerated - pendingWork
	if cached < 0 {
		cached = 0
	}
	state.total.Store(enumerated)
	state.done.Store(cached)
	state.send(proto.MsgTaskProgress, m.scanProgress(state))

	hashResults := make(chan store.HashResult, 1024)
	mediaResults := make(chan proto.FeatureItem, 1024)
	var disks sync.WaitGroup
	for _, group := range groupScanWork(work) {
		streams := m.cfg.Scan.HDDStreams
		if group.identity.KnownSSD && group.identity.SSD {
			streams = m.cfg.Scan.SSDStreams
		}
		disks.Add(1)
		go func(group scanDiskWork, streams int) {
			defer disks.Done()
			m.processDisk(
				state,
				diskNumber(group.identity),
				group.identity,
				group.files,
				streams,
				hashResults,
				mediaResults,
			)
		}(group, streams)
	}

	hashWriterDone := make(chan struct{})
	go m.resultWriter(state, hashResults, hashWriterDone)
	mediaWriterDone := make(chan struct{})
	go m.mediaResultWriter(state, mediaResults, mediaWriterDone)
	disks.Wait()
	close(hashResults)
	close(mediaResults)
	<-hashWriterDone
	<-mediaWriterDone
	finishRun(enumerated)
}

type scanWork struct {
	file        store.PendingFile
	identity    diskio.Identity
	media       *worker.JobMsg
	terminal    <-chan poolTerminal
	cancelRoute func()
	prepareErr  error
}

type scanDiskWork struct {
	identity diskio.Identity
	files    []scanWork
}

type queuedScanWork struct {
	order int
	work  scanWork
}

type mediaRoute struct {
	job    *worker.JobMsg
	cancel func()
}

type pendingWorkSpec struct {
	media bool
	kind  worker.MediaKind
	mask  uint32
}

func (m *ScanManager) pendingSpec(file store.PendingFile) (pendingWorkSpec, bool) {
	kind := MediaKindWithExtensions(
		file.Path,
		m.cfg.Scan.ImageExts,
		m.cfg.Scan.VideoExts,
	)
	if kind == "other" {
		return pendingWorkSpec{}, file.MissingMask&proto.FieldSHA512 != 0
	}
	spec := pendingWorkSpec{media: true, kind: worker.MediaImage}
	if kind == "image" {
		spec.mask = file.MissingMask & store.RequiredStageOneMask(store.MediaImage)
	} else {
		spec.kind = worker.MediaVideo
		spec.mask = file.MissingMask & store.RequiredStageOneMask(store.MediaVideo)
	}
	return spec, spec.mask != 0
}

func (m *ScanManager) pendingWorkCount(pending map[int64][]store.PendingFile) int64 {
	var count int64
	for _, files := range pending {
		for _, file := range files {
			if _, ok := m.pendingSpec(file); ok {
				count++
			}
		}
	}
	return count
}

func (m *ScanManager) preparePending(
	state *ScanState,
	pending map[int64][]store.PendingFile,
	identitiesByPath ...map[string]diskio.Identity,
) (map[int64][]scanWork, map[int64]mediaRoute) {
	state.ensureControl()
	state.dispatchMu.Lock()
	defer state.dispatchMu.Unlock()
	work := make(map[int64][]scanWork, len(pending))
	terminals := make(map[int64]mediaRoute)
	if state.stopRequested() {
		return work, terminals
	}
	router := m.ensurePoolRouter()
	for diskNo, files := range pending {
		for _, file := range files {
			identity := m.diskIdentity(diskNo)
			if len(identitiesByPath) != 0 {
				if exact, ok := identitiesByPath[0][pathKey(file.Path)]; ok {
					identity = exact
				}
			}
			spec, needed := m.pendingSpec(file)
			if !needed {
				continue
			}
			if !spec.media {
				work[diskNo] = append(work[diskNo], scanWork{file: file, identity: identity})
				continue
			}
			current := scanWork{file: file, identity: identity}
			if m.pool == nil || router == nil {
				current.prepareErr = fmt.Errorf("worker pool is unavailable")
				work[diskNo] = append(work[diskNo], current)
				continue
			}
			var knownSHA []byte
			if file.SHA512 != nil {
				if len(*file.SHA512) != 128 ||
					*file.SHA512 != strings.ToLower(*file.SHA512) {
					current.prepareErr = fmt.Errorf(
						"persisted SHA-512 must be 128 lowercase hex characters",
					)
					work[diskNo] = append(work[diskNo], current)
					continue
				}
				decoded, err := hex.DecodeString(*file.SHA512)
				if err != nil || len(decoded) != 64 {
					current.prepareErr = fmt.Errorf(
						"persisted SHA-512 must be 128 lowercase hex characters",
					)
					work[diskNo] = append(work[diskNo], current)
					continue
				}
				knownSHA = decoded
			}
			jobID := router.NextJobID()
			current.media = &worker.JobMsg{
				JobID: jobID, ScanTaskID: state.Task.TaskID,
				ScanInstanceID: state.Task.InstanceID,
				DiskKey:        string(identity.Key),
				Path:           file.Path, Kind: spec.kind, Phase: worker.Phase1,
				FieldsMask: spec.mask, Size: file.Size, MTimeUnix: file.MTime,
				KnownSHA: knownSHA,
			}
			work[diskNo] = append(work[diskNo], current)
		}
	}
	return work, terminals
}

func (m *ScanManager) ensurePoolRouter() *PoolRouter {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.router == nil && m.pool != nil {
		m.router = NewPoolRouter(m.pool, m.log)
	}
	return m.router
}

func filterPendingSeen(
	pending map[int64][]store.PendingFile,
	seen map[string]diskio.Identity,
) map[int64][]store.PendingFile {
	filtered := make(map[int64][]store.PendingFile, len(pending))
	for diskNo, files := range pending {
		for _, file := range files {
			if _, ok := seen[pathKey(file.Path)]; ok {
				filtered[diskNo] = append(filtered[diskNo], file)
			}
		}
	}
	return filtered
}

func groupScanWork(work map[int64][]scanWork) map[diskio.DiskKey]scanDiskWork {
	groups := make(map[diskio.DiskKey]scanDiskWork)
	for _, files := range work {
		for _, item := range files {
			key := item.identity.Key
			group := groups[key]
			group.identity = item.identity
			group.files = append(group.files, item)
			groups[key] = group
		}
	}
	return groups
}

func pathKey(path string) string {
	return strings.ToLower(strings.TrimPrefix(path, `\\?\`))
}

func (m *ScanManager) isSSD(diskNo int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	identity := m.disks[diskNo]
	return identity.KnownSSD && identity.SSD
}

func (m *ScanManager) diskIdentity(diskNo int64) diskio.Identity {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.disks[diskNo]
}

func (m *ScanManager) processDisk(
	state *ScanState,
	diskNo int64,
	identity diskio.Identity,
	files []scanWork,
	streams int,
	hashOut chan<- store.HashResult,
	mediaOut chan<- proto.FeatureItem,
) {
	if identity.KnownSSD && identity.SSD {
		m.processDiskBatch(state, diskNo, files, streams, hashOut, mediaOut)
		return
	}

	images := make([]scanWork, 0, len(files))
	other := make([]scanWork, 0, len(files))
	videos := make([]scanWork, 0, len(files))
	for _, work := range files {
		switch {
		case work.media != nil && work.media.Kind == worker.MediaImage:
			images = append(images, work)
		case work.media != nil && work.media.Kind == worker.MediaVideo:
			videos = append(videos, work)
		default:
			other = append(other, work)
		}
	}
	sort.SliceStable(images, func(left, right int) bool {
		if images[left].file.Size != images[right].file.Size {
			return images[left].file.Size < images[right].file.Size
		}
		return pathKey(images[left].file.Path) < pathKey(images[right].file.Path)
	})

	m.processDiskBatch(state, diskNo, images, streams, hashOut, mediaOut)
	if state.stopRequested() {
		cancelScanWorkBatch(other)
		cancelScanWorkBatch(videos)
		return
	}
	m.processDiskBatch(state, diskNo, other, streams, hashOut, mediaOut)
	if state.stopRequested() {
		cancelScanWorkBatch(videos)
		return
	}
	m.processDiskBatch(state, diskNo, videos, streams, hashOut, mediaOut)
}

func (m *ScanManager) processDiskBatch(
	state *ScanState,
	diskNo int64,
	files []scanWork,
	streams int,
	hashOut chan<- store.HashResult,
	mediaOut chan<- proto.FeatureItem,
) {
	if len(files) == 0 {
		return
	}
	jobs := make(chan queuedScanWork, pendingJobCapacity(
		m.cfg.Worker.Count,
		m.cfg.IO.MaxQueuedPerWorker,
	))
	var orderMu sync.Mutex
	orderCond := sync.NewCond(&orderMu)
	nextOrder := 0
	var workers sync.WaitGroup
	for index := 0; index < streams; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for queued := range jobs {
				work := queued.work
				orderMu.Lock()
				for queued.order != nextOrder {
					orderCond.Wait()
				}
				orderMu.Unlock()
				var releaseOrder sync.Once
				advanceOrder := func() {
					releaseOrder.Do(func() {
						orderMu.Lock()
						nextOrder++
						orderCond.Broadcast()
						orderMu.Unlock()
					})
				}
				err := runObservedWork(
					state.dispatchCtx,
					m.limiter,
					m.currentObserver(),
					diskNo,
					work.file.Size,
					func() (time.Duration, time.Duration) {
						state.dispatchMu.Lock()
						if state.stopRequested() {
							state.dispatchMu.Unlock()
							cancelScanWork(work)
							advanceOrder()
							return 0, 0
						}
						var submitErr error
						if work.media != nil && work.prepareErr == nil {
							terminal, cancelRoute, registerErr := m.ensurePoolRouter().Register(work.media)
							if registerErr != nil {
								submitErr = registerErr
							} else {
								work.terminal = terminal
								work.cancelRoute = cancelRoute
								submitErr = submitScanJob(state.dispatchCtx, m.pool, work.media)
							}
						}
						state.dispatchMu.Unlock()
						advanceOrder()
						if submitErr != nil && state.stopRequested() {
							cancelScanWork(work)
							return 0, 0
						}
						if work.media != nil || work.prepareErr != nil {
							return m.processMediaWork(state, work, submitErr, mediaOut)
						}
						started := time.Now()
						file := work.file
						result := store.HashResult{
							Path:  file.Path,
							Size:  file.Size,
							MTime: file.MTime,
						}
						hash, hashErr := m.hasher.HashFile(file.Path)
						if hashErr != nil {
							result.Err = hashErr.Error()
							state.failed.Add(1)
							m.reportErr(state, file.Path, "hash", hashErr)
						} else {
							result.SHA512 = hash
						}
						hashOut <- result
						return time.Since(started), 0
					},
				)
				advanceOrder()
				if err != nil {
					cancelScanWork(work)
					if state.stopRequested() {
						continue
					}
					state.failed.Add(1)
					m.reportErr(state, work.file.Path, "backpressure", err)
				}
			}
		}()
	}
	for index, file := range files {
		select {
		case <-state.stopNew:
			for _, remaining := range files[index:] {
				cancelScanWork(remaining)
			}
			close(jobs)
			workers.Wait()
			return
		default:
		}
		select {
		case jobs <- queuedScanWork{order: index, work: file}:
			if len(jobs) == cap(jobs) {
				m.notifyPendingJobsFull()
			}
		case <-state.stopNew:
			for _, remaining := range files[index:] {
				cancelScanWork(remaining)
			}
			close(jobs)
			workers.Wait()
			return
		}
	}
	close(jobs)
	workers.Wait()
}

func (m *ScanManager) notifyPendingJobsFull() {
	m.mu.Lock()
	notify := m.pendingJobsFull
	m.mu.Unlock()
	if notify != nil {
		notify()
	}
}

func cancelScanWorkBatch(work []scanWork) {
	for _, item := range work {
		cancelScanWork(item)
	}
}

func cancelScanWork(work scanWork) {
	if work.cancelRoute != nil {
		work.cancelRoute()
	}
}

func (m *ScanManager) processMediaWork(
	state *ScanState,
	work scanWork,
	submitErr error,
	out chan<- proto.FeatureItem,
) (time.Duration, time.Duration) {
	if work.prepareErr != nil {
		state.failed.Add(1)
		m.reportErr(state, work.file.Path, "worker", work.prepareErr)
		out <- proto.FeatureItem{
			Path:   work.file.Path,
			Size:   work.file.Size,
			MTime:  work.file.MTime,
			Status: proto.StatusFailed,
			Err:    work.prepareErr.Error(),
			FieldErrors: []proto.FieldError{{
				Field: work.file.MissingMask,
				Stage: "worker",
				Msg:   work.prepareErr.Error(),
			}},
		}
		return 0, 0
	}
	if submitErr != nil {
		cancelScanWork(work)
		state.failed.Add(1)
		m.reportErr(state, work.file.Path, "worker", submitErr)
		out <- proto.FeatureItem{
			Path:   work.file.Path,
			Size:   work.file.Size,
			MTime:  work.file.MTime,
			Status: proto.StatusFailed,
			Err:    submitErr.Error(),
			FieldErrors: []proto.FieldError{{
				Field: work.media.FieldsMask,
				Stage: "worker",
				Msg:   submitErr.Error(),
			}},
		}
		return 0, 0
	}
	var terminal poolTerminal
	select {
	case terminal = <-work.terminal:
	case <-state.workCtx.Done():
		work.cancelRoute()
		return 0, 0
	}
	work.cancelRoute()
	item := proto.FeatureItem{
		Path:  work.file.Path,
		Size:  work.file.Size,
		MTime: work.file.MTime,
	}
	if terminal.err != nil {
		item.Status = proto.StatusFailed
		item.Err = terminal.err.Error()
		item.FieldErrors = []proto.FieldError{{
			Field: work.media.FieldsMask,
			Stage: "worker",
			Msg:   terminal.err.Error(),
		}}
		state.failed.Add(1)
	} else if terminal.crash != nil {
		item.Status = proto.StatusCrash
		item.Err = terminal.crash.Reason
		item.FieldErrors = []proto.FieldError{{
			Field: work.media.FieldsMask,
			Stage: "worker",
			Msg:   terminal.crash.Reason,
		}}
		state.failed.Add(1)
		state.send(proto.MsgCrashNotice, &proto.CrashNotice{
			TaskID:   state.Task.TaskID,
			PID:      terminal.crash.PID,
			Path:     terminal.crash.File,
			ExitCode: int(terminal.crash.ExitCode),
		})
	} else {
		item = featureItemFromWorker(work.media, terminal.result)
		if item.Status != proto.StatusDone {
			state.failed.Add(1)
		}
	}
	out <- item
	if terminal.result == nil {
		return 0, 0
	}
	return time.Duration(terminal.result.ReadNS), time.Duration(terminal.result.DecodeNS)
}

func featureItemFromWorker(
	job *worker.JobMsg,
	result *worker.JobResultMsg,
) proto.FeatureItem {
	item := proto.FeatureItem{
		Path:  job.Path,
		Size:  job.Size,
		MTime: job.MTimeUnix,
	}
	if result == nil {
		item.Status = proto.StatusFailed
		item.Err = "worker returned no result"
		return item
	}
	item.FieldsDone = result.FieldsDone & job.FieldsMask
	if len(result.SHA512) == 64 {
		item.SHA512 = hex.EncodeToString(result.SHA512)
	} else if len(job.KnownSHA) == 64 {
		item.SHA512 = hex.EncodeToString(job.KnownSHA)
	}
	if len(result.PDQ) != 0 {
		item.PDQ256 = hex.EncodeToString(result.PDQ)
	}
	item.Quality = result.Quality
	item.Width = result.Width
	item.Height = result.Height
	if job.Kind == worker.MediaVideo {
		item.Width = result.ContactSheetWidth
		item.Height = result.ContactSheetHeight
	}
	item.DurationMS = cloneInt64(result.DurationMS)
	item.ThumbPath = result.ThumbPath
	if len(result.ThumbPDQ) != 0 {
		item.ThumbPDQ256 = hex.EncodeToString(result.ThumbPDQ)
	}
	item.ThumbQuality = cloneInt32(result.ThumbQuality)
	for _, fieldError := range result.Errors {
		item.FieldErrors = append(item.FieldErrors, proto.FieldError{
			Field: fieldError.Field,
			Stage: fieldError.Stage,
			Msg:   fieldError.Msg,
		})
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
	case item.FieldsDone != 0 || videoPartialPayload(job, result):
		item.Status = proto.StatusPartial
	default:
		item.Status = proto.StatusFailed
	}
	return item
}

func videoPartialPayload(job *worker.JobMsg, result *worker.JobResultMsg) bool {
	if job.Kind != worker.MediaVideo || result == nil {
		return false
	}
	return result.DurationMS != nil ||
		result.ThumbPath != "" ||
		len(result.ThumbPDQ) != 0 ||
		result.ThumbQuality != nil ||
		result.ContactSheetWidth != 0 ||
		result.ContactSheetHeight != 0
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneInt32(value *int32) *int32 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func (m *ScanManager) resultWriter(
	state *ScanState,
	input <-chan store.HashResult,
	done chan<- struct{},
) {
	defer close(done)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	buffer := make([]store.HashResult, 0, 512)
	flush := func() {
		if len(buffer) == 0 {
			return
		}
		if err := m.st.ApplyHashResults(
			state.workCtx,
			m.cfg.MachineID,
			buffer,
		); err != nil {
			m.reportErr(state, "", "store", err)
			state.scanErrors.Add(1)
			persistError := fmt.Sprintf("store: result batch not committed: %v", err)
			for index := range buffer {
				if buffer[index].Err == "" {
					state.failed.Add(1)
				}
				buffer[index].SHA512 = ""
				buffer[index].Err = persistError
			}
		}
		items := make([]proto.FeatureItem, len(buffer))
		for index, result := range buffer {
			status := proto.StatusDone
			if result.Err != "" {
				status = proto.StatusFailed
			}
			items[index] = proto.FeatureItem{
				Path:   result.Path,
				SHA512: result.SHA512,
				Size:   result.Size,
				MTime:  result.MTime,
				Status: status,
				Err:    result.Err,
			}
		}
		state.publishFeatures(items)
		state.done.Add(int64(len(buffer)))
		state.speedWin.Add(int64(len(buffer)))
		buffer = buffer[:0]
	}
	for {
		select {
		case result, open := <-input:
			if !open {
				flush()
				return
			}
			buffer = append(buffer, result)
			if len(buffer) == cap(buffer) {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (m *ScanManager) mediaResultWriter(
	state *ScanState,
	input <-chan proto.FeatureItem,
	done chan<- struct{},
) {
	defer close(done)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	buffer := make([]proto.FeatureItem, 0, 512)
	flush := func() {
		if len(buffer) == 0 {
			return
		}
		items := append([]proto.FeatureItem(nil), buffer...)
		state.publishFeatures(items)
		state.done.Add(int64(len(buffer)))
		state.speedWin.Add(int64(len(buffer)))
		buffer = buffer[:0]
	}
	for {
		select {
		case result, open := <-input:
			if !open {
				flush()
				return
			}
			buffer = append(buffer, result)
			if len(buffer) == cap(buffer) {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (m *ScanManager) progressLoop(state *ScanState, done <-chan struct{}) {
	ticks, stop := m.progressTicks()
	defer stop()
	for {
		select {
		case <-done:
			return
		case <-ticks:
			state.send(proto.MsgTaskProgress, m.scanProgress(state))
		}
	}
}

func (m *ScanManager) scanProgress(state *ScanState) *proto.TaskProgress {
	state.mu.Lock()
	startedAt := state.startedAt
	state.mu.Unlock()
	elapsedMS := int64(0)
	if !startedAt.IsZero() {
		elapsedMS = time.Since(startedAt).Milliseconds()
	}
	done := state.done.Load()
	if !state.enumerationComplete.Load() {
		done = 0
	}
	return &proto.TaskProgress{
		TaskID:     state.Task.TaskID,
		Done:       done,
		Total:      state.total.Load(),
		TotalKnown: state.enumerationComplete.Load(),
		Speed:      state.speedWin.Rate(),
		Failed:     state.failed.Load(),
		ElapsedMS:  elapsedMS,
	}
}

func (m *ScanManager) finish(
	state *ScanState,
	started time.Time,
	enumerated int64,
) {
	poolDelta := worker.MetricsSnapshot{}
	if m.pool != nil {
		poolDelta = subtractMetrics(m.pool.Metrics(), state.poolStart)
	}
	state.mu.Lock()
	reason := state.drainReason
	total := state.total.Load()
	if enumerated > total {
		total = enumerated
	}
	done := state.done.Load()
	skipped := int64(0)
	if reason == "" && done < total {
		skipped = total - done
		done = total
		state.done.Store(done)
	}
	state.Status = "done"
	avgReadMS, avgDecodeMS := metricAveragesMS(poolDelta)
	state.Stats = proto.TaskStats{
		Total:            total,
		Done:             done,
		Skipped:          skipped,
		Failed:           state.failed.Load(),
		ScanErrors:       state.scanErrors.Load(),
		ElapsedMS:        time.Since(started).Milliseconds(),
		FilesDone:        poolDelta.FilesDone,
		FilesFailed:      poolDelta.FilesFailed,
		DecodeCalls:      poolDelta.DecodeCalls,
		ReadAttempts:     poolDelta.ReadAttempts,
		DecodeAttempts:   poolDelta.DecodeAttempts,
		AvgReadMS:        avgReadMS,
		AvgDecodeMS:      avgDecodeMS,
		ThumbGenerated:   poolDelta.ThumbGenerated,
		ThumbCacheHits:   poolDelta.ThumbCacheHits,
		SingleFlightHits: poolDelta.SingleFlightHits,
		Crashes:          poolDelta.Crashes,
	}
	stats := state.Stats
	state.mu.Unlock()
	m.log.Info(
		"scan done",
		"task_id", state.Task.TaskID,
		"files_done", stats.FilesDone,
		"files_failed", stats.FilesFailed,
		"decode_calls", stats.DecodeCalls,
		"read_attempts", stats.ReadAttempts,
		"decode_attempts", stats.DecodeAttempts,
		"avg_read_ms", stats.AvgReadMS,
		"avg_decode_ms", stats.AvgDecodeMS,
		"thumb_generated", stats.ThumbGenerated,
		"thumb_cache_hits", stats.ThumbCacheHits,
		"singleflight_hits", stats.SingleFlightHits,
		"crashes", stats.Crashes,
		"elapsed_ms", stats.ElapsedMS,
	)
	state.send(proto.MsgTaskProgress, m.scanProgress(state))
	state.send(proto.MsgTaskDone, &proto.TaskDone{
		TaskID: state.Task.TaskID,
		Stats:  stats,
		Reason: reason,
	})
	if taskPool, ok := m.pool.(interface{ EndTask(string) }); ok {
		taskPool.EndTask(state.Task.TaskID)
	}
	time.AfterFunc(10*time.Minute, func() {
		m.mu.Lock()
		identity := scanTaskIdentity(state.Task)
		if m.tasks[identity] == state {
			delete(m.tasks, identity)
		}
		m.mu.Unlock()
	})
}

func metricAveragesMS(metrics worker.MetricsSnapshot) (float64, float64) {
	var avgReadMS, avgDecodeMS float64
	if metrics.ReadAttempts != 0 {
		avgReadMS = float64(metrics.ReadNS) / float64(metrics.ReadAttempts) / float64(time.Millisecond)
	}
	if metrics.DecodeAttempts != 0 {
		avgDecodeMS = float64(metrics.DecodeNS) / float64(metrics.DecodeAttempts) / float64(time.Millisecond)
	}
	return avgReadMS, avgDecodeMS
}

func subtractMetrics(
	current worker.MetricsSnapshot,
	start worker.MetricsSnapshot,
) worker.MetricsSnapshot {
	nonnegative := func(value int64) int64 {
		if value < 0 {
			return 0
		}
		return value
	}
	return worker.MetricsSnapshot{
		FilesDone:   nonnegative(current.FilesDone - start.FilesDone),
		FilesFailed: nonnegative(current.FilesFailed - start.FilesFailed),
		DecodeCalls: nonnegative(current.DecodeCalls - start.DecodeCalls),
		ReadAttempts: nonnegative(
			current.ReadAttempts - start.ReadAttempts,
		),
		DecodeAttempts: nonnegative(
			current.DecodeAttempts - start.DecodeAttempts,
		),
		ReadNS:   nonnegative(current.ReadNS - start.ReadNS),
		DecodeNS: nonnegative(current.DecodeNS - start.DecodeNS),
		ThumbGenerated: nonnegative(
			current.ThumbGenerated - start.ThumbGenerated,
		),
		ThumbCacheHits: nonnegative(
			current.ThumbCacheHits - start.ThumbCacheHits,
		),
		SingleFlightHits: nonnegative(
			current.SingleFlightHits - start.SingleFlightHits,
		),
		Crashes: nonnegative(current.Crashes - start.Crashes),
	}
}

func (m *ScanManager) reportErr(
	state *ScanState,
	path string,
	stage string,
	err error,
) {
	m.errLog.Error(
		"file error",
		"path_id", worker.PathID(path),
		"stage", stage,
		"screen_stage", worker.ScreenStageLegacy,
		"source", worker.JobSourceScan,
		"err", worker.RedactKnownPath(err.Error(), path),
	)
	state.send(proto.MsgError, &proto.Error{
		TaskID: state.Task.TaskID,
		Path:   path,
		Stage:  stage,
		Msg:    err.Error(),
	})
}

func extIn(path string, extensions []string) bool {
	extension := filepath.Ext(path)
	for _, candidate := range extensions {
		if strings.EqualFold(extension, candidate) {
			return true
		}
	}
	return false
}

type speedWindow struct {
	mu     sync.Mutex
	window time.Duration
	events []time.Time
}

func newSpeedWindow(window time.Duration) *speedWindow {
	return &speedWindow{window: window}
}

func (s *speedWindow) Add(count int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for index := int64(0); index < count; index++ {
		s.events = append(s.events, now)
	}
	s.compact(now)
}

func (s *speedWindow) Rate() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.compact(time.Now())
	return float64(len(s.events)) / s.window.Seconds()
}

func (s *speedWindow) compact(now time.Time) {
	cutoff := now.Add(-s.window)
	first := 0
	for first < len(s.events) && !s.events[first].After(cutoff) {
		first++
	}
	if first > 0 {
		s.events = append(s.events[:0], s.events[first:]...)
	}
}
