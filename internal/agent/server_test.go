package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dedup/internal/config"
	"dedup/internal/proto"
)

func TestServerHandshakePingAndUnsupportedMessageKeepConnectionAlive(t *testing.T) {
	cfg := config.DefaultAgent()
	cfg.MachineID = "machine-a"
	cfg.Proto.HeartbeatS = 60
	log := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	server := NewServer(cfg, scanHandlerFunc(func(
		task proto.ScanTask,
		sender Sender,
	) (proto.TaskAck, func()) {
		return proto.TaskAck{
			TaskID: task.TaskID, Accepted: true, Reason: "accepted", Total: -1,
		}, nil
	}), log)

	serverSide, clientSide := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go server.handleConn(ctx, serverSide)
	client := proto.NewConn(clientSide)
	defer client.Close()

	msgType, body, err := client.ReadFrame()
	if err != nil {
		t.Fatalf("read Hello: %v", err)
	}
	message, err := proto.Decode(msgType, body)
	if err != nil {
		t.Fatal(err)
	}
	hello, ok := message.(*proto.Hello)
	if !ok || hello.MachineID != "machine-a" ||
		hello.Version != proto.ProtocolVersion {
		t.Fatalf("Hello = %#v", message)
	}

	if err := client.WriteFrame(proto.MsgConfigPush, &proto.ConfigPush{
		KV: map[string]string{"future": "value"},
	}); err != nil {
		t.Fatal(err)
	}
	msgType, body, err = client.ReadFrame()
	if err != nil {
		t.Fatalf("read unsupported error: %v", err)
	}
	message, err = proto.Decode(msgType, body)
	if err != nil {
		t.Fatal(err)
	}
	protocolError, ok := message.(*proto.Error)
	if !ok || protocolError.Stage != "proto" {
		t.Fatalf("unsupported response = %#v", message)
	}

	if err := client.WriteFrame(proto.MsgPing, &proto.Ping{TS: 123}); err != nil {
		t.Fatal(err)
	}
	msgType, body, err = client.ReadFrame()
	if err != nil {
		t.Fatalf("connection closed after unsupported message: %v", err)
	}
	message, err = proto.Decode(msgType, body)
	if err != nil {
		t.Fatal(err)
	}
	pong, ok := message.(*proto.Pong)
	if !ok || pong.TS != 123 {
		t.Fatalf("Pong = %#v", message)
	}
}

func TestServerClientAuthAcceptsOnlyLoopbackNodeTrayWithCorrectToken(t *testing.T) {
	const token = "correct-local-control-token"
	server, logs := newLocalControlTestServer(t)
	server.SetLocalControl(token, localHandlerFunc(func(
		_ context.Context,
		request proto.LocalRequest,
	) proto.LocalResponse {
		return proto.LocalResponse{RequestID: request.RequestID, OK: true, Payload: []byte("ready")}
	}))

	wrongTokenClient, cleanupWrong := startLocalControlTestConnection(t, server, net.ParseIP("127.0.0.1"))
	defer cleanupWrong()
	writeLocalControlTestAuth(t, wrongTokenClient, proto.ClientAuth{Role: "nodetray", Token: "wrong-secret", Version: proto.ProtocolVersion})
	wrongResult := readDeleteTestMessage(t, wrongTokenClient).(*proto.ClientAuthResult)
	if wrongResult.Accepted || wrongResult.ErrorCode != "unauthorized" {
		t.Fatalf("wrong token auth = %#v", wrongResult)
	}

	nonLocalClient, cleanupNonLocal := startLocalControlTestConnection(t, server, net.ParseIP("203.0.113.20"))
	defer cleanupNonLocal()
	writeLocalControlTestAuth(t, nonLocalClient, proto.ClientAuth{Role: "nodetray", Token: token, Version: proto.ProtocolVersion})
	nonLocalResult := readDeleteTestMessage(t, nonLocalClient).(*proto.ClientAuthResult)
	if nonLocalResult.Accepted || nonLocalResult.ErrorCode != "local_only" {
		t.Fatalf("non-local auth = %#v", nonLocalResult)
	}

	client, cleanup := startLocalControlTestConnection(t, server, net.ParseIP("::1"))
	defer cleanup()
	writeLocalControlTestAuth(t, client, proto.ClientAuth{Role: "nodetray", Token: token, Version: proto.ProtocolVersion})
	result := readDeleteTestMessage(t, client).(*proto.ClientAuthResult)
	if !result.Accepted || result.ErrorCode != "" {
		t.Fatalf("loopback auth = %#v", result)
	}
	request := proto.LocalRequest{RequestID: "status-1", Operation: proto.LocalOperationStatusGet}
	if err := client.WriteFrame(proto.MsgLocalRequest, &request); err != nil {
		t.Fatal(err)
	}
	response := readDeleteTestMessage(t, client).(*proto.LocalResponse)
	if !response.OK || response.RequestID != "status-1" || string(response.Payload) != "ready" {
		t.Fatalf("local response = %#v", response)
	}
	if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), "wrong-secret") {
		t.Fatal("authentication token leaked into server logs")
	}
}

func TestServerClientAuthRejectsWrongRoleAndProtocolVersion(t *testing.T) {
	server, _ := newLocalControlTestServer(t)
	server.SetLocalControl("token", localHandlerFunc(func(context.Context, proto.LocalRequest) proto.LocalResponse {
		return proto.LocalResponse{OK: true}
	}))
	for _, auth := range []proto.ClientAuth{
		{Role: "manager", Token: "token", Version: proto.ProtocolVersion},
		{Role: "nodetray", Token: "token", Version: proto.ProtocolVersion + 1},
	} {
		client, cleanup := startLocalControlTestConnection(t, server, net.ParseIP("127.0.0.1"))
		writeLocalControlTestAuth(t, client, auth)
		result := readDeleteTestMessage(t, client).(*proto.ClientAuthResult)
		if result.Accepted || result.ErrorCode != "unauthorized" {
			t.Fatalf("auth %#v result = %#v", auth, result)
		}
		cleanup()
	}
}

