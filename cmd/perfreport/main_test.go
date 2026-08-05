package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRunCLIWritesJSONAndMarkdown(t *testing.T) {
	root := t.TempDir()
	artifact := filepath.Join(root, "tooling.json")
	if err := os.WriteFile(artifact, []byte(`{
		"schema_version":1,"kind":"tooling","passed":true
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	jsonPath := filepath.Join(root, "report.json")
	markdownPath := filepath.Join(root, "report.md")
	var stdout, stderr bytes.Buffer
	code := runCLI([]string{
		"-input", artifact,
		"-json", jsonPath,
		"-markdown", markdownPath,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, path := range []string{jsonPath, markdownPath} {
		if info, err := os.Stat(path); err != nil || info.Size() == 0 {
			t.Fatalf("output %s: info=%v err=%v", path, info, err)
		}
	}
}
