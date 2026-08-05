package production

import (
	"errors"
	"path/filepath"
	"strings"

	"dedup/internal/nodetray/traymodel"
)

type Layout struct {
	TrayExecutable   string
	AgentExecutable  string
	HelperExecutable string
	TraySettings     string
	AgentConfig      string
	HelperConfig     string
	AgentLogs        string
	HelperLogs       string
}

func ResolveLayout(programFiles, programData, localAppData string) (Layout, error) {
	roots := []string{
		filepath.Clean(programFiles),
		filepath.Clean(programData),
		filepath.Clean(localAppData),
	}
	for _, root := range roots {
		if !validRoot(root) {
			return Layout{}, errors.New("production layout: invalid root")
		}
	}
	for left := range roots {
		for right := 0; right < left; right++ {
			if pathsOverlap(roots[left], roots[right]) {
				return Layout{}, errors.New("production layout: roots overlap")
			}
		}
	}

	programDirectory := filepath.Join(roots[0], "MySingerServer")
	agentDirectory := filepath.Join(roots[1], "MySingerServer", "Node")
	helperDirectory := filepath.Join(roots[1], "MySingerServer", "Helper")
	trayDirectory := filepath.Join(roots[2], "MySingerServer", "NodeTray")
	layout := Layout{
		TrayExecutable:   filepath.Join(programDirectory, "nodetray.exe"),
		AgentExecutable:  filepath.Join(programDirectory, "agent.exe"),
		HelperExecutable: filepath.Join(programDirectory, "helper.exe"),
		TraySettings:     filepath.Join(trayDirectory, "tray.json"),
		AgentConfig:      filepath.Join(agentDirectory, "agent.json"),
		HelperConfig:     filepath.Join(helperDirectory, "helper.json"),
		AgentLogs:        filepath.Join(agentDirectory, "logs"),
		HelperLogs:       filepath.Join(helperDirectory, "logs"),
	}
	for path, root := range map[string]string{
		layout.TrayExecutable:   roots[0],
		layout.AgentExecutable:  roots[0],
		layout.HelperExecutable: roots[0],
		layout.TraySettings:     roots[2],
		layout.AgentConfig:      roots[1],
		layout.HelperConfig:     roots[1],
		layout.AgentLogs:        roots[1],
		layout.HelperLogs:       roots[1],
	} {
		if !strictlyBelow(path, root) {
			return Layout{}, errors.New("production layout: generated path escaped root")
		}
	}
	return layout, nil
}

func DefaultTraySettings() traymodel.TraySettings {
	return traymodel.TraySettings{
		LoginStartTray:         false,
		AgentStartMode:         traymodel.StartManual,
		HelperEnabled:          false,
		HelperStartMode:        traymodel.StartManual,
		CloseToTray:            true,
		RefreshIntervalSeconds: 2,
		NotificationLevel:      traymodel.NotifyImportant,
	}
}

func validRoot(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Base(path) != "."
}

func pathsOverlap(left, right string) bool {
	return sameOrBelow(left, right) || sameOrBelow(right, left)
}

func strictlyBelow(path, root string) bool {
	relative, err := filepath.Rel(strings.ToLower(filepath.Clean(root)), strings.ToLower(filepath.Clean(path)))
	return err == nil && relative != "." && relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func sameOrBelow(path, root string) bool {
	relative, err := filepath.Rel(strings.ToLower(filepath.Clean(root)), strings.ToLower(filepath.Clean(path)))
	return err == nil && !filepath.IsAbs(relative) && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))))
}
