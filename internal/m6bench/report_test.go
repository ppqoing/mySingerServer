package m6bench

import (
	"strings"
	"testing"
)

func TestBuildReportKeepsMissingLongAndHDDGatesNotRun(t *testing.T) {
	report := BuildReport([]Artifact{
		{Kind: "tooling", Data: map[string]any{"passed": true}},
		{Kind: "io", Data: map[string]any{
			"roots": []any{`I:\tmp`}, "files": float64(100), "errors": float64(0),
		}},
		{Kind: "io", Data: map[string]any{
			"roots": []any{`H:\pik\00000000000`}, "files": float64(100), "errors": float64(0),
		}},
		{Kind: "screen", Data: map[string]any{
			"rows": float64(500000), "expected_groups": float64(12500),
			"actual_groups": float64(12500),
		}},
		{Kind: "soak", Data: map[string]any{
			"elapsed_ms": float64(60000), "stop_reason": "duration",
		}},
		{Kind: "log_audit", Data: map[string]any{"passed": true}},
	})
	if report.Gates["hdd_utilization"].Status != StatusNotRun ||
		report.Gates["screen_million"].Status != StatusNotRun ||
		report.Gates["soak_24h"].Status != StatusNotRun {
		t.Fatalf("long gates = %#v", report.Gates)
	}
	if report.Status != "M6_TOOLING_READY" {
		t.Fatalf("status = %q, gates=%#v", report.Status, report.Gates)
	}
}

func TestBuildReportPreservesExplicitFailure(t *testing.T) {
	report := BuildReport([]Artifact{
		{Kind: "io", Data: map[string]any{
			"roots": []any{`I:\tmp`}, "files": float64(100), "errors": float64(2),
		}},
	})
	if report.Gates["ssd_i_short"].Status != StatusFail ||
		report.Status != StatusFail {
		t.Fatalf("report = %#v", report)
	}
}

func TestRedactRecursivelyRemovesCredentialValues(t *testing.T) {
	input := map[string]any{
		"nested": map[string]any{
			"pg_dsn":   "postgres://user:secret@localhost/db",
			"Password": "secret",
			"safe":     "visible",
		},
		"items": []any{map[string]any{"api_token": "secret-token"}},
	}
	output := Redact(input)
	text := strings.ToLower(output.(map[string]any)["nested"].(map[string]any)["pg_dsn"].(string))
	if text != "[redacted]" ||
		output.(map[string]any)["nested"].(map[string]any)["safe"] != "visible" {
		t.Fatalf("redacted output = %#v", output)
	}
}
