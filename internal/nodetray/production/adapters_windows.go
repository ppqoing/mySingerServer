//go:build windows

package production

import (
	"context"
	"os/exec"
)

type commandExplorerBackend struct{}

func (commandExplorerBackend) Start(ctx context.Context, executable string, args []string) error {
	return exec.CommandContext(ctx, executable, args...).Start()
}

func nativeExplorerBackend() ExplorerBackend { return commandExplorerBackend{} }
