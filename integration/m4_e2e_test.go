//go:build windows

package integration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type m4E2EFixture struct {
	ctx      context.Context
	cancel   context.CancelFunc
	admin    *pgxpool.Pool
	scoped   *pgxpool.Pool
	binDir   string
	workDir  string
	evidence string
	runID    string
	schema   string
	dsn      string

	machineA string
	machineB string
	taskA    string
	taskB    string
	addrA    string
	addrB    string
	guiAddr  string
	configA  string
	configB  string
	guiCfg   string
	dataA    string
	dataB    string

	agentA *runningProcess
	agentB *runningProcess
	gui    *runningProcess
	client *http.Client

	normalImage  [2]string
	crashImage   [2]string
	hangImage    [2]string
	corruptImage [2]string
	normalVideo  [2]string
	corruptVideo [2]string
	sha          map[string]string
	original     map[string]fileIdentity

	publicBefore   []string
	semantic       []string
	cleaned        bool
	runtimeCleaned bool

	e1 m4E1
	e2 m4E2
	e3 m4E3
	e4 m4E4
}

type fileIdentity struct {
	size    int64
	modTime time.Time
	sha512  string
}

type m4Marker struct {
	SchemaVersion       int    `json:"schema_version"`
	RunID               string `json:"run_id"`
	Schema              string `json:"schema"`
	EvidencePath        string `json:"evidence_path"`
	Topology            string `json:"topology"`
	SecondWindowsStatus string `json:"second_windows_status"`
	E1                  m4E1   `json:"e1"`
	E2                  m4E2   `json:"e2"`
	E3                  m4E3   `json:"e3"`
	E4                  m4E4   `json:"e4"`
}

type m4E1 struct {
	Passed               bool     `json:"passed"`
	AgentIdentities      []string `json:"agent_identities"`
	AutomaticDispatch    bool     `json:"automatic_dispatch"`
	ActualNativeFeatures bool     `json:"actual_native_features"`
	PHashBlobBytes       int      `json:"phash_blob_bytes"`
	SobelBlobBytes       int      `json:"sobel_blob_bytes"`
}

type m4E2 struct {
	Passed         bool   `json:"passed"`
	VideoFramesA   int    `json:"video_frames_a"`
	VideoFramesB   int    `json:"video_frames_b"`
	ImageVerdict   string `json:"image_verdict"`
	VideoVerdict   string `json:"video_verdict"`
	GroupDetailAPI bool   `json:"group_detail_api"`
}

type m4E3 struct {
	Passed                    bool `json:"passed"`
	GUIRestartRecovery        bool `json:"gui_restart_recovery"`
	CorruptSurvived           bool `json:"corrupt_survived"`
	TimeoutSurvived           bool `json:"timeout_survived"`
	WorkerCrashSurvived       bool `json:"worker_crash_survived"`
	RemainingSamplesCompleted bool `json:"remaining_samples_completed"`
}

type m4E4 struct {
	Passed            bool `json:"passed"`
	IdempotentRerun   bool `json:"idempotent_rerun"`
	PublicUnchanged   bool `json:"public_unchanged"`
	CleanupResidual   int  `json:"cleanup_residual"`
	CentralSQLRuns    int  `json:"central_sql_runs"`
	UserMediaModified bool `json:"user_media_modified"`
}

