//go:build windows && !bindings

package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"dedup/internal/nodetray/process"
	"dedup/internal/nodetray/production"
	"dedup/internal/nodetray/windows/elevation"
	"dedup/internal/nodetray/windows/task"
)

var elevatedCompositionNonce = strings.Repeat("1", 64)

type elevatedCompositionInspector struct{ identities map[int]process.Identity }

func (i elevatedCompositionInspector) Inspect(pid int) (process.Identity, error) {
	value, ok := i.identities[pid]
	if !ok {
		return process.Identity{}, errors.New("missing identity")
	}
	return value, nil
}
func (elevatedCompositionInspector) Wait(context.Context, process.Identity) (int, error) {
	return 0, errors.New("not called")
}

type elevatedCompositionHandler struct{}

func (elevatedCompositionHandler) Execute(context.Context, elevation.Request) elevation.Response {
	return elevation.Response{}
}

func TestElevatedOnceFreezesAuthorityFromValidatedOrdinaryParent(t *testing.T) {
	layout, err := production.ResolvePortableLayout(`D:\便携 工具\Compute\nodetray.exe`)
	if err != nil {
		t.Fatalf("ResolveLayout: %v", err)
	}
	self := process.Identity{PID: os.Getpid(), StartedAtUnixMS: 100, ExecutablePath: layout.TrayExecutable}
	parent := process.Identity{PID: 3131, StartedAtUnixMS: 200, ExecutablePath: layout.TrayExecutable}
	inspector := elevatedCompositionInspector{identities: map[int]process.Identity{self.PID: self, parent.PID: parent}}
	parentSID := "S-1-5-21-101-202-303-1001"
	var sidIdentity process.Identity
	var gotCapability task.Capability
	var gotHelperConfig string
	var gotDefinition task.Definition
	var executorCapability task.Capability
	taskConstructed := false
	executorConstructed := false
	served := false
	factories := elevatedOnceFactories{
		Inspector: inspector,
		FinalPath: func(path string) (string, error) { return path, nil },
		UserSID: func(identity process.Identity) (string, error) {
			sidIdentity = identity
			return parentSID, nil
		},
		NewTask: func(capability task.Capability) (task.Service, error) {
			taskConstructed = true
			gotCapability = capability
			return &windowsCompositionTask{}, nil
		},
		NewExecutor: func(helperConfig string, _ task.Service, definition task.Definition, capability task.Capability) (elevation.Handler, error) {
			executorConstructed = true
			gotHelperConfig = helperConfig
			gotDefinition = definition
			executorCapability = capability
			return elevatedCompositionHandler{}, nil
		},
		Serve: func(_ context.Context, pipe, nonce string, actualInspector process.Inspector, factory elevation.HandlerFactory) error {
			served = true
			if taskConstructed || executorConstructed {
				t.Fatal("elevated authority was constructed before parent validation")
			}
			if pipe != `\\.\pipe\mysingerserver-elevate-`+nonce || actualInspector == nil || factory == nil {
				return errors.New("invalid serve wiring")
			}
			handler, err := factory(parent)
			if err != nil || handler == nil {
				return errors.New("handler factory failed")
			}
			return nil
		},
	}

	if err := runElevatedOnceWith(`\\.\pipe\mysingerserver-elevate-`+elevatedCompositionNonce, elevatedCompositionNonce, factories); err != nil {
		t.Fatalf("runElevatedOnceWith: %v", err)
	}
	if !served || !process.SameProcess(parent, sidIdentity) {
		t.Fatalf("serve=%v SID identity=%#v", served, sidIdentity)
	}
	if gotCapability != task.CapabilityElevated || executorCapability != task.CapabilityElevated {
		t.Fatalf("task capability=%v executor capability=%v", gotCapability, executorCapability)
	}
	wantDefinition := task.Definition{HelperExecutable: layout.HelperExecutable, HelperConfig: layout.HelperConfig, UserSID: parentSID}
	if gotHelperConfig != layout.HelperConfig || gotDefinition != wantDefinition {
		t.Fatalf("helper config=%q definition=%#v", gotHelperConfig, gotDefinition)
	}
}

func TestElevatedEntryDerivesHelperPathsFromItsPortableTrayExecutable(t *testing.T) {
	self := process.Identity{PID: os.Getpid(), StartedAtUnixMS: 100, ExecutablePath: `D:\便携 工具\Compute\nodetray.exe`}
	parent := process.Identity{PID: 3131, StartedAtUnixMS: 200, ExecutablePath: self.ExecutablePath}
	inspector := elevatedCompositionInspector{identities: map[int]process.Identity{self.PID: self, parent.PID: parent}}
	var helperConfig string
	var definition task.Definition
	factories := elevatedOnceFactories{
		Inspector: inspector, FinalPath: func(path string) (string, error) { return path, nil },
		UserSID: func(process.Identity) (string, error) { return "S-1-5-21-101-202-303-1001", nil },
		NewTask: func(task.Capability) (task.Service, error) { return &windowsCompositionTask{}, nil },
		NewExecutor: func(config string, _ task.Service, got task.Definition, _ task.Capability) (elevation.Handler, error) {
			helperConfig, definition = config, got
			return elevatedCompositionHandler{}, nil
		},
		Serve: func(_ context.Context, _ string, _ string, _ process.Inspector, factory elevation.HandlerFactory) error {
			_, err := factory(parent)
			return err
		},
	}
	if err := runElevatedOnceWith(`\\.\pipe\mysingerserver-elevate-`+elevatedCompositionNonce, elevatedCompositionNonce, factories); err != nil {
		t.Fatalf("runElevatedOnceWith: %v", err)
	}
	wantConfig := `D:\便携 工具\Compute\data\helper\helper.json`
	wantDefinition := task.Definition{HelperExecutable: `D:\便携 工具\Compute\helper.exe`, HelperConfig: wantConfig, UserSID: "S-1-5-21-101-202-303-1001"}
	if helperConfig != wantConfig || definition != wantDefinition {
		t.Fatalf("portable helper authority config=%q definition=%#v", helperConfig, definition)
	}
}

func TestElevatedOnceRejectsInvalidPortableExecutableBeforeServing(t *testing.T) {
	self := process.Identity{PID: os.Getpid(), StartedAtUnixMS: 100, ExecutablePath: `C:\Temp\other.exe`}
	serveCalls := 0
	factories := elevatedOnceFactories{
		Inspector: elevatedCompositionInspector{identities: map[int]process.Identity{self.PID: self}},
		FinalPath: func(path string) (string, error) { return path, nil },
		UserSID:   func(process.Identity) (string, error) { return "", errors.New("not called") },
		NewTask:   func(task.Capability) (task.Service, error) { return nil, errors.New("not called") },
		NewExecutor: func(string, task.Service, task.Definition, task.Capability) (elevation.Handler, error) {
			return nil, errors.New("not called")
		},
		Serve: func(context.Context, string, string, process.Inspector, elevation.HandlerFactory) error {
			serveCalls++
			return nil
		},
	}
	if err := runElevatedOnceWith(`\\.\pipe\mysingerserver-elevate-`+elevatedCompositionNonce, elevatedCompositionNonce, factories); err == nil || serveCalls != 0 {
		t.Fatalf("outside executable err=%v serve calls=%d", err, serveCalls)
	}
}
