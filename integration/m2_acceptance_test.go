//go:build windows && m2acceptance

package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"dedup/internal/proto"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sys/windows"
	_ "modernc.org/sqlite"
)

type m2Run struct {
	root       string
	dataDir    string
	sourceDir  string
	configPath string
	machineID  string
	address    string
	process    *m2Process
	conn       *proto.Conn
	hello      proto.Hello
	pgSnapshot *m2PGFeatureSnapshot
}

type m2ScanResult struct {
	Ack      proto.TaskAck
	Features []proto.FeatureItem
	Errors   []proto.Error
	Crashes  []proto.CrashNotice
	Done     proto.TaskDone
}

func TestM2AC1CorruptInputs(t *testing.T) {
	run := newM2Run(t, m2RunOptions{workerCount: 4})
	corpus := requiredM2Corpus(t)
	valid := []string{
		"base/valid.jpg",
		"base/valid.png",
		"base/wrongext.png",
		"base/valid5s.mp4",
		"base/valid8s.mp4",
		"base/copy_of_valid5s.mp4",
		"base/trunc50.mp4",
	}
	for _, relative := range valid {
		copyM2File(t, filepath.Join(corpus, filepath.FromSlash(relative)),
			filepath.Join(run.sourceDir, filepath.Base(relative)))
	}
	corrupt, err := filepath.Glob(filepath.Join(corpus, "corrupt", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(corrupt) != 8 {
		t.Fatalf("AC-1 corrupt classes = %d, want 8", len(corrupt))
	}
	for class, source := range corrupt {
		extension := filepath.Ext(source)
		for copyIndex := 0; copyIndex < 24; copyIndex++ {
			name := fmt.Sprintf("corrupt_%02d_%02d%s", class, copyIndex, extension)
			copyM2File(t, source, filepath.Join(run.sourceDir, name))
		}
	}

	run.start(t)
	agentPID := run.hello.PID
	first := run.scan(t, "m2-ac1-first-"+run.machineID, run.sourceDir, 90*time.Second)
	if run.process.pid() != agentPID || !run.process.running() {
		t.Fatalf("AC-1 Agent changed/exited pid=%d", agentPID)
	}
	if first.Done.Stats.Total != 199 || len(first.Features) != 199 {
		t.Fatalf("AC-1 total/features = %d/%d, want 199/199", first.Done.Stats.Total, len(first.Features))
	}
	if len(readM2JSONLinesOptional(t, filepath.Join(run.dataDir, "crash.log"))) != 0 {
		t.Fatal("AC-1 unexpected worker crash")
	}
	errorLines := readM2JSONLines(t, filepath.Join(run.dataDir, "errors.log"))
	fieldFailures := m2FieldFailureCount(first)
	if len(errorLines) == 0 || len(errorLines) != fieldFailures {
		t.Fatalf("AC-1 error lines=%d field failures=%d", len(errorLines), fieldFailures)
	}
	for _, line := range errorLines {
		for _, key := range []string{"path", "stage", "err"} {
			value, ok := line[key].(string)
			if !ok || value == "" {
				t.Fatalf("AC-1 error line missing %s: %#v", key, line)
			}
		}
		workerPID, ok := line["worker_pid"].(float64)
		if !ok || workerPID == 0 {
			t.Fatalf("AC-1 error line missing worker_pid: %#v", line)
		}
	}
	items := m2FeatureItemsByBase(t, first.Features)
	for _, name := range []string{"valid.jpg", "valid.png", "wrongext.png"} {
		assertM2CompleteImage(t, items[name], name)
	}
	for _, name := range []string{"valid5s.mp4", "valid8s.mp4", "copy_of_valid5s.mp4"} {
		assertM2CompleteVideo(t, items[name], name)
	}
	truncated := items["trunc50.mp4"]
	if truncated.Status != proto.StatusPartial && truncated.Status != proto.StatusFailed {
		t.Fatalf("AC-1 trunc50 status=%q, want partial/failed", truncated.Status)
	}
	truncatedStages := make(map[string]bool)
	for _, fieldError := range truncated.FieldErrors {
		truncatedStages[fieldError.Stage] = true
	}
	if !truncatedStages["ffprobe"] || !truncatedStages["ffmpeg"] {
		t.Fatalf("AC-1 trunc50 errors=%#v, want ffprobe and ffmpeg", truncated.FieldErrors)
	}
	counts := m2FileStatusMap(t, run.dataDir)
	if counts["done"] != 6 || counts["partial"]+counts["failed"] != 193 {
		t.Fatalf("AC-1 status counts = %#v, want done=6 partial+failed=193", counts)
	}
	images, videos := m2FeatureCounts(t, run.dataDir)
	if images != 2 || videos != 2 {
		t.Fatalf("AC-1 feature rows image=%d video=%d, want 2/2", images, videos)
	}
	fileStateBefore := m2FileStateSnapshot(t, run.dataDir)
	featureStateBefore := m2AllFeatureSnapshot(t, run.dataDir)
	for path, state := range fileStateBefore {
		name := filepath.Base(path)
		if strings.HasPrefix(name, "corrupt_") || name == "trunc50.mp4" {
			if state.MissingMask == 0 {
				t.Fatalf("AC-1 retry target %s missing_mask=0", path)
			}
		} else if state.MissingMask != 0 || state.Status != proto.StatusDone {
			t.Fatalf("AC-1 valid state %s=%#v", path, state)
		}
	}
	rowsBefore := m2FileRowCount(t, run.dataDir)
	second := run.scan(t, "m2-ac1-second-"+run.machineID, run.sourceDir, 90*time.Second)
	secondFailures := m2FieldFailureCount(second)
	if second.Done.Stats.Total != 199 ||
		second.Done.Stats.Skipped != 6 ||
		second.Done.Stats.FilesFailed != 193 ||
		secondFailures != fieldFailures {
		t.Fatalf("AC-1 rescan stats=%#v field failures=%d", second.Done.Stats, secondFailures)
	}
	if run.process.pid() != agentPID || !run.process.running() {
		t.Fatalf("AC-1 Agent changed/exited after rescan pid=%d", agentPID)
	}
	if m2FileRowCount(t, run.dataDir) != rowsBefore {
		t.Fatal("AC-1 rescan created duplicate file rows")
	}
	if got := m2FileStateSnapshot(t, run.dataDir); !equalM2FileStates(fileStateBefore, got) {
		t.Fatalf("AC-1 rescan file state changed\nbefore=%#v\nafter=%#v", fileStateBefore, got)
	}
	if got := m2AllFeatureSnapshot(t, run.dataDir); fmt.Sprint(got) != fmt.Sprint(featureStateBefore) {
		t.Fatalf("AC-1 rescan feature values changed\nbefore=%#v\nafter=%#v", featureStateBefore, got)
	}
	imagesAfter, videosAfter := m2FeatureCounts(t, run.dataDir)
	if imagesAfter != images || videosAfter != videos {
		t.Fatalf("AC-1 rescan duplicated features: image=%d video=%d", imagesAfter, videosAfter)
	}
	if len(readM2JSONLinesOptional(t, filepath.Join(run.dataDir, "crash.log"))) != 0 {
		t.Fatal("AC-1 rescan caused worker crash")
	}
	allErrors := readM2JSONLines(t, filepath.Join(run.dataDir, "errors.log"))
	if len(allErrors) != fieldFailures+secondFailures {
		t.Fatalf("AC-1 rescan errors.log=%d want=%d", len(allErrors), fieldFailures+secondFailures)
	}
	t.Logf(
		"AC-1 corpus_resolution=199(8_corrupt_classes*24+7_base_files) agent_pid=%d total=199 first_field_failures=%d statuses=%v sqlite_features=%d/%d rescan_field_failures=%d valid_paths=6 trunc50_stages=ffprobe,ffmpeg",
		agentPID, fieldFailures, counts, images, videos, secondFailures,
	)
}

func m2FeatureItemsByBase(t *testing.T, items []proto.FeatureItem) map[string]proto.FeatureItem {
	t.Helper()
	result := make(map[string]proto.FeatureItem, len(items))
	for _, item := range items {
		name := filepath.Base(item.Path)
		if _, exists := result[name]; exists {
			t.Fatalf("duplicate terminal FeatureItem basename %s", name)
		}
		result[name] = item
	}
	return result
}

func assertM2CompleteImage(t *testing.T, item proto.FeatureItem, name string) {
	t.Helper()
	if item.Status != proto.StatusDone ||
		item.FieldsDone&(proto.FieldSHA512|proto.FieldPDQ256) !=
			proto.FieldSHA512|proto.FieldPDQ256 ||
		len(item.SHA512) != 128 || len(item.PDQ256) != 64 ||
		item.Width <= 0 || item.Height <= 0 || item.Quality <= 0 ||
		len(item.FieldErrors) != 0 {
		t.Fatalf("AC-1 valid image %s incomplete: %#v", name, item)
	}
}

func assertM2CompleteVideo(t *testing.T, item proto.FeatureItem, name string) {
	t.Helper()
	if item.Status != proto.StatusDone ||
		item.FieldsDone&(proto.FieldSHA512|proto.FieldThumb) !=
			proto.FieldSHA512|proto.FieldThumb ||
		len(item.SHA512) != 128 || item.DurationMS == nil || *item.DurationMS <= 0 ||
		item.ThumbPath == "" || len(item.ThumbPDQ256) != 64 ||
		item.ThumbQuality == nil || *item.ThumbQuality <= 0 ||
		len(item.FieldErrors) != 0 {
		t.Fatalf("AC-1 valid video %s incomplete: %#v", name, item)
	}
}

func m2FieldFailureCount(result m2ScanResult) int {
	count := len(result.Errors)
	for _, item := range result.Features {
		count += len(item.FieldErrors)
	}
	return count
}

func TestM2AC2RealNativeAccessViolation(t *testing.T) {
	run := newM2Run(t, m2RunOptions{
		workerCount: 4, crashInjection: true, imageTimeout: 30,
	})
	seed := filepath.Join(requiredM2Corpus(t), "base", "valid.jpg")
	for index := 0; index < 90; index++ {
		copyM2File(t, seed, filepath.Join(run.sourceDir, fmt.Sprintf("normal_%03d.jpg", index)))
	}
	for index := 0; index < 10; index++ {
		copyM2File(t, seed, filepath.Join(run.sourceDir, fmt.Sprintf("img__crash__%02d.jpg", index)))
	}
	run.start(t)
	agentPID := run.hello.PID
	result := run.scan(t, "m2-ac2-"+run.machineID, run.sourceDir, 90*time.Second)
	if run.process.pid() != agentPID || !run.process.running() {
		t.Fatalf("agent PID changed/exited: hello=%d process=%d", agentPID, run.process.pid())
	}
	if result.Done.Stats.Total != 100 || result.Done.Stats.Crashes != 10 {
		t.Fatalf("AC-2 stats = %#v, want total=100 crashes=10", result.Done.Stats)
	}
	crashes := readM2JSONLines(t, filepath.Join(run.dataDir, "crash.log"))
	if len(crashes) != 10 {
		t.Fatalf("AC-2 crash lines = %d, want 10", len(crashes))
	}
	for _, line := range crashes {
		path, _ := line["file"].(string)
		if !strings.Contains(path, "__crash__") ||
			int64(line["pid"].(float64)) == 0 ||
			int64(line["exit_code"].(float64)) != -1073741819 ||
			line["reason"] != "exit_code" {
			t.Fatalf("AC-2 crash line = %#v", line)
		}
	}
	done, crashed := m2StatusCounts(t, run.dataDir)
	if done != 90 || crashed != 10 {
		t.Fatalf("AC-2 SQLite status done=%d crash=%d, want 90/10", done, crashed)
	}
	ready := waitM2LogCount(
		t,
		filepath.Join(run.dataDir, "agent.log"),
		"worker ready",
		14,
		10*time.Second,
	)
	if ready != 14 {
		t.Fatalf("AC-2 worker ready lines = %d, want 14", ready)
	}
	var workerPIDs []int64
	for _, line := range crashes {
		workerPIDs = append(workerPIDs, int64(line["pid"].(float64)))
	}
	t.Logf(
		"AC-2 agent_pid=%d crash_worker_pids=%v total=100 sqlite_done=90 sqlite_crash=10 ready=14 exit_code=-1073741819",
		agentPID, workerPIDs,
	)
}

func TestM2AC3NativeHangWatchdog(t *testing.T) {
	run := newM2Run(t, m2RunOptions{
		workerCount: 4, crashInjection: true, imageTimeout: 30,
	})
	seed := filepath.Join(requiredM2Corpus(t), "base", "valid.jpg")
	for index := 0; index < 9; index++ {
		copyM2File(t, seed, filepath.Join(run.sourceDir, fmt.Sprintf("normal_%02d.jpg", index)))
	}
	hangPath := filepath.Join(run.sourceDir, "slow__hang__.jpg")
	copyM2File(t, seed, hangPath)
	run.start(t)
	agentPID := run.hello.PID
	started := time.Now()
	result := run.scan(t, "m2-ac3-"+run.machineID, run.sourceDir, 60*time.Second)
	elapsed := time.Since(started)
	if elapsed >= 60*time.Second {
		t.Fatalf("AC-3 elapsed = %v, want <60s", elapsed)
	}
	if run.process.pid() != agentPID || !run.process.running() {
		t.Fatalf("AC-3 Agent changed/exited pid=%d", agentPID)
	}
	if result.Done.Stats.Total != 10 || result.Done.Stats.Crashes != 1 {
		t.Fatalf("AC-3 stats = %#v", result.Done.Stats)
	}
	crashes := readM2JSONLines(t, filepath.Join(run.dataDir, "crash.log"))
	if len(crashes) != 1 {
		t.Fatalf("AC-3 crash lines = %d, want 1", len(crashes))
	}
	line := crashes[0]
	if line["reason"] != "watchdog_image" ||
		!sameM2File(t, line["file"].(string), hangPath) {
		t.Fatalf("AC-3 crash line = %#v", line)
	}
	crashTime, err := time.Parse(time.RFC3339Nano, line["time"].(string))
	if err != nil {
		t.Fatal(err)
	}
	watchdogElapsed := crashTime.Sub(started)
	if watchdogElapsed < 27*time.Second || watchdogElapsed > 33*time.Second {
		t.Fatalf("AC-3 watchdog elapsed = %v, want 30s ±3s", watchdogElapsed)
	}
	done, crashed := m2StatusCounts(t, run.dataDir)
	if done != 9 || crashed != 1 {
		t.Fatalf("AC-3 SQLite status done=%d crash=%d, want 9/1", done, crashed)
	}
	ready := waitM2LogCount(
		t,
		filepath.Join(run.dataDir, "agent.log"),
		"worker ready",
		5,
		10*time.Second,
	)
	if ready != 5 {
		t.Fatalf("AC-3 worker ready lines = %d, want 5", ready)
	}
	t.Logf(
		"AC-3 agent_pid=%d crash_worker_pid=%d watchdog_ms=%d sqlite_done=9 sqlite_crash=1 ready=5 elapsed_ms=%d",
		agentPID,
		int64(line["pid"].(float64)),
		watchdogElapsed.Milliseconds(),
		elapsed.Milliseconds(),
	)
}

func TestM2AC4SingleFlight(t *testing.T) {
	run := newM2Run(t, m2RunOptions{workerCount: 8})
	corpus := requiredM2Corpus(t)
	imageRoot := filepath.Join(run.sourceDir, "images")
	videoRoot := filepath.Join(run.sourceDir, "videos")
	copyM2Directory(t, filepath.Join(corpus, "singleflight", "images"), imageRoot)
	copyM2Directory(t, filepath.Join(corpus, "singleflight", "videos"), videoRoot)

	run.start(t)
	images := run.scan(t, "m2-ac4-images-"+run.machineID, imageRoot, 90*time.Second)
	if images.Done.Stats.Total != 100 ||
		images.Done.Stats.DecodeCalls != 1 ||
		images.Done.Stats.SingleFlightHits != 99 {
		t.Fatalf("AC-4 image stats = %#v, want total/decode/singleflight=100/1/99", images.Done.Stats)
	}
	rows, distinctSHA, done := m2FileSHAStats(t, run.dataDir, "%.jpg")
	imageFeatures, videoFeatures := m2FeatureCounts(t, run.dataDir)
	if rows != 100 || distinctSHA != 1 || done != 100 || imageFeatures != 1 || videoFeatures != 0 {
		t.Fatalf("AC-4 image persistence rows=%d distinct_sha=%d done=%d features=%d/%d",
			rows, distinctSHA, done, imageFeatures, videoFeatures)
	}

	videos := run.scan(t, "m2-ac4-videos-"+run.machineID, videoRoot, 120*time.Second)
	if videos.Done.Stats.Total != 20 ||
		videos.Done.Stats.ThumbGenerated != 1 ||
		videos.Done.Stats.SingleFlightHits != 19 {
		t.Fatalf("AC-4 video stats = %#v, want total/generated/singleflight=20/1/19", videos.Done.Stats)
	}
	rows, distinctSHA, done = m2FileSHAStats(t, run.dataDir, "%.mp4")
	imageFeatures, videoFeatures = m2FeatureCounts(t, run.dataDir)
	if rows != 20 || distinctSHA != 1 || done != 20 || imageFeatures != 1 || videoFeatures != 1 {
		t.Fatalf("AC-4 video persistence rows=%d distinct_sha=%d done=%d features=%d/%d",
			rows, distinctSHA, done, imageFeatures, videoFeatures)
	}
	if len(readM2JSONLinesOptional(t, filepath.Join(run.dataDir, "crash.log"))) != 0 {
		t.Fatal("AC-4 unexpected worker crash")
	}
	t.Logf(
		"AC-4 agent_pid=%d image_decode_calls=1 image_singleflight_hits=99 video_thumb_generated=1 video_singleflight_hits=19 sqlite_image_rows=1 sqlite_video_rows=1",
		run.hello.PID,
	)
}

func TestM2AC5ThumbnailCache(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "retained-thumb-cache")
	run := newM2Run(t, m2RunOptions{workerCount: 4, cacheDir: cacheDir})
	copyM2Directory(t, filepath.Join(requiredM2Corpus(t), "cache"), run.sourceDir)
	run.start(t)

	round1 := run.scan(t, "m2-ac5-round1-"+run.machineID, run.sourceDir, 120*time.Second)
	if round1.Done.Stats.Total != 10 ||
		round1.Done.Stats.ThumbGenerated != 10 ||
		round1.Done.Stats.ThumbCacheHits != 0 {
		t.Fatalf("AC-5 round1 stats = %#v, want total/generated/hits=10/10/0", round1.Done.Stats)
	}
	cache1 := m2CacheSnapshot(t, cacheDir)
	if cache1.jpegCount != 10 || cache1.sidecarCount != 10 {
		t.Fatalf("AC-5 round1 cache JPEG/sidecar=%d/%d, want 10/10",
			cache1.jpegCount, cache1.sidecarCount)
	}
	features1 := m2VideoFeatureSnapshot(t, run.dataDir)
	if len(features1) != 10 {
		t.Fatalf("AC-5 round1 video features=%d, want 10", len(features1))
	}

	m2ResetVideoState(t, run.dataDir)
	round2 := run.scan(t, "m2-ac5-round2-"+run.machineID, run.sourceDir, 120*time.Second)
	if round2.Done.Stats.ThumbGenerated != 0 ||
		round2.Done.Stats.ThumbCacheHits != 10 {
		t.Fatalf("AC-5 round2 stats = %#v, want generated/hits=0/10", round2.Done.Stats)
	}
	cache2 := m2CacheSnapshot(t, cacheDir)
	features2 := m2VideoFeatureSnapshot(t, run.dataDir)
	if fmt.Sprint(cache2.bundles) != fmt.Sprint(cache1.bundles) {
		t.Fatal("AC-5 round2 changed retained cache")
	}
	if fmt.Sprint(features2) != fmt.Sprint(features1) {
		t.Fatal("AC-5 round2 feature values differ from round1")
	}

	changedSource := filepath.Join(run.sourceDir, "video_03.mp4")
	info, err := os.Stat(changedSource)
	if err != nil {
		t.Fatal(err)
	}
	changedTime := info.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(changedSource, changedTime, changedTime); err != nil {
		t.Fatal(err)
	}
	m2ResetVideoState(t, run.dataDir)
	round3 := run.scan(t, "m2-ac5-round3-"+run.machineID, run.sourceDir, 120*time.Second)
	if round3.Done.Stats.ThumbGenerated != 1 ||
		round3.Done.Stats.ThumbCacheHits != 9 {
		t.Fatalf("AC-5 round3 stats = %#v, want generated/hits=1/9", round3.Done.Stats)
	}
	cache3 := m2CacheSnapshot(t, cacheDir)
	if cache3.jpegCount != 10 || cache3.sidecarCount != 10 {
		t.Fatalf("AC-5 round3 cache JPEG/sidecar=%d/%d, want 10/10",
			cache3.jpegCount, cache3.sidecarCount)
	}
	if changed := m2ChangedCacheBundles(cache2.bundles, cache3.bundles); changed != 1 {
		t.Fatalf("AC-5 round3 changed cache bundles=%d, want 1", changed)
	}
	t.Logf(
		"AC-5 agent_pid=%d round1_generated_hits=10/0 cache_jpeg_sidecar=10/10 round2_generated_hits=0/10 round3_generated_hits=1/9 changed_bundles=1",
		run.hello.PID,
	)
}

func TestM2AC6PathsAndAccess(t *testing.T) {
	run := newM2Run(t, m2RunOptions{workerCount: 4})
	corpusPaths := filepath.Join(requiredM2Corpus(t), "paths")
	unicodePath := filepath.Join(run.sourceDir, "图片_😀 副本.jpg")
	readonlyPath := filepath.Join(run.sourceDir, "readonly.jpg")
	deniedPath := filepath.Join(run.sourceDir, "denied.jpg")
	copyM2File(t, filepath.Join(corpusPaths, "图片_😀 副本.jpg"), unicodePath)
	copyM2File(t, filepath.Join(corpusPaths, "readonly.jpg"), readonlyPath)
	copyM2File(t, filepath.Join(corpusPaths, "denied.jpg"), deniedPath)
	longSource := findM2FileByBase(t, filepath.Join(corpusPaths, "long"), "long.jpg")
	longRelative, err := filepath.Rel(filepath.Join(corpusPaths, "long"), longSource)
	if err != nil {
		t.Fatal(err)
	}
	longPath := filepath.Join(run.sourceDir, "long", longRelative)
	copyM2File(t, longSource, longPath)
	if len(longPath) <= 260 {
		t.Fatalf("AC-6 long path length=%d, want >260: %s", len(longPath), longPath)
	}

	if err := os.Chmod(readonlyPath, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(readonlyPath, 0o666) })
	lockedUTF16, err := windows.UTF16PtrFromString(deniedPath)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := windows.CreateFile(
		lockedUTF16,
		windows.GENERIC_READ,
		0,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = windows.CloseHandle(lock) })

	run.start(t)
	result := run.scan(t, "m2-ac6-"+run.machineID, run.sourceDir, 90*time.Second)
	if result.Done.Stats.Total != 4 || !run.process.running() {
		t.Fatalf("AC-6 stats=%#v Agent running=%t", result.Done.Stats, run.process.running())
	}
	status := make(map[string]string)
	fieldFailures := 0
	for _, item := range result.Features {
		status[filepath.Base(item.Path)] = item.Status
		fieldFailures += len(item.FieldErrors)
	}
	for _, name := range []string{"图片_😀 副本.jpg", "readonly.jpg", "long.jpg"} {
		if status[name] != proto.StatusDone {
			t.Fatalf("AC-6 %s status=%q, want done; all=%#v", name, status[name], status)
		}
	}
	if status["denied.jpg"] != proto.StatusFailed {
		t.Fatalf("AC-6 denied status=%q, want failed", status["denied.jpg"])
	}
	errors := readM2JSONLines(t, filepath.Join(run.dataDir, "errors.log"))
	if len(errors) != 1 || fieldFailures != 1 {
		t.Fatalf("AC-6 errors log=%d field failures=%d, want 1/1; errors=%#v features=%#v",
			len(errors), fieldFailures, errors, result.Features)
	}
	if errors[0]["stage"] != "open" ||
		!strings.EqualFold(filepath.Base(errors[0]["path"].(string)), "denied.jpg") {
		t.Fatalf("AC-6 denied error=%#v, want open stage", errors[0])
	}
	counts := m2FileStatusMap(t, run.dataDir)
	if counts["done"] != 3 || counts["failed"] != 1 {
		t.Fatalf("AC-6 SQLite statuses=%#v, want done=3 failed=1", counts)
	}
	t.Logf(
		"AC-6 agent_pid=%d long_path_chars=%d sqlite_done=3 sqlite_failed=1 error_lines=1 denied_stage=open",
		run.hello.PID, len(longPath),
	)
}

