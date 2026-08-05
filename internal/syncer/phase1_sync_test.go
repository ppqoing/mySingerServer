package syncer

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dedup/internal/proto"
	"dedup/internal/store"
)

func TestSyncOnceUsesOneFairMixedTransactionWithinConfiguredMaximum(t *testing.T) {
	ctx := context.Background()
	local := openSyncStore(t)
	seedPhase1Image(t, local, `D:\media\image.png`, 0x11)
	seedPhase1Video(t, local, `D:\media\video.mp4`, 0x22, true)

	remote := &transactionalRemote{}
	uploader := NewWithRemote(local, remote, Config{
		Interval: time.Minute, TriggerRows: 50_000, UpsertBatch: 4,
	}, discardLogger())
	uploader.syncOnce(ctx)

	if len(remote.committed) != 1 {
		t.Fatalf("committed transactions = %d, want 1", len(remote.committed))
	}
	batch := remote.committed[0]
	if got := len(batch.files) + len(batch.images) + len(batch.videos); got != 4 {
		t.Fatalf("mixed transaction rows = %d, want 4", got)
	}
	if len(batch.files) != 2 || len(batch.images) != 1 || len(batch.videos) != 1 {
		t.Fatalf("mixed transaction = files:%d images:%d videos:%d, want 2/1/1",
			len(batch.files), len(batch.images), len(batch.videos))
	}
	if pending, err := local.PendingSyncCount(ctx); err != nil || pending != 0 {
		t.Fatalf("pending after mixed commit = %d, err=%v; want 0", pending, err)
	}
}

func TestSyncOnceCapsOversizedConfigurationAtFiveThousand(t *testing.T) {
	rows := make([]store.SyncQueueRow, 5_001)
	files := make([]store.FileRow, len(rows))
	for index := range rows {
		rows[index] = store.SyncQueueRow{
			TableName: "files", RowPK: stringID(index + 1), Generation: 1,
		}
		files[index] = store.FileRow{ID: int64(index + 1), MachineID: "cap", Path: stringID(index + 1)}
	}
	local := &scriptedLocal{pending: rows, files: files}
	remote := &transactionalRemote{failBeginAt: 2}
	uploader := &Syncer{
		local: local, remote: remote, cfg: Config{UpsertBatch: 99_999}, log: discardLogger(),
	}
	uploader.syncOnce(context.Background())

	if len(remote.committed) != 1 || len(remote.committed[0].files) != 5_000 {
		t.Fatalf("first remote transaction rows = %#v, want exactly 5000",
			remote.committed)
	}
	if local.requestedLimits[0] != 5_000 {
		t.Fatalf("local requested limit = %d, want 5000", local.requestedLimits[0])
	}
}

func TestSyncOnceRollsBackAndRetriesEveryRemoteFailureBoundary(t *testing.T) {
	stages := []string{"begin", "files", "images", "videos", "close", "commit"}
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			local := openSyncStore(t)
			seedPhase1Image(t, local, `D:\media\image.png`, 0x31)
			seedPhase1Video(t, local, `D:\media\video.mp4`, 0x32, true)

			remote := &transactionalRemote{failStage: stage}
			uploader := NewWithRemote(local, remote, Config{UpsertBatch: 10}, discardLogger())
			uploader.syncOnce(ctx)
			if pending, err := local.PendingSyncCount(ctx); err != nil || pending != 4 {
				t.Fatalf("pending after %s failure = %d, err=%v; want 4", stage, pending, err)
			}
			if stage != "begin" && remote.rollbacks != 1 {
				t.Fatalf("rollbacks after %s failure = %d, want 1", stage, remote.rollbacks)
			}

			remote.failStage = ""
			uploader.syncOnce(ctx)
			if pending, err := local.PendingSyncCount(ctx); err != nil || pending != 0 {
				t.Fatalf("pending after %s retry = %d, err=%v; want 0", stage, pending, err)
			}
		})
	}
}

