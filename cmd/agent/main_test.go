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
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"dedup/internal/agent"
	agentdelete "dedup/internal/agent/delete"
	"dedup/internal/agentinstance"
	"dedup/internal/config"
	fileenum "dedup/internal/enum"
	"dedup/internal/machineid"
	"dedup/internal/proto"
	"dedup/internal/worker"
)

// Break caught: production's aggregate local handler forwards only task and
// analysis prefixes, leaving groups/review/preview permanently unsupported.
func TestAgentLocalHandlerForwardsResultOperations(t *testing.T) {
	results := &recordingLocalHandler{}
	handler := newAgentLocalHandler(agentLocalHandlerInputs{Results: results})
	for _, operation := range []string{
		proto.LocalOperationGroupsList, proto.LocalOperationGroupsDetail,
		proto.LocalOperationReviewSave, proto.LocalOperationPreviewImage,
	} {
		response := handler.HandleLocal(context.Background(), proto.LocalRequest{RequestID: operation, Operation: operation})
		if !response.OK || results.operations[len(results.operations)-1] != operation {
			t.Fatalf("operation %q response=%#v forwarded=%v", operation, response, results.operations)
		}
	}
}

func TestAgentLocalHandlerForwardsDeleteOperations(t *testing.T) {
	deletes := &recordingLocalHandler{}
	handler := newAgentLocalHandler(agentLocalHandlerInputs{Deletes: deletes})
	for _, operation := range []string{
		proto.LocalOperationDeletePrepare, proto.LocalOperationDeleteExecute, proto.LocalOperationDeleteStatus,
	} {
		response := handler.HandleLocal(context.Background(), proto.LocalRequest{RequestID: operation, Operation: operation})
		if !response.OK || deletes.operations[len(deletes.operations)-1] != operation {
			t.Fatalf("operation %q response=%#v forwarded=%v", operation, response, deletes.operations)
		}
	}
}

type recordingLocalHandler struct{ operations []string }

func (handler *recordingLocalHandler) HandleLocal(_ context.Context, request proto.LocalRequest) proto.LocalResponse {
	handler.operations = append(handler.operations, request.Operation)
	return proto.LocalResponse{RequestID: request.RequestID, OK: true}
}

func TestNewAgentEnumeratorDisabledUsesWalker(t *testing.T) {
	primary := &agentAvailabilityProbe{
		called: make(chan struct{}),
		err:    fileenum.ErrIndexNotReady,
	}
	enumr := newAgentEnumerator(context.Background(), agentEnumeratorOptions{
		Enabled: false,
		Primary: primary,
	})
	if enumr.Name() != "walker" {
		t.Fatalf("enumerator name = %q, want walker", enumr.Name())
	}
	select {
	case <-primary.called:
		t.Fatal("disabled Everything configuration started a readiness probe")
	default:
	}
}

// This fails if production can construct more than one browser or forgets to
// inject the constructed browser into the Agent server.
func TestSetAgentFilesystemBrowserConstructsAndInjectsOnce(t *testing.T) {
	setter := &recordingFilesystemBrowserSetter{}
	browser := &recordingInjectedFilesystemBrowser{}
	constructCalls := 0
	setAgentFilesystemBrowser(setter, func() agent.FilesystemBrowser {
		constructCalls++
		return browser
	})
	if constructCalls != 1 || setter.calls != 1 {
		t.Fatalf("browser construction/injection calls=%d/%d", constructCalls, setter.calls)
	}
	response := setter.browser.Browse(context.Background(), proto.FilesystemBrowseRequest{RequestID: "browse-wiring"})
	if browser.calls != 1 || response.RequestID != "browse-wiring" || response.ErrorCode != "injected_browser" {
		t.Fatalf("injected browser calls=%d response=%#v", browser.calls, response)
	}
}

type recordingInjectedFilesystemBrowser struct{ calls int }

