// Package nodectl defines the bounded, local control protocol shared by the
// Agent, the delete Helper, and the node tray application.
package nodectl

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	ProtocolVersion        uint16 = 1
	maxRequestIDRunes             = 64
	maxMachineIDRunes             = 128
	maxExecutablePathBytes        = 1024
	maxSummaryRunes               = 512
	maxWorkerTaskBytes            = 96
	maxWorkerErrorBytes           = 192
	maxWorkers                    = 1024
)

type Component string

const (
	ComponentAgent  Component = "agent"
	ComponentHelper Component = "delete-helper"
)

type Command string

const (
	CommandStatus   Command = "status"
	CommandShutdown Command = "shutdown"
)

type WorkerStatus struct {
	Index              int    `msgpack:"index"`
	PID                int    `msgpack:"pid"`
	Ready              bool   `msgpack:"ready"`
	CurrentTaskSummary string `msgpack:"current_task_summary"`
	LastErrorSummary   string `msgpack:"last_error_summary"`
}

type Request struct {
	Version   uint16  `msgpack:"version"`
	RequestID string  `msgpack:"request_id"`
	Command   Command `msgpack:"command"`
}

type Status struct {
	Component        Component      `msgpack:"component"`
	MachineID        string         `msgpack:"machine_id"`
	PID              int            `msgpack:"pid"`
	StartedAtUnixMS  int64          `msgpack:"started_at_unix_ms"`
	ExecutablePath   string         `msgpack:"executable_path"`
	ConfigSHA256     string         `msgpack:"config_sha256"`
	Lifecycle        string         `msgpack:"lifecycle"`
	ServiceReady     bool           `msgpack:"service_ready"`
	Ready            bool           `msgpack:"ready"`
	WorkerExpected   int            `msgpack:"worker_expected"`
	WorkerReady      int            `msgpack:"worker_ready"`
	Workers          []WorkerStatus `msgpack:"workers"`
	SyncHealthy      bool           `msgpack:"sync_healthy"`
	SyncErrorSummary string         `msgpack:"sync_error_summary"`
	LastErrorSummary string         `msgpack:"last_error_summary"`
	ActiveRequests   int            `msgpack:"active_requests"`
}

type Response struct {
	Version      uint16  `msgpack:"version"`
	RequestID    string  `msgpack:"request_id"`
	OK           bool    `msgpack:"ok"`
	ErrorCode    string  `msgpack:"error_code"`
	ErrorSummary string  `msgpack:"error_summary"`
	Status       *Status `msgpack:"status,omitempty"`
}

var (
	lowerSHA256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	uriWithUserinfo    = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^\s/@]+@[^\s]+`)
	jsonSecretValue    = regexp.MustCompile(`(?i)("(?:password|passwd|pwd|secret|token|dsn|database_url|databaseurl|connection_string|connectionstring)"\s*:\s*)"(?:\\.|[^"\\])*"`)
	quotedSecretValue  = regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|dsn|database_url|databaseurl|connection_string|connectionstring)(\s*[:=]\s*)"(?:\\.|[^"\\])*"`)
	secretAssignment   = regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|dsn|database_url|databaseurl|connection_string|connectionstring)(\s*[:=]\s*)[^[:space:],;}\]\["']+`)
	mediaPathPattern   = regexp.MustCompile(`(?i)(?:(?:[a-z]:\\|/)|(?:\\\\[^\\\r\n]+\\[^\\\r\n]+\\))[^\r\n]*?\.(?:mp4|mkv|avi|mov|wmv|flv|webm|mp3|wav|flac|m4a|aac|jpg|jpeg|png|gif|webp|bmp|tif|tiff)(?:\b|$)`)
)

func (r Request) Validate() error {
	if r.Version != ProtocolVersion {
		return fmt.Errorf("unsupported protocol version %d", r.Version)
	}
	if err := validateString("request_id", r.RequestID, 1, maxRequestIDRunes); err != nil {
		return err
	}
	if r.Command != CommandStatus && r.Command != CommandShutdown {
		return fmt.Errorf("unsupported command %q", r.Command)
	}
	return nil
}

