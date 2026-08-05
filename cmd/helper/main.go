package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"dedup/internal/helper"
	"dedup/internal/helpercontrol"
	"dedup/internal/machineid"
	"dedup/internal/nodectl"
)

type helperServer interface {
	Serve(context.Context) error
	ActiveRequests() int
	Listening() bool
}

type helperControlService interface {
	Run(context.Context) error
}

type machineIdentityProvider func() (machineid.Result, error)

type dependencies struct {
	identity     machineIdentityProvider
	now          func() time.Time
	loadConfig   func(string, string) (helper.Config, error)
	newLogger    func(string) (*slog.Logger, func() error, error)
	acquireLock  func(string) (helper.InstanceLock, error)
	listenPipe   func(helper.Config) (net.Listener, error)
	newValidator func(helper.Config) (*helper.Validator, error)
	newProcessor func(helper.Config, *helper.Validator) *helper.Processor
	newServer    func(helper.Config, net.Listener, *helper.Processor, *slog.Logger) helperServer
	newControl   func(nodectl.StatusProvider, nodectl.ShutdownFunc) helperControlService
}

var productionDependencies = dependencies{
	identity:     machineid.Current,
	now:          time.Now,
	loadConfig:   helper.LoadConfig,
	newLogger:    helper.NewLogger,
	acquireLock:  helper.AcquireInstanceLock,
	listenPipe:   helper.ListenPipe,
	newValidator: helper.NewValidator,
	newProcessor: helper.NewProcessor,
	newServer: func(cfg helper.Config, listener net.Listener, processor *helper.Processor, logger *slog.Logger) helperServer {
		return helper.NewServer(cfg, listener, processor, logger)
	},
	newControl: func(provider nodectl.StatusProvider, shutdown nodectl.ShutdownFunc) helperControlService {
		return helpercontrol.New(provider, shutdown)
	},
}

func main() {
	executable, err := os.Executable()
	if err == nil {
		var configPath string
		configPath, err = configPathFromArgs(os.Args[1:], executable)
		if err == nil {
			ctx, stop := helperSignalContext()
			defer stop()
			err = runWith(ctx, configPath, executable, productionDependencies)
		}
	}
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func helperSignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func configPathFromArgs(args []string, executable string) (string, error) {
	flags := flag.NewFlagSet("helper", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "helper configuration path")
	if err := flags.Parse(args); err != nil {
		return "", err
	}
	if flags.NArg() != 0 {
		return "", fmt.Errorf("unexpected argument: %s", flags.Arg(0))
	}
	explicit := false
	flags.Visit(func(item *flag.Flag) {
		if item.Name == "config" {
			explicit = true
		}
	})
	if explicit {
		return *configPath, nil
	}
	return filepath.Join(filepath.Dir(executable), "helper.json"), nil
}

func runWith(ctx context.Context, configPath, executable string, deps dependencies) (err error) {
	cfg, err := deps.loadConfig(configPath, executable)
	if err != nil {
		return fmt.Errorf("load helper config: %w", err)
	}
	if deps.identity == nil {
		return errors.New("resolve Helper machine identity: provider is nil")
	}
	identity, err := deps.identity()
	if err != nil {
		return fmt.Errorf("resolve Helper machine identity: %w", err)
	}
	machineID := identity.ID
	if !machineid.Valid(machineID) {
		return errors.New("control identity invalid: generated machine ID has invalid format")
	}
	if err := nodectl.ValidateControlIdentity(machineID, executable); err != nil {
		return fmt.Errorf("control identity invalid: %w", err)
	}
	configSHA256, err := effectiveHelperConfigSHA256(cfg)
	if err != nil {
		return fmt.Errorf("fingerprint effective Helper config: %w", err)
	}
	startedAt := deps.now()
	logger, closeLogger, err := deps.newLogger(cfg.LogDir)
	if err != nil {
		return fmt.Errorf("open helper log: %w", err)
	}
	defer func() { err = joinCleanupError(err, closeLogger()) }()
	for _, warning := range identity.Warnings {
		logger.Warn("machine identity source unavailable", "warning", warning)
	}

	lock, err := deps.acquireLock(helper.HelperMutexName)
	if err != nil {
		return fmt.Errorf("acquire helper mutex: %w", err)
	}
	defer func() { err = joinCleanupError(err, lock.Close()) }()

	rawListener, err := deps.listenPipe(cfg)
	if err != nil {
		return fmt.Errorf("listen helper pipe: %w", err)
	}
	listener := &closeOnceListener{listener: rawListener}
	defer func() { err = joinCleanupError(err, listener.Close()) }()

	validator, err := deps.newValidator(cfg)
	if err != nil {
		return fmt.Errorf("create helper validator: %w", err)
	}
	processor := deps.newProcessor(cfg, validator)
	server := deps.newServer(cfg, listener, processor, logger)
	root, cancel := context.WithCancel(ctx)
	defer cancel()
	provider := helpercontrol.NewProvider(helpercontrol.Inputs{
		MachineID:      machineID,
		ExecutablePath: executable,
		ConfigSHA256:   configSHA256,
		StartedAt:      startedAt,
		DeleteService:  server,
	})
	control := deps.newControl(provider, nodectl.ShutdownFunc(cancel))
	return runControlledHelper(root, cancel, server, control)
}

func runControlledHelper(
	ctx context.Context,
	cancel context.CancelFunc,
	deletes helperServer,
	control helperControlService,
) error {
	deleteResult := make(chan error, 1)
	controlResult := make(chan error, 1)
	go func() { deleteResult <- deletes.Serve(ctx) }()
	go func() { controlResult <- control.Run(ctx) }()

	var primary error
	deleteDone := false
	controlDone := false
	select {
	case <-ctx.Done():
	case err := <-deleteResult:
		deleteDone = true
		// A nil delete-service return preserves the legacy MsgShutdown command
		// on the delete protocol. Tray lifecycle commands use the distinct
		// nodectl Helper pipe and must never be merged into that protocol.
		if err != nil && ctx.Err() == nil {
			primary = fmt.Errorf("serve helper: %w", err)
		}
	case err := <-controlResult:
		controlDone = true
		if ctx.Err() == nil {
			if err == nil {
				primary = errors.New("Helper control service exited unexpectedly")
			} else {
				primary = fmt.Errorf("serve Helper control: %w", err)
			}
		}
	}
	cancel()
	if !deleteDone {
		<-deleteResult
	}
	if !controlDone {
		<-controlResult
	}
	return primary
}

func effectiveHelperConfigSHA256(cfg helper.Config) (string, error) {
	canonical, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	canonical = append(canonical, '\n')
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

type closeOnceListener struct {
	listener net.Listener

	mu       sync.Mutex
	closed   bool
	closeErr error
}

func (l *closeOnceListener) Accept() (net.Conn, error) { return l.listener.Accept() }

func (l *closeOnceListener) Addr() net.Addr { return l.listener.Addr() }

func (l *closeOnceListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return l.closeErr
	}
	l.closed = true
	l.closeErr = l.listener.Close()
	return l.closeErr
}

func joinCleanupError(primary, cleanup error) error {
	if cleanup == nil {
		return primary
	}
	if primary == nil {
		return cleanup
	}
	return errors.Join(primary, cleanup)
}
