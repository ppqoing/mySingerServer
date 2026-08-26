package proto

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/vmihailenco/msgpack/v5"

	"dedup/internal/nodectl"
)

const LocalPayloadMaxBytes = 4 * 1024 * 1024
const MaxLocalPreviewEncodedBytes = LocalPayloadMaxBytes - 1024

const (
	UnsupportedOperationErrorCode = "unsupported_operation"
	LocalPayloadTooLargeErrorCode = "payload_too_large"
	InvalidLocalTopicErrorCode    = "invalid_topic"
)

const (
	LocalOperationStatusGet      = "local.status.get"
	LocalOperationConfigGet      = "local.config.get"
	LocalOperationConfigValidate = "local.config.validate"
	LocalOperationConfigSave     = "local.config.save"
	LocalOperationTaskCreate     = "local.task.create"
	LocalOperationTaskList       = "local.task.list"
	LocalOperationTaskCancel     = "local.task.cancel"
	LocalOperationTaskRetry      = "local.task.retry"
	LocalOperationAnalysisStart  = "local.analysis.start"
	LocalOperationAnalysisStatus = "local.analysis.status"
	LocalOperationGroupsList     = "local.groups.list"
	LocalOperationGroupsDetail   = "local.groups.detail"
	LocalOperationReviewSave     = "local.review.save"
	LocalOperationDeletePrepare  = "local.delete.prepare"
	LocalOperationDeleteExecute  = "local.delete.execute"
	LocalOperationDeleteStatus   = "local.delete.status"
	LocalOperationPreviewImage   = "local.preview.image"
	LocalOperationShutdown       = "local.shutdown"
)

type ClientAuth struct {
	Role    string `msgpack:"role"`
	Token   string `msgpack:"token"`
	Version int    `msgpack:"version"`
}

type ClientAuthResult struct {
	Accepted  bool   `msgpack:"accepted"`
	ErrorCode string `msgpack:"error_code,omitempty"`
}

type LocalRequest struct {
	RequestID string `msgpack:"request_id"`
	Operation string `msgpack:"operation"`
	Payload   []byte `msgpack:"payload,omitempty"`
}

func (request LocalRequest) Validate() error {
	if len(request.Payload) > LocalPayloadMaxBytes {
		return errors.New(LocalPayloadTooLargeErrorCode)
	}
	if !IsLocalOperation(request.Operation) {
		return errors.New(UnsupportedOperationErrorCode)
	}
	return nil
}

type LocalResponse struct {
	RequestID string `msgpack:"request_id"`
	OK        bool   `msgpack:"ok"`
	ErrorCode string `msgpack:"error_code,omitempty"`
	Payload   []byte `msgpack:"payload,omitempty"`
}

func (response LocalResponse) Validate() error {
	if len(response.Payload) > LocalPayloadMaxBytes {
		return errors.New(LocalPayloadTooLargeErrorCode)
	}
	return nil
}

type LocalEvent struct {
	Sequence uint64 `msgpack:"sequence"`
	Topic    string `msgpack:"topic"`
	Payload  []byte `msgpack:"payload,omitempty"`
}

type LocalStatusGetResponse struct {
	Status nodectl.Status `msgpack:"status"`
}

type LocalConfigGetResponse struct {
	CanonicalJSON []byte `msgpack:"canonical_json"`
	SHA256        string `msgpack:"sha256"`
}

type LocalConfigRequest struct {
	CanonicalJSON []byte `msgpack:"canonical_json"`
}

type LocalConfigValidateResponse struct {
	Valid           bool   `msgpack:"valid"`
	SHA256          string `msgpack:"sha256"`
	RestartRequired bool   `msgpack:"restart_required"`
}

type LocalConfigSaveResponse struct {
	SHA256          string `msgpack:"sha256"`
	RestartRequired bool   `msgpack:"restart_required"`
}

type LocalShutdownResponse struct {
	Accepted bool `msgpack:"accepted"`
}

