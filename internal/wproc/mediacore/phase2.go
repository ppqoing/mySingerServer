//go:build cgo && windows && legacy_mediacore

package mediacore

/*
#cgo CFLAGS: -I${SRCDIR}/../../../mediacore/include
#cgo LDFLAGS: -L${SRCDIR} -lmediacore
#include <stdatomic.h>
#include <stdlib.h>
#include <mediacore/mediacore.h>

typedef struct mc_go_boundary_call_counts {
	uint64_t decode;
	uint64_t pdq256;
	uint64_t phase2;
	uint64_t phash_parts;
	uint64_t sobel_hist;
} mc_go_boundary_call_counts;

static _Atomic uint64_t mc_go_decode_calls;
static _Atomic uint64_t mc_go_pdq256_calls;
static _Atomic uint64_t mc_go_phase2_calls;
static _Atomic uint64_t mc_go_phash_parts_calls;
static _Atomic uint64_t mc_go_sobel_hist_calls;

static int mc_go_decode_gray(
	const uint8_t* buf,
	size_t len,
	mc_image* out,
	char* errbuf,
	size_t errbuf_len) {
	atomic_fetch_add_explicit(
		&mc_go_decode_calls, UINT64_C(1), memory_order_relaxed);
	return mc_decode_gray(buf, len, out, errbuf, errbuf_len);
}

static int mc_go_pdq256_from_gray(
	const uint8_t* gray,
	int32_t width,
	int32_t height,
	uint8_t out_hash[MC_PDQ256_BYTES],
	int32_t* out_quality,
	char* errbuf,
	size_t errbuf_len) {
	atomic_fetch_add_explicit(
		&mc_go_pdq256_calls, UINT64_C(1), memory_order_relaxed);
	return mc_pdq256_from_gray(
		gray, width, height, out_hash, out_quality, errbuf, errbuf_len);
}

static int mc_go_phase2_image(
	const mc_image* image,
	mc_phase2_image_out* out,
	char* errbuf,
	size_t errbuf_len) {
	atomic_fetch_add_explicit(
		&mc_go_phase2_calls, UINT64_C(1), memory_order_relaxed);
	return mc_phase2_image(image, out, errbuf, errbuf_len);
}

static int mc_go_phash_parts(
	const mc_image* image,
	uint64_t out_parts[MC_PHASH_PARTS],
	char* errbuf,
	size_t errbuf_len) {
	atomic_fetch_add_explicit(
		&mc_go_phash_parts_calls, UINT64_C(1), memory_order_relaxed);
	return mc_phash_parts(image, out_parts, errbuf, errbuf_len);
}

static int mc_go_sobel_hist(
	const mc_image* image,
	float out_hist[MC_SOBEL_HIST_DIM],
	char* errbuf,
	size_t errbuf_len) {
	atomic_fetch_add_explicit(
		&mc_go_sobel_hist_calls, UINT64_C(1), memory_order_relaxed);
	return mc_sobel_hist(image, out_hist, errbuf, errbuf_len);
}

static void mc_go_reset_boundary_call_counts(void) {
	atomic_store_explicit(&mc_go_decode_calls, UINT64_C(0), memory_order_relaxed);
	atomic_store_explicit(&mc_go_pdq256_calls, UINT64_C(0), memory_order_relaxed);
	atomic_store_explicit(&mc_go_phase2_calls, UINT64_C(0), memory_order_relaxed);
	atomic_store_explicit(
		&mc_go_phash_parts_calls, UINT64_C(0), memory_order_relaxed);
	atomic_store_explicit(
		&mc_go_sobel_hist_calls, UINT64_C(0), memory_order_relaxed);
}

static void mc_go_snapshot_boundary_call_counts(
	mc_go_boundary_call_counts* out) {
	out->decode = atomic_load_explicit(
		&mc_go_decode_calls, memory_order_relaxed);
	out->pdq256 = atomic_load_explicit(
		&mc_go_pdq256_calls, memory_order_relaxed);
	out->phase2 = atomic_load_explicit(
		&mc_go_phase2_calls, memory_order_relaxed);
	out->phash_parts = atomic_load_explicit(
		&mc_go_phash_parts_calls, memory_order_relaxed);
	out->sobel_hist = atomic_load_explicit(
		&mc_go_sobel_hist_calls, memory_order_relaxed);
}
*/
import "C"

import (
	"errors"
	"runtime"
	"sync"
	"unsafe"
)

