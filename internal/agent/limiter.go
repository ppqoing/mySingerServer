package agent

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
)

type ScanObserver interface {
	Begin(diskNo int64, bytes int64)
	End(
		diskNo int64,
		bytes int64,
		elapsed time.Duration,
		read time.Duration,
		decode time.Duration,
	)
}

type byteLimiter struct {
	capacity int64
	weighted *semaphore.Weighted
}

func newByteLimiter(capacity int64) *byteLimiter {
	if capacity < 1 {
		capacity = 1
	}
	return &byteLimiter{
		capacity: capacity,
		weighted: semaphore.NewWeighted(capacity),
	}
}

func (l *byteLimiter) acquire(ctx context.Context, bytes int64) (func(), error) {
	weight := bytes
	if weight < 1 {
		weight = 1
	}
	if weight > l.capacity {
		weight = l.capacity
	}
	if err := l.weighted.Acquire(ctx, weight); err != nil {
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() { l.weighted.Release(weight) })
	}, nil
}

func runObservedWork(
	ctx context.Context,
	limiter *byteLimiter,
	observer ScanObserver,
	diskNo int64,
	bytes int64,
	work func() (time.Duration, time.Duration),
) (err error) {
	release, err := limiter.acquire(ctx, bytes)
	if err != nil {
		return err
	}
	started := time.Now()
	if observer != nil {
		observer.Begin(diskNo, bytes)
	}
	var read, decode time.Duration
	defer func() {
		if observer != nil {
			observer.End(diskNo, bytes, time.Since(started), read, decode)
		}
		release()
	}()
	read, decode = work()
	return nil
}
