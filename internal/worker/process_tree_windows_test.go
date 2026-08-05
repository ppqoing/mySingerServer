package worker

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

const processTreeHelperModeEnv = "GO_PROCESS_TREE_HELPER_MODE"
const processTreePIDFileEnv = "GO_PROCESS_TREE_PID_FILE"
const processTreeStartFileEnv = "GO_PROCESS_TREE_START_FILE"

func TestMain(m *testing.M) {
	switch os.Getenv(processTreeHelperModeEnv) {
	case "child":
		for {
			time.Sleep(time.Hour)
		}
	case "direct-parent":
		os.Exit(runDirectProcessTreeParent())
	case "pool-worker":
		os.Exit(runPoolProcessTreeWorker())
	case "exit":
		os.Exit(0)
	default:
		os.Exit(m.Run())
	}
}

func TestManagedProcessKillWaitTerminatesDescendant(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "direct-child.pid")
	startFile := filepath.Join(t.TempDir(), "start-child")
	process, err := launchWorkerProcess(Config{
		WorkerExe: os.Args[0],
		WorkerEnv: []string{
			processTreeHelperModeEnv + "=direct-parent",
			processTreePIDFileEnv + "=" + pidFile,
			processTreeStartFileEnv + "=" + startFile,
		},
	}, `\\.\pipe\unused-process-tree-test`, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = process.Kill()
		_, _ = process.Wait()
		if closer, ok := process.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	})
	if err := os.WriteFile(startFile, []byte("start"), 0o600); err != nil {
		t.Fatal(err)
	}
	childPID := waitPIDFile(t, pidFile, 5*time.Second)
	t.Cleanup(func() { terminateTestPID(childPID) })

	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := process.Kill(); err != nil {
		t.Fatalf("second Kill: %v", err)
	}
	if _, err := process.Wait(); err == nil {
		t.Fatal("killed worker Wait returned nil error")
	}
	if !waitForPIDExit(childPID, 500*time.Millisecond) {
		t.Fatalf("worker descendant pid %d remained alive after managed Kill/Wait", childPID)
	}
}

