package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"dedup/internal/features"
	"dedup/internal/proto"
)

func TestSavePhase2ImagePartialSharedSHAIdempotentAndPersistedCompletion(t *testing.T) {
	db := openPhase2TestStore(t)
	ctx := context.Background()
	sha := phase2TestSHA(0x11)
	shaText := hex.EncodeToString(sha)
	pathA := `D:\phase2\a.jpg`
	pathB := `D:\phase2\b.jpg`
	idA := seedPhase2File(t, db, pathA, MediaImage, sha,
		proto.FieldPHashParts|proto.FieldSobelHist, true)
	idB := seedPhase2File(t, db, pathB, MediaImage, sha,
		proto.FieldPHashParts|proto.FieldSobelHist, true)
	pHash, sobel := phase2TestBlobs(t, 1)

	first := Phase2Result{
		MachineID: "m", Path: pathA, Kind: MediaImage, SHA512: sha,
		FieldsDone: proto.FieldPHashParts, PHashParts: pHash,
		Errors: []FieldError{{
			Field: proto.FieldSobelHist, Stage: "sobel", Msg: "not ready",
		}},
	}
	if err := db.SavePhase2(ctx, first); err != nil {
		t.Fatalf("first SavePhase2: %v", err)
	}
	assertPhase2FileState(t, db, pathA, proto.FieldSobelHist, false, true, "partial", "sobel: not ready")
	var gotPHash, gotSobel, phase1PDQ []byte
	if err := db.db.QueryRowContext(ctx, `
		SELECT phash_parts, sobel_hist, pdq256
		FROM image_features WHERE sha512=?1`, shaText,
	).Scan(&gotPHash, &gotSobel, &phase1PDQ); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotPHash, pHash) || gotSobel != nil ||
		!bytes.Equal(phase1PDQ, []byte{0xaa, 0xbb}) {
		t.Fatalf("partial image row phash=%x sobel=%x phase1=%x", gotPHash, gotSobel, phase1PDQ)
	}

	first.Errors = nil
	if err := db.SavePhase2(ctx, first); err != nil {
		t.Fatalf("idempotent SavePhase2: %v", err)
	}
	assertPhase2FileState(t, db, pathA, proto.FieldSobelHist, false, true, "partial", "sobel: not ready")

	if err := db.SavePhase2(ctx, Phase2Result{
		MachineID: "m", Path: pathB, Kind: MediaImage, SHA512: sha,
		FieldsDone: proto.FieldSobelHist, SobelHist: sobel,
	}); err != nil {
		t.Fatalf("shared SHA Sobel SavePhase2: %v", err)
	}
	assertPhase2FileState(t, db, pathB, 0, true, true, "done", "")

	if err := db.SavePhase2(ctx, first); err != nil {
		t.Fatalf("persisted completion SavePhase2: %v", err)
	}
	assertPhase2FileState(t, db, pathA, 0, true, true, "done", "")
	missing, err := db.Phase2MissingMask(ctx, "m", pathA)
	if err != nil {
		t.Fatal(err)
	}
	if missing != 0 {
		t.Fatalf("Phase2MissingMask=%#x, want 0 from persisted shared row", missing)
	}

	var filesA, filesB, imageGeneration int64
	if err := db.db.QueryRowContext(ctx, `
		SELECT
			(SELECT generation FROM sync_queue WHERE table_name='files' AND row_pk=?1),
			(SELECT generation FROM sync_queue WHERE table_name='files' AND row_pk=?2),
			(SELECT generation FROM sync_queue WHERE table_name='image_features' AND row_pk=?3)`,
		idA, idB, shaText,
	).Scan(&filesA, &filesB, &imageGeneration); err != nil {
		t.Fatal(err)
	}
	if filesA != 3 || filesB != 1 || imageGeneration != 4 {
		t.Fatalf("queue generations filesA=%d filesB=%d image=%d, want 3/1/4",
			filesA, filesB, imageGeneration)
	}

	pathC := `D:\phase2\phase1-still-missing.jpg`
	seedPhase2File(t, db, pathC, MediaImage, sha,
		proto.FieldSHA512|proto.FieldPHashParts|proto.FieldSobelHist, false)
	if err := db.SavePhase2(ctx, firstWithPath(first, pathC)); err != nil {
		t.Fatal(err)
	}
	assertPhase2FileState(t, db, pathC, proto.FieldSHA512, true, false, "partial", "previous phase2 error")
}

