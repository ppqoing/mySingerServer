package agent

import (
	"context"
	"fmt"

	"dedup/internal/localtask"
	"dedup/internal/proto"
	"dedup/internal/worker"
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
	if err := w.pool.Submit(job); err != nil {
		return nil, err
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
	Cancel(context.Context, string) error
	Retry(context.Context, string) (localtask.Task, error)
	Resume(context.Context) error
}

type LocalTaskHandler struct{ service LocalTaskService }

func NewLocalTaskHandler(service LocalTaskService) *LocalTaskHandler {
	return &LocalTaskHandler{service: service}
}

func (h *LocalTaskHandler) HandleLocal(ctx context.Context, request proto.LocalRequest) proto.LocalResponse {
	if h == nil || h.service == nil {
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
	case proto.LocalOperationTaskCancel:
		var input proto.LocalTaskIDRequest
		if err := proto.DecodeLocalPayload(request.Payload, &input); err != nil || input.Validate() != nil {
			return localTaskFailure(request.RequestID, "invalid_task_id")
		}
		if err := h.service.Cancel(ctx, input.TaskID); err != nil {
			return localTaskFailure(request.RequestID, safeLocalTaskError(err))
		}
		return localTaskSuccess(request.RequestID, struct{}{})
	case proto.LocalOperationTaskRetry:
		var input proto.LocalTaskIDRequest
		if err := proto.DecodeLocalPayload(request.Payload, &input); err != nil || input.Validate() != nil {
			return localTaskFailure(request.RequestID, "invalid_task_id")
		}
		task, err := h.service.Retry(ctx, input.TaskID)
		if err != nil {
			return localTaskFailure(request.RequestID, safeLocalTaskError(err))
		}
		return localTaskSuccess(request.RequestID, proto.LocalTaskRetryResponse{Task: task})
	default:
		return localTaskFailure(request.RequestID, proto.UnsupportedOperationErrorCode)
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
