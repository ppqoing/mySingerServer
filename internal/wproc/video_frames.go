package wproc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"dedup/internal/features"
	"dedup/internal/worker"
)

func processPhase2Video(
	ctx context.Context,
	cfg Config,
	job *worker.JobMsg,
	deps phase2PipelineDeps,
	result *worker.JobResultMsg,
) (*worker.JobResultMsg, error) {
	if err := validatePhase2VideoConfig(cfg); err != nil {
		return nil, fmt.Errorf("worker phase-2 video pipeline: invalid config: %w", err)
	}
	if deps.open == nil || deps.stat == nil || deps.sameFile == nil ||
		deps.newHash == nil || deps.decode == nil || deps.runCommand == nil {
		return nil, fmt.Errorf("worker phase-2 video pipeline: dependency is unavailable")
	}

	path := fixPath(job.Path)
	pathBefore, err := deps.stat(path)
	if err != nil {
		phase2FileError(result, "stat", err)
		return result, nil
	}
	if !matchesPhase2Dispatch(pathBefore, job) {
		if err := confirmPhase2KnownSHA(cfg, path, job.KnownSHA, deps); err != nil {
			if errors.Is(err, errPhase2StaleContent) {
				phase2Stale(result, err)
			} else {
				phase2FileError(result, phase2HashErrorStage(err), err)
			}
			return result, nil
		}
	}

	frameMask := job.FrameMask
	if frameMask == 0 {
		frameMask = 0x3f
	}
	times := phase2FrameTimes(job.DurationMS)
	allSixComplete := frameMask == 0x3f
	for frameIdx, timeMS := range times {
		if frameMask&(1<<frameIdx) == 0 {
			continue
		}
		frame := processPhase2Frame(ctx, cfg, path, frameIdx, timeMS, deps)
		if frame.Error != "" {
			allSixComplete = false
		}
		result.Frames = append(result.Frames, frame)
	}
	if allSixComplete && len(result.Frames) == 6 {
		result.FieldsDone |= worker.MaskVideo6F
	}

	pathAfter, err := deps.stat(path)
	if err != nil {
		phase2FileError(result, "stat", err)
		return result, nil
	}
	if !deps.sameFile(pathBefore, pathAfter) ||
		!samePhase2FileState(pathBefore, pathAfter) {
		phase2Stale(result, fmt.Errorf("video changed during frame extraction"))
	}
	return result, nil
}

var errPhase2StaleContent = errors.New("phase-2 source content is stale")

type phase2HashStageError struct {
	stage string
	err   error
}

func (e *phase2HashStageError) Error() string { return e.err.Error() }
func (e *phase2HashStageError) Unwrap() error { return e.err }

