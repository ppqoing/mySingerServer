package nodectl

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type statusProviderFunc func() Status

func (f statusProviderFunc) ControlStatus() Status { return f() }

func TestServerReturnsStatusSnapshotAndCorrelatesRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln := newPipeListener()
	done := serveForTest(ctx, t, ln, statusProviderFunc(validAgentStatus), nil)

	conn, err := ln.Dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	want := validAgentStatus()
	request := Request{Version: ProtocolVersion, RequestID: "status-request", Command: CommandStatus}
	if err := WriteFrame(conn, request); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := ReadFrame(conn, &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || response.RequestID != request.RequestID || response.Status == nil {
		t.Fatalf("response = %#v, want correlated successful status", response)
	}
	if response.Status.MachineID != want.MachineID || response.Status.WorkerReady != want.WorkerReady {
		t.Fatalf("status = %#v, want provider snapshot %#v", *response.Status, want)
	}
	cancelAndWait(t, cancel, done)
}

func TestServerShutdownRespondsBeforeCallingOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln := newPipeListener()
	var completedWrites atomic.Int32
	ln.serverWrap = func(conn net.Conn) net.Conn {
		return &writeCountingConn{Conn: conn, writes: &completedWrites}
	}
	called := make(chan struct{}, 2)
	orderViolation := make(chan struct{}, 1)
	done := serveForTest(ctx, t, ln, statusProviderFunc(validAgentStatus), func() {
		if completedWrites.Load() < 2 {
			orderViolation <- struct{}{}
		}
		called <- struct{}{}
	})

	for i := 0; i < 2; i++ {
		conn, err := ln.Dial(ctx)
		if err != nil {
			t.Fatal(err)
		}
		request := Request{Version: ProtocolVersion, RequestID: "shutdown-request", Command: CommandShutdown}
		if err := WriteFrame(conn, request); err != nil {
			t.Fatal(err)
		}
		var response Response
		if err := ReadFrame(conn, &response); err != nil {
			t.Fatal(err)
		}
		conn.Close()
		if !response.OK || response.RequestID != request.RequestID {
			t.Fatalf("response = %#v, want correlated shutdown success", response)
		}
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("shutdown callback was not called")
	}
	select {
	case <-called:
		t.Fatal("shutdown callback was called more than once")
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case <-orderViolation:
		t.Fatal("shutdown callback ran before the success response frame was written")
	default:
	}
	cancelAndWait(t, cancel, done)
}

func TestServerHandlesConcurrentRequestsWithoutCrossingFrames(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln := newPipeListener()
	done := serveForTest(ctx, t, ln, statusProviderFunc(validAgentStatus), nil)
	client := NewClient(ln.Dial)

	const requests = 40
	errCh := make(chan error, requests)
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, err := client.Status(ctx)
			if err == nil && status.MachineID != "node-a" {
				err = errors.New("wrong status snapshot")
			}
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent Status() error = %v", err)
		}
	}
	cancelAndWait(t, cancel, done)
}

func TestServerLimitsConcurrentHandlersToSixteen(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln := newPipeListener()
	release := make(chan struct{})
	reached := make(chan struct{}, 32)
	var active atomic.Int32
	var maximum atomic.Int32
	provider := statusProviderFunc(func() Status {
		now := active.Add(1)
		for {
			old := maximum.Load()
			if now <= old || maximum.CompareAndSwap(old, now) {
				break
			}
		}
		reached <- struct{}{}
		<-release
		active.Add(-1)
		return validAgentStatus()
	})
	done := serveForTest(ctx, t, ln, provider, nil)
	client := NewClient(ln.Dial)

	const requests = 24
	errCh := make(chan error, requests)
	for i := 0; i < requests; i++ {
		go func() {
			_, err := client.Status(ctx)
			errCh <- err
		}()
	}
	for i := 0; i < 16; i++ {
		select {
		case <-reached:
		case <-time.After(time.Second):
			t.Fatalf("only %d handlers reached provider", i)
		}
	}
	select {
	case <-reached:
		t.Fatal("more than 16 handlers entered concurrently")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	for i := 0; i < requests; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("Status() error = %v", err)
		}
	}
	if got := maximum.Load(); got != 16 {
		t.Fatalf("maximum concurrent handlers = %d, want 16", got)
	}
	cancelAndWait(t, cancel, done)
}

func TestServerReturnsStableErrorsAndKeepsListeningAfterBadFrame(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln := newPipeListener()
	done := serveForTest(ctx, t, ln, statusProviderFunc(validAgentStatus), nil)

	conn, err := ln.Dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	unknown := Request{Version: ProtocolVersion, RequestID: "unknown-request", Command: "reboot"}
	if err := WriteFrame(conn, unknown); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := ReadFrame(conn, &response); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if response.OK || response.ErrorCode != "unsupported_command" || response.RequestID != unknown.RequestID {
		t.Fatalf("unknown-command response = %#v", response)
	}

	bad, err := ln.Dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], MaxFrameSize+1)
	if _, err := bad.Write(header[:]); err != nil {
		t.Fatal(err)
	}
	bad.Close()

	status, err := NewClient(ln.Dial).Status(ctx)
	if err != nil {
		t.Fatalf("Status() after bad frame error = %v", err)
	}
	if status.MachineID != "node-a" {
		t.Fatalf("Status() after bad frame = %#v", status)
	}
	cancelAndWait(t, cancel, done)
}

