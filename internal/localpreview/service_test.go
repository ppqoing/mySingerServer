package localpreview

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"testing"

	"dedup/internal/proto"
	"dedup/internal/store"
	"dedup/internal/worker"
)

func TestImagePreviewServiceBuildsWorkerJobOnlyFromOwnedActiveDatabaseFile(t *testing.T) {
	sha := bytes64Preview(0x91)
	backend := &previewSourceStoreFake{source: store.LocalPreviewSource{
		FileID: 91, MachineID: "machine-a", Path: `D:\media\owned.jpg`,
		Kind: "image", Status: "done", SHA512: hex.EncodeToString(sha),
		Size: 123, MTime: 456,
	}}
	executor := &previewWorkerFake{result: &worker.JobResultMsg{
		Path: backend.source.Path, Kind: worker.MediaImage, SHA512: append([]byte(nil), sha...),
		PreviewFormat: worker.PreviewFormatWebP, PreviewWidth: 80, PreviewHeight: 60,
		PreviewBytes: []byte{1, 2, 3},
	}}
	service := NewService("machine-a", backend, executor)
	response, err := service.Preview(context.Background(), proto.LocalImagePreviewRequest{
		FileID: 91, MaxWidth: 100, MaxHeight: 100, Format: "webp", Quality: 82,
	})
	if err != nil {
		t.Fatal(err)
	}
	if backend.machineID != "machine-a" || backend.fileID != 91 ||
		executor.job.Path != backend.source.Path || executor.job.Phase != worker.PhasePreview ||
		executor.job.Size != 123 || executor.job.MTimeUnix != 456 ||
		!bytes.Equal(executor.job.KnownSHA, sha) {
		t.Fatalf("source/job = %#v / %#v", backend, executor.job)
	}
	if response.MIME != "image/webp" || response.Width != 80 || response.Height != 60 || len(response.Bytes) != 3 {
		t.Fatalf("response = %#v", response)
	}
}

func TestImagePreviewServiceRejectsUnsafeDatabaseRowsBeforeWorker(t *testing.T) {
	valid := store.LocalPreviewSource{
		FileID: 92, MachineID: "machine-a", Path: `D:\private\source.jpg`,
		Kind: "image", Status: "done", SHA512: hex.EncodeToString(bytes64Preview(0x92)),
		Size: 1, MTime: 2,
	}
	tests := []struct {
		name   string
		mutate func(*store.LocalPreviewSource)
	}{
		{"cross machine", func(source *store.LocalPreviewSource) { source.MachineID = "machine-b" }},
		{"deleted", func(source *store.LocalPreviewSource) { source.Status = "deleted" }},
		{"video", func(source *store.LocalPreviewSource) { source.Kind = "video" }},
		{"missing SHA", func(source *store.LocalPreviewSource) { source.SHA512 = "" }},
		{"bad SHA", func(source *store.LocalPreviewSource) { source.SHA512 = "xyz" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := valid
			test.mutate(&source)
			backend := &previewSourceStoreFake{source: source}
			executor := &previewWorkerFake{}
			service := NewService("machine-a", backend, executor)
			_, err := service.Preview(context.Background(), proto.LocalImagePreviewRequest{
				FileID: 92, MaxWidth: 10, MaxHeight: 10, Format: "jpeg", Quality: 80,
			})
			if err == nil || executor.calls != 0 {
				t.Fatalf("unsafe source reached worker: err=%v calls=%d", err, executor.calls)
			}
			if bytes.Contains([]byte(err.Error()), []byte("private")) || bytes.Contains([]byte(err.Error()), []byte("source.jpg")) {
				t.Fatalf("safe error leaked path: %v", err)
			}
		})
	}
}

func TestStalePreviewServiceRejectsWorkerPayloadMismatch(t *testing.T) {
	sha := bytes64Preview(0x93)
	source := store.LocalPreviewSource{
		FileID: 93, MachineID: "machine-a", Path: `D:\media\source.jpg`,
		Kind: "image", Status: "partial", SHA512: hex.EncodeToString(sha), Size: 10, MTime: 20,
	}
	base := &worker.JobResultMsg{
		Path: source.Path, Kind: worker.MediaImage, SHA512: append([]byte(nil), sha...),
		PreviewFormat: worker.PreviewFormatJPEG, PreviewWidth: 10, PreviewHeight: 10,
		PreviewBytes: []byte{1},
	}
	tests := []struct {
		name   string
		mutate func(*worker.JobResultMsg)
	}{
		{"SHA", func(result *worker.JobResultMsg) { result.SHA512[0]++ }},
		{"dimension", func(result *worker.JobResultMsg) { result.PreviewWidth = 0 }},
		{"format", func(result *worker.JobResultMsg) { result.PreviewFormat = worker.PreviewFormatWebP }},
		{"size", func(result *worker.JobResultMsg) { result.PreviewBytes = make([]byte, worker.MaxPreviewBytes+1) }},
		{"worker stale", func(result *worker.JobResultMsg) {
			result.PreviewErrorCode = "stale_preview"
			result.PreviewBytes = nil
			result.PreviewFormat = ""
			result.PreviewWidth = 0
			result.PreviewHeight = 0
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := *base
			result.SHA512 = append([]byte(nil), base.SHA512...)
			result.PreviewBytes = append([]byte(nil), base.PreviewBytes...)
			test.mutate(&result)
			service := NewService("machine-a", &previewSourceStoreFake{source: source}, &previewWorkerFake{result: &result})
			if _, err := service.Preview(context.Background(), proto.LocalImagePreviewRequest{
				FileID: 93, MaxWidth: 10, MaxHeight: 10, Format: "jpeg", Quality: 80,
			}); err == nil {
				t.Fatal("invalid worker preview was accepted")
			}
		})
	}
}

type previewSourceStoreFake struct {
	source    store.LocalPreviewSource
	err       error
	machineID string
	fileID    int64
}

func (fake *previewSourceStoreFake) LoadLocalPreviewSource(_ context.Context, machineID string, fileID int64) (store.LocalPreviewSource, error) {
	fake.machineID, fake.fileID = machineID, fileID
	return fake.source, fake.err
}

type previewWorkerFake struct {
	job    worker.JobMsg
	result *worker.JobResultMsg
	err    error
	calls  int
}

func (fake *previewWorkerFake) Execute(_ context.Context, job *worker.JobMsg) (*worker.JobResultMsg, error) {
	fake.calls++
	if job == nil {
		return nil, errors.New("nil job")
	}
	fake.job = *job
	fake.job.KnownSHA = append([]byte(nil), job.KnownSHA...)
	return fake.result, fake.err
}

func bytes64Preview(value byte) []byte {
	data := make([]byte, sha512.Size)
	for index := range data {
		data[index] = value
	}
	return data
}
