package agentdelete

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dedup/internal/agent"
	"dedup/internal/config"
	"dedup/internal/proto"
)

func TestForwarderSplitsExactlyAndUsesOneConnection(t *testing.T) {
	paths := make([]string, 5000)
	for index := range paths {
		paths[index] = fmt.Sprintf(`C:\inert\file-%04d.jpg`, index)
	}

	var receivedMu sync.Mutex
	var received []proto.DeleteTask
	dialer := newScriptedDialer(func(conn net.Conn) {
		framed := proto.NewConn(conn)
		if err := framed.WriteFrame(proto.MsgHello, &proto.Hello{
			Version: proto.ProtocolVersion,
			PID:     1234,
			Role:    "delete-helper",
		}); err != nil {
			return
		}
		for index := 0; index < 3; index++ {
			messageType, body, err := framed.ReadFrame()
			if err != nil {
				return
			}
			value, err := proto.Decode(messageType, body)
			if err != nil {
				return
			}
			chunk, ok := value.(*proto.DeleteTask)
			if !ok {
				return
			}
			receivedMu.Lock()
			received = append(received, cloneDeleteTask(*chunk))
			receivedMu.Unlock()

			results := make([]proto.DeleteResult, len(chunk.Entries))
			for resultIndex, path := range chunk.Entries {
				results[resultIndex] = proto.DeleteResult{Path: path, OK: true}
			}
			if err := framed.WriteFrame(proto.MsgDeleteReport, &proto.DeleteReport{
				TaskID:  chunk.TaskID,
				Seq:     chunk.Seq,
				LastSeq: chunk.LastSeq,
				Entries: results,
			}); err != nil {
				return
			}
		}
	})
	state := &recordingState{}
	sender := &recordingSender{}
	forwarder := newTestForwarder(dialer, state, sender, nil)

	err := forwarder.Handle(context.Background(), proto.DeleteTask{
		TaskID:    "task-split",
		Mode:      proto.ModeHard,
		Confirmed: true,
		Entries:   paths,
	}, sender.Send)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	dialer.wait(t)

	if got := dialer.callCount(); got != 1 {
		t.Fatalf("dial calls = %d, want 1", got)
	}
	receivedMu.Lock()
	gotChunks := append([]proto.DeleteTask(nil), received...)
	receivedMu.Unlock()
	if got := chunkLengths(gotChunks); !reflect.DeepEqual(got, []int{2000, 2000, 1000}) {
		t.Fatalf("chunk lengths = %v, want [2000 2000 1000]", got)
	}
	for index, chunk := range gotChunks {
		if chunk.TaskID != "task-split" ||
			chunk.Mode != proto.ModeHard ||
			!chunk.Confirmed ||
			chunk.Seq != uint32(index) ||
			chunk.LastSeq != 2 {
			t.Fatalf("chunk %d metadata = %+v", index, chunk)
		}
	}
	if got := state.pathsByCall(); !reflect.DeepEqual(got, [][]string{
		paths[:2000],
		paths[2000:4000],
		paths[4000:],
	}) {
		t.Fatalf("state batches do not match exact chunks")
	}
	if got := sender.reportCount(); got != 3 {
		t.Fatalf("GUI reports = %d, want 3", got)
	}
	if got := dialer.closeCalls(); !reflect.DeepEqual(got, []int32{1}) {
		t.Fatalf("client close calls = %v, want [1]", got)
	}
}

func TestForwarderValidatesHelperHelloBeforeWriting(t *testing.T) {
	tests := []struct {
		name  string
		hello proto.Hello
	}{
		{
			name: "role",
			hello: proto.Hello{
				Version: proto.ProtocolVersion,
				PID:     1234,
				Role:    "agent",
			},
		},
		{
			name: "version",
			hello: proto.Hello{
				Version: proto.ProtocolVersion + 1,
				PID:     1234,
				Role:    "delete-helper",
			},
		},
		{
			name: "pid",
			hello: proto.Hello{
				Version: proto.ProtocolVersion,
				PID:     0,
				Role:    "delete-helper",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requestReads atomic.Int32
			dialer := newScriptedDialer(func(conn net.Conn) {
				framed := proto.NewConn(conn)
				if err := framed.WriteFrame(proto.MsgHello, &test.hello); err != nil {
					return
				}
				if err := conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
					return
				}
				if _, _, err := framed.ReadFrame(); err == nil {
					requestReads.Add(1)
				}
			})
			state := &recordingState{}
			sender := &recordingSender{}
			forwarder := newTestForwarder(dialer, state, sender, nil)

			err := forwarder.Handle(context.Background(), proto.DeleteTask{
				TaskID:  "task-bad-hello",
				Entries: []string{`C:\inert\a.jpg`},
			}, sender.Send)
			if err == nil {
				t.Fatal("Handle returned nil error")
			}
			dialer.wait(t)

			if got := requestReads.Load(); got != 0 {
				t.Fatalf("Helper request reads = %d, want 0", got)
			}
			if got := state.callCount(); got != 0 {
				t.Fatalf("state calls = %d, want 0", got)
			}
			reports := sender.reportsSnapshot()
			if len(reports) != 1 {
				t.Fatalf("GUI reports = %d, want 1", len(reports))
			}
			assertSyntheticReport(t, reports[0], false, []string{`C:\inert\a.jpg`})
		})
	}
}

func TestForwarderOrdersAuditStateAndGUI(t *testing.T) {
	events := &eventLog{}
	auditCapture := &captureHandler{events: events}
	audit := slog.New(auditCapture)
	dialer := newScriptedDialer(func(conn net.Conn) {
		framed := proto.NewConn(conn)
		if err := framed.WriteFrame(proto.MsgHello, &proto.Hello{
			Version: proto.ProtocolVersion,
			PID:     1234,
			Role:    "delete-helper",
		}); err != nil {
			return
		}
		messageType, body, err := framed.ReadFrame()
		if err != nil {
			return
		}
		value, err := proto.Decode(messageType, body)
		if err != nil {
			return
		}
		chunk, ok := value.(*proto.DeleteTask)
		if !ok {
			return
		}
		_ = framed.WriteFrame(proto.MsgDeleteReport, &proto.DeleteReport{
			TaskID:  chunk.TaskID,
			Seq:     chunk.Seq,
			LastSeq: chunk.LastSeq,
			Entries: []proto.DeleteResult{
				{Path: chunk.Entries[0], OK: true, ReadonlyCleared: true},
				{
					Path:       chunk.Entries[1],
					ErrCode:    proto.DeleteErrAccessDenied,
					Err:        "denied",
					RecycledTo: `C:\recycle\item`,
				},
			},
		})
	})
	state := &recordingState{events: events}
	sender := &recordingSender{events: events}
	forwarder := newTestForwarder(dialer, state, sender, audit)

	err := forwarder.Handle(context.Background(), proto.DeleteTask{
		TaskID:  "task-order",
		Entries: []string{`C:\inert\a.jpg`, `C:\inert\b.jpg`},
	}, sender.Send)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	dialer.wait(t)

	if got, want := events.snapshot(), []string{
		"audit:delete_physical_result:C:\\inert\\a.jpg",
		"audit:delete_physical_result:C:\\inert\\b.jpg",
		"state:C:\\inert\\a.jpg",
		"gui:0",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("event order = %#v, want %#v", got, want)
	}
	records := auditCapture.recordsSnapshot()
	if len(records) != 2 {
		t.Fatalf("audit records = %d, want 2", len(records))
	}
	assertAuditFields(t, records[0], map[string]any{
		"task_id":          "task-order",
		"machine_id":       "machine-a",
		"seq":              uint64(0),
		"path":             `C:\inert\a.jpg`,
		"mode":             proto.ModeSoft,
		"ok":               true,
		"err_code":         "",
		"err":              "",
		"readonly_cleared": true,
		"recycled_to":      "",
		"uncertain":        false,
	})
	assertAuditFields(t, records[1], map[string]any{
		"task_id":          "task-order",
		"machine_id":       "machine-a",
		"seq":              uint64(0),
		"path":             `C:\inert\b.jpg`,
		"mode":             proto.ModeSoft,
		"ok":               false,
		"err_code":         proto.DeleteErrAccessDenied,
		"err":              "denied",
		"readonly_cleared": false,
		"recycled_to":      `C:\recycle\item`,
		"uncertain":        false,
	})
}

