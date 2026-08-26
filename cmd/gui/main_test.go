package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"dedup/internal/config"
	"dedup/internal/firstscreen"
	"dedup/internal/gui"
	"dedup/internal/phase2"
	"dedup/internal/proto"
)

type noopAnalysisLifecycle struct{}

func (noopAnalysisLifecycle) BeginAnalysisShutdown() {}
func (noopAnalysisLifecycle) WaitForAnalysis()       {}

func TestGUIOpensBrowserOnlyAfterListenerIsBound(t *testing.T) {
	originalBrowser := guiOpenBrowser
	defer func() { guiOpenBrowser = originalBrowser }()
	events := []string{"listen"}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	guiOpenBrowser = func(string) error { events = append(events, "browser"); return nil }
	server := newFakeGUIServer(http.ErrServerClosed, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := serveBoundGUI(ctx, cancel, server, listener, &noopAnalysisLifecycle{}, time.Second, "127.0.0.1:8080", false, slog.Default(), nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"listen", "browser"}) {
		t.Fatalf("events = %v", events)
	}
}

func TestGUINoBrowserFlagSuppressesBrowserLaunch(t *testing.T) {
	originalBrowser := guiOpenBrowser
	defer func() { guiOpenBrowser = originalBrowser }()
	events := []string{"listen"}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	guiOpenBrowser = func(string) error { events = append(events, "browser"); return nil }
	server := newFakeGUIServer(http.ErrServerClosed, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := serveBoundGUI(ctx, cancel, server, listener, &noopAnalysisLifecycle{}, time.Second, "127.0.0.1:8080", true, slog.Default(), nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"listen"}) {
		t.Fatalf("events = %v", events)
	}
}

func TestGUIMissingConfigurationIsCreatedBeforeServing(t *testing.T) {
	originalExecutable := guiExecutablePath
	originalListen := guiListen
	originalServer := guiNewHTTPServer
	originalBuilder := guiBuildOperationalRuntime
	defer func() {
		guiExecutablePath = originalExecutable
		guiListen = originalListen
		guiNewHTTPServer = originalServer
		guiBuildOperationalRuntime = originalBuilder
	}()
	root := t.TempDir()
	guiExecutablePath = func() (string, error) { return filepath.Join(root, "gui.exe"), nil }
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	guiListen = func(string, string) (net.Listener, error) { return listener, nil }
	built := make(chan struct{})
	guiBuildOperationalRuntime = func(
		context.Context,
		*config.GUIConfig,
		*slog.Logger,
	) (*operationalRuntime, error) {
		close(built)
		return nil, errors.New("postgres unavailable")
	}
	server := newFakeGUIServer(http.ErrServerClosed, true)
	defer server.Shutdown(context.Background())
	guiNewHTTPServer = func(string, http.Handler) guiHTTPServer { return server }
	result := make(chan error, 1)
	go func() { result <- run([]string{"-no-browser"}) }()
	select {
	case <-server.serveStarted:
	case <-time.After(time.Second):
		t.Fatal("GUI did not serve after creating the default configuration")
	}
	select {
	case <-built:
	case <-time.After(time.Second):
		t.Fatal("operational runtime initialization did not start")
	}
	if _, err := config.LoadGUI(filepath.Join(root, "gui.json")); err != nil {
		t.Fatalf("created GUI configuration: %v", err)
	}
	server.shutdownOnce.Do(func() { close(server.shutdown) })
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("GUI did not stop")
	}
}

func TestGUIRestartTokenFlagIsExposedByReplacementHealth(t *testing.T) {
	originalExecutable := guiExecutablePath
	originalListen := guiListen
	originalServer := guiNewHTTPServer
	originalBuilder := guiBuildOperationalRuntime
	defer func() {
		guiExecutablePath = originalExecutable
		guiListen = originalListen
		guiNewHTTPServer = originalServer
		guiBuildOperationalRuntime = originalBuilder
	}()

	root := t.TempDir()
	guiExecutablePath = func() (string, error) { return filepath.Join(root, "gui.exe"), nil }
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	guiListen = func(string, string) (net.Listener, error) { return listener, nil }
	guiBuildOperationalRuntime = func(context.Context, *config.GUIConfig, *slog.Logger) (*operationalRuntime, error) {
		return nil, errors.New("postgres unavailable")
	}
	server := newFakeGUIServer(http.ErrServerClosed, true)
	defer server.Shutdown(context.Background())
	handlers := make(chan http.Handler, 1)
	guiNewHTTPServer = func(_ string, handler http.Handler) guiHTTPServer {
		handlers <- handler
		return server
	}

	result := make(chan error, 1)
	go func() {
		result <- run([]string{"-no-browser", "-restart-token", "replacement-instance-token"})
	}()
	var handler http.Handler
	select {
	case handler = <-handlers:
	case err := <-result:
		t.Fatalf("run returned before exposing restart health: %v", err)
	case <-time.After(time.Second):
		t.Fatal("GUI did not create its runtime host")
	}

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/api/restart/health", nil))
	if got := health.Body.String(); got != "{\"ok\":true,\"restart_token\":\"replacement-instance-token\",\"restarting\":false}\n" {
		t.Fatalf("restart health=%s", got)
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

func TestGUIPingFailureIsLoggedBeforeInteractiveNotification(t *testing.T) {
	originalExecutable := guiExecutablePath
	originalNotify := guiShowStartupError
	originalListen := guiListen
	originalServer := guiNewHTTPServer
	originalLogger := guiNewRuntimeLogger
	defer func() {
		guiExecutablePath = originalExecutable
		guiShowStartupError = originalNotify
		guiListen = originalListen
		guiNewHTTPServer = originalServer
		guiNewRuntimeLogger = originalLogger
	}()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "gui.json"), []byte(`{
		"listen_addr":"127.0.0.1:18080",
        "pg_dsn":"postgres://user:secret@127.0.0.1:1/dedup?connect_timeout=1",
        "agents":[{"addr":"127.0.0.1:9101"}]
    }`), 0o600); err != nil {
		t.Fatal(err)
	}
	guiExecutablePath = func() (string, error) { return filepath.Join(root, "gui.exe"), nil }
	events := &orderedEvents{}
	failureLogged := make(chan struct{})
	logHandler := &startupEventLogHandler{
		events:        events,
		failureLogged: failureLogged,
	}
	guiNewRuntimeLogger = func(string, io.Writer) (*slog.Logger, func() error, error) {
		return slog.New(logHandler), func() error { return nil }, nil
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	guiListen = func(string, string) (net.Listener, error) {
		events.add("listen")
		return listener, nil
	}
	server := &runtimeFailureGUIServer{
		failureLogged: failureLogged,
		shutdown:      make(chan struct{}),
		started:       make(chan struct{}),
	}
	defer server.Shutdown(context.Background())
	guiNewHTTPServer = func(string, http.Handler) guiHTTPServer { return server }
	var notification string
	guiShowStartupError = func(message string) { notification = message }
	result := make(chan error, 1)
	go func() { result <- executeGUI([]string{"-no-browser"}) }()
	select {
	case <-server.started:
	case <-time.After(time.Second):
		t.Fatal("GUI did not begin serving before PostgreSQL initialization")
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("executeGUI: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("GUI did not stop after server cancellation")
	}
	if notification != "" {
		t.Fatalf("database failure triggered startup notification %q", notification)
	}
	content := logHandler.snapshot()
	if strings.Contains(content, "secret") || strings.Contains(content, "postgres://") {
		t.Fatalf("runtime failure log leaked DSN: %s", content)
	}
	want := []string{"listen", "serve", "postgres-error"}
	if got := events.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestGUIBindsAndServesBeforeOperationalRuntimeRestore(t *testing.T) {
	originalExecutable := guiExecutablePath
	originalListen := guiListen
	originalServer := guiNewHTTPServer
	originalBuilder := guiBuildOperationalRuntime
	defer func() {
		guiExecutablePath = originalExecutable
		guiListen = originalListen
		guiNewHTTPServer = originalServer
		guiBuildOperationalRuntime = originalBuilder
	}()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "gui.json"), []byte(`{
		"listen_addr":"127.0.0.1:18080",
		"pg_dsn":"postgres://dedup@127.0.0.1:5432/dedup",
		"agents":[{"addr":"127.0.0.1:9101"}]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	guiExecutablePath = func() (string, error) { return filepath.Join(root, "gui.exe"), nil }
	events := &orderedEvents{}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	guiListen = func(string, string) (net.Listener, error) {
		events.add("listen")
		return listener, nil
	}
	server := &recordingCancelableGUIServer{
		events:  events,
		release: make(chan struct{}),
		started: make(chan struct{}),
	}
	defer server.Shutdown(context.Background())
	guiNewHTTPServer = func(string, http.Handler) guiHTTPServer { return server }
	restoreStarted := make(chan struct{})
	continueRestore := make(chan struct{})
	defer func() {
		select {
		case <-continueRestore:
		default:
			close(continueRestore)
		}
	}()
	restoreFinished := make(chan struct{})
	guiBuildOperationalRuntime = func(
		context.Context,
		*config.GUIConfig,
		*slog.Logger,
	) (*operationalRuntime, error) {
		close(restoreStarted)
		<-continueRestore
		events.add("restore")
		close(restoreFinished)
		return nil, errors.New("restore failed")
	}
	result := make(chan error, 1)
	go func() { result <- run([]string{"-no-browser"}) }()
	select {
	case <-restoreStarted:
	case <-time.After(time.Second):
		t.Fatal("operational runtime restore did not start")
	}
	select {
	case <-server.started:
	case <-time.After(time.Second):
		t.Fatal("GUI did not serve while operational runtime restore was blocked")
	}
	if got := events.snapshot(); !reflect.DeepEqual(got, []string{"listen", "serve"}) {
		t.Fatalf("events before restore continuation = %v", got)
	}
	close(continueRestore)
	select {
	case <-restoreFinished:
	case <-time.After(time.Second):
		t.Fatal("operational runtime restore did not continue")
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
	if got := events.snapshot(); !reflect.DeepEqual(got, []string{"listen", "serve", "restore"}) {
		t.Fatalf("events = %v", got)
	}
}

type recordingCancelableGUIServer struct {
	events       *orderedEvents
	release      chan struct{}
	started      chan struct{}
	shutdownOnce sync.Once
}

func (s *recordingCancelableGUIServer) Serve(net.Listener) error {
	s.events.add("serve")
	close(s.started)
	<-s.release
	return http.ErrServerClosed
}

func (s *recordingCancelableGUIServer) Shutdown(context.Context) error {
	s.shutdownOnce.Do(func() {
		select {
		case <-s.release:
		default:
			close(s.release)
		}
	})
	return nil
}

func (s *recordingCancelableGUIServer) Close() error {
	return s.Shutdown(context.Background())
}

type runtimeFailureGUIServer struct {
	failureLogged <-chan struct{}
	shutdown      chan struct{}
	started       chan struct{}
	shutdownOnce  sync.Once
}

func (s *runtimeFailureGUIServer) Serve(net.Listener) error {
	close(s.started)
	select {
	case <-s.failureLogged:
	case <-s.shutdown:
	}
	return http.ErrServerClosed
}

func (s *runtimeFailureGUIServer) Shutdown(context.Context) error {
	s.shutdownOnce.Do(func() { close(s.shutdown) })
	return nil
}

func (s *runtimeFailureGUIServer) Close() error {
	return s.Shutdown(context.Background())
}

type startupEventLogHandler struct {
	events        *orderedEvents
	failureLogged chan struct{}
	failureOnce   sync.Once

	mu      sync.Mutex
	records []string
}

func (*startupEventLogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (handler *startupEventLogHandler) Handle(_ context.Context, record slog.Record) error {
	var fields strings.Builder
	fields.WriteString(record.Message)
	record.Attrs(func(attr slog.Attr) bool {
		fields.WriteByte(' ')
		fields.WriteString(attr.Key)
		fields.WriteByte('=')
		fields.WriteString(attr.Value.String())
		return true
	})
	handler.mu.Lock()
	handler.records = append(handler.records, fields.String())
	handler.mu.Unlock()
	switch record.Message {
	case "gui serving":
		handler.events.add("serve")
	case "operational runtime unavailable":
		handler.events.add("postgres-error")
		handler.failureOnce.Do(func() { close(handler.failureLogged) })
	}
	return nil
}

func (handler *startupEventLogHandler) WithAttrs([]slog.Attr) slog.Handler {
	return handler
}

func (handler *startupEventLogHandler) WithGroup(string) slog.Handler {
	return handler
}

func (handler *startupEventLogHandler) snapshot() string {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return strings.Join(handler.records, "\n")
}

func TestLoadGUIRuntimeReturnsAbsoluteNonDefaultPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom-gui.json")
	if err := os.WriteFile(path, []byte(`{
		"pg_dsn":"postgres://user:pass@127.0.0.1:5432/dedup",
		"agents":[{"machine_id":"agent-a","addr":"192.168.1.10:9101"}]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	absolute, cfg, err := loadGUIRuntime(path)
	if err != nil {
		t.Fatal(err)
	}
	wantAbsolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	if absolute != wantAbsolute || cfg.ListenAddr != config.DefaultGUI().ListenAddr {
		t.Fatalf("path=%q cfg=%#v", absolute, cfg)
	}
}

type fakeAnalysisPoolConn struct {
	mu       sync.Mutex
	released int
}

func (*fakeAnalysisPoolConn) Conn() *pgx.Conn {
	return nil
}

func (c *fakeAnalysisPoolConn) Release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.released++
}

func (c *fakeAnalysisPoolConn) releaseCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.released
}

type fakeAnalysisPool struct {
	mu          sync.Mutex
	acquireErr  error
	acquireCtxs []context.Context
	conns       []*fakeAnalysisPoolConn
}

func (p *fakeAnalysisPool) Acquire(ctx context.Context) (analysisPoolConn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.acquireCtxs = append(p.acquireCtxs, ctx)
	if p.acquireErr != nil {
		return nil, p.acquireErr
	}
	conn := &fakeAnalysisPoolConn{}
	p.conns = append(p.conns, conn)
	return conn, nil
}

func (p *fakeAnalysisPool) snapshot() (int, []*fakeAnalysisPoolConn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.acquireCtxs), append([]*fakeAnalysisPoolConn(nil), p.conns...)
}

type analysisEngineFunc func(context.Context) (*firstscreen.RunStats, error)

func (f analysisEngineFunc) Run(ctx context.Context) (*firstscreen.RunStats, error) {
	return f(ctx)
}

type guiAnalysisRunnerFunc func(context.Context) (*firstscreen.RunStats, error)

func (f guiAnalysisRunnerFunc) Run(ctx context.Context) (*firstscreen.RunStats, error) {
	return f(ctx)
}

func TestFirstScreenCompositionAcquiresAndReleasesDedicatedConnectionPerRun(t *testing.T) {
	pool := &fakeAnalysisPool{}
	var factoryMu sync.Mutex
	var factoryCalls int
	factory := analysisEngineFactory(func(*pgx.Conn, firstscreen.Config, *slog.Logger) analysisEngine {
		factoryMu.Lock()
		factoryCalls++
		call := factoryCalls
		factoryMu.Unlock()
		return analysisEngineFunc(func(context.Context) (*firstscreen.RunStats, error) {
			if call == 2 {
				return nil, errors.New("second run failed")
			}
			return &firstscreen.RunStats{FilesScanned: call}, nil
		})
	})
	runner := newPooledAnalysisRunner(
		context.Background(),
		pool,
		firstscreen.DefaultConfig(),
		slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		factory,
	)

	first, err := runner.Run(context.Background())
	if err != nil || first == nil || first.FilesScanned != 1 {
		t.Fatalf("first Run() = (%#v, %v)", first, err)
	}
	if _, err := runner.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "second run failed") {
		t.Fatalf("second Run() error = %v", err)
	}

	acquires, conns := pool.snapshot()
	if acquires != 2 || len(conns) != 2 || conns[0] == conns[1] {
		t.Fatalf("acquires = %d conns = %#v, want two independent acquisitions", acquires, conns)
	}
	for index, conn := range conns {
		if conn.releaseCount() != 1 {
			t.Errorf("connection %d release count = %d, want 1", index, conn.releaseCount())
		}
	}
	factoryMu.Lock()
	defer factoryMu.Unlock()
	if factoryCalls != 2 {
		t.Fatalf("factory calls = %d, want 2", factoryCalls)
	}
}

