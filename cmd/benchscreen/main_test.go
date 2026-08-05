package main

import (
	"bytes"
	"testing"
)

func TestRunCLIEmitsCorrectQuickResult(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runCLI([]string{
		"-rows", "10000",
		"-cluster-size", "4",
		"-seed", "20260729",
		"-timeout", "30s",
	}, &stdout, &stderr)
	if code != 0 ||
		!bytes.Contains(stdout.Bytes(), []byte(`"kind": "screen"`)) ||
		!bytes.Contains(stdout.Bytes(), []byte(`"rows": 10000`)) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
