package phase2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"dedup/internal/config"
	"dedup/internal/proto"
)

const phase2TargetType = "phase2"

var ErrTaskEnvelopeConflict = errors.New("phase2: task ID envelope conflict")

type postgresStore struct {
	pool postgresDB
}

type postgresDB interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type phase2Target struct {
	Type      string           `json:"type"`
	MachineID string           `json:"machine_id"`
	Task      proto.Phase2Task `json:"task"`
}

type persistedStats struct {
	Stats   proto.TaskStats `json:"stats"`
	LastErr string          `json:"last_err,omitempty"`
}

func (store *postgresStore) auditPendingTargets(ctx context.Context) error {
	rows, err := store.pool.Query(ctx, `
		SELECT DISTINCT target ? 'type', target->>'type'
		FROM scan_tasks
		WHERE status IN ('sent','acked','running')
		ORDER BY 1,2 NULLS FIRST`)
	if err != nil {
		return fmt.Errorf("phase2: audit pending task targets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			hasType    bool
			targetType *string
		)
		if err := rows.Scan(&hasType, &targetType); err != nil {
			return fmt.Errorf("phase2: scan pending target discriminator: %w", err)
		}
		if !hasType {
			continue
		}
		if targetType == nil ||
			(*targetType != "scan" && *targetType != phase2TargetType) {
			value := "<null>"
			if targetType != nil {
				value = *targetType
			}
			return fmt.Errorf(
				"phase2: unknown pending target discriminator %q",
				value,
			)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("phase2: read pending target discriminators: %w", err)
	}
	return nil
}

// NewDispatcher constructs the production PostgreSQL-backed Phase2
// dispatcher.
func NewDispatcher(
	pool postgresDB,
	sender Sender,
	cfg config.Phase2Config,
	logger *slog.Logger,
) *Dispatcher {
	store := &postgresStore{pool: pool}
	return newDispatcher(store, sender, cfg, logger)
}

func (store *postgresStore) loadBuildSnapshot(
	ctx context.Context,
	kind uint8,
) (snapshot buildSnapshot, err error) {
	if store.pool == nil {
		return snapshot, fmt.Errorf("phase2: PostgreSQL pool is nil")
	}
	candidateKind := candidateImage
	if kind == proto.KindVideo {
		candidateKind = candidateVideo
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return snapshot, fmt.Errorf("phase2: begin build snapshot: %w", err)
	}
	defer func() {
		if err != nil {
			rollbackCtx, cancel := context.WithTimeout(
				context.WithoutCancel(ctx),
				5*time.Second,
			)
			defer cancel()
			_ = tx.Rollback(rollbackCtx)
		}
	}()

	rows, err := tx.Query(ctx, `
		SELECT g.id,g.kind,m.file_id,f.sha512,f.status
		FROM dup_groups AS g
		JOIN dup_members AS m ON m.group_id=g.id
		JOIN files AS f ON f.id=m.file_id
		WHERE g.kind=$1
		  AND f.status <> 'deleted'
		ORDER BY g.id,m.file_id`,
		candidateKind,
	)
	if err != nil {
		return snapshot, fmt.Errorf("phase2: query candidate members: %w", err)
	}
	var (
		lastGroupID int64
		haveGroup   bool
		current     candidateGroup
	)
	for rows.Next() {
		var groupID int64
		var member candidateMember
		if err := rows.Scan(
			&groupID,
			&current.Kind,
			&member.FileID,
			&member.SHA512,
			&member.Status,
		); err != nil {
			rows.Close()
			return snapshot, fmt.Errorf("phase2: scan candidate member: %w", err)
		}
		if !haveGroup || groupID != lastGroupID {
			if haveGroup {
				snapshot.Groups = append(snapshot.Groups, current)
			}
			current = candidateGroup{Kind: current.Kind}
			lastGroupID = groupID
			haveGroup = true
		}
		current.Members = append(current.Members, member)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return snapshot, fmt.Errorf("phase2: read candidate members: %w", err)
	}
	rows.Close()
	if haveGroup {
		snapshot.Groups = append(snapshot.Groups, current)
	}

	pairs, err := normalizedPairs(snapshot.Groups, candidateKind)
	if err != nil {
		return snapshot, err
	}
	shaSet := make(map[string]struct{}, len(pairs)*2)
	for _, pair := range pairs {
		shaSet[pair[0]] = struct{}{}
		shaSet[pair[1]] = struct{}{}
	}
	shas := make([]string, 0, len(shaSet))
	for sha := range shaSet {
		shas = append(shas, sha)
	}
	sort.Strings(shas)
	snapshot.Features = make(map[string]featureState, len(shas))
	if len(shas) > 0 {
		if err := store.loadCopies(ctx, tx, shas, &snapshot); err != nil {
			return snapshot, err
		}
		if kind == proto.KindImage {
			err = store.loadImageFeatureState(ctx, tx, shas, &snapshot)
		} else {
			err = store.loadVideoFeatureState(ctx, tx, shas, &snapshot)
		}
		if err != nil {
			return snapshot, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return snapshot, fmt.Errorf("phase2: commit build snapshot: %w", err)
	}
	return snapshot, nil
}

func (store *postgresStore) loadCopies(
	ctx context.Context,
	tx pgx.Tx,
	shas []string,
	snapshot *buildSnapshot,
) error {
	rows, err := tx.Query(ctx, `
		SELECT id,machine_id,path,sha512,size,mtime,status
		FROM files
		WHERE sha512=ANY($1::text[])
		  AND status <> 'deleted'
		ORDER BY machine_id,sha512,path,id`,
		shas,
	)
	if err != nil {
		return fmt.Errorf("phase2: query live file copies: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var copy fileCopy
		if err := rows.Scan(
			&copy.ID,
			&copy.MachineID,
			&copy.Path,
			&copy.SHA512,
			&copy.Size,
			&copy.MTime,
			&copy.Status,
		); err != nil {
			return fmt.Errorf("phase2: scan live file copy: %w", err)
		}
		snapshot.Copies = append(snapshot.Copies, copy)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("phase2: read live file copies: %w", err)
	}
	return nil
}

func (store *postgresStore) loadImageFeatureState(
	ctx context.Context,
	tx pgx.Tx,
	shas []string,
	snapshot *buildSnapshot,
) error {
	rows, err := tx.Query(ctx, `
		SELECT sha512,phash_parts,sobel_hist
		FROM image_features
		WHERE sha512=ANY($1::text[])
		ORDER BY sha512`,
		shas,
	)
	if err != nil {
		return fmt.Errorf("phase2: query image feature state: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sha string
		var state featureState
		if err := rows.Scan(&sha, &state.PHashParts, &state.SobelHist); err != nil {
			return fmt.Errorf("phase2: scan image feature state: %w", err)
		}
		snapshot.Features[sha] = state
	}
	return rows.Err()
}

func (store *postgresStore) loadVideoFeatureState(
	ctx context.Context,
	tx pgx.Tx,
	shas []string,
	snapshot *buildSnapshot,
) error {
	rows, err := tx.Query(ctx, `
		SELECT sha512,duration_ms
		FROM video_features
		WHERE sha512=ANY($1::text[])
		ORDER BY sha512`,
		shas,
	)
	if err != nil {
		return fmt.Errorf("phase2: query video feature state: %w", err)
	}
	for rows.Next() {
		var sha string
		var duration *int64
		if err := rows.Scan(&sha, &duration); err != nil {
			rows.Close()
			return fmt.Errorf("phase2: scan video feature state: %w", err)
		}
		state := snapshot.Features[sha]
		if duration != nil {
			state.DurationMS = *duration
		}
		snapshot.Features[sha] = state
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("phase2: read video feature state: %w", err)
	}
	rows.Close()

	rows, err = tx.Query(ctx, `
		SELECT sha512,frame_idx,pdq256,phash_parts,sobel_hist
		FROM video_frames
		WHERE sha512=ANY($1::text[])
		ORDER BY sha512,frame_idx`,
		shas,
	)
	if err != nil {
		return fmt.Errorf("phase2: query video frame state: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sha string
		var frame frameFeature
		if err := rows.Scan(
			&sha,
			&frame.FrameIdx,
			&frame.PDQ256,
			&frame.PHashParts,
			&frame.SobelHist,
		); err != nil {
			return fmt.Errorf("phase2: scan video frame state: %w", err)
		}
		state := snapshot.Features[sha]
		state.Frames = append(state.Frames, frame)
		snapshot.Features[sha] = state
	}
	return rows.Err()
}

func (store *postgresStore) persistPending(
	ctx context.Context,
	task persistedTask,
) (persistedTask, error) {
	target := phase2Target{
		Type:      phase2TargetType,
		MachineID: task.Envelope.MachineID,
		Task:      task.Envelope.Task,
	}
	targetJSON, err := json.Marshal(target)
	if err != nil {
		return persistedTask{}, fmt.Errorf("phase2: marshal target: %w", err)
	}
	command, err := store.pool.Exec(ctx, `
		INSERT INTO scan_tasks(id,machine_id,phase,target,status)
		VALUES($1,$2,2,$3::jsonb,'sent')
		ON CONFLICT(id) DO NOTHING`,
		task.Envelope.Task.TaskID,
		task.Envelope.MachineID,
		targetJSON,
	)
	if err != nil {
		return persistedTask{}, fmt.Errorf("phase2: persist pending task: %w", err)
	}
	if command.RowsAffected() == 1 {
		task.Status = taskStatusSent
		return task, nil
	}

	var (
		machineID string
		phase     int
		rawTarget []byte
		status    string
		rawStats  []byte
	)
	if err := store.pool.QueryRow(ctx, `
		SELECT machine_id,phase,target,status,stats_json
		FROM scan_tasks
		WHERE id=$1`,
		task.Envelope.Task.TaskID,
	).Scan(&machineID, &phase, &rawTarget, &status, &rawStats); err != nil {
		return persistedTask{}, fmt.Errorf("phase2: load existing task: %w", err)
	}
	var existing phase2Target
	if err := json.Unmarshal(rawTarget, &existing); err != nil {
		return persistedTask{}, fmt.Errorf("phase2: decode existing target: %w", err)
	}
	if phase != 2 || machineID != task.Envelope.MachineID ||
		!reflect.DeepEqual(existing, target) {
		return persistedTask{}, fmt.Errorf(
			"%w: task_id=%s",
			ErrTaskEnvelopeConflict,
			task.Envelope.Task.TaskID,
		)
	}
	task.Status = status
	if !validPersistedTaskStatus(status) {
		return persistedTask{}, fmt.Errorf(
			"phase2: existing task has invalid status %q",
			status,
		)
	}
	decodePersistedStats(rawStats, &task)
	return task, nil
}

func (store *postgresStore) restorePending(
	ctx context.Context,
) ([]persistedTask, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT machine_id,target,status,stats_json
		FROM scan_tasks
		WHERE phase=2
		  AND target->>'type'=$1
		  AND status NOT IN ('done','failed')
		ORDER BY machine_id,id`,
		phase2TargetType,
	)
	if err != nil {
		return nil, fmt.Errorf("phase2: query restored tasks: %w", err)
	}
	defer rows.Close()
	var tasks []persistedTask
	for rows.Next() {
		var machineID, status string
		var rawTarget, rawStats []byte
		if err := rows.Scan(
			&machineID,
			&rawTarget,
			&status,
			&rawStats,
		); err != nil {
			return nil, fmt.Errorf("phase2: scan restored task: %w", err)
		}
		var target phase2Target
		if err := json.Unmarshal(rawTarget, &target); err != nil {
			return nil, fmt.Errorf("phase2: decode restored target: %w", err)
		}
		task := persistedTask{
			Envelope: RoutedTask{MachineID: machineID, Task: target.Task},
			Status:   status,
		}
		if !validPersistedTaskStatus(status) ||
			isTerminalTaskStatus(status) {
			return nil, fmt.Errorf(
				"phase2: restored task has invalid pending status %q",
				status,
			)
		}
		if err := validateRestoredTarget(target, task.Envelope); err != nil {
			return nil, err
		}
		decodePersistedStats(rawStats, &task)
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("phase2: read restored tasks: %w", err)
	}
	return tasks, nil
}

func validPersistedTaskStatus(status string) bool {
	switch status {
	case taskStatusSent, taskStatusAcked, taskStatusRunning,
		taskStatusDone, taskStatusFailed:
		return true
	default:
		return false
	}
}

func (store *postgresStore) updateTask(
	ctx context.Context,
	taskID string,
	machineID string,
	status string,
	stats proto.TaskStats,
	lastErr string,
) (durableTaskState, error) {
	if !validPersistedTaskStatus(status) {
		return durableTaskState{}, fmt.Errorf(
			"phase2: invalid requested task status %q",
			status,
		)
	}
	raw, err := json.Marshal(persistedStats{Stats: stats, LastErr: lastErr})
	if err != nil {
		return durableTaskState{}, fmt.Errorf(
			"phase2: marshal task stats: %w",
			err,
		)
	}
	var (
		durable  durableTaskState
		rawStats []byte
	)
	err = store.pool.QueryRow(ctx, `
		UPDATE scan_tasks
		SET
		  status=CASE
		    WHEN status IN ('done','failed') THEN status
		    ELSE $3
		  END,
		  stats_json=CASE
		    WHEN status IN ('done','failed') THEN stats_json
		    ELSE $4::jsonb
		  END,
		  updated_at=CASE
		    WHEN status IN ('done','failed') THEN updated_at
		    ELSE now()
		  END
		WHERE id=$1
		  AND machine_id=$2
		  AND phase=2
		  AND target->>'type'=$5
		RETURNING status,stats_json`,
		taskID,
		machineID,
		status,
		raw,
		phase2TargetType,
	).Scan(&durable.Status, &rawStats)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return durableTaskState{}, fmt.Errorf(
				"phase2: task %s disappeared during update",
				taskID,
			)
		}
		return durableTaskState{}, fmt.Errorf(
			"phase2: update task state: %w",
			err,
		)
	}
	if !validPersistedTaskStatus(durable.Status) {
		return durableTaskState{}, fmt.Errorf(
			"phase2: updated task has invalid status %q",
			durable.Status,
		)
	}
	var document persistedStats
	if err := json.Unmarshal(rawStats, &document); err != nil {
		return durableTaskState{}, fmt.Errorf(
			"phase2: decode updated task stats: %w",
			err,
		)
	}
	durable.Stats = document.Stats
	durable.LastErr = document.LastErr
	return durable, nil
}

func validateRestoredTarget(target phase2Target, envelope RoutedTask) error {
	if target.Type != phase2TargetType ||
		target.MachineID == "" ||
		target.MachineID != envelope.MachineID ||
		target.Task.TaskID == "" ||
		len(target.Task.Items) == 0 {
		return fmt.Errorf("phase2: invalid restored target identity")
	}
	if len(target.Task.Items) > maxShardItems {
		return fmt.Errorf(
			"phase2: restored task has %d items, limit is %d",
			len(target.Task.Items),
			maxShardItems,
		)
	}
	for _, item := range target.Task.Items {
		if item.MachineID != target.MachineID {
			return fmt.Errorf("phase2: restored item machine mismatch")
		}
		if err := item.Validate(); err != nil {
			return fmt.Errorf("phase2: invalid restored item: %w", err)
		}
	}
	if stableTaskID(envelope) != target.Task.TaskID {
		return fmt.Errorf("phase2: restored task ID does not match envelope")
	}
	if _, err := proto.EncodeFramePayload(
		proto.MsgPhase2Task,
		&target.Task,
	); err != nil {
		return fmt.Errorf("phase2: invalid restored wire envelope: %w", err)
	}
	return nil
}

func decodePersistedStats(raw []byte, task *persistedTask) {
	if len(raw) == 0 {
		return
	}
	var document persistedStats
	if json.Unmarshal(raw, &document) == nil {
		task.Stats = document.Stats
		task.LastErr = document.LastErr
	}
}
