package firstscreen

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
)

var errAnalyzerStage = errors.New("injected analyzer stage failure")

type analyzerFileRow struct {
	sha  [64]byte
	file FileRef
}

type fakeAnalyzerStore struct {
	calls         []string
	failStage     string
	badRows       int
	files         []analyzerFileRow
	imageFeatures []ImageFeature
	videoFeatures []VideoFeature
	imageBadRows  int
	videoBadRows  int
	writtenExact  []ExactGroup
	writtenPairs  []CandidatePair
}

func (s *fakeAnalyzerStore) call(stage string) error {
	s.calls = append(s.calls, stage)
	if s.failStage == stage {
		return errAnalyzerStage
	}
	return nil
}

func (s *fakeAnalyzerStore) StreamFilesBySHA(
	_ context.Context,
	visit func([64]byte, FileRef) error,
) error {
	if err := s.call("exact_group"); err != nil {
		return err
	}
	for _, row := range s.files {
		if err := visit(row.sha, row.file); err != nil {
			return err
		}
	}
	return nil
}

func (s *fakeAnalyzerStore) LoadImageFeatures(context.Context) ([]ImageFeature, error) {
	if err := s.call("image_load"); err != nil {
		return nil, err
	}
	s.badRows += s.imageBadRows
	return append([]ImageFeature(nil), s.imageFeatures...), nil
}

func (s *fakeAnalyzerStore) LoadVideoFeatures(context.Context) ([]VideoFeature, error) {
	if err := s.call("video_load"); err != nil {
		return nil, err
	}
	s.badRows += s.videoBadRows
	return append([]VideoFeature(nil), s.videoFeatures...), nil
}

func (s *fakeAnalyzerStore) ReplaceResults(
	_ context.Context,
	exact []ExactGroup,
	pairs []CandidatePair,
) (groups, members, skipped int, err error) {
	s.calls = append(s.calls, "db_write")
	s.writtenExact = append([]ExactGroup(nil), exact...)
	s.writtenPairs = append([]CandidatePair(nil), pairs...)
	if s.failStage == "db_write" {
		return 4, 8, 3, errAnalyzerStage
	}
	return 4, 8, 3, nil
}

func (s *fakeAnalyzerStore) BadRows() int {
	return s.badRows
}

