package phase2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestRebuildGroupsRejectsInvalidKindBeforeBegin(t *testing.T) {
	db := &groupTestDB{}
	rebuilder := NewGroupRebuilder(db)

	stats, err := rebuilder.RebuildGroups(context.Background(), "exact")
	if err == nil || !strings.Contains(err.Error(), "invalid confirmed group kind") {
		t.Fatalf("RebuildGroups error=%v, want invalid kind", err)
	}
	if stats != (GroupStats{}) {
		t.Fatalf("invalid-kind stats=%+v, want zero", stats)
	}
	if db.begins != 0 {
		t.Fatalf("invalid kind opened %d transactions, want zero", db.begins)
	}
}

func TestRebuildGroupsTransitiveCopiesRepresentativeAndDirectViaDetail(t *testing.T) {
	a, b, c := groupTestSHA('a'), groupTestSHA('b'), groupTestSHA('c')
	noLeft, noRight := groupTestSHA('d'), groupTestSHA('e')
	inconclusiveLeft, inconclusiveRight := groupTestSHA('1'), groupTestSHA('f')
	lostLeft, lostRight := groupTestSHA('2'), groupTestSHA('3')
	tx := &groupTestTx{
		scoreRows: groupScoreRows(
			groupTestScore("image", a, b, "yes", .91),
			groupTestScore("image", b, c, "yes", .97),
			groupTestScore("image", noLeft, noRight, "no", .2),
			groupTestScore(
				"image",
				inconclusiveLeft,
				inconclusiveRight,
				"inconclusive",
				0,
			),
			groupTestScore("image", lostLeft, lostRight, "yes", .99),
		),
		fileRows: groupFileRows(
			groupTestFile{1, a, "z-machine", `z:\copy.jpg`, 70},
			groupTestFile{2, a, "a-machine", `a:\representative.jpg`, 99},
			groupTestFile{3, b, "b-machine", `b:\member.jpg`, 90},
			groupTestFile{4, c, "c-machine", `c:\copy-1.jpg`, 80},
			groupTestFile{5, c, "d-machine", `d:\copy-2.jpg`, 80},
			groupTestFile{6, lostLeft, "lost", `l:\only-one-sha.jpg`, 100},
		),
		nextGroupID: 41,
	}
	rebuilder := NewGroupRebuilder(&groupTestDB{tx: tx})

	stats, err := rebuilder.RebuildGroups(context.Background(), "image")
	if err != nil {
		t.Fatalf("RebuildGroups: %v", err)
	}
	if stats != (GroupStats{Groups: 1, Members: 5}) {
		t.Fatalf("stats=%+v", stats)
	}
	if !tx.committed || tx.rollbackCalls != 0 {
		t.Fatalf("commit=%v rollbackCalls=%d", tx.committed, tx.rollbackCalls)
	}
	if tx.options.IsoLevel != pgx.RepeatableRead {
		t.Fatalf("isolation=%v, want repeatable read", tx.options.IsoLevel)
	}
	if tx.deleteKind != "image" {
		t.Fatalf("deleted kind=%q, want image", tx.deleteKind)
	}
	if len(tx.groups) != 1 {
		t.Fatalf("inserted groups=%v", tx.groups)
	}
	if got := tx.groups[0]; got.representative != 2 || got.members != 5 {
		t.Fatalf("group=%+v, want representative=2 members=5", got)
	}

	members := tx.memberDocuments(t)
	if got := members[2]; !reflect.DeepEqual(got, map[string]any{
		"role": "representative",
	}) {
		t.Fatalf("representative score=%v", got)
	}
	assertGroupMemberScore(t, members[1], a, false, "", "", nil)
	directAB := groupTestScore("image", a, b, "yes", .91).document
	assertGroupMemberScore(t, members[3], a, false, a, b, directAB)
	viaBC := groupTestScore("image", b, c, "yes", .97).document
	assertGroupMemberScore(t, members[4], a, true, b, c, viaBC)
	assertGroupMemberScore(t, members[5], a, true, b, c, viaBC)
}

