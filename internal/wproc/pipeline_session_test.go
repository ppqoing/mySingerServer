package wproc

import (
	"bytes"
	"context"
	"crypto/sha512"
	"errors"
	"image/color"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dedup/internal/proto"
	"dedup/internal/worker"
	"dedup/internal/wproc/videocore"
)

// Break caught: a frame/contact-sheet failure erases metadata that was frozen
// successfully after the same AVFormatContext completed stream probing.
func TestSessionPipelineKeepsVideoMetadataWhenContactSheetFails(t *testing.T) {
	job, deps, fake := newSessionPipelineTest(t, worker.MediaVideo,
		worker.MaskVideoMetadata|worker.MaskVideoContactSheet, 0)
	job.Phase = worker.Phase1
	deps.query = sessionPipelineMissingReply(job,
		worker.MaskVideoMetadata|worker.MaskVideoContactSheet, 0)
	primary := int32(0)
	fake.videoMetadata = &videocore.VideoMetadata{
		Container: proto.VideoContainerMetadata{
			FormatName: "mov,mp4", TagsJSON: `{}`, PrimaryVideoStream: &primary,
		},
		Streams: []proto.VideoStreamMetadata{{
			Index: 0, MediaType: "video", CodecID: 27, CodecName: "h264", TagsJSON: `{}`,
		}},
	}
	fake.analyzeErr = errors.New("contact sheet decode failed")

	result, err := processMediaWithDeps(context.Background(), sessionPipelineTestConfig(), job, deps)
	if err != nil {
		t.Fatal(err)
	}
	if result.FieldsDone&worker.MaskVideoMetadata == 0 || result.VideoContainer == nil || len(result.VideoStreams) != 1 {
		t.Fatalf("metadata lost on independent failure: %#v", result)
	}
	if result.FieldsDone&worker.MaskVideoContactSheet != 0 || len(result.Errors) == 0 {
		t.Fatalf("contact failure not isolated: %#v", result)
	}
	if fake.rehashes != 1 {
		t.Fatalf("partial metadata result skipped final identity rehash: %d", fake.rehashes)
	}
}

func TestSessionPipelineOneOpenOneHashOneAnalyze(t *testing.T) {
	job, deps, fake := newSessionPipelineTest(t, worker.MediaImage,
		worker.MaskSHA512|worker.MaskImagePDQ, 0)
	deps.query = func(query *worker.SHAQueryMsg) (*worker.SHAReplyMsg, error) {
		return &worker.SHAReplyMsg{
			JobID: job.JobID, Found: false,
			RequestedFields: worker.MaskImagePDQ,
			MissingFields:   worker.MaskImagePDQ,
		}, nil
	}

	if _, err := processMediaWithDeps(context.Background(), sessionPipelineTestConfig(), job, deps); err != nil {
		t.Fatal(err)
	}
	if fake.opens != 1 || fake.hashes != 1 || fake.rehashes != 1 || fake.analyzes != 1 || fake.closes != 1 {
		t.Fatalf("calls open/hash/rehash/analyze/close = %d/%d/%d/%d/%d, want 1/1/1/1/1", fake.opens, fake.hashes, fake.rehashes, fake.analyzes, fake.closes)
	}
}

// Break caught: file-level decoder failures combine independent requested
// fields into one protocol-invalid error instead of preserving each field.
func TestSessionPipelineFileErrorSplitsRequestedFieldBits(t *testing.T) {
	job := &worker.JobMsg{
		JobID: 801, Path: `D:\media\broken.mp4`, Kind: worker.MediaVideo,
		Phase:      worker.Phase1,
		FieldsMask: worker.MaskSHA512 | worker.MaskVideoDuration | worker.MaskVideoContactSheet,
	}
	result := newSessionPipelineResult(job)
	sessionPipelineFileError(result,
		worker.MaskVideoDuration|worker.MaskVideoContactSheet,
		"video_probe", errors.New("decoder rejected stream"))
	if len(result.Errors) != 2 ||
		result.Errors[0].Field != worker.MaskVideoDuration ||
		result.Errors[1].Field != worker.MaskVideoContactSheet {
		t.Fatalf("field errors = %#v", result.Errors)
	}
	for _, fieldError := range result.Errors {
		if fieldError.Stage != "video_probe" || fieldError.Msg != "decoder rejected stream" ||
			fieldError.Field&(fieldError.Field-1) != 0 {
			t.Fatalf("invalid field error = %#v", fieldError)
		}
	}
}

func TestSessionPipelineCompleteHitSkipsAnalyze(t *testing.T) {
	job, deps, fake := newSessionPipelineTest(t, worker.MediaImage,
		worker.MaskSHA512|worker.MaskImagePDQ, 0)
	deps.query = func(query *worker.SHAQueryMsg) (*worker.SHAReplyMsg, error) {
		return &worker.SHAReplyMsg{
			JobID: job.JobID, Found: true,
			RequestedFields: worker.MaskImagePDQ,
			FieldsPresent:   worker.MaskImagePDQ,
			PDQ:             make([]byte, 32),
			Quality:         80,
			Width:           640,
			Height:          360,
		}, nil
	}

	if _, err := processMediaWithDeps(context.Background(), sessionPipelineTestConfig(), job, deps); err != nil {
		t.Fatal(err)
	}
	if fake.opens != 1 || fake.hashes != 1 || fake.rehashes != 1 || fake.analyzes != 0 || fake.closes != 1 {
		t.Fatalf("calls open/hash/rehash/analyze/close = %d/%d/%d/%d/%d, want 1/1/1/0/1", fake.opens, fake.hashes, fake.rehashes, fake.analyzes, fake.closes)
	}
}

