//go:build !windows

package production

import (
	"context"
	"errors"
)

type failClosedExplorerBackend struct{}

func (failClosedExplorerBackend) Start(context.Context, string, []string) error {
	return errors.New("production location opener: unsupported platform")
}

func nativeExplorerBackend() ExplorerBackend { return failClosedExplorerBackend{} }
