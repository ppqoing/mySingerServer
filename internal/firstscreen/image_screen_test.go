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

func TestBandIndexUsesFourPositionedBandsAndOnlyPriorFeatures(t *testing.T) {
	target := [4]uint64{10, 20, 30, 40}
	index := newBandIndex(5)
	scratch := make([]uint32, 0, 8)
	if got := index.query(target, scratch); len(got) != 0 {
		t.Fatalf("empty index query = %v", got)
	}

	index.add(0, [4]uint64{10, 1, 2, 3})
	index.add(1, [4]uint64{4, 20, 5, 6})
	index.add(2, [4]uint64{7, 8, 30, 9})
	index.add(3, [4]uint64{11, 12, 13, 40})
	index.add(4, [4]uint64{99, 10, 99, 99}) // same value as band 0, wrong position

	got := index.query(target, scratch)
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	want := []uint32{0, 1, 2, 3}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("query = %v, want %v", got, want)
	}
}

func TestBandIndexSuppressesMultiBandDuplicatesAndReusesScratch(t *testing.T) {
	hash := [4]uint64{1, 2, 3, 4}
	index := newBandIndex(1)
	index.add(0, hash)
	scratch := make([]uint32, 0, 4)
	got := index.query(hash, scratch)
	if !reflect.DeepEqual(got, []uint32{0}) {
		t.Fatalf("query = %v, want [0]", got)
	}
	if &got[0] != &scratch[:1][0] {
		t.Fatal("query did not reuse caller scratch")
	}
}

func TestBandIndexDenseQueryReusesExpandedScratch(t *testing.T) {
	hash := [4]uint64{1, 2, 3, 4}
	index := newBandIndex(512)
	for i := 0; i < 512; i++ {
		index.add(uint32(i), hash)
	}
	scratch := make([]uint32, 0, 256)
	scratch = index.query(hash, scratch)
	if len(scratch) != 512 || cap(scratch) <= 256 {
		t.Fatalf("expanded scratch len=%d cap=%d", len(scratch), cap(scratch))
	}
	allocations := testing.AllocsPerRun(100, func() {
		scratch = index.query(hash, scratch)
		if len(scratch) != 512 {
			panic("dense query lost candidates")
		}
	})
	if allocations != 0 {
		t.Fatalf("reused expanded scratch allocated %.2f times/run", allocations)
	}
}

func TestBandIndexStampOverflowClearsPriorMarks(t *testing.T) {
	hash := [4]uint64{1, 2, 3, 4}
	index := newBandIndex(2)
	index.add(0, hash)
	index.add(1, hash)
	index.stamp[0], index.stamp[1] = 1, 1
	index.cur = math.MaxUint32

	got := index.query(hash, make([]uint32, 0, 2))
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	if !reflect.DeepEqual(got, []uint32{0, 1}) {
		t.Fatalf("overflow query = %v, want [0 1]", got)
	}
}

// Four exact 64-bit bands guarantee full recall only through Hamming distance
// three: at most three changed bits leave at least one whole band unchanged.
func TestBandIndexRecallWithinThreeBitsTenThousandCases(t *testing.T) {
	rng := rand.New(rand.NewSource(20260728))
	index := newBandIndex(10000)
	scratch := make([]uint32, 0, 16)
	for i := 0; i < 10000; i++ {
		base := task2RandomPDQ(rng)
		index.add(uint32(i), base)
		mutated := task2MutateDistinct(base, i%4, rng)
		got := index.query(mutated, scratch)
		found := 0
		seen := make(map[uint32]struct{}, len(got))
		for _, candidate := range got {
			if _, duplicate := seen[candidate]; duplicate {
				t.Fatalf("case %d returned duplicate index %d", i, candidate)
			}
			seen[candidate] = struct{}{}
			if candidate == uint32(i) {
				found++
			}
		}
		if found != 1 {
			t.Fatalf("case %d distance=%d found base %d times in %v", i, i%4, found, got)
		}
	}
}

