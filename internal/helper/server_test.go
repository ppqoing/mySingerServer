package helper

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"dedup/internal/proto"
	"github.com/vmihailenco/msgpack/v5"
)

func TestServerSendsExactHelperHelloBeforeReadingRequests(t *testing.T) {
	listener := newQueuedListener()
	logger, _ := testServerLogger()
	server := NewServer(
		Config{FrameReadTimeoutSec: 60, FrameWriteTimeoutSec: 60},
		listener,
		nil,
		logger,
	)
	run := startTestServer(t, server)
	serverSide, clientSide := net.Pipe()
	listener.add(t, serverSide)
	client := proto.NewConn(clientSide)
	t.Cleanup(func() { _ = client.Close() })

	hello := readHelperHello(t, client)

	if hello.Role != HelperRole ||
		hello.Version != proto.ProtocolVersion ||
		hello.PID != os.Getpid() {
		t.Fatalf("Hello = %#v", hello)
	}
	run.stop(t)
}

func TestServerAppliesConfiguredDeadlinesToHelloReadsAndReports(t *testing.T) {
	const readTimeout = 7 * time.Second
	const writeTimeout = 11 * time.Second
	listener := newQueuedListener()
	logger, _ := testServerLogger()
	processor := &Processor{cfg: Config{MaxEntriesPerFrame: 2000}}
	server := NewServer(
		Config{
			FrameReadTimeoutSec:  int(readTimeout / time.Second),
			FrameWriteTimeoutSec: int(writeTimeout / time.Second),
		},
		listener,
		processor,
		logger,
	)
	run := startTestServer(t, server)
	serverSide, clientSide := net.Pipe()
	recording := &deadlineRecordingConn{Conn: serverSide}
	listener.add(t, recording)
	client := proto.NewConn(clientSide)
	t.Cleanup(func() { _ = client.Close() })

	readHelperHello(t, client)
	task := proto.DeleteTask{
		TaskID:    "not-a-secret-task-id",
		Confirmed: false,
		Entries:   []string{`C:\run-owned\deadline.bin`},
	}
	if err := client.WriteFrame(proto.MsgDeleteTask, &task); err != nil {
		t.Fatalf("write DeleteTask: %v", err)
	}
	msgType, body, err := client.ReadFrame()
	if err != nil {
		t.Fatalf("read DeleteReport: %v", err)
	}
	decoded, err := proto.Decode(msgType, body)
	if err != nil {
		t.Fatalf("decode DeleteReport: %v", err)
	}
	report, ok := decoded.(*proto.DeleteReport)
	if !ok || msgType != proto.MsgDeleteReport ||
		len(report.Entries) != 1 ||
		report.Entries[0].ErrCode != proto.DeleteErrNotConfirmed {
		t.Fatalf("DeleteReport = %#v", decoded)
	}

	readCalls, writeCalls := recording.deadlineCalls()
	if len(readCalls) == 0 {
		t.Fatal("server never set a read deadline")
	}
	if len(writeCalls) < 2 {
		t.Fatalf("nonzero write deadlines = %d, want Hello and report", len(writeCalls))
	}
	requireDeadlineDuration(t, "read", readCalls[0], readTimeout)
	requireDeadlineDuration(t, "Hello write", writeCalls[0], writeTimeout)
	requireDeadlineDuration(t, "report write", writeCalls[1], writeTimeout)
	run.stop(t)
}

func TestServerProcessesOneConnectionAtATimeAndSurvivesDisconnect(t *testing.T) {
	listener := newQueuedListener()
	logger, _ := testServerLogger()
	server := NewServer(
		Config{FrameReadTimeoutSec: 60, FrameWriteTimeoutSec: 60},
		listener,
		nil,
		logger,
	)
	run := startTestServer(t, server)
	firstServer, firstClientSide := net.Pipe()
	secondServer, secondClientSide := net.Pipe()
	listener.add(t, firstServer)
	listener.add(t, secondServer)
	firstClient := proto.NewConn(firstClientSide)
	secondClient := proto.NewConn(secondClientSide)
	t.Cleanup(func() {
		_ = firstClient.Close()
		_ = secondClient.Close()
	})

	readHelperHello(t, firstClient)
	if err := secondClient.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := secondClient.ReadFrame(); err == nil {
		t.Fatal("second connection received Hello before first disconnected")
	}
	if err := secondClient.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}

	if err := firstClient.Close(); err != nil {
		t.Fatalf("close first client: %v", err)
	}
	readHelperHello(t, secondClient)
	run.stop(t)
}