const (
	LocalTaskModeScanOnly         = "scan_only"
	LocalTaskModeScanThenAnalysis = "scan_then_analysis"
)

type LocalTaskCreateRequest struct {
	TaskID     string   `msgpack:"task_id"`
	Roots      []string `msgpack:"roots"`
	Mode       string   `msgpack:"mode"`
	Rescan     bool     `msgpack:"rescan"`
	Extensions []string `msgpack:"extensions,omitempty"`
}

func (request LocalTaskCreateRequest) Validate() error {
	if request.TaskID == "" || strings.TrimSpace(request.TaskID) != request.TaskID {
		return fmt.Errorf("invalid_task_id")
	}
	if request.Mode != LocalTaskModeScanOnly && request.Mode != LocalTaskModeScanThenAnalysis {
		return fmt.Errorf("invalid_task_mode")
	}
	if len(request.Roots) == 0 {
		return fmt.Errorf("invalid_roots")
	}
	roots := make(map[string]struct{}, len(request.Roots))
	for _, root := range request.Roots {
		if root == "" || strings.TrimSpace(root) != root {
			return fmt.Errorf("invalid_roots")
		}
		key := strings.ToLower(filepath.Clean(root))
		if _, exists := roots[key]; exists {
			return fmt.Errorf("duplicate_root")
		}
		roots[key] = struct{}{}
	}
	extensions := make(map[string]struct{}, len(request.Extensions))
	for _, extension := range request.Extensions {
		if extension == "" || strings.TrimSpace(extension) != extension ||
			!strings.HasPrefix(extension, ".") || extension != strings.ToLower(extension) {
			return fmt.Errorf("invalid_extension")
		}
		if _, exists := extensions[extension]; exists {
			return fmt.Errorf("duplicate_extension")
		}
		extensions[extension] = struct{}{}
	}
	return nil
}

type LocalTask struct {
	TaskID           string   `msgpack:"task_id"`
	Source           string   `msgpack:"source"`
	Mode             string   `msgpack:"mode"`
	Stage            int      `msgpack:"stage"`
	Status           string   `msgpack:"status"`
	Roots            []string `msgpack:"roots,omitempty"`
	Rescan           bool     `msgpack:"rescan,omitempty"`
	Extensions       []string `msgpack:"extensions,omitempty"`
	ProgressComplete int64    `msgpack:"progress_complete"`
	ProgressTotal    int64    `msgpack:"progress_total"`
	StatsJSON        string   `msgpack:"stats_json"`
	SafeErrorCode    string   `msgpack:"safe_error_code,omitempty"`
	SafeErrorMessage string   `msgpack:"safe_error_message,omitempty"`
	CreatedAt        int64    `msgpack:"created_at"`
	UpdatedAt        int64    `msgpack:"updated_at"`
}

type LocalTaskCreateResponse struct {
	Task LocalTask `msgpack:"task"`
}

type LocalTaskListRequest struct {
	Offset int `msgpack:"offset"`
	Limit  int `msgpack:"limit"`
}

type LocalTaskListResponse struct {
	Tasks      []LocalTask `msgpack:"tasks"`
	Offset     int         `msgpack:"offset"`
	NextOffset int         `msgpack:"next_offset"`
}

type LocalTaskIDRequest struct {
	TaskID string `msgpack:"task_id"`
}

func (request LocalTaskIDRequest) Validate() error {
	if request.TaskID == "" || strings.TrimSpace(request.TaskID) != request.TaskID {
		return fmt.Errorf("invalid_task_id")
	}
	return nil
}

type LocalTaskRetryResponse struct {
	Task LocalTask `msgpack:"task"`
}

