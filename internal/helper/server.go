package helper

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"dedup/internal/proto"
)

const HelperRole = "delete-helper"

type Server struct {
	cfg       Config
	listener  net.Listener
	processor *Processor
	logger    *slog.Logger

	activeMu sync.Mutex
	active   net.Conn

	listening atomic.Bool

	requestMu      sync.Mutex
	activeRequests int
	stopping       bool
}

func NewServer(cfg Config, listener net.Listener, processor *Processor, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Server{
		cfg:       cfg,
		listener:  listener,
		processor: processor,
		logger:    logger,
	}
}

func (s *Server) Serve(ctx context.Context) error {
	s.listening.Store(true)
	defer s.listening.Store(false)
	cancelWatch := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			s.listening.Store(false)
			_ = s.listener.Close()
			s.markStoppingAndCloseIfIdle()
		case <-cancelWatch:
		}
	}()
	defer close(cancelWatch)
	defer func() {
		_ = s.listener.Close()
		s.closeActive()
	}()

	s.logger.Info("helper server started",
		"event", "server_started",
		"allowed_root_count", len(s.cfg.AllowedRoots),
		"read_timeout_seconds", s.cfg.FrameReadTimeoutSec,
		"write_timeout_seconds", s.cfg.FrameWriteTimeoutSec,
	)

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			s.logger.Warn("helper connection rejected",
				"event", "accept_failed",
				"reason", "listener_error",
			)
			continue
		}

		s.setActive(conn)
		if ctx.Err() != nil {
			s.clearActive(conn)
			_ = conn.Close()
			return nil
		}
		shutdown := s.serveConnection(ctx, conn)
		s.clearActive(conn)
		_ = conn.Close()
		if shutdown {
			return nil
		}
	}
}

func (s *Server) serveConnection(ctx context.Context, conn net.Conn) bool {
	framed := proto.NewConn(conn)
	framed.SetWriteTimeout(time.Duration(s.cfg.FrameWriteTimeoutSec) * time.Second)

	if err := framed.WriteFrame(proto.MsgHello, proto.Hello{
		Version: proto.ProtocolVersion,
		PID:     os.Getpid(),
		Role:    HelperRole,
	}); err != nil {
		s.logger.Warn("helper connection closed",
			"event", "hello_write_failed",
			"reason", "write_error",
		)
		return false
	}

	for {
		if err := conn.SetReadDeadline(time.Now().Add(time.Duration(s.cfg.FrameReadTimeoutSec) * time.Second)); err != nil {
			s.logger.Warn("helper connection closed",
				"event", "read_deadline_failed",
				"reason", "deadline_error",
			)
			return false
		}

		messageType, body, err := framed.ReadFrame()
		if err != nil {
			s.logger.Info("helper connection closed",
				"event", "frame_read_stopped",
				"reason", "read_error_or_disconnect",
			)
			return false
		}

		s.logger.Info("helper frame received",
			"event", "frame_received",
			"message_type", int(messageType),
			"body_bytes", len(body),
		)

		message, err := proto.Decode(messageType, body)
		if err != nil {
			s.logger.Warn("helper frame rejected",
				"event", "frame_rejected",
				"message_type", int(messageType),
				"reason", "malformed_or_unknown",
			)
			return false
		}

		switch value := message.(type) {
		case *proto.DeleteTask:
			if s.processor == nil {
				s.logger.Warn("helper frame rejected",
					"event", "frame_rejected",
					"message_type", int(messageType),
					"reason", "processor_unavailable",
				)
				return false
			}

			if !s.beginRequest() {
				return false
			}
			report, writeErr := func() (proto.DeleteReport, error) {
				defer s.finishRequest()
				report := s.processor.Process(context.WithoutCancel(ctx), *value)
				return report, framed.WriteFrame(proto.MsgDeleteReport, report)
			}()
			if writeErr != nil {
				s.logger.Warn("helper report write failed",
					"event", "report_write_failed",
					"reason", "write_error",
					"attempted_count", len(report.Entries),
				)
				return false
			}
			rejectionCounts := summarizeSecurityRejections(report.Entries)
			s.logger.Info("helper delete task completed",
				"event", "delete_completed",
				"result_count", len(report.Entries),
				"security_rejection_total", rejectionCounts.total,
				"security_rejection_bad_path_count", rejectionCounts.badPath,
				"security_rejection_path_denied_count", rejectionCounts.pathDenied,
				"security_rejection_not_confirmed_count", rejectionCounts.notConfirmed,
				"security_rejection_access_denied_count", rejectionCounts.accessDenied,
				"security_rejection_reparse_count", rejectionCounts.reparse,
				"security_rejection_bad_mode_count", rejectionCounts.badMode,
				"security_rejection_other_count", rejectionCounts.other,
			)
			if ctx.Err() != nil {
				return false
			}

		case *proto.Shutdown:
			// Legacy Agent compatibility stays on the delete protocol pipe.
			// Tray lifecycle shutdown uses the separate nodectl Helper pipe.
			s.logger.Info("helper shutdown requested", "event", "shutdown_requested")
			return true

		default:
			s.logger.Warn("helper frame rejected",
				"event", "frame_rejected",
				"message_type", int(messageType),
				"reason", "unsupported_message_type",
			)
			return false
		}
	}
}

