package phase2

import (
	"fmt"
	"math"

	"dedup/internal/config"
	"dedup/internal/features"
	"dedup/internal/proto"
)

// Config is the shared three-stage threshold configuration.
type Config = config.Phase2Config

// StageScore is the stable, display-safe result of one independent screen.
type StageScore struct {
	Verdict      Verdict            `json:"verdict"`
	Reason       string             `json:"reason"`
	PassRatio    float64            `json:"pass_ratio,omitempty"`
	Similarity   float64            `json:"similarity,omitempty"`
	ValidFrames  int                `json:"valid_frames,omitempty"`
	PassedFrames int                `json:"passed_frames,omitempty"`
	Frames       [6]StageFrameScore `json:"frames,omitempty"`
}

type StageFrameScore struct {
	Valid      bool    `json:"valid"`
	PassRatio  float64 `json:"pass_ratio,omitempty"`
	Similarity float64 `json:"similarity,omitempty"`
	Passed     bool    `json:"passed"`
}

func JudgeImageStage2(a, b []byte, cfg Config) StageScore {
	if err := validateJudgeConfig(cfg); err != nil {
		return StageScore{Verdict: VerdictInconclusive, Reason: "invalid_config"}
	}
	left, err := features.DecodePHashParts(a)
	if err != nil {
		return StageScore{Verdict: VerdictInconclusive, Reason: "invalid_phash"}
	}
	right, err := features.DecodePHashParts(b)
	if err != nil {
		return StageScore{Verdict: VerdictInconclusive, Reason: "invalid_phash"}
	}
	ratio := pHashPassRatio(left, right, cfg.PHashPartThreshold)
	if ratio < cfg.PHashPassT2 {
		return StageScore{Verdict: VerdictNo, Reason: "phash_below_threshold", PassRatio: ratio}
	}
	return StageScore{Verdict: VerdictYes, Reason: "phash_passed", PassRatio: ratio}
}

func JudgeImageStage3(a, b []byte, cfg Config) StageScore {
	if err := validateJudgeConfig(cfg); err != nil {
		return StageScore{Verdict: VerdictInconclusive, Reason: "invalid_config"}
	}
	left, err := features.DecodeSobelHist(a)
	if err != nil || validateSobelHist(left, "left image") != nil {
		return StageScore{Verdict: VerdictInconclusive, Reason: "invalid_sobel"}
	}
	right, err := features.DecodeSobelHist(b)
	if err != nil || validateSobelHist(right, "right image") != nil {
		return StageScore{Verdict: VerdictInconclusive, Reason: "invalid_sobel"}
	}
	similarity := features.SobelCosine(left, right)
	if similarity < cfg.SobelT3 {
		return StageScore{Verdict: VerdictNo, Reason: "sobel_below_threshold", Similarity: similarity}
	}
	return StageScore{Verdict: VerdictYes, Reason: "sobel_passed", Similarity: similarity}
}

func JudgeVideoStage2(a, b []proto.FrameFeature, cfg Config) StageScore {
	return judgeVideoStage(a, b, cfg, true)
}

func JudgeVideoStage3(a, b []proto.FrameFeature, cfg Config) StageScore {
	return judgeVideoStage(a, b, cfg, false)
}

func judgeVideoStage(a, b []proto.FrameFeature, cfg Config, phash bool) StageScore {
	if err := validateJudgeConfig(cfg); err != nil {
		return StageScore{Verdict: VerdictInconclusive, Reason: "invalid_config"}
	}
	left, leftOK := indexedFrames(a)
	right, rightOK := indexedFrames(b)
	if !leftOK || !rightOK {
		return StageScore{Verdict: VerdictInconclusive, Reason: "invalid_frames"}
	}
	score := StageScore{}
	var sum float64
	for index := 0; index < cfg.VideoFrames; index++ {
		lf, lok := left[index]
		rf, rok := right[index]
		if !lok || !rok {
			continue
		}
		score.ValidFrames++
		score.Frames[index].Valid = true
		if phash {
			lp, err1 := features.DecodePHashParts(lf.PHashParts)
			rp, err2 := features.DecodePHashParts(rf.PHashParts)
			if err1 != nil || err2 != nil {
				return StageScore{Verdict: VerdictInconclusive, Reason: "invalid_phash"}
			}
			ratio := pHashPassRatio(lp, rp, cfg.PHashPartThreshold)
			score.Frames[index].PassRatio = ratio
			sum += ratio
			score.Frames[index].Passed = ratio >= cfg.PHashPassT2
			if score.Frames[index].Passed {
				score.PassedFrames++
			}
		} else {
			lh, err1 := features.DecodeSobelHist(lf.SobelHist)
			rh, err2 := features.DecodeSobelHist(rf.SobelHist)
			if err1 != nil || err2 != nil || validateSobelHist(lh, "left frame") != nil || validateSobelHist(rh, "right frame") != nil {
				return StageScore{Verdict: VerdictInconclusive, Reason: "invalid_sobel"}
			}
			sim := features.SobelCosine(lh, rh)
			score.Frames[index].Similarity = sim
			sum += sim
			score.Frames[index].Passed = sim >= cfg.SobelT3
			if score.Frames[index].Passed {
				score.PassedFrames++
			}
		}
	}
	if score.ValidFrames < cfg.VideoMinValid {
		score.Verdict, score.Reason = VerdictInconclusive, "insufficient_valid_frames"
		return score
	}
	score.Similarity = sum / float64(score.ValidFrames)
	if phash {
		if score.Similarity >= cfg.PHashPassT2 || score.PassedFrames >= cfg.VideoMinPassed {
			score.Verdict, score.Reason = VerdictYes, "phash_passed"
		} else {
			score.Verdict, score.Reason = VerdictNo, "phash_below_threshold"
		}
		return score
	}
	if score.Similarity >= cfg.VideoAvgT4 || score.PassedFrames >= cfg.VideoMinPassed {
		score.Verdict, score.Reason = VerdictYes, "sobel_passed"
	} else {
		score.Verdict, score.Reason = VerdictNo, "sobel_below_threshold"
	}
	return score
}

