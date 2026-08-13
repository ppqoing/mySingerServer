package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	trayapp "dedup/internal/nodetray/app"
	"dedup/internal/nodetray/bootstrap"
	trayconfig "dedup/internal/nodetray/config"
	"dedup/internal/nodetray/traymodel"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

var (
	errBackendNotStarted       = errors.New("backend_not_started")
	errBackendUnavailable      = errors.New("backend_unavailable")
	windowHideAdapter          = runtime.WindowHide
	eventsEmitAdapter          = runtime.EventsEmit
	openDirectoryDialogAdapter = runtime.OpenDirectoryDialog
)

type Backend struct {
	ctx             context.Context
	service         *trayapp.Service
	lifecycle       BackendLifecycle
	lifeMu          sync.Mutex
	started         bool
	closed          bool
	startup         BackendStartup
	closeErr        error
	quit            func(context.Context)
	webViewDataPath string
	exitAuthorized  atomic.Bool
}

type BackendLifecycle interface {
	Start(context.Context) (*bootstrap.Runtime, error)
	Close() error
}

type BackendStartup struct {
	Ready     bool
	Duplicate bool
	ErrorCode string
}

type backendContext struct {
	mu     sync.RWMutex
	active context.Context
	cancel context.CancelFunc
}

func NewBackend(service *trayapp.Service, lifecycle ...BackendLifecycle) *Backend {
	var selected BackendLifecycle
	if len(lifecycle) != 0 {
		selected = lifecycle[0]
	}
	return &Backend{
		ctx: &backendContext{}, service: service, lifecycle: selected,
		quit: func(ctx context.Context) { wailsQuitAdapter(ctx) },
	}
}

func (b *Backend) Startup(ctx context.Context) BackendStartup {
	if b == nil {
		return BackendStartup{ErrorCode: "runtime_start_failed"}
	}
	state, ok := b.ctx.(*backendContext)
	if !ok || state == nil {
		return BackendStartup{ErrorCode: "runtime_start_failed"}
	}
	b.lifeMu.Lock()
	defer b.lifeMu.Unlock()
	if b.closed {
		return BackendStartup{ErrorCode: "runtime_start_failed"}
	}
	state.activate(ctx)
	if b.started {
		return b.startup
	}
	b.started = true
	if b.lifecycle == nil {
		b.startup = BackendStartup{Ready: true}
		return b.startup
	}
	runtime, err := b.lifecycle.Start(ctx)
	if err != nil || runtime == nil {
		b.startup = BackendStartup{ErrorCode: "runtime_start_failed"}
		return b.startup
	}
	if runtime.Duplicate {
		b.startup = BackendStartup{Duplicate: true}
		return b.startup
	}
	b.startup = BackendStartup{Ready: true}
	return b.startup
}

func (b *Backend) Shutdown(context.Context) error {
	if b == nil {
		return nil
	}
	state, ok := b.ctx.(*backendContext)
	if !ok || state == nil {
		return nil
	}
	b.lifeMu.Lock()
	if b.closed {
		err := b.closeErr
		b.lifeMu.Unlock()
		return err
	}
	b.closed = true
	if b.lifecycle != nil {
		if err := b.lifecycle.Close(); err != nil {
			b.closeErr = errors.New("runtime_close_failed")
		}
	}
	err := b.closeErr
	b.lifeMu.Unlock()
	state.deactivate()
	return err
}

func (b *Backend) GetOverview() (traymodel.Overview, error) {
	ctx, service, err := b.ready()
	if err != nil {
		return traymodel.Overview{}, err
	}
	return service.GetOverview(ctx)
}

func (b *Backend) CreateLocalTask(value traymodel.LocalTaskCreate) traymodel.LocalTaskResult {
	ctx, service, err := b.ready()
	if err != nil {
		return traymodel.LocalTaskResult{ErrorCode: "backend_not_started", ErrorSummary: "本机控制台尚未启动"}
	}
	return service.CreateLocalTask(ctx, value)
}

