package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"dedup/internal/config"
	"dedup/internal/firstscreen"
	"dedup/internal/gui"
	"dedup/internal/phase2"
	"dedup/internal/proto"
)

type analysisPoolConn interface {
	Conn() *pgx.Conn
	Release()
}

type analysisPool interface {
	Acquire(context.Context) (analysisPoolConn, error)
}

type pgxAnalysisPool struct {
	pool *pgxpool.Pool
}

type deleteQueryDB interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func newDeleteRuntime(
	db deleteQueryDB,
	transport gui.DeleteTransport,
	ttl time.Duration,
	now func() time.Time,
	logger *slog.Logger,
) (*gui.DeleteService, *gui.ConfirmStore) {
	confirms := gui.NewConfirmStore(ttl, now)
	return gui.NewDeleteService(db, transport, confirms, logger), confirms
}

func (p pgxAnalysisPool) Acquire(ctx context.Context) (analysisPoolConn, error) {
	return p.pool.Acquire(ctx)
}

type analysisEngine interface {
	Run(context.Context) (*firstscreen.RunStats, error)
}

type analysisEngineFactory func(
	*pgx.Conn,
	firstscreen.Config,
	*slog.Logger,
) analysisEngine

type pooledAnalysisRunner struct {
	shutdownContext context.Context
	pool            analysisPool
	cfg             firstscreen.Config
	logger          *slog.Logger
	factory         analysisEngineFactory
}

func newPooledAnalysisRunner(
	shutdownContext context.Context,
	pool analysisPool,
	cfg firstscreen.Config,
	logger *slog.Logger,
	factories ...analysisEngineFactory,
) gui.AnalysisRunner {
	factory := analysisEngineFactory(func(
		conn *pgx.Conn,
		cfg firstscreen.Config,
		logger *slog.Logger,
	) analysisEngine {
		return firstscreen.NewAnalyzer(firstscreen.NewStore(conn, cfg), cfg, logger)
	})
	if len(factories) > 0 {
		factory = factories[0]
	}
	return &pooledAnalysisRunner{
		shutdownContext: shutdownContext,
		pool:            pool,
		cfg:             cfg,
		logger:          logger,
		factory:         factory,
	}
}

func (r *pooledAnalysisRunner) Run() (*firstscreen.RunStats, error) {
	conn, err := r.pool.Acquire(r.shutdownContext)
	if err != nil {
		return nil, fmt.Errorf("firstscreen: acquire dedicated connection: %w", err)
	}
	defer conn.Release()
	return r.factory(conn.Conn(), r.cfg, r.logger).Run(r.shutdownContext)
}

func firstScreenConfig(cfg config.FirstScreenConfig) firstscreen.Config {
	return firstscreen.Config{
		HammingMax:            cfg.HammingMax,
		AspectTolerance:       cfg.AspectTolerance,
		VideoDurationWindowMs: cfg.VideoDurationWindowMs,
		ImageQualityMin:       cfg.ImageQualityMin,
		ReadPageSize:          cfg.ReadPageSize,
		GroupInsertBatch:      cfg.GroupInsertBatch,
		SHAResolveChunk:       cfg.SHAResolveChunk,
	}
}

type guiHTTPServer interface {
	Serve(net.Listener) error
	Shutdown(context.Context) error
}

var (
	guiExecutablePath   = os.Executable
	guiListen           = net.Listen
	guiOpenBrowser      = openGUIBrowser
	guiShowStartupError = showGUIStartupError
)

type analysisLifecycle interface {
	BeginAnalysisShutdown()
	WaitForAnalysis()
}

type phase2MessageDispatcher interface {
	BindFeatureResult(
		string,
		*proto.FeatureResult,
	) (*phase2.BoundFeatureResult, error)
	HandleMessage(string, any) bool
}

type rescreenConsumer interface {
	HandleFeatureResult(context.Context, *phase2.BoundFeatureResult) error
	Reload(context.Context) error
	FinalizeIfIdle(context.Context) (bool, error)
	Progress() phase2.RescreenProgress
}

type phase2RouteConsumer interface {
	HandleFeatureResult(context.Context, *phase2.BoundFeatureResult) error
	FinalizeIfIdle(context.Context) (bool, error)
}