func TestSavePhase2VideoRetainsPartialFramesReplacesAtomicallyAndCompletesFromRows(t *testing.T) {
	db := openPhase2TestStore(t)
	ctx := context.Background()
	sha := phase2TestSHA(0x31)
	shaText := hex.EncodeToString(sha)
	path := `D:\phase2\clip.mp4`
	fileID := seedPhase2File(t, db, path, MediaVideo, sha, proto.FieldVideo6F, true)

	first0 := phase2TestFrame(t, 0, 1)
	first1 := phase2TestFrame(t, 1, 2)
	if err := db.SavePhase2(ctx, Phase2Result{
		MachineID: "m", Path: path, Kind: MediaVideo, SHA512: sha,
		Frames: []Phase2Frame{
			first0,
			first1,
			{FrameIdx: 2, Error: "decode failed"},
		},
	}); err != nil {
		t.Fatalf("partial video SavePhase2: %v", err)
	}
	assertPhase2FileState(t, db, path, proto.FieldVideo6F, false, true, "partial", "frame[2]: decode failed")
	var count int
	if err := db.db.QueryRowContext(ctx,
		`SELECT count(*) FROM video_frames WHERE sha512=?1`, shaText,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("partial frame count=%d, want 2", count)
	}

	replacement := phase2TestFrame(t, 0, 9)
	if err := db.SavePhase2(ctx, Phase2Result{
		MachineID: "m", Path: path, Kind: MediaVideo, SHA512: sha,
		Frames: []Phase2Frame{replacement},
	}); err != nil {
		t.Fatalf("replacement SavePhase2: %v", err)
	}
	var gotPDQ, gotPHash, gotSobel []byte
	if err := db.db.QueryRowContext(ctx, `
		SELECT pdq256, phash_parts, sobel_hist FROM video_frames
		WHERE sha512=?1 AND frame_idx=0`, shaText,
	).Scan(&gotPDQ, &gotPHash, &gotSobel); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotPDQ, replacement.PDQ256) ||
		!bytes.Equal(gotPHash, replacement.PHashParts) ||
		!bytes.Equal(gotSobel, replacement.SobelHist) {
		t.Fatalf("replacement row pdq=%x phash=%x sobel=%x", gotPDQ, gotPHash, gotSobel)
	}

	var remaining []Phase2Frame
	for index := 2; index < 6; index++ {
		remaining = append(remaining, phase2TestFrame(t, index, byte(index+10)))
	}
	if err := db.SavePhase2(ctx, Phase2Result{
		MachineID: "m", Path: path, Kind: MediaVideo, SHA512: sha,
		FieldsDone: 0, Frames: remaining,
	}); err != nil {
		t.Fatalf("completion from persisted rows SavePhase2: %v", err)
	}
	assertPhase2FileState(t, db, path, 0, true, true, "done", "")
	missing, err := db.Phase2MissingMask(ctx, "m", path)
	if err != nil {
		t.Fatal(err)
	}
	if missing != 0 {
		t.Fatalf("Phase2MissingMask=%#x, want 0 after six persisted rows", missing)
	}

	var frame0Generation, frame5Generation, filesGeneration int64
	if err := db.db.QueryRowContext(ctx, `
		SELECT
			(SELECT generation FROM sync_queue WHERE table_name='video_frames' AND row_pk=?1),
			(SELECT generation FROM sync_queue WHERE table_name='video_frames' AND row_pk=?2),
			(SELECT generation FROM sync_queue WHERE table_name='files' AND row_pk=?3)`,
		shaText+":0", shaText+":5", fileID,
	).Scan(&frame0Generation, &frame5Generation, &filesGeneration); err != nil {
		t.Fatal(err)
	}
	if frame0Generation != 2 || frame5Generation != 1 || filesGeneration != 3 {
		t.Fatalf("queue generations frame0=%d frame5=%d files=%d, want 2/1/3",
			frame0Generation, frame5Generation, filesGeneration)
	}
}