func openM4E2EFixture(t *testing.T) *m4E2EFixture {
	t.Helper()
	binDir := os.Getenv("M4_E2E_BIN_DIR")
	if binDir == "" {
		binDir = os.Getenv("DEDUP_TEST_M4_BIN_DIR")
	}
	adminDSN := os.Getenv("DEDUP_TEST_PG_DSN")
	if binDir == "" || adminDSN == "" {
		t.Skip("set M4_E2E_BIN_DIR and DEDUP_TEST_PG_DSN to run M4 E2E")
	}
	runID := os.Getenv("M4_VERIFY_RUN_ID")
	evidence := os.Getenv("M4_EVIDENCE_PATH")
	if runID == "" || evidence == "" {
		t.Fatal("M4_VERIFY_RUN_ID and M4_EVIDENCE_PATH are required for enabled M4 E2E")
	}
	evidence, err := filepath.Abs(evidence)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(evidence)
	if err != nil || !info.IsDir() {
		t.Fatalf("M4 evidence directory %q: %v", evidence, err)
	}
	for _, name := range []string{
		"agent.exe", "gui.exe", "worker.exe", "mediacore.dll",
		filepath.Join("tools", "ffmpeg.exe"),
		filepath.Join("tools", "ffprobe.exe"),
	} {
		path := filepath.Join(binDir, name)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("required staged binary %s: %v", path, err)
		}
	}

	suffix := randomHex(t, 12)
	schema := "m4_e2e_" + suffix
	scopedDSN := m4ScopedDSN(t, adminDSN, schema)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := admin.Ping(ctx); err != nil {
		admin.Close()
		cancel()
		t.Fatal(err)
	}
	publicBefore := m4PublicSchemaSnapshot(t, admin)
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+quotedSchema); err != nil {
		admin.Close()
		cancel()
		t.Fatal(err)
	}
	scoped, err := pgxpool.New(ctx, scopedDSN)
	if err != nil {
		_, _ = admin.Exec(context.Background(), `DROP SCHEMA `+quotedSchema+` CASCADE`)
		admin.Close()
		cancel()
		t.Fatal(err)
	}
	schemaSQL, err := os.ReadFile(filepath.Join("..", "deploy", "central.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for run := 1; run <= 2; run++ {
		if _, err := scoped.Exec(ctx, string(schemaSQL)); err != nil {
			t.Fatalf("central.sql run %d: %v", run, err)
		}
	}
	var serverVersion int
	var currentSchema string
	if err := scoped.QueryRow(ctx,
		`SELECT current_setting('server_version_num')::int,current_schema()`,
	).Scan(&serverVersion, &currentSchema); err != nil {
		t.Fatal(err)
	}
	if serverVersion < 160000 || serverVersion >= 170000 ||
		currentSchema != schema {
		t.Fatalf("PostgreSQL version/schema=%d/%q, want 16/%q",
			serverVersion, currentSchema, schema)
	}

	workDir, err := os.MkdirTemp("", "dedup-m4-e2e-"+suffix+"-")
	if err != nil {
		t.Fatal(err)
	}
	if !m4PathWithin(os.TempDir(), workDir) ||
		m4PathWithin(evidence, workDir) {
		_ = os.RemoveAll(workDir)
		t.Fatalf(
			"M4 runtime directory must be under system temp and outside evidence: %s",
			workDir,
		)
	}
	fixture := &m4E2EFixture{
		ctx:          ctx,
		cancel:       cancel,
		admin:        admin,
		scoped:       scoped,
		binDir:       binDir,
		workDir:      workDir,
		evidence:     evidence,
		runID:        runID,
		schema:       schema,
		dsn:          scopedDSN,
		machineA:     "m4-local-agent-a-" + suffix[:8],
		machineB:     "m4-local-agent-b-" + suffix[:8],
		taskA:        m4UUID(t),
		taskB:        m4UUID(t),
		addrA:        freeAddress(t),
		addrB:        freeAddress(t),
		guiAddr:      freeAddress(t),
		configA:      filepath.Join(workDir, "agent-a.json"),
		configB:      filepath.Join(workDir, "agent-b.json"),
		guiCfg:       filepath.Join(workDir, "gui.json"),
		dataA:        filepath.Join(workDir, "agent-data-a"),
		dataB:        filepath.Join(workDir, "agent-data-b"),
		client:       &http.Client{Timeout: 3 * time.Second},
		sha:          make(map[string]string),
		original:     make(map[string]fileIdentity),
		publicBefore: publicBefore,
	}
	fixture.createMedia(t)
	fixture.writeConfigs(t, false)
	t.Cleanup(func() { fixture.cleanup(t) })
	return fixture
}

func (fixture *m4E2EFixture) runE1(t *testing.T) {
	agentExe := filepath.Join(fixture.binDir, "agent.exe")
	guiExe := filepath.Join(fixture.binDir, "gui.exe")
	fixture.agentA = startProcess(t, fixture.ctx, agentExe, fixture.configA)
	fixture.agentB = startProcess(t, fixture.ctx, agentExe, fixture.configB)
	fixture.gui = startProcess(t, fixture.ctx, guiExe, fixture.guiCfg)
	baseURL := "http://" + fixture.guiAddr
	waitFor(t, fixture.ctx, "both M4 Agents online",
		fixture.processes(), func() (bool, error) {
			return allAgentsOnline(
				fixture.client, baseURL, fixture.machineA, fixture.machineB,
			)
		})
	postScan(t, fixture.client, baseURL,
		fixture.taskA, fixture.machineA, filepath.Join(fixture.workDir, "media-a"))
	postScan(t, fixture.client, baseURL,
		fixture.taskB, fixture.machineB, filepath.Join(fixture.workDir, "media-b"))
	waitFor(t, fixture.ctx, "M4 phase-1 scans complete",
		fixture.processes(), func() (bool, error) {
			return tasksDone(fixture.client, baseURL, fixture.taskA, fixture.taskB)
		})
	waitFor(t, fixture.ctx, "all M4 phase-1 rows synchronize",
		fixture.processes(), func() (bool, error) {
			var files, images, videos int
			err := fixture.scoped.QueryRow(fixture.ctx, `
				SELECT
				  (SELECT count(*) FROM files
				   WHERE machine_id IN ($1,$2) AND phase1_done=1),
				  (SELECT count(*) FROM image_features
				   WHERE pdq256 IS NOT NULL),
				  (SELECT count(*) FROM video_features
				   WHERE duration_ms IS NOT NULL
				     AND thumb_pdq256 IS NOT NULL)`,
				fixture.machineA, fixture.machineB,
			).Scan(&files, &images, &videos)
			return files == 12 && images == 8 && videos == 4, err
		})

	fixture.agentA.stop()
	fixture.agentB.stop()
	fixture.agentA, fixture.agentB = nil, nil
	m4StartAnalysis(t, fixture.client, baseURL)
	m4WaitAnalysis(t, fixture.ctx, fixture.client, baseURL, fixture.processes())
	var (
		phase2Tasks int
		machines    []string
	)
	if err := fixture.scoped.QueryRow(fixture.ctx, `
		SELECT count(*),array_agg(DISTINCT machine_id ORDER BY machine_id)
		FROM scan_tasks
		WHERE phase=2 AND status='sent'`,
	).Scan(&phase2Tasks, &machines); err != nil {
		t.Fatal(err)
	}
	wantMachines := []string{fixture.machineA, fixture.machineB}
	if phase2Tasks < 2 || !reflect.DeepEqual(machines, wantMachines) {
		t.Fatalf("automatic phase2 durable tasks=%d machines=%v, want >=2/%v",
			phase2Tasks, machines, wantMachines)
	}

	fixture.gui.stop()
	fixture.gui = startProcess(t, fixture.ctx, guiExe, fixture.guiCfg)
	waitFor(t, fixture.ctx, "GUI restart restores pending phase2 rows",
		fixture.processes(), func() (bool, error) {
			var count int
			err := fixture.scoped.QueryRow(fixture.ctx, `
				SELECT count(*) FROM scan_tasks
				WHERE phase=2 AND status IN ('sent','acked','running')`,
			).Scan(&count)
			return count == phase2Tasks, err
		})

	fixture.corruptInPlace(t, fixture.corruptImage[1])
	fixture.corruptInPlace(t, fixture.corruptVideo[1])
	fixture.writeConfigs(t, true)
	fixture.agentA = startProcess(t, fixture.ctx, agentExe, fixture.configA)
	fixture.agentB = startProcess(t, fixture.ctx, agentExe, fixture.configB)
	waitFor(t, fixture.ctx, "M4 Agents reconnect and resume phase2",
		fixture.processes(), func() (bool, error) {
			return allAgentsOnline(
				fixture.client, baseURL, fixture.machineA, fixture.machineB,
			)
		})
	waitFor(t, fixture.ctx, "all automatic phase2 tasks become terminal",
		fixture.processes(), func() (bool, error) {
			var active int
			err := fixture.scoped.QueryRow(fixture.ctx, `
				SELECT count(*) FROM scan_tasks
				WHERE phase=2 AND status <> 'done'`,
			).Scan(&active)
			return active == 0, err
		})
	waitFor(t, fixture.ctx, "normal native image phase2 features synchronize",
		fixture.processes(), func() (bool, error) {
			var count, pHashBytes, sobelBytes int
			err := fixture.scoped.QueryRow(fixture.ctx, `
				SELECT count(*),min(octet_length(phash_parts)),
				       min(octet_length(sobel_hist))
				FROM image_features
				WHERE sha512=ANY($1::text[])
				  AND phash_parts IS NOT NULL
				  AND sobel_hist IS NOT NULL`,
				[]string{
					fixture.sha[fixture.normalImage[0]],
					fixture.sha[fixture.normalImage[1]],
				},
			).Scan(&count, &pHashBytes, &sobelBytes)
			return count == 2 && pHashBytes == 76 && sobelBytes == 516, err
		})
	fixture.e1 = m4E1{
		Passed:               true,
		AgentIdentities:      wantMachines,
		AutomaticDispatch:    true,
		ActualNativeFeatures: true,
		PHashBlobBytes:       76,
		SobelBlobBytes:       516,
	}
}

