package wproc

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dedup/internal/wproc/videocore"
)

// Break caught: a fresh portable Agent passed a missing thumbcache root to
// workers, so every contact-sheet lookup failed before it could create shards.
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

func TestContactSheetCachePath(t *testing.T) {
	root := t.TempDir()
	sha := testContactSheetSHA(0)
	shaHex := hex.EncodeToString(sha[:])

	paths, err := contactSheetPaths(root, sha, 41, 99, "AbC_7-z")
	if err != nil {
		t.Fatal(err)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	wantJPEG := filepath.Join(absRoot, "vc-grid-v1", shaHex[:2], shaHex+".jpg")
	want := ContactSheetPaths{
		JPEG:        wantJPEG,
		Sidecar:     wantJPEG + ".json",
		TempJPEG:    wantJPEG + ".tmp-41-99-AbC_7-z",
		TempSidecar: wantJPEG + ".json.tmp-41-99-AbC_7-z",
	}
	if paths != want {
		t.Fatalf("paths = %#v, want %#v", paths, want)
	}
	if info, err := os.Stat(filepath.Dir(paths.JPEG)); err != nil || !info.IsDir() {
		t.Fatalf("content-addressed directory was not created: info=%v err=%v", info, err)
	}

	otherWriter, err := contactSheetPaths(root, sha, 42, 100, "other")
	if err != nil {
		t.Fatal(err)
	}
	if otherWriter.JPEG != paths.JPEG || otherWriter.Sidecar != paths.Sidecar {
		t.Fatalf("same SHA changed final path: first=%#v second=%#v", paths, otherWriter)
	}
	if otherWriter.TempJPEG == paths.TempJPEG || otherWriter.TempSidecar == paths.TempSidecar {
		t.Fatalf("different writers shared temp paths: first=%#v second=%#v", paths, otherWriter)
	}
}

func TestContactSheetSidecar(t *testing.T) {
	t.Run("round trips complete metadata and rejects mixed JPEG", func(t *testing.T) {
		root := t.TempDir()
		sha := testContactSheetSHA(0)
		paths := mustContactSheetPaths(t, root, sha, 7, 11, "publish")
		jpeg := syntheticJPEG("first")
		if err := os.WriteFile(paths.TempJPEG, jpeg, 0o644); err != nil {
			t.Fatal(err)
		}
		meta := testContactSheetMeta(sha, 1234)
		validated := 0
		if err := publishContactSheet(paths, meta, func() error {
			validated++
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if validated != 1 {
			t.Fatalf("source validation calls = %d, want 1", validated)
		}
		wantDigest := sha256Hex(jpeg)
		got, hit, err := lookupContactSheet(root, sha)
		if err != nil || !hit {
			t.Fatalf("lookup = (%#v,%v,%v), want hit", got, hit, err)
		}
		meta.JPEGSHA256 = wantDigest
		if !contactSheetMetaEqual(got, meta) {
			t.Fatalf("metadata = %#v, want %#v", got, meta)
		}
		raw, err := os.ReadFile(paths.Sidecar)
		if err != nil {
			t.Fatal(err)
		}
		var decoded ContactSheetMeta
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("sidecar is not JSON: %v", err)
		}
		if !contactSheetMetaEqual(decoded, meta) {
			t.Fatalf("sidecar = %#v, want %#v", decoded, meta)
		}

		if err := os.WriteFile(paths.JPEG, syntheticJPEG("mixed"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, hit, err := lookupContactSheet(root, sha); err != nil || hit {
			t.Fatalf("mixed JPEG/sidecar lookup = (%#v,%v,%v), want miss", got, hit, err)
		}
	})

	t.Run("rejects incomplete metadata", func(t *testing.T) {
		root := t.TempDir()
		sha := testContactSheetSHA(0)
		paths := mustContactSheetPaths(t, root, sha, 8, 12, "invalid")
		jpeg := syntheticJPEG("valid")
		if err := os.WriteFile(paths.JPEG, jpeg, 0o644); err != nil {
			t.Fatal(err)
		}
		valid := testContactSheetMeta(sha, 1234)
		valid.JPEGSHA256 = sha256Hex(jpeg)
		cases := map[string]func(*ContactSheetMeta){
			"schema":           func(m *ContactSheetMeta) { m.SchemaVersion = 0 },
			"pipeline":         func(m *ContactSheetMeta) { m.Pipeline = "vc-grid-v2" },
			"source SHA":       func(m *ContactSheetMeta) { m.SourceSHA512 = "00" },
			"JPEG SHA":         func(m *ContactSheetMeta) { m.JPEGSHA256 = "ABC" },
			"canvas":           func(m *ContactSheetMeta) { m.CanvasWidth = 0 },
			"tile":             func(m *ContactSheetMeta) { m.TileHeight = 0 },
			"sample":           func(m *ContactSheetMeta) { m.Samples[5].Status = "" },
			"VideoCore":        func(m *ContactSheetMeta) { m.VideoCoreVersion = "" },
			"FFmpeg component": func(m *ContactSheetMeta) { m.FFmpeg[3].Name = "" },
		}
		for name, mutate := range cases {
			t.Run(name, func(t *testing.T) {
				candidate := valid
				mutate(&candidate)
				raw, err := json.Marshal(candidate)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(paths.Sidecar, raw, 0o644); err != nil {
					t.Fatal(err)
				}
				if got, hit, err := lookupContactSheet(root, sha); err != nil || hit {
					t.Fatalf("invalid metadata lookup = (%#v,%v,%v), want miss", got, hit, err)
				}
			})
		}
	})

	t.Run("source drift commits neither final file", func(t *testing.T) {
		root := t.TempDir()
		sha := testContactSheetSHA(0)
		paths := mustContactSheetPaths(t, root, sha, 9, 13, "stale")
		if err := os.WriteFile(paths.TempJPEG, syntheticJPEG("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
		wantErr := errors.New("source drift")
		if err := publishContactSheet(paths, testContactSheetMeta(sha, 1234), func() error { return wantErr }); !errors.Is(err, wantErr) {
			t.Fatalf("publish error = %v, want source drift", err)
		}
		for _, path := range []string{paths.JPEG, paths.Sidecar} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("drift published %q: %v", path, err)
			}
		}
	})
}

func TestContactSheetConcurrentPublish(t *testing.T) {
	root := t.TempDir()
	sha := testContactSheetSHA(0)
	const writers = 12
	wantDigests := make(map[string]bool, writers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	errCh := make(chan error, writers)
	for i := 0; i < writers; i++ {
		jpeg := syntheticJPEG(fmt.Sprintf("writer-%d", i))
		wantDigests[sha256Hex(jpeg)] = true
		paths := mustContactSheetPaths(t, root, sha, 100+i, int64(i), fmt.Sprintf("n%d", i))
		if err := os.WriteFile(paths.TempJPEG, jpeg, 0o644); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(paths ContactSheetPaths) {
			defer wg.Done()
			<-start
			errCh <- publishContactSheet(paths, testContactSheetMeta(sha, 1234), func() error { return nil })
		}(paths)
	}
	close(start)
	var done atomic.Bool
	go func() {
		wg.Wait()
		done.Store(true)
		close(errCh)
	}()
	hits := 0
	for !done.Load() {
		meta, hit, err := lookupContactSheet(root, sha)
		if err != nil {
			t.Fatal(err)
		}
		if hit {
			hits++
			if !wantDigests[meta.JPEGSHA256] {
				t.Fatalf("reader accepted half-published metadata: %#v", meta)
			}
		}
	}
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent publish: %v", err)
		}
	}
	meta, hit, err := lookupContactSheet(root, sha)
	if err != nil || !hit || !wantDigests[meta.JPEGSHA256] {
		t.Fatalf("final lookup = (%#v,%v,%v), want one complete writer", meta, hit, err)
	}
	t.Logf("observed %d valid concurrent hits", hits)
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
		{name: "absolute", root: root, pid: 1, jobID: 1, nonce: filepath.Join(root, "escape")},
		{name: "punctuation", root: root, pid: 1, jobID: 1, nonce: "a.b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if paths, err := contactSheetPaths(tc.root, sha, tc.pid, tc.jobID, tc.nonce); err == nil {
				t.Fatalf("contactSheetPaths accepted escape input: %#v", paths)
			}
		})
	}

	t.Run("stale cleanup is limited to current SHA regular temps", func(t *testing.T) {
		paths := mustContactSheetPaths(t, root, sha, 20, 30, "stale")
		live := mustContactSheetPaths(t, root, sha, 21, 31, "live")
		otherSHA := sha
		otherSHA[63]++
		other := mustContactSheetPaths(t, root, otherSHA, 20, 30, "stale")
		for _, path := range []string{paths.TempJPEG, paths.TempSidecar, live.TempJPEG, other.TempJPEG, paths.JPEG, paths.Sidecar} {
			if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		directory := paths.TempJPEG + "-directory"
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-2 * time.Hour)
		for _, path := range []string{paths.TempJPEG, paths.TempSidecar, other.TempJPEG, paths.JPEG, paths.Sidecar, directory} {
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		}
		if err := cleanContactSheetStaleTemps(paths, time.Now().Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{paths.TempJPEG, paths.TempSidecar} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("current SHA stale temp remains %q: %v", path, err)
			}
		}
		for _, path := range []string{live.TempJPEG, other.TempJPEG, paths.JPEG, paths.Sidecar, directory} {
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("scoped cleanup removed %q: %v", path, err)
			}
		}
	})
}

