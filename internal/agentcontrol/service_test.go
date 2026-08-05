package agentcontrol

import (
	"context"
	"errors"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"dedup/internal/nodectl"
)

func TestControlServiceShutdownCancelsAgentRootContext(t *testing.T) {
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverConn, clientConn := net.Pipe()
	listener := newControlTestListener(serverConn)
	service := New(controlStatusProvider{}, nodectl.ShutdownFunc(cancel))
	service.listen = func(string) (net.Listener, error) { return listener, nil }
	serveDone := make(chan error, 1)
	go func() { serveDone <- service.Run(root) }()

	request := nodectl.Request{
		Version: nodectl.ProtocolVersion, RequestID: "shutdown-agent", Command: nodectl.CommandShutdown,
	}
	if err := nodectl.WriteFrame(clientConn, request); err != nil {
		t.Fatal(err)
	}
	var response nodectl.Response
	if err := nodectl.ReadFrame(clientConn, &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK {
		t.Fatalf("shutdown response = %#v", response)
	}
	select {
	case <-root.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not cancel Agent root context")
	}
	_ = clientConn.Close()
	if err := <-serveDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("service Run error = %v, want context cancellation", err)
	}
}

func TestSingleInstanceRejectsSecondAgentAndReleasesMutex(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows named mutex contract")
	}
	machineID := "single-instance-" + time.Now().UTC().Format("150405.000000000")
	first, err := AcquireSingleInstance(machineID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := AcquireSingleInstance(machineID)
	if second != nil {
		_ = second.Close()
	}
	if !errors.Is(err, ErrAlreadyRunning) {
		_ = first.Close()
		t.Fatalf("second acquisition error = %v, want ErrAlreadyRunning", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	reacquired, err := AcquireSingleInstance(machineID)
	if err != nil {
		t.Fatalf("mutex was not released: %v", err)
	}
	if err := reacquired.Close(); err != nil {
		t.Fatal(err)
	}
}

type controlStatusProvider struct{}

func (controlStatusProvider) ControlStatus() nodectl.Status {
	return nodectl.Status{
		Component: nodectl.ComponentAgent, MachineID: "control-test", PID: 1,
		ExecutablePath: `C:\agent.exe`, Lifecycle: "running", ServiceReady: true,
		Ready: true, SyncHealthy: true,
	}
}

type controlTestListener struct {
	conn net.Conn
	once sync.Once
	done chan struct{}
}

func newControlTestListener(conn net.Conn) *controlTestListener {
	return &controlTestListener{conn: conn, done: make(chan struct{})}
}

func (l *controlTestListener) Accept() (net.Conn, error) {
	var accepted net.Conn
	l.once.Do(func() { accepted = l.conn })
	if accepted != nil {
		return accepted, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *controlTestListener) Close() error {
	select {
	case <-l.done:
	default:
		close(l.done)
	}
	return nil
}

func (*controlTestListener) Addr() net.Addr { return controlTestAddr("control-test") }

type controlTestAddr string

func (a controlTestAddr) Network() string { return string(a) }
func (a controlTestAddr) String() string  { return string(a) }