func (fixture *m4E2EFixture) runE2(t *testing.T) {
	if !fixture.e1.Passed {
		t.Skip("E1 did not complete")
	}
	baseURL := "http://" + fixture.guiAddr
	imageA, imageB := m4NormalizedPair(
		fixture.sha[fixture.normalImage[0]],
		fixture.sha[fixture.normalImage[1]],
	)
	videoA, videoB := m4NormalizedPair(
		fixture.sha[fixture.normalVideo[0]],
		fixture.sha[fixture.normalVideo[1]],
	)
	waitFor(t, fixture.ctx, "normal M4 pair scores finalize",
		fixture.processes(), func() (bool, error) {
			var count int
			err := fixture.scoped.QueryRow(fixture.ctx, `
				SELECT count(*) FROM pair_scores
				WHERE verdict='yes'
				  AND ((kind='image' AND sha_a=$1 AND sha_b=$2)
				    OR (kind='video' AND sha_a=$3 AND sha_b=$4))`,
				imageA, imageB, videoA, videoB,
			).Scan(&count)
			return count == 2, err
		})
	var imageVerdict, videoVerdict string
	var videoFramesJSON int
	if err := fixture.scoped.QueryRow(fixture.ctx, `
		SELECT verdict FROM pair_scores
		WHERE kind='image' AND sha_a=$1 AND sha_b=$2`,
		imageA, imageB,
	).Scan(&imageVerdict); err != nil {
		t.Fatal(err)
	}
	if err := fixture.scoped.QueryRow(fixture.ctx, `
		SELECT verdict,jsonb_array_length(phase2_json->'video'->'frames')
		FROM pair_scores
		WHERE kind='video' AND sha_a=$1 AND sha_b=$2`,
		videoA, videoB,
	).Scan(&videoVerdict, &videoFramesJSON); err != nil {
		t.Fatal(err)
	}
	var framesA, framesB int
	if err := fixture.scoped.QueryRow(fixture.ctx, `
		SELECT
		  count(*) FILTER (WHERE sha512=$1),
		  count(*) FILTER (WHERE sha512=$2)
		FROM video_frames
		WHERE sha512 IN ($1,$2)
		  AND octet_length(pdq256)=32
		  AND octet_length(phash_parts)=76
		  AND octet_length(sobel_hist)=516`,
		videoA, videoB,
	).Scan(&framesA, &framesB); err != nil {
		t.Fatal(err)
	}
	if imageVerdict != "yes" || videoVerdict != "yes" ||
		framesA != 6 || framesB != 6 || videoFramesJSON != 6 {
		t.Fatalf("phase2 results image=%q video=%q frames=%d/%d json=%d",
			imageVerdict, videoVerdict, framesA, framesB, videoFramesJSON)
	}

	for _, pair := range []struct {
		kind string
		a    string
		b    string
	}{
		{"image", imageA, imageB},
		{"video", videoA, videoB},
	} {
		var groupID int64
		if err := fixture.scoped.QueryRow(fixture.ctx, `
			SELECT g.id
			FROM dup_groups g
			JOIN dup_members m ON m.group_id=g.id
			JOIN files f ON f.id=m.file_id
			WHERE g.kind=$1 AND f.sha512 IN ($2,$3)
			GROUP BY g.id
			HAVING count(DISTINCT f.sha512)=2
			ORDER BY g.id LIMIT 1`,
			pair.kind, pair.a, pair.b,
		).Scan(&groupID); err != nil {
			t.Fatalf("%s confirmed group: %v", pair.kind, err)
		}
		response, err := fixture.client.Get(
			fmt.Sprintf("%s/api/groups/%d", baseURL, groupID),
		)
		if err != nil {
			t.Fatal(err)
		}
		var detail struct {
			Kind    string `json:"kind"`
			Members []struct {
				Score json.RawMessage `json:"score_json"`
			} `json:"members"`
		}
		decodeErr := json.NewDecoder(response.Body).Decode(&detail)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || decodeErr != nil ||
			detail.Kind != pair.kind || len(detail.Members) < 2 {
			t.Fatalf("%s group detail status=%d detail=%#v err=%v",
				pair.kind, response.StatusCode, detail, decodeErr)
		}
		hasScore := false
		for _, member := range detail.Members {
			if string(member.Score) != "null" && len(member.Score) != 0 {
				hasScore = true
			}
		}
		if !hasScore {
			t.Fatalf("%s group detail has no score JSON", pair.kind)
		}
	}
	fixture.e2 = m4E2{
		Passed:         true,
		VideoFramesA:   framesA,
		VideoFramesB:   framesB,
		ImageVerdict:   imageVerdict,
		VideoVerdict:   videoVerdict,
		GroupDetailAPI: true,
	}
}

