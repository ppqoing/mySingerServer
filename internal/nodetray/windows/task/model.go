package task

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

const TaskPath = `\MySingerServer\DeleteHelper`

const (
	logonTypeInteractiveToken  = "InteractiveToken"
	runLevelHighest            = "HighestAvailable"
	triggerKindLogon           = "LogonTrigger"
	actionKindExec             = "Exec"
	multipleInstancesIgnoreNew = "IgnoreNew"
)

var (
	ErrAccessDenied     = errors.New("task: access denied")
	ErrTaskNotInstalled = errors.New("task: fixed task is not installed")
	ErrTaskNotRunning   = errors.New("task: fixed task is not running")
	ErrBackend          = errors.New("task: scheduler backend failed")
	ErrWindowsRequired  = errors.New("task: Windows is required")
)

type Capability uint8

const (
	CapabilityUser Capability = iota
	CapabilityElevated
)

type Definition struct {
	HelperExecutable string
	HelperConfig     string
	UserSID          string
}

type Status struct {
	Installed  bool
	Running    bool
	LastResult uint32
}

type Service interface {
	Inspect(ctx context.Context) (Status, error)
	Install(ctx context.Context, definition Definition) error
	Remove(ctx context.Context) error
	Run(ctx context.Context) error
	Stop(ctx context.Context) error
}

type schedulerBackend interface {
	Inspect(ctx context.Context, path string) (Status, error)
	Register(ctx context.Context, path string, registration taskRegistration) error
	Run(ctx context.Context, path string) error
	Stop(ctx context.Context, path string) error
	Delete(ctx context.Context, path string) error
}

type service struct {
	backend    schedulerBackend
	capability Capability
	resolver   finalPathResolver
}

func New(capability Capability) (Service, error) {
	backend, err := newPlatformSchedulerBackend()
	if err != nil {
		return nil, err
	}
	return newServiceWithBackend(backend, capability, platformResolveFinalHelper)
}

func newServiceWithBackend(backend schedulerBackend, capability Capability, resolver finalPathResolver) (*service, error) {
	if backend == nil {
		return nil, errors.New("task: scheduler backend is required")
	}
	if resolver == nil {
		return nil, errors.New("task: final path resolver is required")
	}
	if capability != CapabilityUser && capability != CapabilityElevated {
		return nil, errors.New("task: invalid capability")
	}
	return &service{backend: backend, capability: capability, resolver: resolver}, nil
}

func (s *service) Inspect(ctx context.Context) (Status, error) {
	if err := contextError(ctx); err != nil {
		return Status{}, err
	}
	status, err := s.backend.Inspect(ctx, TaskPath)
	if contextErr := contextError(ctx); contextErr != nil {
		return Status{}, contextErr
	}
	if errors.Is(err, ErrTaskNotInstalled) {
		return Status{}, nil
	}
	if err != nil {
		return Status{}, stableBackendError(err)
	}
	return status, nil
}

func (s *service) Install(ctx context.Context, definition Definition) error {
	if s.capability != CapabilityElevated {
		return ErrAccessDenied
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	registration, err := buildTaskRegistration(definition, s.resolver)
	if err != nil {
		return err
	}
	err = s.backend.Register(ctx, TaskPath, registration)
	if contextErr := contextError(ctx); contextErr != nil {
		return contextErr
	}
	return stableBackendError(err)
}

func (s *service) Remove(ctx context.Context) error {
	if s.capability != CapabilityElevated {
		return ErrAccessDenied
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	err := s.backend.Delete(ctx, TaskPath)
	if contextErr := contextError(ctx); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, ErrTaskNotInstalled) {
		return nil
	}
	return stableBackendError(err)
}

func (s *service) Run(ctx context.Context) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	err := s.backend.Run(ctx, TaskPath)
	if contextErr := contextError(ctx); contextErr != nil {
		return contextErr
	}
	return stableBackendError(err)
}

