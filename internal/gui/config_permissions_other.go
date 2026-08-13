//go:build !windows

package gui

import "os"

func restrictGUIConfigPermissions(file *os.File) error {
	return file.Chmod(0o600)
}
