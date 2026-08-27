package store

import (
	"bytes"
	"context"
	"encoding/hex"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"dedup/internal/proto"
)

func TestSyncVideoMetadataLoaderReturnsOneContainerWithAllOrderedStreams(t *testing.T) {
	db := openSyncQueueTestStore(t)
	ctx := context.Background()
	sha := strings.Repeat("8", 128)
	if _, err := db.db.ExecContext(ctx, `
		INSERT INTO video_containers(
			sha512,format_name,start_time_us,tags_json,primary_video_stream
		) VALUES(?1,'matroska',NULL,'{}',2);
		INSERT INTO video_streams(
			sha512,stream_index,media_type,codec_id,codec_name,tags_json,width,height
		) VALUES
			(?1,2,'video',27,'h264','{}',1920,1080),
			(?1,0,'audio',86018,'aac','{}',NULL,NULL);`, sha); err != nil {
		t.Fatal(err)
	}
	rows, err := db.LoadVideoMetadataBySHAs(ctx, []string{sha, sha})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].SHA512 != sha || rows[0].Container.StartTimeUS != nil ||
		len(rows[0].Streams) != 2 || rows[0].Streams[0].Index != 0 || rows[0].Streams[1].Index != 2 {
		t.Fatalf("loaded metadata = %#v", rows)
	}
	want := []string{"audio", "video"}
	got := []string{rows[0].Streams[0].MediaType, rows[0].Streams[1].MediaType}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ordered media types=%v, want %v", got, want)
	}
}