func TestSavePhase2VideoStageColumnsMergeInEitherOrder(t *testing.T) {
	for _, order := range [][]uint32{
		{proto.FieldVideo6FPHash, proto.FieldVideo6FSobel},
		{proto.FieldVideo6FSobel, proto.FieldVideo6FPHash},
	} {
		name := fmt.Sprintf("%#x-then-%#x", order[0], order[1])
		t.Run(name, func(t *testing.T) {
			db := openPhase2TestStore(t)
			ctx := context.Background()
			sha := phase2TestSHA(byte(order[0] >> 4))
			shaText := hex.EncodeToString(sha)
			path := `D:\phase2\split-` + name + `.mp4`
			seedPhase2File(t, db, path, MediaVideo, sha,
				proto.FieldVideo6FPHash|proto.FieldVideo6FSobel, true)
			wantPHash, wantSobel := phase2TestBlobs(t, 41)
			for _, field := range order {
				frames := make([]Phase2Frame, 6)
				for index := range frames {
					frames[index].FrameIdx = index
					if field == proto.FieldVideo6FPHash {
						frames[index].PHashParts = append([]byte(nil), wantPHash...)
					} else {
						frames[index].SobelHist = append([]byte(nil), wantSobel...)
					}
				}
				if err := db.SavePhase2(ctx, Phase2Result{
					MachineID: "m", Path: path, Kind: MediaVideo, SHA512: sha,
					FieldsDone: field, Frames: frames,
				}); err != nil {
					t.Fatalf("SavePhase2 field %#x: %v", field, err)
				}
			}
			rows, err := db.db.QueryContext(ctx, `
				SELECT phash_parts, sobel_hist FROM video_frames
				WHERE sha512=?1 ORDER BY frame_idx`, shaText)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			count := 0
			for rows.Next() {
				var gotPHash, gotSobel []byte
				if err := rows.Scan(&gotPHash, &gotSobel); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(gotPHash, wantPHash) || !bytes.Equal(gotSobel, wantSobel) {
					t.Fatalf("merged frame %d pHash=%x Sobel=%x", count, gotPHash, gotSobel)
				}
				count++
			}
			if count != 6 {
				t.Fatalf("merged frame count=%d, want 6", count)
			}
			assertPhase2FileState(t, db, path, 0, true, true, proto.StatusDone, "")
			legacy, err := db.Phase2CommittedStateForFields(ctx, "m", path, MediaVideo, proto.FieldVideo6F)
			if err != nil {
				t.Fatal(err)
			}
			if legacy.MissingFields != proto.FieldVideo6F || legacy.MissingFrames != proto.FrameMaskFull {
				t.Fatalf("split-only legacy cache state=%#v, want strict legacy PDQ miss", legacy)
			}
		})
	}
}

func TestSavePhase2SplitCompletionPreservesUnrelatedMissingState(t *testing.T) {
	db := openPhase2TestStore(t)
	ctx := context.Background()
	sha := phase2TestSHA(0x58)
	path := `D:\phase2\split-other-missing.mp4`
	seedPhase2File(t, db, path, MediaVideo, sha,
		proto.FieldSHA512|proto.FieldVideo6FPHash|proto.FieldVideo6FSobel, false)
	pHash, sobel := phase2TestBlobs(t, 51)
	for _, field := range []uint32{proto.FieldVideo6FSobel, proto.FieldVideo6FPHash} {
		frames := make([]Phase2Frame, 6)
		for index := range frames {
			frames[index].FrameIdx = index
			if field == proto.FieldVideo6FPHash {
				frames[index].PHashParts = pHash
			} else {
				frames[index].SobelHist = sobel
			}
		}
		if err := db.SavePhase2(ctx, Phase2Result{MachineID: "m", Path: path, Kind: MediaVideo, SHA512: sha, FieldsDone: field, Frames: frames}); err != nil {
			t.Fatal(err)
		}
	}
	assertPhase2FileState(t, db, path, proto.FieldSHA512, true, false, proto.StatusPartial, "")
}