func (fixture *m4E2EFixture) runE3(t *testing.T) {
	if !fixture.e2.Passed {
		t.Skip("E2 did not complete")
	}
	baseURL := "http://" + fixture.guiAddr
	online, err := allAgentsOnline(
		fixture.client, baseURL, fixture.machineA, fixture.machineB,
	)
	if err != nil || !online {
		t.Fatalf("Agents did not survive negative paths: online=%t err=%v", online, err)
	}
	var (
		inconclusive int
		normalYes    int
	)
	if err := fixture.scoped.QueryRow(fixture.ctx, `
		SELECT
		  count(*) FILTER (WHERE verdict='inconclusive'),
		  count(*) FILTER (WHERE verdict='yes')
		FROM pair_scores`,
	).Scan(&inconclusive, &normalYes); err != nil {
		t.Fatal(err)
	}
	if inconclusive < 3 || normalYes < 2 {
		t.Fatalf("negative/normal verdict counts inconclusive=%d yes=%d",
			inconclusive, normalYes)
	}
	errorsText := m4ReadLogs(t,
		filepath.Join(fixture.dataA, "errors.log"),
		filepath.Join(fixture.dataB, "errors.log"),
	)
	crashText := m4ReadLogs(t,
		filepath.Join(fixture.dataA, "crash.log"),
		filepath.Join(fixture.dataB, "crash.log"),
	)
	corruptSurvived := strings.Contains(errorsText, filepath.Base(fixture.corruptImage[1])) &&
		strings.Contains(errorsText, filepath.Base(fixture.corruptVideo[1]))
	timeoutSurvived := strings.Contains(crashText, "__hang__") &&
		strings.Contains(strings.ToLower(crashText), "watchdog")
	crashSurvived := strings.Contains(crashText, "__crash__")
	if !corruptSurvived || !timeoutSurvived || !crashSurvived {
		t.Fatalf("negative evidence corrupt=%t timeout=%t crash=%t errors=%q crashes=%q",
			corruptSurvived, timeoutSurvived, crashSurvived, errorsText, crashText)
	}
	fixture.e3 = m4E3{
		Passed:                    true,
		GUIRestartRecovery:        true,
		CorruptSurvived:           true,
		TimeoutSurvived:           true,
		WorkerCrashSurvived:       true,
		RemainingSamplesCompleted: normalYes >= 2,
	}
}

