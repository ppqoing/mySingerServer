-- mySingerServer Rust V2 中心 PostgreSQL schema。
-- 只面向空数据库，由管理员手动执行；应用只校验，绝不运行本文件或隐式迁移。
BEGIN;

CREATE TABLE schema_metadata (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
INSERT INTO schema_metadata(key, value) VALUES
    ('schema_id', 'mysingerserver-rust-v2'),
    ('schema_version', '1');

CREATE TABLE nodes (
    machine_id       CHAR(64) PRIMARY KEY,
    last_seen_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_listen_addr TEXT,
    CHECK(machine_id ~ '^[0-9a-f]{64}$')
);

CREATE TABLE sync_cursors (
    machine_id    CHAR(64) PRIMARY KEY REFERENCES nodes(machine_id) ON DELETE CASCADE,
    committed_seq BIGINT NOT NULL DEFAULT 0 CHECK(committed_seq >= 0),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE contents (
    content_id BIGSERIAL PRIMARY KEY,
    md5        BYTEA NOT NULL CHECK(octet_length(md5) = 16),
    file_size  BIGINT NOT NULL CHECK(file_size >= 0),
    media_kind TEXT NOT NULL CHECK(media_kind IN ('image', 'video', 'other')),
    UNIQUE(md5, file_size)
);
CREATE INDEX contents_md5_idx ON contents(md5);

CREATE TABLE file_locations (
    machine_id      CHAR(64) NOT NULL REFERENCES nodes(machine_id) ON DELETE CASCADE,
    normalized_path TEXT NOT NULL,
    display_path    TEXT NOT NULL,
    file_size       BIGINT NOT NULL CHECK(file_size >= 0),
    content_id      BIGINT NOT NULL REFERENCES contents(content_id),
    active          BOOLEAN NOT NULL DEFAULT TRUE,
    updated_seq     BIGINT NOT NULL CHECK(updated_seq >= 0),
    PRIMARY KEY(machine_id, normalized_path)
);
CREATE INDEX file_locations_content_idx ON file_locations(content_id, active);

CREATE TABLE image_stage1 (
    content_id BIGINT PRIMARY KEY REFERENCES contents(content_id) ON DELETE CASCADE,
    width      INTEGER,
    height     INTEGER,
    pdq        BYTEA CHECK(pdq IS NULL OR octet_length(pdq) = 32),
    quality    SMALLINT CHECK(quality IS NULL OR quality BETWEEN 0 AND 100)
);

CREATE TABLE image_stage2 (
    content_id  BIGINT PRIMARY KEY REFERENCES contents(content_id) ON DELETE CASCADE,
    phash_parts BYTEA CHECK(phash_parts IS NULL OR octet_length(phash_parts) = 72),
    sobel       BYTEA CHECK(sobel IS NULL OR octet_length(sobel) = 512)
);

CREATE TABLE video_metadata (
    content_id  BIGINT PRIMARY KEY REFERENCES contents(content_id) ON DELETE CASCADE,
    duration_ms BIGINT,
    width       INTEGER,
    height      INTEGER
);

CREATE TABLE video_frame_stage1 (
    content_id BIGINT NOT NULL REFERENCES contents(content_id) ON DELETE CASCADE,
    slot       SMALLINT NOT NULL CHECK(slot BETWEEN 0 AND 5),
    time_ms    BIGINT NOT NULL CHECK(time_ms >= 0),
    decoded    BOOLEAN NOT NULL,
    width      INTEGER,
    height     INTEGER,
    pdq        BYTEA CHECK(pdq IS NULL OR octet_length(pdq) = 32),
    quality    SMALLINT CHECK(quality IS NULL OR quality BETWEEN 0 AND 100),
    PRIMARY KEY(content_id, slot)
);

CREATE TABLE video_frame_stage2 (
    content_id  BIGINT NOT NULL REFERENCES contents(content_id) ON DELETE CASCADE,
    slot        SMALLINT NOT NULL CHECK(slot BETWEEN 0 AND 5),
    phash_parts BYTEA CHECK(phash_parts IS NULL OR octet_length(phash_parts) = 72),
    sobel       BYTEA CHECK(sobel IS NULL OR octet_length(sobel) = 512),
    PRIMARY KEY(content_id, slot)
);

CREATE TABLE deletion_tombstones (
    machine_id      CHAR(64) NOT NULL REFERENCES nodes(machine_id) ON DELETE CASCADE,
    normalized_path TEXT NOT NULL,
    md5             BYTEA NOT NULL CHECK(octet_length(md5) = 16),
    file_size       BIGINT NOT NULL CHECK(file_size >= 0),
    outcome         TEXT NOT NULL CHECK(outcome IN ('recycled', 'deleted')),
    updated_seq     BIGINT NOT NULL CHECK(updated_seq >= 0),
    PRIMARY KEY(machine_id, normalized_path)
);

CREATE TABLE analysis_runs (
    analysis_run_id TEXT PRIMARY KEY,
    status          TEXT NOT NULL CHECK(status IN (
        'collecting_stage1', 'stage1_synced', 'screening', 'phase2_dispatched',
        'phase2_synced', 'finalizing', 'completed', 'partial', 'cancelled'
    )),
    thresholds_toml TEXT NOT NULL,
    inputs_frozen   BOOLEAN NOT NULL DEFAULT FALSE,
    error_text      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE analysis_run_nodes (
    analysis_run_id TEXT NOT NULL REFERENCES analysis_runs(analysis_run_id) ON DELETE CASCADE,
    machine_id      CHAR(64) NOT NULL REFERENCES nodes(machine_id),
    task_id         TEXT NOT NULL,
    task_highwater  BIGINT NOT NULL CHECK(task_highwater >= 0),
    sync_highwater  BIGINT NOT NULL CHECK(sync_highwater >= 0),
    task_status     TEXT NOT NULL,
    PRIMARY KEY(analysis_run_id, machine_id, task_id)
);

CREATE TABLE analysis_run_inputs (
    analysis_run_id TEXT NOT NULL REFERENCES analysis_runs(analysis_run_id) ON DELETE CASCADE,
    md5             BYTEA NOT NULL CHECK(octet_length(md5) = 16),
    file_size       BIGINT NOT NULL CHECK(file_size >= 0),
    machine_id      CHAR(64) NOT NULL,
    normalized_path TEXT NOT NULL,
    PRIMARY KEY(analysis_run_id, md5, file_size, machine_id, normalized_path),
    FOREIGN KEY(md5, file_size) REFERENCES contents(md5, file_size),
    FOREIGN KEY(machine_id, normalized_path)
        REFERENCES file_locations(machine_id, normalized_path)
);

-- 输入一旦封存只能随整个 AnalysisRun 级联删除，不能原地改写为另一份内容。
CREATE FUNCTION reject_analysis_run_input_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'analysis_run_inputs are immutable';
END;
$$;
CREATE TRIGGER analysis_run_inputs_no_update
BEFORE UPDATE ON analysis_run_inputs
FOR EACH ROW EXECUTE FUNCTION reject_analysis_run_input_update();

CREATE TABLE candidate_pairs (
    analysis_run_id   TEXT NOT NULL REFERENCES analysis_runs(analysis_run_id) ON DELETE CASCADE,
    pair_kind         TEXT NOT NULL CHECK(pair_kind IN ('image', 'video')),
    left_md5          BYTEA NOT NULL CHECK(octet_length(left_md5) = 16),
    left_size         BIGINT NOT NULL CHECK(left_size >= 0),
    right_md5         BYTEA NOT NULL CHECK(octet_length(right_md5) = 16),
    right_size        BIGINT NOT NULL CHECK(right_size >= 0),
    stage1_score      DOUBLE PRECISION NOT NULL,
    phash_passed_parts SMALLINT,
    stage2_score      DOUBLE PRECISION,
    status            TEXT NOT NULL CHECK(status IN ('stage1_passed', 'passed', 'rejected', 'incomplete')),
    PRIMARY KEY(analysis_run_id, pair_kind, left_md5, left_size, right_md5, right_size),
    CHECK(left_md5 < right_md5 OR (left_md5 = right_md5 AND left_size < right_size)),
    FOREIGN KEY(left_md5, left_size) REFERENCES contents(md5, file_size),
    FOREIGN KEY(right_md5, right_size) REFERENCES contents(md5, file_size)
);

CREATE TABLE duplicate_groups (
    analysis_run_id   TEXT NOT NULL REFERENCES analysis_runs(analysis_run_id) ON DELETE CASCADE,
    group_id          TEXT NOT NULL,
    group_kind        TEXT NOT NULL CHECK(group_kind IN ('exact', 'image', 'video')),
    representative_md5 BYTEA NOT NULL CHECK(octet_length(representative_md5) = 16),
    representative_size BIGINT NOT NULL CHECK(representative_size >= 0),
    PRIMARY KEY(analysis_run_id, group_id),
    FOREIGN KEY(representative_md5, representative_size) REFERENCES contents(md5, file_size)
);

CREATE TABLE group_members (
    analysis_run_id TEXT NOT NULL,
    group_id        TEXT NOT NULL,
    machine_id      CHAR(64) NOT NULL,
    normalized_path TEXT NOT NULL,
    md5             BYTEA NOT NULL CHECK(octet_length(md5) = 16),
    file_size       BIGINT NOT NULL CHECK(file_size >= 0),
    representative  BOOLEAN NOT NULL,
    stage1_score    DOUBLE PRECISION NOT NULL DEFAULT 1,
    phash_passed_parts SMALLINT,
    stage2_score    DOUBLE PRECISION,
    active          BOOLEAN NOT NULL DEFAULT TRUE,
    PRIMARY KEY(analysis_run_id, group_id, machine_id, normalized_path),
    FOREIGN KEY(analysis_run_id, group_id)
        REFERENCES duplicate_groups(analysis_run_id, group_id) ON DELETE CASCADE,
    FOREIGN KEY(machine_id, normalized_path)
        REFERENCES file_locations(machine_id, normalized_path),
    FOREIGN KEY(md5, file_size) REFERENCES contents(md5, file_size)
);

CREATE TABLE review_marks (
    analysis_run_id TEXT NOT NULL,
    group_id        TEXT NOT NULL,
    machine_id      CHAR(64) NOT NULL,
    normalized_path TEXT NOT NULL,
    decision        TEXT NOT NULL CHECK(decision IN ('undecided', 'keep', 'delete')),
    PRIMARY KEY(analysis_run_id, group_id, machine_id, normalized_path),
    FOREIGN KEY(analysis_run_id, group_id, machine_id, normalized_path)
        REFERENCES group_members(analysis_run_id, group_id, machine_id, normalized_path)
        ON DELETE CASCADE
);

CREATE TABLE delete_batches (
    delete_batch_id TEXT PRIMARY KEY,
    analysis_run_id TEXT NOT NULL REFERENCES analysis_runs(analysis_run_id),
    mode            TEXT NOT NULL CHECK(mode IN ('recycle_bin', 'permanent')),
    status          TEXT NOT NULL CHECK(status IN ('queued', 'running', 'completed', 'failed', 'cancelled')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE delete_items (
    delete_item_id  TEXT PRIMARY KEY,
    delete_batch_id TEXT NOT NULL REFERENCES delete_batches(delete_batch_id) ON DELETE CASCADE,
    group_id        TEXT NOT NULL,
    machine_id      CHAR(64) NOT NULL,
    normalized_path TEXT NOT NULL,
    expected_md5    BYTEA NOT NULL CHECK(octet_length(expected_md5) = 16),
    expected_size   BIGINT NOT NULL CHECK(expected_size >= 0),
    status          TEXT NOT NULL CHECK(status IN ('queued', 'running', 'recycled', 'deleted', 'skipped', 'failed')),
    message         TEXT,
    FOREIGN KEY(machine_id, normalized_path)
        REFERENCES file_locations(machine_id, normalized_path)
);

COMMIT;
