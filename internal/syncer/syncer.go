package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"dedup/internal/store"
)

const maxRemoteBatch = 5_000

type Config struct {
	Interval    time.Duration
	TriggerRows int64
	UpsertBatch int
	OnHealth    HealthCallback
}

// HealthUpdate is emitted after a terminal synchronization failure and after
// a complete successful or healthy no-op pass. Handled poison rows do not make
// the syncer unhealthy when quarantine succeeds.
type HealthUpdate struct {
	Healthy      bool
	ErrorSummary string
}

type HealthCallback func(HealthUpdate)

type Remote interface {
	Begin(ctx context.Context) (RemoteTx, error)
}

type RemoteTx interface {
	UpsertFiles(ctx context.Context, rows []store.FileRow) error
	UpsertImages(ctx context.Context, rows []store.ImageFeatureSyncRow) error
	UpsertVideos(ctx context.Context, rows []store.VideoFeatureSyncRow) error
	UpsertFrames(ctx context.Context, rows []store.VideoFrameSyncRow) error
	UpsertLocal(ctx context.Context, batch store.LocalSyncBatch) error
	CloseBatch(ctx context.Context) error
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

type syncStore interface {
	PendingSyncBatch(ctx context.Context, limit int) ([]store.SyncQueueRow, error)
	PendingSyncCount(ctx context.Context) (int64, error)
	LoadFilesByIDs(ctx context.Context, ids []string) ([]store.FileRow, error)
	LoadImageFeaturesBySHAs(ctx context.Context, shas []string) ([]store.ImageFeatureSyncRow, error)
	LoadVideoFeaturesBySHAs(ctx context.Context, shas []string) ([]store.VideoFeatureSyncRow, error)
	LoadVideoFramesByKeys(ctx context.Context, keys []string) ([]store.VideoFrameSyncRow, error)
	MarkSyncBatch(ctx context.Context, rows []store.SyncQueueRow) error
	PruneMissingSyncRows(ctx context.Context, rows []store.SyncQueueRow) error
	QuarantineSyncRows(ctx context.Context, rows []store.SyncQueueRow) error
	PendingLocalSyncEvents(ctx context.Context, limit int) ([]store.LocalOutboxSyncRow, error)
	LoadLocalSyncBatch(ctx context.Context, events []store.LocalOutboxSyncRow) (store.LocalSyncBatch, error)
	AcknowledgeLocalSyncEvents(ctx context.Context, events []store.LocalOutboxSyncRow) error
}

type Syncer struct {
	local  syncStore
	remote Remote
	cfg    Config
	log    *slog.Logger
}

func New(
	local *store.DB,
	pool *pgxpool.Pool,
	cfg Config,
	logger *slog.Logger,
) *Syncer {
	return NewWithRemote(local, &PGRemote{pool: pool}, cfg, logger)
}

func NewWithRemote(
	local *store.DB,
	remote Remote,
	cfg Config,
	logger *slog.Logger,
) *Syncer {
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Minute
	}
	if cfg.TriggerRows <= 0 {
		cfg.TriggerRows = 50_000
	}
	if cfg.UpsertBatch <= 0 {
		cfg.UpsertBatch = maxRemoteBatch
	}
	if cfg.UpsertBatch > maxRemoteBatch {
		cfg.UpsertBatch = maxRemoteBatch
	}
	return &Syncer{local: local, remote: remote, cfg: cfg, log: logger}
}

func (s *Syncer) Run(ctx context.Context) {
	period := time.NewTicker(s.cfg.Interval)
	check := time.NewTicker(30 * time.Second)
	defer period.Stop()
	defer check.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-period.C:
			s.syncOnce(ctx)
		case <-check.C:
			count, err := s.local.PendingSyncCount(ctx)
			if err != nil {
				s.log.Error("sync: count queue", "err", err)
				s.reportFailure(ctx, err)
				continue
			}
			localEvents, localErr := s.local.PendingLocalSyncEvents(ctx, 1)
			if localErr != nil {
				s.log.Error("sync: count local outbox", "err", localErr)
				s.reportFailure(ctx, localErr)
				continue
			}
			if count >= s.cfg.TriggerRows || len(localEvents) != 0 {
				s.log.Info("sync: backlog trigger", "pending", count)
				s.syncOnce(ctx)
			} else if count == 0 {
				s.reportHealthy()
			}
		}
	}
}

