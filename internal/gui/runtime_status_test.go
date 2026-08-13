package gui

import (
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestClassifyRuntimeFailureReturnsOnlyStableCodesAndChineseSummaries(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{err: &pgconn.PgError{Code: "28P01", Message: "password authentication failed"}, want: "postgres_auth_failed"},
		{err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, want: "postgres_unreachable"},
	}
	for _, test := range tests {
		status := ClassifyRuntimeFailure(test.err)
		if status.Code != test.want || strings.Contains(status.Summary, "password") || status.Summary == "" {
			t.Fatalf("unsafe status: %#v", status)
		}
	}
}

func TestClassifyRuntimeFailureMapsPostgresNotConfigured(t *testing.T) {
	status := ClassifyRuntimeFailure(ErrPostgresNotConfigured)
	if status.Code != "postgres_not_configured" || status.Summary != "PostgreSQL 尚未配置" {
		t.Fatalf("status = %#v", status)
	}
	if !errors.Is(ErrPostgresNotConfigured, ErrPostgresNotConfigured) {
		t.Fatal("unconfigured error must remain comparable")
	}
}
