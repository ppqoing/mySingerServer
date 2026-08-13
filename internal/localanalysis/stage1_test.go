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

type stageOneSink struct{ runID string }

func (s *stageOneSink) ReplaceStageOne(_ context.Context, runID string, _ firstscreen.Result) error {
	s.runID = runID
	return nil
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
