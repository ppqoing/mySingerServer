package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type localTaskDeletionFixture struct {
	DB       *DB
	ControlA LocalTaskControl
	TaskB    LocalTask
	RunA     string
	RunB     string
	Before   map[string][][]any
}

type productionTaskDeleteSyncFixture struct {
	DB       *DB
	Control  LocalTaskControl
	RunID    string
	OtherRun string
}

// Break caught: task deletion matches synthetic topic prefixes instead of the
// production analysis/review payload identities, leaving task-private pending
// events behind or deleting acknowledged, unrelated, or central events.
func TestDeleteLocalTaskDataRemovesProductionAnalysisAndReviewOutbox(t *testing.T) {
	fixture := seedProductionTaskDeleteSyncFixture(t)
	if _, err := fixture.DB.DeleteLocalTaskData(context.Background(), "machine-a", fixture.Control); err != nil {
		t.Fatal(err)
	}

	if got := countWhere(t, fixture.DB, "local_outbox", `ack_at IS NULL AND (
		(topic='local.analysis.published' AND json_extract(payload_json,'$.run_id')='`+fixture.RunID+`') OR
		(topic='local_analysis.stage' AND json_extract(payload_json,'$.run_id')='`+fixture.RunID+`') OR
		(topic='local.review' AND json_extract(payload_json,'$.run_id')='`+fixture.RunID+`')
	)`); got != 0 {
		t.Fatalf("pending task-private production events = %d, want 0", got)
	}
	for _, predicate := range []string{
		"topic='local_analysis.stage' AND entity_key='" + fixture.RunID + ":acked' AND ack_at=77",
		"topic='local.delete' AND entity_key='sync-delete:6' AND ack_at=66 AND json_type(payload_json,'$.run_id') IS NULL",
		"topic='local.analysis.published' AND entity_key='" + fixture.OtherRun + "' AND ack_at IS NULL",
		"topic='central.keep' AND entity_key='" + fixture.RunID + "' AND ack_at=88",
		"topic='local.delete' AND json_extract(payload_json,'$.batch_id')='sync-delete' AND json_extract(payload_json,'$.run_id')='" + fixture.RunID + "'",
	} {
		if got := countWhere(t, fixture.DB, "local_outbox", predicate); got != 1 {
			t.Fatalf("preserved event %q rows = %d, want 1", predicate, got)
		}
	}
	var detached sql.NullString
	if err := fixture.DB.db.QueryRow(`SELECT run_id FROM local_delete_batches WHERE batch_id='sync-delete'`).Scan(&detached); err != nil {
		t.Fatal(err)
	}
	if detached.Valid {
		t.Fatalf("delete audit run_id = %q, want NULL", detached.String)
	}
	if got := countWhere(t, fixture.DB, "local_delete_items", "batch_id='sync-delete'"); got != 1 {
		t.Fatalf("retained delete items = %d, want 1", got)
	}
}

// Break caught: a retained local.delete event still scans a detached batch
// run_id into string or reloads the deleted run, making it a permanent poison
// event ahead of otherwise loadable local sync work.
func TestDeleteLocalTaskDataLeavesLoadableDetachedDeleteSync(t *testing.T) {
	fixture := seedProductionTaskDeleteSyncFixture(t)
	ctx := context.Background()
	if _, err := fixture.DB.DeleteLocalTaskData(ctx, "machine-a", fixture.Control); err != nil {
		t.Fatal(err)
	}
	events, err := fixture.DB.PendingLocalSyncEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := fixture.DB.LoadLocalSyncBatch(ctx, events)
	if err != nil {
		t.Fatalf("LoadLocalSyncBatch after task delete: %v; events=%#v", err, events)
	}
	if len(batch.Events) != 2 || len(batch.Deletes) != 1 || len(batch.Runs) != 1 {
		t.Fatalf("post-delete sync shape events/deletes/runs=%d/%d/%d", len(batch.Events), len(batch.Deletes), len(batch.Runs))
	}
	if batch.Deletes[0].RunID != fixture.RunID || batch.Deletes[0].BatchID != "sync-delete" || batch.Deletes[0].FileID != 6 {
		t.Fatalf("detached delete row = %#v", batch.Deletes[0])
	}
	if batch.Runs[0].RunID != fixture.OtherRun {
		t.Fatalf("deleted run was recalled into sync batch: %#v", batch.Runs)
	}
}

