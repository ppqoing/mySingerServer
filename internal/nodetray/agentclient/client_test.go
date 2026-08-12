package agentclient

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	"dedup/internal/proto"
)

func TestAgentClientReadsHelloBeforeAuthAndRejectsWrongMachine(t *testing.T) {
	machineID := testMachineID("1")
	token := "local-control-test-token"
	listener := listenAgentClientTestTCP(t)
	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		framed := proto.NewConn(conn)
		if err := framed.WriteFrame(proto.MsgHello, &proto.Hello{
			Version: proto.ProtocolVersion, MachineID: machineID, PID: 41,
		}); err != nil {
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
		if !ok || auth.Role != "nodetray" || auth.Token != token || auth.Version != proto.ProtocolVersion {
			serverErr <- errors.New("client did not send the fixed authentication envelope after Hello")
			return
		}
		serverErr <- framed.WriteFrame(proto.MsgClientAuthResult, &proto.ClientAuthResult{Accepted: true})
	}()

	client, err := Dial(context.Background(), listener.Addr().String(), token, machineID)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("server: %v", err)
	}

	wrongListener := listenAgentClientTestTCP(t)
	wrongDone := make(chan error, 1)
	go func() {
		conn, err := wrongListener.Accept()
		if err != nil {
			wrongDone <- err
			return
		}
		defer conn.Close()
		framed := proto.NewConn(conn)
		if err := framed.WriteFrame(proto.MsgHello, &proto.Hello{
			Version: proto.ProtocolVersion, MachineID: testMachineID("2"), PID: 42,
		}); err != nil {
			wrongDone <- err
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _, readErr := framed.ReadFrame()
		if readErr == nil {
			wrongDone <- errors.New("client authenticated a mismatched machine")
			return
		}
		wrongDone <- nil
	}()
	if client, err := Dial(context.Background(), wrongListener.Addr().String(), token, machineID); err == nil || client != nil {
		if client != nil {
			_ = client.Close()
		}
		t.Fatalf("Dial accepted mismatched Hello: client=%#v err=%v", client, err)
	}
	if err := <-wrongDone; err != nil {
		t.Fatal(err)
	}
}

func TestAgentClientConcurrentCallsUseOneReaderAndMatchRequestIDs(t *testing.T) {
	transport := newTrackingFrameTransport()
	client := newClientForTransport(transport)
	t.Cleanup(func() { _ = client.Close() })

	type response struct {
		Value string `msgpack:"value"`
	}
	results := make(chan response, 2)
	errorsSeen := make(chan error, 2)
	for _, operation := range []string{proto.LocalOperationStatusGet, proto.LocalOperationConfigGet} {
		operation := operation
		go func() {
			var got response
			err := client.Call(context.Background(), operation, struct{}{}, &got)
			results <- got
			errorsSeen <- err
		}()
	}
	first := <-transport.requests
	second := <-transport.requests
	transport.respond(second.RequestID, response{Value: "second"})
	transport.respond(first.RequestID, response{Value: "first"})

	seen := map[string]bool{}
	for range 2 {
		if err := <-errorsSeen; err != nil {
			t.Fatalf("Call: %v", err)
		}
		seen[(<-results).Value] = true
	}
	if !seen["first"] || !seen["second"] {
		t.Fatalf("responses were not paired by request ID: %#v", seen)
	}
	if max := transport.maxReaders.Load(); max != 1 {
		t.Fatalf("concurrent ReadFrame calls = %d, want exactly one", max)
	}
}

func TestAgentClientDisconnectFailsEveryPendingCall(t *testing.T) {
	transport := newTrackingFrameTransport()
	client := newClientForTransport(transport)

	errorsSeen := make(chan error, 2)
	for range 2 {
		go func() {
			var response struct{}
			errorsSeen <- client.Call(context.Background(), proto.LocalOperationStatusGet, struct{}{}, &response)
		}()
	}
	<-transport.requests
	<-transport.requests
	transport.disconnect()
	for range 2 {
		if err := <-errorsSeen; !errors.Is(err, ErrAgentDisconnected) {
			t.Fatalf("pending Call error = %v, want %v", err, ErrAgentDisconnected)
		}
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close after disconnect: %v", err)
	}
}

type testReadFrame struct {
	messageType uint8
	body        []byte
}

type trackingFrameTransport struct {
	reads      chan testReadFrame
	requests   chan proto.LocalRequest
	done       chan struct{}
	closeOnce  sync.Once
	readers    atomic.Int32
	maxReaders atomic.Int32
}

func newTrackingFrameTransport() *trackingFrameTransport {
	return &trackingFrameTransport{
		reads: make(chan testReadFrame, 4), requests: make(chan proto.LocalRequest, 4), done: make(chan struct{}),
	}
}

func (t *trackingFrameTransport) ReadFrame() (uint8, []byte, error) {
	active := t.readers.Add(1)
	defer t.readers.Add(-1)
	for {
		maximum := t.maxReaders.Load()
		if active <= maximum || t.maxReaders.CompareAndSwap(maximum, active) {
			break
		}
	}
	select {
	case <-t.done:
		return 0, nil, io.EOF
	case frame := <-t.reads:
		return frame.messageType, frame.body, nil
	}
}

func (t *trackingFrameTransport) WriteFrame(messageType uint8, value any) error {
	if messageType != proto.MsgLocalRequest {
		return errors.New("unexpected client frame")
	}
	request, ok := value.(*proto.LocalRequest)
	if !ok {
		return errors.New("unexpected local request type")
	}
	copy := *request
	copy.Payload = append([]byte(nil), request.Payload...)
	select {
	case <-t.done:
		return io.ErrClosedPipe
	case t.requests <- copy:
		return nil
	}
}

func (t *trackingFrameTransport) Close() error {
	t.disconnect()
	return nil
}

func (t *trackingFrameTransport) disconnect() { t.closeOnce.Do(func() { close(t.done) }) }

func (t *trackingFrameTransport) respond(requestID string, value any) {
	payload, err := msgpack.Marshal(value)
	if err != nil {
		panic(err)
	}
	body, err := msgpack.Marshal(proto.LocalResponse{RequestID: requestID, OK: true, Payload: payload})
	if err != nil {
		panic(err)
	}
	t.reads <- testReadFrame{messageType: proto.MsgLocalResponse, body: body}
}

func listenAgentClientTestTCP(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

func testMachineID(fill string) string {
	value := "node-"
	for len(value) < len("node-")+64 {
		value += fill
	}
	return value
}