func indexedFrames(frames []proto.FrameFeature) (map[int]proto.FrameFeature, bool) {
	result := make(map[int]proto.FrameFeature, len(frames))
	for _, frame := range frames {
		if frame.FrameIdx < 0 || frame.FrameIdx >= 6 {
			return nil, false
		}
		if _, exists := result[frame.FrameIdx]; exists {
			return nil, false
		}
		result[frame.FrameIdx] = frame
	}
	return result, true
}

// Verdict is the explicit outcome of a phase-2 pair comparison.
type Verdict uint8

const (
	VerdictNo Verdict = iota
	VerdictYes
	VerdictInconclusive
)

// ImagePairScore contains every score used to judge an image pair.
type ImagePairScore struct {
	PHashPassRatio float64
	SobelCosine    float64
	SobelEvaluated bool
	Verdict        Verdict
}

// FramePhase2 is the decoded phase-2 feature set for one video frame.
type FramePhase2 struct {
	PDQ256     [32]byte
	Quality    int
	PHashParts [9]uint64
	SobelHist  [128]float32
}

// FrameScore contains the ordered comparison detail for one video frame.
type FrameScore struct {
	FrameIdx       int
	Valid          bool
	PHashPassRatio float64
	SobelCosine    float64
	SobelEvaluated bool
	Sim            float64
	Passed         bool
}

// VideoPairScore contains the six frame details and the conclusive aggregate,
// if enough valid frames exist.
type VideoPairScore struct {
	Frames           [6]FrameScore
	ValidFrames      int
	AvgSim           float64
	AverageEvaluated bool
	PassedFrames     int
	Verdict          Verdict
}

// JudgeImagePair applies the configured partition-pHash and Sobel thresholds.
func JudgeImagePair(
	aParts, bParts [9]uint64,
	aHist, bHist [128]float32,
	cfg config.Phase2Config,
) (ImagePairScore, error) {
	if err := validateJudgeConfig(cfg); err != nil {
		return ImagePairScore{}, err
	}
	if err := validateSobelHist(aHist, "left image"); err != nil {
		return ImagePairScore{}, err
	}
	if err := validateSobelHist(bHist, "right image"); err != nil {
		return ImagePairScore{}, err
	}

	stage2 := JudgeImageStage2(
		features.EncodePHashParts(aParts),
		features.EncodePHashParts(bParts),
		cfg,
	)
	score := ImagePairScore{PHashPassRatio: stage2.PassRatio, Verdict: VerdictNo}
	if stage2.Verdict != VerdictYes {
		return score, nil
	}

	aRaw, _ := features.EncodeSobelHist(aHist)
	bRaw, _ := features.EncodeSobelHist(bHist)
	stage3 := JudgeImageStage3(aRaw, bRaw, cfg)
	score.SobelEvaluated = true
	score.SobelCosine = stage3.Similarity
	score.Verdict = stage3.Verdict
	return score, nil
}

