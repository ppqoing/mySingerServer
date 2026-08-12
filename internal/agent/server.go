package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"reflect"
	"sync"
	"time"
	"unicode/utf8"

	"dedup/internal/config"
	"dedup/internal/proto"
)

type ScanHandler interface {
	Prepare(task proto.ScanTask, sender Sender) (proto.TaskAck, func())
}

type Phase2Handler interface {
	Prepare(task proto.Phase2Task, sender Sender) (proto.TaskAck, func())
}

type DeleteHandler interface {
	Handle(context.Context, proto.DeleteTask, Sender) error
}

type StatsProvider interface {
	Stats(windowSeconds int) proto.StatsReport
}

type LocalHandler interface {
	HandleLocal(context.Context, proto.LocalRequest) proto.LocalResponse
}

type phase2ConnectionHandler interface {
	PrepareConnection(
		task proto.Phase2Task,
		sender Sender,
	) (proto.TaskAck, func(), func())
}

type phase2DisconnectHandler interface {
	PrepareConnectionWithDisconnect(
		task proto.Phase2Task,
		sender Sender,
		disconnect func(),
	) (proto.TaskAck, func(), func())
}

type Server struct {
	cfg    *config.AgentConfig
	sm     ScanHandler
	phase2 Phase2Handler
	log    *slog.Logger

	deleteMu          sync.RWMutex
	deleteHandler     DeleteHandler
	statsMu           sync.RWMutex
	statsProvider     StatsProvider
	heartbeatInterval time.Duration
	localMu           sync.RWMutex
	localToken        string
	localHandler      LocalHandler
}

func (s *Server) SetLocalControl(token string, handler LocalHandler) {
	s.localMu.Lock()
	s.localToken = token
	s.localHandler = handler
	s.localMu.Unlock()
}

func NewServer(
	cfg *config.AgentConfig,
	scans ScanHandler,
	log *slog.Logger,
	phase2 ...Phase2Handler,
) *Server {
	server := &Server{cfg: cfg, sm: scans, log: log}
	if len(phase2) != 0 {
		server.phase2 = phase2[0]
	}
	return server
}

func (s *Server) SetDeleteHandler(handler DeleteHandler) {
	s.deleteMu.Lock()
	s.deleteHandler = handler
	s.deleteMu.Unlock()
}

func (s *Server) SetStatsProvider(provider StatsProvider) {
	s.statsMu.Lock()
	s.statsProvider = provider
	s.statsMu.Unlock()
}

func (s *Server) heartbeat() time.Duration {
	if s.heartbeatInterval > 0 {
		return s.heartbeatInterval
	}
	return time.Duration(s.cfg.Proto.HeartbeatS) * time.Second
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return err
	}
	defer listener.Close()
	s.log.Info("agent listening", "addr", listener.Addr().String())
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	var connections sync.WaitGroup
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			s.log.Error("accept", "err", err)
			continue
		}
		connections.Add(1)
		go func(connection net.Conn) {
			defer connections.Done()
			s.handleConn(ctx, connection)
		}(connection)
	}
	connections.Wait()
	return nil
}

