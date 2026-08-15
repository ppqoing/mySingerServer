package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"

	_ "modernc.org/sqlite"
)

type DB struct {
	db              *sql.DB
	syncTableCursor atomic.Uint32
}

func Open(path string) (*DB, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)",
		filepath.ToSlash(path),
	)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(1)
	if _, err := sqlDB.Exec(ddl); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	if err := migrateVideoFeaturePresence(sqlDB); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("store: migrate video features: %w", err)
	}
	if err := migrateSyncQueueGeneration(sqlDB); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("store: migrate sync queue: %w", err)
	}
	if err := migrateLocalTaskEnvelope(sqlDB); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("store: migrate local task envelope: %w", err)
	}
	if err := migrateLocalTaskLifecycle(sqlDB); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("store: migrate local task lifecycle: %w", err)
	}
	if err := verifyLocalForeignKeys(sqlDB); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("store: verify local foreign keys: %w", err)
	}
	if _, err := sqlDB.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, localSchemaVersion)); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("store: set schema version: %w", err)
	}
	return &DB{db: sqlDB}, nil
}

func migrateLocalTaskEnvelope(db *sql.DB) error {
	var exists int
	err := db.QueryRow(`SELECT 1 FROM pragma_table_info('local_tasks') WHERE name='envelope'`).Scan(&exists)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	_, err = db.Exec(`ALTER TABLE local_tasks ADD COLUMN envelope BLOB NOT NULL DEFAULT X''`)
	return err
}

