package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"dedup/internal/store"
)

const (
	defaultReadyTimeout    = 10 * time.Second
	defaultImageTimeout    = 30 * time.Second
	defaultVideoTimeout    = 120 * time.Second
	defaultRespawnDelay    = 500 * time.Millisecond
	defaultShutdownTimeout = 3 * time.Second
	defaultExitGrace       = 25 * time.Millisecond
)

var ErrPoolClosed = errors.New("worker pool is closed")

type Config struct {
	WorkerExe        string
	WorkerCount      int
	MachineID        string
	ImageTimeout     time.Duration
	VideoTimeout     time.Duration
	ReadyTimeout     time.Duration
	RespawnDelay     time.Duration
	ShutdownTimeout  time.Duration
	ExitGrace        time.Duration
	WorkerEnv        []string
	IPCMaxFrameBytes int
}

type FeatureStore interface {
	LookupContent(context.Context, []byte, store.MediaKind, uint32, uint8) (store.ContentState, error)
	SaveAnalysis(context.Context, store.AnalysisResult) (store.CommittedState, error)
	MarkCrash(context.Context, string, string, string) error
}

type MetricsSnapshot struct {
	FilesDone        int64
	FilesFailed      int64
	DecodeCalls      int64
	ReadAttempts     int64
	DecodeAttempts   int64
	ReadNS           int64
	DecodeNS         int64
	ThumbGenerated   int64
	ThumbCacheHits   int64
	SingleFlightHits int64
	Crashes          int64
	ReadyWorkers     int64
}

// RuntimeSnapshot is a bounded, read-only view of the worker runtime. It owns
// its Workers slice and never exposes worker messages or input media paths.
type RuntimeSnapshot struct {
	Expected         int
	Ready            int
	LastErrorSummary string
	Workers          []RuntimeWorkerStatus
}

type RuntimeWorkerStatus struct {
	Index              int
	PID                int
	Ready              bool
	CurrentTaskSummary string
	LastErrorSummary   string
}

type poolMetrics struct {
	filesDone        atomic.Int64
	filesFailed      atomic.Int64
	decodeCalls      atomic.Int64
	readAttempts     atomic.Int64
	decodeAttempts   atomic.Int64
	readNS           atomic.Int64
	decodeNS         atomic.Int64
	thumbGenerated   atomic.Int64
	thumbCacheHits   atomic.Int64
	singleFlightHits atomic.Int64
	crashes          atomic.Int64
	readyWorkers     atomic.Int64
}

type CrashRecord struct {
	Timestamp   time.Time
	JobID       int64
	ScanTaskID  string
	PID         int
	WorkerIndex int
	File        string
	ExitCode    int32
	Reason      string
}

type Pool struct {
	cfg    Config
	store  FeatureStore
	deps   supervisorDeps
	dedup  *Deduper
	ctx    context.Context
	cancel context.CancelFunc

	jobs         chan *JobMsg
	results      chan *JobResultMsg
	crashes      chan CrashRecord
	free         chan *workerProc
	freeMu       sync.Mutex
	quit         chan struct{}
	closing      atomic.Bool
	started      atomic.Bool
	once         sync.Once
	wg           sync.WaitGroup
	metrics      poolMetrics
	beforeSubmit func()
	beforeClose  func()
	submitMu     sync.Mutex
	submitCond   *sync.Cond

	activeMu sync.Mutex
	active   map[int]*workerProc

	runtimeMu     sync.RWMutex
	runtimeErrors map[int]string
}

func NewPool(cfg Config, store FeatureStore, logger, errorLogger, crashLogger *slog.Logger) *Pool {
	deps := defaultSupervisorDeps()
	if logger != nil {
		deps.logger = logger
	}
	if errorLogger != nil {
		deps.errorLogger = errorLogger
	}
	if crashLogger != nil {
		deps.crash = func(record CrashRecord) {
			crashLogger.Error("worker crash",
				"pid", record.PID,
				"worker_index", record.WorkerIndex,
				"path_id", PathID(record.File),
				"exit_code", record.ExitCode,
				"reason", RedactKnownPath(record.Reason, record.File),
			)
		}
	}
	return newPoolWithDeps(cfg, store, deps)
}

