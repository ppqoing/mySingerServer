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
	"dedup/internal/localcontrol"
	"dedup/internal/machineid"
	"dedup/internal/nodectl"
	"dedup/internal/proto"
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
	})
	return runService(
		workerPool,
		func() error {
			businessLogger := slog.New(&listenerReadyHandler{
				next: logger.Handler(),
				ready: func() {
					listenerReady.Store(true)
				},
			})
			server := agent.NewServer(cfg, scans, businessLogger, phase2)
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
		return localAgentFailure(request.RequestID, proto.UnsupportedOperationErrorCode)
	}
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
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(canonical); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	written, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(written, canonical) {
		if err != nil {
			return err
		}
		return errors.New("saved Agent configuration verification failed")
	}
	return nil
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
