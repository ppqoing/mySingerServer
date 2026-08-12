package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

var ErrLocalResultNotFound = errors.New("local_result_not_found")

type LocalGroupQuery struct {
	MachineID        string
	Scope            string
	RunID            string
	Category         string
	PathContains     string
	FileNameContains string
	MinSize          *int64
	MaxSize          *int64
	ReviewStatus     string
	Offset           int
	Limit            int
}

type LocalGroupMember struct {
	FileID           int64
	Path             string
	FileName         string
	Size             int64
	Status           string
	SHA512           string
	Decision         string
	VideoPreviewPath string
}

type LocalResultGroup struct {
	RunID        string
	Generation   int64
	GroupID      string
	Category     string
	Verdict      string
	ReviewStatus string
	Members      []LocalGroupMember
}

type LocalGroupPage struct {
	Groups     []LocalResultGroup
	Offset     int
	NextOffset int
}

type LocalReviewChoice struct {
	FileID   int64  `json:"file_id"`
	Decision string `json:"decision"`
}

type LocalReviewCommit struct {
	MachineID string
	RunID     string
	GroupID   string
	Reviewer  string
	Note      string
	Decisions []LocalReviewChoice
}

type LocalPreviewSource struct {
	FileID    int64
	MachineID string
	Path      string
	Kind      string
	Status    string
	SHA512    string
	Size      int64
	MTime     int64
}

