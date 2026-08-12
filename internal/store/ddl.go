package store

const localSchemaVersion = 3

const ddl = `
PRAGMA foreign_keys = ON;

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

CREATE TABLE IF NOT EXISTS local_tasks (
    task_id            TEXT    PRIMARY KEY,
    machine_id         TEXT    NOT NULL,
    source             TEXT    NOT NULL CHECK (source IN ('local','manager')),
    type               TEXT    NOT NULL CHECK (type IN ('scan','analysis','stage2','stage3','delete')),
    stage              INTEGER NOT NULL CHECK (stage IN (0,1,2,3)),
    status             TEXT    NOT NULL CHECK (status IN
                               ('pending','running','waiting_recovery','succeeded','failed','cancelled')),
    envelope_digest    TEXT    NOT NULL,
    progress_completed INTEGER NOT NULL DEFAULT 0 CHECK (progress_completed >= 0),
    progress_total     INTEGER NOT NULL DEFAULT 0 CHECK (progress_total >= 0),
    stats_json         TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(stats_json)),
    safe_error_code    TEXT,
    safe_error_message TEXT,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    started_at         INTEGER,
    completed_at       INTEGER,
    CHECK (progress_total = 0 OR progress_completed <= progress_total)
);
CREATE INDEX IF NOT EXISTS idx_local_tasks_machine_status
    ON local_tasks (machine_id, status, created_at, task_id);

CREATE TABLE IF NOT EXISTS local_analysis_runs (
    run_id       TEXT    PRIMARY KEY,
    machine_id   TEXT    NOT NULL,
    generation   INTEGER NOT NULL CHECK (generation > 0),
    task_id      TEXT    NOT NULL UNIQUE,
    status       TEXT    NOT NULL CHECK (status IN ('building','complete','published','failed')),
    created_at   INTEGER NOT NULL,
    completed_at INTEGER,
    published_at INTEGER,
    UNIQUE (machine_id, generation),
    UNIQUE (machine_id, run_id),
    UNIQUE (run_id, generation),
    FOREIGN KEY (task_id) REFERENCES local_tasks(task_id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_local_analysis_machine_status
    ON local_analysis_runs (machine_id, status, generation);

CREATE TABLE IF NOT EXISTS local_pair_scores (
    pair_id          INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id           TEXT    NOT NULL,
    generation       INTEGER NOT NULL CHECK (generation > 0),
    pair_key         TEXT    NOT NULL,
    left_file_id     INTEGER NOT NULL,
    right_file_id    INTEGER NOT NULL,
    left_sha512      TEXT    NOT NULL,
    right_sha512     TEXT    NOT NULL,
    stage1_json      TEXT    NOT NULL CHECK (json_valid(stage1_json)),
    stage2_json      TEXT    CHECK (stage2_json IS NULL OR json_valid(stage2_json)),
    stage3_json      TEXT    CHECK (stage3_json IS NULL OR json_valid(stage3_json)),
    final_verdict    TEXT    NOT NULL DEFAULT 'undecided'
                           CHECK (final_verdict IN ('undecided','duplicate','not_duplicate','uncertain')),
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL,
    UNIQUE (run_id, pair_key),
    CHECK (left_file_id <> right_file_id),
    FOREIGN KEY (run_id, generation)
        REFERENCES local_analysis_runs(run_id, generation) ON DELETE RESTRICT,
    FOREIGN KEY (left_file_id) REFERENCES files(id) ON DELETE RESTRICT,
    FOREIGN KEY (right_file_id) REFERENCES files(id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_local_pair_scores_run_order
    ON local_pair_scores (run_id, generation, pair_key, pair_id);
CREATE INDEX IF NOT EXISTS idx_local_pair_scores_sha
    ON local_pair_scores (left_sha512, right_sha512);

CREATE TABLE IF NOT EXISTS local_dup_groups (
    group_id    TEXT    PRIMARY KEY,
    run_id      TEXT    NOT NULL,
    generation  INTEGER NOT NULL CHECK (generation > 0),
    category    TEXT    NOT NULL CHECK (category IN ('exact','image','video','uncertain')),
    verdict     TEXT    NOT NULL CHECK (verdict IN ('duplicate','not_duplicate','uncertain')),
    created_at  INTEGER NOT NULL,
    UNIQUE (run_id, group_id),
    FOREIGN KEY (run_id, generation)
        REFERENCES local_analysis_runs(run_id, generation) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_local_dup_groups_run
    ON local_dup_groups (run_id, generation, group_id);

CREATE TABLE IF NOT EXISTS local_dup_members (
    group_id    TEXT    NOT NULL,
    run_id      TEXT    NOT NULL,
    generation  INTEGER NOT NULL CHECK (generation > 0),
    file_id     INTEGER NOT NULL,
    sha512      TEXT    NOT NULL,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (group_id, file_id),
    FOREIGN KEY (run_id, group_id)
        REFERENCES local_dup_groups(run_id, group_id) ON DELETE RESTRICT,
    FOREIGN KEY (run_id, generation)
        REFERENCES local_analysis_runs(run_id, generation) ON DELETE RESTRICT,
    FOREIGN KEY (file_id) REFERENCES files(id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_local_dup_members_run
    ON local_dup_members (run_id, generation, group_id, file_id);

CREATE TABLE IF NOT EXISTS local_current_analysis (
    machine_id   TEXT    PRIMARY KEY,
    run_id       TEXT    NOT NULL UNIQUE,
    generation   INTEGER NOT NULL CHECK (generation > 0),
    published_at INTEGER NOT NULL,
    FOREIGN KEY (machine_id, run_id)
        REFERENCES local_analysis_runs(machine_id, run_id) ON DELETE RESTRICT,
    FOREIGN KEY (run_id, generation)
        REFERENCES local_analysis_runs(run_id, generation) ON DELETE RESTRICT
);

CREATE TABLE IF NOT EXISTS local_reviews (
    review_id  TEXT    PRIMARY KEY,
    machine_id TEXT    NOT NULL,
    run_id     TEXT    NOT NULL,
    generation INTEGER NOT NULL CHECK (generation > 0),
    group_id   TEXT    NOT NULL,
    file_id    INTEGER NOT NULL,
    decision   TEXT    NOT NULL CHECK (decision IN ('keep','delete','undecided')),
    reviewer   TEXT    NOT NULL,
    note       TEXT    NOT NULL DEFAULT '',
    reviewed_at INTEGER NOT NULL,
    UNIQUE (run_id, group_id, file_id),
    FOREIGN KEY (machine_id, run_id)
        REFERENCES local_analysis_runs(machine_id, run_id) ON DELETE RESTRICT,
    FOREIGN KEY (run_id, group_id)
        REFERENCES local_dup_groups(run_id, group_id) ON DELETE RESTRICT,
    FOREIGN KEY (group_id, file_id)
        REFERENCES local_dup_members(group_id, file_id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_local_reviews_run
    ON local_reviews (run_id, group_id, file_id);

CREATE TABLE IF NOT EXISTS local_delete_batches (
    batch_id             TEXT    PRIMARY KEY,
    machine_id           TEXT    NOT NULL,
    run_id               TEXT,
    confirmation_digest  TEXT    NOT NULL,
    status               TEXT    NOT NULL CHECK (status IN
                                 ('pending','running','succeeded','failed','uncertain')),
    requested_count      INTEGER NOT NULL DEFAULT 0 CHECK (requested_count >= 0),
    succeeded_count      INTEGER NOT NULL DEFAULT 0 CHECK (succeeded_count >= 0),
    failed_count         INTEGER NOT NULL DEFAULT 0 CHECK (failed_count >= 0),
    uncertain_count      INTEGER NOT NULL DEFAULT 0 CHECK (uncertain_count >= 0),
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL,
    completed_at         INTEGER,
    FOREIGN KEY (machine_id, run_id)
        REFERENCES local_analysis_runs(machine_id, run_id) ON DELETE RESTRICT,
    CHECK (succeeded_count + failed_count + uncertain_count <= requested_count)
);
CREATE INDEX IF NOT EXISTS idx_local_delete_batches_machine
    ON local_delete_batches (machine_id, created_at, batch_id);

CREATE TABLE IF NOT EXISTS local_delete_items (
    item_id        INTEGER PRIMARY KEY AUTOINCREMENT,
    batch_id       TEXT    NOT NULL,
    file_id        INTEGER NOT NULL,
    path_snapshot  TEXT    NOT NULL,
    sha512         TEXT    NOT NULL,
    result         TEXT    NOT NULL CHECK (result IN ('pending','deleted','failed','uncertain')),
    error_code     TEXT,
    error_message  TEXT,
    uncertain      INTEGER NOT NULL DEFAULT 0 CHECK (uncertain IN (0,1)),
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    completed_at   INTEGER,
    UNIQUE (batch_id, file_id),
    FOREIGN KEY (batch_id) REFERENCES local_delete_batches(batch_id) ON DELETE RESTRICT,
    FOREIGN KEY (file_id) REFERENCES files(id) ON DELETE RESTRICT,
    CHECK ((result = 'uncertain' AND uncertain = 1) OR
           (result <> 'uncertain' AND uncertain = 0))
);
CREATE INDEX IF NOT EXISTS idx_local_delete_items_batch
    ON local_delete_items (batch_id, item_id);

CREATE TABLE IF NOT EXISTS local_outbox (
    sequence      INTEGER PRIMARY KEY AUTOINCREMENT,
    topic         TEXT    NOT NULL,
    entity_key    TEXT    NOT NULL,
    generation    INTEGER NOT NULL CHECK (generation >= 0),
    payload_json  TEXT    NOT NULL CHECK (json_valid(payload_json)),
    ack_at        INTEGER,
    retry_count   INTEGER NOT NULL DEFAULT 0 CHECK (retry_count >= 0),
    next_retry_at INTEGER,
    last_error    TEXT,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    UNIQUE (topic, entity_key, generation)
);
CREATE INDEX IF NOT EXISTS idx_local_outbox_pending
    ON local_outbox (ack_at, next_retry_at, sequence);

PRAGMA user_version = 3;
`
