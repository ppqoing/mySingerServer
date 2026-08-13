package store

import (
	"context"
	"encoding/json"
	"testing"
)

func TestLocalReviewSavesExplicitDecisionAndEnforcesForeignKeys(t *testing.T) {
	db := openLocalTestDB(t)
	ctx := context.Background()
	createAnalysisTask(t, db, "task", "machine-a")
	run, err := db.BeginLocalAnalysis(ctx, "machine-a", "task")
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.db.Exec(`INSERT INTO files(machine_id,path,sha512,status) VALUES ('machine-a','D:\\a.jpg','sha-a','done')`)
	if err != nil {
		t.Fatal(err)
	}
	fileID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`
		INSERT INTO local_dup_groups(group_id,machine_id,run_id,generation,category,verdict,created_at)
		VALUES ('group-1','machine-a',?,?, 'exact','duplicate',1)`, run.RunID, run.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`
		INSERT INTO local_dup_members(group_id,machine_id,run_id,generation,file_id,sha512,created_at)
		VALUES ('group-1','machine-a',?,?,?,'sha-a',1)`, run.RunID, run.Generation, fileID); err != nil {
		t.Fatal(err)
	}

	review := LocalReview{
		ReviewID: "review-1", MachineID: "machine-a", RunID: run.RunID,
		GroupID: "group-1", FileID: fileID, Decision: "keep",
		Reviewer: "local-user", Note: "best copy",
	}
	if err := db.SaveLocalReview(ctx, review); err != nil {
		t.Fatalf("SaveLocalReview: %v", err)
	}
	var decision, note string
	if err := db.db.QueryRow(`SELECT decision,note FROM local_reviews WHERE review_id=?`, review.ReviewID).Scan(&decision, &note); err != nil {
		t.Fatal(err)
	}
	if decision != "keep" || note != "best copy" {
		t.Fatalf("saved review = %q/%q", decision, note)
	}

	review.ReviewID = "review-invalid"
	review.Decision = "erase"
	if err := db.SaveLocalReview(ctx, review); err == nil {
		t.Fatal("invalid review decision was accepted")
	}
	review.ReviewID = "review-wrong-machine"
	review.Decision = "delete"
	review.MachineID = "machine-b"
	if err := db.SaveLocalReview(ctx, review); err == nil {
		t.Fatal("review for another machine was accepted")
	}
}

func TestLocalDeleteAuditSchemaModelsFailureAndUncertainWithoutChangingFile(t *testing.T) {
	db := openLocalTestDB(t)
	result, err := db.db.Exec(`INSERT INTO files(machine_id,path,sha512,status) VALUES ('machine-a','D:\\keep.jpg','sha-keep','done')`)
	if err != nil {
		t.Fatal(err)
	}
	fileID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`
		INSERT INTO local_delete_batches
		(batch_id,machine_id,confirmation_digest,status,created_at,updated_at)
		VALUES ('batch-1','machine-a','confirm','failed',1,2);
		INSERT INTO local_delete_items
		(batch_id,machine_id,file_id,path_snapshot,sha512,result,error_code,error_message,uncertain,created_at,updated_at)
		VALUES ('batch-1','machine-a',?1,'D:\\keep.jpg','sha-keep','failed','access_denied','denied',0,1,2);
		INSERT INTO local_delete_items
		(batch_id,machine_id,file_id,path_snapshot,sha512,result,error_code,error_message,uncertain,created_at,updated_at)
		VALUES ('batch-1','machine-a',?1,'D:\\keep.jpg','sha-keep','uncertain','unknown','unknown result',1,1,2);`, fileID); err == nil {
		t.Fatal("duplicate batch/file audit item was accepted")
	}
	var status string
	if err := db.db.QueryRow(`SELECT status FROM files WHERE id=?`, fileID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "done" {
		t.Fatalf("file status = %q, want unchanged done", status)
	}
	if _, err := db.db.Exec(`
		INSERT INTO local_delete_items
		(batch_id,machine_id,file_id,path_snapshot,sha512,result,uncertain,created_at,updated_at)
		VALUES ('missing','machine-a',?,'D:\\keep.jpg','sha-keep','failed',0,1,1)`, fileID); err == nil {
		t.Fatal("delete item without batch was accepted")
	}
}

