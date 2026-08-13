package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"dedup/internal/config"
	"dedup/internal/gui"
	"dedup/internal/phase2"
)

type operationalRuntime struct {
	api          *gui.API
	closeOnce    sync.Once
	closeRuntime func()
}

type operationalRuntimeResources interface {
	Ping(context.Context) error
	RestoreTasks(context.Context) error
	RestorePhase2(context.Context) error
	Start(context.Context) error
	API() *gui.API
	BeginAnalysisShutdown()
	WaitForAnalysis()
	WaitForPhase2()
	StopPool()
	ShutdownPhase2()
	ClosePostgres()
}

type postgresOperationalRuntimeResources struct {
	cfg    *config.GUIConfig
	logger *slog.Logger
	pg     *pgxpool.Pool

	tasks            *gui.TaskRegistry
	pool             *gui.Pool
	deleteService    *gui.DeleteService
	phase2Dispatcher *phase2.Dispatcher
	phase2Router     *phase2Orchestration
	api              *gui.API
}

func newOperationalRuntimeResources(
	ctx context.Context,
	cfg *config.GUIConfig,
	logger *slog.Logger,
) (operationalRuntimeResources, error) {
	if strings.TrimSpace(cfg.PGDSN) == "" {
		return nil, gui.ErrPostgresNotConfigured
	}
	pg, err := pgxpool.New(ctx, cfg.PGDSN)
	if err != nil {
		return nil, err
	}
	return &postgresOperationalRuntimeResources{
		cfg:    cfg,
		logger: logger,
		pg:     pg,
	}, nil
}

var guiNewOperationalRuntimeResources = newOperationalRuntimeResources

func (runtime *operationalRuntime) API() *gui.API {
	if runtime == nil {
		return nil
	}
	return runtime.api
}

func (runtime *operationalRuntime) Close() {
	if runtime == nil {
		return
	}
	runtime.closeOnce.Do(func() {
		if runtime.closeRuntime != nil {
			runtime.closeRuntime()
		}
	})
}

func buildOperationalRuntime(
	ctx context.Context,
	cfg *config.GUIConfig,
	logger *slog.Logger,
) (_ *operationalRuntime, err error) {
	resources, err := guiNewOperationalRuntimeResources(ctx, cfg, logger)
	if err != nil {
		return nil, fmt.Errorf("parse postgres DSN: %w", err)
	}
	defer func() {
		if err != nil {
			closeOperationalRuntimeResources(resources)
		}
	}()
	if err = resources.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if err = resources.RestoreTasks(ctx); err != nil {
		return nil, fmt.Errorf("restore scan tasks: %w", err)
	}
	if err = resources.RestorePhase2(ctx); err != nil {
		return nil, fmt.Errorf("restore phase2 runtime: %w", err)
	}
	if err = resources.Start(ctx); err != nil {
		return nil, fmt.Errorf("start operational runtime: %w", err)
	}
	return &operationalRuntime{
		api: resources.API(),
		closeRuntime: func() {
			closeOperationalRuntimeResources(resources)
		},
	}, nil
}

func closeOperationalRuntimeResources(resources operationalRuntimeResources) {
	resources.BeginAnalysisShutdown()
	resources.WaitForAnalysis()
	resources.WaitForPhase2()
	resources.StopPool()
	resources.ShutdownPhase2()
	resources.ClosePostgres()
}

func (resources *postgresOperationalRuntimeResources) Ping(ctx context.Context) error {
	pingContext, cancelPing := context.WithTimeout(ctx, 5*time.Second)
	defer cancelPing()
	return resources.pg.Ping(pingContext)
}

func (resources *postgresOperationalRuntimeResources) RestoreTasks(ctx context.Context) error {
	resources.tasks = gui.NewTaskRegistry(resources.pg, resources.logger)
	return resources.tasks.Restore(ctx)
}

