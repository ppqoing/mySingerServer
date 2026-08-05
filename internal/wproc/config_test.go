package wproc

import (
	"strings"
	"testing"
	"time"
)

func TestConfigRejectsOverflowNegativeAndExcessiveValues(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{name: "image memory parse overflow", env: map[string]string{"WPROC_IMAGE_MEM_MB": "9223372036854775807"}},
		{name: "negative image memory", env: map[string]string{"WPROC_IMAGE_MEM_MB": "-1"}},
		{name: "image memory above hard cap", env: map[string]string{"WPROC_IMAGE_MEM_MB": "257"}},
		{name: "IPC maximum above hard cap", env: map[string]string{"WPROC_IPC_MAX_MB": "17"}},
		{name: "native timeout overflow", env: map[string]string{"WPROC_NATIVE_TIMEOUT_S": "9223372036854775807"}},
		{name: "frame timeout zero", env: map[string]string{"WPROC_FRAME_TIMEOUT_S": "0"}},
		{name: "frame timeout above hard cap", env: map[string]string{"WPROC_FRAME_TIMEOUT_S": "3601"}},
		{name: "tile max side zero", env: map[string]string{"WPROC_TILE_MAX_SIDE": "0"}},
		{name: "tile max side above hard cap", env: map[string]string{"WPROC_TILE_MAX_SIDE": "8193"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := configFromLookup(func(key string) string { return tc.env[key] })
			if err == nil {
				t.Fatalf("configFromLookup(%v) succeeded, want bounded parse error", tc.env)
			}
			if !strings.Contains(err.Error(), "WPROC_") {
				t.Fatalf("error = %q, want offending environment key", err)
			}
		})
	}
}

func TestConfigAcceptsDocumentedDefaultsAndLimits(t *testing.T) {
	cfg, err := configFromLookup(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReadChunkBytes != 4<<20 || cfg.ImageMemBytes != 256<<20 ||
		cfg.IPCMaxFrameBytes != 16<<20 ||
		cfg.ProbeTimeout != 15*time.Second || cfg.NativeTimeout != 60*time.Second ||
		cfg.FrameTimeout != 20*time.Second || cfg.TileMaxSide != 256 {
		t.Fatalf("defaults = chunk %d memory %d IPC %d probe %s native %s frame %s side %d",
			cfg.ReadChunkBytes, cfg.ImageMemBytes, cfg.IPCMaxFrameBytes,
			cfg.ProbeTimeout, cfg.NativeTimeout, cfg.FrameTimeout, cfg.TileMaxSide)
	}

	cfg, err = configFromLookup(func(key string) string {
		values := map[string]string{
			"WPROC_IMAGE_MEM_MB":     "256",
			"WPROC_IPC_MAX_MB":       "16",
			"WPROC_PROBE_TIMEOUT_S":  "17",
			"WPROC_NATIVE_TIMEOUT_S": "62",
			"WPROC_FRAME_TIMEOUT_S":  "33",
			"WPROC_TILE_MAX_SIDE":    "1024",
		}
		return values[key]
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReadChunkBytes != 4<<20 || cfg.ImageMemBytes != 256<<20 ||
		cfg.IPCMaxFrameBytes != 16<<20 ||
		cfg.ProbeTimeout != 17*time.Second || cfg.NativeTimeout != 62*time.Second ||
		cfg.FrameTimeout != 33*time.Second || cfg.TileMaxSide != 1024 {
		t.Fatalf("limits = chunk %d memory %d IPC %d probe %s native %s frame %s side %d",
			cfg.ReadChunkBytes, cfg.ImageMemBytes, cfg.IPCMaxFrameBytes,
			cfg.ProbeTimeout, cfg.NativeTimeout, cfg.FrameTimeout, cfg.TileMaxSide)
	}
}

func TestVideoCoreConfigUsesOnlyLibraryEnvironment(t *testing.T) {
	values := map[string]string{
		"WPROC_THUMB_CACHE":       `D:\cache`,
		"WPROC_TILE_MAX_SIDE":     "384",
		"WPROC_PROBE_TIMEOUT_S":   "16",
		"WPROC_NATIVE_TIMEOUT_S":  "61",
		"WPROC_FRAME_TIMEOUT_S":   "21",
		"WPROC_IMAGE_MEM_MB":      "128",
		"WPROC_IPC_MAX_MB":        "8",
		"WPROC_FFMPEG":            `D:\forbidden\ffmpeg.exe`,
		"WPROC_FFPROBE":           `D:\forbidden\ffprobe.exe`,
		"WPROC_FFMPEG_TIMEOUT_S":  "999",
		"WPROC_FFPROBE_TIMEOUT_S": "999",
	}
	cfg, err := configFromLookup(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ThumbCacheDir != `D:\cache` || cfg.TileMaxSide != 384 ||
		cfg.ProbeTimeout != 16*time.Second || cfg.NativeTimeout != 61*time.Second ||
		cfg.FrameTimeout != 21*time.Second || cfg.ImageMemBytes != 128<<20 ||
		cfg.IPCMaxFrameBytes != 8<<20 {
		t.Fatalf("VideoCore config = %#v", cfg)
	}
}
