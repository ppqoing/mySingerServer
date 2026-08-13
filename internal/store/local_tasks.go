package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrLocalTaskConflict = errors.New("task_conflict")
var ErrLocalTaskProgressRollback = errors.New("task_progress_rollback")
var ErrLocalTaskTransition = errors.New("task_transition")

type LocalTaskCreate struct {
	TaskID         string
	MachineID      string
	Source         string
	Type           string
	Stage          int
	EnvelopeDigest string
	Envelope       []byte
}

type LocalTask struct {
	TaskID           string
	MachineID        string
	Source           string
	Type             string
	Stage            int
	Status           string
	EnvelopeDigest   string
	Envelope         []byte
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
			(task_id,machine_id,source,type,stage,status,envelope_digest,envelope,created_at,updated_at)
		VALUES (?1,?2,?3,?4,?5,'pending',?6,?7,?8,?8)
		ON CONFLICT(task_id) DO NOTHING`,
		in.TaskID, in.MachineID, in.Source, in.Type, in.Stage, in.EnvelopeDigest, in.Envelope, now,
	); err != nil {
		return LocalTask{}, fmt.Errorf("store: create local task: %w", err)
	}
	task, err := d.loadLocalTask(ctx, in.TaskID)
	if err != nil {
		return LocalTask{}, err
	}
	if task.MachineID != in.MachineID || task.Source != in.Source ||
		task.Type != in.Type ||
		task.EnvelopeDigest != in.EnvelopeDigest || !bytes.Equal(task.Envelope, in.Envelope) {
		return LocalTask{}, fmt.Errorf("%w: task %s", ErrLocalTaskConflict, in.TaskID)
	}
	return task, nil
}

func validateLocalTaskCreate(in LocalTaskCreate) error {
	if in.TaskID == "" || in.MachineID == "" || in.EnvelopeDigest == "" || len(in.Envelope) == 0 {
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
		SELECT task_id,machine_id,source,type,stage,status,envelope_digest,envelope,
		       progress_completed,progress_total,stats_json,
		       safe_error_code,safe_error_message,created_at,updated_at
		FROM local_tasks WHERE task_id=?1`, taskID).Scan(
		&task.TaskID, &task.MachineID, &task.Source, &task.Type, &task.Stage,
		&task.Status, &task.EnvelopeDigest, &task.Envelope, &task.ProgressComplete,
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

func (d *DB) LoadLocalTask(ctx context.Context, machineID, taskID string) (LocalTask, error) {
	if machineID == "" || taskID == "" {
		return LocalTask{}, fmt.Errorf("store: load local task: machine and task are required")
	}
	task, err := d.loadLocalTask(ctx, taskID)
	if err != nil {
		return LocalTask{}, err
	}
	if task.MachineID != machineID {
		return LocalTask{}, sql.ErrNoRows
	}
	task.Envelope = append([]byte(nil), task.Envelope...)
	return task, nil
}

func (d *DB) RecoverLocalTasks(ctx context.Context, machineID string) ([]LocalTask, error) {
	if machineID == "" {
		return nil, fmt.Errorf("store: recover local tasks: empty machine ID")
	}
	now := time.Now().UnixMilli()
	if _, err := d.db.ExecContext(ctx, `
		UPDATE local_tasks SET status='waiting_recovery',updated_at=?2
		WHERE machine_id=?1 AND status IN ('pending','running')`, machineID, now); err != nil {
		return nil, fmt.Errorf("store: recover local tasks: %w", err)
	}
	rows, err := d.db.QueryContext(ctx, `
		SELECT task_id FROM local_tasks
		WHERE machine_id=?1 AND status IN ('pending','waiting_recovery')
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

type LocalTaskUpdate struct {
	Status           string
	Stage            int
	ProgressComplete int64
	ProgressTotal    int64
	StatsJSON        string
	SafeErrorCode    *string
	SafeErrorMessage *string
}

func (d *DB) ListLocalTasks(ctx context.Context, machineID string, offset, limit int) ([]LocalTask, error) {
	if machineID == "" || offset < 0 {
		return nil, fmt.Errorf("store: invalid local task list")
	}
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	rows, err := d.db.QueryContext(ctx, `SELECT task_id FROM local_tasks WHERE machine_id=?1 ORDER BY task_id LIMIT ?2 OFFSET ?3`, machineID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("store: list local tasks: %w", err)
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
	tasks := make([]LocalTask, 0, len(ids))
	for _, id := range ids {
		task, err := d.loadLocalTask(ctx, id)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}

func (d *DB) TransitionLocalTask(ctx context.Context, machineID, taskID string, update LocalTaskUpdate) (LocalTask, error) {
	if machineID == "" || taskID == "" || !validLocalTaskStatus(update.Status) || update.Stage < 0 || update.Stage > 3 || update.ProgressComplete < 0 || update.ProgressTotal < 0 || (update.ProgressTotal > 0 && update.ProgressComplete > update.ProgressTotal) {
		return LocalTask{}, fmt.Errorf("%w: invalid update", ErrLocalTaskTransition)
	}
	if update.StatsJSON == "" {
		update.StatsJSON = "{}"
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return LocalTask{}, err
	}
	defer tx.Rollback()
	var status string
	var completed, total int64
	var stage int
	if err := tx.QueryRowContext(ctx, `SELECT status,stage,progress_completed,progress_total FROM local_tasks WHERE machine_id=?1 AND task_id=?2`, machineID, taskID).Scan(&status, &stage, &completed, &total); err != nil {
		return LocalTask{}, fmt.Errorf("store: load local task transition: %w", err)
	}
	if !allowedLocalTaskTransition(status, update.Status) || update.Stage < stage || (total != 0 && update.ProgressTotal != total) {
		return LocalTask{}, fmt.Errorf("%w: %s to %s", ErrLocalTaskTransition, status, update.Status)
	}
	if update.ProgressComplete < completed {
		return LocalTask{}, ErrLocalTaskProgressRollback
	}
	now := time.Now().UnixMilli()
	_, err = tx.ExecContext(ctx, `UPDATE local_tasks SET status=?3,stage=?4,progress_completed=?5,progress_total=?6,stats_json=?7,safe_error_code=?8,safe_error_message=?9,updated_at=?10,started_at=CASE WHEN ?3='running' THEN COALESCE(started_at,?10) ELSE started_at END,completed_at=CASE WHEN ?3 IN ('succeeded','failed','cancelled') THEN ?10 ELSE NULL END WHERE machine_id=?1 AND task_id=?2`, machineID, taskID, update.Status, update.Stage, update.ProgressComplete, update.ProgressTotal, update.StatsJSON, update.SafeErrorCode, update.SafeErrorMessage, now)
	if err != nil {
		return LocalTask{}, fmt.Errorf("store: transition local task: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return LocalTask{}, err
	}
	return d.loadLocalTask(ctx, taskID)
}

func (d *DB) CancelLocalTask(ctx context.Context, machineID, taskID string) error {
	task, err := d.loadLocalTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.MachineID != machineID {
		return sql.ErrNoRows
	}
	if task.Status == "cancelled" {
		return nil
	}
	_, err = d.TransitionLocalTask(ctx, machineID, taskID, LocalTaskUpdate{Status: "cancelled", Stage: task.Stage, ProgressComplete: task.ProgressComplete, ProgressTotal: task.ProgressTotal, StatsJSON: task.StatsJSON})
	return err
}

func (d *DB) RetryLocalTask(ctx context.Context, machineID, taskID string) (LocalTask, error) {
	task, err := d.loadLocalTask(ctx, taskID)
	if err != nil {
		return LocalTask{}, err
	}
	if task.MachineID != machineID {
		return LocalTask{}, sql.ErrNoRows
	}
	return d.TransitionLocalTask(ctx, machineID, taskID, LocalTaskUpdate{Status: "pending", Stage: task.Stage, ProgressComplete: task.ProgressComplete, ProgressTotal: task.ProgressTotal, StatsJSON: task.StatsJSON})
}

func allowedLocalTaskTransition(from, to string) bool {
	if from == to {
		return from == "running"
	}
	switch from {
	case "pending":
		return to == "running" || to == "cancelled"
	case "running":
		return to == "waiting_recovery" || to == "succeeded" || to == "failed" || to == "cancelled"
	case "waiting_recovery":
		return to == "running" || to == "cancelled" || to == "failed"
	case "failed", "cancelled":
		return to == "pending"
	default:
		return false
	}
}

func validLocalTaskStatus(status string) bool {
	switch status {
	case "pending", "running", "waiting_recovery", "succeeded", "failed", "cancelled":
		return true
	default:
		return false
	}
}