func TestFirstScreenCompositionPassesShutdownCancellationToAnalyzer(t *testing.T) {
	shutdownContext, cancelShutdown := context.WithCancel(context.Background())
	defer cancelShutdown()
	pool := &fakeAnalysisPool{}
	analyzerStarted := make(chan context.Context, 1)
	factory := analysisEngineFactory(func(*pgx.Conn, firstscreen.Config, *slog.Logger) analysisEngine {
		return analysisEngineFunc(func(ctx context.Context) (*firstscreen.RunStats, error) {
			analyzerStarted <- ctx
			<-ctx.Done()
			return nil, ctx.Err()
		})
	})
	runner := newPooledAnalysisRunner(
		shutdownContext,
		pool,
		firstscreen.DefaultConfig(),
		slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		factory,
	)

	result := make(chan error, 1)
	go func() {
		_, err := runner.Run(context.Background())
		result <- err
	}()
	var analyzerContext context.Context
	select {
	case analyzerContext = <-analyzerStarted:
	case <-time.After(time.Second):
		t.Fatal("analyzer did not start")
	}
	// 分析器收到的是叠加 per-run 取消的派生上下文：进程 shutdown 依旧兜底。
	if analyzerContext == shutdownContext || analyzerContext.Err() != nil {
		t.Fatal("analyzer did not receive a live run-scoped context")
	}
	cancelShutdown()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown cancellation did not reach analyzer")
	}

	_, conns := pool.snapshot()
	if len(conns) != 1 || conns[0].releaseCount() != 1 {
		t.Fatalf("connections after cancellation = %#v", conns)
	}
}

func TestFirstScreenCompositionPerRunCancelReachesAnalyzerWithoutShutdown(t *testing.T) {
	shutdownContext, cancelShutdown := context.WithCancel(context.Background())
	defer cancelShutdown()
	pool := &fakeAnalysisPool{}
	analyzerStarted := make(chan context.Context, 1)
	factory := analysisEngineFactory(func(*pgx.Conn, firstscreen.Config, *slog.Logger) analysisEngine {
		return analysisEngineFunc(func(ctx context.Context) (*firstscreen.RunStats, error) {
			analyzerStarted <- ctx
			<-ctx.Done()
			return nil, ctx.Err()
		})
	})
	runner := newPooledAnalysisRunner(
		shutdownContext,
		pool,
		firstscreen.DefaultConfig(),
		slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		factory,
	)

	runContext, cancelRun := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := runner.Run(runContext)
		result <- err
	}()
	select {
	case <-analyzerStarted:
	case <-time.After(time.Second):
		t.Fatal("analyzer did not start")
	}
	cancelRun()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("per-run cancellation did not reach analyzer")
	}
	if shutdownContext.Err() != nil {
		t.Fatal("per-run cancel leaked into the process shutdown context")
	}
	_, conns := pool.snapshot()
	if len(conns) != 1 || conns[0].releaseCount() != 1 {
		t.Fatalf("connections after per-run cancellation = %#v", conns)
	}
}

func TestFirstScreenCompositionAcquireFailureIsVisibleAndDoesNotLeak(t *testing.T) {
	acquireErr := errors.New("pool exhausted")
	pool := &fakeAnalysisPool{acquireErr: acquireErr}
	var factoryCalled bool
	factory := analysisEngineFactory(func(*pgx.Conn, firstscreen.Config, *slog.Logger) analysisEngine {
		factoryCalled = true
		return analysisEngineFunc(func(context.Context) (*firstscreen.RunStats, error) {
			return nil, nil
		})
	})
	runner := newPooledAnalysisRunner(
		context.Background(),
		pool,
		firstscreen.DefaultConfig(),
		slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		factory,
	)

	stats, err := runner.Run(context.Background())
	if stats != nil {
		t.Fatalf("stats = %#v, want nil", stats)
	}
	if !errors.Is(err, acquireErr) || !strings.Contains(err.Error(), "acquire") {
		t.Fatalf("Run() error = %v, want visible acquire cause", err)
	}
	if factoryCalled {
		t.Fatal("factory called after acquire failure")
	}
	acquires, conns := pool.snapshot()
	if acquires != 1 || len(conns) != 0 {
		t.Fatalf("acquires = %d conns = %#v", acquires, conns)
	}
}

