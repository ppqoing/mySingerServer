package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"dedup/internal/m6bench"
)

func main() {
	os.Exit(runCLI(os.Args[1:], os.Stdout, os.Stderr))
}

func runCLI(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("corpusgen", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", "", "explicit generated-corpus root")
	files := flags.Int("files", 1000, "regular files")
	duplicates := flags.Int("duplicates", 100, "two-file duplicate groups")
	sparse := flags.Int("sparse", 0, "64 MiB sparse files")
	seed := flags.Uint64("seed", 20260729, "deterministic seed")
	runID := flags.String("run-id", time.Now().UTC().Format("20060102_150405_000000000"), "ownership run ID")
	clean := flags.Bool("clean", false, "clean only files in the owned manifest")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *clean {
		if err := m6bench.CleanCorpus(*root, *runID); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	manifest, err := m6bench.GenerateCorpus(context.Background(), m6bench.CorpusConfig{
		Root: *root, Files: *files, DuplicateGroups: *duplicates,
		SparseFiles: *sparse, Seed: *seed, RunID: *runID,
	})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	var totalBytes int64
	for _, file := range manifest.Files {
		totalBytes += file.Size
	}
	summary := struct {
		SchemaVersion int    `json:"schema_version"`
		Kind          string `json:"kind"`
		RunID         string `json:"run_id"`
		Root          string `json:"root"`
		FileCount     int    `json:"file_count"`
		TotalBytes    int64  `json:"total_bytes"`
		Manifest      string `json:"manifest"`
	}{
		SchemaVersion: m6bench.SchemaVersion,
		Kind:          "corpus",
		RunID:         manifest.RunID,
		Root:          manifest.Root,
		FileCount:     len(manifest.Files),
		TotalBytes:    totalBytes,
		Manifest:      ".m6-corpus-manifest.json",
	}
	if err := m6bench.EncodeJSON(stdout, summary); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
