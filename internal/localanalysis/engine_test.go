package localanalysis

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"dedup/internal/config"
	"dedup/internal/features"
	"dedup/internal/firstscreen"
	"dedup/internal/store"
	"dedup/internal/worker"
)

type engineStageOne struct {
	result firstscreen.Result
	err    error
}

func (s engineStageOne) Run(context.Context, string, string) (firstscreen.Result, error) {
	return s.result, s.err
}

type engineStore struct {
	run     store.LocalAnalysisRun
	current store.LocalAnalysisRun
	calls   []string
	pairs   []store.LocalPairScore
	groups  []store.LocalAnalysisGroup
	events  []store.LocalOutboxEvent
	fail    map[string]error
}

func (s *engineStore) BeginLocalAnalysis(context.Context, string, string) (store.LocalAnalysisRun, error) {
	s.calls = append(s.calls, "begin")
	return s.run, s.fail["begin"]
}
func (s *engineStore) CurrentLocalAnalysis(context.Context, string) (store.LocalAnalysisRun, error) {
	s.calls = append(s.calls, "current")
	return s.current, s.fail["current"]
}
func (s *engineStore) SaveLocalPairScore(_ context.Context, p store.LocalPairScore) error {
	s.calls = append(s.calls, "pair")
	s.pairs = append(s.pairs, p)
	return s.fail["pair"]
}
func (s *engineStore) ReplaceLocalAnalysisGroups(_ context.Context, _ string, g []store.LocalAnalysisGroup) error {
	s.calls = append(s.calls, "groups")
	s.groups = g
	return s.fail["groups"]
}
func (s *engineStore) CompleteLocalAnalysis(context.Context, string) error {
	s.calls = append(s.calls, "complete")
	return s.fail["complete"]
}
func (s *engineStore) PublishLocalAnalysis(context.Context, string) error {
	s.calls = append(s.calls, "publish")
	return s.fail["publish"]
}
func (s *engineStore) EnqueueLocalEvent(_ context.Context, event store.LocalOutboxEvent) error {
	s.calls = append(s.calls, "outbox")
	s.events = append(s.events, event)
	return s.fail["outbox"]
}

type engineWorker struct {
	jobs       []*worker.JobMsg
	makeResult func(*worker.JobMsg) *worker.JobResultMsg
	fail       func(*worker.JobMsg, int) error
}

type engineWorkerFunc func(context.Context, *worker.JobMsg) (*worker.JobResultMsg, error)

func (execute engineWorkerFunc) Execute(ctx context.Context, job *worker.JobMsg) (*worker.JobResultMsg, error) {
	return execute(ctx, job)
}

func (w *engineWorker) Execute(_ context.Context, job *worker.JobMsg) (*worker.JobResultMsg, error) {
	w.jobs = append(w.jobs, job)
	job.JobID = int64(len(w.jobs))
	if w.fail != nil {
		if err := w.fail(job, len(w.jobs)); err != nil {
			return nil, err
		}
	}
	return w.makeResult(job), nil
}

func TestEngineRecoveryConvergesCompleteAndPublishedRunsWithoutRecomputing(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	createRun := func(taskID string) store.LocalAnalysisRun {
		t.Helper()
		if _, err := db.CreateOrLoadLocalTask(context.Background(), store.LocalTaskCreate{TaskID: taskID, MachineID: "machine-a", Source: "local", Type: "analysis", Stage: 2, EnvelopeDigest: "digest-" + taskID, Envelope: []byte("envelope-" + taskID)}); err != nil {
			t.Fatal(err)
		}
		run, err := db.BeginLocalAnalysis(context.Background(), "machine-a", taskID)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.CompleteLocalAnalysis(context.Background(), run.RunID); err != nil {
			t.Fatal(err)
		}
		return run
	}
	engine := NewEngine("machine-a", engineStageOne{err: errors.New("stage one must not rerun")}, db, &engineWorker{makeResult: validEngineStageResult}, config.Phase2Config{})

	first := createRun("task-complete")
	if err := engine.RunWithProgress(context.Background(), first.TaskID, nil); err != nil {
		t.Fatalf("recover complete: %v", err)
	}
	current, err := db.CurrentLocalAnalysis(context.Background(), "machine-a")
	if err != nil || current.RunID != first.RunID {
		t.Fatalf("current after complete recovery=%#v err=%v", current, err)
	}
	if err := engine.RunWithProgress(context.Background(), first.TaskID, nil); err != nil {
		t.Fatalf("recover published current: %v", err)
	}

	second := createRun("task-newer")
	if err := db.PublishLocalAnalysis(context.Background(), second.RunID); err != nil {
		t.Fatal(err)
	}
	if err := engine.RunWithProgress(context.Background(), first.TaskID, nil); err == nil {
		t.Fatal("superseded published run was accepted as current")
	}
}