func TestServerLocalRequestWithoutHandlerReturnsLocalUnavailable(t *testing.T) {
	server, _ := newLocalControlTestServer(t)
	server.SetLocalControl("token", nil)
	client, cleanup := startLocalControlTestConnection(t, server, net.ParseIP("127.0.0.1"))
	defer cleanup()
	writeLocalControlTestAuth(t, client, proto.ClientAuth{Role: "nodetray", Token: "token", Version: proto.ProtocolVersion})
	if result := readDeleteTestMessage(t, client).(*proto.ClientAuthResult); !result.Accepted {
		t.Fatalf("auth result = %#v", result)
	}
	if err := client.WriteFrame(proto.MsgLocalRequest, &proto.LocalRequest{RequestID: "status-2", Operation: proto.LocalOperationStatusGet}); err != nil {
		t.Fatal(err)
	}
	response := readDeleteTestMessage(t, client).(*proto.LocalResponse)
	if response.OK || response.ErrorCode != "local_unavailable" || response.RequestID != "status-2" {
		t.Fatalf("local unavailable response = %#v", response)
	}
	if err := client.WriteFrame(proto.MsgPing, &proto.Ping{TS: 44}); err != nil {
		t.Fatal(err)
	}
	if pong := readDeleteTestMessage(t, client).(*proto.Pong); pong.TS != 44 {
		t.Fatalf("pong = %#v", pong)
	}
}

func TestServerManagerCompatibilityAndShutdownAuthorization(t *testing.T) {
	server, _ := newLocalControlTestServer(t)
	server.SetLocalControl("token", nil)
	server.phase2 = phase2HandlerFunc(func(task proto.Phase2Task, _ Sender) (proto.TaskAck, func()) {
		return proto.TaskAck{TaskID: task.TaskID, Accepted: true, Reason: "accepted"}, nil
	})
	client, cleanup := startLocalControlTestConnection(t, server, net.ParseIP("127.0.0.1"))
	defer cleanup()

	sendManagerScanAndReadAck(t, client, "manager-before-auth")
	if err := client.WriteFrame(proto.MsgPhase2Task, &proto.Phase2Task{TaskID: "manager-phase2"}); err != nil {
		t.Fatal(err)
	}
	phase2Ack := readDeleteTestMessage(t, client).(*proto.TaskAck)
	if !phase2Ack.Accepted || phase2Ack.TaskID != "manager-phase2" {
		t.Fatalf("manager Phase2 ack = %#v", phase2Ack)
	}
	if err := client.WriteFrame(proto.MsgLocalRequest, &proto.LocalRequest{RequestID: "manager-local", Operation: proto.LocalOperationStatusGet}); err != nil {
		t.Fatal(err)
	}
	response := readDeleteTestMessage(t, client).(*proto.LocalResponse)
	if response.OK || response.ErrorCode != "unauthorized" {
		t.Fatalf("unauthenticated local response = %#v", response)
	}
	if err := client.WriteFrame(proto.MsgShutdown, &proto.Shutdown{}); err != nil {
		t.Fatal(err)
	}
	protocolError := readDeleteTestMessage(t, client).(*proto.Error)
	if protocolError.Stage != "local" || protocolError.Msg != "unauthorized" {
		t.Fatalf("unauthenticated shutdown response = %#v", protocolError)
	}
	sendManagerScanAndReadAck(t, client, "manager-after-denial")
}

func TestServerAuthenticatedRawShutdownIsUnsupported(t *testing.T) {
	server, _ := newLocalControlTestServer(t)
	server.SetLocalControl("token", nil)
	client, cleanup := startLocalControlTestConnection(t, server, net.ParseIP("127.0.0.1"))
	defer cleanup()
	writeLocalControlTestAuth(t, client, proto.ClientAuth{Role: "nodetray", Token: "token", Version: proto.ProtocolVersion})
	_ = readDeleteTestMessage(t, client).(*proto.ClientAuthResult)
	if err := client.WriteFrame(proto.MsgShutdown, &proto.Shutdown{}); err != nil {
		t.Fatal(err)
	}
	protocolError := readDeleteTestMessage(t, client).(*proto.Error)
	if protocolError.Stage != "local" || protocolError.Msg != "unsupported_operation" {
		t.Fatalf("authenticated shutdown response = %#v", protocolError)
	}
}

func TestServerLocalResponseDoesNotLeakControlToken(t *testing.T) {
	const token = "response-must-not-contain-this-token"
	server, _ := newLocalControlTestServer(t)
	server.SetLocalControl(token, localHandlerFunc(func(context.Context, proto.LocalRequest) proto.LocalResponse {
		return proto.LocalResponse{RequestID: token, OK: true, ErrorCode: token, Payload: []byte(token)}
	}))
	client, cleanup := startLocalControlTestConnection(t, server, net.ParseIP("127.0.0.1"))
	defer cleanup()
	writeLocalControlTestAuth(t, client, proto.ClientAuth{Role: "nodetray", Token: token, Version: proto.ProtocolVersion})
	_ = readDeleteTestMessage(t, client).(*proto.ClientAuthResult)
	if err := client.WriteFrame(proto.MsgLocalRequest, &proto.LocalRequest{RequestID: "safe-id", Operation: proto.LocalOperationStatusGet}); err != nil {
		t.Fatal(err)
	}
	response := readDeleteTestMessage(t, client).(*proto.LocalResponse)
	encoded := response.RequestID + response.ErrorCode + string(response.Payload)
	if strings.Contains(encoded, token) {
		t.Fatalf("local response leaked control token: %#v", response)
	}
}

