//go:build windows

package wproc

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"dedup/internal/worker"

	webpcodec "github.com/gen2brain/webp"
)

func TestImagePreviewWorkerEncodesJPEGAndWebPWithoutCreatingFiles(t *testing.T) {
	sourceDir := t.TempDir()
	taskTemp := t.TempDir()
	t.Setenv("TEMP", taskTemp)
	t.Setenv("TMP", taskTemp)
	path := filepath.Join(sourceDir, "source.jpg")
	writePreviewJPEG(t, path, 80, 40, false)
	beforeSource := directoryNames(t, sourceDir)
	beforeTemp := directoryNames(t, taskTemp)

	for _, format := range []string{worker.PreviewFormatJPEG, worker.PreviewFormatWebP} {
		t.Run(format, func(t *testing.T) {
			job := previewJobForFile(t, path, format)
			result := generateImagePreview(context.Background(), &job, 256<<20)
			if result.PreviewErrorCode != "" {
				t.Fatalf("generateImagePreview error = %q", result.PreviewErrorCode)
			}
			if result.PreviewWidth != 40 || result.PreviewHeight != 20 ||
				len(result.PreviewBytes) == 0 || len(result.PreviewBytes) > worker.MaxPreviewBytes {
				t.Fatalf("preview = %dx%d bytes=%d", result.PreviewWidth, result.PreviewHeight, len(result.PreviewBytes))
			}
			var decoded image.Image
			var err error
			if format == worker.PreviewFormatWebP {
				decoded, err = webpcodec.Decode(bytes.NewReader(result.PreviewBytes))
			} else {
				decoded, err = jpeg.Decode(bytes.NewReader(result.PreviewBytes))
			}
			if err != nil || decoded.Bounds().Dx() != 40 || decoded.Bounds().Dy() != 20 {
				t.Fatalf("decode %s = %v bounds=%v", format, err, decodedBounds(decoded))
			}
		})
	}
	if got := directoryNames(t, sourceDir); !reflect.DeepEqual(got, beforeSource) {
		t.Fatalf("source directory changed: before=%v after=%v", beforeSource, got)
	}
	if got := directoryNames(t, taskTemp); !reflect.DeepEqual(got, beforeTemp) {
		t.Fatalf("TEMP changed: before=%v after=%v", beforeTemp, got)
	}
}

// Break caught: the pre-read budget check treats the caller's maximum output
// dimensions as the actual image size and rejects a small, valid preview.
func TestImagePreviewWorkerBudgetsActualDimensionsAfterReadingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "small.webp")
	writePreviewWebP(t, path, 16, 8, false)
	job := previewJobForFile(t, path, worker.PreviewFormatWebP)
	job.PreviewMaxWidth, job.PreviewMaxHeight = 8192, 8192
	result := generateImagePreview(context.Background(), &job, 256<<20)
	if result.PreviewErrorCode != "" || result.PreviewWidth != 16 || result.PreviewHeight != 8 {
		t.Fatalf("small preview = code:%q dimensions:%dx%d", result.PreviewErrorCode,
			result.PreviewWidth, result.PreviewHeight)
	}
}

func TestStalePreviewWorkerRejectsImmutableIdentityMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.jpg")
	writePreviewJPEG(t, path, 40, 20, false)
	base := previewJobForFile(t, path, worker.PreviewFormatJPEG)
	tests := []struct {
		name   string
		mutate func(*worker.JobMsg)
	}{
		{"size", func(job *worker.JobMsg) { job.Size++ }},
		{"mtime", func(job *worker.JobMsg) { job.MTimeUnix-- }},
		{"SHA", func(job *worker.JobMsg) { job.KnownSHA[0]++ }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			job := base
			job.KnownSHA = append([]byte(nil), base.KnownSHA...)
			test.mutate(&job)
			result := generateImagePreview(context.Background(), &job, 256<<20)
			if result.PreviewErrorCode != "stale_preview" || len(result.PreviewBytes) != 0 {
				t.Fatalf("stale result = code:%q bytes:%d", result.PreviewErrorCode, len(result.PreviewBytes))
			}
		})
	}
}

