package stats

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"time"
)

func newPprofMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}

func StartPprof(ctx context.Context, address string, log *slog.Logger) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("pprof address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("pprof address must be loopback")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("pprof listen: %w", err)
	}
	server := &http.Server{
		Handler:           newPprofMux(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil && log != nil {
			log.Warn("pprof shutdown failed", "err", err)
		}
	}()
	go func() {
		if err := server.Serve(listener); err != nil &&
			err != http.ErrServerClosed && log != nil {
			log.Warn("pprof server failed", "err", err)
		}
	}()
	if log != nil {
		log.Info("pprof listening", "addr", listener.Addr().String())
	}
	return nil
}