type orderedEvents struct {
	mu     sync.Mutex
	events []string
}

func (e *orderedEvents) add(event string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, event)
}

func (e *orderedEvents) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.events...)
}

type orderedAnalysisConn struct {
	events *orderedEvents
}

func (*orderedAnalysisConn) Conn() *pgx.Conn {
	return nil
}

func (c *orderedAnalysisConn) Release() {
	c.events.add("conn release")
}

type singleAnalysisPool struct {
	conn analysisPoolConn
}

func (p singleAnalysisPool) Acquire(context.Context) (analysisPoolConn, error) {
	return p.conn, nil
}

type recordingAnalysisLifecycle struct {
	api       *gui.API
	events    *orderedEvents
	beginOnce sync.Once
}

func (l *recordingAnalysisLifecycle) BeginAnalysisShutdown() {
	l.beginOnce.Do(func() {
		l.events.add("admission closed")
	})
	l.api.BeginAnalysisShutdown()
}

func (l *recordingAnalysisLifecycle) WaitForAnalysis() {
	l.api.WaitForAnalysis()
	l.events.add("wait returns")
}

type fakeGUIServer struct {
	serveStarted chan struct{}
	shutdown     chan struct{}
	shutdownOnce sync.Once
	serveErr     error
	waitShutdown bool
}

func newFakeGUIServer(serveErr error, waitShutdown bool) *fakeGUIServer {
	return &fakeGUIServer{
		serveStarted: make(chan struct{}),
		shutdown:     make(chan struct{}),
		serveErr:     serveErr,
		waitShutdown: waitShutdown,
	}
}

func (s *fakeGUIServer) Serve(net.Listener) error {
	close(s.serveStarted)
	if s.waitShutdown {
		<-s.shutdown
	}
	return s.serveErr
}

func (s *fakeGUIServer) Shutdown(context.Context) error {
	s.shutdownOnce.Do(func() {
		close(s.shutdown)
	})
	return nil
}

func (s *fakeGUIServer) Close() error {
	return s.Shutdown(context.Background())
}

func TestFirstScreenServeErrorDrainsAcceptedRunBeforeReturning(t *testing.T) {
	events := &orderedEvents{}
	processContext, cancelProcess := context.WithCancel(context.Background())
	var cancelOnce sync.Once
	cancelAndRecord := func() {
		cancelOnce.Do(func() {
			events.add("process context canceled")
			cancelProcess()
		})
	}
	defer cancelAndRecord()

	engineStarted := make(chan struct{})
	runner := newPooledAnalysisRunner(
		processContext,
		singleAnalysisPool{conn: &orderedAnalysisConn{events: events}},
		firstscreen.DefaultConfig(),
		slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		analysisEngineFactory(func(*pgx.Conn, firstscreen.Config, *slog.Logger) analysisEngine {
			return analysisEngineFunc(func(ctx context.Context) (*firstscreen.RunStats, error) {
				close(engineStarted)
				<-ctx.Done()
				events.add("runner exits")
				return nil, ctx.Err()
			})
		}),
	)
	api := gui.NewAPI(nil, nil, nil, runner)
	routes := api.Routes()
	runResponse := httptest.NewRecorder()
	routes.ServeHTTP(runResponse, httptest.NewRequest(
		http.MethodPost,
		"/api/analysis/firstscreen/run",
		nil,
	))
	if runResponse.Code != http.StatusAccepted {
		t.Fatalf("POST status = %d body=%s", runResponse.Code, runResponse.Body.String())
	}
	select {
	case <-engineStarted:
	case <-time.After(time.Second):
		t.Fatal("accepted analysis did not start")
	}

	serveErr := errors.New("listener failed")
	server := newFakeGUIServer(serveErr, false)
	lifecycle := &recordingAnalysisLifecycle{api: api, events: events}
	err := serveAndDrain(
		processContext,
		cancelAndRecord,
		server,
		nil,
		lifecycle,
		time.Second,
	)
	if !errors.Is(err, serveErr) {
		t.Fatalf("serveAndDrain() error = %v, want listener cause", err)
	}
	events.add("pool close")

	wantOrder := []string{
		"admission closed",
		"process context canceled",
		"runner exits",
		"conn release",
		"wait returns",
		"pool close",
	}
	if got := events.snapshot(); !equalEventOrder(got, wantOrder) {
		t.Fatalf("events = %v, want ordered subsequence %v", got, wantOrder)
	}

	rejected := httptest.NewRecorder()
	routes.ServeHTTP(rejected, httptest.NewRequest(
		http.MethodPost,
		"/api/analysis/firstscreen/run",
		nil,
	))
	if rejected.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST after serve error status = %d body=%s", rejected.Code, rejected.Body.String())
	}
}

func TestServeErrorCancelsAdmittedSuccessHookBeforeWaiting(t *testing.T) {
	processContext, cancelProcess := context.WithCancel(context.Background())
	defer cancelProcess()
	runner := guiAnalysisRunnerFunc(func(context.Context) (*firstscreen.RunStats, error) {
		return &firstscreen.RunStats{StageElapsedMs: map[string]int64{}}, nil
	})
	api := gui.NewAPI(nil, nil, nil, runner)
	hookStarted := make(chan struct{})
	api.SetAnalysisSuccessHook(func() error {
		close(hookStarted)
		<-processContext.Done()
		return processContext.Err()
	})
	response := httptest.NewRecorder()
	api.Routes().ServeHTTP(response, httptest.NewRequest(
		http.MethodPost,
		"/api/analysis/firstscreen/run",
		nil,
	))
	if response.Code != http.StatusAccepted {
		t.Fatalf("POST status=%d body=%s", response.Code, response.Body.String())
	}
	select {
	case <-hookStarted:
	case <-time.After(time.Second):
		t.Fatal("success hook did not start")
	}
	serveErr := errors.New("listener failed during hook")
	result := make(chan error, 1)
	go func() {
		result <- serveAndDrain(
			processContext,
			cancelProcess,
			newFakeGUIServer(serveErr, false),
			nil,
			api,
			time.Second,
		)
	}()
	select {
	case err := <-result:
		if !errors.Is(err, serveErr) {
			t.Fatalf("serveAndDrain error=%v, want listener failure", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serveAndDrain waited before cancelling active hook")
	}
}

type fakeAnalysisLifecycle struct {
	mu         sync.Mutex
	beginCalls int
	waitCalls  int
}

func (l *fakeAnalysisLifecycle) BeginAnalysisShutdown() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.beginCalls++
}

func (l *fakeAnalysisLifecycle) WaitForAnalysis() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.waitCalls++
}

func (l *fakeAnalysisLifecycle) counts() (int, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.beginCalls, l.waitCalls
}

func TestFirstScreenSignalServerCloseIsNotReportedAsError(t *testing.T) {
	processContext, cancelProcess := context.WithCancel(context.Background())
	server := newFakeGUIServer(http.ErrServerClosed, true)
	lifecycle := &fakeAnalysisLifecycle{}
	result := make(chan error, 1)
	go func() {
		result <- serveAndDrain(
			processContext,
			cancelProcess,
			server,
			nil,
			lifecycle,
			time.Second,
		)
	}()
	<-server.serveStarted
	cancelProcess()

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("serveAndDrain() error = %v, want nil for signal shutdown", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serveAndDrain did not finish after signal cancellation")
	}
	beginCalls, waitCalls := lifecycle.counts()
	if beginCalls == 0 || waitCalls != 1 {
		t.Fatalf("lifecycle calls = begin:%d wait:%d", beginCalls, waitCalls)
	}
}

func TestFirstScreenNilServeReturnStillRunsCommonDrain(t *testing.T) {
	processContext, cancelProcess := context.WithCancel(context.Background())
	server := newFakeGUIServer(nil, false)
	lifecycle := &fakeAnalysisLifecycle{}

	if err := serveAndDrain(
		processContext,
		cancelProcess,
		server,
		nil,
		lifecycle,
		time.Second,
	); err != nil {
		t.Fatalf("serveAndDrain() error = %v, want nil", err)
	}
	if processContext.Err() == nil {
		t.Fatal("nil serve return did not cancel process context")
	}
	beginCalls, waitCalls := lifecycle.counts()
	if beginCalls == 0 || waitCalls != 1 {
		t.Fatalf("lifecycle calls = begin:%d wait:%d", beginCalls, waitCalls)
	}
}

type shutdownTimeoutGUIServer struct {
	serveStarted chan struct{}
	forceClosed  chan struct{}
	closeOnce    sync.Once
}

func newShutdownTimeoutGUIServer() *shutdownTimeoutGUIServer {
	return &shutdownTimeoutGUIServer{
		serveStarted: make(chan struct{}),
		forceClosed:  make(chan struct{}),
	}
}

func (s *shutdownTimeoutGUIServer) Serve(net.Listener) error {
	close(s.serveStarted)
	<-s.forceClosed
	return http.ErrServerClosed
}

func (*shutdownTimeoutGUIServer) Shutdown(context.Context) error {
	return context.DeadlineExceeded
}

func (s *shutdownTimeoutGUIServer) Close() error {
	s.closeOnce.Do(func() { close(s.forceClosed) })
	return nil
}

type recordingHTTPDrainLifecycle struct {
	events *orderedEvents
}

func (l *recordingHTTPDrainLifecycle) BeginAnalysisShutdown() {
	l.events.add("analysis begin")
}

func (l *recordingHTTPDrainLifecycle) WaitForAnalysis() {
	l.events.add("analysis wait")
}

func (l *recordingHTTPDrainLifecycle) BeginHTTPShutdown() {
	l.events.add("http begin")
}

func (l *recordingHTTPDrainLifecycle) WaitForHTTP() {
	l.events.add("http wait")
}

func TestShutdownTimeoutForceClosesServerAndWaitsForHTTPHandlers(t *testing.T) {
	processContext, cancelProcess := context.WithCancel(context.Background())
	server := newShutdownTimeoutGUIServer()
	events := &orderedEvents{}
	lifecycle := &recordingHTTPDrainLifecycle{events: events}
	result := make(chan error, 1)
	go func() {
		result <- serveAndDrain(
			processContext,
			cancelProcess,
			server,
			nil,
			lifecycle,
			50*time.Millisecond,
		)
	}()
	select {
	case <-server.serveStarted:
	case <-time.After(time.Second):
		t.Fatal("server did not start")
	}
	cancelProcess()
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("serveAndDrain error=%v, want shutdown deadline", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown timeout did not force-close the server")
	}
	if got := events.snapshot(); !equalEventOrder(got, []string{
		"http begin",
		"analysis begin",
		"http wait",
		"analysis wait",
	}) {
		t.Fatalf("shutdown events=%v", got)
	}
}

