package production

import (
	"context"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"

	"dedup/internal/nodectl"
	trayconfig "dedup/internal/nodetray/config"
	"dedup/internal/nodetray/traymodel"
)

type fakeValidationStore struct {
	agent  []trayconfig.FieldError
	helper []trayconfig.FieldError
}

func (f fakeValidationStore) ValidateAgentForm(trayconfig.AgentForm) []trayconfig.FieldError {
	return append([]trayconfig.FieldError(nil), f.agent...)
}
func (f fakeValidationStore) ValidateHelperForm(trayconfig.HelperForm) []trayconfig.FieldError {
	return append([]trayconfig.FieldError(nil), f.helper...)
}

func TestValidatorDelegatesToStoreSharedPureValidation(t *testing.T) {
	wantAgent := []trayconfig.FieldError{{Field: "agent", Code: "invalid", Message: "Agent 配置无效"}}
	wantHelper := []trayconfig.FieldError{{Field: "helper", Code: "invalid", Message: "Helper 配置无效"}}
	validator := NewValidator(fakeValidationStore{agent: wantAgent, helper: wantHelper})
	if got := validator.ValidateAgent(trayconfig.AgentForm{}); !reflect.DeepEqual(got, wantAgent) {
		t.Fatalf("ValidateAgent = %#v", got)
	}
	if got := validator.ValidateHelper(trayconfig.HelperForm{}); !reflect.DeepEqual(got, wantHelper) {
		t.Fatalf("ValidateHelper = %#v", got)
	}
}

type scriptedDialer struct {
	mu       sync.Mutex
	statuses []nodectl.Status
	names    []string
	commands []nodectl.Command
}

func (d *scriptedDialer) Dial(_ context.Context, name string) (net.Conn, error) {
	d.mu.Lock()
	d.names = append(d.names, name)
	d.mu.Unlock()
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		var request nodectl.Request
		if err := nodectl.ReadFrame(server, &request); err != nil {
			return
		}
		d.mu.Lock()
		d.commands = append(d.commands, request.Command)
		response := nodectl.Response{Version: nodectl.ProtocolVersion, RequestID: request.RequestID, OK: true}
		if request.Command == nodectl.CommandStatus && len(d.statuses) != 0 {
			status := d.statuses[0]
			d.statuses = d.statuses[1:]
			response.Status = &status
		}
		d.mu.Unlock()
		_ = nodectl.WriteFrame(server, response)
	}()
	return client, nil
}

func controllerMachineID(fill string) string {
	return "node-" + strings.Repeat(fill, 64)
}

func validAgentControlStatus() nodectl.Status {
	return nodectl.Status{
		Component: nodectl.ComponentAgent, MachineID: controllerMachineID("1"), PID: 1001,
		StartedAtUnixMS: 123456, ExecutablePath: `C:\Program Files\MySingerServer\agent.exe`,
		ConfigSHA256: strings.Repeat("a", 64), Lifecycle: "running", ServiceReady: true, Ready: true,
		SyncHealthy: true,
	}
}

func TestControllerFreezesAgentPipeAndValidatesStatusIdentity(t *testing.T) {
	dialer := &scriptedDialer{statuses: []nodectl.Status{validAgentControlStatus()}}
	controller, err := NewAgentController(dialer, controllerMachineID("1"))
	if err != nil {
		t.Fatalf("NewAgentController: %v", err)
	}
	status, err := controller.Status(context.Background())
	if err != nil || status.MachineID != controllerMachineID("1") {
		t.Fatalf("Status = %#v, %v", status, err)
	}
	if err := controller.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	dialer.mu.Lock()
	defer dialer.mu.Unlock()
	if !reflect.DeepEqual(dialer.names, []string{nodectl.AgentPipeName(), nodectl.AgentPipeName()}) {
		t.Fatalf("dialed pipes = %v", dialer.names)
	}
	if !reflect.DeepEqual(dialer.commands, []nodectl.Command{nodectl.CommandStatus, nodectl.CommandShutdown}) {
		t.Fatalf("commands = %v", dialer.commands)
	}
}

