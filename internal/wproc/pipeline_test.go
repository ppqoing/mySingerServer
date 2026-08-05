package wproc

import (
	"bytes"
	"errors"
	"io"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"dedup/internal/worker"
)

func TestReadImageUsesOneOpenAndExactFourMiBReads(t *testing.T) {
	const chunk = 4 << 20
	data := bytes.Repeat([]byte{0x5a}, chunk+17)
	file := newFakeFile(data, int64(len(data)), 123)
	openCalls := 0
	deps, _ := testPipelineDeps(file)
	deps.open = func(string) (readStatCloser, error) {
		openCalls++
		return file, nil
	}
	job := &worker.JobMsg{
		JobID: 1, Path: `C:\media\a.jpg`, Kind: worker.MediaImage,
		FieldsMask: worker.MaskAllImage, Size: int64(len(data)), MTimeUnix: 123,
	}

	result, err := processImageWithDeps(testConfig(), job, deps)
	if err != nil {
		t.Fatal(err)
	}
	if openCalls != 1 {
		t.Fatalf("open calls = %d, want exactly 1", openCalls)
	}
	if len(file.requested) < 2 {
		t.Fatalf("read calls = %d, want at least 2", len(file.requested))
	}
	if len(file.requested) != 3 || file.offset != len(data) {
		t.Fatalf("read instrumentation = calls %d bytes %d, want exactly 3 calls and %d bytes", len(file.requested), file.offset, len(data))
	}
	for i, n := range file.requested {
		if n != chunk {
			t.Fatalf("Read call %d buffer = %d, want exactly %d", i, n, chunk)
		}
	}
	if result.FieldsDone != worker.MaskAllImage {
		t.Fatalf("fields done = %#x, want %#x", result.FieldsDone, worker.MaskAllImage)
	}
}

func TestRetentionBoundary(t *testing.T) {
	const capBytes = int64(256 << 20)
	if !shouldRetainImage(capBytes, capBytes) {
		t.Fatal("image exactly at 256 MiB was not retained")
	}
	if !shouldRetainImage(capBytes-1, capBytes) {
		t.Fatal("image below 256 MiB was not retained")
	}
	if shouldRetainImage(capBytes+1, capBytes) {
		t.Fatal("image above 256 MiB was retained")
	}
}

func TestRetentionAtAndBelowConfiguredLimitAllowsDecode(t *testing.T) {
	for _, size := range []int{7, 8} {
		t.Run(string(rune('0'+size)), func(t *testing.T) {
			data := bytes.Repeat([]byte{0x44}, size)
			file := newFakeFile(data, int64(size), 123)
			deps, state := testPipelineDeps(file)
			cfg := testConfig()
			cfg.ImageMemBytes = 8
			job := &worker.JobMsg{
				JobID: int64(size), Path: `C:\media\boundary.jpg`, Kind: worker.MediaImage,
				FieldsMask: worker.MaskAllImage, Size: int64(size), MTimeUnix: 123,
			}
			result, err := processImageWithDeps(cfg, job, deps)
			if err != nil {
				t.Fatal(err)
			}
			if !result.Decoded || state.decodeCalls != 1 {
				t.Fatalf("size %d: decoded=%v calls=%d, want retained decode", size, result.Decoded, state.decodeCalls)
			}
		})
	}
}

func TestImageAboveRetentionCapIsReadOnceAndReturnsStructuredDecodeError(t *testing.T) {
	data := []byte("123456789")
	file := newFakeFile(data, int64(len(data)), 123)
	deps, state := testPipelineDeps(file)
	openCalls := 0
	deps.open = func(string) (readStatCloser, error) {
		openCalls++
		return file, nil
	}
	cfg := testConfig()
	cfg.ImageMemBytes = 8
	job := &worker.JobMsg{
		JobID: 2, Path: `C:\media\large.jpg`, Kind: worker.MediaImage,
		FieldsMask: worker.MaskAllImage, Size: int64(len(data)), MTimeUnix: 123,
	}

	result, err := processImageWithDeps(cfg, job, deps)
	if err != nil {
		t.Fatal(err)
	}
	if openCalls != 1 {
		t.Fatalf("open calls = %d, want 1", openCalls)
	}
	if state.decodeCalls != 0 {
		t.Fatalf("decode calls = %d, want 0", state.decodeCalls)
	}
	if len(result.Errors) != 1 || result.Errors[0].Stage != "decode" {
		t.Fatalf("errors = %#v, want one structured decode error", result.Errors)
	}
}

