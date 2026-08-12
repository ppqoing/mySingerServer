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

func guiWaitForParent(int) error {
	return errors.New("waiting for a parent process is unsupported on this platform")
}

func guiStartReplacement(string, []string) error {
	return errors.New("restarting the GUI is unsupported on this platform")
}

func showGUIStartupError(string) {}
