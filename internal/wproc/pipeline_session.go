package wproc

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
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
	if !matchesDispatchedFile(before, job) {
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
	if err := validateSessionPipelineCachedPayload(reply, cachedPresent); err != nil {
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
		return result, nil
	}

	analysisFields := sessionPipelineAnalysisFields(missingFields)
	request := videocore.AnalysisRequest{Fields: analysisFields, FrameMask: missingFrames, KnownDurationMS: job.DurationMS,
		ProbeTimeout: cfg.FFprobeTimeout, FrameTimeout: cfg.Phase2FrameTimeout, TileMaxSide: int32(cfg.Phase2FrameMaxSide)}
	var paths ContactSheetPaths
	if analysisFields&worker.MaskVideoContactSheet != 0 {
		paths, err = deps.contactSheetPaths(cfg.ThumbCacheDir, sha, deps.pid(), job.JobID, deps.nonce())
		if err != nil {
			return sessionPipelineFileError(result, worker.MaskVideoContactSheet, "thumb_cache", err), nil
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
	if err != nil || !deps.sameFile(before, after) || !sameFileState(before, after) || !matchesDispatchedFile(after, job) {
		return sessionPipelineStale(result, job, paths), nil
	}
	sessionPipelineMergeAnalysis(result, job, missingFields, analysisFields, missingFrames, analysis)

	if request.TempJPEGPath != "" && analysis.ContactSheetStatus == videocore.StatusOK && contactSheetHasSuccessfulSample(analysis) {
		runtime, runtimeErr := deps.runtime()
		if runtimeErr != nil {
			return sessionPipelineFileError(result, worker.MaskVideoContactSheet, "thumb_cache", runtimeErr), nil
		}
		meta := contactSheetMetaFromAnalysis(sha, job.Size, runtime, analysis)
		if meta.CanvasWidth > 0 && meta.CanvasHeight > 0 && meta.TileWidth > 0 && meta.TileHeight > 0 {
			if err := deps.publishContactSheet(paths, meta, func() error {
				info, err := deps.stat(path)
				if err != nil || !deps.sameFile(before, info) || !sameFileState(before, info) || !matchesDispatchedFile(info, job) {
					return fmt.Errorf("source drift before contact sheet publish")
				}
				return nil
			}); err != nil {
				return sessionPipelineFileError(result, worker.MaskVideoContactSheet, "thumb_cache", err), nil
			}
			if missingFields&worker.MaskVideoContactSheet != 0 {
				result.FieldsDone |= worker.MaskVideoContactSheet
			}
			if missingFields&worker.MaskVideoThumb != 0 {
				result.FieldsDone |= worker.MaskVideoThumb
			}
		}
	}
	return result, nil
}

func newSessionPipelineResult(job *worker.JobMsg) *worker.JobResultMsg {
	if job == nil {
		return &worker.JobResultMsg{}
	}
	return &worker.JobResultMsg{JobID: job.JobID, ScanTaskID: job.ScanTaskID, Path: job.Path, Kind: job.Kind, Phase: job.Phase}
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
	if job.FieldsMask == 0 || job.FieldsMask&^(worker.MaskSHA512|worker.MaskImagePDQ|worker.MaskVideoThumb|worker.MaskPHashParts|worker.MaskSobelHist|worker.MaskVideo6F|worker.MaskVideoDuration|worker.MaskVideoContactSheet) != 0 || job.FrameMask&^worker.FrameMaskFull != 0 {
		return fmt.Errorf("invalid media job masks")
	}
	return nil
}

func validateSessionPipelineDeps(deps sessionPipelineDeps) error {
	if deps.stat == nil || deps.sameFile == nil || deps.runtime == nil || deps.open == nil || deps.query == nil || deps.contactSheetLookup == nil || deps.contactSheetPaths == nil || deps.publishContactSheet == nil || deps.pid == nil || deps.nonce == nil || deps.now == nil {
		return fmt.Errorf("session pipeline dependency is unavailable")
	}
	return nil
}

func sessionPipelineRequested(job *worker.JobMsg) (uint32, uint8) {
	fields := job.FieldsMask &^ worker.MaskSHA512
	frames := job.FrameMask
	if job.Kind == worker.MediaVideo && fields&worker.MaskVideo6F != 0 && frames == 0 {
		frames = worker.FrameMaskFull
	}
	return fields, frames
}

func sessionPipelineAnalysisFields(missing uint32) uint32 {
	fields := missing &^ worker.MaskVideoThumb
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

func validateSessionPipelineCachedPayload(reply *worker.SHAReplyMsg, present uint32) error {
	if present&worker.MaskImagePDQ != 0 && (len(reply.PDQ) != videocore.PDQBytes || reply.Quality < 0 || reply.Quality > 100 || reply.Width <= 0 || reply.Height <= 0) {
		return fmt.Errorf("session pipeline incompatible cached image PDQ")
	}
	if present&worker.MaskVideoDuration != 0 && (reply.DurationMS == nil || *reply.DurationMS < 0) {
		return fmt.Errorf("session pipeline incompatible cached duration")
	}
	if present&(worker.MaskVideoThumb|worker.MaskVideoContactSheet) != 0 && (reply.ThumbPath == "" || len(reply.ThumbPDQ) != videocore.PDQBytes || reply.ThumbQuality == nil || *reply.ThumbQuality < 0 || *reply.ThumbQuality > 100) {
		return fmt.Errorf("session pipeline incompatible cached contact sheet")
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
}

func sessionPipelineAnalyzeStage(job *worker.JobMsg, fields uint32, frames uint8) string {
	if job.Kind == worker.MediaImage {
		return "image_decode"
	}
	if fields&worker.MaskVideoContactSheet != 0 {
		return "video_contact_sheet"
	}
	if frames != 0 || fields&worker.MaskVideo6F != 0 {
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
			frame.PDQ256 = append([]byte(nil), native.Features.PDQ[:]...)
			frame.Quality = int32(native.Features.PDQQuality)
			frame.PHashParts = features.EncodePHashParts(native.Features.PHash)
			frame.SobelHist, _ = features.EncodeSobelHist(native.Features.SobelHistogram)
			result.FramesDone |= 1 << uint(index)
		}
		result.FrameResults[index] = frame
	}
	if result.FramesDone == worker.FrameMaskFull {
		result.FieldsDone |= worker.MaskVideo6F
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
		fields = 1
	}
	result.Errors = append(result.Errors, worker.FieldError{Field: fields, Stage: stage, Msg: err.Error()})
	return result
}

func sessionPipelineCancelled(result *worker.JobResultMsg, paths ContactSheetPaths) *worker.JobResultMsg {
	removeContactSheetTemps(paths)
	clearSessionPipelineResult(result)
	return result
}

func sessionPipelineStale(result *worker.JobResultMsg, job *worker.JobMsg, paths ContactSheetPaths) *worker.JobResultMsg {
	removeContactSheetTemps(paths)
	clearSessionPipelineResult(result)
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