func TestServerMalformedEnvelopeBodyAndUnknownTypeOnlyCloseCurrentConnection(t *testing.T) {
	tests := []struct {
		name string
		send func(*testing.T, net.Conn, *proto.Conn)
	}{
		{
			name: "malformed envelope",
			send: func(t *testing.T, raw net.Conn, _ *proto.Conn) {
				writeRawServerFrame(t, raw, []byte{0xc1})
			},
		},
		{
			name: "malformed body",
			send: func(t *testing.T, raw net.Conn, _ *proto.Conn) {
				payload, err := msgpack.Marshal(map[string]any{
					"t": proto.MsgDeleteTask,
					"b": msgpack.RawMessage([]byte{0xc1}),
				})
				if err != nil {
					t.Fatal(err)
				}
				writeRawServerFrame(t, raw, payload)
			},
		},
		{
			name: "unknown message type",
			send: func(t *testing.T, _ net.Conn, framed *proto.Conn) {
				if err := framed.WriteFrame(250, &proto.ConfigPush{
					KV: map[string]string{"value": "not-logged"},
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			listener := newQueuedListener()
			logger, _ := testServerLogger()
			server := NewServer(
				Config{FrameReadTimeoutSec: 60, FrameWriteTimeoutSec: 60},
				listener,
				nil,
				logger,
			)
			run := startTestServer(t, server)
			firstServer, firstClientSide := net.Pipe()
			secondServer, secondClientSide := net.Pipe()
			listener.add(t, firstServer)
			listener.add(t, secondServer)
			firstClient := proto.NewConn(firstClientSide)
			secondClient := proto.NewConn(secondClientSide)
			t.Cleanup(func() {
				_ = firstClient.Close()
				_ = secondClient.Close()
			})

			readHelperHello(t, firstClient)
			tt.send(t, firstClientSide, firstClient)
			requireFramedConnectionClosed(t, firstClient)
			readHelperHello(t, secondClient)
			run.stop(t)
		})
	}
}

func TestServerShutdownWaitsForPriorDeleteReportThenStops(t *testing.T) {
	fixture := newLocalProcessorFixture(t, nil)
	source := fixture.writeFile(t, "shutdown-order.bin", "content")
	removeStarted := make(chan struct{})
	allowRemove := make(chan struct{})
	var startOnce sync.Once
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(allowRemove) }) })
	realRemove := fixture.processor.ops.remove
	fixture.processor.ops.remove = func(path string) error {
		startOnce.Do(func() { close(removeStarted) })
		<-allowRemove
		return realRemove(path)
	}

	listener := newQueuedListener()
	logger, _ := testServerLogger()
	server := NewServer(
		Config{FrameReadTimeoutSec: 60, FrameWriteTimeoutSec: 60},
		listener,
		fixture.processor,
		logger,
	)
	run := startTestServer(t, server)
	serverSide, clientSide := net.Pipe()
	listener.add(t, serverSide)
	client := proto.NewConn(clientSide)
	t.Cleanup(func() { _ = client.Close() })
	readHelperHello(t, client)

	task := validProcessorTask([]string{source}, proto.ModeHard)
	if err := client.WriteFrame(proto.MsgDeleteTask, &task); err != nil {
		t.Fatalf("write DeleteTask: %v", err)
	}
	select {
	case <-removeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("delete processor did not reach remove")
	}

	shutdownWritten := make(chan error, 1)
	go func() {
		shutdownWritten <- client.WriteFrame(
			proto.MsgShutdown,
			&proto.Shutdown{},
		)
	}()
	select {
	case err := <-shutdownWritten:
		t.Fatalf("Shutdown completed before prior report: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	select {
	case <-run.done:
		t.Fatal("server stopped before prior DeleteReport completed")
	default:
	}

	releaseOnce.Do(func() { close(allowRemove) })
	msgType, body, err := client.ReadFrame()
	if err != nil {
		t.Fatalf("read prior DeleteReport: %v", err)
	}
	decoded, err := proto.Decode(msgType, body)
	if err != nil {
		t.Fatal(err)
	}
	report, ok := decoded.(*proto.DeleteReport)
	if !ok || msgType != proto.MsgDeleteReport ||
		len(report.Entries) != 1 || !report.Entries[0].OK {
		t.Fatalf("prior DeleteReport = %#v", decoded)
	}
	select {
	case err := <-shutdownWritten:
		if err != nil {
			t.Fatalf("write Shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown write did not complete after prior report")
	}
	if err := run.await(t); err != nil {
		t.Fatalf("Serve after Shutdown: %v", err)
	}
	requireFilesMissing(t, source)
}

func TestActiveRequestsAndListeningTrackDeleteDrainDuringCancellation(t *testing.T) {
	fixture := newLocalProcessorFixture(t, nil)
	source := fixture.writeFile(t, "active-request.bin", "content")
	removeStarted := make(chan struct{})
	allowRemove := make(chan struct{})
	var startOnce sync.Once
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(allowRemove) }) })
	realRemove := fixture.processor.ops.remove
	fixture.processor.ops.remove = func(path string) error {
		startOnce.Do(func() { close(removeStarted) })
		<-allowRemove
		return realRemove(path)
	}

	listener := newQueuedListener()
	logger, _ := testServerLogger()
	server := NewServer(
		Config{FrameReadTimeoutSec: 60, FrameWriteTimeoutSec: 60},
		listener,
		fixture.processor,
		logger,
	)
	if server.Listening() {
		t.Fatal("server reports listening before Serve starts")
	}
	if got := server.ActiveRequests(); got != 0 {
		t.Fatalf("active requests before Serve = %d, want 0", got)
	}
	run := startTestServer(t, server)
	waitForHelperServerState(t, time.Second, func() bool { return server.Listening() }, "listener did not become ready")

	serverSide, clientSide := net.Pipe()
	listener.add(t, serverSide)
	client := proto.NewConn(clientSide)
	t.Cleanup(func() { _ = client.Close() })
	readHelperHello(t, client)
	if got := server.ActiveRequests(); got != 0 {
		t.Fatalf("idle accepted connection counted as active request: %d", got)
	}

	task := validProcessorTask([]string{source}, proto.ModeHard)
	if err := client.WriteFrame(proto.MsgDeleteTask, &task); err != nil {
		t.Fatalf("write DeleteTask: %v", err)
	}
	select {
	case <-removeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("delete processor did not start")
	}
	if got := server.ActiveRequests(); got != 1 {
		t.Fatalf("active requests during delete = %d, want 1", got)
	}

	run.cancel()
	waitForHelperServerState(t, time.Second, func() bool { return !server.Listening() }, "listener remained ready after cancellation")
	if got := server.ActiveRequests(); got != 1 {
		t.Fatalf("accepted delete was dropped during drain: active requests = %d", got)
	}
	select {
	case <-run.done:
		t.Fatal("server exited before accepted delete completed")
	default:
	}

	releaseOnce.Do(func() { close(allowRemove) })
	msgType, body, err := client.ReadFrame()
	if err != nil {
		t.Fatalf("read drained DeleteReport: %v", err)
	}
	decoded, err := proto.Decode(msgType, body)
	if err != nil {
		t.Fatal(err)
	}
	report, ok := decoded.(*proto.DeleteReport)
	if !ok || msgType != proto.MsgDeleteReport || len(report.Entries) != 1 || !report.Entries[0].OK {
		t.Fatalf("drained DeleteReport = %#v", decoded)
	}
	if err := run.await(t); err != nil {
		t.Fatalf("Serve after drained cancellation: %v", err)
	}
	if got := server.ActiveRequests(); got != 0 {
		t.Fatalf("active requests after completion = %d, want 0", got)
	}
	if server.Listening() {
		t.Fatal("server reports listening after Serve stopped")
	}
}

