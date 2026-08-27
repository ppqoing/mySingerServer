package wproc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"dedup/internal/worker"
	"dedup/internal/wproc/mediacore"
)

type videoPipelineDeps struct {
	open        func(string) (readStatCloser, error)
	stat        func(string) (os.FileInfo, error)
	sameFile    func(os.FileInfo, os.FileInfo) bool
	newSHA      func() (sha512Stream, error)
	query       func(*worker.SHAQueryMsg) (*worker.SHAReplyMsg, error)
	probe       func(context.Context, Config, string) (int64, error)
	cache       func(Config, [64]byte) (ContactSheetJPEG, bool, error)
	paths       func(Config, [64]byte, int, int64, string) (ContactSheetPaths, error)
	generate    func(context.Context, Config, string, float64, string) (string, error)
	publish     func(ContactSheetPaths, func() error) error
	readThumb   func(string) ([]byte, error)
	decodeThumb func([]byte) (imagePhase1, error)
	pid         func() int
	nonce       func() string
}

func defaultVideoPipelineDeps(query func(*worker.SHAQueryMsg) (*worker.SHAReplyMsg, error)) videoPipelineDeps {
	runner := execCommandRunner{}
	return videoPipelineDeps{
		open: func(path string) (readStatCloser, error) {
			return os.Open(path)
		},
		stat:     os.Stat,
		sameFile: os.SameFile,
		newSHA: func() (sha512Stream, error) {
			return mediacore.NewSHA512()
		},
		query: query,
		probe: func(ctx context.Context, cfg Config, path string) (int64, error) {
			return ffprobeDuration(ctx, cfg, path, runner)
		},
		cache: func(cfg Config, sha [64]byte) (ContactSheetJPEG, bool, error) {
			return lookupContactSheet(cfg.ThumbCacheDir, sha)
		},
		paths: func(cfg Config, sha [64]byte, pid int, jobID int64, nonce string) (ContactSheetPaths, error) {
			return contactSheetPaths(cfg.ThumbCacheDir, sha, pid, jobID, nonce)
		},
		generate: func(ctx context.Context, cfg Config, source string, seek float64, destination string) (string, error) {
			return ffmpegRGBShotWithDigest(ctx, cfg, source, seek, destination, runner)
		},
		publish:   publishContactSheet,
		readThumb: os.ReadFile,
		decodeThumb: func(data []byte) (imagePhase1, error) {
			result, err := mediacore.ImagePhase1(data)
			if err != nil {
				return imagePhase1{}, err
			}
			return imagePhase1{
				Hash:    append([]byte(nil), result.Hash[:]...),
				Quality: result.Quality,
				Width:   result.Width,
				Height:  result.Height,
			}, nil
		},
		pid:   os.Getpid,
		nonce: func() string { return strconv.FormatInt(time.Now().UnixNano(), 36) },
	}
}

