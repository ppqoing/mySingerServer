package agent

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dedup/internal/config"
	"dedup/internal/diskmap"
	fileenum "dedup/internal/enum"
	"dedup/internal/proto"
	"dedup/internal/store"
	"dedup/internal/worker"
)

type Sender func(msgType uint8, value any) error

type DiskResolver func(root string) (diskNo int64, isSSD bool, err error)

type WorkerPool interface {
	Submit(*worker.JobMsg) error
	Results() <-chan *worker.JobResultMsg
	Crashes() <-chan worker.CrashRecord
	Metrics() worker.MetricsSnapshot
}

type ScanManager struct {
	cfg      *config.AgentConfig
	st       *store.DB
	enumr    fileenum.Enumerator
	hasher   Hasher
	log      *slog.Logger
	errLog   *slog.Logger
	resolver DiskResolver
	pool     WorkerPool
	router   *PoolRouter
	limiter  *byteLimiter
	observer ScanObserver

	mu    sync.Mutex
	tasks map[string]*ScanState
	disks map[int64]bool
	runMu sync.Mutex
}

type ScanState struct {
	Task   proto.ScanTask
	Status string
	Stats  proto.TaskStats

	mu        sync.Mutex
	featureMu sync.Mutex
	sender    Sender
	binding   uint64
	seq       uint64
	start     sync.Once

	total      atomic.Int64
	done       atomic.Int64
	failed     atomic.Int64
	scanErrors atomic.Int64
	speedWin   *speedWindow
	poolStart  worker.MetricsSnapshot
}

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
	return &ScanManager{
		cfg:      cfg,
		st:       st,
		enumr:    enumr,
		hasher:   hasher,
		log:      log,
		errLog:   errLog,
		resolver: resolver,
		tasks:    make(map[string]*ScanState),
		disks:    make(map[int64]bool),
		limiter:  newByteLimiter(int64(cfg.Tuning.PendingBytesMB) << 20),
	}
}

func (m *ScanManager) SetObserver(observer ScanObserver) {
	m.mu.Lock()
	m.observer = observer
	m.mu.Unlock()
}

func (m *ScanManager) currentObserver() ScanObserver {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.observer
}