var errGrayImageClosed = errors.New("mediacore: gray image is closed")

type nativeBoundaryCallCounts struct {
	decode     uint64
	pdq256     uint64
	phase2     uint64
	phashParts uint64
	sobelHist  uint64
}

type nativeGrayImage struct {
	image *C.mc_image
}

type nativeGrayImageOps struct {
	allocate   func() *nativeGrayImage
	decode     func([]byte, *nativeGrayImage) error
	pdq256     func(*nativeGrayImage) ([PDQ256Bytes]byte, int32, error)
	phase2     func(*nativeGrayImage) (Phase2Result, error)
	phashParts func(*nativeGrayImage) ([PHashPartsCount]uint64, error)
	sobelHist  func(*nativeGrayImage) ([SobelHistDim]float32, error)
	freeImage  func(*nativeGrayImage)
	freeOuter  func(*nativeGrayImage)
}

var realNativeGrayImageOps = &nativeGrayImageOps{
	allocate:   allocateNativeGrayImage,
	decode:     decodeNativeGrayImage,
	pdq256:     pdq256NativeGrayImage,
	phase2:     phase2NativeGrayImage,
	phashParts: phashPartsNativeGrayImage,
	sobelHist:  sobelHistNativeGrayImage,
	freeImage:  freeNativeGrayImage,
	freeOuter:  freeNativeGrayImageOuter,
}

var nativeGrayImageOpsSelection = struct {
	sync.RWMutex
	ops *nativeGrayImageOps
}{
	ops: realNativeGrayImageOps,
}

type GrayImage struct {
	mu    sync.Mutex
	image *nativeGrayImage
	ops   *nativeGrayImageOps
}

func DecodeFromMemory(data []byte) (*GrayImage, error) {
	if len(data) == 0 {
		return nil, errors.New("mediacore: image input is empty")
	}

	ops := selectedNativeGrayImageOps()
	native := ops.allocate()
	if native == nil {
		return nil, errors.New("mediacore: allocate gray image failed")
	}
	if err := ops.decode(data, native); err != nil {
		ops.freeImage(native)
		ops.freeOuter(native)
		return nil, err
	}

	decoded := &GrayImage{image: native, ops: ops}
	runtime.SetFinalizer(decoded, finalizeGrayImage)
	return decoded, nil
}

func (g *GrayImage) PDQ256() ([PDQ256Bytes]byte, int32, error) {
	var hash [PDQ256Bytes]byte
	if g == nil {
		return hash, 0, errGrayImageClosed
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.image == nil {
		return hash, 0, errGrayImageClosed
	}
	return g.ops.pdq256(g.image)
}

func (g *GrayImage) Phase2() (Phase2Result, error) {
	var result Phase2Result
	if g == nil {
		return result, errGrayImageClosed
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.image == nil {
		return result, errGrayImageClosed
	}
	return g.ops.phase2(g.image)
}

func (g *GrayImage) Free() {
	if g == nil {
		return
	}
	runtime.SetFinalizer(g, nil)
	g.release()
}

func Phase2Image(data []byte) (Phase2Result, error) {
	decoded, err := DecodeFromMemory(data)
	if err != nil {
		return Phase2Result{}, err
	}
	defer decoded.Free()
	return decoded.Phase2()
}

func finalizeGrayImage(g *GrayImage) {
	g.release()
}

func (g *GrayImage) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.image == nil {
		return
	}
	g.ops.freeImage(g.image)
	g.ops.freeOuter(g.image)
	g.image = nil
}

func selectedNativeGrayImageOps() *nativeGrayImageOps {
	nativeGrayImageOpsSelection.RLock()
	defer nativeGrayImageOpsSelection.RUnlock()
	return nativeGrayImageOpsSelection.ops
}

func swapNativeGrayImageOpsForTest(ops *nativeGrayImageOps) func() {
	nativeGrayImageOpsSelection.Lock()
	previous := nativeGrayImageOpsSelection.ops
	nativeGrayImageOpsSelection.ops = ops
	nativeGrayImageOpsSelection.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			nativeGrayImageOpsSelection.Lock()
			nativeGrayImageOpsSelection.ops = previous
			nativeGrayImageOpsSelection.Unlock()
		})
	}
}

func allocateNativeGrayImage() *nativeGrayImage {
	image := (*C.mc_image)(C.calloc(1, C.size_t(C.sizeof_mc_image)))
	if image == nil {
		return nil
	}
	return &nativeGrayImage{image: image}
}

