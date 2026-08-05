package helper

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"gopkg.in/natefinch/lumberjack.v2"
)

func NewLogger(logDir string) (*slog.Logger, func() error, error) {
	logger, _, closeLogger, err := newHelperLogger(logDir)
	return logger, closeLogger, err
}

func newHelperLogger(logDir string) (*slog.Logger, *lumberjack.Logger, func() error, error) {
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return nil, nil, nil, fmt.Errorf("create helper log directory: %w", err)
	}

	sink := &lumberjack.Logger{
		Filename:   filepath.Join(logDir, "helper.log"),
		MaxSize:    100,
		MaxBackups: 5,
		MaxAge:     30,
		Compress:   true,
	}
	logger := slog.New(slog.NewJSONHandler(sink, nil))

	var (
		closeOnce sync.Once
		closeErr  error
	)
	closeLogger := func() error {
		closeOnce.Do(func() {
			closeErr = sink.Close()
		})
		return closeErr
	}

	return logger, sink, closeLogger, nil
}
