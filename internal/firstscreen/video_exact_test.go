package firstscreen

import (
	"bytes"
	"math"
	"math/bits"
	"math/rand"
	"reflect"
	"sort"
	"testing"
)

func TestScreenVideosDurationBoundary(t *testing.T) {
	hash := [4]uint64{1, 2, 3, 4}
	tests := []struct {
		name       string
		difference int64
		want       int
	}{
		{"2000ms passes", 2000, 1},
		{"2001ms fails", 2001, 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			features := []VideoFeature{
				{SHA512: task3SHA(1), DurationMs: 60000, ThumbPDQ: hash, ThumbQuality: 80},
				{SHA512: task3SHA(2), DurationMs: 60000 + test.difference, ThumbPDQ: hash, ThumbQuality: 70},
			}
			pairs := screenVideos(features, 2000, 31)
			if len(pairs) != test.want {
				t.Fatalf("difference=%d pairs=%+v, want count %d", test.difference, pairs, test.want)
			}
			if test.want == 1 && pairs[0].DurationDiffMs != 2000 {
				t.Fatalf("duration difference = %d, want 2000", pairs[0].DurationDiffMs)
			}
		})
	}
}

func TestScreenVideosExtremeDurationDifferencesDoNotOverflow(t *testing.T) {
	hash := [4]uint64{1, 2, 3, 4}
	tests := []struct {
		name       string
		first      int64
		second     int64
		window     int64
		wantPairs  int
		wantDiffMs int64
	}{
		{"MinInt64 to MaxInt64 exceeds representable window", math.MinInt64, math.MaxInt64, math.MaxInt64, 0, 0},
		{"MinInt64 to zero exceeds MaxInt64 window", math.MinInt64, 0, math.MaxInt64, 0, 0},
		{"MinInt64 to negative one equals MaxInt64", math.MinInt64, -1, math.MaxInt64, 1, math.MaxInt64},
		{"zero to MaxInt64 equals MaxInt64", 0, math.MaxInt64, math.MaxInt64, 1, math.MaxInt64},
		{"negative to zero at boundary", -2000, 0, 2000, 1, 2000},
		{"negative to zero just outside", -2001, 0, 2000, 0, 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			features := []VideoFeature{
				{SHA512: task3SHA(1), DurationMs: test.first, ThumbPDQ: hash},
				{SHA512: task3SHA(2), DurationMs: test.second, ThumbPDQ: hash},
			}
			pairs := screenVideos(features, test.window, 31)
			if len(pairs) != test.wantPairs {
				t.Fatalf("durations=(%d,%d) window=%d pairs=%+v, want %d",
					test.first, test.second, test.window, pairs, test.wantPairs)
			}
			if test.wantPairs == 1 {
				if pairs[0].DurationDiffMs != test.wantDiffMs {
					t.Fatalf("duration difference = %d, want %d", pairs[0].DurationDiffMs, test.wantDiffMs)
				}
				if pairs[0].DurationDiffMs < 0 {
					t.Fatalf("negative duration difference: %d", pairs[0].DurationDiffMs)
				}
			}
		})
	}
}

func TestScreenVideosHammingBoundary(t *testing.T) {
	base := [4]uint64{0, 11, 22, 33}
	tests := []struct {
		name string
		bits int
		want int
	}{
		{"31 passes", 31, 1},
		{"32 fails", 32, 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			other := base
			for bit := 0; bit < test.bits; bit++ {
				other[0] ^= uint64(1) << bit
			}
			features := []VideoFeature{
				{SHA512: task3SHA(1), DurationMs: 60000, ThumbPDQ: base, ThumbQuality: 80},
				{SHA512: task3SHA(2), DurationMs: 60001, ThumbPDQ: other, ThumbQuality: 70},
			}
			pairs := screenVideos(features, 2000, 31)
			if len(pairs) != test.want {
				t.Fatalf("distance=%d pairs=%+v, want count %d", test.bits, pairs, test.want)
			}
			if test.want == 1 && pairs[0].Hamming != 31 {
				t.Fatalf("hamming = %d, want 31", pairs[0].Hamming)
			}
		})
	}
}