func TestServerAllLocalResponseBranchesCleanControlTokenFromRequestID(t *testing.T) {
	const token = "request-id-must-not-leak-this-token"
	tests := []struct {
		name         string
		authenticate bool
		operation    string
		wantError    string
	}{
		{
			name:      "unauthenticated",
			operation: proto.LocalOperationStatusGet,
			wantError: "unauthorized",
		},
		{
			name:         "invalid request",
			authenticate: true,
			operation:    "local.invalid",
			wantError:    proto.UnsupportedOperationErrorCode,
		},
		{
			name:         "local unavailable",
			authenticate: true,
			operation:    proto.LocalOperationStatusGet,
			wantError:    "local_unavailable",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, _ := newLocalControlTestServer(t)
			server.SetLocalControl(token, nil)
			client, cleanup := startLocalControlTestConnection(t, server, net.ParseIP("127.0.0.1"))
			defer cleanup()
			if test.authenticate {
				writeLocalControlTestAuth(t, client, proto.ClientAuth{
					Role: "nodetray", Token: token, Version: proto.ProtocolVersion,
				})
				if result := readDeleteTestMessage(t, client).(*proto.ClientAuthResult); !result.Accepted {
					t.Fatalf("auth result = %#v", result)
				}
			}
			if err := client.WriteFrame(proto.MsgLocalRequest, &proto.LocalRequest{
				RequestID: token, Operation: test.operation,
			}); err != nil {
				t.Fatal(err)
			}
			response := readDeleteTestMessage(t, client).(*proto.LocalResponse)
			if strings.Contains(response.RequestID, token) || response.ErrorCode != test.wantError {
				t.Fatalf("local response = %#v, want cleaned request ID and error %q", response, test.wantError)
			}
		})
	}
}

type recordingStatsProvider struct {
	window int
	report proto.StatsReport
}

func (p *recordingStatsProvider) Stats(windowSeconds int) proto.StatsReport {
	p.window = windowSeconds
	return p.report
}

func TestServerStatsQueryDispatchesClampedWindow(t *testing.T) {
	server := newDeleteTestServer(t)
	provider := &recordingStatsProvider{report: proto.StatsReport{
		CPU: 12.5, Workers: 6, WindowS: 300, FilesDone: 42,
	}}
	server.SetStatsProvider(provider)
	client, cleanup := startDeleteTestConnection(t, server, context.Background())
	defer cleanup()

	if err := client.WriteFrame(proto.MsgStatsQuery, &proto.StatsQuery{
		WindowSeconds: 999,
	}); err != nil {
		t.Fatal(err)
	}
	message := readDeleteTestMessage(t, client)
	report, ok := message.(*proto.StatsReport)
	if !ok || report.CPU != 12.5 || report.Workers != 6 || report.FilesDone != 42 {
		t.Fatalf("stats response = %#v", message)
	}
	if provider.window != 300 {
		t.Fatalf("provider window = %d, want 300", provider.window)
	}
}

func TestServerStatsQueryWithoutProviderReturnsProtocolError(t *testing.T) {
	server := newDeleteTestServer(t)
	client, cleanup := startDeleteTestConnection(t, server, context.Background())
	defer cleanup()
	if err := client.WriteFrame(proto.MsgStatsQuery, &proto.StatsQuery{
		WindowSeconds: 0,
	}); err != nil {
		t.Fatal(err)
	}
	message := readDeleteTestMessage(t, client)
	protocolError, ok := message.(*proto.Error)
	if !ok || protocolError.Stage != "stats" {
		t.Fatalf("stats unavailable response = %#v", message)
	}
}

func TestServerDeleteDispatchesOnceAndKeepsConnectionAlive(t *testing.T) {
	server := newDeleteTestServer(t)
	var calls atomic.Int32
	handler := deleteHandlerFunc(func(
		ctx context.Context,
		task proto.DeleteTask,
		sender Sender,
	) error {
		calls.Add(1)
		if ctx.Err() != nil {
			return fmt.Errorf("handler context already cancelled: %w", ctx.Err())
		}
		if task.TaskID != "delete-success" ||
			task.Seq != 7 ||
			task.LastSeq != 9 ||
			!reflectStringsEqual(task.Entries, []string{`D:\one`, `D:\two`}) {
			return fmt.Errorf("task = %#v", task)
		}
		task.Entries[0] = `D:\handler-owned-copy`
		return sender(proto.MsgDeleteReport, &proto.DeleteReport{
			TaskID:  task.TaskID,
			Seq:     task.Seq,
			LastSeq: task.LastSeq,
			Stats:   proto.DeleteStats{Total: 2, OK: 2},
			Entries: []proto.DeleteResult{
				{Path: `D:\one`, OK: true},
				{Path: `D:\two`, OK: true},
			},
		})
	})
	server.SetDeleteHandler(handler)

	client, cleanup := startDeleteTestConnection(t, server, context.Background())
	defer cleanup()
	task := proto.DeleteTask{
		TaskID:  "delete-success",
		Seq:     7,
		LastSeq: 9,
		Entries: []string{`D:\one`, `D:\two`},
	}
	if err := client.WriteFrame(proto.MsgDeleteTask, &task); err != nil {
		t.Fatal(err)
	}
	message := readDeleteTestMessage(t, client)
	report, ok := message.(*proto.DeleteReport)
	if !ok || report.TaskID != task.TaskID ||
		report.Seq != 7 ||
		report.LastSeq != 9 ||
		report.Stats != (proto.DeleteStats{Total: 2, OK: 2}) {
		t.Fatalf("delete report = %#v", message)
	}
	if task.Entries[0] != `D:\one` {
		t.Fatalf("handler mutated caller task entries: %#v", task.Entries)
	}
	if calls.Load() != 1 {
		t.Fatalf("delete handler calls = %d, want 1", calls.Load())
	}

	if err := client.WriteFrame(proto.MsgPing, &proto.Ping{TS: 812}); err != nil {
		t.Fatal(err)
	}
	pong, ok := readDeleteTestMessage(t, client).(*proto.Pong)
	if !ok || pong.TS != 812 {
		t.Fatalf("Pong = %#v", pong)
	}
}

