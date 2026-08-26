package gui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/google/uuid"

	"dedup/internal/proto"
)

const maxDeleteRequestBytes int64 = 1 << 20

type deleteHTTPService interface {
	Prepare(context.Context, []int64) (DeleteSummary, string, error)
	Execute(context.Context, string, string) (string, error)
	Status(string) (DeleteTaskStatus, bool)
	ListTasks(context.Context, int) []DeleteTaskSummary
	ConsumedTaskID(string) (string, bool)
}

type deletePrepareRequest struct {
	MemberIDs []int64 `json:"member_ids"`
}

type deleteExecuteRequest struct {
	ConfirmToken string `json:"confirm_token"`
	Mode         string `json:"mode"`
}

func decodeDeleteJSON(
	response http.ResponseWriter,
	request *http.Request,
	value any,
) bool {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeJSON(response, http.StatusUnsupportedMediaType, map[string]string{
			"error": "application/json required",
		})
		return false
	}
	request.Body = http.MaxBytesReader(response, request.Body, maxDeleteRequestBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		writeDeleteDecodeError(response, err)
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeDeleteDecodeError(response, err)
		return false
	}
	return true
}

func writeDeleteDecodeError(response http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeJSON(response, http.StatusRequestEntityTooLarge, map[string]string{
			"error": "request body too large",
		})
		return
	}
	writeJSON(response, http.StatusBadRequest, map[string]string{
		"error": "invalid request",
	})
}

func (api *API) handleDeletePrepare(response http.ResponseWriter, request *http.Request) {
	var input deletePrepareRequest
	if !decodeDeleteJSON(response, request, &input) {
		return
	}
	if !validDeleteMemberIDs(input.MemberIDs) {
		writeJSON(response, http.StatusBadRequest, map[string]string{
			"error": "invalid member selection",
		})
		return
	}
	if api.delete == nil {
		writeJSON(response, http.StatusServiceUnavailable, map[string]string{
			"error": "delete service unavailable",
		})
		return
	}
	summary, token, err := api.delete.Prepare(request.Context(), input.MemberIDs)
	if err != nil {
		writeDeletePrepareError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"confirm_token":      token,
		"expires_in_seconds": 60,
		"summary":            summary,
	})
}

func validDeleteMemberIDs(ids []int64) bool {
	if len(ids) == 0 || len(ids) > 10000 {
		return false
	}
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return false
		}
		if _, exists := seen[id]; exists {
			return false
		}
		seen[id] = struct{}{}
	}
	return true
}

func writeDeletePrepareError(response http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrDeleteSelection):
		writeJSON(response, http.StatusConflict, map[string]string{
			"error": "delete selection conflict",
		})
	case errors.Is(err, ErrDeleteUnavailable):
		writeJSON(response, http.StatusServiceUnavailable, map[string]string{
			"error": "delete service unavailable",
		})
	default:
		writeJSON(response, http.StatusInternalServerError, map[string]string{
			"error": "delete request failed",
		})
	}
}

func (api *API) handleDeleteExecute(response http.ResponseWriter, request *http.Request) {
	var input deleteExecuteRequest
	if !decodeDeleteJSON(response, request, &input) {
		return
	}
	if input.ConfirmToken == "" {
		writeJSON(response, http.StatusBadRequest, map[string]string{
			"error": "invalid confirmation",
		})
		return
	}
	switch input.Mode {
	case "", proto.ModeSoft, proto.ModeHard:
	default:
		writeJSON(response, http.StatusBadRequest, map[string]string{
			"error": "invalid delete mode",
		})
		return
	}
	if api.delete == nil {
		writeJSON(response, http.StatusServiceUnavailable, map[string]string{
			"error": "delete service unavailable",
		})
		return
	}
	taskID, err := api.delete.Execute(
		request.Context(),
		input.ConfirmToken,
		input.Mode,
	)
	if err != nil {
		if errors.Is(err, ErrConfirmationConsumed) {
			// Idempotent replay: a repeated execute of a consumed token
			// returns the first accepted task instead of a conflict.
			if firstTaskID, ok := api.delete.ConsumedTaskID(
				input.ConfirmToken,
			); ok && firstTaskID != "" {
				writeJSON(response, http.StatusOK, map[string]string{
					"task_id": firstTaskID,
				})
				return
			}
		}
		writeDeleteExecuteError(response, err)
		return
	}
	writeJSON(response, http.StatusAccepted, map[string]string{
		"task_id": taskID,
	})
}

