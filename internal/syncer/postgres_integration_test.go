package syncer

import (
	"context"
	"crypto/rand"
	"crypto/sha512"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"dedup/internal/proto"
	"dedup/internal/store"
)

func TestPGRemoteUpsertFilesMatchesCentralSchemaWhenIntegrationEnabled(
	t *testing.T,
) {
	t.Parallel()
	dsn := os.Getenv("FS_PG_DSN")
	if dsn == "" {
		t.Skip("set FS_PG_DSN to run PostgreSQL integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	machineID := uniquePGMachineID(t, "remote-files")
	t.Cleanup(func() {
		cleanupPGRows(t, pool, `DELETE FROM files WHERE machine_id=$1`, machineID)
	})

	hash := "schema-compatible-hash"
	remote := &PGRemote{pool: pool}
	tx, err := remote.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback(ctx)
	err = tx.UpsertFiles(ctx, []store.FileRow{{
		MachineID: machineID, DiskNo: 7, Path: `D:\schema.bin`,
		Size: 42, MTime: 123, SHA512: &hash,
		Phase1Done: true, Phase2Done: false, Status: "done",
		MissingMask: 0, UpdatedAt: 456,
	}})
	if err != nil {
		t.Fatalf("UpsertFiles: %v", err)
	}
	if err := tx.CloseBatch(ctx); err != nil {
		t.Fatalf("CloseBatch: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	var phase1, phase2 int
	if err := pool.QueryRow(ctx, `
		SELECT phase1_done, phase2_done
		FROM files WHERE machine_id=$1 AND path=$2`,
		machineID, `D:\schema.bin`,
	).Scan(&phase1, &phase2); err != nil {
		t.Fatal(err)
	}
	if phase1 != 1 || phase2 != 0 {
		t.Fatalf("phase flags = (%d,%d), want (1,0)", phase1, phase2)
	}
}

func TestPostgresSyncIsIdempotentWhenIntegrationEnabled(t *testing.T) {
	t.Parallel()
	dsn := os.Getenv("FS_PG_DSN")
	if dsn == "" {
		t.Skip("set FS_PG_DSN to run PostgreSQL integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	machineID := uniquePGMachineID(t, "syncer-files")
	t.Cleanup(func() {
		cleanupPGRows(t, pool, `DELETE FROM files WHERE machine_id=$1`, machineID)
	})

	local, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	records := []store.EnumUpsert{
		{MachineID: machineID, DiskNo: 1, Path: `D:\a.bin`, Size: 1, MTime: 1, MissingBase: proto.FieldSHA512},
		{MachineID: machineID, DiskNo: 1, Path: `D:\b.bin`, Size: 2, MTime: 2, MissingBase: proto.FieldSHA512},
	}
	if err := local.UpsertEnumerated(ctx, records); err != nil {
		t.Fatal(err)
	}
	if err := local.ApplyHashResults(ctx, machineID, []store.HashResult{
		{Path: records[0].Path, SHA512: "hash-a"},
		{Path: records[1].Path, SHA512: "hash-b"},
	}); err != nil {
		t.Fatal(err)
	}
	uploader := New(local, pool, Config{
		Interval: time.Minute, TriggerRows: 50_000, UpsertBatch: 1,
	}, discardLogger())
	uploader.syncOnce(ctx)
	uploader.syncOnce(ctx)

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM files WHERE machine_id=$1`, machineID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("central rows = %d, want 2", count)
	}
	if pending, err := local.PendingSyncCount(ctx); err != nil || pending != 0 {
		t.Fatalf("local pending = %d, err=%v", pending, err)
	}
}

func TestPGRemoteFeatureUpsertsAndCentralMigrationWhenIntegrationEnabled(t *testing.T) {
	t.Parallel()
	dsn := os.Getenv("FS_PG_DSN")
	if dsn == "" {
		t.Skip("set FS_PG_DSN to run PostgreSQL integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	schema, err := os.ReadFile(filepath.Join("..", "..", "deploy", "central.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for run := 1; run <= 2; run++ {
		if _, err := pool.Exec(ctx, string(schema)); err != nil {
			t.Fatalf("central migration run %d: %v", run, err)
		}
	}

	runToken := uniquePGToken(t)
	imageSHA := derivePGSHA(runToken, "image")
	videoSHA := derivePGSHA(runToken, "video")
	partialSHA := derivePGSHA(runToken, "partial-video")
	keys := []string{imageSHA, videoSHA, partialSHA}
	t.Cleanup(func() {
		cleanupPGRows(t, pool, `DELETE FROM image_features WHERE sha512 = ANY($1)`, keys)
		cleanupPGRows(t, pool, `DELETE FROM video_features WHERE sha512 = ANY($1)`, keys)
	})

	duration := int64(9_000)
	quality := int32(73)
	path := `D:\thumbs\complete.jpg`
	remote := &PGRemote{pool: pool}
	commitFeatures := func(
		images []store.ImageFeatureSyncRow,
		videos []store.VideoFeatureSyncRow,
	) {
		t.Helper()
		tx, err := remote.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err := tx.UpsertImages(ctx, images); err != nil {
			t.Fatal(err)
		}
		if err := tx.UpsertVideos(ctx, videos); err != nil {
			t.Fatal(err)
		}
		if err := tx.CloseBatch(ctx); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}

	commitFeatures(
		[]store.ImageFeatureSyncRow{{
			SHA512: imageSHA, Width: 640, Height: 480,
			PDQ256: []byte{1, 2, 3}, PDQQuality: 80,
			PHashParts: []byte{4}, SobelHist: []byte{5}, UpdatedAt: 1_700_000_000,
		}},
		[]store.VideoFeatureSyncRow{
			{
				SHA512: videoSHA, DurationMS: &duration, ThumbPath: &path,
				ThumbPDQ256: []byte{6, 7}, ThumbQuality: &quality, UpdatedAt: 1_700_000_001,
			},
			{
				SHA512: partialSHA, DurationMS: &duration, UpdatedAt: 1_700_000_002,
			},
		},
	)

	// Update and then resend the same image. Retry a partial video result with
	// all failed fields NULL; it must not erase the successful remote values.
	updatedImage := store.ImageFeatureSyncRow{
		SHA512: imageSHA, Width: 800, Height: 600,
		PDQ256: []byte{8, 9}, PDQQuality: 91,
		PHashParts: []byte{10}, SobelHist: []byte{11}, UpdatedAt: 1_700_000_003,
	}
	nullRetry := store.VideoFeatureSyncRow{
		SHA512: videoSHA, UpdatedAt: 1_700_000_004,
	}
	commitFeatures([]store.ImageFeatureSyncRow{updatedImage}, []store.VideoFeatureSyncRow{nullRetry})
	commitFeatures([]store.ImageFeatureSyncRow{updatedImage}, []store.VideoFeatureSyncRow{nullRetry})
	nullImageRetry := store.ImageFeatureSyncRow{
		SHA512: imageSHA, UpdatedAt: 1_700_000_005,
	}
	commitFeatures([]store.ImageFeatureSyncRow{nullImageRetry}, nil)

	var width, height, imageQuality int32
	var pdq, phash, sobel []byte
	if err := pool.QueryRow(ctx, `
		SELECT width, height, pdq256, pdq_quality, phash_parts, sobel_hist
		FROM image_features WHERE sha512=$1`, imageSHA,
	).Scan(&width, &height, &pdq, &imageQuality, &phash, &sobel); err != nil {
		t.Fatal(err)
	}
	if width != 800 || height != 600 || imageQuality != 91 ||
		string(pdq) != string([]byte{8, 9}) ||
		string(phash) != string([]byte{10}) ||
		string(sobel) != string([]byte{11}) {
		t.Fatalf("updated image = %dx%d q%d pdq=%v phash=%v sobel=%v",
			width, height, imageQuality, pdq, phash, sobel)
	}

	var storedDuration sql.NullInt64
	var storedPath sql.NullString
	var storedPDQ []byte
	var storedQuality sql.NullInt32
	if err := pool.QueryRow(ctx, `
		SELECT duration_ms, thumb_path, thumb_pdq256, thumb_quality
		FROM video_features WHERE sha512=$1`, videoSHA,
	).Scan(&storedDuration, &storedPath, &storedPDQ, &storedQuality); err != nil {
		t.Fatal(err)
	}
	if !storedDuration.Valid || storedDuration.Int64 != duration ||
		!storedPath.Valid || storedPath.String != path ||
		string(storedPDQ) != string([]byte{6, 7}) ||
		!storedQuality.Valid || storedQuality.Int32 != quality {
		t.Fatalf("complete video after NULL retry = duration:%v path:%v pdq:%v quality:%v",
			storedDuration, storedPath, storedPDQ, storedQuality)
	}

	if err := pool.QueryRow(ctx, `
		SELECT duration_ms, thumb_path, thumb_pdq256, thumb_quality
		FROM video_features WHERE sha512=$1`, partialSHA,
	).Scan(&storedDuration, &storedPath, &storedPDQ, &storedQuality); err != nil {
		t.Fatal(err)
	}
	if !storedDuration.Valid || storedDuration.Int64 != duration ||
		storedPath.Valid || storedPDQ != nil || storedQuality.Valid {
		t.Fatalf("partial video = duration:%v path:%v pdq:%v quality:%v",
			storedDuration, storedPath, storedPDQ, storedQuality)
	}

	for _, column := range []string{"duration_ms", "thumb_quality"} {
		var nullable string
		if err := pool.QueryRow(ctx, `
			SELECT is_nullable FROM information_schema.columns
			WHERE table_schema=current_schema()
			  AND table_name='video_features' AND column_name=$1`, column,
		).Scan(&nullable); err != nil {
			t.Fatal(err)
		}
		if nullable != "YES" {
			t.Fatalf("video_features.%s nullable = %s, want YES", column, nullable)
		}
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO image_features (sha512) VALUES ($1)`, strings.ToUpper(derivePGSHA(runToken, "uppercase"))); err == nil {
		t.Fatal("uppercase feature SHA was accepted, want lowercase SHA constraint")
	}
}

func uniquePGToken(t *testing.T) []byte {
	t.Helper()
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		t.Fatal(err)
	}
	return token
}

func derivePGSHA(token []byte, label string) string {
	input := make([]byte, 0, len(token)+len(label))
	input = append(input, token...)
	input = append(input, label...)
	sum := sha512.Sum512(input)
	return hex.EncodeToString(sum[:])
}

func uniquePGMachineID(t *testing.T, label string) string {
	t.Helper()
	return "task9-" + derivePGSHA(uniquePGToken(t), label)[:32]
}

func cleanupPGRows(
	t *testing.T,
	pool *pgxpool.Pool,
	statement string,
	args ...any,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, statement, args...); err != nil {
		t.Errorf("PostgreSQL scoped cleanup: %v", err)
	}
}