// ActiveRequests returns the number of delete transactions currently being
// processed. An accepted but idle connection is not an active request.
func (s *Server) ActiveRequests() int {
	s.requestMu.Lock()
	defer s.requestMu.Unlock()
	return s.activeRequests
}

// Listening reports whether Serve is currently accepting delete connections.
func (s *Server) Listening() bool {
	return s.listening.Load()
}

type securityRejectionCounts struct {
	total        int
	badPath      int
	pathDenied   int
	notConfirmed int
	accessDenied int
	reparse      int
	badMode      int
	other        int
}

func summarizeSecurityRejections(
	entries []proto.DeleteResult,
) securityRejectionCounts {
	var counts securityRejectionCounts
	for _, entry := range entries {
		switch entry.ErrCode {
		case "":
			continue
		case proto.DeleteErrBadPath:
			counts.badPath++
		case proto.DeleteErrPathDenied:
			counts.pathDenied++
		case proto.DeleteErrNotConfirmed:
			counts.notConfirmed++
		case proto.DeleteErrAccessDenied:
			counts.accessDenied++
		case proto.DeleteErrReparse:
			counts.reparse++
		case proto.DeleteErrBadMode:
			counts.badMode++
		case proto.DeleteErrNotFound,
			proto.DeleteErrReadonly,
			proto.DeleteErrDeleteFailed,
			proto.DeleteErrRecycleFailed,
			proto.DeleteErrInUse,
			proto.DeleteErrHelperLost:
			continue
		default:
			counts.other++
		}
		counts.total++
	}
	return counts
}

func (s *Server) setActive(conn net.Conn) {
	s.activeMu.Lock()
	s.active = conn
	s.activeMu.Unlock()
}

func (s *Server) clearActive(conn net.Conn) {
	s.activeMu.Lock()
	if s.active == conn {
		s.active = nil
	}
	s.activeMu.Unlock()
}

func (s *Server) closeActive() {
	s.activeMu.Lock()
	conn := s.active
	s.activeMu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (s *Server) beginRequest() bool {
	s.requestMu.Lock()
	defer s.requestMu.Unlock()
	if s.stopping {
		return false
	}
	s.activeRequests++
	return true
}

func (s *Server) finishRequest() {
	s.requestMu.Lock()
	if s.activeRequests > 0 {
		s.activeRequests--
	}
	s.requestMu.Unlock()
}

func (s *Server) markStoppingAndCloseIfIdle() {
	// Keep this lock order stable: connection state first, request state second.
	// Request promotion only needs requestMu, so a fully accepted transaction
	// can become active while cancellation waits to inspect the connection.
	s.activeMu.Lock()
	s.requestMu.Lock()
	s.stopping = true
	conn := s.active
	idle := s.activeRequests == 0
	s.requestMu.Unlock()
	s.activeMu.Unlock()
	if idle && conn != nil {
		_ = conn.Close()
	}
}