func TestServerDeleteWithoutHandlerReportsEveryPathAndClosesConnection(t *testing.T) {
	for _, configure := range []func(*Server){
		func(*Server) {},
		func(server *Server) {
			var handler *typedNilDeleteHandler
			server.SetDeleteHandler(handler)
		},
	} {
		server := newDeleteTestServer(t)
		configure(server)
		client, cleanup := startDeleteTestConnection(t, server, context.Background())

		task := proto.DeleteTask{
			TaskID:  "delete-unavailable",
			Seq:     3,
			LastSeq: 5,
			Entries: []string{`D:\first`, `D:\second`, `D:\third`},
		}
		if err := client.WriteFrame(proto.MsgDeleteTask, &task); err != nil {
			t.Fatal(err)
		}
		message := readDeleteTestMessage(t, client)
		report, ok := message.(*proto.DeleteReport)
		if !ok {
			t.Fatalf("response = %#v, want DeleteReport", message)
		}
		if report.TaskID != task.TaskID ||
			report.Seq != task.Seq ||
			report.LastSeq != task.LastSeq ||
			report.Stats != (proto.DeleteStats{Total: 3, Failed: 3}) {
			t.Fatalf("report metadata/stats = %#v", report)
		}
		for index, result := range report.Entries {
			if result.Path != task.Entries[index] ||
				result.OK ||
				result.ErrCode != proto.DeleteErrHelperLost ||
				result.Err != "Agent delete handler unavailable" ||
				result.Uncertain {
				t.Fatalf("result[%d] = %#v", index, result)
			}
		}
		assertDeleteTestConnectionClosed(t, client)
		cleanup()
	}
}

func TestServerDeleteHandlerErrorClosesOnlyCurrentConnection(t *testing.T) {
	server := newDeleteTestServer(t)
	server.SetDeleteHandler(deleteHandlerFunc(func(
		context.Context,
		proto.DeleteTask,
		Sender,
	) error {
		return errors.New("forwarder failed")
	}))

	first, cleanupFirst := startDeleteTestConnection(t, server, context.Background())
	if err := first.WriteFrame(proto.MsgDeleteTask, &proto.DeleteTask{
		TaskID: "delete-error", Entries: []string{`D:\entry`},
	}); err != nil {
		t.Fatal(err)
	}
	assertDeleteTestConnectionClosed(t, first)
	cleanupFirst()

	second, cleanupSecond := startDeleteTestConnection(t, server, context.Background())
	defer cleanupSecond()
	if err := second.WriteFrame(proto.MsgPing, &proto.Ping{TS: 902}); err != nil {
		t.Fatal(err)
	}
	pong, ok := readDeleteTestMessage(t, second).(*proto.Pong)
	if !ok || pong.TS != 902 {
		t.Fatalf("fresh connection Pong = %#v", pong)
	}
}

func TestServerDeleteHandlerErrorLogsOnlyBoundedMetadata(t *testing.T) {
	cfg := config.DefaultAgent()
	cfg.MachineID = "machine-delete-log"
	cfg.Proto.HeartbeatS = 60
	var output bytes.Buffer
	server := NewServer(
		cfg,
		scanHandlerFunc(func(
			task proto.ScanTask,
			_ Sender,
		) (proto.TaskAck, func()) {
			return proto.TaskAck{TaskID: task.TaskID, Accepted: true}, nil
		}),
		slog.New(slog.NewJSONHandler(&output, nil)),
	)
	server.SetDeleteHandler(deleteHandlerFunc(func(
		context.Context,
		proto.DeleteTask,
		Sender,
	) error {
		return errors.New("dsn=postgres://secret")
	}))
	longTaskID := strings.Repeat("x", 4096)
	client, cleanup := startDeleteTestConnection(t, server, context.Background())
	if err := client.WriteFrame(proto.MsgDeleteTask, &proto.DeleteTask{
		TaskID:  longTaskID,
		Entries: []string{`D:\private\credential.txt`},
	}); err != nil {
		t.Fatal(err)
	}
	assertDeleteTestConnectionClosed(t, client)
	cleanup()

	logged := output.String()
	if strings.Contains(logged, longTaskID) || len(logged) > 1024 {
		t.Fatalf("delete failure log metadata was not bounded: length=%d", len(logged))
	}
	for _, secret := range []string{
		`D:\private\credential.txt`,
		"postgres://secret",
	} {
		if strings.Contains(logged, secret) {
			t.Fatalf("delete failure log leaked %q: %s", secret, logged)
		}
	}
	if !strings.Contains(logged, "delete_handler_error") {
		t.Fatalf("delete failure log lacks fixed category: %s", logged)
	}
}

