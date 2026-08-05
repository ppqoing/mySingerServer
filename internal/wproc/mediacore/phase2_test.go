//go:build cgo && windows && legacy_mediacore

package mediacore

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"testing"
)

func TestDecodeFromMemoryRejectsEmptyAndCorruptInput(t *testing.T) {
	for name, data := range map[string][]byte{
		"nil":     nil,
		"empty":   {},
		"corrupt": []byte("not an image"),
	} {
		t.Run(name, func(t *testing.T) {
			if decoded, err := DecodeFromMemory(data); err == nil {
				decoded.Free()
				t.Fatal("DecodeFromMemory succeeded, want an input error")
			}
		})
	}
}

func TestGrayImagePhase2IsDeterministicAndReportsDimensions(t *testing.T) {
	decoded, err := DecodeFromMemory(testPNG(t, 96, 80))
	if err != nil {
		t.Fatal(err)
	}
	defer decoded.Free()

	first, err := decoded.Phase2()
	if err != nil {
		t.Fatal(err)
	}
	second, err := decoded.Phase2()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("Phase2 returned different output for the same decoded image")
	}
	if first.Width != 96 || first.Height != 80 {
		t.Fatalf("dimensions = %dx%d, want 96x80", first.Width, first.Height)
	}
	for i, value := range first.SobelHist {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			t.Fatalf("SobelHist[%d] = %v, want a finite value", i, value)
		}
	}
}

func TestPhase2ImageMatchesExplicitDecode(t *testing.T) {
	data := testJPEG(t, 91, 73)
	decoded, err := DecodeFromMemory(data)
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := decoded.Phase2()
	decoded.Free()
	if err != nil {
		t.Fatal(err)
	}

	convenience, err := Phase2Image(data)
	if err != nil {
		t.Fatal(err)
	}
	if convenience != explicit {
		t.Fatal("Phase2Image output differs from DecodeFromMemory followed by Phase2")
	}
}

func TestGrayImagePDQMatchesImagePhase1(t *testing.T) {
	data := testJPEG(t, 87, 69)
	legacy, err := ImagePhase1(data)
	if err != nil {
		t.Fatal(err)
	}

	decoded, err := DecodeFromMemory(data)
	if err != nil {
		t.Fatal(err)
	}
	defer decoded.Free()
	hash, quality, err := decoded.PDQ256()
	if err != nil {
		t.Fatal(err)
	}
	phase2, err := decoded.Phase2()
	if err != nil {
		t.Fatal(err)
	}
	if hash != legacy.Hash {
		t.Fatalf("PDQ hash = %x, want legacy hash %x", hash, legacy.Hash)
	}
	if quality != legacy.Quality {
		t.Fatalf("PDQ quality = %d, want legacy quality %d", quality, legacy.Quality)
	}
	if phase2.Width != legacy.Width || phase2.Height != legacy.Height {
		t.Fatalf(
			"decoded dimensions = %dx%d, want legacy %dx%d",
			phase2.Width,
			phase2.Height,
			legacy.Width,
			legacy.Height,
		)
	}
}

func TestPhase2ImagePropagatesNativeSmallImageError(t *testing.T) {
	if _, err := Phase2Image(testPNG(t, 7, 9)); err == nil {
		t.Fatal("Phase2Image accepted an image below the native 8x8 size boundary")
	}
}

func TestGrayImageFreeIsNilSafeIdempotentAndClosesMethods(t *testing.T) {
	var nilImage *GrayImage
	nilImage.Free()
	if _, _, err := nilImage.PDQ256(); err == nil {
		t.Fatal("PDQ256 on a nil GrayImage succeeded")
	}
	if _, err := nilImage.Phase2(); err == nil {
		t.Fatal("Phase2 on a nil GrayImage succeeded")
	}

	decoded, err := DecodeFromMemory(testPNG(t, 64, 64))
	if err != nil {
		t.Fatal(err)
	}
	decoded.Free()
	decoded.Free()
	if _, _, err := decoded.PDQ256(); err == nil {
		t.Fatal("PDQ256 after Free succeeded")
	}
	if _, err := decoded.Phase2(); err == nil {
		t.Fatal("Phase2 after Free succeeded")
	}
}

func testJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8((x*17 + y*3) & 0xff),
				G: uint8((y*23 + x*5) & 0xff),
				B: uint8(((x+y)*11 + x*y) & 0xff),
				A: 0xff,
			})
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 91}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