func (b *Backend) ChooseLocalTaskRoot(currentPath string) traymodel.PathSelectionResult {
	if b == nil {
		return traymodel.PathSelectionResult{ErrorCode: "backend_not_started", ErrorSummary: "本机控制台尚未启动"}
	}
	state, ok := b.ctx.(*backendContext)
	if !ok || state == nil {
		return traymodel.PathSelectionResult{ErrorCode: "backend_not_started", ErrorSummary: "本机控制台尚未启动"}
	}
	ctx := state.snapshot()
	if ctx == nil {
		return traymodel.PathSelectionResult{ErrorCode: "backend_not_started", ErrorSummary: "本机控制台尚未启动"}
	}

	options := runtime.OpenDialogOptions{Title: "选择本地任务扫描目录"}
	if filepath.IsAbs(currentPath) {
		if info, err := os.Stat(currentPath); err == nil && info.IsDir() {
			options.DefaultDirectory = currentPath
		}
	}
	selected, err := openDirectoryDialogAdapter(ctx, options)
	if err != nil {
		return traymodel.PathSelectionResult{ErrorCode: "directory_dialog_failed", ErrorSummary: "无法打开目录选择窗口"}
	}
	if selected == "" {
		return traymodel.PathSelectionResult{OK: true, Cancelled: true}
	}
	if info, statErr := os.Stat(selected); statErr != nil || !info.IsDir() {
		return traymodel.PathSelectionResult{ErrorCode: "directory_dialog_failed", ErrorSummary: "无法选择目录"}
	}
	return traymodel.PathSelectionResult{OK: true, Path: selected}
}

func (b *Backend) ListLocalTasks(value traymodel.PageRequest) traymodel.LocalTaskPage {
	ctx, service, err := b.ready()
	if err != nil {
		return traymodel.LocalTaskPage{Tasks: []traymodel.LocalTask{}, ErrorCode: "backend_not_started", ErrorSummary: "本机控制台尚未启动"}
	}
	return service.ListLocalTasks(ctx, value)
}

func (b *Backend) StartLocalAnalysis(value traymodel.LocalAnalysisStart) traymodel.OperationResult {
	ctx, service, err := b.ready()
	if err != nil {
		return backendOperationError(err)
	}
	return service.StartLocalAnalysis(ctx, value)
}

func (b *Backend) ListLocalGroups(value traymodel.LocalGroupQuery) traymodel.LocalGroupPage {
	ctx, service, err := b.ready()
	if err != nil {
		return traymodel.LocalGroupPage{Groups: []traymodel.LocalGroup{}, ErrorCode: "backend_not_started", ErrorSummary: "本机控制台尚未启动"}
	}
	return service.ListLocalGroups(ctx, value)
}

func (b *Backend) SaveLocalReview(value traymodel.LocalReviewSave) traymodel.OperationResult {
	ctx, service, err := b.ready()
	if err != nil {
		return backendOperationError(err)
	}
	return service.SaveLocalReview(ctx, value)
}

func (b *Backend) PrepareLocalDelete(value traymodel.LocalDeletePrepare) traymodel.LocalDeletePreview {
	ctx, service, err := b.ready()
	if err != nil {
		return traymodel.LocalDeletePreview{Files: []traymodel.LocalDeleteFile{}, ErrorCode: "backend_not_started", ErrorSummary: "本机控制台尚未启动"}
	}
	return service.PrepareLocalDelete(ctx, value)
}

func (b *Backend) ExecuteLocalDelete(value traymodel.LocalDeleteExecute) traymodel.LocalDeleteBatch {
	ctx, service, err := b.ready()
	if err != nil {
		return traymodel.LocalDeleteBatch{Items: []traymodel.LocalDeleteItem{}, ErrorCode: "backend_not_started", ErrorSummary: "本机控制台尚未启动"}
	}
	return service.ExecuteLocalDelete(ctx, value)
}

func (b *Backend) GetLocalImagePreview(fileID int64) traymodel.ImagePreview {
	ctx, service, err := b.ready()
	if err != nil {
		return traymodel.ImagePreview{ErrorCode: "backend_not_started", ErrorSummary: "本机控制台尚未启动"}
	}
	return service.GetLocalImagePreview(ctx, fileID)
}

