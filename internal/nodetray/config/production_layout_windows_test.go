//go:build windows

package config_test

import (
	"path/filepath"
	"testing"

	trayconfig "dedup/internal/nodetray/config"
	"dedup/internal/nodetray/production"
)

func TestProductionLayoutCanInitializeConfigStore(t *testing.T) {
	root := t.TempDir()
	layout, err := production.ResolveLayout(
		filepath.Join(root, "program-files"),
		filepath.Join(root, "program-data"),
		filepath.Join(root, "local-app-data"),
	)
	if err != nil {
		t.Fatalf("ResolveLayout: %v", err)
	}

	_, err = trayconfig.NewStore(trayconfig.Paths{
		TraySettings:     layout.TraySettings,
		AgentConfig:      layout.AgentConfig,
		HelperConfig:     layout.HelperConfig,
		AgentExecutable:  layout.AgentExecutable,
		HelperExecutable: layout.HelperExecutable,
	})
	if err != nil {
		t.Fatalf("production layout cannot initialize config store: %v", err)
	}
}