func TestEngineCompletesAndPersistsAllStage2BeforeAnyStage3(t *testing.T) {
	result := engineTwoCandidateResult()
	w := &engineWorker{makeResult: validEngineStageResult}
	w.fail = func(job *worker.JobMsg, _ int) error {
		if job.ScreenStage == worker.ScreenStageThree && job.Path == `D:\d.jpg` {
			return errors.New(`decode failed D:\d.jpg`)
		}
		return nil
	}
	s := &engineStore{run: store.LocalAnalysisRun{RunID: "run-staged", MachineID: "machine-a", Generation: 5, TaskID: "task-staged", Status: "building"}, fail: map[string]error{}}
	engine := NewEngine("machine-a", engineStageOne{result: result}, s, w, testPhase2Config())
	engine.fileMetadata = func(string) (int64, int64, error) { return 10, 20, nil }
	err := engine.Run(context.Background(), "task-staged")
	if err == nil {
		t.Fatal("stage three injected failure returned nil")
	}
	stage2Saved := 0
	stage3Saved := 0
	for _, pair := range s.pairs {
		if pair.Stage2JSON != nil && pair.Stage3JSON == nil {
			stage2Saved++
			if !strings.Contains(*pair.Stage2JSON, `"verdict":"yes"`) || !strings.Contains(*pair.Stage2JSON, `"reason":"phash_passed"`) {
				t.Fatalf("stage2 JSON = %s", *pair.Stage2JSON)
			}
		}
		if pair.Stage3JSON != nil {
			stage3Saved++
			if !strings.Contains(*pair.Stage3JSON, `"verdict":"yes"`) || !strings.Contains(*pair.Stage3JSON, `"reason":"sobel_passed"`) {
				t.Fatalf("stage3 JSON = %s", *pair.Stage3JSON)
			}
		}
	}
	if stage2Saved != 2 || stage3Saved != 1 || !hasEngineStageEvent(s.events, "stage2") || hasEngineStageEvent(s.events, "stage3") {
		t.Fatalf("stage2/stage3 saved=%d/%d events=%#v", stage2Saved, stage3Saved, s.events)
	}
	stage3Jobs := 0
	for _, job := range w.jobs {
		if job.ScreenStage == worker.ScreenStageThree {
			stage3Jobs++
		}
	}
	if stage3Jobs != 4 {
		t.Fatalf("stage3 jobs = %d, want stop at failing fourth endpoint", stage3Jobs)
	}
	for _, call := range s.calls {
		if call == "groups" || call == "complete" || call == "publish" {
			t.Fatalf("failed stage3 called %q", call)
		}
	}
}

func TestEngineStage2FailureHasNoStageCompletionOrStage3(t *testing.T) {
	w := &engineWorker{makeResult: validEngineStageResult}
	w.fail = func(job *worker.JobMsg, index int) error {
		if index == 3 {
			return errors.New("stage two failed")
		}
		return nil
	}
	s := &engineStore{run: store.LocalAnalysisRun{RunID: "run-stage2-fail", MachineID: "machine-a", Generation: 6, TaskID: "task-stage2-fail", Status: "building"}, fail: map[string]error{}}
	engine := NewEngine("machine-a", engineStageOne{result: engineTwoCandidateResult()}, s, w, testPhase2Config())
	engine.fileMetadata = func(string) (int64, int64, error) { return 10, 20, nil }
	if err := engine.Run(context.Background(), "task-stage2-fail"); err == nil {
		t.Fatal("stage two failure returned nil")
	}
	if hasEngineStageEvent(s.events, "stage2") || hasEngineStageEvent(s.events, "stage3") {
		t.Fatalf("failed stage emitted completion event: %#v", s.events)
	}
	for _, job := range w.jobs {
		if job.ScreenStage == worker.ScreenStageThree {
			t.Fatal("stage three scheduled after stage two failure")
		}
	}
}