func TestActiveRequestPromotionIsAtomicWithCancellation(t *testing.T) {
	fixture := newLocalProcessorFixture(t, nil)
	source := fixture.writeFile(t, "promotion-race.bin", "content")
	removeStarted := make(chan struct{})
	allowRemove := make(chan struct{})
	var startOnce sync.Once
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(allowRemove) }) })
	realRemove := fixture.processor.ops.remove
	fixture.processor.ops.remove = func(path string) error {
		startOnce.Do(func() { close(removeStarted) })
		<-allowRemove
		return realRemove(path)
	}

	listener := newQueuedListener()
	logger, _ := testServerLogger()
	server := NewServer(
		Config{FrameReadTimeoutSec: 60, FrameWriteTimeoutSec: 60},
		listener,
		fixture.processor,
		logger,
	)
	run := startTestServer(t, server)
	serverSide, clientSide := net.Pipe()
	listener.add(t, serverSide)
	client := proto.NewConn(clientSide)
	t.Cleanup(func() { _ = client.Close() })
	readHelperHello(t, client)

	// Hold the connection-state lock so cancellation reaches the idle-close
	// decision but cannot close the connection until the request is promoted.
	server.activeMu.Lock()
	locked := true
	t.Cleanup(func() {
		if locked {
			server.activeMu.Unlock()
		}
	})
	run.cancel()
	waitForHelperServerState(t, time.Second, func() bool { return !server.Listening() }, "listener remained ready after cancellation")

	task := validProcessorTask([]string{source}, proto.ModeHard)
	if err := client.WriteFrame(proto.MsgDeleteTask, &task); err != nil {
		t.Fatalf("write DeleteTask during cancellation: %v", err)
	}
	select {
	case <-removeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("request was not promoted while cancellation waited")
	}
	if got := server.ActiveRequests(); got != 1 {
		t.Fatalf("promoted active requests = %d, want 1", got)
	}

	server.activeMu.Unlock()
	locked = false
	releaseOnce.Do(func() { close(allowRemove) })
	msgType, body, err := client.ReadFrame()
	if err != nil {
		t.Fatalf("accepted request was truncated by cancellation: %v", err)
	}
	decoded, err := proto.Decode(msgType, body)
	if err != nil {
		t.Fatal(err)
	}
	report, ok := decoded.(*proto.DeleteReport)
	if !ok || msgType != proto.MsgDeleteReport || len(report.Entries) != 1 || !report.Entries[0].OK {
		t.Fatalf("drained DeleteReport = %#v", decoded)
	}
	if err := run.await(t); err != nil {
		t.Fatalf("Serve after promoted drain: %v", err)
	}
	if got := server.ActiveRequests(); got != 0 {
		t.Fatalf("active requests after promoted drain = %d, want 0", got)
	}
}

