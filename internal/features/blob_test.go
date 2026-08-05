package features

import (
	"encoding/binary"
	"math"
	"math/rand"
	"reflect"
	"testing"
)

func TestPHashCodecUsesExactPortableBytes(t *testing.T) {
	parts := [9]uint64{0x0102030405060708, 1, 2, 3, 4, 5, 6, 7, 8}
	want := []byte{
		1, 3, 3, 0,
		8, 7, 6, 5, 4, 3, 2, 1,
		1, 0, 0, 0, 0, 0, 0, 0,
		2, 0, 0, 0, 0, 0, 0, 0,
		3, 0, 0, 0, 0, 0, 0, 0,
		4, 0, 0, 0, 0, 0, 0, 0,
		5, 0, 0, 0, 0, 0, 0, 0,
		6, 0, 0, 0, 0, 0, 0, 0,
		7, 0, 0, 0, 0, 0, 0, 0,
		8, 0, 0, 0, 0, 0, 0, 0,
	}
	if got := EncodePHashParts(parts); !reflect.DeepEqual(got, want) {
		t.Fatalf("EncodePHashParts() = %v, want %v", got, want)
	}
	got, err := DecodePHashParts(want)
	if err != nil || got != parts {
		t.Fatalf("DecodePHashParts() = %#v, %v; want %#v, nil", got, err, parts)
	}
}

func TestPHashDecoderRejectsMalformedHeadersAndLengths(t *testing.T) {
	valid := EncodePHashParts([9]uint64{})
	for _, tt := range []struct {
		name string
		blob []byte
	}{
		{"short", valid[:75]},
		{"long", append(valid, 0)},
		{"version", append([]byte{2}, valid[1:]...)},
		{"rows", append([]byte{1, 4, 3, 0}, valid[4:]...)},
		{"columns", append([]byte{1, 3, 4, 0}, valid[4:]...)},
		{"flags", append([]byte{1, 3, 3, 1}, valid[4:]...)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := DecodePHashParts(tt.blob); err == nil {
				t.Fatal("DecodePHashParts accepted malformed BLOB")
			}
		})
	}
}

func TestSobelCodecUsesExactPortableBytes(t *testing.T) {
	var hist [128]float32
	hist[0], hist[1], hist[127] = 1.5, -2.25, 0.5
	got, err := EncodeSobelHist(hist)
	if err != nil {
		t.Fatalf("EncodeSobelHist: %v", err)
	}
	if len(got) != 516 || !reflect.DeepEqual(got[:4], []byte{1, 4, 8, 0}) {
		t.Fatalf("Sobel BLOB header/length = %v/%d", got[:4], len(got))
	}
	if bits := binary.LittleEndian.Uint32(got[4:8]); bits != math.Float32bits(1.5) {
		t.Fatalf("first value bits = %#x, want %#x", bits, math.Float32bits(1.5))
	}
	decoded, err := DecodeSobelHist(got)
	if err != nil || decoded != hist {
		t.Fatalf("DecodeSobelHist() = %#v, %v; want %#v, nil", decoded, err, hist)
	}
}

func TestSobelCodecRejectsMalformedAndNonFiniteValues(t *testing.T) {
	var valid [128]float32
	blob, err := EncodeSobelHist(valid)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []float32{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))} {
		var hist [128]float32
		hist[12] = bad
		if _, err := EncodeSobelHist(hist); err == nil {
			t.Fatalf("EncodeSobelHist accepted %v", bad)
		}
	}
	for _, tt := range []struct {
		name string
		blob []byte
	}{
		{"short", blob[:515]},
		{"long", append(blob, 0)},
		{"version", append([]byte{2}, blob[1:]...)},
		{"grid", append([]byte{1, 3, 8, 0}, blob[4:]...)},
		{"bins", append([]byte{1, 4, 7, 0}, blob[4:]...)},
		{"flags", append([]byte{1, 4, 8, 1}, blob[4:]...)},
		{"nan", func() []byte {
			b := append([]byte(nil), blob...)
			binary.LittleEndian.PutUint32(b[4:], math.Float32bits(float32(math.NaN())))
			return b
		}()},
		{"positive infinity", func() []byte {
			b := append([]byte(nil), blob...)
			binary.LittleEndian.PutUint32(b[4:], math.Float32bits(float32(math.Inf(1))))
			return b
		}()},
		{"negative infinity", func() []byte {
			b := append([]byte(nil), blob...)
			binary.LittleEndian.PutUint32(b[4:], math.Float32bits(float32(math.Inf(-1))))
			return b
		}()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := DecodeSobelHist(tt.blob); err == nil {
				t.Fatal("DecodeSobelHist accepted malformed BLOB")
			}
		})
	}
}

func TestBlobCodecsRoundTripRandomizedData(t *testing.T) {
	rng := rand.New(rand.NewSource(20260728))
	for n := 0; n < 100; n++ {
		var parts [9]uint64
		var hist [128]float32
		for i := range parts {
			parts[i] = rng.Uint64()
		}
		for i := range hist {
			hist[i] = rng.Float32()*2 - 1
		}
		p, err := DecodePHashParts(EncodePHashParts(parts))
		if err != nil || p != parts {
			t.Fatalf("iteration %d pHash round trip = %#v, %v", n, p, err)
		}
		blob, err := EncodeSobelHist(hist)
		if err != nil {
			t.Fatalf("iteration %d EncodeSobelHist: %v", n, err)
		}
		h, err := DecodeSobelHist(blob)
		if err != nil || h != hist {
			t.Fatalf("iteration %d Sobel round trip = %#v, %v", n, h, err)
		}
	}
}

func TestHamming64MatchesIndependentBitOracle(t *testing.T) {
	values := [][2]uint64{{0, 0}, {0, ^uint64(0)}, {0x0102030405060708, 0x0807060504030201}}
	rng := rand.New(rand.NewSource(99))
	for n := 0; n < 100; n++ {
		values = append(values, [2]uint64{rng.Uint64(), rng.Uint64()})
	}
	for _, pair := range values {
		want := 0
		for x := pair[0] ^ pair[1]; x != 0; x >>= 1 {
			want += int(x & 1)
		}
		if got := Hamming64(pair[0], pair[1]); got != want {
			t.Fatalf("Hamming64(%#x, %#x) = %d, want %d", pair[0], pair[1], got, want)
		}
	}
}

func TestSobelCosineDefinesZeroVectorsAndClampsResults(t *testing.T) {
	var zero, x, y [128]float32
	x[0], y[0], y[1] = 1, 1, 1
	if got := SobelCosine(zero, zero); got != 1 {
		t.Fatalf("both-zero cosine = %v, want 1", got)
	}
	if got := SobelCosine(zero, x); got != 0 {
		t.Fatalf("one-zero cosine = %v, want 0", got)
	}
	if got := SobelCosine(x, y); math.Abs(got-1/math.Sqrt2) > 1e-12 {
		t.Fatalf("cosine = %.16f, want %.16f", got, 1/math.Sqrt2)
	}
	for i := range x {
		x[i] = 1e30
		y[i] = 1e30
	}
	if got := SobelCosine(x, y); math.IsNaN(got) || math.IsInf(got, 0) || got < -1 || got > 1 {
		t.Fatalf("large-vector cosine = %v, want finite value in [-1, 1]", got)
	}
}
