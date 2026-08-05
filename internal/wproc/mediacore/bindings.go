//go:build cgo && windows && legacy_mediacore

package mediacore

/*
#cgo CFLAGS: -I${SRCDIR}/../../../mediacore/include
#cgo LDFLAGS: -L${SRCDIR} -lmediacore
#include <stdlib.h>
#include <mediacore/mediacore.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"unsafe"
)

const (
	SHA512Bytes = 64
	PDQ256Bytes = 32
)

type ImageResult struct {
	Hash    [PDQ256Bytes]byte
	Quality int32
	Width   int32
	Height  int32
}

type SHA512 struct {
	mu        sync.Mutex
	ctx       *C.mc_sha512
	finalized bool
}

func Version() string {
	version := C.mc_version()
	if version == nil {
		return ""
	}
	return C.GoString(version)
}

func NewSHA512() (*SHA512, error) {
	ctx := C.mc_sha512_new()
	if ctx == nil {
		return nil, errors.New("mediacore: create SHA-512 context failed")
	}
	return &SHA512{ctx: ctx}, nil
}

func (h *SHA512) Update(data []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ctx == nil || h.finalized {
		return errors.New("mediacore: SHA-512 context is closed or finalized")
	}

	var dataPtr *C.uint8_t
	if len(data) != 0 {
		dataPtr = (*C.uint8_t)(unsafe.Pointer(unsafe.SliceData(data)))
	}
	var errbuf [C.MC_ERRBUF_LEN]C.char
	rc := C.mc_sha512_update(
		h.ctx,
		dataPtr,
		C.size_t(len(data)),
		&errbuf[0],
		C.size_t(len(errbuf)),
	)
	runtime.KeepAlive(data)
	return resultError("SHA-512 update", rc, &errbuf[0])
}

func (h *SHA512) Final() ([SHA512Bytes]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var result [SHA512Bytes]byte
	if h.ctx == nil || h.finalized {
		return result, errors.New("mediacore: SHA-512 context is closed or finalized")
	}

	nativeOut := C.malloc(C.size_t(SHA512Bytes))
	if nativeOut == nil {
		return result, errors.New("mediacore: allocate SHA-512 output failed")
	}
	defer C.free(nativeOut)

	var errbuf [C.MC_ERRBUF_LEN]C.char
	rc := C.mc_sha512_final(
		h.ctx,
		(*C.uint8_t)(nativeOut),
		&errbuf[0],
		C.size_t(len(errbuf)),
	)
	if err := resultError("SHA-512 final", rc, &errbuf[0]); err != nil {
		return result, err
	}
	copy(result[:], C.GoBytes(nativeOut, C.int(SHA512Bytes)))
	h.finalized = true
	return result, nil
}

func (h *SHA512) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ctx != nil {
		C.mc_sha512_free(h.ctx)
		h.ctx = nil
	}
	return nil
}

func ImagePhase1(data []byte) (ImageResult, error) {
	var result ImageResult
	if len(data) == 0 {
		return result, errors.New("mediacore: image input is empty")
	}

	nativeHash := C.malloc(C.size_t(PDQ256Bytes))
	if nativeHash == nil {
		return result, errors.New("mediacore: allocate image hash output failed")
	}
	defer C.free(nativeHash)

	var quality, width, height C.int32_t
	var errbuf [C.MC_ERRBUF_LEN]C.char
	rc := C.mc_image_phase1(
		(*C.uint8_t)(unsafe.Pointer(unsafe.SliceData(data))),
		C.size_t(len(data)),
		(*C.uint8_t)(nativeHash),
		&quality,
		&width,
		&height,
		&errbuf[0],
		C.size_t(len(errbuf)),
	)
	runtime.KeepAlive(data)
	if err := resultError("image phase 1", rc, &errbuf[0]); err != nil {
		return result, err
	}
	copy(result.Hash[:], C.GoBytes(nativeHash, C.int(PDQ256Bytes)))
	result.Quality = int32(quality)
	result.Width = int32(width)
	result.Height = int32(height)
	return result, nil
}

func DebugCrash() {
	C.mc_debug_crash()
}

func DebugSleep(durationMS uint32) {
	C.mc_debug_sleep_ms(C.uint32_t(durationMS))
}

func resultError(operation string, rc C.int, errbuf *C.char) error {
	if rc == C.MC_OK {
		return nil
	}
	message := C.GoString(errbuf)
	if message == "" {
		message = "native error"
	}
	return fmt.Errorf("mediacore: %s failed (%d): %s", operation, int(rc), message)
}
