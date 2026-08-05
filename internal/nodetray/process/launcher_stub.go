//go:build !windows

package process

import (
	"context"
	"errors"
)

// AgentLauncher fail-closes outside Windows and deliberately does not emulate
// process startup.
type AgentLauncher struct{}

func NewAgentLauncher(Inspector) *AgentLauncher { return &AgentLauncher{} }

func (*AgentLauncher) Start(context.Context, string, []string, []string) (Identity, error) {
	return Identity{}, errors.New("agent launch is only supported on Windows")
}
