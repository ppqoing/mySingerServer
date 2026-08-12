package production

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"

	"dedup/internal/nodectl"
	trayconfig "dedup/internal/nodetray/config"
	"dedup/internal/nodetray/traymodel"
	"dedup/internal/proto"
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

func TestAgentControllerUsesLoopbackSocketAndNeverUsesHelperPipeDialer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	token := "production-agent-token"
	machineID := controllerMachineID("1")
	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		framed := proto.NewConn(conn)
		if err := framed.WriteFrame(proto.MsgHello, &proto.Hello{Version: proto.ProtocolVersion, MachineID: machineID, PID: 1001}); err != nil {
			serverErr <- err
			return
		}
		messageType, body, err := framed.ReadFrame()
		if err != nil {
			serverErr <- err
			return
		}
		decoded, err := proto.Decode(messageType, body)
		if err != nil {
			serverErr <- err
			return
		}
		auth, ok := decoded.(*proto.ClientAuth)
		if !ok || auth.Token != token || auth.Role != "nodetray" {
			serverErr <- errors.New("invalid Agent controller authentication")
			return
		}
		if err := framed.WriteFrame(proto.MsgClientAuthResult, &proto.ClientAuthResult{Accepted: true}); err != nil {
			serverErr <- err
			return
		}
		for _, operation := range []string{proto.LocalOperationStatusGet, proto.LocalOperationShutdown} {
			messageType, body, err = framed.ReadFrame()
			if err != nil {
				serverErr <- err
				return
			}
			decoded, err = proto.Decode(messageType, body)
			if err != nil {
				serverErr <- err
				return
			}
			request, ok := decoded.(*proto.LocalRequest)
			if !ok || request.Operation != operation {
				serverErr <- errors.New("Agent controller did not use local operation")
				return
			}
			var payload any = proto.LocalShutdownResponse{Accepted: true}
			if operation == proto.LocalOperationStatusGet {
				payload = proto.LocalStatusGetResponse{Status: validAgentControlStatus()}
			}
			encoded, err := proto.EncodeLocalPayload(payload)
			if err != nil {
				serverErr <- err
				return
			}
			if err := framed.WriteFrame(proto.MsgLocalResponse, &proto.LocalResponse{RequestID: request.RequestID, OK: true, Payload: encoded}); err != nil {
				serverErr <- err
				return
			}
		}
		serverErr <- nil
	}()

	pipeDialer := &scriptedDialer{}
	controller, err := NewAgentController(pipeDialer, machineID, func(context.Context) (string, string, error) {
		return net.JoinHostPort("0.0.0.0", port), token, nil
	})
	if err != nil {
		t.Fatalf("NewAgentController: %v", err)
	}
	status, err := controller.Status(context.Background())
	if err != nil || status.MachineID != machineID {
		t.Fatalf("Status = %#v, %v", status, err)
	}
	if err := controller.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	pipeDialer.mu.Lock()
	defer pipeDialer.mu.Unlock()
	if len(pipeDialer.names) != 0 {
		t.Fatalf("Agent controller dialed Helper pipe transport: %v", pipeDialer.names)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
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
	if _, err := NewAgentController(nil, ""); err == nil {
		t.Fatal("empty Agent identity accepted")
	}
	if _, err := NewHelperController(&scriptedDialer{}, ""); err == nil {
		t.Fatal("empty generated identity accepted")
	}
	if _, err := NewWorkerProvider(nil).Snapshot(context.Background()); err == nil {
		t.Fatal("nil worker controller accepted")
	}
}
