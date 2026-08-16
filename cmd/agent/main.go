package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"dedup/internal/agent"
	agentdelete "dedup/internal/agent/delete"
	"dedup/internal/agentinstance"
	"dedup/internal/config"
	fileenum "dedup/internal/enum"
	"dedup/internal/firstscreen"
	"dedup/internal/localanalysis"
	"dedup/internal/localcontrol"
	"dedup/internal/localdelete"
	"dedup/internal/localpreview"
	"dedup/internal/localreview"
	"dedup/internal/localtask"
	"dedup/internal/machineid"
	"dedup/internal/nodectl"
	"dedup/internal/proto"
	"dedup/internal/securefile"
	"dedup/internal/stats"
	"dedup/internal/store"
	"dedup/internal/syncer"
	"dedup/internal/worker"
	"dedup/internal/wproc"
)

func main() {
	configPath := flag.String("config", "agent.json", "配置文件路径")
	flag.Parse()
	if err := run(*configPath); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	return runWithDependencies(configPath, agent.NewDeleteLogger, machineid.Current)
}

type deleteLoggerFactory func(
	string,
) (*slog.Logger, func() error, error)

type machineIdentityProvider func() (machineid.Result, error)

type filesystemBrowserSetter interface {
	SetFilesystemBrowser(agent.FilesystemBrowser)
}

type filesystemBrowserFactory func() agent.FilesystemBrowser

func setAgentFilesystemBrowser(server filesystemBrowserSetter, newBrowser filesystemBrowserFactory) {
	server.SetFilesystemBrowser(newBrowser())
}

type agentEnumeratorOptions struct {
	Enabled     bool
	Primary     fileenum.Enumerator
	Fallback    fileenum.Enumerator
	StartClient func() error
	Poll        func(context.Context) error
	Logger      *slog.Logger
}

func newAgentEnumerator(
	ctx context.Context,
	options agentEnumeratorOptions,
) fileenum.Enumerator {
	if !options.Enabled {
		return fileenum.WalkerEnumerator{}
	}
	if options.Fallback == nil {
		options.Fallback = fileenum.WalkerEnumerator{}
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}

	var waitingMu sync.Mutex
	var lastWaitingLog time.Time
	enumerator := fileenum.NewAutoStartEnumerator(fileenum.AutoStartOptions{
		Context:     ctx,
		Primary:     options.Primary,
		Fallback:    options.Fallback,
		StartClient: options.StartClient,
		Poll:        options.Poll,
		OnWaiting: func(err error) {
			waitingMu.Lock()
			defer waitingMu.Unlock()
			now := time.Now()
			if !lastWaitingLog.IsZero() && now.Sub(lastWaitingLog) < 30*time.Second {
				return
			}
			lastWaitingLog = now
			options.Logger.Info("waiting for everything index", "err", err)
		},
		OnFallback: func(err error) {
			options.Logger.Warn(
				"everything unavailable, fallback to walker",
				"err", err,
			)
		},
		OnReady: func() {
			options.Logger.Info("everything enumerator ready")
		},
		OnRootFallback: func(root string, cause error) {
			logEverythingRootFallback(options.Logger, root, cause)
		},
	})
	enumerator.Start()
	return enumerator
}

func logEverythingRootFallback(logger *slog.Logger, root string, _ error) {
	logger.Warn("everything root unavailable, fallback to walker", "path_id", worker.PathID(root), "error_code", "everything_root_fallback")
}

func runWithDeleteLogger(
	configPath string,
	openDeleteLogger deleteLoggerFactory,
) error {
	return runWithDependencies(configPath, openDeleteLogger, machineid.Current)
}