func TestRebuildGroupsRepresentativeUsesQualityThenMachinePathThenFileID(t *testing.T) {
	a, b := groupTestSHA('2'), groupTestSHA('3')
	tests := []struct {
		name  string
		files []groupTestFile
		want  int64
	}{
		{
			name: "highest quality",
			files: []groupTestFile{
				{1, a, "a", "a", 10}, {2, b, "z", "z", 11},
			},
			want: 2,
		},
		{
			name: "machine before path",
			files: []groupTestFile{
				{1, a, "b", "a", 10}, {2, b, "a", "z", 10},
			},
			want: 2,
		},
		{
			name: "path after machine",
			files: []groupTestFile{
				{1, a, "a", "z", 10}, {2, b, "a", "a", 10},
			},
			want: 2,
		},
		{
			name: "file id final tie",
			files: []groupTestFile{
				{9, a, "a", "a", 10}, {2, b, "a", "a", 10},
			},
			want: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tx := &groupTestTx{
				scoreRows:   groupScoreRows(groupTestScore("image", a, b, "yes", .9)),
				fileRows:    groupFileRows(tc.files...),
				nextGroupID: 1,
			}
			stats, err := NewGroupRebuilder(&groupTestDB{tx: tx}).
				RebuildGroups(context.Background(), "image")
			if err != nil {
				t.Fatal(err)
			}
			if stats != (GroupStats{Groups: 1, Members: 2}) {
				t.Fatalf("stats=%+v", stats)
			}
			if got := tx.groups[0].representative; got != tc.want {
				t.Fatalf("representative=%d, want %d", got, tc.want)
			}
		})
	}
}

func TestRebuildGroupsVideoIndirectUsesHighestSimilarityThenPairKey(t *testing.T) {
	a, b, c, d := groupTestSHA('4'), groupTestSHA('5'), groupTestSHA('6'), groupTestSHA('7')
	edgeAB := groupTestScore("video", a, b, "yes", .90)
	edgeBC := groupTestScore("video", b, c, "yes", .95)
	edgeCD := groupTestScore("video", c, d, "yes", .95)
	tx := &groupTestTx{
		scoreRows: groupScoreRows(edgeCD, edgeBC, edgeAB),
		fileRows: groupFileRows(
			groupTestFile{1, a, "a", "a", 100},
			groupTestFile{2, b, "b", "b", 1},
			groupTestFile{3, c, "c", "c", 1},
			groupTestFile{4, d, "d", "d", 1},
		),
		nextGroupID: 1,
	}
	_, err := NewGroupRebuilder(&groupTestDB{tx: tx}).
		RebuildGroups(context.Background(), "video")
	if err != nil {
		t.Fatal(err)
	}
	members := tx.memberDocuments(t)
	assertGroupMemberScore(t, members[3], a, true, b, c, edgeBC.document)
	assertGroupMemberScore(t, members[4], a, true, c, d, edgeCD.document)
}

func TestRebuildGroupsStrictlyAuditsEveryRequestedKindScore(t *testing.T) {
	a, b := groupTestSHA('8'), groupTestSHA('9')
	validImage := groupTestScore("image", a, b, "yes", .9)
	cases := []struct {
		name string
		row  groupTestScoreRow
	}{
		{"reversed", groupTestScore("image", b, a, "yes", .9)},
		{"noncanonical", groupTestScore("image", "A"+a[1:], b, "yes", .9)},
		{"kind mismatch", func() groupTestScoreRow {
			row := validImage
			var doc map[string]any
			_ = json.Unmarshal(row.document, &doc)
			doc["kind"] = "video"
			row.document, _ = json.Marshal(doc)
			return row
		}()},
		{"verdict mismatch", func() groupTestScoreRow {
			row := validImage
			var doc map[string]any
			_ = json.Unmarshal(row.document, &doc)
			doc["verdict"] = "no"
			row.document, _ = json.Marshal(doc)
			return row
		}()},
		{"null document", func() groupTestScoreRow {
			row := validImage
			row.document = nil
			return row
		}()},
		{"malformed document", func() groupTestScoreRow {
			row := validImage
			row.document = []byte(`{"version":1}`)
			return row
		}()},
		{"yes without final similarity", func() groupTestScoreRow {
			row := validImage
			var doc map[string]any
			_ = json.Unmarshal(row.document, &doc)
			image := doc["image"].(map[string]any)
			image["sobel_evaluated"] = false
			delete(image, "sobel_cosine")
			row.document, _ = json.Marshal(doc)
			return row
		}()},
		{"yes non-finite final similarity", func() groupTestScoreRow {
			row := validImage
			row.overrideDocument = &pairScoreDocument{
				Version: 1, Kind: "image", Verdict: "yes",
				Image: &imageScoreDocument{
					PHashEvaluated: true,
					PHashPassRatio: groupFloat(1),
					SobelEvaluated: true,
					SobelCosine:    groupFloat(math.NaN()),
				},
			}
			return row
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx := &groupTestTx{scoreRows: groupScoreRows(tc.row)}
			stats, err := NewGroupRebuilder(&groupTestDB{tx: tx}).
				RebuildGroups(context.Background(), "image")
			if err == nil {
				t.Fatal("RebuildGroups succeeded for malformed requested-kind row")
			}
			if stats != (GroupStats{}) {
				t.Fatalf("stats=%+v, want zero", stats)
			}
			if tx.deleteKind != "" || tx.committed {
				t.Fatalf("malformed audit mutated transaction: delete=%q commit=%v",
					tx.deleteKind, tx.committed)
			}
			if tx.rollbackCalls != 1 {
				t.Fatalf("rollback calls=%d, want 1", tx.rollbackCalls)
			}
		})
	}
}

