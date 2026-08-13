package firstscreen

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
)

// File is the machine-scoped file identity consumed by local first screening.
type File struct {
	FileRef
	SHA512 [64]byte
}

// Result is a self-contained first-screen snapshot for one local run.
type Result struct {
	Files          []File
	ExactGroups    []ExactGroup
	ExactVerdicts  map[[64]byte]string
	CandidatePairs []CandidatePair
}

// CandidateSource supplies only active files and their phase-one features.
type CandidateSource interface {
	StreamActiveFiles(context.Context, string, func(File) error) error
	LoadImageFeatures(context.Context, []string) (map[string]ImageFeature, error)
	LoadVideoFeatures(context.Context, []string) (map[string]VideoFeature, error)
}

// CandidateSink persists a snapshot into the explicitly requested local run.
type CandidateSink interface {
	ReplaceStageOne(context.Context, string, Result) error
}

// CandidateAnalyzer reuses the established first-screen algorithm with a
// machine-scoped source and a local run sink.
type CandidateAnalyzer struct {
	source CandidateSource
	sink   CandidateSink
	cfg    Config
	log    *slog.Logger
}

func NewCandidateAnalyzer(source CandidateSource, sink CandidateSink, cfg Config, log *slog.Logger) *CandidateAnalyzer {
	if log == nil {
		log = slog.Default()
	}
	return &CandidateAnalyzer{source: source, sink: sink, cfg: cfg, log: log}
}

func (a *CandidateAnalyzer) Run(ctx context.Context, machineID, runID string) (Result, error) {
	if machineID == "" || runID == "" {
		return Result{}, fmt.Errorf("firstscreen: local candidate run requires machine and run ID")
	}
	if a.source == nil || a.sink == nil {
		return Result{}, fmt.Errorf("firstscreen: local candidate source and sink are required")
	}
	if err := a.cfg.Validate(); err != nil {
		return Result{}, err
	}

	files := make([]File, 0)
	if err := a.source.StreamActiveFiles(ctx, machineID, func(file File) error {
		if file.MachineID == machineID {
			files = append(files, file)
		}
		return nil
	}); err != nil {
		return Result{}, fmt.Errorf("firstscreen: stream local active files: %w", err)
	}
	sort.Slice(files, func(i, j int) bool {
		if order := bytes.Compare(files[i].SHA512[:], files[j].SHA512[:]); order != 0 {
			return order < 0
		}
		return files[i].ID < files[j].ID
	})

	collector := &exactCollector{}
	shaSet := make(map[[64]byte]struct{}, len(files))
	for _, file := range files {
		collector.add(file.SHA512, file.FileRef)
		shaSet[file.SHA512] = struct{}{}
	}
	shaTexts := make([]string, 0, len(shaSet))
	for sha := range shaSet {
		shaTexts = append(shaTexts, hex.EncodeToString(sha[:]))
	}
	sort.Strings(shaTexts)

	images, err := a.source.LoadImageFeatures(ctx, shaTexts)
	if err != nil {
		return Result{}, fmt.Errorf("firstscreen: load local image features: %w", err)
	}
	videos, err := a.source.LoadVideoFeatures(ctx, shaTexts)
	if err != nil {
		return Result{}, fmt.Errorf("firstscreen: load local video features: %w", err)
	}
	imageFeatures := featuresForActiveSHAs(images, shaSet)
	videoFeatures := videoFeaturesForActiveSHAs(videos, shaSet)
	imagePairs := screenImages(imageFeatures, a.cfg.HammingMax, a.cfg.AspectTolerance, a.cfg.ImageQualityMin)
	videoPairs := screenVideos(videoFeatures, a.cfg.VideoDurationWindowMs, a.cfg.HammingMax)
	pairs := append(imagePairs, videoPairs...)
	exact := collector.finish()
	exactVerdicts := make(map[[64]byte]string, len(exact))
	for _, group := range exact {
		exactVerdicts[group.SHA512] = "yes"
	}
	result := Result{Files: files, ExactGroups: exact, ExactVerdicts: exactVerdicts, CandidatePairs: pairs}
	if err := a.sink.ReplaceStageOne(ctx, runID, result); err != nil {
		return Result{}, fmt.Errorf("firstscreen: replace local stage one: %w", err)
	}
	a.log.Info("local first-screen candidates generated", "machine_id", machineID, "files", len(files), "exact_groups", len(result.ExactGroups), "pairs", len(pairs))
	return result, nil
}

func featuresForActiveSHAs(features map[string]ImageFeature, active map[[64]byte]struct{}) []ImageFeature {
	result := make([]ImageFeature, 0, len(features))
	for _, feature := range features {
		if _, ok := active[feature.SHA512]; ok {
			result = append(result, feature)
		}
	}
	sort.Slice(result, func(i, j int) bool { return bytes.Compare(result[i].SHA512[:], result[j].SHA512[:]) < 0 })
	return result
}

func videoFeaturesForActiveSHAs(features map[string]VideoFeature, active map[[64]byte]struct{}) []VideoFeature {
	result := make([]VideoFeature, 0, len(features))
	for _, feature := range features {
		if _, ok := active[feature.SHA512]; ok {
			result = append(result, feature)
		}
	}
	return result
}
