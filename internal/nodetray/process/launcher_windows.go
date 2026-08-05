//go:build windows

package process

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"syscall"
	"unicode"
)

var (
	errAgentLaunchArguments = errors.New("agent launch arguments are invalid")
	errAgentLaunchStart     = errors.New("agent process could not be started")
	errAgentLaunchIdentity  = errors.New("agent launch identity could not be verified")
)

type agentStarter interface {
	Start(ctx context.Context, executable string, args []string) (int, error)
}

// AgentLauncher starts only the fixed ordinary Agent invocation. Its Start
// method has the same shape as supervisor.Launcher without importing that
// package, which would create an import cycle.
type AgentLauncher struct {
	inspector Inspector
	starter   agentStarter
}

func NewAgentLauncher(inspector Inspector) *AgentLauncher {
	return newAgentLauncher(inspector, nativeAgentStarter{})
}

func newAgentLauncher(inspector Inspector, starter agentStarter) *AgentLauncher {
	return &AgentLauncher{inspector: inspector, starter: starter}
}

func (l *AgentLauncher) Start(ctx context.Context, executable string, args []string, env []string) (Identity, error) {
	cleanExecutable, configPath, ok := validAgentInvocation(executable, args, env)
	if !ok {
		return Identity{}, errAgentLaunchArguments
	}
	if ctx == nil || l == nil || l.inspector == nil || l.starter == nil {
		return Identity{}, errAgentLaunchIdentity
	}
	pid, err := l.starter.Start(ctx, cleanExecutable, []string{"--config", configPath})
	if err != nil || pid <= 0 {
		return Identity{}, errAgentLaunchStart
	}
	identity, err := l.inspector.Inspect(pid)
	if err != nil || identity.PID != pid || identity.StartedAtUnixMS <= 0 || identity.ExecutablePath == "" || !sameExecutablePath(cleanExecutable, identity.ExecutablePath) {
		return Identity{}, errAgentLaunchIdentity
	}
	return identity, nil
}

func validAgentInvocation(executable string, args []string, env []string) (string, string, bool) {
	if len(env) != 0 || len(args) != 2 || args[0] != "--config" || hasControlCharacter(executable) || hasControlCharacter(args[1]) {
		return "", "", false
	}
	if !filepath.IsAbs(executable) || !filepath.IsAbs(args[1]) {
		return "", "", false
	}
	cleanExecutable := filepath.Clean(executable)
	if !filepath.IsAbs(cleanExecutable) || !stringsEqualFold(filepath.Base(cleanExecutable), "agent.exe") {
		return "", "", false
	}
	return cleanExecutable, args[1], true
}

func hasControlCharacter(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func stringsEqualFold(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		l, r := left[i], right[i]
		if l >= 'A' && l <= 'Z' {
			l += 'a' - 'A'
		}
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		if l != r {
			return false
		}
	}
	return true
}

type nativeAgentStarter struct{}

func (nativeAgentStarter) Start(ctx context.Context, executable string, args []string) (int, error) {
	command := exec.CommandContext(ctx, executable, args...)
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := command.Start(); err != nil {
		return 0, err
	}
	pid := command.Process.Pid
	if err := command.Process.Release(); err != nil {
		return 0, err
	}
	return pid, nil
}