func TestPhase2CommittedStateDerivesMissingImageFieldsAndCompleteVideoFrames(t *testing.T) {
	db := openPhase2TestStore(t)
	ctx := context.Background()

	imageSHA := phase2TestSHA(0x41)
	imagePath := `D:\phase2\committed-image.jpg`
	seedPhase2File(
		t,
		db,
		imagePath,
		MediaImage,
		imageSHA,
		proto.FieldPHashParts|proto.FieldSobelHist,
		true,
	)
	pHash, _ := phase2TestBlobs(t, 5)
	if _, err := db.db.ExecContext(ctx, `
		UPDATE image_features SET phash_parts=?1
		WHERE sha512=?2`,
		pHash,
		hex.EncodeToString(imageSHA),
	); err != nil {
		t.Fatal(err)
	}
	image, err := db.Phase2CommittedState(
		ctx,
		"m",
		imagePath,
		MediaImage,
	)
	if err != nil {
		t.Fatal(err)
	}
	if image.SHA512 != hex.EncodeToString(imageSHA) ||
		image.MissingFields != proto.FieldSobelHist ||
		image.MissingFrames != 0 {
		t.Fatalf("image committed state=%#v", image)
	}

	videoSHA := phase2TestSHA(0x42)
	videoPath := `D:\phase2\committed-video.mp4`
	seedPhase2File(
		t,
		db,
		videoPath,
		MediaVideo,
		videoSHA,
		proto.FieldVideo6F,
		true,
	)
	videoSHAText := hex.EncodeToString(videoSHA)
	for _, index := range []int{0, 2} {
		frame := phase2TestFrame(t, index, byte(index+1))
		if _, err := db.db.ExecContext(ctx, `
			INSERT INTO video_frames(
				sha512, frame_idx, pdq256, phash_parts, sobel_hist
			) VALUES(?1, ?2, ?3, ?4, ?5)`,
			videoSHAText,
			frame.FrameIdx,
			frame.PDQ256,
			frame.PHashParts,
			frame.SobelHist,
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.db.ExecContext(ctx, `
		INSERT INTO video_frames(
			sha512, frame_idx, pdq256, phash_parts, sobel_hist
		) VALUES(?1, 1, x'01', x'02', NULL)`,
		videoSHAText,
	); err != nil {
		t.Fatal(err)
	}
	video, err := db.Phase2CommittedState(
		ctx,
		"m",
		videoPath,
		MediaVideo,
	)
	if err != nil {
		t.Fatal(err)
	}
	wantFrames := proto.FrameMaskFull &^ (1<<0 | 1<<2)
	if video.SHA512 != videoSHAText ||
		video.MissingFields != proto.FieldVideo6F ||
		video.MissingFrames != wantFrames {
		t.Fatalf("video committed state=%#v, want missing frames %#x",
			video, wantFrames)
	}
}

func TestPhase2CommittedStateTreatsMalformedBlobsAndFramesAsMissing(t *testing.T) {
	db := openPhase2TestStore(t)
	ctx := context.Background()
	validPHash, validSobel := phase2TestBlobs(t, 20)

	for _, test := range []struct {
		name         string
		corruptPHash func([]byte) []byte
		corruptSobel func([]byte) []byte
		want         uint32
	}{
		{
			name:         "pHash wrong length",
			corruptPHash: func([]byte) []byte { return []byte{1} },
			want:         proto.FieldPHashParts,
		},
		{
			name: "pHash old version",
			corruptPHash: func(value []byte) []byte {
				out := append([]byte(nil), value...)
				out[0] = 0
				return out
			},
			want: proto.FieldPHashParts,
		},
		{
			name:         "Sobel wrong length",
			corruptSobel: func([]byte) []byte { return []byte{1} },
			want:         proto.FieldSobelHist,
		},
		{
			name: "Sobel old version",
			corruptSobel: func(value []byte) []byte {
				out := append([]byte(nil), value...)
				out[0] = 0
				return out
			},
			want: proto.FieldSobelHist,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			sha := phase2TestSHA(byte(0x80 + len(test.name)))
			path := `D:\phase2\malformed-` + test.name + `.jpg`
			seedPhase2File(
				t,
				db,
				path,
				MediaImage,
				sha,
				proto.FieldPHashParts|proto.FieldSobelHist,
				true,
			)
			pHash := append([]byte(nil), validPHash...)
			sobel := append([]byte(nil), validSobel...)
			if test.corruptPHash != nil {
				pHash = test.corruptPHash(pHash)
			}
			if test.corruptSobel != nil {
				sobel = test.corruptSobel(sobel)
			}
			if _, err := db.db.ExecContext(ctx, `
				UPDATE image_features
				SET phash_parts=?1, sobel_hist=?2
				WHERE sha512=?3`,
				pHash,
				sobel,
				hex.EncodeToString(sha),
			); err != nil {
				t.Fatal(err)
			}
			state, err := db.Phase2CommittedState(
				ctx,
				"m",
				path,
				MediaImage,
			)
			if err != nil {
				t.Fatal(err)
			}
			if state.MissingFields != test.want {
				t.Fatalf("committed state=%#v, want missing %#x",
					state, test.want)
			}
		})
	}

	videoSHA := phase2TestSHA(0xa0)
	videoPath := `D:\phase2\malformed-video.mp4`
	seedPhase2File(
		t,
		db,
		videoPath,
		MediaVideo,
		videoSHA,
		proto.FieldVideo6F,
		true,
	)
	videoText := hex.EncodeToString(videoSHA)
	validFrame := phase2TestFrame(t, 0, 30)
	invalidPDQ := phase2TestFrame(t, 1, 31)
	invalidPDQ.PDQ256 = []byte{1}
	invalidPHash := phase2TestFrame(t, 2, 32)
	invalidPHash.PHashParts[0] = 0
	invalidSobel := phase2TestFrame(t, 3, 33)
	invalidSobel.SobelHist[0] = 0
	for _, frame := range []Phase2Frame{
		validFrame,
		invalidPDQ,
		invalidPHash,
		invalidSobel,
	} {
		if _, err := db.db.ExecContext(ctx, `
			INSERT INTO video_frames(
				sha512, frame_idx, pdq256, phash_parts, sobel_hist
			) VALUES(?1, ?2, ?3, ?4, ?5)`,
			videoText,
			frame.FrameIdx,
			frame.PDQ256,
			frame.PHashParts,
			frame.SobelHist,
		); err != nil {
			t.Fatal(err)
		}
	}
	state, err := db.Phase2CommittedState(
		ctx,
		"m",
		videoPath,
		MediaVideo,
	)
	if err != nil {
		t.Fatal(err)
	}
	wantFrames := proto.FrameMaskFull &^ 1
	if state.MissingFields != proto.FieldVideo6F ||
		state.MissingFrames != wantFrames {
		t.Fatalf("video committed state=%#v, want frames %#x",
			state, wantFrames)
	}
}

func TestSavePhase2RejectsInvalidPayloadBeforeAnyWrite(t *testing.T) {
	pHash, sobel := phase2TestBlobs(t, 4)
	nonFinite := append([]byte(nil), sobel...)
	nonFinite[4], nonFinite[5], nonFinite[6], nonFinite[7] = 0, 0, 0xc0, 0x7f
	tests := []struct {
		name    string
		kind    MediaKind
		missing uint32
		result  func([]byte, string) Phase2Result
	}{
		{
			name: "phash length", kind: MediaImage,
			missing: proto.FieldPHashParts | proto.FieldSobelHist,
			result: func(sha []byte, path string) Phase2Result {
				return Phase2Result{
					MachineID: "m", Path: path, Kind: MediaImage, SHA512: sha,
					FieldsDone: proto.FieldPHashParts | proto.FieldSobelHist,
					PHashParts: []byte{1}, SobelHist: sobel,
				}
			},
		},
		{
			name: "phash version", kind: MediaImage,
			missing: proto.FieldPHashParts | proto.FieldSobelHist,
			result: func(sha []byte, path string) Phase2Result {
				bad := append([]byte(nil), pHash...)
				bad[0]++
				return Phase2Result{
					MachineID: "m", Path: path, Kind: MediaImage, SHA512: sha,
					FieldsDone: proto.FieldPHashParts, PHashParts: bad,
				}
			},
		},
		{
			name: "sobel non-finite", kind: MediaImage,
			missing: proto.FieldPHashParts | proto.FieldSobelHist,
			result: func(sha []byte, path string) Phase2Result {
				return Phase2Result{
					MachineID: "m", Path: path, Kind: MediaImage, SHA512: sha,
					FieldsDone: proto.FieldSobelHist, SobelHist: nonFinite,
				}
			},
		},
		{
			name: "duplicate frame", kind: MediaVideo, missing: proto.FieldVideo6F,
			result: func(sha []byte, path string) Phase2Result {
				return Phase2Result{
					MachineID: "m", Path: path, Kind: MediaVideo, SHA512: sha,
					Frames: []Phase2Frame{
						phase2TestFrame(t, 0, 1),
						phase2TestFrame(t, 0, 2),
					},
				}
			},
		},
		{
			name: "out of range frame", kind: MediaVideo, missing: proto.FieldVideo6F,
			result: func(sha []byte, path string) Phase2Result {
				return Phase2Result{
					MachineID: "m", Path: path, Kind: MediaVideo, SHA512: sha,
					Frames: []Phase2Frame{phase2TestFrame(t, 6, 1)},
				}
			},
		},
		{
			name: "error plus frame payload", kind: MediaVideo, missing: proto.FieldVideo6F,
			result: func(sha []byte, path string) Phase2Result {
				frame := phase2TestFrame(t, 0, 1)
				frame.Error = "ffmpeg failed"
				return Phase2Result{
					MachineID: "m", Path: path, Kind: MediaVideo, SHA512: sha,
					Frames: []Phase2Frame{frame},
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := openPhase2TestStore(t)
			ctx := context.Background()
			sha := phase2TestSHA(0x51)
			path := `D:\phase2\invalid-` + test.name
			seedPhase2File(t, db, path, test.kind, sha, test.missing, true)
			if err := db.SavePhase2(ctx, test.result(sha, path)); err == nil {
				t.Fatal("SavePhase2 error=nil, want invalid payload rejection")
			}
			var frameCount, queueCount int
			var phash, storedSobel []byte
			if err := db.db.QueryRowContext(ctx, `
				SELECT
					(SELECT phash_parts FROM image_features WHERE sha512=?1),
					(SELECT sobel_hist FROM image_features WHERE sha512=?1),
					(SELECT count(*) FROM video_frames),
					(SELECT count(*) FROM sync_queue)`,
				hex.EncodeToString(sha),
			).Scan(&phash, &storedSobel, &frameCount, &queueCount); err != nil {
				t.Fatal(err)
			}
			if phash != nil || storedSobel != nil || frameCount != 0 || queueCount != 0 {
				t.Fatalf("invalid result wrote phash=%x sobel=%x frames=%d queue=%d",
					phash, storedSobel, frameCount, queueCount)
			}
			assertPhase2FileState(t, db, path, test.missing, false, true, "partial", "previous phase2 error")
		})
	}
}

func TestSavePhase2StaleSHAIsNoOpBeforePayloadValidation(t *testing.T) {
	for _, test := range []struct {
		name      string
		storedSHA []byte
	}{
		{name: "mismatch", storedSHA: phase2TestSHA(0x61)},
		{name: "null"},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := openPhase2TestStore(t)
			ctx := context.Background()
			path := `D:\phase2\stale-` + test.name + `.jpg`
			seedPhase2File(t, db, path, MediaImage, test.storedSHA,
				proto.FieldPHashParts|proto.FieldSobelHist, true)
			incomingSHA := phase2TestSHA(0x62)
			err := db.SavePhase2(ctx, Phase2Result{
				MachineID: "m", Path: path, Kind: MediaImage, SHA512: incomingSHA,
				FieldsDone: proto.FieldPHashParts, PHashParts: []byte{1},
			})
			if !errors.Is(err, ErrPhase2Stale) {
				t.Fatalf("stale SavePhase2 error=%v, want ErrPhase2Stale", err)
			}
			var incomingFeatures, queueCount int
			if err := db.db.QueryRowContext(ctx, `
				SELECT
					(SELECT count(*) FROM image_features WHERE sha512=?1),
					(SELECT count(*) FROM sync_queue)`,
				hex.EncodeToString(incomingSHA),
			).Scan(&incomingFeatures, &queueCount); err != nil {
				t.Fatal(err)
			}
			if incomingFeatures != 0 || queueCount != 0 {
				t.Fatalf("stale result wrote features=%d queue=%d", incomingFeatures, queueCount)
			}
			assertPhase2FileState(t, db, path,
				proto.FieldPHashParts|proto.FieldSobelHist,
				false, true, "partial", "previous phase2 error")
		})
	}
}

