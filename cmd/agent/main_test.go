package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dedup/internal/agent"
	agentdelete "dedup/internal/agent/delete"
	"dedup/internal/agentcontrol"
	"dedup/internal/config"
	"dedup/internal/machineid"
	"dedup/internal/proto"
)

func fixedMachineIdentity(fill string) machineIdentityProvider {
	return func() (machineid.Result, error) {
		return machineid.Result{
			ID:              "node-" + strings.Repeat(fill, 64),
			CPUAvailable:    true,
			BoardAvailable:  true,
			SystemAvailable: true,
		}, nil
	}
}

func TestEffectiveConfigSHA256MatchesNodeTrayCanonicalEncoding(t *testing.T) {
	cfg := config.DefaultAgent()
	cfg.MachineID = "fingerprint-contract"
	cfg.PGDSN = "postgres://fixture.invalid/dedup"

	canonical, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	canonical = append(canonical, '\n')
	want := fmt.Sprintf("%x", sha256.Sum256(canonical))

	got, err := effectiveConfigSHA256(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("Agent config fingerprint = %s, want NodeTray canonical fingerprint %s", got, want)
	}
	cfg.MachineID = "another-runtime-identity"
	again, err := effectiveConfigSHA256(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if again != got {
		t.Fatalf("runtime machine identity changed config fingerprint: %s != %s", again, got)
	}
}

func TestRunWithDependenciesStopsBeforeResourcesWhenIdentityUnavailable(t *testing.T) {
	root := t.TempDir()
	cfg := config.DefaultAgent()
	cfg.PGDSN = "postgres://fixture.invalid/dedup"
	cfg.DataDir = filepath.Join(root, "data")
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "agent.json")
	if err := os.WriteFile(configPath, body, 0o600); err != nil {
		t.Fatal(err)
	}

	sentinel := errors.New("hardware sources unavailable")
	loggerOpened := false
	err = runWithDependencies(
		configPath,
		func(string) (*slog.Logger, func() error, error) {
			loggerOpened = true
			return nil, nil, errors.New("must not be called")
		},
		func() (machineid.Result, error) { return machineid.Result{}, sentinel },
	)
	if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "resolve Agent machine identity") {
		t.Fatalf("runWithDependencies error = %v", err)
	}
	if loggerOpened {
		t.Fatal("identity failure opened the delete logger")
	}
	if _, statErr := os.Stat(cfg.DataDir); !os.IsNotExist(statErr) {
		t.Fatalf("identity failure created data directory: %v", statErr)
	}
}

func TestWorkerPoolConfigMapsAllAgentWorkerSettingsAndExactEnv(t *testing.T) {
	cfg := config.DefaultAgent()
	cfg.MachineID = "machine-a"
	cfg.Worker.Count = 7
	cfg.Worker.ExePath = `D:\portable\worker.exe`
	cfg.Worker.ImageTimeoutS = 31
	cfg.Worker.VideoTimeoutS = 121
	cfg.Worker.RespawnDelayMS = 750
	cfg.Worker.ImageMemoryMB = 128
	cfg.Worker.CrashInjection = true
	cfg.Pipeline.ReadChunkKB = 2048
	cfg.Thumb.CacheDir = `D:\cache`
	cfg.Thumb.TileMaxSide = 512
	cfg.Thumb.ProbeTimeoutS = 16
	cfg.Thumb.NativeTimeoutS = 61
	cfg.Thumb.FrameTimeoutS = 21
	cfg.IPC.MaxFrameMB = 8

	got := workerPoolConfig(cfg)
	if got.WorkerExe != cfg.Worker.ExePath ||
		got.WorkerCount != 7 ||
		got.MachineID != "machine-a" ||
		got.ImageTimeout != 31*time.Second ||
		got.VideoTimeout != 121*time.Second ||
		got.RespawnDelay != 750*time.Millisecond ||
		got.IPCMaxFrameBytes != 8<<20 ||
		!reflect.DeepEqual(got.WorkerEnv, cfg.WorkerEnv()) {
		t.Fatalf("worker pool config = %#v", got)
	}
}

func TestRunServiceClosesStartedPoolWhenServerReturnsError(t *testing.T) {
	sentinel := errors.New("listen failed")
	pool := &lifecyclePool{}
	err := runService(pool, func() error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("runService error=%v, want %v", err, sentinel)
	}
	if pool.starts != 1 || pool.closes != 1 {
		t.Fatalf("pool lifecycle starts=%d closes=%d", pool.starts, pool.closes)
	}
}

