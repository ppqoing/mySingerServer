package wproc

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"time"

	"dedup/internal/features"
	"dedup/internal/worker"
	"dedup/internal/wproc/videocore"
)

type mediaSession interface {
	Hash() ([64]byte, error)
	Analyze(context.Context, videocore.AnalysisRequest) (videocore.AnalysisResult, error)
	Close() error
}

type sessionPipelineDeps struct {
	stat                func(string) (fs.FileInfo, error)
	sameFile            func(fs.FileInfo, fs.FileInfo) bool
	runtime             func() (videocore.RuntimeInfo, error)
	open                func(context.Context, string, videocore.OpenOptions) (mediaSession, error)
	rehash              func(context.Context, string, fs.FileInfo, *worker.JobMsg) ([64]byte, error)
	query               func(*worker.SHAQueryMsg) (*worker.SHAReplyMsg, error)
	contactSheetLookup  func(string, [64]byte) (ContactSheetMeta, bool, error)
	contactSheetPaths   func(string, [64]byte, int, int64, string) (ContactSheetPaths, error)
	publishContactSheet func(ContactSheetPaths, ContactSheetMeta, func() error) error
	pid                 func() int
	nonce               func() string
	now                 func() time.Time
}

func defaultSessionPipelineDeps(query func(*worker.SHAQueryMsg) (*worker.SHAReplyMsg, error)) sessionPipelineDeps {
	return sessionPipelineDeps{
		stat:     os.Stat,
		sameFile: os.SameFile,
		runtime:  videocore.Runtime,
		open: func(ctx context.Context, path string, options videocore.OpenOptions) (mediaSession, error) {
			return videocore.Open(ctx, path, options)
		},
		rehash:              rehashMediaFile,
		query:               query,
		contactSheetLookup:  lookupContactSheet,
		contactSheetPaths:   contactSheetPaths,
		publishContactSheet: publishContactSheet,
		pid:                 os.Getpid,
		nonce:               func() string { return strconv.FormatInt(time.Now().UnixNano(), 36) },
		now:                 time.Now,
	}
}