type loadedBatch struct {
	queue    []store.SyncQueueRow
	files    []store.FileRow
	images   []store.ImageFeatureSyncRow
	videos   []store.VideoFeatureSyncRow
	frames   []store.VideoFrameSyncRow
	metadata []store.VideoMetadataSyncRow
	local    store.LocalSyncBatch
}

type videoMetadataLoader interface {
	LoadVideoMetadataBySHAs(context.Context, []string) ([]store.VideoMetadataSyncRow, error)
}

type videoMetadataRemoteTx interface {
	UpsertVideoMetadata(context.Context, []store.VideoMetadataSyncRow) error
}

func (s *Syncer) syncOnce(ctx context.Context) {
	limit := s.cfg.UpsertBatch
	if limit <= 0 || limit > maxRemoteBatch {
		limit = maxRemoteBatch
	}
	for {
		queueRows, err := s.local.PendingSyncBatch(ctx, limit)
		if err != nil {
			s.log.Error("sync: read mixed queue", "err", err)
			s.reportFailure(ctx, err)
			return
		}
		localEvents, err := s.local.PendingLocalSyncEvents(ctx, limit)
		if err != nil {
			s.log.Error("sync: read local outbox", "err", err)
			s.reportFailure(ctx, err)
			return
		}
		if len(queueRows) == 0 && len(localEvents) == 0 {
			s.reportHealthy()
			return
		}
		validRows, malformed, err := partitionQueueRows(queueRows)
		if err != nil {
			s.log.Error("sync: validate mixed queue", "err", err)
			s.reportFailure(ctx, err)
			return
		}
		if len(malformed) != 0 {
			for _, row := range malformed {
				s.log.Error(
					"sync: quarantine malformed feature SHA",
					"table", row.TableName,
					"row_pk", row.RowPK,
					"generation", row.Generation,
					"error", "feature SHA-512 must be exactly 128 lowercase hex characters",
				)
			}
			if err := s.local.QuarantineSyncRows(ctx, malformed); err != nil {
				s.log.Error("sync: quarantine malformed queue rows", "err", err)
				s.reportFailure(ctx, err)
				return
			}
		}
		if len(validRows) == 0 && len(localEvents) == 0 {
			continue
		}
		batch, missing, err := s.loadBatch(ctx, validRows)
		if err != nil {
			s.log.Error("sync: load batch", "err", err)
			s.reportFailure(ctx, err)
			return
		}
		if len(missing) != 0 {
			if err := s.local.PruneMissingSyncRows(ctx, missing); err != nil {
				s.log.Error("sync: prune missing source rows", "err", err)
				s.reportFailure(ctx, err)
				return
			}
			s.log.Warn("sync: pruned orphan queue rows", "rows", len(missing))
		}
		batch.local, err = s.local.LoadLocalSyncBatch(ctx, localEvents)
		if err != nil {
			s.log.Error("sync: load local outbox", "err", err)
			s.reportFailure(ctx, err)
			return
		}
		if len(batch.queue) == 0 && len(batch.local.Events) == 0 {
			continue
		}
		if err := s.commitRemoteBatch(ctx, batch); err != nil {
			s.log.Error(
				"sync: batch failed, retry next round",
				"err", err,
				"rows", len(batch.queue),
			)
			s.reportFailure(ctx, err)
			return
		}
		if err := s.local.MarkSyncBatch(ctx, batch.queue); err != nil {
			// Remote UPSERTs are idempotent. A commit followed by a local
			// acknowledgement failure is deliberately retried.
			s.log.Error("sync: mark local rows", "err", err)
			s.reportFailure(ctx, err)
			return
		}
		if err := s.local.AcknowledgeLocalSyncEvents(ctx, batch.local.Events); err != nil {
			s.log.Error("sync: acknowledge local outbox", "err", err)
			s.reportFailure(ctx, err)
			return
		}
	}
}