func decodeNativeGrayImage(data []byte, image *nativeGrayImage) error {
	var errbuf [C.MC_ERRBUF_LEN]C.char
	rc := C.mc_go_decode_gray(
		(*C.uint8_t)(unsafe.Pointer(unsafe.SliceData(data))),
		C.size_t(len(data)),
		image.image,
		&errbuf[0],
		C.size_t(len(errbuf)),
	)
	runtime.KeepAlive(data)
	return resultError("decode gray", rc, &errbuf[0])
}

func pdq256NativeGrayImage(
	image *nativeGrayImage,
) ([PDQ256Bytes]byte, int32, error) {
	var hash [PDQ256Bytes]byte
	var nativeHash [PDQ256Bytes]C.uint8_t
	var quality C.int32_t
	var errbuf [C.MC_ERRBUF_LEN]C.char
	rc := C.mc_go_pdq256_from_gray(
		image.image.gray,
		image.image.width,
		image.image.height,
		&nativeHash[0],
		&quality,
		&errbuf[0],
		C.size_t(len(errbuf)),
	)
	if err := resultError("PDQ256 from gray", rc, &errbuf[0]); err != nil {
		return hash, 0, err
	}
	for i := range hash {
		hash[i] = byte(nativeHash[i])
	}
	return hash, int32(quality), nil
}

func phase2NativeGrayImage(image *nativeGrayImage) (Phase2Result, error) {
	var result Phase2Result
	var native C.mc_phase2_image_out
	var errbuf [C.MC_ERRBUF_LEN]C.char
	rc := C.mc_go_phase2_image(
		image.image,
		&native,
		&errbuf[0],
		C.size_t(len(errbuf)),
	)
	if err := resultError("image phase 2", rc, &errbuf[0]); err != nil {
		return result, err
	}
	for i := range result.PHashParts {
		result.PHashParts[i] = uint64(native.phash_parts[i])
	}
	for i := range result.SobelHist {
		result.SobelHist[i] = float32(native.sobel_hist[i])
	}
	result.Width = int32(image.image.width)
	result.Height = int32(image.image.height)
	return result, nil
}

func phashPartsNativeGrayImage(
	image *nativeGrayImage,
) ([PHashPartsCount]uint64, error) {
	var result [PHashPartsCount]uint64
	var native [PHashPartsCount]C.uint64_t
	var errbuf [C.MC_ERRBUF_LEN]C.char
	rc := C.mc_go_phash_parts(
		image.image,
		&native[0],
		&errbuf[0],
		C.size_t(len(errbuf)),
	)
	if err := resultError("phase-2 pHash", rc, &errbuf[0]); err != nil {
		return result, err
	}
	for i := range result {
		result[i] = uint64(native[i])
	}
	return result, nil
}

func sobelHistNativeGrayImage(
	image *nativeGrayImage,
) ([SobelHistDim]float32, error) {
	var result [SobelHistDim]float32
	var native [SobelHistDim]C.float
	var errbuf [C.MC_ERRBUF_LEN]C.char
	rc := C.mc_go_sobel_hist(
		image.image,
		&native[0],
		&errbuf[0],
		C.size_t(len(errbuf)),
	)
	if err := resultError("phase-2 Sobel", rc, &errbuf[0]); err != nil {
		return result, err
	}
	for i := range result {
		result[i] = float32(native[i])
	}
	return result, nil
}

func freeNativeGrayImage(image *nativeGrayImage) {
	if image == nil || image.image == nil {
		return
	}
	C.mc_free_image(image.image)
}

func freeNativeGrayImageOuter(image *nativeGrayImage) {
	if image == nil || image.image == nil {
		return
	}
	C.free(unsafe.Pointer(image.image))
	image.image = nil
}

func resetNativeBoundaryCallCountsForTest() {
	C.mc_go_reset_boundary_call_counts()
}

func snapshotNativeBoundaryCallCountsForTest() nativeBoundaryCallCounts {
	var native C.mc_go_boundary_call_counts
	C.mc_go_snapshot_boundary_call_counts(&native)
	return nativeBoundaryCallCounts{
		decode:     uint64(native.decode),
		pdq256:     uint64(native.pdq256),
		phase2:     uint64(native.phase2),
		phashParts: uint64(native.phash_parts),
		sobelHist:  uint64(native.sobel_hist),
	}
}
