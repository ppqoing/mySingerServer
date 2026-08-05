//go:build !windows

package process

import (
	"context"
	"errors"
)

type PIDWaiter struct{}

func NewPIDWaiter() *PIDWaiter { return &PIDWaiter{} }

func (*PIDWaiter) WaitPIDGone(context.Context, int) error {
	return errors.New("pid waiting is only supported on Windows")
}
