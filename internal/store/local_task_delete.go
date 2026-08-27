package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	sqlite "modernc.org/sqlite"
)

type LocalTaskDeleteResult struct {
	Deleted        bool
	AlreadyDeleted bool
	DeletedAt      int64
}

func (d *DB) HasLocalTaskDeletionReceipt(ctx context.Context, machineID, taskID string) (bool, error) {
	if machineID == "" || taskID == "" {
		return false, fmt.Errorf("store: local task deletion receipt requires machine and task")
	}
	var exists int
	err := d.db.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM local_task_deletion_receipts
			WHERE machine_id=?1 AND task_id=?2
		)`, machineID, taskID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("store: check local task deletion receipt: %w", err)
	}
	return exists == 1, nil
}

func (d *DB) LoadLocalTaskDeletionReceipt(
	ctx context.Context,
	machineID, taskID, instanceID string,
) (LocalTaskDeleteResult, error) {
	if machineID == "" || taskID == "" || instanceID == "" {
		return LocalTaskDeleteResult{}, fmt.Errorf("store: local task deletion receipt requires machine, task, and instance")
	}
	var deletedAt int64
	err := d.db.QueryRowContext(ctx, `
		SELECT deleted_at FROM local_task_deletion_receipts
		WHERE machine_id=?1 AND task_id=?2 AND instance_id=?3`,
		machineID, taskID, instanceID,
	).Scan(&deletedAt)
	if err != nil {
		return LocalTaskDeleteResult{}, fmt.Errorf("store: load local task deletion receipt: %w", err)
	}
	return LocalTaskDeleteResult{AlreadyDeleted: true, DeletedAt: deletedAt}, nil
}

func (d *DB) DeleteLocalTaskData(
	ctx context.Context,
	machineID string,
	control LocalTaskControl,
) (LocalTaskDeleteResult, error) {
	if err := validateLocalTaskControl(machineID, control); err != nil {
		return LocalTaskDeleteResult{}, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return LocalTaskDeleteResult{}, wrapLocalTaskDeleteError("begin transaction", err)
	}
	defer tx.Rollback()

	var deletedAt int64
	err = tx.QueryRowContext(ctx, `
		SELECT deleted_at FROM local_task_deletion_receipts
		WHERE machine_id=?1 AND task_id=?2 AND instance_id=?3`,
		machineID, control.TaskID, control.InstanceID,
	).Scan(&deletedAt)
	if err == nil {
		return LocalTaskDeleteResult{AlreadyDeleted: true, DeletedAt: deletedAt}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return LocalTaskDeleteResult{}, wrapLocalTaskDeleteError("load exact receipt", err)
	}

	task, err := loadLocalTaskTx(ctx, tx, machineID, control.TaskID)
	if err != nil {
		return LocalTaskDeleteResult{}, wrapLocalTaskDeleteError("load current task", err)
	}
	if task.InstanceID != control.InstanceID {
		return LocalTaskDeleteResult{}, fmt.Errorf("%w: task %s", ErrLocalTaskInstanceMismatch, control.TaskID)
	}
	if task.Revision != control.ExpectedRevision {
		return LocalTaskDeleteResult{}, fmt.Errorf("%w: task %s", ErrLocalTaskStale, control.TaskID)
	}
	if task.Status != "deleting" {
		return LocalTaskDeleteResult{}, fmt.Errorf("%w: %s to deleted", ErrLocalTaskTransition, task.Status)
	}

	var runID string
	var runGeneration int64
	err = tx.QueryRowContext(ctx, `
		SELECT run_id,generation FROM local_analysis_runs
		WHERE machine_id=?1 AND task_id=?2`, machineID, control.TaskID).Scan(&runID, &runGeneration)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return LocalTaskDeleteResult{}, wrapLocalTaskDeleteError("locate analysis run", err)
	}

	if err == nil {
		statements := []struct {
			name  string
			query string
			args  []any
		}{
			{name: "delete current analysis", query: `DELETE FROM local_current_analysis WHERE machine_id=?1 AND run_id=?2`, args: []any{machineID, runID}},
			{name: "make delete events self-contained", query: `
			UPDATE local_outbox AS o
			SET payload_json=json_set(o.payload_json,'$.run_id',?2)
			WHERE o.topic='local.delete'
			  AND o.ack_at IS NULL
			  AND o.generation=?3
			  AND EXISTS (
				SELECT 1
				FROM local_delete_batches b
				JOIN local_delete_items i
				  ON i.machine_id=b.machine_id AND i.batch_id=b.batch_id
				WHERE b.machine_id=?1 AND b.run_id=?2
				  AND json_extract(o.payload_json,'$.machine_id')=b.machine_id
				  AND json_extract(o.payload_json,'$.batch_id')=b.batch_id
				  AND json_extract(o.payload_json,'$.file_id')=i.file_id
				  AND json_extract(o.payload_json,'$.sha512')=i.sha512
				  AND json_extract(o.payload_json,'$.status')='deleted'
				  AND i.result='deleted'
				  AND o.entity_key=b.batch_id || ':' || CAST(i.file_id AS TEXT)
			  )`, args: []any{machineID, runID, runGeneration}},
			{name: "detach delete batches", query: `UPDATE local_delete_batches SET run_id=NULL WHERE machine_id=?1 AND run_id=?2`, args: []any{machineID, runID}},
			{name: "delete task-owned sync events", query: `
			DELETE FROM local_outbox
			WHERE ack_at IS NULL
			  AND (
				(topic='local.analysis.published'
				 AND generation=?3
				 AND entity_key=?2
				 AND json_extract(payload_json,'$.machine_id')=?1
				 AND json_extract(payload_json,'$.run_id')=?2
				 AND json_extract(payload_json,'$.generation')=?3)
				OR
				(topic='local_analysis.stage'
				 AND generation=?3
				 AND substr(entity_key,1,length(?2)+1)=?2 || ':'
				 AND json_extract(payload_json,'$.run_id')=?2)
				OR
				(topic='local.review'
				 AND generation=?3
				 AND entity_key=json_extract(payload_json,'$.group_id')
				 AND json_extract(payload_json,'$.machine_id')=?1
				 AND json_extract(payload_json,'$.run_id')=?2
				 AND json_extract(payload_json,'$.generation')=?3
				 AND EXISTS (
					SELECT 1 FROM local_dup_groups g
					WHERE g.machine_id=?1 AND g.run_id=?2 AND g.generation=?3
					  AND g.group_id=local_outbox.entity_key
				 ))
			  )`, args: []any{machineID, runID, runGeneration}},
			{name: "delete reviews", query: `DELETE FROM local_reviews WHERE machine_id=?1 AND run_id=?2`, args: []any{machineID, runID}},
			{name: "delete duplicate members", query: `DELETE FROM local_dup_members WHERE machine_id=?1 AND run_id=?2`, args: []any{machineID, runID}},
			{name: "delete duplicate groups", query: `DELETE FROM local_dup_groups WHERE machine_id=?1 AND run_id=?2`, args: []any{machineID, runID}},
			{name: "delete pair scores", query: `DELETE FROM local_pair_scores WHERE machine_id=?1 AND run_id=?2`, args: []any{machineID, runID}},
			{name: "delete analysis run", query: `DELETE FROM local_analysis_runs WHERE machine_id=?1 AND run_id=?2 AND task_id=?3`, args: []any{machineID, runID, control.TaskID}},
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement.query, statement.args...); err != nil {
				return LocalTaskDeleteResult{}, wrapLocalTaskDeleteError(statement.name, err)
			}
		}
	}

	deletedAt = time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO local_task_deletion_receipts(machine_id,task_id,instance_id,deleted_at)
		VALUES (?1,?2,?3,?4)`,
		machineID, control.TaskID, control.InstanceID, deletedAt,
	); err != nil {
		return LocalTaskDeleteResult{}, wrapLocalTaskDeleteError("insert receipt", err)
	}
	deleteResult, err := tx.ExecContext(ctx, `
		DELETE FROM local_tasks
		WHERE machine_id=?1 AND task_id=?2 AND instance_id=?3 AND revision=?4`,
		machineID, control.TaskID, control.InstanceID, control.ExpectedRevision,
	)
	if err != nil {
		return LocalTaskDeleteResult{}, wrapLocalTaskDeleteError("delete task", err)
	}
	changed, err := deleteResult.RowsAffected()
	if err != nil {
		return LocalTaskDeleteResult{}, wrapLocalTaskDeleteError("count deleted tasks", err)
	}
	if changed != 1 {
		return LocalTaskDeleteResult{}, fmt.Errorf("%w: task %s", ErrLocalTaskStale, control.TaskID)
	}
	if err := tx.Commit(); err != nil {
		return LocalTaskDeleteResult{}, wrapLocalTaskDeleteError("commit", err)
	}
	return LocalTaskDeleteResult{Deleted: true, DeletedAt: deletedAt}, nil
}

func wrapLocalTaskDeleteError(operation string, err error) error {
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		switch sqliteErr.Code() & 0xff {
		case 5, 6: // SQLITE_BUSY, SQLITE_LOCKED
			return fmt.Errorf("store: delete local task data: %s: %w: %w", operation, ErrLocalTaskDeleteRetryable, err)
		}
	}
	return fmt.Errorf("store: delete local task data: %s: %w", operation, err)
}
