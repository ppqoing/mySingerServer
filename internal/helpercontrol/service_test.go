package helpercontrol

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"dedup/internal/nodectl"
)

func TestControlServiceUsesSeparateHelperPipeAndCancelsRoot(t *testing.T) {
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverConn, clientConn := net.Pipe()
	listener := newHelperControlTestListener(serverConn)
	service := New(helperControlStatusProvider{}, nodectl.ShutdownFunc(cancel))
	var requestedName string
	service.listen = func(name string) (net.Listener, error) {
		requestedName = name
		return listener, nil
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- service.Run(root) }()

	request := nodectl.Request{
		Version: nodectl.ProtocolVersion, RequestID: "shutdown-helper", Command: nodectl.CommandShutdown,
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
	if requestedName != nodectl.HelperPipeName() {
		t.Fatalf("control listener name = %q, want %q", requestedName, nodectl.HelperPipeName())
	}
	select {
	case <-root.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not cancel Helper root context")
	}
	_ = clientConn.Close()
	if err := <-serveDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("service Run error = %v, want context cancellation", err)
	}
}

type helperControlStatusProvider struct{}

func (helperControlStatusProvider) ControlStatus() nodectl.Status {
	return nodectl.Status{
		Component: nodectl.ComponentHelper, MachineID: "helper-control-test", PID: 1,
		ExecutablePath: `C:\helper.exe`, Lifecycle: "running", ServiceReady: true, Ready: true,
	}
}

type helperControlTestListener struct {
	conn net.Conn
	once sync.Once
	done chan struct{}
}

func newHelperControlTestListener(conn net.Conn) *helperControlTestListener {
	return &helperControlTestListener{conn: conn, done: make(chan struct{})}
}

func (l *helperControlTestListener) Accept() (net.Conn, error) {
	var accepted net.Conn
	l.once.Do(func() { accepted = l.conn })
	if accepted != nil {
		return accepted, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *helperControlTestListener) Close() error {
	select {
	case <-l.done:
	default:
		close(l.done)
	}
	return nil
}

func (*helperControlTestListener) Addr() net.Addr {
	return helperControlTestAddr("helper-control-test")
}

type helperControlTestAddr string

func (a helperControlTestAddr) Network() string { return string(a) }
func (a helperControlTestAddr) String() string  { return string(a) }