type taskMessageConsumer interface {
	Dispatch(string, any)
}

type deleteReportConsumer interface {
	HandleReport(string, *proto.DeleteReport)
}

type pendingDispatcher interface {
	DispatchPending(context.Context) error
}

type pendingScanSource interface {
	PendingScans(string) []proto.ScanTask
}

type agentMessageSender interface {
	Send(string, uint8, any) error
}

type machinePendingDispatcher interface {
	DispatchMachinePending(context.Context, string) error
}

type confirmedGroupRebuilder interface {
	RebuildGroups(context.Context, string) (phase2.GroupStats, error)
}

type phase2FinalizeWorkerConfig struct {
	AttemptTimeout time.Duration
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

type phase2Orchestration struct {
	gate sync.Mutex

	rescreener  rescreenConsumer
	dispatcher  pendingDispatcher
	groupWriter confirmedGroupRebuilder
	armed       bool

	groupGeneration    uint64
	groupGenerationSet bool
	groupKindIndex     int

	startOnce sync.Once
	notify    chan struct{}
	workerWG  sync.WaitGroup
	workerCfg phase2FinalizeWorkerConfig
	workerLog *slog.Logger
}

func newPhase2Orchestration(
	rescreener rescreenConsumer,
	dispatcher pendingDispatcher,
	groupWriters ...confirmedGroupRebuilder,
) *phase2Orchestration {
	var groupWriter confirmedGroupRebuilder
	if len(groupWriters) != 0 {
		groupWriter = groupWriters[0]
	}
	return &phase2Orchestration{
		rescreener:  rescreener,
		dispatcher:  dispatcher,
		groupWriter: groupWriter,
		armed:       true,
		notify:      make(chan struct{}, 1),
	}
}

func (orchestration *phase2Orchestration) Start(
	ctx context.Context,
	logger *slog.Logger,
	cfg phase2FinalizeWorkerConfig,
) {
	orchestration.startOnce.Do(func() {
		if cfg.AttemptTimeout <= 0 {
			cfg.AttemptTimeout = 5 * time.Minute
		}
		if cfg.InitialBackoff <= 0 {
			cfg.InitialBackoff = 250 * time.Millisecond
		}
		if cfg.MaxBackoff < cfg.InitialBackoff {
			cfg.MaxBackoff = 5 * time.Second
		}
		orchestration.workerCfg = cfg
		orchestration.workerLog = logger
		orchestration.workerWG.Add(1)
		go orchestration.runFinalizeWorker(ctx)
	})
}

func (orchestration *phase2Orchestration) SignalFinalize() {
	select {
	case orchestration.notify <- struct{}{}:
	default:
	}
}

func (orchestration *phase2Orchestration) Wait() {
	orchestration.workerWG.Wait()
}

func (orchestration *phase2Orchestration) runFinalizeWorker(ctx context.Context) {
	defer orchestration.workerWG.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-orchestration.notify:
		}
		backoff := orchestration.workerCfg.InitialBackoff
		attempt := 0
		for {
			attempt++
			attemptCtx, cancel := context.WithTimeout(
				ctx,
				orchestration.workerCfg.AttemptTimeout,
			)
			finalized, err := orchestration.FinalizeIfIdle(attemptCtx)
			cancel()
			if err == nil && finalized {
				break
			}
			if orchestration.workerLog != nil &&
				(attempt == 1 || attempt&(attempt-1) == 0) {
				orchestration.workerLog.Warn(
					"retry phase2 finalization",
					"attempt", attempt,
					"backoff", backoff,
					"err", err,
				)
			}
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-orchestration.notify:
				if !timer.Stop() {
					<-timer.C
				}
			case <-timer.C:
			}
			backoff *= 2
			if backoff > orchestration.workerCfg.MaxBackoff {
				backoff = orchestration.workerCfg.MaxBackoff
			}
		}
	}
}

func (orchestration *phase2Orchestration) HandleFeatureResult(
	ctx context.Context,
	result *phase2.BoundFeatureResult,
) error {
	return orchestration.rescreener.HandleFeatureResult(ctx, result)
}

func (orchestration *phase2Orchestration) FinalizeIfIdle(
	ctx context.Context,
) (bool, error) {
	orchestration.gate.Lock()
	defer orchestration.gate.Unlock()
	if !orchestration.armed {
		return false, nil
	}
	return orchestration.finalizeLocked(ctx)
}