func TestEngineRunWithProgressCheckpointsAfterDurableStage2BeforeStage3(t *testing.T) {
	w := &engineWorker{makeResult: validEngineStageResult}
	s := &engineStore{run: store.LocalAnalysisRun{RunID: "run-progress", MachineID: "machine-a", Generation: 1, TaskID: "task-progress", Status: "building"}, fail: map[string]error{}}
	engine := NewEngine("machine-a", engineStageOne{result: engineTwoCandidateResult()}, s, w, testPhase2Config())
	engine.fileMetadata = func(string) (int64, int64, error) { return 10, 20, nil }
	callbackErr := errors.New("checkpoint failed")
	calls := 0
	err := engine.RunWithProgress(context.Background(), "task-progress", func(stage int) error {
		calls++
		if stage != 2 {
			t.Fatalf("stage=%d want2", stage)
		}
		if !hasEngineStageEvent(s.events, "stage2") {
			t.Fatal("callback ran before durable stage2 outbox")
		}
		for _, job := range w.jobs {
			if job.ScreenStage == worker.ScreenStageThree {
				t.Fatal("stage3 ran before callback")
			}
		}
		return callbackErr
	})
	if !errors.Is(err, callbackErr) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	for _, job := range w.jobs {
		if job.ScreenStage == worker.ScreenStageThree {
			t.Fatal("stage3 ran after failed callback")
		}
	}
}

func TestEngineComputeRejectsForeignIdentityAndStagePayload(t *testing.T) {
	path := `D:\secret\media.jpg`
	file := firstscreen.File{FileRef: firstscreen.FileRef{ID: 9, MachineID: "machine-a", Path: path}, SHA512: [64]byte{7}}
	validResult := func(job *worker.JobMsg) *worker.JobResultMsg {
		return &worker.JobResultMsg{
			JobID: job.JobID, ScanTaskID: job.ScanTaskID, Path: job.Path,
			Kind: job.Kind, Phase: job.Phase, ScreenStage: job.ScreenStage,
			Source: job.Source, SHA512: append([]byte(nil), job.KnownSHA...),
			FieldsDone: job.FieldsMask, FramesDone: job.FrameMask,
			PHashParts: features.EncodePHashParts([9]uint64{}),
		}
	}
	tests := []struct {
		name   string
		mutate func(*worker.JobResultMsg)
	}{
		{"job id", func(r *worker.JobResultMsg) { r.JobID++ }},
		{"task id", func(r *worker.JobResultMsg) { r.ScanTaskID = "foreign" }},
		{"path", func(r *worker.JobResultMsg) { r.Path = `D:\other.jpg` }},
		{"kind", func(r *worker.JobResultMsg) { r.Kind = worker.MediaVideo }},
		{"phase", func(r *worker.JobResultMsg) { r.Phase = worker.Phase1 }},
		{"screen stage", func(r *worker.JobResultMsg) { r.ScreenStage = worker.ScreenStageThree }},
		{"source", func(r *worker.JobResultMsg) { r.Source = worker.JobSourceManager }},
		{"sha", func(r *worker.JobResultMsg) { r.SHA512[0]++ }},
		{"fields missing", func(r *worker.JobResultMsg) { r.FieldsDone = 0 }},
		{"fields extra", func(r *worker.JobResultMsg) { r.FieldsDone |= worker.MaskSobelHist }},
		{"frames crossed into image", func(r *worker.JobResultMsg) { r.FramesDone = 1 }},
		{"extra stage payload", func(r *worker.JobResultMsg) { r.SobelHist = []byte{1} }},
		{"missing stage payload", func(r *worker.JobResultMsg) { r.PHashParts = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			w := &engineWorker{makeResult: func(job *worker.JobMsg) *worker.JobResultMsg {
				result := validResult(job)
				test.mutate(result)
				return result
			}}
			engine := NewEngine("machine-a", engineStageOne{}, &engineStore{}, w, testPhase2Config())
			engine.fileMetadata = func(string) (int64, int64, error) { return 10, 20, nil }
			if _, err := engine.compute(context.Background(), "task-a", file, worker.MediaImage, worker.ScreenStageTwo); err == nil {
				t.Fatal("foreign or malformed worker result was accepted")
			} else if strings.Contains(strings.ToLower(err.Error()), strings.ToLower(path)) || strings.Contains(strings.ToLower(err.Error()), "media.jpg") {
				t.Fatalf("error leaked media path: %v", err)
			}
		})
	}

	t.Run("valid", func(t *testing.T) {
		w := &engineWorker{makeResult: validResult}
		engine := NewEngine("machine-a", engineStageOne{}, &engineStore{}, w, testPhase2Config())
		engine.fileMetadata = func(string) (int64, int64, error) { return 10, 20, nil }
		result, err := engine.compute(context.Background(), "task-a", file, worker.MediaImage, worker.ScreenStageTwo)
		if err != nil || !bytes.Equal(result.SHA512, file.SHA512[:]) {
			t.Fatalf("valid result = %#v, %v", result, err)
		}
	})

	t.Run("unassigned job id", func(t *testing.T) {
		w := engineWorkerFunc(func(_ context.Context, job *worker.JobMsg) (*worker.JobResultMsg, error) {
			return validResult(job), nil
		})
		engine := NewEngine("machine-a", engineStageOne{}, &engineStore{}, w, testPhase2Config())
		engine.fileMetadata = func(string) (int64, int64, error) { return 10, 20, nil }
		if _, err := engine.compute(context.Background(), "task-a", file, worker.MediaImage, worker.ScreenStageTwo); err == nil {
			t.Fatal("worker result with unassigned zero job ID was accepted")
		}
	})
}

