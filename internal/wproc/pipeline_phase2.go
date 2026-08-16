package wproc

import (
	"bytes"
	"context"
	"crypto/sha512"
	"fmt"
	"hash"
	"io"
	"os"
	"time"

	"dedup/internal/features"
	"dedup/internal/worker"
	"dedup/internal/wproc/mediacore"
)

type phase2GrayImage interface {
	PDQ256() ([mediacore.PDQ256Bytes]byte, int32, error)
	Phase2() (mediacore.Phase2Result, error)
	Free()
}

type phase2PipelineDeps struct {
	open       func(string) (readStatCloser, error)
	stat       func(string) (os.FileInfo, error)
	sameFile   func(os.FileInfo, os.FileInfo) bool
	newHash    func() (hash.Hash, error)
	decode     func([]byte) (phase2GrayImage, error)
	runCommand func(context.Context, string, []string, io.Writer, io.Writer) error
}

func defaultPhase2PipelineDeps() phase2PipelineDeps {
	return phase2PipelineDeps{
		open:     func(path string) (readStatCloser, error) { return os.Open(path) },
		stat:     os.Stat,
		sameFile: os.SameFile,
		newHash:  func() (hash.Hash, error) { return sha512.New(), nil },
		decode: func(data []byte) (phase2GrayImage, error) {
			return mediacore.DecodeFromMemory(data)
		},
		runCommand: runPhase2Command,
	}
}

func processPhase2WithDeps(
	ctx context.Context,
	cfg Config,
	job *worker.JobMsg,
	deps phase2PipelineDeps,
) (*worker.JobResultMsg, error) {
	result := newPhase2Result(job)
	if err := validatePhase2Job(job); err != nil {
		phase2FileError(result, "validate", err)
		return result, nil
	}
	switch job.Kind {
	case worker.MediaImage:
		return processPhase2Image(ctx, cfg, job, deps, result)
	case worker.MediaVideo:
		return processPhase2Video(ctx, cfg, job, deps, result)
	default:
		phase2FileError(result, "kind", fmt.Errorf("unsupported media kind"))
		return result, nil
	}
}

