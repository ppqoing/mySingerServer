package gui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"dedup/internal/proto"
)

func TestTaskRegistryRestoresPendingScanEnvelopeWhenIntegrationEnabled(
	t *testing.T,
) {
	dsn := os.Getenv("DEDUP_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set DEDUP_TEST_PG_DSN to run PostgreSQL integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	const taskID = "integration-restore-task"
	const phase2TaskID = "integration-restore-phase2-isolation-task"
	if _, err := pool.Exec(
		ctx,
		`DELETE FROM scan_tasks WHERE id=ANY($1::text[])`,
		[]string{taskID, phase2TaskID},
	); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = pool.Exec(
			cleanupCtx,
			`DELETE FROM scan_tasks WHERE id=ANY($1::text[])`,
			[]string{taskID, phase2TaskID},
		)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO scan_tasks (id, machine_id, phase, target, status)
		VALUES ($1, $2, 1, $3::jsonb, 'running')`,
		taskID,
		"integration-restore-machine",
		`{"roots":["D:\\media"],"rescan":true}`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO scan_tasks (id, machine_id, phase, target, status)
		VALUES ($1, $2, 2, $3::jsonb, 'running')`,
		phase2TaskID,
		"integration-restore-machine",
		`{"type":"phase2","machine_id":"integration-restore-machine","task":{"task_id":"integration-restore-phase2-isolation-task","items":[]}}`,
	); err != nil {
		t.Fatal(err)
	}

	registry := NewTaskRegistry(pool, testLogger())
	if err := registry.Restore(ctx); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	scans := registry.PendingScans("integration-restore-machine")
	if len(scans) != 1 || scans[0].TaskID != taskID ||
		len(scans[0].Roots) != 1 || scans[0].Roots[0] != `D:\media` ||
		!scans[0].Options.Rescan {
		t.Fatalf("restored scans = %#v", scans)
	}
}

func TestTaskRegistryPersistsRescanOptionWhenIntegrationEnabled(t *testing.T) {
	dsn := os.Getenv("DEDUP_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set DEDUP_TEST_PG_DSN to run PostgreSQL integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	const taskID = "integration-persist-task"
	if _, err := pool.Exec(ctx, `DELETE FROM scan_tasks WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM scan_tasks WHERE id=$1`, taskID)
	})

	registry := NewTaskRegistry(pool, testLogger())
	registry.Register(&TaskInfo{
		TaskID: taskID, MachineID: "integration-persist-machine",
		Phase: 1, Roots: []string{`D:\media`}, Rescan: true, Status: "sent",
	})
	var rescan bool
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE((target->>'rescan')::boolean, false)
		FROM scan_tasks WHERE id=$1`,
		taskID,
	).Scan(&rescan); err != nil {
		t.Fatal(err)
	}
	if !rescan {
		t.Fatal("persisted target lost rescan=true")
	}
	registry.Dispatch("integration-persist-machine", &proto.TaskProgress{
		TaskID: taskID, Done: 2, Total: 10, Speed: 1,
	})
	var status string
	if err := pool.QueryRow(ctx, `
		SELECT status FROM scan_tasks WHERE id=$1`,
		taskID,
	).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "running" {
		t.Fatalf("persisted status = %q, want running", status)
	}
}

func TestTaskRegistryRejectsPersistedEnvelopeConflictWhenIntegrationEnabled(
	t *testing.T,
) {
	dsn := os.Getenv("DEDUP_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set DEDUP_TEST_PG_DSN to run PostgreSQL integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	const taskID = "integration-envelope-conflict-task"
	if _, err := pool.Exec(ctx, `DELETE FROM scan_tasks WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM scan_tasks WHERE id=$1`, taskID)
	})

	first := NewTaskRegistry(pool, testLogger())
	if err := first.Register(&TaskInfo{
		TaskID: taskID, MachineID: "integration-conflict-machine",
		Phase: 1, Roots: []string{`D:\one`}, Status: "sent",
	}); err != nil {
		t.Fatal(err)
	}
	afterRestart := NewTaskRegistry(pool, testLogger())
	err = afterRestart.Register(&TaskInfo{
		TaskID: taskID, MachineID: "integration-conflict-machine",
		Phase: 1, Roots: []string{`D:\two`}, Status: "sent",
	})
	if !errors.Is(err, ErrTaskEnvelopeConflict) {
		t.Fatalf("Register conflict error = %v, want ErrTaskEnvelopeConflict", err)
	}
	var root string
	if err := pool.QueryRow(ctx, `
		SELECT target->'roots'->>0 FROM scan_tasks WHERE id=$1`,
		taskID,
	).Scan(&root); err != nil {
		t.Fatal(err)
	}
	if root != `D:\one` {
		t.Fatalf("persisted root = %q, want original envelope", root)
	}
}

func TestDuplicateQueriesExcludeDeletedRowsWhenIntegrationEnabled(t *testing.T) {
	dsn := os.Getenv("DEDUP_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set DEDUP_TEST_PG_DSN to run PostgreSQL integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	hash := strings.Repeat("ab", 64)
	machines := []string{"integration-gui-a", "integration-gui-b", "integration-gui-deleted"}
	for _, machine := range machines {
		_, _ = pool.Exec(ctx, `DELETE FROM files WHERE machine_id=$1`, machine)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		for _, machine := range machines {
			_, _ = pool.Exec(cleanupCtx, `DELETE FROM files WHERE machine_id=$1`, machine)
		}
	})
	for index, machine := range machines {
		status := "done"
		if index == 2 {
			status = "deleted"
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO files (
			    machine_id, disk_no, path, size, mtime, sha512,
			    phase1_done, phase2_done, status, missing_mask, updated_at
			)
			VALUES ($1,1,$2,100,1,$3,1,0,$4,0,1)
			ON CONFLICT (machine_id,path) DO UPDATE SET
			    sha512=EXCLUDED.sha512, status=EXCLUDED.status`,
			machine, `D:\`+machine+`.bin`, hash, status); err != nil {
			t.Fatal(err)
		}
	}
	api := NewAPI(NewPool(nil, testLogger(), nil),
		NewTaskRegistry(pool, testLogger()), pool)

	groupsRequest := httptest.NewRequest(http.MethodGet, "/api/dup_groups", nil)
	groupsResponse := httptest.NewRecorder()
	api.Routes().ServeHTTP(groupsResponse, groupsRequest)
	if groupsResponse.Code != http.StatusOK {
		t.Fatalf("groups status=%d body=%s", groupsResponse.Code, groupsResponse.Body.String())
	}
	var groups []DupGroup
	if err := json.Unmarshal(groupsResponse.Body.Bytes(), &groups); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, group := range groups {
		if group.SHA512 == hash {
			found = true
			if group.MemberCount != 2 || group.Machines != 2 {
				t.Fatalf("group = %#v, deleted row leaked", group)
			}
		}
	}
	if !found {
		t.Fatalf("hash %s missing from groups: %#v", hash, groups)
	}

	membersRequest := httptest.NewRequest(http.MethodGet, "/api/dup_groups/"+hash, nil)
	membersResponse := httptest.NewRecorder()
	api.Routes().ServeHTTP(membersResponse, membersRequest)
	if membersResponse.Code != http.StatusOK {
		t.Fatalf("members status=%d body=%s", membersResponse.Code, membersResponse.Body.String())
	}
	var members []DupMember
	if err := json.Unmarshal(membersResponse.Body.Bytes(), &members); err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 {
		t.Fatalf("members = %#v, want 2 non-deleted rows", members)
	}
}