func TestAgentControllerRejectsEveryDifferentReportedMachineID(t *testing.T) {
	status := validAgentControlStatus()
	status.MachineID = controllerMachineID("2")
	controller, err := NewAgentController(
		&scriptedDialer{statuses: []nodectl.Status{status}},
		controllerMachineID("1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Status(context.Background()); err == nil {
		t.Fatal("controller accepted a different generated machine ID")
	}
}

func TestControllerRejectsInvalidOrMismatchedStatusWithoutLeakingResponse(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*nodectl.Status)
	}{
		{name: "protocol invalid", mutate: func(s *nodectl.Status) { s.PID = -1; s.LastErrorSummary = "password=fixture-secret" }},
		{name: "wrong component", mutate: func(s *nodectl.Status) { s.Component = nodectl.ComponentHelper }},
		{name: "wrong machine", mutate: func(s *nodectl.Status) { s.MachineID = "private-machine-name" }},
		{name: "empty current sha", mutate: func(s *nodectl.Status) { s.ConfigSHA256 = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := validAgentControlStatus()
			tt.mutate(&status)
			dialer := &scriptedDialer{statuses: []nodectl.Status{status}}
			controller, err := NewAgentController(dialer, controllerMachineID("1"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = controller.Status(context.Background())
			if err == nil {
				t.Fatal("Status accepted invalid identity")
			}
			for _, forbidden := range []string{"fixture-secret", "private-machine-name", `C:\Program Files`} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("Status error leaked %q: %v", forbidden, err)
				}
			}
		})
	}
}

func TestHelperControllerUsesInjectedMachineIDAndHelperPipe(t *testing.T) {
	status := validAgentControlStatus()
	status.Component = nodectl.ComponentHelper
	status.MachineID = controllerMachineID("1")
	status.SyncHealthy = false
	dialer := &scriptedDialer{statuses: []nodectl.Status{status}}
	controller, err := NewHelperController(dialer, controllerMachineID("1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(dialer.names, []string{nodectl.HelperPipeName()}) {
		t.Fatalf("pipes=%v", dialer.names)
	}
}

type fakeStatusController struct {
	status nodectl.Status
	err    error
	calls  int
}

func (f *fakeStatusController) Status(context.Context) (nodectl.Status, error) {
	f.calls++
	return f.status, f.err
}

func TestWorkerProviderOnlyReadsAgentStatusAndSanitizesSummaries(t *testing.T) {
	controller := &fakeStatusController{status: nodectl.Status{Component: nodectl.ComponentAgent, Workers: []nodectl.WorkerStatus{{
		Index: 2, PID: 2002, Ready: true, CurrentTaskSummary: `D:\media\private\clip.mp4`, LastErrorSummary: "postgres://u:p@db/media",
	}}}}
	provider := NewWorkerProvider(controller)
	workers, err := provider.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []traymodel.WorkerState{{Index: 2, PID: 2002, Ready: true, CurrentTaskSummary: "[REDACTED_PATH]", LastErrorSummary: "[REDACTED_URI]"}}
	if controller.calls != 1 || !reflect.DeepEqual(workers, want) {
		t.Fatalf("calls=%d workers=%#v", controller.calls, workers)
	}
	controller.status.Component = nodectl.ComponentHelper
	if _, err := provider.Snapshot(context.Background()); err == nil {
		t.Fatal("WorkerProvider accepted Helper status")
	}
}

type fakeExplorerBackend struct {
	executable string
	args       []string
	err        error
}

func (f *fakeExplorerBackend) Start(_ context.Context, executable string, args []string) error {
	f.executable = executable
	f.args = append([]string(nil), args...)
	return f.err
}

func TestLocationOpenerUsesFixedExplorerWithOneValidatedArgument(t *testing.T) {
	backend := &fakeExplorerBackend{}
	opener := NewLocationOpener(backend)
	path := `C:\ProgramData\MySingerServer\Node\logs`
	if err := opener.Open(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if backend.executable != "explorer.exe" || !reflect.DeepEqual(backend.args, []string{path}) {
		t.Fatalf("executable=%q args=%v", backend.executable, backend.args)
	}
	backend.executable, backend.args = "", nil
	if err := opener.Open(context.Background(), `relative\logs`); err == nil {
		t.Fatal("relative location accepted")
	}
	if backend.executable != "" || backend.args != nil {
		t.Fatal("invalid location reached Explorer backend")
	}
}

func TestAdaptersFailClosedWhenDependenciesAreUnavailable(t *testing.T) {
	if _, err := NewAgentController(nil, controllerMachineID("1")); err == nil {
		t.Fatal("nil dialer accepted")
	}
	if _, err := NewHelperController(&scriptedDialer{}, ""); err == nil {
		t.Fatal("empty generated identity accepted")
	}
	if _, err := NewWorkerProvider(nil).Snapshot(context.Background()); err == nil {
		t.Fatal("nil worker controller accepted")
	}
}
