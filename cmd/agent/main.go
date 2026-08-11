package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"dedup/internal/agent"
	agentdelete "dedup/internal/agent/delete"
	"dedup/internal/agentcontrol"
	"dedup/internal/config"
	fileenum "dedup/internal/enum"
	"dedup/internal/machineid"
	"dedup/internal/nodectl"
	"dedup/internal/stats"
	"dedup/internal/store"
	"dedup/internal/syncer"
	"dedup/internal/worker"
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
			options.Logger.Warn(
				"everything root unavailable, fallback to walker",
				"root", root,
				"err", cause,
			)
		},
	})
	enumerator.Start()
	return enumerator
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
	instance, err := agentcontrol.AcquireSingleInstance(cfg.MachineID)
	if err != nil {
		return fmt.Errorf("acquire Agent single-instance lock: %w", err)
	}
	defer instance.Close()
	configSHA256, err := effectiveConfigSHA256(cfg)
	if err != nil {
		return fmt.Errorf("fingerprint effective config: %w", err)
	}
	startedAt := time.Now()
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

	pg, err := pgxpool.New(context.Background(), cfg.PGDSN)
	if err != nil {
		return fmt.Errorf("parse postgres DSN: %w", err)
	}
	defer pg.Close()
	syncHealth := newSyncHealthState()
	pingContext, cancelPing := context.WithTimeout(context.Background(), 5*time.Second)
	if err := pg.Ping(pingContext); err != nil {
		logger.Warn("postgres unreachable at startup, syncer will retry", "err", err)
		syncHealth.set(false, err.Error())
	} else {
		syncHealth.set(true, "")
	}
	cancelPing()

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
	uploader := syncer.New(local, pg, syncer.Config{
		Interval:    cfg.SyncInterval(),
		TriggerRows: int64(cfg.Sync.TriggerRows),
		UpsertBatch: cfg.Sync.UpsertBatch,
		OnHealth: func(update syncer.HealthUpdate) {
			syncHealth.set(update.Healthy, update.ErrorSummary)
		},
	}, logger)
	go uploader.Run(ctx)

	router := agent.NewPoolRouter(workerPool, logger)
	scans := agent.NewScanManagerWithPoolRouter(
		cfg,
		local,
		enumerator,
		agent.GoHasher{},
		workerPool,
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
		workerPool,
		router,
		nil,
		logger,
	)
	dialer := agentdelete.NewPipeDialer(cfg.Delete.PipeName)
	forwarder := buildDeleteForwarder(
		cfg,
		dialer,
		local,
		deleteLogger,
		logger,
	)
	var listenerReady atomic.Bool
	provider := agentcontrol.NewProvider(agentcontrol.Inputs{
		MachineID:      cfg.MachineID,
		ExecutablePath: executablePath,
		ConfigSHA256:   configSHA256,
		StartedAt:      startedAt,
		ListenerReady:  listenerReady.Load,
		Workers:        workerPool,
		SyncHealth:     syncHealth.snapshot,
	})
	controlService := agentcontrol.New(provider, nodectl.ShutdownFunc(stop))
	return runControlledService(
		ctx,
		stop,
		workerPool,
		func(ctx context.Context, ready func()) error {
			businessLogger := slog.New(&listenerReadyHandler{
				next: logger.Handler(),
				ready: func() {
					listenerReady.Store(true)
					ready()
				},
			})
			server := agent.NewServer(cfg, scans, businessLogger, phase2)
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
		controlService.Run,
		func() error { return drainPhase2(phase2, phase2DrainTimeout(cfg)) },
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
) agent.DeleteHandler {
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

type managedPool interface {
	Start()
	Close()
}

type contextualService func(context.Context, func()) error

func runControlledService(
	ctx context.Context,
	cancel context.CancelFunc,
	pool managedPool,
	business contextualService,
	control func(context.Context) error,
	drain ...func() error,
) error {
	pool.Start()
	defer pool.Close()

	ready := make(chan struct{})
	var readyOnce sync.Once
	businessResult := make(chan error, 1)
	go func() {
		businessResult <- business(ctx, func() { readyOnce.Do(func() { close(ready) }) })
	}()

	var primary error
	businessDone := false
	controlDone := false
	controlStarted := false
	controlResult := make(chan error, 1)
	select {
	case <-ctx.Done():
	case err := <-businessResult:
		businessDone = true
		primary = unexpectedServiceExit("business", err, ctx)
	case <-ready:
		controlStarted = true
		go func() { controlResult <- control(ctx) }()
	}

	if controlStarted && primary == nil && ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case err := <-businessResult:
			businessDone = true
			primary = unexpectedServiceExit("business", err, ctx)
		case err := <-controlResult:
			controlDone = true
			primary = unexpectedServiceExit("control", err, ctx)
		}
	}
	cancel()
	if !businessDone {
		<-businessResult
	}
	if controlStarted && !controlDone {
		<-controlResult
	}

	var drainErr error
	for _, wait := range drain {
		if wait == nil {
			continue
		}
		if err := wait(); err != nil && drainErr == nil {
			drainErr = err
		}
	}
	if primary != nil {
		return primary
	}
	return drainErr
}

func unexpectedServiceExit(name string, err error, root context.Context) error {
	if root.Err() != nil && (err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return nil
	}
	if err == nil {
		return fmt.Errorf("%s service exited unexpectedly", name)
	}
	return fmt.Errorf("%s service exited: %w", name, err)
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

func (s *syncHealthState) snapshot() agentcontrol.SyncHealth {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return agentcontrol.SyncHealth{Healthy: s.healthy, ErrorSummary: s.error}
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
