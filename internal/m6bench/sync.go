package m6bench

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var syncRunIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,47}$`)

type SyncConfig struct {
	Rows       int
	BatchSizes []int
	RunID      string
}

type SyncBatchResult struct {
	BatchSize     int     `json:"batch_size"`
	Rows          int64   `json:"rows"`
	DistinctKeys  int64   `json:"distinct_keys"`
	ElapsedMS     int64   `json:"elapsed_ms"`
	RowsPerSecond float64 `json:"rows_per_second"`
}

type SyncResult struct {
	SchemaVersion int               `json:"schema_version"`
	Kind          string            `json:"kind"`
	Rows          int               `json:"rows"`
	ServerVersion string            `json:"server_version"`
	Batches       []SyncBatchResult `json:"batches"`
}

func ValidateSyncConfig(cfg SyncConfig) (string, error) {
	if cfg.Rows < 1 || cfg.Rows > 10_000_000 {
		return "", fmt.Errorf("benchsync: rows must be in 1..10000000")
	}
	if len(cfg.BatchSizes) == 0 {
		return "", fmt.Errorf("benchsync: at least one batch size is required")
	}
	for _, size := range cfg.BatchSizes {
		if size < 1 || size > 50_000 {
			return "", fmt.Errorf("benchsync: batch size must be in 1..50000")
		}
	}
	if !syncRunIDPattern.MatchString(cfg.RunID) {
		return "", fmt.Errorf("benchsync: run ID must use ASCII letters, digits, underscore, or dash")
	}
	return "m6_" + strings.ToLower(strings.ReplaceAll(cfg.RunID, "-", "_")), nil
}

func RunSync(
	ctx context.Context,
	pool *pgxpool.Pool,
	cfg SyncConfig,
) (result SyncResult, err error) {
	if pool == nil {
		return SyncResult{}, fmt.Errorf("benchsync: PostgreSQL pool is nil")
	}
	schema, err := ValidateSyncConfig(cfg)
	if err != nil {
		return SyncResult{}, err
	}
	schemaSQL := pgx.Identifier{schema}.Sanitize()
	if _, err := pool.Exec(ctx, `CREATE SCHEMA `+schemaSQL); err != nil {
		return SyncResult{}, fmt.Errorf("benchsync: create isolated schema: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, cleanupErr := pool.Exec(
			cleanupCtx,
			`DROP SCHEMA `+schemaSQL+` CASCADE`,
		); err == nil && cleanupErr != nil {
			err = fmt.Errorf("benchsync: drop isolated schema: %w", cleanupErr)
		}
	}()

	result = SyncResult{
		SchemaVersion: SchemaVersion,
		Kind:          "sync",
		Rows:          cfg.Rows,
	}
	if err := pool.QueryRow(ctx, `SHOW server_version`).Scan(&result.ServerVersion); err != nil {
		return SyncResult{}, fmt.Errorf("benchsync: read server version: %w", err)
	}
	for _, batchSize := range cfg.BatchSizes {
		table := fmt.Sprintf("batch_%d", batchSize)
		tableSQL := pgx.Identifier{schema, table}.Sanitize()
		if _, err := pool.Exec(ctx, `CREATE UNLOGGED TABLE `+tableSQL+` (
			machine_id text NOT NULL,
			path text NOT NULL,
			size bigint NOT NULL,
			sha512 char(128) NOT NULL,
			PRIMARY KEY(machine_id, path)
		)`); err != nil {
			return SyncResult{}, fmt.Errorf("benchsync: create batch table: %w", err)
		}
		started := time.Now()
		for offset := 0; offset < cfg.Rows; offset += batchSize {
			end := offset + batchSize
			if end > cfg.Rows {
				end = cfg.Rows
			}
			tx, err := pool.Begin(ctx)
			if err != nil {
				return SyncResult{}, fmt.Errorf("benchsync: begin batch: %w", err)
			}
			batch := &pgx.Batch{}
			for index := offset; index < end; index++ {
				batch.Queue(
					`INSERT INTO `+tableSQL+`
						(machine_id,path,size,sha512)
					 VALUES($1,$2,$3,$4)
					 ON CONFLICT(machine_id,path) DO UPDATE
					 SET size=excluded.size,sha512=excluded.sha512`,
					"m6", fmt.Sprintf("D:/m6/%09d.dat", index),
					int64(index+1), fmt.Sprintf("%0128x", index+1),
				)
			}
			batchResults := tx.SendBatch(ctx, batch)
			for index := offset; index < end; index++ {
				if _, err := batchResults.Exec(); err != nil {
					_ = batchResults.Close()
					_ = tx.Rollback(ctx)
					return SyncResult{}, fmt.Errorf("benchsync: execute batch: %w", err)
				}
			}
			if err := batchResults.Close(); err != nil {
				_ = tx.Rollback(ctx)
				return SyncResult{}, fmt.Errorf("benchsync: close batch: %w", err)
			}
			if err := tx.Commit(ctx); err != nil {
				return SyncResult{}, fmt.Errorf("benchsync: commit batch: %w", err)
			}
		}
		elapsed := time.Since(started)
		var rows, distinct int64
		if err := pool.QueryRow(ctx,
			`SELECT count(*), count(DISTINCT path) FROM `+tableSQL,
		).Scan(&rows, &distinct); err != nil {
			return SyncResult{}, fmt.Errorf("benchsync: verify rows: %w", err)
		}
		current := SyncBatchResult{
			BatchSize: batchSize, Rows: rows, DistinctKeys: distinct,
			ElapsedMS: elapsed.Milliseconds(),
		}
		if elapsed > 0 {
			current.RowsPerSecond = float64(rows) / elapsed.Seconds()
		}
		result.Batches = append(result.Batches, current)
	}
	return result, nil
}
