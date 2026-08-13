package localreview

import (
	"context"
	"testing"

	"dedup/internal/proto"
	"dedup/internal/store"
)

// Break caught: the service drops filters or machine ownership before SQLite,
// or fails to map undecided members and existing video contact sheets to wire.
func TestGroupQueryMapsNarrowStorePage(t *testing.T) {
	backend := &reviewStoreFake{page: store.LocalGroupPage{
		Offset: 2, NextOffset: 3,
		Groups: []store.LocalResultGroup{{
			RunID: "run-1", Generation: 4, GroupID: "group-1",
			Category: "video", Verdict: "duplicate", ReviewStatus: "undecided",
			Members: []store.LocalGroupMember{{
				FileID: 9, Path: `D:\video\clip.mp4`, FileName: "clip.mp4",
				Size: 100, Status: "done", Decision: "undecided",
				VideoPreviewPath: `D:\cache\sheet.jpg`,
			}},
		}},
	}}
	service := NewService("machine-a", backend)
	response, err := service.List(context.Background(), proto.LocalGroupListRequest{
		Scope: "current", Category: "video", PathContains: "video",
		ReviewStatus: "undecided", Offset: 2, Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if backend.query.MachineID != "machine-a" || backend.query.Category != "video" ||
		backend.query.PathContains != "video" || backend.query.Offset != 2 || backend.query.Limit != 1 {
		t.Fatalf("store query = %#v", backend.query)
	}
	if len(response.Groups) != 1 || response.Groups[0].Members[0].Decision != "undecided" ||
		response.Groups[0].Members[0].VideoPreviewPath != `D:\cache\sheet.jpg` {
		t.Fatalf("wire response = %#v", response)
	}
}

// Break caught: an invalid or non-explicit selection reaches persistence, or
// service persistence omits the configured machine identity.
func TestReviewSaveValidatesAndCommitsExplicitSelection(t *testing.T) {
	backend := &reviewStoreFake{}
	service := NewService("machine-a", backend)
	request := proto.LocalReviewSaveRequest{
		RunID: "run-1", GroupID: "group-1", Reviewer: "user",
		Decisions: []proto.LocalReviewDecision{{FileID: 1, Decision: "keep"}},
	}
	response, err := service.Save(context.Background(), request)
	if err != nil || !response.Saved {
		t.Fatalf("Save = %#v, %v", response, err)
	}
	if backend.commit.MachineID != "machine-a" || backend.commit.RunID != "run-1" ||
		len(backend.commit.Decisions) != 1 || backend.commit.Decisions[0].Decision != "keep" {
		t.Fatalf("store commit = %#v", backend.commit)
	}
	request.Decisions = nil
	if _, err := service.Save(context.Background(), request); err == nil {
		t.Fatal("empty review selection was accepted")
	}
	if backend.commitCalls != 1 {
		t.Fatalf("invalid save reached store; calls=%d", backend.commitCalls)
	}
}

type reviewStoreFake struct {
	query       store.LocalGroupQuery
	page        store.LocalGroupPage
	group       store.LocalResultGroup
	commit      store.LocalReviewCommit
	commitCalls int
}

func (fake *reviewStoreFake) ListLocalGroups(_ context.Context, query store.LocalGroupQuery) (store.LocalGroupPage, error) {
	fake.query = query
	return fake.page, nil
}

func (fake *reviewStoreFake) LoadLocalGroup(context.Context, string, string, string, bool) (store.LocalResultGroup, error) {
	return fake.group, nil
}

func (fake *reviewStoreFake) CommitLocalReview(_ context.Context, commit store.LocalReviewCommit) error {
	fake.commitCalls++
	fake.commit = commit
	return nil
}
