//go:build !windows

package wproc

import "path/filepath"

func contactSheetCanonicalDirectory(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}
