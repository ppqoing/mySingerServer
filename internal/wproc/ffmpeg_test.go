package wproc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"dedup/internal/wproc/mediacore"
)

func TestFFprobeParsesRoundedPositiveFiniteMilliseconds(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want int64
	}{
		{name: "five seconds", out: "5.000000\n", want: 5000},
		{name: "round down", out: "1.2344", want: 1234},
		{name: "round up", out: "1.2345", want: 1235},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeCommandRunner{stdout: []byte(tc.out)}
			got, err := ffprobeDuration(context.Background(), testVideoConfig(t.TempDir()), `C:\视频 文件\five seconds.mp4`, runner)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("duration = %d, want %d", got, tc.want)
			}
			wantArgs := []string{
				"-v", "error", "-show_entries", "format=duration",
				"-of", "default=noprint_wrappers=1:nokey=1",
				`C:\视频 文件\five seconds.mp4`,
			}
			if !reflect.DeepEqual(runner.args, wantArgs) {
				t.Fatalf("ffprobe args = %#v, want %#v", runner.args, wantArgs)
			}
		})
	}
}

func TestFFprobeRejectsMalformedNonFiniteAndNonPositiveOutput(t *testing.T) {
	for _, output := range []string{"oops", "NaN", "+Inf", "-Inf", "-1", "0"} {
		t.Run(strings.ReplaceAll(output, "+", "plus"), func(t *testing.T) {
			_, err := ffprobeDuration(context.Background(), testVideoConfig(t.TempDir()), "video.mp4", &fakeCommandRunner{stdout: []byte(output)})
			if err == nil || !strings.Contains(err.Error(), "duration") {
				t.Fatalf("output %q error = %v, want duration validation", output, err)
			}
		})
	}
}

func TestFFprobeReportsStderrAndCancellationReapsRunner(t *testing.T) {
	cfg := testVideoConfig(t.TempDir())
	runner := &fakeCommandRunner{stderr: []byte("demux failed"), err: errors.New("exit status 9")}
	_, err := ffprobeDuration(context.Background(), cfg, "bad.mp4", runner)
	if err == nil || !strings.Contains(err.Error(), "demux failed") || !strings.Contains(err.Error(), "exit status 9") {
		t.Fatalf("nonzero exit error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	blocked := &fakeCommandRunner{waitForContext: true}
	_, err = ffprobeDuration(ctx, cfg, "blocked.mp4", blocked)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v, want context.Canceled", err)
	}
	if !blocked.reaped {
		t.Fatal("cancelled ffprobe runner was not reaped")
	}

	timeoutCfg := testVideoConfig(t.TempDir())
	timeoutCfg.FFprobeTimeout = time.Nanosecond
	timedOut := &fakeCommandRunner{waitForContext: true}
	_, err = ffprobeDuration(context.Background(), timeoutCfg, "timeout.mp4", timedOut)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v, want context.DeadlineExceeded", err)
	}
	if !timedOut.reaped {
		t.Fatal("timed-out ffprobe runner was not reaped")
	}
}

func TestFFmpegArgumentsPreserveUnicodeSpacesAndBoundLongestSide(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "cache", "thumb.jpg")
	runner := &fakeCommandRunner{writeOutput: []byte{0xff, 0xd8, 0xff, 0xd9}}
	cfg := testVideoConfig(filepath.Join(root, "cache"))
	src := filepath.Join(root, strings.Repeat("很长目录 ", 25), "视频 😀.mp4")
	if err := ffmpegShot(context.Background(), cfg, src, 2.5, dst, runner); err != nil {
		t.Fatal(err)
	}
	if runner.name != cfg.FFmpegPath {
		t.Fatalf("command = %q, want %q", runner.name, cfg.FFmpegPath)
	}
	if !containsArgPair(runner.args, "-i", src) {
		t.Fatalf("ffmpeg args lost source boundary: %#v", runner.args)
	}
	if !containsArgPair(runner.args, "-ss", "2.500") {
		t.Fatalf("ffmpeg args lost seek: %#v", runner.args)
	}
	if !containsArgPair(runner.args, "-vf", "scale=256:256:force_original_aspect_ratio=decrease,format=gray") {
		t.Fatalf("ffmpeg filter does not bound longest side: %#v", runner.args)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 4 {
		t.Fatalf("committed JPEG length = %d, want 4", len(data))
	}
}

