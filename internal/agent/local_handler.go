package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"dedup/internal/localdelete"
	"dedup/internal/localtask"
	"dedup/internal/proto"
	"dedup/internal/store"
	"dedup/internal/worker"
	"github.com/vmihailenco/msgpack/v5"
)

type LocalStageWorker struct {
	pool   WorkerPool
	router *PoolRouter
}

func NewLocalStageWorker(pool WorkerPool, router *PoolRouter) *LocalStageWorker {
	return &LocalStageWorker{pool: pool, router: router}
}

func (w *LocalStageWorker) Execute(ctx context.Context, job *worker.JobMsg) (*worker.JobResultMsg, error) {
	if ctx == nil {
		return nil, fmt.Errorf("agent: local worker context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if w == nil || w.pool == nil || w.router == nil || job == nil {
		return nil, fmt.Errorf("agent: local worker dependencies are required")
	}
	job.JobID = w.router.NextJobID()
	terminal, cancelRoute, err := w.router.Register(job)
	if err != nil {
		return nil, err
	}
	defer cancelRoute()
	var submitErr error
	if submitter, ok := w.pool.(interface {
		SubmitContext(context.Context, *worker.JobMsg) error
	}); ok {
		submitErr = submitter.SubmitContext(ctx, job)
	} else {
		submitErr = w.pool.Submit(job)
	}
	if submitErr != nil {
		return nil, submitErr
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case outcome := <-terminal:
		if outcome.err != nil {
			return nil, outcome.err
		}
		if outcome.crash != nil {
			return nil, fmt.Errorf("agent: local worker crashed")
		}
		if outcome.result == nil {
			return nil, fmt.Errorf("agent: local worker returned empty terminal result")
		}
		return outcome.result, nil
	}
}

type LocalTaskService interface {
	Create(context.Context, localtask.CreateRequest) (localtask.Task, error)
	List(context.Context, localtask.ListRequest) (localtask.Page[localtask.Task], error)
	Pause(context.Context, localtask.ControlRequest) (localtask.Task, error)
	ResumeTask(context.Context, localtask.ControlRequest) (localtask.Task, error)
	Cancel(context.Context, localtask.ControlRequest) (localtask.Task, error)
	Delete(context.Context, localtask.ControlRequest) (localtask.ControlResult, error)
	Retry(context.Context, localtask.ControlRequest) (localtask.Task, error)
	LegacyCancel(context.Context, string) (localtask.Task, error)
	LegacyRetry(context.Context, string) (localtask.Task, error)
}

type LocalTaskHandler struct{ service LocalTaskService }

func NewLocalTaskHandler(service LocalTaskService) *LocalTaskHandler {
	return &LocalTaskHandler{service: service}
}

type LocalReviewService interface {
	List(context.Context, proto.LocalGroupListRequest) (proto.LocalGroupListResponse, error)
	Detail(context.Context, proto.LocalGroupDetailRequest) (proto.LocalGroupDetailResponse, error)
	Save(context.Context, proto.LocalReviewSaveRequest) (proto.LocalReviewSaveResponse, error)
}

type LocalPreviewService interface {
	Preview(context.Context, proto.LocalImagePreviewRequest) (proto.LocalImagePreviewResponse, error)
}

type LocalDeleteHandler struct{ service localdelete.Service }

func NewLocalDeleteHandler(service localdelete.Service) *LocalDeleteHandler {
	return &LocalDeleteHandler{service: service}
}

func (handler *LocalDeleteHandler) HandleLocal(ctx context.Context, request proto.LocalRequest) proto.LocalResponse {
	if handler == nil || handler.service == nil || ctx == nil || ctx.Err() != nil {
		return localTaskFailure(request.RequestID, "local_delete_unavailable")
	}
	switch request.Operation {
	case proto.LocalOperationDeletePrepare:
		var input proto.LocalDeletePrepareRequest
		if err := proto.DecodeLocalDeletePayload(request.Payload, &input); err != nil || input.Validate() != nil {
			return localTaskFailure(request.RequestID, "invalid_delete_selection")
		}
		preview, err := handler.service.Prepare(ctx, localdelete.DeleteSelection{RunID: input.RunID, GroupID: input.GroupID})
		if err != nil {
			return localTaskFailure(request.RequestID, safeLocalDeleteError(err))
		}
		return localTaskSuccess(request.RequestID, preview)
	case proto.LocalOperationDeleteExecute:
		var input proto.LocalDeleteExecuteRequest
		if err := proto.DecodeLocalDeletePayload(request.Payload, &input); err != nil || input.Validate() != nil {
			return localTaskFailure(request.RequestID, "invalid_delete_execution")
		}
		batch, err := handler.service.Execute(ctx, localdelete.DeleteExecution{
			BatchID: input.BatchID, SelectionDigest: input.SelectionDigest, Token: input.Token,
		})
		if err != nil {
			return localTaskFailure(request.RequestID, safeLocalDeleteError(err))
		}
		return localTaskSuccess(request.RequestID, batch)
	case proto.LocalOperationDeleteStatus:
		var input proto.LocalDeleteStatusRequest
		if err := proto.DecodeLocalDeletePayload(request.Payload, &input); err != nil || input.Validate() != nil {
			return localTaskFailure(request.RequestID, "invalid_delete_batch")
		}
		batch, err := handler.service.Status(ctx, input.BatchID)
		if err != nil {
			return localTaskFailure(request.RequestID, safeLocalDeleteError(err))
		}
		return localTaskSuccess(request.RequestID, batch)
	default:
		return localTaskFailure(request.RequestID, proto.UnsupportedOperationErrorCode)
	}
}

func safeLocalDeleteError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, localdelete.ErrInvalidToken) {
		return "invalid_delete_token"
	}
	if errors.Is(err, localdelete.ErrSelectionChanged) {
		return "delete_selection_changed"
	}
	return "local_delete_failed"
}

