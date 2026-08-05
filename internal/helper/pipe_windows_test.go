package helper

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"dedup/internal/proto"
	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

var helperPipeTestSequence atomic.Uint64

func TestPipeSecurityDescriptorUsesExactDenyThenAllowOrder(t *testing.T) {
	const sid = "S-1-5-21-111-222-333-444"
	const want = "D:(D;;GA;;;NU)(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;S-1-5-21-111-222-333-444)"
	if got := PipeSecurityDescriptor(sid); got != want {
		t.Fatalf("PipeSecurityDescriptor() = %q, want %q", got, want)
	}
}

func TestPipeCurrentUserSIDComesFromProcessToken(t *testing.T) {
	got, err := currentProcessUserSID()
	if err != nil {
		t.Fatalf("currentProcessUserSID: %v", err)
	}
	var token windows.Token
	if err := windows.OpenProcessToken(
		windows.CurrentProcess(),
		windows.TOKEN_QUERY,
		&token,
	); err != nil {
		t.Fatalf("OpenProcessToken: %v", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatalf("GetTokenUser: %v", err)
	}
	want := user.User.Sid.String()
	if got != want {
		t.Fatalf("currentProcessUserSID = %q, process token SID = %q", got, want)
	}
}

func TestPipeConfigurationUsesACL64KiBBuffersAndByteStreamMode(t *testing.T) {
	const sid = "S-1-5-21-111-222-333-444"
	cfg := helperPipeConfig(sid)
	if cfg.SecurityDescriptor != PipeSecurityDescriptor(sid) {
		t.Fatalf("SecurityDescriptor = %q", cfg.SecurityDescriptor)
	}
	if cfg.InputBufferSize != 64<<10 || cfg.OutputBufferSize != 64<<10 {
		t.Fatalf(
			"pipe buffers = input %d output %d, want 65536/65536",
			cfg.InputBufferSize,
			cfg.OutputBufferSize,
		)
	}
	if cfg.MessageMode {
		t.Fatal("pipe config is message mode, want byte-stream mode")
	}
}

func TestPipeListenRejectsNonLocalOrMalformedNames(t *testing.T) {
	runID := fmt.Sprintf("%d-%d", time.Now().UnixNano(), time.Now().Nanosecond())
	for _, name := range []string{
		`\\server\pipe\dedup-` + runID,
		`\\?\pipe\dedup-` + runID,
		`\\.\pipe\dedup\` + runID,
		`\\.\pipe\dedup ` + runID,
		`\\.\pipe\` + strings.Repeat("x", 129),
	} {
		t.Run(name, func(t *testing.T) {
			cfg := Config{PipeName: name}
			listener, err := ListenPipe(cfg)
			if err == nil {
				_ = listener.Close()
				t.Fatalf("ListenPipe accepted invalid local pipe name %q", name)
			}
		})
	}
}

func TestPipeCurrentUserCanConnectAndClosedListenerLeavesNoPipe(t *testing.T) {
	name := uniqueHelperPipeName()
	listener, err := ListenPipe(Config{PipeName: name})
	if err != nil {
		t.Fatalf("ListenPipe: %v", err)
	}
	acceptCh := make(chan pipeAcceptResult, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		acceptCh <- pipeAcceptResult{conn: conn, err: acceptErr}
	}()

	client := dialHelperPipe(t, name)
	accepted := waitPipeAccept(t, acceptCh)
	if _, messageMode := client.(interface{ CloseWrite() error }); messageMode {
		t.Fatal("client exposes CloseWrite, pipe was created in message mode")
	}
	if _, messageMode := accepted.(interface{ CloseWrite() error }); messageMode {
		t.Fatal("server exposes CloseWrite, pipe was created in message mode")
	}
	if err := client.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := accepted.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write([]byte("local-current-user")); err != nil {
		t.Fatalf("client write: %v", err)
	}
	buffer := make([]byte, len("local-current-user"))
	if _, err := accepted.Read(buffer); err != nil {
		t.Fatalf("server read: %v", err)
	}
	if string(buffer) != "local-current-user" {
		t.Fatalf("server read %q", buffer)
	}
	_ = client.Close()
	_ = accepted.Close()
	if err := listener.Close(); err != nil {
		t.Fatalf("listener Close: %v", err)
	}
	assertHelperPipeUnavailable(t, name)
}

func TestPipeCurrentUserReceivesExactServerHello(t *testing.T) {
	name := uniqueHelperPipeName()
	listener, err := ListenPipe(Config{PipeName: name})
	if err != nil {
		t.Fatalf("ListenPipe: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	logger, _ := testServerLogger()
	server := NewServer(
		Config{
			PipeName:             name,
			FrameReadTimeoutSec:  60,
			FrameWriteTimeoutSec: 60,
		},
		listener,
		nil,
		logger,
	)
	run := startTestServer(t, server)
	client := proto.NewConn(dialHelperPipe(t, name))
	t.Cleanup(func() { _ = client.Close() })

	hello := readHelperHello(t, client)
	if hello.Role != HelperRole ||
		hello.Version != proto.ProtocolVersion ||
		hello.PID != os.Getpid() {
		t.Fatalf("real-pipe Hello = %#v", hello)
	}

	_ = client.Close()
	run.stop(t)
	assertHelperPipeUnavailable(t, name)
}

func TestPipeNetworkRestrictedTokenIsDeniedAndThreadTokenIsRestored(t *testing.T) {
	name := uniqueHelperPipeName()
	listener, err := ListenPipe(Config{PipeName: name})
	if err != nil {
		t.Fatalf("ListenPipe: %v", err)
	}
	defer listener.Close()

	acceptCh := make(chan pipeAcceptResult, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		acceptCh <- pipeAcceptResult{conn: conn, err: acceptErr}
	}()

	dialErr, cleanupErr := dialHelperPipeWithNetworkRestrictedToken(name)
	if cleanupErr != nil {
		t.Fatalf("restricted token cleanup: %v", cleanupErr)
	}
	if !errors.Is(dialErr, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf(
			"NETWORK restricted token dial error = %v, want ERROR_ACCESS_DENIED",
			dialErr,
		)
	}

	client := dialHelperPipe(t, name)
	accepted := waitPipeAccept(t, acceptCh)
	_ = client.Close()
	_ = accepted.Close()
	if err := listener.Close(); err != nil {
		t.Fatalf("listener Close: %v", err)
	}
	assertHelperPipeUnavailable(t, name)
}

type pipeAcceptResult struct {
	conn net.Conn
	err  error
}

func waitPipeAccept(t *testing.T, results <-chan pipeAcceptResult) net.Conn {
	t.Helper()
	select {
	case result := <-results:
		if result.err != nil {
			t.Fatalf("Accept: %v", result.err)
		}
		return result.conn
	case <-time.After(3 * time.Second):
		t.Fatal("Accept did not complete")
		return nil
	}
}

func uniqueHelperPipeName() string {
	return fmt.Sprintf(
		`\\.\pipe\dedup-helper-test-%d-%d-%d`,
		os.Getpid(),
		time.Now().UnixNano(),
		helperPipeTestSequence.Add(1),
	)
}

func dialHelperPipe(t *testing.T, name string) net.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := winio.DialPipeContext(ctx, name)
	if err != nil {
		t.Fatalf("DialPipeContext(%q): %v", name, err)
	}
	return conn
}

func assertHelperPipeUnavailable(t *testing.T, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	conn, err := winio.DialPipeContext(ctx, name)
	if conn != nil {
		_ = conn.Close()
		t.Fatalf("pipe %q remained dialable after teardown", name)
	}
	if err == nil {
		t.Fatalf("pipe %q returned no connection and no error", name)
	}
}

var testCreateRestrictedToken = windows.NewLazySystemDLL("advapi32.dll").
	NewProc("CreateRestrictedToken")

func dialHelperPipeWithNetworkRestrictedToken(
	name string,
) (dialErr, cleanupErr error) {
	type result struct {
		dialErr    error
		cleanupErr error
	}
	resultCh := make(chan result, 1)
	go func() {
		attemptedDialErr, attemptedCleanupErr := func() (
			dialErr error,
			cleanupErr error,
		) {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()

			var existing windows.Token
			beforeErr := windows.OpenThreadToken(
				windows.CurrentThread(),
				windows.TOKEN_QUERY|windows.TOKEN_IMPERSONATE,
				true,
				&existing,
			)
			if beforeErr == nil {
				_ = existing.Close()
				return nil, errors.New(
					"NEEDS_CONTEXT: dedicated test thread already had an impersonation token",
				)
			}
			if !errors.Is(beforeErr, windows.ERROR_NO_TOKEN) {
				return nil, fmt.Errorf(
					"OpenThreadToken before impersonation: %w",
					beforeErr,
				)
			}

			token, err := makeNetworkRestrictedImpersonationToken()
			if err != nil {
				return nil, err
			}
			defer func() {
				cleanupErr = errors.Join(cleanupErr, token.Close())
			}()
			if err := windows.SetThreadToken(nil, token); err != nil {
				return nil, fmt.Errorf("SetThreadToken: %w", err)
			}
			impersonating := true
			defer func() {
				if impersonating {
					revertErr := windows.RevertToSelf()
					if revertErr != nil {
						cleanupErr = errors.Join(
							cleanupErr,
							fmt.Errorf("deferred RevertToSelf: %w", revertErr),
						)
					}
				}
			}()

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			conn, dialErr := winio.DialPipeContext(ctx, name)
			cancel()
			if conn != nil {
				cleanupErr = errors.Join(cleanupErr, conn.Close())
			}

			revertErr := windows.RevertToSelf()
			impersonating = false
			if revertErr != nil {
				cleanupErr = errors.Join(
					cleanupErr,
					fmt.Errorf("RevertToSelf: %w", revertErr),
				)
			}
			var after windows.Token
			afterErr := windows.OpenThreadToken(
				windows.CurrentThread(),
				windows.TOKEN_QUERY,
				true,
				&after,
			)
			if afterErr == nil {
				cleanupErr = errors.Join(
					cleanupErr,
					after.Close(),
					errors.New("thread impersonation token remained after RevertToSelf"),
				)
			} else if !errors.Is(afterErr, windows.ERROR_NO_TOKEN) {
				cleanupErr = errors.Join(
					cleanupErr,
					fmt.Errorf("OpenThreadToken after RevertToSelf: %w", afterErr),
				)
			}
			return dialErr, cleanupErr
		}()
		resultCh <- result{
			dialErr:    attemptedDialErr,
			cleanupErr: attemptedCleanupErr,
		}
	}()
	got := <-resultCh
	return got.dialErr, got.cleanupErr
}

func makeNetworkRestrictedImpersonationToken() (windows.Token, error) {
	var processToken windows.Token
	if err := windows.OpenProcessToken(
		windows.CurrentProcess(),
		windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE,
		&processToken,
	); err != nil {
		return 0, fmt.Errorf("OpenProcessToken: %w", err)
	}
	defer processToken.Close()

	var impersonationToken windows.Token
	if err := windows.DuplicateTokenEx(
		processToken,
		windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE|windows.TOKEN_IMPERSONATE,
		nil,
		windows.SecurityImpersonation,
		windows.TokenImpersonation,
		&impersonationToken,
	); err != nil {
		return 0, fmt.Errorf("DuplicateTokenEx: %w", err)
	}
	defer impersonationToken.Close()

	networkSID, err := windows.CreateWellKnownSid(windows.WinNetworkSid)
	if err != nil {
		return 0, fmt.Errorf("CreateWellKnownSid(NETWORK): %w", err)
	}
	restriction := windows.SIDAndAttributes{
		Sid: networkSID,
	}
	var restricted windows.Token
	result, _, callErr := testCreateRestrictedToken.Call(
		uintptr(impersonationToken),
		0,
		0,
		0,
		0,
		0,
		1,
		uintptr(unsafe.Pointer(&restriction)),
		uintptr(unsafe.Pointer(&restricted)),
	)
	runtime.KeepAlive(networkSID)
	runtime.KeepAlive(restriction)
	if result == 0 {
		if callErr == nil || callErr == syscall.Errno(0) {
			callErr = windows.ERROR_INVALID_FUNCTION
		}
		return 0, fmt.Errorf("CreateRestrictedToken: %w", callErr)
	}
	isRestricted, err := restricted.IsRestricted()
	if err != nil {
		_ = restricted.Close()
		return 0, fmt.Errorf("IsRestricted: %w", err)
	}
	containsNetwork, err := restrictedTokenContainsSID(restricted, networkSID)
	if err != nil {
		_ = restricted.Close()
		return 0, err
	}
	if !isRestricted || !containsNetwork {
		_ = restricted.Close()
		return 0, errors.New(
			"NEEDS_CONTEXT: CreateRestrictedToken did not retain NETWORK as a restricting SID",
		)
	}
	return restricted, nil
}

func restrictedTokenContainsSID(
	token windows.Token,
	want *windows.SID,
) (bool, error) {
	var size uint32
	err := windows.GetTokenInformation(
		token,
		windows.TokenRestrictedSids,
		nil,
		0,
		&size,
	)
	if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
		return false, fmt.Errorf("GetTokenInformation(size): %w", err)
	}
	buffer := make([]byte, size)
	if err := windows.GetTokenInformation(
		token,
		windows.TokenRestrictedSids,
		&buffer[0],
		uint32(len(buffer)),
		&size,
	); err != nil {
		return false, fmt.Errorf("GetTokenInformation(restricted SIDs): %w", err)
	}
	groups := (*windows.Tokengroups)(unsafe.Pointer(&buffer[0]))
	for _, group := range groups.AllGroups() {
		if group.Sid != nil && group.Sid.Equals(want) {
			runtime.KeepAlive(buffer)
			return true, nil
		}
	}
	runtime.KeepAlive(buffer)
	return false, nil
}
