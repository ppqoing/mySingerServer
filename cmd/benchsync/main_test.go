package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunCLIRequiresEnvironmentDSNWithoutEchoingArguments(t *testing.T) {
	t.Setenv("M6_PG_DSN", "")
	var stdout, stderr bytes.Buffer
	code := runCLI(
		[]string{"-rows", "10", "-batches", "5", "-run-id", "test"},
		&stdout,
		&stderr,
	)
	if code == 0 || !strings.Contains(stderr.String(), "M6_PG_DSN") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