func TestFFmpegFailureAndCancelPreserveOldFinalAndRemoveTemps(t *testing.T) {
	for _, tc := range []struct {
		name    string
		runner  *fakeCommandRunner
		cancel  bool
		timeout bool
	}{
		{name: "nonzero", runner: &fakeCommandRunner{writeOutput: []byte("partial"), stderr: []byte("encoder failed"), err: errors.New("exit status 2")}},
		{name: "cancel", runner: &fakeCommandRunner{writeOutput: []byte("partial"), waitForContext: true}, cancel: true},
		{name: "timeout", runner: &fakeCommandRunner{writeOutput: []byte("partial"), waitForContext: true}, timeout: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dst := filepath.Join(root, "thumb.jpg")
			if err := os.WriteFile(dst, []byte("old-valid"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst+".json", []byte("old-sidecar"), 0o644); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if tc.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			cfg := testVideoConfig(root)
			if tc.timeout {
				cfg.FFmpegTimeout = time.Nanosecond
			}
			err := ffmpegShot(ctx, cfg, "source.mp4", 0, dst, tc.runner)
			if err == nil {
				t.Fatal("ffmpegShot succeeded, want failure")
			}
			got, readErr := os.ReadFile(dst)
			if readErr != nil || string(got) != "old-valid" {
				t.Fatalf("old final changed: data=%q err=%v", got, readErr)
			}
			sidecar, readErr := os.ReadFile(dst + ".json")
			if readErr != nil || string(sidecar) != "old-sidecar" {
				t.Fatalf("old sidecar changed: data=%q err=%v", sidecar, readErr)
			}
			matches, globErr := filepath.Glob(dst + ".tmp-*.jpg")
			if globErr != nil {
				t.Fatal(globErr)
			}
			if len(matches) != 0 {
				t.Fatalf("temporary files remain: %v", matches)
			}
			if (tc.cancel || tc.timeout) && !tc.runner.reaped {
				t.Fatal("cancelled/timed-out ffmpeg runner was not reaped")
			}
		})
	}
}

func TestFFmpegPrepareRemoveFailureStillCleansCreatedTemp(t *testing.T) {
	root := t.TempDir()
	var createdPath string
	removeCalls := 0
	ops := ffmpegFileOps{
		createTemp: func(dir, pattern string) (*os.File, error) {
			file, err := os.CreateTemp(dir, pattern)
			if err == nil {
				createdPath = file.Name()
			}
			return file, err
		},
		remove: func(path string) error {
			removeCalls++
			if removeCalls == 1 {
				return errors.New("injected prepare remove failure")
			}
			return os.Remove(path)
		},
	}
	_, err := ffmpegShotWithFileOps(
		context.Background(), testVideoConfig(root), "source.mp4", 0,
		filepath.Join(root, "thumb.jpg"), &fakeCommandRunner{}, ops,
	)
	if err == nil || !strings.Contains(err.Error(), "prepare thumbnail temp") {
		t.Fatalf("error = %v, want prepare thumbnail temp failure", err)
	}
	if removeCalls != 2 {
		t.Fatalf("remove calls = %d, want prepare attempt plus deferred cleanup", removeCalls)
	}
	if _, statErr := os.Stat(createdPath); !os.IsNotExist(statErr) {
		t.Fatalf("created temp leaked after prepare-remove failure: %q stat=%v", createdPath, statErr)
	}
}

