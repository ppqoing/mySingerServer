package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"dedup/internal/helper"
	"dedup/internal/machineid"
	"dedup/internal/nodectl"
	"golang.org/x/sys/windows"
)

// Each test names the production break it catches: changing the supplied
// config path, acquiring privileged resources before config validation, or
// unwinding resources in acquisition order would be a startup safety bug.

func nodeTrayCanonicalHelperConfigSHA256(t *testing.T, cfg helper.Config) string {
	t.Helper()
	canonical, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal NodeTray canonical Helper config: %v", err)
	}
	canonical = append(canonical, '\n')
	return fmt.Sprintf("%x", sha256.Sum256(canonical))
}

func TestEffectiveHelperConfigSHA256MatchesNodeTrayCanonicalJSON(t *testing.T) {
	cfg := helper.Config{
		PipeName:             `\\.\pipe\dedup-delete`,
		AllowedRoots:         []string{`D:\media`, `E:\archive`},
		DeniedRoots:          []string{`D:\media\private`},
		DefaultMode:          "soft",
		AllowHardDelete:      false,
		RecycleDirName:       "$DedupRecycle",
		MaxEntriesPerFrame:   2000,
		FrameReadTimeoutSec:  120,
		FrameWriteTimeoutSec: 60,
		LogDir:               `C:\ProgramData\MySingerServer\Helper\logs`,
	}

	want := nodeTrayCanonicalHelperConfigSHA256(t, cfg)
	got, err := effectiveHelperConfigSHA256(cfg)
	if err != nil {
		t.Fatalf("effectiveHelperConfigSHA256() error = %v", err)
	}
	if got != want {
		t.Fatalf("effectiveHelperConfigSHA256() = %q, want NodeTray canonical digest %q", got, want)
	}
}

func TestConfigPathFromArgsUsesHelperBesideExecutableByDefault(t *testing.T) {
	got, err := configPathFromArgs(nil, `C:\Program Files\Dedup\helper.exe`)
	if err != nil {
		t.Fatal(err)
	}
	if want := `C:\Program Files\Dedup\helper.json`; got != want {
		t.Fatalf("config path = %q, want %q", got, want)
	}
}

func TestConfigPathFromArgsUsesExplicitConfigUnchanged(t *testing.T) {
	got, err := configPathFromArgs([]string{"-config", `D:\operator\helper.json`}, `C:\Program Files\Dedup\helper.exe`)
	if err != nil {
		t.Fatal(err)
	}
	if want := `D:\operator\helper.json`; got != want {
		t.Fatalf("config path = %q, want %q", got, want)
	}
}

