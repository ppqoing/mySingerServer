//go:build windows

package wproc

import (
	"bytes"
	"context"
	"crypto/sha512"
	"errors"
	"image"
	"image/jpeg"
	"io"
	"os"

	"dedup/internal/worker"

	webpcodec "github.com/gen2brain/webp"
	_ "image/gif"
	_ "image/png"
)

var errPreviewTooLarge = errors.New("preview exceeds response limit")

// generateImagePreview runs only in the Worker process. It independently binds
// the source path to Agent's immutable identity and returns encoded bytes only.
func generateImagePreview(ctx context.Context, job *worker.JobMsg, memoryBudget int64) *worker.JobResultMsg {
	result := previewResult(job)
	if ctx == nil || job == nil || job.Kind != worker.MediaImage ||
		job.Phase != worker.PhasePreview || job.Size < 0 || job.MTimeUnix <= 0 ||
		len(job.KnownSHA) != sha512.Size || job.PreviewMaxWidth <= 0 ||
		job.PreviewMaxHeight <= 0 || job.PreviewQuality < 1 || job.PreviewQuality > 100 ||
		(job.PreviewFormat != worker.PreviewFormatJPEG && job.PreviewFormat != worker.PreviewFormatWebP) {
		return previewFailure(result, "preview_encode_failed")
	}
	if memoryBudget <= 0 || job.Size > memoryBudget {
		return previewFailure(result, "preview_memory_limit")
	}
	if err := ctx.Err(); err != nil {
		return previewFailure(result, "preview_io_failed")
	}

	pathInfo, err := os.Stat(job.Path)
	if err != nil {
		return previewFailure(result, "preview_io_failed")
	}
	if !matchesPreviewIdentity(pathInfo, job) {
		return previewFailure(result, "stale_preview")
	}
	file, err := os.Open(job.Path)
	if err != nil {
		return previewFailure(result, "preview_io_failed")
	}
	defer file.Close()
	handleBefore, err := file.Stat()
	if err != nil {
		return previewFailure(result, "preview_io_failed")
	}
	if !os.SameFile(pathInfo, handleBefore) || !samePreviewState(pathInfo, handleBefore) {
		return previewFailure(result, "stale_preview")
	}

	data, sum, err := readPreviewSource(ctx, file, job.Size)
	if err != nil {
		return previewFailure(result, "preview_io_failed")
	}
	handleAfter, handleErr := file.Stat()
	pathAfter, pathErr := os.Stat(job.Path)
	if handleErr != nil || pathErr != nil {
		return previewFailure(result, "preview_io_failed")
	}
	if !os.SameFile(pathInfo, pathAfter) || !os.SameFile(handleBefore, handleAfter) ||
		!os.SameFile(handleAfter, pathAfter) || !samePreviewState(handleBefore, handleAfter) ||
		!samePreviewState(pathInfo, pathAfter) || !matchesPreviewIdentity(pathAfter, job) ||
		!bytes.Equal(sum[:], job.KnownSHA) {
		return previewFailure(result, "stale_preview")
	}

	config, err := decodePreviewConfig(data)
	if err != nil {
		return previewFailure(result, "preview_decode_failed")
	}
	if !previewFitsMemoryBudget(job.Size, config.Width, config.Height,
		int(job.PreviewMaxWidth), int(job.PreviewMaxHeight), memoryBudget) {
		return previewFailure(result, "preview_memory_limit")
	}
	decoded, err := decodePreview(data)
	if err != nil {
		return previewFailure(result, "preview_decode_failed")
	}
	resized := resizeWithin(decoded, int(job.PreviewMaxWidth), int(job.PreviewMaxHeight))
	encoded, tooLarge, err := encodePreview(resized, job.PreviewFormat, int(job.PreviewQuality))
	if tooLarge {
		return previewFailure(result, "preview_too_large")
	}
	if err != nil {
		return previewFailure(result, "preview_encode_failed")
	}
	bounds := resized.Bounds()
	result.PreviewFormat = job.PreviewFormat
	result.PreviewWidth = int32(bounds.Dx())
	result.PreviewHeight = int32(bounds.Dy())
	result.PreviewBytes = encoded
	return result
}

func decodePreviewConfig(data []byte) (image.Config, error) {
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return webpcodec.DecodeConfig(bytes.NewReader(data))
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	return config, err
}

func previewFitsMemoryBudget(sourceBytes int64, width, height, maxWidth, maxHeight int, budget int64) bool {
	if sourceBytes < 0 || width <= 0 || height <= 0 || maxWidth <= 0 || maxHeight <= 0 || budget <= 0 {
		return false
	}
	decodedPixels, ok := safePreviewProduct(int64(width), int64(height))
	if !ok {
		return false
	}
	decodedBytes, ok := safePreviewProduct(decodedPixels, 8)
	if !ok {
		return false
	}
	targetWidth, targetHeight := resizeDimensions(width, height, maxWidth, maxHeight)
	resizedPixels, ok := safePreviewProduct(int64(targetWidth), int64(targetHeight))
	if !ok {
		return false
	}
	resizedBytes, ok := safePreviewProduct(resizedPixels, 4)
	if !ok {
		return false
	}
	total := int64(0)
	for _, amount := range []int64{sourceBytes, decodedBytes, resizedBytes, int64(worker.MaxPreviewBytes)} {
		if amount > budget-total {
			return false
		}
		total += amount
	}
	return true
}

