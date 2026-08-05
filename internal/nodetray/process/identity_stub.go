//go:build !windows

package process

import (
	"context"
	"errors"
)

type unsupportedInspector struct{}

func NewInspector() Inspector { return unsupportedInspector{} }

func UserSIDForProcess(Identity) (string, error) {
	return "", errors.New("process user SID requires Windows")
}

func (unsupportedInspector) Inspect(int) (Identity, error) {
	return Identity{}, errors.New("process identity inspection is only supported on Windows")
}

func (unsupportedInspector) Wait(context.Context, Identity) (int, error) {
	return 0, errors.New("process handle waiting is only supported on Windows")
}

func sameExecutablePath(left, right string) bool { return left == right }
