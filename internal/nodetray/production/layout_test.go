package production

import (
	"reflect"
	"testing"

	"dedup/internal/nodetray/traymodel"
)

func TestResolvePortableLayoutUsesExecutableDirectoryForProgramsAndData(t *testing.T) {
	tests := []struct {
		name       string
		executable string
		want       Layout
	}{
		{
			name:       "portable executable",
			executable: `D:\便携 工具\MySingerServer-Compute\nodetray.exe`,
			want: Layout{
				Root:             `D:\便携 工具\MySingerServer-Compute`,
				TrayExecutable:   `D:\便携 工具\MySingerServer-Compute\nodetray.exe`,
				AgentExecutable:  `D:\便携 工具\MySingerServer-Compute\agent.exe`,
				HelperExecutable: `D:\便携 工具\MySingerServer-Compute\helper.exe`,
				TraySettings:     `D:\便携 工具\MySingerServer-Compute\data\nodetray\tray.json`,
				AgentConfig:      `D:\便携 工具\MySingerServer-Compute\data\agent\agent.json`,
				HelperConfig:     `D:\便携 工具\MySingerServer-Compute\data\helper\helper.json`,
				AgentLogs:        `D:\便携 工具\MySingerServer-Compute\data\agent\logs`,
				HelperLogs:       `D:\便携 工具\MySingerServer-Compute\data\helper\logs`,
				WebViewData:      `D:\便携 工具\MySingerServer-Compute\data\nodetray\webview2`,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolvePortableLayout(tt.executable)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("layout=%#v want=%#v", got, tt.want)
			}
		})
	}
}

func TestResolvePortableLayoutRejectsRelativeUNCAndWrongExecutableName(t *testing.T) {
	tests := []struct {
		name       string
		executable string
	}{
		{name: "relative path", executable: `MySingerServer-Compute\nodetray.exe`},
		{name: "UNC path", executable: `\\server\share\nodetray.exe`},
		{name: "root directory", executable: `D:\nodetray.exe`},
		{name: "wrong executable name", executable: `D:\MySingerServer-Compute\agent.exe`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ResolvePortableLayout(tt.executable); err == nil {
				t.Fatal("ResolvePortableLayout accepted unsafe executable path")
			}
		})
	}
}

func TestDefaultTraySettingsUsesTheSafeFirstRunValues(t *testing.T) {
	want := traymodel.TraySettings{
		LoginStartTray:         false,
		AgentStartMode:         traymodel.StartAutomatic,
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
