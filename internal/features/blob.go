package features

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
)

const (
	pHashPartsBlobLen = 76
	sobelHistBlobLen  = 516
)

func EncodePHashParts(parts [9]uint64) []byte {
	blob := make([]byte, pHashPartsBlobLen)
	blob[0], blob[1], blob[2], blob[3] = 1, 3, 3, 0
	for i, part := range parts {
		binary.LittleEndian.PutUint64(blob[4+i*8:], part)
	}
	return blob
}

func DecodePHashParts(blob []byte) ([9]uint64, error) {
	var parts [9]uint64
	if len(blob) != pHashPartsBlobLen {
		return parts, fmt.Errorf("features: phash BLOB length %d, want %d", len(blob), pHashPartsBlobLen)
	}
	if blob[0] != 1 {
		return parts, fmt.Errorf("features: unsupported phash BLOB version %d", blob[0])
	}
	if blob[1] != 3 || blob[2] != 3 || blob[3] != 0 {
		return parts, fmt.Errorf("features: invalid phash BLOB header")
	}
	for i := range parts {
		parts[i] = binary.LittleEndian.Uint64(blob[4+i*8:])
	}
	return parts, nil
}

func EncodeSobelHist(hist [128]float32) ([]byte, error) {
	blob := make([]byte, sobelHistBlobLen)
	blob[0], blob[1], blob[2], blob[3] = 1, 4, 8, 0
	for i, value := range hist {
		if !isFiniteFloat32(value) {
			return nil, fmt.Errorf("features: sobel histogram value %d is not finite", i)
		}
		binary.LittleEndian.PutUint32(blob[4+i*4:], math.Float32bits(value))
	}
	return blob, nil
}

func DecodeSobelHist(blob []byte) ([128]float32, error) {
	var hist [128]float32
	if len(blob) != sobelHistBlobLen {
		return hist, fmt.Errorf("features: sobel BLOB length %d, want %d", len(blob), sobelHistBlobLen)
	}
	if blob[0] != 1 {
		return hist, fmt.Errorf("features: unsupported sobel BLOB version %d", blob[0])
	}
	if blob[1] != 4 || blob[2] != 8 || blob[3] != 0 {
		return hist, fmt.Errorf("features: invalid sobel BLOB header")
	}
	for i := range hist {
		hist[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[4+i*4:]))
		if !isFiniteFloat32(hist[i]) {
			return [128]float32{}, fmt.Errorf("features: sobel histogram value %d is not finite", i)
		}
	}
	return hist, nil
}

func Hamming64(a, b uint64) int {
	return bits.OnesCount64(a ^ b)
}

func SobelCosine(a, b [128]float32) float64 {
	var dot, normA, normB float64
	for i := range a {
		if !isFiniteFloat32(a[i]) || !isFiniteFloat32(b[i]) {
			return 0
		}
		av, bv := float64(a[i]), float64(b[i])
		dot += av * bv
		normA += av * av
		normB += bv * bv
	}
	if normA == 0 && normB == 0 {
		return 1
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	value := dot / math.Sqrt(normA*normB)
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	if value < -1 {
		return -1
	}
	if value > 1 {
		return 1
	}
	return value
}

func isFiniteFloat32(value float32) bool {
	return !math.IsNaN(float64(value)) && !math.IsInf(float64(value), 0)
}
