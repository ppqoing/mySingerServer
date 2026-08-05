package wproc

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestThumbCacheKeyIsLiteralAndPathEquivalent(t *testing.T) {
	const want = "db6385b9c50fc471aefae077358ab089af5ef40a"
	if got := mustThumbCacheKey(t, `C:\Media Folder\Video.mp4`); got != want {
		t.Fatalf("mixed-case absolute key = %q, want literal %q", got, want)
	}
	if got := mustThumbCacheKey(t, `c:\media folder\VIDEO.MP4`); got != want {
		t.Fatalf("case-equivalent absolute key = %q, want %q", got, want)
	}

	root := t.TempDir()
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	videoDir := filepath.Join(root, "Folder")
	if err := os.MkdirAll(videoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(videoDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(originalDir) })
	relative := filepath.Join("..", "Folder", "Movie.MP4")
	absolute := filepath.Join(videoDir, "movie.mp4")
	if got, want := mustThumbCacheKey(t, relative), mustThumbCacheKey(t, absolute); got != want {
		t.Fatalf("relative key = %q, absolute equivalent = %q", got, want)
	}
}

func TestThumbCacheKeyPropagatesAbsolutePathFailure(t *testing.T) {
	_, err := thumbCacheKeyWithAbs("relative.mp4", func(string) (string, error) {
		return "", os.ErrInvalid
	})
	if !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("absolute-path error = %v, want os.ErrInvalid", err)
	}
}

