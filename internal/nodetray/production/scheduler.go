package production

import (
	"context"
	"errors"
	"sync"
	"time"

	"dedup/internal/nodetray/bootstrap"
)

type RuntimeTicker interface {
	C() <-chan time.Time
	Stop()
}

type RuntimeTickerFactory func(time.Duration) RuntimeTicker

type Scheduler struct{ newTicker RuntimeTickerFactory }

func NewScheduler(factory RuntimeTickerFactory) *Scheduler {
	if factory == nil {
		factory = func(duration time.Duration) RuntimeTicker {
			return &systemRuntimeTicker{ticker: time.NewTicker(duration)}
		}
	}
	return &Scheduler{newTicker: factory}
}

func (s *Scheduler) Start(parent context.Context, visible, recovery time.Duration, refresh func(context.Context)) (bootstrap.Closer, error) {
	if s == nil || s.newTicker == nil || parent == nil || visible <= 0 || recovery <= 0 || refresh == nil {
		return nil, errors.New("production scheduler: dependencies unavailable")
	}
	visibleTicker := s.newTicker(visible)
	recoveryTicker := s.newTicker(recovery)
	if visibleTicker == nil || recoveryTicker == nil {
		if visibleTicker != nil {
			visibleTicker.Stop()
		}
		if recoveryTicker != nil {
			recoveryTicker.Stop()
		}
		return nil, errors.New("production scheduler: ticker unavailable")
	}
	ctx, cancel := context.WithCancel(parent)
	closer := &schedulerCloser{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(closer.done)
		defer visibleTicker.Stop()
		defer recoveryTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-visibleTicker.C():
				refresh(ctx)
			case <-recoveryTicker.C():
				refresh(ctx)
			}
		}
	}()
	return closer, nil
}

type systemRuntimeTicker struct{ ticker *time.Ticker }

func (t *systemRuntimeTicker) C() <-chan time.Time { return t.ticker.C }
func (t *systemRuntimeTicker) Stop()               { t.ticker.Stop() }

type schedulerCloser struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func (c *schedulerCloser) Close() error {
	if c == nil {
		return nil
	}
	c.once.Do(func() {
		c.cancel()
		<-c.done
	})
	return nil
}
