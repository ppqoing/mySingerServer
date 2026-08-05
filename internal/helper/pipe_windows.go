package helper

import (
	"fmt"
	"net"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func PipeSecurityDescriptor(currentUserSID string) string {
	return "D:(D;;GA;;;NU)" +
		"(A;;GA;;;SY)" +
		"(A;;GA;;;BA)" +
		"(A;;GA;;;" + currentUserSID + ")"
}

func currentProcessUserSID() (string, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(
		windows.CurrentProcess(),
		windows.TOKEN_QUERY,
		&token,
	); err != nil {
		return "", fmt.Errorf("helper pipe: open process token: %w", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("helper pipe: get token user: %w", err)
	}
	return user.User.Sid.String(), nil
}

func helperPipeConfig(currentUserSID string) *winio.PipeConfig {
	return &winio.PipeConfig{
		SecurityDescriptor: PipeSecurityDescriptor(currentUserSID),
		MessageMode:        false,
		InputBufferSize:    64 << 10,
		OutputBufferSize:   64 << 10,
	}
}

func ListenPipe(cfg Config) (net.Listener, error) {
	if err := validatePipeName(cfg.PipeName); err != nil {
		return nil, err
	}
	currentUserSID, err := currentProcessUserSID()
	if err != nil {
		return nil, err
	}
	listener, err := winio.ListenPipe(
		cfg.PipeName,
		helperPipeConfig(currentUserSID),
	)
	if err != nil {
		return nil, fmt.Errorf("helper pipe: listen: %w", err)
	}
	return listener, nil
}
