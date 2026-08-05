package tray

import (
	"errors"
	"sync"
	"time"
)

var ErrUnavailable = errors.New("tray_unavailable")

type Options struct {
	Snapshot    func() Snapshot
	Handle      func(Command)
	ShowConsole func()
	OnError     func(code string)
}

type Controller interface {
	Close() error
	Notify(Event) (bool, error)
}

type nativeEvent uint8

const (
	eventDoubleClick nativeEvent = iota + 1
	eventMenuRequested
	eventTaskbarCreated
)

type nativeSession interface {
	NotificationSink
	Initialize(events func(nativeEvent)) error
	Run() error
	Readd() error
	ShowMenu(items []Item) (Command, bool, error)
	RequestClose() error
	Remove() error
}

type controller struct {
	session   nativeSession
	notifier  *Notifier
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

func startSession(session nativeSession, options Options) (Controller, error) {
	if session == nil || options.Snapshot == nil || options.Handle == nil || options.ShowConsole == nil {
		return nil, ErrUnavailable
	}
	value := &controller{session: session, done: make(chan struct{})}
	value.notifier = NewNotifier(time.Now, session)
	initialized := make(chan error, 1)

	go func() {
		initReported := false
		defer func() {
			if recovered := recover(); recovered != nil {
				if !initReported {
					initialized <- ErrUnavailable
					initReported = true
				} else {
					reportError(options, "tray_lifecycle_failed")
				}
			}
			if initReported {
				_ = session.Remove()
			}
			close(value.done)
		}()

		events := func(event nativeEvent) {
			dispatchNativeEvent(session, options, event)
		}
		if err := session.Initialize(events); err != nil {
			initialized <- ErrUnavailable
			initReported = true
			return
		}
		initialized <- nil
		initReported = true
		if err := session.Run(); err != nil {
			reportError(options, "tray_lifecycle_failed")
		}
	}()

	if err := <-initialized; err != nil {
		<-value.done
		return nil, err
	}
	return value, nil
}

func dispatchNativeEvent(session nativeSession, options Options, event nativeEvent) {
	defer func() {
		if recover() != nil {
			reportError(options, "tray_lifecycle_failed")
		}
	}()
	switch event {
	case eventDoubleClick:
		options.ShowConsole()
	case eventMenuRequested:
		items := BuildMenu(options.Snapshot())
		command, selected, err := session.ShowMenu(items)
		if err != nil {
			reportError(options, "tray_menu_failed")
			return
		}
		if selected && commandEnabled(items, command) {
			if isLifecycleCommand(command) {
				dispatchLifecycleCommand(options, command)
			} else {
				options.Handle(command)
			}
		}
	case eventTaskbarCreated:
		if err := session.Readd(); err != nil {
			reportError(options, "tray_readd_failed")
		}
	}
}

func dispatchLifecycleCommand(options Options, command Command) {
	go func() {
		defer func() {
			if recover() != nil {
				reportError(options, "tray_command_failed")
			}
		}()
		options.Handle(command)
	}()
}

func isLifecycleCommand(command Command) bool {
	switch command {
	case StartAgent, RestartAgent, StopAgent, StartHelper, StopHelper:
		return true
	default:
		return false
	}
}

func commandEnabled(items []Item, command Command) bool {
	for _, item := range items {
		if item.Command == command {
			return item.Enabled
		}
	}
	return false
}

func reportError(options Options, code string) {
	if options.OnError != nil {
		options.OnError(code)
	}
}

func (c *controller) Notify(event Event) (bool, error) {
	if c == nil || c.notifier == nil {
		return false, ErrUnavailable
	}
	return c.notifier.Notify(event)
}

func (c *controller) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		if err := c.session.RequestClose(); err != nil {
			c.closeErr = errors.New("tray_close_failed")
			return
		}
		<-c.done
	})
	return c.closeErr
}