func TestAspectCloseBoundaryAndMissingDimensions(t *testing.T) {
	tests := []struct {
		name           string
		w1, h1, w2, h2 int
		tolerance      float64
		want           bool
	}{
		{"equal", 1000, 1000, 1000, 1000, 0.10, true},
		{"exact decimal ten percent", 1000, 1000, 900, 1000, 0.10, true},
		{"exact rational ten percent", 1, 5, 9, 50, 0.10, true},
		{"just over decimal ten percent", 1000, 1000, 899, 1000, 0.10, false},
		{"just over rational ten percent", 1, 5, 899, 5000, 0.10, false},
		{"zero width", 0, 1000, 899, 1000, 0.10, true},
		{"zero height", 1000, 0, 899, 1000, 0.10, true},
		{"negative peer width", 1000, 1000, -1, 1000, 0.10, true},
		{"negative peer height", 1000, 1000, 1000, -1, 0.10, true},
		{"NaN tolerance", 1000, 1000, 1000, 1000, math.NaN(), false},
		{"positive infinity tolerance", 1000, 1000, 1000, 1000, math.Inf(1), false},
		{"negative infinity tolerance", 1000, 1000, 1000, 1000, math.Inf(-1), false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := aspectClose(test.w1, test.h1, test.w2, test.h2, test.tolerance); got != test.want {
				t.Fatalf("aspectClose(%d,%d,%d,%d) = %t, want %t",
					test.w1, test.h1, test.w2, test.h2, got, test.want)
			}
		})
	}
}

func TestScreenImagesHammingBoundary(t *testing.T) {
	base := [4]uint64{0, 11, 22, 33}
	tests := []struct {
		name string
		bits int
		want bool
	}{
		{"distance 31 passes", 31, true},
		{"distance 32 fails", 32, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			other := base
			for bit := 0; bit < test.bits; bit++ {
				other[0] ^= uint64(1) << bit
			}
			feats := []ImageFeature{
				{SHA512: task2SHA(1), PDQ: base, Quality: 50, Width: 1000, Height: 1000},
				{SHA512: task2SHA(2), PDQ: other, Quality: 50, Width: 1000, Height: 1000},
			}
			got := screenImages(feats, 31, 0.10, 50)
			if (len(got) == 1) != test.want {
				t.Fatalf("distance %d pairs = %+v, wantPair=%t", test.bits, got, test.want)
			}
			if test.want && got[0].Hamming != 31 {
				t.Fatalf("hamming = %d, want 31", got[0].Hamming)
			}
		})
	}
}

func TestScreenImagesQualityBoundaryDoesNotQueryOrIndexLowQuality(t *testing.T) {
	hash := [4]uint64{1, 2, 3, 4}
	tests := []struct {
		name  string
		feats []ImageFeature
		want  int
	}{
		{
			"quality 50 pairs",
			[]ImageFeature{
				{SHA512: task2SHA(1), PDQ: hash, Quality: 50, Width: 1, Height: 1},
				{SHA512: task2SHA(2), PDQ: hash, Quality: 50, Width: 1, Height: 1},
			},
			1,
		},
		{
			"quality 49 is not indexed",
			[]ImageFeature{
				{SHA512: task2SHA(1), PDQ: hash, Quality: 49, Width: 1, Height: 1},
				{SHA512: task2SHA(2), PDQ: hash, Quality: 50, Width: 1, Height: 1},
			},
			0,
		},
		{
			"quality 49 does not query",
			[]ImageFeature{
				{SHA512: task2SHA(1), PDQ: hash, Quality: 50, Width: 1, Height: 1},
				{SHA512: task2SHA(2), PDQ: hash, Quality: 49, Width: 1, Height: 1},
			},
			0,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := screenImages(test.feats, 31, 0.10, 50); len(got) != test.want {
				t.Fatalf("pairs = %+v, want count %d", got, test.want)
			}
		})
	}
}

func TestScreenImagesAspectBoundaryAndMissingDimensions(t *testing.T) {
	hash := [4]uint64{1, 2, 3, 4}
	tests := []struct {
		name       string
		peerWidth  int
		peerHeight int
		want       int
	}{
		{"exact ten percent", 900, 1000, 1},
		{"just over ten percent", 899, 1000, 0},
		{"missing dimensions bypass pruning", 0, 0, 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			feats := []ImageFeature{
				{SHA512: task2SHA(1), PDQ: hash, Quality: 50, Width: 1000, Height: 1000},
				{SHA512: task2SHA(2), PDQ: hash, Quality: 50, Width: test.peerWidth, Height: test.peerHeight},
			}
			if got := screenImages(feats, 31, 0.10, 50); len(got) != test.want {
				t.Fatalf("pairs = %+v, want count %d", got, test.want)
			}
		})
	}
}