type LocalResultHandler struct {
	reviews  LocalReviewService
	previews LocalPreviewService
}

func NewLocalResultHandler(reviews LocalReviewService, previews LocalPreviewService) *LocalResultHandler {
	return &LocalResultHandler{reviews: reviews, previews: previews}
}

func (handler *LocalResultHandler) HandleLocal(ctx context.Context, request proto.LocalRequest) proto.LocalResponse {
	if handler == nil || ctx == nil || ctx.Err() != nil {
		return localTaskFailure(request.RequestID, "local_results_unavailable")
	}
	switch request.Operation {
	case proto.LocalOperationGroupsList:
		if handler.reviews == nil {
			return localTaskFailure(request.RequestID, "local_results_unavailable")
		}
		var input proto.LocalGroupListRequest
		if err := proto.DecodeLocalPayload(request.Payload, &input); err != nil || input.Validate() != nil {
			return localTaskFailure(request.RequestID, "invalid_group_query")
		}
		page, err := handler.reviews.List(ctx, input)
		if err != nil {
			return localTaskFailure(request.RequestID, "local_results_failed")
		}
		return localTaskSuccess(request.RequestID, page)
	case proto.LocalOperationGroupsDetail:
		if handler.reviews == nil {
			return localTaskFailure(request.RequestID, "local_results_unavailable")
		}
		var input proto.LocalGroupDetailRequest
		if err := proto.DecodeLocalPayload(request.Payload, &input); err != nil || input.Validate() != nil {
			return localTaskFailure(request.RequestID, "invalid_group_id")
		}
		detail, err := handler.reviews.Detail(ctx, input)
		if err != nil {
			return localTaskFailure(request.RequestID, "local_results_failed")
		}
		return localTaskSuccess(request.RequestID, detail)
	case proto.LocalOperationReviewSave:
		if handler.reviews == nil {
			return localTaskFailure(request.RequestID, "local_results_unavailable")
		}
		var input proto.LocalReviewSaveRequest
		if err := proto.DecodeLocalPayload(request.Payload, &input); err != nil || input.Validate() != nil {
			return localTaskFailure(request.RequestID, "invalid_review")
		}
		saved, err := handler.reviews.Save(ctx, input)
		if err != nil {
			return localTaskFailure(request.RequestID, "review_failed")
		}
		return localTaskSuccess(request.RequestID, saved)
	case proto.LocalOperationPreviewImage:
		if handler.previews == nil {
			return localTaskFailure(request.RequestID, "local_preview_unavailable")
		}
		var input proto.LocalImagePreviewRequest
		if err := proto.DecodeLocalImagePreviewPayload(request.Payload, &input); err != nil || input.Validate() != nil {
			return localTaskFailure(request.RequestID, "invalid_preview")
		}
		preview, err := handler.previews.Preview(ctx, input)
		if err != nil {
			return localTaskFailure(request.RequestID, safePreviewError(err))
		}
		return localTaskSuccess(request.RequestID, preview)
	default:
		return localTaskFailure(request.RequestID, proto.UnsupportedOperationErrorCode)
	}
}

