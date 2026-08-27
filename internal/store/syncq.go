package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"dedup/internal/proto"
)

type SyncQueueRow struct {
	TableName  string
	RowPK      string
	Generation int64
	EnqueuedAt int64
}

func (d *DB) PendingSyncRows(
	ctx context.Context,
	table string,
	limit int,
) ([]SyncQueueRow, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT table_name, row_pk, generation, enqueued_at
		FROM sync_queue
		WHERE synced = 0 AND table_name = ?1
		ORDER BY enqueued_at, row_pk
		LIMIT ?2;`, table, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SyncQueueRow
	for rows.Next() {
		var row SyncQueueRow
		if err := rows.Scan(&row.TableName, &row.RowPK, &row.Generation, &row.EnqueuedAt); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// PendingSyncBatch returns a single fair, mixed-table batch. Ranking each
// supported table independently and then ordering by rank prevents a busy
// table from starving the other tables while retaining deterministic FIFO
// order within a table.
func (d *DB) PendingSyncBatch(ctx context.Context, limit int) ([]SyncQueueRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	tableOffset := (d.syncTableCursor.Add(1) - 1) % 6
	rows, err := d.db.QueryContext(ctx, `
		WITH ranked AS (
			SELECT table_name, row_pk, generation, enqueued_at,
			       ROW_NUMBER() OVER (
			           PARTITION BY table_name
			           ORDER BY enqueued_at, row_pk
			       ) AS table_rank
			FROM sync_queue
			WHERE synced = 0
			  AND table_name IN (
			      'files', 'image_features', 'video_features', 'video_frames',
			      'video_containers', 'video_streams'
			  )
		)
		SELECT table_name, row_pk, generation, enqueued_at
		FROM ranked
		ORDER BY table_rank,
		         (
		             CASE table_name
		                 WHEN 'files' THEN 0
		                 WHEN 'image_features' THEN 1
		                 WHEN 'video_features' THEN 2
			                 WHEN 'video_frames' THEN 3
			                 WHEN 'video_containers' THEN 4
			                 WHEN 'video_streams' THEN 5
			             END - ?2 + 6
			         ) % 6,
		         enqueued_at,
		         row_pk
		LIMIT ?1;`, limit, tableOffset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SyncQueueRow
	for rows.Next() {
		var row SyncQueueRow
		if err := rows.Scan(
			&row.TableName,
			&row.RowPK,
			&row.Generation,
			&row.EnqueuedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (d *DB) PendingSyncCount(ctx context.Context) (int64, error) {
	var count int64
	err := d.db.QueryRowContext(
		ctx,
		`SELECT count(*) FROM sync_queue WHERE synced = 0;`,
	).Scan(&count)
	return count, err
}

func (d *DB) MarkSynced(
	ctx context.Context,
	table string,
	rows []SyncQueueRow,
) error {
	mixed := make([]SyncQueueRow, len(rows))
	copy(mixed, rows)
	for index := range mixed {
		mixed[index].TableName = table
	}
	return d.MarkSyncBatch(ctx, mixed)
}

// MarkSyncBatch acknowledges all observed queue generations in one local
// transaction. A row whose generation advanced during remote I/O is left
// pending by the exact generation predicate.
func (d *DB) MarkSyncBatch(ctx context.Context, rows []SyncQueueRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, row := range rows {
		if row.TableName == "" {
			return fmt.Errorf("store: mark synced row has empty table name")
		}
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE sync_queue
			 SET synced = 1
			 WHERE table_name = ?1 AND row_pk = ?2 AND generation = ?3;`,
			row.TableName,
			row.RowPK,
			row.Generation,
		); err != nil {
			return fmt.Errorf(
				"store: mark synced %s/%s@%d: %w",
				row.TableName,
				row.RowPK,
				row.Generation,
				err,
			)
		}
	}
	return tx.Commit()
}

