package firstscreen

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestDefaultConfigValidatesAndUsesDocumentedValues(t *testing.T) {
	got := DefaultConfig()
	want := Config{
		HammingMax:            31,
		AspectTolerance:       0.10,
		VideoDurationWindowMs: 2000,
		ImageQualityMin:       50,
		ReadPageSize:          50000,
		GroupInsertBatch:      1000,
		SHAResolveChunk:       10000,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DefaultConfig() = %+v, want %+v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("DefaultConfig().Validate() = %v", err)
	}
}

func TestConfigValidateRejectsOutOfRangeValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"negative hamming", func(c *Config) { c.HammingMax = -1 }},
		{"hamming over 256", func(c *Config) { c.HammingMax = 257 }},
		{"negative aspect tolerance", func(c *Config) { c.AspectTolerance = -0.01 }},
		{"aspect tolerance over one", func(c *Config) { c.AspectTolerance = 1.01 }},
		{"negative duration window", func(c *Config) { c.VideoDurationWindowMs = -1 }},
		{"negative quality", func(c *Config) { c.ImageQualityMin = -1 }},
		{"quality over 100", func(c *Config) { c.ImageQualityMin = 101 }},
		{"zero read page", func(c *Config) { c.ReadPageSize = 0 }},
		{"negative read page", func(c *Config) { c.ReadPageSize = -1 }},
		{"zero insert batch", func(c *Config) { c.GroupInsertBatch = 0 }},
		{"negative insert batch", func(c *Config) { c.GroupInsertBatch = -1 }},
		{"zero SHA chunk", func(c *Config) { c.SHAResolveChunk = 0 }},
		{"negative SHA chunk", func(c *Config) { c.SHAResolveChunk = -1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultConfig()
			test.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("Validate() accepted %+v", cfg)
			}
		})
	}
}

func TestConfigValidateAcceptsInclusiveFilterBoundaries(t *testing.T) {
	cfg := DefaultConfig()
	cfg.HammingMax = 0
	cfg.AspectTolerance = 0
	cfg.VideoDurationWindowMs = 0
	cfg.ImageQualityMin = 0
	if err := cfg.Validate(); err != nil {
		t.Fatalf("lower boundaries: %v", err)
	}
	cfg.HammingMax = 256
	cfg.AspectTolerance = 1
	cfg.ImageQualityMin = 100
	if err := cfg.Validate(); err != nil {
		t.Fatalf("upper boundaries: %v", err)
	}
}

func TestConfigValidateRejectsNaNAspectTolerance(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AspectTolerance = math.NaN()
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() accepted NaN aspect tolerance")
	}
}

func TestPDQFromBytesUsesBigEndianAndRejectsWrongLength(t *testing.T) {
	raw := make([]byte, 32)
	for i, word := range []uint64{
		0x0102030405060708,
		0x1112131415161718,
		0x2122232425262728,
		0x3132333435363738,
	} {
		binary.BigEndian.PutUint64(raw[i*8:(i+1)*8], word)
	}
	got, ok := pdqFromBytes(raw)
	want := [4]uint64{
		0x0102030405060708,
		0x1112131415161718,
		0x2122232425262728,
		0x3132333435363738,
	}
	if !ok || got != want {
		t.Fatalf("pdqFromBytes() = %x, %t; want %x, true", got, ok, want)
	}
	for _, size := range []int{0, 31, 33} {
		if _, ok := pdqFromBytes(make([]byte, size)); ok {
			t.Fatalf("pdqFromBytes accepted %d bytes", size)
		}
	}
}

func TestSHAFromTextRequiresCanonicalSHA512(t *testing.T) {
	valid := strings.Repeat("ab", 64)
	got, ok := shaFromText(valid)
	if !ok || got[0] != 0xab || got[63] != 0xab {
		t.Fatalf("shaFromText(canonical) = %x, %t", got, ok)
	}
	for _, bad := range []string{
		strings.ToUpper(valid),
		valid[:127],
		valid + "0",
		strings.Repeat("gg", 64),
	} {
		if _, ok := shaFromText(bad); ok {
			t.Fatalf("shaFromText accepted %q", bad)
		}
	}
}

func TestHamming256CountsAllFourWords(t *testing.T) {
	a := [4]uint64{}
	b := [4]uint64{^uint64(0), 1, 3, 7}
	if got := hamming256(a, b); got != 70 {
		t.Fatalf("hamming256() = %d, want 70", got)
	}
	if got := hamming256(b, b); got != 0 {
		t.Fatalf("self hamming = %d, want 0", got)
	}
}

func TestCandidatePairNormalizesSHAOrderAndQualities(t *testing.T) {
	var low, high [64]byte
	low[63] = 1
	high[0] = 0xab
	got := newCandidatePair(KindImageCandidate, high, low, 17, 0, 82, 76)
	if got.ShaA != low || got.ShaB != high {
		t.Fatalf("SHA order = %x, %x", got.ShaA, got.ShaB)
	}
	if got.QualityA != 76 || got.QualityB != 82 {
		t.Fatalf("qualities = %d, %d; want 76, 82", got.QualityA, got.QualityB)
	}
	if !got.less(newCandidatePair(KindImageCandidate, high, [64]byte{1}, 0, 0, 0, 0)) {
		t.Fatal("less did not order by normalized SHA")
	}
}

func TestScoreJSONHasExactFieldsAndNormalizedPeers(t *testing.T) {
	var low, high [64]byte
	low[63] = 1
	high[0] = 0xab
	pair := newCandidatePair(KindImageCandidate, high, low, 17, 380, 82, 76)
	lowHex := strings.Repeat("0", 126) + "01"
	highHex := "ab" + strings.Repeat("0", 126)

	assertJSONFields(t, pair.scoreJSON(true), map[string]any{
		"hamming":      float64(17),
		"quality_self": float64(76),
		"quality_peer": float64(82),
		"peer_sha512":  highHex,
	})
	assertJSONFields(t, pair.scoreJSON(false), map[string]any{
		"hamming":      float64(17),
		"quality_self": float64(82),
		"quality_peer": float64(76),
		"peer_sha512":  lowHex,
	})

	pair.Kind = KindVideoCandidate
	assertJSONFields(t, pair.scoreJSON(true), map[string]any{
		"hamming":          float64(17),
		"duration_diff_ms": float64(380),
		"quality_self":     float64(76),
		"quality_peer":     float64(82),
		"peer_sha512":      highHex,
	})

	pair.Kind = KindExact
	assertJSONFields(t, pair.scoreJSON(true), map[string]any{"basis": "sha512"})
}

func TestM3KindsContainOnlyM3OwnedKinds(t *testing.T) {
	want := []string{KindExact, KindImageCandidate, KindVideoCandidate}
	if !reflect.DeepEqual(M3Kinds, want) {
		t.Fatalf("M3Kinds = %#v, want %#v", M3Kinds, want)
	}
}

func assertJSONFields(t *testing.T, raw []byte, want map[string]any) {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("invalid JSON %q: %v", raw, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("JSON fields = %#v, want %#v", got, want)
	}
}
