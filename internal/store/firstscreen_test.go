package store

import (
	"context"
	"encoding/hex"
	"testing"

	"dedup/internal/firstscreen"
)

func TestDeletedExcludedFromCandidateSourceWhileHistoricalGenerationRemains(t *testing.T) {
	db := openLocalTestDB(t)
	ctx := context.Background()
	sha := firstscreenTestSHA(0x41)
	shaText := hex.EncodeToString(sha[:])
	for _, row := range []struct{ path, status string }{{`D:\active.jpg`, "done"}, {`D:\deleted.jpg`, "deleted"}} {
		if _, err := db.db.Exec(`INSERT INTO files(machine_id,path,sha512,status) VALUES ('machine-a',?,?,?)`, row.path, shaText, row.status); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.db.Exec(`INSERT INTO image_features(sha512,width,height,pdq256,pdq_quality) VALUES (?,100,100,?,80)`, shaText, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	var active []firstscreen.File
	if err := db.StreamActiveFiles(ctx, "machine-a", func(file firstscreen.File) error {
		active = append(active, file)
		return nil
	}); err != nil {
		t.Fatalf("StreamActiveFiles: %v", err)
	}
	if len(active) != 1 || active[0].Path != `D:\active.jpg` {
		t.Fatalf("active files = %#v, want only active row", active)
	}
	features, err := db.LoadImageFeatures(ctx, []string{shaText})
	if err != nil {
		t.Fatalf("LoadImageFeatures: %v", err)
	}
	if len(features) != 1 {
		t.Fatalf("image features = %#v, want active SHA feature", features)
	}
	// A prior generation remains independently addressable after deletion.
	createAnalysisTask(t, db, "history", "machine-a")
	run, err := db.BeginLocalAnalysis(ctx, "machine-a", "history")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceStageOne(ctx, run.RunID, firstscreen.Result{}); err != nil {
		t.Fatalf("ReplaceStageOne: %v", err)
	}
	var pairs int
	if err := db.db.QueryRow(`SELECT count(*) FROM local_pair_scores WHERE run_id=?`, run.RunID).Scan(&pairs); err != nil {
		t.Fatal(err)
	}
	if pairs != 0 {
		t.Fatalf("historical generation pair count = %d, want zero but queryable", pairs)
	}
}

func firstscreenTestSHA(value byte) [64]byte {
	var sha [64]byte
	sha[0] = value
	return sha
}
