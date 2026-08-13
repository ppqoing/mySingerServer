package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"dedup/internal/proto"
)

func TestOpenMigratesLegacySyncQueueWithGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`
		CREATE TABLE sync_queue (
		    table_name TEXT NOT NULL,
		    row_pk TEXT NOT NULL,
		    synced INTEGER NOT NULL DEFAULT 0,
		    enqueued_at INTEGER NOT NULL DEFAULT 0,
		    PRIMARY KEY (table_name, row_pk)
		);`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy database: %v", err)
	}
	defer db.Close()
	var generation int64
	if err := db.db.QueryRow(`
		SELECT generation FROM sync_queue
		WHERE table_name='files' AND row_pk='missing'`,
	).Scan(&generation); err != sql.ErrNoRows {
		t.Fatalf("generation column query error = %v, want sql.ErrNoRows", err)
	}
}

func TestOpenIsIdempotentAndEnumerationPrunesUnchangedFiles(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	rec := EnumUpsert{
		MachineID:   "machine-a",
		DiskNo:      2,
		Path:        `D:\media\a.txt`,
		Size:        3,
		MTime:       100,
		MissingBase: proto.FieldSHA512,
	}
	if err := db.UpsertEnumerated(ctx, []EnumUpsert{rec}); err != nil {
		t.Fatalf("first UpsertEnumerated: %v", err)
	}
	if err := db.ApplyHashResults(ctx, "machine-a", []HashResult{{
		Path: rec.Path, SHA512: "abc",
	}}); err != nil {
		t.Fatalf("ApplyHashResults: %v", err)
	}
	if err := db.UpsertEnumerated(ctx, []EnumUpsert{rec}); err != nil {
		t.Fatalf("second UpsertEnumerated: %v", err)
	}

	var count, missing, phase1 int
	var status string
	if err := db.db.QueryRowContext(ctx,
		`SELECT count(*), missing_mask, phase1_done, status FROM files WHERE machine_id=? AND path=?`,
		rec.MachineID, rec.Path).Scan(&count, &missing, &phase1, &status); err != nil {
		t.Fatal(err)
	}
	if count != 1 || missing != 0 || phase1 != 1 || status != proto.StatusDone {
		t.Fatalf("unchanged row = count:%d missing:%d phase1:%d status:%s",
			count, missing, phase1, status)
	}

	db.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer db.Close()
}

func TestEnumerationChangesAndForcedRescanBecomePending(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rec := EnumUpsert{
		MachineID: "machine-a", DiskNo: 1, Path: `D:\a.jpg`,
		Size: 10, MTime: 100, MissingBase: proto.FieldSHA512 | proto.FieldPDQ256,
	}
	if err := db.UpsertEnumerated(ctx, []EnumUpsert{rec}); err != nil {
		t.Fatal(err)
	}
	if err := db.ApplyHashResults(ctx, "machine-a", []HashResult{{
		Path: rec.Path, SHA512: "hash",
	}}); err != nil {
		t.Fatal(err)
	}

	rec.MTime = 101
	if err := db.UpsertEnumerated(ctx, []EnumUpsert{rec}); err != nil {
		t.Fatal(err)
	}
	assertPendingSHA(t, db, rec.Path)

	if err := db.ApplyHashResults(ctx, "machine-a", []HashResult{{
		Path: rec.Path, SHA512: "hash2",
	}}); err != nil {
		t.Fatal(err)
	}
	rec.Force = true
	if err := db.UpsertEnumerated(ctx, []EnumUpsert{rec}); err != nil {
		t.Fatal(err)
	}
	assertPendingSHA(t, db, rec.Path)
}