func TestForwarderRejectsInvalidInputBeforeSideEffects(t *testing.T) {
	validTask := proto.DeleteTask{
		TaskID:  "task-invalid",
		Entries: []string{`C:\inert\a.jpg`},
	}
	tests := []struct {
		name           string
		machineID      string
		changeCfg      func(*config.DeleteConfig)
		task           proto.DeleteTask
		nilDialer      bool
		nilState       bool
		nilSender      bool
		typedNilDialer bool
		typedNilState  bool
	}{
		{
			name:      "empty machine ID",
			machineID: "",
			task:      validTask,
		},
		{
			name:      "nil dialer",
			machineID: "machine-a",
			task:      validTask,
			nilDialer: true,
		},
		{
			name:      "nil state",
			machineID: "machine-a",
			task:      validTask,
			nilState:  true,
		},
		{
			name:           "typed nil dialer",
			machineID:      "machine-a",
			task:           validTask,
			typedNilDialer: true,
		},
		{
			name:          "typed nil state",
			machineID:     "machine-a",
			task:          validTask,
			typedNilState: true,
		},
		{
			name:      "nil sender",
			machineID: "machine-a",
			task:      validTask,
			nilSender: true,
		},
		{
			name:      "zero maximum",
			machineID: "machine-a",
			changeCfg: func(cfg *config.DeleteConfig) {
				cfg.MaxEntriesPerFrame = 0
			},
			task: validTask,
		},
		{
			name:      "maximum above protocol limit",
			machineID: "machine-a",
			changeCfg: func(cfg *config.DeleteConfig) {
				cfg.MaxEntriesPerFrame = 2001
			},
			task: validTask,
		},
		{
			name:      "zero dial timeout",
			machineID: "machine-a",
			changeCfg: func(cfg *config.DeleteConfig) {
				cfg.DialTimeoutMS = 0
			},
			task: validTask,
		},
		{
			name:      "zero Hello timeout",
			machineID: "machine-a",
			changeCfg: func(cfg *config.DeleteConfig) {
				cfg.HelloTimeoutS = 0
			},
			task: validTask,
		},
		{
			name:      "zero report timeout",
			machineID: "machine-a",
			changeCfg: func(cfg *config.DeleteConfig) {
				cfg.ReportTimeoutS = 0
			},
			task: validTask,
		},
		{
			name:      "split GUI sequence",
			machineID: "machine-a",
			task: proto.DeleteTask{
				TaskID:  "task-invalid",
				Seq:     1,
				Entries: []string{`C:\inert\a.jpg`},
			},
		},
		{
			name:      "split GUI last sequence",
			machineID: "machine-a",
			task: proto.DeleteTask{
				TaskID:  "task-invalid",
				LastSeq: 1,
				Entries: []string{`C:\inert\a.jpg`},
			},
		},
		{
			name:      "empty task",
			machineID: "machine-a",
			task:      proto.DeleteTask{TaskID: "task-invalid"},
		},
		{
			name:      "empty path",
			machineID: "machine-a",
			task: proto.DeleteTask{
				TaskID:  "task-invalid",
				Entries: []string{`C:\inert\a.jpg`, ""},
			},
		},
		{
			name:      "byte-exact duplicate path",
			machineID: "machine-a",
			task: proto.DeleteTask{
				TaskID:  "task-invalid",
				Entries: []string{`C:\inert\a.jpg`, `C:\inert\a.jpg`},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.DefaultAgent().Delete
			if test.changeCfg != nil {
				test.changeCfg(&cfg)
			}
			dialer := &errorDialer{err: errors.New("must not dial")}
			var helperDialer HelperDialer = dialer
			if test.nilDialer {
				helperDialer = nil
			} else if test.typedNilDialer {
				helperDialer = (*nilHelperDialer)(nil)
			}
			state := &recordingState{}
			var stateStore StateStore = state
			if test.nilState {
				stateStore = nil
			} else if test.typedNilState {
				stateStore = (*nilStateStore)(nil)
			}
			sender := &recordingSender{}
			var send agent.Sender = sender.Send
			if test.nilSender {
				send = nil
			}
			auditCapture := &captureHandler{}
			logCapture := &captureHandler{}
			forwarder := NewForwarder(
				test.machineID,
				cfg,
				helperDialer,
				stateStore,
				slog.New(auditCapture),
				slog.New(logCapture),
			)

			if err := forwarder.Handle(context.Background(), test.task, send); err == nil {
				t.Fatal("Handle returned nil error")
			}
			if got := dialer.callCount(); got != 0 {
				t.Fatalf("dial calls = %d, want 0", got)
			}
			if got := state.callCount(); got != 0 {
				t.Fatalf("state calls = %d, want 0", got)
			}
			if got := sender.reportCount(); got != 0 {
				t.Fatalf("GUI sends = %d, want 0", got)
			}
			if got := len(auditCapture.recordsSnapshot()); got != 0 {
				t.Fatalf("audit records = %d, want 0", got)
			}
			if got := len(logCapture.recordsSnapshot()); got != 0 {
				t.Fatalf("general log records = %d, want 0", got)
			}
		})
	}
}

func TestForwarderCopiesEntriesBeforeDialAndKeepsByteExactPaths(t *testing.T) {
	received := make(chan proto.DeleteTask, 1)
	inner := newScriptedDialer(func(conn net.Conn) {
		framed := proto.NewConn(conn)
		if err := framed.WriteFrame(proto.MsgHello, &proto.Hello{
			Version: proto.ProtocolVersion,
			PID:     1234,
			Role:    "delete-helper",
		}); err != nil {
			return
		}
		messageType, body, err := framed.ReadFrame()
		if err != nil {
			return
		}
		value, err := proto.Decode(messageType, body)
		if err != nil {
			return
		}
		chunk, ok := value.(*proto.DeleteTask)
		if !ok {
			return
		}
		received <- cloneDeleteTask(*chunk)
		results := make([]proto.DeleteResult, len(chunk.Entries))
		for index, path := range chunk.Entries {
			results[index] = proto.DeleteResult{Path: path, OK: true}
		}
		_ = framed.WriteFrame(proto.MsgDeleteReport, &proto.DeleteReport{
			TaskID:  chunk.TaskID,
			Seq:     chunk.Seq,
			LastSeq: chunk.LastSeq,
			Entries: results,
		})
	})
	gated := &gatedDialer{
		inner:   inner,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	state := &recordingState{}
	sender := &recordingSender{}
	forwarder := newTestForwarder(gated, state, sender, nil)
	task := proto.DeleteTask{
		TaskID:  "task-copy",
		Entries: []string{`C:\Inert\A.jpg`, `c:\inert\a.jpg`},
	}
	result := make(chan error, 1)
	go func() {
		result <- forwarder.Handle(context.Background(), task, sender.Send)
	}()

	select {
	case <-gated.entered:
	case <-time.After(time.Second):
		t.Fatal("Dial was not reached")
	}
	task.Entries[0] = `C:\mutated-after-copy.jpg`
	close(gated.release)

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Handle did not return")
	}
	inner.wait(t)

	var chunk proto.DeleteTask
	select {
	case chunk = <-received:
	default:
		t.Fatal("Helper did not receive a task")
	}
	if got, want := chunk.Entries, []string{
		`C:\Inert\A.jpg`,
		`c:\inert\a.jpg`,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("received paths = %#v, want %#v", got, want)
	}
	if got := state.pathsByCall(); !reflect.DeepEqual(got, [][]string{{
		`C:\Inert\A.jpg`,
		`c:\inert\a.jpg`,
	}}) {
		t.Fatalf("state calls = %#v", got)
	}
}

