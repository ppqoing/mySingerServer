package store

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func createAnalysisTask(t *testing.T, db *DB, taskID, machineID string) {
	t.Helper()
	_, err := db.CreateOrLoadLocalTask(context.Background(), LocalTaskCreate{
		TaskID: taskID, MachineID: machineID, Source: "local",
		Type: "analysis", Stage: 1, EnvelopeDigest: "digest-" + taskID,
	})
	if err != nil {
		t.Fatalf("CreateOrLoadLocalTask: %v", err)
	}
}

func TestLocalAnalysisGenerationIsMonotonicAndPublishRollsBackCurrentOnFailure(t *testing.T) {
	db := openLocalTestDB(t)
	ctx := context.Background()
	createAnalysisTask(t, db, "task-1", "machine-a")
	createAnalysisTask(t, db, "task-2", "machine-a")

	first, err := db.BeginLocalAnalysis(ctx, "machine-a", "task-1")
	if err != nil {
		t.Fatalf("Begin first: %v", err)
	}
	if first.Generation != 1 {
		t.Fatalf("first generation = %d, want 1", first.Generation)
	}
	if err := db.CompleteLocalAnalysis(ctx, first.RunID); err != nil {
		t.Fatalf("Complete first: %v", err)
	}
	if err := db.PublishLocalAnalysis(ctx, first.RunID); err != nil {
		t.Fatalf("Publish first: %v", err)
	}

	second, err := db.BeginLocalAnalysis(ctx, "machine-a", "task-2")
	if err != nil {
		t.Fatalf("Begin second: %v", err)
	}
	if second.Generation != 2 {
		t.Fatalf("second generation = %d, want 2", second.Generation)
	}
	if err := db.CompleteLocalAnalysis(ctx, second.RunID); err != nil {
		t.Fatalf("Complete second: %v", err)
	}
	if _, err := db.db.Exec(`
		CREATE TRIGGER fail_local_current_update
		BEFORE UPDATE ON local_current_analysis
		BEGIN SELECT RAISE(ABORT, 'injected publish failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if err := db.PublishLocalAnalysis(ctx, second.RunID); err == nil {
		t.Fatal("Publish second error = nil")
	}

	current, err := db.CurrentLocalAnalysis(ctx, "machine-a")
	if err != nil {
		t.Fatalf("CurrentLocalAnalysis: %v", err)
	}
	if current.RunID != first.RunID || current.Generation != first.Generation {
		t.Fatalf("current after failed publish = %#v, want first run", current)
	}
	var status string
	if err := db.db.QueryRow(`SELECT status FROM local_analysis_runs WHERE run_id=?`, second.RunID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "complete" {
		t.Fatalf("second status after rollback = %q, want complete", status)
	}
}

func TestLocalAnalysisPublishesOnlyCompleteRun(t *testing.T) {
	db := openLocalTestDB(t)
	createAnalysisTask(t, db, "task", "machine-a")
	run, err := db.BeginLocalAnalysis(context.Background(), "machine-a", "task")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PublishLocalAnalysis(context.Background(), run.RunID); err == nil {
		t.Fatal("building run was published")
	}
	if _, err := db.CurrentLocalAnalysis(context.Background(), "machine-a"); err != sql.ErrNoRows {
		t.Fatalf("current error = %v, want sql.ErrNoRows", err)
	}
}

func TestLocalAnalysisCurrentPairsAreMachineScopedStableAndCapped(t *testing.T) {
	db := openLocalTestDB(t)
	ctx := context.Background()
	createAnalysisTask(t, db, "task-a", "machine-a")
	createAnalysisTask(t, db, "task-b", "machine-b")
	runA, err := db.BeginLocalAnalysis(ctx, "machine-a", "task-a")
	if err != nil {
		t.Fatal(err)
	}
	runB, err := db.BeginLocalAnalysis(ctx, "machine-b", "task-b")
	if err != nil {
		t.Fatal(err)
	}

	for machineIndex, entry := range []struct {
		machine string
		run     LocalAnalysisRun
		pairs   int
	}{{"machine-a", runA, 205}, {"machine-b", runB, 1}} {
		for i := 0; i < entry.pairs*2; i++ {
			path := fmt.Sprintf(`D:\\%s-%03d.jpg`, entry.machine, i)
			if _, err := db.db.Exec(`
				INSERT INTO files(machine_id,path,sha512,status) VALUES (?,?,?,'done')`,
				entry.machine, path, fmt.Sprintf("%s-sha-%03d", entry.machine, i)); err != nil {
				t.Fatal(err)
			}
		}
		rows, err := db.db.Query(`SELECT id,sha512 FROM files WHERE machine_id=? ORDER BY id`, entry.machine)
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		var shas []string
		for rows.Next() {
			var id int64
			var sha string
			if err := rows.Scan(&id, &sha); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			ids = append(ids, id)
			shas = append(shas, sha)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < entry.pairs; i++ {
			pairKey := fmt.Sprintf("pair-%03d", entry.pairs-i-1)
			if err := db.SaveLocalPairScore(ctx, LocalPairScore{
				RunID: entry.run.RunID, PairKey: pairKey,
				LeftFileID: ids[2*i], RightFileID: ids[2*i+1],
				LeftSHA512: shas[2*i], RightSHA512: shas[2*i+1],
				Stage1JSON: `{"exact":false}`, Verdict: "undecided",
			}); err != nil {
				t.Fatalf("SaveLocalPairScore(%d,%d): %v", machineIndex, i, err)
			}
		}
		if err := db.CompleteLocalAnalysis(ctx, entry.run.RunID); err != nil {
			t.Fatal(err)
		}
		if err := db.PublishLocalAnalysis(ctx, entry.run.RunID); err != nil {
			t.Fatal(err)
		}
	}

	page, err := db.ListCurrentLocalPairScores(ctx, "machine-a", 0, 9999)
	if err != nil {
		t.Fatalf("ListCurrentLocalPairScores: %v", err)
	}
	if len(page) != MaxLocalPageSize {
		t.Fatalf("page length = %d, want cap %d", len(page), MaxLocalPageSize)
	}
	for i, pair := range page {
		want := fmt.Sprintf("pair-%03d", i)
		if pair.PairKey != want || pair.MachineID != "machine-a" {
			t.Fatalf("pair[%d] = key:%q machine:%q, want %q/machine-a", i, pair.PairKey, pair.MachineID, want)
		}
	}
	bPage, err := db.ListCurrentLocalPairScores(ctx, "machine-b", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(bPage) != 1 || bPage[0].MachineID != "machine-b" || bPage[0].RunID != runB.RunID {
		t.Fatalf("machine-b page = %#v", bPage)
	}
}
