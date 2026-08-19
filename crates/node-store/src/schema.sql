-- mySingerServer Rust V2 节点数据库。只在空数据库一次创建，不兼容旧表结构。
PRAGMA user_version = 1;

CREATE TABLE metadata (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;

CREATE TABLE contents (
    content_id INTEGER PRIMARY KEY,
    md5        BLOB NOT NULL CHECK(length(md5) = 16),
    file_size  INTEGER NOT NULL CHECK(file_size >= 0),
    media_kind TEXT NOT NULL CHECK(media_kind IN ('image', 'video', 'other')),
    UNIQUE(md5, file_size)
) STRICT;
CREATE INDEX contents_md5_idx ON contents(md5);

CREATE TABLE files (
    machine_id     TEXT NOT NULL,
    normalized_path TEXT NOT NULL,
    display_path   TEXT NOT NULL,
    file_size      INTEGER NOT NULL CHECK(file_size >= 0),
    content_id     INTEGER NOT NULL REFERENCES contents(content_id),
    active         INTEGER NOT NULL DEFAULT 1 CHECK(active IN (0, 1)),
    PRIMARY KEY(machine_id, normalized_path)
) STRICT;
CREATE INDEX files_content_idx ON files(content_id, active);

-- 一筛允许保存部分结果；完整性只在查询边界集中判断。
CREATE TABLE image_stage1 (
    content_id INTEGER PRIMARY KEY REFERENCES contents(content_id) ON DELETE CASCADE,
    width      INTEGER,
    height     INTEGER,
    pdq        BLOB CHECK(pdq IS NULL OR length(pdq) = 32),
    quality    INTEGER CHECK(quality IS NULL OR quality BETWEEN 0 AND 100)
) STRICT;

CREATE TABLE image_stage2 (
    content_id  INTEGER PRIMARY KEY REFERENCES contents(content_id) ON DELETE CASCADE,
    phash_parts BLOB CHECK(phash_parts IS NULL OR length(phash_parts) = 72),
    sobel       BLOB CHECK(sobel IS NULL OR length(sobel) = 512)
) STRICT;

CREATE TABLE video_metadata (
    content_id  INTEGER PRIMARY KEY REFERENCES contents(content_id) ON DELETE CASCADE,
    duration_ms INTEGER,
    width       INTEGER,
    height      INTEGER
) STRICT;

CREATE TABLE video_frame_stage1 (
    content_id INTEGER NOT NULL REFERENCES contents(content_id) ON DELETE CASCADE,
    slot       INTEGER NOT NULL CHECK(slot BETWEEN 0 AND 5),
    time_ms    INTEGER NOT NULL CHECK(time_ms >= 0),
    decoded    INTEGER NOT NULL CHECK(decoded IN (0, 1)),
    width      INTEGER,
    height     INTEGER,
    pdq        BLOB CHECK(pdq IS NULL OR length(pdq) = 32),
    quality    INTEGER CHECK(quality IS NULL OR quality BETWEEN 0 AND 100),
    PRIMARY KEY(content_id, slot)
) STRICT;

CREATE TABLE video_frame_stage2 (
    content_id  INTEGER NOT NULL REFERENCES contents(content_id) ON DELETE CASCADE,
    slot        INTEGER NOT NULL CHECK(slot BETWEEN 0 AND 5),
    phash_parts BLOB CHECK(phash_parts IS NULL OR length(phash_parts) = 72),
    sobel       BLOB CHECK(sobel IS NULL OR length(sobel) = 512),
    PRIMARY KEY(content_id, slot)
) STRICT;

CREATE TABLE contact_sheets (
    content_id    INTEGER PRIMARY KEY REFERENCES contents(content_id) ON DELETE CASCADE,
    relative_path TEXT NOT NULL
) STRICT;

CREATE TABLE tasks (
    task_id       TEXT PRIMARY KEY,
    kind          TEXT NOT NULL,
    status        TEXT NOT NULL CHECK(status IN ('queued','running','completed','failed','cancelled')),
    event_seq     INTEGER NOT NULL DEFAULT 0,
    total_items   INTEGER NOT NULL DEFAULT 0,
    succeeded     INTEGER NOT NULL DEFAULT 0,
    failed_items  INTEGER NOT NULL DEFAULT 0,
    cancelled     INTEGER NOT NULL DEFAULT 0,
    created_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL
) STRICT;

CREATE TABLE task_items (
    item_id         TEXT PRIMARY KEY,
    task_id         TEXT NOT NULL REFERENCES tasks(task_id) ON DELETE CASCADE,
    machine_id      TEXT,
    normalized_path TEXT,
    display_path    TEXT,
    file_size       INTEGER,
    content_id      INTEGER REFERENCES contents(content_id),
    status          TEXT NOT NULL CHECK(status IN ('queued','running','succeeded','failed','cancelled')),
    stage           TEXT,
    error           TEXT
) STRICT;
CREATE INDEX task_items_claim_idx ON task_items(task_id, status, item_id);

CREATE TABLE task_scan_roots (
    task_id         TEXT NOT NULL REFERENCES tasks(task_id) ON DELETE CASCADE,
    normalized_root TEXT NOT NULL,
    PRIMARY KEY(task_id, normalized_root)
) STRICT;

CREATE TABLE analysis_runs (
    analysis_run_id TEXT PRIMARY KEY,
    mode            TEXT NOT NULL CHECK(mode IN ('local','central')),
    status          TEXT NOT NULL,
    thresholds_toml TEXT NOT NULL,
    inputs_frozen   INTEGER NOT NULL DEFAULT 0 CHECK(inputs_frozen IN (0,1)),
    skipped_incomplete INTEGER NOT NULL DEFAULT 0,
    created_at_ms   INTEGER NOT NULL,
    updated_at_ms   INTEGER NOT NULL
) STRICT;

CREATE TABLE analysis_run_inputs (
    analysis_run_id TEXT NOT NULL REFERENCES analysis_runs(analysis_run_id) ON DELETE CASCADE,
    md5             BLOB NOT NULL CHECK(length(md5) = 16),
    file_size       INTEGER NOT NULL,
    machine_id      TEXT NOT NULL,
    normalized_path TEXT NOT NULL,
    PRIMARY KEY(analysis_run_id, md5, file_size, machine_id, normalized_path)
) STRICT;

CREATE TABLE candidate_pairs (
    analysis_run_id   TEXT NOT NULL REFERENCES analysis_runs(analysis_run_id) ON DELETE CASCADE,
    pair_kind         TEXT NOT NULL CHECK(pair_kind IN ('image','video')),
    left_md5          BLOB NOT NULL CHECK(length(left_md5) = 16),
    left_size         INTEGER NOT NULL,
    right_md5         BLOB NOT NULL CHECK(length(right_md5) = 16),
    right_size        INTEGER NOT NULL,
    stage1_score      REAL NOT NULL,
    phash_passed_parts INTEGER,
    stage2_score      REAL,
    status            TEXT NOT NULL,
    PRIMARY KEY(analysis_run_id, pair_kind, left_md5, left_size, right_md5, right_size),
    CHECK(left_md5 < right_md5 OR (left_md5 = right_md5 AND left_size < right_size))
) STRICT;

CREATE TABLE duplicate_groups (
    analysis_run_id  TEXT NOT NULL REFERENCES analysis_runs(analysis_run_id) ON DELETE CASCADE,
    group_id         TEXT NOT NULL,
    group_kind       TEXT NOT NULL CHECK(group_kind IN ('exact','image','video')),
    representative_md5  BLOB NOT NULL CHECK(length(representative_md5) = 16),
    representative_size INTEGER NOT NULL,
    PRIMARY KEY(analysis_run_id, group_id)
) STRICT;

CREATE TABLE group_members (
    analysis_run_id TEXT NOT NULL,
    group_id        TEXT NOT NULL,
    machine_id      TEXT NOT NULL,
    normalized_path TEXT NOT NULL,
    md5             BLOB NOT NULL CHECK(length(md5) = 16),
    file_size       INTEGER NOT NULL,
    representative  INTEGER NOT NULL CHECK(representative IN (0,1)),
    stage1_score    REAL NOT NULL DEFAULT 1,
    phash_passed_parts INTEGER,
    stage2_score    REAL,
    active          INTEGER NOT NULL DEFAULT 1 CHECK(active IN (0,1)),
    PRIMARY KEY(analysis_run_id, group_id, machine_id, normalized_path),
    FOREIGN KEY(analysis_run_id, group_id)
        REFERENCES duplicate_groups(analysis_run_id, group_id) ON DELETE CASCADE
) STRICT;

CREATE TABLE review_marks (
    analysis_run_id TEXT NOT NULL,
    group_id        TEXT NOT NULL,
    machine_id      TEXT NOT NULL,
    normalized_path TEXT NOT NULL,
    decision        TEXT NOT NULL CHECK(decision IN ('undecided','keep','delete')),
    PRIMARY KEY(analysis_run_id, group_id, machine_id, normalized_path),
    FOREIGN KEY(analysis_run_id, group_id, machine_id, normalized_path)
        REFERENCES group_members(analysis_run_id, group_id, machine_id, normalized_path)
        ON DELETE CASCADE
) STRICT;

CREATE TABLE sync_outbox (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    entity_kind TEXT NOT NULL,
    payload     BLOB NOT NULL
) STRICT;

CREATE TABLE sync_state (
    singleton          INTEGER PRIMARY KEY CHECK(singleton = 1),
    acked_seq          INTEGER NOT NULL DEFAULT 0,
    pruned_through_seq INTEGER NOT NULL DEFAULT 0
) STRICT;
INSERT INTO sync_state(singleton, acked_seq, pruned_through_seq) VALUES(1, 0, 0);

CREATE TABLE delete_batches (
    delete_batch_id TEXT PRIMARY KEY,
    analysis_run_id TEXT NOT NULL,
    mode            TEXT NOT NULL CHECK(mode IN ('recycle_bin','permanent')),
    status          TEXT NOT NULL CHECK(status IN ('queued','running','completed','failed','cancelled')),
    created_at_ms   INTEGER NOT NULL
) STRICT;

CREATE TABLE delete_items (
    delete_item_id  TEXT PRIMARY KEY,
    delete_batch_id TEXT NOT NULL REFERENCES delete_batches(delete_batch_id) ON DELETE CASCADE,
    group_id        TEXT NOT NULL,
    machine_id      TEXT NOT NULL,
    normalized_path TEXT NOT NULL,
    expected_md5    BLOB NOT NULL CHECK(length(expected_md5) = 16),
    expected_size   INTEGER NOT NULL,
    status          TEXT NOT NULL CHECK(status IN ('queued','running','recycled','deleted','skipped','failed')),
    message         TEXT
) STRICT;
