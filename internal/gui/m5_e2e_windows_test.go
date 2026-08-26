//go:build windows

package gui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sys/windows"
	_ "modernc.org/sqlite"

	"dedup/internal/proto"
)

const m5E2EActivation = "1"
const m5RemoteTC10Status = "VERIFIED_ON_SECOND_WINDOWS"

var m5ProtectedMediaRoots = []string{
	`I:\tmp`,
	`H:\pik\00000000000`,
}

type m5HarnessEnv struct {
	workspace           string
	runID               string
	runRoot             string
	generated           string
	drive               string
	pipeName            string
	schema              string
	adminDSN            string
	scopedDSN           string
	helperExe           string
	agentExe            string
	guiExe              string
	helperConfig        string
	agentConfig         string
	guiConfig           string
	agentData           string
	evidenceDir         string
	baseURL             string
	machineID           string
	secondWindowsStatus string
	helperPID           int
	agentPID            int
	guiPID              int
}

type m5TCResult struct {
	ID         string         `json:"id"`
	Status     string         `json:"status"`
	DurationMS int64          `json:"duration_ms"`
	Assertions map[string]any `json:"assertions,omitempty"`
	Error      string         `json:"error,omitempty"`
}

type m5AccessRecord struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}

type m5Evidence struct {
	SchemaVersion       int              `json:"schema_version"`
	RunID               string           `json:"run_id"`
	Schema              string           `json:"schema"`
	PipeName            string           `json:"pipe_name"`
	DriveLetter         string           `json:"drive_letter"`
	SecondWindowsStatus string           `json:"second_windows_status"`
	TC                  []m5TCResult     `json:"tc"`
	AccessLedger        []m5AccessRecord `json:"access_ledger"`
	ProtectedAccesses   int              `json:"protected_media_access_count"`
	TaskIDs             []string         `json:"task_ids"`
	ComponentPIDs       []int            `json:"component_pids"`
}

type m5RemoteTC10Env struct {
	runID        string
	runRoot      string
	helperExe    string
	helperConfig string
	pipeName     string
	evidenceDir  string
}

type m5RemoteTC10Cleanup struct {
	ProcessResidue []int    `json:"process_residue"`
	PipeResidue    int      `json:"pipe_residue"`
	RunRootResidue int      `json:"run_root_residue"`
	Failures       []string `json:"failures"`
}

type m5RemoteTC10Evidence struct {
	SchemaVersion          int                 `json:"schema_version"`
	RunID                  string              `json:"run_id"`
	Host                   string              `json:"host"`
	WindowsVersion         string              `json:"windows_version"`
	WindowsBuild           uint32              `json:"windows_build"`
	GOARCH                 string              `json:"goarch"`
	HelperSHA256           string              `json:"helper_sha256"`
	VerifierSHA256         string              `json:"verifier_sha256"`
	Status                 string              `json:"status"`
	SecondWindowsStatus    string              `json:"second_windows_status"`
	StartedUTC             string              `json:"started_utc"`
	CompletedUTC           string              `json:"completed_utc"`
	HelperPID              int                 `json:"helper_pid"`
	HelloPID               int                 `json:"hello_pid"`
	PipeName               string              `json:"pipe_name"`
	Assertions             map[string]bool     `json:"assertions"`
	ProtectedMediaAccesses int                 `json:"protected_media_access_count"`
	Cleanup                m5RemoteTC10Cleanup `json:"cleanup"`
}

type m5RemoteTC10Fixture struct {
	env            m5RemoteTC10Env
	started        time.Time
	process        *m5RunningProcess
	helperPID      int
	helloPID       int
	status         string
	assertions     map[string]bool
	helperSHA256   string
	verifierSHA256 string
	windowsVersion string
	windowsBuild   uint32
}

type m5Fixture struct {
	t      *testing.T
	ctx    context.Context
	cancel context.CancelFunc
	env    m5HarnessEnv

	pg     *pgxpool.Pool
	sqlite *sql.DB
	client *http.Client

	ledgerMu sync.Mutex
	ledger   []m5AccessRecord
	taskIDs  []string
	pids     []int
	results  []m5TCResult
	assert   map[string]any

	restartedMu      sync.Mutex
	restartedHelpers []*m5RunningProcess
	activeHelper     *m5RunningProcess
	activeHelperPID  int
}

type m5RunningProcess struct {
	cmd      *exec.Cmd
	exe      string
	stdout   *os.File
	stderr   *os.File
	waitOnce sync.Once
	waitErr  error
}

type m5SeedSpec struct {
	name string
	path string
	data []byte
}

type m5SeededCase struct {
	name       string
	groupID    int64
	repID      int64
	memberIDs  []int64
	paths      []string
	pathByName map[string]string
	idByName   map[string]int64
	dataByName map[string][]byte
}

type m5FileInvariant struct {
	Path            string
	SHA512          string
	Size            int64
	ModTimeUnixNano int64
	Mode            os.FileMode
	Attributes      uint32
}

type m5DBFileInvariant struct {
	Path        string
	Size        int64
	MTime       int64
	SHA512      string
	Status      string
	MissingMask int64
}

type m5DeleteAuditRecord struct {
	Message         string  `json:"msg"`
	TaskID          string  `json:"task_id"`
	MachineID       string  `json:"machine_id"`
	Seq             *uint64 `json:"seq"`
	Path            string  `json:"path"`
	Mode            string  `json:"mode"`
	OK              bool    `json:"ok"`
	ErrCode         string  `json:"err_code"`
	Err             string  `json:"err"`
	ReadonlyCleared bool    `json:"readonly_cleared"`
	RecycledTo      string  `json:"recycled_to"`
	Uncertain       bool    `json:"uncertain"`
	SuccessCount    int     `json:"success_count,omitempty"`
}

type m5TC09LocalState struct {
	Path       string `json:"path"`
	LocalID    *int64 `json:"local_id,omitempty"`
	Status     string `json:"status,omitempty"`
	QueueRowPK string `json:"queue_row_pk,omitempty"`
	Synced     *int64 `json:"synced,omitempty"`
	Generation *int64 `json:"generation,omitempty"`
	QueryError string `json:"query_error,omitempty"`
}

type m5TC09RawAuditRow struct {
	Sequence   int64  `json:"sequence"`
	RowPK      string `json:"row_pk"`
	Generation int64  `json:"generation"`
}

type m5TC09Diagnostics struct {
	SchemaVersion int                   `json:"schema_version"`
	RunID         string                `json:"run_id"`
	RetryTaskID   string                `json:"retry_task_id"`
	AuditBaseline int64                 `json:"audit_baseline"`
	LocalStates   []m5TC09LocalState    `json:"local_states"`
	RawAuditRows  []m5TC09RawAuditRow   `json:"raw_audit_rows_after_baseline"`
	DeleteAudit   []m5DeleteAuditRecord `json:"filtered_delete_audit"`
	RetryStatus   DeleteTaskStatus      `json:"retry_status"`
}

func TestM5DeleteAuditJSONLDecodesWindowsPaths(t *testing.T) {
	data := []byte(
		"{\"msg\":\"delete_physical_result\"," +
			"\"task_id\":\"task-1\",\"machine_id\":\"machine-1\",\"seq\":0," +
			"\"path\":\"Z:\\\\generated\\\\tc01\\\\readonly.bin\"," +
			"\"mode\":\"hard\",\"ok\":true,\"readonly_cleared\":true}\n",
	)
	records, err := parseM5DeleteAuditJSONL(data)
	if err != nil {
		t.Fatalf("parse delete audit JSONL: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("delete audit records = %d, want 1", len(records))
	}
	record := records[0]
	if record.Message != "delete_physical_result" ||
		record.TaskID != "task-1" ||
		record.MachineID != "machine-1" ||
		record.Seq == nil ||
		*record.Seq != 0 ||
		record.Path != `Z:\generated\tc01\readonly.bin` ||
		record.Mode != proto.ModeHard ||
		!record.OK ||
		!record.ReadonlyCleared {
		t.Fatalf("decoded delete audit record = %#v", record)
	}
}

func TestM5NamedPipeSecurityLookupByExactOpen(t *testing.T) {
	name := `\\.\pipe\m5-e2e-sddl-` + uuid.NewString()
	listener, err := winio.ListenPipe(name, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- connection
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sddl, err := queryM5PipeSDDL(ctx, name)
	if err != nil {
		t.Fatalf("query exact named-pipe security through bounded open: %v", err)
	}
	if sddl == "" {
		t.Fatal("exact named-pipe security query returned an empty descriptor")
	}
	select {
	case connection := <-accepted:
		_ = connection.Close()
	case err := <-acceptErr:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestM5SecondWindowsStatusRequiresExactVerified(t *testing.T) {
	if err := validateM5SecondWindowsStatus(m5RemoteTC10Status); err != nil {
		t.Fatalf("exact verified second-Windows status: %v", err)
	}
	for _, status := range []string{
		"",
		"PENDING_REMOTE_VALIDATION",
		"USER_WAIVED",
		"OTHER_REMOTE_STATUS",
		"pending",
		"VERIFIED ON SECOND WINDOWS",
	} {
		if err := validateM5SecondWindowsStatus(status); err == nil {
			t.Errorf("invalid second-Windows status %q was accepted", status)
		}
	}
}

func TestM5ProtectedAccessCountComesFromLedger(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		records []m5AccessRecord
		want    int
	}{
		{
			name: "synthetic-run-only",
			records: []m5AccessRecord{
				{Kind: "generated", Path: `Z:\generated\tc12\04000.bin`},
				{Kind: "component", Path: `C:\tools\helper.exe`},
				{Kind: "url", Path: "http://127.0.0.1:6253/api/delete/status"},
			},
			want: 0,
		},
		{
			name: "protected-root-descendant-and-ancestor",
			records: []m5AccessRecord{
				{Kind: "generated", Path: `I:\tmp`},
				{Kind: "database-path", Path: `H:\pik\00000000000\media.bin`},
				{Kind: "generated", Path: `H:\pik`},
				{Kind: "pipe", Path: `\\.\pipe\dedup-m5-safe`},
			},
			want: 3,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := countM5ProtectedAccesses(testCase.records); got != testCase.want {
				t.Fatalf("countM5ProtectedAccesses()=%d, want %d", got, testCase.want)
			}
		})
	}
}

func TestM5PipeSecurityRecognizesNormalizedNetworkFullDeny(t *testing.T) {
	for _, testCase := range []struct {
		name string
		sddl string
		want bool
	}{
		{"generic-all", "D:(D;;GA;;;NU)", true},
		{"file-all", "D:(D;;FA;;;NU)", true},
		{"generic-read", "D:(D;;GR;;;NU)", false},
		{"allow-file-all", "D:(A;;FA;;;NU)", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := m5PipeSDDLHasNetworkFullDeny(testCase.sddl)
			if err != nil {
				t.Fatal(err)
			}
			if got != testCase.want {
				t.Fatalf(
					"m5PipeSDDLHasNetworkFullDeny(%q)=%v, want %v",
					testCase.sddl,
					got,
					testCase.want,
				)
			}
		})
	}
}

func TestM5TokenGroupLookupDetectsNetworkSID(t *testing.T) {
	networkSID, err := windows.CreateWellKnownSid(windows.WinNetworkSid)
	if err != nil {
		t.Fatal(err)
	}
	worldSID, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	groups := []windows.SIDAndAttributes{
		{Sid: worldSID},
		{Sid: networkSID},
	}
	if !m5SIDAndAttributesContain(groups, networkSID) {
		t.Fatal("NETWORK SID was not detected in a synthetic token group")
	}
	if m5SIDAndAttributesContain(groups[:1], networkSID) {
		t.Fatal("NETWORK SID was reported present in a synthetic token group without it")
	}
}

func TestM5RemoteTC10RunRootValidationIsExactAndInert(t *testing.T) {
	const runID = "0123456789abcdef0123456789abcdef"
	if err := validateM5RemoteTC10RunRoot(
		`C:\remote-bundle\m5-remote-tc10-`+runID,
		runID,
	); err != nil {
		t.Fatalf("valid remote TC10 run root: %v", err)
	}
	for _, root := range []string{
		`C:\`,
		`C:\remote-bundle\wrong-` + runID,
		`I:\tmp`,
		`H:\pik\00000000000`,
	} {
		if err := validateM5RemoteTC10RunRoot(root, runID); err == nil {
			t.Errorf("unsafe remote TC10 run root %q was accepted", root)
		}
	}
}

func TestM5FileSHA256IsBoundedHex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artifact.bin")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := m5FileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	const want = "ba7816bf8f01cfea414140de5dae2223" +
		"b00361a396177a9cb410ff61f20015ad"
	if got != want {
		t.Fatalf("m5FileSHA256()=%q, want %q", got, want)
	}
}

func TestM5PendingSyncAuditSurvivesImmediateSync(t *testing.T) {
	database, err := sql.Open(
		"sqlite",
		"file:m5-pending-audit-"+uuid.NewString()+"?mode=memory&cache=shared",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`
		CREATE TABLE files (
			id INTEGER PRIMARY KEY,
			path TEXT NOT NULL
		);
		CREATE TABLE sync_queue (
			table_name TEXT NOT NULL,
			row_pk TEXT NOT NULL,
			synced INTEGER NOT NULL,
			enqueued_at INTEGER NOT NULL,
			generation INTEGER NOT NULL,
			PRIMARY KEY (table_name, row_pk)
		);
		INSERT INTO files(id,path) VALUES(41,'Z:\generated\tc09\retry.bin');
	`); err != nil {
		t.Fatal(err)
	}
	if err := installM5PendingSyncAudit(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	observer, err := newM5PendingObserver(
		context.Background(),
		database,
		[]string{`Z:\generated\tc09\retry.bin`},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`
		INSERT INTO sync_queue(table_name,row_pk,synced,enqueued_at,generation)
		VALUES('files','41',0,1,1);
		UPDATE sync_queue SET synced=1
		WHERE table_name='files' AND row_pk='41';
	`); err != nil {
		t.Fatal(err)
	}
	if err := observer.verify(); err != nil {
		t.Fatal(err)
	}
}

func TestM5FileInvariantsDetectBytesAndMetadataMutation(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(t *testing.T, path string, baselineTime time.Time)
	}{
		{
			name: "bytes",
			mutate: func(t *testing.T, path string, baselineTime time.Time) {
				t.Helper()
				if err := os.WriteFile(path, []byte("xyz"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, baselineTime, baselineTime); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "metadata",
			mutate: func(t *testing.T, path string, baselineTime time.Time) {
				t.Helper()
				changed := baselineTime.Add(2 * time.Second)
				if err := os.Chtimes(path, changed, changed); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "later-chunk.bin")
			if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
				t.Fatal(err)
			}
			baselineTime := time.Unix(1_700_000_000, 0)
			if err := os.Chtimes(path, baselineTime, baselineTime); err != nil {
				t.Fatal(err)
			}
			baseline, err := captureM5FileInvariants([]string{path})
			if err != nil {
				t.Fatal(err)
			}
			testCase.mutate(t, path, baselineTime)
			if err := verifyM5FileInvariants(baseline); err == nil {
				t.Fatalf("%s mutation was not detected", testCase.name)
			}
		})
	}
}

func TestM5NoPendingObserverRejectsDeletionUpstream(t *testing.T) {
	database, err := sql.Open(
		"sqlite",
		"file:m5-no-pending-"+uuid.NewString()+"?mode=memory&cache=shared",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`
		CREATE TABLE files (
			id INTEGER PRIMARY KEY,
			path TEXT NOT NULL
		);
		CREATE TABLE sync_queue (
			table_name TEXT NOT NULL,
			row_pk TEXT NOT NULL,
			synced INTEGER NOT NULL,
			enqueued_at INTEGER NOT NULL,
			generation INTEGER NOT NULL,
			PRIMARY KEY (table_name, row_pk)
		);
		INSERT INTO files(id,path) VALUES(41,'Z:\generated\tc12\04000.bin');
	`); err != nil {
		t.Fatal(err)
	}
	if err := installM5PendingSyncAudit(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	observer, err := newM5NoPendingObserver(
		context.Background(),
		database,
		[]string{`Z:\generated\tc12\04000.bin`},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := observer.verify(); err != nil {
		t.Fatalf("clean no-pending baseline: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO sync_queue(table_name,row_pk,synced,enqueued_at,generation)
		VALUES('files','41',0,1,1);
	`); err != nil {
		t.Fatal(err)
	}
	if err := observer.verify(); err == nil {
		t.Fatal("pending deletion upstream was not detected")
	}
}

func TestM5DatabaseInvariantComparisonRejectsMutation(t *testing.T) {
	baseline := []m5DBFileInvariant{
		{
			Path:        `Z:\generated\tc12\04000.bin`,
			Size:        3,
			MTime:       1700000000000,
			SHA512:      "sha-04000",
			Status:      "done",
			MissingMask: 0,
		},
		{
			Path:        `Z:\generated\tc12\04001.bin`,
			Size:        3,
			MTime:       1700000000001,
			SHA512:      "sha-04001",
			Status:      "done",
			MissingMask: 0,
		},
	}
	for _, testCase := range []struct {
		name     string
		observed []m5DBFileInvariant
	}{
		{
			name: "deleted-status",
			observed: []m5DBFileInvariant{
				baseline[0],
				{
					Path:        baseline[1].Path,
					Size:        3,
					MTime:       1700000000001,
					SHA512:      "sha-04001",
					Status:      "deleted",
					MissingMask: 0,
				},
			},
		},
		{
			name: "changed-hash",
			observed: []m5DBFileInvariant{
				{
					Path:        baseline[0].Path,
					Size:        3,
					MTime:       1700000000000,
					SHA512:      "changed",
					Status:      "done",
					MissingMask: 0,
				},
				baseline[1],
			},
		},
		{name: "missing-row", observed: []m5DBFileInvariant{baseline[0]}},
		{
			name: "unexpected-row",
			observed: append(
				append([]m5DBFileInvariant(nil), baseline...),
				m5DBFileInvariant{
					Path:        `Z:\generated\tc12\04999.bin`,
					Size:        3,
					MTime:       1700000000999,
					SHA512:      "sha-unexpected",
					Status:      "done",
					MissingMask: 0,
				},
			),
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if err := compareM5DBFileInvariants(baseline, testCase.observed); err == nil {
				t.Fatalf("%s mutation was not detected", testCase.name)
			}
		})
	}
	if err := compareM5DBFileInvariants(baseline, baseline); err != nil {
		t.Fatalf("identical database invariants: %v", err)
	}
}

func TestM5TC09AuditFilterSelectsOnlyBoundedRetryRecords(t *testing.T) {
	records := []m5DeleteAuditRecord{
		{Message: "delete_physical_result", TaskID: "old", Path: `Z:\generated\old.bin`},
		{Message: "unrelated", TaskID: "retry"},
		{Message: "delete_physical_result", TaskID: "retry", Path: `Z:\generated\a.bin`},
		{Message: "delete_state_sync_error", TaskID: "retry", SuccessCount: 2},
		{Message: "delete_physical_result", TaskID: "retry", Path: `Z:\generated\b.bin`},
	}
	filtered := filterM5TC09AuditRecords(records, "retry", 2)
	if len(filtered) != 2 ||
		filtered[0].Message != "delete_physical_result" ||
		filtered[0].Path != `Z:\generated\a.bin` ||
		filtered[1].Message != "delete_state_sync_error" ||
		filtered[1].SuccessCount != 2 {
		t.Fatalf("filtered TC09 audit records=%#v", filtered)
	}
}

func TestM5DACLOnlyDescriptorOmitsNilOwnerAndGroupFlags(t *testing.T) {
	information, owner, group, dacl, err := m5SecurityDescriptorParts(
		"D:P(A;;FA;;;SY)",
	)
	if err != nil {
		t.Fatalf("parse DACL-only descriptor: %v", err)
	}
	if owner != nil || group != nil || dacl == nil {
		t.Fatalf(
			"DACL-only descriptor owner=%v group=%v dacl=%v",
			owner,
			group,
			dacl,
		)
	}
	if information&windows.DACL_SECURITY_INFORMATION == 0 ||
		information&windows.PROTECTED_DACL_SECURITY_INFORMATION == 0 ||
		information&windows.OWNER_SECURITY_INFORMATION != 0 ||
		information&windows.GROUP_SECURITY_INFORMATION != 0 {
		t.Fatalf("DACL-only security information = %#x", information)
	}
}