func TestResolveFFmpegToolsRelativeToExecutable(t *testing.T) {
	exe := filepath.Join(`C:\Program Files`, "Dedup", "worker.exe")
	cfg := testVideoConfig(t.TempDir())
	probe, ffmpeg, err := resolveFFmpegTools(cfg, exe)
	if err != nil {
		t.Fatal(err)
	}
	if probe != filepath.Join(filepath.Dir(exe), `tools\ffprobe.exe`) {
		t.Fatalf("probe = %q", probe)
	}
	if ffmpeg != filepath.Join(filepath.Dir(exe), `tools\ffmpeg.exe`) {
		t.Fatalf("ffmpeg = %q", ffmpeg)
	}
	cfg.FFprobePath = `D:\portable\ffprobe.exe`
	cfg.FFmpegPath = `D:\portable\ffmpeg.exe`
	probe, ffmpeg, err = resolveFFmpegTools(cfg, exe)
	if err != nil || probe != cfg.FFprobePath || ffmpeg != cfg.FFmpegPath {
		t.Fatalf("absolute tools = (%q,%q,%v)", probe, ffmpeg, err)
	}
}

func TestFFprobeFFmpegBlackBoxFiveSecondVideoAndPDQ(t *testing.T) {
	if mediacore.Version() == "" {
		t.Skip("mediacore cgo binding unavailable")
	}
	ffmpeg, ffprobe := repositoryFFmpegTools(t)
	root := t.TempDir()
	video := filepath.Join(root, "真实 five seconds.mp4")
	runner := execCommandRunner{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, stderr, err := runner.Run(ctx, ffmpeg, []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi",
		"-i", "testsrc=duration=5:size=640x360:rate=25",
		"-pix_fmt", "yuv420p",
		"-y", video,
	})
	if err != nil {
		t.Fatalf("generate real video: %v: %s", err, stderr)
	}
	cfg := testVideoConfig(filepath.Join(root, "cache"))
	cfg.FFmpegPath = ffmpeg
	cfg.FFprobePath = ffprobe
	duration, err := ffprobeDuration(context.Background(), cfg, video, runner)
	if err != nil {
		t.Fatal(err)
	}
	if duration < 4900 || duration > 5100 {
		t.Fatalf("real duration = %dms, want [4900,5100]", duration)
	}
	thumb := filepath.Join(root, "真实 thumbnail.jpg")
	if err := ffmpegShot(context.Background(), cfg, video, float64(duration)/2000, thumb, runner); err != nil {
		t.Fatal(err)
	}
	jpeg, err := os.ReadFile(thumb)
	if err != nil {
		t.Fatal(err)
	}
	if len(jpeg) < 4 || jpeg[0] != 0xff || jpeg[1] != 0xd8 {
		t.Fatalf("real thumbnail is not a non-empty JPEG: %d bytes", len(jpeg))
	}
	result, err := mediacore.ImagePhase1(jpeg)
	if err != nil {
		t.Fatalf("real thumbnail PDQ: %v", err)
	}
	if result.Width <= 0 || result.Height <= 0 || result.Width > 256 || result.Height > 256 {
		t.Fatalf("real thumbnail dimensions = %dx%d", result.Width, result.Height)
	}
}

func repositoryFFmpegTools(t *testing.T) (ffmpeg, ffprobe string) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	ffmpeg = filepath.Join(root, "third_party", "ffmpeg", "bin", "ffmpeg.exe")
	ffprobe = filepath.Join(root, "third_party", "ffmpeg", "bin", "ffprobe.exe")
	for _, tool := range []string{ffmpeg, ffprobe} {
		if _, err := os.Stat(tool); err != nil {
			t.Skipf("repository FFmpeg tool missing: %s", tool)
		}
	}
	return ffmpeg, ffprobe
}

type fakeCommandRunner struct {
	name           string
	args           []string
	stdout         []byte
	stderr         []byte
	err            error
	waitForContext bool
	reaped         bool
	writeOutput    []byte
}

func (r *fakeCommandRunner) Run(ctx context.Context, name string, args []string) ([]byte, []byte, error) {
	r.name = name
	r.args = append([]string(nil), args...)
	if r.waitForContext {
		<-ctx.Done()
		r.reaped = true
		return nil, nil, ctx.Err()
	}
	if len(r.writeOutput) != 0 {
		output := args[len(args)-1]
		if err := os.WriteFile(output, r.writeOutput, 0o644); err != nil {
			return nil, nil, err
		}
	}
	return append([]byte(nil), r.stdout...), append([]byte(nil), r.stderr...), r.err
}

func containsArgPair(args []string, key, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == key && args[i+1] == value {
			return true
		}
	}
	return false
}
