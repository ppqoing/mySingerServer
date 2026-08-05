package enum

import (
	"io/fs"
	"path/filepath"
)

type WalkerEnumerator struct {
	walkDir func(string, fs.WalkDirFunc) error
}

func (WalkerEnumerator) Name() string     { return "walker" }
func (WalkerEnumerator) Available() error { return nil }

func (w WalkerEnumerator) Enum(root string, visit func(FileRecord) error) error {
	canonicalRoot, err := canonicalExistingPath(root)
	if err != nil {
		return err
	}
	walkRoot := longPath(canonicalRoot)
	walkDir := w.walkDir
	if walkDir == nil {
		walkDir = filepath.WalkDir
	}
	return walkDir(walkRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return visit(FileRecord{
			Path:  cleanPath(path),
			Size:  info.Size(),
			MTime: info.ModTime().Unix(),
		})
	})
}
