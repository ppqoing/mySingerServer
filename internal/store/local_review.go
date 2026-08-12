package store

import (
	"context"
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