func TestEngineComputeRejectsVideoCoverageAndPayloadCrossover(t *testing.T) {
	file := firstscreen.File{FileRef: firstscreen.FileRef{ID: 10, MachineID: "machine-a", Path: `D:\private\clip.mp4`}, SHA512: [64]byte{8}}
	tests := []struct {
		name   string
		mutate func(*worker.JobResultMsg)
	}{
		{"fields missing", func(result *worker.JobResultMsg) { result.FieldsDone = 0 }},
		{"fields crossed", func(result *worker.JobResultMsg) { result.FieldsDone = worker.MaskVideo6FSobel }},
		{"frames missing", func(result *worker.JobResultMsg) { result.FramesDone &^= 1 << 5 }},
		{"frames extra", func(result *worker.JobResultMsg) { result.FramesDone |= 1 << 6 }},
		{"partial fields done", func(result *worker.JobResultMsg) {
			result.FramesDone &^= 1 << 5
			result.Frames[5].PHashParts = nil
			result.Frames[5].Error = "native_status_-20"
		}},
		{"successful frame has error", func(result *worker.JobResultMsg) { result.Frames[0].Error = "native_status_-20" }},
		{"failed frame lacks error", func(result *worker.JobResultMsg) {
			result.FieldsDone = 0
			result.FramesDone &^= 1 << 5
			result.Frames[5].PHashParts = nil
		}},
		{"failed frame carries payload", func(result *worker.JobResultMsg) {
			result.FieldsDone = 0
			result.FramesDone &^= 1 << 5
			result.Frames[5].Error = "native_status_-20"
		}},
		{"frame payload crossed", func(result *worker.JobResultMsg) { result.Frames[0].SobelHist = []byte{1} }},
		{"frame payload missing", func(result *worker.JobResultMsg) { result.Frames[0].PHashParts = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			w := &engineWorker{makeResult: func(job *worker.JobMsg) *worker.JobResultMsg {
				result := validEngineStageResult(job)
				test.mutate(result)
				return result
			}}
			engine := NewEngine("machine-a", engineStageOne{}, &engineStore{}, w, testPhase2Config())
			engine.fileMetadata = func(string) (int64, int64, error) { return 10, 20, nil }
			if _, err := engine.compute(context.Background(), "task-video", file, worker.MediaVideo, worker.ScreenStageTwo); err == nil {
				t.Fatal("crossed video coverage or payload was accepted")
			} else if strings.Contains(strings.ToLower(err.Error()), "clip.mp4") || strings.Contains(strings.ToLower(err.Error()), "private") {
				t.Fatalf("error leaked media path: %v", err)
			}
		})
	}
}