func runWithDependencies(
	configPath string,
	openDeleteLogger deleteLoggerFactory,
	resolveIdentity machineIdentityProvider,
) error {
	cfg, err := config.LoadAgent(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if resolveIdentity == nil {
		return errors.New("resolve Agent machine identity: provider is nil")
	}
	identity, err := resolveIdentity()
	if err != nil {
		return fmt.Errorf("resolve Agent machine identity: %w", err)
	}
	cfg.MachineID = identity.ID
	executablePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve Agent executable: %w", err)
	}
	executablePath, err = filepath.Abs(executablePath)
	if err != nil {
		return fmt.Errorf("resolve absolute Agent executable: %w", err)
	}
	if err := validateAgentControlIdentity(cfg, executablePath); err != nil {
		return err
	}
	instance, err := agentinstance.AcquireSingleInstance(cfg.MachineID)
	if err != nil {
		return fmt.Errorf("acquire Agent single-instance lock: %w", err)
	}
	defer instance.Close()
	configSHA256, err := effectiveConfigSHA256(cfg)
	if err != nil {
		return fmt.Errorf("fingerprint effective config: %w", err)
	}
	startedAt := time.Now()
	portableRoot := filepath.Dir(executablePath)
	controlToken, err := (localcontrol.FileTokenStore{}).LoadOrCreate(localcontrol.TokenPath(portableRoot))
	if err != nil {
		return fmt.Errorf("load local control token: %w", err)
	}
	if err := wproc.PrepareContactSheetRoot(cfg.Thumb.CacheDir); err != nil {
		return fmt.Errorf("prepare thumb cache root: %w", err)
	}
	logger, errorLogger, closeLogs, err := agent.NewLoggers(cfg.DataDir, os.Stdout)
	if err != nil {
		return fmt.Errorf("open logs: %w", err)
	}
	slog.SetDefault(logger)
	defer closeLogs()
	for _, warning := range identity.Warnings {
		logger.Warn("machine identity source unavailable", "warning", warning)
	}
	crashLogger, closeCrashLog, err := agent.NewCrashLogger(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("open crash log: %w", err)
	}
	defer closeCrashLog()
	deleteLogger, closeDeleteLog, err := openDeleteLogger(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("open delete log: %w", err)
	}
	defer closeDeleteLog()

	local, err := store.Open(filepath.Join(cfg.DataDir, "agent.db"))
	if err != nil {
		return fmt.Errorf("open sqlite: %w", err)
	}
	defer local.Close()
	workerPool := worker.NewPool(
		workerPoolConfig(cfg),
		local,
		logger,
		errorLogger,
		crashLogger,
	)

	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stop()
	everythingPath := filepath.Join(filepath.Dir(executablePath), "Everything.exe")
	enumerator := newAgentEnumerator(ctx, agentEnumeratorOptions{
		Enabled:  cfg.UseEverything,
		Primary:  fileenum.NewEverythingEnumerator(),
		Fallback: fileenum.WalkerEnumerator{},
		StartClient: func() error {
			return fileenum.StartEverythingClientAt(everythingPath)
		},
		Logger: logger,
	})
	logger.Info("enumerator configured", "name", enumerator.Name())

	syncHealth := newSyncHealthState()
	pg := initializePostgres(context.Background(), cfg.PGDSN, syncHealth, logger)
	if pg != nil {
		defer pg.Close()
	}

	var statistics *stats.Collector
	var statsSink *stats.JSONLSink
	if cfg.Tuning.StatsEnabled {
		statistics = stats.New(cfg.Tuning.StatsHistoryS, workerPool)
		statsSink, err = stats.NewJSONLSink(
			filepath.Join(cfg.DataDir, "stats.log"),
			cfg.Tuning.StatsLogMB,
		)
		if err != nil {
			logger.Warn("open stats log failed", "err", err)
		} else {
			defer statsSink.Close()
			go statistics.Run(ctx, cfg.StatsInterval(), statsSink, func(err error) {
				logger.Warn("write stats log failed", "err", err)
			})
		}
	}
	if cfg.Tuning.PprofAddr != "" {
		if err := stats.StartPprof(ctx, cfg.Tuning.PprofAddr, logger); err != nil {
			logger.Warn("start pprof failed", "err", err)
		}
	}
	if pg != nil {
		uploader := syncer.New(local, pg, syncer.Config{
			Interval:    cfg.SyncInterval(),
			TriggerRows: int64(cfg.Sync.TriggerRows),
			UpsertBatch: cfg.Sync.UpsertBatch,
			OnHealth: func(update syncer.HealthUpdate) {
				syncHealth.set(update.Healthy, update.ErrorSummary)
			},
		}, logger)
		go uploader.Run(ctx)
	}

	fairPool := localtask.NewFairScheduler(workerPool)
	router := agent.NewPoolRouter(fairPool, logger)
	scans := agent.NewScanManagerWithPoolRouter(
		cfg,
		local,
		enumerator,
		agent.GoHasher{},
		fairPool,
		router,
		logger,
		errorLogger,
	)
	if statistics != nil {
		scans.SetObserver(statistics)
	}
	phase2 := agent.NewPhase2ManagerWithRuntime(
		cfg.MachineID,
		local,
		fairPool,
		router,
		nil,
		logger,
	)
	stageOne := localanalysis.NewStageOne(local, local, firstscreen.DefaultConfig(), logger)
	stageWorker := agent.NewLocalStageWorker(fairPool, router)
	analysisEngine := localanalysis.NewEngine(cfg.MachineID, stageOne, local, stageWorker, config.DefaultGUI().Phase2)
	tasks := localtask.NewService(cfg.MachineID, local, &agentLocalTaskRunner{scans: scans, analysis: analysisEngine})
	reviews := localreview.NewService(cfg.MachineID, local)
	previews := localpreview.NewService(cfg.MachineID, local, stageWorker)
	resumeLocalTasks, err := prepareLocalTaskLifecycle(ctx, tasks, logger)
	if err != nil {
		return fmt.Errorf("prepare local task recovery: %w", err)
	}
	dialer := agentdelete.NewPipeDialer(cfg.Delete.PipeName)
	forwarder := buildDeleteForwarder(
		cfg,
		dialer,
		local,
		deleteLogger,
		logger,
	)
	deletes := localdelete.NewService(cfg.MachineID, local, forwarder)
	var listenerReady atomic.Bool
	provider := newAgentStatusProvider(agentStatusInputs{
		MachineID:      cfg.MachineID,
		ExecutablePath: executablePath,
		ConfigSHA256:   configSHA256,
		StartedAt:      startedAt,
		ListenerReady:  listenerReady.Load,
		Workers:        workerPool,
		SyncHealth:     syncHealth.snapshot,
	})
	localHandler := newAgentLocalHandler(agentLocalHandlerInputs{
		Status: provider.ControlStatus, Shutdown: stop,
		ConfigPath: configPath, ExecutablePath: executablePath, CPUCount: runtime.NumCPU(),
		EffectiveConfigSHA256: configSHA256,
		Tasks:                 agent.NewLocalTaskHandler(tasks),
		Results:               agent.NewLocalResultHandler(reviews, previews),
		Deletes:               agent.NewLocalDeleteHandler(deletes),
	})
	return runService(
		workerPool,
		func() error {
			businessLogger := slog.New(&listenerReadyHandler{
				next: logger.Handler(),
				ready: func() {
					listenerReady.Store(true)
					resumeLocalTasks()
				},
			})
			server := agent.NewServer(cfg, scans, businessLogger, phase2)
			setAgentFilesystemBrowser(server, agent.NewFilesystemBrowser)
			server.SetLocalControl(controlToken, localHandler)
			if statistics != nil {
				server.SetStatsProvider(statistics)
			}
			server.SetDeleteHandler(forwarder)
			defer listenerReady.Store(false)
			if err := server.ListenAndServe(ctx); err != nil {
				return fmt.Errorf("server exited: %w", err)
			}
			return nil
		},
		func() error {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), phase2DrainTimeout(cfg))
			defer cancel()
			return tasks.Shutdown(shutdownCtx)
		},
		func() error { return drainPhase2(phase2, phase2DrainTimeout(cfg)) },
		func() error {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), phase2DrainTimeout(cfg))
			defer cancel()
			return fairPool.Shutdown(shutdownCtx)
		},
	)
}

