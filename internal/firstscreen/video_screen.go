package firstscreen

import (
	"bytes"
	"sort"
)

type VideoFeature struct {
	SHA512       [64]byte
	DurationMs   int64
	ThumbPDQ     [4]uint64
	ThumbQuality int
}

func screenVideos(features []VideoFeature, windowMs int64, hammingMax int) []CandidatePair {
	sort.Slice(features, func(i, j int) bool {
		if features[i].DurationMs != features[j].DurationMs {
			return features[i].DurationMs < features[j].DurationMs
		}
		return bytes.Compare(features[i].SHA512[:], features[j].SHA512[:]) < 0
	})

	var pairs []CandidatePair
	for i := 0; i < len(features); i++ {
		first := features[i]
		firstDurationKey := uint64(first.DurationMs) ^ (uint64(1) << 63)
		for j := i + 1; j < len(features); j++ {
			second := features[j]
			secondDurationKey := uint64(second.DurationMs) ^ (uint64(1) << 63)
			unsignedDifference := secondDurationKey - firstDurationKey
			if unsignedDifference > uint64(windowMs) {
				break
			}
			distance := hamming256(first.ThumbPDQ, second.ThumbPDQ)
			if distance > hammingMax {
				continue
			}
			pairs = append(pairs, newCandidatePair(
				KindVideoCandidate,
				first.SHA512,
				second.SHA512,
				distance,
				int64(unsignedDifference),
				first.ThumbQuality,
				second.ThumbQuality,
			))
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].less(pairs[j])
	})
	return pairs
}