func TestEngineAcceptsExplicitPartialVideoFrames(t *testing.T) {
	tests := []struct {
		name         string
		partialStage worker.ScreenStage
		healthy      int
		wantStage2   string
		wantStage3   string
		wantVerdict  string
		wantGroups   int
	}{
		{name: "stage two five healthy frames pass", partialStage: worker.ScreenStageTwo, healthy: 5, wantStage2: `"verdict":"yes"`, wantStage3: `"verdict":"yes"`, wantVerdict: "duplicate", wantGroups: 1},
		{name: "stage three five healthy frames pass", partialStage: worker.ScreenStageThree, healthy: 5, wantStage2: `"verdict":"yes"`, wantStage3: `"verdict":"yes"`, wantVerdict: "duplicate", wantGroups: 1},
		{name: "stage two three healthy frames are inconclusive", partialStage: worker.ScreenStageTwo, healthy: 3, wantStage2: `"verdict":"inconclusive"`, wantVerdict: "uncertain"},
		{name: "stage three three healthy frames are inconclusive", partialStage: worker.ScreenStageThree, healthy: 3, wantStage2: `"verdict":"yes"`, wantStage3: `"verdict":"inconclusive"`, wantVerdict: "uncertain"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			w := &engineWorker{makeResult: func(job *worker.JobMsg) *worker.JobResultMsg {
				result := validEngineStageResult(job)
				if job.ScreenStage != test.partialStage {
					return result
				}
				result.FieldsDone = 0
				result.FramesDone = uint8(1<<uint(test.healthy)) - 1
				for index := test.healthy; index < len(result.Frames); index++ {
					result.Frames[index].PHashParts = nil
					result.Frames[index].SobelHist = nil
					result.Frames[index].Error = "native_status_-20"
				}
				return result
			}}
			s := &engineStore{run: store.LocalAnalysisRun{RunID: "run-video-partial", MachineID: "machine-a", Generation: 7, TaskID: "task-video-partial", Status: "building"}, fail: map[string]error{}}
			engine := NewEngine("machine-a", engineStageOne{result: engineVideoCandidateResult()}, s, w, testPhase2Config())
			engine.fileMetadata = func(string) (int64, int64, error) { return 10, 20, nil }

			if err := engine.Run(context.Background(), "task-video-partial"); err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if len(s.pairs) == 0 {
				t.Fatal("partial video pair was not persisted")
			}
			first, final := s.pairs[0], s.pairs[len(s.pairs)-1]
			if first.Stage2JSON == nil || !strings.Contains(*first.Stage2JSON, test.wantStage2) {
				t.Fatalf("stage2 JSON = %v, want %s", first.Stage2JSON, test.wantStage2)
			}
			if test.wantStage3 == "" {
				if len(s.pairs) != 1 || final.Stage3JSON != nil {
					t.Fatalf("inconclusive stage2 entered stage3: %#v", s.pairs)
				}
			} else if final.Stage3JSON == nil || !strings.Contains(*final.Stage3JSON, test.wantStage3) {
				t.Fatalf("stage3 JSON = %v, want %s", final.Stage3JSON, test.wantStage3)
			}
			if final.Verdict != test.wantVerdict || len(s.groups) != test.wantGroups {
				t.Fatalf("final verdict/groups = %q/%d, want %q/%d", final.Verdict, len(s.groups), test.wantVerdict, test.wantGroups)
			}
			if test.wantGroups == 1 && s.groups[0].Category != "video" {
				t.Fatalf("duplicate group = %#v, want video", s.groups[0])
			}
			if s.calls[len(s.calls)-1] != "publish" {
				t.Fatalf("last call = %q, want publish", s.calls[len(s.calls)-1])
			}
		})
	}
}