func TestSessionPipelineFinalIdentityRejectsDriftAfterCacheQuery(t *testing.T) {
	job, deps, fake := newSessionPipelineTest(t, worker.MediaImage,
		worker.MaskImagePDQ, 0)
	before := sessionPipelineTestInfo{size: job.Size, mtime: time.UnixMilli(job.MTimeMS)}
	after := sessionPipelineTestInfo{size: job.Size + 1, mtime: time.UnixMilli(job.MTimeMS + 1)}
	current := fs.FileInfo(before)
	deps.stat = func(string) (fs.FileInfo, error) { return current, nil }
	deps.query = func(*worker.SHAQueryMsg) (*worker.SHAReplyMsg, error) {
		current = after
		return &worker.SHAReplyMsg{
			JobID: job.JobID, Found: true,
			RequestedFields: worker.MaskImagePDQ,
			FieldsPresent:   worker.MaskImagePDQ,
			PDQ:             bytes.Repeat([]byte{7}, 32),
			Quality:         80,
			Width:           640,
			Height:          360,
		}, nil
	}

	result, err := processMediaWithDeps(context.Background(), sessionPipelineTestConfig(), job, deps)
	if err != nil {
		t.Fatal(err)
	}
	assertSessionPipelineStalePayload(t, result)
	if fake.analyzes != 0 || fake.hashes != 1 || fake.rehashes != 0 {
		t.Fatalf("analyze/hash/rehash = %d/%d/%d, want metadata drift before rehash", fake.analyzes, fake.hashes, fake.rehashes)
	}
}

func TestSessionPipelineFinalIdentityRejectsSameMetadataContentReplacementAfterAnalyze(t *testing.T) {
	job, deps, fake := newSessionPipelineTest(t, worker.MediaImage,
		worker.MaskPHashParts, 0)
	deps.query = sessionPipelineMissingReply(job, worker.MaskPHashParts, 0)
	fake.result = videocore.AnalysisResult{
		MediaType: 1, ImageStatus: videocore.StatusOK,
		ImageFeatures: videocore.FeatureSet{PHash: [9]uint64{1}},
	}
	changed := fake.sha
	changed[0] ^= 0xff
	fake.rehashSHA = &changed

	result, err := processMediaWithDeps(context.Background(), sessionPipelineTestConfig(), job, deps)
	if err != nil {
		t.Fatal(err)
	}
	assertSessionPipelineStalePayload(t, result)
	if fake.analyzes != 1 || fake.hashes != 1 || fake.rehashes != 1 {
		t.Fatalf("analyze/hash/rehash = %d/%d/%d, want 1/1/1", fake.analyzes, fake.hashes, fake.rehashes)
	}
}

