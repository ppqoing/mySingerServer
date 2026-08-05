//go:build windows && !bindings

package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	trayapp "dedup/internal/nodetray/app"
	trayconfig "dedup/internal/nodetray/config"
	"dedup/internal/nodetray/process"
	"dedup/internal/nodetray/production"
	"dedup/internal/nodetray/supervisor"
	"dedup/internal/nodetray/traymodel"
	"dedup/internal/nodetray/windows/task"
	"golang.org/x/sys/windows"
)

type windowsCompositionStore struct {
	compositionStore
	ensureCalls int
}

func (*windowsCompositionStore) ValidateAgentForm(trayconfig.AgentForm) []trayconfig.FieldError {
	return nil
}
func (*windowsCompositionStore) ValidateHelperForm(trayconfig.HelperForm) []trayconfig.FieldError {
	return nil
}
func (*windowsCompositionStore) AgentFingerprint() (string, error) {
	return "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil
}
func (*windowsCompositionStore) HelperFingerprint() (string, error) {
	return "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", nil
}
func (s *windowsCompositionStore) EnsureTraySettings(traymodel.TraySettings) error {
	s.ensureCalls++
	return nil
}

type windowsCompositionInspector struct{}

func (windowsCompositionInspector) Inspect(int) (process.Identity, error) {
	return process.Identity{}, errors.New("not called")
}
func (windowsCompositionInspector) Wait(context.Context, process.Identity) (int, error) {
	return 0, errors.New("not called")
}

type windowsCompositionLauncher struct{ calls int }

func (l *windowsCompositionLauncher) Start(context.Context, string, []string, []string) (process.Identity, error) {
	l.calls++
	return process.Identity{}, errors.New("unexpected launch")
}

type windowsCompositionTerminator struct{ calls int }

func (t *windowsCompositionTerminator) Terminate(process.Identity, uint32) error {
	t.calls++
	return errors.New("unexpected terminate")
}

type windowsCompositionDialer struct{ calls int }

func (d *windowsCompositionDialer) Dial(context.Context, string) (net.Conn, error) {
	d.calls++
	return nil, errors.New("unexpected dial")
}

type windowsCompositionTask struct{ calls int }

func (t *windowsCompositionTask) Inspect(context.Context) (task.Status, error) {
	t.calls++
	return task.Status{}, nil
}
func (t *windowsCompositionTask) Install(context.Context, task.Definition) error {
	t.calls++
	return nil
}
func (t *windowsCompositionTask) Remove(context.Context) error { t.calls++; return nil }
func (t *windowsCompositionTask) Run(context.Context) error    { t.calls++; return nil }
func (t *windowsCompositionTask) Stop(context.Context) error   { t.calls++; return nil }

func TestResolveWindowsLayoutUsesKnownFoldersOnly(t *testing.T) {
	called := make(map[*windows.KNOWNFOLDERID]int)
	lookup := func(id *windows.KNOWNFOLDERID, _ uint32) (string, error) {
		called[id]++
		switch id {
		case windows.FOLDERID_ProgramFiles:
			return `C:\Program Files`, nil
		case windows.FOLDERID_ProgramData:
			return `C:\ProgramData`, nil
		case windows.FOLDERID_LocalAppData:
			return `C:\Users\u\AppData\Local`, nil
		default:
			return "", errors.New("unexpected known folder")
		}
	}

	layout, err := resolveWindowsLayout(lookup)
	if err != nil {
		t.Fatalf("resolveWindowsLayout: %v", err)
	}
	if layout.TrayExecutable != `C:\Program Files\MySingerServer\nodetray.exe` ||
		layout.AgentConfig != `C:\ProgramData\MySingerServer\Node\agent.json` ||
		layout.TraySettings != `C:\Users\u\AppData\Local\MySingerServer\NodeTray\tray.json` {
		t.Fatalf("unexpected fixed layout: %#v", layout)
	}
	if len(called) != 3 {
		t.Fatalf("known folder calls=%v", called)
	}
}

