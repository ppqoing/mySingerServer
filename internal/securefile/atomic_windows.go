//go:build windows

package securefile

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"github.com/google/uuid"
	"golang.org/x/sys/windows"
)

func createRestrictedTemp(directory, pattern string) (*os.File, error) {
	descriptor, err := restrictedSecurityDescriptor()
	if err != nil {
		return nil, err
	}
	attributes := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}
	for attempts := 0; attempts < 100; attempts++ {
		name := stringsReplaceAsterisk(pattern, uuid.NewString())
		path := filepath.Join(directory, name)
		path16, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return nil, err
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
			continue
		}
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(handle), path), nil
	}
	return nil, errors.New("secure temp name unavailable")
}

func stringsReplaceAsterisk(pattern, replacement string) string {
	for index := range pattern {
		if pattern[index] == '*' {
			return pattern[:index] + replacement + pattern[index+1:]
		}
	}
	return pattern + replacement
}

func restrictedSecurityDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	sid, err := currentProcessSID()
	if err != nil {
		return nil, err
	}
	return windows.SecurityDescriptorFromString(
		"D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + sid + ")",
	)
}

func currentProcessSID() (string, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return "", err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", err
	}
	return user.User.Sid.String(), nil
}

func atomicReplace(source, destination string) error {
	source16, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destination16, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return retryAtomicReplace(func() error {
		return windows.MoveFileEx(source16, destination16, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
	}, time.Sleep)
}

func retryAtomicReplace(move func() error, pause func(time.Duration)) error {
	for attempt := 0; ; attempt++ {
		err := move()
		if err == nil || attempt == 99 || !replaceTransient(err) {
			return err
		}
		pause(time.Millisecond)
	}
}

func replaceTransient(err error) bool {
	return errors.Is(err, syscall.Errno(5)) || errors.Is(err, syscall.Errno(32)) || errors.Is(err, syscall.Errno(33))
}

func syncDirectory(string) error {
	// MOVEFILE_WRITE_THROUGH flushes the move before returning.
	return nil
}
