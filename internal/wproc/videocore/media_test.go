package videocore

import (
	"context"
	"errors"
	"testing"
	"unsafe"
)

func TestSessionCloseIdempotent(t *testing.T) {
	bridge := &fakeNativeBridge{}
	session := newSession(
		nativeSession{value: unsafe.Pointer(new(byte))},
		nativeCancel{value: unsafe.Pointer(new(byte))},
		bridge,
	)

	if err := session.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	closeCalls, _, cancelFrees := bridge.counts()
	if closeCalls != 1 || cancelFrees != 1 {
		t.Fatalf("native lifecycle close=%d cancel_free=%d, want 1/1", closeCalls, cancelFrees)
	}
}

func TestAnalyzeCancellationWins(t *testing.T) {
	bridge := &fakeNativeBridge{
		analyzeStarted: make(chan struct{}),
		analyzeRelease: make(chan struct{}),
		analyzeResult:  AnalysisResult{DurationMS: 9_999, DurationStatus: StatusOK},
	}
	session := newSession(
		nativeSession{value: unsafe.Pointer(new(byte))},
		nativeCancel{value: unsafe.Pointer(new(byte))},
		bridge,
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type outcome struct {
		result AnalysisResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := session.Analyze(ctx, AnalysisRequest{})
		done <- outcome{result: result, err: err}
	}()

	<-bridge.analyzeStarted
	cancel()
	close(bridge.analyzeRelease)
	got := <-done
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("Analyze cancellation race error = %v, want context.Canceled", got.err)
	}
	if got.result != (AnalysisResult{}) {
		t.Fatalf("Analyze published result after cancellation: %#v", got.result)
	}
	_, cancelRequests, cancelFrees := bridge.counts()
	if cancelRequests != 1 || cancelFrees != 0 {
		t.Fatalf("active session cancel lifecycle requests=%d frees=%d, want 1/0", cancelRequests, cancelFrees)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close after cancellation error = %v", err)
	}
	_, _, cancelFrees = bridge.counts()
	if cancelFrees != 1 {
		t.Fatalf("Close cancel_free calls = %d, want 1", cancelFrees)
	}
}

func TestAnalyzeRejectsEmbeddedNULTempPath(t *testing.T) {
	bridge := &fakeNativeBridge{}
	session := newSession(
		nativeSession{value: unsafe.Pointer(new(byte))},
		nativeCancel{value: unsafe.Pointer(new(byte))},
		bridge,
	)
	defer session.Close()

	result, err := session.Analyze(context.Background(), AnalysisRequest{
		TempJPEGPath: `D:\thumbs\bad` + "\x00" + `.jpg`,
	})
	if result != (AnalysisResult{}) {
		t.Fatalf("Analyze invalid temp path result = %#v, want zero", result)
	}
	if !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("Analyze invalid temp path error = %v, want ErrInvalidPath", err)
	}
}
