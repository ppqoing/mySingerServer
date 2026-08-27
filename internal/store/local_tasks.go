package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

var ErrLocalTaskConflict = errors.New("task_conflict")
var ErrLocalTaskDeleteRetryable = errors.New("task_delete_retryable")
var ErrLocalTaskInstanceMismatch = errors.New("task_instance_mismatch")
var ErrLocalTaskProgressRollback = errors.New("task_progress_rollback")
var ErrLocalTaskStale = errors.New("stale_task")
var ErrLocalTaskTransition = errors.New("task_transition")

type LocalTaskVersion struct {
	InstanceID string
	Revision   int64
}

type LocalTaskControl struct {
	TaskID           string
	InstanceID       string
	ExpectedRevision int64
}

type LocalTaskProgressUpdate struct {
	Phase              string
	Stage              int
	ProgressComplete   int64
	ProgressTotal      int64
	ProgressTotalKnown bool
	StatsJSON          string
}

var localTaskPhaseOrder = map[string]int{
	"waiting": 0, "scan": 1, "stage1": 2,
	"stage2": 3, "stage3": 4, "finalizing": 5,
}

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
	TaskID             string
	InstanceID         string
	Revision           int64
	MachineID          string
	Source             string
	Type               string
	Stage              int
	Status             string
	Phase              string
	EnvelopeDigest     string
	Envelope           []byte
	ProgressComplete   int64
	ProgressTotal      int64
	ProgressTotalKnown bool
	StatsJSON          string
	SafeErrorCode      *string
	SafeErrorMessage   *string
	CreatedAt          int64
	UpdatedAt          int64
	StartedAt          *int64
	CompletedAt        *int64
}

