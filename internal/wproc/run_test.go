package wproc

import (
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"dedup/internal/worker"
	"dedup/internal/wproc/videocore"
)

// Break caught: the preview dispatcher reads the real source without first
// obtaining and later reporting a parent-issued I/O lease.
func TestServeIOLeaseGovernsImagePreviewSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.jpg")
	img := image.NewNRGBA(image.Rect(0, 0, 20, 10))
	for y := range 10 {
		for x := range 20 {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 100, A: 255})
		}
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(file, img, &jpeg.Options{Quality: 90}); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha512.Sum512(data)
	job := worker.JobMsg{
		JobID: 1801, ScanTaskID: "preview-1801", ScanInstanceID: "preview-instance-1801",
		DiskKey: "preview-disk-1801", Path: path,
		Kind: worker.MediaImage, Phase: worker.PhasePreview,
		ScreenStage: worker.ScreenStagePreview, Source: worker.JobSourceLocal,
		Size: info.Size(), MTimeUnix: info.ModTime().Unix(),
		KnownSHA: append([]byte(nil), sum[:]...), PreviewFormat: worker.PreviewFormatJPEG,
		PreviewMaxWidth: 10, PreviewMaxHeight: 10, PreviewQuality: 80,
	}

	server, parent := net.Pipe()
	audit := newSourceAccessAudit(t, path)
	done := make(chan int, 1)
	go func() {
		done <- serve(server, 18, testConfig(), pipelineDeps{
			runtime:    testReadyRuntimeInfo,
			sourceOpen: audit.open,
		})
	}()
	conn := worker.NewIPCConn(parent)
	if _, err := conn.Read(); err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(worker.MsgJob, job); err != nil {
		t.Fatal(err)
	}
	var result worker.JobResultMsg
	leaseAcquires, leaseReports := 0, 0
	for {
		envelope, err := conn.Read()
		if err != nil {
			t.Fatal(err)
		}
		switch envelope.Type {
		case worker.MsgIOLeaseAcquire:
			request, err := worker.DecodeBody[worker.IOLeaseAcquireMsg](envelope)
			if err != nil {
				t.Fatal(err)
			}
			leaseAcquires++
			if request.JobID != job.JobID || request.TaskID != job.ScanTaskID ||
				request.InstanceID != job.ScanInstanceID || request.DiskKey != job.DiskKey {
				t.Fatalf("preview lease request used wrong identity: %#v", request)
			}
			grant := worker.IOLeaseGrantMsg{
				JobID: request.JobID, RequestID: request.RequestID,
				LeaseID: uint64(700 + leaseAcquires), Generation: 11,
				Bytes: request.WantBytes,
			}
			if request.WantSeek {
				grant.Seeks = 1
			}
			audit.grant(grant, request.WantSeek)
			if err := conn.Write(worker.MsgIOLeaseGrant, grant); err != nil {
				t.Fatal(err)
			}
		case worker.MsgIOLeaseReport:
			report, err := worker.DecodeBody[worker.IOLeaseReportMsg](envelope)
			if err != nil {
				t.Fatal(err)
			}
			leaseReports++
			audit.report()
			if report.JobID != job.JobID || !report.Completed || report.Cancelled {
				t.Fatalf("preview lease report = %#v", report)
			}
		case worker.MsgResult:
			result, err = worker.DecodeBody[worker.JobResultMsg](envelope)
			if err != nil {
				t.Fatal(err)
			}
			goto resultReceived
		default:
			t.Fatalf("preview message type=%q", envelope.Type)
		}
	}

resultReceived:
	audit.assertGoverned(t, true)
	if leaseAcquires == 0 || leaseReports == 0 {
		t.Fatalf("preview source lease traffic acquires=%d reports=%d, want both nonzero", leaseAcquires, leaseReports)
	}
	if result.PreviewErrorCode != "" ||
		result.PreviewWidth != 10 || result.PreviewHeight != 5 || len(result.PreviewBytes) == 0 {
		t.Fatalf("preview dispatch result = %#v", result)
	}
	if err := conn.Write(worker.MsgShutdown, struct{}{}); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 0 {
		t.Fatalf("serve exit=%d", code)
	}
}

// Break caught: the legacy Go phase-one SHA loop or phase-two image loop is
// restored to its default os.Open dependency and bypasses the Worker governor.
func TestServeIOLeaseGovernsLegacyPhaseOneAndPhaseTwoSourceReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.jpg")
	writePreviewJPEG(t, path, 80, 40, false)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha512.Sum512(data)

	tests := []struct {
		name string
		job  worker.JobMsg
	}{
		{
			name: "phase one SHA",
			job: worker.JobMsg{
				JobID: 1811, ScanTaskID: "phase-one-task", ScanInstanceID: "phase-one-instance",
				DiskKey: "phase-one-disk", Path: path, Kind: worker.MediaImage, Phase: worker.Phase1,
				FieldsMask: worker.MaskSHA512, Size: info.Size(), MTimeUnix: info.ModTime().Unix(),
			},
		},
		{
			name: "phase two image",
			job: worker.JobMsg{
				JobID: 1812, ScanTaskID: "phase-two-task", ScanInstanceID: "phase-two-instance",
				DiskKey: "phase-two-disk", Path: path, Kind: worker.MediaImage, Phase: worker.Phase2,
				FieldsMask: worker.MaskPHashParts | worker.MaskSobelHist,
				Size:       info.Size(), MTimeUnix: info.ModTime().Unix(), MTimeMS: info.ModTime().Unix(),
				KnownSHA: append([]byte(nil), sum[:]...),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, acquires, reports := serveLegacySourceWithLease(t, test.job)
			if acquires == 0 || reports == 0 {
				t.Fatalf("source lease traffic acquires=%d reports=%d result=%#v", acquires, reports, result)
			}
			if len(result.Errors) != 0 {
				t.Fatalf("legacy result errors=%#v", result.Errors)
			}
		})
	}
}