func processVideoWithDeps(ctx context.Context, cfg Config, job *worker.JobMsg, deps videoPipelineDeps) (*worker.JobResultMsg, error) {
	result := &worker.JobResultMsg{
		JobID: job.JobID,
		Path:  job.Path,
		Kind:  worker.MediaVideo,
	}
	if err := validateVideoPipelineConfig(cfg); err != nil {
		return nil, fmt.Errorf("worker video pipeline: invalid config: %w", err)
	}
	if job.Size < 0 {
		appendFieldError(result, job.FieldsMask, "size", fmt.Errorf("file size must not be negative"))
		return result, nil
	}
	if err := validateVideoDeps(deps); err != nil {
		return nil, err
	}

	sourceInfo, err := readAndHashVideo(cfg, job, deps, result)
	if err != nil {
		return nil, err
	}
	if len(result.Errors) != 0 {
		return result, nil
	}
	if job.FieldsMask&worker.MaskVideoThumb == 0 {
		return result, nil
	}
	if len(result.SHA512) != mediacore.SHA512Bytes {
		appendFieldError(result, worker.MaskVideoThumb, "sha512", fmt.Errorf("SHA-512 must be %d bytes", mediacore.SHA512Bytes))
		return result, nil
	}

	reply, err := deps.query(&worker.SHAQueryMsg{
		JobID:  job.JobID,
		SHA512: append([]byte(nil), result.SHA512...),
		Kind:   worker.MediaVideo,
	})
	if err != nil {
		return nil, fmt.Errorf("worker video pipeline: SHA query: %w", err)
	}
	if reply == nil || reply.JobID != job.JobID {
		return nil, fmt.Errorf("worker video pipeline: incompatible SHA reply")
	}
	var sha [64]byte
	copy(sha[:], result.SHA512)
	var cached ContactSheetJPEG
	hit := false
	if reply.Found {
		if err := validateCachedVideoReply(reply); err != nil {
			return nil, fmt.Errorf("worker video pipeline: incompatible SHA reply: %w", err)
		}
		cached, hit, err = deps.cache(cfg, sha)
		if err != nil {
			appendFieldError(result, worker.MaskVideoThumb, "thumb_cache", err)
			return result, nil
		}
		result.ThumbCacheHit = hit
		duration := *reply.DurationMS
		result.DurationMS = &duration
		if hit && len(reply.ThumbPDQ) == mediacore.PDQ256Bytes && reply.ThumbQuality != nil && *reply.ThumbQuality >= 0 && *reply.ThumbQuality <= 100 {
			quality := *reply.ThumbQuality
			result.ThumbPath = cached.Path
			result.ThumbPDQ = append([]byte(nil), reply.ThumbPDQ...)
			result.ThumbQuality = &quality
			result.FieldsDone |= worker.MaskVideoThumb
			return result, nil
		}
	}

	duration := int64(0)
	durationErr := error(nil)
	if reply.Found {
		duration = *reply.DurationMS
	} else {
		duration, durationErr = deps.probe(ctx, cfg, fixPath(job.Path))
	}
	seek := 0.0
	if durationErr != nil {
		appendFieldError(result, worker.MaskVideoThumb, "ffprobe", durationErr)
	} else {
		durationValue := duration
		result.DurationMS = &durationValue
		result.FieldsDone |= worker.MaskVideoThumb
		seek = float64(duration) / 2000
	}
	if !reply.Found {
		cached, hit, err = deps.cache(cfg, sha)
		if err != nil {
			appendFieldError(result, worker.MaskVideoThumb, "thumb_cache", err)
			return result, nil
		}
		result.ThumbCacheHit = hit
	}

	if !hit {
		paths, pathErr := deps.paths(cfg, sha, deps.pid(), job.JobID, deps.nonce())
		if pathErr != nil {
			appendFieldError(result, worker.MaskVideoThumb, "thumb_cache", pathErr)
			return result, nil
		}
		defer func() { _ = os.Remove(paths.TempJPEG) }()
		started := time.Now()
		_, err = deps.generate(ctx, cfg, fixPath(job.Path), seek, paths.TempJPEG)
		if err != nil {
			appendFieldError(result, worker.MaskVideoThumb, "ffmpeg", err)
			return result, nil
		}
		result.ThumbMS = time.Since(started).Milliseconds()
		if err := deps.publish(paths, func() error { return videoSourceDrifted(deps, job, sourceInfo) }); err != nil {
			if errors.Is(err, errVideoSourceDrifted) {
				invalidateVideoResultForDrift(result, job, err)
				return result, nil
			}
			appendFieldError(result, worker.MaskVideoThumb, "thumb_cache", err)
			return result, nil
		}
		result.ThumbGenerated = true
		cached = ContactSheetJPEG{Path: paths.JPEG}
	}
	if err := videoSourceDrifted(deps, job, sourceInfo); err != nil {
		invalidateVideoResultForDrift(result, job, err)
		return result, nil
	}

	data, err := deps.readThumb(cached.Path)
	if err != nil {
		appendFieldError(result, worker.MaskVideoThumb, "thumb_pdq", err)
		return result, nil
	}
	if _, err := inspectRGBJPEG(data); err != nil {
		appendFieldError(result, worker.MaskVideoThumb, "thumb_cache", err)
		return result, nil
	}
	started := time.Now()
	result.DecodeAttempts++
	thumb, err := deps.decodeThumb(data)
	result.DecodeNS += time.Since(started).Nanoseconds()
	if err != nil {
		appendFieldError(result, worker.MaskVideoThumb, "thumb_pdq", err)
		return result, nil
	}
	if len(thumb.Hash) != mediacore.PDQ256Bytes || thumb.Quality < 0 || thumb.Quality > 100 {
		appendFieldError(result, worker.MaskVideoThumb, "thumb_pdq", fmt.Errorf("thumbnail PDQ result is invalid"))
		return result, nil
	}
	if err := videoSourceDrifted(deps, job, sourceInfo); err != nil {
		invalidateVideoResultForDrift(result, job, err)
		return result, nil
	}
	quality := thumb.Quality
	result.Decoded = true
	result.ThumbPath = cached.Path
	result.ThumbPDQ = append([]byte(nil), thumb.Hash...)
	result.ThumbQuality = &quality
	result.FieldsDone |= worker.MaskVideoThumb
	return result, nil
}