func TestWindowsProductionInputsShareFactoryAndPerformNoActionsDuringConstruction(t *testing.T) {
	layout, err := production.ResolveLayout(`C:\Program Files`, `C:\ProgramData`, `C:\Users\u\AppData\Local`)
	if err != nil {
		t.Fatalf("ResolveLayout: %v", err)
	}
	store := &windowsCompositionStore{}
	agentLauncher := &windowsCompositionLauncher{}
	helperLauncher := &windowsCompositionLauncher{}
	terminator := &windowsCompositionTerminator{}
	dialer := &windowsCompositionDialer{}
	taskService := &windowsCompositionTask{}
	native := windowsProductionNative{
		Store: store, Inspector: windowsCompositionInspector{},
		AgentLauncher: agentLauncher, HelperLauncher: helperLauncher, Terminator: terminator,
		Dialer: dialer, MachineID: "node-" + strings.Repeat("1", 64),
		Task: taskService, Elevation: compositionElevation{}, LoginStart: compositionLoginStart{},
		Instance: compositionInstance{}, UI: compositionUI{}, Opener: compositionOpener{},
		Emitter: func(context.Context, string, any) {}, Show: func(context.Context) {}, Quit: func(context.Context) {},
	}

	inputs, err := buildWindowsProductionInputs(layout, "S-1-5-21-101-202-303-1001", native)
	if err != nil {
		t.Fatalf("buildWindowsProductionInputs: %v", err)
	}
	factory, ok := inputs.Factory.(*production.Factory)
	if !ok || inputs.Agent != factory.Agent() || inputs.Helper != factory.Helper() {
		t.Fatal("app components do not share the bootstrap factory instances")
	}
	if inputs.MachineID != native.MachineID || inputs.AgentFingerprint != factory.Agent() || inputs.HelperFingerprint != factory.Helper() {
		t.Fatal("machine identity or configuration updaters are not wired to the shared instances")
	}
	if store.ensureCalls != 0 || agentLauncher.calls != 0 || helperLauncher.calls != 0 || terminator.calls != 0 || dialer.calls != 0 || taskService.calls != 0 {
		t.Fatalf("construction performed actions ensure=%d agent=%d helper=%d terminate=%d dial=%d task=%d",
			store.ensureCalls, agentLauncher.calls, helperLauncher.calls, terminator.calls, dialer.calls, taskService.calls)
	}
	if inputs.TaskDefinition != (task.Definition{HelperExecutable: layout.HelperExecutable, HelperConfig: layout.HelperConfig, UserSID: "S-1-5-21-101-202-303-1001"}) {
		t.Fatalf("task authority=%#v", inputs.TaskDefinition)
	}
	wantLocations := map[traymodel.LocationKind]trayapp.Location{
		traymodel.AgentLogs:    {Path: layout.AgentLogs, Root: `C:\ProgramData\MySingerServer\Node`},
		traymodel.HelperLogs:   {Path: layout.HelperLogs, Root: `C:\ProgramData\MySingerServer\Helper`},
		traymodel.AgentBackup:  {Path: layout.AgentConfig + ".last-good", Root: `C:\ProgramData\MySingerServer\Node`},
		traymodel.HelperBackup: {Path: layout.HelperConfig + ".last-good", Root: `C:\ProgramData\MySingerServer\Helper`},
	}
	for kind, want := range wantLocations {
		if got := inputs.Locations[kind]; got != want {
			t.Fatalf("location %s=%#v want %#v", kind, got, want)
		}
	}
	if err := inputs.Prepare(); err != nil || store.ensureCalls != 1 {
		t.Fatalf("Prepare err=%v calls=%d", err, store.ensureCalls)
	}
}

var _ supervisor.Launcher = (*windowsCompositionLauncher)(nil)
var _ supervisor.Terminator = (*windowsCompositionTerminator)(nil)
var _ production.Dialer = (*windowsCompositionDialer)(nil)
var _ task.Service = (*windowsCompositionTask)(nil)