func TestDefaultSessionPipelineRehashReadsFreshBytesAndHonorsCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "media.bin")
	first := bytes.Repeat([]byte{1}, 8192)
	second := bytes.Repeat([]byte{2}, len(first))
	if err := os.WriteFile(path, first, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	job := &worker.JobMsg{Size: before.Size(), MTimeUnix: before.ModTime().Unix(), MTimeMS: before.ModTime().UnixMilli()}
	deps := defaultSessionPipelineDeps(nil)
	firstDigest, err := deps.rehash(context.Background(), path, before, job)
	if err != nil || firstDigest != sha512.Sum512(first) {
		t.Fatalf("first rehash=%x err=%v", firstDigest, err)
	}
	if err := os.WriteFile(path, second, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	secondDigest, err := deps.rehash(context.Background(), path, before, job)
	if err != nil || secondDigest != sha512.Sum512(second) || secondDigest == firstDigest {
		t.Fatalf("fresh rehash=%x first=%x err=%v", secondDigest, firstDigest, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := deps.rehash(ctx, path, before, job); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled rehash error=%v, want context.Canceled", err)
	}
}

func TestSessionPipelineContactGuardRejectsDriftBeforeAtomicPublish(t *testing.T) {
	job, deps, fake := newSessionPipelineTest(t, worker.MediaVideo,
		worker.MaskSHA512|worker.MaskVideoDuration|worker.MaskVideoContactSheet, 0)
	root := t.TempDir()
	paths, err := contactSheetPaths(root, fake.sha, 99, job.JobID, "guard")
	if err != nil {
		t.Fatal(err)
	}
	deps.contactSheetPaths = func(string, [64]byte, int, int64, string) (ContactSheetPaths, error) { return paths, nil }
	deps.publishContactSheet = publishContactSheet
	deps.query = sessionPipelineMissingReply(job, worker.MaskVideoDuration|worker.MaskVideoContactSheet, 0)
	changed := fake.sha
	changed[0] ^= 0xff
	fake.rehashSHA = &changed
	fake.result = videocore.AnalysisResult{
		MediaType: 2, DurationStatus: videocore.StatusOK, DurationMS: 4321,
		ContactSheetStatus: videocore.StatusOK, ContactSheetWidth: 960, ContactSheetHeight: 540,
		ContactSheetFeatures: videocore.FeatureSet{PDQ: [32]byte{7}, PDQQuality: 88},
		CompletedFrameMask:   1,
		Frames:               [6]videocore.FrameResult{{StandardIndex: 0, Status: videocore.StatusOK}},
	}
	fake.onAnalyze = func(request videocore.AnalysisRequest) {
		if err := writeRGBJPEG(request.TempJPEGPath, color.RGBA{R: 200, G: 40, B: 20, A: 255}); err != nil {
			t.Fatal(err)
		}
	}
	result, err := processMediaWithDeps(context.Background(), sessionPipelineTestConfig(), job, deps)
	if err != nil {
		t.Fatal(err)
	}
	assertSessionPipelineStalePayload(t, result)
	if _, err := os.Stat(paths.JPEG); !os.IsNotExist(err) {
		t.Fatalf("stale contact published final %q: %v", paths.JPEG, err)
	}
}

func TestSessionPipelinePartialMask(t *testing.T) {
	job, deps, fake := newSessionPipelineTest(t, worker.MediaVideo,
		worker.MaskSHA512|worker.MaskVideoDuration|worker.MaskVideo6F|worker.MaskVideoContactSheet, 0)
	deps.query = func(query *worker.SHAQueryMsg) (*worker.SHAReplyMsg, error) {
		if query.RequestedFields != worker.MaskVideoDuration|worker.MaskVideo6F|worker.MaskVideoContactSheet || query.RequestedFrames != worker.FrameMaskFull {
			t.Fatalf("query mask = fields %#x frames %#x", query.RequestedFields, query.RequestedFrames)
		}
		return &worker.SHAReplyMsg{
			JobID: job.JobID, Found: true,
			RequestedFields: query.RequestedFields,
			FieldsPresent:   worker.MaskVideoDuration,
			MissingFields:   worker.MaskVideo6F | worker.MaskVideoContactSheet,
			DurationMS:      ptrInt64(1234),
			RequestedFrames: worker.FrameMaskFull,
			MissingFrames:   worker.FrameMaskFull,
		}, nil
	}

	if _, err := processMediaWithDeps(context.Background(), sessionPipelineTestConfig(), job, deps); err != nil {
		t.Fatal(err)
	}
	if fake.analyzes != 1 || fake.request.Fields != worker.MaskVideo6F|worker.MaskVideoContactSheet || fake.request.FrameMask != worker.FrameMaskFull {
		t.Fatalf("analyze = %d request=%#v, want exactly missing fields and six frames", fake.analyzes, fake.request)
	}
	if fake.opens != 1 || fake.hashes != 1 || fake.rehashes != 1 || fake.closes != 1 {
		t.Fatalf("calls open/hash/rehash/close = %d/%d/%d/%d, want 1/1/1/1", fake.opens, fake.hashes, fake.rehashes, fake.closes)
	}
}

func TestSessionPipelineCancellation(t *testing.T) {
	job, deps, fake := newSessionPipelineTest(t, worker.MediaVideo,
		worker.MaskSHA512|worker.MaskVideo6F|worker.MaskVideoContactSheet, worker.FrameMaskFull)
	temporary := filepath.Join(t.TempDir(), "contact.tmp.jpg")
	paths := ContactSheetPaths{TempJPEG: temporary}
	deps.contactSheetPaths = func(string, [64]byte, int, int64, string) (ContactSheetPaths, error) { return paths, nil }
	deps.query = sessionPipelineMissingReply(job, worker.MaskVideo6F|worker.MaskVideoContactSheet, worker.FrameMaskFull)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake.onAnalyze = func(request videocore.AnalysisRequest) {
		if request.TempJPEGPath != temporary {
			t.Errorf("temp JPEG = %q, want %q", request.TempJPEGPath, temporary)
		}
		if err := os.WriteFile(temporary, []byte("temp"), 0o644); err != nil {
			t.Fatal(err)
		}
		cancel()
	}

	result, err := processMediaWithDeps(ctx, sessionPipelineTestConfig(), job, deps)
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	assertSessionPipelineCleared(t, result, fake, paths, false)
}

func TestSessionPipelineStale(t *testing.T) {
	job, deps, fake := newSessionPipelineTest(t, worker.MediaVideo,
		worker.MaskSHA512|worker.MaskVideo6F|worker.MaskVideoContactSheet, worker.FrameMaskFull)
	temporary := filepath.Join(t.TempDir(), "contact.tmp.jpg")
	paths := ContactSheetPaths{TempJPEG: temporary}
	deps.contactSheetPaths = func(string, [64]byte, int, int64, string) (ContactSheetPaths, error) { return paths, nil }
	deps.query = sessionPipelineMissingReply(job, worker.MaskVideo6F|worker.MaskVideoContactSheet, worker.FrameMaskFull)
	current := sessionPipelineTestInfo{size: job.Size, mtime: time.Unix(job.MTimeUnix, 0)}
	deps.stat = func(string) (fs.FileInfo, error) { return current, nil }
	fake.onAnalyze = func(request videocore.AnalysisRequest) {
		if err := os.WriteFile(request.TempJPEGPath, []byte("temp"), 0o644); err != nil {
			t.Fatal(err)
		}
		current = sessionPipelineTestInfo{size: job.Size + 1, mtime: time.Unix(job.MTimeUnix+1, 0)}
	}

	result, err := processMediaWithDeps(context.Background(), sessionPipelineTestConfig(), job, deps)
	if err != nil {
		t.Fatal(err)
	}
	assertSessionPipelineCleared(t, result, fake, paths, true)
	if len(result.Errors) != 1 || result.Errors[0].Stage != "stale" {
		t.Fatalf("stale errors = %#v, want one stale file error", result.Errors)
	}
}

func TestSessionPipelinePartialFrames(t *testing.T) {
	t.Run("partial success retains only successful slots", func(t *testing.T) {
		job, deps, fake := newSessionPipelineTest(t, worker.MediaVideo,
			worker.MaskSHA512|worker.MaskVideo6F, 0x03)
		deps.query = sessionPipelineMissingReply(job, worker.MaskVideo6F, 0x03)
		fake.result = videocore.AnalysisResult{
			MediaType: 2, CompletedFrameMask: 0x01,
			Frames: [6]videocore.FrameResult{
				{StandardIndex: 0, Status: videocore.StatusOK, SampleTimeMS: 1000, Features: videocore.FeatureSet{PDQ: [32]byte{1}, PDQQuality: 88}},
				{StandardIndex: 1, Status: videocore.StatusDecode, SampleTimeMS: 2000, Features: videocore.FeatureSet{PDQ: [32]byte{2}, PDQQuality: 99}},
			},
		}

		result, err := processMediaWithDeps(context.Background(), sessionPipelineTestConfig(), job, deps)
		if err != nil {
			t.Fatal(err)
		}
		if fake.closes != 1 || result.FramesDone != 0x01 || result.FieldsDone&worker.MaskVideo6F != 0 {
			t.Fatalf("close/frames/fields = %d/%#x/%#x, want 1/1/no six-frame completion", fake.closes, result.FramesDone, result.FieldsDone)
		}
		if result.FrameResults[0].Status != 0 || len(result.FrameResults[0].PDQ256) == 0 || result.FrameResults[1].Status == 0 || len(result.FrameResults[1].PDQ256) != 0 || result.FrameResults[1].Quality != 0 || len(result.FrameResults[1].PHashParts) != 0 || len(result.FrameResults[1].SobelHist) != 0 {
			t.Fatalf("frame slots retained failed payload: %#v", result.FrameResults)
		}
	})

	t.Run("all failed frames publish nothing", func(t *testing.T) {
		job, deps, fake := newSessionPipelineTest(t, worker.MediaVideo,
			worker.MaskSHA512|worker.MaskVideo6F|worker.MaskVideoContactSheet, 0x03)
		deps.query = sessionPipelineMissingReply(job, worker.MaskVideo6F|worker.MaskVideoContactSheet, 0x03)
		published := 0
		deps.publishContactSheet = func(ContactSheetPaths, func() error) error {
			published++
			return nil
		}
		fake.result = videocore.AnalysisResult{MediaType: 2, Frames: [6]videocore.FrameResult{
			{StandardIndex: 0, Status: videocore.StatusDecode, SampleTimeMS: 1000},
			{StandardIndex: 1, Status: videocore.StatusDecode, SampleTimeMS: 2000},
		}}

		result, err := processMediaWithDeps(context.Background(), sessionPipelineTestConfig(), job, deps)
		if err != nil {
			t.Fatal(err)
		}
		if fake.closes != 1 || result.FramesDone != 0 || result.FieldsDone&worker.MaskVideo6F != 0 || published != 0 {
			t.Fatalf("close/frames/fields/publish = %d/%#x/%#x/%d, want 1/0/no six-frame completion/0", fake.closes, result.FramesDone, result.FieldsDone, published)
		}
	})
}

func TestSessionPipelineBadCachedPayload(t *testing.T) {
	job, deps, fake := newSessionPipelineTest(t, worker.MediaImage, worker.MaskSHA512|worker.MaskImagePDQ, 0)
	deps.query = func(*worker.SHAQueryMsg) (*worker.SHAReplyMsg, error) {
		return &worker.SHAReplyMsg{JobID: job.JobID, Found: true, RequestedFields: worker.MaskImagePDQ, FieldsPresent: worker.MaskImagePDQ, PDQ: []byte{1}, Quality: 80, Width: 40, Height: 30}, nil
	}
	result, err := processMediaWithDeps(context.Background(), sessionPipelineTestConfig(), job, deps)
	if err == nil || result != nil || fake.analyzes != 0 || fake.closes != 1 {
		t.Fatalf("bad cache reply = result=%#v err=%v analyze/close=%d/%d, want rejected before analyze", result, err, fake.analyzes, fake.closes)
	}
}

func TestSessionPipelineAnalyzeCancellationClears(t *testing.T) {
	job, deps, fake := newSessionPipelineTest(t, worker.MediaImage, worker.MaskSHA512|worker.MaskImagePDQ, 0)
	deps.query = sessionPipelineMissingReply(job, worker.MaskImagePDQ, 0)
	fake.analyzeErr = context.Canceled
	result, err := processMediaWithDeps(context.Background(), sessionPipelineTestConfig(), job, deps)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("analyze cancellation error = %v, want context.Canceled", err)
	}
	assertSessionPipelineCleared(t, result, fake, ContactSheetPaths{}, false)
}

func TestSessionPipelineStaleIdentityHashMismatchReturnsStableStaleWithoutPayload(t *testing.T) {
	job, deps, fake := newSessionPipelineTest(t, worker.MediaImage, worker.MaskPHashParts, 0)
	job.ScreenStage = worker.ScreenStageTwo
	job.Source = worker.JobSourceManager
	job.KnownSHA = bytesRepeat64(0xee)
	deps.query = sessionPipelineMissingReply(job, worker.MaskPHashParts, 0)

	result, err := processMediaWithDeps(context.Background(), sessionPipelineTestConfig(), job, deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) != 1 || result.Errors[0].Stage != "stale" ||
		string(result.SHA512) != string(job.KnownSHA) || result.FieldsDone != 0 ||
		len(result.PHashParts) != 0 || fake.analyzes != 0 {
		t.Fatalf("hash-mismatch stale result=%#v analyze=%d", result, fake.analyzes)
	}
}

func TestSessionPipelineLegacyThumbValidatesAndPublishes(t *testing.T) {
	t.Run("present cache needs a valid contact sheet", func(t *testing.T) {
		job, deps, fake := newSessionPipelineTest(t, worker.MediaVideo, worker.MaskSHA512|worker.MaskVideoThumb, 0)
		cached := ContactSheetJPEG{Path: `C:\cache\grid.jpg`, Width: 960, Height: 360}
		deps.contactSheetLookup = func(string, [64]byte) (ContactSheetJPEG, bool, error) { return cached, true, nil }
		quality := int32(76)
		deps.query = func(*worker.SHAQueryMsg) (*worker.SHAReplyMsg, error) {
			return &worker.SHAReplyMsg{JobID: job.JobID, Found: true, RequestedFields: worker.MaskVideoThumb, FieldsPresent: worker.MaskVideoThumb,
				DurationMS: ptrInt64(1234), ThumbPath: `C:\cache\grid.jpg`, ThumbPDQ: make([]byte, 32), ThumbQuality: &quality}, nil
		}
		result, err := processMediaWithDeps(context.Background(), sessionPipelineTestConfig(), job, deps)
		if err != nil {
			t.Fatal(err)
		}
		if fake.analyzes != 0 || result.FieldsDone&worker.MaskVideoThumb == 0 || result.ThumbPath == "" || len(result.ThumbPDQ) != 32 || result.ThumbQuality == nil || result.ContactSheetWidth == 0 || result.ContactSheetHeight == 0 {
			t.Fatalf("legacy cache result = %#v, analyze=%d", result, fake.analyzes)
		}
	})

	t.Run("valid JPEG repairs missing Store PDQ and dimensions without video analysis", func(t *testing.T) {
		job, deps, fake := newSessionPipelineTest(t, worker.MediaVideo, worker.MaskSHA512|worker.MaskVideoThumb, 0)
		cached := ContactSheetJPEG{Path: `C:\cache\00\sha.jpg`, Width: 640, Height: 360}
		deps.contactSheetLookup = func(string, [64]byte) (ContactSheetJPEG, bool, error) { return cached, true, nil }
		decoded := 0
		deps.decodeContactSheet = func(path string) (imagePhase1, error) {
			decoded++
			if path != cached.Path {
				t.Fatalf("decoded path = %q, want canonical cache %q", path, cached.Path)
			}
			return imagePhase1{Hash: bytes.Repeat([]byte{9}, 32), Quality: 82, Width: 640, Height: 360}, nil
		}
		deps.query = func(*worker.SHAQueryMsg) (*worker.SHAReplyMsg, error) {
			return &worker.SHAReplyMsg{JobID: job.JobID, Found: true, RequestedFields: worker.MaskVideoThumb, FieldsPresent: worker.MaskVideoThumb}, nil
		}
		result, err := processMediaWithDeps(context.Background(), sessionPipelineTestConfig(), job, deps)
		if err != nil {
			t.Fatal(err)
		}
		if decoded != 1 || fake.opens != 0 || fake.hashes != 0 || fake.rehashes != 0 || fake.analyzes != 0 || result.ThumbPath != cached.Path || len(result.ThumbPDQ) != 32 || result.ThumbQuality == nil || *result.ThumbQuality != 82 || result.ContactSheetWidth != 640 || result.ContactSheetHeight != 360 {
			t.Fatalf("repaired cache = decoded:%d open/hash/rehash/analyze:%d/%d/%d/%d result:%#v", decoded, fake.opens, fake.hashes, fake.rehashes, fake.analyzes, result)
		}
	})

	t.Run("force or missing field overwrites the same final path", func(t *testing.T) {
		job, deps, fake := newSessionPipelineTest(t, worker.MediaVideo, worker.MaskSHA512|worker.MaskVideoThumb, 0)
		temporary := filepath.Join(t.TempDir(), "grid.jpg.tmp")
		paths := ContactSheetPaths{TempJPEG: temporary}
		deps.contactSheetPaths = func(string, [64]byte, int, int64, string) (ContactSheetPaths, error) { return paths, nil }
		deps.contactSheetLookup = func(string, [64]byte) (ContactSheetJPEG, bool, error) {
			t.Fatal("forced missing field must not reuse an existing JPEG")
			return ContactSheetJPEG{}, false, nil
		}
		deps.query = sessionPipelineMissingReply(job, worker.MaskVideoThumb, 0)
		published := 0
		deps.publishContactSheet = func(ContactSheetPaths, func() error) error { published++; return nil }
		fake.result = videocore.AnalysisResult{MediaType: 2, DurationStatus: videocore.StatusOK, DurationMS: 1234,
			ContactSheetStatus: videocore.StatusOK, ContactSheetWidth: 960, ContactSheetHeight: 360, CompletedFrameMask: 1,
			Frames: [6]videocore.FrameResult{{StandardIndex: 0, Status: videocore.StatusOK, SampleTimeMS: 1000}}}
		fake.onAnalyze = func(request videocore.AnalysisRequest) {
			if request.Fields != worker.MaskVideoDuration|worker.MaskVideoContactSheet {
				t.Fatalf("legacy analysis fields = %#x", request.Fields)
			}
			if err := os.WriteFile(request.TempJPEGPath, []byte("jpeg"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		result, err := processMediaWithDeps(context.Background(), sessionPipelineTestConfig(), job, deps)
		if err != nil {
			t.Fatal(err)
		}
		if published != 1 || result.FieldsDone&worker.MaskVideoThumb == 0 {
			t.Fatalf("legacy publish = %d result=%#v", published, result)
		}
	})
}

func TestVideoBaseFeaturesSessionPublishesCompleteContactPayload(t *testing.T) {
	job, deps, fake := newSessionPipelineTest(
		t, worker.MediaVideo, worker.MaskAllVideo, 0,
	)
	root := t.TempDir()
	paths := ContactSheetPaths{JPEG: filepath.Join(root, "grid.jpg"), TempJPEG: filepath.Join(root, "grid.tmp.jpg")}
	deps.contactSheetPaths = func(string, [64]byte, int, int64, string) (ContactSheetPaths, error) {
		return paths, nil
	}
	deps.query = sessionPipelineMissingReply(
		job, worker.MaskVideoDuration|worker.MaskVideoContactSheet|worker.MaskVideoMetadata, 0,
	)
	published := 0
	deps.publishContactSheet = func(got ContactSheetPaths, validate func() error) error {
		published++
		if got != paths {
			t.Fatalf("publish paths = %#v, want %#v", got, paths)
		}
		return validate()
	}
	fake.result = videocore.AnalysisResult{
		MediaType: 2, DurationStatus: videocore.StatusOK, DurationMS: 4321,
		ContactSheetStatus: videocore.StatusOK, ContactSheetWidth: 960, ContactSheetHeight: 540,
		ContactSheetFeatures: videocore.FeatureSet{PDQ: [32]byte{7}, PDQQuality: 88},
		CompletedFrameMask:   1,
		Frames:               [6]videocore.FrameResult{{StandardIndex: 0, Status: videocore.StatusOK, SampleTimeMS: 1000}},
	}
	fake.onAnalyze = func(request videocore.AnalysisRequest) {
		if err := os.WriteFile(request.TempJPEGPath, []byte("jpeg"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	result, err := processMediaWithDeps(context.Background(), sessionPipelineTestConfig(), job, deps)
	if err != nil {
		t.Fatal(err)
	}
	if published != 1 || result.FieldsDone != worker.MaskAllVideo ||
		result.DurationMS == nil || *result.DurationMS != 4321 ||
		result.ThumbPath != paths.JPEG || len(result.ThumbPDQ) != 32 || result.ThumbPDQ[0] != 7 ||
		result.ThumbQuality == nil || *result.ThumbQuality != 88 ||
		result.ContactSheetWidth != 960 || result.ContactSheetHeight != 540 || !result.ThumbGenerated {
		t.Fatalf("video base result = published:%d %#v", published, result)
	}
}

func TestVideoBaseFeaturesUnpublishedContactPreservesDurationPartial(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*sessionPipelineDeps, *videocore.AnalysisResult)
	}{
		{
			name: "invalid dimensions",
			configure: func(_ *sessionPipelineDeps, analysis *videocore.AnalysisResult) {
				analysis.ContactSheetWidth = 2
				analysis.ContactSheetHeight = 1
			},
		},
		{
			name: "publish failure",
			configure: func(deps *sessionPipelineDeps, _ *videocore.AnalysisResult) {
				deps.publishContactSheet = func(ContactSheetPaths, func() error) error {
					return errors.New("publish failed")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job, deps, fake := newSessionPipelineTest(
				t, worker.MediaVideo, worker.MaskAllVideo, 0,
			)
			root := t.TempDir()
			paths := ContactSheetPaths{JPEG: filepath.Join(root, "grid.jpg"), TempJPEG: filepath.Join(root, "grid.tmp.jpg")}
			deps.contactSheetPaths = func(string, [64]byte, int, int64, string) (ContactSheetPaths, error) {
				return paths, nil
			}
			deps.query = sessionPipelineMissingReply(
				job, worker.MaskVideoDuration|worker.MaskVideoContactSheet|worker.MaskVideoMetadata, 0,
			)
			fake.result = videocore.AnalysisResult{
				MediaType: 2, DurationStatus: videocore.StatusOK, DurationMS: 4321,
				ContactSheetStatus: videocore.StatusOK, ContactSheetWidth: 960, ContactSheetHeight: 540,
				ContactSheetFeatures: videocore.FeatureSet{PDQ: [32]byte{7}, PDQQuality: 88},
				CompletedFrameMask:   1,
				Frames:               [6]videocore.FrameResult{{StandardIndex: 0, Status: videocore.StatusOK, SampleTimeMS: 1000}},
			}
			fake.onAnalyze = func(request videocore.AnalysisRequest) {
				if err := os.WriteFile(request.TempJPEGPath, []byte("jpeg"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			tt.configure(&deps, &fake.result)

			result, err := processMediaWithDeps(
				context.Background(), sessionPipelineTestConfig(), job, deps,
			)
			if err != nil {
				t.Fatal(err)
			}
			wantDone := uint32(worker.MaskSHA512 | worker.MaskVideoDuration | worker.MaskVideoMetadata)
			if result.FieldsDone != wantDone || result.DurationMS == nil || *result.DurationMS != 4321 ||
				result.VideoContainer == nil || len(result.VideoStreams) != 1 ||
				result.ContactSheetStatus != 0 || result.ContactSheetWidth != 0 || result.ContactSheetHeight != 0 ||
				result.ThumbPath != "" || len(result.ThumbPDQ) != 0 || result.ThumbQuality != nil ||
				result.ThumbGenerated || len(result.Errors) != 1 ||
				result.Errors[0].Field != worker.MaskVideoContactSheet {
				t.Fatalf("unpublished contact result = %#v", result)
			}
		})
	}
}

func TestSessionPipelineImagePhase2Features(t *testing.T) {
	job, deps, fake := newSessionPipelineTest(t, worker.MediaImage, worker.MaskSHA512|worker.MaskImagePDQ|worker.MaskPHashParts|worker.MaskSobelHist, 0)
	deps.query = sessionPipelineMissingReply(job, worker.MaskImagePDQ|worker.MaskPHashParts|worker.MaskSobelHist, 0)
	fake.result = videocore.AnalysisResult{MediaType: 1, ImageStatus: videocore.StatusOK,
		ImageFeatures: videocore.FeatureSet{PDQ: [32]byte{1}, PDQQuality: 90, PHash: [9]uint64{2}, SobelHistogram: [128]float32{3}}}
	result, err := processMediaWithDeps(context.Background(), sessionPipelineTestConfig(), job, deps)
	if err != nil {
		t.Fatal(err)
	}
	want := worker.MaskImagePDQ | worker.MaskPHashParts | worker.MaskSobelHist
	if result.FieldsDone&want != want || len(result.PDQ) != 32 || len(result.PHashParts) == 0 || len(result.SobelHist) == 0 {
		t.Fatalf("image phase2 result = %#v", result)
	}
}

func TestSessionPipelineImageStageTwoAndThreeReturnOnlyRequestedFeature(t *testing.T) {
	tests := []struct {
		name      string
		stage     worker.ScreenStage
		field     uint32
		wantPHash bool
		wantSobel bool
	}{
		{name: "stage two", stage: worker.ScreenStageTwo, field: worker.MaskPHashParts, wantPHash: true},
		{name: "stage three", stage: worker.ScreenStageThree, field: worker.MaskSobelHist, wantSobel: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			job, deps, fake := newSessionPipelineTest(t, worker.MediaImage, test.field, 0)
			job.ScreenStage = test.stage
			job.Source = worker.JobSourceManager
			deps.query = sessionPipelineMissingReply(job, test.field, 0)
			fake.result = videocore.AnalysisResult{MediaType: 1, ImageStatus: videocore.StatusOK,
				ImageFeatures: videocore.FeatureSet{PHash: [9]uint64{2}, SobelHistogram: [128]float32{3}}}

			result, err := processMediaWithDeps(context.Background(), sessionPipelineTestConfig(), job, deps)
			if err != nil {
				t.Fatal(err)
			}
			if result.ScreenStage != test.stage || result.Source != worker.JobSourceManager ||
				result.FieldsDone != test.field || (len(result.PHashParts) != 0) != test.wantPHash ||
				(len(result.SobelHist) != 0) != test.wantSobel {
				t.Fatalf("stage-isolated image result=%#v", result)
			}
			if fake.request.Fields != test.field {
				t.Fatalf("native image fields=%#x, want %#x", fake.request.Fields, test.field)
			}
		})
	}
}

func TestSessionPipelineVideoSixFrameStagesMapToLegacyNativeAndTrimPayload(t *testing.T) {
	tests := []struct {
		name      string
		stage     worker.ScreenStage
		field     uint32
		wantPHash bool
		wantSobel bool
	}{
		{name: "stage two", stage: worker.ScreenStageTwo, field: worker.MaskVideo6FPHash, wantPHash: true},
		{name: "stage three", stage: worker.ScreenStageThree, field: worker.MaskVideo6FSobel, wantSobel: true},
		{name: "legacy combined", stage: worker.ScreenStageLegacy, field: worker.MaskVideo6F, wantPHash: true, wantSobel: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			job, deps, fake := newSessionPipelineTest(t, worker.MediaVideo, test.field, worker.FrameMaskFull)
			job.ScreenStage = test.stage
			job.Source = worker.JobSourceManager
			deps.query = sessionPipelineMissingReply(job, test.field, worker.FrameMaskFull)
			fake.result = sessionStageVideoResult()

			result, err := processMediaWithDeps(context.Background(), sessionPipelineTestConfig(), job, deps)
			if err != nil {
				t.Fatal(err)
			}
			if fake.request.Fields != worker.MaskVideo6F {
				t.Fatalf("native fields=%#x, want legacy %#x", fake.request.Fields, worker.MaskVideo6F)
			}
			if result.FieldsDone != test.field || result.FramesDone != worker.FrameMaskFull {
				t.Fatalf("done fields/frames=%#x/%#x, want %#x/%#x", result.FieldsDone, result.FramesDone, test.field, worker.FrameMaskFull)
			}
			for index, frame := range result.FrameResults {
				if (len(frame.PHashParts) != 0) != test.wantPHash || (len(frame.SobelHist) != 0) != test.wantSobel {
					t.Fatalf("frame[%d] leaked stage payload: pHash=%d Sobel=%d", index, len(frame.PHashParts), len(frame.SobelHist))
				}
				if test.stage != worker.ScreenStageLegacy && (len(frame.PDQ256) != 0 || frame.Quality != 0) {
					t.Fatalf("frame[%d] stage %d leaked legacy PDQ", index, test.stage)
				}
				if test.stage == worker.ScreenStageLegacy && (len(frame.PDQ256) != 32 || frame.Quality == 0) {
					t.Fatalf("legacy frame[%d] lost PDQ/quality", index)
				}
			}
		})
	}
}

func TestSessionPipelineVideoStageCacheReturnsOnlyRequestedPayload(t *testing.T) {
	job, deps, fake := newSessionPipelineTest(t, worker.MediaVideo, worker.MaskVideo6FPHash, worker.FrameMaskFull)
	job.ScreenStage = worker.ScreenStageTwo
	job.Source = worker.JobSourceManager
	cached := sessionStageVideoResult()
	frames := [6]worker.FrameResult{}
	for index, native := range cached.Frames {
		frames[index] = worker.FrameResult{
			FrameIdx: index, Status: native.Status, TimeMS: native.SampleTimeMS,
			PHashParts: make([]byte, 76),
		}
	}
	deps.query = func(query *worker.SHAQueryMsg) (*worker.SHAReplyMsg, error) {
		return &worker.SHAReplyMsg{
			JobID: query.JobID, Found: true,
			RequestedFields: worker.MaskVideo6FPHash, FieldsPresent: worker.MaskVideo6FPHash,
			RequestedFrames: worker.FrameMaskFull, FramesPresent: worker.FrameMaskFull,
			FrameResults: frames,
		}, nil
	}

	result, err := processMediaWithDeps(context.Background(), sessionPipelineTestConfig(), job, deps)
	if err != nil {
		t.Fatal(err)
	}
	if fake.analyzes != 0 || result.FieldsDone != worker.MaskVideo6FPHash || result.FramesDone != worker.FrameMaskFull {
		t.Fatalf("stage cache result=%#v analyzes=%d", result, fake.analyzes)
	}
	for index, frame := range result.FrameResults {
		if len(frame.PHashParts) == 0 || len(frame.SobelHist) != 0 || len(frame.PDQ256) != 0 {
			t.Fatalf("cached frame[%d] leaked payload: %#v", index, frame)
		}
	}
}

func sessionStageVideoResult() videocore.AnalysisResult {
	result := videocore.AnalysisResult{MediaType: 2, CompletedFrameMask: worker.FrameMaskFull}
	for index := range result.Frames {
		result.Frames[index] = videocore.FrameResult{
			StandardIndex: uint32(index), Status: videocore.StatusOK, SampleTimeMS: int64(index+1) * 1000,
			Features: videocore.FeatureSet{PDQ: [32]byte{byte(index + 1)}, PDQQuality: 80,
				PHash: [9]uint64{uint64(index + 1)}, SobelHistogram: [128]float32{float32(index + 1)}},
		}
	}
	return result
}

func TestSessionPipelineCachedFrameMasksWithoutPayloadAreRejected(t *testing.T) {
	job, deps, fake := newSessionPipelineTest(t, worker.MediaVideo, worker.MaskSHA512|worker.MaskVideo6F, worker.FrameMaskFull)
	deps.query = func(*worker.SHAQueryMsg) (*worker.SHAReplyMsg, error) {
		return &worker.SHAReplyMsg{JobID: job.JobID, Found: true, RequestedFields: worker.MaskVideo6F, FieldsPresent: worker.MaskVideo6F,
			RequestedFrames: worker.FrameMaskFull, FramesPresent: worker.FrameMaskFull}, nil
	}
	result, err := processMediaWithDeps(context.Background(), sessionPipelineTestConfig(), job, deps)
	if err == nil || result != nil || fake.analyzes != 0 {
		t.Fatalf("mask-only cached frames result=%#v err=%v analyze=%d, want rejection", result, err, fake.analyzes)
	}
}

func TestSessionPipelineUnrequestedSHABit(t *testing.T) {
	job, deps, fake := newSessionPipelineTest(t, worker.MediaImage, worker.MaskImagePDQ, 0)
	deps.query = sessionPipelineMissingReply(job, worker.MaskImagePDQ, 0)
	result, err := processMediaWithDeps(context.Background(), sessionPipelineTestConfig(), job, deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.SHA512) != 64 || result.FieldsDone&worker.MaskSHA512 != 0 || fake.closes != 1 {
		t.Fatalf("unrequested SHA result = %#v close=%d", result, fake.closes)
	}
}

type sessionPipelineFake struct {
	sha              [64]byte
	result           videocore.AnalysisResult
	opens            int
	hashes           int
	analyzes         int
	closes           int
	request          videocore.AnalysisRequest
	onAnalyze        func(videocore.AnalysisRequest)
	analyzeErr       error
	videoMetadata    *videocore.VideoMetadata
	videoMetadataErr error
	rehashes         int
	rehashSHA        *[64]byte
	rehashErr        error
}

func (fake *sessionPipelineFake) Hash() ([64]byte, error) {
	fake.hashes++
	return fake.sha, nil
}

func (fake *sessionPipelineFake) Analyze(_ context.Context, request videocore.AnalysisRequest) (videocore.AnalysisResult, error) {
	fake.analyzes++
	fake.request = request
	if fake.onAnalyze != nil {
		fake.onAnalyze(request)
	}
	return fake.result, fake.analyzeErr
}

func (fake *sessionPipelineFake) VideoMetadata() (*videocore.VideoMetadata, error) {
	return fake.videoMetadata, fake.videoMetadataErr
}

func sessionPipelineMissingReply(job *worker.JobMsg, fields uint32, frames uint8) func(*worker.SHAQueryMsg) (*worker.SHAReplyMsg, error) {
	return func(query *worker.SHAQueryMsg) (*worker.SHAReplyMsg, error) {
		return &worker.SHAReplyMsg{
			JobID: job.JobID, Found: true,
			RequestedFields: query.RequestedFields, MissingFields: fields,
			RequestedFrames: query.RequestedFrames, MissingFrames: frames,
		}, nil
	}
}

func assertSessionPipelineCleared(t *testing.T, result *worker.JobResultMsg, fake *sessionPipelineFake, paths ContactSheetPaths, wantKnownSHA bool) {
	t.Helper()
	if result == nil {
		t.Fatal("cancellation/stale returned nil result")
	}
	wantSHALen := 0
	if wantKnownSHA {
		wantSHALen = 64
	}
	if fake.closes != 1 || len(result.SHA512) != wantSHALen || result.FieldsDone != 0 || result.FramesDone != 0 || len(result.PDQ) != 0 || result.DurationMS != nil || result.ContactSheetWidth != 0 || result.ContactSheetHeight != 0 {
		t.Fatalf("cancellation/stale retained result or did not close once: close=%d result=%#v", fake.closes, result)
	}
	if _, err := os.Stat(paths.TempJPEG); !os.IsNotExist(err) {
		t.Fatalf("cancellation/stale left temp %q: %v", paths.TempJPEG, err)
	}
}

func assertSessionPipelineStalePayload(t *testing.T, result *worker.JobResultMsg) {
	t.Helper()
	if result == nil || result.FieldsDone != 0 || result.FramesDone != 0 || len(result.PDQ) != 0 ||
		len(result.PHashParts) != 0 || len(result.SobelHist) != 0 || result.DurationMS != nil ||
		len(result.ThumbPDQ) != 0 || result.ThumbQuality != nil {
		t.Fatalf("stale result retained payload: %#v", result)
	}
	if len(result.Errors) != 1 || result.Errors[0].Stage != "stale" {
		t.Fatalf("stale errors = %#v, want stable stale", result.Errors)
	}
}

func (fake *sessionPipelineFake) Close() error {
	fake.closes++
	return nil
}

func newSessionPipelineTest(t *testing.T, kind worker.MediaKind, fields uint32, frames uint8) (*worker.JobMsg, sessionPipelineDeps, *sessionPipelineFake) {
	t.Helper()
	info := sessionPipelineTestInfo{size: 1234, mtime: time.Unix(1_700_000_000, 0)}
	fake := &sessionPipelineFake{}
	if kind == worker.MediaVideo {
		primary := int32(0)
		fake.videoMetadata = &videocore.VideoMetadata{
			Container: proto.VideoContainerMetadata{
				FormatName: "mov,mp4", TagsJSON: `{}`,
				PrimaryVideoStream: &primary, DecoderName: "h264",
			},
			Streams: []proto.VideoStreamMetadata{{
				Index: 0, MediaType: "video", CodecID: 27,
				CodecName: "h264", TagsJSON: `{}`,
			}},
		}
	}
	for index := range fake.sha {
		fake.sha[index] = byte(index)
	}
	job := &worker.JobMsg{
		JobID: 77, ScanTaskID: "scan", Path: `C:\synthetic\input.bin`, Kind: kind,
		Phase: worker.Phase2, FieldsMask: fields, FrameMask: frames,
		Size: info.size, MTimeUnix: info.mtime.Unix(), MTimeMS: info.mtime.UnixMilli(),
	}
	job.KnownSHA = append([]byte(nil), fake.sha[:]...)
	deps := sessionPipelineDeps{
		stat:     func(string) (fs.FileInfo, error) { return info, nil },
		sameFile: func(left, right fs.FileInfo) bool { return left == right },
		runtime: func() (videocore.RuntimeInfo, error) {
			return videocore.RuntimeInfo{Version: "test"}, nil
		},
		open: func(context.Context, string, videocore.OpenOptions) (mediaSession, error) {
			fake.opens++
			return fake, nil
		},
		rehash: func(context.Context, string, fs.FileInfo, *worker.JobMsg) ([64]byte, error) {
			fake.rehashes++
			if fake.rehashErr != nil {
				return [64]byte{}, fake.rehashErr
			}
			if fake.rehashSHA != nil {
				return *fake.rehashSHA, nil
			}
			return fake.sha, nil
		},
		query:              func(*worker.SHAQueryMsg) (*worker.SHAReplyMsg, error) { return nil, nil },
		contactSheetLookup: contactSheetLookupNoop,
		contactSheetPaths:  contactSheetPathsNoop,
		publishContactSheet: func(ContactSheetPaths, func() error) error {
			return nil
		},
		decodeContactSheet: func(string) (imagePhase1, error) {
			return imagePhase1{Hash: bytes.Repeat([]byte{7}, 32), Quality: 76, Width: 960, Height: 360}, nil
		},
		pid:   func() int { return 99 },
		nonce: func() string { return "test" },
		now:   func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
	return job, deps, fake
}

func bytesRepeat64(value byte) []byte {
	out := make([]byte, 64)
	for index := range out {
		out[index] = value
	}
	return out
}

func contactSheetLookupNoop(string, [64]byte) (ContactSheetJPEG, bool, error) {
	return ContactSheetJPEG{}, false, nil
}

func contactSheetPathsNoop(string, [64]byte, int, int64, string) (ContactSheetPaths, error) {
	return ContactSheetPaths{}, nil
}

func sessionPipelineTestConfig() Config {
	return Config{
		ReadChunkBytes: 4096, ImageMemBytes: 1 << 20,
		FFprobeTimeout: time.Second, FFmpegTimeout: time.Second,
		Phase2FrameTimeout: time.Second, Phase2FrameMaxSide: 512,
		ThumbCacheDir: `C:\synthetic\cache`, ThumbMaxSide: 256, IPCMaxFrameBytes: 1 << 20,
	}
}

type sessionPipelineTestInfo struct {
	size  int64
	mtime time.Time
}

func (info sessionPipelineTestInfo) Name() string       { return "input.bin" }
func (info sessionPipelineTestInfo) Size() int64        { return info.size }
func (info sessionPipelineTestInfo) Mode() fs.FileMode  { return 0 }
func (info sessionPipelineTestInfo) ModTime() time.Time { return info.mtime }
func (info sessionPipelineTestInfo) IsDir() bool        { return false }
func (info sessionPipelineTestInfo) Sys() any           { return nil }

func ptrInt64(value int64) *int64 { return &value }
