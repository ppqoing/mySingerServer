package firstscreen

import (
	"encoding/binary"
	"encoding/hex"
	"math/bits"
)

func hamming256(a, b [4]uint64) int {
	return bits.OnesCount64(a[0]^b[0]) +
		bits.OnesCount64(a[1]^b[1]) +
		bits.OnesCount64(a[2]^b[2]) +
		bits.OnesCount64(a[3]^b[3])
}

func pdqFromBytes(b []byte) ([4]uint64, bool) {
	var hash [4]uint64
	if len(b) != pdqLen {
		return hash, false
	}
	for i := range hash {
		hash[i] = binary.BigEndian.Uint64(b[i*8 : (i+1)*8])
	}
	return hash, true
}

func shaFromText(text string) ([64]byte, bool) {
	var sha [sha512Len]byte
	if len(text) != sha512Len*2 {
		return sha, false
	}
	for _, c := range text {
		if !('0' <= c && c <= '9') && !('a' <= c && c <= 'f') {
			return sha, false
		}
	}
	decoded, err := hex.DecodeString(text)
	if err != nil {
		return sha, false
	}
	copy(sha[:], decoded)
	return sha, true
}