func TestForwarderDialAndHelloFailuresCloseEveryChunkDefinite(t *testing.T) {
	tests := []struct {
		name   string
		dialer func(requests *atomic.Int32) HelperDialer
	}{
		{
			name: "dial error",
			dialer: func(*atomic.Int32) HelperDialer {
				return &errorDialer{err: errors.New("offline")}
			},
		},
		{
			name: "nil connection",
			dialer: func(*atomic.Int32) HelperDialer {
				return dialerFunc(func(context.Context) (net.Conn, error) {
					return nil, nil
				})
			},
		},
		{
			name: "wrong frame type",
			dialer: func(requests *atomic.Int32) HelperDialer {
				return newScriptedDialer(func(conn net.Conn) {
					framed := proto.NewConn(conn)
					if framed.WriteFrame(proto.MsgPing, &proto.Ping{TS: 1}) != nil {
						return
					}
					recordUnexpectedRequest(conn, framed, requests)
				})
			},
		},
		{
			name: "malformed Hello body",
			dialer: func(requests *atomic.Int32) HelperDialer {
				return newScriptedDialer(func(conn net.Conn) {
					framed := proto.NewConn(conn)
					if framed.WriteFrame(proto.MsgHello, map[string]any{
						"version": "not-an-integer",
						"pid":     "not-an-integer",
						"role":    []int{1},
					}) != nil {
						return
					}
					recordUnexpectedRequest(conn, framed, requests)
				})
			},
		},
		{
			name: "Hello timeout",
			dialer: func(*atomic.Int32) HelperDialer {
				return newScriptedDialer(func(conn net.Conn) {
					_, _ = io.Copy(io.Discard, conn)
				})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			dialer := test.dialer(&requests)
			state := &recordingState{}
			sender := &recordingSender{}
			auditCapture := &captureHandler{}
			cfg := config.DefaultAgent().Delete
			cfg.MaxEntriesPerFrame = 2
			forwarder := NewForwarder(
				"machine-a",
				cfg,
				dialer,
				state,
				slog.New(auditCapture),
				nil,
			)
			forwarder.helloTimeout = 20 * time.Millisecond

			err := forwarder.Handle(context.Background(), proto.DeleteTask{
				TaskID: "task-offline",
				Entries: []string{
					`C:\inert\a.jpg`,
					`C:\inert\b.jpg`,
					`C:\inert\c.jpg`,
				},
			}, sender.Send)
			if err == nil {
				t.Fatal("Handle returned nil error")
			}
			if scripted, ok := dialer.(*scriptedDialer); ok {
				scripted.wait(t)
				if got := scripted.closeCalls(); !reflect.DeepEqual(got, []int32{1}) {
					t.Fatalf("close calls = %v, want [1]", got)
				}
			}
			if got := requests.Load(); got != 0 {
				t.Fatalf("Helper requests = %d, want 0", got)
			}
			if got := state.callCount(); got != 0 {
				t.Fatalf("state calls = %d, want 0", got)
			}
			reports := sender.reportsSnapshot()
			if len(reports) != 2 {
				t.Fatalf("GUI reports = %d, want 2", len(reports))
			}
			assertReportMetadata(t, reports[0], "task-offline", 0, 1)
			assertReportMetadata(t, reports[1], "task-offline", 1, 1)
			assertSyntheticReport(t, reports[0], false, []string{
				`C:\inert\a.jpg`,
				`C:\inert\b.jpg`,
			})
			assertSyntheticReport(t, reports[1], false, []string{
				`C:\inert\c.jpg`,
			})
			if got := len(auditCapture.recordsSnapshot()); got != 3 {
				t.Fatalf("audit records = %d, want 3", got)
			}
		})
	}
}

func TestForwarderTreatsTypedNilDialConnectionAsDefiniteOffline(t *testing.T) {
	tests := []struct {
		name    string
		dialErr error
	}{
		{name: "nil error"},
		{name: "non-nil error", dialErr: errors.New("dial failed")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			typedNilConnMethodCalls.Store(0)
			events := &eventLog{}
			auditCapture := &captureHandler{events: events}
			dialer := &typedNilConnDialer{err: test.dialErr}
			state := &recordingState{events: events}
			sender := &recordingSender{events: events}
			cfg := config.DefaultAgent().Delete
			cfg.MaxEntriesPerFrame = 2
			forwarder := NewForwarder(
				"machine-a",
				cfg,
				dialer,
				state,
				slog.New(auditCapture),
				nil,
			)
			paths := []string{
				`C:\inert\a.jpg`,
				`C:\inert\b.jpg`,
				`C:\inert\c.jpg`,
			}

			err := forwarder.Handle(context.Background(), proto.DeleteTask{
				TaskID:  "task-typed-nil-connection",
				Entries: paths,
			}, sender.Send)
			if err == nil {
				t.Fatal("Handle returned nil error")
			}
			if test.dialErr != nil && !errors.Is(err, test.dialErr) {
				t.Fatalf("Handle error = %v, want wrapped dial error", err)
			}
			if got := dialer.calls.Load(); got != 1 {
				t.Fatalf("dial calls = %d, want 1", got)
			}
			if got := typedNilConnMethodCalls.Load(); got != 0 {
				t.Fatalf("typed-nil connection method calls = %d, want 0", got)
			}
			if got := state.callCount(); got != 0 {
				t.Fatalf("state calls = %d, want 0", got)
			}
			reports := sender.reportsSnapshot()
			if len(reports) != 2 {
				t.Fatalf("GUI reports = %d, want 2", len(reports))
			}
			assertReportMetadata(
				t,
				reports[0],
				"task-typed-nil-connection",
				0,
				1,
			)
			assertReportMetadata(
				t,
				reports[1],
				"task-typed-nil-connection",
				1,
				1,
			)
			assertSyntheticReport(t, reports[0], false, paths[:2])
			assertSyntheticReport(t, reports[1], false, paths[2:])
			if got := len(auditCapture.recordsSnapshot()); got != 3 {
				t.Fatalf("audit records = %d, want 3", got)
			}
			if got, want := events.snapshot(), []string{
				"audit:delete_physical_result:C:\\inert\\a.jpg",
				"audit:delete_physical_result:C:\\inert\\b.jpg",
				"gui:0",
				"audit:delete_physical_result:C:\\inert\\c.jpg",
				"gui:1",
			}; !reflect.DeepEqual(got, want) {
				t.Fatalf("events = %#v, want %#v", got, want)
			}
		})
	}
}

func TestForwarderAppliesDefault500MillisecondDialBudget(t *testing.T) {
	dialer := &blockingDialer{
		entered: make(chan context.Context, 1),
	}
	state := &recordingState{}
	sender := &recordingSender{}
	forwarder := NewForwarder(
		"machine-a",
		config.DefaultAgent().Delete,
		dialer,
		state,
		nil,
		nil,
	)
	parent, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	started := time.Now()
	go func() {
		result <- forwarder.Handle(parent, proto.DeleteTask{
			TaskID:  "task-dial-budget",
			Entries: []string{`C:\inert\a.jpg`},
		}, sender.Send)
	}()

	var dialContext context.Context
	select {
	case dialContext = <-dialer.entered:
	case <-time.After(time.Second):
		t.Fatal("dial was not called")
	}
	deadline, ok := dialContext.Deadline()
	if !ok {
		t.Fatal("dial context has no deadline")
	}
	remaining := deadline.Sub(started)
	if remaining < 450*time.Millisecond || remaining > 550*time.Millisecond {
		t.Fatalf("dial deadline budget = %v, want 500ms", remaining)
	}
	cancel()

	select {
	case err := <-result:
		if err == nil {
			t.Fatal("Handle returned nil error")
		}
	case <-time.After(time.Second):
		t.Fatal("dial did not unblock on parent cancellation")
	}
	if elapsed := time.Since(started); elapsed >= 400*time.Millisecond {
		t.Fatalf("parent cancellation took %v; dial waited for its full budget", elapsed)
	}
	reports := sender.reportsSnapshot()
	if len(reports) != 1 {
		t.Fatalf("GUI reports = %d, want 1", len(reports))
	}
	assertSyntheticReport(t, reports[0], false, []string{`C:\inert\a.jpg`})
}

