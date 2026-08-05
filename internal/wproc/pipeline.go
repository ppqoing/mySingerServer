package wproc

import (
	"fmt"
	"io"
	"os"
	"time"

	"dedup/internal/worker"
	"dedup/internal/wproc/mediacore"
	"dedup/internal/wproc/videocore"
)

type readStatCloser interface {
	io.Reader
	Stat() (os.FileInfo, error)
	Close() error
}

type sha512Stream interface {
	Update([]byte) error
	Final() ([mediacore.SHA512Bytes]byte, error)
	Close() error
}

type imagePhase1 struct {
	Hash    []byte
	Quality int32
	Width   int32
	Height  int32
}

type pipelineDeps struct {
	runtime  func() (videocore.RuntimeInfo, error)
	open     func(string) (readStatCloser, error)
	stat     func(string) (os.FileInfo, error)
	sameFile func(os.FileInfo, os.FileInfo) bool
	newSHA   func() (sha512Stream, error)
	query    func(*worker.SHAQueryMsg) (*worker.SHAReplyMsg, error)
	decode   func([]byte) (imagePhase1, error)
	video    *videoPipelineDeps
	phase2   *phase2PipelineDeps
	session  *sessionPipelineDeps
}

func defaultPipelineDeps(query func(*worker.SHAQueryMsg) (*worker.SHAReplyMsg, error)) pipelineDeps {
	return pipelineDeps{
		open: func(path string) (readStatCloser, error) {
			return os.Open(path)
		},
		stat:     os.Stat,
		sameFile: os.SameFile,
		newSHA: func() (sha512Stream, error) {
			return mediacore.NewSHA512()
		},
		query: query,
		decode: func(data []byte) (imagePhase1, error) {
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

func shouldRetainImage(size, limit int64) bool {
	_, retain := retentionCapacity(size, limit, 1)
	return retain
}

func retentionCapacity(size, limit int64, readChunkBytes int) (int, bool) {
	const hardLimit = int64(maxImageMemMB) << 20
	if size < 0 || limit <= 0 || readChunkBytes <= 0 {
		return 0, false
	}
	if limit > hardLimit {
		limit = hardLimit
	}
	if size > limit {
		return 0, false
	}
	initial := size
	if initial > int64(readChunkBytes) {
		initial = int64(readChunkBytes)
	}
	if initial > int64(maxIntValue()) {
		return 0, false
	}
	return int(initial), true
}

func processImageWithDeps(cfg Config, job *worker.JobMsg, deps pipelineDeps) (*worker.JobResultMsg, error) {
	result := &worker.JobResultMsg{
		JobID: job.JobID,
		Path:  job.Path,
		Kind:  worker.MediaImage,
	}
	if err := validatePipelineConfig(cfg); err != nil {
		return nil, fmt.Errorf("worker image pipeline: invalid config: %w", err)
	}
	if job.Size < 0 {
		appendFieldError(result, job.FieldsMask, "size", fmt.Errorf("file size must not be negative"))
		return result, nil
	}
	if deps.open == nil || deps.stat == nil || deps.sameFile == nil || deps.newSHA == nil || deps.query == nil || deps.decode == nil {
		return nil, errorsResult(result, job.FieldsMask, "worker", "pipeline dependency is unavailable")
	}

	started := time.Now()
	result.ReadAttempts = 1
	readComplete := false
	defer func() {
		if !readComplete {
			result.ReadNS = time.Since(started).Nanoseconds()
		}
	}()
	path := fixPath(job.Path)
	pathBefore, err := deps.stat(path)
	if err != nil {
		appendFieldError(result, job.FieldsMask, "stat", err)
		return result, nil
	}
	if !matchesDispatchedFile(pathBefore, job) {
		appendFieldError(result, job.FieldsMask, "stat", fmt.Errorf("file changed before open"))
		return result, nil
	}

	file, err := deps.open(path)
	if err != nil {
		appendFieldError(result, job.FieldsMask, "open", err)
		return result, nil
	}
	defer file.Close()

	handleBefore, err := file.Stat()
	if err != nil {
		appendFieldError(result, job.FieldsMask, "stat", err)
		return result, nil
	}
	if !deps.sameFile(pathBefore, handleBefore) || !sameFileState(pathBefore, handleBefore) {
		appendFieldError(result, job.FieldsMask, "stat", fmt.Errorf("opened file does not match dispatched path"))
		return result, nil
	}

	needSHA := job.FieldsMask&worker.MaskSHA512 != 0
	var hasher sha512Stream
	if needSHA {
		hasher, err = deps.newSHA()
		if err != nil {
			appendFieldError(result, worker.MaskSHA512, "sha512", err)
			return result, nil
		}
		defer hasher.Close()
	}

	var retained []byte
	if initialCapacity, retain := retentionCapacity(job.Size, cfg.ImageMemBytes, cfg.ReadChunkBytes); retain {
		retained = make([]byte, 0, initialCapacity)
	}
	chunk := make([]byte, cfg.ReadChunkBytes)
	for {
		n, readErr := file.Read(chunk)
		if n > 0 {
			block := chunk[:n]
			if hasher != nil {
				if err := hasher.Update(block); err != nil {
					appendFieldError(result, worker.MaskSHA512, "sha512", err)
					return result, nil
				}
			}
			if retained != nil {
				if int64(len(retained))+int64(n) > cfg.ImageMemBytes {
					retained = nil
				} else {
					retained = append(retained, block...)
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			appendFieldError(result, job.FieldsMask, "read", readErr)
			return result, nil
		}
		if n == 0 {
			appendFieldError(result, job.FieldsMask, "read", io.ErrNoProgress)
			return result, nil
		}
	}

	handleAfter, err := file.Stat()
	if err != nil {
		appendFieldError(result, job.FieldsMask, "stat", err)
		return result, nil
	}
	pathAfter, err := deps.stat(path)
	if err != nil {
		appendFieldError(result, job.FieldsMask, "stat", err)
		return result, nil
	}
	if !deps.sameFile(handleBefore, handleAfter) ||
		!deps.sameFile(pathBefore, pathAfter) ||
		!deps.sameFile(handleAfter, pathAfter) ||
		!sameFileState(handleBefore, handleAfter) ||
		!sameFileState(pathBefore, pathAfter) ||
		!matchesDispatchedFile(pathAfter, job) {
		appendFieldError(result, job.FieldsMask, "stat", fmt.Errorf("file changed during read"))
		return result, nil
	}
	result.ReadNS = time.Since(started).Nanoseconds()
	readComplete = true

	if hasher != nil {
		sum, err := hasher.Final()
		if err != nil {
			appendFieldError(result, worker.MaskSHA512, "sha512", err)
			return result, nil
		}
		result.SHA512 = append([]byte(nil), sum[:]...)
		result.FieldsDone |= worker.MaskSHA512
	} else {
		result.SHA512 = append([]byte(nil), job.KnownSHA...)
	}

	if job.FieldsMask&worker.MaskImagePDQ == 0 {
		return result, nil
	}
	if len(result.SHA512) != mediacore.SHA512Bytes {
		appendFieldError(result, worker.MaskImagePDQ, "sha512", fmt.Errorf("SHA-512 must be %d bytes", mediacore.SHA512Bytes))
		return result, nil
	}

	reply, err := deps.query(&worker.SHAQueryMsg{
		JobID:  job.JobID,
		SHA512: append([]byte(nil), result.SHA512...),
		Kind:   worker.MediaImage,
	})
	if err != nil {
		return nil, fmt.Errorf("worker image pipeline: SHA query: %w", err)
	}
	if reply == nil || reply.JobID != job.JobID {
		return nil, fmt.Errorf("worker image pipeline: incompatible SHA reply")
	}
	if reply.Found {
		if err := validateCachedImageReply(reply); err != nil {
			return nil, fmt.Errorf("worker image pipeline: incompatible SHA reply: %w", err)
		}
		result.PDQ = append([]byte(nil), reply.PDQ...)
		result.Quality = reply.Quality
		result.Width = reply.Width
		result.Height = reply.Height
		result.FieldsDone |= worker.MaskImagePDQ
		return result, nil
	}

	if retained == nil {
		appendFieldError(result, worker.MaskImagePDQ, "decode", fmt.Errorf("image exceeds memory threshold (%d bytes), SHA-512 only", cfg.ImageMemBytes))
		return result, nil
	}
	decodeStarted := time.Now()
	result.DecodeAttempts++
	features, err := deps.decode(retained)
	result.DecodeNS += time.Since(decodeStarted).Nanoseconds()
	if err != nil {
		appendFieldError(result, worker.MaskImagePDQ, "decode", err)
		return result, nil
	}
	result.Decoded = true
	result.PDQ = append([]byte(nil), features.Hash...)
	result.Quality = features.Quality
	result.Width = features.Width
	result.Height = features.Height
	result.FieldsDone |= worker.MaskImagePDQ
	return result, nil
}

func matchesDispatchedFile(info os.FileInfo, job *worker.JobMsg) bool {
	return info.Size() == job.Size && info.ModTime().Unix() == job.MTimeUnix
}

func sameFileState(left, right os.FileInfo) bool {
	return left.Size() == right.Size() && left.ModTime().UnixNano() == right.ModTime().UnixNano()
}

func validatePipelineConfig(cfg Config) error {
	const hardImageLimit = int64(maxImageMemMB) << 20
	if cfg.ReadChunkBytes <= 0 || int64(cfg.ReadChunkBytes) > maxReadChunkKB<<10 {
		return fmt.Errorf("read chunk bytes must be between 1 and %d", maxReadChunkKB<<10)
	}
	if cfg.ImageMemBytes <= 0 || cfg.ImageMemBytes > hardImageLimit {
		return fmt.Errorf("image memory bytes must be between 1 and %d", hardImageLimit)
	}
	return nil
}

func validateCachedImageReply(reply *worker.SHAReplyMsg) error {
	if len(reply.PDQ) != mediacore.PDQ256Bytes {
		return fmt.Errorf("PDQ must be %d bytes", mediacore.PDQ256Bytes)
	}
	if reply.Width <= 0 || reply.Height <= 0 {
		return fmt.Errorf("image dimensions must be positive")
	}
	if reply.Quality < 0 || reply.Quality > 100 {
		return fmt.Errorf("image quality must be between 0 and 100")
	}
	return nil
}

func appendFieldError(result *worker.JobResultMsg, field uint32, stage string, err error) {
	result.Errors = append(result.Errors, worker.FieldError{
		Field: field,
		Stage: stage,
		Msg:   err.Error(),
	})
}

func errorsResult(result *worker.JobResultMsg, field uint32, stage, message string) error {
	return fmt.Errorf("worker image pipeline: field %#x stage %s: %s", field, stage, message)
}