func TestAnalyzerRunsStagesInOrderAndReportsMetrics(t *testing.T) {
	sha1, sha2, sha3, sha4 := testAnalyzerSHA(1), testAnalyzerSHA(2), testAnalyzerSHA(3), testAnalyzerSHA(4)
	store := &fakeAnalyzerStore{
		files: []analyzerFileRow{
			{sha: sha1, file: FileRef{ID: 1}},
			{sha: sha1, file: FileRef{ID: 2}},
			{sha: sha2, file: FileRef{ID: 3}},
		},
		imageFeatures: []ImageFeature{{SHA512: sha1}},
		videoFeatures: []VideoFeature{{SHA512: sha2}},
		imageBadRows:  1,
		videoBadRows:  1,
	}
	imagePairs := []CandidatePair{{Kind: KindImageCandidate, ShaA: sha1, ShaB: sha2}}
	videoPairs := []CandidatePair{{Kind: KindVideoCandidate, ShaA: sha3, ShaB: sha4}}

	var logOutput bytes.Buffer
	analyzer := newAnalyzer(store, DefaultConfig(), slog.New(slog.NewJSONHandler(&logOutput, nil)))
	analyzer.screenImage = func([]ImageFeature, int, float64, int) ([]CandidatePair, error) {
		store.calls = append(store.calls, "image_screen")
		return append([]CandidatePair(nil), imagePairs...), nil
	}
	analyzer.screenVideo = func([]VideoFeature, int64, int) ([]CandidatePair, error) {
		store.calls = append(store.calls, "video_screen")
		return append([]CandidatePair(nil), videoPairs...), nil
	}

	stats, err := analyzer.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	wantCalls := []string{
		"exact_group",
		"image_load",
		"image_screen",
		"video_load",
		"video_screen",
		"db_write",
	}
	if !reflect.DeepEqual(store.calls, wantCalls) {
		t.Fatalf("stage order = %v, want %v", store.calls, wantCalls)
	}
	if !reflect.DeepEqual(store.writtenPairs, append(append([]CandidatePair(nil), imagePairs...), videoPairs...)) {
		t.Fatalf("written pairs = %#v, want image pairs followed by video pairs", store.writtenPairs)
	}
	if len(store.writtenExact) != 1 || len(store.writtenExact[0].Members) != 2 {
		t.Fatalf("written exact groups = %#v, want one two-member group", store.writtenExact)
	}

	if stats.FilesScanned != 3 ||
		stats.ExactGroups != 1 ||
		stats.ExactMembers != 2 ||
		stats.ImageFeatures != 1 ||
		stats.ImagePairs != 1 ||
		stats.VideoFeatures != 1 ||
		stats.VideoPairs != 1 ||
		stats.BadRows != 2 ||
		stats.SkippedPairs != 3 ||
		stats.GroupsWritten != 4 ||
		stats.MembersWritten != 8 {
		t.Fatalf("RunStats = %#v", stats)
	}
	assertAnalyzerStageKeys(t, stats)
	if stats.HeapAllocBytes == 0 {
		t.Fatal("HeapAllocBytes = 0, want runtime metrics collected after GC")
	}

	encoded, err := json.Marshal(stats)
	if err != nil {
		t.Fatalf("json.Marshal(RunStats): %v", err)
	}
	var document struct {
		StageElapsedMs map[string]int64 `json:"stage_elapsed_ms"`
	}
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("json.Unmarshal(RunStats): %v", err)
	}
	if len(document.StageElapsedMs) != len(analyzerStageNames) {
		t.Fatalf("serialized stage keys = %v", document.StageElapsedMs)
	}

	logText := logOutput.String()
	for _, field := range []string{
		`"files_scanned":3`,
		`"image_pairs":1`,
		`"video_pairs":1`,
		`"groups_written":4`,
		`"members_written":8`,
		`"skipped_pairs":3`,
	} {
		if !strings.Contains(logText, field) {
			t.Errorf("logger output missing %s:\n%s", field, logText)
		}
	}
}

func TestPostgresAnalyzerStillWritesExactAndBothScreenCandidateKinds(t *testing.T) {
	shaExact := testAnalyzerSHA(0x31)
	shaImageA, shaImageB := testAnalyzerSHA(0x32), testAnalyzerSHA(0x33)
	shaVideoA, shaVideoB := testAnalyzerSHA(0x34), testAnalyzerSHA(0x35)
	store := &fakeAnalyzerStore{
		files: []analyzerFileRow{
			{sha: shaExact, file: FileRef{ID: 1}}, {sha: shaExact, file: FileRef{ID: 2}},
		},
		imageFeatures: []ImageFeature{
			{SHA512: shaImageA, PDQ: [4]uint64{1}, Quality: 80, Width: 100, Height: 100},
			{SHA512: shaImageB, PDQ: [4]uint64{3}, Quality: 80, Width: 100, Height: 100},
		},
		videoFeatures: []VideoFeature{
			{SHA512: shaVideoA, DurationMs: 1000, ThumbPDQ: [4]uint64{5}, ThumbQuality: 70},
			{SHA512: shaVideoB, DurationMs: 1500, ThumbPDQ: [4]uint64{7}, ThumbQuality: 70},
		},
	}
	stats, err := newAnalyzer(store, DefaultConfig(), nil).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.ExactGroups != 1 || stats.ImagePairs != 1 || stats.VideoPairs != 1 {
		t.Fatalf("PostgreSQL analyzer stats = %#v, want one exact/image/video result", stats)
	}
	if len(store.writtenExact) != 1 || len(store.writtenPairs) != 2 ||
		store.writtenPairs[0].Kind != KindImageCandidate || store.writtenPairs[1].Kind != KindVideoCandidate {
		t.Fatalf("PostgreSQL analyzer output = exact:%#v pairs:%#v", store.writtenExact, store.writtenPairs)
	}
}

