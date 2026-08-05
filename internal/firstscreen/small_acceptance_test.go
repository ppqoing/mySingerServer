package firstscreen

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type smallAcceptanceMarker struct {
	RunID             string         `json:"run_id"`
	Counts            map[string]int `json:"counts"`
	StageKeys         []string       `json:"stage_keys"`
	CleanupResidual   int            `json:"cleanup_residual"`
	Rerun             bool           `json:"rerun"`
	CentralSQLRuns    int            `json:"central_sql_runs"`
	ReadPageSize      int            `json:"read_page_size"`
	SentinelPreserved bool           `json:"sentinel_preserved"`
	PublicUnchanged   bool           `json:"public_unchanged"`
}

type smallAcceptanceFixture struct {
	conn       *pgx.Conn
	schema     string
	marker     smallAcceptanceMarker
	emitMarker bool
}

func TestIntegrationSmallDB(t *testing.T) {
	fixture := newSmallAcceptanceFixture(t)
	ids := seedSmallAcceptance(t, fixture)
	seedSmallM4Sentinels(t, fixture, ids["A1"])
	m4Before := snapshotSmallM4(t, fixture)

	cfg := DefaultConfig()
	cfg.ReadPageSize = 3
	first, err := NewAnalyzer(NewStore(fixture.conn, cfg), cfg, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("first analyzer run: %v", err)
	}
	assertSmallAcceptanceStats(t, first)
	firstSnapshot := assertSmallAcceptanceGroups(t, fixture)
	if after := snapshotSmallM4(t, fixture); !reflect.DeepEqual(after, m4Before) {
		t.Fatalf("M4 sentinels changed after first run:\nbefore=%v\nafter=%v", m4Before, after)
	}

	second, err := NewAnalyzer(NewStore(fixture.conn, cfg), cfg, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("second analyzer run: %v", err)
	}
	assertSmallAcceptanceStats(t, second)
	secondSnapshot := assertSmallAcceptanceGroups(t, fixture)
	if !reflect.DeepEqual(secondSnapshot, firstSnapshot) {
		t.Fatalf("rerun semantic snapshot changed:\nfirst=%v\nsecond=%v", firstSnapshot, secondSnapshot)
	}
	if after := snapshotSmallM4(t, fixture); !reflect.DeepEqual(after, m4Before) {
		t.Fatalf("M4 sentinels changed after rerun:\nbefore=%v\nafter=%v", m4Before, after)
	}

	stageKeys := make([]string, 0, len(second.StageElapsedMs))
	for stage := range second.StageElapsedMs {
		stageKeys = append(stageKeys, stage)
	}
	sort.Strings(stageKeys)
	fixture.marker = smallAcceptanceMarker{
		RunID: os.Getenv("M3_VERIFY_RUN_ID"),
		Counts: map[string]int{
			"files_scanned":   second.FilesScanned,
			"image_features":  second.ImageFeatures,
			"video_features":  second.VideoFeatures,
			"exact_groups":    second.ExactGroups,
			"exact_members":   second.ExactMembers,
			"image_pairs":     second.ImagePairs,
			"video_pairs":     second.VideoPairs,
			"groups_written":  second.GroupsWritten,
			"members_written": second.MembersWritten,
			"skipped_pairs":   second.SkippedPairs,
			"bad_rows":        second.BadRows,
		},
		StageKeys:         stageKeys,
		CleanupResidual:   -1,
		Rerun:             true,
		CentralSQLRuns:    2,
		ReadPageSize:      cfg.ReadPageSize,
		SentinelPreserved: true,
		PublicUnchanged:   true,
	}
	if fixture.marker.RunID == "" {
		fixture.marker.RunID = "manual-" + smallAcceptanceToken(t)
	}
	fixture.emitMarker = true
}