func (fixture *m4E2EFixture) runE4(t *testing.T) {
	if !fixture.e3.Passed {
		t.Skip("E3 did not complete")
	}
	fixture.semantic = m4SemanticSnapshot(t, fixture.scoped)
	baseURL := "http://" + fixture.guiAddr
	m4StartAnalysis(t, fixture.client, baseURL)
	m4WaitAnalysis(t, fixture.ctx, fixture.client, baseURL, fixture.processes())
	waitFor(t, fixture.ctx, "idempotent M4 rerun settles",
		fixture.processes(), func() (bool, error) {
			var active int
			err := fixture.scoped.QueryRow(fixture.ctx, `
				SELECT count(*) FROM scan_tasks
				WHERE phase=2 AND status <> 'done'`,
			).Scan(&active)
			return active == 0, err
		})
	after := m4SemanticSnapshot(t, fixture.scoped)
	if !reflect.DeepEqual(after, fixture.semantic) {
		t.Fatalf("M4 semantic state changed after rerun:\nbefore=%v\nafter=%v",
			fixture.semantic, after)
	}
	fixture.stopProcesses()
	fixture.scoped.Close()
	fixture.scoped = nil
	quotedSchema := pgx.Identifier{fixture.schema}.Sanitize()
	if _, err := fixture.admin.Exec(
		context.Background(), `DROP SCHEMA `+quotedSchema+` CASCADE`,
	); err != nil {
		t.Fatal(err)
	}
	var residual int
	if err := fixture.admin.QueryRow(context.Background(), `
		SELECT count(*) FROM pg_namespace WHERE nspname=$1`,
		fixture.schema,
	).Scan(&residual); err != nil {
		t.Fatal(err)
	}
	publicAfter := m4PublicSchemaSnapshot(t, fixture.admin)
	if !reflect.DeepEqual(publicAfter, fixture.publicBefore) {
		t.Fatalf("public schema changed:\nbefore=%v\nafter=%v",
			fixture.publicBefore, publicAfter)
	}
	fixture.assertExpectedMediaState(t)
	if err := os.RemoveAll(fixture.workDir); err != nil {
		t.Fatalf("remove M4 runtime directory: %v", err)
	}
	if _, err := os.Stat(fixture.workDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("M4 runtime directory still exists: %v", err)
	}
	fixture.runtimeCleaned = true
	findings, err := m4CredentialURIFindings(fixture.evidence)
	if err != nil {
		t.Fatalf("scan M4 evidence for credentials: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("credential-bearing URI found in M4 evidence: %v", findings)
	}
	fixture.cleaned = true
	fixture.e4 = m4E4{
		Passed:            true,
		IdempotentRerun:   true,
		PublicUnchanged:   true,
		CleanupResidual:   residual,
		CentralSQLRuns:    2,
		UserMediaModified: false,
	}
}

func (fixture *m4E2EFixture) emitMarker(t *testing.T) {
	t.Helper()
	if !fixture.e1.Passed || !fixture.e2.Passed ||
		!fixture.e3.Passed || !fixture.e4.Passed {
		t.Fatal("M4 acceptance marker refused because E1-E4 did not all pass")
	}
	marker := m4Marker{
		SchemaVersion:       1,
		RunID:               fixture.runID,
		Schema:              fixture.schema,
		EvidencePath:        fixture.evidence,
		Topology:            "SINGLE_WINDOWS_TWO_LOCAL_AGENT_IDENTITIES",
		SecondWindowsStatus: "USER_WAIVED",
		E1:                  fixture.e1,
		E2:                  fixture.e2,
		E3:                  fixture.e3,
		E4:                  fixture.e4,
	}
	raw, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("M4_ACCEPTANCE %s\n", raw)
}

func (fixture *m4E2EFixture) createMedia(t *testing.T) {
	t.Helper()
	rootA := filepath.Join(fixture.workDir, "media-a")
	rootB := filepath.Join(fixture.workDir, "media-b")
	for _, root := range []string{rootA, rootB} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fixture.normalImage = m4CreateImagePair(t, rootA, rootB, "normal", 11)
	fixture.crashImage = m4CreateImagePair(t, rootA, rootB, "__crash__", 29)
	fixture.hangImage = m4CreateImagePair(t, rootA, rootB, "__hang__", 47)
	fixture.corruptImage = m4CreateImagePair(t, rootA, rootB, "corrupt", 71)
	ffmpeg := filepath.Join(fixture.binDir, "tools", "ffmpeg.exe")
	fixture.normalVideo = m4CreateVideoPair(
		t, ffmpeg, rootA, rootB, "normal-video", "testsrc2",
	)
	fixture.corruptVideo = m4CreateVideoPair(
		t, ffmpeg, rootA, rootB, "corrupt-video", "smptebars",
	)
	for _, path := range []string{
		fixture.normalImage[0], fixture.normalImage[1],
		fixture.crashImage[0], fixture.crashImage[1],
		fixture.hangImage[0], fixture.hangImage[1],
		fixture.corruptImage[0], fixture.corruptImage[1],
		fixture.normalVideo[0], fixture.normalVideo[1],
		fixture.corruptVideo[0], fixture.corruptVideo[1],
	} {
		fixture.sha[path] = m4HashFile(t, path)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		fixture.original[path] = fileIdentity{
			size: info.Size(), modTime: info.ModTime(), sha512: fixture.sha[path],
		}
	}
	for _, pair := range [][2]string{
		fixture.normalImage, fixture.crashImage, fixture.hangImage,
		fixture.corruptImage, fixture.normalVideo, fixture.corruptVideo,
	} {
		if fixture.sha[pair[0]] == fixture.sha[pair[1]] {
			t.Fatalf("fixture pair unexpectedly has identical SHA-512: %v", pair)
		}
	}
}

func (fixture *m4E2EFixture) writeConfigs(t *testing.T, crashInjection bool) {
	t.Helper()
	workerExe := filepath.Join(fixture.binDir, "worker.exe")
	agent := func(machine, addr, data string) map[string]any {
		cfg := agentConfig(machine, addr, data, fixture.dsn)
		cfg["worker"] = map[string]any{
			"count": 3, "exe_path": workerExe,
			"image_timeout_s": 3, "video_timeout_s": 30,
			"image_memory_mb": 256, "respawn_delay_ms": 100,
			"crash_injection": crashInjection,
		}
		return cfg
	}
	writeJSON(t, fixture.configA,
		agent(fixture.machineA, fixture.addrA, fixture.dataA))
	writeJSON(t, fixture.configB,
		agent(fixture.machineB, fixture.addrB, fixture.dataB))
	writeJSON(t, fixture.guiCfg, map[string]any{
		"listen_addr": fixture.guiAddr,
		"pg_dsn":      fixture.dsn,
		"heartbeat_s": 1,
		"agents": []map[string]string{
			{"machine_id": fixture.machineA, "addr": fixture.addrA},
			{"machine_id": fixture.machineB, "addr": fixture.addrB},
		},
		"firstscreen": map[string]any{
			"hamming_max": 0, "aspect_tolerance": 0.0,
			"video_duration_window_ms": 100,
			"image_quality_min":        0, "read_page_size": 100,
			"group_insert_batch": 100, "sha_resolve_chunk": 100,
		},
		"phase2": map[string]any{
			"phash_pass_t2": 0.80, "phash_part_threshold": 10,
			"sobel_t3": 0.85, "video_frames": 6,
			"video_avg_t4": 0.80, "video_min_passed": 4,
			"video_min_valid": 4, "video_file_timeout_s": 60,
			"video_frame_command_timeout_s": 15,
			"image_file_timeout_s":          10, "task_shard_size": 100,
			"auto_dispatch": true,
		},
	})
}

func (fixture *m4E2EFixture) corruptInPlace(t *testing.T, path string) {
	t.Helper()
	identity := fixture.original[path]
	if identity.size < 32 {
		t.Fatalf("fixture %s is too small to corrupt", path)
	}
	data := bytes.Repeat([]byte{0xff, 0x00, 0x7f, 0x13}, int(identity.size/4)+1)
	if err := os.WriteFile(path, data[:identity.size], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, identity.modTime, identity.modTime); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != identity.size ||
		info.ModTime().UnixMilli() != identity.modTime.UnixMilli() {
		t.Fatalf("corrupt fixture identity changed for %s", path)
	}
	identity.sha512 = m4HashFile(t, path)
	fixture.original[path] = identity
}

func (fixture *m4E2EFixture) assertExpectedMediaState(t *testing.T) {
	t.Helper()
	for path, expected := range fixture.original {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat expected media %s: %v", path, err)
		}
		actualSHA := m4HashFile(t, path)
		if info.Size() != expected.size ||
			info.ModTime().UnixMilli() != expected.modTime.UnixMilli() ||
			actualSHA != expected.sha512 {
			t.Fatalf(
				"media changed path=%s size=%d/%d mtime=%d/%d sha=%s/%s",
				path, info.Size(), expected.size,
				info.ModTime().UnixMilli(), expected.modTime.UnixMilli(),
				actualSHA, expected.sha512,
			)
		}
	}
}