func TestManagedProcessWaitAndCloseAreIdempotent(t *testing.T) {
	process, err := launchWorkerProcess(Config{
		WorkerExe: os.Args[0],
		WorkerEnv: []string{processTreeHelperModeEnv + "=exit"},
	}, `\\.\pipe\unused-process-close-test`, 0)
	if err != nil {
		t.Fatal(err)
	}
	firstCode, firstErr := process.Wait()
	secondCode, secondErr := process.Wait()
	if firstCode != secondCode || !errors.Is(secondErr, firstErr) {
		t.Fatalf("Wait results differ: first %d/%v second %d/%v",
			firstCode, firstErr, secondCode, secondErr)
	}
	closer, ok := process.(interface{ Close() error })
	if !ok {
		t.Fatal("managed process does not expose Close")
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestPoolWatchdogTerminatesRealWorkerDescendantBeforeReplacementReady(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pool-child.pid")
	store := &poolTestStore{}
	deps := defaultSupervisorDeps()
	ready := make(chan ReadyMsg, 4)
	deps.ready = func(msg ReadyMsg) { ready <- msg }
	pool := newPoolWithDeps(Config{
		WorkerExe: os.Args[0],
		WorkerEnv: []string{
			processTreeHelperModeEnv + "=pool-worker",
			processTreePIDFileEnv + "=" + pidFile,
		},
		WorkerCount:     1,
		ReadyTimeout:    5 * time.Second,
		VideoTimeout:    200 * time.Millisecond,
		RespawnDelay:    10 * time.Millisecond,
		ShutdownTimeout: 2 * time.Second,
	}, store, deps)
	pool.Start()
	t.Cleanup(pool.Close)

	firstReady := waitReady(t, ready, 5*time.Second)
	job := JobMsg{
		JobID: 801, ScanTaskID: "task-process-tree",
		Path: `D:\media\watchdog-tree.mp4`, Kind: MediaVideo, Phase: Phase1,
	}
	if err := pool.Submit(&job); err != nil {
		t.Fatal(err)
	}
	childPID := waitPIDFile(t, pidFile, 5*time.Second)
	t.Cleanup(func() { terminateTestPID(childPID) })
	select {
	case crash := <-pool.Crashes():
		if crash.Reason != "watchdog_video" || crash.PID != firstReady.PID {
			t.Fatalf("watchdog crash = %#v", crash)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("video watchdog did not fire")
	}
	secondReady := waitReady(t, ready, 5*time.Second)
	if secondReady.PID == firstReady.PID {
		t.Fatalf("replacement reused pid %d", secondReady.PID)
	}
	if processAlive(childPID) {
		t.Fatalf("replacement pid %d became Ready while old descendant pid %d was alive",
			secondReady.PID, childPID)
	}
	pool.Close()
	if got := pool.Metrics().Crashes; got != 1 {
		t.Fatalf("normal replacement shutdown changed crash count to %d, want 1", got)
	}
}

func runDirectProcessTreeParent() int {
	startFile := os.Getenv(processTreeStartFileEnv)
	if startFile == "" {
		return 2
	}
	for {
		if _, err := os.Stat(startFile); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return 2
		}
		time.Sleep(10 * time.Millisecond)
	}
	child, err := startProcessTreeChild()
	if err != nil {
		return 2
	}
	if err := writeHelperPID(child.Process.Pid); err != nil {
		_ = child.Process.Kill()
		return 2
	}
	for {
		time.Sleep(time.Hour)
	}
}

func runPoolProcessTreeWorker() int {
	pipeName := helperArgument("--pipe=")
	indexRaw := helperArgument("--worker-index=")
	index, err := strconv.Atoi(indexRaw)
	if err != nil || pipeName == "" {
		return 2
	}
	timeout := 5 * time.Second
	conn, err := winio.DialPipe(pipeName, &timeout)
	if err != nil {
		return 2
	}
	defer conn.Close()
	ipc := NewIPCConn(conn)
	ready := validReadyForTest()
	ready.PID, ready.WorkerIndex = os.Getpid(), index
	if err := ipc.Write(MsgReady, ready); err != nil {
		return 2
	}
	for {
		envelope, err := ipc.Read()
		if err != nil {
			return 2
		}
		switch envelope.Type {
		case MsgShutdown:
			return 0
		case MsgJob:
			child, err := startProcessTreeChild()
			if err != nil {
				return 2
			}
			if err := writeHelperPID(child.Process.Pid); err != nil {
				_ = child.Process.Kill()
				return 2
			}
			for {
				time.Sleep(time.Hour)
			}
		default:
			return 2
		}
	}
}

func startProcessTreeChild() (*exec.Cmd, error) {
	command := exec.Command(os.Args[0])
	command.Env = append(os.Environ(), processTreeHelperModeEnv+"=child")
	if err := command.Start(); err != nil {
		return nil, err
	}
	return command, nil
}

func writeHelperPID(pid int) error {
	path := os.Getenv(processTreePIDFileEnv)
	if path == "" {
		return fmt.Errorf("%s is empty", processTreePIDFileEnv)
	}
	return os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o600)
}

func helperArgument(prefix string) string {
	for _, argument := range os.Args[1:] {
		if strings.HasPrefix(argument, prefix) {
			return strings.TrimPrefix(argument, prefix)
		}
	}
	return ""
}

func waitPIDFile(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr == nil && pid > 0 {
				return pid
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for helper pid file %s", path)
	return 0
}

func waitReady(t *testing.T, ready <-chan ReadyMsg, timeout time.Duration) ReadyMsg {
	t.Helper()
	select {
	case message := <-ready:
		return message
	case <-time.After(timeout):
		t.Fatal("timed out waiting for worker Ready")
		return ReadyMsg{}
	}
}

func processAlive(pid int) bool {
	handle, err := windows.OpenProcess(
		windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false,
		uint32(pid),
	)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	status, err := windows.WaitForSingleObject(handle, 0)
	return err == nil && status == uint32(windows.WAIT_TIMEOUT)
}

func waitForPIDExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return !processAlive(pid)
}

func terminateTestPID(pid int) {
	handle, err := windows.OpenProcess(
		windows.PROCESS_TERMINATE|windows.SYNCHRONIZE,
		false,
		uint32(pid),
	)
	if err != nil {
		return
	}
	defer windows.CloseHandle(handle)
	_ = windows.TerminateProcess(handle, 99)
	_, _ = windows.WaitForSingleObject(handle, 5000)
}
