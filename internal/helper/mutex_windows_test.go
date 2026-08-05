package helper

import (
	"fmt"
	"testing"
	"time"
)

func TestInstanceLockUsesExactHelperMutexNameConstant(t *testing.T) {
	const want = `Local\DedupDeleteHelperMutex`
	if HelperMutexName != want {
		t.Fatalf("HelperMutexName = %q, want %q", HelperMutexName, want)
	}
}

func TestInstanceLockRejectsSecondAcquireAndCanBeReacquiredAfterClose(t *testing.T) {
	name := fmt.Sprintf(
		`Local\DedupDeleteHelperMutex-test-%d-%d`,
		time.Now().UnixNano(),
		time.Now().Nanosecond(),
	)
	first, err := AcquireInstanceLock(name)
	if err != nil {
		t.Fatalf("first AcquireInstanceLock: %v", err)
	}
	t.Cleanup(func() {
		if first != nil {
			_ = first.Close()
		}
	})

	started := time.Now()
	if second, err := AcquireInstanceLock(name); err == nil {
		_ = second.Close()
		t.Fatal("second AcquireInstanceLock succeeded while first lock is live")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("second AcquireInstanceLock blocked for %v, want immediate failure", elapsed)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first lock: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("second Close must be harmless: %v", err)
	}
	first = nil

	third, err := AcquireInstanceLock(name)
	if err != nil {
		t.Fatalf("reacquire after Close: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatalf("close reacquired lock: %v", err)
	}
}