func TestServerDeleteSenderFailureClosesOnlyCurrentConnection(t *testing.T) {
	server := newDeleteTestServer(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	senderResult := make(chan error, 1)
	server.SetDeleteHandler(deleteHandlerFunc(func(
		_ context.Context,
		task proto.DeleteTask,
		sender Sender,
	) error {
		close(entered)
		<-release
		err := sender(proto.MsgDeleteReport, &proto.DeleteReport{
			TaskID: task.TaskID,
			Stats:  proto.DeleteStats{Total: 1, OK: 1},
			Entries: []proto.DeleteResult{{
				Path: task.Entries[0],
				OK:   true,
			}},
		})
		senderResult <- err
		return err
	}))

	serverSide, clientSide := net.Pipe()
	connectionDone := make(chan struct{})
	go func() {
		server.handleConn(context.Background(), serverSide)
		close(connectionDone)
	}()
	first := proto.NewConn(clientSide)
	if _, _, err := first.ReadFrame(); err != nil {
		t.Fatal(err)
	}
	if err := first.WriteFrame(proto.MsgDeleteTask, &proto.DeleteTask{
		TaskID: "delete-sender-error", Entries: []string{`D:\entry`},
	}); err != nil {
		t.Fatal(err)
	}
	<-entered
	_ = first.Close()
	close(release)
	select {
	case err := <-senderResult:
		if err == nil {
			t.Fatal("Sender unexpectedly succeeded after GUI close")
		}
	case <-time.After(time.Second):
		t.Fatal("Sender remained blocked after GUI close")
	}
	select {
	case <-connectionDone:
	case <-time.After(time.Second):
		t.Fatal("failed GUI connection goroutine did not exit")
	}

	handlerDone := make(chan error, 1)
	go func() {
		second, cleanupSecond := startDeleteTestConnection(t, server, context.Background())
		defer cleanupSecond()
		if err := second.WriteFrame(proto.MsgPing, &proto.Ping{TS: 903}); err != nil {
			handlerDone <- err
			return
		}
		pong, ok := readDeleteTestMessage(t, second).(*proto.Pong)
		if !ok || pong.TS != 903 {
			handlerDone <- fmt.Errorf("fresh connection Pong = %#v", pong)
			return
		}
		handlerDone <- nil
	}()
	select {
	case err := <-handlerDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("fresh connection blocked after sender failure")
	}
}

func TestServerDeleteParentCancellationCancelsHandlerAndClosesConnection(t *testing.T) {
	server := newDeleteTestServer(t)
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	server.SetDeleteHandler(deleteHandlerFunc(func(
		ctx context.Context,
		_ proto.DeleteTask,
		_ Sender,
	) error {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	}))

	ctx, cancel := context.WithCancel(context.Background())
	client, cleanup := startDeleteTestConnection(t, server, ctx)
	defer cleanup()
	if err := client.WriteFrame(proto.MsgDeleteTask, &proto.DeleteTask{
		TaskID: "delete-cancel", Entries: []string{`D:\entry`},
	}); err != nil {
		t.Fatal(err)
	}
	<-entered
	cancel()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("delete handler context was not cancelled by parent")
	}
	assertDeleteTestConnectionClosed(t, client)
}