func equalEventOrder(got, want []string) bool {
	next := 0
	for _, event := range got {
		if next < len(want) && event == want[next] {
			next++
		}
	}
	return next == len(want)
}

func TestRouteAgentMessageBindsRecognizedPhase2ResultAndStillBroadcasts(t *testing.T) {
	events := make([]string, 0)
	dispatcher := &fakePhase2MessageDispatcher{
		events:     &events,
		recognized: true,
		bound: &phase2.BoundFeatureResult{
			TaskID: "phase2-task",
			Items: []phase2.BoundFeatureItem{{
				Kind: proto.KindImage,
				Item: proto.FeatureItem{SHA512: strings.Repeat("a", 128)},
			}},
		},
	}
	rescreener := &fakeRescreenConsumer{events: &events}
	registry := &fakeTaskMessageConsumer{events: &events}
	result := &proto.FeatureResult{TaskID: "phase2-task"}

	routeAgentMessage(
		context.Background(),
		"machine-a",
		result,
		dispatcher,
		rescreener,
		registry,
		slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	)
	want := []string{"bind", "phase2", "rescreen", "scan"}
	if !equalEventOrder(events, want) || len(events) != len(want) {
		t.Fatalf("events=%v, want %v", events, want)
	}
	if rescreener.results != 1 || registry.messages != 1 {
		t.Fatalf("rescreener results=%d registry messages=%d", rescreener.results, registry.messages)
	}
}

type testDeleteReportConsumerFunc func(string, *proto.DeleteReport)

func (consumer testDeleteReportConsumerFunc) HandleReport(
	machineID string,
	report *proto.DeleteReport,
) {
	consumer(machineID, report)
}

func TestGUIDeleteWiringRoutesReportBeforePhase2AndBroadcast(t *testing.T) {
	events := make([]string, 0, 3)
	dispatcher := &fakePhase2MessageDispatcher{events: &events}
	rescreener := &fakeRescreenConsumer{events: &events}
	registry := &fakeTaskMessageConsumer{events: &events}
	deleteConsumer := testDeleteReportConsumerFunc(func(
		machineID string,
		report *proto.DeleteReport,
	) {
		if machineID != "machine-a" || report.TaskID != "delete-task" {
			t.Fatalf("delete callback machine=%q report=%#v", machineID, report)
		}
		events = append(events, "delete")
	})

	routeAgentMessage(
		context.Background(),
		"machine-a",
		&proto.DeleteReport{TaskID: "delete-task"},
		dispatcher,
		rescreener,
		registry,
		slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		deleteConsumer,
	)

	want := []string{"delete", "phase2", "scan"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v, want %v", events, want)
	}
}

type guiDeleteTransportStub struct {
	online bool
	types  []uint8
	tasks  []proto.DeleteTask
}

func (transport *guiDeleteTransportStub) Send(
	_ string,
	msgType uint8,
	value any,
) error {
	transport.types = append(transport.types, msgType)
	task, ok := value.(*proto.DeleteTask)
	if !ok {
		return fmt.Errorf("message type %T is not *proto.DeleteTask", value)
	}
	transport.tasks = append(transport.tasks, *task)
	return nil
}

func (transport *guiDeleteTransportStub) IsOnline(string) bool {
	return transport.online
}

func TestGUIDeleteWiringRuntimeUsesSixtySecondStoreAndInjectedTransport(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	transport := &guiDeleteTransportStub{online: true}
	service, confirms := newDeleteRuntime(
		nil,
		transport,
		time.Minute,
		func() time.Time { return now },
		slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	)
	token, _, err := confirms.Create([]gui.DeleteMember{{
		FileID: 1, MachineID: "machine-a", Path: `D:\one`, Size: 10,
	}})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, err := confirms.Consume(token); !errors.Is(err, gui.ErrConfirmationExpired) {
		t.Fatalf("Consume at 60 seconds error=%v", err)
	}

	token, _, err = confirms.Create([]gui.DeleteMember{{
		FileID: 2, MachineID: "machine-a", Path: `D:\two`, Size: 20,
	}})
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := service.Execute(context.Background(), token, "")
	if err != nil {
		t.Fatal(err)
	}
	if taskID == "" || !reflect.DeepEqual(transport.types, []uint8{proto.MsgDeleteTask}) ||
		len(transport.tasks) != 1 ||
		transport.tasks[0].Mode != proto.ModeSoft ||
		!reflect.DeepEqual(transport.tasks[0].Entries, []string{`D:\two`}) {
		t.Fatalf("taskID=%q types=%v tasks=%#v", taskID, transport.types, transport.tasks)
	}
}

func TestGUIDeleteWiringNilBeforePublicationDoesNotPanic(t *testing.T) {
	events := make([]string, 0, 2)
	dispatcher := &fakePhase2MessageDispatcher{events: &events}
	rescreener := &fakeRescreenConsumer{events: &events}
	registry := &fakeTaskMessageConsumer{events: &events}
	var unpublished deleteReportConsumer

	routeAgentMessage(
		context.Background(),
		"machine-a",
		&proto.DeleteReport{TaskID: "delete-task"},
		dispatcher,
		rescreener,
		registry,
		slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		unpublished,
	)

	if !reflect.DeepEqual(events, []string{"phase2", "scan"}) {
		t.Fatalf("events=%v", events)
	}
}

type pendingScanSourceStub struct {
	tasks []proto.ScanTask
}

func (source pendingScanSourceStub) PendingScans(string) []proto.ScanTask {
	return append([]proto.ScanTask(nil), source.tasks...)
}

type reconnectSenderStub struct {
	types []uint8
}

func (sender *reconnectSenderStub) Send(_ string, msgType uint8, _ any) error {
	sender.types = append(sender.types, msgType)
	return nil
}

type reconnectPhase2Stub struct {
	calls int
}

func (dispatcher *reconnectPhase2Stub) DispatchMachinePending(
	context.Context,
	string,
) error {
	dispatcher.calls++
	return nil
}

func TestGUIDeleteWiringReconnectResumesOnlyScanAndPhase2(t *testing.T) {
	source := pendingScanSourceStub{tasks: []proto.ScanTask{{
		TaskID: "scan-task", Roots: []string{`D:\media`}, Phase: 1,
	}}}
	sender := &reconnectSenderStub{}
	phase2Dispatcher := &reconnectPhase2Stub{}

	resumeAgentWork(
		context.Background(),
		"machine-a",
		source,
		sender,
		phase2Dispatcher,
		slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	)

	if !reflect.DeepEqual(sender.types, []uint8{proto.MsgScanTask}) {
		t.Fatalf("sent message types=%v", sender.types)
	}
	if phase2Dispatcher.calls != 1 {
		t.Fatalf("phase2 resume calls=%d", phase2Dispatcher.calls)
	}
}

func TestRouteAgentMessageRejectsUnboundResultButPreservesExistingBroadcast(t *testing.T) {
	events := make([]string, 0)
	dispatcher := &fakePhase2MessageDispatcher{
		events: &events, bindErr: errors.New("wrong machine"),
	}
	rescreener := &fakeRescreenConsumer{events: &events}
	registry := &fakeTaskMessageConsumer{events: &events}

	routeAgentMessage(
		context.Background(),
		"machine-b",
		&proto.FeatureResult{TaskID: "phase2-task"},
		dispatcher,
		rescreener,
		registry,
		slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	)
	if rescreener.results != 0 || registry.messages != 1 {
		t.Fatalf("rescreener results=%d registry messages=%d", rescreener.results, registry.messages)
	}
	if !equalEventOrder(events, []string{"bind", "phase2", "scan"}) {
		t.Fatalf("events=%v", events)
	}
}

func TestRouteAgentMessageFinalizesOnlyAfterRecognizedTerminalAndAlwaysBroadcasts(t *testing.T) {
	events := make([]string, 0)
	dispatcher := &fakePhase2MessageDispatcher{events: &events, recognized: true}
	rescreener := &fakeRescreenConsumer{events: &events}
	registry := &fakeTaskMessageConsumer{events: &events}

	routeAgentMessage(
		context.Background(),
		"machine-a",
		&proto.TaskDone{TaskID: "phase2-task"},
		dispatcher,
		rescreener,
		registry,
		slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	)
	want := []string{"phase2", "finalize", "scan"}
	if !equalEventOrder(events, want) || len(events) != len(want) {
		t.Fatalf("events=%v, want %v", events, want)
	}

	events = nil
	dispatcher.events = &events
	dispatcher.recognized = false
	rescreener.events = &events
	registry.events = &events
	routeAgentMessage(
		context.Background(),
		"machine-a",
		&proto.TaskDone{TaskID: "scan-task"},
		dispatcher,
		rescreener,
		registry,
		slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	)
	if rescreener.finalizations != 1 || registry.messages != 2 ||
		!equalEventOrder(events, []string{"phase2", "scan"}) {
		t.Fatalf("scan TaskDone consumed or swept: events=%v finalizations=%d messages=%d",
			events, rescreener.finalizations, registry.messages)
	}
}

func TestReloadDispatchAndFinalizeOrdersM3GenerationBeforeTaskBuild(t *testing.T) {
	events := make([]string, 0)
	rescreener := &fakeRescreenConsumer{
		events: &events, finalizedDone: make(chan struct{}),
	}
	dispatcher := &fakePendingDispatcher{events: &events}
	orchestration := newPhase2Orchestration(rescreener, dispatcher)
	ctx, cancel := context.WithCancel(context.Background())
	orchestration.Start(ctx, nil, phase2FinalizeWorkerConfig{
		AttemptTimeout: time.Second,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     2 * time.Millisecond,
	})
	if err := reloadDispatchAndFinalize(
		context.Background(),
		orchestration,
		true,
	); err != nil {
		t.Fatal(err)
	}
	select {
	case <-rescreener.finalizedDone:
	case <-time.After(time.Second):
		t.Fatal("background finalizer did not run after reload dispatch")
	}
	cancel()
	orchestration.Wait()
	want := []string{"reload", "dispatch", "finalize"}
	if !equalEventOrder(events, want) || len(events) != len(want) {
		t.Fatalf("events=%v, want %v", events, want)
	}

	events = nil
	rescreener.events = &events
	rescreener.reloadErr = errors.New("bad candidate state")
	dispatcher.events = &events
	if err := reloadDispatchAndFinalize(
		context.Background(),
		orchestration,
		true,
	); err == nil {
		t.Fatal("reload error did not stop dispatch")
	}
	if !equalEventOrder(events, []string{"reload"}) || dispatcher.calls != 1 {
		t.Fatalf("reload failure events=%v dispatch calls=%d", events, dispatcher.calls)
	}
}

