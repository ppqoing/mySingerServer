//go:build windows

package integration_test

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestM1TwoAgentGUIRestartAndExactGroupsWhenEnabled(t *testing.T) {
	binDir := os.Getenv("DEDUP_TEST_M1_BIN_DIR")
	dsn := os.Getenv("DEDUP_TEST_PG_DSN")
	if binDir == "" || dsn == "" {
		t.Skip("set DEDUP_TEST_M1_BIN_DIR and DEDUP_TEST_PG_DSN to run M1 E2E")
	}
	agentExe := filepath.Join(binDir, "agent.exe")
	guiExe := filepath.Join(binDir, "gui.exe")
	for _, executable := range []string{agentExe, guiExe} {
		if _, err := os.Stat(executable); err != nil {
			t.Fatalf("required binary %s: %v", executable, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	machineA := "m1-e2e-a"
	machineB := "m1-e2e-b"
	taskA := "11111111-1111-4111-8111-111111111111"
	taskB := "22222222-2222-4222-8222-222222222222"
	rescanTask := "33333333-3333-4333-8333-333333333333"
	cleanupCentralRows(t, pool, machineA, machineB, taskA, taskB, rescanTask)

	temp := t.TempDir()
	rootA := filepath.Join(temp, "corpus-a")
	rootB := filepath.Join(temp, "corpus-b")
	expected := createCorpus(t, rootA, rootB)

	addrA := freeAddress(t)
	addrB := freeAddress(t)
	guiAddr := freeAddress(t)
	configA := filepath.Join(temp, "agent-a.json")
	configB := filepath.Join(temp, "agent-b.json")
	guiConfig := filepath.Join(temp, "gui.json")
	writeJSON(t, configA, agentConfig(
		machineA, addrA, filepath.Join(temp, "data-a"), dsn,
	))
	writeJSON(t, configB, agentConfig(
		machineB, addrB, filepath.Join(temp, "data-b"), dsn,
	))
	writeJSON(t, guiConfig, map[string]any{
		"listen_addr": guiAddr,
		"pg_dsn":      dsn,
		"heartbeat_s": 1,
		"agents": []map[string]string{
			{"machine_id": machineA, "addr": addrA},
			{"machine_id": machineB, "addr": addrB},
		},
	})

	agentA := startProcess(t, ctx, agentExe, configA)
	agentB := startProcess(t, ctx, agentExe, configB)
	defer agentA.stop()
	defer agentB.stop()
	gui := startProcess(t, ctx, guiExe, guiConfig)
	defer gui.stop()
	client := &http.Client{Timeout: 2 * time.Second}
	baseURL := "http://" + guiAddr
	waitFor(t, ctx, "both Agents online", []*runningProcess{agentA, agentB, gui},
		func() (bool, error) {
			return allAgentsOnline(client, baseURL, machineA, machineB)
		})

	postScan(t, client, baseURL, taskA, machineA, rootA)
	postScan(t, client, baseURL, taskB, machineB, rootB)
	gui.stop()
	gui = startProcess(t, ctx, guiExe, guiConfig)
	defer gui.stop()
	waitFor(t, ctx, "Agents reconnect after GUI restart",
		[]*runningProcess{agentA, agentB, gui},
		func() (bool, error) {
			return allAgentsOnline(client, baseURL, machineA, machineB)
		})
	waitFor(t, ctx, "restored scans complete",
		[]*runningProcess{agentA, agentB, gui},
		func() (bool, error) {
			return tasksDone(client, baseURL, taskA, taskB)
		})

	waitFor(t, ctx, "all Agent rows synchronize",
		[]*runningProcess{agentA, agentB, gui},
		func() (bool, error) {
			var count int
			err := pool.QueryRow(ctx, `
				SELECT count(*) FROM files
				WHERE machine_id IN ($1,$2)`,
				machineA, machineB,
			).Scan(&count)
			return count == 12, err
		})
	assertExactGroups(t, ctx, pool, machineA, machineB, expected)
	assertHTTPGroups(t, client, baseURL, expected)

	postScan(t, client, baseURL, rescanTask, machineA, rootA)
	waitFor(t, ctx, "idempotent rescan completes",
		[]*runningProcess{agentA, agentB, gui},
		func() (bool, error) {
			return tasksDone(client, baseURL, rescanTask)
		})
	var skipped, done int64
	if err := pool.QueryRow(ctx, `
		SELECT
		    COALESCE((stats_json->>'Skipped')::bigint, 0),
		    COALESCE((stats_json->>'Done')::bigint, 0)
		FROM scan_tasks WHERE id=$1`,
		rescanTask,
	).Scan(&skipped, &done); err != nil {
		t.Fatal(err)
	}
	if skipped != 7 || done != 0 {
		t.Fatalf("rescan stats skipped=%d done=%d, want 7/0", skipped, done)
	}

	sendOversizedFrame(t, addrA)
	waitFor(t, ctx, "Agent remains online after oversized frame",
		[]*runningProcess{agentA, agentB, gui},
		func() (bool, error) {
			return allAgentsOnline(client, baseURL, machineA, machineB)
		})
}

func agentConfig(machineID, addr, dataDir, dsn string) map[string]any {
	return map[string]any{
		"machine_id":     machineID,
		"listen_addr":    addr,
		"data_dir":       dataDir,
		"pg_dsn":         dsn,
		"use_everything": false,
		"scan": map[string]any{
			"hdd_read_block_mb":     4,
			"hdd_streams_per_disk":  2,
			"ssd_streams_per_disk":  4,
			"image_mem_resident_mb": 256,
			"image_timeout_s":       30,
			"video_timeout_s":       120,
			"image_exts":            []string{},
			"video_exts":            []string{},
		},
		"sync": map[string]any{
			"interval_s": 1, "trigger_rows": 1000, "upsert_batch": 100,
		},
		"proto": map[string]any{"heartbeat_s": 1},
	}
}

func createCorpus(t *testing.T, rootA, rootB string) map[string]groupWant {
	t.Helper()
	duplicateOne := []byte("duplicate-one")
	duplicateTwo := []byte("duplicate-two")
	duplicateThree := []byte("duplicate-three")
	corpora := map[string]map[string][]byte{
		rootA: {
			"one/dup1.bin": duplicateOne,
			"two/dup1.bin": duplicateOne,
			"dup2.bin":     duplicateTwo,
			"dup3.bin":     duplicateThree,
			"empty.dat":    {},
			"unique-a.bin": []byte("unique-a"),
			"big.bin":      bytes.Repeat([]byte{0x5a, 0xa5}, 4<<20),
		},
		rootB: {
			"dup1.bin":     duplicateOne,
			"dup2.bin":     duplicateTwo,
			"dup3.bin":     duplicateThree,
			"empty.dat":    {},
			"unique-b.bin": []byte("unique-b"),
		},
	}
	for root, files := range corpora {
		for relative, data := range files {
			path := filepath.Join(root, filepath.FromSlash(relative))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return map[string]groupWant{
		hashOf(duplicateOne):   {Members: 3, Machines: 2},
		hashOf(duplicateTwo):   {Members: 2, Machines: 2},
		hashOf(duplicateThree): {Members: 2, Machines: 2},
		hashOf(nil):            {Members: 2, Machines: 2},
	}
}

type groupWant struct {
	Members  int64
	Machines int64
}

func assertExactGroups(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	machineA, machineB string,
	want map[string]groupWant,
) {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT sha512, count(*), count(DISTINCT machine_id)
		FROM files
		WHERE machine_id IN ($1,$2) AND sha512 IS NOT NULL
		GROUP BY sha512 HAVING count(*) > 1
		ORDER BY sha512`,
		machineA, machineB,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := make(map[string]groupWant)
	for rows.Next() {
		var hash string
		var group groupWant
		if err := rows.Scan(&hash, &group.Members, &group.Machines); err != nil {
			t.Fatal(err)
		}
		got[hash] = group
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !groupsEqual(got, want) {
		t.Fatalf("exact groups = %#v, want %#v", got, want)
	}
}

func assertHTTPGroups(
	t *testing.T,
	client *http.Client,
	baseURL string,
	want map[string]groupWant,
) {
	t.Helper()
	response, err := client.Get(baseURL + "/api/dup_groups?limit=1000")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var groups []struct {
		SHA512      string `json:"sha512"`
		MemberCount int64  `json:"member_count"`
		Machines    int64  `json:"machines"`
	}
	if err := json.NewDecoder(response.Body).Decode(&groups); err != nil {
		t.Fatal(err)
	}
	got := make(map[string]groupWant)
	for _, group := range groups {
		if _, expected := want[group.SHA512]; expected {
			got[group.SHA512] = groupWant{
				Members: group.MemberCount, Machines: group.Machines,
			}
		}
	}
	if !groupsEqual(got, want) {
		t.Fatalf("HTTP groups = %#v, want %#v", got, want)
	}
}

func groupsEqual(left, right map[string]groupWant) bool {
	if len(left) != len(right) {
		return false
	}
	for hash, want := range right {
		if left[hash] != want {
			return false
		}
	}
	return true
}

func postScan(
	t *testing.T,
	client *http.Client,
	baseURL, taskID, machineID, root string,
) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"task_id": taskID, "machine_id": machineID,
		"roots": []string{root}, "phase": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Post(
		baseURL+"/api/scan",
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("POST scan status=%d body=%s", response.StatusCode, data)
	}
}

func allAgentsOnline(
	client *http.Client,
	baseURL string,
	machineIDs ...string,
) (bool, error) {
	response, err := client.Get(baseURL + "/api/agents")
	if err != nil {
		return false, nil
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false, nil
	}
	var statuses []struct {
		MachineID string `json:"machine_id"`
		Online    bool   `json:"online"`
	}
	if err := json.NewDecoder(response.Body).Decode(&statuses); err != nil {
		return false, err
	}
	online := make(map[string]bool, len(statuses))
	for _, status := range statuses {
		online[status.MachineID] = status.Online
	}
	for _, machineID := range machineIDs {
		if !online[machineID] {
			return false, nil
		}
	}
	return true, nil
}

func tasksDone(
	client *http.Client,
	baseURL string,
	taskIDs ...string,
) (bool, error) {
	response, err := client.Get(baseURL + "/api/tasks")
	if err != nil {
		return false, nil
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false, nil
	}
	var tasks []struct {
		TaskID string `json:"task_id"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(response.Body).Decode(&tasks); err != nil {
		return false, err
	}
	statuses := make(map[string]string, len(tasks))
	for _, task := range tasks {
		statuses[task.TaskID] = task.Status
	}
	for _, taskID := range taskIDs {
		if statuses[taskID] != "done" {
			return false, nil
		}
	}
	return true, nil
}

func sendOversizedFrame(t *testing.T, addr string) {
	t.Helper()
	connection, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	var helloLength uint32
	if err := binary.Read(connection, binary.BigEndian, &helloLength); err != nil {
		t.Fatalf("read Hello length: %v", err)
	}
	if helloLength == 0 || helloLength > 16<<20 {
		t.Fatalf("invalid Hello length %d", helloLength)
	}
	if _, err := io.CopyN(io.Discard, connection, int64(helloLength)); err != nil {
		t.Fatalf("read Hello body: %v", err)
	}
	if err := binary.Write(connection, binary.BigEndian, uint32(0x7fffffff)); err != nil {
		t.Fatal(err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var one [1]byte
	if _, err := connection.Read(one[:]); err == nil {
		t.Fatal("Agent kept oversized-frame connection open")
	}
}

func cleanupCentralRows(
	t *testing.T,
	pool *pgxpool.Pool,
	machineA, machineB string,
	taskIDs ...string,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx,
		`DELETE FROM files WHERE machine_id IN ($1,$2)`,
		machineA, machineB,
	); err != nil {
		t.Fatal(err)
	}
	for _, taskID := range taskIDs {
		if _, err := pool.Exec(ctx, `DELETE FROM scan_tasks WHERE id=$1`, taskID); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_, _ = pool.Exec(cleanupCtx,
			`DELETE FROM files WHERE machine_id IN ($1,$2)`,
			machineA, machineB,
		)
		for _, taskID := range taskIDs {
			_, _ = pool.Exec(cleanupCtx, `DELETE FROM scan_tasks WHERE id=$1`, taskID)
		}
	})
}

func hashOf(data []byte) string {
	sum := sha512.Sum512(data)
	return hex.EncodeToString(sum[:])
}

func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func waitFor(
	t *testing.T,
	ctx context.Context,
	description string,
	processes []*runningProcess,
	check func() (bool, error),
) {
	t.Helper()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		ok, err := check()
		if err != nil {
			lastErr = err
		}
		if ok {
			return
		}
		select {
		case <-ctx.Done():
			outputs := make([]string, 0, len(processes))
			for _, process := range processes {
				outputs = append(outputs, process.output.String())
			}
			sort.Strings(outputs)
			t.Fatalf(
				"timeout waiting for %s: last error=%v outputs=%q",
				description,
				lastErr,
				outputs,
			)
		case <-ticker.C:
		}
	}
}

type runningProcess struct {
	command *exec.Cmd
	done    chan error
	output  *lockedBuffer
	once    sync.Once
}

func startProcess(
	t *testing.T,
	ctx context.Context,
	executable, configPath string,
) *runningProcess {
	t.Helper()
	output := &lockedBuffer{}
	command := exec.CommandContext(ctx, executable, "-config", configPath)
	command.Stdout = output
	command.Stderr = output
	command.SysProcAttr = &syscall.SysProcAttr{
		HideWindow: true, CreationFlags: 0x08000000,
	}
	if err := command.Start(); err != nil {
		t.Fatalf("start %s: %v", executable, err)
	}
	process := &runningProcess{
		command: command,
		done:    make(chan error, 1),
		output:  output,
	}
	go func() {
		process.done <- command.Wait()
	}()
	return process
}

func (p *runningProcess) stop() {
	p.once.Do(func() {
		if p.command.Process != nil {
			_ = p.command.Process.Kill()
		}
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
		}
	})
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(data)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}
