package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"dedup/internal/m6bench"
)

func main() {
	os.Exit(runCLI(os.Args[1:], os.Stdout, os.Stderr))
}

func runCLI(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("benchsync", flag.ContinueOnError)
	flags.SetOutput(stderr)
	rows := flags.Int("rows", 100000, "deterministic rows per batch-size run")
	batches := flags.String("batches", "1000,5000,10000,50000", "comma-separated transaction sizes")
	runID := flags.String("run-id", time.Now().UTC().Format("20060102_150405_000000000"), "isolated run identifier")
	timeout := flags.Duration("timeout", 30*time.Minute, "maximum duration")
	out := flags.String("out", "", "optional JSON output path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	dsn, ok := os.LookupEnv("M6_PG_DSN")
	if !ok || strings.TrimSpace(dsn) == "" {
		_, _ = fmt.Fprintln(stderr, "M6_PG_DSN is required and is read only from the environment")
		return 2
	}
	var batchSizes []int
	for _, raw := range strings.Split(*batches, ",") {
		value, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "invalid batch size")
			return 2
		}
		batchSizes = append(batchSizes, value)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "PostgreSQL connection configuration failed")
		return 1
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		_, _ = fmt.Fprintln(stderr, "PostgreSQL is unavailable")
		return 1
	}
	result, err := m6bench.RunSync(ctx, pool, m6bench.SyncConfig{
		Rows: *rows, BatchSizes: batchSizes, RunID: *runID,
	})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "synchronization benchmark failed")
		return 1
	}
	if *out != "" {
		if err := m6bench.WriteJSON(*out, result); err != nil {
			_, _ = fmt.Fprintln(stderr, "write output failed")
			return 1
		}
	}
	if err := m6bench.EncodeJSON(stdout, result); err != nil {
		_, _ = fmt.Fprintln(stderr, "encode output failed")
		return 1
	}
	return 0
}