func resolveDisk(root string) (int64, bool, error) {
	mountPoint, err := diskmap.MountPointOf(root)
	if err != nil {
		return -1, false, err
	}
	info, err := diskmap.Resolve(mountPoint)
	if err != nil {
		return -1, false, err
	}
	if !info.MediaTypeKnown {
		slog.Warn(
			"disk media type unavailable, using HDD scheduling",
			"root", root,
			"mount_point", mountPoint,
			"device_number", info.DeviceNumber,
		)
	}
	return int64(info.DeviceNumber), info.IsSSD, nil
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
	if state, exists := m.tasks[task.TaskID]; exists {
		if !sameScanEnvelope(state.Task, task) {
			m.mu.Unlock()
			return rejectedAck(task.TaskID, "task_id envelope mismatch"), nil
		}
		state.bindSender(sender)
		state.mu.Lock()
		status := state.Status
		stats := state.Stats
		state.mu.Unlock()
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

	state := &ScanState{
		Task:     task,
		Status:   "running",
		speedWin: newSpeedWindow(10 * time.Second),
	}
	state.bindSender(sender)
	m.tasks[task.TaskID] = state
	m.mu.Unlock()
	return proto.TaskAck{
		TaskID: task.TaskID, Accepted: true, Reason: "accepted", Total: -1,
	}, m.startScan(state)
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
	if left.TaskID != right.TaskID || left.Phase != right.Phase ||
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
	ctx := context.Background()
	if m.pool != nil {
		state.poolStart = m.pool.Metrics()
	}
	var enumerated int64
	seen := make(map[string]struct{})

	for _, root := range state.Task.Roots {
		diskNo, isSSD, err := m.resolver(root)
		if err != nil {
			state.failed.Add(1)
			state.scanErrors.Add(1)
			m.reportErr(state, root, "enum", err)
			continue
		}
		m.mu.Lock()
		m.disks[diskNo] = isSSD
		m.mu.Unlock()

		buffer := make([]store.EnumUpsert, 0, 10_000)
		flush := func() error {
			if len(buffer) == 0 {
				return nil
			}
			if err := m.st.UpsertEnumerated(ctx, buffer); err != nil {
				return err
			}
			for _, record := range buffer {
				seen[pathKey(record.Path)] = struct{}{}
			}
			buffer = buffer[:0]
			return nil
		}
		err = m.enumr.Enum(root, func(record fileenum.FileRecord) error {
			if len(state.Task.Options.Extensions) > 0 &&
				!extIn(record.Path, state.Task.Options.Extensions) {
				return nil
			}
			enumerated++
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
		if err == nil {
			err = flush()
		}
		if err != nil {
			state.failed.Add(1)
			state.scanErrors.Add(1)
			m.reportErr(state, root, "enum", err)
		}
	}

	pending, err := m.st.PendingSnapshot(ctx, m.cfg.MachineID)
	if err != nil {
		state.failed.Add(1)
		state.scanErrors.Add(1)
		m.reportErr(state, "", "enum", err)
		m.finish(state, started, enumerated)
		return
	}
	pending = filterPendingSeen(pending, seen)
	work, _ := m.preparePending(state, pending)
	var total int64
	for _, files := range work {
		total += int64(len(files))
	}
	state.total.Store(total)
	state.send(proto.MsgTaskProgress, &proto.TaskProgress{
		TaskID: state.Task.TaskID, Total: total,
	})

	hashResults := make(chan store.HashResult, 1024)
	mediaResults := make(chan proto.FeatureItem, 1024)
	var disks sync.WaitGroup
	for diskNo, files := range work {
		streams := m.cfg.Scan.HDDStreams
		if m.isSSD(diskNo) {
			streams = m.cfg.Scan.SSDStreams
		}
		disks.Add(1)
		go func(diskNo int64, files []scanWork, streams int) {
			defer disks.Done()
			m.processDisk(
				state,
				diskNo,
				files,
				streams,
				hashResults,
				mediaResults,
			)
		}(diskNo, files, streams)
	}

	hashWriterDone := make(chan struct{})
	go m.resultWriter(state, hashResults, hashWriterDone)
	mediaWriterDone := make(chan struct{})
	go m.mediaResultWriter(state, mediaResults, mediaWriterDone)
	progressDone := make(chan struct{})
	go m.progressLoop(state, progressDone)
	disks.Wait()
	close(hashResults)
	close(mediaResults)
	<-hashWriterDone
	<-mediaWriterDone
	close(progressDone)
	m.finish(state, started, enumerated)
}

type scanWork struct {
	file        store.PendingFile
	media       *worker.JobMsg
	terminal    <-chan poolTerminal
	cancelRoute func()
	prepareErr  error
}

type mediaRoute struct {
	job    *worker.JobMsg
	cancel func()
}

func (m *ScanManager) preparePending(
	state *ScanState,
	pending map[int64][]store.PendingFile,
) (map[int64][]scanWork, map[int64]mediaRoute) {
	work := make(map[int64][]scanWork, len(pending))
	terminals := make(map[int64]mediaRoute)
	router := m.ensurePoolRouter()
	for diskNo, files := range pending {
		for _, file := range files {
			kind := MediaKindWithExtensions(
				file.Path,
				m.cfg.Scan.ImageExts,
				m.cfg.Scan.VideoExts,
			)
			if kind == "other" {
				if file.MissingMask&proto.FieldSHA512 != 0 {
					work[diskNo] = append(work[diskNo], scanWork{file: file})
				}
				continue
			}
			mask := file.MissingMask
			mediaKind := worker.MediaImage
			if kind == "image" {
				mask &= worker.MaskAllImage
			} else {
				mediaKind = worker.MediaVideo
				mask &= worker.MaskAllVideo
			}
			if mask == 0 {
				continue
			}
			current := scanWork{file: file}
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
				Path: file.Path, Kind: mediaKind, Phase: worker.Phase1,
				FieldsMask: mask, Size: file.Size, MTimeUnix: file.MTime,
				KnownSHA: knownSHA,
			}
			terminal, cancelRoute, err := router.Register(current.media)
			if err != nil {
				current.media = nil
				current.prepareErr = err
				work[diskNo] = append(work[diskNo], current)
				continue
			}
			current.terminal = terminal
			current.cancelRoute = cancelRoute
			work[diskNo] = append(work[diskNo], current)
			terminals[jobID] = mediaRoute{
				job: current.media, cancel: current.cancelRoute,
			}
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
	seen map[string]struct{},
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

func pathKey(path string) string {
	return strings.ToLower(strings.TrimPrefix(path, `\\?\`))
}

func (m *ScanManager) isSSD(diskNo int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.disks[diskNo]
}

func (m *ScanManager) processDisk(
	state *ScanState,
	diskNo int64,
	files []scanWork,
	streams int,
	hashOut chan<- store.HashResult,
	mediaOut chan<- proto.FeatureItem,
) {
	jobs := make(chan scanWork)
	var workers sync.WaitGroup
	for index := 0; index < streams; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for work := range jobs {
				err := runObservedWork(
					context.Background(),
					m.limiter,
					m.currentObserver(),
					diskNo,
					work.file.Size,
					func() (time.Duration, time.Duration) {
						if work.media != nil || work.prepareErr != nil {
							return m.processMediaWork(state, work, mediaOut)
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
						state.speedWin.Add(1)
						state.done.Add(1)
						hashOut <- result
						return time.Since(started), 0
					},
				)
				if err != nil {
					state.failed.Add(1)
					m.reportErr(state, work.file.Path, "backpressure", err)
				}
			}
		}()
	}
	for _, file := range files {
		jobs <- file
	}
	close(jobs)
	workers.Wait()
}

func (m *ScanManager) processMediaWork(
	state *ScanState,
	work scanWork,
	out chan<- proto.FeatureItem,
) (time.Duration, time.Duration) {
	if work.prepareErr != nil {
		state.failed.Add(1)
		state.done.Add(1)
		state.speedWin.Add(1)
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
	if err := m.pool.Submit(work.media); err != nil {
		work.cancelRoute()
		state.failed.Add(1)
		state.done.Add(1)
		state.speedWin.Add(1)
		m.reportErr(state, work.file.Path, "worker", err)
		out <- proto.FeatureItem{
			Path:   work.file.Path,
			Size:   work.file.Size,
			MTime:  work.file.MTime,
			Status: proto.StatusFailed,
			Err:    err.Error(),
			FieldErrors: []proto.FieldError{{
				Field: work.media.FieldsMask,
				Stage: "worker",
				Msg:   err.Error(),
			}},
		}
		return 0, 0
	}
	terminal := <-work.terminal
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
	state.done.Add(1)
	state.speedWin.Add(1)
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
			context.Background(),
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
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			state.send(proto.MsgTaskProgress, &proto.TaskProgress{
				TaskID: state.Task.TaskID,
				Done:   state.done.Load(),
				Total:  state.total.Load(),
				Speed:  state.speedWin.Rate(),
			})
		}
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
	state.Status = "done"
	avgReadMS, avgDecodeMS := metricAveragesMS(poolDelta)
	state.Stats = proto.TaskStats{
		Total:            enumerated,
		Done:             state.done.Load(),
		Skipped:          enumerated - state.done.Load(),
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
	state.send(proto.MsgTaskDone, &proto.TaskDone{
		TaskID: state.Task.TaskID,
		Stats:  stats,
	})
	if taskPool, ok := m.pool.(interface{ EndTask(string) }); ok {
		taskPool.EndTask(state.Task.TaskID)
	}
	time.AfterFunc(10*time.Minute, func() {
		m.mu.Lock()
		delete(m.tasks, state.Task.TaskID)
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
		"path", path,
		"stage", stage,
		"err", err.Error(),
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