func newSmallAcceptanceFixture(t *testing.T) *smallAcceptanceFixture {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("FS_PG_DSN"))
	if dsn == "" {
		t.Skip("set FS_PG_DSN to run PostgreSQL acceptance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect PostgreSQL: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if err := conn.Close(closeCtx); err != nil {
			t.Errorf("close PostgreSQL: %v", err)
		}
	})

	var versionText string
	if err := conn.QueryRow(ctx, `SHOW server_version_num`).Scan(&versionText); err != nil {
		t.Fatalf("read PostgreSQL version: %v", err)
	}
	if !strings.HasPrefix(versionText, "16") {
		t.Fatalf("PostgreSQL version_num=%s, want major 16", versionText)
	}

	publicBefore := task4PublicSchemaSnapshot(t, conn)
	fixture := &smallAcceptanceFixture{
		conn:   conn,
		schema: "m3_small_" + smallAcceptanceToken(t),
	}
	quotedSchema := pgx.Identifier{fixture.schema}.Sanitize()
	if _, err := conn.Exec(ctx, `CREATE SCHEMA `+quotedSchema); err != nil {
		t.Fatalf("create run-unique schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		residual := -1
		if _, err := conn.Exec(cleanupCtx, `SET search_path TO public`); err != nil {
			t.Errorf("cleanup set public search_path: %v", err)
		} else if _, err := conn.Exec(cleanupCtx, `DROP SCHEMA `+quotedSchema+` CASCADE`); err != nil {
			t.Errorf("drop acceptance schema: %v", err)
		} else if err := conn.QueryRow(cleanupCtx,
			`SELECT count(*) FROM pg_namespace WHERE nspname=$1`,
			fixture.schema,
		).Scan(&residual); err != nil {
			t.Errorf("verify acceptance cleanup: %v", err)
		}
		t.Logf("M3 small acceptance cleanup schema=%s residual=%d", fixture.schema, residual)
		if residual != 0 {
			t.Errorf("acceptance cleanup residual=%d, want 0", residual)
		}
		if fixture.emitMarker {
			fixture.marker.CleanupResidual = residual
			raw, err := json.Marshal(fixture.marker)
			if err != nil {
				t.Errorf("marshal acceptance marker: %v", err)
				return
			}
			t.Logf("M3_SMALL_ACCEPTANCE %s", raw)
		}
	})
	if _, err := conn.Exec(ctx, `SET search_path TO `+quotedSchema); err != nil {
		t.Fatalf("set acceptance search_path: %v", err)
	}

	schemaSQL := smallAcceptanceCentralSQL(t)
	for run := 1; run <= 2; run++ {
		if _, err := conn.Exec(ctx, schemaSQL); err != nil {
			t.Fatalf("apply central.sql run %d: %v", run, err)
		}
	}
	publicAfter := task4PublicSchemaSnapshot(t, conn)
	if !reflect.DeepEqual(publicAfter, publicBefore) {
		t.Fatalf(
			"public schema changed while testing scoped central.sql:\nbefore=%v\nafter=%v",
			publicBefore,
			publicAfter,
		)
	}
	return fixture
}