type LocalGroupListRequest struct {
	Scope            string `msgpack:"scope,omitempty"`
	RunID            string `msgpack:"run_id,omitempty"`
	Category         string `msgpack:"category,omitempty"`
	PathContains     string `msgpack:"path_contains,omitempty"`
	FileNameContains string `msgpack:"file_name_contains,omitempty"`
	MinSize          *int64 `msgpack:"min_size,omitempty"`
	MaxSize          *int64 `msgpack:"max_size,omitempty"`
	ReviewStatus     string `msgpack:"review_status,omitempty"`
	Offset           int    `msgpack:"offset"`
	Limit            int    `msgpack:"limit"`
}

func (request LocalGroupListRequest) Validate() error {
	if request.Scope == "" {
		request.Scope = "current"
	}
	if request.Scope != "current" && request.Scope != "history" {
		return fmt.Errorf("invalid_group_scope")
	}
	if request.Scope == "history" && request.RunID == "" {
		return fmt.Errorf("invalid_run_id")
	}
	if request.RunID != "" && strings.TrimSpace(request.RunID) != request.RunID {
		return fmt.Errorf("invalid_run_id")
	}
	switch request.Category {
	case "", "exact", "image", "video", "inconclusive":
	default:
		return fmt.Errorf("invalid_group_category")
	}
	switch request.ReviewStatus {
	case "", "undecided", "reviewed", "keep", "delete":
	default:
		return fmt.Errorf("invalid_review_status")
	}
	if request.Offset < 0 || request.Limit < 0 || request.Limit > 200 {
		return fmt.Errorf("invalid_group_page")
	}
	if request.MinSize != nil && *request.MinSize < 0 ||
		request.MaxSize != nil && *request.MaxSize < 0 ||
		request.MinSize != nil && request.MaxSize != nil && *request.MinSize > *request.MaxSize {
		return fmt.Errorf("invalid_size_filter")
	}
	return nil
}

type LocalGroupMember struct {
	FileID           int64  `msgpack:"file_id"`
	Path             string `msgpack:"path"`
	FileName         string `msgpack:"file_name"`
	Size             int64  `msgpack:"size"`
	Status           string `msgpack:"status"`
	Decision         string `msgpack:"decision"`
	VideoPreviewPath string `msgpack:"video_preview_path,omitempty"`
}

type LocalGroup struct {
	RunID        string             `msgpack:"run_id"`
	Generation   int64              `msgpack:"generation"`
	GroupID      string             `msgpack:"group_id"`
	Category     string             `msgpack:"category"`
	Verdict      string             `msgpack:"verdict"`
	ReviewStatus string             `msgpack:"review_status"`
	Members      []LocalGroupMember `msgpack:"members"`
}

type LocalGroupListResponse struct {
	Groups     []LocalGroup `msgpack:"groups"`
	Offset     int          `msgpack:"offset"`
	NextOffset int          `msgpack:"next_offset"`
}

type LocalGroupDetailRequest struct {
	RunID   string `msgpack:"run_id,omitempty"`
	GroupID string `msgpack:"group_id"`
}

func (request LocalGroupDetailRequest) Validate() error {
	if request.GroupID == "" || strings.TrimSpace(request.GroupID) != request.GroupID ||
		(request.RunID != "" && strings.TrimSpace(request.RunID) != request.RunID) {
		return fmt.Errorf("invalid_group_id")
	}
	return nil
}

type LocalGroupDetailResponse struct {
	Group LocalGroup `msgpack:"group"`
}

type LocalReviewDecision struct {
	FileID   int64  `msgpack:"file_id"`
	Decision string `msgpack:"decision"`
}

type LocalReviewSaveRequest struct {
	RunID     string                `msgpack:"run_id"`
	GroupID   string                `msgpack:"group_id"`
	Reviewer  string                `msgpack:"reviewer"`
	Note      string                `msgpack:"note,omitempty"`
	Decisions []LocalReviewDecision `msgpack:"decisions"`
}

