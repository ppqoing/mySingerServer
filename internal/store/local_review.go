package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type LocalReview struct {
	ReviewID  string
	MachineID string
	RunID     string
	GroupID   string
	FileID    int64
	Decision  string
	Reviewer  string
	Note      string
}

type DeletionFile struct {
	FileID    int64
	MachineID string
	Path      string
	SHA512    string
	Size      int64
	MTime     int64
}

type CommittedDeletion struct {
	MachineID  string
	RunID      string
	GroupID    string
	Generation int64
	Category   string
	Verdict    string
	Files      []DeletionFile
}

// LoadCommittedDeletion loads only delete choices from a fully committed
// review belonging to the machine's current published analysis.
func (d *DB) LoadCommittedDeletion(
	ctx context.Context,
	machineID, runID, groupID string,
) (CommittedDeletion, error) {
	if d == nil || d.db == nil || machineID == "" || runID == "" || groupID == "" {
		return CommittedDeletion{}, fmt.Errorf("store: invalid deletion selection")
	}
	selection := CommittedDeletion{MachineID: machineID, RunID: runID, GroupID: groupID}
	err := d.db.QueryRowContext(ctx, `
		SELECT g.generation,g.category,g.verdict
		FROM local_dup_groups g
		JOIN local_current_analysis c
		  ON c.machine_id=g.machine_id AND c.run_id=g.run_id AND c.generation=g.generation
		WHERE g.machine_id=?1 AND g.run_id=?2 AND g.group_id=?3`,
		machineID, runID, groupID,
	).Scan(&selection.Generation, &selection.Category, &selection.Verdict)
	if err != nil {
		if err == sql.ErrNoRows {
			return CommittedDeletion{}, ErrLocalResultNotFound
		}
		return CommittedDeletion{}, err
	}
	if selection.Category != "exact" && selection.Verdict != "duplicate" {
		return CommittedDeletion{}, fmt.Errorf("store: deletion selection is not eligible")
	}
	var memberCount, reviewCount, keepCount int
	if err := d.db.QueryRowContext(ctx, `
		SELECT COUNT(*),COUNT(r.file_id),COALESCE(SUM(CASE WHEN r.decision='keep' THEN 1 ELSE 0 END),0)
		FROM local_dup_members m
		LEFT JOIN local_reviews r
		  ON r.machine_id=m.machine_id AND r.run_id=m.run_id AND r.generation=m.generation
		 AND r.group_id=m.group_id AND r.file_id=m.file_id
		WHERE m.machine_id=?1 AND m.run_id=?2 AND m.generation=?3 AND m.group_id=?4`,
		machineID, runID, selection.Generation, groupID,
	).Scan(&memberCount, &reviewCount, &keepCount); err != nil {
		return CommittedDeletion{}, err
	}
	if memberCount == 0 || reviewCount != memberCount || keepCount == 0 {
		return CommittedDeletion{}, fmt.Errorf("store: deletion review is incomplete")
	}
	rows, err := d.db.QueryContext(ctx, `
		SELECT f.id,f.machine_id,f.path,f.sha512,f.size,f.mtime
		FROM local_reviews r
		JOIN files f ON f.machine_id=r.machine_id AND f.id=r.file_id
		WHERE r.machine_id=?1 AND r.run_id=?2 AND r.generation=?3 AND r.group_id=?4
		  AND r.decision='delete' AND f.status<>'deleted' AND f.sha512 IS NOT NULL
		ORDER BY f.id`, machineID, runID, selection.Generation, groupID)
	if err != nil {
		return CommittedDeletion{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var file DeletionFile
		if err := rows.Scan(&file.FileID, &file.MachineID, &file.Path, &file.SHA512, &file.Size, &file.MTime); err != nil {
			return CommittedDeletion{}, err
		}
		selection.Files = append(selection.Files, file)
	}
	if err := rows.Err(); err != nil {
		return CommittedDeletion{}, err
	}
	if len(selection.Files) == 0 {
		return CommittedDeletion{}, fmt.Errorf("store: deletion review has no active delete members")
	}
	return selection, nil
}

func (d *DB) SaveLocalReview(ctx context.Context, review LocalReview) error {
	if review.ReviewID == "" || review.MachineID == "" || review.RunID == "" ||
		review.GroupID == "" || review.FileID <= 0 || review.Reviewer == "" {
		return fmt.Errorf("store: invalid local review identity")
	}
	if review.Decision != "keep" && review.Decision != "delete" && review.Decision != "undecided" {
		return fmt.Errorf("store: invalid local review decision %q", review.Decision)
	}
	now := time.Now().UnixMilli()
	result, err := d.db.ExecContext(ctx, `
		INSERT INTO local_reviews
			(review_id,machine_id,run_id,generation,group_id,file_id,decision,reviewer,note,reviewed_at)
		SELECT ?1,r.machine_id,r.run_id,r.generation,g.group_id,m.file_id,?6,?7,?8,?9
		FROM local_analysis_runs r
		JOIN local_dup_groups g ON g.run_id=r.run_id AND g.group_id=?4
		JOIN local_dup_members m ON m.group_id=g.group_id AND m.run_id=r.run_id AND m.file_id=?5
		WHERE r.run_id=?3 AND r.machine_id=?2
		ON CONFLICT(review_id) DO UPDATE SET
			decision=excluded.decision,reviewer=excluded.reviewer,
			note=excluded.note,reviewed_at=excluded.reviewed_at
		WHERE local_reviews.machine_id=excluded.machine_id
		  AND local_reviews.run_id=excluded.run_id
		  AND local_reviews.group_id=excluded.group_id
		  AND local_reviews.file_id=excluded.file_id`,
		review.ReviewID, review.MachineID, review.RunID, review.GroupID,
		review.FileID, review.Decision, review.Reviewer, review.Note, now,
	)
	if err != nil {
		return fmt.Errorf("store: save local review: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("store: save local review: run, group, file, or machine mismatch")
	}
	return nil
}
