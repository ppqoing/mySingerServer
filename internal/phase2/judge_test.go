package phase2

import (
	"encoding/json"
	"math"
	"testing"

	"dedup/internal/config"
	"dedup/internal/features"
	"dedup/internal/proto"
)

func TestStageScoreJSONUsesStableVerdictTextAndReason(t *testing.T) {
	raw, err := json.Marshal(StageScore{Verdict: VerdictYes, Reason: "sobel_passed"})
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	if document["verdict"] != "yes" || document["reason"] != "sobel_passed" {
		t.Fatalf("stage JSON = %s, want textual yes and stable reason", raw)
	}

	for verdict, want := range map[Verdict]string{
		VerdictNo: "no", VerdictYes: "yes", VerdictInconclusive: "inconclusive",
	} {
		raw, err := json.Marshal(verdict)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != `"`+want+`"` {
			t.Fatalf("verdict %d JSON = %s, want %q", verdict, raw, want)
		}
	}
}

func TestJudgeImageStage2AndStage3AreIndependent(t *testing.T) {
	a, b := partsWithPassCount(7)
	stage2 := JudgeImageStage2(features.EncodePHashParts(a), features.EncodePHashParts(b), validJudgeConfig())
	if stage2.Verdict != VerdictNo || stage2.Reason != "phash_below_threshold" {
		t.Fatalf("stage 2 = %#v", stage2)
	}
	hist := unitHist()
	raw, err := features.EncodeSobelHist(hist)
	if err != nil {
		t.Fatal(err)
	}
	stage3 := JudgeImageStage3(raw, raw, validJudgeConfig())
	if stage3.Verdict != VerdictYes || stage3.Reason != "sobel_passed" {
		t.Fatalf("stage 3 = %#v", stage3)
	}
}

func TestJudgeVideoStage3StableInconclusiveReason(t *testing.T) {
	raw, err := features.EncodeSobelHist(unitHist())
	if err != nil {
		t.Fatal(err)
	}
	frames := []proto.FrameFeature{
		{FrameIdx: 0, SobelHist: raw},
		{FrameIdx: 2, SobelHist: raw},
		{FrameIdx: 5, SobelHist: raw},
	}
	score := JudgeVideoStage3(frames, frames, validJudgeConfig())
	if score.Verdict != VerdictInconclusive || score.Reason != "insufficient_valid_frames" {
		t.Fatalf("stage 3 = %#v", score)
	}
}

func TestJudgeVideoStagesExcludeExplicitErrorFramesFromValidCount(t *testing.T) {
	pHash := features.EncodePHashParts([9]uint64{})
	sobel, err := features.EncodeSobelHist(unitHist())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		judge func([]proto.FrameFeature, []proto.FrameFeature, Config) StageScore
		field func(int, bool) proto.FrameFeature
	}{
		{
			name: "stage2", judge: JudgeVideoStage2,
			field: func(index int, failed bool) proto.FrameFeature {
				frame := proto.FrameFeature{FrameIdx: index, PHashParts: pHash}
				if failed {
					frame.PHashParts, frame.Error = nil, "decode_failed"
				}
				return frame
			},
		},
		{
			name: "stage3", judge: JudgeVideoStage3,
			field: func(index int, failed bool) proto.FrameFeature {
				frame := proto.FrameFeature{FrameIdx: index, SobelHist: sobel}
				if failed {
					frame.SobelHist, frame.Error = nil, "decode_failed"
				}
				return frame
			},
		},
	} {
		t.Run(test.name+" five healthy", func(t *testing.T) {
			var left, right []proto.FrameFeature
			for index := 0; index < 6; index++ {
				left = append(left, test.field(index, index == 5))
				right = append(right, test.field(index, false))
			}
			score := test.judge(left, right, validJudgeConfig())
			if score.ValidFrames != 5 || score.Verdict != VerdictYes {
				t.Fatalf("score = %#v, want five valid and yes", score)
			}
		})
		t.Run(test.name+" three healthy", func(t *testing.T) {
			var left, right []proto.FrameFeature
			for index := 0; index < 6; index++ {
				left = append(left, test.field(index, index >= 3))
				right = append(right, test.field(index, false))
			}
			score := test.judge(left, right, validJudgeConfig())
			if score.ValidFrames != 3 || score.Verdict != VerdictInconclusive || score.Reason != "insufficient_valid_frames" {
				t.Fatalf("score = %#v, want three valid and stable inconclusive", score)
			}
		})
		t.Run(test.name+" malformed without error", func(t *testing.T) {
			left := []proto.FrameFeature{test.field(0, false)}
			right := []proto.FrameFeature{test.field(0, false)}
			if test.name == "stage2" {
				left[0].PHashParts = []byte{1}
			} else {
				left[0].SobelHist = []byte{1}
			}
			score := test.judge(left, right, validJudgeConfig())
			if score.Verdict != VerdictInconclusive || score.Reason == "insufficient_valid_frames" {
				t.Fatalf("malformed payload score = %#v, want fail-closed payload reason", score)
			}
		})
	}
}

