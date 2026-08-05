package gui

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"sync"
	"time"

	"dedup/internal/config"
	"dedup/internal/machineid"
	"dedup/internal/proto"
)

type IdentityState string

const (
	IdentityPending  IdentityState = "pending"
	IdentityClaimed  IdentityState = "claimed"
	IdentityConflict IdentityState = "conflict"
)

type AgentStatus struct {
	MachineID     string        `json:"machine_id"`
	Addr          string        `json:"addr"`
	Online        bool          `json:"online"`
	IdentityState IdentityState `json:"identity_state"`
	LastErr       string        `json:"last_err,omitempty"`
}

type AgentConn struct {
	ep          config.AgentEndpoint
	log         *slog.Logger
	on          func(machineID string, conn *AgentConn, message any)
	onConnected func(machineID string)
	claim       func(*AgentConn, string) error
	release     func(*AgentConn, string)

	mu            sync.Mutex
	conn          *proto.Conn
	machineID     string
	online        bool
	identityState IdentityState
	lastErr       string
}

func newAgentConn(
	endpoint config.AgentEndpoint,
	logger *slog.Logger,
	onMessage func(string, *AgentConn, any),
) *AgentConn {
	return &AgentConn{
		ep: endpoint, log: logger, on: onMessage,
		identityState: IdentityPending,
		claim: func(_ *AgentConn, machineID string) error {
			if !machineid.Valid(machineID) {
				return fmt.Errorf("invalid agent machine_id %q", machineID)
			}
			return nil
		},
		release: func(*AgentConn, string) {},
	}
}

func (agent *AgentConn) Run(ctx context.Context, heartbeat time.Duration) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := agent.runOnce(ctx, heartbeat)
		wasOnline := agent.setOffline(err)
		if wasOnline {
			backoff = time.Second
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if !wasOnline && backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

func (agent *AgentConn) runOnce(
	ctx context.Context,
	heartbeat time.Duration,
) error {
	dialer := net.Dialer{Timeout: 10 * time.Second}
	networkConn, err := dialer.DialContext(ctx, "tcp", agent.ep.Addr)
	if err != nil {
		return err
	}
	conn := proto.NewConn(networkConn)
	defer conn.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	msgType, body, err := conn.ReadFrame()
	if err != nil {
		return err
	}
	message, err := proto.Decode(msgType, body)
	if err != nil {
		return err
	}
	hello, ok := message.(*proto.Hello)
	if !ok {
		return fmt.Errorf("expect Hello, got type=%d", msgType)
	}
	if hello.Version != proto.ProtocolVersion {
		return fmt.Errorf(
			"protocol version mismatch: agent=%d gui=%d",
			hello.Version,
			proto.ProtocolVersion,
		)
	}
	if err := agent.claimMachineID(hello.MachineID); err != nil {
		return err
	}
	defer agent.release(agent, hello.MachineID)

	agent.setOnline(conn)
	agent.log.Info(
		"agent connected",
		"machine_id", hello.MachineID,
		"addr", agent.ep.Addr,
	)
	if agent.onConnected != nil {
		agent.onConnected(hello.MachineID)
	}
	heartbeatContext, cancelHeartbeat := context.WithCancel(ctx)
	defer cancelHeartbeat()
	go proto.Heartbeat(heartbeatContext, conn, heartbeat)

	for {
		_ = conn.SetReadDeadline(time.Now().Add(3 * heartbeat))
		msgType, body, err = conn.ReadFrame()
		if err != nil {
			return err
		}
		message, err = proto.Decode(msgType, body)
		if err != nil {
			agent.log.Warn(
				"decode agent message",
				"machine_id", hello.MachineID,
				"err", err,
			)
			continue
		}
		if ping, ok := message.(*proto.Ping); ok {
			if err := conn.WriteFrame(proto.MsgPong, &proto.Pong{TS: ping.TS}); err != nil {
				return err
			}
			continue
		}
		if _, ok := message.(*proto.Pong); ok {
			continue
		}
		if agent.on != nil {
			agent.on(hello.MachineID, agent, message)
		}
	}
}

func (agent *AgentConn) claimMachineID(machineID string) error {
	if !machineid.Valid(machineID) {
		return fmt.Errorf("invalid agent machine_id %q", machineID)
	}
	if err := agent.claim(agent, machineID); err != nil {
		agent.mu.Lock()
		agent.machineID = machineID
		agent.identityState = IdentityConflict
		agent.lastErr = err.Error()
		agent.mu.Unlock()
		return err
	}
	agent.mu.Lock()
	agent.machineID = machineID
	agent.identityState = IdentityClaimed
	agent.lastErr = ""
	agent.mu.Unlock()
	return nil
}

func (agent *AgentConn) Send(msgType uint8, value any) error {
	agent.mu.Lock()
	conn := agent.conn
	agent.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("agent at %s offline", agent.ep.Addr)
	}
	return conn.WriteFrame(msgType, value)
}

func (agent *AgentConn) setOnline(conn *proto.Conn) {
	agent.mu.Lock()
	agent.conn = conn
	agent.online = true
	agent.lastErr = ""
	agent.mu.Unlock()
}

func (agent *AgentConn) setOffline(err error) bool {
	agent.mu.Lock()
	wasOnline := agent.online
	agent.conn = nil
	agent.online = false
	if err != nil && ctxErrorText(err) != "" {
		agent.lastErr = err.Error()
	}
	agent.mu.Unlock()
	return wasOnline
}

func ctxErrorText(err error) string {
	if err == nil || err == context.Canceled {
		return ""
	}
	return err.Error()
}

