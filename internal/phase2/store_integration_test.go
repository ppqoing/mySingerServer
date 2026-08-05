package phase2

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"dedup/internal/features"
	"dedup/internal/proto"
)

func TestPostgres16ScopedRescreenerRestoreReplayAndBarrierWhenIntegrationEnabled(
	t *testing.T,
) {
	dsn := os.Getenv("DEDUP_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set DEDUP_TEST_PG_DSN to run PostgreSQL 16 integration")
	}
	admin, scoped, schema := openTask9ScopedPostgres(t, dsn)
	defer cleanupTask9ScopedPostgres(t, admin, scoped, schema)
	ctx := context.Background()

	var (
		version int
		current string
	)
	if err := scoped.QueryRow(ctx,
		`SELECT current_setting('server_version_num')::int,current_schema()`,
	).Scan(&version, &current); err != nil {
		t.Fatal(err)
	}
	if version < 160000 || current != schema {
		t.Fatalf("PostgreSQL fixture version=%d current_schema=%q, want PG16 and %q",
			version, current, schema)
	}

	shaA, shaB := rescreenSHA('a'), rescreenSHA('b')
	shaC, shaD := rescreenSHA('c'), rescreenSHA('d')
	shaE, shaF := rescreenSHA('e'), rescreenSHA('f')
	shaG, shaH := strings.Repeat("1", 128), strings.Repeat("2", 128)
	fileA := seedTask9File(t, scoped, shaA, `D:\a.jpg`)
	fileB := seedTask9File(t, scoped, shaB, `D:\b.jpg`)
	fileE := seedTask9File(t, scoped, shaE, `D:\e.jpg`)
	fileF := seedTask9File(t, scoped, shaF, `D:\f.jpg`)
	fileG := seedTask9File(t, scoped, shaG, `D:\g.mp4`)
	fileH := seedTask9File(t, scoped, shaH, `D:\h.mp4`)
	seedTask9Candidate(t, scoped, "image_candidate", fileB, shaB, fileA, shaA, 3, false)
	seedTask9Candidate(t, scoped, "image_candidate", fileA, shaA, fileB, shaB, 3, false)
	seedTask9Candidate(t, scoped, "image_candidate", fileE, shaE, fileF, shaF, 0, true)
	seedTask9VideoCandidate(t, scoped, fileG, shaG, fileH, shaH, 4, 1250)

	partsBlob, sobelBlob := rescreenFeatureBlobs(t, 1)
	for _, sha := range []string{shaA, shaB} {
		if _, err := scoped.Exec(ctx, `
			INSERT INTO image_features(sha512,phash_parts,sobel_hist)
			VALUES($1,$2,$3)`,
			sha, partsBlob, sobelBlob,
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := scoped.Exec(ctx, `
		INSERT INTO image_features(sha512,phash_parts,sobel_hist)
		VALUES($1,$2,NULL)`,
		shaE, partsBlob,
	); err != nil {
		t.Fatal(err)
	}
	for _, sha := range []string{shaG, shaH} {
		for frameIndex := 0; frameIndex < 4; frameIndex++ {
			if _, err := scoped.Exec(ctx, `
				INSERT INTO video_frames(
					sha512,frame_idx,pdq256,phash_parts,sobel_hist
				) VALUES($1,$2,$3,$4,$5)`,
				sha,
				frameIndex,
				bytes.Repeat([]byte{byte(frameIndex + 1)}, 32),
				partsBlob,
				sobelBlob,
			); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := scoped.Exec(ctx, `
		INSERT INTO pair_scores(kind,sha_a,sha_b,phase2_json,verdict)
		VALUES(
			'image',$1,$2,
			'{
				"version":1,
				"kind":"image",
				"verdict":"inconclusive",
				"image":{
					"phash_evaluated":false,
					"sobel_evaluated":false
				}
			}'::jsonb,
			'inconclusive'
		)`,
		shaC, shaD,
	); err != nil {
		t.Fatal(err)
	}

	store := &postgresRescreenStore{pool: scoped}
	for _, test := range []struct {
		name     string
		kind     string
		document string
	}{
		{
			name: "unrelated bad kind",
			kind: "mystery",
			document: `{
				"version":1,
				"kind":"mystery",
				"verdict":"inconclusive",
				"image":{
					"phash_evaluated":false,
					"sobel_evaluated":false
				}
			}`,
		},
		{
			name:     "unrelated bad JSON version",
			kind:     "image",
			document: `{"version":999}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := scoped.Exec(ctx, `
				INSERT INTO pair_scores(kind,sha_a,sha_b,phase2_json,verdict)
				VALUES($1,$2,$3,$4::jsonb,'inconclusive')`,
				test.kind,
				strings.Repeat("5", 128),
				strings.Repeat("6", 128),
				test.document,
			); err != nil {
				t.Fatal(err)
			}
			if _, err := store.reconcile(ctx); err == nil {
				t.Fatal("malformed unrelated durable row was hidden by stale sweep")
			}
			if _, err := scoped.Exec(ctx, `
				DELETE FROM pair_scores
				WHERE kind=$1 AND sha_a=$2 AND sha_b=$3`,
				test.kind,
				strings.Repeat("5", 128),
				strings.Repeat("6", 128),
			); err != nil {
				t.Fatal(err)
			}
		})
	}
	rescreener := newRescreener(store, rescreenConfig(), nil)
	if err := rescreener.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	progress := rescreener.Progress()
	if progress.TotalPairs != 3 || progress.ResolvedPairs != 2 ||
		progress.UnresolvedPairs != 1 || progress.CachedEndpoints != 1 {
		t.Fatalf("initial restore progress = %#v", progress)
	}

	key := PairKey{Kind: "image", SHAA: shaA, SHAB: shaB}
	beforeJSON, beforeVerdict, beforeCreated := readTask9Score(
		t, scoped, key,
	)
	if beforeVerdict != "yes" || !strings.Contains(beforeJSON, `"version": 1`) {
		t.Fatalf("persisted row verdict=%q json=%s", beforeVerdict, beforeJSON)
	}
	var staleCount int
	if err := scoped.QueryRow(ctx, `
		SELECT count(*) FROM pair_scores
		WHERE kind='image' AND sha_a=$1 AND sha_b=$2`,
		shaC, shaD,
	).Scan(&staleCount); err != nil {
		t.Fatal(err)
	}
	if staleCount != 0 {
		t.Fatal("stale candidate score survived reconciliation")
	}

	snapshot, err := store.reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Pairs) != 3 || len(snapshot.Resolved) != 2 {
		t.Fatalf("reconciled snapshot pairs=%d resolved=%d",
			len(snapshot.Pairs), len(snapshot.Resolved))
	}
	replay := snapshot.Resolved[key]
	for iteration := 0; iteration < 10; iteration++ {
		got, err := store.upsertScore(ctx, replay)
		if err != nil {
			t.Fatalf("replay %d: %v", iteration, err)
		}
		if !reflect.DeepEqual(got, replay) {
			t.Fatalf("replay %d returned %#v, want %#v", iteration, got, replay)
		}
	}
	afterJSON, afterVerdict, afterCreated := readTask9Score(t, scoped, key)
	if beforeJSON != afterJSON || beforeVerdict != afterVerdict ||
		!beforeCreated.Equal(afterCreated) {
		t.Fatalf("ten replays changed row:\nbefore=%s|%s|%s\nafter=%s|%s|%s",
			beforeJSON, beforeVerdict, beforeCreated,
			afterJSON, afterVerdict, afterCreated)
	}
	var rowCount int
	if err := scoped.QueryRow(ctx, `
		SELECT count(*) FROM pair_scores
		WHERE kind=$1 AND sha_a=$2 AND sha_b=$3`,
		key.Kind, key.SHAA, key.SHAB,
	).Scan(&rowCount); err != nil {
		t.Fatal(err)
	}
	if rowCount != 1 {
		t.Fatalf("replay row count=%d, want 1", rowCount)
	}

	conflict := replay
	conflict.Verdict = "no"
	conflict.Document.Verdict = "no"
	if _, err := store.upsertScore(ctx, conflict); err == nil {
		t.Fatal("conflicting existing pair score was overwritten")
	}
	conflictJSON, conflictVerdict, conflictCreated := readTask9Score(t, scoped, key)
	if conflictJSON != beforeJSON || conflictVerdict != beforeVerdict ||
		!conflictCreated.Equal(beforeCreated) {
		t.Fatal("conflicting upsert changed durable row")
	}

	restarted := newRescreener(store, rescreenConfig(), nil)
	callbacks := 0
	restarted.SetOnPairResolved(func(PairKey, Verdict) { callbacks++ })
	if err := restarted.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	if callbacks != 0 || restarted.Progress().ResolvedPairs != 2 ||
		restarted.Progress().UnresolvedPairs != 1 {
		t.Fatalf("restart state callbacks=%d progress=%#v",
			callbacks, restarted.Progress())
	}
	restartJSON, restartVerdict, restartCreated := readTask9Score(t, scoped, key)
	if restartJSON != beforeJSON || restartVerdict != beforeVerdict ||
		!restartCreated.Equal(beforeCreated) {
		t.Fatal("restart rewrote existing resolved score")
	}

	unresolved := []PairKey{{Kind: "image", SHAA: shaE, SHAB: shaF}}
	seedTask9PendingEnvelope(t, scoped, "unrelated", rescreenSHA('9'), proto.KindImage)
	active, err := store.hasRelevantActivePhase2(ctx, unresolved)
	if err != nil {
		t.Fatal(err)
	}
	if active {
		t.Fatal("unrelated pending Phase2 task blocked current generation")
	}
	seedTask9PendingEnvelope(t, scoped, "related", shaE, proto.KindImage)
	active, err = store.hasRelevantActivePhase2(ctx, unresolved)
	if err != nil {
		t.Fatal(err)
	}
	if !active {
		t.Fatal("related pending Phase2 task was not detected")
	}
	if _, err := scoped.Exec(ctx, `
		INSERT INTO scan_tasks(id,machine_id,phase,target,status)
		VALUES('malformed','machine-a',2,'{"type":"phase2"}'::jsonb,'running')`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.hasRelevantActivePhase2(ctx, unresolved); err == nil {
		t.Fatal("malformed active target allowed finalization")
	}
	if _, err := scoped.Exec(ctx, `DELETE FROM scan_tasks`); err != nil {
		t.Fatal(err)
	}

	var conflictingMember int64
	if err := scoped.QueryRow(ctx, `
		SELECT m.file_id
		FROM dup_groups g
		JOIN dup_members m ON m.group_id=g.id
		JOIN files f ON f.id=m.file_id
		WHERE g.kind='image_candidate' AND f.sha512=$1
		ORDER BY g.id,m.file_id LIMIT 1`,
		shaA,
	).Scan(&conflictingMember); err != nil {
		t.Fatal(err)
	}
	if _, err := scoped.Exec(ctx, `
		UPDATE dup_members
		SET score_json=jsonb_set(score_json,'{hamming}','4'::jsonb)
		WHERE file_id=$1`,
		conflictingMember,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.reconcile(ctx); err == nil {
		t.Fatal("conflicting duplicate/reversed M3 trace was accepted")
	}
	if _, err := scoped.Exec(ctx, `
		UPDATE dup_members
		SET score_json=jsonb_set(score_json,'{hamming}','3'::jsonb)
		WHERE file_id=$1`,
		conflictingMember,
	); err != nil {
		t.Fatal(err)
	}

	if _, err := scoped.Exec(ctx, `
		UPDATE pair_scores SET phase2_json='{"version":99}'::jsonb
		WHERE kind=$1 AND sha_a=$2 AND sha_b=$3`,
		key.Kind, key.SHAA, key.SHAB,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.reconcile(ctx); err == nil {
		t.Fatal("malformed durable score document was accepted")
	}
	if _, err := scoped.Exec(ctx, `
		UPDATE pair_scores SET phase2_json=$1::jsonb
		WHERE kind=$2 AND sha_a=$3 AND sha_b=$4`,
		beforeJSON, key.Kind, key.SHAA, key.SHAB,
	); err != nil {
		t.Fatal(err)
	}

	if _, err := scoped.Exec(ctx, `
		UPDATE pair_scores SET sha_a=$1,sha_b=$2
		WHERE kind=$3 AND sha_a=$2 AND sha_b=$1`,
		shaB, shaA, key.Kind,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.reconcile(ctx); err == nil {
		t.Fatal("reversed durable TEXT key was accepted under weak schema")
	}
	if _, err := scoped.Exec(ctx, `
		UPDATE pair_scores SET sha_a=$1,sha_b=$2
		WHERE kind=$3 AND sha_a=$2 AND sha_b=$1`,
		shaA, shaB, key.Kind,
	); err != nil {
		t.Fatal(err)
	}

	if _, err := scoped.Exec(ctx, `
		UPDATE image_features SET phash_parts=$1 WHERE sha512=$2`,
		[]byte{1}, shaE,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.reconcile(ctx); err == nil {
		t.Fatal("corrupt durable partial feature was accepted")
	}
}

func openTask9ScopedPostgres(
	t *testing.T,
	dsn string,
) (*pgxpool.Pool, *pgxpool.Pool, string) {
	t.Helper()
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "task9_rescreen_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		_, _ = admin.Exec(ctx, "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
		t.Fatal(err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET search_path TO "+identifier)
		return err
	}
	scoped, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		_, _ = admin.Exec(ctx, "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
		t.Fatal(err)
	}
	for _, statement := range task9SchemaStatements {
		if _, err := scoped.Exec(ctx, statement); err != nil {
			scoped.Close()
			_, _ = admin.Exec(ctx, "DROP SCHEMA "+identifier+" CASCADE")
			admin.Close()
			t.Fatal(err)
		}
	}
	return admin, scoped, schema
}

func cleanupTask9ScopedPostgres(
	t *testing.T,
	admin, scoped *pgxpool.Pool,
	schema string,
) {
	t.Helper()
	scoped.Close()
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(context.Background(),
		"DROP SCHEMA "+identifier+" CASCADE",
	); err != nil {
		t.Errorf("drop Task9 schema: %v", err)
	}
	var residual int
	if err := admin.QueryRow(context.Background(), `
		SELECT count(*) FROM pg_namespace WHERE nspname=$1`,
		schema,
	).Scan(&residual); err != nil {
		t.Errorf("audit Task9 schema cleanup: %v", err)
	} else if residual != 0 {
		t.Errorf("Task9 schema residual=%d", residual)
	}
	admin.Close()
}

var task9SchemaStatements = []string{
	`CREATE TABLE files(
		id BIGSERIAL PRIMARY KEY,
		machine_id TEXT NOT NULL,
		path TEXT NOT NULL,
		sha512 TEXT,
		status TEXT NOT NULL
	)`,
	`CREATE TABLE image_features(
		sha512 TEXT PRIMARY KEY,
		phash_parts BYTEA,
		sobel_hist BYTEA
	)`,
	`CREATE TABLE video_frames(
		sha512 TEXT NOT NULL,
		frame_idx INTEGER NOT NULL,
		pdq256 BYTEA,
		phash_parts BYTEA,
		sobel_hist BYTEA,
		PRIMARY KEY(sha512,frame_idx)
	)`,
	`CREATE TABLE dup_groups(
		id BIGSERIAL PRIMARY KEY,
		kind TEXT NOT NULL
	)`,
	`CREATE TABLE dup_members(
		group_id BIGINT NOT NULL,
		file_id BIGINT NOT NULL,
		score_json JSONB,
		PRIMARY KEY(group_id,file_id)
	)`,
	`CREATE TABLE pair_scores(
		id BIGSERIAL PRIMARY KEY,
		kind TEXT NOT NULL,
		sha_a TEXT NOT NULL,
		sha_b TEXT NOT NULL,
		phase2_json JSONB,
		verdict TEXT NOT NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		UNIQUE(kind,sha_a,sha_b)
	)`,
	`CREATE TABLE scan_tasks(
		id TEXT PRIMARY KEY,
		machine_id TEXT NOT NULL,
		phase INTEGER NOT NULL,
		target JSONB NOT NULL,
		status TEXT NOT NULL
	)`,
}

func seedTask9File(
	t *testing.T,
	pool *pgxpool.Pool,
	sha, path string,
) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO files(machine_id,path,sha512,status)
		VALUES('machine-a',$1,$2,'done')
		RETURNING id`,
		path, sha,
	).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func seedTask9Candidate(
	t *testing.T,
	pool *pgxpool.Pool,
	kind string,
	leftFile int64,
	leftSHA string,
	rightFile int64,
	rightSHA string,
	hamming int,
	nullScores bool,
) {
	t.Helper()
	var groupID int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO dup_groups(kind) VALUES($1) RETURNING id`,
		kind,
	).Scan(&groupID); err != nil {
		t.Fatal(err)
	}
	leftQuality, rightQuality := 80, 70
	if leftSHA < rightSHA {
		leftQuality, rightQuality = 70, 80
	}
	for _, member := range []struct {
		fileID int64
		self   string
		peer   string
		qSelf  int
		qPeer  int
	}{
		{leftFile, leftSHA, rightSHA, leftQuality, rightQuality},
		{rightFile, rightSHA, leftSHA, rightQuality, leftQuality},
	} {
		var score any
		if !nullScores {
			score = fmt.Sprintf(
				`{"hamming":%d,"quality_self":%d,"quality_peer":%d,"peer_sha512":%q}`,
				hamming, member.qSelf, member.qPeer, member.peer,
			)
		}
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO dup_members(group_id,file_id,score_json)
			VALUES($1,$2,$3::jsonb)`,
			groupID, member.fileID, score,
		); err != nil {
			t.Fatal(err)
		}
	}
}

func seedTask9VideoCandidate(
	t *testing.T,
	pool *pgxpool.Pool,
	leftFile int64,
	leftSHA string,
	rightFile int64,
	rightSHA string,
	hamming int,
	durationDiff int64,
) {
	t.Helper()
	var groupID int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO dup_groups(kind) VALUES('video_candidate') RETURNING id`,
	).Scan(&groupID); err != nil {
		t.Fatal(err)
	}
	for _, member := range []struct {
		fileID int64
		peer   string
		qSelf  int
		qPeer  int
	}{
		{leftFile, rightSHA, 60, 75},
		{rightFile, leftSHA, 75, 60},
	} {
		score := fmt.Sprintf(
			`{"hamming":%d,"duration_diff_ms":%d,"quality_self":%d,"quality_peer":%d,"peer_sha512":%q}`,
			hamming, durationDiff, member.qSelf, member.qPeer, member.peer,
		)
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO dup_members(group_id,file_id,score_json)
			VALUES($1,$2,$3::jsonb)`,
			groupID, member.fileID, score,
		); err != nil {
			t.Fatal(err)
		}
	}
}

func readTask9Score(
	t *testing.T,
	pool *pgxpool.Pool,
	key PairKey,
) (string, string, time.Time) {
	t.Helper()
	var (
		document string
		verdict  string
		created  time.Time
	)
	if err := pool.QueryRow(context.Background(), `
		SELECT phase2_json::text,verdict,created_at
		FROM pair_scores
		WHERE kind=$1 AND sha_a=$2 AND sha_b=$3`,
		key.Kind, key.SHAA, key.SHAB,
	).Scan(&document, &verdict, &created); err != nil {
		t.Fatal(err)
	}
	return document, verdict, created
}

func seedTask9PendingEnvelope(
	t *testing.T,
	pool *pgxpool.Pool,
	_ string,
	sha string,
	kind uint8,
) {
	t.Helper()
	fields := uint32(proto.FieldPHashParts | proto.FieldSobelHist)
	path := `D:\pending.jpg`
	frameMask := uint8(0)
	duration := int64(0)
	if kind == proto.KindVideo {
		fields = proto.FieldVideo6F
		path = `D:\pending.mp4`
		frameMask = proto.FrameMaskFull
		duration = 12000
	}
	target := phase2Target{
		Type:      phase2TargetType,
		MachineID: "machine-a",
		Task: proto.Phase2Task{
			Items: []proto.Phase2Item{{
				Path: path, FieldsMask: fields, MachineID: "machine-a",
				SHA512: sha, Kind: kind, FrameMask: frameMask,
				DurationMS: duration,
			}},
		},
	}
	envelope := RoutedTask{MachineID: target.MachineID, Task: target.Task}
	target.Task.TaskID = stableTaskID(envelope)
	raw, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO scan_tasks(id,machine_id,phase,target,status)
		VALUES($1,'machine-a',2,$2::jsonb,'running')`,
		target.Task.TaskID, raw,
	); err != nil {
		t.Fatal(err)
	}
}

func TestTask9FixtureCodecBytesArePortable(t *testing.T) {
	parts, sobel := rescreenFeatureBlobs(t, 5)
	if len(parts) != 76 || len(sobel) != 516 {
		t.Fatalf("fixture BLOB sizes=%d/%d", len(parts), len(sobel))
	}
	if _, err := features.DecodePHashParts(parts); err != nil {
		t.Fatal(err)
	}
	if _, err := features.DecodeSobelHist(sobel); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(parts, sobel) {
		t.Fatal("fixture codecs unexpectedly equal")
	}
}
