package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
)

// Break caught: current queries leak deleted members, filters are applied
// after pagination, or offset ordering duplicates/omits groups.
func TestLocalGroupQueryFiltersAndPaginatesStableCurrentAndHistory(t *testing.T) {
	db := openLocalTestDB(t)
	run := seedLocalResultGroups(t, db)
	ctx := context.Background()

	current, err := db.ListLocalGroups(ctx, LocalGroupQuery{MachineID: "machine-a", Scope: "current", Category: "exact", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(current.Groups) != 1 || current.Groups[0].GroupID != "group-exact" ||
		len(current.Groups[0].Members) != 1 || current.Groups[0].Members[0].Status == "deleted" {
		t.Fatalf("current exact groups = %#v", current.Groups)
	}
	history, err := db.ListLocalGroups(ctx, LocalGroupQuery{MachineID: "machine-a", Scope: "history", RunID: run.RunID, Category: "exact", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Groups) != 1 || len(history.Groups[0].Members) != 2 || history.Groups[0].Members[1].Status != "deleted" {
		t.Fatalf("history exact groups = %#v", history.Groups)
	}

	filters := []struct {
		name  string
		query LocalGroupQuery
		want  string
	}{
		{"image", LocalGroupQuery{Category: "image"}, "group-image"},
		{"video", LocalGroupQuery{Category: "video"}, "group-video"},
		{"inconclusive", LocalGroupQuery{Category: "inconclusive"}, "group-uncertain"},
		{"path", LocalGroupQuery{PathContains: `video`}, "group-video"},
		{"filename", LocalGroupQuery{FileNameContains: "name-match"}, "group-image"},
		{"size", LocalGroupQuery{MinSize: int64Pointer(350), MaxSize: int64Pointer(450)}, "group-image"},
		{"review keep", LocalGroupQuery{ReviewStatus: "keep"}, "group-exact"},
	}
	for _, test := range filters {
		t.Run(test.name, func(t *testing.T) {
			test.query.MachineID = "machine-a"
			test.query.Scope = "current"
			test.query.Limit = 20
			page, err := db.ListLocalGroups(ctx, test.query)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Groups) != 1 || page.Groups[0].GroupID != test.want {
				t.Fatalf("groups = %#v, want %s", page.Groups, test.want)
			}
		})
	}

	var ids []string
	for offset := 0; ; offset++ {
		page, err := db.ListLocalGroups(ctx, LocalGroupQuery{MachineID: "machine-a", Scope: "current", Offset: offset, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Groups) == 0 {
			break
		}
		ids = append(ids, page.Groups[0].GroupID)
	}
	want := []string{"group-exact", "group-image", "group-uncertain", "group-video"}
	if len(ids) != len(want) {
		t.Fatalf("paged IDs = %v", ids)
	}
	for index := range want {
		if ids[index] != want[index] {
			t.Fatalf("paged IDs = %v, want %v", ids, want)
		}
	}

	filenameOnly, err := db.ListLocalGroups(ctx, LocalGroupQuery{
		MachineID: "machine-a", Scope: "current", FileNameContains: "directory-only", Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(filenameOnly.Groups) != 0 {
		t.Fatalf("filename filter matched a directory component: %#v", filenameOnly.Groups)
	}

	if _, err := db.db.Exec(`INSERT INTO local_reviews(review_id,machine_id,run_id,generation,group_id,file_id,decision,reviewer,reviewed_at) VALUES ('partial-review','machine-a',?,?,'group-image',3,'keep','seed',1)`, run.RunID, run.Generation); err != nil {
		t.Fatal(err)
	}
	reviewed, err := db.ListLocalGroups(ctx, LocalGroupQuery{
		MachineID: "machine-a", Scope: "current", ReviewStatus: "reviewed", Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(reviewed.Groups) != 1 || reviewed.Groups[0].GroupID != "group-exact" || reviewed.Groups[0].ReviewStatus != "reviewed" {
		t.Fatalf("reviewed filter admitted a partially reviewed group: %#v", reviewed.Groups)
	}
}

// Break caught: group details are implemented by scanning only the first
// maximum-sized list page and report existing groups after row 200 as missing.
func TestLoadLocalGroupDoesNotDependOnFirstListPage(t *testing.T) {
	db := openLocalTestDB(t)
	run := seedLocalResultGroups(t, db)
	for index := 0; index < MaxLocalPageSize+1; index++ {
		groupID := fmt.Sprintf("bulk-%03d", index)
		if _, err := db.db.Exec(`INSERT INTO local_dup_groups(group_id,machine_id,run_id,generation,category,verdict,created_at) VALUES (?,'machine-a',?,?,'image','uncertain',1)`, groupID, run.RunID, run.Generation); err != nil {
			t.Fatal(err)
		}
		if _, err := db.db.Exec(`INSERT INTO local_dup_members(group_id,machine_id,run_id,generation,file_id,sha512,created_at) VALUES (?,'machine-a',?,?,1,'sha-1',1)`, groupID, run.RunID, run.Generation); err != nil {
			t.Fatal(err)
		}
	}
	group, err := db.LoadLocalGroup(context.Background(), "machine-a", run.RunID, "bulk-200", false)
	if err != nil || group.GroupID != "bulk-200" || len(group.Members) != 1 {
		t.Fatalf("detail beyond first page = %#v, %v", group, err)
	}
}

// Break caught: production persists inconclusive comparisons only in
// local_pair_scores, so querying local_dup_groups alone hides them entirely.
func TestLocalGroupQuerySynthesizesStableInconclusivePairFromPersistedScore(t *testing.T) {
	db := openLocalTestDB(t)
	run := createLocalRunFixture(t, db, "inconclusive-results")
	ctx := context.Background()
	for _, file := range []struct {
		id   int64
		path string
		sha  string
	}{
		{901, `D:\Maybe\left.jpg`, "zz-left"},
		{902, `D:\Maybe\right.jpg`, "aa-right"},
	} {
		if _, err := db.db.Exec(`INSERT INTO files(id,machine_id,path,size,mtime,sha512,status) VALUES (?,'machine-a',?,100,1000,?,'done')`, file.id, file.path, file.sha); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SaveLocalPairScore(ctx, LocalPairScore{
		RunID: run.RunID, PairKey: "image:uncertain-pair",
		LeftFileID: 901, RightFileID: 902, LeftSHA512: "zz-left", RightSHA512: "aa-right",
		Stage1JSON: `{"kind":"image"}`, Verdict: "uncertain",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteLocalAnalysis(ctx, run.RunID); err != nil {
		t.Fatal(err)
	}
	if err := db.PublishLocalAnalysis(ctx, run.RunID); err != nil {
		t.Fatal(err)
	}

	page, err := db.ListLocalGroups(ctx, LocalGroupQuery{
		MachineID: "machine-a", Scope: "current", Category: "inconclusive", Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Groups) != 1 {
		t.Fatalf("inconclusive groups = %#v", page.Groups)
	}
	group := page.Groups[0]
	wantID := "inconclusive:" + run.RunID + ":image:uncertain-pair"
	if group.GroupID != wantID || group.Category != "inconclusive" || group.Verdict != "uncertain" ||
		group.ReviewStatus != "undecided" || len(group.Members) != 2 ||
		group.Members[0].FileID != 902 || group.Members[1].FileID != 901 {
		t.Fatalf("synthesized group = %#v", group)
	}
	detail, err := db.LoadLocalGroup(ctx, "machine-a", "", wantID, true)
	if err != nil || detail.GroupID != wantID || len(detail.Members) != 2 {
		t.Fatalf("current detail = %#v, %v", detail, err)
	}
	if err := db.CommitLocalReview(ctx, LocalReviewCommit{
		MachineID: "machine-a", RunID: run.RunID, GroupID: wantID, Reviewer: "user",
		Decisions: []LocalReviewChoice{{FileID: 902, Decision: "keep"}},
	}); err != nil {
		t.Fatalf("review synthesized group: %v", err)
	}
	if countRows(t, db, `SELECT count(*) FROM local_reviews WHERE group_id='`+wantID+`'`) != 2 {
		t.Fatal("synthesized review did not materialize both member decisions")
	}
	if _, err := db.db.Exec(`UPDATE files SET status='deleted' WHERE machine_id='machine-a' AND id=901`); err != nil {
		t.Fatal(err)
	}
	current, err := db.LoadLocalGroup(ctx, "machine-a", "", wantID, true)
	if err != nil || len(current.Members) != 1 || current.Members[0].FileID != 902 {
		t.Fatalf("current deleted filtering = %#v, %v", current, err)
	}
	history, err := db.LoadLocalGroup(ctx, "machine-a", run.RunID, wantID, false)
	if err != nil || len(history.Members) != 2 {
		t.Fatalf("history deleted retention = %#v, %v", history, err)
	}
}

// Break caught: partial review rows are committed without an explicit keep,
// an ineligible delete is accepted, or outbox failure leaves reviews behind.
func TestLocalReviewCommitValidatesGroupAndAtomicallyWritesOutbox(t *testing.T) {
	db := openLocalTestDB(t)
	run := seedLocalResultGroups(t, db)
	ctx := context.Background()

	if err := db.CommitLocalReview(ctx, LocalReviewCommit{
		MachineID: "machine-a", RunID: run.RunID, GroupID: "group-image", Reviewer: "user",
		Decisions: []LocalReviewChoice{{FileID: 3, Decision: "delete"}},
	}); err == nil {
		t.Fatal("review without keep and with ineligible delete was accepted")
	}
	if countRows(t, db, `SELECT count(*) FROM local_reviews WHERE group_id='group-image'`) != 0 ||
		countRows(t, db, `SELECT count(*) FROM local_outbox WHERE entity_key='group-image'`) != 0 {
		t.Fatal("rejected selection changed review/outbox state")
	}

	if err := db.CommitLocalReview(ctx, LocalReviewCommit{
		MachineID: "machine-a", RunID: run.RunID, GroupID: "group-exact", Reviewer: "user", Note: "chosen",
		Decisions: []LocalReviewChoice{{FileID: 1, Decision: "keep"}},
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := db.db.Query(`SELECT file_id,decision FROM local_reviews WHERE group_id='group-exact' ORDER BY file_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var decisions []string
	for rows.Next() {
		var fileID int64
		var decision string
		if err := rows.Scan(&fileID, &decision); err != nil {
			t.Fatal(err)
		}
		decisions = append(decisions, decision)
	}
	if len(decisions) != 2 || decisions[0] != "keep" || decisions[1] != "undecided" {
		t.Fatalf("stored decisions = %v", decisions)
	}
	var payload string
	if err := db.db.QueryRow(`SELECT payload_json FROM local_outbox WHERE topic='local.review' AND entity_key='group-exact'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !json.Valid([]byte(payload)) {
		t.Fatalf("invalid outbox payload %q", payload)
	}

	if _, err := db.db.Exec(`CREATE TRIGGER fail_review_outbox BEFORE INSERT ON local_outbox WHEN NEW.entity_key='group-uncertain' BEGIN SELECT RAISE(ABORT,'outbox blocked'); END;`); err != nil {
		t.Fatal(err)
	}
	if err := db.CommitLocalReview(ctx, LocalReviewCommit{
		MachineID: "machine-a", RunID: run.RunID, GroupID: "group-uncertain", Reviewer: "user",
		Decisions: []LocalReviewChoice{{FileID: 7, Decision: "keep"}},
	}); err == nil {
		t.Fatal("outbox trigger failure was hidden")
	}
	if countRows(t, db, `SELECT count(*) FROM local_reviews WHERE group_id='group-uncertain'`) != 0 {
		t.Fatal("review rows survived outbox rollback")
	}
}

// Break caught: preview source lookup accepts a foreign machine, deleted file,
// or video row and lets the Agent construct a path-bearing worker job.
func TestLocalPreviewSourceLookupReturnsMachineOwnedIdentity(t *testing.T) {
	db := openLocalTestDB(t)
	seedLocalResultGroups(t, db)
	ctx := context.Background()
	source, err := db.LoadLocalPreviewSource(ctx, "machine-a", 1)
	if err != nil {
		t.Fatal(err)
	}
	if source.FileID != 1 || source.Kind != "image" || source.Status != "done" || source.Path == "" || source.SHA512 == "" || source.Size != 100 {
		t.Fatalf("preview source = %#v", source)
	}
	for _, input := range []struct {
		machine string
		fileID  int64
	}{{"machine-b", 1}, {"machine-a", 2}, {"machine-a", 5}} {
		if _, err := db.LoadLocalPreviewSource(ctx, input.machine, input.fileID); err == nil {
			t.Fatalf("preview source accepted machine=%s file=%d", input.machine, input.fileID)
		}
	}
}

func seedLocalResultGroups(t *testing.T, db *DB) LocalAnalysisRun {
	t.Helper()
	run := createLocalRunFixture(t, db, "results")
	files := []struct {
		id     int
		path   string
		size   int
		status string
		sha    string
		kind   string
	}{
		{1, `D:\Albums\match-one.jpg`, 100, "done", "sha-1", "image"},
		{2, `D:\Albums\deleted.jpg`, 200, "deleted", "sha-2", "image"},
		{3, `D:\Pictures\ordinary.jpg`, 300, "done", "sha-3", "image"},
		{4, `D:\Other\name-match.jpg`, 400, "done", "sha-4", "image"},
		{5, `D:\directory-only\clip.mp4`, 500, "done", "sha-5", "video"},
		{6, `D:\Video\copy.mp4`, 500, "done", "sha-6", "video"},
		{7, `D:\Maybe\left.jpg`, 700, "done", "sha-7", "image"},
		{8, `D:\Maybe\right.jpg`, 800, "done", "sha-8", "image"},
	}
	for _, file := range files {
		if _, err := db.db.Exec(`INSERT INTO files(id,machine_id,path,size,mtime,sha512,status) VALUES (?,'machine-a',?,?,1000,?,?)`, file.id, file.path, file.size, file.sha, file.status); err != nil {
			t.Fatal(err)
		}
		if file.kind == "image" {
			if _, err := db.db.Exec(`INSERT OR IGNORE INTO image_features(sha512,width,height) VALUES (?,10,10)`, file.sha); err != nil {
				t.Fatal(err)
			}
		} else if _, err := db.db.Exec(`INSERT OR IGNORE INTO video_features(sha512,thumb_path,thumb_width,thumb_height) VALUES (?,'D:\cache\sheet.jpg',100,100)`, file.sha); err != nil {
			t.Fatal(err)
		}
	}
	groups := []struct {
		id       string
		category string
		verdict  string
		members  [2]int
	}{
		{"group-exact", "exact", "duplicate", [2]int{1, 2}},
		{"group-image", "image", "uncertain", [2]int{3, 4}},
		{"group-video", "video", "duplicate", [2]int{5, 6}},
		{"group-uncertain", "uncertain", "uncertain", [2]int{7, 8}},
	}
	for _, group := range groups {
		if _, err := db.db.Exec(`INSERT INTO local_dup_groups(group_id,machine_id,run_id,generation,category,verdict,created_at) VALUES (?,'machine-a',?,?,?,?,1)`, group.id, run.RunID, run.Generation, group.category, group.verdict); err != nil {
			t.Fatal(err)
		}
		for _, fileID := range group.members {
			if _, err := db.db.Exec(`INSERT INTO local_dup_members(group_id,machine_id,run_id,generation,file_id,sha512,created_at) SELECT ?,'machine-a',?,?,id,sha512,1 FROM files WHERE id=?`, group.id, run.RunID, run.Generation, fileID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := db.db.Exec(`INSERT INTO local_reviews(review_id,machine_id,run_id,generation,group_id,file_id,decision,reviewer,reviewed_at) VALUES ('seed-review','machine-a',?,?, 'group-exact',1,'keep','seed',1)`, run.RunID, run.Generation); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteLocalAnalysis(context.Background(), run.RunID); err != nil {
		t.Fatal(err)
	}
	if err := db.PublishLocalAnalysis(context.Background(), run.RunID); err != nil {
		t.Fatal(err)
	}
	return run
}

func int64Pointer(value int64) *int64 { return &value }

func countRows(t *testing.T, db *DB, query string) int {
	t.Helper()
	var count int
	if err := db.db.QueryRow(query).Scan(&count); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	return count
}
