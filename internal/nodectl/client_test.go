package nodectl

import (
	"context"
	"encoding/hex"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClientUsesFreshConnectionAnd128BitRequestID(t *testing.T) {
	var mu sync.Mutex
	var ids []string
	var dials int
	dial := func(ctx context.Context) (net.Conn, error) {
		client, server := net.Pipe()
		mu.Lock()
		dials++
		mu.Unlock()
		go func() {
			defer server.Close()
			var request Request
			if err := ReadFrame(server, &request); err != nil {
				return
			}
			mu.Lock()
			ids = append(ids, request.RequestID)
			mu.Unlock()
			_ = WriteFrame(server, Response{Version: ProtocolVersion, RequestID: request.RequestID, OK: true, Status: ptrStatus(validAgentStatus())})
		}()
		return client, nil
	}
	client := NewClient(dial)
	for i := 0; i < 2; i++ {
		if _, err := client.Status(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if dials != 2 || len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("dials=%d ids=%v, want two fresh connections and IDs", dials, ids)
	}
	for _, id := range ids {
		decoded, err := hex.DecodeString(id)
		if err != nil || len(decoded) != 16 {
			t.Fatalf("request ID %q decodes to %d bytes, err=%v; want 16 bytes", id, len(decoded), err)
		}
	}
}

func TestClientSetsConnectionDeadlineFromContext(t *testing.T) {
	deadline := time.Now().Add(time.Minute).Round(0)
	recorded := make(chan time.Time, 1)
	dial := func(ctx context.Context) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			var request Request
			if err := ReadFrame(server, &request); err != nil {
				return
			}
			_ = WriteFrame(server, Response{Version: ProtocolVersion, RequestID: request.RequestID, OK: true, Status: ptrStatus(validAgentStatus())})
		}()
		return &deadlineConn{Conn: client, deadlines: recorded}, nil
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if _, err := NewClient(dial).Status(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-recorded:
		if !got.Equal(deadline) {
			t.Fatalf("SetDeadline(%v), want %v", got, deadline)
		}
	case <-time.After(time.Second):
		t.Fatal("client did not set a connection deadline")
	}
}

func TestClientRejectsMismatchedResponseID(t *testing.T) {
	dial := func(ctx context.Context) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			var request Request
			if err := ReadFrame(server, &request); err != nil {
				return
			}
			_ = WriteFrame(server, Response{Version: ProtocolVersion, RequestID: "different-request", OK: true, Status: ptrStatus(validAgentStatus())})
		}()
		return client, nil
	}
	_, err := NewClient(dial).Status(context.Background())
	if err == nil || !strings.Contains(err.Error(), "request ID") {
		t.Fatalf("Status() error = %v, want request ID mismatch", err)
	}
}

func TestClientHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dial := func(ctx context.Context) (net.Conn, error) {
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("dial context error = %v, want context.Canceled", ctx.Err())
		}
		return nil, ctx.Err()
	}
	_, err := NewClient(dial).Status(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Status() error = %v, want context.Canceled", err)
	}
}

func TestClientShutdownReturnsServerError(t *testing.T) {
	dial := func(ctx context.Context) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			var request Request
			if err := ReadFrame(server, &request); err != nil {
				return
			}
			_ = WriteFrame(server, Response{Version: ProtocolVersion, RequestID: request.RequestID, OK: false, ErrorCode: "internal_error", ErrorSummary: "shutdown unavailable"})
		}()
		return client, nil
	}
	err := NewClient(dial).Shutdown(context.Background())
	if err == nil || !strings.Contains(err.Error(), "internal_error") || !strings.Contains(err.Error(), "shutdown unavailable") {
		t.Fatalf("Shutdown() error = %v, want stable server error", err)
	}
}

type deadlineConn struct {
	net.Conn
	deadlines chan<- time.Time
}

func (c *deadlineConn) SetDeadline(deadline time.Time) error {
	select {
	case c.deadlines <- deadline:
	default:
	}
	return c.Conn.SetDeadline(deadline)
}
