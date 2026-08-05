package traymodel

import "fmt"

type Lifecycle string

const (
	Stopped  Lifecycle = "stopped"
	Starting Lifecycle = "starting"
	Running  Lifecycle = "running"
	Stopping Lifecycle = "stopping"
	Failed   Lifecycle = "failed"
)

type StartMode string

const (
	StartManual    StartMode = "manual"
	StartAutomatic StartMode = "automatic"
)

func (m StartMode) Validate() error {
	if m != StartManual && m != StartAutomatic {
		return fmt.Errorf("invalid start mode")
	}
	return nil
}

type NotificationLevel string

const (
	NotifyImportant NotificationLevel = "important"
	NotifyAll       NotificationLevel = "all"
)

func (n NotificationLevel) Validate() error {
	if n != NotifyImportant && n != NotifyAll {
		return fmt.Errorf("invalid notification level")
	}
	return nil
}

type LocationKind string

const (
	AgentLogs    LocationKind = "agent-logs"
	HelperLogs   LocationKind = "helper-logs"
	AgentBackup  LocationKind = "agent-backup"
	HelperBackup LocationKind = "helper-backup"
)

type ComponentState struct {
	Lifecycle           Lifecycle `json:"lifecycle"`
	Healthy             bool      `json:"healthy"`
	Ready               bool      `json:"ready"`
	PID                 int       `json:"pid"`
	StartedAtUnixMS     int64     `json:"startedAtUnixMs"`
	UptimeSeconds       int64     `json:"uptimeSeconds"`
	WorkerReady         int       `json:"workerReady"`
	WorkerExpected      int       `json:"workerExpected"`
	ActiveRequests      int       `json:"activeRequests"`
	ErrorCode           string    `json:"errorCode"`
	ErrorSummary        string    `json:"errorSummary"`
	NeedsAttention      bool      `json:"needsAttention"`
	RuntimeConfigSHA256 string    `json:"runtimeConfigSha256"`
	SavedConfigSHA256   string    `json:"savedConfigSha256"`
	NeedsRestart        bool      `json:"needsRestart"`
}

type WorkerState struct {
	Index              int    `json:"index"`
	PID                int    `json:"pid"`
	Ready              bool   `json:"ready"`
	CurrentTaskSummary string `json:"currentTaskSummary"`
	LastErrorSummary   string `json:"lastErrorSummary"`
}

type Overview struct {
	MachineID       string         `json:"machineId"`
	Agent           ComponentState `json:"agent"`
	Workers         []WorkerState  `json:"workers"`
	Helper          ComponentState `json:"helper"`
	AgentStartMode  StartMode      `json:"agentStartMode"`
	HelperStartMode StartMode      `json:"helperStartMode"`
	HelperEnabled   bool           `json:"helperEnabled"`
	HelperTaskDrift bool           `json:"helperTaskDrift"`
	LoginStartDrift bool           `json:"loginStartDrift"`
}

type TraySettings struct {
	LoginStartTray         bool              `json:"loginStartTray"`
	AgentStartMode         StartMode         `json:"agentStartMode"`
	HelperEnabled          bool              `json:"helperEnabled"`
	HelperStartMode        StartMode         `json:"helperStartMode"`
	CloseToTray            bool              `json:"closeToTray"`
	RefreshIntervalSeconds int               `json:"refreshIntervalSeconds"`
	NotificationLevel      NotificationLevel `json:"notificationLevel"`
}

func (s TraySettings) Validate() error {
	if err := s.AgentStartMode.Validate(); err != nil {
		return err
	}
	if err := s.HelperStartMode.Validate(); err != nil {
		return err
	}
	if !s.HelperEnabled && s.HelperStartMode == StartAutomatic {
		return fmt.Errorf("automatic Helper requires Helper to be enabled")
	}
	if s.RefreshIntervalSeconds < 1 || s.RefreshIntervalSeconds > 3 {
		return fmt.Errorf("refresh interval must be 1..3 seconds")
	}
	return s.NotificationLevel.Validate()
}

type OperationResult struct {
	OK           bool   `json:"ok"`
	ErrorCode    string `json:"errorCode"`
	ErrorSummary string `json:"errorSummary"`
	UACCancelled bool   `json:"uacCancelled"`
}

type ForceExitResult struct {
	OK               bool     `json:"ok"`
	FailedComponents []string `json:"failedComponents"`
	ErrorCode        string   `json:"errorCode"`
	ErrorSummary     string   `json:"errorSummary"`
}

type ConfigApplyResult struct {
	OK           bool   `json:"ok"`
	Saved        bool   `json:"saved"`
	Restarted    bool   `json:"restarted"`
	SHA256       string `json:"sha256"`
	NeedsRestart bool   `json:"needsRestart"`
	ErrorCode    string `json:"errorCode"`
	ErrorSummary string `json:"errorSummary"`
}
