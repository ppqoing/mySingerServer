package m6bench

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	StatusPass   = "PASS"
	StatusFail   = "FAIL"
	StatusNotRun = "NOT_RUN"
)

type Artifact struct {
	Path   string         `json:"path,omitempty"`
	SHA256 string         `json:"sha256,omitempty"`
	Kind   string         `json:"kind"`
	Data   map[string]any `json:"data"`
}

type GateResult struct {
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type Report struct {
	SchemaVersion int                   `json:"schema_version"`
	Status        string                `json:"status"`
	Gates         map[string]GateResult `json:"gates"`
	Artifacts     []Artifact            `json:"artifacts"`
}

var reportGateNames = []string{
	"tooling",
	"ssd_i_short",
	"ssd_h_short",
	"sync_short",
	"screen_short",
	"screen_million",
	"soak_short",
	"soak_24h",
	"log_audit",
	"hdd_utilization",
}

func LoadArtifact(path string) (Artifact, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Artifact{}, err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return Artifact{}, fmt.Errorf("perfreport: decode %q: %w", path, err)
	}
	version, _ := number(raw["schema_version"])
	if version != SchemaVersion {
		return Artifact{}, fmt.Errorf("perfreport: unsupported schema version in %q", path)
	}
	kind, _ := raw["kind"].(string)
	if kind == "" {
		return Artifact{}, fmt.Errorf("perfreport: artifact %q has no kind", path)
	}
	sum := sha256.Sum256(data)
	return Artifact{
		Path: path, SHA256: hex.EncodeToString(sum[:]), Kind: kind,
		Data: Redact(raw).(map[string]any),
	}, nil
}

func BuildReport(inputs []Artifact) Report {
	report := Report{
		SchemaVersion: SchemaVersion,
		Status:        StatusNotRun,
		Gates:         make(map[string]GateResult, len(reportGateNames)),
		Artifacts:     append([]Artifact(nil), inputs...),
	}
	for _, name := range reportGateNames {
		report.Gates[name] = GateResult{Status: StatusNotRun}
	}
	for _, artifact := range inputs {
		switch artifact.Kind {
		case "tooling":
			setBooleanGate(report.Gates, "tooling", artifact.Data, "tooling verification")
		case "io":
			updateIOGate(report.Gates, artifact.Data)
		case "sync":
			updateSyncGate(report.Gates, artifact.Data)
		case "screen":
			updateScreenGates(report.Gates, artifact.Data)
		case "soak":
			updateSoakGates(report.Gates, artifact.Data)
		case "log_audit":
			setBooleanGate(report.Gates, "log_audit", artifact.Data, "JSONL log audit")
		case "disk":
			updateHDDGate(report.Gates, artifact.Data)
		}
	}
	for _, gate := range report.Gates {
		if gate.Status == StatusFail {
			report.Status = StatusFail
			return report
		}
	}
	required := []string{
		"tooling", "ssd_i_short", "ssd_h_short",
		"screen_short", "soak_short", "log_audit",
	}
	for _, name := range required {
		if report.Gates[name].Status != StatusPass {
			report.Status = StatusNotRun
			return report
		}
	}
	report.Status = "M6_TOOLING_READY"
	return report
}

func setBooleanGate(gates map[string]GateResult, name string, data map[string]any, detail string) {
	passed, ok := data["passed"].(bool)
	if !ok {
		gates[name] = GateResult{Status: StatusFail, Detail: detail + " missing passed flag"}
		return
	}
	status := StatusFail
	if passed {
		status = StatusPass
	}
	gates[name] = GateResult{Status: status, Detail: detail}
}

func updateIOGate(gates map[string]GateResult, data map[string]any) {
	roots, _ := data["roots"].([]any)
	if len(roots) == 0 {
		return
	}
	root, _ := roots[0].(string)
	name := ""
	switch {
	case strings.EqualFold(filepath.Clean(root), filepath.Clean(`I:\tmp`)):
		name = "ssd_i_short"
	case strings.EqualFold(filepath.Clean(root), filepath.Clean(`H:\pik\00000000000`)):
		name = "ssd_h_short"
	default:
		return
	}
	files, filesOK := number(data["files"])
	errors, errorsOK := number(data["errors"])
	status := StatusFail
	if filesOK && errorsOK && files > 0 && errors == 0 {
		status = StatusPass
	}
	gates[name] = GateResult{
		Status: status,
		Detail: fmt.Sprintf("%.0f files, %.0f read errors", files, errors),
	}
}

