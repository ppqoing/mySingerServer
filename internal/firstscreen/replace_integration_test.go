package firstscreen

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestPGReplaceResultsSuccessIdempotencyAndM4Preservation(t *testing.T) {
	fixture := newTask4PGFixture(t, true)
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.GroupInsertBatch = 2
	cfg.SHAResolveChunk = 2
	state := &task5HookState{}
	store := &Store{
		conn: &task5HookConn{Conn: fixture.conn, state: state},
		cfg:  cfg,
	}

	exactSHA := fixture.sha("replace-exact", 0)
	imageA := fixture.sha("replace-image-a", 0)
	imageB := fixture.sha("replace-image-b", 0)
	videoA := fixture.sha("replace-video-a", 0)
	videoB := fixture.sha("replace-video-b", 0)
	missingA := fixture.sha("replace-missing-a", 0)
	missingB := fixture.sha("replace-missing-b", 0)

	exactFiles := task5InsertCopies(t, fixture, exactSHA, 3)
	imageAFiles := task5InsertCopies(t, fixture, imageA, 2)
	imageBFiles := task5InsertCopies(t, fixture, imageB, 2)
	videoAFiles := task5InsertCopies(t, fixture, videoA, 2)
	videoBFiles := task5InsertCopies(t, fixture, videoB, 1)
	task5InsertCopies(t, fixture, missingA, 1)
	sentinelFile := task5InsertCopies(t, fixture, fixture.sha("replace-m4", 0), 1)[0]

	task5SeedOldM3(t, fixture, sentinelFile.ID)
	task5SeedM4Sentinels(t, fixture, sentinelFile.ID)
	m4Before := task5SnapshotM4(t, fixture)

	exactSHABytes := task5MustSHA(t, exactSHA)
	// Deliberately unsorted: replacement owns the representative=min(file_id)
	// contract and must not rely on caller slice order.
	exact := []ExactGroup{{
		SHA512: exactSHABytes,
		Members: []FileRef{
			exactFiles[2],
			exactFiles[0],
			exactFiles[1],
		},
	}}
	imagePair := newCandidatePair(
		KindImageCandidate,
		task5MustSHA(t, imageA),
		task5MustSHA(t, imageB),
		17,
		0,
		82,
		76,
	)
	videoPair := newCandidatePair(
		KindVideoCandidate,
		task5MustSHA(t, videoA),
		task5MustSHA(t, videoB),
		9,
		380,
		70,
		66,
	)
	missingPair := newCandidatePair(
		KindImageCandidate,
		task5MustSHA(t, missingA),
		task5MustSHA(t, missingB),
		3,
		0,
		90,
		91,
	)
	pairs := []CandidatePair{imagePair, videoPair, missingPair}
	if imagePair.ShaA != task5MustSHA(t, imageA) {
		imageAFiles, imageBFiles = imageBFiles, imageAFiles
	}
	if videoPair.ShaA != task5MustSHA(t, videoA) {
		videoAFiles, videoBFiles = videoBFiles, videoAFiles
	}

	groups, members, skipped, err := store.ReplaceResults(ctx, exact, pairs)
	if err != nil {
		t.Fatalf("ReplaceResults: %v", err)
	}
	if groups != 3 || members != 10 || skipped != 1 {
		t.Fatalf("counts=(%d,%d,%d), want (3,10,1)", groups, members, skipped)
	}
	if state.resolveCalls != 3 || state.maxResolveChunk != 2 {
		t.Fatalf("resolve calls=%d max chunk=%d, want 3 calls with max chunk 2",
			state.resolveCalls, state.maxResolveChunk)
	}

	task5AssertGroup(
		t, fixture, KindExact, exactFiles[0].ID, exactFiles,
		func(_ FileRef, score task5Score) {
			if score.Basis != "sha512" {
				t.Fatalf("exact score basis=%q, want sha512", score.Basis)
			}
		},
	)
	task5AssertCandidateGroup(
		t, fixture, imagePair, imageAFiles, imageBFiles, 0,
	)
	task5AssertCandidateGroup(
		t, fixture, videoPair, videoAFiles, videoBFiles, 380,
	)
	if got := task5CountM3Groups(t, fixture); got != 3 {
		t.Fatalf("M3 group count=%d, want 3", got)
	}
	if m4After := task5SnapshotM4(t, fixture); !reflect.DeepEqual(m4After, m4Before) {
		t.Fatalf("M4 sentinels changed:\nbefore=%v\nafter=%v", m4Before, m4After)
	}

	semanticBefore := task5SnapshotM3Semantic(t, fixture)
	groups2, members2, skipped2, err := store.ReplaceResults(ctx, exact, pairs)
	if err != nil {
		t.Fatalf("ReplaceResults rerun: %v", err)
	}
	if groups2 != groups || members2 != members || skipped2 != skipped {
		t.Fatalf("rerun counts=(%d,%d,%d), want (%d,%d,%d)",
			groups2, members2, skipped2, groups, members, skipped)
	}
	if semanticAfter := task5SnapshotM3Semantic(t, fixture); !reflect.DeepEqual(semanticAfter, semanticBefore) {
		t.Fatalf("rerun semantic result changed:\nbefore=%v\nafter=%v",
			semanticBefore, semanticAfter)
	}
	if m4After := task5SnapshotM4(t, fixture); !reflect.DeepEqual(m4After, m4Before) {
		t.Fatalf("M4 sentinels changed after rerun:\nbefore=%v\nafter=%v", m4Before, m4After)
	}
}