func TestServerDeleteListenAndServeWaitsForHandlerCancellationUnwind(
	t *testing.T,
) {
	cfg := config.DefaultAgent()
	cfg.MachineID = "machine-listener-join"
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.Proto.HeartbeatS = 60
	listening := make(chan string, 1)
	server := NewServer(
		cfg,
		scanHandlerFunc(func(
			task proto.ScanTask,
			_ Sender,
		) (proto.TaskAck, func()) {
			return proto.TaskAck{TaskID: task.TaskID, Accepted: true}, nil
		}),
		slog.New(&listenerAddressHandler{listening: listening}),
	)
	handlerEntered := make(chan struct{})
	cancellationObserved := make(chan struct{})
	releaseUnwind := make(chan struct{})
	handlerFinished := make(chan struct{})
	var releaseOnce sync.Once
	releaseHandler := func() {
		releaseOnce.Do(func() { close(releaseUnwind) })
	}
	defer releaseHandler()
	server.SetDeleteHandler(deleteHandlerFunc(func(
		ctx context.Context,
		_ proto.DeleteTask,
		_ Sender,
	) error {
		close(handlerEntered)
		<-ctx.Done()
		close(cancellationObserved)
		<-releaseUnwind
		close(handlerFinished)
		return ctx.Err()
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.ListenAndServe(ctx)
	}()
	var address string
	select {
	case address = <-listening:
	case err := <-serveDone:
		t.Fatalf("server exited before listening: %v", err)
	case <-time.After(time.Second):
		t.Fatal("server did not start listening")
	}

	rawClient, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client := proto.NewConn(rawClient)
	defer client.Close()
	if _, _, err := client.ReadFrame(); err != nil {
		t.Fatalf("read Hello: %v", err)
	}
	if err := client.WriteFrame(proto.MsgDeleteTask, &proto.DeleteTask{
		TaskID:  "delete-listener-join",
		Entries: []string{`D:\join-proof`},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-handlerEntered:
	case <-time.After(time.Second):
		t.Fatal("delete handler did not start")
	}

	cancel()
	select {
	case <-cancellationObserved:
	case <-time.After(time.Second):
		t.Fatal("delete handler did not observe server cancellation")
	}
	select {
	case err := <-serveDone:
		t.Fatalf(
			"ListenAndServe returned before handler cancellation unwind: %v",
			err,
		)
	case <-time.After(50 * time.Millisecond):
	}

	releaseHandler()
	select {
	case <-handlerFinished:
	case <-time.After(time.Second):
		t.Fatal("delete handler did not finish cancellation unwind")
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("ListenAndServe after connection join: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ListenAndServe did not return after handler finished")
	}
}

func TestServerDeleteRemoteCloseCancelsHandlerThroughHeartbeat(t *testing.T) {
	server := newDeleteTestServer(t)
	server.heartbeatInterval = 5 * time.Millisecond
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	server.SetDeleteHandler(deleteHandlerFunc(func(
		ctx context.Context,
		_ proto.DeleteTask,
		_ Sender,
	) error {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	}))

	serverSide, clientSide := net.Pipe()
	done := make(chan struct{})
	go func() {
		server.handleConn(context.Background(), serverSide)
		close(done)
	}()
	client := proto.NewConn(clientSide)
	if _, _, err := client.ReadFrame(); err != nil {
		t.Fatal(err)
	}
	if err := client.WriteFrame(proto.MsgDeleteTask, &proto.DeleteTask{
		TaskID: "delete-peer-close", Entries: []string{`D:\entry`},
	}); err != nil {
		t.Fatal(err)
	}
	<-entered
	_ = client.Close()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("heartbeat failure did not cancel delete handler")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("connection goroutine remained after heartbeat failure")
	}
}

func TestServerDeleteConcurrentHeartbeatAndReportWritesStayFramed(t *testing.T) {
	const reportCount = 40
	server := newDeleteTestServer(t)
	server.heartbeatInterval = time.Millisecond
	server.SetDeleteHandler(deleteHandlerFunc(func(
		_ context.Context,
		task proto.DeleteTask,
		sender Sender,
	) error {
		var group sync.WaitGroup
		errs := make(chan error, reportCount)
		for index := 0; index < reportCount; index++ {
			index := index
			group.Add(1)
			go func() {
				defer group.Done()
				errs <- sender(proto.MsgDeleteReport, &proto.DeleteReport{
					TaskID: task.TaskID,
					Seq:    uint32(index),
					Stats:  proto.DeleteStats{Total: 1, OK: 1},
					Entries: []proto.DeleteResult{{
						Path: fmt.Sprintf(`D:\entry-%02d`, index),
						OK:   true,
					}},
				})
			}()
		}
		group.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				return err
			}
		}
		return nil
	}))

	client, cleanup := startDeleteTestConnection(t, server, context.Background())
	defer cleanup()
	if err := client.WriteFrame(proto.MsgDeleteTask, &proto.DeleteTask{
		TaskID: "delete-concurrent", Entries: []string{`D:\entry`},
	}); err != nil {
		t.Fatal(err)
	}
	seenReports := make(map[uint32]string, reportCount)
	sawHeartbeat := false
	deadline := time.Now().Add(2 * time.Second)
	for len(seenReports) < reportCount || !sawHeartbeat {
		_ = client.SetReadDeadline(deadline)
		msgType, body, err := client.ReadFrame()
		if err != nil {
			t.Fatalf("read concurrent frame: %v", err)
		}
		message, err := proto.Decode(msgType, body)
		if err != nil {
			t.Fatalf("decode concurrent frame: %v", err)
		}
		switch value := message.(type) {
		case *proto.Ping:
			sawHeartbeat = true
		case *proto.DeleteReport:
			if len(value.Entries) != 1 ||
				value.Stats != (proto.DeleteStats{Total: 1, OK: 1}) {
				t.Fatalf("corrupt report = %#v", value)
			}
			seenReports[value.Seq] = value.Entries[0].Path
		default:
			t.Fatalf("unexpected concurrent message = %#v", message)
		}
	}
	for index := 0; index < reportCount; index++ {
		want := fmt.Sprintf(`D:\entry-%02d`, index)
		if seenReports[uint32(index)] != want {
			t.Fatalf("report[%d] path = %q, want %q",
				index, seenReports[uint32(index)], want)
		}
	}
}

func TestServerAcceptsScanTaskAndReturnsAck(t *testing.T) {
	cfg := config.DefaultAgent()
	cfg.MachineID = "machine-a"
	cfg.Proto.HeartbeatS = 60
	log := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	server := NewServer(cfg, scanHandlerFunc(func(
		task proto.ScanTask,
		sender Sender,
	) (proto.TaskAck, func()) {
		return proto.TaskAck{
			TaskID: task.TaskID, Accepted: true, Reason: "accepted", Total: -1,
		}, nil
	}), log)
	serverSide, clientSide := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go server.handleConn(ctx, serverSide)
	client := proto.NewConn(clientSide)
	defer client.Close()
	if _, _, err := client.ReadFrame(); err != nil {
		t.Fatal(err)
	}
	if err := client.WriteFrame(proto.MsgScanTask, &proto.ScanTask{
		TaskID: "scan-1", Roots: []string{`D:\media`}, Phase: 1,
	}); err != nil {
		t.Fatal(err)
	}
	msgType, body, err := client.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	message, err := proto.Decode(msgType, body)
	if err != nil {
		t.Fatal(err)
	}
	ack, ok := message.(*proto.TaskAck)
	if !ok || !ack.Accepted || ack.TaskID != "scan-1" {
		t.Fatalf("Ack = %#v", message)
	}
}

