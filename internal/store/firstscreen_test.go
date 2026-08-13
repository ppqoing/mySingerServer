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

func TestLocalFeatureLoadersKeepLegacyFeaturesEligibleLikePostgres(t *testing.T) {
	db := openLocalTestDB(t)
	ctx := context.Background()
	imageSHA := firstscreenTestSHA(0x51)
	videoSHA := firstscreenTestSHA(0x52)
	imageText := hex.EncodeToString(imageSHA[:])
	videoText := hex.EncodeToString(videoSHA[:])
	pdq := make([]byte, 32)
	pdq[31] = 1
	if _, err := db.db.Exec(`
		INSERT INTO image_features(sha512,width,height,pdq256,pdq_quality)
		VALUES (?,0,0,?,80)`, imageText, pdq); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`
		INSERT INTO video_features(sha512,duration_ms,thumb_pdq256)
		VALUES (?,1000,?)`, videoText, pdq); err != nil {
		t.Fatal(err)
	}
	images, err := db.LoadImageFeatures(ctx, []string{imageText})
	if err != nil {
		t.Fatalf("LoadImageFeatures: %v", err)
	}
	if got, ok := images[imageText]; !ok || got.Width != 0 || got.Height != 0 || got.Quality != 80 {
		t.Fatalf("legacy image = %#v, present=%t; want zero dimensions retained", got, ok)
	}
	videos, err := db.LoadVideoFeatures(ctx, []string{videoText})
	if err != nil {
		t.Fatalf("LoadVideoFeatures: %v", err)
	}
	if got, ok := videos[videoText]; !ok || got.DurationMs != 1000 || got.ThumbQuality != 0 {
		t.Fatalf("legacy video = %#v, present=%t; want duration and PDQ retained", got, ok)
	}
}

func TestLocalFeatureLoadersBatchMoreThanTwoSafeChunks(t *testing.T) {
	db := openLocalTestDB(t)
	ctx := context.Background()
	const inputCount = 33000 // exceeds two 500-item chunks and SQLite's common variable limit.
	shas := make([]string, inputCount)
	for index := range shas {
		sha := largeBatchSHA(index)
		shas[index] = hex.EncodeToString(sha[:])
	}
	pdq := make([]byte, 32)
	pdq[0] = 1
	for _, index := range []int{0, 500, inputCount - 1} {
		if _, err := db.db.Exec(`
			INSERT INTO image_features(sha512,width,height,pdq256,pdq_quality)
			VALUES (?,100,100,?,80)`, shas[index], pdq); err != nil {
			t.Fatal(err)
		}
	}
	features, err := db.LoadImageFeatures(ctx, shas)
	if err != nil {
		t.Fatalf("LoadImageFeatures with %d SHA values: %v", inputCount, err)
	}
	if len(features) != 3 {
		t.Fatalf("loaded features = %d, want all three seeded values", len(features))
	}
}

func TestNewGenerationExcludesDeletedFilesWithoutChangingPublishedHistoryOrCurrent(t *testing.T) {
	db := openLocalTestDB(t)
	ctx := context.Background()
	shaExact := firstscreenTestSHA(0x61)
	shaLeft := firstscreenTestSHA(0x62)
	shaRight := firstscreenTestSHA(0x63)
	old := createStageOneRun(t, db, "old")
	files := []firstscreen.File{
		insertFirstscreenFile(t, db, "machine-a", "old-exact-a", shaExact),
		insertFirstscreenFile(t, db, "machine-a", "old-exact-b", shaExact),
		insertFirstscreenFile(t, db, "machine-a", "old-left", shaLeft),
		insertFirstscreenFile(t, db, "machine-a", "old-right", shaRight),
	}
	oldResult := firstscreen.Result{
		Files:          files,
		ExactGroups:    []firstscreen.ExactGroup{{SHA512: shaExact, Members: []firstscreen.FileRef{files[0].FileRef, files[1].FileRef}}},
		CandidatePairs: []firstscreen.CandidatePair{{Kind: firstscreen.KindImageCandidate, ShaA: shaLeft, ShaB: shaRight, Hamming: 1, QualityA: 80, QualityB: 81}},
	}
	if err := db.ReplaceStageOne(ctx, old.RunID, oldResult); err != nil {
		t.Fatalf("write published source run: %v", err)
	}
	if err := db.CompleteLocalAnalysis(ctx, old.RunID); err != nil {
		t.Fatal(err)
	}
	if err := db.PublishLocalAnalysis(ctx, old.RunID); err != nil {
		t.Fatal(err)
	}
	before := stageOneCounts(t, db, old.RunID)
	if before != (stageOneCount{groups: 2, members: 4, pairs: 1}) {
		t.Fatalf("published history = %#v, want actual group/member/pair content", before)
	}
	if err := db.MarkDeleted(ctx, "machine-a", []string{files[0].Path, files[1].Path}); err != nil {
		t.Fatal(err)
	}
	newRun := createStageOneRun(t, db, "new")
	result, err := firstscreen.NewCandidateAnalyzer(db, db, firstscreen.DefaultConfig(), nil).Run(ctx, "machine-a", newRun.RunID)
	if err != nil {
		t.Fatalf("run new generation: %v", err)
	}
	if len(result.Files) != 2 || len(result.ExactGroups) != 0 {
		t.Fatalf("new result = %#v, want deleted exact members excluded", result)
	}
	if got := stageOneCounts(t, db, newRun.RunID); got != (stageOneCount{}) {
		t.Fatalf("new generation content = %#v, want no candidate records for deleted exact pair", got)
	}
	if got := stageOneCounts(t, db, old.RunID); got != before {
		t.Fatalf("published historical content changed after deletion = %#v, want %#v", got, before)
	}
	current, err := db.CurrentLocalAnalysis(ctx, "machine-a")
	if err != nil {
		t.Fatal(err)
	}
	if current.RunID != old.RunID || current.Generation != old.Generation {
		t.Fatalf("current = %#v, want unchanged published run %#v", current, old)
	}
}

