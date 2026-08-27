package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

const MaxLocalSyncBatch = 5_000

type LocalOutboxSyncRow struct {
	Sequence    int64
	MachineID   string
	Topic       string
	EntityKey   string
	Generation  int64
	PayloadJSON string
}

type LocalAnalysisRunSyncRow struct {
	MachineID   string
	RunID       string
	Generation  int64
	TaskID      string
	Status      string
	CreatedAt   int64
	CompletedAt *int64
	PublishedAt *int64
}

type LocalPairScoreSyncRow struct {
	MachineID   string
	RunID       string
	Generation  int64
	PairKey     string
	LeftFileID  int64
	RightFileID int64
	LeftSHA512  string
	RightSHA512 string
	Stage1JSON  string
	Stage2JSON  *string
	Stage3JSON  *string
	Verdict     string
}

type LocalGroupSyncRow struct {
	MachineID  string
	RunID      string
	Generation int64
	GroupID    string
	Category   string
	Verdict    string
}

type LocalMemberSyncRow struct {
	MachineID  string
	RunID      string
	Generation int64
	GroupID    string
	FileID     int64
	SHA512     string
}

type LocalReviewSyncRow struct {
	MachineID  string
	RunID      string
	Generation int64
	GroupID    string
	FileID     int64
	Decision   string
	Reviewer   string
	Note       string
	ReviewedAt int64
}

type LocalDeleteSyncRow struct {
	MachineID   string
	RunID       string
	Generation  int64
	BatchID     string
	FileID      int64
	Path        string
	SHA512      string
	Result      string
	Status      string
	ErrorCode   string
	Uncertain   bool
	CompletedAt int64
}

type LocalSyncBatch struct {
	Events  []LocalOutboxSyncRow
	Runs    []LocalAnalysisRunSyncRow
	Pairs   []LocalPairScoreSyncRow
	Groups  []LocalGroupSyncRow
	Members []LocalMemberSyncRow
	Reviews []LocalReviewSyncRow
	Deletes []LocalDeleteSyncRow
}

