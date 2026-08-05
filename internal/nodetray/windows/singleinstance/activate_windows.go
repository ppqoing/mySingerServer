//go:build windows

package singleinstance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

const (
	maxActivationMessageBytes = 32
	activationSignalTimeout   = 500 * time.Millisecond
)

var (
	activationMessage     = []byte("mysingerserver-show-v1")
	ErrNoExistingInstance = errors.New("no existing tray instance")
)

func ListenActivation(ctx context.Context, show func()) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if show == nil {
		return errors.New("singleinstance: activation callback is required")
	}
	userSID, err := currentProcessUserSID()
	if err != nil {
		return err
	}
	listener, err := winio.ListenPipe(activationPipeName(), &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;SY)(A;;GA;;;" + userSID + ")",
		MessageMode:        true,
		InputBufferSize:    256,
		OutputBufferSize:   256,
	})
	if err != nil {
		return fmt.Errorf("singleinstance: listen activation pipe: %w", err)
	}
	defer listener.Close()

	stopClosing := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-stopClosing:
		}
	}()
	defer close(stopClosing)

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("singleinstance: accept activation: %w", err)
		}
		handleActivationConnection(ctx, conn, show)
	}
}

func SignalExisting(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	signalCtx := ctx
	cancel := func() {}
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > activationSignalTimeout {
		signalCtx, cancel = context.WithTimeout(ctx, activationSignalTimeout)
	}
	defer cancel()

	conn, err := winio.DialPipeContext(signalCtx, activationPipeName())
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: activation endpoint unavailable", ErrNoExistingInstance)
	}
	defer conn.Close()
	if deadline, ok := signalCtx.Deadline(); ok {
		if err := conn.SetWriteDeadline(deadline); err != nil {
			return fmt.Errorf("singleinstance: set activation deadline: %w", err)
		}
	}
	if _, err := conn.Write(activationMessage); err != nil {
		if signalCtx.Err() != nil {
			return signalContextError(ctx, signalCtx)
		}
		return fmt.Errorf("singleinstance: signal existing instance: %w", err)
	}
	closeWriter, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		return errors.New("singleinstance: activation pipe does not support framed writes")
	}
	if err := closeWriter.CloseWrite(); err != nil {
		if signalCtx.Err() != nil {
			return signalContextError(ctx, signalCtx)
		}
		return fmt.Errorf("singleinstance: finish activation frame: %w", err)
	}
	return nil
}

func signalContextError(parent, bounded context.Context) error {
	if err := parent.Err(); err != nil {
		return err
	}
	return fmt.Errorf("%w: activation deadline exceeded: %v", ErrNoExistingInstance, bounded.Err())
}

func handleActivationConnection(ctx context.Context, conn net.Conn, show func()) {
	defer conn.Close()
	stopClosing := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stopClosing:
		}
	}()
	defer close(stopClosing)

	payload, err := io.ReadAll(io.LimitReader(conn, maxActivationMessageBytes+1))
	if err == nil && bytes.Equal(payload, activationMessage) {
		show()
	}
}

func activationPipeName() string {
	userSID, _ := currentProcessUserSID()
	digest := sha256.Sum256([]byte(instanceNamespace + "\x00activation\x00" + userSID))
	return `\\.\pipe\mysingerserver-node-tray-activate-` + hex.EncodeToString(digest[:16])
}

func currentProcessUserSID() (string, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return "", fmt.Errorf("singleinstance: open process token: %w", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("singleinstance: get token user: %w", err)
	}
	return user.User.Sid.String(), nil
}