func TestScreenVideosSortsEqualDurationBySHA(t *testing.T) {
	hash := [4]uint64{1, 2, 3, 4}
	features := []VideoFeature{
		{SHA512: task3SHA(3), DurationMs: 2000, ThumbPDQ: hash},
		{SHA512: task3SHA(2), DurationMs: 1000, ThumbPDQ: hash},
		{SHA512: task3SHA(1), DurationMs: 1000, ThumbPDQ: hash},
	}
	screenVideos(features, 0, 31)
	if features[0].SHA512 != task3SHA(1) ||
		features[1].SHA512 != task3SHA(2) ||
		features[2].SHA512 != task3SHA(3) {
		t.Fatalf("in-place order = %x, %x, %x", features[0].SHA512, features[1].SHA512, features[2].SHA512)
	}
}

func TestScreenVideosKeepsZeroQualityAndNormalizesIndependentOfDuration(t *testing.T) {
	hash := [4]uint64{1, 2, 3, 4}
	highSHA := task3SHA(9)
	lowSHA := task3SHA(1)
	features := []VideoFeature{
		{SHA512: highSHA, DurationMs: 1000, ThumbPDQ: hash, ThumbQuality: 0},
		{SHA512: lowSHA, DurationMs: 1500, ThumbPDQ: hash, ThumbQuality: 7},
	}
	pairs := screenVideos(features, 2000, 31)
	if len(pairs) != 1 {
		t.Fatalf("pairs = %+v, want one", pairs)
	}
	pair := pairs[0]
	if pair.ShaA != lowSHA || pair.ShaB != highSHA {
		t.Fatalf("SHA order = %x, %x", pair.ShaA, pair.ShaB)
	}
	if pair.QualityA != 7 || pair.QualityB != 0 {
		t.Fatalf("qualities = %d, %d; want 7, 0", pair.QualityA, pair.QualityB)
	}
	if pair.DurationDiffMs != 500 {
		t.Fatalf("duration difference = %d, want 500", pair.DurationDiffMs)
	}
}

func TestScreenVideosDeterministicAcrossInputOrder(t *testing.T) {
	rng := rand.New(rand.NewSource(303))
	var source []VideoFeature
	for cluster := 0; cluster < 60; cluster++ {
		base := task3RandomPDQ(rng)
		duration := int64(cluster * 10000)
		for member := 0; member < 3; member++ {
			source = append(source, VideoFeature{
				SHA512:       task3SHA(len(source) + 1),
				DurationMs:   duration + int64(member*500),
				ThumbPDQ:     task3MutatePDQ(base, member, rng),
				ThumbQuality: member,
			})
		}
	}
	baselineInput := append([]VideoFeature(nil), source...)
	want := screenVideos(baselineInput, 2000, 31)
	if len(want) != 180 {
		t.Fatalf("baseline pairs = %d, want 180", len(want))
	}
	for run := 0; run < 20; run++ {
		input := append([]VideoFeature(nil), source...)
		rng.Shuffle(len(input), func(i, j int) { input[i], input[j] = input[j], input[i] })
		got := screenVideos(input, 2000, 31)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d differs: got=%d want=%d", run, len(got), len(want))
		}
	}
}

