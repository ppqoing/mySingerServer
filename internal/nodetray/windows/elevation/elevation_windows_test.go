//go:build windows

package elevation

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	nodeprocess "dedup/internal/nodetray/process"
	"golang.org/x/sys/windows"
)

func TestNativeServerDialOpensOverlappedDeadlineHandleAndHonorsBusyContext(t *testing.T) {
	t.Run("overlapped handle supports deadline", func(t *testing.T) {
		local, remote := net.Pipe()
		defer remote.Close()
		backend := &fakeNativeDialBackend{file: local, handle: windows.Handle(71)}
		connection, err := (nativeServerPlatform{backend: backend}).Dial(context.Background(), `\\.\pipe\fake-overlapped`)
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		defer connection.Close()
		if backend.flags&windows.FILE_FLAG_OVERLAPPED == 0 {
			t.Fatalf("CreateFile flags %#x omit FILE_FLAG_OVERLAPPED", backend.flags)
		}
		deadline := time.Now().Add(time.Second)
		if err := connection.SetDeadline(deadline); err != nil {
			t.Fatalf("SetDeadline on injected overlapped handle: %v", err)
		}
	})

	t.Run("busy retries stop at context", func(t *testing.T) {
		backend := &fakeNativeDialBackend{openErr: windows.ERROR_PIPE_BUSY}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err := (nativeServerPlatform{backend: backend}).Dial(ctx, `\\.\pipe\fake-busy`)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Dial busy error = %v, want context deadline", err)
		}
		if backend.calls == 0 || backend.flags&windows.FILE_FLAG_OVERLAPPED == 0 {
			t.Fatalf("busy open calls=%d flags=%#x", backend.calls, backend.flags)
		}
	})
}

func TestOneShotClientUsesFixedRunasAndBindsPeerIdentity(t *testing.T) {
	self := nodeprocess.Identity{PID: os.Getpid(), StartedAtUnixMS: 100, ExecutablePath: `C:\Program Files\MySingerServer\nodetray.exe`}
	child := nodeprocess.Identity{PID: 4242, StartedAtUnixMS: 200, ExecutablePath: self.ExecutablePath}
	inspector := newFakeInspector(self, child)
	childExited := make(chan struct{})
	close(childExited)
	inspector.waitBlock[child.PID] = childExited
	platform := newFakeClientPlatform(child.PID, func(request Request) Response {
		return Response{Version: ProtocolVersion, Nonce: request.Nonce, OK: true}
	})
	inspector.handleIdentity = child
	inspector.handle = platform.process
	platform.beforePeerPID = func() error {
		if platform.process.isClosed() {
			return errors.New("launch handle closed before peer binding")
		}
		if !inspector.handleInspected {
			return errors.New("peer binding ran before handle identity capture")
		}
		return nil
	}
	platform.duringSession = func() {
		if platform.process.isClosed() {
			t.Error("launch handle closed while request session was active")
		}
	}
	client, err := newClientWithBackend(self.ExecutablePath, inspector, platform, time.Second)
	if err != nil {
		t.Fatalf("newClientWithBackend: %v", err)
	}

	secretPayload := []byte(`{"password":"must-not-reach-command-line","target_path":"C:\\private\\helper.json"}`)
	result, err := client.Invoke(context.Background(), ActionWriteHelperConfig, secretPayload)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.UACCancelled || !result.Response.OK {
		t.Fatalf("unexpected result: %#v", result)
	}
	if platform.launchVerb != "runas" || platform.launchExecutable != self.ExecutablePath || !platform.requestProcessHandle {
		t.Fatalf("unsafe launch contract: verb=%q executable=%q processHandle=%v", platform.launchVerb, platform.launchExecutable, platform.requestProcessHandle)
	}
	for _, forbidden := range []string{"must-not-reach", "private", "password", "target_path", string(ActionWriteHelperConfig)} {
		if strings.Contains(strings.ToLower(platform.launchArguments), strings.ToLower(forbidden)) {
			t.Fatalf("launch arguments leaked %q: %q", forbidden, platform.launchArguments)
		}
	}
	if !strings.HasPrefix(platform.pipeName, `\\.\pipe\mysingerserver-elevate-`) {
		t.Fatalf("unexpected pipe name %q", platform.pipeName)
	}
	if platform.pipeSDDL == "" || !strings.Contains(platform.pipeSDDL, "SY") || !strings.Contains(platform.pipeSDDL, "BA") {
		t.Fatalf("pipe SDDL is not restricted: %q", platform.pipeSDDL)
	}
	if platform.received.Nonce == "" || platform.received.Action != ActionWriteHelperConfig || string(platform.received.Payload) != string(secretPayload) {
		t.Fatalf("pipe request mismatch: %#v", platform.received)
	}
	if !platform.process.isClosed() {
		t.Fatal("launch handle was not closed after session completion")
	}
}

