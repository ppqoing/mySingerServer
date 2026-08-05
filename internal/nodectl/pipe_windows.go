//go:build windows

package nodectl

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

const (
	agentPipeName  = `\\.\pipe\mysingerserver-agent-control-v1`
	helperPipeName = `\\.\pipe\mysingerserver-helper-control-v1`
)

const (
	agentPipeMutexName  = `Local\mysingerserver-agent-control-v1-listener`
	helperPipeMutexName = `Local\mysingerserver-helper-control-v1-listener`
)

func AgentPipeName() string {
	return agentPipeName
}

func HelperPipeName() string {
	return helperPipeName
}

func Listen(name string) (net.Listener, error) {
	mutexName, err := pipeMutexName(name)
	if err != nil {
		return nil, err
	}
	mutex, err := createListenerMutex(mutexName)
	if err != nil {
		return nil, err
	}

	currentUserSID, err := currentProcessUserSID()
	if err != nil {
		_ = windows.CloseHandle(mutex)
		return nil, err
	}
	listener, err := winio.ListenPipe(name, pipeConfig(currentUserSID))
	if err != nil {
		_ = windows.CloseHandle(mutex)
		return nil, fmt.Errorf("nodectl pipe: listen %q: %w", name, err)
	}
	return &exclusivePipeListener{Listener: listener, mutex: mutex}, nil
}

func Dial(ctx context.Context, name string) (net.Conn, error) {
	if _, err := pipeMutexName(name); err != nil {
		return nil, err
	}
	conn, err := winio.DialPipeContext(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("nodectl pipe: dial %q: %w", name, err)
	}
	return conn, nil
}

func pipeMutexName(name string) (string, error) {
	switch name {
	case agentPipeName:
		return agentPipeMutexName, nil
	case helperPipeName:
		return helperPipeMutexName, nil
	default:
		return "", fmt.Errorf("nodectl pipe: unsupported pipe name %q", name)
	}
}

func currentProcessUserSID() (string, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(
		windows.CurrentProcess(),
		windows.TOKEN_QUERY,
		&token,
	); err != nil {
		return "", fmt.Errorf("nodectl pipe: open process token: %w", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("nodectl pipe: get token user: %w", err)
	}
	return user.User.Sid.String(), nil
}

func pipeConfig(currentUserSID string) *winio.PipeConfig {
	return &winio.PipeConfig{
		SecurityDescriptor: "D:(D;;GA;;;NU)" +
			"(A;;GA;;;SY)" +
			"(A;;GA;;;BA)" +
			"(A;;GA;;;" + currentUserSID + ")",
		MessageMode:      false,
		InputBufferSize:  64 << 10,
		OutputBufferSize: 64 << 10,
	}
}

func createListenerMutex(name string) (windows.Handle, error) {
	name16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, fmt.Errorf("nodectl pipe: encode listener mutex name: %w", err)
	}
	mutex, err := windows.CreateMutex(nil, false, name16)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		if mutex != 0 {
			_ = windows.CloseHandle(mutex)
		}
		return 0, fmt.Errorf("nodectl pipe: listener already exists for %q", name)
	}
	if err != nil {
		return 0, fmt.Errorf("nodectl pipe: create listener mutex: %w", err)
	}
	return mutex, nil
}

type exclusivePipeListener struct {
	net.Listener
	mutex windows.Handle
	once  sync.Once
	err   error
}

func (l *exclusivePipeListener) Close() error {
	l.once.Do(func() {
		l.err = errors.Join(l.Listener.Close(), windows.CloseHandle(l.mutex))
	})
	return l.err
}