func TestDefaultStageOneUnchangedVideoReusesOnlyCompleteContactCache(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	required := RequiredStageOneMask(MediaVideo)
	record := EnumUpsert{
		MachineID: "machine-a", DiskNo: 1, Path: `D:\complete.mp4`,
		Size: 10, MTime: 20, MissingBase: required,
	}
	if err := db.UpsertEnumerated(ctx, []EnumUpsert{record}); err != nil {
		t.Fatal(err)
	}
	duration, quality := int64(1234), int32(88)
	width, height := int32(960), int32(540)
	sha := phase1TestSHA()
	if _, err := db.SaveAnalysis(ctx, AnalysisResult{
		MachineID: record.MachineID, Path: record.Path, Kind: MediaVideo,
		Size: record.Size, MTime: record.MTime, SHA512: sha,
		RequestedFields: required, FieldsDone: required,
		DurationMS: &duration, ThumbPath: `D:\cache\contact.jpg`,
		ThumbPDQ: make([]byte, 32), ThumbQuality: &quality,
		ThumbWidth: &width, ThumbHeight: &height,
	}); err != nil {
		t.Fatalf("SaveAnalysis: %v", err)
	}
	if err := db.UpsertEnumerated(ctx, []EnumUpsert{record}); err != nil {
		t.Fatal(err)
	}
	assertStageOneFileState(t, db, record.Path, proto.StatusDone, 0, true, true)

	if _, err := db.db.ExecContext(ctx, `
		UPDATE video_features SET thumb_width=NULL, thumb_height=NULL
		WHERE sha512=?1`, hex.EncodeToString(sha)); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertEnumerated(ctx, []EnumUpsert{record}); err != nil {
		t.Fatal(err)
	}
	assertStageOneFileState(
		t, db, record.Path, proto.StatusPartial,
		proto.FieldVideoContactSheet, false, true,
	)
	pending, err := db.PendingSnapshot(ctx, record.MachineID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending[record.DiskNo]) != 1 ||
		pending[record.DiskNo][0].MissingMask != proto.FieldVideoContactSheet {
		t.Fatalf("pending explicit video fields = %#v", pending)
	}
}

func TestDefaultStageOneRevalidateCompleteSharedCacheRestoresDone(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	required := RequiredStageOneMask(MediaVideo)
	record := EnumUpsert{
		MachineID: "machine-a", DiskNo: 1, Path: `D:\shared.mp4`,
		Size: 10, MTime: 20, MissingBase: required,
	}
	if err := db.UpsertEnumerated(ctx, []EnumUpsert{record}); err != nil {
		t.Fatal(err)
	}
	sha := phase1TestSHA()
	if _, err := db.db.ExecContext(ctx, `
		UPDATE files SET sha512=?1, status='failed', error='retry failed',
			missing_mask=?2, phase1_done=0 WHERE path=?3;
		INSERT INTO video_features
			(sha512, duration_ms, thumb_path, thumb_pdq256, thumb_quality, thumb_width, thumb_height)
		VALUES (?1, 1234, 'shared.jpg', zeroblob(32), 80, 960, 540)`,
		hex.EncodeToString(sha), proto.FieldVideoContactSheet, record.Path,
	); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertEnumerated(ctx, []EnumUpsert{record}); err != nil {
		t.Fatal(err)
	}
	var status string
	var errorText sql.NullString
	var missing uint32
	var phase1 int
	if err := db.db.QueryRowContext(ctx, `
		SELECT status, error, missing_mask, phase1_done FROM files WHERE path=?1`, record.Path,
	).Scan(&status, &errorText, &missing, &phase1); err != nil {
		t.Fatal(err)
	}
	if status != proto.StatusDone || errorText.Valid || missing != 0 || phase1 != 1 {
		t.Fatalf("revalidated complete cache = %q/%#v/%#x/%d, want done/null/0/1",
			status, errorText, missing, phase1)
	}
}

