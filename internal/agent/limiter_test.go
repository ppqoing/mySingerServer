package agent

import (
	"context"
	"testing"
	"time"
)

type recordingScanObserver struct {
	beginDisk  int64
	beginBytes int64
	endDisk    int64
	endBytes   int64
	elapsed    time.Duration
	read       time.Duration
	decode     time.Duration
}

func (o *recordingScanObserver) Begin(diskNo int64, bytes int64) {
	o.beginDisk, o.beginBytes = diskNo, bytes
}

func (o *recordingScanObserver) End(
	diskNo int64,
	bytes int64,
	elapsed time.Duration,
	read time.Duration,
	decode time.Duration,
) {
	o.endDisk, o.endBytes = diskNo, bytes
	o.elapsed, o.read, o.decode = elapsed, read, decode
}

func TestByteLimiterBlocksUntilWeightIsReleased(t *testing.T) {
	limiter := newByteLimiter(10)
	releaseFirst, err := limiter.acquire(context.Background(), 8)
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan func(), 1)
	go func() {
		release, acquireErr := limiter.acquire(context.Background(), 8)
		if acquireErr == nil {
			acquired <- release
		}
	}()
	select {
	case <-acquired:
		t.Fatal("second 8-byte job bypassed 10-byte limit")
	case <-time.After(25 * time.Millisecond):
	}
	releaseFirst()
	select {
	case releaseSecond := <-acquired:
		releaseSecond()
	case <-time.After(time.Second):
		t.Fatal("second job did not proceed after release")
	}
}

func TestByteLimiterLetsOversizedJobAcquireFullCapacity(t *testing.T) {
	limiter := newByteLimiter(10)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := limiter.acquire(ctx, 100)
	if err != nil {
		t.Fatalf("oversized acquire: %v", err)
	}
	release()
}

func TestRunObservedWorkBalancesLimiterAndMetrics(t *testing.T) {
	limiter := newByteLimiter(10)
	observer := &recordingScanObserver{}
	err := runObservedWork(
		context.Background(),
		limiter,
		observer,
		7,
		8,
		func() (time.Duration, time.Duration) {
			return 2 * time.Millisecond, 3 * time.Millisecond
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if observer.beginDisk != 7 || observer.beginBytes != 8 ||
		observer.endDisk != 7 || observer.endBytes != 8 ||
		observer.read != 2*time.Millisecond ||
		observer.decode != 3*time.Millisecond {
		t.Fatalf("observer = %#v", observer)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := limiter.acquire(ctx, 10)
	if err != nil {
		t.Fatalf("limiter weight leaked: %v", err)
	}
	release()
}
