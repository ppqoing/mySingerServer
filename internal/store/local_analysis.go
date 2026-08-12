package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

const MaxLocalPageSize = 200

var ErrStaleLocalAnalysisGeneration = errors.New("stale_generation")

type LocalAnalysisRun struct {
	RunID       string
	MachineID   string
	Generation  int64
	TaskID      string
	Status      string
	CreatedAt   int64
	CompletedAt *int64
	PublishedAt *int64
}

type LocalPairScore struct {
	PairID      int64
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

type LocalAnalysisMember struct {
	FileID int64
	SHA512 string
}

type LocalAnalysisGroup struct {
	GroupID              string
	Category             string
	RepresentativeFileID int64
	Members              []LocalAnalysisMember
}

// ReplaceLocalAnalysisGroups atomically replaces only the final groups of a
// building run. Existing published generations are never touched.
func (d *DB) ReplaceLocalAnalysisGroups(ctx context.Context, runID string, groups []LocalAnalysisGroup) error {
	if runID == "" {
		return fmt.Errorf("store: replace local analysis groups: empty run ID")
	}
	normalized := make([]LocalAnalysisGroup, len(groups))
	seenGroups := make(map[string]struct{}, len(groups))
	for index, group := range groups {
		if group.GroupID == "" || (group.Category != "exact" && group.Category != "image" && group.Category != "video") || len(group.Members) < 2 {
			return fmt.Errorf("store: replace local analysis groups: invalid group")
		}
		if _, exists := seenGroups[group.GroupID]; exists {
			return fmt.Errorf("store: replace local analysis groups: duplicate group ID")
		}
		seenGroups[group.GroupID] = struct{}{}
		normalized[index] = group
		normalized[index].Members = append([]LocalAnalysisMember(nil), group.Members...)
		sort.Slice(normalized[index].Members, func(i, j int) bool {
			if normalized[index].Members[i].SHA512 != normalized[index].Members[j].SHA512 {
				return normalized[index].Members[i].SHA512 < normalized[index].Members[j].SHA512
			}
			return normalized[index].Members[i].FileID < normalized[index].Members[j].FileID
		})
		seenMembers := make(map[int64]struct{}, len(group.Members))
		representativeFound := false
		for _, member := range normalized[index].Members {
			if member.FileID <= 0 || member.SHA512 == "" {
				return fmt.Errorf("store: replace local analysis groups: invalid member")
			}
			if _, exists := seenMembers[member.FileID]; exists {
				return fmt.Errorf("store: replace local analysis groups: duplicate member")
			}
			seenMembers[member.FileID] = struct{}{}
			if member.FileID == group.RepresentativeFileID {
				representativeFound = true
			}
		}
		if !representativeFound {
			return fmt.Errorf("store: replace local analysis groups: representative is not a member")
		}
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i].GroupID < normalized[j].GroupID })

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var machineID, status string
	var generation int64
	if err := tx.QueryRowContext(ctx, `SELECT machine_id,generation,status FROM local_analysis_runs WHERE run_id=?1`, runID).Scan(&machineID, &generation, &status); err != nil {
		return fmt.Errorf("store: replace local analysis groups: load run: %w", err)
	}
	if status != "building" {
		return fmt.Errorf("store: replace local analysis groups: run is not building")
	}
	for _, group := range normalized {
		for _, member := range group.Members {
			var storedSHA string
			if err := tx.QueryRowContext(ctx, `SELECT sha512 FROM files WHERE machine_id=?1 AND id=?2 AND status!='deleted'`, machineID, member.FileID).Scan(&storedSHA); err != nil || storedSHA != member.SHA512 {
				if err == nil {
					err = fmt.Errorf("SHA mismatch")
				}
				return fmt.Errorf("store: replace local analysis groups: member identity: %w", err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM local_dup_members WHERE run_id=?1`, runID); err != nil {
		return fmt.Errorf("store: replace local analysis groups: delete members: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM local_dup_groups WHERE run_id=?1`, runID); err != nil {
		return fmt.Errorf("store: replace local analysis groups: delete groups: %w", err)
	}
	now := time.Now().UnixMilli()
	for _, group := range normalized {
		if _, err := tx.ExecContext(ctx, `INSERT INTO local_dup_groups(group_id,machine_id,run_id,generation,category,verdict,created_at) VALUES (?1,?2,?3,?4,?5,'duplicate',?6)`, group.GroupID, machineID, runID, generation, group.Category, now); err != nil {
			return fmt.Errorf("store: replace local analysis groups: insert group: %w", err)
		}
		for _, member := range group.Members {
			if _, err := tx.ExecContext(ctx, `INSERT INTO local_dup_members(group_id,machine_id,run_id,generation,file_id,sha512,created_at) VALUES (?1,?2,?3,?4,?5,?6,?7)`, group.GroupID, machineID, runID, generation, member.FileID, member.SHA512, now); err != nil {
				return fmt.Errorf("store: replace local analysis groups: insert member: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: replace local analysis groups: commit: %w", err)
	}
	return nil
}

func (d *DB) BeginLocalAnalysis(ctx context.Context, machineID, taskID string) (LocalAnalysisRun, error) {
	if machineID == "" || taskID == "" {
		return LocalAnalysisRun{}, fmt.Errorf("store: begin local analysis: empty machine or task ID")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return LocalAnalysisRun{}, err
	}
	defer tx.Rollback()
	if run, err := loadLocalAnalysisByTask(ctx, tx, taskID); err == nil {
		if run.MachineID != machineID {
			return LocalAnalysisRun{}, fmt.Errorf("store: local analysis task belongs to another machine")
		}
		return run, nil
	} else if err != sql.ErrNoRows {
		return LocalAnalysisRun{}, err
	}
	var taskMachine string
	if err := tx.QueryRowContext(ctx, `SELECT machine_id FROM local_tasks WHERE task_id=?1`, taskID).Scan(&taskMachine); err != nil {
		return LocalAnalysisRun{}, fmt.Errorf("store: load local analysis task: %w", err)
	}
	if taskMachine != machineID {
		return LocalAnalysisRun{}, fmt.Errorf("store: local analysis task belongs to another machine")
	}
	var generation int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(generation),0)+1 FROM local_analysis_runs WHERE machine_id=?1`,
		machineID).Scan(&generation); err != nil {
		return LocalAnalysisRun{}, fmt.Errorf("store: allocate local generation: %w", err)
	}
	runID, err := newLocalID()
	if err != nil {
		return LocalAnalysisRun{}, err
	}
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO local_analysis_runs(run_id,machine_id,generation,task_id,status,created_at)
		VALUES (?1,?2,?3,?4,'building',?5)`, runID, machineID, generation, taskID, now); err != nil {
		return LocalAnalysisRun{}, fmt.Errorf("store: insert local analysis: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE local_tasks SET status='running',started_at=COALESCE(started_at,?2),updated_at=?2
		WHERE task_id=?1`, taskID, now); err != nil {
		return LocalAnalysisRun{}, fmt.Errorf("store: start local analysis task: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return LocalAnalysisRun{}, err
	}
	return LocalAnalysisRun{
		RunID: runID, MachineID: machineID, Generation: generation,
		TaskID: taskID, Status: "building", CreatedAt: now,
	}, nil
}

type rowQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func loadLocalAnalysisByTask(ctx context.Context, q rowQueryer, taskID string) (LocalAnalysisRun, error) {
	var run LocalAnalysisRun
	var completed, published sql.NullInt64
	err := q.QueryRowContext(ctx, `
		SELECT run_id,machine_id,generation,task_id,status,created_at,completed_at,published_at
		FROM local_analysis_runs WHERE task_id=?1`, taskID).Scan(
		&run.RunID, &run.MachineID, &run.Generation, &run.TaskID,
		&run.Status, &run.CreatedAt, &completed, &published,
	)
	if err != nil {
		return LocalAnalysisRun{}, err
	}
	if completed.Valid {
		value := completed.Int64
		run.CompletedAt = &value
	}
	if published.Valid {
		value := published.Int64
		run.PublishedAt = &value
	}
	return run, nil
}

func newLocalID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("store: create local ID: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func (d *DB) CompleteLocalAnalysis(ctx context.Context, runID string) error {
	if runID == "" {
		return fmt.Errorf("store: complete local analysis: empty run ID")
	}
	now := time.Now().UnixMilli()
	result, err := d.db.ExecContext(ctx, `
		UPDATE local_analysis_runs SET status='complete',completed_at=COALESCE(completed_at,?2)
		WHERE run_id=?1 AND status='building'`, runID, now)
	if err != nil {
		return fmt.Errorf("store: complete local analysis: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 1 {
		return nil
	}
	var status string
	if err := d.db.QueryRowContext(ctx, `SELECT status FROM local_analysis_runs WHERE run_id=?1`, runID).Scan(&status); err != nil {
		return fmt.Errorf("store: complete local analysis: %w", err)
	}
	if status == "complete" || status == "published" {
		return nil
	}
	return fmt.Errorf("store: cannot complete local analysis in status %q", status)
}

func (d *DB) PublishLocalAnalysis(ctx context.Context, runID string) error {
	if runID == "" {
		return fmt.Errorf("store: publish local analysis: empty run ID")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var machineID, status string
	var generation int64
	if err := tx.QueryRowContext(ctx, `
		SELECT machine_id,generation,status FROM local_analysis_runs WHERE run_id=?1`, runID,
	).Scan(&machineID, &generation, &status); err != nil {
		return fmt.Errorf("store: load local analysis for publish: %w", err)
	}
	if status == "published" {
		var current string
		if err := tx.QueryRowContext(ctx, `SELECT run_id FROM local_current_analysis WHERE machine_id=?1`, machineID).Scan(&current); err != nil {
			return fmt.Errorf("store: verify published local analysis: %w", err)
		}
		if current != runID {
			return fmt.Errorf("store: published local analysis is not current")
		}
		return tx.Commit()
	}
	if status != "complete" {
		return fmt.Errorf("store: local analysis is not complete")
	}
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `
		UPDATE local_analysis_runs SET status='published',published_at=?2
		WHERE run_id=?1 AND status='complete'`, runID, now); err != nil {
		return fmt.Errorf("store: mark local analysis published: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO local_current_analysis(machine_id,run_id,generation,published_at)
		VALUES (?1,?2,?3,?4)
		ON CONFLICT(machine_id) DO UPDATE SET
			run_id=excluded.run_id,generation=excluded.generation,published_at=excluded.published_at
		WHERE excluded.generation > local_current_analysis.generation`,
		machineID, runID, generation, now)
	if err != nil {
		return fmt.Errorf("store: switch local current analysis: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: inspect local current switch: %w", err)
	}
	if changed != 1 {
		return fmt.Errorf("%w: machine %s generation %d", ErrStaleLocalAnalysisGeneration, machineID, generation)
	}
	return tx.Commit()
}

func (d *DB) CurrentLocalAnalysis(ctx context.Context, machineID string) (LocalAnalysisRun, error) {
	var run LocalAnalysisRun
	var completed, published sql.NullInt64
	err := d.db.QueryRowContext(ctx, `
		SELECT r.run_id,r.machine_id,r.generation,r.task_id,r.status,r.created_at,
		       r.completed_at,r.published_at
		FROM local_current_analysis c
		JOIN local_analysis_runs r ON r.run_id=c.run_id AND r.machine_id=c.machine_id
		WHERE c.machine_id=?1`, machineID).Scan(
		&run.RunID, &run.MachineID, &run.Generation, &run.TaskID, &run.Status,
		&run.CreatedAt, &completed, &published,
	)
	if err != nil {
		return LocalAnalysisRun{}, err
	}
	if completed.Valid {
		value := completed.Int64
		run.CompletedAt = &value
	}
	if published.Valid {
		value := published.Int64
		run.PublishedAt = &value
	}
	return run, nil
}

func (d *DB) SaveLocalPairScore(ctx context.Context, pair LocalPairScore) error {
	if pair.RunID == "" || pair.PairKey == "" || pair.LeftFileID == pair.RightFileID ||
		pair.LeftSHA512 == "" || pair.RightSHA512 == "" || !json.Valid([]byte(pair.Stage1JSON)) {
		return fmt.Errorf("store: invalid local pair score")
	}
	if pair.Stage2JSON != nil && !json.Valid([]byte(*pair.Stage2JSON)) {
		return fmt.Errorf("store: invalid local pair stage 2 JSON")
	}
	if pair.Stage3JSON != nil && !json.Valid([]byte(*pair.Stage3JSON)) {
		return fmt.Errorf("store: invalid local pair stage 3 JSON")
	}
	switch pair.Verdict {
	case "undecided", "duplicate", "not_duplicate", "uncertain":
	default:
		return fmt.Errorf("store: invalid local pair verdict %q", pair.Verdict)
	}
	now := time.Now().UnixMilli()
	result, err := d.db.ExecContext(ctx, `
		INSERT INTO local_pair_scores
			(machine_id,run_id,generation,pair_key,left_file_id,right_file_id,left_sha512,right_sha512,
			 stage1_json,stage2_json,stage3_json,final_verdict,created_at,updated_at)
		SELECT r.machine_id,r.run_id,r.generation,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11,?11
		FROM local_analysis_runs r
		JOIN files lf ON lf.id=?3 AND lf.machine_id=r.machine_id AND lf.sha512=?5
		JOIN files rf ON rf.id=?4 AND rf.machine_id=r.machine_id AND rf.sha512=?6
		WHERE r.run_id=?1 AND r.status='building'
		ON CONFLICT(run_id,pair_key) DO UPDATE SET
			stage1_json=excluded.stage1_json,stage2_json=excluded.stage2_json,
			stage3_json=excluded.stage3_json,final_verdict=excluded.final_verdict,
			updated_at=excluded.updated_at`,
		pair.RunID, pair.PairKey, pair.LeftFileID, pair.RightFileID,
		pair.LeftSHA512, pair.RightSHA512, pair.Stage1JSON,
		pair.Stage2JSON, pair.Stage3JSON, pair.Verdict, now,
	)
	if err != nil {
		return fmt.Errorf("store: save local pair score: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("store: save local pair score: run or file identity mismatch")
	}
	return nil
}

func (d *DB) ListCurrentLocalPairScores(ctx context.Context, machineID string, offset, limit int) ([]LocalPairScore, error) {
	if machineID == "" {
		return nil, fmt.Errorf("store: list local pair scores: empty machine ID")
	}
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > MaxLocalPageSize {
		limit = MaxLocalPageSize
	}
	rows, err := d.db.QueryContext(ctx, `
		SELECT p.pair_id,p.machine_id,p.run_id,p.generation,p.pair_key,
		       p.left_file_id,p.right_file_id,p.left_sha512,p.right_sha512,
		       p.stage1_json,p.stage2_json,p.stage3_json,p.final_verdict
		FROM local_current_analysis c
		JOIN local_pair_scores p ON p.run_id=c.run_id AND p.generation=c.generation
		WHERE c.machine_id=?1 AND p.machine_id=c.machine_id
		ORDER BY p.pair_key,p.pair_id
		LIMIT ?2 OFFSET ?3`, machineID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("store: list local pair scores: %w", err)
	}
	defer rows.Close()
	result := make([]LocalPairScore, 0, limit)
	for rows.Next() {
		var pair LocalPairScore
		var stage2, stage3 sql.NullString
		if err := rows.Scan(
			&pair.PairID, &pair.MachineID, &pair.RunID, &pair.Generation,
			&pair.PairKey, &pair.LeftFileID, &pair.RightFileID,
			&pair.LeftSHA512, &pair.RightSHA512, &pair.Stage1JSON,
			&stage2, &stage3, &pair.Verdict,
		); err != nil {
			return nil, err
		}
		if stage2.Valid {
			value := stage2.String
			pair.Stage2JSON = &value
		}
		if stage3.Valid {
			value := stage3.String
			pair.Stage3JSON = &value
		}
		result = append(result, pair)
	}
	return result, rows.Err()
}
