package phase2

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"dedup/internal/config"
	"dedup/internal/features"
	"dedup/internal/proto"
)

func TestPostgres16BuildAndPersistFixtureWhenIntegrationEnabled(t *testing.T) {
	dsn := os.Getenv("DEDUP_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set DEDUP_TEST_PG_DSN to run PostgreSQL 16 integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var versionText string
	if err := pool.QueryRow(ctx, `SHOW server_version_num`).Scan(&versionText); err != nil {
		t.Fatal(err)
	}
	version, err := strconv.Atoi(versionText)
	if err != nil {
		t.Fatalf("parse server_version_num %q: %v", versionText, err)
	}
	if version < 160000 || version >= 170000 {
		t.Fatalf("server_version_num=%d, Task 7 fixture requires PostgreSQL 16", version)
	}

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	shaA := "aa" + suffix + strings.Repeat("1", 128-2-len(suffix))
	shaB := "bb" + suffix + strings.Repeat("2", 128-2-len(suffix))
	machineA := "phase2-pg-a-" + suffix
	machineB := "phase2-pg-b-" + suffix
	var fileA, fileB int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO files(
			machine_id,disk_no,path,size,mtime,sha512,
			phase1_done,phase2_done,status,missing_mask,updated_at
		) VALUES($1,1,$2,10,100,$3,1,0,'done',0,1)
		RETURNING id`,
		machineA, `D:\a.jpg`, shaA,
	).Scan(&fileA); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO files(
			machine_id,disk_no,path,size,mtime,sha512,
			phase1_done,phase2_done,status,missing_mask,updated_at
		) VALUES($1,1,$2,20,200,$3,1,0,'done',0,1)
		RETURNING id`,
		machineB, `D:\b.jpg`, shaB,
	).Scan(&fileB); err != nil {
		t.Fatal(err)
	}
	var groupID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO dup_groups(kind,representative_file_id,member_count)
		VALUES('image_candidate',$1,2)
		RETURNING id`,
		fileA,
	).Scan(&groupID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO dup_members(group_id,file_id,score_json)
		VALUES($1,$2,'{}'),($1,$3,'{}')`,
		groupID, fileA, fileB,
	); err != nil {
		t.Fatal(err)
	}
	pHash := features.EncodePHashParts([9]uint64{})
	sobel, err := features.EncodeSobelHist([128]float32{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO image_features(
			sha512,width,height,pdq256,pdq_quality,phash_parts,sobel_hist
		) VALUES
		  ($1,10,10,$3,80,$4,$5),
		  ($2,10,10,$3,80,$4,$5)`,
		shaA, shaB, make([]byte, 32), pHash, sobel,
	); err != nil {
		t.Fatal(err)
	}
	// Make A decoder-incomplete while B remains complete.
	if _, err := pool.Exec(
		ctx,
		`UPDATE image_features SET sobel_hist=$2 WHERE sha512=$1`,
		shaA,
		[]byte{1},
	); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(
			context.Background(),
			5*time.Second,
		)
		defer cleanupCancel()
		_, _ = pool.Exec(
			cleanupCtx,
			`DELETE FROM scan_tasks WHERE id LIKE 'phase2-%' AND machine_id=ANY($1::text[])`,
			[]string{machineA, machineB},
		)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM dup_members WHERE group_id=$1`, groupID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM dup_groups WHERE id=$1`, groupID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM image_features WHERE sha512=ANY($1::text[])`, []string{shaA, shaB})
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM files WHERE id=ANY($1::bigint[])`, []int64{fileA, fileB})
	})

	sender := &recordingSender{
		online: map[string]bool{machineA: true, machineB: true},
	}
	dispatcher := NewDispatcher(
		pool,
		sender,
		config.Phase2Config{TaskShardSize: 5000},
		nil,
	)
	tasks, err := dispatcher.BuildTasks(ctx, proto.KindImage)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].MachineID != machineA ||
		len(tasks[0].Task.Items) != 1 ||
		tasks[0].Task.Items[0].FieldsMask != proto.FieldSobelHist {
		t.Fatalf("built tasks = %#v", tasks)
	}
	if err := dispatcher.DispatchPending(ctx); err != nil {
		t.Fatal(err)
	}
	var targetType, status string
	if err := pool.QueryRow(ctx, `
		SELECT target->>'type',status
		FROM scan_tasks
		WHERE id=$1`,
		tasks[0].Task.TaskID,
	).Scan(&targetType, &status); err != nil {
		t.Fatal(err)
	}
	if targetType != phase2TargetType || status != taskStatusSent {
		t.Fatalf("persisted discriminator=%q status=%q", targetType, status)
	}
}

func TestPostgres16ScopedPendingEnvelopeRestoreWhenIntegrationEnabled(
	t *testing.T,
) {
	dsn := os.Getenv("DEDUP_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set DEDUP_TEST_PG_DSN to run PostgreSQL 16 integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := "task7_restore_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+quotedSchema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(
			context.Background(),
			5*time.Second,
		)
		defer cleanupCancel()
		_, _ = admin.Exec(cleanupCtx, `DROP SCHEMA `+quotedSchema+` CASCADE`)
	})

	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	scoped, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(scoped.Close)
	if _, err := scoped.Exec(ctx, `
		CREATE TABLE scan_tasks (
			id TEXT PRIMARY KEY,
			machine_id TEXT NOT NULL,
			phase INTEGER NOT NULL,
			target JSONB NOT NULL,
			status TEXT NOT NULL,
			stats_json JSONB NOT NULL DEFAULT '{}',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		t.Fatal(err)
	}

	sha := strings.Repeat("ef", 64)
	envelope := RoutedTask{
		MachineID: "scoped-machine",
		Task: proto.Phase2Task{Items: []proto.Phase2Item{{
			Path:       `D:\scoped.jpg`,
			FieldsMask: proto.FieldPHashParts | proto.FieldSobelHist,
			MachineID:  "scoped-machine",
			SHA512:     sha,
			Size:       123,
			MTimeMS:    456,
			Kind:       proto.KindImage,
		}}},
	}
	if err := finalizeTaskEnvelope(&envelope); err != nil {
		t.Fatal(err)
	}
	store := &postgresStore{pool: scoped}
	if _, err := store.persistPending(ctx, persistedTask{
		Envelope: envelope,
		Status:   taskStatusSent,
	}); err != nil {
		t.Fatal(err)
	}
	pendingStats := proto.TaskStats{
		Total: 4, Done: 2, Failed: 1, ElapsedMS: 90,
	}
	const pendingErr = "resume detail"
	durable, err := store.updateTask(
		ctx,
		envelope.Task.TaskID,
		envelope.MachineID,
		taskStatusRunning,
		pendingStats,
		pendingErr,
	)
	if err != nil {
		t.Fatal(err)
	}
	if durable.Status != taskStatusRunning ||
		durable.Stats != pendingStats ||
		durable.LastErr != pendingErr {
		t.Fatalf("initial durable state = %#v", durable)
	}

	sender := &recordingSender{
		online: map[string]bool{envelope.MachineID: true},
	}
	afterRestart := NewDispatcher(
		scoped,
		sender,
		config.Phase2Config{TaskShardSize: 5000},
		nil,
	)
	t.Cleanup(afterRestart.Shutdown)
	if err := afterRestart.RestorePending(ctx); err != nil {
		t.Fatal(err)
	}
	if err := afterRestart.DispatchMachinePending(
		ctx,
		envelope.MachineID,
	); err != nil {
		t.Fatal(err)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("restored sends = %#v, want exactly one", sender.sent)
	}
	gotEnvelope := sender.sent[0].task
	if gotEnvelope.TaskID != envelope.Task.TaskID {
		t.Fatalf(
			"restored task ID = %q, want %q",
			gotEnvelope.TaskID,
			envelope.Task.TaskID,
		)
	}
	wantWire, err := proto.EncodeFramePayload(
		proto.MsgPhase2Task,
		&envelope.Task,
	)
	if err != nil {
		t.Fatal(err)
	}
	gotWire, err := proto.EncodeFramePayload(
		proto.MsgPhase2Task,
		&gotEnvelope,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotWire, wantWire) {
		t.Fatal("restored Phase2 envelope is not byte-equivalent")
	}
	entry := afterRestart.ensureMemory().tasks[envelope.Task.TaskID]
	entry.mu.Lock()
	restoredState := entry.task
	entry.mu.Unlock()
	if restoredState.Status != taskStatusRunning ||
		restoredState.Stats != pendingStats ||
		restoredState.LastErr != pendingErr {
		t.Fatalf("restored durable state = %#v", restoredState)
	}
	var (
		targetType string
		status     string
		rawStats   []byte
	)
	if err := scoped.QueryRow(ctx, `
		SELECT target->>'type',status,stats_json
		FROM scan_tasks WHERE id=$1`,
		envelope.Task.TaskID,
	).Scan(&targetType, &status, &rawStats); err != nil {
		t.Fatal(err)
	}
	var statsDocument persistedStats
	if err := json.Unmarshal(rawStats, &statsDocument); err != nil {
		t.Fatal(err)
	}
	if targetType != phase2TargetType ||
		status != taskStatusRunning ||
		statsDocument.Stats != pendingStats ||
		statsDocument.LastErr != pendingErr {
		t.Fatalf(
			"stored target/state = type:%q status:%q stats:%#v",
			targetType,
			status,
			statsDocument,
		)
	}

	terminalStats := proto.TaskStats{
		Total: 4, Done: 3, Failed: 1, ElapsedMS: 120,
	}
	const terminalErr = "terminal detail"
	if _, err := store.updateTask(
		ctx,
		envelope.Task.TaskID,
		envelope.MachineID,
		taskStatusDone,
		terminalStats,
		terminalErr,
	); err != nil {
		t.Fatal(err)
	}
	staleResult, err := store.updateTask(
		ctx,
		envelope.Task.TaskID,
		envelope.MachineID,
		taskStatusRunning,
		proto.TaskStats{Total: 4, Done: 1},
		"stale replay",
	)
	if err != nil {
		t.Fatal(err)
	}
	if staleResult.Status != taskStatusDone ||
		staleResult.Stats != terminalStats ||
		staleResult.LastErr != terminalErr {
		t.Fatalf("PostgreSQL terminal state regressed: %#v", staleResult)
	}
}

