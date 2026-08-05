package wproc

import (
	"bytes"
	"context"
	"crypto/sha512"
	"errors"
	"hash"
	"io"
	"os"
	"reflect"
	"testing"
	"time"

	"dedup/internal/features"
	"dedup/internal/worker"
	"dedup/internal/wproc/mediacore"
)

func TestPhase2CompileTimeSurface(t *testing.T) {
	cfg := Config{
		Phase2FrameTimeout: 20 * time.Second,
		Phase2FrameMaxSide: 512,
	}
	deps := phase2PipelineDeps{}
	_ = pipelineDeps{phase2: &deps}

	if false {
		job := worker.JobMsg{Phase: worker.Phase2}
		_, _ = processPhase2WithDeps(context.Background(), cfg, &job, deps)
	}
}

func TestPhase2ImageReadsAndDecodesOnceAndEncodesRequestedFields(t *testing.T) {
	job, deps, state := newPhase2ImageHarness([]byte("phase2 image"))

	result, err := processPhase2WithDeps(context.Background(), phase2TestConfig(), job, deps)
	if err != nil {
		t.Fatal(err)
	}
	if state.openCalls != 1 || state.file.bytesRead != len(state.file.data) ||
		state.decodeCalls != 1 || state.gray.phase2Calls != 1 ||
		state.gray.freeCalls != 1 {
		t.Fatalf("work counts = open %d bytes %d decode %d phase2 %d free %d",
			state.openCalls, state.file.bytesRead, state.decodeCalls,
			state.gray.phase2Calls, state.gray.freeCalls)
	}
	if state.hashCalls != 0 {
		t.Fatalf("matching metadata created %d SHA hashers, want 0", state.hashCalls)
	}
	if result.ReadAttempts != 1 || result.DecodeAttempts != 1 ||
		result.ReadNS < 0 || result.DecodeNS < 0 || !result.Decoded {
		t.Fatalf("metrics = %#v", result)
	}
	wantFields := worker.MaskPHashParts | worker.MaskSobelHist
	if result.FieldsDone != wantFields || len(result.Errors) != 0 {
		t.Fatalf("result fields/errors = %#x/%#v", result.FieldsDone, result.Errors)
	}
	if len(result.PHashParts) != 76 || len(result.SobelHist) != 516 {
		t.Fatalf("BLOB lengths = phash %d sobel %d", len(result.PHashParts), len(result.SobelHist))
	}
	parts, err := features.DecodePHashParts(result.PHashParts)
	if err != nil || parts != state.gray.output.PHashParts {
		t.Fatalf("decoded pHash = %#v, %v", parts, err)
	}
	hist, err := features.DecodeSobelHist(result.SobelHist)
	if err != nil || hist != state.gray.output.SobelHist {
		t.Fatalf("decoded Sobel differs: %v", err)
	}
	if !bytes.Equal(result.SHA512, job.KnownSHA) {
		t.Fatalf("SHA = %x, want known SHA %x", result.SHA512, job.KnownSHA)
	}
	original := result.SHA512[0]
	job.KnownSHA[0] ^= 0xff
	if result.SHA512[0] != original {
		t.Fatal("result SHA aliases job KnownSHA")
	}
}

func TestPhase2ImageEncodesOnlyRequestedField(t *testing.T) {
	tests := []struct {
		name      string
		field     uint32
		wantPHash bool
		wantSobel bool
	}{
		{name: "phash", field: worker.MaskPHashParts, wantPHash: true},
		{name: "sobel", field: worker.MaskSobelHist, wantSobel: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			job, deps, _ := newPhase2ImageHarness([]byte("phase2 field"))
			job.FieldsMask = tc.field
			result, err := processPhase2WithDeps(context.Background(), phase2TestConfig(), job, deps)
			if err != nil {
				t.Fatal(err)
			}
			if result.FieldsDone != tc.field ||
				(len(result.PHashParts) != 0) != tc.wantPHash ||
				(len(result.SobelHist) != 0) != tc.wantSobel {
				t.Fatalf("field result = %#v", result)
			}
		})
	}
}