func (s *Syncer) reportHealthy() {
	if s.cfg.OnHealth != nil {
		s.cfg.OnHealth(HealthUpdate{Healthy: true})
	}
}

func (s *Syncer) reportFailure(ctx context.Context, err error) {
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	if s.cfg.OnHealth != nil {
		s.cfg.OnHealth(HealthUpdate{ErrorSummary: err.Error()})
	}
}

func partitionQueueRows(
	rows []store.SyncQueueRow,
) ([]store.SyncQueueRow, []store.SyncQueueRow, error) {
	valid := make([]store.SyncQueueRow, 0, len(rows))
	var malformed []store.SyncQueueRow
	for _, row := range rows {
		switch row.TableName {
		case "files":
			valid = append(valid, row)
		case "image_features", "video_features", "video_containers", "video_streams":
			if validFeatureSHA(row.RowPK) {
				valid = append(valid, row)
			} else {
				malformed = append(malformed, row)
			}
		case "video_frames":
			if validVideoFrameKey(row.RowPK) {
				valid = append(valid, row)
			} else {
				malformed = append(malformed, row)
			}
		default:
			return nil, nil, fmt.Errorf("sync: unsupported queue table %q", row.TableName)
		}
	}
	return valid, malformed, nil
}

func (s *Syncer) loadBatch(
	ctx context.Context,
	queueRows []store.SyncQueueRow,
) (loadedBatch, []store.SyncQueueRow, error) {
	keys := map[string][]string{
		"files":            nil,
		"image_features":   nil,
		"video_features":   nil,
		"video_frames":     nil,
		"video_containers": nil,
		"video_streams":    nil,
	}
	queueByTableKey := make(map[string]store.SyncQueueRow, len(queueRows))
	for _, row := range queueRows {
		switch row.TableName {
		case "files":
		case "image_features", "video_features", "video_frames", "video_containers", "video_streams":
		default:
			return loadedBatch{}, nil, fmt.Errorf(
				"sync: unsupported queue table %q", row.TableName,
			)
		}
		keys[row.TableName] = append(keys[row.TableName], row.RowPK)
		queueByTableKey[row.TableName+"\x00"+row.RowPK] = row
	}

	loadedFiles, err := s.local.LoadFilesByIDs(ctx, keys["files"])
	if err != nil {
		return loadedBatch{}, nil, fmt.Errorf("load files: %w", err)
	}
	loadedImages, err := s.local.LoadImageFeaturesBySHAs(ctx, keys["image_features"])
	if err != nil {
		return loadedBatch{}, nil, fmt.Errorf("load image features: %w", err)
	}
	loadedVideos, err := s.local.LoadVideoFeaturesBySHAs(ctx, keys["video_features"])
	if err != nil {
		return loadedBatch{}, nil, fmt.Errorf("load video features: %w", err)
	}
	loadedFrames, err := s.local.LoadVideoFramesByKeys(ctx, keys["video_frames"])
	if err != nil {
		return loadedBatch{}, nil, fmt.Errorf("load video frames: %w", err)
	}
	metadataKeys := append(append([]string(nil), keys["video_containers"]...), keys["video_streams"]...)
	var loadedMetadata []store.VideoMetadataSyncRow
	if len(metadataKeys) != 0 {
		loader, ok := s.local.(videoMetadataLoader)
		if !ok {
			return loadedBatch{}, nil, fmt.Errorf("load video metadata: local store does not support video metadata")
		}
		loadedMetadata, err = loader.LoadVideoMetadataBySHAs(ctx, metadataKeys)
		if err != nil {
			return loadedBatch{}, nil, fmt.Errorf("load video metadata: %w", err)
		}
	}

	found := make(map[string]bool, len(queueRows))
	files := make([]store.FileRow, 0, len(loadedFiles))
	for index := range loadedFiles {
		key := "files\x00" + strconv.FormatInt(loadedFiles[index].ID, 10)
		if _, ok := queueByTableKey[key]; ok {
			found[key] = true
			files = append(files, loadedFiles[index])
		}
	}
	images := make([]store.ImageFeatureSyncRow, 0, len(loadedImages))
	for index := range loadedImages {
		key := "image_features\x00" + loadedImages[index].SHA512
		if queued, ok := queueByTableKey[key]; ok {
			loadedImages[index].UpdatedAt = queued.EnqueuedAt
			found[key] = true
			images = append(images, loadedImages[index])
		}
	}
	videos := make([]store.VideoFeatureSyncRow, 0, len(loadedVideos))
	for index := range loadedVideos {
		key := "video_features\x00" + loadedVideos[index].SHA512
		if queued, ok := queueByTableKey[key]; ok {
			loadedVideos[index].UpdatedAt = queued.EnqueuedAt
			found[key] = true
			videos = append(videos, loadedVideos[index])
		}
	}
	frames := make([]store.VideoFrameSyncRow, 0, len(loadedFrames))
	for index := range loadedFrames {
		frameKey := fmt.Sprintf(
			"%s:%d",
			loadedFrames[index].SHA512,
			loadedFrames[index].FrameIdx,
		)
		key := "video_frames\x00" + frameKey
		if _, ok := queueByTableKey[key]; ok {
			found[key] = true
			frames = append(frames, loadedFrames[index])
		}
	}
	metadata := make([]store.VideoMetadataSyncRow, 0, len(loadedMetadata))
	for index := range loadedMetadata {
		matched := false
		for _, table := range []string{"video_containers", "video_streams"} {
			key := table + "\x00" + loadedMetadata[index].SHA512
			if queued, ok := queueByTableKey[key]; ok {
				found[key] = true
				if queued.EnqueuedAt > loadedMetadata[index].UpdatedAt {
					loadedMetadata[index].UpdatedAt = queued.EnqueuedAt
				}
				matched = true
			}
		}
		if matched {
			metadata = append(metadata, loadedMetadata[index])
		}
	}

	var acknowledged, missing []store.SyncQueueRow
	for _, row := range queueRows {
		if found[row.TableName+"\x00"+row.RowPK] {
			acknowledged = append(acknowledged, row)
		} else {
			missing = append(missing, row)
		}
	}
	return loadedBatch{
		queue: acknowledged, files: files, images: images, videos: videos, frames: frames,
		metadata: metadata,
	}, missing, nil
}