func processMediaWithDeps(ctx context.Context, cfg Config, job *worker.JobMsg, deps sessionPipelineDeps) (*worker.JobResultMsg, error) {
	result := newSessionPipelineResult(job)
	if err := validateSessionPipelineJob(job); err != nil {
		return sessionPipelineFileError(result, job.FieldsMask, "feature_compute", err), nil
	}
	if err := validateSessionPipelineDeps(deps); err != nil {
		return nil, err
	}
	path := fixPath(job.Path)
	before, err := deps.stat(path)
	if err != nil {
		return sessionPipelineFileError(result, job.FieldsMask, "stale", err), nil
	}
	if !matchesSessionDispatchedFile(before, job) {
		return sessionPipelineStale(result, job, ContactSheetPaths{}), nil
	}
	if err := ctx.Err(); err != nil {
		return sessionPipelineCancelled(result, ContactSheetPaths{}), err
	}

	session, err := deps.open(ctx, path, videocore.OpenOptions{Kind: job.Kind, ImageMemoryBytes: cfg.ImageMemBytes, NativeTimeout: cfg.FFmpegTimeout})
	if err != nil {
		return sessionPipelineFileError(result, job.FieldsMask, "native_open", err), nil
	}
	defer session.Close()

	sha, err := session.Hash()
	if err != nil {
		return sessionPipelineFileError(result, worker.MaskSHA512, "native_hash", err), nil
	}
	result.SHA512 = append([]byte(nil), sha[:]...)
	if job.Phase == worker.Phase2 && !bytes.Equal(job.KnownSHA, sha[:]) {
		return sessionPipelineStale(result, job, ContactSheetPaths{}), nil
	}
	if job.FieldsMask&worker.MaskSHA512 != 0 {
		result.FieldsDone |= worker.MaskSHA512
	}

	requestedFields, requestedFrames := sessionPipelineRequested(job)
	reply, err := deps.query(&worker.SHAQueryMsg{
		JobID: job.JobID, ScanTaskID: job.ScanTaskID, SHA512: append([]byte(nil), sha[:]...), Kind: job.Kind,
		RequestedFields: requestedFields, RequestedFrames: requestedFrames,
	})
	if err != nil {
		return nil, fmt.Errorf("session pipeline SHA query: %w", err)
	}
	if err := validateSessionPipelineReply(job, requestedFields, requestedFrames, reply); err != nil {
		return nil, err
	}
	missingFields, missingFrames := reply.MissingFields, reply.MissingFrames
	cachedPresent := reply.FieldsPresent
	if err := validateSessionPipelineCachedPayload(job, reply, cachedPresent); err != nil {
		return nil, err
	}
	var cachedContact *ContactSheetMeta
	contactFields := worker.MaskVideoThumb | worker.MaskVideoContactSheet
	if cachedPresent&contactFields != 0 {
		meta, hit, lookupErr := deps.contactSheetLookup(cfg.ThumbCacheDir, sha)
		if lookupErr != nil {
			return nil, fmt.Errorf("session pipeline contact cache lookup: %w", lookupErr)
		}
		if !hit || meta.SourceSize != job.Size {
			missingFields |= cachedPresent & contactFields
			cachedPresent &^= contactFields
		} else {
			cachedContact = &meta
		}
	}
	sessionPipelineMergeCached(result, reply, cachedPresent, cachedContact)
	if missingFields == 0 && missingFrames == 0 {
		return sessionPipelineFinalIdentity(ctx, result, job, path, before, sha, deps, ContactSheetPaths{})
	}

	analysisFields := sessionPipelineAnalysisFields(missingFields)
	request := videocore.AnalysisRequest{Fields: analysisFields, FrameMask: missingFrames, KnownDurationMS: job.DurationMS,
		ProbeTimeout: cfg.FFprobeTimeout, FrameTimeout: cfg.Phase2FrameTimeout, TileMaxSide: int32(cfg.Phase2FrameMaxSide)}
	var paths ContactSheetPaths
	if analysisFields&worker.MaskVideoContactSheet != 0 {
		paths, err = deps.contactSheetPaths(cfg.ThumbCacheDir, sha, deps.pid(), job.JobID, deps.nonce())
		if err != nil {
			return sessionPipelineContactError(result, missingFields, "thumb_cache", err), nil
		}
		request.TempJPEGPath = paths.TempJPEG
	}
	if request.TempJPEGPath != "" {
		defer removeContactSheetTemps(paths)
	}

	analysis, err := session.Analyze(ctx, request)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return sessionPipelineCancelled(result, paths), err
		}
		return sessionPipelineFileError(result, missingFields, sessionPipelineAnalyzeStage(job, analysisFields, missingFrames), err), nil
	}
	if err := ctx.Err(); err != nil {
		return sessionPipelineCancelled(result, paths), err
	}
	after, err := deps.stat(path)
	if err != nil || !deps.sameFile(before, after) || !sameFileState(before, after) || !matchesSessionDispatchedFile(after, job) {
		return sessionPipelineStale(result, job, paths), nil
	}
	sessionPipelineMergeAnalysis(result, job, missingFields, analysisFields, missingFrames, analysis)

	if request.TempJPEGPath != "" {
		if analysis.ContactSheetStatus != videocore.StatusOK {
			return sessionPipelineContactError(
				result, missingFields, "video_contact_sheet", errors.New("contact sheet analysis failed"),
			), nil
		}
		if !contactSheetHasSuccessfulSample(analysis) {
			return sessionPipelineContactError(
				result, missingFields, "video_contact_sheet", errors.New("contact sheet has no successful sample"),
			), nil
		}
		runtime, runtimeErr := deps.runtime()
		if runtimeErr != nil {
			return sessionPipelineContactError(result, missingFields, "thumb_cache", runtimeErr), nil
		}
		meta := contactSheetMetaFromAnalysis(sha, job.Size, runtime, analysis)
		if meta.CanvasWidth <= 0 || meta.CanvasHeight <= 0 || meta.TileWidth <= 0 || meta.TileHeight <= 0 {
			return sessionPipelineContactError(
				result, missingFields, "thumb_cache", errors.New("invalid contact sheet metadata"),
			), nil
		}
		if err := deps.publishContactSheet(paths, meta, func() error {
			return sessionPipelineFinalIdentityError(ctx, job, path, before, sha, deps)
		}); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return sessionPipelineCancelled(result, paths), err
			}
			if errors.Is(err, errSessionPipelineStale) {
				return sessionPipelineStale(result, job, paths), nil
			}
			return sessionPipelineContactError(result, missingFields, "thumb_cache", err), nil
		}
		if missingFields&worker.MaskVideoContactSheet != 0 {
			result.FieldsDone |= worker.MaskVideoContactSheet
		}
		if missingFields&worker.MaskVideoThumb != 0 {
			result.FieldsDone |= worker.MaskVideoThumb
		}
		quality := int32(analysis.ContactSheetFeatures.PDQQuality)
		result.ThumbPath = paths.JPEG
		result.ThumbPDQ = append(
			[]byte(nil), analysis.ContactSheetFeatures.PDQ[:]...,
		)
		result.ThumbQuality = &quality
		result.ThumbGenerated = true
		return result, nil
	}
	return sessionPipelineFinalIdentity(ctx, result, job, path, before, sha, deps, paths)
}