func TestOneShotClientUsesLaunchHandleIdentityAndRejectsFastExitPIDReuse(t *testing.T) {
	self := nodeprocess.Identity{PID: os.Getpid(), StartedAtUnixMS: 100, ExecutablePath: `C:\Program Files\MySingerServer\nodetray.exe`}
	launched := nodeprocess.Identity{PID: 4242, StartedAtUnixMS: 200, ExecutablePath: self.ExecutablePath}
	reused := nodeprocess.Identity{PID: launched.PID, StartedAtUnixMS: 900, ExecutablePath: self.ExecutablePath}
	inspector := newFakeInspector(self, reused)
	platform := newFakeClientPlatform(reused.PID, func(request Request) Response {
		return Response{Version: ProtocolVersion, Nonce: request.Nonce, OK: true}
	})
	inspector.handleIdentity = launched
	inspector.handle = platform.process
	client, err := newClientWithBackend(self.ExecutablePath, inspector, platform, time.Second)
	if err != nil {
		t.Fatalf("newClientWithBackend: %v", err)
	}

	if _, err := client.Invoke(context.Background(), ActionRemoveHelperTask, nil); err == nil || !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("PID reuse error = %v, want identity mismatch", err)
	}
	if !inspector.handleInspected {
		t.Fatal("launch identity was not captured from the persistent process handle")
	}
	if !platform.process.isClosed() {
		t.Fatal("launch handle leaked after PID reuse rejection")
	}
}

