//go:build windows

package finalpath

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// ResolveExisting returns the Windows final path of an existing filesystem
// object. The returned path is derived from the opened object, not its launch
// alias, so junctions, symbolic links, short names, and mapped drives cannot
// select a second portable root.
func ResolveExisting(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve absolute path: %w", err)
	}
	path16, err := windows.UTF16PtrFromString(filepath.Clean(abs))
	if err != nil {
		return "", errors.New("final path contains invalid characters")
	}
	handle, err := windows.CreateFile(
		path16,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		0,
		0,
	)
	if err != nil {
		return "", fmt.Errorf("open final path: %w", err)
	}
	defer windows.CloseHandle(handle)

	buffer := make([]uint16, 32768)
	n, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
	if err != nil {
		return "", fmt.Errorf("query final path: %w", err)
	}
	if n == 0 || n >= uint32(len(buffer)) {
		return "", errors.New("final path exceeds supported length")
	}
	resolved := windows.UTF16ToString(buffer[:n])
	if strings.HasPrefix(resolved, `\\?\UNC\`) {
		return `\\` + strings.TrimPrefix(resolved, `\\?\UNC\`), nil
	}
	return strings.TrimPrefix(resolved, `\\?\`), nil
}
