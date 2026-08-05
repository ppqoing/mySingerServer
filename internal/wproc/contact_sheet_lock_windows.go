//go:build windows

package wproc

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

type contactSheetWindowsLock struct {
	file *os.File
}

func lockContactSheetPublish(jpeg string) (contactSheetPublishLock, error) {
	file, err := os.OpenFile(jpeg+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open contact sheet publish lock: %w", err)
	}
	overlapped := windows.Overlapped{}
	if err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &overlapped); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock contact sheet publish: %w", err)
	}
	return &contactSheetWindowsLock{file: file}, nil
}

func (lock *contactSheetWindowsLock) Release() error {
	overlapped := windows.Overlapped{}
	unlockErr := windows.UnlockFileEx(windows.Handle(lock.file.Fd()), 0, 1, 0, &overlapped)
	closeErr := lock.file.Close()
	if unlockErr != nil {
		return fmt.Errorf("unlock contact sheet publish: %w", unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close contact sheet publish lock: %w", closeErr)
	}
	return nil
}
