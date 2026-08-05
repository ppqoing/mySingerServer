package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"dedup/internal/firstscreen"
	"dedup/internal/m6bench"
)

func main() {
	os.Exit(runCLI(os.Args[1:], os.Stdout, os.Stderr))
}

func runCLI(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("benchscreen", flag.ContinueOnError)
	flags.SetOutput(stderr)
	rows := flags.Int("rows", 100000, "synthetic feature rows")
	clusterSize := flags.Int("cluster-size", 4, "known near-duplicate group size")
	seed := flags.Uint64("seed", 20260729, "deterministic seed")
	timeout := flags.Duration("timeout", 10*time.Minute, "maximum duration")
	out := flags.String("out", "", "optional JSON output path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	result, err := firstscreen.RunBenchmark(ctx, firstscreen.BenchmarkConfig{
		Rows: *rows, ClusterSize: *clusterSize, Seed: *seed,
	})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if result.ActualGroups != result.ExpectedGroups {
		_, _ = fmt.Fprintln(stderr, "candidate grouping correctness check failed")
		return 1
	}
	if *out != "" {
		if err := m6bench.WriteJSON(*out, result); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
	}
	if err := m6bench.EncodeJSON(stdout, result); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