func TestReloadAfterM3WithoutAutoDispatchDoesNotDispatchOrFinalize(t *testing.T) {
	events := make([]string, 0)
	rescreener := &fakeRescreenConsumer{events: &events}
	dispatcher := &fakePendingDispatcher{events: &events}
	orchestration := newPhase2Orchestration(rescreener, dispatcher)
	if err := reloadDispatchAndFinalize(
		context.Background(),
		orchestration,
		false,
	); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0] != "reload" {
		t.Fatalf("AutoDispatch=false events=%v, want reload only", events)
	}
	if dispatcher.calls != 0 || rescreener.finalizations != 0 {
		t.Fatalf(
			"AutoDispatch=false dispatched=%d finalized=%d",
			dispatcher.calls,
			rescreener.finalizations,
		)
	}
	if finalized, err := orchestration.FinalizeIfIdle(
		context.Background(),
	); err != nil || finalized {
		t.Fatalf(
			"AutoDispatch=false terminal finalization=%v err=%v",
			finalized,
			err,
		)
	}
	if rescreener.finalizations != 0 {
		t.Fatal("AutoDispatch=false generation was finalized by a terminal route")
	}
}

func TestPhase2OrchestrationRebuildsConfirmedKindsOncePerGeneration(
	t *testing.T,
) {
	events := make([]string, 0)
	rescreener := &fakeRescreenConsumer{events: &events, generation: 7}
	rebuilder := &fakeConfirmedGroupRebuilder{events: &events}
	orchestration := newPhase2Orchestration(
		rescreener,
		&fakePendingDispatcher{events: &events},
		rebuilder,
	)

	for call := 0; call < 2; call++ {
		finalized, err := orchestration.FinalizeIfIdle(context.Background())
		if err != nil || !finalized {
			t.Fatalf("finalize call %d result=%t err=%v", call, finalized, err)
		}
	}
	want := []string{"finalize", "image", "video", "finalize"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v, want %v", events, want)
	}
}

func TestPhase2OrchestrationRebuildFailureIsRetryableWithoutRepeatingKind(
	t *testing.T,
) {
	events := make([]string, 0)
	rescreener := &fakeRescreenConsumer{events: &events, generation: 3}
	videoErr := errors.New("video rebuild failed")
	rebuilder := &fakeConfirmedGroupRebuilder{
		events: &events,
		errors: map[string][]error{
			"video": {videoErr, nil},
		},
	}
	orchestration := newPhase2Orchestration(
		rescreener,
		&fakePendingDispatcher{events: &events},
		rebuilder,
	)

	if finalized, err := orchestration.FinalizeIfIdle(
		context.Background(),
	); !errors.Is(err, videoErr) || finalized {
		t.Fatalf("failed rebuild result=%t err=%v", finalized, err)
	}
	if finalized, err := orchestration.FinalizeIfIdle(
		context.Background(),
	); err != nil || !finalized {
		t.Fatalf("retry result=%t err=%v", finalized, err)
	}
	want := []string{
		"finalize", "image", "video",
		"finalize", "video",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v, want %v", events, want)
	}
}

func TestPhase2OrchestrationRebuildsOnlyAfterFinalizationAndNewGeneration(
	t *testing.T,
) {
	events := make([]string, 0)
	notFinalized := false
	rescreener := &fakeRescreenConsumer{
		events: &events, generation: 11, finalized: &notFinalized,
	}
	rebuilder := &fakeConfirmedGroupRebuilder{events: &events}
	dispatcher := &fakePendingDispatcher{events: &events}
	orchestration := newPhase2Orchestration(rescreener, dispatcher, rebuilder)

	if finalized, err := orchestration.FinalizeIfIdle(
		context.Background(),
	); err != nil || finalized {
		t.Fatalf("active finalization=%t err=%v", finalized, err)
	}
	if !reflect.DeepEqual(events, []string{"finalize"}) {
		t.Fatalf("active events=%v", events)
	}

	yes := true
	rescreener.finalized = &yes
	rebuilder.completed = make(chan struct{})
	events = nil
	rescreener.events, dispatcher.events, rebuilder.events =
		&events, &events, &events
	ctx, cancel := context.WithCancel(context.Background())
	orchestration.Start(ctx, nil, phase2FinalizeWorkerConfig{
		AttemptTimeout: time.Second,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     2 * time.Millisecond,
	})
	if err := reloadDispatchAndFinalize(
		context.Background(), orchestration, true,
	); err != nil {
		t.Fatal(err)
	}
	select {
	case <-rebuilder.completed:
	case <-time.After(time.Second):
		t.Fatal("new generation was not rebuilt in background")
	}
	cancel()
	orchestration.Wait()
	want := []string{"reload", "dispatch", "finalize", "image", "video"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("new generation events=%v, want %v", events, want)
	}
}