func TestImagePreviewWorkerEnforcesFourMiBWhileEncoding(t *testing.T) {
	if testing.Short() {
		t.Skip("large deterministic image")
	}
	path := filepath.Join(t.TempDir(), "large.jpg")
	writePreviewJPEG(t, path, 2600, 2600, true)
	job := previewJobForFile(t, path, worker.PreviewFormatJPEG)
	job.PreviewMaxWidth = 2600
	job.PreviewMaxHeight = 2600
	job.PreviewQuality = 100
	result := generateImagePreview(context.Background(), &job, 256<<20)
	if result.PreviewErrorCode != "preview_too_large" || len(result.PreviewBytes) != 0 {
		t.Fatalf("large preview = code:%q bytes:%d", result.PreviewErrorCode, len(result.PreviewBytes))
	}
}

// Break caught: a tiny compressed image with forged huge dimensions reaches
// image.Decode and allocates independently of WPROC_IMAGE_MEM_MB.
func TestImagePreviewWorkerRejectsSourceAndDecodedPixelsOutsideMemoryBudget(t *testing.T) {
	t.Run("source bytes", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "source.jpg")
		writePreviewJPEG(t, path, 20, 10, false)
		job := previewJobForFile(t, path, worker.PreviewFormatJPEG)
		result := generateImagePreview(context.Background(), &job, job.Size-1)
		if result.PreviewErrorCode != "preview_memory_limit" || len(result.PreviewBytes) != 0 {
			t.Fatalf("source budget result = code:%q bytes:%d", result.PreviewErrorCode, len(result.PreviewBytes))
		}
	})
	t.Run("decoded pixels", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bomb.png")
		writeHugePreviewPNGHeader(t, path, 100000, 100000)
		job := previewJobForFile(t, path, worker.PreviewFormatJPEG)
		result := generateImagePreview(context.Background(), &job, 8<<20)
		if result.PreviewErrorCode != "preview_memory_limit" || len(result.PreviewBytes) != 0 {
			t.Fatalf("decoded budget result = code:%q bytes:%d", result.PreviewErrorCode, len(result.PreviewBytes))
		}
	})
}

// Break caught: DecodeConfig reports only canvas dimensions, then Decode on an
// animated WebP materializes every frame in the WASM module before returning.
func TestImagePreviewWorkerRejectsAnimatedWebPBeforeCodec(t *testing.T) {
	path := filepath.Join(t.TempDir(), "animated.bin")
	writeAnimatedPreviewWebP(t, path)
	job := previewJobForFile(t, path, worker.PreviewFormatJPEG)
	result := generateImagePreview(context.Background(), &job, 256<<20)
	if result.PreviewErrorCode != "preview_memory_limit" || len(result.PreviewBytes) != 0 {
		t.Fatalf("animated preview = code:%q bytes:%d", result.PreviewErrorCode, len(result.PreviewBytes))
	}
}

func TestInspectPreviewWebPUsesConstantMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "static.webp")
	writePreviewWebP(t, path, 20, 10, false)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	allocations := testing.AllocsPerRun(1000, func() {
		isWebP, animated, inspectErr := inspectPreviewWebP(data)
		if inspectErr != nil || !isWebP || animated {
			panic("static WebP inspection failed")
		}
	})
	if allocations != 0 {
		t.Fatalf("RIFF inspection allocations = %v, want 0", allocations)
	}
}

// Break caught: static WebP metadata/source duplication and fallback codec
// copies are omitted, admitting a request whose simultaneous live set exceeds
// the configured budget before decode or encode returns.
func TestStaticWebPMemoryBudgetIncludesMetadataBackendCopiesAndEncodedResultCopies(t *testing.T) {
	t.Run("large metadata source", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "metadata.bin")
		writePreviewWebP(t, path, 2, 2, false)
		appendWebPChunk(t, path, "JUNK", make([]byte, 7<<20))
		job := previewJobForFile(t, path, worker.PreviewFormatJPEG)
		job.PreviewMaxWidth, job.PreviewMaxHeight = 2, 2
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		result := generateImagePreview(context.Background(), &job, 24<<20)
		runtime.ReadMemStats(&after)
		if result.PreviewErrorCode != "preview_memory_limit" || len(result.PreviewBytes) != 0 {
			t.Fatalf("metadata preview = code:%q bytes:%d", result.PreviewErrorCode, len(result.PreviewBytes))
		}
		if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
			t.Fatalf("metadata rejection allocated %d bytes before codec", allocated)
		}
	})
	t.Run("webp encode live set", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "noisy.webp")
		writePreviewWebP(t, path, 1800, 1800, true)
		job := previewJobForFile(t, path, worker.PreviewFormatWebP)
		job.PreviewMaxWidth, job.PreviewMaxHeight = 1800, 1800
		job.PreviewQuality = 100
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		result := generateImagePreview(context.Background(), &job, 64<<20)
		runtime.ReadMemStats(&after)
		if result.PreviewErrorCode != "preview_memory_limit" || len(result.PreviewBytes) != 0 {
			t.Fatalf("encode live-set preview = code:%q bytes:%d", result.PreviewErrorCode, len(result.PreviewBytes))
		}
		if allocated := after.TotalAlloc - before.TotalAlloc; allocated > uint64(job.Size)+(2<<20) {
			t.Fatalf("encode live-set rejection allocated %d bytes before codec", allocated)
		}
	})
}

