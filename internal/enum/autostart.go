package enum

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrIPC           = errors.New("everything: IPC unavailable (Everything not running?)")
	ErrIndexNotReady = errors.New("everything: database is not loaded")
)

type AutoStartOptions struct {
	Context        context.Context
	Primary        Enumerator
	Fallback       Enumerator
	StartClient    func() error
	Poll           func(context.Context) error
	OnWaiting      func(error)
	OnFallback     func(error)
	OnReady        func()
	OnRootFallback func(string, error)
}

type AutoStartEnumerator struct {
	options AutoStartOptions
	ready   chan struct{}
	once    sync.Once

	mu       sync.Mutex
	selected Enumerator
	readyErr error
}

func NewAutoStartEnumerator(options AutoStartOptions) *AutoStartEnumerator {
	if options.Context == nil {
		options.Context = context.Background()
	}
	if options.Poll == nil {
		options.Poll = pollEverythingReadiness
	}
	return &AutoStartEnumerator{
		options: options,
		ready:   make(chan struct{}),
	}
}

func (e *AutoStartEnumerator) Name() string {
	if e.options.Primary == nil {
		return "everything-auto"
	}
	if e.options.Fallback == nil {
		return e.options.Primary.Name() + "-auto"
	}
	return e.options.Primary.Name() + "-auto+" + e.options.Fallback.Name()
}

func (e *AutoStartEnumerator) Start() {
	e.once.Do(func() {
		go e.prepare()
	})
}

func (e *AutoStartEnumerator) Available() error {
	e.Start()
	return e.waitUntilReady()
}

func (e *AutoStartEnumerator) Enum(
	root string,
	visit func(FileRecord) error,
) error {
	e.Start()
	if err := e.waitUntilReady(); err != nil {
		return err
	}
	e.mu.Lock()
	selected := e.selected
	e.mu.Unlock()
	if selected == nil {
		return errors.New("everything auto-start: no enumerator selected")
	}
	return selected.Enum(root, visit)
}

func (e *AutoStartEnumerator) waitUntilReady() error {
	select {
	case <-e.ready:
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.readyErr
	case <-e.options.Context.Done():
		return e.options.Context.Err()
	}
}

func (e *AutoStartEnumerator) prepare() {
	if e.options.Primary == nil {
		e.useFallback(errors.New("everything auto-start: primary enumerator is nil"))
		return
	}

	startAttempted := false
	for {
		err := e.options.Primary.Available()
		if err == nil {
			selected := Enumerator(e.options.Primary)
			if e.options.Fallback != nil {
				selected = NewResilientEnumerator(
					e.options.Primary,
					e.options.Fallback,
					e.options.OnRootFallback,
				)
			}
			e.finish(selected, nil)
			if e.options.OnReady != nil {
				e.options.OnReady()
			}
			return
		}

		if errors.Is(err, ErrIPC) && !startAttempted {
			startAttempted = true
			if e.options.StartClient == nil {
				e.useFallback(errors.New("everything auto-start: client starter is nil"))
				return
			}
			if startErr := e.options.StartClient(); startErr != nil {
				e.useFallback(fmt.Errorf("everything auto-start: %w", startErr))
				return
			}
		} else if !errors.Is(err, ErrIPC) && !errors.Is(err, ErrIndexNotReady) {
			e.useFallback(err)
			return
		}

		if e.options.OnWaiting != nil {
			e.options.OnWaiting(err)
		}
		if pollErr := e.options.Poll(e.options.Context); pollErr != nil {
			e.finish(nil, pollErr)
			return
		}
	}
}

func (e *AutoStartEnumerator) useFallback(cause error) {
	if e.options.OnFallback != nil {
		e.options.OnFallback(cause)
	}
	if e.options.Fallback == nil {
		e.finish(nil, cause)
		return
	}
	e.finish(e.options.Fallback, nil)
}

func (e *AutoStartEnumerator) finish(selected Enumerator, err error) {
	e.mu.Lock()
	e.selected = selected
	e.readyErr = err
	e.mu.Unlock()
	close(e.ready)
}

func pollEverythingReadiness(ctx context.Context) error {
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
