-- Multi-machine media deduplication central schema.
-- PostgreSQL 16; architecture-plan v1.2 is authoritative.

CREATE TABLE IF NOT EXISTS files (
    id           BIGSERIAL PRIMARY KEY,
    machine_id   TEXT     NOT NULL,
    disk_no      INTEGER  NOT NULL DEFAULT -1,
    path         TEXT     NOT NULL,
    size         BIGINT   NOT NULL DEFAULT -1,
    mtime        BIGINT   NOT NULL DEFAULT 0,
    sha512       TEXT,
    phase1_done  SMALLINT NOT NULL DEFAULT 0,
    phase2_done  SMALLINT NOT NULL DEFAULT 0,
    status       TEXT     NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending','done','partial','failed','crash','deleted')),
    missing_mask INTEGER  NOT NULL DEFAULT 0,
    error        TEXT,
    updated_at   BIGINT   NOT NULL DEFAULT 0,
    synced_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (machine_id, path)
);
CREATE INDEX IF NOT EXISTS idx_files_sha512
    ON files (sha512) WHERE sha512 IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_files_sha512_id
    ON files (sha512, id) WHERE sha512 IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_files_status ON files (status);

CREATE TABLE IF NOT EXISTS image_features (
    sha512       TEXT PRIMARY KEY
                 CONSTRAINT image_features_sha512_lower_hex
                 CHECK (sha512 ~ '^[0-9a-f]{128}$'),
    width        INTEGER NOT NULL DEFAULT 0,
    height       INTEGER NOT NULL DEFAULT 0,
    pdq256       BYTEA,
    pdq_quality  INTEGER NOT NULL DEFAULT 0,
    phash_parts  BYTEA,
    sobel_hist   BYTEA,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS video_features (
    sha512        TEXT PRIMARY KEY
                  CONSTRAINT video_features_sha512_lower_hex
                  CHECK (sha512 ~ '^[0-9a-f]{128}$'),
    duration_ms   BIGINT,
    thumb_path    TEXT,
    thumb_pdq256  BYTEA,
    thumb_quality INTEGER,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Idempotent M1 -> M2 migration. CREATE TABLE IF NOT EXISTS does not alter
-- columns in an existing M1 database, so explicitly remove the legacy
-- defaults and NOT NULL constraints used to represent unknown values as 0.
ALTER TABLE video_features
    ALTER COLUMN duration_ms DROP NOT NULL,
    ALTER COLUMN duration_ms DROP DEFAULT,
    ALTER COLUMN thumb_quality DROP NOT NULL,
    ALTER COLUMN thumb_quality DROP DEFAULT;

-- PostgreSQL feature identities are canonical lowercase SHA-512 hex. These
-- blocks add the checks to an existing database without duplicating them
-- when this schema file is run repeatedly.
DO $migration$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'image_features'::regclass
          AND conname = 'image_features_sha512_lower_hex'
    ) THEN
        ALTER TABLE image_features
            ADD CONSTRAINT image_features_sha512_lower_hex
            CHECK (sha512 ~ '^[0-9a-f]{128}$');
    END IF;
END
$migration$;

DO $migration$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'video_features'::regclass
          AND conname = 'video_features_sha512_lower_hex'
    ) THEN
        ALTER TABLE video_features
            ADD CONSTRAINT video_features_sha512_lower_hex
            CHECK (sha512 ~ '^[0-9a-f]{128}$');
    END IF;
END
$migration$;

CREATE TABLE IF NOT EXISTS video_frames (
    sha512      TEXT    NOT NULL,
    frame_idx   INTEGER NOT NULL,
    pdq256      BYTEA,
    phash_parts BYTEA,
    sobel_hist  BYTEA,
    PRIMARY KEY (sha512, frame_idx)
);

