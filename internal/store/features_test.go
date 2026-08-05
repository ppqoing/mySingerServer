package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"testing"

	"dedup/internal/proto"
)

func TestSavePhase1Idempotent(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.UpsertEnumerated(ctx, []EnumUpsert{{
		MachineID: "m", DiskNo: 1, Path: `D:\image.jpg`, Size: 10, MTime: 20, MissingBase: 3,
	}}); err != nil {
		t.Fatal(err)
	}
	result := Phase1Result{
		MachineID: "m", Path: `D:\image.jpg`, Kind: MediaImage, SHA512: phase1TestSHA(),
		FieldsDone: 3, PDQ: []byte{1, 2}, Quality: 91, Width: 640, Height: 480,
	}
	if err := db.SavePhase1(ctx, result); err != nil {
		t.Fatalf("first SavePhase1: %v", err)
	}
	if err := db.SavePhase1(ctx, result); err != nil {
		t.Fatalf("second SavePhase1: %v", err)
	}

	var featureCount, phase1, missing int
	var sha, status string
	if err := db.db.QueryRowContext(ctx, `
		SELECT (SELECT count(*) FROM image_features), sha512, status, missing_mask, phase1_done
		FROM files WHERE machine_id='m' AND path=?`, result.Path,
	).Scan(&featureCount, &sha, &status, &missing, &phase1); err != nil {
		t.Fatal(err)
	}
	if featureCount != 1 || sha != "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f" ||
		status != "done" || missing != 0 || phase1 != 1 {
		t.Fatalf("saved row = features:%d sha:%q status:%q missing:%d phase1:%d", featureCount, sha, status, missing, phase1)
	}
	var filesGeneration, featureGeneration int
	if err := db.db.QueryRowContext(ctx, `SELECT generation FROM sync_queue WHERE table_name='files' AND row_pk='1'`).Scan(&filesGeneration); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRowContext(ctx, `SELECT generation FROM sync_queue WHERE table_name='image_features' AND row_pk=?`, sha).Scan(&featureGeneration); err != nil {
		t.Fatal(err)
	}
	if filesGeneration != 2 || featureGeneration != 2 {
		t.Fatalf("queue generations = files:%d feature:%d, want 2:2", filesGeneration, featureGeneration)
	}
}

func TestSavePhase1RejectsInvalidSHA(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, tc := range []struct {
		name string
		sha  []byte
	}{
		{name: "nil", sha: nil},
		{name: "63 bytes", sha: make([]byte, 63)},
		{name: "65 bytes", sha: make([]byte, 65)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := `D:\invalid-` + tc.name
			if err := db.UpsertEnumerated(ctx, []EnumUpsert{{
				MachineID: "m", Path: path, Size: 10, MTime: 20, MissingBase: 1,
			}}); err != nil {
				t.Fatal(err)
			}
			if err := db.SavePhase1(ctx, Phase1Result{
				MachineID: "m", Path: path, Kind: MediaImage, SHA512: tc.sha,
			}); err == nil {
				t.Fatalf("SavePhase1(%d byte SHA) error = nil", len(tc.sha))
			}
			if _, err := db.LookupImage(ctx, tc.sha); err == nil {
				t.Fatalf("LookupImage(%d byte SHA) error = nil", len(tc.sha))
			}
			if _, err := db.LookupVideo(ctx, tc.sha); err == nil {
				t.Fatalf("LookupVideo(%d byte SHA) error = nil", len(tc.sha))
			}
		})
	}
}

func TestSavePhase1PersistsPreSHAFailureWithoutSyntheticStoreError(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	path := `D:\locked.jpg`
	if err := db.UpsertEnumerated(ctx, []EnumUpsert{{
		MachineID: "m", Path: path, Size: 10, MTime: 20,
		MissingBase: proto.FieldSHA512 | proto.FieldPDQ256,
	}}); err != nil {
		t.Fatal(err)
	}
	openError := "sharing violation"
	if err := db.SavePhase1(ctx, Phase1Result{
		MachineID: "m", Path: path, Kind: MediaImage,
		Errors: []FieldError{{
			Field: proto.FieldSHA512 | proto.FieldPDQ256,
			Stage: "open", Msg: openError,
		}},
	}); err != nil {
		t.Fatalf("SavePhase1 pre-SHA failure: %v", err)
	}
	var status, storedError string
	var sha sql.NullString
	if err := db.db.QueryRowContext(ctx, `
		SELECT status, error, sha512 FROM files WHERE machine_id='m' AND path=?1`,
		path,
	).Scan(&status, &storedError, &sha); err != nil {
		t.Fatal(err)
	}
	if status != proto.StatusFailed || storedError != "open: "+openError || sha.Valid {
		t.Fatalf("pre-SHA row status=%q error=%q sha=%#v", status, storedError, sha)
	}
}

