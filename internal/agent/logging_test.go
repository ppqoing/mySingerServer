package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewLoggersWriteSeparateJSONLineFiles(t *testing.T) {
	dataDir := t.TempDir()
	log, errorLog, closeLogs, err := NewLoggers(dataDir, nil)
	if err != nil {
		t.Fatalf("NewLoggers: %v", err)
	}
	log.Info("agent started", "machine_id", "machine-a")
	errorLog.Error("file error", "path", `D:\bad.bin`, "stage", "hash")
	if err := closeLogs(); err != nil {
		t.Fatalf("close logs: %v", err)
	}

	for _, file := range []string{"agent.log", "errors.log"} {
		data, err := os.ReadFile(filepath.Join(dataDir, file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		if len(lines) != 1 {
			t.Fatalf("%s lines = %d, want 1", file, len(lines))
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
			t.Fatalf("%s is not JSONL: %v", file, err)
		}
	}
}

func TestDeleteLoggerWritesOrderedAuditJSONLAndClosesIdempotently(t *testing.T) {
	// Catches a logger that omits an audit field, writes state failure before
	// physical result, tees to an unrelated sink, or cannot safely close twice.
	dataDir := filepath.Join(t.TempDir(), "delete-logs")
	logger, closeLog, err := NewDeleteLogger(dataDir)
	if err != nil {
		t.Fatalf("NewDeleteLogger: %v", err)
	}
	logger.Info("delete_physical_result",
		"task_id", "task-42", "machine_id", "machine-a", "seq", 7,
		"path", `D:\media\gone.jpg`, "mode", "recycle", "ok", true,
		"err_code", "", "err", "", "readonly_cleared", true,
		"recycled_to", `C:\$Recycle.Bin\gone.jpg`, "uncertain", false,
	)
	logger.Error("delete_state_sync_error", "task_id", "task-42", "err", "sqlite busy")
	if err := closeLog(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := closeLog(); err != nil {
		t.Fatalf("second close: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dataDir, "delete.log"))
	if err != nil {
		t.Fatalf("read delete.log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("delete.log lines = %d, want 2", len(lines))
	}
	var physical, stateError map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &physical); err != nil {
		t.Fatalf("physical result JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &stateError); err != nil {
		t.Fatalf("state error JSON: %v", err)
	}
	for key, want := range map[string]any{
		"task_id": "task-42", "machine_id": "machine-a", "seq": float64(7),
		"path": `D:\media\gone.jpg`, "mode": "recycle", "ok": true,
		"err_code": "", "err": "", "readonly_cleared": true,
		"recycled_to": `C:\$Recycle.Bin\gone.jpg`, "uncertain": false,
	} {
		if got := physical[key]; got != want {
			t.Fatalf("physical audit %q = %#v, want %#v", key, got, want)
		}
	}
	if physical["msg"] != "delete_physical_result" || stateError["msg"] != "delete_state_sync_error" {
		t.Fatalf("audit ordering = %#v then %#v", physical["msg"], stateError["msg"])
	}
	if err := os.RemoveAll(dataDir); err != nil {
		t.Fatalf("remove closed delete logs: %v", err)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("delete log residue after removal: %v", err)
	}
}

func TestDeleteLoggerRuntimeSinkUsesRequiredRotationPolicy(t *testing.T) {
	// Catches a dedicated audit sink that silently diverges from the required
	// retention policy even though it can still write syntactically valid JSON.
	dataDir := filepath.Join(t.TempDir(), "runtime-sink")
	logger, sink, closeLog, err := newDeleteLogger(dataDir)
	if err != nil {
		t.Fatalf("newDeleteLogger: %v", err)
	}
	if logger == nil {
		t.Fatal("newDeleteLogger returned nil logger")
	}
	if sink.Filename != filepath.Join(dataDir, "delete.log") || sink.MaxSize != 100 ||
		sink.MaxBackups != 5 || sink.MaxAge != 30 || !sink.Compress {
		t.Fatalf("sink = filename:%q size:%d backups:%d age:%d compress:%v",
			sink.Filename, sink.MaxSize, sink.MaxBackups, sink.MaxAge, sink.Compress)
	}
	if err := closeLog(); err != nil {
		t.Fatalf("close delete logger: %v", err)
	}
}

func TestNewCrashLoggerWritesRequiredJSONFields(t *testing.T) {
	dataDir := t.TempDir()
	crashLog, closeLog, err := NewCrashLogger(dataDir)
	if err != nil {
		t.Fatalf("NewCrashLogger: %v", err)
	}
	crashLog.Info("worker crash",
		"pid", 4321,
		"worker_index", 2,
		"file", `D:\media\bad.jpg`,
		"exit_code", -1073741819,
		"reason", "exit_code",
	)
	if err := closeLog(); err != nil {
		t.Fatalf("close crash log: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dataDir, "crash.log"))
	if err != nil {
		t.Fatalf("read crash.log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("crash.log lines = %d, want 1", len(lines))
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("crash.log is not JSONL: %v", err)
	}
	for key, want := range map[string]any{
		"pid": float64(4321), "worker_index": float64(2),
		"file": `D:\media\bad.jpg`, "exit_code": float64(-1073741819),
		"reason": "exit_code",
	} {
		if got := record[key]; got != want {
			t.Fatalf("crash field %q = %#v, want %#v", key, got, want)
		}
	}
	if ts, ok := record["time"].(string); !ok || ts == "" {
		t.Fatalf("crash timestamp = %#v, want non-empty JSON time", record["time"])
	}
}
