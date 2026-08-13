package wproc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dedup/internal/store"
	"dedup/internal/worker"
	"dedup/internal/wproc/mediacore"
)

const legacyPhase1VideoMask = worker.MaskSHA512 | worker.MaskVideoThumb

func TestVideoQueriesSHABeforeProbeAndThumbnailWork(t *testing.T) {
	file := newFakeFile([]byte("video"), 5, 123)
	deps, state := testVideoPipelineDeps(file)
	job := testVideoJob(701)

	result, err := processVideoWithDeps(context.Background(), testVideoConfig(t.TempDir()), job, deps)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(state.events, ","); got != "query,probe,cache,ffmpeg,thumb-read,thumb-pdq" {
		t.Fatalf("event order = %q, want SHA query before probe/thumbnail work", got)
	}
	if result.FieldsDone != legacyPhase1VideoMask || result.DurationMS == nil || *result.DurationMS != 5000 {
		t.Fatalf("owner result = %#v", result)
	}
	if !result.ThumbGenerated || result.ThumbCacheHit || !result.Decoded {
		t.Fatalf("owner metrics = generated %v hit %v decoded %v", result.ThumbGenerated, result.ThumbCacheHit, result.Decoded)
	}
	if result.ReadAttempts != 1 || result.DecodeAttempts != 1 ||
		result.ReadNS < 0 || result.DecodeNS < 0 {
		t.Fatalf("owner timing metrics = %#v", result)
	}
}

func TestVideoCompleteHitSkipsProbeFFmpegAndThumbPDQ(t *testing.T) {
	file := newFakeFile([]byte("video"), 5, 123)
	deps, state := testVideoPipelineDeps(file)
	duration := int64(5000)
	quality := int32(88)
	state.queryReply = &worker.SHAReplyMsg{
		JobID: 702, Found: true, DurationMS: &duration,
		ThumbPath: `C:\cache\thumb.jpg`, ThumbPDQ: bytes.Repeat([]byte{7}, 32), ThumbQuality: &quality,
	}
	result, err := processVideoWithDeps(context.Background(), testVideoConfig(t.TempDir()), testVideoJob(702), deps)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(state.events, ","); got != "query" {
		t.Fatalf("complete hit events = %q, want query only", got)
	}
	if result.FieldsDone != legacyPhase1VideoMask || result.Decoded || result.ThumbGenerated || result.ThumbCacheHit {
		t.Fatalf("complete hit result = %#v", result)
	}
}

func TestVideoIncompleteFoundReplyIsFatal(t *testing.T) {
	file := newFakeFile([]byte("video"), 5, 123)
	deps, state := testVideoPipelineDeps(file)
	duration := int64(5000)
	state.queryReply = &worker.SHAReplyMsg{JobID: 703, Found: true, DurationMS: &duration}
	_, err := processVideoWithDeps(context.Background(), testVideoConfig(t.TempDir()), testVideoJob(703), deps)
	if err == nil || !strings.Contains(err.Error(), "incompatible SHA reply") {
		t.Fatalf("error = %v, want incompatible SHA reply", err)
	}
	if got := strings.Join(state.events, ","); got != "query" {
		t.Fatalf("incomplete hit events = %q, want query only", got)
	}
}