func TestDefaultStageOneRevalidateDoesNotCompletePendingPhaseTwo(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	required := RequiredStageOneMask(MediaImage)
	record := EnumUpsert{
		MachineID: "machine-a", DiskNo: 1, Path: `D:\phase-two.jpg`,
		Size: 10, MTime: 20, MissingBase: required,
	}
	if err := db.UpsertEnumerated(ctx, []EnumUpsert{record}); err != nil {
		t.Fatal(err)
	}
	sha := phase1TestSHA()
	if _, err := db.db.ExecContext(ctx, `
		UPDATE files SET sha512=?1, status='partial', missing_mask=?2,
			phase1_done=1, phase2_done=0 WHERE path=?3;
		INSERT INTO image_features (sha512, width, height, pdq256, pdq_quality)
		VALUES (?1, 640, 480, zeroblob(32), 80)`,
		hex.EncodeToString(sha), proto.FieldPHashParts, record.Path,
	); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertEnumerated(ctx, []EnumUpsert{record}); err != nil {
		t.Fatal(err)
	}
	var status string
	var missing uint32
	var phase1, phase2 int
	if err := db.db.QueryRowContext(ctx, `
		SELECT status, missing_mask, phase1_done, phase2_done FROM files WHERE path=?1`, record.Path,
	).Scan(&status, &missing, &phase1, &phase2); err != nil {
		t.Fatal(err)
	}
	if status != proto.StatusPartial || missing != proto.FieldPHashParts || phase1 != 1 || phase2 != 0 {
		t.Fatalf("phase-two state = %q/%#x/%d/%d, want partial/%#x/1/0",
			status, missing, phase1, phase2, proto.FieldPHashParts)
	}
}

func assertStageOneFileState(
	t *testing.T,
	db *DB,
	path string,
	wantStatus string,
	wantMissing uint32,
	wantDone bool,
	wantSHA bool,
) {
	t.Helper()
	var status string
	var missing uint32
	var done int
	var sha sql.NullString
	if err := db.db.QueryRow(`
		SELECT status, missing_mask, phase1_done, sha512 FROM files WHERE path=?1`, path,
	).Scan(&status, &missing, &done, &sha); err != nil {
		t.Fatal(err)
	}
	if status != wantStatus || missing != wantMissing || done != boolToInt(wantDone) || sha.Valid != wantSHA {
		t.Fatalf("file state = status:%q missing:%#x done:%d sha:%#v", status, missing, done, sha)
	}
}

func TestApplyHashResultsPreservesRetryAndDeduplicatesSyncQueue(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	recs := []EnumUpsert{
		{MachineID: "machine-a", DiskNo: 1, Path: `D:\ok.txt`, MissingBase: proto.FieldSHA512},
		{MachineID: "machine-a", DiskNo: 1, Path: `D:\bad.jpg`, MissingBase: proto.FieldSHA512 | proto.FieldPDQ256},
	}
	if err := db.UpsertEnumerated(ctx, recs); err != nil {
		t.Fatal(err)
	}
	results := []HashResult{
		{Path: recs[0].Path, SHA512: "okhash"},
		{Path: recs[1].Path, Err: "access denied"},
	}
	if err := db.ApplyHashResults(ctx, "machine-a", results); err != nil {
		t.Fatal(err)
	}
	if err := db.ApplyHashResults(ctx, "machine-a", results); err != nil {
		t.Fatal(err)
	}

	var status string
	var missing, phase1 int
	if err := db.db.QueryRowContext(ctx,
		`SELECT status, missing_mask, phase1_done FROM files WHERE path=?`,
		recs[0].Path).Scan(&status, &missing, &phase1); err != nil {
		t.Fatal(err)
	}
	if status != proto.StatusDone || missing != 0 || phase1 != 1 {
		t.Fatalf("successful row = status:%s missing:%d phase1:%d", status, missing, phase1)
	}
	var errorText string
	if err := db.db.QueryRowContext(ctx,
		`SELECT status, missing_mask, error FROM files WHERE path=?`,
		recs[1].Path).Scan(&status, &missing, &errorText); err != nil {
		t.Fatal(err)
	}
	if status != proto.StatusFailed || missing&int(proto.FieldSHA512) == 0 || errorText != "access denied" {
		t.Fatalf("failed row = status:%s missing:%d error:%s", status, missing, errorText)
	}
	var queueCount int
	if err := db.db.QueryRowContext(ctx, `SELECT count(*) FROM sync_queue`).Scan(&queueCount); err != nil {
		t.Fatal(err)
	}
	if queueCount != 2 {
		t.Fatalf("sync_queue rows = %d, want 2", queueCount)
	}
}