func TestServerContextCancellationClosesListenerAndActiveConnection(t *testing.T) {
	t.Run("blocked accept", func(t *testing.T) {
		listener := newQueuedListener()
		logger, _ := testServerLogger()
		server := NewServer(
			Config{FrameReadTimeoutSec: 60, FrameWriteTimeoutSec: 60},
			listener,
			nil,
			logger,
		)
		run := startTestServer(t, server)

		run.stop(t)
		if !listener.isClosed() {
			t.Fatal("listener remained open after context cancellation")
		}
	})

	t.Run("active read", func(t *testing.T) {
		listener := newQueuedListener()
		logger, _ := testServerLogger()
		server := NewServer(
			Config{FrameReadTimeoutSec: 60, FrameWriteTimeoutSec: 60},
			listener,
			nil,
			logger,
		)
		run := startTestServer(t, server)
		serverSide, clientSide := net.Pipe()
		listener.add(t, serverSide)
		client := proto.NewConn(clientSide)
		t.Cleanup(func() { _ = client.Close() })
		readHelperHello(t, client)

		run.stop(t)
		requireFramedConnectionClosed(t, client)
		if !listener.isClosed() {
			t.Fatal("listener remained open after active cancellation")
		}
	})
}

func TestServerCancellationClosesConnectionAcceptedDuringCancelRace(t *testing.T) {
	serverSide, clientSide := net.Pipe()
	defer clientSide.Close()
	listener := newCancelRaceListener(serverSide)
	logger, _ := testServerLogger()
	server := NewServer(
		Config{FrameReadTimeoutSec: 60, FrameWriteTimeoutSec: 60},
		listener,
		nil,
		logger,
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()

	<-listener.acceptStarted
	cancel()
	<-listener.closed
	close(listener.releaseAccept)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve after cancel/accept race: %v", err)
		}
	case <-time.After(2 * time.Second):
		_ = clientSide.Close()
		<-done
		t.Fatal("Serve remained blocked on a connection accepted during cancellation")
	}
}

