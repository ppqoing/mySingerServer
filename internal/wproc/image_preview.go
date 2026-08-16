//go:build windows

package wproc

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/binary"
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

const previewWASMBaseBytes int64 = 128 << 10

// generateImagePreview runs only in the Worker process. It independently binds
// the source path to Agent's immutable identity and returns encoded bytes only.
func generateImagePreview(ctx context.Context, job *worker.JobMsg, memoryBudget int64) *worker.JobResultMsg {
	return generateImagePreviewWithOpen(ctx, job, memoryBudget, func() (previewSourceFile, error) {
		return os.Open(job.Path)
	})
}

type previewSourceFile interface {
	io.ReadSeeker
	Stat() (os.FileInfo, error)
	Close() error
}

func generateImagePreviewWithOpen(
	ctx context.Context,
	job *worker.JobMsg,
	memoryBudget int64,
	open func() (previewSourceFile, error),
) *worker.JobResultMsg {
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
	if open == nil {
		return previewFailure(result, "preview_io_failed")
	}
	file, err := open()
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
	sourceWebP, err := previewSourceIsWebP(file, job.Size)
	if err != nil {
		return previewFailure(result, "preview_io_failed")
	}
	if !previewFitsInitialMemoryBudget(job.Size, memoryBudget, sourceWebP) {
		return previewFailure(result, "preview_memory_limit")
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

	isWebP, animated, err := inspectPreviewWebP(data)
	if err != nil {
		return previewFailure(result, "preview_decode_failed")
	}
	if animated {
		return previewFailure(result, "preview_memory_limit")
	}
	if isWebP != sourceWebP {
		return previewFailure(result, "stale_preview")
	}
	config, err := decodePreviewConfig(data)
	if err != nil {
		return previewFailure(result, "preview_decode_failed")
	}
	if !previewFitsMemoryBudget(job.Size, config.Width, config.Height,
		int(job.PreviewMaxWidth), int(job.PreviewMaxHeight), memoryBudget,
		isWebP, job.PreviewFormat) {
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

func previewSourceIsWebP(file io.ReadSeeker, size int64) (bool, error) {
	if size < 12 {
		return false, nil
	}
	var header [12]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		return false, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	return header[0] == 'R' && header[1] == 'I' && header[2] == 'F' && header[3] == 'F' &&
		header[8] == 'W' && header[9] == 'E' && header[10] == 'B' && header[11] == 'P', nil
}

// inspectPreviewWebP walks the RIFF chunk table without allocating. Animated
// WebP is outside the product contract and must not reach either codec backend.
func inspectPreviewWebP(data []byte) (isWebP, animated bool, err error) {
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return false, false, nil
	}
	declared := uint64(binary.LittleEndian.Uint32(data[4:8])) + 8
	if declared != uint64(len(data)) {
		return true, false, errors.New("invalid WebP RIFF size")
	}
	for offset := uint64(12); offset < declared; {
		if declared-offset < 8 {
			return true, false, errors.New("truncated WebP chunk")
		}
		start := int(offset)
		fourCC := string(data[start : start+4])
		size := uint64(binary.LittleEndian.Uint32(data[start+4 : start+8]))
		payload := offset + 8
		end := payload + size
		if end < payload || end > declared {
			return true, false, errors.New("invalid WebP chunk size")
		}
		if fourCC == "ANIM" || fourCC == "ANMF" {
			return true, true, nil
		}
		if fourCC == "VP8X" {
			if size < 10 {
				return true, false, errors.New("invalid WebP VP8X chunk")
			}
			if data[int(payload)]&0x02 != 0 {
				return true, true, nil
			}
		}
		offset = end + size%2
		if offset < end || offset > declared {
			return true, false, errors.New("invalid WebP padding")
		}
	}
	return true, false, nil
}

func decodePreviewConfig(data []byte) (image.Config, error) {
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return webpcodec.DecodeConfig(bytes.NewReader(data))
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	return config, err
}

func previewFitsMemoryBudget(sourceBytes int64, width, height, maxWidth, maxHeight int,
	budget int64, sourceWebP bool, outputFormat string,
) bool {
	if sourceBytes < 0 || width <= 0 || height <= 0 || maxWidth <= 0 || maxHeight <= 0 || budget <= 0 {
		return false
	}
	decodedPixels, ok := safePreviewProduct(int64(width), int64(height))
	if !ok {
		return false
	}
	decodedFactor := int64(8)
	sourceFactor := int64(1)
	if sourceWebP {
		// The nodynamic backend keeps the caller buffer while io.ReadAll copies
		// it and copies it again into a growing WASM linear memory. Conservative
		// factors include transient slice growth and decoder scratch.
		sourceFactor = 6
		decodedFactor = 16
	}
	sourceLive, ok := safePreviewProduct(sourceBytes, sourceFactor)
	if !ok {
		return false
	}
	decodedBytes, ok := safePreviewProduct(decodedPixels, decodedFactor)
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
	encodePixels, encodedBytes, ok := previewEncodeMemory(resizedPixels, outputFormat)
	if !ok {
		return false
	}
	fixedBytes := int64(0)
	if sourceWebP {
		fixedBytes = previewWASMBaseBytes
	}
	if outputFormat == worker.PreviewFormatWebP {
		var added bool
		fixedBytes, added = safePreviewAdd(fixedBytes, previewWASMBaseBytes)
		if !added {
			return false
		}
	}
	total := int64(0)
	for _, amount := range []int64{sourceLive, decodedBytes, resizedBytes, encodePixels, encodedBytes, fixedBytes} {
		var added bool
		total, added = safePreviewAdd(total, amount)
		if !added || total > budget {
			return false
		}
	}
	return true
}

func previewFitsInitialMemoryBudget(sourceBytes, budget int64, sourceWebP bool) bool {
	if sourceBytes < 0 || budget <= 0 {
		return false
	}
	sourceFactor := int64(1)
	if sourceWebP {
		sourceFactor = 6
	}
	sourceLive, ok := safePreviewProduct(sourceBytes, sourceFactor)
	if !ok {
		return false
	}
	if sourceWebP {
		sourceLive, ok = safePreviewAdd(sourceLive, previewWASMBaseBytes)
	}
	return ok && sourceLive <= budget
}

func previewEncodeMemory(targetPixels int64, outputFormat string) (pixels, encoded int64, ok bool) {
	encodePixelsFactor := int64(4)
	encodedCopies := int64(2)
	if outputFormat == worker.PreviewFormatWebP {
		// NRGBA conversion/input, WASM linear-memory growth and libwebp scratch
		// coexist with WASM output, bytes.Buffer storage, and the returned copy.
		encodePixelsFactor = 24
		encodedCopies = 5
	}
	pixels, ok = safePreviewProduct(targetPixels, encodePixelsFactor)
	if !ok {
		return 0, 0, false
	}
	encoded, ok = safePreviewProduct(int64(worker.MaxPreviewBytes), encodedCopies)
	return pixels, encoded, ok
}

func safePreviewProduct(left, right int64) (int64, bool) {
	if left < 0 || right < 0 || left != 0 && right > int64(^uint64(0)>>1)/left {
		return 0, false
	}
	return left * right, true
}

func safePreviewAdd(left, right int64) (int64, bool) {
	if left < 0 || right < 0 || right > int64(^uint64(0)>>1)-left {
		return 0, false
	}
	return left + right, true
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