func TestOneShotClientMapsCancelAndRejectsTimeoutIdentityAndResponseMismatch(t *testing.T) {
	self := nodeprocess.Identity{PID: os.Getpid(), StartedAtUnixMS: 100, ExecutablePath: `C:\Program Files\MySingerServer\nodetray.exe`}
	child := nodeprocess.Identity{PID: 4242, StartedAtUnixMS: 200, ExecutablePath: self.ExecutablePath}

	t.Run("UAC cancelled", func(t *testing.T) {
		platform := newFakeClientPlatform(child.PID, nil)
		platform.launchErr = errPlatformUACCancelled
		client, err := newClientWithBackend(self.ExecutablePath, newFakeInspector(self, child), platform, time.Second)
		if err != nil {
			t.Fatalf("newClientWithBackend: %v", err)
		}
		result, err := client.Invoke(context.Background(), ActionRemoveHelperTask, nil)
		if err != nil {
			t.Fatalf("Invoke cancellation: %v", err)
		}
		if !result.UACCancelled || result.Response.OK {
			t.Fatalf("cancellation was not typed: %#v", result)
		}
	})

	t.Run("overall timeout", func(t *testing.T) {
		platform := newFakeClientPlatform(child.PID, nil)
		platform.acceptBlock = true
		client, err := newClientWithBackend(self.ExecutablePath, newFakeInspector(self, child), platform, 20*time.Millisecond)
		if err != nil {
			t.Fatalf("newClientWithBackend: %v", err)
		}
		started := time.Now()
		if _, err := client.Invoke(context.Background(), ActionRemoveHelperTask, nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Invoke timeout error = %v", err)
		}
		if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
			t.Fatalf("overall timeout was not enforced: %v", elapsed)
		}
		if !platform.process.isClosed() {
			t.Fatal("launch handle leaked after timeout")
		}
	})

	t.Run("peer identity mismatch", func(t *testing.T) {
		impostor := nodeprocess.Identity{PID: 9999, StartedAtUnixMS: 300, ExecutablePath: self.ExecutablePath}
		platform := newFakeClientPlatform(impostor.PID, func(request Request) Response {
			return Response{Version: ProtocolVersion, Nonce: request.Nonce, OK: true}
		})
		inspector := newFakeInspector(self, child, impostor)
		client, err := newClientWithBackend(self.ExecutablePath, inspector, platform, time.Second)
		if err != nil {
			t.Fatalf("newClientWithBackend: %v", err)
		}
		if _, err := client.Invoke(context.Background(), ActionRemoveHelperTask, nil); err == nil {
			t.Fatal("identity mismatch was accepted")
		}
		if !platform.process.isClosed() {
			t.Fatal("launch handle leaked after peer rejection")
		}
	})

	t.Run("response nonce mismatch", func(t *testing.T) {
		platform := newFakeClientPlatform(child.PID, func(Request) Response {
			return Response{Version: ProtocolVersion, Nonce: strings.Repeat("a", 64), OK: true}
		})
		client, err := newClientWithBackend(self.ExecutablePath, newFakeInspector(self, child), platform, time.Second)
		if err != nil {
			t.Fatalf("newClientWithBackend: %v", err)
		}
		if _, err := client.Invoke(context.Background(), ActionRemoveHelperTask, nil); err == nil {
			t.Fatal("response nonce mismatch was accepted")
		}
		if !platform.process.isClosed() {
			t.Fatal("launch handle leaked after response rejection")
		}
	})
}

func TestOneShotServerExecutesOnceRejectsSecondFrameAndStopsWhenParentExits(t *testing.T) {
	self := nodeprocess.Identity{PID: os.Getpid(), StartedAtUnixMS: 100, ExecutablePath: `C:\Program Files\MySingerServer\nodetray.exe`}
	parent := nodeprocess.Identity{PID: 3131, StartedAtUnixMS: 200, ExecutablePath: self.ExecutablePath}
	inspector := newFakeInspector(self, parent)
	nonce := testNonce

	t.Run("one request one response", func(t *testing.T) {
		platform, ordinary := newFakeServerPlatform(parent.PID)
		handler := &fakeHandler{}
		done := make(chan error, 1)
		go func() {
			done <- serveOnceWithBackend(context.Background(), `\\.\pipe\mysingerserver-elevate-`+nonce, nonce, inspector, platform, handler, time.Second)
		}()
		request := Request{Version: ProtocolVersion, Nonce: nonce, Action: ActionRemoveHelperTask}
		if err := WriteRequestFrame(ordinary, request); err != nil {
			t.Fatalf("write first request: %v", err)
		}
		response, err := ReadResponseFrame(ordinary, nonce)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		if !response.OK {
			t.Fatalf("unexpected response: %#v", response)
		}
		if err := <-done; err != nil {
			t.Fatalf("serveOnceWithBackend: %v", err)
		}
		if handler.calls != 1 {
			t.Fatalf("handler calls = %d, want 1", handler.calls)
		}
		_ = ordinary.SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
		if err := WriteRequestFrame(ordinary, request); err == nil {
			t.Fatal("second frame was accepted after one-shot response")
		}
		_ = ordinary.Close()
	})

	t.Run("parent exits before action", func(t *testing.T) {
		platform, ordinary := newFakeServerPlatform(parent.PID)
		handler := &fakeHandler{}
		parentExit := make(chan struct{})
		inspector := newFakeInspector(self, parent)
		inspector.waitBlock[parent.PID] = parentExit
		done := make(chan error, 1)
		go func() {
			done <- serveOnceWithBackend(context.Background(), `\\.\pipe\mysingerserver-elevate-`+nonce, nonce, inspector, platform, handler, time.Second)
		}()
		close(parentExit)
		time.Sleep(10 * time.Millisecond)
		_ = ordinary.SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
		_ = WriteRequestFrame(ordinary, Request{Version: ProtocolVersion, Nonce: nonce, Action: ActionRemoveHelperTask})
		if err := <-done; err == nil {
			t.Fatal("parent exit did not stop elevated server")
		}
		if handler.calls != 0 {
			t.Fatalf("handler executed after parent exit: %d", handler.calls)
		}
		_ = ordinary.Close()
	})
}

