//go:build windows

package agent

import (
	"context"
	"encoding/base64"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"dedup/internal/proto"
	"golang.org/x/sys/windows"
)

const defaultFilesystemBrowseLimit = 200

type filesystemBrowser struct {
	readDir           func(string) ([]os.DirEntry, error)
	getFileAttributes func(*uint16) (uint32, error)
}

func NewFilesystemBrowser() FilesystemBrowser {
	return newFilesystemBrowser(os.ReadDir, windows.GetFileAttributes)
}

func newFilesystemBrowser(
	readDir func(string) ([]os.DirEntry, error),
	getFileAttributes func(*uint16) (uint32, error),
) filesystemBrowser {
	return filesystemBrowser{
		readDir:           readDir,
		getFileAttributes: getFileAttributes,
	}
}

func (browser filesystemBrowser) Browse(
	ctx context.Context,
	request proto.FilesystemBrowseRequest,
) proto.FilesystemBrowseResponse {
	response := proto.FilesystemBrowseResponse{RequestID: request.RequestID}
	if ctx.Err() != nil {
		response.ErrorCode = "browse_cancelled"
		return response
	}
	if err := request.Validate(); err != nil {
		response.ErrorCode = "invalid_path"
		return response
	}
	limit := request.Limit
	if limit == 0 {
		limit = defaultFilesystemBrowseLimit
	}
	cursor, err := decodeFilesystemBrowseCursor(request.Cursor)
	if err != nil {
		response.ErrorCode = "invalid_path"
		return response
	}
	if request.Path == "" {
		return browseWindowsDrives(ctx, response, cursor, limit)
	}
	return browser.browseWindowsDirectory(ctx, request, response, cursor, limit)
}