func TestServerLogsBoundedMetadataWithoutSensitiveFrameValues(t *testing.T) {
	listener := newQueuedListener()
	logger, logBuffer := testServerLogger()
	server := NewServer(
		Config{FrameReadTimeoutSec: 60, FrameWriteTimeoutSec: 60},
		listener,
		nil,
		logger,
	)
	run := startTestServer(t, server)
	serverSide, clientSide := net.Pipe()
	listener.add(t, serverSide)
	client := proto.NewConn(clientSide)
	t.Cleanup(func() { _ = client.Close() })
	readHelperHello(t, client)

	markers := []string{
		"confirmation-secret",
		"credential-secret",
		"database-secret",
		`C:\private\secret.bin`,
	}
	if err := client.WriteFrame(251, &proto.ConfigPush{KV: map[string]string{
		"confirmation_token": markers[0],
		"password":           markers[1],
		"database":           markers[2],
		"path":               markers[3],
	}}); err != nil {
		t.Fatal(err)
	}
	requireFramedConnectionClosed(t, client)
	run.stop(t)

	logged := logBuffer.String()
	if len(logged) == 0 || len(logged) > 8<<10 {
		t.Fatalf("server log length = %d, want bounded non-empty metadata", len(logged))
	}
	if !strings.Contains(logged, `"message_type":251`) {
		t.Fatalf("server log lacks bounded frame type metadata: %s", logged)
	}
	for _, marker := range markers {
		if strings.Contains(logged, marker) {
			t.Fatalf("server log leaked sensitive marker %q: %s", marker, logged)
		}
	}
	escapedPathMarker := strings.ReplaceAll(markers[3], `\`, `\\`)
	if strings.Contains(logged, escapedPathMarker) {
		t.Fatalf("server log leaked JSON-escaped path marker %q: %s", escapedPathMarker, logged)
	}
}

func TestServerLogsFixedBoundedSecurityRejectionSummary(t *testing.T) {
	const (
		taskTokenMarker  = "11111111-1111-4111-8111-111111111111"
		unknownCode      = "E_FUTURE_POLICY_secret-code"
		dynamicErrMarker = "dynamic-error-secret"
	)
	fixture := newLocalProcessorFixture(t, []string{"denied"})
	badPath := `C:\unsafe~alias\path-token-secret.bin`
	denied := fixture.writeFile(t, "denied/denied-path-secret.bin", "denied")
	reparse := fixture.writeFile(t, "reparse-path-secret.bin", "reparse")
	access := fixture.writeFile(t, "access-path-secret.bin", "access")
	unknown := fixture.writeFile(t, "unknown-path-secret.bin", "unknown")
	notFound := fixture.writeFile(t, "not-found-path-secret.bin", "not-found")
	success := fixture.writeFile(t, "success-path-secret.bin", "success")
	realRemove := fixture.processor.ops.remove
	fixture.processor.ops.remove = func(path string) error {
		switch {
		case ordinalEqualFold(path, reparse):
			return pathError(
				proto.DeleteErrReparse,
				errors.New(dynamicErrMarker+"-reparse"),
			)
		case ordinalEqualFold(path, access):
			return pathError(
				proto.DeleteErrAccessDenied,
				errors.New(dynamicErrMarker+"-access"),
			)
		case ordinalEqualFold(path, unknown):
			return pathError(
				unknownCode,
				errors.New(dynamicErrMarker+"-unknown"),
			)
		case ordinalEqualFold(path, notFound):
			return pathError(
				proto.DeleteErrNotFound,
				errors.New(dynamicErrMarker+"-not-found"),
			)
		default:
			return realRemove(path)
		}
	}

	listener := newQueuedListener()
	logger, logBuffer := testServerLogger()
	server := NewServer(
		Config{FrameReadTimeoutSec: 60, FrameWriteTimeoutSec: 60},
		listener,
		fixture.processor,
		logger,
	)
	run := startTestServer(t, server)
	serverSide, clientSide := net.Pipe()
	listener.add(t, serverSide)
	client := proto.NewConn(clientSide)
	t.Cleanup(func() { _ = client.Close() })
	readHelperHello(t, client)

	task := validProcessorTask(
		[]string{badPath, denied, reparse, access, unknown, notFound, success},
		proto.ModeHard,
	)
	task.TaskID = taskTokenMarker
	if err := client.WriteFrame(proto.MsgDeleteTask, &task); err != nil {
		t.Fatalf("write DeleteTask: %v", err)
	}
	msgType, body, err := client.ReadFrame()
	if err != nil {
		t.Fatalf("read DeleteReport: %v", err)
	}
	decoded, err := proto.Decode(msgType, body)
	if err != nil {
		t.Fatalf("decode DeleteReport: %v", err)
	}
	report, ok := decoded.(*proto.DeleteReport)
	if !ok || msgType != proto.MsgDeleteReport || len(report.Entries) != 7 {
		t.Fatalf("DeleteReport = %#v", decoded)
	}
	if err := client.WriteFrame(proto.MsgShutdown, &proto.Shutdown{}); err != nil {
		t.Fatalf("write Shutdown: %v", err)
	}
	if err := run.await(t); err != nil {
		t.Fatalf("Serve after Shutdown: %v", err)
	}

	logged := logBuffer.String()
	completed := findServerLogEvent(t, logged, "delete_completed")
	wantCounts := map[string]float64{
		"result_count":                           7,
		"security_rejection_total":               5,
		"security_rejection_bad_path_count":      1,
		"security_rejection_path_denied_count":   1,
		"security_rejection_not_confirmed_count": 0,
		"security_rejection_access_denied_count": 1,
		"security_rejection_reparse_count":       1,
		"security_rejection_bad_mode_count":      0,
		"security_rejection_other_count":         1,
	}
	for key, want := range wantCounts {
		if got := completed[key]; got != want {
			t.Errorf("%s = %#v, want %.0f; event=%#v", key, got, want, completed)
		}
	}
	for _, marker := range []string{
		taskTokenMarker,
		unknownCode,
		dynamicErrMarker,
		badPath,
		denied,
		reparse,
		access,
		unknown,
		notFound,
		success,
	} {
		if strings.Contains(logged, marker) ||
			strings.Contains(logged, strings.ReplaceAll(marker, `\`, `\\`)) {
			t.Fatalf("security summary log leaked marker %q: %s", marker, logged)
		}
	}
}

func findServerLogEvent(
	t *testing.T,
	logged string,
	event string,
) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode server log line: %v: %q", err, line)
		}
		if record["event"] == event {
			return record
		}
	}
	t.Fatalf("server log lacks event %q: %s", event, logged)
	return nil
}

func readHelperHello(t *testing.T, conn *proto.Conn) *proto.Hello {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	msgType, body, err := conn.ReadFrame()
	if err != nil {
		t.Fatalf("read Hello: %v", err)
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	decoded, err := proto.Decode(msgType, body)
	if err != nil {
		t.Fatalf("decode Hello: %v", err)
	}
	hello, ok := decoded.(*proto.Hello)
	if !ok || msgType != proto.MsgHello {
		t.Fatalf("Hello type = %T message=%d", decoded, msgType)
	}
	return hello
}

func writeRawServerFrame(t *testing.T, conn net.Conn, payload []byte) {
	t.Helper()
	if err := conn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if _, err := conn.Write(header[:]); err != nil {
		t.Fatalf("write raw frame header: %v", err)
	}
	if _, err := conn.Write(payload); err != nil {
		if errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) {
			return
		}
		t.Fatalf("write raw frame payload: %v", err)
	}
}

func requireFramedConnectionClosed(t *testing.T, conn *proto.Conn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		if errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) {
			return
		}
		t.Fatalf("set closed-connection read deadline: %v", err)
	}
	if _, _, err := conn.ReadFrame(); err == nil {
		t.Fatal("connection remained open")
	}
}

type queuedListener struct {
	connections chan net.Conn
	closed      chan struct{}
	closeOnce   sync.Once
}

type cancelRaceListener struct {
	conn          net.Conn
	acceptStarted chan struct{}
	releaseAccept chan struct{}
	closed        chan struct{}
	closeOnce     sync.Once
	acceptMu      sync.Mutex
	accepted      bool
}

func newCancelRaceListener(conn net.Conn) *cancelRaceListener {
	return &cancelRaceListener{
		conn:          conn,
		acceptStarted: make(chan struct{}),
		releaseAccept: make(chan struct{}),
		closed:        make(chan struct{}),
	}
}

func (l *cancelRaceListener) Accept() (net.Conn, error) {
	l.acceptMu.Lock()
	if l.accepted {
		l.acceptMu.Unlock()
		return nil, net.ErrClosed
	}
	l.accepted = true
	l.acceptMu.Unlock()
	close(l.acceptStarted)
	<-l.releaseAccept
	return l.conn, nil
}

func (l *cancelRaceListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *cancelRaceListener) Addr() net.Addr {
	return testServerAddr("cancel-race")
}

func newQueuedListener() *queuedListener {
	return &queuedListener{
		connections: make(chan net.Conn, 8),
		closed:      make(chan struct{}),
	}
}

func (l *queuedListener) add(t *testing.T, conn net.Conn) {
	t.Helper()
	select {
	case <-l.closed:
		_ = conn.Close()
		t.Fatal("add connection to closed listener")
	case l.connections <- conn:
	}
}

func (l *queuedListener) Accept() (net.Conn, error) {
	select {
	case <-l.closed:
		return nil, net.ErrClosed
	case conn := <-l.connections:
		return conn, nil
	}
}

func (l *queuedListener) Close() error {
	l.closeOnce.Do(func() {
		close(l.closed)
		for {
			select {
			case conn := <-l.connections:
				_ = conn.Close()
			default:
				return
			}
		}
	})
	return nil
}

func (l *queuedListener) Addr() net.Addr { return testServerAddr("queued") }

func (l *queuedListener) isClosed() bool {
	select {
	case <-l.closed:
		return true
	default:
		return false
	}
}

type testServerAddr string

func (a testServerAddr) Network() string { return "test" }
func (a testServerAddr) String() string  { return string(a) }

type testServerRun struct {
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	err    error
}

func startTestServer(t *testing.T, server *Server) *testServerRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	run := &testServerRun{cancel: cancel, done: make(chan struct{})}
	go func() {
		err := server.Serve(ctx)
		run.mu.Lock()
		run.err = err
		run.mu.Unlock()
		close(run.done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-run.done:
		case <-time.After(2 * time.Second):
			t.Errorf("Task 4 server goroutine residue")
		}
	})
	return run
}

func (r *testServerRun) await(t *testing.T) error {
	t.Helper()
	select {
	case <-r.done:
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.err
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return")
		return nil
	}
}

func (r *testServerRun) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	if err := r.await(t); err != nil {
		t.Fatalf("Serve after context cancellation: %v", err)
	}
}

func waitForHelperServerState(t *testing.T, timeout time.Duration, predicate func() bool, failure string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(failure)
}

type deadlineCall struct {
	at       time.Time
	deadline time.Time
}

type deadlineRecordingConn struct {
	net.Conn
	mu     sync.Mutex
	reads  []deadlineCall
	writes []deadlineCall
}

func (c *deadlineRecordingConn) SetReadDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		c.mu.Lock()
		c.reads = append(c.reads, deadlineCall{at: time.Now(), deadline: deadline})
		c.mu.Unlock()
	}
	return c.Conn.SetReadDeadline(deadline)
}

func (c *deadlineRecordingConn) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		c.mu.Lock()
		c.writes = append(c.writes, deadlineCall{at: time.Now(), deadline: deadline})
		c.mu.Unlock()
	}
	return c.Conn.SetWriteDeadline(deadline)
}

func (c *deadlineRecordingConn) deadlineCalls() ([]deadlineCall, []deadlineCall) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]deadlineCall(nil), c.reads...),
		append([]deadlineCall(nil), c.writes...)
}

func requireDeadlineDuration(
	t *testing.T,
	label string,
	call deadlineCall,
	want time.Duration,
) {
	t.Helper()
	got := call.deadline.Sub(call.at)
	if got < want-time.Second || got > want+time.Second {
		t.Fatalf("%s deadline duration = %v, want %v", label, got, want)
	}
}

func testServerLogger() (*slog.Logger, *bytes.Buffer) {
	buffer := &bytes.Buffer{}
	return slog.New(slog.NewJSONHandler(buffer, nil)), buffer
}