func seedSmallAcceptance(t *testing.T, fixture *smallAcceptanceFixture) map[string]int64 {
	t.Helper()
	ctx := context.Background()
	shas := map[string]string{
		"A1": smallAcceptanceSHA(1),
		"A2": smallAcceptanceSHA(2),
		"A3": smallAcceptanceSHA(3),
		"A4": smallAcceptanceSHA(4),
		"A5": smallAcceptanceSHA(5),
		"V1": smallAcceptanceSHA(11),
		"V2": smallAcceptanceSHA(12),
		"V3": smallAcceptanceSHA(13),
		"V4": smallAcceptanceSHA(14),
		"E":  smallAcceptanceSHA(99),
	}

	imageBase := make([]byte, 32)
	imageNear := append([]byte(nil), imageBase...)
	imageNear[0] = 0b00000111
	imageFar := make([]byte, 32)
	for index := range imageFar {
		imageFar[index] = 0xff
	}
	imageRows := [][]any{
		{shas["A1"], 1920, 1080, imageBase, 80},
		{shas["A2"], 1920, 1080, imageNear, 90},
		{shas["A3"], 1920, 1080, imageNear, 30},
		{shas["A4"], 1440, 1080, imageNear, 80},
		{shas["A5"], 1920, 1080, imageFar, 99},
	}
	if _, err := fixture.conn.CopyFrom(
		ctx,
		pgx.Identifier{"image_features"},
		[]string{"sha512", "width", "height", "pdq256", "pdq_quality"},
		pgx.CopyFromRows(imageRows),
	); err != nil {
		t.Fatalf("seed image_features: %v", err)
	}

	videoBase := make([]byte, 32)
	for index := range videoBase {
		videoBase[index] = 0xaa
	}
	videoNear := append([]byte(nil), videoBase...)
	videoNear[0] ^= 1
	videoFar := make([]byte, 32)
	for index := range videoFar {
		videoFar[index] = 0x55
	}
	videoRows := [][]any{
		{shas["V1"], int64(60000), videoBase, 70},
		{shas["V2"], int64(61500), videoNear, 70},
		{shas["V3"], int64(62600), videoNear, 70},
		{shas["V4"], int64(60000), videoFar, 70},
	}
	if _, err := fixture.conn.CopyFrom(
		ctx,
		pgx.Identifier{"video_features"},
		[]string{"sha512", "duration_ms", "thumb_pdq256", "thumb_quality"},
		pgx.CopyFromRows(videoRows),
	); err != nil {
		t.Fatalf("seed video_features: %v", err)
	}

	fileRows := [][]any{
		{"m1", 0, "D:/a1.jpg", int64(100), shas["A1"]},
		{"m1", 0, "D:/a2.jpg", int64(100), shas["A2"]},
		{"m2", 1, "E:/copy/a2.jpg", int64(100), shas["A2"]},
		{"m1", 0, "D:/a3.jpg", int64(100), shas["A3"]},
		{"m1", 0, "D:/a4.jpg", int64(100), shas["A4"]},
		{"m1", 0, "D:/a5.jpg", int64(100), shas["A5"]},
		{"m1", 0, "D:/v1.mp4", int64(200), shas["V1"]},
		{"m1", 0, "D:/v2.mp4", int64(200), shas["V2"]},
		{"m1", 0, "D:/v3.mp4", int64(200), shas["V3"]},
		{"m1", 0, "D:/v4.mp4", int64(200), shas["V4"]},
		{"m1", 0, "D:/e1.bin", int64(300), shas["E"]},
		{"m2", 1, "E:/e2.bin", int64(300), shas["E"]},
		{"m3", 2, "F:/e3.bin", int64(300), shas["E"]},
	}
	if _, err := fixture.conn.CopyFrom(
		ctx,
		pgx.Identifier{"files"},
		[]string{"machine_id", "disk_no", "path", "size", "sha512"},
		pgx.CopyFromRows(fileRows),
	); err != nil {
		t.Fatalf("seed files: %v", err)
	}

	rows, err := fixture.conn.Query(ctx, `SELECT id,path FROM files ORDER BY id`)
	if err != nil {
		t.Fatalf("read seeded file IDs: %v", err)
	}
	defer rows.Close()
	ids := make(map[string]int64)
	pathLabels := map[string]string{
		"D:/a1.jpg": "A1",
		"D:/a2.jpg": "A2",
	}
	for rows.Next() {
		var id int64
		var path string
		if err := rows.Scan(&id, &path); err != nil {
			t.Fatalf("scan seeded file ID: %v", err)
		}
		if label := pathLabels[path]; label != "" {
			ids[label] = id
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read seeded file IDs: %v", err)
	}
	if ids["A1"] == 0 || ids["A2"] == 0 {
		t.Fatalf("seeded IDs = %v", ids)
	}
	return ids
}

func seedSmallM4Sentinels(t *testing.T, fixture *smallAcceptanceFixture, fileID int64) {
	t.Helper()
	for index, kind := range []string{"image", "video"} {
		var groupID int64
		if err := fixture.conn.QueryRow(context.Background(), `
			INSERT INTO dup_groups(kind,representative_file_id,member_count,created_at)
			VALUES($1,$2,1,$3::timestamptz)
			RETURNING id`,
			kind,
			fileID,
			fmt.Sprintf("2002-02-0%d 03:04:05+00", index+1),
		).Scan(&groupID); err != nil {
			t.Fatalf("seed M4 %s group: %v", kind, err)
		}
		if _, err := fixture.conn.Exec(context.Background(), `
			INSERT INTO dup_members(group_id,file_id,score_json)
			VALUES($1,$2,jsonb_build_object('owner','m4','sentinel',$3::int))`,
			groupID,
			fileID,
			index+10,
		); err != nil {
			t.Fatalf("seed M4 %s member: %v", kind, err)
		}
	}
}

func snapshotSmallM4(t *testing.T, fixture *smallAcceptanceFixture) []string {
	t.Helper()
	return snapshotSmallRows(t, fixture.conn, `
		SELECT concat_ws('|',
			g.id::text,
			g.kind,
			g.representative_file_id::text,
			g.member_count::text,
			to_char(g.created_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US'),
			m.file_id::text,
			m.score_json::text)
		FROM dup_groups g
		JOIN dup_members m ON m.group_id=g.id
		WHERE g.kind IN ('image','video')
		ORDER BY g.id,m.file_id`)
}

func assertSmallAcceptanceStats(t *testing.T, stats *RunStats) {
	t.Helper()
	if stats.FilesScanned != 13 ||
		stats.ImageFeatures != 4 ||
		stats.VideoFeatures != 4 ||
		stats.ExactGroups != 2 ||
		stats.ExactMembers != 5 ||
		stats.ImagePairs != 1 ||
		stats.VideoPairs != 2 ||
		stats.GroupsWritten != 5 ||
		stats.MembersWritten != 12 ||
		stats.SkippedPairs != 0 ||
		stats.BadRows != 0 {
		t.Fatalf("acceptance stats = %#v", stats)
	}
	wantStages := []string{
		"db_write",
		"exact_group",
		"image_load",
		"image_screen",
		"video_load",
		"video_screen",
	}
	gotStages := make([]string, 0, len(stats.StageElapsedMs))
	for stage := range stats.StageElapsedMs {
		gotStages = append(gotStages, stage)
	}
	sort.Strings(gotStages)
	if !reflect.DeepEqual(gotStages, wantStages) {
		t.Fatalf("stage keys = %v, want %v", gotStages, wantStages)
	}
}

type smallGroupRow struct {
	kind        string
	memberCount int
	repPath     string
	memberPath  string
	memberSHA   string
	score       string
}

func assertSmallAcceptanceGroups(t *testing.T, fixture *smallAcceptanceFixture) []string {
	t.Helper()
	rows, err := fixture.conn.Query(context.Background(), `
		SELECT
			g.kind,
			g.member_count,
			representative.path,
			member_file.path,
			member_file.sha512,
			member.score_json::text
		FROM dup_groups g
		JOIN files representative ON representative.id=g.representative_file_id
		JOIN dup_members member ON member.group_id=g.id
		JOIN files member_file ON member_file.id=member.file_id
		WHERE g.kind = ANY($1)
		ORDER BY g.kind,representative.path,member_file.path`,
		M3Kinds,
	)
	if err != nil {
		t.Fatalf("query acceptance result groups: %v", err)
	}
	defer rows.Close()

	var result []smallGroupRow
	for rows.Next() {
		var row smallGroupRow
		if err := rows.Scan(
			&row.kind,
			&row.memberCount,
			&row.repPath,
			&row.memberPath,
			&row.memberSHA,
			&row.score,
		); err != nil {
			t.Fatalf("scan acceptance result group: %v", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read acceptance result groups: %v", err)
	}
	if len(result) != 12 {
		t.Fatalf("M3 member rows = %d, want 12: %#v", len(result), result)
	}

	groupMembers := make(map[string][]string)
	groupCounts := make(map[string]int)
	snapshot := make([]string, 0, len(result))
	for _, row := range result {
		key := row.kind + "|" + row.repPath
		groupMembers[key] = append(groupMembers[key], row.memberPath)
		groupCounts[key] = row.memberCount
		assertSmallScore(t, row)
		snapshot = append(snapshot, strings.Join([]string{
			row.kind,
			row.repPath,
			fmt.Sprint(row.memberCount),
			row.memberPath,
			row.memberSHA,
			row.score,
		}, "|"))
	}
	wantMembers := map[string][]string{
		"exact|D:/a2.jpg": {
			"D:/a2.jpg",
			"E:/copy/a2.jpg",
		},
		"exact|D:/e1.bin": {
			"D:/e1.bin",
			"E:/e2.bin",
			"F:/e3.bin",
		},
		"image_candidate|D:/a1.jpg": {
			"D:/a1.jpg",
			"D:/a2.jpg",
			"E:/copy/a2.jpg",
		},
		"video_candidate|D:/v1.mp4": {
			"D:/v1.mp4",
			"D:/v2.mp4",
		},
		"video_candidate|D:/v2.mp4": {
			"D:/v2.mp4",
			"D:/v3.mp4",
		},
	}
	if !reflect.DeepEqual(groupMembers, wantMembers) {
		t.Fatalf("group members = %#v, want %#v", groupMembers, wantMembers)
	}
	if len(groupCounts) != 5 {
		t.Fatalf("M3 groups = %d, want 5: %v", len(groupCounts), groupCounts)
	}
	for key, members := range wantMembers {
		if groupCounts[key] != len(members) {
			t.Errorf("%s member_count = %d, want %d", key, groupCounts[key], len(members))
		}
	}
	return snapshot
}

func assertSmallScore(t *testing.T, row smallGroupRow) {
	t.Helper()
	var score map[string]any
	if err := json.Unmarshal([]byte(row.score), &score); err != nil {
		t.Fatalf("%s %s score_json=%q: %v", row.kind, row.memberPath, row.score, err)
	}
	if row.kind == KindExact {
		if !reflect.DeepEqual(score, map[string]any{"basis": "sha512"}) {
			t.Fatalf("%s exact score = %#v", row.memberPath, score)
		}
		return
	}

	var peer string
	var hamming int
	var duration int64
	var qualitySelf, qualityPeer int
	switch row.repPath {
	case "D:/a1.jpg":
		hamming = 3
		if row.memberSHA == smallAcceptanceSHA(1) {
			peer, qualitySelf, qualityPeer = smallAcceptanceSHA(2), 80, 90
		} else {
			peer, qualitySelf, qualityPeer = smallAcceptanceSHA(1), 90, 80
		}
	case "D:/v1.mp4":
		hamming, duration, qualitySelf, qualityPeer = 1, 1500, 70, 70
		if row.memberSHA == smallAcceptanceSHA(11) {
			peer = smallAcceptanceSHA(12)
		} else {
			peer = smallAcceptanceSHA(11)
		}
	case "D:/v2.mp4":
		hamming, duration, qualitySelf, qualityPeer = 0, 1100, 70, 70
		if row.memberSHA == smallAcceptanceSHA(12) {
			peer = smallAcceptanceSHA(13)
		} else {
			peer = smallAcceptanceSHA(12)
		}
	default:
		t.Fatalf("unexpected candidate representative %q", row.repPath)
	}
	want := map[string]any{
		"hamming":      float64(hamming),
		"peer_sha512":  peer,
		"quality_peer": float64(qualityPeer),
		"quality_self": float64(qualitySelf),
	}
	if row.kind == KindVideoCandidate {
		want["duration_diff_ms"] = float64(duration)
	}
	if !reflect.DeepEqual(score, want) {
		t.Fatalf("%s %s score = %#v, want %#v", row.kind, row.memberPath, score, want)
	}
}

func snapshotSmallRows(t *testing.T, conn *pgx.Conn, query string, args ...any) []string {
	t.Helper()
	rows, err := conn.Query(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("snapshot rows: %v", err)
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatalf("scan snapshot row: %v", err)
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read snapshot rows: %v", err)
	}
	return result
}

func smallAcceptanceSHA(value byte) string {
	raw := make([]byte, 64)
	raw[len(raw)-1] = value
	return hex.EncodeToString(raw)
}

func smallAcceptanceToken(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("crypto/rand: %v", err)
	}
	return hex.EncodeToString(raw)
}

func smallAcceptanceCentralSQL(t *testing.T) string {
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