func TestEngineStage2FailureDoesNotScheduleStage3(t *testing.T) {
	result := engineCandidateResult()
	partsA, partsB := [9]uint64{}, [9]uint64{}
	partsB[0], partsB[1] = (1<<11)-1, (1<<11)-1
	w := &engineWorker{}
	w.makeResult = func(job *worker.JobMsg) *worker.JobResultMsg {
		result := validEngineStageResult(job)
		raw := features.EncodePHashParts(partsA)
		if job.Path == `D:\b.jpg` {
			raw = features.EncodePHashParts(partsB)
		}
		result.PHashParts = raw
		return result
	}
	s := &engineStore{run: store.LocalAnalysisRun{RunID: "run-1", MachineID: "machine-a", Generation: 2, TaskID: "task-1", Status: "building"}, fail: map[string]error{}}
	engine := NewEngine("machine-a", engineStageOne{result: result}, s, w, testPhase2Config())
	engine.fileMetadata = func(string) (int64, int64, error) { return 1234, 5678, nil }
	if err := engine.Run(context.Background(), "task-1"); err != nil {
		t.Fatal(err)
	}
	for _, job := range w.jobs {
		if job.Size != 1234 || job.MTimeMS != 5678 || len(job.KnownSHA) != 64 || job.Source != worker.JobSourceLocal {
			t.Fatalf("stage job identity = %#v", job)
		}
		if job.ScreenStage == worker.ScreenStageThree {
			t.Fatal("stage 3 was scheduled after stage 2 no")
		}
	}
	if len(s.pairs) != 1 || s.pairs[0].Verdict != "not_duplicate" || s.pairs[0].Stage2JSON == nil || s.pairs[0].Stage3JSON != nil {
		t.Fatalf("pair = %#v", s.pairs)
	}
	if len(s.groups) != 1 || s.groups[0].Category != "exact" {
		t.Fatalf("groups = %#v, want exact only", s.groups)
	}
}

func TestEnginePublishFailureNeverReportsPublishedOrReplacesOldCurrent(t *testing.T) {
	publishErr := errors.New("injected publish failure")
	s := &engineStore{run: store.LocalAnalysisRun{RunID: "run-2", MachineID: "machine-a", Generation: 3, TaskID: "task-2", Status: "building"}, fail: map[string]error{"publish": publishErr}}
	w := &engineWorker{makeResult: func(job *worker.JobMsg) *worker.JobResultMsg { return nil }}
	engine := NewEngine("machine-a", engineStageOne{result: firstscreen.Result{}}, s, w, testPhase2Config())
	err := engine.Run(context.Background(), "task-2")
	if !errors.Is(err, publishErr) {
		t.Fatalf("Run error = %v", err)
	}
	if got := s.calls[len(s.calls)-1]; got != "publish" {
		t.Fatalf("last call = %q, want publish", got)
	}
}

func TestEngineCancellationStopsBeforeFinalGroupReplacement(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &engineStore{run: store.LocalAnalysisRun{RunID: "run-3", MachineID: "machine-a", Generation: 4, TaskID: "task-3", Status: "building"}, fail: map[string]error{}}
	w := &engineWorker{}
	err := NewEngine("machine-a", engineStageOne{result: firstscreen.Result{}}, s, w, testPhase2Config()).Run(ctx, "task-3")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v", err)
	}
	for _, call := range s.calls {
		if call == "groups" || call == "complete" || call == "publish" {
			t.Fatalf("canceled run called %q", call)
		}
	}
}

func engineCandidateResult() firstscreen.Result {
	exact, a, b := [64]byte{1}, [64]byte{2}, [64]byte{3}
	return firstscreen.Result{
		Files: []firstscreen.File{
			{FileRef: firstscreen.FileRef{ID: 1, MachineID: "machine-a", Path: `D:\exact-1.jpg`}, SHA512: exact},
			{FileRef: firstscreen.FileRef{ID: 2, MachineID: "machine-a", Path: `D:\exact-2.jpg`}, SHA512: exact},
			{FileRef: firstscreen.FileRef{ID: 3, MachineID: "machine-a", Path: `D:\a.jpg`}, SHA512: a},
			{FileRef: firstscreen.FileRef{ID: 4, MachineID: "machine-a", Path: `D:\b.jpg`}, SHA512: b},
		},
		ExactGroups:    []firstscreen.ExactGroup{{SHA512: exact, Members: []firstscreen.FileRef{{ID: 1, MachineID: "machine-a"}, {ID: 2, MachineID: "machine-a"}}}},
		CandidatePairs: []firstscreen.CandidatePair{{Kind: firstscreen.KindImageCandidate, ShaA: a, ShaB: b, QualityA: 80, QualityB: 70}},
	}
}

