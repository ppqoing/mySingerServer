package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

// Break caught: opening a v3 Agent database used to retain only the mutable
// task row, so a restart could not distinguish task incarnations or recover a
// durable lifecycle phase.
func TestLocalTaskV3MigrationAddsVersionedLifecycleWithoutLosingDependents(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy-v3.db")
	seedLocalTaskV3Database(t, dbPath, "running", 2)
	db := openTestDBAt(t, dbPath)

	var version int
	if err := db.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 4 {
		t.Fatalf("user_version=%d, want 4", version)
	}
	task, err := db.LoadLocalTask(context.Background(), "machine-1", "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if task.InstanceID == "" {
		t.Fatal("missing migrated instance ID")
	}
	if task.Revision != 1 {
		t.Fatalf("revision=%d, want 1", task.Revision)
	}
	if task.Phase != "stage3" {
		t.Fatalf("phase=%q, want stage3", task.Phase)
	}
	if task.ProgressComplete != 5 || task.ProgressTotal != 10 || !task.ProgressTotalKnown ||
		string(task.Envelope) != "legacy-envelope" || task.CreatedAt != 100 || task.UpdatedAt != 200 ||
		task.StartedAt == nil || *task.StartedAt != 150 || task.CompletedAt != nil {
		t.Fatalf("migrated task lost snapshot data: %#v", task)
	}
	var runID, taskID string
	if err := db.db.QueryRow(`SELECT run_id,task_id FROM local_analysis_runs`).Scan(&runID, &taskID); err != nil {
		t.Fatalf("load dependent local_analysis_run: %v", err)
	}
	if runID != "run-1" || taskID != "task-1" {
		t.Fatalf("dependent row = (%q,%q)", runID, taskID)
	}
	requireForeignKeysValid(t, db)
}

// Break caught: a failed lifecycle rebuild previously stamped the database as
// current, preventing a corrected subsequent Open from completing migration.
func TestLocalTaskV3MigrationFailureDoesNotAdvanceVersion(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy-v3-invalid.db")
	seedLocalTaskV3Database(t, dbPath, "unknown", 2)
	if _, err := Open(dbPath); err == nil {
		t.Fatal("Open accepted invalid legacy lifecycle status")
	}

	legacy, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := legacy.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	if version != 3 {
		legacy.Close()
		t.Fatalf("user_version after failed migration=%d, want 3", version)
	}
	if _, err := legacy.Exec(`UPDATE local_tasks SET status='pending' WHERE task_id='task-1'`); err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	if db := openTestDBAt(t, dbPath); db == nil {
		t.Fatal("Open after correcting legacy data returned nil DB")
	}
}

// Break caught: deriving the phase directly from the numeric v3 stage made
// pending work look scanned and shifted the legacy stage-2 meaning.
func TestLocalTaskV3MigrationMapsLegacyStatusAndStageToNearestPhase(t *testing.T) {
	for _, test := range []struct {
		name   string
		status string
		stage  int
		phase  string
	}{
		{name: "pending stage zero", status: "pending", stage: 0, phase: "waiting"},
		{name: "running stage zero", status: "running", stage: 0, phase: "scan"},
		{name: "stage one", status: "running", stage: 1, phase: "stage1"},
		{name: "stage two", status: "running", stage: 2, phase: "stage3"},
		{name: "stage three", status: "running", stage: 3, phase: "finalizing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "legacy-v3.db")
			seedLocalTaskV3Database(t, path, test.status, test.stage)
			db := openTestDBAt(t, path)
			task, err := db.LoadLocalTask(context.Background(), "machine-1", "task-1")
			if err != nil {
				t.Fatal(err)
			}
			if task.Phase != test.phase {
				t.Fatalf("phase=%q, want %q", task.Phase, test.phase)
			}
		})
	}
}