func (orchestration *phase2Orchestration) finalizeLocked(
	ctx context.Context,
) (bool, error) {
	finalized, err := orchestration.rescreener.FinalizeIfIdle(ctx)
	if err != nil || !finalized {
		return finalized, err
	}
	if orchestration.groupWriter == nil {
		return true, nil
	}
	generation := orchestration.rescreener.Progress().Generation
	if !orchestration.groupGenerationSet ||
		orchestration.groupGeneration != generation {
		orchestration.groupGeneration = generation
		orchestration.groupGenerationSet = true
		orchestration.groupKindIndex = 0
	}
	kinds := [...]string{"image", "video"}
	for orchestration.groupKindIndex < len(kinds) {
		kind := kinds[orchestration.groupKindIndex]
		if _, err := orchestration.groupWriter.RebuildGroups(ctx, kind); err != nil {
			return false, fmt.Errorf(
				"rebuild confirmed %s groups for generation %d: %w",
				kind,
				generation,
				err,
			)
		}
		orchestration.groupKindIndex++
	}
	return true, nil
}

func routeAgentMessage(
	processContext context.Context,
	machineID string,
	message any,
	dispatcher phase2MessageDispatcher,
	rescreener phase2RouteConsumer,
	tasks taskMessageConsumer,
	logger *slog.Logger,
	deleteConsumers ...deleteReportConsumer,
) {
	if report, ok := message.(*proto.DeleteReport); ok {
		for _, consumer := range deleteConsumers {
			if consumer != nil {
				consumer.HandleReport(machineID, report)
			}
		}
	}
	var (
		bound   *phase2.BoundFeatureResult
		bindErr error
	)
	if result, ok := message.(*proto.FeatureResult); ok {
		bound, bindErr = dispatcher.BindFeatureResult(machineID, result)
	}
	recognized := dispatcher.HandleMessage(machineID, message)
	if recognized && bound != nil && bindErr == nil {
		ctx, cancel := context.WithTimeout(processContext, 5*time.Second)
		err := rescreener.HandleFeatureResult(ctx, bound)
		cancel()
		if err != nil && logger != nil {
			logger.Error(
				"rescreen phase2 result",
				"machine_id", machineID,
				"task_id", bound.TaskID,
				"err", err,
			)
		}
	} else if bindErr != nil && logger != nil {
		logger.Error(
			"bind phase2 result",
			"machine_id", machineID,
			"err", bindErr,
		)
	}
	if recognized && isPhase2TerminalMessage(message) {
		if signaler, ok := rescreener.(interface{ SignalFinalize() }); ok {
			signaler.SignalFinalize()
		} else {
			ctx, cancel := context.WithTimeout(processContext, 5*time.Second)
			_, err := rescreener.FinalizeIfIdle(ctx)
			cancel()
			if err != nil && logger != nil {
				logger.Error(
					"finalize phase2 rescreen",
					"machine_id", machineID,
					"err", err,
				)
			}
		}
	}
	tasks.Dispatch(machineID, message)
}

func isPhase2TerminalMessage(message any) bool {
	switch value := message.(type) {
	case *proto.TaskDone:
		return true
	case *proto.TaskAck:
		return !value.Accepted
	default:
		return false
	}
}

func resumeAgentWork(
	ctx context.Context,
	machineID string,
	tasks pendingScanSource,
	sender agentMessageSender,
	dispatcher machinePendingDispatcher,
	logger *slog.Logger,
) {
	for _, task := range tasks.PendingScans(machineID) {
		if ctx.Err() != nil {
			return
		}
		task := task
		if err := sender.Send(machineID, proto.MsgScanTask, &task); err != nil &&
			logger != nil {
			logger.Warn(
				"resume scan",
				"machine_id", machineID,
				"task_id", task.TaskID,
				"err", err,
			)
		}
	}
	if err := dispatcher.DispatchMachinePending(
		ctx,
		machineID,
	); err != nil && ctx.Err() == nil && logger != nil {
		logger.Warn(
			"resume phase2",
			"machine_id", machineID,
			"err", err,
		)
	}
}

