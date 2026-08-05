//go:build !cgo || !windows || !legacy_mediacore

package mediacore

const (
	SHA512Bytes = 64
	PDQ256Bytes = 32
)

var errUnavailable = ErrUnavailable

type ImageResult struct {
	Hash    [PDQ256Bytes]byte
	Quality int32
	Width   int32
	Height  int32
}

type SHA512 struct{}
type GrayImage struct{}

func Version() string                         { return "" }
func NewSHA512() (*SHA512, error)             { return nil, errUnavailable }
func (h *SHA512) Update([]byte) error         { return errUnavailable }
func (h *SHA512) Final() ([64]byte, error)    { return [64]byte{}, errUnavailable }
func (h *SHA512) Close() error                { return nil }
func ImagePhase1([]byte) (ImageResult, error) { return ImageResult{}, errUnavailable }
func DebugCrash()                             {}
func DebugSleep(uint32)                       {}

func DecodeFromMemory([]byte) (*GrayImage, error) { return nil, ErrUnavailable }

func (g *GrayImage) PDQ256() ([PDQ256Bytes]byte, int32, error) {
	return [PDQ256Bytes]byte{}, 0, ErrUnavailable
}

func (g *GrayImage) Phase2() (Phase2Result, error) {
	return Phase2Result{}, ErrUnavailable
}

func (g *GrayImage) Free() {}

func Phase2Image([]byte) (Phase2Result, error) {
	return Phase2Result{}, ErrUnavailable
}
