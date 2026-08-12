package localanalysis

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"dedup/internal/config"
	"dedup/internal/firstscreen"
	"dedup/internal/phase2"
	"dedup/internal/proto"
	"dedup/internal/store"
	"dedup/internal/worker"
)

type StageOneRunner interface {
	Run(context.Context, string, string) (firstscreen.Result, error)
}

type StageWorker interface {
	Execute(context.Context, *worker.JobMsg) (*worker.JobResultMsg, error)
}

type EngineStore interface {
	BeginLocalAnalysis(context.Context, string, string) (store.LocalAnalysisRun, error)
	SaveLocalPairScore(context.Context, store.LocalPairScore) error
	ReplaceLocalAnalysisGroups(context.Context, string, []store.LocalAnalysisGroup) error
	CompleteLocalAnalysis(context.Context, string) error
	PublishLocalAnalysis(context.Context, string) error
	EnqueueLocalEvent(context.Context, store.LocalOutboxEvent) error
}

type Engine struct {
	machineID    string
	stageOne     StageOneRunner
	store        EngineStore
	worker       StageWorker
	cfg          config.Phase2Config
	fileMetadata func(string) (int64, int64, error)
}

func NewEngine(machineID string, stageOne StageOneRunner, analysisStore EngineStore, stageWorker StageWorker, cfg config.Phase2Config) *Engine {
	return &Engine{machineID: machineID, stageOne: stageOne, store: analysisStore, worker: stageWorker, cfg: cfg, fileMetadata: func(path string) (int64, int64, error) {
		info, err := os.Stat(path)
		if err != nil {
			return 0, 0, err
		}
		return info.Size(), info.ModTime().UnixMilli(), nil
	}}
}

