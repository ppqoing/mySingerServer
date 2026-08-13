//go:build !windows

package localcontrol

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func platformLoadOrCreate(path, candidate string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		keep := false
		defer func() {
			_ = file.Close()
			if !keep {
				_ = os.Remove(path)
			}
		}()
		if _, err := file.WriteString(candidate); err != nil {
			return "", err
		}
		if err := file.Sync(); err != nil {
			return "", err
		}
		keep = true
		return candidate, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return "", err
	}
	for attempt := 0; ; attempt++ {
		contents, readErr := os.ReadFile(path)
		if readErr == nil && len(contents) != 0 {
			return strings.TrimSpace(string(contents)), nil
		}
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return "", readErr
		}
		if attempt == 199 {
			if readErr != nil {
				return "", readErr
			}
			return "", errors.New("local control token file remained empty")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