// PruneMissingSyncRows removes orphan queue entries by exact observed
// generation. A concurrent source-row recreation must enqueue its write and
// advance the generation, so that newer work is not deleted here.
func (d *DB) PruneMissingSyncRows(ctx context.Context, rows []SyncQueueRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, row := range rows {
		var statement string
		switch row.TableName {
		case "files":
			id, err := strconv.ParseInt(row.RowPK, 10, 64)
			if err != nil || id <= 0 || strconv.FormatInt(id, 10) != row.RowPK {
				return fmt.Errorf("store: invalid files queue key %q", row.RowPK)
			}
			statement = `
				DELETE FROM sync_queue
				WHERE table_name = ?1 AND row_pk = ?2
				  AND generation = ?3 AND synced = 0
				  AND NOT EXISTS (
				      SELECT 1 FROM files WHERE id = ?4
				  );`
			if _, err := tx.ExecContext(
				ctx, statement, row.TableName, row.RowPK, row.Generation, id,
			); err != nil {
				return fmt.Errorf(
					"store: prune missing %s/%s@%d: %w",
					row.TableName, row.RowPK, row.Generation, err,
				)
			}
			continue
		case "image_features":
			if !canonicalFeatureSHA(row.RowPK) {
				return fmt.Errorf("store: invalid image feature queue key %q", row.RowPK)
			}
			statement = `
				DELETE FROM sync_queue
				WHERE table_name = ?1 AND row_pk = ?2
				  AND generation = ?3 AND synced = 0
				  AND NOT EXISTS (
				      SELECT 1 FROM image_features WHERE sha512 = ?2
				  );`
		case "video_features":
			if !canonicalFeatureSHA(row.RowPK) {
				return fmt.Errorf("store: invalid video feature queue key %q", row.RowPK)
			}
			statement = `
				DELETE FROM sync_queue
				WHERE table_name = ?1 AND row_pk = ?2
				  AND generation = ?3 AND synced = 0
				  AND NOT EXISTS (
				      SELECT 1 FROM video_features WHERE sha512 = ?2
				  );`
		case "video_frames":
			sha, frameIdx, ok := parseVideoFrameKey(row.RowPK)
			if !ok {
				return fmt.Errorf("store: invalid video frame queue key %q", row.RowPK)
			}
			statement = `
				DELETE FROM sync_queue
				WHERE table_name = ?1 AND row_pk = ?2
				  AND generation = ?3 AND synced = 0
				  AND NOT EXISTS (
				      SELECT 1 FROM video_frames
				      WHERE sha512 = ?4 AND frame_idx = ?5
				        AND pdq256 IS NOT NULL
				        AND phash_parts IS NOT NULL
				        AND sobel_hist IS NOT NULL
				  );`
			if _, err := tx.ExecContext(
				ctx,
				statement,
				row.TableName,
				row.RowPK,
				row.Generation,
				sha,
				frameIdx,
			); err != nil {
				return fmt.Errorf(
					"store: prune missing %s/%s@%d: %w",
					row.TableName,
					row.RowPK,
					row.Generation,
					err,
				)
			}
			continue
		case "video_containers", "video_streams":
			if !canonicalFeatureSHA(row.RowPK) {
				return fmt.Errorf("store: invalid video metadata queue key %q", row.RowPK)
			}
			_, _, complete, err := loadVideoMetadata(ctx, tx, row.RowPK)
			if err != nil {
				return err
			}
			if complete {
				continue
			}
			if _, err := tx.ExecContext(ctx, `
				DELETE FROM sync_queue
				WHERE table_name=?1 AND row_pk=?2 AND generation=?3 AND synced=0`,
				row.TableName, row.RowPK, row.Generation,
			); err != nil {
				return fmt.Errorf("store: prune incomplete %s/%s@%d: %w", row.TableName, row.RowPK, row.Generation, err)
			}
			continue
		default:
			return fmt.Errorf("store: unsupported missing-row table %q", row.TableName)
		}
		if _, err := tx.ExecContext(
			ctx, statement, row.TableName, row.RowPK, row.Generation,
		); err != nil {
			return fmt.Errorf(
				"store: prune missing %s/%s@%d: %w",
				row.TableName,
				row.RowPK,
				row.Generation,
				err,
			)
		}
	}
	return tx.Commit()
}