// Break caught: expanding the lifecycle state machine without a database
// constraint allowed typos to persist and omitted valid paused/delete states.
func TestLocalTaskSchemaAcceptsOnlyVersionedLifecycleStates(t *testing.T) {
	db := openLocalTestDB(t)
	for index, status := range []string{
		"pending", "running", "waiting_recovery", "pausing", "paused", "stopping",
		"cancelled", "succeeded", "failed", "deleting", "delete_failed",
	} {
		if _, err := db.db.Exec(`
			INSERT INTO local_tasks
			(task_id,instance_id,revision,machine_id,source,type,stage,status,phase,envelope_digest,envelope,progress_total_known,stats_json,created_at,updated_at)
			VALUES (?1,?2,1,'m','local','analysis',0,?3,'waiting','digest',X'01',0,'{}',1,1)`,
			"allowed-"+status, fmt.Sprintf("instance-%d", index), status,
		); err != nil {
			t.Fatalf("status %q rejected: %v", status, err)
		}
	}
	if _, err := db.db.Exec(`
		INSERT INTO local_tasks
		(task_id,instance_id,revision,machine_id,source,type,stage,status,phase,envelope_digest,envelope,progress_total_known,stats_json,created_at,updated_at)
		VALUES ('bad','instance-bad',1,'m','local','analysis',0,'bogus','waiting','digest',X'01',0,'{}',1,1)`); err == nil {
		t.Fatal("unknown task status was accepted")
	}
	if !pragmaHasUniqueIndex(t, db, "local_task_deletion_receipts", []string{"machine_id", "task_id", "instance_id"}) {
		t.Fatal("local_task_deletion_receipts primary key is missing")
	}
}

// Break caught: a table that merely named the v4 columns and states, while
// accepting invalid values, was treated as migrated and stamped version 4.
func TestLocalTaskMigrationRebuildsNearV4TablesWithWeakenedConstraints(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "near-v4.db")
	legacy, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`
		CREATE TABLE local_tasks (
			task_id TEXT PRIMARY KEY,
			instance_id TEXT NOT NULL,
			revision INTEGER NOT NULL DEFAULT 1 CHECK (revision > 0),
			machine_id TEXT NOT NULL,
			source TEXT NOT NULL CHECK (source IN ('local','manager')),
			type TEXT NOT NULL CHECK (type IN ('scan','analysis','stage2','stage3','delete')),
			stage INTEGER NOT NULL CHECK (stage IN (0,1,2,3)),
			status TEXT NOT NULL CHECK (status IN ('pending','running','waiting_recovery','pausing','paused','stopping','cancelled','succeeded','failed','deleting','delete_failed','bogus')),
			phase TEXT NOT NULL DEFAULT 'waiting',
			envelope_digest TEXT NOT NULL,
			envelope BLOB NOT NULL DEFAULT X'',
			progress_completed INTEGER NOT NULL DEFAULT 0 CHECK (progress_completed >= 0),
			progress_total INTEGER NOT NULL DEFAULT 0 CHECK (progress_total >= 0),
			progress_total_known INTEGER NOT NULL DEFAULT 0,
			stats_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(stats_json)),
			safe_error_code TEXT,
			safe_error_message TEXT,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			started_at INTEGER,
			completed_at INTEGER,
			UNIQUE (machine_id, task_id),
			CHECK (progress_total = 0 OR progress_completed <= progress_total)
		);
		CREATE TABLE local_task_deletion_receipts (
			machine_id TEXT NOT NULL,
			task_id TEXT NOT NULL,
			instance_id TEXT NOT NULL,
			deleted_at INTEGER NOT NULL,
			PRIMARY KEY (machine_id, task_id)
		);
		INSERT INTO local_tasks
			(task_id,instance_id,revision,machine_id,source,type,stage,status,phase,envelope_digest,envelope,progress_total_known,stats_json,created_at,updated_at)
		VALUES ('near-v4','instance-1',1,'m','local','analysis',0,'running','scan','digest',X'01',0,'{}',1,1);
		PRAGMA user_version = 3;`); err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	db := openTestDBAt(t, dbPath)
	for name, statement := range map[string]string{
		"revision": `UPDATE local_tasks SET revision=0 WHERE task_id='near-v4'`,
		"status":   `UPDATE local_tasks SET status='bogus' WHERE task_id='near-v4'`,
		"phase":    `UPDATE local_tasks SET phase='bogus' WHERE task_id='near-v4'`,
		"total":    `UPDATE local_tasks SET progress_total_known=2 WHERE task_id='near-v4'`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := db.db.Exec(statement); err == nil {
				t.Fatalf("weakened %s constraint was retained", name)
			}
		})
	}
	if !pragmaHasUniqueIndex(t, db, "local_task_deletion_receipts", []string{"machine_id", "task_id", "instance_id"}) {
		t.Fatal("deletion receipt v4 primary key was not rebuilt")
	}
	var version int
	if err := db.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 4 {
		t.Fatalf("user_version=%d, want 4 after rebuilt constraints", version)
	}
}

