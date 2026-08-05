package enum

import (
	"path/filepath"
	"strings"
)

type FileRecord struct {
	Path  string
	Size  int64
	MTime int64
}

type Enumerator interface {
	Name() string
	Available() error
	Enum(root string, visit func(FileRecord) error) error
}

func longPath(path string) string {
	if len(path) < 248 || strings.HasPrefix(path, `\\?\`) {
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

func cleanPath(path string) string {
	if strings.HasPrefix(path, `\\?\UNC\`) {
		return `\\` + strings.TrimPrefix(path, `\\?\UNC\`)
	}
	return strings.TrimPrefix(path, `\\?\`)
}

func pathWithinRoot(path, root string) bool {
	path = strings.TrimRight(cleanPath(path), `\/`)
	root = strings.TrimRight(cleanPath(root), `\/`)
	if strings.EqualFold(path, root) {
		return true
	}
	if len(path) <= len(root) || !strings.EqualFold(path[:len(root)], root) {
		return false
	}
	return path[len(root)] == '\\' || path[len(root)] == '/'
}
