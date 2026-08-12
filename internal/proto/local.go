package proto

import (
	"errors"
	"strings"

	"github.com/vmihailenco/msgpack/v5"

	"dedup/internal/nodectl"
)

const LocalPayloadMaxBytes = 4 * 1024 * 1024

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

func EncodeLocalPayload(value any) ([]byte, error) {
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
