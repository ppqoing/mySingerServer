package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dedup/internal/nodetray/traymodel"
	"dedup/internal/nodetray/windows/singleinstance"
)

const hiddenRecoveryInterval = 10 * time.Second

type Paths struct {
	TraySettings string
	AgentConfig  string
	HelperConfig string
}

type PathResolver interface {
	Resolve(context.Context) (Paths, error)
}
type FinalPathResolver interface{ Final(string) (string, error) }

// OSFinalPathResolver is the fail-closed production default. Every existing
// path component is inspected for reparse points. A completely missing suffix
// is preserved as the fixed first-run target for the component-specific writer.
type OSFinalPathResolver struct{}

func (OSFinalPathResolver) Final(path string) (string, error) {
	return resolveOSFinalPath(path)
}

type SettingsLoader interface {
	LoadTraySettings() (traymodel.TraySettings, error)
}
type Lease interface{ Close() error }
type InstanceService interface {
	AcquireTray(context.Context) (Lease, error)
	ListenActivation(context.Context, func()) (Closer, error)
	SignalExisting(context.Context) error
}

type Managed interface {
	Adopt(context.Context) traymodel.OperationResult
	Start(context.Context) traymodel.OperationResult
	Refresh(context.Context) traymodel.ComponentState
}

type Factory interface {
	NewAgent(context.Context, Paths) (Managed, error)
	NewHelper(context.Context, Paths) (Managed, error)
}

type TaskRunner interface{ Run(context.Context) error }
type Closer interface{ Close() error }
type RefreshScheduler interface {
	Start(context.Context, time.Duration, time.Duration, func(context.Context)) (Closer, error)
}
type UI interface{ Ready(context.Context) error }
type AttentionSink interface {
	Required(component, code, summary string)
}

type Dependencies struct {
	Paths      PathResolver
	FinalPaths FinalPathResolver
	Settings   SettingsLoader
	Instance   InstanceService
	Factory    Factory
	Task       TaskRunner
	Scheduler  RefreshScheduler
	UI         UI
	Attention  AttentionSink
	Show       func()
}

type Runtime struct {
	Duplicate bool
	lease     Lease
	listener  Closer
	timer     Closer
	closeOnce sync.Once
	closeErr  error
}

func Start(ctx context.Context, dependencies Dependencies) (*Runtime, error) {
	if ctx == nil {
		return nil, errors.New("bootstrap: context is required")
	}
	if dependencies.Paths == nil {
		return nil, errors.New("bootstrap: path resolver is required")
	}
	paths, err := dependencies.Paths.Resolve(ctx)
	if err != nil || !validPaths(paths) {
		return nil, errors.New("bootstrap: fixed deployment paths are invalid")
	}
	finalResolver := dependencies.FinalPaths
	if finalResolver == nil {
		finalResolver = OSFinalPathResolver{}
	}
	paths, err = resolveFinalPaths(paths, finalResolver)
	if err != nil {
		return nil, errors.New("bootstrap: fixed deployment final paths are invalid")
	}
	if dependencies.Settings == nil {
		return nil, errors.New("bootstrap: settings loader is required")
	}
	settings, err := dependencies.Settings.LoadTraySettings()
	if err != nil || settings.Validate() != nil {
		return nil, errors.New("bootstrap: tray settings are invalid")
	}
	if dependencies.Instance == nil {
		return nil, errors.New("bootstrap: instance service is required")
	}
	lease, err := dependencies.Instance.AcquireTray(ctx)
	if errors.Is(err, singleinstance.ErrAlreadyExists) {
		if lease != nil {
			_ = lease.Close()
		}
		if signalErr := dependencies.Instance.SignalExisting(ctx); signalErr != nil {
			return nil, errors.New("bootstrap: existing tray activation failed")
		}
		return &Runtime{Duplicate: true}, nil
	}
	if err != nil || lease == nil {
		if lease != nil {
			_ = lease.Close()
		}
		return nil, errors.New("bootstrap: tray lease failed")
	}
	runtime := &Runtime{lease: lease}
	fail := func(err error) (*Runtime, error) { _ = runtime.Close(); return nil, err }
	if dependencies.Show == nil {
		return fail(errors.New("bootstrap: activation callback is required"))
	}
	listener, listenErr := dependencies.Instance.ListenActivation(ctx, dependencies.Show)
	if listener != nil {
		runtime.listener = listener
	}
	if listenErr != nil || listener == nil {
		return fail(errors.New("bootstrap: activation listener failed"))
	}
	if dependencies.Factory == nil || dependencies.Scheduler == nil || dependencies.UI == nil || dependencies.Attention == nil {
		return fail(errors.New("bootstrap: required dependency is unavailable"))
	}

	agent, agentErr := dependencies.Factory.NewAgent(ctx, paths)
	if agentErr != nil || agent == nil {
		dependencies.Attention.Required("agent", "unavailable", "Agent 初始化失败")
	}
	helper, helperErr := dependencies.Factory.NewHelper(ctx, paths)
	if helperErr != nil || helper == nil {
		dependencies.Attention.Required("helper", "unavailable", "Helper 初始化失败")
	}
	if agent != nil {
		reportOperation(dependencies.Attention, "agent", agent.Adopt(ctx), "Agent 认领失败")
	}
	if helper != nil {
		reportOperation(dependencies.Attention, "helper", helper.Adopt(ctx), "Helper 认领失败")
	}
	if settings.AgentStartMode == traymodel.StartAutomatic && agent != nil {
		reportOperation(dependencies.Attention, "agent", agent.Start(ctx), "Agent 自动启动失败")
	}
	if settings.HelperEnabled && settings.HelperStartMode == traymodel.StartAutomatic {
		if dependencies.Task == nil {
			dependencies.Attention.Required("helper", "task_unavailable", "Helper 固定任务不可用")
		} else if err := dependencies.Task.Run(ctx); err != nil {
			dependencies.Attention.Required("helper", "task_failed", "Helper 固定任务启动失败")
		}
	}
	refresh := func(refreshCtx context.Context) {
		if refreshCtx == nil {
			refreshCtx = context.Background()
		}
		if agent != nil {
			_ = agent.Refresh(refreshCtx)
		}
		if helper != nil {
			_ = helper.Refresh(refreshCtx)
		}
	}
	timer, scheduleErr := dependencies.Scheduler.Start(ctx, time.Duration(settings.RefreshIntervalSeconds)*time.Second, hiddenRecoveryInterval, refresh)
	if scheduleErr != nil {
		if timer != nil {
			_ = timer.Close()
		}
		dependencies.Attention.Required("tray", "refresh_failed", "状态刷新调度失败")
	} else {
		runtime.timer = timer
	}
	if err := dependencies.UI.Ready(ctx); err != nil {
		return fail(errors.New("bootstrap: tray UI startup failed"))
	}
	return runtime, nil
}