func confirmPhase2KnownSHA(
	cfg Config,
	path string,
	knownSHA []byte,
	deps phase2PipelineDeps,
) error {
	file, err := deps.open(path)
	if err != nil {
		return &phase2HashStageError{stage: "open", err: err}
	}
	defer file.Close()
	hasher, err := deps.newHash()
	if err != nil {
		return &phase2HashStageError{stage: "hash", err: err}
	}
	chunk := make([]byte, cfg.ReadChunkBytes)
	for {
		n, readErr := file.Read(chunk)
		if n > 0 {
			if _, err := hasher.Write(chunk[:n]); err != nil {
				return &phase2HashStageError{stage: "hash", err: err}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return &phase2HashStageError{stage: "hash", err: readErr}
		}
		if n == 0 {
			return &phase2HashStageError{stage: "hash", err: io.ErrNoProgress}
		}
	}
	if !bytes.Equal(hasher.Sum(nil), knownSHA) {
		return fmt.Errorf("%w: file no longer matches known SHA-512", errPhase2StaleContent)
	}
	return nil
}

func phase2HashErrorStage(err error) string {
	var staged *phase2HashStageError
	if errors.As(err, &staged) {
		return staged.stage
	}
	return "hash"
}

func phase2FrameTimes(durationMS int64) [6]int64 {
	var times [6]int64
	quotient, remainder := durationMS/12, durationMS%12
	for i := range times {
		multiplier := int64(2*i + 1)
		times[i] = quotient*multiplier + remainder*multiplier/12
	}
	return times
}

func formatFrameTimeMS(timeMS int64) string {
	return fmt.Sprintf("%d.%03d", timeMS/1000, timeMS%1000)
}

func processPhase2Frame(
	parent context.Context,
	cfg Config,
	path string,
	frameIdx int,
	timeMS int64,
	deps phase2PipelineDeps,
) worker.FrameFeature {
	frame := worker.FrameFeature{FrameIdx: frameIdx, TimeMS: timeMS}
	args := []string{
		"-nostdin",
		"-hide_banner",
		"-loglevel", "error",
		"-i", path,
		"-ss", formatFrameTimeMS(timeMS),
		"-frames:v", "1",
		"-vf", fmt.Sprintf(
			"scale=%d:%d:force_original_aspect_ratio=decrease,format=gray",
			cfg.Phase2FrameMaxSide, cfg.Phase2FrameMaxSide,
		),
		"-f", "image2pipe",
		"-vcodec", "png",
		"pipe:1",
	}
	stdout := &phase2BoundedBuffer{limit: cfg.IPCMaxFrameBytes}
	stderr := &phase2BoundedBuffer{limit: 64 << 10}
	commandCtx, cancel := context.WithTimeout(parent, cfg.Phase2FrameTimeout)
	runErr := deps.runCommand(commandCtx, cfg.FFmpegPath, args, stdout, stderr)
	contextErr := commandCtx.Err()
	cancel()
	if contextErr != nil {
		frame.Error = "ffmpeg: " + contextErr.Error()
		if detail := strings.TrimSpace(string(stderr.bytes)); detail != "" {
			frame.Error += ": " + detail
		}
		return frame
	}
	if stdout.exceeded {
		frame.Error = fmt.Sprintf("ffmpeg stdout too large (maximum %d bytes)", cfg.IPCMaxFrameBytes)
		return frame
	}
	if runErr != nil {
		frame.Error = "ffmpeg: " + runErr.Error()
		if detail := strings.TrimSpace(string(stderr.bytes)); detail != "" {
			frame.Error += ": " + detail
		}
		return frame
	}
	if len(stdout.bytes) == 0 {
		frame.Error = "ffmpeg returned empty stdout"
		return frame
	}

	gray, err := deps.decode(stdout.bytes)
	if err != nil {
		frame.Error = "decode: " + err.Error()
		return frame
	}
	defer gray.Free()
	pdq, quality, pdqErr := gray.PDQ256()
	combined, phase2Err := gray.Phase2()
	if pdqErr != nil {
		frame.Error = "pdq: " + pdqErr.Error()
		return frame
	}
	if phase2Err != nil {
		frame.Error = "phase2: " + phase2Err.Error()
		return frame
	}
	sobel, err := features.EncodeSobelHist(combined.SobelHist)
	if err != nil {
		frame.Error = "encode: " + err.Error()
		return frame
	}
	frame.PDQ256 = append([]byte(nil), pdq[:]...)
	frame.Quality = quality
	frame.PHashParts = features.EncodePHashParts(combined.PHashParts)
	frame.SobelHist = sobel
	return frame
}

func runPhase2Command(
	ctx context.Context,
	path string,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) error {
	command := exec.CommandContext(ctx, path, args...)
	command.Stdout = stdout
	command.Stderr = stderr
	return command.Run()
}

type phase2BoundedBuffer struct {
	bytes    []byte
	limit    int
	exceeded bool
}

func (b *phase2BoundedBuffer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	remaining := b.limit - len(b.bytes)
	if remaining <= 0 {
		b.exceeded = true
		return 0, fmt.Errorf("phase-2 command output exceeds %d bytes", b.limit)
	}
	if len(p) > remaining {
		b.bytes = append(b.bytes, p[:remaining]...)
		b.exceeded = true
		return remaining, fmt.Errorf("phase-2 command output exceeds %d bytes", b.limit)
	}
	b.bytes = append(b.bytes, p...)
	return len(p), nil
}

func validatePhase2VideoConfig(cfg Config) error {
	if err := validatePhase2ImageConfig(cfg); err != nil {
		return err
	}
	if cfg.Phase2FrameTimeout <= 0 ||
		cfg.Phase2FrameTimeout > time.Duration(maxTimeoutSeconds)*time.Second {
		return fmt.Errorf("phase-2 frame timeout is out of range")
	}
	if cfg.Phase2FrameMaxSide <= 0 ||
		int64(cfg.Phase2FrameMaxSide) > maxThumbSide {
		return fmt.Errorf("phase-2 frame max side is out of range")
	}
	const maxIPCFrameBytes = 16 << 20
	if cfg.IPCMaxFrameBytes <= 0 || cfg.IPCMaxFrameBytes > maxIPCFrameBytes {
		return fmt.Errorf("IPC maximum frame bytes must be between 1 and %d", maxIPCFrameBytes)
	}
	return nil
}