func TestPhase2ImageSharedFailuresEmitOneErrorPerRequestedBit(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*phase2ImageState)
		wantStage string
		wantFree  int
	}{
		{
			name: "corrupt decode",
			mutate: func(state *phase2ImageState) {
				state.decodeErr = errors.New("corrupt image")
			},
			wantStage: "decode",
		},
		{
			name: "small combined phase2",
			mutate: func(state *phase2ImageState) {
				state.gray.phase2Err = errors.New("image too small")
			},
			wantStage: "phase2",
			wantFree:  1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			job, deps, state := newPhase2ImageHarness([]byte("bad phase2 image"))
			tc.mutate(state)
			result, err := processPhase2WithDeps(context.Background(), phase2TestConfig(), job, deps)
			if err != nil {
				t.Fatal(err)
			}
			assertPhase2BitErrors(t, result, tc.wantStage,
				worker.MaskPHashParts, worker.MaskSobelHist)
			if result.FieldsDone != 0 || len(result.PHashParts) != 0 || len(result.SobelHist) != 0 {
				t.Fatalf("failed shared operation published payload: %#v", result)
			}
			if state.gray.freeCalls != tc.wantFree {
				t.Fatalf("Free calls = %d, want %d", state.gray.freeCalls, tc.wantFree)
			}
		})
	}
}

func TestPhase2ImageOversizeReadsBoundedlyWithoutDecode(t *testing.T) {
	job, deps, state := newPhase2ImageHarness([]byte("123456789"))
	cfg := phase2TestConfig()
	cfg.ImageMemBytes = 8
	cfg.ReadChunkBytes = 4

	result, err := processPhase2WithDeps(context.Background(), cfg, job, deps)
	if err != nil {
		t.Fatal(err)
	}
	assertPhase2BitErrors(t, result, "memory",
		worker.MaskPHashParts, worker.MaskSobelHist)
	if state.openCalls != 1 || state.file.bytesRead != len(state.file.data) {
		t.Fatalf("oversize read = opens %d bytes %d", state.openCalls, state.file.bytesRead)
	}
	if state.file.maxReadBuffer != cfg.ReadChunkBytes {
		t.Fatalf("largest read buffer = %d, want %d", state.file.maxReadBuffer, cfg.ReadChunkBytes)
	}
	if state.decodeCalls != 0 || state.gray.phase2Calls != 0 || state.gray.freeCalls != 0 {
		t.Fatalf("oversize native calls = decode %d phase2 %d free %d",
			state.decodeCalls, state.gray.phase2Calls, state.gray.freeCalls)
	}
}

func TestPhase2ImageMetadataMismatchSameSHAContinues(t *testing.T) {
	job, deps, state := newPhase2ImageHarness([]byte("same content"))
	job.Size++
	job.MTimeMS--
	cfg := phase2TestConfig()
	cfg.ImageMemBytes = int64(len(state.file.data))

	result, err := processPhase2WithDeps(context.Background(), cfg, job, deps)
	if err != nil {
		t.Fatal(err)
	}
	if state.hashCalls != 1 || state.decodeCalls != 1 || len(result.Errors) != 0 {
		t.Fatalf("same-SHA mismatch = hash %d decode %d result %#v",
			state.hashCalls, state.decodeCalls, result)
	}
	if result.FieldsDone != worker.MaskPHashParts|worker.MaskSobelHist {
		t.Fatalf("fields = %#x", result.FieldsDone)
	}
}

func TestPhase2ImageContentMismatchIsStale(t *testing.T) {
	job, deps, state := newPhase2ImageHarness([]byte("new content"))
	old := sha512.Sum512([]byte("old content"))
	job.KnownSHA = append([]byte(nil), old[:]...)
	job.MTimeMS--

	result, err := processPhase2WithDeps(context.Background(), phase2TestConfig(), job, deps)
	if err != nil {
		t.Fatal(err)
	}
	assertPhase2Stale(t, result, job.KnownSHA)
	if state.hashCalls != 1 || state.decodeCalls != 0 {
		t.Fatalf("stale work = hash %d decode %d", state.hashCalls, state.decodeCalls)
	}
}

