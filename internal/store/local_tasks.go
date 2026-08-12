package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrLocalTaskConflict = errors.New("task_conflict")

type LocalTaskCreate struct {
	TaskID         string
	MachineID      string
	Source         string
	Type           string
	Stage          int
	EnvelopeDigest string
}

type LocalTask struct {
	TaskID           string
	MachineID        string
	Source           string
	Type             string
	Stage            int
	Status           string
	EnvelopeDigest   string
	ProgressComplete int64
	ProgressTotal    int64
	StatsJSON        string
	SafeErrorCode    *string
	SafeErrorMessage *string
	CreatedAt        int64
	UpdatedAt        int64
}

func (d *DB) CreateOrLoadLocalTask(ctx context.Context, in LocalTaskCreate) (LocalTask, error) {
	if err := validateLocalTaskCreate(in); err != nil {
		return LocalTask{}, err
	}
	now := time.Now().UnixMilli()
	if _, err := d.db.ExecContext(ctx, `
		INSERT INTO local_tasks
			(task_id,machine_id,source,type,stage,status,envelope_digest,created_at,updated_at)
		VALUES (?1,?2,?3,?4,?5,'pending',?6,?7,?7)
		ON CONFLICT(task_id) DO NOTHING`,
		in.TaskID, in.MachineID, in.Source, in.Type, in.Stage, in.EnvelopeDigest, now,
	); err != nil {
		return LocalTask{}, fmt.Errorf("store: create local task: %w", err)
	}
	task, err := d.loadLocalTask(ctx, in.TaskID)
	if err != nil {
		return LocalTask{}, err
	}
	if task.MachineID != in.MachineID || task.Source != in.Source ||
		task.Type != in.Type || task.Stage != in.Stage ||
		task.EnvelopeDigest != in.EnvelopeDigest {
		return LocalTask{}, fmt.Errorf("%w: task %s", ErrLocalTaskConflict, in.TaskID)
	}
	return task, nil
}

func validateLocalTaskCreate(in LocalTaskCreate) error {
	if in.TaskID == "" || in.MachineID == "" || in.EnvelopeDigest == "" {
		return fmt.Errorf("store: local task requires task, machine, and envelope digest")
	}
	if in.Source != "local" && in.Source != "manager" {
		return fmt.Errorf("store: invalid local task source %q", in.Source)
	}
	switch in.Type {
	case "scan", "analysis", "stage2", "stage3", "delete":
	default:
		return fmt.Errorf("store: invalid local task type %q", in.Type)
	}
	if in.Stage < 0 || in.Stage > 3 {
		return fmt.Errorf("store: invalid local task stage %d", in.Stage)
	}
	return nil
}

func (d *DB) loadLocalTask(ctx context.Context, taskID string) (LocalTask, error) {
	var task LocalTask
	var errorCode, errorMessage sql.NullString
	err := d.db.QueryRowContext(ctx, `
		SELECT task_id,machine_id,source,type,stage,status,envelope_digest,
		       progress_completed,progress_total,stats_json,
		       safe_error_code,safe_error_message,created_at,updated_at
		FROM local_tasks WHERE task_id=?1`, taskID).Scan(
		&task.TaskID, &task.MachineID, &task.Source, &task.Type, &task.Stage,
		&task.Status, &task.EnvelopeDigest, &task.ProgressComplete,
		&task.ProgressTotal, &task.StatsJSON, &errorCode, &errorMessage,
		&task.CreatedAt, &task.UpdatedAt,
	)
	if err != nil {
		return LocalTask{}, fmt.Errorf("store: load local task: %w", err)
	}
	if errorCode.Valid {
		value := errorCode.String
		task.SafeErrorCode = &value
	}
	if errorMessage.Valid {
		value := errorMessage.String
		task.SafeErrorMessage = &value
	}
	return task, nil
}

func (d *DB) RecoverLocalTasks(ctx context.Context, machineID string) ([]LocalTask, error) {
	if machineID == "" {
		return nil, fmt.Errorf("store: recover local tasks: empty machine ID")
	}
	now := time.Now().UnixMilli()
	if _, err := d.db.ExecContext(ctx, `
		UPDATE local_tasks SET status='waiting_recovery',updated_at=?2
		WHERE machine_id=?1 AND status='running'`, machineID, now); err != nil {
		return nil, fmt.Errorf("store: recover local tasks: %w", err)
	}
	rows, err := d.db.QueryContext(ctx, `
		SELECT task_id FROM local_tasks
		WHERE machine_id=?1 AND status='waiting_recovery'
		ORDER BY created_at,task_id`, machineID)
	if err != nil {
		return nil, fmt.Errorf("store: list recovered local tasks: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make([]LocalTask, 0, len(ids))
	for _, id := range ids {
		task, err := d.loadLocalTask(ctx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, task)
	}
	return result, nil
}