func TestM2AC8Baseline(t *testing.T) {
	run := newM2Run(t, m2RunOptions{workerCount: 8})
	warmupDir := filepath.Join(run.sourceDir, "warmup")
	measureDir := filepath.Join(run.sourceDir, "measure")
	copyM2Directory(t, filepath.Join(requiredM2Corpus(t), "warmup"), warmupDir)
	copyM2Directory(t, filepath.Join(requiredM2Corpus(t), "smoke"), measureDir)
	run.start(t)
	warmup := run.scan(t, "m2-ac8-warmup-"+run.machineID, warmupDir, 180*time.Second)
	if warmup.Done.Stats.Total != 2000 ||
		warmup.Done.Stats.FilesDone != 2000 ||
		warmup.Done.Stats.FilesFailed != 0 ||
		warmup.Done.Stats.DecodeCalls != 2000 ||
		warmup.Done.Stats.ReadAttempts != 2000 ||
		warmup.Done.Stats.DecodeAttempts != 2000 {
		t.Fatalf("AC-8 warmup incomplete stats=%#v", warmup.Done.Stats)
	}
	stableSamples := waitM2RSSStable(t, run.process.pid(), 10*time.Second)

	stopSamples := make(chan struct{})
	samplesDone := make(chan []m2RSSSample, 1)
	scanStarted := time.Now()
	go func() {
		samplesDone <- sampleM2RSSLoop(run.process.pid(), stopSamples)
	}()
	result := run.scan(t, "m2-ac8-measure-"+run.machineID, measureDir, 180*time.Second)
	scanEnded := time.Now()
	close(stopSamples)
	samples := <-samplesDone

	stats := result.Done.Stats
	if stats.Total != 1000 || stats.FilesDone != 1000 ||
		stats.FilesFailed != 0 || stats.Crashes != 0 ||
		stats.DecodeCalls != 1000 ||
		stats.ReadAttempts != 1000 || stats.DecodeAttempts != 1000 ||
		stats.SingleFlightHits != 0 || !run.process.running() {
		t.Fatalf("AC-8 incomplete stats=%#v Agent running=%t", stats, run.process.running())
	}
	if stats.ElapsedMS <= 0 || stats.AvgReadMS <= 0 || stats.AvgDecodeMS <= 0 {
		t.Fatalf("AC-8 invalid elapsed/averages=%#v", stats)
	}
	filesPerSecond := float64(stats.Total) * 1000 / float64(stats.ElapsedMS)
	if len(samples) < 3 {
		t.Fatalf("AC-8 RSS samples=%d, want at least 3", len(samples))
	}
	for _, sample := range samples {
		if sample.Time.Before(scanStarted) || sample.Time.After(scanEnded) {
			t.Fatalf("AC-8 RSS sample outside scan window %s..%s: %#v", scanStarted, scanEnded, sample)
		}
	}
	scanDuration := scanEnded.Sub(scanStarted)
	if samples[0].Time.Sub(scanStarted) > scanDuration/4 ||
		scanEnded.Sub(samples[len(samples)-1].Time) > scanDuration/4 {
		t.Fatalf("AC-8 RSS samples do not cover scan window: duration=%s first=%s last=%s",
			scanDuration, samples[0].Time.Sub(scanStarted), scanEnded.Sub(samples[len(samples)-1].Time))
	}
	if m2RSSStrictlyIncreasing(samples) {
		t.Fatalf("AC-8 RSS grew monotonically throughout formal scan: %#v", samples)
	}
	t.Logf(
		"AC-8 warmup_total=2000 warmup_decode_calls=2000 stable_rss=%s elapsed_ms=%d files_s=%.2f decode_calls=%d read_attempts=%d decode_attempts=%d avg_read_ms=%.6f avg_decode_ms=%.6f rss_scan_window=%s rss_monotonic=false",
		m2RSSJSON(t, stableSamples),
		stats.ElapsedMS,
		filesPerSecond,
		stats.DecodeCalls,
		stats.ReadAttempts,
		stats.DecodeAttempts,
		stats.AvgReadMS,
		stats.AvgDecodeMS,
		m2RSSJSON(t, samples),
	)
}

