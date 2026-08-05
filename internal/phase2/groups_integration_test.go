package phase2

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgres16ScopedGroupRebuildSchemaTwiceCleanupAndConcurrencyWhenEnabled(
	t *testing.T,
) {
	dsn := strings.TrimSpace(os.Getenv("DEDUP_TEST_PG_DSN"))
	if dsn == "" {
		t.Skip("set DEDUP_TEST_PG_DSN to run PostgreSQL 16 integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	publicBefore := task10PublicSnapshot(t, admin)
	schema := "task10_groups_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+quotedSchema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	scoped, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		scoped.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(
			context.Background(),
			10*time.Second,
		)
		defer cleanupCancel()
		if _, err := admin.Exec(
			cleanupCtx,
			`DROP SCHEMA `+quotedSchema+` CASCADE`,
		); err != nil {
			t.Errorf("drop Task10 schema: %v", err)
		}
		var residual int
		if err := admin.QueryRow(cleanupCtx, `
			SELECT count(*) FROM pg_namespace WHERE nspname=$1`,
			schema,
		).Scan(&residual); err != nil {
			t.Errorf("audit Task10 cleanup: %v", err)
		} else if residual != 0 {
			t.Errorf("Task10 scoped-schema residual=%d", residual)
		}
		publicAfter := task10PublicSnapshot(t, admin)
		if !reflect.DeepEqual(publicAfter, publicBefore) {
			t.Errorf("Task10 changed public schema:\nbefore=%v\nafter=%v",
				publicBefore, publicAfter)
		}
		admin.Close()
	}()

	var versionText string
	if err := scoped.QueryRow(ctx, `SHOW server_version_num`).Scan(&versionText); err != nil {
		t.Fatal(err)
	}
	version, err := strconv.Atoi(versionText)
	if err != nil || version < 160000 || version >= 170000 {
		t.Fatalf("server_version_num=%q, Task10 requires PostgreSQL 16", versionText)
	}
	current := ""
	if err := scoped.QueryRow(ctx, `SELECT current_schema()`).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current != schema {
		t.Fatalf("current_schema=%q, want %q", current, schema)
	}

	centralSQL, err := os.ReadFile(
		filepath.Join("..", "..", "deploy", "central.sql"),
	)
	if err != nil {
		t.Fatal(err)
	}
	for run := 1; run <= 2; run++ {
		if _, err := scoped.Exec(ctx, string(centralSQL)); err != nil {
			t.Fatalf("apply central.sql run %d: %v", run, err)
		}
	}

	fixture := seedTask10Fixture(t, scoped)
	rebuilder := NewGroupRebuilder(scoped)
	otherKindBadA, otherKindBadB := groupTestSHA('4'), groupTestSHA('5')
	if _, err := scoped.Exec(ctx, `
		INSERT INTO pair_scores(kind,sha_a,sha_b,phase2_json,verdict)
		VALUES('video',$1,$2,'{"version":999}'::jsonb,'no')`,
		otherKindBadA, otherKindBadB,
	); err != nil {
		t.Fatal(err)
	}
	sentinelsBefore := task10KindsSnapshot(
		t,
		scoped,
		[]string{"exact", "image_candidate", "video_candidate", "video"},
	)
	stats, err := rebuilder.RebuildGroups(ctx, "image")
	if err != nil {
		t.Fatal(err)
	}
	if stats != (GroupStats{Groups: 1, Members: 4}) {
		t.Fatalf("image stats=%+v", stats)
	}
	sentinelsAfter := task10KindsSnapshot(
		t,
		scoped,
		[]string{"exact", "image_candidate", "video_candidate", "video"},
	)
	if !reflect.DeepEqual(sentinelsAfter, sentinelsBefore) {
		t.Fatalf("image rebuild changed sentinels:\nbefore=%v\nafter=%v",
			sentinelsBefore, sentinelsAfter)
	}
	if _, err := scoped.Exec(ctx, `
		DELETE FROM pair_scores
		WHERE kind='video' AND sha_a=$1 AND sha_b=$2`,
		otherKindBadA, otherKindBadB,
	); err != nil {
		t.Fatal(err)
	}
	imageFirst := task10SemanticSnapshot(t, scoped, "image")
	task10AssertImageResult(t, scoped, fixture)
	stats, err = rebuilder.RebuildGroups(ctx, "image")
	if err != nil {
		t.Fatal(err)
	}
	if stats != (GroupStats{Groups: 1, Members: 4}) {
		t.Fatalf("image rerun stats=%+v", stats)
	}
	imageSecond := task10SemanticSnapshot(t, scoped, "image")
	if !reflect.DeepEqual(imageSecond, imageFirst) {
		t.Fatalf("image rerun changed semantic result:\nfirst=%v\nsecond=%v",
			imageFirst, imageSecond)
	}

	imageBeforeVideo := task10KindsSnapshot(t, scoped, []string{"image"})
	stats, err = rebuilder.RebuildGroups(ctx, "video")
	if err != nil {
		t.Fatal(err)
	}
	if stats != (GroupStats{Groups: 1, Members: 3}) {
		t.Fatalf("video stats=%+v", stats)
	}
	if got := task10KindsSnapshot(t, scoped, []string{"image"}); !reflect.DeepEqual(got, imageBeforeVideo) {
		t.Fatalf("video rebuild changed image kind:\nbefore=%v\nafter=%v",
			imageBeforeVideo, got)
	}
	task10AssertVideoVia(t, scoped, fixture.videoC)

	for _, stage := range []string{
		"begin",
		"score_query",
		"score_scan",
		"file_query",
		"file_scan",
		"delete",
		"group_insert",
		"member_insert",
		"commit_rollback",
	} {
		t.Run("rollback_"+stage, func(t *testing.T) {
			before := task10KindsSnapshot(t, scoped, []string{"image"})
			hook := &task10HookDB{
				postgresDB: scoped,
				stage:      stage,
				failure:    errors.New("forced " + stage),
			}
			stats, err := NewGroupRebuilder(hook).RebuildGroups(ctx, "image")
			if err == nil || stats != (GroupStats{}) {
				t.Fatalf("stage=%s stats=%+v err=%v", stage, stats, err)
			}
			if stage == "commit_rollback" {
				if !errors.Is(err, pgx.ErrTxCommitRollback) ||
					errors.Is(err, ErrGroupCommitOutcomeUnknown) {
					t.Fatalf("aborted commit taxonomy: %v", err)
				}
			}
			after := task10KindsSnapshot(t, scoped, []string{"image"})
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("%s failure changed old confirmed image rows:\nbefore=%v\nafter=%v",
					stage, before, after)
			}
		})
	}

	beforeLostACK := task10SemanticSnapshot(t, scoped, "image")
	lostACK := errors.New("forced lost commit acknowledgement")
	unknownStats, err := NewGroupRebuilder(&task10HookDB{
		postgresDB: scoped,
		stage:      "commit_unknown",
		failure:    lostACK,
	}).RebuildGroups(ctx, "image")
	if !errors.Is(err, ErrGroupCommitOutcomeUnknown) ||
		!errors.Is(err, lostACK) ||
		unknownStats != (GroupStats{}) {
		t.Fatalf("lost ACK stats=%+v err=%v", unknownStats, err)
	}
	afterLostACK := task10SemanticSnapshot(t, scoped, "image")
	if !reflect.DeepEqual(afterLostACK, beforeLostACK) {
		t.Fatalf("lost-ACK committed result did not converge semantically:\nbefore=%v\nafter=%v",
			beforeLostACK, afterLostACK)
	}
	stats, err = rebuilder.RebuildGroups(ctx, "image")
	if err != nil || stats != (GroupStats{Groups: 1, Members: 4}) {
		t.Fatalf("retry after lost ACK stats=%+v err=%v", stats, err)
	}
	if retry := task10SemanticSnapshot(t, scoped, "image"); !reflect.DeepEqual(retry, beforeLostACK) {
		t.Fatalf("retry after lost ACK diverged: %v", retry)
	}

	t.Run("same confirmed kind empty-old-set serializes before snapshot", func(t *testing.T) {
		videoBefore := task10KindsSnapshot(t, scoped, []string{"video"})
		if _, err := scoped.Exec(ctx, `
			DELETE FROM dup_groups WHERE kind='image'`); err != nil {
			t.Fatal(err)
		}
		connA, err := scoped.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer connA.Release()
		connB, err := scoped.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer connB.Release()

		coordinator := newTask10SameKindCoordinator()
		results := make(chan task10RebuildResult, 2)
		for _, db := range []groupRebuildDB{connA, connB} {
			db := db
			go func() {
				stats, err := NewGroupRebuilder(&task10SameKindDB{
					groupRebuildDB: db,
					coordinator:    coordinator,
				}).RebuildGroups(context.Background(), "image")
				results <- task10RebuildResult{stats: stats, err: err}
			}()
		}

		mode := <-coordinator.mode
		if mode == "locked" {
			if first := <-coordinator.lockedSnapshots; first != 1 {
				t.Fatalf("first locked snapshot ordinal=%d", first)
			}
			select {
			case ordinal := <-coordinator.lockedSnapshots:
				t.Fatalf(
					"second transaction reached snapshot before first released lock: ordinal=%d",
					ordinal,
				)
			case <-time.After(150 * time.Millisecond):
			}
			close(coordinator.releaseFirstLockedSnapshot)
			if second := <-coordinator.lockedSnapshots; second != 2 {
				t.Fatalf("second locked snapshot ordinal=%d", second)
			}
		}

		for call := 1; call <= 2; call++ {
			result := <-results
			if result.err != nil {
				t.Fatalf("same-kind rebuild %d: %v", call, result.err)
			}
			if result.stats != (GroupStats{Groups: 1, Members: 4}) {
				t.Fatalf("same-kind rebuild %d stats=%+v", call, result.stats)
			}
		}
		var groups int
		if err := scoped.QueryRow(ctx, `
			SELECT count(*) FROM dup_groups WHERE kind='image'`,
		).Scan(&groups); err != nil {
			t.Fatal(err)
		}
		if groups != 1 {
			t.Fatalf(
				"concurrent same-kind rebuild left %d semantic groups, want exactly 1",
				groups,
			)
		}
		if videoAfter := task10KindsSnapshot(t, scoped, []string{"video"}); !reflect.DeepEqual(videoAfter, videoBefore) {
			t.Fatalf("same-kind concurrency changed video rows:\nbefore=%v\nafter=%v",
				videoBefore, videoAfter)
		}
	})

	t.Run("lock wait cancellation rolls back without mutation", func(t *testing.T) {
		kinds := []string{
			"exact", "image_candidate", "video_candidate", "image", "video",
		}
		before := task10KindsSnapshot(t, scoped, kinds)
		blocker, err := scoped.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Release()
		blockingTx, err := blocker.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = blockingTx.Rollback(context.Background()) }()
		if _, err := blockingTx.Exec(ctx, `
			LOCK TABLE dup_groups IN ACCESS EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}

		waiter, err := scoped.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer waiter.Release()
		waitCtx, waitCancel := context.WithTimeout(
			context.Background(),
			150*time.Millisecond,
		)
		defer waitCancel()
		stats, err := NewGroupRebuilder(waiter).RebuildGroups(waitCtx, "image")
		if err == nil ||
			!errors.Is(err, context.DeadlineExceeded) ||
			!strings.Contains(err.Error(), "lock group replacement domain") ||
			stats != (GroupStats{}) {
			t.Fatalf("lock cancellation stats=%+v err=%v", stats, err)
		}
		if err := blockingTx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		var one int
		if err := scoped.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil || one != 1 {
			t.Fatalf("pool unusable after lock cancellation: one=%d err=%v",
				one, err)
		}
		after := task10KindsSnapshot(t, scoped, kinds)
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("lock cancellation mutated groups:\nbefore=%v\nafter=%v",
				before, after)
		}
	})

	beforeDelete := make(chan struct{})
	releaseDelete := make(chan struct{})
	concurrentErr := make(chan error, 1)
	go func() {
		_, err := NewGroupRebuilder(&task10HookDB{
			postgresDB:   scoped,
			stage:        "pause_delete",
			beforeDelete: beforeDelete,
			release:      releaseDelete,
		}).RebuildGroups(context.Background(), "image")
		concurrentErr <- err
	}()
	select {
	case <-beforeDelete:
	case <-ctx.Done():
		t.Fatal("Task10 rebuild did not reach concurrent delete barrier")
	}
	replacementStarted := make(chan struct{})
	replacementDone := make(chan error, 1)
	go func() {
		replacementDone <- task10ReplaceM3Kinds(
			context.Background(),
			scoped,
			fixture.imageA,
			replacementStarted,
		)
	}()
	<-replacementStarted
	select {
	case err := <-replacementDone:
		t.Fatalf("M3 replacement did not wait for Task10 table lock: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(releaseDelete)
	if err := <-concurrentErr; err != nil {
		t.Fatalf("concurrent Task10 rebuild: %v", err)
	}
	if err := <-replacementDone; err != nil {
		t.Fatalf("concurrent M3 replacement: %v", err)
	}
	for _, kind := range []string{
		"exact", "image_candidate", "video_candidate",
	} {
		var replacements int
		if err := scoped.QueryRow(ctx, `
			SELECT count(*)
			FROM dup_groups g
			JOIN dup_members m ON m.group_id=g.id
			WHERE g.kind=$1
			  AND m.score_json->>'replacement'=$1`,
			kind,
		).Scan(&replacements); err != nil {
			t.Fatal(err)
		}
		if replacements != 1 {
			t.Fatalf("concurrent M3 replacement kind=%s rows=%d, want 1",
				kind, replacements)
		}
	}
}

type task10Fixture struct {
	imageA int64
	imageB int64
	imageC int64
	videoC int64
}

func seedTask10Fixture(t *testing.T, pool *pgxpool.Pool) task10Fixture {
	t.Helper()
	ctx := context.Background()
	imageA, imageB, imageC := groupTestSHA('a'), groupTestSHA('b'), groupTestSHA('c')
	videoA, videoB, videoC := groupTestSHA('1'), groupTestSHA('2'), groupTestSHA('3')

	insertFile := func(machine, path, sha, status string) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO files(
				machine_id,disk_no,path,size,mtime,sha512,status
			) VALUES($1,1,$2,10,1,$3,$4)
			RETURNING id`,
			machine, path, sha, status,
		).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	result := task10Fixture{
		imageA: insertFile("machine-a", `A:\rep.jpg`, imageA, "done"),
		imageB: insertFile("machine-b", `B:\copy-1.jpg`, imageB, "done"),
		imageC: insertFile("machine-c", `C:\via.jpg`, imageC, "done"),
		videoC: insertFile("video-c", `C:\via.mp4`, videoC, "done"),
	}
	_ = insertFile("machine-d", `D:\copy-2.jpg`, imageB, "partial")
	_ = insertFile("machine-z", `Z:\deleted.jpg`, imageC, "deleted")
	videoFileA := insertFile("video-a", `A:\rep.mp4`, videoA, "done")
	_ = insertFile("video-b", `B:\direct.mp4`, videoB, "done")
	sentinelFile := insertFile("sentinel", `S:\sentinel`, groupTestSHA('d'), "done")

	if _, err := pool.Exec(ctx, `
		INSERT INTO image_features(sha512,pdq_quality) VALUES
			($1,99),($2,80),($3,70)`,
		imageA, imageB, imageC,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO video_features(sha512,thumb_quality) VALUES
			($1,99),($2,80),($3,NULL)`,
		videoA, videoB, videoC,
	); err != nil {
		t.Fatal(err)
	}
	for _, score := range []groupTestScoreRow{
		groupTestScore("image", imageA, imageB, "yes", .91),
		groupTestScore("image", imageB, imageC, "yes", .97),
		groupTestScore("image", groupTestSHA('e'), groupTestSHA('f'), "no", .1),
		groupTestScore("video", videoA, videoB, "yes", .90),
		groupTestScore("video", videoB, videoC, "yes", .96),
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO pair_scores(kind,sha_a,sha_b,phase2_json,verdict)
			VALUES($1,$2,$3,$4::jsonb,$5)`,
			score.kind, score.a, score.b, score.document, score.verdict,
		); err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []string{
		"exact", "image_candidate", "video_candidate", "image", "video",
	} {
		var groupID int64
		representative := sentinelFile
		if kind == "image" {
			representative = result.imageA
		}
		if kind == "video" {
			representative = videoFileA
		}
		if err := pool.QueryRow(ctx, `
			INSERT INTO dup_groups(
				kind,representative_file_id,member_count,created_at
			) VALUES($1,$2,1,'2026-01-02T03:04:05Z')
			RETURNING id`,
			kind, representative,
		).Scan(&groupID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO dup_members(group_id,file_id,score_json)
			VALUES($1,$2,$3::jsonb)`,
			groupID,
			representative,
			fmt.Sprintf(`{"sentinel":%q}`, kind),
		); err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func task10AssertImageResult(
	t *testing.T,
	pool *pgxpool.Pool,
	fixture task10Fixture,
) {
	t.Helper()
	var (
		representative int64
		memberCount    int
	)
	if err := pool.QueryRow(context.Background(), `
		SELECT representative_file_id,member_count
		FROM dup_groups WHERE kind='image'`,
	).Scan(&representative, &memberCount); err != nil {
		t.Fatal(err)
	}
	if representative != fixture.imageA || memberCount != 4 {
		t.Fatalf("image group representative=%d members=%d", representative, memberCount)
	}
	var deletedMembers int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*)
		FROM dup_groups g
		JOIN dup_members m ON m.group_id=g.id
		JOIN files f ON f.id=m.file_id
		WHERE g.kind='image' AND f.status='deleted'`,
	).Scan(&deletedMembers); err != nil {
		t.Fatal(err)
	}
	if deletedMembers != 0 {
		t.Fatalf("image group contains %d deleted files", deletedMembers)
	}
	var via bool
	var detailKind, verdict string
	if err := pool.QueryRow(context.Background(), `
		SELECT
			COALESCE((m.score_json->>'via')::boolean,false),
			m.score_json->'detail'->>'kind',
			m.score_json->'detail'->>'verdict'
		FROM dup_groups g
		JOIN dup_members m ON m.group_id=g.id
		WHERE g.kind='image' AND m.file_id=$1`,
		fixture.imageC,
	).Scan(&via, &detailKind, &verdict); err != nil {
		t.Fatal(err)
	}
	if !via || detailKind != "image" || verdict != "yes" {
		t.Fatalf("image via=%v detail kind/verdict=%s/%s",
			via, detailKind, verdict)
	}
}

func task10AssertVideoVia(
	t *testing.T,
	pool *pgxpool.Pool,
	videoC int64,
) {
	t.Helper()
	var (
		via      bool
		average  float64
		frameLen int
	)
	if err := pool.QueryRow(context.Background(), `
		SELECT
			(m.score_json->>'via')::boolean,
			(m.score_json->'detail'->'video'->>'average')::float8,
			jsonb_array_length(m.score_json->'detail'->'video'->'frames')
		FROM dup_groups g
		JOIN dup_members m ON m.group_id=g.id
		WHERE g.kind='video' AND m.file_id=$1`,
		videoC,
	).Scan(&via, &average, &frameLen); err != nil {
		t.Fatal(err)
	}
	if !via || average != .96 || frameLen != 6 {
		t.Fatalf("video via=%v average=%v frames=%d", via, average, frameLen)
	}
}

func task10SemanticSnapshot(
	t *testing.T,
	pool *pgxpool.Pool,
	kind string,
) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT concat_ws(
			'|',
			rep.machine_id,
			rep.path,
			g.member_count::text,
			f.machine_id,
			f.path,
			m.score_json::text
		)
		FROM dup_groups g
		JOIN files rep ON rep.id=g.representative_file_id
		JOIN dup_members m ON m.group_id=g.id
		JOIN files f ON f.id=m.file_id
		WHERE g.kind=$1
		ORDER BY rep.machine_id,rep.path,f.machine_id,f.path,m.file_id`,
		kind,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatal(err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func task10KindsSnapshot(
	t *testing.T,
	pool *pgxpool.Pool,
	kinds []string,
) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT concat_ws(
			'|',
			g.id::text,
			g.kind,
			COALESCE(g.representative_file_id,0)::text,
			g.member_count::text,
			g.created_at::text,
			COALESCE(m.file_id,0)::text,
			COALESCE(m.score_json::text,'null')
		)
		FROM dup_groups g
		LEFT JOIN dup_members m ON m.group_id=g.id
		WHERE g.kind=ANY($1::text[])
		ORDER BY g.kind,g.id,m.file_id`,
		kinds,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatal(err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func task10ReplaceM3Kinds(
	ctx context.Context,
	pool *pgxpool.Pool,
	fileID int64,
	started chan<- struct{},
) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	kinds := []string{"exact", "image_candidate", "video_candidate"}
	close(started)
	if _, err := tx.Exec(ctx, `
		DELETE FROM dup_groups WHERE kind=ANY($1::text[])`,
		kinds,
	); err != nil {
		return err
	}
	for _, kind := range kinds {
		var groupID int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO dup_groups(
				kind,representative_file_id,member_count,created_at
			) VALUES($1,$2,1,'2026-06-07T08:09:10Z')
			RETURNING id`,
			kind, fileID,
		).Scan(&groupID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO dup_members(group_id,file_id,score_json)
			VALUES($1,$2,$3::jsonb)`,
			groupID,
			fileID,
			fmt.Sprintf(`{"replacement":%q}`, kind),
		); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return nil
}

func task10PublicSnapshot(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT concat_ws(
			E'\t',
			c.relkind::text,
			c.relname,
			COALESCE(a.attname,''),
			COALESCE(pg_catalog.format_type(a.atttypid,a.atttypmod),'')
		)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid=c.relnamespace
		LEFT JOIN pg_attribute a
		  ON a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped
		WHERE n.nspname='public'
		ORDER BY c.relkind,c.relname,a.attnum`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatal(err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

type task10RebuildResult struct {
	stats GroupStats
	err   error
}

type task10SameKindCoordinator struct {
	mu                         sync.Mutex
	lockAttempts               int
	unlockedSnapshots          int
	lockedSnapshotCount        int
	modeOnce                   sync.Once
	mode                       chan string
	releaseLockAttempts        chan struct{}
	releaseUnlockedSnapshots   chan struct{}
	lockedSnapshots            chan int
	releaseFirstLockedSnapshot chan struct{}
}

func newTask10SameKindCoordinator() *task10SameKindCoordinator {
	return &task10SameKindCoordinator{
		mode:                       make(chan string, 1),
		releaseLockAttempts:        make(chan struct{}),
		releaseUnlockedSnapshots:   make(chan struct{}),
		lockedSnapshots:            make(chan int, 2),
		releaseFirstLockedSnapshot: make(chan struct{}),
	}
}

func (coordinator *task10SameKindCoordinator) beforeLock() {
	coordinator.mu.Lock()
	coordinator.lockAttempts++
	if coordinator.lockAttempts == 2 {
		coordinator.modeOnce.Do(func() { coordinator.mode <- "locked" })
		close(coordinator.releaseLockAttempts)
	}
	coordinator.mu.Unlock()
	<-coordinator.releaseLockAttempts
}

func (coordinator *task10SameKindCoordinator) afterUnlockedSnapshot() {
	coordinator.mu.Lock()
	coordinator.unlockedSnapshots++
	if coordinator.unlockedSnapshots == 2 {
		coordinator.modeOnce.Do(func() { coordinator.mode <- "unlocked" })
		close(coordinator.releaseUnlockedSnapshots)
	}
	coordinator.mu.Unlock()
	<-coordinator.releaseUnlockedSnapshots
}

func (coordinator *task10SameKindCoordinator) afterLockedSnapshot() {
	coordinator.mu.Lock()
	coordinator.lockedSnapshotCount++
	ordinal := coordinator.lockedSnapshotCount
	coordinator.mu.Unlock()
	coordinator.lockedSnapshots <- ordinal
	if ordinal == 1 {
		<-coordinator.releaseFirstLockedSnapshot
	}
}

type task10SameKindDB struct {
	groupRebuildDB
	coordinator *task10SameKindCoordinator
}

func (db *task10SameKindDB) BeginTx(
	ctx context.Context,
	options pgx.TxOptions,
) (pgx.Tx, error) {
	tx, err := db.groupRebuildDB.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return &task10SameKindTx{
		Tx:          tx,
		coordinator: db.coordinator,
	}, nil
}

type task10SameKindTx struct {
	pgx.Tx
	coordinator *task10SameKindCoordinator
	locked      bool
}

func (tx *task10SameKindTx) Exec(
	ctx context.Context,
	sql string,
	args ...any,
) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "LOCK TABLE dup_groups") {
		tx.coordinator.beforeLock()
		tag, err := tx.Tx.Exec(ctx, sql, args...)
		if err == nil {
			tx.locked = true
		}
		return tag, err
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

func (tx *task10SameKindTx) Query(
	ctx context.Context,
	sql string,
	args ...any,
) (pgx.Rows, error) {
	rows, err := tx.Tx.Query(ctx, sql, args...)
	if err != nil || !strings.Contains(sql, "FROM pair_scores") {
		return rows, err
	}
	if tx.locked {
		tx.coordinator.afterLockedSnapshot()
	} else {
		tx.coordinator.afterUnlockedSnapshot()
	}
	return rows, nil
}

type task10HookDB struct {
	postgresDB
	stage        string
	failure      error
	beforeDelete chan struct{}
	release      chan struct{}
}

func (db *task10HookDB) BeginTx(
	ctx context.Context,
	options pgx.TxOptions,
) (pgx.Tx, error) {
	if db.stage == "begin" {
		return nil, db.failure
	}
	tx, err := db.postgresDB.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return &task10HookTx{
		Tx:           tx,
		stage:        db.stage,
		failure:      db.failure,
		beforeDelete: db.beforeDelete,
		release:      db.release,
	}, nil
}

type task10HookTx struct {
	pgx.Tx
	stage        string
	failure      error
	beforeDelete chan struct{}
	release      chan struct{}
	once         sync.Once
}

func (tx *task10HookTx) Query(
	ctx context.Context,
	sql string,
	args ...any,
) (pgx.Rows, error) {
	scoreQuery := strings.Contains(sql, "FROM pair_scores")
	fileQuery := strings.Contains(sql, "FROM files")
	if (tx.stage == "score_query" && scoreQuery) ||
		(tx.stage == "file_query" && fileQuery) {
		return nil, tx.failure
	}
	rows, err := tx.Tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	if (tx.stage == "score_scan" && scoreQuery) ||
		(tx.stage == "file_scan" && fileQuery) {
		return &task10HookRows{Rows: rows, failure: tx.failure}, nil
	}
	return rows, nil
}

func (tx *task10HookTx) Exec(
	ctx context.Context,
	sql string,
	args ...any,
) (pgconn.CommandTag, error) {
	deleteGroups := strings.Contains(sql, "DELETE FROM dup_groups")
	memberInsert := strings.Contains(sql, "INSERT INTO dup_members")
	if tx.stage == "delete" && deleteGroups {
		return pgconn.CommandTag{}, tx.failure
	}
	if tx.stage == "member_insert" && memberInsert {
		return pgconn.CommandTag{}, tx.failure
	}
	if tx.stage == "pause_delete" && deleteGroups {
		tx.once.Do(func() { close(tx.beforeDelete) })
		select {
		case <-tx.release:
		case <-ctx.Done():
			return pgconn.CommandTag{}, ctx.Err()
		}
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

func (tx *task10HookTx) QueryRow(
	ctx context.Context,
	sql string,
	args ...any,
) pgx.Row {
	if tx.stage == "group_insert" &&
		strings.Contains(sql, "INSERT INTO dup_groups") {
		return task10HookRow{failure: tx.failure}
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}

func (tx *task10HookTx) Commit(ctx context.Context) error {
	switch tx.stage {
	case "commit_rollback":
		_, _ = tx.Tx.Exec(ctx, `SELECT 1/0`)
		return tx.Tx.Commit(ctx)
	case "commit_unknown":
		if err := tx.Tx.Commit(ctx); err != nil {
			return err
		}
		return tx.failure
	default:
		return tx.Tx.Commit(ctx)
	}
}

type task10HookRows struct {
	pgx.Rows
	failure error
	failed  bool
}

func (rows *task10HookRows) Scan(dest ...any) error {
	if !rows.failed {
		rows.failed = true
		return rows.failure
	}
	return rows.Rows.Scan(dest...)
}

type task10HookRow struct {
	failure error
}

func (row task10HookRow) Scan(...any) error { return row.failure }
