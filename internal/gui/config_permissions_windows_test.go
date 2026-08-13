//go:build windows

package gui

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestLoadOrCreateGUIConfigCreatesProtectedWindowsDACL(t *testing.T) {
	dir := t.TempDir()
	setGUIConfigTestDACL(t, dir, "D:P(A;OICI;FA;;;WD)")
	path := filepath.Join(dir, "gui.json")

	if _, err := LoadOrCreateGUIConfig(path); err != nil {
		t.Fatalf("LoadOrCreateGUIConfig: %v", err)
	}

	assertRestrictedGUIConfigDACL(t, path)
}

func TestGUIConfigServiceSaveReplacesWithProtectedWindowsDACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gui.json")
	runtime := testGUIConfig()
	writeTestGUIConfig(t, path, runtime)
	setGUIConfigTestDACL(t, path, "D:P(A;;FA;;;WD)")

	service, err := NewGUIConfigService(path, runtime)
	if err != nil {
		t.Fatal(err)
	}
	changed := testGUIConfig()
	changed.HeartbeatS++
	if _, err := service.Save(context.Background(), changed); err != nil {
		t.Fatalf("Save: %v", err)
	}

	assertRestrictedGUIConfigDACL(t, path)
}

func assertRestrictedGUIConfigDACL(t *testing.T, path string) {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo(%s): %v", filepath.Base(path), err)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatalf("read DACL control: %v", err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("%s DACL is not protected: %s", filepath.Base(path), descriptor.String())
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatalf("read DACL: %v", err)
	}
	if dacl == nil {
		t.Fatal("GUI config DACL is missing")
	}

	currentSID := currentGUIConfigTestUserSID(t)
	seen := map[string]bool{}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			t.Fatalf("GetAce(%d): %v", index, err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			t.Fatalf("GUI config DACL contains non-allow ACE type %d: %s", ace.Header.AceType, descriptor.String())
		}
		if ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
			t.Fatalf("GUI config DACL contains an inherited ACE: %s", descriptor.String())
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		switch {
		case sid.String() == currentSID:
			seen["current-user"] = true
		case sid.IsWellKnown(windows.WinBuiltinAdministratorsSid):
			seen["administrators"] = true
		case sid.IsWellKnown(windows.WinLocalSystemSid):
			seen["system"] = true
		default:
			t.Fatalf("GUI config DACL grants an unexpected trustee %s: %s", sid.String(), descriptor.String())
		}
	}
	for _, trustee := range []string{"current-user", "administrators", "system"} {
		if !seen[trustee] {
			t.Fatalf("GUI config DACL is missing %s: %s", trustee, descriptor.String())
		}
	}
	if len(seen) != 3 || dacl.AceCount != 3 {
		t.Fatalf("GUI config DACL has %d ACEs and trustees %#v, want exactly three: %s", dacl.AceCount, seen, descriptor.String())
	}
}

func currentGUIConfigTestUserSID(t *testing.T) string {
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

func setGUIConfigTestDACL(t *testing.T, path, sddl string) {
	t.Helper()
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatalf("SecurityDescriptorFromString: %v", err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatalf("read fixture DACL: %v", err)
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
		t.Fatalf("SetNamedSecurityInfo(%s): %v", filepath.Base(path), err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat ACL fixture: %v", err)
	}
}