func (fixture *m4E2EFixture) processes() []*runningProcess {
	var result []*runningProcess
	for _, process := range []*runningProcess{
		fixture.agentA, fixture.agentB, fixture.gui,
	} {
		if process != nil {
			result = append(result, process)
		}
	}
	return result
}

func (fixture *m4E2EFixture) stopProcesses() {
	for _, process := range []*runningProcess{
		fixture.agentA, fixture.agentB, fixture.gui,
	} {
		if process != nil {
			process.stop()
		}
	}
	fixture.agentA, fixture.agentB, fixture.gui = nil, nil, nil
}

func (fixture *m4E2EFixture) cleanup(t *testing.T) {
	t.Helper()
	fixture.stopProcesses()
	if fixture.scoped != nil {
		fixture.scoped.Close()
		fixture.scoped = nil
	}
	if fixture.admin != nil {
		if !fixture.cleaned &&
			regexp.MustCompile(`^m4_e2e_[a-z0-9_]{8,96}$`).MatchString(fixture.schema) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			_, err := fixture.admin.Exec(
				ctx,
				`DROP SCHEMA `+pgx.Identifier{fixture.schema}.Sanitize()+` CASCADE`,
			)
			cancel()
			if err != nil {
				t.Errorf("cleanup M4 schema: %v", err)
			}
		}
		fixture.admin.Close()
		fixture.admin = nil
	}
	if fixture.cancel != nil {
		fixture.cancel()
	}
	if !fixture.runtimeCleaned && fixture.workDir != "" {
		if err := os.RemoveAll(fixture.workDir); err != nil {
			t.Errorf("cleanup M4 runtime directory: %v", err)
		}
	}
}

