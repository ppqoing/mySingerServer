//go:build windows

package singleinstance

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/windows"
	"golang.org/x/text/unicode/norm"
)

var ErrAlreadyExists = errors.New("single instance already exists")

var instanceNamespace = "mysingerserver-node-tray-v1"

type Lease interface {
	Close() error
}

type mutexLease struct {
	handle windows.Handle
	once   sync.Once
	err    error
}

func AcquireTray(userSID string) (Lease, error) {
	normalized, err := normalizeSID(userSID)
	if err != nil {
		return nil, err
	}
	return acquireMutex("tray", normalized)
}

func AcquireAgent(machineID string) (Lease, error) {
	normalized, err := normalizeMachineID(machineID)
	if err != nil {
		return nil, err
	}
	return acquireMutex("agent", normalized)
}

func acquireMutex(kind, identity string) (Lease, error) {
	digest := sha256.Sum256([]byte(instanceNamespace + "\x00" + kind + "\x00" + identity))
	name := `Local\mysingerserver-node-tray-` + kind + `-` + hex.EncodeToString(digest[:16])
	name16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, fmt.Errorf("singleinstance: encode mutex name: %w", err)
	}
	handle, err := windows.CreateMutex(nil, false, name16)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		return nil, ErrAlreadyExists
	}
	if err != nil {
		return nil, fmt.Errorf("singleinstance: create mutex: %w", err)
	}
	return &mutexLease{handle: handle}, nil
}

func normalizeSID(value string) (string, error) {
	if value == "" || strings.TrimSpace(value) != value || len(value) > 184 || containsUnsafeIdentityRune(value) {
		return "", errors.New("singleinstance: invalid user SID")
	}
	sid, err := windows.StringToSid(value)
	if err != nil {
		return "", errors.New("singleinstance: invalid user SID")
	}
	return strings.ToUpper(sid.String()), nil
}

func normalizeMachineID(value string) (string, error) {
	if value == "" || !utf8.ValidString(value) || strings.TrimSpace(value) != value ||
		utf8.RuneCountInString(value) > 128 || containsUnsafeIdentityRune(value) {
		return "", errors.New("singleinstance: invalid machine ID")
	}
	return strings.ToLower(norm.NFC.String(value)), nil
}

func containsUnsafeIdentityRune(value string) bool {
	for _, r := range value {
		if r == '/' || r == '\\' || r == ':' || unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func (l *mutexLease) Close() error {
	l.once.Do(func() {
		l.err = windows.CloseHandle(l.handle)
	})
	return l.err
}