func TestPhase2ImageIdentityDriftDiscardsAllPayload(t *testing.T) {
	tests := []struct {
		name            string
		mutate          func(*phase2ImageState)
		wantBytesRead   bool
		wantDecodeCalls int
		wantPhase2Calls int
		wantFreeCalls   int
	}{
		{
			name: "opened handle substitution",
			mutate: func(state *phase2ImageState) {
				state.file.handleStats[0].identity = "substituted-handle"
			},
		},
		{
			name: "mid-read handle drift",
			mutate: func(state *phase2ImageState) {
				state.file.handleStats[1] = phase2Info{
					size: int64(len(state.file.data)), mtimeMS: 123000,
					identity: "replacement-handle",
				}
			},
			wantBytesRead: true,
		},
		{
			name: "post-decode path drift",
			mutate: func(state *phase2ImageState) {
				state.pathStats[2] = phase2Info{
					size: int64(len(state.file.data)) + 1, mtimeMS: 124000,
					identity: "replacement-path",
				}
			},
			wantBytesRead: true, wantDecodeCalls: 1, wantPhase2Calls: 1, wantFreeCalls: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			job, deps, state := newPhase2ImageHarness([]byte("identity image"))
			tc.mutate(state)
			result, err := processPhase2WithDeps(context.Background(), phase2TestConfig(), job, deps)
			if err != nil {
				t.Fatal(err)
			}
			assertPhase2Stale(t, result, job.KnownSHA)
			if (state.file.bytesRead > 0) != tc.wantBytesRead ||
				state.decodeCalls != tc.wantDecodeCalls ||
				state.gray.phase2Calls != tc.wantPhase2Calls ||
				state.gray.freeCalls != tc.wantFreeCalls {
				t.Fatalf("work = bytes %d decode %d phase2 %d free %d",
					state.file.bytesRead, state.decodeCalls,
					state.gray.phase2Calls, state.gray.freeCalls)
			}
		})
	}
}

func TestPhase2ImagePreOwnershipFailuresAreFileLevel(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*phase2PipelineDeps, *phase2ImageState)
		wantStage string
	}{
		{
			name: "stat",
			mutate: func(deps *phase2PipelineDeps, _ *phase2ImageState) {
				deps.stat = func(string) (os.FileInfo, error) {
					return nil, errors.New("stat denied")
				}
			},
			wantStage: "stat",
		},
		{
			name: "open",
			mutate: func(deps *phase2PipelineDeps, _ *phase2ImageState) {
				deps.open = func(string) (readStatCloser, error) {
					return nil, errors.New("open denied")
				}
			},
			wantStage: "open",
		},
		{
			name: "hash",
			mutate: func(deps *phase2PipelineDeps, _ *phase2ImageState) {
				deps.newHash = func() (hash.Hash, error) {
					return nil, errors.New("hash unavailable")
				}
			},
			wantStage: "hash",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			job, deps, state := newPhase2ImageHarness([]byte("failure image"))
			if tc.wantStage == "hash" {
				job.MTimeMS--
			}
			tc.mutate(&deps, state)
			result, err := processPhase2WithDeps(context.Background(), phase2TestConfig(), job, deps)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Errors) != 1 || result.Errors[0].Field != 0 ||
				result.Errors[0].Stage != tc.wantStage {
				t.Fatalf("errors = %#v, want file-level %s", result.Errors, tc.wantStage)
			}
			if result.FieldsDone != 0 || len(result.PHashParts) != 0 || len(result.SobelHist) != 0 {
				t.Fatalf("failure published payload: %#v", result)
			}
		})
	}
}

func TestPhase2ImageRejectsInvalidJobBeforeIO(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*worker.JobMsg)
	}{
		{name: "negative size", mutate: func(job *worker.JobMsg) { job.Size = -1 }},
		{name: "negative mtime", mutate: func(job *worker.JobMsg) { job.MTimeMS = -1 }},
		{name: "short known SHA", mutate: func(job *worker.JobMsg) { job.KnownSHA = job.KnownSHA[:63] }},
		{name: "empty fields", mutate: func(job *worker.JobMsg) { job.FieldsMask = 0 }},
		{name: "phase1 field", mutate: func(job *worker.JobMsg) { job.FieldsMask = worker.MaskSHA512 }},
		{name: "video field", mutate: func(job *worker.JobMsg) { job.FieldsMask = worker.MaskVideo6F }},
		{name: "frame mask", mutate: func(job *worker.JobMsg) { job.FrameMask = 1 }},
		{name: "duration", mutate: func(job *worker.JobMsg) { job.DurationMS = 1 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			job, deps, state := newPhase2ImageHarness([]byte("validate image"))
			tc.mutate(job)
			result, err := processPhase2WithDeps(context.Background(), phase2TestConfig(), job, deps)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Errors) != 1 || result.Errors[0].Field != 0 ||
				result.Errors[0].Stage != "validate" {
				t.Fatalf("errors = %#v, want one validate file error", result.Errors)
			}
			if state.openCalls != 0 || state.pathStatCall != 0 || state.decodeCalls != 0 {
				t.Fatalf("invalid job performed IO: opens %d stats %d decodes %d",
					state.openCalls, state.pathStatCall, state.decodeCalls)
			}
		})
	}
}