func TestForwarderCanonicalizesReorderedPartialReportAndRecomputesStats(t *testing.T) {
	events := &eventLog{}
	auditCapture := &captureHandler{events: events}
	dialer := newScriptedDialer(func(conn net.Conn) {
		framed := proto.NewConn(conn)
		if err := writeValidHello(framed); err != nil {
			return
		}
		chunk, err := readDeleteTask(framed)
		if err != nil {
			return
		}
		_ = framed.WriteFrame(proto.MsgDeleteReport, &proto.DeleteReport{
			TaskID:  chunk.TaskID,
			Seq:     chunk.Seq,
			LastSeq: chunk.LastSeq,
			Stats: proto.DeleteStats{
				Total:     999,
				OK:        999,
				Failed:    999,
				Uncertain: 999,
			},
			Entries: []proto.DeleteResult{
				{
					Path:            chunk.Entries[2],
					OK:              true,
					ReadonlyCleared: true,
					RecycledTo:      `C:\recycle\c`,
				},
				{
					Path: chunk.Entries[0],
					OK:   true,
				},
			},
		})
	})
	state := &recordingState{events: events}
	sender := &recordingSender{events: events}
	forwarder := newTestForwarder(
		dialer,
		state,
		sender,
		slog.New(auditCapture),
	)
	paths := []string{
		`C:\inert\a.jpg`,
		`C:\inert\b.jpg`,
		`C:\inert\c.jpg`,
	}

	if err := forwarder.Handle(context.Background(), proto.DeleteTask{
		TaskID:  "task-partial",
		Mode:    proto.ModeHard,
		Entries: paths,
	}, sender.Send); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	dialer.wait(t)

	reports := sender.reportsSnapshot()
	if len(reports) != 1 {
		t.Fatalf("GUI reports = %d, want 1", len(reports))
	}
	report := reports[0]
	if got, want := report.Stats, (proto.DeleteStats{
		Total:     3,
		OK:        2,
		Failed:    1,
		Uncertain: 1,
	}); got != want {
		t.Fatalf("stats = %+v, want %+v", got, want)
	}
	if got := report.Entries; len(got) != 3 ||
		got[0].Path != paths[0] ||
		!got[0].OK ||
		got[0].Uncertain ||
		got[1].Path != paths[1] ||
		got[1].ErrCode != proto.DeleteErrHelperLost ||
		!got[1].Uncertain ||
		got[2].Path != paths[2] ||
		!got[2].OK ||
		!got[2].ReadonlyCleared ||
		got[2].RecycledTo != `C:\recycle\c` {
		t.Fatalf("canonical partial entries = %+v", got)
	}
	if got, want := state.pathsByCall(), [][]string{{
		paths[0],
		paths[2],
	}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("state calls = %#v, want %#v", got, want)
	}
	if got, want := events.snapshot(), []string{
		"audit:delete_physical_result:C:\\inert\\a.jpg",
		"audit:delete_physical_result:C:\\inert\\b.jpg",
		"audit:delete_physical_result:C:\\inert\\c.jpg",
		"state:C:\\inert\\a.jpg",
		"gui:0",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
	for _, record := range auditCapture.recordsSnapshot() {
		if got := record.attrs["mode"]; got != proto.ModeHard {
			t.Fatalf("audit mode = %#v, want %q", got, proto.ModeHard)
		}
	}
}

func TestForwarderAcceptsEmptyPartialReportAsUncertainSubset(t *testing.T) {
	dialer := newScriptedDialer(func(conn net.Conn) {
		framed := proto.NewConn(conn)
		if writeValidHello(framed) != nil {
			return
		}
		chunk, err := readDeleteTask(framed)
		if err != nil {
			return
		}
		_ = framed.WriteFrame(proto.MsgDeleteReport, &proto.DeleteReport{
			TaskID:  chunk.TaskID,
			Seq:     chunk.Seq,
			LastSeq: chunk.LastSeq,
			Entries: nil,
		})
	})
	state := &recordingState{}
	sender := &recordingSender{}
	forwarder := newTestForwarder(dialer, state, sender, nil)
	paths := []string{`C:\inert\a.jpg`, `C:\inert\b.jpg`}

	if err := forwarder.Handle(context.Background(), proto.DeleteTask{
		TaskID:  "task-empty-subset",
		Entries: paths,
	}, sender.Send); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	dialer.wait(t)

	reports := sender.reportsSnapshot()
	if len(reports) != 1 {
		t.Fatalf("GUI reports = %d, want 1", len(reports))
	}
	assertSyntheticReport(t, reports[0], true, paths)
	if got := state.callCount(); got != 0 {
		t.Fatalf("state calls = %d, want 0", got)
	}
}

func TestForwarderOwnsStateSyncAnnotationsAtAgentBoundary(t *testing.T) {
	localStateErr := errors.New("sqlite busy")
	tests := []struct {
		name     string
		stateErr error
	}{
		{name: "state success"},
		{name: "state failure", stateErr: localStateErr},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dialer := newScriptedDialer(func(conn net.Conn) {
				framed := proto.NewConn(conn)
				if writeValidHello(framed) != nil {
					return
				}
				chunk, err := readDeleteTask(framed)
				if err != nil {
					return
				}
				_ = framed.WriteFrame(
					proto.MsgDeleteReport,
					&proto.DeleteReport{
						TaskID:  chunk.TaskID,
						Seq:     chunk.Seq,
						LastSeq: chunk.LastSeq,
						Stats: proto.DeleteStats{
							Total:     99,
							OK:        99,
							Failed:    99,
							Uncertain: 0,
						},
						Entries: []proto.DeleteResult{
							{
								Path:         chunk.Entries[1],
								ErrCode:      proto.DeleteErrAccessDenied,
								Err:          "denied",
								Uncertain:    false,
								StateSyncErr: "forged failed-item SQLite status",
							},
							{
								Path:         chunk.Entries[0],
								OK:           true,
								Uncertain:    true,
								StateSyncErr: "forged successful-item SQLite status",
							},
						},
					},
				)
			})
			state := &recordingState{
				errFn: func(int, string, []string) error {
					return test.stateErr
				},
			}
			sender := &recordingSender{}
			forwarder := newTestForwarder(dialer, state, sender, nil)
			paths := []string{`C:\inert\a.jpg`, `C:\inert\b.jpg`}

			err := forwarder.Handle(context.Background(), proto.DeleteTask{
				TaskID:  "task-state-ownership",
				Entries: paths,
			}, sender.Send)
			if test.stateErr == nil {
				if err != nil {
					t.Fatalf("Handle: %v", err)
				}
			} else if !errors.Is(err, test.stateErr) {
				t.Fatalf("Handle error = %v, want wrapped state error", err)
			}
			dialer.wait(t)

			if got, want := state.pathsByCall(), [][]string{{paths[0]}}; !reflect.DeepEqual(got, want) {
				t.Fatalf("state calls = %#v, want %#v", got, want)
			}
			reports := sender.reportsSnapshot()
			if len(reports) != 1 {
				t.Fatalf("GUI reports = %d, want 1", len(reports))
			}
			report := reports[0]
			if got, want := report.Stats, (proto.DeleteStats{
				Total:     2,
				OK:        1,
				Failed:    1,
				Uncertain: 1,
			}); got != want {
				t.Fatalf("stats = %+v, want %+v", got, want)
			}
			if len(report.Entries) != 2 {
				t.Fatalf("entries = %d, want 2", len(report.Entries))
			}
			success := report.Entries[0]
			failed := report.Entries[1]
			if success.Path != paths[0] ||
				!success.OK ||
				!success.Uncertain {
				t.Fatalf("successful physical result changed: %+v", success)
			}
			if failed.Path != paths[1] ||
				failed.OK ||
				failed.Uncertain ||
				failed.ErrCode != proto.DeleteErrAccessDenied ||
				failed.Err != "denied" {
				t.Fatalf("failed physical result changed: %+v", failed)
			}
			wantSuccessStateErr := ""
			if test.stateErr != nil {
				wantSuccessStateErr = test.stateErr.Error()
			}
			if success.StateSyncErr != wantSuccessStateErr {
				t.Fatalf(
					"successful StateSyncErr = %q, want %q",
					success.StateSyncErr,
					wantSuccessStateErr,
				)
			}
			if failed.StateSyncErr != "" {
				t.Fatalf(
					"failed StateSyncErr = %q, want empty",
					failed.StateSyncErr,
				)
			}
		})
	}
}

