package main

import (
	"bytes"
	"testing"
)

func TestRunCLIRejectsMissingCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runCLI([]string{"-corpus-root", t.TempDir(), "-duration", "1s"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
