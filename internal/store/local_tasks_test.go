package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

func openLocalTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestLocalMigrationNewAndLegacyDatabasesHaveSameSchema(t *testing.T) {
	newDB := openLocalTestDB(t)

	legacyPath := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", "file:"+filepath.ToSlash(legacyPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`
		CREATE TABLE files (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			machine_id TEXT NOT NULL, disk_no INTEGER NOT NULL DEFAULT -1,
			path TEXT NOT NULL, size INTEGER NOT NULL DEFAULT -1,
			mtime INTEGER NOT NULL DEFAULT 0, sha512 TEXT,
			phase1_done INTEGER NOT NULL DEFAULT 0,
			phase2_done INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'pending',
			missing_mask INTEGER NOT NULL DEFAULT 0, error TEXT,
			updated_at INTEGER NOT NULL DEFAULT 0,
			UNIQUE(machine_id, path)
		);
		PRAGMA user_version = 3;`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(legacyPath)
	if err != nil {
		t.Fatalf("Open legacy: %v", err)
	}
	defer upgraded.Close()

	newSchema := normalizedLocalSchema(t, newDB)
	upgradedSchema := normalizedLocalSchema(t, upgraded)
	if !reflect.DeepEqual(upgradedSchema, newSchema) {
		t.Fatalf("upgraded local sqlite_schema differs from new schema\nnew: %#v\nupgraded: %#v", newSchema, upgradedSchema)
	}
	if len(newSchema) < 20 {
		t.Fatalf("local sqlite_schema has only %d objects, indexes were not included", len(newSchema))
	}
	for label, db := range map[string]*DB{"new": newDB, "upgraded": upgraded} {
		var version int
		if err := db.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if version != localSchemaVersion {
			t.Fatalf("%s user_version = %d, want %d", label, version, localSchemaVersion)
		}
		for _, check := range []struct {
			table  string
			column string
		}{
			{"local_pair_scores", "machine_id"},
			{"local_dup_groups", "machine_id"},
			{"local_dup_members", "machine_id"},
			{"local_delete_items", "machine_id"},
		} {
			if !pragmaColumnNotNull(t, db, check.table, check.column) {
				t.Fatalf("%s %s.%s is absent or nullable", label, check.table, check.column)
			}
		}
		for table, want := range map[string][]string{
			"local_analysis_runs": {
				"local_tasks:machine_id->machine_id", "local_tasks:task_id->task_id",
			},
			"local_pair_scores": {
				"files:machine_id->machine_id", "files:left_file_id->id",
				"files:machine_id->machine_id", "files:right_file_id->id",
				"local_analysis_runs:machine_id->machine_id", "local_analysis_runs:run_id->run_id",
				"local_analysis_runs:generation->generation",
			},
			"local_reviews": {
				"local_analysis_runs:machine_id->machine_id", "local_analysis_runs:run_id->run_id",
				"local_analysis_runs:generation->generation",
				"local_dup_groups:machine_id->machine_id", "local_dup_groups:run_id->run_id",
				"local_dup_groups:group_id->group_id",
				"local_dup_members:machine_id->machine_id", "local_dup_members:group_id->group_id",
				"local_dup_members:file_id->file_id",
			},
			"local_delete_items": {
				"files:machine_id->machine_id", "files:file_id->id",
				"local_delete_batches:machine_id->machine_id", "local_delete_batches:batch_id->batch_id",
			},
		} {
			got := pragmaForeignKeyMappings(t, db, table)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s foreign keys for %s = %v, want %v", label, table, got, want)
			}
		}
		if !pragmaHasUniqueIndex(t, db, "files", []string{"machine_id", "id"}) {
			t.Fatalf("%s files lacks UNIQUE(machine_id,id)", label)
		}
		if !pragmaHasIndex(t, db, "local_outbox", "idx_local_outbox_pending") {
			t.Fatalf("%s local_outbox pending index missing", label)
		}
		if _, err := db.db.Exec(`
			INSERT INTO local_outbox(topic,entity_key,generation,payload_json,created_at,updated_at)
			VALUES ('bad','bad',1,'{broken',1,1)`); err == nil {
			t.Fatalf("%s malformed JSON bypassed CHECK", label)
		}
	}
}

type localSchemaObject struct {
	Type  string
	Name  string
	Table string
	SQL   string
}

func normalizedLocalSchema(t *testing.T, db *DB) []localSchemaObject {
	t.Helper()
	rows, err := db.db.Query(`
		SELECT type,name,tbl_name,sql
		FROM sqlite_schema
		WHERE name LIKE 'local_%' OR tbl_name LIKE 'local_%' OR name='idx_files_machine_id'
		ORDER BY type,name,tbl_name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []localSchemaObject
	for rows.Next() {
		var object localSchemaObject
		var sqlText sql.NullString
		if err := rows.Scan(&object.Type, &object.Name, &object.Table, &sqlText); err != nil {
			t.Fatal(err)
		}
		if sqlText.Valid {
			object.SQL = strings.Join(strings.Fields(sqlText.String), " ")
		}
		result = append(result, object)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func pragmaColumnNotNull(t *testing.T, db *DB, table, column string) bool {
	t.Helper()
	var notNull int
	err := db.db.QueryRow(`SELECT "notnull" FROM pragma_table_info(?) WHERE name=?`, table, column).Scan(&notNull)
	if err == sql.ErrNoRows {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return notNull == 1
}

func pragmaForeignKeyMappings(t *testing.T, db *DB, table string) []string {
	t.Helper()
	rows, err := db.db.Query(`SELECT "table","from","to" FROM pragma_foreign_key_list(?)`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var parent, from, to string
		if err := rows.Scan(&parent, &from, &to); err != nil {
			t.Fatal(err)
		}
		result = append(result, parent+":"+from+"->"+to)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(result)
	return result
}

func pragmaHasUniqueIndex(t *testing.T, db *DB, table string, want []string) bool {
	t.Helper()
	rows, err := db.db.Query(`SELECT name,"unique" FROM pragma_index_list(?)`, table)
	if err != nil {
		t.Fatal(err)
	}
	var uniqueNames []string
	for rows.Next() {
		var name string
		var unique int
		if err := rows.Scan(&name, &unique); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if unique == 1 {
			uniqueNames = append(uniqueNames, name)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range uniqueNames {
		columns, err := pragmaIndexColumns(db, name)
		if err != nil {
			t.Fatal(err)
		}
		if reflect.DeepEqual(columns, want) {
			return true
		}
	}
	return false
}

func pragmaHasIndex(t *testing.T, db *DB, table, wantName string) bool {
	t.Helper()
	var count int
	if err := db.db.QueryRow(`SELECT count(*) FROM pragma_index_list(?) WHERE name=?`, table, wantName).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count == 1
}

func pragmaIndexColumns(db *DB, index string) ([]string, error) {
	rows, err := db.db.Query(`SELECT name FROM pragma_index_info(?) ORDER BY seqno`, index)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		result = append(result, name)
	}
	return result, rows.Err()
}

func TestLocalTaskConcurrentCreateIsIdempotentAndConflictsOnEnvelope(t *testing.T) {
	db := openLocalTestDB(t)
	ctx := context.Background()
	in := LocalTaskCreate{
		TaskID: "task-1", MachineID: "machine-a", Source: "local",
		Type: "analysis", Stage: 1, EnvelopeDigest: "digest-a", Envelope: []byte("envelope-a"),
	}

	const callers = 12
	results := make(chan LocalTask, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			task, err := db.CreateOrLoadLocalTask(ctx, in)
			if err != nil {
				errs <- err
				return
			}
			results <- task
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("CreateOrLoadLocalTask: %v", err)
	}
	for task := range results {
		if task.TaskID != in.TaskID || task.Status != "pending" || task.EnvelopeDigest != in.EnvelopeDigest {
			t.Fatalf("task = %#v", task)
		}
	}
	var count int
	if err := db.db.QueryRow(`SELECT count(*) FROM local_tasks WHERE task_id=?`, in.TaskID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("task rows = %d, want 1", count)
	}

	in.EnvelopeDigest = "digest-b"
	if _, err := db.CreateOrLoadLocalTask(ctx, in); !errors.Is(err, ErrLocalTaskConflict) {
		t.Fatalf("different envelope error = %v, want %v", err, ErrLocalTaskConflict)
	}
}

// Break caught: recovery used to retain only a digest, making the accepted
// scan roots and mode impossible to reconstruct after an Agent restart.
func TestLocalTaskPersistsOpaqueEnvelopeAndConflictsOnDifferentBytes(t *testing.T) {
	db := openLocalTestDB(t)
	ctx := context.Background()
	in := LocalTaskCreate{TaskID: "task-envelope", MachineID: "machine-a", Source: "local", Type: "scan", EnvelopeDigest: "digest", Envelope: []byte{1, 2, 3}}
	task, err := db.CreateOrLoadLocalTask(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(task.Envelope, in.Envelope) {
		t.Fatalf("envelope = %v, want %v", task.Envelope, in.Envelope)
	}
	in.Envelope = []byte{1, 2, 4}
	if _, err := db.CreateOrLoadLocalTask(ctx, in); !errors.Is(err, ErrLocalTaskConflict) {
		t.Fatalf("different opaque envelope error = %v, want task conflict", err)
	}
	if _, err := db.CreateOrLoadLocalTask(ctx, LocalTaskCreate{TaskID: "empty", MachineID: "machine-a", Source: "local", Type: "scan", EnvelopeDigest: "digest"}); err == nil {
		t.Fatal("new empty envelope was accepted")
	}
}

// Break caught: unrestricted UPDATEs permit progress rollback and invalid
// terminal-to-running transitions, which makes retries and recovery ambiguous.
func TestLocalTaskLifecycleUsesExplicitTransitionsAndStablePagination(t *testing.T) {
	db := openLocalTestDB(t)
	ctx := context.Background()
	for _, id := range []string{"task-c", "task-a", "task-b"} {
		if _, err := db.CreateOrLoadLocalTask(ctx, LocalTaskCreate{TaskID: id, MachineID: "machine-a", Source: "local", Type: "scan", EnvelopeDigest: id, Envelope: []byte(id)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.CreateOrLoadLocalTask(ctx, LocalTaskCreate{TaskID: "foreign", MachineID: "machine-b", Source: "local", Type: "scan", EnvelopeDigest: "foreign", Envelope: []byte("foreign")}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.TransitionLocalTask(ctx, "machine-a", "task-a", LocalTaskUpdate{Status: "running", Stage: 1, ProgressComplete: 4, ProgressTotal: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.TransitionLocalTask(ctx, "machine-a", "task-a", LocalTaskUpdate{Status: "running", Stage: 1, ProgressComplete: 3, ProgressTotal: 10}); !errors.Is(err, ErrLocalTaskProgressRollback) {
		t.Fatalf("progress rollback error = %v", err)
	}
	page, err := db.ListLocalTasks(ctx, "machine-a", 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0].TaskID != "task-a" || page[1].TaskID != "task-b" {
		t.Fatalf("first page = %#v", page)
	}
	if err := db.CancelLocalTask(ctx, "machine-a", "task-a"); err != nil {
		t.Fatal(err)
	}
	if err := db.CancelLocalTask(ctx, "machine-a", "task-a"); err != nil {
		t.Fatalf("idempotent CancelLocalTask: %v", err)
	}
	retried, err := db.RetryLocalTask(ctx, "machine-a", "task-a")
	if err != nil {
		t.Fatal(err)
	}
	if retried.TaskID != "task-a" || retried.Status != "pending" || retried.Stage != 1 || string(retried.Envelope) != "task-a" {
		t.Fatalf("retried = %#v", retried)
	}
}

// Break caught: trusting a caller-provided source-state allowlist lets a
// succeeded task reopen, a stage move backward, or a known total drift.
func TestLocalTaskTransitionGraphIsOwnedByStore(t *testing.T) {
	db := openLocalTestDB(t)
	ctx := context.Background()
	if _, err := db.CreateOrLoadLocalTask(ctx, LocalTaskCreate{TaskID: "graph", MachineID: "machine-a", Source: "local", Type: "analysis", EnvelopeDigest: "digest", Envelope: []byte("envelope")}); err != nil {
		t.Fatal(err)
	}
	transitions := []LocalTaskUpdate{
		{Status: "running", Stage: 1, ProgressComplete: 2, ProgressTotal: 10},
		{Status: "waiting_recovery", Stage: 1, ProgressComplete: 2, ProgressTotal: 10},
		{Status: "running", Stage: 1, ProgressComplete: 2, ProgressTotal: 10},
		{Status: "succeeded", Stage: 2, ProgressComplete: 10, ProgressTotal: 10},
	}
	for _, update := range transitions {
		if _, err := db.TransitionLocalTask(ctx, "machine-a", "graph", update); err != nil {
			t.Fatalf("transition to %s: %v", update.Status, err)
		}
	}
	for name, update := range map[string]LocalTaskUpdate{
		"terminal reopen": {Status: "running", Stage: 2, ProgressComplete: 10, ProgressTotal: 10},
		"stage rollback":  {Status: "succeeded", Stage: 1, ProgressComplete: 10, ProgressTotal: 10},
		"total drift":     {Status: "succeeded", Stage: 2, ProgressComplete: 10, ProgressTotal: 11},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := db.TransitionLocalTask(ctx, "machine-a", "graph", update); !errors.Is(err, ErrLocalTaskTransition) {
				t.Fatalf("error = %v, want transition rejection", err)
			}
		})
	}
}

func TestLocalTaskRecoveryChangesOnlyRunningTasksForMachine(t *testing.T) {
	db := openLocalTestDB(t)
	ctx := context.Background()
	for _, task := range []LocalTaskCreate{
		{TaskID: "run-a", MachineID: "machine-a", Source: "local", Type: "analysis", Stage: 1, EnvelopeDigest: "a", Envelope: []byte("a")},
		{TaskID: "done-a", MachineID: "machine-a", Source: "local", Type: "analysis", Stage: 1, EnvelopeDigest: "b", Envelope: []byte("b")},
		{TaskID: "run-b", MachineID: "machine-b", Source: "manager", Type: "stage2", Stage: 2, EnvelopeDigest: "c", Envelope: []byte("c")},
	} {
		if _, err := db.CreateOrLoadLocalTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.db.Exec(`UPDATE local_tasks SET status='running' WHERE task_id IN ('run-a','run-b')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE local_tasks SET status='succeeded' WHERE task_id='done-a'`); err != nil {
		t.Fatal(err)
	}

	recovered, err := db.RecoverLocalTasks(ctx, "machine-a")
	if err != nil {
		t.Fatalf("RecoverLocalTasks: %v", err)
	}
	if len(recovered) != 1 || recovered[0].TaskID != "run-a" || recovered[0].Status != "waiting_recovery" {
		t.Fatalf("recovered = %#v", recovered)
	}
	for taskID, want := range map[string]string{
		"run-a": "waiting_recovery", "done-a": "succeeded", "run-b": "running",
	} {
		var got string
		if err := db.db.QueryRow(`SELECT status FROM local_tasks WHERE task_id=?`, taskID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s status = %q, want %q", taskID, got, want)
		}
	}
}

func TestLocalTaskSchemaRejectsInvalidState(t *testing.T) {
	db := openLocalTestDB(t)
	_, err := db.db.Exec(`
		INSERT INTO local_tasks
		(task_id,machine_id,source,type,stage,status,envelope_digest,stats_json,created_at,updated_at)
		VALUES ('bad','m','local','analysis',1,'bogus','d','{}',1,1)`)
	if err == nil {
		t.Fatal("invalid task status was accepted")
	}
}
