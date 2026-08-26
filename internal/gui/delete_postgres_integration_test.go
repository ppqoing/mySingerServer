package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"dedup/internal/proto"
)

func TestDeletePrepareResolvesCanonicalMembersFromPostgres(t *testing.T) {
	fixture := newDeletePostgresFixture(t)
	service := fixture.service()

	summary, token, err := service.Prepare(
		fixture.ctx,
		[]int64{fixture.goodB, fixture.goodA, fixture.goodB},
	)
	if err != nil {
		t.Fatal(err)
	}
	wantSummary := DeleteSummary{
		TotalFiles: 2,
		TotalBytes: 30,
		ByMachine:  map[string]int64{fixture.machineA: 1, fixture.machineB: 1},
		Samples:    []string{fixture.pathA, fixture.pathB},
	}
	if !reflect.DeepEqual(summary, wantSummary) {
		t.Fatalf("summary = %#v, want %#v", summary, wantSummary)
	}
	members, err := service.confirms.Consume(token)
	if err != nil {
		t.Fatal(err)
	}
	wantMembers := []DeleteMember{
		{FileID: fixture.goodA, MachineID: fixture.machineA, Path: fixture.pathA, Size: 10},
		{FileID: fixture.goodB, MachineID: fixture.machineB, Path: fixture.pathB, Size: 20},
	}
	if !reflect.DeepEqual(members, wantMembers) {
		t.Fatalf("members = %#v, want %#v", members, wantMembers)
	}
}

func TestDeletePrepareRejectsWholePostgresSelectionOnAnyConflict(t *testing.T) {
	fixture := newDeletePostgresFixture(t)
	tests := []struct {
		name string
		id   int64
	}{
		{name: "missing", id: fixture.missing},
		{name: "deleted", id: fixture.deleted},
		{name: "representative", id: fixture.representative},
		{name: "non-member", id: fixture.nonMember},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			summary, token, err := fixture.service().Prepare(
				fixture.ctx,
				[]int64{fixture.goodA, test.id},
			)
			if !errors.Is(err, ErrDeleteSelection) {
				t.Fatalf("Prepare() error = %v, want ErrDeleteSelection", err)
			}
			if token != "" || !reflect.DeepEqual(summary, DeleteSummary{}) {
				t.Fatalf("conflict returned summary/token = %#v/%q", summary, token)
			}
		})
	}
}

type deletePostgresFixture struct {
	ctx            context.Context
	tx             pgx.Tx
	machineA       string
	machineB       string
	pathA          string
	pathB          string
	goodA          int64
	goodB          int64
	deleted        int64
	representative int64
	nonMember      int64
	missing        int64
}

func newDeletePostgresFixture(t *testing.T) *deletePostgresFixture {
	t.Helper()
	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		t.Skip("set PG_DSN to run delete PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = tx.Rollback(context.Background())
	})

	runID := uuid.NewString()
	fixture := &deletePostgresFixture{
		ctx:      ctx,
		tx:       tx,
		machineA: "task9-a-" + runID,
		machineB: "task9-b-" + runID,
		pathA:    fmt.Sprintf("task9://%s/a", runID),
		pathB:    fmt.Sprintf("task9://%s/b", runID),
	}
	fixture.goodA = fixture.insertFile(t, fixture.machineA, fixture.pathA, 10, "done")
	fixture.goodB = fixture.insertFile(t, fixture.machineB, fixture.pathB, 20, "done")
	fixture.deleted = fixture.insertFile(
		t, fixture.machineA, fmt.Sprintf("task9://%s/deleted", runID), 30, "deleted",
	)
	fixture.representative = fixture.insertFile(
		t, fixture.machineA, fmt.Sprintf("task9://%s/representative", runID), 40, "done",
	)
	fixture.nonMember = fixture.insertFile(
		t, fixture.machineA, fmt.Sprintf("task9://%s/non-member", runID), 50, "done",
	)
	fixture.missing = fixture.insertFile(
		t, fixture.machineA, fmt.Sprintf("task9://%s/missing", runID), 60, "done",
	)
	if _, err := tx.Exec(ctx, `DELETE FROM files WHERE id=$1`, fixture.missing); err != nil {
		t.Fatal(err)
	}

	groupID := fixture.insertGroup(t, fixture.representative)
	for _, fileID := range []int64{
		fixture.representative, fixture.goodA, fixture.goodB, fixture.deleted,
	} {
		if _, err := tx.Exec(ctx, `
			INSERT INTO dup_members(group_id,file_id,score_json)
			VALUES($1,$2,'{}'::jsonb)`, groupID, fileID); err != nil {
			t.Fatal(err)
		}
	}
	secondRepresentative := fixture.insertFile(
		t, fixture.machineB, fmt.Sprintf("task9://%s/second-representative", runID), 1, "done",
	)
	secondGroup := fixture.insertGroup(t, secondRepresentative)
	for _, fileID := range []int64{secondRepresentative, fixture.goodB} {
		if _, err := tx.Exec(ctx, `
			INSERT INTO dup_members(group_id,file_id,score_json)
			VALUES($1,$2,'{}'::jsonb)`, secondGroup, fileID); err != nil {
			t.Fatal(err)
		}
	}
	return fixture
}

