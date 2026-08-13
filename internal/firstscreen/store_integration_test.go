package firstscreen

import (
	"context"
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestPGKeysetNullableFirstPageIncludesEmptyKeysAndTerminates(t *testing.T) {
	fixture := newTask4PGFixture(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg := DefaultConfig()
	cfg.ReadPageSize = 1
	store := NewStore(fixture.conn, cfg)

	imageSHA := fixture.sha("after-empty-image", 0)
	if _, err := fixture.conn.Exec(ctx, `
		INSERT INTO image_features(sha512,width,height,pdq256,pdq_quality) VALUES
		('',1,1,$1,80),
		($2,2,2,$3,80)`,
		task4PDQ(900), imageSHA, task4PDQ(901)); err != nil {
		t.Fatalf("seed empty-key images: %v", err)
	}
	images, err := store.LoadImageFeatures(ctx)
	if err != nil {
		t.Fatalf("LoadImageFeatures across empty first page: %v", err)
	}
	if got := task4ImageSHAs(images); !reflect.DeepEqual(got, []string{imageSHA}) {
		t.Fatalf("image SHAs after empty first page = %v, want [%s]", got, imageSHA)
	}
	if store.BadRows() != 1 {
		t.Fatalf("BadRows after empty image key = %d, want 1", store.BadRows())
	}

	videoSHA := fixture.sha("after-empty-video", 0)
	if _, err := fixture.conn.Exec(ctx, `
		INSERT INTO video_features(sha512,duration_ms,thumb_pdq256,thumb_quality) VALUES
		('',1000,$1,7),
		($2,1001,$3,8)`,
		task4PDQ(902), videoSHA, task4PDQ(903)); err != nil {
		t.Fatalf("seed empty-key videos: %v", err)
	}
	videos, err := store.LoadVideoFeatures(ctx)
	if err != nil {
		t.Fatalf("LoadVideoFeatures across empty first page: %v", err)
	}
	if got := task4VideoSHAs(videos); !reflect.DeepEqual(got, []string{videoSHA}) {
		t.Fatalf("video SHAs after empty first page = %v, want [%s]", got, videoSHA)
	}
	if store.BadRows() != 2 {
		t.Fatalf("BadRows after empty feature keys = %d, want 2", store.BadRows())
	}

	if _, err := fixture.conn.Exec(ctx, `
		INSERT INTO files(machine_id,disk_no,path,size,sha512)
		VALUES($1,0,$2,1,'')`,
		"empty-"+fixture.token, "/empty/"+fixture.token); err != nil {
		t.Fatalf("seed empty files SHA: %v", err)
	}
	err = store.StreamFilesBySHA(ctx, func([64]byte, FileRef) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "canonical lowercase SHA-512") {
		t.Fatalf("empty files SHA error = %v", err)
	}
}

func TestPGKeysetReadersPageThreeFilteringOrderingAndBadRows(t *testing.T) {
	fixture := newTask4PGFixture(t, false)
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.ReadPageSize = 3
	store := NewStore(fixture.conn, cfg)

	imageExpected := make([]string, 0, 11)
	for i := 0; i < 11; i++ {
		sha := fixture.sha("image-valid", i)
		imageExpected = append(imageExpected, sha)
		if _, err := fixture.conn.Exec(ctx, `
			INSERT INTO image_features(sha512,width,height,pdq256,pdq_quality)
			VALUES($1,$2,$3,$4,$5)`,
			sha, 800+i, 600+i, task4PDQ(i), 50+i); err != nil {
			t.Fatalf("seed image %d: %v", i, err)
		}
	}
	sort.Strings(imageExpected)
	if _, err := fixture.conn.Exec(ctx, `
		INSERT INTO image_features(sha512,width,height,pdq256,pdq_quality) VALUES
		($1,1,1,$2,49),
		($3,1,1,NULL,80),
		($4,1,1,$5,80),
		($6,1,1,$2,80)`,
		fixture.sha("image-low", 0), task4PDQ(100),
		fixture.sha("image-null", 0),
		fixture.sha("image-bad-pdq", 0), make([]byte, 31),
		"malformed-image-"+fixture.token); err != nil {
		t.Fatalf("seed image edge rows: %v", err)
	}

	images, err := store.LoadImageFeatures(ctx)
	if err != nil {
		t.Fatalf("LoadImageFeatures: %v", err)
	}
	if got := task4ImageSHAs(images); !reflect.DeepEqual(got, imageExpected) {
		t.Fatalf("image SHAs:\n got %v\nwant %v", got, imageExpected)
	}
	if len(images) != 11 {
		t.Fatalf("images = %d, want 11", len(images))
	}

	videoExpected := make([]string, 0, 11)
	videoQuality := make(map[string]int, 11)
	for i := 0; i < 11; i++ {
		sha := fixture.sha("video-valid", i)
		videoExpected = append(videoExpected, sha)
		var quality any = i
		if i == 0 {
			quality = nil
			videoQuality[sha] = 0
		} else {
			videoQuality[sha] = i
		}
		if _, err := fixture.conn.Exec(ctx, `
			INSERT INTO video_features(sha512,duration_ms,thumb_pdq256,thumb_quality)
			VALUES($1,$2,$3,$4)`,
			sha, int64(1000+i), task4PDQ(200+i), quality); err != nil {
			t.Fatalf("seed video %d: %v", i, err)
		}
	}
	sort.Strings(videoExpected)
	if _, err := fixture.conn.Exec(ctx, `
		INSERT INTO video_features(sha512,duration_ms,thumb_pdq256,thumb_quality) VALUES
		($1,1000,NULL,1),
		($2,NULL,$3,2),
		($4,1000,$5,3),
		($6,1000,$3,0)`,
		fixture.sha("video-null-pdq", 0),
		fixture.sha("video-null-duration", 0), task4PDQ(300),
		fixture.sha("video-bad-pdq", 0), make([]byte, 31),
		"malformed-video-"+fixture.token); err != nil {
		t.Fatalf("seed video edge rows: %v", err)
	}

	videos, err := store.LoadVideoFeatures(ctx)
	if err != nil {
		t.Fatalf("LoadVideoFeatures: %v", err)
	}
	if got := task4VideoSHAs(videos); !reflect.DeepEqual(got, videoExpected) {
		t.Fatalf("video SHAs:\n got %v\nwant %v", got, videoExpected)
	}
	for _, video := range videos {
		sha := hex.EncodeToString(video.SHA512[:])
		if video.ThumbQuality != videoQuality[sha] {
			t.Fatalf("video %s quality=%d want=%d", sha, video.ThumbQuality, videoQuality[sha])
		}
	}
	if store.BadRows() != 4 {
		t.Fatalf("BadRows = %d, want 4", store.BadRows())
	}
	if fresh := NewStore(fixture.conn, cfg); fresh.BadRows() != 0 {
		t.Fatalf("new Store BadRows = %d, want 0", fresh.BadRows())
	}

	type expectedFile struct {
		sha string
		ref FileRef
	}
	var fileExpected []expectedFile
	for _, shaIndex := range []int{3, 0, 2, 1} {
		sha := fixture.sha("file", shaIndex)
		for copyIndex := 0; copyIndex < 3; copyIndex++ {
			ref := FileRef{
				MachineID: fmt.Sprintf("m-%s-%d", fixture.token, copyIndex),
				DiskNo:    copyIndex,
				Path:      fmt.Sprintf("/%s/%d/%d", fixture.token, shaIndex, copyIndex),
				Size:      int64(100 + shaIndex),
			}
			if err := fixture.conn.QueryRow(ctx, `
				INSERT INTO files(machine_id,disk_no,path,size,sha512)
				VALUES($1,$2,$3,$4,$5) RETURNING id`,
				ref.MachineID, ref.DiskNo, ref.Path, ref.Size, sha).Scan(&ref.ID); err != nil {
				t.Fatalf("seed file: %v", err)
			}
			fileExpected = append(fileExpected, expectedFile{sha: sha, ref: ref})
		}
	}
	sort.Slice(fileExpected, func(i, j int) bool {
		if fileExpected[i].sha != fileExpected[j].sha {
			return fileExpected[i].sha < fileExpected[j].sha
		}
		return fileExpected[i].ref.ID < fileExpected[j].ref.ID
	})
	var fileGot []expectedFile
	err = store.StreamFilesBySHA(ctx, func(sha [64]byte, file FileRef) error {
		fileGot = append(fileGot, expectedFile{sha: hex.EncodeToString(sha[:]), ref: file})
		return nil
	})
	if err != nil {
		t.Fatalf("StreamFilesBySHA: %v", err)
	}
	if !reflect.DeepEqual(fileGot, fileExpected) {
		t.Fatalf("files:\n got %+v\nwant %+v", fileGot, fileExpected)
	}

	if _, err := fixture.conn.Exec(ctx, `
		INSERT INTO files(machine_id,disk_no,path,size,sha512)
		VALUES($1,0,$2,1,$3)`,
		"bad-"+fixture.token, "/bad/"+fixture.token, "malformed-file-"+fixture.token); err != nil {
		t.Fatalf("seed malformed file SHA: %v", err)
	}
	err = store.StreamFilesBySHA(ctx, func([64]byte, FileRef) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "canonical lowercase SHA-512") {
		t.Fatalf("malformed files SHA error = %v", err)
	}
}

func TestPGKeysetContextCancellationQueryAndCallbackErrors(t *testing.T) {
	fixture := newTask4PGFixture(t, false)
	cfg := DefaultConfig()
	cfg.ReadPageSize = 3
	store := NewStore(fixture.conn, cfg)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.LoadVideoFeatures(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled LoadVideoFeatures error = %v", err)
	}

	sentinel := errors.New("callback stopped")
	sha := fixture.sha("callback", 0)
	var id int64
	if err := fixture.conn.QueryRow(context.Background(), `
		INSERT INTO files(machine_id,disk_no,path,size,sha512)
		VALUES($1,0,$2,1,$3) RETURNING id`,
		"callback-"+fixture.token, "/callback/"+fixture.token, sha).Scan(&id); err != nil {
		t.Fatalf("seed callback file: %v", err)
	}
	err := store.StreamFilesBySHA(context.Background(), func([64]byte, FileRef) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("callback error = %v, want sentinel", err)
	}

	if _, err := fixture.conn.Exec(context.Background(), `DROP TABLE image_features`); err != nil {
		t.Fatalf("drop scoped image_features: %v", err)
	}
	if _, err := store.LoadImageFeatures(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "query image_features") {
		t.Fatalf("query error = %v", err)
	}
}

func TestDeletedExcludedFromFirstScreenFileStreamsAndSHASet(t *testing.T) {
	fixture := newTask4PGFixture(t, false)
	ctx := context.Background()
	sha := fixture.sha("deleted-excluded", 0)
	var activeID int64
	if err := fixture.conn.QueryRow(ctx, `
		INSERT INTO files(machine_id,disk_no,path,size,sha512,status)
		VALUES($1,0,$2,1,$3,'done') RETURNING id`,
		"active-"+fixture.token, "/active/"+fixture.token, sha).Scan(&activeID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.conn.Exec(ctx, `
		INSERT INTO files(machine_id,disk_no,path,size,sha512,status)
		VALUES($1,0,$2,1,$3,'deleted')`,
		"deleted-"+fixture.token, "/deleted/"+fixture.token, sha); err != nil {
		t.Fatal(err)
	}
	store := NewStore(fixture.conn, DefaultConfig())
	var ids []int64
	if err := store.StreamFilesBySHA(ctx, func(_ [64]byte, file FileRef) error {
		if file.MachineID == "active-"+fixture.token || file.MachineID == "deleted-"+fixture.token {
			ids = append(ids, file.ID)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids, []int64{activeID}) {
		t.Fatalf("first-screen file IDs=%v want [%d]", ids, activeID)
	}
	rows, err := fixture.conn.Query(ctx, qFilesBySHASet, []string{sha})
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	ids = nil
	for rows.Next() {
		var file FileRef
		var gotSHA string
		if err := rows.Scan(&file.ID, &gotSHA, &file.MachineID, &file.DiskNo, &file.Path, &file.Size); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, file.ID)
	}
	if !reflect.DeepEqual(ids, []int64{activeID}) {
		t.Fatalf("SHA set file IDs=%v want [%d]", ids, activeID)
	}
}

func TestPGKeysetSchemaTwiceIndexesAndExplainEligibility(t *testing.T) {
	fixture := newTask4PGFixture(t, true)
	ctx := context.Background()

	var hasCentralOnlyColumn bool
	if err := fixture.conn.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM information_schema.columns
			WHERE table_schema=$1 AND table_name='files' AND column_name='mtime'
		)`, fixture.schema).Scan(&hasCentralOnlyColumn); err != nil {
		t.Fatalf("check central.sql files.mtime: %v", err)
	}
	if !hasCentralOnlyColumn {
		t.Fatalf("schema %s was not created from central.sql: files.mtime is absent", fixture.schema)
	}

	expectedIndexes := map[string]string{
		"idx_files_sha512_id": fmt.Sprintf(
			"CREATE INDEX idx_files_sha512_id ON %s.files USING btree (sha512, id) WHERE (sha512 IS NOT NULL)",
			fixture.schema,
		),
		"idx_dup_groups_kind": fmt.Sprintf(
			"CREATE INDEX idx_dup_groups_kind ON %s.dup_groups USING btree (kind)",
			fixture.schema,
		),
		"idx_dup_members_file": fmt.Sprintf(
			"CREATE INDEX idx_dup_members_file ON %s.dup_members USING btree (file_id)",
			fixture.schema,
		),
	}
	for index, want := range expectedIndexes {
		qualified := fixture.schema + "." + index
		var got string
		if err := fixture.conn.QueryRow(ctx,
			`SELECT pg_get_indexdef(to_regclass($1))`, qualified).Scan(&got); err != nil {
			t.Fatalf("read definition for %s: %v", qualified, err)
		}
		if got != want {
			t.Fatalf("%s definition:\n got %q\nwant %q", qualified, got, want)
		}
	}

	for i := 0; i < 20; i++ {
		if _, err := fixture.conn.Exec(ctx, `
			INSERT INTO files(machine_id,disk_no,path,size,sha512)
			VALUES($1,0,$2,1,$3)`,
			fmt.Sprintf("explain-%s-%d", fixture.token, i),
			fmt.Sprintf("/explain/%s/%d", fixture.token, i),
			fixture.sha("explain", i)); err != nil {
			t.Fatalf("seed explain row %d: %v", i, err)
		}
	}
	if _, err := fixture.conn.Exec(ctx, `ANALYZE files`); err != nil {
		t.Fatalf("analyze files: %v", err)
	}
	if _, err := fixture.conn.Exec(ctx, `SET enable_seqscan=off`); err != nil {
		t.Fatalf("disable seqscan: %v", err)
	}
	rows, err := fixture.conn.Query(ctx, `
		EXPLAIN (ANALYZE, BUFFERS)
		SELECT sha512,id,machine_id,disk_no,path,size
		FROM files
		WHERE sha512 IS NOT NULL
		  AND ($1::text IS NULL OR (sha512,id) > ($1::text,$2))
		ORDER BY sha512,id
		LIMIT 3`, "00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000", int64(0))
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	var planLines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			t.Fatalf("scan EXPLAIN: %v", err)
		}
		planLines = append(planLines, line)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN rows: %v", err)
	}
	plan := strings.Join(planLines, "\n")
	t.Logf("EXPLAIN (ANALYZE, BUFFERS):\n%s", plan)
	if !strings.Contains(plan, "idx_files_sha512_id") {
		t.Fatalf("EXPLAIN did not use idx_files_sha512_id:\n%s", plan)
	}

	publicAfter := task4PublicSchemaSnapshot(t, fixture.conn)
	if !reflect.DeepEqual(publicAfter, fixture.publicBefore) {
		t.Fatalf("public schema changed while testing scoped central.sql:\nbefore=%v\nafter=%v",
			fixture.publicBefore, publicAfter)
	}
}

type task4PGFixture struct {
	conn         *pgx.Conn
	schema       string
	token        string
	publicBefore []string
}

func newTask4PGFixture(t *testing.T, applyCentralTwice bool) *task4PGFixture {
	t.Helper()
	dsn := os.Getenv("FS_PG_DSN")
	if dsn == "" {
		t.Skip("set FS_PG_DSN to run PostgreSQL integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect PostgreSQL: %v", err)
	}
	// Registered first: cleanup is LIFO, so scoped schema cleanup registered
	// below runs before connection closure.
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if err := conn.Close(closeCtx); err != nil {
			t.Errorf("close PostgreSQL: %v", err)
		}
	})

	var versionText string
	if err := conn.QueryRow(ctx, `SHOW server_version_num`).Scan(&versionText); err != nil {
		t.Fatalf("PostgreSQL version: %v", err)
	}
	version, err := strconv.Atoi(versionText)
	if err != nil {
		t.Fatalf("parse PostgreSQL version_num %q: %v", versionText, err)
	}
	if version < 160000 || version >= 170000 {
		t.Fatalf("PostgreSQL version_num=%d, want major 16", version)
	}
	publicBefore := task4PublicSchemaSnapshot(t, conn)

	token := task4RandomToken(t)
	schema := "fs_t4_" + token
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := conn.Exec(ctx, `CREATE SCHEMA `+quotedSchema); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if _, err := conn.Exec(cleanupCtx, `SET search_path TO public`); err != nil {
			t.Errorf("cleanup set public search_path: %v", err)
			return
		}
		if _, err := conn.Exec(cleanupCtx, `DROP SCHEMA `+quotedSchema+` CASCADE`); err != nil {
			t.Errorf("drop scoped schema %s: %v", schema, err)
			return
		}
		var residual int
		if err := conn.QueryRow(cleanupCtx,
			`SELECT count(*) FROM pg_namespace WHERE nspname=$1`, schema).Scan(&residual); err != nil {
			t.Errorf("verify schema cleanup %s: %v", schema, err)
			return
		}
		t.Logf("Task4 cleanup schema=%s residual=%d", schema, residual)
		if residual != 0 {
			t.Errorf("schema %s residual=%d, want 0", schema, residual)
		}
	})
	if _, err := conn.Exec(ctx, `SET search_path TO `+quotedSchema); err != nil {
		t.Fatalf("set scoped search_path: %v", err)
	}
	if applyCentralTwice {
		schemaSQL := task4CentralSQL(t)
		for run := 1; run <= 2; run++ {
			if _, err := conn.Exec(ctx, schemaSQL); err != nil {
				t.Fatalf("apply central.sql in schema %s run %d: %v", schema, run, err)
			}
		}
	} else if _, err := conn.Exec(ctx, `
		CREATE TABLE files (
			id BIGSERIAL PRIMARY KEY,
			machine_id TEXT NOT NULL,
			disk_no INTEGER NOT NULL,
			path TEXT NOT NULL,
			size BIGINT NOT NULL,
			sha512 TEXT
		);
		CREATE INDEX idx_files_sha512_id ON files(sha512,id) WHERE sha512 IS NOT NULL;
		CREATE TABLE image_features (
			sha512 TEXT PRIMARY KEY,
			width INTEGER NOT NULL,
			height INTEGER NOT NULL,
			pdq256 BYTEA,
			pdq_quality INTEGER NOT NULL
		);
		CREATE TABLE video_features (
			sha512 TEXT PRIMARY KEY,
			duration_ms BIGINT,
			thumb_pdq256 BYTEA,
			thumb_quality INTEGER
		);
		CREATE TABLE dup_groups (
			id BIGSERIAL PRIMARY KEY,
			kind TEXT NOT NULL
		);
		CREATE INDEX idx_dup_groups_kind ON dup_groups(kind);
		CREATE TABLE dup_members (
			group_id BIGINT NOT NULL,
			file_id BIGINT NOT NULL,
			PRIMARY KEY(group_id,file_id)
		);
		CREATE INDEX idx_dup_members_file ON dup_members(file_id);
	`); err != nil {
		t.Fatalf("create scoped tables: %v", err)
	}
	return &task4PGFixture{
		conn:         conn,
		schema:       schema,
		token:        token,
		publicBefore: publicBefore,
	}
}

func (f *task4PGFixture) sha(kind string, index int) string {
	sum := sha512.Sum512([]byte(fmt.Sprintf("%s/%s/%d", f.token, kind, index)))
	return hex.EncodeToString(sum[:])
}

func task4RandomToken(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("crypto/rand: %v", err)
	}
	return hex.EncodeToString(raw)
}

func task4PDQ(seed int) []byte {
	raw := make([]byte, 32)
	for i := 0; i < 4; i++ {
		binary.BigEndian.PutUint64(raw[i*8:(i+1)*8], uint64(seed*4+i+1))
	}
	return raw
}

func task4ImageSHAs(features []ImageFeature) []string {
	out := make([]string, len(features))
	for i, feature := range features {
		out[i] = hex.EncodeToString(feature.SHA512[:])
	}
	return out
}

func task4VideoSHAs(features []VideoFeature) []string {
	out := make([]string, len(features))
	for i, feature := range features {
		out[i] = hex.EncodeToString(feature.SHA512[:])
	}
	return out
}

func task4CentralSQL(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(filename), "..", "..", "deploy", "central.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read central.sql: %v", err)
	}
	return string(raw)
}

func task4PublicSchemaSnapshot(t *testing.T, conn *pgx.Conn) []string {
	t.Helper()
	ctx := context.Background()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin public schema snapshot: %v", err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("rollback public schema snapshot: %v", err)
		}
	}()
	if _, err := tx.Exec(ctx, `SET LOCAL search_path TO pg_catalog`); err != nil {
		t.Fatalf("fix public snapshot search_path: %v", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT kind || E'\t' || object_name || E'\t' || definition
		FROM (
			SELECT
				'relation' AS kind,
				c.relname AS object_name,
				c.relkind::text AS definition
			FROM pg_class c
			JOIN pg_namespace n ON n.oid=c.relnamespace
			WHERE n.nspname='public'
			UNION ALL
			SELECT
				'column',
				c.relname || '.' || a.attname,
				format_type(a.atttypid,a.atttypmod)
					|| '|notnull=' || a.attnotnull::text
					|| '|default=' || COALESCE(pg_get_expr(d.adbin,d.adrelid),'')
			FROM pg_attribute a
			JOIN pg_class c ON c.oid=a.attrelid
			JOIN pg_namespace n ON n.oid=c.relnamespace
			LEFT JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum
			WHERE n.nspname='public' AND a.attnum>0 AND NOT a.attisdropped
			UNION ALL
			SELECT
				'constraint',
				c.relname || '.' || x.conname,
				pg_get_constraintdef(x.oid,true)
			FROM pg_constraint x
			JOIN pg_class c ON c.oid=x.conrelid
			JOIN pg_namespace n ON n.oid=c.relnamespace
			WHERE n.nspname='public'
			UNION ALL
			SELECT
				'index',
				c.relname,
				pg_get_indexdef(c.oid)
			FROM pg_class c
			JOIN pg_namespace n ON n.oid=c.relnamespace
			WHERE n.nspname='public' AND c.relkind='i'
		) snapshot
		ORDER BY kind,object_name,definition`)
	if err != nil {
		t.Fatalf("query public schema snapshot: %v", err)
	}
	defer rows.Close()
	var snapshot []string
	for rows.Next() {
		var item string
		if err := rows.Scan(&item); err != nil {
			t.Fatalf("scan public schema snapshot: %v", err)
		}
		snapshot = append(snapshot, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read public schema snapshot: %v", err)
	}
	return snapshot
}
