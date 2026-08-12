package main

import (
	"context"
	"fmt"
	"log/slog"
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
	pg, err := pgxpool.New(ctx, cfg.PGDSN)
	if err != nil {
		return nil, fmt.Errorf("parse postgres DSN: %w", err)
	}
	var (
		pool             *gui.Pool
		phase2Dispatcher *phase2.Dispatcher
	)
	defer func() {
		if err == nil {
			return
		}
		if pool != nil {
			pool.StopReconnects()
		}
		if phase2Dispatcher != nil {
			phase2Dispatcher.Shutdown()
		}
		pg.Close()
	}()

	pingContext, cancelPing := context.WithTimeout(ctx, 5*time.Second)
	err = pg.Ping(pingContext)
	cancelPing()
	if err != nil {
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	tasks := gui.NewTaskRegistry(pg, logger)
	if err = tasks.Restore(ctx); err != nil {
		return nil, fmt.Errorf("restore scan tasks: %w", err)
	}

	var (
		phase2Router  *phase2Orchestration
		deleteService *gui.DeleteService
	)
	pool = gui.NewPool(
		cfg.Agents,
		logger,
		func(machineID string, _ *gui.AgentConn, message any) {
			if phase2Dispatcher != nil && phase2Router != nil {
				routeAgentMessage(
					ctx,
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
	if err = phase2Dispatcher.RestorePending(ctx); err != nil {
		return nil, fmt.Errorf("restore phase2 tasks: %w", err)
	}
	phase2Rescreener := phase2.NewRescreener(pg, cfg.Phase2, logger)
	restoreContext, cancelRestore := context.WithTimeout(ctx, 5*time.Minute)
	err = phase2Rescreener.Restore(restoreContext)
	cancelRestore()
	if err != nil {
		return nil, fmt.Errorf("restore phase2 rescreener: %w", err)
	}
	phase2Router = newPhase2Orchestration(
		phase2Rescreener,
		phase2Dispatcher,
		phase2.NewGroupRebuilder(pg),
	)
	phase2Router.Start(ctx, logger, phase2FinalizeWorkerConfig{})
	phase2Router.SignalFinalize()

	pool.SetOnConnectContext(func(connectContext context.Context, machineID string) {
		resumeAgentWork(
			connectContext,
			machineID,
			tasks,
			pool,
			phase2Dispatcher,
			logger,
		)
	})
	pool.Start(ctx, time.Duration(cfg.HeartbeatS)*time.Second)
	analysisRunner := newPooledAnalysisRunner(
		ctx,
		pgxAnalysisPool{pool: pg},
		firstScreenConfig(cfg.FirstScreen),
		logger,
	)
	api := gui.NewAPI(pool, tasks, pg, analysisRunner)
	api.SetDeleteService(deleteService)
	api.SetAnalysisSuccessHook(func() error {
		hookContext, cancelHook := context.WithTimeout(ctx, 5*time.Minute)
		defer cancelHook()
		return reloadDispatchAndFinalize(
			hookContext,
			phase2Router,
			cfg.Phase2.AutoDispatch,
		)
	})

	return &operationalRuntime{
		api: api,
		closeRuntime: func() {
			api.BeginAnalysisShutdown()
			api.WaitForAnalysis()
			phase2Router.Wait()
			pool.StopReconnects()
			phase2Dispatcher.Shutdown()
			pg.Close()
		},
	}, nil
}

var guiBuildOperationalRuntime = buildOperationalRuntime

func initializeOperationalRuntime(
	ctx context.Context,
	cfg *config.GUIConfig,
	host *gui.RuntimeHost,
	logger *slog.Logger,
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
	runtime.Close()
}
