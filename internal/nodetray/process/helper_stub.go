//go:build !windows

package process

import (
	"context"
	"errors"
)

const SeeMaskNoCloseProcess uint32 = 0x00000040

type ShellExecuteRequest struct {
	Verb       string
	File       string
	Parameters string
	Mask       uint32
}

type ShellExecuteBackend interface {
	Execute(context.Context, ShellExecuteRequest) (uintptr, error)
}

type HandleInspector interface {
	InspectHandle(uintptr) (Identity, error)
}

type ManualHelperLauncher struct{}

func NewManualHelperLauncher(ShellExecuteBackend, HandleInspector) *ManualHelperLauncher {
	return &ManualHelperLauncher{}
}

func (*ManualHelperLauncher) Start(context.Context, string, []string, []string) (Identity, error) {
	return Identity{}, errors.New("elevated Helper launch is only supported on Windows")
}

func (*ManualHelperLauncher) StartHelper(context.Context, string, string) (Identity, error) {
	return Identity{}, errors.New("elevated Helper launch is only supported on Windows")
}