func TestOneShotServerBuildsHandlerOnlyFromValidatedParentIdentity(t *testing.T) {
	self := nodeprocess.Identity{PID: os.Getpid(), StartedAtUnixMS: 100, ExecutablePath: `C:\Program Files\MySingerServer\nodetray.exe`}
	parent := nodeprocess.Identity{PID: 3131, StartedAtUnixMS: 200, ExecutablePath: self.ExecutablePath}
	nonce := testNonce

	t.Run("validated parent is passed to factory", func(t *testing.T) {
		inspector := newFakeInspector(self, parent)
		platform, ordinary := newFakeServerPlatform(parent.PID)
		defer ordinary.Close()
		handler := &fakeHandler{}
		var received nodeprocess.Identity
		factory := HandlerFactory(func(actual nodeprocess.Identity) (Handler, error) {
			received = actual
			return handler, nil
		})
		done := make(chan error, 1)
		go func() {
			done <- serveOnceWithBackendFactory(context.Background(), `\\.\pipe\mysingerserver-elevate-`+nonce, nonce, inspector, platform, factory, time.Second)
		}()
		if err := WriteRequestFrame(ordinary, Request{Version: ProtocolVersion, Nonce: nonce, Action: ActionRemoveHelperTask}); err != nil {
			t.Fatalf("WriteRequestFrame: %v", err)
		}
		if _, err := ReadResponseFrame(ordinary, nonce); err != nil {
			t.Fatalf("ReadResponseFrame: %v", err)
		}
		if err := <-done; err != nil {
			t.Fatalf("serveOnceWithBackendFactory: %v", err)
		}
		if !nodeprocess.SameProcess(parent, received) || handler.calls != 1 {
			t.Fatalf("factory parent=%#v handler calls=%d", received, handler.calls)
		}
	})

	t.Run("image mismatch rejects before factory", func(t *testing.T) {
		impostor := parent
		impostor.ExecutablePath = `C:\Temp\nodetray.exe`
		inspector := newFakeInspector(self, impostor)
		platform, ordinary := newFakeServerPlatform(impostor.PID)
		defer ordinary.Close()
		factoryCalls := 0
		err := serveOnceWithBackendFactory(context.Background(), `\\.\pipe\mysingerserver-elevate-`+nonce, nonce, inspector, platform, func(nodeprocess.Identity) (Handler, error) {
			factoryCalls++
			return &fakeHandler{}, nil
		}, time.Second)
		if err == nil || factoryCalls != 0 {
			t.Fatalf("mismatched parent err=%v factory calls=%d", err, factoryCalls)
		}
	})
}