func TestM2RSSStrictGrowthContract(t *testing.T) {
	increasing := []m2RSSSample{{Bytes: 1}, {Bytes: 2}, {Bytes: 3}}
	if !m2RSSStrictlyIncreasing(increasing) {
		t.Fatal("strictly increasing RSS sequence was not rejected")
	}
	for name, samples := range map[string][]m2RSSSample{
		"plateau": {{Bytes: 1}, {Bytes: 2}, {Bytes: 2}},
		"decline": {{Bytes: 2}, {Bytes: 3}, {Bytes: 1}},
	} {
		if m2RSSStrictlyIncreasing(samples) {
			t.Fatalf("%s RSS sequence was treated as monotonic growth: %#v", name, samples)
		}
	}
}

func TestM2PostgresCleanupRestoresFeatureSnapshot(t *testing.T) {
	dsn := requiredM2Env(t, "FS_PG_DSN")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	token := strconv.FormatInt(time.Now().UnixNano(), 36)
	machineID := "m2-cleanup-" + token
	existingSHA := m2SHA512Hex([]byte("m2-existing-" + token))
	newSHA := m2SHA512Hex([]byte("m2-new-" + token))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM files WHERE machine_id=$1`, machineID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM image_features WHERE sha512=ANY($1::text[])`, []string{existingSHA, newSHA})
		_, _ = pool.Exec(context.Background(), `DELETE FROM video_features WHERE sha512=ANY($1::text[])`, []string{existingSHA, newSHA})
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO image_features
		    (sha512,width,height,pdq256,pdq_quality,phash_parts,sobel_hist,updated_at)
		VALUES ($1,11,12,$2,13,$3,$4,to_timestamp(1700000000))`,
		existingSHA, []byte{1}, []byte{2}, []byte{3},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO video_features
		    (sha512,duration_ms,thumb_path,thumb_pdq256,thumb_quality,updated_at)
		VALUES ($1,14,'sentinel-thumb',$2,15,to_timestamp(1700000001))`,
		existingSHA, []byte{4},
	); err != nil {
		t.Fatal(err)
	}
	snapshot := snapshotM2PostgresFeatures(t, dsn, []string{existingSHA, newSHA})
	if _, err := pool.Exec(ctx, `
		UPDATE image_features SET width=91,height=92,pdq256=$2,pdq_quality=93,
		    phash_parts=NULL,sobel_hist=NULL,updated_at=now() WHERE sha512=$1`,
		existingSHA, []byte{9},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE video_features SET duration_ms=94,thumb_path='overwritten',
		    thumb_pdq256=$2,thumb_quality=95,updated_at=now() WHERE sha512=$1`,
		existingSHA, []byte{8},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO image_features (sha512,width,height,pdq256,pdq_quality)
		    VALUES ($1,21,22,$2,23)`,
		newSHA, []byte{9},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO video_features (sha512,duration_ms,thumb_path,thumb_pdq256,thumb_quality)
		    VALUES ($1,24,'new-thumb',$2,25)`,
		newSHA, []byte{8},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO files (machine_id,path,sha512,status)
		    VALUES ($1,'existing',$2,'done'),($1,'new',$3,'done')`,
		machineID, existingSHA, newSHA,
	); err != nil {
		t.Fatal(err)
	}
	cleanupM2Postgres(t, dsn, machineID, snapshot)

	var width, height, quality int
	var pdq, phash, sobel []byte
	var updated int64
	if err := pool.QueryRow(ctx, `
		SELECT width,height,pdq256,pdq_quality,phash_parts,sobel_hist,
		       extract(epoch FROM updated_at)::bigint
		FROM image_features WHERE sha512=$1`, existingSHA,
	).Scan(&width, &height, &pdq, &quality, &phash, &sobel, &updated); err != nil {
		t.Fatal(err)
	}
	if width != 11 || height != 12 || quality != 13 ||
		!bytes.Equal(pdq, []byte{1}) || !bytes.Equal(phash, []byte{2}) ||
		!bytes.Equal(sobel, []byte{3}) || updated != 1700000000 {
		t.Fatalf("restored image row=%d/%d/%v/%d/%v/%v/%d",
			width, height, pdq, quality, phash, sobel, updated)
	}
	var duration int64
	var thumbPath string
	var thumbPDQ []byte
	var thumbQuality int
	if err := pool.QueryRow(ctx, `
		SELECT duration_ms,thumb_path,thumb_pdq256,thumb_quality
		FROM video_features WHERE sha512=$1`, existingSHA,
	).Scan(&duration, &thumbPath, &thumbPDQ, &thumbQuality); err != nil {
		t.Fatal(err)
	}
	if duration != 14 || thumbPath != "sentinel-thumb" ||
		!bytes.Equal(thumbPDQ, []byte{4}) || thumbQuality != 15 {
		t.Fatalf("restored video row=%d/%q/%v/%d", duration, thumbPath, thumbPDQ, thumbQuality)
	}
	var files, newImages, newVideos int
	if err := pool.QueryRow(ctx, `
		SELECT
		    (SELECT count(*) FROM files WHERE machine_id=$1),
		    (SELECT count(*) FROM image_features WHERE sha512=$2),
		    (SELECT count(*) FROM video_features WHERE sha512=$2)`,
		machineID, newSHA,
	).Scan(&files, &newImages, &newVideos); err != nil {
		t.Fatal(err)
	}
	if files != 0 || newImages != 0 || newVideos != 0 {
		t.Fatalf("cleanup residual files/image/video=%d/%d/%d", files, newImages, newVideos)
	}
}