func TestSyncOnceGenerationRaceDuringCommitLeavesNewGenerationPending(t *testing.T) {
	ctx := context.Background()
	local := openSyncStore(t)
	seedFile(t, local, `D:\media\race.bin`, "old-hash")

	remote := &transactionalRemote{failBeginAt: 2}
	remote.commitHook = func() {
		if err := local.ApplyHashResults(ctx, "machine-a", []store.HashResult{{
			Path: `D:\media\race.bin`, SHA512: "new-hash",
		}}); err != nil {
			t.Fatalf("advance generation: %v", err)
		}
	}
	uploader := NewWithRemote(local, remote, Config{UpsertBatch: 10}, discardLogger())
	uploader.syncOnce(ctx)

	if pending, err := local.PendingSyncCount(ctx); err != nil || pending != 1 {
		t.Fatalf("pending newer generation = %d, err=%v; want 1", pending, err)
	}
}

func TestSyncOnceCommitSuccessThenLocalMarkFailureResendsIdempotently(t *testing.T) {
	ctx := context.Background()
	db := openSyncStore(t)
	seedFile(t, db, `D:\media\resend.bin`, "hash")
	local := &markFailLocal{DB: db, failures: 1}
	remote := &transactionalRemote{}
	uploader := &Syncer{
		local: local, remote: remote, cfg: Config{UpsertBatch: 10}, log: discardLogger(),
	}

	uploader.syncOnce(ctx)
	if len(remote.committed) != 1 {
		t.Fatalf("commits after local mark failure = %d, want 1", len(remote.committed))
	}
	uploader.syncOnce(ctx)
	if len(remote.committed) != 2 {
		t.Fatalf("commits after retry = %d, want 2", len(remote.committed))
	}
	if pending, err := db.PendingSyncCount(ctx); err != nil || pending != 0 {
		t.Fatalf("pending after idempotent resend = %d, err=%v; want 0", pending, err)
	}
}

func TestSyncOnceRejectsMalformedFeatureSHAWithoutAcknowledging(t *testing.T) {
	row := store.SyncQueueRow{TableName: "image_features", RowPK: strings.Repeat("A", 128), Generation: 7}
	local := &scriptedLocal{
		pending: []store.SyncQueueRow{row},
		images:  []store.ImageFeatureSyncRow{{SHA512: row.RowPK}},
	}
	remote := &transactionalRemote{}
	uploader := &Syncer{
		local: local, remote: remote, cfg: Config{UpsertBatch: 10}, log: discardLogger(),
	}
	uploader.syncOnce(context.Background())

	if len(remote.committed) != 0 || len(local.marked) != 0 || len(local.pruned) != 0 {
		t.Fatalf("malformed SHA was processed: committed=%d marked=%v pruned=%v",
			len(remote.committed), local.marked, local.pruned)
	}
}

func TestSyncOnceQuarantinesPoisonSHAAndProgressesAtBatchSizeOne(t *testing.T) {
	poison := store.SyncQueueRow{
		TableName: "image_features", RowPK: strings.Repeat("A", 128), Generation: 9,
	}
	valid := store.SyncQueueRow{
		TableName: "video_features", RowPK: strings.Repeat("d", 128), Generation: 3,
	}
	local := &scriptedLocal{
		pending: []store.SyncQueueRow{poison, valid},
		videos:  []store.VideoFeatureSyncRow{{SHA512: valid.RowPK}},
	}
	remote := &transactionalRemote{}
	var logs bytes.Buffer
	uploader := &Syncer{
		local: local, remote: remote, cfg: Config{UpsertBatch: 1},
		log: slog.New(slog.NewJSONHandler(&logs, nil)),
	}
	uploader.syncOnce(context.Background())

	if len(local.quarantined) != 1 || local.quarantined[0] != poison {
		t.Fatalf("quarantined = %#v, want exact poison generation", local.quarantined)
	}
	if len(remote.committed) != 1 ||
		len(remote.committed[0].videos) != 1 ||
		remote.committed[0].videos[0].SHA512 != valid.RowPK {
		t.Fatalf("valid row did not progress after poison: %#v", remote.committed)
	}
	for _, row := range local.marked {
		if row == poison {
			t.Fatal("poison row was acknowledged as remotely synced")
		}
	}
	logText := logs.String()
	if !strings.Contains(logText, "sync: quarantine malformed feature SHA") ||
		!strings.Contains(logText, `"generation":9`) ||
		!strings.Contains(logText, poison.RowPK) {
		t.Fatalf("quarantine diagnostic = %s", logText)
	}
}

