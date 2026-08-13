package firstscreen

import (
	"context"
	"encoding/hex"
	"log/slog"
	"testing"
)

type candidateSourceFixture struct {
	files  []File
	images map[string]ImageFeature
	videos map[string]VideoFeature
}

func (s candidateSourceFixture) StreamActiveFiles(_ context.Context, _ string, visit func(File) error) error {
	for _, file := range s.files {
		if err := visit(file); err != nil {
			return err
		}
	}
	return nil
}

func (s candidateSourceFixture) LoadImageFeatures(_ context.Context, _ []string) (map[string]ImageFeature, error) {
	return s.images, nil
}

func (s candidateSourceFixture) LoadVideoFeatures(_ context.Context, _ []string) (map[string]VideoFeature, error) {
	return s.videos, nil
}

type candidateSinkFixture struct{ got Result }

func (s *candidateSinkFixture) ReplaceStageOne(_ context.Context, _ string, result Result) error {
	s.got = result
	return nil
}

func TestCandidateSourceCreatesExactYesAndSimilarityCandidatesForOneMachine(t *testing.T) {
	shaExact := testAnalyzerSHA(1)
	shaImageA, shaImageB := testAnalyzerSHA(2), testAnalyzerSHA(3)
	shaVideoA, shaVideoB := testAnalyzerSHA(4), testAnalyzerSHA(5)
	foreignSHA := shaExact

	imagePDQ := [4]uint64{1}
	videoPDQ := [4]uint64{9}
	source := candidateSourceFixture{
		files: []File{
			{FileRef: FileRef{ID: 1, MachineID: "machine-a"}, SHA512: shaExact},
			{FileRef: FileRef{ID: 2, MachineID: "machine-a"}, SHA512: shaExact},
			{FileRef: FileRef{ID: 3, MachineID: "machine-a"}, SHA512: shaImageA},
			{FileRef: FileRef{ID: 4, MachineID: "machine-a"}, SHA512: shaImageB},
			{FileRef: FileRef{ID: 5, MachineID: "machine-a"}, SHA512: shaVideoA},
			{FileRef: FileRef{ID: 6, MachineID: "machine-a"}, SHA512: shaVideoB},
			{FileRef: FileRef{ID: 7, MachineID: "machine-b"}, SHA512: foreignSHA},
		},
		images: map[string]ImageFeature{
			hex.EncodeToString(shaImageA[:]): {SHA512: shaImageA, PDQ: imagePDQ, Quality: 80, Width: 100, Height: 100},
			hex.EncodeToString(shaImageB[:]): {SHA512: shaImageB, PDQ: [4]uint64{3}, Quality: 80, Width: 200, Height: 200},
		},
		videos: map[string]VideoFeature{
			hex.EncodeToString(shaVideoA[:]): {SHA512: shaVideoA, DurationMs: 10_000, ThumbPDQ: videoPDQ, ThumbQuality: 75},
			hex.EncodeToString(shaVideoB[:]): {SHA512: shaVideoB, DurationMs: 10_500, ThumbPDQ: [4]uint64{11}, ThumbQuality: 75},
		},
	}
	sink := &candidateSinkFixture{}

	analyzer := NewCandidateAnalyzer(source, sink, DefaultConfig(), slog.New(slog.NewTextHandler(testWriter{t}, nil)))
	result, err := analyzer.Run(context.Background(), "machine-a", "run-a")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.ExactGroups) != 1 || len(result.ExactGroups[0].Members) != 2 {
		t.Fatalf("exact groups = %#v, want one two-member SHA group", result.ExactGroups)
	}
	if result.ExactVerdicts[shaExact] != "yes" {
		t.Fatalf("exact verdict = %q, want yes", result.ExactVerdicts[shaExact])
	}
	if len(result.CandidatePairs) != 2 {
		t.Fatalf("candidate pairs = %#v, want one image and one video pair", result.CandidatePairs)
	}
	if result.CandidatePairs[0].Kind != KindImageCandidate || result.CandidatePairs[0].Hamming != 1 {
		t.Fatalf("image pair = %#v, want PDQ hamming 1", result.CandidatePairs[0])
	}
	if result.CandidatePairs[1].Kind != KindVideoCandidate || result.CandidatePairs[1].DurationDiffMs != 500 || result.CandidatePairs[1].Hamming != 1 {
		t.Fatalf("video pair = %#v, want duration 500 and PDQ hamming 1", result.CandidatePairs[1])
	}
	if len(result.Files) != 6 || len(sink.got.Files) != 6 {
		t.Fatalf("machine scoped files = %d/%d, want foreign machine excluded", len(result.Files), len(sink.got.Files))
	}
}

func TestCandidateSourceNeverGroupsSameSHAFromAnotherMachine(t *testing.T) {
	sha := testAnalyzerSHA(0x44)
	source := candidateSourceFixture{files: []File{
		{FileRef: FileRef{ID: 1, MachineID: "machine-a"}, SHA512: sha},
		{FileRef: FileRef{ID: 2, MachineID: "machine-b"}, SHA512: sha},
	}}
	result, err := NewCandidateAnalyzer(source, &candidateSinkFixture{}, DefaultConfig(), nil).Run(context.Background(), "machine-a", "run-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ExactGroups) != 0 || len(result.Files) != 1 {
		t.Fatalf("same-SHA cross-machine result = %#v, want no exact group and one local file", result)
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}