func TestPhase2OrchestrationRebuildPropagatesCanceledContext(t *testing.T) {
	events := make([]string, 0)
	rescreener := &fakeRescreenConsumer{events: &events, generation: 5}
	rebuilder := &fakeConfirmedGroupRebuilder{
		events:       &events,
		checkContext: true,
	}
	orchestration := newPhase2Orchestration(
		rescreener,
		&fakePendingDispatcher{events: &events},
		rebuilder,
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if finalized, err := orchestration.FinalizeIfIdle(ctx); !errors.Is(err, context.Canceled) || finalized {
		t.Fatalf("canceled rebuild result=%t err=%v", finalized, err)
	}
}

func TestPhase2BackgroundFinalizerRetriesOnlyFailedKindWithoutSecondSignal(
	t *testing.T,
) {
	events := make([]string, 0)
	rescreener := &fakeRescreenConsumer{events: &events, generation: 9}
	videoErr := errors.New("video timeout")
	rebuilder := &fakeConfirmedGroupRebuilder{
		events:    &events,
		errors:    map[string][]error{"video": {videoErr, nil}},
		completed: make(chan struct{}),
	}
	orchestration := newPhase2Orchestration(
		rescreener, &fakePendingDispatcher{events: &events}, rebuilder,
	)
	ctx, cancel := context.WithCancel(context.Background())
	orchestration.Start(ctx, nil, phase2FinalizeWorkerConfig{
		AttemptTimeout: 50 * time.Millisecond,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     2 * time.Millisecond,
	})
	orchestration.SignalFinalize()
	select {
	case <-rebuilder.completed:
	case <-time.After(time.Second):
		t.Fatal("background retry did not complete video without another signal")
	}
	cancel()
	orchestration.Wait()
	if got := rebuilder.kinds(); !reflect.DeepEqual(
		got, []string{"image", "video", "video"},
	) {
		t.Fatalf("rebuild kinds=%v", got)
	}
}

func TestTerminalRouteSignalsBackgroundAndReturnsBeforeFinalize(t *testing.T) {
	events := make([]string, 0)
	blocked := false
	rescreener := &fakeRescreenConsumer{
		events: &events, generation: 2, finalized: &blocked,
		finalizeEntered: make(chan struct{}), releaseFinalize: make(chan struct{}),
	}
	orchestration := newPhase2Orchestration(
		rescreener, &fakePendingDispatcher{events: &events},
	)
	ctx, cancel := context.WithCancel(context.Background())
	orchestration.Start(ctx, nil, phase2FinalizeWorkerConfig{
		AttemptTimeout: time.Second,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     2 * time.Millisecond,
	})
	returned := make(chan struct{})
	go func() {
		routeAgentMessage(
			ctx, "machine-a", &proto.TaskDone{TaskID: "only-terminal"},
			alwaysRecognizedPhase2Dispatcher{}, orchestration,
			&threadSafeTaskConsumer{}, nil,
		)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("terminal route blocked on background finalization")
	}
	select {
	case <-rescreener.finalizeEntered:
	case <-time.After(time.Second):
		t.Fatal("background finalizer was not signaled")
	}
	close(rescreener.releaseFinalize)
	cancel()
	orchestration.Wait()
}

func TestPhase2BackgroundFinalizerCoalescesConcurrentGenerationSignals(
	t *testing.T,
) {
	events := make([]string, 0)
	rescreener := &fakeRescreenConsumer{events: &events, generation: 4}
	rebuilder := &fakeConfirmedGroupRebuilder{
		events: &events, completed: make(chan struct{}),
	}
	orchestration := newPhase2Orchestration(
		rescreener, &fakePendingDispatcher{events: &events}, rebuilder,
	)
	ctx, cancel := context.WithCancel(context.Background())
	orchestration.Start(ctx, nil, phase2FinalizeWorkerConfig{
		AttemptTimeout: time.Second,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     2 * time.Millisecond,
	})
	var signals sync.WaitGroup
	for index := 0; index < 20; index++ {
		signals.Add(1)
		go func() {
			defer signals.Done()
			orchestration.SignalFinalize()
		}()
	}
	signals.Wait()
	select {
	case <-rebuilder.completed:
	case <-time.After(time.Second):
		t.Fatal("coalesced generation did not complete")
	}
	cancel()
	orchestration.Wait()
	if got := rebuilder.kinds(); !reflect.DeepEqual(
		got, []string{"image", "video"},
	) {
		t.Fatalf("concurrent signals rebuilt kinds=%v", got)
	}
}

func TestPhase2BackgroundFinalizerProcessesNewGenerationSignal(t *testing.T) {
	events := make([]string, 0)
	rescreener := &fakeRescreenConsumer{events: &events, generation: 1}
	rebuilder := &fakeConfirmedGroupRebuilder{
		events: &events, completed: make(chan struct{}),
	}
	orchestration := newPhase2Orchestration(
		rescreener, &fakePendingDispatcher{events: &events}, rebuilder,
	)
	ctx, cancel := context.WithCancel(context.Background())
	orchestration.Start(ctx, nil, phase2FinalizeWorkerConfig{
		AttemptTimeout: time.Second,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     2 * time.Millisecond,
	})
	orchestration.SignalFinalize()
	select {
	case <-rebuilder.completed:
	case <-time.After(time.Second):
		t.Fatal("first generation was not rebuilt")
	}

	rebuilder.mu.Lock()
	rebuilder.completed = make(chan struct{})
	rebuilder.completeOnce = sync.Once{}
	secondCompleted := rebuilder.completed
	rebuilder.mu.Unlock()
	rescreener.generation = 2
	orchestration.SignalFinalize()
	select {
	case <-secondCompleted:
	case <-time.After(time.Second):
		t.Fatal("new generation signal was not processed")
	}
	cancel()
	orchestration.Wait()
	if got := rebuilder.kinds(); !reflect.DeepEqual(
		got, []string{"image", "video", "image", "video"},
	) {
		t.Fatalf("generation rebuild kinds=%v", got)
	}
}

func TestPhase2BackgroundFinalizerCancellationStopsRetryAndWaits(t *testing.T) {
	rescreener := &contextBlockingRescreenConsumer{
		entered: make(chan struct{}),
	}
	orchestration := newPhase2Orchestration(
		rescreener, noOpPendingDispatcher{},
	)
	ctx, cancel := context.WithCancel(context.Background())
	orchestration.Start(ctx, nil, phase2FinalizeWorkerConfig{
		AttemptTimeout: time.Minute,
		InitialBackoff: time.Minute,
		MaxBackoff:     time.Minute,
	})
	orchestration.SignalFinalize()
	select {
	case <-rescreener.entered:
	case <-time.After(time.Second):
		t.Fatal("background finalizer did not enter attempt")
	}
	cancel()
	waited := make(chan struct{})
	go func() {
		orchestration.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(time.Second):
		t.Fatal("Wait did not observe worker shutdown after cancellation")
	}
}

type fakePhase2MessageDispatcher struct {
	events     *[]string
	recognized bool
	bound      *phase2.BoundFeatureResult
	bindErr    error
}

func (dispatcher *fakePhase2MessageDispatcher) BindFeatureResult(
	_ string,
	_ *proto.FeatureResult,
) (*phase2.BoundFeatureResult, error) {
	*dispatcher.events = append(*dispatcher.events, "bind")
	return dispatcher.bound, dispatcher.bindErr
}

func (dispatcher *fakePhase2MessageDispatcher) HandleMessage(
	_ string,
	_ any,
) bool {
	*dispatcher.events = append(*dispatcher.events, "phase2")
	return dispatcher.recognized
}

type fakeRescreenConsumer struct {
	events          *[]string
	results         int
	finalizations   int
	reloadErr       error
	generation      uint64
	finalized       *bool
	finalizeErr     error
	finalizeEntered chan struct{}
	releaseFinalize chan struct{}
	finalizeOnce    sync.Once
	finalizedDone   chan struct{}
	finalizedOnce   sync.Once
}

func (consumer *fakeRescreenConsumer) HandleFeatureResult(
	context.Context,
	*phase2.BoundFeatureResult,
) error {
	*consumer.events = append(*consumer.events, "rescreen")
	consumer.results++
	return nil
}

func (consumer *fakeRescreenConsumer) FinalizeIfIdle(
	context.Context,
) (bool, error) {
	*consumer.events = append(*consumer.events, "finalize")
	consumer.finalizations++
	if consumer.finalizeEntered != nil {
		consumer.finalizeOnce.Do(func() { close(consumer.finalizeEntered) })
	}
	if consumer.releaseFinalize != nil {
		<-consumer.releaseFinalize
	}
	if consumer.finalizeErr != nil {
		return false, consumer.finalizeErr
	}
	if consumer.finalized != nil {
		finalized := *consumer.finalized
		if finalized && consumer.finalizedDone != nil {
			consumer.finalizedOnce.Do(func() { close(consumer.finalizedDone) })
		}
		return finalized, nil
	}
	if consumer.finalizedDone != nil {
		consumer.finalizedOnce.Do(func() { close(consumer.finalizedDone) })
	}
	return true, nil
}

type contextBlockingRescreenConsumer struct {
	entered chan struct{}
	once    sync.Once
}

func (consumer *contextBlockingRescreenConsumer) HandleFeatureResult(
	context.Context,
	*phase2.BoundFeatureResult,
) error {
	return nil
}

func (consumer *contextBlockingRescreenConsumer) FinalizeIfIdle(
	ctx context.Context,
) (bool, error) {
	consumer.once.Do(func() { close(consumer.entered) })
	<-ctx.Done()
	return false, ctx.Err()
}

func (*contextBlockingRescreenConsumer) Reload(context.Context) error {
	return nil
}

func (*contextBlockingRescreenConsumer) Progress() phase2.RescreenProgress {
	return phase2.RescreenProgress{}
}

func (consumer *fakeRescreenConsumer) Reload(context.Context) error {
	*consumer.events = append(*consumer.events, "reload")
	if consumer.reloadErr == nil {
		consumer.generation++
	}
	return consumer.reloadErr
}

func (consumer *fakeRescreenConsumer) Progress() phase2.RescreenProgress {
	return phase2.RescreenProgress{Generation: consumer.generation}
}

type fakeConfirmedGroupRebuilder struct {
	mu           sync.Mutex
	events       *[]string
	errors       map[string][]error
	checkContext bool
	completed    chan struct{}
	completeOnce sync.Once
	calls        []string
}

func (rebuilder *fakeConfirmedGroupRebuilder) RebuildGroups(
	ctx context.Context,
	kind string,
) (phase2.GroupStats, error) {
	rebuilder.mu.Lock()
	defer rebuilder.mu.Unlock()
	*rebuilder.events = append(*rebuilder.events, kind)
	rebuilder.calls = append(rebuilder.calls, kind)
	if rebuilder.checkContext && ctx.Err() != nil {
		return phase2.GroupStats{}, ctx.Err()
	}
	if len(rebuilder.errors[kind]) != 0 {
		err := rebuilder.errors[kind][0]
		rebuilder.errors[kind] = rebuilder.errors[kind][1:]
		if err != nil {
			return phase2.GroupStats{}, err
		}
	}
	if kind == "video" && len(rebuilder.errors[kind]) == 0 &&
		rebuilder.completed != nil {
		rebuilder.completeOnce.Do(func() { close(rebuilder.completed) })
	}
	return phase2.GroupStats{}, nil
}

func (rebuilder *fakeConfirmedGroupRebuilder) kinds() []string {
	rebuilder.mu.Lock()
	defer rebuilder.mu.Unlock()
	return append([]string(nil), rebuilder.calls...)
}

type fakeTaskMessageConsumer struct {
	events   *[]string
	messages int
}

func (consumer *fakeTaskMessageConsumer) Dispatch(string, any) {
	*consumer.events = append(*consumer.events, "scan")
	consumer.messages++
}

type fakePendingDispatcher struct {
	events *[]string
	calls  int
}

func (dispatcher *fakePendingDispatcher) DispatchPending(context.Context) error {
	*dispatcher.events = append(*dispatcher.events, "dispatch")
	dispatcher.calls++
	return nil
}

func TestPhase2OrchestrationDoesNotSweepReloadedGenerationBeforeDispatchPersists(
	t *testing.T,
) {
	dispatcher := newLinearizedTestDispatcher()
	rescreener := newLinearizedTestRescreener(dispatcher.active)
	tasks := &threadSafeTaskConsumer{}
	orchestration := newPhase2Orchestration(rescreener, dispatcher)
	ctx, cancel := context.WithCancel(context.Background())
	orchestration.Start(ctx, nil, phase2FinalizeWorkerConfig{
		AttemptTimeout: time.Second,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     2 * time.Millisecond,
	})
	defer func() {
		cancel()
		orchestration.Wait()
	}()

	hookDone := make(chan error, 1)
	go func() {
		hookDone <- reloadDispatchAndFinalize(
			context.Background(),
			orchestration,
			true,
		)
	}()
	select {
	case <-dispatcher.dispatchEntered:
	case <-time.After(time.Second):
		t.Fatal("M3 hook did not reach pre-persistence dispatch boundary")
	}
	if unresolved, callbacks := rescreener.state(); unresolved != 1 || callbacks != 0 {
		t.Fatalf("reloaded state unresolved=%d callbacks=%d", unresolved, callbacks)
	}

	oldTerminalDone := make(chan struct{})
	go func() {
		routeAgentMessage(
			context.Background(),
			"machine-a",
			&proto.TaskDone{TaskID: "old-generation-task"},
			dispatcher,
			orchestration,
			tasks,
			nil,
		)
		close(oldTerminalDone)
	}()
	select {
	case <-dispatcher.oldTerminalHandled:
	case <-time.After(time.Second):
		t.Fatal("old TaskDone did not complete Task 7 durable handling")
	}
	select {
	case <-rescreener.finalizeEntered:
		t.Fatal("old terminal finalizer entered before new tasks were persisted")
	case <-time.After(100 * time.Millisecond):
	}
	if unresolved, callbacks := rescreener.state(); unresolved != 1 || callbacks != 0 {
		t.Fatalf(
			"pre-persistence terminal swept generation: unresolved=%d callbacks=%d",
			unresolved,
			callbacks,
		)
	}

	close(dispatcher.releaseDispatch)
	select {
	case <-dispatcher.dispatchPersisted:
	case <-time.After(time.Second):
		t.Fatal("new task was not durably admitted")
	}
	fastResultDone := make(chan struct{})
	go func() {
		routeAgentMessage(
			context.Background(),
			"machine-a",
			&proto.FeatureResult{TaskID: "new-generation-task"},
			dispatcher,
			orchestration,
			tasks,
			nil,
		)
		close(fastResultDone)
	}()
	select {
	case <-fastResultDone:
	case <-time.After(time.Second):
		t.Fatal("feature aggregation was unnecessarily blocked by orchestration gate")
	}
	newTerminalDone := make(chan struct{})
	go func() {
		routeAgentMessage(
			context.Background(),
			"machine-a",
			&proto.TaskDone{TaskID: "new-generation-task"},
			dispatcher,
			orchestration,
			tasks,
			nil,
		)
		close(newTerminalDone)
	}()
	select {
	case <-dispatcher.newTerminalHandled:
	case <-time.After(time.Second):
		t.Fatal("new TaskDone did not complete Task 7 durable handling")
	}
	select {
	case <-newTerminalDone:
	case <-time.After(time.Second):
		t.Fatal("new terminal route blocked on background finalization")
	}
	if unresolved, callbacks := rescreener.state(); unresolved != 0 || callbacks != 1 {
		t.Fatalf(
			"fast pre-return result unresolved=%d callbacks=%d, want 0/1",
			unresolved,
			callbacks,
		)
	}
	close(dispatcher.releaseDispatchReturn)
	select {
	case err := <-hookDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("M3 hook deadlocked after dispatch persistence")
	}
	select {
	case <-rescreener.finalizeEntered:
	case <-time.After(time.Second):
		t.Fatal("background finalizer did not run after orchestration release")
	}
	select {
	case <-oldTerminalDone:
	case <-time.After(time.Second):
		t.Fatal("old terminal route did not finish after orchestration release")
	}
	select {
	case <-newTerminalDone:
	case <-time.After(time.Second):
		t.Fatal("new terminal route deadlocked after dispatch returned")
	}
	if unresolved, callbacks := rescreener.state(); unresolved != 0 || callbacks != 1 {
		t.Fatalf(
			"fast result/TaskDone state unresolved=%d callbacks=%d, want 0/1",
			unresolved,
			callbacks,
		)
	}
	if tasks.count() != 3 {
		t.Fatalf("legacy broadcasts=%d, want old terminal/result/new terminal", tasks.count())
	}
}

func TestPhase2OrchestrationReleasesGateAfterReloadOrDispatchError(t *testing.T) {
	for _, test := range []struct {
		name        string
		reloadErr   error
		dispatchErr error
	}{
		{name: "reload", reloadErr: errors.New("reload failed")},
		{name: "dispatch", dispatchErr: errors.New("dispatch failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			dispatcher := &linearizedTestDispatcher{
				dispatchErr: test.dispatchErr,
			}
			rescreener := newLinearizedTestRescreener(dispatcher.active)
			rescreener.reloadErr = test.reloadErr
			orchestration := newPhase2Orchestration(rescreener, dispatcher)
			if err := reloadDispatchAndFinalize(
				context.Background(),
				orchestration,
				true,
			); err == nil {
				t.Fatal("orchestration error was hidden")
			}

			finalized := make(chan struct{})
			go func() {
				_, _ = orchestration.FinalizeIfIdle(context.Background())
				close(finalized)
			}()
			select {
			case <-finalized:
			case <-time.After(time.Second):
				t.Fatal("orchestration gate remained locked after error")
			}
		})
	}
}

