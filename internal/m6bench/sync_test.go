package m6bench

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestValidateSyncConfigUsesSafeRunSchema(t *testing.T) {
	cfg := SyncConfig{
		Rows: 257, BatchSizes: []int{1, 64, 257},
		RunID: "20260729-abc123",
	}
	schema, err := ValidateSyncConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if schema != "m6_20260729_abc123" {
		t.Fatalf("schema = %q", schema)
	}
	for _, invalid := range []SyncConfig{
		{},
		{Rows: 1, BatchSizes: []int{0}, RunID: "run"},
		{Rows: 1, BatchSizes: []int{50001}, RunID: "run"},
		{Rows: 1, BatchSizes: []int{1}, RunID: "../escape"},
	} {
		if _, err := ValidateSyncConfig(invalid); err == nil {
			t.Fatalf("accepted invalid config %#v", invalid)
		}
	}
}

func TestRunSyncIntegrationCreatesVerifiesAndDropsOwnSchema(t *testing.T) {
	dsn := os.Getenv("M6_PG_DSN")
	if dsn == "" {
		t.Skip("M6_PG_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	cfg := SyncConfig{Rows: 257, BatchSizes: []int{1, 64, 257}, RunID: "integration"}
	result, err := RunSync(ctx, pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Batches) != 3 {
		t.Fatalf("batches = %#v", result.Batches)
	}
	for _, batch := range result.Batches {
		if batch.Rows != 257 || batch.DistinctKeys != 257 {
			t.Fatalf("batch = %#v", batch)
		}
	}
	schema, _ := ValidateSyncConfig(cfg)
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)`,
		schema,
	).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatalf("schema %q remained after benchmark", schema)
	}
	if strings.Contains(strings.ToLower(result.ServerVersion), "password") {
		t.Fatalf("server version contains secret-like data: %q", result.ServerVersion)
	}
}
