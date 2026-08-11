//go:build !windows

package main

import "errors"

func openGUIBrowser(string) error {
	return errors.New("automatic browser launch is unsupported on this platform")
}
func showGUIStartupError(string) {}