func TestPhase2OrchestrationArmsOnlyAfterDurableTaskAdmission(t *testing.T) {
	t.Run("transport error after complete admission remains terminal-finalizable", func(t *testing.T) {
		dispatcher := &linearizedTestDispatcher{
			dispatchErr: durablyAdmittedTestError{
				err: errors.New("send failed"),
			},
		}
		rescreener := newLinearizedTestRescreener(dispatcher.active)
		orchestration := newPhase2Orchestration(rescreener, dispatcher)
		if err := reloadDispatchAndFinalize(
			context.Background(),
			orchestration,
			true,
		); err == nil {
			t.Fatal("transport error was hidden")
		}
		if !dispatcher.active() {
			t.Fatal("fully admitted pending task was not recorded active")
		}
		if _, err := orchestration.FinalizeIfIdle(context.Background()); err != nil {
			t.Fatal(err)
		}
		if finalizations := rescreener.finalizationCount(); finalizations != 1 {
			t.Fatalf("armed generation finalizations=%d, want barrier check", finalizations)
		}
		if unresolved, callbacks := rescreener.state(); unresolved != 1 || callbacks != 0 {
			t.Fatalf("active barrier state unresolved=%d callbacks=%d", unresolved, callbacks)
		}

		dispatcher.setActive(false)
		if _, err := orchestration.FinalizeIfIdle(context.Background()); err != nil {
			t.Fatal(err)
		}
		if unresolved, callbacks := rescreener.state(); unresolved != 0 || callbacks != 1 {
			t.Fatalf(
				"terminal admitted state unresolved=%d callbacks=%d, want 0/1",
				unresolved,
				callbacks,
			)
		}
	})

	t.Run("admission failure remains unarmed", func(t *testing.T) {
		dispatcher := &linearizedTestDispatcher{
			dispatchErr: errors.New("first persist failed"),
		}
		rescreener := newLinearizedTestRescreener(dispatcher.active)
		orchestration := newPhase2Orchestration(rescreener, dispatcher)
		if err := reloadDispatchAndFinalize(
			context.Background(),
			orchestration,
			true,
		); err == nil {
			t.Fatal("admission error was hidden")
		}
		if finalized, err := orchestration.FinalizeIfIdle(
			context.Background(),
		); err != nil || finalized {
			t.Fatalf("unarmed finalization=%v err=%v", finalized, err)
		}
		if finalizations := rescreener.finalizationCount(); finalizations != 0 {
			t.Fatalf("admission failure reached barrier %d times", finalizations)
		}
		if unresolved, callbacks := rescreener.state(); unresolved != 1 || callbacks != 0 {
			t.Fatalf(
				"admission failure swept state unresolved=%d callbacks=%d",
				unresolved,
				callbacks,
			)
		}
	})
}

func TestPhase2OrchestrationReloadErrorUsesObservedGenerationChange(
	t *testing.T,
) {
	t.Run("post-install error keeps new generation unarmed", func(t *testing.T) {
		dispatcher := &linearizedTestDispatcher{
			oldTerminalHandled: make(chan struct{}),
		}
		rescreener := newLinearizedTestRescreener(dispatcher.active)
		rescreener.reloadErr = errors.New("ready pair upsert failed")
		rescreener.installBeforeReloadError = true
		rescreener.reloadInstalled = make(chan struct{})
		rescreener.releaseReload = make(chan struct{})
		tasks := &threadSafeTaskConsumer{}
		orchestration := newPhase2Orchestration(rescreener, dispatcher)

		hookDone := make(chan error, 1)
		go func() {
			hookDone <- reloadDispatchAndFinalize(
				context.Background(),
				orchestration,
				true,
			)
		}()
		select {
		case <-rescreener.reloadInstalled:
		case <-time.After(time.Second):
			t.Fatal("reload did not install the replacement generation")
		}
		oldTerminalDone := make(chan struct{})
		go func() {
			routeAgentMessage(
				context.Background(),
				"machine-a",
				&proto.TaskDone{TaskID: "old-generation-task"},
				dispatcher,
				orchestration,
				tasks,
				nil,
			)
			close(oldTerminalDone)
		}()
		select {
		case <-dispatcher.oldTerminalHandled:
		case <-time.After(time.Second):
			t.Fatal("old TaskDone did not complete Task 7 durable handling")
		}
		select {
		case <-rescreener.finalizeEntered:
			t.Fatal("old terminal entered finalizer while errored reload held gate")
		case <-time.After(100 * time.Millisecond):
		}

		close(rescreener.releaseReload)
		select {
		case err := <-hookDone:
			if err == nil {
				t.Fatal("post-install reload error was hidden")
			}
		case <-time.After(time.Second):
			t.Fatal("post-install reload did not return")
		}
		select {
		case <-oldTerminalDone:
		case <-time.After(time.Second):
			t.Fatal("queued old terminal did not leave the released gate")
		}
		if finalizations := rescreener.finalizationCount(); finalizations != 0 {
			t.Fatalf("post-install error reached finalizer %d times", finalizations)
		}
		generation, unresolved, callbacks := rescreener.generationState()
		if generation != 1 || unresolved != 1 || callbacks != 0 {
			t.Fatalf(
				"post-install state generation=%d unresolved=%d callbacks=%d",
				generation,
				unresolved,
				callbacks,
			)
		}

		rescreener.reloadErr = nil
		rescreener.installBeforeReloadError = false
		rescreener.reloadInstalled = nil
		rescreener.releaseReload = nil
		if err := reloadDispatchAndFinalize(
			context.Background(),
			orchestration,
			true,
		); err != nil {
			t.Fatal(err)
		}
		if !dispatcher.active() {
			t.Fatal("successful retry did not durably arm replacement generation")
		}
		dispatcher.setActive(false)
		if _, err := orchestration.FinalizeIfIdle(context.Background()); err != nil {
			t.Fatal(err)
		}
		generation, unresolved, callbacks = rescreener.generationState()
		if generation != 2 || unresolved != 0 || callbacks != 1 {
			t.Fatalf(
				"successful retry state generation=%d unresolved=%d callbacks=%d",
				generation,
				unresolved,
				callbacks,
			)
		}
	})

	t.Run("pre-install error restores prior armed generation", func(t *testing.T) {
		dispatcher := &linearizedTestDispatcher{}
		rescreener := newLinearizedTestRescreener(dispatcher.active)
		rescreener.generation = 7
		rescreener.unresolved = 1
		rescreener.reloadErr = errors.New("reconcile failed")
		orchestration := newPhase2Orchestration(rescreener, dispatcher)
		if err := reloadDispatchAndFinalize(
			context.Background(),
			orchestration,
			true,
		); err == nil {
			t.Fatal("pre-install reload error was hidden")
		}
		if _, err := orchestration.FinalizeIfIdle(context.Background()); err != nil {
			t.Fatal(err)
		}
		generation, unresolved, callbacks := rescreener.generationState()
		if generation != 7 || unresolved != 0 || callbacks != 1 {
			t.Fatalf(
				"prior generation state generation=%d unresolved=%d callbacks=%d",
				generation,
				unresolved,
				callbacks,
			)
		}
	})
}

