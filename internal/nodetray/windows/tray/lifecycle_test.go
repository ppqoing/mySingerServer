package tray

import (
	"errors"
	"sync"
	"testing"
	"time"

	"dedup/internal/nodetray/traymodel"
)

type fakeSession struct {
	mu          sync.Mutex
	events      func(nativeEvent)
	runRelease  chan struct{}
	initialized int
	readded     int
	removed     int
	closeCalls  int
	showCalls   int
	menu        []Item
	command     Command
	selected    bool
	initErr     error
	runErr      error
	readdErr    error
}

func newFakeSession() *fakeSession { return &fakeSession{runRelease: make(chan struct{})} }

func (s *fakeSession) Initialize(events func(nativeEvent)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initialized++
	s.events = events
	return s.initErr
}

func (s *fakeSession) Run() error { <-s.runRelease; return s.runErr }
func (s *fakeSession) Readd() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readded++
	return s.readdErr
}
func (s *fakeSession) ShowMenu(items []Item) (Command, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.showCalls++
	s.menu = append([]Item(nil), items...)
	return s.command, s.selected, nil
}
func (s *fakeSession) Send(string, string) error { return nil }
func (s *fakeSession) RequestClose() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeCalls++
	if s.closeCalls == 1 {
		close(s.runRelease)
	}
	return nil
}
func (s *fakeSession) Remove() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removed++
	return nil
}
func (s *fakeSession) emit(event nativeEvent) {
	s.mu.Lock()
	callback := s.events
	s.mu.Unlock()
	callback(event)
}

func TestControllerDispatchesOnlyFixedMenuCommandAndRestoresAfterExplorerRestart(t *testing.T) {
	session := newFakeSession()
	session.command, session.selected = StopAgent, true
	shown := 0
	handled := make(chan Command, 1)
	controller, err := startSession(session, Options{
		Snapshot: func() Snapshot {
			return Snapshot{MachineID: "node-a", Agent: traymodel.ComponentState{Lifecycle: traymodel.Running}}
		},
		Handle:      func(command Command) { handled <- command },
		ShowConsole: func() { shown++ },
	})
	if err != nil {
		t.Fatal(err)
	}

	session.emit(eventDoubleClick)
	session.emit(eventMenuRequested)
	session.emit(eventTaskbarCreated)
	var handledCommand Command
	select {
	case handledCommand = <-handled:
	case <-time.After(time.Second):
		t.Fatal("menu command was not dispatched")
	}
	if shown != 1 || handledCommand != StopAgent {
		t.Fatalf("shown=%d handled=%v", shown, handledCommand)
	}
	session.mu.Lock()
	showCalls, readded, menu := session.showCalls, session.readded, append([]Item(nil), session.menu...)
	session.mu.Unlock()
	if showCalls != 1 || readded != 1 || len(menu) == 0 {
		t.Fatalf("showCalls=%d readded=%d menu=%v", showCalls, readded, menu)
	}

	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
	session.mu.Lock()
	closeCalls, removed := session.closeCalls, session.removed
	session.mu.Unlock()
	if closeCalls != 1 || removed != 1 {
		t.Fatalf("closeCalls=%d removed=%d", closeCalls, removed)
	}
}

func TestLifecycleCommandDispatchDoesNotBlockWindowProc(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	returned := make(chan struct{})
	options := Options{Handle: func(Command) {
		close(started)
		<-release
	}}

	go func() {
		dispatchLifecycleCommand(options, StartAgent)
		close(returned)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler was not dispatched")
	}
	select {
	case <-returned:
	case <-time.After(100 * time.Millisecond):
		close(release)
		t.Fatal("lifecycle dispatch blocked the native event callback")
	}
	close(release)
}

func TestLifecycleCommandPanicIsReportedAndDoesNotStopLaterDispatch(t *testing.T) {
	errorsSeen := make(chan string, 1)
	handled := make(chan Command, 1)
	dispatchLifecycleCommand(Options{
		Handle:  func(Command) { panic("boom") },
		OnError: func(code string) { errorsSeen <- code },
	}, StartAgent)
	select {
	case code := <-errorsSeen:
		if code != "tray_command_failed" {
			t.Fatalf("error code = %q", code)
		}
	case <-time.After(time.Second):
		t.Fatal("command panic was not reported")
	}

	dispatchLifecycleCommand(Options{Handle: func(command Command) { handled <- command }}, StopAgent)
	select {
	case command := <-handled:
		if command != StopAgent {
			t.Fatalf("handled = %q", command)
		}
	case <-time.After(time.Second):
		t.Fatal("later command was not dispatched")
	}
}

func TestControllerReportsStableLifecycleErrorsWithoutRunningCommands(t *testing.T) {
	session := newFakeSession()
	session.runErr = errors.New(`password=hunter2 C:\\secret\\tray.log`)
	var handled []Command
	errorsSeen := make(chan string, 2)
	controller, err := startSession(session, Options{
		Snapshot:    func() Snapshot { return Snapshot{} },
		Handle:      func(command Command) { handled = append(handled, command) },
		ShowConsole: func() {},
		OnError:     func(code string) { errorsSeen <- code },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-errorsSeen:
		if code != "tray_lifecycle_failed" {
			t.Fatalf("error code = %q", code)
		}
	case <-time.After(time.Second):
		t.Fatal("lifecycle error was not reported")
	}
	if len(handled) != 0 {
		t.Fatalf("lifecycle failure ran commands: %v", handled)
	}
}

func TestControllerFailsClosedWhenNativeInitializationFails(t *testing.T) {
	session := newFakeSession()
	session.initErr = errors.New("native unavailable")
	controller, err := startSession(session, Options{Snapshot: func() Snapshot { return Snapshot{} }, Handle: func(Command) {}, ShowConsole: func() {}})
	if controller != nil || !errors.Is(err, ErrUnavailable) || err.Error() != "tray_unavailable" {
		t.Fatalf("controller=%v err=%v", controller, err)
	}
}
