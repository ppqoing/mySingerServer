//go:build windows

package enum

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func canonicalExistingPath(path string) (string, error) {
	absolute, err := filepath.Abs(cleanPath(path))
	if err != nil {
		return "", fmt.Errorf("enumerator: absolute path %q: %w", path, err)
	}
	pathPointer, err := windows.UTF16PtrFromString(absolute)
	if err != nil {
		return "", fmt.Errorf("enumerator: bad path %q: %w", path, err)
	}
	buffer := make([]uint16, 32768)
	length, err := windows.GetLongPathName(
		pathPointer,
		&buffer[0],
		uint32(len(buffer)),
	)
	if err != nil {
		return "", fmt.Errorf("enumerator: expand path %q: %w", path, err)
	}
	if length == 0 || length >= uint32(len(buffer)) {
		return "", fmt.Errorf(
			"enumerator: expanded path %q has invalid length %d",
			path,
			length,
		)
	}
	return filepath.Clean(windows.UTF16ToString(buffer[:length])), nil
}