var errSessionPipelineStale = errors.New("session pipeline stale identity")

func sessionPipelineFinalIdentity(
	ctx context.Context,
	result *worker.JobResultMsg,
	job *worker.JobMsg,
	path string,
	before fs.FileInfo,
	initialSHA [64]byte,
	deps sessionPipelineDeps,
	paths ContactSheetPaths,
) (*worker.JobResultMsg, error) {
	err := sessionPipelineFinalIdentityError(ctx, job, path, before, initialSHA, deps)
	if err == nil {
		return result, nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return sessionPipelineCancelled(result, paths), err
	}
	return sessionPipelineStale(result, job, paths), nil
}

func sessionPipelineFinalIdentityError(
	ctx context.Context,
	job *worker.JobMsg,
	path string,
	before fs.FileInfo,
	initialSHA [64]byte,
	deps sessionPipelineDeps,
) error {
	after, err := deps.stat(path)
	if err != nil || !deps.sameFile(before, after) || !sameFileState(before, after) || !matchesSessionDispatchedFile(after, job) {
		return errSessionPipelineStale
	}
	finalSHA, err := deps.rehash(ctx, path, before, job)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if err != nil || !bytes.Equal(finalSHA[:], initialSHA[:]) ||
		(len(job.KnownSHA) != 0 && !bytes.Equal(finalSHA[:], job.KnownSHA)) {
		return errSessionPipelineStale
	}
	return nil
}

func rehashMediaFile(ctx context.Context, path string, before fs.FileInfo, job *worker.JobMsg) ([64]byte, error) {
	return rehashMediaFileWithOpen(ctx, path, before, job, func(path string) (readStatCloser, error) {
		return os.Open(path)
	})
}

func rehashMediaFileWithOpen(
	ctx context.Context,
	path string,
	before fs.FileInfo,
	job *worker.JobMsg,
	open func(string) (readStatCloser, error),
) ([64]byte, error) {
	var digest [64]byte
	if err := ctx.Err(); err != nil {
		return digest, err
	}
	pathBefore, err := os.Stat(path)
	if err != nil || !sameRehashIdentity(before, pathBefore, job) {
		return digest, errSessionPipelineStale
	}
	if open == nil {
		return digest, fmt.Errorf("source opener is unavailable")
	}
	file, err := open(path)
	if err != nil {
		return digest, err
	}
	defer file.Close()
	handleBefore, err := file.Stat()
	if err != nil || !sameRehashIdentity(before, handleBefore, job) || !os.SameFile(pathBefore, handleBefore) {
		return digest, errSessionPipelineStale
	}
	hash := sha512.New()
	buffer := make([]byte, 1024*1024)
	for {
		if err := ctx.Err(); err != nil {
			return digest, err
		}
		read, readErr := file.Read(buffer)
		if read != 0 {
			_, _ = hash.Write(buffer[:read])
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return digest, readErr
		}
	}
	handleAfter, err := file.Stat()
	if err != nil || !sameRehashIdentity(before, handleAfter, job) || !os.SameFile(handleBefore, handleAfter) {
		return digest, errSessionPipelineStale
	}
	pathAfter, err := os.Stat(path)
	if err != nil || !sameRehashIdentity(before, pathAfter, job) || !os.SameFile(handleAfter, pathAfter) {
		return digest, errSessionPipelineStale
	}
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}

func sameRehashIdentity(before, current fs.FileInfo, job *worker.JobMsg) bool {
	return before != nil && current != nil && job != nil &&
		os.SameFile(before, current) && sameFileState(before, current) && matchesSessionDispatchedFile(current, job)
}