func (runtime *Runtime) Close() error {
	if runtime == nil {
		return nil
	}
	runtime.closeOnce.Do(func() {
		var errs []error
		if runtime.timer != nil {
			if err := runtime.timer.Close(); err != nil {
				errs = append(errs, errors.New("timer_close_failed"))
			}
		}
		if runtime.listener != nil {
			if err := runtime.listener.Close(); err != nil {
				errs = append(errs, errors.New("activation_close_failed"))
			}
		}
		if runtime.lease != nil {
			if err := runtime.lease.Close(); err != nil {
				errs = append(errs, errors.New("lease_close_failed"))
			}
		}
		runtime.closeErr = errors.Join(errs...)
	})
	return runtime.closeErr
}

func reportOperation(attention AttentionSink, component string, result traymodel.OperationResult, summary string) {
	if result.OK {
		return
	}
	code := result.ErrorCode
	if code == "" {
		code = "operation_failed"
	}
	attention.Required(component, stableText(code), summary)
}

func stableText(value string) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
	if value == "" {
		return "operation_failed"
	}
	return value
}

func validPaths(paths Paths) bool {
	values := []string{paths.TraySettings, paths.AgentConfig, paths.HelperConfig}
	for i, value := range values {
		if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value || filepath.Base(value) == "." {
			return false
		}
		for j := 0; j < i; j++ {
			if pathsOverlap(value, values[j]) {
				return false
			}
		}
	}
	return true
}

func resolveFinalPaths(paths Paths, resolver FinalPathResolver) (Paths, error) {
	if resolver == nil {
		return Paths{}, errors.New("bootstrap: final path resolver is required")
	}
	raw := []string{paths.TraySettings, paths.AgentConfig, paths.HelperConfig}
	resolved := make([]string, len(raw))
	for index, value := range raw {
		finalParent, err := resolver.Final(filepath.Dir(value))
		if err != nil || !validAbsolutePath(finalParent) {
			return Paths{}, errors.New("bootstrap: final parent path is unavailable")
		}
		finalValue, err := resolver.Final(value)
		if errors.Is(err, os.ErrNotExist) {
			finalValue = filepath.Join(finalParent, filepath.Base(value))
		} else if err != nil {
			return Paths{}, errors.New("bootstrap: final path is unavailable")
		}
		if !validAbsolutePath(finalValue) || !strictlyBelow(finalValue, finalParent) {
			return Paths{}, errors.New("bootstrap: final path escaped its fixed parent")
		}
		resolved[index] = filepath.Clean(finalValue)
	}
	for i := range resolved {
		for j := 0; j < i; j++ {
			if pathsOverlap(resolved[i], resolved[j]) {
				return Paths{}, errors.New("bootstrap: final paths overlap")
			}
		}
	}
	return Paths{TraySettings: resolved[0], AgentConfig: resolved[1], HelperConfig: resolved[2]}, nil
}

func validAbsolutePath(value string) bool {
	return value != "" && filepath.IsAbs(value) && filepath.Clean(value) == value && filepath.Base(value) != "."
}

func pathsOverlap(left, right string) bool {
	return sameOrBelowPath(left, right) || sameOrBelowPath(right, left)
}

func strictlyBelow(path, root string) bool {
	relative, err := filepath.Rel(strings.ToLower(filepath.Clean(root)), strings.ToLower(filepath.Clean(path)))
	return err == nil && relative != "." && relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func sameOrBelowPath(path, root string) bool {
	relative, err := filepath.Rel(strings.ToLower(filepath.Clean(root)), strings.ToLower(filepath.Clean(path)))
	return err == nil && !filepath.IsAbs(relative) && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))))
}