func TestPostgresPendingTargetAuditRejectsUnknownDiscriminatorWhenIntegrationEnabled(
	t *testing.T,
) {
	dsn := os.Getenv("DEDUP_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set DEDUP_TEST_PG_DSN to run PostgreSQL 16 integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for name, test := range map[string]struct {
		target  string
		wantErr string
	}{
		"unknown string": {target: `{"type":"mystery"}`, wantErr: "mystery"},
		"explicit null":  {target: `{"type":null}`, wantErr: "<null>"},
	} {
		t.Run(name, func(t *testing.T) {
			taskID := "phase2-unknown-target-" + uuid.NewString()
			if _, err := pool.Exec(ctx, `
				INSERT INTO scan_tasks(id,machine_id,phase,target,status)
				VALUES($1,'phase2-audit-machine',1,$2::jsonb,'running')`,
				taskID,
				test.target,
			); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(
					context.Background(),
					5*time.Second,
				)
				defer cleanupCancel()
				_, _ = pool.Exec(
					cleanupCtx,
					`DELETE FROM scan_tasks WHERE id=$1`,
					taskID,
				)
			})
			dispatcher := NewDispatcher(
				pool,
				&recordingSender{online: map[string]bool{}},
				config.Phase2Config{TaskShardSize: 5000},
				nil,
			)
			err := dispatcher.RestorePending(ctx)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatal("RestorePending silently ignored an invalid target discriminator")
			}
		})
	}
}