func TestCacheHitQueriesSHAThenSkipsDecode(t *testing.T) {
	file := newFakeFile([]byte("pixels"), 6, 123)
	deps, state := testPipelineDeps(file)
	state.queryReply = &worker.SHAReplyMsg{
		JobID: 3, Found: true, PDQ: bytes.Repeat([]byte{7}, 32),
		Quality: 88, Width: 10, Height: 20,
	}
	job := &worker.JobMsg{
		JobID: 3, Path: `C:\media\hit.jpg`, Kind: worker.MediaImage,
		FieldsMask: worker.MaskAllImage, Size: 6, MTimeUnix: 123,
	}

	result, err := processImageWithDeps(testConfig(), job, deps)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(state.events, ","); got != "query" {
		t.Fatalf("event order = %q, want query only", got)
	}
	if result.Decoded {
		t.Fatal("cache-hit result says it decoded")
	}
	if result.Quality != 88 || result.Width != 10 || result.Height != 20 {
		t.Fatalf("cache-hit fields not copied: %#v", result)
	}
}

func TestOwnerQueriesBeforeDecodeAndReturnsSuccess(t *testing.T) {
	file := newFakeFile([]byte("pixels"), 6, 123)
	deps, state := testPipelineDeps(file)
	job := &worker.JobMsg{
		JobID: 4, Path: `C:\media\owner.jpg`, Kind: worker.MediaImage,
		FieldsMask: worker.MaskAllImage, Size: 6, MTimeUnix: 123,
	}

	result, err := processImageWithDeps(testConfig(), job, deps)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(state.events, ","); got != "query,decode" {
		t.Fatalf("event order = %q, want query,decode", got)
	}
	if !result.Decoded || result.FieldsDone != worker.MaskAllImage {
		t.Fatalf("owner result = %#v", result)
	}
	if result.ReadAttempts != 1 || result.DecodeAttempts != 1 ||
		result.ReadNS < 0 || result.DecodeNS < 0 {
		t.Fatalf("owner timing metrics = %#v", result)
	}
}

func TestPipelineReportsOpenReadDriftAndDecodeFailures(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*pipelineDeps, *fakeFile, *testDepsState)
		stage string
	}{
		{
			name: "open",
			setup: func(d *pipelineDeps, _ *fakeFile, _ *testDepsState) {
				d.open = func(string) (readStatCloser, error) { return nil, errors.New("denied") }
			},
			stage: "open",
		},
		{
			name:  "read",
			setup: func(_ *pipelineDeps, f *fakeFile, _ *testDepsState) { f.readErr = errors.New("disk failure") },
			stage: "read",
		},
		{
			name: "stat drift before read",
			setup: func(_ *pipelineDeps, _ *fakeFile, state *testDepsState) {
				state.pathStats = []fakeInfo{{size: 7, mtime: 124, identity: "path"}, {size: 7, mtime: 124, identity: "path"}}
			},
			stage: "stat",
		},
		{
			name: "stat drift after read",
			setup: func(_ *pipelineDeps, _ *fakeFile, state *testDepsState) {
				state.pathStats = []fakeInfo{{size: 6, mtime: 123, identity: "path"}, {size: 7, mtime: 124, identity: "path"}}
			},
			stage: "stat",
		},
		{
			name: "open handle drift after read",
			setup: func(_ *pipelineDeps, file *fakeFile, _ *testDepsState) {
				file.handleStats = []fakeInfo{{size: 6, mtime: 123, identity: "path"}, {size: 7, mtime: 124, identity: "path"}}
			},
			stage: "stat",
		},
		{
			name:  "decode",
			setup: func(_ *pipelineDeps, _ *fakeFile, state *testDepsState) { state.decodeErr = errors.New("bad pixels") },
			stage: "decode",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			file := newFakeFile([]byte("pixels"), 6, 123)
			deps, state := testPipelineDeps(file)
			tc.setup(&deps, file, state)
			job := &worker.JobMsg{
				JobID: 5, Path: `C:\media\bad.jpg`, Kind: worker.MediaImage,
				FieldsMask: worker.MaskAllImage, Size: 6, MTimeUnix: 123,
			}
			result, err := processImageWithDeps(testConfig(), job, deps)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Errors) != 1 || result.Errors[0].Stage != tc.stage {
				t.Fatalf("errors = %#v, want stage %q", result.Errors, tc.stage)
			}
			if result.ReadAttempts != 1 || result.ReadNS < 0 {
				t.Fatalf("failed read timing metrics = %#v", result)
			}
			wantDecodeAttempts := int64(0)
			if tc.stage == "decode" {
				wantDecodeAttempts = 1
			}
			if result.DecodeAttempts != wantDecodeAttempts ||
				result.DecodeNS < 0 {
				t.Fatalf("failed decode timing metrics = %#v", result)
			}
		})
	}
}

