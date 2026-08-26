package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dedup/internal/proto"
	"golang.org/x/sys/windows"
)

// This fails if files are hidden from the browser response, or if a file is
// accidentally made selectable as a scan root.
func TestFilesystemBrowserShowsFilesButOnlyDirectoriesSelectable(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Photos"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cover.jpg"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	response := NewFilesystemBrowser().Browse(context.Background(), proto.FilesystemBrowseRequest{
		RequestID: "browse-1", Path: root, Limit: 200,
	})
	if response.ErrorCode != "" {
		t.Fatal(response.ErrorCode)
	}
	if len(response.Entries) != 2 {
		t.Fatalf("entries=%#v", response.Entries)
	}
	if response.Entries[0].Kind != proto.FilesystemEntryDirectory || !response.Entries[0].Selectable {
		t.Fatal("directory not selectable")
	}
	if response.Entries[1].Kind != proto.FilesystemEntryFile || response.Entries[1].Selectable {
		t.Fatal("file selectable")
	}
}

// This fails if ShowHidden exposes hidden or system entries without an explicit
// request, or if attribute inspection stops being reflected in the response.
func TestFilesystemBrowserFiltersHiddenAndSystemEntries(t *testing.T) {
	root := t.TempDir()
	hiddenPath := filepath.Join(root, "hidden")
	systemPath := filepath.Join(root, "system")
	for _, path := range []string{filepath.Join(root, "visible"), hiddenPath, systemPath} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	setWindowsAttributes(t, hiddenPath, windows.FILE_ATTRIBUTE_HIDDEN)
	setWindowsAttributes(t, systemPath, windows.FILE_ATTRIBUTE_SYSTEM)

	browser := NewFilesystemBrowser()
	filtered := browser.Browse(context.Background(), proto.FilesystemBrowseRequest{
		RequestID: "browse-filtered", Path: root, Limit: 200,
	})
	if len(filtered.Entries) != 1 || filtered.Entries[0].Name != "visible" {
		t.Fatalf("filtered entries=%#v", filtered.Entries)
	}
	shown := browser.Browse(context.Background(), proto.FilesystemBrowseRequest{
		RequestID: "browse-shown", Path: root, ShowHidden: true, Limit: 200,
	})
	if len(shown.Entries) != 3 || !hasBrowseEntry(shown.Entries, "hidden", true, false) || !hasBrowseEntry(shown.Entries, "system", false, true) {
		t.Fatalf("shown entries=%#v", shown.Entries)
	}
}

// This fails if a lexical sort accidentally places files ahead of directories,
// or if case-insensitive name sorting becomes unstable.
func TestFilesystemBrowserSortsDirectoriesBeforeFiles(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"zulu", "Alpha"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"z.txt", "A.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	response := NewFilesystemBrowser().Browse(context.Background(), proto.FilesystemBrowseRequest{
		RequestID: "browse-sort", Path: root, Limit: 200,
	})
	if response.ErrorCode != "" {
		t.Fatal(response.ErrorCode)
	}
	got := []string{response.Entries[0].Name, response.Entries[1].Name, response.Entries[2].Name, response.Entries[3].Name}
	want := []string{"Alpha", "zulu", "A.txt", "z.txt"}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("sort[%d]=%q, want %q (%#v)", index, got[index], want[index], response.Entries)
		}
	}
}

