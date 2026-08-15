package localanalysis

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"

	"dedup/internal/config"
	"dedup/internal/features"
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
	CurrentLocalAnalysis(context.Context, string) (store.LocalAnalysisRun, error)
	ListLocalPairScoresForRun(context.Context, string) ([]store.LocalPairScore, error)
	SaveLocalPairScore(context.Context, store.LocalPairScore) error
	ReplaceLocalAnalysisGroups(context.Context, string, []store.LocalAnalysisGroup) error
	CompleteLocalAnalysis(context.Context, string) error
	PublishLocalAnalysis(context.Context, string) error
	EnqueueLocalEvent(context.Context, store.LocalOutboxEvent) error
}

type AnalysisProgress struct {
	Phase           string
	Complete        int64
	Total           int64
	TotalKnown      bool
	CheckpointStage int
}

var ErrDrainRequested = errors.New("local_analysis_drain_requested")

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
	return e.RunWithProgress(ctx, taskID, nil, nil)
}

func (e *Engine) RunWithProgress(ctx context.Context, taskID string, drain <-chan struct{}, report func(AnalysisProgress) error) error {
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
	if run.MachineID != e.machineID || run.TaskID != taskID {
		return fmt.Errorf("localanalysis: run identity mismatch")
	}
	switch run.Status {
	case "complete":
		if err := reportAnalysisProgress(report, AnalysisProgress{Phase: "finalizing", TotalKnown: false, CheckpointStage: 3}); err != nil {
			return err
		}
		if drainRequested(drain) {
			return ErrDrainRequested
		}
		if err := e.store.PublishLocalAnalysis(ctx, run.RunID); err != nil {
			return fmt.Errorf("localanalysis: publish completed run: %w", err)
		}
		return reportAnalysisProgress(report, AnalysisProgress{Phase: "finalizing", Complete: 1, Total: 1, TotalKnown: true, CheckpointStage: 3})
	case "published":
		current, err := e.store.CurrentLocalAnalysis(ctx, e.machineID)
		if err != nil {
			return fmt.Errorf("localanalysis: verify published run: %w", err)
		}
		if current.RunID != run.RunID || current.MachineID != e.machineID {
			return fmt.Errorf("localanalysis: published run is not current")
		}
		return reportAnalysisProgress(report, AnalysisProgress{Phase: "finalizing", Complete: 1, Total: 1, TotalKnown: true, CheckpointStage: 3})
	case "building":
	default:
		return fmt.Errorf("localanalysis: unsupported run status %q", run.Status)
	}
	existingRows, err := e.store.ListLocalPairScoresForRun(ctx, run.RunID)
	if err != nil {
		return fmt.Errorf("localanalysis: list durable pair scores: %w", err)
	}
	existing := make(map[string]store.LocalPairScore, len(existingRows))
	for _, pair := range existingRows {
		if pair.RunID == run.RunID {
			existing[pair.PairKey] = pair
		}
	}
	if err := reportAnalysisProgress(report, AnalysisProgress{Phase: "stage1", TotalKnown: false, CheckpointStage: 1}); err != nil {
		return err
	}
	if drainRequested(drain) {
		return ErrDrainRequested
	}
	result, err := e.stageOne.Run(ctx, e.machineID, run.RunID)
	if err != nil {
		return fmt.Errorf("localanalysis: stage one: %w", err)
	}
	if err = e.event(ctx, run, "stage1", map[string]int{"files": len(result.Files), "pairs": len(result.CandidatePairs), "exact_groups": len(result.ExactGroups)}); err != nil {
		return err
	}
	if err := reportAnalysisProgress(report, AnalysisProgress{Phase: "stage1", Complete: 1, Total: 1, TotalKnown: true, CheckpointStage: 1}); err != nil {
		return err
	}
	if drainRequested(drain) {
		return ErrDrainRequested
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
	type stagedPair struct {
		category  string
		kind      worker.MediaKind
		left      firstscreen.File
		right     firstscreen.File
		persisted store.LocalPairScore
	}
	stage2Passed := make([]stagedPair, 0, len(result.CandidatePairs))
	persistedStage2 := int64(0)
	for _, pair := range result.CandidatePairs {
		if saved, ok := existing[pairKey(pair)]; ok && saved.Stage2JSON != nil {
			persistedStage2++
		}
	}
	if err := reportAnalysisProgress(report, AnalysisProgress{Phase: "stage2", Complete: persistedStage2, Total: int64(len(result.CandidatePairs)), TotalKnown: true, CheckpointStage: 2}); err != nil {
		return err
	}
	for _, pair := range result.CandidatePairs {
		if drainRequested(drain) {
			return ErrDrainRequested
		}
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
		key := pairKey(pair)
		if saved, ok := existing[key]; ok && saved.Stage2JSON != nil {
			stage2Verdict, err := storedStageVerdict(saved.Stage2JSON)
			if err != nil {
				return fmt.Errorf("localanalysis: decode durable stage two verdict: %w", err)
			}
			if stage2Verdict == "yes" {
				stage2Passed = append(stage2Passed, stagedPair{category: category, kind: kind, left: leftFiles[0], right: rightFiles[0], persisted: saved})
			}
			continue
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
		stage1JSON, err := json.Marshal(map[string]any{"kind": pair.Kind, "hamming": pair.Hamming, "quality_a": pair.QualityA, "quality_b": pair.QualityB, "duration_diff_ms": pair.DurationDiffMs})
		if err != nil {
			return fmt.Errorf("localanalysis: marshal stage one: %w", err)
		}
		stage2JSON, err := json.Marshal(stage2)
		if err != nil {
			return fmt.Errorf("localanalysis: marshal stage two: %w", err)
		}
		localPair := store.LocalPairScore{RunID: run.RunID, PairKey: key, LeftFileID: leftFiles[0].ID, RightFileID: rightFiles[0].ID, LeftSHA512: hex.EncodeToString(pair.ShaA[:]), RightSHA512: hex.EncodeToString(pair.ShaB[:]), Stage1JSON: string(stage1JSON), Stage2JSON: stringPointer(string(stage2JSON)), Verdict: localVerdict(stage2.Verdict)}
		if err := e.store.SaveLocalPairScore(ctx, localPair); err != nil {
			return fmt.Errorf("localanalysis: save stage two: %w", err)
		}
		existing[key] = localPair
		persistedStage2++
		if err := reportAnalysisProgress(report, AnalysisProgress{Phase: "stage2", Complete: persistedStage2, Total: int64(len(result.CandidatePairs)), TotalKnown: true, CheckpointStage: 2}); err != nil {
			return err
		}
		if stage2.Verdict == phase2.VerdictYes {
			stage2Passed = append(stage2Passed, stagedPair{category: category, kind: kind, left: leftFiles[0], right: rightFiles[0], persisted: localPair})
		}
	}
	if err = e.event(ctx, run, "stage2", map[string]int{"pairs": len(result.CandidatePairs)}); err != nil {
		return err
	}

	decisions := make([]PairDecision, 0, len(stage2Passed))
	persistedStage3 := int64(0)
	for _, staged := range stage2Passed {
		if staged.persisted.Stage3JSON != nil {
			persistedStage3++
		}
	}
	if err := reportAnalysisProgress(report, AnalysisProgress{Phase: "stage3", Complete: persistedStage3, Total: int64(len(stage2Passed)), TotalKnown: true, CheckpointStage: 3}); err != nil {
		return err
	}
	for _, staged := range stage2Passed {
		if drainRequested(drain) {
			return ErrDrainRequested
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if staged.persisted.Stage3JSON != nil {
			stage3Verdict, err := storedStageVerdict(staged.persisted.Stage3JSON)
			if err != nil {
				return fmt.Errorf("localanalysis: decode durable stage three verdict: %w", err)
			}
			decisions = append(decisions, PairDecision{Category: staged.category, SHAA: staged.persisted.LeftSHA512, SHAB: staged.persisted.RightSHA512, Verdict: stage3Verdict})
			continue
		}
		left3, err := e.compute(ctx, run.TaskID, staged.left, staged.kind, worker.ScreenStageThree)
		if err != nil {
			return err
		}
		right3, err := e.compute(ctx, run.TaskID, staged.right, staged.kind, worker.ScreenStageThree)
		if err != nil {
			return err
		}
		stage3 := judgeResult(staged.kind, worker.ScreenStageThree, left3, right3, e.cfg)
		stage3JSON, err := json.Marshal(stage3)
		if err != nil {
			return fmt.Errorf("localanalysis: marshal stage three: %w", err)
		}
		localPair := staged.persisted
		localPair.Stage3JSON = stringPointer(string(stage3JSON))
		localPair.Verdict = localVerdict(stage3.Verdict)
		if err := e.store.SaveLocalPairScore(ctx, localPair); err != nil {
			return fmt.Errorf("localanalysis: save stage three: %w", err)
		}
		existing[localPair.PairKey] = localPair
		persistedStage3++
		if err := reportAnalysisProgress(report, AnalysisProgress{Phase: "stage3", Complete: persistedStage3, Total: int64(len(stage2Passed)), TotalKnown: true, CheckpointStage: 3}); err != nil {
			return err
		}
		decisions = append(decisions, PairDecision{Category: staged.category, SHAA: localPair.LeftSHA512, SHAB: localPair.RightSHA512, Verdict: verdictText(stage3.Verdict)})
	}
	if err = e.event(ctx, run, "stage3", map[string]int{"pairs": len(stage2Passed)}); err != nil {
		return err
	}
	if drainRequested(drain) {
		return ErrDrainRequested
	}
	if err := reportAnalysisProgress(report, AnalysisProgress{Phase: "finalizing", TotalKnown: false, CheckpointStage: 3}); err != nil {
		return err
	}
	if drainRequested(drain) {
		return ErrDrainRequested
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
	return reportAnalysisProgress(report, AnalysisProgress{Phase: "finalizing", Complete: 1, Total: 1, TotalKnown: true, CheckpointStage: 3})
}

func reportAnalysisProgress(report func(AnalysisProgress) error, progress AnalysisProgress) error {
	if report == nil {
		return nil
	}
	return report(progress)
}

func drainRequested(drain <-chan struct{}) bool {
	if drain == nil {
		return false
	}
	select {
	case <-drain:
		return true
	default:
		return false
	}
}

func storedStageVerdict(raw *string) (string, error) {
	if raw == nil {
		return "", fmt.Errorf("missing stage JSON")
	}
	var payload struct {
		Verdict string `json:"verdict"`
	}
	if err := json.Unmarshal([]byte(*raw), &payload); err != nil {
		return "", err
	}
	switch payload.Verdict {
	case "yes", "no", "inconclusive":
		return payload.Verdict, nil
	default:
		return "", fmt.Errorf("invalid stage verdict %q", payload.Verdict)
	}
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
	if result == nil || job.JobID <= 0 || result.JobID != job.JobID || result.ScanTaskID != job.ScanTaskID ||
		result.Path != job.Path || result.Kind != job.Kind || result.Phase != job.Phase ||
		result.ScreenStage != job.ScreenStage || result.Source != job.Source ||
		!bytes.Equal(result.SHA512, job.KnownSHA) {
		return nil, fmt.Errorf("localanalysis: worker result identity mismatch")
	}
	if len(result.Errors) > 0 {
		return nil, fmt.Errorf("localanalysis: worker stage %d failed", stage)
	}
	if err := validateStagePayload(job, result); err != nil {
		return nil, fmt.Errorf("localanalysis: worker stage %d payload invalid", stage)
	}
	return result, nil
}

func validateStagePayload(job *worker.JobMsg, result *worker.JobResultMsg) error {
	switch job.Kind {
	case worker.MediaImage:
		if result.FieldsDone != job.FieldsMask || result.FramesDone != 0 || len(result.Frames) != 0 {
			return fmt.Errorf("image contains video frames")
		}
		switch job.ScreenStage {
		case worker.ScreenStageTwo:
			if _, err := features.DecodePHashParts(result.PHashParts); err != nil || len(result.SobelHist) != 0 {
				return fmt.Errorf("stage two image payload mismatch")
			}
		case worker.ScreenStageThree:
			if _, err := features.DecodeSobelHist(result.SobelHist); err != nil || len(result.PHashParts) != 0 {
				return fmt.Errorf("stage three image payload mismatch")
			}
		default:
			return fmt.Errorf("invalid image stage")
		}
	case worker.MediaVideo:
		if result.FieldsDone&^job.FieldsMask != 0 || result.FramesDone&^job.FrameMask != 0 {
			return fmt.Errorf("video coverage contains extra bits")
		}
		if result.FramesDone == job.FrameMask {
			if result.FieldsDone != job.FieldsMask {
				return fmt.Errorf("complete video field coverage mismatch")
			}
		} else if result.FieldsDone != 0 {
			return fmt.Errorf("partial video contains completed field")
		}
		if len(result.PHashParts) != 0 || len(result.SobelHist) != 0 {
			return fmt.Errorf("video contains image payload")
		}
		seen := make(map[int]struct{}, len(result.Frames))
		for _, frame := range result.Frames {
			if frame.FrameIdx < 0 || frame.FrameIdx >= 6 || job.FrameMask&(1<<uint(frame.FrameIdx)) == 0 {
				return fmt.Errorf("video frame identity mismatch")
			}
			if _, exists := seen[frame.FrameIdx]; exists {
				return fmt.Errorf("duplicate video frame")
			}
			seen[frame.FrameIdx] = struct{}{}
			done := result.FramesDone&(1<<uint(frame.FrameIdx)) != 0
			if !done {
				if frame.Error == "" {
					return fmt.Errorf("unsuccessful video frame lacks error")
				}
				if len(frame.PDQ256) != 0 || frame.Quality != 0 || len(frame.PHashParts) != 0 || len(frame.SobelHist) != 0 {
					return fmt.Errorf("errored video frame contains payload")
				}
				continue
			}
			if frame.Error != "" {
				return fmt.Errorf("successful video frame contains error")
			}
			switch job.ScreenStage {
			case worker.ScreenStageTwo:
				if _, err := features.DecodePHashParts(frame.PHashParts); err != nil || len(frame.SobelHist) != 0 || len(frame.PDQ256) != 0 || frame.Quality != 0 {
					return fmt.Errorf("stage two video payload mismatch")
				}
			case worker.ScreenStageThree:
				if _, err := features.DecodeSobelHist(frame.SobelHist); err != nil || len(frame.PHashParts) != 0 || len(frame.PDQ256) != 0 || frame.Quality != 0 {
					return fmt.Errorf("stage three video payload mismatch")
				}
			default:
				return fmt.Errorf("invalid video stage")
			}
		}
		for index := 0; index < 6; index++ {
			if job.FrameMask&(1<<uint(index)) != 0 {
				if _, exists := seen[index]; !exists {
					return fmt.Errorf("video frame missing")
				}
			}
		}
	default:
		return fmt.Errorf("invalid media kind")
	}
	return nil
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