func validateAgentControlIdentity(cfg *config.AgentConfig, executablePath string) error {
	if cfg == nil {
		return errors.New("control identity requires loaded config")
	}
	if err := nodectl.ValidateControlIdentity(cfg.MachineID, executablePath); err != nil {
		return fmt.Errorf("control identity invalid: %w", err)
	}
	return nil
}

func buildDeleteForwarder(
	cfg *config.AgentConfig,
	dialer agentdelete.HelperDialer,
	state agentdelete.StateStore,
	deleteLogger *slog.Logger,
	logger *slog.Logger,
) *agentdelete.Forwarder {
	return agentdelete.NewForwarder(
		cfg.MachineID,
		cfg.Delete,
		dialer,
		state,
		deleteLogger,
		logger,
	)
}

const phase2PoolCleanupGrace = 5 * time.Second

type phase2Shutdowner interface {
	Shutdown(context.Context) error
}

func phase2DrainTimeout(cfg *config.AgentConfig) time.Duration {
	timeoutS := cfg.Worker.ImageTimeoutS
	if cfg.Worker.VideoTimeoutS > timeoutS {
		timeoutS = cfg.Worker.VideoTimeoutS
	}
	return time.Duration(timeoutS)*time.Second + phase2PoolCleanupGrace
}

func drainPhase2(manager phase2Shutdowner, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return manager.Shutdown(ctx)
}

func initializePostgres(ctx context.Context, dsn string, health *syncHealthState, logger *slog.Logger) *pgxpool.Pool {
	if strings.TrimSpace(dsn) == "" {
		health.set(false, "sync_not_configured")
		return nil
	}
	pg, err := pgxpool.New(ctx, dsn)
	if err != nil {
		health.set(false, "postgres_config_invalid")
		if logger != nil {
			logger.Warn("postgres initialization degraded", "error_code", "postgres_config_invalid")
		}
		return nil
	}
	health.set(false, "sync_pending")
	return pg
}

type localTaskLifecycle interface {
	PrepareRecovery(context.Context) error
	ResumeRecoveredTasks(context.Context) error
	Shutdown(context.Context) error
}

