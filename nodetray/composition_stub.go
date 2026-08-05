//go:build !windows && !bindings

package main

func init() {
	composeBackend = func() (*Backend, error) { return nil, errCompositionUnavailable }
	runElevatedOnce = func(string, string) error { return errCompositionUnavailable }
}