func TestRunWithRefusesInvalidConfigBeforeMutexOrPipe(t *testing.T) {
	sentinel := errors.New("invalid config")
	events := make([]string, 0, 3)
	deps := testDependencies(&events)
	deps.loadConfig = func(string, string) (helper.Config, error) {
		events = append(events, "load")
		return helper.Config{}, sentinel
	}

	err := runWith(context.Background(), "bad.json", "helper.exe", deps)
	if !errors.Is(err, sentinel) {
		t.Fatalf("run error = %v, want %v", err, sentinel)
	}
	if want := []string{"load"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestRunWithIdentityFailureBeforeLoggerMutexOrPipes(t *testing.T) {
	sentinel := errors.New("hardware sources unavailable")
	events := make([]string, 0, 2)
	deps := testDependencies(&events)
	deps.identity = func() (machineid.Result, error) {
		return machineid.Result{}, sentinel
	}

	err := runWith(context.Background(), "helper.json", "helper.exe", deps)
	if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "resolve Helper machine identity") {
		t.Fatalf("run error = %v", err)
	}
	if want := []string{"load"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestRunWithRefusesSecondInstanceAndClosesLogger(t *testing.T) {
	sentinel := errors.New("already running")
	events := make([]string, 0, 5)
	deps := testDependencies(&events)
	deps.acquireLock = func(string) (helper.InstanceLock, error) {
		events = append(events, "lock")
		return nil, sentinel
	}

	err := runWith(context.Background(), "helper.json", "helper.exe", deps)
	if !errors.Is(err, sentinel) {
		t.Fatalf("run error = %v, want %v", err, sentinel)
	}
	if want := []string{"load", "logger", "lock", "logger.close"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestRunWithClosesListenerThenMutexThenLoggerAndKeepsAllErrors(t *testing.T) {
	serveErr := errors.New("serve failed")
	listenerCloseErr := errors.New("listener close failed")
	lockCloseErr := errors.New("lock close failed")
	loggerCloseErr := errors.New("logger close failed")
	events := make([]string, 0, 12)
	deps := testDependencies(&events)
	deps.listenPipe = func(helper.Config) (net.Listener, error) {
		events = append(events, "listener")
		return &recordingListener{events: &events, closeErr: listenerCloseErr}, nil
	}
	deps.acquireLock = func(string) (helper.InstanceLock, error) {
		events = append(events, "lock")
		return &recordingLock{events: &events, closeErr: lockCloseErr}, nil
	}
	deps.newLogger = func(string) (*slog.Logger, func() error, error) {
		events = append(events, "logger")
		return slog.New(slog.NewTextHandler(io.Discard, nil)), func() error {
			events = append(events, "logger.close")
			return loggerCloseErr
		}, nil
	}
	deps.newValidator = func(helper.Config) (*helper.Validator, error) {
		events = append(events, "validator")
		return nil, nil
	}
	deps.newProcessor = func(helper.Config, *helper.Validator) *helper.Processor {
		events = append(events, "processor")
		return nil
	}
	deps.newServer = func(helper.Config, net.Listener, *helper.Processor, *slog.Logger) helperServer {
		events = append(events, "server")
		return serverFunc(func(context.Context) error {
			events = append(events, "serve")
			return serveErr
		})
	}

	err := runWith(context.Background(), "helper.json", "helper.exe", deps)
	for _, wantErr := range []error{serveErr, listenerCloseErr, lockCloseErr, loggerCloseErr} {
		if !errors.Is(err, wantErr) {
			t.Fatalf("run error = %v, missing %v", err, wantErr)
		}
	}
	want := []string{"load", "logger", "lock", "listener", "validator", "processor", "server", "serve", "listener.close", "lock.close", "logger.close"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestRunWithSharesOneListenerCloseAndRetainsFirstCloseError(t *testing.T) {
	listenerCloseErr := errors.New("listener close failed")
	events := make([]string, 0, 12)
	rawListener := &recordingListener{events: &events, closeErr: listenerCloseErr}
	deps := testDependencies(&events)
	deps.listenPipe = func(helper.Config) (net.Listener, error) {
		events = append(events, "listener")
		return rawListener, nil
	}
	deps.newServer = func(_ helper.Config, listener net.Listener, _ *helper.Processor, _ *slog.Logger) helperServer {
		events = append(events, "server")
		return serverFunc(func(context.Context) error {
			events = append(events, "serve")
			_ = listener.Close() // Task4 Server.Serve owns a shutdown close.
			return nil
		})
	}

	err := runWith(context.Background(), "helper.json", "helper.exe", deps)
	if !errors.Is(err, listenerCloseErr) {
		t.Fatalf("run error = %v, want first listener close error", err)
	}
	if rawListener.closeCalls != 1 {
		t.Fatalf("underlying listener close calls = %d, want 1", rawListener.closeCalls)
	}
	want := []string{"load", "logger", "lock", "listener", "validator", "processor", "server", "serve", "listener.close", "lock.close", "logger.close"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestRunWithPassesCancellationToServer(t *testing.T) {
	events := make([]string, 0, 9)
	deps := testDependencies(&events)
	serverSawCancellation := make(chan struct{})
	deps.newServer = func(helper.Config, net.Listener, *helper.Processor, *slog.Logger) helperServer {
		return serverFunc(func(ctx context.Context) error {
			<-ctx.Done()
			close(serverSawCancellation)
			return nil
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runWith(ctx, "helper.json", "helper.exe", deps) }()
	cancel()
	select {
	case <-serverSawCancellation:
	case <-time.After(time.Second):
		t.Fatal("server did not receive shutdown cancellation")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRunWithControlShutdownCancelsDeleteServerAndPublishesIdentity(t *testing.T) {
	events := make([]string, 0, 12)
	deps := testDependencies(&events)
	started := time.Date(2026, 8, 2, 10, 0, 0, 789000000, time.UTC)
	wantMachineID := "node-" + strings.Repeat("b", 64)
	deps.identity = func() (machineid.Result, error) {
		return machineid.Result{
			ID:           wantMachineID,
			CPUAvailable: true,
			Warnings:     []string{"board source unavailable"},
		}, nil
	}
	deps.now = func() time.Time { return started }
	deleteCanceled := make(chan struct{})
	deps.newServer = func(helper.Config, net.Listener, *helper.Processor, *slog.Logger) helperServer {
		return serverFunc(func(ctx context.Context) error {
			<-ctx.Done()
			close(deleteCanceled)
			return nil
		})
	}
	var status nodectl.Status
	deps.newControl = func(provider nodectl.StatusProvider, shutdown nodectl.ShutdownFunc) helperControlService {
		status = provider.ControlStatus()
		return controlFunc(func(ctx context.Context) error {
			shutdown()
			<-ctx.Done()
			return ctx.Err()
		})
	}

	if err := runWith(context.Background(), "helper.json", `C:\Program Files\MySingerServer\helper.exe`, deps); err != nil {
		t.Fatalf("controlled shutdown error = %v", err)
	}
	select {
	case <-deleteCanceled:
	case <-time.After(time.Second):
		t.Fatal("control shutdown did not cancel delete server")
	}
	wantDigest := nodeTrayCanonicalHelperConfigSHA256(t, helper.Config{LogDir: "logs"})
	if status.Component != nodectl.ComponentHelper || status.MachineID != wantMachineID ||
		status.ExecutablePath != `C:\Program Files\MySingerServer\helper.exe` ||
		status.StartedAtUnixMS != started.UnixMilli() || status.ConfigSHA256 != wantDigest {
		t.Fatalf("Helper control identity = %#v", status)
	}
}

func TestRunWithControlFailureCancelsDeleteServerAndReturnsFailure(t *testing.T) {
	sentinel := errors.New("control listener failed")
	events := make([]string, 0, 12)
	deps := testDependencies(&events)
	deleteCanceled := make(chan struct{})
	deps.newServer = func(helper.Config, net.Listener, *helper.Processor, *slog.Logger) helperServer {
		return serverFunc(func(ctx context.Context) error {
			<-ctx.Done()
			close(deleteCanceled)
			return nil
		})
	}
	deps.newControl = func(nodectl.StatusProvider, nodectl.ShutdownFunc) helperControlService {
		return controlFunc(func(context.Context) error { return sentinel })
	}

	err := runWith(context.Background(), "helper.json", `C:\helper.exe`, deps)
	if !errors.Is(err, sentinel) {
		t.Fatalf("run error = %v, want %v", err, sentinel)
	}
	select {
	case <-deleteCanceled:
	case <-time.After(time.Second):
		t.Fatal("control failure did not cancel delete server")
	}
}

func TestRunWithRejectsInvalidControlIdentityBeforePrivilegedResources(t *testing.T) {
	events := make([]string, 0, 3)
	deps := testDependencies(&events)
	deps.identity = func() (machineid.Result, error) {
		return machineid.Result{ID: strings.Repeat("界", 129)}, nil
	}

	err := runWith(context.Background(), "helper.json", `C:\helper.exe`, deps)
	if err == nil || !strings.Contains(err.Error(), "control identity") {
		t.Fatalf("invalid control identity error = %v", err)
	}
	if want := []string{"load"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestHelperSignalContextCancelsServeOnCtrlBreak(t *testing.T) {
	root := repoRoot(t)
	base := filepath.Join(root, ".superpowers", "tmp")
	work, err := os.MkdirTemp(base, "m5-ctrl-break-")
	if err != nil {
		t.Fatal(err)
	}
	defer removeRunUniqueTemp(t, base, work)
	ready := filepath.Join(work, "ready")
	done := filepath.Join(work, "serve-canceled")
	child := exec.Command(os.Args[0], "-test.run=^TestHelperSignalContextChild$", "-test.v")
	child.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	child.Env = append(os.Environ(), "M5_HELPER_SIGNAL_READY="+ready, "M5_HELPER_SIGNAL_DONE="+done)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	childDone := make(chan error, 1)
	go func() { childDone <- child.Wait() }()
	if err := waitForFile(ready, 5*time.Second); err != nil {
		_ = child.Process.Kill()
		<-childDone
		t.Fatal(err)
	}
	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(child.Process.Pid)); err != nil {
		_ = child.Process.Kill()
		<-childDone
		t.Fatalf("GenerateConsoleCtrlEvent CTRL_BREAK_EVENT: %v", err)
	}
	select {
	case err := <-childDone:
		if err != nil {
			t.Fatalf("Ctrl-Break child failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = child.Process.Kill()
		<-childDone
		t.Fatal("Ctrl-Break child did not exit after Serve cancellation")
	}
	if err := waitForFile(done, time.Second); err != nil {
		t.Fatalf("Serve did not observe signal-context cancellation: %v", err)
	}
}

func TestHelperSignalContextChild(t *testing.T) {
	ready := os.Getenv("M5_HELPER_SIGNAL_READY")
	if ready == "" {
		return
	}
	done := os.Getenv("M5_HELPER_SIGNAL_DONE")
	ctx, stop := helperSignalContext()
	defer stop()
	events := make([]string, 0, 8)
	deps := testDependencies(&events)
	deps.newServer = func(helper.Config, net.Listener, *helper.Processor, *slog.Logger) helperServer {
		return serverFunc(func(ctx context.Context) error {
			if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
				return err
			}
			<-ctx.Done()
			return os.WriteFile(done, []byte("canceled"), 0o600)
		})
	}
	if err := runWith(ctx, "helper.json", "helper.exe", deps); err != nil {
		t.Fatal(err)
	}
}

func readPEMachineAndSubsystem(path string) (uint16, uint16, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	if len(data) < 0x40 || binary.LittleEndian.Uint16(data[0:2]) != 0x5a4d {
		return 0, 0, errors.New("invalid PE DOS header")
	}
	peOffset := int(binary.LittleEndian.Uint32(data[0x3c:0x40]))
	const optionalHeaderDelta = 24
	const subsystemDelta = 68
	if peOffset < 0x40 || peOffset > len(data)-(optionalHeaderDelta+subsystemDelta+2) {
		return 0, 0, errors.New("invalid PE header offset")
	}
	if binary.LittleEndian.Uint32(data[peOffset:peOffset+4]) != 0x00004550 {
		return 0, 0, errors.New("invalid PE signature")
	}
	optionalHeader := peOffset + optionalHeaderDelta
	sizeOfOptionalHeader := int(binary.LittleEndian.Uint16(data[peOffset+20 : peOffset+22]))
	if sizeOfOptionalHeader < subsystemDelta+2 {
		return 0, 0, errors.New("PE Optional Header does not cover Subsystem")
	}
	if optionalHeader > len(data)-sizeOfOptionalHeader {
		return 0, 0, errors.New("PE Optional Header extends beyond file")
	}
	if binary.LittleEndian.Uint16(data[optionalHeader:optionalHeader+2]) != 0x020b {
		return 0, 0, errors.New("Helper is not PE32+")
	}
	machine := binary.LittleEndian.Uint16(data[peOffset+4 : peOffset+6])
	subsystem := binary.LittleEndian.Uint16(
		data[optionalHeader+subsystemDelta : optionalHeader+subsystemDelta+2],
	)
	return machine, subsystem, nil
}

func TestReadPEMachineAndSubsystemRejectsInvalidOptionalHeaderSize(t *testing.T) {
	const peOffset = 0x40
	const optionalHeader = peOffset + 24
	for _, test := range []struct {
		name                 string
		sizeOfOptionalHeader uint16
	}{
		{name: "zero", sizeOfOptionalHeader: 0},
		{name: "declared range extends beyond file", sizeOfOptionalHeader: 0x100},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := make([]byte, optionalHeader+70)
			binary.LittleEndian.PutUint16(data[0:2], 0x5a4d)
			binary.LittleEndian.PutUint32(data[0x3c:0x40], peOffset)
			binary.LittleEndian.PutUint32(data[peOffset:peOffset+4], 0x00004550)
			binary.LittleEndian.PutUint16(data[peOffset+4:peOffset+6], 0x8664)
			binary.LittleEndian.PutUint16(data[peOffset+20:peOffset+22], test.sizeOfOptionalHeader)
			binary.LittleEndian.PutUint16(data[optionalHeader:optionalHeader+2], 0x020b)
			binary.LittleEndian.PutUint16(data[optionalHeader+68:optionalHeader+70], 2)
			path := filepath.Join(t.TempDir(), "helper.exe")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if machine, subsystem, err := readPEMachineAndSubsystem(path); err == nil {
				t.Fatalf("invalid Optional Header size was accepted: machine=%#x subsystem=%d", machine, subsystem)
			}
		})
	}
}

func TestManifestContract(t *testing.T) {
	root := repoRoot(t)
	windres := requiredWindres(t)
	mt := newestMT(t)
	base := filepath.Join(root, ".superpowers", "tmp")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	work, err := os.MkdirTemp(base, "m5-manifest-")
	if err != nil {
		t.Fatal(err)
	}
	defer removeRunUniqueTemp(t, base, work)
	syso := filepath.Join(root, "cmd", "helper", "rsrc_windows_amd64.syso")
	if _, err := os.Lstat(syso); err == nil {
		t.Fatalf("refusing to overwrite pre-existing helper resource: %s", syso)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	claim, err := os.OpenFile(syso, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("claim generated helper resource exclusively: %v", err)
	}
	createdSyso := true
	defer func() {
		if createdSyso {
			if err := os.Remove(syso); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Errorf("remove generated helper resource: %v", err)
			}
		}
	}()
	if err := claim.Close(); err != nil {
		t.Fatal(err)
	}
	runCommand(t, root, windres, "-i", filepath.Join(root, "cmd", "helper", "helper.rc"), "-O", "coff", "-o", syso)
	goExe := filepath.Join(runtime.GOROOT(), "bin", "go.exe")
	if _, err := os.Stat(goExe); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(work, "helper.exe")
	command := exec.Command(
		goExe,
		"-C", root,
		"build", "-trimpath", "-ldflags=-H=windowsgui",
		"-o", exe,
		"./cmd/helper",
	)
	command.Env = append(os.Environ(), "GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("fresh helper build failed: %v\n%s", err, output)
	}
	if err := os.Remove(syso); err != nil {
		t.Fatal(err)
	}
	createdSyso = false
	machine, subsystem, err := readPEMachineAndSubsystem(exe)
	if err != nil {
		t.Fatalf("read Helper PE contract: %v", err)
	}
	if machine != 0x8664 {
		t.Fatalf("Helper PE machine = %#x, want AMD64 0x8664", machine)
	}
	if subsystem != 2 {
		t.Fatalf("Helper PE subsystem = %d, want WINDOWS_GUI 2", subsystem)
	}
	extracted := filepath.Join(work, "helper.extracted.manifest")
	runCommand(t, root, mt, "-inputresource:"+exe+";#1", "-out:"+extracted)
	body, err := os.ReadFile(extracted)
	if err != nil {
		t.Fatal(err)
	}
	var manifest extractedManifest
	if err := xml.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("parse extracted helper manifest: %v", err)
	}
	if manifest.RequestedExecutionLevel.Level != "requireAdministrator" || manifest.RequestedExecutionLevel.UIAccess != "false" {
		t.Fatalf("extracted helper manifest elevation = level=%q uiAccess=%q, want requireAdministrator/false", manifest.RequestedExecutionLevel.Level, manifest.RequestedExecutionLevel.UIAccess)
	}
	if _, err := os.Stat(syso); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("generated syso remained after manifest test: %v", err)
	}
}

func TestManifestContractRemovesOwnedResourceWhenWindresFails(t *testing.T) {
	root := repoRoot(t)
	syso := filepath.Join(root, "cmd", "helper", "rsrc_windows_amd64.syso")
	if _, err := os.Lstat(syso); err == nil {
		t.Fatalf("refusing to use pre-existing helper resource: %s", syso)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	ownedResource := false
	defer func() {
		if ownedResource {
			if err := os.Remove(syso); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Errorf("remove test-owned helper resource: %v", err)
			}
		}
	}()
	base := filepath.Join(root, ".superpowers", "tmp")
	work, err := os.MkdirTemp(base, "m5-windres-failure-")
	if err != nil {
		t.Fatal(err)
	}
	defer removeRunUniqueTemp(t, base, work)
	magic := "m5-windres-failure-" + filepath.Base(work)
	fakeWindres := filepath.Join(work, "windres-fail.cmd")
	if err := os.WriteFile(fakeWindres, []byte("@echo off\r\necho "+magic+"\r\necho "+magic+">\"%6\"\r\nexit /b 1\r\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go.exe"), "test", "-count=1", "./cmd/helper", "-run", "^TestManifestContract$")
	child.Dir = root
	child.Env = append(os.Environ(), "M5_WINDRES="+fakeWindres)
	output, err := child.CombinedOutput()
	if body, statErr := os.ReadFile(syso); statErr == nil && string(body) == magic+"\r\n" {
		ownedResource = true
	}
	if err == nil {
		t.Fatalf("manifest child unexpectedly passed with failing windres:\n%s", output)
	}
	if !strings.Contains(string(output), magic) || !strings.Contains(string(output), "windres-fail.cmd") || !strings.Contains(string(output), "failed: exit status 1") {
		t.Fatalf("manifest child did not reach the fake windres failure path:\n%s", output)
	}
	if _, err := os.Lstat(syso); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed windres left its owned helper resource: %v", err)
	}
}

func TestBuildScriptPackagesHelperAndDefaultConfigInFreshStage(t *testing.T) {
	root := repoRoot(t)
	base := filepath.Join(root, ".superpowers", "tmp")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	work, err := os.MkdirTemp(base, "m5-build-script-")
	if err != nil {
		t.Fatal(err)
	}
	defer removeRunUniqueTemp(t, base, work)
	out := filepath.Join(work, "fresh-stage")
	powershell := requiredPowerShell(t)
	goExe := requiredExecutable(t, "Go", filepath.Join(runtime.GOROOT(), "bin", "go.exe"))
	cc := os.Getenv("M5_CC")
	if cc == "" {
		cc = os.Getenv("CC")
	}
	if cc == "" {
		t.Fatal("build script test requires M5_CC or CC")
	}
	cc = requiredExecutable(t, "C compiler", cc)
	windres := requiredWindres(t)
	command := exec.Command(powershell, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", filepath.Join(root, "scripts", "build.ps1"), "-Go", goExe, "-CC", cc, "-Windres", windres, "-StageDir", out, "-SkipWebBuild", "-SkipNodeTrayBuild")
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("fresh build script failed: %v\n%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(out, "helper.exe")); err != nil {
		t.Fatalf("helper.exe was not packaged: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "helper.default.json")); err != nil {
		t.Fatalf("helper.default.json was not packaged: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "helper.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fresh stage unexpectedly contains operator helper.json: %v", err)
	}
	if matches, err := filepath.Glob(filepath.Join(root, "cmd", "helper", "*.syso")); err != nil {
		t.Fatal(err)
	} else if len(matches) != 0 {
		t.Fatalf("build script left Helper resource files: %v", matches)
	}
}

func TestBuildScriptFailsClosedWhenExactResourceCleanupFails(t *testing.T) {
	root := repoRoot(t)
	syso := filepath.Join(root, "cmd", "helper", "rsrc_windows_amd64.syso")
	if _, err := os.Lstat(syso); err == nil {
		t.Fatalf("refusing to use pre-existing helper resource: %s", syso)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	ownedResource := false
	magic := ""
	defer func() {
		if ownedResource {
			removeOwnedHelperResource(t, syso, magic)
		}
	}()
	base := filepath.Join(root, ".superpowers", "tmp")
	work, err := os.MkdirTemp(base, "m5-resource-cleanup-")
	if err != nil {
		t.Fatal(err)
	}
	defer removeRunUniqueTemp(t, base, work)
	magic = "m5-resource-cleanup-" + filepath.Base(work)
	fakeWindres := filepath.Join(work, "windres-resource-dir.cmd")
	if err := os.WriteFile(fakeWindres, []byte("@echo off\r\nmkdir \"%6\"\r\nif errorlevel 1 exit /b 2\r\necho "+magic+">\"%6\\marker\"\r\nif errorlevel 1 exit /b 3\r\necho "+magic+"\r\nexit /b 1\r\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(work, "out")
	powershell := requiredPowerShell(t)
	goExe := requiredExecutable(t, "Go", filepath.Join(runtime.GOROOT(), "bin", "go.exe"))
	cc := os.Getenv("M5_CC")
	if cc == "" {
		cc = os.Getenv("CC")
	}
	if cc == "" {
		t.Fatal("build script test requires M5_CC or CC")
	}
	cc = requiredExecutable(t, "C compiler", cc)
	command := exec.Command(powershell, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", filepath.Join(root, "scripts", "build.ps1"), "-Go", goExe, "-CC", cc, "-Windres", fakeWindres, "-StageDir", out, "-SkipWebBuild", "-SkipNodeTrayBuild")
	command.Dir = root
	output, err := command.CombinedOutput()
	if info, statErr := os.Stat(syso); statErr != nil || !info.IsDir() {
		t.Fatalf("cleanup failure did not leave the exact owned resource directory for diagnosis: info=%v err=%v", info, statErr)
	}
	marker := filepath.Join(syso, "marker")
	if body, markerErr := os.ReadFile(marker); markerErr != nil || string(body) != magic+"\r\n" {
		t.Fatalf("unexpected residual target after cleanup failure: %v", markerErr)
	}
	ownedResource = true
	if err == nil {
		t.Fatal("build unexpectedly passed despite unremovable generated resource")
	}
	if !strings.Contains(string(output), "remove generated Helper resource failed") || !strings.Contains(string(output), syso) || !strings.Contains(string(output), magic) {
		t.Fatalf("build did not report exact resource cleanup failure:\n%s", output)
	}
}

type extractedManifest struct {
	RequestedExecutionLevel struct {
		Level    string `xml:"level,attr"`
		UIAccess string `xml:"uiAccess,attr"`
	} `xml:"trustInfo>security>requestedPrivileges>requestedExecutionLevel"`
}

func requiredWindres(t *testing.T) string {
	t.Helper()
	if requested := os.Getenv("M5_WINDRES"); requested != "" {
		return requiredExecutable(t, "windres", requested)
	}
	cc := os.Getenv("M5_CC")
	if cc == "" {
		cc = os.Getenv("CC")
	}
	if cc == "" {
		t.Fatal("windres requires M5_WINDRES or M5_CC/CC so it can be resolved beside the approved compiler")
	}
	cc = requiredExecutable(t, "C compiler", cc)
	return requiredExecutable(t, "windres beside approved C compiler", filepath.Join(filepath.Dir(cc), "windres.exe"))
}

func newestMT(t *testing.T) string {
	t.Helper()
	if requested := os.Getenv("M5_MT"); requested != "" {
		return requiredExecutable(t, "mt.exe", requested)
	}
	matches, err := filepath.Glob(filepath.Join(`C:\Program Files (x86)\Windows Kits\10\bin`, "*", "x64", "mt.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("newest Windows SDK x64 mt.exe was not found")
	}
	sort.Slice(matches, func(i, j int) bool { return strings.Compare(matches[i], matches[j]) > 0 })
	return requiredExecutable(t, "newest Windows SDK x64 mt.exe", matches[0])
}

func requiredExecutable(t *testing.T, label, path string) string {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		t.Fatalf("required %s unavailable: %s: %v", label, path, err)
	}
	return path
}

func requiredPowerShell(t *testing.T) string {
	t.Helper()
	if requested := os.Getenv("M5_POWERSHELL"); requested != "" {
		return requiredExecutable(t, "PowerShell", requested)
	}
	for _, name := range []string{"pwsh.exe", "powershell.exe"} {
		if path, err := exec.LookPath(name); err == nil {
			return requiredExecutable(t, "PowerShell", path)
		}
	}
	t.Fatal("PowerShell was not found; set M5_POWERSHELL")
	return ""
}

func waitForFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", path)
}

func removeOwnedHelperResource(t *testing.T, syso, magic string) {
	t.Helper()
	marker := filepath.Join(syso, "marker")
	entries, err := os.ReadDir(syso)
	if err != nil {
		t.Errorf("read test-owned helper resource directory: %v", err)
		return
	}
	if len(entries) != 1 || entries[0].Name() != "marker" || entries[0].IsDir() {
		t.Errorf("refusing to recursively remove non-owned helper resource contents: %v", entries)
		return
	}
	body, err := os.ReadFile(marker)
	if err != nil || string(body) != magic+"\r\n" {
		t.Errorf("refusing to remove helper resource with mismatched ownership marker: %v", err)
		return
	}
	if err := os.Remove(marker); err != nil {
		t.Errorf("remove test-owned helper resource marker: %v", err)
		return
	}
	if err := os.Remove(syso); err != nil {
		t.Errorf("remove now-empty test-owned helper resource directory: %v", err)
	}
}

func removeRunUniqueTemp(t *testing.T, base, work string) {
	t.Helper()
	baseFull, err := filepath.Abs(base)
	if err != nil {
		t.Fatal(err)
	}
	workFull, err := filepath.Abs(work)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(filepath.Dir(workFull), filepath.Clean(baseFull)) {
		t.Fatalf("refusing to remove non-run-unique manifest directory: %s", workFull)
	}
	if err := os.RemoveAll(workFull); err != nil {
		t.Fatalf("remove manifest test directory: %v", err)
	}
}

func testDependencies(events *[]string) dependencies {
	return dependencies{
		identity: func() (machineid.Result, error) {
			return machineid.Result{ID: "node-" + strings.Repeat("a", 64), CPUAvailable: true}, nil
		},
		now: func() time.Time { return time.Unix(1, 0) },
		loadConfig: func(string, string) (helper.Config, error) {
			*events = append(*events, "load")
			return helper.Config{LogDir: "logs"}, nil
		},
		newLogger: func(string) (*slog.Logger, func() error, error) {
			*events = append(*events, "logger")
			return slog.New(slog.NewTextHandler(io.Discard, nil)), func() error {
				*events = append(*events, "logger.close")
				return nil
			}, nil
		},
		acquireLock: func(string) (helper.InstanceLock, error) {
			*events = append(*events, "lock")
			return &recordingLock{events: events}, nil
		},
		listenPipe: func(helper.Config) (net.Listener, error) {
			*events = append(*events, "listener")
			return &recordingListener{events: events}, nil
		},
		newValidator: func(helper.Config) (*helper.Validator, error) {
			*events = append(*events, "validator")
			return nil, nil
		},
		newProcessor: func(helper.Config, *helper.Validator) *helper.Processor {
			*events = append(*events, "processor")
			return nil
		},
		newServer: func(helper.Config, net.Listener, *helper.Processor, *slog.Logger) helperServer {
			*events = append(*events, "server")
			return serverFunc(func(context.Context) error { return nil })
		},
		newControl: func(provider nodectl.StatusProvider, shutdown nodectl.ShutdownFunc) helperControlService {
			return controlFunc(func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			})
		},
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func runCommand(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	command := exec.Command(name, args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(args, " "), err, output)
	}
}

type serverFunc func(context.Context) error

func (f serverFunc) Serve(ctx context.Context) error { return f(ctx) }
func (serverFunc) ActiveRequests() int               { return 0 }
func (serverFunc) Listening() bool                   { return true }

type controlFunc func(context.Context) error

func (f controlFunc) Run(ctx context.Context) error { return f(ctx) }

type recordingLock struct {
	events   *[]string
	closeErr error
}

func (l *recordingLock) Close() error {
	*l.events = append(*l.events, "lock.close")
	return l.closeErr
}

type recordingListener struct {
	events     *[]string
	closeErr   error
	closeCalls int
}

func (l *recordingListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }
func (l *recordingListener) Close() error {
	l.closeCalls++
	*l.events = append(*l.events, "listener.close")
	return l.closeErr
}
func (l *recordingListener) Addr() net.Addr { return &net.TCPAddr{} }
