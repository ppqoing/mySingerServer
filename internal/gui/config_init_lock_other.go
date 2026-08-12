//go:build !windows

package gui

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type guiConfigInitOtherLock struct {
	file *os.File
}

func lockGUIConfigInit(absolute string) (guiConfigInitLock, error) {
	path := filepath.Join(filepath.Dir(absolute), "."+filepath.Base(absolute)+".init.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open config initialization lock: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock config initialization: %w", err)
	}
	return &guiConfigInitOtherLock{file: file}, nil
}

func isGUIConfigInitTransientReadError(error) bool {
	return false
}

func (lock *guiConfigInitOtherLock) Release() error {
	unlockErr := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	closeErr := lock.file.Close()
	if unlockErr != nil {
		return fmt.Errorf("unlock config initialization: %w", unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close config initialization lock: %w", closeErr)
	}
	return nil
}