func TestScreenVideosMatchesRandomizedNaiveDurationHammingOracle(t *testing.T) {
	rng := rand.New(rand.NewSource(404))
	features := make([]VideoFeature, 0, 700)
	for cluster := 0; cluster < 300; cluster++ {
		base := task3RandomPDQ(rng)
		duration := rng.Int63n(1_000_000)
		features = append(features, VideoFeature{
			SHA512:       task3SHA(len(features) + 1),
			DurationMs:   duration,
			ThumbPDQ:     base,
			ThumbQuality: rng.Intn(101),
		})
		features = append(features, VideoFeature{
			SHA512:       task3SHA(len(features) + 1),
			DurationMs:   duration + int64(rng.Intn(3001)),
			ThumbPDQ:     task3MutatePDQ(base, rng.Intn(41), rng),
			ThumbQuality: rng.Intn(101),
		})
	}
	for len(features) < cap(features) {
		features = append(features, VideoFeature{
			SHA512:       task3SHA(len(features) + 1),
			DurationMs:   int64(rng.Uint64()),
			ThumbPDQ:     task3RandomPDQ(rng),
			ThumbQuality: rng.Intn(101),
		})
	}
	rng.Shuffle(len(features), func(i, j int) { features[i], features[j] = features[j], features[i] })

	oracleInput := append([]VideoFeature(nil), features...)
	want := task3NaiveVideoOracle(oracleInput, 2000, 31)
	got := screenVideos(features, 2000, 31)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("screenVideos differs from oracle: got=%d want=%d", len(got), len(want))
	}
	seen := make(map[[128]byte]struct{}, len(got))
	for i, pair := range got {
		var key [128]byte
		copy(key[:64], pair.ShaA[:])
		copy(key[64:], pair.ShaB[:])
		if _, duplicate := seen[key]; duplicate {
			t.Fatalf("duplicate pair at %d: %x/%x", i, pair.ShaA, pair.ShaB)
		}
		seen[key] = struct{}{}
		if i > 0 && got[i].less(got[i-1]) {
			t.Fatalf("output not sorted at %d", i)
		}
	}
}

func TestExactCollectorAggregatesCrossMachineDiskAndSameDiskPaths(t *testing.T) {
	collector := &exactCollector{}
	sha1, sha2, sha3 := task3SHA(1), task3SHA(2), task3SHA(3)
	collector.add(sha1, FileRef{ID: 10, MachineID: "m1", DiskNo: 0, Path: "a", Size: 100})
	collector.add(sha1, FileRef{ID: 11, MachineID: "m2", DiskNo: 1, Path: "b", Size: 100})
	collector.add(sha1, FileRef{ID: 12, MachineID: "m1", DiskNo: 0, Path: "c", Size: 100})
	collector.add(sha2, FileRef{ID: 20, MachineID: "m1", DiskNo: 0, Path: "singleton", Size: 200})
	collector.add(sha3, FileRef{ID: 30, MachineID: "m3", DiskNo: 2, Path: "d", Size: 300})
	collector.add(sha3, FileRef{ID: 31, MachineID: "m3", DiskNo: 2, Path: "e", Size: 300})

	groups := collector.finish()
	if len(groups) != 2 {
		t.Fatalf("groups = %+v, want two", groups)
	}
	if groups[0].SHA512 != sha1 || groups[1].SHA512 != sha3 {
		t.Fatalf("group SHAs = %x, %x", groups[0].SHA512, groups[1].SHA512)
	}
	if got := task3MemberIDs(groups[0]); !reflect.DeepEqual(got, []int64{10, 11, 12}) {
		t.Fatalf("first group IDs = %v", got)
	}
	if groups[0].Members[0].ID != 10 {
		t.Fatalf("representative ID = %d, want 10", groups[0].Members[0].ID)
	}
	if got := task3MemberIDs(groups[1]); !reflect.DeepEqual(got, []int64{30, 31}) {
		t.Fatalf("final group IDs = %v", got)
	}
	if groups[1].Members[0].MachineID != groups[1].Members[1].MachineID ||
		groups[1].Members[0].DiskNo != groups[1].Members[1].DiskNo ||
		groups[1].Members[0].Path == groups[1].Members[1].Path {
		t.Fatalf("same-disk different-path duplicate lost: %+v", groups[1].Members)
	}
}

func TestExactCollectorFinishIsIdempotentAndFlushesFinalGroupOnce(t *testing.T) {
	collector := &exactCollector{}
	sha := task3SHA(1)
	collector.add(sha, FileRef{ID: 1, Path: "a"})
	collector.add(sha, FileRef{ID: 2, Path: "b"})
	first := collector.finish()
	second := collector.finish()
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("finish lengths = %d, %d; want 1, 1", len(first), len(second))
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("repeated finish changed output: first=%+v second=%+v", first, second)
	}
}

