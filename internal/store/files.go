package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"dedup/internal/proto"
)

type FileRow struct {
	ID          int64
	MachineID   string
	DiskNo      int64
	Path        string
	Size        int64
	MTime       int64
	SHA512      *string
	Phase1Done  bool
	Phase2Done  bool
	Status      string
	MissingMask uint32
	Error       *string
	UpdatedAt   int64
}

type EnumUpsert struct {
	MachineID   string
	DiskNo      int64
	Path        string
	Size        int64
	MTime       int64
	MissingBase uint32
	Force       bool
}

const upsertEnumeratedSQL = `
INSERT INTO files (machine_id, disk_no, path, size, mtime, status, missing_mask, updated_at)
VALUES (?1, ?2, ?3, ?4, ?5, 'pending', ?6, ?7)
ON CONFLICT (machine_id, path) DO UPDATE SET
    disk_no = excluded.disk_no,
    sha512 = CASE
        WHEN NOT ?8 AND files.size = excluded.size AND files.mtime = excluded.mtime
             AND files.sha512 IS NOT NULL AND files.status != 'deleted'
        THEN files.sha512 ELSE NULL END,
    missing_mask = CASE
        WHEN NOT ?8 AND files.size = excluded.size AND files.mtime = excluded.mtime
             AND files.sha512 IS NOT NULL AND files.status != 'deleted'
        THEN files.missing_mask ELSE excluded.missing_mask END,
    phase1_done = CASE
        WHEN NOT ?8 AND files.size = excluded.size AND files.mtime = excluded.mtime
             AND files.sha512 IS NOT NULL AND files.status != 'deleted'
        THEN files.phase1_done ELSE 0 END,
    phase2_done = CASE
        WHEN NOT ?8 AND files.size = excluded.size AND files.mtime = excluded.mtime
             AND files.sha512 IS NOT NULL AND files.status != 'deleted'
        THEN files.phase2_done ELSE 0 END,
    status = CASE
        WHEN NOT ?8 AND files.size = excluded.size AND files.mtime = excluded.mtime
             AND files.sha512 IS NOT NULL AND files.status != 'deleted'
        THEN files.status ELSE 'pending' END,
    error = CASE
        WHEN NOT ?8 AND files.size = excluded.size AND files.mtime = excluded.mtime
             AND files.sha512 IS NOT NULL AND files.status != 'deleted'
        THEN files.error ELSE NULL END,
    size = excluded.size,
    mtime = excluded.mtime,
    updated_at = excluded.updated_at;`