// Break caught: deletion is prepared from unchecked file paths rather than
// the committed current review, or inactive/foreign members leak into it.
func TestDeletePrepareLoadsOnlyCommittedEligibleActiveReviewMembers(t *testing.T) {
	db := openLocalTestDB(t)
	run := seedLocalResultGroups(t, db)
	ctx := context.Background()
	if err := db.CommitLocalReview(ctx, LocalReviewCommit{
		MachineID: "machine-a", RunID: run.RunID, GroupID: "group-video", Reviewer: "user",
		Decisions: []LocalReviewChoice{{FileID: 5, Decision: "keep"}, {FileID: 6, Decision: "delete"}},
	}); err != nil {
		t.Fatal(err)
	}

	selection, err := db.LoadCommittedDeletion(ctx, "machine-a", run.RunID, "group-video")
	if err != nil {
		t.Fatal(err)
	}
	if selection.Generation != run.Generation || selection.Category != "video" ||
		len(selection.Files) != 1 || selection.Files[0].FileID != 6 ||
		selection.Files[0].Path != `D:\Video\copy.mp4` || selection.Files[0].SHA512 != "sha-6" {
		t.Fatalf("selection = %#v", selection)
	}
	if _, err := db.LoadCommittedDeletion(ctx, "machine-b", run.RunID, "group-video"); err == nil {
		t.Fatal("foreign machine selection was accepted")
	}
	if _, err := db.LoadCommittedDeletion(ctx, "machine-a", run.RunID, "group-image"); err == nil {
		t.Fatal("uncommitted or ineligible selection was accepted")
	}
}

// Break caught: a partial Helper report marks uncertain/failed rows deleted,
// clears retained analysis data, or emits an incomplete PostgreSQL event.
func TestCommitDeletionResultsIsAtomicRetainsHashesAndAuditsPartialDelete(t *testing.T) {
	db := openLocalTestDB(t)
	run := seedLocalResultGroups(t, db)
	ctx := context.Background()
	if _, err := db.db.Exec(`
		INSERT INTO files(id,machine_id,path,size,mtime,sha512,status)
		VALUES (9,'machine-a','D:\extra\copy.mp4',600,1000,'sha-9','done');
		INSERT INTO local_dup_members(group_id,machine_id,run_id,generation,file_id,sha512,created_at)
		VALUES ('group-video','machine-a',?1,?2,9,'sha-9',1)`, run.RunID, run.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`INSERT INTO video_frames(sha512,frame_idx) VALUES ('sha-6',0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`
		INSERT INTO local_pair_scores(
		 machine_id,run_id,generation,pair_key,left_file_id,right_file_id,left_sha512,right_sha512,
		 stage1_json,final_verdict,created_at,updated_at)
		VALUES ('machine-a',?1,?2,'video:5:6',5,6,'sha-5','sha-6','{}','duplicate',1,1)`,
		run.RunID, run.Generation); err != nil {
		t.Fatal(err)
	}
	if err := db.CommitLocalReview(ctx, LocalReviewCommit{
		MachineID: "machine-a", RunID: run.RunID, GroupID: "group-video", Reviewer: "user",
		Decisions: []LocalReviewChoice{{FileID: 5, Decision: "keep"}, {FileID: 6, Decision: "delete"}, {FileID: 9, Decision: "delete"}},
	}); err != nil {
		t.Fatal(err)
	}

	selection, err := db.LoadCommittedDeletion(ctx, "machine-a", run.RunID, "group-video")
	if err != nil {
		t.Fatal(err)
	}
	file := selection.Files[0]
	results := []DeletionResult{{
		FileID: file.FileID, MachineID: "machine-a", Path: file.Path, SHA512: file.SHA512,
		Size: file.Size, MTime: file.MTime, BatchID: "batch-partial", RunID: run.RunID,
		GroupID: "group-video", Generation: run.Generation, ConfirmationDigest: "digest",
		OK: true,
	}, {
		FileID: 9, MachineID: "machine-a", Path: `D:\extra\copy.mp4`, SHA512: "sha-9",
		Size: 600, MTime: 1000, BatchID: "batch-partial", RunID: run.RunID,
		GroupID: "group-video", Generation: run.Generation, ConfirmationDigest: "digest",
		ErrorCode: "access_denied",
	}}
	if err := db.CommitDeletionResults(ctx, "batch-partial", results); err != nil {
		t.Fatal(err)
	}

	for _, want := range []struct {
		id     int64
		status string
		sha    string
	}{{6, "deleted", "sha-6"}, {9, "done", "sha-9"}} {
		var status, sha string
		if err := db.db.QueryRow(`SELECT status,sha512 FROM files WHERE machine_id='machine-a' AND id=?`, want.id).Scan(&status, &sha); err != nil {
			t.Fatal(err)
		}
		if status != want.status || sha != want.sha {
			t.Fatalf("file %d=%s/%s", want.id, status, sha)
		}
	}
	for table, want := range map[string]int{
		"video_features": 2, "video_frames": 1, "local_pair_scores": 1,
		"local_dup_members": 9, "local_reviews": 4,
	} {
		if got := countRows(t, db, "SELECT count(*) FROM "+table); got != want {
			t.Fatalf("%s rows=%d want=%d", table, got, want)
		}
	}
	if countRows(t, db, `SELECT count(*) FROM files INDEXED BY idx_files_sha512 WHERE sha512='sha-6' AND status='deleted'`) != 1 {
		t.Fatal("SHA index no longer finds deleted row")
	}
	current, err := db.LoadLocalGroup(ctx, "machine-a", "", "group-video", true)
	if err != nil || len(current.Members) != 2 || current.Members[0].FileID != 5 || current.Members[1].FileID != 9 {
		t.Fatalf("current group=%#v err=%v", current, err)
	}
	history, err := db.LoadLocalGroup(ctx, "machine-a", run.RunID, "group-video", false)
	if err != nil || len(history.Members) != 3 || history.Members[1].Status != "deleted" {
		t.Fatalf("history group=%#v err=%v", history, err)
	}

	batch, err := db.LoadDeletionBatch(ctx, "machine-a", "batch-partial")
	if err != nil || batch.Status != "failed" || batch.Succeeded != 1 || batch.Failed != 1 || batch.Uncertain != 0 {
		t.Fatalf("batch=%#v err=%v", batch, err)
	}
	var payload string
	if err := db.db.QueryRow(`SELECT payload_json FROM local_outbox WHERE topic='local.delete'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		t.Fatal(err)
	}
	if event["file_id"] != float64(6) || event["machine_id"] != "machine-a" ||
		event["status"] != "deleted" || event["sha512"] != "sha-6" || event["batch_id"] != "batch-partial" {
		t.Fatalf("delete event=%#v", event)
	}
}