func TestPipelineRejectsOpenedHandleDifferentFromPathAtSameMetadata(t *testing.T) {
	file := newFakeFile([]byte("pixels"), 6, 123)
	file.handleStats = []fakeInfo{
		{size: 6, mtime: 123, identity: "old-handle"},
		{size: 6, mtime: 123, identity: "old-handle"},
	}
	deps, state := testPipelineDeps(file)
	state.pathStats = []fakeInfo{
		{size: 6, mtime: 123, identity: "replacement-path"},
		{size: 6, mtime: 123, identity: "replacement-path"},
	}
	job := &worker.JobMsg{
		JobID: 66, Path: `C:\media\replaced.jpg`, Kind: worker.MediaImage,
		FieldsMask: worker.MaskAllImage, Size: 6, MTimeUnix: 123,
	}

	result, err := processImageWithDeps(testConfig(), job, deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) != 1 || result.Errors[0].Stage != "stat" {
		t.Fatalf("errors = %#v, want opened-handle identity stat rejection", result.Errors)
	}
	if file.offset != 0 {
		t.Fatalf("read %d bytes from stale handle before rejecting identity", file.offset)
	}
}

func TestInvalidJobSizeAndRetentionCapacityNeverAllocate(t *testing.T) {
	file := newFakeFile(nil, 0, 123)
	deps, _ := testPipelineDeps(file)
	job := &worker.JobMsg{
		JobID: 67, Path: `C:\media\negative.jpg`, Kind: worker.MediaImage,
		FieldsMask: worker.MaskAllImage, Size: -1, MTimeUnix: 123,
	}
	result, err := processImageWithDeps(testConfig(), job, deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) != 1 || result.Errors[0].Stage != "size" {
		t.Fatalf("negative-size errors = %#v, want structured size error", result.Errors)
	}

	if capacity, retain := retentionCapacity(math.MaxInt64, math.MaxInt64, 4<<20); retain || capacity != 0 {
		t.Fatalf("MaxInt64 retention = (%d,%v), want safe rejection", capacity, retain)
	}
	if capacity, retain := retentionCapacity(256<<20, 256<<20, 4<<20); !retain || capacity != 4<<20 {
		t.Fatalf("256MiB initial retention = (%d,%v), want bounded 4MiB initial allocation", capacity, retain)
	}
}

func TestParentIPCErrorIsFatalAndDecodeDoesNotRun(t *testing.T) {
	file := newFakeFile([]byte("pixels"), 6, 123)
	deps, state := testPipelineDeps(file)
	state.queryErr = errors.New("parent gone")
	job := &worker.JobMsg{
		JobID: 6, Path: `C:\media\ipc.jpg`, Kind: worker.MediaImage,
		FieldsMask: worker.MaskAllImage, Size: 6, MTimeUnix: 123,
	}

	_, err := processImageWithDeps(testConfig(), job, deps)
	if err == nil || !strings.Contains(err.Error(), "parent gone") {
		t.Fatalf("error = %v, want parent gone", err)
	}
	if state.decodeCalls != 0 {
		t.Fatalf("decode calls = %d, want 0", state.decodeCalls)
	}
}