func m4CreateImagePair(
	t *testing.T,
	rootA, rootB, name string,
	seed uint8,
) [2]string {
	t.Helper()
	imageA := filepath.Join(rootA, name+"-a.png")
	imageB := filepath.Join(rootB, name+"-b.png")
	canvas := image.NewRGBA(image.Rect(0, 0, 128, 96))
	for y := 0; y < 96; y++ {
		for x := 0; x < 128; x++ {
			block := uint8(((x/8)*17 + (y/8)*31 + int(seed)) & 0xff)
			canvas.SetRGBA(x, y, color.RGBA{
				R: block ^ seed,
				G: uint8((x*3 + y*5 + int(seed)) & 0xff),
				B: uint8((x*y + int(seed)*11) & 0xff),
				A: 0xff,
			})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, canvas); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imageA, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	second := append(append([]byte(nil), encoded.Bytes()...), seed)
	if err := os.WriteFile(imageB, second, 0o600); err != nil {
		t.Fatal(err)
	}
	return [2]string{imageA, imageB}
}

func m4CreateVideoPair(
	t *testing.T,
	ffmpeg, rootA, rootB, name, source string,
) [2]string {
	t.Helper()
	videoA := filepath.Join(rootA, name+"-a.mp4")
	videoB := filepath.Join(rootB, name+"-b.mp4")
	filter := source + "=size=320x240:rate=12:duration=3"
	command := exec.Command(
		ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", filter,
		"-an", "-c:v", "mpeg4", "-q:v", "3", "-pix_fmt", "yuv420p",
		videoA,
	)
	command.SysProcAttr = m4HiddenProcessAttributes()
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generate video %s: %v\n%s", name, err, output)
	}
	data, err := os.ReadFile(videoA)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte("\nM4-"+name)...)
	if err := os.WriteFile(videoB, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return [2]string{videoA, videoB}
}

func m4HiddenProcessAttributes() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		HideWindow: true, CreationFlags: 0x08000000,
	}
}

func m4StartAnalysis(t *testing.T, client *http.Client, baseURL string) {
	t.Helper()
	response, err := client.Post(
		baseURL+"/api/analysis/firstscreen/run",
		"application/json",
		bytes.NewReader(nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("start firstscreen status=%d body=%s", response.StatusCode, body)
	}
}

func m4WaitAnalysis(
	t *testing.T,
	ctx context.Context,
	client *http.Client,
	baseURL string,
	processes []*runningProcess,
) {
	t.Helper()
	waitFor(t, ctx, "firstscreen analysis and phase2 hook",
		processes, func() (bool, error) {
			response, err := client.Get(baseURL + "/api/analysis/firstscreen/status")
			if err != nil {
				return false, nil
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				return false, nil
			}
			var status struct {
				Running bool            `json:"running"`
				Last    json.RawMessage `json:"last"`
				LastErr string          `json:"last_err"`
			}
			if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
				return false, err
			}
			if status.Running || len(status.Last) == 0 ||
				string(status.Last) == "null" {
				return false, nil
			}
			if status.LastErr != "" {
				return false, fmt.Errorf("firstscreen analysis: %s", status.LastErr)
			}
			return true, nil
		})
}

