package agentclient

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/vmihailenco/msgpack/v5"

	agentconfig "dedup/internal/config"
	"dedup/internal/nodectl"
	"dedup/internal/proto"
)

func TestAgentControllerUsesLoopbackLocalOperationsAndValidatesStatus(t *testing.T) {
	machineID := testMachineID("3")
	token := "controller-token"
	status := validAgentStatus(machineID)
	listener := listenAgentClientTestTCP(t)
	operations := make(chan string, 2)
	serverErr := make(chan error, 1)
	go serveAgentControllerConnection(listener, token, machineID, 2, func(request proto.LocalRequest) proto.LocalResponse {
		operations <- request.Operation
		switch request.Operation {
		case proto.LocalOperationStatusGet:
			return localSuccess(request.RequestID, proto.LocalStatusGetResponse{Status: status})
		case proto.LocalOperationShutdown:
			return localSuccess(request.RequestID, proto.LocalShutdownResponse{Accepted: true})
		default:
			return proto.LocalResponse{RequestID: request.RequestID, ErrorCode: "unsupported_operation"}
		}
	}, serverErr)

	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewController(net.JoinHostPort("0.0.0.0", port), token, machineID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := controller.Status(context.Background())
	if err != nil || !reflect.DeepEqual(got, status) {
		t.Fatalf("Status = %#v, %v", got, err)
	}
	if err := controller.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := []string{<-operations, <-operations}; !reflect.DeepEqual(got, []string{proto.LocalOperationStatusGet, proto.LocalOperationShutdown}) {
		t.Fatalf("operations = %v", got)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("server: %v", err)
	}
}

func TestAgentControllerReconnectsAfterAgentDisconnected(t *testing.T) {
	machineID := testMachineID("4")
	token := "reconnect-token"
	status := validAgentStatus(machineID)
	listener := listenAgentClientTestTCP(t)
	serverErr := make(chan error, 1)
	go func() {
		first, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		if err := authenticateAgentClientTest(first, token, machineID); err != nil {
			_ = first.Close()
			serverErr <- err
			return
		}
		firstFramed := proto.NewConn(first)
		if _, _, err := firstFramed.ReadFrame(); err != nil {
			_ = first.Close()
			serverErr <- err
			return
		}
		_ = first.Close()

		second, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer second.Close()
		if err := authenticateAgentClientTest(second, token, machineID); err != nil {
			serverErr <- err
			return
		}
		framed := proto.NewConn(second)
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
		request := decoded.(*proto.LocalRequest)
		serverErr <- framed.WriteFrame(proto.MsgLocalResponse, localSuccess(request.RequestID, proto.LocalStatusGetResponse{Status: status}))
	}()

	controller, err := NewController(listener.Addr().String(), token, machineID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Status(context.Background()); !errors.Is(err, ErrAgentDisconnected) {
		t.Fatalf("first Status error = %v, want agent_disconnected", err)
	}
	got, err := controller.Status(context.Background())
	if err != nil || got.PID != status.PID {
		t.Fatalf("reconnected Status = %#v, %v", got, err)
	}
	_ = controller.Close()
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestAgentControllerConfigUsesCanonicalRawJSONAndPreservesStoredPassword(t *testing.T) {
	machineID := testMachineID("5")
	token := "config-token"
	listener := listenAgentClientTestTCP(t)
	base := validControllerAgentConfig()
	canonical, err := json.MarshalIndent(base, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	canonical = append(canonical, '\n')
	requests := make(chan proto.LocalRequest, 3)
	serverErr := make(chan error, 1)
	go serveAgentControllerConnection(listener, token, machineID, 3, func(request proto.LocalRequest) proto.LocalResponse {
		requests <- request
		switch request.Operation {
		case proto.LocalOperationConfigGet:
			return localSuccess(request.RequestID, proto.LocalConfigGetResponse{CanonicalJSON: canonical, SHA256: strings.Repeat("a", 64)})
		case proto.LocalOperationConfigValidate:
			return localSuccess(request.RequestID, proto.LocalConfigValidateResponse{Valid: true, SHA256: strings.Repeat("b", 64), RestartRequired: true})
		case proto.LocalOperationConfigSave:
			return localSuccess(request.RequestID, proto.LocalConfigSaveResponse{SHA256: strings.Repeat("b", 64), RestartRequired: true})
		default:
			return proto.LocalResponse{RequestID: request.RequestID, ErrorCode: "unsupported_operation"}
		}
	}, serverErr)

	controller, err := NewController(listener.Addr().String(), token, machineID)
	if err != nil {
		t.Fatal(err)
	}
	form, err := controller.LoadAgentForm(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if form.Database.Password != "" || !form.Database.PasswordStored {
		t.Fatalf("LoadAgentForm exposed or lost stored password state: %#v", form.Database)
	}
	form.ListenPort++
	if errors := controller.ValidateAgentForm(context.Background(), form); len(errors) != 0 {
		t.Fatalf("ValidateAgentForm = %#v", errors)
	}
	result, err := controller.SaveAgentForm(context.Background(), form)
	if err != nil || result.SHA256 != strings.Repeat("b", 64) || !result.RestartRequired {
		t.Fatalf("SaveAgentForm = %#v, %v", result, err)
	}

	getRequest := <-requests
	validateRequest := <-requests
	saveRequest := <-requests
	if getRequest.Operation != proto.LocalOperationConfigGet || validateRequest.Operation != proto.LocalOperationConfigValidate || saveRequest.Operation != proto.LocalOperationConfigSave {
		t.Fatalf("config operations = %q, %q, %q", getRequest.Operation, validateRequest.Operation, saveRequest.Operation)
	}
	for _, request := range []proto.LocalRequest{validateRequest, saveRequest} {
		var wire proto.LocalConfigRequest
		if err := msgpack.Unmarshal(request.Payload, &wire); err != nil {
			t.Fatal(err)
		}
		var cfg agentconfig.AgentConfig
		if err := json.Unmarshal(wire.CanonicalJSON, &cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.PGDSN != base.PGDSN || cfg.ListenAddr == base.ListenAddr || !strings.HasSuffix(string(wire.CanonicalJSON), "\n") {
			t.Fatalf("canonical config lost secret base or edit: listen=%q dsn=%q", cfg.ListenAddr, cfg.PGDSN)
		}
	}
	_ = controller.Close()
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestAgentControllerStagesSavedPortUntilOldShutdownThenReconnectsToNewPort(t *testing.T) {
	machineID := testMachineID("6")
	token := "endpoint-transition-token"
	oldListener := listenAgentClientTestTCP(t)
	newListener := listenAgentClientTestTCP(t)
	_, oldPort, err := net.SplitHostPort(oldListener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_, newPort, err := net.SplitHostPort(newListener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	base := validControllerAgentConfig()
	base.ListenAddr = net.JoinHostPort("0.0.0.0", oldPort)
	canonical, err := json.MarshalIndent(base, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	canonical = append(canonical, '\n')
	oldStatus := validAgentStatus(machineID)
	oldStatus.PID = 1001
	newStatus := validAgentStatus(machineID)
	newStatus.PID = 2002
	oldOperations := make(chan string, 4)
	oldServerErr := make(chan error, 1)
	go serveAgentControllerConnection(oldListener, token, machineID, 4, func(request proto.LocalRequest) proto.LocalResponse {
		oldOperations <- request.Operation
		switch request.Operation {
		case proto.LocalOperationConfigGet:
			return localSuccess(request.RequestID, proto.LocalConfigGetResponse{CanonicalJSON: canonical, SHA256: strings.Repeat("a", 64)})
		case proto.LocalOperationConfigSave:
			return localSuccess(request.RequestID, proto.LocalConfigSaveResponse{SHA256: strings.Repeat("b", 64), RestartRequired: true})
		case proto.LocalOperationStatusGet:
			return localSuccess(request.RequestID, proto.LocalStatusGetResponse{Status: oldStatus})
		case proto.LocalOperationShutdown:
			return localSuccess(request.RequestID, proto.LocalShutdownResponse{Accepted: true})
		default:
			return proto.LocalResponse{RequestID: request.RequestID, ErrorCode: "unsupported_operation"}
		}
	}, oldServerErr)
	newServerErr := make(chan error, 1)
	go serveAgentControllerConnection(newListener, token, machineID, 1, func(request proto.LocalRequest) proto.LocalResponse {
		if request.Operation != proto.LocalOperationStatusGet {
			return proto.LocalResponse{RequestID: request.RequestID, ErrorCode: "unsupported_operation"}
		}
		return localSuccess(request.RequestID, proto.LocalStatusGetResponse{Status: newStatus})
	}, newServerErr)

	controller, err := NewController(oldListener.Addr().String(), token, machineID)
	if err != nil {
		t.Fatal(err)
	}
	form, err := controller.LoadAgentForm(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	form.ListenPort = mustAgentClientPort(t, newPort)
	if _, err := controller.SaveAgentForm(context.Background(), form); err != nil {
		t.Fatal(err)
	}
	if status, err := controller.Status(context.Background()); err != nil || status.PID != oldStatus.PID {
		t.Fatalf("Status before shutdown = %#v, %v", status, err)
	}
	if err := controller.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status, err := controller.Status(context.Background()); err != nil || status.PID != newStatus.PID {
		t.Fatalf("Status after shutdown = %#v, %v", status, err)
	}
	_ = controller.Close()
	if got := []string{<-oldOperations, <-oldOperations, <-oldOperations, <-oldOperations}; !reflect.DeepEqual(got, []string{
		proto.LocalOperationConfigGet, proto.LocalOperationConfigSave, proto.LocalOperationStatusGet, proto.LocalOperationShutdown,
	}) {
		t.Fatalf("old endpoint operations = %v", got)
	}
	if err := <-oldServerErr; err != nil {
		t.Fatal(err)
	}
	if err := <-newServerErr; err != nil {
		t.Fatal(err)
	}
}

func mustAgentClientPort(t *testing.T, value string) int {
	t.Helper()
	port, err := strconv.Atoi(value)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func validAgentStatus(machineID string) nodectl.Status {
	return nodectl.Status{
		Component: nodectl.ComponentAgent, MachineID: machineID, PID: 1001,
		StartedAtUnixMS: 123456, ExecutablePath: `C:\Program Files\MySingerServer\agent.exe`,
		ConfigSHA256: strings.Repeat("a", 64), Lifecycle: "running", ServiceReady: true,
		Ready: true, SyncHealthy: true,
	}
}

func validControllerAgentConfig() *agentconfig.AgentConfig {
	cfg := agentconfig.DefaultAgent()
	cfg.ListenAddr = "0.0.0.0:9101"
	cfg.DataDir = `C:\ProgramData\MySingerServer\Node\data`
	cfg.PGDSN = "postgres://agent-user:stored-secret@127.0.0.1:5432/dedup?sslmode=prefer"
	cfg.Worker.Count = 4
	cfg.Worker.ExePath = `C:\Program Files\MySingerServer\worker.exe`
	cfg.Thumb.CacheDir = `C:\ProgramData\MySingerServer\Node\thumbcache`
	return cfg
}

func serveAgentControllerConnection(listener net.Listener, token, machineID string, requests int, handler func(proto.LocalRequest) proto.LocalResponse, result chan<- error) {
	conn, err := listener.Accept()
	if err != nil {
		result <- err
		return
	}
	defer conn.Close()
	if err := authenticateAgentClientTest(conn, token, machineID); err != nil {
		result <- err
		return
	}
	framed := proto.NewConn(conn)
	for range requests {
		messageType, body, err := framed.ReadFrame()
		if err != nil {
			result <- err
			return
		}
		decoded, err := proto.Decode(messageType, body)
		if err != nil {
			result <- err
			return
		}
		request, ok := decoded.(*proto.LocalRequest)
		if !ok {
			result <- errors.New("controller used a non-local request")
			return
		}
		response := handler(*request)
		if err := framed.WriteFrame(proto.MsgLocalResponse, &response); err != nil {
			result <- err
			return
		}
	}
	result <- nil
}

func authenticateAgentClientTest(conn net.Conn, token, machineID string) error {
	framed := proto.NewConn(conn)
	if err := framed.WriteFrame(proto.MsgHello, &proto.Hello{Version: proto.ProtocolVersion, MachineID: machineID, PID: 99}); err != nil {
		return err
	}
	messageType, body, err := framed.ReadFrame()
	if err != nil {
		return err
	}
	decoded, err := proto.Decode(messageType, body)
	if err != nil {
		return err
	}
	auth, ok := decoded.(*proto.ClientAuth)
	if !ok || auth.Role != "nodetray" || auth.Token != token || auth.Version != proto.ProtocolVersion {
		return errors.New("invalid controller authentication")
	}
	return framed.WriteFrame(proto.MsgClientAuthResult, &proto.ClientAuthResult{Accepted: true})
}

func localSuccess(requestID string, value any) proto.LocalResponse {
	payload, err := msgpack.Marshal(value)
	if err != nil {
		panic(err)
	}
	return proto.LocalResponse{RequestID: requestID, OK: true, Payload: payload}
}