func TestContactSheetCrossProcessPublishKeepsCompletePair(t *testing.T) {
	root := t.TempDir()
	sha := testContactSheetSHA(0)
	first := mustContactSheetPaths(t, root, sha, 41, 1, "first")
	second := mustContactSheetPaths(t, root, sha, 42, 2, "second")
	if err := os.WriteFile(first.TempJPEG, syntheticJPEG("first-process"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second.TempJPEG, syntheticJPEG("second-process"), 0o644); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(root, "first-ready")
	release := filepath.Join(root, "release-first")
	child := exec.Command(os.Args[0], "-test.run=^TestContactSheetPublishHelper$")
	child.Env = append(os.Environ(),
		"CONTACT_SHEET_PUBLISH_HELPER=1",
		"CONTACT_SHEET_HELPER_ROOT="+root,
		"CONTACT_SHEET_HELPER_SHA="+hex.EncodeToString(sha[:]),
		"CONTACT_SHEET_HELPER_READY="+ready,
		"CONTACT_SHEET_HELPER_RELEASE="+release,
	)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill() }()
	waitForContactSheetFile(t, ready)

	secondDone := make(chan error, 1)
	go func() {
		secondDone <- publishContactSheet(second, testContactSheetMeta(sha, 1234), func() error { return nil })
	}()
	completedBeforeRelease := false
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("second publish before release: %v", err)
		}
		completedBeforeRelease = true
	case <-time.After(150 * time.Millisecond):
	}
	if err := os.WriteFile(release, []byte("release"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	if !completedBeforeRelease {
		if err := <-secondDone; err != nil {
			t.Fatalf("second publish after release: %v", err)
		}
	}
	if completedBeforeRelease {
		t.Fatal("second process committed while first process held the JPEG/sidecar publish interval")
	}
	if meta, hit, err := lookupContactSheet(root, sha); err != nil || !hit || meta.JPEGSHA256 == "" {
		t.Fatalf("cross-process final lookup = (%#v,%v,%v), want a complete writer", meta, hit, err)
	}
}

func TestContactSheetRejectsLinkedCacheDirectory(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	linkedCache := filepath.Join(root, contactSheetPipeline)
	if err := os.Symlink(outside, linkedCache); err != nil {
		t.Skipf("creating directory link is unavailable: %v", err)
	}
	sha := testContactSheetSHA(0)
	if paths, err := contactSheetPaths(root, sha, 1, 1, "linked"); err == nil {
		t.Fatalf("contactSheetPaths accepted linked cache directory: %#v", paths)
	}
	if _, err := os.Stat(filepath.Join(outside, hex.EncodeToString(sha[:])[:2])); !os.IsNotExist(err) {
		t.Fatalf("linked cache created SHA directory outside root: %v", err)
	}
}

func TestContactSheetPublishHelper(t *testing.T) {
	if os.Getenv("CONTACT_SHEET_PUBLISH_HELPER") != "1" {
		return
	}
	root := os.Getenv("CONTACT_SHEET_HELPER_ROOT")
	encodedSHA := os.Getenv("CONTACT_SHEET_HELPER_SHA")
	decoded, err := hex.DecodeString(encodedSHA)
	if err != nil || len(decoded) != 64 {
		t.Fatalf("helper SHA: %q: %v", encodedSHA, err)
	}
	var sha [64]byte
	copy(sha[:], decoded)
	paths := mustContactSheetPaths(t, root, sha, 41, 1, "first")
	ready := os.Getenv("CONTACT_SHEET_HELPER_READY")
	release := os.Getenv("CONTACT_SHEET_HELPER_RELEASE")
	err = publishContactSheetWithHook(paths, testContactSheetMeta(sha, 1234), func() error { return nil }, func() error {
		if err := os.WriteFile(ready, []byte("ready"), 0o644); err != nil {
			return err
		}
		waitForContactSheetFile(t, release)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func waitForContactSheetFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q", path)
}

func testContactSheetSHA(offset byte) [64]byte {
	var sha [64]byte
	for i := range sha {
		sha[i] = byte(i) + offset
	}
	return sha
}

func testContactSheetMeta(sha [64]byte, sourceSize int64) ContactSheetMeta {
	return ContactSheetMeta{
		SchemaVersion: 1,
		Pipeline:      "vc-grid-v1",
		SourceSHA512:  hex.EncodeToString(sha[:]),
		SourceSize:    sourceSize,
		CanvasWidth:   960,
		CanvasHeight:  360,
		TileWidth:     320,
		TileHeight:    180,
		Samples: [6]ContactSheetSample{
			{TimeMS: 1_000, Status: "ok"},
			{TimeMS: 3_000, Status: "ok"},
			{TimeMS: 5_000, Status: "placeholder"},
			{TimeMS: 7_000, Status: "ok"},
			{TimeMS: 9_000, Status: "ok"},
			{TimeMS: 11_000, Status: "ok"},
		},
		VideoCoreVersion: "1.0.0",
		FFmpeg: [4]videocore.RuntimeComponent{
			{Name: "avformat", HeaderVersion: 0x3f0100, RuntimeVersion: 0x3f0200},
			{Name: "avcodec", HeaderVersion: 0x3f0100, RuntimeVersion: 0x3f0200},
			{Name: "avutil", HeaderVersion: 0x3d0100, RuntimeVersion: 0x3d0200},
			{Name: "swscale", HeaderVersion: 0x0a0100, RuntimeVersion: 0x0a0200},
		},
	}
}

func mustContactSheetPaths(t *testing.T, root string, sha [64]byte, pid int, jobID int64, nonce string) ContactSheetPaths {
	t.Helper()
	paths, err := contactSheetPaths(root, sha, pid, jobID, nonce)
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func syntheticJPEG(label string) []byte {
	return append(append([]byte{0xff, 0xd8, 0xff, 0xe0}, []byte(label)...), 0xff, 0xd9)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func contactSheetMetaEqual(left, right ContactSheetMeta) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}