func TestVideoPartialFieldResultsPreserveSuccessfulBundleMembers(t *testing.T) {
	tests := []struct {
		name          string
		mutate        func(*videoTestState)
		wantDuration  bool
		wantThumb     bool
		wantStage     string
		wantSeek      float64
		wantGenerated bool
	}{
		{
			name: "duration only after ffmpeg failure",
			mutate: func(s *videoTestState) {
				s.generateErr = errors.New("encoder failed")
			},
			wantDuration: true, wantStage: "ffmpeg", wantSeek: 2.5,
		},
		{
			name: "thumbnail only after probe failure falls back to zero",
			mutate: func(s *videoTestState) {
				s.probeErr = errors.New("no duration")
			},
			wantThumb: true, wantStage: "ffprobe", wantSeek: 0, wantGenerated: true,
		},
		{
			name: "thumbnail read failure",
			mutate: func(s *videoTestState) {
				s.readErr = errors.New("thumbnail disappeared")
			},
			wantDuration: true, wantStage: "thumb_pdq", wantSeek: 2.5, wantGenerated: true,
		},
		{
			name: "thumbnail PDQ failure",
			mutate: func(s *videoTestState) {
				s.decodeErr = errors.New("bad jpeg")
			},
			wantDuration: true, wantStage: "thumb_pdq", wantSeek: 2.5, wantGenerated: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			file := newFakeFile([]byte("video"), 5, 123)
			deps, state := testVideoPipelineDeps(file)
			tc.mutate(state)
			result, err := processVideoWithDeps(context.Background(), testVideoConfig(t.TempDir()), testVideoJob(704), deps)
			if err != nil {
				t.Fatal(err)
			}
			if result.FieldsDone&worker.MaskVideoThumb == 0 {
				t.Fatalf("partial result fields = %#x, want bundle bit set", result.FieldsDone)
			}
			if (result.DurationMS != nil) != tc.wantDuration {
				t.Fatalf("duration present = %v, want %v", result.DurationMS != nil, tc.wantDuration)
			}
			if (len(result.ThumbPDQ) == 32) != tc.wantThumb {
				t.Fatalf("thumb PDQ length = %d, want present %v", len(result.ThumbPDQ), tc.wantThumb)
			}
			if len(result.Errors) != 1 || result.Errors[0].Stage != tc.wantStage {
				t.Fatalf("errors = %#v, want stage %q", result.Errors, tc.wantStage)
			}
			if state.seek != tc.wantSeek {
				t.Fatalf("seek = %v, want %v", state.seek, tc.wantSeek)
			}
			if result.ThumbGenerated != tc.wantGenerated {
				t.Fatalf("ThumbGenerated = %v, want %v", result.ThumbGenerated, tc.wantGenerated)
			}
		})
	}
}

func TestVideoCacheHitStillReadsAndHashesThumbnail(t *testing.T) {
	file := newFakeFile([]byte("video"), 5, 123)
	deps, state := testVideoPipelineDeps(file)
	state.cacheHit = true
	result, err := processVideoWithDeps(context.Background(), testVideoConfig(t.TempDir()), testVideoJob(705), deps)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(state.events, ","); got != "query,probe,cache,thumb-read,thumb-pdq" {
		t.Fatalf("cache hit events = %q", got)
	}
	if !result.ThumbCacheHit || result.ThumbGenerated || len(result.ThumbPDQ) != 32 {
		t.Fatalf("cache-hit result = %#v", result)
	}
}

func TestVideoStatDriftStopsBeforeSHAQuery(t *testing.T) {
	file := newFakeFile([]byte("video"), 5, 123)
	deps, state := testVideoPipelineDeps(file)
	state.pathStats[0] = fakeInfo{size: 6, mtime: 124, identity: "path"}
	result, err := processVideoWithDeps(context.Background(), testVideoConfig(t.TempDir()), testVideoJob(706), deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) != 1 || result.Errors[0].Stage != "stat" {
		t.Fatalf("drift result = %#v", result)
	}
	if len(state.events) != 0 {
		t.Fatalf("drift events = %v, want none", state.events)
	}
}

func TestVideoStatDriftDuringThumbnailDoesNotCommitOrPublishBundle(t *testing.T) {
	file := newFakeFile([]byte("video"), 5, 123)
	deps, state := testVideoPipelineDeps(file)
	state.pathStats = append(state.pathStats, fakeInfo{size: 6, mtime: 124, identity: "path"})
	result, err := processVideoWithDeps(context.Background(), testVideoConfig(t.TempDir()), testVideoJob(707), deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) != 1 || result.Errors[0].Stage != "stat" {
		t.Fatalf("late drift result errors = %#v, want one stat error", result.Errors)
	}
	if result.FieldsDone != 0 || result.DurationMS != nil || result.ThumbPath != "" || len(result.ThumbPDQ) != 0 {
		t.Fatalf("late drift published stale fields: %#v", result)
	}
	if state.writeMetaCalls != 0 {
		t.Fatalf("sidecar writes = %d after source drift, want 0", state.writeMetaCalls)
	}
}