func (s *Syncer) commitRemoteBatch(ctx context.Context, batch loadedBatch) error {
	tx, err := s.remote.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin remote transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			rollbackCtx, cancel := context.WithTimeout(
				context.WithoutCancel(ctx),
				5*time.Second,
			)
			defer cancel()
			_ = tx.Rollback(rollbackCtx)
		}
	}()
	if len(batch.files) != 0 {
		if err := tx.UpsertFiles(ctx, batch.files); err != nil {
			return err
		}
	}
	if len(batch.images) != 0 {
		if err := tx.UpsertImages(ctx, batch.images); err != nil {
			return err
		}
	}
	if len(batch.videos) != 0 {
		if err := tx.UpsertVideos(ctx, batch.videos); err != nil {
			return err
		}
	}
	if len(batch.frames) != 0 {
		if err := tx.UpsertFrames(ctx, batch.frames); err != nil {
			return err
		}
	}
	if len(batch.metadata) != 0 {
		remote, ok := tx.(videoMetadataRemoteTx)
		if !ok {
			return fmt.Errorf("remote transaction does not support video metadata")
		}
		if err := remote.UpsertVideoMetadata(ctx, batch.metadata); err != nil {
			return err
		}
	}
	if len(batch.local.Events) != 0 {
		if err := tx.UpsertLocal(ctx, batch.local); err != nil {
			return err
		}
	}
	if err := tx.CloseBatch(ctx); err != nil {
		return fmt.Errorf("close remote batch: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit remote transaction: %w", err)
	}
	committed = true
	return nil
}