func (d *DB) UpsertEnumerated(ctx context.Context, records []EnumUpsert) error {
	if len(records) == 0 {
		return nil
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	statement, err := tx.PrepareContext(ctx, upsertEnumeratedSQL)
	if err != nil {
		return err
	}
	defer statement.Close()

	now := time.Now().Unix()
	for _, record := range records {
		if _, err := statement.ExecContext(
			ctx,
			record.MachineID,
			record.DiskNo,
			record.Path,
			record.Size,
			record.MTime,
			record.MissingBase,
			now,
			record.Force,
		); err != nil {
			return fmt.Errorf("store: upsert %s: %w", record.Path, err)
		}
		if err := revalidateEnumeratedPhase1(ctx, tx, record); err != nil {
			return fmt.Errorf("store: revalidate %s: %w", record.Path, err)
		}
	}
	return tx.Commit()
}

func revalidateEnumeratedPhase1(ctx context.Context, tx *sql.Tx, record EnumUpsert) error {
	var sha sql.NullString
	var currentMissing uint32
	if err := tx.QueryRowContext(ctx, `
		SELECT sha512, missing_mask FROM files WHERE machine_id=?1 AND path=?2`,
		record.MachineID, record.Path,
	).Scan(&sha, &currentMissing); err != nil {
		return err
	}
	if !sha.Valid || sha.String == "" {
		return nil
	}

	kind := MediaKind("")
	switch {
	case record.MissingBase&proto.FieldPDQ256 != 0:
		kind = MediaImage
	case record.MissingBase&(proto.FieldThumb|proto.FieldVideoDuration|proto.FieldVideoContactSheet) != 0:
		kind = MediaVideo
	}
	row := FileRow{
		MachineID: record.MachineID,
		Path:      record.Path,
		Size:      record.Size,
		MTime:     record.MTime,
		SHA512:    &sha.String,
	}
	missing, err := missingPhase1(ctx, tx, row, kind, record.MissingBase)
	if err != nil {
		return err
	}
	updatedMissing := currentMissing&^phaseOneFieldsMask | missing
	status, phase1Done := stageOneState(kind, updatedMissing, false)
	if _, err := tx.ExecContext(ctx, `
		UPDATE files SET
			missing_mask=?3,
			phase1_done=?4,
			status=CASE WHEN ?4=0 THEN ?5 WHEN ?3=0 THEN 'done' ELSE status END,
			error=CASE WHEN ?3=0 THEN NULL ELSE error END
		WHERE machine_id=?1 AND path=?2`,
		record.MachineID, record.Path, updatedMissing, boolToInt(phase1Done), status,
	); err != nil {
		return err
	}
	return nil
}

type PendingFile struct {
	Path        string
	Size        int64
	MTime       int64
	DiskNo      int64
	MissingMask uint32
	SHA512      *string
}

func (d *DB) PendingSnapshot(
	ctx context.Context,
	machineID string,
) (map[int64][]PendingFile, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT disk_no, path, size, mtime, missing_mask, sha512
		FROM files
		WHERE machine_id = ?1
		  AND status != 'deleted'
		  AND (missing_mask & ?2) != 0
		ORDER BY disk_no, path;`, machineID, phaseOneFieldsMask)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[int64][]PendingFile)
	for rows.Next() {
		var file PendingFile
		var sha sql.NullString
		if err := rows.Scan(
			&file.DiskNo,
			&file.Path,
			&file.Size,
			&file.MTime,
			&file.MissingMask,
			&sha,
		); err != nil {
			return nil, err
		}
		if sha.Valid {
			value := sha.String
			file.SHA512 = &value
		}
		out[file.DiskNo] = append(out[file.DiskNo], file)
	}
	return out, rows.Err()
}

type HashResult struct {
	Path   string
	SHA512 string
	Size   int64
	MTime  int64
	Err    string
}

const markHashOKSQL = `
UPDATE files SET sha512 = ?3, status = 'done', error = NULL,
    missing_mask = missing_mask & ~1,
	phase1_done = CASE WHEN ((missing_mask & ~1) & ?5) = 0 THEN 1 ELSE 0 END,
    updated_at = ?4
WHERE machine_id = ?1 AND path = ?2;`

const markHashFailSQL = `
UPDATE files SET status = 'failed', error = ?3, updated_at = ?4
WHERE machine_id = ?1 AND path = ?2;`

const enqueueFilesSyncSQL = `
INSERT INTO sync_queue (table_name, row_pk, synced, enqueued_at, generation)
SELECT 'files', CAST(id AS TEXT), 0, ?3, 1
FROM files WHERE machine_id = ?1 AND path = ?2
ON CONFLICT (table_name, row_pk) DO UPDATE
SET synced = 0,
    enqueued_at = excluded.enqueued_at,
    generation = sync_queue.generation + 1;`

const markDeletedSQL = `
UPDATE files SET status = 'deleted', error = NULL, updated_at = ?3
WHERE machine_id = ?1 AND path = ?2;`

// MarkDeleted records a physically successful local deletion and schedules the
// affected file rows for synchronization as one atomic SQLite transaction.
func (d *DB) MarkDeleted(
	ctx context.Context,
	machineID string,
	paths []string,
) error {
	if len(paths) == 0 {
		return nil
	}
	if machineID == "" {
		return fmt.Errorf("store: mark deleted: empty machine ID")
	}
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path == "" {
			return fmt.Errorf("store: mark deleted: empty path")
		}
		if _, exists := seen[path]; exists {
			return fmt.Errorf("store: mark deleted: duplicate path %q", path)
		}
		seen[path] = struct{}{}
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	for _, path := range paths {
		result, err := tx.ExecContext(ctx, markDeletedSQL, machineID, path, now)
		if err != nil {
			return fmt.Errorf("store: mark deleted %s: %w", path, err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: mark deleted %s rows affected: %w", path, err)
		}
		if changed != 1 {
			return fmt.Errorf("store: mark deleted %s: matched %d rows", path, changed)
		}

		result, err = tx.ExecContext(ctx, enqueueFilesSyncSQL, machineID, path, now)
		if err != nil {
			return fmt.Errorf("store: enqueue deleted %s: %w", path, err)
		}
		changed, err = result.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: enqueue deleted %s rows affected: %w", path, err)
		}
		if changed != 1 {
			return fmt.Errorf("store: enqueue deleted %s: affected %d rows", path, changed)
		}
	}
	return tx.Commit()
}

func (d *DB) ApplyHashResults(
	ctx context.Context,
	machineID string,
	results []HashResult,
) error {
	if len(results) == 0 {
		return nil
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	for _, result := range results {
		if result.Err == "" {
			_, err = tx.ExecContext(
				ctx, markHashOKSQL, machineID, result.Path, result.SHA512, now, phaseOneFieldsMask,
			)
		} else {
			_, err = tx.ExecContext(
				ctx, markHashFailSQL, machineID, result.Path, result.Err, now,
			)
		}
		if err != nil {
			return fmt.Errorf("store: apply %s: %w", result.Path, err)
		}
		if _, err := tx.ExecContext(
			ctx, enqueueFilesSyncSQL, machineID, result.Path, now,
		); err != nil {
			return fmt.Errorf("store: enqueue %s: %w", result.Path, err)
		}
	}
	return tx.Commit()
}

func (d *DB) LoadFilesByIDs(ctx context.Context, ids []string) ([]FileRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	query := `SELECT id, machine_id, disk_no, path, size, mtime, sha512,
	                 phase1_done, phase2_done, status, missing_mask, error, updated_at
	          FROM files WHERE id IN (` + placeholders + `);`
	args := make([]any, len(ids))
	for index, id := range ids {
		args[index] = id
	}
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []FileRow
	for rows.Next() {
		var row FileRow
		var sha, rowError sql.NullString
		var phase1, phase2 int64
		if err := rows.Scan(
			&row.ID,
			&row.MachineID,
			&row.DiskNo,
			&row.Path,
			&row.Size,
			&row.MTime,
			&sha,
			&phase1,
			&phase2,
			&row.Status,
			&row.MissingMask,
			&rowError,
			&row.UpdatedAt,
		); err != nil {
			return nil, err
		}
		if sha.Valid {
			value := sha.String
			row.SHA512 = &value
		}
		if rowError.Valid {
			value := rowError.String
			row.Error = &value
		}
		row.Phase1Done = phase1 != 0
		row.Phase2Done = phase2 != 0
		out = append(out, row)
	}
	return out, rows.Err()
}