func TestVideoStatDriftDuringThumbnailPDQDoesNotCommitSidecar(t *testing.T) {
	file := newFakeFile([]byte("video"), 5, 123)
	deps, state := testVideoPipelineDeps(file)
	state.pathStats = append(state.pathStats,
		fakeInfo{size: 5, mtime: 123, identity: "path"},
		fakeInfo{size: 6, mtime: 124, identity: "path"},
	)
	result, err := processVideoWithDeps(context.Background(), testVideoConfig(t.TempDir()), testVideoJob(708), deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) != 1 || result.Errors[0].Stage != "stat" {
		t.Fatalf("PDQ-time drift errors = %#v, want one stat error", result.Errors)
	}
	if result.FieldsDone != 0 || result.DurationMS != nil || result.ThumbPath != "" || len(result.ThumbPDQ) != 0 {
		t.Fatalf("PDQ-time drift published stale fields: %#v", result)
	}
	if state.writeMetaCalls != 0 {
		t.Fatalf("sidecar writes = %d before final PDQ drift check, want 0", state.writeMetaCalls)
	}
}

func TestVideoPublishConflictNeverEmitsThumbnailFromDifferentCacheFile(t *testing.T) {
	file := newFakeFile([]byte("video"), 5, 123)
	deps, state := testVideoPipelineDeps(file)
	state.writeMetaErr = fmt.Errorf("%w: writer A lost to writer B", errThumbnailPublishConflict)
	result, err := processVideoWithDeps(context.Background(), testVideoConfig(t.TempDir()), testVideoJob(709), deps)
	if err != nil {
		t.Fatal(err)
	}
	if result.DurationMS == nil || *result.DurationMS != 5000 {
		t.Fatalf("safe duration partial was lost: %#v", result.DurationMS)
	}
	if result.FieldsDone != legacyPhase1VideoMask {
		t.Fatalf("duration partial fields = %#x, want frozen bundle bit plus SHA", result.FieldsDone)
	}
	if result.ThumbPath != "" || len(result.ThumbPDQ) != 0 || result.ThumbQuality != nil ||
		result.Width != 0 || result.Height != 0 || result.Decoded {
		t.Fatalf("publish conflict emitted inconsistent thumbnail bundle: %#v", result)
	}
	if len(result.Errors) != 1 || result.Errors[0].Stage != "thumb_cache" || result.Errors[0].Field != 0 {
		t.Fatalf("publish conflict errors = %#v, want one cache publication error", result.Errors)
	}
}

func TestVideoRealFiveSecondTwoPassCacheAndMTimeInvalidation(t *testing.T) {
	if mediacore.Version() == "" {
		t.Skip("mediacore cgo binding unavailable")
	}
	ffmpeg, ffprobe := repositoryFFmpegTools(t)
	root := t.TempDir()
	video := filepath.Join(root, "cache source.mp4")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	runner := execCommandRunner{}
	if _, stderr, err := runner.Run(ctx, ffmpeg, []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=duration=5:size=320x240:rate=15",
		"-pix_fmt", "yuv420p", "-y", video,
	}); err != nil {
		t.Fatalf("generate real cache video: %v: %s", err, stderr)
	}
	cfg := testVideoConfig(filepath.Join(root, "thumbcache"))
	cfg.FFmpegPath = ffmpeg
	cfg.FFprobePath = ffprobe
	generateCalls := 0

	run := func(id int64) *worker.JobResultMsg {
		t.Helper()
		info, err := os.Stat(video)
		if err != nil {
			t.Fatal(err)
		}
		deps := defaultVideoPipelineDeps(func(query *worker.SHAQueryMsg) (*worker.SHAReplyMsg, error) {
			return &worker.SHAReplyMsg{JobID: query.JobID}, nil
		})
		realGenerate := deps.generate
		deps.generate = func(ctx context.Context, cfg Config, source string, seek float64, destination string) (string, error) {
			generateCalls++
			return realGenerate(ctx, cfg, source, seek, destination)
		}
		job := &worker.JobMsg{
			JobID: id, Path: video, Kind: worker.MediaVideo, Phase: worker.Phase1,
			FieldsMask: legacyPhase1VideoMask, Size: info.Size(), MTimeUnix: info.ModTime().Unix(),
		}
		result, err := processVideoWithDeps(ctx, cfg, job, deps)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Errors) != 0 {
			t.Fatalf("pass %d errors = %#v", id, result.Errors)
		}
		if result.DurationMS == nil || *result.DurationMS < 4900 || *result.DurationMS > 5100 ||
			len(result.ThumbPDQ) != 32 || result.ThumbQuality == nil {
			t.Fatalf("pass %d incomplete result = %#v", id, result)
		}
		return result
	}

	first := run(801)
	if !first.ThumbGenerated || first.ThumbCacheHit || generateCalls != 1 {
		t.Fatalf("first pass = generated %v hit %v calls %d", first.ThumbGenerated, first.ThumbCacheHit, generateCalls)
	}
	firstInfo, err := os.Stat(first.ThumbPath)
	if err != nil {
		t.Fatal(err)
	}
	second := run(802)
	if second.ThumbGenerated || !second.ThumbCacheHit || generateCalls != 1 {
		t.Fatalf("second pass = generated %v hit %v calls %d", second.ThumbGenerated, second.ThumbCacheHit, generateCalls)
	}
	secondInfo, err := os.Stat(second.ThumbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !firstInfo.ModTime().Equal(secondInfo.ModTime()) || firstInfo.Size() != secondInfo.Size() {
		t.Fatalf("cache hit rewrote thumbnail: before=%v/%d after=%v/%d",
			firstInfo.ModTime(), firstInfo.Size(), secondInfo.ModTime(), secondInfo.Size())
	}

	sourceInfo, err := os.Stat(video)
	if err != nil {
		t.Fatal(err)
	}
	changed := time.Unix(sourceInfo.ModTime().Unix()+2, 0)
	if err := os.Chtimes(video, changed, changed); err != nil {
		t.Fatal(err)
	}
	third := run(803)
	if !third.ThumbGenerated || third.ThumbCacheHit || generateCalls != 2 {
		t.Fatalf("mtime pass = generated %v hit %v calls %d", third.ThumbGenerated, third.ThumbCacheHit, generateCalls)
	}
}