// JudgeVideoPair compares frames by index and excludes missing endpoints from
// the denominator. Present malformed frames and invalid configuration fail
// closed.
func JudgeVideoPair(
	aFrames, bFrames [6]*FramePhase2,
	cfg config.Phase2Config,
) (VideoPairScore, error) {
	if err := validateJudgeConfig(cfg); err != nil {
		return VideoPairScore{}, err
	}
	for index := 0; index < len(aFrames); index++ {
		if aFrames[index] != nil {
			if err := validateSobelHist(aFrames[index].SobelHist, fmt.Sprintf("left frame %d", index)); err != nil {
				return VideoPairScore{}, err
			}
		}
		if bFrames[index] != nil {
			if err := validateSobelHist(bFrames[index].SobelHist, fmt.Sprintf("right frame %d", index)); err != nil {
				return VideoPairScore{}, err
			}
		}
	}

	leftStage2, rightStage2 := make([]proto.FrameFeature, 0, len(aFrames)), make([]proto.FrameFeature, 0, len(bFrames))
	leftStage3, rightStage3 := make([]proto.FrameFeature, 0, len(aFrames)), make([]proto.FrameFeature, 0, len(bFrames))
	for index := range aFrames {
		if aFrames[index] != nil {
			aSobel, _ := features.EncodeSobelHist(aFrames[index].SobelHist)
			leftStage2 = append(leftStage2, proto.FrameFeature{FrameIdx: index, PHashParts: features.EncodePHashParts(aFrames[index].PHashParts)})
			leftStage3 = append(leftStage3, proto.FrameFeature{FrameIdx: index, SobelHist: aSobel})
		}
		if bFrames[index] != nil {
			bSobel, _ := features.EncodeSobelHist(bFrames[index].SobelHist)
			rightStage2 = append(rightStage2, proto.FrameFeature{FrameIdx: index, PHashParts: features.EncodePHashParts(bFrames[index].PHashParts)})
			rightStage3 = append(rightStage3, proto.FrameFeature{FrameIdx: index, SobelHist: bSobel})
		}
	}
	stage2 := JudgeVideoStage2(leftStage2, rightStage2, cfg)
	stage3 := JudgeVideoStage3(leftStage3, rightStage3, cfg)

	var score VideoPairScore
	var similaritySum float64
	for index := 0; index < len(score.Frames); index++ {
		frameScore := FrameScore{FrameIdx: index, Valid: stage2.Frames[index].Valid}
		if !frameScore.Valid {
			score.Frames[index] = frameScore
			continue
		}
		score.ValidFrames++
		frameScore.PHashPassRatio = stage2.Frames[index].PassRatio
		if stage2.Frames[index].Passed {
			frameScore.SobelEvaluated = true
			frameScore.SobelCosine = stage3.Frames[index].Similarity
			frameScore.Sim = frameScore.SobelCosine
			frameScore.Passed = stage3.Frames[index].Passed
		}
		if frameScore.Passed {
			score.PassedFrames++
		}
		similaritySum += frameScore.Sim
		score.Frames[index] = frameScore
	}

	if score.ValidFrames < cfg.VideoMinValid {
		score.Verdict = VerdictInconclusive
		return score, nil
	}

	score.AverageEvaluated = true
	score.AvgSim = similaritySum / float64(score.ValidFrames)
	if score.AvgSim >= cfg.VideoAvgT4 || score.PassedFrames >= cfg.VideoMinPassed {
		score.Verdict = VerdictYes
	}
	return score, nil
}

func pHashPassRatio(a, b [9]uint64, threshold int) float64 {
	passed := 0
	for index := range a {
		if features.Hamming64(a[index], b[index]) <= threshold {
			passed++
		}
	}
	return float64(passed) / float64(len(a))
}

func validateJudgeConfig(cfg config.Phase2Config) error {
	thresholds := []struct {
		name  string
		value float64
	}{
		{name: "phash_pass_t2", value: cfg.PHashPassT2},
		{name: "sobel_t3", value: cfg.SobelT3},
		{name: "video_avg_t4", value: cfg.VideoAvgT4},
	}
	for _, threshold := range thresholds {
		name, value := threshold.name, threshold.value
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
			return fmt.Errorf("phase2: %s must be finite and between 0 and 1", name)
		}
	}
	if cfg.PHashPartThreshold < 0 || cfg.PHashPartThreshold > 64 {
		return fmt.Errorf("phase2: phash_part_threshold must be between 0 and 64")
	}
	if cfg.VideoFrames != 6 {
		return fmt.Errorf("phase2: video_frames must be 6")
	}
	if cfg.VideoMinValid < 1 || cfg.VideoMinValid > cfg.VideoFrames {
		return fmt.Errorf("phase2: video_min_valid must be between 1 and video_frames")
	}
	if cfg.VideoMinPassed < 1 || cfg.VideoMinPassed > cfg.VideoFrames {
		return fmt.Errorf("phase2: video_min_passed must be between 1 and video_frames")
	}
	return nil
}

func validateSobelHist(hist [128]float32, label string) error {
	for index, value := range hist {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return fmt.Errorf("phase2: %s Sobel bin %d is not finite", label, index)
		}
	}
	return nil
}