func migrateLocalTaskLifecycle(db *sql.DB) error {
	complete, err := localTaskLifecycleSchemaComplete(db)
	if err != nil {
		return err
	}
	if complete {
		return verifyLocalForeignKeys(db)
	}
	receiptsComplete, err := localTaskDeletionReceiptsSchemaComplete(db)
	if err != nil {
		return err
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	foreignKeysDisabled := true
	defer func() {
		if foreignKeysDisabled {
			_, _ = db.Exec(`PRAGMA foreign_keys = ON`)
		}
	}()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
		CREATE TABLE local_tasks_v4 (
			task_id TEXT PRIMARY KEY,
			instance_id TEXT NOT NULL,
			revision INTEGER NOT NULL DEFAULT 1 CHECK (revision > 0),
			machine_id TEXT NOT NULL,
			source TEXT NOT NULL CHECK (source IN ('local','manager')),
			type TEXT NOT NULL CHECK (type IN ('scan','analysis','stage2','stage3','delete')),
			stage INTEGER NOT NULL CHECK (stage IN (0,1,2,3)),
			status TEXT NOT NULL CHECK (status IN ('pending','running','waiting_recovery','pausing','paused','stopping','cancelled','succeeded','failed','deleting','delete_failed')),
			phase TEXT NOT NULL DEFAULT 'waiting' CHECK (phase IN ('waiting','scan','stage1','stage2','stage3','finalizing')),
			envelope_digest TEXT NOT NULL,
			envelope BLOB NOT NULL DEFAULT X'',
			progress_completed INTEGER NOT NULL DEFAULT 0 CHECK (progress_completed >= 0),
			progress_total INTEGER NOT NULL DEFAULT 0 CHECK (progress_total >= 0),
			progress_total_known INTEGER NOT NULL DEFAULT 0 CHECK (progress_total_known IN (0,1)),
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
		INSERT INTO local_tasks_v4
			(task_id,instance_id,revision,machine_id,source,type,stage,status,phase,envelope_digest,envelope,progress_completed,progress_total,progress_total_known,stats_json,safe_error_code,safe_error_message,created_at,updated_at,started_at,completed_at)
		SELECT task_id,lower(hex(randomblob(16))),1,machine_id,source,type,stage,status,
			CASE
				WHEN stage=0 AND status='pending' THEN 'waiting'
				WHEN stage=0 THEN 'scan'
				WHEN stage=1 THEN 'stage1'
				WHEN stage=2 THEN 'stage3'
				WHEN stage=3 THEN 'finalizing'
				ELSE 'scan'
			END,
			envelope_digest,envelope,progress_completed,progress_total,
			CASE WHEN progress_total > 0 THEN 1 ELSE 0 END,
			stats_json,safe_error_code,safe_error_message,created_at,updated_at,started_at,completed_at
		FROM local_tasks;
		DROP TABLE local_tasks;
		ALTER TABLE local_tasks_v4 RENAME TO local_tasks;
		CREATE INDEX idx_local_tasks_machine_status
			ON local_tasks (machine_id, status, created_at, task_id);`); err != nil {
		return err
	}
	if !receiptsComplete {
		if _, err := tx.Exec(`
			CREATE TABLE local_task_deletion_receipts_v4 (
				machine_id TEXT NOT NULL,
				task_id TEXT NOT NULL,
				instance_id TEXT NOT NULL,
				deleted_at INTEGER NOT NULL,
				PRIMARY KEY (machine_id, task_id, instance_id)
			);
			INSERT INTO local_task_deletion_receipts_v4
				(machine_id,task_id,instance_id,deleted_at)
			SELECT machine_id,task_id,instance_id,deleted_at
			FROM local_task_deletion_receipts;
			DROP TABLE local_task_deletion_receipts;
			ALTER TABLE local_task_deletion_receipts_v4 RENAME TO local_task_deletion_receipts;`); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return err
	}
	foreignKeysDisabled = false
	return verifyLocalForeignKeys(db)
}

func localTaskLifecycleSchemaComplete(db *sql.DB) (bool, error) {
	var sqlText string
	err := db.QueryRow(`SELECT sql FROM sqlite_schema WHERE type='table' AND name='local_tasks'`).Scan(&sqlText)
	if err != nil {
		return false, err
	}
	normalized := strings.ToLower(strings.Join(strings.Fields(sqlText), " "))
	for _, requirement := range []string{
		"instance_id text not null",
		"revision integer not null default 1 check (revision > 0)",
		"status text not null check (status in ('pending','running','waiting_recovery','pausing','paused','stopping','cancelled','succeeded','failed','deleting','delete_failed'))",
		"phase text not null default 'waiting' check (phase in ('waiting','scan','stage1','stage2','stage3','finalizing'))",
		"progress_total_known integer not null default 0 check (progress_total_known in (0,1))",
	} {
		if !strings.Contains(normalized, requirement) {
			return false, nil
		}
	}
	return localTaskDeletionReceiptsSchemaComplete(db)
}

func localTaskDeletionReceiptsSchemaComplete(db *sql.DB) (bool, error) {
	rows, err := db.Query(`PRAGMA table_info('local_task_deletion_receipts')`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	primaryKey := make([]string, 3)
	for rows.Next() {
		var cid, notNull, position int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &position); err != nil {
			return false, err
		}
		if position > 0 && position <= len(primaryKey) {
			primaryKey[position-1] = name
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return primaryKey[0] == "machine_id" && primaryKey[1] == "task_id" && primaryKey[2] == "instance_id", nil
}

func verifyLocalForeignKeys(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		var table string
		var rowID, parent, foreignKeyID any
		if err := rows.Scan(&table, &rowID, &parent, &foreignKeyID); err != nil {
			return err
		}
		return fmt.Errorf("foreign key violation in %s", table)
	}
	return rows.Err()
}

func migrateVideoFeaturePresence(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info('video_features');`)
	if err != nil {
		return err
	}
	defer rows.Close()
	needsRebuild := false
	hasThumbWidth := false
	hasThumbHeight := false
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull int
		var defaultValue any
		var primaryKey int
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		if (name == "duration_ms" || name == "thumb_quality") && notNull != 0 {
			needsRebuild = true
		}
		switch name {
		case "thumb_width":
			hasThumbWidth = true
		case "thumb_height":
			hasThumbHeight = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !needsRebuild {
		if !hasThumbWidth {
			if _, err := db.Exec(`ALTER TABLE video_features ADD COLUMN thumb_width INTEGER;`); err != nil {
				return err
			}
		}
		if !hasThumbHeight {
			if _, err := db.Exec(`ALTER TABLE video_features ADD COLUMN thumb_height INTEGER;`); err != nil {
				return err
			}
		}
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
		CREATE TABLE video_features_phase1 (
			sha512 TEXT PRIMARY KEY,
			duration_ms INTEGER,
			thumb_path TEXT,
			thumb_pdq256 BLOB,
			thumb_quality INTEGER,
			thumb_width INTEGER,
			thumb_height INTEGER
		);
		INSERT INTO video_features_phase1
			(sha512, duration_ms, thumb_path, thumb_pdq256, thumb_quality)
		SELECT sha512, duration_ms, thumb_path, thumb_pdq256, thumb_quality
		FROM video_features;
		DROP TABLE video_features;
		ALTER TABLE video_features_phase1 RENAME TO video_features;`); err != nil {
		return err
	}
	return tx.Commit()
}

func migrateSyncQueueGeneration(db *sql.DB) error {
	var exists int
	err := db.QueryRow(`
		SELECT 1
		FROM pragma_table_info('sync_queue')
		WHERE name = 'generation';`,
	).Scan(&exists)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	_, err = db.Exec(`
		ALTER TABLE sync_queue
		ADD COLUMN generation INTEGER NOT NULL DEFAULT 1;`,
	)
	return err
}

func (d *DB) Close() error {
	return d.db.Close()
}
