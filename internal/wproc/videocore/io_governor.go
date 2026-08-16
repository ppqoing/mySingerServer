//go:build cgo && windows

package videocore

/*
#include <stdint.h>
#include <videocore/videocore.h>
*/
import "C"

//export go_vc_io_acquire
func go_vc_io_acquire(
	contextValue C.uintptr_t,
	operation C.uint32_t,
	requestedBytes C.uint64_t,
	leaseID *C.uint64_t,
	grantedBytes *C.uint64_t,
	nativeErr *C.vc_error,
) C.int32_t {
	id, granted, status, message := invokeIOAcquire(
		uintptr(contextValue), uint32(operation), uint64(requestedBytes),
	)
	if leaseID != nil {
		*leaseID = C.uint64_t(id)
	}
	if grantedBytes != nil {
		*grantedBytes = C.uint64_t(granted)
	}
	setIOGovernorError(nativeErr, status, message)
	return C.int32_t(status)
}

//export go_vc_io_report
func go_vc_io_report(
	contextValue C.uintptr_t,
	leaseID C.uint64_t,
	actualBytes C.uint64_t,
	elapsedNS C.uint64_t,
	status C.int32_t,
) {
	invokeIOReport(uintptr(contextValue), uint64(leaseID),
		uint64(actualBytes), uint64(elapsedNS), int32(status))
}

func setIOGovernorError(nativeErr *C.vc_error, status int32, message string) {
	if nativeErr == nil {
		return
	}
	nativeErr.abi_version = C.uint32_t(ABIVersion)
	nativeErr.code = C.int32_t(status)
	nativeErr.ffmpeg_code = 0
	nativeErr.win32_code = 0
	for index := range nativeErr.message_utf8 {
		nativeErr.message_utf8[index] = 0
	}
	limit := len(nativeErr.message_utf8) - 1
	if len(message) < limit {
		limit = len(message)
	}
	for index := 0; index < limit; index++ {
		nativeErr.message_utf8[index] = C.char(message[index])
	}
}
