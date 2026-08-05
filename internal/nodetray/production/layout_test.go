package production

import (
	"reflect"
	"testing"

	"dedup/internal/nodetray/traymodel"
)

func TestResolveLayoutReturnsTheFixedProductionLocations(t *testing.T) {
	layout, err := ResolveLayout(`C:\Program Files`, `C:\ProgramData`, `C:\Users\node\AppData\Local`)
	if err != nil {
		t.Fatalf("ResolveLayout: %v", err)
	}
	want := Layout{
		TrayExecutable:   `C:\Program Files\MySingerServer\nodetray.exe`,
		AgentExecutable:  `C:\Program Files\MySingerServer\agent.exe`,
		HelperExecutable: `C:\Program Files\MySingerServer\helper.exe`,
		TraySettings:     `C:\Users\node\AppData\Local\MySingerServer\NodeTray\tray.json`,
		AgentConfig:      `C:\ProgramData\MySingerServer\Node\agent.json`,
		HelperConfig:     `C:\ProgramData\MySingerServer\Helper\helper.json`,
		AgentLogs:        `C:\ProgramData\MySingerServer\Node\logs`,
		HelperLogs:       `C:\ProgramData\MySingerServer\Helper\logs`,
	}
	if !reflect.DeepEqual(layout, want) {
		t.Fatalf("layout = %#v, want %#v", layout, want)
	}
}

func TestResolveLayoutRejectsNonAbsoluteOverlappingAndEscapingRoots(t *testing.T) {
	tests := []struct {
		name                             string
		programFiles, programData, local string
	}{
		{name: "relative Program Files", programFiles: `Program Files`, programData: `C:\ProgramData`, local: `C:\Users\node\AppData\Local`},
		{name: "ProgramData below Program Files", programFiles: `C:\Company`, programData: `C:\Company\ProgramData`, local: `C:\Users\node\AppData\Local`},
		{name: "LocalAppData is ProgramData", programFiles: `C:\Program Files`, programData: `C:\ProgramData`, local: `C:\ProgramData`},
		{name: "root traversal cleans into overlap", programFiles: `C:\Company\Programs\..`, programData: `C:\Company`, local: `C:\Users\node\AppData\Local`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ResolveLayout(tt.programFiles, tt.programData, tt.local); err == nil {
				t.Fatal("ResolveLayout accepted unsafe roots")
			}
		})
	}
}

func TestDefaultTraySettingsUsesTheSafeFirstRunValues(t *testing.T) {
	want := traymodel.TraySettings{
		LoginStartTray:         false,
		AgentStartMode:         traymodel.StartManual,
		HelperEnabled:          false,
		HelperStartMode:        traymodel.StartManual,
		CloseToTray:            true,
		RefreshIntervalSeconds: 2,
		NotificationLevel:      traymodel.NotifyImportant,
	}
	if got := DefaultTraySettings(); !reflect.DeepEqual(got, want) {
		t.Fatalf("DefaultTraySettings() = %#v, want %#v", got, want)
	}
}
