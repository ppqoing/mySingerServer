package syncer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"dedup/internal/features"
	"dedup/internal/proto"
	"dedup/internal/store"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSyncOnceCommitsFilesImagesVideosAndFramesInOneMixedTransaction(t *testing.T) {
	ctx := context.Background()
	local := openSyncStore(t)
	seedPhase1Image(t, local, `D:\mixed\image.jpg`, 0x21)
	seedPhase1Video(t, local, `D:\mixed\video.mp4`, 0x22, true)
	videoSHA := bytes.Repeat([]byte{0x22}, 64)
	if err := local.SavePhase2(ctx, store.Phase2Result{
		MachineID: "machine-a", Path: `D:\mixed\video.mp4`,
		Kind: store.MediaVideo, SHA512: videoSHA,
		FieldsDone: proto.FieldVideo6F,
		Frames:     phase2StoreFrames(t, 0, 6),
	}); err != nil {
		t.Fatal(err)
	}
	remote := &phase2RecordingRemote{}
	uploader := NewWithRemote(local, remote, Config{UpsertBatch: 32}, discardLogger())
	uploader.syncOnce(ctx)

	if len(remote.committed) != 1 {
		t.Fatalf("remote commits=%d, want one mixed transaction", len(remote.committed))
	}
	batch := remote.committed[0]
	if len(batch.files) != 2 || len(batch.images) != 1 ||
		len(batch.videos) != 1 || len(batch.frames) != 6 {
		t.Fatalf("mixed commit files=%d images=%d videos=%d frames=%d",
			len(batch.files), len(batch.images), len(batch.videos), len(batch.frames))
	}
	for index, frame := range batch.frames {
		if frame.SHA512 != strings.Repeat("22", 64) || frame.FrameIdx != index ||
			len(frame.PDQ256) != 32 || len(frame.PHashParts) == 0 || len(frame.SobelHist) == 0 {
			t.Fatalf("remote frame[%d]=%#v", index, frame)
		}
	}
	if pending, err := local.PendingSyncCount(ctx); err != nil || pending != 0 {
		t.Fatalf("pending after mixed commit=%d err=%v, want 0", pending, err)
	}
}

func TestSyncOnceFrameRemoteFailureRollsBackLeavesGenerationAndRetries(t *testing.T) {
	ctx := context.Background()
	local := openSyncStore(t)
	path := `D:\retry\video.mp4`
	seedPhase1Video(t, local, path, 0x31, true)
	baseline := &phase2RecordingRemote{}
	NewWithRemote(local, baseline, Config{UpsertBatch: 32}, discardLogger()).syncOnce(ctx)
	if pending, err := local.PendingSyncCount(ctx); err != nil || pending != 0 {
		t.Fatalf("baseline pending=%d err=%v", pending, err)
	}

	sha := bytes.Repeat([]byte{0x31}, 64)
	if err := local.SavePhase2(ctx, store.Phase2Result{
		MachineID: "machine-a", Path: path, Kind: store.MediaVideo, SHA512: sha,
		Frames: phase2StoreFrames(t, 4, 1),
	}); err != nil {
		t.Fatal(err)
	}
	frameRows, err := local.PendingSyncRows(ctx, "video_frames", 10)
	if err != nil || len(frameRows) != 1 {
		t.Fatalf("pending frame rows=%#v err=%v", frameRows, err)
	}
	observedGeneration := frameRows[0].Generation
	remote := &phase2RecordingRemote{failFrames: true}
	uploader := NewWithRemote(local, remote, Config{UpsertBatch: 32}, discardLogger())
	uploader.syncOnce(ctx)

	if len(remote.committed) != 0 || remote.rollbacks != 1 {
		t.Fatalf("failed frame transaction commits=%d rollbacks=%d, want 0/1",
			len(remote.committed), remote.rollbacks)
	}
	frameRows, err = local.PendingSyncRows(ctx, "video_frames", 10)
	if err != nil || len(frameRows) != 1 || frameRows[0].Generation != observedGeneration {
		t.Fatalf("frame generation after rollback=%#v err=%v, want pending @%d",
			frameRows, err, observedGeneration)
	}

	remote.failFrames = false
	uploader.syncOnce(ctx)
	if len(remote.committed) != 1 || len(remote.committed[0].frames) != 1 {
		t.Fatalf("retry commits=%#v, want one frame transaction", remote.committed)
	}
	if pending, err := local.PendingSyncCount(ctx); err != nil || pending != 0 {
		t.Fatalf("pending after retry=%d err=%v, want 0", pending, err)
	}
}

