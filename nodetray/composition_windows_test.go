//go:build windows && !bindings

package main

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"

	"dedup/internal/machineid"
	trayapp "dedup/internal/nodetray/app"
	trayconfig "dedup/internal/nodetray/config"
	"dedup/internal/nodetray/process"
	"dedup/internal/nodetray/production"
	"dedup/internal/nodetray/supervisor"
	"dedup/internal/nodetray/traymodel"
	"dedup/internal/nodetray/windows/task"
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

type inspectedWindowsCompositionInspector struct{ self process.Identity }

func (i inspectedWindowsCompositionInspector) Inspect(pid int) (process.Identity, error) {
	if pid != os.Getpid() {
		return process.Identity{}, errors.New("unexpected process inspection")
	}
	return i.self, nil
}
func (inspectedWindowsCompositionInspector) Wait(context.Context, process.Identity) (int, error) {
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

func TestWindowsProductionCompositionUsesInspectedPortableExecutable(t *testing.T) {
	self := process.Identity{PID: 4242, StartedAtUnixMS: 100, ExecutablePath: `D:\便携 工具\Compute\nodetray.exe`}
	store := &windowsCompositionStore{}
	taskService := &windowsCompositionTask{}
	login := compositionLoginStart{}
	native := windowsProductionNative{
		Store: store, Inspector: windowsCompositionInspector{},
		AgentLauncher: &windowsCompositionLauncher{}, HelperLauncher: &windowsCompositionLauncher{}, Terminator: &windowsCompositionTerminator{},
		Dialer: &windowsCompositionDialer{}, MachineID: "node-" + strings.Repeat("1", 64),
		Task: taskService, Elevation: compositionElevation{}, LoginStart: login,
		Instance: compositionInstance{}, UI: compositionUI{}, Opener: compositionOpener{},
		Emitter: func(context.Context, string, any) {}, Show: func(context.Context) {}, Quit: func(context.Context) {},
	}
	inspector := inspectedWindowsCompositionInspector{self: self}
	var inputs productionCompositionInputs
	backend, err := composeWindowsProductionBackendWith(windowsProductionCompositionDependencies{
		MachineIdentity: func() (machineid.Result, error) { return machineid.Result{ID: native.MachineID}, nil },
		Inspector:       inspector,
		FinalPath:       func(path string) (string, error) { return path, nil },
		UserSID:         func(process.Identity) (string, error) { return "S-1-5-21-101-202-303-1001", nil },
		BuildInputs: func(layout production.Layout, userSID string, gotInspector process.Inspector, identity machineid.Result) (productionCompositionInputs, error) {
			if layout.TrayExecutable != self.ExecutablePath || gotInspector != inspector || identity.ID != native.MachineID {
				t.Fatalf("entry dependencies layout=%#v inspector=%T identity=%#v", layout, gotInspector, identity)
			}
			var err error
			inputs, err = buildWindowsProductionInputs(layout, userSID, native)
			return inputs, err
		},
	})
	if err != nil {
		t.Fatalf("composeWindowsProductionBackendWith: %v", err)
	}
	if backend == nil || backend.service == nil || backend.webViewDataPath != `D:\便携 工具\Compute\data\nodetray\webview2` {
		t.Fatalf("backend=%#v", backend)
	}
	if inputs.PortableRoot != `D:\便携 工具\Compute` || inputs.WebViewDataPath != `D:\便携 工具\Compute\data\nodetray\webview2` {
		t.Fatalf("portable inputs = root %q webview %q", inputs.PortableRoot, inputs.WebViewDataPath)
	}
	if inputs.TrayExecutable != self.ExecutablePath || inputs.TaskDefinition.HelperExecutable != `D:\便携 工具\Compute\helper.exe` || inputs.TaskDefinition.HelperConfig != `D:\便携 工具\Compute\data\helper\helper.json` {
		t.Fatalf("portable executable authority = tray %q task %#v", inputs.TrayExecutable, inputs.TaskDefinition)
	}
	if inputs.Store != store || inputs.Task != taskService || inputs.LoginStart != login || inputs.Agent == nil || inputs.Helper == nil {
		t.Fatalf("portable composition components store=%T agent=%T helper=%T login=%T task=%T", inputs.Store, inputs.Agent, inputs.Helper, inputs.LoginStart, inputs.Task)
	}
	paths, err := inputs.Paths.Resolve(context.Background())
	if err != nil || paths.TraySettings != `D:\便携 工具\Compute\data\nodetray\tray.json` || paths.AgentConfig != `D:\便携 工具\Compute\data\agent\agent.json` || paths.HelperConfig != `D:\便携 工具\Compute\data\helper\helper.json` {
		t.Fatalf("portable store paths = %#v err=%v", paths, err)
	}
	for kind, location := range inputs.Locations {
		if !strings.HasPrefix(location.Path, `D:\便携 工具\Compute\data\`) || !strings.HasPrefix(location.Root, `D:\便携 工具\Compute\data\`) {
			t.Fatalf("location %s escaped portable root: %#v", kind, location)
		}
	}
}

func TestWindowsProductionInputsShareFactoryAndPerformNoActionsDuringConstruction(t *testing.T) {
	layout, err := production.ResolvePortableLayout(`D:\便携 工具\Compute\nodetray.exe`)
	if err != nil {
		t.Fatalf("ResolvePortableLayout: %v", err)
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
		traymodel.AgentLogs:    {Path: layout.AgentLogs, Root: `D:\便携 工具\Compute\data\agent`},
		traymodel.HelperLogs:   {Path: layout.HelperLogs, Root: `D:\便携 工具\Compute\data\helper`},
		traymodel.AgentBackup:  {Path: layout.AgentConfig + ".last-good", Root: `D:\便携 工具\Compute\data\agent`},
		traymodel.HelperBackup: {Path: layout.HelperConfig + ".last-good", Root: `D:\便携 工具\Compute\data\helper`},
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
