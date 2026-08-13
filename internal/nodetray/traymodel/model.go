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

type PathSelectionResult struct {
	OK           bool   `json:"ok"`
	Path         string `json:"path"`
	Cancelled    bool   `json:"cancelled"`
	ErrorCode    string `json:"errorCode"`
	ErrorSummary string `json:"errorSummary"`
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

// Local console DTOs are deliberately separate from the Agent wire structs.
// Wails only receives display-safe data; notably, delete authorization tokens
// and PostgreSQL credentials never cross this boundary.
type PageRequest struct {
	Offset int `json:"offset"`
	Limit  int `json:"limit"`
}

type LocalTaskCreate struct {
	TaskID     string   `json:"taskId"`
	Roots      []string `json:"roots"`
	Mode       string   `json:"mode"`
	Rescan     bool     `json:"rescan"`
	Extensions []string `json:"extensions"`
}

type LocalAnalysisStart = LocalTaskCreate

type LocalTask struct {
	TaskID           string   `json:"taskId"`
	Source           string   `json:"source"`
	Mode             string   `json:"mode"`
	Stage            int      `json:"stage"`
	Status           string   `json:"status"`
	Roots            []string `json:"roots"`
	ProgressComplete int64    `json:"progressComplete"`
	ProgressTotal    int64    `json:"progressTotal"`
	Speed            string   `json:"speed"`
	Failures         int64    `json:"failures"`
	Duration         string   `json:"duration"`
	SyncStatus       string   `json:"syncStatus"`
	ErrorCode        string   `json:"errorCode"`
	ErrorSummary     string   `json:"errorSummary"`
}

type LocalTaskResult struct {
	OK           bool      `json:"ok"`
	Task         LocalTask `json:"task"`
	ErrorCode    string    `json:"errorCode"`
	ErrorSummary string    `json:"errorSummary"`
}

type LocalTaskPage struct {
	OK           bool        `json:"ok"`
	Tasks        []LocalTask `json:"tasks"`
	Offset       int         `json:"offset"`
	NextOffset   int         `json:"nextOffset"`
	ErrorCode    string      `json:"errorCode"`
	ErrorSummary string      `json:"errorSummary"`
}

type LocalGroupQuery struct {
	Scope            string `json:"scope"`
	RunID            string `json:"runId"`
	Category         string `json:"category"`
	PathContains     string `json:"pathContains"`
	FileNameContains string `json:"fileNameContains"`
	ReviewStatus     string `json:"reviewStatus"`
	Offset           int    `json:"offset"`
	Limit            int    `json:"limit"`
}

type LocalGroupMember struct {
	FileID   int64  `json:"fileId"`
	Path     string `json:"path"`
	FileName string `json:"fileName"`
	Size     int64  `json:"size"`
	Status   string `json:"status"`
	Decision string `json:"decision"`
}

type LocalGroup struct {
	RunID        string             `json:"runId"`
	Generation   int64              `json:"generation"`
	GroupID      string             `json:"groupId"`
	Category     string             `json:"category"`
	Verdict      string             `json:"verdict"`
	ReviewStatus string             `json:"reviewStatus"`
	StageOne     string             `json:"stageOne"`
	StageTwo     string             `json:"stageTwo"`
	StageThree   string             `json:"stageThree"`
	Members      []LocalGroupMember `json:"members"`
}

type LocalGroupPage struct {
	OK           bool         `json:"ok"`
	Groups       []LocalGroup `json:"groups"`
	Offset       int          `json:"offset"`
	NextOffset   int          `json:"nextOffset"`
	ErrorCode    string       `json:"errorCode"`
	ErrorSummary string       `json:"errorSummary"`
}

type LocalReviewDecision struct {
	FileID   int64  `json:"fileId"`
	Decision string `json:"decision"`
}

type LocalReviewSave struct {
	RunID     string                `json:"runId"`
	GroupID   string                `json:"groupId"`
	Reviewer  string                `json:"reviewer"`
	Note      string                `json:"note"`
	Decisions []LocalReviewDecision `json:"decisions"`
}

type LocalDeletePrepare struct {
	RunID   string `json:"runId"`
	GroupID string `json:"groupId"`
}

type LocalDeleteFile struct {
	FileID int64  `json:"fileId"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
}

type LocalDeletePreview struct {
	OK              bool              `json:"ok"`
	BatchID         string            `json:"batchId"`
	SelectionDigest string            `json:"selectionDigest"`
	Count           int               `json:"count"`
	TotalSize       int64             `json:"totalSize"`
	ExpiresAt       int64             `json:"expiresAt"`
	Files           []LocalDeleteFile `json:"files"`
	ErrorCode       string            `json:"errorCode"`
	ErrorSummary    string            `json:"errorSummary"`
}

type LocalDeleteExecute struct {
	BatchID         string `json:"batchId"`
	SelectionDigest string `json:"selectionDigest"`
}

type LocalDeleteItem struct {
	FileID    int64  `json:"fileId"`
	Result    string `json:"result"`
	ErrorCode string `json:"errorCode"`
	Uncertain bool   `json:"uncertain"`
}

type LocalDeleteBatch struct {
	OK           bool              `json:"ok"`
	BatchID      string            `json:"batchId"`
	Status       string            `json:"status"`
	Requested    int               `json:"requested"`
	Succeeded    int               `json:"succeeded"`
	Failed       int               `json:"failed"`
	Uncertain    int               `json:"uncertain"`
	Items        []LocalDeleteItem `json:"items"`
	ErrorCode    string            `json:"errorCode"`
	ErrorSummary string            `json:"errorSummary"`
}

type ImagePreview struct {
	OK           bool   `json:"ok"`
	MIME         string `json:"mime"`
	Width        int32  `json:"width"`
	Height       int32  `json:"height"`
	DataBase64   string `json:"dataBase64"`
	ErrorCode    string `json:"errorCode"`
	ErrorSummary string `json:"errorSummary"`
}
