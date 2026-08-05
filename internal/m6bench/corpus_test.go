package m6bench

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateAndCleanCorpusUsesOwnedManifestOnly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "corpus")
	cfg := CorpusConfig{
		Root: root, Files: 12, DuplicateGroups: 2, SparseFiles: 1,
		Seed: 20260729, RunID: "unit-test",
	}
	manifest, err := GenerateCorpus(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.RunID != "unit-test" || len(manifest.Files) != 13 {
		t.Fatalf("manifest = %#v", manifest)
	}
	sentinel := filepath.Join(root, "keep-me.txt")
	if err := os.WriteFile(sentinel, []byte("unlisted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CleanCorpus(root, "wrong-run"); err == nil {
		t.Fatal("cleanup accepted wrong run ID")
	}
	if err := CleanCorpus(root, "unit-test"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("cleanup removed unlisted sentinel: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, CorpusMarker)); !os.IsNotExist(err) {
		t.Fatalf("ownership marker remains: %v", err)
	}
}

func TestGenerateCorpusRejectsProtectedAndAmbiguousRoots(t *testing.T) {
	workspace, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{
		`I:\tmp`,
		`I:\tmp\child`,
		`H:\pik\00000000000`,
		`\\server\share\m6`,
		filepath.VolumeName(workspace) + `\`,
		filepath.Clean(filepath.Join(workspace, "..", filepath.Base(workspace))),
	} {
		if _, err := GenerateCorpus(context.Background(), CorpusConfig{
			Root: root, Files: 2, Seed: 1, RunID: "reject",
		}); err == nil {
			t.Fatalf("GenerateCorpus accepted %q", root)
		}
	}
}

func TestGenerateCorpusRefusesNonEmptyUnmarkedDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "existing.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateCorpus(context.Background(), CorpusConfig{
		Root: root, Files: 2, Seed: 1, RunID: "reject",
	}); err == nil {
		t.Fatal("GenerateCorpus accepted non-empty unmarked directory")
	}
}
