//go:build bindings

package main

import (
	"path/filepath"
	"testing"
)

func TestBindingsCompositionProvidesAbsoluteWebViewDataPath(t *testing.T) {
	backend, err := composeBackend()
	if err != nil {
		t.Fatalf("composeBackend: %v", err)
	}
	if backend == nil || !filepath.IsAbs(backend.webViewDataPath) {
		t.Fatalf("bindings WebView2 data path = %q, want absolute path", backend.webViewDataPath)
	}
}