func TestM5ACLFixtureRoundTrip(t *testing.T) {
	if os.Getenv("M5_E2E_ACTIVE") != m5E2EActivation ||
		os.Getenv("M5_E2E_ACTION") != "acl-gate" {
		t.Skip("ACL gate action is not selected")
	}
	workspace, err := filepath.Abs(os.Getenv("M5_E2E_WORKSPACE"))
	if err != nil || workspace == "" {
		t.Fatalf("ACL gate workspace: %v", err)
	}
	gateRoot, err := filepath.Abs(os.Getenv("M5_E2E_ACL_GATE_ROOT"))
	if err != nil || gateRoot == "" {
		t.Fatalf("ACL gate root: %v", err)
	}
	tmpRoot := filepath.Join(workspace, ".superpowers", "tmp")
	leaf := filepath.Base(gateRoot)
	suffix := strings.TrimPrefix(leaf, "m5-acl-gate-")
	if !strings.EqualFold(filepath.Dir(gateRoot), tmpRoot) ||
		len(suffix) != 32 {
		t.Fatalf("ACL gate root %q is outside the scoped tmp child", gateRoot)
	}
	if _, err := hex.DecodeString(suffix); err != nil {
		t.Fatalf("ACL gate root suffix is not hexadecimal: %v", err)
	}
	info, err := os.Lstat(gateRoot)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("ACL gate root is not a plain existing directory: %v", err)
	}

	directory := filepath.Join(gateRoot, "acl-isolated")
	file := filepath.Join(directory, "denied.bin")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("m5-acl-gate"), 0o600); err != nil {
		t.Fatal(err)
	}
	directoryACL := saveM5ACL(t, directory)
	fileACL := saveM5ACL(t, file)
	t.Cleanup(func() {
		if _, err := os.Lstat(directory); err == nil {
			_ = setM5SecurityDescriptor(directory, directoryACL)
		}
		if _, err := os.Lstat(file); err == nil {
			_ = setM5SecurityDescriptor(file, fileACL)
			_ = os.Remove(file)
		}
		if _, err := os.Lstat(directory); err == nil {
			_ = os.Remove(directory)
		}
	})

	denyM5Delete(t, directory, file)
	if err := os.Remove(file); err == nil {
		t.Fatal("DACL-only deny unexpectedly allowed file removal")
	} else if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("DACL-only deny removal error = %v", err)
	}
	if err := setM5SecurityDescriptor(directory, directoryACL); err != nil {
		t.Fatalf("restore ACL gate directory: %v", err)
	}
	if err := setM5SecurityDescriptor(file, fileACL); err != nil {
		t.Fatalf("restore ACL gate file: %v", err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatalf("remove restored ACL gate file: %v", err)
	}
	if err := os.Remove(directory); err != nil {
		t.Fatalf("remove restored ACL gate directory: %v", err)
	}
}

func TestM5E2ESchemaSetup(t *testing.T) {
	if os.Getenv("M5_E2E_ACTIVE") == "" {
		t.Skip("set M5_E2E_ACTIVE=1 through verify_m5_e2e.ps1")
	}
	if os.Getenv("M5_E2E_ACTION") != "schema-setup" {
		t.Skip("schema setup action is not selected")
	}
	env := loadM5HarnessEnv(t, false)
	requireM5Schema(t, env.schema)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, env.adminDSN)
	if err != nil {
		t.Fatal(redactM5Error(err))
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(redactM5Error(err))
	}
	quoted := pgx.Identifier{env.schema}.Sanitize()
	var exists int
	err = pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_namespace WHERE nspname=$1`,
		env.schema,
	).Scan(&exists)
	if err != nil {
		t.Fatal(redactM5Error(err))
	}
	if exists != 0 {
		t.Fatalf("schema setup refused existing recorded schema %q", env.schema)
	}
	if _, err := pool.Exec(ctx, `CREATE SCHEMA `+quoted); err != nil {
		t.Fatal(redactM5Error(err))
	}
	created := true
	defer func() {
		if created && t.Failed() {
			_, _ = pool.Exec(context.Background(), `DROP SCHEMA `+quoted+` CASCADE`)
		}
	}()
	sqlBytes, err := os.ReadFile(filepath.Join(env.workspace, "deploy", "central.sql"))
	if err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(env.scopedDSN)
	if err != nil {
		t.Fatal(redactM5Error(err))
	}
	scoped, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(redactM5Error(err))
	}
	defer scoped.Close()
	if _, err := scoped.Exec(ctx, string(sqlBytes)); err != nil {
		t.Fatal(redactM5Error(err))
	}
	var current string
	if err := scoped.QueryRow(ctx, `SELECT current_schema()`).Scan(&current); err != nil {
		t.Fatal(redactM5Error(err))
	}
	if current != env.schema {
		t.Fatalf("schema setup current_schema=%q, want %q", current, env.schema)
	}
	created = false
	fmt.Printf("M5_SCHEMA_SETUP_OK run_id=%s schema=%s\n", env.runID, env.schema)
}

func TestM5E2ESchemaCleanup(t *testing.T) {
	if os.Getenv("M5_E2E_ACTIVE") == "" {
		t.Skip("set M5_E2E_ACTIVE=1 through verify_m5_e2e.ps1")
	}
	if os.Getenv("M5_E2E_ACTION") != "schema-cleanup" {
		t.Skip("schema cleanup action is not selected")
	}
	env := loadM5HarnessEnv(t, false)
	requireM5Schema(t, env.schema)
	if os.Getenv("M5_E2E_RECORDED_SCHEMA") != env.schema {
		t.Fatal("cleanup schema does not match the harness record")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, env.adminDSN)
	if err != nil {
		t.Fatal(redactM5Error(err))
	}
	defer pool.Close()
	quoted := pgx.Identifier{env.schema}.Sanitize()
	if _, err := pool.Exec(ctx, `DROP SCHEMA IF EXISTS `+quoted+` CASCADE`); err != nil {
		t.Fatal(redactM5Error(err))
	}
	var remains int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_namespace WHERE nspname=$1`,
		env.schema,
	).Scan(&remains); err != nil {
		t.Fatal(redactM5Error(err))
	}
	if remains != 0 {
		t.Fatalf("recorded schema %q remains after cleanup", env.schema)
	}
	fmt.Printf("M5_SCHEMA_CLEANUP_OK run_id=%s schema=%s\n", env.runID, env.schema)
}

func TestM5E2EWindows(t *testing.T) {
	if os.Getenv("M5_E2E_ACTIVE") == "" {
		t.Skip("set M5_E2E_ACTIVE=1 through verify_m5_e2e.ps1")
	}
	if os.Getenv("M5_E2E_ACTION") != "acceptance" {
		t.Skip("acceptance action is not selected")
	}
	fixture := openM5Fixture(t)
	defer fixture.finishEvidence()

	steps := []struct {
		id  string
		run func(*testing.T)
	}{
		{"TC-01", fixture.tc01HardReadonly},
		{"TC-02", fixture.tc02MissingMixed},
		{"TC-03", fixture.tc03InUseAndAccessDenied},
		{"TC-04", fixture.tc04SoftRecycleAndCollision},
		{"TC-05", fixture.tc05PathDenied},
		{"TC-06", fixture.tc06UnconfirmedDirectFrame},
		{"TC-07", fixture.tc07JunctionAndNeighbor},
		{"TC-08", fixture.tc08FiveThousandChunks},
		{"TC-09", fixture.tc09OfflineAndRestart},
		{"TC-10", fixture.tc10PipeSecurity},
		{"TC-11", fixture.tc11LifetimeAndShutdown},
		{"TC-12", fixture.tc12KillAfterFirstChunk},
	}
	fixture.results = make([]m5TCResult, len(steps))
	for index, step := range steps {
		fixture.results[index] = m5TCResult{
			ID: step.id, Status: "NOT_RUN",
		}
	}
	for index, step := range steps {
		fixture.assert = make(map[string]any)
		started := time.Now()
		ok := t.Run(step.id, step.run)
		fixture.results[index].DurationMS = time.Since(started).Milliseconds()
		fixture.results[index].Assertions = fixture.assert
		if !ok {
			fixture.results[index].Status = "FAILED"
			fixture.results[index].Error = "see bounded Go test output"
			break
		}
		fixture.results[index].Status = "PASSED"
	}
	for _, result := range fixture.results {
		if result.Status != "PASSED" {
			t.Fatalf("%s status=%s", result.ID, result.Status)
		}
	}
}

func TestM5RemoteTC10Windows(t *testing.T) {
	if os.Getenv("M5_E2E_ACTION") != "remote-tc10" {
		t.Skip("remote TC10 action is not selected")
	}
	for _, name := range []string{
		"M5_E2E_ADMIN_DSN",
		"M5_E2E_SCOPED_DSN",
		"PGDSN",
	} {
		if os.Getenv(name) != "" {
			t.Fatalf("remote TC10 forbids database environment %s", name)
		}
	}
	env := loadM5RemoteTC10Env(t)
	fixture := &m5RemoteTC10Fixture{
		env:        env,
		started:    time.Now().UTC(),
		status:     "FAIL",
		assertions: make(map[string]bool),
	}
	defer fixture.finish(t)
	fixture.requireElevated(t)
	var err error
	fixture.helperSHA256, err = m5FileSHA256(env.helperExe)
	if err != nil {
		t.Fatalf("hash remote Helper: %v", err)
	}
	verifierExe, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve remote verifier executable: %v", err)
	}
	fixture.verifierSHA256, err = m5FileSHA256(verifierExe)
	if err != nil {
		t.Fatalf("hash remote verifier: %v", err)
	}
	version := windows.RtlGetVersion()
	fixture.windowsVersion = fmt.Sprintf(
		"%d.%d",
		version.MajorVersion,
		version.MinorVersion,
	)
	fixture.windowsBuild = version.BuildNumber
	fixture.assertions["artifact_hashes_recorded"] = true
	fixture.assertions["windows_build_recorded"] = true

	fixture.prepareRunRoot(t)
	if pids, err := findM5ProcessesByImageExact(env.helperExe); err != nil {
		t.Fatal(err)
	} else if len(pids) != 0 {
		t.Fatalf("remote TC10 preflight found exact Helper PIDs %v", pids)
	}
	fixture.requirePipeUnavailable(t)
	fixture.startHelper(t)

	connection, hello := fixture.dialHello(t)
	_ = connection.Close()
	if hello.PID != fixture.helperPID {
		t.Fatalf(
			"remote TC10 current-user Hello PID=%d, want exact Helper PID=%d",
			hello.PID,
			fixture.helperPID,
		)
	}
	fixture.helloPID = hello.PID
	fixture.assertions["current_user_connected"] = true
	fixture.assertions["hello_pid_matches_exact_process"] = true

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	sddl, err := queryM5PipeSDDL(ctx, env.pipeName)
	cancel()
	if err != nil {
		t.Fatalf("remote TC10 query exact Helper pipe DACL: %v", err)
	}
	networkFullDeny, err := m5PipeSDDLHasNetworkFullDeny(sddl)
	if err != nil {
		t.Fatalf("remote TC10 parse real Helper pipe DACL: %v", err)
	}
	if !networkFullDeny {
		t.Fatal("remote TC10 real Helper pipe lacks NETWORK full deny")
	}
	fixture.assertions["network_full_deny"] = true

	dialErr, cleanupErr := dialM5PipeWithNetworkRestrictedToken(env.pipeName)
	if cleanupErr != nil {
		t.Fatalf("remote TC10 restricted token cleanup: %v", cleanupErr)
	}
	if !errors.Is(dialErr, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf(
			"remote TC10 NETWORK restricted dial error=%v, want ERROR_ACCESS_DENIED",
			dialErr,
		)
	}
	fixture.assertions["network_restricted_denied"] = true

	connection, hello = fixture.dialHello(t)
	_ = connection.Close()
	if hello.PID != fixture.helperPID {
		t.Fatalf("remote TC10 reconnect Hello PID=%d, want %d", hello.PID, fixture.helperPID)
	}
	fixture.assertions["current_user_reconnected"] = true

	fixture.shutdownHelper(t)
	fixture.assertions["explicit_shutdown"] = true
	fixture.assertions["process_residue_zero"] = true
	fixture.assertions["pipe_residue_zero"] = true
	fixture.status = "PASS"
}

