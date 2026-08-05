package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestPhase1MissingMask(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	shaText := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f"
	if _, err := db.db.ExecContext(ctx, `
		INSERT INTO files (machine_id, path, size, mtime, sha512)
		VALUES ('m', 'image.jpg', 10, 20, ?), ('m', 'video.mp4', 10, 20, ?), ('m', 'stale.jpg', 10, 20, ?);`,
		shaText, shaText, shaText); err != nil {
		t.Fatal(err)
	}

	absent := FileRow{MachineID: "m", Path: "none", Size: 10, MTime: 20}
	if got, err := db.MissingPhase1(ctx, absent, MediaImage); err != nil || got != 3 {
		t.Fatalf("absent image = (%d, %v), want (3, nil)", got, err)
	}
	if got, err := db.MissingPhase1(ctx, absent, MediaVideo); err != nil || got != 5 {
		t.Fatalf("absent video = (%d, %v), want (5, nil)", got, err)
	}

	image := FileRow{MachineID: "m", Path: "image.jpg", Size: 10, MTime: 20, SHA512: &shaText}
	if got, err := db.MissingPhase1(ctx, image, MediaImage); err != nil || got != 2 {
		t.Fatalf("incomplete image = (%d, %v), want (2, nil)", got, err)
	}
	if _, err := db.db.ExecContext(ctx, `
		INSERT INTO image_features (sha512, width, height, pdq256, pdq_quality)
		VALUES (?, 4, 5, X'01', 6);`, shaText); err != nil {
		t.Fatal(err)
	}
	if got, err := db.MissingPhase1(ctx, image, MediaImage); err != nil || got != 0 {
		t.Fatalf("complete image = (%d, %v), want (0, nil)", got, err)
	}

	video := FileRow{MachineID: "m", Path: "video.mp4", Size: 10, MTime: 20, SHA512: &shaText}
	videoCases := []struct {
		name   string
		insert string
		want   uint32
	}{
		{"no feature", "", 4},
		{"missing duration", "INSERT INTO video_features (sha512, thumb_path, thumb_pdq256, thumb_quality) VALUES (?, 'thumb.jpg', X'01', 7)", 4},
		{"missing path", "INSERT INTO video_features (sha512, duration_ms, thumb_pdq256, thumb_quality) VALUES (?, 1, X'01', 7)", 4},
		{"missing pdq", "INSERT INTO video_features (sha512, duration_ms, thumb_path, thumb_quality) VALUES (?, 1, 'thumb.jpg', 7)", 4},
		{"missing quality", "INSERT INTO video_features (sha512, duration_ms, thumb_path, thumb_pdq256) VALUES (?, 1, 'thumb.jpg', X'01')", 4},
		{"complete", "INSERT INTO video_features (sha512, duration_ms, thumb_path, thumb_pdq256, thumb_quality) VALUES (?, 1, 'thumb.jpg', X'01', 7)", 0},
	}
	for _, tc := range videoCases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.db.ExecContext(ctx, `DELETE FROM video_features WHERE sha512=?`, shaText); err != nil {
				t.Fatal(err)
			}
			if tc.insert != "" {
				if _, err := db.db.ExecContext(ctx, tc.insert, shaText); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := db.MissingPhase1(ctx, video, MediaVideo); err != nil || got != tc.want {
				t.Fatalf("MissingPhase1 = (%d, %v), want (%d, nil)", got, err, tc.want)
			}
		})
	}

	stale := FileRow{MachineID: "m", Path: "stale.jpg", Size: 11, MTime: 20, SHA512: &shaText}
	if got, err := db.MissingPhase1(ctx, stale, MediaImage); err != nil || got != 3 {
		t.Fatalf("stale metadata = (%d, %v), want (3, nil)", got, err)
	}

}

func phase1TestSHA() []byte {
	value := make([]byte, 64)
	for i := range value {
		value[i] = byte(i)
	}
	return value
}
