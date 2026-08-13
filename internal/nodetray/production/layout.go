package production

import (
	"errors"
	"path/filepath"
	"strings"

	"dedup/internal/nodetray/traymodel"
)

type Layout struct {
	Root             string
	TrayExecutable   string
	AgentExecutable  string
	HelperExecutable string
	TraySettings     string
	AgentConfig      string
	HelperConfig     string
	AgentLogs        string
	HelperLogs       string
	WebViewData      string
}

func ResolvePortableLayout(trayExecutable string) (Layout, error) {
	trayExecutable = filepath.Clean(trayExecutable)
	volume := filepath.VolumeName(trayExecutable)
	if !filepath.IsAbs(trayExecutable) || strings.HasPrefix(volume, `\\`) || !strings.EqualFold(filepath.Base(trayExecutable), "nodetray.exe") {
		return Layout{}, errors.New("production layout: invalid tray executable")
	}

	root := filepath.Dir(trayExecutable)
	if root == filepath.Clean(volume+string(filepath.Separator)) {
		return Layout{}, errors.New("production layout: tray executable cannot be in volume root")
	}

	dataDirectory := filepath.Join(root, "data")
	trayDirectory := filepath.Join(dataDirectory, "nodetray")
	agentDirectory := filepath.Join(dataDirectory, "agent")
	helperDirectory := filepath.Join(dataDirectory, "helper")
	layout := Layout{
		Root:             root,
		TrayExecutable:   trayExecutable,
		AgentExecutable:  filepath.Join(root, "agent.exe"),
		HelperExecutable: filepath.Join(root, "helper.exe"),
		TraySettings:     filepath.Join(trayDirectory, "tray.json"),
		AgentConfig:      filepath.Join(agentDirectory, "agent.json"),
		HelperConfig:     filepath.Join(helperDirectory, "helper.json"),
		AgentLogs:        filepath.Join(agentDirectory, "logs"),
		HelperLogs:       filepath.Join(helperDirectory, "logs"),
		WebViewData:      filepath.Join(trayDirectory, "webview2"),
	}
	for path := range map[string]struct{}{
		layout.TrayExecutable:   {},
		layout.AgentExecutable:  {},
		layout.HelperExecutable: {},
		layout.TraySettings:     {},
		layout.AgentConfig:      {},
		layout.HelperConfig:     {},
		layout.AgentLogs:        {},
		layout.HelperLogs:       {},
		layout.WebViewData:      {},
	} {
		if !strictlyBelow(path, root) {
			return Layout{}, errors.New("production layout: generated path escaped root")
		}
	}
	for path := range map[string]struct{}{
		layout.TraySettings: {},
		layout.AgentConfig:  {},
		layout.HelperConfig: {},
		layout.AgentLogs:    {},
		layout.HelperLogs:   {},
		layout.WebViewData:  {},
	} {
		if !strictlyBelow(path, dataDirectory) {
			return Layout{}, errors.New("production layout: generated data path escaped data directory")
		}
	}
	return layout, nil
}

func DefaultTraySettings() traymodel.TraySettings {
	return traymodel.TraySettings{
		LoginStartTray:         false,
		AgentStartMode:         traymodel.StartAutomatic,
		HelperEnabled:          false,
		HelperStartMode:        traymodel.StartManual,
		CloseToTray:            true,
		RefreshIntervalSeconds: 2,
		NotificationLevel:      traymodel.NotifyImportant,
	}
}

func strictlyBelow(path, root string) bool {
	relative, err := filepath.Rel(strings.ToLower(filepath.Clean(root)), strings.ToLower(filepath.Clean(path)))
	return err == nil && relative != "." && relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