func m4SemanticSnapshot(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT value FROM (
		  SELECT
		    'pair|' || kind || '|' || sha_a || '|' || sha_b || '|' ||
		    verdict || '|' || phase2_json::text AS value
		  FROM pair_scores
		  UNION ALL
		  SELECT
		    'group|' || g.kind || '|' ||
		    COALESCE(rep.machine_id,'') || '|' || COALESCE(rep.path,'') || '|' ||
		    string_agg(
		      f.machine_id || '|' || f.path || '|' ||
		      COALESCE(m.score_json::text,'null'),
		      E'\n' ORDER BY f.machine_id,f.path,f.id
		    ) AS value
		  FROM dup_groups g
		  JOIN dup_members m ON m.group_id=g.id
		  JOIN files f ON f.id=m.file_id
		  LEFT JOIN files rep ON rep.id=g.representative_file_id
		  WHERE g.kind IN ('image','video')
		  GROUP BY g.id,g.kind,rep.machine_id,rep.path
		) snapshot
		ORDER BY value`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func m4PublicSchemaSnapshot(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("rollback public snapshot: %v", err)
		}
	}()
	if _, err := tx.Exec(ctx, `SET LOCAL search_path TO pg_catalog`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, `
		SELECT kind || E'\t' || object_name || E'\t' || definition
		FROM (
		  SELECT 'relation' AS kind,c.relname AS object_name,
		         c.relkind::text AS definition
		  FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		  WHERE n.nspname='public'
		  UNION ALL
		  SELECT 'column',c.relname || '.' || a.attname,
		         format_type(a.atttypid,a.atttypmod)
		         || '|notnull=' || a.attnotnull::text
		         || '|default=' || COALESCE(pg_get_expr(d.adbin,d.adrelid),'')
		  FROM pg_attribute a
		  JOIN pg_class c ON c.oid=a.attrelid
		  JOIN pg_namespace n ON n.oid=c.relnamespace
		  LEFT JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum
		  WHERE n.nspname='public' AND a.attnum>0 AND NOT a.attisdropped
		  UNION ALL
		  SELECT 'constraint',c.relname || '.' || x.conname,
		         pg_get_constraintdef(x.oid,true)
		  FROM pg_constraint x
		  JOIN pg_class c ON c.oid=x.conrelid
		  JOIN pg_namespace n ON n.oid=c.relnamespace
		  WHERE n.nspname='public'
		  UNION ALL
		  SELECT 'index',c.relname,pg_get_indexdef(c.oid)
		  FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		  WHERE n.nspname='public' AND c.relkind='i'
		) snapshot ORDER BY kind,object_name,definition`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func m4ScopedDSN(t *testing.T, dsn, schema string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	values := parsed.Query()
	values.Set("search_path", schema)
	parsed.RawQuery = values.Encode()
	return parsed.String()
}

func m4NormalizedPair(a, b string) (string, string) {
	if a < b {
		return a, b
	}
	return b, a
}

func m4HashFile(t *testing.T, path string) string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	sum := sha512.New()
	if _, err := io.Copy(sum, file); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(sum.Sum(nil))
}

func m4ReadLogs(t *testing.T, paths ...string) string {
	t.Helper()
	sort.Strings(paths)
	var result strings.Builder
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatalf("read log %s: %v", path, err)
		}
		result.Write(data)
		result.WriteByte('\n')
	}
	return result.String()
}

func randomHex(t *testing.T, bytesCount int) string {
	t.Helper()
	data := make([]byte, bytesCount)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(data)
}

func m4UUID(t *testing.T) string {
	t.Helper()
	raw := randomHex(t, 16)
	return fmt.Sprintf("%s-%s-4%s-8%s-%s",
		raw[:8], raw[8:12], raw[13:16], raw[17:20], raw[20:32])
}

func TestM4E2EWhenEnabled(t *testing.T) {
	fixture := openM4E2EFixture(t)
	t.Run("E1_AutomaticDispatchAndNativeFeatures", fixture.runE1)
	t.Run("E2_SixFramesVerdictsAndGroupDetailAPI", fixture.runE2)
	t.Run("E3_RestartCorruptTimeoutAndWorkerCrashSurvival", fixture.runE3)
	t.Run("E4_IdempotentPublicUnchangedAndCleanup", fixture.runE4)
	fixture.emitMarker(t)
}

func TestM4VerifierPreflightWritesFailSummaryAndNotRunGates(t *testing.T) {
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	pwsh, err := exec.LookPath("pwsh.exe")
	if err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing.exe")
	command := exec.Command(
		pwsh,
		"-NoLogo",
		"-NoProfile",
		"-File", filepath.Join(repoRoot, "scripts", "verify_m4.ps1"),
		"-Go", missing,
		"-GCC", missing,
		"-PGDSN", "postgres://invalid.invalid/secret",
	)
	command.Dir = repoRoot
	output, runErr := command.CombinedOutput()
	if runErr == nil {
		t.Fatalf("M4 verifier preflight unexpectedly exited zero:\n%s", output)
	}
	text := string(output)
	if strings.Contains(text, "postgres://invalid.invalid/secret") {
		t.Fatalf("M4 verifier leaked the supplied DSN:\n%s", text)
	}
	if !strings.Contains(text, "M4 FINAL RESULT FAIL") {
		t.Fatalf("M4 verifier did not emit final FAIL:\n%s", text)
	}
	summaryPath := m4SummaryPathFromOutput(t, text)
	summaryDir := filepath.Dir(summaryPath)
	t.Cleanup(func() {
		evidenceRoot := filepath.Join(repoRoot, ".superpowers", "evidence")
		if m4PathWithin(evidenceRoot, summaryDir) &&
			strings.HasPrefix(filepath.Base(summaryDir), "m4-") {
			if err := os.RemoveAll(summaryDir); err != nil {
				t.Errorf("remove verifier preflight evidence: %v", err)
			}
		}
	})
	data, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatalf("read M4 failure summary: %v\n%s", err, text)
	}
	var summary struct {
		Status        string   `json:"status"`
		RequiredGates []string `json:"required_gates"`
		Gates         map[string]struct {
			Status   string `json:"status"`
			ExitCode *int   `json:"exit_code"`
			Log      string `json:"log"`
		} `json:"gates"`
	}
	if err := json.Unmarshal(data, &summary); err != nil {
		t.Fatalf("decode M4 failure summary: %v", err)
	}
	if summary.Status != "FAIL" || len(summary.RequiredGates) == 0 {
		t.Fatalf("M4 failure summary=%#v", summary)
	}
	for _, name := range summary.RequiredGates {
		gate, ok := summary.Gates[name]
		if !ok || gate.Status != "NOT_RUN" ||
			gate.ExitCode != nil || gate.Log != "" {
			t.Fatalf("preflight gate %q=%#v, want NOT_RUN/null/empty", name, gate)
		}
	}
}

func m4SummaryPathFromOutput(t *testing.T, output string) string {
	t.Helper()
	match := regexp.MustCompile(
		`(?m)^M4 FINAL RESULT FAIL .* evidence=([^\r\n ]+)`,
	).FindStringSubmatch(output)
	if len(match) != 2 {
		t.Fatalf("M4 verifier output has no failure evidence path:\n%s", output)
	}
	return match[1]
}

func m4PathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator)) &&
		!filepath.IsAbs(relative)
}

func m4CredentialURIFindings(root string) ([]string, error) {
	credentialURI := regexp.MustCompile(
		`(?i)postgres(?:ql)?://[^/\s:@]+:[^@\s/]+@`,
	)
	var findings []string
	err := filepath.WalkDir(root, func(
		path string, entry fs.DirEntry, walkErr error,
	) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".log", ".json", ".txt":
		default:
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if credentialURI.Match(data) {
			findings = append(findings, path)
		}
		return nil
	})
	sort.Strings(findings)
	return findings, err
}

func TestM4CredentialURIFindingsScansEvidenceRecursively(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	safe := filepath.Join(root, "safe.log")
	secret := filepath.Join(nested, "summary.json")
	if err := os.WriteFile(safe, []byte("postgres://[REDACTED]@db"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		secret,
		[]byte(`{"dsn":"postgres://dedup:secret@127.0.0.1/db"}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	findings, err := m4CredentialURIFindings(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(findings, []string{secret}) {
		t.Fatalf("credential findings=%v, want %v", findings, []string{secret})
	}
}