func (d *DB) PendingLocalSyncEvents(ctx context.Context, limit int) ([]LocalOutboxSyncRow, error) {
	if d == nil || d.db == nil {
		return nil, fmt.Errorf("store: local sync unavailable")
	}
	if limit <= 0 || limit > MaxLocalSyncBatch {
		limit = MaxLocalSyncBatch
	}
	rows, err := d.db.QueryContext(ctx, `
		SELECT sequence,topic,entity_key,generation,payload_json
		FROM local_outbox
		WHERE ack_at IS NULL AND (next_retry_at IS NULL OR next_retry_at<=?1)
		ORDER BY sequence LIMIT ?2`, time.Now().UnixMilli(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: pending local sync events: %w", err)
	}
	defer rows.Close()
	var events []LocalOutboxSyncRow
	for rows.Next() {
		var event LocalOutboxSyncRow
		if err := rows.Scan(&event.Sequence, &event.Topic, &event.EntityKey, &event.Generation, &event.PayloadJSON); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (d *DB) LoadLocalSyncBatch(ctx context.Context, events []LocalOutboxSyncRow) (LocalSyncBatch, error) {
	batch := LocalSyncBatch{Events: append([]LocalOutboxSyncRow(nil), events...)}
	if d == nil || d.db == nil {
		return LocalSyncBatch{}, fmt.Errorf("store: local sync unavailable")
	}
	runs := make(map[string]LocalAnalysisRunSyncRow)
	reviews := make(map[string]LocalReviewSyncRow)
	deletes := make(map[string]LocalDeleteSyncRow)
	for eventIndex, event := range events {
		if event.Sequence <= 0 || event.Topic == "" || event.EntityKey == "" || event.Generation < 0 ||
			!json.Valid([]byte(event.PayloadJSON)) {
			return LocalSyncBatch{}, fmt.Errorf("store: invalid local sync event")
		}
		switch event.Topic {
		case "local_analysis.stage", "local.analysis.published":
			var payload struct {
				MachineID string `json:"machine_id"`
				RunID     string `json:"run_id"`
			}
			if json.Unmarshal([]byte(event.PayloadJSON), &payload) != nil || payload.RunID == "" {
				return LocalSyncBatch{}, fmt.Errorf("store: invalid local analysis event")
			}
			run, err := d.loadLocalSyncRun(ctx, payload.RunID, event.Generation)
			if err != nil {
				return LocalSyncBatch{}, err
			}
			runs[runKey(run.MachineID, run.RunID, run.Generation)] = run
			if payload.MachineID != "" && payload.MachineID != run.MachineID {
				return LocalSyncBatch{}, fmt.Errorf("store: local analysis machine mismatch")
			}
			event.MachineID = run.MachineID
		case "local.review":
			var payload struct {
				MachineID  string `json:"machine_id"`
				RunID      string `json:"run_id"`
				Generation int64  `json:"generation"`
				GroupID    string `json:"group_id"`
			}
			if json.Unmarshal([]byte(event.PayloadJSON), &payload) != nil || payload.MachineID == "" ||
				payload.RunID == "" || payload.GroupID == "" || payload.Generation != event.Generation {
				return LocalSyncBatch{}, fmt.Errorf("store: invalid local review event")
			}
			run, err := d.loadLocalSyncRun(ctx, payload.RunID, payload.Generation)
			if err != nil || run.MachineID != payload.MachineID {
				return LocalSyncBatch{}, fmt.Errorf("store: local review run mismatch")
			}
			runs[runKey(run.MachineID, run.RunID, run.Generation)] = run
			event.MachineID = payload.MachineID
			loaded, err := d.loadLocalSyncReviews(ctx, payload.MachineID, payload.RunID, payload.Generation, payload.GroupID)
			if err != nil {
				return LocalSyncBatch{}, err
			}
			for _, review := range loaded {
				reviews[fmt.Sprintf("%s\x00%s\x00%d", review.RunID, review.GroupID, review.FileID)] = review
			}
		case "local.delete":
			var payload struct {
				FileID    int64  `json:"file_id"`
				MachineID string `json:"machine_id"`
				RunID     string `json:"run_id"`
				Status    string `json:"status"`
				SHA512    string `json:"sha512"`
				BatchID   string `json:"batch_id"`
			}
			if json.Unmarshal([]byte(event.PayloadJSON), &payload) != nil || payload.FileID <= 0 ||
				payload.MachineID == "" || payload.Status != "deleted" || payload.SHA512 == "" || payload.BatchID == "" {
				return LocalSyncBatch{}, fmt.Errorf("store: invalid local delete event")
			}
			row, attachedToRun, err := d.loadLocalSyncDelete(ctx, payload.MachineID, payload.BatchID, payload.FileID, event.Generation)
			if err != nil || row.SHA512 != payload.SHA512 || row.Status != "deleted" {
				return LocalSyncBatch{}, fmt.Errorf("store: local delete identity mismatch")
			}
			if attachedToRun {
				if payload.RunID != "" && payload.RunID != row.RunID {
					return LocalSyncBatch{}, fmt.Errorf("store: local delete run mismatch")
				}
			} else {
				if payload.RunID == "" {
					return LocalSyncBatch{}, fmt.Errorf("store: detached local delete missing run identity")
				}
				row.RunID = payload.RunID
			}
			deletes[fmt.Sprintf("%s\x00%d", row.BatchID, row.FileID)] = row
			event.MachineID = row.MachineID
			if attachedToRun {
				run, err := d.loadLocalSyncRun(ctx, row.RunID, row.Generation)
				if err != nil || run.MachineID != row.MachineID {
					return LocalSyncBatch{}, fmt.Errorf("store: local delete run mismatch")
				}
				runs[runKey(run.MachineID, run.RunID, run.Generation)] = run
			}
		case "local.task":
			var payload struct {
				MachineID string `json:"machine_id"`
			}
			if json.Unmarshal([]byte(event.PayloadJSON), &payload) != nil || payload.MachineID == "" {
				return LocalSyncBatch{}, fmt.Errorf("store: invalid local task event")
			}
			event.MachineID = payload.MachineID
		default:
			return LocalSyncBatch{}, fmt.Errorf("store: unsupported local sync topic %q", event.Topic)
		}
		batch.Events[eventIndex] = event
	}

	for _, run := range runs {
		batch.Runs = append(batch.Runs, run)
		pairs, groups, members, err := d.loadLocalSyncAnalysis(ctx, run)
		if err != nil {
			return LocalSyncBatch{}, err
		}
		batch.Pairs = append(batch.Pairs, pairs...)
		batch.Groups = append(batch.Groups, groups...)
		batch.Members = append(batch.Members, members...)
	}
	for _, review := range reviews {
		batch.Reviews = append(batch.Reviews, review)
	}
	for _, deleted := range deletes {
		batch.Deletes = append(batch.Deletes, deleted)
	}
	sortLocalSyncBatch(&batch)
	return batch, nil
}

func (d *DB) loadLocalSyncRun(ctx context.Context, runID string, generation int64) (LocalAnalysisRunSyncRow, error) {
	var row LocalAnalysisRunSyncRow
	var completed, published sql.NullInt64
	err := d.db.QueryRowContext(ctx, `
		SELECT machine_id,run_id,generation,task_id,status,created_at,completed_at,published_at
		FROM local_analysis_runs WHERE run_id=?1 AND generation=?2`, runID, generation).Scan(
		&row.MachineID, &row.RunID, &row.Generation, &row.TaskID, &row.Status, &row.CreatedAt,
		&completed, &published,
	)
	if err != nil {
		return row, fmt.Errorf("store: load local sync run: %w", err)
	}
	if completed.Valid {
		row.CompletedAt = &completed.Int64
	}
	if published.Valid {
		row.PublishedAt = &published.Int64
	}
	return row, nil
}

func (d *DB) loadLocalSyncAnalysis(ctx context.Context, run LocalAnalysisRunSyncRow) (
	[]LocalPairScoreSyncRow, []LocalGroupSyncRow, []LocalMemberSyncRow, error,
) {
	pairRows, err := d.db.QueryContext(ctx, `
		SELECT machine_id,run_id,generation,pair_key,left_file_id,right_file_id,left_sha512,right_sha512,
		       stage1_json,stage2_json,stage3_json,final_verdict
		FROM local_pair_scores WHERE machine_id=?1 AND run_id=?2 AND generation=?3 ORDER BY pair_key`,
		run.MachineID, run.RunID, run.Generation)
	if err != nil {
		return nil, nil, nil, err
	}
	var pairs []LocalPairScoreSyncRow
	for pairRows.Next() {
		var pair LocalPairScoreSyncRow
		var stage2, stage3 sql.NullString
		if err := pairRows.Scan(&pair.MachineID, &pair.RunID, &pair.Generation, &pair.PairKey,
			&pair.LeftFileID, &pair.RightFileID, &pair.LeftSHA512, &pair.RightSHA512,
			&pair.Stage1JSON, &stage2, &stage3, &pair.Verdict); err != nil {
			pairRows.Close()
			return nil, nil, nil, err
		}
		if stage2.Valid {
			pair.Stage2JSON = &stage2.String
		}
		if stage3.Valid {
			pair.Stage3JSON = &stage3.String
		}
		pairs = append(pairs, pair)
	}
	if err := pairRows.Close(); err != nil {
		return nil, nil, nil, err
	}
	groupRows, err := d.db.QueryContext(ctx, `
		SELECT machine_id,run_id,generation,group_id,category,verdict
		FROM local_dup_groups WHERE machine_id=?1 AND run_id=?2 AND generation=?3 ORDER BY group_id`,
		run.MachineID, run.RunID, run.Generation)
	if err != nil {
		return nil, nil, nil, err
	}
	var groups []LocalGroupSyncRow
	for groupRows.Next() {
		var group LocalGroupSyncRow
		if err := groupRows.Scan(&group.MachineID, &group.RunID, &group.Generation, &group.GroupID, &group.Category, &group.Verdict); err != nil {
			groupRows.Close()
			return nil, nil, nil, err
		}
		groups = append(groups, group)
	}
	if err := groupRows.Close(); err != nil {
		return nil, nil, nil, err
	}
	memberRows, err := d.db.QueryContext(ctx, `
		SELECT machine_id,run_id,generation,group_id,file_id,sha512
		FROM local_dup_members WHERE machine_id=?1 AND run_id=?2 AND generation=?3 ORDER BY group_id,file_id`,
		run.MachineID, run.RunID, run.Generation)
	if err != nil {
		return nil, nil, nil, err
	}
	defer memberRows.Close()
	var members []LocalMemberSyncRow
	for memberRows.Next() {
		var member LocalMemberSyncRow
		if err := memberRows.Scan(&member.MachineID, &member.RunID, &member.Generation, &member.GroupID, &member.FileID, &member.SHA512); err != nil {
			return nil, nil, nil, err
		}
		members = append(members, member)
	}
	return pairs, groups, members, memberRows.Err()
}

func (d *DB) loadLocalSyncReviews(ctx context.Context, machineID, runID string, generation int64, groupID string) ([]LocalReviewSyncRow, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT machine_id,run_id,generation,group_id,file_id,decision,reviewer,note,reviewed_at
		FROM local_reviews WHERE machine_id=?1 AND run_id=?2 AND generation=?3 AND group_id=?4
		ORDER BY file_id`, machineID, runID, generation, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var reviews []LocalReviewSyncRow
	for rows.Next() {
		var review LocalReviewSyncRow
		if err := rows.Scan(&review.MachineID, &review.RunID, &review.Generation, &review.GroupID,
			&review.FileID, &review.Decision, &review.Reviewer, &review.Note, &review.ReviewedAt); err != nil {
			return nil, err
		}
		reviews = append(reviews, review)
	}
	return reviews, rows.Err()
}

func (d *DB) loadLocalSyncDelete(ctx context.Context, machineID, batchID string, fileID, generation int64) (LocalDeleteSyncRow, bool, error) {
	var row LocalDeleteSyncRow
	var runID sql.NullString
	var completed sql.NullInt64
	err := d.db.QueryRowContext(ctx, `
		SELECT b.machine_id,b.run_id,?4,b.batch_id,i.file_id,i.path_snapshot,i.sha512,
		       i.result,f.status,COALESCE(i.error_code,''),i.uncertain,i.completed_at
		FROM local_delete_batches b
		JOIN local_delete_items i ON i.machine_id=b.machine_id AND i.batch_id=b.batch_id
		JOIN files f ON f.machine_id=i.machine_id AND f.id=i.file_id
		WHERE b.machine_id=?1 AND b.batch_id=?2 AND i.file_id=?3`, machineID, batchID, fileID, generation).Scan(
		&row.MachineID, &runID, &row.Generation, &row.BatchID, &row.FileID, &row.Path,
		&row.SHA512, &row.Result, &row.Status, &row.ErrorCode, &row.Uncertain, &completed,
	)
	if err != nil {
		return row, false, err
	}
	if runID.Valid {
		row.RunID = runID.String
	}
	if completed.Valid {
		row.CompletedAt = completed.Int64
	}
	return row, runID.Valid, nil
}

func (d *DB) AcknowledgeLocalSyncEvents(ctx context.Context, events []LocalOutboxSyncRow) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	for _, event := range events {
		var topic, entity string
		var generation int64
		var ack sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT topic,entity_key,generation,ack_at FROM local_outbox WHERE sequence=?1`, event.Sequence).Scan(
			&topic, &entity, &generation, &ack,
		); err != nil {
			return fmt.Errorf("store: verify local sync ack: %w", err)
		}
		if topic != event.Topic || entity != event.EntityKey || generation != event.Generation {
			return fmt.Errorf("store: stale local sync acknowledgement")
		}
		if ack.Valid {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE local_outbox SET ack_at=?2,updated_at=?2 WHERE sequence=?1 AND ack_at IS NULL`, event.Sequence, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func runKey(machineID, runID string, generation int64) string {
	return fmt.Sprintf("%s\x00%s\x00%d", machineID, runID, generation)
}

func sortLocalSyncBatch(batch *LocalSyncBatch) {
	sort.Slice(batch.Runs, func(i, j int) bool {
		return runKey(batch.Runs[i].MachineID, batch.Runs[i].RunID, batch.Runs[i].Generation) < runKey(batch.Runs[j].MachineID, batch.Runs[j].RunID, batch.Runs[j].Generation)
	})
	sort.Slice(batch.Reviews, func(i, j int) bool { return batch.Reviews[i].FileID < batch.Reviews[j].FileID })
	sort.Slice(batch.Deletes, func(i, j int) bool { return batch.Deletes[i].FileID < batch.Deletes[j].FileID })
}