func TestPGReplaceResultsRemoteFailuresRollbackAndKeepConnectionUsable(t *testing.T) {
	testCases := []struct {
		name         string
		boundary     string
		failure      error
		cancelBefore bool
	}{
		{name: "begin", boundary: "begin", failure: errors.New("forced begin failure")},
		{name: "delete members", boundary: "delete_members", failure: errors.New("forced member delete failure")},
		{name: "delete groups", boundary: "delete_groups", failure: errors.New("forced group delete failure")},
		{name: "resolve query", boundary: "resolve", failure: errors.New("forced resolve failure")},
		{name: "group returning row", boundary: "group_row", failure: errors.New("forced group row failure")},
		{name: "group batch close", boundary: "group_close", failure: errors.New("forced group close failure")},
		{name: "member CopyFrom", boundary: "copy", failure: errors.New("forced copy failure")},
		{name: "commit before call", boundary: "commit_pre_call", failure: errors.New("forced pre-call commit failure")},
		{name: "canceled after delete", boundary: "delete_groups", failure: context.Canceled, cancelBefore: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newTask4PGFixture(t, true)
			ctx := context.Background()
			var cancel context.CancelFunc
			if tc.cancelBefore {
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
			}

			shaA := fixture.sha("failure-a", 0)
			shaB := fixture.sha("failure-b", 0)
			filesA := task5InsertCopies(t, fixture, shaA, 1)
			task5InsertCopies(t, fixture, shaB, 1)
			task5SeedOldM3(t, fixture, filesA[0].ID)
			task5SeedM4Sentinels(t, fixture, filesA[0].ID)
			before := task5SnapshotAllResults(t, fixture)

			state := &task5HookState{
				boundary: tc.boundary,
				failure:  tc.failure,
				cancel:   cancel,
			}
			cfg := DefaultConfig()
			cfg.GroupInsertBatch = 1
			cfg.SHAResolveChunk = 1
			store := &Store{
				conn: &task5HookConn{Conn: fixture.conn, state: state},
				cfg:  cfg,
			}
			pair := newCandidatePair(
				KindImageCandidate,
				task5MustSHA(t, shaA),
				task5MustSHA(t, shaB),
				5,
				0,
				80,
				81,
			)
			groups, members, skipped, err := store.ReplaceResults(ctx, nil, []CandidatePair{pair})
			if !errors.Is(err, tc.failure) {
				t.Fatalf("ReplaceResults error=%v, want %v", err, tc.failure)
			}
			if groups != 0 || members != 0 || skipped != 0 {
				t.Fatalf("failure counts=(%d,%d,%d), want zeros", groups, members, skipped)
			}
			if !state.hit {
				t.Fatalf("failure boundary %q was not reached", tc.boundary)
			}
			if tc.boundary == "begin" {
				if state.rollbackCalled {
					t.Fatal("rollback called although transaction begin failed")
				}
			} else {
				if !state.rollbackCalled {
					t.Fatal("rollback was not called")
				}
				if !state.rollbackContextOK {
					t.Fatal("rollback context was canceled or had no finite deadline")
				}
			}
			after := task5SnapshotAllResults(t, fixture)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("results changed after %s failure:\nbefore=%v\nafter=%v",
					tc.name, before, after)
			}
			var one int
			if err := fixture.conn.QueryRow(context.Background(), `SELECT 1`).Scan(&one); err != nil || one != 1 {
				t.Fatalf("connection after %s failure: one=%d err=%v", tc.name, one, err)
			}
		})
	}
}

