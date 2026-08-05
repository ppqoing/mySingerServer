//go:build !windows

package tray

func Start(Options) (Controller, error) { return nil, ErrUnavailable }
