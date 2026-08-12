package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dedup/internal/config"
	"dedup/internal/gui"
)

func TestBuildOperationalRuntimeClosesResourcesAfterIntermediateFailure(t *testing.T) {
	originalFactory := guiNewOperationalRuntimeResources
	defer func() { guiNewOperationalRuntimeResources = originalFactory }()
	events := &orderedEvents{}
	resources := &fakeOperationalRuntimeResources{
		events: events,
		fail:   "restore-phase2",
	}
	guiNewOperationalRuntimeResources = func(
		context.Context,
		*config.GUIConfig,
		*slog.Logger,
	) (operationalRuntimeResources, error) {
		events.add("resources")
		return resources, nil
	}

	_, err := buildOperationalRuntime(
		context.Background(),
		config.DefaultGUI(),
		testOperationalLogger(),
	)
	if err == nil || !strings.Contains(err.Error(), "restore phase2") {
		t.Fatalf("buildOperationalRuntime error = %v", err)
	}
	want := []string{
		"resources",
		"ping",
		"restore-tasks",
		"restore-phase2",
		"analysis-begin",
		"analysis-wait",
		"phase2-wait",
		"pool-stop",
		"dispatcher-shutdown",
		"postgres-close",
	}
	if got := events.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestBuildOperationalRuntimeClosesSuccessfulResourcesInOrder(t *testing.T) {
	originalFactory := guiNewOperationalRuntimeResources
	defer func() { guiNewOperationalRuntimeResources = originalFactory }()
	events := &orderedEvents{}
	resources := &fakeOperationalRuntimeResources{events: events}
	guiNewOperationalRuntimeResources = func(
		context.Context,
		*config.GUIConfig,
		*slog.Logger,
	) (operationalRuntimeResources, error) {
		events.add("resources")
		return resources, nil
	}

	runtime, err := buildOperationalRuntime(
		context.Background(),
		config.DefaultGUI(),
		testOperationalLogger(),
	)
	if err != nil {
		t.Fatal(err)
	}
	runtime.Close()
	runtime.Close()
	want := []string{
		"resources",
		"ping",
		"restore-tasks",
		"restore-phase2",
		"start",
		"api",
		"analysis-begin",
		"analysis-wait",
		"phase2-wait",
		"pool-stop",
		"dispatcher-shutdown",
		"postgres-close",
	}
	if got := events.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestInitializeOperationalRuntimeDoesNotInstallAfterCancellation(t *testing.T) {
	originalBuilder := guiBuildOperationalRuntime
	defer func() { guiBuildOperationalRuntime = originalBuilder }()
	cfg := config.DefaultGUI()
	configService, err := gui.NewGUIConfigService(
		filepath.Join(t.TempDir(), "gui.json"),
		cfg,
	)
	if err != nil {
		t.Fatal(err)
	}
	host := gui.NewRuntimeHost(configService, cfg.Agents)
	buildStarted := make(chan struct{})
	releaseBuild := make(chan struct{})
	var closeCalls atomic.Int32
	guiBuildOperationalRuntime = func(
		context.Context,
		*config.GUIConfig,
		*slog.Logger,
	) (*operationalRuntime, error) {
		close(buildStarted)
		<-releaseBuild
		return &operationalRuntime{
			api: gui.NewAPI(
				nil,
				gui.NewTaskRegistry(nil, testOperationalLogger()),
				nil,
			),
			closeRuntime: func() { closeCalls.Add(1) },
		}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() {
		select {
		case <-releaseBuild:
		default:
			close(releaseBuild)
		}
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		initializeOperationalRuntime(ctx, cfg, host, testOperationalLogger(), nil)
	}()
	waitOperationalSignal(t, buildStarted, "runtime build start")
	cancel()
	close(releaseBuild)
	waitOperationalSignal(t, done, "runtime initializer exit")
	if got := closeCalls.Load(); got != 1 {
		t.Fatalf("close calls = %d, want 1", got)
	}
	response := httptest.NewRecorder()
	host.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/tasks", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("tasks status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
}

func TestInitializeOperationalRuntimeWaitsForHTTPDrainBeforeClose(t *testing.T) {
	originalBuilder := guiBuildOperationalRuntime
	defer func() { guiBuildOperationalRuntime = originalBuilder }()
	cfg := config.DefaultGUI()
	configService, err := gui.NewGUIConfigService(
		filepath.Join(t.TempDir(), "gui.json"),
		cfg,
	)
	if err != nil {
		t.Fatal(err)
	}
	host := gui.NewRuntimeHost(configService, cfg.Agents, "")
	built := make(chan struct{})
	closed := make(chan struct{})
	guiBuildOperationalRuntime = func(
		context.Context,
		*config.GUIConfig,
		*slog.Logger,
	) (*operationalRuntime, error) {
		close(built)
		return &operationalRuntime{
			api: gui.NewAPI(nil, nil, nil),
			closeRuntime: func() {
				close(closed)
			},
		}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	drained := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		initializeOperationalRuntime(ctx, cfg, host, testOperationalLogger(), drained)
	}()
	waitOperationalSignal(t, built, "runtime build")
	cancel()
	select {
	case <-closed:
		t.Fatal("runtime closed before HTTP drain completed")
	case <-time.After(50 * time.Millisecond):
	}
	close(drained)
	waitOperationalSignal(t, closed, "runtime close after HTTP drain")
	waitOperationalSignal(t, done, "runtime initializer exit")
}

type fakeOperationalRuntimeResources struct {
	events *orderedEvents
	fail   string
	api    *gui.API
}

func (resources *fakeOperationalRuntimeResources) Ping(context.Context) error {
	resources.events.add("ping")
	return resources.failure("ping")
}

func (resources *fakeOperationalRuntimeResources) RestoreTasks(context.Context) error {
	resources.events.add("restore-tasks")
	return resources.failure("restore-tasks")
}

func (resources *fakeOperationalRuntimeResources) RestorePhase2(context.Context) error {
	resources.events.add("restore-phase2")
	return resources.failure("restore-phase2")
}

func (resources *fakeOperationalRuntimeResources) Start(context.Context) error {
	resources.events.add("start")
	return resources.failure("start")
}

func (resources *fakeOperationalRuntimeResources) API() *gui.API {
	resources.events.add("api")
	if resources.api == nil {
		resources.api = gui.NewAPI(nil, nil, nil)
	}
	return resources.api
}

func (resources *fakeOperationalRuntimeResources) BeginAnalysisShutdown() {
	resources.events.add("analysis-begin")
}

func (resources *fakeOperationalRuntimeResources) WaitForAnalysis() {
	resources.events.add("analysis-wait")
}

func (resources *fakeOperationalRuntimeResources) WaitForPhase2() {
	resources.events.add("phase2-wait")
}

func (resources *fakeOperationalRuntimeResources) StopPool() {
	resources.events.add("pool-stop")
}

func (resources *fakeOperationalRuntimeResources) ShutdownPhase2() {
	resources.events.add("dispatcher-shutdown")
}

func (resources *fakeOperationalRuntimeResources) ClosePostgres() {
	resources.events.add("postgres-close")
}

func (resources *fakeOperationalRuntimeResources) failure(stage string) error {
	if resources.fail == stage {
		return errors.New(stage + " failed")
	}
	return nil
}

func testOperationalLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func waitOperationalSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}