func TestExactCollectorEmptyAndSingletonProduceNoGroups(t *testing.T) {
	empty := (&exactCollector{}).finish()
	if len(empty) != 0 {
		t.Fatalf("empty finish = %+v", empty)
	}
	collector := &exactCollector{}
	collector.add(task3SHA(1), FileRef{ID: 1, Path: "only"})
	if groups := collector.finish(); len(groups) != 0 {
		t.Fatalf("singleton groups = %+v", groups)
	}
}

func BenchmarkScreenVideosTwoHundredThousand(b *testing.B) {
	rng := rand.New(rand.NewSource(505))
	source := make([]VideoFeature, 200000)
	for i := range source {
		source[i] = VideoFeature{
			SHA512:       task3SHA(i + 1),
			DurationMs:   int64(i) * 250,
			ThumbPDQ:     task3RandomPDQ(rng),
			ThumbQuality: rng.Intn(101),
		}
	}
	rng.Shuffle(len(source), func(i, j int) { source[i], source[j] = source[j], source[i] })
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		input := append([]VideoFeature(nil), source...)
		b.StartTimer()
		_ = screenVideos(input, 2000, 31)
	}
}

func task3SHA(value int) (sha [64]byte) {
	sha[60] = byte(value >> 24)
	sha[61] = byte(value >> 16)
	sha[62] = byte(value >> 8)
	sha[63] = byte(value)
	return sha
}

func task3RandomPDQ(rng *rand.Rand) [4]uint64 {
	return [4]uint64{rng.Uint64(), rng.Uint64(), rng.Uint64(), rng.Uint64()}
}

func task3MutatePDQ(hash [4]uint64, count int, rng *rand.Rand) [4]uint64 {
	used := make(map[int]struct{}, count)
	for len(used) < count {
		position := rng.Intn(256)
		if _, duplicate := used[position]; duplicate {
			continue
		}
		used[position] = struct{}{}
		hash[position/64] ^= uint64(1) << (position % 64)
	}
	return hash
}

func task3NaiveVideoOracle(features []VideoFeature, windowMs int64, hammingMax int) []CandidatePair {
	var pairs []CandidatePair
	for i := 0; i < len(features); i++ {
		for j := i + 1; j < len(features); j++ {
			difference := task3UnsignedDurationDifference(features[i].DurationMs, features[j].DurationMs)
			if difference > uint64(windowMs) {
				continue
			}
			distance := task3Hamming(features[i].ThumbPDQ, features[j].ThumbPDQ)
			if distance > hammingMax {
				continue
			}
			pairs = append(pairs, task3OracleVideoPair(features[i], features[j], distance, int64(difference)))
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if order := bytes.Compare(pairs[i].ShaA[:], pairs[j].ShaA[:]); order != 0 {
			return order < 0
		}
		return bytes.Compare(pairs[i].ShaB[:], pairs[j].ShaB[:]) < 0
	})
	return pairs
}

func task3UnsignedDurationDifference(a, b int64) uint64 {
	if a < b {
		return uint64(b) - uint64(a)
	}
	return uint64(a) - uint64(b)
}

func task3Hamming(a, b [4]uint64) int {
	return bits.OnesCount64(a[0]^b[0]) +
		bits.OnesCount64(a[1]^b[1]) +
		bits.OnesCount64(a[2]^b[2]) +
		bits.OnesCount64(a[3]^b[3])
}

func task3OracleVideoPair(a, b VideoFeature, distance int, durationDifference int64) CandidatePair {
	if bytes.Compare(a.SHA512[:], b.SHA512[:]) > 0 {
		a, b = b, a
	}
	return CandidatePair{
		Kind:           KindVideoCandidate,
		ShaA:           a.SHA512,
		ShaB:           b.SHA512,
		Hamming:        distance,
		DurationDiffMs: durationDifference,
		QualityA:       a.ThumbQuality,
		QualityB:       b.ThumbQuality,
	}
}

func task3MemberIDs(group ExactGroup) []int64 {
	ids := make([]int64, len(group.Members))
	for i, member := range group.Members {
		ids[i] = member.ID
	}
	return ids
}