func TestRebuildGroupsRollsBackDefiniteFailuresAndReportsNoWrites(t *testing.T) {
	a, b := groupTestSHA('a'), groupTestSHA('b')
	stages := []string{
		"begin", "lock", "score_query", "score_scan", "file_query", "file_scan",
		"delete", "group_insert", "member_insert", "commit_rollback",
	}
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			forced := errors.New("forced " + stage)
			tx := &groupTestTx{
				scoreRows:   groupScoreRows(groupTestScore("image", a, b, "yes", .9)),
				fileRows:    groupFileRows(groupTestFile{1, a, "a", "a", 1}, groupTestFile{2, b, "b", "b", 1}),
				failStage:   stage,
				failure:     forced,
				nextGroupID: 1,
			}
			db := &groupTestDB{tx: tx}
			if stage == "begin" {
				db.beginErr = forced
			}
			stats, err := NewGroupRebuilder(db).RebuildGroups(context.Background(), "image")
			if err == nil || stats != (GroupStats{}) {
				t.Fatalf("stats=%+v err=%v, want zero/error", stats, err)
			}
			if stage == "commit_rollback" {
				if !errors.Is(err, pgx.ErrTxCommitRollback) ||
					errors.Is(err, ErrGroupCommitOutcomeUnknown) {
					t.Fatalf("rollback commit error=%v", err)
				}
			}
			if stage == "begin" {
				if tx.rollbackCalls != 0 {
					t.Fatalf("begin failure rollback calls=%d", tx.rollbackCalls)
				}
			} else if tx.rollbackCalls != 1 {
				t.Fatalf("%s rollback calls=%d, want 1", stage, tx.rollbackCalls)
			}
		})
	}
}

func TestRebuildGroupsCommitOutcomeUnknownReportsZeroWithoutAssumingRollback(
	t *testing.T,
) {
	a, b := groupTestSHA('a'), groupTestSHA('b')
	lostACK := errors.New("commit response lost")
	tx := &groupTestTx{
		scoreRows: groupScoreRows(
			groupTestScore("image", a, b, "yes", .9),
		),
		fileRows: groupFileRows(
			groupTestFile{1, a, "a", "a", 1},
			groupTestFile{2, b, "b", "b", 1},
		),
		failStage:   "commit_unknown",
		failure:     lostACK,
		nextGroupID: 1,
	}
	stats, err := NewGroupRebuilder(&groupTestDB{tx: tx}).
		RebuildGroups(context.Background(), "image")
	if !errors.Is(err, ErrGroupCommitOutcomeUnknown) ||
		!errors.Is(err, lostACK) ||
		stats != (GroupStats{}) {
		t.Fatalf("unknown commit stats=%+v err=%v", stats, err)
	}
	if !tx.commitApplied {
		t.Fatal("fake lost-ACK did not model a possibly committed transaction")
	}
	// RebuildGroups performs a best-effort Rollback after any returned error.
	// That call cannot prove rollback after Commit outcome became unknown.
}

type groupTestDB struct {
	tx       *groupTestTx
	beginErr error
	begins   int
}

func (db *groupTestDB) BeginTx(_ context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	db.begins++
	if db.beginErr != nil {
		return nil, db.beginErr
	}
	db.tx.options = options
	return db.tx, nil
}

func (*groupTestDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	panic("unexpected group DB Exec")
}

func (*groupTestDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	panic("unexpected group DB Query")
}

func (*groupTestDB) QueryRow(context.Context, string, ...any) pgx.Row {
	panic("unexpected group DB QueryRow")
}

type groupTestTx struct {
	pgx.Tx
	options       pgx.TxOptions
	scoreRows     *groupTestRows
	fileRows      *groupTestRows
	failStage     string
	failure       error
	deleteKind    string
	nextGroupID   int64
	groups        []groupTestInsertedGroup
	members       []groupTestInsertedMember
	committed     bool
	commitApplied bool
	rollbackCalls int
}

