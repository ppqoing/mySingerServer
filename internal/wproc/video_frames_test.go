package wproc

import (
	"context"
	"crypto/sha512"
	"errors"
	"hash"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"dedup/internal/worker"
)

func TestPhase2FrameTimesAreExactAndOverflowSafe(t *testing.T) {
	if got, want := phase2FrameTimes(12000), [6]int64{1000, 3000, 5000, 7000, 9000, 11000}; got != want {
		t.Fatalf("12-second frame times = %v, want %v", got, want)
	}
	wantLarge := [6]int64{
		768614336404564650,
		2305843009213693951,
		3843071682022823252,
		5380300354831952554,
		6917529027641081855,
		8454757700450211156,
	}
	if got := phase2FrameTimes(int64(^uint64(0) >> 1)); got != wantLarge {
		t.Fatalf("max-int64 frame times = %v, want %v", got, wantLarge)
	}
}

func TestPhase2VideoRunsSixExactOutputSeekCommandsAndEncodesFrames(t *testing.T) {
	job, deps, state := newPhase2VideoHarness()
	job.Path = `C:\media folder\` + strings.Repeat(`long segment\`, 20) + `video.mp4`
	result, err := processPhase2WithDeps(context.Background(), phase2TestConfig(), job, deps)
	if err != nil {
		t.Fatal(err)
	}
	if result.FieldsDone != worker.MaskVideo6F || len(result.Errors) != 0 || len(result.Frames) != 6 {
		t.Fatalf("video result = %#v", result)
	}
	if result.Path != job.Path {
		t.Fatalf("protocol path = %q, want original %q", result.Path, job.Path)
	}
	wantInputPath := fixPath(job.Path)
	if wantInputPath == job.Path {
		t.Fatalf("test path length %d did not exercise long-path normalization", len(job.Path))
	}
	wantTimes := [6]int64{1000, 3000, 5000, 7000, 9000, 11000}
	for i, frame := range result.Frames {
		if frame.FrameIdx != i || frame.TimeMS != wantTimes[i] || frame.Error != "" {
			t.Fatalf("frame %d identity = %#v", i, frame)
		}
		if len(frame.PDQ256) != 32 || len(frame.PHashParts) != 76 || len(frame.SobelHist) != 516 {
			t.Fatalf("frame %d BLOB lengths = %d/%d/%d", i,
				len(frame.PDQ256), len(frame.PHashParts), len(frame.SobelHist))
		}
		gray := state.grays[i]
		if gray.pdqCalls != 1 || gray.phase2Calls != 1 || gray.freeCalls != 1 {
			t.Fatalf("frame %d native calls = PDQ %d Phase2 %d Free %d",
				i, gray.pdqCalls, gray.phase2Calls, gray.freeCalls)
		}
		call := state.calls[i]
		if call.path != `tools\ffmpeg.exe` {
			t.Fatalf("frame %d executable = %q", i, call.path)
		}
		inputAt := stringArgIndex(call.args, "-i")
		seekAt := stringArgIndex(call.args, "-ss")
		if inputAt < 0 || seekAt <= inputAt+1 || call.args[inputAt+1] != wantInputPath {
			t.Fatalf("frame %d args do not use one exact input before output seek: %#v", i, call.args)
		}
		if strings.Contains(call.args[inputAt+1], `"`) {
			t.Fatalf("frame %d input argument contains shell quoting: %q", i, call.args[inputAt+1])
		}
		if call.args[seekAt+1] != formatFrameTimeMS(wantTimes[i]) {
			t.Fatalf("frame %d seek = %q", i, call.args[seekAt+1])
		}
		filterAt := stringArgIndex(call.args, "-vf")
		if filterAt < 0 || call.args[filterAt+1] !=
			"scale=512:512:force_original_aspect_ratio=decrease,format=gray" {
			t.Fatalf("frame %d filter args = %#v", i, call.args)
		}
		if call.timeout < 19*time.Second || call.timeout > 20*time.Second {
			t.Fatalf("frame %d timeout = %s, want per-command 20s", i, call.timeout)
		}
	}
	if state.decodeCalls != 6 || state.openCalls != 0 {
		t.Fatalf("video work = decode %d open/hash %d/%d",
			state.decodeCalls, state.openCalls, state.hashCalls)
	}
}

