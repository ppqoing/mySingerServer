//go:build windows

package wproc

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func contactSheetCanonicalDirectory(path string) (string, error) {
	attributes, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(path))
	if err != nil {
		return "", err
	}
	if contactSheetDirectoryHasReparsePoint(attributes) {
		return "", fmt.Errorf("contact sheet cache directory is a reparse point")
	}
	return filepath.EvalSymlinks(path)
}

func contactSheetDirectoryHasReparsePoint(attributes uint32) bool {
	return attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
}