// Break caught: deleting a task either leaves task-owned analysis rows behind,
// removes global/audit/sync data, or promotes an older run after clearing the
// current pointer.
func TestDeleteLocalTaskDataRemovesOnlyTaskOwnedAnalysis(t *testing.T) {
	fixture := seedTwoTaskDeletionFixture(t)
	result, err := fixture.DB.DeleteLocalTaskData(context.Background(), "machine-1", fixture.ControlA)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Deleted || result.AlreadyDeleted || result.DeletedAt <= 0 {
		t.Fatalf("result = %#v, want a fresh deletion", result)
	}
	assertTaskAAnalysisAbsent(t, fixture)
	assertTaskBAndGlobalDataUnchanged(t, fixture)
	requireForeignKeysValid(t, fixture.DB)
}

// Break caught: checking the replacement task before the exact receipt lets a
// late command for a deleted incarnation reject or delete the replacement.
func TestDeleteLocalTaskDataIsExactInstanceIdempotentAcrossTaskIDReuse(t *testing.T) {
	fixture := seedTwoTaskDeletionFixture(t)
	ctx := context.Background()
	first, err := fixture.DB.DeleteLocalTaskData(ctx, "machine-1", fixture.ControlA)
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixture.DB.DeleteLocalTaskData(ctx, "machine-1", fixture.ControlA)
	if err != nil {
		t.Fatal(err)
	}
	if !second.AlreadyDeleted || second.Deleted || second.DeletedAt != first.DeletedAt {
		t.Fatalf("second result = %#v, first = %#v", second, first)
	}

	hasReceipt, err := fixture.DB.HasLocalTaskDeletionReceipt(ctx, "machine-1", fixture.ControlA.TaskID)
	if err != nil || !hasReceipt {
		t.Fatalf("HasLocalTaskDeletionReceipt = %v, %v", hasReceipt, err)
	}
	loaded, err := fixture.DB.LoadLocalTaskDeletionReceipt(ctx, "machine-1", fixture.ControlA.TaskID, fixture.ControlA.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.AlreadyDeleted || loaded.DeletedAt != first.DeletedAt {
		t.Fatalf("loaded receipt = %#v", loaded)
	}
	if _, err := fixture.DB.LoadLocalTaskDeletionReceipt(ctx, "machine-1", fixture.ControlA.TaskID, "missing-instance"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing receipt error = %v, want sql.ErrNoRows", err)
	}

	replacement, err := fixture.DB.CreateOrLoadLocalTask(ctx, LocalTaskCreate{
		TaskID: fixture.ControlA.TaskID, MachineID: "machine-1", Source: "local", Type: "analysis",
		EnvelopeDigest: "replacement-digest", Envelope: []byte("replacement-envelope"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if replacement.InstanceID == fixture.ControlA.InstanceID {
		t.Fatal("replacement reused the deleted instance ID")
	}
	if _, err := fixture.DB.LoadLocalTaskDeletionReceipt(ctx, "machine-1", replacement.TaskID, replacement.InstanceID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("replacement matched old receipt: %v", err)
	}

	replacementBefore := snapshotTables(t, fixture.DB, "local_tasks")
	late, err := fixture.DB.DeleteLocalTaskData(ctx, "machine-1", fixture.ControlA)
	if err != nil {
		t.Fatal(err)
	}
	if !late.AlreadyDeleted || late.DeletedAt != first.DeletedAt {
		t.Fatalf("late old-instance result = %#v", late)
	}
	if got := snapshotTables(t, fixture.DB, "local_tasks"); !reflect.DeepEqual(got, replacementBefore) {
		t.Fatalf("late old-instance request mutated replacement\ngot:  %#v\nwant: %#v", got, replacementBefore)
	}
}

// Break caught: direct Store deletion bypasses instance/revision/status checks,
// deletes acknowledged analysis events, or emits a central withdrawal event.
func TestDeleteLocalTaskDataEnforcesControlAndPreservesAcknowledgedOutbox(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(LocalTaskControl) LocalTaskControl
		status string
		want   error
	}{
		{name: "instance", mutate: func(c LocalTaskControl) LocalTaskControl { c.InstanceID = "wrong"; return c }, status: "deleting", want: ErrLocalTaskInstanceMismatch},
		{name: "revision", mutate: func(c LocalTaskControl) LocalTaskControl { c.ExpectedRevision++; return c }, status: "deleting", want: ErrLocalTaskStale},
		{name: "status", mutate: func(c LocalTaskControl) LocalTaskControl { return c }, status: "paused", want: ErrLocalTaskTransition},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := seedTwoTaskDeletionFixture(t)
			if _, err := fixture.DB.db.Exec(`UPDATE local_tasks SET status=? WHERE task_id=?`, test.status, fixture.ControlA.TaskID); err != nil {
				t.Fatal(err)
			}
			before := snapshotAllUserTables(t, fixture.DB)
			_, err := fixture.DB.DeleteLocalTaskData(context.Background(), "machine-1", test.mutate(fixture.ControlA))
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if got := snapshotAllUserTables(t, fixture.DB); !reflect.DeepEqual(got, before) {
				t.Fatalf("rejected delete mutated database\ngot:  %#v\nwant: %#v", got, before)
			}
		})
	}

	fixture := seedTwoTaskDeletionFixture(t)
	if _, err := fixture.DB.DeleteLocalTaskData(context.Background(), "machine-1", fixture.ControlA); err != nil {
		t.Fatal(err)
	}
	var acked, taskReview, central int
	if err := fixture.DB.db.QueryRow(`SELECT count(*) FROM local_outbox WHERE topic='local_analysis.stage' AND entity_key=? AND ack_at IS NOT NULL`, fixture.RunA+":acked").Scan(&acked); err != nil {
		t.Fatal(err)
	}
	if err := fixture.DB.db.QueryRow(`SELECT count(*) FROM local_outbox WHERE topic='local.review' AND json_extract(payload_json,'$.run_id')=?`, fixture.RunA).Scan(&taskReview); err != nil {
		t.Fatal(err)
	}
	if err := fixture.DB.db.QueryRow(`SELECT count(*) FROM local_outbox WHERE topic LIKE 'central.%'`).Scan(&central); err != nil {
		t.Fatal(err)
	}
	if acked != 1 || taskReview != 0 || central != 1 {
		t.Fatalf("outbox boundary = acked:%d task-review:%d central:%d, want 1/0/1", acked, taskReview, central)
	}
}

// Break caught: using an empty run ID in the exact-entity predicate deletes
// unrelated pending analysis events whose entity key is empty or starts with a
// colon when a scan task has never created an analysis run.
func TestDeleteLocalTaskDataWithoutAnalysisRunPreservesUnownedOutbox(t *testing.T) {
	db := openLocalTestDB(t)
	ctx := context.Background()
	task, err := db.CreateOrLoadLocalTask(ctx, LocalTaskCreate{
		TaskID: "scan-only", MachineID: "machine-1", Source: "local", Type: "scan",
		EnvelopeDigest: "scan-digest", Envelope: []byte("scan-envelope"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`
		UPDATE local_tasks SET status='deleting',revision=4 WHERE task_id='scan-only';
		INSERT INTO local_outbox(topic,entity_key,generation,payload_json,created_at,updated_at) VALUES
			('local_analysis.pending','',1,'{}',1,1),
			('local_analysis.pending',':foreign',2,'{}',2,2);`); err != nil {
		t.Fatal(err)
	}
	task, err = db.LoadLocalTask(ctx, "machine-1", task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotTables(t, db, "local_outbox")["local_outbox"]
	result, err := db.DeleteLocalTaskData(ctx, "machine-1", controlFor(task))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Deleted {
		t.Fatalf("result = %#v, want deleted", result)
	}
	if got := snapshotTables(t, db, "local_outbox")["local_outbox"]; !reflect.DeepEqual(got, before) {
		t.Fatalf("runless task removed unowned outbox\ngot:  %#v\nwant: %#v", got, before)
	}
}

// Break caught: changing the SQL statement order violates foreign keys or
// makes audit rows disappear before their run reference is detached.
func TestDeleteLocalTaskDataUsesDeclaredMutationOrder(t *testing.T) {
	fixture := seedTwoTaskDeletionFixture(t)
	installDeletionOrderTriggers(t, fixture)
	if _, err := fixture.DB.DeleteLocalTaskData(context.Background(), "machine-1", fixture.ControlA); err != nil {
		t.Fatal(err)
	}
	rows, err := fixture.DB.db.Query(`SELECT step FROM deletion_order ORDER BY sequence`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var step string
		if err := rows.Scan(&step); err != nil {
			t.Fatal(err)
		}
		got = append(got, step)
	}
	want := []string{"current", "delete_event", "batches", "outbox", "reviews", "members", "groups", "pairs", "runs", "receipt", "task"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mutation order = %v, want %v", got, want)
	}
}

// Break caught: committing any earlier delete/update before a later write
// fails leaves a partial task purge without its receipt.
func TestDeleteLocalTaskDataRollsBackEveryMutationStep(t *testing.T) {
	steps := []struct {
		name  string
		table string
		event string
		when  string
	}{
		{name: "current", table: "local_current_analysis", event: "DELETE", when: "OLD.run_id='run-a'"},
		{name: "delete_event", table: "local_outbox", event: "UPDATE", when: "OLD.topic='local.delete' AND OLD.entity_key='batch-a:101'"},
		{name: "batches", table: "local_delete_batches", event: "UPDATE", when: "OLD.run_id='run-a'"},
		{name: "outbox", table: "local_outbox", event: "DELETE", when: "OLD.topic='local.analysis.published' AND OLD.entity_key='run-a'"},
		{name: "reviews", table: "local_reviews", event: "DELETE", when: "OLD.run_id='run-a'"},
		{name: "members", table: "local_dup_members", event: "DELETE", when: "OLD.run_id='run-a'"},
		{name: "groups", table: "local_dup_groups", event: "DELETE", when: "OLD.run_id='run-a'"},
		{name: "pairs", table: "local_pair_scores", event: "DELETE", when: "OLD.run_id='run-a'"},
		{name: "runs", table: "local_analysis_runs", event: "DELETE", when: "OLD.run_id='run-a'"},
		{name: "receipt", table: "local_task_deletion_receipts", event: "INSERT", when: "NEW.task_id='task-a'"},
		{name: "task", table: "local_tasks", event: "DELETE", when: "OLD.task_id='task-a'"},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			fixture := seedTwoTaskDeletionFixture(t)
			before := snapshotAllUserTables(t, fixture.DB)
			trigger := fmt.Sprintf(`CREATE TRIGGER fail_delete_step BEFORE %s ON %s WHEN %s BEGIN SELECT RAISE(ABORT,'injected %s failure'); END`, step.event, step.table, step.when, step.name)
			if _, err := fixture.DB.db.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			_, err := fixture.DB.DeleteLocalTaskData(context.Background(), "machine-1", fixture.ControlA)
			if err == nil || errors.Is(err, ErrLocalTaskDeleteRetryable) {
				t.Fatalf("error = %v, want non-retryable injected failure", err)
			}
			if _, err := fixture.DB.db.Exec(`DROP TRIGGER fail_delete_step`); err != nil {
				t.Fatal(err)
			}
			if got := snapshotAllUserTables(t, fixture.DB); !reflect.DeepEqual(got, before) {
				t.Fatalf("%s failure did not roll back\ngot:  %#v\nwant: %#v", step.name, got, before)
			}
			requireForeignKeysValid(t, fixture.DB)
		})
	}
}

// Break caught: transient SQLite lock errors are returned as permanent delete
// failures, causing the lifecycle service to stop retrying.
func TestDeleteLocalTaskDataClassifiesSQLiteBusyAndLockedAsRetryable(t *testing.T) {
	t.Run("busy", func(t *testing.T) {
		fixture := seedTwoTaskDeletionFixture(t)
		if _, err := fixture.DB.db.Exec(`PRAGMA busy_timeout=0`); err != nil {
			t.Fatal(err)
		}
		locker := openRawSQLite(t, filepath.Join(t.TempDir(), "unused.db"), false)
		_ = locker.Close()
		path := databasePath(t, fixture.DB)
		locker = openRawSQLite(t, path, false)
		defer locker.Close()
		if _, err := locker.Exec(`BEGIN EXCLUSIVE`); err != nil {
			t.Fatal(err)
		}
		defer locker.Exec(`ROLLBACK`)
		_, err := fixture.DB.DeleteLocalTaskData(context.Background(), "machine-1", fixture.ControlA)
		if !errors.Is(err, ErrLocalTaskDeleteRetryable) {
			t.Fatalf("busy error = %v, want %v", err, ErrLocalTaskDeleteRetryable)
		}
	})

	t.Run("locked", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "locked.db")
		raw := openRawSQLite(t, path, false)
		defer raw.Close()
		if _, err := raw.Exec(ddl); err != nil {
			t.Fatal(err)
		}
		if _, err := raw.Exec(`
			INSERT INTO local_tasks(task_id,instance_id,revision,machine_id,source,type,stage,status,phase,envelope_digest,envelope,created_at,updated_at)
			VALUES ('locked-task','locked-instance',1,'machine-1','local','analysis',0,'deleting','waiting','digest',X'01',1,1)`); err != nil {
			t.Fatal(err)
		}
		conn, err := raw.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		rows, err := conn.QueryContext(context.Background(), `SELECT * FROM local_tasks`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		if !rows.Next() {
			t.Fatal("locked cursor has no row")
		}
		_, lockedErr := conn.ExecContext(context.Background(), `DROP TABLE local_tasks`)
		if lockedErr == nil {
			t.Fatal("schema mutation with active cursor did not return SQLITE_LOCKED")
		}
		wrapped := wrapLocalTaskDeleteError("locked fixture", lockedErr)
		if !errors.Is(wrapped, ErrLocalTaskDeleteRetryable) {
			t.Fatalf("locked error = %v, wrapped = %v, want %v", lockedErr, wrapped, ErrLocalTaskDeleteRetryable)
		}
	})

	t.Run("corrupt is permanent", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "corrupt.db")
		if err := os.WriteFile(path, []byte("not a sqlite database"), 0o600); err != nil {
			t.Fatal(err)
		}
		raw := openRawSQLite(t, path, false)
		defer raw.Close()
		_, corruptErr := raw.Exec(`SELECT * FROM sqlite_schema`)
		if corruptErr == nil {
			t.Fatal("malformed database did not return a SQLite error")
		}
		wrapped := wrapLocalTaskDeleteError("corrupt fixture", corruptErr)
		if errors.Is(wrapped, ErrLocalTaskDeleteRetryable) {
			t.Fatalf("corrupt error incorrectly marked retryable: %v", wrapped)
		}
	})
}

