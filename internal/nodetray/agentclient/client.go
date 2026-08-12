package agentclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sync"
	"time"

	"dedup/internal/proto"
)

var ErrAgentDisconnected = errors.New("agent_disconnected")

var stableRemoteCode = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

type frameTransport interface {
	ReadFrame() (uint8, []byte, error)
	WriteFrame(uint8, any) error
	Close() error
}

type callResult struct {
	response proto.LocalResponse
	err      error
}

type Client struct {
	transport frameTransport
	writeMu   sync.Mutex
	mu        sync.Mutex
	pending   map[string]chan callResult
	closed    bool
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

type RemoteError struct{ Code string }

func (e *RemoteError) Error() string {
	if e == nil || !stableRemoteCode.MatchString(e.Code) {
		return "agent_request_failed"
	}
	return e.Code
}

func Dial(ctx context.Context, endpoint, token, machineID string) (*Client, error) {
	if ctx == nil {
		return nil, errors.New("agent_connect_failed")
	}
	if err := validateLoopbackEndpoint(endpoint); err != nil || token == "" || machineID == "" {
		return nil, errors.New("agent_connect_failed")
	}
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "tcp", endpoint)
	if err != nil {
		return nil, contextOrDisconnected(ctx)
	}
	keep := false
	defer func() {
		if !keep {
			_ = connection.Close()
		}
	}()
	stopCancellation := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopCancellation()
	if deadline, ok := ctx.Deadline(); ok {
		if err := connection.SetDeadline(deadline); err != nil {
			return nil, contextOrDisconnected(ctx)
		}
	}

	framed := proto.NewConn(connection)
	messageType, body, err := framed.ReadFrame()
	if err != nil {
		return nil, contextOrDisconnected(ctx)
	}
	decoded, err := proto.Decode(messageType, body)
	if err != nil {
		return nil, errors.New("agent_handshake_failed")
	}
	hello, ok := decoded.(*proto.Hello)
	if !ok || hello.Version != proto.ProtocolVersion || hello.MachineID != machineID {
		return nil, errors.New("agent_identity_mismatch")
	}
	if err := framed.WriteFrame(proto.MsgClientAuth, &proto.ClientAuth{
		Role: "nodetray", Token: token, Version: proto.ProtocolVersion,
	}); err != nil {
		return nil, contextOrDisconnected(ctx)
	}
	messageType, body, err = framed.ReadFrame()
	if err != nil {
		return nil, contextOrDisconnected(ctx)
	}
	decoded, err = proto.Decode(messageType, body)
	if err != nil {
		return nil, errors.New("agent_handshake_failed")
	}
	auth, ok := decoded.(*proto.ClientAuthResult)
	if !ok || !auth.Accepted {
		return nil, errors.New("agent_auth_failed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := connection.SetDeadline(time.Time{}); err != nil {
		return nil, contextOrDisconnected(ctx)
	}
	client := newClientForTransport(framed)
	keep = true
	return client, nil
}

func newClientForTransport(transport frameTransport) *Client {
	client := &Client{
		transport: transport,
		pending:   make(map[string]chan callResult),
		done:      make(chan struct{}),
	}
	go client.readLoop()
	return client
}

func (c *Client) Call(ctx context.Context, operation string, request, response any) error {
	if ctx == nil {
		return errors.New("agent_request_failed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if c == nil || c.transport == nil {
		return ErrAgentDisconnected
	}
	payload := []byte(nil)
	var err error
	if request != nil {
		payload, err = proto.EncodeLocalPayload(request)
		if err != nil {
			return errors.New("agent_request_failed")
		}
	}
	requestID, err := newRequestID()
	if err != nil {
		return errors.New("agent_request_failed")
	}
	envelope := proto.LocalRequest{RequestID: requestID, Operation: operation, Payload: payload}
	if err := envelope.Validate(); err != nil {
		return &RemoteError{Code: err.Error()}
	}
	result := make(chan callResult, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrAgentDisconnected
	}
	c.pending[requestID] = result
	c.mu.Unlock()

	c.writeMu.Lock()
	writeErr := c.transport.WriteFrame(proto.MsgLocalRequest, &envelope)
	c.writeMu.Unlock()
	if writeErr != nil {
		c.disconnect(ErrAgentDisconnected)
	}
	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, requestID)
		c.mu.Unlock()
		return ctx.Err()
	case outcome := <-result:
		if outcome.err != nil {
			return outcome.err
		}
		if !outcome.response.OK {
			code := outcome.response.ErrorCode
			if !stableRemoteCode.MatchString(code) {
				code = "agent_request_failed"
			}
			return &RemoteError{Code: code}
		}
		if response == nil || len(outcome.response.Payload) == 0 {
			return nil
		}
		if err := proto.DecodeLocalPayload(outcome.response.Payload, response); err != nil {
			return errors.New("agent_response_invalid")
		}
		return nil
	}
}

func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	c.disconnect(ErrAgentDisconnected)
	return c.closeErr
}

func (c *Client) readLoop() {
	for {
		messageType, body, err := c.transport.ReadFrame()
		if err != nil {
			c.disconnect(ErrAgentDisconnected)
			return
		}
		decoded, err := proto.Decode(messageType, body)
		if err != nil {
			c.disconnect(ErrAgentDisconnected)
			return
		}
		switch value := decoded.(type) {
		case *proto.LocalResponse:
			if err := value.Validate(); err != nil {
				c.disconnect(ErrAgentDisconnected)
				return
			}
			c.mu.Lock()
			pending := c.pending[value.RequestID]
			delete(c.pending, value.RequestID)
			c.mu.Unlock()
			if pending != nil {
				pending <- callResult{response: *value}
			}
		case *proto.Ping:
			c.writeMu.Lock()
			err := c.transport.WriteFrame(proto.MsgPong, &proto.Pong{TS: value.TS})
			c.writeMu.Unlock()
			if err != nil {
				c.disconnect(ErrAgentDisconnected)
				return
			}
		case *proto.LocalEvent:
			// Event consumption is added with the local task UI. A single reader
			// owns the stream now so responses cannot race future subscriptions.
		default:
			c.disconnect(ErrAgentDisconnected)
			return
		}
	}
}

func (c *Client) disconnect(reason error) {
	if reason == nil {
		reason = ErrAgentDisconnected
	}
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		pending := c.pending
		c.pending = make(map[string]chan callResult)
		c.mu.Unlock()
		c.closeErr = c.transport.Close()
		close(c.done)
		for _, waiter := range pending {
			waiter <- callResult{err: reason}
		}
	})
}

func newRequestID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func validateLoopbackEndpoint(endpoint string) error {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil || port == "" {
		return errors.New("invalid endpoint")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("endpoint is not loopback")
	}
	return nil
}

func contextOrDisconnected(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrAgentDisconnected
}
