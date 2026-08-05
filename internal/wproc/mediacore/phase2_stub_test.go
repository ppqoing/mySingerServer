//go:build !cgo || !windows || !legacy_mediacore

package mediacore

import (
	"errors"
	"testing"
)

var (
	_ func([]byte) (*GrayImage, error)                   = DecodeFromMemory
	_ func(*GrayImage) ([PDQ256Bytes]byte, int32, error) = (*GrayImage).PDQ256
	_ func(*GrayImage) (Phase2Result, error)             = (*GrayImage).Phase2
	_ func(*GrayImage)                                   = (*GrayImage).Free
	_ func([]byte) (Phase2Result, error)                 = Phase2Image
)

func TestUnavailablePhase2API(t *testing.T) {
	decoded, err := DecodeFromMemory([]byte("image"))
	if decoded != nil {
		t.Fatalf("DecodeFromMemory returned %#v, want nil", decoded)
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("DecodeFromMemory error = %v, want ErrUnavailable", err)
	}
	if _, err := Phase2Image([]byte("image")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Phase2Image error = %v, want ErrUnavailable", err)
	}

	var nilImage *GrayImage
	if _, _, err := nilImage.PDQ256(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("PDQ256 error = %v, want ErrUnavailable", err)
	}
	if _, err := nilImage.Phase2(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Phase2 error = %v, want ErrUnavailable", err)
	}
	nilImage.Free()

	decoded = &GrayImage{}
	decoded.Free()
	decoded.Free()
}