func safePreviewProduct(left, right int64) (int64, bool) {
	if left < 0 || right < 0 || left != 0 && right > int64(^uint64(0)>>1)/left {
		return 0, false
	}
	return left * right, true
}

func resizeDimensions(width, height, maxWidth, maxHeight int) (int, int) {
	targetWidth, targetHeight := width, height
	if targetWidth > maxWidth {
		targetHeight = max(1, targetHeight*maxWidth/targetWidth)
		targetWidth = maxWidth
	}
	if targetHeight > maxHeight {
		targetWidth = max(1, targetWidth*maxHeight/targetHeight)
		targetHeight = maxHeight
	}
	return targetWidth, targetHeight
}

func previewResult(job *worker.JobMsg) *worker.JobResultMsg {
	if job == nil {
		return &worker.JobResultMsg{}
	}
	return &worker.JobResultMsg{
		JobID: job.JobID, ScanTaskID: job.ScanTaskID, Path: job.Path,
		Kind: job.Kind, Phase: job.Phase, ScreenStage: job.ScreenStage,
		Source: job.Source, SHA512: append([]byte(nil), job.KnownSHA...),
	}
}

func previewFailure(result *worker.JobResultMsg, code string) *worker.JobResultMsg {
	result.PreviewFormat = ""
	result.PreviewWidth = 0
	result.PreviewHeight = 0
	result.PreviewBytes = nil
	result.PreviewErrorCode = code
	return result
}

func matchesPreviewIdentity(info os.FileInfo, job *worker.JobMsg) bool {
	return info.Mode().IsRegular() && info.Size() == job.Size && info.ModTime().Unix() == job.MTimeUnix
}

func samePreviewState(left, right os.FileInfo) bool {
	return left.Size() == right.Size() && left.ModTime().UnixNano() == right.ModTime().UnixNano()
}

func readPreviewSource(ctx context.Context, source io.Reader, expected int64) ([]byte, [sha512.Size]byte, error) {
	var zero [sha512.Size]byte
	if expected < 0 || uint64(expected) > uint64(^uint(0)>>1) {
		return nil, zero, io.ErrUnexpectedEOF
	}
	data := make([]byte, 0, int(expected))
	hash := sha512.New()
	buffer := make([]byte, 128<<10)
	for {
		if err := ctx.Err(); err != nil {
			return nil, zero, err
		}
		n, err := source.Read(buffer)
		if n > 0 {
			data = append(data, buffer[:n]...)
			_, _ = hash.Write(buffer[:n])
			if int64(len(data)) > expected {
				return nil, zero, io.ErrUnexpectedEOF
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, zero, err
		}
		if n == 0 {
			return nil, zero, io.ErrNoProgress
		}
	}
	if int64(len(data)) != expected {
		return nil, zero, io.ErrUnexpectedEOF
	}
	copy(zero[:], hash.Sum(nil))
	return data, zero, nil
}

func decodePreview(data []byte) (image.Image, error) {
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return webpcodec.Decode(bytes.NewReader(data))
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	return decoded, err
}

func resizeWithin(source image.Image, maxWidth, maxHeight int) image.Image {
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	targetWidth, targetHeight := resizeDimensions(width, height, maxWidth, maxHeight)
	if targetWidth == width && targetHeight == height {
		return source
	}
	resized := image.NewNRGBA(image.Rect(0, 0, targetWidth, targetHeight))
	for y := range targetHeight {
		sourceY := bounds.Min.Y + y*height/targetHeight
		for x := range targetWidth {
			sourceX := bounds.Min.X + x*width/targetWidth
			resized.Set(x, y, source.At(sourceX, sourceY))
		}
	}
	return resized
}

func encodePreview(source image.Image, format string, quality int) ([]byte, bool, error) {
	var buffer bytes.Buffer
	limited := &previewLimitWriter{writer: &buffer, remaining: worker.MaxPreviewBytes}
	var err error
	switch format {
	case worker.PreviewFormatJPEG:
		err = jpeg.Encode(limited, source, &jpeg.Options{Quality: quality})
	case worker.PreviewFormatWebP:
		err = webpcodec.Encode(limited, source, webpcodec.Options{Quality: quality, Method: 4})
	default:
		err = errors.New("unsupported preview format")
	}
	if limited.exceeded || errors.Is(err, errPreviewTooLarge) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	return append([]byte(nil), buffer.Bytes()...), false, nil
}

type previewLimitWriter struct {
	writer    io.Writer
	remaining int
	exceeded  bool
}

func (writer *previewLimitWriter) Write(data []byte) (int, error) {
	if len(data) > writer.remaining {
		writer.exceeded = true
		return 0, errPreviewTooLarge
	}
	n, err := writer.writer.Write(data)
	writer.remaining -= n
	return n, err
}
