package m6bench

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSoakHelperSleeps(t *testing.T) {
	if os.Getenv("M6_SOAK_TEST_HELPER") != "1" {
		return
	}
	time.Sleep(5 * time.Second)
}

func TestRunSoakStartsAndStopsOnlyConfiguredChild(t *testing.T) {
	root := filepath.Join(t.TempDir(), "corpus")
	if _, err := GenerateCorpus(context.Background(), CorpusConfig{
		Root: root, Files: 2, Seed: 1, RunID: "soak-test",
	}); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "evidence")
	result, err := RunSoak(context.Background(), SoakConfig{
		CorpusRoot: root,
		Duration:   50 * time.Millisecond,
		Commands: [][]string{{
			os.Args[0], "-test.run=^TestSoakHelperSleeps$",
		}},
		Environment: map[string]string{"M6_SOAK_TEST_HELPER": "1"},
		OutputDir:   output,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Children) != 1 || result.Children[0].PID <= 0 ||
		result.StopReason != "duration" {
		t.Fatalf("soak result = %#v", result)
	}
	if _, err := os.Stat(filepath.Join(output, "child-00.stdout.log")); err != nil {
		t.Fatalf("child stdout evidence missing: %v", err)
	}
}

func TestRunSoakRejectsUnownedCorpus(t *testing.T) {
	if _, err := RunSoak(context.Background(), SoakConfig{
		CorpusRoot: t.TempDir(), Duration: time.Second,
		Commands: [][]string{{os.Args[0]}}, OutputDir: t.TempDir(),
	}); err == nil {
		t.Fatal("RunSoak accepted unowned corpus")
	}
}
