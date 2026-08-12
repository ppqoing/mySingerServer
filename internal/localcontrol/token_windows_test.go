//go:build windows

package localcontrol

import (
	"os"
	"path/filepath"
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

func setTokenTestDACL(t *testing.T, path, sddl string) {
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
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	); err != nil {
		t.Fatal(err)
	}
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