func serveLegacySourceWithLease(t *testing.T, job worker.JobMsg) (worker.JobResultMsg, int, int) {
	t.Helper()
	server, parent := net.Pipe()
	done := make(chan int, 1)
	gray := &fakePhase2Gray{}
	gray.output.PHashParts[0] = 1
	gray.output.SobelHist[0] = 1
	phase2 := defaultPhase2PipelineDeps()
	phase2.open = nil
	phase2.decode = func([]byte) (phase2GrayImage, error) { return gray, nil }
	// The partial overrides avoid native algorithm dependencies while leaving
	// source opening to serve, which must bind it to the job's lease client.
	audit := newSourceAccessAudit(t, job.Path)
	deps := pipelineDeps{
		runtime:    testReadyRuntimeInfo,
		newSHA:     func() (sha512Stream, error) { return &fakeSHA{}, nil },
		phase2:     &phase2,
		sourceOpen: audit.open,
	}
	go func() { done <- serve(server, 19, testConfig(), deps) }()
	conn := worker.NewIPCConn(parent)
	if _, err := conn.Read(); err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(worker.MsgJob, job); err != nil {
		t.Fatal(err)
	}
	acquires, reports := 0, 0
	for {
		envelope, err := conn.Read()
		if err != nil {
			t.Fatal(err)
		}
		switch envelope.Type {
		case worker.MsgIOLeaseAcquire:
			request, err := worker.DecodeBody[worker.IOLeaseAcquireMsg](envelope)
			if err != nil {
				t.Fatal(err)
			}
			acquires++
			grant := worker.IOLeaseGrantMsg{
				JobID: request.JobID, RequestID: request.RequestID,
				LeaseID: uint64(900 + acquires), Generation: 13, Bytes: request.WantBytes,
			}
			if request.WantSeek {
				grant.Seeks = 1
			}
			audit.grant(grant, request.WantSeek)
			if err := conn.Write(worker.MsgIOLeaseGrant, grant); err != nil {
				t.Fatal(err)
			}
		case worker.MsgIOLeaseReport:
			report, err := worker.DecodeBody[worker.IOLeaseReportMsg](envelope)
			if err != nil {
				t.Fatal(err)
			}
			reports++
			audit.report()
			if !report.Completed || report.Cancelled {
				t.Fatalf("legacy source report=%#v", report)
			}
		case worker.MsgResult:
			result, err := worker.DecodeBody[worker.JobResultMsg](envelope)
			if err != nil {
				t.Fatal(err)
			}
			if err := conn.Write(worker.MsgShutdown, struct{}{}); err != nil {
				t.Fatal(err)
			}
			if code := <-done; code != 0 {
				t.Fatalf("serve exit=%d", code)
			}
			audit.assertGoverned(t, false)
			return result, acquires, reports
		default:
			t.Fatalf("legacy source message type=%q", envelope.Type)
		}
	}
}

func testReadyRuntimeInfo() (videocore.RuntimeInfo, error) {
	return videocore.RuntimeInfo{ABI: videocore.ABIVersion, Version: videocore.Version, Components: [4]videocore.RuntimeComponent{
		{Name: "avformat", HeaderVersion: 63<<16 | 1<<8, RuntimeVersion: 63<<16 | 2<<8},
		{Name: "avcodec", HeaderVersion: 63<<16 | 1<<8, RuntimeVersion: 63<<16 | 2<<8},
		{Name: "avutil", HeaderVersion: 61<<16 | 1<<8, RuntimeVersion: 61<<16 | 2<<8},
		{Name: "swscale", HeaderVersion: 10<<16 | 1<<8, RuntimeVersion: 10<<16 | 2<<8},
	}}, nil
}

func missingReplyForQuery(query worker.SHAQueryMsg) worker.SHAReplyMsg {
	return worker.SHAReplyMsg{
		JobID:           query.JobID,
		RequestedFields: query.RequestedFields,
		MissingFields:   query.RequestedFields,
		RequestedFrames: query.RequestedFrames,
		MissingFrames:   query.RequestedFrames,
	}
}

// Break caught: the production default session path can appear healthy with
// synthetic dependencies while failing to open a concrete FFmpeg video codec.
func TestServeProductionSessionDecodesVideoTrackCodec(t *testing.T) {
	tests := []struct{ name, fixture, codec string }{
		{"h264 mp4", `h264-standard.mp4`, "h264"},
		{"hevc mkv", `hevc-standard.mkv`, "hevc"},
		{"vp9 webm", `vp9-portrait.webm`, "vp9"},
	}
	for index, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := productionSessionFixturePath(t, "videos", tc.fixture)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			job := worker.JobMsg{
				JobID:          int64(3100 + index),
				ScanTaskID:     "task-3-real-video-" + tc.codec,
				ScanInstanceID: "instance-task-3-real-video-" + tc.codec,
				DiskKey:        "disk-task-3-real-video",
				Path:           path,
				Kind:           worker.MediaVideo,
				Phase:          worker.Phase1,
				Source:         worker.JobSourceLocal,
				FieldsMask:     worker.MaskAllVideo,
				Size:           info.Size(),
				MTimeUnix:      info.ModTime().Unix(),
			}
			result := serveProductionSessionFixture(t, job)
			if result.FieldsDone != worker.MaskAllVideo || result.DurationMS == nil ||
				*result.DurationMS <= 0 || result.ThumbPath == "" || len(result.ThumbPDQ) != 32 ||
				result.ContactSheetWidth <= 0 || result.ContactSheetHeight <= 0 ||
				len(result.Errors) != 0 {
				t.Fatalf("%s production video result = %#v", tc.codec, result)
			}
			if info, err := os.Stat(result.ThumbPath); err != nil || !info.Mode().IsRegular() {
				t.Fatalf("%s contact sheet %q: info=%v err=%v", tc.codec, result.ThumbPath, info, err)
			}
		})
	}
}