func TestCommitDeletionResultsUncertainDoesNotChangeFileAndOutboxFailureRollsBack(t *testing.T) {
	db := openLocalTestDB(t)
	run := seedLocalResultGroups(t, db)
	ctx := context.Background()
	if err := db.CommitLocalReview(ctx, LocalReviewCommit{
		MachineID: "machine-a", RunID: run.RunID, GroupID: "group-video", Reviewer: "user",
		Decisions: []LocalReviewChoice{{FileID: 5, Decision: "keep"}, {FileID: 6, Decision: "delete"}},
	}); err != nil {
		t.Fatal(err)
	}
	selection, err := db.LoadCommittedDeletion(ctx, "machine-a", run.RunID, "group-video")
	if err != nil {
		t.Fatal(err)
	}
	file := selection.Files[0]
	base := DeletionResult{FileID: file.FileID, MachineID: "machine-a", Path: file.Path,
		SHA512: file.SHA512, Size: file.Size, MTime: file.MTime, RunID: run.RunID,
		GroupID: "group-video", Generation: run.Generation, ConfirmationDigest: "digest"}
	uncertain := base
	uncertain.BatchID, uncertain.OK, uncertain.Uncertain, uncertain.ErrorCode = "batch-uncertain", true, true, "helper_lost"
	if err := db.CommitDeletionResults(ctx, uncertain.BatchID, []DeletionResult{uncertain}); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := db.db.QueryRow(`SELECT status FROM files WHERE id=6`).Scan(&status); err != nil || status != "done" {
		t.Fatalf("uncertain file status=%q err=%v", status, err)
	}
	if countRows(t, db, `SELECT count(*) FROM local_outbox WHERE topic='local.delete'`) != 0 {
		t.Fatal("uncertain deletion emitted deleted outbox")
	}

	if _, err := db.db.Exec(`CREATE TRIGGER reject_delete_outbox BEFORE INSERT ON local_outbox WHEN NEW.topic='local.delete' BEGIN SELECT RAISE(ABORT,'reject delete outbox'); END`); err != nil {
		t.Fatal(err)
	}
	success := base
	success.BatchID, success.OK = "batch-rollback", true
	if err := db.CommitDeletionResults(ctx, success.BatchID, []DeletionResult{success}); err == nil {
		t.Fatal("outbox failure did not fail deletion transaction")
	}
	if err := db.db.QueryRow(`SELECT status FROM files WHERE id=6`).Scan(&status); err != nil || status != "done" {
		t.Fatalf("rolled-back file status=%q err=%v", status, err)
	}
	if countRows(t, db, `SELECT count(*) FROM local_delete_batches WHERE batch_id='batch-rollback'`) != 0 {
		t.Fatal("failed transaction left delete audit")
	}
}