func (s *Server) handleConn(parent context.Context, connection net.Conn) {
	conn := proto.NewConn(connection)
	connectionContext, cancelConnection := context.WithCancel(parent)
	var closeOnce sync.Once
	closeConnection := func() {
		closeOnce.Do(func() { _ = connection.Close() })
	}
	parentWatchDone := make(chan struct{})
	go func() {
		defer close(parentWatchDone)
		select {
		case <-parent.Done():
			cancelConnection()
			closeConnection()
		case <-connectionContext.Done():
		}
	}()
	var heartbeatDone <-chan struct{}
	defer func() {
		cancelConnection()
		closeConnection()
		<-parentWatchDone
		if heartbeatDone != nil {
			<-heartbeatDone
		}
	}()

	remote := connection.RemoteAddr().String()
	hostname, _ := os.Hostname()
	if err := conn.WriteFrame(proto.MsgHello, &proto.Hello{
		Version:   proto.ProtocolVersion,
		MachineID: s.cfg.MachineID,
		Hostname:  hostname,
		PID:       os.Getpid(),
	}); err != nil {
		return
	}
	s.log.Info("gui connected", "remote", remote)
	defer s.log.Info("gui disconnected", "remote", remote)

	heartbeatFinished := make(chan struct{})
	heartbeatDone = heartbeatFinished
	go func() {
		defer close(heartbeatFinished)
		if err := heartbeat(connectionContext, conn, s.heartbeat()); err != nil {
			cancelConnection()
			closeConnection()
		}
	}()
	sender := func(msgType uint8, value any) error {
		return conn.WriteFrame(msgType, value)
	}
	var detachPhase2 []func()
	authenticatedNodeTray := false
	defer func() {
		for _, detach := range detachPhase2 {
			detach()
		}
	}()

	for {
		_ = conn.SetReadDeadline(time.Now().Add(3 * s.heartbeat()))
		msgType, body, err := conn.ReadFrame()
		if err != nil {
			if !errors.Is(err, io.EOF) && connectionContext.Err() == nil {
				s.log.Warn("conn closed", "remote", remote, "err", err)
			}
			return
		}
		message, err := proto.Decode(msgType, body)
		if err != nil {
			_ = sender(proto.MsgError, &proto.Error{
				Stage: "proto", Msg: err.Error(),
			})
			continue
		}
		switch value := message.(type) {
		case *proto.ClientAuth:
			authenticatedNodeTray = false
			result := proto.ClientAuthResult{}
			if !isLoopbackRemote(connection.RemoteAddr()) {
				result.ErrorCode = "local_only"
			} else {
				token, _ := s.localControl()
				if value.Role == "nodetray" &&
					value.Version == proto.ProtocolVersion &&
					token != "" &&
					constantTimeTokenEqual(value.Token, token) {
					authenticatedNodeTray = true
					result.Accepted = true
				} else {
					result.ErrorCode = "unauthorized"
				}
			}
			if err := sender(proto.MsgClientAuthResult, &result); err != nil {
				return
			}
		case *proto.LocalRequest:
			if !authenticatedNodeTray {
				_ = sender(proto.MsgLocalResponse, &proto.LocalResponse{
					RequestID: value.RequestID,
					ErrorCode: "unauthorized",
				})
				continue
			}
			if err := value.Validate(); err != nil {
				_ = sender(proto.MsgLocalResponse, &proto.LocalResponse{
					RequestID: value.RequestID,
					ErrorCode: err.Error(),
				})
				continue
			}
			token, handler := s.localControl()
			if handler == nil {
				_ = sender(proto.MsgLocalResponse, &proto.LocalResponse{
					RequestID: value.RequestID,
					ErrorCode: "local_unavailable",
				})
				continue
			}
			response := handler.HandleLocal(connectionContext, *value)
			response.RequestID = value.RequestID
			response = protectLocalResponse(response, token)
			if err := response.Validate(); err != nil {
				response = proto.LocalResponse{
					RequestID: value.RequestID,
					ErrorCode: err.Error(),
				}
			}
			if err := sender(proto.MsgLocalResponse, &response); err != nil {
				return
			}
		case *proto.Shutdown:
			message := "unauthorized"
			if authenticatedNodeTray {
				message = proto.UnsupportedOperationErrorCode
			}
			_ = sender(proto.MsgError, &proto.Error{Stage: "local", Msg: message})
		case *proto.Ping:
			_ = sender(proto.MsgPong, &proto.Pong{TS: value.TS})
		case *proto.Pong:
			// A Pong is only liveness evidence; no application action.
		case *proto.StatsQuery:
			provider := s.currentStatsProvider()
			if provider == nil {
				_ = sender(proto.MsgError, &proto.Error{
					Stage: "stats", Msg: "statistics unavailable",
				})
				continue
			}
			window := value.WindowSeconds
			if window < 1 {
				window = 1
			}
			if window > 300 {
				window = 300
			}
			report := provider.Stats(window)
			if err := sender(proto.MsgStatsReport, &report); err != nil {
				return
			}
		case *proto.DeleteTask:
			handler := s.currentDeleteHandler()
			if isNilDeleteHandler(handler) {
				report := unavailableDeleteReport(*value)
				_ = sender(proto.MsgDeleteReport, &report)
				return
			}
			task := *value
			task.Entries = append([]string(nil), value.Entries...)
			if err := handler.Handle(connectionContext, task, sender); err != nil {
				s.log.Warn(
					"delete handler failed",
					"task_id", boundedDeleteLogTaskID(task.TaskID),
					"category", "delete_handler_error",
				)
				return
			}
		case *proto.ScanTask:
			ack, start := s.sm.Prepare(*value, sender)
			if err := sender(proto.MsgTaskAck, &ack); err != nil {
				return
			}
			if start != nil {
				start()
			}
		case *proto.Phase2Task:
			if s.phase2 == nil {
				_ = sender(proto.MsgError, &proto.Error{
					Stage: "proto", Msg: "unsupported in M1",
				})
				continue
			}
			var ack proto.TaskAck
			var start, detach func()
			if disconnectHandler, ok := s.phase2.(phase2DisconnectHandler); ok {
				ack, start, detach = disconnectHandler.
					PrepareConnectionWithDisconnect(
						*value,
						sender,
						closeConnection,
					)
			} else if connectionHandler, ok := s.phase2.(phase2ConnectionHandler); ok {
				ack, start, detach = connectionHandler.PrepareConnection(
					*value,
					sender,
				)
			} else {
				ack, start = s.phase2.Prepare(*value, sender)
			}
			if detach != nil {
				detachPhase2 = append(detachPhase2, detach)
			}
			if err := sender(proto.MsgTaskAck, &ack); err != nil {
				if detach != nil {
					detach()
				}
				if start != nil {
					start()
				}
				return
			}
			if start != nil {
				start()
			}
		default:
			_ = sender(proto.MsgError, &proto.Error{
				Stage: "proto", Msg: "unsupported in M1",
			})
		}
	}
}