func TestAnalyzerReturnsStageQualifiedErrorsAndPartialStats(t *testing.T) {
	stages := []string{
		"exact_group",
		"image_load",
		"image_screen",
		"video_load",
		"video_screen",
		"db_write",
	}

	for failureIndex, failureStage := range stages {
		t.Run(failureStage, func(t *testing.T) {
			sha1, sha2 := testAnalyzerSHA(1), testAnalyzerSHA(2)
			store := &fakeAnalyzerStore{
				failStage: failureStage,
				badRows:   9,
				files: []analyzerFileRow{
					{sha: sha1, file: FileRef{ID: 1}},
					{sha: sha1, file: FileRef{ID: 2}},
				},
				imageFeatures: []ImageFeature{{SHA512: sha1}},
				videoFeatures: []VideoFeature{{SHA512: sha2}},
				imageBadRows:  2,
				videoBadRows:  3,
			}
			var logOutput bytes.Buffer
			analyzer := newAnalyzer(store, DefaultConfig(), slog.New(slog.NewTextHandler(&logOutput, nil)))
			analyzer.screenImage = func([]ImageFeature, int, float64, int) ([]CandidatePair, error) {
				store.calls = append(store.calls, "image_screen")
				if failureStage == "image_screen" {
					return nil, errAnalyzerStage
				}
				return []CandidatePair{{Kind: KindImageCandidate, ShaA: sha1, ShaB: sha2}}, nil
			}
			analyzer.screenVideo = func([]VideoFeature, int64, int) ([]CandidatePair, error) {
				store.calls = append(store.calls, "video_screen")
				if failureStage == "video_screen" {
					return nil, errAnalyzerStage
				}
				return []CandidatePair{{Kind: KindVideoCandidate, ShaA: sha1, ShaB: sha2}}, nil
			}

			stats, err := analyzer.Run(context.Background())
			if !errors.Is(err, errAnalyzerStage) {
				t.Fatalf("Run() error = %v, want injected failure", err)
			}
			if !strings.Contains(err.Error(), failureStage+":") {
				t.Fatalf("Run() error = %q, want stage-qualified %q", err, failureStage)
			}
			wantCalls := stages[:failureIndex+1]
			if !reflect.DeepEqual(store.calls, wantCalls) {
				t.Fatalf("calls = %v, want %v", store.calls, wantCalls)
			}
			if failureIndex < len(stages)-1 && len(store.writtenPairs) != 0 {
				t.Fatalf("early failure wrote pairs: %#v", store.writtenPairs)
			}
			wantBadRows := []int{0, 0, 2, 2, 5, 5}[failureIndex]
			if stats.BadRows != wantBadRows {
				t.Fatalf("BadRows = %d, want current-run delta %d", stats.BadRows, wantBadRows)
			}
			if failureIndex > 0 && stats.ExactGroups != 1 {
				t.Fatalf("ExactGroups = %d, want preserved completed-stage metric", stats.ExactGroups)
			}
			if failureIndex > 1 && stats.ImageFeatures != 1 {
				t.Fatalf("ImageFeatures = %d, want preserved completed-stage metric", stats.ImageFeatures)
			}
			if failureIndex > 2 && stats.ImagePairs != 1 {
				t.Fatalf("ImagePairs = %d, want preserved completed-stage metric", stats.ImagePairs)
			}
			if failureIndex > 3 && stats.VideoFeatures != 1 {
				t.Fatalf("VideoFeatures = %d, want preserved completed-stage metric", stats.VideoFeatures)
			}
			if failureIndex > 4 && stats.VideoPairs != 1 {
				t.Fatalf("VideoPairs = %d, want preserved completed-stage metric", stats.VideoPairs)
			}
			if failureStage == "db_write" {
				if stats.GroupsWritten != 0 || stats.MembersWritten != 0 || stats.SkippedPairs != 0 {
					t.Fatalf("db_write error stats = %#v, want write counters left at zero", stats)
				}
				if strings.Contains(logOutput.String(), "candidate pairs skipped") {
					t.Fatalf("db_write error emitted skipped warning:\n%s", logOutput.String())
				}
			}
			assertAnalyzerStageKeys(t, stats)
			if stats.HeapAllocBytes == 0 {
				t.Fatal("HeapAllocBytes = 0 on failure")
			}
		})
	}
}

