package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"dedup/internal/config"
)

var errGUIRestartInProgress = errors.New("GUI restart is already in progress")

type guiRestartCoordinator interface {
	Pending() bool
	Prepare(*config.GUIConfig) (string, error)
	Commit()
}

type atomicGUIRestartCoordinator struct {
	executable string
	configPath string
	parentPID  int
	cancel     context.CancelFunc
	start      func(string, []string) error
	pending    atomic.Bool
	commitOnce sync.Once
}

var guiLaunchReplacement = guiStartReplacement

func newGUIRestartCoordinator(
	executable, configPath string,
	parentPID int,
	cancel context.CancelFunc,
) guiRestartCoordinator {
	return &atomicGUIRestartCoordinator{
		executable: executable,
		configPath: configPath,
		parentPID:  parentPID,
		cancel:     cancel,
		start:      guiLaunchReplacement,
	}
}

func (c *atomicGUIRestartCoordinator) Pending() bool {
	return c.pending.Load()
}

func (c *atomicGUIRestartCoordinator) Prepare(cfg *config.GUIConfig) (string, error) {
	if cfg == nil {
		return "", errors.New("restart configuration is nil")
	}
	if !filepath.IsAbs(c.executable) {
		return "", fmt.Errorf("restart executable is not absolute: %s", c.executable)
	}
	if !filepath.IsAbs(c.configPath) {
		return "", fmt.Errorf("restart config path is not absolute: %s", c.configPath)
	}
	if !c.pending.CompareAndSwap(false, true) {
		return "", errGUIRestartInProgress
	}

	browserURL, err := localBrowserURL(cfg.ListenAddr)
	if err != nil {
		c.pending.Store(false)
		return "", err
	}
	args := []string{
		"-config", c.configPath,
		"-no-browser",
		"-wait-parent-pid", strconv.Itoa(c.parentPID),
	}
	if err := c.start(c.executable, args); err != nil {
		c.pending.Store(false)
		return "", err
	}
	return strings.TrimSuffix(browserURL, "/") + "/api/restart/health", nil
}

func (c *atomicGUIRestartCoordinator) Commit() {
	if !c.pending.Load() || c.cancel == nil {
		return
	}
	c.commitOnce.Do(c.cancel)
}