func TestPhase2OrchestrationSerializesConcurrentTerminalFinalizers(t *testing.T) {
	const terminalCount = 20
	rescreener := newBlockingFinalizeRescreener(terminalCount)
	dispatcher := alwaysRecognizedPhase2Dispatcher{}
	tasks := &threadSafeTaskConsumer{}
	orchestration := newPhase2Orchestration(
		rescreener,
		noOpPendingDispatcher{},
	)
	ctx, cancel := context.WithCancel(context.Background())
	orchestration.Start(ctx, nil, phase2FinalizeWorkerConfig{
		AttemptTimeout: time.Second,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     2 * time.Millisecond,
	})

	var routes sync.WaitGroup
	for index := 0; index < terminalCount; index++ {
		routes.Add(1)
		go func(index int) {
			defer routes.Done()
			routeAgentMessage(
				context.Background(),
				"machine-a",
				&proto.TaskDone{TaskID: fmt.Sprintf("terminal-%d", index)},
				dispatcher,
				orchestration,
				tasks,
				nil,
			)
		}(index)
	}
	select {
	case <-rescreener.entered:
	case <-time.After(time.Second):
		t.Fatal("no terminal finalizer entered")
	}
	select {
	case <-rescreener.entered:
		close(rescreener.release)
		t.Fatal("concurrent terminal finalizers bypassed orchestration gate")
	case <-time.After(100 * time.Millisecond):
	}
	close(rescreener.release)
	done := make(chan struct{})
	go func() {
		routes.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("concurrent terminal routes deadlocked")
	}
	cancel()
	orchestration.Wait()
	calls, maximum := rescreener.counts()
	if calls < 1 || calls > 2 || maximum != 1 || tasks.count() != terminalCount {
		t.Fatalf(
			"finalizers calls=%d max-concurrent=%d broadcasts=%d",
			calls,
			maximum,
			tasks.count(),
		)
	}
}

type linearizedTestDispatcher struct {
	mu sync.Mutex

	dispatchEntered       chan struct{}
	releaseDispatch       chan struct{}
	dispatchOnce          sync.Once
	dispatchPersisted     chan struct{}
	persistedOnce         sync.Once
	releaseDispatchReturn chan struct{}
	newTerminalHandled    chan struct{}
	terminalOnce          sync.Once
	oldTerminalHandled    chan struct{}
	oldTerminalOnce       sync.Once
	dispatchErr           error
	newTaskActive         bool
}

func newLinearizedTestDispatcher() *linearizedTestDispatcher {
	return &linearizedTestDispatcher{
		dispatchEntered:       make(chan struct{}),
		releaseDispatch:       make(chan struct{}),
		dispatchPersisted:     make(chan struct{}),
		releaseDispatchReturn: make(chan struct{}),
		newTerminalHandled:    make(chan struct{}),
		oldTerminalHandled:    make(chan struct{}),
	}
}

func (dispatcher *linearizedTestDispatcher) DispatchPending(
	context.Context,
) error {
	if dispatcher.dispatchEntered != nil {
		dispatcher.dispatchOnce.Do(func() {
			close(dispatcher.dispatchEntered)
		})
		<-dispatcher.releaseDispatch
	}
	if dispatcher.dispatchErr != nil {
		var admission interface{ DurablyAdmitted() bool }
		if errors.As(dispatcher.dispatchErr, &admission) &&
			admission.DurablyAdmitted() {
			dispatcher.setActive(true)
		}
		return dispatcher.dispatchErr
	}
	dispatcher.setActive(true)
	if dispatcher.dispatchPersisted != nil {
		dispatcher.persistedOnce.Do(func() {
			close(dispatcher.dispatchPersisted)
		})
	}
	if dispatcher.releaseDispatchReturn != nil {
		<-dispatcher.releaseDispatchReturn
	}
	return nil
}

func (dispatcher *linearizedTestDispatcher) BindFeatureResult(
	_ string,
	result *proto.FeatureResult,
) (*phase2.BoundFeatureResult, error) {
	return &phase2.BoundFeatureResult{
		TaskID: result.TaskID,
		Items: []phase2.BoundFeatureItem{{
			Kind: proto.KindImage,
			Item: proto.FeatureItem{
				SHA512: strings.Repeat("a", 128),
				Status: proto.StatusDone,
			},
		}},
	}, nil
}

func (dispatcher *linearizedTestDispatcher) HandleMessage(
	_ string,
	message any,
) bool {
	if done, ok := message.(*proto.TaskDone); ok &&
		done.TaskID == "new-generation-task" {
		dispatcher.setActive(false)
		if dispatcher.newTerminalHandled != nil {
			dispatcher.terminalOnce.Do(func() {
				close(dispatcher.newTerminalHandled)
			})
		}
	} else if done, ok := message.(*proto.TaskDone); ok &&
		done.TaskID == "old-generation-task" &&
		dispatcher.oldTerminalHandled != nil {
		dispatcher.oldTerminalOnce.Do(func() {
			close(dispatcher.oldTerminalHandled)
		})
	}
	return true
}

func (dispatcher *linearizedTestDispatcher) active() bool {
	dispatcher.mu.Lock()
	defer dispatcher.mu.Unlock()
	return dispatcher.newTaskActive
}

func (dispatcher *linearizedTestDispatcher) setActive(active bool) {
	dispatcher.mu.Lock()
	dispatcher.newTaskActive = active
	dispatcher.mu.Unlock()
}

type linearizedTestRescreener struct {
	mu sync.Mutex

	active                   func() bool
	generation               uint64
	unresolved               int
	callbacks                int
	finalizations            int
	reloadErr                error
	installBeforeReloadError bool
	reloadInstalled          chan struct{}
	releaseReload            chan struct{}
	finalizeEntered          chan struct{}
}

func newLinearizedTestRescreener(
	active func() bool,
) *linearizedTestRescreener {
	return &linearizedTestRescreener{
		active:          active,
		finalizeEntered: make(chan struct{}, 8),
	}
}

func (rescreener *linearizedTestRescreener) Reload(context.Context) error {
	if rescreener.reloadErr != nil && !rescreener.installBeforeReloadError {
		return rescreener.reloadErr
	}
	rescreener.mu.Lock()
	rescreener.generation++
	rescreener.unresolved = 1
	rescreener.mu.Unlock()
	if rescreener.reloadInstalled != nil {
		close(rescreener.reloadInstalled)
	}
	if rescreener.releaseReload != nil {
		<-rescreener.releaseReload
	}
	return rescreener.reloadErr
}

func (rescreener *linearizedTestRescreener) HandleFeatureResult(
	context.Context,
	*phase2.BoundFeatureResult,
) error {
	rescreener.mu.Lock()
	if rescreener.unresolved != 0 {
		rescreener.unresolved = 0
		rescreener.callbacks++
	}
	rescreener.mu.Unlock()
	return nil
}

func (rescreener *linearizedTestRescreener) FinalizeIfIdle(
	context.Context,
) (bool, error) {
	rescreener.finalizeEntered <- struct{}{}
	active := rescreener.active()
	rescreener.mu.Lock()
	rescreener.finalizations++
	if !active && rescreener.unresolved != 0 {
		rescreener.unresolved = 0
		rescreener.callbacks++
	}
	rescreener.mu.Unlock()
	return !active, nil
}

func (rescreener *linearizedTestRescreener) finalizationCount() int {
	rescreener.mu.Lock()
	defer rescreener.mu.Unlock()
	return rescreener.finalizations
}

func (rescreener *linearizedTestRescreener) state() (int, int) {
	rescreener.mu.Lock()
	defer rescreener.mu.Unlock()
	return rescreener.unresolved, rescreener.callbacks
}

func (rescreener *linearizedTestRescreener) Progress() phase2.RescreenProgress {
	rescreener.mu.Lock()
	defer rescreener.mu.Unlock()
	return phase2.RescreenProgress{
		Generation:      rescreener.generation,
		UnresolvedPairs: rescreener.unresolved,
	}
}

func (rescreener *linearizedTestRescreener) generationState() (uint64, int, int) {
	rescreener.mu.Lock()
	defer rescreener.mu.Unlock()
	return rescreener.generation, rescreener.unresolved, rescreener.callbacks
}

type threadSafeTaskConsumer struct {
	mu       sync.Mutex
	messages int
}

func (consumer *threadSafeTaskConsumer) Dispatch(string, any) {
	consumer.mu.Lock()
	consumer.messages++
	consumer.mu.Unlock()
}

func (consumer *threadSafeTaskConsumer) count() int {
	consumer.mu.Lock()
	defer consumer.mu.Unlock()
	return consumer.messages
}

type blockingFinalizeRescreener struct {
	mu sync.Mutex

	entered    chan struct{}
	release    chan struct{}
	active     int
	maximum    int
	finalizers int
}

func newBlockingFinalizeRescreener(capacity int) *blockingFinalizeRescreener {
	return &blockingFinalizeRescreener{
		entered: make(chan struct{}, capacity),
		release: make(chan struct{}),
	}
}

func (*blockingFinalizeRescreener) Reload(context.Context) error {
	return nil
}

func (*blockingFinalizeRescreener) HandleFeatureResult(
	context.Context,
	*phase2.BoundFeatureResult,
) error {
	return nil
}

func (*blockingFinalizeRescreener) Progress() phase2.RescreenProgress {
	return phase2.RescreenProgress{}
}

func (rescreener *blockingFinalizeRescreener) FinalizeIfIdle(
	context.Context,
) (bool, error) {
	rescreener.mu.Lock()
	rescreener.active++
	rescreener.finalizers++
	if rescreener.active > rescreener.maximum {
		rescreener.maximum = rescreener.active
	}
	rescreener.mu.Unlock()
	rescreener.entered <- struct{}{}
	<-rescreener.release
	rescreener.mu.Lock()
	rescreener.active--
	rescreener.mu.Unlock()
	return false, nil
}

func (rescreener *blockingFinalizeRescreener) counts() (int, int) {
	rescreener.mu.Lock()
	defer rescreener.mu.Unlock()
	return rescreener.finalizers, rescreener.maximum
}

type alwaysRecognizedPhase2Dispatcher struct{}

func (alwaysRecognizedPhase2Dispatcher) BindFeatureResult(
	string,
	*proto.FeatureResult,
) (*phase2.BoundFeatureResult, error) {
	return nil, nil
}

func (alwaysRecognizedPhase2Dispatcher) HandleMessage(string, any) bool {
	return true
}

type noOpPendingDispatcher struct{}

func (noOpPendingDispatcher) DispatchPending(context.Context) error {
	return nil
}

type durablyAdmittedTestError struct {
	err error
}

func (err durablyAdmittedTestError) Error() string {
	return err.err.Error()
}

func (durablyAdmittedTestError) DurablyAdmitted() bool {
	return true
}