func TestSavePhase2QueueFailureRollsBackFeatureFileAndQueue(t *testing.T) {
	db := openPhase2TestStore(t)
	ctx := context.Background()
	sha := phase2TestSHA(0x71)
	path := `D:\phase2\queue-rollback.jpg`
	seedPhase2File(t, db, path, MediaImage, sha,
		proto.FieldPHashParts|proto.FieldSobelHist, true)
	if _, err := db.db.ExecContext(ctx, `
		CREATE TRIGGER fail_phase2_queue BEFORE INSERT ON sync_queue
		BEGIN SELECT RAISE(FAIL, 'controlled phase2 queue failure'); END;`); err != nil {
		t.Fatal(err)
	}
	pHash, _ := phase2TestBlobs(t, 7)
	err := db.SavePhase2(ctx, Phase2Result{
		MachineID: "m", Path: path, Kind: MediaImage, SHA512: sha,
		FieldsDone: proto.FieldPHashParts, PHashParts: pHash,
	})
	if err == nil || !strings.Contains(err.Error(), "controlled phase2 queue failure") {
		t.Fatalf("SavePhase2 error=%v, want controlled queue failure", err)
	}
	var phash []byte
	var queueCount int
	if err := db.db.QueryRowContext(ctx, `
		SELECT
			(SELECT phash_parts FROM image_features WHERE sha512=?1),
			(SELECT count(*) FROM sync_queue)`,
		hex.EncodeToString(sha),
	).Scan(&phash, &queueCount); err != nil {
		t.Fatal(err)
	}
	if phash != nil || queueCount != 0 {
		t.Fatalf("queue failure left phash=%x queue=%d", phash, queueCount)
	}
	assertPhase2FileState(t, db, path,
		proto.FieldPHashParts|proto.FieldSobelHist,
		false, true, "partial", "previous phase2 error")
}