func TestScreenImagesSuppressesMultiBandDuplicatePairs(t *testing.T) {
	hash := [4]uint64{1, 2, 3, 4}
	feats := []ImageFeature{
		{SHA512: task2SHA(2), PDQ: hash, Quality: 80, Width: 1, Height: 1},
		{SHA512: task2SHA(1), PDQ: hash, Quality: 70, Width: 1, Height: 1},
	}
	got := screenImages(feats, 31, 0.10, 50)
	if len(got) != 1 {
		t.Fatalf("pairs = %+v, want one", got)
	}
	if got[0].ShaA != task2SHA(1) || got[0].ShaB != task2SHA(2) ||
		got[0].QualityA != 70 || got[0].QualityB != 80 {
		t.Fatalf("pair not normalized: %+v", got[0])
	}
}

func TestScreenImagesReusesExpandedScratchForDenseBuckets(t *testing.T) {
	hash := [4]uint64{1, 2, 3, 4}
	feats := make([]ImageFeature, 512)
	for i := range feats {
		feats[i] = ImageFeature{
			SHA512:  task2SHA(i + 1),
			PDQ:     hash,
			Quality: 50,
			Width:   1,
			Height:  1,
		}
	}
	allocations := testing.AllocsPerRun(5, func() {
		if pairs := screenImages(feats, -1, 0.10, 50); len(pairs) != 0 {
			panic("negative Hamming threshold unexpectedly produced pairs")
		}
	})
	t.Logf("dense_screen_allocs_per_run=%.2f", allocations)
	if allocations > 100 {
		t.Fatalf("dense screen allocated %.2f times/run; expanded query scratch was not reused", allocations)
	}
}

func TestScreenImagesDeterministicAcrossInputOrder(t *testing.T) {
	rng := rand.New(rand.NewSource(17))
	var feats []ImageFeature
	for cluster := 0; cluster < 50; cluster++ {
		base := task2RandomPDQ(rng)
		for member := 0; member < 3; member++ {
			hash := base
			for bit := 0; bit < member+1; bit++ {
				hash[0] ^= uint64(1) << ((cluster + bit) % 64)
			}
			feats = append(feats, ImageFeature{
				SHA512:  task2SHA(cluster*3 + member + 1),
				PDQ:     hash,
				Quality: 80 - member,
				Width:   800,
				Height:  600,
			})
		}
	}
	want := screenImages(feats, 31, 0.10, 50)
	if len(want) != 150 {
		t.Fatalf("baseline pairs = %d, want 150", len(want))
	}
	for run := 0; run < 20; run++ {
		shuffled := append([]ImageFeature(nil), feats...)
		rng.Shuffle(len(shuffled), func(i, j int) {
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		})
		got := screenImages(shuffled, 31, 0.10, 50)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d is non-deterministic: got %d pairs, want %d", run, len(got), len(want))
		}
	}
}