// Break caught: repairing an unrelated malformed receipt table used to rebuild
// an already-valid v4 task row, changing its incarnation and snapshot fields.
func TestLocalTaskMigrationRepairsOnlyMalformedReceiptsWithoutRebuildingV4Tasks(t *testing.T) {
	for name, receiptColumns := range map[string]string{
		"wrong primary key": `
			machine_id TEXT NOT NULL,
			task_id TEXT NOT NULL,
			instance_id TEXT NOT NULL,
			deleted_at INTEGER NOT NULL,
			PRIMARY KEY (machine_id, task_id)`,
		"missing deleted at": `
			machine_id TEXT NOT NULL,
			task_id TEXT NOT NULL,
			instance_id TEXT NOT NULL,
			PRIMARY KEY (machine_id, task_id, instance_id)`,
		"wrong deleted at type": `
			machine_id TEXT NOT NULL,
			task_id TEXT NOT NULL,
			instance_id TEXT NOT NULL,
			deleted_at TEXT NOT NULL,
			PRIMARY KEY (machine_id, task_id, instance_id)`,
		"nullable deleted at": `
			machine_id TEXT NOT NULL,
			task_id TEXT NOT NULL,
			instance_id TEXT NOT NULL,
			deleted_at INTEGER,
			PRIMARY KEY (machine_id, task_id, instance_id)`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "v4-with-bad-receipts.db")
			seed, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			created, err := seed.CreateOrLoadLocalTask(context.Background(), LocalTaskCreate{
				TaskID: "stable-task", MachineID: "machine-1", Source: "local", Type: "analysis", Stage: 1,
				EnvelopeDigest: "stable-digest", Envelope: []byte("stable-envelope"),
			})
			if err != nil {
				seed.Close()
				t.Fatal(err)
			}
			if _, err := seed.db.Exec(`
				UPDATE local_tasks
				SET revision=9,status='paused',phase='stage2',progress_total=0,progress_total_known=1,
					stats_json='{"stable":true}',started_at=123,completed_at=456
				WHERE task_id='stable-task'`); err != nil {
				seed.Close()
				t.Fatal(err)
			}
			if err := seed.Close(); err != nil {
				t.Fatal(err)
			}

			raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(fmt.Sprintf(`
				PRAGMA foreign_keys = OFF;
				DROP TABLE local_task_deletion_receipts;
				CREATE TABLE local_task_deletion_receipts (%s);`, receiptColumns)); err != nil {
				raw.Close()
				t.Fatal(err)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}

			db := openTestDBAt(t, path)
			got, err := db.LoadLocalTask(context.Background(), "machine-1", "stable-task")
			if err != nil {
				t.Fatal(err)
			}
			if got.InstanceID != created.InstanceID || got.Revision != 9 || got.Phase != "stage2" ||
				!got.ProgressTotalKnown || got.Status != "paused" || got.StatsJSON != `{"stable":true}` ||
				got.StartedAt == nil || *got.StartedAt != 123 || got.CompletedAt == nil || *got.CompletedAt != 456 ||
				string(got.Envelope) != "stable-envelope" {
				t.Fatalf("receipt repair changed v4 task snapshot: %#v", got)
			}
			if !pragmaHasUniqueIndex(t, db, "local_task_deletion_receipts", []string{"machine_id", "task_id", "instance_id"}) {
				t.Fatal("receipt primary key was not repaired")
			}
			var columnType string
			var notNull int
			if err := db.db.QueryRow(`SELECT type,"notnull" FROM pragma_table_info('local_task_deletion_receipts') WHERE name='deleted_at'`).Scan(&columnType, &notNull); err != nil {
				t.Fatalf("load repaired deleted_at column: %v", err)
			}
			if columnType != "INTEGER" || notNull != 1 {
				t.Fatalf("deleted_at = (%q, notnull=%d), want INTEGER NOT NULL", columnType, notNull)
			}
		})
	}
}