func TestPendingSnapshotOrdersByDiskAndPathAndExcludesDeleted(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	recs := []EnumUpsert{
		{MachineID: "machine-a", DiskNo: 2, Path: `E:\z.bin`, MissingBase: 1},
		{MachineID: "machine-a", DiskNo: 1, Path: `D:\b.bin`, MissingBase: 1},
		{MachineID: "machine-a", DiskNo: 1, Path: `D:\a.bin`, MissingBase: 1},
	}
	if err := db.UpsertEnumerated(ctx, recs); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE files SET status='deleted' WHERE path=?`, recs[1].Path); err != nil {
		t.Fatal(err)
	}
	got, err := db.PendingSnapshot(ctx, "machine-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got[1]) != 1 || got[1][0].Path != recs[2].Path ||
		len(got[2]) != 1 || got[2][0].Path != recs[0].Path {
		t.Fatalf("PendingSnapshot = %#v", got)
	}
}

func TestPendingSnapshotIncludesPartialPhase1RowsAndKnownSHA(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	known := strings.Repeat("ab", 64)
	recs := []EnumUpsert{
		{MachineID: "machine-a", DiskNo: 1, Path: `D:\known.jpg`, Size: 10, MTime: 20, MissingBase: proto.FieldSHA512 | proto.FieldPDQ256},
		{MachineID: "machine-a", DiskNo: 1, Path: `D:\zero.jpg`, MissingBase: 0},
	}
	if err := db.UpsertEnumerated(ctx, recs); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `
		UPDATE files SET sha512=?1, missing_mask=?2, status='partial'
		WHERE machine_id='machine-a' AND path=?3`,
		known, proto.FieldPDQ256, recs[0].Path,
	); err != nil {
		t.Fatal(err)
	}
	got, err := db.PendingSnapshot(ctx, "machine-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got[1]) != 1 {
		t.Fatalf("PendingSnapshot = %#v, want one incomplete row", got)
	}
	file := got[1][0]
	if file.Path != recs[0].Path || file.MissingMask != proto.FieldPDQ256 ||
		file.SHA512 == nil || *file.SHA512 != known {
		t.Fatalf("pending file = %#v", file)
	}
}

func assertPendingSHA(t *testing.T, db *DB, path string) {
	t.Helper()
	var status string
	var missing int
	if err := db.db.QueryRow(`SELECT status, missing_mask FROM files WHERE path=?`, path).
		Scan(&status, &missing); err != nil {
		t.Fatal(err)
	}
	if status != proto.StatusPending || missing&int(proto.FieldSHA512) == 0 {
		t.Fatalf("row = status:%s missing:%d, want pending with SHA bit", status, missing)
	}
}

func TestMarkDeletedUpdatesRowsQueuesAndLeavesFeaturesUntouched(t *testing.T) {
	// Catches a delete implementation that updates only files, resets pending
	// generations, or removes feature rows while applying local delete state.
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	records := []EnumUpsert{
		{MachineID: "machine-a", DiskNo: 1, Path: `D:\one.jpg`, MissingBase: proto.FieldSHA512},
		{MachineID: "machine-a", DiskNo: 1, Path: `D:\two.jpg`, MissingBase: proto.FieldSHA512},
		{MachineID: "machine-a", DiskNo: 1, Path: `D:\three.jpg`, MissingBase: proto.FieldSHA512},
	}
	if err := db.UpsertEnumerated(ctx, records); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `
		UPDATE files SET status='failed', error='old failure' WHERE machine_id='machine-a';
		INSERT INTO image_features(sha512) VALUES ('image-feature');
		INSERT INTO video_features(sha512) VALUES ('video-feature');
		INSERT INTO video_frames(sha512, frame_idx) VALUES ('video-feature', 0);`); err != nil {
		t.Fatal(err)
	}
	ids := make(map[string]int64)
	rows, err := db.db.QueryContext(ctx, `SELECT id, path FROM files WHERE machine_id='machine-a'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		var path string
		if err := rows.Scan(&id, &path); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		ids[path] = id
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `
		INSERT INTO sync_queue(table_name, row_pk, synced, enqueued_at, generation) VALUES
		('files', ?1, 1, 11, 4), ('files', ?2, 0, 12, 7);`,
		ids[records[0].Path], ids[records[1].Path]); err != nil {
		t.Fatal(err)
	}

	paths := []string{records[0].Path, records[1].Path, records[2].Path}
	if err := db.MarkDeleted(ctx, "machine-a", paths); err != nil {
		t.Fatalf("MarkDeleted: %v", err)
	}
	for _, path := range paths {
		var status string
		var rowErr sql.NullString
		if err := db.db.QueryRowContext(ctx,
			`SELECT status, error FROM files WHERE machine_id=?1 AND path=?2`, "machine-a", path,
		).Scan(&status, &rowErr); err != nil {
			t.Fatal(err)
		}
		if status != "deleted" || rowErr.Valid {
			t.Fatalf("file %q = status:%q error:%#v, want deleted with NULL error", path, status, rowErr)
		}
	}
	for _, want := range []struct {
		path       string
		generation int64
	}{
		{records[0].Path, 5}, {records[1].Path, 8}, {records[2].Path, 1},
	} {
		var synced, generation int64
		if err := db.db.QueryRowContext(ctx, `
			SELECT synced, generation FROM sync_queue WHERE table_name='files' AND row_pk=?1`,
			ids[want.path],
		).Scan(&synced, &generation); err != nil {
			t.Fatal(err)
		}
		if synced != 0 || generation != want.generation {
			t.Fatalf("queue %q = synced:%d generation:%d, want 0/%d", want.path, synced, generation, want.generation)
		}
	}
	for _, table := range []string{"image_features", "video_features", "video_frames"} {
		var count int
		if err := db.db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("%s rows = %d, want 1", table, count)
		}
	}
}