func TestPackagedWorkerProcessesVideoWithCleanPATH(t *testing.T) {
	if mediacore.Version() == "" {
		t.Skip("mediacore cgo binding unavailable")
	}
	ffmpeg, _ := repositoryFFmpegTools(t)
	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	workerExe := filepath.Join(repositoryRoot, "bin", "worker.exe")
	if _, err := os.Stat(workerExe); err != nil {
		t.Skipf("packaged worker missing: %s", workerExe)
	}
	root := t.TempDir()
	video := filepath.Join(root, "packaged worker source.mp4")
	runner := execCommandRunner{}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, stderr, err := runner.Run(ctx, ffmpeg, []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=5:size=320x180:rate=15",
		"-pix_fmt", "yuv420p", "-y", video,
	}); err != nil {
		t.Fatalf("generate packaged-worker video: %v: %s", err, stderr)
	}
	info, err := os.Stat(video)
	if err != nil {
		t.Fatal(err)
	}
	emptyPath := filepath.Join(root, "empty-path")
	if err := os.MkdirAll(emptyPath, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", emptyPath)
	featureStore := &videoBlackBoxStore{}
	pool := worker.NewPool(worker.Config{
		WorkerExe:       workerExe,
		WorkerCount:     1,
		MachineID:       "task7-blackbox",
		ReadyTimeout:    10 * time.Second,
		VideoTimeout:    30 * time.Second,
		ShutdownTimeout: 3 * time.Second,
		WorkerEnv: []string{
			"WPROC_THUMB_CACHE=" + filepath.Join(root, "thumbcache"),
		},
	}, featureStore, nil, nil, nil)
	pool.Start()
	defer pool.Close()
	job := &worker.JobMsg{
		JobID: 901, Path: video, Kind: worker.MediaVideo, Phase: worker.Phase1,
		FieldsMask: legacyPhase1VideoMask, Size: info.Size(), MTimeUnix: info.ModTime().Unix(),
	}
	if err := pool.Submit(job); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-pool.Results():
		if result == nil {
			t.Fatal("packaged worker result channel closed")
		}
		if len(result.Errors) != 0 || result.FieldsDone != legacyPhase1VideoMask ||
			result.DurationMS == nil || *result.DurationMS < 4900 || *result.DurationMS > 5100 ||
			len(result.ThumbPDQ) != 32 || !result.ThumbGenerated {
			t.Fatalf("packaged clean-PATH video result = %#v", result)
		}
	case <-ctx.Done():
		t.Fatalf("packaged worker timed out with clean PATH: %v", ctx.Err())
	}
}

