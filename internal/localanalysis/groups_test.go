package localanalysis

import (
	"reflect"
	"testing"
)

func TestDeterministicGroupIDAndRepresentativeIgnoreInputOrder(t *testing.T) {
	files := []GroupFile{
		{FileID: 3, SHA512: "b", Path: `D:\z.jpg`, Quality: 90},
		{FileID: 1, SHA512: "a", Path: `D:\a.jpg`, Quality: 80},
		{FileID: 2, SHA512: "a", Path: `D:\b.jpg`, Quality: 95},
	}
	edges := []PairDecision{{Category: "image", SHAA: "b", SHAB: "a", Verdict: "yes"}}
	first, err := BuildFinalGroups("run-1", files, edges)
	if err != nil {
		t.Fatal(err)
	}
	reversedFiles := []GroupFile{files[2], files[1], files[0]}
	reversedEdges := []PairDecision{{Category: "image", SHAA: "a", SHAB: "b", Verdict: "yes"}}
	second, err := BuildFinalGroups("run-1", reversedFiles, reversedEdges)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("groups vary by input order:\n%#v\n%#v", first, second)
	}
	if len(first) != 2 || first[1].RepresentativeFileID != 2 {
		t.Fatalf("groups = %#v, want exact plus image and representative 2", first)
	}
}

func TestBuildFinalGroupsIncludesOnlyExactAndYes(t *testing.T) {
	files := []GroupFile{
		{FileID: 1, SHA512: "exact"}, {FileID: 2, SHA512: "exact"},
		{FileID: 3, SHA512: "yes-a"}, {FileID: 4, SHA512: "yes-b"},
		{FileID: 5, SHA512: "no-a"}, {FileID: 6, SHA512: "no-b"},
		{FileID: 7, SHA512: "maybe-a"}, {FileID: 8, SHA512: "maybe-b"},
	}
	edges := []PairDecision{
		{Category: "image", SHAA: "yes-a", SHAB: "yes-b", Verdict: "yes"},
		{Category: "image", SHAA: "no-a", SHAB: "no-b", Verdict: "no"},
		{Category: "video", SHAA: "maybe-a", SHAB: "maybe-b", Verdict: "inconclusive"},
	}
	groups, err := BuildFinalGroups("run-2", files, edges)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 || groups[0].Category != "exact" || groups[1].Category != "image" {
		t.Fatalf("groups = %#v, want exact and yes only", groups)
	}
}
