package wproc

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dedup/internal/worker"
	"dedup/internal/wproc/videocore"
)

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
	if fake.opens != 1 || fake.hashes != 1 || fake.analyzes != 1 || fake.closes != 1 {
		t.Fatalf("calls open/hash/analyze/close = %d/%d/%d/%d, want 1/1/1/1", fake.opens, fake.hashes, fake.analyzes, fake.closes)
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
	if fake.opens != 1 || fake.hashes != 1 || fake.analyzes != 0 || fake.closes != 1 {
		t.Fatalf("calls open/hash/analyze/close = %d/%d/%d/%d, want 1/1/0/1", fake.opens, fake.hashes, fake.analyzes, fake.closes)
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
	if fake.opens != 1 || fake.hashes != 1 || fake.closes != 1 {
		t.Fatalf("calls open/hash/close = %d/%d/%d, want 1/1/1", fake.opens, fake.hashes, fake.closes)
	}
}

func TestSessionPipelineCancellation(t *testing.T) {
	job, deps, fake := newSessionPipelineTest(t, worker.MediaVideo,
		worker.MaskSHA512|worker.MaskVideo6F|worker.MaskVideoContactSheet, worker.FrameMaskFull)
	temporary := filepath.Join(t.TempDir(), "contact.tmp.jpg")
	paths := ContactSheetPaths{TempJPEG: temporary, TempSidecar: temporary + ".json"}
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
		if err := os.WriteFile(paths.TempSidecar, []byte("temp"), 0o644); err != nil {
			t.Fatal(err)
		}
		cancel()
	}

	result, err := processMediaWithDeps(ctx, sessionPipelineTestConfig(), job, deps)
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	assertSessionPipelineCleared(t, result, fake, paths)
}