func TestMarkDeletedRejectsInvalidInputBeforeChangingState(t *testing.T) {
	// Catches validation that opens a partial transaction or accepts byte-exact
	// duplicate paths, which would accidentally enqueue the same row twice.
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	record := EnumUpsert{MachineID: "machine-a", DiskNo: 1, Path: `D:\keep.jpg`, MissingBase: proto.FieldSHA512}
	if err := db.UpsertEnumerated(ctx, []EnumUpsert{record}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE files SET status='failed', error='keep me' WHERE path=?`, record.Path); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkDeleted(ctx, "", []string{record.Path}); err == nil {
		t.Fatal("empty machineID error = nil")
	}
	if err := db.MarkDeleted(ctx, "machine-a", []string{""}); err == nil {
		t.Fatal("empty path error = nil")
	}
	if err := db.MarkDeleted(ctx, "machine-a", []string{record.Path, record.Path}); err == nil {
		t.Fatal("duplicate path error = nil")
	}
	if err := db.MarkDeleted(ctx, "machine-a", nil); err != nil {
		t.Fatalf("empty paths: %v", err)
	}
	var status, rowErr string
	var queueCount int
	if err := db.db.QueryRowContext(ctx, `SELECT status, error FROM files WHERE path=?`, record.Path).Scan(&status, &rowErr); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRowContext(ctx, `SELECT count(*) FROM sync_queue`).Scan(&queueCount); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || rowErr != "keep me" || queueCount != 0 {
		t.Fatalf("state after rejected input = status:%q error:%q queue:%d", status, rowErr, queueCount)
	}
}

func TestMarkDeletedRollsBackWholeBatchOnMachineOrPathMismatch(t *testing.T) {
	// Catches returning an error after an earlier file and queue change have
	// already committed inside the same requested batch.
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	record := EnumUpsert{MachineID: "machine-a", DiskNo: 1, Path: `D:\first.jpg`, MissingBase: proto.FieldSHA512}
	if err := db.UpsertEnumerated(ctx, []EnumUpsert{record}); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkDeleted(ctx, "machine-a", []string{record.Path, `D:\missing.jpg`}); err == nil {
		t.Fatal("missing path error = nil")
	}
	if err := db.MarkDeleted(ctx, "machine-b", []string{record.Path}); err == nil {
		t.Fatal("wrong machine error = nil")
	}
	var status string
	var queueCount int
	if err := db.db.QueryRowContext(ctx, `SELECT status FROM files WHERE path=?`, record.Path).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRowContext(ctx, `SELECT count(*) FROM sync_queue`).Scan(&queueCount); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || queueCount != 0 {
		t.Fatalf("rolled-back state = status:%q queue:%d, want pending/0", status, queueCount)
	}
}

func TestUpsertEnumeratedResurrectsActuallyReappearedDeletedPath(t *testing.T) {
	// Catches preserving the same-size/mtime hash-complete branch after a
	// physical re-enumeration, leaving a reappeared file permanently deleted.
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	record := EnumUpsert{MachineID: "machine-a", DiskNo: 1, Path: `D:\return.jpg`, Size: 10, MTime: 20, MissingBase: proto.FieldSHA512 | proto.FieldPDQ256}
	if err := db.UpsertEnumerated(ctx, []EnumUpsert{record}); err != nil {
		t.Fatal(err)
	}
	if err := db.ApplyHashResults(ctx, "machine-a", []HashResult{{Path: record.Path, SHA512: "old-hash"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkDeleted(ctx, "machine-a", []string{record.Path}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertEnumerated(ctx, []EnumUpsert{record}); err != nil {
		t.Fatal(err)
	}
	var status string
	var sha sql.NullString
	var phase1, phase2, missing int
	if err := db.db.QueryRowContext(ctx, `
		SELECT status, sha512, phase1_done, phase2_done, missing_mask FROM files WHERE path=?`, record.Path,
	).Scan(&status, &sha, &phase1, &phase2, &missing); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || sha.Valid || phase1 != 0 || phase2 != 0 || missing != int(record.MissingBase) {
		t.Fatalf("resurrected row = status:%q sha:%#v phase1:%d phase2:%d missing:%d", status, sha, phase1, phase2, missing)
	}
}

func TestMarkDeletedConcurrentCallsIncrementGenerationExactly(t *testing.T) {
	// Catches a read-modify-write queue update that loses generation increments
	// when independent SQLite connections concurrently report physical delete
	// success against the same database.
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "agent.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close setup database: %v", err)
		}
	})
	record := EnumUpsert{MachineID: "machine-a", DiskNo: 1, Path: `D:\concurrent.jpg`, MissingBase: proto.FieldSHA512}
	if err := db.UpsertEnumerated(ctx, []EnumUpsert{record}); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := db.db.QueryRowContext(ctx, `SELECT id FROM files WHERE path=?`, record.Path).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `
		INSERT INTO sync_queue(table_name, row_pk, synced, enqueued_at, generation) VALUES ('files', ?1, 1, 0, 1)`, id,
	); err != nil {
		t.Fatal(err)
	}
	const calls = 12
	workers := make([]*DB, calls)
	for index := range workers {
		worker, err := Open(dbPath)
		if err != nil {
			t.Fatalf("open worker database %d: %v", index, err)
		}
		workers[index] = worker
		t.Cleanup(func() {
			if err := worker.Close(); err != nil {
				t.Errorf("close worker database %d: %v", index, err)
			}
		})
	}
	start := make(chan struct{})
	errs := make(chan error, calls)
	var wg sync.WaitGroup
	for _, worker := range workers {
		wg.Add(1)
		go func(worker *DB) {
			defer wg.Done()
			<-start
			errs <- worker.MarkDeleted(ctx, "machine-a", []string{record.Path})
		}(worker)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent MarkDeleted: %v", err)
		}
	}
	var synced, generation int64
	if err := db.db.QueryRowContext(ctx, `
		SELECT synced, generation FROM sync_queue WHERE table_name='files' AND row_pk=?`, id,
	).Scan(&synced, &generation); err != nil {
		t.Fatal(err)
	}
	if synced != 0 || generation != 13 {
		t.Fatalf("concurrent queue = synced:%d generation:%d, want 0/13", synced, generation)
	}
}
