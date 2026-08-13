package main

import (
	"context"
	"embed"
	"encoding/hex"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dedup/internal/nodetray/traymodel"
	traynative "dedup/internal/nodetray/windows/tray"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

var errCompositionUnavailable = errors.New("composition_unavailable")

type startupStageError struct {
	code string
}

func (e *startupStageError) Error() string { return e.code }

var compositionFailureCodes = map[string]string{
	"production composition: required dependency unavailable":                   "required_dependency_unavailable",
	"production composition: fixed authority invalid":                           "fixed_authority_invalid",
	"production composition: fixed locations invalid":                           "fixed_locations_invalid",
	"production composition: runtime unavailable":                               "runtime_unavailable",
	"production composition: tray settings unavailable":                         "tray_settings_unavailable",
	"production composition: current process identity unavailable":              "process_identity_unavailable",
	"production composition: portable layout unavailable":                       "portable_layout_unavailable",
	"production composition: portable tray executable unavailable":              "portable_tray_executable_unavailable",
	"production composition: current executable is outside portable deployment": "outside_portable_deployment",
	"production composition: current user identity unavailable":                 "user_identity_unavailable",
	"production composition: configuration store unavailable":                   "configuration_store_unavailable",
	"production composition: task service unavailable":                          "task_service_unavailable",
	"production composition: login-start service unavailable":                   "login_start_service_unavailable",
	"production composition: elevation client unavailable":                      "elevation_client_unavailable",
	"production composition: process handle inspector unavailable":              "process_handle_inspector_unavailable",
	"production composition: portable data invalid":                             "portable_data_unavailable",
	"production composition: machine identity unavailable":                      "machine_identity_unavailable",
	"production composition: Windows dependencies unavailable":                  "windows_dependencies_unavailable",
	"production composition: shared component factory unavailable":              "component_factory_unavailable",
	"production composition: Wails context unavailable":                         "wails_context_unavailable",
	"production composition: instance context unavailable":                      "instance_context_unavailable",
	"production composition: activation dependencies unavailable":               "activation_dependencies_unavailable",
}

//go:embed all:frontend/dist
var embeddedFrontend embed.FS

var frontendAssets = mustFrontendAssets()

type launchMode struct {
	elevated   bool
	background bool
	pipe       string
	nonce      string
}

var (
	// Platform composition files replace these fail-closed defaults during
	// package initialization; installing the function values performs no OS work.
	runElevatedOnce          = func(string, string) error { return errCompositionUnavailable }
	composeBackend           = func() (*Backend, error) { return nil, errCompositionUnavailable }
	runNormalTray            = runNormalWails
	wailsRunAdapter          = wails.Run
	startupFailureLogAdapter = func(code string) { log.Print(code) }
	trayStartAdapter         = traynative.Start
	trayMonitorTickerAdapter = func(duration time.Duration) trayMonitorTicker {
		return &systemTrayMonitorTicker{ticker: time.NewTicker(duration)}
	}
	windowShowAdapter       = runtime.WindowShow
	windowUnminimiseAdapter = runtime.WindowUnminimise
	windowCenterAdapter     = runtime.WindowCenter
	wailsQuitAdapter        = runtime.Quit
	logWarningAdapter       = runtime.LogWarning
)

func main() {
	if executeMain(os.Args[1:]) != 0 {
		os.Exit(1)
	}
}

func executeMain(args []string) int {
	if err := run(args); err != nil {
		startupFailureLogAdapter("nodetray_start_failed code=" + stableStartupFailureCode(err))
		return 1
	}
	return 0
}

func stableStartupFailureCode(err error) string {
	var stageError *startupStageError
	if errors.As(err, &stageError) && stageError != nil && stageError.code != "" {
		return stageError.code
	}
	return "startup_unavailable"
}

func compositionFailureCode(err error) string {
	if err == nil {
		return "composition_unavailable"
	}
	if code, ok := compositionFailureCodes[err.Error()]; ok {
		return code
	}
	return "composition_unavailable"
}

func run(args []string) error {
	mode, err := parseLaunchMode(args)
	if err != nil {
		return err
	}
	if mode.elevated {
		return runElevatedOnce(mode.pipe, mode.nonce)
	}
	return runNormalTray(mode.background)
}

func parseLaunchMode(args []string) (launchMode, error) {
	if len(args) == 0 {
		return launchMode{}, nil
	}
	if len(args) == 1 && args[0] == "--background" {
		return launchMode{background: true}, nil
	}
	if len(args) != 5 || args[0] != "--elevated-once" || args[1] != "--pipe" || args[3] != "--nonce" {
		return launchMode{}, errors.New("invalid_arguments")
	}
	nonce := args[4]
	if len(nonce) != 64 || strings.ToLower(nonce) != nonce {
		return launchMode{}, errors.New("invalid_nonce")
	}
	decoded, err := hex.DecodeString(nonce)
	if err != nil || len(decoded) != 32 {
		return launchMode{}, errors.New("invalid_nonce")
	}
	pipe := `\\.\pipe\mysingerserver-elevate-` + nonce
	if args[2] != pipe {
		return launchMode{}, errors.New("invalid_pipe")
	}
	return launchMode{elevated: true, pipe: pipe, nonce: nonce}, nil
}

func runNormalWails(background bool) error {
	backend, err := composeBackend()
	if err != nil {
		return &startupStageError{code: compositionFailureCode(err)}
	}
	if backend == nil || backend.service == nil {
		return &startupStageError{code: "composition_unavailable"}
	}
	if backend.webViewDataPath == "" || !filepath.IsAbs(backend.webViewDataPath) {
		return &startupStageError{code: "portable_data_unavailable"}
	}
	if err := wailsRunAdapter(newWailsOptions(frontendAssets, backend, backend.webViewDataPath, background)); err != nil {
		return &startupStageError{code: "wails_run_failed"}
	}
	return nil
}

func newWailsOptions(assets fs.FS, backend *Backend, userDataPath string, backgroundMode ...bool) *options.App {
	background := len(backgroundMode) != 0 && backgroundMode[0]
	var trayMu sync.Mutex
	var trayController traynative.Controller
	var statusMonitor *trayMonitorLoop
	notifications := &trayNotificationTarget{}
	onStartup := func(ctx context.Context) {
		startup := backend.Startup(ctx)
		if startup.Duplicate {
			backend.authorizeAndQuit(ctx)
			return
		}
		if !startup.Ready {
			reportRuntimeAttention(ctx, "runtime_start_failed", "节点运行时启动失败，请重启托盘程序。")
			return
		}
		controller, err := startTraySafely(newNativeTrayOptions(ctx, backend, notifications.Notify))
		if err != nil || controller == nil {
			reportTrayAttention(ctx, "tray_unavailable")
			return
		}
		notifications.Set(controller)
		monitor := startTrayStatusMonitor(ctx, backend, notifications.Notify)
		trayMu.Lock()
		trayController = controller
		statusMonitor = monitor
		trayMu.Unlock()
		if background {
			windowHideAdapter(ctx)
		}
	}
	onShutdown := func(ctx context.Context) {
		trayMu.Lock()
		controller := trayController
		monitor := statusMonitor
		trayController = nil
		statusMonitor = nil
		trayMu.Unlock()
		if monitor != nil {
			monitor.Stop()
		}
		notifications.Clear()
		if controller != nil {
			if err := controller.Close(); err != nil {
				reportTrayAttention(ctx, "tray_close_failed")
			}
		}
		if err := backend.Shutdown(ctx); err != nil {
			reportRuntimeAttention(ctx, "runtime_close_failed", "节点运行时关闭失败，请重启托盘程序。")
		}
	}
	return &options.App{
		Title:                    "媒体节点控制台",
		Width:                    1080,
		Height:                   720,
		MinWidth:                 860,
		MinHeight:                600,
		HideWindowOnClose:        false,
		BackgroundColour:         options.NewRGBA(247, 248, 250, 255),
		AssetServer:              &assetserver.Options{Assets: assets},
		OnStartup:                onStartup,
		OnShutdown:               onShutdown,
		OnBeforeClose:            backend.onBeforeClose,
		Bind:                     []interface{}{backend},
		EnableDefaultContextMenu: false,
		BindingsAllowedOrigins:   "",
		Windows: &windows.Options{
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
			WebviewUserDataPath:  userDataPath,
			Theme:                windows.Light,
			BackdropType:         windows.None,
			Messages: &windows.Messages{
				InstallationRequired: "运行此程序需要 WebView2 Runtime。请选择确定安装微软官方 WebView2 Runtime。",
				UpdateRequired:       "WebView2 Runtime 需要更新。请选择确定安装微软官方 WebView2 Runtime 更新。",
				MissingRequirements:  "缺少 WebView2 Runtime 运行条件",
				Webview2NotInstalled: "尚未安装 WebView2 Runtime。",
				Error:                "WebView2 Runtime 错误",
				FailedToInstall:      "WebView2 Runtime 安装失败，请重试或联系管理员。",
				DownloadPage:         "运行此程序需要 WebView2 Runtime。请选择确定打开微软官方 WebView2 Runtime 下载页面。最低版本：",
				PressOKToInstall:     "请选择确定安装 WebView2 Runtime。",
				ContactAdmin:         "运行此程序需要 WebView2 Runtime，请联系管理员安装。",
				InvalidFixedWebview2: "指定的 WebView2 Runtime 无效，请检查安装和最低版本。",
				WebView2ProcessCrash: "WebView2 Runtime 进程异常退出，请重启托盘程序。",
			},
		},
	}
}

func startTraySafely(options traynative.Options) (controller traynative.Controller, err error) {
	defer func() {
		if recover() != nil {
			controller = nil
			err = traynative.ErrUnavailable
		}
	}()
	return trayStartAdapter(options)
}

func newNativeTrayOptions(ctx context.Context, backend *Backend, notify func(traynative.Event)) traynative.Options {
	showConsole := func() { showNodeWindow(ctx) }
	return traynative.Options{
		Snapshot: func() traynative.Snapshot {
			overview, err := backend.GetOverview()
			if err != nil {
				reportTrayAttention(ctx, "tray_snapshot_failed")
				return traynative.Snapshot{}
			}
			return traynative.Snapshot{
				MachineID:       overview.MachineID,
				Agent:           overview.Agent,
				Helper:          overview.Helper,
				HelperEnabled:   overview.HelperEnabled,
				HelperStartMode: overview.HelperStartMode,
			}
		},
		Handle: func(command traynative.Command) {
			handleTrayCommand(ctx, backend, showConsole, command, notify)
		},
		ShowConsole: showConsole,
		OnError: func(code string) {
			reportTrayAttention(ctx, normalizeTrayErrorCode(code))
		},
	}
}

func handleTrayCommand(ctx context.Context, backend *Backend, showConsole func(), command traynative.Command, notify func(traynative.Event)) {
	switch command {
	case traynative.ShowConsole:
		showConsole()
	case traynative.StartAgent:
		notifyStartFailure(notify, "agent", backend.StartAgent())
	case traynative.RestartAgent:
		notifyStartFailure(notify, "agent", backend.RestartAgent())
	case traynative.StopAgent:
		_ = backend.StopAgent()
	case traynative.StartHelper:
		if overview, err := backend.GetOverview(); err == nil && overview.HelperEnabled && overview.HelperStartMode == traymodel.StartManual {
			notifyEvent(notify, traynative.Event{Component: "helper", Code: traynative.CodeUACRequired})
		}
		notifyStartFailure(notify, "helper", backend.StartHelper())
	case traynative.StopHelper:
		_ = backend.StopHelper()
	case traynative.OpenLogs:
		_ = backend.OpenLocation(traymodel.AgentLogs)
	case traynative.OpenSettings:
		showConsole()
		eventsEmitAdapter(ctx, "open-settings-requested")
	case traynative.ExitTray:
		showConsole()
		eventsEmitAdapter(ctx, "force-exit-requested")
	}
}

func notifyStartFailure(notify func(traynative.Event), component string, result traymodel.OperationResult) {
	if !result.OK {
		notifyEvent(notify, traynative.Event{Component: component, Code: traynative.CodeStartFailed})
	}
}

func notifyEvent(notify func(traynative.Event), event traynative.Event) {
	if notify != nil {
		notify(event)
	}
}

type trayNotificationTarget struct {
	mu         sync.RWMutex
	controller traynative.Controller
}

func (t *trayNotificationTarget) Set(controller traynative.Controller) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.controller = controller
}