// This fails if the browser returns more than its bounded page size or loses
// the stateless continuation cursor after an exact first page.
func TestFilesystemBrowserPagesAtMostTwoHundredEntries(t *testing.T) {
	root := t.TempDir()
	for index := 0; index < 201; index++ {
		if err := os.Mkdir(filepath.Join(root, browseName(index)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	browser := NewFilesystemBrowser()
	first := browser.Browse(context.Background(), proto.FilesystemBrowseRequest{
		RequestID: "browse-page-1", Path: root, Limit: 0,
	})
	if first.ErrorCode != "" || len(first.Entries) != 200 || first.NextCursor == "" {
		t.Fatalf("first page=%#v", first)
	}
	second := browser.Browse(context.Background(), proto.FilesystemBrowseRequest{
		RequestID: "browse-page-2", Path: root, Cursor: first.NextCursor, Limit: 200,
	})
	if second.ErrorCode != "" || len(second.Entries) != 1 || second.NextCursor != "" {
		t.Fatalf("second page=%#v", second)
	}
}

// This fails if malformed cursors can change the browse position rather than
// producing the stable protocol error code.
func TestFilesystemBrowserRejectsInvalidCursor(t *testing.T) {
	response := NewFilesystemBrowser().Browse(context.Background(), proto.FilesystemBrowseRequest{
		RequestID: "browse-cursor", Path: t.TempDir(), Cursor: "not-a-cursor", Limit: 200,
	})
	if response.ErrorCode != "invalid_path" {
		t.Fatalf("invalid cursor error=%q", response.ErrorCode)
	}
}

// This fails if callers receive path details instead of the stable not-found
// category for a location that no longer exists.
func TestFilesystemBrowserMapsMissingPath(t *testing.T) {
	response := NewFilesystemBrowser().Browse(context.Background(), proto.FilesystemBrowseRequest{
		RequestID: "browse-missing", Path: filepath.Join(t.TempDir(), "missing"), Limit: 200,
	})
	if response.ErrorCode != "path_not_found" {
		t.Fatalf("missing path error=%q", response.ErrorCode)
	}
}

// This fails if a Windows access failure leaks platform-specific errors rather
// than the stable error code, without relying on the test token's DACL rights.
func TestFilesystemBrowserMapsAccessDenied(t *testing.T) {
	if got := mapFilesystemBrowseError(windows.ERROR_ACCESS_DENIED); got != "access_denied" {
		t.Fatalf("access denied error=%q", got)
	}
}

// This fails if a cancelled connection context still starts filesystem work.
func TestFilesystemBrowserMapsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	response := NewFilesystemBrowser().Browse(ctx, proto.FilesystemBrowseRequest{
		RequestID: "browse-cancelled", Path: t.TempDir(), Limit: 200,
	})
	if response.ErrorCode != "browse_cancelled" {
		t.Fatalf("cancelled context error=%q", response.ErrorCode)
	}
}

// This fails if cancellation after a successful directory read returns the
// requested local path in an otherwise error-only response.
func TestFilesystemBrowserDoesNotReturnPathsAfterLateCancellation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "entry.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	browser := newFilesystemBrowser(
		func(string) ([]os.DirEntry, error) {
			cancel()
			return entries, nil
		},
		windows.GetFileAttributes,
	)
	response := browser.Browse(ctx, proto.FilesystemBrowseRequest{
		RequestID: "browse-late-cancel", Path: root, Limit: 200,
	})
	if response.ErrorCode != "browse_cancelled" || response.CurrentPath != "" || response.ParentPath != "" {
		t.Fatalf("late cancellation response=%#v", response)
	}
}

// This fails if an entry attribute error after a successful directory read
// aborts the whole listing instead of skipping just the failed entry.
func TestFilesystemBrowserSkipsEntriesWithAttributeErrors(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "good.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "broken.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	browser := newFilesystemBrowser(
		func(string) ([]os.DirEntry, error) { return entries, nil },
		func(path *uint16) (uint32, error) {
			if strings.HasSuffix(windows.UTF16PtrToString(path), "broken.txt") {
				return 0, windows.ERROR_ACCESS_DENIED
			}
			return 0, nil
		},
	)
	response := browser.Browse(context.Background(), proto.FilesystemBrowseRequest{
		RequestID: "browse-attribute-error", Path: root, Limit: 200,
	})
	if response.ErrorCode != "" || response.CurrentPath != root {
		t.Fatalf("attribute error aborted listing: %#v", response)
	}
	if len(response.Entries) != 1 || response.Entries[0].Name != "good.txt" {
		t.Fatalf("failed entry was not skipped: %#v", response.Entries)
	}
}

func setWindowsAttributes(t *testing.T, path string, attributes uint32) {
	t.Helper()
	pathUTF16 := windows.StringToUTF16Ptr(path)
	if err := windows.SetFileAttributes(pathUTF16, attributes); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = windows.SetFileAttributes(pathUTF16, windows.FILE_ATTRIBUTE_NORMAL)
	})
}

func hasBrowseEntry(entries []proto.FilesystemEntry, name string, hidden, system bool) bool {
	for _, entry := range entries {
		if entry.Name == name && entry.Hidden == hidden && entry.System == system {
			return true
		}
	}
	return false
}

func browseName(index int) string {
	return string(rune('a'+index/26)) + string(rune('a'+index%26))
}
