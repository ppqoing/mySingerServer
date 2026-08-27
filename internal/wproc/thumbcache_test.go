package wproc

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestThumbnailCacheDigestReadsPublishedJPEG(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thumb.jpg")
	data := []byte("published-jpeg")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := fileSHA256Hex(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := bytesSHA256Hex(data); got != want {
		t.Fatalf("file digest = %q, want %q", got, want)
	}
}

func testVideoConfig(cache string) Config {
	cfg := testConfig()
	cfg.FFprobePath = `tools\ffprobe.exe`
	cfg.FFmpegPath = `tools\ffmpeg.exe`
	cfg.FFprobeTimeout = 15 * time.Second
	cfg.FFmpegTimeout = 60 * time.Second
	cfg.ThumbCacheDir = cache
	cfg.ThumbMaxSide = 256
	return cfg
}