func TestPhase2VideoArbitraryMaskRetainsSuccessesAndContinuesAfterFailure(t *testing.T) {
	job, deps, state := newPhase2VideoHarness()
	job.FrameMask = 1<<0 | 1<<2 | 1<<5
	state.runErrorAt[1] = errors.New("ffmpeg failed")
	state.stderrAt[1] = []byte("decoder details")

	result, err := processPhase2WithDeps(context.Background(), phase2TestConfig(), job, deps)
	if err != nil {
		t.Fatal(err)
	}
	if result.FieldsDone != 0 || len(result.Frames) != 3 || len(state.calls) != 3 {
		t.Fatalf("partial result = %#v, calls %d", result, len(state.calls))
	}
	if got := []int{result.Frames[0].FrameIdx, result.Frames[1].FrameIdx, result.Frames[2].FrameIdx}; !reflect.DeepEqual(got, []int{0, 2, 5}) {
		t.Fatalf("frame indices = %v", got)
	}
	failed := result.Frames[1]
	if failed.Error == "" || !strings.Contains(failed.Error, "decoder details") ||
		len(failed.PDQ256) != 0 || len(failed.PHashParts) != 0 || len(failed.SobelHist) != 0 {
		t.Fatalf("failed frame = %#v", failed)
	}
	if result.Frames[0].Error != "" || result.Frames[2].Error != "" {
		t.Fatalf("valid partial frames were discarded: %#v", result.Frames)
	}
}

func TestPhase2VideoRejectsEmptyOversizedCanceledAndTimedOutCommands(t *testing.T) {
	tests := []struct {
		name   string
		config func(*Config)
		setup  func(context.Context, *phase2VideoState) context.Context
		stage  string
	}{
		{
			name: "empty stdout",
			setup: func(ctx context.Context, state *phase2VideoState) context.Context {
				state.stdoutAt[0] = nil
				return ctx
			},
			stage: "empty",
		},
		{
			name:   "oversized stdout",
			config: func(cfg *Config) { cfg.IPCMaxFrameBytes = 4 },
			setup: func(ctx context.Context, state *phase2VideoState) context.Context {
				state.stdoutAt[0] = []byte("12345")
				return ctx
			},
			stage: "large",
		},
		{
			name: "canceled",
			setup: func(ctx context.Context, _ *phase2VideoState) context.Context {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				return canceled
			},
			stage: "canceled",
		},
		{
			name:   "timed out",
			config: func(cfg *Config) { cfg.Phase2FrameTimeout = time.Millisecond },
			setup: func(ctx context.Context, state *phase2VideoState) context.Context {
				state.waitForContextAt[0] = true
				return ctx
			},
			stage: "deadline",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			job, deps, state := newPhase2VideoHarness()
			job.FrameMask = 1
			cfg := phase2TestConfig()
			if tc.config != nil {
				tc.config(&cfg)
			}
			ctx := tc.setup(context.Background(), state)
			result, err := processPhase2WithDeps(ctx, cfg, job, deps)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Frames) != 1 || result.Frames[0].Error == "" ||
				!strings.Contains(strings.ToLower(result.Frames[0].Error), tc.stage) ||
				state.decodeCalls != 0 {
				t.Fatalf("command failure result = %#v, decodes %d", result, state.decodeCalls)
			}
		})
	}
}

func TestPhase2VideoDoesNotPublishFrameWhenRunnerIgnoresCanceledParent(t *testing.T) {
	job, deps, state := newPhase2VideoHarness()
	job.FrameMask = 1
	deps.runCommand = func(
		_ context.Context,
		_ string,
		_ []string,
		stdout io.Writer,
		_ io.Writer,
	) error {
		_, _ = stdout.Write([]byte{1})
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := processPhase2WithDeps(ctx, phase2TestConfig(), job, deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Frames) != 1 || result.Frames[0].Error == "" ||
		!strings.Contains(strings.ToLower(result.Frames[0].Error), "canceled") ||
		state.decodeCalls != 0 {
		t.Fatalf("ignored cancellation published frame: %#v, decodes %d",
			result, state.decodeCalls)
	}
}

func TestPhase2VideoMetadataMismatchUsesKnownSHAAndContentMismatchIsStale(t *testing.T) {
	t.Run("same SHA continues", func(t *testing.T) {
		job, deps, state := newPhase2VideoHarness()
		job.Size++
		job.MTimeMS--
		result, err := processPhase2WithDeps(context.Background(), phase2TestConfig(), job, deps)
		if err != nil {
			t.Fatal(err)
		}
		if state.openCalls != 1 || state.hashCalls != 1 || len(result.Frames) != 6 ||
			len(result.Errors) != 0 {
			t.Fatalf("same-SHA mismatch = open/hash %d/%d result %#v",
				state.openCalls, state.hashCalls, result)
		}
	})

	t.Run("different SHA is stale before ffmpeg", func(t *testing.T) {
		job, deps, state := newPhase2VideoHarness()
		old := sha512.Sum512([]byte("old video"))
		job.KnownSHA = append([]byte(nil), old[:]...)
		job.MTimeMS--
		result, err := processPhase2WithDeps(context.Background(), phase2TestConfig(), job, deps)
		if err != nil {
			t.Fatal(err)
		}
		assertPhase2Stale(t, result, job.KnownSHA)
		if state.openCalls != 1 || state.hashCalls != 1 || len(state.calls) != 0 {
			t.Fatalf("stale video work = open/hash %d/%d commands %d",
				state.openCalls, state.hashCalls, len(state.calls))
		}
	})
}

func TestPhase2VideoFinalSourceDriftDiscardsAllFrames(t *testing.T) {
	job, deps, state := newPhase2VideoHarness()
	state.pathStats[1] = phase2Info{
		size: state.pathStats[0].size + 1, mtimeMS: state.pathStats[0].mtimeMS + 1,
		identity: "replacement-video",
	}
	result, err := processPhase2WithDeps(context.Background(), phase2TestConfig(), job, deps)
	if err != nil {
		t.Fatal(err)
	}
	assertPhase2Stale(t, result, job.KnownSHA)
	if len(state.calls) != 6 {
		t.Fatalf("final drift commands = %d, want all six attempted before final check", len(state.calls))
	}
}

func TestPhase2VideoRejectsInvalidDurationAndFrameMaskBeforeIO(t *testing.T) {
	for _, mutate := range []func(*worker.JobMsg){
		func(job *worker.JobMsg) { job.DurationMS = 0 },
		func(job *worker.JobMsg) { job.FrameMask = 0x80 },
	} {
		job, deps, state := newPhase2VideoHarness()
		mutate(job)
		result, err := processPhase2WithDeps(context.Background(), phase2TestConfig(), job, deps)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Errors) != 1 || result.Errors[0].Field != 0 ||
			result.Errors[0].Stage != "validate" || state.pathStatCall != 0 {
			t.Fatalf("invalid video result/work = %#v / stats %d", result, state.pathStatCall)
		}
	}
}