func updateSyncGate(gates map[string]GateResult, data map[string]any) {
	batches, ok := data["batches"].([]any)
	if !ok || len(batches) == 0 {
		gates["sync_short"] = GateResult{Status: StatusFail, Detail: "missing batch results"}
		return
	}
	for _, raw := range batches {
		batch, ok := raw.(map[string]any)
		if !ok {
			gates["sync_short"] = GateResult{Status: StatusFail, Detail: "malformed batch"}
			return
		}
		rows, rowsOK := number(batch["rows"])
		distinct, distinctOK := number(batch["distinct_keys"])
		if !rowsOK || !distinctOK || rows < 1 || rows != distinct {
			gates["sync_short"] = GateResult{Status: StatusFail, Detail: "row reconciliation failed"}
			return
		}
	}
	gates["sync_short"] = GateResult{Status: StatusPass, Detail: "all batch sizes reconciled"}
}

func updateScreenGates(gates map[string]GateResult, data map[string]any) {
	rows, rowsOK := number(data["rows"])
	expected, expectedOK := number(data["expected_groups"])
	actual, actualOK := number(data["actual_groups"])
	status := StatusFail
	if rowsOK && expectedOK && actualOK && rows >= 100_000 && expected == actual {
		status = StatusPass
	}
	gates["screen_short"] = GateResult{
		Status: status,
		Detail: fmt.Sprintf("%.0f rows, expected %.0f groups, got %.0f", rows, expected, actual),
	}
	if rows >= 1_000_000 {
		gates["screen_million"] = GateResult{
			Status: status,
			Detail: fmt.Sprintf("%.0f rows", rows),
		}
	}
}

func updateSoakGates(gates map[string]GateResult, data map[string]any) {
	elapsed, elapsedOK := number(data["elapsed_ms"])
	stopReason, _ := data["stop_reason"].(string)
	status := StatusFail
	if elapsedOK && elapsed > 0 &&
		(stopReason == "duration" || stopReason == "children_exit") {
		status = StatusPass
	}
	gates["soak_short"] = GateResult{
		Status: status, Detail: fmt.Sprintf("%.0f ms, stop=%s", elapsed, stopReason),
	}
	if elapsed >= float64((24 * 60 * 60 * 1000)) {
		gates["soak_24h"] = GateResult{
			Status: status, Detail: fmt.Sprintf("%.0f ms", elapsed),
		}
	}
}

func updateHDDGate(gates map[string]GateResult, data map[string]any) {
	mediaType, _ := data["media_type"].(string)
	utilization, ok := number(data["utilization"])
	if !strings.EqualFold(mediaType, "hdd") || !ok {
		return
	}
	status := StatusFail
	if utilization >= 0.80 {
		status = StatusPass
	}
	gates["hdd_utilization"] = GateResult{
		Status: status, Detail: fmt.Sprintf("%.3f utilization", utilization),
	}
}

func number(value any) (float64, bool) {
	switch value := value.(type) {
	case float64:
		return value, true
	case float32:
		return float64(value), true
	case int:
		return float64(value), true
	case int64:
		return float64(value), true
	case json.Number:
		number, err := value.Float64()
		return number, err == nil
	default:
		return 0, false
	}
}

func Redact(value any) any {
	switch value := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(value))
		for key, item := range value {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "dsn") ||
				strings.Contains(lower, "password") ||
				strings.Contains(lower, "passwd") ||
				strings.Contains(lower, "token") ||
				strings.Contains(lower, "secret") ||
				strings.Contains(lower, "credential") {
				result[key] = "[REDACTED]"
			} else {
				result[key] = Redact(item)
			}
		}
		return result
	case []any:
		result := make([]any, len(value))
		for index, item := range value {
			result[index] = Redact(item)
		}
		return result
	default:
		return value
	}
}

func WriteReport(report Report, jsonPath, markdownPath string) error {
	if jsonPath == "" || markdownPath == "" {
		return fmt.Errorf("perfreport: JSON and Markdown output paths are required")
	}
	if err := WriteJSON(jsonPath, report); err != nil {
		return err
	}
	var builder strings.Builder
	builder.WriteString("# M6 Performance Report\n\n")
	builder.WriteString("Overall: `" + report.Status + "`\n\n")
	builder.WriteString("| Gate | Status | Detail |\n")
	builder.WriteString("|---|---|---|\n")
	names := append([]string(nil), reportGateNames...)
	sort.Strings(names)
	for _, name := range names {
		gate := report.Gates[name]
		detail := strings.ReplaceAll(gate.Detail, "|", "\\|")
		builder.WriteString(fmt.Sprintf("| %s | %s | %s |\n", name, gate.Status, detail))
	}
	return writeTextAtomic(markdownPath, builder.String())
}

func writeTextAtomic(path, content string) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(parent, ".m6-report-*.tmp")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)
	if _, err := file.WriteString(content); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}