func constantTimeTokenEqual(provided, expected string) bool {
	providedHash := sha256.Sum256([]byte(provided))
	expectedHash := sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(providedHash[:], expectedHash[:]) == 1
}

func (s *Server) localControl() (string, LocalHandler) {
	s.localMu.RLock()
	defer s.localMu.RUnlock()
	return s.localToken, s.localHandler
}

func isLoopbackRemote(address net.Addr) bool {
	if address == nil {
		return false
	}
	if tcpAddress, ok := address.(*net.TCPAddr); ok {
		return tcpAddress.IP.IsLoopback()
	}
	host, _, err := net.SplitHostPort(address.String())
	if err != nil {
		return false
	}
	return net.ParseIP(host).IsLoopback()
}

func protectLocalResponse(response proto.LocalResponse, token string) proto.LocalResponse {
	if token == "" {
		return response
	}
	if bytes.Contains([]byte(response.RequestID), []byte(token)) {
		response.RequestID = ""
	}
	if bytes.Contains([]byte(response.ErrorCode), []byte(token)) ||
		bytes.Contains(response.Payload, []byte(token)) {
		response.OK = false
		response.ErrorCode = "internal_error"
		response.Payload = nil
	}
	return response
}

func (s *Server) currentStatsProvider() StatsProvider {
	s.statsMu.RLock()
	defer s.statsMu.RUnlock()
	return s.statsProvider
}

const maxDeleteLogTaskIDBytes = 128

func boundedDeleteLogTaskID(taskID string) string {
	if len(taskID) <= maxDeleteLogTaskIDBytes {
		return taskID
	}
	end := maxDeleteLogTaskIDBytes
	for end > 0 && !utf8.ValidString(taskID[:end]) {
		end--
	}
	return taskID[:end] + "..."
}

func (s *Server) currentDeleteHandler() DeleteHandler {
	s.deleteMu.RLock()
	defer s.deleteMu.RUnlock()
	return s.deleteHandler
}

func isNilDeleteHandler(handler DeleteHandler) bool {
	if handler == nil {
		return true
	}
	value := reflect.ValueOf(handler)
	switch value.Kind() {
	case reflect.Chan,
		reflect.Func,
		reflect.Interface,
		reflect.Map,
		reflect.Pointer,
		reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func unavailableDeleteReport(task proto.DeleteTask) proto.DeleteReport {
	results := make([]proto.DeleteResult, len(task.Entries))
	for index, path := range task.Entries {
		results[index] = proto.DeleteResult{
			Path:      path,
			ErrCode:   proto.DeleteErrHelperLost,
			Err:       "Agent delete handler unavailable",
			Uncertain: false,
		}
	}
	return proto.DeleteReport{
		TaskID:  task.TaskID,
		Seq:     task.Seq,
		LastSeq: task.LastSeq,
		Stats: proto.DeleteStats{
			Total:  len(results),
			Failed: len(results),
		},
		Entries: results,
	}
}

func heartbeat(
	ctx context.Context,
	conn *proto.Conn,
	interval time.Duration,
) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			if err := conn.WriteFrame(
				proto.MsgPing,
				&proto.Ping{TS: now.UnixMilli()},
			); err != nil {
				return err
			}
		}
	}
}