func TestSyncVideoMetadataLoaderUsesOneSnapshotAcrossConcurrentExactReplace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dbPath := filepath.Join(t.TempDir(), "snapshot.db")
	reader, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	writer, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	shaBytes := bytes.Repeat([]byte{0x42}, 64)
	sha := hex.EncodeToString(shaBytes)
	path := `D:\snapshot\video.mkv`
	if _, err := reader.db.ExecContext(ctx, `
		INSERT INTO files(machine_id,path,size,mtime,sha512,missing_mask)
		VALUES('m',?1,10,20,?2,?3)`, path, sha, proto.FieldVideoMetadata); err != nil {
		t.Fatal(err)
	}
	save := func(db *DB, formatName, codecName string, streamIndex int32) error {
		_, err := db.SaveAnalysis(ctx, AnalysisResult{
			MachineID: "m", Path: path, Kind: MediaVideo, Size: 10, MTime: 20,
			SHA512: shaBytes, RequestedFields: proto.FieldVideoMetadata,
			FieldsDone:     proto.FieldVideoMetadata,
			VideoContainer: &proto.VideoContainerMetadata{FormatName: formatName, TagsJSON: `{}`},
			VideoStreams: []proto.VideoStreamMetadata{{
				Index: streamIndex, MediaType: "video", CodecID: 27,
				CodecName: codecName, TagsJSON: `{}`,
			}},
		})
		return err
	}
	if err := save(reader, "old-container", "old-codec", 0); err != nil {
		t.Fatal(err)
	}
	var observed []SyncQueueRow
	for _, table := range []string{"video_containers", "video_streams"} {
		rows, err := reader.PendingSyncRows(ctx, table, 1)
		if err != nil || len(rows) != 1 {
			t.Fatalf("pending %s before replace=%#v err=%v", table, rows, err)
		}
		observed = append(observed, rows[0])
	}

	containerRead := make(chan struct{})
	continueWithStreams := make(chan struct{})
	type loadResult struct {
		rows []VideoMetadataSyncRow
		err  error
	}
	loaded := make(chan loadResult, 1)
	go func() {
		rows, err := reader.loadVideoMetadataBySHAsWithBarrier(ctx, []string{sha}, func() {
			close(containerRead)
			<-continueWithStreams
		})
		loaded <- loadResult{rows: rows, err: err}
	}()

	select {
	case <-containerRead:
	case <-ctx.Done():
		t.Fatalf("loader did not reach container/streams barrier: %v", ctx.Err())
	}
	writerDone := make(chan error, 1)
	go func() { writerDone <- save(writer, "new-container", "new-codec", 1) }()
	select {
	case err := <-writerDone:
		if err != nil {
			t.Fatalf("concurrent exact replace: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("writer could not commit while WAL reader snapshot was open: %v", ctx.Err())
	}
	close(continueWithStreams)

	var result loadResult
	select {
	case result = <-loaded:
	case <-ctx.Done():
		t.Fatalf("loader did not complete after barrier release: %v", ctx.Err())
	}
	if result.err != nil || len(result.rows) != 1 || len(result.rows[0].Streams) != 1 {
		t.Fatalf("snapshot load=%#v err=%v", result.rows, result.err)
	}
	containerName := result.rows[0].Container.FormatName
	stream := result.rows[0].Streams[0]
	oldSet := containerName == "old-container" && stream.CodecName == "old-codec" && stream.Index == 0
	newSet := containerName == "new-container" && stream.CodecName == "new-codec" && stream.Index == 1
	if !oldSet && !newSet {
		t.Fatalf("loader returned mixed snapshot: container=%q stream=%#v", containerName, stream)
	}

	if err := reader.MarkSyncBatch(ctx, observed); err != nil {
		t.Fatal(err)
	}
	for _, before := range observed {
		rows, err := reader.PendingSyncRows(ctx, before.TableName, 1)
		if err != nil || len(rows) != 1 {
			t.Fatalf("new generation %s pending=%#v err=%v", before.TableName, rows, err)
		}
		if rows[0].Generation != before.Generation+1 {
			t.Fatalf("%s generation=%d, want %d pending", before.TableName, rows[0].Generation, before.Generation+1)
		}
	}
}

func TestSyncVideoMetadataTablesParticipateInFairRoundRobin(t *testing.T) {
	db := openSyncQueueTestStore(t)
	ctx := context.Background()
	sha := strings.Repeat("9", 128)
	if _, err := db.db.ExecContext(ctx, `
		INSERT INTO sync_queue(table_name,row_pk,synced,enqueued_at,generation) VALUES
			('files','1',0,1,1),('image_features',?1,0,1,1),
			('video_features',?1,0,1,1),('video_frames',?2,0,1,1),
			('video_containers',?1,0,1,1),('video_streams',?1,0,1,1);`, sha, sha+":0"); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for round := 0; round < 6; round++ {
		rows, err := db.PendingSyncBatch(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("round %d rows=%#v", round, rows)
		}
		seen[rows[0].TableName] = true
	}
	for _, table := range []string{
		"files", "image_features", "video_features", "video_frames", "video_containers", "video_streams",
	} {
		if !seen[table] {
			t.Fatalf("fair round robin starved %s: %#v", table, seen)
		}
	}
}

func TestSyncVideoMetadataAckUsesExactObservedGeneration(t *testing.T) {
	db := openSyncQueueTestStore(t)
	ctx := context.Background()
	sha := strings.Repeat("a", 128)
	if _, err := db.db.ExecContext(ctx, `
		INSERT INTO sync_queue(table_name,row_pk,synced,enqueued_at,generation)
		VALUES('video_containers',?1,0,1,5),('video_streams',?1,0,1,7)`, sha); err != nil {
		t.Fatal(err)
	}
	observed := []SyncQueueRow{
		{TableName: "video_containers", RowPK: sha, Generation: 5},
		{TableName: "video_streams", RowPK: sha, Generation: 7},
	}
	if _, err := db.db.ExecContext(ctx, `
		UPDATE sync_queue SET generation=generation+1,synced=0 WHERE row_pk=?1`, sha); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkSyncBatch(ctx, observed); err != nil {
		t.Fatal(err)
	}
	rows, err := db.db.QueryContext(ctx, `SELECT generation,synced FROM sync_queue WHERE row_pk=?1 ORDER BY table_name`, sha)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var generation, synced int64
		if err := rows.Scan(&generation, &synced); err != nil {
			t.Fatal(err)
		}
		if synced != 0 || generation != 6 && generation != 8 {
			t.Fatalf("new metadata generation acknowledged: generation=%d synced=%d", generation, synced)
		}
	}
}