func TestRunServiceDrainsPhase2BeforeClosingPool(t *testing.T) {
	var events []string
	pool := &orderedLifecyclePool{events: &events}
	err := runService(
		pool,
		func() error {
			events = append(events, "serve")
			return nil
		},
		func() error {
			events = append(events, "drain")
			if pool.closed {
				t.Fatal("pool closed before Phase2 drain")
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"start", "serve", "drain", "close"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("lifecycle events=%v, want %v", events, want)
	}
}

func TestControlShutdownCancelsRootThenDrainsAndClosesPoolOnce(t *testing.T) {
	root, cancel := context.WithCancel(context.Background())
	pool := &countingLifecyclePool{}
	var businessReady atomic.Bool
	var drainCalls atomic.Int32
	err := runControlledService(
		root,
		cancel,
		pool,
		func(ctx context.Context, ready func()) error {
			businessReady.Store(true)
			ready()
			<-ctx.Done()
			return ctx.Err()
		},
		func(ctx context.Context) error {
			if !businessReady.Load() {
				return errors.New("control started before business listener")
			}
			cancel()
			<-ctx.Done()
			return ctx.Err()
		},
		func() error {
			drainCalls.Add(1)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("controlled shutdown error = %v", err)
	}
	if pool.starts.Load() != 1 || pool.closes.Load() != 1 || drainCalls.Load() != 1 {
		t.Fatalf("lifecycle starts=%d closes=%d drains=%d, want 1/1/1",
			pool.starts.Load(), pool.closes.Load(), drainCalls.Load())
	}
}

func TestControlServiceFailureCancelsRootReturnsErrorAndCleansOnce(t *testing.T) {
	root, cancel := context.WithCancel(context.Background())
	pool := &countingLifecyclePool{}
	sentinel := errors.New("control listener failed")
	var drainCalls atomic.Int32
	err := runControlledService(
		root,
		cancel,
		pool,
		func(ctx context.Context, ready func()) error {
			ready()
			<-ctx.Done()
			return ctx.Err()
		},
		func(context.Context) error { return sentinel },
		func() error {
			drainCalls.Add(1)
			return nil
		},
	)
	if !errors.Is(err, sentinel) {
		t.Fatalf("controlled service error = %v, want %v", err, sentinel)
	}
	if root.Err() == nil {
		t.Fatal("control failure did not cancel Agent root context")
	}
	if pool.starts.Load() != 1 || pool.closes.Load() != 1 || drainCalls.Load() != 1 {
		t.Fatalf("failure cleanup starts=%d closes=%d drains=%d, want 1/1/1",
			pool.starts.Load(), pool.closes.Load(), drainCalls.Load())
	}
}

func TestControlBusinessReadinessDoesNotDependOnInfoLogFiltering(t *testing.T) {
	ready := make(chan struct{}, 1)
	logger := slog.New(&listenerReadyHandler{
		next: disabledInfoHandler{},
		ready: func() {
			ready <- struct{}{}
		},
	})
	logger.Info("agent listening", "addr", "127.0.0.1:9101")
	select {
	case <-ready:
	default:
		t.Fatal("business listener readiness was suppressed with Info logging")
	}
}

func TestControlStartupRejectsOversizedExecutablePathBeforeOpeningResources(t *testing.T) {
	cfg := config.DefaultAgent()
	cfg.MachineID = "machine-control-identity"
	cfg.PGDSN = "postgres://user:secret@fictional.invalid/dedup"
	executable := strings.Repeat("x", 1025)
	err := validateAgentControlIdentity(cfg, executable)
	if err == nil {
		t.Fatal("control startup accepted executable path beyond protocol bound")
	}
	if strings.Contains(err.Error(), executable) || strings.Contains(err.Error(), cfg.PGDSN) ||
		strings.Contains(err.Error(), "secret") {
		t.Fatalf("control identity error echoed rejected path or sensitive config: %v", err)
	}
}

func TestSingleInstanceReleasedWhenAgentStartupFails(t *testing.T) {
	root := t.TempDir()
	cfg := config.DefaultAgent()
	cfg.PGDSN = "://invalid-postgres-dsn"
	cfg.DataDir = filepath.Join(root, "data")
	cfg.UseEverything = false
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "agent.json")
	if err := os.WriteFile(configPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	resolveIdentity := fixedMachineIdentity("a")
	if err := runWithDependencies(configPath, agent.NewDeleteLogger, resolveIdentity); err == nil {
		t.Fatal("run unexpectedly accepted invalid PostgreSQL DSN")
	}
	identity, err := resolveIdentity()
	if err != nil {
		t.Fatal(err)
	}
	lock, err := agentcontrol.AcquireSingleInstance(identity.ID)
	if err != nil {
		t.Fatalf("startup failure retained single-instance lock: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDrainPhase2UsesBoundedProductionContext(t *testing.T) {
	cfg := config.DefaultAgent()
	cfg.Worker.ImageTimeoutS = 31
	cfg.Worker.VideoTimeoutS = 121
	timeout := phase2DrainTimeout(cfg)
	if timeout <= 121*time.Second {
		t.Fatalf("Phase2 drain timeout=%v must include worker-exit grace", timeout)
	}
	shutdown := &deadlineShutdown{}
	if err := drainPhase2(shutdown, timeout); err != nil {
		t.Fatal(err)
	}
	if !shutdown.hasDeadline {
		t.Fatal("production Phase2 drain received an unbounded context")
	}
	if shutdown.remaining <= 0 || shutdown.remaining > timeout {
		t.Fatalf(
			"production Phase2 drain deadline remaining=%v, timeout=%v",
			shutdown.remaining,
			timeout,
		)
	}
}

func TestRunClosesOpenedResourcesWhenPostgresConfigurationFails(t *testing.T) {
	root := t.TempDir()
	cfg := config.DefaultAgent()
	cfg.MachineID = "machine-cleanup"
	cfg.PGDSN = "://invalid-postgres-dsn"
	cfg.DataDir = filepath.Join(root, "data")
	cfg.UseEverything = false
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "agent.json")
	if err := os.WriteFile(configPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runWithDependencies(configPath, agent.NewDeleteLogger, fixedMachineIdentity("b")); err == nil {
		t.Fatal("run unexpectedly accepted invalid PostgreSQL DSN")
	}
	for _, name := range []string{
		"agent.db", "agent.log", "errors.log", "crash.log", "delete.log",
	} {
		path := filepath.Join(cfg.DataDir, name)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := os.Rename(path, path+".closed"); err != nil {
			t.Fatalf("resource %s remained open after run error: %v", name, err)
		}
	}
}

func TestAgentDeleteWiringUsesLoadedConfigStoreAndDedicatedLogger(t *testing.T) {
	cfg := config.DefaultAgent()
	cfg.MachineID = "machine-wired"
	cfg.Delete.MaxEntriesPerFrame = 1
	cfg.Delete.DialTimeoutMS = 73
	cfg.Delete.HelloTimeoutS = 2
	cfg.Delete.ReportTimeoutS = 3

	helperErr := make(chan error, 1)
	deadlines := &readDeadlineRecorder{}
	dialer := helperDialerFunc(func(context.Context) (net.Conn, error) {
		agentSide, helperSide := net.Pipe()
		go func() {
			framed := proto.NewConn(helperSide)
			defer framed.Close()
			if err := framed.WriteFrame(proto.MsgHello, &proto.Hello{
				Version: proto.ProtocolVersion,
				Role:    "delete-helper",
				PID:     71,
			}); err != nil {
				helperErr <- err
				return
			}
			for sequence, path := range []string{`D:\wired-one`, `D:\wired-two`} {
				msgType, body, err := framed.ReadFrame()
				if err != nil {
					helperErr <- err
					return
				}
				message, err := proto.Decode(msgType, body)
				if err != nil {
					helperErr <- err
					return
				}
				task, ok := message.(*proto.DeleteTask)
				if !ok ||
					task.TaskID != "delete-wired" ||
					task.Seq != uint32(sequence) ||
					task.LastSeq != 1 ||
					!reflect.DeepEqual(task.Entries, []string{path}) {
					helperErr <- fmt.Errorf("helper task[%d] = %#v", sequence, message)
					return
				}
				if err := framed.WriteFrame(
					proto.MsgDeleteReport,
					&proto.DeleteReport{
						TaskID:  task.TaskID,
						Seq:     task.Seq,
						LastSeq: task.LastSeq,
						Stats:   proto.DeleteStats{Total: 1, OK: 1},
						Entries: []proto.DeleteResult{{Path: path, OK: true}},
					},
				); err != nil {
					helperErr <- err
					return
				}
			}
			helperErr <- nil
		}()
		return &recordingReadDeadlineConn{
			Conn:     agentSide,
			recorder: deadlines,
		}, nil
	})
	state := &recordingDeleteState{}
	var auditOutput, generalOutput bytes.Buffer
	audit := slog.New(slog.NewJSONHandler(&auditOutput, nil))
	general := slog.New(slog.NewJSONHandler(&generalOutput, nil))
	handler := buildDeleteForwarder(cfg, dialer, state, audit, general)

	var reports []proto.DeleteReport
	err := handler.Handle(
		context.Background(),
		proto.DeleteTask{
			TaskID:    "delete-wired",
			Mode:      proto.ModeHard,
			Confirmed: true,
			Entries:   []string{`D:\wired-one`, `D:\wired-two`},
		},
		func(msgType uint8, value any) error {
			if msgType != proto.MsgDeleteReport {
				return fmt.Errorf("message type = %d", msgType)
			}
			report, ok := value.(*proto.DeleteReport)
			if !ok {
				return fmt.Errorf("report type = %T", value)
			}
			reports = append(reports, *report)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-helperErr; err != nil {
		t.Fatal(err)
	}
	if len(reports) != 2 {
		t.Fatalf("GUI reports = %d, want 2 configured chunks", len(reports))
	}
	if !reflect.DeepEqual(state.machineIDs, []string{"machine-wired", "machine-wired"}) ||
		!reflect.DeepEqual(state.paths, [][]string{
			{`D:\wired-one`},
			{`D:\wired-two`},
		}) {
		t.Fatalf("state calls machineIDs=%v paths=%v", state.machineIDs, state.paths)
	}
	if !strings.Contains(auditOutput.String(), "delete_physical_result") ||
		!strings.Contains(auditOutput.String(), `D:\\wired-one`) {
		t.Fatalf("dedicated delete audit output = %q", auditOutput.String())
	}
	if generalOutput.Len() != 0 {
		t.Fatalf("general logger received delete audit: %q", generalOutput.String())
	}
	gotDeadlines := deadlines.snapshot()
	if len(gotDeadlines) != 3 {
		t.Fatalf("read deadlines = %v, want Hello plus two reports", gotDeadlines)
	}
	if gotDeadlines[0] <= 1500*time.Millisecond ||
		gotDeadlines[0] > 2*time.Second {
		t.Fatalf("Hello deadline = %v, want configured 2s", gotDeadlines[0])
	}
	for index, deadline := range gotDeadlines[1:] {
		if deadline <= 2500*time.Millisecond || deadline > 3*time.Second {
			t.Fatalf("report deadline[%d] = %v, want configured 3s",
				index, deadline)
		}
	}
}

func TestAgentDeleteWiringMapsDialTimeoutFromLoadedConfig(t *testing.T) {
	cfg := config.DefaultAgent()
	cfg.MachineID = "machine-timeout"
	cfg.Delete.DialTimeoutMS = 37
	dialer := helperDialerFunc(func(ctx context.Context) (net.Conn, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			return nil, errors.New("dial context has no deadline")
		}
		remaining := time.Until(deadline)
		if remaining <= 0 || remaining > 37*time.Millisecond {
			return nil, fmt.Errorf("dial deadline remaining = %v", remaining)
		}
		return nil, errors.New("expected dial failure")
	})
	handler := buildDeleteForwarder(
		cfg,
		dialer,
		&recordingDeleteState{},
		slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
		slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
	)
	var reports int
	err := handler.Handle(
		context.Background(),
		proto.DeleteTask{
			TaskID: "delete-timeout", Entries: []string{`D:\timeout`},
		},
		func(msgType uint8, value any) error {
			if msgType != proto.MsgDeleteReport {
				return fmt.Errorf("message type = %d", msgType)
			}
			reports++
			return nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), "expected dial failure") {
		t.Fatalf("Handle error = %v", err)
	}
	if reports != 1 {
		t.Fatalf("synthetic reports = %d, want 1", reports)
	}
}

func TestAgentDeleteWiringClosesDedicatedLoggerOnLaterStartupError(t *testing.T) {
	root := t.TempDir()
	cfg := config.DefaultAgent()
	cfg.MachineID = "machine-delete-cleanup"
	cfg.PGDSN = "://invalid-postgres-dsn"
	cfg.DataDir = filepath.Join(root, "data")
	cfg.UseEverything = false
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "agent.json")
	if err := os.WriteFile(configPath, body, 0o600); err != nil {
		t.Fatal(err)
	}

	var acquisitions, closes int
	openDeleteLogger := func(
		string,
	) (*slog.Logger, func() error, error) {
		acquisitions++
		return slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), func() error {
			closes++
			return nil
		}, nil
	}
	if err := runWithDependencies(configPath, openDeleteLogger, fixedMachineIdentity("c")); err == nil {
		t.Fatal("run unexpectedly accepted invalid PostgreSQL DSN")
	}
	if acquisitions != 1 || closes != 1 {
		t.Fatalf("delete logger lifecycle acquisitions=%d closes=%d, want 1/1",
			acquisitions, closes)
	}
}

type lifecyclePool struct {
	starts int
	closes int
}

type countingLifecyclePool struct {
	starts atomic.Int32
	closes atomic.Int32
}

func (p *countingLifecyclePool) Start() { p.starts.Add(1) }
func (p *countingLifecyclePool) Close() { p.closes.Add(1) }

type disabledInfoHandler struct{}

func (disabledInfoHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (disabledInfoHandler) Handle(context.Context, slog.Record) error { return nil }
func (disabledInfoHandler) WithAttrs([]slog.Attr) slog.Handler        { return disabledInfoHandler{} }
func (disabledInfoHandler) WithGroup(string) slog.Handler             { return disabledInfoHandler{} }

func (p *lifecyclePool) Start() { p.starts++ }
func (p *lifecyclePool) Close() { p.closes++ }

type orderedLifecyclePool struct {
	events *[]string
	closed bool
}

type deadlineShutdown struct {
	hasDeadline bool
	remaining   time.Duration
}

func (s *deadlineShutdown) Shutdown(ctx context.Context) error {
	deadline, ok := ctx.Deadline()
	s.hasDeadline = ok
	if ok {
		s.remaining = time.Until(deadline)
	}
	return nil
}

func (p *orderedLifecyclePool) Start() {
	*p.events = append(*p.events, "start")
}

func (p *orderedLifecyclePool) Close() {
	p.closed = true
	*p.events = append(*p.events, "close")
}

type helperDialerFunc func(context.Context) (net.Conn, error)

func (fn helperDialerFunc) Dial(ctx context.Context) (net.Conn, error) {
	return fn(ctx)
}

var _ agentdelete.HelperDialer = helperDialerFunc(nil)

type recordingDeleteState struct {
	mu         sync.Mutex
	machineIDs []string
	paths      [][]string
}

func (state *recordingDeleteState) MarkDeleted(
	_ context.Context,
	machineID string,
	paths []string,
) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.machineIDs = append(state.machineIDs, machineID)
	state.paths = append(state.paths, append([]string(nil), paths...))
	return nil
}

var _ agentdelete.StateStore = (*recordingDeleteState)(nil)

type recordingReadDeadlineConn struct {
	net.Conn
	recorder *readDeadlineRecorder
}

func (connection *recordingReadDeadlineConn) SetReadDeadline(
	deadline time.Time,
) error {
	connection.recorder.record(time.Until(deadline))
	return connection.Conn.SetReadDeadline(deadline)
}

type readDeadlineRecorder struct {
	mu        sync.Mutex
	durations []time.Duration
}

func (recorder *readDeadlineRecorder) record(duration time.Duration) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.durations = append(recorder.durations, duration)
}

func (recorder *readDeadlineRecorder) snapshot() []time.Duration {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]time.Duration(nil), recorder.durations...)
}

var _ agent.DeleteHandler = deleteHandlerCompileCheck(nil)

type deleteHandlerCompileCheck func(
	context.Context,
	proto.DeleteTask,
	agent.Sender,
) error

func (fn deleteHandlerCompileCheck) Handle(
	ctx context.Context,
	task proto.DeleteTask,
	sender agent.Sender,
) error {
	return fn(ctx, task, sender)
}