func (t *trayNotificationTarget) Clear() {
	t.Set(nil)
}

func (t *trayNotificationTarget) Notify(event traynative.Event) {
	t.mu.RLock()
	controller := t.controller
	t.mu.RUnlock()
	if controller != nil {
		_, _ = controller.Notify(event)
	}
}

type trayStatusMonitor struct {
	havePrevious       bool
	lastAgent          traymodel.Lifecycle
	lastHelper         traymodel.Lifecycle
	workerPendingSince time.Time
	workerNotified     bool
	configUnavailable  bool
	driftActive        bool
}

func (m *trayStatusMonitor) Observe(now time.Time, overview traymodel.Overview, err error, notify func(traynative.Event)) {
	if err != nil {
		if !m.configUnavailable {
			notifyEvent(notify, traynative.Event{Component: "config", Code: traynative.CodeConfigCorrupt})
		}
		m.configUnavailable = true
		return
	}
	m.configUnavailable = false

	drift := overview.HelperTaskDrift || overview.LoginStartDrift
	if drift && !m.driftActive {
		notifyEvent(notify, traynative.Event{Component: "config", Code: traynative.CodeConfigDrift})
	}
	m.driftActive = drift

	if m.havePrevious {
		observeLifecycleTransition("agent", m.lastAgent, overview.Agent.Lifecycle, notify)
		observeLifecycleTransition("helper", m.lastHelper, overview.Helper.Lifecycle, notify)
	}
	m.havePrevious = true
	m.lastAgent = overview.Agent.Lifecycle
	m.lastHelper = overview.Helper.Lifecycle

	workersNotReady := overview.Agent.Lifecycle == traymodel.Running &&
		overview.Agent.WorkerExpected > 0 && overview.Agent.WorkerReady < overview.Agent.WorkerExpected
	if !workersNotReady {
		m.workerPendingSince = time.Time{}
		m.workerNotified = false
		return
	}
	if m.workerPendingSince.IsZero() {
		m.workerPendingSince = now
		return
	}
	if !m.workerNotified && now.Sub(m.workerPendingSince) >= 30*time.Second {
		notifyEvent(notify, traynative.Event{Component: "worker", Code: traynative.CodeWorkersNotReady})
		m.workerNotified = true
	}
}