func TestM2ProcessStopTreeAuditsScopedDescendants(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	script := `$child=Start-Process powershell.exe -PassThru -WindowStyle Hidden ` +
		`-ArgumentList '-NoProfile','-Command','Start-Sleep -Seconds 60';` +
		`Set-Content -LiteralPath '` + strings.ReplaceAll(pidFile, `'`, `''`) + `' -Value $child.Id;` +
		`Wait-Process -Id $child.Id`
	process := startM2Process(t, "powershell.exe", "-NoProfile", "-Command", script)
	deadline := time.Now().Add(10 * time.Second)
	var childPID int
	for childPID == 0 {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			childPID, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
		if time.Now().After(deadline) {
			t.Fatalf("child PID was not published: %v output=%s", err, process.output.String())
		}
		time.Sleep(25 * time.Millisecond)
	}
	audit, err := process.stopTree()
	if err != nil {
		t.Fatalf("stopTree: %v audit=%#v", err, audit)
	}
	if _, ok := audit.Scoped[process.pid()]; !ok {
		t.Fatalf("root PID %d absent from scoped audit %#v", process.pid(), audit)
	}
	if _, ok := audit.Scoped[childPID]; !ok {
		t.Fatalf("child PID %d absent from scoped audit %#v", childPID, audit)
	}
	if len(audit.Residual) != 0 {
		t.Fatalf("scoped residual processes = %#v", audit.Residual)
	}
}

func sameM2File(t *testing.T, left, right string) bool {
	t.Helper()
	leftInfo, err := os.Stat(left)
	if err != nil {
		t.Fatal(err)
	}
	rightInfo, err := os.Stat(right)
	if err != nil {
		t.Fatal(err)
	}
	return os.SameFile(leftInfo, rightInfo)
}