func TestReplaceStageOneRollsBackExistingBuildingSnapshotOnInsertFailure(t *testing.T) {
	db := openLocalTestDB(t)
	ctx := context.Background()
	run := createStageOneRun(t, db, "rollback")
	leftSHA, rightSHA := firstscreenTestSHA(0x71), firstscreenTestSHA(0x72)
	left := insertFirstscreenFile(t, db, "machine-a", "rollback-left", leftSHA)
	right := insertFirstscreenFile(t, db, "machine-a", "rollback-right", rightSHA)
	previous := firstscreen.Result{Files: []firstscreen.File{left, right}, CandidatePairs: []firstscreen.CandidatePair{{Kind: firstscreen.KindImageCandidate, ShaA: leftSHA, ShaB: rightSHA, Hamming: 1, QualityA: 80, QualityB: 81}}}
	if err := db.ReplaceStageOne(ctx, run.RunID, previous); err != nil {
		t.Fatalf("seed building snapshot: %v", err)
	}
	before := stageOneCounts(t, db, run.RunID)
	if _, err := db.db.Exec(`
		CREATE TRIGGER fail_pair_replacement
		BEFORE INSERT ON local_pair_scores
		BEGIN SELECT RAISE(ABORT, 'injected pair failure'); END;`); err != nil {
		t.Fatal(err)
	}
	err := db.ReplaceStageOne(ctx, run.RunID, previous)
	if err == nil {
		t.Fatal("ReplaceStageOne error = nil, want injected insert failure")
	}
	if got := stageOneCounts(t, db, run.RunID); got != before {
		t.Fatalf("building snapshot after failed replacement = %#v, want rollback to %#v", got, before)
	}
}

type stageOneCount struct{ groups, members, pairs int }

func stageOneCounts(t *testing.T, db *DB, runID string) stageOneCount {
	t.Helper()
	var count stageOneCount
	for _, entry := range []struct {
		query string
		out   *int
	}{
		{`SELECT count(*) FROM local_dup_groups WHERE run_id=?`, &count.groups},
		{`SELECT count(*) FROM local_dup_members WHERE run_id=?`, &count.members},
		{`SELECT count(*) FROM local_pair_scores WHERE run_id=?`, &count.pairs},
	} {
		if err := db.db.QueryRow(entry.query, runID).Scan(entry.out); err != nil {
			t.Fatal(err)
		}
	}
	return count
}

func createStageOneRun(t *testing.T, db *DB, suffix string) LocalAnalysisRun {
	t.Helper()
	taskID := "stage-one-" + suffix
	createAnalysisTask(t, db, taskID, "machine-a")
	run, err := db.BeginLocalAnalysis(context.Background(), "machine-a", taskID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func insertFirstscreenFile(t *testing.T, db *DB, machineID, suffix string, sha [64]byte) firstscreen.File {
	t.Helper()
	result, err := db.db.Exec(`INSERT INTO files(machine_id,path,sha512,status) VALUES (?,?,?,'done')`, machineID, `D:\`+suffix, hex.EncodeToString(sha[:]))
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return firstscreen.File{FileRef: firstscreen.FileRef{ID: id, MachineID: machineID, Path: `D:\` + suffix}, SHA512: sha}
}

func largeBatchSHA(index int) [64]byte {
	var sha [64]byte
	for offset := 0; offset < 8; offset++ {
		sha[offset] = byte(index >> (offset * 8))
	}
	return sha
}

func firstscreenTestSHA(value byte) [64]byte {
	var sha [64]byte
	sha[0] = value
	return sha
}
