//go:build windows && videocoreacceptance

package integration

import (
	"os"
	"path/filepath"
	"testing"
)

// TestVideoCoreAcceptanceHarness is the explicit real-machine entry point.
// The injected runner owns the Agent/Worker protocol, continuous process-tree
// sampling, fault injection, and PID-scoped cleanup; the wrapper audits all
// evidence and fails closed.
func TestVideoCoreAcceptanceHarness(t *testing.T) {
	repo := testRepositoryRoot(t)
	stageDir := requiredVideoCoreAcceptanceEnv(t, "VIDEOCORE_ACCEPTANCE_STAGE")
	corpusDir := requiredVideoCoreAcceptanceEnv(t, "VIDEOCORE_ACCEPTANCE_CORPUS")
	evidenceDir := requiredVideoCoreAcceptanceEnv(t, "VIDEOCORE_ACCEPTANCE_EVIDENCE")
	runner := requiredVideoCoreAcceptanceEnv(t, "VIDEOCORE_ACCEPTANCE_RUNNER")
	if os.Getenv("FS_PG_DSN") == "" {
		t.Fatal("FS_PG_DSN is required in the environment for videocoreacceptance")
	}

	result := runPowerShell(t,
		filepath.Join(repo, "scripts", "verify_videocore_acceptance.ps1"),
		"-StageDir", stageDir,
		"-CorpusDir", corpusDir,
		"-EvidenceDir", evidenceDir,
		"-Runner", runner,
	)
	if result.exitCode != 0 {
		t.Fatalf("VideoCore dynamic acceptance failed with exit=%d; inspect evidence directory", result.exitCode)
	}
}

func requiredVideoCoreAcceptanceEnv(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s is required for videocoreacceptance", name)
	}
	if !filepath.IsAbs(value) {
		t.Fatalf("%s must be an absolute path", name)
	}
	return value
}