func newPoolWithDeps(cfg Config, featureStore FeatureStore, deps supervisorDeps) *Pool {
	applyConfigDefaults(&cfg)
	if deps.logger == nil {
		deps.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if deps.errorLogger == nil {
		deps.errorLogger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if deps.clock == nil {
		deps.clock = realClock{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	originalCrash := deps.crash
	pool := &Pool{
		cfg: cfg, store: featureStore, deps: deps, dedup: NewDeduper(featureStore),
		ctx: ctx, cancel: cancel,
		jobs: make(chan *JobMsg, 1024), results: make(chan *JobResultMsg, 1024),
		crashes: make(chan CrashRecord, 1024),
		free:    make(chan *workerProc, max(1, cfg.WorkerCount)), quit: make(chan struct{}),
		active: make(map[int]*workerProc), runtimeErrors: make(map[int]string),
	}
	pool.deps.crash = func(record CrashRecord) {
		pool.recordRuntimeError(record.WorkerIndex, record.Reason)
		if originalCrash != nil {
			originalCrash(record)
		}
	}
	pool.submitCond = sync.NewCond(&pool.submitMu)
	return pool
}

func applyConfigDefaults(cfg *Config) {
	if cfg.WorkerCount <= 0 {
		cfg.WorkerCount = 1
	}
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = defaultReadyTimeout
	}
	if cfg.ImageTimeout <= 0 {
		cfg.ImageTimeout = defaultImageTimeout
	}
	if cfg.VideoTimeout <= 0 {
		cfg.VideoTimeout = defaultVideoTimeout
	}
	if cfg.RespawnDelay <= 0 {
		cfg.RespawnDelay = defaultRespawnDelay
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = defaultShutdownTimeout
	}
	if cfg.ExitGrace <= 0 {
		cfg.ExitGrace = defaultExitGrace
	}
	if cfg.IPCMaxFrameBytes <= 0 {
		cfg.IPCMaxFrameBytes = MaxFrameBytes
	}
}

func (p *Pool) Start() {
	if !p.started.CompareAndSwap(false, true) {
		return
	}
	for index := 0; index < p.cfg.WorkerCount; index++ {
		p.wg.Add(1)
		go p.supervise(index)
	}
	p.wg.Add(1)
	go p.dispatchLoop()
}

func (p *Pool) Submit(job *JobMsg) error {
	if job == nil {
		return fmt.Errorf("worker pool: nil job")
	}
	p.submitMu.Lock()
	defer p.submitMu.Unlock()
	if p.closing.Load() {
		return ErrPoolClosed
	}
	if p.beforeSubmit != nil {
		p.beforeSubmit()
	}
	for len(p.jobs) == cap(p.jobs) && !p.closing.Load() {
		p.submitCond.Wait()
	}
	if p.closing.Load() {
		return ErrPoolClosed
	}
	p.jobs <- job
	return nil
}

// StopAccepting linearizes the pool's submission boundary without performing
// the final worker/results shutdown. Close remains the sole finalizer.
func (p *Pool) StopAccepting() {
	p.submitMu.Lock()
	p.closing.Store(true)
	p.submitCond.Broadcast()
	p.submitMu.Unlock()
}

func (p *Pool) Results() <-chan *JobResultMsg { return p.results }
func (p *Pool) Crashes() <-chan CrashRecord   { return p.crashes }

func (p *Pool) EndTask(taskID string) {
	p.dedup.EndTask(taskID)
}

func (p *Pool) Metrics() MetricsSnapshot {
	return MetricsSnapshot{
		FilesDone: p.metrics.filesDone.Load(), FilesFailed: p.metrics.filesFailed.Load(),
		DecodeCalls: p.metrics.decodeCalls.Load(), ThumbGenerated: p.metrics.thumbGenerated.Load(),
		ReadAttempts: p.metrics.readAttempts.Load(), DecodeAttempts: p.metrics.decodeAttempts.Load(),
		ReadNS: p.metrics.readNS.Load(), DecodeNS: p.metrics.decodeNS.Load(),
		ThumbCacheHits: p.metrics.thumbCacheHits.Load(), SingleFlightHits: p.metrics.singleFlightHits.Load(),
		Crashes: p.metrics.crashes.Load(), ReadyWorkers: p.metrics.readyWorkers.Load(),
	}
}

func (p *Pool) RuntimeSnapshot() RuntimeSnapshot {
	expected := p.cfg.WorkerCount
	if expected < 0 {
		expected = 0
	}
	workers := make([]RuntimeWorkerStatus, expected)
	active := make([]*workerProc, expected)

	p.activeMu.Lock()
	for index, worker := range p.active {
		if index >= 0 && index < expected {
			active[index] = worker
		}
	}
	p.activeMu.Unlock()

	ready := 0
	unavailable := 0
	lastError := ""
	for index := range workers {
		workers[index].Index = index
		worker := active[index]
		if worker == nil {
			unavailable++
			workers[index].LastErrorSummary = p.runtimeError(index)
			if workers[index].LastErrorSummary == "" {
				workers[index].LastErrorSummary = "worker unavailable; start or respawn pending"
			}
			if lastError == "" && workers[index].LastErrorSummary != "worker unavailable; start or respawn pending" {
				lastError = fmt.Sprintf("worker %d: %s", index, workers[index].LastErrorSummary)
			}
			continue
		}

		worker.mu.Lock()
		workers[index].PID = worker.proc.PID()
		workers[index].Ready = worker.ready && !worker.failureClaimed.Load()
		if worker.current != nil && worker.current.message != nil {
			workers[index].CurrentTaskSummary = runtimeTaskSummary(worker.current.message)
		}
		if worker.failureClaimed.Load() {
			workers[index].LastErrorSummary = p.runtimeError(index)
			if workers[index].LastErrorSummary == "" {
				workers[index].LastErrorSummary = "worker failed; respawn pending"
			}
		}
		worker.mu.Unlock()
		if workers[index].Ready {
			ready++
		} else {
			unavailable++
		}
	}

	snapshot := RuntimeSnapshot{Expected: expected, Ready: ready, Workers: workers}
	if lastError != "" {
		snapshot.LastErrorSummary = lastError
	} else if unavailable != 0 {
		snapshot.LastErrorSummary = fmt.Sprintf("worker slots not ready: %d", unavailable)
	}
	return snapshot
}

func (p *Pool) recordRuntimeError(index int, reason string) {
	if index < 0 || index >= p.cfg.WorkerCount {
		return
	}
	reasonRunes := []rune(reason)
	if len(reasonRunes) > 96 {
		reasonRunes = reasonRunes[:96]
	}
	p.runtimeMu.Lock()
	p.runtimeErrors[index] = string(reasonRunes)
	p.runtimeMu.Unlock()
}

func (p *Pool) runtimeError(index int) string {
	p.runtimeMu.RLock()
	defer p.runtimeMu.RUnlock()
	return p.runtimeErrors[index]
}

func runtimeTaskSummary(job *JobMsg) string {
	if job == nil {
		return ""
	}
	return fmt.Sprintf("phase=%d screen_stage=%d source=%s job_id=%d", job.Phase, job.ScreenStage, job.Source, job.JobID)
}

func (p *Pool) publishCrash(record CrashRecord) bool {
	// Idle process failures remain durable in the crash log and metrics, but
	// they are not scan terminals and must never consume the bounded channel
	// reserved for active jobs.
	if record.JobID == 0 || record.ScanTaskID == "" {
		return false
	}
	if p.closing.Load() {
		return false
	}
	select {
	case p.crashes <- record:
		return true
	case <-p.quit:
		return false
	}
}

func (p *Pool) Close() {
	p.once.Do(func() {
		if p.beforeClose != nil {
			p.beforeClose()
		}
		p.StopAccepting()
		p.submitMu.Lock()
		close(p.quit)
		p.submitMu.Unlock()

		active := p.activeSnapshot()
		timer := p.deps.clock.NewTimer(p.cfg.ShutdownTimeout)
		defer timer.Stop()
		var shutdownWrites sync.WaitGroup
		for _, worker := range active {
			shutdownWrites.Add(1)
			go func() {
				defer shutdownWrites.Done()
				worker.sendShutdown()
			}()
		}

		done := make(chan struct{})
		go func() {
			p.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-timer.C():
			p.cancel()
			for _, worker := range p.activeSnapshot() {
				worker.kill()
			}
			<-done
		}
		p.cancel()
		shutdownWrites.Wait()
		close(p.results)
	})
}

func (p *Pool) activeSnapshot() []*workerProc {
	p.activeMu.Lock()
	defer p.activeMu.Unlock()
	active := make([]*workerProc, 0, len(p.active))
	for _, worker := range p.active {
		active = append(active, worker)
	}
	return active
}

func (p *Pool) dispatchLoop() {
	defer p.wg.Done()
	for {
		select {
		case <-p.quit:
			return
		case job := <-p.jobs:
			p.submitMu.Lock()
			p.submitCond.Signal()
			p.submitMu.Unlock()
			for {
				var worker *workerProc
				select {
				case <-p.quit:
					return
				case worker = <-p.free:
				}
				if worker.assign(job, p.watchdogDuration(job.Kind)) {
					break
				}
			}
		}
	}
}

func (p *Pool) watchdogDuration(kind MediaKind) time.Duration {
	if kind == MediaVideo {
		return p.cfg.VideoTimeout
	}
	return p.cfg.ImageTimeout
}

func (p *Pool) saveResult(job JobMsg, result JobResultMsg) {
	if job.Phase == PhasePreview {
		result.PreviewBytes = cloneBytes(result.PreviewBytes)
		if result.PreviewErrorCode == "" {
			p.metrics.filesDone.Add(1)
		} else {
			p.metrics.filesFailed.Add(1)
		}
		select {
		case p.results <- &result:
		case <-p.quit:
		}
		return
	}
	materializeFixedFrameResults(job, &result)
	frameErrors := erroredFrames(result.Frames)
	p.saveAnalysisResult(job, &result)
	for _, fieldError := range result.Errors {
		p.deps.errorLogger.Error("file error",
			"path_id", PathID(result.Path),
			"stage", fieldError.Stage,
			"screen_stage", job.ScreenStage,
			"source", job.Source,
			"field_mask", fieldError.Field,
			"err", RedactKnownPath(fieldError.Msg, result.Path),
			"worker_pid", result.WorkerPID,
		)
	}
	for _, frame := range frameErrors {
		p.deps.errorLogger.Error("file error",
			"path_id", PathID(result.Path),
			"stage", "frame",
			"field_mask", job.FieldsMask&videoSixFrameWorkerFields(),
			"screen_stage", job.ScreenStage,
			"source", job.Source,
			"frame_idx", frame.FrameIdx,
			"err", RedactKnownPath(frame.Error, result.Path),
			"worker_pid", result.WorkerPID,
		)
	}
	if len(result.Errors) == 0 && len(frameErrors) == 0 {
		p.metrics.filesDone.Add(1)
	} else {
		p.metrics.filesFailed.Add(1)
	}
	if result.Decoded {
		p.metrics.decodeCalls.Add(1)
	}
	p.metrics.readAttempts.Add(result.ReadAttempts)
	p.metrics.decodeAttempts.Add(result.DecodeAttempts)
	p.metrics.readNS.Add(result.ReadNS)
	p.metrics.decodeNS.Add(result.DecodeNS)
	if result.ThumbGenerated {
		p.metrics.thumbGenerated.Add(1)
	}
	if result.ThumbCacheHit {
		p.metrics.thumbCacheHits.Add(1)
	}
	select {
	case p.results <- &result:
	case <-p.quit:
	}
}

func materializeFixedFrameResults(job JobMsg, result *JobResultMsg) {
	if result == nil || len(result.Frames) != 0 || job.Kind != MediaVideo ||
		job.FieldsMask&videoSixFrameWorkerFields() == 0 {
		return
	}
	requested := normalizedRequestedFrames(job)
	result.Frames = make([]FrameFeature, 0, 6)
	for index, frame := range result.FrameResults {
		bit := uint8(1 << uint(index))
		if requested&bit == 0 {
			continue
		}
		converted := FrameFeature{FrameIdx: index, TimeMS: frame.TimeMS}
		if result.FramesDone&bit != 0 {
			converted.PDQ256 = cloneBytes(frame.PDQ256)
			converted.Quality = frame.Quality
			converted.PHashParts = cloneBytes(frame.PHashParts)
			converted.SobelHist = cloneBytes(frame.SobelHist)
		} else {
			converted.Error = fmt.Sprintf("native_status_%d", frame.Status)
		}
		result.Frames = append(result.Frames, converted)
	}
}

func (p *Pool) saveAnalysisResult(job JobMsg, result *JobResultMsg) {
	if p.store == nil {
		p.dedup.Resolve(*result)
		return
	}
	committed, err := p.store.SaveAnalysis(p.ctx, analysisStoreResult(p.cfg.MachineID, job, *result))
	if err != nil {
		p.dedup.FailByJob(result.JobID)
		clearAllAnalysisPayload(result)
		if errors.Is(err, store.ErrStale) {
			result.Errors = []FieldError{{Field: 0, Stage: "stale", Msg: "stale"}}
			return
		}
		if !errors.Is(err, context.Canceled) {
			p.deps.logger.Error("save analysis failed",
				"path_id", PathID(result.Path),
				"screen_stage", job.ScreenStage,
				"source", job.Source,
				"err", RedactKnownPath(err.Error(), result.Path),
			)
		}
		result.Errors = append(result.Errors, FieldError{Field: 0, Stage: "store", Msg: err.Error()})
		return
	}
	result.FieldsDone = committed.FieldsPresent & job.FieldsMask
	result.FramesDone = committed.FramesPresent & normalizedRequestedFrames(job)
	p.dedup.Resolve(*result)
	pruneUncommittedPayload(result)
}

func analysisStoreResult(machineID string, job JobMsg, result JobResultMsg) store.AnalysisResult {
	kind := store.MediaImage
	if job.Kind == MediaVideo {
		kind = store.MediaVideo
	}
	mtime := job.MTimeUnix
	if job.Phase == Phase2 {
		mtime = job.MTimeMS
	}
	errorsOut := make([]store.FieldError, len(result.Errors))
	for index, fieldError := range result.Errors {
		errorsOut[index] = store.FieldError{Field: fieldError.Field, Stage: fieldError.Stage, Msg: fieldError.Msg}
	}
	frames := analysisFrames(job, result)
	analysis := store.AnalysisResult{
		MachineID: machineID, Path: job.Path, Kind: kind, Size: job.Size, MTime: mtime,
		SHA512: cloneBytes(result.SHA512), RequestedFields: job.FieldsMask,
		FieldsDone: result.FieldsDone, RequestedFrames: normalizedRequestedFrames(job),
		PDQ: cloneBytes(result.PDQ), Quality: result.Quality, Width: result.Width, Height: result.Height,
		DurationMS: cloneInt64(result.DurationMS), ThumbPath: result.ThumbPath,
		ThumbPDQ: cloneBytes(result.ThumbPDQ), ThumbQuality: cloneInt32(result.ThumbQuality),
		PHashParts: cloneBytes(result.PHashParts), SobelHist: cloneBytes(result.SobelHist),
		Frames: frames, Errors: errorsOut,
	}
	if result.ContactSheetWidth > 0 {
		width := result.ContactSheetWidth
		analysis.ThumbWidth = &width
	}
	if result.ContactSheetHeight > 0 {
		height := result.ContactSheetHeight
		analysis.ThumbHeight = &height
	}
	return analysis
}

func normalizedRequestedFrames(job JobMsg) uint8 {
	frames := job.FrameMask
	if job.Kind == MediaVideo && job.FieldsMask&videoSixFrameWorkerFields() != 0 && frames == 0 {
		return FrameMaskFull
	}
	return frames
}

func analysisFrames(job JobMsg, result JobResultMsg) []store.Phase2Frame {
	if len(result.Frames) != 0 {
		frames := make([]store.Phase2Frame, len(result.Frames))
		for index, frame := range result.Frames {
			frames[index] = store.Phase2Frame{FrameIdx: frame.FrameIdx, PDQ256: cloneBytes(frame.PDQ256), Quality: frame.Quality, PHashParts: cloneBytes(frame.PHashParts), SobelHist: cloneBytes(frame.SobelHist), Error: frame.Error}
		}
		return frames
	}
	requested := normalizedRequestedFrames(job)
	frames := make([]store.Phase2Frame, 0, 6)
	for index := range 6 {
		bit := uint8(1 << uint(index))
		if requested&bit == 0 {
			continue
		}
		frame := result.FrameResults[index]
		stored := store.Phase2Frame{FrameIdx: index}
		if result.FramesDone&bit != 0 {
			stored.PDQ256 = cloneBytes(frame.PDQ256)
			stored.Quality = frame.Quality
			stored.PHashParts = cloneBytes(frame.PHashParts)
			stored.SobelHist = cloneBytes(frame.SobelHist)
		} else {
			stored.Error = fmt.Sprintf("native status %d", frame.Status)
		}
		frames = append(frames, stored)
	}
	return frames
}

func clearAllAnalysisPayload(result *JobResultMsg) {
	result.FieldsDone = 0
	result.FramesDone = 0
	clearFeaturePayload(result)
	clearPhase2FeaturePayload(result)
	result.ContactSheetStatus = 0
	result.ContactSheetWidth = 0
	result.ContactSheetHeight = 0
	result.FrameResults = [6]FrameResult{}
}

func pruneUncommittedPayload(result *JobResultMsg) {
	if result.FieldsDone&MaskSHA512 == 0 {
		result.SHA512 = nil
	}
	if result.FieldsDone&MaskImagePDQ == 0 {
		result.PDQ, result.Quality, result.Width, result.Height = nil, 0, 0, 0
	}
	if result.FieldsDone&MaskPHashParts == 0 {
		result.PHashParts = nil
	}
	if result.FieldsDone&MaskSobelHist == 0 {
		result.SobelHist = nil
	}
	if result.FieldsDone&(MaskVideoDuration|MaskVideoThumb) == 0 {
		result.DurationMS = nil
	}
	if result.FieldsDone&(MaskVideoContactSheet|MaskVideoThumb) == 0 {
		result.ThumbPath, result.ThumbPDQ, result.ThumbQuality = "", nil, nil
		result.ContactSheetStatus, result.ContactSheetWidth, result.ContactSheetHeight = 0, 0, 0
	}
	for index := range result.FrameResults {
		if result.FramesDone&(1<<uint(index)) == 0 {
			result.FrameResults[index] = FrameResult{}
		}
	}
	keptFrames := result.Frames[:0]
	for _, frame := range result.Frames {
		bit := uint8(1 << uint(frame.FrameIdx))
		if frame.FrameIdx < 0 || frame.FrameIdx >= 6 {
			continue
		}
		if result.FramesDone&bit == 0 {
			frame.PDQ256, frame.Quality, frame.PHashParts, frame.SobelHist = nil, 0, nil, nil
			if frame.Error == "" {
				continue
			}
		}
		keptFrames = append(keptFrames, frame)
	}
	result.Frames = keptFrames
}

func clearFeaturePayload(result *JobResultMsg) {
	result.SHA512 = nil
	result.PDQ = nil
	result.Quality = 0
	result.Width = 0
	result.Height = 0
	result.DurationMS = nil
	result.ThumbPath = ""
	result.ThumbPDQ = nil
	result.ThumbQuality = nil
}

func clearPhase2FeaturePayload(result *JobResultMsg) {
	result.PHashParts = nil
	result.SobelHist = nil
	result.Frames = nil
}

func attemptedPhase2Fields(result JobResultMsg) uint32 {
	attempted := result.FieldsDone & (MaskPHashParts | MaskSobelHist | videoSixFrameWorkerFields())
	if len(result.PHashParts) != 0 {
		attempted |= MaskPHashParts
	}
	if len(result.SobelHist) != 0 {
		attempted |= MaskSobelHist
	}
	if len(result.Frames) != 0 {
		switch result.ScreenStage {
		case ScreenStageTwo:
			attempted |= MaskVideo6FPHash
		case ScreenStageThree:
			attempted |= MaskVideo6FSobel
		default:
			attempted |= MaskVideo6F
		}
	}
	for _, fieldError := range result.Errors {
		attempted |= fieldError.Field & (MaskPHashParts | MaskSobelHist | videoSixFrameWorkerFields())
	}
	return attempted
}

func videoSixFrameWorkerFields() uint32 {
	return MaskVideo6F | MaskVideo6FPHash | MaskVideo6FSobel
}

func erroredFrames(frames []FrameFeature) []FrameFeature {
	var out []FrameFeature
	for _, frame := range frames {
		if frame.Error != "" {
			out = append(out, FrameFeature{
				FrameIdx: frame.FrameIdx,
				Error:    frame.Error,
			})
		}
	}
	return out
}

func phase1StoreResult(machineID string, result JobResultMsg) store.Phase1Result {
	errors := make([]store.FieldError, len(result.Errors))
	for i, fieldError := range result.Errors {
		errors[i] = store.FieldError{Field: fieldError.Field, Stage: fieldError.Stage, Msg: fieldError.Msg}
	}
	kind := store.MediaImage
	if result.Kind == MediaVideo {
		kind = store.MediaVideo
	}
	return store.Phase1Result{
		MachineID: machineID, Path: result.Path, Kind: kind, SHA512: cloneBytes(result.SHA512),
		FieldsDone: result.FieldsDone, PDQ: cloneBytes(result.PDQ), Quality: result.Quality,
		Width: result.Width, Height: result.Height, DurationMS: cloneInt64(result.DurationMS),
		ThumbPath: result.ThumbPath, ThumbPDQ: cloneBytes(result.ThumbPDQ),
		ThumbQuality: cloneInt32(result.ThumbQuality), Errors: errors,
	}
}

func phase2StoreResult(machineID string, result JobResultMsg) store.Phase2Result {
	errors := make([]store.FieldError, len(result.Errors))
	for i, fieldError := range result.Errors {
		errors[i] = store.FieldError{
			Field: fieldError.Field,
			Stage: fieldError.Stage,
			Msg:   fieldError.Msg,
		}
	}
	frames := make([]store.Phase2Frame, len(result.Frames))
	for i, frame := range result.Frames {
		frames[i] = store.Phase2Frame{
			FrameIdx:   frame.FrameIdx,
			PDQ256:     cloneBytes(frame.PDQ256),
			Quality:    frame.Quality,
			PHashParts: cloneBytes(frame.PHashParts),
			SobelHist:  cloneBytes(frame.SobelHist),
			Error:      frame.Error,
		}
	}
	kind := store.MediaImage
	if result.Kind == MediaVideo {
		kind = store.MediaVideo
	}
	return store.Phase2Result{
		MachineID:  machineID,
		Path:       result.Path,
		Kind:       kind,
		SHA512:     cloneBytes(result.SHA512),
		FieldsDone: result.FieldsDone,
		PHashParts: cloneBytes(result.PHashParts),
		SobelHist:  cloneBytes(result.SobelHist),
		Frames:     frames,
		Errors:     errors,
	}
}
