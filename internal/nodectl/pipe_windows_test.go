//go:build windows

package nodectl

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestHelperPipeName(t *testing.T) {
	if got, want := HelperPipeName(), `\\.\pipe\mysingerserver-helper-control-v1`; got != want {
		t.Fatalf("HelperPipeName() = %q, want %q", got, want)
	}
}

func TestHelperPipeACL(t *testing.T) {
	for _, name := range []string{HelperPipeName()} {
		t.Run(name, func(t *testing.T) {
			listener, err := Listen(name)
			if err != nil {
				t.Fatalf("Listen(%q): %v", name, err)
			}
			t.Cleanup(func() { _ = listener.Close() })

			accepted := make(chan pipeAcceptResult, 1)
			go func() {
				conn, acceptErr := listener.Accept()
				accepted <- pipeAcceptResult{conn: conn, err: acceptErr}
			}()
			sddl := readPipeDACL(t, name)
			select {
			case result := <-accepted:
				if result.err != nil {
					t.Fatalf("Accept(%q): %v", name, result.err)
				}
				if err := result.conn.Close(); err != nil {
					t.Fatalf("accepted connection Close(%q): %v", name, err)
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("Accept(%q) did not complete", name)
			}
			currentSID := currentTestProcessUserSID(t)
			for _, sid := range []string{currentSID, "BA", "SY"} {
				if !allowsReadWrite(sddl, sid) {
					t.Fatalf("pipe DACL %q does not grant %s generic read/write", sddl, sid)
				}
			}
			if allowsAnyAccess(sddl, "NU") {
				t.Fatalf("pipe DACL %q grants NETWORK", sddl)
			}
			if allowsAnyAccess(sddl, "WD") {
				t.Fatalf("pipe DACL %q grants Everyone", sddl)
			}
		})
	}
}

type pipeAcceptResult struct {
	conn net.Conn
	err  error
}

func allowsReadWrite(sddl, sid string) bool {
	if strings.Contains(sddl, "(A;;GA;;;"+sid+")") ||
		strings.Contains(sddl, "(A;;FA;;;"+sid+")") {
		return true
	}
	parsed, err := windows.StringToSid(sid)
	if err != nil || !parsed.IsWellKnown(windows.WinAccountAdministratorSid) {
		return false
	}
	return strings.Contains(sddl, "(A;;GA;;;LA)") ||
		strings.Contains(sddl, "(A;;FA;;;LA)")
}

func allowsAnyAccess(sddl, sid string) bool {
	for _, ace := range strings.Split(sddl, "(") {
		if strings.HasPrefix(ace, "A;;") && strings.HasSuffix(ace, ";;;"+sid+")") {
			return true
		}
	}
	return false
}

func TestPipeListenRejectsNonFrozenName(t *testing.T) {
	listener, err := Listen(`\\.\pipe\mysingerserver-test-untrusted`)
	if err == nil {
		_ = listener.Close()
		t.Fatal("Listen accepted a non-frozen pipe name")
	}
}

func TestHelperPipeDoesNotPermitSecondListener(t *testing.T) {
	for _, name := range []string{HelperPipeName()} {
		t.Run(name, func(t *testing.T) {
			first, err := Listen(name)
			if err != nil {
				t.Fatalf("first Listen(%q): %v", name, err)
			}
			t.Cleanup(func() { _ = first.Close() })

			second, err := Listen(name)
			if err == nil {
				_ = second.Close()
				t.Fatal("second Listen unexpectedly succeeded")
			}
		})
	}
}

func TestHelperPipeDialUsesCallerContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn, err := Dial(ctx, HelperPipeName())
	if conn != nil {
		_ = conn.Close()
		t.Fatal("Dial returned a connection for a canceled context")
	}
	if err == nil {
		t.Fatal("Dial returned nil error for a canceled context")
	}
}

func readPipeDACL(t *testing.T, name string) string {
	t.Helper()
	path, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatalf("UTF16PtrFromString(%q): %v", name, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		handle, openErr := windows.CreateFile(
			path,
			windows.GENERIC_READ|windows.READ_CONTROL,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
			nil,
			windows.OPEN_EXISTING,
			windows.FILE_FLAG_OVERLAPPED,
			0,
		)
		if openErr == nil {
			defer windows.CloseHandle(handle)
			descriptor, descriptorErr := windows.GetSecurityInfo(
				handle,
				windows.SE_KERNEL_OBJECT,
				windows.DACL_SECURITY_INFORMATION,
			)
			if descriptorErr != nil {
				t.Fatalf("GetSecurityInfo(%q): %v", name, descriptorErr)
			}
			return descriptor.String()
		}
		if !errors.Is(openErr, windows.ERROR_PIPE_BUSY) && !errors.Is(openErr, windows.ERROR_FILE_NOT_FOUND) {
			t.Fatalf("CreateFile(%q): %v", name, openErr)
		}
		if time.Now().After(deadline) {
			t.Fatalf("CreateFile(%q) did not become available: %v", name, openErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func currentTestProcessUserSID(t *testing.T) string {
	t.Helper()
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		t.Fatalf("OpenProcessToken: %v", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatalf("GetTokenUser: %v", err)
	}
	return user.User.Sid.String()
}
