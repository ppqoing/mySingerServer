package store

import (
	"context"
	"database/sql"
	"encoding/json"
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

type DeletionResult struct {
	FileID             int64
	MachineID          string
	Path               string
	SHA512             string
	Size               int64
	MTime              int64
	BatchID            string
	RunID              string
	GroupID            string
	Generation         int64
	ConfirmationDigest string
	OK                 bool
	Uncertain          bool
	ErrorCode          string
	ErrorMessage       string
}

type DeletionItem struct {
	FileID    int64
	Result    string
	ErrorCode string
	Uncertain bool
}

type DeletionBatch struct {
	BatchID   string
	Status    string
	Requested int
	Succeeded int
	Failed    int
	Uncertain int
	Items     []DeletionItem
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

func (d *DB) BeginDeletionBatch(ctx context.Context, batchID string, selection CommittedDeletion, confirmationDigest string) error {
	if d == nil || d.db == nil || batchID == "" || confirmationDigest == "" || selection.MachineID == "" ||
		selection.RunID == "" || selection.GroupID == "" || selection.Generation <= 0 || len(selection.Files) == 0 {
		return fmt.Errorf("store: invalid deletion batch")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var currentRun string
	var currentGeneration int64
	if err := tx.QueryRowContext(ctx, `SELECT run_id,generation FROM local_current_analysis WHERE machine_id=?1`, selection.MachineID).Scan(&currentRun, &currentGeneration); err != nil {
		return fmt.Errorf("store: verify current deletion review: %w", err)
	}
	if currentRun != selection.RunID || currentGeneration != selection.Generation {
		return fmt.Errorf("store: deletion review generation changed")
	}
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO local_delete_batches(
		 batch_id,machine_id,run_id,confirmation_digest,status,requested_count,created_at,updated_at)
		VALUES (?1,?2,?3,?4,'running',?5,?6,?6)`,
		batchID, selection.MachineID, selection.RunID, confirmationDigest, len(selection.Files), now); err != nil {
		return fmt.Errorf("store: begin deletion batch: %w", err)
	}
	for _, file := range selection.Files {
		var status, sha string
		var size, mtime int64
		if err := tx.QueryRowContext(ctx, `
			SELECT status,sha512,size,mtime FROM files
			WHERE machine_id=?1 AND id=?2 AND path=?3`, file.MachineID, file.FileID, file.Path,
		).Scan(&status, &sha, &size, &mtime); err != nil {
			return fmt.Errorf("store: verify deletion file %d: %w", file.FileID, err)
		}
		if status == "deleted" || file.MachineID != selection.MachineID || sha != file.SHA512 || size != file.Size || mtime != file.MTime {
			return fmt.Errorf("store: deletion file identity changed")
		}
		var reviewCount int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM local_reviews
			WHERE machine_id=?1 AND run_id=?2 AND generation=?3 AND group_id=?4
			  AND file_id=?5 AND decision='delete'`,
			selection.MachineID, selection.RunID, selection.Generation, selection.GroupID, file.FileID,
		).Scan(&reviewCount); err != nil || reviewCount != 1 {
			return fmt.Errorf("store: deletion review changed")
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO local_delete_items(
			 batch_id,machine_id,file_id,path_snapshot,sha512,result,uncertain,created_at,updated_at)
			VALUES (?1,?2,?3,?4,?5,'pending',0,?6,?6)`,
			batchID, selection.MachineID, file.FileID, file.Path, file.SHA512, now); err != nil {
			return fmt.Errorf("store: begin deletion item: %w", err)
		}
	}
	return tx.Commit()
}

// CommitDeletionResults atomically completes a previously persisted batch and
// marks only explicit, certain successes deleted.
func (d *DB) CommitDeletionResults(ctx context.Context, batchID string, results []DeletionResult) error {
	if d == nil || d.db == nil || batchID == "" || len(results) == 0 {
		return fmt.Errorf("store: invalid deletion results")
	}
	first := results[0]
	if first.BatchID != batchID || first.MachineID == "" || first.RunID == "" || first.GroupID == "" ||
		first.Generation <= 0 || first.ConfirmationDigest == "" {
		return fmt.Errorf("store: invalid deletion batch identity")
	}
	seen := make(map[int64]struct{}, len(results))
	for _, result := range results {
		if result.BatchID != batchID || result.MachineID != first.MachineID || result.RunID != first.RunID ||
			result.GroupID != first.GroupID || result.Generation != first.Generation ||
			result.ConfirmationDigest != first.ConfirmationDigest || result.FileID <= 0 || result.Path == "" || result.SHA512 == "" {
			return fmt.Errorf("store: inconsistent deletion result identity")
		}
		if _, duplicate := seen[result.FileID]; duplicate {
			return fmt.Errorf("store: duplicate deletion file %d", result.FileID)
		}
		seen[result.FileID] = struct{}{}
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var machineID, runID, digest, status string
	var requested int
	if err := tx.QueryRowContext(ctx, `
		SELECT machine_id,run_id,confirmation_digest,status,requested_count
		FROM local_delete_batches WHERE batch_id=?1`, batchID,
	).Scan(&machineID, &runID, &digest, &status, &requested); err != nil {
		return fmt.Errorf("store: load deletion batch: %w", err)
	}
	if machineID != first.MachineID || runID != first.RunID || digest != first.ConfirmationDigest || status != "running" || requested != len(results) {
		return fmt.Errorf("store: deletion batch identity changed")
	}

	now := time.Now().UnixMilli()
	succeeded, failed, uncertainCount := 0, 0, 0
	for _, result := range results {
		var path, sha, itemStatus string
		if err := tx.QueryRowContext(ctx, `
			SELECT path_snapshot,sha512,result FROM local_delete_items
			WHERE batch_id=?1 AND machine_id=?2 AND file_id=?3`,
			batchID, result.MachineID, result.FileID,
		).Scan(&path, &sha, &itemStatus); err != nil {
			return fmt.Errorf("store: load deletion item %d: %w", result.FileID, err)
		}
		if path != result.Path || sha != result.SHA512 || itemStatus != "pending" {
			return fmt.Errorf("store: deletion item identity changed")
		}
		resultStatus := "failed"
		uncertain := 0
		switch {
		case result.OK && !result.Uncertain:
			resultStatus = "deleted"
			succeeded++
		case result.Uncertain:
			resultStatus = "uncertain"
			uncertain = 1
			uncertainCount++
		default:
			failed++
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE local_delete_items SET result=?4,error_code=NULLIF(?5,''),error_message=NULLIF(?6,''),
			 uncertain=?7,updated_at=?8,completed_at=?8
			WHERE batch_id=?1 AND machine_id=?2 AND file_id=?3`,
			batchID, result.MachineID, result.FileID, resultStatus, result.ErrorCode, result.ErrorMessage, uncertain, now); err != nil {
			return fmt.Errorf("store: update deletion item: %w", err)
		}
		if resultStatus != "deleted" {
			continue
		}
		changed, err := tx.ExecContext(ctx, markDeletedSQL, result.MachineID, result.Path, now)
		if err != nil {
			return fmt.Errorf("store: mark deletion result: %w", err)
		}
		if rows, err := changed.RowsAffected(); err != nil || rows != 1 {
			return fmt.Errorf("store: mark deletion result rows: %d %v", rows, err)
		}
		if _, err := tx.ExecContext(ctx, enqueueFilesSyncSQL, result.MachineID, result.Path, now); err != nil {
			return fmt.Errorf("store: enqueue deletion result: %w", err)
		}
		payload, err := json.Marshal(struct {
			FileID    int64  `json:"file_id"`
			MachineID string `json:"machine_id"`
			Status    string `json:"status"`
			SHA512    string `json:"sha512"`
			BatchID   string `json:"batch_id"`
		}{result.FileID, result.MachineID, "deleted", result.SHA512, batchID})
		if err != nil {
			return err
		}
		entityKey := fmt.Sprintf("%s:%d", batchID, result.FileID)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO local_outbox(topic,entity_key,generation,payload_json,created_at,updated_at)
			VALUES ('local.delete',?1,?2,?3,?4,?4)`, entityKey, result.Generation, string(payload), now); err != nil {
			return fmt.Errorf("store: enqueue local delete event: %w", err)
		}
	}
	batchStatus := "failed"
	if succeeded == len(results) {
		batchStatus = "succeeded"
	} else if uncertainCount > 0 {
		batchStatus = "uncertain"
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE local_delete_batches SET status=?2,succeeded_count=?3,failed_count=?4,
		 uncertain_count=?5,updated_at=?6,completed_at=?6 WHERE batch_id=?1`,
		batchID, batchStatus, succeeded, failed, uncertainCount, now); err != nil {
		return fmt.Errorf("store: finish deletion batch: %w", err)
	}
	return tx.Commit()
}

func (d *DB) LoadDeletionBatch(ctx context.Context, machineID, batchID string) (DeletionBatch, error) {
	if d == nil || d.db == nil || machineID == "" || batchID == "" {
		return DeletionBatch{}, fmt.Errorf("store: invalid deletion batch")
	}
	batch := DeletionBatch{BatchID: batchID}
	if err := d.db.QueryRowContext(ctx, `
		SELECT status,requested_count,succeeded_count,failed_count,uncertain_count
		FROM local_delete_batches WHERE machine_id=?1 AND batch_id=?2`, machineID, batchID,
	).Scan(&batch.Status, &batch.Requested, &batch.Succeeded, &batch.Failed, &batch.Uncertain); err != nil {
		return DeletionBatch{}, err
	}
	rows, err := d.db.QueryContext(ctx, `
		SELECT file_id,result,COALESCE(error_code,''),uncertain
		FROM local_delete_items WHERE machine_id=?1 AND batch_id=?2 ORDER BY item_id`, machineID, batchID)
	if err != nil {
		return DeletionBatch{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var item DeletionItem
		if err := rows.Scan(&item.FileID, &item.Result, &item.ErrorCode, &item.Uncertain); err != nil {
			return DeletionBatch{}, err
		}
		batch.Items = append(batch.Items, item)
	}
	return batch, rows.Err()
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
