//go:build windows && !bindings

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentconfig "dedup/internal/config"
	"dedup/internal/machineid"
	"dedup/internal/nodectl"
	trayapp "dedup/internal/nodetray/app"
	trayconfig "dedup/internal/nodetray/config"
	"dedup/internal/nodetray/process"
	"dedup/internal/nodetray/production"
	"dedup/internal/nodetray/supervisor"
	"dedup/internal/nodetray/traymodel"
	"dedup/internal/nodetray/windows/task"
	"dedup/internal/proto"
)

type windowsCompositionStore struct {
	compositionStore
	ensureCalls int
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
func (inspectedWindowsCompositionInspector) InspectHandle(uintptr) (process.Identity, error) {
	return process.Identity{}, errors.New("not called")
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
	machineID := "node-" + strings.Repeat("1", 64)
	inspector := inspectedWindowsCompositionInspector{self: self}
	var storePaths trayconfig.Paths
	var loginExecutable, elevationExecutable string
	var taskCapability task.Capability
	var agentInspector process.Inspector
	var helperInspector process.HandleInspector
	backend, err := composeWindowsProductionBackendWith(windowsProductionCompositionDependencies{
		MachineIdentity: func() (machineid.Result, error) { return machineid.Result{ID: machineID}, nil },
		Inspector:       inspector,
		FinalPath:       func(path string) (string, error) { return path, nil },
		UserSID:         func(process.Identity) (string, error) { return "S-1-5-21-101-202-303-1001", nil },
		AgentConnectionSource: func(context.Context) (string, string, error) {
			return "127.0.0.1:1", "composition-token", nil
		},
		Constructors: windowsProductionConstructors{
			NewStore: func(paths trayconfig.Paths) (windowsProductionStore, error) { storePaths = paths; return store, nil },
			NewTask: func(capability task.Capability) (task.Service, error) {
				taskCapability = capability
				return taskService, nil
			},
			NewLoginStart: func(executable string) (trayapp.LoginStart, error) { loginExecutable = executable; return login, nil },
			NewElevation: func(executable string, gotInspector process.Inspector) (trayapp.ElevationClient, error) {
				elevationExecutable = executable
				if gotInspector != inspector {
					t.Fatal("elevation inspector differs")
				}
				return compositionElevation{}, nil
			},
			NewAgentLauncher: func(gotInspector process.Inspector) supervisor.Launcher {
				agentInspector = gotInspector
				return &windowsCompositionLauncher{}
			},
			NewHelperLauncher: func(gotInspector process.HandleInspector) supervisor.Launcher {
				helperInspector = gotInspector
				return &windowsCompositionLauncher{}
			},
		},
	})
	if err != nil {
		t.Fatalf("composeWindowsProductionBackendWith: %v", err)
	}
	if backend == nil || backend.service == nil || backend.webViewDataPath != `D:\便携 工具\Compute\data\nodetray\webview2` {
		t.Fatalf("backend=%#v", backend)
	}
	if storePaths != (trayconfig.Paths{TraySettings: `D:\便携 工具\Compute\data\nodetray\tray.json`, AgentConfig: `D:\便携 工具\Compute\data\agent\agent.json`, HelperConfig: `D:\便携 工具\Compute\data\helper\helper.json`, AgentExecutable: `D:\便携 工具\Compute\agent.exe`, HelperExecutable: `D:\便携 工具\Compute\helper.exe`}) {
		t.Fatalf("store paths = %#v", storePaths)
	}
	if loginExecutable != self.ExecutablePath || elevationExecutable != self.ExecutablePath || taskCapability != task.CapabilityUser || agentInspector != inspector || helperInspector != inspector {
		t.Fatalf("constructor inputs login=%q elevation=%q task=%v agent=%T helper=%T", loginExecutable, elevationExecutable, taskCapability, agentInspector, helperInspector)
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
		AgentConnectionSource: func(context.Context) (string, string, error) {
			return "127.0.0.1:1", "construction-token", nil
		},
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

func TestWindowsProductionInputsShareAndCloseOneAgentSocketController(t *testing.T) {
	layout, err := production.ResolvePortableLayout(`D:\Portable\Compute\nodetray.exe`)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	machineID := "node-" + strings.Repeat("7", 64)
	token := "shared-controller-token"
	base := agentconfig.DefaultAgent()
	base.ListenAddr = listener.Addr().String()
	base.DataDir = `D:\Portable\Compute\data\agent\data`
	base.PGDSN = "postgres://127.0.0.1:5432/dedup?sslmode=prefer"
	base.Worker.Count = 2
	base.Worker.ExePath = layout.AgentExecutable
	base.Thumb.CacheDir = `D:\Portable\Compute\data\agent\thumbcache`
	canonical, err := json.MarshalIndent(base, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	canonical = append(canonical, '\n')
	status := nodectl.Status{
		Component: nodectl.ComponentAgent, MachineID: machineID, PID: 1001,
		StartedAtUnixMS: 123, ExecutablePath: layout.AgentExecutable,
		ConfigSHA256: strings.Repeat("a", 64), Lifecycle: "running",
		ServiceReady: true, Ready: true, SyncHealthy: true,
	}
	var connections atomic.Int32
	operations := make(chan string, 4)
	closed := make(chan struct{}, 4)
	serverErr := make(chan error, 4)
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			connections.Add(1)
			go func(conn net.Conn) {
				defer func() { _ = conn.Close(); closed <- struct{}{} }()
				framed := proto.NewConn(conn)
				if writeErr := framed.WriteFrame(proto.MsgHello, &proto.Hello{Version: proto.ProtocolVersion, MachineID: machineID, PID: 1001}); writeErr != nil {
					serverErr <- writeErr
					return
				}
				messageType, body, readErr := framed.ReadFrame()
				if readErr != nil {
					serverErr <- readErr
					return
				}
				decoded, decodeErr := proto.Decode(messageType, body)
				if decodeErr != nil {
					serverErr <- decodeErr
					return
				}
				auth, ok := decoded.(*proto.ClientAuth)
				if !ok || auth.Token != token {
					serverErr <- errors.New("unexpected Agent authentication")
					return
				}
				if writeErr := framed.WriteFrame(proto.MsgClientAuthResult, &proto.ClientAuthResult{Accepted: true}); writeErr != nil {
					serverErr <- writeErr
					return
				}
				for {
					messageType, body, readErr = framed.ReadFrame()
					if readErr != nil {
						return
					}
					decoded, decodeErr = proto.Decode(messageType, body)
					if decodeErr != nil {
						serverErr <- decodeErr
						return
					}
					request := decoded.(*proto.LocalRequest)
					operations <- request.Operation
					var response proto.LocalResponse
					switch request.Operation {
					case proto.LocalOperationConfigGet:
						response = windowsLocalSuccess(request.RequestID, proto.LocalConfigGetResponse{CanonicalJSON: canonical, SHA256: strings.Repeat("a", 64)})
					case proto.LocalOperationStatusGet:
						response = windowsLocalSuccess(request.RequestID, proto.LocalStatusGetResponse{Status: status})
					default:
						response = proto.LocalResponse{RequestID: request.RequestID, ErrorCode: "unsupported_operation"}
					}
					if writeErr := framed.WriteFrame(proto.MsgLocalResponse, &response); writeErr != nil {
						serverErr <- writeErr
						return
					}
				}
			}(connection)
		}
	}()

	store := &windowsCompositionStore{}
	native := windowsProductionNative{
		Store: store, Inspector: windowsCompositionInspector{},
		AgentLauncher: &windowsCompositionLauncher{}, HelperLauncher: &windowsCompositionLauncher{},
		Terminator: &windowsCompositionTerminator{}, Dialer: &windowsCompositionDialer{}, MachineID: machineID,
		Task: &windowsCompositionTask{}, Elevation: compositionElevation{}, LoginStart: compositionLoginStart{},
		Instance: compositionInstance{}, UI: compositionUI{}, Opener: compositionOpener{},
		Emitter: func(context.Context, string, any) {}, Show: func(context.Context) {}, Quit: func(context.Context) {},
		AgentConnectionSource: func(context.Context) (string, string, error) { return listener.Addr().String(), token, nil },
	}
	inputs, err := buildWindowsProductionInputs(layout, "S-1-5-21-101-202-303-1001", native)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inputs.AgentConfig.LoadAgentForm(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := inputs.Workers.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case <-operations:
		case err := <-serverErr:
			t.Fatal(err)
		case <-time.After(time.Second):
			t.Fatal("Agent Socket operation timed out")
		}
	}
	if got := connections.Load(); got != 1 {
		t.Fatalf("Agent Socket connections = %d, want one shared controller", got)
	}
	if err := inputs.CloseAgentControl(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case err := <-serverErr:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("shared Agent Socket connection was not closed")
	}
}

func windowsLocalSuccess(requestID string, value any) proto.LocalResponse {
	payload, err := proto.EncodeLocalPayload(value)
	if err != nil {
		panic(err)
	}
	return proto.LocalResponse{RequestID: requestID, OK: true, Payload: payload}
}

var _ supervisor.Launcher = (*windowsCompositionLauncher)(nil)
var _ supervisor.Terminator = (*windowsCompositionTerminator)(nil)
var _ production.Dialer = (*windowsCompositionDialer)(nil)
var _ task.Service = (*windowsCompositionTask)(nil)
