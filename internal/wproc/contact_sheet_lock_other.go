//go:build !windows

package wproc

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

type contactSheetOtherLock struct {
	file *os.File
}

func lockContactSheetPublish(jpeg string) (contactSheetPublishLock, error) {
	file, err := os.OpenFile(jpeg+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open contact sheet publish lock: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock contact sheet publish: %w", err)
	}
	return &contactSheetOtherLock{file: file}, nil
}

func (lock *contactSheetOtherLock) Release() error {
	unlockErr := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	closeErr := lock.file.Close()
	if unlockErr != nil {
		return fmt.Errorf("unlock contact sheet publish: %w", unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close contact sheet publish lock: %w", closeErr)
	}
	return nil
}