func (agent *AgentConn) status() AgentStatus {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	return AgentStatus{
		MachineID:     agent.machineID,
		Addr:          agent.ep.Addr,
		Online:        agent.online,
		IdentityState: agent.identityState,
		LastErr:       agent.lastErr,
	}
}

type Pool struct {
	byAddr      map[string]*AgentConn
	identityMu  sync.RWMutex
	byMachineID map[string]*AgentConn

	runMu      sync.Mutex
	runStarted bool
	runClosed  bool
	runCancel  context.CancelFunc
	runWG      sync.WaitGroup

	connectMu     sync.Mutex
	onConnect     func(context.Context, string)
	connectCtx    context.Context
	connectCancel context.CancelFunc
	connectClosed bool
	connectWG     sync.WaitGroup
}

func NewPool(
	endpoints []config.AgentEndpoint,
	logger *slog.Logger,
	onMessage func(string, *AgentConn, any),
) *Pool {
	connectCtx, connectCancel := context.WithCancel(context.Background())
	pool := &Pool{
		byAddr:        make(map[string]*AgentConn, len(endpoints)),
		byMachineID:   make(map[string]*AgentConn, len(endpoints)),
		connectCtx:    connectCtx,
		connectCancel: connectCancel,
	}
	for _, endpoint := range endpoints {
		conn := newAgentConn(endpoint, logger, onMessage)
		conn.onConnected = pool.notifyConnected
		conn.claim = pool.claimIdentity
		conn.release = pool.releaseIdentity
		pool.byAddr[endpoint.Addr] = conn
	}
	return pool
}

func (pool *Pool) SetOnConnect(callback func(machineID string)) {
	if callback == nil {
		pool.SetOnConnectContext(nil)
		return
	}
	pool.SetOnConnectContext(func(_ context.Context, machineID string) {
		callback(machineID)
	})
}

func (pool *Pool) SetOnConnectContext(
	callback func(context.Context, string),
) {
	pool.connectMu.Lock()
	pool.onConnect = callback
	pool.connectMu.Unlock()
}

func (pool *Pool) notifyConnected(machineID string) {
	pool.connectMu.Lock()
	if pool.connectClosed || pool.onConnect == nil {
		pool.connectMu.Unlock()
		return
	}
	callback := pool.onConnect
	ctx := pool.connectCtx
	pool.connectWG.Add(1)
	pool.connectMu.Unlock()
	go func() {
		defer pool.connectWG.Done()
		callback(ctx, machineID)
	}()
}

func (pool *Pool) Start(ctx context.Context, heartbeat time.Duration) {
	pool.runMu.Lock()
	if pool.runStarted || pool.runClosed {
		pool.runMu.Unlock()
		return
	}
	pool.runStarted = true
	runContext, cancelRun := context.WithCancel(ctx)
	pool.runCancel = cancelRun
	pool.runWG.Add(len(pool.byAddr))
	for _, conn := range pool.byAddr {
		conn := conn
		go func() {
			defer pool.runWG.Done()
			conn.Run(runContext, heartbeat)
		}()
	}
	context.AfterFunc(ctx, pool.StopReconnects)
	pool.runMu.Unlock()
}

// StopReconnects permanently closes Pool lifecycle admission, cancels Agent
// connections and reconnect callbacks, and waits for all admitted work.
func (pool *Pool) StopReconnects() {
	pool.runMu.Lock()
	if !pool.runClosed {
		pool.runClosed = true
		if pool.runCancel != nil {
			pool.runCancel()
		}
	}
	pool.connectMu.Lock()
	if !pool.connectClosed {
		pool.connectClosed = true
		pool.connectCancel()
	}
	pool.connectMu.Unlock()
	pool.runMu.Unlock()
	pool.runWG.Wait()
	pool.connectWG.Wait()
}

func (pool *Pool) Send(machineID string, msgType uint8, value any) error {
	pool.identityMu.RLock()
	conn, ok := pool.byMachineID[machineID]
	pool.identityMu.RUnlock()
	if !ok {
		return fmt.Errorf("unknown agent %q", machineID)
	}
	return conn.Send(msgType, value)
}

func (pool *Pool) IsOnline(machineID string) bool {
	pool.identityMu.RLock()
	conn, ok := pool.byMachineID[machineID]
	pool.identityMu.RUnlock()
	return ok && conn.status().Online
}

func (pool *Pool) Status() []AgentStatus {
	out := make([]AgentStatus, 0, len(pool.byAddr))
	for _, conn := range pool.byAddr {
		out = append(out, conn.status())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Online != out[j].Online {
			return out[i].Online
		}
		if out[i].MachineID != out[j].MachineID {
			return out[i].MachineID < out[j].MachineID
		}
		return out[i].Addr < out[j].Addr
	})
	return out
}

func (pool *Pool) claimIdentity(conn *AgentConn, machineID string) error {
	if !machineid.Valid(machineID) {
		return fmt.Errorf("invalid agent machine_id %q", machineID)
	}
	pool.identityMu.Lock()
	defer pool.identityMu.Unlock()
	if existing := pool.byMachineID[machineID]; existing != nil && existing != conn {
		return fmt.Errorf("identity conflict: machine_id %s already connected", machineID)
	}
	pool.byMachineID[machineID] = conn
	return nil
}

func (pool *Pool) releaseIdentity(conn *AgentConn, machineID string) {
	pool.identityMu.Lock()
	if pool.byMachineID[machineID] == conn {
		delete(pool.byMachineID, machineID)
	}
	pool.identityMu.Unlock()
}