var errVideoSourceDrifted = errors.New("video source drifted")

func videoSourceDrifted(deps videoPipelineDeps, job *worker.JobMsg, baseline os.FileInfo) error {
	current, err := deps.stat(fixPath(job.Path))
	if err != nil {
		return fmt.Errorf("%w: %v", errVideoSourceDrifted, err)
	}
	if !deps.sameFile(baseline, current) || !sameFileState(baseline, current) || !matchesDispatchedFile(current, job) {
		return fmt.Errorf("%w: file changed during video feature extraction", errVideoSourceDrifted)
	}
	return nil
}

func invalidateVideoResultForDrift(result *worker.JobResultMsg, job *worker.JobMsg, err error) {
	result.SHA512 = nil
	result.FieldsDone = 0
	result.DurationMS = nil
	result.ThumbPath = ""
	result.ThumbPDQ = nil
	result.ThumbQuality = nil
	result.Decoded = false
	appendFieldError(result, job.FieldsMask, "stat", err)
}

func readAndHashVideo(cfg Config, job *worker.JobMsg, deps videoPipelineDeps, result *worker.JobResultMsg) (os.FileInfo, error) {
	started := time.Now()
	result.ReadAttempts++
	defer func() {
		result.ReadNS += time.Since(started).Nanoseconds()
	}()
	path := fixPath(job.Path)
	pathBefore, err := deps.stat(path)
	if err != nil {
		appendFieldError(result, job.FieldsMask, "stat", err)
		return nil, nil
	}
	if !matchesDispatchedFile(pathBefore, job) {
		appendFieldError(result, job.FieldsMask, "stat", fmt.Errorf("file changed before open"))
		return nil, nil
	}
	file, err := deps.open(path)
	if err != nil {
		appendFieldError(result, job.FieldsMask, "open", err)
		return nil, nil
	}
	defer file.Close()
	handleBefore, err := file.Stat()
	if err != nil {
		appendFieldError(result, job.FieldsMask, "stat", err)
		return nil, nil
	}
	if !deps.sameFile(pathBefore, handleBefore) || !sameFileState(pathBefore, handleBefore) {
		appendFieldError(result, job.FieldsMask, "stat", fmt.Errorf("opened file does not match dispatched path"))
		return nil, nil
	}

	needSHA := job.FieldsMask&worker.MaskSHA512 != 0
	var hasher sha512Stream
	if needSHA {
		hasher, err = deps.newSHA()
		if err != nil {
			appendFieldError(result, worker.MaskSHA512, "sha512", err)
			return nil, nil
		}
		defer hasher.Close()
	}
	chunk := make([]byte, cfg.ReadChunkBytes)
	for {
		count, readErr := file.Read(chunk)
		if count > 0 && hasher != nil {
			if err := hasher.Update(chunk[:count]); err != nil {
				appendFieldError(result, worker.MaskSHA512, "sha512", err)
				return nil, nil
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			appendFieldError(result, job.FieldsMask, "read", readErr)
			return nil, nil
		}
		if count == 0 {
			appendFieldError(result, job.FieldsMask, "read", io.ErrNoProgress)
			return nil, nil
		}
	}
	handleAfter, err := file.Stat()
	if err != nil {
		appendFieldError(result, job.FieldsMask, "stat", err)
		return nil, nil
	}
	pathAfter, err := deps.stat(path)
	if err != nil {
		appendFieldError(result, job.FieldsMask, "stat", err)
		return nil, nil
	}
	if !deps.sameFile(handleBefore, handleAfter) ||
		!deps.sameFile(pathBefore, pathAfter) ||
		!deps.sameFile(handleAfter, pathAfter) ||
		!sameFileState(handleBefore, handleAfter) ||
		!sameFileState(pathBefore, pathAfter) ||
		!matchesDispatchedFile(pathAfter, job) {
		appendFieldError(result, job.FieldsMask, "stat", fmt.Errorf("file changed during read"))
		return nil, nil
	}
	if hasher != nil {
		sum, err := hasher.Final()
		if err != nil {
			appendFieldError(result, worker.MaskSHA512, "sha512", err)
			return nil, nil
		}
		result.SHA512 = append([]byte(nil), sum[:]...)
		result.FieldsDone |= worker.MaskSHA512
	} else {
		result.SHA512 = append([]byte(nil), job.KnownSHA...)
	}
	return pathAfter, nil
}

func validateVideoPipelineConfig(cfg Config) error {
	if err := validatePipelineConfig(cfg); err != nil {
		return err
	}
	if cfg.FFprobeTimeout <= 0 || cfg.FFmpegTimeout <= 0 {
		return fmt.Errorf("FFmpeg timeouts must be positive")
	}
	if cfg.FFprobePath == "" || cfg.FFmpegPath == "" || cfg.ThumbCacheDir == "" {
		return fmt.Errorf("FFmpeg and thumbnail cache paths must not be empty")
	}
	if cfg.ThumbMaxSide <= 0 {
		return fmt.Errorf("thumbnail maximum side must be positive")
	}
	return nil
}

func validateVideoDeps(deps videoPipelineDeps) error {
	if deps.open == nil || deps.stat == nil || deps.sameFile == nil || deps.newSHA == nil ||
		deps.query == nil || deps.probe == nil || deps.cache == nil || deps.paths == nil || deps.generate == nil ||
		deps.publish == nil || deps.readThumb == nil || deps.decodeThumb == nil || deps.pid == nil || deps.nonce == nil {
		return fmt.Errorf("worker video pipeline: pipeline dependency is unavailable")
	}
	return nil
}

func validateCachedVideoReply(reply *worker.SHAReplyMsg) error {
	if reply.DurationMS == nil || *reply.DurationMS <= 0 {
		return fmt.Errorf("duration must be positive")
	}
	return nil
}

func ffmpegRGBShotWithDigest(parent context.Context, cfg Config, source string, seekSeconds float64, destination string, runner commandRunner) (string, error) {
	if runner == nil {
		runner = execCommandRunner{}
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
	ctx, cancel := context.WithTimeout(parent, cfg.FFmpegTimeout)
	defer cancel()
	filter := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,format=rgb24", cfg.ThumbMaxSide, cfg.ThumbMaxSide)
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-ss", strconv.FormatFloat(seekSeconds, 'f', 3, 64),
		"-i", source,
		"-frames:v", "1",
		"-an", "-sn", "-dn",
		"-vf", filter,
		"-q:v", "3",
		"-f", "image2",
		"-y", destination,
	}
	_, stderr, err := runner.Run(ctx, cfg.FFmpegPath, args)
	if ctxErr := ctx.Err(); ctxErr != nil {
		if ctxErr == context.DeadlineExceeded {
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
	output, err := os.OpenFile(destination, os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("open generated thumbnail: %w", err)
	}
	info, statErr := output.Stat()
	if statErr == nil && (!info.Mode().IsRegular() || info.Size() == 0) {
		statErr = fmt.Errorf("ffmpeg produced no thumbnail")
	}
	if statErr == nil {
		statErr = output.Sync()
	}
	closeErr := output.Close()
	if statErr != nil {
		return "", statErr
	}
	if closeErr != nil {
		return "", fmt.Errorf("close generated thumbnail: %w", closeErr)
	}
	return fileSHA256Hex(destination)
}