func (resources *postgresOperationalRuntimeResources) RestorePhase2(ctx context.Context) error {
	resources.pool = gui.NewPool(
		resources.cfg.Agents,
		resources.logger,
		func(machineID string, _ *gui.AgentConn, message any) {
			if resources.phase2Dispatcher != nil && resources.phase2Router != nil {
				routeAgentMessage(
					ctx,
					machineID,
					message,
					resources.phase2Dispatcher,
					resources.phase2Router,
					resources.tasks,
					resources.logger,
					resources.deleteService,
				)
				return
			}
			resources.tasks.Dispatch(machineID, message)
		},
	)
	resources.deleteService, _ = newDeleteRuntime(
		resources.pg,
		resources.pool,
		time.Minute,
		time.Now,
		resources.logger,
	)
	resources.phase2Dispatcher = phase2.NewDispatcher(
		resources.pg,
		resources.pool,
		resources.cfg.Phase2,
		resources.logger,
	)
	if err := resources.phase2Dispatcher.RestorePending(ctx); err != nil {
		return fmt.Errorf("restore phase2 tasks: %w", err)
	}
	phase2Rescreener := phase2.NewRescreener(
		resources.pg,
		resources.cfg.Phase2,
		resources.logger,
	)
	restoreContext, cancelRestore := context.WithTimeout(ctx, 5*time.Minute)
	err := phase2Rescreener.Restore(restoreContext)
	cancelRestore()
	if err != nil {
		return fmt.Errorf("restore phase2 rescreener: %w", err)
	}
	resources.phase2Router = newPhase2Orchestration(
		phase2Rescreener,
		resources.phase2Dispatcher,
		phase2.NewGroupRebuilder(resources.pg),
	)
	return nil
}

func (resources *postgresOperationalRuntimeResources) Start(ctx context.Context) error {
	resources.phase2Router.Start(ctx, resources.logger, phase2FinalizeWorkerConfig{})
	resources.phase2Router.SignalFinalize()
	resources.pool.SetOnConnectContext(func(connectContext context.Context, machineID string) {
		resumeAgentWork(
			connectContext,
			machineID,
			resources.tasks,
			resources.pool,
			resources.phase2Dispatcher,
			resources.logger,
		)
	})
	resources.pool.Start(ctx, time.Duration(resources.cfg.HeartbeatS)*time.Second)
	analysisRunner := newPooledAnalysisRunner(
		ctx,
		pgxAnalysisPool{pool: resources.pg},
		firstScreenConfig(resources.cfg.FirstScreen),
		resources.logger,
	)
	resources.api = gui.NewAPI(
		resources.pool,
		resources.tasks,
		resources.pg,
		analysisRunner,
	)
	resources.api.SetDeleteService(resources.deleteService)
	resources.api.SetAnalysisSuccessHook(func() error {
		hookContext, cancelHook := context.WithTimeout(ctx, 5*time.Minute)
		defer cancelHook()
		return reloadDispatchAndFinalize(
			hookContext,
			resources.phase2Router,
			resources.cfg.Phase2.AutoDispatch,
		)
	})
	return nil
}

func (resources *postgresOperationalRuntimeResources) API() *gui.API {
	return resources.api
}

func (resources *postgresOperationalRuntimeResources) BeginAnalysisShutdown() {
	if resources.api != nil {
		resources.api.BeginAnalysisShutdown()
	}
}

func (resources *postgresOperationalRuntimeResources) WaitForAnalysis() {
	if resources.api != nil {
		resources.api.WaitForAnalysis()
	}
}

func (resources *postgresOperationalRuntimeResources) WaitForPhase2() {
	if resources.phase2Router != nil {
		resources.phase2Router.Wait()
	}
}

func (resources *postgresOperationalRuntimeResources) StopPool() {
	if resources.pool != nil {
		resources.pool.StopReconnects()
	}
}

func (resources *postgresOperationalRuntimeResources) ShutdownPhase2() {
	if resources.phase2Dispatcher != nil {
		resources.phase2Dispatcher.Shutdown()
	}
}

func (resources *postgresOperationalRuntimeResources) ClosePostgres() {
	resources.pg.Close()
}

var guiBuildOperationalRuntime = buildOperationalRuntime

func initializeOperationalRuntime(
	ctx context.Context,
	cfg *config.GUIConfig,
	host *gui.RuntimeHost,
	logger *slog.Logger,
	httpDrained <-chan struct{},
) {
	runtime, err := guiBuildOperationalRuntime(ctx, cfg, logger)
	if err != nil {
		failure := gui.ClassifyRuntimeFailure(err)
		host.SetDatabaseFailure(err)
		logger.Error(
			"operational runtime unavailable",
			"code", failure.Code,
			"summary", failure.Summary,
		)
		return
	}
	if ctx.Err() != nil {
		runtime.Close()
		return
	}
	host.Install(runtime.API())
	<-ctx.Done()
	if httpDrained != nil {
		<-httpDrained
	}
	runtime.Close()
}