func engineTwoCandidateResult() firstscreen.Result {
	a, b, c, d := [64]byte{1}, [64]byte{2}, [64]byte{3}, [64]byte{4}
	return firstscreen.Result{
		Files: []firstscreen.File{
			{FileRef: firstscreen.FileRef{ID: 1, MachineID: "machine-a", Path: `D:\a.jpg`}, SHA512: a},
			{FileRef: firstscreen.FileRef{ID: 2, MachineID: "machine-a", Path: `D:\b.jpg`}, SHA512: b},
			{FileRef: firstscreen.FileRef{ID: 3, MachineID: "machine-a", Path: `D:\c.jpg`}, SHA512: c},
			{FileRef: firstscreen.FileRef{ID: 4, MachineID: "machine-a", Path: `D:\d.jpg`}, SHA512: d},
		},
		CandidatePairs: []firstscreen.CandidatePair{
			{Kind: firstscreen.KindImageCandidate, ShaA: a, ShaB: b, QualityA: 80, QualityB: 70},
			{Kind: firstscreen.KindImageCandidate, ShaA: c, ShaB: d, QualityA: 60, QualityB: 50},
		},
	}
}

func engineVideoCandidateResult() firstscreen.Result {
	a, b := [64]byte{5}, [64]byte{6}
	return firstscreen.Result{
		Files: []firstscreen.File{
			{FileRef: firstscreen.FileRef{ID: 5, MachineID: "machine-a", Path: `D:\a.mp4`}, SHA512: a},
			{FileRef: firstscreen.FileRef{ID: 6, MachineID: "machine-a", Path: `D:\b.mp4`}, SHA512: b},
		},
		CandidatePairs: []firstscreen.CandidatePair{{Kind: firstscreen.KindVideoCandidate, ShaA: a, ShaB: b, QualityA: 80, QualityB: 70}},
	}
}

func validEngineStageResult(job *worker.JobMsg) *worker.JobResultMsg {
	result := &worker.JobResultMsg{
		JobID: job.JobID, ScanTaskID: job.ScanTaskID, Path: job.Path,
		Kind: job.Kind, Phase: job.Phase, ScreenStage: job.ScreenStage,
		Source: job.Source, SHA512: append([]byte(nil), job.KnownSHA...),
		FieldsDone: job.FieldsMask, FramesDone: job.FrameMask,
	}
	if job.Kind == worker.MediaImage {
		if job.ScreenStage == worker.ScreenStageTwo {
			result.PHashParts = features.EncodePHashParts([9]uint64{})
		} else {
			result.SobelHist, _ = features.EncodeSobelHist([128]float32{})
		}
	} else {
		result.Frames = make([]worker.FrameFeature, 6)
		for index := range result.Frames {
			result.Frames[index].FrameIdx = index
			if job.ScreenStage == worker.ScreenStageTwo {
				result.Frames[index].PHashParts = features.EncodePHashParts([9]uint64{})
			} else {
				result.Frames[index].SobelHist, _ = features.EncodeSobelHist([128]float32{})
			}
		}
	}
	return result
}

func hasEngineStageEvent(events []store.LocalOutboxEvent, stage string) bool {
	for _, event := range events {
		if strings.HasSuffix(event.EntityKey, ":"+stage) {
			return true
		}
	}
	return false
}

func testPhase2Config() config.Phase2Config {
	return config.Phase2Config{PHashPassT2: .8, PHashPartThreshold: 10, SobelT3: .85, VideoFrames: 6, VideoAvgT4: .8, VideoMinPassed: 4, VideoMinValid: 4}
}