func (request LocalReviewSaveRequest) Validate() error {
	if request.RunID == "" || request.GroupID == "" || request.Reviewer == "" ||
		strings.TrimSpace(request.RunID) != request.RunID ||
		strings.TrimSpace(request.GroupID) != request.GroupID ||
		strings.TrimSpace(request.Reviewer) != request.Reviewer || len(request.Decisions) == 0 {
		return fmt.Errorf("invalid_review")
	}
	seen := make(map[int64]struct{}, len(request.Decisions))
	for _, decision := range request.Decisions {
		if decision.FileID <= 0 ||
			(decision.Decision != "keep" && decision.Decision != "delete" && decision.Decision != "undecided") {
			return fmt.Errorf("invalid_review")
		}
		if _, exists := seen[decision.FileID]; exists {
			return fmt.Errorf("invalid_review")
		}
		seen[decision.FileID] = struct{}{}
	}
	return nil
}

type LocalReviewSaveResponse struct {
	Saved bool `msgpack:"saved"`
}

type LocalDeletePrepareRequest struct {
	RunID   string `msgpack:"run_id"`
	GroupID string `msgpack:"group_id"`
}

func (request LocalDeletePrepareRequest) Validate() error {
	if request.RunID == "" || request.GroupID == "" ||
		strings.TrimSpace(request.RunID) != request.RunID ||
		strings.TrimSpace(request.GroupID) != request.GroupID {
		return errors.New("invalid_delete_selection")
	}
	return nil
}

type LocalDeleteFile struct {
	FileID int64  `msgpack:"file_id"`
	Path   string `msgpack:"path"`
	Size   int64  `msgpack:"size"`
	SHA512 string `msgpack:"sha512"`
}

type LocalDeletePreview struct {
	BatchID         string            `msgpack:"batch_id"`
	RunID           string            `msgpack:"run_id"`
	GroupID         string            `msgpack:"group_id"`
	Generation      int64             `msgpack:"generation"`
	Count           int               `msgpack:"count"`
	TotalSize       int64             `msgpack:"total_size"`
	SelectionDigest string            `msgpack:"selection_digest"`
	Token           string            `msgpack:"token"`
	ExpiresAt       int64             `msgpack:"expires_at"`
	Files           []LocalDeleteFile `msgpack:"files"`
}

type LocalDeleteExecuteRequest struct {
	BatchID         string `msgpack:"batch_id"`
	SelectionDigest string `msgpack:"selection_digest"`
	Token           string `msgpack:"token"`
}

func (request LocalDeleteExecuteRequest) Validate() error {
	if request.BatchID == "" || request.SelectionDigest == "" || request.Token == "" ||
		strings.TrimSpace(request.BatchID) != request.BatchID ||
		strings.TrimSpace(request.SelectionDigest) != request.SelectionDigest ||
		strings.TrimSpace(request.Token) != request.Token {
		return errors.New("invalid_delete_execution")
	}
	return nil
}

type LocalDeleteStatusRequest struct {
	BatchID string `msgpack:"batch_id"`
}

func (request LocalDeleteStatusRequest) Validate() error {
	if request.BatchID == "" || strings.TrimSpace(request.BatchID) != request.BatchID {
		return errors.New("invalid_delete_batch")
	}
	return nil
}

type LocalDeleteItem struct {
	FileID    int64  `msgpack:"file_id"`
	Result    string `msgpack:"result"`
	ErrorCode string `msgpack:"error_code,omitempty"`
	Uncertain bool   `msgpack:"uncertain,omitempty"`
}

type LocalDeleteBatch struct {
	BatchID   string            `msgpack:"batch_id"`
	Status    string            `msgpack:"status"`
	Requested int               `msgpack:"requested"`
	Succeeded int               `msgpack:"succeeded"`
	Failed    int               `msgpack:"failed"`
	Uncertain int               `msgpack:"uncertain"`
	Items     []LocalDeleteItem `msgpack:"items"`
}

