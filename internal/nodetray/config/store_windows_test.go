//go:build windows

package config

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"dedup/internal/securefile"
	"github.com/google/uuid"
	"golang.org/x/sys/windows"
)

func TestStoreAndAgentShareSecureAtomicWriter(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "shared-secure-writer")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(directory, "tray.json")
	want := []byte("{\n  \"value\": true\n}\n")
	if err := securefile.WriteAtomic(target, want, func(path string) ([]byte, error) { return os.ReadFile(path) }); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("shared writer bytes = %q, %v", got, err)
	}
	assertRestrictedWritableACL(t, target, currentTestUserSID(t))
}

func TestStoreWritableConfigACLAllowsOnlyCurrentUserAdministratorsAndSystem(t *testing.T) {
	root := filepath.Join(t.TempDir(), "acl-"+uuid.NewString())
	paths := Paths{
		TraySettings:     filepath.Join(root, "tray", "settings.json"),
		AgentConfig:      filepath.Join(root, "agent", "agent.json"),
		HelperConfig:     filepath.Join(root, "protected", "helper.json"),
		AgentExecutable:  filepath.Join(root, "bin", "agent.exe"),
		HelperExecutable: filepath.Join(root, "bin", "helper.exe"),
	}
	store, err := NewStore(paths)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTraySettings(validTraySettings()); err != nil {
		t.Fatalf("SaveTraySettings: %v", err)
	}
	base := fullyPopulatedAgentConfig()
	writeBytesFixture(t, paths.AgentConfig, mustCanonicalJSON(t, base))
	form, err := AgentToForm(base)
	if err != nil {
		t.Fatal(err)
	}
	form.DataDir = `D:\acl-agent`
	if _, err := store.SaveAgentForm(form); err != nil {
		t.Fatalf("SaveAgentForm: %v", err)
	}

	currentSID := currentTestUserSID(t)
	for _, path := range []string{
		paths.TraySettings,
		paths.AgentConfig,
		paths.AgentConfig + ".last-good",
		paths.TraySettings + ".lock",
		paths.AgentConfig + ".lock",
		filepath.Dir(paths.TraySettings),
		filepath.Dir(paths.AgentConfig),
	} {
		assertRestrictedWritableACL(t, path, currentSID)
	}
}

func TestStoreHelperPreparationDoesNotWriteOrWeakenProtectedACL(t *testing.T) {
	root := filepath.Join(t.TempDir(), "helper-acl-"+uuid.NewString())
	paths := Paths{
		TraySettings:     filepath.Join(root, "tray", "settings.json"),
		AgentConfig:      filepath.Join(root, "agent", "agent.json"),
		HelperConfig:     filepath.Join(root, "protected", "helper.json"),
		AgentExecutable:  filepath.Join(root, "bin", "agent.exe"),
		HelperExecutable: filepath.Join(root, "bin", "helper.exe"),
	}
	store, err := NewStore(paths)
	if err != nil {
		t.Fatal(err)
	}
	writeBytesFixture(t, paths.HelperConfig, []byte("protected-original"))
	currentSID := currentTestUserSID(t)
	setNamedDACL(t, filepath.Dir(paths.HelperConfig),
		"D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;GRGX;;;"+currentSID+")")
	t.Cleanup(func() {
		_ = platformRestrictWritable(filepath.Dir(paths.HelperConfig))
	})
	setProtectedTestACL(t, paths.HelperConfig)
	beforeACL := namedDACLString(t, paths.HelperConfig)
	beforeBody := string(readFixture(t, paths.HelperConfig))

	prepared, err := store.PrepareHelperWrite(HelperToForm(validHelperConfig(t)))
	if err != nil {
		t.Fatalf("PrepareHelperWrite: %v", err)
	}
	if prepared.TargetPath != paths.HelperConfig {
		t.Fatalf("prepared target = %q, want %q", prepared.TargetPath, paths.HelperConfig)
	}
	if got := string(readFixture(t, paths.HelperConfig)); got != beforeBody {
		t.Fatalf("Helper body changed = %q, want %q", got, beforeBody)
	}
	if got := namedDACLString(t, paths.HelperConfig); got != beforeACL {
		t.Fatalf("Helper ACL changed\n got: %s\nwant: %s", got, beforeACL)
	}
	if _, err := os.Stat(paths.HelperConfig + ".last-good"); !os.IsNotExist(err) {
		t.Fatalf("ordinary Store created protected Helper backup: %v", err)
	}
}