func TestForwarderRejectsInvalidReportsWithoutReplay(t *testing.T) {
	tests := []struct {
		name string
		send func(net.Conn, *proto.Conn, proto.DeleteTask)
	}{
		{
			name: "wrong message type",
			send: func(_ net.Conn, framed *proto.Conn, _ proto.DeleteTask) {
				_ = framed.WriteFrame(proto.MsgPong, &proto.Pong{TS: 1})
			},
		},
		{
			name: "malformed frame",
			send: func(conn net.Conn, _ *proto.Conn, _ proto.DeleteTask) {
				writeMalformedFrame(conn)
			},
		},
		{
			name: "malformed report body",
			send: func(_ net.Conn, framed *proto.Conn, chunk proto.DeleteTask) {
				_ = framed.WriteFrame(proto.MsgDeleteReport, map[string]any{
					"task_id":  chunk.TaskID,
					"seq":      "not-an-integer",
					"last_seq": chunk.LastSeq,
					"entries":  []any{},
				})
			},
		},
		{
			name: "wrong task ID",
			send: func(_ net.Conn, framed *proto.Conn, chunk proto.DeleteTask) {
				report := validSuccessReport(chunk)
				report.TaskID = "other-task"
				_ = framed.WriteFrame(proto.MsgDeleteReport, &report)
			},
		},
		{
			name: "wrong sequence",
			send: func(_ net.Conn, framed *proto.Conn, chunk proto.DeleteTask) {
				report := validSuccessReport(chunk)
				report.Seq++
				_ = framed.WriteFrame(proto.MsgDeleteReport, &report)
			},
		},
		{
			name: "wrong last sequence",
			send: func(_ net.Conn, framed *proto.Conn, chunk proto.DeleteTask) {
				report := validSuccessReport(chunk)
				report.LastSeq++
				_ = framed.WriteFrame(proto.MsgDeleteReport, &report)
			},
		},
		{
			name: "foreign path",
			send: func(_ net.Conn, framed *proto.Conn, chunk proto.DeleteTask) {
				report := validSuccessReport(chunk)
				report.Entries[0].Path = `C:\foreign.jpg`
				_ = framed.WriteFrame(proto.MsgDeleteReport, &report)
			},
		},
		{
			name: "duplicate path",
			send: func(_ net.Conn, framed *proto.Conn, chunk proto.DeleteTask) {
				report := validSuccessReport(chunk)
				report.Entries = append(report.Entries, report.Entries[0])
				_ = framed.WriteFrame(proto.MsgDeleteReport, &report)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			dialer := newScriptedDialer(func(conn net.Conn) {
				framed := proto.NewConn(conn)
				if writeValidHello(framed) != nil {
					return
				}
				chunk, err := readDeleteTask(framed)
				if err != nil {
					return
				}
				requests.Add(1)
				test.send(conn, framed, chunk)
				if conn.SetReadDeadline(time.Now().Add(100*time.Millisecond)) != nil {
					return
				}
				if _, _, err := framed.ReadFrame(); err == nil {
					requests.Add(1)
				}
			})
			cfg := config.DefaultAgent().Delete
			cfg.MaxEntriesPerFrame = 1
			state := &recordingState{}
			sender := &recordingSender{}
			forwarder := NewForwarder(
				"machine-a",
				cfg,
				dialer,
				state,
				nil,
				nil,
			)
			forwarder.helloTimeout = 250 * time.Millisecond
			forwarder.reportTimeout = 250 * time.Millisecond
			paths := []string{`C:\inert\a.jpg`, `C:\inert\b.jpg`}

			err := forwarder.Handle(context.Background(), proto.DeleteTask{
				TaskID:  "task-invalid-report",
				Entries: paths,
			}, sender.Send)
			if err == nil {
				t.Fatal("Handle returned nil error")
			}
			dialer.wait(t)

			if got := dialer.callCount(); got != 1 {
				t.Fatalf("dial calls = %d, want 1", got)
			}
			if got := requests.Load(); got != 1 {
				t.Fatalf("Helper request count = %d, want 1", got)
			}
			if got := state.callCount(); got != 0 {
				t.Fatalf("state calls = %d, want 0", got)
			}
			reports := sender.reportsSnapshot()
			if len(reports) != 2 {
				t.Fatalf("GUI reports = %d, want 2", len(reports))
			}
			assertSyntheticReport(t, reports[0], true, paths[:1])
			assertSyntheticReport(t, reports[1], false, paths[1:])
			assertReportMetadata(t, reports[0], "task-invalid-report", 0, 1)
			assertReportMetadata(t, reports[1], "task-invalid-report", 1, 1)
			if got := dialer.closeCalls(); !reflect.DeepEqual(got, []int32{1}) {
				t.Fatalf("close calls = %v, want [1]", got)
			}
		})
	}
}

func TestForwarderClassifiesPostWriteFailuresAndStopsLaterChunks(t *testing.T) {
	tests := []struct {
		name         string
		newDialer    func(requests *atomic.Int32) *scriptedDialer
		wantRequests int32
		reportWait   time.Duration
	}{
		{
			name: "request write failure",
			newDialer: func(requests *atomic.Int32) *scriptedDialer {
				return newWrappedScriptedDialer(
					func(conn net.Conn) {
						framed := proto.NewConn(conn)
						if writeValidHello(framed) != nil {
							return
						}
						recordUnexpectedRequest(conn, framed, requests)
					},
					func(conn net.Conn) net.Conn {
						return &writeFailConn{Conn: conn}
					},
				)
			},
			wantRequests: 0,
			reportWait:   250 * time.Millisecond,
		},
		{
			name: "connection loss after request",
			newDialer: func(requests *atomic.Int32) *scriptedDialer {
				return newScriptedDialer(func(conn net.Conn) {
					framed := proto.NewConn(conn)
					if writeValidHello(framed) != nil {
						return
					}
					if _, err := readDeleteTask(framed); err == nil {
						requests.Add(1)
					}
				})
			},
			wantRequests: 1,
			reportWait:   250 * time.Millisecond,
		},
		{
			name: "report timeout after request",
			newDialer: func(requests *atomic.Int32) *scriptedDialer {
				return newScriptedDialer(func(conn net.Conn) {
					framed := proto.NewConn(conn)
					if writeValidHello(framed) != nil {
						return
					}
					if _, err := readDeleteTask(framed); err != nil {
						return
					}
					requests.Add(1)
					_, _ = io.Copy(io.Discard, conn)
				})
			},
			wantRequests: 1,
			reportWait:   20 * time.Millisecond,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			dialer := test.newDialer(&requests)
			cfg := config.DefaultAgent().Delete
			cfg.MaxEntriesPerFrame = 1
			state := &recordingState{}
			sender := &recordingSender{}
			forwarder := NewForwarder(
				"machine-a",
				cfg,
				dialer,
				state,
				nil,
				nil,
			)
			forwarder.helloTimeout = 250 * time.Millisecond
			forwarder.reportTimeout = test.reportWait
			paths := []string{`C:\inert\a.jpg`, `C:\inert\b.jpg`}

			err := forwarder.Handle(context.Background(), proto.DeleteTask{
				TaskID:  "task-post-write-loss",
				Entries: paths,
			}, sender.Send)
			if err == nil {
				t.Fatal("Handle returned nil error")
			}
			dialer.wait(t)

			if got := requests.Load(); got != test.wantRequests {
				t.Fatalf("Helper requests = %d, want %d", got, test.wantRequests)
			}
			if got := dialer.callCount(); got != 1 {
				t.Fatalf("dial calls = %d, want 1", got)
			}
			reports := sender.reportsSnapshot()
			if len(reports) != 2 {
				t.Fatalf("GUI reports = %d, want 2", len(reports))
			}
			assertSyntheticReport(t, reports[0], true, paths[:1])
			assertSyntheticReport(t, reports[1], false, paths[1:])
			if got := state.callCount(); got != 0 {
				t.Fatalf("state calls = %d, want 0", got)
			}
			if got := dialer.closeCalls(); !reflect.DeepEqual(got, []int32{1}) {
				t.Fatalf("close calls = %v, want [1]", got)
			}
		})
	}
}