func TestPGReplaceResultsAbortedCommitIsDefiniteRollback(t *testing.T) {
	fixture := newTask4PGFixture(t, true)
	shaA := fixture.sha("aborted-commit-a", 0)
	shaB := fixture.sha("aborted-commit-b", 0)
	filesA := task5InsertCopies(t, fixture, shaA, 1)
	task5InsertCopies(t, fixture, shaB, 1)
	task5SeedOldM3(t, fixture, filesA[0].ID)
	task5SeedM4Sentinels(t, fixture, filesA[0].ID)
	before := task5SnapshotAllResults(t, fixture)

	state := &task5HookState{boundary: "commit_aborted"}
	store := &Store{
		conn: &task5HookConn{Conn: fixture.conn, state: state},
		cfg:  DefaultConfig(),
	}
	pair := newCandidatePair(
		KindImageCandidate,
		task5MustSHA(t, shaA),
		task5MustSHA(t, shaB),
		4,
		0,
		80,
		81,
	)
	_, _, _, err := store.ReplaceResults(
		context.Background(),
		nil,
		[]CandidatePair{pair},
	)
	if !errors.Is(err, pgx.ErrTxCommitRollback) {
		t.Fatalf("aborted Commit error=%v, want pgx.ErrTxCommitRollback", err)
	}
	if errors.Is(err, ErrCommitOutcomeUnknown) {
		t.Fatalf("aborted Commit incorrectly marked outcome unknown: %v", err)
	}
	if state.abortStatementErr == nil {
		t.Fatal("real PostgreSQL statement error was not produced before Commit")
	}
	if !state.rollbackCalled || !state.rollbackContextOK {
		t.Fatalf("rollback called=%v contextOK=%v",
			state.rollbackCalled, state.rollbackContextOK)
	}
	after := task5SnapshotAllResults(t, fixture)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("aborted Commit changed results:\nbefore=%v\nafter=%v", before, after)
	}
}