func observeLifecycleTransition(component string, previous, current traymodel.Lifecycle, notify func(traynative.Event)) {
	if previous == traymodel.Starting && current == traymodel.Failed {
		notifyEvent(notify, traynative.Event{Component: component, Code: traynative.CodeStartFailed})
		return
	}
	if previous == traymodel.Running && current == traymodel.Failed {
		notifyEvent(notify, traynative.Event{Component: component, Code: traynative.CodeUnexpectedExit})
	}
}

type trayMonitorLoop struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type trayMonitorTicker interface {
	C() <-chan time.Time
	Stop()
}

type systemTrayMonitorTicker struct{ ticker *time.Ticker }

func (t *systemTrayMonitorTicker) C() <-chan time.Time { return t.ticker.C }
func (t *systemTrayMonitorTicker) Stop()               { t.ticker.Stop() }

func startTrayStatusMonitor(parent context.Context, backend *Backend, notify func(traynative.Event)) *trayMonitorLoop {
	ctx, cancel := context.WithCancel(parent)
	loop := &trayMonitorLoop{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(loop.done)
		monitor := &trayStatusMonitor{}
		observe := func(now time.Time) {
			overview, err := backend.getOverviewWithContext(ctx)
			monitor.Observe(now, overview, err, notify)
		}
		observe(time.Now())
		ticker := trayMonitorTickerAdapter(2 * time.Second)
		if ticker == nil {
			return
		}
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C():
				observe(now)
			}
		}
	}()
	return loop
}