func safePreviewError(err error) string {
	if err == nil {
		return ""
	}
	switch err.Error() {
	case "stale_preview", "preview_not_available", "preview_too_large", "preview_memory_limit":
		return err.Error()
	default:
		return "preview_failed"
	}
}

func (h *LocalTaskHandler) HandleLocal(ctx context.Context, request proto.LocalRequest) proto.LocalResponse {
	if h == nil || h.service == nil || ctx == nil || ctx.Err() != nil {
		return localTaskFailure(request.RequestID, "local_task_unavailable")
	}
	switch request.Operation {
	case proto.LocalOperationTaskCreate, proto.LocalOperationAnalysisStart:
		var input proto.LocalTaskCreateRequest
		if err := proto.DecodeLocalPayload(request.Payload, &input); err != nil || input.Validate() != nil {
			return localTaskFailure(request.RequestID, "invalid_task")
		}
		if request.Operation == proto.LocalOperationAnalysisStart {
			input.Mode = proto.LocalTaskModeScanThenAnalysis
		}
		task, err := h.service.Create(ctx, input)
		if err != nil {
			return localTaskFailure(request.RequestID, safeLocalTaskError(err))
		}
		return localTaskSuccess(request.RequestID, proto.LocalTaskCreateResponse{Task: task})
	case proto.LocalOperationTaskList, proto.LocalOperationAnalysisStatus:
		var input proto.LocalTaskListRequest
		if err := proto.DecodeLocalPayload(request.Payload, &input); err != nil {
			return localTaskFailure(request.RequestID, "invalid_task_list")
		}
		page, err := h.service.List(ctx, input)
		if err != nil {
			return localTaskFailure(request.RequestID, "local_task_failed")
		}
		return localTaskSuccess(request.RequestID, proto.LocalTaskListResponse{Tasks: page.Items, Offset: page.Offset, NextOffset: page.NextOffset})
	case proto.LocalOperationTaskPause:
		input, _, legacy, err := decodeLocalTaskControl(request.Payload, false)
		if err != nil || legacy {
			return localTaskFailure(request.RequestID, proto.InvalidTaskControlErrorCode)
		}
		task, err := h.service.Pause(ctx, input)
		return localTaskControlResponse(request.RequestID, task, err, false)
	case proto.LocalOperationTaskResume:
		input, _, legacy, err := decodeLocalTaskControl(request.Payload, false)
		if err != nil || legacy {
			return localTaskFailure(request.RequestID, proto.InvalidTaskControlErrorCode)
		}
		task, err := h.service.ResumeTask(ctx, input)
		return localTaskControlResponse(request.RequestID, task, err, false)
	case proto.LocalOperationTaskCancel:
		input, legacyTaskID, legacy, err := decodeLocalTaskControl(request.Payload, true)
		if err != nil {
			return localTaskFailure(request.RequestID, proto.InvalidTaskControlErrorCode)
		}
		if legacy {
			task, err := h.service.LegacyCancel(ctx, legacyTaskID)
			return localTaskControlResponse(request.RequestID, task, err, false)
		}
		task, err := h.service.Cancel(ctx, input)
		return localTaskControlResponse(request.RequestID, task, err, false)
	case proto.LocalOperationTaskRetry:
		input, legacyTaskID, legacy, err := decodeLocalTaskControl(request.Payload, true)
		if err != nil {
			return localTaskFailure(request.RequestID, proto.InvalidTaskControlErrorCode)
		}
		if legacy {
			task, err := h.service.LegacyRetry(ctx, legacyTaskID)
			return localTaskControlResponse(request.RequestID, task, err, false)
		}
		task, err := h.service.Retry(ctx, input)
		return localTaskControlResponse(request.RequestID, task, err, false)
	case proto.LocalOperationTaskDelete:
		input, _, legacy, err := decodeLocalTaskControl(request.Payload, false)
		if err != nil || legacy {
			return localTaskFailure(request.RequestID, proto.InvalidTaskControlErrorCode)
		}
		result, err := h.service.Delete(ctx, input)
		if err != nil {
			return localTaskFailure(request.RequestID, safeLocalTaskControlError(err, true))
		}
		return localTaskSuccess(request.RequestID, result)
	default:
		return localTaskFailure(request.RequestID, proto.UnsupportedOperationErrorCode)
	}
}

