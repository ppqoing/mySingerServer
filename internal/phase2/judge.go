package phase2

import (
	"fmt"
	"math"

	"dedup/internal/config"
	"dedup/internal/features"
)

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

	score := ImagePairScore{
		PHashPassRatio: pHashPassRatio(aParts, bParts, cfg.PHashPartThreshold),
		Verdict:        VerdictNo,
	}
	if score.PHashPassRatio < cfg.PHashPassT2 {
		return score, nil
	}

	score.SobelEvaluated = true
	score.SobelCosine = features.SobelCosine(aHist, bHist)
	if score.SobelCosine >= cfg.SobelT3 {
		score.Verdict = VerdictYes
	}
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

	var score VideoPairScore
	var similaritySum float64
	for index := 0; index < len(score.Frames); index++ {
		frameScore := FrameScore{FrameIdx: index}
		left, right := aFrames[index], bFrames[index]
		if left == nil || right == nil {
			score.Frames[index] = frameScore
			continue
		}

		frameScore.Valid = true
		score.ValidFrames++
		frameScore.PHashPassRatio = pHashPassRatio(
			left.PHashParts,
			right.PHashParts,
			cfg.PHashPartThreshold,
		)
		if frameScore.PHashPassRatio >= cfg.PHashPassT2 {
			frameScore.SobelEvaluated = true
			frameScore.SobelCosine = features.SobelCosine(left.SobelHist, right.SobelHist)
			frameScore.Sim = frameScore.SobelCosine
			frameScore.Passed = frameScore.Sim >= cfg.SobelT3
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
