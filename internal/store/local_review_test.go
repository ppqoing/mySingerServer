package store

import (
	"context"
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
		INSERT INTO local_dup_groups(group_id,run_id,generation,category,verdict,created_at)
		VALUES ('group-1',?,?, 'exact','duplicate',1)`, run.RunID, run.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`
		INSERT INTO local_dup_members(group_id,run_id,generation,file_id,sha512,created_at)
		VALUES ('group-1',?,?,?,'sha-a',1)`, run.RunID, run.Generation, fileID); err != nil {
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
		(batch_id,file_id,path_snapshot,sha512,result,error_code,error_message,uncertain,created_at,updated_at)
		VALUES ('batch-1',?1,'D:\\keep.jpg','sha-keep','failed','access_denied','denied',0,1,2);
		INSERT INTO local_delete_items
		(batch_id,file_id,path_snapshot,sha512,result,error_code,error_message,uncertain,created_at,updated_at)
		VALUES ('batch-1',?1,'D:\\keep.jpg','sha-keep','uncertain','unknown','unknown result',1,1,2);`, fileID); err == nil {
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
		(batch_id,file_id,path_snapshot,sha512,result,uncertain,created_at,updated_at)
		VALUES ('missing',?,'D:\\keep.jpg','sha-keep','failed',0,1,1)`, fileID); err == nil {
		t.Fatal("delete item without batch was accepted")
	}
}
