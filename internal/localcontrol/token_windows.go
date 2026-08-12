//go:build windows

package localcontrol

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func platformLoadOrCreate(path, candidate string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	created, err := createProtectedTokenFile(path, []byte(candidate))
	if err != nil {
		return "", err
	}
	if created {
		return candidate, nil
	}
	for attempt := 0; ; attempt++ {
		contents, readErr := os.ReadFile(path)
		if readErr == nil {
			return strings.TrimSpace(string(contents)), nil
		}
		if attempt == 199 || !isConcurrentTokenCreateError(readErr) {
			return "", readErr
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func createProtectedTokenFile(path string, contents []byte) (bool, error) {
	descriptor, err := protectedTokenSecurityDescriptor()
	if err != nil {
		return false, err
	}
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	attributes := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}
	handle, err := windows.CreateFile(
		path16,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		0,
		attributes,
		windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	keep := false
	defer func() {
		_ = windows.CloseHandle(handle)
		if !keep {
			_ = os.Remove(path)
		}
	}()
	var written uint32
	if err := windows.WriteFile(handle, contents, &written, nil); err != nil {
		return false, err
	}
	if written != uint32(len(contents)) {
		return false, errors.New("short write creating local control token")
	}
	if err := windows.FlushFileBuffers(handle); err != nil {
		return false, err
	}
	keep = true
	return true, nil
}

func protectedTokenSecurityDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	var processToken windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &processToken); err != nil {
		return nil, err
	}
	defer processToken.Close()
	user, err := processToken.GetTokenUser()
	if err != nil {
		return nil, err
	}
	return windows.SecurityDescriptorFromString(
		"D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + user.User.Sid.String() + ")",
	)
}

func isConcurrentTokenCreateError(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, os.ErrNotExist)
}