func TestForwarderContextCancellationUnblocksHelloAndReportReads(t *testing.T) {
	t.Run("Hello", func(t *testing.T) {
		connected := make(chan struct{})
		dialer := newScriptedDialer(func(conn net.Conn) {
			close(connected)
			_, _ = io.Copy(io.Discard, conn)
		})
		state := &recordingState{}
		sender := &recordingSender{}
		forwarder := newTestForwarder(dialer, state, sender, nil)
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			result <- forwarder.Handle(ctx, proto.DeleteTask{
				TaskID:  "task-cancel-hello",
				Entries: []string{`C:\inert\a.jpg`},
			}, sender.Send)
		}()
		select {
		case <-connected:
		case <-time.After(time.Second):
			t.Fatal("Helper connection was not established")
		}
		cancel()
		select {
		case err := <-result:
			if err == nil {
				t.Fatal("Handle returned nil error")
			}
		case <-time.After(time.Second):
			t.Fatal("Hello read was not unblocked")
		}
		dialer.wait(t)
		reports := sender.reportsSnapshot()
		if len(reports) != 1 {
			t.Fatalf("GUI reports = %d, want 1", len(reports))
		}
		assertSyntheticReport(t, reports[0], false, []string{`C:\inert\a.jpg`})
		if got := dialer.closeCalls(); !reflect.DeepEqual(got, []int32{1}) {
			t.Fatalf("close calls = %v, want [1]", got)
		}
	})

	t.Run("report", func(t *testing.T) {
		requestRead := make(chan struct{})
		dialer := newScriptedDialer(func(conn net.Conn) {
			framed := proto.NewConn(conn)
			if writeValidHello(framed) != nil {
				return
			}
			if _, err := readDeleteTask(framed); err != nil {
				return
			}
			close(requestRead)
			_, _ = io.Copy(io.Discard, conn)
		})
		cfg := config.DefaultAgent().Delete
		cfg.MaxEntriesPerFrame = 1
		state := &recordingState{}
		sender := &recordingSender{}
		forwarder := NewForwarder(
			"machine-a",
			cfg,
			dialer,
			state,
			nil,
			nil,
		)
		forwarder.helloTimeout = 250 * time.Millisecond
		forwarder.reportTimeout = time.Second
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		paths := []string{`C:\inert\a.jpg`, `C:\inert\b.jpg`}
		go func() {
			result <- forwarder.Handle(ctx, proto.DeleteTask{
				TaskID:  "task-cancel-report",
				Entries: paths,
			}, sender.Send)
		}()
		select {
		case <-requestRead:
		case <-time.After(time.Second):
			t.Fatal("Helper did not read the request")
		}
		cancel()
		select {
		case err := <-result:
			if err == nil {
				t.Fatal("Handle returned nil error")
			}
		case <-time.After(time.Second):
			t.Fatal("report read was not unblocked")
		}
		dialer.wait(t)
		reports := sender.reportsSnapshot()
		if len(reports) != 2 {
			t.Fatalf("GUI reports = %d, want 2", len(reports))
		}
		assertSyntheticReport(t, reports[0], true, paths[:1])
		assertSyntheticReport(t, reports[1], false, paths[1:])
		if got := dialer.closeCalls(); !reflect.DeepEqual(got, []int32{1}) {
			t.Fatalf("close calls = %v, want [1]", got)
		}
	})
}