type m2RunOptions struct {
	workerCount    int
	crashInjection bool
	imageTimeout   int
	videoTimeout   int
	cacheDir       string
}

func newM2Run(t *testing.T, options m2RunOptions) *m2Run {
	t.Helper()
	if options.workerCount == 0 {
		options.workerCount = 4
	}
	if options.imageTimeout == 0 {
		options.imageTimeout = 30
	}
	if options.videoTimeout == 0 {
		options.videoTimeout = 120
	}
	binDir := requiredM2Env(t, "M2_BIN_DIR")
	dsn := requiredM2Env(t, "FS_PG_DSN")
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	sourceDir := filepath.Join(root, "source")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if options.cacheDir == "" {
		options.cacheDir = filepath.Join(dataDir, "thumbcache")
	}
	machineID := "m2-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	address := freeAddress(t)
	configPath := filepath.Join(root, "agent.json")
	writeJSON(t, configPath, map[string]any{
		"machine_id":     machineID,
		"listen_addr":    address,
		"data_dir":       dataDir,
		"pg_dsn":         dsn,
		"use_everything": false,
		"scan": map[string]any{
			"hdd_read_block_mb":     4,
			"hdd_streams_per_disk":  2,
			"ssd_streams_per_disk":  6,
			"image_mem_resident_mb": 256,
			"image_timeout_s":       options.imageTimeout,
			"video_timeout_s":       options.videoTimeout,
		},
		"sync": map[string]any{
			"interval_s": 1, "trigger_rows": 1, "upsert_batch": 5000,
		},
		"proto": map[string]any{"heartbeat_s": 1},
		"worker": map[string]any{
			"count":            options.workerCount,
			"exe_path":         filepath.Join(binDir, "worker.exe"),
			"image_timeout_s":  options.imageTimeout,
			"video_timeout_s":  options.videoTimeout,
			"image_memory_mb":  256,
			"respawn_delay_ms": 500,
			"crash_injection":  options.crashInjection,
		},
		"pipeline": map[string]any{"read_chunk_kb": 4096},
		"thumb": map[string]any{
			"cache_dir":         options.cacheDir,
			"max_side":          256,
			"ffmpeg_path":       filepath.Join(binDir, "tools", "ffmpeg.exe"),
			"ffprobe_path":      filepath.Join(binDir, "tools", "ffprobe.exe"),
			"ffprobe_timeout_s": 15,
			"ffmpeg_timeout_s":  60,
		},
		"ipc": map[string]any{"max_frame_mb": 16},
	})
	run := &m2Run{
		root: root, dataDir: dataDir, sourceDir: sourceDir,
		configPath: configPath, machineID: machineID, address: address,
	}
	t.Cleanup(func() {
		if run.conn != nil {
			_ = run.conn.Close()
		}
		if run.process != nil {
			audit, err := run.process.stopTree()
			if err != nil {
				t.Errorf("M2 process cleanup: %v audit=%#v", err, audit)
			} else {
				t.Logf("M2 process cleanup root_pid=%d scoped=%v residual=0 taskkill_exit=%d wait=%q",
					audit.RootPID, audit.Scoped, audit.TaskkillExit, audit.WaitError)
			}
		}
		cleanupM2Postgres(t, dsn, machineID, run.pgSnapshot)
	})
	return run
}

type m2PGImageFeature struct {
	SHA512     string
	Width      int32
	Height     int32
	PDQ        []byte
	Quality    int32
	PHashParts []byte
	SobelHist  []byte
	UpdatedAt  time.Time
}

type m2PGVideoFeature struct {
	SHA512       string
	DurationMS   *int64
	ThumbPath    *string
	ThumbPDQ     []byte
	ThumbQuality *int32
	UpdatedAt    time.Time
}

type m2PGFeatureSnapshot struct {
	Hashes []string
	Images map[string]m2PGImageFeature
	Videos map[string]m2PGVideoFeature
}

func snapshotM2PostgresFeatures(t *testing.T, dsn string, hashes []string) *m2PGFeatureSnapshot {
	t.Helper()
	hashSet := make(map[string]struct{}, len(hashes))
	for _, hash := range hashes {
		if len(hash) != 128 {
			t.Fatalf("refusing PostgreSQL feature snapshot for invalid SHA-512 %q", hash)
		}
		hashSet[hash] = struct{}{}
	}
	hashes = hashes[:0]
	for hash := range hashSet {
		hashes = append(hashes, hash)
	}
	sort.Strings(hashes)
	snapshot := &m2PGFeatureSnapshot{
		Hashes: hashes,
		Images: make(map[string]m2PGImageFeature),
		Videos: make(map[string]m2PGVideoFeature),
	}
	if len(hashes) == 0 {
		return snapshot
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open PostgreSQL for feature snapshot: %v", err)
	}
	defer pool.Close()
	imageRows, err := pool.Query(ctx, `
		SELECT sha512,width,height,pdq256,pdq_quality,phash_parts,sobel_hist,updated_at
		FROM image_features WHERE sha512=ANY($1::text[])`, hashes)
	if err != nil {
		t.Fatalf("snapshot PostgreSQL image features: %v", err)
	}
	for imageRows.Next() {
		var row m2PGImageFeature
		if err := imageRows.Scan(
			&row.SHA512, &row.Width, &row.Height, &row.PDQ, &row.Quality,
			&row.PHashParts, &row.SobelHist, &row.UpdatedAt,
		); err != nil {
			imageRows.Close()
			t.Fatalf("scan PostgreSQL image feature snapshot: %v", err)
		}
		snapshot.Images[row.SHA512] = row
	}
	if err := imageRows.Err(); err != nil {
		imageRows.Close()
		t.Fatalf("read PostgreSQL image feature snapshot: %v", err)
	}
	imageRows.Close()
	videoRows, err := pool.Query(ctx, `
		SELECT sha512,duration_ms,thumb_path,thumb_pdq256,thumb_quality,updated_at
		FROM video_features WHERE sha512=ANY($1::text[])`, hashes)
	if err != nil {
		t.Fatalf("snapshot PostgreSQL video features: %v", err)
	}
	for videoRows.Next() {
		var row m2PGVideoFeature
		if err := videoRows.Scan(
			&row.SHA512, &row.DurationMS, &row.ThumbPath, &row.ThumbPDQ,
			&row.ThumbQuality, &row.UpdatedAt,
		); err != nil {
			videoRows.Close()
			t.Fatalf("scan PostgreSQL video feature snapshot: %v", err)
		}
		snapshot.Videos[row.SHA512] = row
	}
	if err := videoRows.Err(); err != nil {
		videoRows.Close()
		t.Fatalf("read PostgreSQL video feature snapshot: %v", err)
	}
	videoRows.Close()
	t.Logf("PostgreSQL feature snapshot hashes=%d image=%d video=%d",
		len(snapshot.Hashes), len(snapshot.Images), len(snapshot.Videos))
	return snapshot
}

func auditM2PostgresFeatureSnapshot(
	t *testing.T,
	ctx context.Context,
	tx pgx.Tx,
	snapshot *m2PGFeatureSnapshot,
) bool {
	t.Helper()
	for hash, want := range snapshot.Images {
		var got m2PGImageFeature
		if err := tx.QueryRow(ctx, `
			SELECT sha512,width,height,pdq256,pdq_quality,phash_parts,sobel_hist,updated_at
			FROM image_features WHERE sha512=$1`, hash,
		).Scan(
			&got.SHA512, &got.Width, &got.Height, &got.PDQ, &got.Quality,
			&got.PHashParts, &got.SobelHist, &got.UpdatedAt,
		); err != nil {
			t.Errorf("audit restored PostgreSQL image feature %s: %v", hash, err)
			return false
		}
		if got.SHA512 != want.SHA512 || got.Width != want.Width ||
			got.Height != want.Height || got.Quality != want.Quality ||
			!bytes.Equal(got.PDQ, want.PDQ) ||
			!bytes.Equal(got.PHashParts, want.PHashParts) ||
			!bytes.Equal(got.SobelHist, want.SobelHist) ||
			!got.UpdatedAt.Equal(want.UpdatedAt) {
			t.Errorf("restored PostgreSQL image feature differs hash=%s got=%#v want=%#v",
				hash, got, want)
			return false
		}
	}
	for hash, want := range snapshot.Videos {
		var got m2PGVideoFeature
		if err := tx.QueryRow(ctx, `
			SELECT sha512,duration_ms,thumb_path,thumb_pdq256,thumb_quality,updated_at
			FROM video_features WHERE sha512=$1`, hash,
		).Scan(
			&got.SHA512, &got.DurationMS, &got.ThumbPath, &got.ThumbPDQ,
			&got.ThumbQuality, &got.UpdatedAt,
		); err != nil {
			t.Errorf("audit restored PostgreSQL video feature %s: %v", hash, err)
			return false
		}
		if got.SHA512 != want.SHA512 ||
			!m2EqualInt64Pointers(got.DurationMS, want.DurationMS) ||
			!m2EqualStringPointers(got.ThumbPath, want.ThumbPath) ||
			!bytes.Equal(got.ThumbPDQ, want.ThumbPDQ) ||
			!m2EqualInt32Pointers(got.ThumbQuality, want.ThumbQuality) ||
			!got.UpdatedAt.Equal(want.UpdatedAt) {
			t.Errorf("restored PostgreSQL video feature differs hash=%s got=%#v want=%#v",
				hash, got, want)
			return false
		}
	}
	return true
}

