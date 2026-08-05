//go:build !cgo || !windows

package videocore

type unavailableBridge struct{}

func platformNativeBridge() nativeBridge { return unavailableBridge{} }

func (unavailableBridge) runtime() (RuntimeInfo, error) {
	return RuntimeInfo{}, ErrUnavailable
}

func (unavailableBridge) cancelCreate() (nativeCancel, error) {
	return nativeCancel{}, ErrUnavailable
}

func (unavailableBridge) cancelRequest(nativeCancel) {}
func (unavailableBridge) cancelFree(nativeCancel)    {}

func (unavailableBridge) open([]uint16, OpenOptions, nativeCancel) (nativeSession, error) {
	return nativeSession{}, ErrUnavailable
}

func (unavailableBridge) hash(nativeSession) ([64]byte, error) {
	return [64]byte{}, ErrUnavailable
}

func (unavailableBridge) analyze(nativeSession, AnalysisRequest) (AnalysisResult, error) {
	return AnalysisResult{}, ErrUnavailable
}

func (unavailableBridge) close(nativeSession) {}