func (browser *recordingInjectedFilesystemBrowser) Browse(
	_ context.Context,
	request proto.FilesystemBrowseRequest,
) proto.FilesystemBrowseResponse {
	browser.calls++
	return proto.FilesystemBrowseResponse{
		RequestID: request.RequestID,
		ErrorCode: "injected_browser",
	}
}

type recordingFilesystemBrowserSetter struct {
	calls   int
	browser agent.FilesystemBrowser
}

func (setter *recordingFilesystemBrowserSetter) SetFilesystemBrowser(browser agent.FilesystemBrowser) {
	setter.calls++
	setter.browser = browser
}

func TestNewAgentEnumeratorEnabledDoesNotBlockStartup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	primary := &agentAvailabilityProbe{
		called: make(chan struct{}),
		err:    fileenum.ErrIndexNotReady,
	}
	startCalls := 0
	returned := make(chan fileenum.Enumerator, 1)
	go func() {
		returned <- newAgentEnumerator(ctx, agentEnumeratorOptions{
			Enabled:  true,
			Primary:  primary,
			Fallback: fileenum.WalkerEnumerator{},
			StartClient: func() error {
				startCalls++
				return nil
			},
			Poll: func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			},
			Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		})
	}()

	select {
	case enumr := <-returned:
		if enumr.Name() == "walker" {
			t.Fatalf("enabled enumerator name = %q, want Everything wrapper", enumr.Name())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("newAgentEnumerator blocked Agent startup while index was not ready")
	}
	select {
	case <-primary.called:
	case <-time.After(2 * time.Second):
		t.Fatal("Everything readiness probe did not start in background")
	}
	if startCalls != 0 {
		t.Fatalf("StartClient calls = %d, want 0 for a running client", startCalls)
	}
}

type agentAvailabilityProbe struct {
	called chan struct{}
	once   sync.Once
	err    error
}

func (e *agentAvailabilityProbe) Name() string { return "everything" }
func (e *agentAvailabilityProbe) Available() error {
	e.once.Do(func() { close(e.called) })
	return e.err
}
func (e *agentAvailabilityProbe) Enum(
	_ string,
	_ func(fileenum.FileRecord) error,
) error {
	return nil
}

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

