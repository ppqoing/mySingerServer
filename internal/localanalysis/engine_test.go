package localanalysis

import (
	"context"
	"errors"
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
	run    store.LocalAnalysisRun
	calls  []string
	pairs  []store.LocalPairScore
	groups []store.LocalAnalysisGroup
	fail   map[string]error
}

func (s *engineStore) BeginLocalAnalysis(context.Context, string, string) (store.LocalAnalysisRun, error) {
	s.calls = append(s.calls, "begin")
	return s.run, s.fail["begin"]
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
func (s *engineStore) EnqueueLocalEvent(context.Context, store.LocalOutboxEvent) error {
	s.calls = append(s.calls, "outbox")
	return s.fail["outbox"]
}

type engineWorker struct {
	jobs       []*worker.JobMsg
	makeResult func(*worker.JobMsg) *worker.JobResultMsg
}

func (w *engineWorker) Execute(_ context.Context, job *worker.JobMsg) (*worker.JobResultMsg, error) {
	w.jobs = append(w.jobs, job)
	job.JobID = int64(len(w.jobs))
	return w.makeResult(job), nil
}

func TestEngineStage2FailureDoesNotScheduleStage3(t *testing.T) {
	result := engineCandidateResult()
	partsA, partsB := [9]uint64{}, [9]uint64{}
	partsB[0], partsB[1] = (1<<11)-1, (1<<11)-1
	w := &engineWorker{}
	w.makeResult = func(job *worker.JobMsg) *worker.JobResultMsg {
		raw := features.EncodePHashParts(partsA)
		if job.Path == `D:\b.jpg` {
			raw = features.EncodePHashParts(partsB)
		}
		return &worker.JobResultMsg{JobID: job.JobID, ScreenStage: job.ScreenStage, Source: job.Source, Kind: job.Kind, PHashParts: raw}
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

func testPhase2Config() config.Phase2Config {
	return config.Phase2Config{PHashPassT2: .8, PHashPartThreshold: 10, SobelT3: .85, VideoFrames: 6, VideoAvgT4: .8, VideoMinPassed: 4, VideoMinValid: 4}
}