func TestValidateProtectedHelperSecurityRejectsOrdinaryMutationRightsAndAcceptsReadOnlyDeploymentUser(t *testing.T) {
	currentSID := currentTestUserSID(t)
	safeParent := "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;GRGX;;;" + currentSID + ")"
	safeFile := "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;GR;;;" + currentSID + ")"
	tests := []struct {
		name       string
		parentSDDL string
		fileSDDL   string
		wantErr    bool
	}{
		{name: "safe read only deployment user", parentSDDL: safeParent, fileSDDL: safeFile},
		{
			name:       "deployment user can replace from parent",
			parentSDDL: "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;" + currentSID + ")",
			fileSDDL:   safeFile,
			wantErr:    true,
		},
		{
			name:       "deployment user can write file",
			parentSDDL: safeParent,
			fileSDDL:   "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FW;;;" + currentSID + ")",
			wantErr:    true,
		},
		{
			name:       "everyone has delete child",
			parentSDDL: safeParent + "(A;;0x00000040;;;WD)",
			fileSDDL:   safeFile,
			wantErr:    true,
		},
		{
			name:       "builtin users has generic write",
			parentSDDL: safeParent + "(A;;GW;;;BU)",
			fileSDDL:   safeFile,
			wantErr:    true,
		},
		{
			name:       "authenticated users has delete",
			parentSDDL: safeParent + "(A;;SD;;;AU)",
			fileSDDL:   safeFile,
			wantErr:    true,
		},
		{
			name:       "interactive users can append file",
			parentSDDL: safeParent,
			fileSDDL:   safeFile + "(A;;0x00000004;;;IU)",
			wantErr:    true,
		},
		{
			name:       "network can change DACL",
			parentSDDL: safeParent,
			fileSDDL:   safeFile + "(A;;WD;;;NU)",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := securityDescriptorFromTestSDDL(t, "O:BA"+tt.parentSDDL)
			file := securityDescriptorFromTestSDDL(t, "O:BA"+tt.fileSDDL)
			err := validateProtectedHelperSecurityDescriptor(parent)
			if err == nil {
				err = validateProtectedHelperSecurityDescriptor(file)
			}
			if tt.wantErr && err == nil {
				t.Fatal("validator accepted Helper ACL with ordinary mutation rights")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("validator rejected safe read-only Helper ACL: %v", err)
			}
		})
	}
}