func processPhase2Image(
	ctx context.Context,
	cfg Config,
	job *worker.JobMsg,
	deps phase2PipelineDeps,
	result *worker.JobResultMsg,
) (*worker.JobResultMsg, error) {
	if err := validatePhase2ImageConfig(cfg); err != nil {
		return nil, fmt.Errorf("worker phase-2 image pipeline: invalid config: %w", err)
	}
	if deps.open == nil || deps.stat == nil || deps.sameFile == nil ||
		deps.newHash == nil || deps.decode == nil {
		return nil, fmt.Errorf("worker phase-2 image pipeline: dependency is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}

	path := fixPath(job.Path)
	pathBefore, err := deps.stat(path)
	if err != nil {
		phase2FileError(result, "stat", err)
		return result, nil
	}
	metadataMatches := matchesPhase2Dispatch(pathBefore, job)

	file, err := deps.open(path)
	if err != nil {
		phase2FileError(result, "open", err)
		return result, nil
	}
	defer file.Close()

	handleBefore, err := file.Stat()
	if err != nil {
		phase2FileError(result, "stat", err)
		return result, nil
	}
	if !deps.sameFile(pathBefore, handleBefore) ||
		!samePhase2FileState(pathBefore, handleBefore) {
		phase2Stale(result, fmt.Errorf("opened file does not match path"))
		return result, nil
	}

	var hasher hash.Hash
	if !metadataMatches {
		hasher, err = deps.newHash()
		if err != nil {
			phase2FileError(result, "hash", err)
			return result, nil
		}
	}

	result.ReadAttempts = 1
	readStarted := time.Now()
	initialCapacity, retain := retentionCapacity(pathBefore.Size(), cfg.ImageMemBytes, cfg.ReadChunkBytes)
	var retained []byte
	if retain {
		retained = make([]byte, 0, initialCapacity)
	}
	chunk := make([]byte, cfg.ReadChunkBytes)
	for {
		if err := ctx.Err(); err != nil {
			result.ReadNS = time.Since(readStarted).Nanoseconds()
			return result, err
		}
		n, readErr := file.Read(chunk)
		if n > 0 {
			block := chunk[:n]
			if hasher != nil {
				_, _ = hasher.Write(block)
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
			result.ReadNS = time.Since(readStarted).Nanoseconds()
			if isContextError(readErr) {
				return result, readErr
			}
			phase2FileError(result, "read", readErr)
			return result, nil
		}
		if n == 0 {
			result.ReadNS = time.Since(readStarted).Nanoseconds()
			phase2FileError(result, "read", io.ErrNoProgress)
			return result, nil
		}
	}
	result.ReadNS = time.Since(readStarted).Nanoseconds()

	handleAfterRead, err := file.Stat()
	if err != nil {
		phase2FileError(result, "stat", err)
		return result, nil
	}
	pathAfterRead, err := deps.stat(path)
	if err != nil {
		phase2FileError(result, "stat", err)
		return result, nil
	}
	if phase2IdentityDrifted(
		deps.sameFile,
		pathBefore, handleBefore, pathAfterRead, handleAfterRead,
	) {
		phase2Stale(result, fmt.Errorf("file changed during read"))
		return result, nil
	}
	if hasher != nil && !bytes.Equal(hasher.Sum(nil), job.KnownSHA) {
		phase2Stale(result, fmt.Errorf("file content no longer matches known SHA-512"))
		return result, nil
	}

	if retained == nil {
		phase2BitErrors(result, job.FieldsMask, "memory",
			fmt.Errorf("image exceeds memory threshold (%d bytes)", cfg.ImageMemBytes))
		return result, nil
	}

	decodeStarted := time.Now()
	result.DecodeAttempts = 1
	gray, err := deps.decode(retained)
	result.DecodeNS = time.Since(decodeStarted).Nanoseconds()
	if err != nil {
		phase2BitErrors(result, job.FieldsMask, "decode", err)
		return result, nil
	}
	result.Decoded = true
	defer gray.Free()

	output, err := gray.Phase2()
	if err != nil {
		phase2BitErrors(result, job.FieldsMask, "phase2", err)
		return result, nil
	}
	if job.FieldsMask&worker.MaskPHashParts != 0 {
		result.PHashParts = features.EncodePHashParts(output.PHashParts)
		result.FieldsDone |= worker.MaskPHashParts
	}
	if job.FieldsMask&worker.MaskSobelHist != 0 {
		result.SobelHist, err = features.EncodeSobelHist(output.SobelHist)
		if err != nil {
			appendFieldError(result, worker.MaskSobelHist, "encode", err)
		} else {
			result.FieldsDone |= worker.MaskSobelHist
		}
	}

	handleAfterDecode, err := file.Stat()
	if err != nil {
		phase2FileError(result, "stat", err)
		return result, nil
	}
	pathAfterDecode, err := deps.stat(path)
	if err != nil {
		phase2FileError(result, "stat", err)
		return result, nil
	}
	if phase2IdentityDrifted(
		deps.sameFile,
		pathAfterRead, handleAfterRead, pathAfterDecode, handleAfterDecode,
	) {
		phase2Stale(result, fmt.Errorf("file changed during decode"))
	}
	return result, nil
}

func validatePhase2Job(job *worker.JobMsg) error {
	if job == nil {
		return fmt.Errorf("job is nil")
	}
	if job.Size < 0 {
		return fmt.Errorf("file size must not be negative")
	}
	if job.MTimeMS < 0 {
		return fmt.Errorf("mtime milliseconds must not be negative")
	}
	if len(job.KnownSHA) != sha512.Size {
		return fmt.Errorf("known SHA-512 must be %d bytes", sha512.Size)
	}
	switch job.Kind {
	case worker.MediaImage:
		const allowed = worker.MaskPHashParts | worker.MaskSobelHist
		if job.FieldsMask == 0 || job.FieldsMask&^allowed != 0 {
			return fmt.Errorf("invalid phase-2 image field mask %#x", job.FieldsMask)
		}
		if job.FrameMask != 0 || job.DurationMS != 0 {
			return fmt.Errorf("image job must not contain video fields")
		}
	case worker.MediaVideo:
		if job.FieldsMask != worker.MaskVideo6F {
			return fmt.Errorf("invalid phase-2 video field mask %#x", job.FieldsMask)
		}
		if job.FrameMask&^uint8(0x3f) != 0 {
			return fmt.Errorf("invalid frame mask %#x", job.FrameMask)
		}
		if job.DurationMS <= 0 {
			return fmt.Errorf("video duration must be positive")
		}
	default:
		return fmt.Errorf("unsupported media kind")
	}
	return nil
}

func validatePhase2ImageConfig(cfg Config) error {
	return validatePipelineConfig(cfg)
}

func matchesPhase2Dispatch(info os.FileInfo, job *worker.JobMsg) bool {
	return info.Size() == job.Size && info.ModTime().UnixMilli() == job.MTimeMS
}

func samePhase2FileState(left, right os.FileInfo) bool {
	return left.Size() == right.Size() &&
		left.ModTime().UnixNano() == right.ModTime().UnixNano()
}

func phase2IdentityDrifted(
	sameFile func(os.FileInfo, os.FileInfo) bool,
	pathBefore, handleBefore, pathAfter, handleAfter os.FileInfo,
) bool {
	return !sameFile(pathBefore, pathAfter) ||
		!sameFile(handleBefore, handleAfter) ||
		!sameFile(pathAfter, handleAfter) ||
		!samePhase2FileState(pathBefore, pathAfter) ||
		!samePhase2FileState(handleBefore, handleAfter) ||
		!samePhase2FileState(pathAfter, handleAfter)
}

func newPhase2Result(job *worker.JobMsg) *worker.JobResultMsg {
	result := &worker.JobResultMsg{}
	if job == nil {
		return result
	}
	result.JobID = job.JobID
	result.ScanTaskID = job.ScanTaskID
	result.Phase = worker.Phase2
	result.Path = job.Path
	result.Kind = job.Kind
	result.SHA512 = append([]byte(nil), job.KnownSHA...)
	return result
}

func phase2BitErrors(result *worker.JobResultMsg, mask uint32, stage string, err error) {
	for _, field := range []uint32{worker.MaskPHashParts, worker.MaskSobelHist, worker.MaskVideo6F} {
		if mask&field != 0 {
			appendFieldError(result, field, stage, err)
		}
	}
}

func phase2FileError(result *worker.JobResultMsg, stage string, err error) {
	result.FieldsDone = 0
	result.PHashParts = nil
	result.SobelHist = nil
	result.Frames = nil
	result.Errors = []worker.FieldError{{Field: 0, Stage: stage, Msg: err.Error()}}
}

func phase2Stale(result *worker.JobResultMsg, err error) {
	phase2FileError(result, "stale", err)
}