func (d *DB) ListLocalGroups(ctx context.Context, query LocalGroupQuery) (LocalGroupPage, error) {
	if d == nil || d.db == nil || query.MachineID == "" || query.Offset < 0 ||
		query.Limit < 0 || query.Limit > MaxLocalPageSize {
		return LocalGroupPage{}, fmt.Errorf("store: invalid local group query")
	}
	if query.Scope == "" {
		query.Scope = "current"
	}
	if query.Scope != "current" && query.Scope != "history" ||
		query.Scope == "history" && query.RunID == "" {
		return LocalGroupPage{}, fmt.Errorf("store: invalid local group scope")
	}
	if query.Limit == 0 {
		query.Limit = 50
	}
	category := query.Category
	if category == "inconclusive" {
		category = "uncertain"
	}

	var sqlText strings.Builder
	sqlText.WriteString(`
		SELECT DISTINCT g.run_id,g.generation,g.group_id,g.category,g.verdict
		FROM local_dup_groups g
		JOIN local_dup_members m
		  ON m.machine_id=g.machine_id AND m.run_id=g.run_id AND m.group_id=g.group_id
		JOIN files f ON f.machine_id=m.machine_id AND f.id=m.file_id
		LEFT JOIN local_reviews r
		  ON r.machine_id=m.machine_id AND r.run_id=m.run_id AND r.group_id=m.group_id AND r.file_id=m.file_id`)
	args := []any{query.MachineID}
	if query.Scope == "current" {
		sqlText.WriteString(` JOIN local_current_analysis c ON c.machine_id=g.machine_id AND c.run_id=g.run_id AND c.generation=g.generation`)
	}
	sqlText.WriteString(` WHERE g.machine_id=?`)
	if query.Scope == "current" {
		sqlText.WriteString(` AND f.status<>'deleted'`)
	} else {
		args = append(args, query.RunID)
		sqlText.WriteString(fmt.Sprintf(` AND g.run_id=?%d`, len(args)))
	}
	if category != "" {
		args = append(args, category)
		sqlText.WriteString(fmt.Sprintf(` AND g.category=?%d`, len(args)))
	}
	if query.PathContains != "" {
		args = append(args, strings.ToLower(query.PathContains))
		sqlText.WriteString(fmt.Sprintf(` AND instr(lower(f.path),?%d)>0`, len(args)))
	}
	if query.FileNameContains != "" {
		args = append(args, strings.ToLower(query.FileNameContains))
		sqlText.WriteString(fmt.Sprintf(` AND EXISTS (
			WITH RECURSIVE path_part(rest,name) AS (
				SELECT replace(f.path,char(92),'/'),''
				UNION ALL
				SELECT CASE WHEN instr(rest,'/')=0 THEN '' ELSE substr(rest,instr(rest,'/')+1) END,
				       CASE WHEN instr(rest,'/')=0 THEN rest ELSE substr(rest,1,instr(rest,'/')-1) END
				FROM path_part WHERE rest<>''
			)
			SELECT 1 FROM path_part WHERE rest='' AND instr(lower(name),?%d)>0
		)`, len(args)))
	}
	if query.MinSize != nil {
		args = append(args, *query.MinSize)
		sqlText.WriteString(fmt.Sprintf(` AND f.size>=?%d`, len(args)))
	}
	if query.MaxSize != nil {
		args = append(args, *query.MaxSize)
		sqlText.WriteString(fmt.Sprintf(` AND f.size<=?%d`, len(args)))
	}
	switch query.ReviewStatus {
	case "undecided":
		sqlText.WriteString(` AND coalesce(r.decision,'undecided')='undecided'`)
	case "reviewed":
		sqlText.WriteString(` AND NOT EXISTS (
			SELECT 1 FROM local_dup_members rm
			JOIN files rf ON rf.machine_id=rm.machine_id AND rf.id=rm.file_id
			LEFT JOIN local_reviews rr
			  ON rr.machine_id=rm.machine_id AND rr.run_id=rm.run_id
			 AND rr.group_id=rm.group_id AND rr.file_id=rm.file_id
			WHERE rm.machine_id=g.machine_id AND rm.run_id=g.run_id
			  AND rm.group_id=g.group_id AND coalesce(rr.decision,'undecided')='undecided'`)
		if query.Scope == "current" {
			sqlText.WriteString(` AND rf.status<>'deleted'`)
		}
		sqlText.WriteString(`)`)
	case "keep", "delete":
		args = append(args, query.ReviewStatus)
		sqlText.WriteString(fmt.Sprintf(` AND r.decision=?%d`, len(args)))
	case "":
	default:
		return LocalGroupPage{}, fmt.Errorf("store: invalid local review filter")
	}
	args = append(args, query.Limit+1, query.Offset)
	sqlText.WriteString(fmt.Sprintf(` ORDER BY g.generation DESC,g.group_id ASC LIMIT ?%d OFFSET ?%d`, len(args)-1, len(args)))

	rows, err := d.db.QueryContext(ctx, sqlText.String(), args...)
	if err != nil {
		return LocalGroupPage{}, fmt.Errorf("store: list local groups: %w", err)
	}
	defer rows.Close()
	groups := make([]LocalResultGroup, 0, query.Limit+1)
	for rows.Next() {
		var group LocalResultGroup
		if err := rows.Scan(&group.RunID, &group.Generation, &group.GroupID, &group.Category, &group.Verdict); err != nil {
			return LocalGroupPage{}, err
		}
		if group.Category == "uncertain" {
			group.Category = "inconclusive"
		}
		groups = append(groups, group)
	}
	if err := rows.Err(); err != nil {
		return LocalGroupPage{}, err
	}
	hasMore := len(groups) > query.Limit
	if hasMore {
		groups = groups[:query.Limit]
	}
	for index := range groups {
		members, status, err := d.loadLocalGroupMembers(ctx, query.MachineID, groups[index].RunID, groups[index].GroupID, query.Scope == "current")
		if err != nil {
			return LocalGroupPage{}, err
		}
		groups[index].Members = members
		groups[index].ReviewStatus = status
	}
	page := LocalGroupPage{Groups: groups, Offset: query.Offset}
	if hasMore {
		page.NextOffset = query.Offset + len(groups)
	}
	return page, nil
}