func browseWindowsDrives(
	ctx context.Context,
	response proto.FilesystemBrowseResponse,
	cursor filesystemBrowseCursor,
	limit int,
) proto.FilesystemBrowseResponse {
	drives, err := windows.GetLogicalDrives()
	if err != nil {
		response.ErrorCode = mapFilesystemBrowseError(err)
		return response
	}
	entries := make([]filesystemBrowseEntry, 0, 26)
	for index := uint(0); index < 26; index++ {
		if ctx.Err() != nil {
			response.ErrorCode = "browse_cancelled"
			return response
		}
		if drives&(1<<index) == 0 {
			continue
		}
		root := string(rune('A'+index)) + `:\`
		driveType := windows.GetDriveType(windows.StringToUTF16Ptr(root))
		if driveType == windows.DRIVE_UNKNOWN || driveType == windows.DRIVE_NO_ROOT_DIR {
			continue
		}
		entries = append(entries, filesystemBrowseEntry{
			entry: proto.FilesystemEntry{
				Name: string(rune('A'+index)) + ":", Path: root,
				Kind: proto.FilesystemEntryDrive, Selectable: true,
			},
			rank: 0,
		})
	}
	return paginateFilesystemBrowseEntries(response, entries, cursor, limit)
}

func (browser filesystemBrowser) browseWindowsDirectory(
	ctx context.Context,
	request proto.FilesystemBrowseRequest,
	response proto.FilesystemBrowseResponse,
	cursor filesystemBrowseCursor,
	limit int,
) proto.FilesystemBrowseResponse {
	directoryEntries, err := browser.readDir(request.Path)
	if err != nil {
		response.ErrorCode = mapFilesystemBrowseError(err)
		return response
	}
	entries := make([]filesystemBrowseEntry, 0, len(directoryEntries))
	for _, directoryEntry := range directoryEntries {
		if ctx.Err() != nil {
			response.ErrorCode = "browse_cancelled"
			return response
		}
		path := filepath.Join(request.Path, directoryEntry.Name())
		attributes, err := browser.getFileAttributes(windows.StringToUTF16Ptr(path))
		if err != nil {
			// A single unreadable entry (denied access, broken reparse
			// point, offline placeholder) must not fail the whole
			// directory listing; skip it.
			continue
		}
		hidden := attributes&windows.FILE_ATTRIBUTE_HIDDEN != 0
		system := attributes&windows.FILE_ATTRIBUTE_SYSTEM != 0
		if !request.ShowHidden && (hidden || system) {
			continue
		}
		kind := proto.FilesystemEntryFile
		rank := byte(1)
		selectable := false
		if directoryEntry.IsDir() {
			kind = proto.FilesystemEntryDirectory
			rank = 0
			selectable = true
		}
		entries = append(entries, filesystemBrowseEntry{
			entry: proto.FilesystemEntry{
				Name: directoryEntry.Name(), Path: path, Kind: kind,
				Hidden: hidden, System: system, Selectable: selectable,
			},
			rank: rank,
		})
	}
	response.CurrentPath = request.Path
	response.ParentPath = filepath.Dir(request.Path)
	return paginateFilesystemBrowseEntries(response, entries, cursor, limit)
}

type filesystemBrowseEntry struct {
	entry proto.FilesystemEntry
	rank  byte
}

type filesystemBrowseCursor struct {
	rank byte
	name string
	set  bool
}

func paginateFilesystemBrowseEntries(
	response proto.FilesystemBrowseResponse,
	entries []filesystemBrowseEntry,
	cursor filesystemBrowseCursor,
	limit int,
) proto.FilesystemBrowseResponse {
	sort.Slice(entries, func(left, right int) bool {
		return compareFilesystemBrowseEntries(entries[left], entries[right]) < 0
	})
	for _, item := range entries {
		if cursor.set && compareFilesystemBrowseEntryCursor(item, cursor) <= 0 {
			continue
		}
		if len(response.Entries) == limit {
			break
		}
		response.Entries = append(response.Entries, item.entry)
	}
	if len(response.Entries) == limit {
		last := response.Entries[len(response.Entries)-1]
		lastRank := byte(1)
		if last.Kind == proto.FilesystemEntryDrive || last.Kind == proto.FilesystemEntryDirectory {
			lastRank = 0
		}
		for _, item := range entries {
			if compareFilesystemBrowseEntryCursor(item, filesystemBrowseCursor{rank: lastRank, name: last.Name, set: true}) > 0 {
				response.NextCursor = encodeFilesystemBrowseCursor(lastRank, last.Name)
				break
			}
		}
	}
	return response
}

func compareFilesystemBrowseEntries(left, right filesystemBrowseEntry) int {
	return compareFilesystemBrowseKeys(left.rank, left.entry.Name, right.rank, right.entry.Name)
}

func compareFilesystemBrowseEntryCursor(entry filesystemBrowseEntry, cursor filesystemBrowseCursor) int {
	return compareFilesystemBrowseKeys(entry.rank, entry.entry.Name, cursor.rank, cursor.name)
}

func compareFilesystemBrowseKeys(leftRank byte, leftName string, rightRank byte, rightName string) int {
	if leftRank < rightRank {
		return -1
	}
	if leftRank > rightRank {
		return 1
	}
	if lowerLeft, lowerRight := strings.ToLower(leftName), strings.ToLower(rightName); lowerLeft < lowerRight {
		return -1
	} else if lowerLeft > lowerRight {
		return 1
	}
	if leftName < rightName {
		return -1
	}
	if leftName > rightName {
		return 1
	}
	return 0
}

func encodeFilesystemBrowseCursor(rank byte, name string) string {
	return base64.RawURLEncoding.EncodeToString(append([]byte{rank, 0}, []byte(name)...))
}

func decodeFilesystemBrowseCursor(value string) (filesystemBrowseCursor, error) {
	if value == "" {
		return filesystemBrowseCursor{}, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) < 3 || decoded[1] != 0 || decoded[0] > 1 {
		return filesystemBrowseCursor{}, errors.New("invalid filesystem browse cursor")
	}
	return filesystemBrowseCursor{rank: decoded[0], name: string(decoded[2:]), set: true}, nil
}

func mapFilesystemBrowseError(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "browse_cancelled"
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, windows.ERROR_FILE_NOT_FOUND), errors.Is(err, windows.ERROR_PATH_NOT_FOUND):
		return "path_not_found"
	case errors.Is(err, fs.ErrPermission), errors.Is(err, windows.ERROR_ACCESS_DENIED):
		return "access_denied"
	case errors.Is(err, windows.ERROR_NOT_READY):
		return "volume_unavailable"
	default:
		return "browse_failed"
	}
}
