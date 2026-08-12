package store

import (
	"context"
	"database/sql"
	"errors"
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

func TestReplaceLocalAnalysisGroupsRejectsNonBuildingAndPreservesHistory(t *testing.T) {
	db := openLocalTestDB(t)
	ctx := context.Background()
	createAnalysisTask(t, db, "old-task", "machine-a")
	old := createLocalRunFixture(t, db, "old-history")
	oldFile := insertLocalFileFixture(t, db, "machine-a", "old", "old-sha")
	insertLocalGroupFixture(t, db, old, "old-group")
	if err := insertLocalMemberFixture(t, db, old, "old-group", "machine-a", oldFile, "old-sha"); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteLocalAnalysis(ctx, old.RunID); err != nil {
		t.Fatal(err)
	}

	err := db.ReplaceLocalAnalysisGroups(ctx, old.RunID, []LocalAnalysisGroup{{
		GroupID: "replacement", Category: "exact", RepresentativeFileID: oldFile,
		Members: []LocalAnalysisMember{{FileID: oldFile, SHA512: "old-sha"}},
	}})
	if err == nil {
		t.Fatal("non-building run accepted final groups")
	}
	var oldCount int
	if err := db.db.QueryRow(`SELECT count(*) FROM local_dup_groups WHERE run_id=? AND group_id='old-group'`, old.RunID).Scan(&oldCount); err != nil {
		t.Fatal(err)
	}
	if oldCount != 1 {
		t.Fatalf("historical group count = %d, want 1", oldCount)
	}
}

func TestReplaceLocalAnalysisGroupsRollsBackWholeReplacement(t *testing.T) {
	db := openLocalTestDB(t)
	run := createLocalRunFixture(t, db, "replace-rollback")
	left := insertLocalFileFixture(t, db, "machine-a", "left-final", "sha-left-final")
	right := insertLocalFileFixture(t, db, "machine-a", "right-final", "sha-right-final")
	insertLocalGroupFixture(t, db, run, "candidate-old")
	if err := insertLocalMemberFixture(t, db, run, "candidate-old", "machine-a", left, "sha-left-final"); err != nil {
		t.Fatal(err)
	}
	trigger := fmt.Sprintf(`CREATE TRIGGER fail_final_member BEFORE INSERT ON local_dup_members WHEN NEW.file_id=%d BEGIN SELECT RAISE(ABORT, 'injected final member failure'); END;`, right)
	if _, err := db.db.Exec(trigger); err != nil {
		t.Fatal(err)
	}

	err := db.ReplaceLocalAnalysisGroups(context.Background(), run.RunID, []LocalAnalysisGroup{{
		GroupID: "deterministic-final", Category: "image", RepresentativeFileID: left,
		Members: []LocalAnalysisMember{{FileID: left, SHA512: "sha-left-final"}, {FileID: right, SHA512: "sha-right-final"}},
	}})
	if err == nil {
		t.Fatal("injected member failure returned nil")
	}
	var oldCount, finalCount int
	if err := db.db.QueryRow(`SELECT count(*) FROM local_dup_groups WHERE run_id=? AND group_id='candidate-old'`, run.RunID).Scan(&oldCount); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT count(*) FROM local_dup_groups WHERE run_id=? AND group_id='deterministic-final'`, run.RunID).Scan(&finalCount); err != nil {
		t.Fatal(err)
	}
	if oldCount != 1 || finalCount != 0 {
		t.Fatalf("groups after rollback old/final = %d/%d", oldCount, finalCount)
	}
}

func TestReplaceLocalAnalysisGroupsRejectsForeignIdentityBeforeDelete(t *testing.T) {
	db := openLocalTestDB(t)
	run := createLocalRunFixture(t, db, "replace-identity")
	local := insertLocalFileFixture(t, db, "machine-a", "local", "sha-local")
	foreign := insertLocalFileFixture(t, db, "machine-b", "foreign", "sha-foreign")
	insertLocalGroupFixture(t, db, run, "candidate-keep")
	if err := insertLocalMemberFixture(t, db, run, "candidate-keep", "machine-a", local, "sha-local"); err != nil {
		t.Fatal(err)
	}
	err := db.ReplaceLocalAnalysisGroups(context.Background(), run.RunID, []LocalAnalysisGroup{{
		GroupID: "foreign-final", Category: "image", RepresentativeFileID: foreign,
		Members: []LocalAnalysisMember{{FileID: local, SHA512: "wrong-sha"}, {FileID: foreign, SHA512: "sha-foreign"}},
	}})
	if err == nil {
		t.Fatal("foreign or mismatched group identity was accepted")
	}
	var count int
	if err := db.db.QueryRow(`SELECT count(*) FROM local_dup_groups WHERE run_id=? AND group_id='candidate-keep'`, run.RunID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("candidate group count after rejected input = %d", count)
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

func TestLocalAnalysisRejectsPublishingOlderGeneration(t *testing.T) {
	db := openLocalTestDB(t)
	ctx := context.Background()
	createAnalysisTask(t, db, "task-1", "machine-a")
	createAnalysisTask(t, db, "task-2", "machine-a")
	first, err := db.BeginLocalAnalysis(ctx, "machine-a", "task-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.BeginLocalAnalysis(ctx, "machine-a", "task-2")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteLocalAnalysis(ctx, first.RunID); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteLocalAnalysis(ctx, second.RunID); err != nil {
		t.Fatal(err)
	}
	if err := db.PublishLocalAnalysis(ctx, second.RunID); err != nil {
		t.Fatalf("Publish newer generation: %v", err)
	}

	err = db.PublishLocalAnalysis(ctx, first.RunID)
	if !errors.Is(err, ErrStaleLocalAnalysisGeneration) {
		t.Fatalf("Publish older generation error = %v, want stable stale_generation", err)
	}
	current, err := db.CurrentLocalAnalysis(ctx, "machine-a")
	if err != nil {
		t.Fatal(err)
	}
	if current.RunID != second.RunID || current.Generation != second.Generation {
		t.Fatalf("current = %#v, want generation 2 %#v", current, second)
	}
	var firstStatus, secondStatus string
	if err := db.db.QueryRow(`SELECT status FROM local_analysis_runs WHERE run_id=?`, first.RunID).Scan(&firstStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT status FROM local_analysis_runs WHERE run_id=?`, second.RunID).Scan(&secondStatus); err != nil {
		t.Fatal(err)
	}
	if firstStatus != "complete" || secondStatus != "published" {
		t.Fatalf("statuses after stale publish = %q/%q, want complete/published", firstStatus, secondStatus)
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

func TestLocalSchemaRejectsCrossMachineOwnership(t *testing.T) {
	t.Run("task to run", func(t *testing.T) {
		db := openLocalTestDB(t)
		createAnalysisTask(t, db, "task-a", "machine-a")
		_, err := db.db.Exec(`
			INSERT INTO local_analysis_runs(run_id,machine_id,generation,task_id,status,created_at)
			VALUES ('cross-run','machine-b',1,'task-a','building',1)`)
		if err == nil {
			t.Fatal("run owned by a different machine than its task was accepted")
		}
	})

	t.Run("pair to file", func(t *testing.T) {
		db := openLocalTestDB(t)
		run := createLocalRunFixture(t, db, "pair")
		leftID := insertLocalFileFixture(t, db, "machine-a", "left", "sha-left")
		rightID := insertLocalFileFixture(t, db, "machine-b", "right", "sha-right")
		_, err := db.db.Exec(`
			INSERT INTO local_pair_scores
			(machine_id,run_id,generation,pair_key,left_file_id,right_file_id,left_sha512,right_sha512,stage1_json,created_at,updated_at)
			VALUES ('machine-a',?,?, 'cross',?,?,'sha-left','sha-right','{}',1,1)`,
			run.RunID, run.Generation, leftID, rightID)
		if err == nil {
			t.Fatal("pair referencing a file from another machine was accepted")
		}
	})

	t.Run("member to file", func(t *testing.T) {
		db := openLocalTestDB(t)
		run := createLocalRunFixture(t, db, "member")
		insertLocalGroupFixture(t, db, run, "group-member")
		fileID := insertLocalFileFixture(t, db, "machine-b", "member", "sha-member")
		err := insertLocalMemberFixture(t, db, run, "group-member", "machine-a", fileID, "sha-member")
		if err == nil {
			t.Fatal("group member referencing a file from another machine was accepted")
		}
	})

	t.Run("review to member", func(t *testing.T) {
		db := openLocalTestDB(t)
		run := createLocalRunFixture(t, db, "review")
		insertLocalGroupFixture(t, db, run, "group-review")
		fileID := insertLocalFileFixture(t, db, "machine-b", "review", "sha-review")
		if _, err := db.db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
			t.Fatal(err)
		}
		memberMachine := "machine-b"
		if err := insertLocalMemberFixture(t, db, run, "group-review", memberMachine, fileID, "sha-review"); err != nil {
			t.Fatalf("insert controlled invalid member with foreign keys disabled: %v", err)
		}
		if _, err := db.db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
			t.Fatal(err)
		}
		_, err := db.db.Exec(`
			INSERT INTO local_reviews
			(review_id,machine_id,run_id,generation,group_id,file_id,decision,reviewer,reviewed_at)
			VALUES ('cross-review','machine-a',?,?, 'group-review',?,'keep','tester',1)`,
			run.RunID, run.Generation, fileID)
		if err == nil {
			t.Fatal("review referencing a member from another machine was accepted")
		}
	})

	t.Run("delete item to file", func(t *testing.T) {
		db := openLocalTestDB(t)
		fileID := insertLocalFileFixture(t, db, "machine-b", "delete", "sha-delete")
		if _, err := db.db.Exec(`
			INSERT INTO local_delete_batches
			(batch_id,machine_id,confirmation_digest,status,requested_count,created_at,updated_at)
			VALUES ('batch-a','machine-a','digest','pending',1,1,1)`); err != nil {
			t.Fatal(err)
		}
		_, err := db.db.Exec(`
			INSERT INTO local_delete_items
			(batch_id,machine_id,file_id,path_snapshot,sha512,result,created_at,updated_at)
			VALUES ('batch-a','machine-a',?,'D:\\cross.jpg','sha-delete','pending',1,1)`, fileID)
		if err == nil {
			t.Fatal("delete item referencing a file from another machine was accepted")
		}
	})
}

