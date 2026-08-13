//go:build windows

package localcontrol

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestFileTokenStoreCreatesProtectedWindowsDACL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	setTokenTestDACL(t, dir, "D:P(A;OICI;FA;;;WD)(A;OICI;FA;;;BU)")
	path := filepath.Join(dir, "local-control.token")
	if _, err := (FileTokenStore{}).LoadOrCreate(path); err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo: %v", err)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("token DACL is not protected: %s", descriptor.String())
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if dacl == nil || dacl.AceCount != 3 {
		t.Fatalf("token DACL ACE count = %v, want 3: %s", dacl, descriptor.String())
	}
	t.Logf("protected token DACL: %s", descriptor.String())
	wantUser := currentTokenTestUserSID(t)
	seen := map[string]bool{}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			t.Fatalf("GetAce(%d): %v", index, err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE ||
			ace.Header.AceFlags&windows.INHERITED_ACE != 0 ||
			ace.Mask != 0x1f01ff {
			t.Fatalf("unexpected token ACE: type=%d flags=%#x mask=%#x descriptor=%s", ace.Header.AceType, ace.Header.AceFlags, ace.Mask, descriptor.String())
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		switch {
		case sid.String() == wantUser:
			seen["current-user"] = true
		case sid.IsWellKnown(windows.WinBuiltinAdministratorsSid):
			seen["administrators"] = true
		case sid.IsWellKnown(windows.WinLocalSystemSid):
			seen["system"] = true
		default:
			t.Fatalf("token DACL grants unexpected trustee %s: %s", sid.String(), descriptor.String())
		}
	}
	for _, trustee := range []string{"current-user", "administrators", "system"} {
		if !seen[trustee] {
			t.Fatalf("token DACL missing %s: %s", trustee, descriptor.String())
		}
	}
}

func TestFileTokenStoreRejectsUnsafeExistingWindowsToken(t *testing.T) {
	currentSID := currentTokenTestUserSID(t)
	tests := []struct {
		name string
		sddl string
		info windows.SECURITY_INFORMATION
	}{
		{
			name: "Everyone and Users",
			sddl: "D:P(A;;FA;;;WD)(A;;FA;;;BU)",
			info: windows.DACL_SECURITY_INFORMATION |
				windows.PROTECTED_DACL_SECURITY_INFORMATION,
		},
		{
			name: "extra allow ACE",
			sddl: "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + currentSID + ")" +
				"(A;;FR;;;BU)",
			info: windows.DACL_SECURITY_INFORMATION |
				windows.PROTECTED_DACL_SECURITY_INFORMATION,
		},
		{
			name: "unprotected DACL",
			sddl: "D:(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + currentSID + ")",
			info: windows.DACL_SECURITY_INFORMATION |
				windows.UNPROTECTED_DACL_SECURITY_INFORMATION,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "local-control.token")
			if err := os.WriteFile(path, []byte(validTokenFixture()), 0o600); err != nil {
				t.Fatal(err)
			}
			setTokenTestDACLWithInfo(t, path, test.sddl, test.info)

			token, err := (FileTokenStore{}).LoadOrCreate(path)
			if err == nil {
				t.Fatalf("LoadOrCreate accepted unsafe existing token %q", token)
			}
			if strings.Contains(err.Error(), validTokenFixture()) {
				t.Fatalf("error leaked existing token: %v", err)
			}
		})
	}
}

func TestFileTokenStoreRejectsExistingWindowsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.token")
	link := filepath.Join(dir, "local-control.token")
	if err := os.WriteFile(target, []byte(validTokenFixture()), 0o600); err != nil {
		t.Fatal(err)
	}
	setTokenTestDACL(t, target, protectedTokenTestSDDL(t))
	if err := os.Symlink(target, link); err != nil {
		if errorsIsWindowsSymlinkPrivilege(err) {
			t.Skipf("symbolic link privilege unavailable: %v", err)
		}
		t.Fatalf("Symlink: %v", err)
	}

	if token, err := (FileTokenStore{}).LoadOrCreate(link); err == nil {
		t.Fatalf("LoadOrCreate followed token symlink and returned %q", token)
	}
}

func TestValidateExistingTokenObjectRejectsUnsafeInjectedHandleMetadata(t *testing.T) {
	tests := []struct {
		name       string
		fileType   uint32
		attributes uint32
	}{
		{name: "reparse point", fileType: windows.FILE_TYPE_DISK, attributes: windows.FILE_ATTRIBUTE_REPARSE_POINT},
		{name: "directory", fileType: windows.FILE_TYPE_DISK, attributes: windows.FILE_ATTRIBUTE_DIRECTORY},
		{name: "non-disk object", fileType: windows.FILE_TYPE_PIPE},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateExistingTokenObjectWith(
				windows.InvalidHandle,
				func(windows.Handle) (existingTokenObjectMetadata, error) {
					return existingTokenObjectMetadata{
						fileType:   test.fileType,
						attributes: test.attributes,
					}, nil
				},
			)
			if err == nil {
				t.Fatal("unsafe injected token handle metadata was accepted")
			}
		})
	}
}

func TestFileTokenStoreReadsSafeExistingWindowsToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-control.token")
	want, err := (FileTokenStore{}).LoadOrCreate(path)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	got, err := (FileTokenStore{}).LoadOrCreate(path)
	if err != nil {
		t.Fatalf("read safe existing token: %v", err)
	}
	if got != want {
		t.Fatal("safe existing token changed")
	}
}

func setTokenTestDACL(t *testing.T, path, sddl string) {
	t.Helper()
	setTokenTestDACLWithInfo(
		t,
		path,
		sddl,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
	)
}

func setTokenTestDACLWithInfo(
	t *testing.T,
	path string,
	sddl string,
	info windows.SECURITY_INFORMATION,
) {
	t.Helper()
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		info,
		nil,
		nil,
		dacl,
		nil,
	); err != nil {
		t.Fatal(err)
	}
}

func protectedTokenTestSDDL(t *testing.T) string {
	t.Helper()
	return "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + currentTokenTestUserSID(t) + ")"
}

func validTokenFixture() string {
	return base64.RawURLEncoding.EncodeToString(make([]byte, 32))
}

func errorsIsWindowsSymlinkPrivilege(err error) bool {
	return os.IsPermission(err) || strings.Contains(strings.ToLower(err.Error()), "privilege")
}

func currentTokenTestUserSID(t *testing.T) string {
	t.Helper()
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return user.User.Sid.String()
}
