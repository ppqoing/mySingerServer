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
	layout, err := production.ResolvePortableLayout(filepath.Join(root, "compute", "nodetray.exe"))
	if err != nil {
		t.Fatalf("ResolvePortableLayout: %v", err)
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
