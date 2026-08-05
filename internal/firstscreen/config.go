package firstscreen

import (
	"fmt"
	"math"
)

const (
	bandCount = 4
	sha512Len = 64
	pdqLen    = 32
)

type Config struct {
	HammingMax            int
	AspectTolerance       float64
	VideoDurationWindowMs int64
	ImageQualityMin       int
	ReadPageSize          int
	GroupInsertBatch      int
	SHAResolveChunk       int
}

func DefaultConfig() Config {
	return Config{
		HammingMax:            31,
		AspectTolerance:       0.10,
		VideoDurationWindowMs: 2000,
		ImageQualityMin:       50,
		ReadPageSize:          50000,
		GroupInsertBatch:      1000,
		SHAResolveChunk:       10000,
	}
}

func (c Config) Validate() error {
	if c.HammingMax < 0 || c.HammingMax > 256 {
		return fmt.Errorf("firstscreen: hamming_max must be between 0 and 256")
	}
	if math.IsNaN(c.AspectTolerance) || c.AspectTolerance < 0 || c.AspectTolerance > 1 {
		return fmt.Errorf("firstscreen: aspect_tolerance must be between 0 and 1")
	}
	if c.VideoDurationWindowMs < 0 {
		return fmt.Errorf("firstscreen: video_duration_window_ms must not be negative")
	}
	if c.ImageQualityMin < 0 || c.ImageQualityMin > 100 {
		return fmt.Errorf("firstscreen: image_quality_min must be between 0 and 100")
	}
	if c.ReadPageSize < 1 {
		return fmt.Errorf("firstscreen: read_page_size must be positive")
	}
	if c.GroupInsertBatch < 1 {
		return fmt.Errorf("firstscreen: group_insert_batch must be positive")
	}
	if c.SHAResolveChunk < 1 {
		return fmt.Errorf("firstscreen: sha_resolve_chunk must be positive")
	}
	return nil
}
