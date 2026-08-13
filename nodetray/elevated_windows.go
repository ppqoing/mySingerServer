//go:build windows && !bindings

package main

import (
	"context"
	"errors"
	"os"
	"strings"

	"dedup/internal/nodetray/bootstrap"
	elevatedactions "dedup/internal/nodetray/elevated"
	"dedup/internal/nodetray/process"
	"dedup/internal/nodetray/production"
	"dedup/internal/nodetray/windows/elevation"
	"dedup/internal/nodetray/windows/task"
)

type elevatedOnceFactories struct {
	Inspector   process.Inspector
	FinalPath   func(string) (string, error)
	UserSID     func(process.Identity) (string, error)
	NewTask     func(task.Capability) (task.Service, error)
	NewExecutor func(string, task.Service, task.Definition, task.Capability) (elevation.Handler, error)
	Serve       func(context.Context, string, string, process.Inspector, elevation.HandlerFactory) error
}

func init() {
	runElevatedOnce = runWindowsElevatedOnce
}

func runWindowsElevatedOnce(pipeName, nonce string) error {
	inspector := process.NewInspector()
	return runElevatedOnceWith(pipeName, nonce, elevatedOnceFactories{
		Inspector: inspector,
		FinalPath: func(path string) (string, error) { return (bootstrap.OSFinalPathResolver{}).Final(path) },
		UserSID:   process.UserSIDForProcess,
		NewTask:   task.New,
		NewExecutor: func(helperConfig string, service task.Service, definition task.Definition, capability task.Capability) (elevation.Handler, error) {
			return elevatedactions.NewExecutor(helperConfig, service, definition, capability)
		},
		Serve: elevation.ServeOnceWithHandlerFactory,
	})
}

func runElevatedOnceWith(pipeName, nonce string, factories elevatedOnceFactories) error {
	if factories.Inspector == nil || factories.FinalPath == nil || factories.UserSID == nil || factories.NewTask == nil ||
		factories.NewExecutor == nil || factories.Serve == nil {
		return errors.New("elevated composition: dependencies unavailable")
	}
	if err := elevation.ValidateNonce(nonce); err != nil || pipeName != `\\.\pipe\mysingerserver-elevate-`+nonce {
		return errors.New("elevated composition: invalid one-shot endpoint")
	}
	self, err := factories.Inspector.Inspect(os.Getpid())
	if err != nil || self.PID != os.Getpid() || self.StartedAtUnixMS <= 0 || self.ExecutablePath == "" {
		return errors.New("elevated composition: current process identity unavailable")
	}
	layout, err := production.ResolvePortableLayout(self.ExecutablePath)
	if err != nil {
		return errors.New("elevated composition: portable layout unavailable")
	}
	finalTray, err := factories.FinalPath(layout.TrayExecutable)
	if err != nil || finalTray == "" {
		return errors.New("elevated composition: portable tray executable unavailable")
	}
	expected := self
	expected.ExecutablePath = finalTray
	if !process.SameProcess(expected, self) {
		return errors.New("elevated composition: current executable is outside portable deployment")
	}
	return factories.Serve(context.Background(), pipeName, nonce, factories.Inspector, func(parent process.Identity) (elevation.Handler, error) {
		userSID, err := factories.UserSID(parent)
		if err != nil || userSID == "" || strings.TrimSpace(userSID) != userSID || !strings.HasPrefix(userSID, "S-1-") {
			return nil, errors.New("elevated composition: ordinary parent identity unavailable")
		}
		service, err := factories.NewTask(task.CapabilityElevated)
		if err != nil || service == nil {
			return nil, errors.New("elevated composition: elevated task service unavailable")
		}
		definition := task.Definition{
			HelperExecutable: layout.HelperExecutable,
			HelperConfig:     layout.HelperConfig,
			UserSID:          userSID,
		}
		handler, err := factories.NewExecutor(layout.HelperConfig, service, definition, task.CapabilityElevated)
		if err != nil || handler == nil {
			return nil, errors.New("elevated composition: fixed action executor unavailable")
		}
		return handler, nil
	})
}