func TestForwarderAnnotatesStateFailureAndContinuesLaterChunks(t *testing.T) {
	events := &eventLog{}
	auditCapture := &captureHandler{events: events}
	logCapture := &captureHandler{}
	dialer := newScriptedDialer(func(conn net.Conn) {
		framed := proto.NewConn(conn)
		if writeValidHello(framed) != nil {
			return
		}
		for index := 0; index < 2; index++ {
			chunk, err := readDeleteTask(framed)
			if err != nil {
				return
			}
			var entries []proto.DeleteResult
			if chunk.Seq == 0 {
				entries = []proto.DeleteResult{
					{
						Path:            chunk.Entries[0],
						OK:              true,
						ReadonlyCleared: true,
						RecycledTo:      `C:\recycle\a`,
					},
					{
						Path:       chunk.Entries[1],
						ErrCode:    proto.DeleteErrAccessDenied,
						Err:        "denied",
						Uncertain:  false,
						RecycledTo: "",
					},
				}
			} else {
				entries = []proto.DeleteResult{{
					Path: chunk.Entries[0],
					OK:   true,
				}}
			}
			if framed.WriteFrame(proto.MsgDeleteReport, &proto.DeleteReport{
				TaskID:  chunk.TaskID,
				Seq:     chunk.Seq,
				LastSeq: chunk.LastSeq,
				Entries: entries,
			}) != nil {
				return
			}
		}
	})
	sqliteErr := errors.New("sqlite busy")
	state := &recordingState{
		events: events,
		errFn: func(call int, _ string, _ []string) error {
			if call == 0 {
				return sqliteErr
			}
			return nil
		},
	}
	sender := &recordingSender{events: events}
	cfg := config.DefaultAgent().Delete
	cfg.MaxEntriesPerFrame = 2
	forwarder := NewForwarder(
		"machine-a",
		cfg,
		dialer,
		state,
		slog.New(auditCapture),
		slog.New(logCapture),
	)
	forwarder.helloTimeout = 250 * time.Millisecond
	forwarder.reportTimeout = 250 * time.Millisecond
	paths := []string{
		`C:\inert\a.jpg`,
		`C:\inert\b.jpg`,
		`C:\inert\c.jpg`,
	}

	err := forwarder.Handle(context.Background(), proto.DeleteTask{
		TaskID:  "task-state-error",
		Entries: paths,
	}, sender.Send)
	if !errors.Is(err, sqliteErr) {
		t.Fatalf("Handle error = %v, want wrapped sqlite error", err)
	}
	dialer.wait(t)

	if got, want := state.pathsByCall(), [][]string{
		{paths[0]},
		{paths[2]},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("state calls = %#v, want %#v", got, want)
	}
	reports := sender.reportsSnapshot()
	if len(reports) != 2 {
		t.Fatalf("GUI reports = %d, want 2", len(reports))
	}
	if first := reports[0].Entries; len(first) != 2 ||
		!first[0].OK ||
		first[0].ErrCode != "" ||
		first[0].Err != "" ||
		first[0].Uncertain ||
		!first[0].ReadonlyCleared ||
		first[0].RecycledTo != `C:\recycle\a` ||
		first[0].StateSyncErr != sqliteErr.Error() ||
		first[1].OK ||
		first[1].ErrCode != proto.DeleteErrAccessDenied ||
		first[1].Err != "denied" ||
		first[1].StateSyncErr != "" {
		t.Fatalf("annotated first report = %+v", first)
	}
	if second := reports[1].Entries; len(second) != 1 ||
		!second[0].OK ||
		second[0].StateSyncErr != "" {
		t.Fatalf("second report = %+v", second)
	}
	if got, want := events.snapshot(), []string{
		"audit:delete_physical_result:C:\\inert\\a.jpg",
		"audit:delete_physical_result:C:\\inert\\b.jpg",
		"state:C:\\inert\\a.jpg",
		"audit:delete_state_sync_error:",
		"gui:0",
		"audit:delete_physical_result:C:\\inert\\c.jpg",
		"state:C:\\inert\\c.jpg",
		"gui:1",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
	auditRecords := auditCapture.recordsSnapshot()
	if len(auditRecords) != 4 {
		t.Fatalf("audit records = %d, want 4", len(auditRecords))
	}
	stateRecord := auditRecords[2]
	if stateRecord.message != "delete_state_sync_error" {
		t.Fatalf("state audit message = %q", stateRecord.message)
	}
	assertRecordFields(t, stateRecord, map[string]any{
		"task_id":       "task-state-error",
		"machine_id":    "machine-a",
		"seq":           uint64(0),
		"err":           sqliteErr.Error(),
		"success_count": int64(1),
	})
	for _, record := range logCapture.recordsSnapshot() {
		if _, exists := record.attrs["path"]; exists {
			t.Fatalf("general logger duplicated a path-bearing record: %+v", record)
		}
		if record.message == "delete_physical_result" {
			t.Fatalf("general logger duplicated physical audit: %+v", record)
		}
	}
}

func TestForwarderSenderFailureStopsDestructiveProgressWithoutReplay(t *testing.T) {
	var requests atomic.Int32
	dialer := newScriptedDialer(func(conn net.Conn) {
		framed := proto.NewConn(conn)
		if writeValidHello(framed) != nil {
			return
		}
		chunk, err := readDeleteTask(framed)
		if err != nil {
			return
		}
		requests.Add(1)
		if framed.WriteFrame(
			proto.MsgDeleteReport,
			ptrDeleteReport(validSuccessReport(chunk)),
		) != nil {
			return
		}
		if conn.SetReadDeadline(time.Now().Add(100*time.Millisecond)) != nil {
			return
		}
		if _, _, err := framed.ReadFrame(); err == nil {
			requests.Add(1)
		}
	})
	senderErr := errors.New("GUI connection lost")
	state := &recordingState{}
	sender := &recordingSender{
		errFn: func(call int, _ proto.DeleteReport) error {
			if call == 0 {
				return senderErr
			}
			return nil
		},
	}
	auditCapture := &captureHandler{}
	cfg := config.DefaultAgent().Delete
	cfg.MaxEntriesPerFrame = 1
	forwarder := NewForwarder(
		"machine-a",
		cfg,
		dialer,
		state,
		slog.New(auditCapture),
		nil,
	)
	forwarder.helloTimeout = 250 * time.Millisecond
	forwarder.reportTimeout = 250 * time.Millisecond

	err := forwarder.Handle(context.Background(), proto.DeleteTask{
		TaskID: "task-gui-loss",
		Entries: []string{
			`C:\inert\a.jpg`,
			`C:\inert\b.jpg`,
		},
	}, sender.Send)
	if !errors.Is(err, senderErr) {
		t.Fatalf("Handle error = %v, want wrapped sender error", err)
	}
	dialer.wait(t)

	if got := requests.Load(); got != 1 {
		t.Fatalf("Helper requests = %d, want 1", got)
	}
	if got := dialer.callCount(); got != 1 {
		t.Fatalf("dial calls = %d, want 1", got)
	}
	if got, want := state.pathsByCall(), [][]string{{`C:\inert\a.jpg`}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("state calls = %#v, want %#v", got, want)
	}
	if got := sender.reportCount(); got != 1 {
		t.Fatalf("GUI send attempts = %d, want 1", got)
	}
	if got := len(auditCapture.recordsSnapshot()); got != 1 {
		t.Fatalf("audit records = %d, want 1", got)
	}
	if got := dialer.closeCalls(); !reflect.DeepEqual(got, []int32{1}) {
		t.Fatalf("close calls = %v, want [1]", got)
	}
}

func newTestForwarder(
	dialer HelperDialer,
	state StateStore,
	sender *recordingSender,
	audit *slog.Logger,
) *Forwarder {
	cfg := config.DefaultAgent().Delete
	forwarder := NewForwarder("machine-a", cfg, dialer, state, audit, nil)
	forwarder.helloTimeout = 250 * time.Millisecond
	forwarder.reportTimeout = 250 * time.Millisecond
	return forwarder
}

type scriptedDialer struct {
	script     func(net.Conn)
	clientWrap func(net.Conn) net.Conn

	mu          sync.Mutex
	calls       int
	connections []*closeCountingConn
	done        []<-chan struct{}
}

func newScriptedDialer(script func(net.Conn)) *scriptedDialer {
	return &scriptedDialer{script: script}
}

func newWrappedScriptedDialer(
	script func(net.Conn),
	clientWrap func(net.Conn) net.Conn,
) *scriptedDialer {
	return &scriptedDialer{script: script, clientWrap: clientWrap}
}

func (d *scriptedDialer) Dial(context.Context) (net.Conn, error) {
	client, server := net.Pipe()
	var clientConn net.Conn = client
	if d.clientWrap != nil {
		clientConn = d.clientWrap(clientConn)
	}
	counting := &closeCountingConn{Conn: clientConn}
	done := make(chan struct{})

	d.mu.Lock()
	d.calls++
	d.connections = append(d.connections, counting)
	d.done = append(d.done, done)
	d.mu.Unlock()

	go func() {
		defer close(done)
		defer server.Close()
		d.script(server)
	}()
	return counting, nil
}

func (d *scriptedDialer) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func (d *scriptedDialer) closeCalls() []int32 {
	d.mu.Lock()
	connections := append([]*closeCountingConn(nil), d.connections...)
	d.mu.Unlock()
	calls := make([]int32, len(connections))
	for index, connection := range connections {
		calls[index] = connection.closeCalls.Load()
	}
	return calls
}

func (d *scriptedDialer) wait(t *testing.T) {
	t.Helper()
	d.mu.Lock()
	doneChannels := append([]<-chan struct{}(nil), d.done...)
	d.mu.Unlock()
	for _, done := range doneChannels {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Helper script did not exit")
		}
	}
}

type errorDialer struct {
	err   error
	calls atomic.Int32
}

func (d *errorDialer) Dial(context.Context) (net.Conn, error) {
	d.calls.Add(1)
	return nil, d.err
}

func (d *errorDialer) callCount() int32 {
	return d.calls.Load()
}

type nilHelperDialer struct{}

func (*nilHelperDialer) Dial(context.Context) (net.Conn, error) {
	return nil, errors.New("typed nil Helper dialer was invoked")
}

type nilStateStore struct{}

func (*nilStateStore) MarkDeleted(
	context.Context,
	string,
	[]string,
) error {
	return errors.New("typed nil state store was invoked")
}

var typedNilConnMethodCalls atomic.Int32

type typedNilConn struct{}

func (*typedNilConn) recordMethodCall() {
	typedNilConnMethodCalls.Add(1)
}

func (connection *typedNilConn) Read([]byte) (int, error) {
	connection.recordMethodCall()
	return 0, io.ErrClosedPipe
}

func (connection *typedNilConn) Write([]byte) (int, error) {
	connection.recordMethodCall()
	return 0, io.ErrClosedPipe
}

func (connection *typedNilConn) Close() error {
	connection.recordMethodCall()
	return io.ErrClosedPipe
}

func (connection *typedNilConn) LocalAddr() net.Addr {
	connection.recordMethodCall()
	return nil
}

func (connection *typedNilConn) RemoteAddr() net.Addr {
	connection.recordMethodCall()
	return nil
}

func (connection *typedNilConn) SetDeadline(time.Time) error {
	connection.recordMethodCall()
	return io.ErrClosedPipe
}

func (connection *typedNilConn) SetReadDeadline(time.Time) error {
	connection.recordMethodCall()
	return io.ErrClosedPipe
}

func (connection *typedNilConn) SetWriteDeadline(time.Time) error {
	connection.recordMethodCall()
	return io.ErrClosedPipe
}

type typedNilConnDialer struct {
	err   error
	calls atomic.Int32
}

func (dialer *typedNilConnDialer) Dial(context.Context) (net.Conn, error) {
	dialer.calls.Add(1)
	var connection *typedNilConn
	return connection, dialer.err
}

type dialerFunc func(context.Context) (net.Conn, error)

func (function dialerFunc) Dial(ctx context.Context) (net.Conn, error) {
	return function(ctx)
}

type blockingDialer struct {
	entered chan context.Context
}

func (d *blockingDialer) Dial(ctx context.Context) (net.Conn, error) {
	d.entered <- ctx
	<-ctx.Done()
	return nil, ctx.Err()
}

type gatedDialer struct {
	inner   HelperDialer
	entered chan struct{}
	release chan struct{}
}

