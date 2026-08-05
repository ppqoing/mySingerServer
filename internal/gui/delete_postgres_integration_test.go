package gui

import (
	"context"
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