type LocalImagePreviewRequest struct {
	FileID    int64  `msgpack:"file_id"`
	MaxWidth  int32  `msgpack:"max_width"`
	MaxHeight int32  `msgpack:"max_height"`
	Format    string `msgpack:"format"`
	Quality   int32  `msgpack:"quality"`
	// Sha512 bridges the manager (GUI) channel: the Web side only knows the
	// Postgres files.id, which lives in a different ID space than the Agent's
	// local database. FileID wins when both are set; Sha512 is used only when
	// FileID is zero.
	Sha512 string `msgpack:"sha512,omitempty"`
}

func (request LocalImagePreviewRequest) Validate() error {
	if request.MaxWidth <= 0 || request.MaxWidth > 8192 ||
		request.MaxHeight <= 0 || request.MaxHeight > 8192 ||
		(request.Format != "jpeg" && request.Format != "webp") ||
		request.Quality < 1 || request.Quality > 100 {
		return fmt.Errorf("invalid_preview")
	}
	if request.FileID > 0 {
		return nil
	}
	if !IsSHA512LowerHex(request.Sha512) {
		return fmt.Errorf("invalid_preview")
	}
	return nil
}

// IsSHA512LowerHex reports whether value is a canonical SHA-512 hex digest.
func IsSHA512LowerHex(value string) bool {
	if len(value) != 128 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

type LocalImagePreviewResponse struct {
	MIME   string `msgpack:"mime"`
	Width  int32  `msgpack:"width"`
	Height int32  `msgpack:"height"`
	Bytes  []byte `msgpack:"bytes"`
}

func EncodeLocalPayload(value any) ([]byte, error) {
	if response, ok := value.(LocalImagePreviewResponse); ok && len(response.Bytes) > MaxLocalPreviewEncodedBytes {
		return nil, errors.New(LocalPayloadTooLargeErrorCode)
	}
	payload, err := msgpack.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(payload) > LocalPayloadMaxBytes {
		return nil, errors.New(LocalPayloadTooLargeErrorCode)
	}
	return payload, nil
}

func DecodeLocalPayload(payload []byte, destination any) error {
	if len(payload) > LocalPayloadMaxBytes {
		return errors.New(LocalPayloadTooLargeErrorCode)
	}
	return msgpack.Unmarshal(payload, destination)
}

func DecodeLocalImagePreviewPayload(payload []byte, destination *LocalImagePreviewRequest) error {
	if len(payload) > LocalPayloadMaxBytes {
		return errors.New(LocalPayloadTooLargeErrorCode)
	}
	if destination == nil {
		return errors.New("invalid_preview")
	}
	decoder := msgpack.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields(true)
	return decoder.Decode(destination)
}

func DecodeLocalDeletePayload(payload []byte, destination any) error {
	if len(payload) > LocalPayloadMaxBytes || destination == nil {
		return errors.New("invalid_delete")
	}
	decoder := msgpack.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields(true)
	return decoder.Decode(destination)
}

func (event LocalEvent) Validate() error {
	if len(event.Payload) > LocalPayloadMaxBytes {
		return errors.New(LocalPayloadTooLargeErrorCode)
	}
	if event.Topic == "" || strings.TrimSpace(event.Topic) != event.Topic {
		return errors.New(InvalidLocalTopicErrorCode)
	}
	return nil
}

func IsLocalOperation(operation string) bool {
	switch operation {
	case LocalOperationStatusGet,
		LocalOperationConfigGet,
		LocalOperationConfigValidate,
		LocalOperationConfigSave,
		LocalOperationTaskCreate,
		LocalOperationTaskList,
		LocalOperationTaskCancel,
		LocalOperationTaskRetry,
		LocalOperationAnalysisStart,
		LocalOperationAnalysisStatus,
		LocalOperationGroupsList,
		LocalOperationGroupsDetail,
		LocalOperationReviewSave,
		LocalOperationDeletePrepare,
		LocalOperationDeleteExecute,
		LocalOperationDeleteStatus,
		LocalOperationPreviewImage,
		LocalOperationShutdown:
		return true
	default:
		return false
	}
}