func prepareLocalTaskLifecycle(ctx context.Context, tasks localTaskLifecycle, logger *slog.Logger) (func(), error) {
	if tasks == nil {
		return nil, errors.New("local task lifecycle is required")
	}
	if err := tasks.PrepareRecovery(ctx); err != nil {
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			go func() {
				if err := tasks.ResumeRecoveredTasks(ctx); err != nil && logger != nil {
					logger.Error("resume local tasks failed", "err", safeAgentSummary(err.Error()))
				}
			}()
		})
	}, nil
}

type managedPool interface {
	Start()
	Close()
}

func runService(
	pool managedPool,
	serve func() error,
	drain ...func() error,
) error {
	pool.Start()
	defer pool.Close()
	serveErr := serve()
	var drainErr error
	for _, wait := range drain {
		if wait == nil {
			continue
		}
		if err := wait(); err != nil && drainErr == nil {
			drainErr = err
		}
	}
	if serveErr != nil {
		return serveErr
	}
	return drainErr
}

func effectiveConfigSHA256(cfg *config.AgentConfig) (string, error) {
	canonical, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	canonical = append(canonical, '\n')
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

type listenerReadyHandler struct {
	next  slog.Handler
	ready func()
	once  sync.Once
}

func (h *listenerReadyHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level == slog.LevelInfo || h.next.Enabled(ctx, level)
}

func (h *listenerReadyHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == "agent listening" && h.ready != nil {
		h.once.Do(h.ready)
	}
	if !h.next.Enabled(ctx, record.Level) {
		return nil
	}
	return h.next.Handle(ctx, record)
}

func (h *listenerReadyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &listenerReadyHandler{next: h.next.WithAttrs(attrs), ready: h.ready}
}

func (h *listenerReadyHandler) WithGroup(name string) slog.Handler {
	return &listenerReadyHandler{next: h.next.WithGroup(name), ready: h.ready}
}

type syncHealthState struct {
	mu      sync.RWMutex
	healthy bool
	error   string
}

func newSyncHealthState() *syncHealthState { return &syncHealthState{} }

func (s *syncHealthState) set(healthy bool, summary string) {
	s.mu.Lock()
	s.healthy = healthy
	s.error = summary
	s.mu.Unlock()
}

func (s *syncHealthState) snapshot() agentSyncHealth {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return agentSyncHealth{Healthy: s.healthy, ErrorSummary: s.error}
}

const maxReportedWorkers = 1024

var (
	mediaPathSummary = regexp.MustCompile(`(?i)(?:[a-z]:\\|/)[^\r\n]*?\.(?:mp4|mkv|avi|mov|wmv|flv|webm|mp3|wav|flac|m4a|aac|jpg|jpeg|png|gif|webp|bmp|tif|tiff)(?:\b|$)`)
	envSummary       = regexp.MustCompile(`(?i)\benv(?:ironment)?\s*[:=]\s*[^[:space:],;}\]]+`)
)

type agentStatusInputs struct {
	MachineID      string
	ExecutablePath string
	ConfigSHA256   string
	StartedAt      time.Time
	ListenerReady  func() bool
	Workers        interface{ RuntimeSnapshot() worker.RuntimeSnapshot }
	SyncHealth     func() agentSyncHealth
}

type agentSyncHealth struct {
	Healthy      bool
	ErrorSummary string
}

type agentStatusProvider struct{ inputs agentStatusInputs }

func newAgentStatusProvider(inputs agentStatusInputs) *agentStatusProvider {
	return &agentStatusProvider{inputs: inputs}
}

func (p *agentStatusProvider) ControlStatus() nodectl.Status {
	serviceReady := p != nil && p.inputs.ListenerReady != nil && p.inputs.ListenerReady()
	var runtimeSnapshot worker.RuntimeSnapshot
	if p != nil && p.inputs.Workers != nil {
		runtimeSnapshot = p.inputs.Workers.RuntimeSnapshot()
	}
	expected := runtimeSnapshot.Expected
	if expected < 0 {
		expected = 0
	}
	if expected > maxReportedWorkers {
		expected = maxReportedWorkers
	}
	mapped := make([]nodectl.WorkerStatus, expected)
	for index := range mapped {
		mapped[index].Index = index
	}
	for _, source := range runtimeSnapshot.Workers {
		if source.Index < 0 || source.Index >= expected {
			continue
		}
		pid := source.PID
		if pid < 0 {
			pid = 0
		}
		mapped[source.Index] = nodectl.WorkerStatus{
			Index: source.Index, PID: pid, Ready: source.Ready,
			CurrentTaskSummary: boundedAgentSummary(source.CurrentTaskSummary, 96),
			LastErrorSummary:   boundedAgentSummary(source.LastErrorSummary, 192),
		}
	}
	ready := 0
	for _, status := range mapped {
		if status.Ready {
			ready++
		}
	}
	syncHealth := agentSyncHealth{}
	if p != nil && p.inputs.SyncHealth != nil {
		syncHealth = p.inputs.SyncHealth()
	}
	fullyReady := serviceReady && ready == expected
	lifecycle := "starting"
	if fullyReady {
		lifecycle = "running"
	}
	return nodectl.Status{
		Component: nodectl.ComponentAgent, MachineID: p.inputs.MachineID, PID: os.Getpid(),
		StartedAtUnixMS: p.inputs.StartedAt.UnixMilli(), ExecutablePath: p.inputs.ExecutablePath,
		ConfigSHA256: strings.ToLower(p.inputs.ConfigSHA256), Lifecycle: lifecycle,
		ServiceReady: serviceReady, Ready: fullyReady,
		WorkerExpected: expected, WorkerReady: ready, Workers: mapped,
		SyncHealthy: syncHealth.Healthy, SyncErrorSummary: safeAgentSummary(syncHealth.ErrorSummary),
		LastErrorSummary: safeAgentSummary(runtimeSnapshot.LastErrorSummary),
	}
}

func safeAgentSummary(value string) string {
	value = nodectl.SanitizeSummary(value)
	value = mediaPathSummary.ReplaceAllString(value, "[REDACTED_PATH]")
	value = envSummary.ReplaceAllString(value, "env=[REDACTED]")
	return nodectl.SanitizeSummary(value)
}

func boundedAgentSummary(value string, maxBytes int) string {
	value = safeAgentSummary(value)
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

type agentLocalHandlerInputs struct {
	Status                func() nodectl.Status
	Shutdown              context.CancelFunc
	ShutdownDelay         time.Duration
	ConfigPath            string
	ExecutablePath        string
	CPUCount              int
	EffectiveConfigSHA256 string
	Tasks                 agent.LocalHandler
	Results               agent.LocalHandler
	Deletes               agent.LocalHandler
}

type agentLocalHandler struct {
	inputs agentLocalHandlerInputs
	mu     sync.Mutex
	once   sync.Once
}

func newAgentLocalHandler(inputs agentLocalHandlerInputs) *agentLocalHandler {
	if inputs.ShutdownDelay <= 0 {
		inputs.ShutdownDelay = 25 * time.Millisecond
	}
	return &agentLocalHandler{inputs: inputs}
}

func (h *agentLocalHandler) HandleLocal(ctx context.Context, request proto.LocalRequest) proto.LocalResponse {
	if h == nil || ctx == nil || ctx.Err() != nil {
		return localAgentFailure(request.RequestID, "local_unavailable")
	}
	switch request.Operation {
	case proto.LocalOperationStatusGet:
		if h.inputs.Status == nil {
			return localAgentFailure(request.RequestID, "status_unavailable")
		}
		status := h.inputs.Status()
		if err := status.Validate(); err != nil {
			return localAgentFailure(request.RequestID, "status_unavailable")
		}
		return localAgentSuccess(request.RequestID, proto.LocalStatusGetResponse{Status: status})
	case proto.LocalOperationConfigGet:
		h.mu.Lock()
		defer h.mu.Unlock()
		_, canonical, digest, err := h.loadConfig()
		if err != nil {
			return localAgentFailure(request.RequestID, "config_unavailable")
		}
		return localAgentSuccess(request.RequestID, proto.LocalConfigGetResponse{CanonicalJSON: canonical, SHA256: digest})
	case proto.LocalOperationConfigValidate:
		_, _, digest, err := h.decodeConfigRequest(request.Payload)
		if err != nil {
			return localAgentFailure(request.RequestID, "invalid_config")
		}
		return localAgentSuccess(request.RequestID, proto.LocalConfigValidateResponse{
			Valid: true, SHA256: digest, RestartRequired: digest != h.inputs.EffectiveConfigSHA256,
		})
	case proto.LocalOperationConfigSave:
		_, canonical, digest, err := h.decodeConfigRequest(request.Payload)
		if err != nil {
			return localAgentFailure(request.RequestID, "invalid_config")
		}
		h.mu.Lock()
		err = writeAgentConfigAtomic(h.inputs.ConfigPath, canonical)
		h.mu.Unlock()
		if err != nil {
			return localAgentFailure(request.RequestID, "config_save_failed")
		}
		return localAgentSuccess(request.RequestID, proto.LocalConfigSaveResponse{
			SHA256: digest, RestartRequired: digest != h.inputs.EffectiveConfigSHA256,
		})
	case proto.LocalOperationShutdown:
		if h.inputs.Shutdown == nil {
			return localAgentFailure(request.RequestID, "shutdown_unavailable")
		}
		h.once.Do(func() {
			time.AfterFunc(h.inputs.ShutdownDelay, h.inputs.Shutdown)
		})
		return localAgentSuccess(request.RequestID, proto.LocalShutdownResponse{Accepted: true})
	default:
		if h.inputs.Tasks != nil && (strings.HasPrefix(request.Operation, "local.task.") || strings.HasPrefix(request.Operation, "local.analysis.")) {
			return h.inputs.Tasks.HandleLocal(ctx, request)
		}
		if h.inputs.Results != nil && (strings.HasPrefix(request.Operation, "local.groups.") ||
			request.Operation == proto.LocalOperationReviewSave ||
			request.Operation == proto.LocalOperationPreviewImage) {
			return h.inputs.Results.HandleLocal(ctx, request)
		}
		if h.inputs.Deletes != nil && strings.HasPrefix(request.Operation, "local.delete.") {
			return h.inputs.Deletes.HandleLocal(ctx, request)
		}
		return localAgentFailure(request.RequestID, proto.UnsupportedOperationErrorCode)
	}
}

type localAnalysisRunner interface {
	Run(context.Context, string) error
	RunWithProgress(context.Context, string, <-chan struct{}, func(localanalysis.AnalysisProgress) error) error
}

type localScanRunner interface {
	Prepare(proto.ScanTask, agent.Sender) (proto.TaskAck, func())
	DrainInstance(string, string, proto.TaskDrainReason) (bool, *proto.TaskStats)
	AbortInstance(string, string) bool
}

type agentLocalTaskRunner struct {
	scans    localScanRunner
	analysis localAnalysisRunner
	now      func() time.Time
}

func (r *agentLocalTaskRunner) currentTime() time.Time {
	if r != nil && r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *agentLocalTaskRunner) Run(control localtask.RunControl, request localtask.CreateRequest, snapshot localtask.Task, report func(localtask.ProgressUpdate) error) error {
	if r == nil || r.scans == nil || r.analysis == nil {
		return errors.New("local task runner unavailable")
	}
	if control.Context == nil {
		control.Context = context.Background()
	}
	emit := func(update localtask.ProgressUpdate) error {
		if report == nil {
			return nil
		}
		return report(update)
	}
	displayStats := runnerDecodeDisplayStats(snapshot.StatsJSON)
	if snapshot.Stage < 1 {
		terminal := make(chan proto.TaskDone, 1)
		var progressMu sync.Mutex
		latest := proto.TaskProgress{TaskID: request.TaskID}
		if snapshot.Phase == "scan" {
			latest.Done = snapshot.ProgressComplete
			latest.Total = snapshot.ProgressTotal
			latest.TotalKnown = snapshot.ProgressTotalKnown
		}
		var callbackErr error
		reportScan := func(message proto.TaskProgress, checkpoint int, finalStats *proto.TaskStats) {
			progressMu.Lock()
			message.Done = max(message.Done, latest.Done)
			message.Total = max(message.Total, latest.Total)
			message.TotalKnown = message.TotalKnown || latest.TotalKnown
			latest = message
			if finalStats == nil {
				displayStats = runnerMergeProgressStats(displayStats, message)
			} else {
				displayStats = runnerMergeFinalStats(displayStats, *finalStats)
			}
			statsJSON := runnerDisplayStatsJSON(displayStats)
			progressMu.Unlock()
			if err := emit(localtask.ProgressUpdate{Phase: "scan", Stage: checkpoint, ProgressComplete: message.Done, ProgressTotal: message.Total, ProgressTotalKnown: message.TotalKnown, StatsJSON: statsJSON}); err != nil {
				progressMu.Lock()
				if callbackErr == nil {
					callbackErr = err
				}
				progressMu.Unlock()
			}
		}
		reportScan(latest, 0, nil)
		ack, start := r.scans.Prepare(proto.ScanTask{TaskID: request.TaskID, InstanceID: snapshot.InstanceID, Roots: append([]string(nil), request.Roots...), Phase: 1, Options: proto.ScanOptions{Rescan: request.Rescan, Extensions: append([]string(nil), request.Extensions...)}}, func(messageType uint8, value any) error {
			switch messageType {
			case proto.MsgTaskProgress:
				reportScan(*value.(*proto.TaskProgress), 0, nil)
			case proto.MsgTaskDone:
				select {
				case terminal <- *value.(*proto.TaskDone):
				default:
				}
			}
			return nil
		})
		if !ack.Accepted {
			return errors.New("local scan rejected")
		}
		if start != nil {
			start()
		} else if ack.Stats != nil {
			terminal <- proto.TaskDone{TaskID: request.TaskID, Stats: *ack.Stats}
		}
		drain := control.Drain
		drainIssued := false
		var final proto.TaskDone
		for {
			select {
			case <-control.Context.Done():
				r.scans.AbortInstance(request.TaskID, snapshot.InstanceID)
				return control.Context.Err()
			case <-drain:
				reason := localDrainReason(control)
				r.scans.DrainInstance(request.TaskID, snapshot.InstanceID, reason)
				drainIssued = true
				drain = nil
			case final = <-terminal:
				goto scanFinished
			}
		}
	scanFinished:
		progressMu.Lock()
		message := latest
		progressMu.Unlock()
		message.Total = max(message.Total, final.Stats.Total)
		message.Done = max(message.Done, final.Stats.Done)
		checkpoint := 0
		if !drainIssued && final.Reason == "" {
			if missing := message.Total - final.Stats.Total; missing > 0 {
				final.Stats.Skipped += missing
			}
			message.Done = message.Total
			message.TotalKnown = true
			checkpoint = 1
		}
		final.Stats.Total = message.Total
		final.Stats.Done = message.Done
		reportScan(message, checkpoint, &final.Stats)
		progressMu.Lock()
		reportError := callbackErr
		progressMu.Unlock()
		if reportError != nil {
			return reportError
		}
		if drainIssued || final.Reason != "" {
			return localtask.ErrDrainRequested
		}
		snapshot.Stage = 1
		snapshot.Phase = "scan"
		snapshot.ProgressComplete = message.Done
		snapshot.ProgressTotal = message.Total
		snapshot.ProgressTotalKnown = true
		snapshot.StatsJSON = runnerDisplayStatsJSON(displayStats)
	}
	if request.Mode == proto.LocalTaskModeScanOnly {
		if snapshot.Phase != "finalizing" || !snapshot.ProgressTotalKnown || snapshot.ProgressComplete < snapshot.ProgressTotal {
			if err := emit(localtask.ProgressUpdate{Phase: "finalizing", Stage: 1, ProgressTotalKnown: false, StatsJSON: snapshot.StatsJSON}); err != nil {
				return err
			}
			if err := emit(localtask.ProgressUpdate{Phase: "finalizing", Stage: 1, ProgressComplete: 1, ProgressTotal: 1, ProgressTotalKnown: true, StatsJSON: snapshot.StatsJSON}); err != nil {
				return err
			}
		}
		return nil
	}
	if request.Mode == proto.LocalTaskModeScanThenAnalysis {
		analysisStarted := r.currentTime()
		analysisBaseDuration := displayStats.DurationMS
		if err := r.analysis.RunWithProgress(control.Context, request.TaskID, control.Drain, func(progress localanalysis.AnalysisProgress) error {
			if phaseRank(progress.Phase) < phaseRank(snapshot.Phase) {
				return nil
			}
			complete, total, known := progress.Complete, progress.Total, progress.TotalKnown
			if progress.Phase == snapshot.Phase {
				complete = max(complete, snapshot.ProgressComplete)
				total = max(total, snapshot.ProgressTotal)
				known = known || snapshot.ProgressTotalKnown
			}
			analysisElapsed := r.currentTime().Sub(analysisStarted).Milliseconds()
			if analysisElapsed < 0 {
				analysisElapsed = 0
			}
			displayStats.DurationMS = max(displayStats.DurationMS, analysisBaseDuration+analysisElapsed)
			return emit(localtask.ProgressUpdate{Phase: progress.Phase, Stage: progress.CheckpointStage, ProgressComplete: complete, ProgressTotal: total, ProgressTotalKnown: known, StatsJSON: runnerDisplayStatsJSON(displayStats)})
		}); err != nil {
			if errors.Is(err, localanalysis.ErrDrainRequested) {
				return localtask.ErrDrainRequested
			}
			return err
		}
	}
	return nil
}

func localDrainReason(control localtask.RunControl) proto.TaskDrainReason {
	if control.Reason == nil {
		return proto.TaskDrainProcessShutdown
	}
	switch control.Reason() {
	case localtask.DrainPause:
		return proto.TaskDrainPause
	case localtask.DrainStop:
		return proto.TaskDrainStop
	case localtask.DrainDelete:
		return proto.TaskDrainDelete
	default:
		return proto.TaskDrainProcessShutdown
	}
}

func phaseRank(phase string) int {
	switch phase {
	case "scan":
		return 1
	case "stage1":
		return 2
	case "stage2":
		return 3
	case "stage3":
		return 4
	case "finalizing":
		return 5
	default:
		return 0
	}
}

func runnerDisplayStatsJSON(stats proto.LocalTaskDisplayStats) string {
	stats.SchemaVersion = proto.LocalTaskDisplayStatsVersion
	encoded, err := json.Marshal(stats)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func runnerDecodeDisplayStats(statsJSON string) proto.LocalTaskDisplayStats {
	var stats proto.LocalTaskDisplayStats
	if json.Unmarshal([]byte(statsJSON), &stats) != nil || stats.SchemaVersion != proto.LocalTaskDisplayStatsVersion {
		return proto.LocalTaskDisplayStats{SchemaVersion: proto.LocalTaskDisplayStatsVersion}
	}
	if math.IsNaN(stats.Speed) || math.IsInf(stats.Speed, 0) || stats.Speed < 0 {
		stats.Speed = 0
	}
	stats.Failures = max(stats.Failures, 0)
	stats.DurationMS = max(stats.DurationMS, 0)
	return stats
}

func runnerMergeProgressStats(current proto.LocalTaskDisplayStats, progress proto.TaskProgress) proto.LocalTaskDisplayStats {
	if progress.Speed > 0 && !math.IsNaN(progress.Speed) && !math.IsInf(progress.Speed, 0) {
		current.Speed = progress.Speed
	}
	current.Failures = max(current.Failures, progress.Failed)
	current.DurationMS = max(current.DurationMS, progress.ElapsedMS)
	return current
}

func runnerMergeFinalStats(current proto.LocalTaskDisplayStats, stats proto.TaskStats) proto.LocalTaskDisplayStats {
	current.Failures = max(current.Failures, stats.Failed)
	current.DurationMS = max(current.DurationMS, stats.ElapsedMS)
	return current
}

func (h *agentLocalHandler) loadConfig() (*config.AgentConfig, []byte, string, error) {
	data, err := os.ReadFile(h.inputs.ConfigPath)
	if err != nil {
		return nil, nil, "", err
	}
	return h.validateCanonicalConfig(data, false)
}

func (h *agentLocalHandler) decodeConfigRequest(payload []byte) (*config.AgentConfig, []byte, string, error) {
	var request proto.LocalConfigRequest
	if err := proto.DecodeLocalPayload(payload, &request); err != nil {
		return nil, nil, "", err
	}
	return h.validateCanonicalConfig(request.CanonicalJSON, true)
}

func (h *agentLocalHandler) validateCanonicalConfig(data []byte, requireCanonical bool) (*config.AgentConfig, []byte, string, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	cfg := config.DefaultAgent()
	if err := decoder.Decode(cfg); err != nil {
		return nil, nil, "", err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("trailing Agent configuration")
		}
		return nil, nil, "", err
	}
	if requireCanonical {
		inputCanonical, err := canonicalAgentConfig(cfg)
		if err != nil || !bytes.Equal(inputCanonical, data) {
			return nil, nil, "", errors.New("Agent configuration is not canonical")
		}
	}
	validated, err := config.ValidateAgent(cfg, h.inputs.ExecutablePath, h.inputs.CPUCount)
	if err != nil {
		return nil, nil, "", err
	}
	canonical, err := canonicalAgentConfig(validated)
	if err != nil {
		return nil, nil, "", err
	}
	digest := sha256.Sum256(canonical)
	return validated, canonical, hex.EncodeToString(digest[:]), nil
}

func canonicalAgentConfig(cfg *config.AgentConfig) ([]byte, error) {
	canonical, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(canonical, '\n'), nil
}

func writeAgentConfigAtomic(path string, canonical []byte) (err error) {
	return securefile.WriteAtomic(path, canonical, os.ReadFile)
}

func localAgentSuccess(requestID string, payload any) proto.LocalResponse {
	encoded, err := proto.EncodeLocalPayload(payload)
	if err != nil {
		return localAgentFailure(requestID, "internal_error")
	}
	return proto.LocalResponse{RequestID: requestID, OK: true, Payload: encoded}
}

func localAgentFailure(requestID, code string) proto.LocalResponse {
	return proto.LocalResponse{RequestID: requestID, ErrorCode: code}
}

func workerPoolConfig(cfg *config.AgentConfig) worker.Config {
	return worker.Config{
		WorkerExe:        cfg.Worker.ExePath,
		WorkerCount:      cfg.Worker.Count,
		MachineID:        cfg.MachineID,
		ImageTimeout:     time.Duration(cfg.Worker.ImageTimeoutS) * time.Second,
		VideoTimeout:     time.Duration(cfg.Worker.VideoTimeoutS) * time.Second,
		RespawnDelay:     time.Duration(cfg.Worker.RespawnDelayMS) * time.Millisecond,
		WorkerEnv:        cfg.WorkerEnv(),
		IPCMaxFrameBytes: cfg.IPC.MaxFrameMB << 20,
	}
}