type videoBlackBoxStore struct{}

func (*videoBlackBoxStore) LookupContent(context.Context, []byte, store.MediaKind, uint32, uint8) (store.ContentState, error) {
	return store.ContentState{}, nil
}

func (*videoBlackBoxStore) SaveAnalysis(context.Context, store.AnalysisResult) (store.CommittedState, error) {
	return store.CommittedState{}, nil
}

func (*videoBlackBoxStore) LookupImage(context.Context, []byte) (*store.ImageFeature, error) {
	return nil, nil
}

func (*videoBlackBoxStore) LookupVideo(context.Context, []byte) (*store.VideoFeature, error) {
	return nil, nil
}

func (*videoBlackBoxStore) SavePhase1(context.Context, store.Phase1Result) error {
	return nil
}

func (*videoBlackBoxStore) SavePhase2(context.Context, store.Phase2Result) error {
	return nil
}

func (*videoBlackBoxStore) Phase2MissingMask(context.Context, string, string) (uint32, error) {
	return 0, nil
}

func (*videoBlackBoxStore) MarkCrash(context.Context, string, string, string) error {
	return nil
}

type videoTestState struct {
	events         []string
	queryReply     *worker.SHAReplyMsg
	probeErr       error
	generateErr    error
	readErr        error
	decodeErr      error
	cacheHit       bool
	seek           float64
	pathStats      []fakeInfo
	pathStatCall   int
	writeMetaCalls int
	writeMetaErr   error
}

func testVideoPipelineDeps(file *fakeFile) (videoPipelineDeps, *videoTestState) {
	state := &videoTestState{
		pathStats: []fakeInfo{
			{size: 5, mtime: 123, identity: "path"},
			{size: 5, mtime: 123, identity: "path"},
		},
	}
	deps := videoPipelineDeps{
		open: func(string) (readStatCloser, error) { return file, nil },
		stat: func(string) (os.FileInfo, error) {
			index := state.pathStatCall
			if index >= len(state.pathStats) {
				index = len(state.pathStats) - 1
			}
			state.pathStatCall++
			return state.pathStats[index], nil
		},
		sameFile: func(left, right os.FileInfo) bool {
			return left.(fakeInfo).identity == right.(fakeInfo).identity
		},
		newSHA: func() (sha512Stream, error) { return &fakeSHA{}, nil },
		query: func(query *worker.SHAQueryMsg) (*worker.SHAReplyMsg, error) {
			state.events = append(state.events, "query")
			if state.queryReply != nil {
				return state.queryReply, nil
			}
			return &worker.SHAReplyMsg{JobID: query.JobID}, nil
		},
		probe: func(context.Context, Config, string) (int64, error) {
			state.events = append(state.events, "probe")
			return 5000, state.probeErr
		},
		cache: func(Config, string, os.FileInfo) (string, bool, string, error) {
			state.events = append(state.events, "cache")
			return `C:\cache\thumb.jpg`, state.cacheHit, bytesSHA256Hex([]byte("jpeg")), nil
		},
		generate: func(_ context.Context, _ Config, _ string, seek float64, _ string) (string, error) {
			state.events = append(state.events, "ffmpeg")
			state.seek = seek
			return bytesSHA256Hex([]byte("jpeg")), state.generateErr
		},
		writeMeta: func(Config, string, os.FileInfo, string) error {
			state.writeMetaCalls++
			return state.writeMetaErr
		},
		readThumb: func(string) ([]byte, error) {
			state.events = append(state.events, "thumb-read")
			return []byte("jpeg"), state.readErr
		},
		decodeThumb: func([]byte) (imagePhase1, error) {
			state.events = append(state.events, "thumb-pdq")
			return imagePhase1{Hash: bytes.Repeat([]byte{9}, 32), Quality: 81, Width: 256, Height: 144}, state.decodeErr
		},
	}
	return deps, state
}

func testVideoJob(id int64) *worker.JobMsg {
	return &worker.JobMsg{
		JobID: id, Path: `C:\media\video.mp4`, Kind: worker.MediaVideo,
		Phase: worker.Phase1, FieldsMask: legacyPhase1VideoMask, Size: 5, MTimeUnix: 123,
	}
}
