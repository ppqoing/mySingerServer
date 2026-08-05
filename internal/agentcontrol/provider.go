package agentcontrol

import (
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"dedup/internal/nodectl"
	"dedup/internal/worker"
)

const maxReportedWorkers = 1024

var (
	mediaPathSummary = regexp.MustCompile(`(?i)(?:[a-z]:\\|/)[^\r\n]*?\.(?:mp4|mkv|avi|mov|wmv|flv|webm|mp3|wav|flac|m4a|aac|jpg|jpeg|png|gif|webp|bmp|tif|tiff)(?:\b|$)`)
	envSummary       = regexp.MustCompile(`(?i)\benv(?:ironment)?\s*[:=]\s*[^[:space:],;}\]]+`)
)

type Inputs struct {
	MachineID      string
	ExecutablePath string
	ConfigSHA256   string
	StartedAt      time.Time
	ListenerReady  func() bool
	Workers        interface{ RuntimeSnapshot() worker.RuntimeSnapshot }
	SyncHealth     func() SyncHealth
}

type SyncHealth struct {
	Healthy      bool
	ErrorSummary string
}

type Provider struct{ inputs Inputs }

func NewProvider(inputs Inputs) *Provider { return &Provider{inputs: inputs} }

func (p *Provider) ControlStatus() nodectl.Status {
	serviceReady := p.inputs.ListenerReady != nil && p.inputs.ListenerReady()
	var runtime worker.RuntimeSnapshot
	if p.inputs.Workers != nil {
		runtime = p.inputs.Workers.RuntimeSnapshot()
	}
	expected := runtime.Expected
	if expected < 0 {
		expected = 0
	}
	if expected > maxReportedWorkers {
		expected = maxReportedWorkers
	}

	mapped := make([]nodectl.WorkerStatus, expected)
	for index := range mapped {
		mapped[index].Index = index
	}
	for _, source := range runtime.Workers {
		if source.Index < 0 || source.Index >= expected {
			continue
		}
		pid := source.PID
		if pid < 0 {
			pid = 0
		}
		mapped[source.Index] = nodectl.WorkerStatus{
			Index:              source.Index,
			PID:                pid,
			Ready:              source.Ready,
			CurrentTaskSummary: boundedSummary(source.CurrentTaskSummary, 96),
			LastErrorSummary:   boundedSummary(source.LastErrorSummary, 192),
		}
	}
	ready := 0
	for _, status := range mapped {
		if status.Ready {
			ready++
		}
	}

	syncHealth := SyncHealth{}
	if p.inputs.SyncHealth != nil {
		syncHealth = p.inputs.SyncHealth()
	}
	fullyReady := serviceReady && ready == expected
	lifecycle := "starting"
	if fullyReady {
		lifecycle = "running"
	}
	return nodectl.Status{
		Component:        nodectl.ComponentAgent,
		MachineID:        p.inputs.MachineID,
		PID:              os.Getpid(),
		StartedAtUnixMS:  p.inputs.StartedAt.UnixMilli(),
		ExecutablePath:   p.inputs.ExecutablePath,
		ConfigSHA256:     strings.ToLower(p.inputs.ConfigSHA256),
		Lifecycle:        lifecycle,
		ServiceReady:     serviceReady,
		Ready:            fullyReady,
		WorkerExpected:   expected,
		WorkerReady:      ready,
		Workers:          mapped,
		SyncHealthy:      syncHealth.Healthy,
		SyncErrorSummary: safeSummary(syncHealth.ErrorSummary),
		LastErrorSummary: safeSummary(runtime.LastErrorSummary),
	}
}

func safeSummary(value string) string {
	value = nodectl.SanitizeSummary(value)
	value = mediaPathSummary.ReplaceAllString(value, "[REDACTED_PATH]")
	value = envSummary.ReplaceAllString(value, "env=[REDACTED]")
	return nodectl.SanitizeSummary(value)
}

func boundedSummary(value string, maxBytes int) string {
	value = safeSummary(value)
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
