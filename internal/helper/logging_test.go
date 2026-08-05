package helper

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHelperLoggerWritesAndClosesJSONHelperLog(t *testing.T) {
	parent := t.TempDir()
	logDir := filepath.Join(parent, "helper-logs")
	logger, closeLog, err := NewLogger(logDir)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	logger.Info("helper lifecycle", "event", "started")
	if err := closeLog(); err != nil {
		t.Fatalf("close helper logger: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(logDir, "helper.log"))
	if err != nil {
		t.Fatalf("read helper.log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("helper.log lines = %d, want 1", len(lines))
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("helper.log is not JSONL: %v", err)
	}
	if record["msg"] != "helper lifecycle" || record["event"] != "started" {
		t.Fatalf("helper.log record = %#v", record)
	}
	if err := os.RemoveAll(logDir); err != nil {
		t.Fatalf("remove closed log directory: %v", err)
	}
	if _, err := os.Stat(logDir); !os.IsNotExist(err) {
		t.Fatalf("log directory residue after removal: %v", err)
	}
}

func TestHelperLoggerRuntimeSinkUsesRequiredRotationPolicy(t *testing.T) {
	logDir := filepath.Join(t.TempDir(), "runtime-sink")
	logger, sink, closeLog, err := newHelperLogger(logDir)
	if err != nil {
		t.Fatalf("newHelperLogger: %v", err)
	}
	if logger == nil {
		t.Fatal("newHelperLogger returned nil logger")
	}
	t.Cleanup(func() { _ = closeLog() })
	if sink.Filename != filepath.Join(logDir, "helper.log") {
		t.Fatalf("sink filename = %q", sink.Filename)
	}
	if sink.MaxSize != 100 ||
		sink.MaxBackups != 5 ||
		sink.MaxAge != 30 ||
		!sink.Compress {
		t.Fatalf(
			"sink rotation = size=%d backups=%d age=%d compress=%v",
			sink.MaxSize,
			sink.MaxBackups,
			sink.MaxAge,
			sink.Compress,
		)
	}
	if err := closeLog(); err != nil {
		t.Fatalf("close helper logger: %v", err)
	}
}
