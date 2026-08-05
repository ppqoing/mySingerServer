//go:build !windows

package singleinstance

import (
	"context"
	"errors"
)

var (
	ErrAlreadyExists      = errors.New("single instance already exists")
	ErrNoExistingInstance = errors.New("no existing tray instance")
	errWindowsRequired    = errors.New("singleinstance requires Windows")
)

type Lease interface {
	Close() error
}

func AcquireTray(string) (Lease, error) {
	return nil, errWindowsRequired
}

func AcquireAgent(string) (Lease, error) {
	return nil, errWindowsRequired
}

func ListenActivation(context.Context, func()) error {
	return errWindowsRequired
}

func SignalExisting(context.Context) error {
	return errWindowsRequired
}
