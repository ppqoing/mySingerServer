package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"dedup/internal/config"
)

type fakePreparedGUIReplacement struct {
	events *[]string
}

func (r *fakePreparedGUIReplacement) Commit() error {
	*r.events = append(*r.events, "replacement commit")
	return nil
}

func (r *fakePreparedGUIReplacement) Abort() error {
	*r.events = append(*r.events, "replacement abort")
	return nil
}

func TestGUIRestartAlwaysStartsReplacementWithExplicitConfigAndParentWait(t *testing.T) {
	originalStart := guiPrepareReplacementLaunch
	defer func() { guiPrepareReplacementLaunch = originalStart }()

	root := t.TempDir()
	executable := filepath.Join(root, "gui.exe")
	configPath := filepath.Join(root, "custom-gui.json")
	parentPID := os.Getpid()
	var gotExecutable string
	var gotArgs []string
	events := []string{}
	guiPrepareReplacementLaunch = func(exe string, args []string) (guiPreparedReplacement, error) {
		gotExecutable = exe
		gotArgs = append([]string(nil), args...)
		return &fakePreparedGUIReplacement{events: &events}, nil
	}
	cancelCalls := 0
	restart := newGUIRestartCoordinator(executable, configPath, parentPID, func() {
		cancelCalls++
	})
	cfg := config.DefaultGUI()
	cfg.ListenAddr = "0.0.0.0:18081"

	recoveryURL, err := restart.Prepare(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if gotExecutable != executable || !filepath.IsAbs(gotExecutable) {
		t.Fatalf("executable=%q want final absolute %q", gotExecutable, executable)
	}
	if len(gotArgs) != 7 || !reflect.DeepEqual(gotArgs[:5], []string{
		"-config", configPath,
		"-no-browser",
		"-wait-parent-pid", strconv.Itoa(parentPID),
	}) || gotArgs[5] != "-restart-token" || gotArgs[6] == "" {
		t.Fatalf("replacement args=%q", gotArgs)
	}
	parsedRecoveryURL, err := url.Parse(recoveryURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsedRecoveryURL.Scheme != "http" || parsedRecoveryURL.Host != "127.0.0.1:18081" ||
		parsedRecoveryURL.Path != "/api/restart/health" ||
		parsedRecoveryURL.Query().Get("restart_token") != gotArgs[6] || !restart.Pending() {
		t.Fatalf("recoveryURL=%q pending=%t", recoveryURL, restart.Pending())
	}
	restart.Commit()
	restart.Commit()
	if cancelCalls != 1 || !reflect.DeepEqual(events, []string{"replacement commit"}) {
		t.Fatalf("cancel calls=%d events=%v", cancelCalls, events)
	}
}

func TestGUIRestartRejectsDuplicatePrepare(t *testing.T) {
	originalStart := guiPrepareReplacementLaunch
	defer func() { guiPrepareReplacementLaunch = originalStart }()
	var starts int
	events := []string{}
	guiPrepareReplacementLaunch = func(string, []string) (guiPreparedReplacement, error) {
		starts++
		return &fakePreparedGUIReplacement{events: &events}, nil
	}
	restart := newGUIRestartCoordinator(
		filepath.Join(t.TempDir(), "gui.exe"),
		filepath.Join(t.TempDir(), "gui.json"),
		os.Getpid(),
		func() {},
	)
	cfg := config.DefaultGUI()

	if _, err := restart.Prepare(cfg); err != nil {
		t.Fatalf("first Prepare: %v", err)
	}
	if _, err := restart.Prepare(cfg); err == nil {
		t.Fatal("duplicate Prepare succeeded")
	}
	if starts != 1 {
		t.Fatalf("replacement starts=%d want=1", starts)
	}
}

func TestGUIRestartLaunchFailureClearsPendingForRetry(t *testing.T) {
	originalStart := guiPrepareReplacementLaunch
	defer func() { guiPrepareReplacementLaunch = originalStart }()
	launchErr := errors.New("CreateProcess failed")
	guiPrepareReplacementLaunch = func(string, []string) (guiPreparedReplacement, error) {
		return nil, launchErr
	}
	restart := newGUIRestartCoordinator(
		filepath.Join(t.TempDir(), "gui.exe"),
		filepath.Join(t.TempDir(), "gui.json"),
		os.Getpid(),
		func() {},
	)

	if _, err := restart.Prepare(config.DefaultGUI()); !errors.Is(err, launchErr) {
		t.Fatalf("Prepare error=%v want=%v", err, launchErr)
	}
	if restart.Pending() {
		t.Fatal("failed replacement launch left restart pending")
	}
}

func TestGUIRestartAbortTerminatesPreparedReplacementAndAllowsRetry(t *testing.T) {
	originalStart := guiPrepareReplacementLaunch
	defer func() { guiPrepareReplacementLaunch = originalStart }()
	events := []string{}
	guiPrepareReplacementLaunch = func(string, []string) (guiPreparedReplacement, error) {
		return &fakePreparedGUIReplacement{events: &events}, nil
	}
	cancelCalls := 0
	restart := newGUIRestartCoordinator(
		filepath.Join(t.TempDir(), "gui.exe"),
		filepath.Join(t.TempDir(), "gui.json"),
		os.Getpid(),
		func() { cancelCalls++ },
	)

	if !restart.Begin() {
		t.Fatal("initial restart transaction was not reserved")
	}
	if _, err := restart.Prepare(config.DefaultGUI()); err != nil {
		t.Fatal(err)
	}
	restart.Abort()
	restart.End()
	if restart.Pending() || cancelCalls != 0 || !reflect.DeepEqual(events, []string{"replacement abort"}) {
		t.Fatalf("pending=%t cancelCalls=%d events=%v", restart.Pending(), cancelCalls, events)
	}

	if !restart.Begin() {
		t.Fatal("restart reservation was not released after Abort")
	}
	if _, err := restart.Prepare(config.DefaultGUI()); err != nil {
		t.Fatal(err)
	}
	restart.Commit()
	restart.End()
	if cancelCalls != 1 || !reflect.DeepEqual(events, []string{"replacement abort", "replacement commit"}) {
		t.Fatalf("cancelCalls=%d events=%v", cancelCalls, events)
	}
}

func TestGUIWaitForParentCompletesBeforeConfigLoadAndListen(t *testing.T) {
	originalExecutable := guiExecutablePath
	originalWait := guiWaitParent
	originalListen := guiListen
	originalServer := guiNewHTTPServer
	originalBuilder := guiBuildOperationalRuntime
	defer func() {
		guiExecutablePath = originalExecutable
		guiWaitParent = originalWait
		guiListen = originalListen
		guiNewHTTPServer = originalServer
		guiBuildOperationalRuntime = originalBuilder
	}()

	root := t.TempDir()
	configPath := filepath.Join(root, "custom-gui.json")
	guiExecutablePath = func() (string, error) { return filepath.Join(root, "gui.exe"), nil }
	waitStarted := make(chan int, 1)
	releaseWait := make(chan struct{})
	guiWaitParent = func(pid int) error {
		waitStarted <- pid
		<-releaseWait
		return nil
	}
	var listenCalls atomic.Int32
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	guiListen = func(string, string) (net.Listener, error) {
		listenCalls.Add(1)
		return listener, nil
	}
	server := newFakeGUIServer(http.ErrServerClosed, true)
	defer server.Shutdown(context.Background())
	guiNewHTTPServer = func(string, http.Handler) guiHTTPServer { return server }
	guiBuildOperationalRuntime = func(context.Context, *config.GUIConfig, *slog.Logger) (*operationalRuntime, error) {
		return nil, errors.New("postgres unavailable")
	}

	result := make(chan error, 1)
	go func() {
		result <- run([]string{
			"-config", configPath,
			"-no-browser",
			"-wait-parent-pid", "4242",
		})
	}()
	select {
	case gotPID := <-waitStarted:
		if gotPID != 4242 {
			t.Fatalf("wait pid=%d want=4242", gotPID)
		}
	case <-time.After(time.Second):
		t.Fatal("parent wait did not start")
	}
	if _, err := os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("config was accessed before parent wait completed: %v", err)
	}
	if got := listenCalls.Load(); got != 0 {
		t.Fatalf("listen calls before parent wait=%d", got)
	}
	select {
	case err := <-result:
		t.Fatalf("run returned before parent wait completed: %v", err)
	default:
	}

	close(releaseWait)
	select {
	case <-server.serveStarted:
	case <-time.After(time.Second):
		t.Fatal("GUI did not serve after parent wait completed")
	}
	if _, err := config.LoadGUI(configPath); err != nil {
		t.Fatalf("config after parent wait: %v", err)
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("GUI did not stop")
	}
}