type groupTestInsertedGroup struct {
	kind           string
	representative int64
	members        int
}

type groupTestInsertedMember struct {
	groupID int64
	fileID  int64
	raw     []byte
}

func (tx *groupTestTx) Query(
	_ context.Context,
	sql string,
	_ ...any,
) (pgx.Rows, error) {
	if strings.Contains(sql, "FROM pair_scores") {
		if tx.failStage == "score_query" {
			return nil, tx.failure
		}
		if tx.failStage == "score_scan" {
			tx.scoreRows.scanErr = tx.failure
		}
		return tx.scoreRows, nil
	}
	if strings.Contains(sql, "FROM files") {
		if tx.failStage == "file_query" {
			return nil, tx.failure
		}
		if tx.failStage == "file_scan" {
			tx.fileRows.scanErr = tx.failure
		}
		return tx.fileRows, nil
	}
	return nil, fmt.Errorf("unexpected Query: %s", sql)
}

func (tx *groupTestTx) Exec(
	_ context.Context,
	sql string,
	args ...any,
) (pgconn.CommandTag, error) {
	switch {
	case strings.Contains(sql, "DELETE FROM dup_groups"):
		if tx.failStage == "delete" {
			return pgconn.CommandTag{}, tx.failure
		}
		tx.deleteKind = args[0].(string)
	case strings.Contains(sql, "LOCK TABLE dup_groups"):
		if tx.failStage == "lock" {
			return pgconn.CommandTag{}, tx.failure
		}
	case strings.Contains(sql, "INSERT INTO dup_members"):
		if tx.failStage == "member_insert" {
			return pgconn.CommandTag{}, tx.failure
		}
		raw := append([]byte(nil), args[2].([]byte)...)
		tx.members = append(tx.members, groupTestInsertedMember{
			groupID: args[0].(int64), fileID: args[1].(int64), raw: raw,
		})
	default:
		return pgconn.CommandTag{}, fmt.Errorf("unexpected Exec: %s", sql)
	}
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func (tx *groupTestTx) QueryRow(
	_ context.Context,
	sql string,
	args ...any,
) pgx.Row {
	if !strings.Contains(sql, "INSERT INTO dup_groups") {
		return groupTestRow{err: fmt.Errorf("unexpected QueryRow: %s", sql)}
	}
	if tx.failStage == "group_insert" {
		return groupTestRow{err: tx.failure}
	}
	tx.groups = append(tx.groups, groupTestInsertedGroup{
		kind:           args[0].(string),
		representative: args[1].(int64),
		members:        args[2].(int),
	})
	id := tx.nextGroupID
	tx.nextGroupID++
	return groupTestRow{value: id}
}

func (tx *groupTestTx) Commit(context.Context) error {
	switch tx.failStage {
	case "commit_rollback":
		return pgx.ErrTxCommitRollback
	case "commit_unknown":
		tx.commitApplied = true
		return tx.failure
	default:
		tx.committed = true
		return nil
	}
}

func (tx *groupTestTx) Rollback(context.Context) error {
	tx.rollbackCalls++
	return nil
}

func (tx *groupTestTx) memberDocuments(t *testing.T) map[int64]map[string]any {
	t.Helper()
	result := make(map[int64]map[string]any, len(tx.members))
	for _, member := range tx.members {
		var document map[string]any
		if err := json.Unmarshal(member.raw, &document); err != nil {
			t.Fatalf("decode member %d score: %v", member.fileID, err)
		}
		result[member.fileID] = document
	}
	return result
}

type groupTestRows struct {
	pgx.Rows
	scans   []func([]any) error
	index   int
	scanErr error
	closed  bool
}

func (rows *groupTestRows) Next() bool {
	return rows.index < len(rows.scans)
}

func (rows *groupTestRows) Scan(dest ...any) error {
	if rows.scanErr != nil {
		return rows.scanErr
	}
	err := rows.scans[rows.index](dest)
	rows.index++
	return err
}

func (rows *groupTestRows) Err() error { return nil }
func (rows *groupTestRows) Close()     { rows.closed = true }

type groupTestRow struct {
	value int64
	err   error
}

func (row groupTestRow) Scan(dest ...any) error {
	if row.err != nil {
		return row.err
	}
	*(dest[0].(*int64)) = row.value
	return nil
}

type groupTestScoreRow struct {
	kind             string
	a                string
	b                string
	verdict          string
	document         []byte
	overrideDocument *pairScoreDocument
}

func groupTestScore(kind, a, b, verdict string, similarity float64) groupTestScoreRow {
	document := pairScoreDocument{
		Version: pairScoreVersion,
		Kind:    kind,
		Verdict: verdict,
	}
	switch kind {
	case "image":
		document.Image = &imageScoreDocument{
			PHashEvaluated: verdict != "inconclusive",
			SobelEvaluated: verdict == "yes",
		}
		if verdict != "inconclusive" {
			document.Image.PHashPassRatio = groupFloat(.9)
		}
		if verdict == "yes" {
			document.Image.SobelCosine = groupFloat(similarity)
		}
	case "video":
		document.Video = &videoScoreDocument{
			ValidFrames:      6,
			PassedFrames:     6,
			AverageEvaluated: verdict != "inconclusive",
			Frames:           make([]frameScoreDocument, 6),
		}
		if verdict != "inconclusive" {
			document.Video.Average = groupFloat(similarity)
		}
		for index := range document.Video.Frames {
			document.Video.Frames[index] = frameScoreDocument{
				FrameIdx: index, Valid: true,
				PHashPassRatio: groupFloat(1),
				SobelEvaluated: true,
				SobelCosine:    groupFloat(similarity),
				Similarity:     groupFloat(similarity),
				Passed:         true,
			}
		}
	}
	raw, _ := json.Marshal(document)
	return groupTestScoreRow{
		kind: kind, a: a, b: b, verdict: verdict, document: raw,
	}
}

func groupScoreRows(scores ...groupTestScoreRow) *groupTestRows {
	rows := &groupTestRows{}
	for _, score := range scores {
		score := score
		rows.scans = append(rows.scans, func(dest []any) error {
			*(dest[0].(*string)) = score.kind
			*(dest[1].(*string)) = score.a
			*(dest[2].(*string)) = score.b
			if score.overrideDocument != nil {
				raw, err := json.Marshal(score.overrideDocument)
				if err != nil {
					// encoding/json rejects NaN, so preserve the non-finite value
					// through the typed decoder hook used only by this fake.
					raw = []byte(`{"version":1,"kind":"image","verdict":"yes","image":{"phash_evaluated":true,"phash_pass_ratio":1,"sobel_evaluated":true,"sobel_cosine":1e10000}}`)
				}
				*(dest[3].(*[]byte)) = raw
			} else {
				*(dest[3].(*[]byte)) = append([]byte(nil), score.document...)
			}
			*(dest[4].(*string)) = score.verdict
			return nil
		})
	}
	return rows
}

type groupTestFile struct {
	id      int64
	sha     string
	machine string
	path    string
	quality int
}

func groupFileRows(files ...groupTestFile) *groupTestRows {
	rows := &groupTestRows{}
	for _, file := range files {
		file := file
		rows.scans = append(rows.scans, func(dest []any) error {
			*(dest[0].(*int64)) = file.id
			*(dest[1].(*string)) = file.sha
			*(dest[2].(*string)) = file.machine
			*(dest[3].(*string)) = file.path
			*(dest[4].(*int)) = file.quality
			return nil
		})
	}
	return rows
}

func assertGroupMemberScore(
	t *testing.T,
	got map[string]any,
	representativeSHA string,
	via bool,
	edgeA, edgeB string,
	detail []byte,
) {
	t.Helper()
	if got["role"] != "member" || got["vs_rep_sha"] != representativeSHA {
		t.Fatalf("member identity=%v", got)
	}
	if gotVia, present := got["via"]; present != via || (present && gotVia != true) {
		t.Fatalf("member via=%v present=%v, want %v", gotVia, present, via)
	}
	if edgeA == "" {
		if _, present := got["edge"]; present {
			t.Fatalf("same-SHA copy unexpectedly has edge: %v", got)
		}
		return
	}
	wantEdge := map[string]any{"sha_a": edgeA, "sha_b": edgeB}
	if !reflect.DeepEqual(got["edge"], wantEdge) {
		t.Fatalf("edge=%v, want %v", got["edge"], wantEdge)
	}
	var wantDetail any
	if err := json.Unmarshal(detail, &wantDetail); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got["detail"], wantDetail) {
		t.Fatalf("detail=%v, want %v", got["detail"], wantDetail)
	}
}

func groupTestSHA(digit byte) string {
	return strings.Repeat(string(digit), 128)
}

func groupFloat(value float64) *float64 { return &value }