func (s Status) Validate() error {
	if s.Component != ComponentAgent && s.Component != ComponentHelper {
		return fmt.Errorf("unsupported component %q", s.Component)
	}
	if err := ValidateControlIdentity(s.MachineID, s.ExecutablePath); err != nil {
		return err
	}
	if s.ConfigSHA256 != "" && !lowerSHA256Pattern.MatchString(s.ConfigSHA256) {
		return errors.New("config_sha256 must be empty or 64 lower-case hexadecimal characters")
	}
	if s.Lifecycle != "starting" && s.Lifecycle != "running" && s.Lifecycle != "stopping" && s.Lifecycle != "failed" {
		return fmt.Errorf("unsupported lifecycle %q", s.Lifecycle)
	}
	if s.PID < 0 || s.WorkerExpected < 0 || s.WorkerReady < 0 || s.ActiveRequests < 0 {
		return errors.New("pid and counters must not be negative")
	}
	if s.WorkerExpected > maxWorkers || s.WorkerReady > maxWorkers || len(s.Workers) > maxWorkers {
		return fmt.Errorf("worker count exceeds %d", maxWorkers)
	}
	if err := validateSummary("sync_error_summary", s.SyncErrorSummary); err != nil {
		return err
	}
	if err := validateSummary("last_error_summary", s.LastErrorSummary); err != nil {
		return err
	}
	if s.Component == ComponentHelper {
		if s.WorkerExpected != 0 || s.WorkerReady != 0 || len(s.Workers) != 0 {
			return errors.New("helper must not report workers")
		}
		if s.SyncHealthy || s.SyncErrorSummary != "" {
			return errors.New("helper must not report sync state")
		}
		return nil
	}
	if len(s.Workers) != s.WorkerExpected {
		return errors.New("agent worker list does not match worker_expected")
	}
	seen := make(map[int]struct{}, len(s.Workers))
	ready := 0
	for _, worker := range s.Workers {
		if worker.Index < 0 || worker.Index >= s.WorkerExpected {
			return fmt.Errorf("worker index %d outside expected range", worker.Index)
		}
		if _, duplicate := seen[worker.Index]; duplicate {
			return fmt.Errorf("duplicate worker index %d", worker.Index)
		}
		seen[worker.Index] = struct{}{}
		if worker.PID < 0 {
			return errors.New("worker pid must not be negative")
		}
		if err := validateWorkerSummary("current_task_summary", worker.CurrentTaskSummary, maxWorkerTaskBytes); err != nil {
			return err
		}
		if err := validateWorkerSummary("last_error_summary", worker.LastErrorSummary, maxWorkerErrorBytes); err != nil {
			return err
		}
		if worker.Ready {
			ready++
		}
	}
	if s.WorkerReady != ready {
		return errors.New("worker_ready does not match worker readiness")
	}
	return nil
}

// ValidateControlIdentity checks fields that identify a local control endpoint
// before the process opens any runtime resources. Errors intentionally describe
// only the violated bound and never include the rejected values.
func ValidateControlIdentity(machineID, executablePath string) error {
	if err := validateString("machine_id", machineID, 1, maxMachineIDRunes); err != nil {
		return err
	}
	if !utf8.ValidString(executablePath) || len(executablePath) == 0 || len(executablePath) > maxExecutablePathBytes {
		return errors.New("executable_path must be valid UTF-8 and 1..1024 bytes")
	}
	return nil
}

func (r Response) Validate() error {
	if r.Version != ProtocolVersion {
		return fmt.Errorf("unsupported protocol version %d", r.Version)
	}
	if err := validateString("request_id", r.RequestID, 1, maxRequestIDRunes); err != nil {
		return err
	}
	if r.OK {
		if r.ErrorCode != "" || r.ErrorSummary != "" {
			return errors.New("successful response must not carry an error")
		}
		if r.Status != nil {
			return r.Status.Validate()
		}
		return nil
	}
	if r.Status != nil {
		return errors.New("failed response must not carry status")
	}
	if !isStableErrorCode(r.ErrorCode) {
		return fmt.Errorf("unsupported error_code %q", r.ErrorCode)
	}
	return validateSummary("error_summary", r.ErrorSummary)
}

// SanitizeSummary removes non-display control characters and common secret
// representations before limiting the value to the protocol's 512-rune bound.
func SanitizeSummary(value string) string {
	if !utf8.ValidString(value) {
		return "[INVALID_UTF8]"
	}
	value = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == 0 || (r < 0x20 && r != '\t') {
			return -1
		}
		return r
	}, value)
	value = uriWithUserinfo.ReplaceAllString(value, "[REDACTED_URI]")
	value = jsonSecretValue.ReplaceAllString(value, "$1\"[REDACTED]\"")
	value = quotedSecretValue.ReplaceAllString(value, "$1$2\"[REDACTED]\"")
	value = secretAssignment.ReplaceAllString(value, "$1$2[REDACTED]")
	value = mediaPathPattern.ReplaceAllString(value, "[REDACTED_PATH]")
	return truncateRunes(value, maxSummaryRunes)
}

func validateString(field, value string, minRunes, maxRunes int) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s must be valid UTF-8", field)
	}
	length := utf8.RuneCountInString(value)
	if length < minRunes || length > maxRunes {
		return fmt.Errorf("%s must contain %d..%d runes", field, minRunes, maxRunes)
	}
	return nil
}

func validateSummary(field, value string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s must be valid UTF-8", field)
	}
	if utf8.RuneCountInString(value) > maxSummaryRunes {
		return fmt.Errorf("%s exceeds %d runes", field, maxSummaryRunes)
	}
	if value != SanitizeSummary(value) {
		return fmt.Errorf("%s is not sanitized", field)
	}
	if mediaPathPattern.MatchString(value) {
		return fmt.Errorf("%s contains a media path", field)
	}
	return nil
}

func validateWorkerSummary(field, value string, maxBytes int) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("worker %s must be valid UTF-8", field)
	}
	if len(value) > maxBytes {
		return fmt.Errorf("worker %s exceeds %d bytes", field, maxBytes)
	}
	if value != SanitizeSummary(value) {
		return fmt.Errorf("worker %s is not sanitized", field)
	}
	if mediaPathPattern.MatchString(value) {
		return fmt.Errorf("worker %s contains a media path", field)
	}
	return nil
}

func truncateRunes(value string, limit int) string {
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	return string([]rune(value)[:limit])
}

func isStableErrorCode(value string) bool {
	switch value {
	case "invalid_request", "unsupported_command", "status_unavailable", "internal_error":
		return true
	default:
		return false
	}
}
