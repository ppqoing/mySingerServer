package agentdelete

import (
	"context"
	"errors"
	"net"

	"github.com/Microsoft/go-winio"
)

type PipeDialer struct {
	pipeName string
}

func NewPipeDialer(pipeName string) *PipeDialer {
	return &PipeDialer{pipeName: pipeName}
}

func (d *PipeDialer) Dial(ctx context.Context) (net.Conn, error) {
	if d == nil {
		return nil, errors.New("delete pipe dialer: nil dialer")
	}
	return winio.DialPipeContext(ctx, d.pipeName)
}