func TestPGReplaceResultsLostCommitAckIsUnknownAndRetryConverges(t *testing.T) {
	fixture := newTask4PGFixture(t, true)
	shaA := fixture.sha("lost-ack-a", 0)
	shaB := fixture.sha("lost-ack-b", 0)
	filesA := task5InsertCopies(t, fixture, shaA, 1)
	filesB := task5InsertCopies(t, fixture, shaB, 1)
	task5SeedOldM3(t, fixture, filesA[0].ID)
	task5SeedM4Sentinels(t, fixture, filesA[0].ID)
	m4Before := task5SnapshotM4(t, fixture)

	lostACK := errors.New("commit response lost")
	state := &task5HookState{
		boundary: "commit_lost_ack",
		failure:  lostACK,
	}
	store := &Store{
		conn: &task5HookConn{Conn: fixture.conn, state: state},
		cfg:  DefaultConfig(),
	}
	pair := newCandidatePair(
		KindImageCandidate,
		task5MustSHA(t, shaA),
		task5MustSHA(t, shaB),
		2,
		0,
		88,
		77,
	)
	if pair.ShaA != task5MustSHA(t, shaA) {
		filesA, filesB = filesB, filesA
	}
	groups, members, skipped, err := store.ReplaceResults(
		context.Background(),
		nil,
		[]CandidatePair{pair},
	)
	if !errors.Is(err, ErrCommitOutcomeUnknown) {
		t.Fatalf("lost-ACK error=%v, want ErrCommitOutcomeUnknown", err)
	}
	if !errors.Is(err, lostACK) {
		t.Fatalf("lost-ACK error=%v does not preserve original cause", err)
	}
	if groups != 0 || members != 0 || skipped != 0 {
		t.Fatalf("unknown outcome counts=(%d,%d,%d), want zeros",
			groups, members, skipped)
	}
	if !state.underlyingCommitSucceeded {
		t.Fatal("lost-ACK hook did not commit the real PostgreSQL transaction")
	}

	task5AssertCandidateGroup(t, fixture, pair, filesA, filesB, 0)
	committedSemantic := task5SnapshotM3Semantic(t, fixture)
	if got := task5CountM3Groups(t, fixture); got != 1 {
		t.Fatalf("M3 groups after lost ACK=%d, want committed group 1", got)
	}
	if m4After := task5SnapshotM4(t, fixture); !reflect.DeepEqual(m4After, m4Before) {
		t.Fatalf("M4 changed after lost ACK:\nbefore=%v\nafter=%v", m4Before, m4After)
	}

	groups, members, skipped, err = NewStore(
		fixture.conn,
		DefaultConfig(),
	).ReplaceResults(context.Background(), nil, []CandidatePair{pair})
	if err != nil {
		t.Fatalf("idempotent reconciliation retry: %v", err)
	}
	if groups != 1 || members != 2 || skipped != 0 {
		t.Fatalf("retry counts=(%d,%d,%d), want (1,2,0)",
			groups, members, skipped)
	}
	if retried := task5SnapshotM3Semantic(t, fixture); !reflect.DeepEqual(retried, committedSemantic) {
		t.Fatalf("retry did not converge:\ncommitted=%v\nretried=%v",
			committedSemantic, retried)
	}
	if m4After := task5SnapshotM4(t, fixture); !reflect.DeepEqual(m4After, m4Before) {
		t.Fatalf("M4 changed after reconciliation retry:\nbefore=%v\nafter=%v", m4Before, m4After)
	}
}

func TestPGReplaceResultsClosedConnectionDoesNotChangeResults(t *testing.T) {
	fixture := newTask4PGFixture(t, true)
	file := task5InsertCopies(t, fixture, fixture.sha("closed-connection", 0), 1)[0]
	task5SeedOldM3(t, fixture, file.ID)
	task5SeedM4Sentinels(t, fixture, file.ID)
	before := task5SnapshotAllResults(t, fixture)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	closed, err := pgx.Connect(ctx, os.Getenv("FS_PG_DSN"))
	if err != nil {
		t.Fatalf("connect disposable connection: %v", err)
	}
	if _, err := closed.Exec(ctx,
		`SET search_path TO `+pgx.Identifier{fixture.schema}.Sanitize(),
	); err != nil {
		_ = closed.Close(ctx)
		t.Fatalf("set disposable search_path: %v", err)
	}
	if err := closed.Close(ctx); err != nil {
		t.Fatalf("close disposable connection: %v", err)
	}

	_, _, _, err = NewStore(closed, DefaultConfig()).ReplaceResults(ctx, nil, nil)
	if err == nil {
		t.Fatal("ReplaceResults on closed connection returned nil")
	}
	after := task5SnapshotAllResults(t, fixture)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("closed connection changed results:\nbefore=%v\nafter=%v", before, after)
	}
}

