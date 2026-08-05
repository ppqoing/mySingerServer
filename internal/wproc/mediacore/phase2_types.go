package mediacore

import "errors"

const (
	PHashPartsCount = 9
	SobelHistDim    = 128
)

type Phase2Result struct {
	PHashParts [PHashPartsCount]uint64
	SobelHist  [SobelHistDim]float32
	Width      int32
	Height     int32
}

var ErrUnavailable = errors.New("mediacore: cgo Windows binding unavailable")
