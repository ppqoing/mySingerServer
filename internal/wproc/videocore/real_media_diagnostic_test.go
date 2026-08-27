//go:build cgo && windows

package videocore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dedup/internal/worker"
)

func TestRealMediaContactSheetCompletesWithinProductionFrameDeadline(t *testing.T) {
	sample := os.Getenv("VC_REAL_MEDIA_SAMPLE")
	if sample == "" {
		t.Skip("VC_REAL_MEDIA_SAMPLE is not set")
	}
	info, err := os.Stat(sample)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("real media sample stat = %v, err=%v", info, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	session, err := Open(ctx, sample, OpenOptions{
		Kind:          worker.MediaVideo,
		NativeTimeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := session.Close(); err != nil {
			t.Errorf("close real media session: %v", err)
		}
	}()
	if _, err := session.Hash(); err != nil {
		t.Fatalf("hash real media sample: %v", err)
	}

	result, err := session.Analyze(ctx, AnalysisRequest{
		Fields:       worker.MaskVideoDuration | worker.MaskVideoContactSheet,
		ProbeTimeout: 15 * time.Second,
		FrameTimeout: 20 * time.Second,
		TileMaxSide:  256,
		TempJPEGPath: filepath.Join(t.TempDir(), "contact-sheet.jpg"),
	})
	if err != nil {
		t.Fatalf("analyze real media sample: %v", err)
	}
	if result.DurationStatus != StatusOK ||
		result.ContactSheetStatus != StatusOK ||
		result.CompletedFrameMask != 0x3f {
		t.Fatalf("real media result = duration:%d contact:%d frames:%#x elapsed:%d/%dms",
			result.DurationStatus, result.ContactSheetStatus,
			result.CompletedFrameMask, result.OperationElapsedMS,
			result.DecodeElapsedMS)
	}
}