func openTestDBAt(t *testing.T, path string) *DB {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedLocalTaskV3Database(t *testing.T, path, status string, stage int) {
	t.Helper()
	legacy, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = legacy.Close() })
	if _, err := legacy.Exec(`
		PRAGMA foreign_keys = ON;
		CREATE TABLE local_tasks (
			task_id TEXT PRIMARY KEY,
			machine_id TEXT NOT NULL,
			source TEXT NOT NULL,
			type TEXT NOT NULL,
			stage INTEGER NOT NULL,
			status TEXT NOT NULL,
			envelope_digest TEXT NOT NULL,
			envelope BLOB NOT NULL DEFAULT X'',
			progress_completed INTEGER NOT NULL DEFAULT 0,
			progress_total INTEGER NOT NULL DEFAULT 0,
			stats_json TEXT NOT NULL DEFAULT '{}',
			safe_error_code TEXT,
			safe_error_message TEXT,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			started_at INTEGER,
			completed_at INTEGER,
			UNIQUE(machine_id, task_id)
		);
		CREATE TABLE local_analysis_runs (
			run_id TEXT PRIMARY KEY,
			machine_id TEXT NOT NULL,
			generation INTEGER NOT NULL,
			task_id TEXT NOT NULL UNIQUE,
			status TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			completed_at INTEGER,
			published_at INTEGER,
			UNIQUE(machine_id, generation),
			UNIQUE(machine_id, run_id),
			UNIQUE(run_id, generation),
			UNIQUE(machine_id, run_id, generation),
			FOREIGN KEY(machine_id, task_id) REFERENCES local_tasks(machine_id, task_id) ON DELETE RESTRICT
		);
		INSERT INTO local_tasks
			(task_id,machine_id,source,type,stage,status,envelope_digest,envelope,progress_completed,progress_total,stats_json,safe_error_code,safe_error_message,created_at,updated_at,started_at,completed_at)
		VALUES ('task-1','machine-1','local','analysis',?2,?1,'digest-1',X'6C65676163792D656E76656C6F7065',5,10,'{"legacy":true}','legacy-code','legacy-message',100,200,150,NULL);
		INSERT INTO local_analysis_runs
			(run_id,machine_id,generation,task_id,status,created_at)
		VALUES ('run-1','machine-1',1,'task-1','building',100);
		PRAGMA user_version = 3;`, status, stage); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
}