func TestSyncOnceQuarantinesMalformedFrameKeyAndPrunesMissingExactGeneration(t *testing.T) {
	sha := strings.Repeat("a", 128)
	poison := store.SyncQueueRow{
		TableName: "video_frames", RowPK: strings.ToUpper(sha) + ":0", Generation: 7,
	}
	missing := store.SyncQueueRow{
		TableName: "video_frames", RowPK: sha + ":3", Generation: 9,
	}
	local := &phase2ScriptedLocal{pending: []store.SyncQueueRow{poison, missing}}
	remote := &phase2RecordingRemote{}
	uploader := &Syncer{
		local: local, remote: remote,
		cfg: Config{UpsertBatch: 1}, log: discardLogger(),
	}
	uploader.syncOnce(context.Background())

	if len(local.quarantined) != 1 || local.quarantined[0] != poison {
		t.Fatalf("quarantined=%#v, want exact malformed frame generation", local.quarantined)
	}
	if len(local.pruned) != 1 || local.pruned[0] != missing ||
		local.pruned[0].Generation != 9 {
		t.Fatalf("pruned=%#v, want exact missing frame generation", local.pruned)
	}
	if len(remote.committed) != 0 || len(local.marked) != 0 {
		t.Fatalf("poison/missing frames remotely processed commits=%d marked=%#v",
			len(remote.committed), local.marked)
	}
}

func TestPGRemoteQueuesCompositeKeyFrameReplacement(t *testing.T) {
	tx := &pgRemoteTx{}
	row := store.VideoFrameSyncRow{
		SHA512: strings.Repeat("5", 128), FrameIdx: 4,
		PDQ256: []byte{1}, PHashParts: []byte{2}, SobelHist: []byte{3},
	}
	if err := tx.UpsertFrames(context.Background(), []store.VideoFrameSyncRow{row}); err != nil {
		t.Fatal(err)
	}
	if tx.commands != 1 || tx.batch.Len() != 1 {
		t.Fatalf("queued frame commands=%d batch=%d, want 1/1", tx.commands, tx.batch.Len())
	}
	query := tx.batch.QueuedQueries[0]
	for _, fragment := range []string{
		"ON CONFLICT (sha512, frame_idx) DO UPDATE SET",
		"pdq256 = EXCLUDED.pdq256",
		"phash_parts = EXCLUDED.phash_parts",
		"sobel_hist = EXCLUDED.sobel_hist",
	} {
		if !strings.Contains(query.SQL, fragment) {
			t.Fatalf("frame UPSERT missing %q: %s", fragment, query.SQL)
		}
	}
	wantArgs := []any{row.SHA512, row.FrameIdx, row.PDQ256, row.PHashParts, row.SobelHist}
	if len(query.Arguments) != len(wantArgs) {
		t.Fatalf("frame UPSERT args=%#v", query.Arguments)
	}
	for index := range wantArgs {
		switch want := wantArgs[index].(type) {
		case []byte:
			got, ok := query.Arguments[index].([]byte)
			if !ok || !bytes.Equal(got, want) {
				t.Fatalf("frame UPSERT arg[%d]=%#v, want %x", index, query.Arguments[index], want)
			}
		default:
			if query.Arguments[index] != want {
				t.Fatalf("frame UPSERT arg[%d]=%#v, want %#v", index, query.Arguments[index], want)
			}
		}
	}
}

