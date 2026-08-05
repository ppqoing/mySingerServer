package store

const ddl = `
CREATE TABLE IF NOT EXISTS files (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    machine_id   TEXT    NOT NULL,
    disk_no      INTEGER NOT NULL DEFAULT -1,
    path         TEXT    NOT NULL,
    size         INTEGER NOT NULL DEFAULT -1,
    mtime        INTEGER NOT NULL DEFAULT 0,
    sha512       TEXT,
    phase1_done  INTEGER NOT NULL DEFAULT 0,
    phase2_done  INTEGER NOT NULL DEFAULT 0,
    status       TEXT    NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending','done','partial','failed','crash','deleted')),
    missing_mask INTEGER NOT NULL DEFAULT 0,
    error        TEXT,
    updated_at   INTEGER NOT NULL DEFAULT 0,
    UNIQUE (machine_id, path)
);
CREATE INDEX IF NOT EXISTS idx_files_sha512      ON files (sha512);
CREATE INDEX IF NOT EXISTS idx_files_status      ON files (status);
CREATE INDEX IF NOT EXISTS idx_files_disk_status ON files (disk_no, status);

CREATE TABLE IF NOT EXISTS image_features (
    sha512       TEXT PRIMARY KEY,
    width        INTEGER NOT NULL DEFAULT 0,
    height       INTEGER NOT NULL DEFAULT 0,
    pdq256       BLOB,
    pdq_quality  INTEGER NOT NULL DEFAULT 0,
    phash_parts  BLOB,
    sobel_hist   BLOB
);

CREATE TABLE IF NOT EXISTS video_features (
    sha512        TEXT PRIMARY KEY,
    duration_ms   INTEGER,
    thumb_path    TEXT,
    thumb_pdq256  BLOB,
    thumb_quality INTEGER,
    thumb_width   INTEGER,
    thumb_height  INTEGER
);

CREATE TABLE IF NOT EXISTS video_frames (
    sha512      TEXT    NOT NULL,
    frame_idx   INTEGER NOT NULL,
    pdq256      BLOB,
    phash_parts BLOB,
    sobel_hist  BLOB,
    PRIMARY KEY (sha512, frame_idx)
);

CREATE TABLE IF NOT EXISTS sync_queue (
    table_name  TEXT    NOT NULL,
    row_pk      TEXT    NOT NULL,
    synced      INTEGER NOT NULL DEFAULT 0,
    enqueued_at INTEGER NOT NULL DEFAULT 0,
    generation  INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (table_name, row_pk)
);
CREATE INDEX IF NOT EXISTS idx_sync_queue_pending ON sync_queue (synced);

PRAGMA user_version = 3;
`
