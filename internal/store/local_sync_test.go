package store

import (
	"context"
	"testing"
)

// Break caught: local_outbox rows are never loaded into a stable local-scope
// snapshot, or are acknowledged without retaining history/deletion identity.
func TestLocalOutboxLoadsLocalScopeSnapshotAndAcknowledgesBySequence(t *testing.T) {
	db := openLocalTestDB(t)
	run := seedLocalResultGroups(t, db)
	ctx := context.Background()
	if err := db.CommitLocalReview(ctx, LocalReviewCommit{
		MachineID: "machine-a", RunID: run.RunID, GroupID: "group-video", Reviewer: "user",
		Decisions: []LocalReviewChoice{{FileID: 5, Decision: "keep"}, {FileID: 6, Decision: "delete"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.EnqueueLocalEvent(ctx, LocalOutboxEvent{
		Topic: "local_analysis.stage", EntityKey: run.RunID + ":final", Generation: run.Generation,
		PayloadJSON: `{"run_id":"` + run.RunID + `","stage":"final","counts":{"groups":4}}`,
	}); err != nil {
		t.Fatal(err)
	}
	selection, err := db.LoadCommittedDeletion(ctx, "machine-a", run.RunID, "group-video")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.BeginDeletionBatch(ctx, "sync-delete", selection, "digest"); err != nil {
		t.Fatal(err)
	}
	if err := db.CommitDeletionResults(ctx, "sync-delete", []DeletionResult{{
		FileID: 6, MachineID: "machine-a", Path: `D:\Video\copy.mp4`, SHA512: "sha-6",
		Size: 500, MTime: 1000, BatchID: "sync-delete", RunID: run.RunID,
		GroupID: "group-video", Generation: run.Generation, ConfirmationDigest: "digest", OK: true,
	}}); err != nil {
		t.Fatal(err)
	}

	rows, err := db.PendingLocalSyncEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 || rows[0].Sequence >= rows[1].Sequence || rows[1].Sequence >= rows[2].Sequence || rows[2].Sequence >= rows[3].Sequence {
		t.Fatalf("pending rows=%#v", rows)
	}
	batch, err := db.LoadLocalSyncBatch(ctx, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Events) != 4 || len(batch.Runs) != 1 || batch.Runs[0].MachineID != "machine-a" ||
		batch.Runs[0].RunID != run.RunID || batch.Runs[0].Generation != run.Generation {
		t.Fatalf("local sync run/events=%#v/%#v", batch.Runs, batch.Events)
	}
	if len(batch.Groups) != 4 || len(batch.Members) != 8 {
		t.Fatalf("groups/members=%d/%d", len(batch.Groups), len(batch.Members))
	}
	if len(batch.Reviews) != 2 || batch.Reviews[0].MachineID != "machine-a" {
		t.Fatalf("reviews=%#v", batch.Reviews)
	}
	if len(batch.Deletes) != 1 {
		t.Fatalf("deletes=%#v", batch.Deletes)
	}
	deleted := batch.Deletes[0]
	if deleted.FileID != 6 || deleted.Path != `D:\Video\copy.mp4` || deleted.SHA512 != "sha-6" ||
		deleted.Result != "deleted" || deleted.Status != "deleted" || deleted.BatchID != "sync-delete" {
		t.Fatalf("deleted row=%#v", deleted)
	}

	if err := db.AcknowledgeLocalSyncEvents(ctx, rows); err != nil {
		t.Fatal(err)
	}
	remaining, err := db.PendingLocalSyncEvents(ctx, 100)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("remaining=%#v err=%v", remaining, err)
	}
	if countRows(t, db, `SELECT count(*) FROM local_dup_members`) != 8 ||
		countRows(t, db, `SELECT count(*) FROM local_reviews`) != 3 ||
		countRows(t, db, `SELECT count(*) FROM local_delete_items`) != 1 {
		t.Fatal("acknowledging local sync removed retained history")
	}
}

func TestLocalOutboxAcknowledgeIsGenerationBoundAndReplaySafe(t *testing.T) {
	db := openLocalTestDB(t)
	ctx := context.Background()
	event := LocalOutboxEvent{Topic: "local.task", EntityKey: "task", Generation: 1, PayloadJSON: `{"state":"done"}`}
	if err := db.EnqueueLocalEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	rows, err := db.PendingLocalSyncEvents(ctx, 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%#v err=%v", rows, err)
	}
	stale := append([]LocalOutboxSyncRow(nil), rows...)
	stale[0].Generation++
	if err := db.AcknowledgeLocalSyncEvents(ctx, stale); err == nil {
		t.Fatal("stale local event generation was acknowledged")
	}
	if pending, _ := db.PendingLocalSyncEvents(ctx, 1); len(pending) != 1 {
		t.Fatal("failed stale ack removed local event")
	}
	if err := db.AcknowledgeLocalSyncEvents(ctx, rows); err != nil {
		t.Fatal(err)
	}
	if err := db.AcknowledgeLocalSyncEvents(ctx, rows); err != nil {
		t.Fatalf("idempotent replay ack failed: %v", err)
	}
}