func TestPGRemoteReplacesAllFrameColumnsWhenIntegrationEnabled(t *testing.T) {
	t.Parallel()
	dsn := os.Getenv("FS_PG_DSN")
	if dsn == "" {
		t.Skip("set FS_PG_DSN to run PostgreSQL integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	sha := derivePGSHA(uniquePGToken(t), "phase2-frame")
	t.Cleanup(func() {
		cleanupPGRows(t, pool, `DELETE FROM video_frames WHERE sha512=$1`, sha)
	})
	remote := &PGRemote{pool: pool}
	upsert := func(row store.VideoFrameSyncRow) {
		t.Helper()
		tx, err := remote.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err := tx.UpsertFrames(ctx, []store.VideoFrameSyncRow{row}); err != nil {
			t.Fatal(err)
		}
		if err := tx.CloseBatch(ctx); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	upsert(store.VideoFrameSyncRow{
		SHA512: sha, FrameIdx: 2,
		PDQ256: []byte{1}, PHashParts: []byte{2}, SobelHist: []byte{3},
	})
	replacement := store.VideoFrameSyncRow{
		SHA512: sha, FrameIdx: 2,
		PDQ256: []byte{4}, PHashParts: []byte{5}, SobelHist: []byte{6},
	}
	upsert(replacement)
	upsert(replacement)

	var pdq, pHash, sobel []byte
	if err := pool.QueryRow(ctx, `
		SELECT pdq256, phash_parts, sobel_hist
		FROM video_frames WHERE sha512=$1 AND frame_idx=$2`,
		sha,
		2,
	).Scan(&pdq, &pHash, &sobel); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pdq, replacement.PDQ256) ||
		!bytes.Equal(pHash, replacement.PHashParts) ||
		!bytes.Equal(sobel, replacement.SobelHist) {
		t.Fatalf("remote replacement pdq=%x phash=%x sobel=%x", pdq, pHash, sobel)
	}
}

func phase2StoreFrames(t *testing.T, start, count int) []store.Phase2Frame {
	t.Helper()
	frames := make([]store.Phase2Frame, 0, count)
	for index := start; index < start+count; index++ {
		var parts [9]uint64
		parts[0] = uint64(index + 1)
		pHash := features.EncodePHashParts(parts)
		var histogram [128]float32
		histogram[0] = float32(index + 1)
		sobel, err := features.EncodeSobelHist(histogram)
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, store.Phase2Frame{
			FrameIdx: index, PDQ256: bytes.Repeat([]byte{byte(index + 1)}, 32),
			Quality: 80, PHashParts: pHash, SobelHist: sobel,
		})
	}
	return frames
}

type phase2RecordedBatch struct {
	files  []store.FileRow
	images []store.ImageFeatureSyncRow
	videos []store.VideoFeatureSyncRow
	frames []store.VideoFrameSyncRow
}

type phase2RecordingRemote struct {
	failFrames bool
	rollbacks  int
	committed  []phase2RecordedBatch
}

func (remote *phase2RecordingRemote) Begin(context.Context) (RemoteTx, error) {
	return &phase2RecordingTx{owner: remote}, nil
}

type phase2RecordingTx struct {
	owner *phase2RecordingRemote
	rows  phase2RecordedBatch
}

func (tx *phase2RecordingTx) UpsertFiles(_ context.Context, rows []store.FileRow) error {
	tx.rows.files = append(tx.rows.files, rows...)
	return nil
}

func (tx *phase2RecordingTx) UpsertImages(_ context.Context, rows []store.ImageFeatureSyncRow) error {
	tx.rows.images = append(tx.rows.images, rows...)
	return nil
}

func (tx *phase2RecordingTx) UpsertVideos(_ context.Context, rows []store.VideoFeatureSyncRow) error {
	tx.rows.videos = append(tx.rows.videos, rows...)
	return nil
}

func (tx *phase2RecordingTx) UpsertFrames(_ context.Context, rows []store.VideoFrameSyncRow) error {
	if tx.owner.failFrames {
		return errors.New("frames failed")
	}
	tx.rows.frames = append(tx.rows.frames, rows...)
	return nil
}

func (*phase2RecordingTx) CloseBatch(context.Context) error { return nil }

func (tx *phase2RecordingTx) Commit(context.Context) error {
	tx.owner.committed = append(tx.owner.committed, tx.rows)
	return nil
}

func (tx *phase2RecordingTx) Rollback(context.Context) error {
	tx.owner.rollbacks++
	return nil
}

type phase2ScriptedLocal struct {
	pending     []store.SyncQueueRow
	frames      []store.VideoFrameSyncRow
	marked      []store.SyncQueueRow
	pruned      []store.SyncQueueRow
	quarantined []store.SyncQueueRow
}

func (local *phase2ScriptedLocal) PendingSyncBatch(
	_ context.Context,
	limit int,
) ([]store.SyncQueueRow, error) {
	if limit > len(local.pending) {
		limit = len(local.pending)
	}
	return append([]store.SyncQueueRow(nil), local.pending[:limit]...), nil
}

func (local *phase2ScriptedLocal) PendingSyncCount(context.Context) (int64, error) {
	return int64(len(local.pending)), nil
}

func (*phase2ScriptedLocal) LoadFilesByIDs(context.Context, []string) ([]store.FileRow, error) {
	return nil, nil
}

func (*phase2ScriptedLocal) LoadImageFeaturesBySHAs(
	context.Context,
	[]string,
) ([]store.ImageFeatureSyncRow, error) {
	return nil, nil
}

func (*phase2ScriptedLocal) LoadVideoFeaturesBySHAs(
	context.Context,
	[]string,
) ([]store.VideoFeatureSyncRow, error) {
	return nil, nil
}

func (local *phase2ScriptedLocal) LoadVideoFramesByKeys(
	context.Context,
	[]string,
) ([]store.VideoFrameSyncRow, error) {
	return append([]store.VideoFrameSyncRow(nil), local.frames...), nil
}

func (local *phase2ScriptedLocal) MarkSyncBatch(
	_ context.Context,
	rows []store.SyncQueueRow,
) error {
	local.marked = append(local.marked, rows...)
	local.remove(rows)
	return nil
}

func (local *phase2ScriptedLocal) PruneMissingSyncRows(
	_ context.Context,
	rows []store.SyncQueueRow,
) error {
	local.pruned = append(local.pruned, rows...)
	local.remove(rows)
	return nil
}

func (local *phase2ScriptedLocal) QuarantineSyncRows(
	_ context.Context,
	rows []store.SyncQueueRow,
) error {
	local.quarantined = append(local.quarantined, rows...)
	local.remove(rows)
	return nil
}

func (local *phase2ScriptedLocal) remove(rows []store.SyncQueueRow) {
	remaining := local.pending[:0]
	for _, pending := range local.pending {
		remove := false
		for _, row := range rows {
			if pending == row {
				remove = true
				break
			}
		}
		if !remove {
			remaining = append(remaining, pending)
		}
	}
	local.pending = remaining
}
