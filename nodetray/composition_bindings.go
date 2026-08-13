//go:build bindings

package main

import (
	"os"
	"path/filepath"

	trayapp "dedup/internal/nodetray/app"
)

// Wails executes the application under its private bindings build tag to
// reflect exported methods. This metadata-only service is excluded from every
// normal/debug/production binary and never constructs Windows dependencies or
// performs operating-system actions.
func init() {
	composeBackend = func() (*Backend, error) {
		backend := NewBackend(trayapp.NewService(trayapp.Dependencies{}))
		backend.webViewDataPath = filepath.Join(os.TempDir(), "mysingerserver-wails-bindings")
		return backend, nil
	}
}