// Break caught: serve creates a per-job lease client but the production
// VideoCore session opens its native source without that governor, so native
// hash reads/seeks emit no lease traffic before the final Go rehash.
func TestServeProductionSessionGovernsNativeSourceIO(t *testing.T) {
	path := productionSessionFixturePath(t, "videos", "h264-standard.mp4")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	job := worker.JobMsg{
		JobID: 3301, ScanTaskID: "task-5-native-governor",
		ScanInstanceID: "instance-task-5-native-governor", DiskKey: "disk-task-5-native-governor",
		Path: path, Kind: worker.MediaVideo, Phase: worker.Phase1, Source: worker.JobSourceLocal,
		FieldsMask: worker.MaskSHA512, Size: info.Size(), MTimeUnix: info.ModTime().Unix(),
	}
	result := serveProductionSessionFixture(t, job)
	if result.FieldsDone != worker.MaskSHA512 || len(result.SHA512) != sha512.Size || len(result.Errors) != 0 {
		t.Fatalf("governed native source result = %#v", result)
	}
}

// Break caught: extension-agnostic production image analysis can regress to a
// single decoder branch and silently fail JPEG, PNG, or WebP content.
func TestServeProductionSessionDecodesImageContentFormat(t *testing.T) {
	tests := []struct{ name, fixture string }{
		{"jpeg", `synthetic-pattern.jpg`},
		{"png", `synthetic-bars.png`},
		{"webp", `synthetic-portrait.webp`},
	}
	for index, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := productionSessionFixturePath(t, "images", tc.fixture)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			job := worker.JobMsg{
				JobID:          int64(3200 + index),
				ScanTaskID:     "task-3-real-image-" + tc.name,
				ScanInstanceID: "instance-task-3-real-image-" + tc.name,
				DiskKey:        "disk-task-3-real-image",
				Path:           path,
				Kind:           worker.MediaImage,
				Phase:          worker.Phase1,
				Source:         worker.JobSourceLocal,
				FieldsMask:     worker.MaskAllImage,
				Size:           info.Size(),
				MTimeUnix:      info.ModTime().Unix(),
			}
			result := serveProductionSessionFixture(t, job)
			if result.FieldsDone != worker.MaskAllImage || len(result.PDQ) != 32 ||
				result.Width <= 0 || result.Height <= 0 || len(result.Errors) != 0 {
				t.Fatalf("%s production image result = %#v", tc.name, result)
			}
		})
	}
}

func productionSessionFixturePath(t *testing.T, kind, fixture string) string {
	t.Helper()
	videoRoot := os.Getenv("VC_TESTDATA_ROOT")
	if videoRoot == "" {
		t.Fatal("VC_TESTDATA_ROOT is required for production session fixture tests")
	}
	root := videoRoot
	if kind == "images" {
		root = filepath.Join(filepath.Dir(videoRoot), "images")
	}
	path, err := filepath.Abs(filepath.Join(root, fixture))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// Break caught: a clean checkout does not contain the ignored .tmp parent, so
// the production session fixture helper failed before starting Worker IPC.
func TestProductionSessionCacheRootCreatesMissingParent(t *testing.T) {
	workspaceRoot := t.TempDir()
	cacheParent := filepath.Join(workspaceRoot, ".tmp", "task-3-real-codec-ipc")
	if _, err := os.Stat(cacheParent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("initial cache parent stat error = %v, want not exist", err)
	}

	var cacheRoot, sibling string
	if !t.Run("isolated leaf", func(t *testing.T) {
		cacheRoot = productionSessionCacheRoot(t, workspaceRoot)
		info, err := os.Stat(cacheRoot)
		if err != nil || !info.IsDir() {
			t.Fatalf("cache root info = %v, err=%v", info, err)
		}
		sibling = filepath.Join(cacheParent, "keep.txt")
		if err := os.WriteFile(sibling, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}) {
		return
	}

	if _, err := os.Stat(cacheRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache leaf stat after cleanup error = %v, want not exist", err)
	}
	if info, err := os.Stat(cacheParent); err != nil || !info.IsDir() {
		t.Fatalf("cache parent after cleanup = %v, err=%v", info, err)
	}
	if data, err := os.ReadFile(sibling); err != nil || string(data) != "keep" {
		t.Fatalf("shared sibling after cleanup = %q, err=%v", data, err)
	}
}

func productionSessionCacheRoot(t *testing.T, workspaceRoot string) string {
	t.Helper()
	cacheParent := filepath.Join(workspaceRoot, ".tmp", "task-3-real-codec-ipc")
	if err := os.MkdirAll(cacheParent, 0o755); err != nil {
		t.Fatal(err)
	}
	parentInfo, err := os.Lstat(cacheParent)
	if err != nil {
		t.Fatal(err)
	}
	if !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("production session cache parent is not a normal directory: %s", cacheParent)
	}
	cacheRoot, err := os.MkdirTemp(cacheParent, "case-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(cacheRoot); err != nil {
			t.Errorf("remove production session cache root: %v", err)
		}
	})
	return cacheRoot
}

