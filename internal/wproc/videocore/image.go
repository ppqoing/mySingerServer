package videocore

const (
	PDQBytes       = 32
	PHashCount     = 9
	SobelHistCount = 128
)

type FeatureSet struct {
	PDQ            [PDQBytes]byte
	PDQQuality     uint32
	PHash          [PHashCount]uint64
	SobelHistogram [SobelHistCount]float32
}

type nativeFeatureSet struct {
	pdq        [PDQBytes]byte
	pdqQuality uint32
	phash      [PHashCount]uint64
	sobel      [SobelHistCount]float32
}

func featureSetFromNative(native nativeFeatureSet) FeatureSet {
	return FeatureSet{
		PDQ:            native.pdq,
		PDQQuality:     native.pdqQuality,
		PHash:          native.phash,
		SobelHistogram: native.sobel,
	}
}
