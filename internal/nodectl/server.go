package nodectl

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
)

const maxConcurrentControlConnections = 16

type StatusProvider interface {
	ControlStatus() Status
}

type ShutdownFunc func()

// Serve handles one bounded control request per connection until the context
// is canceled or the listener is closed.
func Serve(ctx context.Context, ln net.Listener, provider StatusProvider, shutdown ShutdownFunc) error {
	if ln == nil {
		return errors.New("control listener is nil")
	}
	serveCtx, stop := context.WithCancel(ctx)

	listenerWatchDone := make(chan struct{})
	go func() {
		select {
		case <-serveCtx.Done():
			_ = ln.Close()
		case <-listenerWatchDone:
		}
	}()
	defer close(listenerWatchDone)

	semaphore := make(chan struct{}, maxConcurrentControlConnections)
	var handlers sync.WaitGroup
	var shutdownOnce sync.Once
	defer func() {
		stop()
		handlers.Wait()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept control connection: %w", err)
		}
		select {
		case semaphore <- struct{}{}:
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				defer func() { <-semaphore }()
				handleControlConnection(serveCtx, conn, provider, shutdown, &shutdownOnce)
			}()
		case <-serveCtx.Done():
			_ = conn.Close()
			return serveCtx.Err()
		}
	}
}

type uncheckedRequest Request

func handleControlConnection(ctx context.Context, conn net.Conn, provider StatusProvider, shutdown ShutdownFunc, shutdownOnce *sync.Once) {
	defer conn.Close()
	connectionWatchDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-connectionWatchDone:
		}
	}()
	defer close(connectionWatchDone)

	var wire uncheckedRequest
	if err := ReadFrame(conn, &wire); err != nil {
		return
	}
	request := Request(wire)
	requestID := responseRequestID(request.RequestID)
	if request.Version != ProtocolVersion || !validRequestID(request.RequestID) {
		_ = writeControlError(conn, requestID, "invalid_request", "invalid control request")
		return
	}
	switch request.Command {
	case CommandStatus:
		status, code, summary := controlStatus(provider)
		if code != "" {
			_ = writeControlError(conn, requestID, code, summary)
			return
		}
		response := Response{Version: ProtocolVersion, RequestID: requestID, OK: true, Status: &status}
		if err := WriteFrame(conn, response); err != nil {
			return
		}
	case CommandShutdown:
		response := Response{Version: ProtocolVersion, RequestID: requestID, OK: true}
		if err := WriteFrame(conn, response); err != nil {
			return
		}
		if shutdown != nil {
			shutdownOnce.Do(shutdown)
		}
	default:
		_ = writeControlError(conn, requestID, "unsupported_command", "unsupported control command")
	}
}

func controlStatus(provider StatusProvider) (status Status, code, summary string) {
	if provider == nil {
		return Status{}, "status_unavailable", "status provider unavailable"
	}
	defer func() {
		if recover() != nil {
			status = Status{}
			code = "internal_error"
			summary = "status provider failed"
		}
	}()
	status = provider.ControlStatus()
	if err := status.Validate(); err != nil {
		return Status{}, "status_unavailable", SanitizeSummary(err.Error())
	}
	return status, "", ""
}

func writeControlError(conn net.Conn, requestID, code, summary string) error {
	return WriteFrame(conn, Response{
		Version:      ProtocolVersion,
		RequestID:    requestID,
		OK:           false,
		ErrorCode:    code,
		ErrorSummary: SanitizeSummary(summary),
	})
}

func validRequestID(value string) bool {
	request := Request{Version: ProtocolVersion, RequestID: value, Command: CommandStatus}
	return request.Validate() == nil
}

func responseRequestID(value string) string {
	if validRequestID(value) {
		return value
	}
	return "invalid-request"
}