func m2EqualInt64Pointers(left, right *int64) bool {
	return left == nil && right == nil ||
		left != nil && right != nil && *left == *right
}

func m2EqualInt32Pointers(left, right *int32) bool {
	return left == nil && right == nil ||
		left != nil && right != nil && *left == *right
}

func m2EqualStringPointers(left, right *string) bool {
	return left == nil && right == nil ||
		left != nil && right != nil && *left == *right
}

func cleanupM2Postgres(t *testing.T, dsn, machineID string, snapshot *m2PGFeatureSnapshot) {
	t.Helper()
	if !strings.HasPrefix(machineID, "m2-") {
		t.Errorf("refusing PostgreSQL cleanup for non-M2 machine %q", machineID)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Errorf("open PostgreSQL for cleanup: %v", err)
		return
	}
	defer pool.Close()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Errorf("begin PostgreSQL cleanup machine %q: %v", machineID, err)
		return
	}
	defer tx.Rollback(ctx)
	var before int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM files WHERE machine_id=$1`, machineID).Scan(&before); err != nil {
		t.Errorf("query PostgreSQL machine %q before cleanup: %v", machineID, err)
		return
	}
	tag, err := tx.Exec(ctx, `DELETE FROM files WHERE machine_id=$1`, machineID)
	if err != nil {
		t.Errorf("cleanup PostgreSQL machine %q: %v", machineID, err)
		return
	}
	imageDeleted, videoDeleted := int64(0), int64(0)
	imageRestored, videoRestored := 0, 0
	if snapshot != nil && len(snapshot.Hashes) != 0 {
		newImageHashes := make([]string, 0, len(snapshot.Hashes))
		newVideoHashes := make([]string, 0, len(snapshot.Hashes))
		for _, hash := range snapshot.Hashes {
			if _, existed := snapshot.Images[hash]; !existed {
				newImageHashes = append(newImageHashes, hash)
			}
			if _, existed := snapshot.Videos[hash]; !existed {
				newVideoHashes = append(newVideoHashes, hash)
			}
		}
		imageTag, err := tx.Exec(ctx, `
			DELETE FROM image_features feature
			WHERE feature.sha512=ANY($1::text[])
			  AND NOT EXISTS (SELECT 1 FROM files WHERE sha512=feature.sha512)`,
			newImageHashes,
		)
		if err != nil {
			t.Errorf("cleanup new PostgreSQL image features: %v", err)
			return
		}
		imageDeleted = imageTag.RowsAffected()
		videoTag, err := tx.Exec(ctx, `
			DELETE FROM video_features feature
			WHERE feature.sha512=ANY($1::text[])
			  AND NOT EXISTS (SELECT 1 FROM files WHERE sha512=feature.sha512)`,
			newVideoHashes,
		)
		if err != nil {
			t.Errorf("cleanup new PostgreSQL video features: %v", err)
			return
		}
		videoDeleted = videoTag.RowsAffected()
		for _, row := range snapshot.Images {
			if _, err := tx.Exec(ctx, `
				INSERT INTO image_features
				    (sha512,width,height,pdq256,pdq_quality,phash_parts,sobel_hist,updated_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
				ON CONFLICT (sha512) DO UPDATE SET
				    width=EXCLUDED.width,height=EXCLUDED.height,pdq256=EXCLUDED.pdq256,
				    pdq_quality=EXCLUDED.pdq_quality,phash_parts=EXCLUDED.phash_parts,
				    sobel_hist=EXCLUDED.sobel_hist,updated_at=EXCLUDED.updated_at`,
				row.SHA512, row.Width, row.Height, row.PDQ, row.Quality,
				row.PHashParts, row.SobelHist, row.UpdatedAt,
			); err != nil {
				t.Errorf("restore PostgreSQL image feature %s: %v", row.SHA512, err)
				return
			}
			imageRestored++
		}
		for _, row := range snapshot.Videos {
			if _, err := tx.Exec(ctx, `
				INSERT INTO video_features
				    (sha512,duration_ms,thumb_path,thumb_pdq256,thumb_quality,updated_at)
				VALUES ($1,$2,$3,$4,$5,$6)
				ON CONFLICT (sha512) DO UPDATE SET
				    duration_ms=EXCLUDED.duration_ms,thumb_path=EXCLUDED.thumb_path,
				    thumb_pdq256=EXCLUDED.thumb_pdq256,thumb_quality=EXCLUDED.thumb_quality,
				    updated_at=EXCLUDED.updated_at`,
				row.SHA512, row.DurationMS, row.ThumbPath, row.ThumbPDQ,
				row.ThumbQuality, row.UpdatedAt,
			); err != nil {
				t.Errorf("restore PostgreSQL video feature %s: %v", row.SHA512, err)
				return
			}
			videoRestored++
		}
		var imageResidual, videoResidual int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM image_features feature
			WHERE feature.sha512=ANY($1::text[])
			  AND NOT EXISTS (SELECT 1 FROM files WHERE sha512=feature.sha512)`,
			newImageHashes,
		).Scan(&imageResidual); err != nil {
			t.Errorf("audit PostgreSQL image feature cleanup: %v", err)
			return
		}
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM video_features feature
			WHERE feature.sha512=ANY($1::text[])
			  AND NOT EXISTS (SELECT 1 FROM files WHERE sha512=feature.sha512)`,
			newVideoHashes,
		).Scan(&videoResidual); err != nil {
			t.Errorf("audit PostgreSQL video feature cleanup: %v", err)
			return
		}
		if imageResidual != 0 || videoResidual != 0 {
			t.Errorf("PostgreSQL feature cleanup residual image=%d video=%d",
				imageResidual, videoResidual)
			return
		}
		if !auditM2PostgresFeatureSnapshot(t, ctx, tx, snapshot) {
			return
		}
	}
	var after int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM files WHERE machine_id=$1`, machineID).Scan(&after); err != nil {
		t.Errorf("query PostgreSQL machine %q after cleanup: %v", machineID, err)
		return
	}
	if after != 0 || int(tag.RowsAffected()) != before {
		t.Errorf("PostgreSQL cleanup machine=%q before=%d deleted=%d after=%d",
			machineID, before, tag.RowsAffected(), after)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		t.Errorf("commit PostgreSQL cleanup machine %q: %v", machineID, err)
		return
	}
	t.Logf("PostgreSQL cleanup machine=%s files_before=%d files_deleted=%d files_after=0 image_deleted=%d image_restored=%d image_residual=0 video_deleted=%d video_restored=%d video_residual=0",
		machineID, before, tag.RowsAffected(), imageDeleted, imageRestored,
		videoDeleted, videoRestored)
}

