package store

import (
	"bytes"
	"context"
	"encoding/hex"
	"path/filepath"
	"testing"

	"dedup/internal/features"
	"dedup/internal/proto"
)

func TestLookupContentImageFullHitReturnsExactRequestedMasks(t *testing.T) {
	db := openContentTestDB(t)
	sha := bytes.Repeat([]byte{0x11}, 64)
	pdq := bytes.Repeat([]byte{0x22}, 32)
	pHash := features.EncodePHashParts([9]uint64{1, 2, 3})
	sobel, err := features.EncodeSobelHist([128]float32{1, 2, 3})
	if err != nil {
		t.Fatalf("EncodeSobelHist: %v", err)
	}
	if _, err := db.db.Exec(`
		INSERT INTO image_features
			(sha512, width, height, pdq256, pdq_quality, phash_parts, sobel_hist)
		VALUES (?1, 640, 480, ?2, 87, ?3, ?4)`,
		hex.EncodeToString(sha), pdq, pHash, sobel,
	); err != nil {
		t.Fatalf("insert image feature: %v", err)
	}

	requested := proto.FieldSHA512 | proto.FieldPDQ256 |
		proto.FieldPHashParts | proto.FieldSobelHist
	state, err := db.LookupContent(
		context.Background(), sha, MediaImage, requested, 0,
	)
	if err != nil {
		t.Fatalf("LookupContent: %v", err)
	}
	if state.FieldsPresent != requested || state.MissingFields != 0 {
		t.Fatalf("field masks = present %#x missing %#x, want %#x/0",
			state.FieldsPresent, state.MissingFields, requested)
	}
	if state.FramesPresent != 0 || state.MissingFrames != 0 {
		t.Fatalf("image frame masks = %#x/%#x, want 0/0",
			state.FramesPresent, state.MissingFrames)
	}
	if state.Image == nil || !bytes.Equal(state.Image.PDQ, pdq) ||
		!bytes.Equal(state.Image.PHashParts, pHash) ||
		!bytes.Equal(state.Image.SobelHist, sobel) {
		t.Fatalf("image payload = %#v, want all requested decoded blobs", state.Image)
	}
}

func TestLookupContentVideoDurationOnlyIgnoresUnrequestedContactSheet(t *testing.T) {
	db := openContentTestDB(t)
	sha := bytes.Repeat([]byte{0x33}, 64)
	if _, err := db.db.Exec(`
		INSERT INTO video_features
			(sha512, duration_ms, thumb_path, thumb_pdq256, thumb_quality)
		VALUES (?1, 12345, NULL, ?2, NULL)`,
		hex.EncodeToString(sha), []byte{0xff},
	); err != nil {
		t.Fatalf("insert video feature: %v", err)
	}

	state, err := db.LookupContent(
		context.Background(), sha, MediaVideo, proto.FieldVideoDuration, 0,
	)
	if err != nil {
		t.Fatalf("LookupContent: %v", err)
	}
	if state.FieldsPresent != proto.FieldVideoDuration || state.MissingFields != 0 {
		t.Fatalf("duration masks = present %#x missing %#x, want %#x/0",
			state.FieldsPresent, state.MissingFields, proto.FieldVideoDuration)
	}
	if state.Video == nil || state.Video.DurationMS == nil || *state.Video.DurationMS != 12345 {
		t.Fatalf("duration payload = %#v, want 12345", state.Video)
	}
}

func TestLookupContentVideoFiveOfSixFramesPreservesExactPartialState(t *testing.T) {
	db := openContentTestDB(t)
	sha := bytes.Repeat([]byte{0x44}, 64)
	shaText := hex.EncodeToString(sha)
	pHash := features.EncodePHashParts([9]uint64{9})
	sobel, err := features.EncodeSobelHist([128]float32{1})
	if err != nil {
		t.Fatalf("EncodeSobelHist: %v", err)
	}
	for frameIdx := 0; frameIdx < 5; frameIdx++ {
		if _, err := db.db.Exec(`
			INSERT INTO video_frames (sha512, frame_idx, pdq256, phash_parts, sobel_hist)
			VALUES (?1, ?2, ?3, ?4, ?5)`,
			shaText, frameIdx, bytes.Repeat([]byte{byte(frameIdx + 1)}, 32), pHash, sobel,
		); err != nil {
			t.Fatalf("insert frame %d: %v", frameIdx, err)
		}
	}

	state, err := db.LookupContent(
		context.Background(), sha, MediaVideo,
		proto.FieldVideo6F, proto.FrameMaskFull,
	)
	if err != nil {
		t.Fatalf("LookupContent: %v", err)
	}
	if state.FieldsPresent != 0 || state.MissingFields != proto.FieldVideo6F {
		t.Fatalf("video-6f masks = present %#x missing %#x, want 0/%#x",
			state.FieldsPresent, state.MissingFields, proto.FieldVideo6F)
	}
	if state.FramesPresent != 0x1f || state.MissingFrames != 0x20 {
		t.Fatalf("frame masks = present %#x missing %#x, want 0x1f/0x20",
			state.FramesPresent, state.MissingFrames)
	}
	if len(state.Frames) != 5 {
		t.Fatalf("returned frames = %d, want five committed frames", len(state.Frames))
	}
	for index, frame := range state.Frames {
		if frame.FrameIdx != index {
			t.Fatalf("frame[%d].FrameIdx = %d, want %d", index, frame.FrameIdx, index)
		}
	}
}

func TestLookupContentCorruptBlobsAreMissingAndNotReturned(t *testing.T) {
	db := openContentTestDB(t)
	sha := bytes.Repeat([]byte{0x55}, 64)
	if _, err := db.db.Exec(`
		INSERT INTO image_features
			(sha512, width, height, pdq256, pdq_quality, phash_parts, sobel_hist)
		VALUES (?1, 10, 20, ?2, 50, ?3, ?4)`,
		hex.EncodeToString(sha), []byte{1}, []byte{2}, []byte{3},
	); err != nil {
		t.Fatalf("insert corrupt image feature: %v", err)
	}

	requested := proto.FieldPDQ256 | proto.FieldPHashParts | proto.FieldSobelHist
	state, err := db.LookupContent(
		context.Background(), sha, MediaImage, requested, 0,
	)
	if err != nil {
		t.Fatalf("LookupContent: %v", err)
	}
	if state.FieldsPresent != 0 || state.MissingFields != requested {
		t.Fatalf("corrupt masks = present %#x missing %#x, want 0/%#x",
			state.FieldsPresent, state.MissingFields, requested)
	}
	if state.Image != nil {
		t.Fatalf("corrupt image payload returned: %#v", state.Image)
	}
}

func openContentTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "content.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
