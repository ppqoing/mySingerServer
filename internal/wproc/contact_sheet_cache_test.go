package wproc

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"
)

func TestPrepareContactSheetRootCreatesMissingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data", "thumbcache")
	if err := PrepareContactSheetRoot(root); err != nil {
		t.Fatal(err)
	}
	canonical, err := contactSheetRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(canonical) {
		t.Fatalf("prepared canonical root = %q, want absolute path", canonical)
	}
}

func TestPrepareContactSheetRootRejectsFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "thumbcache")
	if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PrepareContactSheetRoot(root); err == nil {
		t.Fatal("PrepareContactSheetRoot accepted a regular file")
	}
}

// Break caught: reintroducing the version directory or either metadata file
// violates the single-file cache contract, even when the JPEG itself is valid.
func TestContactSheetCacheUsesOnlyShardRGBJPEG(t *testing.T) {
	root := t.TempDir()
	sha := testContactSheetSHA(0)
	encoded := hex.EncodeToString(sha[:])
	paths := mustContactSheetPaths(t, root, sha, 41, 99, "rgb")
	want := filepath.Join(mustCanonicalRoot(t, root), encoded[:2], encoded+".jpg")
	if paths.JPEG != want {
		t.Fatalf("final JPEG = %q, want %q", paths.JPEG, want)
	}
	if err := writeRGBJPEG(paths.TempJPEG, color.RGBA{R: 220, G: 40, B: 15, A: 255}); err != nil {
		t.Fatal(err)
	}
	if err := publishContactSheet(paths, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	entries, err := cacheRelativeFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	wantEntries := []string{filepath.Join(encoded[:2], encoded+".jpg")}
	if fmt.Sprint(entries) != fmt.Sprint(wantEntries) {
		t.Fatalf("cache files = %v, want %v", entries, wantEntries)
	}
	entry, hit, err := lookupContactSheet(root, sha)
	if err != nil || !hit || entry.Path != paths.JPEG || entry.Width != 8 || entry.Height != 8 {
		t.Fatalf("lookup = (%#v,%v,%v), want complete RGB hit", entry, hit, err)
	}
}

func TestContactSheetCacheRepairsInvalidJPEG(t *testing.T) {
	root := t.TempDir()
	sha := testContactSheetSHA(0)
	paths := mustContactSheetPaths(t, root, sha, 1, 1, "repair")
	if err := writeRGBJPEG(paths.JPEG, color.RGBA{R: 10, G: 40, B: 220, A: 255}); err != nil {
		t.Fatal(err)
	}
	if _, hit, err := lookupContactSheet(root, sha); err != nil || !hit {
		t.Fatalf("RGB JPEG lookup = (%v,%v), want hit", hit, err)
	}

	gray := image.NewGray(image.Rect(0, 0, 8, 8))
	for index := range gray.Pix {
		gray.Pix[index] = 127
	}
	if err := writeJPEG(paths.JPEG, gray); err != nil {
		t.Fatal(err)
	}
	assertContactSheetMiss(t, root, sha, "grayscale JPEG")
	if err := os.WriteFile(paths.JPEG, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	assertContactSheetMiss(t, root, sha, "zero-byte JPEG")
	if err := os.WriteFile(paths.JPEG, []byte{0xff, 0xd8, 0xff, 0xc0, 0, 17, 8}, 0o600); err != nil {
		t.Fatal(err)
	}
	assertContactSheetMiss(t, root, sha, "truncated JPEG")
}

func TestContactSheetConcurrentPublishReadersSeeOnlyCompleteRGBJPEG(t *testing.T) {
	root := t.TempDir()
	sha := testContactSheetSHA(0)
	oldPaths := mustContactSheetPaths(t, root, sha, 1, 1, "old")
	if err := writeRGBJPEG(oldPaths.TempJPEG, color.RGBA{R: 20, G: 30, B: 40, A: 255}); err != nil {
		t.Fatal(err)
	}
	if err := publishContactSheet(oldPaths, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{}
	oldData, err := os.ReadFile(oldPaths.JPEG)
	if err != nil {
		t.Fatal(err)
	}
	allowed[bytesSHA256Hex(oldData)] = true

	start := make(chan struct{})
	var writers sync.WaitGroup
	errCh := make(chan error, 2)
	for index, fill := range []color.RGBA{{R: 230, G: 20, B: 30, A: 255}, {R: 30, G: 220, B: 40, A: 255}} {
		paths := mustContactSheetPaths(t, root, sha, 20+index, int64(100+index), fmt.Sprintf("writer%d", index))
		if err := writeRGBJPEG(paths.TempJPEG, fill); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(paths.TempJPEG)
		if err != nil {
			t.Fatal(err)
		}
		allowed[bytesSHA256Hex(data)] = true
		writers.Add(1)
		go func(paths ContactSheetPaths) {
			defer writers.Done()
			<-start
			errCh <- publishContactSheet(paths, func() error { return nil })
		}(paths)
	}
	close(start)
	done := make(chan struct{})
	go func() { writers.Wait(); close(done) }()
	for {
		select {
		case <-done:
			goto finished
		default:
		}
		data, err := os.ReadFile(oldPaths.JPEG)
		if err != nil {
			if contactSheetTransientReadError(err) {
				continue
			}
			t.Fatal(err)
		}
		if !allowed[bytesSHA256Hex(data)] {
			t.Fatal("reader observed a partial or foreign JPEG")
		}
		if _, err := inspectRGBJPEG(data); err != nil {
			t.Fatalf("reader observed invalid JPEG: %v", err)
		}
	}

finished:
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(oldPaths.JPEG)
	if err != nil {
		t.Fatal(err)
	}
	if !allowed[bytesSHA256Hex(data)] {
		t.Fatal("final JPEG was not one complete writer")
	}
}

func TestContactSheetReplaceFailurePreservesOldAndCleansOwnTemp(t *testing.T) {
	root := t.TempDir()
	sha := testContactSheetSHA(0)
	oldPaths := mustContactSheetPaths(t, root, sha, 1, 1, "old")
	if err := writeRGBJPEG(oldPaths.TempJPEG, color.RGBA{R: 10, G: 20, B: 30, A: 255}); err != nil {
		t.Fatal(err)
	}
	if err := publishContactSheet(oldPaths, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	oldData, err := os.ReadFile(oldPaths.JPEG)
	if err != nil {
		t.Fatal(err)
	}
	newPaths := mustContactSheetPaths(t, root, sha, 2, 2, "new")
	if err := writeRGBJPEG(newPaths.TempJPEG, color.RGBA{R: 240, G: 220, B: 10, A: 255}); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("replace denied")
	err = publishContactSheetWithReplace(newPaths, func() error { return nil }, func(string, string) error { return wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("publish error = %v, want replace failure", err)
	}
	got, err := os.ReadFile(oldPaths.JPEG)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, oldData) {
		t.Fatal("replace failure changed the previous complete JPEG")
	}
	if _, err := os.Stat(newPaths.TempJPEG); !os.IsNotExist(err) {
		t.Fatalf("writer temp remains after replace failure: %v", err)
	}
}

func TestPrepareContactSheetRootCleansOnlyStaleCurrentTemps(t *testing.T) {
	root := t.TempDir()
	sha := testContactSheetSHA(0)
	paths := mustContactSheetPaths(t, root, sha, 1, 1, "stale")
	live := mustContactSheetPaths(t, root, sha, 2, 2, "live")
	if err := writeRGBJPEG(paths.JPEG, color.RGBA{R: 1, G: 2, B: 3, A: 255}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.TempJPEG, live.TempJPEG, paths.JPEG + ".json", paths.JPEG + ".lock"} {
		if err := os.WriteFile(path, []byte("owned fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	legacy := filepath.Join(root, "vc-grid-v1", "00", filepath.Base(paths.TempJPEG))
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	for _, path := range []string{paths.TempJPEG, paths.JPEG + ".json", paths.JPEG + ".lock", legacy} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := PrepareContactSheetRoot(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.TempJPEG); !os.IsNotExist(err) {
		t.Fatalf("stale current temp remains: %v", err)
	}
	for _, path := range []string{live.TempJPEG, paths.JPEG, paths.JPEG + ".json", paths.JPEG + ".lock", legacy} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("startup cleanup touched non-target %q: %v", path, err)
		}
	}
}

func TestContactSheetRejectsEscape(t *testing.T) {
	root := t.TempDir()
	sha := testContactSheetSHA(0)
	cases := []struct {
		name  string
		root  string
		pid   int
		jobID int64
		nonce string
	}{
		{name: "empty root", root: "", pid: 1, jobID: 1, nonce: "ok"},
		{name: "negative pid", root: root, pid: -1, jobID: 1, nonce: "ok"},
		{name: "negative job", root: root, pid: 1, jobID: -1, nonce: "ok"},
		{name: "empty nonce", root: root, pid: 1, jobID: 1, nonce: ""},
		{name: "dot dot", root: root, pid: 1, jobID: 1, nonce: ".."},
		{name: "slash", root: root, pid: 1, jobID: 1, nonce: "a/b"},
		{name: "backslash", root: root, pid: 1, jobID: 1, nonce: `a\b`},
		{name: "punctuation", root: root, pid: 1, jobID: 1, nonce: "a.b"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if paths, err := contactSheetPaths(test.root, sha, test.pid, test.jobID, test.nonce); err == nil {
				t.Fatalf("contactSheetPaths accepted escape input: %#v", paths)
			}
		})
	}
}

func assertContactSheetMiss(t *testing.T, root string, sha [64]byte, label string) {
	t.Helper()
	if entry, hit, err := lookupContactSheet(root, sha); err != nil || hit {
		t.Fatalf("%s lookup = (%#v,%v,%v), want miss", label, entry, hit, err)
	}
}

func testContactSheetSHA(offset byte) [64]byte {
	var sha [64]byte
	for index := range sha {
		sha[index] = byte(index) + offset
	}
	return sha
}

func mustCanonicalRoot(t *testing.T, root string) string {
	t.Helper()
	canonical, err := contactSheetRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func mustContactSheetPaths(t *testing.T, root string, sha [64]byte, pid int, jobID int64, nonce string) ContactSheetPaths {
	t.Helper()
	paths, err := contactSheetPaths(root, sha, pid, jobID, nonce)
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func writeRGBJPEG(path string, fill color.RGBA) error {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.SetRGBA(x, y, fill)
		}
	}
	return writeJPEG(path, img)
}

func writeJPEG(path string, img image.Image) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	encodeErr := jpeg.Encode(file, img, &jpeg.Options{Quality: 90})
	closeErr := file.Close()
	if encodeErr != nil {
		return encodeErr
	}
	return closeErr
}

func cacheRelativeFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, relative)
		return nil
	})
	sort.Strings(files)
	return files, err
}