func loadM5RemoteTC10Env(t *testing.T) m5RemoteTC10Env {
	t.Helper()
	required := func(name string) string {
		value := os.Getenv(name)
		if value == "" {
			t.Fatalf("remote TC10 missing %s", name)
		}
		return value
	}
	env := m5RemoteTC10Env{
		runID:        required("M5_REMOTE_TC10_RUN_ID"),
		runRoot:      required("M5_REMOTE_TC10_ROOT"),
		helperExe:    required("M5_REMOTE_TC10_HELPER_EXE"),
		helperConfig: required("M5_REMOTE_TC10_HELPER_CONFIG"),
		pipeName:     required("M5_REMOTE_TC10_PIPE"),
		evidenceDir:  required("M5_REMOTE_TC10_EVIDENCE_DIR"),
	}
	if err := validateM5RemoteTC10RunRoot(env.runRoot, env.runID); err != nil {
		t.Fatal(err)
	}
	wantPipe := `\\.\pipe\dedup-m5-remote-` + env.runID
	if env.pipeName != wantPipe {
		t.Fatalf("remote TC10 pipe %q does not match run ID", env.pipeName)
	}
	for _, path := range []string{
		env.runRoot,
		env.helperExe,
		env.helperConfig,
		env.evidenceDir,
	} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			t.Fatalf("remote TC10 path is not clean and absolute: %q", path)
		}
	}
	if err := requireM5RemotePlainPath(env.runRoot, true); err != nil {
		t.Fatalf("remote TC10 run root: %v", err)
	}
	if err := requireM5RemotePlainPath(env.evidenceDir, true); err != nil {
		t.Fatalf("remote TC10 evidence directory: %v", err)
	}
	for _, path := range []string{env.helperExe, env.helperConfig} {
		if err := requireM5RemotePlainPath(path, false); err != nil {
			t.Fatalf("remote TC10 required leaf: %v", err)
		}
	}
	for _, path := range []string{
		env.helperExe,
		env.helperConfig,
		env.evidenceDir,
	} {
		if m5WindowsPathWithin(env.runRoot, path) ||
			m5WindowsPathWithin(path, env.runRoot) {
			t.Fatal("remote TC10 run root overlaps a preserved input/evidence path")
		}
	}
	entries, err := os.ReadDir(env.runRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("remote TC10 run root is not empty before the test")
	}

	file, err := os.Open(env.helperConfig)
	if err != nil {
		t.Fatal(err)
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, 64<<10+1))
	closeErr := file.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if len(raw) > 64<<10 {
		t.Fatal("remote TC10 Helper config exceeds 64 KiB")
	}
	var config struct {
		PipeName     string   `json:"pipe_name"`
		AllowedRoots []string `json:"allowed_roots"`
		DeniedRoots  []string `json:"denied_roots"`
		LogDir       string   `json:"log_dir"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatalf("decode remote TC10 Helper config: %v", err)
	}
	if config.PipeName != env.pipeName {
		t.Fatal("remote TC10 Helper config pipe does not match the recorded pipe")
	}
	wantAllowed := filepath.Join(env.runRoot, "generated")
	wantLog := filepath.Join(env.runRoot, "logs")
	if len(config.AllowedRoots) != 1 ||
		!strings.EqualFold(filepath.Clean(config.AllowedRoots[0]), wantAllowed) ||
		len(config.DeniedRoots) != 0 ||
		!strings.EqualFold(filepath.Clean(config.LogDir), wantLog) {
		t.Fatal("remote TC10 Helper config is not scoped to the exact generated run root")
	}
	for _, value := range append(
		append([]string(nil), config.AllowedRoots...),
		append(config.DeniedRoots, config.LogDir)...,
	) {
		for _, protected := range []string{
			`I:\tmp`,
			`H:\pik\00000000000`,
		} {
			if m5WindowsPathWithin(protected, value) ||
				m5WindowsPathWithin(value, protected) {
				t.Fatal("remote TC10 Helper config intersects a protected media root")
			}
		}
	}
	evidencePath := filepath.Join(
		env.evidenceDir,
		"m5-remote-tc10-"+env.runID+".json",
	)
	if _, err := os.Lstat(evidencePath); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			t.Fatal("remote TC10 evidence path already exists")
		}
		t.Fatal(err)
	}
	return env
}

func validateM5RemoteTC10RunRoot(root, runID string) error {
	if len(runID) != 32 {
		return errors.New("remote TC10 run ID must be 32 hexadecimal characters")
	}
	if _, err := hex.DecodeString(runID); err != nil {
		return errors.New("remote TC10 run ID must be 32 hexadecimal characters")
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return errors.New("remote TC10 run root must be clean and absolute")
	}
	clean := filepath.Clean(root)
	if filepath.Base(clean) != "m5-remote-tc10-"+runID ||
		filepath.Dir(clean) == clean ||
		filepath.VolumeName(clean)+`\` == clean {
		return errors.New("remote TC10 run root does not have the exact scoped leaf")
	}
	for _, protected := range []string{
		`I:\tmp`,
		`H:\pik\00000000000`,
	} {
		if m5WindowsPathWithin(protected, clean) ||
			m5WindowsPathWithin(clean, protected) {
			return errors.New("remote TC10 run root intersects a protected media root")
		}
	}
	return nil
}

func m5WindowsPathWithin(parent, candidate string) bool {
	parent = strings.TrimRight(filepath.Clean(parent), `\`)
	candidate = strings.TrimRight(filepath.Clean(candidate), `\`)
	return strings.EqualFold(parent, candidate) ||
		strings.HasPrefix(
			strings.ToLower(candidate),
			strings.ToLower(parent+`\`),
		)
}

func requireM5RemotePlainPath(path string, wantDirectory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.IsDir() != wantDirectory || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("path has the wrong type or is a symbolic link")
	}
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attributes, err := windows.GetFileAttributes(pathPtr)
	if err != nil {
		return err
	}
	if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("path is a reparse point")
	}
	return nil
}

func m5FileSHA256(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > 256<<20 {
		return "", errors.New("hash input is not a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func (fixture *m5RemoteTC10Fixture) requireElevated(t *testing.T) {
	t.Helper()
	var token windows.Token
	if err := windows.OpenProcessToken(
		windows.CurrentProcess(),
		windows.TOKEN_QUERY,
		&token,
	); err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	if !token.IsElevated() {
		t.Fatal("remote TC10 test token is not elevated")
	}
	fixture.assertions["elevated_test_token"] = true
	networkSID, err := windows.CreateWellKnownSid(windows.WinNetworkSid)
	if err != nil {
		t.Fatalf("create NETWORK SID: %v", err)
	}
	containsNetwork, err := m5TokenContainsGroupSID(token, networkSID)
	if err != nil {
		t.Fatalf("query remote TC10 test token groups: %v", err)
	}
	if containsNetwork {
		t.Fatal("remote TC10 test token contains NETWORK SID (S-1-5-2)")
	}
	fixture.assertions["test_token_excludes_network"] = true
}

func (fixture *m5RemoteTC10Fixture) prepareRunRoot(t *testing.T) {
	t.Helper()
	for _, directory := range []string{
		filepath.Join(fixture.env.runRoot, "generated"),
		filepath.Join(fixture.env.runRoot, "logs"),
	} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fixture.assertions["synthetic_root_only"] = true
}

func (fixture *m5RemoteTC10Fixture) requirePipeUnavailable(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	connection, err := winio.DialPipeContext(ctx, fixture.env.pipeName)
	if connection != nil {
		_ = connection.Close()
		t.Fatal("remote TC10 pipe already exists")
	}
	if err == nil {
		t.Fatal("remote TC10 unavailable-pipe preflight returned no error")
	}
	fixture.assertions["preflight_pipe_absent"] = true
}

func (fixture *m5RemoteTC10Fixture) startHelper(t *testing.T) {
	t.Helper()
	stdout, err := os.OpenFile(
		filepath.Join(fixture.env.runRoot, "helper.stdout.log"),
		os.O_CREATE|os.O_WRONLY|os.O_EXCL,
		0o600,
	)
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.OpenFile(
		filepath.Join(fixture.env.runRoot, "helper.stderr.log"),
		os.O_CREATE|os.O_WRONLY|os.O_EXCL,
		0o600,
	)
	if err != nil {
		_ = stdout.Close()
		t.Fatal(err)
	}
	command := exec.Command(
		fixture.env.helperExe,
		"-config",
		fixture.env.helperConfig,
	)
	command.Stdout = stdout
	command.Stderr = stderr
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := command.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		t.Fatalf("start exact remote Helper: %v", err)
	}
	fixture.process = &m5RunningProcess{
		cmd: command, exe: fixture.env.helperExe, stdout: stdout, stderr: stderr,
	}
	fixture.helperPID = command.Process.Pid
	image, err := m5ProcessImage(fixture.helperPID)
	if err != nil {
		t.Fatalf("query exact remote Helper image: %v", err)
	}
	expected, err := filepath.Abs(fixture.env.helperExe)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(filepath.Clean(image), filepath.Clean(expected)) {
		t.Fatal("remote TC10 started process image does not match Helper bundle leaf")
	}

	readyCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	waitM5(t, readyCtx, "remote Helper Hello", func() (bool, error) {
		dialCtx, dialCancel := context.WithTimeout(readyCtx, 100*time.Millisecond)
		framed, hello, err := dialM5Hello(dialCtx, fixture.env.pipeName)
		dialCancel()
		if framed != nil {
			_ = framed.Close()
		}
		if err != nil {
			if !fixture.process.isAlive() {
				return false, fmt.Errorf(
					"remote Helper exited before Hello: %v",
					fixture.process.wait(),
				)
			}
			return false, nil
		}
		if hello.PID != fixture.helperPID {
			return false, fmt.Errorf(
				"remote Helper Hello PID=%d, want %d",
				hello.PID,
				fixture.helperPID,
			)
		}
		return true, nil
	})
	fixture.assertions["manifest_helper_started"] = true
	fixture.assertions["helper_image_matches_bundle"] = true
}

func (fixture *m5RemoteTC10Fixture) dialHello(
	t *testing.T,
) (*proto.Conn, proto.Hello) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	framed, hello, err := dialM5Hello(ctx, fixture.env.pipeName)
	if err != nil {
		t.Fatal(err)
	}
	return framed, hello
}

func dialM5Hello(
	ctx context.Context,
	pipeName string,
) (*proto.Conn, proto.Hello, error) {
	connection, err := winio.DialPipeContext(ctx, pipeName)
	if err != nil {
		return nil, proto.Hello{}, fmt.Errorf("dial Helper pipe: %w", err)
	}
	framed := proto.NewConn(connection)
	_ = framed.SetReadDeadline(time.Now().Add(5 * time.Second))
	messageType, body, err := framed.ReadFrame()
	if err != nil {
		_ = framed.Close()
		return nil, proto.Hello{}, fmt.Errorf("read Helper Hello: %w", err)
	}
	decoded, err := proto.Decode(messageType, body)
	if err != nil {
		_ = framed.Close()
		return nil, proto.Hello{}, fmt.Errorf("decode Helper Hello: %w", err)
	}
	hello, ok := decoded.(*proto.Hello)
	if messageType != proto.MsgHello || !ok || hello.PID <= 0 {
		_ = framed.Close()
		return nil, proto.Hello{}, fmt.Errorf(
			"invalid Helper Hello type=%d value=%T",
			messageType,
			decoded,
		)
	}
	return framed, *hello, nil
}

func (fixture *m5RemoteTC10Fixture) shutdownHelper(t *testing.T) {
	t.Helper()
	framed, hello := fixture.dialHello(t)
	if hello.PID != fixture.helperPID {
		_ = framed.Close()
		t.Fatalf(
			"remote Shutdown Hello PID=%d, want %d",
			hello.PID,
			fixture.helperPID,
		)
	}
	framed.SetWriteTimeout(5 * time.Second)
	if err := framed.WriteFrame(proto.MsgShutdown, &proto.Shutdown{}); err != nil {
		_ = framed.Close()
		t.Fatalf("write remote Helper Shutdown: %v", err)
	}
	_ = framed.Close()
	exitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	waitM5ProcessExit(t, exitCtx, fixture.helperPID)
	if err := fixture.process.wait(); err != nil {
		t.Fatalf("wait remote Helper Shutdown: %v", err)
	}
	fixture.requirePipeUnavailable(t)
	pids, err := findM5ProcessesByImageExact(fixture.env.helperExe)
	if err != nil {
		t.Fatal(err)
	}
	if len(pids) != 0 {
		t.Fatalf("remote Helper process residue after Shutdown: %v", pids)
	}
}

func (fixture *m5RemoteTC10Fixture) finish(t *testing.T) {
	t.Helper()
	cleanup := m5RemoteTC10Cleanup{
		ProcessResidue: []int{},
		Failures:       []string{},
	}
	if fixture.process != nil {
		if fixture.process.isAlive() {
			image, err := m5ProcessImage(fixture.helperPID)
			if err != nil {
				cleanup.Failures = append(
					cleanup.Failures,
					"query exact Helper image before cleanup",
				)
			} else if !strings.EqualFold(
				filepath.Clean(image),
				filepath.Clean(fixture.env.helperExe),
			) {
				cleanup.Failures = append(
					cleanup.Failures,
					"exact Helper PID changed image before cleanup",
				)
			} else if err := fixture.process.killExact(); err != nil {
				cleanup.Failures = append(
					cleanup.Failures,
					"kill exact Helper process",
				)
			}
		} else {
			_ = fixture.process.wait()
		}
	}
	if pids, err := findM5ProcessesByImageExact(fixture.env.helperExe); err != nil {
		cleanup.Failures = append(cleanup.Failures, "enumerate exact Helper process residue")
	} else if len(pids) != 0 {
		if len(pids) > 16 {
			pids = pids[:16]
		}
		cleanup.ProcessResidue = append(cleanup.ProcessResidue, pids...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	connection, _ := winio.DialPipeContext(ctx, fixture.env.pipeName)
	cancel()
	if connection != nil {
		cleanup.PipeResidue = 1
		_ = connection.Close()
	}
	if err := validateM5RemoteTC10RunRoot(
		fixture.env.runRoot,
		fixture.env.runID,
	); err != nil {
		cleanup.Failures = append(cleanup.Failures, "run-root cleanup validation failed")
	} else if err := auditM5RemoteRunTreePlain(fixture.env.runRoot); err != nil {
		cleanup.Failures = append(cleanup.Failures, "run-root cleanup reparse audit failed")
	} else if err := os.RemoveAll(fixture.env.runRoot); err != nil {
		cleanup.Failures = append(cleanup.Failures, "remove exact remote TC10 run root")
	}
	if _, err := os.Lstat(fixture.env.runRoot); !errors.Is(err, os.ErrNotExist) {
		cleanup.RunRootResidue = 1
	}
	fixture.assertions["run_root_residue_zero"] = cleanup.RunRootResidue == 0

	host, err := os.Hostname()
	if err != nil {
		host = "UNKNOWN"
		cleanup.Failures = append(cleanup.Failures, "resolve remote Windows hostname")
	}
	secondWindowsStatus := "PENDING_REMOTE_VALIDATION"
	if fixture.status == "PASS" &&
		len(cleanup.Failures) == 0 &&
		len(cleanup.ProcessResidue) == 0 &&
		cleanup.PipeResidue == 0 &&
		cleanup.RunRootResidue == 0 {
		secondWindowsStatus = m5RemoteTC10Status
	} else {
		fixture.status = "FAIL"
	}
	evidence := m5RemoteTC10Evidence{
		SchemaVersion:          1,
		RunID:                  fixture.env.runID,
		Host:                   host,
		WindowsVersion:         fixture.windowsVersion,
		WindowsBuild:           fixture.windowsBuild,
		GOARCH:                 runtime.GOARCH,
		HelperSHA256:           fixture.helperSHA256,
		VerifierSHA256:         fixture.verifierSHA256,
		Status:                 fixture.status,
		SecondWindowsStatus:    secondWindowsStatus,
		StartedUTC:             fixture.started.Format(time.RFC3339Nano),
		CompletedUTC:           time.Now().UTC().Format(time.RFC3339Nano),
		HelperPID:              fixture.helperPID,
		HelloPID:               fixture.helloPID,
		PipeName:               fixture.env.pipeName,
		Assertions:             fixture.assertions,
		ProtectedMediaAccesses: 0,
		Cleanup:                cleanup,
	}
	raw, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		t.Errorf("marshal remote TC10 evidence: %v", err)
		return
	}
	if len(raw) > 64<<10 || bytes.Contains(raw, []byte("postgres://")) {
		t.Error("remote TC10 evidence is unbounded or contains a database URL")
		return
	}
	evidencePath := filepath.Join(
		fixture.env.evidenceDir,
		"m5-remote-tc10-"+fixture.env.runID+".json",
	)
	file, err := os.OpenFile(
		evidencePath,
		os.O_CREATE|os.O_WRONLY|os.O_EXCL,
		0o600,
	)
	if err != nil {
		t.Errorf("create remote TC10 evidence: %v", err)
		return
	}
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		t.Errorf("write remote TC10 evidence: %v", err)
		return
	}
	if err := file.Close(); err != nil {
		t.Errorf("close remote TC10 evidence: %v", err)
		return
	}
	fmt.Printf(
		"M5_REMOTE_TC10 run_id=%s status=%s evidence=%s\n",
		fixture.env.runID,
		secondWindowsStatus,
		filepath.Base(evidencePath),
	)
	if fixture.status != "PASS" {
		t.Errorf("remote TC10 did not complete with zero residue: %v", cleanup.Failures)
	}
}

func auditM5RemoteRunTreePlain(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		pathPtr, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return err
		}
		attributes, err := windows.GetFileAttributes(pathPtr)
		if err != nil {
			return err
		}
		if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return errors.New("remote TC10 run tree contains a reparse point")
		}
		return nil
	})
}

func loadM5HarnessEnv(t *testing.T, requireComponents bool) m5HarnessEnv {
	t.Helper()
	required := func(name string) string {
		value := os.Getenv(name)
		if value == "" {
			t.Fatalf("enabled M5 E2E missing %s", name)
		}
		return value
	}
	parsePID := func(name string) int {
		raw := required(name)
		var pid int
		if _, err := fmt.Sscanf(raw, "%d", &pid); err != nil || pid <= 0 {
			t.Fatalf("enabled M5 E2E has invalid %s", name)
		}
		return pid
	}
	env := m5HarnessEnv{
		workspace:           required("M5_E2E_WORKSPACE"),
		runID:               required("M5_E2E_RUN_ID"),
		runRoot:             required("M5_E2E_RUN_ROOT"),
		generated:           required("M5_E2E_GENERATED_ROOT"),
		drive:               required("M5_E2E_DRIVE"),
		pipeName:            required("M5_E2E_PIPE"),
		schema:              required("M5_E2E_SCHEMA"),
		adminDSN:            required("M5_E2E_ADMIN_DSN"),
		scopedDSN:           required("M5_E2E_SCOPED_DSN"),
		helperExe:           required("M5_E2E_HELPER_EXE"),
		agentExe:            required("M5_E2E_AGENT_EXE"),
		guiExe:              required("M5_E2E_GUI_EXE"),
		evidenceDir:         required("M5_E2E_EVIDENCE_DIR"),
		machineID:           required("M5_E2E_MACHINE_ID"),
		secondWindowsStatus: required("M5_E2E_SECOND_WINDOWS_STATUS"),
	}
	requireM5Schema(t, env.schema)
	if err := validateM5SecondWindowsStatus(env.secondWindowsStatus); err != nil {
		t.Fatal(err)
	}
	if requireComponents {
		env.helperConfig = required("M5_E2E_HELPER_CONFIG")
		env.agentConfig = required("M5_E2E_AGENT_CONFIG")
		env.guiConfig = required("M5_E2E_GUI_CONFIG")
		env.agentData = required("M5_E2E_AGENT_DATA")
		env.baseURL = required("M5_E2E_GUI_URL")
		env.helperPID = parsePID("M5_E2E_HELPER_PID")
		env.agentPID = parsePID("M5_E2E_AGENT_PID")
		env.guiPID = parsePID("M5_E2E_GUI_PID")
	}
	return env
}

func validateM5SecondWindowsStatus(status string) error {
	if status != m5RemoteTC10Status {
		return fmt.Errorf(
			"second Windows status %q is not exact %s",
			status,
			m5RemoteTC10Status,
		)
	}
	return nil
}

func requireM5Schema(t *testing.T, schema string) {
	t.Helper()
	if len(schema) < len("m5_e2e_")+8 || len(schema) > 80 ||
		!strings.HasPrefix(schema, "m5_e2e_") {
		t.Fatalf("invalid recorded M5 schema identifier %q", schema)
	}
	for _, character := range schema {
		if character >= 'a' && character <= 'z' ||
			character >= '0' && character <= '9' ||
			character == '_' {
			continue
		}
		t.Fatalf("invalid recorded M5 schema identifier %q", schema)
	}
}

func redactM5Error(err error) error {
	if err == nil {
		return nil
	}
	text := err.Error()
	for {
		start := strings.Index(text, "postgres://")
		if start < 0 {
			break
		}
		end := len(text)
		for index := start; index < len(text); index++ {
			switch text[index] {
			case ' ', '\t', '\r', '\n', '"', '\'':
				end = index
				index = len(text)
			}
		}
		text = text[:start] + "postgres://[REDACTED]" + text[end:]
	}
	return errors.New(text)
}

func openM5Fixture(t *testing.T) *m5Fixture {
	t.Helper()
	env := loadM5HarnessEnv(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	fixture := &m5Fixture{
		t:      t,
		ctx:    ctx,
		cancel: cancel,
		env:    env,
		client: &http.Client{Timeout: 5 * time.Second},
	}
	t.Cleanup(cancel)
	fixture.validateHarnessPaths(t)
	fixture.track("component", env.helperExe)
	fixture.track("component", env.agentExe)
	fixture.track("component", env.guiExe)
	fixture.track("config", env.helperConfig)
	fixture.track("config", env.agentConfig)
	fixture.track("config", env.guiConfig)
	fixture.track("generated", env.runRoot)
	fixture.track("generated", env.generated)
	fixture.track("pipe", env.pipeName)
	fixture.pids = append(fixture.pids, env.helperPID, env.agentPID, env.guiPID)

	pool, err := pgxpool.New(ctx, env.scopedDSN)
	if err != nil {
		t.Fatal(redactM5Error(err))
	}
	fixture.pg = pool
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(redactM5Error(err))
	}
	var currentSchema string
	if err := pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&currentSchema); err != nil {
		t.Fatal(redactM5Error(err))
	}
	if currentSchema != env.schema {
		t.Fatalf("acceptance current_schema=%q, want %q", currentSchema, env.schema)
	}

	sqlitePath := filepath.Join(env.agentData, "agent.db")
	fixture.track("sqlite", sqlitePath)
	sqliteDB, err := sql.Open(
		"sqlite",
		fmt.Sprintf(
			"file:%s?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)",
			filepath.ToSlash(sqlitePath),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	fixture.sqlite = sqliteDB
	t.Cleanup(func() { _ = sqliteDB.Close() })
	waitM5(t, ctx, "Agent SQLite ready", func() (bool, error) {
		return true, sqliteDB.PingContext(ctx)
	})
	if err := installM5PendingSyncAudit(ctx, sqliteDB); err != nil {
		t.Fatal(err)
	}

	fixture.assertProcessIdentity(t, env.helperPID, env.helperExe)
	fixture.assertProcessIdentity(t, env.agentPID, env.agentExe)
	fixture.assertProcessIdentity(t, env.guiPID, env.guiExe)
	fixture.waitAgentOnline(t)
	conn, hello := fixture.dialHelper(t)
	_ = conn.Close()
	if hello.PID != env.helperPID {
		t.Fatalf("initial Helper Hello PID=%d, harness PID=%d", hello.PID, env.helperPID)
	}
	return fixture
}

func (fixture *m5Fixture) validateHarnessPaths(t *testing.T) {
	t.Helper()
	workspace, err := filepath.Abs(fixture.env.workspace)
	if err != nil {
		t.Fatal(err)
	}
	runRoot, err := filepath.Abs(fixture.env.runRoot)
	if err != nil {
		t.Fatal(err)
	}
	tmpRoot := filepath.Join(workspace, ".superpowers", "tmp")
	if filepath.Dir(runRoot) != tmpRoot ||
		!strings.HasPrefix(filepath.Base(runRoot), "m5-delete-") {
		t.Fatalf("run root %q is not the exact scoped M5 child", runRoot)
	}
	info, err := os.Lstat(runRoot)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("run root is not a plain existing directory: %v", err)
	}
	if !strings.EqualFold(
		filepath.Clean(fixture.env.generated),
		filepath.Clean(fixture.env.drive+`\generated`),
	) {
		t.Fatalf("generated mapped root %q does not match recorded drive", fixture.env.generated)
	}
	for _, path := range []string{
		fixture.env.helperExe,
		fixture.env.agentExe,
		fixture.env.guiExe,
		fixture.env.helperConfig,
		fixture.env.agentConfig,
		fixture.env.guiConfig,
	} {
		info, err := os.Lstat(path)
		if err != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("required component/config leaf %q: %v", path, err)
		}
	}
}

func (fixture *m5Fixture) finishEvidence() {
	fixture.restartedMu.Lock()
	processes := append([]*m5RunningProcess(nil), fixture.restartedHelpers...)
	fixture.restartedMu.Unlock()
	for _, process := range processes {
		if process != nil && process.isAlive() {
			_ = process.killExact()
		}
	}
	records, protectedAccesses := fixture.assertLedger(fixture.t)
	evidence := m5Evidence{
		SchemaVersion:       1,
		RunID:               fixture.env.runID,
		Schema:              fixture.env.schema,
		PipeName:            fixture.env.pipeName,
		DriveLetter:         fixture.env.drive,
		SecondWindowsStatus: fixture.env.secondWindowsStatus,
		TC:                  fixture.results,
		AccessLedger:        records,
		ProtectedAccesses:   protectedAccesses,
		TaskIDs:             append([]string(nil), fixture.taskIDs...),
		ComponentPIDs:       append([]int(nil), fixture.pids...),
	}
	raw, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		fixture.t.Errorf("marshal M5 evidence: %v", err)
		return
	}
	path := filepath.Join(
		fixture.env.evidenceDir,
		"m5-"+fixture.env.runID+"-tc-matrix.json",
	)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		fixture.t.Errorf("write M5 evidence: %v", err)
		return
	}
	if bytes.Contains(raw, []byte("postgres://")) {
		fixture.t.Errorf("credential-shaped PostgreSQL URL entered M5 evidence")
		return
	}
	fmt.Printf("M5_ACCEPTANCE run_id=%s evidence=%s\n", fixture.env.runID, filepath.Base(path))
}

func (fixture *m5Fixture) track(kind, path string) {
	fixture.ledgerMu.Lock()
	fixture.ledger = append(fixture.ledger, m5AccessRecord{Kind: kind, Path: path})
	fixture.ledgerMu.Unlock()
}

func m5PathOverlapsProtected(path string) bool {
	if len(path) < 3 ||
		(path[0] < 'A' || path[0] > 'Z') &&
			(path[0] < 'a' || path[0] > 'z') ||
		path[1] != ':' ||
		path[2] != '\\' && path[2] != '/' {
		return false
	}
	clean := filepath.Clean(path)
	for _, protected := range m5ProtectedMediaRoots {
		protected = filepath.Clean(protected)
		if strings.EqualFold(clean, protected) ||
			strings.HasPrefix(
				strings.ToLower(clean),
				strings.ToLower(protected+string(os.PathSeparator)),
			) ||
			strings.HasPrefix(
				strings.ToLower(protected),
				strings.ToLower(clean+string(os.PathSeparator)),
			) {
			return true
		}
	}
	return false
}

func countM5ProtectedAccesses(records []m5AccessRecord) int {
	count := 0
	for _, record := range records {
		if m5PathOverlapsProtected(record.Path) {
			count++
		}
	}
	return count
}

func (fixture *m5Fixture) assertLedger(t *testing.T) ([]m5AccessRecord, int) {
	t.Helper()
	fixture.ledgerMu.Lock()
	records := append([]m5AccessRecord(nil), fixture.ledger...)
	fixture.ledgerMu.Unlock()
	protectedAccesses := countM5ProtectedAccesses(records)
	if protectedAccesses != 0 {
		t.Errorf("access ledger recorded %d protected-media overlaps", protectedAccesses)
	}
	runRoot := filepath.Clean(fixture.env.runRoot)
	mappedPrefix := strings.ToLower(filepath.Clean(fixture.env.drive + `\`))
	allowedLeaves := map[string]bool{}
	for _, path := range []string{
		fixture.env.helperExe,
		fixture.env.agentExe,
		fixture.env.guiExe,
		fixture.env.helperConfig,
		fixture.env.agentConfig,
		fixture.env.guiConfig,
	} {
		allowedLeaves[strings.ToLower(filepath.Clean(path))] = true
	}
	for _, record := range records {
		value := filepath.Clean(record.Path)
		lower := strings.ToLower(value)
		switch record.Kind {
		case "pipe":
			if record.Path != fixture.env.pipeName {
				t.Errorf("access ledger has foreign pipe %q", record.Path)
			}
		case "url":
			if !strings.HasPrefix(record.Path, fixture.env.baseURL+"/") {
				t.Errorf("access ledger has foreign URL %q", record.Path)
			}
		case "component", "config":
			if !allowedLeaves[lower] {
				t.Errorf("access ledger has unrecorded component/config %q", record.Path)
			}
		default:
			physical := strings.EqualFold(value, runRoot) ||
				strings.HasPrefix(
					strings.ToLower(value),
					strings.ToLower(runRoot+string(os.PathSeparator)),
				)
			mapped := lower == strings.TrimSuffix(mappedPrefix, `\`) ||
				strings.HasPrefix(lower, mappedPrefix)
			if !physical && !mapped {
				t.Errorf("access ledger path escaped generated run: kind=%s path=%q", record.Kind, record.Path)
			}
		}
	}
	return records, protectedAccesses
}

func waitM5(
	t *testing.T,
	ctx context.Context,
	description string,
	check func() (bool, error),
) {
	t.Helper()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		ok, err := check()
		if ok && err == nil {
			return
		}
		if err != nil {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for %s: %v (last error: %v)", description, ctx.Err(), lastErr)
		case <-ticker.C:
		}
	}
}

func (fixture *m5Fixture) waitAgentOnline(t *testing.T) {
	t.Helper()
	fixture.waitAgentState(t, true, "real Agent online in GUI", 10*time.Second)
}

func (fixture *m5Fixture) waitAgentOffline(t *testing.T) {
	t.Helper()
	fixture.waitAgentState(t, false, "real Agent offline in GUI", 5*time.Second)
}

func (fixture *m5Fixture) waitAgentState(
	t *testing.T,
	wantOnline bool,
	description string,
	timeout time.Duration,
) {
	t.Helper()
	fixture.track("url", fixture.env.baseURL+"/api/agents")
	ctx, cancel := context.WithTimeout(fixture.ctx, timeout)
	defer cancel()
	waitM5(t, ctx, description, func() (bool, error) {
		request, err := http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			fixture.env.baseURL+"/api/agents",
			nil,
		)
		if err != nil {
			return false, err
		}
		response, err := fixture.client.Do(request)
		if err != nil {
			return false, err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return false, fmt.Errorf("agents status=%d", response.StatusCode)
		}
		var agents []struct {
			MachineID string `json:"machine_id"`
			Online    bool   `json:"online"`
		}
		if err := json.NewDecoder(response.Body).Decode(&agents); err != nil {
			return false, err
		}
		for _, agent := range agents {
			if agent.MachineID == fixture.env.machineID {
				return agent.Online == wantOnline, nil
			}
		}
		return !wantOnline, nil
	})
}

func (fixture *m5Fixture) assertProcessIdentity(
	t *testing.T,
	pid int,
	expectedExe string,
) {
	t.Helper()
	handle, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE,
		false,
		uint32(pid),
	)
	if err != nil {
		t.Fatalf("open recorded PID %d: %v", pid, err)
	}
	defer windows.CloseHandle(handle)
	buffer := make([]uint16, 32768)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(
		handle,
		0,
		&buffer[0],
		&size,
	); err != nil {
		t.Fatalf("query recorded PID %d identity: %v", pid, err)
	}
	actual := filepath.Clean(windows.UTF16ToString(buffer[:size]))
	expected, err := filepath.Abs(expectedExe)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(actual, filepath.Clean(expected)) {
		t.Fatalf("PID %d identity=%q, want %q", pid, actual, expected)
	}
}

func m5ProcessAlive(pid int) bool {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	result, err := windows.WaitForSingleObject(handle, 0)
	return err == nil && result == uint32(windows.WAIT_TIMEOUT)
}

func waitM5ProcessExit(t *testing.T, ctx context.Context, pid int) {
	t.Helper()
	waitM5(t, ctx, fmt.Sprintf("PID %d exit", pid), func() (bool, error) {
		return !m5ProcessAlive(pid), nil
	})
}

func (fixture *m5Fixture) dialHelper(
	t *testing.T,
) (*proto.Conn, proto.Hello) {
	t.Helper()
	fixture.track("pipe", fixture.env.pipeName)
	ctx, cancel := context.WithTimeout(fixture.ctx, 5*time.Second)
	defer cancel()
	connection, err := winio.DialPipeContext(ctx, fixture.env.pipeName)
	if err != nil {
		t.Fatalf("dial real Helper pipe: %v", err)
	}
	framed := proto.NewConn(connection)
	if err := framed.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		_ = framed.Close()
		t.Fatal(err)
	}
	messageType, body, err := framed.ReadFrame()
	if err != nil {
		_ = framed.Close()
		t.Fatalf("read real Helper Hello: %v", err)
	}
	if messageType != proto.MsgHello {
		_ = framed.Close()
		t.Fatalf("real Helper first message=%d, want Hello", messageType)
	}
	decoded, err := proto.Decode(messageType, body)
	if err != nil {
		_ = framed.Close()
		t.Fatal(err)
	}
	hello, ok := decoded.(*proto.Hello)
	if !ok || hello.Role != "delete-helper" ||
		hello.Version != proto.ProtocolVersion || hello.PID <= 0 {
		_ = framed.Close()
		t.Fatalf("real Helper Hello=%#v", decoded)
	}
	return framed, *hello
}

func (fixture *m5Fixture) directDelete(
	t *testing.T,
	task proto.DeleteTask,
) (proto.DeleteReport, proto.Hello) {
	t.Helper()
	framed, hello := fixture.dialHelper(t)
	defer framed.Close()
	framed.SetWriteTimeout(5 * time.Second)
	if err := framed.WriteFrame(proto.MsgDeleteTask, &task); err != nil {
		t.Fatalf("write direct DeleteTask: %v", err)
	}
	if err := framed.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	messageType, body, err := framed.ReadFrame()
	if err != nil {
		t.Fatalf("read direct DeleteReport: %v", err)
	}
	if messageType != proto.MsgDeleteReport {
		t.Fatalf("direct response type=%d, want DeleteReport", messageType)
	}
	decoded, err := proto.Decode(messageType, body)
	if err != nil {
		t.Fatal(err)
	}
	report, ok := decoded.(*proto.DeleteReport)
	if !ok {
		t.Fatalf("direct response decoded as %T", decoded)
	}
	return *report, hello
}

func (fixture *m5Fixture) shutdownHelper(
	t *testing.T,
	expectedPID int,
) {
	t.Helper()
	framed, hello := fixture.dialHelper(t)
	if hello.PID != expectedPID {
		_ = framed.Close()
		t.Fatalf("Shutdown Hello PID=%d, want exact PID %d", hello.PID, expectedPID)
	}
	framed.SetWriteTimeout(5 * time.Second)
	if err := framed.WriteFrame(proto.MsgShutdown, &proto.Shutdown{}); err != nil {
		_ = framed.Close()
		t.Fatalf("write Helper Shutdown: %v", err)
	}
	_ = framed.Close()
	exitCtx, cancel := context.WithTimeout(fixture.ctx, 10*time.Second)
	defer cancel()
	waitM5ProcessExit(t, exitCtx, expectedPID)
	fixture.waitPipeUnavailable(t)
}

func (fixture *m5Fixture) waitPipeUnavailable(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(fixture.ctx, 5*time.Second)
	defer cancel()
	waitM5(t, ctx, "Helper pipe unavailable", func() (bool, error) {
		dialCtx, dialCancel := context.WithTimeout(ctx, 50*time.Millisecond)
		connection, err := winio.DialPipeContext(dialCtx, fixture.env.pipeName)
		dialCancel()
		if connection != nil {
			_ = connection.Close()
			return false, nil
		}
		return err != nil, nil
	})
}

func (fixture *m5Fixture) startHelper(t *testing.T) *m5RunningProcess {
	t.Helper()
	stdoutPath := filepath.Join(
		fixture.env.runRoot,
		"runtime",
		fmt.Sprintf("helper-restart-%d.stdout.log", time.Now().UnixNano()),
	)
	stderrPath := strings.TrimSuffix(stdoutPath, ".stdout.log") + ".stderr.log"
	fixture.track("log", stdoutPath)
	fixture.track("log", stderrPath)
	stdout, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		_ = stdout.Close()
		t.Fatal(err)
	}
	command := exec.Command(fixture.env.helperExe, "-config", fixture.env.helperConfig)
	command.Stdout = stdout
	command.Stderr = stderr
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := command.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		t.Fatalf("start exact passed Helper: %v", err)
	}
	process := &m5RunningProcess{
		cmd: command, exe: fixture.env.helperExe, stdout: stdout, stderr: stderr,
	}
	fixture.restartedMu.Lock()
	fixture.restartedHelpers = append(fixture.restartedHelpers, process)
	fixture.restartedMu.Unlock()
	fixture.pids = append(fixture.pids, command.Process.Pid)
	fixture.assertProcessIdentity(t, command.Process.Pid, fixture.env.helperExe)
	readyCtx, cancel := context.WithTimeout(fixture.ctx, 10*time.Second)
	defer cancel()
	waitM5(t, readyCtx, "restarted Helper Hello", func() (bool, error) {
		dialCtx, dialCancel := context.WithTimeout(readyCtx, 100*time.Millisecond)
		connection, err := winio.DialPipeContext(dialCtx, fixture.env.pipeName)
		dialCancel()
		if err != nil {
			if !process.isAlive() {
				return false, fmt.Errorf("Helper exited: %v", process.wait())
			}
			return false, nil
		}
		framed := proto.NewConn(connection)
		_ = framed.SetReadDeadline(time.Now().Add(time.Second))
		messageType, body, readErr := framed.ReadFrame()
		_ = framed.Close()
		if readErr != nil || messageType != proto.MsgHello {
			return false, readErr
		}
		decoded, decodeErr := proto.Decode(messageType, body)
		if decodeErr != nil {
			return false, decodeErr
		}
		hello, ok := decoded.(*proto.Hello)
		if !ok || hello.PID != command.Process.Pid {
			return false, fmt.Errorf("restarted Hello=%#v", decoded)
		}
		return true, nil
	})
	return process
}

func (process *m5RunningProcess) wait() error {
	process.waitOnce.Do(func() {
		process.waitErr = process.cmd.Wait()
		_ = process.stdout.Close()
		_ = process.stderr.Close()
	})
	return process.waitErr
}

func (process *m5RunningProcess) isAlive() bool {
	if process == nil || process.cmd == nil || process.cmd.Process == nil {
		return false
	}
	return m5ProcessAlive(process.cmd.Process.Pid)
}

func (process *m5RunningProcess) killExact() error {
	if process == nil || process.cmd == nil || process.cmd.Process == nil {
		return nil
	}
	if process.isAlive() {
		if err := process.cmd.Process.Kill(); err != nil {
			return err
		}
	}
	_ = process.wait()
	return nil
}

func (fixture *m5Fixture) postJSON(
	t *testing.T,
	path string,
	input any,
	wantStatus int,
	output any,
) {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	url := fixture.env.baseURL + path
	fixture.track("url", url)
	ctx, cancel := context.WithTimeout(fixture.ctx, 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		url,
		bytes.NewReader(raw),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := fixture.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != wantStatus {
		t.Fatalf("POST %s status=%d body=%s", path, response.StatusCode, body)
	}
	if output != nil {
		if err := json.Unmarshal(body, output); err != nil {
			t.Fatalf("decode POST %s: %v body=%s", path, err, body)
		}
	}
}

func (fixture *m5Fixture) prepareDelete(
	t *testing.T,
	memberIDs []int64,
) string {
	t.Helper()
	var response struct {
		ConfirmToken string        `json:"confirm_token"`
		Summary      DeleteSummary `json:"summary"`
	}
	fixture.postJSON(
		t,
		"/api/delete/prepare",
		map[string]any{"member_ids": memberIDs},
		http.StatusOK,
		&response,
	)
	if response.ConfirmToken == "" ||
		response.Summary.TotalFiles != int64(len(memberIDs)) {
		t.Fatalf("prepare response=%#v members=%d", response, len(memberIDs))
	}
	return response.ConfirmToken
}

func (fixture *m5Fixture) executeDelete(
	t *testing.T,
	token string,
	mode string,
) string {
	t.Helper()
	var response struct {
		TaskID string `json:"task_id"`
	}
	fixture.postJSON(
		t,
		"/api/delete/execute",
		map[string]any{"confirm_token": token, "mode": mode},
		http.StatusAccepted,
		&response,
	)
	parsed, err := uuid.Parse(response.TaskID)
	if err != nil || parsed.String() != response.TaskID {
		t.Fatalf("execute task ID=%q", response.TaskID)
	}
	fixture.taskIDs = append(fixture.taskIDs, response.TaskID)
	return response.TaskID
}

func (fixture *m5Fixture) deleteStatus(
	ctx context.Context,
	taskID string,
) (DeleteTaskStatus, error) {
	url := fixture.env.baseURL + "/api/delete/tasks/" + taskID
	fixture.track("url", url)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return DeleteTaskStatus{}, err
	}
	response, err := fixture.client.Do(request)
	if err != nil {
		return DeleteTaskStatus{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return DeleteTaskStatus{}, fmt.Errorf("status=%d body=%s", response.StatusCode, body)
	}
	var status DeleteTaskStatus
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		return DeleteTaskStatus{}, err
	}
	return status, nil
}

func (fixture *m5Fixture) waitDeleteStatus(
	t *testing.T,
	taskID string,
	predicate func(DeleteTaskStatus) bool,
	description string,
) DeleteTaskStatus {
	t.Helper()
	ctx, cancel := context.WithTimeout(fixture.ctx, 2*time.Minute)
	defer cancel()
	var latest DeleteTaskStatus
	waitM5(t, ctx, description, func() (bool, error) {
		status, err := fixture.deleteStatus(ctx, taskID)
		if err != nil {
			return false, err
		}
		latest = status
		return predicate(status), nil
	})
	return latest
}

func (fixture *m5Fixture) waitDeleteComplete(
	t *testing.T,
	taskID string,
) DeleteTaskStatus {
	t.Helper()
	status := fixture.waitDeleteStatus(
		t,
		taskID,
		func(status DeleteTaskStatus) bool { return status.Complete },
		"delete task "+taskID+" terminal",
	)
	if status.TaskID != taskID || status.Pending != 0 {
		t.Fatalf("terminal status=%#v", status)
	}
	return status
}

func (fixture *m5Fixture) writeGenerated(
	t *testing.T,
	relative string,
	data []byte,
) string {
	t.Helper()
	if relative == "" || filepath.IsAbs(relative) {
		t.Fatalf("generated fixture relative path=%q", relative)
	}
	path := filepath.Join(fixture.env.generated, relative)
	root := filepath.Clean(fixture.env.generated)
	clean := filepath.Clean(path)
	if !strings.HasPrefix(
		strings.ToLower(clean),
		strings.ToLower(root+string(os.PathSeparator)),
	) {
		t.Fatalf("generated fixture escaped root: %q", path)
	}
	fixture.track("generated", path)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func m5Digest(data []byte) string {
	sum := sha512.Sum512(data)
	return hex.EncodeToString(sum[:])
}

func m5FileDigest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return m5Digest(data)
}

func captureM5FileInvariants(paths []string) ([]m5FileInvariant, error) {
	if len(paths) == 0 {
		return nil, errors.New("capture M5 file invariants: no paths")
	}
	seen := make(map[string]struct{}, len(paths))
	invariants := make([]m5FileInvariant, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			return nil, errors.New("capture M5 file invariants: empty path")
		}
		key := strings.ToLower(filepath.Clean(path))
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("capture M5 file invariants: duplicate path %q", path)
		}
		seen[key] = struct{}{}
		info, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("capture M5 file invariant %q: %w", path, err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("capture M5 file invariant %q: not a plain file", path)
		}
		pathUTF16, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return nil, fmt.Errorf("capture M5 file invariant path %q: %w", path, err)
		}
		attributes, err := windows.GetFileAttributes(pathUTF16)
		if err != nil {
			return nil, fmt.Errorf("capture M5 file invariant attributes %q: %w", path, err)
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("capture M5 file invariant bytes %q: %w", path, err)
		}
		hasher := sha512.New()
		_, copyErr := io.Copy(hasher, file)
		closeErr := file.Close()
		if copyErr != nil {
			return nil, fmt.Errorf("capture M5 file invariant bytes %q: %w", path, copyErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("capture M5 file invariant close %q: %w", path, closeErr)
		}
		invariants = append(invariants, m5FileInvariant{
			Path:            path,
			SHA512:          hex.EncodeToString(hasher.Sum(nil)),
			Size:            info.Size(),
			ModTimeUnixNano: info.ModTime().UnixNano(),
			Mode:            info.Mode(),
			Attributes:      attributes,
		})
	}
	sort.Slice(invariants, func(left, right int) bool {
		return strings.ToLower(invariants[left].Path) <
			strings.ToLower(invariants[right].Path)
	})
	return invariants, nil
}

func verifyM5FileInvariants(baseline []m5FileInvariant) error {
	if len(baseline) == 0 {
		return errors.New("verify M5 file invariants: empty baseline")
	}
	paths := make([]string, len(baseline))
	expected := make(map[string]m5FileInvariant, len(baseline))
	for index, invariant := range baseline {
		key := strings.ToLower(filepath.Clean(invariant.Path))
		if invariant.Path == "" {
			return errors.New("verify M5 file invariants: empty baseline path")
		}
		if _, exists := expected[key]; exists {
			return fmt.Errorf(
				"verify M5 file invariants: duplicate baseline path %q",
				invariant.Path,
			)
		}
		expected[key] = invariant
		paths[index] = invariant.Path
	}
	observed, err := captureM5FileInvariants(paths)
	if err != nil {
		return err
	}
	if len(observed) != len(expected) {
		return fmt.Errorf(
			"verify M5 file invariants: observed %d files, want %d",
			len(observed),
			len(expected),
		)
	}
	for _, invariant := range observed {
		key := strings.ToLower(filepath.Clean(invariant.Path))
		want, exists := expected[key]
		if !exists {
			return fmt.Errorf(
				"verify M5 file invariants: unexpected path %q",
				invariant.Path,
			)
		}
		if invariant != want {
			return fmt.Errorf(
				"verify M5 file invariants: file changed %q",
				invariant.Path,
			)
		}
	}
	return nil
}

func (fixture *m5Fixture) seedCase(
	t *testing.T,
	name string,
	specs []m5SeedSpec,
) m5SeededCase {
	t.Helper()
	if len(specs) == 0 {
		t.Fatal("seed case requires members")
	}
	result := m5SeededCase{
		name:       name,
		pathByName: make(map[string]string, len(specs)),
		idByName:   make(map[string]int64, len(specs)),
		dataByName: make(map[string][]byte, len(specs)),
	}
	repData := []byte("representative-" + fixture.env.runID + "-" + name)
	repPath := fixture.writeGenerated(
		t,
		filepath.Join("representatives", name+".bin"),
		repData,
	)
	repID := fixture.insertCentralFile(t, repPath, repData)
	result.repID = repID
	if err := fixture.pg.QueryRow(fixture.ctx, `
		INSERT INTO dup_groups(kind,representative_file_id,member_count)
		VALUES('exact',$1,$2)
		RETURNING id`,
		repID,
		len(specs)+1,
	).Scan(&result.groupID); err != nil {
		t.Fatal(redactM5Error(err))
	}
	if _, err := fixture.pg.Exec(fixture.ctx, `
		INSERT INTO dup_members(group_id,file_id,score_json)
		VALUES($1,$2,'{}'::jsonb)`,
		result.groupID,
		repID,
	); err != nil {
		t.Fatal(redactM5Error(err))
	}
	for _, spec := range specs {
		if spec.name == "" || spec.path == "" {
			t.Fatalf("invalid seed spec %#v", spec)
		}
		fileID := fixture.insertCentralFile(t, spec.path, spec.data)
		fixture.insertLocalFile(t, spec.path, spec.data)
		if _, err := fixture.pg.Exec(fixture.ctx, `
			INSERT INTO dup_members(group_id,file_id,score_json)
			VALUES($1,$2,'{}'::jsonb)`,
			result.groupID,
			fileID,
		); err != nil {
			t.Fatal(redactM5Error(err))
		}
		result.memberIDs = append(result.memberIDs, fileID)
		result.paths = append(result.paths, spec.path)
		result.pathByName[spec.name] = spec.path
		result.idByName[spec.name] = fileID
		result.dataByName[spec.name] = append([]byte(nil), spec.data...)
	}
	return result
}

func (fixture *m5Fixture) insertCentralFile(
	t *testing.T,
	path string,
	data []byte,
) int64 {
	t.Helper()
	fixture.track("database-path", path)
	var id int64
	err := fixture.pg.QueryRow(fixture.ctx, `
		INSERT INTO files(
			machine_id,disk_no,path,size,mtime,sha512,
			phase1_done,phase2_done,status,missing_mask,updated_at
		)
		VALUES($1,1,$2,$3,$4,$5,1,1,'done',0,$4)
		RETURNING id`,
		fixture.env.machineID,
		path,
		len(data),
		time.Now().UnixMilli(),
		m5Digest(data),
	).Scan(&id)
	if err != nil {
		t.Fatal(redactM5Error(err))
	}
	return id
}

func (fixture *m5Fixture) insertLocalFile(
	t *testing.T,
	path string,
	data []byte,
) {
	t.Helper()
	fixture.track("database-path", path)
	ctx, cancel := context.WithTimeout(fixture.ctx, 15*time.Second)
	defer cancel()
	_, err := fixture.sqlite.ExecContext(ctx, `
		INSERT INTO files(
			machine_id,disk_no,path,size,mtime,sha512,
			phase1_done,phase2_done,status,missing_mask,error,updated_at
		)
		VALUES(?1,1,?2,?3,?4,?5,1,1,'done',0,NULL,?4)
		ON CONFLICT(machine_id,path) DO UPDATE SET
			size=excluded.size,
			mtime=excluded.mtime,
			sha512=excluded.sha512,
			phase1_done=1,
			phase2_done=1,
			status='done',
			missing_mask=0,
			error=NULL,
			updated_at=excluded.updated_at`,
		fixture.env.machineID,
		path,
		len(data),
		time.Now().UnixMilli(),
		m5Digest(data),
	)
	if err != nil {
		t.Fatal(err)
	}
}

func (fixture *m5Fixture) seedLargeCase(
	t *testing.T,
	name string,
	count int,
) m5SeededCase {
	t.Helper()
	if count < 1 || count > 10000 {
		t.Fatalf("large seed count=%d", count)
	}
	specs := make([]m5SeedSpec, count)
	for index := range specs {
		data := []byte{byte(index), byte(index >> 8), byte(index >> 16)}
		path := fixture.writeGenerated(
			t,
			filepath.Join(name, fmt.Sprintf("%05d.bin", index)),
			data,
		)
		specs[index] = m5SeedSpec{
			name: fmt.Sprintf("%05d", index),
			path: path,
			data: data,
		}
	}

	result := m5SeededCase{
		name:       name,
		pathByName: make(map[string]string, count),
		idByName:   make(map[string]int64, count),
		dataByName: make(map[string][]byte, count),
	}
	repData := []byte("representative-" + fixture.env.runID + "-" + name)
	repPath := fixture.writeGenerated(
		t,
		filepath.Join("representatives", name+".bin"),
		repData,
	)
	result.repID = fixture.insertCentralFile(t, repPath, repData)
	if err := fixture.pg.QueryRow(fixture.ctx, `
		INSERT INTO dup_groups(kind,representative_file_id,member_count)
		VALUES('exact',$1,$2)
		RETURNING id`,
		result.repID,
		count+1,
	).Scan(&result.groupID); err != nil {
		t.Fatal(redactM5Error(err))
	}
	if _, err := fixture.pg.Exec(fixture.ctx, `
		INSERT INTO dup_members(group_id,file_id,score_json)
		VALUES($1,$2,'{}'::jsonb)`,
		result.groupID,
		result.repID,
	); err != nil {
		t.Fatal(redactM5Error(err))
	}

	paths := make([]string, count)
	sizes := make([]int64, count)
	mtimes := make([]int64, count)
	hashes := make([]string, count)
	names := make([]string, count)
	for index, spec := range specs {
		paths[index] = spec.path
		sizes[index] = int64(len(spec.data))
		mtimes[index] = time.Now().UnixMilli() + int64(index)
		hashes[index] = m5Digest(spec.data)
		names[index] = spec.name
		fixture.track("database-path", spec.path)
	}
	rows, err := fixture.pg.Query(fixture.ctx, `
		INSERT INTO files(
			machine_id,disk_no,path,size,mtime,sha512,
			phase1_done,phase2_done,status,missing_mask,updated_at
		)
		SELECT $1,1,input.path,input.size,input.mtime,input.sha,1,1,'done',0,input.mtime
		FROM unnest($2::text[],$3::bigint[],$4::bigint[],$5::text[])
		     AS input(path,size,mtime,sha)
		RETURNING id,path`,
		fixture.env.machineID,
		paths,
		sizes,
		mtimes,
		hashes,
	)
	if err != nil {
		t.Fatal(redactM5Error(err))
	}
	idsByPath := make(map[string]int64, count)
	for rows.Next() {
		var id int64
		var path string
		if err := rows.Scan(&id, &path); err != nil {
			rows.Close()
			t.Fatal(redactM5Error(err))
		}
		idsByPath[path] = id
	}
	rows.Close()
	if len(idsByPath) != count {
		t.Fatalf("large central seed returned %d IDs, want %d", len(idsByPath), count)
	}
	transaction, err := fixture.sqlite.BeginTx(fixture.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = transaction.Rollback()
		}
	}()
	statement, err := transaction.PrepareContext(fixture.ctx, `
		INSERT INTO files(
			machine_id,disk_no,path,size,mtime,sha512,
			phase1_done,phase2_done,status,missing_mask,error,updated_at
		)
		VALUES(?1,1,?2,?3,?4,?5,1,1,'done',0,NULL,?4)`)
	if err != nil {
		t.Fatal(err)
	}
	for index, path := range paths {
		if _, err := statement.ExecContext(
			fixture.ctx,
			fixture.env.machineID,
			path,
			sizes[index],
			mtimes[index],
			hashes[index],
		); err != nil {
			_ = statement.Close()
			t.Fatal(err)
		}
	}
	if err := statement.Close(); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
	committed = true

	result.memberIDs = make([]int64, count)
	result.paths = append([]string(nil), paths...)
	for index, path := range paths {
		result.memberIDs[index] = idsByPath[path]
		result.pathByName[names[index]] = path
		result.idByName[names[index]] = idsByPath[path]
		result.dataByName[names[index]] = append([]byte(nil), specs[index].data...)
	}
	if _, err := fixture.pg.Exec(fixture.ctx, `
		INSERT INTO dup_members(group_id,file_id,score_json)
		SELECT $1,id,'{}'::jsonb
		FROM files
		WHERE machine_id=$2 AND path=ANY($3::text[])`,
		result.groupID,
		fixture.env.machineID,
		paths,
	); err != nil {
		t.Fatal(redactM5Error(err))
	}
	return result
}

type m5PendingObserver struct {
	ctx      context.Context
	database *sql.DB
	baseline int64
	expected map[string]struct{}
}

type m5NoPendingObserver struct {
	ctx      context.Context
	database *sql.DB
	baseline int64
	expected map[string]struct{}
}

const m5PendingSyncAuditDDL = `
CREATE TABLE IF NOT EXISTS m5_e2e_sync_pending_audit (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	table_name TEXT NOT NULL,
	row_pk TEXT NOT NULL,
	generation INTEGER NOT NULL
);
CREATE TRIGGER IF NOT EXISTS m5_e2e_sync_pending_insert
AFTER INSERT ON sync_queue
WHEN NEW.synced = 0
BEGIN
	INSERT INTO m5_e2e_sync_pending_audit(table_name,row_pk,generation)
	VALUES(NEW.table_name,NEW.row_pk,NEW.generation);
END;
CREATE TRIGGER IF NOT EXISTS m5_e2e_sync_pending_update
AFTER UPDATE OF synced,generation ON sync_queue
WHEN NEW.synced = 0 AND (
	OLD.synced <> NEW.synced OR OLD.generation <> NEW.generation
)
BEGIN
	INSERT INTO m5_e2e_sync_pending_audit(table_name,row_pk,generation)
	VALUES(NEW.table_name,NEW.row_pk,NEW.generation);
END;
`

func installM5PendingSyncAudit(ctx context.Context, database *sql.DB) error {
	if database == nil {
		return errors.New("install M5 pending sync audit: nil database")
	}
	if _, err := database.ExecContext(ctx, m5PendingSyncAuditDDL); err != nil {
		return fmt.Errorf("install M5 pending sync audit: %w", err)
	}
	return nil
}

func newM5PendingObserver(
	ctx context.Context,
	database *sql.DB,
	expectedPaths []string,
) (*m5PendingObserver, error) {
	if database == nil {
		return nil, errors.New("new M5 pending observer: nil database")
	}
	if len(expectedPaths) == 0 {
		return nil, errors.New("new M5 pending observer: no expected paths")
	}
	expected := make(map[string]struct{}, len(expectedPaths))
	for _, path := range expectedPaths {
		if path == "" {
			return nil, errors.New("new M5 pending observer: empty expected path")
		}
		if _, exists := expected[path]; exists {
			return nil, fmt.Errorf(
				"new M5 pending observer: duplicate expected path %q",
				path,
			)
		}
		expected[path] = struct{}{}
	}
	var baseline int64
	if err := database.QueryRowContext(
		ctx,
		`SELECT COALESCE(max(seq),0) FROM m5_e2e_sync_pending_audit`,
	).Scan(&baseline); err != nil {
		return nil, fmt.Errorf("read M5 pending sync audit baseline: %w", err)
	}
	return &m5PendingObserver{
		ctx:      ctx,
		database: database,
		baseline: baseline,
		expected: expected,
	}, nil
}

func newM5NoPendingObserver(
	ctx context.Context,
	database *sql.DB,
	expectedPaths []string,
) (*m5NoPendingObserver, error) {
	if database == nil {
		return nil, errors.New("new M5 no-pending observer: nil database")
	}
	if len(expectedPaths) == 0 {
		return nil, errors.New("new M5 no-pending observer: no expected paths")
	}
	expected := make(map[string]struct{}, len(expectedPaths))
	for _, path := range expectedPaths {
		if path == "" {
			return nil, errors.New("new M5 no-pending observer: empty expected path")
		}
		if _, exists := expected[path]; exists {
			return nil, fmt.Errorf(
				"new M5 no-pending observer: duplicate expected path %q",
				path,
			)
		}
		expected[path] = struct{}{}
	}
	var baseline int64
	if err := database.QueryRowContext(
		ctx,
		`SELECT COALESCE(max(seq),0) FROM m5_e2e_sync_pending_audit`,
	).Scan(&baseline); err != nil {
		return nil, fmt.Errorf("read M5 no-pending audit baseline: %w", err)
	}
	return &m5NoPendingObserver{
		ctx:      ctx,
		database: database,
		baseline: baseline,
		expected: expected,
	}, nil
}

func (observer *m5NoPendingObserver) verify() error {
	rows, err := observer.database.QueryContext(observer.ctx, `
		SELECT files.path
		FROM m5_e2e_sync_pending_audit AS audit
		JOIN files ON CAST(files.id AS TEXT)=audit.row_pk
		WHERE audit.seq > ?1 AND audit.table_name='files'
		ORDER BY audit.seq`,
		observer.baseline,
	)
	if err != nil {
		return fmt.Errorf("query M5 no-pending sync audit: %w", err)
	}
	audited := make([]string, 0)
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			rows.Close()
			return fmt.Errorf("scan M5 no-pending sync audit: %w", err)
		}
		if _, expected := observer.expected[path]; expected {
			audited = append(audited, path)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate M5 no-pending sync audit: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close M5 no-pending sync audit: %w", err)
	}
	if len(audited) != 0 {
		return fmt.Errorf(
			"unexpected pending sync audit for untouched paths: %s",
			summarizeM5Paths(audited),
		)
	}

	rows, err = observer.database.QueryContext(observer.ctx, `
		SELECT files.path
		FROM sync_queue
		JOIN files ON CAST(files.id AS TEXT)=sync_queue.row_pk
		WHERE sync_queue.table_name='files' AND sync_queue.synced=0
		ORDER BY files.path`)
	if err != nil {
		return fmt.Errorf("query M5 no-pending sync queue: %w", err)
	}
	pending := make([]string, 0)
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			rows.Close()
			return fmt.Errorf("scan M5 no-pending sync queue: %w", err)
		}
		if _, expected := observer.expected[path]; expected {
			pending = append(pending, path)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate M5 no-pending sync queue: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close M5 no-pending sync queue: %w", err)
	}
	if len(pending) != 0 {
		return fmt.Errorf(
			"unexpected pending sync_queue rows for untouched paths: %s",
			summarizeM5Paths(pending),
		)
	}
	return nil
}

func (fixture *m5Fixture) observePendingSync(
	t *testing.T,
	expectedPaths []string,
) *m5PendingObserver {
	t.Helper()
	observer, err := newM5PendingObserver(
		fixture.ctx,
		fixture.sqlite,
		expectedPaths,
	)
	if err != nil {
		t.Fatal(err)
	}
	return observer
}

func (observer *m5PendingObserver) finish(t *testing.T) {
	t.Helper()
	if err := observer.verify(); err != nil {
		t.Fatal(err)
	}
}

func (observer *m5PendingObserver) verify() error {
	rows, err := observer.database.QueryContext(observer.ctx, `
		SELECT files.path
		FROM m5_e2e_sync_pending_audit AS audit
		JOIN files ON CAST(files.id AS TEXT)=audit.row_pk
		WHERE audit.seq > ?1 AND audit.table_name='files'
		ORDER BY audit.seq`,
		observer.baseline,
	)
	if err != nil {
		return fmt.Errorf("query M5 pending sync audit: %w", err)
	}
	defer rows.Close()
	seen := make(map[string]int, len(observer.expected))
	unexpected := make(map[string]struct{})
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return fmt.Errorf("scan M5 pending sync audit: %w", err)
		}
		if _, ok := observer.expected[path]; !ok {
			unexpected[path] = struct{}{}
			continue
		}
		seen[path]++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate M5 pending sync audit: %w", err)
	}
	missing := make([]string, 0)
	duplicate := make([]string, 0)
	for path := range observer.expected {
		switch seen[path] {
		case 0:
			missing = append(missing, path)
		case 1:
		default:
			duplicate = append(duplicate, path)
		}
	}
	extra := make([]string, 0, len(unexpected))
	for path := range unexpected {
		extra = append(extra, path)
	}
	sort.Strings(missing)
	sort.Strings(duplicate)
	sort.Strings(extra)
	if len(missing) != 0 || len(duplicate) != 0 || len(extra) != 0 {
		return fmt.Errorf(
			"pending sync_queue audit mismatch: missing=%s duplicate=%s unexpected=%s states=%s",
			summarizeM5Paths(missing),
			summarizeM5Paths(duplicate),
			summarizeM5Paths(extra),
			observer.pendingSyncDiagnostics(missing),
		)
	}
	return nil
}

func (observer *m5PendingObserver) pendingSyncDiagnostics(paths []string) string {
	const limit = 8
	if len(paths) > limit {
		paths = paths[:limit]
	}
	states := make([]string, 0, len(paths))
	for _, path := range paths {
		var (
			rowPK      string
			status     string
			synced     sql.NullInt64
			generation sql.NullInt64
			auditSeq   sql.NullInt64
		)
		err := observer.database.QueryRowContext(observer.ctx, `
			SELECT CAST(files.id AS TEXT), files.status,
			       sync_queue.synced, sync_queue.generation,
			       (
			           SELECT max(audit.seq)
			           FROM m5_e2e_sync_pending_audit AS audit
			           WHERE audit.table_name='files'
			             AND audit.row_pk=CAST(files.id AS TEXT)
			       )
			FROM files
			LEFT JOIN sync_queue
			  ON sync_queue.table_name='files'
			 AND sync_queue.row_pk=CAST(files.id AS TEXT)
			WHERE files.path=?1`,
			path,
		).Scan(&rowPK, &status, &synced, &generation, &auditSeq)
		if err != nil {
			states = append(states, fmt.Sprintf("%q:error=%v", path, err))
			continue
		}
		states = append(states, fmt.Sprintf(
			"%q:{row_pk=%s status=%s synced=%s generation=%s audit_seq=%s baseline=%d}",
			path,
			rowPK,
			status,
			m5NullIntText(synced),
			m5NullIntText(generation),
			m5NullIntText(auditSeq),
			observer.baseline,
		))
	}
	return "[" + strings.Join(states, " ") + "]"
}

func m5NullIntText(value sql.NullInt64) string {
	if !value.Valid {
		return "NULL"
	}
	return fmt.Sprintf("%d", value.Int64)
}

func (fixture *m5Fixture) writeTC09Diagnostics(
	t *testing.T,
	observer *m5PendingObserver,
	seed m5SeededCase,
	retryTaskID string,
	retryStatus DeleteTaskStatus,
) {
	t.Helper()
	expectedPaths := make(map[string]struct{}, len(seed.paths))
	for _, path := range seed.paths {
		expectedPaths[path] = struct{}{}
	}
	diagnostics := m5TC09Diagnostics{
		SchemaVersion: 1,
		RunID:         fixture.env.runID,
		RetryTaskID:   retryTaskID,
		AuditBaseline: observer.baseline,
		LocalStates:   make([]m5TC09LocalState, 0, len(seed.paths)),
		RetryStatus:   retryStatus,
	}
	diagnostics.RetryStatus.Problems = append(
		[]DeleteProblemItem(nil),
		retryStatus.Problems...,
	)
	for index := range diagnostics.RetryStatus.Problems {
		problem := &diagnostics.RetryStatus.Problems[index]
		if _, ok := expectedPaths[problem.Path]; problem.Path != "" && !ok {
			t.Fatalf("TC-09 retry status contains foreign problem path %q", problem.Path)
		}
		problem.ErrorMessage = redactM5Text(problem.ErrorMessage)
		problem.StateSyncErr = redactM5Text(problem.StateSyncErr)
	}

	for _, path := range seed.paths {
		state := m5TC09LocalState{Path: path}
		var localID int64
		if err := fixture.sqlite.QueryRowContext(fixture.ctx, `
			SELECT id,status
			FROM files
			WHERE machine_id=?1 AND path=?2`,
			fixture.env.machineID,
			path,
		).Scan(&localID, &state.Status); err != nil {
			state.QueryError = redactM5Text(err.Error())
			diagnostics.LocalStates = append(diagnostics.LocalStates, state)
			continue
		}
		state.LocalID = &localID
		var synced, generation int64
		err := fixture.sqlite.QueryRowContext(fixture.ctx, `
			SELECT row_pk,synced,generation
			FROM sync_queue
			WHERE table_name='files' AND row_pk=?1`,
			fmt.Sprintf("%d", localID),
		).Scan(&state.QueueRowPK, &synced, &generation)
		switch {
		case err == nil:
			state.Synced = &synced
			state.Generation = &generation
		case errors.Is(err, sql.ErrNoRows):
		default:
			state.QueryError = redactM5Text(err.Error())
		}
		diagnostics.LocalStates = append(diagnostics.LocalStates, state)
	}

	rows, err := fixture.sqlite.QueryContext(fixture.ctx, `
		SELECT seq,row_pk,generation
		FROM m5_e2e_sync_pending_audit
		WHERE seq>?1 AND table_name='files'
		ORDER BY seq
		LIMIT 16`,
		observer.baseline,
	)
	if err != nil {
		t.Fatalf("query TC-09 raw pending audit rows: %v", err)
	}
	for rows.Next() {
		var row m5TC09RawAuditRow
		if err := rows.Scan(&row.Sequence, &row.RowPK, &row.Generation); err != nil {
			rows.Close()
			t.Fatalf("scan TC-09 raw pending audit row: %v", err)
		}
		diagnostics.RawAuditRows = append(diagnostics.RawAuditRows, row)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close TC-09 raw pending audit rows: %v", err)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate TC-09 raw pending audit rows: %v", err)
	}

	deleteLogPath := filepath.Join(fixture.env.agentData, "delete.log")
	fixture.track("log", deleteLogPath)
	logBytes, err := os.ReadFile(deleteLogPath)
	if err != nil {
		t.Fatalf("read TC-09 bounded delete audit: %v", err)
	}
	records, err := parseM5DeleteAuditJSONL(logBytes)
	if err != nil {
		t.Fatalf("parse TC-09 bounded delete audit: %v", err)
	}
	diagnostics.DeleteAudit = filterM5TC09AuditRecords(records, retryTaskID, 16)
	for index := range diagnostics.DeleteAudit {
		record := &diagnostics.DeleteAudit[index]
		if _, ok := expectedPaths[record.Path]; record.Path != "" && !ok {
			t.Fatalf("TC-09 retry audit contains foreign path %q", record.Path)
		}
		record.Err = redactM5Text(record.Err)
		if record.RecycledTo != "" {
			t.Fatalf("TC-09 hard-delete audit unexpectedly recycled to %q", record.RecycledTo)
		}
	}

	raw, err := json.MarshalIndent(diagnostics, "", "  ")
	if err != nil {
		t.Fatalf("marshal TC-09 diagnostics: %v", err)
	}
	if bytes.Contains(raw, []byte("postgres://")) {
		t.Fatal("credential-shaped PostgreSQL URL entered TC-09 diagnostics")
	}
	path := filepath.Join(
		fixture.env.evidenceDir,
		"m5-"+fixture.env.runID+"-tc09-diagnostics.json",
	)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write TC-09 diagnostics: %v", err)
	}
}

func redactM5Text(text string) string {
	if text == "" {
		return ""
	}
	return redactM5Error(errors.New(text)).Error()
}

func summarizeM5Paths(paths []string) string {
	const limit = 8
	if len(paths) <= limit {
		return fmt.Sprintf("%q", paths)
	}
	return fmt.Sprintf("%q (+%d more)", paths[:limit], len(paths)-limit)
}

func compareM5DBFileInvariants(
	baseline []m5DBFileInvariant,
	observed []m5DBFileInvariant,
) error {
	if len(baseline) == 0 {
		return errors.New("compare M5 database invariants: empty baseline")
	}
	expected := make(map[string]m5DBFileInvariant, len(baseline))
	for _, invariant := range baseline {
		if invariant.Path == "" {
			return errors.New("compare M5 database invariants: empty baseline path")
		}
		key := strings.ToLower(filepath.Clean(invariant.Path))
		if _, exists := expected[key]; exists {
			return fmt.Errorf(
				"compare M5 database invariants: duplicate baseline path %q",
				invariant.Path,
			)
		}
		expected[key] = invariant
	}
	if len(observed) != len(expected) {
		return fmt.Errorf(
			"compare M5 database invariants: observed %d rows, want %d",
			len(observed),
			len(expected),
		)
	}
	seen := make(map[string]struct{}, len(observed))
	for _, invariant := range observed {
		key := strings.ToLower(filepath.Clean(invariant.Path))
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf(
				"compare M5 database invariants: duplicate observed path %q",
				invariant.Path,
			)
		}
		seen[key] = struct{}{}
		want, exists := expected[key]
		if !exists {
			return fmt.Errorf(
				"compare M5 database invariants: unexpected path %q",
				invariant.Path,
			)
		}
		if invariant != want {
			return fmt.Errorf(
				"compare M5 database invariants: row changed %q",
				invariant.Path,
			)
		}
	}
	return nil
}

func loadM5SQLiteFileInvariants(
	ctx context.Context,
	database *sql.DB,
	machineID string,
	paths []string,
) ([]m5DBFileInvariant, error) {
	if database == nil || machineID == "" || len(paths) == 0 {
		return nil, errors.New("load M5 SQLite invariants: invalid input")
	}
	expected := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path == "" {
			return nil, errors.New("load M5 SQLite invariants: empty path")
		}
		if _, exists := expected[path]; exists {
			return nil, fmt.Errorf("load M5 SQLite invariants: duplicate path %q", path)
		}
		expected[path] = struct{}{}
	}
	rows, err := database.QueryContext(ctx, `
		SELECT path,size,mtime,sha512,status,missing_mask
		FROM files
		WHERE machine_id=?1
		ORDER BY path`,
		machineID,
	)
	if err != nil {
		return nil, fmt.Errorf("load M5 SQLite invariants: %w", err)
	}
	defer rows.Close()
	invariants := make([]m5DBFileInvariant, 0, len(paths))
	for rows.Next() {
		var invariant m5DBFileInvariant
		if err := rows.Scan(
			&invariant.Path,
			&invariant.Size,
			&invariant.MTime,
			&invariant.SHA512,
			&invariant.Status,
			&invariant.MissingMask,
		); err != nil {
			return nil, fmt.Errorf("scan M5 SQLite invariant: %w", err)
		}
		if _, ok := expected[invariant.Path]; ok {
			invariants = append(invariants, invariant)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate M5 SQLite invariants: %w", err)
	}
	if len(invariants) != len(expected) {
		return nil, fmt.Errorf(
			"load M5 SQLite invariants: found %d rows, want %d",
			len(invariants),
			len(expected),
		)
	}
	return invariants, nil
}

func loadM5PostgresFileInvariants(
	ctx context.Context,
	database *pgxpool.Pool,
	machineID string,
	paths []string,
) ([]m5DBFileInvariant, error) {
	if database == nil || machineID == "" || len(paths) == 0 {
		return nil, errors.New("load M5 PostgreSQL invariants: invalid input")
	}
	rows, err := database.Query(ctx, `
		SELECT path,size,mtime,sha512,status,missing_mask
		FROM files
		WHERE machine_id=$1 AND path=ANY($2::text[])
		ORDER BY path`,
		machineID,
		paths,
	)
	if err != nil {
		return nil, fmt.Errorf("load M5 PostgreSQL invariants: %w", err)
	}
	defer rows.Close()
	invariants := make([]m5DBFileInvariant, 0, len(paths))
	for rows.Next() {
		var invariant m5DBFileInvariant
		if err := rows.Scan(
			&invariant.Path,
			&invariant.Size,
			&invariant.MTime,
			&invariant.SHA512,
			&invariant.Status,
			&invariant.MissingMask,
		); err != nil {
			return nil, fmt.Errorf("scan M5 PostgreSQL invariant: %w", err)
		}
		invariants = append(invariants, invariant)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate M5 PostgreSQL invariants: %w", err)
	}
	if len(invariants) != len(paths) {
		return nil, fmt.Errorf(
			"load M5 PostgreSQL invariants: found %d rows, want %d",
			len(invariants),
			len(paths),
		)
	}
	return invariants, nil
}

func (fixture *m5Fixture) assertPersistence(
	t *testing.T,
	seed m5SeededCase,
	successNames []string,
) {
	t.Helper()
	successPaths := make([]string, 0, len(successNames))
	successIDs := make(map[int64]bool, len(successNames))
	for _, name := range successNames {
		path, ok := seed.pathByName[name]
		if !ok {
			t.Fatalf("unknown successful seed name %q", name)
		}
		successPaths = append(successPaths, path)
		successIDs[seed.idByName[name]] = true
		var status string
		if err := fixture.sqlite.QueryRowContext(
			fixture.ctx,
			`SELECT status FROM files WHERE machine_id=?1 AND path=?2`,
			fixture.env.machineID,
			path,
		).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != proto.StatusDeleted {
			t.Fatalf("SQLite status for %q=%q, want deleted", path, status)
		}
	}
	waitCtx, cancel := context.WithTimeout(fixture.ctx, 30*time.Second)
	defer cancel()
	waitM5(t, waitCtx, "forced Agent sync to PostgreSQL", func() (bool, error) {
		var deleted int
		err := fixture.pg.QueryRow(waitCtx, `
			SELECT count(*) FROM files
			WHERE machine_id=$1 AND path=ANY($2::text[]) AND status='deleted'`,
			fixture.env.machineID,
			successPaths,
		).Scan(&deleted)
		return deleted == len(successPaths), redactM5Error(err)
	})
	fixture.track(
		"url",
		fmt.Sprintf("%s/api/groups/%d", fixture.env.baseURL, seed.groupID),
	)
	request, err := http.NewRequestWithContext(
		fixture.ctx,
		http.MethodGet,
		fmt.Sprintf("%s/api/groups/%d", fixture.env.baseURL, seed.groupID),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := fixture.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		t.Fatalf("group detail after sync status=%d body=%s", response.StatusCode, body)
	}
	var detail GroupDetail
	if err := json.NewDecoder(response.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	for _, member := range detail.Members {
		if successIDs[member.FileID] {
			t.Fatalf("deleted file ID %d remains in groups API", member.FileID)
		}
	}
	fixture.assertDeleteLog(t, successPaths)
}

func (fixture *m5Fixture) assertDeleteLog(
	t *testing.T,
	paths []string,
) {
	t.Helper()
	logPath := filepath.Join(fixture.env.agentData, "delete.log")
	fixture.track("log", logPath)
	waitCtx, cancel := context.WithTimeout(fixture.ctx, 10*time.Second)
	defer cancel()
	waitM5(t, waitCtx, "delete.log physical success evidence", func() (bool, error) {
		data, err := os.ReadFile(logPath)
		if err != nil {
			return false, err
		}
		records, err := parseM5DeleteAuditJSONL(data)
		if err != nil {
			return false, err
		}
		required := make(map[string]struct{}, len(paths))
		for _, path := range paths {
			required[path] = struct{}{}
		}
		knownTasks := make(map[string]struct{}, len(fixture.taskIDs))
		for _, taskID := range fixture.taskIDs {
			knownTasks[taskID] = struct{}{}
		}
		for _, record := range records {
			if record.Message != "delete_physical_result" ||
				record.MachineID != fixture.env.machineID ||
				record.Seq == nil ||
				!record.OK ||
				record.Uncertain {
				continue
			}
			if _, ok := knownTasks[record.TaskID]; !ok {
				continue
			}
			delete(required, record.Path)
		}
		return len(required) == 0, nil
	})
}

func parseM5DeleteAuditJSONL(data []byte) ([]m5DeleteAuditRecord, error) {
	lines := bytes.Split(data, []byte{'\n'})
	records := make([]m5DeleteAuditRecord, 0, len(lines))
	for index, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var record m5DeleteAuditRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, fmt.Errorf(
				"decode delete audit line %d: %w",
				index+1,
				err,
			)
		}
		records = append(records, record)
	}
	return records, nil
}

func filterM5TC09AuditRecords(
	records []m5DeleteAuditRecord,
	taskID string,
	limit int,
) []m5DeleteAuditRecord {
	if taskID == "" || limit <= 0 {
		return nil
	}
	filtered := make([]m5DeleteAuditRecord, 0, min(limit, len(records)))
	for _, record := range records {
		if record.TaskID != taskID ||
			record.Message != "delete_physical_result" &&
				record.Message != "delete_state_sync_error" {
			continue
		}
		filtered = append(filtered, record)
		if len(filtered) == limit {
			break
		}
	}
	return filtered
}

func assertM5StatusCounts(
	t *testing.T,
	status DeleteTaskStatus,
	total, ok, failed, uncertain int64,
) {
	t.Helper()
	if status.Total != total || status.OK != ok || status.Failed != failed ||
		status.Uncertain != uncertain || !status.Complete || status.Pending != 0 {
		t.Fatalf(
			"delete status totals=%d/%d/%d/%d complete=%t pending=%d, want %d/%d/%d/%d",
			status.Total,
			status.OK,
			status.Failed,
			status.Uncertain,
			status.Complete,
			status.Pending,
			total,
			ok,
			failed,
			uncertain,
		)
	}
}

func (fixture *m5Fixture) tc01HardReadonly(t *testing.T) {
	data := []byte("tc01-readonly-generated-bytes")
	path := fixture.writeGenerated(t, filepath.Join("tc01", "readonly.bin"), data)
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	attributes, err := windows.GetFileAttributes(pathUTF16)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetFileAttributes(
		pathUTF16,
		attributes|windows.FILE_ATTRIBUTE_READONLY,
	); err != nil {
		t.Fatal(err)
	}
	seed := fixture.seedCase(t, "tc01", []m5SeedSpec{{
		name: "readonly", path: path, data: data,
	}})
	observer := fixture.observePendingSync(t, seed.paths)
	token := fixture.prepareDelete(t, seed.memberIDs)
	taskID := fixture.executeDelete(t, token, proto.ModeHard)
	status := fixture.waitDeleteComplete(t, taskID)
	observer.finish(t)
	assertM5StatusCounts(t, status, 1, 1, 0, 0)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hard-deleted readonly file still exists: %v", err)
	}
	fixture.assertPersistence(t, seed, []string{"readonly"})
	logBytes, err := os.ReadFile(filepath.Join(fixture.env.agentData, "delete.log"))
	if err != nil {
		t.Fatal(err)
	}
	auditRecords, err := parseM5DeleteAuditJSONL(logBytes)
	if err != nil {
		t.Fatal(err)
	}
	foundReadonlyAudit := false
	for _, record := range auditRecords {
		if record.Message == "delete_physical_result" &&
			record.TaskID == taskID &&
			record.MachineID == fixture.env.machineID &&
			record.Seq != nil &&
			record.Path == path &&
			record.Mode == proto.ModeHard &&
			record.OK &&
			!record.Uncertain {
			if !record.ReadonlyCleared {
				t.Fatal("delete.log physical result omitted readonly_cleared=true")
			}
			foundReadonlyAudit = true
			break
		}
	}
	if !foundReadonlyAudit {
		t.Fatal("delete.log omitted the TC-01 physical result")
	}
	fixture.assert["hard_mode"] = status.Mode == proto.ModeHard
	fixture.assert["readonly_cleared"] = true
	fixture.assert["sqlite_pg_groups_log"] = true
}

func (fixture *m5Fixture) tc02MissingMixed(t *testing.T) {
	specs := make([]m5SeedSpec, 0, 3)
	for _, name := range []string{"success-a", "missing", "success-b"} {
		data := []byte("tc02-" + name)
		path := fixture.writeGenerated(t, filepath.Join("tc02", name+".bin"), data)
		specs = append(specs, m5SeedSpec{name: name, path: path, data: data})
	}
	seed := fixture.seedCase(t, "tc02", specs)
	if err := os.Remove(seed.pathByName["missing"]); err != nil {
		t.Fatal(err)
	}
	observer := fixture.observePendingSync(t, []string{
		seed.pathByName["success-a"],
		seed.pathByName["success-b"],
	})
	token := fixture.prepareDelete(t, seed.memberIDs)
	taskID := fixture.executeDelete(t, token, proto.ModeHard)
	status := fixture.waitDeleteComplete(t, taskID)
	observer.finish(t)
	assertM5StatusCounts(t, status, 3, 2, 1, 0)
	if status.ErrorCodes[proto.DeleteErrNotFound] != 1 {
		t.Fatalf("TC-02 error codes=%#v", status.ErrorCodes)
	}
	for _, name := range []string{"success-a", "success-b"} {
		if _, err := os.Stat(seed.pathByName[name]); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("TC-02 successful path %q remains: %v", name, err)
		}
	}
	var missingLocal, missingPG string
	if err := fixture.sqlite.QueryRowContext(
		fixture.ctx,
		`SELECT status FROM files WHERE machine_id=?1 AND path=?2`,
		fixture.env.machineID,
		seed.pathByName["missing"],
	).Scan(&missingLocal); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pg.QueryRow(fixture.ctx,
		`SELECT status FROM files WHERE id=$1`,
		seed.idByName["missing"],
	).Scan(&missingPG); err != nil {
		t.Fatal(redactM5Error(err))
	}
	if missingLocal != proto.StatusDone || missingPG != proto.StatusDone {
		t.Fatalf("missing path statuses local/PG=%q/%q", missingLocal, missingPG)
	}
	fixture.assertPersistence(t, seed, []string{"success-a", "success-b"})
	fixture.assert["mixed_successes"] = 2
	fixture.assert["missing_error"] = proto.DeleteErrNotFound
}

func (fixture *m5Fixture) tc03InUseAndAccessDenied(t *testing.T) {
	lockedData := []byte("tc03-exclusive-handle")
	deniedData := []byte("tc03-acl-denied")
	lockedPath := fixture.writeGenerated(
		t,
		filepath.Join("tc03", "locked.bin"),
		lockedData,
	)
	deniedDir := filepath.Join(fixture.env.generated, "tc03", "acl-isolated")
	if err := os.MkdirAll(deniedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture.track("generated", deniedDir)
	deniedPath := filepath.Join(deniedDir, "denied.bin")
	if err := os.WriteFile(deniedPath, deniedData, 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.track("generated", deniedPath)
	seed := fixture.seedCase(t, "tc03", []m5SeedSpec{
		{name: "locked", path: lockedPath, data: lockedData},
		{name: "denied", path: deniedPath, data: deniedData},
	})
	lockedBefore := m5FileDigest(t, lockedPath)
	deniedBefore := m5FileDigest(t, deniedPath)

	dirACL := saveM5ACL(t, deniedDir)
	fileACL := saveM5ACL(t, deniedPath)
	if !strings.Contains(dirACL, "(A;") ||
		!strings.Contains(fileACL, "(A;") {
		t.Fatal("TC-03 saved ACL omitted inherited allow entries")
	}
	aclRestored := false
	defer func() {
		if !aclRestored {
			if err := restoreM5ACL(deniedPath, fileACL); err != nil {
				t.Errorf("restore saved file ACL %q: %v", deniedPath, err)
			}
		}
	}()
	defer func() {
		if !aclRestored {
			if err := restoreM5ACL(deniedDir, dirACL); err != nil {
				t.Errorf("restore saved directory ACL %q: %v", deniedDir, err)
			}
		}
	}()
	handle := openM5ExclusiveHandle(t, lockedPath)
	handleOpen := true
	defer func() {
		if handleOpen {
			if err := windows.CloseHandle(handle); err != nil {
				t.Errorf("close TC-03 exclusive handle: %v", err)
			}
		}
	}()
	denyM5Delete(t, deniedDir, deniedPath)

	token := fixture.prepareDelete(t, seed.memberIDs)
	taskID := fixture.executeDelete(t, token, proto.ModeHard)
	status := fixture.waitDeleteComplete(t, taskID)
	closeErr := windows.CloseHandle(handle)
	handleOpen = false
	if closeErr != nil {
		t.Fatalf("close TC-03 exclusive handle after task: %v", closeErr)
	}
	assertM5StatusCounts(t, status, 2, 0, 2, 0)
	if status.ErrorCodes[proto.DeleteErrInUse] != 1 ||
		status.ErrorCodes[proto.DeleteErrAccessDenied] != 1 {
		t.Fatalf("TC-03 error codes=%#v", status.ErrorCodes)
	}
	if m5FileDigest(t, lockedPath) != lockedBefore ||
		m5FileDigest(t, deniedPath) != deniedBefore {
		t.Fatal("TC-03 failure path bytes changed")
	}
	if err := restoreM5ACL(deniedDir, dirACL); err != nil {
		t.Fatalf("restore saved directory ACL %q: %v", deniedDir, err)
	}
	if err := restoreM5ACL(deniedPath, fileACL); err != nil {
		t.Fatalf("restore saved file ACL %q: %v", deniedPath, err)
	}
	restoredFile, err := os.Open(deniedPath)
	if err != nil {
		t.Fatalf("open restored ACL file %q: %v", deniedPath, err)
	}
	if err := restoredFile.Close(); err != nil {
		t.Fatalf("close restored ACL file %q: %v", deniedPath, err)
	}
	aclRestored = true
	var pending int
	if err := fixture.sqlite.QueryRowContext(
		fixture.ctx,
		`SELECT count(*) FROM sync_queue WHERE synced=0`,
	).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("TC-03 enqueued failed physical paths: %d", pending)
	}
	fixture.assert["exclusive_handle"] = proto.DeleteErrInUse
	fixture.assert["acl_denied"] = proto.DeleteErrAccessDenied
	fixture.assert["acl_restored_in_finally"] = true
}

func (fixture *m5Fixture) tc04SoftRecycleAndCollision(t *testing.T) {
	directData := []byte("tc04-direct-collision-source")
	directPath := fixture.writeGenerated(
		t,
		filepath.Join("tc04", "direct-collision.bin"),
		directData,
	)
	directTaskID := uuid.NewString()
	baseDestination := filepath.Join(
		fixture.env.drive+`\`,
		"$DedupRecycle",
		directTaskID,
		"generated",
		"tc04",
		"direct-collision.bin",
	)
	fixture.track("recycle", baseDestination)
	if err := os.MkdirAll(filepath.Dir(baseDestination), 0o700); err != nil {
		t.Fatal(err)
	}
	collisionBytes := []byte("pre-existing-collision")
	if err := os.WriteFile(baseDestination, collisionBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	report, _ := fixture.directDelete(t, proto.DeleteTask{
		TaskID:    directTaskID,
		Confirmed: true,
		Entries:   []string{directPath},
	})
	if report.Stats != (proto.DeleteStats{Total: 1, OK: 1}) ||
		len(report.Entries) != 1 {
		t.Fatalf("TC-04 direct collision report=%#v", report)
	}
	wantCollision := strings.TrimSuffix(baseDestination, ".bin") + "_1.bin"
	fixture.track("recycle", wantCollision)
	if !strings.EqualFold(report.Entries[0].RecycledTo, wantCollision) {
		t.Fatalf(
			"TC-04 recycled_to=%q, want %q",
			report.Entries[0].RecycledTo,
			wantCollision,
		)
	}
	if m5FileDigest(t, wantCollision) != m5Digest(directData) ||
		m5FileDigest(t, baseDestination) != m5Digest(collisionBytes) {
		t.Fatal("TC-04 collision mapping changed source or existing destination bytes")
	}

	guiData := []byte("tc04-gui-default-soft")
	guiPath := fixture.writeGenerated(
		t,
		filepath.Join("tc04", "gui-default-soft.bin"),
		guiData,
	)
	seed := fixture.seedCase(t, "tc04", []m5SeedSpec{{
		name: "default-soft", path: guiPath, data: guiData,
	}})
	observer := fixture.observePendingSync(t, seed.paths)
	token := fixture.prepareDelete(t, seed.memberIDs)
	taskID := fixture.executeDelete(t, token, "")
	status := fixture.waitDeleteComplete(t, taskID)
	observer.finish(t)
	assertM5StatusCounts(t, status, 1, 1, 0, 0)
	if status.Mode != proto.ModeSoft {
		t.Fatalf("TC-04 default mode=%q, want soft", status.Mode)
	}
	guiDestination := filepath.Join(
		fixture.env.drive+`\`,
		"$DedupRecycle",
		taskID,
		"generated",
		"tc04",
		"gui-default-soft.bin",
	)
	fixture.track("recycle", guiDestination)
	if m5FileDigest(t, guiDestination) != m5Digest(guiData) {
		t.Fatal("TC-04 GUI soft-delete hash mismatch")
	}
	if _, err := os.Stat(guiPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("TC-04 soft source remains: %v", err)
	}
	fixture.assertPersistence(t, seed, []string{"default-soft"})
	fixture.assert["default_mode"] = proto.ModeSoft
	fixture.assert["hash_equal"] = true
	fixture.assert["collision_suffix"] = "_1"
}

func (fixture *m5Fixture) tc05PathDenied(t *testing.T) {
	paths := map[string]string{
		"system": filepath.Join(fixture.env.generated, "system", "system.bin"),
		"denied": filepath.Join(fixture.env.generated, "denied", "denied.bin"),
		"outside": filepath.Join(
			fixture.env.drive+`\`,
			"outside",
			"outside.bin",
		),
	}
	specs := make([]m5SeedSpec, 0, 3)
	before := make(map[string]string, 3)
	for _, name := range []string{"system", "outside", "denied"} {
		data := []byte("tc05-" + name)
		path := paths[name]
		fixture.track("generated", path)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		before[name] = m5Digest(data)
		specs = append(specs, m5SeedSpec{name: name, path: path, data: data})
	}
	seed := fixture.seedCase(t, "tc05", specs)
	token := fixture.prepareDelete(t, seed.memberIDs)
	taskID := fixture.executeDelete(t, token, proto.ModeHard)
	status := fixture.waitDeleteComplete(t, taskID)
	assertM5StatusCounts(t, status, 3, 0, 3, 0)
	if status.ErrorCodes[proto.DeleteErrPathDenied] != 3 {
		t.Fatalf("TC-05 error codes=%#v", status.ErrorCodes)
	}
	for name, path := range paths {
		if m5FileDigest(t, path) != before[name] {
			t.Fatalf("TC-05 %s bytes changed", name)
		}
	}
	fixture.assert["generated_system_outside_denied_unchanged"] = true
	fixture.assert["error_code"] = proto.DeleteErrPathDenied
}

func (fixture *m5Fixture) tc06UnconfirmedDirectFrame(t *testing.T) {
	data := []byte("tc06-unconfirmed")
	path := fixture.writeGenerated(
		t,
		filepath.Join("tc06", "unconfirmed.bin"),
		data,
	)
	before := m5FileDigest(t, path)
	taskID := uuid.NewString()
	report, _ := fixture.directDelete(t, proto.DeleteTask{
		TaskID:    taskID,
		Mode:      proto.ModeHard,
		Confirmed: false,
		Entries:   []string{path},
	})
	if report.TaskID != taskID ||
		report.Stats != (proto.DeleteStats{Total: 1, Failed: 1}) ||
		len(report.Entries) != 1 ||
		report.Entries[0].ErrCode != proto.DeleteErrNotConfirmed {
		t.Fatalf("TC-06 report=%#v", report)
	}
	if m5FileDigest(t, path) != before {
		t.Fatal("TC-06 unconfirmed frame touched generated bytes")
	}
	fixture.assert["direct_pipe"] = true
	fixture.assert["confirmed_false"] = proto.DeleteErrNotConfirmed
}

func (fixture *m5Fixture) tc07JunctionAndNeighbor(t *testing.T) {
	targetDir := filepath.Join(fixture.env.generated, "tc07-target")
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(targetDir, "victim.bin")
	targetData := []byte("tc07-junction-target")
	if err := os.WriteFile(targetPath, targetData, 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.track("generated", targetDir)
	fixture.track("generated", targetPath)
	junction := filepath.Join(fixture.env.generated, "tc07-junction")
	createM5Junction(t, junction, targetDir)
	fixture.track("junction", junction)
	junctionPath := filepath.Join(junction, "victim.bin")
	fixture.track("generated", junctionPath)
	neighborData := []byte("tc07-normal-neighbor")
	neighborPath := fixture.writeGenerated(
		t,
		filepath.Join("tc07", "neighbor.bin"),
		neighborData,
	)
	seed := fixture.seedCase(t, "tc07", []m5SeedSpec{
		{name: "junction", path: junctionPath, data: targetData},
		{name: "neighbor", path: neighborPath, data: neighborData},
	})
	observer := fixture.observePendingSync(t, []string{neighborPath})
	token := fixture.prepareDelete(t, seed.memberIDs)
	taskID := fixture.executeDelete(t, token, proto.ModeHard)
	status := fixture.waitDeleteComplete(t, taskID)
	observer.finish(t)
	assertM5StatusCounts(t, status, 2, 1, 1, 0)
	if status.ErrorCodes[proto.DeleteErrReparse] != 1 {
		t.Fatalf("TC-07 error codes=%#v", status.ErrorCodes)
	}
	if m5FileDigest(t, targetPath) != m5Digest(targetData) {
		t.Fatal("TC-07 junction target bytes changed")
	}
	if _, err := os.Stat(neighborPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("TC-07 normal neighbor remains: %v", err)
	}
	fixture.assertPersistence(t, seed, []string{"neighbor"})
	fixture.assert["junction_rejected"] = proto.DeleteErrReparse
	fixture.assert["normal_neighbor_succeeded"] = true
}

func (fixture *m5Fixture) tc08FiveThousandChunks(t *testing.T) {
	seed := fixture.seedLargeCase(t, "tc08", 5000)
	observer := fixture.observePendingSync(t, seed.paths)
	token := fixture.prepareDelete(t, seed.memberIDs)
	taskID := fixture.executeDelete(t, token, proto.ModeHard)
	status := fixture.waitDeleteComplete(t, taskID)
	observer.finish(t)
	assertM5StatusCounts(t, status, 5000, 5000, 0, 0)
	machine, ok := status.ByMachine[fixture.env.machineID]
	if !ok || len(machine.Sequences) != 3 {
		t.Fatalf("TC-08 machine sequence state=%#v", machine)
	}
	want := map[uint32]int64{0: 2000, 1: 2000, 2: 1000}
	for sequence, total := range want {
		got, ok := machine.Sequences[sequence]
		if !ok || !got.Received || got.Total != total || got.OK != total ||
			got.Failed != 0 {
			t.Fatalf("TC-08 sequence %d=%#v, want total/ok=%d", sequence, got, total)
		}
	}
	names := make([]string, 5000)
	for index := range names {
		names[index] = fmt.Sprintf("%05d", index)
	}
	fixture.assertPersistence(t, seed, names)
	fixture.assert["generated_files"] = 5000
	fixture.assert["report_chunks"] = []int{2000, 2000, 1000}
}

func (fixture *m5Fixture) tc09OfflineAndRestart(t *testing.T) {
	fixture.shutdownHelper(t, fixture.env.helperPID)
	if pids := findM5ProcessesByImage(t, fixture.env.helperExe); len(pids) != 0 {
		t.Fatalf("TC-09 Helper processes after manual shutdown=%v", pids)
	}
	fixture.waitAgentOnline(t)

	specs := make([]m5SeedSpec, 0, 2)
	for _, name := range []string{"offline-a", "offline-b"} {
		data := []byte("tc09-" + name)
		path := fixture.writeGenerated(t, filepath.Join("tc09", name+".bin"), data)
		specs = append(specs, m5SeedSpec{name: name, path: path, data: data})
	}
	seed := fixture.seedCase(t, "tc09", specs)
	offlineToken := fixture.prepareDelete(t, seed.memberIDs)
	offlineTask := fixture.executeDelete(t, offlineToken, proto.ModeHard)
	offlineStatus := fixture.waitDeleteComplete(t, offlineTask)
	assertM5StatusCounts(t, offlineStatus, 2, 0, 2, 0)
	if offlineStatus.ErrorCodes[proto.DeleteErrHelperLost] != 2 {
		t.Fatalf("TC-09 offline error codes=%#v", offlineStatus.ErrorCodes)
	}
	fixture.waitAgentOffline(t)
	for _, spec := range specs {
		if m5FileDigest(t, spec.path) != m5Digest(spec.data) {
			t.Fatalf("TC-09 offline Helper touched %q", spec.path)
		}
	}
	fixture.waitPipeUnavailable(t)
	if pids := findM5ProcessesByImage(t, fixture.env.helperExe); len(pids) != 0 {
		t.Fatalf("TC-09 Agent auto-launched Helper/UAC candidate PIDs=%v", pids)
	}

	restarted := fixture.startHelper(t)
	fixture.activeHelper = restarted
	fixture.activeHelperPID = restarted.cmd.Process.Pid
	fixture.waitAgentOnline(t)
	fixture.assertProcessIdentity(t, fixture.env.agentPID, fixture.env.agentExe)
	if !restarted.isAlive() {
		t.Fatal("TC-09 manually restarted Helper exited before retry")
	}
	var conflict map[string]string
	fixture.postJSON(
		t,
		"/api/delete/execute",
		map[string]any{
			"confirm_token": offlineToken,
			"mode":          proto.ModeHard,
		},
		http.StatusConflict,
		&conflict,
	)
	newToken := fixture.prepareDelete(t, seed.memberIDs)
	observer := fixture.observePendingSync(t, seed.paths)
	retryTask := fixture.executeDelete(t, newToken, proto.ModeHard)
	retryStatus := fixture.waitDeleteComplete(t, retryTask)
	fixture.writeTC09Diagnostics(t, observer, seed, retryTask, retryStatus)
	observer.finish(t)
	assertM5StatusCounts(t, retryStatus, 2, 2, 0, 0)
	fixture.assertPersistence(t, seed, []string{"offline-a", "offline-b"})
	fixture.assert["offline_no_uac_or_auto_launch"] = true
	fixture.assert["offline_error_count"] = 2
	fixture.assert["agent_reconnected_same_pid"] = fixture.env.agentPID
	fixture.assert["restarted_helper_alive_for_retry"] = restarted.cmd.Process.Pid
	fixture.assert["newly_prepared_token_succeeded"] = true
}

func (fixture *m5Fixture) tc10PipeSecurity(t *testing.T) {
	if err := validateM5SecondWindowsStatus(fixture.env.secondWindowsStatus); err != nil {
		t.Fatalf("TC-10 remote validation gate: %v", err)
	}
	framed, hello, sddl := fixture.dialHelperWithSecurity(t)
	_ = framed.Close()
	if hello.PID != fixture.activeHelperPID {
		t.Fatalf("TC-10 current-user Hello PID=%d, want %d", hello.PID, fixture.activeHelperPID)
	}
	networkFullDeny, err := m5PipeSDDLHasNetworkFullDeny(sddl)
	if err != nil {
		t.Fatalf("TC-10 parse real pipe DACL: %v", err)
	}
	if !networkFullDeny {
		t.Fatalf("TC-10 pipe SDDL lacks NETWORK deny: %q", sddl)
	}
	dialErr, cleanupErr := dialM5PipeWithNetworkRestrictedToken(
		fixture.env.pipeName,
	)
	if cleanupErr != nil {
		t.Fatalf("TC-10 restricted token cleanup: %v", cleanupErr)
	}
	if !errors.Is(dialErr, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf(
			"TC-10 NETWORK restricted dial error=%v, want ERROR_ACCESS_DENIED",
			dialErr,
		)
	}
	framed, hello = fixture.dialHelper(t)
	_ = framed.Close()
	if hello.PID != fixture.activeHelperPID {
		t.Fatalf("TC-10 post-restriction current-user Hello=%#v", hello)
	}
	fixture.assert["current_user_connected"] = true
	fixture.assert["network_restricted_denied"] = true
	fixture.assert["sddl_network_deny"] = true
	fixture.assert["second_windows_status"] = fixture.env.secondWindowsStatus
}

func (fixture *m5Fixture) tc11LifetimeAndShutdown(t *testing.T) {
	data := []byte("tc11-helper-lifetime")
	path := fixture.writeGenerated(
		t,
		filepath.Join("tc11", "lifetime.bin"),
		data,
	)
	report, hello := fixture.directDelete(t, proto.DeleteTask{
		TaskID:    uuid.NewString(),
		Mode:      proto.ModeHard,
		Confirmed: true,
		Entries:   []string{path},
	})
	if hello.PID != fixture.activeHelperPID ||
		report.Stats != (proto.DeleteStats{Total: 1, OK: 1}) {
		t.Fatalf("TC-11 direct task Hello/report=%#v/%#v", hello, report)
	}
	if !m5ProcessAlive(fixture.activeHelperPID) {
		t.Fatal("TC-11 Helper exited after task completion")
	}
	connection, hello := fixture.dialHelper(t)
	_ = connection.Close()
	if hello.PID != fixture.activeHelperPID ||
		!m5ProcessAlive(fixture.activeHelperPID) {
		t.Fatal("TC-11 Helper did not survive client disconnect")
	}
	fixture.shutdownHelper(t, fixture.activeHelperPID)
	if fixture.activeHelper != nil {
		_ = fixture.activeHelper.wait()
	}
	fixture.activeHelper = nil
	fixture.activeHelperPID = 0
	fixture.assert["alive_after_task"] = true
	fixture.assert["alive_after_disconnect"] = true
	fixture.assert["exited_only_after_shutdown"] = true
}

func (fixture *m5Fixture) tc12KillAfterFirstChunk(t *testing.T) {
	active := fixture.startHelper(t)
	fixture.activeHelper = active
	fixture.activeHelperPID = active.cmd.Process.Pid
	connection, hello := fixture.dialHelper(t)
	_ = connection.Close()
	if hello.PID != active.cmd.Process.Pid {
		t.Fatalf(
			"TC-12 Hello PID=%d, exact process handle PID=%d",
			hello.PID,
			active.cmd.Process.Pid,
		)
	}

	seed := fixture.seedLargeCase(t, "tc12", 4500)
	laterPaths := append([]string(nil), seed.paths[4000:4500]...)
	if len(laterPaths) != 500 {
		t.Fatalf("TC-12 later chunk path count=%d, want 500", len(laterPaths))
	}
	laterFileBaseline, err := captureM5FileInvariants(laterPaths)
	if err != nil {
		t.Fatalf("capture TC-12 later file invariants: %v", err)
	}
	laterSQLiteBaseline, err := loadM5SQLiteFileInvariants(
		fixture.ctx,
		fixture.sqlite,
		fixture.env.machineID,
		laterPaths,
	)
	if err != nil {
		t.Fatalf("capture TC-12 later SQLite invariants: %v", err)
	}
	laterPostgresBaseline, err := loadM5PostgresFileInvariants(
		fixture.ctx,
		fixture.pg,
		fixture.env.machineID,
		laterPaths,
	)
	if err != nil {
		t.Fatalf(
			"capture TC-12 later PostgreSQL invariants: %v",
			redactM5Error(err),
		)
	}
	laterNoPending, err := newM5NoPendingObserver(
		fixture.ctx,
		fixture.sqlite,
		laterPaths,
	)
	if err != nil {
		t.Fatalf("start TC-12 later no-pending observer: %v", err)
	}
	if err := laterNoPending.verify(); err != nil {
		t.Fatalf("TC-12 later paths are not clean at baseline: %v", err)
	}
	token := fixture.prepareDelete(t, seed.memberIDs)
	observer := fixture.observePendingSync(t, seed.paths[:2000])
	taskID := fixture.executeDelete(t, token, proto.ModeHard)
	first := fixture.waitDeleteStatus(
		t,
		taskID,
		func(status DeleteTaskStatus) bool {
			machine, ok := status.ByMachine[fixture.env.machineID]
			if !ok {
				return false
			}
			sequence0, zero := machine.Sequences[0]
			sequence1, one := machine.Sequences[1]
			return zero && sequence0.Received &&
				(!one || !sequence1.Received) &&
				!status.Complete
		},
		"first TC-12 result before next chunk",
	)
	if first.ByMachine[fixture.env.machineID].Sequences[0].Total != 2000 {
		t.Fatalf("TC-12 first sequence=%#v", first)
	}
	fixture.assertProcessIdentity(t, hello.PID, fixture.env.helperExe)
	if err := active.killExact(); err != nil {
		t.Fatalf("kill exact TC-12 Hello process: %v", err)
	}
	waitM5ProcessExit(t, fixture.ctx, hello.PID)
	status := fixture.waitDeleteComplete(t, taskID)
	observer.finish(t)
	assertM5StatusCounts(t, status, 4500, 2000, 2500, 2000)
	if status.ErrorCodes[proto.DeleteErrHelperLost] != 2500 {
		t.Fatalf("TC-12 error codes=%#v", status.ErrorCodes)
	}
	machine := status.ByMachine[fixture.env.machineID]
	want := map[uint32]DeleteSequenceStatus{
		0: {Total: 2000, OK: 2000, Failed: 0, Uncertain: 0, Received: true},
		1: {Total: 2000, OK: 0, Failed: 2000, Uncertain: 2000, Received: true},
		2: {Total: 500, OK: 0, Failed: 500, Uncertain: 0, Received: true},
	}
	for sequence, expected := range want {
		actual, ok := machine.Sequences[sequence]
		if !ok ||
			actual.Total != expected.Total ||
			actual.OK != expected.OK ||
			actual.Failed != expected.Failed ||
			actual.Uncertain != expected.Uncertain ||
			!actual.Received {
			t.Fatalf(
				"TC-12 sequence %d=%#v, want totals %#v",
				sequence,
				actual,
				expected,
			)
		}
	}
	fixture.waitAgentOnline(t)
	// A repeated execute of the consumed token is an idempotent replay: it
	// returns the first accepted task instead of a conflict.
	var replay map[string]string
	fixture.postJSON(
		t,
		"/api/delete/execute",
		map[string]any{"confirm_token": token, "mode": proto.ModeHard},
		http.StatusOK,
		&replay,
	)
	if replay["task_id"] != taskID {
		t.Fatalf("TC-12 replay task_id=%q, want %q", replay["task_id"], taskID)
	}

	restarted := fixture.startHelper(t)
	fixture.activeHelper = restarted
	fixture.activeHelperPID = restarted.cmd.Process.Pid
	retryName := "02000"
	retryID := seed.idByName[retryName]
	newToken := fixture.prepareDelete(t, []int64{retryID})
	retryObserver := fixture.observePendingSync(t, []string{
		seed.pathByName[retryName],
	})
	retryTask := fixture.executeDelete(t, newToken, proto.ModeHard)
	retryStatus := fixture.waitDeleteComplete(t, retryTask)
	retryObserver.finish(t)
	assertM5StatusCounts(t, retryStatus, 1, 1, 0, 0)

	successNames := make([]string, 0, 2001)
	for index := 0; index < 2000; index++ {
		successNames = append(successNames, fmt.Sprintf("%05d", index))
	}
	successNames = append(successNames, retryName)
	fixture.assertPersistence(t, seed, successNames)
	if err := verifyM5FileInvariants(laterFileBaseline); err != nil {
		t.Fatalf("TC-12 later physical file changed: %v", err)
	}
	laterSQLiteObserved, err := loadM5SQLiteFileInvariants(
		fixture.ctx,
		fixture.sqlite,
		fixture.env.machineID,
		laterPaths,
	)
	if err != nil {
		t.Fatalf("load TC-12 later SQLite invariants: %v", err)
	}
	if err := compareM5DBFileInvariants(
		laterSQLiteBaseline,
		laterSQLiteObserved,
	); err != nil {
		t.Fatalf("TC-12 later SQLite state changed: %v", err)
	}
	laterPostgresObserved, err := loadM5PostgresFileInvariants(
		fixture.ctx,
		fixture.pg,
		fixture.env.machineID,
		laterPaths,
	)
	if err != nil {
		t.Fatalf(
			"load TC-12 later PostgreSQL invariants: %v",
			redactM5Error(err),
		)
	}
	if err := compareM5DBFileInvariants(
		laterPostgresBaseline,
		laterPostgresObserved,
	); err != nil {
		t.Fatalf("TC-12 later PostgreSQL state changed: %v", err)
	}
	if err := laterNoPending.verify(); err != nil {
		t.Fatalf("TC-12 later chunk emitted deletion upstream: %v", err)
	}
	fixture.shutdownHelper(t, fixture.activeHelperPID)
	_ = fixture.activeHelper.wait()
	fixture.activeHelper = nil
	fixture.activeHelperPID = 0
	fixture.assert["hello_pid_killed_exactly"] = hello.PID
	fixture.assert["current_chunk_uncertain"] = 2000
	fixture.assert["later_chunks_no_replay"] = 500
	fixture.assert["later_chunk_files_unchanged"] = len(laterPaths)
	fixture.assert["later_chunk_sqlite_unchanged"] = len(laterSQLiteObserved)
	fixture.assert["later_chunk_postgres_unchanged"] = len(laterPostgresObserved)
	fixture.assert["later_chunk_sync_queue_no_delete_upstream"] = len(laterPaths)
	fixture.assert["agent_survived"] = true
	fixture.assert["new_token_after_restart"] = true
}

func openM5ExclusiveHandle(t *testing.T, path string) windows.Handle {
	t.Helper()
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(
		pathUTF16,
		windows.GENERIC_READ,
		0,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatalf("open exclusive generated handle %q: %v", path, err)
	}
	return handle
}

func saveM5ACL(t *testing.T, path string) string {
	t.Helper()
	sddl, err := readM5ACL(path)
	if err != nil {
		t.Fatalf("save ACL %q: %v", path, err)
	}
	return sddl
}

func readM5ACL(path string) (string, error) {
	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|
			windows.GROUP_SECURITY_INFORMATION|
			windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return "", err
	}
	sddl := descriptor.String()
	if sddl == "" {
		return "", errors.New("security descriptor string is empty")
	}
	return sddl, nil
}

func restoreM5ACL(path, sddl string) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("stat ACL target: %w", err)
	}
	if err := setM5SecurityDescriptor(path, sddl); err != nil {
		return err
	}
	restored, err := readM5ACL(path)
	if err != nil {
		return fmt.Errorf("read restored ACL: %w", err)
	}
	if restored != sddl {
		return fmt.Errorf("restored ACL differs from saved ACL")
	}
	return nil
}

func setM5SecurityDescriptor(path, sddl string) error {
	information, owner, group, dacl, err := m5SecurityDescriptorParts(sddl)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		information,
		owner,
		group,
		dacl,
		nil,
	)
}

func m5SecurityDescriptorParts(
	sddl string,
) (
	windows.SECURITY_INFORMATION,
	*windows.SID,
	*windows.SID,
	*windows.ACL,
	error,
) {
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return 0, nil, nil, nil, err
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return 0, nil, nil, nil, err
	}
	group, _, err := descriptor.Group()
	if err != nil {
		return 0, nil, nil, nil, err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return 0, nil, nil, nil, err
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return 0, nil, nil, nil, err
	}
	information := windows.SECURITY_INFORMATION(
		windows.DACL_SECURITY_INFORMATION,
	)
	if owner != nil {
		information |= windows.OWNER_SECURITY_INFORMATION
	}
	if group != nil {
		information |= windows.GROUP_SECURITY_INFORMATION
	}
	if control&windows.SE_DACL_PROTECTED != 0 {
		information |= windows.PROTECTED_DACL_SECURITY_INFORMATION
	} else {
		information |= windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	}
	return information, owner, group, dacl, nil
}

func denyM5Delete(t *testing.T, directory, file string) {
	t.Helper()
	var token windows.Token
	if err := windows.OpenProcessToken(
		windows.CurrentProcess(),
		windows.TOKEN_QUERY,
		&token,
	); err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	directorySDDL := fmt.Sprintf(
		"D:P(D;;0x00000040;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;%s)",
		sid,
		sid,
	)
	fileSDDL := fmt.Sprintf(
		"D:P(D;;SD;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;%s)",
		sid,
		sid,
	)
	if err := setM5SecurityDescriptor(directory, directorySDDL); err != nil {
		t.Fatalf("deny generated directory delete-child: %v", err)
	}
	if err := setM5SecurityDescriptor(file, fileSDDL); err != nil {
		t.Fatalf("deny generated file delete: %v", err)
	}
}

func createM5Junction(t *testing.T, link, target string) {
	t.Helper()
	output, err := exec.Command(
		"cmd.exe",
		"/c",
		"mklink",
		"/J",
		link,
		target,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("create generated junction: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove exact generated junction %q: %v", link, err)
		}
	})
}

func m5ProcessImage(pid int) (string, error) {
	handle, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false,
		uint32(pid),
	)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(handle)
	buffer := make([]uint16, 32768)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(
		handle,
		0,
		&buffer[0],
		&size,
	); err != nil {
		return "", err
	}
	return filepath.Clean(windows.UTF16ToString(buffer[:size])), nil
}

func findM5ProcessesByImage(t *testing.T, expected string) []int {
	t.Helper()
	pids, err := findM5ProcessesByImageExact(expected)
	if err != nil {
		t.Fatal(err)
	}
	return pids
}

func findM5ProcessesByImageExact(expected string) ([]int, error) {
	expectedAbsolute, err := filepath.Abs(expected)
	if err != nil {
		return nil, err
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	err = windows.Process32First(snapshot, &entry)
	var pids []int
	for err == nil {
		pid := int(entry.ProcessID)
		if image, imageErr := m5ProcessImage(pid); imageErr == nil &&
			strings.EqualFold(image, filepath.Clean(expectedAbsolute)) {
			pids = append(pids, pid)
		}
		err = windows.Process32Next(snapshot, &entry)
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, fmt.Errorf("enumerate exact Helper image: %w", err)
	}
	sort.Ints(pids)
	return pids, nil
}

func (fixture *m5Fixture) dialHelperWithSecurity(
	t *testing.T,
) (*proto.Conn, proto.Hello, string) {
	t.Helper()
	fixture.track("pipe", fixture.env.pipeName)
	ctx, cancel := context.WithTimeout(fixture.ctx, 5*time.Second)
	defer cancel()
	sddl, err := queryM5PipeSDDL(ctx, fixture.env.pipeName)
	if err != nil {
		t.Fatalf("query exact Helper pipe DACL: %v", err)
	}
	connection, err := winio.DialPipeContext(ctx, fixture.env.pipeName)
	if err != nil {
		t.Fatalf("dial Helper for Hello: %v", err)
	}
	framed := proto.NewConn(connection)
	_ = framed.SetReadDeadline(time.Now().Add(5 * time.Second))
	messageType, body, err := framed.ReadFrame()
	if err != nil {
		_ = framed.Close()
		t.Fatal(err)
	}
	decoded, err := proto.Decode(messageType, body)
	if err != nil {
		_ = framed.Close()
		t.Fatal(err)
	}
	hello, ok := decoded.(*proto.Hello)
	if messageType != proto.MsgHello || !ok || hello.PID <= 0 {
		_ = framed.Close()
		t.Fatalf("security-query Hello=%#v type=%d", decoded, messageType)
	}
	return framed, *hello, sddl
}

func queryM5PipeSDDL(ctx context.Context, name string) (string, error) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return "", fmt.Errorf("encode named-pipe path: %w", err)
	}
	retry := time.NewTicker(10 * time.Millisecond)
	defer retry.Stop()
	for {
		handle, openErr := windows.CreateFile(
			namePtr,
			windows.READ_CONTROL,
			0,
			nil,
			windows.OPEN_EXISTING,
			windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION,
			0,
		)
		if openErr == nil {
			descriptor, queryErr := windows.GetSecurityInfo(
				handle,
				windows.SE_KERNEL_OBJECT,
				windows.DACL_SECURITY_INFORMATION,
			)
			closeErr := windows.CloseHandle(handle)
			if queryErr != nil {
				return "", fmt.Errorf("query exact named-pipe DACL: %w", queryErr)
			}
			if closeErr != nil {
				return "", fmt.Errorf("close exact named-pipe security handle: %w", closeErr)
			}
			if descriptor == nil {
				return "", errors.New("query exact named-pipe DACL returned nil descriptor")
			}
			return descriptor.String(), nil
		}
		if !errors.Is(openErr, windows.ERROR_PIPE_BUSY) {
			return "", fmt.Errorf(
				"open exact named pipe for READ_CONTROL: %w",
				openErr,
			)
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf(
				"open exact named pipe for READ_CONTROL: %w",
				ctx.Err(),
			)
		case <-retry.C:
		}
	}
}

func m5PipeSDDLHasNetworkFullDeny(sddl string) (bool, error) {
	const fileAllAccess windows.ACCESS_MASK = 0x001F01FF

	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return false, fmt.Errorf("parse named-pipe SDDL: %w", err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return false, fmt.Errorf("read named-pipe DACL: %w", err)
	}
	if dacl == nil {
		return false, errors.New("named-pipe security descriptor has no DACL")
	}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return false, fmt.Errorf("read named-pipe ACE %d: %w", index, err)
		}
		if ace == nil || ace.Header.AceType != windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() || !sid.IsWellKnown(windows.WinNetworkSid) {
			continue
		}
		if ace.Mask&windows.GENERIC_ALL == windows.GENERIC_ALL ||
			ace.Mask&fileAllAccess == fileAllAccess {
			return true, nil
		}
	}
	return false, nil
}

func m5TokenContainsGroupSID(
	token windows.Token,
	want *windows.SID,
) (bool, error) {
	if want == nil || !want.IsValid() {
		return false, errors.New("invalid token-group SID")
	}
	var size uint32
	err := windows.GetTokenInformation(
		token,
		windows.TokenGroups,
		nil,
		0,
		&size,
	)
	if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
		return false, fmt.Errorf("GetTokenInformation(TokenGroups) size: %w", err)
	}
	if size < uint32(unsafe.Sizeof(windows.Tokengroups{})) {
		return false, errors.New("GetTokenInformation(TokenGroups) returned a short buffer")
	}
	buffer := make([]byte, size)
	if err := windows.GetTokenInformation(
		token,
		windows.TokenGroups,
		&buffer[0],
		uint32(len(buffer)),
		&size,
	); err != nil {
		return false, fmt.Errorf("GetTokenInformation(TokenGroups): %w", err)
	}
	groups := (*windows.Tokengroups)(unsafe.Pointer(&buffer[0]))
	contains := m5SIDAndAttributesContain(groups.AllGroups(), want)
	runtime.KeepAlive(buffer)
	return contains, nil
}

func m5SIDAndAttributesContain(
	groups []windows.SIDAndAttributes,
	want *windows.SID,
) bool {
	if want == nil || !want.IsValid() {
		return false
	}
	for _, group := range groups {
		if group.Sid != nil && group.Sid.IsValid() && group.Sid.Equals(want) {
			return true
		}
	}
	return false
}

var m5CreateRestrictedToken = windows.NewLazySystemDLL("advapi32.dll").
	NewProc("CreateRestrictedToken")

func dialM5PipeWithNetworkRestrictedToken(
	name string,
) (dialErr, cleanupErr error) {
	type result struct {
		dialErr    error
		cleanupErr error
	}
	resultChannel := make(chan result, 1)
	go func() {
		attemptedDial, attemptedCleanup := func() (
			dialErr error,
			cleanupErr error,
		) {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()

			var existing windows.Token
			beforeErr := windows.OpenThreadToken(
				windows.CurrentThread(),
				windows.TOKEN_QUERY|windows.TOKEN_IMPERSONATE,
				true,
				&existing,
			)
			if beforeErr == nil {
				_ = existing.Close()
				return nil, errors.New("dedicated restricted-token thread already impersonates")
			}
			if !errors.Is(beforeErr, windows.ERROR_NO_TOKEN) {
				return nil, fmt.Errorf("OpenThreadToken before: %w", beforeErr)
			}

			token, err := makeM5NetworkRestrictedToken()
			if err != nil {
				return nil, err
			}
			defer func() {
				cleanupErr = errors.Join(cleanupErr, token.Close())
			}()
			if err := windows.SetThreadToken(nil, token); err != nil {
				return nil, fmt.Errorf("SetThreadToken: %w", err)
			}
			impersonating := true
			defer func() {
				if impersonating {
					cleanupErr = errors.Join(cleanupErr, windows.RevertToSelf())
				}
			}()

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			connection, dialErr := winio.DialPipeContext(ctx, name)
			cancel()
			if connection != nil {
				cleanupErr = errors.Join(cleanupErr, connection.Close())
			}
			revertErr := windows.RevertToSelf()
			impersonating = false
			cleanupErr = errors.Join(cleanupErr, revertErr)
			var after windows.Token
			afterErr := windows.OpenThreadToken(
				windows.CurrentThread(),
				windows.TOKEN_QUERY,
				true,
				&after,
			)
			if afterErr == nil {
				cleanupErr = errors.Join(
					cleanupErr,
					after.Close(),
					errors.New("restricted token remained after RevertToSelf"),
				)
			} else if !errors.Is(afterErr, windows.ERROR_NO_TOKEN) {
				cleanupErr = errors.Join(
					cleanupErr,
					fmt.Errorf("OpenThreadToken after: %w", afterErr),
				)
			}
			return dialErr, cleanupErr
		}()
		resultChannel <- result{
			dialErr: attemptedDial, cleanupErr: attemptedCleanup,
		}
	}()
	got := <-resultChannel
	return got.dialErr, got.cleanupErr
}

func makeM5NetworkRestrictedToken() (windows.Token, error) {
	var processToken windows.Token
	if err := windows.OpenProcessToken(
		windows.CurrentProcess(),
		windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE,
		&processToken,
	); err != nil {
		return 0, fmt.Errorf("OpenProcessToken: %w", err)
	}
	defer processToken.Close()

	var impersonation windows.Token
	if err := windows.DuplicateTokenEx(
		processToken,
		windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE|windows.TOKEN_IMPERSONATE,
		nil,
		windows.SecurityImpersonation,
		windows.TokenImpersonation,
		&impersonation,
	); err != nil {
		return 0, fmt.Errorf("DuplicateTokenEx: %w", err)
	}
	defer impersonation.Close()
	networkSID, err := windows.CreateWellKnownSid(windows.WinNetworkSid)
	if err != nil {
		return 0, fmt.Errorf("CreateWellKnownSid(NETWORK): %w", err)
	}
	restriction := windows.SIDAndAttributes{Sid: networkSID}
	var restricted windows.Token
	result, _, callErr := m5CreateRestrictedToken.Call(
		uintptr(impersonation),
		0,
		0,
		0,
		0,
		0,
		1,
		uintptr(unsafe.Pointer(&restriction)),
		uintptr(unsafe.Pointer(&restricted)),
	)
	runtime.KeepAlive(networkSID)
	runtime.KeepAlive(restriction)
	if result == 0 {
		if callErr == nil || callErr == syscall.Errno(0) {
			callErr = windows.ERROR_INVALID_FUNCTION
		}
		return 0, fmt.Errorf("CreateRestrictedToken: %w", callErr)
	}
	isRestricted, err := restricted.IsRestricted()
	if err != nil {
		_ = restricted.Close()
		return 0, err
	}
	containsNetwork, err := m5RestrictedTokenContainsSID(restricted, networkSID)
	if err != nil {
		_ = restricted.Close()
		return 0, err
	}
	if !isRestricted || !containsNetwork {
		_ = restricted.Close()
		return 0, errors.New("restricted token lacks NETWORK restricting SID")
	}
	return restricted, nil
}

func m5RestrictedTokenContainsSID(
	token windows.Token,
	want *windows.SID,
) (bool, error) {
	var size uint32
	err := windows.GetTokenInformation(
		token,
		windows.TokenRestrictedSids,
		nil,
		0,
		&size,
	)
	if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
		return false, fmt.Errorf("GetTokenInformation size: %w", err)
	}
	buffer := make([]byte, size)
	if err := windows.GetTokenInformation(
		token,
		windows.TokenRestrictedSids,
		&buffer[0],
		uint32(len(buffer)),
		&size,
	); err != nil {
		return false, err
	}
	groups := (*windows.Tokengroups)(unsafe.Pointer(&buffer[0]))
	for _, group := range groups.AllGroups() {
		if group.Sid != nil && group.Sid.Equals(want) {
			runtime.KeepAlive(buffer)
			return true, nil
		}
	}
	runtime.KeepAlive(buffer)
	return false, nil
}
