package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRunCLIEmitsBoundedJSON(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "one.jpg"), []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runCLI([]string{
		"-root", root,
		"-ext", ".jpg",
		"-max-files", "1",
		"-duration", "1s",
		"-streams", "1",
		"-block-kb", "1",
	}, &stdout, &stderr)
	if code != 0 || !bytes.Contains(stdout.Bytes(), []byte(`"files": 1`)) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
