//go:build !windows

package loginstart

import "errors"

var errWindowsRequired = errors.New("loginstart requires Windows")

type Service interface {
	Enabled() (bool, string, error)
	Enable(executable string) error
	Disable() error
}

func New(string) (Service, error) {
	return nil, errWindowsRequired
}