func TestThumbCacheMissesInvalidEntriesAndHitsExactSidecar(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.mp4")
	if err := os.WriteFile(source, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(source, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testVideoConfig(filepath.Join(root, "cache"))
	thumb := mustThumbPath(t, cfg, source)
	meta := thumb + ".json"

	assertMiss := func(label string) {
		t.Helper()
		got, hit, err := thumbCacheLookup(cfg, source, info)
		if err != nil {
			t.Fatalf("%s: lookup error: %v", label, err)
		}
		if got != thumb || hit {
			t.Fatalf("%s: lookup = (%q,%v), want (%q,false)", label, got, hit, thumb)
		}
	}

	assertMiss("missing thumbnail")
	if err := os.MkdirAll(filepath.Dir(thumb), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(thumb, []byte("jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertMiss("missing sidecar")
	if err := os.WriteFile(meta, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertMiss("malformed sidecar")
	if err := os.WriteFile(meta, []byte(`{"mtime_unix":1700000000,"size":4}`), 0o644); err != nil {
		t.Fatal(err)
	}
	assertMiss("size mismatch")
	if err := os.WriteFile(meta, []byte(`{"mtime_unix":1699999999,"size":5}`), 0o644); err != nil {
		t.Fatal(err)
	}
	assertMiss("mtime mismatch")
	if err := os.WriteFile(meta, []byte(`{"mtime_unix":1700000000,"size":5}`), 0o644); err != nil {
		t.Fatal(err)
	}
	assertMiss("missing JPEG hash")
	if err := os.WriteFile(meta, []byte(`{"mtime_unix":1700000000,"size":5,"jpeg_sha256":"0000000000000000000000000000000000000000000000000000000000000000"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	assertMiss("JPEG hash mismatch")
	validMeta := `{"mtime_unix":1700000000,"size":5,"jpeg_sha256":"` + bytesSHA256Hex([]byte("jpeg")) + `"}`
	if err := os.WriteFile(meta, []byte(validMeta), 0o644); err != nil {
		t.Fatal(err)
	}
	got, hit, err := thumbCacheLookup(cfg, source, info)
	if err != nil || !hit || got != thumb {
		t.Fatalf("valid lookup = (%q,%v,%v), want (%q,true,nil)", got, hit, err, thumb)
	}
}

func TestThumbCacheCommitUsesSidecarAfterThumbnailAndCleansOnlyStaleTemps(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.mp4")
	if err := os.WriteFile(source, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testVideoConfig(filepath.Join(root, "cache"))
	thumb := mustThumbPath(t, cfg, source)
	if err := os.MkdirAll(filepath.Dir(thumb), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := thumb + ".tmp-stale.jpg"
	live := thumb + ".tmp-live.jpg"
	for _, path := range []string{stale, live} {
		if err := os.WriteFile(path, []byte("tmp"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	if err := thumbCacheCleanStaleTemps(thumb, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale temp still exists: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("live concurrent temp was removed: %v", err)
	}
	if err := thumbCacheWriteMeta(cfg, source, info, bytesSHA256Hex([]byte("jpeg"))); err == nil {
		t.Fatal("sidecar commit succeeded before thumbnail existed")
	}
	if err := os.WriteFile(thumb, []byte("jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := thumbCacheWriteMeta(cfg, source, info, bytesSHA256Hex([]byte("jpeg"))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(thumb + ".json"); err != nil {
		t.Fatalf("committed sidecar missing: %v", err)
	}
	matches, err := filepath.Glob(thumb + ".json.tmp-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("sidecar temps remain: %v", matches)
	}
}

func TestThumbCacheConcurrentWritersNeverPublishMismatchedPairAsHit(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.mp4")
	if err := os.WriteFile(source, []byte("new-source"), 0o644); err != nil {
		t.Fatal(err)
	}
	currentTime := time.Unix(1_700_000_100, 0)
	if err := os.Chtimes(source, currentTime, currentTime); err != nil {
		t.Fatal(err)
	}
	currentInfo, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	oldInfo := fakeInfo{size: int64(len("old-source")), mtime: 1_700_000_000, identity: "old"}
	cfg := testVideoConfig(filepath.Join(root, "cache"))
	thumb := mustThumbPath(t, cfg, source)
	if err := os.MkdirAll(filepath.Dir(thumb), 0o755); err != nil {
		t.Fatal(err)
	}
	newTemp := filepath.Join(filepath.Dir(thumb), "new-writer.jpg")
	oldTemp := filepath.Join(filepath.Dir(thumb), "old-writer.jpg")
	newDigest := bytesSHA256Hex([]byte("new-jpeg"))
	oldDigest := bytesSHA256Hex([]byte("old-jpeg"))
	if err := os.WriteFile(newTemp, []byte("new-jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldTemp, []byte("old-jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}

	newImagePublished := make(chan struct{})
	oldMetaPublished := make(chan struct{})
	newWriterDone := make(chan error, 1)
	oldWriterDone := make(chan error, 1)
	go func() {
		if err := atomicReplace(newTemp, thumb); err != nil {
			newWriterDone <- err
			return
		}
		close(newImagePublished)
		<-oldMetaPublished
		newWriterDone <- thumbCacheWriteMeta(cfg, source, currentInfo, newDigest)
	}()
	go func() {
		<-newImagePublished
		if err := atomicReplace(oldTemp, thumb); err != nil {
			oldWriterDone <- err
			return
		}
		if err := thumbCacheWriteMeta(cfg, source, oldInfo, oldDigest); err != nil {
			oldWriterDone <- err
			return
		}
		close(oldMetaPublished)
		oldWriterDone <- nil
	}()
	if err := <-oldWriterDone; err != nil {
		t.Fatal(err)
	}
	if err := <-newWriterDone; err != nil && !errors.Is(err, errThumbnailPublishConflict) {
		t.Fatal(err)
	}
	got, hit, err := thumbCacheLookup(cfg, source, currentInfo)
	if err != nil {
		t.Fatal(err)
	}
	if hit {
		data, readErr := os.ReadFile(got)
		t.Fatalf("mismatched concurrent pair was accepted as a hit: jpeg=%q readErr=%v", data, readErr)
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

func mustThumbCacheKey(t *testing.T, path string) string {
	t.Helper()
	key, err := thumbCacheKey(path)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func mustThumbPath(t *testing.T, cfg Config, source string) string {
	t.Helper()
	path, err := thumbPathFor(cfg, source)
	if err != nil {
		t.Fatal(err)
	}
	return path
}