func TestServerSendsAckBeforeStartingScan(t *testing.T) {
	cfg := config.DefaultAgent()
	cfg.MachineID = "machine-a"
	cfg.Proto.HeartbeatS = 60
	log := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	server := NewServer(cfg, scanHandlerFunc(func(
		task proto.ScanTask,
		sender Sender,
	) (proto.TaskAck, func()) {
		return proto.TaskAck{
				TaskID: task.TaskID, Accepted: true, Reason: "accepted", Total: -1,
			}, func() {
				_ = sender(proto.MsgTaskDone, &proto.TaskDone{
					TaskID: task.TaskID,
				})
			}
	}), log)
	serverSide, clientSide := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go server.handleConn(ctx, serverSide)
	client := proto.NewConn(clientSide)
	defer client.Close()
	if _, _, err := client.ReadFrame(); err != nil {
		t.Fatal(err)
	}
	if err := client.WriteFrame(proto.MsgScanTask, &proto.ScanTask{
		TaskID: "scan-order", Roots: []string{`D:\media`}, Phase: 1,
	}); err != nil {
		t.Fatal(err)
	}
	firstType, _, err := client.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	secondType, _, err := client.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if firstType != proto.MsgTaskAck || secondType != proto.MsgTaskDone {
		t.Fatalf("message order = [%d, %d], want ACK then DONE", firstType, secondType)
	}
}

func TestServerRoutesPhase2TaskAndSendsAckBeforeStarting(t *testing.T) {
	cfg := config.DefaultAgent()
	cfg.MachineID = "machine-a"
	cfg.Proto.HeartbeatS = 60
	log := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	var scanCalls, phase2Calls int
	server := NewServer(
		cfg,
		scanHandlerFunc(func(
			task proto.ScanTask,
			sender Sender,
		) (proto.TaskAck, func()) {
			scanCalls++
			return proto.TaskAck{
				TaskID: task.TaskID, Accepted: true, Reason: "accepted",
			}, nil
		}),
		log,
		phase2HandlerFunc(func(
			task proto.Phase2Task,
			sender Sender,
		) (proto.TaskAck, func()) {
			phase2Calls++
			return proto.TaskAck{
					TaskID:   task.TaskID,
					Accepted: true,
					Reason:   "accepted",
					Total:    int64(len(task.Items)),
				}, func() {
					_ = sender(proto.MsgTaskDone, &proto.TaskDone{
						TaskID: task.TaskID,
					})
				}
		}),
	)
	serverSide, clientSide := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go server.handleConn(ctx, serverSide)
	client := proto.NewConn(clientSide)
	defer client.Close()
	if _, _, err := client.ReadFrame(); err != nil {
		t.Fatal(err)
	}

	task := proto.Phase2Task{
		TaskID: "phase2-order",
		Items: []proto.Phase2Item{
			validPhase2Image(`D:\media\phase2.jpg`),
		},
	}
	if err := client.WriteFrame(proto.MsgPhase2Task, &task); err != nil {
		t.Fatal(err)
	}
	firstType, firstBody, err := client.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	secondType, _, err := client.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	first, err := proto.Decode(firstType, firstBody)
	if err != nil {
		t.Fatal(err)
	}
	ack, ok := first.(*proto.TaskAck)
	if !ok || !ack.Accepted || ack.TaskID != task.TaskID {
		t.Fatalf("first message=%#v", first)
	}
	if firstType != proto.MsgTaskAck || secondType != proto.MsgTaskDone {
		t.Fatalf("message order=[%d %d], want Ack then TaskDone",
			firstType, secondType)
	}
	if phase2Calls != 1 || scanCalls != 0 {
		t.Fatalf("handler calls phase2=%d scan=%d, want 1/0",
			phase2Calls, scanCalls)
	}
}

func TestServerKeepsPhase1RoutedOnlyToScanHandlerWhenPhase2Enabled(t *testing.T) {
	cfg := config.DefaultAgent()
	cfg.MachineID = "machine-a"
	cfg.Proto.HeartbeatS = 60
	log := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	var scanCalls, phase2Calls int
	server := NewServer(
		cfg,
		scanHandlerFunc(func(
			task proto.ScanTask,
			sender Sender,
		) (proto.TaskAck, func()) {
			scanCalls++
			return proto.TaskAck{
				TaskID: task.TaskID, Accepted: true, Reason: "accepted",
			}, nil
		}),
		log,
		phase2HandlerFunc(func(
			task proto.Phase2Task,
			sender Sender,
		) (proto.TaskAck, func()) {
			phase2Calls++
			return proto.TaskAck{
				TaskID: task.TaskID, Accepted: true, Reason: "accepted",
			}, nil
		}),
	)
	serverSide, clientSide := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go server.handleConn(ctx, serverSide)
	client := proto.NewConn(clientSide)
	defer client.Close()
	if _, _, err := client.ReadFrame(); err != nil {
		t.Fatal(err)
	}
	if err := client.WriteFrame(proto.MsgScanTask, &proto.ScanTask{
		TaskID: "scan-phase1-only",
		Roots:  []string{`D:\media`},
		Phase:  1,
	}); err != nil {
		t.Fatal(err)
	}
	msgType, _, err := client.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if msgType != proto.MsgTaskAck || scanCalls != 1 || phase2Calls != 0 {
		t.Fatalf("response=%d calls scan=%d phase2=%d",
			msgType, scanCalls, phase2Calls)
	}
}

type scanHandlerFunc func(proto.ScanTask, Sender) (proto.TaskAck, func())

func (fn scanHandlerFunc) Prepare(
	task proto.ScanTask,
	sender Sender,
) (proto.TaskAck, func()) {
	return fn(task, sender)
}

type phase2HandlerFunc func(proto.Phase2Task, Sender) (proto.TaskAck, func())

func (fn phase2HandlerFunc) Prepare(
	task proto.Phase2Task,
	sender Sender,
) (proto.TaskAck, func()) {
	return fn(task, sender)
}