func testConfig() Config {
	return Config{ReadChunkBytes: 4 << 20, ImageMemBytes: 256 << 20}
}

type fakeSHA struct {
	data []byte
}

func (h *fakeSHA) Update(p []byte) error {
	h.data = append(h.data, p...)
	return nil
}
func (h *fakeSHA) Final() ([64]byte, error) {
	var out [64]byte
	copy(out[:], h.data)
	return out, nil
}
func (h *fakeSHA) Close() error { return nil }

type fakeDecoder struct {
	events *[]string
	calls  *int
	err    *error
}

func (d fakeDecoder) decode([]byte) (imagePhase1, error) {
	*d.calls++
	*d.events = append(*d.events, "decode")
	if *d.err != nil {
		return imagePhase1{}, *d.err
	}
	return imagePhase1{
		Hash: bytes.Repeat([]byte{9}, 32), Quality: 77, Width: 31, Height: 17,
	}, nil
}

type fakeInfo struct {
	size     int64
	mtime    int64
	identity string
}

func (f fakeInfo) Name() string       { return "fake.jpg" }
func (f fakeInfo) Size() int64        { return f.size }
func (f fakeInfo) Mode() os.FileMode  { return 0 }
func (f fakeInfo) ModTime() time.Time { return time.Unix(f.mtime, 0) }
func (f fakeInfo) IsDir() bool        { return false }
func (f fakeInfo) Sys() any           { return nil }

type fakeFile struct {
	data        []byte
	offset      int
	requested   []int
	readErr     error
	handleStats []fakeInfo
	statCall    int
}

func newFakeFile(data []byte, size, mtime int64) *fakeFile {
	return &fakeFile{
		data: data,
		handleStats: []fakeInfo{
			{size: size, mtime: mtime, identity: "path"},
			{size: size, mtime: mtime, identity: "path"},
		},
	}
}

func (f *fakeFile) Read(p []byte) (int, error) {
	f.requested = append(f.requested, len(p))
	if f.offset >= len(f.data) {
		if f.readErr != nil {
			return 0, f.readErr
		}
		return 0, io.EOF
	}
	n := copy(p, f.data[f.offset:])
	f.offset += n
	return n, nil
}
func (f *fakeFile) Stat() (os.FileInfo, error) {
	index := f.statCall
	if index >= len(f.handleStats) {
		index = len(f.handleStats) - 1
	}
	f.statCall++
	return f.handleStats[index], nil
}
func (f *fakeFile) Close() error { return nil }

type testDepsState struct {
	events       []string
	queryReply   *worker.SHAReplyMsg
	queryErr     error
	decodeErr    error
	decodeCalls  int
	pathStats    []fakeInfo
	pathStatCall int
}

func testPipelineDeps(file *fakeFile) (pipelineDeps, *testDepsState) {
	state := &testDepsState{}
	state.pathStats = []fakeInfo{
		{size: file.handleStats[0].size, mtime: file.handleStats[0].mtime, identity: "path"},
		{size: file.handleStats[0].size, mtime: file.handleStats[0].mtime, identity: "path"},
	}
	d := pipelineDeps{}
	d.open = func(string) (readStatCloser, error) { return file, nil }
	d.stat = func(string) (os.FileInfo, error) {
		index := state.pathStatCall
		if index >= len(state.pathStats) {
			index = len(state.pathStats) - 1
		}
		state.pathStatCall++
		return state.pathStats[index], nil
	}
	d.sameFile = func(left, right os.FileInfo) bool {
		return left.(fakeInfo).identity == right.(fakeInfo).identity
	}
	d.newSHA = func() (sha512Stream, error) { return &fakeSHA{}, nil }
	d.query = func(query *worker.SHAQueryMsg) (*worker.SHAReplyMsg, error) {
		state.events = append(state.events, "query")
		if state.queryErr != nil {
			return nil, state.queryErr
		}
		if state.queryReply != nil {
			return state.queryReply, nil
		}
		return &worker.SHAReplyMsg{JobID: query.JobID, Found: false}, nil
	}
	decoder := fakeDecoder{events: &state.events, calls: &state.decodeCalls, err: &state.decodeErr}
	d.decode = decoder.decode
	return d, state
}