func (s *service) Stop(ctx context.Context) error {
	if s.capability != CapabilityElevated {
		return ErrAccessDenied
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	err := s.backend.Stop(ctx, TaskPath)
	if contextErr := contextError(ctx); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, ErrTaskNotRunning) {
		return nil
	}
	return stableBackendError(err)
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return errors.New("task: context is required")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func stableBackendError(err error) error {
	if err == nil {
		return nil
	}
	for _, stable := range []error{
		context.Canceled,
		context.DeadlineExceeded,
		ErrAccessDenied,
		ErrTaskNotInstalled,
		ErrTaskNotRunning,
	} {
		if errors.Is(err, stable) {
			return stable
		}
	}
	return ErrBackend
}

type finalPathResolver func(string) (string, error)

type taskRegistration struct {
	Path                    string
	Principal               taskPrincipal
	Triggers                []taskTrigger
	Actions                 []taskAction
	MultipleInstancesPolicy string
}

type taskPrincipal struct {
	UserSID   string
	LogonType string
	RunLevel  string
}

type taskTrigger struct {
	Kind    string
	UserSID string
}

type taskAction struct {
	Kind       string
	Executable string
	Arguments  string
}

func buildTaskRegistration(definition Definition, resolver finalPathResolver) (taskRegistration, error) {
	if resolver == nil {
		return taskRegistration{}, errors.New("task: final path resolver is required")
	}
	sid, err := canonicalSID(definition.UserSID)
	if err != nil {
		return taskRegistration{}, err
	}
	if err := validateHelperExecutable(definition.HelperExecutable); err != nil {
		return taskRegistration{}, err
	}
	resolved, err := resolver(definition.HelperExecutable)
	if err != nil {
		return taskRegistration{}, errors.New("task: resolve helper executable failed")
	}
	resolved = filepath.Clean(resolved)
	if err := validateHelperExecutable(resolved); err != nil {
		return taskRegistration{}, errors.New("task: resolved executable is not helper.exe")
	}
	config, err := canonicalConfigPath(definition.HelperConfig)
	if err != nil {
		return taskRegistration{}, err
	}

	return taskRegistration{
		Path: TaskPath,
		Principal: taskPrincipal{
			UserSID:   sid,
			LogonType: logonTypeInteractiveToken,
			RunLevel:  runLevelHighest,
		},
		Triggers: []taskTrigger{{
			Kind:    triggerKindLogon,
			UserSID: sid,
		}},
		Actions: []taskAction{{
			Kind:       actionKindExec,
			Executable: resolved,
			Arguments:  `--config "` + config + `"`,
		}},
		MultipleInstancesPolicy: multipleInstancesIgnoreNew,
	}, nil
}

func canonicalSID(value string) (string, error) {
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsFunc(value, unicode.IsControl) {
		return "", errors.New("task: invalid user SID")
	}
	parts := strings.Split(value, "-")
	if len(parts) < 4 || len(parts) > 18 || parts[0] != "S" {
		return "", errors.New("task: invalid user SID")
	}
	if parts[1] != "1" || !canonicalUnsigned(parts[2], 48) {
		return "", errors.New("task: invalid user SID")
	}
	for _, part := range parts[3:] {
		if !canonicalUnsigned(part, 32) {
			return "", errors.New("task: invalid user SID")
		}
	}
	return value, nil
}

func canonicalUnsigned(value string, bits int) bool {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	_, err := strconv.ParseUint(value, 10, bits)
	return err == nil
}

func validateHelperExecutable(value string) error {
	if !safePathText(value) || !filepath.IsAbs(value) ||
		!strings.EqualFold(filepath.Base(filepath.Clean(value)), "helper.exe") {
		return errors.New("task: executable must be an absolute helper.exe path")
	}
	return nil
}

func canonicalConfigPath(value string) (string, error) {
	if !safePathText(value) || !filepath.IsAbs(value) ||
		strings.HasSuffix(value, `\`) || strings.HasSuffix(value, `/`) {
		return "", errors.New("task: config must be an absolute file path")
	}
	cleaned := filepath.Clean(value)
	if cleaned == filepath.VolumeName(cleaned)+string(filepath.Separator) || filepath.Base(cleaned) == "." {
		return "", errors.New("task: config must be an absolute file path")
	}
	info, err := os.Stat(cleaned)
	if err == nil && info.IsDir() {
		return "", errors.New("task: config must be an absolute file path")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("task: config path is unavailable")
	}
	return cleaned, nil
}

func safePathText(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && !strings.ContainsRune(value, '"') &&
		!strings.ContainsFunc(value, unicode.IsControl)
}

type taskXML struct {
	XMLName    xml.Name      `xml:"Task"`
	XMLNS      string        `xml:"xmlns,attr"`
	Version    string        `xml:"version,attr"`
	Principals xmlPrincipals `xml:"Principals"`
	Triggers   xmlTriggers   `xml:"Triggers"`
	Settings   xmlSettings   `xml:"Settings"`
	Actions    xmlActions    `xml:"Actions"`
}

type xmlPrincipals struct {
	Principal xmlPrincipal `xml:"Principal"`
}

type xmlPrincipal struct {
	ID        string `xml:"id,attr"`
	UserID    string `xml:"UserId"`
	LogonType string `xml:"LogonType"`
	RunLevel  string `xml:"RunLevel"`
}

type xmlTriggers struct {
	LogonTrigger xmlLogonTrigger `xml:"LogonTrigger"`
}

type xmlLogonTrigger struct {
	Enabled bool   `xml:"Enabled"`
	UserID  string `xml:"UserId"`
}

type xmlSettings struct {
	MultipleInstancesPolicy string `xml:"MultipleInstancesPolicy"`
}

type xmlActions struct {
	Context string  `xml:"Context,attr"`
	Exec    xmlExec `xml:"Exec"`
}

type xmlExec struct {
	Command   string `xml:"Command"`
	Arguments string `xml:"Arguments"`
}

func renderTaskXML(registration taskRegistration) (string, error) {
	if registration.Path != TaskPath || len(registration.Triggers) != 1 ||
		len(registration.Actions) != 1 || registration.Triggers[0].Kind != triggerKindLogon ||
		registration.Actions[0].Kind != actionKindExec {
		return "", errors.New("task: invalid fixed registration")
	}
	document := taskXML{
		XMLNS:   "http://schemas.microsoft.com/windows/2004/02/mit/task",
		Version: "1.4",
		Principals: xmlPrincipals{Principal: xmlPrincipal{
			ID:        "Principal",
			UserID:    registration.Principal.UserSID,
			LogonType: registration.Principal.LogonType,
			RunLevel:  registration.Principal.RunLevel,
		}},
		Triggers: xmlTriggers{LogonTrigger: xmlLogonTrigger{
			Enabled: true,
			UserID:  registration.Triggers[0].UserSID,
		}},
		Settings: xmlSettings{MultipleInstancesPolicy: registration.MultipleInstancesPolicy},
		Actions: xmlActions{
			Context: "Principal",
			Exec: xmlExec{
				Command:   registration.Actions[0].Executable,
				Arguments: registration.Actions[0].Arguments,
			},
		},
	}
	encoded, err := xml.Marshal(document)
	if err != nil {
		return "", fmt.Errorf("task: encode fixed definition: %w", err)
	}
	return xml.Header + string(encoded), nil
}