func newSessionPipelineResult(job *worker.JobMsg) *worker.JobResultMsg {
	if job == nil {
		return &worker.JobResultMsg{}
	}
	return &worker.JobResultMsg{JobID: job.JobID, ScanTaskID: job.ScanTaskID, Path: job.Path, Kind: job.Kind, Phase: job.Phase, ScreenStage: job.ScreenStage, Source: job.Source}
}

func validateSessionPipelineJob(job *worker.JobMsg) error {
	if job == nil || job.Path == "" || job.Size < 0 {
		return fmt.Errorf("invalid media job")
	}
	if job.Kind != worker.MediaImage && job.Kind != worker.MediaVideo {
		return fmt.Errorf("unsupported media kind")
	}
	if job.Phase != worker.Phase1 && job.Phase != worker.Phase2 {
		return fmt.Errorf("unsupported media phase")
	}
	if job.Phase == worker.Phase2 && len(job.KnownSHA) != 64 {
		return fmt.Errorf("phase-two job known SHA-512 length %d", len(job.KnownSHA))
	}
	if job.FieldsMask == 0 || job.FieldsMask&^(worker.MaskSHA512|worker.MaskImagePDQ|worker.MaskVideoThumb|worker.MaskPHashParts|worker.MaskSobelHist|worker.MaskVideo6F|worker.MaskVideoDuration|worker.MaskVideoContactSheet|worker.MaskVideo6FPHash|worker.MaskVideo6FSobel) != 0 || job.FrameMask&^worker.FrameMaskFull != 0 {
		return fmt.Errorf("invalid media job masks")
	}
	if err := validateSessionPipelineStage(job); err != nil {
		return err
	}
	return nil
}

func validateSessionPipelineStage(job *worker.JobMsg) error {
	if job.Phase != worker.Phase2 {
		return nil
	}
	switch job.ScreenStage {
	case worker.ScreenStageLegacy:
	case worker.ScreenStageTwo:
		want := uint32(worker.MaskPHashParts)
		if job.Kind == worker.MediaVideo {
			want = worker.MaskVideo6FPHash
		}
		if job.FieldsMask != want {
			return fmt.Errorf("stage two field mask %#x, want %#x", job.FieldsMask, want)
		}
	case worker.ScreenStageThree:
		want := uint32(worker.MaskSobelHist)
		if job.Kind == worker.MediaVideo {
			want = worker.MaskVideo6FSobel
		}
		if job.FieldsMask != want {
			return fmt.Errorf("stage three field mask %#x, want %#x", job.FieldsMask, want)
		}
	default:
		return fmt.Errorf("invalid screen stage %d", job.ScreenStage)
	}
	return nil
}

func matchesSessionDispatchedFile(info os.FileInfo, job *worker.JobMsg) bool {
	if info.Size() != job.Size {
		return false
	}
	if job.Phase == worker.Phase2 {
		return info.ModTime().UnixMilli() == job.MTimeMS
	}
	return info.ModTime().Unix() == job.MTimeUnix
}

func validateSessionPipelineDeps(deps sessionPipelineDeps) error {
	if deps.stat == nil || deps.sameFile == nil || deps.runtime == nil || deps.open == nil || deps.rehash == nil || deps.query == nil || deps.contactSheetLookup == nil || deps.contactSheetPaths == nil || deps.publishContactSheet == nil || deps.pid == nil || deps.nonce == nil || deps.now == nil {
		return fmt.Errorf("session pipeline dependency is unavailable")
	}
	return nil
}

func sessionPipelineRequested(job *worker.JobMsg) (uint32, uint8) {
	fields := job.FieldsMask &^ worker.MaskSHA512
	frames := job.FrameMask
	if job.Kind == worker.MediaVideo && fields&(worker.MaskVideo6F|worker.MaskVideo6FPHash|worker.MaskVideo6FSobel) != 0 && frames == 0 {
		frames = worker.FrameMaskFull
	}
	return fields, frames
}

func sessionPipelineAnalysisFields(missing uint32) uint32 {
	fields := missing &^ (worker.MaskVideoThumb | worker.MaskVideo6FPHash | worker.MaskVideo6FSobel)
	if missing&(worker.MaskVideo6FPHash|worker.MaskVideo6FSobel) != 0 {
		fields |= worker.MaskVideo6F
	}
	if missing&worker.MaskVideoThumb != 0 {
		fields |= worker.MaskVideoDuration | worker.MaskVideoContactSheet
	}
	return fields
}

