package process

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestSameProcessRejectsEveryIdentityDrift(t *testing.T) {
	base := Identity{PID: 42, StartedAtUnixMS: 123456, ExecutablePath: `C:\Program Files\Node\agent.exe`}
	cases := []struct {
		name   string
		actual Identity
	}{
		{"pid reuse", Identity{PID: 42, StartedAtUnixMS: 123457, ExecutablePath: base.ExecutablePath}},
		{"different pid", Identity{PID: 43, StartedAtUnixMS: base.StartedAtUnixMS, ExecutablePath: base.ExecutablePath}},
		{"path drift", Identity{PID: base.PID, StartedAtUnixMS: base.StartedAtUnixMS, ExecutablePath: `C:\Other\agent.exe`}},
		{"short path alias is not trusted", Identity{PID: base.PID, StartedAtUnixMS: base.StartedAtUnixMS, ExecutablePath: `C:\PROGRA~1\Node\agent.exe`}},
		{"missing final path", Identity{PID: base.PID, StartedAtUnixMS: base.StartedAtUnixMS}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if SameProcess(base, tc.actual) {
				t.Fatal("identity drift was accepted as the same process")
			}
		})
	}
}

func TestSamePIDAndExecutableIgnoresReportedTimeButRejectsPIDOrPath(t *testing.T) {
	base := Identity{PID: 42, StartedAtUnixMS: 123456, ExecutablePath: `C:\Program Files\Node\agent.exe`}
	reported := base
	reported.StartedAtUnixMS += 250
	if !SamePIDAndExecutable(base, reported) {
		t.Fatal("PID and executable match was rejected because reported time drifted")
	}
	for _, drifted := range []Identity{
		{PID: 43, ExecutablePath: base.ExecutablePath},
		{PID: 42, ExecutablePath: `C:\Other\agent.exe`},
	} {
		if SamePIDAndExecutable(base, drifted) {
			t.Fatalf("drifted PID or path was accepted: %+v", drifted)
		}
	}
}

func TestSameProcessUsesWindowsOrdinalIgnoreCaseForFinalPaths(t *testing.T) {
	base := Identity{PID: 42, StartedAtUnixMS: 123456, ExecutablePath: `C:\Program Files\Node\agent.exe`}
	actual := base
	actual.ExecutablePath = strings.ToUpper(base.ExecutablePath)
	want := runtime.GOOS == "windows"
	if got := SameProcess(base, actual); got != want {
		t.Fatalf("SameProcess case comparison = %v, want %v on %s", got, want, runtime.GOOS)
	}
}

func TestWindowsInspectorReadsCurrentProcessAndCancellationDoesNotPoll(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows process identity contract")
	}
	inspector := NewInspector()
	first, err := inspector.Inspect(os.Getpid())
	if err != nil {
		t.Fatalf("Inspect current process: %v", err)
	}
	second, err := inspector.Inspect(os.Getpid())
	if err != nil {
		t.Fatalf("Inspect current process again: %v", err)
	}
	if first.PID != os.Getpid() || first.StartedAtUnixMS <= 0 || first.ExecutablePath == "" {
		t.Fatalf("incomplete identity: %+v", first)
	}
	if !SameProcess(first, second) {
		t.Fatalf("current process identity was not stable: first=%+v second=%+v", first, second)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = inspector.Wait(ctx, first)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait cancelled error = %v, want context.Canceled", err)
	}
}
