package firstscreen

import (
	"math"
	"sort"
)

type ImageFeature struct {
	SHA512  [64]byte
	PDQ     [4]uint64
	Quality int
	Width   int
	Height  int
}

func aspectClose(w1, h1, w2, h2 int, tolerance float64) bool {
	if math.IsNaN(tolerance) || math.IsInf(tolerance, 0) || tolerance < 0 {
		return false
	}
	if w1 <= 0 || h1 <= 0 || w2 <= 0 || h2 <= 0 {
		return true
	}
	cross1 := float64(w1) * float64(h2)
	cross2 := float64(w2) * float64(h1)
	difference := math.Abs(cross1 - cross2)
	scale := math.Max(cross1, cross2)
	limit := tolerance * scale
	if math.IsInf(limit, 1) {
		return true
	}
	// Admit only one representable step above the computed boundary. This
	// corrects finite multiplication rounding without turning a just-over
	// mathematical ratio into a match.
	epsilon := math.Nextafter(limit, math.Inf(1)) - limit
	return difference <= limit+epsilon
}

func screenImages(feats []ImageFeature, hammingMax int, aspectTolerance float64, qualityMin int) []CandidatePair {
	index := newBandIndex(len(feats))
	scratch := make([]uint32, 0, 256)
	var pairs []CandidatePair
	for i, feature := range feats {
		if feature.Quality < qualityMin {
			continue
		}
		scratch = index.query(feature.PDQ, scratch)
		for _, priorIndex := range scratch {
			prior := feats[priorIndex]
			if !aspectClose(feature.Width, feature.Height, prior.Width, prior.Height, aspectTolerance) {
				continue
			}
			distance := hamming256(feature.PDQ, prior.PDQ)
			if distance > hammingMax {
				continue
			}
			pairs = append(pairs, newCandidatePair(
				KindImageCandidate,
				feature.SHA512,
				prior.SHA512,
				distance,
				0,
				feature.Quality,
				prior.Quality,
			))
		}
		index.add(uint32(i), feature.PDQ)
	}
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].less(pairs[j])
	})
	return pairs
}