func serveProductionSessionFixture(t *testing.T, job worker.JobMsg) worker.JobResultMsg {
	t.Helper()
	cfg, err := configFromLookup(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	videoRoot := os.Getenv("VC_TESTDATA_ROOT")
	workspaceRoot := filepath.Clean(filepath.Join(videoRoot, "..", "..", "..", ".."))
	cfg.ThumbCacheDir = productionSessionCacheRoot(t, workspaceRoot)

	server, parent := net.Pipe()
	deadline := time.Now().Add(2 * time.Minute)
	if err := server.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if err := parent.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	audit := newSourceAccessAudit(t, job.Path)
	go func() {
		defer server.Close()
		done <- serve(server, 3, cfg, pipelineDeps{
			runtime:    testReadyRuntimeInfo,
			sourceOpen: audit.open,
		})
	}()
	t.Cleanup(func() { _ = parent.Close() })
	conn := worker.NewIPCConn(parent)

	ready, err := conn.Read()
	if err != nil {
		t.Fatal(err)
	}
	if ready.Type != worker.MsgReady {
		t.Fatalf("production session first message = %q, want %q", ready.Type, worker.MsgReady)
	}
	if err := conn.Write(worker.MsgJob, job); err != nil {
		t.Fatal(err)
	}
	leaseStats := productionLeaseStats{}
	var queryEnvelope *worker.Envelope
	for queryEnvelope == nil {
		envelope, readErr := conn.Read()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if handleProductionLeaseMessage(t, conn, envelope, job, audit, &leaseStats) {
			continue
		}
		if envelope.Type != worker.MsgSHAQuery {
			t.Fatalf("production session post-job message = %q, want lease traffic or %q", envelope.Type, worker.MsgSHAQuery)
		}
		queryEnvelope = envelope
	}
	query, err := worker.DecodeBody[worker.SHAQueryMsg](queryEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	if query.JobID != job.JobID || query.Kind != job.Kind || len(query.SHA512) != sha512.Size ||
		query.RequestedFields != job.FieldsMask&^worker.MaskSHA512 {
		t.Fatalf("production session SHA query = %#v", query)
	}
	if err := conn.Write(worker.MsgSHAReply, missingReplyForQuery(query)); err != nil {
		t.Fatal(err)
	}
	var result worker.JobResultMsg
	for {
		resultEnvelope, err := conn.Read()
		if err != nil {
			select {
			case code := <-done:
				t.Fatalf("production session result read: %v; serve exit=%d", err, code)
			default:
				t.Fatal(err)
			}
		}
		if handleProductionLeaseMessage(t, conn, resultEnvelope, job, audit, &leaseStats) {
			continue
		}
		switch resultEnvelope.Type {
		case worker.MsgResult:
			result, err = worker.DecodeBody[worker.JobResultMsg](resultEnvelope)
			if err != nil {
				t.Fatal(err)
			}
			goto productionResultReceived
		default:
			t.Fatalf("production session post-reply message = %q", resultEnvelope.Type)
		}
	}

productionResultReceived:
	audit.assertGoverned(t, false)
	if leaseStats.seekAcquires == 0 || leaseStats.reportedSeeks == 0 {
		t.Fatalf("native source lease traffic acquires=%d seek_acquires=%d reports=%d reported_seeks=%d",
			leaseStats.acquires, leaseStats.seekAcquires, leaseStats.reports, leaseStats.reportedSeeks)
	}
	if leaseStats.reportedBytes < job.Size {
		t.Fatalf("source lease reported bytes=%d, want at least job size %d", leaseStats.reportedBytes, job.Size)
	}
	if err := conn.Write(worker.MsgShutdown, struct{}{}); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 0 {
		t.Fatalf("production session shutdown exit = %d, want 0", code)
	}
	t.Logf("IPC chain: %s -> %s -> %s -> %s -> %s -> %s (serve=0)",
		worker.MsgReady, worker.MsgJob, worker.MsgSHAQuery, worker.MsgSHAReply, worker.MsgResult, worker.MsgShutdown)
	return result
}

type productionLeaseStats struct {
	acquires      int
	seekAcquires  int
	reports       int
	reportedBytes int64
	reportedSeeks uint32
}

func handleProductionLeaseMessage(
	t *testing.T,
	conn *worker.IPCConn,
	envelope *worker.Envelope,
	job worker.JobMsg,
	audit *sourceAccessAudit,
	stats *productionLeaseStats,
) bool {
	t.Helper()
	switch envelope.Type {
	case worker.MsgIOLeaseAcquire:
		request, err := worker.DecodeBody[worker.IOLeaseAcquireMsg](envelope)
		if err != nil {
			t.Fatal(err)
		}
		stats.acquires++
		if request.JobID != job.JobID || request.TaskID != job.ScanTaskID ||
			request.InstanceID != job.ScanInstanceID || request.DiskKey != job.DiskKey {
			t.Fatalf("production source lease request = %#v", request)
		}
		grant := worker.IOLeaseGrantMsg{
			JobID: request.JobID, RequestID: request.RequestID,
			LeaseID: uint64(800 + stats.acquires), Generation: 12,
			Bytes: request.WantBytes,
		}
		if request.WantSeek {
			stats.seekAcquires++
			grant.Seeks = 1
		}
		audit.grant(grant, request.WantSeek)
		if err := conn.Write(worker.MsgIOLeaseGrant, grant); err != nil {
			t.Fatal(err)
		}
		return true
	case worker.MsgIOLeaseReport:
		report, err := worker.DecodeBody[worker.IOLeaseReportMsg](envelope)
		if err != nil {
			t.Fatal(err)
		}
		stats.reports++
		stats.reportedBytes += report.Bytes
		stats.reportedSeeks += report.Seeks
		audit.report()
		if report.JobID != job.JobID || report.TaskID != job.ScanTaskID ||
			report.InstanceID != job.ScanInstanceID || report.DiskKey != job.DiskKey ||
			!report.Completed || report.Cancelled {
			t.Fatalf("production source lease report = %#v", report)
		}
		return true
	default:
		return false
	}
}

// sourceAccessAudit is the production-opener seam: a grant is recorded before
// it is sent to Worker, and every real source handle operation verifies that an
// outstanding byte/seek authorization already exists. Only the immutable job
// path may pass this seam; cache, temp contact sheets, sidecars and logs must not.
type sourceAccessAudit struct {
	mu         sync.Mutex
	wantPath   string
	readTokens int64
	seekTokens int64
	reads      int
	seeks      int
	opens      []string
	violations []string
}

func newSourceAccessAudit(t *testing.T, path string) *sourceAccessAudit {
	t.Helper()
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return &sourceAccessAudit{wantPath: filepath.Clean(absolute)}
}

func (audit *sourceAccessAudit) grant(grant worker.IOLeaseGrantMsg, seek bool) {
	audit.mu.Lock()
	defer audit.mu.Unlock()
	if seek {
		audit.seekTokens += int64(grant.Seeks)
	} else {
		audit.readTokens += grant.Bytes
	}
}

func (audit *sourceAccessAudit) report() {
	audit.mu.Lock()
	defer audit.mu.Unlock()
	audit.readTokens = 0
	audit.seekTokens = 0
}

func (audit *sourceAccessAudit) open(path string) (sourceFileHandle, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	absolute = filepath.Clean(absolute)
	audit.mu.Lock()
	audit.opens = append(audit.opens, absolute)
	if !strings.EqualFold(absolute, audit.wantPath) {
		audit.violations = append(audit.violations, "non-source path entered governed opener: "+absolute)
	}
	audit.mu.Unlock()
	file, err := os.Open(absolute)
	if err != nil {
		return nil, err
	}
	return &auditedSourceFile{sourceFileHandle: file, path: absolute, audit: audit}, nil
}

func (audit *sourceAccessAudit) beforeRead(path string) {
	audit.mu.Lock()
	defer audit.mu.Unlock()
	if audit.readTokens <= 0 {
		audit.violations = append(audit.violations, "Read before grant: "+path)
	}
	audit.reads++
}

func (audit *sourceAccessAudit) afterRead(read int) {
	audit.mu.Lock()
	defer audit.mu.Unlock()
	audit.readTokens -= int64(read)
}

func (audit *sourceAccessAudit) beforeSeek(path string) {
	audit.mu.Lock()
	defer audit.mu.Unlock()
	if audit.seekTokens <= 0 {
		audit.violations = append(audit.violations, "Seek before grant: "+path)
	} else {
		audit.seekTokens--
	}
	audit.seeks++
}

func (audit *sourceAccessAudit) assertGoverned(t *testing.T, wantSeek bool) {
	t.Helper()
	audit.mu.Lock()
	defer audit.mu.Unlock()
	if len(audit.violations) != 0 {
		t.Fatalf("source operation audit violations: %v", audit.violations)
	}
	if audit.reads == 0 || len(audit.opens) == 0 {
		t.Fatalf("source audit opens=%v reads=%d, want both nonzero", audit.opens, audit.reads)
	}
	if wantSeek && audit.seeks == 0 {
		t.Fatal("source audit recorded no governed Seek")
	}
	for _, opened := range audit.opens {
		if !strings.EqualFold(opened, audit.wantPath) {
			t.Fatalf("cache/temp/sidecar/log path entered source governor: %s", opened)
		}
	}
}

type auditedSourceFile struct {
	sourceFileHandle
	path  string
	audit *sourceAccessAudit
}

func (file *auditedSourceFile) Read(buffer []byte) (int, error) {
	file.audit.beforeRead(file.path)
	read, err := file.sourceFileHandle.Read(buffer)
	file.audit.afterRead(read)
	return read, err
}

func (file *auditedSourceFile) Seek(offset int64, whence int) (int64, error) {
	file.audit.beforeSeek(file.path)
	return file.sourceFileHandle.Seek(offset, whence)
}

func TestServeReadyReportsVideoCoreRuntime(t *testing.T) {
	server, parent := net.Pipe()
	done := make(chan int, 1)
	deps := pipelineDeps{runtime: testReadyRuntimeInfo}
	go func() { done <- serve(server, 17, testConfig(), deps) }()
	conn := worker.NewIPCConn(parent)
	envelope, err := conn.Read()
	if err != nil {
		t.Fatal(err)
	}
	ready, err := worker.DecodeBody[worker.ReadyMsg](envelope)
	if err != nil {
		t.Fatal(err)
	}
	if ready.DLLVersion != worker.MediaCoreDLLVersion || ready.VideoCoreABI != videocore.ABIVersion || ready.VideoCoreVersion != videocore.Version || len(ready.FFmpegComponents) != 4 || ready.FFmpegComponents[0].BuildMajor != 63 || ready.FFmpegComponents[0].RuntimeMajor != 63 {
		t.Fatalf("runtime Ready=%#v", ready)
	}
	if err := conn.Write(worker.MsgShutdown, struct{}{}); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 0 {
		t.Fatalf("serve exit=%d", code)
	}
}

func TestServeRuntimeErrorDoesNotReady(t *testing.T) {
	server, parent := net.Pipe()
	done := make(chan int, 1)
	go func() {
		done <- serve(server, 18, testConfig(), pipelineDeps{runtime: func() (videocore.RuntimeInfo, error) {
			return videocore.RuntimeInfo{}, errors.New("runtime unavailable")
		}})
	}()
	if code := <-done; code != 2 {
		t.Fatalf("serve runtime error exit=%d, want 2", code)
	}
	_ = parent.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if envelope, err := worker.NewIPCConn(parent).Read(); err == nil {
		t.Fatalf("runtime error emitted Ready %#v", envelope)
	}
}

func TestServeDispatchesPhase2ThroughSessionPipeline(t *testing.T) {
	job, sessionDeps, fake := newSessionPipelineTest(t, worker.MediaImage,
		worker.MaskSHA512|worker.MaskImagePDQ, 0)
	sessionDeps.query = nil

	server, parent := net.Pipe()
	done := make(chan int, 1)
	go func() {
		done <- serve(server, 12, sessionPipelineTestConfig(), pipelineDeps{session: &sessionDeps})
	}()
	conn := worker.NewIPCConn(parent)
	if _, err := conn.Read(); err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(worker.MsgJob, *job); err != nil {
		t.Fatal(err)
	}
	envelope, err := conn.Read()
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Type != worker.MsgSHAQuery {
		t.Fatalf("first phase-2 response = %q, want SHA query", envelope.Type)
	}
	query, err := worker.DecodeBody[worker.SHAQueryMsg](envelope)
	if err != nil {
		t.Fatal(err)
	}
	if query.JobID != job.JobID || query.RequestedFields != worker.MaskImagePDQ {
		t.Fatalf("phase-2 query = %#v", query)
	}
	if err := conn.Write(worker.MsgSHAReply, worker.SHAReplyMsg{
		JobID: job.JobID, Found: true, RequestedFields: worker.MaskImagePDQ,
		FieldsPresent: worker.MaskImagePDQ, PDQ: make([]byte, 32), Quality: 80, Width: 20, Height: 10,
	}); err != nil {
		t.Fatal(err)
	}
	envelope, err = conn.Read()
	if err != nil {
		t.Fatal(err)
	}
	result, err := worker.DecodeBody[worker.JobResultMsg](envelope)
	if err != nil {
		t.Fatal(err)
	}
	// The final identity guard uses the independent rehash dependency rather
	// than the native session's cached Hash result.
	if result.JobID != job.JobID || result.Kind != worker.MediaImage || len(result.SHA512) != sha512.Size || fake.opens != 1 || fake.hashes != 1 || fake.rehashes != 1 || fake.analyzes != 0 || fake.closes != 1 {
		t.Fatalf("phase-2 result/session = %#v; %d/%d/%d/%d", result, fake.opens, fake.hashes, fake.analyzes, fake.closes)
	}
	if err := conn.Write(worker.MsgShutdown, struct{}{}); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 0 {
		t.Fatalf("serve shutdown exit = %d, want 0", code)
	}
}

// Break caught: the default Windows Worker used the legacy MediaCore image
// pipeline for phase one even though the production build only ships VideoCore.
func TestImagePhaseOneServeUsesSessionPipeline(t *testing.T) {
	job, sessionDeps, sessionFake := newSessionPipelineTest(
		t, worker.MediaImage, worker.MaskAllImage, 0,
	)
	job.Phase = worker.Phase1
	job.KnownSHA = nil
	sessionDeps.query = nil
	sessionFake.result = videocore.AnalysisResult{
		MediaType:          1,
		ImageStatus:        videocore.StatusOK,
		ImageWidth:         640,
		ImageHeight:        360,
		ContactSheetWidth:  640,
		ContactSheetHeight: 360,
		ImageFeatures: videocore.FeatureSet{
			PDQ: [32]byte{7}, PDQQuality: 88,
		},
	}
	file := newFakeFile(make([]byte, int(job.Size)), job.Size, job.MTimeUnix)
	imageDeps, imageState := testPipelineDeps(file)
	legacyHashes := 0
	imageDeps.newSHA = func() (sha512Stream, error) {
		legacyHashes++
		return &fakeSHA{}, nil
	}
	imageDeps.runtime = testReadyRuntimeInfo
	imageDeps.query = nil
	imageDeps.session = &sessionDeps

	server, parent := net.Pipe()
	done := make(chan int, 1)
	go func() { done <- serve(server, 14, sessionPipelineTestConfig(), imageDeps) }()
	conn := worker.NewIPCConn(parent)
	if _, err := conn.Read(); err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(worker.MsgJob, *job); err != nil {
		t.Fatal(err)
	}
	envelope, err := conn.Read()
	if err != nil {
		t.Fatal(err)
	}
	query, err := worker.DecodeBody[worker.SHAQueryMsg](envelope)
	if err != nil || envelope.Type != worker.MsgSHAQuery {
		t.Fatalf("image query = type %q %#v err=%v", envelope.Type, query, err)
	}
	if err := conn.Write(worker.MsgSHAReply, worker.SHAReplyMsg{
		JobID: query.JobID, Found: false,
		RequestedFields: query.RequestedFields,
		MissingFields:   worker.MaskImagePDQ,
	}); err != nil {
		t.Fatal(err)
	}
	envelope, err = conn.Read()
	if err != nil {
		t.Fatal(err)
	}
	result, err := worker.DecodeBody[worker.JobResultMsg](envelope)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Type != worker.MsgResult || result.FieldsDone != worker.MaskAllImage ||
		len(result.PDQ) != 32 || result.Width != 640 || result.Height != 360 ||
		len(result.Errors) != 0 {
		t.Fatalf("image result = type %q %#v", envelope.Type, result)
	}
	if legacyHashes != 0 || imageState.decodeCalls != 0 ||
		sessionFake.opens != 1 || sessionFake.hashes != 1 || sessionFake.analyzes != 1 ||
		sessionFake.rehashes != 1 || sessionFake.closes != 1 {
		t.Fatalf("legacy/session calls = hash/decode:%d/%d open/hash/analyze/rehash/close:%d/%d/%d/%d/%d",
			legacyHashes, imageState.decodeCalls, sessionFake.opens, sessionFake.hashes,
			sessionFake.analyzes, sessionFake.rehashes, sessionFake.closes)
	}
	if err := conn.Write(worker.MsgShutdown, struct{}{}); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 0 {
		t.Fatalf("serve exit = %d", code)
	}
}

func TestServeInvalidPhaseOrKindReturnsFileLevelResult(t *testing.T) {
	tests := []struct {
		name  string
		job   worker.JobMsg
		stage string
	}{
		{
			name: "phase",
			job: worker.JobMsg{
				JobID: 1202, Path: `C:\media\phase.jpg`, Kind: worker.MediaImage,
				Phase: 99, FieldsMask: worker.MaskPHashParts,
			},
			stage: "phase",
		},
		{
			name: "kind",
			job: worker.JobMsg{
				JobID: 1203, Path: `C:\media\kind.dat`, Kind: 99,
				Phase: worker.Phase2, FieldsMask: worker.MaskPHashParts,
				KnownSHA: make([]byte, sha512.Size),
			},
			stage: "kind",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Phase2FrameTimeout = 20 * time.Second
			cfg.Phase2FrameMaxSide = 512
			server, parent := net.Pipe()
			done := make(chan int, 1)
			go func() { done <- serve(server, 13, cfg, pipelineDeps{runtime: testReadyRuntimeInfo}) }()
			conn := worker.NewIPCConn(parent)
			if _, err := conn.Read(); err != nil {
				t.Fatal(err)
			}
			if err := conn.Write(worker.MsgJob, tc.job); err != nil {
				t.Fatal(err)
			}
			envelope, err := conn.Read()
			if err != nil {
				t.Fatal(err)
			}
			result, err := worker.DecodeBody[worker.JobResultMsg](envelope)
			if err != nil {
				t.Fatal(err)
			}
			if envelope.Type != worker.MsgResult || len(result.Errors) != 1 ||
				result.Errors[0].Field != 0 || result.Errors[0].Stage != tc.stage {
				t.Fatalf("invalid dispatch result = type %q %#v", envelope.Type, result)
			}
			if err := conn.Write(worker.MsgShutdown, struct{}{}); err != nil {
				t.Fatal(err)
			}
			if code := <-done; code != 0 {
				t.Fatalf("serve exit = %d", code)
			}
		})
	}
}

func TestServeCacheHitQueriesSHAOverIPCBeforeResult(t *testing.T) {
	file := newFakeFile([]byte("pixels"), 6, 123)
	deps, state := testPipelineDeps(file)
	deps.runtime = testReadyRuntimeInfo
	deps.query = nil
	server, parent := net.Pipe()
	done := make(chan int, 1)
	go func() {
		done <- serve(server, 9, testConfig(), deps)
	}()
	conn := worker.NewIPCConn(parent)
	if _, err := conn.Read(); err != nil {
		t.Fatal(err)
	}
	job := worker.JobMsg{
		JobID: 91, Path: `C:\media\cached.jpg`, Kind: worker.MediaImage,
		Phase: worker.Phase1, FieldsMask: worker.MaskAllImage,
		Size: 6, MTimeUnix: 123,
	}
	if err := conn.Write(worker.MsgJob, job); err != nil {
		t.Fatal(err)
	}

	envelope, err := conn.Read()
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Type != worker.MsgSHAQuery {
		t.Fatalf("first post-job message = %q, want %q before any result", envelope.Type, worker.MsgSHAQuery)
	}
	query, err := worker.DecodeBody[worker.SHAQueryMsg](envelope)
	if err != nil {
		t.Fatal(err)
	}
	if query.JobID != job.JobID || len(query.SHA512) != 64 {
		t.Fatalf("SHA query = %#v", query)
	}
	if err := conn.Write(worker.MsgSHAReply, worker.SHAReplyMsg{
		JobID: query.JobID, Found: true, PDQ: make([]byte, 32),
		Quality: 91, Width: 40, Height: 30,
	}); err != nil {
		t.Fatal(err)
	}
	envelope, err = conn.Read()
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Type != worker.MsgResult {
		t.Fatalf("message after SHA reply = %q, want %q", envelope.Type, worker.MsgResult)
	}
	result, err := worker.DecodeBody[worker.JobResultMsg](envelope)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decoded || state.decodeCalls != 0 {
		t.Fatalf("cache hit decoded: result=%v decode calls=%d", result.Decoded, state.decodeCalls)
	}
	if result.Quality != 91 || result.FieldsDone != worker.MaskAllImage {
		t.Fatalf("cache-hit result = %#v", result)
	}
	if err := conn.Write(worker.MsgShutdown, struct{}{}); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 0 {
		t.Fatalf("serve shutdown exit = %d, want 0", code)
	}
}

func TestServeRoutesVideoJobsThroughVideoPipeline(t *testing.T) {
	file := newFakeFile([]byte("video"), 5, 123)
	videoDeps, state := testVideoPipelineDeps(file)
	videoDeps.query = nil
	deps := pipelineDeps{video: &videoDeps, runtime: testReadyRuntimeInfo}
	server, parent := net.Pipe()
	done := make(chan int, 1)
	go func() {
		done <- serve(server, 11, testVideoConfig(t.TempDir()), deps)
	}()
	conn := worker.NewIPCConn(parent)
	if _, err := conn.Read(); err != nil {
		t.Fatal(err)
	}
	job := *testVideoJob(711)
	job.FieldsMask = legacyPhase1VideoMask
	if err := conn.Write(worker.MsgJob, job); err != nil {
		t.Fatal(err)
	}
	envelope, err := conn.Read()
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Type != worker.MsgSHAQuery {
		t.Fatalf("first video response = %q, want SHA query", envelope.Type)
	}
	query, err := worker.DecodeBody[worker.SHAQueryMsg](envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(worker.MsgSHAReply, worker.SHAReplyMsg{JobID: query.JobID}); err != nil {
		t.Fatal(err)
	}
	envelope, err = conn.Read()
	if err != nil {
		t.Fatal(err)
	}
	result, err := worker.DecodeBody[worker.JobResultMsg](envelope)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Type != worker.MsgResult || result.Kind != worker.MediaVideo || result.FieldsDone != legacyPhase1VideoMask {
		t.Fatalf("video result = type %q body %#v", envelope.Type, result)
	}
	if got := strings.Join(state.events, ","); got != "probe,cache,ffmpeg,thumb-read,thumb-pdq" {
		t.Fatalf("video worker events after IPC query = %q", got)
	}
	if err := conn.Write(worker.MsgShutdown, struct{}{}); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 0 {
		t.Fatalf("serve shutdown exit = %d, want 0", code)
	}
}

func TestServeRejectsIncompleteCacheReply(t *testing.T) {
	tests := []struct {
		name  string
		reply worker.SHAReplyMsg
	}{
		{
			name:  "short PDQ",
			reply: worker.SHAReplyMsg{Found: true, PDQ: []byte{1}, Quality: 80, Width: 40, Height: 30},
		},
		{
			name:  "invalid dimensions",
			reply: worker.SHAReplyMsg{Found: true, PDQ: make([]byte, 32), Quality: 80, Width: 0, Height: 30},
		},
		{
			name:  "invalid quality",
			reply: worker.SHAReplyMsg{Found: true, PDQ: make([]byte, 32), Quality: 101, Width: 40, Height: 30},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			file := newFakeFile([]byte("pixels"), 6, 123)
			deps, state := testPipelineDeps(file)
			deps.runtime = testReadyRuntimeInfo
			deps.query = nil
			server, parent := net.Pipe()
			deadline := time.Now().Add(2 * time.Second)
			if err := server.SetDeadline(deadline); err != nil {
				t.Fatal(err)
			}
			if err := parent.SetDeadline(deadline); err != nil {
				t.Fatal(err)
			}
			done := make(chan int, 1)
			go func() {
				defer server.Close()
				done <- serve(server, 10, testConfig(), deps)
			}()
			conn := worker.NewIPCConn(parent)
			defer parent.Close()
			if _, err := conn.Read(); err != nil {
				t.Fatal(err)
			}
			job := worker.JobMsg{
				JobID: 101, Path: `C:\media\invalid-cache.jpg`, Kind: worker.MediaImage,
				Phase: worker.Phase1, FieldsMask: worker.MaskAllImage,
				Size: 6, MTimeUnix: 123,
			}
			if err := conn.Write(worker.MsgJob, job); err != nil {
				t.Fatal(err)
			}
			envelope, err := conn.Read()
			if err != nil {
				t.Fatal(err)
			}
			query, err := worker.DecodeBody[worker.SHAQueryMsg](envelope)
			if err != nil {
				t.Fatal(err)
			}
			tc.reply.JobID = query.JobID
			type readOutcome struct {
				envelope *worker.Envelope
				err      error
			}
			readDone := make(chan readOutcome, 1)
			go func() {
				next, readErr := conn.Read()
				readDone <- readOutcome{envelope: next, err: readErr}
			}()
			if err := conn.Write(worker.MsgSHAReply, tc.reply); err != nil {
				t.Fatal(err)
			}
			if code := <-done; code != 2 {
				t.Fatalf("serve invalid cache reply exit = %d, want fatal 2", code)
			}
			outcome := <-readDone
			if outcome.err == nil {
				t.Fatalf("worker emitted envelope %#v after incompatible cache reply; want connection close with no result", outcome.envelope)
			}
			if outcome.envelope != nil {
				t.Fatalf("worker emitted envelope %#v with read error %v; want no result", outcome.envelope, outcome.err)
			}
			if state.decodeCalls != 0 {
				t.Fatalf("decode calls = %d after incompatible cache reply, want 0", state.decodeCalls)
			}
		})
	}
}

func TestServeSendsReadyAndHandlesShutdown(t *testing.T) {
	server, parent := net.Pipe()
	done := make(chan int, 1)
	go func() {
		done <- serve(server, 7, testConfig(), pipelineDeps{runtime: testReadyRuntimeInfo})
	}()
	conn := worker.NewIPCConn(parent)
	env, err := conn.Read()
	if err != nil {
		t.Fatal(err)
	}
	ready, err := worker.DecodeBody[worker.ReadyMsg](env)
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != worker.MsgReady || ready.WorkerIndex != 7 ||
		ready.IPCVersion != worker.IPCCompatibilityVersion ||
		ready.DLLVersion != worker.MediaCoreDLLVersion || ready.VideoCoreABI != videocore.ABIVersion {
		t.Fatalf("ready = type %q body %#v", env.Type, ready)
	}
	if err := conn.Write(worker.MsgShutdown, struct{}{}); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 0 {
		t.Fatalf("serve shutdown exit = %d, want 0", code)
	}
}

func TestServeTreatsCleanParentEOFAsNormalExit(t *testing.T) {
	server, parent := net.Pipe()
	done := make(chan int, 1)
	go func() {
		done <- serve(server, 0, testConfig(), pipelineDeps{runtime: testReadyRuntimeInfo})
	}()
	conn := worker.NewIPCConn(parent)
	if _, err := conn.Read(); err != nil {
		t.Fatal(err)
	}
	_ = parent.Close()
	if code := <-done; code != 0 {
		t.Fatalf("serve parent EOF exit = %d, want 0", code)
	}
}

func TestServeRejectsTruncatedAndIncompatibleFrames(t *testing.T) {
	t.Run("truncated header", func(t *testing.T) {
		server, parent := net.Pipe()
		done := make(chan int, 1)
		go func() { done <- serve(server, 0, testConfig(), pipelineDeps{runtime: testReadyRuntimeInfo}) }()
		conn := worker.NewIPCConn(parent)
		if _, err := conn.Read(); err != nil {
			t.Fatal(err)
		}
		if _, err := parent.Write([]byte{0, 0}); err != nil {
			t.Fatal(err)
		}
		_ = parent.Close()
		if code := <-done; code != 2 {
			t.Fatalf("serve truncated frame exit = %d, want 2", code)
		}
	})

	t.Run("incompatible envelope", func(t *testing.T) {
		server, parent := net.Pipe()
		done := make(chan int, 1)
		go func() { done <- serve(server, 0, testConfig(), pipelineDeps{runtime: testReadyRuntimeInfo}) }()
		conn := worker.NewIPCConn(parent)
		if _, err := conn.Read(); err != nil {
			t.Fatal(err)
		}
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], 1)
		if _, err := parent.Write(append(header[:], 0xc1)); err != nil {
			t.Fatal(err)
		}
		if code := <-done; code != 2 {
			t.Fatalf("serve incompatible frame exit = %d, want 2", code)
		}
	})
}
