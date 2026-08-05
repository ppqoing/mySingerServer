package wproc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
	cache       func(Config, string, os.FileInfo) (string, bool, string, error)
	generate    func(context.Context, Config, string, float64, string) (string, error)
	writeMeta   func(Config, string, os.FileInfo, string) error
	readThumb   func(string) ([]byte, error)
	decodeThumb func([]byte) (imagePhase1, error)
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
		cache: thumbCacheLookupWithDigest,
		generate: func(ctx context.Context, cfg Config, source string, seek float64, destination string) (string, error) {
			return ffmpegShotWithDigest(ctx, cfg, source, seek, destination, runner)
		},
		writeMeta: thumbCacheWriteMeta,
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
	if reply.Found {
		if err := validateCachedVideoReply(reply); err != nil {
			return nil, fmt.Errorf("worker video pipeline: incompatible SHA reply: %w", err)
		}
		duration := *reply.DurationMS
		quality := *reply.ThumbQuality
		result.DurationMS = &duration
		result.ThumbPath = reply.ThumbPath
		result.ThumbPDQ = append([]byte(nil), reply.ThumbPDQ...)
		result.ThumbQuality = &quality
		result.FieldsDone |= worker.MaskVideoThumb
		return result, nil
	}

	duration, durationErr := deps.probe(ctx, cfg, fixPath(job.Path))
	seek := 0.0
	if durationErr != nil {
		appendFieldError(result, worker.MaskVideoThumb, "ffprobe", durationErr)
	} else {
		durationValue := duration
		result.DurationMS = &durationValue
		result.FieldsDone |= worker.MaskVideoThumb
		seek = float64(duration) / 2000
	}

	thumbPath, hit, expectedJPEG, err := deps.cache(cfg, job.Path, sourceInfo)
	if err != nil {
		appendFieldError(result, worker.MaskVideoThumb, "thumb_cache", err)
		return result, nil
	}
	result.ThumbCacheHit = hit
	generated := false
	if !hit {
		started := time.Now()
		expectedJPEG, err = deps.generate(ctx, cfg, fixPath(job.Path), seek, thumbPath)
		if err != nil {
			appendFieldError(result, worker.MaskVideoThumb, "ffmpeg", err)
			return result, nil
		}
		result.ThumbMS = time.Since(started).Milliseconds()
		result.ThumbGenerated = true
		generated = true
	}
	if err := videoSourceDrifted(deps, job, sourceInfo); err != nil {
		invalidateVideoResultForDrift(result, job, err)
		return result, nil
	}

	data, err := deps.readThumb(thumbPath)
	if err != nil {
		appendFieldError(result, worker.MaskVideoThumb, "thumb_pdq", err)
		return result, nil
	}
	if actual := bytesSHA256Hex(data); actual != expectedJPEG {
		appendFieldError(result, worker.MaskVideoThumb, "thumb_cache",
			fmt.Errorf("%w: expected %s, read %s", errThumbnailPublishConflict, expectedJPEG, actual))
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
	if generated {
		if err := deps.writeMeta(cfg, job.Path, sourceInfo, expectedJPEG); err != nil {
			appendFieldError(result, 0, "thumb_cache", err)
			if errors.Is(err, errThumbnailPublishConflict) {
				return result, nil
			}
		}
	}
	quality := thumb.Quality
	result.Decoded = true
	result.ThumbPath = thumbPath
	result.ThumbPDQ = append([]byte(nil), thumb.Hash...)
	result.ThumbQuality = &quality
	result.FieldsDone |= worker.MaskVideoThumb
	return result, nil
}

func videoSourceDrifted(deps videoPipelineDeps, job *worker.JobMsg, baseline os.FileInfo) error {
	current, err := deps.stat(fixPath(job.Path))
	if err != nil {
		return err
	}
	if !deps.sameFile(baseline, current) || !sameFileState(baseline, current) || !matchesDispatchedFile(current, job) {
		return fmt.Errorf("file changed during video feature extraction")
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
		deps.query == nil || deps.probe == nil || deps.cache == nil || deps.generate == nil ||
		deps.writeMeta == nil || deps.readThumb == nil || deps.decodeThumb == nil {
		return fmt.Errorf("worker video pipeline: pipeline dependency is unavailable")
	}
	return nil
}

func validateCachedVideoReply(reply *worker.SHAReplyMsg) error {
	if reply.DurationMS == nil || *reply.DurationMS <= 0 {
		return fmt.Errorf("duration must be positive")
	}
	if reply.ThumbPath == "" {
		return fmt.Errorf("thumbnail path is empty")
	}
	if len(reply.ThumbPDQ) != mediacore.PDQ256Bytes {
		return fmt.Errorf("thumbnail PDQ must be %d bytes", mediacore.PDQ256Bytes)
	}
	if reply.ThumbQuality == nil || *reply.ThumbQuality < 0 || *reply.ThumbQuality > 100 {
		return fmt.Errorf("thumbnail quality must be between 0 and 100")
	}
	return nil
}