func (d *DB) CreateOrLoadLocalTask(ctx context.Context, in LocalTaskCreate) (LocalTask, error) {
	if err := validateLocalTaskCreate(in); err != nil {
		return LocalTask{}, err
	}
	now := time.Now().UnixMilli()
	if _, err := d.db.ExecContext(ctx, `
		INSERT INTO local_tasks
			(task_id,instance_id,revision,machine_id,source,type,stage,status,phase,envelope_digest,envelope,progress_total_known,created_at,updated_at)
		VALUES (?1,?2,1,?3,?4,?5,?6,'pending',?7,?8,?9,0,?10,?10)
		ON CONFLICT(task_id) DO NOTHING`,
		in.TaskID, uuid.NewString(), in.MachineID, in.Source, in.Type, in.Stage, localTaskPhase("pending", in.Stage), in.EnvelopeDigest, in.Envelope, now,
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
	return scanLocalTask(d.db.QueryRowContext(ctx, `
		SELECT task_id,instance_id,revision,machine_id,source,type,stage,status,phase,envelope_digest,envelope,
		       progress_completed,progress_total,progress_total_known,stats_json,
		       safe_error_code,safe_error_message,created_at,updated_at,started_at,completed_at
		FROM local_tasks WHERE task_id=?1`, taskID))
}

func loadLocalTaskTx(ctx context.Context, tx *sql.Tx, machineID, taskID string) (LocalTask, error) {
	return scanLocalTask(tx.QueryRowContext(ctx, `
		SELECT task_id,instance_id,revision,machine_id,source,type,stage,status,phase,envelope_digest,envelope,
		       progress_completed,progress_total,progress_total_known,stats_json,
		       safe_error_code,safe_error_message,created_at,updated_at,started_at,completed_at
		FROM local_tasks WHERE machine_id=?1 AND task_id=?2`, machineID, taskID))
}

func scanLocalTask(row *sql.Row) (LocalTask, error) {
	var task LocalTask
	var errorCode, errorMessage sql.NullString
	var progressTotalKnown int
	var startedAt, completedAt sql.NullInt64
	err := row.Scan(
		&task.TaskID, &task.InstanceID, &task.Revision, &task.MachineID, &task.Source, &task.Type, &task.Stage,
		&task.Status, &task.Phase, &task.EnvelopeDigest, &task.Envelope, &task.ProgressComplete,
		&task.ProgressTotal, &progressTotalKnown, &task.StatsJSON, &errorCode, &errorMessage,
		&task.CreatedAt, &task.UpdatedAt, &startedAt, &completedAt,
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
	task.ProgressTotalKnown = progressTotalKnown == 1
	if startedAt.Valid {
		value := startedAt.Int64
		task.StartedAt = &value
	}
	if completedAt.Valid {
		value := completedAt.Int64
		task.CompletedAt = &value
	}
	return task, nil
}

func localTaskPhase(status string, stage int) string {
	if stage == 0 {
		if status == "pending" {
			return "waiting"
		}
		return "scan"
	}
	switch stage {
	case 1:
		return "stage1"
	case 2:
		return "stage3"
	default:
		return "finalizing"
	}
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
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `
		UPDATE local_tasks
		SET status=CASE
				WHEN status IN ('pending','running') THEN 'waiting_recovery'
				WHEN status='pausing' THEN 'paused'
				WHEN status='stopping' THEN 'cancelled'
			END,
			revision=revision+1,
			updated_at=?2,
			completed_at=CASE WHEN status='stopping' THEN ?2 ELSE NULL END
		WHERE machine_id=?1 AND status IN ('pending','running','pausing','stopping')`, machineID, now); err != nil {
		return nil, fmt.Errorf("store: recover local tasks: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
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
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
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
	rows, err := d.db.QueryContext(ctx, `SELECT task_id FROM local_tasks WHERE machine_id=?1 ORDER BY created_at DESC,task_id DESC LIMIT ?2 OFFSET ?3`, machineID, limit, offset)
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
	task, err := d.LoadLocalTask(ctx, machineID, taskID)
	if err != nil {
		return LocalTask{}, err
	}
	if task.Status == update.Status {
		if task.Status != "running" {
			return LocalTask{}, fmt.Errorf("%w: %s to %s", ErrLocalTaskTransition, task.Status, update.Status)
		}
	} else if !allowedLocalTaskTransition(task.Status, update.Status) {
		return LocalTask{}, fmt.Errorf("%w: %s to %s", ErrLocalTaskTransition, task.Status, update.Status)
	}
	phase := localTaskPhase(update.Status, update.Stage)
	if phase != task.Phase || update.Stage != task.Stage || update.ProgressComplete != task.ProgressComplete ||
		update.ProgressTotal != task.ProgressTotal || update.StatsJSON != task.StatsJSON {
		task, err = d.UpdateLocalTaskProgress(ctx, machineID, controlForLocalTask(task), LocalTaskProgressUpdate{
			Phase: phase, Stage: update.Stage, ProgressComplete: update.ProgressComplete,
			ProgressTotal: update.ProgressTotal, ProgressTotalKnown: task.ProgressTotalKnown || update.ProgressTotal > 0,
			StatsJSON: update.StatsJSON,
		})
		if err != nil {
			return LocalTask{}, err
		}
	}
	if task.Status == update.Status {
		return task, nil
	}
	return d.TransitionLocalTaskLifecycle(ctx, machineID, controlForLocalTask(task), update.Status, update.SafeErrorCode, update.SafeErrorMessage)
}

func (d *DB) TransitionLocalTaskLifecycle(
	ctx context.Context,
	machineID string,
	control LocalTaskControl,
	toStatus string,
	safeCode *string,
	safeMessage *string,
) (LocalTask, error) {
	if err := validateLocalTaskControl(machineID, control); err != nil {
		return LocalTask{}, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return LocalTask{}, err
	}
	defer tx.Rollback()
	task, err := loadLocalTaskTx(ctx, tx, machineID, control.TaskID)
	if err != nil {
		return LocalTask{}, fmt.Errorf("store: load local task lifecycle: %w", err)
	}
	if task.InstanceID != control.InstanceID {
		return LocalTask{}, fmt.Errorf("%w: task %s", ErrLocalTaskInstanceMismatch, control.TaskID)
	}
	if task.Revision != control.ExpectedRevision {
		return LocalTask{}, fmt.Errorf("%w: task %s", ErrLocalTaskStale, control.TaskID)
	}
	if !validLocalTaskStatus(toStatus) {
		return LocalTask{}, fmt.Errorf("%w: invalid status %q", ErrLocalTaskTransition, toStatus)
	}
	if !allowedLocalTaskTransition(task.Status, toStatus) {
		return LocalTask{}, fmt.Errorf("%w: %s to %s", ErrLocalTaskTransition, task.Status, toStatus)
	}
	now := time.Now().UnixMilli()
	result, err := tx.ExecContext(ctx, `
		UPDATE local_tasks
		SET status=?5,
		    revision=revision+1,
		    safe_error_code=?6,
		    safe_error_message=?7,
		    updated_at=?8,
		    started_at=CASE WHEN ?5='running' THEN COALESCE(started_at,?8) ELSE started_at END,
		    completed_at=CASE WHEN ?5 IN ('succeeded','failed','cancelled') THEN ?8 ELSE NULL END
		WHERE machine_id=?1 AND task_id=?2 AND instance_id=?3 AND revision=?4`,
		machineID, control.TaskID, control.InstanceID, control.ExpectedRevision,
		toStatus, safeCode, safeMessage, now,
	)
	if err != nil {
		return LocalTask{}, fmt.Errorf("store: transition local task lifecycle: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return LocalTask{}, err
	}
	if changed == 0 {
		return LocalTask{}, fmt.Errorf("%w: task %s", ErrLocalTaskStale, control.TaskID)
	}
	updated, err := loadLocalTaskTx(ctx, tx, machineID, control.TaskID)
	if err != nil {
		return LocalTask{}, err
	}
	if err := tx.Commit(); err != nil {
		return LocalTask{}, err
	}
	return updated, nil
}

func (d *DB) UpdateLocalTaskProgress(
	ctx context.Context,
	machineID string,
	control LocalTaskControl,
	update LocalTaskProgressUpdate,
) (LocalTask, error) {
	if err := validateLocalTaskControl(machineID, control); err != nil {
		return LocalTask{}, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return LocalTask{}, err
	}
	defer tx.Rollback()
	task, err := loadLocalTaskTx(ctx, tx, machineID, control.TaskID)
	if err != nil {
		return LocalTask{}, fmt.Errorf("store: load local task progress: %w", err)
	}
	if task.InstanceID != control.InstanceID {
		return LocalTask{}, fmt.Errorf("%w: task %s", ErrLocalTaskInstanceMismatch, control.TaskID)
	}
	if task.Revision != control.ExpectedRevision {
		return LocalTask{}, fmt.Errorf("%w: task %s", ErrLocalTaskStale, control.TaskID)
	}
	if _, ok := localTaskPhaseOrder[update.Phase]; !ok || update.Stage < 0 || update.Stage > 3 ||
		update.ProgressComplete < 0 || update.ProgressTotal < 0 ||
		((update.ProgressTotalKnown || update.ProgressTotal > 0) && update.ProgressComplete > update.ProgressTotal) {
		return LocalTask{}, fmt.Errorf("%w: invalid progress update", ErrLocalTaskTransition)
	}
	if update.StatsJSON == "" {
		update.StatsJSON = "{}"
	}
	oldPhase := localTaskPhaseOrder[task.Phase]
	newPhase := localTaskPhaseOrder[update.Phase]
	if newPhase < oldPhase || (newPhase == oldPhase &&
		(update.ProgressComplete < task.ProgressComplete || update.ProgressTotal < task.ProgressTotal ||
			task.ProgressTotalKnown && !update.ProgressTotalKnown)) {
		return LocalTask{}, ErrLocalTaskProgressRollback
	}
	now := time.Now().UnixMilli()
	result, err := tx.ExecContext(ctx, `
		UPDATE local_tasks
		SET phase=?5,stage=?6,progress_completed=?7,progress_total=?8,
		    progress_total_known=?9,stats_json=?10,updated_at=?11
		WHERE machine_id=?1 AND task_id=?2 AND instance_id=?3 AND revision=?4`,
		machineID, control.TaskID, control.InstanceID, control.ExpectedRevision,
		update.Phase, update.Stage, update.ProgressComplete, update.ProgressTotal,
		boolToInt(update.ProgressTotalKnown), update.StatsJSON, now,
	)
	if err != nil {
		return LocalTask{}, fmt.Errorf("store: update local task progress: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return LocalTask{}, err
	}
	if changed == 0 {
		return LocalTask{}, fmt.Errorf("%w: task %s", ErrLocalTaskStale, control.TaskID)
	}
	updated, err := loadLocalTaskTx(ctx, tx, machineID, control.TaskID)
	if err != nil {
		return LocalTask{}, err
	}
	if err := tx.Commit(); err != nil {
		return LocalTask{}, err
	}
	return updated, nil
}

func (d *DB) CancelLocalTask(ctx context.Context, machineID, taskID string) error {
	task, err := d.LoadLocalTask(ctx, machineID, taskID)
	if err != nil {
		return err
	}
	if task.Status == "cancelled" {
		return nil
	}
	if task.Status != "paused" && task.Status != "stopping" {
		task, err = d.TransitionLocalTaskLifecycle(ctx, machineID, controlForLocalTask(task), "stopping", nil, nil)
		if err != nil {
			return err
		}
	}
	_, err = d.TransitionLocalTaskLifecycle(ctx, machineID, controlForLocalTask(task), "cancelled", nil, nil)
	return err
}

func (d *DB) RetryLocalTask(ctx context.Context, machineID, taskID string) (LocalTask, error) {
	task, err := d.LoadLocalTask(ctx, machineID, taskID)
	if err != nil {
		return LocalTask{}, err
	}
	return d.TransitionLocalTaskLifecycle(ctx, machineID, controlForLocalTask(task), "pending", nil, nil)
}

func allowedLocalTaskTransition(from, to string) bool {
	switch from {
	case "pending":
		return to == "running" || to == "pausing" || to == "stopping" || to == "deleting" || to == "waiting_recovery"
	case "running":
		return to == "pausing" || to == "stopping" || to == "deleting" || to == "succeeded" || to == "failed" || to == "waiting_recovery"
	case "waiting_recovery":
		return to == "running" || to == "pausing" || to == "stopping" || to == "deleting" || to == "failed"
	case "pausing":
		return to == "paused" || to == "stopping" || to == "deleting" || to == "failed" || to == "waiting_recovery"
	case "paused":
		return to == "pending" || to == "cancelled" || to == "deleting"
	case "stopping":
		return to == "cancelled" || to == "deleting" || to == "failed" || to == "waiting_recovery"
	case "failed", "cancelled":
		return to == "pending" || to == "deleting"
	case "deleting":
		return to == "delete_failed"
	case "succeeded", "delete_failed":
		return to == "deleting"
	default:
		return false
	}
}

func validLocalTaskStatus(status string) bool {
	switch status {
	case "pending", "running", "waiting_recovery", "pausing", "paused", "stopping", "cancelled", "succeeded", "failed", "deleting", "delete_failed":
		return true
	default:
		return false
	}
}

func validateLocalTaskControl(machineID string, control LocalTaskControl) error {
	if machineID == "" || control.TaskID == "" || control.InstanceID == "" || control.ExpectedRevision <= 0 {
		return fmt.Errorf("%w: invalid task control", ErrLocalTaskTransition)
	}
	return nil
}

func controlForLocalTask(task LocalTask) LocalTaskControl {
	return LocalTaskControl{TaskID: task.TaskID, InstanceID: task.InstanceID, ExpectedRevision: task.Revision}
}
