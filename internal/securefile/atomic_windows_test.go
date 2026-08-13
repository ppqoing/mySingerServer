//go:build windows

package securefile

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWriteAtomicPublishesVerifiedBytesWithRestrictedWindowsDACL(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "secure-agent")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(directory, "agent.json")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := []byte("{\n  \"listen_addr\": \"127.0.0.1:9101\"\n}\n")
	if err := WriteAtomic(target, want, func(path string) ([]byte, error) { return os.ReadFile(path) }); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("formal bytes = %q, %v", got, err)
	}
	assertSecureFileDACL(t, target)
	matches, err := filepath.Glob(filepath.Join(directory, ".agent.json.*.tmp"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary files = %v, %v", matches, err)
	}
}

func TestAtomicReplaceRetriesOnlyTransientWindowsFailures(t *testing.T) {
	attempts := 0
	err := retryAtomicReplace(func() error {
		attempts++
		if attempts < 3 {
			return windows.ERROR_SHARING_VIOLATION
		}
		return nil
	}, func(time.Duration) {})
	if err != nil || attempts != 3 {
		t.Fatalf("transient replace = %v after %d attempts, want success after 3", err, attempts)
	}

	attempts = 0
	permanent := windows.ERROR_INVALID_PARAMETER
	err = retryAtomicReplace(func() error { attempts++; return permanent }, func(time.Duration) {})
	if !errors.Is(err, permanent) || attempts != 1 {
		t.Fatalf("permanent replace = %v after %d attempts, want one failure", err, attempts)
	}
}

func assertSecureFileDACL(t *testing.T, path string) {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatal(err)
	}
	sddl := descriptor.String()
	if !strings.Contains(sddl, "D:P") {
		t.Fatalf("DACL is not protected: %s", sddl)
	}
	for _, forbidden := range []string{";;;WD)", ";;;BU)", ";;;AU)", ";;;IU)", ";;;NU)"} {
		if strings.Contains(sddl, forbidden) {
			t.Fatalf("DACL grants forbidden trustee %q: %s", forbidden, sddl)
		}
	}
	trustees := map[string]bool{}
	for _, match := range regexp.MustCompile(`\([AD];[^)]*;;;([^)]+)\)`).FindAllStringSubmatch(sddl, -1) {
		trustees[match[1]] = true
	}
	current := currentTestSID(t)
	if !trustees["SY"] || !trustees["BA"] || (!trustees[current] && !trustees["LA"]) {
		t.Fatalf("DACL trustees = %#v, want SYSTEM, Administrators, current user: %s", trustees, sddl)
	}
}

func currentTestSID(t *testing.T) string {
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
