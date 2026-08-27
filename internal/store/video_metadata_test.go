package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"dedup/internal/proto"

	_ "modernc.org/sqlite"
)

func TestSchemaV5MigratesRealV4WithoutChangingExistingVideoBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v4.db")
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 128)
	if _, err := raw.Exec(`
		PRAGMA user_version=4;
		CREATE TABLE video_features (
			sha512 TEXT PRIMARY KEY, duration_ms INTEGER, thumb_path TEXT,
			thumb_pdq256 BLOB, thumb_quality INTEGER, thumb_width INTEGER, thumb_height INTEGER
		);
		CREATE TABLE video_frames (
			sha512 TEXT NOT NULL, frame_idx INTEGER NOT NULL, pdq256 BLOB,
			phash_parts BLOB, sobel_hist BLOB, PRIMARY KEY(sha512,frame_idx)
		);
		INSERT INTO video_features VALUES(?1,123,'thumb.jpg',x'001122ff',77,640,360);
		INSERT INTO video_frames VALUES(?1,2,x'1020',x'3040',x'5060');`, sha); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 5 {
		t.Fatalf("user_version=%d, want 5", version)
	}
	var featurePDQ, framePDQ, framePHash, frameSobel []byte
	if err := db.db.QueryRow(`SELECT thumb_pdq256 FROM video_features WHERE sha512=?1`, sha).Scan(&featurePDQ); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT pdq256,phash_parts,sobel_hist FROM video_frames WHERE sha512=?1 AND frame_idx=2`, sha).Scan(&framePDQ, &framePHash, &frameSobel); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(featurePDQ, []byte{0, 0x11, 0x22, 0xff}) ||
		!bytes.Equal(framePDQ, []byte{0x10, 0x20}) ||
		!bytes.Equal(framePHash, []byte{0x30, 0x40}) ||
		!bytes.Equal(frameSobel, []byte{0x50, 0x60}) {
		t.Fatalf("legacy video bytes changed: %x %x %x %x", featurePDQ, framePDQ, framePHash, frameSobel)
	}
	for _, table := range []string{"video_containers", "video_streams"} {
		var ddlText string
		if err := db.db.QueryRow(`SELECT sql FROM sqlite_schema WHERE type='table' AND name=?1`, table).Scan(&ddlText); err != nil {
			t.Fatalf("load %s DDL: %v", table, err)
		}
		normalized := strings.ToLower(strings.Join(strings.Fields(ddlText), " "))
		if table == "video_streams" && (!strings.Contains(normalized, "references video_containers") ||
			!strings.Contains(normalized, "on delete cascade") ||
			!strings.Contains(normalized, "media_type in ('video','audio','subtitle','data','attachment')")) {
			t.Fatalf("video_streams DDL misses FK/CHECK contract: %s", ddlText)
		}
		if !strings.Contains(normalized, "json_valid(tags_json)") {
			t.Fatalf("%s DDL misses tags JSON CHECK: %s", table, ddlText)
		}
	}
}

func TestVideoMetadataSaveAnalysisRollsBackExactReplacementAndQueuesOnStreamFailure(t *testing.T) {
	db := openAnalysisTestStore(t)
	ctx := context.Background()
	shaBytes := analysisTestSHA(0x71)
	sha := hex.EncodeToString(shaBytes)
	path := `D:\analysis\metadata.mkv`
	fileID := seedAnalysisFile(t, db, path, shaBytes, proto.FieldVideoMetadata)
	if _, err := db.db.ExecContext(ctx, `
		INSERT INTO video_containers(sha512,format_name,tags_json) VALUES(?1,'old','{}');
		INSERT INTO video_streams(sha512,stream_index,media_type,codec_id,codec_name,tags_json)
		VALUES(?1,9,'audio',86018,'aac','{}');
		CREATE TRIGGER fail_second_stream BEFORE INSERT ON video_streams
		WHEN NEW.stream_index=1 BEGIN SELECT RAISE(ABORT,'stream failure'); END;`, sha); err != nil {
		t.Fatal(err)
	}
	beforeQueue := syncQueueSnapshot(t, db)

	_, err := db.SaveAnalysis(ctx, AnalysisResult{
		MachineID: "m", Path: path, Kind: MediaVideo, Size: 10, MTime: 20,
		SHA512: shaBytes, RequestedFields: proto.FieldVideoMetadata,
		FieldsDone:     proto.FieldVideoMetadata,
		VideoContainer: &proto.VideoContainerMetadata{FormatName: "matroska", TagsJSON: `{}`},
		VideoStreams: []proto.VideoStreamMetadata{
			{Index: 1, MediaType: "audio", CodecID: 86018, CodecName: "aac", TagsJSON: `{}`},
			{Index: 0, MediaType: "video", CodecID: 27, CodecName: "h264", TagsJSON: `{}`},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "stream failure") {
		t.Fatalf("SaveAnalysis error=%v, want trigger failure", err)
	}
	var format string
	if err := db.db.QueryRow(`SELECT format_name FROM video_containers WHERE sha512=?1`, sha).Scan(&format); err != nil {
		t.Fatal(err)
	}
	if format != "old" {
		t.Fatalf("container committed despite rollback: %q", format)
	}
	var indexes string
	if err := db.db.QueryRow(`SELECT group_concat(stream_index,',') FROM video_streams WHERE sha512=?1`, sha).Scan(&indexes); err != nil {
		t.Fatal(err)
	}
	if indexes != "9" {
		t.Fatalf("streams changed despite rollback: %q", indexes)
	}
	var missing uint32
	if err := db.db.QueryRow(`SELECT missing_mask FROM files WHERE id=?1`, fileID).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if missing != proto.FieldVideoMetadata {
		t.Fatalf("file missing_mask=%#x, want metadata still missing", missing)
	}
	if after := syncQueueSnapshot(t, db); after != beforeQueue {
		t.Fatalf("sync queue changed despite rollback: before=%q after=%q", beforeQueue, after)
	}
}

func TestVideoMetadataSaveAnalysisCompletesIndependentlyFromThumbnailAndLookupRequiresLegalSet(t *testing.T) {
	db := openAnalysisTestStore(t)
	ctx := context.Background()
	shaBytes := analysisTestSHA(0x72)
	sha := hex.EncodeToString(shaBytes)
	path := `D:\analysis\metadata-partial.mkv`
	seedAnalysisFile(t, db, path, shaBytes, proto.FieldVideoMetadata|proto.FieldVideoContactSheet)
	container := &proto.VideoContainerMetadata{FormatName: "matroska", TagsJSON: `{}`}
	streams := []proto.VideoStreamMetadata{
		{Index: 3, MediaType: "audio", CodecID: 86018, CodecName: "aac", TagsJSON: `{}`},
		{Index: 1, MediaType: "video", CodecID: 27, CodecName: "h264", TagsJSON: `{}`},
	}
	state, err := db.SaveAnalysis(ctx, AnalysisResult{
		MachineID: "m", Path: path, Kind: MediaVideo, Size: 10, MTime: 20, SHA512: shaBytes,
		RequestedFields: proto.FieldVideoMetadata | proto.FieldVideoContactSheet,
		FieldsDone:      proto.FieldVideoMetadata,
		VideoContainer:  container,
		VideoStreams:    streams,
		Errors: []FieldError{{
			Field: proto.FieldVideoContactSheet, Stage: "contact_sheet", Msg: "thumbnail failed",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.FieldsPresent != proto.FieldVideoMetadata || state.MissingFields != proto.FieldVideoContactSheet {
		t.Fatalf("independent metadata/thumbnail state=%#v", state)
	}
	content, err := db.LookupContent(ctx, shaBytes, MediaVideo, proto.FieldVideoMetadata, 0)
	if err != nil {
		t.Fatal(err)
	}
	if content.FieldsPresent != proto.FieldVideoMetadata || content.MissingFields != 0 ||
		content.VideoContainer == nil || len(content.VideoStreams) != 2 ||
		content.VideoStreams[0].Index != 1 || content.VideoStreams[1].Index != 3 {
		t.Fatalf("complete metadata content=%#v", content)
	}
	row := FileRow{MachineID: "m", Path: path, Size: 10, MTime: 20, SHA512: &sha}
	missing, err := db.MissingPhase1(ctx, row, MediaVideo)
	if err != nil || missing != proto.FieldVideoDuration|proto.FieldVideoContactSheet {
		t.Fatalf("MissingPhase1 complete metadata=%#x err=%v", missing, err)
	}

	if _, err := db.db.ExecContext(ctx, `UPDATE video_containers SET tags_json='{"b":1,"a":2}' WHERE sha512=?1`, sha); err != nil {
		t.Fatal(err)
	}
	content, err = db.LookupContent(ctx, shaBytes, MediaVideo, proto.FieldVideoMetadata, 0)
	if err != nil {
		t.Fatal(err)
	}
	if content.FieldsPresent != 0 || content.MissingFields != proto.FieldVideoMetadata || content.VideoContainer != nil {
		t.Fatalf("invalid metadata unexpectedly reusable=%#v", content)
	}
	missing, err = db.MissingPhase1(ctx, row, MediaVideo)
	if err != nil || missing&proto.FieldVideoMetadata == 0 {
		t.Fatalf("invalid metadata cleared missing bit: missing=%#x err=%v", missing, err)
	}
}

func syncQueueSnapshot(t *testing.T, db *DB) string {
	t.Helper()
	rows, err := db.db.Query(`SELECT table_name||':'||row_pk||':'||generation||':'||synced FROM sync_queue ORDER BY table_name,row_pk`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(values, "|")
}
