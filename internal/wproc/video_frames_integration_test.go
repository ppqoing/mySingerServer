//go:build cgo && windows

package wproc

import (
	"context"
	"crypto/sha512"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"dedup/internal/worker"
)

func TestPhase2VideoRealFFmpegAndMediaCore(t *testing.T) {
	ffmpeg := os.Getenv("WPROC_TEST_REAL_FFMPEG")
	if ffmpeg == "" {
		t.Skip("WPROC_TEST_REAL_FFMPEG is not set")
	}
	generated := filepath.Join(t.TempDir(), "phase2-generated.mp4")
	generateRealPhase2Video(t, ffmpeg, generated)
	assertRealPhase2Video(t, ffmpeg, generated, 3000)
}

func TestPhase2VideoRealLongPathFFmpeg(t *testing.T) {
	ffmpeg := os.Getenv("WPROC_TEST_REAL_FFMPEG")
	if ffmpeg == "" {
		t.Skip("WPROC_TEST_REAL_FFMPEG is not set")
	}
	root := t.TempDir()
	shortPath := filepath.Join(root, "phase2-short.mp4")
	generateRealPhase2Video(t, ffmpeg, shortPath)
	longDir := filepath.Join(root, "media folder")
	for range 18 {
		longDir = filepath.Join(longDir, "long segment")
	}
	longPath := filepath.Join(longDir, "phase2 long video.mp4")
	if len(longPath) < longPathThreshold {
		t.Fatalf("long-path fixture length = %d, want at least %d", len(longPath), longPathThreshold)
	}
	if err := os.MkdirAll(fixPath(longDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(shortPath, fixPath(longPath)); err != nil {
		t.Fatal(err)
	}
	assertRealPhase2Video(t, ffmpeg, longPath, 3000)
}

func generateRealPhase2Video(t *testing.T, ffmpeg, generated string) {
	t.Helper()
	command := exec.Command(
		ffmpeg,
		"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=12",
		"-t", "3", "-c:v", "libx264", "-pix_fmt", "yuv420p",
		generated,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generate H.264 fixture: %v: %s", err, output)
	}
}

func TestPhase2VideoReadOnlyRealSample(t *testing.T) {
	ffmpeg := os.Getenv("WPROC_TEST_REAL_FFMPEG")
	sample := os.Getenv("WPROC_TEST_REAL_SAMPLE")
	if ffmpeg == "" || sample == "" {
		t.Skip("WPROC_TEST_REAL_FFMPEG or WPROC_TEST_REAL_SAMPLE is not set")
	}
	durationMS, err := strconv.ParseInt(os.Getenv("WPROC_TEST_REAL_SAMPLE_DURATION_MS"), 10, 64)
	if err != nil || durationMS <= 0 {
		t.Fatalf("WPROC_TEST_REAL_SAMPLE_DURATION_MS must be positive: %q",
			os.Getenv("WPROC_TEST_REAL_SAMPLE_DURATION_MS"))
	}
	assertRealPhase2Video(t, ffmpeg, sample, durationMS)
}

func assertRealPhase2Video(t *testing.T, ffmpeg, path string, durationMS int64) {
	t.Helper()
	fixedPath := fixPath(path)
	info, err := os.Stat(fixedPath)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(fixedPath)
	if err != nil {
		t.Fatal(err)
	}
	hasher := sha512.New()
	_, copyErr := io.Copy(hasher, file)
	closeErr := file.Close()
	if copyErr != nil {
		t.Fatal(copyErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	cfg := Config{
		ReadChunkBytes:     4 << 20,
		ImageMemBytes:      256 << 20,
		FFmpegPath:         ffmpeg,
		Phase2FrameTimeout: 20 * time.Second,
		Phase2FrameMaxSide: 512,
		IPCMaxFrameBytes:   16 << 20,
	}
	job := &worker.JobMsg{
		JobID: 701, Path: path, Kind: worker.MediaVideo, Phase: worker.Phase2,
		FieldsMask: worker.MaskVideo6F, Size: info.Size(),
		MTimeMS: info.ModTime().UnixMilli(), KnownSHA: hasher.Sum(nil),
		DurationMS: durationMS,
	}
	result, err := processPhase2WithDeps(
		context.Background(), cfg, job, defaultPhase2PipelineDeps(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.FieldsDone != worker.MaskVideo6F || len(result.Frames) != 6 ||
		len(result.Errors) != 0 {
		t.Fatalf("real phase-2 video result = %#v", result)
	}
	if result.Path != path || strings.HasPrefix(result.Path, `\\?\`) {
		t.Fatalf("protocol path = %q, want original %q", result.Path, path)
	}
	for i, frame := range result.Frames {
		if frame.FrameIdx != i || frame.Error != "" ||
			len(frame.PDQ256) != 32 || len(frame.PHashParts) != 76 ||
			len(frame.SobelHist) != 516 {
			t.Fatalf("real frame %d = %#v", i, frame)
		}
	}
}
