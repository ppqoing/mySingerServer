package stats

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"gopkg.in/natefinch/lumberjack.v2"
)

type JSONLSink struct {
	mu      sync.Mutex
	writer  *lumberjack.Logger
	encoder *json.Encoder
}

func NewJSONLSink(path string, maxMB int) (*JSONLSink, error) {
	if path == "" {
		return nil, fmt.Errorf("stats: log path is empty")
	}
	if maxMB < 1 {
		return nil, fmt.Errorf("stats: max log size must be positive")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("stats: create log directory: %w", err)
	}
	writer := &lumberjack.Logger{
		Filename:   path,
		MaxSize:    maxMB,
		MaxBackups: 3,
		MaxAge:     7,
		Compress:   false,
	}
	return &JSONLSink{writer: writer, encoder: json.NewEncoder(writer)}, nil
}

func (s *JSONLSink) Write(snapshot Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.encoder.Encode(snapshot)
}

func (s *JSONLSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writer.Close()
}