func requireForeignKeysValid(t *testing.T, db *DB) {
	t.Helper()
	rows, err := db.db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("PRAGMA foreign_key_check reported a violation")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
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

func TestLoadLocalTaskIsMachineScopedAndReturnsOwnedEnvelope(t *testing.T) {
	db := openLocalTestDB(t)
	created, err := db.CreateOrLoadLocalTask(context.Background(), LocalTaskCreate{TaskID: "load-task", MachineID: "machine-a", Source: "local", Type: "scan", EnvelopeDigest: "digest", Envelope: []byte("envelope")})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := db.LoadLocalTask(context.Background(), "machine-a", created.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Envelope[0] = 'X'
	again, err := db.LoadLocalTask(context.Background(), "machine-a", created.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if string(again.Envelope) != "envelope" {
		t.Fatalf("stored envelope aliased caller: %q", again.Envelope)
	}
	if _, err := db.LoadLocalTask(context.Background(), "machine-b", created.TaskID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-machine LoadLocalTask=%v, want sql.ErrNoRows", err)
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

func TestLocalTaskIdempotencyIgnoresMutableStage(t *testing.T) {
	db := openLocalTestDB(t)
	ctx := context.Background()
	in := LocalTaskCreate{TaskID: "stage-idempotent", MachineID: "machine-a", Source: "local", Type: "analysis", EnvelopeDigest: "digest", Envelope: []byte("same"), Stage: 0}
	if _, err := db.CreateOrLoadLocalTask(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := db.TransitionLocalTask(ctx, "machine-a", in.TaskID, LocalTaskUpdate{Status: "running", Stage: 2}); err != nil {
		t.Fatal(err)
	}
	in.Stage = 0
	got, err := db.CreateOrLoadLocalTask(ctx, in)
	if err != nil {
		t.Fatalf("idempotent create after stage advance: %v", err)
	}
	if got.Stage != 2 {
		t.Fatalf("stage=%d want persisted 2", got.Stage)
	}
}

func TestRecoverLocalTasksIncludesDurablePending(t *testing.T) {
	db := openLocalTestDB(t)
	ctx := context.Background()
	for _, in := range []LocalTaskCreate{{TaskID: "pending-good", MachineID: "m", Source: "local", Type: "scan", EnvelopeDigest: "g", Envelope: []byte("good")}, {TaskID: "pending-legacy", MachineID: "m", Source: "local", Type: "scan", EnvelopeDigest: "l", Envelope: []byte("legacy")}} {
		if _, err := db.CreateOrLoadLocalTask(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.db.Exec(`UPDATE local_tasks SET envelope=X'' WHERE task_id='pending-legacy'`); err != nil {
		t.Fatal(err)
	}
	got, err := db.RecoverLocalTasks(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].TaskID != "pending-good" || got[0].Status != "waiting_recovery" || got[1].TaskID != "pending-legacy" {
		t.Fatalf("recovered=%#v", got)
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
	if _, err := db.db.Exec(`UPDATE local_tasks SET created_at=100 WHERE task_id IN ('task-a','task-b','task-c')`); err != nil {
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
	if len(page) != 2 || page[0].TaskID != "task-c" || page[1].TaskID != "task-b" {
		t.Fatalf("first page = %#v", page)
	}
	if err := db.CancelLocalTask(ctx, "machine-a", "task-a"); err != nil {
		t.Fatal(err)
	}
	if err := db.CancelLocalTask(ctx, "machine-a", "task-a"); err != nil {
		t.Fatalf("idempotent CancelLocalTask: %v", err)
	}
	cancelled, err := db.LoadLocalTask(ctx, "machine-a", "task-a")
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != "cancelled" {
		t.Fatalf("cancel status=%q, want cancelled", cancelled.Status)
	}
	retried, err := db.RetryLocalTask(ctx, "machine-a", "task-a")
	if err != nil {
		t.Fatal(err)
	}
	if retried.TaskID != "task-a" || retried.Status != "pending" || retried.Stage != 1 || string(retried.Envelope) != "task-a" {
		t.Fatalf("retried = %#v", retried)
	}
}

// Break caught: the legacy Service expects CancelLocalTask to return only after
// the durable row reaches cancelled, so stopping must be an internal legal edge.
func TestLocalTaskLifecycleLegacyCancelConvergesAlongLegalEdges(t *testing.T) {
	db := openLocalTestDB(t)
	ctx := context.Background()
	for _, test := range []struct {
		status       string
		wantRevision int64
	}{
		{status: "pending", wantRevision: 3},
		{status: "running", wantRevision: 3},
		{status: "waiting_recovery", wantRevision: 3},
		{status: "pausing", wantRevision: 3},
		{status: "stopping", wantRevision: 2},
		{status: "paused", wantRevision: 2},
		{status: "cancelled", wantRevision: 1},
	} {
		t.Run(test.status, func(t *testing.T) {
			task := createVersionedLocalTask(t, db, "legacy-cancel-"+test.status)
			if _, err := db.db.Exec(`UPDATE local_tasks SET status=?1 WHERE task_id=?2`, test.status, task.TaskID); err != nil {
				t.Fatal(err)
			}
			if err := db.CancelLocalTask(ctx, "machine-1", task.TaskID); err != nil {
				t.Fatal(err)
			}
			cancelled, err := db.LoadLocalTask(ctx, "machine-1", task.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			if cancelled.Status != "cancelled" || cancelled.Revision != test.wantRevision {
				t.Fatalf("cancelled=(%q,%d), want=(cancelled,%d)", cancelled.Status, cancelled.Revision, test.wantRevision)
			}
		})
	}
}

// Break caught: treating a legacy retry of pending as success makes two
// serialized Service retries both report success even though only one launches.
func TestLocalTaskLifecycleLegacyRetryRejectsAlreadyPending(t *testing.T) {
	db := openLocalTestDB(t)
	task := createVersionedLocalTask(t, db, "legacy-retry-pending")
	if _, err := db.RetryLocalTask(context.Background(), "machine-1", task.TaskID); !errors.Is(err, ErrLocalTaskTransition) {
		t.Fatalf("RetryLocalTask error=%v, want %v", err, ErrLocalTaskTransition)
	}
	assertLocalTaskSnapshot(t, db, task)
}

// Break caught: omitting any explicitly allowed edge makes a valid lifecycle
// request fail, while accepting an unlisted edge reopens stable terminal work.
func TestLocalTaskLifecycleAllowsOnlyDeclaredEdgesAndBumpsRevisionOnce(t *testing.T) {
	db := openLocalTestDB(t)
	ctx := context.Background()
	edges := map[string][]string{
		"pending":          {"running", "pausing", "stopping", "deleting", "waiting_recovery"},
		"running":          {"pausing", "stopping", "deleting", "succeeded", "failed", "waiting_recovery"},
		"waiting_recovery": {"running", "pausing", "stopping", "deleting", "failed"},
		"pausing":          {"paused", "stopping", "deleting", "failed", "waiting_recovery"},
		"paused":           {"pending", "cancelled", "deleting"},
		"stopping":         {"cancelled", "deleting", "failed", "waiting_recovery"},
		"failed":           {"pending", "deleting"},
		"cancelled":        {"pending", "deleting"},
		"succeeded":        {"deleting"},
		"deleting":         {"delete_failed"},
		"delete_failed":    {"deleting"},
	}
	index := 0
	for from, targets := range edges {
		for _, to := range targets {
			index++
			t.Run(from+"_to_"+to, func(t *testing.T) {
				id := fmt.Sprintf("edge-%03d", index)
				task := createVersionedLocalTask(t, db, id)
				if _, err := db.db.Exec(`UPDATE local_tasks SET status=?1,revision=10 WHERE task_id=?2`, from, id); err != nil {
					t.Fatal(err)
				}
				task, err := db.LoadLocalTask(ctx, "machine-1", task.TaskID)
				if err != nil {
					t.Fatal(err)
				}
				updated, err := db.TransitionLocalTaskLifecycle(ctx, "machine-1", controlFor(task), to, nil, nil)
				if err != nil {
					t.Fatalf("%s -> %s: %v", from, to, err)
				}
				if updated.Status != to || updated.Revision != 11 {
					t.Fatalf("updated=(status=%q revision=%d), want (%q,11)", updated.Status, updated.Revision, to)
				}
			})
		}
	}

	for _, test := range []struct{ from, to string }{
		{from: "succeeded", to: "running"},
		{from: "cancelled", to: "succeeded"},
		{from: "failed", to: "running"},
		{from: "delete_failed", to: "pending"},
		{from: "deleting", to: "pending"},
	} {
		t.Run("reject_"+test.from+"_to_"+test.to, func(t *testing.T) {
			id := "reject-" + test.from
			task := createVersionedLocalTask(t, db, id)
			if _, err := db.db.Exec(`UPDATE local_tasks SET status=?1,revision=20 WHERE task_id=?2`, test.from, id); err != nil {
				t.Fatal(err)
			}
			task, err := db.LoadLocalTask(ctx, "machine-1", task.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.TransitionLocalTaskLifecycle(ctx, "machine-1", controlFor(task), test.to, nil, nil); !errors.Is(err, ErrLocalTaskTransition) {
				t.Fatalf("error=%v, want %v", err, ErrLocalTaskTransition)
			}
			unchanged, err := db.LoadLocalTask(ctx, "machine-1", task.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			if unchanged.Status != test.from || unchanged.Revision != 20 {
				t.Fatalf("rejected transition mutated task: %#v", unchanged)
			}
		})
	}
}

// Break caught: checking only task_id lets a late request mutate a replacement
// incarnation, and checking only instance_id lets stale lifecycle commands win.
func TestLocalTaskStaleAndInstanceMismatchDoNotMutate(t *testing.T) {
	db := openLocalTestDB(t)
	ctx := context.Background()
	task := createVersionedLocalTask(t, db, "cas-task")

	wrongInstance := controlFor(task)
	wrongInstance.InstanceID = "replacement-instance"
	wrongInstance.ExpectedRevision++
	if _, err := db.TransitionLocalTaskLifecycle(ctx, "machine-1", wrongInstance, "not-a-state", nil, nil); !errors.Is(err, ErrLocalTaskInstanceMismatch) {
		t.Fatalf("instance mismatch error=%v, want %v", err, ErrLocalTaskInstanceMismatch)
	}
	assertLocalTaskSnapshot(t, db, task)

	stale := controlFor(task)
	stale.ExpectedRevision++
	if _, err := db.TransitionLocalTaskLifecycle(ctx, "machine-1", stale, "not-a-state", nil, nil); !errors.Is(err, ErrLocalTaskStale) {
		t.Fatalf("stale lifecycle error=%v, want %v", err, ErrLocalTaskStale)
	}
	assertLocalTaskSnapshot(t, db, task)

	if _, err := db.UpdateLocalTaskProgress(ctx, "machine-1", stale, LocalTaskProgressUpdate{
		Phase: "not-a-phase", Stage: -1, ProgressComplete: -1, ProgressTotal: -1, StatsJSON: "{}",
	}); !errors.Is(err, ErrLocalTaskStale) {
		t.Fatalf("stale plus invalid progress error=%v, want %v", err, ErrLocalTaskStale)
	}
	assertLocalTaskSnapshot(t, db, task)

	if _, err := db.UpdateLocalTaskProgress(ctx, "machine-1", wrongInstance, LocalTaskProgressUpdate{
		Phase: "not-a-phase", Stage: -1, ProgressComplete: -1, ProgressTotal: -1, StatsJSON: "{}",
	}); !errors.Is(err, ErrLocalTaskInstanceMismatch) {
		t.Fatalf("instance mismatch plus invalid progress error=%v, want %v", err, ErrLocalTaskInstanceMismatch)
	}
	assertLocalTaskSnapshot(t, db, task)
}

// Break caught: progress persistence used to share lifecycle writes, causing
// polling updates to invalidate the worker's otherwise-current revision.
func TestLocalTaskProgressAdvancesPhaseWithoutBumpingRevision(t *testing.T) {
	db := openLocalTestDB(t)
	ctx := context.Background()
	task := createVersionedLocalTask(t, db, "phase-advance")
	updated, err := db.UpdateLocalTaskProgress(ctx, "machine-1", controlFor(task), LocalTaskProgressUpdate{
		Phase: "stage2", Stage: 2, ProgressComplete: 0,
		ProgressTotal: 12, ProgressTotalKnown: true, StatsJSON: "{}",
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != task.Revision {
		t.Fatalf("revision=%d, want %d", updated.Revision, task.Revision)
	}
	if updated.Phase != "stage2" || updated.Stage != 2 || updated.ProgressTotal != 12 || !updated.ProgressTotalKnown {
		t.Fatalf("updated progress=%#v", updated)
	}
}

// Break caught: allowing counters, totals, or total-known state to roll back in
// one phase makes progress regress; forbidding resets across phases blocks work.
func TestLocalTaskProgressEnforcesMonotonicPhaseRules(t *testing.T) {
	db := openLocalTestDB(t)
	ctx := context.Background()
	task := createVersionedLocalTask(t, db, "progress-rules")

	updates := []LocalTaskProgressUpdate{
		{Phase: "waiting", Stage: 0, ProgressComplete: 1, ProgressTotal: 2, ProgressTotalKnown: false, StatsJSON: `{"step":1}`},
		{Phase: "waiting", Stage: 0, ProgressComplete: 2, ProgressTotal: 4, ProgressTotalKnown: false, StatsJSON: `{"step":2}`},
		{Phase: "waiting", Stage: 0, ProgressComplete: 3, ProgressTotal: 5, ProgressTotalKnown: true, StatsJSON: `{"step":3}`},
	}
	for _, update := range updates {
		var err error
		task, err = db.UpdateLocalTaskProgress(ctx, "machine-1", controlFor(task), update)
		if err != nil {
			t.Fatalf("update %#v: %v", update, err)
		}
	}
	if task.Revision != 1 || !task.ProgressTotalKnown || task.ProgressTotal != 5 {
		t.Fatalf("progress snapshot=%#v", task)
	}
	task, err := db.UpdateLocalTaskProgress(ctx, "machine-1", controlFor(task), LocalTaskProgressUpdate{
		Phase: "waiting", Stage: 0, ProgressComplete: 4, ProgressTotal: 6,
		ProgressTotalKnown: true, StatsJSON: `{"step":4}`,
	})
	if err != nil {
		t.Fatalf("known total increase: %v", err)
	}

	for name, update := range map[string]LocalTaskProgressUpdate{
		"completed rollback": {Phase: "waiting", Stage: 0, ProgressComplete: 3, ProgressTotal: 6, ProgressTotalKnown: true, StatsJSON: "{}"},
		"total rollback":     {Phase: "waiting", Stage: 0, ProgressComplete: 4, ProgressTotal: 5, ProgressTotalKnown: true, StatsJSON: "{}"},
		"known rollback":     {Phase: "waiting", Stage: 0, ProgressComplete: 4, ProgressTotal: 6, ProgressTotalKnown: false, StatsJSON: "{}"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := db.UpdateLocalTaskProgress(ctx, "machine-1", controlFor(task), update); !errors.Is(err, ErrLocalTaskProgressRollback) {
				t.Fatalf("error=%v, want %v", err, ErrLocalTaskProgressRollback)
			}
		})
	}

	advanced, err := db.UpdateLocalTaskProgress(ctx, "machine-1", controlFor(task), LocalTaskProgressUpdate{
		Phase: "scan", Stage: 0, ProgressComplete: 0, ProgressTotal: 0,
		ProgressTotalKnown: false, StatsJSON: `{"phase":"scan"}`,
	})
	if err != nil {
		t.Fatalf("phase advance reset: %v", err)
	}
	if advanced.ProgressTotalKnown || advanced.ProgressTotal != 0 || advanced.ProgressComplete != 0 {
		t.Fatalf("phase reset=%#v", advanced)
	}
	if _, err := db.UpdateLocalTaskProgress(ctx, "machine-1", controlFor(advanced), LocalTaskProgressUpdate{
		Phase: "waiting", Stage: 0, ProgressComplete: 0, ProgressTotal: 0,
		ProgressTotalKnown: false, StatsJSON: "{}",
	}); !errors.Is(err, ErrLocalTaskProgressRollback) {
		t.Fatalf("phase rollback error=%v, want %v", err, ErrLocalTaskProgressRollback)
	}
}

// Break caught: ordering only by task_id ignores creation time and makes the
// newest-task page unstable when timestamps tie.
func TestLocalTaskListOrdersNewestThenTaskIDDescending(t *testing.T) {
	db := openLocalTestDB(t)
	ctx := context.Background()
	for _, id := range []string{"old-z", "new-a", "new-b"} {
		createVersionedLocalTask(t, db, id)
	}
	if _, err := db.db.Exec(`UPDATE local_tasks SET created_at=CASE WHEN task_id='old-z' THEN 100 ELSE 200 END`); err != nil {
		t.Fatal(err)
	}
	tasks, err := db.ListLocalTasks(ctx, "machine-1", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(tasks))
	for i := range tasks {
		got[i] = tasks[i].TaskID
	}
	want := []string{"new-b", "new-a", "old-z"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("task order=%v, want %v", got, want)
	}
}

// Break caught: restart recovery changed statuses without versioning them, so
// in-flight pre-restart commands remained current and could overwrite recovery.
func TestLocalTaskLifecycleRecoveryVersionsEveryChangedRowOnce(t *testing.T) {
	db := openLocalTestDB(t)
	ctx := context.Background()
	statuses := []string{"pending", "running", "pausing", "stopping", "paused", "deleting", "succeeded"}
	for _, status := range statuses {
		task := createVersionedLocalTask(t, db, "recover-"+status)
		if _, err := db.db.Exec(`UPDATE local_tasks SET status=?1,revision=7 WHERE task_id=?2`, status, task.TaskID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.RecoverLocalTasks(ctx, "machine-1"); err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		status   string
		revision int64
	}{
		"pending": {"waiting_recovery", 8}, "running": {"waiting_recovery", 8},
		"pausing": {"paused", 8}, "stopping": {"cancelled", 8},
		"paused": {"paused", 7}, "deleting": {"deleting", 7}, "succeeded": {"succeeded", 7},
	}
	for from, expected := range want {
		task, err := db.LoadLocalTask(ctx, "machine-1", "recover-"+from)
		if err != nil {
			t.Fatal(err)
		}
		if task.Status != expected.status || task.Revision != expected.revision {
			t.Fatalf("%s recovered=(%q,%d), want=(%q,%d)", from, task.Status, task.Revision, expected.status, expected.revision)
		}
	}
}

func createVersionedLocalTask(t *testing.T, db *DB, taskID string) LocalTask {
	t.Helper()
	task, err := db.CreateOrLoadLocalTask(context.Background(), LocalTaskCreate{
		TaskID: taskID, MachineID: "machine-1", Source: "local", Type: "analysis",
		EnvelopeDigest: "digest-" + taskID, Envelope: []byte("envelope-" + taskID),
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func controlFor(task LocalTask) LocalTaskControl {
	return LocalTaskControl{TaskID: task.TaskID, InstanceID: task.InstanceID, ExpectedRevision: task.Revision}
}

func assertLocalTaskSnapshot(t *testing.T, db *DB, want LocalTask) {
	t.Helper()
	got, err := db.LoadLocalTask(context.Background(), want.MachineID, want.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("task mutated\ngot:  %#v\nwant: %#v", got, want)
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