func TestServerReturnsInvalidRequestAndStatusUnavailable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln := newPipeListener()
	invalidStatus := validAgentStatus()
	invalidStatus.ExecutablePath = ""
	done := serveForTest(ctx, t, ln, statusProviderFunc(func() Status { return invalidStatus }), nil)

	invalid := exchangeRequest(t, ctx, ln, Request{Version: ProtocolVersion + 1, RequestID: "invalid-version", Command: CommandStatus})
	if invalid.OK || invalid.ErrorCode != "invalid_request" || invalid.RequestID != "invalid-version" {
		t.Fatalf("invalid request response = %#v", invalid)
	}
	unavailable := exchangeRequest(t, ctx, ln, Request{Version: ProtocolVersion, RequestID: "status-unavailable", Command: CommandStatus})
	if unavailable.OK || unavailable.ErrorCode != "status_unavailable" || unavailable.RequestID != "status-unavailable" {
		t.Fatalf("unavailable status response = %#v", unavailable)
	}
	if unavailable.ErrorSummary != SanitizeSummary(unavailable.ErrorSummary) {
		t.Fatalf("error summary = %q, want sanitized summary", unavailable.ErrorSummary)
	}
	cancelAndWait(t, cancel, done)
}

func TestServerRecoversStatusProviderPanicAsInternalError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln := newPipeListener()
	provider := statusProviderFunc(func() Status { panic("password=must-not-leak") })
	done := serveForTest(ctx, t, ln, provider, nil)

	response := exchangeRequest(t, ctx, ln, Request{Version: ProtocolVersion, RequestID: "provider-panic", Command: CommandStatus})
	if response.OK || response.ErrorCode != "internal_error" || response.RequestID != "provider-panic" {
		t.Fatalf("provider panic response = %#v", response)
	}
	if strings.Contains(response.ErrorSummary, "must-not-leak") || response.ErrorSummary != SanitizeSummary(response.ErrorSummary) {
		t.Fatalf("provider panic summary = %q, want sanitized non-secret summary", response.ErrorSummary)
	}
	cancelAndWait(t, cancel, done)
}

func TestServerProcessesOnlyOneRequestPerConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln := newPipeListener()
	done := serveForTest(ctx, t, ln, statusProviderFunc(validAgentStatus), nil)
	conn, err := ln.Dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	request := Request{Version: ProtocolVersion, RequestID: "first", Command: CommandStatus}
	if err := WriteFrame(conn, request); err != nil {
		t.Fatal(err)
	}
	var first Response
	if err := ReadFrame(conn, &first); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(100 * time.Millisecond))
	if err := WriteFrame(conn, Request{Version: ProtocolVersion, RequestID: "second", Command: CommandStatus}); err == nil {
		var second Response
		if err := ReadFrame(conn, &second); err == nil {
			t.Fatalf("second response = %#v, want connection closed after first request", second)
		}
	}
	conn.Close()
	cancelAndWait(t, cancel, done)
}

func TestServerCancellationReturnsWithoutLeakingAcceptLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ln := newPipeListener()
	done := serveForTest(ctx, t, ln, statusProviderFunc(validAgentStatus), nil)
	cancelAndWait(t, cancel, done)
	if _, err := ln.Dial(context.Background()); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Dial() after cancellation error = %v, want net.ErrClosed", err)
	}
}

func TestServerListenerCloseStopsBlockedConnectionHandlers(t *testing.T) {
	ctx := context.Background()
	ln := newPipeListener()
	done := serveForTest(ctx, t, ln, statusProviderFunc(validAgentStatus), nil)
	conn, err := ln.Dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve() after listener close error = %v, want nil", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Serve() did not stop a blocked connection handler after listener close")
	}
}

func serveForTest(ctx context.Context, t *testing.T, ln net.Listener, provider StatusProvider, shutdown ShutdownFunc) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, provider, shutdown) }()
	return done
}

func cancelAndWait(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve() did not return after context cancellation")
	}
}

func exchangeRequest(t *testing.T, ctx context.Context, ln *pipeListener, request Request) Response {
	t.Helper()
	conn, err := ln.Dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := WriteFrame(conn, request); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := ReadFrame(conn, &response); err != nil {
		t.Fatal(err)
	}
	return response
}

type pipeListener struct {
	connections chan net.Conn
	closed      chan struct{}
	closeOnce   sync.Once
	serverWrap  func(net.Conn) net.Conn
}

func newPipeListener() *pipeListener {
	return &pipeListener{connections: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.connections:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr("nodectl") }

func (l *pipeListener) Dial(ctx context.Context) (net.Conn, error) {
	client, server := net.Pipe()
	if l.serverWrap != nil {
		server = l.serverWrap(server)
	}
	select {
	case l.connections <- server:
		return client, nil
	case <-l.closed:
		client.Close()
		server.Close()
		return nil, net.ErrClosed
	case <-ctx.Done():
		client.Close()
		server.Close()
		return nil, ctx.Err()
	}
}

type pipeAddr string

func (a pipeAddr) Network() string { return "pipe" }
func (a pipeAddr) String() string  { return string(a) }

type writeCountingConn struct {
	net.Conn
	writes *atomic.Int32
}

func (c *writeCountingConn) Write(value []byte) (int, error) {
	n, err := c.Conn.Write(value)
	if err == nil && n == len(value) {
		c.writes.Add(1)
	}
	return n, err
}
