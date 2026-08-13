package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"dedup/internal/config"
)

var errGUIRestartInProgress = errors.New("GUI restart is already in progress")

type guiRestartCoordinator interface {
	Begin() bool
	End()
	Pending() bool
	Prepare(*config.GUIConfig) (string, error)
	Commit()
	Abort()
}

type guiPreparedReplacement interface {
	Commit() error
	Abort() error
}

type atomicGUIRestartCoordinator struct {
	executable  string
	configPath  string
	parentPID   int
	cancel      context.CancelFunc
	start       func(string, []string) (guiPreparedReplacement, error)
	requestMu   sync.Mutex
	replaceMu   sync.Mutex
	replacement guiPreparedReplacement
	pending     atomic.Bool
	commitOnce  sync.Once
}

var guiPrepareReplacementLaunch = guiPrepareReplacement

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
		start:      guiPrepareReplacementLaunch,
	}
}

func (c *atomicGUIRestartCoordinator) Begin() bool {
	c.requestMu.Lock()
	if c.pending.Load() {
		c.requestMu.Unlock()
		return false
	}
	return true
}

func (c *atomicGUIRestartCoordinator) End() {
	c.requestMu.Unlock()
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
	restartToken, err := generateGUIRestartToken()
	if err != nil {
		c.pending.Store(false)
		return "", err
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
		"-restart-token", restartToken,
	}
	replacement, err := c.start(c.executable, args)
	if err != nil {
		c.pending.Store(false)
		return "", err
	}
	c.replaceMu.Lock()
	c.replacement = replacement
	c.replaceMu.Unlock()
	return strings.TrimSuffix(browserURL, "/") + "/api/restart/health?restart_token=" + url.QueryEscape(restartToken), nil
}

func generateGUIRestartToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate restart token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func (c *atomicGUIRestartCoordinator) Commit() {
	if !c.pending.Load() {
		return
	}
	c.commitOnce.Do(func() {
		if replacement := c.takeReplacement(); replacement != nil {
			_ = replacement.Commit()
		}
		if c.cancel != nil {
			c.cancel()
		}
	})
}

func (c *atomicGUIRestartCoordinator) Abort() {
	if replacement := c.takeReplacement(); replacement != nil {
		_ = replacement.Abort()
	}
	c.pending.Store(false)
}

func (c *atomicGUIRestartCoordinator) takeReplacement() guiPreparedReplacement {
	c.replaceMu.Lock()
	defer c.replaceMu.Unlock()
	replacement := c.replacement
	c.replacement = nil
	return replacement
}
