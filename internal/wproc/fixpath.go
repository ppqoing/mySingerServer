package wproc

import (
	"path/filepath"
	"strings"
)

const longPathThreshold = 240

func fixPath(path string) string {
	if len(path) < longPathThreshold || strings.HasPrefix(path, `\\?\`) {
		return path
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	if strings.HasPrefix(absolute, `\\`) {
		return `\\?\UNC\` + strings.TrimPrefix(absolute, `\\`)
	}
	return `\\?\` + absolute
}
