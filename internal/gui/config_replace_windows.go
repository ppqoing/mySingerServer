//go:build windows

package gui

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func replaceFileAtomically(source, destination string) error {
	sourcePtr, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return fmt.Errorf("source path: %w", err)
	}
	destinationPtr, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return fmt.Errorf("destination path: %w", err)
	}
	return windows.MoveFileEx(
		sourcePtr,
		destinationPtr,
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	)
}