func (b *Backend) getOverviewWithContext(ctx context.Context) (traymodel.Overview, error) {
	_, service, err := b.ready()
	if err != nil {
		return traymodel.Overview{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return service.GetOverview(ctx)
}

func (b *Backend) GetAgentForm() (trayconfig.AgentForm, error) {
	ctx, service, err := b.ready()
	if err != nil {
		return trayconfig.AgentForm{}, err
	}
	return service.GetAgentForm(ctx)
}

func (b *Backend) ValidateAgent(value trayconfig.AgentForm) []trayconfig.FieldError {
	ctx, service, err := b.ready()
	if err != nil {
		return backendFieldError(err)
	}
	return service.ValidateAgent(ctx, value)
}

func (b *Backend) SaveAgent(value trayconfig.AgentForm) traymodel.ConfigApplyResult {
	ctx, service, err := b.ready()
	if err != nil {
		return backendConfigApplyError(err)
	}
	return service.SaveAgent(ctx, value)
}

func (b *Backend) SaveAndRestartAgent(value trayconfig.AgentForm) traymodel.ConfigApplyResult {
	ctx, service, err := b.ready()
	if err != nil {
		return backendConfigApplyError(err)
	}
	return service.SaveAndRestartAgent(ctx, value)
}

func (b *Backend) StartAgent() traymodel.OperationResult {
	ctx, service, err := b.ready()
	if err != nil {
		return backendOperationError(err)
	}
	return service.StartAgent(ctx)
}

func (b *Backend) StopAgent() traymodel.OperationResult {
	ctx, service, err := b.ready()
	if err != nil {
		return backendOperationError(err)
	}
	return service.StopAgent(ctx)
}

func (b *Backend) RestartAgent() traymodel.OperationResult {
	ctx, service, err := b.ready()
	if err != nil {
		return backendOperationError(err)
	}
	return service.RestartAgent(ctx)
}

func (b *Backend) ForceStopAgent() traymodel.OperationResult {
	ctx, service, err := b.ready()
	if err != nil {
		return backendOperationError(err)
	}
	return service.ForceStopAgent(ctx)
}

func (b *Backend) GetHelperForm() (trayconfig.HelperForm, error) {
	ctx, service, err := b.ready()
	if err != nil {
		return trayconfig.HelperForm{}, err
	}
	return service.GetHelperForm(ctx)
}

func (b *Backend) ValidateHelper(value trayconfig.HelperForm) []trayconfig.FieldError {
	ctx, service, err := b.ready()
	if err != nil {
		return backendFieldError(err)
	}
	return service.ValidateHelper(ctx, value)
}

func (b *Backend) SaveHelper(value trayconfig.HelperForm) traymodel.ConfigApplyResult {
	ctx, service, err := b.ready()
	if err != nil {
		return backendConfigApplyError(err)
	}
	return service.SaveHelper(ctx, value)
}

func (b *Backend) StartHelper() traymodel.OperationResult {
	ctx, service, err := b.ready()
	if err != nil {
		return backendOperationError(err)
	}
	return service.StartHelper(ctx)
}

func (b *Backend) StopHelper() traymodel.OperationResult {
	ctx, service, err := b.ready()
	if err != nil {
		return backendOperationError(err)
	}
	return service.StopHelper(ctx)
}

func (b *Backend) RestartHelper() traymodel.OperationResult {
	ctx, service, err := b.ready()
	if err != nil {
		return backendOperationError(err)
	}
	return service.RestartHelper(ctx)
}

func (b *Backend) ForceStopHelper() traymodel.OperationResult {
	ctx, service, err := b.ready()
	if err != nil {
		return backendOperationError(err)
	}
	return service.ForceStopHelper(ctx)
}

func (b *Backend) GetTraySettings() (traymodel.TraySettings, error) {
	ctx, service, err := b.ready()
	if err != nil {
		return traymodel.TraySettings{}, err
	}
	return service.GetTraySettings(ctx)
}

func (b *Backend) SaveTraySettings(value traymodel.TraySettings) traymodel.OperationResult {
	ctx, service, err := b.ready()
	if err != nil {
		return backendOperationError(err)
	}
	return service.SaveTraySettings(ctx, value)
}

func (b *Backend) OpenLocation(kind traymodel.LocationKind) traymodel.OperationResult {
	ctx, service, err := b.ready()
	if err != nil {
		return backendOperationError(err)
	}
	return service.OpenLocation(ctx, kind)
}

func (b *Backend) ForceExitAll() traymodel.ForceExitResult {
	ctx, service, err := b.ready()
	if err != nil {
		return backendForceExitError(err)
	}
	result := service.ForceExitAll(ctx)
	if result.OK {
		b.authorizeAndQuit(ctx)
	}
	return result
}

func (b *Backend) authorizeAndQuit(ctx context.Context) {
	if b == nil {
		return
	}
	b.exitAuthorized.Store(true)
	if b.quit != nil {
		b.quit(ctx)
	}
}

func (b *Backend) onBeforeClose(ctx context.Context) bool {
	if b != nil && b.exitAuthorized.Load() {
		return false
	}
	settings, err := b.GetTraySettings()
	if err != nil || settings.CloseToTray {
		windowHideAdapter(ctx)
		return true
	}
	eventsEmitAdapter(ctx, "window-close-requested")
	return true
}

func (b *Backend) ready() (context.Context, *trayapp.Service, error) {
	if b == nil {
		return nil, nil, errBackendNotStarted
	}
	state, ok := b.ctx.(*backendContext)
	if !ok || state == nil {
		return nil, nil, errBackendNotStarted
	}
	ctx := state.snapshot()
	if ctx == nil {
		return nil, nil, errBackendNotStarted
	}
	if b.service == nil {
		return nil, nil, errBackendUnavailable
	}
	if b.lifecycle != nil {
		b.lifeMu.Lock()
		ready := b.started && !b.closed && b.startup.Ready
		b.lifeMu.Unlock()
		if !ready {
			return nil, nil, errBackendNotStarted
		}
	}
	return ctx, b.service, nil
}

func backendFieldError(err error) []trayconfig.FieldError {
	if errors.Is(err, errBackendUnavailable) {
		return []trayconfig.FieldError{{Field: "backend", Code: "backend_unavailable", Message: "后端不可用"}}
	}
	return []trayconfig.FieldError{{Field: "backend", Code: "backend_not_started", Message: "后端尚未启动"}}
}

func backendOperationError(err error) traymodel.OperationResult {
	if errors.Is(err, errBackendUnavailable) {
		return traymodel.OperationResult{ErrorCode: "backend_unavailable"}
	}
	return traymodel.OperationResult{ErrorCode: "backend_not_started"}
}

func backendConfigApplyError(err error) traymodel.ConfigApplyResult {
	if errors.Is(err, errBackendUnavailable) {
		return traymodel.ConfigApplyResult{ErrorCode: "backend_unavailable"}
	}
	return traymodel.ConfigApplyResult{ErrorCode: "backend_not_started"}
}

func backendForceExitError(err error) traymodel.ForceExitResult {
	if errors.Is(err, errBackendUnavailable) {
		return traymodel.ForceExitResult{FailedComponents: []string{"service"}, ErrorCode: "backend_unavailable", ErrorSummary: "后端不可用"}
	}
	return traymodel.ForceExitResult{FailedComponents: []string{"service"}, ErrorCode: "backend_not_started", ErrorSummary: "后端尚未启动"}
}

func (c *backendContext) activate(parent context.Context) {
	if parent == nil {
		parent = context.Background()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != nil && c.active.Err() == nil {
		return
	}
	c.active, c.cancel = context.WithCancel(parent)
}

func (c *backendContext) deactivate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		c.cancel()
	}
	c.cancel = nil
}

func (c *backendContext) snapshot() context.Context {
	active := c.current()
	if active == nil || active.Err() != nil {
		return nil
	}
	return active
}

func (c *backendContext) current() context.Context {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.active
}

func (c *backendContext) Deadline() (time.Time, bool) {
	if ctx := c.current(); ctx != nil {
		return ctx.Deadline()
	}
	return time.Time{}, false
}

func (c *backendContext) Done() <-chan struct{} {
	if ctx := c.current(); ctx != nil {
		return ctx.Done()
	}
	return nil
}

func (c *backendContext) Err() error {
	if ctx := c.current(); ctx != nil {
		return ctx.Err()
	}
	return nil
}

func (c *backendContext) Value(key any) any {
	if ctx := c.current(); ctx != nil {
		return ctx.Value(key)
	}
	return nil
}