func (e *Engine) Run(ctx context.Context, taskID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e == nil || e.machineID == "" || taskID == "" || e.stageOne == nil || e.store == nil || e.worker == nil {
		return fmt.Errorf("localanalysis: engine dependencies are required")
	}
	run, err := e.store.BeginLocalAnalysis(ctx, e.machineID, taskID)
	if err != nil {
		return fmt.Errorf("localanalysis: begin run: %w", err)
	}
	if run.MachineID != e.machineID || run.TaskID != taskID || run.Status != "building" {
		return fmt.Errorf("localanalysis: building run identity mismatch")
	}
	result, err := e.stageOne.Run(ctx, e.machineID, run.RunID)
	if err != nil {
		return fmt.Errorf("localanalysis: stage one: %w", err)
	}
	if err = e.event(ctx, run, "stage1", map[string]int{"files": len(result.Files), "pairs": len(result.CandidatePairs), "exact_groups": len(result.ExactGroups)}); err != nil {
		return err
	}

	filesBySHA := make(map[[64]byte][]firstscreen.File)
	quality := make(map[[64]byte]int)
	for _, file := range result.Files {
		filesBySHA[file.SHA512] = append(filesBySHA[file.SHA512], file)
	}
	for sha := range filesBySHA {
		sort.Slice(filesBySHA[sha], func(i, j int) bool {
			if filesBySHA[sha][i].Path != filesBySHA[sha][j].Path {
				return filesBySHA[sha][i].Path < filesBySHA[sha][j].Path
			}
			return filesBySHA[sha][i].ID < filesBySHA[sha][j].ID
		})
	}
	decisions := make([]PairDecision, 0, len(result.CandidatePairs))
	stage2Count, stage3Count := 0, 0
	for _, pair := range result.CandidatePairs {
		if err := ctx.Err(); err != nil {
			return err
		}
		leftFiles, rightFiles := filesBySHA[pair.ShaA], filesBySHA[pair.ShaB]
		if len(leftFiles) == 0 || len(rightFiles) == 0 {
			return fmt.Errorf("localanalysis: candidate endpoint is missing")
		}
		quality[pair.ShaA], quality[pair.ShaB] = pair.QualityA, pair.QualityB
		category, kind, err := candidateKind(pair.Kind)
		if err != nil {
			return err
		}
		left2, err := e.compute(ctx, run.TaskID, leftFiles[0], kind, worker.ScreenStageTwo)
		if err != nil {
			return err
		}
		right2, err := e.compute(ctx, run.TaskID, rightFiles[0], kind, worker.ScreenStageTwo)
		if err != nil {
			return err
		}
		stage2 := judgeResult(kind, worker.ScreenStageTwo, left2, right2, e.cfg)
		stage2Count++
		stage1JSON, _ := json.Marshal(map[string]any{"kind": pair.Kind, "hamming": pair.Hamming, "quality_a": pair.QualityA, "quality_b": pair.QualityB, "duration_diff_ms": pair.DurationDiffMs})
		stage2JSON, _ := json.Marshal(stage2)
		localPair := store.LocalPairScore{RunID: run.RunID, PairKey: pairKey(pair), LeftFileID: leftFiles[0].ID, RightFileID: rightFiles[0].ID, LeftSHA512: hex.EncodeToString(pair.ShaA[:]), RightSHA512: hex.EncodeToString(pair.ShaB[:]), Stage1JSON: string(stage1JSON), Stage2JSON: stringPointer(string(stage2JSON)), Verdict: "undecided"}
		if stage2.Verdict != phase2.VerdictYes {
			localPair.Verdict = localVerdict(stage2.Verdict)
			if err := e.store.SaveLocalPairScore(ctx, localPair); err != nil {
				return fmt.Errorf("localanalysis: save stage two: %w", err)
			}
			continue
		}
		left3, err := e.compute(ctx, run.TaskID, leftFiles[0], kind, worker.ScreenStageThree)
		if err != nil {
			return err
		}
		right3, err := e.compute(ctx, run.TaskID, rightFiles[0], kind, worker.ScreenStageThree)
		if err != nil {
			return err
		}
		stage3 := judgeResult(kind, worker.ScreenStageThree, left3, right3, e.cfg)
		stage3Count++
		stage3JSON, _ := json.Marshal(stage3)
		localPair.Stage3JSON = stringPointer(string(stage3JSON))
		localPair.Verdict = localVerdict(stage3.Verdict)
		if err := e.store.SaveLocalPairScore(ctx, localPair); err != nil {
			return fmt.Errorf("localanalysis: save stage three: %w", err)
		}
		decisions = append(decisions, PairDecision{Category: category, SHAA: localPair.LeftSHA512, SHAB: localPair.RightSHA512, Verdict: verdictText(stage3.Verdict)})
	}
	if err = e.event(ctx, run, "stage2", map[string]int{"pairs": stage2Count}); err != nil {
		return err
	}
	if err = e.event(ctx, run, "stage3", map[string]int{"pairs": stage3Count}); err != nil {
		return err
	}
	groupFiles := make([]GroupFile, 0, len(result.Files))
	for _, file := range result.Files {
		groupFiles = append(groupFiles, GroupFile{FileID: file.ID, SHA512: hex.EncodeToString(file.SHA512[:]), Path: file.Path, Quality: quality[file.SHA512]})
	}
	groups, err := BuildFinalGroups(run.RunID, groupFiles, decisions)
	if err != nil {
		return err
	}
	persisted := make([]store.LocalAnalysisGroup, len(groups))
	for i, group := range groups {
		persisted[i] = store.LocalAnalysisGroup{GroupID: group.GroupID, Category: group.Category, RepresentativeFileID: group.RepresentativeFileID, Members: make([]store.LocalAnalysisMember, len(group.Members))}
		for j, m := range group.Members {
			persisted[i].Members[j] = store.LocalAnalysisMember{FileID: m.FileID, SHA512: m.SHA512}
		}
	}
	if err = e.store.ReplaceLocalAnalysisGroups(ctx, run.RunID, persisted); err != nil {
		return fmt.Errorf("localanalysis: replace final groups: %w", err)
	}
	if err = e.event(ctx, run, "final", map[string]int{"groups": len(groups)}); err != nil {
		return err
	}
	if err = e.store.CompleteLocalAnalysis(ctx, run.RunID); err != nil {
		return fmt.Errorf("localanalysis: complete run: %w", err)
	}
	if err = e.store.PublishLocalAnalysis(ctx, run.RunID); err != nil {
		return fmt.Errorf("localanalysis: publish run: %w", err)
	}
	return nil
}

