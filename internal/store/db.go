package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sync/atomic"

	_ "modernc.org/sqlite"
)

type DB struct {
	db              *sql.DB
	syncTableCursor atomic.Uint32
}

func Open(path string) (*DB, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)",
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
	return &DB{db: sqlDB}, nil
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
