package wproc

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

const (
	defaultReadChunkKB = int64(4096)
	maxReadChunkKB     = int64(16384)
	defaultImageMemMB  = int64(256)
	maxImageMemMB      = int64(256)
	maxTimeoutSeconds  = int64(3600)
	maxThumbSide       = int64(8192)
)

type Config struct {
	ReadChunkBytes   int
	ImageMemBytes    int64
	ProbeTimeout     time.Duration
	NativeTimeout    time.Duration
	FrameTimeout     time.Duration
	TileMaxSide      int
	ThumbCacheDir    string
	IPCMaxFrameBytes int

	// Task 20 removes these aliases with the executable-based pipeline.
	FFprobePath        string
	FFmpegPath         string
	FFprobeTimeout     time.Duration
	FFmpegTimeout      time.Duration
	Phase2FrameTimeout time.Duration
	Phase2FrameMaxSide int
	ThumbMaxSide       int
	CrashInjection     bool
}

func ConfigFromEnv() (Config, error) {
	return configFromLookup(os.Getenv)
}

func configFromLookup(lookup func(string) string) (Config, error) {
	imageMemMB, err := boundedEnvInt64(lookup, "WPROC_IMAGE_MEM_MB", defaultImageMemMB, 1, maxImageMemMB)
	if err != nil {
		return Config{}, err
	}
	probeTimeout, err := boundedEnvInt64(lookup, "WPROC_PROBE_TIMEOUT_S", 15, 1, maxTimeoutSeconds)
	if err != nil {
		return Config{}, err
	}
	nativeTimeout, err := boundedEnvInt64(lookup, "WPROC_NATIVE_TIMEOUT_S", 60, 1, maxTimeoutSeconds)
	if err != nil {
		return Config{}, err
	}
	frameTimeout, err := boundedEnvInt64(lookup, "WPROC_FRAME_TIMEOUT_S", 20, 1, maxTimeoutSeconds)
	if err != nil {
		return Config{}, err
	}
	tileMaxSide, err := boundedEnvInt64(lookup, "WPROC_TILE_MAX_SIDE", 256, 1, maxThumbSide)
	if err != nil {
		return Config{}, err
	}
	ipcMaxMB, err := boundedEnvInt64(lookup, "WPROC_IPC_MAX_MB", 16, 1, 16)
	if err != nil {
		return Config{}, err
	}

	readChunkBytes := defaultReadChunkKB << 10
	return Config{
		ReadChunkBytes:     int(readChunkBytes),
		ImageMemBytes:      imageMemMB << 20,
		ProbeTimeout:       time.Duration(probeTimeout) * time.Second,
		NativeTimeout:      time.Duration(nativeTimeout) * time.Second,
		FrameTimeout:       time.Duration(frameTimeout) * time.Second,
		TileMaxSide:        int(tileMaxSide),
		ThumbCacheDir:      envString(lookup, "WPROC_THUMB_CACHE", `thumbcache`),
		IPCMaxFrameBytes:   int(ipcMaxMB << 20),
		FFprobePath:        `tools\ffprobe.exe`,
		FFmpegPath:         `tools\ffmpeg.exe`,
		FFprobeTimeout:     time.Duration(probeTimeout) * time.Second,
		FFmpegTimeout:      time.Duration(nativeTimeout) * time.Second,
		Phase2FrameTimeout: time.Duration(frameTimeout) * time.Second,
		Phase2FrameMaxSide: int(tileMaxSide),
		ThumbMaxSide:       int(tileMaxSide),
	}, nil
}

func boundedEnvInt64(lookup func(string) string, key string, fallback, minimum, maximum int64) (int64, error) {
	raw := lookup(key)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a base-10 integer: %w", key, err)
	}
	if value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be between %d and %d", key, minimum, maximum)
	}
	return value, nil
}

func maxIntValue() int {
	return int(^uint(0) >> 1)
}

func envString(lookup func(string) string, key, fallback string) string {
	if value := lookup(key); value != "" {
		return value
	}
	return fallback
}
