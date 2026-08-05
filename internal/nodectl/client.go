package nodectl

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
)

type Client struct {
	dial func(context.Context) (net.Conn, error)
}

func NewClient(dial func(context.Context) (net.Conn, error)) *Client {
	return &Client{dial: dial}
}

func (c *Client) Status(ctx context.Context) (Status, error) {
	response, err := c.request(ctx, CommandStatus)
	if err != nil {
		return Status{}, err
	}
	if response.Status == nil {
		return Status{}, errors.New("control status response did not include status")
	}
	return *response.Status, nil
}

func (c *Client) Shutdown(ctx context.Context) error {
	_, err := c.request(ctx, CommandShutdown)
	return err
}

func (c *Client) request(ctx context.Context, command Command) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	if c == nil || c.dial == nil {
		return Response{}, errors.New("control client has no dial function")
	}
	requestID, err := newRequestID()
	if err != nil {
		return Response{}, fmt.Errorf("generate control request ID: %w", err)
	}
	conn, err := c.dial(ctx)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return Response{}, fmt.Errorf("set control connection deadline: %w", err)
		}
	}
	cancelWatchDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-cancelWatchDone:
		}
	}()
	defer close(cancelWatchDone)

	request := Request{Version: ProtocolVersion, RequestID: requestID, Command: command}
	if err := WriteFrame(conn, request); err != nil {
		return Response{}, contextOrError(ctx, fmt.Errorf("write control request: %w", err))
	}
	var response Response
	if err := ReadFrame(conn, &response); err != nil {
		return Response{}, contextOrError(ctx, fmt.Errorf("read control response: %w", err))
	}
	if response.RequestID != requestID {
		return Response{}, fmt.Errorf("control response request ID mismatch: got %q", response.RequestID)
	}
	if !response.OK {
		return Response{}, fmt.Errorf("control request failed: %s: %s", response.ErrorCode, response.ErrorSummary)
	}
	return response, nil
}

func newRequestID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func contextOrError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}