func decodeLocalTaskControl(payload []byte, legacyAllowed bool) (localtask.ControlRequest, string, bool, error) {
	var fields map[string]msgpack.RawMessage
	if err := proto.DecodeLocalPayload(payload, &fields); err != nil || len(fields) == 0 {
		return localtask.ControlRequest{}, "", false, proto.LocalTaskControlRequest{}.Validate()
	}
	_, hasInstance := fields["instance_id"]
	_, hasRevision := fields["expected_revision"]
	if hasInstance || hasRevision {
		var request localtask.ControlRequest
		if err := proto.DecodeLocalPayload(payload, &request); err != nil || request.Validate() != nil {
			return localtask.ControlRequest{}, "", false, proto.LocalTaskControlRequest{}.Validate()
		}
		return request, "", false, nil
	}
	if !legacyAllowed || len(fields) != 1 || fields["task_id"] == nil {
		return localtask.ControlRequest{}, "", false, proto.LocalTaskControlRequest{}.Validate()
	}
	var request proto.LocalTaskIDRequest
	if err := proto.DecodeLocalPayload(payload, &request); err != nil || request.Validate() != nil {
		return localtask.ControlRequest{}, "", false, proto.LocalTaskControlRequest{}.Validate()
	}
	return localtask.ControlRequest{}, request.TaskID, true, nil
}

func localTaskControlResponse(requestID string, task localtask.Task, err error, deleting bool) proto.LocalResponse {
	if err != nil {
		return localTaskFailure(requestID, safeLocalTaskControlError(err, deleting))
	}
	return localTaskSuccess(requestID, localtask.ControlResult{Task: &task})
}

func safeLocalTaskControlError(err error, deleting bool) string {
	switch {
	case errors.Is(err, store.ErrLocalTaskStale):
		return "stale_task"
	case errors.Is(err, store.ErrLocalTaskInstanceMismatch):
		return "task_instance_mismatch"
	case errors.Is(err, localtask.ErrTaskInstanceRequired):
		return "task_instance_required"
	case errors.Is(err, store.ErrLocalTaskTransition):
		return "invalid_task_state"
	case errors.Is(err, sql.ErrNoRows):
		return "task_not_found"
	case deleting:
		return "task_delete_failed"
	default:
		return "task_control_failed"
	}
}

func localTaskSuccess(requestID string, value any) proto.LocalResponse {
	payload, err := proto.EncodeLocalPayload(value)
	if err != nil {
		return localTaskFailure(requestID, "internal_error")
	}
	return proto.LocalResponse{RequestID: requestID, OK: true, Payload: payload}
}

func localTaskFailure(requestID, code string) proto.LocalResponse {
	return proto.LocalResponse{RequestID: requestID, ErrorCode: code}
}

func safeLocalTaskError(err error) string {
	if err == nil {
		return ""
	}
	if err.Error() == "task_conflict" || len(err.Error()) >= len("task_conflict") && err.Error()[:len("task_conflict")] == "task_conflict" {
		return "task_conflict"
	}
	return "local_task_failed"
}
