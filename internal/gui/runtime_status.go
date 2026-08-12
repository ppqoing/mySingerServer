package gui

import (
	"errors"
	"net"
	"syscall"

	"github.com/jackc/pgx/v5/pgconn"
)

type RuntimeFailure struct {
	Code    string
	Summary string
}

type RuntimeStatus struct {
	DatabaseState     string        `json:"database_state"`
	DatabaseErrorCode string        `json:"database_error_code"`
	Agents            []AgentStatus `json:"agents"`
	Restarting        bool          `json:"restarting"`
	RecoveryURL       string        `json:"recovery_url"`
}

func ClassifyRuntimeFailure(err error) RuntimeFailure {
	var pgError *pgconn.PgError
	if errors.As(err, &pgError) && pgError.Code == "28P01" {
		return RuntimeFailure{Code: "postgres_auth_failed", Summary: "PostgreSQL 认证失败"}
	}
	var networkError *net.OpError
	if errors.As(err, &networkError) && errors.Is(networkError.Err, syscall.ECONNREFUSED) {
		return RuntimeFailure{Code: "postgres_unreachable", Summary: "PostgreSQL 无法连接"}
	}
	return RuntimeFailure{Code: "postgres_unavailable", Summary: "PostgreSQL 不可用"}
}