func openPhase2TestStore(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedPhase2File(
	t *testing.T,
	db *DB,
	path string,
	kind MediaKind,
	sha []byte,
	missing uint32,
	phase1Done bool,
) int64 {
	t.Helper()
	ctx := context.Background()
	if err := db.UpsertEnumerated(ctx, []EnumUpsert{{
		MachineID: "m", DiskNo: 1, Path: path, Size: 10, MTime: 20,
		MissingBase: missing,
	}}); err != nil {
		t.Fatal(err)
	}
	var shaValue any
	if len(sha) != 0 {
		shaValue = hex.EncodeToString(sha)
	}
	if _, err := db.db.ExecContext(ctx, `
		UPDATE files SET sha512=?1, phase1_done=?2, phase2_done=0,
			status='partial', missing_mask=?3, error='previous phase2 error'
		WHERE machine_id='m' AND path=?4`,
		shaValue, boolToInt(phase1Done), missing, path,
	); err != nil {
		t.Fatal(err)
	}
	if len(sha) != 0 {
		shaText := hex.EncodeToString(sha)
		switch kind {
		case MediaImage:
			if _, err := db.db.ExecContext(ctx, `
				INSERT INTO image_features(sha512,width,height,pdq256,pdq_quality)
				VALUES(?1,20,10,x'aabb',80)
				ON CONFLICT(sha512) DO NOTHING`, shaText,
			); err != nil {
				t.Fatal(err)
			}
		case MediaVideo:
			if _, err := db.db.ExecContext(ctx, `
				INSERT INTO video_features(sha512,duration_ms,thumb_path,thumb_pdq256,thumb_quality)
				VALUES(?1,1000,'thumb.jpg',x'0102',70)
				ON CONFLICT(sha512) DO NOTHING`, shaText,
			); err != nil {
				t.Fatal(err)
			}
		}
	}
	var id int64
	if err := db.db.QueryRowContext(ctx,
		`SELECT id FROM files WHERE machine_id='m' AND path=?1`, path,
	).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func assertPhase2FileState(
	t *testing.T,
	db *DB,
	path string,
	wantMissing uint32,
	wantPhase2 bool,
	wantPhase1 bool,
	wantStatus string,
	wantError string,
) {
	t.Helper()
	var missing uint32
	var phase2, phase1 int
	var status string
	var errorText sql.NullString
	if err := db.db.QueryRowContext(context.Background(), `
		SELECT missing_mask, phase2_done, phase1_done, status, error
		FROM files WHERE machine_id='m' AND path=?1`, path,
	).Scan(&missing, &phase2, &phase1, &status, &errorText); err != nil {
		t.Fatal(err)
	}
	if missing != wantMissing || phase2 != boolToInt(wantPhase2) ||
		phase1 != boolToInt(wantPhase1) || status != wantStatus ||
		errorText.String != wantError || errorText.Valid != (wantError != "") {
		t.Fatalf("file state missing=%#x phase2=%d phase1=%d status=%q error=%q/%t; want %#x/%t/%t/%q/%q",
			missing, phase2, phase1, status, errorText.String, errorText.Valid,
			wantMissing, wantPhase2, wantPhase1, wantStatus, wantError)
	}
}

func firstWithPath(result Phase2Result, path string) Phase2Result {
	result.Path = path
	return result
}

func phase2TestSHA(seed byte) []byte {
	sha := make([]byte, 64)
	for i := range sha {
		sha[i] = seed + byte(i)
	}
	return sha
}

func phase2TestBlobs(t *testing.T, seed uint64) ([]byte, []byte) {
	t.Helper()
	parts := [9]uint64{}
	for i := range parts {
		parts[i] = seed + uint64(i)
	}
	pHash := features.EncodePHashParts(parts)
	var histogram [128]float32
	for i := range histogram {
		histogram[i] = float32(seed) + float32(i)/128
	}
	sobel, err := features.EncodeSobelHist(histogram)
	if err != nil {
		t.Fatal(err)
	}
	return pHash, sobel
}

func phase2TestFrame(t *testing.T, index int, seed byte) Phase2Frame {
	t.Helper()
	pHash, sobel := phase2TestBlobs(t, uint64(seed))
	return Phase2Frame{
		FrameIdx: index, PDQ256: bytes.Repeat([]byte{seed}, 32),
		Quality: 75, PHashParts: pHash, SobelHist: sobel,
	}
}
