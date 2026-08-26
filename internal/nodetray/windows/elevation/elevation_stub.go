//go:build !windows

package elevation

import (
	"context"
	"errors"

	nodeprocess "dedup/internal/nodetray/process"
)

var ErrWindowsRequired = errors.New("elevation: Windows is required")

type InvocationResult struct {
	Response     Response
	UACCancelled bool
}

type Handler interface {
	Execute(context.Context, Request) Response
}

type Client struct{}

func NewClient(string, nodeprocess.Inspector) (*Client, error) {
	return nil, ErrWindowsRequired
}

func (*Client) Invoke(context.Context, Action, []byte) (InvocationResult, error) {
	return InvocationResult{}, ErrWindowsRequired
}