func createLocalRunFixture(t *testing.T, db *DB, suffix string) LocalAnalysisRun {
	t.Helper()
	taskID := "task-" + suffix
	createAnalysisTask(t, db, taskID, "machine-a")
	run, err := db.BeginLocalAnalysis(context.Background(), "machine-a", taskID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func insertLocalFileFixture(t *testing.T, db *DB, machineID, suffix, sha string) int64 {
	t.Helper()
	result, err := db.db.Exec(`
		INSERT INTO files(machine_id,path,sha512,status)
		VALUES (?,? ,?,'done')`, machineID, `D:\\`+suffix+`.jpg`, sha)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func insertLocalGroupFixture(t *testing.T, db *DB, run LocalAnalysisRun, groupID string) {
	t.Helper()
	_, err := db.db.Exec(`
		INSERT INTO local_dup_groups(group_id,machine_id,run_id,generation,category,verdict,created_at)
		VALUES (?,'machine-a',?,?,'exact','duplicate',1)`, groupID, run.RunID, run.Generation)
	if err != nil {
		t.Fatal(err)
	}
}

func insertLocalMemberFixture(t *testing.T, db *DB, run LocalAnalysisRun, groupID, machineID string, fileID int64, sha string) error {
	t.Helper()
	_, err := db.db.Exec(`
		INSERT INTO local_dup_members(group_id,machine_id,run_id,generation,file_id,sha512,created_at)
		VALUES (?,?,?,?,?,?,1)`, groupID, machineID, run.RunID, run.Generation, fileID, sha)
	return err
}