func validateSessionPipelineReply(job *worker.JobMsg, fields uint32, frames uint8, reply *worker.SHAReplyMsg) error {
	if reply == nil || reply.JobID != job.JobID || reply.RequestedFields != fields || reply.RequestedFrames != frames {
		return fmt.Errorf("session pipeline incompatible SHA reply")
	}
	if err := reply.ValidateMasks(); err != nil {
		return fmt.Errorf("session pipeline incompatible SHA reply: %w", err)
	}
	return nil
}

func validateSessionPipelineCachedPayload(job *worker.JobMsg, reply *worker.SHAReplyMsg, present uint32) error {
	if present&worker.MaskImagePDQ != 0 && (len(reply.PDQ) != videocore.PDQBytes || reply.Quality < 0 || reply.Quality > 100 || reply.Width <= 0 || reply.Height <= 0) {
		return fmt.Errorf("session pipeline incompatible cached image PDQ")
	}
	if present&worker.MaskVideoDuration != 0 && (reply.DurationMS == nil || *reply.DurationMS < 0) {
		return fmt.Errorf("session pipeline incompatible cached duration")
	}
	if present&(worker.MaskVideoThumb|worker.MaskVideoContactSheet) != 0 && (reply.ThumbPath == "" || len(reply.ThumbPDQ) != videocore.PDQBytes || reply.ThumbQuality == nil || *reply.ThumbQuality < 0 || *reply.ThumbQuality > 100) {
		return fmt.Errorf("session pipeline incompatible cached contact sheet")
	}
	for index, frame := range reply.FrameResults {
		bit := uint8(1 << uint(index))
		if reply.FramesPresent&bit == 0 {
			if frame.FrameIdx != 0 || frame.Status != 0 || frame.TimeMS != 0 || len(frame.PDQ256) != 0 || frame.Quality != 0 || len(frame.PHashParts) != 0 || len(frame.SobelHist) != 0 {
				return fmt.Errorf("session pipeline unclaimed cached frame %d", index)
			}
			continue
		}
		if frame.FrameIdx != index || frame.Status != 0 {
			return fmt.Errorf("session pipeline incompatible cached frame %d", index)
		}
		switch job.ScreenStage {
		case worker.ScreenStageTwo:
			if len(frame.PHashParts) == 0 || len(frame.PDQ256) != 0 || frame.Quality != 0 || len(frame.SobelHist) != 0 {
				return fmt.Errorf("session pipeline incompatible stage-two cached frame %d", index)
			}
		case worker.ScreenStageThree:
			if len(frame.SobelHist) == 0 || len(frame.PDQ256) != 0 || frame.Quality != 0 || len(frame.PHashParts) != 0 {
				return fmt.Errorf("session pipeline incompatible stage-three cached frame %d", index)
			}
		case worker.ScreenStageLegacy:
			if len(frame.PDQ256) != 32 || len(frame.PHashParts) == 0 || len(frame.SobelHist) == 0 {
				return fmt.Errorf("session pipeline incompatible legacy cached frame %d", index)
			}
		}
	}
	return nil
}

func sessionPipelineMergeCached(result *worker.JobResultMsg, reply *worker.SHAReplyMsg, present uint32, contact *ContactSheetMeta) {
	if present&worker.MaskImagePDQ != 0 {
		result.PDQ, result.Quality, result.Width, result.Height = append([]byte(nil), reply.PDQ...), reply.Quality, reply.Width, reply.Height
		result.FieldsDone |= worker.MaskImagePDQ
	}
	if present&worker.MaskVideoDuration != 0 && reply.DurationMS != nil {
		value := *reply.DurationMS
		result.DurationMS = &value
		result.FieldsDone |= worker.MaskVideoDuration
	}
	if contact != nil && present&(worker.MaskVideoThumb|worker.MaskVideoContactSheet) != 0 {
		quality := *reply.ThumbQuality
		result.ThumbPath = reply.ThumbPath
		result.ThumbPDQ = append([]byte(nil), reply.ThumbPDQ...)
		result.ThumbQuality = &quality
		result.ContactSheetWidth = int32(contact.CanvasWidth)
		result.ContactSheetHeight = int32(contact.CanvasHeight)
		result.FieldsDone |= present & (worker.MaskVideoThumb | worker.MaskVideoContactSheet)
	}
	for index, frame := range reply.FrameResults {
		if reply.FramesPresent&(1<<uint(index)) == 0 {
			continue
		}
		result.FrameResults[index] = cloneSessionFrameResult(frame)
		result.FramesDone |= 1 << uint(index)
	}
	result.FieldsDone |= present & (worker.MaskVideo6F | worker.MaskVideo6FPHash | worker.MaskVideo6FSobel)
}