func TestStoreHelperACLRejectsUntrustedOwnerAndAcceptsTrustedOwner(t *testing.T) {
	currentSID := currentTestUserSID(t)
	safeParent := "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;GRGX;;;" + currentSID + ")"
	safeFile := "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;GR;;;" + currentSID + ")"

	t.Run("NewStore rejects deployment user owner on real paths", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "helper-owner-"+uuid.NewString())
		paths := Paths{
			TraySettings:     filepath.Join(root, "tray", "settings.json"),
			AgentConfig:      filepath.Join(root, "agent", "agent.json"),
			HelperConfig:     filepath.Join(root, "protected", "helper.json"),
			AgentExecutable:  filepath.Join(root, "bin", "agent.exe"),
			HelperExecutable: filepath.Join(root, "bin", "helper.exe"),
		}
		writeBytesFixture(t, paths.HelperConfig, []byte("helper-owner-fixture"))
		setNamedDACL(t, filepath.Dir(paths.HelperConfig), safeParent)
		setNamedDACL(t, paths.HelperConfig, safeFile)
		for _, path := range []string{filepath.Dir(paths.HelperConfig), paths.HelperConfig} {
			owner := namedOwnerSID(t, path)
			if owner.String() != currentSID {
				t.Fatalf("test fixture owner = %s, want deployment user", owner.String())
			}
		}
		t.Cleanup(func() {
			_ = platformRestrictWritable(paths.HelperConfig)
			_ = platformRestrictWritable(filepath.Dir(paths.HelperConfig))
		})

		_, err := NewStore(paths)
		if err == nil {
			t.Fatal("NewStore accepted deployment-user-owned Helper parent and file")
		}
		assertErrorRedacted(t, err, root, paths.HelperConfig)
	})

	for _, tt := range []struct {
		name    string
		owner   string
		wantErr bool
	}{
		{name: "Administrators", owner: "BA"},
		{name: "SYSTEM", owner: "SY"},
		{name: "deployment user", owner: currentSID, wantErr: true},
		{name: "Everyone", owner: "WD", wantErr: true},
		{name: "Builtin Users", owner: "BU", wantErr: true},
		{name: "Authenticated Users", owner: "AU", wantErr: true},
		{name: "Interactive Users", owner: "IU", wantErr: true},
		{name: "NETWORK", owner: "NU", wantErr: true},
	} {
		t.Run(tt.name+" descriptor owner", func(t *testing.T) {
			descriptor := securityDescriptorFromTestSDDL(t, "O:"+tt.owner+safeFile)
			err := validateProtectedHelperSecurityDescriptor(descriptor)
			if tt.wantErr && err == nil {
				t.Fatal("validator accepted untrusted Helper owner")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("validator rejected trusted Helper owner: %v", err)
			}
		})
	}
}

func currentTestUserSID(t *testing.T) string {
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

func assertRestrictedWritableACL(t *testing.T, path, currentSID string) {
	t.Helper()
	sddl := namedDACLString(t, path)
	if !strings.Contains(sddl, "D:P") {
		t.Fatalf("%s DACL is not protected: %s", filepath.Base(path), sddl)
	}
	for _, forbidden := range []string{";;;WD)", ";;;NU)", ";;;BU)", ";;;AU)", ";;;IU)", ";;;S-1-1-0)", ";;;S-1-5-2)"} {
		if strings.Contains(sddl, forbidden) {
			t.Fatalf("%s DACL grants forbidden trustee %q: %s", filepath.Base(path), forbidden, sddl)
		}
	}
	trustees := map[string]bool{}
	for _, match := range regexp.MustCompile(`\([AD];[^)]*;;;([^)]+)\)`).FindAllStringSubmatch(sddl, -1) {
		trustee := match[1]
		if trustee == "LA" {
			if sid, err := windows.StringToSid(currentSID); err == nil && sid.IsWellKnown(windows.WinAccountAdministratorSid) {
				trustee = currentSID
			}
		}
		trustees[trustee] = true
	}
	for _, wanted := range []string{"SY", "BA", currentSID} {
		if !trustees[wanted] {
			t.Fatalf("%s DACL missing %q: %s", filepath.Base(path), wanted, sddl)
		}
	}
	if len(trustees) != 3 {
		t.Fatalf("%s DACL trustees = %#v, want only current user, BA, SY: %s", filepath.Base(path), trustees, sddl)
	}
}

func namedDACLString(t *testing.T, path string) string {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo(%s): %v", filepath.Base(path), err)
	}
	return descriptor.String()
}

func setProtectedTestACL(t *testing.T, path string) {
	t.Helper()
	currentSID := currentTestUserSID(t)
	descriptor, err := windows.SecurityDescriptorFromString(
		"D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;" + currentSID + ")",
	)
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
	t.Cleanup(func() {
		_ = platformRestrictWritable(path)
	})
}

func setNamedDACL(t *testing.T, path, sddl string) {
	t.Helper()
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatalf("SecurityDescriptorFromString: %v", err)
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

func securityDescriptorFromTestSDDL(t *testing.T, sddl string) *windows.SECURITY_DESCRIPTOR {
	t.Helper()
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func namedOwnerSID(t *testing.T, path string) *windows.SID {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo owner: %v", err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		t.Fatalf("security descriptor Owner: %v", err)
	}
	return owner
}