func TestPhase2ImageIgnoresVideoOnlyConfiguration(t *testing.T) {
	job, deps, _ := newPhase2ImageHarness([]byte("image-only config"))
	cfg := phase2TestConfig()
	cfg.Phase2FrameTimeout = 0
	cfg.Phase2FrameMaxSide = 0
	cfg.IPCMaxFrameBytes = 0

	result, err := processPhase2WithDeps(context.Background(), cfg, job, deps)
	if err != nil {
		t.Fatalf("image rejected ffmpeg-only config: %v", err)
	}
	if result.FieldsDone != worker.MaskPHashParts|worker.MaskSobelHist {
		t.Fatalf("image fields = %#x, errors %#v", result.FieldsDone, result.Errors)
	}
}

func TestPhase2ImageDetectsSubMillisecondSourceDrift(t *testing.T) {
	job, deps, state := newPhase2ImageHarness([]byte("sub-ms drift"))
	drifted := state.pathStats[0]
	drifted.mtimeNS = drifted.ModTime().UnixNano() + 1
	state.file.handleStats[1] = drifted
	state.file.handleStats[2] = drifted
	state.pathStats[1] = drifted
	state.pathStats[2] = drifted

	result, err := processPhase2WithDeps(context.Background(), phase2TestConfig(), job, deps)
	if err != nil {
		t.Fatal(err)
	}
	assertPhase2Stale(t, result, job.KnownSHA)
}

func phase2TestConfig() Config {
	return Config{
		ReadChunkBytes:     4,
		ImageMemBytes:      1 << 20,
		Phase2FrameTimeout: 20 * time.Second,
		Phase2FrameMaxSide: 512,
		FFmpegPath:         `tools\ffmpeg.exe`,
		IPCMaxFrameBytes:   16 << 20,
	}
}

type phase2Info struct {
	size     int64
	mtimeMS  int64
	mtimeNS  int64
	identity string
}

func (f phase2Info) Name() string      { return "phase2.jpg" }
func (f phase2Info) Size() int64       { return f.size }
func (f phase2Info) Mode() os.FileMode { return 0 }
func (f phase2Info) ModTime() time.Time {
	if f.mtimeNS != 0 {
		return time.Unix(0, f.mtimeNS)
	}
	return time.UnixMilli(f.mtimeMS)
}
func (f phase2Info) IsDir() bool { return false }
func (f phase2Info) Sys() any    { return nil }

type phase2FakeFile struct {
	data          []byte
	offset        int
	bytesRead     int
	maxReadBuffer int
	handleStats   []phase2Info
	statCall      int
	closeCalls    int
}

func (f *phase2FakeFile) Read(p []byte) (int, error) {
	if len(p) > f.maxReadBuffer {
		f.maxReadBuffer = len(p)
	}
	if f.offset >= len(f.data) {
		return 0, io.EOF
	}
	n := copy(p, f.data[f.offset:])
	f.offset += n
	f.bytesRead += n
	return n, nil
}

func (f *phase2FakeFile) Stat() (os.FileInfo, error) {
	index := f.statCall
	if index >= len(f.handleStats) {
		index = len(f.handleStats) - 1
	}
	f.statCall++
	return f.handleStats[index], nil
}

func (f *phase2FakeFile) Close() error {
	f.closeCalls++
	return nil
}

type fakePhase2Gray struct {
	output      mediacore.Phase2Result
	phase2Err   error
	phase2Calls int
	pdqCalls    int
	freeCalls   int
}

