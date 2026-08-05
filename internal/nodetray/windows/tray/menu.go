package tray

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"dedup/internal/nodetray/traymodel"
)

type Command string

const (
	ShowConsole  Command = "show-console"
	StartAgent   Command = "start-agent"
	RestartAgent Command = "restart-agent"
	StopAgent    Command = "stop-agent"
	StartHelper  Command = "start-helper"
	StopHelper   Command = "stop-helper"
	OpenLogs     Command = "open-logs"
	OpenSettings Command = "open-settings"
	ExitTray     Command = "exit-tray"
)

type Item struct {
	Label     string
	Command   Command
	Enabled   bool
	Separator bool
}

type Snapshot struct {
	MachineID       string
	Agent           traymodel.ComponentState
	Helper          traymodel.ComponentState
	HelperEnabled   bool
	HelperStartMode traymodel.StartMode
}

var (
	menuURI        = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s]+`)
	menuSecret     = regexp.MustCompile(`(?i)\b(?:password|passwd|pwd|[\w-]*(?:credential|secret|token)[\w-]*)\s*[:=]\s*(?:"[^"]*"|'[^']*'|[^\s;,]+)`)
	menuDOSPath    = regexp.MustCompile(`(?i)(?:\b[a-z]:\\|\\\\)[^\s]+`)
	menuWhitespace = regexp.MustCompile(`\s+`)
)

func BuildMenu(snapshot Snapshot) []Item {
	machine := safeMenuText(snapshot.MachineID, 64)
	if machine == "" {
		machine = "未配置"
	}
	health := "需要处理"
	if aggregateHealthy(snapshot) {
		health = "健康"
	}
	helperStartLabel := "启动删除 Helper"
	agentStopLabel := "停止 Agent"
	if snapshot.Agent.Lifecycle == traymodel.Starting {
		agentStopLabel = "取消 Agent 启动"
	}
	helperStopLabel := "停止删除 Helper"
	if snapshot.Helper.Lifecycle == traymodel.Starting {
		helperStopLabel = "取消删除 Helper 启动"
	}
	if snapshot.HelperEnabled && snapshot.HelperStartMode == traymodel.StartManual {
		helperStartLabel += "（需要管理员权限）"
	}

	return []Item{
		{Label: fmt.Sprintf("节点：%s｜%s", machine, health)},
		{Label: "Agent：" + lifecycleLabel(snapshot.Agent.Lifecycle)},
		{Label: fmt.Sprintf("Worker：%d/%d Ready", nonNegative(snapshot.Agent.WorkerReady), nonNegative(snapshot.Agent.WorkerExpected))},
		{Label: "删除 Helper：" + helperStateLabel(snapshot)},
		{Separator: true},
		{Label: "显示节点控制台", Command: ShowConsole, Enabled: true},
		{Separator: true},
		{Label: "启动 Agent", Command: StartAgent, Enabled: canStart(snapshot.Agent.Lifecycle)},
		{Label: "重启 Agent", Command: RestartAgent, Enabled: canControlRunning(snapshot.Agent.Lifecycle)},
		{Label: agentStopLabel, Command: StopAgent, Enabled: canStop(snapshot.Agent.Lifecycle)},
		{Separator: true},
		{Label: helperStartLabel, Command: StartHelper, Enabled: snapshot.HelperEnabled && canStart(snapshot.Helper.Lifecycle)},
		{Label: helperStopLabel, Command: StopHelper, Enabled: snapshot.HelperEnabled && canStop(snapshot.Helper.Lifecycle)},
		{Separator: true},
		{Label: "打开 Agent 日志", Command: OpenLogs, Enabled: true},
		{Label: "程序设置", Command: OpenSettings, Enabled: true},
		{Label: "退出托盘程序", Command: ExitTray, Enabled: true},
	}
}

func aggregateHealthy(snapshot Snapshot) bool {
	if snapshot.Agent.Lifecycle != traymodel.Running || !snapshot.Agent.Healthy || !snapshot.Agent.Ready {
		return false
	}
	if snapshot.Agent.WorkerExpected > 0 && snapshot.Agent.WorkerReady != snapshot.Agent.WorkerExpected {
		return false
	}
	return !snapshot.HelperEnabled || (snapshot.Helper.Lifecycle == traymodel.Running && snapshot.Helper.Healthy)
}

func helperStateLabel(snapshot Snapshot) string {
	if !snapshot.HelperEnabled {
		return "未启用"
	}
	return lifecycleLabel(snapshot.Helper.Lifecycle)
}

func lifecycleLabel(value traymodel.Lifecycle) string {
	switch value {
	case traymodel.Stopped:
		return "已停止"
	case traymodel.Starting:
		return "正在启动"
	case traymodel.Running:
		return "运行中"
	case traymodel.Stopping:
		return "正在停止"
	case traymodel.Failed:
		return "需要处理"
	default:
		return "未知"
	}
}

func canStart(value traymodel.Lifecycle) bool {
	return value == traymodel.Stopped || value == traymodel.Failed
}

func canControlRunning(value traymodel.Lifecycle) bool { return value == traymodel.Running }

func canStop(value traymodel.Lifecycle) bool {
	return value == traymodel.Running || value == traymodel.Starting
}

func nonNegative(value int) int {
	if value < 0 {
		return 0
	}
	return value
}

func safeMenuText(value string, limit int) string {
	if !utf8.ValidString(value) {
		return ""
	}
	value = strings.NewReplacer(`\r`, " ", `\n`, " ").Replace(value)
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	value = menuURI.ReplaceAllString(value, "[已隐藏]")
	value = menuSecret.ReplaceAllString(value, "[已隐藏]")
	value = menuDOSPath.ReplaceAllString(value, "[已隐藏路径]")
	value = menuWhitespace.ReplaceAllString(value, " ")
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) > limit {
		value = string(runes[:limit])
	}
	return value
}
