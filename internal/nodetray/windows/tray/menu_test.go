package tray

import (
	"strings"
	"testing"

	"dedup/internal/nodetray/traymodel"
)

func TestBuildMenuShowsOnlyAggregateStateAndFixedCommands(t *testing.T) {
	items := BuildMenu(Snapshot{
		MachineID: "node-a",
		Agent: traymodel.ComponentState{
			Lifecycle: traymodel.Running, Healthy: true, Ready: true,
			WorkerReady: 2, WorkerExpected: 2,
		},
		Helper:          traymodel.ComponentState{Lifecycle: traymodel.Stopped},
		HelperEnabled:   true,
		HelperStartMode: traymodel.StartManual,
	})

	labels := labelsOf(items)
	for _, want := range []string{"节点：node-a｜需要处理", "Agent：运行中", "Worker：2/2 Ready", "删除 Helper：已停止"} {
		if !strings.Contains(labels, want) {
			t.Fatalf("menu labels %q do not contain %q", labels, want)
		}
	}

	wantCommands := []Command{ShowConsole, StartAgent, RestartAgent, StopAgent, StartHelper, StopHelper, OpenLogs, OpenSettings, ExitTray}
	gotCommands := commandsOf(items)
	if strings.Join(commandsToStrings(gotCommands), ",") != strings.Join(commandsToStrings(wantCommands), ",") {
		t.Fatalf("commands = %v, want %v", gotCommands, wantCommands)
	}
	for _, command := range gotCommands {
		text := strings.ToLower(string(command))
		if strings.Contains(text, "worker") || strings.Contains(text, "force") || strings.Contains(text, "delete") || strings.ContainsAny(text, `\\/`) {
			t.Fatalf("unsafe command exposed: %q", command)
		}
	}

	assertEnabled(t, items, StartAgent, false)
	assertEnabled(t, items, RestartAgent, true)
	assertEnabled(t, items, StopAgent, true)
	assertEnabled(t, items, StartHelper, true)
	assertEnabled(t, items, StopHelper, false)
	if label := labelFor(items, StartHelper); label != "启动删除 Helper（需要管理员权限）" {
		t.Fatalf("manual Helper label = %q", label)
	}
}

func TestBuildMenuDisablesConflictingAndDisabledHelperActions(t *testing.T) {
	items := BuildMenu(Snapshot{
		MachineID:     "node-b",
		Agent:         traymodel.ComponentState{Lifecycle: traymodel.Starting, WorkerReady: 1, WorkerExpected: 2},
		Helper:        traymodel.ComponentState{Lifecycle: traymodel.Running, Healthy: true},
		HelperEnabled: false,
	})

	assertEnabled(t, items, StartAgent, false)
	assertEnabled(t, items, RestartAgent, false)
	assertEnabled(t, items, StopAgent, true)
	if label := labelFor(items, StopAgent); label != "取消 Agent 启动" {
		t.Fatalf("starting stop label = %q", label)
	}
	assertEnabled(t, items, StartHelper, false)
	assertEnabled(t, items, StopHelper, false)
	if strings.Contains(labelFor(items, StartHelper), "管理员权限") {
		t.Fatalf("disabled Helper action claims UAC: %q", labelFor(items, StartHelper))
	}
}

func TestBuildMenuFailsClosedForUnavailableSnapshot(t *testing.T) {
	items := BuildMenu(Snapshot{})
	for _, command := range []Command{StartAgent, RestartAgent, StopAgent, StartHelper, StopHelper} {
		assertEnabled(t, items, command, false)
	}
}

func TestBuildMenuDoesNotLeakMachineOrComponentMetadata(t *testing.T) {
	items := BuildMenu(Snapshot{
		MachineID: `node\r\npassword=hunter2 C:\\secret\\agent.json`,
		Agent: traymodel.ComponentState{
			Lifecycle: traymodel.Failed, PID: 4321,
			ErrorSummary: `postgres://user:pass@db.local/db C:\\secret\\agent.log`,
		},
		Helper: traymodel.ComponentState{Lifecycle: traymodel.Failed, PID: 9876, ErrorSummary: "token=abc"},
	})

	labels := labelsOf(items)
	for _, forbidden := range []string{"hunter2", "postgres://", "user:pass", `C:\\secret`, "4321", "9876", "token=abc", `\r`, `\n`} {
		if strings.Contains(labels, forbidden) {
			t.Fatalf("menu leaked %q in %q", forbidden, labels)
		}
	}
	for _, item := range items {
		if strings.ContainsAny(item.Label, "\r\n") {
			t.Fatalf("menu label contains a control line break: %q", item.Label)
		}
	}
}

func labelsOf(items []Item) string {
	labels := make([]string, 0, len(items))
	for _, item := range items {
		if !item.Separator {
			labels = append(labels, item.Label)
		}
	}
	return strings.Join(labels, "\n")
}

func commandsOf(items []Item) []Command {
	var commands []Command
	for _, item := range items {
		if item.Command != "" {
			commands = append(commands, item.Command)
		}
	}
	return commands
}

func commandsToStrings(commands []Command) []string {
	values := make([]string, len(commands))
	for index, command := range commands {
		values[index] = string(command)
	}
	return values
}

func assertEnabled(t *testing.T, items []Item, command Command, want bool) {
	t.Helper()
	for _, item := range items {
		if item.Command == command {
			if item.Enabled != want {
				t.Fatalf("%s enabled = %v, want %v", command, item.Enabled, want)
			}
			return
		}
	}
	t.Fatalf("command %s is missing", command)
}

func labelFor(items []Item, command Command) string {
	for _, item := range items {
		if item.Command == command {
			return item.Label
		}
	}
	return ""
}