func TestSavePhase1PreSHAFailureWhitelist(t *testing.T) {
	duration := int64(1)
	quality := int32(1)
	tests := []struct {
		name   string
		mutate func(*Phase1Result)
		allow  bool
	}{
		{
			name: "stat SHA error",
			mutate: func(result *Phase1Result) {
				result.Errors = []FieldError{{Field: proto.FieldSHA512, Stage: "stat", Msg: "gone"}}
			},
			allow: true,
		},
		{
			name: "open error covers all requested image fields",
			mutate: func(result *Phase1Result) {
				result.Errors = []FieldError{{Field: proto.FieldSHA512 | proto.FieldPDQ256, Stage: "open", Msg: "locked"}}
			},
			allow: true,
		},
		{
			name: "multiple read and open SHA errors",
			mutate: func(result *Phase1Result) {
				result.Errors = []FieldError{
					{Field: proto.FieldSHA512, Stage: "read", Msg: "short read"},
					{Field: proto.FieldSHA512 | proto.FieldThumb, Stage: "open", Msg: "closed"},
				}
			},
			allow: true,
		},
		{
			name: "decode stage",
			mutate: func(result *Phase1Result) {
				result.Errors = []FieldError{{Field: proto.FieldSHA512 | proto.FieldPDQ256, Stage: "decode", Msg: "bad image"}}
			},
		},
		{
			name: "thumb stage",
			mutate: func(result *Phase1Result) {
				result.Errors = []FieldError{{Field: proto.FieldSHA512 | proto.FieldThumb, Stage: "thumb", Msg: "bad video"}}
			},
		},
		{
			name: "arbitrary stage",
			mutate: func(result *Phase1Result) {
				result.Errors = []FieldError{{Field: proto.FieldSHA512, Stage: "network", Msg: "bad"}}
			},
		},
		{
			name: "error does not cover SHA",
			mutate: func(result *Phase1Result) {
				result.Errors = []FieldError{{Field: proto.FieldPDQ256, Stage: "read", Msg: "bad"}}
			},
		},
		{
			name: "one of multiple errors does not cover SHA",
			mutate: func(result *Phase1Result) {
				result.Errors = []FieldError{
					{Field: proto.FieldSHA512, Stage: "read", Msg: "bad"},
					{Field: proto.FieldPDQ256, Stage: "read", Msg: "also bad"},
				}
			},
		},
		{
			name: "fields done",
			mutate: func(result *Phase1Result) {
				result.FieldsDone = proto.FieldPDQ256
				result.Errors = []FieldError{{Field: proto.FieldSHA512, Stage: "read", Msg: "bad"}}
			},
		},
		{
			name: "image feature payload",
			mutate: func(result *Phase1Result) {
				result.PDQ = []byte{1}
				result.Errors = []FieldError{{Field: proto.FieldSHA512, Stage: "read", Msg: "bad"}}
			},
		},
		{
			name: "video feature payload",
			mutate: func(result *Phase1Result) {
				result.DurationMS = &duration
				result.ThumbPath = `D:\thumb.jpg`
				result.ThumbPDQ = []byte{1}
				result.ThumbQuality = &quality
				result.Errors = []FieldError{{Field: proto.FieldSHA512, Stage: "read", Msg: "bad"}}
			},
		},
		{
			name:   "no errors",
			mutate: func(*Phase1Result) {},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db, err := Open(filepath.Join(t.TempDir(), "agent.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			path := `D:\pre-sha-` + tc.name
			if err := db.UpsertEnumerated(ctx, []EnumUpsert{{
				MachineID: "m", Path: path, Size: 10, MTime: 20,
				MissingBase: proto.FieldSHA512 | proto.FieldPDQ256,
			}}); err != nil {
				t.Fatal(err)
			}
			result := Phase1Result{MachineID: "m", Path: path, Kind: MediaImage}
			tc.mutate(&result)
			err = db.SavePhase1(ctx, result)
			if tc.allow && err != nil {
				t.Fatalf("SavePhase1 rejected whitelisted pre-SHA failure: %v", err)
			}
			if !tc.allow && err == nil {
				t.Fatal("SavePhase1 accepted non-whitelisted pre-SHA result")
			}
		})
	}
}

func TestLookupVideoAllowsPartialDurationOnlyRow(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sha := phase1TestSHA()
	shaText := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f"
	if _, err := db.db.ExecContext(ctx, `
		INSERT INTO video_features (sha512, duration_ms) VALUES (?1, 1234)`, shaText,
	); err != nil {
		t.Fatal(err)
	}
	feature, err := db.LookupVideo(ctx, sha)
	if err != nil {
		t.Fatalf("LookupVideo: %v", err)
	}
	if feature == nil || feature.DurationMS == nil || *feature.DurationMS != 1234 || feature.ThumbPath != "" || feature.ThumbPDQ != nil || feature.ThumbQuality != nil {
		t.Fatalf("LookupVideo partial feature = %#v", feature)
	}
}

func TestSavePhase1RetryFailurePreservesPartialStatus(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	shaText := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f"
	if err := db.UpsertEnumerated(ctx, []EnumUpsert{
		{MachineID: "m", Path: `D:\partial.jpg`, Size: 10, MTime: 20, MissingBase: 2},
		{MachineID: "m", Path: `D:\partial.mp4`, Size: 10, MTime: 20, MissingBase: 4},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `
		UPDATE files SET sha512=?1, status='partial' WHERE machine_id='m';
		INSERT INTO video_features (sha512, duration_ms) VALUES (?1, 1234);`, shaText,
	); err != nil {
		t.Fatal(err)
	}
	for _, result := range []Phase1Result{
		{
			MachineID: "m", Path: `D:\partial.jpg`, Kind: MediaImage, SHA512: phase1TestSHA(),
			Errors: []FieldError{{Field: 2, Stage: "pdq", Msg: "retry failed"}},
		},
		{
			MachineID: "m", Path: `D:\partial.mp4`, Kind: MediaVideo, SHA512: phase1TestSHA(),
			Errors: []FieldError{{Field: 4, Stage: "thumb", Msg: "retry failed"}},
		},
	} {
		if err := db.SavePhase1(ctx, result); err != nil {
			t.Fatalf("SavePhase1(%s): %v", result.Path, err)
		}
		var status string
		if err := db.db.QueryRowContext(ctx, `SELECT status FROM files WHERE path=?1`, result.Path).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "partial" {
			t.Fatalf("retry status for %s = %q, want partial", result.Path, status)
		}
	}
}

func TestSavePhase1PreservesPartialVideoFields(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.UpsertEnumerated(ctx, []EnumUpsert{{
		MachineID: "m", Path: `D:\video.mp4`, Size: 10, MTime: 20, MissingBase: 5,
	}}); err != nil {
		t.Fatal(err)
	}
	duration := int64(1234)
	if err := db.SavePhase1(ctx, Phase1Result{
		MachineID: "m", Path: `D:\video.mp4`, Kind: MediaVideo, SHA512: phase1TestSHA(),
		FieldsDone: 5, DurationMS: &duration,
	}); err != nil {
		t.Fatal(err)
	}
	var missing int
	if err := db.db.QueryRowContext(ctx, `SELECT missing_mask FROM files WHERE path=?`, `D:\video.mp4`).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if missing != 4 {
		t.Fatalf("duration-only missing_mask = %d, want 4", missing)
	}
	quality := int32(88)
	if err := db.SavePhase1(ctx, Phase1Result{
		MachineID: "m", Path: `D:\video.mp4`, Kind: MediaVideo, SHA512: phase1TestSHA(),
		FieldsDone: 4, ThumbPath: `D:\thumb.jpg`, ThumbPDQ: []byte{9}, ThumbQuality: &quality,
	}); err != nil {
		t.Fatal(err)
	}
	var savedDuration int64
	var savedPath string
	var savedPDQ []byte
	var savedQuality int32
	if err := db.db.QueryRowContext(ctx, `
		SELECT duration_ms, thumb_path, thumb_pdq256, thumb_quality FROM video_features`,
	).Scan(&savedDuration, &savedPath, &savedPDQ, &savedQuality); err != nil {
		t.Fatal(err)
	}
	if savedDuration != 1234 || savedPath != `D:\thumb.jpg` || len(savedPDQ) != 1 || savedPDQ[0] != 9 || savedQuality != 88 {
		t.Fatalf("saved video feature = duration:%d path:%q pdq:%v quality:%d", savedDuration, savedPath, savedPDQ, savedQuality)
	}
	if err := db.db.QueryRowContext(ctx, `SELECT missing_mask FROM files WHERE path=?`, `D:\video.mp4`).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if missing != 0 {
		t.Fatalf("completed video missing_mask = %d, want 0", missing)
	}
	if err := db.SavePhase1(ctx, Phase1Result{
		MachineID: "m", Path: `D:\video.mp4`, Kind: MediaVideo, SHA512: phase1TestSHA(),
		FieldsDone: 4, Errors: []FieldError{{Field: 4, Stage: "thumb", Msg: "retry"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRowContext(ctx, `
		SELECT duration_ms, thumb_path, thumb_pdq256, thumb_quality FROM video_features`,
	).Scan(&savedDuration, &savedPath, &savedPDQ, &savedQuality); err != nil {
		t.Fatal(err)
	}
	if savedDuration != 1234 || savedPath != `D:\thumb.jpg` || len(savedPDQ) != 1 || savedPDQ[0] != 9 || savedQuality != 88 {
		t.Fatalf("failed update erased video feature = duration:%d path:%q pdq:%v quality:%d", savedDuration, savedPath, savedPDQ, savedQuality)
	}
}

func TestMarkCrashPreservesMissingMask(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.UpsertEnumerated(ctx, []EnumUpsert{{
		MachineID: "m", Path: `D:\crash.mp4`, Size: 10, MTime: 20, MissingBase: 5,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkCrash(ctx, "m", `D:\crash.mp4`, "worker exited"); err != nil {
		t.Fatal(err)
	}
	var status, message string
	var missing, generation int
	if err := db.db.QueryRowContext(ctx, `
		SELECT status, error, missing_mask, (SELECT generation FROM sync_queue WHERE table_name='files' AND row_pk='1')
		FROM files WHERE machine_id='m' AND path=?`, `D:\crash.mp4`,
	).Scan(&status, &message, &missing, &generation); err != nil {
		t.Fatal(err)
	}
	if status != "crash" || message != "worker exited" || missing != 5 || generation != 1 {
		t.Fatalf("crash row = status:%q message:%q missing:%d generation:%d", status, message, missing, generation)
	}
}

func TestSavePhase1RollsBack(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.UpsertEnumerated(ctx, []EnumUpsert{{
		MachineID: "m", Path: `D:\rollback.jpg`, Size: 10, MTime: 20, MissingBase: 3,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `
		CREATE TRIGGER fail_phase1_queue BEFORE INSERT ON sync_queue
		BEGIN SELECT RAISE(FAIL, 'controlled queue failure'); END;`); err != nil {
		t.Fatal(err)
	}
	err = db.SavePhase1(ctx, Phase1Result{
		MachineID: "m", Path: `D:\rollback.jpg`, Kind: MediaImage, SHA512: phase1TestSHA(),
		FieldsDone: 3, PDQ: []byte{1}, Quality: 2, Width: 3, Height: 4,
	})
	if err == nil {
		t.Fatal("SavePhase1 error = nil, want controlled queue failure")
	}
	var sha sql.NullString
	var missing, featureCount, queueCount int
	if err := db.db.QueryRowContext(ctx, `
		SELECT sha512, missing_mask, (SELECT count(*) FROM image_features), (SELECT count(*) FROM sync_queue)
		FROM files WHERE machine_id='m' AND path=?`, `D:\rollback.jpg`,
	).Scan(&sha, &missing, &featureCount, &queueCount); err != nil {
		t.Fatal(err)
	}
	if sha.Valid || missing != 3 || featureCount != 0 || queueCount != 0 {
		t.Fatalf("rollback state = sha:%q valid:%t missing:%d features:%d queue:%d", sha.String, sha.Valid, missing, featureCount, queueCount)
	}
}

func TestSavePhase1MarksFeatureFailure(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.UpsertEnumerated(ctx, []EnumUpsert{{
		MachineID: "m", Path: `D:\failed.jpg`, Size: 10, MTime: 20, MissingBase: 3,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := db.SavePhase1(ctx, Phase1Result{
		MachineID: "m", Path: `D:\failed.jpg`, Kind: MediaImage, SHA512: phase1TestSHA(),
		Errors: []FieldError{{Field: 2, Stage: "pdq", Msg: "decode failed"}},
	}); err != nil {
		t.Fatal(err)
	}
	var status, message string
	if err := db.db.QueryRowContext(ctx, `SELECT status, error FROM files WHERE path=?`, `D:\failed.jpg`).Scan(&status, &message); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || message != "pdq: decode failed" {
		t.Fatalf("failed status = %q, %q", status, message)
	}
}

func TestPhase1MigrationIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`
		CREATE TABLE video_features (
			sha512 TEXT PRIMARY KEY,
			duration_ms INTEGER NOT NULL DEFAULT 0,
			thumb_path TEXT,
			thumb_pdq256 BLOB,
			thumb_quality INTEGER NOT NULL DEFAULT 0
		);`); err != nil {
		t.Fatal(err)
	}
	shaText := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f"
	if _, err := legacy.Exec(`
		INSERT INTO video_features (sha512, duration_ms, thumb_path, thumb_pdq256, thumb_quality)
		VALUES (?1, 1234, 'D:\thumb.jpg', X'0102', 88)`, shaText); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		db, err := Open(path)
		if err != nil {
			t.Fatalf("Open %d: %v", i, err)
		}
		var durationNotNull, qualityNotNull, dimensionColumns, userVersion int
		if err := db.db.QueryRow(`
			SELECT (SELECT "notnull" FROM pragma_table_info('video_features') WHERE name='duration_ms'),
			       (SELECT "notnull" FROM pragma_table_info('video_features') WHERE name='thumb_quality'),
			       (SELECT count(*) FROM pragma_table_info('video_features')
			        WHERE name IN ('thumb_width','thumb_height')),
			       (SELECT user_version FROM pragma_user_version)`,
		).Scan(&durationNotNull, &qualityNotNull, &dimensionColumns, &userVersion); err != nil {
			db.Close()
			t.Fatal(err)
		}
		if durationNotNull != 0 || qualityNotNull != 0 || dimensionColumns != 2 || userVersion != 3 {
			db.Close()
			t.Fatalf("video schema duration_notnull=%d quality_notnull=%d dimension_columns=%d user_version=%d, want 0/0/2/3",
				durationNotNull, qualityNotNull, dimensionColumns, userVersion)
		}
		var gotSHA, gotPath string
		var gotDuration, gotQuality int64
		var gotPDQ []byte
		var gotWidth, gotHeight sql.NullInt64
		if err := db.db.QueryRow(`
			SELECT sha512, duration_ms, thumb_path, thumb_pdq256, thumb_quality,
			       thumb_width, thumb_height
			FROM video_features WHERE sha512=?1`, shaText,
		).Scan(&gotSHA, &gotDuration, &gotPath, &gotPDQ, &gotQuality, &gotWidth, &gotHeight); err != nil {
			db.Close()
			t.Fatal(err)
		}
		if gotSHA != shaText || gotDuration != 1234 || gotPath != `D:\thumb.jpg` || len(gotPDQ) != 2 || gotPDQ[0] != 1 || gotPDQ[1] != 2 || gotQuality != 88 {
			db.Close()
			t.Fatalf("migrated row = sha:%q duration:%d path:%q pdq:%v quality:%d", gotSHA, gotDuration, gotPath, gotPDQ, gotQuality)
		}
		if gotWidth.Valid || gotHeight.Valid {
			db.Close()
			t.Fatalf("legacy contact dimensions = %v/%v, want NULL/NULL", gotWidth, gotHeight)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSaveAnalysisVideoDimensionsFlowThroughLookupAndSyncLoader(t *testing.T) {
	ctx := context.Background()
	db := openAnalysisTestStore(t)
	sha := analysisTestSHA(0x61)
	path := `D:\analysis\dimensions.mp4`
	seedAnalysisFile(t, db, path, sha,
		proto.FieldVideoDuration|proto.FieldVideoContactSheet)
	duration := int64(2345)
	quality := int32(89)
	width := int32(960)
	height := int32(540)
	requested := proto.FieldVideoDuration | proto.FieldVideoContactSheet
	if _, err := db.SaveAnalysis(ctx, AnalysisResult{
		MachineID: "m", Path: path, Kind: MediaVideo, Size: 10, MTime: 20,
		SHA512: sha, RequestedFields: requested, FieldsDone: requested,
		DurationMS: &duration, ThumbPath: `D:\thumb\grid.jpg`,
		ThumbPDQ: make([]byte, 32), ThumbQuality: &quality,
		ThumbWidth: &width, ThumbHeight: &height,
	}); err != nil {
		t.Fatalf("SaveAnalysis: %v", err)
	}
	feature, err := db.LookupVideo(ctx, sha)
	if err != nil {
		t.Fatalf("LookupVideo: %v", err)
	}
	if feature == nil || feature.ThumbWidth == nil || *feature.ThumbWidth != width ||
		feature.ThumbHeight == nil || *feature.ThumbHeight != height {
		t.Fatalf("LookupVideo dimensions = %#v", feature)
	}
	rows, err := db.LoadVideoFeaturesBySHAs(ctx, []string{hex.EncodeToString(sha)})
	if err != nil {
		t.Fatalf("LoadVideoFeaturesBySHAs: %v", err)
	}
	if len(rows) != 1 || rows[0].ThumbWidth == nil || *rows[0].ThumbWidth != width ||
		rows[0].ThumbHeight == nil || *rows[0].ThumbHeight != height {
		t.Fatalf("sync dimensions = %#v", rows)
	}
}