func (d *gatedDialer) Dial(ctx context.Context) (net.Conn, error) {
	close(d.entered)
	select {
	case <-d.release:
		return d.inner.Dial(ctx)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type writeFailConn struct {
	net.Conn
}

func (c *writeFailConn) Write([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}

type closeCountingConn struct {
	net.Conn
	closeCalls atomic.Int32
}

func (c *closeCountingConn) Close() error {
	c.closeCalls.Add(1)
	return c.Conn.Close()
}

type stateCall struct {
	machineID string
	paths     []string
}

type recordingState struct {
	mu     sync.Mutex
	calls  []stateCall
	events *eventLog
	errFn  func(call int, machineID string, paths []string) error
}

func (s *recordingState) MarkDeleted(
	_ context.Context,
	machineID string,
	paths []string,
) error {
	s.mu.Lock()
	call := len(s.calls)
	copied := append([]string(nil), paths...)
	s.calls = append(s.calls, stateCall{machineID: machineID, paths: copied})
	s.mu.Unlock()
	if s.events != nil {
		s.events.add("state:" + copied[0])
	}
	if s.errFn != nil {
		return s.errFn(call, machineID, copied)
	}
	return nil
}

func (s *recordingState) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *recordingState) pathsByCall() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]string, len(s.calls))
	for index := range s.calls {
		out[index] = append([]string(nil), s.calls[index].paths...)
	}
	return out
}

type recordingSender struct {
	mu      sync.Mutex
	reports []proto.DeleteReport
	events  *eventLog
	errFn   func(call int, report proto.DeleteReport) error
}

func (s *recordingSender) Send(messageType uint8, value any) error {
	if messageType != proto.MsgDeleteReport {
		return fmt.Errorf("unexpected GUI message type %d", messageType)
	}
	report, ok := value.(*proto.DeleteReport)
	if !ok || report == nil {
		return fmt.Errorf("unexpected GUI report value %T", value)
	}
	copied := cloneDeleteReport(*report)
	s.mu.Lock()
	call := len(s.reports)
	s.reports = append(s.reports, copied)
	s.mu.Unlock()
	if s.events != nil {
		s.events.add(fmt.Sprintf("gui:%d", report.Seq))
	}
	if s.errFn != nil {
		return s.errFn(call, copied)
	}
	return nil
}

func (s *recordingSender) reportCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reports)
}

func (s *recordingSender) reportsSnapshot() []proto.DeleteReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]proto.DeleteReport, len(s.reports))
	for index := range s.reports {
		out[index] = cloneDeleteReport(s.reports[index])
	}
	return out
}

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(event string) {
	l.mu.Lock()
	l.events = append(l.events, event)
	l.mu.Unlock()
}

func (l *eventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

type capturedRecord struct {
	message string
	attrs   map[string]any
}

type captureHandler struct {
	mu      sync.Mutex
	records []capturedRecord
	events  *eventLog
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

func (h *captureHandler) Handle(_ context.Context, record slog.Record) error {
	attrs := make(map[string]any)
	record.Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value.Any()
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, capturedRecord{
		message: record.Message,
		attrs:   attrs,
	})
	h.mu.Unlock()
	if h.events != nil {
		path, _ := attrs["path"].(string)
		h.events.add("audit:" + record.Message + ":" + path)
	}
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler {
	return h
}

func (h *captureHandler) WithGroup(string) slog.Handler {
	return h
}

func (h *captureHandler) recordsSnapshot() []capturedRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]capturedRecord, len(h.records))
	for index, record := range h.records {
		attrs := make(map[string]any, len(record.attrs))
		for key, value := range record.attrs {
			attrs[key] = value
		}
		out[index] = capturedRecord{message: record.message, attrs: attrs}
	}
	return out
}

func writeValidHello(framed *proto.Conn) error {
	return framed.WriteFrame(proto.MsgHello, &proto.Hello{
		Version: proto.ProtocolVersion,
		PID:     1234,
		Role:    "delete-helper",
	})
}

func readDeleteTask(framed *proto.Conn) (proto.DeleteTask, error) {
	messageType, body, err := framed.ReadFrame()
	if err != nil {
		return proto.DeleteTask{}, err
	}
	if messageType != proto.MsgDeleteTask {
		return proto.DeleteTask{}, fmt.Errorf(
			"message type = %d, want %d",
			messageType,
			proto.MsgDeleteTask,
		)
	}
	value, err := proto.Decode(messageType, body)
	if err != nil {
		return proto.DeleteTask{}, err
	}
	task, ok := value.(*proto.DeleteTask)
	if !ok {
		return proto.DeleteTask{}, fmt.Errorf("decoded task type = %T", value)
	}
	return cloneDeleteTask(*task), nil
}

func validSuccessReport(chunk proto.DeleteTask) proto.DeleteReport {
	entries := make([]proto.DeleteResult, len(chunk.Entries))
	for index, path := range chunk.Entries {
		entries[index] = proto.DeleteResult{Path: path, OK: true}
	}
	return proto.DeleteReport{
		TaskID:  chunk.TaskID,
		Seq:     chunk.Seq,
		LastSeq: chunk.LastSeq,
		Entries: entries,
	}
}

func ptrDeleteReport(report proto.DeleteReport) *proto.DeleteReport {
	return &report
}

func writeMalformedFrame(conn net.Conn) {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], 1)
	_, _ = conn.Write(header[:])
	_, _ = conn.Write([]byte{0xc1})
}

func recordUnexpectedRequest(
	conn net.Conn,
	framed *proto.Conn,
	requests *atomic.Int32,
) {
	if conn.SetReadDeadline(time.Now().Add(100*time.Millisecond)) != nil {
		return
	}
	if _, _, err := framed.ReadFrame(); err == nil {
		requests.Add(1)
	}
}

func cloneDeleteTask(task proto.DeleteTask) proto.DeleteTask {
	task.Entries = append([]string(nil), task.Entries...)
	return task
}

func cloneDeleteReport(report proto.DeleteReport) proto.DeleteReport {
	report.Entries = append([]proto.DeleteResult(nil), report.Entries...)
	return report
}

func chunkLengths(chunks []proto.DeleteTask) []int {
	lengths := make([]int, len(chunks))
	for index := range chunks {
		lengths[index] = len(chunks[index].Entries)
	}
	return lengths
}

func assertSyntheticReport(
	t *testing.T,
	report proto.DeleteReport,
	uncertain bool,
	paths []string,
) {
	t.Helper()
	if report.Stats != (proto.DeleteStats{
		Total:     len(paths),
		Failed:    len(paths),
		Uncertain: boolInt(uncertain) * len(paths),
	}) {
		t.Fatalf("stats = %+v", report.Stats)
	}
	if len(report.Entries) != len(paths) {
		t.Fatalf("entries = %d, want %d", len(report.Entries), len(paths))
	}
	for index, entry := range report.Entries {
		if entry.Path != paths[index] ||
			entry.OK ||
			entry.ErrCode != proto.DeleteErrHelperLost ||
			entry.Err == "" ||
			entry.Uncertain != uncertain {
			t.Fatalf("entry %d = %+v", index, entry)
		}
		message := strings.ToLower(entry.Err)
		if !strings.Contains(message, "helper.exe") ||
			!strings.Contains(message, "administrator") {
			t.Fatalf(
				"entry %d helper-loss guidance = %q; want helper.exe and administrator",
				index,
				entry.Err,
			)
		}
	}
}

func assertAuditFields(
	t *testing.T,
	record capturedRecord,
	expected map[string]any,
) {
	t.Helper()
	if record.message != "delete_physical_result" {
		t.Fatalf("audit message = %q", record.message)
	}
	assertRecordFields(t, record, expected)
}

func assertRecordFields(
	t *testing.T,
	record capturedRecord,
	expected map[string]any,
) {
	t.Helper()
	for key, want := range expected {
		if got := record.attrs[key]; !reflect.DeepEqual(got, want) {
			t.Fatalf("audit field %q = %#v (%T), want %#v (%T)",
				key, got, got, want, want)
		}
	}
}

func assertReportMetadata(
	t *testing.T,
	report proto.DeleteReport,
	taskID string,
	seq uint32,
	lastSeq uint32,
) {
	t.Helper()
	if report.TaskID != taskID ||
		report.Seq != seq ||
		report.LastSeq != lastSeq {
		t.Fatalf(
			"report metadata = task %q seq %d last %d, want task %q seq %d last %d",
			report.TaskID,
			report.Seq,
			report.LastSeq,
			taskID,
			seq,
			lastSeq,
		)
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