func (e *Engine) compute(ctx context.Context, taskID string, file firstscreen.File, kind worker.MediaKind, stage worker.ScreenStage) (*worker.JobResultMsg, error) {
	size, mtimeMS, err := e.fileMetadata(file.Path)
	if err != nil {
		return nil, fmt.Errorf("localanalysis: stat media for stage %d failed", stage)
	}
	fields := worker.MaskPHashParts
	if stage == worker.ScreenStageThree {
		fields = worker.MaskSobelHist
	}
	frameMask := uint8(0)
	if kind == worker.MediaVideo {
		frameMask = worker.FrameMaskFull
		if stage == worker.ScreenStageTwo {
			fields = worker.MaskVideo6FPHash
		} else {
			fields = worker.MaskVideo6FSobel
		}
	}
	job := &worker.JobMsg{ScanTaskID: taskID, Path: file.Path, Kind: kind, Phase: worker.Phase2, ScreenStage: stage, Source: worker.JobSourceLocal, FieldsMask: fields, Size: size, MTimeMS: mtimeMS, KnownSHA: append([]byte(nil), file.SHA512[:]...), FrameMask: frameMask}
	result, err := e.worker.Execute(ctx, job)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("localanalysis: execute stage %d job: %s", stage, worker.RedactKnownPath(err.Error(), file.Path))
	}
	if result == nil || result.ScreenStage != stage || result.Source != worker.JobSourceLocal || result.Kind != kind {
		return nil, fmt.Errorf("localanalysis: worker result identity mismatch")
	}
	if len(result.Errors) > 0 {
		return nil, fmt.Errorf("localanalysis: worker stage %d failed", stage)
	}
	return result, nil
}

func judgeResult(kind worker.MediaKind, stage worker.ScreenStage, left, right *worker.JobResultMsg, cfg config.Phase2Config) phase2.StageScore {
	if kind == worker.MediaImage {
		if stage == worker.ScreenStageTwo {
			return phase2.JudgeImageStage2(left.PHashParts, right.PHashParts, cfg)
		}
		return phase2.JudgeImageStage3(left.SobelHist, right.SobelHist, cfg)
	}
	lf, rf := protoFrames(left.Frames), protoFrames(right.Frames)
	if stage == worker.ScreenStageTwo {
		return phase2.JudgeVideoStage2(lf, rf, cfg)
	}
	return phase2.JudgeVideoStage3(lf, rf, cfg)
}
func protoFrames(frames []worker.FrameFeature) []proto.FrameFeature {
	result := make([]proto.FrameFeature, len(frames))
	for i, f := range frames {
		result[i] = proto.FrameFeature{FrameIdx: f.FrameIdx, TimeMS: f.TimeMS, PDQ256: append([]byte(nil), f.PDQ256...), Quality: f.Quality, PHashParts: append([]byte(nil), f.PHashParts...), SobelHist: append([]byte(nil), f.SobelHist...), Error: f.Error}
	}
	return result
}
func candidateKind(kind string) (string, worker.MediaKind, error) {
	switch kind {
	case firstscreen.KindImageCandidate:
		return "image", worker.MediaImage, nil
	case firstscreen.KindVideoCandidate:
		return "video", worker.MediaVideo, nil
	default:
		return "", 0, fmt.Errorf("localanalysis: invalid candidate kind %q", kind)
	}
}
func pairKey(pair firstscreen.CandidatePair) string {
	return pair.Kind + ":" + hex.EncodeToString(pair.ShaA[:]) + ":" + hex.EncodeToString(pair.ShaB[:])
}
func localVerdict(v phase2.Verdict) string {
	switch v {
	case phase2.VerdictYes:
		return "duplicate"
	case phase2.VerdictNo:
		return "not_duplicate"
	default:
		return "uncertain"
	}
}
func verdictText(v phase2.Verdict) string {
	switch v {
	case phase2.VerdictYes:
		return "yes"
	case phase2.VerdictNo:
		return "no"
	default:
		return "inconclusive"
	}
}
func stringPointer(v string) *string { return &v }
func (e *Engine) event(ctx context.Context, run store.LocalAnalysisRun, stage string, counts map[string]int) error {
	payload, _ := json.Marshal(map[string]any{"run_id": run.RunID, "stage": stage, "counts": counts})
	if err := e.store.EnqueueLocalEvent(ctx, store.LocalOutboxEvent{Topic: "local_analysis.stage", EntityKey: run.RunID + ":" + stage, Generation: run.Generation, PayloadJSON: string(payload)}); err != nil {
		return fmt.Errorf("localanalysis: enqueue %s event: %w", stage, err)
	}
	return nil
}