func reloadDispatchAndFinalize(
	ctx context.Context,
	orchestration *phase2Orchestration,
	autoDispatch bool,
) error {
	orchestration.gate.Lock()
	defer orchestration.gate.Unlock()
	priorGeneration := orchestration.rescreener.Progress().Generation
	priorArmed := orchestration.armed
	orchestration.armed = false
	if err := orchestration.rescreener.Reload(ctx); err != nil {
		if orchestration.rescreener.Progress().Generation == priorGeneration {
			orchestration.armed = priorArmed
		}
		return fmt.Errorf("reload phase2 candidates: %w", err)
	}
	if !autoDispatch {
		return nil
	}
	if err := orchestration.dispatcher.DispatchPending(ctx); err != nil {
		var admission interface{ DurablyAdmitted() bool }
		if errors.As(err, &admission) && admission.DurablyAdmitted() {
			orchestration.armed = true
		}
		return err
	}
	orchestration.armed = true
	orchestration.SignalFinalize()
	return nil
}

func serveAndDrain(
	processContext context.Context,
	cancelProcess context.CancelFunc,
	server guiHTTPServer,
	listener net.Listener,
	analysis analysisLifecycle,
	shutdownTimeout time.Duration,
) error {
	shutdownResult := make(chan error, 1)
	go func() {
		<-processContext.Done()
		analysis.BeginAnalysisShutdown()
		shutdownContext, cancelShutdown := context.WithTimeout(
			context.Background(),
			shutdownTimeout,
		)
		defer cancelShutdown()
		shutdownResult <- server.Shutdown(shutdownContext)
	}()

	serveErr := server.Serve(listener)
	analysis.BeginAnalysisShutdown()
	cancelProcess()
	analysis.WaitForAnalysis()
	shutdownErr := <-shutdownResult

	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("http server exited: %w", serveErr)
	}
	if shutdownErr != nil {
		return fmt.Errorf("http server shutdown: %w", shutdownErr)
	}
	return nil
}

func serveGUIAfterBind(
	processContext context.Context,
	cancelProcess context.CancelFunc,
	server guiHTTPServer,
	analysis analysisLifecycle,
	shutdownTimeout time.Duration,
	listenAddr string,
	noBrowser bool,
	logger *slog.Logger,
) error {
	listener, err := guiListen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("bind GUI listener: %w", err)
	}
	defer listener.Close()
	logger.Info("gui listening", "addr", listener.Addr().String())
	if !noBrowser {
		browserURL, urlErr := localBrowserURL(listenAddr)
		if urlErr != nil {
			logger.Warn("GUI browser URL unavailable", "err", urlErr)
		} else if openErr := guiOpenBrowser(browserURL); openErr != nil {
			logger.Warn("open GUI browser", "err", openErr)
		}
	}
	return serveAndDrain(processContext, cancelProcess, server, listener, analysis, shutdownTimeout)
}

func main() {
	if err := executeGUI(os.Args[1:]); err != nil {
		slog.Error("gui exited", "err", err)
		os.Exit(1)
	}
}

func executeGUI(args []string) error {
	err := run(args)
	if err != nil {
		guiShowStartupError("GUI 启动失败，请检查便携目录中的 gui.json 和 data\\logs\\gui.log。")
	}
	return err
}

func loadGUIRuntime(path string) (string, *config.GUIConfig, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", nil, fmt.Errorf("resolve config path: %w", err)
	}
	cfg, err := config.LoadGUI(absolute)
	if err != nil {
		return "", nil, err
	}
	return absolute, cfg, nil
}

