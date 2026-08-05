package main

import (
	"bytes"
	"path/filepath"
	"testing"
)

func TestRunCLIProducesOwnedCorpusManifest(t *testing.T) {
	root := filepath.Join(t.TempDir(), "corpus")
	var stdout, stderr bytes.Buffer
	code := runCLI([]string{
		"-root", root, "-files", "4", "-duplicates", "1",
		"-seed", "7", "-run-id", "cli-test",
	}, &stdout, &stderr)
	if code != 0 ||
		!bytes.Contains(stdout.Bytes(), []byte(`"run_id": "cli-test"`)) ||
		!bytes.Contains(stdout.Bytes(), []byte(`"file_count": 4`)) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if stdout.Len() > 2048 || bytes.Contains(stdout.Bytes(), []byte(`"sha256"`)) {
		t.Fatalf("CLI leaked full manifest instead of bounded summary: %d bytes", stdout.Len())
	}
}
