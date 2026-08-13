package enum

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestWalkerEnumeratesRegularFilesWithUnicodePaths(t *testing.T) {
	root := t.TempDir()
	files := []string{
		filepath.Join(root, "普通.txt"),
		filepath.Join(root, "子 目录", "照片.jpg"),
	}
	for _, path := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(path), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "空目录"), 0o755); err != nil {
		t.Fatal(err)
	}

	var got []FileRecord
	if err := (WalkerEnumerator{}).Enum(root, func(record FileRecord) error {
		got = append(got, record)
		return nil
	}); err != nil {
		t.Fatalf("Enum: %v", err)
	}
	sort.Slice(got, func(i, j int) bool { return got[i].Path < got[j].Path })
	if len(got) != len(files) {
		t.Fatalf("records = %d, want %d: %#v", len(got), len(files), got)
	}
	for index, path := range files {
		canonical, canonicalErr := canonicalExistingPath(path)
		if canonicalErr != nil {
			t.Fatal(canonicalErr)
		}
		files[index] = canonical
	}
	sort.Strings(files)
	for i, want := range files {
		if got[i].Path != want {
			t.Errorf("record[%d].Path = %q, want %q", i, got[i].Path, want)
		}
		if strings.HasPrefix(got[i].Path, `\\?\`) {
			t.Errorf("record path leaked long-path prefix: %q", got[i].Path)
		}
		if got[i].Size <= 0 || got[i].MTime <= 0 {
			t.Errorf("record metadata not populated: %#v", got[i])
		}
	}
}

func TestWalkerStopsWhenVisitorReturnsError(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := sentinelError("stop")
	err := (WalkerEnumerator{}).Enum(root, func(FileRecord) error { return want })
	if err != want {
		t.Fatalf("Enum error = %v, want %v", err, want)
	}
}

func TestWalkerSurfacesChildDirectoryAccessError(t *testing.T) {
	root := t.TempDir()
	want := errors.New("access denied")
	walker := WalkerEnumerator{
		walkDir: func(path string, visit fs.WalkDirFunc) error {
			return visit(filepath.Join(path, "denied"), nil, want)
		},
	}
	err := walker.Enum(root, func(FileRecord) error { return nil })
	if !errors.Is(err, want) {
		t.Fatalf("Enum error = %v, want %v", err, want)
	}
}

func TestWalkerSurfacesFileInfoError(t *testing.T) {
	root := t.TempDir()
	want := errors.New("metadata denied")
	walker := WalkerEnumerator{
		walkDir: func(path string, visit fs.WalkDirFunc) error {
			return visit(filepath.Join(path, "file.bin"), infoErrorEntry{err: want}, nil)
		},
	}
	err := walker.Enum(root, func(FileRecord) error { return nil })
	if !errors.Is(err, want) {
		t.Fatalf("Enum error = %v, want %v", err, want)
	}
}

type infoErrorEntry struct {
	err error
}

func (entry infoErrorEntry) Name() string               { return "file.bin" }
func (entry infoErrorEntry) IsDir() bool                { return false }
func (entry infoErrorEntry) Type() fs.FileMode          { return 0 }
func (entry infoErrorEntry) Info() (fs.FileInfo, error) { return nil, entry.err }

func TestLongPathAddsAndStripsWindowsPrefix(t *testing.T) {
	path := `C:\` + strings.Repeat(`directory\`, 30) + "file.txt"
	prefixed := longPath(path)
	if !strings.HasPrefix(prefixed, `\\?\`) {
		t.Fatalf("longPath(%q) = %q, want prefix", path, prefixed)
	}
	if got := cleanPath(prefixed); got != path {
		t.Fatalf("cleanPath(%q) = %q, want %q", prefixed, got, path)
	}
}

func TestEverythingMissingDLLIsReportedWithoutPanic(t *testing.T) {
	enumr := NewEverythingEnumeratorAt(filepath.Join(t.TempDir(), "missing.dll"))
	if err := enumr.Available(); err == nil {
		t.Fatal("Available returned nil for a missing DLL")
	}
}

func TestCanonicalSearchRootExpandsExistingShortPath(t *testing.T) {
	root := t.TempDir()
	canonical, err := canonicalSearchRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToUpper(root), "~") &&
		strings.Contains(strings.ToUpper(canonical), "~") {
		t.Fatalf("short root %q was not expanded: %q", root, canonical)
	}
	if _, err := os.Stat(canonical); err != nil {
		t.Fatalf("canonical root %q is not the same existing directory: %v", canonical, err)
	}
}

func TestResilientEnumeratorFallsBackAfterPrimaryErrorWithoutDuplicates(
	t *testing.T,
) {
	primary := scriptedEnumerator{
		name:    "primary",
		records: []FileRecord{{Path: `D:\media\a.bin`}},
		err:     ErrIPC,
	}
	fallback := scriptedEnumerator{
		name: "walker",
		records: []FileRecord{
			{Path: `D:\media\a.bin`},
			{Path: `D:\media\b.bin`},
		},
	}
	var fallbackErr error
	enumr := NewResilientEnumerator(
		primary,
		fallback,
		func(_ string, err error) { fallbackErr = err },
	)
	var paths []string
	if err := enumr.Enum(`D:\media`, func(record FileRecord) error {
		paths = append(paths, record.Path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(fallbackErr, ErrIPC) {
		t.Fatalf("fallback cause = %v, want ErrIPC", fallbackErr)
	}
	want := []string{`D:\media\a.bin`, `D:\media\b.bin`}
	if strings.Join(paths, "|") != strings.Join(want, "|") {
		t.Fatalf("paths = %#v, want %#v", paths, want)
	}
}

func TestResilientEnumeratorFallsBackWhenPrimaryRootIsEmpty(t *testing.T) {
	primary := scriptedEnumerator{name: "primary"}
	fallback := scriptedEnumerator{
		name:    "walker",
		records: []FileRecord{{Path: `D:\media\a.bin`}},
	}
	var fallbackErr error
	enumr := NewResilientEnumerator(
		primary,
		fallback,
		func(_ string, err error) { fallbackErr = err },
	)
	var count int
	if err := enumr.Enum(`D:\media`, func(FileRecord) error {
		count++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 1 || !errors.Is(fallbackErr, ErrNoResults) {
		t.Fatalf("count=%d fallback=%v, want walker result and ErrNoResults", count, fallbackErr)
	}
}

func TestResilientEnumeratorDoesNotFallbackOnVisitorError(t *testing.T) {
	primary := scriptedEnumerator{
		name:    "primary",
		records: []FileRecord{{Path: `D:\media\a.bin`}},
	}
	fallback := &countingEnumerator{
		scriptedEnumerator: scriptedEnumerator{
			name:    "walker",
			records: []FileRecord{{Path: `D:\media\b.bin`}},
		},
	}
	want := sentinelError("database commit failed")
	enumr := NewResilientEnumerator(primary, fallback, nil)
	if err := enumr.Enum(`D:\media`, func(FileRecord) error {
		return want
	}); !errors.Is(err, want) {
		t.Fatalf("Enum error = %v, want visitor error", err)
	}
	if fallback.calls != 0 {
		t.Fatalf("fallback calls = %d, want 0", fallback.calls)
	}
}

func TestEverythingMatchesWalkerWhenIntegrationEnabled(t *testing.T) {
	dll := os.Getenv("DEDUP_TEST_EVERYTHING_DLL")
	if dll == "" {
		t.Skip("set DEDUP_TEST_EVERYTHING_DLL to run the Everything IPC integration")
	}
	root := t.TempDir()
	for _, name := range []string{"a.txt", "中文.jpg"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	enumr := NewEverythingEnumeratorAt(dll)
	availableErr := enumr.Available()
	if availableErr != nil && !errors.Is(availableErr, ErrIndexNotReady) {
		t.Fatalf("Everything Available: %v", availableErr)
	}
	deadline := time.Now().Add(30 * time.Second)
	for errors.Is(availableErr, ErrIndexNotReady) && time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
		availableErr = enumr.Available()
	}
	if availableErr != nil {
		t.Fatalf("Everything database did not become ready: %v", availableErr)
	}
	var everythingRecords []FileRecord
	for {
		everythingRecords = everythingRecords[:0]
		if err := enumr.Enum(root, func(record FileRecord) error {
			everythingRecords = append(everythingRecords, record)
			return nil
		}); err != nil {
			t.Fatalf("%s Enum: %v", enumr.Name(), err)
		}
		if len(everythingRecords) == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	var walkerRecords []FileRecord
	if err := (WalkerEnumerator{}).Enum(root, func(record FileRecord) error {
		walkerRecords = append(walkerRecords, record)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Slice(everythingRecords, func(i, j int) bool {
		return everythingRecords[i].Path < everythingRecords[j].Path
	})
	sort.Slice(walkerRecords, func(i, j int) bool {
		return walkerRecords[i].Path < walkerRecords[j].Path
	})
	if len(everythingRecords) != len(walkerRecords) {
		for _, query := range []string{"", "*", "Everything.exe", root} {
			enumr.mu.Lock()
			var indexed int
			var samples []string
			diagnosticErr := enumr.queryLocked(query, func(record FileRecord) error {
				indexed++
				if len(samples) < 5 {
					samples = append(samples, record.Path)
				}
				return nil
			})
			enumr.mu.Unlock()
			t.Logf("Everything diagnostics: query=%q indexed=%d samples=%q err=%v",
				query, indexed, samples, diagnosticErr)
		}
		t.Fatalf("Everything=%#v Walker=%#v", everythingRecords, walkerRecords)
	}
	for index := range walkerRecords {
		if everythingRecords[index] != walkerRecords[index] {
			t.Fatalf(
				"record[%d] Everything=%#v Walker=%#v",
				index,
				everythingRecords[index],
				walkerRecords[index],
			)
		}
	}
}

type sentinelError string

func (e sentinelError) Error() string { return string(e) }

type scriptedEnumerator struct {
	name    string
	records []FileRecord
	err     error
}

func (e scriptedEnumerator) Name() string     { return e.name }
func (e scriptedEnumerator) Available() error { return nil }
func (e scriptedEnumerator) Enum(_ string, visit func(FileRecord) error) error {
	for _, record := range e.records {
		if err := visit(record); err != nil {
			return err
		}
	}
	return e.err
}

type countingEnumerator struct {
	scriptedEnumerator
	calls int
}

func (e *countingEnumerator) Enum(
	root string,
	visit func(FileRecord) error,
) error {
	e.calls++
	return e.scriptedEnumerator.Enum(root, visit)
}
