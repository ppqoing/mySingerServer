//go:build windows

package bootstrap

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

func resolveOSFinalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	volume := filepath.VolumeName(abs)
	if volume == "" {
		return "", errors.New("bootstrap: final path has no volume")
	}
	root := volume + string(filepath.Separator)
	relative := strings.TrimPrefix(abs, root)
	current := root
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				// Every existing ancestor has already been inspected. A missing
				// suffix is a valid first-run state; later writers still create
				// and protect their fixed directories through their own authority.
				return abs, nil
			}
			return "", statErr
		}
		data, ok := info.Sys().(*syscall.Win32FileAttributeData)
		if !ok || data == nil {
			return "", errors.New("bootstrap: final path metadata unavailable")
		}
		if data.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			resolved, evalErr := filepath.EvalSymlinks(abs)
			if evalErr != nil {
				return "", evalErr
			}
			return filepath.Abs(filepath.Clean(resolved))
		}
	}
	return abs, nil
}