func TestOneShotServerCancelsRunningHandlerWhenParentExitsAndWaitsForSafeStop(t *testing.T) {
	self := nodeprocess.Identity{PID: os.Getpid(), StartedAtUnixMS: 100, ExecutablePath: `C:\Program Files\MySingerServer\nodetray.exe`}
	parent := nodeprocess.Identity{PID: 3131, StartedAtUnixMS: 200, ExecutablePath: self.ExecutablePath}
	inspector := newFakeInspector(self, parent)
	parentExit := make(chan struct{})
	inspector.waitBlock[parent.PID] = parentExit
	platform, ordinary := newFakeServerPlatform(parent.PID)
	defer ordinary.Close()
	handler := newCancelAwareHandler()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serveOnceWithBackend(ctx, `\\.\pipe\mysingerserver-elevate-`+testNonce, testNonce, inspector, platform, handler, 2*time.Second)
	}()
	if err := WriteRequestFrame(ordinary, Request{Version: ProtocolVersion, Nonce: testNonce, Action: ActionRemoveHelperTask}); err != nil {
		t.Fatalf("write request: %v", err)
	}
	select {
	case <-handler.started:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("handler did not start")
	}
	close(parentExit)
	select {
	case <-handler.stopped:
	case <-time.After(100 * time.Millisecond):
		cancel()
		t.Fatal("parent exit did not cancel the running handler promptly")
	}
	select {
	case err := <-done:
		if !errors.Is(err, errParentExited) {
			t.Fatalf("ServeOnce error = %v, want parent exit", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("ServeOnce returned before neither safe handler stop nor parent-exit handling")
	}
	if handler.committed {
		t.Fatal("handler committed after its parent exited")
	}
}

type fakeInspector struct {
	identities      map[int]nodeprocess.Identity
	waitBlock       map[int]chan struct{}
	handleIdentity  nodeprocess.Identity
	handle          *fakeProcessHandle
	handleInspected bool
}

func (inspector *fakeInspector) InspectHandle(raw uintptr) (nodeprocess.Identity, error) {
	if raw == 0 || (inspector.handle != nil && inspector.handle.RawHandle() != raw) {
		return nodeprocess.Identity{}, errors.New("missing fake process handle")
	}
	if inspector.handle != nil && inspector.handle.isClosed() {
		return nodeprocess.Identity{}, errors.New("fake process handle was already closed")
	}
	inspector.handleInspected = true
	if inspector.handleIdentity.PID <= 0 {
		return nodeprocess.Identity{}, errors.New("missing fake handle identity")
	}
	return inspector.handleIdentity, nil
}

func newFakeInspector(identities ...nodeprocess.Identity) *fakeInspector {
	result := &fakeInspector{identities: make(map[int]nodeprocess.Identity), waitBlock: make(map[int]chan struct{})}
	for _, identity := range identities {
		result.identities[identity.PID] = identity
		if identity.PID == 4242 && result.handleIdentity.PID == 0 {
			result.handleIdentity = identity
		}
	}
	return result
}

func (inspector *fakeInspector) Inspect(pid int) (nodeprocess.Identity, error) {
	identity, ok := inspector.identities[pid]
	if !ok {
		return nodeprocess.Identity{}, errors.New("missing fake identity")
	}
	return identity, nil
}

func (inspector *fakeInspector) Wait(ctx context.Context, identity nodeprocess.Identity) (int, error) {
	block := inspector.waitBlock[identity.PID]
	if block == nil {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-block:
		return 0, nil
	}
}

type fakePeerConn struct {
	net.Conn
	peerPID       int
	beforePeerPID func() error
}

func (connection *fakePeerConn) PeerPID() (int, error) {
	if connection.beforePeerPID != nil {
		if err := connection.beforePeerPID(); err != nil {
			return 0, err
		}
	}
	return connection.peerPID, nil
}

type fakeListener struct {
	connection peerConnection
	block      bool
	closed     bool
}

func (listener *fakeListener) Accept(ctx context.Context) (peerConnection, error) {
	if listener.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return listener.connection, nil
}

func (listener *fakeListener) Close() error {
	listener.closed = true
	if listener.connection != nil {
		return listener.connection.Close()
	}
	return nil
}

type fakeClientPlatform struct {
	peerPID              int
	responder            func(Request) Response
	listener             *fakeListener
	pipeName             string
	pipeSDDL             string
	launchVerb           string
	launchExecutable     string
	launchArguments      string
	requestProcessHandle bool
	launchErr            error
	acceptBlock          bool
	received             Request
	process              *fakeProcessHandle
	beforePeerPID        func() error
	duringSession        func()
}

func newFakeClientPlatform(peerPID int, responder func(Request) Response) *fakeClientPlatform {
	return &fakeClientPlatform{peerPID: peerPID, responder: responder, process: &fakeProcessHandle{raw: 0x1234}}
}

func (platform *fakeClientPlatform) Listen(_ context.Context, pipeName, sddl string) (oneShotListener, error) {
	platform.pipeName = pipeName
	platform.pipeSDDL = sddl
	server, elevated := net.Pipe()
	platform.listener = &fakeListener{connection: &fakePeerConn{Conn: server, peerPID: platform.peerPID, beforePeerPID: platform.beforePeerPID}, block: platform.acceptBlock}
	if platform.responder != nil {
		go func() {
			defer elevated.Close()
			request, err := ReadRequestFrame(elevated)
			if err != nil {
				return
			}
			platform.received = request
			if platform.duringSession != nil {
				platform.duringSession()
			}
			_ = WriteResponseFrame(elevated, platform.responder(request))
		}()
	}
	return platform.listener, nil
}

func (platform *fakeClientPlatform) LaunchRunas(verb, executable, arguments string, requestProcessHandle bool) (processHandle, error) {
	platform.launchVerb = verb
	platform.launchExecutable = executable
	platform.launchArguments = arguments
	platform.requestProcessHandle = requestProcessHandle
	if platform.launchErr != nil {
		return nil, platform.launchErr
	}
	return platform.process, nil
}

type fakeProcessHandle struct {
	mu     sync.Mutex
	raw    uintptr
	closed bool
}

func (handle *fakeProcessHandle) RawHandle() uintptr { return handle.raw }

func (handle *fakeProcessHandle) Close() error {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.closed {
		return errors.New("fake process handle closed twice")
	}
	handle.closed = true
	return nil
}

func (handle *fakeProcessHandle) isClosed() bool {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	return handle.closed
}

type fakeServerPlatform struct {
	connection peerConnection
}

func newFakeServerPlatform(parentPID int) (*fakeServerPlatform, net.Conn) {
	elevated, ordinary := net.Pipe()
	return &fakeServerPlatform{connection: &fakePeerConn{Conn: elevated, peerPID: parentPID}}, ordinary
}

func (platform *fakeServerPlatform) Dial(context.Context, string) (peerConnection, error) {
	return platform.connection, nil
}

type fakeHandler struct {
	mu    sync.Mutex
	calls int
}

type cancelAwareHandler struct {
	started   chan struct{}
	stopped   chan struct{}
	committed bool
}

func newCancelAwareHandler() *cancelAwareHandler {
	return &cancelAwareHandler{started: make(chan struct{}), stopped: make(chan struct{})}
}

func (handler *cancelAwareHandler) Execute(ctx context.Context, request Request) Response {
	close(handler.started)
	<-ctx.Done()
	close(handler.stopped)
	return Response{
		Version:      ProtocolVersion,
		Nonce:        request.Nonce,
		ErrorCode:    ErrorCodeTimeout,
		ErrorSummary: "operation cancelled",
	}
}

type fakeNativeDialBackend struct {
	file    deadlinePipeFile
	handle  windows.Handle
	openErr error
	flags   uint32
	calls   int
}

func (backend *fakeNativeDialBackend) Open(_ string, flags uint32) (deadlinePipeFile, windows.Handle, error) {
	backend.calls++
	backend.flags = flags
	return backend.file, backend.handle, backend.openErr
}

func (handler *fakeHandler) Execute(_ context.Context, request Request) Response {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	handler.calls++
	return Response{Version: ProtocolVersion, Nonce: request.Nonce, OK: true}
}
