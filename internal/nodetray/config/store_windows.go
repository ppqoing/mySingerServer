//go:build windows

package config

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type platformFileLock struct {
	handle windows.Handle
}

const helperMutationAccess = windows.ACCESS_MASK(
	windows.GENERIC_ALL |
		windows.GENERIC_WRITE |
		windows.DELETE |
		windows.WRITE_DAC |
		windows.WRITE_OWNER |
		windows.FILE_WRITE_DATA |
		windows.FILE_APPEND_DATA |
		windows.FILE_WRITE_EA |
		windows.FILE_WRITE_ATTRIBUTES |
		0x00000040, // FILE_DELETE_CHILD: replace/delete children through the parent.
)

func platformValidateProtectedHelper(path string) error {
	parent := filepath.Dir(path)
	if err := validateProtectedHelperDACL(parent); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Fresh installations have no existing Helper replacement surface yet.
			// The later elevated writer is responsible for creating it securely.
			return nil
		}
		return err
	}
	if err := validateProtectedHelperDACL(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return nil
}

func validateProtectedHelperDACL(path string) error {
	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|
			windows.DACL_SECURITY_INFORMATION|
			windows.PROTECTED_DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return err
	}
	return validateProtectedHelperSecurityDescriptor(descriptor)
}

func validateProtectedHelperSecurityDescriptor(descriptor *windows.SECURITY_DESCRIPTOR) error {
	owner, _, err := descriptor.Owner()
	if err != nil {
		return err
	}
	if owner == nil ||
		(!owner.IsWellKnown(windows.WinBuiltinAdministratorsSid) &&
			!owner.IsWellKnown(windows.WinLocalSystemSid)) {
		return errors.New("Helper owner is not trusted")
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return err
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("Helper DACL is not protected")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	if dacl == nil {
		return errors.New("Helper DACL is missing")
	}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return err
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			if sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) ||
				sid.IsWellKnown(windows.WinLocalSystemSid) {
				continue
			}
			if ace.Mask&helperMutationAccess != 0 {
				return errors.New("Helper DACL grants mutation access")
			}
		default:
			// Object-specific or unknown allow layouts are not interpreted here.
			// Fail closed instead of accidentally accepting a mutation grant.
			return errors.New("Helper DACL contains an unsupported ACE")
		}
	}
	return nil
}

func platformAcquireLock(path string) (*platformFileLock, error) {
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	for {
		handle, openErr := windows.CreateFile(
			path16,
			windows.GENERIC_READ|windows.GENERIC_WRITE|windows.WRITE_DAC,
			0,
			nil,
			windows.OPEN_ALWAYS,
			windows.FILE_ATTRIBUTE_NORMAL,
			0,
		)
		if openErr == nil {
			if aclErr := restrictHandleACL(handle); aclErr != nil {
				_ = windows.CloseHandle(handle)
				return nil, aclErr
			}
			return &platformFileLock{handle: handle}, nil
		}
		if !errors.Is(openErr, windows.ERROR_SHARING_VIOLATION) &&
			!errors.Is(openErr, windows.ERROR_LOCK_VIOLATION) {
			return nil, openErr
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func restrictHandleACL(handle windows.Handle) error {
	sid, err := currentProcessSID()
	if err != nil {
		return err
	}
	descriptor, err := windows.SecurityDescriptorFromString(
		"D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + sid + ")",
	)
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	)
}

func (l *platformFileLock) Close() error {
	if l == nil || l.handle == 0 {
		return nil
	}
	err := windows.CloseHandle(l.handle)
	l.handle = 0
	return err
}

func platformRestrictWritable(path string) error {
	sid, err := currentProcessSID()
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	aceFlags := ""
	if info.IsDir() {
		aceFlags = "OICI"
	}
	descriptor, err := windows.SecurityDescriptorFromString(
		"D:P(A;" + aceFlags + ";FA;;;SY)" +
			"(A;" + aceFlags + ";FA;;;BA)" +
			"(A;" + aceFlags + ";FA;;;" + sid + ")",
	)
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
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

func platformAtomicReplace(source, destination string) error {
	source16, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destination16, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		err = windows.MoveFileEx(
			source16,
			destination16,
			windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
		)
		if err == nil || attempt == 99 || !atomicReplaceTransientError(err) {
			return err
		}
		time.Sleep(time.Millisecond)
	}
}

func atomicReplaceTransientError(err error) bool {
	return errors.Is(err, syscall.Errno(5)) ||
		errors.Is(err, syscall.Errno(32)) ||
		errors.Is(err, syscall.Errno(33))
}

func platformSyncDirectory(string) error {
	// MoveFileEx with MOVEFILE_WRITE_THROUGH does not return until the move has
	// been flushed. Windows does not expose a portable directory fsync handle.
	return nil
}