func (d *DB) loadLocalGroupMembers(ctx context.Context, machineID, runID, groupID string, activeOnly bool) ([]LocalGroupMember, string, error) {
	query := `
		SELECT f.id,f.path,f.size,f.status,f.sha512,
		       coalesce(r.decision,'undecided'),coalesce(v.thumb_path,'')
		FROM local_dup_members m
		JOIN files f ON f.machine_id=m.machine_id AND f.id=m.file_id
		LEFT JOIN local_reviews r ON r.machine_id=m.machine_id AND r.run_id=m.run_id AND r.group_id=m.group_id AND r.file_id=m.file_id
		LEFT JOIN video_features v ON v.sha512=f.sha512
		WHERE m.machine_id=?1 AND m.run_id=?2 AND m.group_id=?3`
	if activeOnly {
		query += ` AND f.status<>'deleted'`
	}
	query += ` ORDER BY f.id ASC`
	rows, err := d.db.QueryContext(ctx, query, machineID, runID, groupID)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	members := make([]LocalGroupMember, 0)
	allReviewed := true
	for rows.Next() {
		var member LocalGroupMember
		if err := rows.Scan(&member.FileID, &member.Path, &member.Size, &member.Status,
			&member.SHA512, &member.Decision, &member.VideoPreviewPath); err != nil {
			return nil, "", err
		}
		member.FileName = filepath.Base(member.Path)
		if member.Decision == "undecided" {
			allReviewed = false
		}
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	status := "undecided"
	if len(members) > 0 && allReviewed {
		status = "reviewed"
	}
	return members, status, nil
}

func (d *DB) LoadLocalGroup(ctx context.Context, machineID, runID, groupID string, current bool) (LocalResultGroup, error) {
	if d == nil || d.db == nil || machineID == "" || groupID == "" || (!current && runID == "") {
		return LocalResultGroup{}, fmt.Errorf("store: invalid local group identity")
	}
	query := `SELECT g.run_id,g.generation,g.group_id,g.category,g.verdict
		FROM local_dup_groups g`
	if current {
		query += ` JOIN local_current_analysis c
			ON c.machine_id=g.machine_id AND c.run_id=g.run_id AND c.generation=g.generation
			WHERE g.machine_id=?1 AND g.group_id=?2`
	} else {
		query += ` WHERE g.machine_id=?1 AND g.group_id=?2 AND g.run_id=?3`
	}
	query += ` ORDER BY g.generation DESC LIMIT 1`
	args := []any{machineID, groupID}
	if !current {
		args = append(args, runID)
	}
	var group LocalResultGroup
	if err := d.db.QueryRowContext(ctx, query, args...).Scan(
		&group.RunID, &group.Generation, &group.GroupID, &group.Category, &group.Verdict,
	); err != nil {
		if err == sql.ErrNoRows {
			return LocalResultGroup{}, ErrLocalResultNotFound
		}
		return LocalResultGroup{}, err
	}
	members, status, err := d.loadLocalGroupMembers(ctx, machineID, group.RunID, group.GroupID, current)
	if err != nil {
		return LocalResultGroup{}, err
	}
	if len(members) == 0 {
		return LocalResultGroup{}, ErrLocalResultNotFound
	}
	if group.Category == "uncertain" {
		group.Category = "inconclusive"
	}
	group.Members, group.ReviewStatus = members, status
	return group, nil
}

func (d *DB) CommitLocalReview(ctx context.Context, commit LocalReviewCommit) error {
	if d == nil || d.db == nil || commit.MachineID == "" || commit.RunID == "" ||
		commit.GroupID == "" || commit.Reviewer == "" || len(commit.Decisions) == 0 {
		return fmt.Errorf("store: invalid local review commit")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var generation int64
	var category, verdict string
	if err := tx.QueryRowContext(ctx, `SELECT generation,category,verdict FROM local_dup_groups WHERE machine_id=?1 AND run_id=?2 AND group_id=?3`, commit.MachineID, commit.RunID, commit.GroupID).Scan(&generation, &category, &verdict); err != nil {
		if err == sql.ErrNoRows {
			return ErrLocalResultNotFound
		}
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT file_id FROM local_dup_members WHERE machine_id=?1 AND run_id=?2 AND group_id=?3 ORDER BY file_id`, commit.MachineID, commit.RunID, commit.GroupID)
	if err != nil {
		return err
	}
	var members []int64
	for rows.Next() {
		var fileID int64
		if err := rows.Scan(&fileID); err != nil {
			rows.Close()
			return err
		}
		members = append(members, fileID)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	memberSet := make(map[int64]struct{}, len(members))
	for _, fileID := range members {
		memberSet[fileID] = struct{}{}
	}
	decisions := make(map[int64]string, len(commit.Decisions))
	keepCount := 0
	for _, choice := range commit.Decisions {
		if _, exists := memberSet[choice.FileID]; !exists {
			return fmt.Errorf("store: review file is not a group member")
		}
		if _, duplicate := decisions[choice.FileID]; duplicate ||
			(choice.Decision != "keep" && choice.Decision != "delete" && choice.Decision != "undecided") {
			return fmt.Errorf("store: invalid local review decision")
		}
		if choice.Decision == "keep" {
			keepCount++
		}
		if choice.Decision == "delete" && category != "exact" && verdict != "duplicate" {
			return fmt.Errorf("store: delete decision is not eligible")
		}
		decisions[choice.FileID] = choice.Decision
	}
	if keepCount == 0 {
		return fmt.Errorf("store: review requires an explicit keep")
	}
	now := time.Now().UnixMilli()
	choices := make([]LocalReviewChoice, 0, len(members))
	for _, fileID := range members {
		decision := decisions[fileID]
		if decision == "" {
			decision = "undecided"
		}
		choices = append(choices, LocalReviewChoice{FileID: fileID, Decision: decision})
		reviewID := fmt.Sprintf("local-review:%s:%s:%d", commit.RunID, commit.GroupID, fileID)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO local_reviews(review_id,machine_id,run_id,generation,group_id,file_id,decision,reviewer,note,reviewed_at)
			VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10)
			ON CONFLICT(run_id,group_id,file_id) DO UPDATE SET
			 decision=excluded.decision,reviewer=excluded.reviewer,note=excluded.note,reviewed_at=excluded.reviewed_at`,
			reviewID, commit.MachineID, commit.RunID, generation, commit.GroupID,
			fileID, decision, commit.Reviewer, commit.Note, now); err != nil {
			return fmt.Errorf("store: commit local review: %w", err)
		}
	}
	payload, err := json.Marshal(struct {
		MachineID  string              `json:"machine_id"`
		RunID      string              `json:"run_id"`
		Generation int64               `json:"generation"`
		GroupID    string              `json:"group_id"`
		Decisions  []LocalReviewChoice `json:"decisions"`
	}{commit.MachineID, commit.RunID, generation, commit.GroupID, choices})
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO local_outbox(topic,entity_key,generation,payload_json,created_at,updated_at)
		VALUES ('local.review',?1,?2,?3,?4,?4)
		ON CONFLICT(topic,entity_key,generation) DO UPDATE SET
		 payload_json=excluded.payload_json,ack_at=NULL,retry_count=0,
		 next_retry_at=NULL,last_error=NULL,updated_at=excluded.updated_at`,
		commit.GroupID, generation, string(payload), now); err != nil {
		return fmt.Errorf("store: commit local review outbox: %w", err)
	}
	return tx.Commit()
}

func (d *DB) LoadLocalPreviewSource(ctx context.Context, machineID string, fileID int64) (LocalPreviewSource, error) {
	if d == nil || d.db == nil || machineID == "" || fileID <= 0 {
		return LocalPreviewSource{}, fmt.Errorf("store: invalid preview identity")
	}
	var source LocalPreviewSource
	err := d.db.QueryRowContext(ctx, `
		SELECT f.id,f.machine_id,f.path,'image',f.status,f.sha512,f.size,f.mtime
		FROM files f
		JOIN image_features i ON i.sha512=f.sha512
		WHERE f.machine_id=?1 AND f.id=?2 AND f.status<>'deleted'
		  AND f.sha512 IS NOT NULL`, machineID, fileID).Scan(
		&source.FileID, &source.MachineID, &source.Path, &source.Kind,
		&source.Status, &source.SHA512, &source.Size, &source.MTime,
	)
	if err == sql.ErrNoRows {
		return LocalPreviewSource{}, ErrLocalResultNotFound
	}
	if err != nil {
		return LocalPreviewSource{}, err
	}
	return source, nil
}
