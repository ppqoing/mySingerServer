package localpreview

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"

	"dedup/internal/proto"
	"dedup/internal/store"
	"dedup/internal/worker"
)

var ErrUnavailable = errors.New("local_preview_unavailable")

type SourceStore interface {
	LoadLocalPreviewSource(context.Context, string, int64) (store.LocalPreviewSource, error)
}

type Executor interface {
	Execute(context.Context, *worker.JobMsg) (*worker.JobResultMsg, error)
}

type Service struct {
	machineID string
	store     SourceStore
	executor  Executor
}

func NewService(machineID string, backend SourceStore, executor Executor) *Service {
	return &Service{machineID: machineID, store: backend, executor: executor}
}

// Preview resolves the caller's opaque file ID inside Agent and dispatches the
// canonical path only over the private Worker IPC boundary.
func (service *Service) Preview(ctx context.Context, request proto.LocalImagePreviewRequest) (proto.LocalImagePreviewResponse, error) {
	if service == nil || service.store == nil || service.executor == nil ||
		service.machineID == "" || ctx == nil {
		return proto.LocalImagePreviewResponse{}, ErrUnavailable
	}
	if err := request.Validate(); err != nil {
		return proto.LocalImagePreviewResponse{}, err
	}
	source, err := service.store.LoadLocalPreviewSource(ctx, service.machineID, request.FileID)
	if err != nil {
		return proto.LocalImagePreviewResponse{}, err
	}
	if source.FileID != request.FileID || source.MachineID != service.machineID ||
		source.Path == "" || source.Kind != "image" || source.Status == "deleted" ||
		source.Size < 0 || source.MTime <= 0 {
		return proto.LocalImagePreviewResponse{}, errors.New("preview_not_available")
	}
	sha, err := hex.DecodeString(source.SHA512)
	if err != nil || len(sha) != sha512.Size {
		return proto.LocalImagePreviewResponse{}, errors.New("preview_not_available")
	}
	job := &worker.JobMsg{
		ScanTaskID: "local-preview", Path: source.Path, Kind: worker.MediaImage,
		Phase: worker.PhasePreview, ScreenStage: worker.ScreenStagePreview,
		Source: worker.JobSourceLocal, Size: source.Size, MTimeUnix: source.MTime,
		KnownSHA: append([]byte(nil), sha...), PreviewFormat: request.Format,
		PreviewMaxWidth: request.MaxWidth, PreviewMaxHeight: request.MaxHeight,
		PreviewQuality: request.Quality,
	}
	result, err := service.executor.Execute(ctx, job)
	if err != nil || result == nil {
		return proto.LocalImagePreviewResponse{}, errors.New("preview_failed")
	}
	if result.PreviewErrorCode != "" {
		return proto.LocalImagePreviewResponse{}, errors.New(result.PreviewErrorCode)
	}
	if result.Path != source.Path || result.Kind != worker.MediaImage ||
		len(result.SHA512) != sha512.Size || !bytes.Equal(result.SHA512, sha) ||
		result.PreviewFormat != request.Format || result.PreviewWidth <= 0 ||
		result.PreviewHeight <= 0 || result.PreviewWidth > request.MaxWidth ||
		result.PreviewHeight > request.MaxHeight || len(result.PreviewBytes) == 0 ||
		len(result.PreviewBytes) > worker.MaxPreviewBytes {
		return proto.LocalImagePreviewResponse{}, errors.New("preview_invalid_result")
	}
	mime := "image/jpeg"
	if result.PreviewFormat == worker.PreviewFormatWebP {
		mime = "image/webp"
	}
	return proto.LocalImagePreviewResponse{
		MIME: mime, Width: result.PreviewWidth, Height: result.PreviewHeight,
		Bytes: append([]byte(nil), result.PreviewBytes...),
	}, nil
}
