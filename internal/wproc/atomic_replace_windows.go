//go:build windows

package wproc

import (
	"errors"
	"fmt"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

func atomicReplace(source, destination string) error {
	sourcePtr, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return fmt.Errorf("source path: %w", err)
	}
	destinationPtr, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return fmt.Errorf("destination path: %w", err)
	}
	for attempt := 0; ; attempt++ {
		err = windows.MoveFileEx(
			sourcePtr,
			destinationPtr,
			windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
		)
		if err == nil || attempt == 99 || !atomicReplaceTransientError(err) {
			return err
		}
		time.Sleep(time.Millisecond)
	}
}

func atomicReplaceTransientError(err error) bool {
	return errors.Is(err, syscall.Errno(5)) || errors.Is(err, syscall.Errno(32)) || errors.Is(err, syscall.Errno(33))
}
