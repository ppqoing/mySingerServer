package agentdelete

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
)

var pipeDialerTestSequence atomic.Uint64

func TestPipeDialerConnectsWithByteStreamSemanticsAndTearsDown(t *testing.T) {
	pipeName := uniqueAgentDeletePipeName()
	listener, err := winio.ListenPipe(pipeName, &winio.PipeConfig{
		MessageMode:      false,
		InputBufferSize:  64 << 10,
		OutputBufferSize: 64 << 10,
	})
	if err != nil {
		t.Fatalf("ListenPipe: %v", err)
	}
	t.Cleanup(func() {
		if listener != nil {
			_ = listener.Close()
		}
	})

	acceptResults := make(chan pipeAcceptResult, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		acceptResults <- pipeAcceptResult{conn: conn, err: acceptErr}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, err := NewPipeDialer(pipeName).Dial(ctx)
	if err != nil {
		t.Fatalf("PipeDialer.Dial: %v", err)
	}
	t.Cleanup(func() {
		if client != nil {
			_ = client.Close()
		}
	})
	server := waitPipeAcceptResult(t, acceptResults)
	t.Cleanup(func() {
		if server != nil {
			_ = server.Close()
		}
	})

	if _, messageMode := client.(interface{ CloseWrite() error }); messageMode {
		t.Fatal("client exposes CloseWrite; connection is message-mode")
	}
	if _, messageMode := server.(interface{ CloseWrite() error }); messageMode {
		t.Fatal("server exposes CloseWrite; connection is message-mode")
	}
	if err := client.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("client deadline: %v", err)
	}
	if err := server.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("server deadline: %v", err)
	}
	if _, err := client.Write([]byte("byte-")); err != nil {
		t.Fatalf("client first write: %v", err)
	}
	if _, err := client.Write([]byte("stream")); err != nil {
		t.Fatalf("client second write: %v", err)
	}
	fromClient := make([]byte, len("byte-stream"))
	if _, err := io.ReadFull(server, fromClient); err != nil {
		t.Fatalf("server ReadFull: %v", err)
	}
	if got := string(fromClient); got != "byte-stream" {
		t.Fatalf("server read %q, want %q", got, "byte-stream")
	}
	if _, err := server.Write([]byte("two-")); err != nil {
		t.Fatalf("server first write: %v", err)
	}
	if _, err := server.Write([]byte("parts")); err != nil {
		t.Fatalf("server second write: %v", err)
	}
	fromServer := make([]byte, len("two-parts"))
	if _, err := io.ReadFull(client, fromServer); err != nil {
		t.Fatalf("client ReadFull: %v", err)
	}
	if got := string(fromServer); got != "two-parts" {
		t.Fatalf("client read %q, want %q", got, "two-parts")
	}

	if err := client.Close(); err != nil {
		t.Fatalf("client Close: %v", err)
	}
	client = nil
	if err := server.Close(); err != nil {
		t.Fatalf("server Close: %v", err)
	}
	server = nil
	if err := listener.Close(); err != nil {
		t.Fatalf("listener Close: %v", err)
	}
	listener = nil
	assertPipeDialerUnavailable(t, pipeName)
}

func TestPipeDialerObeysCancellationAndBusyPipeDeadline(t *testing.T) {
	pipeName := uniqueAgentDeletePipeName()
	listener, err := winio.ListenPipe(pipeName, &winio.PipeConfig{
		MessageMode: false,
	})
	if err != nil {
		t.Fatalf("ListenPipe: %v", err)
	}
	t.Cleanup(func() {
		if listener != nil {
			_ = listener.Close()
		}
	})

	acceptResults := make(chan pipeAcceptResult, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		acceptResults <- pipeAcceptResult{conn: conn, err: acceptErr}
	}()
	blockerCtx, blockerCancel := context.WithTimeout(
		context.Background(),
		time.Second,
	)
	defer blockerCancel()
	blocker, err := winio.DialPipeContext(blockerCtx, pipeName)
	if err != nil {
		t.Fatalf("occupy first pipe instance: %v", err)
	}
	t.Cleanup(func() {
		if blocker != nil {
			_ = blocker.Close()
		}
	})
	accepted := waitPipeAcceptResult(t, acceptResults)
	t.Cleanup(func() {
		if accepted != nil {
			_ = accepted.Close()
		}
	})

	dialer := NewPipeDialer(pipeName)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if conn, dialErr := dialer.Dial(cancelled); conn != nil ||
		!errors.Is(dialErr, context.Canceled) {
		if conn != nil {
			_ = conn.Close()
		}
		t.Fatalf("cancelled Dial = conn %v err %v, want context.Canceled", conn, dialErr)
	}

	deadlineCtx, deadlineCancel := context.WithTimeout(
		context.Background(),
		40*time.Millisecond,
	)
	started := time.Now()
	conn, dialErr := dialer.Dial(deadlineCtx)
	elapsed := time.Since(started)
	deadlineCancel()
	if conn != nil {
		_ = conn.Close()
		t.Fatal("busy pipe deadline unexpectedly connected")
	}
	if !errors.Is(dialErr, context.DeadlineExceeded) {
		t.Fatalf("busy pipe Dial error = %v, want context deadline", dialErr)
	}
	if elapsed < 20*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("busy pipe deadline elapsed = %v, want bounded near 40ms", elapsed)
	}

	if err := blocker.Close(); err != nil {
		t.Fatalf("blocker Close: %v", err)
	}
	blocker = nil
	if err := accepted.Close(); err != nil {
		t.Fatalf("accepted Close: %v", err)
	}
	accepted = nil
	if err := listener.Close(); err != nil {
		t.Fatalf("listener Close: %v", err)
	}
	listener = nil
	assertPipeDialerUnavailable(t, pipeName)
}

type pipeAcceptResult struct {
	conn net.Conn
	err  error
}

func waitPipeAcceptResult(
	t *testing.T,
	results <-chan pipeAcceptResult,
) net.Conn {
	t.Helper()
	select {
	case result := <-results:
		if result.err != nil {
			t.Fatalf("Accept: %v", result.err)
		}
		return result.conn
	case <-time.After(2 * time.Second):
		t.Fatal("Accept did not return")
		return nil
	}
}

func uniqueAgentDeletePipeName() string {
	return fmt.Sprintf(
		`\\.\pipe\dedup-agent-delete-test-%d-%d-%d`,
		os.Getpid(),
		time.Now().UnixNano(),
		pipeDialerTestSequence.Add(1),
	)
}

func assertPipeDialerUnavailable(t *testing.T, pipeName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	conn, err := NewPipeDialer(pipeName).Dial(ctx)
	if conn != nil {
		_ = conn.Close()
		t.Fatalf("pipe %q remained dialable after teardown", pipeName)
	}
	if err == nil {
		t.Fatalf("pipe %q returned no connection and no error", pipeName)
	}
}