func TestJudgePairCompatibilityWrappersComposeIndependentStages(t *testing.T) {
	cfg := validJudgeConfig()
	aParts, bParts := partsWithPassCount(9)
	aHist, bHist := cosineHalfHists()
	image, err := JudgeImagePair(aParts, bParts, aHist, bHist, cfg)
	if err != nil {
		t.Fatal(err)
	}
	aPHash, bPHash := features.EncodePHashParts(aParts), features.EncodePHashParts(bParts)
	aSobel, _ := features.EncodeSobelHist(aHist)
	bSobel, _ := features.EncodeSobelHist(bHist)
	stage2, stage3 := JudgeImageStage2(aPHash, bPHash, cfg), JudgeImageStage3(aSobel, bSobel, cfg)
	if image.PHashPassRatio != stage2.PassRatio || image.SobelCosine != stage3.Similarity || image.Verdict != stage3.Verdict {
		t.Fatalf("image wrapper = %#v; stages = %#v/%#v", image, stage2, stage3)
	}

	var left, right [6]*FramePhase2
	leftFrames, rightFrames := make([]proto.FrameFeature, 0, 6), make([]proto.FrameFeature, 0, 6)
	for index := 0; index < 6; index++ {
		left[index], right[index] = identicalFrame(), identicalFrame()
		pHash := features.EncodePHashParts(left[index].PHashParts)
		sobel, _ := features.EncodeSobelHist(left[index].SobelHist)
		leftFrames = append(leftFrames, proto.FrameFeature{FrameIdx: index, PHashParts: pHash, SobelHist: sobel})
		rightFrames = append(rightFrames, proto.FrameFeature{FrameIdx: index, PHashParts: pHash, SobelHist: sobel})
	}
	video, err := JudgeVideoPair(left, right, cfg)
	if err != nil {
		t.Fatal(err)
	}
	video2, video3 := JudgeVideoStage2(leftFrames, rightFrames, cfg), JudgeVideoStage3(leftFrames, rightFrames, cfg)
	if video.ValidFrames != video2.ValidFrames || video.PassedFrames != video3.PassedFrames || video.AvgSim != video3.Similarity || video.Verdict != video3.Verdict {
		t.Fatalf("video wrapper = %#v; stages = %#v/%#v", video, video2, video3)
	}
}