type phase2CommandCall struct {
	path    string
	args    []string
	timeout time.Duration
}

type phase2VideoState struct {
	data             []byte
	pathStats        []phase2Info
	pathStatCall     int
	openCalls        int
	hashCalls        int
	decodeCalls      int
	calls            []phase2CommandCall
	grays            []*fakePhase2Gray
	runErrorAt       map[int]error
	stdoutAt         map[int][]byte
	stderrAt         map[int][]byte
	waitForContextAt map[int]bool
}

func newPhase2VideoHarness() (*worker.JobMsg, phase2PipelineDeps, *phase2VideoState) {
	data := []byte("phase2 video bytes")
	info := phase2Info{size: int64(len(data)), mtimeMS: 321000, identity: "video"}
	state := &phase2VideoState{
		data:             append([]byte(nil), data...),
		pathStats:        []phase2Info{info, info},
		runErrorAt:       make(map[int]error),
		stdoutAt:         make(map[int][]byte),
		stderrAt:         make(map[int][]byte),
		waitForContextAt: make(map[int]bool),
	}
	for i := 0; i < 6; i++ {
		state.stdoutAt[i] = []byte{byte(i + 1)}
		gray := &fakePhase2Gray{}
		for j := range gray.output.PHashParts {
			gray.output.PHashParts[j] = uint64(i*10 + j)
		}
		gray.output.SobelHist[i] = float32(i + 1)
		state.grays = append(state.grays, gray)
	}
	deps := phase2PipelineDeps{
		open: func(string) (readStatCloser, error) {
			state.openCalls++
			fileInfo := state.pathStats[0]
			return &phase2FakeFile{
				data:        append([]byte(nil), state.data...),
				handleStats: []phase2Info{fileInfo},
			}, nil
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
			if len(input) != 1 {
				return nil, errors.New("unexpected PNG fixture")
			}
			index := int(input[0]) - 1
			if index < 0 || index >= len(state.grays) {
				return nil, errors.New("unexpected PNG frame")
			}
			state.decodeCalls++
			return state.grays[index], nil
		},
		runCommand: func(ctx context.Context, path string, args []string, stdout, stderr io.Writer) error {
			index := len(state.calls)
			deadline, ok := ctx.Deadline()
			var timeout time.Duration
			if ok {
				timeout = time.Until(deadline)
			}
			state.calls = append(state.calls, phase2CommandCall{
				path: path, args: append([]string(nil), args...), timeout: timeout,
			})
			if state.waitForContextAt[index] {
				<-ctx.Done()
				return ctx.Err()
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if output := state.stdoutAt[index]; output != nil {
				if _, err := stdout.Write(output); err != nil {
					return err
				}
			}
			if output := state.stderrAt[index]; output != nil {
				_, _ = stderr.Write(output)
			}
			return state.runErrorAt[index]
		},
	}
	sum := sha512.Sum512(data)
	job := &worker.JobMsg{
		JobID: 601, Path: `C:\media folder\quoted video.mp4`,
		Kind: worker.MediaVideo, Phase: worker.Phase2, FieldsMask: worker.MaskVideo6F,
		Size: info.size, MTimeMS: info.mtimeMS, KnownSHA: append([]byte(nil), sum[:]...),
		DurationMS: 12000,
	}
	return job, deps, state
}

func stringArgIndex(args []string, value string) int {
	for i, arg := range args {
		if arg == value {
			return i
		}
	}
	return -1
}