func run(args []string) error {
	flags := flag.NewFlagSet("gui", flag.ContinueOnError)
	configPath := flags.String("config", "", "配置文件路径（默认：EXE 同目录 gui.json）")
	noBrowser := flags.Bool("no-browser", false, "不自动打开浏览器")
	if err := flags.Parse(args); err != nil {
		return err
	}

	executable, err := guiExecutablePath()
	if err != nil {
		return fmt.Errorf("resolve GUI executable path: %w", err)
	}
	runtimePaths, err := resolveGUIRuntimePaths(executable, *configPath)
	if err != nil {
		return err
	}
	logger, closeLogger, err := newGUIRuntimeLogger(runtimePaths.LogPath, os.Stdout)
	if err != nil {
		return err
	}
	defer closeLogger()
	cfg, err := config.LoadGUI(runtimePaths.ConfigPath)
	if err != nil {
		logger.Error("gui startup failed", "stage", "load config", "err", err)
		return fmt.Errorf("load config: %w", err)
	}
	configService, err := gui.NewGUIConfigService(runtimePaths.ConfigPath, cfg)
	if err != nil {
		logger.Error("gui startup failed", "stage", "initialize config service", "err", err)
		return fmt.Errorf("initialize GUI config service: %w", err)
	}
	pg, err := pgxpool.New(context.Background(), cfg.PGDSN)
	if err != nil {
		return fmt.Errorf("parse postgres DSN: %w", err)
	}
	defer pg.Close()
	pingContext, cancelPing := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelPing()
	if err := pg.Ping(pingContext); err != nil {
		return fmt.Errorf("postgres unreachable: %w", err)
	}
	cancelPing()

	processContext, cancelProcess := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer cancelProcess()
	tasks := gui.NewTaskRegistry(pg, logger)
	if err := tasks.Restore(processContext); err != nil {
		return fmt.Errorf("restore scan tasks: %w", err)
	}
	var phase2Dispatcher *phase2.Dispatcher
	var phase2Rescreener *phase2.Rescreener
	var phase2Router *phase2Orchestration
	var deleteService *gui.DeleteService
	pool := gui.NewPool(
		cfg.Agents,
		logger,
		func(machineID string, _ *gui.AgentConn, message any) {
			if phase2Dispatcher != nil && phase2Router != nil {
				routeAgentMessage(
					processContext,
					machineID,
					message,
					phase2Dispatcher,
					phase2Router,
					tasks,
					logger,
					deleteService,
				)
				return
			}
			tasks.Dispatch(machineID, message)
		},
	)
	deleteService, _ = newDeleteRuntime(
		pg,
		pool,
		time.Minute,
		time.Now,
		logger,
	)
	phase2Dispatcher = phase2.NewDispatcher(
		pg,
		pool,
		cfg.Phase2,
		logger,
	)
	if err := phase2Dispatcher.RestorePending(processContext); err != nil {
		return fmt.Errorf("restore phase2 tasks: %w", err)
	}
	phase2Rescreener = phase2.NewRescreener(pg, cfg.Phase2, logger)
	restoreContext, cancelRestore := context.WithTimeout(
		processContext,
		5*time.Minute,
	)
	if err := phase2Rescreener.Restore(restoreContext); err != nil {
		cancelRestore()
		return fmt.Errorf("restore phase2 rescreener: %w", err)
	}
	phase2Router = newPhase2Orchestration(
		phase2Rescreener,
		phase2Dispatcher,
		phase2.NewGroupRebuilder(pg),
	)
	phase2Router.Start(processContext, logger, phase2FinalizeWorkerConfig{})
	phase2Router.SignalFinalize()
	cancelRestore()
	defer phase2Dispatcher.Shutdown()
	defer pool.StopReconnects()
	pool.SetOnConnectContext(func(ctx context.Context, machineID string) {
		resumeAgentWork(
			ctx,
			machineID,
			tasks,
			pool,
			phase2Dispatcher,
			logger,
		)
	})
	pool.Start(processContext, time.Duration(cfg.HeartbeatS)*time.Second)
	analysisRunner := newPooledAnalysisRunner(
		processContext,
		pgxAnalysisPool{pool: pg},
		firstScreenConfig(cfg.FirstScreen),
		logger,
	)
	api := gui.NewAPI(pool, tasks, pg, analysisRunner)
	api.SetConfigService(configService)
	api.SetDeleteService(deleteService)
	api.SetAnalysisSuccessHook(func() error {
		hookContext, cancelHook := context.WithTimeout(
			processContext,
			5*time.Minute,
		)
		defer cancelHook()
		return reloadDispatchAndFinalize(
			hookContext,
			phase2Router,
			cfg.Phase2.AutoDispatch,
		)
	})

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           api.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	serveErr := serveGUIAfterBind(
		processContext,
		cancelProcess,
		server,
		api,
		5*time.Second,
		cfg.ListenAddr,
		*noBrowser,
		logger,
	)
	phase2Router.Wait()
	return serveErr
}