func cloneSessionFrameResult(frame worker.FrameResult) worker.FrameResult {
	frame.PDQ256 = append([]byte(nil), frame.PDQ256...)
	frame.PHashParts = append([]byte(nil), frame.PHashParts...)
	frame.SobelHist = append([]byte(nil), frame.SobelHist...)
	return frame
}

func sessionPipelineAnalyzeStage(job *worker.JobMsg, fields uint32, frames uint8) string {
	if job.Kind == worker.MediaImage {
		return "image_decode"
	}
	if fields&worker.MaskVideoContactSheet != 0 {
		return "video_contact_sheet"
	}
	if frames != 0 || fields&(worker.MaskVideo6F|worker.MaskVideo6FPHash|worker.MaskVideo6FSobel) != 0 {
		return "video_frame"
	}
	if fields&worker.MaskVideoDuration != 0 {
		return "video_probe"
	}
	return "feature_compute"
}

func sessionPipelineMergeAnalysis(result *worker.JobResultMsg, job *worker.JobMsg, missingFields uint32, analysisFields uint32, frames uint8, analysis videocore.AnalysisResult) {
	if analysisFields&worker.MaskImagePDQ != 0 && analysis.ImageStatus == videocore.StatusOK {
		result.PDQ = append([]byte(nil), analysis.ImageFeatures.PDQ[:]...)
		result.Quality = int32(analysis.ImageFeatures.PDQQuality)
		result.Width = int32(analysis.ContactSheetWidth)
		result.Height = int32(analysis.ContactSheetHeight)
		result.FieldsDone |= worker.MaskImagePDQ
	}
	if analysisFields&worker.MaskPHashParts != 0 && analysis.ImageStatus == videocore.StatusOK {
		result.PHashParts = features.EncodePHashParts(analysis.ImageFeatures.PHash)
		result.FieldsDone |= worker.MaskPHashParts
	}
	if analysisFields&worker.MaskSobelHist != 0 && analysis.ImageStatus == videocore.StatusOK {
		if encoded, err := features.EncodeSobelHist(analysis.ImageFeatures.SobelHistogram); err == nil {
			result.SobelHist = encoded
			result.FieldsDone |= worker.MaskSobelHist
		}
	}
	if analysisFields&worker.MaskVideoDuration != 0 && analysis.DurationStatus == videocore.StatusOK {
		value := analysis.DurationMS
		result.DurationMS = &value
		if missingFields&worker.MaskVideoDuration != 0 {
			result.FieldsDone |= worker.MaskVideoDuration
		}
	}
	if job.Kind != worker.MediaVideo {
		return
	}
	for index := 0; index < len(result.FrameResults); index++ {
		if frames&(1<<uint(index)) == 0 {
			continue
		}
		native := analysis.Frames[index]
		frame := worker.FrameResult{FrameIdx: index, Status: native.Status, TimeMS: native.SampleTimeMS}
		if native.Status == videocore.StatusOK && analysis.CompletedFrameMask&(1<<uint(index)) != 0 {
			if missingFields&worker.MaskVideo6F != 0 {
				frame.PDQ256 = append([]byte(nil), native.Features.PDQ[:]...)
				frame.Quality = int32(native.Features.PDQQuality)
			}
			if missingFields&(worker.MaskVideo6F|worker.MaskVideo6FPHash) != 0 {
				frame.PHashParts = features.EncodePHashParts(native.Features.PHash)
			}
			if missingFields&(worker.MaskVideo6F|worker.MaskVideo6FSobel) != 0 {
				frame.SobelHist, _ = features.EncodeSobelHist(native.Features.SobelHistogram)
			}
			result.FramesDone |= 1 << uint(index)
		}
		result.FrameResults[index] = frame
	}
	if result.FramesDone&frames == frames {
		result.FieldsDone |= missingFields & (worker.MaskVideo6F | worker.MaskVideo6FPHash | worker.MaskVideo6FSobel)
	}
	if analysisFields&worker.MaskVideoContactSheet != 0 && analysis.ContactSheetStatus == videocore.StatusOK {
		result.ContactSheetStatus = analysis.ContactSheetStatus
		result.ContactSheetWidth = int32(analysis.ContactSheetWidth)
		result.ContactSheetHeight = int32(analysis.ContactSheetHeight)
	}
}

