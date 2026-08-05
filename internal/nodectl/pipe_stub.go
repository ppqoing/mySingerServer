//go:build !windows

package nodectl

import (
	"context"
	"errors"
	"net"
)

const (
	agentPipeName  = `\\.\pipe\mysingerserver-agent-control-v1`
	helperPipeName = `\\.\pipe\mysingerserver-helper-control-v1`
)

var errNamedPipesRequireWindows = errors.New("nodectl named pipes require windows")

func AgentPipeName() string {
	return agentPipeName
}

func HelperPipeName() string {
	return helperPipeName
}

func Listen(string) (net.Listener, error) {
	return nil, errNamedPipesRequireWindows
}

func Dial(context.Context, string) (net.Conn, error) {
	return nil, errNamedPipesRequireWindows
}
