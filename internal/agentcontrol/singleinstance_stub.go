//go:build !windows

package agentcontrol

import "errors"

var ErrAlreadyRunning = errors.New("Agent instance is already running")

type instanceLock struct{}

func AcquireSingleInstance(string) (*instanceLock, error) { return &instanceLock{}, nil }
func (*instanceLock) Close() error                        { return nil }