func (run *m2Run) start(t *testing.T) {
	t.Helper()
	if run.pgSnapshot == nil {
		hashes, skipped := m2FileSHA512s(t, run.sourceDir)
		if len(skipped) != 0 {
			t.Logf("PostgreSQL feature snapshot skipped_pre_sha=%d paths=%v",
				len(skipped), skipped)
		}
		run.pgSnapshot = snapshotM2PostgresFeatures(
			t,
			requiredM2Env(t, "FS_PG_DSN"),
			hashes,
		)
	}
	agentExe := filepath.Join(requiredM2Env(t, "M2_BIN_DIR"), "agent.exe")
	process := startM2Process(t, agentExe, "-config", run.configPath)
	run.process = process
	deadline := time.Now().Add(30 * time.Second)
	for {
		connection, err := net.DialTimeout("tcp", run.address, 500*time.Millisecond)
		if err == nil {
			run.conn = proto.NewConn(connection)
			if err := run.conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
				t.Fatal(err)
			}
			messageType, body, err := run.conn.ReadFrame()
			if err != nil {
				t.Fatal(err)
			}
			message, err := proto.Decode(messageType, body)
			if err != nil {
				t.Fatal(err)
			}
			hello, ok := message.(*proto.Hello)
			if !ok || messageType != proto.MsgHello {
				t.Fatalf("first Agent message = %T type=%d", message, messageType)
			}
			run.hello = *hello
			_ = run.conn.SetReadDeadline(time.Time{})
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Agent did not listen: %v output=%s", err, process.output.String())
		}
		if !process.running() {
			t.Fatalf("Agent exited before listen: %s", process.output.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (run *m2Run) scan(
	t *testing.T,
	taskID string,
	root string,
	timeout time.Duration,
) m2ScanResult {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go proto.Heartbeat(ctx, run.conn, time.Second)
	if err := run.conn.WriteFrame(proto.MsgScanTask, &proto.ScanTask{
		TaskID: taskID, Roots: []string{root}, Phase: 1,
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(timeout)
	var result m2ScanResult
	for {
		if err := run.conn.SetReadDeadline(time.Now().Add(time.Until(deadline))); err != nil {
			t.Fatal(err)
		}
		messageType, body, err := run.conn.ReadFrame()
		if err != nil {
			t.Fatalf("scan read: %v output=%s", err, run.process.output.String())
		}
		message, err := proto.Decode(messageType, body)
		if err != nil {
			t.Fatal(err)
		}
		switch value := message.(type) {
		case *proto.TaskAck:
			result.Ack = *value
			if !value.Accepted {
				t.Fatalf("task rejected: %#v", value)
			}
		case *proto.FeatureResult:
			result.Features = append(result.Features, value.Items...)
		case *proto.Error:
			result.Errors = append(result.Errors, *value)
		case *proto.CrashNotice:
			result.Crashes = append(result.Crashes, *value)
		case *proto.TaskDone:
			result.Done = *value
			return result
		}
	}
}

type m2Process struct {
	command   *exec.Cmd
	done      chan error
	output    *lockedBuffer
	once      sync.Once
	mu        sync.Mutex
	waitErr   error
	finished  bool
	stopAudit m2ProcessAudit
	stopErr   error
}

type m2ProcessAudit struct {
	RootPID      int
	Scoped       map[int]string
	Residual     map[int]string
	TaskkillExit int
	WaitError    string
}

type m2ProcessEntry struct {
	PID    int
	Parent int
	Name   string
}

func startM2Process(t *testing.T, executable string, arguments ...string) *m2Process {
	t.Helper()
	output := &lockedBuffer{}
	command := exec.Command(executable, arguments...)
	command.Stdout = output
	command.Stderr = output
	command.SysProcAttr = &syscall.SysProcAttr{
		HideWindow: true, CreationFlags: 0x08000000,
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	process := &m2Process{
		command: command, done: make(chan error, 1), output: output,
	}
	go func() {
		err := command.Wait()
		process.mu.Lock()
		process.waitErr = err
		process.finished = true
		process.mu.Unlock()
		process.done <- err
	}()
	return process
}

func (process *m2Process) pid() int {
	if process == nil || process.command.Process == nil {
		return 0
	}
	return process.command.Process.Pid
}

func (process *m2Process) running() bool {
	process.mu.Lock()
	defer process.mu.Unlock()
	return !process.finished
}

func (process *m2Process) stopTree() (m2ProcessAudit, error) {
	process.once.Do(func() {
		rootPID := process.pid()
		process.stopAudit = m2ProcessAudit{
			RootPID: rootPID, Scoped: make(map[int]string),
			Residual: make(map[int]string), TaskkillExit: -1,
		}
		before, err := snapshotM2Processes()
		if err != nil {
			process.stopErr = fmt.Errorf("snapshot process tree before cleanup: %w", err)
			return
		}
		process.stopAudit.Scoped = scopedM2ProcessTree(rootPID, before)
		if rootPID != 0 {
			if _, exists := process.stopAudit.Scoped[rootPID]; !exists {
				process.stopAudit.Scoped[rootPID] = filepath.Base(process.command.Path)
			}
		}
		killed := false
		if rootPID != 0 && process.running() {
			command := exec.Command(
				"taskkill.exe", "/PID", strconv.Itoa(rootPID), "/T", "/F",
			)
			command.SysProcAttr = &syscall.SysProcAttr{
				HideWindow: true, CreationFlags: 0x08000000,
			}
			output, killErr := command.CombinedOutput()
			if command.ProcessState != nil {
				process.stopAudit.TaskkillExit = command.ProcessState.ExitCode()
			}
			if killErr != nil {
				process.stopErr = fmt.Errorf(
					"taskkill scoped tree root=%d exit=%d: %w output=%s",
					rootPID, process.stopAudit.TaskkillExit, killErr, strings.TrimSpace(string(output)),
				)
			} else {
				killed = true
			}
		}
		select {
		case waitErr := <-process.done:
			if waitErr != nil {
				process.stopAudit.WaitError = waitErr.Error()
				var exitErr *exec.ExitError
				if !killed || !errors.As(waitErr, &exitErr) {
					process.stopErr = errors.Join(
						process.stopErr,
						fmt.Errorf("wait scoped root=%d: %w", rootPID, waitErr),
					)
				}
			}
		case <-time.After(10 * time.Second):
			process.stopErr = errors.Join(
				process.stopErr,
				fmt.Errorf("wait scoped root=%d timed out after 10s", rootPID),
			)
		}
		after, err := snapshotM2Processes()
		if err != nil {
			process.stopErr = errors.Join(
				process.stopErr,
				fmt.Errorf("snapshot process tree after cleanup: %w", err),
			)
			return
		}
		process.stopAudit.Residual = residualM2ProcessTree(
			rootPID,
			process.stopAudit.Scoped,
			after,
		)
		if len(process.stopAudit.Residual) != 0 {
			process.stopErr = errors.Join(
				process.stopErr,
				fmt.Errorf("scoped process residual: %v", process.stopAudit.Residual),
			)
		}
	})
	return process.stopAudit, process.stopErr
}

func snapshotM2Processes() (map[int]m2ProcessEntry, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return nil, err
	}
	entries := make(map[int]m2ProcessEntry)
	for {
		pid := int(entry.ProcessID)
		entries[pid] = m2ProcessEntry{
			PID: pid, Parent: int(entry.ParentProcessID),
			Name: windows.UTF16ToString(entry.ExeFile[:]),
		}
		entry.Size = uint32(unsafe.Sizeof(windows.ProcessEntry32{}))
		err = windows.Process32Next(snapshot, &entry)
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	return entries, nil
}

func scopedM2ProcessTree(rootPID int, entries map[int]m2ProcessEntry) map[int]string {
	scoped := make(map[int]string)
	if root, exists := entries[rootPID]; exists {
		scoped[rootPID] = root.Name
	}
	changed := true
	for changed {
		changed = false
		for pid, entry := range entries {
			if _, exists := scoped[pid]; exists {
				continue
			}
			if entry.Parent == rootPID {
				scoped[pid] = entry.Name
				changed = true
				continue
			}
			if _, parentScoped := scoped[entry.Parent]; parentScoped {
				scoped[pid] = entry.Name
				changed = true
			}
		}
	}
	return scoped
}

func residualM2ProcessTree(
	rootPID int,
	scoped map[int]string,
	entries map[int]m2ProcessEntry,
) map[int]string {
	residual := make(map[int]string)
	for pid, name := range scoped {
		if _, exists := entries[pid]; exists {
			residual[pid] = name
		}
	}
	for pid, entry := range entries {
		seen := make(map[int]struct{})
		parent := entry.Parent
		for parent != 0 {
			if parent == rootPID {
				residual[pid] = entry.Name
				break
			}
			if _, wasScoped := scoped[parent]; wasScoped {
				residual[pid] = entry.Name
				break
			}
			if _, loop := seen[parent]; loop {
				break
			}
			seen[parent] = struct{}{}
			next, exists := entries[parent]
			if !exists {
				break
			}
			parent = next.Parent
		}
	}
	return residual
}

func requiredM2Corpus(t *testing.T) string {
	t.Helper()
	return requiredM2Env(t, "M2_CORPUS_DIR")
}

func requiredM2Env(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s is required for m2acceptance tests", name)
	}
	return value
}

func m2FileSHA512s(t *testing.T, root string) ([]string, []string) {
	t.Helper()
	hashSet := make(map[string]struct{})
	var skipped []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			if errors.Is(err, os.ErrPermission) ||
				errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
				errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
				errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
				skipped = append(skipped, path)
				return nil
			}
			return err
		}
		hasher := sha512.New()
		_, copyErr := io.Copy(hasher, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		hashSet[hex.EncodeToString(hasher.Sum(nil))] = struct{}{}
		return nil
	})
	if err != nil {
		t.Fatalf("hash M2 source files for PostgreSQL snapshot: %v", err)
	}
	hashes := make([]string, 0, len(hashSet))
	for hash := range hashSet {
		hashes = append(hashes, hash)
	}
	sort.Strings(hashes)
	sort.Strings(skipped)
	return hashes, skipped
}

func m2SHA512Hex(data []byte) string {
	sum := sha512.Sum512(data)
	return hex.EncodeToString(sum[:])
}

func copyM2File(t *testing.T, source, destination string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.Create(destination)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
}

func copyM2Directory(t *testing.T, source, destination string) {
	t.Helper()
	err := filepath.Walk(source, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		copyM2File(t, path, target)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func findM2FileByBase(t *testing.T, root, base string) string {
	t.Helper()
	var found string
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !info.IsDir() && filepath.Base(path) == base {
			found = path
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == "" {
		t.Fatalf("%s not found under %s", base, root)
	}
	return found
}

func readM2JSONLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var lines []map[string]any
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var line map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("invalid JSON line %s: %v", scanner.Text(), err)
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

func readM2JSONLinesOptional(t *testing.T, path string) []map[string]any {
	t.Helper()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		t.Fatal(err)
	}
	return readM2JSONLines(t, path)
}

func openM2DB(t *testing.T, dataDir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dataDir, "agent.db"))+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func m2FileStatusMap(t *testing.T, dataDir string) map[string]int {
	t.Helper()
	rows, err := openM2DB(t, dataDir).Query(`SELECT status, count(*) FROM files GROUP BY status`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	counts := make(map[string]int)
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			t.Fatal(err)
		}
		counts[status] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return counts
}

func m2FileRowCount(t *testing.T, dataDir string) int {
	t.Helper()
	var count int
	if err := openM2DB(t, dataDir).QueryRow(`SELECT count(*) FROM files`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func m2FeatureCounts(t *testing.T, dataDir string) (images, videos int) {
	t.Helper()
	db := openM2DB(t, dataDir)
	if err := db.QueryRow(`SELECT count(*) FROM image_features`).Scan(&images); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM video_features`).Scan(&videos); err != nil {
		t.Fatal(err)
	}
	return images, videos
}

type m2FileState struct {
	Status      string
	MissingMask int64
	SHA512      string
	Error       string
}

func m2FileStateSnapshot(t *testing.T, dataDir string) map[string]m2FileState {
	t.Helper()
	rows, err := openM2DB(t, dataDir).Query(`
		SELECT path, status, missing_mask, coalesce(sha512, ''), coalesce(error, '')
		FROM files ORDER BY path`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := make(map[string]m2FileState)
	for rows.Next() {
		var path string
		var state m2FileState
		if err := rows.Scan(
			&path, &state.Status, &state.MissingMask, &state.SHA512, &state.Error,
		); err != nil {
			t.Fatal(err)
		}
		result[path] = state
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func equalM2FileStates(left, right map[string]m2FileState) bool {
	if len(left) != len(right) {
		return false
	}
	for path, state := range left {
		other, ok := right[path]
		if !ok ||
			other.Status != state.Status ||
			other.MissingMask != state.MissingMask ||
			other.SHA512 != state.SHA512 {
			return false
		}
	}
	return true
}

func m2AllFeatureSnapshot(t *testing.T, dataDir string) []string {
	t.Helper()
	db := openM2DB(t, dataDir)
	rows, err := db.Query(`
		SELECT 'image', sha512, width, height, hex(pdq256), pdq_quality
		FROM image_features ORDER BY sha512`)
	if err != nil {
		t.Fatal(err)
	}
	var result []string
	for rows.Next() {
		var kind, sha, pdq string
		var width, height, quality int
		if err := rows.Scan(&kind, &sha, &width, &height, &pdq, &quality); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		result = append(result, fmt.Sprintf("%s|%s|%d|%d|%s|%d",
			kind, sha, width, height, pdq, quality))
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	result = append(result, m2VideoFeatureSnapshot(t, dataDir)...)
	sort.Strings(result)
	return result
}

type m2CacheState struct {
	jpegCount    int
	sidecarCount int
	bundles      map[string]string
}

func m2CacheSnapshot(t *testing.T, cacheDir string) m2CacheState {
	t.Helper()
	state := m2CacheState{bundles: make(map[string]string)}
	err := filepath.Walk(cacheDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		switch {
		case strings.HasSuffix(path, ".jpg"):
			state.jpegCount++
		case strings.HasSuffix(path, ".jpg.json"):
			state.sidecarCount++
		default:
			return fmt.Errorf("unexpected cache file %s", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		key := strings.TrimSuffix(filepath.Base(path), ".json")
		state.bundles[key] += fmt.Sprintf("%x", sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func m2ChangedCacheBundles(before, after map[string]string) int {
	changed := 0
	keys := make(map[string]struct{}, len(before)+len(after))
	for key := range before {
		keys[key] = struct{}{}
	}
	for key := range after {
		keys[key] = struct{}{}
	}
	for key := range keys {
		if before[key] != after[key] {
			changed++
		}
	}
	return changed
}

func m2VideoFeatureSnapshot(t *testing.T, dataDir string) []string {
	t.Helper()
	rows, err := openM2DB(t, dataDir).Query(`
		SELECT sha512, duration_ms, thumb_path, hex(thumb_pdq256), thumb_quality
		FROM video_features ORDER BY sha512`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var snapshot []string
	for rows.Next() {
		var sha, path, pdq string
		var duration int64
		var quality int
		if err := rows.Scan(&sha, &duration, &path, &pdq, &quality); err != nil {
			t.Fatal(err)
		}
		snapshot = append(snapshot, fmt.Sprintf("%s|%d|%s|%s|%d",
			sha, duration, path, pdq, quality))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(snapshot)
	return snapshot
}

func m2ResetVideoState(t *testing.T, dataDir string) {
	t.Helper()
	db := openM2DB(t, dataDir)
	for _, statement := range []string{
		`DELETE FROM files`,
		`DELETE FROM video_features`,
		`DELETE FROM sync_queue WHERE table_name='video_features'`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

func m2FileSHAStats(t *testing.T, dataDir, pathPattern string) (rows, distinctSHA, done int) {
	t.Helper()
	if err := openM2DB(t, dataDir).QueryRow(`
		SELECT count(*), count(DISTINCT sha512),
		       coalesce(sum(CASE WHEN status='done' THEN 1 ELSE 0 END), 0)
		FROM files WHERE lower(path) LIKE ?`, pathPattern).Scan(&rows, &distinctSHA, &done); err != nil {
		t.Fatal(err)
	}
	return rows, distinctSHA, done
}

func m2StatusCounts(t *testing.T, dataDir string) (done, crashed int) {
	t.Helper()
	db := openM2DB(t, dataDir)
	if err := db.QueryRow(`
		SELECT
		    sum(CASE WHEN status='done' THEN 1 ELSE 0 END),
		    sum(CASE WHEN status='crash' THEN 1 ELSE 0 END)
		FROM files;`).Scan(&done, &crashed); err != nil {
		t.Fatal(err)
	}
	return done, crashed
}

func countM2LogMessage(t *testing.T, path, message string) int {
	t.Helper()
	count := 0
	for _, line := range readM2JSONLines(t, path) {
		if line["msg"] == message {
			count++
		}
	}
	return count
}

func waitM2LogCount(
	t *testing.T,
	path string,
	message string,
	want int,
	timeout time.Duration,
) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		count := countM2LogMessage(t, path, message)
		if count >= want || time.Now().After(deadline) {
			return count
		}
		time.Sleep(100 * time.Millisecond)
	}
}

type m2RSSSample struct {
	Time  time.Time `json:"time"`
	Bytes int64     `json:"bytes"`
}

var getProcessMemoryInfo = windows.NewLazySystemDLL("psapi.dll").NewProc("GetProcessMemoryInfo")

type m2ProcessMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

func sampleM2RSSLoop(pid int, stop <-chan struct{}) []m2RSSSample {
	var samples []m2RSSSample
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return samples
		default:
		}
		sample, err := queryM2RSS(pid)
		if err == nil {
			samples = append(samples, sample)
		}
		select {
		case <-stop:
			return samples
		case <-ticker.C:
		}
	}
}

func queryM2RSS(pid int) (m2RSSSample, error) {
	sampledAt := time.Now()
	handle, err := windows.OpenProcess(
		windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ,
		false,
		uint32(pid),
	)
	if err != nil {
		return m2RSSSample{}, fmt.Errorf("open Agent process for RSS: %w", err)
	}
	defer windows.CloseHandle(handle)
	counters := m2ProcessMemoryCounters{CB: uint32(unsafe.Sizeof(m2ProcessMemoryCounters{}))}
	result, _, callErr := getProcessMemoryInfo.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&counters)),
		uintptr(counters.CB),
	)
	if result == 0 {
		return m2RSSSample{}, fmt.Errorf("query Agent RSS: %w", callErr)
	}
	if counters.WorkingSetSize == 0 {
		return m2RSSSample{}, fmt.Errorf("query Agent RSS returned zero")
	}
	return m2RSSSample{Time: sampledAt, Bytes: int64(counters.WorkingSetSize)}, nil
}

func m2RSSStrictlyIncreasing(samples []m2RSSSample) bool {
	if len(samples) < 2 {
		return false
	}
	for index := 1; index < len(samples); index++ {
		if samples[index].Bytes <= samples[index-1].Bytes {
			return false
		}
	}
	return true
}

func waitM2RSSStable(t *testing.T, pid int, timeout time.Duration) []m2RSSSample {
	t.Helper()
	const stableSpread = int64(4 << 20)
	deadline := time.Now().Add(timeout)
	samples := make([]m2RSSSample, 0, 16)
	for {
		sample, err := queryM2RSS(pid)
		if err != nil {
			t.Fatalf("sample Agent RSS while waiting for stability: %v", err)
		}
		samples = append(samples, sample)
		if len(samples) >= 5 {
			tail := samples[len(samples)-5:]
			minimum, maximum := tail[0].Bytes, tail[0].Bytes
			for _, point := range tail[1:] {
				if point.Bytes < minimum {
					minimum = point.Bytes
				}
				if point.Bytes > maximum {
					maximum = point.Bytes
				}
			}
			if maximum-minimum <= stableSpread && !m2RSSStrictlyIncreasing(tail) {
				return samples
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("Agent RSS did not stabilize within %s: %#v", timeout, samples)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func m2RSSJSON(t *testing.T, samples []m2RSSSample) string {
	t.Helper()
	data, err := json.Marshal(samples)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