func openSyncQueueTestStore(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestPendingSyncBatchRoundRobinsSupportedTablesWithinOneLimit(t *testing.T) {
	db := openSyncQueueTestStore(t)
	ctx := context.Background()
	imageSHA := strings.Repeat("a", 128)
	videoSHA := strings.Repeat("b", 128)
	if _, err := db.db.ExecContext(ctx, `
		INSERT INTO sync_queue(table_name,row_pk,synced,enqueued_at,generation) VALUES
			('files','1',0,1,1),
			('files','2',0,2,1),
			('files','3',0,3,1),
			('image_features',?1,0,10,1),
			('video_features',?2,0,20,1);`,
		imageSHA, videoSHA,
	); err != nil {
		t.Fatal(err)
	}

	rows, err := db.PendingSyncBatch(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 ||
		rows[0].TableName != "files" ||
		rows[1].TableName != "image_features" ||
		rows[2].TableName != "video_features" {
		t.Fatalf("fair mixed rows = %#v, want one from each table", rows)
	}
}

func TestPendingSyncBatchAndLoaderIncludeExactCompleteVideoFrames(t *testing.T) {
	db := openSyncQueueTestStore(t)
	ctx := context.Background()
	imageSHA := strings.Repeat("a", 128)
	videoSHA := strings.Repeat("b", 128)
	frameSHA := strings.Repeat("c", 128)
	frameKey := frameSHA + ":2"
	incompleteKey := frameSHA + ":3"
	if _, err := db.db.ExecContext(ctx, `
		INSERT INTO files(id,machine_id,path) VALUES(1,'m','D:\mixed.bin');
		INSERT INTO image_features(sha512) VALUES(?1);
		INSERT INTO video_features(sha512) VALUES(?2);
		INSERT INTO video_frames(sha512,frame_idx,pdq256,phash_parts,sobel_hist)
		VALUES
			(?3,2,x'0102',x'0304',x'0506'),
			(?3,3,x'0708',NULL,x'090a');
		INSERT INTO sync_queue(table_name,row_pk,synced,enqueued_at,generation) VALUES
			('files','1',0,1,1),
			('image_features',?1,0,1,1),
			('video_features',?2,0,1,1),
			('video_frames',?4,0,1,1);`,
		imageSHA, videoSHA, frameSHA, frameKey,
	); err != nil {
		t.Fatal(err)
	}
	rows, err := db.PendingSyncBatch(ctx, 4)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, row := range rows {
		seen[row.TableName]++
	}
	for _, table := range []string{"files", "image_features", "video_features", "video_frames"} {
		if seen[table] != 1 {
			t.Fatalf("mixed batch=%#v, want one %s row", rows, table)
		}
	}

	frames, err := db.LoadVideoFramesByKeys(ctx, []string{frameKey, incompleteKey})
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 || frames[0].SHA512 != frameSHA || frames[0].FrameIdx != 2 ||
		string(frames[0].PDQ256) != string([]byte{1, 2}) ||
		string(frames[0].PHashParts) != string([]byte{3, 4}) ||
		string(frames[0].SobelHist) != string([]byte{5, 6}) {
		t.Fatalf("loaded complete video frames=%#v", frames)
	}
	for _, malformed := range []string{
		frameSHA, frameSHA + ":6", frameSHA + ":01", strings.ToUpper(frameSHA) + ":0",
	} {
		if _, err := db.LoadVideoFramesByKeys(ctx, []string{malformed}); err == nil {
			t.Fatalf("LoadVideoFramesByKeys accepted malformed key %q", malformed)
		}
	}
}

func TestPendingSyncBatchRotatesStartAcrossSmallBatches(t *testing.T) {
	for _, limit := range []int{1, 2} {
		t.Run(string(rune('0'+limit)), func(t *testing.T) {
			db := openSyncQueueTestStore(t)
			ctx := context.Background()
			imageSHA := strings.Repeat("a", 128)
			videoSHA := strings.Repeat("b", 128)
			if _, err := db.db.ExecContext(ctx, `
				INSERT INTO sync_queue(table_name,row_pk,synced,enqueued_at,generation) VALUES
					('files','1',0,1,1),
					('files','2',0,2,1),
					('image_features',?1,0,1,1),
					('video_features',?2,0,1,1);`,
				imageSHA, videoSHA,
			); err != nil {
				t.Fatal(err)
			}
			seen := map[string]int{}
			for round := 0; round < 6; round++ {
				rows, err := db.PendingSyncBatch(ctx, limit)
				if err != nil {
					t.Fatal(err)
				}
				for _, row := range rows {
					seen[row.TableName]++
				}
			}
			for _, table := range []string{"files", "image_features", "video_features"} {
				if seen[table] == 0 {
					t.Fatalf("limit=%d starved %s across repeated batches: %#v",
						limit, table, seen)
				}
			}
		})
	}
}

func TestMarkSyncBatchIsAtomicAndUsesExactGeneration(t *testing.T) {
	db := openSyncQueueTestStore(t)
	ctx := context.Background()
	if _, err := db.db.ExecContext(ctx, `
		INSERT INTO sync_queue(table_name,row_pk,synced,enqueued_at,generation) VALUES
			('files','1',0,1,2),
			('files','2',0,1,2);
		CREATE TRIGGER fail_second_sync_mark
		BEFORE UPDATE OF synced ON sync_queue
		WHEN OLD.row_pk='2' AND NEW.synced=1
		BEGIN SELECT RAISE(ABORT, 'mark failure'); END;`); err != nil {
		t.Fatal(err)
	}
	rows := []SyncQueueRow{
		{TableName: "files", RowPK: "1", Generation: 2},
		{TableName: "files", RowPK: "2", Generation: 2},
	}
	if err := db.MarkSyncBatch(ctx, rows); err == nil {
		t.Fatal("MarkSyncBatch error = nil, want trigger failure")
	}
	var synced int
	if err := db.db.QueryRowContext(ctx,
		`SELECT sum(synced) FROM sync_queue`).Scan(&synced); err != nil {
		t.Fatal(err)
	}
	if synced != 0 {
		t.Fatalf("synced rows after atomic failure = %d, want 0", synced)
	}

	if _, err := db.db.ExecContext(ctx, `DROP TRIGGER fail_second_sync_mark;`); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkSyncBatch(ctx, []SyncQueueRow{{
		TableName: "files", RowPK: "1", Generation: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRowContext(ctx, `
		SELECT synced FROM sync_queue WHERE table_name='files' AND row_pk='1'`,
	).Scan(&synced); err != nil {
		t.Fatal(err)
	}
	if synced != 0 {
		t.Fatalf("newer generation synced by stale observation = %d, want 0", synced)
	}
}

func TestPruneMissingSyncRowsCannotDeleteAdvancedGeneration(t *testing.T) {
	db := openSyncQueueTestStore(t)
	ctx := context.Background()
	if _, err := db.db.ExecContext(ctx, `
		INSERT INTO sync_queue(table_name,row_pk,synced,enqueued_at,generation)
		VALUES ('video_features',?1,0,1,5);`, strings.Repeat("c", 128)); err != nil {
		t.Fatal(err)
	}
	stale := SyncQueueRow{
		TableName: "video_features", RowPK: strings.Repeat("c", 128), Generation: 4,
	}
	if err := db.PruneMissingSyncRows(ctx, []SyncQueueRow{stale}); err != nil {
		t.Fatal(err)
	}
	var generation int64
	if err := db.db.QueryRowContext(ctx, `
		SELECT generation FROM sync_queue
		WHERE table_name=?1 AND row_pk=?2`,
		stale.TableName, stale.RowPK,
	).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if generation != 5 {
		t.Fatalf("generation after stale prune = %d, want 5", generation)
	}
	stale.Generation = 5
	if err := db.PruneMissingSyncRows(ctx, []SyncQueueRow{stale}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.db.QueryRowContext(ctx, `
		SELECT count(*) FROM sync_queue
		WHERE table_name=?1 AND row_pk=?2`,
		stale.TableName, stale.RowPK,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("queue rows after exact orphan prune = %d, want 0", count)
	}
}

func TestPruneMissingSyncRowsRevalidatesSourceAbsenceInTransaction(t *testing.T) {
	tests := []struct {
		name       string
		table      string
		rowPK      string
		createSQL  string
		createArgs []any
	}{
		{
			name: "files", table: "files", rowPK: "42",
			createSQL: `INSERT INTO files(id,machine_id,path) VALUES(42,'reappeared','D:\reappeared.bin');`,
		},
		{
			name: "image", table: "image_features", rowPK: strings.Repeat("d", 128),
			createSQL:  `INSERT INTO image_features(sha512) VALUES(?1);`,
			createArgs: []any{strings.Repeat("d", 128)},
		},
		{
			name: "video", table: "video_features", rowPK: strings.Repeat("e", 128),
			createSQL:  `INSERT INTO video_features(sha512) VALUES(?1);`,
			createArgs: []any{strings.Repeat("e", 128)},
		},
		{
			name: "video frame", table: "video_frames", rowPK: strings.Repeat("f", 128) + ":4",
			createSQL: `
				INSERT INTO video_frames(sha512,frame_idx,pdq256,phash_parts,sobel_hist)
				VALUES(?1,4,x'01',x'02',x'03');`,
			createArgs: []any{strings.Repeat("f", 128)},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := openSyncQueueTestStore(t)
			ctx := context.Background()
			row := SyncQueueRow{
				TableName: test.table, RowPK: test.rowPK, Generation: 1,
			}
			if _, err := db.db.ExecContext(ctx, `
				INSERT INTO sync_queue(table_name,row_pk,synced,enqueued_at,generation)
				VALUES(?1,?2,0,1,1);`, row.TableName, row.RowPK); err != nil {
				t.Fatal(err)
			}
			// Simulate a source row reappearing after the syncer loaded the
			// queue but without relying on a generation bump.
			if _, err := db.db.ExecContext(ctx, test.createSQL, test.createArgs...); err != nil {
				t.Fatal(err)
			}
			if err := db.PruneMissingSyncRows(ctx, []SyncQueueRow{row}); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := db.db.QueryRowContext(ctx, `
				SELECT count(*) FROM sync_queue
				WHERE table_name=?1 AND row_pk=?2`,
				row.TableName, row.RowPK,
			).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("queue rows after source reappeared = %d, want 1", count)
			}
		})
	}
}

func TestPruneMissingVideoFramesUsesExactKeyCompletenessAndGeneration(t *testing.T) {
	db := openSyncQueueTestStore(t)
	ctx := context.Background()
	sha := strings.Repeat("6", 128)
	missing := SyncQueueRow{
		TableName: "video_frames", RowPK: sha + ":5", Generation: 2,
	}
	incomplete := SyncQueueRow{
		TableName: "video_frames", RowPK: sha + ":4", Generation: 1,
	}
	if _, err := db.db.ExecContext(ctx, `
		INSERT INTO video_frames(sha512,frame_idx,pdq256,phash_parts,sobel_hist)
		VALUES(?1,4,x'01',NULL,x'03');
		INSERT INTO sync_queue(table_name,row_pk,synced,enqueued_at,generation) VALUES
			(?2,?3,0,1,2),
			(?4,?5,0,1,1);`,
		sha,
		missing.TableName, missing.RowPK,
		incomplete.TableName, incomplete.RowPK,
	); err != nil {
		t.Fatal(err)
	}
	stale := missing
	stale.Generation = 1
	if err := db.PruneMissingSyncRows(ctx, []SyncQueueRow{stale}); err != nil {
		t.Fatal(err)
	}
	var generation int64
	if err := db.db.QueryRowContext(ctx, `
		SELECT generation FROM sync_queue WHERE table_name=?1 AND row_pk=?2`,
		missing.TableName, missing.RowPK,
	).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if generation != 2 {
		t.Fatalf("newer missing frame generation=%d, want 2", generation)
	}
	if err := db.PruneMissingSyncRows(ctx, []SyncQueueRow{missing, incomplete}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.db.QueryRowContext(ctx, `
		SELECT count(*) FROM sync_queue WHERE table_name='video_frames'`,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("orphan/incomplete video frame queue rows=%d, want 0", count)
	}
}

func TestQuarantineSyncRowsAcceptsOnlyExactGenerationMalformedFeatures(t *testing.T) {
	db := openSyncQueueTestStore(t)
	ctx := context.Background()
	poison := SyncQueueRow{
		TableName: "image_features", RowPK: strings.Repeat("A", 128), Generation: 2,
	}
	if _, err := db.db.ExecContext(ctx, `
		INSERT INTO sync_queue(table_name,row_pk,synced,enqueued_at,generation)
		VALUES(?1,?2,0,1,?3);`,
		poison.TableName, poison.RowPK, poison.Generation,
	); err != nil {
		t.Fatal(err)
	}
	stale := poison
	stale.Generation = 1
	if err := db.QuarantineSyncRows(ctx, []SyncQueueRow{stale}); err != nil {
		t.Fatal(err)
	}
	valid := poison
	valid.RowPK = strings.Repeat("a", 128)
	if err := db.QuarantineSyncRows(ctx, []SyncQueueRow{valid}); err == nil {
		t.Fatal("valid SHA quarantine error = nil")
	}
	if err := db.QuarantineSyncRows(ctx, []SyncQueueRow{poison}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.db.QueryRowContext(ctx, `
		SELECT count(*) FROM sync_queue
		WHERE table_name=?1 AND row_pk=?2`, poison.TableName, poison.RowPK,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("poison queue rows after exact quarantine = %d, want 0", count)
	}
}

func TestQuarantineSyncRowsAcceptsOnlyMalformedCanonicalVideoFrameKeys(t *testing.T) {
	db := openSyncQueueTestStore(t)
	ctx := context.Background()
	sha := strings.Repeat("7", 128)
	malformed := []SyncQueueRow{
		{TableName: "video_frames", RowPK: sha, Generation: 1},
		{TableName: "video_frames", RowPK: sha + ":6", Generation: 2},
		{TableName: "video_frames", RowPK: sha + ":01", Generation: 3},
		{TableName: "video_frames", RowPK: strings.Repeat("A", 128) + ":0", Generation: 4},
	}
	for index, row := range malformed {
		if _, err := db.db.ExecContext(ctx, `
			INSERT INTO sync_queue(table_name,row_pk,synced,enqueued_at,generation)
			VALUES(?1,?2,0,?3,?4)`,
			row.TableName, row.RowPK, index+1, row.Generation,
		); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.QuarantineSyncRows(ctx, malformed); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sync_queue WHERE table_name='video_frames'`,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("malformed frame queue rows after quarantine=%d, want 0", count)
	}
	if err := db.QuarantineSyncRows(ctx, []SyncQueueRow{{
		TableName: "video_frames", RowPK: sha + ":5", Generation: 1,
	}}); err == nil {
		t.Fatal("valid video frame key was accepted for quarantine")
	}
}