func validFeatureSHA(value string) bool {
	if len(value) != 128 {
		return false
	}
	for index := range value {
		character := value[index]
		if (character < '0' || character > '9') &&
			(character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func validVideoFrameKey(value string) bool {
	sha, indexText, found := strings.Cut(value, ":")
	return found &&
		!strings.Contains(indexText, ":") &&
		validFeatureSHA(sha) &&
		len(indexText) == 1 &&
		indexText[0] >= '0' &&
		indexText[0] <= '5'
}

type PGRemote struct {
	pool *pgxpool.Pool
}

type pgRemoteTx struct {
	tx       pgx.Tx
	batch    pgx.Batch
	commands int
}

func (remote *PGRemote) Begin(ctx context.Context) (RemoteTx, error) {
	tx, err := remote.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &pgRemoteTx{tx: tx}, nil
}

const upsertFilesPG = `
INSERT INTO files (
    machine_id, disk_no, path, size, mtime, sha512,
    phase1_done, phase2_done, status, missing_mask, error, updated_at
)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
ON CONFLICT (machine_id, path) DO UPDATE SET
    disk_no = EXCLUDED.disk_no,
    size = EXCLUDED.size,
    mtime = EXCLUDED.mtime,
    sha512 = COALESCE(EXCLUDED.sha512, files.sha512),
    phase1_done = EXCLUDED.phase1_done,
    phase2_done = EXCLUDED.phase2_done,
    status = EXCLUDED.status,
    missing_mask = EXCLUDED.missing_mask,
    error = EXCLUDED.error,
    updated_at = EXCLUDED.updated_at,
    synced_at = now();`

const upsertImagesPG = `
INSERT INTO image_features (
    sha512, width, height, pdq256, pdq_quality,
    phash_parts, sobel_hist, updated_at
)
VALUES ($1,$2,$3,$4,$5,$6,$7,to_timestamp($8))
ON CONFLICT (sha512) DO UPDATE SET
    width = CASE WHEN EXCLUDED.width > 0
                 THEN EXCLUDED.width ELSE image_features.width END,
    height = CASE WHEN EXCLUDED.height > 0
                  THEN EXCLUDED.height ELSE image_features.height END,
    pdq256 = COALESCE(EXCLUDED.pdq256, image_features.pdq256),
    pdq_quality = CASE WHEN EXCLUDED.pdq_quality > 0
                       THEN EXCLUDED.pdq_quality ELSE image_features.pdq_quality END,
    phash_parts = COALESCE(EXCLUDED.phash_parts, image_features.phash_parts),
    sobel_hist = COALESCE(EXCLUDED.sobel_hist, image_features.sobel_hist),
    updated_at = EXCLUDED.updated_at;`

const upsertVideosPG = `
INSERT INTO video_features (
    sha512, duration_ms, thumb_path, thumb_pdq256, thumb_quality, updated_at
)
VALUES ($1,$2,$3,$4,$5,to_timestamp($6))
ON CONFLICT (sha512) DO UPDATE SET
    duration_ms = COALESCE(EXCLUDED.duration_ms, video_features.duration_ms),
    thumb_path = COALESCE(EXCLUDED.thumb_path, video_features.thumb_path),
    thumb_pdq256 = COALESCE(EXCLUDED.thumb_pdq256, video_features.thumb_pdq256),
    thumb_quality = COALESCE(EXCLUDED.thumb_quality, video_features.thumb_quality),
    updated_at = EXCLUDED.updated_at;`

const upsertFramesPG = `
INSERT INTO video_frames (
    sha512, frame_idx, pdq256, phash_parts, sobel_hist
)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (sha512, frame_idx) DO UPDATE SET
    pdq256 = EXCLUDED.pdq256,
    phash_parts = EXCLUDED.phash_parts,
    sobel_hist = EXCLUDED.sobel_hist;`

const upsertVideoContainerPG = `
INSERT INTO video_containers (
    sha512, format_name, format_long_name, start_time_us, duration_us, bit_rate,
    file_size, probe_score, tags_json, primary_video_stream, decoder_name, updated_at
)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10,$11,to_timestamp($12))
ON CONFLICT (sha512) DO UPDATE SET
    format_name = EXCLUDED.format_name,
    format_long_name = EXCLUDED.format_long_name,
    start_time_us = EXCLUDED.start_time_us,
    duration_us = EXCLUDED.duration_us,
    bit_rate = EXCLUDED.bit_rate,
    file_size = EXCLUDED.file_size,
    probe_score = EXCLUDED.probe_score,
    tags_json = EXCLUDED.tags_json,
    primary_video_stream = EXCLUDED.primary_video_stream,
    decoder_name = EXCLUDED.decoder_name,
    updated_at = EXCLUDED.updated_at;`

const deleteVideoStreamsPG = `DELETE FROM video_streams WHERE sha512=$1;`

const insertVideoStreamPG = `
INSERT INTO video_streams (
    sha512, stream_index, media_type, codec_id, codec_name, codec_long_name, codec_tag,
    profile, level, time_base, start_time_us, duration_us, bit_rate, frame_count,
    disposition, language, title, tags_json, pixel_format, bit_depth, width, height,
    sar, dar, avg_frame_rate, real_frame_rate, rotation, color_range, color_space,
    color_transfer, color_primaries, chroma_location, field_order, sample_format,
    sample_rate, channels, channel_layout, audio_bit_depth
)
VALUES (
    $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18::jsonb,$19,
    $20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35,$36,$37,$38
);`

const upsertLocalRunPG = `INSERT INTO local_analysis_runs(machine_id,run_id,generation,task_id,status,created_at,completed_at,published_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8)
ON CONFLICT(machine_id,run_id,generation) DO UPDATE SET task_id=EXCLUDED.task_id,status=EXCLUDED.status,
 completed_at=COALESCE(EXCLUDED.completed_at,local_analysis_runs.completed_at),
 published_at=COALESCE(EXCLUDED.published_at,local_analysis_runs.published_at);`
const upsertLocalPairPG = `INSERT INTO local_pair_scores(machine_id,run_id,generation,pair_key,left_file_id,right_file_id,left_sha512,right_sha512,stage1_json,stage2_json,stage3_json,final_verdict)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10::jsonb,$11::jsonb,$12)
ON CONFLICT(machine_id,run_id,generation,pair_key) DO UPDATE SET stage1_json=EXCLUDED.stage1_json,
 stage2_json=COALESCE(EXCLUDED.stage2_json,local_pair_scores.stage2_json),
 stage3_json=COALESCE(EXCLUDED.stage3_json,local_pair_scores.stage3_json),final_verdict=EXCLUDED.final_verdict;`
const upsertLocalGroupPG = `INSERT INTO local_dup_groups(machine_id,run_id,generation,group_id,category,verdict)
VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(machine_id,run_id,generation,group_id) DO UPDATE SET category=EXCLUDED.category,verdict=EXCLUDED.verdict;`
const upsertLocalMemberPG = `INSERT INTO local_dup_members(machine_id,run_id,generation,group_id,file_id,sha512)
VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(machine_id,run_id,generation,group_id,file_id) DO UPDATE SET sha512=EXCLUDED.sha512;`
const upsertLocalEventPG = `INSERT INTO local_task_events(machine_id,sequence,topic,entity_key,generation,payload_json)
VALUES($1,$2,$3,$4,$5,$6::jsonb) ON CONFLICT(machine_id,sequence) DO UPDATE SET payload_json=EXCLUDED.payload_json;`
const upsertLocalReviewPG = `INSERT INTO local_review_decisions(machine_id,run_id,generation,group_id,file_id,decision,reviewer,note,reviewed_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(machine_id,run_id,generation,group_id,file_id) DO UPDATE SET
 decision=EXCLUDED.decision,reviewer=EXCLUDED.reviewer,note=EXCLUDED.note,reviewed_at=EXCLUDED.reviewed_at;`
const upsertLocalDeletePG = `INSERT INTO local_delete_results(machine_id,batch_id,file_id,run_id,generation,path,sha512,result,error_code,uncertain,completed_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),$10,$11) ON CONFLICT(machine_id,batch_id,file_id) DO UPDATE SET
 result=EXCLUDED.result,error_code=EXCLUDED.error_code,uncertain=EXCLUDED.uncertain,completed_at=EXCLUDED.completed_at;`
const applyLocalDeletedFilePG = `UPDATE files SET status='deleted',error=NULL,updated_at=GREATEST(updated_at,$4),synced_at=now()
WHERE machine_id=$1 AND path=$2 AND sha512=$3;`

func (tx *pgRemoteTx) UpsertFiles(_ context.Context, rows []store.FileRow) error {
	for _, row := range rows {
		tx.batch.Queue(
			upsertFilesPG,
			row.MachineID,
			row.DiskNo,
			row.Path,
			row.Size,
			row.MTime,
			nullableString(row.SHA512),
			boolToSmallint(row.Phase1Done),
			boolToSmallint(row.Phase2Done),
			row.Status,
			row.MissingMask,
			nullableString(row.Error),
			row.UpdatedAt,
		)
		tx.commands++
	}
	return nil
}

func (tx *pgRemoteTx) UpsertImages(
	_ context.Context,
	rows []store.ImageFeatureSyncRow,
) error {
	for _, row := range rows {
		tx.batch.Queue(
			upsertImagesPG,
			row.SHA512,
			row.Width,
			row.Height,
			nullableBytes(row.PDQ256),
			row.PDQQuality,
			nullableBytes(row.PHashParts),
			nullableBytes(row.SobelHist),
			row.UpdatedAt,
		)
		tx.commands++
	}
	return nil
}

func (tx *pgRemoteTx) UpsertVideos(
	_ context.Context,
	rows []store.VideoFeatureSyncRow,
) error {
	for _, row := range rows {
		tx.batch.Queue(
			upsertVideosPG,
			row.SHA512,
			nullableInt64(row.DurationMS),
			nullableString(row.ThumbPath),
			nullableBytes(row.ThumbPDQ256),
			nullableInt32(row.ThumbQuality),
			row.UpdatedAt,
		)
		tx.commands++
	}
	return nil
}

func (tx *pgRemoteTx) UpsertFrames(
	_ context.Context,
	rows []store.VideoFrameSyncRow,
) error {
	for _, row := range rows {
		tx.batch.Queue(
			upsertFramesPG,
			row.SHA512,
			row.FrameIdx,
			row.PDQ256,
			row.PHashParts,
			row.SobelHist,
		)
		tx.commands++
	}
	return nil
}

func (tx *pgRemoteTx) UpsertVideoMetadata(
	_ context.Context,
	rows []store.VideoMetadataSyncRow,
) error {
	for _, row := range rows {
		container := row.Container
		tx.batch.Queue(
			upsertVideoContainerPG,
			row.SHA512,
			container.FormatName,
			nullableMetadataString(container.FormatLongName),
			nullableInt64(container.StartTimeUS),
			nullableInt64(container.DurationUS),
			nullableInt64(container.BitRate),
			nullableInt64(container.FileSize),
			nullableInt32(container.ProbeScore),
			container.TagsJSON,
			nullableInt32(container.PrimaryVideoStream),
			nullableMetadataString(container.DecoderName),
			row.UpdatedAt,
		)
		tx.commands++
		tx.batch.Queue(deleteVideoStreamsPG, row.SHA512)
		tx.commands++
		for _, stream := range row.Streams {
			tx.batch.Queue(
				insertVideoStreamPG,
				row.SHA512, stream.Index, stream.MediaType, stream.CodecID, stream.CodecName,
				nullableMetadataString(stream.CodecLongName), nullableMetadataString(stream.CodecTag),
				nullableMetadataString(stream.Profile), nullableInt32(stream.Level),
				nullableMetadataString(stream.TimeBase), nullableInt64(stream.StartTimeUS),
				nullableInt64(stream.DurationUS), nullableInt64(stream.BitRate), nullableInt64(stream.FrameCount),
				stream.Disposition, nullableMetadataString(stream.Language), nullableMetadataString(stream.Title),
				stream.TagsJSON, nullableMetadataString(stream.PixelFormat), nullableInt32(stream.BitDepth),
				nullableInt32(stream.Width), nullableInt32(stream.Height), nullableMetadataString(stream.SAR),
				nullableMetadataString(stream.DAR), nullableMetadataString(stream.AvgFrameRate),
				nullableMetadataString(stream.RealFrameRate), nullableInt32(stream.Rotation),
				nullableMetadataString(stream.ColorRange), nullableMetadataString(stream.ColorSpace),
				nullableMetadataString(stream.ColorTransfer), nullableMetadataString(stream.ColorPrimaries),
				nullableMetadataString(stream.ChromaLocation), nullableMetadataString(stream.FieldOrder),
				nullableMetadataString(stream.SampleFormat), nullableInt32(stream.SampleRate),
				nullableInt32(stream.Channels), nullableMetadataString(stream.ChannelLayout),
				nullableInt32(stream.AudioBitDepth),
			)
			tx.commands++
		}
	}
	return nil
}

func (tx *pgRemoteTx) UpsertLocal(_ context.Context, batch store.LocalSyncBatch) error {
	for _, row := range batch.Runs {
		tx.batch.Queue(upsertLocalRunPG, row.MachineID, row.RunID, row.Generation, row.TaskID,
			row.Status, row.CreatedAt, nullableInt64(row.CompletedAt), nullableInt64(row.PublishedAt))
		tx.commands++
	}
	for _, row := range batch.Pairs {
		tx.batch.Queue(upsertLocalPairPG, row.MachineID, row.RunID, row.Generation, row.PairKey,
			row.LeftFileID, row.RightFileID, row.LeftSHA512, row.RightSHA512, row.Stage1JSON,
			nullableString(row.Stage2JSON), nullableString(row.Stage3JSON), row.Verdict)
		tx.commands++
	}
	for _, row := range batch.Groups {
		tx.batch.Queue(upsertLocalGroupPG, row.MachineID, row.RunID, row.Generation, row.GroupID, row.Category, row.Verdict)
		tx.commands++
	}
	for _, row := range batch.Members {
		tx.batch.Queue(upsertLocalMemberPG, row.MachineID, row.RunID, row.Generation, row.GroupID, row.FileID, row.SHA512)
		tx.commands++
	}
	for _, row := range batch.Events {
		tx.batch.Queue(upsertLocalEventPG, row.MachineID, row.Sequence, row.Topic, row.EntityKey, row.Generation, row.PayloadJSON)
		tx.commands++
	}
	for _, row := range batch.Reviews {
		tx.batch.Queue(upsertLocalReviewPG, row.MachineID, row.RunID, row.Generation, row.GroupID,
			row.FileID, row.Decision, row.Reviewer, row.Note, row.ReviewedAt)
		tx.commands++
	}
	for _, row := range batch.Deletes {
		tx.batch.Queue(upsertLocalDeletePG, row.MachineID, row.BatchID, row.FileID, row.RunID,
			row.Generation, row.Path, row.SHA512, row.Result, row.ErrorCode, row.Uncertain, row.CompletedAt)
		tx.commands++
		if row.Result == "deleted" && row.Status == "deleted" && !row.Uncertain {
			tx.batch.Queue(applyLocalDeletedFilePG, row.MachineID, row.Path, row.SHA512, row.CompletedAt)
			tx.commands++
		}
	}
	return nil
}

func (tx *pgRemoteTx) CloseBatch(ctx context.Context) error {
	results := tx.tx.SendBatch(ctx, &tx.batch)
	for index := 0; index < tx.commands; index++ {
		if _, err := results.Exec(); err != nil {
			_ = results.Close()
			return fmt.Errorf("execute remote batch item %d: %w", index, err)
		}
	}
	if err := results.Close(); err != nil {
		return err
	}
	return nil
}

func (tx *pgRemoteTx) Commit(ctx context.Context) error {
	return tx.tx.Commit(ctx)
}

func (tx *pgRemoteTx) Rollback(ctx context.Context) error {
	return tx.tx.Rollback(ctx)
}

func nullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableInt32(value *int32) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableMetadataString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func boolToSmallint(value bool) int16 {
	if value {
		return 1
	}
	return 0
}