// QuarantineSyncRows removes only malformed feature identities at the exact
// generation observed by the syncer. The caller records the actionable
// diagnostic before invoking this operation; valid feature work and files
// can never be discarded through this path.
func (d *DB) QuarantineSyncRows(ctx context.Context, rows []SyncQueueRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, row := range rows {
		var malformed bool
		switch row.TableName {
		case "image_features", "video_features", "video_containers", "video_streams":
			malformed = !canonicalFeatureSHA(row.RowPK)
		case "video_frames":
			_, _, valid := parseVideoFrameKey(row.RowPK)
			malformed = !valid
		default:
			return fmt.Errorf("store: cannot quarantine supported row table %q", row.TableName)
		}
		if !malformed {
			return fmt.Errorf("store: cannot quarantine valid feature key %s", row.RowPK)
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM sync_queue
			WHERE table_name = ?1 AND row_pk = ?2
			  AND generation = ?3 AND synced = 0;`,
			row.TableName, row.RowPK, row.Generation,
		); err != nil {
			return fmt.Errorf(
				"store: quarantine malformed %s/%s@%d: %w",
				row.TableName,
				row.RowPK,
				row.Generation,
				err,
			)
		}
	}
	return tx.Commit()
}

func canonicalFeatureSHA(value string) bool {
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

func parseVideoFrameKey(value string) (string, int, bool) {
	sha, indexText, found := strings.Cut(value, ":")
	if !found || strings.Contains(indexText, ":") ||
		!canonicalFeatureSHA(sha) || len(indexText) != 1 ||
		indexText[0] < '0' || indexText[0] > '5' {
		return "", 0, false
	}
	return sha, int(indexText[0] - '0'), true
}

type ImageFeatureSyncRow struct {
	SHA512     string
	Width      int32
	Height     int32
	PDQ256     []byte
	PDQQuality int32
	PHashParts []byte
	SobelHist  []byte
	UpdatedAt  int64
}

func (d *DB) LoadImageFeaturesBySHAs(
	ctx context.Context,
	shas []string,
) ([]ImageFeatureSyncRow, error) {
	if len(shas) == 0 {
		return nil, nil
	}
	query, args := syncInQuery(`
		SELECT sha512, width, height, pdq256, pdq_quality, phash_parts, sobel_hist
		FROM image_features WHERE sha512 IN (`, shas)
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ImageFeatureSyncRow
	for rows.Next() {
		var row ImageFeatureSyncRow
		if err := rows.Scan(
			&row.SHA512,
			&row.Width,
			&row.Height,
			&row.PDQ256,
			&row.PDQQuality,
			&row.PHashParts,
			&row.SobelHist,
		); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

type VideoFeatureSyncRow struct {
	SHA512       string
	DurationMS   *int64
	ThumbPath    *string
	ThumbPDQ256  []byte
	ThumbQuality *int32
	ThumbWidth   *int32
	ThumbHeight  *int32
	UpdatedAt    int64
}

type VideoMetadataSyncRow struct {
	SHA512    string
	Container proto.VideoContainerMetadata
	Streams   []proto.VideoStreamMetadata
	UpdatedAt int64
}

func (d *DB) LoadVideoMetadataBySHAs(
	ctx context.Context,
	shas []string,
) ([]VideoMetadataSyncRow, error) {
	return d.loadVideoMetadataBySHAsWithBarrier(ctx, shas, nil)
}

func (d *DB) loadVideoMetadataBySHAsWithBarrier(
	ctx context.Context,
	shas []string,
	afterFirstContainer func(),
) ([]VideoMetadataSyncRow, error) {
	seen := make(map[string]struct{}, len(shas))
	for _, sha := range shas {
		if !canonicalFeatureSHA(sha) {
			return nil, fmt.Errorf("store: invalid video metadata SHA %q", sha)
		}
		seen[sha] = struct{}{}
	}
	tx, err := d.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("store: begin video metadata snapshot: %w", err)
	}
	defer tx.Rollback()
	loaded := make(map[string]struct{}, len(seen))
	result := make([]VideoMetadataSyncRow, 0, len(seen))
	barrier := afterFirstContainer
	for _, sha := range shas {
		if _, exists := loaded[sha]; exists {
			continue
		}
		loaded[sha] = struct{}{}
		container, streams, complete, err := loadVideoMetadataWithBarrier(ctx, tx, sha, barrier)
		barrier = nil
		if err != nil {
			return nil, err
		}
		if complete {
			result = append(result, VideoMetadataSyncRow{
				SHA512: sha, Container: *container, Streams: streams,
			})
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit video metadata snapshot: %w", err)
	}
	return result, nil
}

func (d *DB) LoadVideoFeaturesBySHAs(
	ctx context.Context,
	shas []string,
) ([]VideoFeatureSyncRow, error) {
	if len(shas) == 0 {
		return nil, nil
	}
	query, args := syncInQuery(`
		SELECT sha512, duration_ms, thumb_path, thumb_pdq256, thumb_quality,
		       thumb_width, thumb_height
		FROM video_features WHERE sha512 IN (`, shas)
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []VideoFeatureSyncRow
	for rows.Next() {
		var row VideoFeatureSyncRow
		var duration, quality, thumbWidth, thumbHeight sql.NullInt64
		var path sql.NullString
		if err := rows.Scan(
			&row.SHA512,
			&duration,
			&path,
			&row.ThumbPDQ256,
			&quality,
			&thumbWidth,
			&thumbHeight,
		); err != nil {
			return nil, err
		}
		if duration.Valid {
			value := duration.Int64
			row.DurationMS = &value
		}
		if path.Valid {
			value := path.String
			row.ThumbPath = &value
		}
		if quality.Valid {
			value := int32(quality.Int64)
			row.ThumbQuality = &value
		}
		if thumbWidth.Valid {
			value := int32(thumbWidth.Int64)
			row.ThumbWidth = &value
		}
		if thumbHeight.Valid {
			value := int32(thumbHeight.Int64)
			row.ThumbHeight = &value
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

type VideoFrameSyncRow struct {
	SHA512     string
	FrameIdx   int
	PDQ256     []byte
	PHashParts []byte
	SobelHist  []byte
}

func (d *DB) LoadVideoFramesByKeys(
	ctx context.Context,
	keys []string,
) ([]VideoFrameSyncRow, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	clauses := make([]string, 0, len(keys))
	args := make([]any, 0, len(keys)*2)
	for _, key := range keys {
		sha, frameIdx, ok := parseVideoFrameKey(key)
		if !ok {
			return nil, fmt.Errorf("store: invalid video frame queue key %q", key)
		}
		clauses = append(clauses, "(sha512=? AND frame_idx=?)")
		args = append(args, sha, frameIdx)
	}
	rows, err := d.db.QueryContext(ctx, `
		SELECT sha512, frame_idx, pdq256, phash_parts, sobel_hist
		FROM video_frames
		WHERE (`+strings.Join(clauses, " OR ")+`)
		  AND pdq256 IS NOT NULL
		  AND phash_parts IS NOT NULL
		  AND sobel_hist IS NOT NULL;`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []VideoFrameSyncRow
	for rows.Next() {
		var row VideoFrameSyncRow
		if err := rows.Scan(
			&row.SHA512,
			&row.FrameIdx,
			&row.PDQ256,
			&row.PHashParts,
			&row.SobelHist,
		); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func syncInQuery(prefix string, keys []string) (string, []any) {
	placeholders := strings.TrimRight(strings.Repeat("?,", len(keys)), ",")
	args := make([]any, len(keys))
	for index, key := range keys {
		args[index] = key
	}
	return prefix + placeholders + ");", args
}