func TestScreenImagesMatchesRandomizedNaiveExactBandOracle(t *testing.T) {
	rng := rand.New(rand.NewSource(91))
	feats := make([]ImageFeature, 0, 500)
	for cluster := 0; cluster < 200; cluster++ {
		base := task2RandomPDQ(rng)
		quality := 50 + rng.Intn(51)
		feats = append(feats, ImageFeature{
			SHA512: task2SHA(len(feats) + 1), PDQ: base, Quality: quality, Width: 800, Height: 600,
		})
		peer := task2MutateWithinFirstBand(base, 1+rng.Intn(32), rng)
		peerQuality, width, height := 50+rng.Intn(51), 800, 600
		switch cluster % 5 {
		case 0:
			peerQuality = 49
		case 1:
			width = 400
		case 2:
			width, height = 0, 0
		}
		feats = append(feats, ImageFeature{
			SHA512: task2SHA(len(feats) + 1), PDQ: peer, Quality: peerQuality, Width: width, Height: height,
		})
	}
	for len(feats) < cap(feats) {
		feats = append(feats, ImageFeature{
			SHA512: task2SHA(len(feats) + 1), PDQ: task2RandomPDQ(rng),
			Quality: 40 + rng.Intn(61), Width: 640, Height: 480,
		})
	}
	rng.Shuffle(len(feats), func(i, j int) { feats[i], feats[j] = feats[j], feats[i] })

	got := screenImages(feats, 31, 0.10, 50)
	want := task2NaiveImageOracle(feats, 31, 0.10, 50)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("screenImages differs from naive exact-band oracle: got=%d want=%d", len(got), len(want))
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

func BenchmarkScreenImagesFiftyThousandRandom(b *testing.B) {
	rng := rand.New(rand.NewSource(123))
	feats := make([]ImageFeature, 50000)
	for i := range feats {
		feats[i] = ImageFeature{
			SHA512: task2SHA(i + 1), PDQ: task2RandomPDQ(rng),
			Quality: 80, Width: 1920, Height: 1080,
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = screenImages(feats, 31, 0.10, 50)
	}
}

func task2SHA(value int) (sha [64]byte) {
	sha[60] = byte(value >> 24)
	sha[61] = byte(value >> 16)
	sha[62] = byte(value >> 8)
	sha[63] = byte(value)
	return sha
}

func task2RandomPDQ(rng *rand.Rand) [4]uint64 {
	return [4]uint64{rng.Uint64(), rng.Uint64(), rng.Uint64(), rng.Uint64()}
}

func task2MutateDistinct(base [4]uint64, count int, rng *rand.Rand) [4]uint64 {
	used := make(map[int]struct{}, count)
	for len(used) < count {
		position := rng.Intn(256)
		if _, exists := used[position]; exists {
			continue
		}
		used[position] = struct{}{}
		base[position/64] ^= uint64(1) << (position % 64)
	}
	return base
}

func task2MutateWithinFirstBand(base [4]uint64, count int, rng *rand.Rand) [4]uint64 {
	used := make(map[int]struct{}, count)
	for len(used) < count {
		position := rng.Intn(64)
		if _, exists := used[position]; exists {
			continue
		}
		used[position] = struct{}{}
		base[0] ^= uint64(1) << position
	}
	return base
}

func task2NaiveImageOracle(feats []ImageFeature, hammingMax int, aspectTol float64, qualityMin int) []CandidatePair {
	var pairs []CandidatePair
	for i := 0; i < len(feats); i++ {
		if feats[i].Quality < qualityMin {
			continue
		}
		for j := i + 1; j < len(feats); j++ {
			if feats[j].Quality < qualityMin ||
				!task2SharesExactBand(feats[i].PDQ, feats[j].PDQ) ||
				!task2AspectOracle(feats[i], feats[j], aspectTol) {
				continue
			}
			distance := task2HammingOracle(feats[i].PDQ, feats[j].PDQ)
			if distance > hammingMax {
				continue
			}
			pairs = append(pairs, task2OraclePair(feats[i], feats[j], distance))
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

func task2SharesExactBand(a, b [4]uint64) bool {
	return a[0] == b[0] || a[1] == b[1] || a[2] == b[2] || a[3] == b[3]
}

func task2AspectOracle(a, b ImageFeature, tolerance float64) bool {
	if a.Width <= 0 || a.Height <= 0 || b.Width <= 0 || b.Height <= 0 {
		return true
	}
	ratioA := float64(a.Width) / float64(a.Height)
	ratioB := float64(b.Width) / float64(b.Height)
	return math.Abs(ratioA-ratioB)/math.Max(ratioA, ratioB) <= tolerance
}

func task2HammingOracle(a, b [4]uint64) int {
	total := 0
	for i := 0; i < 4; i++ {
		total += bits.OnesCount64(a[i] ^ b[i])
	}
	return total
}

func task2OraclePair(a, b ImageFeature, distance int) CandidatePair {
	if bytes.Compare(a.SHA512[:], b.SHA512[:]) > 0 {
		a, b = b, a
	}
	return CandidatePair{
		Kind:     KindImageCandidate,
		ShaA:     a.SHA512,
		ShaB:     b.SHA512,
		Hamming:  distance,
		QualityA: a.Quality,
		QualityB: b.Quality,
	}
}
