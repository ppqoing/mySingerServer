package gui

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"dedup/internal/config"
	"dedup/internal/proto"
)

const (
	machineA = "node-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	machineB = "node-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestAgentConnValidatesHelloAndDispatchesMessages(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	release := make(chan struct{})
	serverErr := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer connection.Close()
		conn := proto.NewConn(connection)
		if err := conn.WriteFrame(proto.MsgHello, &proto.Hello{
			Version: proto.ProtocolVersion, MachineID: machineA,
		}); err != nil {
			serverErr <- err
			return
		}
		if err := conn.WriteFrame(proto.MsgTaskProgress, &proto.TaskProgress{
			TaskID: "task-1", Done: 2, Total: 4,
		}); err != nil {
			serverErr <- err
			return
		}
		<-release
		serverErr <- nil
	}()

	dispatched := make(chan any, 1)
	dispatchedMachine := make(chan string, 1)
	agent := newAgentConn(config.AgentEndpoint{
		Addr: listener.Addr().String(),
	}, testLogger(), func(machineID string, _ *AgentConn, message any) {
		dispatchedMachine <- machineID
		dispatched <- message
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- agent.runOnce(ctx, time.Minute) }()
	select {
	case message := <-dispatched:
		progress, ok := message.(*proto.TaskProgress)
		if !ok || progress.Done != 2 {
			t.Fatalf("message = %#v", message)
		}
		if got := <-dispatchedMachine; got != machineA {
			t.Fatalf("dispatched machine = %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("message was not dispatched")
	}
	status := agent.status()
	if !status.Online {
		t.Fatalf("status = %#v, want online", status)
	}
	close(release)
	select {
	case <-runErr:
	case <-time.After(3 * time.Second):
		t.Fatal("runOnce did not exit after peer closed")
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestAgentConnRejectsInvalidGeneratedMachineIdentity(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		_ = proto.NewConn(connection).WriteFrame(proto.MsgHello, &proto.Hello{
			Version: proto.ProtocolVersion, MachineID: "impostor",
		})
	}()
	agent := newAgentConn(config.AgentEndpoint{
		Addr: listener.Addr().String(),
	}, testLogger(), func(string, *AgentConn, any) {})
	if err := agent.runOnce(context.Background(), time.Minute); err == nil {
		t.Fatal("runOnce accepted an invalid generated machine_id")
	}
	if agent.status().Online {
		t.Fatal("invalid agent was marked online")
	}
	for _, value := range []string{"", "machine-a", "node-" + strings.Repeat("a", 63), "node-" + strings.Repeat("A", 64)} {
		if err := agent.claimMachineID(value); err == nil {
			t.Fatalf("claimMachineID accepted %q", value)
		}
	}
}

func TestAgentConnNotifiesPoolAfterValidHello(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	release := make(chan struct{})
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		_ = proto.NewConn(connection).WriteFrame(proto.MsgHello, &proto.Hello{
			Version: proto.ProtocolVersion, MachineID: machineB,
		})
		<-release
	}()

	connected := make(chan string, 1)
	agent := newAgentConn(config.AgentEndpoint{
		Addr: listener.Addr().String(),
	}, testLogger(), nil)
	agent.onConnected = func(machineID string) {
		connected <- machineID
	}
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- agent.runOnce(ctx, time.Minute) }()
	select {
	case machineID := <-connected:
		if machineID != machineB {
			t.Fatalf("connected machine = %q", machineID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("valid Hello did not trigger connect callback")
	}
	cancel()
	close(release)
	select {
	case <-runErr:
	case <-time.After(3 * time.Second):
		t.Fatal("runOnce did not stop")
	}
}

func TestPoolSetOnConnectReceivesAgentNotification(t *testing.T) {
	pool := NewPool([]config.AgentEndpoint{{
		Addr: "127.0.0.1:1",
	}}, testLogger(), nil)
	connected := make(chan string, 1)
	pool.SetOnConnect(func(machineID string) {
		connected <- machineID
	})
	pool.byAddr["127.0.0.1:1"].onConnected(machineA)
	select {
	case machineID := <-connected:
		if machineID != machineA {
			t.Fatalf("connected machine = %q", machineID)
		}
	case <-time.After(time.Second):
		t.Fatal("pool callback was not invoked")
	}
}

func TestPoolSetOnDisconnectReceivesClaimedConnectionNotification(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		_ = proto.NewConn(connection).WriteFrame(proto.MsgHello, &proto.Hello{
			Version: proto.ProtocolVersion, MachineID: machineA,
		})
	}()

	disconnected := make(chan string, 1)
	pool := NewPool([]config.AgentEndpoint{{Addr: listener.Addr().String()}}, testLogger(), nil)
	pool.SetOnDisconnect(func(machineID string) {
		disconnected <- machineID
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx, time.Minute)
	select {
	case machineID := <-disconnected:
		if machineID != machineA {
			t.Fatalf("disconnected machine=%q", machineID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("claimed connection did not notify disconnect")
	}
	pool.StopReconnects()
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("server did not finish")
	}
}

func TestPoolIdentityClaimConflictReleaseAndLateRelease(t *testing.T) {
	pool := NewPool([]config.AgentEndpoint{
		{Addr: "127.0.0.1:9101"},
		{Addr: "127.0.0.1:9102"},
	}, testLogger(), nil)
	first := pool.byAddr["127.0.0.1:9101"]
	second := pool.byAddr["127.0.0.1:9102"]
	for _, status := range pool.Status() {
		if status.MachineID != "" || status.Online || status.IdentityState != IdentityPending {
			t.Fatalf("initial status = %#v", status)
		}
	}

	if err := first.claimMachineID(machineA); err != nil {
		t.Fatal(err)
	}
	if err := second.claimMachineID(machineA); err == nil {
		t.Fatal("second connection claimed an occupied identity")
	}
	if status := second.status(); status.IdentityState != IdentityConflict || status.MachineID != machineA {
		t.Fatalf("conflicting status = %#v", status)
	}
	pool.identityMu.RLock()
	owner := pool.byMachineID[machineA]
	pool.identityMu.RUnlock()
	if owner != first {
		t.Fatal("identity conflict replaced the first owner")
	}

	pool.releaseIdentity(first, machineA)
	if err := second.claimMachineID(machineA); err != nil {
		t.Fatalf("second connection could not reclaim released identity: %v", err)
	}
	pool.releaseIdentity(first, machineA)
	pool.identityMu.RLock()
	owner = pool.byMachineID[machineA]
	pool.identityMu.RUnlock()
	if owner != second {
		t.Fatal("late release removed the new identity owner")
	}
}

func TestPoolReconnectCallbackIsAsyncCancellableAndWaited(t *testing.T) {
	pool := NewPool(nil, testLogger(), nil)
	started := make(chan struct{})
	pool.SetOnConnectContext(func(ctx context.Context, _ string) {
		close(started)
		<-ctx.Done()
	})
	returned := make(chan struct{})
	go func() {
		pool.notifyConnected(machineA)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("notifyConnected blocked behind reconnect sends")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("async reconnect callback did not start")
	}
	waited := make(chan struct{})
	go func() {
		pool.StopReconnects()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(time.Second):
		t.Fatal("StopReconnects did not cancel and wait for callback")
	}
}

func TestPoolStopWaitsForAgentRunIncludingBlockedMessageHandler(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		conn := proto.NewConn(connection)
		if conn.WriteFrame(proto.MsgHello, &proto.Hello{
			Version:   proto.ProtocolVersion,
			MachineID: machineA,
		}) != nil {
			return
		}
		_ = conn.WriteFrame(proto.MsgTaskProgress, &proto.TaskProgress{
			TaskID: "phase2-task",
			Done:   1,
			Total:  2,
		})
		buffer := make([]byte, 1)
		_, _ = connection.Read(buffer)
	}()

	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	pool := NewPool([]config.AgentEndpoint{{
		Addr: listener.Addr().String(),
	}}, testLogger(), func(string, *AgentConn, any) {
		close(handlerStarted)
		<-releaseHandler
	})
	ctx, cancel := context.WithCancel(context.Background())
	pool.Start(ctx, time.Minute)
	select {
	case <-handlerStarted:
	case <-time.After(3 * time.Second):
		cancel()
		close(releaseHandler)
		t.Fatal("agent message handler did not start")
	}

	cancel()
	stopped := make(chan struct{})
	go func() {
		pool.StopReconnects()
		close(stopped)
	}()
	select {
	case <-stopped:
		close(releaseHandler)
		t.Fatal("pool stopped while an Agent Run message handler was active")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseHandler)
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("pool did not finish after the message handler returned")
	}
	select {
	case <-serverDone:
	case <-time.After(3 * time.Second):
		t.Fatal("server connection did not close during pool shutdown")
	}
}

func TestPoolStartIsIdempotent(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 2)
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			accepted <- connection
		}
	}()

	pool := NewPool([]config.AgentEndpoint{{
		Addr: listener.Addr().String(),
	}}, testLogger(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx, time.Minute)
	pool.Start(ctx, time.Minute)
	var first net.Conn
	select {
	case first = <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("Pool.Start did not launch its Agent Run")
	}
	defer first.Close()
	select {
	case duplicate := <-accepted:
		duplicate.Close()
		t.Fatal("repeated Pool.Start launched a duplicate Agent Run")
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	pool.StopReconnects()
}

func TestPoolStopBeforeStartPermanentlyClosesRunAdmission(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- connection
		}
	}()

	pool := NewPool([]config.AgentEndpoint{{
		Addr: listener.Addr().String(),
	}}, testLogger(), nil)
	pool.StopReconnects()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx, time.Minute)
	select {
	case connection := <-accepted:
		connection.Close()
		t.Fatal("Pool.Start admitted Agent Run after shutdown")
	case <-time.After(100 * time.Millisecond):
	}
	pool.StopReconnects()
}

func TestPoolConcurrentRepeatedStartAndStop(t *testing.T) {
	for iteration := 0; iteration < 50; iteration++ {
		pool := NewPool([]config.AgentEndpoint{{
			Addr: "127.0.0.1:1",
		}}, testLogger(), nil)
		ctx, cancel := context.WithCancel(context.Background())
		start := make(chan struct{})
		var calls sync.WaitGroup
		for call := 0; call < 8; call++ {
			calls.Add(1)
			go func(call int) {
				defer calls.Done()
				<-start
				if call%2 == 0 {
					pool.Start(ctx, time.Minute)
					return
				}
				pool.StopReconnects()
			}(call)
		}
		close(start)
		calls.Wait()
		cancel()
		pool.StopReconnects()
	}
}
