//go:build windows

package localcontrol

import (
	"errors"
	"io"
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
		contents, readErr := readProtectedExistingToken(path)
		if readErr == nil {
			return strings.TrimSpace(contents), nil
		}
		if attempt == 199 || !isConcurrentTokenCreateError(readErr) {
			return "", readErr
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func readProtectedExistingToken(path string) (string, error) {
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	handle, err := windows.CreateFile(
		path16,
		windows.GENERIC_READ|windows.READ_CONTROL,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|
			windows.FILE_FLAG_OPEN_REPARSE_POINT|
			windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return "", err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return "", errors.New("open existing local control token handle")
	}
	defer file.Close()

	if err := validateExistingTokenObject(handle); err != nil {
		return "", err
	}
	contents, err := io.ReadAll(io.LimitReader(file, 1025))
	if err != nil {
		return "", err
	}
	if len(contents) > 1024 {
		return "", errors.New("local control token file is too large")
	}
	return string(contents), nil
}

type existingTokenObjectMetadata struct {
	fileType   uint32
	attributes uint32
	descriptor *windows.SECURITY_DESCRIPTOR
}

type existingTokenObjectInspector func(windows.Handle) (existingTokenObjectMetadata, error)

func validateExistingTokenObject(handle windows.Handle) error {
	return validateExistingTokenObjectWith(handle, inspectExistingTokenObject)
}

func inspectExistingTokenObject(handle windows.Handle) (existingTokenObjectMetadata, error) {
	fileType, err := windows.GetFileType(handle)
	if err != nil {
		return existingTokenObjectMetadata{}, err
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return existingTokenObjectMetadata{}, err
	}
	descriptor, err := windows.GetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return existingTokenObjectMetadata{}, err
	}
	return existingTokenObjectMetadata{
		fileType:   fileType,
		attributes: information.FileAttributes,
		descriptor: descriptor,
	}, nil
}

func validateExistingTokenObjectWith(
	handle windows.Handle,
	inspect existingTokenObjectInspector,
) error {
	metadata, err := inspect(handle)
	if err != nil {
		return err
	}
	if metadata.fileType != windows.FILE_TYPE_DISK {
		return errors.New("local control token is not a disk file")
	}
	if metadata.attributes&(windows.FILE_ATTRIBUTE_DIRECTORY|
		windows.FILE_ATTRIBUTE_REPARSE_POINT|
		windows.FILE_ATTRIBUTE_DEVICE) != 0 {
		return errors.New("local control token is not a regular file")
	}
	if metadata.descriptor == nil {
		return errors.New("local control token security descriptor is missing")
	}
	return validateProtectedTokenDACL(metadata.descriptor)
}

func validateProtectedTokenDACL(descriptor *windows.SECURITY_DESCRIPTOR) error {
	control, _, err := descriptor.Control()
	if err != nil {
		return err
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("local control token DACL is not protected")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	if dacl == nil || dacl.AceCount != 3 {
		return errors.New("local control token DACL has an unexpected ACE set")
	}
	currentSID, err := currentProcessTokenSID()
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE ||
			ace.Header.AceFlags != 0 ||
			ace.Mask != 0x1f01ff {
			return errors.New("local control token DACL contains an unexpected ACE")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		trustee := ""
		switch {
		case sid.String() == currentSID:
			trustee = "current-user"
		case sid.IsWellKnown(windows.WinBuiltinAdministratorsSid):
			trustee = "administrators"
		case sid.IsWellKnown(windows.WinLocalSystemSid):
			trustee = "system"
		default:
			return errors.New("local control token DACL contains an untrusted trustee")
		}
		if seen[trustee] {
			return errors.New("local control token DACL contains a duplicate trustee")
		}
		seen[trustee] = true
	}
	if !seen["current-user"] || !seen["administrators"] || !seen["system"] {
		return errors.New("local control token DACL is missing a required trustee")
	}
	return nil
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
	sid, err := currentProcessTokenSID()
	if err != nil {
		return nil, err
	}
	return windows.SecurityDescriptorFromString(
		"D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + sid + ")",
	)
}

func currentProcessTokenSID() (string, error) {
	var processToken windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &processToken); err != nil {
		return "", err
	}
	defer processToken.Close()
	user, err := processToken.GetTokenUser()
	if err != nil {
		return "", err
	}
	return user.User.Sid.String(), nil
}

func isConcurrentTokenCreateError(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, os.ErrNotExist)
}
