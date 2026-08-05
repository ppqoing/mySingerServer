package m6bench

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRunIOReadsOnlyMediaAndPreservesSource(t *testing.T) {
	root := t.TempDir()
	mediaPath := filepath.Join(root, "a.jpg")
	ignoredPath := filepath.Join(root, "b.txt")
	media := bytes.Repeat([]byte{0x5a}, 8192)
	if err := os.WriteFile(mediaPath, media, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ignoredPath, []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(mediaPath)
	if err != nil {
		t.Fatal(err)
	}

	result, err := RunIO(context.Background(), IOConfig{
		Roots: []string{root}, Extensions: []string{".jpg"},
		MaxFiles: 10, Duration: time.Second, Streams: 2, BlockBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Files != 1 || result.Bytes != int64(len(media)) ||
		result.Errors != 0 || result.StopReason != "completed" {
		t.Fatalf("result = %#v", result)
	}
	afterBytes, err := os.ReadFile(mediaPath)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(mediaPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterBytes, media) ||
		before.Mode() != after.Mode() ||
		!before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("source changed: before=%#v after=%#v", before, after)
	}
}

func TestRunIOStopsAtStableMaxFiles(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"c.jpg", "A.jpg", "b.jpg"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	result, err := RunIO(context.Background(), IOConfig{
		Roots: []string{root}, Extensions: []string{".jpg"},
		MaxFiles: 2, Duration: time.Second, Streams: 1, BlockBytes: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Files != 2 || result.StopReason != "max_files" {
		t.Fatalf("result = %#v", result)
	}
	if len(result.Selected) != 2 ||
		filepath.Base(result.Selected[0]) != "A.jpg" ||
		filepath.Base(result.Selected[1]) != "b.jpg" {
		t.Fatalf("stable selection = %#v", result.Selected)
	}
}

func TestRunIORejectsUnsafeOrUnboundedConfiguration(t *testing.T) {
	for _, cfg := range []IOConfig{
		{},
		{Roots: []string{t.TempDir()}, MaxFiles: 0, Duration: time.Second, Streams: 1, BlockBytes: 1},
		{Roots: []string{t.TempDir()}, MaxFiles: 1, Duration: 0, Streams: 1, BlockBytes: 1},
		{Roots: []string{t.TempDir()}, MaxFiles: 1, Duration: time.Second, Streams: 0, BlockBytes: 1},
	} {
		if _, err := RunIO(context.Background(), cfg); err == nil {
			t.Fatalf("RunIO accepted %#v", cfg)
		}
	}
}
