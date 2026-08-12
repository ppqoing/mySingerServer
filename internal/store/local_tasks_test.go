package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
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

	wantTables := []string{
		"local_analysis_runs", "local_current_analysis", "local_delete_batches",
		"local_delete_items", "local_dup_groups", "local_dup_members",
		"local_outbox", "local_pair_scores", "local_reviews", "local_tasks",
	}
	for label, db := range map[string]*DB{"new": newDB, "upgraded": upgraded} {
		var version int
		if err := db.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if version != localSchemaVersion {
			t.Fatalf("%s user_version = %d, want %d", label, version, localSchemaVersion)
		}
		rows, err := db.db.Query(`
			SELECT name FROM sqlite_schema
			WHERE type='table' AND name LIKE 'local_%'
			ORDER BY name`)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			got = append(got, name)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, wantTables) {
			t.Fatalf("%s local tables = %v, want %v", label, got, wantTables)
		}
	}
}

func TestLocalTaskConcurrentCreateIsIdempotentAndConflictsOnEnvelope(t *testing.T) {
	db := openLocalTestDB(t)
	ctx := context.Background()
	in := LocalTaskCreate{
		TaskID: "task-1", MachineID: "machine-a", Source: "local",
		Type: "analysis", Stage: 1, EnvelopeDigest: "digest-a",
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

func TestLocalTaskRecoveryChangesOnlyRunningTasksForMachine(t *testing.T) {
	db := openLocalTestDB(t)
	ctx := context.Background()
	for _, task := range []LocalTaskCreate{
		{TaskID: "run-a", MachineID: "machine-a", Source: "local", Type: "analysis", Stage: 1, EnvelopeDigest: "a"},
		{TaskID: "done-a", MachineID: "machine-a", Source: "local", Type: "analysis", Stage: 1, EnvelopeDigest: "b"},
		{TaskID: "run-b", MachineID: "machine-b", Source: "manager", Type: "stage2", Stage: 2, EnvelopeDigest: "c"},
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