CREATE TABLE IF NOT EXISTS dup_groups (
    id                     BIGSERIAL PRIMARY KEY,
    kind                   TEXT NOT NULL
                           CHECK (kind IN (
                               'exact',
                               'image',
                               'video',
                               'image_candidate',
                               'video_candidate'
                           )),
    representative_file_id BIGINT REFERENCES files (id),
    member_count           INTEGER NOT NULL DEFAULT 0,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_dup_groups_kind ON dup_groups (kind);

CREATE TABLE IF NOT EXISTS dup_members (
    group_id   BIGINT NOT NULL REFERENCES dup_groups (id) ON DELETE CASCADE,
    file_id    BIGINT NOT NULL REFERENCES files (id),
    score_json JSONB,
    PRIMARY KEY (group_id, file_id)
);
CREATE INDEX IF NOT EXISTS idx_dup_members_file ON dup_members (file_id);

-- M4 consumes candidate pairs only by this content key. dup_groups.id is
-- intentionally not referenced because M3 rewrites candidate groups.
CREATE TABLE IF NOT EXISTS pair_scores (
    id          BIGSERIAL PRIMARY KEY,
    kind        TEXT NOT NULL CHECK (kind IN ('image','video')),
    sha_a       TEXT NOT NULL,
    sha_b       TEXT NOT NULL,
    phase2_json JSONB,
    verdict     TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (kind, sha_a, sha_b)
);

CREATE TABLE IF NOT EXISTS scan_tasks (
    id         TEXT PRIMARY KEY,
    machine_id TEXT NOT NULL,
    phase      INTEGER NOT NULL,
    target     JSONB NOT NULL,
    status     TEXT NOT NULL CHECK (status IN ('sent','acked','running','done','failed')),
    stats_json JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Agent-local scope is isolated from Manager's global duplicate tables. All
-- identities below include machine_id and generation so replay is idempotent.
CREATE TABLE IF NOT EXISTS local_analysis_runs (
    machine_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    generation BIGINT NOT NULL,
    task_id TEXT NOT NULL,
    status TEXT NOT NULL,
    created_at BIGINT NOT NULL,
    completed_at BIGINT,
    published_at BIGINT,
    PRIMARY KEY (machine_id, run_id, generation)
);

CREATE TABLE IF NOT EXISTS local_pair_scores (
    machine_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    generation BIGINT NOT NULL,
    pair_key TEXT NOT NULL,
    left_file_id BIGINT NOT NULL,
    right_file_id BIGINT NOT NULL,
    left_sha512 TEXT NOT NULL,
    right_sha512 TEXT NOT NULL,
    stage1_json JSONB NOT NULL,
    stage2_json JSONB,
    stage3_json JSONB,
    final_verdict TEXT NOT NULL,
    PRIMARY KEY (machine_id, run_id, generation, pair_key),
    FOREIGN KEY (machine_id, run_id, generation)
        REFERENCES local_analysis_runs(machine_id, run_id, generation) ON DELETE RESTRICT
);

CREATE TABLE IF NOT EXISTS local_dup_groups (
    machine_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    generation BIGINT NOT NULL,
    group_id TEXT NOT NULL,
    category TEXT NOT NULL,
    verdict TEXT NOT NULL,
    PRIMARY KEY (machine_id, run_id, generation, group_id),
    FOREIGN KEY (machine_id, run_id, generation)
        REFERENCES local_analysis_runs(machine_id, run_id, generation) ON DELETE RESTRICT
);

CREATE TABLE IF NOT EXISTS local_dup_members (
    machine_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    generation BIGINT NOT NULL,
    group_id TEXT NOT NULL,
    file_id BIGINT NOT NULL,
    sha512 TEXT NOT NULL,
    PRIMARY KEY (machine_id, run_id, generation, group_id, file_id),
    FOREIGN KEY (machine_id, run_id, generation, group_id)
        REFERENCES local_dup_groups(machine_id, run_id, generation, group_id) ON DELETE RESTRICT
);

CREATE TABLE IF NOT EXISTS local_task_events (
    machine_id TEXT NOT NULL,
    sequence BIGINT NOT NULL,
    topic TEXT NOT NULL,
    entity_key TEXT NOT NULL,
    generation BIGINT NOT NULL,
    payload_json JSONB NOT NULL,
    PRIMARY KEY (machine_id, sequence),
    UNIQUE (machine_id, topic, entity_key, generation)
);

CREATE TABLE IF NOT EXISTS local_review_decisions (
    machine_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    generation BIGINT NOT NULL,
    group_id TEXT NOT NULL,
    file_id BIGINT NOT NULL,
    decision TEXT NOT NULL,
    reviewer TEXT NOT NULL,
    note TEXT NOT NULL DEFAULT '',
    reviewed_at BIGINT NOT NULL,
    PRIMARY KEY (machine_id, run_id, generation, group_id, file_id),
    FOREIGN KEY (machine_id, run_id, generation, group_id)
        REFERENCES local_dup_groups(machine_id, run_id, generation, group_id) ON DELETE RESTRICT
);

CREATE TABLE IF NOT EXISTS local_delete_results (
    machine_id TEXT NOT NULL,
    batch_id TEXT NOT NULL,
    file_id BIGINT NOT NULL,
    run_id TEXT NOT NULL,
    generation BIGINT NOT NULL,
    path TEXT NOT NULL,
    sha512 TEXT NOT NULL,
    result TEXT NOT NULL,
    error_code TEXT,
    uncertain BOOLEAN NOT NULL DEFAULT false,
    completed_at BIGINT NOT NULL,
    PRIMARY KEY (machine_id, batch_id, file_id),
    FOREIGN KEY (machine_id, run_id, generation)
        REFERENCES local_analysis_runs(machine_id, run_id, generation) ON DELETE RESTRICT
);