func (l *trayMonitorLoop) Stop() {
	if l == nil {
		return
	}
	l.cancel()
	<-l.done
}

func showNodeWindow(ctx context.Context) {
	windowShowAdapter(ctx)
	windowUnminimiseAdapter(ctx)
	windowCenterAdapter(ctx)
}

func reportTrayAttention(ctx context.Context, code string) {
	code = normalizeTrayErrorCode(code)
	logWarningAdapter(ctx, code)
	eventsEmitAdapter(ctx, "attention-required", map[string]string{
		"component": "tray",
		"code":      code,
		"summary":   "通知区域功能不可用，请使用节点控制台。",
	})
	showNodeWindow(ctx)
}

func reportRuntimeAttention(ctx context.Context, code, summary string) {
	switch code {
	case "runtime_start_failed", "runtime_close_failed":
	default:
		code = "runtime_start_failed"
	}
	logWarningAdapter(ctx, code)
	eventsEmitAdapter(ctx, "attention-required", map[string]string{
		"component": "tray",
		"code":      code,
		"summary":   summary,
	})
}

func normalizeTrayErrorCode(code string) string {
	switch code {
	case "tray_unavailable", "tray_lifecycle_failed", "tray_menu_failed", "tray_readd_failed", "tray_close_failed", "tray_snapshot_failed":
		return code
	default:
		return "tray_lifecycle_failed"
	}
}

func mustFrontendAssets() fs.FS {
	assets, err := fs.Sub(embeddedFrontend, "frontend/dist")
	if err != nil {
		panic("embedded_frontend_unavailable")
	}
	return assets
}
