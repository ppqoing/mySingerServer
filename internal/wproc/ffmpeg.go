package wproc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type commandRunner interface {
	Run(context.Context, string, []string) (stdout, stderr []byte, err error)
}

type execCommandRunner struct{}

type ffmpegFileOps struct {
	createTemp func(string, string) (*os.File, error)
	remove     func(string) error
}

var defaultFFmpegFileOps = ffmpegFileOps{
	createTemp: os.CreateTemp,
	remove:     os.Remove,
}

func (execCommandRunner) Run(ctx context.Context, name string, args []string) ([]byte, []byte, error) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command := exec.CommandContext(ctx, name, args...)
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func resolveFFmpegTools(cfg Config, executable string) (string, string, error) {
	base := filepath.Dir(executable)
	resolve := func(path, label string) (string, error) {
		if path == "" {
			return "", fmt.Errorf("%s path is empty", label)
		}
		if filepath.IsAbs(path) {
			return filepath.Clean(path), nil
		}
		return filepath.Clean(filepath.Join(base, path)), nil
	}
	probe, err := resolve(cfg.FFprobePath, "ffprobe")
	if err != nil {
		return "", "", err
	}
	ffmpeg, err := resolve(cfg.FFmpegPath, "ffmpeg")
	if err != nil {
		return "", "", err
	}
	return probe, ffmpeg, nil
}

func ffprobeDuration(parent context.Context, cfg Config, path string, runner commandRunner) (int64, error) {
	if runner == nil {
		runner = execCommandRunner{}
	}
	ctx, cancel := context.WithTimeout(parent, cfg.FFprobeTimeout)
	defer cancel()
	args := []string{
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path,
	}
	stdout, stderr, err := runner.Run(ctx, cfg.FFprobePath, args)
	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return 0, fmt.Errorf("ffprobe timeout after %s: %w", cfg.FFprobeTimeout, ctxErr)
		}
		return 0, fmt.Errorf("ffprobe cancelled: %w", ctxErr)
	}
	if err != nil {
		message := strings.TrimSpace(string(stderr))
		if message == "" {
			return 0, fmt.Errorf("ffprobe: %w", err)
		}
		return 0, fmt.Errorf("ffprobe: %w: %s", err, message)
	}
	raw := strings.TrimSpace(string(stdout))
	seconds, parseErr := strconv.ParseFloat(raw, 64)
	if parseErr != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
		return 0, fmt.Errorf("ffprobe duration %q must be finite and positive", raw)
	}
	milliseconds := seconds * 1000
	if milliseconds > math.MaxInt64 {
		return 0, fmt.Errorf("ffprobe duration %q exceeds millisecond range", raw)
	}
	return int64(math.Round(milliseconds)), nil
}

func ffmpegShot(parent context.Context, cfg Config, source string, seekSeconds float64, destination string, runner commandRunner) error {
	_, err := ffmpegShotWithDigest(parent, cfg, source, seekSeconds, destination, runner)
	return err
}

func ffmpegShotWithDigest(parent context.Context, cfg Config, source string, seekSeconds float64, destination string, runner commandRunner) (string, error) {
	return ffmpegShotWithFileOps(parent, cfg, source, seekSeconds, destination, runner, defaultFFmpegFileOps)
}

func ffmpegShotWithFileOps(parent context.Context, cfg Config, source string, seekSeconds float64, destination string, runner commandRunner, fileOps ffmpegFileOps) (string, error) {
	if runner == nil {
		runner = execCommandRunner{}
	}
	if fileOps.createTemp == nil || fileOps.remove == nil {
		return "", fmt.Errorf("ffmpeg file operations are unavailable")
	}
	if !isFiniteNonNegative(seekSeconds) {
		return "", fmt.Errorf("ffmpeg seek must be finite and non-negative")
	}
	if cfg.ThumbMaxSide <= 0 {
		return "", fmt.Errorf("thumbnail maximum side must be positive")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return "", fmt.Errorf("create thumbnail directory: %w", err)
	}
	tempFile, err := fileOps.createTemp(filepath.Dir(destination), filepath.Base(destination)+".tmp-*.jpg")
	if err != nil {
		return "", fmt.Errorf("create thumbnail temp: %w", err)
	}
	tempPath := tempFile.Name()
	defer fileOps.remove(tempPath)
	if err := tempFile.Close(); err != nil {
		return "", fmt.Errorf("close thumbnail temp: %w", err)
	}
	if err := fileOps.remove(tempPath); err != nil {
		return "", fmt.Errorf("prepare thumbnail temp: %w", err)
	}
	ctx, cancel := context.WithTimeout(parent, cfg.FFmpegTimeout)
	defer cancel()
	filter := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,format=gray", cfg.ThumbMaxSide, cfg.ThumbMaxSide)
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-ss", strconv.FormatFloat(seekSeconds, 'f', 3, 64),
		"-i", source,
		"-frames:v", "1",
		"-an", "-sn", "-dn",
		"-vf", filter,
		"-q:v", "3",
		"-f", "image2",
		"-y", tempPath,
	}
	_, stderr, err := runner.Run(ctx, cfg.FFmpegPath, args)
	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return "", fmt.Errorf("ffmpeg timeout after %s: %w", cfg.FFmpegTimeout, ctxErr)
		}
		return "", fmt.Errorf("ffmpeg cancelled: %w", ctxErr)
	}
	if err != nil {
		message := strings.TrimSpace(string(stderr))
		if message == "" {
			return "", fmt.Errorf("ffmpeg: %w", err)
		}
		return "", fmt.Errorf("ffmpeg: %w: %s", err, message)
	}
	output, err := os.OpenFile(tempPath, os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("open generated thumbnail: %w", err)
	}
	info, statErr := output.Stat()
	if statErr != nil {
		_ = output.Close()
		return "", fmt.Errorf("stat generated thumbnail: %w", statErr)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		_ = output.Close()
		return "", fmt.Errorf("ffmpeg produced no thumbnail")
	}
	if err := output.Sync(); err != nil {
		_ = output.Close()
		return "", fmt.Errorf("sync generated thumbnail: %w", err)
	}
	if err := output.Close(); err != nil {
		return "", fmt.Errorf("close generated thumbnail: %w", err)
	}
	digest, err := fileSHA256Hex(tempPath)
	if err != nil {
		return "", fmt.Errorf("hash generated thumbnail: %w", err)
	}
	if err := atomicReplace(tempPath, destination); err != nil {
		return "", fmt.Errorf("commit thumbnail: %w", err)
	}
	return digest, nil
}

func isFiniteNonNegative(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}
