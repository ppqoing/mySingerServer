package store

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"

	"dedup/internal/proto"
)

// TestSaveAnalysisMergedAtomic catches an implementation that persists phase-1
// and phase-2 results through separate commits.  A caller must observe the
// image feature and final file state together, along with both queue entries.
func TestSaveAnalysisMergedAtomic(t *testing.T) {
	db := openAnalysisTestStore(t)
	ctx := context.Background()
	sha := analysisTestSHA(0x11)
	shaText := hex.EncodeToString(sha)
	path := `D:\analysis\merged.jpg`
	fileID := seedAnalysisFile(t, db, path, sha,
		proto.FieldPDQ256|proto.FieldPHashParts|proto.FieldSobelHist)
	pHash, sobel := phase2TestBlobs(t, 11)

	state, err := db.SaveAnalysis(ctx, AnalysisResult{
		MachineID: "m", Path: path, Kind: MediaImage, Size: 10, MTime: 20,
		SHA512:          sha,
		RequestedFields: proto.FieldPDQ256 | proto.FieldPHashParts | proto.FieldSobelHist,
		FieldsDone:      proto.FieldPDQ256 | proto.FieldPHashParts | proto.FieldSobelHist,
		PDQ:             bytes.Repeat([]byte{0x51}, 32), Quality: 81, Width: 640, Height: 480,
		PHashParts: pHash, SobelHist: sobel,
	})
	if err != nil {
		t.Fatalf("SaveAnalysis: %v", err)
	}
	if state.FieldsPresent != proto.FieldPDQ256|proto.FieldPHashParts|proto.FieldSobelHist ||
		state.MissingFields != 0 || state.FramesPresent != 0 || state.MissingFrames != 0 {
		t.Fatalf("committed state = %#v", state)
	}

	var pdq, gotPHash, gotSobel []byte
	if err := db.db.QueryRowContext(ctx, `
		SELECT pdq256, phash_parts, sobel_hist FROM image_features WHERE sha512=?1`, shaText,
	).Scan(&pdq, &gotPHash, &gotSobel); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pdq, bytes.Repeat([]byte{0x51}, 32)) ||
		!bytes.Equal(gotPHash, pHash) || !bytes.Equal(gotSobel, sobel) {
		t.Fatalf("committed image feature pdq=%x phash=%x sobel=%x", pdq, gotPHash, gotSobel)
	}
	assertAnalysisFile(t, db, path, 0, true, true)
	assertAnalysisQueue(t, db, "files", fileID)
	assertAnalysisQueue(t, db, "image_features", shaText)
}

// TestSaveAnalysisStaleNoOp catches stale validation performed after feature
// or sync writes.  Size mismatch has precedence even if the payload is bad.
func TestSaveAnalysisStaleNoOp(t *testing.T) {
	db := openAnalysisTestStore(t)
	ctx := context.Background()
	sha := analysisTestSHA(0x21)
	path := `D:\analysis\stale.jpg`
	seedAnalysisFile(t, db, path, sha, proto.FieldPDQ256)

	_, err := db.SaveAnalysis(ctx, AnalysisResult{
		MachineID: "m", Path: path, Kind: MediaImage, Size: 11, MTime: 20,
		SHA512: sha, RequestedFields: proto.FieldPDQ256, FieldsDone: proto.FieldPDQ256,
		PDQ: []byte{1}, // Invalid too: ErrStale must win before payload validation/writes.
	})
	if !errors.Is(err, ErrStale) {
		t.Fatalf("SaveAnalysis error = %v, want ErrStale", err)
	}
	var features, queue int
	if err := db.db.QueryRowContext(ctx, `SELECT count(*) FROM image_features`).Scan(&features); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRowContext(ctx, `SELECT count(*) FROM sync_queue`).Scan(&queue); err != nil {
		t.Fatal(err)
	}
	if features != 0 || queue != 0 {
		t.Fatalf("stale result wrote image_features=%d sync_queue=%d", features, queue)
	}
	assertAnalysisFile(t, db, path, proto.FieldPDQ256, false, false)
}

