//go:build windows

package config_test

import (
	"os"
	"path/filepath"
	"testing"

	trayconfig "dedup/internal/nodetray/config"
	"dedup/internal/nodetray/production"
)

func TestExtractedComputeLayoutWithoutHelperDirectoryCanInitializeProductionStore(t *testing.T) {
	root := t.TempDir()
	computeRoot := filepath.Join(root, "MySingerServer-Compute")
	for _, relative := range []string{"data/agent", "data/nodetray"} {
		if err := os.MkdirAll(filepath.Join(computeRoot, filepath.FromSlash(relative)), 0o755); err != nil {
			t.Fatalf("create extracted Compute fixture: %v", err)
		}
	}
	layout, err := production.ResolvePortableLayout(filepath.Join(computeRoot, "nodetray.exe"))
	if err != nil {
		t.Fatalf("ResolvePortableLayout: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(layout.HelperConfig)); !os.IsNotExist(err) {
		t.Fatalf("fresh Compute fixture unexpectedly contains data/helper: %v", err)
	}

	_, err = trayconfig.NewStore(trayconfig.Paths{
		TraySettings:     layout.TraySettings,
		AgentConfig:      layout.AgentConfig,
		HelperConfig:     layout.HelperConfig,
		AgentExecutable:  layout.AgentExecutable,
		HelperExecutable: layout.HelperExecutable,
	})
	if err != nil {
		t.Fatalf("ordinary-user extracted Compute root cannot initialize production Store: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(layout.HelperConfig)); !os.IsNotExist(err) {
		t.Fatalf("ordinary Store pre-created protected Helper parent: %v", err)
	}
}
