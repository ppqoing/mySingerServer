//go:build windows

package enum

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

func StartEverythingClientAt(path string) error {
	command, err := newEverythingClientCommand(path)
	if err != nil {
		return err
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start Everything client %s: %w", command.Path, err)
	}
	if err := command.Process.Release(); err != nil {
		return fmt.Errorf("release Everything client process %s: %w", command.Path, err)
	}
	return nil
}

func newEverythingClientCommand(path string) (*exec.Cmd, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve Everything executable %s: %w", path, err)
	}
	info, err := os.Stat(absolutePath)
	if err != nil {
		return nil, fmt.Errorf("inspect Everything executable %s: %w", absolutePath, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("Everything executable is not a regular file: %s", absolutePath)
	}
	command := exec.Command(absolutePath, "-startup")
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return command, nil
}