func contactSheetHasSuccessfulSample(analysis videocore.AnalysisResult) bool {
	for _, frame := range analysis.Frames {
		if frame.Status == videocore.StatusOK {
			return true
		}
	}
	return false
}

func contactSheetMetaFromAnalysis(sha [64]byte, size int64, runtime videocore.RuntimeInfo, analysis videocore.AnalysisResult) ContactSheetMeta {
	meta := ContactSheetMeta{
		SchemaVersion: 1, Pipeline: contactSheetPipeline, SourceSHA512: hex.EncodeToString(sha[:]), SourceSize: size,
		CanvasWidth: int(analysis.ContactSheetWidth), CanvasHeight: int(analysis.ContactSheetHeight),
		TileWidth: int(analysis.ContactSheetWidth) / 3, TileHeight: int(analysis.ContactSheetHeight) / 2,
		VideoCoreVersion: runtime.Version, FFmpeg: runtime.Components,
	}
	for index, frame := range analysis.Frames {
		status := "ok"
		if frame.Status != videocore.StatusOK {
			status = "placeholder"
		}
		meta.Samples[index] = ContactSheetSample{TimeMS: frame.SampleTimeMS, Status: status}
	}
	return meta
}

func sessionPipelineFileError(result *worker.JobResultMsg, fields uint32, stage string, err error) *worker.JobResultMsg {
	if fields == 0 {
		result.Errors = append(result.Errors, worker.FieldError{Field: 0, Stage: stage, Msg: err.Error()})
		return result
	}
	for bit := uint32(1); fields != 0; bit <<= 1 {
		if fields&bit == 0 {
			continue
		}
		result.Errors = append(result.Errors, worker.FieldError{Field: bit, Stage: stage, Msg: err.Error()})
		fields &^= bit
	}
	return result
}

func sessionPipelineContactError(
	result *worker.JobResultMsg,
	missingFields uint32,
	stage string,
	err error,
) *worker.JobResultMsg {
	result.FieldsDone &^= worker.MaskVideoThumb | worker.MaskVideoContactSheet
	result.ContactSheetStatus, result.ContactSheetWidth, result.ContactSheetHeight = 0, 0, 0
	result.ThumbPath, result.ThumbPDQ, result.ThumbQuality = "", nil, nil
	result.ThumbGenerated, result.ThumbCacheHit = false, false
	field := uint32(worker.MaskVideoContactSheet)
	if missingFields&worker.MaskVideoContactSheet == 0 && missingFields&worker.MaskVideoThumb != 0 {
		field = worker.MaskVideoThumb
	}
	return sessionPipelineFileError(result, field, stage, err)
}

func sessionPipelineCancelled(result *worker.JobResultMsg, paths ContactSheetPaths) *worker.JobResultMsg {
	removeContactSheetTemps(paths)
	clearSessionPipelineResult(result)
	return result
}

func sessionPipelineStale(result *worker.JobResultMsg, job *worker.JobMsg, paths ContactSheetPaths) *worker.JobResultMsg {
	removeContactSheetTemps(paths)
	clearSessionPipelineResult(result)
	if job.Phase == worker.Phase2 && len(job.KnownSHA) == 64 {
		result.SHA512 = append([]byte(nil), job.KnownSHA...)
	}
	result.Errors = append(result.Errors, worker.FieldError{Field: job.FieldsMask, Stage: "stale", Msg: "media file changed"})
	return result
}

func clearSessionPipelineResult(result *worker.JobResultMsg) {
	result.SHA512 = nil
	result.FieldsDone = 0
	result.FramesDone = 0
	result.PDQ, result.PHashParts, result.SobelHist = nil, nil, nil
	result.Quality, result.Width, result.Height = 0, 0, 0
	result.DurationMS = nil
	result.ContactSheetStatus, result.ContactSheetWidth, result.ContactSheetHeight = 0, 0, 0
	result.FrameResults = [6]worker.FrameResult{}
	result.Frames = nil
}

func removeContactSheetTemps(paths ContactSheetPaths) {
	for _, path := range []string{paths.TempJPEG, paths.TempSidecar} {
		if path != "" {
			_ = os.Remove(path)
		}
	}
}