func TestSessionPipelineStale(t *testing.T) {
	job, deps, fake := newSessionPipelineTest(t, worker.MediaVideo,
		worker.MaskSHA512|worker.MaskVideo6F|worker.MaskVideoContactSheet, worker.FrameMaskFull)
	temporary := filepath.Join(t.TempDir(), "contact.tmp.jpg")
	paths := ContactSheetPaths{TempJPEG: temporary, TempSidecar: temporary + ".json"}
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
	assertSessionPipelineCleared(t, result, fake, paths)
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
		deps.publishContactSheet = func(ContactSheetPaths, ContactSheetMeta, func() error) error {
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
	assertSessionPipelineCleared(t, result, fake, ContactSheetPaths{})
}

func TestSessionPipelineLegacyThumbValidatesAndPublishes(t *testing.T) {
	t.Run("present cache needs a valid contact sheet", func(t *testing.T) {
		job, deps, fake := newSessionPipelineTest(t, worker.MediaVideo, worker.MaskSHA512|worker.MaskVideoThumb, 0)
		meta := testContactSheetMeta(fake.sha, job.Size)
		deps.contactSheetLookup = func(string, [64]byte) (ContactSheetMeta, bool, error) { return meta, true, nil }
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

	t.Run("missing legacy maps duration and contact then publishes", func(t *testing.T) {
		job, deps, fake := newSessionPipelineTest(t, worker.MediaVideo, worker.MaskSHA512|worker.MaskVideoThumb, 0)
		temporary := filepath.Join(t.TempDir(), "grid.jpg.tmp")
		paths := ContactSheetPaths{TempJPEG: temporary, TempSidecar: temporary + ".json"}
		deps.contactSheetPaths = func(string, [64]byte, int, int64, string) (ContactSheetPaths, error) { return paths, nil }
		deps.query = sessionPipelineMissingReply(job, worker.MaskVideoThumb, 0)
		published := 0
		deps.publishContactSheet = func(ContactSheetPaths, ContactSheetMeta, func() error) error { published++; return nil }
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

func TestSessionPipelineCachedFramesAreNotFabricated(t *testing.T) {
	job, deps, fake := newSessionPipelineTest(t, worker.MediaVideo, worker.MaskSHA512|worker.MaskVideo6F, worker.FrameMaskFull)
	deps.query = func(*worker.SHAQueryMsg) (*worker.SHAReplyMsg, error) {
		return &worker.SHAReplyMsg{JobID: job.JobID, Found: true, RequestedFields: worker.MaskVideo6F, FieldsPresent: worker.MaskVideo6F,
			RequestedFrames: worker.FrameMaskFull, FramesPresent: worker.FrameMaskFull}, nil
	}
	result, err := processMediaWithDeps(context.Background(), sessionPipelineTestConfig(), job, deps)
	if err != nil {
		t.Fatal(err)
	}
	if fake.analyzes != 0 || result.FramesDone != 0 || result.FieldsDone&worker.MaskVideo6F != 0 {
		t.Fatalf("cached frames were fabricated: %#v analyze=%d", result, fake.analyzes)
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
	sha        [64]byte
	result     videocore.AnalysisResult
	opens      int
	hashes     int
	analyzes   int
	closes     int
	request    videocore.AnalysisRequest
	onAnalyze  func(videocore.AnalysisRequest)
	analyzeErr error
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

func sessionPipelineMissingReply(job *worker.JobMsg, fields uint32, frames uint8) func(*worker.SHAQueryMsg) (*worker.SHAReplyMsg, error) {
	return func(query *worker.SHAQueryMsg) (*worker.SHAReplyMsg, error) {
		return &worker.SHAReplyMsg{
			JobID: job.JobID, Found: true,
			RequestedFields: query.RequestedFields, MissingFields: fields,
			RequestedFrames: query.RequestedFrames, MissingFrames: frames,
		}, nil
	}
}

func assertSessionPipelineCleared(t *testing.T, result *worker.JobResultMsg, fake *sessionPipelineFake, paths ContactSheetPaths) {
	t.Helper()
	if result == nil {
		t.Fatal("cancellation/stale returned nil result")
	}
	if fake.closes != 1 || len(result.SHA512) != 0 || result.FieldsDone != 0 || result.FramesDone != 0 || len(result.PDQ) != 0 || result.DurationMS != nil || result.ContactSheetWidth != 0 || result.ContactSheetHeight != 0 {
		t.Fatalf("cancellation/stale retained result or did not close once: close=%d result=%#v", fake.closes, result)
	}
	for _, path := range []string{paths.TempJPEG, paths.TempSidecar} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("cancellation/stale left temp %q: %v", path, err)
		}
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
	for index := range fake.sha {
		fake.sha[index] = byte(index)
	}
	job := &worker.JobMsg{
		JobID: 77, ScanTaskID: "scan", Path: `C:\synthetic\input.bin`, Kind: kind,
		Phase: worker.Phase2, FieldsMask: fields, FrameMask: frames,
		Size: info.size, MTimeUnix: info.mtime.Unix(),
	}
	deps := sessionPipelineDeps{
		stat:     func(string) (fs.FileInfo, error) { return info, nil },
		sameFile: func(left, right fs.FileInfo) bool { return left == right },
		runtime: func() (videocore.RuntimeInfo, error) {
			return videocore.RuntimeInfo{Version: "1.0.0", Components: [4]videocore.RuntimeComponent{
				{Name: "avformat", HeaderVersion: 1, RuntimeVersion: 1},
				{Name: "avcodec", HeaderVersion: 1, RuntimeVersion: 1},
				{Name: "avutil", HeaderVersion: 1, RuntimeVersion: 1},
				{Name: "swscale", HeaderVersion: 1, RuntimeVersion: 1},
			}}, nil
		},
		open: func(context.Context, string, videocore.OpenOptions) (mediaSession, error) {
			fake.opens++
			return fake, nil
		},
		query:              func(*worker.SHAQueryMsg) (*worker.SHAReplyMsg, error) { return nil, nil },
		contactSheetLookup: contactSheetLookupNoop,
		contactSheetPaths:  contactSheetPathsNoop,
		publishContactSheet: func(ContactSheetPaths, ContactSheetMeta, func() error) error {
			return nil
		},
		pid:   func() int { return 99 },
		nonce: func() string { return "test" },
		now:   func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
	return job, deps, fake
}

func contactSheetLookupNoop(string, [64]byte) (ContactSheetMeta, bool, error) {
	return ContactSheetMeta{}, false, nil
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