func TestAnalyzerBadRowsAreLocalToEachRun(t *testing.T) {
	sha1, sha2 := testAnalyzerSHA(1), testAnalyzerSHA(2)
	store := &fakeAnalyzerStore{
		files: []analyzerFileRow{
			{sha: sha1, file: FileRef{ID: 1}},
			{sha: sha1, file: FileRef{ID: 2}},
		},
		imageFeatures: []ImageFeature{{SHA512: sha1}},
		videoFeatures: []VideoFeature{{SHA512: sha2}},
		imageBadRows:  2,
		videoBadRows:  3,
	}
	analyzer := newAnalyzer(store, DefaultConfig(), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))

	first, err := analyzer.Run(context.Background())
	if err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	second, err := analyzer.Run(context.Background())
	if err != nil {
		t.Fatalf("second Run() error = %v", err)
	}

	if first.BadRows != 5 {
		t.Fatalf("first BadRows = %d, want 5", first.BadRows)
	}
	if second.BadRows != 5 {
		t.Fatalf("second BadRows = %d, want only second-run delta 5", second.BadRows)
	}
	if store.BadRows() != 10 {
		t.Fatalf("cumulative fake BadRows = %d, want 10", store.BadRows())
	}
}

func TestAnalyzerEarlyFailureDoesNotLeakPreviousRunBadRows(t *testing.T) {
	sha1, sha2 := testAnalyzerSHA(1), testAnalyzerSHA(2)
	store := &fakeAnalyzerStore{
		files: []analyzerFileRow{
			{sha: sha1, file: FileRef{ID: 1}},
			{sha: sha1, file: FileRef{ID: 2}},
		},
		imageFeatures: []ImageFeature{{SHA512: sha1}},
		videoFeatures: []VideoFeature{{SHA512: sha2}},
		imageBadRows:  2,
		videoBadRows:  3,
	}
	analyzer := newAnalyzer(store, DefaultConfig(), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))

	first, err := analyzer.Run(context.Background())
	if err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	if first.BadRows != 5 {
		t.Fatalf("first BadRows = %d, want 5", first.BadRows)
	}

	store.failStage = "exact_group"
	second, err := analyzer.Run(context.Background())
	if !errors.Is(err, errAnalyzerStage) {
		t.Fatalf("second Run() error = %v, want injected exact_group failure", err)
	}
	if second.BadRows != 0 {
		t.Fatalf("second BadRows = %d, want zero without current-run loads", second.BadRows)
	}
}

func TestAnalyzerDBWriteErrorDoesNotTrustReturnedCountsOrWarnSkipped(t *testing.T) {
	sha1 := testAnalyzerSHA(1)
	store := &fakeAnalyzerStore{
		failStage: "db_write",
		files: []analyzerFileRow{
			{sha: sha1, file: FileRef{ID: 1}},
			{sha: sha1, file: FileRef{ID: 2}},
		},
	}
	var logOutput bytes.Buffer
	analyzer := newAnalyzer(store, DefaultConfig(), slog.New(slog.NewTextHandler(&logOutput, nil)))

	stats, err := analyzer.Run(context.Background())
	if !errors.Is(err, errAnalyzerStage) {
		t.Fatalf("Run() error = %v, want injected db_write failure", err)
	}
	if !strings.Contains(err.Error(), "db_write:") {
		t.Fatalf("Run() error = %q, want db_write stage prefix", err)
	}
	if stats.GroupsWritten != 0 || stats.MembersWritten != 0 || stats.SkippedPairs != 0 {
		t.Errorf("db_write error stats = %#v, want write counters left at zero", stats)
	}
	if strings.Contains(logOutput.String(), "candidate pairs skipped") {
		t.Errorf("db_write error emitted skipped warning:\n%s", logOutput.String())
	}
}

func assertAnalyzerStageKeys(t *testing.T, stats *RunStats) {
	t.Helper()
	if len(stats.StageElapsedMs) != len(analyzerStageNames) {
		t.Fatalf("StageElapsedMs = %v, want exactly six stages", stats.StageElapsedMs)
	}
	for _, stage := range analyzerStageNames {
		if _, ok := stats.StageElapsedMs[stage]; !ok {
			t.Errorf("StageElapsedMs missing %q: %v", stage, stats.StageElapsedMs)
		}
	}
}

func testAnalyzerSHA(value byte) [64]byte {
	var sha [64]byte
	sha[0] = value
	return sha
}
