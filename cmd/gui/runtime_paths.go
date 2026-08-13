package main

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/natefinch/lumberjack.v2"
)

type guiRuntimePaths struct {
	Root       string
	ConfigPath string
	LogPath    string
}

func resolveGUIExecutablePath(
	executablePath func() (string, error),
	finalPath func(string) (string, error),
) (string, error) {
	if executablePath == nil || finalPath == nil {
		return "", fmt.Errorf("GUI executable path resolver is unavailable")
	}
	executable, err := executablePath()
	if err != nil {
		return "", fmt.Errorf("resolve GUI executable path: %w", err)
	}
	resolved, err := finalPath(executable)
	if err != nil {
		return "", fmt.Errorf("resolve GUI executable final path: %w", err)
	}
	return resolved, nil
}

func resolveGUIRuntimePaths(executable, requestedConfig string) (guiRuntimePaths, error) {
	absExecutable, err := filepath.Abs(executable)
	if err != nil {
		return guiRuntimePaths{}, fmt.Errorf("resolve executable path: %w", err)
	}
	root := filepath.Dir(absExecutable)
	if strings.HasPrefix(root, `\\`) {
		return guiRuntimePaths{}, fmt.Errorf("GUI does not support UNC executable roots")
	}
	configPath := requestedConfig
	if configPath == "" {
		configPath = filepath.Join(root, "gui.json")
	} else {
		configPath, err = filepath.Abs(configPath)
		if err != nil {
			return guiRuntimePaths{}, fmt.Errorf("resolve config path: %w", err)
		}
	}
	return guiRuntimePaths{
		Root:       root,
		ConfigPath: configPath,
		LogPath:    filepath.Join(root, "data", "logs", "gui.log"),
	}, nil
}

func newGUIRuntimeLogger(logPath string, console io.Writer) (*slog.Logger, func() error, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, nil, fmt.Errorf("create GUI log directory: %w", err)
	}
	fileLog := &lumberjack.Logger{Filename: logPath, MaxSize: 10, MaxBackups: 5, MaxAge: 14}
	logger := slog.New(slog.NewJSONHandler(io.MultiWriter(console, fileLog), nil))
	return logger, fileLog.Close, nil
}

func localBrowserURL(listenAddr string) (string, error) {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "", fmt.Errorf("invalid GUI listener address: %w", err)
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::", "[::]":
		host = "::1"
	}
	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, port), Path: "/"}).String(), nil
}