func seedProductionTaskDeleteSyncFixture(t *testing.T) productionTaskDeleteSyncFixture {
	t.Helper()
	db := openLocalTestDB(t)
	ctx := context.Background()
	run := seedLocalResultGroups(t, db)
	if err := db.CommitLocalReview(ctx, LocalReviewCommit{
		MachineID: "machine-a", RunID: run.RunID, GroupID: "group-video", Reviewer: "user",
		Decisions: []LocalReviewChoice{{FileID: 5, Decision: "keep"}, {FileID: 6, Decision: "delete"}},
	}); err != nil {
		t.Fatal(err)
	}
	// 按生产路径先持久化删除批次，再提交 Helper 返回的逐文件结果。
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
	if _, err := db.db.Exec(`
		INSERT INTO local_outbox(topic,entity_key,generation,payload_json,ack_at,created_at,updated_at)
		VALUES ('local.delete','sync-delete:6',101,
		'{"file_id":6,"machine_id":"machine-a","status":"deleted","sha512":"sha-6","batch_id":"sync-delete"}',66,1,1)`); err != nil {
		t.Fatal(err)
	}
	for _, event := range []LocalOutboxEvent{
		{Topic: "local_analysis.stage", EntityKey: run.RunID + ":final", Generation: run.Generation,
			PayloadJSON: `{"run_id":"` + run.RunID + `","stage":"final","counts":{"groups":4}}`},
		{Topic: "local_analysis.stage", EntityKey: run.RunID + ":acked", Generation: run.Generation + 1,
			PayloadJSON: `{"run_id":"` + run.RunID + `","stage":"acked","counts":{}}`},
	} {
		if err := db.EnqueueLocalEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.db.Exec(`UPDATE local_outbox SET ack_at=77 WHERE topic='local_analysis.stage' AND entity_key=?`, run.RunID+":acked"); err != nil {
		t.Fatal(err)
	}

	createAnalysisTask(t, db, "task-sync-other", "machine-a")
	other, err := db.BeginLocalAnalysis(ctx, "machine-a", "task-sync-other")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteLocalAnalysis(ctx, other.RunID); err != nil {
		t.Fatal(err)
	}
	if err := db.PublishLocalAnalysis(ctx, other.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`
		INSERT INTO local_outbox(topic,entity_key,generation,payload_json,ack_at,created_at,updated_at)
		VALUES ('central.keep',?1,0,'{"scope":"central"}',88,1,1)`, run.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE local_tasks SET status='deleting',revision=7 WHERE task_id=?`, run.TaskID); err != nil {
		t.Fatal(err)
	}
	task, err := db.LoadLocalTask(ctx, "machine-a", run.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	return productionTaskDeleteSyncFixture{DB: db, Control: controlFor(task), RunID: run.RunID, OtherRun: other.RunID}
}

func seedTwoTaskDeletionFixture(t *testing.T) localTaskDeletionFixture {
	t.Helper()
	db := openLocalTestDB(t)
	ctx := context.Background()
	create := func(taskID string) LocalTask {
		task, err := db.CreateOrLoadLocalTask(ctx, LocalTaskCreate{
			TaskID: taskID, MachineID: "machine-1", Source: "local", Type: "analysis",
			EnvelopeDigest: "digest-" + taskID, Envelope: []byte("envelope-" + taskID),
		})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	taskA, taskB := create("task-a"), create("task-b")
	if _, err := db.db.Exec(`UPDATE local_tasks SET status='deleting',revision=7 WHERE task_id='task-a'`); err != nil {
		t.Fatal(err)
	}
	taskA, _ = db.LoadLocalTask(ctx, "machine-1", "task-a")

	if _, err := db.db.Exec(`
		INSERT INTO files(id,machine_id,path,sha512,status) VALUES
			(101,'machine-1','D:\\a.jpg','sha-a','done'),
			(102,'machine-1','D:\\b.jpg','sha-b','done');
		INSERT INTO image_features(sha512,width,height,pdq256) VALUES
			('sha-a',10,20,X'01'),('sha-b',30,40,X'02');
		INSERT INTO video_features(sha512,duration_ms,thumb_path) VALUES
			('sha-a',100,'cache-a'),('sha-b',200,'cache-b');
		INSERT INTO video_frames(sha512,frame_idx,pdq256) VALUES
			('sha-a',0,X'03'),('sha-b',0,X'04');
		INSERT INTO sync_queue(table_name,row_pk,synced,enqueued_at,generation) VALUES
			('files','101',0,11,3),('image_features','sha-a',1,12,4);
		INSERT INTO local_analysis_runs(run_id,machine_id,generation,task_id,status,created_at,published_at) VALUES
			('run-b','machine-1',10,'task-b','published',10,20),
			('run-a','machine-1',20,'task-a','published',30,40);
		INSERT INTO local_pair_scores(machine_id,run_id,generation,pair_key,left_file_id,right_file_id,left_sha512,right_sha512,stage1_json,created_at,updated_at) VALUES
			('machine-1','run-a',20,'pair-a',101,102,'sha-a','sha-b','{}',1,1),
			('machine-1','run-b',10,'pair-b',101,102,'sha-a','sha-b','{}',2,2);
		INSERT INTO local_dup_groups(group_id,machine_id,run_id,generation,category,verdict,created_at) VALUES
			('group-a','machine-1','run-a',20,'exact','duplicate',1),
			('group-b','machine-1','run-b',10,'image','uncertain',2);
		INSERT INTO local_dup_members(group_id,machine_id,run_id,generation,file_id,sha512,created_at) VALUES
			('group-a','machine-1','run-a',20,101,'sha-a',1),
			('group-b','machine-1','run-b',10,102,'sha-b',2);
		INSERT INTO local_reviews(review_id,machine_id,run_id,generation,group_id,file_id,decision,reviewer,note,reviewed_at) VALUES
			('review-a','machine-1','run-a',20,'group-a',101,'delete','user','a',1),
			('review-b','machine-1','run-b',10,'group-b',102,'keep','user','b',2);
		INSERT INTO local_current_analysis(machine_id,run_id,generation,published_at) VALUES
			('machine-1','run-a',20,40);
		INSERT INTO local_delete_batches(batch_id,machine_id,run_id,confirmation_digest,status,requested_count,succeeded_count,created_at,updated_at) VALUES
			('batch-a','machine-1','run-a','confirm-a','succeeded',1,1,1,2),
			('batch-b','machine-1','run-b','confirm-b','failed',1,0,3,4);
		INSERT INTO local_delete_items(batch_id,machine_id,file_id,path_snapshot,sha512,result,created_at,updated_at) VALUES
			('batch-a','machine-1',101,'D:\\a.jpg','sha-a','deleted',1,2),
			('batch-b','machine-1',102,'D:\\b.jpg','sha-b','failed',3,4);
		INSERT INTO local_outbox(topic,entity_key,generation,payload_json,ack_at,retry_count,next_retry_at,last_error,created_at,updated_at) VALUES
			('local.analysis.published','run-a',20,'{"machine_id":"machine-1","run_id":"run-a","generation":20,"status":"published"}',NULL,0,NULL,NULL,1,1),
			('local_analysis.stage','run-a:final',20,'{"run_id":"run-a","stage":"final","counts":{}}',NULL,2,10,'retry',2,2),
			('local_analysis.stage','run-a:acked',21,'{"run_id":"run-a","stage":"acked","counts":{}}',99,0,NULL,NULL,3,3),
			('local_analysis.stage','run-a-extra:final',22,'{"run_id":"run-other","stage":"final","counts":{}}',NULL,0,NULL,NULL,4,4),
			('local_analysis.stage','xrun-a:final',23,'{"run_id":"xrun-a","stage":"final","counts":{}}',NULL,0,NULL,NULL,5,5),
			('local.review','group-a',20,'{"machine_id":"machine-1","run_id":"run-a","generation":20,"group_id":"group-a","decisions":[]}',NULL,0,NULL,NULL,6,6),
			('local.analysis.published','run-b',10,'{"machine_id":"machine-1","run_id":"run-b","generation":10,"status":"published"}',NULL,0,NULL,NULL,7,7),
			('local.delete','batch-a:101',20,'{"file_id":101,"machine_id":"machine-1","status":"deleted","sha512":"sha-a","batch_id":"batch-a"}',NULL,0,NULL,NULL,8,8),
			('central.sync','run-a',0,'{}',NULL,0,NULL,NULL,9,9);`); err != nil {
		t.Fatal(err)
	}
	return localTaskDeletionFixture{
		DB: db, ControlA: controlFor(taskA), TaskB: taskB, RunA: "run-a", RunB: "run-b",
		Before: snapshotAllUserTables(t, db),
	}
}

func assertTaskAAnalysisAbsent(t *testing.T, fixture localTaskDeletionFixture) {
	t.Helper()
	for table, predicate := range map[string]string{
		"local_tasks": "task_id='task-a'", "local_analysis_runs": "run_id='run-a'",
		"local_current_analysis": "run_id='run-a'", "local_reviews": "run_id='run-a'",
		"local_dup_members": "run_id='run-a'", "local_dup_groups": "run_id='run-a'",
		"local_pair_scores": "run_id='run-a'",
	} {
		var count int
		if err := fixture.DB.db.QueryRow(`SELECT count(*) FROM ` + table + ` WHERE ` + predicate).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s still has %d task-a rows", table, count)
		}
	}
	var runID sql.NullString
	if err := fixture.DB.db.QueryRow(`SELECT run_id FROM local_delete_batches WHERE batch_id='batch-a'`).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	if runID.Valid {
		t.Fatalf("batch-a run_id = %q, want NULL", runID.String)
	}
	if got := countWhere(t, fixture.DB, "local_delete_items", "batch_id='batch-a'"); got != 1 {
		t.Fatalf("batch-a items = %d, want 1", got)
	}
	if got := countWhere(t, fixture.DB, "local_outbox", `ack_at IS NULL AND (
		(topic='local.analysis.published' AND json_extract(payload_json,'$.run_id')='run-a') OR
		(topic='local_analysis.stage' AND json_extract(payload_json,'$.run_id')='run-a') OR
		(topic='local.review' AND json_extract(payload_json,'$.run_id')='run-a')
	)`); got != 0 {
		t.Fatalf("task-a pending analysis outbox = %d, want 0", got)
	}
	if got := countWhere(t, fixture.DB, "local_task_deletion_receipts", "machine_id='machine-1' AND task_id='task-a' AND instance_id='"+fixture.ControlA.InstanceID+"'"); got != 1 {
		t.Fatalf("task-a receipts = %d, want 1", got)
	}
	if got := countWhere(t, fixture.DB, "local_current_analysis", "machine_id='machine-1'"); got != 0 {
		t.Fatalf("current analysis fell back to an older run: %d rows", got)
	}
}

func assertTaskBAndGlobalDataUnchanged(t *testing.T, fixture localTaskDeletionFixture) {
	t.Helper()
	for _, table := range []string{"files", "image_features", "video_features", "video_frames", "sync_queue", "local_delete_items"} {
		got := snapshotTables(t, fixture.DB, table)[table]
		if !reflect.DeepEqual(got, fixture.Before[table]) {
			t.Fatalf("global/audit table %s changed\ngot:  %#v\nwant: %#v", table, got, fixture.Before[table])
		}
	}
	for table, predicate := range map[string]string{
		"local_tasks": "task_id='task-b'", "local_analysis_runs": "run_id='run-b'",
		"local_pair_scores": "run_id='run-b'", "local_dup_groups": "run_id='run-b'",
		"local_dup_members": "run_id='run-b'", "local_reviews": "run_id='run-b'",
		"local_delete_batches": "batch_id='batch-b'", "local_outbox": "entity_key='run-b'",
	} {
		if got := countWhere(t, fixture.DB, table, predicate); got != 1 {
			t.Fatalf("preserved %s rows = %d, want 1", table, got)
		}
	}
	for _, predicate := range []string{
		"topic='local_analysis.stage' AND entity_key='run-a:acked' AND ack_at=99",
		"topic='local_analysis.stage' AND entity_key='run-a-extra:final'",
		"topic='local_analysis.stage' AND entity_key='xrun-a:final'",
		"topic='local.delete' AND entity_key='batch-a:101' AND json_extract(payload_json,'$.run_id')='run-a'",
		"topic='central.sync' AND entity_key='run-a'",
	} {
		if got := countWhere(t, fixture.DB, "local_outbox", predicate); got != 1 {
			t.Fatalf("preserved outbox %q rows = %d, want 1", predicate, got)
		}
	}
}

func installDeletionOrderTriggers(t *testing.T, fixture localTaskDeletionFixture) {
	t.Helper()
	if _, err := fixture.DB.db.Exec(`CREATE TABLE deletion_order(sequence INTEGER PRIMARY KEY AUTOINCREMENT, step TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	triggers := []string{
		`CREATE TRIGGER order_current BEFORE DELETE ON local_current_analysis WHEN OLD.run_id='run-a' BEGIN INSERT INTO deletion_order(step) VALUES ('current'); END`,
		`CREATE TRIGGER order_delete_event BEFORE UPDATE ON local_outbox WHEN OLD.topic='local.delete' AND OLD.entity_key='batch-a:101' BEGIN INSERT INTO deletion_order(step) VALUES ('delete_event'); END`,
		`CREATE TRIGGER order_batches BEFORE UPDATE ON local_delete_batches WHEN OLD.run_id='run-a' BEGIN INSERT INTO deletion_order(step) VALUES ('batches'); END`,
		`CREATE TRIGGER order_outbox BEFORE DELETE ON local_outbox WHEN OLD.topic='local.analysis.published' AND OLD.entity_key='run-a' BEGIN INSERT INTO deletion_order(step) VALUES ('outbox'); END`,
		`CREATE TRIGGER order_reviews BEFORE DELETE ON local_reviews WHEN OLD.run_id='run-a' BEGIN INSERT INTO deletion_order(step) VALUES ('reviews'); END`,
		`CREATE TRIGGER order_members BEFORE DELETE ON local_dup_members WHEN OLD.run_id='run-a' BEGIN INSERT INTO deletion_order(step) VALUES ('members'); END`,
		`CREATE TRIGGER order_groups BEFORE DELETE ON local_dup_groups WHEN OLD.run_id='run-a' BEGIN INSERT INTO deletion_order(step) VALUES ('groups'); END`,
		`CREATE TRIGGER order_pairs BEFORE DELETE ON local_pair_scores WHEN OLD.run_id='run-a' BEGIN INSERT INTO deletion_order(step) VALUES ('pairs'); END`,
		`CREATE TRIGGER order_runs BEFORE DELETE ON local_analysis_runs WHEN OLD.run_id='run-a' BEGIN INSERT INTO deletion_order(step) VALUES ('runs'); END`,
		`CREATE TRIGGER order_receipt BEFORE INSERT ON local_task_deletion_receipts WHEN NEW.task_id='task-a' BEGIN INSERT INTO deletion_order(step) VALUES ('receipt'); END`,
		`CREATE TRIGGER order_task BEFORE DELETE ON local_tasks WHEN OLD.task_id='task-a' BEGIN INSERT INTO deletion_order(step) VALUES ('task'); END`,
	}
	for _, trigger := range triggers {
		if _, err := fixture.DB.db.Exec(trigger); err != nil {
			t.Fatal(err)
		}
	}
}

func countWhere(t *testing.T, db *DB, table, predicate string) int {
	t.Helper()
	var count int
	if err := db.db.QueryRow(`SELECT count(*) FROM ` + table + ` WHERE ` + predicate).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func snapshotAllUserTables(t *testing.T, db *DB) map[string][][]any {
	t.Helper()
	rows, err := db.db.Query(`SELECT name FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name <> 'deletion_order' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	return snapshotTables(t, db, tables...)
}

func snapshotTables(t *testing.T, db *DB, tables ...string) map[string][][]any {
	t.Helper()
	result := make(map[string][][]any, len(tables))
	for _, table := range tables {
		rows, err := db.db.Query(`SELECT * FROM ` + table + ` ORDER BY rowid`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			destinations := make([]any, len(columns))
			for index := range values {
				destinations[index] = &values[index]
			}
			if err := rows.Scan(destinations...); err != nil {
				t.Fatal(err)
			}
			for index, value := range values {
				if bytes, ok := value.([]byte); ok {
					values[index] = append([]byte(nil), bytes...)
				}
			}
			result[table] = append(result[table], values)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func openRawSQLite(t *testing.T, path string, shared bool) *sql.DB {
	t.Helper()
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=busy_timeout(0)&_pragma=foreign_keys(1)"
	if shared {
		dsn += "&cache=shared"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	return db
}

func databasePath(t *testing.T, db *DB) string {
	t.Helper()
	rows, err := db.db.Query(`PRAGMA database_list`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var sequence int
		var name, path string
		if err := rows.Scan(&sequence, &name, &path); err != nil {
			t.Fatal(err)
		}
		if name == "main" {
			return path
		}
	}
	t.Fatal("main database path not found")
	return ""
}
