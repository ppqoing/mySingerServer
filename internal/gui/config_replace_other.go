//go:build !windows

package gui

import "os"

func replaceFileAtomically(source, destination string) error {
	return os.Rename(source, destination)
}