func writeDeleteExecuteError(response http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrConfirmationInvalid),
		errors.Is(err, ErrConfirmationExpired),
		errors.Is(err, ErrDeleteMode):
		writeJSON(response, http.StatusBadRequest, map[string]string{
			"error": "invalid confirmation",
		})
	case errors.Is(err, ErrConfirmationConsumed):
		writeJSON(response, http.StatusConflict, map[string]string{
			"error": "confirmation already used",
		})
	case errors.Is(err, ErrDeleteUnavailable):
		writeJSON(response, http.StatusServiceUnavailable, map[string]string{
			"error": "delete service unavailable",
		})
	default:
		writeJSON(response, http.StatusInternalServerError, map[string]string{
			"error": "delete request failed",
		})
	}
}

func (api *API) handleDeleteStatus(response http.ResponseWriter, request *http.Request) {
	taskID := request.PathValue("task_id")
	if taskID == "" {
		api.handleDeleteTaskList(response, request)
		return
	}
	parsed, err := uuid.Parse(taskID)
	if err != nil || parsed.String() != taskID {
		writeJSON(response, http.StatusNotFound, map[string]string{
			"error": "delete task not found",
		})
		return
	}
	if api.delete == nil {
		writeJSON(response, http.StatusServiceUnavailable, map[string]string{
			"error": "delete service unavailable",
		})
		return
	}
	status, ok := api.delete.Status(taskID)
	if !ok {
		writeJSON(response, http.StatusNotFound, map[string]string{
			"error": "delete task not found",
		})
		return
	}
	writeJSON(response, http.StatusOK, safeDeleteStatus(status))
}

// handleDeleteTaskList serves GET /api/delete/tasks and /api/delete/tasks/:
// the task-center list view, in-progress first, newest created first.
func (api *API) handleDeleteTaskList(response http.ResponseWriter, request *http.Request) {
	if api.delete == nil {
		writeJSON(response, http.StatusServiceUnavailable, map[string]string{
			"error": "delete service unavailable",
		})
		return
	}
	summaries := api.delete.ListTasks(request.Context(), deleteTaskListLimit)
	writeJSON(response, http.StatusOK, map[string]any{
		"tasks": summaries,
	})
}

func safeDeleteStatus(status DeleteTaskStatus) DeleteTaskStatus {
	safe := cloneDeleteTaskStatus(status)
	sanitized := make(map[string]int64, len(safe.ErrorCodes))
	for code, count := range safe.ErrorCodes {
		sanitized[safeDeleteErrorCode(code)] += count
	}
	safe.ErrorCodes = sanitized
	for index := range safe.Problems {
		safe.Problems[index].ErrorCode = safeDeleteErrorCode(
			safe.Problems[index].ErrorCode,
		)
		if safe.Problems[index].ErrorMessage != "" {
			safe.Problems[index].ErrorMessage = "delete item failed"
		}
		if safe.Problems[index].StateSyncErr != "" {
			safe.Problems[index].StateSyncErr = "delete state synchronization failed"
		}
	}
	return safe
}

func safeDeleteErrorCode(code string) string {
	switch code {
	case "",
		proto.DeleteErrNotFound,
		proto.DeleteErrBadPath,
		proto.DeleteErrPathDenied,
		proto.DeleteErrNotConfirmed,
		proto.DeleteErrReadonly,
		proto.DeleteErrAccessDenied,
		proto.DeleteErrDeleteFailed,
		proto.DeleteErrRecycleFailed,
		proto.DeleteErrInUse,
		proto.DeleteErrReparse,
		proto.DeleteErrBadMode,
		proto.DeleteErrHelperLost:
		return code
	default:
		return proto.DeleteErrDeleteFailed
	}
}

func cloneDeleteSequences(
	sequences map[uint32]DeleteSequenceStatus,
) map[uint32]DeleteSequenceStatus {
	copySequences := make(map[uint32]DeleteSequenceStatus, len(sequences))
	for sequence, status := range sequences {
		copySequences[sequence] = status
	}
	return copySequences
}