// TestSaveAnalysisPartialFrames catches replacing failed or unrequested slots
// while applying a partial successful frame result.
func TestSaveAnalysisPartialFrames(t *testing.T) {
	db := openAnalysisTestStore(t)
	ctx := context.Background()
	sha := analysisTestSHA(0x31)
	shaText := hex.EncodeToString(sha)
	path := `D:\analysis\partial.mp4`
	seedAnalysisFile(t, db, path, sha, proto.FieldVideo6F)
	old := phase2TestFrame(t, 0, 1)
	if _, err := db.db.ExecContext(ctx, `
		INSERT INTO video_frames(sha512, frame_idx, pdq256, phash_parts, sobel_hist)
		VALUES(?1, 0, ?2, ?3, ?4)`, shaText, old.PDQ256, old.PHashParts, old.SobelHist); err != nil {
		t.Fatal(err)
	}
	second := phase2TestFrame(t, 1, 2)
	state, err := db.SaveAnalysis(ctx, AnalysisResult{
		MachineID: "m", Path: path, Kind: MediaVideo, Size: 10, MTime: 20, SHA512: sha,
		RequestedFields: proto.FieldVideo6F, RequestedFrames: 0x03,
		Frames: []Phase2Frame{{FrameIdx: 0, Error: "decode failed"}, second},
	})
	if err != nil {
		t.Fatalf("SaveAnalysis: %v", err)
	}
	if state.FieldsPresent != 0 || state.MissingFields != proto.FieldVideo6F ||
		state.FramesPresent != 0x03 || state.MissingFrames != 0 {
		t.Fatalf("partial committed state = %#v", state)
	}
	var gotOld, gotSecond []byte
	if err := db.db.QueryRowContext(ctx, `SELECT pdq256 FROM video_frames WHERE sha512=?1 AND frame_idx=0`, shaText).Scan(&gotOld); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRowContext(ctx, `SELECT pdq256 FROM video_frames WHERE sha512=?1 AND frame_idx=1`, shaText).Scan(&gotSecond); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotOld, old.PDQ256) || !bytes.Equal(gotSecond, second.PDQ256) {
		t.Fatalf("frame persistence old=%x second=%x", gotOld, gotSecond)
	}
}

// TestSaveAnalysisRollback catches a queue failure that leaves any earlier
// feature, frame, or file update committed.
func TestSaveAnalysisRollback(t *testing.T) {
	db := openAnalysisTestStore(t)
	ctx := context.Background()
	sha := analysisTestSHA(0x41)
	shaText := hex.EncodeToString(sha)
	path := `D:\analysis\rollback.jpg`
	seedAnalysisFile(t, db, path, sha, proto.FieldPDQ256)
	if _, err := db.db.ExecContext(ctx, `
		CREATE TRIGGER analysis_test_fail_queue
		BEFORE INSERT ON sync_queue
		BEGIN SELECT RAISE(ABORT, 'injected sync queue failure'); END;`); err != nil {
		t.Fatal(err)
	}
	_, err := db.SaveAnalysis(ctx, AnalysisResult{
		MachineID: "m", Path: path, Kind: MediaImage, Size: 10, MTime: 20, SHA512: sha,
		RequestedFields: proto.FieldPDQ256, FieldsDone: proto.FieldPDQ256,
		PDQ: bytes.Repeat([]byte{0x41}, 32), Quality: 70, Width: 20, Height: 10,
	})
	if err == nil {
		t.Fatal("SaveAnalysis succeeded despite injected sync queue failure")
	}
	var features, queue int
	if err := db.db.QueryRowContext(ctx, `SELECT count(*) FROM image_features WHERE sha512=?1`, shaText).Scan(&features); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRowContext(ctx, `SELECT count(*) FROM sync_queue`).Scan(&queue); err != nil {
		t.Fatal(err)
	}
	if features != 0 || queue != 0 {
		t.Fatalf("rollback left image_features=%d sync_queue=%d", features, queue)
	}
	assertAnalysisFile(t, db, path, proto.FieldPDQ256, false, false)
}

func openAnalysisTestStore(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "analysis.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedAnalysisFile(t *testing.T, db *DB, path string, sha []byte, missing uint32) int64 {
	t.Helper()
	ctx := context.Background()
	if err := db.UpsertEnumerated(ctx, []EnumUpsert{{MachineID: "m", DiskNo: 1, Path: path, Size: 10, MTime: 20, MissingBase: missing}}); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := db.db.QueryRowContext(ctx, `
		UPDATE files SET sha512=?1, status='partial', missing_mask=?2 WHERE machine_id='m' AND path=?3
		RETURNING id`, hex.EncodeToString(sha), missing, path).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func assertAnalysisFile(t *testing.T, db *DB, path string, wantMissing uint32, wantPhase1, wantPhase2 bool) {
	t.Helper()
	var missing uint32
	var phase1, phase2 int
	if err := db.db.QueryRowContext(context.Background(), `
		SELECT missing_mask, phase1_done, phase2_done FROM files WHERE machine_id='m' AND path=?1`, path,
	).Scan(&missing, &phase1, &phase2); err != nil {
		t.Fatal(err)
	}
	if missing != wantMissing || phase1 != boolToInt(wantPhase1) || phase2 != boolToInt(wantPhase2) {
		t.Fatalf("file state missing=%#x phase1=%d phase2=%d, want %#x/%d/%d", missing, phase1, phase2, wantMissing, boolToInt(wantPhase1), boolToInt(wantPhase2))
	}
}

func assertAnalysisQueue(t *testing.T, db *DB, table string, key any) {
	t.Helper()
	var generation int
	if err := db.db.QueryRowContext(context.Background(), `
		SELECT generation FROM sync_queue WHERE table_name=?1 AND row_pk=CAST(?2 AS TEXT)`, table, key,
	).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if generation != 1 {
		t.Fatalf("sync_queue %s/%v generation=%d, want 1", table, key, generation)
	}
}

func analysisTestSHA(seed byte) []byte {
	sha := make([]byte, 64)
	for index := range sha {
		sha[index] = seed + byte(index)
	}
	return sha
}
