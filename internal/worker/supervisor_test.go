package worker

import (
	"strings"
	"testing"
)

// Break caught: the parent accepts a forged, oversized, or dimensionless
// preview result instead of binding it to the dispatched immutable identity.
func TestImagePreviewProtocolValidatesIdentityDimensionsAndFourMiBLimit(t *testing.T) {
	job := &JobMsg{
		JobID: 701, ScanTaskID: "preview-701", Path: `D:\media\source.jpg`,
		Kind: MediaImage, Phase: PhasePreview, ScreenStage: ScreenStagePreview,
		Source: JobSourceLocal, Size: 200, MTimeUnix: 300,
		KnownSHA: bytes64(0x71), PreviewFormat: PreviewFormatJPEG,
		PreviewMaxWidth: 640, PreviewMaxHeight: 480, PreviewQuality: 80,
	}
	valid := &JobResultMsg{
		JobID: job.JobID, ScanTaskID: job.ScanTaskID, Path: job.Path,
		Kind: job.Kind, ScreenStage: job.ScreenStage, Source: job.Source,
		SHA512: bytes64(0x71), PreviewFormat: PreviewFormatJPEG,
		PreviewWidth: 320, PreviewHeight: 240,
		PreviewBytes: []byte{0xff, 0xd8, 0xff, 0xd9},
	}
	if err := validateWorkerResult(job, valid); err != nil {
		t.Fatalf("valid preview result: %v", err)
	}
	memoryFailure := *valid
	memoryFailure.PreviewFormat = ""
	memoryFailure.PreviewWidth = 0
	memoryFailure.PreviewHeight = 0
	memoryFailure.PreviewBytes = nil
	memoryFailure.PreviewErrorCode = "preview_memory_limit"
	if err := validateWorkerResult(job, &memoryFailure); err != nil {
		t.Fatalf("stable preview memory failure: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*JobMsg, *JobResultMsg)
	}{
		{"wrong SHA", func(_ *JobMsg, result *JobResultMsg) { result.SHA512[0]++ }},
		{"zero width", func(_ *JobMsg, result *JobResultMsg) { result.PreviewWidth = 0 }},
		{"oversized width", func(_ *JobMsg, result *JobResultMsg) { result.PreviewWidth = 641 }},
		{"wrong format", func(_ *JobMsg, result *JobResultMsg) { result.PreviewFormat = PreviewFormatWebP }},
		{"oversized bytes", func(_ *JobMsg, result *JobResultMsg) { result.PreviewBytes = make([]byte, MaxPreviewBytes+1) }},
		{"path mismatch", func(_ *JobMsg, result *JobResultMsg) { result.Path = `D:\private\other.jpg` }},
		{"invalid requested size", func(job *JobMsg, _ *JobResultMsg) { job.PreviewMaxHeight = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			jobCopy := *job
			jobCopy.KnownSHA = append([]byte(nil), job.KnownSHA...)
			resultCopy := *valid
			resultCopy.SHA512 = append([]byte(nil), valid.SHA512...)
			resultCopy.PreviewBytes = append([]byte(nil), valid.PreviewBytes...)
			test.mutate(&jobCopy, &resultCopy)
			if err := validateWorkerResult(&jobCopy, &resultCopy); err == nil {
				t.Fatal("invalid preview result was accepted")
			} else if strings.Contains(strings.ToLower(err.Error()), "private") || strings.Contains(strings.ToLower(err.Error()), "other.jpg") {
				t.Fatalf("protocol error leaked path: %v", err)
			}
		})
	}
}
