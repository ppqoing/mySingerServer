//go:build !windows

package main

import (
	"errors"
	"os"

	"dedup/internal/shared/finalpath"
)

func finalGUIExecutablePath() (string, error) {
	return resolveGUIExecutablePath(os.Executable, finalpath.ResolveExisting)
}

func openGUIBrowser(string) error {
	return errors.New("automatic browser launch is unsupported on this platform")
}
func showGUIStartupError(string) {}
