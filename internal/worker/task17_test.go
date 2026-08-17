//go:build windows

package worker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"dedup/internal/store"
)

type task17Store struct {
	committed store.CommittedState
	saveErr   error
	saves     []store.AnalysisResult
}

func (s *task17Store) LookupContent(context.Context, []byte, store.MediaKind, uint32, uint8) (store.ContentState, error) {
	return store.ContentState{}, nil
}
func (s *task17Store) LookupImage(context.Context, []byte) (*store.ImageFeature, error) {
	return nil, nil
}
func (s *task17Store) LookupVideo(context.Context, []byte) (*store.VideoFeature, error) {
	return nil, nil
}
func (s *task17Store) SaveAnalysis(_ context.Context, result store.AnalysisResult) (store.CommittedState, error) {
	s.saves = append(s.saves, result)
	return s.committed, s.saveErr
}
func (s *task17Store) SavePhase1(context.Context, store.Phase1Result) error { return nil }
func (s *task17Store) SavePhase2(context.Context, store.Phase2Result) error { return nil }
func (s *task17Store) Phase2MissingMask(context.Context, string, string) (uint32, error) {
	return 0, nil
}
func (s *task17Store) MarkCrash(context.Context, string, string, string) error { return nil }

func newTask17Pool(featureStore FeatureStore) *Pool {
	return newPoolWithDeps(Config{MachineID: "machine-task17"}, featureStore, supervisorDeps{
		clock:       realClock{},
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		errorLogger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func TestPoolSavesMergedResultOnce(t *testing.T) {
	pHash, sobel := validPhase2Blobs(t)
	featureStore := &task17Store{committed: store.CommittedState{
		FieldsPresent: MaskSHA512 | MaskImagePDQ | MaskPHashParts,
		MissingFields: MaskSobelHist,
	}}
	p := newTask17Pool(featureStore)
	job := JobMsg{
		JobID: 1701, ScanTaskID: "task-17", Path: `D:\media\merged.jpg`,
		Kind: MediaImage, Phase: Phase1,
		FieldsMask: MaskSHA512 | MaskImagePDQ | MaskPHashParts | MaskSobelHist,
		Size:       123, MTimeUnix: 456,
	}
	result := JobResultMsg{
		JobID: job.JobID, Path: job.Path, Kind: job.Kind, SHA512: bytes64(0x17),
		FieldsDone: job.FieldsMask, PDQ: bytes.Repeat([]byte{1}, 32), Quality: 80,
		Width: 32, Height: 24, PHashParts: pHash, SobelHist: sobel,
	}
	p.saveResult(job, result)
	published := <-p.Results()
	if len(featureStore.saves) != 1 {
		t.Fatalf("SaveAnalysis calls=%d, want exactly 1", len(featureStore.saves))
	}
	saved := featureStore.saves[0]
	if saved.MachineID != "machine-task17" || saved.Path != job.Path || saved.Size != 123 || saved.MTime != 456 || saved.RequestedFields != job.FieldsMask {
		t.Fatalf("saved merged identity=%#v", saved)
	}
	if published.FieldsDone != MaskSHA512|MaskImagePDQ|MaskPHashParts || published.SobelHist != nil {
		t.Fatalf("committed masks did not sanitize publication: %#v", published)
	}
}

func TestPoolCommittedStateWithoutSHAClearsSHA(t *testing.T) {
	featureStore := &task17Store{committed: store.CommittedState{
		FieldsPresent: MaskImagePDQ,
		MissingFields: MaskSHA512,
	}}
	p := newTask17Pool(featureStore)
	job := JobMsg{
		JobID: 1711, Path: `D:\media\no-sha.jpg`, Kind: MediaImage,
		Phase: Phase1, FieldsMask: MaskAllImage, Size: 3, MTimeUnix: 4,
	}
	result := JobResultMsg{
		JobID: job.JobID, Path: job.Path, Kind: job.Kind,
		SHA512: bytes64(0x71), FieldsDone: MaskAllImage,
		PDQ: bytes.Repeat([]byte{7}, 32), Quality: 70, Width: 10, Height: 10,
	}
	p.saveResult(job, result)
	published := <-p.Results()
	if published.FieldsDone != MaskImagePDQ || len(published.SHA512) != 0 {
		t.Fatalf("uncommitted SHA leaked into publication: %#v", published)
	}
}

func TestPoolRollbackPublishesNoFields(t *testing.T) {
	featureStore := &task17Store{saveErr: errors.New("transaction rolled back")}
	p := newTask17Pool(featureStore)
	job := JobMsg{JobID: 1702, Path: `D:\media\rollback.jpg`, Kind: MediaImage, Phase: Phase1, FieldsMask: MaskAllImage, Size: 1, MTimeUnix: 2}
	result := JobResultMsg{JobID: job.JobID, Path: job.Path, Kind: job.Kind, SHA512: bytes64(2), FieldsDone: MaskAllImage, PDQ: bytes.Repeat([]byte{2}, 32), Quality: 70, Width: 10, Height: 10}
	p.saveResult(job, result)
	published := <-p.Results()
	if len(featureStore.saves) != 1 || published.FieldsDone != 0 || published.FramesDone != 0 || len(published.SHA512) != 0 || len(published.PDQ) != 0 {
		t.Fatalf("rollback leaked successful publication: saves=%d result=%#v", len(featureStore.saves), published)
	}
}

func TestPoolStalePublishesNothing(t *testing.T) {
	featureStore := &task17Store{saveErr: store.ErrStale}
	p := newTask17Pool(featureStore)
	job := JobMsg{JobID: 1703, Path: `D:\media\stale.mp4`, Kind: MediaVideo, Phase: Phase2, FieldsMask: MaskVideo6F, FrameMask: 1, Size: 1, MTimeMS: 3}
	result := JobResultMsg{JobID: job.JobID, Path: job.Path, Kind: job.Kind, SHA512: bytes64(3), FieldsDone: MaskVideo6F, FramesDone: 1}
	result.FrameResults[0] = FrameResult{FrameIdx: 0, TimeMS: 10, PDQ256: bytes.Repeat([]byte{3}, 32), PHashParts: []byte{1}, SobelHist: []byte{2}}
	p.saveResult(job, result)
	published := <-p.Results()
	if published.FieldsDone != 0 || published.FramesDone != 0 || len(published.SHA512) != 0 || len(published.FrameResults[0].PDQ256) != 0 || len(published.Errors) != 1 || published.Errors[0].Stage != "stale" {
		t.Fatalf("stale result was not fully sanitized: %#v", published)
	}
}

func task17Ready() ReadyMsg {
	return validReadyForTest()
}

// Break caught: the current ABI v2 Ready is rejected, or a legacy VideoCore
// v1 Ready is admitted after the ABI contract becomes mandatory.
func TestTask5VideoCoreReadyGateAcceptsV2AndRejectsV1(t *testing.T) {
	ready := task17Ready()
	if err := validateReady(ready, ready.WorkerIndex, ready.PID); err != nil {
		t.Fatalf("current VideoCore Ready rejected: %v", err)
	}
	ready.VideoCoreABI = 1
	ready.VideoCoreVersion = "1.0.0"
	if err := validateReady(ready, ready.WorkerIndex, ready.PID); err == nil {
		t.Fatal("VideoCore v1 Ready unexpectedly accepted")
	}
}

func TestPoolRejectsVideoCoreRuntimeMismatch(t *testing.T) {
	bad := task17Ready()
	bad.FFmpegComponents[0].RuntimeMajor++
	good := task17Ready()
	h := newLifecycleHarness(t,
		workerScript{ready: true, readyOverride: &bad},
		workerScript{ready: true, readyOverride: &good},
	)
	p := h.newPool(Config{WorkerCount: 1, RespawnDelay: 500 * time.Millisecond})
	p.Start()
	t.Cleanup(p.Close)
	select {
	case <-h.reaps:
	case <-time.After(2 * time.Second):
		t.Fatal("runtime-mismatched worker was not rejected and reaped")
	}
	var respawn *manualTimer
	for respawn == nil {
		timer := <-h.clock.created
		if timer.duration == 10*time.Second {
			continue
		}
		if timer.duration != 500*time.Millisecond {
			t.Fatalf("timer duration=%s, want respawn 500ms", timer.duration)
		}
		respawn = timer
	}
	respawn.fire()
	if ready := h.ready(t); ready.FFmpegComponents[0].RuntimeMajor != 63 {
		t.Fatalf("replacement Ready=%#v", ready)
	}
}

func TestPoolReplacementReadyAfterNativeCrash(t *testing.T) {
	exit := int32(-1073741819)
	ready := task17Ready()
	h := newLifecycleHarness(t,
		workerScript{ready: true, readyOverride: &ready, exitOnJob: &exit},
		workerScript{ready: true, readyOverride: &ready},
	)
	p := h.newPool(Config{WorkerCount: 1, RespawnDelay: 500 * time.Millisecond})
	p.Start()
	t.Cleanup(p.Close)
	h.ready(t)
	job := JobMsg{JobID: 1704, ScanTaskID: "task-crash", Path: `D:\media\crash.mp4`, Kind: MediaVideo, Phase: Phase1}
	if err := p.Submit(&job); err != nil {
		t.Fatal(err)
	}
	select {
	case crash := <-p.Crashes():
		if crash.JobID != job.JobID {
			t.Fatalf("crash=%#v", crash)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("native crash was not published")
	}
	h.clock.next(t, 500*time.Millisecond).fire()
	if replacement := h.ready(t); replacement.VideoCoreVersion != VideoCoreVersion {
		t.Fatalf("replacement did not pass runtime Ready gate: %#v", replacement)
	}
}

func TestValidateWorkerResultAcceptsMergedRequestedImagePayload(t *testing.T) {
	pHash, sobel := validPhase2Blobs(t)
	job := &JobMsg{JobID: 1710, Path: `D:\media\merged-result.jpg`, Kind: MediaImage, Phase: Phase1, FieldsMask: MaskSHA512 | MaskImagePDQ | MaskPHashParts | MaskSobelHist}
	result := &JobResultMsg{JobID: job.JobID, Path: job.Path, Kind: job.Kind, SHA512: bytes64(10), FieldsDone: job.FieldsMask, PDQ: bytes.Repeat([]byte{1}, 32), Quality: 80, Width: 20, Height: 10, PHashParts: pHash, SobelHist: sobel}
	if err := validateWorkerResult(job, result); err != nil {
		t.Fatalf("merged requested image payload rejected: %v", err)
	}
	result.FieldsDone &^= MaskSobelHist
	if err := validateWorkerResult(job, result); err == nil {
		t.Fatal("unclaimed Sobel payload was accepted")
	}
}

func TestValidateWorkerResultRejectsInvalidFixedFrameState(t *testing.T) {
	pHash, sobel := validPhase2Blobs(t)
	job := &JobMsg{JobID: 1711, Path: `D:\media\frames.mp4`, Kind: MediaVideo, Phase: Phase2, FieldsMask: MaskVideo6F, FrameMask: 1, KnownSHA: bytes64(11)}
	base := JobResultMsg{JobID: job.JobID, Path: job.Path, Kind: job.Kind, SHA512: bytes64(11), FieldsDone: MaskVideo6F, FramesDone: 1}
	base.FrameResults[0] = FrameResult{FrameIdx: 0, Status: 0, TimeMS: 100, PDQ256: bytes.Repeat([]byte{1}, 32), Quality: 70, PHashParts: pHash, SobelHist: sobel}
	if err := validateWorkerResult(job, &base); err != nil {
		t.Fatalf("valid fixed frame result rejected: %v", err)
	}
	bad := base
	bad.FrameResults[0].Status = -1
	if err := validateWorkerResult(job, &bad); err == nil {
		t.Fatal("successful frame with nonzero status was accepted")
	}
	bad = base
	bad.FrameResults[1].TimeMS = 200
	if err := validateWorkerResult(job, &bad); err == nil {
		t.Fatal("unrequested frame time was accepted")
	}
}

func TestValidateSHAQueryAllowsUnifiedPhase2KnownSHA(t *testing.T) {
	sha := bytes64(12)
	job := &JobMsg{JobID: 1712, Kind: MediaImage, Phase: Phase2, KnownSHA: sha}
	query := &SHAQueryMsg{JobID: job.JobID, Kind: job.Kind, SHA512: append([]byte(nil), sha...)}
	if err := validateSHAQuery(job, query); err != nil {
		t.Fatalf("unified Phase2 SHA query rejected: %v", err)
	}
	query.SHA512[0]++
	if err := validateSHAQuery(job, query); err == nil {
		t.Fatal("Phase2 SHA query mismatching KnownSHA was accepted")
	}
}