func TestSyncOncePrunesOnlyObservedGenerationForMissingSourceRows(t *testing.T) {
	missing := store.SyncQueueRow{TableName: "video_features", RowPK: strings.Repeat("a", 128), Generation: 4}
	local := &scriptedLocal{pending: []store.SyncQueueRow{missing}}
	remote := &transactionalRemote{}
	uploader := &Syncer{
		local: local, remote: remote, cfg: Config{UpsertBatch: 10}, log: discardLogger(),
	}
	uploader.syncOnce(context.Background())

	if len(local.pruned) != 1 || local.pruned[0].Generation != 4 {
		t.Fatalf("pruned rows = %#v, want exact missing generation", local.pruned)
	}
	if len(local.marked) != 0 || len(remote.committed) != 0 {
		t.Fatalf("missing source was acknowledged/uploaded: marked=%v commits=%d",
			local.marked, len(remote.committed))
	}
}

func TestSyncOnceDoesNotStarveFeatureTablesWhenFilesAreReplenished(t *testing.T) {
	rows := []store.SyncQueueRow{
		{TableName: "files", RowPK: "1", Generation: 1},
		{TableName: "image_features", RowPK: strings.Repeat("b", 128), Generation: 1},
		{TableName: "video_features", RowPK: strings.Repeat("c", 128), Generation: 1},
	}
	local := &scriptedLocal{
		pending:        rows,
		files:          []store.FileRow{{ID: 1, MachineID: "fair", Path: "file"}},
		images:         []store.ImageFeatureSyncRow{{SHA512: rows[1].RowPK}},
		videos:         []store.VideoFeatureSyncRow{{SHA512: rows[2].RowPK}},
		replenishFiles: true,
	}
	remote := &transactionalRemote{failBeginAt: 2}
	uploader := &Syncer{
		local: local, remote: remote, cfg: Config{UpsertBatch: 3}, log: discardLogger(),
	}
	uploader.syncOnce(context.Background())

	if len(remote.committed) != 1 {
		t.Fatalf("commits = %d, want 1", len(remote.committed))
	}
	batch := remote.committed[0]
	if len(batch.files) != 1 || len(batch.images) != 1 || len(batch.videos) != 1 {
		t.Fatalf("fair batch = files:%d images:%d videos:%d", len(batch.files), len(batch.images), len(batch.videos))
	}
}

func TestRemoteRollbackUsesIndependentBoundedContextAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	remote := &cancelDuringUpsertRemote{cancel: cancel}
	uploader := &Syncer{remote: remote}
	err := uploader.commitRemoteBatch(ctx, loadedBatch{
		files: []store.FileRow{{ID: 1}},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("commitRemoteBatch error = %v, want context.Canceled", err)
	}
	if remote.rollbackContextErr != nil {
		t.Fatalf("rollback context error = %v, want independent live context",
			remote.rollbackContextErr)
	}
	if !remote.rollbackHadDeadline {
		t.Fatal("rollback context had no bounded deadline")
	}
}

func openSyncStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedFile(t *testing.T, local *store.DB, path, sha string) {
	t.Helper()
	if err := local.UpsertEnumerated(context.Background(), []store.EnumUpsert{{
		MachineID: "machine-a", DiskNo: 1, Path: path, Size: 1, MTime: 1,
		MissingBase: proto.FieldSHA512,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := local.ApplyHashResults(context.Background(), "machine-a", []store.HashResult{{
		Path: path, SHA512: sha,
	}}); err != nil {
		t.Fatal(err)
	}
}

func seedPhase1Image(t *testing.T, local *store.DB, path string, fill byte) {
	t.Helper()
	sha := make([]byte, 64)
	for index := range sha {
		sha[index] = fill
	}
	if err := local.UpsertEnumerated(context.Background(), []store.EnumUpsert{{
		MachineID: "machine-a", DiskNo: 1, Path: path, Size: 1, MTime: 1,
		MissingBase: proto.FieldSHA512 | proto.FieldPDQ256,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := local.SavePhase1(context.Background(), store.Phase1Result{
		MachineID: "machine-a", Path: path, Kind: store.MediaImage,
		SHA512: sha, FieldsDone: proto.FieldSHA512 | proto.FieldPDQ256,
		PDQ: []byte{1, 2, 3}, Quality: 88, Width: 640, Height: 480,
	}); err != nil {
		t.Fatal(err)
	}
}

func seedPhase1Video(t *testing.T, local *store.DB, path string, fill byte, complete bool) {
	t.Helper()
	sha := make([]byte, 64)
	for index := range sha {
		sha[index] = fill
	}
	if err := local.UpsertEnumerated(context.Background(), []store.EnumUpsert{{
		MachineID: "machine-a", DiskNo: 1, Path: path, Size: 1, MTime: 1,
		MissingBase: proto.FieldSHA512 | proto.FieldThumb,
	}}); err != nil {
		t.Fatal(err)
	}
	duration := int64(5_000)
	quality := int32(77)
	result := store.Phase1Result{
		MachineID: "machine-a", Path: path, Kind: store.MediaVideo,
		SHA512: sha, FieldsDone: proto.FieldSHA512 | proto.FieldThumb,
		DurationMS: &duration,
	}
	if complete {
		result.ThumbPath = `D:\cache\thumb.jpg`
		result.ThumbPDQ = []byte{4, 5, 6}
		result.ThumbQuality = &quality
	}
	if err := local.SavePhase1(context.Background(), result); err != nil {
		t.Fatal(err)
	}
}

type recordedTx struct {
	files  []store.FileRow
	images []store.ImageFeatureSyncRow
	videos []store.VideoFeatureSyncRow
	frames []store.VideoFrameSyncRow
}

type transactionalRemote struct {
	begins      int
	failBeginAt int
	failStage   string
	rollbacks   int
	committed   []recordedTx
	commitHook  func()
}

func (r *transactionalRemote) Begin(context.Context) (RemoteTx, error) {
	r.begins++
	if r.failStage == "begin" || r.failBeginAt == r.begins {
		return nil, errors.New("begin failed")
	}
	return &transactionalRemoteTx{owner: r}, nil
}

type transactionalRemoteTx struct {
	owner transactionalRemoteOwner
	rows  recordedTx
}

type cancelDuringUpsertRemote struct {
	cancel              context.CancelFunc
	rollbackContextErr  error
	rollbackHadDeadline bool
}

func (r *cancelDuringUpsertRemote) Begin(context.Context) (RemoteTx, error) {
	return &cancelDuringUpsertTx{owner: r}, nil
}

type cancelDuringUpsertTx struct {
	owner *cancelDuringUpsertRemote
}

func (tx *cancelDuringUpsertTx) UpsertFiles(context.Context, []store.FileRow) error {
	tx.owner.cancel()
	return context.Canceled
}

func (*cancelDuringUpsertTx) UpsertImages(context.Context, []store.ImageFeatureSyncRow) error {
	return nil
}

func (*cancelDuringUpsertTx) UpsertVideos(context.Context, []store.VideoFeatureSyncRow) error {
	return nil
}

func (*cancelDuringUpsertTx) UpsertFrames(context.Context, []store.VideoFrameSyncRow) error {
	return nil
}

func (*cancelDuringUpsertTx) CloseBatch(context.Context) error { return nil }
func (*cancelDuringUpsertTx) Commit(context.Context) error     { return nil }

func (tx *cancelDuringUpsertTx) Rollback(ctx context.Context) error {
	tx.owner.rollbackContextErr = ctx.Err()
	_, tx.owner.rollbackHadDeadline = ctx.Deadline()
	return nil
}

type transactionalRemoteOwner interface {
	fail(stage string) error
	rollback()
	commit(recordedTx) error
}

func (r *transactionalRemote) fail(stage string) error {
	if r.failStage == stage {
		return errors.New(stage + " failed")
	}
	return nil
}

func (r *transactionalRemote) rollback() { r.rollbacks++ }

func (r *transactionalRemote) commit(rows recordedTx) error {
	if err := r.fail("commit"); err != nil {
		return err
	}
	if r.commitHook != nil {
		r.commitHook()
	}
	r.committed = append(r.committed, rows)
	return nil
}

func (tx *transactionalRemoteTx) UpsertFiles(_ context.Context, rows []store.FileRow) error {
	if err := tx.owner.fail("files"); err != nil {
		return err
	}
	tx.rows.files = append(tx.rows.files, rows...)
	return nil
}

func (tx *transactionalRemoteTx) UpsertImages(_ context.Context, rows []store.ImageFeatureSyncRow) error {
	if err := tx.owner.fail("images"); err != nil {
		return err
	}
	tx.rows.images = append(tx.rows.images, rows...)
	return nil
}

func (tx *transactionalRemoteTx) UpsertVideos(_ context.Context, rows []store.VideoFeatureSyncRow) error {
	if err := tx.owner.fail("videos"); err != nil {
		return err
	}
	tx.rows.videos = append(tx.rows.videos, rows...)
	return nil
}

func (tx *transactionalRemoteTx) UpsertFrames(_ context.Context, rows []store.VideoFrameSyncRow) error {
	if err := tx.owner.fail("frames"); err != nil {
		return err
	}
	tx.rows.frames = append(tx.rows.frames, rows...)
	return nil
}

func (tx *transactionalRemoteTx) CloseBatch(context.Context) error {
	return tx.owner.fail("close")
}

func (tx *transactionalRemoteTx) Commit(context.Context) error {
	return tx.owner.commit(tx.rows)
}

func (tx *transactionalRemoteTx) Rollback(context.Context) error {
	tx.owner.rollback()
	return nil
}

type markFailLocal struct {
	*store.DB
	failures int
}

func (l *markFailLocal) MarkSyncBatch(ctx context.Context, rows []store.SyncQueueRow) error {
	if l.failures > 0 {
		l.failures--
		return errors.New("local mark failed")
	}
	return l.DB.MarkSyncBatch(ctx, rows)
}

type scriptedLocal struct {
	pending         []store.SyncQueueRow
	files           []store.FileRow
	images          []store.ImageFeatureSyncRow
	videos          []store.VideoFeatureSyncRow
	frames          []store.VideoFrameSyncRow
	marked          []store.SyncQueueRow
	pruned          []store.SyncQueueRow
	quarantined     []store.SyncQueueRow
	requestedLimits []int
	replenishFiles  bool
}

func (l *scriptedLocal) PendingSyncBatch(_ context.Context, limit int) ([]store.SyncQueueRow, error) {
	l.requestedLimits = append(l.requestedLimits, limit)
	if len(l.pending) == 0 {
		return nil, nil
	}
	count := limit
	if count > len(l.pending) {
		count = len(l.pending)
	}
	rows := append([]store.SyncQueueRow(nil), l.pending[:count]...)
	if !l.replenishFiles {
		l.pending = l.pending[count:]
	}
	return rows, nil
}

func (l *scriptedLocal) PendingSyncCount(context.Context) (int64, error) {
	return int64(len(l.pending)), nil
}

func (l *scriptedLocal) LoadFilesByIDs(context.Context, []string) ([]store.FileRow, error) {
	return append([]store.FileRow(nil), l.files...), nil
}

func (l *scriptedLocal) LoadImageFeaturesBySHAs(context.Context, []string) ([]store.ImageFeatureSyncRow, error) {
	return append([]store.ImageFeatureSyncRow(nil), l.images...), nil
}

func (l *scriptedLocal) LoadVideoFeaturesBySHAs(context.Context, []string) ([]store.VideoFeatureSyncRow, error) {
	return append([]store.VideoFeatureSyncRow(nil), l.videos...), nil
}

func (l *scriptedLocal) LoadVideoFramesByKeys(context.Context, []string) ([]store.VideoFrameSyncRow, error) {
	return append([]store.VideoFrameSyncRow(nil), l.frames...), nil
}

func (l *scriptedLocal) MarkSyncBatch(_ context.Context, rows []store.SyncQueueRow) error {
	l.marked = append(l.marked, rows...)
	if !l.replenishFiles {
		return nil
	}
	remaining := l.pending[:0]
	for _, pending := range l.pending {
		matched := false
		for _, marked := range rows {
			if pending.TableName == marked.TableName && pending.RowPK == marked.RowPK &&
				pending.Generation == marked.Generation {
				matched = true
				break
			}
		}
		if !matched || pending.TableName == "files" {
			remaining = append(remaining, pending)
		}
	}
	l.pending = remaining
	return nil
}

func (l *scriptedLocal) PruneMissingSyncRows(_ context.Context, rows []store.SyncQueueRow) error {
	l.pruned = append(l.pruned, rows...)
	l.pending = nil
	return nil
}

func (l *scriptedLocal) QuarantineSyncRows(_ context.Context, rows []store.SyncQueueRow) error {
	l.quarantined = append(l.quarantined, rows...)
	return nil
}

func stringID(value int) string {
	const digits = "0123456789"
	if value == 0 {
		return "0"
	}
	var reversed [32]byte
	index := len(reversed)
	for value > 0 {
		index--
		reversed[index] = digits[value%10]
		value /= 10
	}
	return string(reversed[index:])
}
