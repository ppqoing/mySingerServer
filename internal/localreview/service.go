package localreview

import (
	"context"
	"errors"

	"dedup/internal/proto"
	"dedup/internal/store"
)

var ErrUnavailable = errors.New("local_review_unavailable")

type Store interface {
	ListLocalGroups(context.Context, store.LocalGroupQuery) (store.LocalGroupPage, error)
	LoadLocalGroup(context.Context, string, string, string, bool) (store.LocalResultGroup, error)
	CommitLocalReview(context.Context, store.LocalReviewCommit) error
}

type Service struct {
	machineID string
	store     Store
}

func NewService(machineID string, backend Store) *Service {
	return &Service{machineID: machineID, store: backend}
}

func (service *Service) List(ctx context.Context, request proto.LocalGroupListRequest) (proto.LocalGroupListResponse, error) {
	if service == nil || service.store == nil || service.machineID == "" || ctx == nil {
		return proto.LocalGroupListResponse{}, ErrUnavailable
	}
	if err := request.Validate(); err != nil {
		return proto.LocalGroupListResponse{}, err
	}
	page, err := service.store.ListLocalGroups(ctx, store.LocalGroupQuery{
		MachineID: service.machineID, Scope: request.Scope, RunID: request.RunID,
		Category: request.Category, PathContains: request.PathContains,
		FileNameContains: request.FileNameContains, MinSize: request.MinSize,
		MaxSize: request.MaxSize, ReviewStatus: request.ReviewStatus,
		Offset: request.Offset, Limit: request.Limit,
	})
	if err != nil {
		return proto.LocalGroupListResponse{}, err
	}
	groups := make([]proto.LocalGroup, len(page.Groups))
	for index, group := range page.Groups {
		groups[index] = mapGroup(group)
	}
	return proto.LocalGroupListResponse{
		Groups: groups, Offset: page.Offset, NextOffset: page.NextOffset,
	}, nil
}

func (service *Service) Detail(ctx context.Context, request proto.LocalGroupDetailRequest) (proto.LocalGroupDetailResponse, error) {
	if service == nil || service.store == nil || service.machineID == "" || ctx == nil {
		return proto.LocalGroupDetailResponse{}, ErrUnavailable
	}
	if err := request.Validate(); err != nil {
		return proto.LocalGroupDetailResponse{}, err
	}
	group, err := service.store.LoadLocalGroup(ctx, service.machineID, request.RunID, request.GroupID, request.RunID == "")
	if err != nil {
		return proto.LocalGroupDetailResponse{}, err
	}
	return proto.LocalGroupDetailResponse{Group: mapGroup(group)}, nil
}

func (service *Service) Save(ctx context.Context, request proto.LocalReviewSaveRequest) (proto.LocalReviewSaveResponse, error) {
	if service == nil || service.store == nil || service.machineID == "" || ctx == nil {
		return proto.LocalReviewSaveResponse{}, ErrUnavailable
	}
	if err := request.Validate(); err != nil {
		return proto.LocalReviewSaveResponse{}, err
	}
	choices := make([]store.LocalReviewChoice, len(request.Decisions))
	hasKeep := false
	for index, decision := range request.Decisions {
		choices[index] = store.LocalReviewChoice{FileID: decision.FileID, Decision: decision.Decision}
		hasKeep = hasKeep || decision.Decision == "keep"
	}
	if !hasKeep {
		return proto.LocalReviewSaveResponse{}, errors.New("review_requires_keep")
	}
	if err := service.store.CommitLocalReview(ctx, store.LocalReviewCommit{
		MachineID: service.machineID, RunID: request.RunID, GroupID: request.GroupID,
		Reviewer: request.Reviewer, Note: request.Note, Decisions: choices,
	}); err != nil {
		return proto.LocalReviewSaveResponse{}, err
	}
	return proto.LocalReviewSaveResponse{Saved: true}, nil
}

func mapGroup(group store.LocalResultGroup) proto.LocalGroup {
	members := make([]proto.LocalGroupMember, len(group.Members))
	for index, member := range group.Members {
		members[index] = proto.LocalGroupMember{
			FileID: member.FileID, Path: member.Path, FileName: member.FileName,
			Size: member.Size, Status: member.Status, Decision: member.Decision,
			VideoPreviewPath: member.VideoPreviewPath,
		}
	}
	return proto.LocalGroup{
		RunID: group.RunID, Generation: group.Generation, GroupID: group.GroupID,
		Category: group.Category, Verdict: group.Verdict,
		ReviewStatus: group.ReviewStatus, Members: members,
	}
}
