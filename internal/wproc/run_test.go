package wproc

import (
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"dedup/internal/worker"
	"dedup/internal/wproc/videocore"
)

func testReadyRuntimeInfo() (videocore.RuntimeInfo, error) {
	return videocore.RuntimeInfo{ABI: videocore.ABIVersion, Version: "1.0.0", Components: [4]videocore.RuntimeComponent{
		{Name: "avformat", HeaderVersion: 63<<16 | 1<<8, RuntimeVersion: 63<<16 | 2<<8},
		{Name: "avcodec", HeaderVersion: 63<<16 | 1<<8, RuntimeVersion: 63<<16 | 2<<8},
		{Name: "avutil", HeaderVersion: 61<<16 | 1<<8, RuntimeVersion: 61<<16 | 2<<8},
		{Name: "swscale", HeaderVersion: 10<<16 | 1<<8, RuntimeVersion: 10<<16 | 2<<8},
	}}, nil
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
	if ready.VideoCoreABI != 1 || ready.VideoCoreVersion != "1.0.0" || len(ready.FFmpegComponents) != 4 || ready.FFmpegComponents[0].BuildMajor != 63 || ready.FFmpegComponents[0].RuntimeMajor != 63 {
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
	if result.JobID != job.JobID || result.Kind != worker.MediaImage || len(result.SHA512) != sha512.Size || fake.opens != 1 || fake.hashes != 1 || fake.analyzes != 0 || fake.closes != 1 {
		t.Fatalf("phase-2 result/session = %#v; %d/%d/%d/%d", result, fake.opens, fake.hashes, fake.analyzes, fake.closes)
	}
	if err := conn.Write(worker.MsgShutdown, struct{}{}); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 0 {
		t.Fatalf("serve shutdown exit = %d, want 0", code)
	}
}

func TestImageNoThumbnailServeUsesImagePipelineEvenWithSessionConfigured(t *testing.T) {
	file := newFakeFile([]byte("pixels"), 6, 123)
	imageDeps, imageState := testPipelineDeps(file)
	imageDeps.runtime = testReadyRuntimeInfo
	imageDeps.query = nil
	cacheDir := t.TempDir()
	cfg := testConfig()
	cfg.ThumbCacheDir = cacheDir
	_, sessionDeps, sessionFake := newSessionPipelineTest(
		t, worker.MediaImage, worker.MaskAllImage, 0,
	)
	sessionDeps.query = nil
	imageDeps.session = &sessionDeps

	server, parent := net.Pipe()
	done := make(chan int, 1)
	go func() { done <- serve(server, 14, cfg, imageDeps) }()
	conn := worker.NewIPCConn(parent)
	if _, err := conn.Read(); err != nil {
		t.Fatal(err)
	}
	job := worker.JobMsg{
		JobID: 1401, Path: `C:\media\no-thumb.jpg`, Kind: worker.MediaImage,
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
	if err != nil || envelope.Type != worker.MsgSHAQuery {
		t.Fatalf("image query = type %q %#v err=%v", envelope.Type, query, err)
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
	if envelope.Type != worker.MsgResult || result.FieldsDone != worker.MaskAllImage ||
		len(result.PDQ) != 32 || result.Width <= 0 || result.Height <= 0 ||
		result.ThumbPath != "" || result.ThumbGenerated || result.ThumbCacheHit {
		t.Fatalf("image result = type %q %#v", envelope.Type, result)
	}
	if imageState.decodeCalls != 1 || sessionFake.opens != 0 || sessionFake.analyzes != 0 ||
		sessionFake.closes != 0 {
		t.Fatalf("image/session calls = decode:%d open/analyze/close:%d/%d/%d",
			imageState.decodeCalls, sessionFake.opens, sessionFake.analyzes, sessionFake.closes)
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("image pipeline created thumbnail cache entries: %#v", entries)
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
		ready.DLLVersion != "1.0.0" || ready.VideoCoreABI != videocore.ABIVersion {
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