func (g *fakePhase2Gray) PDQ256() ([mediacore.PDQ256Bytes]byte, int32, error) {
	g.pdqCalls++
	return [mediacore.PDQ256Bytes]byte{}, 75, nil
}

func (g *fakePhase2Gray) Phase2() (mediacore.Phase2Result, error) {
	g.phase2Calls++
	return g.output, g.phase2Err
}

func (g *fakePhase2Gray) Free() {
	g.freeCalls++
}

type phase2ImageState struct {
	file         *phase2FakeFile
	gray         *fakePhase2Gray
	pathStats    []phase2Info
	pathStatCall int
	openCalls    int
	hashCalls    int
	decodeCalls  int
	decodeErr    error
	decodeInput  []byte
}

func newPhase2ImageHarness(data []byte) (*worker.JobMsg, phase2PipelineDeps, *phase2ImageState) {
	info := phase2Info{size: int64(len(data)), mtimeMS: 123000, identity: "path"}
	file := &phase2FakeFile{
		data: append([]byte(nil), data...),
		handleStats: []phase2Info{
			info, info, info,
		},
	}
	gray := &fakePhase2Gray{}
	gray.output.PHashParts = [9]uint64{1, 2, 3, 4, 5, 6, 7, 8, 9}
	gray.output.SobelHist[0] = 1
	gray.output.SobelHist[127] = 2
	state := &phase2ImageState{
		file: file,
		gray: gray,
		pathStats: []phase2Info{
			info, info, info,
		},
	}
	deps := phase2PipelineDeps{
		open: func(string) (readStatCloser, error) {
			state.openCalls++
			return file, nil
		},
		stat: func(string) (os.FileInfo, error) {
			index := state.pathStatCall
			if index >= len(state.pathStats) {
				index = len(state.pathStats) - 1
			}
			state.pathStatCall++
			return state.pathStats[index], nil
		},
		sameFile: func(left, right os.FileInfo) bool {
			return left.(phase2Info).identity == right.(phase2Info).identity
		},
		newHash: func() (hash.Hash, error) {
			state.hashCalls++
			return sha512.New(), nil
		},
		decode: func(input []byte) (phase2GrayImage, error) {
			state.decodeCalls++
			state.decodeInput = append([]byte(nil), input...)
			if state.decodeErr != nil {
				return nil, state.decodeErr
			}
			return gray, nil
		},
	}
	sum := sha512.Sum512(data)
	job := &worker.JobMsg{
		JobID: 501, Path: `C:\media\phase2.jpg`, Kind: worker.MediaImage,
		Phase:      worker.Phase2,
		FieldsMask: worker.MaskPHashParts | worker.MaskSobelHist,
		Size:       info.size, MTimeMS: info.mtimeMS,
		KnownSHA: append([]byte(nil), sum[:]...),
	}
	return job, deps, state
}

func assertPhase2BitErrors(t *testing.T, result *worker.JobResultMsg, stage string, fields ...uint32) {
	t.Helper()
	if len(result.Errors) != len(fields) {
		t.Fatalf("errors = %#v, want %d", result.Errors, len(fields))
	}
	got := make(map[uint32]string, len(result.Errors))
	for _, fieldError := range result.Errors {
		if fieldError.Field == 0 || fieldError.Field&(fieldError.Field-1) != 0 {
			t.Fatalf("non-single-bit error = %#v", fieldError)
		}
		got[fieldError.Field] = fieldError.Stage
	}
	want := make(map[uint32]string, len(fields))
	for _, field := range fields {
		want[field] = stage
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("errors = %#v, want fields/stage %#v", got, want)
	}
}

func assertPhase2Stale(t *testing.T, result *worker.JobResultMsg, knownSHA []byte) {
	t.Helper()
	if result.FieldsDone != 0 || len(result.PHashParts) != 0 ||
		len(result.SobelHist) != 0 || len(result.Frames) != 0 {
		t.Fatalf("stale result retained payload: %#v", result)
	}
	if len(result.Errors) != 1 || result.Errors[0].Field != 0 ||
		result.Errors[0].Stage != "stale" {
		t.Fatalf("stale errors = %#v", result.Errors)
	}
	if !bytes.Equal(result.SHA512, knownSHA) {
		t.Fatalf("stale SHA = %x, want %x", result.SHA512, knownSHA)
	}
}
