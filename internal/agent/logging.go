package agent

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"gopkg.in/natefinch/lumberjack.v2"
)

func NewLoggers(
	dataDir string,
	console io.Writer,
) (
	agentLog *slog.Logger,
	errorLog *slog.Logger,
	closeLogs func() error,
	err error,
) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, nil, nil, err
	}
	agentFile := &lumberjack.Logger{
		Filename:   filepath.Join(dataDir, "agent.log"),
		MaxSize:    100,
		MaxBackups: 5,
		MaxAge:     30,
		Compress:   true,
	}
	errorsFile := &lumberjack.Logger{
		Filename:   filepath.Join(dataDir, "errors.log"),
		MaxSize:    100,
		MaxBackups: 5,
		MaxAge:     30,
		Compress:   true,
	}
	var agentWriter io.Writer = agentFile
	if console != nil {
		agentWriter = io.MultiWriter(console, agentFile)
	}
	agentLog = slog.New(slog.NewJSONHandler(agentWriter, nil))
	errorLog = slog.New(slog.NewJSONHandler(errorsFile, nil))
	closeLogs = func() error {
		return errors.Join(agentFile.Close(), errorsFile.Close())
	}
	return agentLog, errorLog, closeLogs, nil
}

// NewCrashLogger creates the dedicated JSON-lines worker crash log.
// The caller supplies the stable crash fields on each record.
func NewCrashLogger(dataDir string) (*slog.Logger, func() error, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, nil, err
	}
	crashFile := &lumberjack.Logger{
		Filename:   filepath.Join(dataDir, "crash.log"),
		MaxSize:    100,
		MaxBackups: 5,
		MaxAge:     30,
		Compress:   true,
	}
	return slog.New(slog.NewJSONHandler(crashFile, nil)), crashFile.Close, nil
}

// NewDeleteLogger creates the dedicated JSON-lines local delete audit log.
func NewDeleteLogger(dataDir string) (*slog.Logger, func() error, error) {
	logger, _, closeLog, err := newDeleteLogger(dataDir)
	return logger, closeLog, err
}

func newDeleteLogger(dataDir string) (*slog.Logger, *lumberjack.Logger, func() error, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, nil, nil, err
	}
	sink := &lumberjack.Logger{
		Filename:   filepath.Join(dataDir, "delete.log"),
		MaxSize:    100,
		MaxBackups: 5,
		MaxAge:     30,
		Compress:   true,
	}
	var (
		closeOnce sync.Once
		closeErr  error
	)
	closeLog := func() error {
		closeOnce.Do(func() {
			closeErr = sink.Close()
		})
		return closeErr
	}
	return slog.New(slog.NewJSONHandler(sink, nil)), sink, closeLog, nil
}