func previewJobForFile(t *testing.T, path, format string) worker.JobMsg {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha512.Sum512(data)
	return worker.JobMsg{
		JobID: 801, ScanTaskID: "preview-801", Path: path, Kind: worker.MediaImage,
		Phase: worker.PhasePreview, ScreenStage: worker.ScreenStagePreview,
		Source: worker.JobSourceLocal, Size: info.Size(), MTimeUnix: info.ModTime().Unix(),
		KnownSHA: append([]byte(nil), sum[:]...), PreviewFormat: format,
		PreviewMaxWidth: 40, PreviewMaxHeight: 40, PreviewQuality: 82,
	}
}

func writePreviewJPEG(t *testing.T, path string, width, height int, noisy bool) {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	state := uint32(1)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			if noisy {
				state = state*1664525 + 1013904223
				img.SetNRGBA(x, y, color.NRGBA{R: uint8(state), G: uint8(state >> 8), B: uint8(state >> 16), A: 255})
			} else {
				img.SetNRGBA(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 120, A: 255})
			}
		}
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(file, img, &jpeg.Options{Quality: 95}); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeHugePreviewPNGHeader(t *testing.T, path string, width, height uint32) {
	t.Helper()
	data := []byte{
		0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n',
		0, 0, 0, 13, 'I', 'H', 'D', 'R',
		0, 0, 0, 1, 0, 0, 0, 1,
		8, 2, 0, 0, 0,
		0, 0, 0, 0,
	}
	binary.BigEndian.PutUint32(data[16:20], width)
	binary.BigEndian.PutUint32(data[20:24], height)
	binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writePreviewWebP(t *testing.T, path string, width, height int, noisy bool) {
	t.Helper()
	img := previewTestImage(width, height, noisy)
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := webpcodec.Encode(file, img, webpcodec.Options{Quality: 90, Method: 4}); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeAnimatedPreviewWebP(t *testing.T, path string) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	animation := &webpcodec.WEBP{
		Image: []image.Image{previewTestImage(2, 2, false), previewTestImage(2, 2, true)},
		Delay: []int{20, 20}, LoopCount: 0,
	}
	if err := webpcodec.EncodeAll(file, animation, webpcodec.Options{Quality: 80, Method: 4}); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func appendWebPChunk(t *testing.T, path, fourCC string, payload []byte) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(fourCC) != 4 || len(data) < 12 {
		t.Fatal("invalid test WebP")
	}
	data = append(data, fourCC...)
	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], uint32(len(payload)))
	data = append(data, size[:]...)
	data = append(data, payload...)
	if len(payload)%2 != 0 {
		data = append(data, 0)
	}
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func previewTestImage(width, height int, noisy bool) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	state := uint32(1)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			if noisy {
				state = state*1664525 + 1013904223
				img.SetNRGBA(x, y, color.NRGBA{R: uint8(state), G: uint8(state >> 8), B: uint8(state >> 16), A: 255})
			} else {
				img.SetNRGBA(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 120, A: 255})
			}
		}
	}
	return img
}

func directoryNames(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(entries))
	for index, entry := range entries {
		names[index] = entry.Name()
	}
	return names
}

func decodedBounds(img image.Image) image.Rectangle {
	if img == nil {
		return image.Rectangle{}
	}
	return img.Bounds()
}
