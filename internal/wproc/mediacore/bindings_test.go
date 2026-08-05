//go:build cgo && windows && legacy_mediacore

package mediacore

import (
	"bytes"
	"encoding/hex"
	"image"
	"image/color"
	"image/png"
	"runtime"
	"testing"
)

func TestVersionReportsLoadedDLL(t *testing.T) {
	if got, want := Version(), "1.0.0"; got != want {
		t.Fatalf("Version() = %q, want %q", got, want)
	}
}

func TestSHA512StreamsNISTABCVector(t *testing.T) {
	h, err := NewSHA512()
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	for _, chunk := range [][]byte{[]byte("a"), []byte("bc")} {
		if err := h.Update(chunk); err != nil {
			t.Fatal(err)
		}
	}
	got, err := h.Final()
	if err != nil {
		t.Fatal(err)
	}
	const want = "ddaf35a193617abacc417349ae20413112e6fa4e89a97ea20a9eeee64b55d39a2192992a274fc1a836ba3c23a3feebbd454d4423643ce80e2a9ac94fa54ca49f"
	if hex.EncodeToString(got[:]) != want {
		t.Fatalf("SHA-512(abc) = %x, want %s", got, want)
	}
	if _, err := h.Final(); err == nil {
		t.Fatal("second Final succeeded; want a closed/finalized error")
	}
}

func TestImagePhase1CopiesNativeOutput(t *testing.T) {
	src := testPNG(t, 19, 11)
	got, err := ImagePhase1(src)
	if err != nil {
		t.Fatal(err)
	}
	if got.Width != 19 || got.Height != 11 {
		t.Fatalf("dimensions = %dx%d, want 19x11", got.Width, got.Height)
	}
	firstHash := got.Hash

	for i := range src {
		src[i] = 0
	}
	runtime.GC()
	_ = make([]byte, 8<<20)
	if got.Hash != firstHash {
		t.Fatal("Go result changed after input/native-call lifetime ended")
	}
}

func TestImagePhase1ReturnsDecodeError(t *testing.T) {
	if _, err := ImagePhase1([]byte("not an image")); err == nil {
		t.Fatal("ImagePhase1 accepted invalid image bytes")
	}
}

func testPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8((x * 17) & 0xff),
				G: uint8((y * 23) & 0xff),
				B: uint8(((x + y) * 11) & 0xff),
				A: 0xff,
			})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