func TestJudgeImagePairHammingTenPassesAndElevenFailsPart(t *testing.T) {
	cfg := validJudgeConfig()
	cfg.PHashPassT2 = 0
	cfg.SobelT3 = 0

	var a, b [9]uint64
	b[0] = (uint64(1) << 10) - 1
	score, err := JudgeImagePair(a, b, [128]float32{}, [128]float32{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if score.PHashPassRatio != 1 {
		t.Fatalf("Hamming 10 ratio = %v, want 1", score.PHashPassRatio)
	}

	b[0] = (uint64(1) << 11) - 1
	score, err = JudgeImagePair(a, b, [128]float32{}, [128]float32{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if want := 8.0 / 9.0; score.PHashPassRatio != want {
		t.Fatalf("Hamming 11 ratio = %v, want %v", score.PHashPassRatio, want)
	}
}

func TestJudgeImagePairDefaultT2EightOfNineEvaluatesAndSevenShortCircuits(t *testing.T) {
	cfg := validJudgeConfig()
	hist := unitHist()

	for _, tt := range []struct {
		name          string
		passingParts  int
		wantVerdict   Verdict
		wantEvaluated bool
	}{
		{name: "eight of nine", passingParts: 8, wantVerdict: VerdictYes, wantEvaluated: true},
		{name: "seven of nine", passingParts: 7, wantVerdict: VerdictNo, wantEvaluated: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, b := partsWithPassCount(tt.passingParts)
			score, err := JudgeImagePair(a, b, hist, hist, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if score.Verdict != tt.wantVerdict {
				t.Fatalf("Verdict = %v, want %v", score.Verdict, tt.wantVerdict)
			}
			if score.SobelEvaluated != tt.wantEvaluated {
				t.Fatalf("SobelEvaluated = %v, want %v", score.SobelEvaluated, tt.wantEvaluated)
			}
			if !tt.wantEvaluated && score.SobelCosine != 0 {
				t.Fatalf("short-circuit SobelCosine = %v, want zero", score.SobelCosine)
			}
		})
	}
}

func TestJudgeImagePairT2ExactBoundaryAndAdjacentThreshold(t *testing.T) {
	a, b := partsWithPassCount(8)
	hist := unitHist()
	ratio := 8.0 / 9.0

	cfg := validJudgeConfig()
	cfg.PHashPassT2 = ratio
	score, err := JudgeImagePair(a, b, hist, hist, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !score.SobelEvaluated {
		t.Fatal("Sobel was not evaluated at exact T2 boundary")
	}

	cfg.PHashPassT2 = math.Nextafter(ratio, 1)
	score, err = JudgeImagePair(a, b, hist, hist, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if score.SobelEvaluated || score.Verdict != VerdictNo {
		t.Fatalf("adjacent-above T2 score = %#v, want observable short circuit", score)
	}
}

func TestJudgeImagePairT3ExactBoundaryAndAdjacentThreshold(t *testing.T) {
	a, b := partsWithPassCount(9)
	aHist, bHist := cosineHalfHists()
	cosine := features.SobelCosine(aHist, bHist)

	cfg := validJudgeConfig()
	cfg.SobelT3 = cosine
	score, err := JudgeImagePair(a, b, aHist, bHist, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if score.Verdict != VerdictYes || score.SobelCosine != cosine {
		t.Fatalf("exact T3 score = %#v, want yes at cosine %v", score, cosine)
	}

	cfg.SobelT3 = math.Nextafter(cosine, 1)
	score, err = JudgeImagePair(a, b, aHist, bHist, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if score.Verdict != VerdictNo {
		t.Fatalf("adjacent-above T3 verdict = %v, want no", score.Verdict)
	}
}

func TestJudgeImagePairPreservesSobelZeroNormRules(t *testing.T) {
	a, b := partsWithPassCount(9)
	cfg := validJudgeConfig()

	bothZero, err := JudgeImagePair(a, b, [128]float32{}, [128]float32{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !bothZero.SobelEvaluated || bothZero.SobelCosine != 1 || bothZero.Verdict != VerdictYes {
		t.Fatalf("both-zero score = %#v, want evaluated cosine 1 and yes", bothZero)
	}

	oneZero, err := JudgeImagePair(a, b, [128]float32{}, unitHist(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !oneZero.SobelEvaluated || oneZero.SobelCosine != 0 || oneZero.Verdict != VerdictNo {
		t.Fatalf("one-zero score = %#v, want evaluated cosine 0 and no", oneZero)
	}
}

func TestJudgeImagePairRejectsNonFiniteHistogramEvenWhenPHashWouldShortCircuit(t *testing.T) {
	a, b := partsWithPassCount(0)
	for _, value := range []float32{
		float32(math.NaN()),
		float32(math.Inf(1)),
		float32(math.Inf(-1)),
	} {
		hist := [128]float32{}
		hist[37] = value
		if _, err := JudgeImagePair(a, b, hist, [128]float32{}, validJudgeConfig()); err == nil {
			t.Fatalf("JudgeImagePair accepted non-finite histogram value %v", value)
		}
	}
}

func TestJudgeImagePairRejectsInvalidDirectConfiguration(t *testing.T) {
	tests := []struct {
		name  string
		apply func(*config.Phase2Config)
	}{
		{name: "T2 NaN", apply: func(c *config.Phase2Config) { c.PHashPassT2 = math.NaN() }},
		{name: "T2 positive infinity", apply: func(c *config.Phase2Config) { c.PHashPassT2 = math.Inf(1) }},
		{name: "T2 below zero", apply: func(c *config.Phase2Config) { c.PHashPassT2 = -math.SmallestNonzeroFloat64 }},
		{name: "T2 above one", apply: func(c *config.Phase2Config) { c.PHashPassT2 = math.Nextafter(1, 2) }},
		{name: "T3 NaN", apply: func(c *config.Phase2Config) { c.SobelT3 = math.NaN() }},
		{name: "T3 negative infinity", apply: func(c *config.Phase2Config) { c.SobelT3 = math.Inf(-1) }},
		{name: "T4 NaN", apply: func(c *config.Phase2Config) { c.VideoAvgT4 = math.NaN() }},
		{name: "T4 above one", apply: func(c *config.Phase2Config) { c.VideoAvgT4 = math.Nextafter(1, 2) }},
		{name: "part threshold negative", apply: func(c *config.Phase2Config) { c.PHashPartThreshold = -1 }},
		{name: "part threshold above bits", apply: func(c *config.Phase2Config) { c.PHashPartThreshold = 65 }},
		{name: "wrong frame count", apply: func(c *config.Phase2Config) { c.VideoFrames = 5 }},
		{name: "zero minimum valid", apply: func(c *config.Phase2Config) { c.VideoMinValid = 0 }},
		{name: "minimum valid above frames", apply: func(c *config.Phase2Config) { c.VideoMinValid = 7 }},
		{name: "zero minimum passed", apply: func(c *config.Phase2Config) { c.VideoMinPassed = 0 }},
		{name: "minimum passed above frames", apply: func(c *config.Phase2Config) { c.VideoMinPassed = 7 }},
	}

	a, b := partsWithPassCount(9)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validJudgeConfig()
			tt.apply(&cfg)
			if _, err := JudgeImagePair(a, b, unitHist(), unitHist(), cfg); err == nil {
				t.Fatal("JudgeImagePair accepted invalid direct configuration")
			}
		})
	}
}

func TestJudgeVideoPairPreservesFrameOrderAndValidDenominator(t *testing.T) {
	var a, b [6]*FramePhase2
	for _, index := range []int{0, 2, 3, 5} {
		a[index] = identicalFrame()
		b[index] = identicalFrame()
	}

	score, err := JudgeVideoPair(a, b, validJudgeConfig())
	if err != nil {
		t.Fatal(err)
	}
	if score.ValidFrames != 4 || score.PassedFrames != 4 {
		t.Fatalf("counts = valid %d passed %d, want 4 and 4", score.ValidFrames, score.PassedFrames)
	}
	if !score.AverageEvaluated || score.AvgSim != 1 || score.Verdict != VerdictYes {
		t.Fatalf("video score = %#v, want average 1 over valid frames and yes", score)
	}
	for index, frame := range score.Frames {
		if frame.FrameIdx != index {
			t.Fatalf("Frames[%d].FrameIdx = %d", index, frame.FrameIdx)
		}
		wantValid := index == 0 || index == 2 || index == 3 || index == 5
		if frame.Valid != wantValid {
			t.Fatalf("Frames[%d].Valid = %v, want %v", index, frame.Valid, wantValid)
		}
	}
}

func TestJudgeVideoPairDistinguishesMissingAndShortCircuitedFrames(t *testing.T) {
	var a, b [6]*FramePhase2
	a[0], b[0] = identicalFrame(), identicalFrame()
	a[1] = identicalFrame()
	a[2], b[2] = shortCircuitFrame(), identicalFrame()
	a[3], b[3] = identicalFrame(), identicalFrame()
	a[4], b[4] = identicalFrame(), identicalFrame()
	a[5], b[5] = identicalFrame(), identicalFrame()

	score, err := JudgeVideoPair(a, b, validJudgeConfig())
	if err != nil {
		t.Fatal(err)
	}
	missing := score.Frames[1]
	if missing.Valid || missing.SobelEvaluated || missing.PHashPassRatio != 0 || missing.Sim != 0 {
		t.Fatalf("missing frame detail = %#v, want invalid and unevaluated", missing)
	}
	short := score.Frames[2]
	if !short.Valid || short.SobelEvaluated || short.PHashPassRatio != 7.0/9.0 ||
		short.SobelCosine != 0 || short.Sim != 0 || short.Passed {
		t.Fatalf("short-circuited frame detail = %#v", short)
	}
}

func TestJudgeVideoPairFewerThanFourIsInconclusiveWithoutPublishedAverage(t *testing.T) {
	var a, b [6]*FramePhase2
	for index := 0; index < 3; index++ {
		a[index], b[index] = identicalFrame(), identicalFrame()
	}
	cfg := validJudgeConfig()
	cfg.VideoMinPassed = 3

	score, err := JudgeVideoPair(a, b, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if score.ValidFrames != 3 || score.PassedFrames != 3 {
		t.Fatalf("counts = valid %d passed %d, want 3 and 3", score.ValidFrames, score.PassedFrames)
	}
	if score.Verdict != VerdictInconclusive || score.AverageEvaluated || score.AvgSim != 0 {
		t.Fatalf("inconclusive score = %#v, want unpublished final average", score)
	}
}

func TestJudgeVideoPairORSemanticsAverageRouteOnly(t *testing.T) {
	var a, b [6]*FramePhase2
	for index := 0; index < 3; index++ {
		a[index], b[index] = identicalFrame(), identicalFrame()
	}
	for index := 3; index < 6; index++ {
		a[index], b[index] = shortCircuitFrame(), identicalFrame()
	}
	cfg := validJudgeConfig()
	cfg.VideoAvgT4 = 0.5
	cfg.VideoMinPassed = 4

	score, err := JudgeVideoPair(a, b, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if score.PassedFrames != 3 || score.AvgSim != 0.5 || score.Verdict != VerdictYes {
		t.Fatalf("average-only route score = %#v", score)
	}
}

func TestJudgeVideoPairORSemanticsFourFrameFallbackOnly(t *testing.T) {
	aHist, bHist := cosineHalfHists()
	similarity := features.SobelCosine(aHist, bHist)
	var a, b [6]*FramePhase2
	for index := 0; index < 4; index++ {
		a[index] = frameWithHist(aHist)
		b[index] = frameWithHist(bHist)
	}
	for index := 4; index < 6; index++ {
		a[index], b[index] = shortCircuitFrame(), identicalFrame()
	}
	cfg := validJudgeConfig()
	cfg.SobelT3 = similarity
	cfg.VideoAvgT4 = 0.8
	cfg.VideoMinPassed = 4

	score, err := JudgeVideoPair(a, b, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if score.PassedFrames != 4 || !(score.AvgSim < cfg.VideoAvgT4) || score.Verdict != VerdictYes {
		t.Fatalf("four-frame fallback score = %#v", score)
	}
}

func TestJudgeVideoPairT4ExactBoundaryAndAdjacentThreshold(t *testing.T) {
	var a, b [6]*FramePhase2
	for index := 0; index < 3; index++ {
		a[index], b[index] = identicalFrame(), identicalFrame()
	}
	for index := 3; index < 6; index++ {
		a[index], b[index] = shortCircuitFrame(), identicalFrame()
	}
	cfg := validJudgeConfig()
	cfg.VideoMinPassed = 4
	cfg.VideoAvgT4 = 0.5

	score, err := JudgeVideoPair(a, b, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if score.Verdict != VerdictYes || score.AvgSim != 0.5 {
		t.Fatalf("exact T4 score = %#v, want yes at 0.5", score)
	}

	cfg.VideoAvgT4 = math.Nextafter(0.5, 1)
	score, err = JudgeVideoPair(a, b, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if score.Verdict != VerdictNo {
		t.Fatalf("adjacent-above T4 verdict = %v, want no", score.Verdict)
	}
}

func TestJudgeVideoPairRejectsNonFinitePresentFrameIncludingUnpairedEndpoint(t *testing.T) {
	bad := identicalFrame()
	bad.SobelHist[9] = float32(math.NaN())

	for _, tt := range []struct {
		name string
		a    [6]*FramePhase2
		b    [6]*FramePhase2
	}{
		{
			name: "paired",
			a:    [6]*FramePhase2{bad},
			b:    [6]*FramePhase2{identicalFrame()},
		},
		{
			name: "unpaired endpoint",
			a:    [6]*FramePhase2{bad},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := JudgeVideoPair(tt.a, tt.b, validJudgeConfig()); err == nil {
				t.Fatal("JudgeVideoPair accepted a present non-finite frame")
			}
		})
	}
}

func TestJudgeVideoPairRejectsInvalidDirectConfiguration(t *testing.T) {
	tests := []struct {
		name  string
		apply func(*config.Phase2Config)
	}{
		{name: "T2 NaN", apply: func(c *config.Phase2Config) { c.PHashPassT2 = math.NaN() }},
		{name: "T3 infinity", apply: func(c *config.Phase2Config) { c.SobelT3 = math.Inf(1) }},
		{name: "T4 NaN", apply: func(c *config.Phase2Config) { c.VideoAvgT4 = math.NaN() }},
		{name: "T4 negative infinity", apply: func(c *config.Phase2Config) { c.VideoAvgT4 = math.Inf(-1) }},
		{name: "part threshold negative", apply: func(c *config.Phase2Config) { c.PHashPartThreshold = -1 }},
		{name: "part threshold above bits", apply: func(c *config.Phase2Config) { c.PHashPartThreshold = 65 }},
		{name: "wrong frame count", apply: func(c *config.Phase2Config) { c.VideoFrames = 5 }},
		{name: "zero minimum valid", apply: func(c *config.Phase2Config) { c.VideoMinValid = 0 }},
		{name: "minimum valid above frames", apply: func(c *config.Phase2Config) { c.VideoMinValid = 7 }},
		{name: "zero minimum passed", apply: func(c *config.Phase2Config) { c.VideoMinPassed = 0 }},
		{name: "minimum passed above frames", apply: func(c *config.Phase2Config) { c.VideoMinPassed = 7 }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validJudgeConfig()
			tt.apply(&cfg)
			if _, err := JudgeVideoPair([6]*FramePhase2{}, [6]*FramePhase2{}, cfg); err == nil {
				t.Fatal("JudgeVideoPair accepted invalid direct configuration")
			}
		})
	}
}

func validJudgeConfig() config.Phase2Config {
	return config.Phase2Config{
		PHashPassT2:        0.80,
		PHashPartThreshold: 10,
		SobelT3:            0.85,
		VideoFrames:        6,
		VideoAvgT4:         0.80,
		VideoMinPassed:     4,
		VideoMinValid:      4,
	}
}

func partsWithPassCount(count int) ([9]uint64, [9]uint64) {
	var a, b [9]uint64
	passValue := (uint64(1) << 10) - 1
	failValue := (uint64(1) << 11) - 1
	for index := range b {
		if index < count {
			b[index] = passValue
		} else {
			b[index] = failValue
		}
	}
	return a, b
}

func unitHist() [128]float32 {
	var hist [128]float32
	hist[0] = 1
	return hist
}

func cosineHalfHists() ([128]float32, [128]float32) {
	var a, b [128]float32
	a[0] = 1
	b[0] = 1
	b[1] = float32(math.Sqrt(3))
	return a, b
}

func identicalFrame() *FramePhase2 {
	a, _ := partsWithPassCount(9)
	return &FramePhase2{PHashParts: a, SobelHist: unitHist()}
}

func shortCircuitFrame() *FramePhase2 {
	_, b := partsWithPassCount(7)
	return &FramePhase2{PHashParts: b, SobelHist: unitHist()}
}

func frameWithHist(hist [128]float32) *FramePhase2 {
	a, _ := partsWithPassCount(9)
	return &FramePhase2{PHashParts: a, SobelHist: hist}
}