func TestPGReplaceResultsConcurrentM4InsertAfterDeleteIsPreserved(t *testing.T) {
	fixture := newTask4PGFixture(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shaA := fixture.sha("concurrent-a", 0)
	shaB := fixture.sha("concurrent-b", 0)
	filesA := task5InsertCopies(t, fixture, shaA, 1)
	task5InsertCopies(t, fixture, shaB, 1)
	task5SeedOldM3(t, fixture, filesA[0].ID)

	deleted := make(chan struct{})
	resume := make(chan struct{})
	state := &task5HookState{
		boundary: "pause_after_delete_groups",
		deleted:  deleted,
		resume:   resume,
	}
	cfg := DefaultConfig()
	store := &Store{
		conn: &task5HookConn{Conn: fixture.conn, state: state},
		cfg:  cfg,
	}
	pair := newCandidatePair(
		KindImageCandidate,
		task5MustSHA(t, shaA),
		task5MustSHA(t, shaB),
		1,
		0,
		90,
		91,
	)
	result := make(chan error, 1)
	go func() {
		_, _, _, err := store.ReplaceResults(ctx, nil, []CandidatePair{pair})
		result <- err
	}()
	select {
	case <-deleted:
	case <-ctx.Done():
		t.Fatalf("wait for M3 delete: %v", ctx.Err())
	}

	second, err := pgx.Connect(ctx, os.Getenv("FS_PG_DSN"))
	if err != nil {
		close(resume)
		t.Fatalf("connect concurrent M4 writer: %v", err)
	}
	t.Cleanup(func() {
		_ = second.Close(context.Background())
	})
	quotedSchema := pgx.Identifier{fixture.schema}.Sanitize()
	if _, err := second.Exec(ctx, `SET search_path TO `+quotedSchema); err != nil {
		close(resume)
		t.Fatalf("set concurrent writer search_path: %v", err)
	}
	var m4GroupID int64
	if err := second.QueryRow(ctx, `
		INSERT INTO dup_groups(kind,representative_file_id,member_count,created_at)
		VALUES('image',$1,1,'2003-03-03 03:03:03+00')
		RETURNING id`,
		filesA[0].ID,
	).Scan(&m4GroupID); err != nil {
		close(resume)
		t.Fatalf("insert concurrent M4 group: %v", err)
	}
	if _, err := second.Exec(ctx, `
		INSERT INTO dup_members(group_id,file_id,score_json)
		VALUES($1,$2,'{"owner":"concurrent-m4"}'::jsonb)`,
		m4GroupID, filesA[0].ID,
	); err != nil {
		close(resume)
		t.Fatalf("insert concurrent M4 member: %v", err)
	}
	close(resume)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("ReplaceResults with concurrent M4 insert: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("wait for ReplaceResults: %v", ctx.Err())
	}

	var got string
	if err := fixture.conn.QueryRow(context.Background(), `
		SELECT m.score_json::text
		FROM dup_groups g
		JOIN dup_members m ON m.group_id=g.id
		WHERE g.id=$1 AND g.kind='image'`,
		m4GroupID,
	).Scan(&got); err != nil {
		t.Fatalf("read concurrent M4 sentinel: %v", err)
	}
	if got != `{"owner": "concurrent-m4"}` {
		t.Fatalf("concurrent M4 score=%q", got)
	}
}

type task5Score struct {
	Basis        string `json:"basis"`
	Hamming      int    `json:"hamming"`
	DurationDiff int64  `json:"duration_diff_ms"`
	QualitySelf  int    `json:"quality_self"`
	QualityPeer  int    `json:"quality_peer"`
	PeerSHA512   string `json:"peer_sha512"`
}

func task5InsertCopies(
	t *testing.T,
	fixture *task4PGFixture,
	sha string,
	count int,
) []FileRef {
	t.Helper()
	files := make([]FileRef, 0, count)
	for i := 0; i < count; i++ {
		file := FileRef{
			MachineID: fmt.Sprintf("t5-%s-m%d", fixture.token, i),
			DiskNo:    i % 2,
			Path:      fmt.Sprintf("/task5/%s/%s/%d", fixture.token, sha[:12], i),
			Size:      int64(1000 + i),
		}
		if err := fixture.conn.QueryRow(context.Background(), `
			INSERT INTO files(machine_id,disk_no,path,size,sha512)
			VALUES($1,$2,$3,$4,$5)
			RETURNING id`,
			file.MachineID, file.DiskNo, file.Path, file.Size, sha,
		).Scan(&file.ID); err != nil {
			t.Fatalf("insert file copy %d for %s: %v", i, sha, err)
		}
		files = append(files, file)
	}
	return files
}

func task5SeedOldM3(t *testing.T, fixture *task4PGFixture, fileID int64) {
	t.Helper()
	for i, kind := range M3Kinds {
		var groupID int64
		if err := fixture.conn.QueryRow(context.Background(), `
			INSERT INTO dup_groups(kind,representative_file_id,member_count,created_at)
			VALUES($1,$2,1,'2001-01-01 00:00:00+00')
			RETURNING id`,
			kind, fileID,
		).Scan(&groupID); err != nil {
			t.Fatalf("seed old %s group: %v", kind, err)
		}
		if _, err := fixture.conn.Exec(context.Background(), `
			INSERT INTO dup_members(group_id,file_id,score_json)
			VALUES($1,$2,jsonb_build_object('old',$3::int))`,
			groupID, fileID, i,
		); err != nil {
			t.Fatalf("seed old %s member: %v", kind, err)
		}
	}
}

func task5SeedM4Sentinels(t *testing.T, fixture *task4PGFixture, fileID int64) {
	t.Helper()
	for i, kind := range []string{"image", "video"} {
		var groupID int64
		if err := fixture.conn.QueryRow(context.Background(), `
			INSERT INTO dup_groups(kind,representative_file_id,member_count,created_at)
			VALUES($1,$2,1,$3::timestamptz)
			RETURNING id`,
			kind, fileID, fmt.Sprintf("2002-02-0%d 03:04:05+00", i+1),
		).Scan(&groupID); err != nil {
			t.Fatalf("seed M4 %s group: %v", kind, err)
		}
		if _, err := fixture.conn.Exec(context.Background(), `
			INSERT INTO dup_members(group_id,file_id,score_json)
			VALUES($1,$2,jsonb_build_object('owner','m4','sentinel',$3::int))`,
			groupID, fileID, i+10,
		); err != nil {
			t.Fatalf("seed M4 %s member: %v", kind, err)
		}
	}
}

func task5SnapshotM4(t *testing.T, fixture *task4PGFixture) []string {
	t.Helper()
	return task5SnapshotRows(t, fixture, `
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

func task5SnapshotM3Semantic(t *testing.T, fixture *task4PGFixture) []string {
	t.Helper()
	return task5SnapshotRows(t, fixture, `
		SELECT concat_ws('|',
			g.kind,
			g.representative_file_id::text,
			g.member_count::text,
			m.file_id::text,
			m.score_json::text)
		FROM dup_groups g
		JOIN dup_members m ON m.group_id=g.id
		WHERE g.kind = ANY($1)
		ORDER BY g.kind,g.representative_file_id,m.file_id,m.score_json::text`,
		M3Kinds)
}

func task5SnapshotAllResults(t *testing.T, fixture *task4PGFixture) []string {
	t.Helper()
	return task5SnapshotRows(t, fixture, `
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
		ORDER BY g.id,m.file_id`)
}

func task5SnapshotRows(
	t *testing.T,
	fixture *task4PGFixture,
	query string,
	args ...any,
) []string {
	t.Helper()
	rows, err := fixture.conn.Query(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("snapshot query: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var item string
		if err := rows.Scan(&item); err != nil {
			t.Fatalf("snapshot scan: %v", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("snapshot rows: %v", err)
	}
	return out
}

func task5CountM3Groups(t *testing.T, fixture *task4PGFixture) int {
	t.Helper()
	var count int
	if err := fixture.conn.QueryRow(context.Background(),
		`SELECT count(*) FROM dup_groups WHERE kind = ANY($1)`,
		M3Kinds,
	).Scan(&count); err != nil {
		t.Fatalf("count M3 groups: %v", err)
	}
	return count
}

func task5AssertGroup(
	t *testing.T,
	fixture *task4PGFixture,
	kind string,
	wantRepresentative int64,
	wantFiles []FileRef,
	checkScore func(FileRef, task5Score),
) {
	t.Helper()
	var groupID, representative int64
	var memberCount int
	if err := fixture.conn.QueryRow(context.Background(), `
		SELECT id,representative_file_id,member_count
		FROM dup_groups
		WHERE kind=$1`,
		kind,
	).Scan(&groupID, &representative, &memberCount); err != nil {
		t.Fatalf("read %s group: %v", kind, err)
	}
	if representative != wantRepresentative {
		t.Fatalf("%s representative=%d, want %d", kind, representative, wantRepresentative)
	}
	if memberCount != len(wantFiles) {
		t.Fatalf("%s member_count=%d, want %d", kind, memberCount, len(wantFiles))
	}

	wantIDs := make([]int64, len(wantFiles))
	filesByID := make(map[int64]FileRef, len(wantFiles))
	for i, file := range wantFiles {
		wantIDs[i] = file.ID
		filesByID[file.ID] = file
	}
	sort.Slice(wantIDs, func(i, j int) bool { return wantIDs[i] < wantIDs[j] })
	rows, err := fixture.conn.Query(context.Background(), `
		SELECT file_id,score_json::text
		FROM dup_members
		WHERE group_id=$1
		ORDER BY file_id`,
		groupID)
	if err != nil {
		t.Fatalf("read %s members: %v", kind, err)
	}
	defer rows.Close()
	var gotIDs []int64
	for rows.Next() {
		var (
			fileID int64
			raw    string
		)
		if err := rows.Scan(&fileID, &raw); err != nil {
			t.Fatalf("scan %s member: %v", kind, err)
		}
		var score task5Score
		if err := json.Unmarshal([]byte(raw), &score); err != nil {
			t.Fatalf("decode %s score %q: %v", kind, raw, err)
		}
		checkScore(filesByID[fileID], score)
		gotIDs = append(gotIDs, fileID)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %s member rows: %v", kind, err)
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("%s member IDs=%v, want %v", kind, gotIDs, wantIDs)
	}
}

func task5AssertCandidateGroup(
	t *testing.T,
	fixture *task4PGFixture,
	pair CandidatePair,
	sideA []FileRef,
	sideB []FileRef,
	wantDuration int64,
) {
	t.Helper()
	all := append(append([]FileRef(nil), sideA...), sideB...)
	sideAIDs := make(map[int64]bool, len(sideA))
	for _, file := range sideA {
		sideAIDs[file.ID] = true
	}
	wantPeerA := hex.EncodeToString(pair.ShaB[:])
	wantPeerB := hex.EncodeToString(pair.ShaA[:])
	task5AssertGroup(t, fixture, pair.Kind, sideA[0].ID, all,
		func(file FileRef, score task5Score) {
			wantSelf, wantPeer, wantPeerSHA := pair.QualityB, pair.QualityA, wantPeerB
			if sideAIDs[file.ID] {
				wantSelf, wantPeer, wantPeerSHA = pair.QualityA, pair.QualityB, wantPeerA
			}
			if score.Hamming != pair.Hamming ||
				score.DurationDiff != wantDuration ||
				score.QualitySelf != wantSelf ||
				score.QualityPeer != wantPeer ||
				score.PeerSHA512 != wantPeerSHA {
				t.Fatalf("%s file %d score=%+v, want h=%d dur=%d self=%d peer=%d peerSHA=%s",
					pair.Kind, file.ID, score, pair.Hamming, wantDuration,
					wantSelf, wantPeer, wantPeerSHA)
			}
		})
}

func task5MustSHA(t *testing.T, text string) [64]byte {
	t.Helper()
	sha, ok := shaFromText(text)
	if !ok {
		t.Fatalf("test SHA %q is not canonical", text)
	}
	return sha
}

type task5HookState struct {
	boundary                  string
	failure                   error
	cancel                    context.CancelFunc
	hit                       bool
	rollbackCalled            bool
	rollbackContextOK         bool
	deleted                   chan struct{}
	resume                    chan struct{}
	resolveCalls              int
	maxResolveChunk           int
	abortStatementErr         error
	underlyingCommitSucceeded bool
}

type task5HookConn struct {
	*pgx.Conn
	state *task5HookState
}

func (c *task5HookConn) BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	if c.state.boundary == "begin" {
		c.state.hit = true
		return nil, c.state.failure
	}
	tx, err := c.Conn.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return &task5HookTx{Tx: tx, state: c.state}, nil
}

type task5HookTx struct {
	pgx.Tx
	state *task5HookState
}

func (tx *task5HookTx) Exec(
	ctx context.Context,
	sql string,
	arguments ...any,
) (pgconn.CommandTag, error) {
	boundary := ""
	switch {
	case strings.Contains(sql, "DELETE FROM dup_members"):
		boundary = "delete_members"
	case strings.Contains(sql, "DELETE FROM dup_groups"):
		boundary = "delete_groups"
	}
	if tx.state.boundary == boundary {
		tx.state.hit = true
		if tx.state.cancel != nil {
			tx.state.cancel()
		}
		return pgconn.CommandTag{}, tx.state.failure
	}
	tag, err := tx.Tx.Exec(ctx, sql, arguments...)
	if err == nil && boundary == "delete_groups" &&
		tx.state.boundary == "pause_after_delete_groups" {
		tx.state.hit = true
		close(tx.state.deleted)
		select {
		case <-tx.state.resume:
		case <-ctx.Done():
			return pgconn.CommandTag{}, ctx.Err()
		}
	}
	return tag, err
}

func (tx *task5HookTx) Query(
	ctx context.Context,
	sql string,
	args ...any,
) (pgx.Rows, error) {
	if strings.Contains(sql, "sha512 = ANY") {
		tx.state.resolveCalls++
		if len(args) > 0 {
			if shas, ok := args[0].([]string); ok && len(shas) > tx.state.maxResolveChunk {
				tx.state.maxResolveChunk = len(shas)
			}
		}
		if tx.state.boundary == "resolve" {
			tx.state.hit = true
			return nil, tx.state.failure
		}
	}
	return tx.Tx.Query(ctx, sql, args...)
}

func (tx *task5HookTx) SendBatch(
	ctx context.Context,
	batch *pgx.Batch,
) pgx.BatchResults {
	results := tx.Tx.SendBatch(ctx, batch)
	return &task5HookBatchResults{
		BatchResults: results,
		state:        tx.state,
	}
}

func (tx *task5HookTx) CopyFrom(
	ctx context.Context,
	tableName pgx.Identifier,
	columnNames []string,
	rowSrc pgx.CopyFromSource,
) (int64, error) {
	if tx.state.boundary == "copy" {
		tx.state.hit = true
		return 0, tx.state.failure
	}
	return tx.Tx.CopyFrom(ctx, tableName, columnNames, rowSrc)
}

func (tx *task5HookTx) Commit(ctx context.Context) error {
	if tx.state.boundary == "commit_pre_call" {
		tx.state.hit = true
		return tx.state.failure
	}
	if tx.state.boundary == "commit_aborted" {
		tx.state.hit = true
		_, tx.state.abortStatementErr = tx.Tx.Exec(ctx, `SELECT 1/0`)
		return tx.Tx.Commit(ctx)
	}
	if tx.state.boundary == "commit_lost_ack" {
		tx.state.hit = true
		if err := tx.Tx.Commit(ctx); err != nil {
			return err
		}
		tx.state.underlyingCommitSucceeded = true
		return tx.state.failure
	}
	return tx.Tx.Commit(ctx)
}

func (tx *task5HookTx) Rollback(ctx context.Context) error {
	tx.state.rollbackCalled = true
	_, hasDeadline := ctx.Deadline()
	tx.state.rollbackContextOK = ctx.Err() == nil && hasDeadline
	return tx.Tx.Rollback(ctx)
}

type task5HookBatchResults struct {
	pgx.BatchResults
	state *task5HookState
}

func (results *task5HookBatchResults) QueryRow() pgx.Row {
	if results.state.boundary == "group_row" && !results.state.hit {
		results.state.hit = true
		return task5ErrorRow{err: results.state.failure}
	}
	return results.BatchResults.QueryRow()
}

func (results *task5HookBatchResults) Close() error {
	err := results.BatchResults.Close()
	if err != nil {
		return err
	}
	if results.state.boundary == "group_close" {
		results.state.hit = true
		return results.state.failure
	}
	return nil
}

type task5ErrorRow struct {
	err error
}

func (row task5ErrorRow) Scan(...any) error {
	return row.err
}
