package localanalysis

import (
	"context"
	"testing"

	"dedup/internal/firstscreen"
)

type stageOneSource struct{}

func (stageOneSource) StreamActiveFiles(_ context.Context, machineID string, visit func(firstscreen.File) error) error {
	sha := [64]byte{1}
	return visit(firstscreen.File{FileRef: firstscreen.FileRef{ID: 1, MachineID: machineID}, SHA512: sha})
}

func (stageOneSource) LoadImageFeatures(context.Context, []string) (map[string]firstscreen.ImageFeature, error) {
	return nil, nil
}

func (stageOneSource) LoadVideoFeatures(context.Context, []string) (map[string]firstscreen.VideoFeature, error) {
	return nil, nil
}

type stageOneSink struct {
	runID  string
	result firstscreen.Result
}

func (s *stageOneSink) ReplaceStageOne(_ context.Context, runID string, result firstscreen.Result) error {
	s.runID = runID
	s.result = result
	return nil
}

func TestLocalStageOneRunForRootsFiltersBeforePersisting(t *testing.T) {
	source := rootSourceFixture{files: []firstscreen.File{
		{FileRef: firstscreen.FileRef{ID: 1, MachineID: "machine-a", Path: `I:\tmp\wallpa\a.jpg`}, SHA512: [64]byte{1}},
		{FileRef: firstscreen.FileRef{ID: 2, MachineID: "machine-a", Path: `H:\pik\b.jpg`}, SHA512: [64]byte{2}},
	}}
	sink := &stageOneSink{}
	stage := NewStageOne(source, sink, firstscreen.DefaultConfig(), nil)
	result, err := stage.RunForRoots(context.Background(), "machine-a", "building-run", []string{`I:\tmp\wallpa`})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 1 || result.Files[0].ID != 1 || len(sink.result.Files) != 1 || sink.result.Files[0].ID != 1 {
		t.Fatalf("result=%#v persisted=%#v", result.Files, sink.result.Files)
	}
}

func TestLocalStageOneWritesOnlyRequestedRunWithoutPublishingCurrent(t *testing.T) {
	sink := &stageOneSink{}
	stage := NewStageOne(stageOneSource{}, sink, firstscreen.DefaultConfig(), nil)
	if _, err := stage.Run(context.Background(), "machine-a", "building-run"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sink.runID != "building-run" {
		t.Fatalf("sink run ID = %q, want requested building run", sink.runID)
	}
}