func TestRunServiceDrainsPhase2ThenSchedulerBeforeClosingPool(t *testing.T) {
	var events []string
	pool := &orderedLifecyclePool{events: &events}
	err := runService(
		pool,
		func() error {
			events = append(events, "serve")
			return nil
		},
		func() error {
			events = append(events, "phase2-drain")
			if pool.closed {
				t.Fatal("pool closed before Phase2 drain")
			}
			return nil
		},
		func() error {
			events = append(events, "scheduler-shutdown")
			if pool.closed {
				t.Fatal("pool closed before scheduler shutdown")
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"start", "serve", "phase2-drain", "scheduler-shutdown", "close"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("lifecycle events=%v, want %v", events, want)
	}
}

func TestLocalTaskRunnerRecoverySkipsScanAndCompletesStageThree(t *testing.T) {
	scans := &recordingLocalScanRunner{}
	analysis := &recordingLocalAnalysisRunner{}
	runner := &agentLocalTaskRunner{scans: scans, analysis: analysis}
	var stages []int
	request := proto.LocalTaskCreateRequest{TaskID: "recover", Roots: []string{`D:\media`}, Mode: proto.LocalTaskModeScanThenAnalysis}
	if err := runner.Run(context.Background(), request, 1, func(stage int) error { stages = append(stages, stage); return nil }); err != nil {
		t.Fatal(err)
	}
	if scans.calls != 0 || analysis.calls != 1 || !reflect.DeepEqual(stages, []int{2, 3}) {
		t.Fatalf("scan=%d analysis=%d stages=%v", scans.calls, analysis.calls, stages)
	}
	if analysis.taskID != "recover" || !reflect.DeepEqual(analysis.roots, []string{`D:\media`}) {
		t.Fatalf("analysis task=%q roots=%v", analysis.taskID, analysis.roots)
	}
	stages = nil
	if err := runner.Run(context.Background(), request, 2, func(stage int) error { stages = append(stages, stage); return nil }); err != nil {
		t.Fatal(err)
	}
	if scans.calls != 0 || analysis.calls != 2 || !reflect.DeepEqual(stages, []int{2, 3}) {
		t.Fatalf("stage2 recovery scan=%d analysis=%d stages=%v", scans.calls, analysis.calls, stages)
	}
	request.Roots[0] = `D:\changed`
	if !reflect.DeepEqual(analysis.roots, []string{`D:\media`}) {
		t.Fatalf("analysis roots aliased request=%v", analysis.roots)
	}
}

func TestLocalTaskRunnerScanOnlyEndsAtStageOne(t *testing.T) {
	scans := &recordingLocalScanRunner{}
	runner := &agentLocalTaskRunner{scans: scans, analysis: &recordingLocalAnalysisRunner{}}
	var stages []int
	request := proto.LocalTaskCreateRequest{TaskID: "scan", Roots: []string{`D:\media`}, Mode: proto.LocalTaskModeScanOnly}
	if err := runner.Run(context.Background(), request, 0, func(stage int) error { stages = append(stages, stage); return nil }); err != nil {
		t.Fatal(err)
	}
	if scans.calls != 1 || !reflect.DeepEqual(stages, []int{1}) {
		t.Fatalf("scan=%d stages=%v", scans.calls, stages)
	}
}

func TestLocalTaskRunnerAutoAnalysisPersistsEveryDurableCheckpoint(t *testing.T) {
	scans := &recordingLocalScanRunner{}
	analysis := &recordingLocalAnalysisRunner{}
	runner := &agentLocalTaskRunner{scans: scans, analysis: analysis}
	var stages []int
	request := proto.LocalTaskCreateRequest{TaskID: "auto", Roots: []string{`D:\media`}, Mode: proto.LocalTaskModeScanThenAnalysis}
	if err := runner.Run(context.Background(), request, 0, func(stage int) error { stages = append(stages, stage); return nil }); err != nil {
		t.Fatal(err)
	}
	if scans.calls != 1 || analysis.calls != 1 || !reflect.DeepEqual(stages, []int{1, 2, 3}) {
		t.Fatalf("scan=%d analysis=%d stages=%v", scans.calls, analysis.calls, stages)
	}
	if analysis.taskID != "auto" || !reflect.DeepEqual(analysis.roots, []string{`D:\media`}) {
		t.Fatalf("analysis task=%q roots=%v", analysis.taskID, analysis.roots)
	}
}

func TestPostgresParseFailureIsImmediateDegradedState(t *testing.T) {
	health := newSyncHealthState()
	started := time.Now()
	if pool := initializePostgres(context.Background(), "://invalid-postgres-dsn", health, nil); pool != nil {
		pool.Close()
		t.Fatal("invalid DSN returned pool")
	}
	if time.Since(started) > 100*time.Millisecond || health.snapshot().Healthy {
		t.Fatalf("parse failure blocked or reported healthy: elapsed=%v health=%#v", time.Since(started), health.snapshot())
	}
}

func TestEmptyPostgresConfigurationIsLocalOnlyAndNonBlocking(t *testing.T) {
	health := newSyncHealthState()
	if pool := initializePostgres(context.Background(), "", health, nil); pool != nil {
		t.Fatal("empty PostgreSQL configuration created a pool")
	}
	if snapshot := health.snapshot(); snapshot.Healthy || snapshot.ErrorSummary != "sync_not_configured" {
		t.Fatalf("health = %#v", snapshot)
	}
}

func TestPostgresConfigFailureDoesNotExposeDSN(t *testing.T) {
	secret := `postgres://private-user:private-password@secret-%zz-host/private-db?token=private-query`
	health := newSyncHealthState()
	var output bytes.Buffer
	pool := initializePostgres(context.Background(), secret, health, slog.New(slog.NewTextHandler(&output, nil)))
	if pool != nil {
		pool.Close()
		t.Fatal("invalid DSN returned pool")
	}
	combined := output.String() + health.snapshot().ErrorSummary
	for _, value := range []string{"private-user", "private-password", "secret-host", "private-db", "private-query", "token="} {
		if strings.Contains(combined, value) {
			t.Fatalf("diagnostics leaked %q: %q", value, combined)
		}
	}
	if health.snapshot().ErrorSummary != "postgres_config_invalid" {
		t.Fatalf("health=%#v", health.snapshot())
	}
}

func TestEverythingRootFallbackLogUsesOnlyPathIdentity(t *testing.T) {
	root := `D:\Private\Media`
	cause := errors.New(`Everything failed under D:/Private\Media token=secret`)
	var output bytes.Buffer
	logEverythingRootFallback(slog.New(slog.NewTextHandler(&output, nil)), root, cause)
	got := output.String()
	for _, value := range []string{"Private", "Media", "token=secret", "Everything failed"} {
		if strings.Contains(got, value) {
			t.Fatalf("log leaked %q: %q", value, got)
		}
	}
	if !strings.Contains(got, "path_id") || !strings.Contains(got, "everything_root_fallback") {
		t.Fatalf("safe fields missing: %q", got)
	}
}

func TestLocalTaskRecoveryRegistersBeforeListenerAndResumesAfterReady(t *testing.T) {
	lifecycle := &recordingLocalTaskLifecycle{prepared: make(chan struct{}), resumed: make(chan struct{})}
	ready, err := prepareLocalTaskLifecycle(context.Background(), lifecycle, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-lifecycle.prepared:
	default:
		t.Fatal("PrepareRecovery did not finish synchronously before listener setup")
	}
	select {
	case <-lifecycle.resumed:
		t.Fatal("Resume ran before listener-ready callback")
	default:
	}
	ready()
	select {
	case <-lifecycle.resumed:
	case <-time.After(time.Second):
		t.Fatal("Resume did not run asynchronously after listener ready")
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

func TestAgentControllerHandlerReturnsMigratedStatusAndUsesOnlyLocalShutdown(t *testing.T) {
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	machineID := "node-" + strings.Repeat("1", 64)
	started := time.Date(2026, 8, 12, 12, 0, 0, 123000000, time.UTC)
	provider := newAgentStatusProvider(agentStatusInputs{
		MachineID: machineID, ExecutablePath: `C:\portable\agent.exe`,
		ConfigSHA256: strings.Repeat("a", 64), StartedAt: started,
		ListenerReady: func() bool { return true },
		Workers: &agentSnapshotProvider{snapshot: worker.RuntimeSnapshot{
			Expected: 1, Ready: 1, Workers: []worker.RuntimeWorkerStatus{{Index: 0, PID: 501, Ready: true}},
		}},
		SyncHealth: func() agentSyncHealth { return agentSyncHealth{Healthy: true} },
	})
	handler := newAgentLocalHandler(agentLocalHandlerInputs{
		Status: provider.ControlStatus, Shutdown: cancel, ShutdownDelay: time.Millisecond,
	})

	statusResponse := handler.HandleLocal(context.Background(), proto.LocalRequest{
		RequestID: "status-1", Operation: proto.LocalOperationStatusGet,
	})
	if !statusResponse.OK || statusResponse.ErrorCode != "" {
		t.Fatalf("status response = %#v", statusResponse)
	}
	var statusPayload proto.LocalStatusGetResponse
	if err := proto.DecodeLocalPayload(statusResponse.Payload, &statusPayload); err != nil {
		t.Fatal(err)
	}
	if statusPayload.Status.MachineID != machineID || statusPayload.Status.PID != os.Getpid() ||
		statusPayload.Status.StartedAtUnixMS != started.UnixMilli() || !statusPayload.Status.Ready {
		t.Fatalf("status payload = %#v", statusPayload.Status)
	}

	shutdownResponse := handler.HandleLocal(context.Background(), proto.LocalRequest{
		RequestID: "shutdown-1", Operation: proto.LocalOperationShutdown,
	})
	if !shutdownResponse.OK {
		t.Fatalf("shutdown response = %#v", shutdownResponse)
	}
	var shutdown proto.LocalShutdownResponse
	if err := proto.DecodeLocalPayload(shutdownResponse.Payload, &shutdown); err != nil || !shutdown.Accepted {
		t.Fatalf("shutdown payload = %#v, %v", shutdown, err)
	}
	select {
	case <-root.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("local.shutdown did not cancel Agent root context")
	}
}

func TestAgentControllerHandlerGetsValidatesAndAtomicallySavesCanonicalConfig(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "agent.exe")
	configPath := filepath.Join(root, "data", "agent.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	running := validAgentLocalConfig(t, root, executable)
	runningJSON := mustAgentCanonicalJSON(t, running)
	if err := os.WriteFile(configPath, runningJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	runningSHA, err := effectiveConfigSHA256(running)
	if err != nil {
		t.Fatal(err)
	}
	handler := newAgentLocalHandler(agentLocalHandlerInputs{
		ConfigPath: configPath, ExecutablePath: executable, CPUCount: runtime.NumCPU(),
		EffectiveConfigSHA256: runningSHA,
	})

	get := handler.HandleLocal(context.Background(), proto.LocalRequest{RequestID: "get-1", Operation: proto.LocalOperationConfigGet})
	if !get.OK {
		t.Fatalf("config get = %#v", get)
	}
	var gotConfig proto.LocalConfigGetResponse
	if err := proto.DecodeLocalPayload(get.Payload, &gotConfig); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotConfig.CanonicalJSON, runningJSON) || gotConfig.SHA256 != runningSHA {
		t.Fatalf("config get digest=%q bytes_equal=%v", gotConfig.SHA256, bytes.Equal(gotConfig.CanonicalJSON, runningJSON))
	}

	updated := *running
	updated.ListenAddr = "0.0.0.0:9201"
	updatedJSON := mustAgentCanonicalJSON(t, &updated)
	requestPayload, err := proto.EncodeLocalPayload(proto.LocalConfigRequest{CanonicalJSON: updatedJSON})
	if err != nil {
		t.Fatal(err)
	}
	validate := handler.HandleLocal(context.Background(), proto.LocalRequest{
		RequestID: "validate-1", Operation: proto.LocalOperationConfigValidate, Payload: requestPayload,
	})
	if !validate.OK {
		t.Fatalf("config validate = %#v", validate)
	}
	if disk, err := os.ReadFile(configPath); err != nil || !bytes.Equal(disk, runningJSON) {
		t.Fatalf("validation changed disk config: equal=%v err=%v", bytes.Equal(disk, runningJSON), err)
	}
	var validated proto.LocalConfigValidateResponse
	if err := proto.DecodeLocalPayload(validate.Payload, &validated); err != nil || !validated.Valid || !validated.RestartRequired {
		t.Fatalf("validate payload = %#v, %v", validated, err)
	}

	save := handler.HandleLocal(context.Background(), proto.LocalRequest{
		RequestID: "save-1", Operation: proto.LocalOperationConfigSave, Payload: requestPayload,
	})
	if !save.OK {
		t.Fatalf("config save = %#v", save)
	}
	var saved proto.LocalConfigSaveResponse
	if err := proto.DecodeLocalPayload(save.Payload, &saved); err != nil || !saved.RestartRequired || saved.SHA256 != validated.SHA256 {
		t.Fatalf("save payload = %#v, %v", saved, err)
	}
	disk, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(disk, updatedJSON) || !strings.HasSuffix(string(disk), "\n") {
		t.Fatalf("saved config is not exact canonical JSON: %q", disk)
	}
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(configPath), ".agent.json.*.tmp"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("atomic save leftovers = %v, %v", leftovers, err)
	}
}

func TestAgentControllerHandlerRejectsInvalidConfigWithoutSecretLeakOrWrite(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "agent.exe")
	configPath := filepath.Join(root, "agent.json")
	running := validAgentLocalConfig(t, root, executable)
	original := mustAgentCanonicalJSON(t, running)
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	handler := newAgentLocalHandler(agentLocalHandlerInputs{
		ConfigPath: configPath, ExecutablePath: executable, CPUCount: runtime.NumCPU(),
	})
	secret := "postgres://private-user:private-password@db.invalid/dedup"
	payload, err := proto.EncodeLocalPayload(proto.LocalConfigRequest{CanonicalJSON: []byte(`{"pg_dsn":"` + secret + `","unknown":"token=private-token"}`)})
	if err != nil {
		t.Fatal(err)
	}
	response := handler.HandleLocal(context.Background(), proto.LocalRequest{
		RequestID: "invalid-1", Operation: proto.LocalOperationConfigSave, Payload: payload,
	})
	if response.OK || response.ErrorCode != "invalid_config" {
		t.Fatalf("invalid save response = %#v", response)
	}
	wire := response.RequestID + response.ErrorCode + string(response.Payload)
	for _, forbidden := range []string{secret, "private-password", "private-token", configPath} {
		if strings.Contains(wire, forbidden) {
			t.Fatalf("invalid config response leaked %q: %q", forbidden, wire)
		}
	}
	if disk, err := os.ReadFile(configPath); err != nil || !bytes.Equal(disk, original) {
		t.Fatalf("invalid save changed config: equal=%v err=%v", bytes.Equal(disk, original), err)
	}
}

func TestAgentControllerHandlerNormalizesCanonicalConfigForCurrentCPU(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "agent.exe")
	configPath := filepath.Join(root, "agent.json")
	running := validAgentLocalConfig(t, root, executable)
	if err := os.WriteFile(configPath, mustAgentCanonicalJSON(t, running), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate := *running
	candidate.Worker.Count = 0
	payload, err := proto.EncodeLocalPayload(proto.LocalConfigRequest{CanonicalJSON: mustAgentCanonicalJSON(t, &candidate)})
	if err != nil {
		t.Fatal(err)
	}
	handler := newAgentLocalHandler(agentLocalHandlerInputs{
		ConfigPath: configPath, ExecutablePath: executable, CPUCount: 3,
	})
	response := handler.HandleLocal(context.Background(), proto.LocalRequest{
		RequestID: "normalize-1", Operation: proto.LocalOperationConfigSave, Payload: payload,
	})
	if !response.OK {
		t.Fatalf("canonical config requiring normalization was rejected: %#v", response)
	}
	disk, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var saved config.AgentConfig
	if err := json.Unmarshal(disk, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Worker.Count != 3 {
		t.Fatalf("saved worker count = %d, want current CPU count 3", saved.Worker.Count)
	}
}

func TestAgentControllerStatusSanitizesWorkerPoolAndSyncDiagnostics(t *testing.T) {
	mediaPath := `D:\private media\secret.mp4`
	dsn := "postgres://admin:password@db.example/dedup"
	provider := newAgentStatusProvider(agentStatusInputs{
		MachineID: "node-" + strings.Repeat("2", 64), ExecutablePath: `C:\agent.exe`,
		ConfigSHA256: strings.Repeat("b", 64), StartedAt: time.Unix(10, 0),
		ListenerReady: func() bool { return true },
		Workers: &agentSnapshotProvider{snapshot: worker.RuntimeSnapshot{
			Expected: 1, Ready: 1, LastErrorSummary: "env=TOP_SECRET path=" + mediaPath,
			Workers: []worker.RuntimeWorkerStatus{{Index: 0, PID: 44, Ready: true, CurrentTaskSummary: "input=" + mediaPath, LastErrorSummary: "dsn=" + dsn + " password=hunter2"}},
		}},
		SyncHealth: func() agentSyncHealth {
			return agentSyncHealth{ErrorSummary: "sync dsn=" + dsn + " media=" + mediaPath}
		},
	})
	status := provider.ControlStatus()
	joined := status.SyncErrorSummary + status.LastErrorSummary + status.Workers[0].CurrentTaskSummary + status.Workers[0].LastErrorSummary
	for _, secret := range []string{"TOP_SECRET", mediaPath, "admin:password", "hunter2"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("status leaked %q in %q", secret, joined)
		}
	}
	if status.Lifecycle != "running" || !status.Ready || status.SyncHealthy {
		t.Fatalf("status readiness changed by sync health: %#v", status)
	}
	if err := status.Validate(); err != nil {
		t.Fatalf("invalid status: %v", err)
	}
}

func TestAgentControllerStatusRemainsStartingUntilEveryWorkerIsReady(t *testing.T) {
	provider := newAgentStatusProvider(agentStatusInputs{
		MachineID: "node-" + strings.Repeat("4", 64), ExecutablePath: `C:\agent.exe`,
		ConfigSHA256: strings.Repeat("c", 64), StartedAt: time.Unix(20, 0),
		ListenerReady: func() bool { return true },
		Workers: &agentSnapshotProvider{snapshot: worker.RuntimeSnapshot{
			Expected: 2, Ready: 1, Workers: []worker.RuntimeWorkerStatus{
				{Index: 0, PID: 5201, Ready: true},
				{Index: 1, LastErrorSummary: "worker unavailable; start or respawn pending"},
			},
		}},
		SyncHealth: func() agentSyncHealth { return agentSyncHealth{Healthy: true} },
	})
	status := provider.ControlStatus()
	if !status.ServiceReady || status.Ready || status.Lifecycle != "starting" ||
		status.WorkerExpected != 2 || status.WorkerReady != 1 || len(status.Workers) != 2 {
		t.Fatalf("starting status = %#v", status)
	}
	if err := status.Validate(); err != nil {
		t.Fatalf("invalid starting status: %v", err)
	}
}

func TestAgentControllerStatusSanitizesUNCMediaPaths(t *testing.T) {
	unc := `\\fictional-server\fictional-share\private clip.mp4`
	provider := newAgentStatusProvider(agentStatusInputs{
		MachineID: "node-" + strings.Repeat("5", 64), ExecutablePath: `C:\agent.exe`,
		ConfigSHA256: strings.Repeat("d", 64), StartedAt: time.Unix(30, 0),
		ListenerReady: func() bool { return true },
		Workers: &agentSnapshotProvider{snapshot: worker.RuntimeSnapshot{
			Expected: 1, Ready: 1, LastErrorSummary: "pool path=" + unc,
			Workers: []worker.RuntimeWorkerStatus{{
				Index: 0, PID: 5401, Ready: true,
				CurrentTaskSummary: "input=" + unc, LastErrorSummary: "worker path=" + unc,
			}},
		}},
		SyncHealth: func() agentSyncHealth { return agentSyncHealth{ErrorSummary: "sync path=" + unc} },
	})
	status := provider.ControlStatus()
	joined := status.LastErrorSummary + status.SyncErrorSummary + status.Workers[0].CurrentTaskSummary + status.Workers[0].LastErrorSummary
	if strings.Contains(joined, "fictional-server") || strings.Contains(joined, "private clip.mp4") {
		t.Fatalf("status leaked UNC media path: %q", joined)
	}
	if err := status.Validate(); err != nil {
		t.Fatalf("invalid UNC-redacted status: %v", err)
	}
}

func TestSingleInstanceRejectsSecondAgentAndReleasesMutex(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows named mutex contract")
	}
	machineID := "node-" + strings.Repeat("3", 48) + time.Now().UTC().Format("1504050000000000")
	first, err := agentinstance.AcquireSingleInstance(machineID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := agentinstance.AcquireSingleInstance(machineID)
	if second != nil {
		_ = second.Close()
	}
	if !errors.Is(err, agentinstance.ErrAlreadyRunning) {
		_ = first.Close()
		t.Fatalf("second acquisition error = %v, want ErrAlreadyRunning", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	reacquired, err := agentinstance.AcquireSingleInstance(machineID)
	if err != nil {
		t.Fatalf("mutex was not released: %v", err)
	}
	if err := reacquired.Close(); err != nil {
		t.Fatal(err)
	}
}

type agentSnapshotProvider struct{ snapshot worker.RuntimeSnapshot }

func (p *agentSnapshotProvider) RuntimeSnapshot() worker.RuntimeSnapshot {
	copy := p.snapshot
	copy.Workers = append([]worker.RuntimeWorkerStatus(nil), p.snapshot.Workers...)
	return copy
}

func validAgentLocalConfig(t *testing.T, root, executable string) *config.AgentConfig {
	t.Helper()
	cfg := config.DefaultAgent()
	cfg.ListenAddr = "0.0.0.0:9101"
	cfg.DataDir = filepath.Join(root, "runtime")
	cfg.PGDSN = "postgres://agent-user:stored-secret@127.0.0.1:5432/dedup?sslmode=prefer"
	validated, err := config.ValidateAgent(cfg, executable, runtime.NumCPU())
	if err != nil {
		t.Fatal(err)
	}
	return validated
}

func mustAgentCanonicalJSON(t *testing.T, cfg *config.AgentConfig) []byte {
	t.Helper()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
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
	blockedListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blockedListener.Close()
	cfg := config.DefaultAgent()
	cfg.PGDSN = "://invalid-postgres-dsn"
	cfg.ListenAddr = blockedListener.Addr().String()
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
		t.Fatal("run unexpectedly accepted invalid listen address")
	}
	identity, err := resolveIdentity()
	if err != nil {
		t.Fatal(err)
	}
	lock, err := agentinstance.AcquireSingleInstance(identity.ID)
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
	blockedListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blockedListener.Close()
	cfg := config.DefaultAgent()
	cfg.MachineID = "machine-cleanup"
	cfg.PGDSN = "://invalid-postgres-dsn"
	cfg.ListenAddr = blockedListener.Addr().String()
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
		t.Fatal("run unexpectedly accepted invalid listen address")
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
	blockedListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blockedListener.Close()
	cfg := config.DefaultAgent()
	cfg.MachineID = "machine-delete-cleanup"
	cfg.PGDSN = "://invalid-postgres-dsn"
	cfg.ListenAddr = blockedListener.Addr().String()
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
		t.Fatal("run unexpectedly accepted invalid listen address")
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

type recordingLocalScanRunner struct{ calls int }

func (r *recordingLocalScanRunner) Prepare(task proto.ScanTask, sender agent.Sender) (proto.TaskAck, func()) {
	r.calls++
	return proto.TaskAck{TaskID: task.TaskID, Accepted: true}, func() {
		_ = sender(proto.MsgTaskDone, &proto.TaskDone{TaskID: task.TaskID})
	}
}

type recordingLocalAnalysisRunner struct {
	calls  int
	taskID string
	roots  []string
}

func (r *recordingLocalAnalysisRunner) Run(context.Context, string) error {
	r.calls++
	return nil
}
func (r *recordingLocalAnalysisRunner) RunWithProgressForRoots(ctx context.Context, task string, roots []string, checkpoint func(int) error) error {
	r.calls++
	r.taskID = task
	r.roots = roots
	return checkpoint(2)
}

type recordingLocalTaskLifecycle struct {
	prepared chan struct{}
	resumed  chan struct{}
}

func (r *recordingLocalTaskLifecycle) PrepareRecovery(context.Context) error {
	close(r.prepared)
	return nil
}
func (r *recordingLocalTaskLifecycle) Resume(context.Context) error {
	close(r.resumed)
	return nil
}

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
