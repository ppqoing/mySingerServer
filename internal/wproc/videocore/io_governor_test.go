//go:build cgo && windows

package videocore

import (
	"context"
	"errors"
	"os"
	"runtime/cgo"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type testIOGovernor struct {
	beforeRead func(context.Context, int) (uint64, int, error)
	afterRead  func(uint64, int, time.Duration, error)
	beforeSeek func(context.Context) (uint64, error)
	afterSeek  func(uint64, time.Duration, error)
}

// Break caught: the Go callback table is populated but never actually reaches
// native WinFile hash reads/seeks, or callback failures expose IPC details.
func TestIOGovernorCgoCallbacksGovernNativeHash(t *testing.T) {
	path := t.TempDir() + `\source.bin`
	if err := os.WriteFile(path, []byte("governed source bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	var reads, seeks, reports atomic.Int32
	governor := &testIOGovernor{
		beforeRead: func(_ context.Context, want int) (uint64, int, error) {
			reads.Add(1)
			return uint64(100 + reads.Load()), want, nil
		},
		beforeSeek: func(context.Context) (uint64, error) {
			seeks.Add(1)
			return 200, nil
		},
		afterRead: func(uint64, int, time.Duration, error) { reports.Add(1) },
		afterSeek: func(uint64, time.Duration, error) { reports.Add(1) },
	}
	session, err := Open(context.Background(), path, OpenOptions{IOGovernor: governor})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Hash(); err != nil {
		_ = session.Close()
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if reads.Load() == 0 || seeks.Load() == 0 || reports.Load() != reads.Load()+seeks.Load() {
		t.Fatalf("native callback counts reads=%d seeks=%d reports=%d", reads.Load(), seeks.Load(), reports.Load())
	}

	raw := `write \\.\pipe\private-worker: access denied`
	rejecting := &testIOGovernor{
		beforeRead: func(context.Context, int) (uint64, int, error) { return 0, 0, errors.New(raw) },
		beforeSeek: func(context.Context) (uint64, error) { return 0, errors.New(raw) },
	}
	session, err = Open(context.Background(), path, OpenOptions{IOGovernor: rejecting})
	if err != nil {
		t.Fatal(err)
	}
	_, err = session.Hash()
	_ = session.Close()
	if err == nil || strings.Contains(strings.ToLower(err.Error()), "pipe") || strings.Contains(err.Error(), "private-worker") {
		t.Fatalf("native callback error was not sanitized: %v", err)
	}
}

func (governor *testIOGovernor) BeforeRead(ctx context.Context, want int) (uint64, int, error) {
	return governor.beforeRead(ctx, want)
}
func (governor *testIOGovernor) AfterRead(id uint64, bytes int, elapsed time.Duration, err error) {
	if governor.afterRead != nil {
		governor.afterRead(id, bytes, elapsed, err)
	}
}
func (governor *testIOGovernor) BeforeSeek(ctx context.Context) (uint64, error) {
	return governor.beforeSeek(ctx)
}
func (governor *testIOGovernor) AfterSeek(id uint64, elapsed time.Duration, err error) {
	if governor.afterSeek != nil {
		governor.afterSeek(id, elapsed, err)
	}
}

func acceptingIOGovernor() *testIOGovernor {
	return &testIOGovernor{
		beforeRead: func(_ context.Context, want int) (uint64, int, error) {
			return 91, want, nil
		},
		beforeSeek: func(context.Context) (uint64, error) { return 92, nil },
	}
}

func handleIsLive(value uintptr) (live bool) {
	defer func() { _ = recover() }()
	_ = cgo.Handle(value).Value()
	return true
}

// Break caught: the cgo.Handle backing a native governor is deleted too early,
// leaked on a failed/cancelled/panicking open, or deleted twice on Close.
func TestIOGovernorHandleLifecycle(t *testing.T) {
	t.Run("success and repeated close", func(t *testing.T) {
		bridge := &fakeNativeBridge{}
		session, err := openWith(context.Background(), `D:\media\ok.mp4`, OpenOptions{
			IOGovernor: acceptingIOGovernor(),
		}, bridge)
		if err != nil {
			t.Fatal(err)
		}
		value := bridge.lastOpenOptions.ioGovernorContext
		if value == 0 || !handleIsLive(value) {
			t.Fatalf("governor handle %d was not live during session", value)
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
		if handleIsLive(value) {
			t.Fatal("governor handle survived Close")
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("native failure", func(t *testing.T) {
		bridge := &fakeNativeBridge{openErr: errors.New("open failed")}
		_, err := openWith(context.Background(), `D:\media\fail.mp4`, OpenOptions{
			IOGovernor: acceptingIOGovernor(),
		}, bridge)
		if err == nil {
			t.Fatal("open unexpectedly succeeded")
		}
		if handleIsLive(bridge.lastOpenOptions.ioGovernorContext) {
			t.Fatal("governor handle leaked after open failure")
		}
	})

	t.Run("cancel after native success", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		bridge := &fakeNativeBridge{openEntered: make(chan struct{}), openRelease: make(chan struct{})}
		done := make(chan error, 1)
		go func() {
			_, err := openWith(ctx, `D:\media\cancel.mp4`, OpenOptions{
				IOGovernor: acceptingIOGovernor(),
			}, bridge)
			done <- err
		}()
		<-bridge.openEntered
		cancel()
		close(bridge.openRelease)
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("open error = %v, want context.Canceled", err)
		}
		if handleIsLive(bridge.lastOpenOptions.ioGovernorContext) {
			t.Fatal("governor handle leaked after cancellation")
		}
	})

	t.Run("native panic", func(t *testing.T) {
		bridge := &fakeNativeBridge{openPanic: "boom"}
		func() {
			defer func() { _ = recover() }()
			_, _ = openWith(context.Background(), `D:\media\panic.mp4`, OpenOptions{
				IOGovernor: acceptingIOGovernor(),
			}, bridge)
		}()
		if handleIsLive(bridge.lastOpenOptions.ioGovernorContext) {
			t.Fatal("governor handle leaked while unwinding panic")
		}
	})
}

// Break caught: a callback panic escapes across the C ABI, or raw IPC/pipe
// details are copied into native/user-visible errors.
func TestIOGovernorCallbacksRecoverAndSanitizeErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		governor *testIOGovernor
		want     int32
	}{
		{
			name: "infrastructure error",
			governor: &testIOGovernor{beforeRead: func(context.Context, int) (uint64, int, error) {
				return 0, 0, errors.New(`write \\.\pipe\private-worker: access denied`)
			}},
			want: StatusIO,
		},
		{
			name: "panic",
			governor: &testIOGovernor{beforeRead: func(context.Context, int) (uint64, int, error) {
				panic(`\\.\pipe\panic-secret`)
			}},
			want: StatusIO,
		},
		{
			name: "cancellation",
			governor: &testIOGovernor{beforeRead: func(context.Context, int) (uint64, int, error) {
				return 0, 0, context.Canceled
			}},
			want: StatusCancelled,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := newIOGovernorHandle(context.Background(), tc.governor)
			value := owner.Value()
			defer owner.Delete()
			_, _, status, message := invokeIOAcquire(value, ioOperationRead, 4096)
			if status != tc.want {
				t.Fatalf("status = %d, want %d", status, tc.want)
			}
			if strings.Contains(strings.ToLower(message), "pipe") || strings.Contains(message, "private-worker") || strings.Contains(message, "panic-secret") {
				t.Fatalf("callback error leaked infrastructure details: %q", message)
			}
		})
	}
}