type deleteHandlerFunc func(context.Context, proto.DeleteTask, Sender) error

func (fn deleteHandlerFunc) Handle(
	ctx context.Context,
	task proto.DeleteTask,
	sender Sender,
) error {
	return fn(ctx, task, sender)
}

type typedNilDeleteHandler struct{}

func (*typedNilDeleteHandler) Handle(
	context.Context,
	proto.DeleteTask,
	Sender,
) error {
	panic("typed-nil delete handler must not be called")
}

type localHandlerFunc func(context.Context, proto.LocalRequest) proto.LocalResponse

func (fn localHandlerFunc) HandleLocal(
	ctx context.Context,
	request proto.LocalRequest,
) proto.LocalResponse {
	return fn(ctx, request)
}

type localControlTestConn struct {
	net.Conn
	remote net.Addr
}

func (connection localControlTestConn) RemoteAddr() net.Addr {
	return connection.remote
}

func newLocalControlTestServer(t *testing.T) (*Server, *bytes.Buffer) {
	t.Helper()
	cfg := config.DefaultAgent()
	cfg.MachineID = "machine-local-control"
	cfg.Proto.HeartbeatS = 60
	logs := &bytes.Buffer{}
	server := NewServer(
		cfg,
		scanHandlerFunc(func(task proto.ScanTask, _ Sender) (proto.TaskAck, func()) {
			return proto.TaskAck{TaskID: task.TaskID, Accepted: true, Reason: "accepted"}, nil
		}),
		slog.New(slog.NewJSONHandler(logs, nil)),
	)
	return server, logs
}

func startLocalControlTestConnection(
	t *testing.T,
	server *Server,
	remoteIP net.IP,
) (*proto.Conn, func()) {
	t.Helper()
	serverSide, clientSide := net.Pipe()
	done := make(chan struct{})
	go func() {
		server.handleConn(context.Background(), localControlTestConn{
			Conn:   serverSide,
			remote: &net.TCPAddr{IP: remoteIP, Port: 43210},
		})
		close(done)
	}()
	client := proto.NewConn(clientSide)
	if _, _, err := client.ReadFrame(); err != nil {
		t.Fatalf("read Hello: %v", err)
	}
	return client, func() {
		_ = client.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("local control server goroutine did not exit")
		}
	}
}

func writeLocalControlTestAuth(t *testing.T, client *proto.Conn, auth proto.ClientAuth) {
	t.Helper()
	if err := client.WriteFrame(proto.MsgClientAuth, &auth); err != nil {
		t.Fatalf("write ClientAuth: %v", err)
	}
}

func sendManagerScanAndReadAck(t *testing.T, client *proto.Conn, taskID string) {
	t.Helper()
	if err := client.WriteFrame(proto.MsgScanTask, &proto.ScanTask{TaskID: taskID, Roots: []string{`D:\media`}, Phase: 1}); err != nil {
		t.Fatal(err)
	}
	ack := readDeleteTestMessage(t, client).(*proto.TaskAck)
	if !ack.Accepted || ack.TaskID != taskID {
		t.Fatalf("manager scan ack = %#v", ack)
	}
}

func newDeleteTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.DefaultAgent()
	cfg.MachineID = "machine-delete"
	cfg.Proto.HeartbeatS = 60
	return NewServer(
		cfg,
		scanHandlerFunc(func(
			task proto.ScanTask,
			_ Sender,
		) (proto.TaskAck, func()) {
			return proto.TaskAck{
				TaskID: task.TaskID, Accepted: true, Reason: "accepted",
			}, nil
		}),
		slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
	)
}

func startDeleteTestConnection(
	t *testing.T,
	server *Server,
	ctx context.Context,
) (*proto.Conn, func()) {
	t.Helper()
	serverSide, clientSide := net.Pipe()
	done := make(chan struct{})
	go func() {
		server.handleConn(ctx, serverSide)
		close(done)
	}()
	client := proto.NewConn(clientSide)
	if _, _, err := client.ReadFrame(); err != nil {
		t.Fatalf("read Hello: %v", err)
	}
	var once sync.Once
	return client, func() {
		once.Do(func() {
			_ = client.Close()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("server connection goroutine did not exit")
			}
		})
	}
}

func readDeleteTestMessage(t *testing.T, client *proto.Conn) any {
	t.Helper()
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	msgType, body, err := client.ReadFrame()
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	message, err := proto.Decode(msgType, body)
	if err != nil {
		t.Fatalf("decode message type=%d: %v", msgType, err)
	}
	return message
}

func assertDeleteTestConnectionClosed(t *testing.T, client *proto.Conn) {
	t.Helper()
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if msgType, _, err := client.ReadFrame(); err == nil {
		t.Fatalf("connection remained open; received message type %d", msgType)
	}
}

func reflectStringsEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

type listenerAddressHandler struct {
	listening chan<- string
}

func (handler *listenerAddressHandler) Enabled(
	context.Context,
	slog.Level,
) bool {
	return true
}

func (handler *listenerAddressHandler) Handle(
	_ context.Context,
	record slog.Record,
) error {
	if record.Message != "agent listening" {
		return nil
	}
	record.Attrs(func(attribute slog.Attr) bool {
		if attribute.Key == "addr" {
			handler.listening <- attribute.Value.String()
			return false
		}
		return true
	})
	return nil
}

func (handler *listenerAddressHandler) WithAttrs([]slog.Attr) slog.Handler {
	return handler
}

func (handler *listenerAddressHandler) WithGroup(string) slog.Handler {
	return handler
}