func (fixture *deletePostgresFixture) insertFile(
	t *testing.T,
	machineID string,
	path string,
	size int64,
	status string,
) int64 {
	t.Helper()
	var id int64
	if err := fixture.tx.QueryRow(fixture.ctx, `
		INSERT INTO files(machine_id,disk_no,path,size,mtime,status)
		VALUES($1,9,$2,$3,1,$4)
		RETURNING id`,
		machineID, path, size, status,
	).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (fixture *deletePostgresFixture) insertGroup(
	t *testing.T,
	representative int64,
) int64 {
	t.Helper()
	var id int64
	if err := fixture.tx.QueryRow(fixture.ctx, `
		INSERT INTO dup_groups(kind,representative_file_id,member_count)
		VALUES('exact',$1,0)
		RETURNING id`,
		representative,
	).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (fixture *deletePostgresFixture) service() *DeleteService {
	return NewDeleteService(
		fixture.tx,
		nil,
		NewConfirmStore(time.Minute, time.Now),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
}

func TestDeleteTaskPersistenceRoundTrip(t *testing.T) {
	fixture := newDeletePostgresFixture(t)
	transport := &deleteTestTransport{
		online: map[string]bool{
			fixture.machineA: true,
			fixture.machineB: true,
		},
		onlineCalls: make(map[string]int),
		sendErrors:  make(map[string]error),
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := NewDeleteService(
		fixture.tx,
		transport,
		NewConfirmStore(time.Minute, time.Now),
		logger,
	)
	service.SetTaskStore(fixture.tx)

	summary, token, err := service.Prepare(
		fixture.ctx,
		[]int64{fixture.goodA, fixture.goodB},
	)
	if err != nil {
		t.Fatal(err)
	}
	if summary.TotalFiles != 2 {
		t.Fatalf("summary = %#v", summary)
	}
	taskID, err := service.Execute(fixture.ctx, token, "soft")
	if err != nil {
		t.Fatal(err)
	}

	// Task creation persisted a non-terminal snapshot.
	var (
		mode       string
		statusJSON []byte
	)
	if err := fixture.tx.QueryRow(fixture.ctx, `
		SELECT mode, status_json FROM delete_tasks WHERE id=$1`, taskID,
	).Scan(&mode, &statusJSON); err != nil {
		t.Fatal(err)
	}
	if mode != "soft" {
		t.Fatalf("persisted mode = %q, want soft", mode)
	}
	var persisted DeleteTaskStatus
	if err := json.Unmarshal(statusJSON, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.TaskID != taskID || persisted.Complete ||
		persisted.Pending != 2 || persisted.Total != 2 {
		t.Fatalf("persisted snapshot = %#v", persisted)
	}

	// Each accepted report refreshes the snapshot.
	service.HandleReport(fixture.machineA, &proto.DeleteReport{
		TaskID: taskID, Seq: 0, LastSeq: 0,
		Stats:   proto.DeleteStats{Total: 1, OK: 1},
		Entries: []proto.DeleteResult{{Path: fixture.pathA, OK: true, RecycledTo: `Z:\recycled\a`}},
	})
	if err := fixture.tx.QueryRow(fixture.ctx, `
		SELECT status_json FROM delete_tasks WHERE id=$1`, taskID,
	).Scan(&statusJSON); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(statusJSON, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.OK != 1 || persisted.Pending != 1 || persisted.Complete {
		t.Fatalf("snapshot after report = %#v", persisted)
	}

	// A fresh service (post-restart) restores the non-terminal task; the
	// restore is idempotent and reports for it are dropped.
	restored := NewDeleteService(
		fixture.tx,
		nil,
		NewConfirmStore(time.Minute, time.Now),
		logger,
	)
	restored.SetTaskStore(fixture.tx)
	if err := restored.Restore(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	if err := restored.Restore(fixture.ctx); err != nil {
		t.Fatalf("second Restore() error = %v", err)
	}
	status, ok := restored.Status(taskID)
	if !ok || status.Complete || status.OK != 1 || status.Pending != 1 ||
		status.Mode != "soft" {
		t.Fatalf("restored status = %#v found=%v", status, ok)
	}
	if status.ByMachine[fixture.machineA].RecycledTo[fixture.pathA] != `Z:\recycled\a` {
		t.Fatalf("restored RecycledTo = %#v", status.ByMachine[fixture.machineA].RecycledTo)
	}
	restored.HandleReport(fixture.machineB, &proto.DeleteReport{
		TaskID: taskID, Seq: 0, LastSeq: 0,
		Stats:   proto.DeleteStats{Total: 1, OK: 1},
		Entries: []proto.DeleteResult{{Path: fixture.pathB, OK: true}},
	})
	status, ok = restored.Status(taskID)
	if !ok || status.Pending != 1 || status.OK != 1 {
		t.Fatalf("restored status after late report = %#v found=%v", status, ok)
	}

	// The store-backed list surfaces the in-progress task with counts only.
	summaries := restored.ListTasks(fixture.ctx, 10)
	if len(summaries) == 0 || summaries[0].TaskID != taskID {
		t.Fatalf("ListTasks() = %#v", summaries)
	}
	if summaries[0].Complete || summaries[0].Pending != 1 ||
		summaries[0].OK != 1 || summaries[0].Total != 2 ||
		summaries[0].CreatedAt.IsZero() {
		t.Fatalf("summary = %#v", summaries[0])
	}
}
